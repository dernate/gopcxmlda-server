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
}

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

	cfg     Config
	clock   clock.Clock
	backend backend.Backend
	log     telemetry.Logger
	metrics telemetry.Metrics

	rootCtx    context.Context
	rootCancel context.CancelFunc
	wg         sync.WaitGroup

	pollSem chan struct{}

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
	}
	m.startReaper()
	return m
}

// update is one recorded change to a subscribed item: either a new
// sample (ResultID zero, HaveSample true) or an abnormal per-item
// condition reported by the backend while the subscription was live
// (ResultID non-zero, HaveSample false — e.g. the item vanished from the
// address space, or its watch broke).
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

	mu sync.Mutex
	// haveLast reports whether last holds a real sample. It stays true
	// (and last keeps the last good sample) across an abnormal condition,
	// so recovering from one is evaluated against the last value the
	// client actually saw rather than against a synthetic blank.
	haveLast bool
	last     backend.ItemSample
	// lastResultID is the item's currently-reported condition, the zero
	// ErrorCode while it is healthy. Compared against each new poll/push
	// outcome so a persistent failure is reported once rather than on
	// every tick.
	lastResultID xmlda.ErrorCode
	buffer       []update // pending, undelivered changes; oldest first
	overflowed   bool
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

// count returns the current number of active subscriptions.
func (m *Manager) count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.subs)
}

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
	}
	m.mu.Unlock()
	if !ok {
		return false
	}
	s.cancel()
	s.stopPolling()
	return true
}
