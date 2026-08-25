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
	m.clock.AfterFunc(m.cfg.ReapInterval, func() {
		// See schedulePoll's comment (poll.go) on why Add happens
		// unconditionally as the callback's very first statement, before
		// the ctx check, rather than after it: this closes the window
		// where a concurrent Manager.Wait could otherwise return before
		// an already-firing callback's real work (reapOnce) is visible to
		// it. It remains safe for an armed-but-never-fired timer, since a
		// callback that never executes never reaches this line at all.
		m.wg.Add(1)
		defer m.wg.Done()
		if m.rootCtx.Err() != nil {
			return // shut down: stop the chain
		}
		m.reapOnce()
		if m.rootCtx.Err() == nil {
			m.scheduleReap()
		}
	})
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
	s.mu.Lock()
	stillAbandoned := asOf.Sub(s.lastPolledAt) > reapGrace(s.pingRate, m.cfg.ReapGraceMultiplier)
	s.mu.Unlock()
	if !stillAbandoned {
		return false
	}
	delete(m.subs, handle)
	s.cancel()
	return true
}
