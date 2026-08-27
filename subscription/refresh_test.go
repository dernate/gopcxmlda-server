package subscription

import (
	"context"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock/clocktest"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// longPollCfg keeps background poll/reap scheduling far outside any test's
// Advance window, so PendingCount-based synchronization only ever
// observes the timer(s) the test itself cares about.
func longPollCfg() Config {
	return Config{ReapInterval: time.Hour, DefaultSamplingRate: time.Hour}
}

type refreshCall struct {
	result RefreshResult
	err    error
}

func callRefreshAsync(m *Manager, req RefreshRequest) <-chan refreshCall {
	ch := make(chan refreshCall, 1)
	go func() {
		res, err := m.PolledRefresh(context.Background(), req)
		ch <- refreshCall{res, err}
	}()
	return ch
}

func waitForPending(t *testing.T, fake *clocktest.Fake, n int) {
	t.Helper()
	if !fake.WaitForPending(n, 2*time.Second) {
		t.Fatalf("timed out waiting for %d pending timer(s), got %d", n, fake.PendingCount())
	}
}

func awaitRefresh(t *testing.T, ch <-chan refreshCall) refreshCall {
	t.Helper()
	select {
	case call := <-ch:
		return call
	case <-time.After(2 * time.Second):
		t.Fatalf("PolledRefresh did not return in time")
		return refreshCall{}
	}
}

func createOneItem(t *testing.T, m *Manager, ref backend.ItemRef, rate time.Duration, buffering bool) Handle {
	t.Helper()
	res, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH1", RequestedSamplingRate: rate, EnableBuffering: buffering}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Handle == "" {
		t.Fatalf("expected a valid handle")
	}
	return res.Handle
}

func TestPolledRefresh_NoHoldTime_ReturnsImmediately(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(r, fake, longPollCfg())
	defer shutdownManager(t, m)
	handle := createOneItem(t, m, ref, time.Hour, false)

	res, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{handle}})
	if err != nil {
		t.Fatalf("PolledRefresh: %v", err)
	}
	if len(res.Subscriptions) != 0 {
		t.Fatalf("expected no subscriptions (nothing changed since Create), got %+v", res.Subscriptions)
	}
}

func TestPolledRefresh_HoldTime_NoChanges_WaitsFullDuration(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(r, fake, longPollCfg())
	defer shutdownManager(t, m)
	handle := createOneItem(t, m, ref, time.Hour, false)

	before := fake.PendingCount()
	hold := fake.Now().Add(2 * time.Second)
	ch := callRefreshAsync(m, RefreshRequest{Handles: []Handle{handle}, HoldTime: &hold, WaitTime: time.Second})

	waitForPending(t, fake, before+1) // Phase 1 (hold) timer registered
	fake.Advance(2 * time.Second)
	waitForPending(t, fake, before+1) // Phase 2 (wait) timer registered
	fake.Advance(time.Second)

	call := awaitRefresh(t, ch)
	if call.err != nil {
		t.Fatalf("PolledRefresh: %v", call.err)
	}
	if len(call.result.Subscriptions) != 0 {
		t.Fatalf("expected no subscriptions reported, got %+v", call.result.Subscriptions)
	}
}

func TestPolledRefresh_EarlyReturnOnChangeDuringWait(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(r, fake, longPollCfg())
	defer shutdownManager(t, m)
	handle := createOneItem(t, m, ref, time.Hour, false)

	before := fake.PendingCount()
	hold := fake.Now().Add(2 * time.Second)
	ch := callRefreshAsync(m, RefreshRequest{Handles: []Handle{handle}, HoldTime: &hold, WaitTime: 10 * time.Second})

	waitForPending(t, fake, before+1)
	fake.Advance(2 * time.Second) // hold elapses, enters wait phase
	waitForPending(t, fake, before+1)

	// Simulate an external change and manually trigger the change signal
	// (bypassing poll scheduling, which is parked far in the future by
	// longPollCfg) to isolate exactly the early-return-on-change behavior.
	r.Set(ref, xmlda.NewInt32(2))
	m.mu.RLock()
	s := m.subs[handle]
	m.mu.RUnlock()
	if changed := applyUpdate(s.items[0], backend.ItemSample{Value: xmlda.NewInt32(2), Quality: xmlda.NewGoodQuality()}, xmlda.ErrorCode{}, m.cfg.MaxBufferedSamplesPerItem); changed {
		s.notifyChanged()
	}

	call := awaitRefresh(t, ch)
	if call.err != nil {
		t.Fatalf("PolledRefresh: %v", call.err)
	}
	if len(call.result.Subscriptions) != 1 || len(call.result.Subscriptions[0].Items) != 1 {
		t.Fatalf("expected exactly one changed item, got %+v", call.result.Subscriptions)
	}
	i32, err := call.result.Subscriptions[0].Items[0].Sample.Value.Int32()
	if err != nil || i32 != 2 {
		t.Fatalf("got (%d, %v), want (2, nil)", i32, err)
	}
	// The 10s WaitTime must NOT have been fully consumed — confirm by
	// checking the clock only advanced by the 2s hold, not 12s.
	if got := fake.Now(); !got.Equal(testEpoch.Add(2 * time.Second)) {
		t.Fatalf("got clock at %v, want %v (early return must not itself advance time)", got, testEpoch.Add(2*time.Second))
	}
}

func TestPolledRefresh_ChangeDuringHold_ReturnsFastAfterHoldElapses(t *testing.T) {
	// A change during the HOLD phase must not cause an early return
	// before HoldTime itself — but once HoldTime elapses, the manager
	// must not needlessly wait out the full WaitTime on top, since data
	// is already available (see refresh.go's hasPendingChanges check).
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(r, fake, longPollCfg())
	defer shutdownManager(t, m)
	handle := createOneItem(t, m, ref, time.Hour, false)

	before := fake.PendingCount()
	hold := fake.Now().Add(2 * time.Second)
	ch := callRefreshAsync(m, RefreshRequest{Handles: []Handle{handle}, HoldTime: &hold, WaitTime: 10 * time.Second})
	waitForPending(t, fake, before+1)

	// Change happens mid-hold.
	m.mu.RLock()
	s := m.subs[handle]
	m.mu.RUnlock()
	applyUpdate(s.items[0], backend.ItemSample{Value: xmlda.NewInt32(2), Quality: xmlda.NewGoodQuality()}, xmlda.ErrorCode{}, m.cfg.MaxBufferedSamplesPerItem)

	select {
	case <-ch:
		t.Fatalf("PolledRefresh must not return before HoldTime elapses, even if a change already occurred")
	case <-time.After(20 * time.Millisecond):
	}

	fake.Advance(2 * time.Second) // hold elapses; data already pending, phase 2 must be skipped

	call := awaitRefresh(t, ch)
	if call.err != nil {
		t.Fatalf("PolledRefresh: %v", call.err)
	}
	if len(call.result.Subscriptions) != 1 {
		t.Fatalf("expected the mid-hold change to be reported, got %+v", call.result.Subscriptions)
	}
	// Confirm phase 2's 10s WaitTime was skipped entirely: clock only at +2s.
	if got := fake.Now(); !got.Equal(testEpoch.Add(2 * time.Second)) {
		t.Fatalf("got clock at %v, want %v (phase 2 must have been skipped)", got, testEpoch.Add(2*time.Second))
	}
}

func TestPolledRefresh_ReturnAllItems_IgnoresWaitTime(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(r, fake, longPollCfg())
	defer shutdownManager(t, m)
	handle := createOneItem(t, m, ref, time.Hour, false)

	before := fake.PendingCount()
	hold := fake.Now().Add(2 * time.Second)
	ch := callRefreshAsync(m, RefreshRequest{Handles: []Handle{handle}, HoldTime: &hold, WaitTime: 10 * time.Second, ReturnAllItems: true})
	waitForPending(t, fake, before+1)
	fake.Advance(2 * time.Second) // ReturnAllItems: WaitTime must be ignored entirely

	call := awaitRefresh(t, ch)
	if call.err != nil {
		t.Fatalf("PolledRefresh: %v", call.err)
	}
	if len(call.result.Subscriptions) != 1 || len(call.result.Subscriptions[0].Items) != 1 {
		t.Fatalf("expected a full snapshot of the one subscribed item, got %+v", call.result.Subscriptions)
	}
	if got := fake.Now(); !got.Equal(testEpoch.Add(2 * time.Second)) {
		t.Fatalf("got clock at %v, want %v (WaitTime must be ignored under ReturnAllItems)", got, testEpoch.Add(2*time.Second))
	}
}

func TestPolledRefresh_Buffering_CapturesMultipleChanges(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(r, fake, Config{ReapInterval: time.Hour})
	defer shutdownManager(t, m)
	handle := createOneItem(t, m, ref, time.Second, true) // EnableBuffering=true

	// Each Advance(1s) synchronously triggers exactly one poll cycle
	// (AfterFunc callbacks run inline within Advance — see poll.go).
	r.Set(ref, xmlda.NewInt32(2))
	fake.Advance(time.Second)
	r.Set(ref, xmlda.NewInt32(3))
	fake.Advance(time.Second)

	res, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{handle}})
	if err != nil {
		t.Fatalf("PolledRefresh: %v", err)
	}
	if len(res.Subscriptions) != 1 {
		t.Fatalf("expected one subscription reported, got %+v", res.Subscriptions)
	}
	items := res.Subscriptions[0].Items
	if len(items) != 2 {
		t.Fatalf("expected both buffered changes (2 and 3), got %d items: %+v", len(items), items)
	}
	v1, _ := items[0].Sample.Value.Int32()
	v2, _ := items[1].Sample.Value.Int32()
	if v1 != 2 || v2 != 3 {
		t.Fatalf("got values %d, %d, want 2, 3 (chronological order)", v1, v2)
	}
}

func TestPolledRefresh_NoBuffering_KeepsOnlyLatest(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(r, fake, Config{ReapInterval: time.Hour})
	defer shutdownManager(t, m)
	handle := createOneItem(t, m, ref, time.Second, false) // EnableBuffering=false

	r.Set(ref, xmlda.NewInt32(2))
	fake.Advance(time.Second)
	r.Set(ref, xmlda.NewInt32(3))
	fake.Advance(time.Second)

	res, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{handle}})
	if err != nil {
		t.Fatalf("PolledRefresh: %v", err)
	}
	items := res.Subscriptions[0].Items
	if len(items) != 1 {
		t.Fatalf("expected only the latest value without buffering, got %d items: %+v", len(items), items)
	}
	v, _ := items[0].Sample.Value.Int32()
	if v != 3 {
		t.Fatalf("got %d, want 3 (latest)", v)
	}
}

func TestPolledRefresh_BufferOverflow_SetsDataBufferOverflow(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(0))
	m := newTestManager(r, fake, Config{ReapInterval: time.Hour, MaxBufferedSamplesPerItem: 3})
	defer shutdownManager(t, m)
	handle := createOneItem(t, m, ref, time.Second, true)

	for i := 1; i <= 5; i++ {
		r.Set(ref, xmlda.NewInt32(int32(i)))
		fake.Advance(time.Second)
	}

	res, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{handle}})
	if err != nil {
		t.Fatalf("PolledRefresh: %v", err)
	}
	if !res.DataBufferOverflow {
		t.Fatalf("expected DataBufferOverflow=true after exceeding MaxBufferedSamplesPerItem")
	}
	items := res.Subscriptions[0].Items
	if len(items) != 3 {
		t.Fatalf("got %d buffered items, want 3 (bounded)", len(items))
	}
	// Oldest purged first; the Latest Changed Value must survive.
	last, _ := items[len(items)-1].Sample.Value.Int32()
	if last != 5 {
		t.Fatalf("got latest=%d, want 5 (LCV must always survive overflow)", last)
	}
}

func TestPolledRefresh_MultipleHandlesInOneCall(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref1 := backend.ItemRef{ItemName: "Item1"}
	ref2 := backend.ItemRef{ItemName: "Item2"}
	r.Set(ref1, xmlda.NewInt32(1))
	r.Set(ref2, xmlda.NewInt32(2))
	m := newTestManager(r, fake, Config{ReapInterval: time.Hour})
	defer shutdownManager(t, m)
	h1 := createOneItem(t, m, ref1, time.Second, true)
	h2 := createOneItem(t, m, ref2, time.Second, true)

	r.Set(ref1, xmlda.NewInt32(10))
	r.Set(ref2, xmlda.NewInt32(20))
	fake.Advance(time.Second)

	res, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{h1, h2}})
	if err != nil {
		t.Fatalf("PolledRefresh: %v", err)
	}
	if len(res.Subscriptions) != 2 {
		t.Fatalf("got %d subscriptions, want 2", len(res.Subscriptions))
	}
	byHandle := map[Handle]RefreshSubscriptionResult{}
	for _, s := range res.Subscriptions {
		byHandle[s.Handle] = s
	}
	v1, _ := byHandle[h1].Items[0].Sample.Value.Int32()
	v2, _ := byHandle[h2].Items[0].Sample.Value.Int32()
	if v1 != 10 || v2 != 20 {
		t.Fatalf("got v1=%d v2=%d, want 10, 20", v1, v2)
	}
}

func TestPolledRefresh_InvalidHandleMixedWithValid(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(r, fake, longPollCfg())
	defer shutdownManager(t, m)
	handle := createOneItem(t, m, ref, time.Hour, false)

	res, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{handle, "bogus"}})
	if err != nil {
		t.Fatalf("PolledRefresh: %v", err)
	}
	if len(res.InvalidHandles) != 1 || res.InvalidHandles[0] != "bogus" {
		t.Fatalf("got %v, want [bogus]", res.InvalidHandles)
	}
}

func TestPolledRefresh_AllInvalidHandles_ErrNoSubscription(t *testing.T) {
	fake := clocktest.New(testEpoch)
	m := newTestManager(newFakeReader(), fake, longPollCfg())
	defer shutdownManager(t, m)

	_, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{"bogus1", "bogus2"}})
	if err != ErrNoSubscription {
		t.Fatalf("got %v, want ErrNoSubscription", err)
	}
}

func TestPolledRefresh_EmptyHandleList_ErrNoSubscription(t *testing.T) {
	fake := clocktest.New(testEpoch)
	m := newTestManager(newFakeReader(), fake, longPollCfg())
	defer shutdownManager(t, m)

	_, err := m.PolledRefresh(context.Background(), RefreshRequest{})
	if err != ErrNoSubscription {
		t.Fatalf("got %v, want ErrNoSubscription", err)
	}
}

func TestPolledRefresh_OverlappingCalls_EBusy(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(r, fake, longPollCfg())
	defer shutdownManager(t, m)
	handle := createOneItem(t, m, ref, time.Hour, false)

	before := fake.PendingCount()
	hold := fake.Now().Add(time.Hour)
	ch1 := callRefreshAsync(m, RefreshRequest{Handles: []Handle{handle}, HoldTime: &hold, WaitTime: time.Second})
	waitForPending(t, fake, before+1) // first call now holds the busy flag

	_, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{handle}})
	if err != ErrBusy {
		t.Fatalf("got %v, want ErrBusy", err)
	}

	// Clean up: cancel to unblock the first call before returning.
	m.Cancel(handle)
	awaitRefresh(t, ch1)
}

func TestPolledRefresh_DisjointHandles_NoFalseBusy(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref1 := backend.ItemRef{ItemName: "Item1"}
	ref2 := backend.ItemRef{ItemName: "Item2"}
	r.Set(ref1, xmlda.NewInt32(1))
	r.Set(ref2, xmlda.NewInt32(2))
	m := newTestManager(r, fake, longPollCfg())
	defer shutdownManager(t, m)
	h1 := createOneItem(t, m, ref1, time.Hour, false)
	h2 := createOneItem(t, m, ref2, time.Hour, false)

	before := fake.PendingCount()
	hold := fake.Now().Add(time.Hour)
	ch1 := callRefreshAsync(m, RefreshRequest{Handles: []Handle{h1}, HoldTime: &hold, WaitTime: time.Second})
	waitForPending(t, fake, before+1)

	// h2 is disjoint from h1's in-flight call — must NOT report E_BUSY.
	res, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{h2}})
	if err != nil {
		t.Fatalf("expected no error for a disjoint handle, got %v", err)
	}
	_ = res

	m.Cancel(h1)
	awaitRefresh(t, ch1)
}

// TestPolledRefresh_DuplicateHandleInOneRequest_NoSelfBusy is a regression
// test: a single ServerSubHandle listed twice in the same request's Handles
// used to make the busy-flag acquisition loop CAS against the flag it had
// just set on the handle's first occurrence, self-triggering ErrBusy even
// though no other call is concurrently touching the subscription at all.
func TestPolledRefresh_DuplicateHandleInOneRequest_NoSelfBusy(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(r, fake, longPollCfg())
	defer shutdownManager(t, m)
	handle := createOneItem(t, m, ref, time.Hour, false)

	res, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{handle, handle}})
	if err != nil {
		t.Fatalf("PolledRefresh with a duplicated handle: got %v, want nil (not a real conflict)", err)
	}
	// A duplicated handle must also not produce a duplicated subscription
	// entry in the result.
	if len(res.Subscriptions) > 1 {
		t.Fatalf("expected at most one subscription entry for a deduplicated handle, got %d: %+v", len(res.Subscriptions), res.Subscriptions)
	}

	// The subscription must remain usable afterward (busy flag correctly
	// released exactly once, not left stuck acquired).
	if _, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{handle}}); err != nil {
		t.Fatalf("expected the subscription to remain usable after a deduplicated call, got %v", err)
	}
}

func TestPolledRefresh_CancelMidHold_UnblocksImmediately(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(r, fake, longPollCfg())
	defer shutdownManager(t, m)
	handle := createOneItem(t, m, ref, time.Hour, false)

	before := fake.PendingCount()
	hold := fake.Now().Add(time.Hour) // very long hold — must NOT need to elapse
	ch := callRefreshAsync(m, RefreshRequest{Handles: []Handle{handle}, HoldTime: &hold, WaitTime: time.Second})
	waitForPending(t, fake, before+1)

	m.Cancel(handle)

	call := awaitRefresh(t, ch)
	if call.err != nil {
		t.Fatalf("PolledRefresh: %v", call.err)
	}
	if len(call.result.Subscriptions) != 0 {
		t.Fatalf("expected the cancelled subscription to be omitted, got %+v", call.result.Subscriptions)
	}
}

func TestPolledRefresh_ShutdownMidHold_UnblocksImmediately(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(r, fake, longPollCfg())
	handle := createOneItem(t, m, ref, time.Hour, false)

	before := fake.PendingCount()
	hold := fake.Now().Add(time.Hour)
	ch := callRefreshAsync(m, RefreshRequest{Handles: []Handle{handle}, HoldTime: &hold, WaitTime: time.Second})
	waitForPending(t, fake, before+1)

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- m.Shutdown(context.Background()) }()

	awaitRefresh(t, ch)

	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Shutdown did not complete promptly after mid-hold cancellation")
	}
}

func TestPolledRefresh_RequestContextCancellation(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(r, fake, longPollCfg())
	defer shutdownManager(t, m)
	handle := createOneItem(t, m, ref, time.Hour, false)

	ctx, cancel := context.WithCancel(context.Background())
	before := fake.PendingCount()
	hold := fake.Now().Add(time.Hour)

	ch := make(chan refreshCall, 1)
	go func() {
		res, err := m.PolledRefresh(ctx, RefreshRequest{Handles: []Handle{handle}, HoldTime: &hold, WaitTime: time.Second})
		ch <- refreshCall{res, err}
	}()
	waitForPending(t, fake, before+1)

	cancel()

	call := awaitRefresh(t, ch)
	if call.err == nil {
		t.Fatalf("expected an error after request context cancellation")
	}
}
