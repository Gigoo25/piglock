//go:build linux

package functions

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// schedFIFO is the SCHED_FIFO real-time scheduling policy.
const schedFIFO = 1

// schedParam mirrors struct sched_param: a single priority field.
type schedParam struct {
	priority int32
}

// SetThreadRealtime pins the calling goroutine to its OS thread and raises that
// thread to SCHED_FIFO at the given priority (1..99). This removes scheduler
// jitter from the pulse path (Power-of-Ten threat: missed/double steps).
//
// The thread is pinned even if raising priority is denied (e.g. missing
// CAP_SYS_NICE); the caller should log the error and continue best-effort.
func SetThreadRealtime(priority int) error {
	if err := Assert(priority >= 1 && priority <= 99, "rt priority in 1..99"); err != nil {
		return err
	}
	runtime.LockOSThread()
	p := schedParam{priority: int32(priority)}
	// pid 0 = the calling thread, which LockOSThread has fixed in place.
	_, _, errno := unix.RawSyscall(unix.SYS_SCHED_SETSCHEDULER, 0, schedFIFO, uintptr(unsafe.Pointer(&p)))
	if errno != 0 {
		return fmt.Errorf("sched_setscheduler: %w", errno)
	}
	return nil
}
