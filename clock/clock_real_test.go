package clock_test

import (
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/clock"
)

// clock.Real is the implementation every deployment actually runs, and
// three of its five methods had no test — the fake was covered instead.
// These are cheap, and they are what says the interface the whole
// subscription engine is written against behaves the same in production
// as it does under clocktest.Fake.

func TestReal_NowAdvances(t *testing.T) {
	c := clock.Real{}
	first := c.Now()
	if first.IsZero() {
		t.Fatal("Now returned the zero time")
	}
	time.Sleep(2 * time.Millisecond)
	if !c.Now().After(first) {
		t.Error("Now did not advance across a real sleep")
	}
}

func TestReal_AfterFires(t *testing.T) {
	c := clock.Real{}
	start := time.Now()
	select {
	case fired := <-c.After(5 * time.Millisecond):
		if fired.Before(start) {
			t.Errorf("After delivered %v, which is before the call at %v", fired, start)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("After never fired")
	}
}

func TestReal_SleepBlocksForAtLeastTheDuration(t *testing.T) {
	c := clock.Real{}
	start := time.Now()
	c.Sleep(5 * time.Millisecond)
	if elapsed := time.Since(start); elapsed < 5*time.Millisecond {
		t.Errorf("Sleep returned after %v, want at least 5ms", elapsed)
	}
}

func TestReal_TimerStopPreventsTheCallback(t *testing.T) {
	c := clock.Real{}
	fired := make(chan struct{}, 1)
	timer := c.AfterFunc(time.Hour, func() { fired <- struct{}{} })
	if !timer.Stop() {
		t.Fatal("Stop reported the callback had already run, an hour early")
	}
	select {
	case <-fired:
		t.Error("the callback ran after a successful Stop")
	case <-time.After(20 * time.Millisecond):
	}
	// Stopping twice must be safe: the subscription engine stops timers
	// from both the cancel path and the shutdown path.
	if timer.Stop() {
		t.Error("the second Stop claimed to have prevented the callback again")
	}
}

func TestReal_NewTimerDelivers(t *testing.T) {
	c := clock.Real{}
	timer := c.NewTimer(5 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C():
	case <-time.After(2 * time.Second):
		t.Fatal("NewTimer never delivered")
	}
}
