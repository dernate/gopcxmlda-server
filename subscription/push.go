package subscription

import (
	"context"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// startPush begins push-mode refresh for s using cn: exactly one
// long-lived drain goroutine for the subscription's lifetime — the one
// documented, accepted goroutine cost of push efficiency (ADR-008). It
// falls back to poll-mode if the initial WatchItems call fails.
func (m *Manager) startPush(ctx context.Context, s *subState, cn backend.ChangeNotifier) {
	s.mu.Lock()
	items := append([]*itemState(nil), s.items...)
	s.mu.Unlock()

	// byRef maps each backend.ItemRef to every itemState watching it — a
	// client may legally subscribe the same ItemRef under two different
	// ClientItemHandles; a plain map[ItemRef]*itemState would silently
	// keep only the last one, freezing the others at their Create-time
	// value for the subscription's whole lifetime.
	watchItems := make([]backend.WatchRequest, len(items))
	byRef := make(map[backend.ItemRef][]*itemState, len(items))
	for i, it := range items {
		watchItems[i] = backend.WatchRequest{
			Ref:                   it.ref,
			RequestedSamplingRate: it.revisedSamplingRate,
			Deadband:              it.deadband,
		}
		byRef[it.ref] = append(byRef[it.ref], it)
	}

	// cn.WatchItems is a call into third-party backend code with no
	// documented time bound, made synchronously from Create. s.ctx (not a
	// request-scoped, short-lived context) must be the ctx passed to it,
	// since ChangeNotifier's own contract ties the returned channel's
	// lifetime to that same ctx ("backend ... must close it when ctx is
	// done") — a context that later expired on its own would incorrectly
	// tell the backend to close a channel meant to live for the whole
	// subscription. So the timeout below bounds only how long Create
	// waits for WatchItems to respond, not s.ctx itself: a hanging
	// backend no longer blocks the calling Subscribe request forever,
	// and if it eventually does respond after the wait gave up, that
	// late channel is simply not wired up (this subscription has already
	// committed to poll-mode by then, and running both modes at once
	// would risk duplicate/conflicting updates) — s.ctx still governs its
	// real lifetime either way, so the backend's own close-on-ctx-done
	// contract obligation is unaffected.
	type watchResult struct {
		ch  <-chan backend.ChangeEvent
		err error
	}
	resCh := make(chan watchResult, 1)
	// m.goTracked, not a bare "go func" and not m.wg.Go: a backend slower
	// than PollTimeout leaves this goroutine running past the select
	// below, which falls back to poll-mode without it. Manager.Wait/
	// Shutdown must still know about it and wait for it to actually
	// return — otherwise shutdown could complete while a call into
	// third-party backend code is still in flight, silently violating
	// Wait's documented "every background goroutine has exited"
	// guarantee. goTracked (rather than wg.Go) takes the WaitGroup slot
	// under m.mu together with the shutdown check, which is what keeps
	// the Add from racing a concurrent Wait — see its doc comment.
	if !m.goTracked(func() {
		// WatchItems is third-party backend code on a bare goroutine —
		// no net/http per-request recover exists here, so an unrecovered
		// panic would take the whole process down.
		defer m.recoverBackgroundPanic("WatchItems")
		ch, err := cn.WatchItems(s.ctx, watchItems)
		resCh <- watchResult{ch, err}
	}) {
		// Shutdown has already begun; s.ctx is cancelled and there is
		// nothing left to watch. Starting a poll chain instead would only
		// create a timer for stopPolling to tear down again.
		return
	}

	// NewTimer + Stop rather than clock.After: the losing branch's timer
	// would otherwise stay armed for the rest of PollTimeout with nothing
	// left to receive it.
	timeout := m.clock.NewTimer(m.cfg.PollTimeout)
	defer timeout.Stop()

	select {
	case res := <-resCh:
		if res.err != nil {
			m.log.Warn("subscription: WatchItems failed, falling back to polling", "handle", string(s.handle), "error", res.err.Error())
			m.schedulePoll(s, s.minSamplingRate())
			return
		}
		m.goTracked(func() {
			defer m.recoverBackgroundPanic("drainPush")
			m.drainPush(s, res.ch, byRef)
		})
	case <-timeout.C():
		m.log.Warn("subscription: WatchItems did not respond within PollTimeout, falling back to polling", "handle", string(s.handle))
		m.schedulePoll(s, s.minSamplingRate())
	case <-s.ctx.Done():
		// The subscription was cancelled, or the server began shutting
		// down, while waiting on the backend. Returning here rather than
		// falling through to schedulePoll matters twice over: this call
		// runs synchronously inside Create (and so inside the client's
		// Subscribe request), which would otherwise sit here for the
		// whole PollTimeout with nobody left to answer; and starting a
		// poll chain for an already-cancelled subscription only creates
		// a timer for stopPolling to tear down again.
		return
	case <-ctx.Done():
		// The Subscribe request itself was abandoned (client hung up, or
		// its own deadline elapsed). The subscription lives on — s.ctx is
		// still valid and the handle was already issued — so fall back to
		// poll mode rather than leaving it with no refresh source at all.
		m.log.Warn("subscription: Subscribe request ended while awaiting WatchItems, falling back to polling",
			"handle", string(s.handle))
		m.schedulePoll(s, s.minSamplingRate())
	}
}

// drainPush drains ch until s is cancelled or the backend closes ch,
// whichever comes first — the two conditions documented on
// backend.ChangeNotifier.WatchItems as its exit contract. A close that
// happens for any reason *other* than s.ctx being done (the backend's own
// connection dropping, or an error path on its side) falls back to
// poll-mode instead of silently going stale forever, the same way a
// failed initial WatchItems call already does in startPush.
func (m *Manager) drainPush(s *subState, ch <-chan backend.ChangeEvent, byRef map[backend.ItemRef][]*itemState) {
	for {
		select {
		case <-s.ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				if s.ctx.Err() == nil {
					m.log.Warn("subscription: push channel closed unexpectedly, falling back to polling", "handle", string(s.handle))
					m.schedulePoll(s, s.minSamplingRate())
				}
				return
			}
			m.handlePushEvent(s, ev, byRef)
		}
	}
}

// handlePushEvent processes one backend-supplied ChangeEvent, recovering
// from any panic inside it: this call reaches into third-party backend
// data (docs/backend-implementation.md), and this loop's own goroutine has
// no net/http per-request recover around it, so an unrecovered panic here
// would otherwise crash the whole process rather than just this one event.
func (m *Manager) handlePushEvent(s *subState, ev backend.ChangeEvent, byRef map[backend.ItemRef][]*itemState) {
	defer m.recoverBackgroundPanic("drainPush")
	its, ok := byRef[ev.Ref]
	if !ok {
		return
	}
	// A broken item watch is reported to the client as that item's
	// ResultID on the next SubscriptionPolledRefresh, not merely logged:
	// the client is the only party that can react to one of its subscribed
	// items having gone away, and it cannot do so if the condition never
	// leaves the server's log. backend.ErrorCodeFor is the same mapping
	// the server layer applies to a whole-operation backend error, so a
	// backend signalling e.g. FaultAccessDenied gets E_ACCESS_DENIED here
	// too rather than a flat E_FAIL.
	resultID := xmlda.ErrorCode{}
	if ev.Err != nil {
		resultID = backend.ErrorCodeFor(ev.Err)
		m.log.Warn("subscription: item watch broke",
			"handle", string(s.handle), "resultID", resultID.Local, "error", ev.Err.Error())
	}
	changed := false
	for _, it := range its {
		if applyUpdateMetered(it, ev.Sample, resultID, ev.DiagnosticInfo, m.cfg.MaxBufferedSamplesPerItem, m.budget, m.metrics) {
			changed = true
		}
	}
	if changed {
		s.notifyChanged()
	}
}
