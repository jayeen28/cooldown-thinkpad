package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ui draws a small status screen on a terminal, or prints periodic lines otherwise.
type ui struct {
	model    string
	mode     string
	verbose  bool
	tty      bool
	events   []string
	progMsg  string
	body     []string
	lastLine time.Time
}

func newUI(verbose bool, model string) *ui {
	u := &ui{model: model, verbose: verbose}
	if st, err := os.Stdout.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 && !verbose {
		u.tty = enableANSI()
	}
	return u
}

func (u *ui) event(msg string) {
	line := time.Now().Format("15:04:05") + "  " + msg
	if !u.tty {
		fmt.Println(line)
		return
	}
	u.events = append(u.events, line)
	if len(u.events) > 8 {
		u.events = u.events[len(u.events)-8:]
	}
	u.draw(nil)
}

func (u *ui) progress(msg string) {
	if !u.tty {
		if msg != "" {
			fmt.Println(time.Now().Format("15:04:05") + "  " + msg)
		}
		return
	}
	u.progMsg = msg
	u.draw(nil)
}

func (u *ui) show(d Decision, fan FanState, level, status string) {
	lines := []string{
		"CPU Temperature:  " + fmtTemp(d.Temp, !d.FailSafe) + rawNote(d),
		"Rise Rate:        " + fmtRate(d),
		"Target Fan Speed: " + fmtTarget(d),
		"Actual Fan Speed: " + fmtFanRPM(fan),
		"Fan Level:        " + fmtLevel(fan, level),
		"Status:           " + status,
	}
	if u.tty {
		u.draw(lines)
		return
	}
	if u.verbose || time.Since(u.lastLine) >= DefaultConfig().UIRefresh {
		u.lastLine = time.Now()
		fmt.Printf("%s  temp %s  rate %s  target %s  fan %s  level %s  %s\n", time.Now().Format("15:04:05"),
			fmtTemp(d.Temp, !d.FailSafe), fmtRate(d), fmtTarget(d), fmtFanRPM(fan), fmtLevel(fan, level), status)
	}
}

func (u *ui) draw(body []string) {
	if body != nil {
		u.body = body
	}
	var b strings.Builder
	b.WriteString("\033[H\033[2J")
	b.WriteString("ThinkPad Fan Controller\n")
	b.WriteString("Model: " + u.model + "\n")
	b.WriteString("Mode:  " + u.mode + "\n\n")
	if u.progMsg != "" {
		b.WriteString(u.progMsg + "\n")
	}
	for _, l := range u.body {
		b.WriteString(l + "\n")
	}
	if len(u.events) > 0 {
		b.WriteString("\nEvents:\n")
		for _, e := range u.events {
			b.WriteString("  " + e + "\n")
		}
	}
	b.WriteString("\nCtrl+C to stop\n")
	fmt.Print(b.String())
}

// done leaves the screen in place and moves to a fresh line.
func (u *ui) done() { fmt.Println() }

func fmtTemp(c float64, ok bool) string {
	if !ok {
		return "Unavailable"
	}
	return fmt.Sprintf("%.1f°C", c)
}

func rawNote(d Decision) string {
	if d.FailSafe {
		return ""
	}
	if d.Holding {
		return "  (last reading rejected)"
	}
	return fmt.Sprintf("  (raw %.0f°C)", d.RawTemp)
}

func fmtRate(d Decision) string {
	if !d.RateOK {
		return "Unavailable"
	}
	return fmt.Sprintf("%+.2f°C/s", d.Rate)
}

func fmtTarget(d Decision) string {
	if d.FailSafe {
		return "Unavailable"
	}
	s := fmtRPM(d.TargetRPM) + " RPM"
	if d.BoostRPM > 0 {
		s += fmt.Sprintf(" (incl. +%d rise boost)", d.BoostRPM)
	}
	return s
}

func fmtFanRPM(f FanState) string {
	if !f.RPMOK || len(f.RPM) == 0 {
		return "Unavailable"
	}
	var parts []string
	for _, r := range f.RPM {
		parts = append(parts, fmtRPM(r)+" RPM")
	}
	return strings.Join(parts, ", ")
}

func fmtLevel(f FanState, ours string) string {
	switch {
	case ours != "":
		return ours
	case f.LevelOK:
		return f.Level + " (not set by cooldown)"
	default:
		return "Unavailable"
	}
}

// fmtRPM adds thousands separators: 4500 -> "4,500".
func fmtRPM(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
