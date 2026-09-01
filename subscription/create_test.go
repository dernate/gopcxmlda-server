package subscription

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock/clocktest"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

func TestCreate_AllValidItems(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(42))
	m := newTestManager(r, fake, Config{})
	defer shutdownManager(t, m)

	res, err := m.Create(context.Background(), CreateRequest{
		Items:               []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH1"}},
		ReturnValuesOnReply: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Handle == "" {
		t.Fatalf("expected a non-empty handle for a valid item")
	}
	if len(res.Items) != 1 || !res.Items[0].ResultID.IsZero() {
		t.Fatalf("got %+v", res.Items)
	}
	if !res.Items[0].HaveSample {
		t.Fatalf("expected ReturnValuesOnReply to populate a sample")
	}
	i32, err := res.Items[0].Sample.Value.Int32()
	if err != nil || i32 != 42 {
		t.Fatalf("got (%d, %v), want (42, nil)", i32, err)
	}
}

func TestCreate_AllInvalidItems_EmptyHandle(t *testing.T) {
	// REQ-SUBSCRIPTION-002: empty ServerSubHandle iff every item invalid.
	fake := clocktest.New(testEpoch)
	r := newFakeReader() // nothing registered: every item is "not found"
	m := newTestManager(r, fake, Config{})
	defer shutdownManager(t, m)

	res, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{{Ref: backend.ItemRef{ItemName: "Unknown"}, ClientItemHandle: "CIH1"}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Handle != "" {
		t.Fatalf("expected empty handle, got %q", res.Handle)
	}
	if res.Items[0].ResultID != xmlda.ErrUnknownItemName {
		t.Fatalf("got %+v", res.Items[0])
	}
}

func TestCreate_PartiallyValid_SubscriptionCreated(t *testing.T) {
	// A subscription is created if AT LEAST ONE item is valid.
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	goodRef := backend.ItemRef{ItemName: "Good"}
	r.Set(goodRef, xmlda.NewInt32(1))
	m := newTestManager(r, fake, Config{})
	defer shutdownManager(t, m)

	res, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{
			{Ref: goodRef, ClientItemHandle: "CIH1"},
			{Ref: backend.ItemRef{ItemName: "Bad"}, ClientItemHandle: "CIH2"},
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Handle == "" {
		t.Fatalf("expected a subscription to be created when at least one item is valid")
	}
	if !res.Items[0].ResultID.IsZero() {
		t.Fatalf("item 0 should be valid, got %+v", res.Items[0])
	}
	if res.Items[1].ResultID != xmlda.ErrUnknownItemName {
		t.Fatalf("item 1 should be invalid, got %+v", res.Items[1])
	}
}

func TestCreate_PingRateZero_ResolvesToDefault(t *testing.T) {
	// REQ-SUBSCRIPTION-015 / OQ-10: SubscriptionPingRate=0 must not mean
	// "reap immediately" — it resolves to Config.DefaultSubscriptionPingRate.
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	cfg := Config{DefaultSubscriptionPingRate: time.Minute, ReapInterval: time.Second, ReapGraceMultiplier: 1}
	m := newTestManager(r, fake, cfg)
	defer shutdownManager(t, m)

	res, err := m.Create(context.Background(), CreateRequest{
		Items:                []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH1"}},
		SubscriptionPingRate: 0,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Advance well past what a literal-zero ping rate would have reaped
	// at (immediately), but short of the real 1-minute default grace.
	fake.Advance(5 * time.Second)
	if m.count() != 1 {
		t.Fatalf("expected the subscription to survive with PingRate=0 resolved to the 1-minute default, got count=%d", m.count())
	}
	_ = res
}

func TestCreate_MaxConcurrentSubscriptions(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref1 := backend.ItemRef{ItemName: "Item1"}
	ref2 := backend.ItemRef{ItemName: "Item2"}
	r.Set(ref1, xmlda.NewInt32(1))
	r.Set(ref2, xmlda.NewInt32(2))
	m := newTestManager(r, fake, Config{MaxConcurrentSubscriptions: 1})
	defer shutdownManager(t, m)

	if _, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{{Ref: ref1, ClientItemHandle: "CIH1"}},
	}); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	_, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{{Ref: ref2, ClientItemHandle: "CIH2"}},
	})
	if !errors.Is(err, ErrTooManySubscriptions) {
		t.Fatalf("got %v, want ErrTooManySubscriptions", err)
	}
	if m.count() != 1 {
		t.Fatalf("expected the rejected Create to not increase the count, got %d", m.count())
	}
}

// TestCreate_MaxConcurrentSubscriptions_ConcurrentRace reproduces many
// concurrent Create calls racing the MaxConcurrentSubscriptions=1 limit.
// Before this was fixed, the limit was checked once (m.count() against
// the configured max) before the backend.Reader.Read call, with no
// re-check under m.mu at the actual m.subs insert — so multiple
// concurrent callers could all observe count < max before any of them
// had inserted, letting the subscription count exceed the configured
// limit. The check and the insert must be atomic (ADR-007).
func TestCreate_MaxConcurrentSubscriptions_ConcurrentRace(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	const n = 20
	refs := make([]backend.ItemRef, n)
	for i := range n {
		refs[i] = backend.ItemRef{ItemName: fmt.Sprintf("Item%d", i)}
		r.Set(refs[i], xmlda.NewInt32(int32(i)))
	}
	m := newTestManager(r, fake, Config{MaxConcurrentSubscriptions: 1})
	defer shutdownManager(t, m)

	var wg sync.WaitGroup
	var successes atomic.Int32
	var tooMany atomic.Int32
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := m.Create(context.Background(), CreateRequest{
				Items: []CreateItemRequest{{Ref: refs[i], ClientItemHandle: fmt.Sprintf("CIH%d", i)}},
			})
			switch {
			case err == nil && res.Handle != "":
				successes.Add(1)
			case errors.Is(err, ErrTooManySubscriptions):
				tooMany.Add(1)
			case err != nil:
				t.Errorf("Create: unexpected error: %v", err)
			default:
				t.Errorf("Create: got no error but an empty handle: %+v", res)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if got := successes.Load(); got != 1 {
		t.Fatalf("got %d successful concurrent Creates, want exactly 1 (MaxConcurrentSubscriptions=1)", got)
	}
	if got := tooMany.Load(); got != n-1 {
		t.Fatalf("got %d ErrTooManySubscriptions, want %d", got, n-1)
	}
	if m.count() != 1 {
		t.Fatalf("m.count() = %d, want 1 — MaxConcurrentSubscriptions was not enforced atomically", m.count())
	}
}

// TestCreate_AfterBeginShutdown_ErrShuttingDown reproduces the gap where
// Create, once BeginShutdown had run, returned the raw m.rootCtx.Err()
// (context.Canceled) instead of the dedicated ErrShuttingDown — the
// server layer maps ErrShuttingDown to E_SERVERSTATE specifically, so a
// bare context.Canceled here would have fallen back to a generic E_FAIL
// fault for what is actually a well-understood condition.
func TestCreate_AfterBeginShutdown_ErrShuttingDown(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(r, fake, Config{})

	m.BeginShutdown()

	_, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH1"}},
	})
	if !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("got %v, want ErrShuttingDown", err)
	}
}

func TestCancel_Idempotent(t *testing.T) {
	// REQ-SUBSCRIPTION-014 / OQ-9: cancelling twice, or an unknown handle,
	// is a safe no-op.
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(r, fake, Config{})
	defer shutdownManager(t, m)

	res, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH1"}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if !m.Cancel(res.Handle) {
		t.Fatalf("expected first Cancel to report found=true")
	}
	if m.Cancel(res.Handle) {
		t.Fatalf("expected second Cancel (already cancelled) to report found=false, not error/panic")
	}
	if m.Cancel("never-existed") {
		t.Fatalf("expected Cancel on an unknown handle to report found=false, not error/panic")
	}
	if m.count() != 0 {
		t.Fatalf("expected 0 active subscriptions after cancel, got %d", m.count())
	}
}

// TestCreate_BackendRevisesSamplingRate pins the fix for
// RevisedSamplingRate always echoing the requested rate. A backend with a
// fixed scan cycle can now say so, and the item reports
// S_UNSUPPORTEDRATE — the specification's own signal for "you asked for a
// rate I cannot do; here is the one you get" (§3.5.2) — while staying
// subscribed, since it is a success code.
func TestCreate_BackendRevisesSamplingRate(t *testing.T) {
	r := &rateReviser{fakeReader: newFakeReader(), fixedRate: time.Second}
	r.Set(backend.ItemRef{ItemName: "A"}, xmlda.NewInt32(1))
	fake := clocktest.New(testEpoch)
	m := newTestManager(r, fake, Config{ReapInterval: time.Hour})
	defer shutdownManager(t, m)

	res, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{{
			Ref:                   backend.ItemRef{ItemName: "A"},
			RequestedSamplingRate: 50 * time.Millisecond,
		}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if r.Calls() != 1 {
		t.Errorf("ReviseSamplingRates was called %d times, want exactly 1 (one batch call)", r.Calls())
	}
	if len(res.Items) != 1 {
		t.Fatalf("got %d item results, want 1", len(res.Items))
	}
	it := res.Items[0]
	if it.RevisedSamplingRate != time.Second {
		t.Errorf("RevisedSamplingRate = %v, want 1s — the backend's answer was ignored", it.RevisedSamplingRate)
	}
	if it.ResultID != xmlda.SuccessUnsupportedRate {
		t.Errorf("ResultID = %v, want S_UNSUPPORTEDRATE", it.ResultID)
	}
	if res.Handle == "" {
		t.Error("no subscription was created; S_UNSUPPORTEDRATE is a success code and must not exclude the item")
	}
	// The engine must actually poll at the revised rate, not the requested
	// one — otherwise the code is a label with no behavior behind it.
	m.mu.RLock()
	s := m.subs[res.Handle]
	m.mu.RUnlock()
	if got := s.items[0].revisedSamplingRate; got != time.Second {
		t.Errorf("the item is scheduled at %v, not the revised 1s", got)
	}
}

// TestCreate_RateUnchangedReportsNoCondition pins that a backend agreeing
// with the requested rate produces no result code at all — S_UNSUPPORTED
// RATE on every item would make the code meaningless.
func TestCreate_RateUnchangedReportsNoCondition(t *testing.T) {
	r := &rateReviser{fakeReader: newFakeReader(), fixedRate: 250 * time.Millisecond}
	r.Set(backend.ItemRef{ItemName: "A"}, xmlda.NewInt32(1))
	fake := clocktest.New(testEpoch)
	m := newTestManager(r, fake, Config{ReapInterval: time.Hour})
	defer shutdownManager(t, m)

	res, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{{
			Ref:                   backend.ItemRef{ItemName: "A"},
			RequestedSamplingRate: 250 * time.Millisecond,
		}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := res.Items[0].ResultID; !got.IsZero() {
		t.Errorf("ResultID = %v, want none: the backend granted exactly what was asked", got)
	}
}

// TestCreate_RateRevisionFailureHonorsRequest pins the fallbacks: a
// reviser that errors, or that answers with the wrong number of rates, is
// a reason to serve the subscription at the rate the client named — not a
// reason to refuse it.
func TestCreate_RateRevisionFailureHonorsRequest(t *testing.T) {
	for name, r := range map[string]*rateReviser{
		"error":       {fakeReader: newFakeReader(), err: errors.New("device offline")},
		"wrong count": {fakeReader: newFakeReader(), fixedRate: time.Second, shortBy: 1},
	} {
		t.Run(name, func(t *testing.T) {
			r.Set(backend.ItemRef{ItemName: "A"}, xmlda.NewInt32(1))
			fake := clocktest.New(testEpoch)
			m := newTestManager(r, fake, Config{ReapInterval: time.Hour})
			defer shutdownManager(t, m)

			res, err := m.Create(context.Background(), CreateRequest{
				Items: []CreateItemRequest{{
					Ref:                   backend.ItemRef{ItemName: "A"},
					RequestedSamplingRate: 300 * time.Millisecond,
				}},
			})
			if err != nil {
				t.Fatalf("Create failed instead of falling back: %v", err)
			}
			if res.Handle == "" {
				t.Fatal("no subscription was created")
			}
			it := res.Items[0]
			if it.RevisedSamplingRate != 300*time.Millisecond {
				t.Errorf("RevisedSamplingRate = %v, want the requested 300ms", it.RevisedSamplingRate)
			}
			if !it.ResultID.IsZero() {
				t.Errorf("ResultID = %v, want none", it.ResultID)
			}
		})
	}
}

// TestCreate_SuccessCodeItemIsStillSubscribed pins a smaller correctness
// fix in the same area: an initial read that came back with a
// success-with-caveat code (S_CLAMP) describes a readable item, so it must
// be subscribed. The old `res.ResultID.IsZero()` gate silently dropped
// items the backend had explicitly called usable.
func TestCreate_SuccessCodeItemIsStillSubscribed(t *testing.T) {
	r := &clampReader{fakeReader: newFakeReader()}
	r.Set(backend.ItemRef{ItemName: "A"}, xmlda.NewInt32(1))
	fake := clocktest.New(testEpoch)
	m := newTestManager(r, fake, Config{ReapInterval: time.Hour})
	defer shutdownManager(t, m)

	res, err := m.Create(context.Background(), CreateRequest{
		Items:               []CreateItemRequest{{Ref: backend.ItemRef{ItemName: "A"}}},
		ReturnValuesOnReply: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Handle == "" {
		t.Fatal("an S_CLAMP item was not subscribed")
	}
	if got := res.Items[0].ResultID; got != xmlda.SuccessClamp {
		t.Errorf("ResultID = %v, want S_CLAMP preserved", got)
	}
	if !res.Items[0].HaveSample {
		t.Error("the item's value was dropped despite a success code")
	}
}

// --- the server-wide item budget ---

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
