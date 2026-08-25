// Package clock abstracts time so that subscription Hold+Wait blocking,
// sampling-rate scheduling, and abandonment-reaper sweeps can be tested
// deterministically, with no real sleeps — see
// docs/architecture/decisions/009-clock-abstraction.md.
package clock

import "time"

// Clock abstracts the passage of time. clock.Real wraps the standard
// library directly with no behavioral change; clock/clocktest.Fake gives
// tests explicit, synchronous control.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	NewTimer(d time.Duration) Timer
	AfterFunc(d time.Duration, f func()) Timer
	Sleep(d time.Duration)
}

// Timer abstracts *time.Timer.
type Timer interface {
	Stop() bool
	Reset(d time.Duration) bool
	// C returns the timer's channel. It is only meaningful for a Timer
	// created via NewTimer; a Timer created via AfterFunc returns a
	// channel that never fires (the callback is the notification
	// mechanism instead).
	C() <-chan time.Time
}

// Real is a Clock backed directly by the standard library.
type Real struct{}

// Now returns time.Now().
func (Real) Now() time.Time { return time.Now() }

// After returns time.After(d).
func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }

// NewTimer returns a Timer wrapping time.NewTimer(d).
func (Real) NewTimer(d time.Duration) Timer { return &realTimer{t: time.NewTimer(d)} }

// AfterFunc returns a Timer wrapping time.AfterFunc(d, f).
func (Real) AfterFunc(d time.Duration, f func()) Timer { return &realTimer{t: time.AfterFunc(d, f)} }

// Sleep calls time.Sleep(d).
func (Real) Sleep(d time.Duration) { time.Sleep(d) }

type realTimer struct{ t *time.Timer }

func (r *realTimer) Stop() bool                 { return r.t.Stop() }
func (r *realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }
func (r *realTimer) C() <-chan time.Time        { return r.t.C }
