# ADR-009: A small hand-rolled Clock abstraction, not stdlib time directly or an external dependency

## Status
Accepted

## Context
Subscription Hold+Wait blocking, sampling-rate scheduling, and abandonment-reaper sweeps are the most
correctness-sensitive, timing-dependent logic in this library (REQ-SUBSCRIPTION-005/006/013). Testing them
against real wall-clock time would make tests slow and flaky (real sleeps proportional to `HoldTime`/
`WaitTime`/`PingRate`).

## Decision
A small `Clock`/`Timer` interface (`Now`, `After`, `NewTimer`, `AfterFunc`, `Sleep` / `Stop`, `Reset`, `C`),
with `clock.Real` wrapping the stdlib directly (zero behavioral change, zero overhead) and
`clock/clocktest.Fake` (a hand-rolled fake, kept in its own sub-package so production code never imports it)
exposing `Advance(d)`/`Set(t)` for deterministic test control. Every Hold/Wait/PingRate/reaper computation in
`subscription` goes through this interface exclusively.

## Alternatives considered
- **No abstraction — call `time.*` directly, accept real-sleep tests**: rejected — directly conflicts with
  the project directive that subscription tests must be deterministic and not rely on long real sleeps.
- **Depend on an external fake-clock library** (e.g. `github.com/benbjohnson/clock`): rejected in favor of
  owning a small, purpose-shaped interface outright — the surface needed is small (~150 LOC for the fake),
  this is the single most correctness-critical piece of timing logic in the library, and the interface shape
  chosen is intentionally close to that library's own, so adopting it later costs nothing if ever desired.

## Consequences
- Subscription lifecycle tests advance virtual time explicitly and assert deterministically, with zero real
  sleeps (see `docs/architecture/testing-strategy.md`, item 9).
- An application embedding this library can inject its own fake clock via `server.Deps.Clock` for its own
  deterministic tests, using the same mechanism this library uses internally.
