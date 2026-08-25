package subscription

import "github.com/dernate/gopcxmlda-server/backend"

// startPush begins push-mode refresh for s using cn: exactly one
// long-lived drain goroutine for the subscription's lifetime — the one
// documented, accepted goroutine cost of push efficiency (ADR-008). It
// falls back to poll-mode if the initial WatchItems call fails.
func (m *Manager) startPush(s *subState, cn backend.ChangeNotifier) {
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
	// m.wg.Go (not a bare "go func"): a backend slower than PollTimeout
	// leaves this goroutine running past the select below, which falls
	// back to poll-mode without it. Manager.Wait/Shutdown must still know
	// about it and wait for it to actually return — otherwise shutdown
	// could complete while a call into third-party backend code is still
	// in flight, silently violating Wait's documented "every background
	// goroutine has exited" guarantee.
	m.wg.Go(func() {
		ch, err := cn.WatchItems(s.ctx, watchItems)
		resCh <- watchResult{ch, err}
	})

	select {
	case res := <-resCh:
		if res.err != nil {
			m.log.Warn("subscription: WatchItems failed, falling back to polling", "handle", string(s.handle), "error", res.err.Error())
			m.schedulePoll(s, s.minSamplingRate())
			return
		}
		m.wg.Go(func() {
			m.drainPush(s, res.ch, byRef)
		})
	case <-m.clock.After(m.cfg.PollTimeout):
		m.log.Warn("subscription: WatchItems did not respond within PollTimeout, falling back to polling", "handle", string(s.handle))
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
	if ev.Err != nil {
		m.log.Warn("subscription: item watch broke, item will go stale", "handle", string(s.handle), "error", ev.Err.Error())
		return
	}
	its, ok := byRef[ev.Ref]
	if !ok {
		return
	}
	changed := false
	for _, it := range its {
		if applySample(it, ev.Sample, m.cfg.MaxBufferedSamplesPerItem) {
			changed = true
		}
	}
	if changed {
		s.notifyChanged()
	}
}
