# PiClock (piglock)

Drive a real analog **quartz Lavet-motor** wall clock from a Raspberry Pi by
pulsing the movement's coil through GPIO. The Pi keeps the physical hands
locked to true time. **This must be bulletproof** — it runs unattended forever.

This Go repo (`piglock`) is the **source of truth**. It compiles to a single
static binary deployed as `/usr/bin/piclock` and run by `piclock.service`.

> History note: sibling dirs `piclock/` (Python) and a since-deleted
> `piclock_go/` are abandoned. Ignore them. The binary currently running on the
> Pi was built from orphaned Go source that no longer exists; this repo replaces
> it.

## Hardware

| Part | Bus / Pin | Address | Notes |
|------|-----------|---------|-------|
| Pi | — | — | dietpi, aarch64 (`linux/arm64`), kernel 6.12 |
| Clock coil | GPIO 12 & GPIO 13 | — | Lavet motor. Drive with **alternating polarity** pulses — flip which pin is driven each tick. |
| DS3231 RTC | I2C bus 1 | `0x68` | Battery-backed. **Source of truth for time.** |
| MB85RC256V FRAM | I2C bus 1 | `0x50` | Persists physical hand position across power loss. |

### Lavet motor facts that constrain the code
- A Lavet motor steps **one second per pulse**. Polarity must alternate every
  step or the motor stalls.
- Forward step: single ~32 ms pulse.
- Reverse: a quartz movement has no native reverse. It's faked with the
  ESPCLOCK4 waveform — a short pulse, a gap, then a longer opposite-polarity
  pulse — that nudges the rotor backward. It is finicky and **clock-specific**;
  it must be calibrated per movement (`piclock -test rev`) and can slip if
  mistuned. Region A/B params differ for the 35–55 s arc.
- **Pin polarity must be tracked and persisted.** Polarity alternates every
  step (forward or reverse). Starting reverse on the wrong pin causes major
  slippage, so the current pin is stored in FRAM alongside position.
- Max safe step rate is a handful per second. Never pulse faster than the
  rotor can physically follow or steps are silently dropped.
- **There is no position feedback sensor.** The code cannot directly confirm a
  hand moved. Reliability comes from clean, correctly-timed pulses + persisted
  position, not verification. (A sense coil / photo-interrupter would be needed
  for true closed-loop verification — not present.)

### Known environment issues
- `dtoverlay=i2c-rtc,ds1307` is in config but **`/dev/rtc0` does not bind** — the
  kernel RTC is dead. We talk to the DS3231 directly over I2C, so this is fine,
  but kernel `hwclock` / boot-time restore is unavailable. Do not rely on it.
- `i2cdetect` is not installed on the Pi.
- SPI is disabled (fine — FRAM is on I2C).

## Requirements — what "bulletproof" means

Four threats, all in scope, in priority order:

1. **Power loss / reboot.** Hand position survives a sudden power cut, including
   one *during* a write, and is restored exactly. → dual-slot CRC'd FRAM records
   (`functions/fram.go`). On boot, restore position, read RTC, then catch up.
2. **Missed / double pulses.** The coil must get one clean, correctly-timed
   pulse per step and never double-step beyond what the rotor can follow.
   → real-time scheduling + busy-waited pulse windows, capped catch-up rate.
3. **NTP / RTC failure.** Keep correct time through network outages, NTP
   failures, and RTC battery loss. → RTC is truth; NTP only trims the RTC
   periodically. Detect RTC oscillator-stop (OSF); free-wheel at 1 Hz when time
   source is untrusted rather than driving hands to a wrong position.
4. **Crash auto-recovery.** Self-heal on panic/hang. → `systemd Restart=always`
   + `Type=notify` watchdog; on restart, re-derive position from FRAM and
   re-sync. Pins driven low on exit.

## Layout

```
piclock.go            main: lifecycle, tick loop, RTC decision, -set/-test modes
tick.go               pulse PWM, forward/reverse ticks, position, decide()
config.go             TickConfig: JSON tick params + defaults (calibration)
clock.json            sample config -> deploy to /etc/piclock/clock.json
functions/fram.go     dual-slot CRC persistence of position + pin polarity (threat 1)
functions/ds3231.go   RTC: read/write, OSF detection (threat 3)
functions/ntp.go      periodic NTP -> RTC trim, multi-server (threat 3)
functions/rt.go       OS-thread pin + SCHED_FIFO (threat 2)
functions/watchdog.go sd_notify READY/WATCHDOG (threat 4)
piclock.service       systemd unit
```

## Direction & calibration

The tick loop chooses forward or reverse by **shortest correction time**
(`decide()` in `tick.go`): forward at `fwd_rate`, reverse at `rev_rate`, pick
the fewer-ticks path. Forward is reliable; reverse is finicky.

Tick parameters live in `/etc/piclock/clock.json` (see `clock.json`), loaded at
startup — **calibrate without recompiling**. Pulses are software-PWM (100 µs
carrier, `*_on_us` high) to soften the Pi's 3.3 V toward the motor's ~1.5 V on
direct drive; set `*_on_us: 100` for solid pulses if you fit a diode clamp.

Calibration:
- `piclock -test fwd` — tick forward forever; confirm every pulse advances one
  step (tune `fwd_tick_ms` / `fwd_on_us` / `fwd_rate`).
- `piclock -test rev` — tick reverse forever; the hardest to tune. Tune
  `rev_*` per region; verify the hand returns to start over a long run.
- `reverse_enabled: false` disables reverse entirely (forward-only/hold) until
  you trust the calibration.

## Build & deploy

```bash
# from this dir, on the dev machine
go mod download                                   # first time / after dep changes
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o piclock .   # CGO off or net pulls cgo and cross-build fails
scp piclock dietpi@192.168.80.165:/tmp/piclock
ssh dietpi@192.168.80.165 'sudo install -m755 /tmp/piclock /usr/bin/piclock && sudo systemctl restart piclock'
ssh dietpi@192.168.80.165 'journalctl -u piclock -f'

# tick config (optional; built-in defaults used if absent):
scp clock.json dietpi@192.168.80.165:/tmp/clock.json
ssh dietpi@192.168.80.165 'sudo mkdir -p /etc/piclock && sudo install -m644 /tmp/clock.json /etc/piclock/clock.json && sudo systemctl restart piclock'
```

Quality gates — **must pass with zero output before every deploy** (see Rule 10):

```bash
gofmt -l .            # must print nothing
go vet ./...
staticcheck ./...     # go install honnef.co/go/tools/cmd/staticcheck@latest
```

## Coding rules — Power of Ten (Go-adapted)

We follow NASA/JPL's "Power of Ten" as closely as Go allows. Several rules are
C-specific; the Go reading is given. Hold to these for **all** code here.

1. **Simple control flow.** No `goto`. **No recursion** (direct or indirect) —
   a static caller graph must be acyclic. No `panic`/`recover` for control flow
   (recover only at goroutine top to log + exit cleanly).
2. **Bounded loops.** Every loop has a statically obvious fixed upper bound.
   *Sole exception:* the top-level service loop and long-lived goroutine loops,
   which are intentionally unbounded but **must** be governed by a
   `context.Context` and a `for { select { ... } }` shape. No other unbounded
   loops. Retry loops use a constant cap (e.g. `for i := 0; i < 3; i++`).
3. **No dynamic allocation in steady state.** GC can't be removed, but the hot
   path (the per-tick loop and pulse code) must not allocate: preallocate
   buffers, no `make`/`append`/string formatting per tick. Allocation is allowed
   only during init/setup.
4. **Short functions.** ≤ 60 lines each. Split anything longer.
5. **Assertions, ≥2 per function.** Go has no `assert`. Use the `must`/`require`
   helpers (in `functions/assert.go`) for invariants that should never fail;
   each must be a side-effect-free boolean test that triggers an explicit
   recovery (return an error, or at goroutine top, log + exit). Validate
   anomalous, never-should-happen conditions — not ordinary errors.
6. **Smallest scope.** Declare every variable at the tightest scope. No package
   globals for mutable state except where a single owner guards them with a
   mutex (documented).
7. **Check every return value; validate every parameter.** No ignored errors
   (`_ =` only with a comment justifying it). Exported functions validate their
   args at entry and return an error on bad input.
8. **No metaprogramming.** (C preprocessor rule.) Go reading: no `go:generate`
   trickery, no codegen, build tags only for OS portability (`//go:build
   linux`). Keep constants as typed `const`.
9. **Restrict pointers; no function pointers.** At most one level of pointer
   indirection (no `**T`). Do not store func-typed struct fields or pass
   callbacks where a plain method/branch works — keep the call graph statically
   visible.
10. **Zero-warning, analyzed daily.** Build clean. `gofmt`, `go vet`, and
    `staticcheck` must all pass with **zero** findings before any commit or
    deploy. Treat their output as errors.

When a rule genuinely cannot be met (e.g. the unbounded service loop), document
the exception inline with `// PoT rule N exception:` and the reason.
