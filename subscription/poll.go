package subscription

import (
	"context"
	"math"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
)

// startRefreshing begins keeping s's items up to date: push-mode if the
// backend's Reader also implements backend.ChangeNotifier, poll-mode
// otherwise (see ADR-008).
func (m *Manager) startRefreshing(s *subState) {
	if cn, ok := m.backend.Reader.(backend.ChangeNotifier); ok {
		m.startPush(s, cn)
		return
	}
	m.schedulePoll(s, s.minSamplingRate())
}

func (s *subState) minSamplingRate() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	minRate := time.Duration(0)
	for _, it := range s.items {
		if minRate == 0 || it.revisedSamplingRate < minRate {
			minRate = it.revisedSamplingRate
		}
	}
	if minRate <= 0 {
		minRate = time.Second
	}
	return minRate
}

// schedulePoll arranges for s to be polled after in, via a
// self-rescheduling clock.Clock.AfterFunc chain: no goroutine exists
// while idle, only transiently while the callback executes
// (docs/architecture/subscription-model.md, ADR-008). Checking
// s.ctx.Err() at the top of the callback, and again before rescheduling,
// is the cleanup path — a cancelled or shut-down subscription's poll
// chain simply stops here.
func (m *Manager) schedulePoll(s *subState, in time.Duration) {
	m.clock.AfterFunc(in, func() {
		// Add/Done bracket exactly this one callback invocation — the
		// genuine goroutine that exists "only while a poll callback is
		// actually executing" (docs/architecture/subscription-model.md).
		// Add happens unconditionally as the very first statement, before
		// the ctx check below, so there is no window in which this
		// callback has already started running but a concurrent
		// Manager.Wait cannot yet see it: Add must complete before the ctx
		// check can possibly let pollOnceBounded's real backend call
		// proceed, so Wait can never return early while that call is
		// still in flight (previously, checking ctx first and only
		// calling Add afterward left exactly that gap). This is still
		// safe for an armed-but-never-fired clock.Clock.AfterFunc timer
		// (e.g. under clocktest.Fake, if a test never advances that far):
		// if the callback body never executes at all, this line is never
		// reached, so the WaitGroup counter is never touched — only a
		// callback that actually fires calls Add, exactly as before.
		m.wg.Add(1)
		defer m.wg.Done()
		if s.ctx.Err() != nil {
			return
		}
		m.pollOnceBounded(s)
		if s.ctx.Err() == nil {
			m.schedulePoll(s, s.minSamplingRate())
		}
	})
}

// pollOnceBounded gates the actual backend call behind Manager.pollSem so
// many subscriptions firing at the same instant cannot spawn unbounded
// concurrent backend calls.
func (m *Manager) pollOnceBounded(s *subState) {
	select {
	case m.pollSem <- struct{}{}:
	case <-s.ctx.Done():
		return
	}
	defer func() { <-m.pollSem }()
	m.pollOnce(s)
}

// pollOnce fires on the subscription's single shared timer, which ticks
// at minSamplingRate() — the fastest rate any one item requested, since
// one shared timer can only ever be as fast as its most demanding item
// (ADR-008: no per-item goroutine/timer). Reading and evaluating every
// item on every such tick would poll a slower item far more often than
// its own RevisedSamplingRate promises the client. Instead, each item's
// own lastPolledAt gates whether it's actually due on this particular
// tick — a slower item is skipped (not read, not evaluated for change)
// on ticks that land before its own interval has elapsed.
func (m *Manager) pollOnce(s *subState) {
	defer m.recoverBackgroundPanic("pollOnce")
	s.mu.Lock()
	items := append([]*itemState(nil), s.items...)
	s.mu.Unlock()
	if len(items) == 0 {
		return
	}

	now := m.clock.Now()
	due := make([]*itemState, 0, len(items))
	for _, it := range items {
		it.mu.Lock()
		isDue := it.lastPolledAt.IsZero() || !now.Before(it.lastPolledAt.Add(it.revisedSamplingRate))
		it.mu.Unlock()
		if isDue {
			due = append(due, it)
		}
	}
	if len(due) == 0 {
		return
	}

	readItems := make([]backend.ReadRequestItem, len(due))
	for i, it := range due {
		readItems[i] = backend.ReadRequestItem{Ref: it.ref}
	}

	ctx, cancel := context.WithTimeout(s.ctx, m.cfg.PollTimeout)
	defer cancel()
	results, err := m.backend.Reader.Read(ctx, readItems)
	if err != nil {
		m.log.Warn("subscription poll failed", "handle", string(s.handle), "error", err.Error())
		return
	}

	changed := false
	for i, it := range due {
		if i >= len(results) {
			break
		}
		it.mu.Lock()
		it.lastPolledAt = now
		it.mu.Unlock()
		if applySample(it, results[i].Value, m.cfg.MaxBufferedSamplesPerItem) {
			changed = true
		}
	}
	if changed {
		s.notifyChanged()
	}
}

// applySample records sample for it if it represents a change worth
// reporting (a quality change, or a value change outside the item's
// deadband for numeric types), buffering it if EnableBuffering is set.
// It reports whether anything changed.
//
// Deadband here is a best-effort approximation: the specification's
// percentage-of-engineering-range definition needs the item's EU range
// (highEU/lowEU), which is a property this layer does not have access
// to — so, when a deadband is set and both the previous and new values
// are numeric, this compares percentage change relative to the previous
// value instead of the item's EU range. Documented as an accepted
// simplification (deadband/buffering are explicitly "soft negotiated
// behaviors" per docs/architecture/subscription-model.md), not a bug.
func applySample(it *itemState, sample backend.ItemSample, maxBuffer int) bool {
	it.mu.Lock()
	defer it.mu.Unlock()

	changed := !it.haveLast || sampleChanged(it.last, sample, it.deadband)
	it.last = sample
	it.haveLast = true
	if !changed {
		return false
	}
	if it.enableBuffering {
		it.buffer = append(it.buffer, sample)
		if len(it.buffer) > maxBuffer {
			// Oldest purged first; the Latest Changed Value (the most
			// recent entry) is always retained (REQ-SUBSCRIPTION-007).
			it.buffer = it.buffer[len(it.buffer)-maxBuffer:]
			it.overflowed = true
		}
	} else {
		it.buffer = []backend.ItemSample{sample} // only the latest value
	}
	return true
}

// sampleChanged reports whether next differs meaningfully from prev: any
// quality change always counts; a numeric value change counts if it
// exceeds deadbandPct percent of the previous value (or unconditionally,
// if deadbandPct is 0 or either value is non-numeric).
func sampleChanged(prev, next backend.ItemSample, deadbandPct float64) bool {
	if prev.Quality.QualityField() != next.Quality.QualityField() ||
		prev.Quality.LimitField() != next.Quality.LimitField() ||
		prev.Quality.VendorField() != next.Quality.VendorField() {
		return true
	}
	if deadbandPct > 0 {
		pf, pok := prev.Value.NumericAsFloat64()
		nf, nok := next.Value.NumericAsFloat64()
		if pok && nok {
			pNaN, nNaN := math.IsNaN(pf), math.IsNaN(nf)
			if pNaN || nNaN {
				// NaN compares false against every operation below
				// (IEEE-754), so it can't take part in a percentage
				// comparison — without this, a transition into or out of
				// NaN (e.g. a sensor fault clearing) silently computed a
				// NaN delta, which "delta >= deadbandPct" then always
				// reported as unchanged, permanently hiding that
				// transition from a deadbanded client. Treat any
				// transition into/out of NaN as significant, and two
				// consecutive NaN readings as unchanged (avoiding a
				// notification storm while the value stays invalid).
				return pNaN != nNaN
			}
			if pf == nf {
				return false
			}
			if pf != 0 {
				delta := (nf - pf) / pf * 100
				if delta < 0 {
					delta = -delta
				}
				return delta >= deadbandPct
			}
			return true // previous was exactly zero: any change is significant
		}
	}
	return !prev.Value.Equal(next.Value)
}
