package clocktest

import (
	"testing"
	"time"
)

var epoch = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

func TestFake_NewTimer_FiresExactlyAtDeadline(t *testing.T) {
	f := New(epoch)
	timer := f.NewTimer(10 * time.Second)

	f.Advance(5 * time.Second)
	select {
	case <-timer.C():
		t.Fatalf("timer fired too early")
	default:
	}

	f.Advance(5 * time.Second) // now at 10s: exactly due
	select {
	case got := <-timer.C():
		want := epoch.Add(10 * time.Second)
		if !got.Equal(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	default:
		t.Fatalf("expected timer to have fired")
	}
}

func TestFake_AfterFunc_RunsSynchronouslyDuringAdvance(t *testing.T) {
	f := New(epoch)
	fired := false
	f.AfterFunc(time.Second, func() { fired = true })
	if fired {
		t.Fatalf("callback fired before Advance")
	}
	f.Advance(time.Second)
	if !fired {
		t.Fatalf("expected callback to have fired synchronously during Advance")
	}
}

func TestFake_MultipleTimers_FireInDeadlineOrder(t *testing.T) {
	f := New(epoch)
	var order []int
	f.AfterFunc(3*time.Second, func() { order = append(order, 3) })
	f.AfterFunc(1*time.Second, func() { order = append(order, 1) })
	f.AfterFunc(2*time.Second, func() { order = append(order, 2) })

	f.Advance(3 * time.Second)
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("got order %v, want [1 2 3]", order)
	}
}

// TestFake_SameDeadline_FiresInRegistrationOrder targets dueLocked's tie
// break (due[i].seq < due[j].seq) directly: the package's own doc comment
// promises multiple timers scheduled for the same fake instant fire in a
// deterministic order, but no existing test ever registered two timers
// with an identical deadline — TestFake_MultipleTimers_FireInDeadlineOrder
// above only covers distinct deadlines, leaving this exact branch at 0%
// coverage despite every subscription lifecycle test's determinism
// resting on it.
func TestFake_SameDeadline_FiresInRegistrationOrder(t *testing.T) {
	f := New(epoch)
	var order []int
	f.AfterFunc(1*time.Second, func() { order = append(order, 1) })
	f.AfterFunc(1*time.Second, func() { order = append(order, 2) })
	f.AfterFunc(1*time.Second, func() { order = append(order, 3) })

	f.Advance(1 * time.Second)
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("got order %v, want [1 2 3] (registration order)", order)
	}
}

// TestFake_SelfReschedulingChain drives a toy self-rescheduling
// AfterFunc chain — the exact pattern subscription.Manager uses for
// poll-mode subscriptions (docs/architecture/subscription-model.md) —
// purely via Advance, with zero real sleeps, confirming the fake clock
// supports a callback calling back into AfterFunc without deadlocking.
func TestFake_SelfReschedulingChain(t *testing.T) {
	f := New(epoch)
	ticks := 0
	var reschedule func()
	reschedule = func() {
		f.AfterFunc(time.Second, func() {
			ticks++
			if ticks < 5 {
				reschedule()
			}
		})
	}
	reschedule()

	for range 5 {
		f.Advance(time.Second)
	}
	if ticks != 5 {
		t.Fatalf("got %d ticks, want 5", ticks)
	}
}

func TestFake_Stop_PreventsFiring(t *testing.T) {
	f := New(epoch)
	fired := false
	timer := f.AfterFunc(time.Second, func() { fired = true })
	if !timer.Stop() {
		t.Fatalf("expected Stop() to report the timer was pending")
	}
	f.Advance(time.Hour)
	if fired {
		t.Fatalf("expected a stopped timer to never fire")
	}
	if timer.Stop() {
		t.Fatalf("expected a second Stop() to report false (already stopped)")
	}
}

func TestFake_Reset_Reschedules(t *testing.T) {
	f := New(epoch)
	var fireTime time.Time
	timer := f.AfterFunc(time.Second, func() { fireTime = f.Now() })

	if !timer.Reset(10 * time.Second) {
		t.Fatalf("expected Reset() to report the timer was pending")
	}
	f.Advance(5 * time.Second)
	if !fireTime.IsZero() {
		t.Fatalf("expected the rescheduled timer to not have fired yet, fired at %v", fireTime)
	}
	f.Advance(5 * time.Second)
	want := epoch.Add(10 * time.Second)
	if !fireTime.Equal(want) {
		t.Fatalf("got %v, want %v", fireTime, want)
	}
}

func TestFake_Set_MovesTimeAndFiresDueTimers(t *testing.T) {
	f := New(epoch)
	fired := false
	f.AfterFunc(time.Minute, func() { fired = true })
	f.Set(epoch.Add(2 * time.Minute))
	if !fired {
		t.Fatalf("expected the timer to have fired after Set moved time past its deadline")
	}
	if got := f.Now(); !got.Equal(epoch.Add(2 * time.Minute)) {
		t.Fatalf("got %v, want %v", got, epoch.Add(2*time.Minute))
	}
}

func TestFake_Set_PanicsOnBackwardTime(t *testing.T) {
	f := New(epoch)
	defer func() {
		if recover() == nil {
			t.Fatalf("expected Set to panic when moving time backward")
		}
	}()
	f.Set(epoch.Add(-time.Second))
}

func TestFake_After(t *testing.T) {
	f := New(epoch)
	ch := f.After(time.Second)
	select {
	case <-ch:
		t.Fatalf("channel fired before Advance")
	default:
	}
	f.Advance(time.Second)
	select {
	case <-ch:
	default:
		t.Fatalf("expected the channel to have fired")
	}
}

func TestFake_Sleep_UnblocksOnAdvanceFromAnotherGoroutine(t *testing.T) {
	f := New(epoch)
	done := make(chan struct{})
	go func() {
		f.Sleep(time.Second)
		close(done)
	}()
	// Give the goroutine a moment to register its waiter — this uses a
	// real (short) sleep only because we're synchronizing with a
	// separate goroutine's registration, not simulating protocol timing.
	time.Sleep(10 * time.Millisecond)
	f.Advance(time.Second)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("Sleep did not unblock after Advance")
	}
}

// TestFake_MultiPeriodJumpFiresEveryTick pins the fidelity of a jump over
// several periods. Moving the clock straight to the target and firing
// whatever had come due collapsed such a jump into a SINGLE tick for a
// self-rescheduling AfterFunc chain — the shape subscription.Manager's
// poll scheduling uses — because the callback rearms relative to the
// clock's new time. A test advancing 10 s over a 1 s rate then exercised
// one poll while appearing to exercise ten.
func TestFake_MultiPeriodJumpFiresEveryTick(t *testing.T) {
	f := New(time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC))
	start := f.Now()

	var seen []time.Duration
	var rearm func()
	rearm = func() {
		seen = append(seen, f.Now().Sub(start))
		f.AfterFunc(time.Second, rearm)
	}
	f.AfterFunc(time.Second, rearm)

	f.Advance(10 * time.Second)

	if len(seen) != 10 {
		t.Fatalf("a 10 s jump over a 1 s chain fired %d times, want 10: %v", len(seen), seen)
	}
	// Each callback must see its OWN due time, not the end of the jump.
	for i, d := range seen {
		if want := time.Duration(i+1) * time.Second; d != want {
			t.Errorf("tick %d saw Now()-start = %v, want %v", i+1, d, want)
		}
	}
	if got := f.Now().Sub(start); got != 10*time.Second {
		t.Errorf("clock ended at +%v, want +10s", got)
	}
}

// TestFake_JumpPastANonRearmingTimerStillLandsOnTarget guards the loop's
// exit: a one-shot timer inside the jump must not leave the clock short
// of where it was told to go.
func TestFake_JumpPastANonRearmingTimerStillLandsOnTarget(t *testing.T) {
	f := New(time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC))
	start := f.Now()
	fired := 0
	f.AfterFunc(2*time.Second, func() { fired++ })

	f.Advance(10 * time.Second)

	if fired != 1 {
		t.Errorf("one-shot timer fired %d times, want 1", fired)
	}
	if got := f.Now().Sub(start); got != 10*time.Second {
		t.Errorf("clock ended at +%v, want +10s", got)
	}
}
