package subscription

import (
	"context"
	"errors"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// ErrTooManySubscriptions is returned by Create when
// Config.MaxConcurrentSubscriptions has been reached — a whole-operation
// SOAP fault at the server layer (E_OUTOFMEMORY is the closest standard
// code; REQ-LIMITS-001, ADR-011: this limit is an implementation policy,
// not a specification requirement).
var ErrTooManySubscriptions = errors.New("subscription: maximum number of concurrent subscriptions reached")

// CreateItemRequest is one item requested in a Create call.
type CreateItemRequest struct {
	Ref                   backend.ItemRef
	ClientItemHandle      string
	RequestedSamplingRate time.Duration // 0 = fastest practical
	Deadband              float64       // 0-100%, analog/array types only
	EnableBuffering       bool
}

// CreateRequest is the input to Manager.Create (the Subscribe operation's
// engine-facing shape).
type CreateRequest struct {
	Items               []CreateItemRequest
	ReturnValuesOnReply bool
	// SubscriptionPingRate is milliseconds-equivalent as a Duration; 0
	// means "use the server's own default" (REQ-SUBSCRIPTION-015).
	SubscriptionPingRate time.Duration
}

// CreateItemResult is one item's outcome from Create.
type CreateItemResult struct {
	ClientItemHandle    string
	RevisedSamplingRate time.Duration
	// ResultID is the zero ErrorCode iff this item was valid and is now
	// part of the subscription.
	ResultID xmlda.ErrorCode
	// Sample and HaveSample are populated iff ReturnValuesOnReply was
	// true and this item was valid.
	Sample     backend.ItemSample
	HaveSample bool
}

// CreateResult is the outcome of Create. Handle is "" iff no item was
// valid — no subscription was created (REQ-SUBSCRIPTION-002).
type CreateResult struct {
	Handle Handle
	Items  []CreateItemResult
}

// Create validates req's items via backend.Reader.Read and, if at least
// one is valid, creates a new subscription and begins refreshing it:
// push-mode if the backend's Reader also implements
// backend.ChangeNotifier, poll-mode otherwise. A non-nil error is a
// whole-operation failure (the initial Read call failed); it is not used
// for per-item conditions, which are reported via each item's ResultID.
func (m *Manager) Create(ctx context.Context, req CreateRequest) (CreateResult, error) {
	// Advisory only, not atomic with the real insert below: this lets an
	// obviously-over-limit request fail fast without an unnecessary
	// backend.Reader.Read call. Two concurrent Create calls can both pass
	// this check before either has inserted — that race is closed by the
	// authoritative re-check under m.mu immediately before the insert.
	if m.cfg.MaxConcurrentSubscriptions > 0 && m.count() >= m.cfg.MaxConcurrentSubscriptions {
		return CreateResult{}, ErrTooManySubscriptions
	}

	readItems := make([]backend.ReadRequestItem, len(req.Items))
	for i, it := range req.Items {
		readItems[i] = backend.ReadRequestItem{Ref: it.Ref}
	}
	results, err := m.backend.Reader.Read(ctx, readItems)
	if err != nil {
		return CreateResult{}, err
	}

	pingRate := req.SubscriptionPingRate
	if pingRate <= 0 {
		pingRate = m.cfg.DefaultSubscriptionPingRate
	}

	var items []*itemState
	out := CreateResult{Items: make([]CreateItemResult, len(req.Items))}
	for i, it := range req.Items {
		var res backend.Result[backend.ItemSample]
		if i < len(results) {
			res = results[i]
		}
		rate := it.RequestedSamplingRate
		if rate <= 0 {
			rate = m.cfg.DefaultSamplingRate
		}
		itemResult := CreateItemResult{
			ClientItemHandle:    it.ClientItemHandle,
			RevisedSamplingRate: rate,
			ResultID:            res.ResultID,
		}
		if res.ResultID.IsZero() {
			items = append(items, &itemState{
				ref:                   it.Ref,
				clientItemHandle:      it.ClientItemHandle,
				requestedSamplingRate: rate,
				revisedSamplingRate:   rate,
				deadband:              it.Deadband,
				enableBuffering:       it.EnableBuffering,
				haveLast:              true,
				last:                  res.Value,
			})
			if req.ReturnValuesOnReply {
				itemResult.Sample = res.Value
				itemResult.HaveSample = true
			}
		}
		out.Items[i] = itemResult
	}

	if len(items) == 0 {
		return out, nil // Handle stays "" — no subscription created
	}

	ctx2, cancel := context.WithCancel(m.rootCtx)
	s := &subState{
		handle:              newHandle(),
		mgr:                 m,
		ctx:                 ctx2,
		cancel:              cancel,
		returnValuesOnReply: req.ReturnValuesOnReply,
		items:               items,
		pingRate:            pingRate,
		lastPolledAt:        m.clock.Now(),
		changedCh:           make(chan struct{}),
	}

	// The shutdown check and the limit re-check must both happen in the
	// same critical section as the insert itself — otherwise either can
	// race a concurrent BeginShutdown/Create the same way the old
	// pre-Read-call-only limit check did (ADR-007 calls for an atomic
	// reject-if-over-limit check "at insert time", not merely "before the
	// backend call"). BeginShutdown holds this same mutex while calling
	// rootCancel (see manager.go), so if it wins the race, the check
	// below is guaranteed to observe m.rootCtx already cancelled; if
	// Create wins, the subscription is fully, normally registered before
	// any concurrent shutdown can proceed — never a half-registered,
	// orphaned entry that nothing would otherwise clean up (the reaper's
	// own chain stops rescheduling once shutdown begins).
	m.mu.Lock()
	if m.rootCtx.Err() != nil {
		m.mu.Unlock()
		cancel()
		return CreateResult{}, m.rootCtx.Err()
	}
	if m.cfg.MaxConcurrentSubscriptions > 0 && len(m.subs) >= m.cfg.MaxConcurrentSubscriptions {
		m.mu.Unlock()
		cancel()
		return CreateResult{}, ErrTooManySubscriptions
	}
	m.subs[s.handle] = s
	m.mu.Unlock()
	m.metrics.SetActiveSubscriptions(m.count())

	m.startRefreshing(s)

	out.Handle = s.handle
	return out, nil
}

// Cancel cancels the subscription identified by handle, freeing its
// resources and invalidating the handle for future PolledRefresh calls.
// If handle was part of an in-flight, still-blocked multi-handle
// PolledRefresh call, that call returns immediately with whatever data
// remains available for the other handles. Cancelling an unknown or
// already-cancelled handle is a safe, idempotent no-op (returns false)
// rather than an error — REQ-SUBSCRIPTION-014, open-questions.md OQ-9.
func (m *Manager) Cancel(handle Handle) bool {
	found := m.terminate(handle)
	if found {
		m.metrics.SetActiveSubscriptions(m.count())
	}
	return found
}
