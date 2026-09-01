package subscription

import (
	"math"
	"time"
)

// startReaper begins the abandonment-reaper sweep: a self-rescheduling
// clock.Clock.AfterFunc chain (the same idiom as poll scheduling,
// ADR-008), not a permanently-blocked loop, waking every
// Config.ReapInterval to free subscriptions the client has stopped
// polling (REQ-SUBSCRIPTION-013). Lazy/on-demand checking alone would be
// insufficient: a truly abandoned subscription has no future request to
// trigger a check.
func (m *Manager) startReaper() {
	m.scheduleReap()
}

func (m *Manager) scheduleReap() {
	// armTimer (see manager.go) tracks this callback in m.wg from the
	// moment it is armed, so a concurrent Manager.Wait cannot return
	// while a sweep is pending or running.
	t, armed := m.armTimer(m.cfg.ReapInterval, func() {
		if m.rootCtx.Err() != nil {
			return // shut down: stop the chain
		}
		m.reapOnce()
		if m.rootCtx.Err() == nil {
			m.scheduleReap()
		}
	})
	if !armed {
		return // shut down: armTimer declined, nothing to record
	}
	// Hand the armed timer to the Manager so BeginShutdown can stop it;
	// an unstopped one keeps firing-closure state reachable for up to a
	// full ReapInterval after shutdown. If shutdown already happened while
	// this timer was being armed, stop it here instead.
	m.mu.Lock()
	if m.rootCtx.Err() != nil {
		m.mu.Unlock()
		t.Stop()
		return
	}
	m.reapTimer = t
	m.mu.Unlock()
}

// reapGrace computes pingRate scaled by multiplier, clamped to a
// non-negative value representable as a time.Duration. Without this, an
// extreme (but not otherwise rejected) SubscriptionPingRate/
// ReapGraceMultiplier combination can overflow int64 nanoseconds when
// converted from float64, producing a negative "grace" that reaps a
// subscription the client asked to keep alive for a very long time.
func reapGrace(pingRate time.Duration, multiplier float64) time.Duration {
	g := float64(pingRate) * multiplier
	if g < 0 {
		return 0
	}
	if g > float64(math.MaxInt64) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(g)
}

// reapOnce terminates every subscription that hasn't been polled within
// pingRate * Config.ReapGraceMultiplier. pingRate is always already the
// resolved, nonzero value (Config.DefaultSubscriptionPingRate substituted
// at Create time when the client sent 0) — see REQ-SUBSCRIPTION-015 and
// open-questions.md OQ-10; this sweep never needs to special-case zero.
func (m *Manager) reapOnce() {
	defer m.recoverBackgroundPanic("reapOnce")
	now := m.clock.Now()
	m.mu.RLock()
	var toReap []Handle
	for handle, s := range m.subs {
		if s.isBusy() {
			// A PolledRefresh is in flight for this subscription right
			// now — that IS the client's liveness signal, and it is the
			// strongest one available. Without this check the sweep
			// reaps a subscription mid-call whenever the client's own
			// requested Hold+Wait outlasts the grace period, which is
			// entirely ordinary: a SubscriptionPingRate of 3s (as in the
			// real captured traffic) gives a 6s grace against a Hold+Wait
			// that may legitimately run to MaxPolledRefreshWait.
			continue
		}
		s.mu.Lock()
		abandoned := now.Sub(s.lastPolledAt) > reapGrace(s.pingRate, m.cfg.ReapGraceMultiplier)
		s.mu.Unlock()
		if abandoned {
			toReap = append(toReap, handle)
		}
	}
	m.mu.RUnlock()

	reaped := 0
	for _, handle := range toReap {
		// Re-validate right before removal, under the subscription's own
		// lock: this pass ran under only a read lock on the map plus a
		// per-subscription lock released between the decision and this
		// action, so a client's PolledRefresh may have legitimately
		// renewed lastPolledAt in that window (REQ-SUBSCRIPTION-013 must
		// not undo a just-renewed subscription).
		if m.terminateIfStillAbandoned(handle, now) {
			reaped++
			m.metrics.IncSubscriptionError("abandoned")
			m.log.Info("subscription abandoned and reaped", "handle", string(handle))
		}
	}
	if reaped > 0 {
		m.metrics.SetActiveSubscriptions(m.count())
	}
}

// terminateIfStillAbandoned removes handle only if it is still abandoned
// as of asOf (the reap sweep's start time), re-checked under handle's own
// subState lock while holding the map's write lock — closing the race
// window between reapOnce's decision pass and this action pass.
func (m *Manager) terminateIfStillAbandoned(handle Handle, asOf time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.subs[handle]
	if !ok {
		return false
	}
	// Re-checked here too, not just in the decision pass: a PolledRefresh
	// may have started in the window between the two.
	if s.isBusy() {
		return false
	}
	s.mu.Lock()
	stillAbandoned := asOf.Sub(s.lastPolledAt) > reapGrace(s.pingRate, m.cfg.ReapGraceMultiplier)
	s.mu.Unlock()
	if !stillAbandoned {
		return false
	}
	delete(m.subs, handle)
	s.cancel()
	s.stopPolling()
	s.releaseBuffers(m.budget)
	return true
}
