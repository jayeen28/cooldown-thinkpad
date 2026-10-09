//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// ibmFan is a variable only so tests can substitute a fake file.
var ibmFan = "/proc/acpi/ibm/fan"

const (
	hwmonDir   = "/sys/class/hwmon"
	thermalDir = "/sys/class/thermal"
	fanParam   = "/sys/module/thinkpad_acpi/parameters/fan_control"
)

type sensor struct {
	name, path string
	control    bool
}

type hardware struct {
	model       string
	sensors     []sensor
	fanInputs   []string // thinkpad hwmon fan*_input, primary first
	critC       float64
	levels      []string // manual levels accepted by the driver, excluding 0 (fan off)
	hasWatchdog bool
	watchdogSec int
	prevLevel   string
}

func openHardware() (*hardware, error) {
	h := &hardware{model: readModel()}
	h.discoverSensors()
	if len(h.sensors) == 0 {
		return nil, fmt.Errorf("no readable temperature sensors found under %s or %s", hwmonDir, thermalDir)
	}
	if data, err := os.ReadFile(ibmFan); err == nil {
		h.levels, h.hasWatchdog = parseFanCommands(string(data))
	}
	return h, nil
}

func readModel() string {
	vendor := readString("/sys/class/dmi/id/sys_vendor")
	version := readString("/sys/class/dmi/id/product_version") // e.g. "ThinkPad T480s"
	product := readString("/sys/class/dmi/id/product_name")    // e.g. "20L8S1NR00"
	model := strings.TrimSpace(version + " (" + product + ")")
	if version == "" {
		model = product
	}
	if !strings.Contains(strings.ToLower(version+product), "thinkpad") {
		model += " — not identified as a ThinkPad (vendor: " + vendor + ")"
	}
	return model
}

func (h *hardware) discoverSensors() {
	dirs, _ := filepath.Glob(filepath.Join(hwmonDir, "hwmon*"))
	var fallback []sensor
	for _, dir := range dirs {
		name := readString(filepath.Join(dir, "name"))
		inputs, _ := filepath.Glob(filepath.Join(dir, "temp*_input"))
		for _, in := range inputs {
			if _, ok := readMilli(in); !ok {
				continue // absent or broken sensor (e.g. thinkpad GPU slot without a GPU)
			}
			label := readString(strings.TrimSuffix(in, "_input") + "_label")
			if label == "" {
				label = strings.TrimSuffix(filepath.Base(in), "_input")
			}
			s := sensor{name: name + " " + label, path: in}
			switch {
			case name == "coretemp": // Intel: package and cores
				s.control = true
				if crit, ok := readMilli(strings.TrimSuffix(in, "_input") + "_crit"); ok && (h.critC == 0 || crit < h.critC) {
					h.critC = crit
				}
			case name == "k10temp" || name == "zenpower": // AMD: Tctl/Tdie/Tccd
				s.control = strings.HasPrefix(label, "T")
			case name == "thinkpad" && label == "CPU":
				fallback = append(fallback, s)
			}
			h.sensors = append(h.sensors, s)
		}
		if name == "thinkpad" {
			fans, _ := filepath.Glob(filepath.Join(dir, "fan*_input"))
			h.fanInputs = fans // Glob sorts, so fan1 comes first
		}
	}
	for _, s := range h.sensors {
		if s.control {
			return
		}
	}
	// No coretemp/k10temp: fall back to the package thermal zone, then the EC's CPU sensor.
	zones, _ := filepath.Glob(filepath.Join(thermalDir, "thermal_zone*"))
	for _, z := range zones {
		if readString(filepath.Join(z, "type")) == "x86_pkg_temp" {
			if _, ok := readMilli(filepath.Join(z, "temp")); ok {
				h.sensors = append(h.sensors, sensor{"x86_pkg_temp", filepath.Join(z, "temp"), true})
				return
			}
		}
	}
	for i := range h.sensors {
		if len(fallback) > 0 && h.sensors[i].path == fallback[0].path {
			h.sensors[i].control = true
			return
		}
	}
}

var levelRange = regexp.MustCompile(`(\d+)-(\d+)`)

// parseFanCommands extracts the manual levels (fastest first, never 0 = fan
// off) and watchdog support from /proc/acpi/ibm/fan. The driver lists
// commands only when thinkpad_acpi was loaded with fan_control=1.
func parseFanCommands(text string) (levels []string, watchdog bool) {
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "commands:") {
			continue
		}
		if strings.Contains(line, "watchdog") {
			watchdog = true
		}
		if !strings.Contains(line, "level <level>") {
			continue
		}
		if strings.Contains(line, "full-speed") {
			levels = append(levels, "full-speed")
		}
		if m := levelRange.FindStringSubmatch(line); m != nil {
			lo, _ := strconv.Atoi(m[1])
			hi, _ := strconv.Atoi(m[2])
			for l := hi; l >= max(lo, 1); l-- {
				levels = append(levels, strconv.Itoa(l))
			}
		}
	}
	return levels, watchdog
}

func (h *hardware) Model() string           { return h.model }
func (h *hardware) CritTemp() float64       { return h.critC }
func (h *hardware) Interface() string       { return "thinkpad_acpi (" + ibmFan + ")" }
func (h *hardware) ControlLevels() []string { return h.levels }

func (h *hardware) ReadTemps() []TempReading {
	out := make([]TempReading, len(h.sensors))
	for i, s := range h.sensors {
		c, ok := readMilli(s.path)
		out[i] = TempReading{Name: s.name, Celsius: c, OK: ok, Control: s.control}
	}
	return out
}

func (h *hardware) ReadFan() FanState {
	var f FanState
	for _, in := range h.fanInputs {
		if v, err := readInt(in); err == nil {
			f.RPM = append(f.RPM, v)
			f.RPMOK = true
		}
	}
	data, err := os.ReadFile(ibmFan)
	if err != nil {
		return f
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, val, _ := strings.Cut(line, ":")
		val = strings.TrimSpace(val)
		switch key {
		case "level":
			f.Level, f.LevelOK = val, true
		case "speed":
			if v, err := strconv.Atoi(val); err == nil && len(h.fanInputs) == 0 {
				f.RPM, f.RPMOK = []int{v}, true
			}
		}
	}
	return f
}

// ControlSupport reports whether active control is possible. permission is
// true when the only problem is missing privileges.
func (h *hardware) ControlSupport() (ok bool, reason string, permission bool) {
	if _, err := os.Stat(ibmFan); err != nil {
		return false, ibmFan + " not found: the thinkpad_acpi driver is not loaded or this is not a ThinkPad", false
	}
	if len(h.levels) == 0 {
		param := readString(fanParam)
		return false, fmt.Sprintf("thinkpad_acpi was loaded without fan control (fan_control=%s).\n"+
			"  To enable it (as root):\n"+
			"    echo 'options thinkpad_acpi fan_control=1' > /etc/modprobe.d/thinkpad_acpi.conf\n"+
			"  then reboot (or reload the thinkpad_acpi module)", orUnknown(param)), false
	}
	if syscall.Access(ibmFan, 2 /* W_OK */) != nil {
		return false, "writing " + ibmFan + " requires root: run with sudo (or use --dry-run / --status)", true
	}
	return true, "", false
}

// fanTools are programs known to control ThinkPad fans.
var fanTools = []string{"thinkfan", "fancontrol", "tpfand", "nbfc", "zcfan", "simpfand", "tp-fan", "tpfancontrol"}

// Competitors lists running fan-control programs, including scripts run by
// an interpreter (e.g. thinkfan-ui runs as "python3 /opt/thinkfan-ui/main.py").
func (h *hardware) Competitors() []string {
	dirs, _ := filepath.Glob("/proc/[0-9]*")
	self := strconv.Itoa(os.Getpid())
	var found []string
	for _, d := range dirs {
		pid := filepath.Base(d)
		if pid == self {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(d, "cmdline"))
		if name := fanToolName(readString(filepath.Join(d, "comm")), strings.Split(string(b), "\x00")); name != "" {
			found = append(found, fmt.Sprintf("%s (pid %s)", name, pid))
		}
	}
	return found
}

// fanToolName returns the matching program or script name, or "".
func fanToolName(comm string, args []string) string {
	candidates := []string{comm}
	if len(args) > 0 {
		exe := filepath.Base(args[0])
		candidates = append(candidates, exe)
		interp := strings.HasPrefix(exe, "python") || strings.HasPrefix(exe, "perl") ||
			exe == "bash" || exe == "sh" || exe == "node"
		if interp && len(args) > 1 {
			candidates = append(candidates, args[1]) // script path
		}
	}
	for _, c := range candidates {
		for _, tool := range fanTools {
			if strings.Contains(strings.ToLower(c), tool) {
				return c
			}
		}
	}
	return ""
}

// BeginControl saves the current level and arms the driver's watchdog, which
// returns the fan to firmware control if this program dies without cleanup.
func (h *hardware) BeginControl(watchdogSec int) error {
	if f := h.ReadFan(); f.LevelOK {
		h.prevLevel = f.Level
	}
	h.watchdogSec = watchdogSec
	return h.Keepalive()
}

func (h *hardware) SetLevel(level string) error { return writeFan("level " + level) }

func (h *hardware) Keepalive() error {
	if !h.hasWatchdog {
		return nil
	}
	return writeFan(fmt.Sprintf("watchdog %d", h.watchdogSec))
}

// Release disarms the watchdog and, if restore is set, returns the fan to the
// level saved at start. Level 0 (fan off) is never restored; auto is used instead.
func (h *hardware) Release(restore bool) (string, error) {
	var errs []string
	if h.hasWatchdog {
		if err := writeFan("watchdog 0"); err != nil {
			errs = append(errs, err.Error())
		}
	}
	level := ""
	if restore {
		level = h.prevLevel
		if level == "" || level == "0" {
			level = "auto"
		}
		if err := writeFan("level " + level); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return level, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return level, nil
}

func writeFan(cmd string) error {
	f, err := os.OpenFile(ibmFan, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString(cmd)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("%q rejected: %w", cmd, err)
	}
	return nil
}

func readString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readInt(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// readMilli reads a millidegree value. Values outside a plausible range are
// failures, never "0 °C".
func readMilli(path string) (float64, bool) {
	v, err := readInt(path)
	if err != nil {
		return 0, false
	}
	c := float64(v) / 1000
	return c, plausible(c, DefaultConfig())
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// enableANSI: Linux terminals understand ANSI escape codes natively.
func enableANSI() bool { return true }
