package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// FanState is what the hardware reports about the fan(s).
type FanState struct {
	RPM     []int // one per fan, primary first
	RPMOK   bool
	Level   string // e.g. "auto", "3", "disengaged"
	LevelOK bool
}

func (f FanState) primaryRPM() (int, bool) {
	if !f.RPMOK || len(f.RPM) == 0 {
		return 0, false
	}
	return f.RPM[0], true
}

// FanLevel is a discrete hardware level with its measured speed.
type FanLevel struct {
	Name string
	RPM  int
}

// sameLevel compares level names, treating thinkpad_acpi's synonyms as equal.
func sameLevel(a, b string) bool {
	norm := func(s string) string {
		if s == "disengaged" {
			return "full-speed"
		}
		return s
	}
	return norm(a) == norm(b)
}

// LevelPicker maps an RPM target onto calibrated levels (ascending RPM).
// Steps up are immediate; steps down wait MinDownHold after the last change.
type LevelPicker struct {
	levels    []FanLevel
	tolerance float64
	hold      time.Duration
	cur       int
	changed   time.Time
}

func NewLevelPicker(levels []FanLevel, cfg Config) *LevelPicker {
	return &LevelPicker{levels: levels, tolerance: cfg.LevelTolerance, hold: cfg.MinDownHold, cur: -1}
}

// cover returns the lowest level that delivers the target (within tolerance).
func (p *LevelPicker) cover(target int) int {
	for i, l := range p.levels {
		if float64(l.RPM) >= float64(target)*(1-p.tolerance) {
			return i
		}
	}
	return len(p.levels) - 1
}

func (p *LevelPicker) Pick(now time.Time, target int) int {
	want := p.cover(target)
	if p.cur < 0 || want > p.cur || (want < p.cur && now.Sub(p.changed) >= p.hold) {
		p.cur, p.changed = want, now
	}
	return p.cur
}

// Reset forgets the current level so the next Pick is applied unconditionally.
func (p *LevelPicker) Reset() { p.cur = -1 }

// Verdict is the result of checking the fan against what we commanded.
type Verdict int

const (
	VerdictOK          Verdict = iota
	VerdictOverride            // level changed by someone else (another program or firmware)
	VerdictRPMMismatch         // level is ours, but speed does not match calibration
	VerdictFanStopped          // level is ours, but the fan reports 0 RPM
)

// Verifier checks that commands are accepted and stay in effect.
type Verifier struct {
	cfg         Config
	expected    string
	expectedRPM int
	wroteAt     time.Time
	mismatches  int
	rpmBad      int
	failures    int
}

func (v *Verifier) Wrote(now time.Time, level string, rpm int) {
	v.expected, v.expectedRPM, v.wroteAt = level, rpm, now
	v.mismatches, v.rpmBad, v.failures = 0, 0, 0
}

// WriteFailed records a failed write and reports whether to give up.
func (v *Verifier) WriteFailed() bool {
	v.failures++
	return v.failures >= v.cfg.MaxWriteFailures
}

// Check compares the reported fan state with the last command. Each problem is
// reported once; a level mismatch must persist for two checks.
func (v *Verifier) Check(now time.Time, f FanState) Verdict {
	if v.expected == "" {
		return VerdictOK
	}
	if f.LevelOK && !sameLevel(f.Level, v.expected) {
		v.mismatches++
		if v.mismatches == 2 {
			return VerdictOverride
		}
		return VerdictOK
	}
	v.mismatches = 0

	rpm, ok := f.primaryRPM()
	if !ok || v.expectedRPM == 0 || now.Sub(v.wroteAt) < v.cfg.SettleTime {
		return VerdictOK
	}
	tol := max(float64(v.cfg.RPMToleranceAbs), float64(v.expectedRPM)*v.cfg.RPMTolerance)
	if float64(abs(rpm-v.expectedRPM)) <= tol {
		v.rpmBad = 0
		return VerdictOK
	}
	v.rpmBad++
	if v.rpmBad != 5 {
		return VerdictOK
	}
	if rpm == 0 {
		return VerdictFanStopped
	}
	return VerdictRPMMismatch
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// errTooHot means calibration stopped early to cool down; the levels measured so far are usable.
var errTooHot = errors.New("calibration stopped early: temperature reached the emergency threshold")

// calibrate measures the steady RPM of each level. levels must be ordered
// from fastest to slowest so the laptop is cooled first. wait sleeps and
// returns false if cancelled. The result is ascending, without levels that
// are not meaningfully faster than the one below.
func calibrate(ctx context.Context, cfg Config, levels []string,
	set func(string) error, read func() FanState, tooHot func() bool,
	wait func(context.Context, time.Duration) bool, progress func(string)) ([]FanLevel, error) {

	var measured []FanLevel
	var stopErr error
	for _, name := range levels {
		if len(measured) > 0 && tooHot() {
			stopErr = errTooHot
			if err := set(levels[0]); err != nil {
				return nil, fmt.Errorf("set level %s: %w", levels[0], err)
			}
			break
		}
		progress(fmt.Sprintf("measuring level %s...", name))
		if err := set(name); err != nil {
			return nil, fmt.Errorf("set level %s: %w", name, err)
		}
		var rpms []int
		for len(rpms) < 30 { // ThinkPad fans change speed slowly (~200 RPM/s)
			if !wait(ctx, time.Second) {
				return nil, ctx.Err()
			}
			f := read()
			if f.LevelOK && !sameLevel(f.Level, name) {
				return nil, fmt.Errorf("fan command not accepted: requested level %s, firmware reports %s", name, f.Level)
			}
			rpm, ok := f.primaryRPM()
			if !ok {
				return nil, errors.New("fan RPM is unavailable, so fan commands cannot be verified")
			}
			rpms = append(rpms, rpm)
			if steady(rpms) {
				break
			}
		}
		n := len(rpms)
		avg := (rpms[n-1] + rpms[n-2] + rpms[n-3]) / 3
		measured = append(measured, FanLevel{name, avg})
		progress(fmt.Sprintf("level %s = %d RPM", name, avg))
	}

	var table []FanLevel
	for i := len(measured) - 1; i >= 0; i-- {
		l := measured[i]
		if len(table) == 0 || l.RPM >= table[len(table)-1].RPM+cfg.MinLevelStepRPM {
			table = append(table, l)
		}
	}
	if stopErr == nil && (len(table) < 2 || table[len(table)-1].RPM-table[0].RPM < 500) {
		return nil, errors.New("fan speed did not respond to level changes; firmware may be ignoring manual control")
	}
	return table, stopErr
}

// steady reports whether the last three readings agree within 2% (at least 50 RPM).
func steady(rpms []int) bool {
	n := len(rpms)
	if n < 3 {
		return false
	}
	lo, hi := min(rpms[n-1], rpms[n-2], rpms[n-3]), max(rpms[n-1], rpms[n-2], rpms[n-3])
	return float64(hi-lo) <= max(50, 0.02*float64(hi))
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
