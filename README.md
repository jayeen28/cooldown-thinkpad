# cooldown

**An aggressive fan controller for ThinkPads. One static binary, zero setup.**

ThinkPad firmware is conservative. It will let a CPU sit at 50–60 °C with the
fan at 3,000–4,000 RPM, even though the same fan can spin past 6,000 RPM.
`cooldown` uses that headroom. It spins the fan up as soon as the
temperature starts climbing and brings it down slowly, so the laptop runs
noticeably cooler.

```
ThinkPad Fan Controller
Model: ThinkPad T480s (20L8S1NR00)
Mode:  Aggressive Cooling (active control)

CPU Temperature:  51.4°C  (raw 52°C)
Rise Rate:        -0.10°C/s
Target Fan Speed: 5,098 RPM
Actual Fan Speed: 6,622 RPM
Fan Level:        full-speed
Status:           Cooling
```

- **Single executable.** No Go, Python, thinkfan, lm-sensors, config files or
  network access needed.
- **Measures your hardware instead of assuming.** At startup it measures the
  real RPM of every fan level on your machine.
- **Verifies control.** It reads back the fan level and RPM, and refuses to
  control a fan that doesn't respond.
- **Safe by design.** A firmware watchdog, a fail-safe for bad sensor data,
  emergency handling, and it backs off when another controller takes over.

## Quick start

Download the binary for your system from
[Releases](https://github.com/jayeen28/cooldown-thinkpad/releases), then:

**Linux**

```sh
chmod +x cooldown-linux-amd64
sudo ./cooldown-linux-amd64
```

**Windows** (monitoring only, see [Windows](#windows)): right-click
`cooldown-windows-amd64.exe` → *Run as administrator*.

Press **Ctrl+C** to stop. The fan goes back to the mode it was in before
(normally `auto`).

### Options

None are needed for normal use.

| Flag        | What it does                                                         |
|-------------|----------------------------------------------------------------------|
| `--status`  | Show model, sensors, fan state and control support. Changes nothing. |
| `--dry-run` | Show the decisions the controller would make. Never sends a fan command. |
| `--verbose` | Print one log line per second instead of the status screen.         |
| `--version` | Print the version.                                                   |
| `--help`    | Usage.                                                               |

`--status` and `--dry-run` work without root. A good first step:

```sh
./cooldown-linux-amd64 --status
```

### Linux prerequisite: enable manual fan control

The `thinkpad_acpi` kernel driver ignores fan commands unless it was loaded
with `fan_control=1`. `--status` tells you if this is missing. To enable it
once:

```sh
echo 'options thinkpad_acpi fan_control=1' | sudo tee /etc/modprobe.d/thinkpad_acpi.conf
sudo reboot
```

## How it works

### 1. Discovery

- **Model** from DMI.
- **CPU temperatures** from hwmon: `coretemp` (package and cores) on Intel,
  `k10temp` or `zenpower` on AMD. If none are available it falls back to the
  `x86_pkg_temp` thermal zone, then the embedded controller's CPU sensor.
- **Fan control** through `/proc/acpi/ibm/fan`. The levels it supports are
  parsed from the driver, not assumed.
- **Other fan controllers.** If thinkfan, thinkfan-ui, fancontrol, nbfc and
  similar tools are running, cooldown stays in monitor-only mode instead of
  fighting them.

### 2. Calibration (about 1–2 minutes, fan loud)

Before taking control, cooldown sets each level, **fastest first** so the
laptop cools while it measures, and waits for the RPM to settle. This:

- proves the firmware accepts the commands (the level reads back and the
  speed changes),
- records the real speed of each level on *your* machine,
- drops levels that aren't meaningfully faster than the one below.

If the fan doesn't follow commands, cooldown refuses to control it and only
monitors.

Measured on a ThinkPad T480s:

| Level | 1     | 2     | 3     | 4     | 5     | 6     | 7     | full-speed |
|-------|-------|-------|-------|-------|-------|-------|-------|------------|
| RPM   | 2,686 | 3,029 | 3,186 | 3,334 | 3,907 | 4,255 | 4,288 | 6,636      |

Numbered levels top out around 4,300 RPM. Anything faster requires
`full-speed` (the driver also calls it `disengaged`).

### 3. Control loop (every second)

1. Read the CPU sensors, reject invalid readings, and take the **hottest**
   valid one.
2. **Smooth** it. Smoothing follows rises quickly and falls slowly.
3. Look up the **fan curve** for a base target.
4. Add a **rise-rate boost** if the temperature is climbing fast.
5. **Clamp** the target to the measured hardware range.
6. **Ramp.** Go up immediately; come down at 200 RPM/s, with a 150 RPM
   deadband so the fan doesn't hunt.
7. Pick the **lowest level that covers the target**. Steps down are held for
   at least 4 s.
8. Send a command only when the level changes, then **read back** the level
   and RPM to check it took effect.

**Fan curve.** Values above your hardware's maximum are clamped. From 70 °C
the fastest measured level is used.

| CPU °C | 35    | 40    | 45    | 50    | 55    | 60    | 65    | ≥ 70 |
|--------|-------|-------|-------|-------|-------|-------|-------|------|
| RPM    | 2,000 | 3,000 | 4,000 | 4,800 | 5,400 | 6,000 | 6,500 | max  |

**Rise-rate boost**

| Rise rate | ≥ 0.3 °C/s | ≥ 0.8 °C/s | ≥ 1.5 °C/s |
|-----------|------------|------------|------------|
| Boost     | +200 RPM   | +500 RPM   | +800 RPM   |

This is deliberately aggressive. On a T480s the fan sits at `full-speed`
whenever the CPU is above about 47 °C. All values live in
[`config.go`](config.go) if you want a quieter curve.

## Safety

Cooling never comes at the expense of hardware safety.

- **Watchdog.** cooldown arms the `thinkpad_acpi` fan watchdog (15 s) and
  refreshes it every 5 s. If the process is killed with `kill -9` or crashes,
  the firmware puts the fan back to `auto` by itself. This was verified on
  hardware.
- **Bad sensor data.** A failed reading is never treated as 0 °C. Implausible
  values are rejected, and a single jump of more than 30 °C must be confirmed
  by the next reading. If there's no valid reading for 5 s, the fan goes back
  to firmware `auto` until readings recover.
- **Emergency.** At 85 °C, or 15 °C below the CPU's reported critical
  temperature if that is lower, maximum fan output is requested at once, even
  from an unconfirmed spike. If the temperature keeps rising for 15 s at
  maximum output, cooldown prints a warning.
- **Ownership.** If anything else changes the fan level while cooldown runs,
  it stops writing, disarms its watchdog and drops to monitor-only. It does
  not restore anything on exit, so it never overwrites another controller's
  settings.
- **Failures.** After three consecutive failed fan commands, cooldown gives the
  fan back to `auto` and stops controlling. It warns if the RPM doesn't match
  the commanded level or the fan reports 0 RPM.
- **Never touched:** level 0 (fan off), CPU clocks or voltages, power
  settings, or firmware thermal protection.
- **About `full-speed`.** The kernel documentation describes this mode as
  unregulated: the fan may exceed its rated speed. It is a documented,
  supported driver command, also used by thinkfan, and it is the only way to
  get the extra RPM. Expect some extra fan wear and noise.

## Windows

The Windows build is **monitoring only**, and says so on screen.

Windows has no documented user-mode interface for ThinkPad fan control.
Setting the speed would need a third-party kernel driver that writes
undocumented embedded-controller registers, which cooldown deliberately does
not do. On Windows it:

- reads ACPI thermal-zone temperatures through WMI (Administrator required).
  These are firmware zones, not necessarily the CPU die, and some firmware
  updates them rarely;
- shows fan RPM only if Windows reports one (usually *Unavailable*);
- shows what the controller *would* do.

For active control, use Linux.

## Limitations

- **Supported systems.** Linux needs root, or write access to
  `/proc/acpi/ibm/fan`, plus `thinkpad_acpi fan_control=1`. Only amd64
  binaries are published.
- **Restoring the previous level.** This happens only on a normal exit
  (Ctrl+C or SIGTERM). After `kill -9` the watchdog restores `auto` instead.
  A power loss resets the embedded controller anyway.
- **Dual-fan models.** Both fans follow the same level, and calibration
  measures fan 1.
- **Dry run.** No calibration is done, so targets are shown against the
  example curve range, not your measured levels.
- **No config file.** This is by design; tune `config.go` and rebuild.

## Tested on

| Machine | OS | Result |
|---------|----|--------|
| ThinkPad T480s (20L8S1NR00) | Linux, kernel 7.0 | Active control verified: calibration, level and RPM readback, curve and boost, emergency at 96 °C, Ctrl+C restore, `kill -9` watchdog recovery, refusal while thinkfan-ui ran |
| Windows | — | Builds and vets; **not yet run on real hardware** |

Reports from other models are very welcome. Please open an issue with your
`--status` output.

## Building from source

Requires Go 1.24+. There are no third-party modules.

```sh
make            # vet (linux + windows), test, build both binaries into dist/
make test       # vet + unit tests only
make linux      # dist/cooldown-linux-amd64        (static, CGO_ENABLED=0)
make windows    # dist/cooldown-windows-amd64.exe
```

Releases are built by GitHub Actions when a `v*` tag is pushed.

### Source layout

| File | Purpose |
|------|---------|
| [`config.go`](config.go) | All tunables: curve, boosts, ramp, safety thresholds |
| [`control.go`](control.go) | Hardware-free controller: validation, smoothing, rise rate, curve, ramp, emergency |
| [`fan.go`](fan.go) | Level picking, command verification, calibration (I/O injected, so it is testable) |
| [`run.go`](run.go) | Control loop: modes, ownership, fail-safe, cleanup |
| [`ui.go`](ui.go) | Status screen and log lines |
| [`main.go`](main.go) | Flags and `--status` |
| [`hw_linux.go`](hw_linux.go) | `thinkpad_acpi` procfs, hwmon, thermal zones |
| [`hw_windows.go`](hw_windows.go), [`wmi_windows.go`](wmi_windows.go) | WMI sensors via raw COM, model from the registry, elevation check |

The control algorithm is unit-tested without hardware. The tests cover:

- curve, clamping, rise rate and boost;
- invalid, stale and implausible readings;
- ramping, hysteresis, emergency and rising-at-max;
- level picking, override detection, RPM mismatch, stopped fan and write
  failures;
- calibration: ignored commands, rejected commands, no tachometer, too hot,
  cancelled;
- competitor detection;
- a dry-run of the real loop against a fake fan file, checking that nothing
  is written.

## Disclaimer

cooldown changes how your laptop's fan is driven. It relies on documented
kernel interfaces and keeps firmware protections in place, but you use it at
your own risk. ThinkPad is a trademark of Lenovo; this project is not
affiliated with Lenovo.
