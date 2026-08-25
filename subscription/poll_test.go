package subscription

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock/clocktest"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// TestPollOnce_HonorsPerItemSamplingRate reproduces the gap where a
// slower item in a subscription got read/evaluated at the fastest item's
// rate, since both share one timer chain (ADR-008: one shared timer per
// subscription, not one per item). fastRef requests 1s, slowRef requests
// 10s; the shared timer necessarily ticks at the faster rate (1s), but
// slowRef must only actually be read from the backend roughly once per
// 10s, not on every 1s tick.
func TestPollOnce_HonorsPerItemSamplingRate(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	fastRef := backend.ItemRef{ItemName: "Fast"}
	slowRef := backend.ItemRef{ItemName: "Slow"}
	r.Set(fastRef, xmlda.NewInt32(1))
	r.Set(slowRef, xmlda.NewInt32(1))
	m := newTestManager(r, fake, Config{ReapInterval: time.Hour})
	defer shutdownManager(t, m)

	if _, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{
			{Ref: fastRef, ClientItemHandle: "CIH-fast", RequestedSamplingRate: time.Second},
			{Ref: slowRef, ClientItemHandle: "CIH-slow", RequestedSamplingRate: 10 * time.Second},
		},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	for range 10 {
		fake.Advance(time.Second) // shared timer ticks at the fastest item's rate (1s)
	}

	fastCount := r.RefReadCount(fastRef)
	slowCount := r.RefReadCount(slowRef)
	if fastCount < 9 {
		t.Fatalf("fast item: got %d reads over 10s at a 1s rate, want close to 10", fastCount)
	}
	if slowCount > 2 {
		t.Fatalf("slow item: got %d reads over 10s at a 10s rate, want close to 1 (was reading at the fast item's rate before this was fixed)", slowCount)
	}
	if slowCount < 1 {
		t.Fatalf("slow item: got 0 reads over 10s at a 10s rate, want at least 1")
	}
}

// TestSampleChanged_NaNTransition reproduces the gap where a numeric value
// transitioning into or out of NaN under a percentage deadband was
// silently reported as "unchanged": (nf-pf)/pf*100 is NaN whenever either
// operand is NaN, and "NaN >= deadbandPct" is always false (IEEE-754), so
// sampleChanged never reported the transition — permanently hiding it from
// a deadbanded subscriber.
func TestSampleChanged_NaNTransition(t *testing.T) {
	good := xmlda.NewGoodQuality()
	num := backend.ItemSample{Value: xmlda.NewFloat64(5), Quality: good}
	nan1 := backend.ItemSample{Value: xmlda.NewFloat64(math.NaN()), Quality: good}
	nan2 := backend.ItemSample{Value: xmlda.NewFloat64(math.NaN()), Quality: good}

	if !sampleChanged(num, nan1, 10) {
		t.Fatalf("expected a transition from a numeric value into NaN to be reported as changed")
	}
	if !sampleChanged(nan1, num, 10) {
		t.Fatalf("expected a transition from NaN back to a numeric value to be reported as changed")
	}
	if sampleChanged(nan1, nan2, 10) {
		t.Fatalf("expected two consecutive NaN readings to be reported as unchanged")
	}
}

// TestPollOnce_SingleRate_Unaffected is the regression-safety companion:
// when every item in a subscription shares the same rate (the common
// case), per-item due-gating must not change behavior at all — every
// item should still be read on every tick.
func TestPollOnce_SingleRate_Unaffected(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref1 := backend.ItemRef{ItemName: "Item1"}
	ref2 := backend.ItemRef{ItemName: "Item2"}
	r.Set(ref1, xmlda.NewInt32(1))
	r.Set(ref2, xmlda.NewInt32(1))
	m := newTestManager(r, fake, Config{ReapInterval: time.Hour})
	defer shutdownManager(t, m)

	if _, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{
			{Ref: ref1, ClientItemHandle: "CIH1", RequestedSamplingRate: time.Second},
			{Ref: ref2, ClientItemHandle: "CIH2", RequestedSamplingRate: time.Second},
		},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	for range 5 {
		fake.Advance(time.Second)
	}

	c1, c2 := r.RefReadCount(ref1), r.RefReadCount(ref2)
	if c1 != c2 {
		t.Fatalf("expected both same-rate items to be read the same number of times, got %d vs %d", c1, c2)
	}
	if c1 < 4 {
		t.Fatalf("got %d reads over 5s at a 1s rate, want close to 5", c1)
	}
}
