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
