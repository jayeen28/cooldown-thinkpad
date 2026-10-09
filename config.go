package main

import "time"

// CurvePoint maps a temperature to a fan-speed target.
type CurvePoint struct {
	TempC float64
	RPM   int
}

// BoostStep adds RPM when the temperature rises at least MinRate °C/s.
type BoostStep struct {
	MinRate float64
	RPM     int
}

// Config holds every tunable of the controller. All defaults live in DefaultConfig.
type Config struct {
	Interval time.Duration // sensor/control period

	// Fan curve. Points are clamped to the verified hardware range at runtime;
	// at MaxSpeedTemp and above the target is the maximum verified speed.
	Curve        []CurvePoint
	MaxSpeedTemp float64

	// Rise-rate boost, highest MinRate first.
	RateBoost    []BoostStep
	RateWindow   time.Duration // span used to compute the rise rate
	MaxSampleGap time.Duration // readings further apart than this are not used for a rate

	// Sensor validation and smoothing.
	MinPlausibleC float64
	MaxPlausibleC float64
	MaxJumpC      float64       // a single-sample jump larger than this must be confirmed by the next sample
	StaleAfter    time.Duration // no valid reading for this long: hand the fan back to firmware
	SmoothUp      float64       // EMA weight of a new sample when temperature rises (react fast)
	SmoothDown    float64       // EMA weight of a new sample when temperature falls (calm)

	// Preferred operating temperature. Informational: the curve already encodes
	// how hard to push for it, so no extra error term is added (no double counting).
	TargetTemp float64

	// Ramping and stability.
	RampDownRPMPerSec float64
	DeadbandRPM       int
	LevelTolerance    float64       // a level within this fraction below the target counts as covering it
	MinDownHold       time.Duration // minimum time between a level change and the next step down

	// Emergency handling. EmergencyTemp is lowered automatically when the
	// hardware reports a lower critical temperature (see emergencyFor).
	EmergencyTemp    float64
	EmergencyClear   float64       // leave emergency once this many °C below EmergencyTemp
	RisingAtMaxAfter time.Duration // warn if still rising after this long at maximum output

	// Hardware command handling.
	WatchdogSeconds  int           // firmware reverts to auto if we stop talking for this long
	KeepaliveEvery   time.Duration // how often the watchdog is refreshed
	MaxWriteFailures int           // consecutive failed writes before giving up
	SettleTime       time.Duration // time a fan needs to reach a new speed
	RPMTolerance     float64       // allowed relative RPM deviation from calibration
	RPMToleranceAbs  int           // ...but never less than this many RPM
	MinLevelStepRPM  int           // levels closer than this to the one below are dropped
	UIRefresh        time.Duration // status line period when not on a terminal (e.g. systemd journal)
}

func DefaultConfig() Config {
	return Config{
		Interval: time.Second,

		Curve: []CurvePoint{
			{35, 2000},
			{40, 3000},
			{45, 4000},
			{50, 4800},
			{55, 5400},
			{60, 6000},
			{65, 6500},
		},
		MaxSpeedTemp: 70,

		RateBoost: []BoostStep{
			{1.5, 800},
			{0.8, 500},
			{0.3, 200},
		},
		RateWindow:   3 * time.Second,
		MaxSampleGap: 2500 * time.Millisecond,

		MinPlausibleC: 1,
		MaxPlausibleC: 125,
		MaxJumpC:      30,
		StaleAfter:    5 * time.Second,
		SmoothUp:      0.6,
		SmoothDown:    0.3,

		TargetTemp: 45,

		RampDownRPMPerSec: 200,
		DeadbandRPM:       150,
		LevelTolerance:    0.03,
		MinDownHold:       4 * time.Second,

		EmergencyTemp:    85,
		EmergencyClear:   5,
		RisingAtMaxAfter: 15 * time.Second,

		WatchdogSeconds:  15,
		KeepaliveEvery:   5 * time.Second,
		MaxWriteFailures: 3,
		SettleTime:       8 * time.Second,
		RPMTolerance:     0.25,
		RPMToleranceAbs:  600,
		MinLevelStepRPM:  100,
		UIRefresh:        time.Minute,
	}
}

// emergencyFor derives the emergency threshold from a hardware critical
// temperature: 15 °C below critical, never above the configured default.
func emergencyFor(cfg Config, critC float64) float64 {
	if critC <= 0 {
		return cfg.EmergencyTemp
	}
	return max(cfg.MaxSpeedTemp, min(cfg.EmergencyTemp, critC-15))
}
