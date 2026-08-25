package subscription

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock"
	"github.com/dernate/gopcxmlda-server/clock/clocktest"
	"github.com/dernate/gopcxmlda-server/telemetry"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// waitForGoroutineCount polls runtime.NumGoroutine() until it is <= want,
// or timeout elapses. This is this project's lightweight, dependency-free
// stand-in for a goroutine-leak check (see docs/development/tasks.md WP-8
// — no external leak-detection library is added; this project's own
// concurrency primitives are all bounded and self-cleaning by
// construction, and this check verifies that empirically).
func waitForGoroutineCount(t *testing.T, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		n := runtime.NumGoroutine()
		if n <= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine count did not settle: got %d, want <= %d", n, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestShutdown_NoGoroutineLeak_MixedPollAndPush(t *testing.T) {
	baseline := runtime.NumGoroutine()

	fake := clocktest.New(testEpoch)
	pr := newPushReader()
	for i := range 5 {
		pr.Set(backend.ItemRef{ItemName: "PollItem"}, xmlda.NewInt32(int32(i)))
	}
	m := newTestManager(pr, fake, Config{ReapInterval: time.Hour, DefaultSamplingRate: time.Hour})

	// Several push-mode subscriptions (this backend implements
	// ChangeNotifier, so every subscription uses push mode) — each costs
	// exactly one drain goroutine, the documented, accepted cost.
	var handles []Handle
	for i := range 5 {
		ref := backend.ItemRef{ItemName: "Item"}
		pr.Set(ref, xmlda.NewInt32(int32(i)))
		res, err := m.Create(context.Background(), CreateRequest{
			Items: []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH"}},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		handles = append(handles, res.Handle)
	}

	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// Every drain goroutine, plus any transient fan-in goroutines from
	// PolledRefresh calls (none outstanding here), must have exited.
	waitForGoroutineCount(t, baseline+2, 2*time.Second) // +2 slack for GC/runtime bookkeeping goroutines
}

// blockingReader lets its first Read call through immediately (Create's
// own validating call), then blocks every subsequent call until release
// is closed — modeling a poll callback stuck inside a slow backend call
// under the real clock (clocktest.Fake's AfterFunc callbacks run
// synchronously on the calling goroutine, so this specific scenario — a
// genuine background goroutine still executing when Shutdown/Wait is
// called — cannot be reproduced with the fake clock at all).
type blockingReader struct {
	*fakeReader
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
}

func (b *blockingReader) Read(ctx context.Context, items []backend.ReadRequestItem) ([]backend.Result[backend.ItemSample], error) {
	b.mu.Lock()
	b.calls++
	first := b.calls == 1
	b.mu.Unlock()
	if first {
		return b.fakeReader.Read(ctx, items)
	}
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-b.release
	return b.fakeReader.Read(ctx, items)
}

// TestManagerWait_BlocksForInFlightPollCallback reproduces the gap where
// Manager.Wait's own doc comment claims it "blocks until every background
// goroutine this Manager started has exited" — but, before this was
// fixed, m.wg only ever tracked push-mode drain goroutines, never
// poll-mode callback goroutines (a genuine, independent goroutine per
// firing under the real clock). A poll callback blocked inside a slow
// backend call at the moment Shutdown/Wait is invoked must still be
// waited for, not silently ignored.
func TestManagerWait_BlocksForInFlightPollCallback(t *testing.T) {
	r := &blockingReader{fakeReader: newFakeReader(), started: make(chan struct{}, 1), release: make(chan struct{})}
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	m := NewManager(backend.Backend{Reader: r}, clock.Real{}, telemetry.NoopLogger(), telemetry.NoopMetrics(), Config{
		ReapInterval:        time.Hour,
		DefaultSamplingRate: time.Millisecond,
		PollTimeout:         10 * time.Second,
	})

	if _, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH1"}},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	select {
	case <-r.started:
	case <-time.After(2 * time.Second):
		t.Fatalf("poll callback never started its (blocking) backend Read call")
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- m.Shutdown(context.Background()) }()

	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned (err=%v) before the in-flight poll callback finished — Manager.Wait did not actually wait for it", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(r.release) // let the blocked poll callback's Read call return

	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Shutdown did not return within 2s after the blocked poll callback was released")
	}
}

// TestManagerWait_BlocksForInFlightWatchItemsCall reproduces the gap where
// startPush's initial cn.WatchItems call ran in a bare "go func(){...}()",
// outside m.wg — unlike drainPush and pollOnceBounded's goroutines, in
// flight time on this one was invisible to Manager.Wait. A backend slower
// than Config.PollTimeout leaves it running after startPush has already
// fallen back to poll-mode; before this was fixed, Shutdown/Wait could
// return while that call into third-party backend code was still in
// flight, silently violating Wait's own "every background goroutine has
// exited" doc comment.
func TestManagerWait_BlocksForInFlightWatchItemsCall(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newBlockingWatchReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(r, fake, Config{ReapInterval: time.Hour, DefaultSamplingRate: time.Second, PollTimeout: 5 * time.Second})

	before := fake.PendingCount()
	createDone := make(chan error, 1)
	go func() {
		_, err := m.Create(context.Background(), CreateRequest{
			Items: []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH1"}},
		})
		createDone <- err
	}()

	// Wait for startPush's bounded wait (m.clock.After(PollTimeout)) to
	// register, then advance the fake clock past it so Create returns via
	// the poll-mode fallback — WatchItems is still blocked (Unblock has not
	// been called yet), leaving its goroutine in flight.
	if !fake.WaitForPending(before+1, 2*time.Second) {
		t.Fatalf("timed out waiting for the WatchItems wait-timeout to register")
	}
	fake.Advance(5 * time.Second)
	if err := <-createDone; err != nil {
		t.Fatalf("Create: %v", err)
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- m.Shutdown(context.Background()) }()

	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned (err=%v) before the in-flight WatchItems call finished — Manager.Wait did not actually wait for it", err)
	case <-time.After(200 * time.Millisecond):
	}

	r.Unblock() // let the blocked WatchItems call return

	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Shutdown did not return within 2s after the blocked WatchItems call was released")
	}
}

func TestShutdown_Idempotent(t *testing.T) {
	fake := clocktest.New(testEpoch)
	m := newTestManager(newFakeReader(), fake, Config{})
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown (idempotent) should not error: %v", err)
	}
}

func TestShutdown_StopsPollScheduling(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(r, fake, Config{ReapInterval: time.Hour, DefaultSamplingRate: time.Second})

	if _, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH1"}},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	countBefore := r.ReadCount()
	fake.Advance(10 * time.Second) // would have fired several more polls if scheduling weren't stopped
	if got := r.ReadCount(); got != countBefore {
		t.Fatalf("expected no further polls after Shutdown, got %d more reads", got-countBefore)
	}
}
