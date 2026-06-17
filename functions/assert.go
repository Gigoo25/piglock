package functions

import "errors"

// Assert implements Power-of-Ten rule 5: a side-effect-free boolean test of an
// invariant that should never fail in real execution. On failure it returns an
// error so the caller can take an explicit recovery action; it never panics.
func Assert(cond bool, msg string) error {
	if !cond {
		return errors.New("assertion failed: " + msg)
	}
	return nil
}
