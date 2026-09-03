// Package subscription implements the OPC XML-DA subscription lifecycle:
// Subscribe/SubscriptionPolledRefresh/SubscriptionCancel's shared engine.
// It has no knowledge of HTTP or XML encoding — only of backend and OPC
// XML-DA vocabulary types — see docs/architecture/subscription-model.md
// for the full design this package implements.
package subscription

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock"
	"github.com/dernate/gopcxmlda-server/telemetry"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// Handle is an opaque, server-issued subscription identifier
// (ServerSubHandle on the wire).
type Handle string

// Config holds Manager's tunable limits, all implementation defaults —
// the specification mandates none of these numbers (ADR-011).
type Config struct {
	// MaxConcurrentSubscriptions caps how many subscriptions may exist at
	// once; 0 means unlimited.
	MaxConcurrentSubscriptions int
	// MaxConcurrentPolls bounds how many poll-mode backend calls may run
	// at once, regardless of how many subscriptions are due to poll at
	// the same instant.
	MaxConcurrentPolls int
	// ReapInterval is how often the abandonment reaper sweeps.
	ReapInterval time.Duration
	// ReapGraceMultiplier scales a subscription's ping rate to compute
	// its abandonment grace period (grace = pingRate * multiplier).
	ReapGraceMultiplier float64
	// DefaultSubscriptionPingRate is substituted whenever a client sends
	// SubscriptionPingRate=0 (REQ-SUBSCRIPTION-015, OQ-10).
	DefaultSubscriptionPingRate time.Duration
	// DefaultSamplingRate is substituted whenever a client requests
	// RequestedSamplingRate=0 ("fastest practical") for a poll-mode item.
	DefaultSamplingRate time.Duration
	// MaxBufferedSamplesPerItem bounds how many buffered (undelivered)
	// changes one item may accumulate before the oldest are purged
	// (REQ-SUBSCRIPTION-007). The Latest Changed Value is always
	// retained regardless of this limit.
	MaxBufferedSamplesPerItem int
	// PollTimeout bounds each individual poll-mode backend.Reader.Read
	// call.
	PollTimeout time.Duration
	// MaxTotalSubscribedItems caps the number of subscribed items across
	// all subscriptions at once; <= 0 means unlimited. It exists because
	// the per-axis limits multiply: MaxConcurrentSubscriptions and the
	// server's per-subscription item cap together permit a live item
	// count neither limit alone suggests, and every item holds its own
	// last sample plus up to MaxBufferedSamplesPerItem buffered ones.
	MaxTotalSubscribedItems int
	// MaxTotalBufferedSamples caps the number of buffered (undelivered)
	// samples held across every subscribed item at once; <= 0 means
	// unlimited. It exists for the same reason MaxTotalSubscribedItems
	// does, one axis further out: MaxBufferedSamplesPerItem bounds one
	// item, and the product of the two bounds nothing anybody configured.
	// On exhaustion a buffering item keeps only its Latest Changed Value
	// (always retained, REQ-SUBSCRIPTION-007) and reports the loss through
	// DataBufferOverflow.
	MaxTotalBufferedSamples int
}

// sampleBudget is the server-wide accounting for buffered samples
// (Config.MaxTotalBufferedSamples).
//
// It counts entries held in the buffers of items that have
// EnableBuffering set — and only those. A non-buffering item's single
// latest-value slot is deliberately outside the budget: it is bounded by
// the item count, which MaxTotalSubscribedItems already governs, and
// making it compete for this budget would let buffered history starve the
// plain change delivery every subscription depends on.
type sampleBudget struct {
	max int64 // <= 0: unlimited
	n   atomic.Int64
}

// acquire takes one slot, reporting false if the budget is exhausted.
func (b *sampleBudget) acquire() bool {
	if b == nil || b.max <= 0 {
		return true
	}
	if b.n.Add(1) > b.max {
		b.n.Add(-1)
		return false
	}
	return true
}

// add takes n slots unconditionally, past the budget if necessary. Used
// only for the one entry per item that is never given up: an item must be
// able to report its Latest Changed Value even under a full budget, and
// that floor is bounded by the item count rather than by this budget.
func (b *sampleBudget) add(n int64) {
	if b == nil || b.max <= 0 || n == 0 {
		return
	}
	b.n.Add(n)
}

// release returns n slots.
func (b *sampleBudget) release(n int64) {
	if b == nil || b.max <= 0 || n <= 0 {
		return
	}
	b.n.Add(-n)
}

// count reports the slots currently held. Test/diagnostic use only.
func (b *sampleBudget) count() int64 {
	if b == nil {
		return 0
	}
	return b.n.Load()
}

// WithDefaults returns a copy of c with every unset field replaced by its
// built-in default. Exported so server.Config — which forwards these
// fields — can resolve them from the one place their real values live,
// rather than restating the numbers and letting the two copies drift.
func (c Config) WithDefaults() Config { return c.withDefaults() }

func (c Config) withDefaults() Config {
	if c.MaxConcurrentPolls <= 0 {
		c.MaxConcurrentPolls = 32
	}
	if c.ReapInterval <= 0 {
		c.ReapInterval = 10 * time.Second
	}
	if c.ReapGraceMultiplier <= 0 {
		c.ReapGraceMultiplier = 2.0
	}
	if c.DefaultSubscriptionPingRate <= 0 {
		c.DefaultSubscriptionPingRate = 60 * time.Second
	}
	if c.DefaultSamplingRate <= 0 {
		c.DefaultSamplingRate = time.Second
	}
	if c.MaxBufferedSamplesPerItem <= 0 {
		c.MaxBufferedSamplesPerItem = 100
	}
	if c.PollTimeout <= 0 {
		c.PollTimeout = 30 * time.Second
	}
	return c
}

// Manager owns every active subscription's state: storage, poll/push
// scheduling, Hold+Wait blocking, buffering, and the abandonment reaper.
// See docs/architecture/subscription-model.md.
type Manager struct {
	mu   sync.RWMutex
	subs map[Handle]*subState
	// totalItems is the number of subscribed items across every entry in
	// subs, maintained by Create and terminate so the server-wide item
	// budget needs no map scan under the lock. Guarded by mu.
	totalItems int

	cfg     Config
	clock   clock.Clock
	backend backend.Backend
	log     telemetry.Logger
	metrics telemetry.Metrics

	rootCtx    context.Context
	rootCancel context.CancelFunc
	wg         sync.WaitGroup

	pollSem chan struct{}

	// budget bounds buffered samples across every subscription
	// (Config.MaxTotalBufferedSamples).
	budget *sampleBudget

	// reapTimer is the pending abandonment-reaper timer, guarded by mu and
	// stopped by BeginShutdown (see reaper.go).
	reapTimer clock.Timer

	shutdownOnce sync.Once
}

// NewManager constructs a Manager. clk, log, and metrics may be nil, in
// which case clock.Real{}, telemetry.NoopLogger(), and
// telemetry.NoopMetrics() are used respectively.
func NewManager(be backend.Backend, clk clock.Clock, log telemetry.Logger, metrics telemetry.Metrics, cfg Config) *Manager {
	cfg = cfg.withDefaults()
	if clk == nil {
		clk = clock.Real{}
	}
	if log == nil {
		log = telemetry.NoopLogger()
	}
	if metrics == nil {
		metrics = telemetry.NoopMetrics()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		subs:       make(map[Handle]*subState),
		cfg:        cfg,
		clock:      clk,
		backend:    be,
		log:        log,
		metrics:    metrics,
		rootCtx:    ctx,
		rootCancel: cancel,
		pollSem:    make(chan struct{}, cfg.MaxConcurrentPolls),
		budget:     &sampleBudget{max: int64(cfg.MaxTotalBufferedSamples)},
	}
	m.startReaper()
	return m
}

// update is one recorded change to a subscribed item: either a new
// sample (haveSample true, resultID zero or a non-critical S_ code that
// accompanies it) or a critical per-item condition reported by the
// backend while the subscription was live (resultID an E_ code,
// haveSample false — e.g. the item vanished from the address space, or
// its watch broke).
//
// Carrying the condition alongside the sample is what keeps a failing
// item from being reported to the client as a Good-quality change: a
// backend's per-item ResultID is a first-class outcome of every refresh,
// exactly as it is for Read (docs/backend-implementation.md), not
// something the subscription engine may discard.
type update struct {
	sample     backend.ItemSample
	resultID   xmlda.ErrorCode
	haveSample bool
	// diagnosticInfo is the backend's per-item diagnostic text for this
	// outcome, kept so RequestOptions.ReturnDiagnosticInfo can be honored
	// on SubscriptionPolledRefresh exactly as it is on Read and Write.
	diagnosticInfo string
}

// itemState is one subscribed item's mutable state, private to the
// subState that owns it.
type itemState struct {
	ref                   backend.ItemRef
	clientItemHandle      string
	requestedSamplingRate time.Duration
	revisedSamplingRate   time.Duration
	deadband              float64 // 0-100%, analog/array types only
	enableBuffering       bool
	// reqType is the client's requested value type, carried through
	// untouched so every result can report it back to the server layer,
	// which is where coercion happens.
	reqType *xmlda.QName

	mu sync.Mutex
	// haveLast reports whether last holds a real sample. It stays true
	// (and last keeps the last good sample) across an abnormal condition,
	// so recovering from one is evaluated against the last value the
	// client actually saw rather than against a synthetic blank.
	haveLast bool
	last     backend.ItemSample
	// lastDiagnosticInfo accompanies lastResultID, so a ReturnAllItems
	// reply can report the condition with the same detail a change reply
	// carries.
	lastDiagnosticInfo string
	// lastResultID is the item's currently-reported condition, the zero
	// ErrorCode while it is healthy. Compared against each new poll/push
	// outcome so a persistent critical failure is reported once rather
	// than on every tick; a non-critical S_ code does not suppress the
	// value it accompanies (see applyUpdate).
	lastResultID xmlda.ErrorCode
	buffer       []update // pending, undelivered changes; oldest first
	overflowed   bool
	// released reports that this item's subscription has been terminated
	// and its buffered samples already returned to the server-wide budget
	// (releaseBuffers). An applyUpdate whose s.ctx check passed just
	// before the cancellation would otherwise acquire budget slots
	// afterwards and write them into a buffer nobody will ever drain —
	// leaking those slots permanently, until the budget refuses buffering
	// to live subscriptions on behalf of dead ones.
	released bool
	// lastPolledAt is the last time THIS item was actually read from the
	// backend in poll mode — distinct from subState.lastPolledAt (which
	// tracks client PolledRefresh calls for the reaper). Zero until the
	// first poll. Used by pollOnce to honor each item's own
	// revisedSamplingRate within the subscription's single shared timer
	// chain (ADR-008) instead of reading/evaluating every item at
	// whichever item happens to have the fastest rate.
	lastPolledAt time.Time
}

// subState is one subscription's full state.
type subState struct {
	handle Handle
	mgr    *Manager

	ctx    context.Context
	cancel context.CancelFunc

	returnValuesOnReply bool

	mu           sync.Mutex
	items        []*itemState
	pingRate     time.Duration
	lastPolledAt time.Time
	changedCh    chan struct{}
	// timer is the pending poll-mode timer, stopped on cancellation so a
	// terminated subscription is not kept reachable until its next tick
	// (see schedulePoll/stopPolling in poll.go). nil in push mode.
	timer clock.Timer

	busyFlag int32 // accessed via sync/atomic; guards E_BUSY (REQ-SUBSCRIPTION-009)
}

func newHandle() Handle {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read on the standard reader does not fail in
		// practice on any supported platform; a panic here would only
		// ever fire under a broken OS entropy source, which is not a
		// condition this library can recover from meaningfully.
		panic("subscription: crypto/rand.Read failed: " + err.Error())
	}
	return Handle(hex.EncodeToString(b[:]))
}

// notifyChanged wakes every current SubscriptionPolledRefresh call
// blocked waiting on this subscription's changedCh.
func (s *subState) notifyChanged() {
	s.mu.Lock()
	old := s.changedCh
	s.changedCh = make(chan struct{})
	s.mu.Unlock()
	close(old)
}

func (s *subState) changedChan() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changedCh
}

func (s *subState) touchPolledAt(now time.Time) {
	s.mu.Lock()
	s.lastPolledAt = now
	s.mu.Unlock()
}

// isBusy reports whether a SubscriptionPolledRefresh call currently holds
// this subscription (REQ-SUBSCRIPTION-009's busy flag). Read by the
// abandonment reaper, which must never terminate a subscription a client
// is actively polling — see reapOnce.
func (s *subState) isBusy() bool {
	return atomic.LoadInt32(&s.busyFlag) != 0
}

// Shutdown cancels every subscription's context (unblocking any in-flight
// PolledRefresh call and halting poll/push scheduling immediately) and
// then waits for background goroutines to exit, bounded by ctx.
//
// Callers using this alongside an HTTP server must call the cancellation
// step before http.Server.Shutdown so in-flight long-poll handlers are
// already unblocked before the HTTP server starts waiting for them — see
// docs/architecture/subscription-model.md.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.BeginShutdown()
	return m.Wait(ctx)
}

// BeginShutdown cancels every subscription's context without waiting for
// background goroutines to exit. Idempotent.
//
// Cancellation is explicit per subscription rather than by propagation
// from m.rootCtx: a subscription's context carries the values of the
// Subscribe request that created it (context.WithoutCancel — see Create),
// so it has no cancellable ancestor here. m.rootCtx still gates whether
// NEW subscriptions and timers may be created at all, which is what the
// check under this same mutex is for.
//
// rootCancel is called while holding m.mu — the same mutex Create's
// atomic shutdown-check-and-insert holds (see create.go) — so a Create
// call and a BeginShutdown call can never interleave: whichever
// acquires m.mu first fully completes (either registering a normal,
// tracked subscription, or cancelling before any subscription created
// afterward can be inserted) before the other proceeds. cancel() itself
// only closes channels and propagates to already-derived child contexts
// synchronously; it never calls back into application code that might
// try to reacquire m.mu, so this is not a deadlock risk.
func (m *Manager) BeginShutdown() {
	m.shutdownOnce.Do(func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.rootCancel()
		// Cancelling the contexts stops the poll/reap chains from
		// *rescheduling*, but an already-armed timer still holds its
		// closure (and everything it captures) until it fires. Stop them
		// explicitly so a shut-down Manager releases its subscriptions
		// immediately rather than at the next tick.
		if m.reapTimer != nil {
			m.reapTimer.Stop()
			m.reapTimer = nil
		}
		for _, s := range m.subs {
			// Cancelled explicitly rather than by propagation: a
			// subscription's context is derived from the Subscribe
			// request's values (see Create), so m.rootCtx is no longer
			// its ancestor and cancelling the root no longer reaches it.
			s.cancel()
			s.stopPolling()
		}
	})
}

// Wait blocks until every background goroutine this Manager started has
// exited, or ctx is done first.
func (m *Manager) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Count returns the current number of active subscriptions. Exported
// because it is the one number an embedding application needs for a
// health or diagnostics endpoint, and because a test outside this package
// otherwise has no way to assert that a subscription was cleaned up.
func (m *Manager) Count() int { return m.count() }

// count returns the current number of active subscriptions.
func (m *Manager) count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.subs)
}

// totalItemsLocked returns the number of subscribed items across every
// live subscription. m.mu must be held.
//
// It reads a counter maintained by the two places that change the set of
// live subscriptions (Create's insert and terminate's delete) rather than
// summing the map. Each subscription's item slice is fixed at Create time
// and never changes afterwards (OPC XML-DA has no "add item to existing
// subscription" operation), so a counter cannot drift from the map.
//
// Summing the map was correct but ran under the global write lock on
// every single Subscribe: at the default ceiling of 10 000 subscriptions
// that is a 10 000-entry scan blocking every other subscription
// operation, for a number the manager can just as well carry along.
func (m *Manager) totalItemsLocked() int { return m.totalItems }

// armTimer arms a clock.Clock.AfterFunc timer whose callback is tracked
// by m.wg for its whole lifetime — from the moment it is armed, not from
// the moment it fires.
//
// sync.WaitGroup requires that a positive Add which takes the counter off
// zero happens before Wait. Calling Add as the callback's first statement
// does not satisfy that: between "the timer fired and its goroutine was
// scheduled" and "the callback executed its first line" the counter is
// still zero, so a concurrent Wait can return while a backend call is
// about to start. Timer.Stop does not close the window either — it
// reports false precisely when the callback has already begun.
//
// Counting from arming closes it. The returned timer's Stop releases the
// counter when, and only when, it actually prevented the callback from
// running; otherwise the callback releases it on the way out. Exactly one
// of the two happens, and sync.Once makes a double release impossible
// even under a racing Stop.
//
// Self-rescheduling chains (schedulePoll, scheduleReap) arm the next
// timer from inside the current callback, before its deferred release
// runs, so the counter never dips to zero between links.
//
// The Add itself happens under m.mu, together with a shutdown check, and
// that pairing is what makes the whole scheme safe rather than merely
// well-intentioned. sync.WaitGroup forbids an Add that takes the counter
// off zero from racing a Wait, and Create used to be able to do exactly
// that: it releases m.mu after inserting the subscription and only then
// calls startRefreshing, so a BeginShutdown+Wait that won the mutex in
// between could observe a zero counter, return — and race this Add. The
// race detector reported it, and sync.WaitGroup itself can panic on it
// ("Add called concurrently with Wait"). BeginShutdown calls rootCancel
// while holding this same mutex, so once shutdown has begun no further
// Add can pass the check below, and Wait has nothing left to race.
//
// The returned Timer is nil, and ok false, when shutdown has already
// begun: there is no point arming a timer whose callback would only
// observe a cancelled context and return.
func (m *Manager) armTimer(d time.Duration, f func()) (clock.Timer, bool) {
	m.mu.Lock()
	if m.rootCtx.Err() != nil {
		m.mu.Unlock()
		return nil, false
	}
	m.wg.Add(1)
	m.mu.Unlock()

	var once sync.Once
	release := func() { once.Do(m.wg.Done) }
	t := m.clock.AfterFunc(d, func() {
		defer release()
		f()
	})
	return &trackedTimer{inner: t, release: release}, true
}

// goTracked starts f on a background goroutine tracked by m.wg, taking
// the WaitGroup slot under m.mu together with the shutdown check — the
// same pairing armTimer relies on, and for exactly the same reason.
//
// sync.WaitGroup forbids a positive Add that takes the counter off zero
// from racing a Wait. Create releases m.mu after inserting the
// subscription and only then calls startRefreshing, so a
// BeginShutdown+Wait that won the mutex in between could otherwise
// observe a zero counter, return — and race the Add that startPush's
// goroutines perform. BeginShutdown calls rootCancel while holding this
// same mutex, so once shutdown has begun no further Add can pass the
// check below, and Wait has nothing left to race.
//
// Reports false, having started nothing, when shutdown has already
// begun: the subscription's own context is cancelled by then, so there
// is no work left for f to do.
func (m *Manager) goTracked(f func()) bool {
	m.mu.Lock()
	if m.rootCtx.Err() != nil {
		m.mu.Unlock()
		return false
	}
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		f()
	}()
	return true
}

// trackedTimer is armTimer's return value: a clock.Timer whose Stop also
// releases the WaitGroup counter armTimer took.
//
// The underlying timer is held in a named field rather than embedded, so
// Reset cannot be inherited by accident: armTimer's accounting is a
// one-shot arm/fire-or-stop pair, and resetting a stopped timer would
// re-arm a callback whose counter has already been released — a
// WaitGroup counter going negative, which panics. Nothing in this
// package resets a timer, and this makes that a compile-time fact
// instead of a comment.
type trackedTimer struct {
	inner   clock.Timer
	release func()
}

// Stop stops the underlying timer, releasing the WaitGroup counter if the
// callback was actually prevented from running.
func (t *trackedTimer) Stop() bool {
	stopped := t.inner.Stop()
	if stopped {
		t.release()
	}
	return stopped
}

// Reset is not supported on a wg-tracked timer; see trackedTimer.
func (t *trackedTimer) Reset(time.Duration) bool {
	panic("subscription: trackedTimer.Reset is not supported — arm a new timer via armTimer instead")
}

// C returns the underlying timer's channel. An AfterFunc timer's channel
// never fires (clock.Timer's own contract); this exists only to satisfy
// the interface.
func (t *trackedTimer) C() <-chan time.Time { return t.inner.C() }

// recoverBackgroundPanic recovers a panic occurring within op, a
// background call that reaches into third-party backend/telemetry code
// this library cannot fully trust (docs/backend-implementation.md) —
// turning what would otherwise be a process-crashing unrecovered panic (no
// net/http per-request recover exists on these bare goroutines) into a
// logged, contained failure. Call as `defer m.recoverBackgroundPanic("op")`
// at the top of the function whose body should be protected.
func (m *Manager) recoverBackgroundPanic(op string) {
	if r := recover(); r != nil {
		m.metrics.IncSubscriptionError("panic")
		m.log.Error("subscription: recovered panic in background call", "op", op, "panic", fmt.Sprint(r))
	}
}

// terminate removes handle from the manager and cancels its context, the
// single mechanism used by Cancel and the reaper alike. It is a no-op
// (not an error) if handle is not found — see REQ-SUBSCRIPTION-014.
func (m *Manager) terminate(handle Handle) bool {
	m.mu.Lock()
	s, ok := m.subs[handle]
	if ok {
		delete(m.subs, handle)
		m.totalItems -= len(s.items)
	}
	m.mu.Unlock()
	if !ok {
		return false
	}
	s.cancel()
	s.stopPolling()
	s.releaseBuffers(m.budget)
	return true
}

// releaseBuffers returns every budgeted buffered sample this subscription
// still holds. Called on every path that removes a subscription — without
// it the server-wide budget leaks by the undelivered backlog of each
// cancelled or reaped subscription, and eventually refuses buffering to
// live ones on behalf of subscriptions that no longer exist.
func (s *subState) releaseBuffers(b *sampleBudget) {
	s.mu.Lock()
	items := append([]*itemState(nil), s.items...)
	s.mu.Unlock()
	for _, it := range items {
		it.mu.Lock()
		if it.enableBuffering {
			b.release(int64(len(it.buffer)))
		}
		it.buffer = nil
		// Set under the same lock that guards the release above, so an
		// applyUpdate racing this cannot slip a budget acquisition in
		// between the two.
		it.released = true
		it.mu.Unlock()
	}
}
