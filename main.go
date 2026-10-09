// Command cooldown is an aggressive ThinkPad fan controller.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
)

var version = "1.0.0"

func main() {
	var showVersion, status, dryRun, verbose bool
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.BoolVar(&status, "status", false, "show detected hardware and current readings, change nothing")
	flag.BoolVar(&dryRun, "dry-run", false, "compute and show fan decisions without sending any fan command")
	flag.BoolVar(&verbose, "verbose", false, "log every control cycle instead of a status screen")
	flag.Usage = usage
	flag.Parse()
	if flag.NArg() > 0 {
		usage()
		os.Exit(2)
	}
	if showVersion {
		fmt.Printf("cooldown %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return
	}

	hw, err := openHardware()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cooldown:", err)
		os.Exit(1)
	}
	if status {
		printStatus(hw)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, hw, dryRun, verbose)
	stop()
	os.Exit(code)
}

func usage() {
	fmt.Fprint(os.Stderr, `cooldown - aggressive ThinkPad fan controller

Usage:
  sudo cooldown            take control of the fan and keep the laptop cool
  cooldown --status        show hardware, sensors and fan state, then exit
  cooldown --dry-run       show what would be done; never touches the fan
  cooldown --verbose       print one log line per control cycle
  cooldown --version       print version
  cooldown --help          this help

Linux needs root for active control. Windows needs Administrator.
Press Ctrl+C to stop; the fan is handed back to its previous mode.
`)
}

func printStatus(hw *hardware) {
	cfg := DefaultConfig()
	fmt.Println("ThinkPad Fan Controller — status (no changes made)")
	fmt.Printf("Model:           %s\n", hw.Model())
	fmt.Printf("OS:              %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Println("Temperatures:")
	for _, r := range hw.ReadTemps() {
		role := ""
		if r.Control {
			role = "  [control]"
		}
		fmt.Printf("  %-28s %s%s\n", r.Name, fmtTemp(r.Celsius, r.OK), role)
	}
	t, ok := controlTemp(hw.ReadTemps(), cfg)
	fmt.Printf("Control temp:    %s (hottest CPU reading)\n", fmtTemp(t, ok))
	crit := hw.CritTemp()
	if crit > 0 {
		fmt.Printf("CPU critical:    %.0f°C (reported by hardware)\n", crit)
	}
	fmt.Printf("Emergency at:    %.0f°C\n", emergencyFor(cfg, crit))

	f := hw.ReadFan()
	fmt.Printf("Fan interface:   %s\n", hw.Interface())
	fmt.Printf("Fan speed:       %s\n", fmtFanRPM(f))
	if f.LevelOK {
		fmt.Printf("Fan level:       %s\n", f.Level)
	} else {
		fmt.Println("Fan level:       Unavailable")
	}
	if ok, reason, _ := hw.ControlSupport(); ok {
		fmt.Printf("Manual control:  supported, levels %s\n", strings.Join(hw.ControlLevels(), ", "))
		fmt.Println("                 (RPM per level is measured when control starts)")
	} else if len(hw.ControlLevels()) > 0 {
		fmt.Printf("Manual control:  supported (levels %s) but not available now:\n  %s\n",
			strings.Join(hw.ControlLevels(), ", "), reason)
	} else {
		fmt.Printf("Manual control:  unavailable\n  %s\n", reason)
	}
	if c := hw.Competitors(); len(c) > 0 {
		fmt.Printf("Other fan controllers running: %s\n", strings.Join(c, ", "))
	}
}
