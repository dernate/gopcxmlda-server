package subscription

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock/clocktest"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// Regression tests for the subscription-engine defects found in the
// wire-format review.

// --- H3: the reaper must not kill a subscription being polled ---

// TestReaper_DoesNotReapDuringPolledRefresh pins the fix for the
// abandonment sweep having looked only at lastPolledAt while ignoring the
// busy flag a live PolledRefresh already sets.
//
// The failure was not exotic. lastPolledAt was stamped when the request
// arrived, and PolledRefresh then blocked for the client's requested
// Hold+Wait. Any subscription whose ping rate gave it a grace period
// shorter than that hold — a SubscriptionPingRate of 3s, as in the real
// captured traffic, gives 6s against a hold that may legitimately run to
// MaxPolledRefreshWait — was destroyed mid-call. The call returned early
// with an empty, formally successful response, and the handle was gone.
func TestReaper_DoesNotReapDuringPolledRefresh(t *testing.T) {
	m := NewManager(
		backend.Backend{Status: fixStatus{}, Reader: fixReader{}}, nil, nil, nil,
		Config{
			ReapInterval:        10 * time.Millisecond,
			ReapGraceMultiplier: 1.0,
			DefaultSamplingRate: time.Hour, // never polls the backend
		})
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })

	res, err := m.Create(context.Background(), CreateRequest{
		Items:                []CreateItemRequest{{Ref: backend.ItemRef{ItemName: "A"}, ClientItemHandle: "h"}},
		SubscriptionPingRate: 50 * time.Millisecond, // grace = 50ms
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Hold far longer than the 50ms grace period.
	hold := time.Now().Add(400 * time.Millisecond)
	start := time.Now()
	rr, err := m.PolledRefresh(context.Background(), RefreshRequest{
		Handles: []Handle{res.Handle}, HoldTime: &hold, ReturnAllItems: true,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("PolledRefresh: %v", err)
	}
	if elapsed < 300*time.Millisecond {
		t.Errorf("PolledRefresh returned after %v, well before its %v hold — "+
			"the reaper terminated the subscription mid-call", elapsed, 400*time.Millisecond)
	}
	if len(rr.InvalidHandles) != 0 {
		t.Errorf("handle reported invalid during its own poll: %v", rr.InvalidHandles)
	}
	if len(rr.Subscriptions) != 1 {
		t.Fatalf("got %d subscription results, want 1", len(rr.Subscriptions))
	}

	// And it is still usable afterwards.
	if _, err := m.PolledRefresh(context.Background(), RefreshRequest{
		Handles: []Handle{res.Handle}, ReturnAllItems: true,
	}); err != nil {
		t.Fatalf("subscription unusable after the hold: %v", err)
	}
}

// TestReaper_StillReapsAbandoned is the counterpart: the fix must not
// disable abandonment cleanup, only exempt a subscription that is
// genuinely being polled right now.
func TestReaper_StillReapsAbandoned(t *testing.T) {
	m := NewManager(
		backend.Backend{Status: fixStatus{}, Reader: fixReader{}}, nil, nil, nil,
		Config{
			ReapInterval:        10 * time.Millisecond,
			ReapGraceMultiplier: 1.0,
			DefaultSamplingRate: time.Hour,
		})
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })

	res, err := m.Create(context.Background(), CreateRequest{
		Items:                []CreateItemRequest{{Ref: backend.ItemRef{ItemName: "A"}}},
		SubscriptionPingRate: 30 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for m.count() > 0 {
		if time.Now().After(deadline) {
			t.Fatal("an abandoned subscription was never reaped")
		}
		time.Sleep(5 * time.Millisecond)
	}

	_, err = m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{res.Handle}})
	if !errors.Is(err, ErrNoSubscription) {
		t.Fatalf("got %v, want ErrNoSubscription after the reap", err)
	}
}

// TestPolledRefresh_CancelledMidHoldReportsInvalidHandle pins the second
// half of H3: a subscription cancelled while this very call was blocked
// used to be dropped from the result entirely — neither in RItemList nor
// in InvalidServerSubHandles — which a client reads as "no changes"
// rather than "your subscription is gone".
func TestPolledRefresh_CancelledMidHoldReportsInvalidHandle(t *testing.T) {
	m := NewManager(
		backend.Backend{Status: fixStatus{}, Reader: fixReader{}}, nil, nil, nil,
		Config{ReapInterval: time.Hour, DefaultSamplingRate: time.Hour})
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })

	res, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{{Ref: backend.ItemRef{ItemName: "A"}}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		m.Cancel(res.Handle)
	}()

	hold := time.Now().Add(2 * time.Second)
	rr, err := m.PolledRefresh(context.Background(), RefreshRequest{
		Handles: []Handle{res.Handle}, HoldTime: &hold, ReturnAllItems: true,
	})
	if err != nil {
		t.Fatalf("PolledRefresh: %v", err)
	}
	if len(rr.InvalidHandles) != 1 || rr.InvalidHandles[0] != res.Handle {
		t.Fatalf("got InvalidHandles %v, want the cancelled handle reported there", rr.InvalidHandles)
	}
}

// --- M1: the deadband reference point is the last REPORTED value ---

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
			xmlda.ErrorCode{}, 100) {
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
			xmlda.ErrorCode{}, 100) {
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
		xmlda.ErrorCode{}, 100) {
		t.Error("an identical value was reported as a change")
	}
	if !applyUpdate(it, backend.ItemSample{Value: xmlda.NewFloat64(2), Quality: xmlda.NewGoodQuality()},
		xmlda.ErrorCode{}, 100) {
		t.Error("a different value was not reported as a change")
	}
	v, _ := it.last.Value.Float64()
	if v != 2 {
		t.Errorf("last is %v, want the newly reported 2", v)
	}
}

// --- M2: ReturnAllItems includes buffered values ---

// TestPolledRefresh_ReturnAllItemsIncludesBufferedValues pins the fix for
// the ReturnAllItems branch having discarded the buffer instead of
// sending it. §3.6.1 is explicit: "the server will wait the HoldTime but
// then return with all current values (and any buffered values if
// EnableBuffering)". Before the fix a single ReturnAllItems poll silently
// threw away everything EnableBuffering had collected.
func TestPolledRefresh_ReturnAllItemsIncludesBufferedValues(t *testing.T) {
	m := NewManager(
		backend.Backend{Status: fixStatus{}, Reader: fixReader{}}, nil, nil, nil,
		Config{ReapInterval: time.Hour, DefaultSamplingRate: time.Hour})
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })

	res, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{{
			Ref: backend.ItemRef{ItemName: "A"}, ClientItemHandle: "h", EnableBuffering: true,
		}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Record three distinct changes into the item's buffer.
	m.mu.RLock()
	s := m.subs[res.Handle]
	m.mu.RUnlock()
	it := s.items[0]
	for _, v := range []int32{10, 20, 30} {
		applyUpdate(it, backend.ItemSample{Value: xmlda.NewInt32(v), Quality: xmlda.NewGoodQuality()},
			xmlda.ErrorCode{}, 100)
	}

	rr, err := m.PolledRefresh(context.Background(), RefreshRequest{
		Handles: []Handle{res.Handle}, ReturnAllItems: true,
	})
	if err != nil {
		t.Fatalf("PolledRefresh: %v", err)
	}
	if len(rr.Subscriptions) != 1 {
		t.Fatalf("got %d subscription results, want 1", len(rr.Subscriptions))
	}
	items := rr.Subscriptions[0].Items
	// Three buffered entries plus the current value.
	if len(items) != 4 {
		t.Fatalf("got %d entries, want 3 buffered values plus the current one: %+v", len(items), items)
	}
	want := []int32{10, 20, 30, 30}
	for i, w := range want {
		if !items[i].HaveSample {
			t.Fatalf("entry %d carries no sample", i)
		}
		got, err := items[i].Sample.Value.Int32()
		if err != nil || got != w {
			t.Fatalf("entry %d: got %v (err %v), want %d", i, got, err, w)
		}
	}
	// The buffer is drained afterwards, so a second poll does not repeat.
	rr2, err := m.PolledRefresh(context.Background(), RefreshRequest{
		Handles: []Handle{res.Handle}, ReturnAllItems: true,
	})
	if err != nil {
		t.Fatalf("second PolledRefresh: %v", err)
	}
	if n := len(rr2.Subscriptions[0].Items); n != 1 {
		t.Fatalf("second poll returned %d entries, want only the current value", n)
	}
}

// --- M6: the server-wide item budget ---

func TestCreate_TotalItemBudget(t *testing.T) {
	m := NewManager(
		backend.Backend{Status: fixStatus{}, Reader: fixReader{}}, nil, nil, nil,
		Config{ReapInterval: time.Hour, DefaultSamplingRate: time.Hour, MaxTotalSubscribedItems: 3})
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })

	two := []CreateItemRequest{
		{Ref: backend.ItemRef{ItemName: "A"}},
		{Ref: backend.ItemRef{ItemName: "B"}},
	}
	if _, err := m.Create(context.Background(), CreateRequest{Items: two}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if _, err := m.Create(context.Background(), CreateRequest{Items: two}); !errors.Is(err, ErrTooManyItems) {
		t.Fatalf("got %v, want ErrTooManyItems once the budget would be exceeded", err)
	}
	// A subscription that still fits is accepted.
	if _, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{{Ref: backend.ItemRef{ItemName: "C"}}},
	}); err != nil {
		t.Fatalf("a subscription within the remaining budget was rejected: %v", err)
	}
}

// --- shared minimal backend ---

type fixStatus struct{}

func (fixStatus) GetStatus(context.Context, string) (backend.ServerStatus, error) {
	return backend.ServerStatus{
		State:              xmlda.ServerStateRunning,
		SupportedLocaleIDs: []string{"en-US"},
	}, nil
}

type fixReader struct{}

func (fixReader) Read(_ context.Context, items []backend.ReadRequestItem) ([]backend.Result[backend.ItemSample], error) {
	out := make([]backend.Result[backend.ItemSample], len(items))
	for i := range items {
		out[i] = backend.Result[backend.ItemSample]{Value: backend.ItemSample{
			Value:   xmlda.NewInt32(1),
			Quality: xmlda.NewGoodQuality(),
		}}
	}
	return out, nil
}

// --- N10: the poll chain does not drift ---

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

// slowReader consumes a fixed amount of fake-clock time per Read, so a
// test can make the backend "slow" without any real waiting.
type slowReader struct {
	clk  *clocktest.Fake
	cost time.Duration
	mu   sync.Mutex
	n    int
}

func (r *slowReader) Read(_ context.Context, items []backend.ReadRequestItem) ([]backend.Result[backend.ItemSample], error) {
	r.mu.Lock()
	r.n++
	r.mu.Unlock()
	// Advancing from inside the callback is safe: Fake fires callbacks
	// with its own lock released (see clocktest.Fake's doc comment).
	r.clk.Advance(r.cost)
	out := make([]backend.Result[backend.ItemSample], len(items))
	for i := range items {
		out[i] = backend.Result[backend.ItemSample]{Value: backend.ItemSample{
			Value: xmlda.NewInt32(1), Quality: xmlda.NewGoodQuality()}}
	}
	return out, nil
}

func (r *slowReader) count() int { r.mu.Lock(); defer r.mu.Unlock(); return r.n }
func (r *slowReader) reset()     { r.mu.Lock(); defer r.mu.Unlock(); r.n = 0 }

// --- NEU-2: a wg-tracked timer refuses Reset ---

// TestTrackedTimer_ResetPanics pins the guard on armTimer's accounting.
// The counter is taken once when the timer is armed and released once by
// whichever of {callback runs, Stop prevents it} happens; resetting a
// stopped timer would re-arm a callback whose counter is already
// released, taking the WaitGroup negative — which panics far from the
// cause. Failing at the call site instead is the whole point.
func TestTrackedTimer_ResetPanics(t *testing.T) {
	m := NewManager(backend.Backend{Status: fixStatus{}, Reader: fixReader{}}, nil, nil, nil,
		Config{ReapInterval: time.Hour})
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })

	timer := m.armTimer(time.Hour, func() {})
	defer timer.Stop()

	defer func() {
		if r := recover(); r == nil {
			t.Error("Reset on a wg-tracked timer did not panic")
		}
	}()
	timer.Reset(time.Second)
}
