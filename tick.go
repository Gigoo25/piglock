package main

import (
	"time"

	"piclock/functions"

	rpio "github.com/stianeikeland/go-rpio/v4"
)

// action is the tick-loop decision for one iteration.
type action int

const (
	actHold        action = iota // hands are at the target: wait for time to advance
	actStepNormal                // forward one step at 1 Hz (tracking)
	actStepFast                  // forward one step quickly (catch up)
	actStepReverse               // reverse one step (hands are ahead)

	pwmPeriodUS = 100 // software-PWM carrier period: 100us => 10 kHz
)

// position is the physical hand position on a 12-hour face.
type position struct {
	h, m, s int // h in 0..11, m and s in 0..59
}

// toDial converts a position to seconds-on-the-dial in [0, dialSeconds).
func (p position) toDial() int {
	return (p.h%12)*3600 + p.m*60 + p.s
}

// valid reports whether the position fields are in range.
func (p position) valid() bool {
	return p.h >= 0 && p.h <= 11 && p.m >= 0 && p.m <= 59 && p.s >= 0 && p.s <= 59
}

// advance moves the hand position forward by one second, wrapping at 12 hours.
func (p position) advance() position {
	p.s++
	if p.s >= 60 {
		p.s = 0
		p.m++
		if p.m >= 60 {
			p.m = 0
			p.h++
			if p.h >= 12 {
				p.h = 0
			}
		}
	}
	return p
}

// retreat moves the hand position back by one second, wrapping at 12 hours.
func (p position) retreat() position {
	p.s--
	if p.s < 0 {
		p.s = 59
		p.m--
		if p.m < 0 {
			p.m = 59
			p.h--
			if p.h < 0 {
				p.h = 11
			}
		}
	}
	return p
}

// busyWaitUS spins until us microseconds have elapsed. On the SCHED_FIFO tick
// thread, with the GC disabled during pulses, this gives steady pulse energy.
func busyWaitUS(us int) {
	end := time.Now().Add(time.Duration(us) * time.Microsecond)
	for time.Now().Before(end) {
	}
}

// pulsePWM drives pin high for durationMS, software-PWM modulated at duty onUS
// (out of 100us) to soften the Pi's 3.3V toward the Lavet motor's ~1.5V. onUS
// >= 100 is a solid pulse. The carrier loop has a fixed upper bound.
func pulsePWM(pin rpio.Pin, durationMS, onUS int) {
	if onUS >= pwmPeriodUS {
		pin.High()
		busyWaitUS(durationMS * 1000)
		pin.Low()
		return
	}
	end := time.Now().Add(time.Duration(durationMS) * time.Millisecond)
	offUS := pwmPeriodUS - onUS
	maxCycles := durationMS*1000/pwmPeriodUS + 1
	for i := 0; i < maxCycles && time.Now().Before(end); i++ {
		pin.High()
		busyWaitUS(onUS)
		pin.Low()
		busyWaitUS(offUS)
	}
	pin.Low()
}

// flipPin alternates the driven coil pin (polarity must alternate every step).
func (c *Clock) flipPin() {
	if c.cur == c.pin1 {
		c.cur = c.pin2
	} else {
		c.cur = c.pin1
	}
}

// pinB reports whether the current coil pin is pin2 (persisted so reverse starts
// on the correct polarity after a restart).
func (c *Clock) pinB() bool {
	return c.cur == c.pin2
}

// persist writes hand position and pin polarity to FRAM (no-op without FRAM,
// e.g. in calibration test mode).
func (c *Clock) persist() error {
	if c.fram == nil {
		return nil
	}
	return c.fram.WriteState(c.pos.h, c.pos.m, c.pos.s, c.pinB())
}

// forwardTick emits one forward pulse, alternates polarity, advances, persists.
func (c *Clock) forwardTick(fast bool) error {
	if err := functions.Assert(c.cur == c.pin1 || c.cur == c.pin2, "current pin valid"); err != nil {
		return err
	}
	ms, onUS := c.cfg.NormTickMS, c.cfg.NormOnUS
	if fast {
		ms, onUS = c.cfg.FwdTickMS, c.cfg.FwdOnUS
	}
	pulsePWM(c.cur, ms, onUS)
	c.flipPin()
	c.pos = c.pos.advance()
	if err := functions.Assert(c.pos.valid(), "position valid after advance"); err != nil {
		return err
	}
	return c.persist()
}

// reverseTick emits the ESPCLOCK4 reverse waveform: a short pulse on the current
// pin, a gap, then a long opposite-polarity pulse. Region A/B parameters are
// chosen by the second-hand position. It retreats one step and persists.
func (c *Clock) reverseTick() error {
	if err := functions.Assert(c.cur == c.pin1 || c.cur == c.pin2, "current pin valid"); err != nil {
		return err
	}
	if c.cfg.RevFlipStart {
		c.flipPin() // start the reverse waveform on the opposite polarity
	}
	t1, t2, t3, onUS := c.revParams(c.pos.s)
	pulsePWM(c.cur, t1, onUS) // short pulse, current polarity
	c.cur.Low()
	busyWaitUS(t2 * 1000)     // gap, both pins low
	c.flipPin()               // opposite polarity
	pulsePWM(c.cur, t3, onUS) // long pulse
	c.pos = c.pos.retreat()
	if err := functions.Assert(c.pos.valid(), "position valid after retreat"); err != nil {
		return err
	}
	return c.persist()
}

// revParams returns the reverse pulse timings for the given second-hand value.
func (c *Clock) revParams(second int) (t1, t2, t3, onUS int) {
	if second >= c.cfg.RevALo && second < c.cfg.RevAHi {
		return c.cfg.RevAT1MS, c.cfg.RevAT2MS, c.cfg.RevAT3MS, c.cfg.RevAOnUS
	}
	return c.cfg.RevBT1MS, c.cfg.RevBT2MS, c.cfg.RevBT3MS, c.cfg.RevBOnUS
}

// decide chooses the tick action by shortest correction time. Forward is
// reliable; reverse is used when the hands are ahead and reversing reaches the
// target in less wall-clock time than fast-forwarding the long way around.
func decide(handDial, targetDial, fwdRate, revRate int) action {
	fwd := ((targetDial-handDial)%dialSeconds + dialSeconds) % dialSeconds
	if fwd == 0 {
		return actHold
	}
	if fwd <= normalThreshold {
		return actStepNormal
	}
	bwd := dialSeconds - fwd
	// Forward time fwd/fwdRate vs reverse time bwd/revRate; cross-multiply.
	if fwd*revRate <= bwd*fwdRate {
		return actStepFast
	}
	return actStepReverse
}
