package main

import (
	"encoding/json"
	"os"

	"piclock/functions"
)

// TickConfig holds the per-clock pulse-timing parameters. The Lavet-motor pulse
// recipe is "extremely sensitive" and clock-specific, so these live in a JSON
// file (default /etc/piclock/clock.json) and can be calibrated without
// recompiling. Defaults are the published ESPCLOCK4 values for direct GPIO
// drive (3.3V, no clamp); duty cycles soften the pulse toward the motor's ~1.5V.
//
// Pulses are software-PWM on the Pi: a 100us carrier where *OnUS* of each 100us
// is driven high. OnUS=100 means a solid pulse (use that if you add a clamp).
type TickConfig struct {
	NormTickMS int `json:"norm_tick_ms"` // forward tick pulse width (tracking, 1 Hz)
	NormOnUS   int `json:"norm_on_us"`   // forward tick duty (of 100us)
	FwdTickMS  int `json:"fwd_tick_ms"`  // fast-forward tick pulse width
	FwdOnUS    int `json:"fwd_on_us"`    // fast-forward tick duty
	FwdRate    int `json:"fwd_rate"`     // fast-forward ticks per second (cap)

	RevALo   int `json:"rev_a_lo"`    // region A applies for RevALo <= second < RevAHi
	RevAHi   int `json:"rev_a_hi"`    //
	RevAT1MS int `json:"rev_a_t1_ms"` // region A short pulse width
	RevAT2MS int `json:"rev_a_t2_ms"` // region A gap before long pulse
	RevAT3MS int `json:"rev_a_t3_ms"` // region A long pulse width
	RevAOnUS int `json:"rev_a_on_us"` // region A duty

	RevBT1MS int `json:"rev_b_t1_ms"` // region B short pulse width
	RevBT2MS int `json:"rev_b_t2_ms"` // region B gap before long pulse
	RevBT3MS int `json:"rev_b_t3_ms"` // region B long pulse width
	RevBOnUS int `json:"rev_b_on_us"` // region B duty

	RevRate int `json:"rev_rate"` // fast-reverse ticks per second (cap)

	RevEnabled bool `json:"reverse_enabled"` // false => never reverse (forward-only/hold)

	RevFlipStart bool `json:"rev_flip_start"` // flip coil polarity before the reverse waveform
}

// DefaultTickConfig returns the ESPCLOCK4 20cm-clock parameters.
func DefaultTickConfig() TickConfig {
	return TickConfig{
		NormTickMS: 31, NormOnUS: 60,
		FwdTickMS: 32, FwdOnUS: 60, FwdRate: 4,
		RevALo: 35, RevAHi: 55,
		RevAT1MS: 10, RevAT2MS: 7, RevAT3MS: 28, RevAOnUS: 90,
		RevBT1MS: 10, RevBT2MS: 7, RevBT3MS: 28, RevBOnUS: 82,
		RevRate:    2,
		RevEnabled: true,
	}
}

// validate checks that every parameter is in a sane range.
func (c TickConfig) validate() error {
	okPulse := c.NormTickMS > 0 && c.NormTickMS <= 200 && c.FwdTickMS > 0 && c.FwdTickMS <= 200
	if err := functions.Assert(okPulse, "forward pulse widths in 1..200ms"); err != nil {
		return err
	}
	okDuty := inRange(c.NormOnUS, 1, 100) && inRange(c.FwdOnUS, 1, 100) &&
		inRange(c.RevAOnUS, 1, 100) && inRange(c.RevBOnUS, 1, 100)
	if err := functions.Assert(okDuty, "duty cycles in 1..100us"); err != nil {
		return err
	}
	okRev := c.RevAT1MS > 0 && c.RevAT3MS > 0 && c.RevBT1MS > 0 && c.RevBT3MS > 0 &&
		inRange(c.RevALo, 0, 59) && inRange(c.RevAHi, 0, 60) && c.RevALo < c.RevAHi
	if err := functions.Assert(okRev, "reverse params valid"); err != nil {
		return err
	}
	okRate := c.FwdRate >= 1 && c.FwdRate <= 10 && c.RevRate >= 1 && c.RevRate <= 10
	return functions.Assert(okRate, "tick rates in 1..10 per second")
}

// inRange reports whether v is within [lo, hi].
func inRange(v, lo, hi int) bool {
	return v >= lo && v <= hi
}

// LoadTickConfig reads a TickConfig from path, falling back to defaults when the
// file is absent. A present-but-invalid file is an error (fail loud).
func LoadTickConfig(path string) (TickConfig, error) {
	cfg := DefaultTickConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return DefaultTickConfig(), err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return DefaultTickConfig(), err
	}
	if err := cfg.validate(); err != nil {
		return DefaultTickConfig(), err
	}
	return cfg, nil
}
