package clock

import (
	"testing"
	"time"
)

func TestReal_Now(t *testing.T) {
	var c Real
	before := time.Now()
	got := c.Now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Fatalf("Now() = %v, want between %v and %v", got, before, after)
	}
}

func TestReal_AfterFunc(t *testing.T) {
	var c Real
	done := make(chan struct{})
	c.AfterFunc(time.Millisecond, func() { close(done) })
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("AfterFunc callback did not fire within 1s")
	}
}

func TestReal_NewTimer(t *testing.T) {
	var c Real
	timer := c.NewTimer(time.Millisecond)
	select {
	case <-timer.C():
	case <-time.After(time.Second):
		t.Fatalf("timer did not fire within 1s")
	}
}

func TestReal_TimerStopAndReset(t *testing.T) {
	var c Real
	timer := c.NewTimer(time.Hour)
	if !timer.Stop() {
		t.Fatalf("expected Stop() to report the timer was still pending")
	}
	if timer.Reset(time.Millisecond) {
		t.Fatalf("expected Reset() to report the timer was not pending (already stopped)")
	}
	select {
	case <-timer.C():
	case <-time.After(time.Second):
		t.Fatalf("timer did not fire within 1s after Reset")
	}
}
