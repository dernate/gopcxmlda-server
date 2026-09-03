// Package clocktest provides a fake clock.Clock for deterministic tests.
// It is a separate package from clock so production code can never
// accidentally import it — see
// docs/architecture/decisions/009-clock-abstraction.md.
package clocktest

import (
	"sort"
	"sync"
	"time"

	"github.com/dernate/gopcxmlda-server/clock"
)

// Fake is a manually-advanced clock.Clock. Every pending After/NewTimer/
// AfterFunc fires synchronously, in deadline order, when Advance or Set
// moves the fake clock's time past its deadline — no real sleeps.
//
// AfterFunc callbacks run on the goroutine that called Advance/Set, after
// Fake's internal lock has been released, so a callback may safely call
// back into Fake (e.g. a self-rescheduling AfterFunc chain, exactly the
// pattern subscription.Manager uses) without deadlocking.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*fakeTimer
	seq     uint64
}

// New returns a Fake whose current time is start.
func New(start time.Time) *Fake {
	return &Fake{now: start}
}

// Now returns the fake clock's current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// PendingCount returns the number of currently-registered, not-yet-fired
// waiters. It exists for test synchronization: a test driving a goroutine
// that blocks on this clock can poll PendingCount (via WaitForPending)
// until that goroutine has actually registered its timer, instead of
// racing Advance/Set against the goroutine's scheduling.
func (f *Fake) PendingCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waiters)
}

// WaitForPending blocks (via short, bounded real-time polling — this is
// test-only synchronization with the Go scheduler, not a simulation of
// protocol timing, which remains entirely driven by Advance/Set) until
// PendingCount is at least n, or timeout elapses. It reports whether n
// was reached.
func (f *Fake) WaitForPending(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if f.PendingCount() >= n {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

// After returns NewTimer(d).C().
func (f *Fake) After(d time.Duration) <-chan time.Time {
	return f.NewTimer(d).C()
}

// NewTimer returns a Timer that fires (sends the deadline time on its
// channel) when the fake clock reaches or passes now+d.
func (f *Fake) NewTimer(d time.Duration) clock.Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{f: f, deadline: f.now.Add(d), ch: make(chan time.Time, 1)}
	f.seq++
	t.seq = f.seq
	f.waiters = append(f.waiters, t)
	return t
}

// AfterFunc returns a Timer that calls fn when the fake clock reaches or
// passes now+d, per Fake's synchronous firing order documented above.
func (f *Fake) AfterFunc(d time.Duration, fn func()) clock.Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{f: f, deadline: f.now.Add(d), fn: fn}
	f.seq++
	t.seq = f.seq
	f.waiters = append(f.waiters, t)
	return t
}

// Sleep blocks until some other goroutine calls Advance or Set past
// now+d. It exists only for Clock interface completeness — production
// and test code in this repository always uses After/NewTimer/AfterFunc,
// never Sleep, precisely so tests can drive time without a real wait.
func (f *Fake) Sleep(d time.Duration) {
	<-f.After(d)
}

// Advance moves the fake clock forward by d and fires every waiter whose
// deadline is now due.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	target := f.now.Add(d)
	f.mu.Unlock()
	f.advanceTo(target)
}

// Set moves the fake clock to t and fires every waiter whose deadline is
// now due. t must not be before the current time.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	if t.Before(f.now) {
		f.mu.Unlock()
		panic("clocktest: Set must not move time backward")
	}
	f.mu.Unlock()
	f.advanceTo(t)
}

// advanceTo moves the clock forward to target one deadline at a time.
//
// Jumping straight to target and firing everything that had come due
// collapsed a multi-period jump into a SINGLE tick for a
// self-rescheduling AfterFunc chain — the shape subscription.Manager's
// poll scheduling uses — because the callback rearms relative to the
// clock's new time. A test advancing 10 s over a 1 s sampling rate
// therefore exercised one poll and looked as though it had exercised ten,
// and the callback saw Now() at the end of the jump rather than at its
// own due time, so every timestamp it recorded was wrong by up to the
// whole jump.
//
// Stepping means a rearmed timer whose new deadline is still inside the
// jump fires too, in the right order and each seeing its own due time.
func (f *Fake) advanceTo(target time.Time) {
	for {
		f.mu.Lock()
		next, ok := f.nextDeadlineLocked()
		if !ok || next.After(target) {
			f.now = target
			f.mu.Unlock()
			return
		}
		f.now = next
		due := f.dueLocked()
		f.mu.Unlock()
		fire(due)
	}
}

// nextDeadlineLocked returns the earliest pending deadline. f.mu must be
// held.
func (f *Fake) nextDeadlineLocked() (time.Time, bool) {
	var earliest time.Time
	found := false
	for _, t := range f.waiters {
		if !found || t.deadline.Before(earliest) {
			earliest, found = t.deadline, true
		}
	}
	return earliest, found
}

// dueLocked removes and returns every active waiter whose deadline is
// now due, sorted by deadline then registration order. f.mu must be held.
func (f *Fake) dueLocked() []*fakeTimer {
	var due, remaining []*fakeTimer
	for _, t := range f.waiters {
		if !t.deadline.After(f.now) {
			due = append(due, t)
		} else {
			remaining = append(remaining, t)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].deadline.Equal(due[j].deadline) {
			return due[i].seq < due[j].seq
		}
		return due[i].deadline.Before(due[j].deadline)
	})
	f.waiters = remaining
	return due
}

func fire(due []*fakeTimer) {
	for _, t := range due {
		switch {
		case t.fn != nil:
			t.fn()
		case t.ch != nil:
			select {
			case t.ch <- t.deadline:
			default:
			}
		}
	}
}

type fakeTimer struct {
	f        *Fake
	deadline time.Time
	ch       chan time.Time // set for NewTimer
	fn       func()         // set for AfterFunc
	seq      uint64
}

// Stop cancels t, reporting whether it was still pending (had not
// already fired or been stopped).
func (t *fakeTimer) Stop() bool {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	for i, w := range t.f.waiters {
		if w == t {
			t.f.waiters = append(t.f.waiters[:i:i], t.f.waiters[i+1:]...)
			return true
		}
	}
	return false
}

// Reset reschedules t to fire at now+d, reporting whether it was still
// pending beforehand.
func (t *fakeTimer) Reset(d time.Duration) bool {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	wasActive := false
	for i, w := range t.f.waiters {
		if w == t {
			t.f.waiters = append(t.f.waiters[:i:i], t.f.waiters[i+1:]...)
			wasActive = true
			break
		}
	}
	t.deadline = t.f.now.Add(d)
	t.f.seq++
	t.seq = t.f.seq
	t.f.waiters = append(t.f.waiters, t)
	return wasActive
}

// C returns t's channel (meaningful only for a Timer created via
// NewTimer; see Timer's doc comment).
func (t *fakeTimer) C() <-chan time.Time { return t.ch }
