package subscription

import (
	"context"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock/clocktest"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// --- buffered samples have a server-wide budget ---

// TestSampleBudget_ExhaustionDegradesToLatestValue pins the fix for the
// third axis of the limit multiplication: MaxTotalSubscribedItems and
// MaxBufferedSamplesPerItem together bounded twenty million buffered
// entries, and nothing bounded the product. On exhaustion an item keeps
// only its Latest Changed Value — which REQ-SUBSCRIPTION-007 preserves
// regardless of any limit — and flags the loss.
func TestSampleBudget_ExhaustionDegradesToLatestValue(t *testing.T) {
	budget := &sampleBudget{max: 3}
	it := &itemState{enableBuffering: true, haveLast: true,
		last: backend.ItemSample{Value: xmlda.NewInt32(0), Quality: xmlda.NewGoodQuality()}}

	for v := int32(1); v <= 6; v++ {
		applyUpdate(it, backend.ItemSample{Value: xmlda.NewInt32(v), Quality: xmlda.NewGoodQuality()},
			xmlda.ErrorCode{}, 100, budget)
	}

	it.mu.Lock()
	got := len(it.buffer)
	overflowed := it.overflowed
	last := it.buffer[len(it.buffer)-1]
	it.mu.Unlock()

	if got > 3 {
		t.Errorf("buffer holds %d entries, past the budget of 3", got)
	}
	if !overflowed {
		t.Error("the discard was not flagged; DataBufferOverflow would never reach the client")
	}
	if n := budget.count(); n > 3 {
		t.Errorf("budget counter = %d, past its own maximum of 3", n)
	}
	// The Latest Changed Value is the one entry that must survive.
	v, err := last.sample.Value.Int32()
	if err != nil || v != 6 {
		t.Errorf("the newest value was not retained: got %v (err %v), want 6", v, err)
	}
}

// TestSampleBudget_ReleasedOnDeliveryAndCancel pins the accounting.
// Miscounting here is how a budget starts refusing buffering to live
// items on behalf of samples that were delivered — or subscriptions that
// were cancelled — long ago.
func TestSampleBudget_ReleasedOnDeliveryAndCancel(t *testing.T) {
	r := newFakeReader()
	r.Set(backend.ItemRef{ItemName: "A"}, xmlda.NewInt32(1))
	fake := clocktest.New(testEpoch)
	m := newTestManager(r, fake, Config{
		ReapInterval:            time.Hour,
		MaxTotalBufferedSamples: 100,
	})
	defer shutdownManager(t, m)

	res, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{{Ref: backend.ItemRef{ItemName: "A"}, EnableBuffering: true}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	m.mu.RLock()
	s := m.subs[res.Handle]
	m.mu.RUnlock()

	// Values 2..6: the item's Create-time value is 1, and applyUpdate
	// buffers only actual changes, so starting at 1 would silently produce
	// one fewer entry than the loop suggests.
	for v := int32(2); v <= 6; v++ {
		applyUpdate(s.items[0], backend.ItemSample{Value: xmlda.NewInt32(v), Quality: xmlda.NewGoodQuality()},
			xmlda.ErrorCode{}, 100, m.budget)
	}
	if got := m.budget.count(); got != 5 {
		t.Fatalf("budget counter = %d after 5 buffered samples, want 5", got)
	}

	// Delivering them returns the slots.
	out, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{res.Handle}})
	if err != nil {
		t.Fatalf("PolledRefresh: %v", err)
	}
	if n := len(out.Subscriptions[0].Items); n != 5 {
		t.Errorf("delivered %d items, want 5", n)
	}
	if got := m.budget.count(); got != 0 {
		t.Errorf("budget counter = %d after delivery, want 0 — delivered samples leak their slots", got)
	}

	// So does cancelling a subscription with an undelivered backlog.
	for v := int32(10); v <= 13; v++ {
		applyUpdate(s.items[0], backend.ItemSample{Value: xmlda.NewInt32(v), Quality: xmlda.NewGoodQuality()},
			xmlda.ErrorCode{}, 100, m.budget)
	}
	if got := m.budget.count(); got != 4 {
		t.Fatalf("budget counter = %d, want 4", got)
	}
	if !m.Cancel(res.Handle) {
		t.Fatal("Cancel reported the handle as unknown")
	}
	if got := m.budget.count(); got != 0 {
		t.Errorf("budget counter = %d after Cancel, want 0 — a cancelled subscription leaks its backlog", got)
	}
}

// TestSampleBudget_NonBufferingItemsAreOutsideTheBudget pins the scoping
// decision: an item without EnableBuffering holds exactly one
// latest-value slot, bounded by the item count that
// MaxTotalSubscribedItems already governs. Letting it compete for this
// budget would let buffered history starve the plain change delivery
// every subscription depends on.
func TestSampleBudget_NonBufferingItemsAreOutsideTheBudget(t *testing.T) {
	budget := &sampleBudget{max: 1}
	it := &itemState{enableBuffering: false}
	for v := int32(1); v <= 5; v++ {
		applyUpdate(it, backend.ItemSample{Value: xmlda.NewInt32(v), Quality: xmlda.NewGoodQuality()},
			xmlda.ErrorCode{}, 100, budget)
	}
	if got := budget.count(); got != 0 {
		t.Errorf("budget counter = %d, want 0: non-buffering items are not counted", got)
	}
	it.mu.Lock()
	n := len(it.buffer)
	it.mu.Unlock()
	if n != 1 {
		t.Errorf("a non-buffering item holds %d entries, want exactly the latest", n)
	}
}

// TestSampleBudget_UnlimitedWhenNotConfigured pins that a zero/negative
// maximum disables the accounting entirely, so the default-free
// subscription.Config path behaves exactly as before.
func TestSampleBudget_UnlimitedWhenNotConfigured(t *testing.T) {
	for _, maxVal := range []int64{0, -1} {
		budget := &sampleBudget{max: maxVal}
		it := &itemState{enableBuffering: true}
		for v := int32(1); v <= 50; v++ {
			applyUpdate(it, backend.ItemSample{Value: xmlda.NewInt32(v), Quality: xmlda.NewGoodQuality()},
				xmlda.ErrorCode{}, 1000, budget)
		}
		it.mu.Lock()
		n := len(it.buffer)
		overflowed := it.overflowed
		it.mu.Unlock()
		if n != 50 {
			t.Errorf("max=%d: buffer holds %d entries, want all 50", maxVal, n)
		}
		if overflowed {
			t.Errorf("max=%d: overflow was flagged with no limit configured", maxVal)
		}
	}
}

// --- a wg-tracked timer refuses Reset ---

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

	timer, armed := m.armTimer(time.Hour, func() {})
	if !armed {
		t.Fatal("armTimer declined on a live Manager")
	}
	defer timer.Stop()

	defer func() {
		if r := recover(); r == nil {
			t.Error("Reset on a wg-tracked timer did not panic")
		}
	}()
	timer.Reset(time.Second)
}
