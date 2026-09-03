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

// --- the deadband reference point is the last REPORTED value ---

// TestApplyUpdate_DeadbandReferenceIsLastReportedValue pins the fix for
// it.last having been advanced even when the change was suppressed.
//
// That turned the deadband into a rate-of-change filter: each reading was
// compared against the immediately preceding one rather than against the
// last value the client was actually told about, so a value drifting by
// just under the band on every poll could travel arbitrarily far without
// ever producing a notification.
func TestApplyUpdate_DeadbandReferenceIsLastReportedValue(t *testing.T) {
	it := &itemState{
		deadband: 10, // 10%
		haveLast: true,
		last:     backend.ItemSample{Value: xmlda.NewFloat64(100), Quality: xmlda.NewGoodQuality()},
	}

	// Each step is under 10% of the previous reading, so no single step
	// crosses the band on its own — but the value walks away from 100.
	steps := []float64{105, 110, 115, 120}
	reported := 0
	for _, v := range steps {
		if applyUpdate(it, backend.ItemSample{Value: xmlda.NewFloat64(v), Quality: xmlda.NewGoodQuality()},
			xmlda.ErrorCode{}, "", 100, nil) {
			reported++
		}
	}
	if reported == 0 {
		t.Fatalf("a drift from 100 to %v with a 10%% deadband produced no notification at all — "+
			"the reference point is walking with the readings", steps[len(steps)-1])
	}

	// And the reference point that did get adopted is a value actually
	// reported, not the latest reading.
	last, _ := it.last.Value.Float64()
	if last != 110 {
		t.Errorf("last reported reference is %v, want 110 (the first reading that crossed the band from 100)", last)
	}
}

// TestApplyUpdate_DeadbandStillSuppressesSmallChanges confirms the fix
// did not simply disable the deadband.
func TestApplyUpdate_DeadbandStillSuppressesSmallChanges(t *testing.T) {
	it := &itemState{
		deadband: 10,
		haveLast: true,
		last:     backend.ItemSample{Value: xmlda.NewFloat64(100), Quality: xmlda.NewGoodQuality()},
	}
	for _, v := range []float64{101, 102, 103} {
		if applyUpdate(it, backend.ItemSample{Value: xmlda.NewFloat64(v), Quality: xmlda.NewGoodQuality()},
			xmlda.ErrorCode{}, "", 100, nil) {
			t.Fatalf("%v is within 10%% of the reported 100 but was notified", v)
		}
	}
}

// TestApplyUpdate_NoDeadbandUnchanged confirms the change is behaviorally
// neutral when no deadband is configured — the common case.
func TestApplyUpdate_NoDeadbandUnchanged(t *testing.T) {
	it := &itemState{
		haveLast: true,
		last:     backend.ItemSample{Value: xmlda.NewFloat64(1), Quality: xmlda.NewGoodQuality()},
	}
	if applyUpdate(it, backend.ItemSample{Value: xmlda.NewFloat64(1), Quality: xmlda.NewGoodQuality()},
		xmlda.ErrorCode{}, "", 100, nil) {
		t.Error("an identical value was reported as a change")
	}
	if !applyUpdate(it, backend.ItemSample{Value: xmlda.NewFloat64(2), Quality: xmlda.NewGoodQuality()},
		xmlda.ErrorCode{}, "", 100, nil) {
		t.Error("a different value was not reported as a change")
	}
	v, _ := it.last.Value.Float64()
	if v != 2 {
		t.Errorf("last is %v, want the newly reported 2", v)
	}
}

// --- the poll chain does not drift ---

// TestSchedulePoll_DoesNotDriftWithSlowBackend pins the fix for the poll
// chain having rescheduled "one full interval from now" *after* the poll
// completed. The effective period was then rate + backend duration +
// semaphore wait, so every item was sampled slower than the
// RevisedSamplingRate the client had been promised — and a backend slower
// than the interval drifted without bound.
//
// The clock is driven with Set to ABSOLUTE points on the ideal sampling
// grid, not with relative Advance steps. That distinction is the whole
// test: the slow backend moves the fake clock forward itself, so relative
// steps ride along with the drift and count five ticks either way. On a
// fixed grid the drifting scheduler visibly misses every second one.
func TestSchedulePoll_DoesNotDriftWithSlowBackend(t *testing.T) {
	const rate = 100 * time.Millisecond
	clk := clocktest.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	slow := &slowReader{clk: clk, cost: 60 * time.Millisecond}

	m := NewManager(
		backend.Backend{Status: fixStatus{}, Reader: slow}, clk, nil, nil,
		Config{ReapInterval: time.Hour, DefaultSamplingRate: rate})
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })

	if _, err := m.Create(context.Background(), CreateRequest{
		Items:                []CreateItemRequest{{Ref: backend.ItemRef{ItemName: "A"}}},
		SubscriptionPingRate: time.Hour,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Create's own validity read already moved the clock; the poll chain
	// was armed relative to where it left off.
	base := clk.Now()
	slow.reset()

	const ticks = 5
	for i := 1; i <= ticks; i++ {
		clk.Set(base.Add(time.Duration(i) * rate))
	}
	if got := slow.count(); got != ticks {
		t.Fatalf("got %d polls across %d sampling intervals, want %d — "+
			"the chain is drifting by the backend's own duration", got, ticks, ticks)
	}
}

// TestApplyUpdate_SuccessCodeKeepsDeliveringValues pins the distinction
// between a critical E_ code and a non-critical S_ one for the poll
// engine. §2.6 is explicit that "in case of a critical error the returned
// value may not be useful. For non-critical exceptions the returned value
// IS useful" — server/itemvalue.go's hasUsableValue and
// subscription/create.go's initial read both already applied that rule;
// applyUpdate did not. It treated every non-zero ResultID as "no sample",
// and since a persistent code then compared equal to itself on the next
// tick, the item reported its condition exactly once and never delivered
// another value for the whole lifetime of the subscription — silent
// process-data loss for something as ordinary as a clamped analog value.
func TestApplyUpdate_SuccessCodeKeepsDeliveringValues(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := &clampReader{fakeReader: newFakeReader()}
	ref := backend.ItemRef{ItemName: "Clamped"}
	r.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(r, fake, Config{ReapInterval: time.Hour})
	defer shutdownManager(t, m)

	res, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH", RequestedSamplingRate: time.Second}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Handle == "" {
		t.Fatal("Create returned no handle for an item reporting S_CLAMP")
	}

	// Drain whatever Create seeded, so the assertions below are about the
	// poll engine rather than about the initial read.
	if _, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{res.Handle}}); err != nil {
		t.Fatalf("PolledRefresh (drain): %v", err)
	}

	for i := 2; i <= 4; i++ {
		r.Set(ref, xmlda.NewInt32(int32(i)))
		fake.Advance(time.Second)

		got, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{res.Handle}})
		if err != nil {
			t.Fatalf("PolledRefresh %d: %v", i, err)
		}
		if len(got.Subscriptions) != 1 || len(got.Subscriptions[0].Items) != 1 {
			t.Fatalf("poll %d: got %d subscriptions, want 1 with exactly 1 changed item "+
				"(an S_ code must not suppress the change)", i, len(got.Subscriptions))
		}
		item := got.Subscriptions[0].Items[0]
		if !item.HaveSample {
			t.Fatalf("poll %d: HaveSample is false for an S_CLAMP item; the value is useful (§2.6)", i)
		}
		if item.ResultID != xmlda.SuccessClamp {
			t.Fatalf("poll %d: ResultID = %v, want S_CLAMP carried alongside the value", i, item.ResultID)
		}
		if n, err := item.Sample.Value.Int32(); err != nil || n != int32(i) {
			t.Fatalf("poll %d: value = %v (err=%v), want %d", i, item.Sample.Value, err, i)
		}
	}

	// ReturnAllItems is the client's explicit "give me the current state"
	// and must not degrade to the bare condition either.
	all, err := m.PolledRefresh(context.Background(), RefreshRequest{
		Handles: []Handle{res.Handle}, ReturnAllItems: true,
	})
	if err != nil {
		t.Fatalf("PolledRefresh(ReturnAllItems): %v", err)
	}
	if len(all.Subscriptions) != 1 || len(all.Subscriptions[0].Items) != 1 {
		t.Fatalf("ReturnAllItems: got %d subscriptions, want 1 with 1 item", len(all.Subscriptions))
	}
	if !all.Subscriptions[0].Items[0].HaveSample {
		t.Fatal("ReturnAllItems: HaveSample is false for an S_CLAMP item")
	}
}

// TestApplyUpdate_ErrorCodeStillSuppressesSample is the other half of the
// rule above: a critical E_ code must keep suppressing the sample, and a
// persistent one must still be reported only once rather than on every
// tick.
func TestApplyUpdate_ErrorCodeStillSuppressesSample(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Gone"}
	r.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(r, fake, Config{ReapInterval: time.Hour})
	defer shutdownManager(t, m)

	res, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH", RequestedSamplingRate: time.Second}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{res.Handle}}); err != nil {
		t.Fatalf("PolledRefresh (drain): %v", err)
	}

	r.SetNotFound(ref)
	fake.Advance(time.Second)
	got, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{res.Handle}})
	if err != nil {
		t.Fatalf("PolledRefresh: %v", err)
	}
	if len(got.Subscriptions) != 1 || len(got.Subscriptions[0].Items) != 1 {
		t.Fatalf("got %d subscriptions, want 1 with 1 item reporting the condition", len(got.Subscriptions))
	}
	item := got.Subscriptions[0].Items[0]
	if item.HaveSample {
		t.Error("HaveSample is true for an E_ condition; a critical error carries no usable value")
	}
	if !item.ResultID.IsError() {
		t.Errorf("ResultID = %v, want a critical E_ code", item.ResultID)
	}

	// The same condition on the next tick is not a new change.
	fake.Advance(time.Second)
	again, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{res.Handle}})
	if err != nil {
		t.Fatalf("PolledRefresh (repeat): %v", err)
	}
	if len(again.Subscriptions) != 0 {
		t.Errorf("a persistent E_ condition was reported twice: %+v", again.Subscriptions)
	}
}

// TestApplyUpdate_AfterReleaseDoesNotLeakBudget pins the server-wide
// buffered-sample budget against the window between a subscription being
// terminated and an update that was already in flight landing. terminate
// returns the item's buffered slots via releaseBuffers; an applyUpdate
// whose s.ctx check had already passed then acquired fresh slots and
// wrote them into a buffer nobody would ever drain. Each such race leaked
// its slots permanently, and ordinary client churn eventually exhausted
// the budget — after which every buffering subscription server-wide
// degraded to "latest value only" with DataBufferOverflow set, with no
// way back short of a restart.
func TestApplyUpdate_AfterReleaseDoesNotLeakBudget(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Buffered"}
	r.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(r, fake, Config{ReapInterval: time.Hour, MaxTotalBufferedSamples: 100})
	defer shutdownManager(t, m)

	res, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{{
			Ref: ref, ClientItemHandle: "CIH", RequestedSamplingRate: time.Second, EnableBuffering: true,
		}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for i := 2; i <= 4; i++ {
		r.Set(ref, xmlda.NewInt32(int32(i)))
		fake.Advance(time.Second)
	}
	if m.budget.count() == 0 {
		t.Fatal("setup: nothing was buffered, so the test would prove nothing")
	}

	m.mu.RLock()
	s := m.subs[res.Handle]
	m.mu.RUnlock()
	if s == nil {
		t.Fatal("setup: subscription not found")
	}
	s.mu.Lock()
	it := s.items[0]
	s.mu.Unlock()

	if !m.Cancel(res.Handle) {
		t.Fatal("Cancel reported the subscription was unknown")
	}
	if n := m.budget.count(); n != 0 {
		t.Fatalf("after Cancel the budget still holds %d slots, want 0", n)
	}

	// The update that was already in flight when Cancel ran lands now.
	applyUpdate(it, backend.ItemSample{Value: xmlda.NewInt32(99), Timestamp: fake.Now()},
		xmlda.ErrorCode{}, "", 100, m.budget)

	if n := m.budget.count(); n != 0 {
		t.Fatalf("an update landing after the subscription was terminated leaked %d budget slot(s); "+
			"they can never be released because the subscription is gone", n)
	}
}

// TestSampleChanged_DeadbandAppliesToArrays pins §3.5.1's array rule:
// "The deadband will also apply to array types. The entire array is
// returned if any array element exceeds the deadband threshold." The
// comparison used to fall through to plain equality for arrays, so a
// client that had asked the server to suppress small changes got every
// single element change instead — the exact flood a deadband exists to
// prevent.
func TestSampleChanged_DeadbandAppliesToArrays(t *testing.T) {
	sample := func(v ...float64) backend.ItemSample {
		return backend.ItemSample{Value: xmlda.NewArrayValue(xmlda.NewFloat64Array(v)), Quality: xmlda.NewGoodQuality()}
	}
	const deadband = 50.0

	if sampleChanged(sample(100, 200), sample(100.0001, 200), deadband) {
		t.Error("a 0.0001% element change was reported despite a 50% deadband")
	}
	if !sampleChanged(sample(100, 200), sample(100, 400), deadband) {
		t.Error("a 100% change in one element was suppressed by a 50% deadband")
	}
	// A changed shape is a change regardless of the deadband: there is no
	// element-wise comparison to make.
	if !sampleChanged(sample(100), sample(100, 100), deadband) {
		t.Error("an array that grew was reported as unchanged")
	}
	// Without a deadband, every element change is still reported.
	if !sampleChanged(sample(100), sample(100.0001), 0) {
		t.Error("a tiny element change was suppressed although no deadband was set")
	}
	// Non-numeric arrays have no percentage to compare and keep the plain
	// comparison.
	strs := func(v ...string) backend.ItemSample {
		return backend.ItemSample{Value: xmlda.NewArrayValue(xmlda.NewStringArray(v)), Quality: xmlda.NewGoodQuality()}
	}
	if !sampleChanged(strs("a"), strs("b"), deadband) {
		t.Error("a changed string array was suppressed by a deadband that cannot apply to it")
	}
	if sampleChanged(strs("a"), strs("a"), deadband) {
		t.Error("an unchanged string array was reported as changed")
	}
}
