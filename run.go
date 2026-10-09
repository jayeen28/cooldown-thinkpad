package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

type mode int

const (
	modeMonitor mode = iota
	modeDryRun
	modeActive
)

func (m mode) String() string {
	return [...]string{"MONITOR ONLY — fan not controlled", "DRY RUN — no fan commands sent", "Aggressive Cooling (active control)"}[m]
}

// session is the running state of one invocation.
type session struct {
	hw   *hardware
	cfg  Config
	ui   *ui
	mode mode

	levels   []FanLevel
	picker   *LevelPicker
	verifier Verifier

	owned       bool      // we armed the watchdog and may write levels
	cur         int       // index into levels last written, -1 none
	firmware    bool      // fan handed back to auto because sensor data is unreliable
	lastCmd     time.Time // last command sent (for the watchdog keepalive)
	lastWall    time.Time // wall clock of the previous cycle (suspend detection)
	lastHotWarn time.Time
	inEmergency bool
}

func run(ctx context.Context, hw *hardware, dryRun, verbose bool) int {
	cfg := DefaultConfig()
	cfg.EmergencyTemp = emergencyFor(cfg, hw.CritTemp())
	s := &session{hw: hw, cfg: cfg, ui: newUI(verbose, hw.Model()), cur: -1}
	s.verifier.cfg = cfg

	s.mode = modeDryRun
	if !dryRun {
		ok, reason, permission := hw.ControlSupport()
		switch {
		case permission:
			fmt.Fprintln(os.Stderr, "cooldown:", reason)
			return 1
		case !ok:
			s.mode = modeMonitor
			s.ui.event("Manual fan control unavailable: " + reason)
		case len(hw.Competitors()) > 0:
			s.mode = modeMonitor
			s.ui.event("Another fan controller is running (" + strings.Join(hw.Competitors(), ", ") +
				"); not taking control. Stop it to use active mode.")
		default:
			s.mode = modeActive
		}
	}

	defer func() {
		if r := recover(); r != nil {
			s.release(true)
			panic(r)
		}
	}()

	s.ui.mode = s.mode.String()
	if s.mode == modeActive {
		if err := s.start(ctx); err != nil {
			if ctx.Err() != nil {
				s.release(true)
				return 0
			}
			s.ui.event("Active control not possible: " + err.Error())
			s.release(true)
			s.mode = modeMonitor
		}
	}

	// Without verified levels, clamp to the illustrative curve range.
	minRPM, maxRPM := cfg.Curve[0].RPM, cfg.Curve[len(cfg.Curve)-1].RPM
	if len(s.levels) > 0 {
		minRPM, maxRPM = s.levels[0].RPM, s.levels[len(s.levels)-1].RPM
	}
	ctrl := NewController(cfg, minRPM, maxRPM)
	s.ui.mode = s.mode.String()

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		now := time.Now()
		temps := hw.ReadTemps()
		t, ok := controlTemp(temps, cfg)
		d := ctrl.Update(now, t, ok)
		fan := hw.ReadFan()
		level := ""
		if s.mode == modeActive {
			level = s.step(now, d, fan)
		}
		s.warn(d)
		s.ui.show(d, fan, level, s.statusText(d))

		select {
		case <-ctx.Done():
			s.ui.done()
			if s.owned {
				if restored, ok := s.release(s.stillOurs()); ok && restored != "" {
					fmt.Printf("Fan returned to level %q.\n", restored)
				}
			}
			return 0
		case <-ticker.C:
		}
	}
}

// start arms the hardware and measures the RPM of each level.
func (s *session) start(ctx context.Context) error {
	if err := s.hw.BeginControl(s.cfg.WatchdogSeconds); err != nil {
		return err
	}
	s.owned = true
	s.ui.event("Calibrating: measuring the speed of each fan level, fastest first (about 1-2 min)")
	tooHot := func() bool {
		t, ok := controlTemp(s.hw.ReadTemps(), s.cfg)
		return ok && t >= s.cfg.EmergencyTemp
	}
	levels, err := calibrate(ctx, s.cfg, s.hw.ControlLevels(), s.setLevel, s.hw.ReadFan, tooHot, sleepCtx,
		func(msg string) { s.ui.progress("Calibrating: " + msg) })
	s.ui.progress("")
	if errors.Is(err, errTooHot) {
		s.ui.event("WARNING: " + err.Error())
	} else if err != nil {
		return err
	}
	var names []string
	for _, l := range levels {
		names = append(names, fmt.Sprintf("%s=%d", l.Name, l.RPM))
	}
	s.ui.event("Verified levels (RPM): " + strings.Join(names, ", "))
	s.levels = levels
	s.picker = NewLevelPicker(levels, s.cfg)
	return nil
}

// setLevel writes a level and refreshes the watchdog timestamp.
func (s *session) setLevel(level string) error {
	err := s.hw.SetLevel(level)
	if err == nil {
		s.lastCmd = time.Now()
	}
	return err
}

// step applies one decision in active mode and returns the level in effect.
func (s *session) step(now time.Time, d Decision, fan FanState) string {
	// After a suspend the firmware may have reset the fan; re-apply, don't treat it as an override.
	wall := now.Round(0)
	if !s.lastWall.IsZero() && wall.Sub(s.lastWall) > 3*s.cfg.KeepaliveEvery {
		s.ui.event("Resumed after a pause; re-applying fan level")
		s.verifier.Wrote(now, "", 0)
		s.picker.Reset()
		s.cur = -1
		_ = s.hw.Keepalive()
	}
	s.lastWall = wall

	switch s.verifier.Check(now, fan) {
	case VerdictOverride:
		s.ui.event(fmt.Sprintf("Fan level changed externally to %q (another program or firmware). "+
			"Yielding: switching to monitor-only, settings left as they are.", fan.Level))
		s.release(false)
		s.mode = modeMonitor
		s.ui.mode = s.mode.String()
		return ""
	case VerdictRPMMismatch:
		rpm, _ := fan.primaryRPM()
		s.ui.event(fmt.Sprintf("WARNING: fan at %d RPM, expected about %d RPM for level %s — firmware may be overriding",
			rpm, s.verifier.expectedRPM, s.verifier.expected))
	case VerdictFanStopped:
		s.ui.event("WARNING: fan reports 0 RPM although a running level is set — check the fan")
	}

	if d.FailSafe {
		if !s.firmware {
			s.ui.event("Sensor data unreliable: handing the fan to firmware (auto) until readings recover")
			if s.write(now, "auto", 0) {
				s.firmware = true
				s.cur = -1
				s.picker.Reset()
			}
		}
		return "auto"
	}
	if s.firmware {
		s.ui.event("Sensor readings recovered; resuming control")
		s.firmware = false
	}

	i := s.picker.Pick(now, d.TargetRPM)
	if i != s.cur {
		l := s.levels[i]
		if s.write(now, l.Name, l.RPM) {
			if s.cur >= 0 && s.ui.verbose {
				s.ui.event(fmt.Sprintf("level %s -> %s (target %d RPM)", s.levels[s.cur].Name, l.Name, d.TargetRPM))
			}
			s.cur = i
		}
	} else if now.Sub(s.lastCmd) >= s.cfg.KeepaliveEvery {
		if err := s.hw.Keepalive(); err == nil {
			s.lastCmd = now
		}
	}
	if s.cur < 0 {
		return ""
	}
	return s.levels[s.cur].Name
}

// write sends a level and handles failures; after repeated failures it gives up control.
func (s *session) write(now time.Time, level string, rpm int) bool {
	if err := s.setLevel(level); err != nil {
		s.ui.event("Fan command failed: " + err.Error())
		if s.verifier.WriteFailed() {
			s.ui.event("Fan commands keep failing; giving control back to firmware and stopping control")
			s.release(true)
			s.mode = modeMonitor
			s.ui.mode = s.mode.String()
		}
		return false
	}
	s.verifier.Wrote(now, level, rpm)
	return true
}

// stillOurs re-reads the level to make sure nobody else changed it since our last write.
func (s *session) stillOurs() bool {
	f := s.hw.ReadFan()
	return f.LevelOK && s.verifier.expected != "" && sameLevel(f.Level, s.verifier.expected)
}

func (s *session) release(restore bool) (string, bool) {
	if !s.owned {
		return "", true
	}
	s.owned = false
	level, err := s.hw.Release(restore)
	if err != nil {
		s.ui.event("Could not fully release the fan: " + err.Error())
		return level, false
	}
	return level, true
}

func (s *session) warn(d Decision) {
	if d.Emergency && !s.inEmergency {
		s.ui.event(fmt.Sprintf("EMERGENCY: %.0f°C >= %.0f°C — maximum fan output requested", d.RawTemp, s.cfg.EmergencyTemp))
	}
	s.inEmergency = d.Emergency
	// The rise rate is noisy, so repeat this warning at most once a minute.
	if d.RisingAtMax && time.Since(s.lastHotWarn) >= time.Minute {
		s.lastHotWarn = time.Now()
		s.ui.event("WARNING: temperature still rising at maximum fan output — reduce load or check airflow")
	}
}

func (s *session) statusText(d Decision) string {
	switch {
	case s.mode == modeActive && s.firmware:
		return "Firmware control (sensor data unreliable)"
	case d.FailSafe:
		return "No reliable temperature reading"
	case d.Emergency:
		return "EMERGENCY — maximum cooling"
	case s.mode == modeMonitor:
		return "Monitoring only"
	case s.mode == modeDryRun:
		return "Dry run (would cool)"
	case d.TargetRPM > s.levels[0].RPM:
		return "Cooling"
	default:
		return "Idle (minimum speed)"
	}
}
