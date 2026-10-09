package main

import (
	"math"
	"time"
)

// TempReading is one sensor value. Control marks CPU readings used for control.
type TempReading struct {
	Name    string
	Celsius float64
	OK      bool
	Control bool
}

// controlTemp returns the hottest valid control reading. A failed sensor is
// never treated as a temperature.
func controlTemp(rs []TempReading, cfg Config) (float64, bool) {
	hottest, ok := 0.0, false
	for _, r := range rs {
		if r.Control && r.OK && plausible(r.Celsius, cfg) && (!ok || r.Celsius > hottest) {
			hottest, ok = r.Celsius, true
		}
	}
	return hottest, ok
}

func plausible(c float64, cfg Config) bool {
	return !math.IsNaN(c) && c >= cfg.MinPlausibleC && c <= cfg.MaxPlausibleC
}

// Decision is the controller output for one cycle.
type Decision struct {
	RawTemp float64 // hottest control reading this cycle
	Temp    float64 // smoothed temperature
	Rate    float64 // °C/s, valid only if RateOK
	RateOK  bool

	BaseRPM    int // from the curve
	BoostRPM   int // from the rise rate
	DesiredRPM int // clamped base+boost (max in an emergency)
	TargetRPM  int // after ramping; this is what should be applied

	Emergency   bool
	RisingAtMax bool // at maximum output for a while and still heating up
	Holding     bool // this reading was rejected; previous target kept
	FailSafe    bool // no reliable data: give control back to firmware
}

type sample struct {
	t    time.Time
	temp float64
}

// Controller turns temperatures into fan-speed targets in RPM. It knows
// nothing about hardware except the verified speed range.
type Controller struct {
	cfg            Config
	minRPM, maxRPM int

	haveTemp    bool
	smoothed    float64
	lastValid   time.Time
	pendingJump bool
	hist        []sample

	haveTarget bool
	target     float64
	lastUpdate time.Time

	emergency  bool
	atMaxSince time.Time
}

func NewController(cfg Config, minRPM, maxRPM int) *Controller {
	return &Controller{cfg: cfg, minRPM: minRPM, maxRPM: maxRPM}
}

// Update consumes one reading (ok=false for a failed read) and returns the decision.
func (c *Controller) Update(now time.Time, raw float64, ok bool) Decision {
	d := Decision{RawTemp: raw}
	ok = ok && plausible(raw, c.cfg)
	// Any plausible reading at the emergency threshold counts, even an
	// unconfirmed jump: spinning up on a false spike is harmless.
	hot := ok && raw >= c.cfg.EmergencyTemp
	ok = ok && c.accept(now, raw)
	if !ok && hot {
		c.emergency, c.target, c.haveTarget = true, float64(c.maxRPM), true
		c.lastUpdate = now
	}
	if !ok {
		c.hist = nil
		if !c.haveTemp || now.Sub(c.lastValid) > c.cfg.StaleAfter {
			d.FailSafe = true
			c.haveTarget = false
			c.emergency = false
			return d
		}
		// Brief glitch: keep what we had, make no new decision on bad data.
		d.Temp, d.Holding, d.Emergency = c.smoothed, true, c.emergency
		d.TargetRPM = int(math.Round(c.target))
		d.DesiredRPM = d.TargetRPM
		return d
	}
	d.Temp = c.smoothed
	d.Rate, d.RateOK = c.rate(now)

	d.BaseRPM = c.curveRPM(d.Temp)
	if d.RateOK {
		d.BoostRPM = c.boost(d.Rate)
	}
	d.DesiredRPM = c.clamp(d.BaseRPM + d.BoostRPM)

	// Emergency reacts to the raw reading, not the smoothed one, and clears with hysteresis.
	if hot {
		c.emergency = true
	} else if c.emergency && math.Max(raw, d.Temp) < c.cfg.EmergencyTemp-c.cfg.EmergencyClear {
		c.emergency = false
	}
	if c.emergency {
		d.DesiredRPM = c.maxRPM
	}
	d.Emergency = c.emergency

	d.TargetRPM = c.ramp(now, d.DesiredRPM)

	if d.TargetRPM >= c.maxRPM {
		if c.atMaxSince.IsZero() {
			c.atMaxSince = now
		}
		d.RisingAtMax = now.Sub(c.atMaxSince) >= c.cfg.RisingAtMaxAfter &&
			d.RateOK && d.Rate > 0.1 && d.Temp >= c.cfg.MaxSpeedTemp
	} else {
		c.atMaxSince = time.Time{}
	}
	return d
}

// accept validates and smooths a plausible reading. It returns false when the
// reading is an unconfirmed jump.
func (c *Controller) accept(now time.Time, raw float64) bool {
	gap := now.Sub(c.lastValid)
	switch {
	case !c.haveTemp || gap > c.cfg.StaleAfter:
		c.smoothed, c.hist = raw, nil
	case math.Abs(raw-c.smoothed) > c.cfg.MaxJumpC:
		if !c.pendingJump {
			c.pendingJump = true // wait for one confirming sample
			return false
		}
		c.smoothed, c.hist = raw, nil // confirmed: follow it, rate starts fresh
	default:
		if gap > c.cfg.MaxSampleGap {
			c.hist = nil // widely separated readings say nothing about the rate
		}
		alpha := c.cfg.SmoothDown
		if raw > c.smoothed {
			alpha = c.cfg.SmoothUp
		}
		c.smoothed += alpha * (raw - c.smoothed)
	}
	c.pendingJump = false
	c.haveTemp, c.lastValid = true, now
	c.hist = append(c.hist, sample{now, c.smoothed})
	return true
}

// rate is the slope of the smoothed temperature over RateWindow.
func (c *Controller) rate(now time.Time) (float64, bool) {
	i := 0
	for i < len(c.hist) && now.Sub(c.hist[i].t) > c.cfg.RateWindow {
		i++
	}
	c.hist = c.hist[i:]
	if len(c.hist) < 2 {
		return 0, false
	}
	first, last := c.hist[0], c.hist[len(c.hist)-1]
	span := last.t.Sub(first.t)
	if span < c.cfg.RateWindow/2 {
		return 0, false
	}
	return (last.temp - first.temp) / span.Seconds(), true
}

func (c *Controller) boost(rate float64) int {
	for _, b := range c.cfg.RateBoost {
		if rate >= b.MinRate {
			return b.RPM
		}
	}
	return 0
}

// curveRPM interpolates the fan curve, with every point clamped to the
// verified range and the maximum speed at MaxSpeedTemp.
func (c *Controller) curveRPM(temp float64) int {
	pts := append(append([]CurvePoint(nil), c.cfg.Curve...), CurvePoint{c.cfg.MaxSpeedTemp, c.maxRPM})
	if temp <= pts[0].TempC {
		return c.clamp(pts[0].RPM)
	}
	for i := 1; i < len(pts); i++ {
		lo, hi := pts[i-1], pts[i]
		if temp <= hi.TempC {
			a, b := float64(c.clamp(lo.RPM)), float64(c.clamp(hi.RPM))
			f := (temp - lo.TempC) / (hi.TempC - lo.TempC)
			return int(math.Round(a + f*(b-a)))
		}
	}
	return c.maxRPM
}

func (c *Controller) clamp(rpm int) int {
	return min(max(rpm, c.minRPM), c.maxRPM)
}

// ramp goes up immediately, and down gradually once outside the deadband.
func (c *Controller) ramp(now time.Time, desired int) int {
	dt := min(max(now.Sub(c.lastUpdate).Seconds(), 0), 2)
	c.lastUpdate = now
	want := float64(desired)
	switch {
	case !c.haveTarget || want >= c.target:
		c.target, c.haveTarget = want, true
	case c.target-want > float64(c.cfg.DeadbandRPM):
		c.target = math.Max(want, c.target-c.cfg.RampDownRPMPerSec*dt)
	}
	return int(math.Round(c.target))
}
