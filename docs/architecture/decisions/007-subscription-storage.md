# ADR-007: Subscription storage — plain map + two-level mutex, background reaper

## Status
Accepted

## Context
`subscription.Manager` needs concurrent-safe storage for potentially hundreds to low-thousands of active
subscriptions, with frequent Create/Cancel churn, an atomic "reject if over `MaxConcurrentSubscriptions`"
check, and a periodic sweep for abandoned subscriptions (REQ-SUBSCRIPTION-013).

## Decision
`map[Handle]*subState` guarded by one `sync.RWMutex` for map-level operations (insert/lookup/delete — held
briefly; `RLock` for lookups), with each `subState` additionally guarded by its own `sync.Mutex` for that
subscription's private mutable state. A background reaper (self-rescheduling, see ADR-008's timer idiom)
handles abandonment cleanup; lazy/on-demand checking alone is not used as the sole mechanism.

## Alternatives considered
- **`sync.Map`**: rejected — optimized for read-mostly, rarely-mutated key sets; this workload is
  write-heavy (continuous create/cancel) and needs atomic limit-checking and iterate-and-delete, neither of
  which fits `sync.Map`'s API well, and its concurrency benefit doesn't materialize at the expected scale.
- **Sharded maps**: rejected — solves a lock-contention problem this workload doesn't have; the outer lock is
  held only for map operations, microseconds at most, at plausible subscription counts.
- **Lazy/on-demand-only cleanup (no reaper)**: rejected as the sole mechanism — a client that simply
  disappears never triggers a future request that could lazily notice and free its subscription; a
  background sweep is necessary to reclaim resources (including a live push-mode drain goroutine) from a
  truly abandoned subscription.

## Consequences
- Correctness and simple reasoning about atomic limit checks and `E_BUSY` win over micro-optimized
  concurrent-map throughput, appropriate at the expected scale; revisit only if profiling shows contention.
- The reaper's `Config.ReapInterval`/`Config.ReapGraceMultiplier` are implementation defaults (ADR-011), not
  spec-mandated.
