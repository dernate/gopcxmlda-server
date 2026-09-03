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
	for i := range 5 {
		ref := backend.ItemRef{ItemName: "Item"}
		pr.Set(ref, xmlda.NewInt32(int32(i)))
		if _, err := m.Create(context.Background(), CreateRequest{
			Items: []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH"}},
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
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

// TestCancel_StopsPendingPollTimer reproduces the gap where terminating a
// subscription cancelled its context but left its already-armed poll
// timer running: an unstopped clock.Clock.AfterFunc timer keeps its
// closure — and through it the whole subState and every item's buffered
// data — reachable until it eventually fires, which for a slow sampling
// rate can be long after the subscription is already gone. Cancel must
// stop the timer immediately, which fake.PendingCount() can verify
// directly instead of only observing its downstream effects.
func TestCancel_StopsPendingPollTimer(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	m := newTestManager(r, fake, Config{ReapInterval: time.Hour, DefaultSamplingRate: time.Second})
	defer shutdownManager(t, m)

	before := fake.PendingCount()
	res, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH1"}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := fake.PendingCount(); got != before+1 {
		t.Fatalf("got %d pending timers after Create, want %d (the poll timer)", got, before+1)
	}

	if !m.Cancel(res.Handle) {
		t.Fatalf("expected Cancel to report found=true")
	}
	if got := fake.PendingCount(); got != before {
		t.Fatalf("got %d pending timers after Cancel, want %d — the poll timer was left running, keeping the cancelled subscription reachable until it eventually fires", got, before)
	}
}

// TestBeginShutdown_StopsAllPendingTimers is the manager-wide analogue:
// BeginShutdown must stop every subscription's poll timer plus the
// reaper's own timer, not just cancel their contexts, for the same
// leaked-closure reason as TestCancel_StopsPendingPollTimer.
func TestBeginShutdown_StopsAllPendingTimers(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	refs := []backend.ItemRef{{ItemName: "Item1"}, {ItemName: "Item2"}}
	for _, ref := range refs {
		r.Set(ref, xmlda.NewInt32(1))
	}
	m := newTestManager(r, fake, Config{ReapInterval: time.Hour, DefaultSamplingRate: time.Second})
	defer shutdownManager(t, m)

	for _, ref := range refs {
		if _, err := m.Create(context.Background(), CreateRequest{
			Items: []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH"}},
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	if got := fake.PendingCount(); got == 0 {
		t.Fatalf("expected at least the reap timer plus two poll timers pending before shutdown, got 0")
	}

	m.BeginShutdown()

	if got := fake.PendingCount(); got != 0 {
		t.Fatalf("got %d pending timers after BeginShutdown, want 0", got)
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

// // --- WaitGroup Add must not race Wait ---

// TestShutdown_ConcurrentCreate_NoWaitGroupRace pins the fix for the shutdown race
// the race detector found.
//
// Create released m.mu after inserting the subscription and only then
// called startRefreshing, which arms the poll timer and so takes a
// WaitGroup slot. A BeginShutdown+Wait that won the mutex in between
// could therefore observe a zero counter, return — and race that Add.
// sync.WaitGroup forbids exactly that ("Add called concurrently with
// Wait") and can panic on it; short of a panic, Wait returned while a
// poll chain was still being armed, breaking the one guarantee
// Manager.Wait exists to provide.
//
// Run under -race, this is the test that reproduced it. It also asserts
// Shutdown itself never reports an error, which is what a leaked
// background goroutine would show up as.
func TestShutdown_ConcurrentCreate_NoWaitGroupRace(t *testing.T) {
	iterations := 400
	if testing.Short() {
		iterations = 50
	}
	for i := range iterations {
		r := newFakeReader()
		r.Set(backend.ItemRef{ItemName: "A"}, xmlda.NewInt32(1))
		// A live clock and a very short sampling rate, so the poll chain
		// really does arm and fire inside the race window.
		m := NewManager(backend.Backend{Reader: r}, nil, nil, nil, Config{
			ReapInterval:        time.Hour,
			DefaultSamplingRate: time.Millisecond,
		})

		var wg sync.WaitGroup
		wg.Add(2)
		start := make(chan struct{})
		go func() {
			defer wg.Done()
			<-start
			_, _ = m.Create(context.Background(), CreateRequest{
				Items: []CreateItemRequest{{Ref: backend.ItemRef{ItemName: "A"}}},
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := m.Shutdown(ctx); err != nil {
				t.Errorf("iteration %d: Shutdown: %v", i, err)
			}
		}()
		close(start)
		wg.Wait()
	}
	runtime.GC()
}

// TestShutdown_ConcurrentCreate_PushMode_NoWaitGroupRace is the
// push-mode half of the test above, and it is the half that was missing.
// startPush starts its own background goroutines, and with newFakeReader
// (no ChangeNotifier) the poll path is taken instead, so push.go was
// never reached by any concurrency test at all — its two goroutines took
// their WaitGroup slot with a bare wg.Add, outside the m.mu/rootCtx gate
// armTimer uses, which the race detector reports and sync.WaitGroup can
// panic on ("Add called concurrently with Wait", or "WaitGroup is reused
// before previous Wait has returned").
func TestShutdown_ConcurrentCreate_PushMode_NoWaitGroupRace(t *testing.T) {
	iterations := 400
	if testing.Short() {
		iterations = 50
	}
	for i := range iterations {
		r := newPushReader()
		r.Set(backend.ItemRef{ItemName: "A"}, xmlda.NewInt32(1))
		m := NewManager(backend.Backend{Reader: r}, nil, nil, nil, Config{
			ReapInterval:        time.Hour,
			DefaultSamplingRate: time.Millisecond,
		})

		var wg sync.WaitGroup
		wg.Add(2)
		start := make(chan struct{})
		go func() {
			defer wg.Done()
			<-start
			_, _ = m.Create(context.Background(), CreateRequest{
				Items: []CreateItemRequest{{Ref: backend.ItemRef{ItemName: "A"}}},
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := m.Shutdown(ctx); err != nil {
				t.Errorf("iteration %d: Shutdown: %v", i, err)
			}
		}()
		close(start)
		wg.Wait()
	}
	runtime.GC()
}

// TestGoTracked_DeclinesAfterShutdown pins goTracked's half of the
// mechanism, exactly as TestArmTimer_DeclinesAfterShutdown pins
// armTimer's: once BeginShutdown has run, no further WaitGroup slot is
// taken and nothing is started.
func TestGoTracked_DeclinesAfterShutdown(t *testing.T) {
	m := NewManager(backend.Backend{Reader: newFakeReader()}, nil, nil, nil, Config{ReapInterval: time.Hour})
	m.BeginShutdown()

	ran := make(chan struct{})
	if m.goTracked(func() { close(ran) }) {
		t.Fatal("goTracked reported it started a goroutine after BeginShutdown")
	}
	select {
	case <-ran:
		t.Fatal("goTracked ran f after BeginShutdown")
	default:
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.Wait(ctx); err != nil {
		t.Fatalf("Wait after a declined goTracked: %v", err)
	}
}

// TestArmTimer_DeclinesAfterShutdown pins the mechanism directly: once
// BeginShutdown has run, armTimer takes no further WaitGroup slot and
// reports that it armed nothing. That is what makes the Add/Wait pairing
// safe rather than merely unlikely to collide.
func TestArmTimer_DeclinesAfterShutdown(t *testing.T) {
	m := NewManager(backend.Backend{Reader: newFakeReader()}, nil, nil, nil, Config{ReapInterval: time.Hour})
	m.BeginShutdown()

	fired := make(chan struct{}, 1)
	timer, armed := m.armTimer(time.Millisecond, func() { fired <- struct{}{} })
	if armed {
		t.Error("armTimer armed a timer after BeginShutdown")
		timer.Stop()
	}
	if timer != nil {
		t.Errorf("armTimer returned a non-nil timer (%T) while declining", timer)
	}
	// Wait must return promptly: nothing was added that it could block on.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.Wait(ctx); err != nil {
		t.Errorf("Wait after a declined arm: %v", err)
	}
	select {
	case <-fired:
		t.Error("the declined timer's callback ran anyway")
	case <-time.After(20 * time.Millisecond):
	}
}

// TestSchedulePoll_AfterShutdownIsNoOp pins that the callers handle the
// declined arm rather than dereferencing a nil timer — the failure mode a
// bare `return nil` signal invites.
func TestSchedulePoll_AfterShutdownIsNoOp(t *testing.T) {
	r := newFakeReader()
	r.Set(backend.ItemRef{ItemName: "A"}, xmlda.NewInt32(1))
	m := NewManager(backend.Backend{Reader: r}, nil, nil, nil, Config{ReapInterval: time.Hour})

	res, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{{Ref: backend.ItemRef{ItemName: "A"}}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	m.mu.RLock()
	s := m.subs[res.Handle]
	m.mu.RUnlock()
	if s == nil {
		t.Fatal("subscription not registered")
	}

	m.BeginShutdown()
	// Would panic on a nil timer if schedulePoll did not check.
	m.schedulePoll(s, time.Millisecond)
	m.scheduleReap()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.Wait(ctx); err != nil {
		t.Errorf("Wait: %v", err)
	}
}
