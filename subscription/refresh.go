package subscription

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// ErrBusy is returned when one or more requested subscriptions are
// already being polled by another concurrent PolledRefresh call
// (REQ-SUBSCRIPTION-009) — a whole-operation SOAP fault (E_BUSY) at the
// server layer.
var ErrBusy = errors.New("subscription: one or more requested subscriptions are already being polled")

// ErrNoSubscription is returned when none of the requested handles are
// valid (REQ-SUBSCRIPTION-008) — a whole-operation SOAP fault
// (E_NOSUBSCRIPTION) at the server layer.
var ErrNoSubscription = errors.New("subscription: none of the requested handles are valid")

// RefreshRequest is the input to Manager.PolledRefresh.
type RefreshRequest struct {
	Handles []Handle
	// HoldTime is absolute; nil means "return immediately" (the basic
	// polling approach, §2.5.1 — WaitTime is then ignored,
	// REQ-SUBSCRIPTION-005).
	HoldTime       *time.Time
	WaitTime       time.Duration
	ReturnAllItems bool
}

// RefreshItemResult is one item's outcome in a RefreshSubscriptionResult.
type RefreshItemResult struct {
	Ref              backend.ItemRef
	ClientItemHandle string
	// ReqType echoes the type this item was subscribed with, for the
	// server layer's coercion step; nil if the client requested none.
	ReqType *xmlda.QName
	// Sample is meaningful only if HaveSample is true.
	Sample backend.ItemSample
	// HaveSample is false when this entry reports an abnormal condition
	// rather than a value — i.e. whenever ResultID is non-zero. The server
	// layer must not build a wire Value from Sample in that case.
	HaveSample bool
	// ResultID is the item's condition at the time this entry was
	// recorded; the zero ErrorCode means the item was healthy and Sample
	// holds its value.
	ResultID xmlda.ErrorCode
}

// RefreshSubscriptionResult is one polled subscription's changed (or, if
// ReturnAllItems, all) items.
type RefreshSubscriptionResult struct {
	Handle Handle
	Items  []RefreshItemResult
}

// RefreshResult is the outcome of PolledRefresh.
type RefreshResult struct {
	DataBufferOverflow bool
	InvalidHandles     []Handle
	Subscriptions      []RefreshSubscriptionResult
}

// PolledRefresh implements SubscriptionPolledRefresh (§3.6). It blocks
// according to req.HoldTime/WaitTime/ReturnAllItems
// (docs/architecture/subscription-model.md), and unblocks immediately if
// any requested subscription is cancelled or the server shuts down mid-hold
// (REQ-SUBSCRIPTION-010), or if ctx is done.
func (m *Manager) PolledRefresh(ctx context.Context, req RefreshRequest) (RefreshResult, error) {
	if len(req.Handles) == 0 {
		return RefreshResult{}, ErrNoSubscription
	}

	m.mu.RLock()
	seen := make(map[Handle]struct{}, len(req.Handles))
	subs := make([]*subState, 0, len(req.Handles))
	var invalid []Handle
	for _, h := range req.Handles {
		// A handle repeated within one request must be deduplicated here:
		// otherwise the same *subState would be visited twice below, and
		// the busy-flag acquisition loop would CAS against the flag it
		// just set on the first visit, self-triggering a false ErrBusy
		// rather than reflecting an actual concurrent conflict.
		if _, dup := seen[h]; dup {
			continue
		}
		seen[h] = struct{}{}
		if s, ok := m.subs[h]; ok {
			subs = append(subs, s)
		} else {
			invalid = append(invalid, h)
		}
	}
	m.mu.RUnlock()

	if len(subs) == 0 {
		return RefreshResult{InvalidHandles: invalid}, ErrNoSubscription
	}

	// Non-blocking busy-flag acquisition across all requested handles;
	// release whatever was acquired on any failure or on return
	// (REQ-SUBSCRIPTION-009).
	acquired := make([]*subState, 0, len(subs))
	defer func() {
		for _, s := range acquired {
			// lastPolledAt is refreshed on the way out as well as on the
			// way in: the specification measures abandonment "since the
			// last response to a refresh service call" (§2.5.4), not
			// since the request arrived. For a long Hold+Wait the two are
			// a minute or more apart, and only the later one reflects
			// when the client actually got its answer.
			s.touchPolledAt(m.clock.Now())
			atomic.StoreInt32(&s.busyFlag, 0)
		}
	}()
	for _, s := range subs {
		if !atomic.CompareAndSwapInt32(&s.busyFlag, 0, 1) {
			return RefreshResult{}, ErrBusy
		}
		acquired = append(acquired, s)
	}

	now := m.clock.Now()
	for _, s := range subs {
		s.touchPolledAt(now)
	}

	// callCtx bounds every fan-in goroutine spawned below to this call's
	// own lifetime, however it returns — the mechanism that keeps them
	// self-cleaning regardless of which case fires (see fanIn's doc).
	callCtx, callCancel := context.WithCancel(ctx)
	defer callCancel()

	if req.HoldTime != nil {
		holdDur := max(req.HoldTime.Sub(now), 0)
		cancelled := fanIn(callCtx, cancelChans(subs))

		// Phase 1: hold unconditionally — no early return on change here.
		select {
		case <-m.clock.After(holdDur):
		case <-cancelled:
			return m.snapshotResult(subs, invalid, req.ReturnAllItems), nil
		case <-ctx.Done():
			return RefreshResult{}, ctx.Err()
		}

		if !req.ReturnAllItems {
			// Snapshot each subscription's *current* changedCh generation
			// before checking hasPendingChanges, not after: applySample
			// always appends to it.buffer before calling notifyChanged (in
			// both poll.go and push.go), so if a change lands in the
			// window between these two steps, either hasPendingChanges
			// below still observes the already-appended buffer (caught
			// there), or notifyChanged closes exactly the channel
			// generation captured here (caught by fanIn below, since a
			// closed channel is immediately selectable). Checking
			// hasPendingChanges first and only then snapshotting the
			// channel (the previous order) could capture the *new*
			// generation after the swap, missing the close of the old one
			// and stalling for the full WaitTime despite data already
			// being ready.
			pendingChangeCh := changedChans(subs)
			if !hasPendingChanges(subs) {
				// Phase 2: wait up to WaitTime for a change, early return
				// on change (REQ-SUBSCRIPTION-005/006). Skipped entirely
				// if a change already occurred during the hold phase
				// itself — s.changedCh is edge-triggered (closed once,
				// then replaced), so a change during Phase 1 would
				// otherwise go undetected by a fan-in set up only now,
				// forcing a pointless full WaitTime wait despite the data
				// already being ready.
				changed := fanIn(callCtx, pendingChangeCh)
				select {
				case <-changed:
				case <-m.clock.After(req.WaitTime):
				case <-cancelled:
				case <-ctx.Done():
					return RefreshResult{}, ctx.Err()
				}
			}
		}
	}
	// HoldTime == nil: return immediately, no waiting at all.

	return m.snapshotResult(subs, invalid, req.ReturnAllItems), nil
}

func cancelChans(subs []*subState) []<-chan struct{} {
	chans := make([]<-chan struct{}, len(subs))
	for i, s := range subs {
		chans[i] = s.ctx.Done()
	}
	return chans
}

// hasPendingChanges reports whether any item across subs already has a
// buffered, undelivered change.
func hasPendingChanges(subs []*subState) bool {
	for _, s := range subs {
		s.mu.Lock()
		items := append([]*itemState(nil), s.items...)
		s.mu.Unlock()
		for _, it := range items {
			it.mu.Lock()
			has := len(it.buffer) > 0
			it.mu.Unlock()
			if has {
				return true
			}
		}
	}
	return false
}

func changedChans(subs []*subState) []<-chan struct{} {
	chans := make([]<-chan struct{}, len(subs))
	for i, s := range subs {
		chans[i] = s.changedChan()
	}
	return chans
}

// fanIn returns a channel that closes as soon as any of chans fires, or
// when callCtx is done — whichever comes first. Every spawned goroutine
// exits via callCtx.Done(), so the caller cancelling callCtx (typically
// via defer, immediately before returning) is what keeps these
// goroutines bounded to one call's lifetime, however that call returns.
func fanIn(callCtx context.Context, chans []<-chan struct{}) <-chan struct{} {
	out := make(chan struct{})
	var once sync.Once
	for _, ch := range chans {
		go func(ch <-chan struct{}) {
			select {
			case <-ch:
				once.Do(func() { close(out) })
			case <-callCtx.Done():
			}
		}(ch)
	}
	return out
}

// snapshotResult drains each subscription's changed/buffered items (or,
// if returnAllItems, every item's last known value) into a RefreshResult.
//
// A subscription cancelled while this very call was blocked (ctx.Err() !=
// nil) is reported in InvalidHandles, exactly as a handle that was
// already unknown when the call started: from the client's side the two
// are the same fact ("this handle is no longer usable"), and
// InvalidServerSubHandles is the field the specification gives for
// saying so. Omitting it entirely — the previous behavior — returned a
// formally successful, completely empty response that a client reads as
// "no changes", leaving it to discover the subscription is gone only on
// the next poll.
func (m *Manager) snapshotResult(subs []*subState, invalid []Handle, returnAllItems bool) RefreshResult {
	result := RefreshResult{InvalidHandles: invalid}
	for _, s := range subs {
		if s.ctx.Err() != nil {
			result.InvalidHandles = append(result.InvalidHandles, s.handle)
			continue
		}
		s.mu.Lock()
		items := append([]*itemState(nil), s.items...)
		s.mu.Unlock()

		var itemResults []RefreshItemResult
		overflow := false
		for _, it := range items {
			it.mu.Lock()
			switch {
			case returnAllItems:
				// "The server will wait the HoldTime but then return with
				// all current values (and any buffered values if
				// EnableBuffering)" — §3.6.1. The buffered entries come
				// first, in the order they were recorded, so the client
				// still sees the history it asked to have buffered;
				// discarding them here (the previous behavior) meant one
				// ReturnAllItems poll silently threw away everything
				// EnableBuffering had collected.
				if it.enableBuffering {
					for _, u := range it.buffer {
						itemResults = append(itemResults, RefreshItemResult{
							Ref:              it.ref,
							ClientItemHandle: it.clientItemHandle,
							ReqType:          it.reqType,
							Sample:           u.sample,
							HaveSample:       u.haveSample,
							ResultID:         u.resultID,
						})
					}
				}
				// An item currently in an abnormal condition reports that
				// condition, not its stale last value dressed up as
				// current: ReturnAllItems asks for every item's state, and
				// "unreadable" is that state.
				switch {
				case !it.lastResultID.IsZero():
					itemResults = append(itemResults, RefreshItemResult{
						Ref:              it.ref,
						ClientItemHandle: it.clientItemHandle,
						ReqType:          it.reqType,
						ResultID:         it.lastResultID,
					})
				case it.haveLast:
					itemResults = append(itemResults, RefreshItemResult{
						Ref:              it.ref,
						ClientItemHandle: it.clientItemHandle,
						ReqType:          it.reqType,
						Sample:           it.last,
						HaveSample:       true,
					})
				}
				it.buffer = nil
			case len(it.buffer) > 0:
				for _, u := range it.buffer {
					itemResults = append(itemResults, RefreshItemResult{
						Ref:              it.ref,
						ClientItemHandle: it.clientItemHandle,
						ReqType:          it.reqType,
						Sample:           u.sample,
						HaveSample:       u.haveSample,
						ResultID:         u.resultID,
					})
				}
				it.buffer = nil
			}
			if it.overflowed {
				overflow = true
				it.overflowed = false
			}
			it.mu.Unlock()
		}
		if overflow {
			result.DataBufferOverflow = true
		}
		if len(itemResults) > 0 || returnAllItems {
			result.Subscriptions = append(result.Subscriptions, RefreshSubscriptionResult{
				Handle: s.handle,
				Items:  itemResults,
			})
		}
	}
	return result
}
