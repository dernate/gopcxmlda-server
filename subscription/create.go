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

// ErrTooManyItems is returned by Create when Config.MaxTotalSubscribedItems
// would be exceeded — the server-wide item budget, distinct from the
// per-subscription and per-request item limits the server layer enforces.
// Mapped to the same E_OUTOFMEMORY fault as ErrTooManySubscriptions.
var ErrTooManyItems = errors.New("subscription: maximum number of subscribed items across all subscriptions reached")

// ErrShuttingDown is returned by Create once BeginShutdown has run: the
// Manager will accept no new subscriptions. The server layer maps this to
// an E_SERVERSTATE fault (the specification's code for "the server cannot
// perform this operation in its current state"), not the E_FAIL that a
// bare context.Canceled would otherwise produce.
var ErrShuttingDown = errors.New("subscription: server is shutting down")

// CreateItemRequest is one item requested in a Create call.
type CreateItemRequest struct {
	Ref                   backend.ItemRef
	ClientItemHandle      string
	RequestedSamplingRate time.Duration // 0 = fastest practical
	Deadband              float64       // 0-100%, analog/array types only
	EnableBuffering       bool
	// ReqType is the client's requested value type for this item
	// (§3.1.3's hierarchical ReqType, which SubscribeRequestItemList and
	// SubscribeRequestItem both carry). This engine stores it and hands
	// it back on every result; it never coerces itself — that is pure
	// xmlda.Value logic the server layer applies on the way out, exactly
	// as it does for Read.
	ReqType *xmlda.QName
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
	// ReqType echoes the requested type, for the server layer's coercion
	// step; nil if the client requested none.
	ReqType *xmlda.QName
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

	// The rate the server would promise for each item, before the backend
	// gets a say: the client's own request, or the configured default when
	// it asked for "fastest practical".
	requestedRates := make([]time.Duration, len(req.Items))
	for i, it := range req.Items {
		requestedRates[i] = it.RequestedSamplingRate
		if requestedRates[i] <= 0 {
			requestedRates[i] = m.cfg.DefaultSamplingRate
		}
	}
	revisedRates := m.reviseSamplingRates(ctx, req.Items, requestedRates)

	var items []*itemState
	out := CreateResult{Items: make([]CreateItemResult, len(req.Items))}
	for i, it := range req.Items {
		var res backend.Result[backend.ItemSample]
		if i < len(results) {
			res = results[i]
		}
		rate := revisedRates[i]
		// A success-with-caveat code from the initial read (S_CLAMP,
		// S_DATAQUEUEOVERFLOW) still describes a readable item, so it is
		// subscribed like any other; only a critical E_ code excludes it.
		// Treating every non-zero code as "invalid" silently dropped items
		// the backend had explicitly said were usable.
		usable := res.ResultID.IsZero() || res.ResultID.IsSuccess()
		resultID := res.ResultID
		if usable && resultID.IsZero() && rate != requestedRates[i] {
			// §3.5.2's own signal for "you asked for a rate I cannot do;
			// here is the one you get". A success code, so the item stays
			// subscribed and keeps delivering values.
			resultID = xmlda.SuccessUnsupportedRate
		}
		itemResult := CreateItemResult{
			ClientItemHandle:    it.ClientItemHandle,
			RevisedSamplingRate: rate,
			ReqType:             it.ReqType,
			ResultID:            resultID,
		}
		if usable {
			items = append(items, &itemState{
				ref:                   it.Ref,
				clientItemHandle:      it.ClientItemHandle,
				requestedSamplingRate: requestedRates[i],
				revisedSamplingRate:   rate,
				deadband:              it.Deadband,
				enableBuffering:       it.EnableBuffering,
				reqType:               it.ReqType,
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
		return CreateResult{}, ErrShuttingDown
	}
	if m.cfg.MaxConcurrentSubscriptions > 0 && len(m.subs) >= m.cfg.MaxConcurrentSubscriptions {
		m.mu.Unlock()
		cancel()
		return CreateResult{}, ErrTooManySubscriptions
	}
	// The server-wide item budget, checked in the same critical section
	// for the same reason as the subscription count: a limit only on the
	// number of subscriptions says nothing about how much memory they
	// hold between them.
	if m.cfg.MaxTotalSubscribedItems > 0 &&
		m.totalItemsLocked()+len(items) > m.cfg.MaxTotalSubscribedItems {
		m.mu.Unlock()
		cancel()
		return CreateResult{}, ErrTooManyItems
	}
	m.subs[s.handle] = s
	m.mu.Unlock()
	m.metrics.SetActiveSubscriptions(m.count())

	m.startRefreshing(ctx, s)

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

// reviseSamplingRates asks the backend, if it implements
// backend.SamplingRateReviser, which of the requested rates it can
// actually achieve. It returns one rate per item, defaulting to the
// requested one.
//
// Every failure mode here falls back to the requested rates rather than
// failing the Subscribe: a backend that cannot answer the question, or
// answers it with the wrong number of entries, is a reason to serve the
// subscription at the rate the client named — not a reason to refuse it.
func (m *Manager) reviseSamplingRates(ctx context.Context, reqItems []CreateItemRequest, requested []time.Duration) []time.Duration {
	revised := append([]time.Duration(nil), requested...)
	reviser, ok := m.backend.Reader.(backend.SamplingRateReviser)
	if !ok {
		return revised
	}
	rateReqs := make([]backend.RateRequest, len(reqItems))
	for i, it := range reqItems {
		rateReqs[i] = backend.RateRequest{Ref: it.Ref, RequestedSamplingRate: requested[i]}
	}
	rates, err := reviser.ReviseSamplingRates(ctx, rateReqs)
	if err != nil {
		m.log.Warn("subscription: ReviseSamplingRates failed, honoring the requested rates",
			"error", err.Error())
		return revised
	}
	if len(rates) != len(rateReqs) {
		m.log.Warn("subscription: ReviseSamplingRates returned the wrong number of rates, honoring the requested rates",
			"requested", len(rateReqs), "returned", len(rates))
		return revised
	}
	for i, r := range rates {
		// A zero or negative revision is "no opinion", not "sample
		// instantly": a rate of zero would make this item due on every
		// single tick of its subscription's shared timer.
		if r > 0 {
			revised[i] = r
		}
	}
	return revised
}
