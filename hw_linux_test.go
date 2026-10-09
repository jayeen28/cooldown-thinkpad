//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fanEnabled = `status:		enabled
speed:		3058
level:		auto
commands:	level <level> (<level> is 0-7, auto, disengaged, full-speed)
commands:	enable, disable
commands:	watchdog <timeout> (<timeout> is 0 (off), 1-120 (seconds))
`

// Without fan_control=1 the driver lists no commands.
const fanDisabled = `status:		enabled
speed:		3058
level:		auto
`

func TestParseFanCommands(t *testing.T) {
	levels, wd := parseFanCommands(fanEnabled)
	if got := strings.Join(levels, ","); got != "full-speed,7,6,5,4,3,2,1" || !wd {
		t.Errorf("levels %s watchdog %v", got, wd)
	}
	if levels, wd := parseFanCommands(fanDisabled); len(levels) != 0 || wd {
		t.Errorf("control disabled, but parsed levels %v watchdog %v", levels, wd)
	}
}

func TestUnsupportedControlReported(t *testing.T) {
	dir := t.TempDir()
	defer func(old string) { ibmFan = old }(ibmFan)
	ibmFan = filepath.Join(dir, "fan")
	h := &hardware{}
	if ok, reason, _ := h.ControlSupport(); ok || !strings.Contains(reason, "not found") {
		t.Errorf("missing interface: ok=%v reason=%q", ok, reason)
	}
	os.WriteFile(ibmFan, []byte(fanDisabled), 0o644)
	if ok, reason, _ := h.ControlSupport(); ok || !strings.Contains(reason, "fan_control") {
		t.Errorf("disabled control: ok=%v reason=%q", ok, reason)
	}
}

// TestDryRunNeverWrites runs the real loop in dry-run mode against a fake fan
// file and checks that nothing was written to it.
func TestDryRunNeverWrites(t *testing.T) {
	dir := t.TempDir()
	defer func(old string) { ibmFan = old }(ibmFan)
	ibmFan = filepath.Join(dir, "fan")
	if err := os.WriteFile(ibmFan, []byte(fanEnabled), 0o644); err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(dir, "temp")
	os.WriteFile(temp, []byte("80000\n"), 0o644)
	h := &hardware{model: "test", sensors: []sensor{{"cpu", temp, true}}}
	h.levels, h.hasWatchdog = parseFanCommands(fanEnabled)
	before, _ := os.Stat(ibmFan)

	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	stdout := os.Stdout
	os.Stdout, _ = os.Open(os.DevNull)
	code := run(ctx, h, true, false)
	os.Stdout = stdout

	after, _ := os.Stat(ibmFan)
	data, _ := os.ReadFile(ibmFan)
	if code != 0 || string(data) != fanEnabled || !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("dry run modified the fan file (exit %d):\n%s", code, data)
	}
}

func TestFanToolName(t *testing.T) {
	for _, tc := range []struct {
		comm string
		args []string
		want bool
	}{
		{"thinkfan", []string{"/usr/sbin/thinkfan", "-n"}, true},
		{"python3", []string{"python3", "/opt/thinkfan-ui/main.py"}, true},
		{"bash", []string{"bash", "/usr/bin/thinkfan-ui"}, true},
		{"grep", []string{"grep", "thinkfan"}, false},
		{"vim", []string{"vim", "thinkfan.conf"}, false},
		{"python3", []string{"python3", "server.py"}, false},
	} {
		if got := fanToolName(tc.comm, tc.args) != ""; got != tc.want {
			t.Errorf("%v: got %v, want %v", tc.args, got, tc.want)
		}
	}
}
