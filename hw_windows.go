//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// Windows has no documented, driver-free interface for setting ThinkPad fan
// speed: the embedded controller is only reachable through a kernel-mode
// driver, and Lenovo's WMI interfaces on ThinkPads do not expose fan control.
// This build therefore reads temperatures through WMI and runs in
// monitor-only mode; it never sends fan commands.
const windowsLimitation = "Windows provides no documented user-mode interface for ThinkPad fan control. " +
	"Setting fan speed would require a third-party kernel driver that writes undocumented EC registers, " +
	"which this program deliberately does not do. Use Linux (thinkpad_acpi) for active control."

var errNoControl = errors.New("fan control is not supported on Windows")

type hardware struct {
	model string
}

func openHardware() (*hardware, error) {
	if !isElevated() {
		return nil, errors.New("reading thermal sensors requires Administrator: right-click the program and choose \"Run as administrator\"")
	}
	h := &hardware{model: readModel()}
	if len(h.ReadTemps()) == 0 {
		return nil, errors.New("no temperature sensors available through WMI (MSAcpi_ThermalZoneTemperature)")
	}
	return h, nil
}

func readModel() string {
	const key = `HARDWARE\DESCRIPTION\System\BIOS`
	version := regString(key, "SystemVersion")     // e.g. "ThinkPad T480s"
	product := regString(key, "SystemProductName") // e.g. "20L8S1NR00"
	vendor := regString(key, "SystemManufacturer")
	model := strings.TrimSpace(version + " (" + product + ")")
	if !strings.Contains(strings.ToLower(version+product), "thinkpad") {
		model += " — not identified as a ThinkPad (vendor: " + vendor + ")"
	}
	return model
}

func regString(path, name string) string {
	var k syscall.Handle
	p, _ := syscall.UTF16PtrFromString(path)
	if syscall.RegOpenKeyEx(syscall.HKEY_LOCAL_MACHINE, p, 0, syscall.KEY_READ, &k) != nil {
		return ""
	}
	defer syscall.RegCloseKey(k)
	n, _ := syscall.UTF16PtrFromString(name)
	buf := make([]uint16, 256)
	size := uint32(len(buf) * 2)
	var typ uint32
	if syscall.RegQueryValueEx(k, n, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &size) != nil || typ != syscall.REG_SZ {
		return ""
	}
	return strings.TrimSpace(syscall.UTF16ToString(buf))
}

func isElevated() bool {
	p, err := syscall.GetCurrentProcess()
	if err != nil {
		return false
	}
	var t syscall.Token
	if syscall.OpenProcessToken(p, syscall.TOKEN_QUERY, &t) != nil {
		return false
	}
	defer t.Close()
	var elevated, n uint32
	const tokenElevation = 20
	err = syscall.GetTokenInformation(t, tokenElevation, (*byte)(unsafe.Pointer(&elevated)), 4, &n)
	return err == nil && elevated != 0
}

func (h *hardware) Model() string           { return h.model }
func (h *hardware) CritTemp() float64       { return 0 }
func (h *hardware) Interface() string       { return "none (monitor only: " + windowsLimitation + ")" }
func (h *hardware) ControlLevels() []string { return nil }

// ReadTemps reads ACPI thermal zones. These are firmware zones, not
// necessarily the CPU die, and some firmware updates them only rarely.
func (h *hardware) ReadTemps() []TempReading {
	rows, err := wmiQuery(`root\WMI`, "SELECT InstanceName, CurrentTemperature FROM MSAcpi_ThermalZoneTemperature",
		[]string{"InstanceName", "CurrentTemperature"})
	if err != nil {
		return nil
	}
	var out []TempReading
	for _, r := range rows {
		name, _ := r["InstanceName"].(string)
		tenthsK, ok := r["CurrentTemperature"].(int64)
		c := float64(tenthsK)/10 - 273.15
		ok = ok && plausible(c, DefaultConfig())
		out = append(out, TempReading{Name: "ACPI " + shortZone(name), Celsius: c, OK: ok, Control: true})
	}
	return out
}

func shortZone(s string) string {
	if i := strings.LastIndex(s, `\`); i >= 0 {
		return s[i+1:]
	}
	return s
}

// ReadFan reports what Windows exposes. Win32_Fan rarely carries a real RPM,
// so the speed is normally Unavailable rather than guessed.
func (h *hardware) ReadFan() FanState {
	var f FanState
	rows, err := wmiQuery(`root\CIMV2`, "SELECT DesiredSpeed FROM Win32_Fan", []string{"DesiredSpeed"})
	if err != nil {
		return f
	}
	for _, r := range rows {
		if v, ok := r["DesiredSpeed"].(int64); ok && v > 0 {
			f.RPM = append(f.RPM, int(v))
			f.RPMOK = true
		}
	}
	return f
}

func (h *hardware) ControlSupport() (bool, string, bool) { return false, windowsLimitation, false }
func (h *hardware) Competitors() []string                { return nil }
func (h *hardware) BeginControl(int) error               { return errNoControl }
func (h *hardware) SetLevel(string) error                { return errNoControl }
func (h *hardware) Keepalive() error                     { return errNoControl }
func (h *hardware) Release(bool) (string, error)         { return "", nil }

// enableANSI turns on escape-code processing in the Windows console.
func enableANSI() bool {
	const enableVirtualTerminalProcessing = 0x4
	h := syscall.Handle(os.Stdout.Fd())
	var m uint32
	if syscall.GetConsoleMode(h, &m) != nil {
		return false
	}
	set := syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleMode")
	r, _, _ := set.Call(uintptr(h), uintptr(m|enableVirtualTerminalProcessing))
	if r == 0 {
		fmt.Fprintln(os.Stderr, "note: console does not support status redraw; printing lines instead")
	}
	return r != 0
}
