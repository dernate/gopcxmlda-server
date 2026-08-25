# Subscription Model

Covers `subscription.Manager` — the most concurrency-sensitive component in this library. See ADR-007/008/009
for the alternatives considered.

## Storage

```go
type Manager struct {
    mu         sync.RWMutex     // guards subs map only — brief hold
    subs       map[Handle]*subState
    cfg        Config
    clock      clock.Clock
    backend    backend.Backend
    log        telemetry.Logger
    metrics    telemetry.Metrics
    rootCtx    context.Context
    rootCancel context.CancelFunc
    wg         sync.WaitGroup   // tracks every long-lived background goroutine
}

type subState struct {
    mu                 sync.Mutex // guards everything below
    handle             Handle
    ctx                context.Context
    cancel             context.CancelFunc
    items              []itemState
    pingRate           time.Duration
    lastPolledAt       time.Time
    busy               int32 // atomic CAS guard for E_BUSY (REQ-SUBSCRIPTION-009)
    changedCh          chan struct{} // closed+replaced to broadcast "new data" to waiters
    dataBufferOverflow bool
}
```

A plain map under one `RWMutex` (not `sync.Map`, not sharding — ADR-007): the workload is write-heavy
(constant Create/Cancel), needs an atomic "reject if over `MaxConcurrentSubscriptions`" check at insert time,
and needs to iterate-and-delete for the reaper — none of which maps cleanly onto `sync.Map`'s API, and the
expected scale (hundreds to low thousands of subscriptions) doesn't need sharding's contention relief.

## Goroutine model

**No goroutine for an idle poll-mode subscription.** Each poll-mode subscription uses a self-rescheduling
`clock.Clock.AfterFunc` timer chain — the Go runtime's own timer wheel already is an efficient shared
scheduler, reachable through the `Clock` seam so it stays swappable for deterministic tests:

```go
func (m *Manager) scheduleNextPoll(s *subState, in time.Duration) {
    m.clock.AfterFunc(in, func() {
        if s.ctx.Err() != nil {
            return // cancelled/shutdown: stop the chain here — this IS the cleanup path
        }
        m.pollOnceBounded(s) // acquires m.pollSem before calling backend.Reader.Read
        if s.ctx.Err() == nil {
            m.scheduleNextPoll(s, s.samplingRate())
        }
    })
}
```

The actual backend call is gated by `pollSem chan struct{}` (buffered, size `Config.MaxConcurrentPolls`) so
many subscriptions firing at once cannot spawn unbounded concurrent backend calls.

| Approach | Idle goroutines | Verdict |
|---|---|---|
| Goroutine + `time.Ticker` per subscription | O(N), permanently blocked | Rejected — violates "many subscriptions ≠ many permanent goroutines" |
| Hand-rolled heap + dispatcher + worker pool | O(1) | Rejected — reimplements what the runtime timer wheel already provides |
| `Clock.AfterFunc` self-rescheduling + bounded semaphore | ~0 idle, ≤`MaxConcurrentPolls` while firing | **Adopted** |

**Push-mode exception, stated explicitly**: a subscription whose backend implements `ChangeNotifier` and is
actively used in push mode costs one always-alive goroutine for the duration of the subscription (something
must drain the backend's async channel). This is proportional to the number of subscriptions actually using
the efficient push path, not to total subscription count, and is a deliberate, documented trade-off of push
efficiency — not a leak.

## `SubscriptionPolledRefresh` Hold+Wait blocking

```go
func (m *Manager) notifyChanged(s *subState) {
    s.mu.Lock()
    old := s.changedCh
    s.changedCh = make(chan struct{})
    s.mu.Unlock()
    close(old) // broadcasts to every current waiter
}
```

For a call covering handles `[H1..Hn]`:

1. Non-blocking `atomic.CompareAndSwapInt32(&s.busy, 0, 1)` per handle. Any failure → release whatever was
   acquired, return `E_BUSY` immediately, no waiting (REQ-SUBSCRIPTION-009).
2. `defer` unconditional release of every acquired busy flag (covers normal return, cancellation, panic).
3. Compute the effective deadline from `HoldTime`/`WaitTime`/`RequestDeadline` using `clock.Clock` — never
   `time.Now()`/real sleeps.
4. `select` over: a `Clock`-driven deadline timer; an aggregate change-signal fan-in (one short-lived
   goroutine per requested handle selecting on that handle's `changedCh`/`ctx.Done()`, forwarding to one
   channel); any requested handle's own `ctx.Done()` (subscription cancelled — REQ-SUBSCRIPTION-010); the
   HTTP request's own `ctx.Done()`.
5. Whichever fires first determines the response: deadline → snapshot per `ReturnAllItems`
   (REQ-SUBSCRIPTION-006); change signal → early return with changed items (REQ-SUBSCRIPTION-005); a
   handle's cancellation → immediate return with whatever remains for the other handles
   (REQ-SUBSCRIPTION-010); request cancellation → caller-cancelled/`E_TIMEDOUT`.

The per-call fan-in goroutines are bounded by (and exit with) the HTTP request's own context — no
independent lifetime, nothing to leak.

## Cancellation and shutdown

`SubscriptionCancel` and the reaper both funnel through one `Manager.terminate(handle, reason)`: remove from
`m.subs` under `Manager.mu.Lock()`, then `s.cancel()` — which alone stops the poll chain from rescheduling,
makes a push-mode drain loop's `ctx.Done()` fire, and is one of the fan-in cases in every currently-blocked
`PolledRefresh` call touching that handle (REQ-SUBSCRIPTION-010, REQ-SUBSCRIPTION-011).

**Double/unknown-handle cancellation (REQ-SUBSCRIPTION-014)**: `Manager.Cancel(handle)` looks the handle up
and, whether or not it is found, returns success — `SubscriptionCancelResponse` has no error slot to report
"unknown handle" in anyway (§3.7, p.68), and this makes a benign race between a client's own cancel and an
independent reaper sweep for the same handle safe by construction rather than by special-cased detection.
This was flagged during the Phase 4 self-review (`docs/development/plan-review.md`) — see
`docs/specification/open-questions.md` OQ-9.

Server shutdown:

```go
func (m *Manager) Shutdown(ctx context.Context) error {
    m.rootCancel()       // wakes every blocked PolledRefresh + stops all poll/push chains immediately
    return m.Wait(ctx)   // bounded wait for background goroutines to actually exit
}
```

`m.wg.Wait()` is raced against the caller's deadline (`sync.WaitGroup`, not `errgroup` — these background
goroutines don't produce errors worth aggregating, only completion). `server.Server.Shutdown` calls
`subMgr.BeginShutdown()` *before* `http.Server.Shutdown`, specifically so in-flight long-poll handlers are
already unblocked before the HTTP server starts waiting for them — otherwise shutdown could hang for up to
the full client-requested Hold+Wait duration.

## Abandonment reaper (REQ-SUBSCRIPTION-013)

A self-rescheduling sweep (same `Clock.AfterFunc` idiom as polling, not a permanent loop), waking every
`Config.ReapInterval` (implementation default). For each subscription where
`clock.Now().Sub(lastPolledAt) > pingRate * Config.ReapGraceMultiplier` (both implementation defaults —
the spec gives no numeric guidance), calls `terminate(handle, reasonAbandoned)`. Lazy/on-demand checking
alone is insufficient: a truly abandoned subscription has no future request to trigger a check, so its
resources (including a live push-drain goroutine) would otherwise never be freed.

**`pingRate` here is always the already-resolved, nonzero value (REQ-SUBSCRIPTION-015)**: the spec defines
`SubscriptionPingRate=0` on a Subscribe request as "use the server's own algorithm" (§3.5.1, p.57), not a
literal zero-duration interval. `Manager.Create` substitutes `Config.DefaultSubscriptionPingRate`
(implementation default, e.g. 60s) whenever the client sends `0`, once, at creation time, and stores the
resolved value in `subState.pingRate` — so the reaper formula above never sees a raw `0` and never reaps a
brand-new subscription immediately. This was caught during the Phase 4 self-review (see
`docs/development/plan-review.md` and `docs/specification/open-questions.md` OQ-10) before any code was
written — a real bug the formula as first drafted would otherwise have had.

## Bounded worst-case goroutine count

```
G_total ≤ 1                                    (reaper, only during its brief sweep)
        + P                                     (# push-mode subscriptions — inherent, documented)
        + MaxConcurrentPolls                    (poll callbacks actually executing right now)
        + Σ over in-flight PolledRefresh calls(handles in that call)   (fan-in goroutines,
                                                  bounded by concurrent HTTP requests, self-cleaning)
```

Idle poll-mode subscriptions contribute zero goroutines — the bound is proportional to active work, not to
total subscription count, directly satisfying the requirement that a large number of subscriptions must not
translate into a large number of permanently-running goroutines.

## Buffering and deadband (REQ-SUBSCRIPTION-007)

Each item's buffer is bounded; on overflow, oldest entries are purged first, while the Latest Changed Value
is always retained (never purged) so a client never loses the most recent value even under sustained
overflow. `DataBufferOverflow` (response-level) and `S_DATAQUEUEOVERFLOW` (item-level) both signal this.
Deadband filtering happens before buffering (a change that doesn't exceed the deadband threshold is never
buffered in the first place for that item); the spec's own noted caveat that a buffered-but-deadband-adjacent
value can still be purged under pressure is accepted as a documented limitation (`docs/limitations.md`), not
solved with additional complexity.

## Clock abstraction

```go
type Clock interface {
    Now() time.Time
    After(d time.Duration) <-chan time.Time
    NewTimer(d time.Duration) Timer
    AfterFunc(d time.Duration, f func()) Timer
    Sleep(d time.Duration)
}
type Timer interface {
    Stop() bool
    Reset(d time.Duration) bool
    C() <-chan time.Time
}
```

`clock.Real` wraps the stdlib directly (zero behavioral change). `clock/clocktest.Fake` exposes
`Advance(d)`/`Set(t)`, synchronously firing every due entry — a test drives `PolledRefresh` from a goroutine,
then calls `fake.Advance(holdTime)` and deterministically asserts the call returns, with no real sleep
anywhere. Hand-rolled rather than an external dependency (ADR-009): the interface is small, and this is the
most correctness-sensitive, timing-dependent code in the library, worth owning outright.
