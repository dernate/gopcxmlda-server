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

// dropLastItemReader wraps a fakeReader, dropping the last item's Result
// from the first Read call it sees that requests 2 or more items after
// skip such calls have passed through untouched — modeling a
// non-conforming backend that returns fewer results than items requested
// on exactly one poll tick.
type dropLastItemReader struct {
	*fakeReader
	skip int
}

func (d *dropLastItemReader) Read(ctx context.Context, items []backend.ReadRequestItem) ([]backend.Result[backend.ItemSample], error) {
	results, err := d.fakeReader.Read(ctx, items)
	if err != nil || len(items) < 2 {
		return results, err
	}
	if d.skip > 0 {
		d.skip--
		return results, nil
	}
	return results[:len(results)-1], nil
}

// TestPollOnce_MissingResultForItem_MarksFailAndAdvancesLastPolledAt
// reproduces a bug in the per-item due-gating loop: it used to exit the
// loop early ("if i >= len(results) { break }") the instant the backend
// returned fewer results than items requested, leaving every item from
// that point on — including their lastPolledAt — untouched. For an item
// slower than the subscription's shared tick rate, that meant its
// lastPolledAt stayed at its old value, making it "due" again on every
// subsequent fast tick: a silent busy-poll against a backend that was
// already misbehaving. The fix marks the missing tail E_FAIL and still
// advances lastPolledAt, so the slow item resumes its own cadence.
func TestPollOnce_MissingResultForItem_MarksFailAndAdvancesLastPolledAt(t *testing.T) {
	fake := clocktest.New(testEpoch)
	inner := newFakeReader()
	fastRef := backend.ItemRef{ItemName: "Fast"}
	slowRef := backend.ItemRef{ItemName: "Slow"}
	inner.Set(fastRef, xmlda.NewInt32(1))
	inner.Set(slowRef, xmlda.NewInt32(1))
	// skip=1: let Create's own validating Read (which requests both items
	// together) through untouched, so both items are valid and the
	// subscription is actually created.
	r := &dropLastItemReader{fakeReader: inner, skip: 1}
	m := newTestManager(r, fake, Config{ReapInterval: time.Hour})
	defer shutdownManager(t, m)

	handle, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{
			{Ref: fastRef, ClientItemHandle: "CIH-fast", RequestedSamplingRate: time.Second},
			{Ref: slowRef, ClientItemHandle: "CIH-slow", RequestedSamplingRate: 10 * time.Second},
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// itemState.lastPolledAt starts zero (Create does not set it), so both
	// items are due on their very first poll opportunity regardless of
	// their own rate: at t=1s, due=[fast,slow] — batch size 2, the first
	// one after Create's own (skip=1 let that one through), so this is the
	// tick dropLastItemReader truncates, dropping Slow's result. That's
	// Slow's second read overall (Create's + this one).
	fake.Advance(time.Second)
	if got := r.RefReadCount(slowRef); got != 2 {
		t.Fatalf("got %d reads of the slow item by t=1s, want exactly 2 (Create's + the truncated tick's)", got)
	}

	res, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{handle.Handle}, ReturnAllItems: true})
	if err != nil {
		t.Fatalf("PolledRefresh: %v", err)
	}
	var slowResult *RefreshItemResult
	for i, it := range res.Subscriptions[0].Items {
		if it.Ref == slowRef {
			slowResult = &res.Subscriptions[0].Items[i]
		}
	}
	if slowResult == nil {
		t.Fatalf("expected the slow item to be present in the snapshot, got %+v", res.Subscriptions[0].Items)
	}
	if slowResult.ResultID != xmlda.ErrFail || slowResult.HaveSample {
		t.Fatalf("got %+v, want (ResultID=E_FAIL, HaveSample=false) for the item the backend didn't return a Result for", slowResult)
	}

	// The critical regression check: lastPolledAt must have been advanced
	// to t=1s despite the missing result, so the slow item is NOT due
	// again until its own 10s interval elapses from there (t=11s), not on
	// every subsequent 1s fast tick. Before the fix, the due-gating loop
	// exited early on the missing result and never touched Slow's
	// lastPolledAt, leaving it at its zero value — so Slow stayed "due"
	// (and got requested, and truncated again) on every single tick from
	// here on.
	for range 9 { // t=2s..10s
		fake.Advance(time.Second)
	}
	if got := r.RefReadCount(slowRef); got != 2 {
		t.Fatalf("got %d reads of the slow item by t=10s, want still 2 — it was busy-polled at the fast item's rate instead of its own", got)
	}
	fake.Advance(time.Second) // t=11s: now genuinely due again
	if got := r.RefReadCount(slowRef); got != 3 {
		t.Fatalf("got %d reads of the slow item by t=11s, want 3 (due again at its own 10s interval)", got)
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
