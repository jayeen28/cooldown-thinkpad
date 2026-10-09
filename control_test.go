package main

import (
	"math"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func sec(n float64) time.Time { return t0.Add(time.Duration(n * float64(time.Second))) }

// rawCfg disables smoothing so expectations are exact.
func rawCfg() Config {
	c := DefaultConfig()
	c.SmoothUp, c.SmoothDown = 1, 1
	return c
}

func TestCurveInterpolation(t *testing.T) {
	cfg := rawCfg()
	cfg.Curve = []CurvePoint{{45, 4000}, {55, 5400}}
	c := NewController(cfg, 1000, 7000)
	if got := c.curveRPM(50); got != 4700 {
		t.Errorf("50°C on 45→4000/55→5400: got %d, want 4700", got)
	}

	c = NewController(DefaultConfig(), 1000, 7000)
	for _, tc := range []struct {
		temp float64
		want int
	}{
		{20, 2000}, {35, 2000}, {37.5, 2500}, {45, 4000}, {50, 4800}, {62.5, 6250},
		{67.5, 6750}, // between 65→6500 and 70→max(7000)
		{70, 7000}, {95, 7000},
	} {
		if got := c.curveRPM(tc.temp); got != tc.want {
			t.Errorf("curve(%v) = %d, want %d", tc.temp, got, tc.want)
		}
	}
}

func TestClampToHardwareRange(t *testing.T) {
	// Hardware slower than the illustrative curve: curve points above max are clamped.
	c := NewController(DefaultConfig(), 2400, 4500)
	if got := c.curveRPM(30); got != 2400 {
		t.Errorf("below curve: got %d, want hardware min 2400", got)
	}
	if got := c.curveRPM(60); got != 4500 {
		t.Errorf("60°C: got %d, want hardware max 4500", got)
	}
	// Boost cannot exceed the maximum either.
	d := feed(c, rawCfg(), []float64{60, 61, 63, 66})
	if d.DesiredRPM > 4500 || d.TargetRPM > 4500 {
		t.Errorf("target %d exceeds max 4500", d.TargetRPM)
	}
}

// feed sends one reading per second and returns the last decision.
func feed(c *Controller, _ Config, temps []float64) Decision {
	var d Decision
	for i, tc := range temps {
		d = c.Update(sec(float64(i)), tc, true)
	}
	return d
}

func TestRiseRateAndBoost(t *testing.T) {
	cfg := rawCfg()
	for _, tc := range []struct {
		step  float64
		boost int
	}{{0, 0}, {0.2, 0}, {0.3, 200}, {0.5, 200}, {1.0, 500}, {2.0, 800}} {
		c := NewController(cfg, 1000, 9000)
		var d Decision
		for i := 0; i < 5; i++ {
			d = c.Update(sec(float64(i)), 40+tc.step*float64(i), true)
		}
		if !d.RateOK || math.Abs(d.Rate-tc.step) > 1e-9 {
			t.Errorf("step %v: rate %v ok=%v", tc.step, d.Rate, d.RateOK)
		}
		if d.BoostRPM != tc.boost {
			t.Errorf("rate %v: boost %d, want %d", tc.step, d.BoostRPM, tc.boost)
		}
	}
}

func TestRateNeedsHistory(t *testing.T) {
	c := NewController(rawCfg(), 1000, 9000)
	if d := c.Update(sec(0), 40, true); d.RateOK {
		t.Error("rate from a single reading")
	}
	// Widely separated readings must not produce a rate.
	c.Update(sec(1), 41, true)
	if d := c.Update(sec(4.5), 50, true); d.RateOK {
		t.Errorf("rate across a 3.5 s gap: %v", d.Rate)
	}
}

func TestInvalidReadingsAreNeverZero(t *testing.T) {
	c := NewController(rawCfg(), 1000, 7000)
	d := c.Update(sec(0), 60, true)
	want := d.TargetRPM
	for i, bad := range []struct {
		v  float64
		ok bool
	}{{0, false}, {0, true}, {-128, true}, {200, true}, {math.NaN(), true}} {
		d = c.Update(sec(float64(i+1)*0.5), bad.v, bad.ok)
		if !d.Holding || d.FailSafe || d.TargetRPM != want || d.Temp != 60 {
			t.Errorf("bad reading %v: holding=%v failsafe=%v target=%d temp=%v", bad.v, d.Holding, d.FailSafe, d.TargetRPM, d.Temp)
		}
	}
}

func TestStaleReadingsFailSafe(t *testing.T) {
	c := NewController(rawCfg(), 1000, 7000)
	if d := c.Update(sec(0), 0, false); !d.FailSafe {
		t.Error("no reading yet should be fail-safe")
	}
	c.Update(sec(1), 50, true)
	if d := c.Update(sec(4), 0, false); d.FailSafe {
		t.Error("3 s without data should hold, not fail-safe")
	}
	if d := c.Update(sec(7), 0, false); !d.FailSafe {
		t.Error("6 s without data should be fail-safe")
	}
	// Recovery starts fresh from the new reading.
	if d := c.Update(sec(8), 45, true); d.FailSafe || d.Temp != 45 {
		t.Errorf("recovery: %+v", d)
	}
}

func TestControlTempPicksHottestValidCPU(t *testing.T) {
	cfg := DefaultConfig()
	rs := []TempReading{
		{Name: "pkg", Celsius: 55, OK: true, Control: true},
		{Name: "core", Celsius: 61, OK: true, Control: true},
		{Name: "broken", Celsius: 99, OK: false, Control: true},
		{Name: "wifi", Celsius: 70, OK: true, Control: false},
		{Name: "absent", Celsius: 0, OK: true, Control: true},
	}
	if got, ok := controlTemp(rs, cfg); !ok || got != 61 {
		t.Errorf("got %v %v, want 61", got, ok)
	}
	if _, ok := controlTemp(rs[2:3], cfg); ok {
		t.Error("only failed sensors must not yield a temperature")
	}
}

func TestJumpNeedsConfirmation(t *testing.T) {
	c := NewController(rawCfg(), 1000, 7000)
	c.Update(sec(0), 45, true)
	if d := c.Update(sec(1), 95, true); !d.Holding {
		t.Error("single 50°C spike accepted")
	}
	if d := c.Update(sec(2), 46, true); d.Holding || d.Temp != 46 {
		t.Errorf("normal reading after spike: %+v", d)
	}
	c.Update(sec(3), 95, true)
	if d := c.Update(sec(4), 95, true); d.Holding || d.Temp != 95 {
		t.Errorf("confirmed jump not followed: %+v", d)
	}
}

func TestSmoothingIsAsymmetric(t *testing.T) {
	c := NewController(DefaultConfig(), 1000, 7000)
	c.Update(sec(0), 50, true)
	up := c.Update(sec(1), 60, true).Temp - 50
	c2 := NewController(DefaultConfig(), 1000, 7000)
	c2.Update(sec(0), 60, true)
	down := 60 - c2.Update(sec(1), 50, true).Temp
	if up <= down || up >= 10 {
		t.Errorf("rise should be followed faster than fall: up %v down %v", up, down)
	}
}

func TestRampUpImmediateDownGradual(t *testing.T) {
	cfg := rawCfg()
	cfg.RateBoost = nil
	c := NewController(cfg, 2000, 7000)
	c.Update(sec(0), 40, true) // 3000
	if d := c.Update(sec(1), 60, true); d.TargetRPM != 6000 {
		t.Fatalf("ramp up: got %d, want 6000 immediately", d.TargetRPM)
	}
	// Temperature drops to 40 (3000 RPM): fall 200 RPM per second.
	for i, want := range []int{5800, 5600, 5400} {
		if d := c.Update(sec(float64(i+2)), 40, true); d.TargetRPM != want {
			t.Errorf("ramp down step %d: got %d, want %d", i, d.TargetRPM, want)
		}
	}
	// Elapsed time is capped so a long pause cannot drop the fan at once.
	if d := c.Update(sec(4.5+3), 40, true); d.TargetRPM != 5000 {
		t.Errorf("after a 3.5 s pause: got %d, want 5000 (2 s cap)", d.TargetRPM)
	}
}

func TestHysteresisDeadband(t *testing.T) {
	cfg := rawCfg()
	cfg.RateBoost = nil
	c := NewController(cfg, 2000, 7000)
	c.Update(sec(0), 45, true) // 4000
	// 44.5°C wants 3900, within the 150 RPM deadband: hold.
	if d := c.Update(sec(1), 44.5, true); d.TargetRPM != 4000 {
		t.Errorf("small dip changed target to %d", d.TargetRPM)
	}
	// 44°C wants 3800, outside the deadband: start ramping.
	if d := c.Update(sec(2), 44, true); d.TargetRPM != 3800 {
		t.Errorf("larger dip: got %d, want 3800", d.TargetRPM)
	}
}

func TestEmergency(t *testing.T) {
	cfg := DefaultConfig() // smoothing on: the raw reading must still trigger at once
	cfg.RateBoost = nil
	c := NewController(cfg, 2000, 6800)
	c.Update(sec(0), 40, true)
	d := c.Update(sec(1), 66, true) // jump 26°C: accepted, smoothed stays lower
	if d.Emergency {
		t.Fatal("66°C is not an emergency")
	}
	d = c.Update(sec(2), 86, true)
	if !d.Emergency || d.TargetRPM != 6800 {
		t.Fatalf("86°C: emergency=%v target=%d, want max immediately", d.Emergency, d.TargetRPM)
	}
	// Still emergency until smoothed temperature is EmergencyClear below the threshold.
	d = c.Update(sec(3), 82, true)
	if !d.Emergency {
		t.Error("cleared emergency without hysteresis")
	}
	for i := 4; i < 30; i++ {
		d = c.Update(sec(float64(i)), 60, true)
	}
	if d.Emergency {
		t.Error("emergency did not clear after cooling down")
	}
}

func TestRisingAtMaxWarning(t *testing.T) {
	c := NewController(rawCfg(), 2000, 6800)
	var d Decision
	for i := 0; i <= 20; i++ {
		d = c.Update(sec(float64(i)), 71+0.5*float64(i), true)
		if i < 15 && d.RisingAtMax {
			t.Fatalf("warned too early at %d s", i)
		}
	}
	if !d.RisingAtMax {
		t.Error("no warning while rising at maximum output for 20 s")
	}
}

func TestEmergencyForCrit(t *testing.T) {
	cfg := DefaultConfig()
	for _, tc := range []struct{ crit, want float64 }{{0, 85}, {100, 85}, {95, 80}, {80, 70}} {
		if got := emergencyFor(cfg, tc.crit); got != tc.want {
			t.Errorf("crit %v: got %v, want %v", tc.crit, got, tc.want)
		}
	}
}
