# PiClock

Drives a real analog **quartz (Lavet-motor) wall clock** from a Raspberry Pi by
pulsing the movement's coil through GPIO, keeping the physical hands locked to
true time. Designed to run unattended forever.

## Hardware

| Part | Connection | Address |
|------|-----------|---------|
| Clock coil | GPIO 12 & GPIO 13 (alternating polarity) | — |
| DS3231 RTC | I2C bus 1 | `0x68` |
| MB85RC256V FRAM | I2C bus 1 | `0x50` |

Direct GPIO drive (no diode clamp); pulse duty cycle softens the 3.3 V toward
the motor's ~1.5 V.

## How it works

- The **DS3231 RTC** is the source of truth for time; **NTP** trims the RTC
  hourly (multi-server fallback).
- Each second the tick loop compares the RTC to the tracked hand position and
  ticks the coil forward to stay in sync. When the hands are ahead it holds and
  lets real time catch up. Catch-up uses fast-forward (multiple ticks/sec).
- The hand position **and coil polarity** are persisted to FRAM after every
  tick (dual-slot, CRC-protected, torn-write safe), so the clock resumes exactly
  after a power loss or reboot.

### Reverse ticking

Reverse (the ESPCLOCK4 short→gap→long waveform) is implemented and tunable but
**disabled by default** — it is finicky, clock-specific, and slips on movements
that resist it. Calibrate with `-test rev` before enabling. Forward-only
operation is fully reliable and keeps correct time on its own.

## Build & deploy

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o piclock .
scp piclock dietpi@<pi>:/tmp/piclock
ssh dietpi@<pi> 'sudo install -m755 /tmp/piclock /usr/bin/piclock && sudo systemctl restart piclock'

# install the service unit (once)
scp piclock.service dietpi@<pi>:/tmp/
ssh dietpi@<pi> 'sudo install -m644 /tmp/piclock.service /etc/systemd/system/ && sudo systemctl enable --now piclock'
```

`CGO_ENABLED=0` is required or the cross-build pulls in `net`'s cgo and fails.

Quality gates (must be clean before deploy):

```bash
gofmt -l . && go vet ./... && staticcheck ./...
```

## Usage

```
piclock                          run the clock (default)
piclock -set -hour H -minute M -second S   align stored position to the physical hands, then exit
piclock -test fwd [-count N]     forward calibration ticks (N exact, else until Ctrl-C)
piclock -test rev [-count N]     reverse calibration ticks
piclock -config /path/clock.json use a specific tick-parameter file
```

Hours are 0–11 (12 accepted as the 12-o'clock origin).

### Aligning the hands

The software must know where the hands physically point. Set the hands to a
known time, then:

```bash
sudo systemctl stop piclock
sudo /usr/bin/piclock -set -hour 12 -minute 0 -second 0
sudo systemctl start piclock
```

## Configuration / calibration

Tick parameters live in `/etc/piclock/clock.json` (see `clock.json`), loaded at
startup so you can calibrate **without recompiling**. Pulses are software-PWM
(100 µs carrier, `*_on_us` high; `100` = solid). Use `-test fwd`/`-test rev`,
watch the hand, adjust, repeat. The `-test ... -count 60` mode ticks exactly one
revolution so the second hand should return to its start mark if no steps are
missed.

## Reliability

- **Power loss / reboot:** FRAM dual-slot CRC records restore position + polarity.
- **Pulse timing:** tick thread runs `SCHED_FIFO`; the GC is disabled and run
  between pulses, so pulse energy is steady.
- **RTC / NTP failure:** RTC carries time through outages; oscillator-stop (OSF)
  is detected and the clock free-wheels rather than driving to a wrong position.
- **Crash / hang:** `systemd Type=notify` watchdog (15 s) + `Restart=always`.

See `CLAUDE.md` for design details and the Go-adapted Power-of-Ten coding rules.
