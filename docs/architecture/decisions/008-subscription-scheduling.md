# ADR-008: Self-rescheduling timers + bounded semaphore, not goroutine-per-subscription

## Status
Accepted

## Context
Poll-mode subscriptions (backend has no `ChangeNotifier`) need periodic re-checking at each subscription's
negotiated sampling rate. The project directive explicitly requires that a large number of subscriptions
must not translate into a large number of permanently-running goroutines.

## Decision
Each poll-mode subscription uses a self-rescheduling `clock.Clock.AfterFunc` timer chain — the callback
checks for cancellation, performs (at most) one bounded backend call, then reschedules itself — gated by a
small fixed-size semaphore (`Config.MaxConcurrentPolls`) around the actual backend call to bound worst-case
concurrency if many subscriptions happen to fire at once.

## Alternatives considered
- **One goroutine + `time.Ticker` per subscription**: rejected outright — O(N) permanently-blocked
  goroutines, directly violating the stated non-functional requirement.
- **Hand-rolled priority-queue/heap + dispatcher + worker pool**: rejected — would reimplement
  functionality the Go runtime's own timer wheel already provides efficiently; higher complexity (custom
  heap, wake channel, worker fan-out) for no measurable benefit over routing through `clock.Clock.AfterFunc`.

| Approach | Idle goroutines | Complexity | Verdict |
|---|---|---|---|
| Ticker-per-subscription | O(N), permanently blocked | Trivial | Rejected |
| Hand-rolled heap + dispatcher + workers | O(1) | High | Rejected |
| `Clock.AfterFunc` self-reschedule + bounded semaphore | ~0 idle, ≤`MaxConcurrentPolls` while firing | Low-moderate | **Adopted** |

## Consequences
- Idle poll-mode subscriptions contribute zero goroutines; the worst-case goroutine count is bounded by
  active work (see `docs/architecture/subscription-model.md`'s explicit formula), not by total subscription
  count.
- Push-mode subscriptions (backend implements `ChangeNotifier`) still cost one live drain goroutine each —
  documented explicitly as an inherent, accepted cost of push efficiency, not hidden or treated as a bug.
- Routing every timer through `clock.Clock` (ADR-009) rather than calling `time.AfterFunc` directly is what
  keeps this scheduling logic deterministically testable.
