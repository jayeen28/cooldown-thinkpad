package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var testLevels = []FanLevel{{"1", 2400}, {"3", 3000}, {"5", 3600}, {"7", 4200}, {"full-speed", 6400}}

func TestLevelPickerCoversTarget(t *testing.T) {
	p := NewLevelPicker(testLevels, DefaultConfig())
	for _, tc := range []struct {
		target int
		want   string
	}{{1000, "1"}, {2400, "1"}, {2401, "1"}, {2500, "3"}, {4100, "7"}, {4300, "7"}, {4400, "full-speed"}, {9000, "full-speed"}} {
		p.Reset()
		if got := testLevels[p.Pick(t0, tc.target)].Name; got != tc.want {
			t.Errorf("target %d: got level %s, want %s", tc.target, got, tc.want)
		}
	}
}

func TestLevelPickerUpFastDownHeld(t *testing.T) {
	p := NewLevelPicker(testLevels, DefaultConfig())
	p.Pick(sec(0), 3000) // level 3
	if i := p.Pick(sec(1), 6000); testLevels[i].Name != "full-speed" {
		t.Errorf("step up delayed: %s", testLevels[i].Name)
	}
	if i := p.Pick(sec(2), 3000); testLevels[i].Name != "full-speed" {
		t.Errorf("stepped down within hold time: %s", testLevels[i].Name)
	}
	if i := p.Pick(sec(5), 3000); testLevels[i].Name != "3" {
		t.Errorf("did not step down after hold: %s", testLevels[i].Name)
	}
}

func TestVerifierOverride(t *testing.T) {
	v := Verifier{cfg: DefaultConfig()}
	v.Wrote(sec(0), "full-speed", 6400)
	if got := v.Check(sec(1), FanState{Level: "disengaged", LevelOK: true}); got != VerdictOK {
		t.Errorf("disengaged should equal full-speed, got %v", got)
	}
	v.Wrote(sec(0), "5", 3600)
	if got := v.Check(sec(1), FanState{Level: "auto", LevelOK: true}); got != VerdictOK {
		t.Errorf("one mismatch should be tolerated, got %v", got)
	}
	if got := v.Check(sec(2), FanState{Level: "auto", LevelOK: true}); got != VerdictOverride {
		t.Errorf("persistent mismatch: got %v, want override", got)
	}
}

func TestVerifierRPM(t *testing.T) {
	cfg := DefaultConfig()
	check := func(rpm int) Verdict {
		v := Verifier{cfg: cfg}
		v.Wrote(sec(0), "7", 4200)
		var got Verdict
		for i := 0; i < 5; i++ {
			// Readings during settling are ignored.
			if r := v.Check(sec(float64(i)), FanState{RPM: []int{rpm}, RPMOK: true, Level: "7", LevelOK: true}); r != VerdictOK {
				t.Fatalf("verdict during settle time")
			}
		}
		for i := 0; i < 6; i++ {
			if r := v.Check(sec(float64(10+i)), FanState{RPM: []int{rpm}, RPMOK: true, Level: "7", LevelOK: true}); r != VerdictOK {
				got = r
			}
		}
		return got
	}
	if got := check(4100); got != VerdictOK {
		t.Errorf("4100 vs 4200: %v", got)
	}
	if got := check(2500); got != VerdictRPMMismatch {
		t.Errorf("2500 vs 4200: %v, want mismatch", got)
	}
	if got := check(0); got != VerdictFanStopped {
		t.Errorf("0 vs 4200: %v, want stopped", got)
	}
}

func TestVerifierWriteFailures(t *testing.T) {
	v := Verifier{cfg: DefaultConfig()}
	if v.WriteFailed() || v.WriteFailed() {
		t.Error("gave up before MaxWriteFailures")
	}
	if !v.WriteFailed() {
		t.Error("did not give up after 3 consecutive failures")
	}
	v.Wrote(sec(0), "3", 3000) // a success resets the count
	if v.WriteFailed() {
		t.Error("count not reset by success")
	}
}

// fakeFan simulates a fan that spins up to a per-level speed over a few seconds.
type fakeFan struct {
	speeds   map[string]int
	level    string
	rpm      float64
	reported string // non-empty: firmware reports this level instead
	noRPM    bool
	writes   []string
}

func (f *fakeFan) set(l string) error {
	if _, ok := f.speeds[l]; !ok {
		return errors.New("rejected")
	}
	f.level = l
	f.writes = append(f.writes, l)
	return nil
}

func (f *fakeFan) read() FanState {
	f.rpm += (float64(f.speeds[f.level]) - f.rpm) * 0.7 // first-order spin-up/down
	lvl := f.level
	if f.reported != "" {
		lvl = f.reported
	}
	if f.level == "full-speed" && f.reported == "" {
		lvl = "disengaged"
	}
	return FanState{RPM: []int{int(f.rpm)}, RPMOK: !f.noRPM, Level: lvl, LevelOK: true}
}

func noWait(context.Context, time.Duration) bool { return true }

func runCalibrate(f *fakeFan, tooHot func() bool) ([]FanLevel, error) {
	order := []string{"full-speed", "7", "6", "5", "4", "3", "2", "1"}
	return calibrate(context.Background(), DefaultConfig(), order, f.set, f.read, tooHot, noWait, func(string) {})
}

func TestCalibrate(t *testing.T) {
	f := &fakeFan{speeds: map[string]int{"full-speed": 6400, "7": 4200, "6": 3800, "5": 3600, "4": 3200,
		"3": 3150, "2": 2700, "1": 2400}}
	levels, err := runCalibrate(f, func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for i, l := range levels {
		names = append(names, l.Name)
		if i > 0 && l.RPM <= levels[i-1].RPM {
			t.Errorf("not ascending: %v", levels)
		}
		if want := f.speeds[l.Name]; abs(l.RPM-want) > 60 {
			t.Errorf("level %s measured %d, want ~%d", l.Name, l.RPM, want)
		}
	}
	// Level 3 (3150) is within MinLevelStepRPM of level 4 (3200)... from below: 4 is dropped.
	if got := strings.Join(names, ","); got != "1,2,3,5,6,7,full-speed" {
		t.Errorf("levels %s", got)
	}
	if f.writes[0] != "full-speed" {
		t.Errorf("calibration must start at the fastest level, started at %s", f.writes[0])
	}
}

func TestCalibrateDetectsUnsupportedControl(t *testing.T) {
	speeds := map[string]int{"full-speed": 3000, "7": 3000, "6": 3000, "5": 3000, "4": 3000, "3": 3000, "2": 3000, "1": 3000}
	if _, err := runCalibrate(&fakeFan{speeds: speeds}, func() bool { return false }); err == nil {
		t.Error("fan that ignores levels was accepted")
	}
	if _, err := runCalibrate(&fakeFan{speeds: speeds, reported: "auto"}, func() bool { return false }); err == nil ||
		!strings.Contains(err.Error(), "not accepted") {
		t.Errorf("firmware override not detected: %v", err)
	}
	if _, err := runCalibrate(&fakeFan{speeds: speeds, noRPM: true}, func() bool { return false }); err == nil {
		t.Error("calibration without RPM readings cannot verify anything")
	}
	if _, err := runCalibrate(&fakeFan{speeds: map[string]int{}}, func() bool { return false }); err == nil {
		t.Error("rejected write not reported")
	}
}

func TestCalibrateStopsWhenHot(t *testing.T) {
	f := &fakeFan{speeds: map[string]int{"full-speed": 6400, "7": 4200, "6": 3800, "5": 3600, "4": 3200,
		"3": 3000, "2": 2700, "1": 2400}}
	calls := 0
	levels, err := runCalibrate(f, func() bool { calls++; return calls > 2 })
	if !errors.Is(err, errTooHot) {
		t.Fatalf("err = %v, want errTooHot", err)
	}
	if len(levels) != 3 || f.level != "full-speed" {
		t.Errorf("levels %v, fan left at %s; want 3 measured levels and full-speed", levels, f.level)
	}
}

func TestCalibrateCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakeFan{speeds: map[string]int{"full-speed": 6400, "1": 2400}}
	_, err := calibrate(ctx, DefaultConfig(), []string{"full-speed", "1"}, f.set, f.read,
		func() bool { return false }, sleepCtx, func(string) {})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestFmtRPM(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 4500: "4,500", 12345: "12,345", 1234567: "1,234,567"} {
		if got := fmtRPM(n); got != want {
			t.Errorf("fmtRPM(%d) = %q, want %q", n, got, want)
		}
	}
}
