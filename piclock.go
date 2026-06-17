// Command piclock drives an analog quartz (Lavet-motor) wall clock from a
// Raspberry Pi, keeping the physical hands locked to true time. It can tick
// forward (fast catch-up) and reverse (ESPCLOCK4 waveform), choosing whichever
// direction corrects the hands fastest.
//
// Design and the four bulletproofing requirements are documented in CLAUDE.md.
// The code follows a Go-adaptation of NASA/JPL's Power of Ten rules.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"piclock/functions"

	rpio "github.com/stianeikeland/go-rpio/v4"
)

const (
	tickPin1 = 12 // coil lead A
	tickPin2 = 13 // coil lead B
	i2cBus   = 1

	normalSleep     = time.Second // tracking step period and error backoff
	dialSeconds     = 12 * 3600   // positions on a 12-hour face
	normalThreshold = 1           // fwd distance treated as in-step tracking
	rtPriority      = 50          // SCHED_FIFO priority for the tick thread

	ntpFirstDelay = 5 * time.Second
	ntpInterval   = 60 * time.Minute

	defaultConfigPath = "/etc/piclock/clock.json"
	testPreTicks      = 5 // forward ticks to settle polarity before a test run
)

// Clock owns all hardware, the tick parameters, and the single authoritative
// hand position and pin polarity.
type Clock struct {
	rtc  *functions.DS3231
	fram *functions.FRAM
	cfg  TickConfig
	pin1 rpio.Pin
	pin2 rpio.Pin
	cur  rpio.Pin
	pos  position
}

// nextAction reads the time source and decides the next tick action.
func (c *Clock) nextAction() (action, error) {
	stopped, err := c.rtc.OscillatorStopped()
	if err != nil {
		return actHold, err // RTC unreadable: hold rather than move blindly
	}
	if stopped {
		return actStepNormal, nil // time untrusted: free-wheel forward at 1 Hz
	}
	h, m, s, err := c.rtc.ReadClock()
	if err != nil {
		return actHold, err
	}
	target := (h%12)*3600 + m*60 + s
	if err := functions.Assert(target >= 0 && target < dialSeconds, "target dial in range"); err != nil {
		return actHold, err
	}
	hand := c.pos.toDial()
	if err := functions.Assert(hand >= 0 && hand < dialSeconds, "hand dial in range"); err != nil {
		return actHold, err
	}
	act := decide(hand, target, c.cfg.FwdRate, c.cfg.RevRate)
	if act == actStepReverse && !c.cfg.RevEnabled {
		return actHold, nil // reverse disabled: wait for time to catch up instead
	}
	return act, nil
}

// run is the tick loop. It is the single intentionally-unbounded loop, governed
// by ctx (Power-of-Ten rule 2 exception, documented in CLAUDE.md).
func (c *Clock) run(ctx context.Context, wd *functions.Watchdog) {
	fastSleep := time.Second / time.Duration(c.cfg.FwdRate)
	revSleep := time.Second / time.Duration(c.cfg.RevRate)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		start := time.Now()
		runtime.GC() // collect now, between pulses (auto-GC is disabled)
		if err := wd.Alive(); err != nil {
			log.Printf("watchdog ping: %v", err)
		}
		act, err := c.nextAction()
		if err != nil {
			log.Printf("decision: %v", err)
			sleepRemaining(ctx, start, normalSleep)
			continue
		}
		switch act {
		case actStepFast:
			tickLog(c.forwardTick(true))
			sleepCtx(ctx, fastSleep)
		case actStepReverse:
			tickLog(c.reverseTick())
			sleepCtx(ctx, revSleep)
		case actStepNormal:
			tickLog(c.forwardTick(false))
			sleepRemaining(ctx, start, normalSleep)
		default: // actHold: keep the 1 Hz cadence so the next tick lands on time
			sleepRemaining(ctx, start, normalSleep)
		}
	}
}

// sleepRemaining sleeps so the loop iteration started at start lasts period in
// total, absorbing per-iteration work (GC, I2C reads, the pulse) so 1 Hz
// tracking does not accumulate lag and insert catch-up ticks.
func sleepRemaining(ctx context.Context, start time.Time, period time.Duration) {
	d := period - time.Since(start)
	if d < 0 {
		d = 0
	}
	sleepCtx(ctx, d)
}

// tickLog logs (but survives) a tick failure.
func tickLog(err error) {
	if err != nil {
		log.Printf("tick: %v", err)
	}
}

// shutdown drives the coil low, closes devices, then releases the GPIO mapping.
// Order matters: pins must be driven low while GPIO memory is still mapped, so
// rpio.Close() is the final step (not a separate defer, which under LIFO would
// run first and unmap memory before pin.Low()).
func (c *Clock) shutdown() {
	c.pin1.Low()
	c.pin2.Low()
	if c.fram != nil {
		c.fram.Close()
	}
	if c.rtc != nil {
		c.rtc.Close()
	}
	rpio.Close()
}

// sleepCtx sleeps for d unless ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// rtcWaitTries bounds the startup wait for the RTC (Power-of-Ten rule 2).
const rtcWaitTries = 30

// waitForRTC waits, with a fixed upper bound, for the DS3231 to respond so a
// slow or briefly-flaky I2C bus at boot does not crash-loop the service.
func waitForRTC(rtc *functions.DS3231) error {
	if err := functions.Assert(rtc != nil, "rtc non-nil"); err != nil {
		return err
	}
	for range rtcWaitTries {
		if rtc.IsAvailable() {
			return nil
		}
		time.Sleep(time.Second)
	}
	return &setupError{"DS3231 not responding after wait"}
}

// openPins configures the two coil pins as outputs driven low, current = pin1.
func (c *Clock) openPins() {
	c.pin1 = rpio.Pin(tickPin1)
	c.pin2 = rpio.Pin(tickPin2)
	c.pin1.Output()
	c.pin2.Output()
	c.pin1.Low()
	c.pin2.Low()
	c.cur = c.pin1
}

// setup opens the GPIO and I2C hardware and returns a ready Clock.
func setup(cfg TickConfig) (*Clock, error) {
	if err := rpio.Open(); err != nil {
		return nil, err
	}
	rtc, err := functions.NewDS3231(i2cBus)
	if err != nil {
		return nil, err
	}
	if err := waitForRTC(rtc); err != nil {
		return nil, err
	}
	if err := rtc.EnsureRunning(); err != nil {
		log.Printf("ensure RTC oscillator running: %v", err)
	}
	fram, err := functions.NewFRAM(i2cBus)
	if err != nil {
		return nil, err
	}
	c := &Clock{rtc: rtc, fram: fram, cfg: cfg}
	c.openPins()
	return c, nil
}

type setupError struct{ msg string }

func (e *setupError) Error() string { return e.msg }

// initPosition loads hand position and pin polarity from flags, else FRAM, else
// 12:00:00. Setting from flags resets polarity to pin1.
func (c *Clock) initPosition(h, m, s int) {
	if h >= 0 && h <= 11 && m >= 0 && m <= 59 && s >= 0 && s <= 59 {
		c.pos = position{h, m, s}
		c.cur = c.pin1
		if err := c.persist(); err != nil {
			log.Printf("persist initial position: %v", err)
		}
		log.Printf("position set from flags: %02d:%02d:%02d", h, m, s)
		return
	}
	sh, sm, ss, pinB, err := c.fram.ReadState()
	if err != nil {
		c.pos = position{0, 0, 0}
		c.cur = c.pin1
		log.Printf("no valid FRAM position, starting at 12:00:00")
		return
	}
	c.pos = position{sh, sm, ss}
	if pinB {
		c.cur = c.pin2
	} else {
		c.cur = c.pin1
	}
	log.Printf("position restored from FRAM: %02d:%02d:%02d (pinB=%v)", sh, sm, ss, pinB)
}

// ntpLoop periodically trims the RTC to NTP. It is a long-lived goroutine loop
// governed by ctx (rule 2 exception) and recovers from panics so a transient
// network library fault cannot take down the process.
func ntpLoop(ctx context.Context, rtc *functions.DS3231) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("ntp loop recovered: %v", r)
		}
	}()
	syncer := functions.NewNTPSyncer("")
	timer := time.NewTimer(ntpFirstDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			syncRTC(rtc, syncer)
			timer.Reset(ntpInterval)
		}
	}
}

// syncRTC pulls NTP time and writes it to the RTC, clearing the stop flag.
func syncRTC(rtc *functions.DS3231, syncer *functions.NTPSyncer) {
	t, err := syncer.GetCurrentTime()
	if err != nil {
		log.Printf("ntp: %v", err)
		return
	}
	if err := rtc.WriteTime(t.Hour(), t.Minute(), t.Second()); err != nil {
		log.Printf("rtc write: %v", err)
		return
	}
	if err := rtc.ClearOSF(); err != nil {
		log.Printf("clear OSF: %v", err)
	}
	log.Printf("rtc synced to ntp: %02d:%02d:%02d", t.Hour(), t.Minute(), t.Second())
}

// runSet writes the given hand position to FRAM and returns. Clean alignment
// path: no GPIO, no RT, no tick loop. Hour 12 is an alias for the 12-o'clock
// origin (stored as 0); polarity is reset to pin1.
func runSet(h, m, s int) error {
	if h < 0 || h > 12 || m < 0 || m > 59 || s < 0 || s > 59 {
		return fmt.Errorf("set requires -hour 0..12 and -minute/-second 0..59")
	}
	if h == 12 {
		h = 0
	}
	if err := functions.Assert(h >= 0 && h <= 11, "set hour normalized to 0..11"); err != nil {
		return err
	}
	fram, err := functions.NewFRAM(i2cBus)
	if err != nil {
		return err
	}
	defer fram.Close()
	if err := fram.WriteState(h, m, s, false); err != nil {
		return err
	}
	log.Printf("FRAM position set to %02d:%02d:%02d (12-o'clock origin = hour 0)", h, m, s)
	return nil
}

// runTest ticks continuously in one direction for calibration. It settles
// polarity with a few forward ticks, then ticks "fwd" or "rev" until cancelled.
// No RTC/FRAM; observe the hands and tune the config, then re-run.
func runTest(ctx context.Context, dir string, cfg TickConfig, count int) error {
	if err := functions.Assert(dir == "fwd" || dir == "rev", "test dir is fwd or rev"); err != nil {
		return err
	}
	if err := rpio.Open(); err != nil {
		return err
	}
	c := &Clock{cfg: cfg}
	c.openPins()
	defer func() { c.pin1.Low(); c.pin2.Low(); rpio.Close() }()

	rev := dir == "rev"
	sleep := time.Second / time.Duration(cfg.FwdRate)
	if rev {
		sleep = time.Second / time.Duration(cfg.RevRate)
	}
	if count <= 0 {
		for range testPreTicks { // settle polarity before an open-ended run
			tickLog(c.forwardTick(false))
			sleepCtx(ctx, normalSleep)
		}
		log.Printf("test %s: starting (Ctrl-C to stop)", dir)
	} else {
		log.Printf("test %s: %d ticks (exact) — second hand should return to start", dir, count)
	}
	for n := 0; count <= 0 || n < count; n++ {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		if rev {
			tickLog(c.reverseTick())
		} else {
			tickLog(c.forwardTick(true))
		}
		if count <= 0 && n%10 == 0 {
			log.Printf("test %s: %d ticks", dir, n)
		}
		sleepCtx(ctx, sleep)
	}
	log.Printf("test %s: done", dir)
	return nil
}

func main() {
	hourFlag := flag.Int("hour", -1, "Set hand hour (0-11; 12 = 12 o'clock)")
	minuteFlag := flag.Int("minute", -1, "Set hand minute (0-59)")
	secondFlag := flag.Int("second", -1, "Set hand second (0-59)")
	setFlag := flag.Bool("set", false, "write -hour/-minute/-second to FRAM and exit (hand alignment)")
	testFlag := flag.String("test", "", "calibration tick test: 'fwd' or 'rev' (no RTC/FRAM)")
	countFlag := flag.Int("count", 0, "exact number of ticks for -test (0 = run until Ctrl-C)")
	configPath := flag.String("config", defaultConfigPath, "path to tick-parameter JSON config")
	flag.Parse()

	if *setFlag {
		if err := runSet(*hourFlag, *minuteFlag, *secondFlag); err != nil {
			log.Fatalf("set: %v", err)
		}
		return
	}

	cfg, err := LoadTickConfig(*configPath)
	if err != nil {
		log.Fatalf("config %s: %v", *configPath, err)
	}

	// Disable automatic GC so a collection never pauses the thread mid-pulse
	// (the source of software-PWM jitter). The tick loop calls runtime.GC() in
	// the idle gap between ticks, keeping memory bounded without ever colliding
	// with a pulse.
	debug.SetGCPercent(-1)

	if err := functions.SetThreadRealtime(rtPriority); err != nil {
		log.Printf("realtime scheduling unavailable, continuing: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if *testFlag != "" {
		if err := runTest(ctx, *testFlag, cfg, *countFlag); err != nil {
			log.Fatalf("test: %v", err)
		}
		return
	}

	wd, err := functions.NewWatchdog()
	if err != nil {
		log.Printf("watchdog init: %v", err)
		wd = &functions.Watchdog{}
	}
	defer wd.Close()

	clk, err := setup(cfg)
	if err != nil {
		log.Fatalf("setup: %v", err)
	}
	defer clk.shutdown() // drives pins low and calls rpio.Close() as its last step

	clk.initPosition(*hourFlag, *minuteFlag, *secondFlag)

	go ntpLoop(ctx, clk.rtc)

	if err := wd.Ready(); err != nil {
		log.Printf("watchdog ready: %v", err)
	}
	log.Printf("starting tick loop")
	clk.run(ctx, wd)

	if err := wd.Stopping(); err != nil {
		log.Printf("watchdog stopping: %v", err)
	}
	log.Printf("clean shutdown")
}
