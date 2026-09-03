package subscription

import (
	"context"
	"math"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock"
	"github.com/dernate/gopcxmlda-server/telemetry"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// startRefreshing begins keeping s's items up to date: push-mode if the
// backend's Reader also implements backend.ChangeNotifier, poll-mode
// otherwise (see ADR-008).
// ctx is the caller's request context (Create's), used only to bound how
// long the push-mode handshake waits on the backend — never as the
// subscription's own lifetime, which is s.ctx.
func (m *Manager) startRefreshing(ctx context.Context, s *subState) {
	if cn, ok := m.backend.Reader.(backend.ChangeNotifier); ok {
		m.startPush(ctx, s, cn)
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
//
// The armed timer is handed to s.setTimer so cancellation can stop it:
// an unstopped clock.Clock.AfterFunc timer keeps its closure — and
// through it the whole subState and every item's buffered data — reachable
// until it eventually fires, which for a slow sampling rate can be a long
// time after the subscription is already gone.
func (m *Manager) schedulePoll(s *subState, in time.Duration) {
	// armTimer, not clock.AfterFunc directly: the WaitGroup counter is
	// taken when the timer is armed and released when its callback
	// finishes (or when Stop prevents it), so Manager.Wait cannot return
	// while a poll is pending or in flight. See armTimer's doc comment
	// for why counting from inside the callback was not enough.
	due := m.clock.Now().Add(in)
	t, armed := m.armTimer(in, func() {
		if s.ctx.Err() != nil {
			return
		}
		// How far past its due time this tick actually ran. A
		// subscription promises the client a RevisedSamplingRate, and
		// that promise quietly stops holding once the poll semaphore
		// saturates or the backend slows down — the subscription keeps
		// working, just slower than it said. Nothing else is able to
		// notice that.
		if lag := m.clock.Now().Sub(due); lag > 0 {
			m.metrics.ObservePollLag(lag)
		}
		m.pollOnceBounded(s)
		if s.ctx.Err() == nil {
			// The next tick is placed relative to when this one was DUE,
			// not to when its work finished. Rescheduling "one full
			// interval from now" made the effective period rate + backend
			// duration + semaphore wait, so a slow backend silently
			// stretched every item's real sampling interval past the
			// RevisedSamplingRate the client was promised. A backend
			// slower than the interval itself would drift without bound.
			//
			// max(..., 0) collapses a missed deadline into "fire again
			// immediately" rather than a negative delay: falling behind
			// must not turn into a busy loop that also never catches up.
			next := s.minSamplingRate()
			if elapsed := m.clock.Now().Sub(due); elapsed > 0 {
				next = max(next-elapsed, 0)
			}
			m.schedulePoll(s, next)
		}
	})
	if !armed {
		// The Manager is already shutting down; armTimer declined to take a
		// WaitGroup slot, so there is nothing to stop or record.
		return
	}
	if !s.setTimer(t) {
		// Cancelled while this timer was being armed: nothing else holds a
		// reference to it, so stop it here.
		t.Stop()
	}
}

// setTimer records t as s's pending poll timer, reporting false if s is
// already cancelled — in which case the caller must stop t itself.
func (s *subState) setTimer(t clock.Timer) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		s.timer = nil
		return false
	}
	s.timer = t
	return true
}

// stopPolling stops s's pending poll timer, if any. Called immediately
// after s.cancel() everywhere a subscription is terminated.
func (s *subState) stopPolling() {
	s.mu.Lock()
	t := s.timer
	s.timer = nil
	s.mu.Unlock()
	if t != nil {
		t.Stop()
	}
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
	if len(results) < len(due) {
		// A conforming backend returns exactly one Result per requested
		// item (docs/backend-implementation.md). Report the missing tail
		// as E_FAIL rather than silently leaving those items untouched:
		// leaving them alone would also leave lastPolledAt unset, making
		// them due again on every single tick — a silent busy-poll against
		// a backend that is already misbehaving.
		m.log.Warn("subscription poll returned fewer results than items requested",
			"handle", string(s.handle), "requested", len(due), "returned", len(results))
	}

	changed := false
	for i, it := range due {
		res := backend.Result[backend.ItemSample]{ResultID: xmlda.ErrFail}
		if i < len(results) {
			res = results[i]
		}
		it.mu.Lock()
		it.lastPolledAt = now
		it.mu.Unlock()
		if applyUpdateMetered(it, res.Value, res.ResultID, res.DiagnosticInfo, m.cfg.MaxBufferedSamplesPerItem, m.budget, m.metrics) {
			changed = true
		}
	}
	if changed {
		s.notifyChanged()
	}
}

// applyUpdate records one poll/push outcome for it if it represents a
// change worth reporting, buffering it if EnableBuffering is set. It
// reports whether anything changed.
//
// resultID is the backend's per-item condition for this outcome. A
// non-zero one is a change in its own right (the item just became
// unreadable) and is recorded *without* overwriting the last good sample,
// so the client is told what went wrong instead of being handed a blank
// value dressed up as Good quality. A condition that persists across
// polls is reported once, not on every tick; recovery back to a zero
// resultID always counts as a change, since the client must learn the
// item is healthy again.
//
// Deadband here is a best-effort approximation: the specification's
// percentage-of-engineering-range definition needs the item's EU range
// (highEU/lowEU), which is a property this layer does not have access
// to — so, when a deadband is set and both the previous and new values
// are numeric, this compares percentage change relative to the previous
// value instead of the item's EU range. Documented as an accepted
// simplification (deadband/buffering are explicitly "soft negotiated
// behaviors" per docs/architecture/subscription-model.md), not a bug.
func applyUpdate(it *itemState, sample backend.ItemSample, resultID xmlda.ErrorCode, diagnosticInfo string, maxBuffer int, budget *sampleBudget) bool {
	return applyUpdateMetered(it, sample, resultID, diagnosticInfo, maxBuffer, budget, nil)
}

// applyUpdateMetered is applyUpdate with somewhere to report discarded
// samples. metrics may be nil (tests, and the direct applyUpdate entry
// point).
func applyUpdateMetered(it *itemState, sample backend.ItemSample, resultID xmlda.ErrorCode, diagnosticInfo string, maxBuffer int, budget *sampleBudget, metrics telemetry.Metrics) bool {
	it.mu.Lock()
	defer it.mu.Unlock()

	// The subscription was terminated while this update was in flight;
	// its buffers have already been returned to the server-wide budget.
	// Acquiring more here would leak them (see itemState.released).
	if it.released {
		return false
	}

	var changed bool
	var u update
	switch {
	// Only a critical E_ code means "this outcome carries no usable
	// value". An S_ code is a non-critical exception whose value IS
	// useful (§2.6: "For non-critical exceptions the returned value IS
	// useful, although the client may need to react to an abnormal
	// condition") — the same rule server/itemvalue.go's hasUsableValue
	// applies to Read/Write/Subscribe and subscription/create.go applies
	// to a Subscribe's initial read. Treating every non-zero code as
	// "no sample" here silenced an item reporting a persistent S_CLAMP
	// for the whole lifetime of its subscription: the first poll reported
	// the code, and every later one compared equal to it and so counted
	// as unchanged.
	case resultID.IsError():
		changed = resultID != it.lastResultID
		u = update{resultID: resultID, diagnosticInfo: diagnosticInfo}
	default:
		// Recovering from a reported condition is always a change, even
		// if the value happens to be identical to the last good one.
		// A changed result code is a change in its own right — recovering
		// from an E_ condition, but also moving between S_ codes or in and
		// out of one, which the client must be told about even when the
		// value itself compares equal.
		changed = resultID != it.lastResultID || !it.haveLast ||
			sampleChanged(it.last, sample, it.deadband)
		if changed {
			// it.last is the last value REPORTED to the client, not the
			// last value read — that is what a deadband is defined
			// against. Advancing it on a suppressed reading turned the
			// deadband into a rate-of-change filter: with a 10% deadband
			// and 5%-per-poll drift, every single step compares against
			// the previous reading, stays under the band, and the value
			// walks from 100 to 200 without the client ever being told.
			//
			// With no deadband this is behaviorally identical to the
			// unconditional assignment, since "unchanged" then means the
			// values compare equal anyway.
			it.last = sample
		}
		it.haveLast = true
		u = update{sample: sample, haveSample: true, resultID: resultID, diagnosticInfo: diagnosticInfo}
	}
	it.lastResultID = resultID
	it.lastDiagnosticInfo = diagnosticInfo
	if !changed {
		return false
	}
	switch {
	case !it.enableBuffering:
		it.buffer = []update{u} // only the latest value; outside the budget
	case budget.acquire():
		it.buffer = append(it.buffer, u)
		if over := len(it.buffer) - maxBuffer; over > 0 {
			// Oldest purged first; the Latest Changed Value (the most
			// recent entry) is always retained (REQ-SUBSCRIPTION-007).
			it.buffer = it.buffer[over:]
			budget.release(int64(over))
			it.overflowed = true
			if metrics != nil {
				metrics.IncDroppedSamples(over)
			}
		}
	default:
		// The server-wide buffered-sample budget is exhausted
		// (Config.MaxTotalBufferedSamples). Collapse to the Latest Changed
		// Value — the one entry REQ-SUBSCRIPTION-007 retains regardless of
		// any limit — and flag the loss so the next reply carries
		// DataBufferOverflow. Degrading this item rather than refusing the
		// update keeps change delivery working under memory pressure; the
		// alternative is a subscription that silently goes stale.
		switch n := int64(len(it.buffer)); {
		case n > 1:
			budget.release(n - 1)
		case n == 0:
			budget.add(1)
		}
		if metrics != nil && len(it.buffer) > 0 {
			metrics.IncDroppedSamples(len(it.buffer))
		}
		it.buffer = []update{u}
		it.overflowed = true
	}
	return true
}

// arrayExceedsDeadband applies the deadband element-wise to two numeric
// arrays of the same shape, reporting whether any element crossed it. The
// second return value is false when this comparison does not apply — the
// values are not both numeric arrays, or their lengths differ, in which
// case a changed shape is a change in its own right and the caller's
// ordinary comparison decides.
func arrayExceedsDeadband(prev, next xmlda.Value, deadbandPct float64) (changed, applicable bool) {
	pa, err := prev.Array()
	if err != nil {
		return false, false
	}
	na, err := next.Array()
	if err != nil {
		return false, false
	}
	if pa.ElemType() != na.ElemType() || pa.Len() != na.Len() {
		return false, false
	}
	pv, pok := pa.NumericFloat64s()
	nv, nok := na.NumericFloat64s()
	if !pok || !nok {
		return false, false
	}
	for i := range pv {
		if scalarExceedsDeadband(pv[i], nv[i], deadbandPct) {
			return true, true
		}
	}
	return false, true
}

// scalarExceedsDeadband is the single-value deadband rule, shared by the
// scalar and the element-wise array comparison so the two can never
// disagree.
func scalarExceedsDeadband(pf, nf, deadbandPct float64) bool {
	pNaN, nNaN := math.IsNaN(pf), math.IsNaN(nf)
	if pNaN || nNaN {
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
		// §3.5.1: "The deadband will also apply to array types. The entire
		// array is returned if any array element exceeds the deadband
		// threshold." Falling through to a plain equality comparison for
		// arrays meant a deadbanded client got every single element change
		// — the exact flood it had asked the server to suppress.
		if changed, ok := arrayExceedsDeadband(prev.Value, next.Value, deadbandPct); ok {
			return changed
		}
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
