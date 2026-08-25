package subscription

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock/clocktest"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

func TestPush_ChangeEventUpdatesSubscription(t *testing.T) {
	fake := clocktest.New(testEpoch)
	pr := newPushReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	pr.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(pr, fake, Config{ReapInterval: time.Hour})
	defer shutdownManager(t, m)

	res, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH1", EnableBuffering: true}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	pr.Push(backend.ChangeEvent{Ref: ref, Sample: backend.ItemSample{Value: xmlda.NewInt32(99), Quality: xmlda.NewGoodQuality()}})

	// The drain goroutine processes the pushed event concurrently — this
	// is real-time synchronization with push mode's one documented
	// always-alive goroutine (ADR-008), not a simulation of protocol
	// timing, which is why it's a short bounded poll rather than a fixed
	// sleep.
	deadline := time.Now().Add(2 * time.Second)
	for {
		res2, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{res.Handle}})
		if err != nil {
			t.Fatalf("PolledRefresh: %v", err)
		}
		if len(res2.Subscriptions) > 0 {
			v, err := res2.Subscriptions[0].Items[0].Sample.Value.Int32()
			if err != nil || v != 99 {
				t.Fatalf("got (%d, %v), want (99, nil)", v, err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the pushed change to be reflected")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPush_FallsBackToPollOnWatchItemsError(t *testing.T) {
	fake := clocktest.New(testEpoch)
	fr := &failingWatchReader{fakeReader: newFakeReader()}
	ref := backend.ItemRef{ItemName: "Item1"}
	fr.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(fr, fake, Config{ReapInterval: time.Hour, DefaultSamplingRate: time.Second})
	defer shutdownManager(t, m)

	res, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH1", EnableBuffering: true}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	fr.Set(ref, xmlda.NewInt32(2))
	fake.Advance(time.Second) // poll-mode fallback should pick this up

	res2, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{res.Handle}})
	if err != nil {
		t.Fatalf("PolledRefresh: %v", err)
	}
	if len(res2.Subscriptions) != 1 {
		t.Fatalf("expected the poll-mode fallback to have picked up the change, got %+v", res2.Subscriptions)
	}
}

// TestPush_DuplicateItemRef_BothClientHandlesUpdated reproduces
// subscribing the same backend.ItemRef twice under two different
// ClientItemHandles in one push-mode Create call — legal, nothing in
// Create rejects it. Before this was fixed, startPush's byRef map kept
// only the last itemState registered for a given ItemRef, so the other
// duplicate never received another update for the subscription's whole
// lifetime, frozen at its Create-time value with no error surfaced.
func TestPush_DuplicateItemRef_BothClientHandlesUpdated(t *testing.T) {
	fake := clocktest.New(testEpoch)
	pr := newPushReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	pr.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(pr, fake, Config{ReapInterval: time.Hour})
	defer shutdownManager(t, m)

	res, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{
			{Ref: ref, ClientItemHandle: "CIH-A", EnableBuffering: true},
			{Ref: ref, ClientItemHandle: "CIH-B", EnableBuffering: true},
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(res.Items) != 2 || !res.Items[0].ResultID.IsZero() || !res.Items[1].ResultID.IsZero() {
		t.Fatalf("expected both duplicate-ref items to be accepted, got %+v", res.Items)
	}

	pr.Push(backend.ChangeEvent{Ref: ref, Sample: backend.ItemSample{Value: xmlda.NewInt32(99), Quality: xmlda.NewGoodQuality()}})

	// No ReturnAllItems: only items with an actual buffered change come
	// back, so — unlike a snapshot — an empty/partial result on an early
	// poll (before the drain goroutine has processed the pushed event
	// yet) is unambiguous and safe to just retry, not a false positive.
	seen := map[string]int32{}
	deadline := time.Now().Add(2 * time.Second)
	for len(seen) < 2 {
		res2, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{res.Handle}})
		if err != nil {
			t.Fatalf("PolledRefresh: %v", err)
		}
		if len(res2.Subscriptions) > 0 {
			for _, it := range res2.Subscriptions[0].Items {
				v, err := it.Sample.Value.Int32()
				if err != nil {
					t.Fatalf("Int32: %v", err)
				}
				seen[it.ClientItemHandle] = v
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the pushed change to reach both duplicate-ref items, got %+v so far", seen)
		}
		if len(seen) < 2 {
			time.Sleep(time.Millisecond)
		}
	}
	if seen["CIH-A"] != 99 || seen["CIH-B"] != 99 {
		t.Fatalf("got %+v, want both CIH-A and CIH-B updated to 99", seen)
	}
}

// TestPush_ChangeEventError_LogsAndLeavesItemStale exercises
// backend.ChangeEvent.Err (previously untested anywhere in this package,
// which is exactly how a documentation/behavior mismatch about it went
// unnoticed): a per-item watch error must be logged and otherwise
// tolerated, not crash the drain goroutine or the whole subscription — a
// subsequent, valid push for the same item must still be applied
// afterward, proving the drain loop kept running.
func TestPush_ChangeEventError_LogsAndLeavesItemStale(t *testing.T) {
	fake := clocktest.New(testEpoch)
	pr := newPushReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	pr.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(pr, fake, Config{ReapInterval: time.Hour})
	defer shutdownManager(t, m)

	res, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH1", EnableBuffering: true}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	pr.Push(backend.ChangeEvent{Ref: ref, Err: errors.New("simulated per-item watch failure")})
	pr.Push(backend.ChangeEvent{Ref: ref, Sample: backend.ItemSample{Value: xmlda.NewInt32(99), Quality: xmlda.NewGoodQuality()}})

	deadline := time.Now().Add(2 * time.Second)
	for {
		res2, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{res.Handle}})
		if err != nil {
			t.Fatalf("PolledRefresh: %v", err)
		}
		if len(res2.Subscriptions) > 0 {
			v, err := res2.Subscriptions[0].Items[0].Sample.Value.Int32()
			if err != nil || v != 99 {
				t.Fatalf("got (%d, %v), want (99, nil)", v, err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the valid push after the error event to be reflected")
		}
		time.Sleep(time.Millisecond)
	}
}

// blockingWatchReader is a backend.ChangeNotifier whose WatchItems blocks
// until the test explicitly releases it via Unblock, modeling a hung
// backend implementation.
type blockingWatchReader struct {
	*fakeReader
	release chan struct{}
}

func newBlockingWatchReader() *blockingWatchReader {
	return &blockingWatchReader{fakeReader: newFakeReader(), release: make(chan struct{})}
}

func (r *blockingWatchReader) WatchItems(ctx context.Context, items []backend.WatchRequest) (<-chan backend.ChangeEvent, error) {
	<-r.release
	return nil, errWatchUnavailable
}

func (r *blockingWatchReader) Unblock() { close(r.release) }

// TestPush_WatchItemsTimeout_FallsBackToPoll reproduces a backend whose
// ChangeNotifier.WatchItems call never returns. Before this was fixed,
// startPush called it synchronously with no bound at all, so Create (and
// therefore the whole Subscribe request) would block forever regardless
// of Config.RequestTimeout. It must instead give up after
// Config.PollTimeout and fall back to poll-mode, the same as an outright
// WatchItems error.
func TestPush_WatchItemsTimeout_FallsBackToPoll(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newBlockingWatchReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(r, fake, Config{ReapInterval: time.Hour, DefaultSamplingRate: time.Second, PollTimeout: 5 * time.Second})
	defer shutdownManager(t, m)
	// Must run (LIFO) before shutdownManager's Wait call above: startPush's
	// initial WatchItems call is now tracked by m.wg (previously it was
	// not, which is exactly the bug this ordering guards against), so Wait
	// genuinely blocks until this permanently-blocked goroutine exits —
	// unblocking it only after Wait already returned would deadlock.
	defer r.Unblock()

	type createOutcome struct {
		res CreateResult
		err error
	}
	outcome := make(chan createOutcome, 1)
	before := fake.PendingCount()
	go func() {
		res, err := m.Create(context.Background(), CreateRequest{
			Items: []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH1", EnableBuffering: true}},
		})
		outcome <- createOutcome{res, err}
	}()

	// Wait for startPush's bounded wait (m.clock.After(PollTimeout)) to
	// register, then advance the fake clock past it — WatchItems is still
	// blocked (Unblock is only deferred, not called yet).
	if !fake.WaitForPending(before+1, 2*time.Second) {
		t.Fatalf("timed out waiting for the WatchItems wait-timeout to register")
	}
	fake.Advance(5 * time.Second)

	var got createOutcome
	select {
	case got = <-outcome:
	case <-time.After(2 * time.Second):
		t.Fatalf("Create did not return after the WatchItems wait timed out")
	}
	if got.err != nil {
		t.Fatalf("Create: %v", got.err)
	}
	if got.res.Handle == "" {
		t.Fatalf("expected a subscription to still be created via poll-mode fallback")
	}

	r.Set(ref, xmlda.NewInt32(2))
	fake.Advance(time.Second) // poll-mode fallback should pick this up

	res2, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{got.res.Handle}})
	if err != nil {
		t.Fatalf("PolledRefresh: %v", err)
	}
	if len(res2.Subscriptions) != 1 {
		t.Fatalf("expected the poll-mode fallback to have picked up the change after WatchItems timed out, got %+v", res2.Subscriptions)
	}
}

// uncleanCloseReader is a backend.ChangeNotifier whose WatchItems channel
// is closed directly by the test (via CloseUnclean), modeling a backend
// that closes its channel for its own reasons rather than in response to
// ctx cancellation — unlike pushReader, its WatchItems does not also
// spawn a goroutine to close the channel on ctx.Done(), so there is no
// risk of a double-close.
type uncleanCloseReader struct {
	*fakeReader
	ch chan backend.ChangeEvent
}

func (r *uncleanCloseReader) WatchItems(ctx context.Context, items []backend.WatchRequest) (<-chan backend.ChangeEvent, error) {
	r.ch = make(chan backend.ChangeEvent, 4)
	return r.ch, nil
}

func (r *uncleanCloseReader) CloseUnclean() { close(r.ch) }

// TestPush_UncleanChannelClose_FallsBackToPoll reproduces a push-mode
// backend that closes its ChangeEvent channel for a reason other than
// the subscription's own context being cancelled (e.g. its connection to
// the underlying data source dropped). Before this was fixed, drainPush
// treated any channel close identically to intentional cancellation and
// exited silently — the subscription stayed registered and valid but
// would never receive another update, with no log line and no attempt to
// fall back to polling.
func TestPush_UncleanChannelClose_FallsBackToPoll(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := &uncleanCloseReader{fakeReader: newFakeReader()}
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(r, fake, Config{ReapInterval: time.Hour, DefaultSamplingRate: time.Second})
	defer shutdownManager(t, m)

	res, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH1", EnableBuffering: true}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A pending timer already exists for the reaper's own chain
	// (ReapInterval); wait for one *more* pending timer to appear —
	// schedulePoll's fallback registration — rather than assuming an
	// absolute count.
	before := fake.PendingCount()
	r.CloseUnclean()
	// Real-time synchronization with drainPush's genuine background
	// goroutine observing the close, not simulated protocol timing.
	if !fake.WaitForPending(before+1, 2*time.Second) {
		t.Fatalf("timed out waiting for drainPush to fall back to poll-mode scheduling after an unclean channel close")
	}

	r.Set(ref, xmlda.NewInt32(2))
	fake.Advance(time.Second) // poll-mode fallback should pick this up

	res2, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{res.Handle}})
	if err != nil {
		t.Fatalf("PolledRefresh: %v", err)
	}
	if len(res2.Subscriptions) != 1 {
		t.Fatalf("expected the poll-mode fallback to have picked up the change after the unclean channel close, got %+v", res2.Subscriptions)
	}
}

type failingWatchReader struct {
	*fakeReader
}

func (f *failingWatchReader) WatchItems(ctx context.Context, items []backend.WatchRequest) (<-chan backend.ChangeEvent, error) {
	return nil, errWatchUnavailable
}

var errWatchUnavailable = &watchError{}

type watchError struct{}

func (*watchError) Error() string { return "watch unavailable in this test" }
