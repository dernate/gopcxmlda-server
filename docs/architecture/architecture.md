# Architecture

This document describes the architecture of `gopcxmlda-server`, a Go base library for building OPC XML-DA
1.0 servers. It synthesizes two independent design passes (protocol/wire layer; backend/subscription/server
layer) performed against the requirements in `docs/specification/requirements.md`, reconciled into one
coherent design. Individual decisions with alternatives and rationale are recorded as ADRs under
`docs/architecture/decisions/`; this document describes the resulting shape.

## Layers and responsibilities

```
 HTTP transport            server.Server (net/http wrapper, optional)
        │
 Request dispatch          server.Handler (http.Handler): body limits, timeouts,
        │                  body-based dispatch (Content-Type/SOAPAction are not
        │                  validated — see docs/architecture/data-flow.md)
        │
 SOAP envelope              soap.Envelope[T] / soap.Fault
        │
 OPC XML-DA protocol        xmlda: the 8 operations' request/response structs,
        │                  Value, OPCQuality, ErrorCode/Errors, ReplyBase,
        │                  RequestOptions, ItemParams, ItemProperty, dispatch
        │
 Orchestration              server.Handler: validates against Config limits,
        │                  translates between xmlda types and backend calls,
        │                  applies ResolveValuePresence/DedupeErrors
        │
 Subscription lifecycle     subscription.Manager: storage, poll/push scheduling,
        │                  Hold+Wait blocking, buffering/deadband, reaper
        │
 Backend contract           backend: StatusProvider, Reader, Writer, Browser,
        │                  PropertyReader, optional ChangeNotifier
        │
 Application data source    (supplied by the application embedding this library —
                             not part of this library)
```

Supporting leaf packages: `clock` (time abstraction for deterministic subscription tests),
`telemetry` (Logger/Metrics interfaces, no-op by default).

## Public API

The library's public surface is intentionally small:

- `backend.Backend` (struct of interfaces) — what an application implements to plug in its process-data
  source. This is the primary extension point.
- `server.New(deps, cfg) (*server.Handler, error)` — construct an `http.Handler` an application can mount
  anywhere (its own router, TLS termination, auth middleware).
- `server.NewServer(addr, deps, cfg) (*server.Server, error)` plus `Start`/`Shutdown` — an optional
  convenience wrapper for applications that don't want to assemble their own `net/http.Server`.
- `xmlda` types (`Value`, `OPCQuality`, `ErrorCode`, and the per-operation request/response structs) are
  exported because backends construct/consume them (e.g. a `Reader` returns `backend.ItemSample{Value
  xmlda.Value, ...}`) — but a backend author never touches SOAP envelope or dispatch machinery.
- `clock.Clock` — exported so an application that wants deterministic subscription behavior in its own
  tests can inject a fake clock the same way this library's own tests do.

See `docs/architecture/public-api.md` for the full surface and `docs/architecture/decisions/006-*.md` for why
there is no separate "friendly DTO" layer distinct from the wire model.

## Package structure

See `docs/architecture/package-structure.md` for the full rationale. Summary: `soap` (generic SOAP envelope
+ fault, no OPC vocabulary) and `xmlda` (OPC XML-DA vocabulary + the 8 operations + dispatch) form the
protocol layer. `backend` (small composable interfaces), `clock`/`clock/clocktest`, and `telemetry` are
independent, protocol-agnostic leaves. `subscription` depends on `backend`, `clock`, `telemetry`, and
`xmlda`'s vocabulary types (not its full operation-struct surface, though nothing prevents the import —
see ADR-006 on why a hard split wasn't worth it). `server` is the only package that imports `soap` and the
full `xmlda` surface, and is where HTTP, protocol, and backend concerns meet — deliberately, so those three
concerns stay decoupled from each other everywhere else.

## Central interfaces and structs

- `backend.Backend{Status, Reader, Writer, Browser, Properties}` — composition struct; `Writer`/`Browser`/
  `Properties` are `nil`-able (feature detection via nil-check, not sentinel errors); `ChangeNotifier` is
  type-asserted off `Reader`.
- `backend.Result[T]{Value T, ResultID xmlda.ErrorCode, DiagnosticInfo string}` — the one shape every
  per-item backend outcome uses; `ResultID` zero value means no abnormal condition.
- `xmlda.Value` — generic wire-value container; see `docs/specification/type-mapping.md` for the full
  scalar/array table and `docs/architecture/decisions/002-*.md`/`003-*.md` for the representation rationale.
- `xmlda.ErrorCode{QName}` — the single result/error-code type used by both the wire layer and the backend
  contract (this is the one explicit reconciliation between the two original design passes: the backend
  design initially sketched a bare `ResultID string`, superseded by this QName-based type so vendor codes in
  vendor namespaces round-trip correctly without a parallel representation).
- `subscription.Manager` — owns all subscription state; see `docs/architecture/subscription-model.md`.
- `server.Handler`/`server.Server`/`server.Config`/`server.Deps` — HTTP-facing composition root.

## Server lifecycle

1. Application constructs a `backend.Backend`, optionally a `clock.Clock`/`telemetry.Logger`/
   `telemetry.Metrics`, and a `server.Config` (or accepts defaults).
2. `server.New`/`server.NewServer` validates the backend (`Backend.Validate()`— `Status`/`Reader` required),
   fills `Config` defaults, constructs a `subscription.Manager` bound to the backend and clock.
3. `Server.Start()` begins serving; `Handler` is stateless per-request except for delegating to the shared
   `subscription.Manager`.
4. `Server.Shutdown(ctx)`: `subscription.Manager.BeginShutdown()` cancels the manager's root context first
   (unblocking every in-flight long-poll `SubscriptionPolledRefresh` immediately), *then*
   `http.Server.Shutdown` stops accepting connections and waits for in-flight handlers (now fast, since
   they're unblocked), then `subscription.Manager.Wait(ctx)` bounds how long we wait for background
   goroutines (push-mode drains, reaper) to fully exit. See `docs/architecture/subscription-model.md` for why
   this ordering matters.

## Subscription lifecycle

Summarized here; full detail in `docs/architecture/subscription-model.md`. Subscribe validates items via
`backend.Reader.Read` and creates a `subState` if at least one item is valid. Ongoing refresh is either
push-mode (backend implements `ChangeNotifier`: one drain goroutine per active push subscription) or
poll-mode (self-rescheduling `Clock.AfterFunc` chain gated by a bounded semaphore — no goroutine while idle).
`SubscriptionPolledRefresh` blocks via a `select` over a per-subscription change-broadcast channel, the
request's own deadline, and every requested handle's cancellation context — the same cancellation mechanism
serves normal Hold+Wait expiry, mid-hold cancellation, and server shutdown. An abandonment reaper (same
self-rescheduling idiom) frees subscriptions the client stopped polling.

## Request/response data flow

Full detail in `docs/architecture/data-flow.md`. Summary: `server.Handler` reads a size-limited body,
identifies the operation via `xmlda.IdentifyOperation` (three failure buckets: malformed XML, unknown
operation, malformed-but-recognized operation — see `docs/specification/error-mapping.md`), decodes into the
matching typed request struct, validates against `Config` limits, translates into backend calls (via
`subscription.Manager` for the 3 subscription operations, directly for the other 5), translates backend
results back into `xmlda` response structs (applying `ResolveValuePresence` and `DedupeErrors`), and encodes
the response through `soap.Envelope`.

## Concurrency model

- Per-request state is request-scoped; the only shared mutable state is `subscription.Manager`.
- `Manager` uses a `sync.RWMutex` for map operations (brief hold) plus a per-subscription `sync.Mutex` for
  that subscription's own state — see ADR-007.
- No goroutine exists for an idle poll-mode subscription; goroutines exist only while: a poll callback is
  actually executing (bounded by a semaphore), a push-mode subscription's drain loop is alive (one per active
  push subscription — an accepted, documented cost of push efficiency), or a `SubscriptionPolledRefresh`
  call is in-flight (self-cleaning, bounded by concurrent HTTP requests) — see ADR-008 and
  `docs/architecture/subscription-model.md` for the explicit worst-case goroutine-count formula.
- All time-based logic (Hold/Wait/PingRate/reaper) goes through `clock.Clock`, never `time.Now()`/
  `time.Sleep()` directly — see ADR-009 — enabling deterministic, no-real-sleep tests.

## Error handling

Two independent channels map 1:1 onto two independent Go shapes: a whole-operation `error` from any backend
call (or a `RequiresFault`-triggered condition from `ServerState`) always becomes a SOAP `Fault`; a per-item
`backend.Result[T].ResultID` always becomes that item's `ResultID` plus a deduplicated `Errors` entry. See
`docs/specification/error-mapping.md` and ADR-005.

## Extension points

- `backend.Backend` — the primary extension point; every field beyond `Status`/`Reader` is optional, and
  `ChangeNotifier` is an opportunistic enhancement, not a requirement.
- `clock.Clock` — swappable for tests or for an application with unusual timing requirements.
- `telemetry.Logger`/`telemetry.Metrics` — swappable for any logging/metrics stack; `*slog.Logger` satisfies
  `telemetry.Logger` with zero adapter code.
- `server.Config` — every resource limit is a field with a documented default, never hard-coded.
- TLS and authentication are explicitly **not** extension points inside this library — they belong above it
  (the application's own `http.Server`/middleware/reverse proxy), per REQ-SECURITY-001 and ADR-010.

## Test strategy (summary; full detail in `docs/architecture/testing-strategy.md`)

Golden-file and round-trip tests for the wire layer against real captured fixtures (`testdata/`); table-driven
unit tests for `Value`/type coercion/quality rules; fuzz tests for the QName-prefix resolver and numeric
boundary decoding; deterministic (fake-clock) subscription lifecycle tests; `-race` concurrency stress tests
plus goroutine-leak checks after shutdown; `httptest`-based server tests including partial-success and fault
scenarios; an in-memory reference backend powering both the example server and end-to-end tests.

## Security boundaries

- Request bodies are read through `http.MaxBytesReader` (`Config.MaxRequestBodyBytes`) before any XML
  parsing begins.
- Item-list sizes are checked against `Config.MaxItemsPerRequest`/`MaxItemsPerSubscription` before backend
  calls are made.
- `Config.ReadOnly` provides the spec-recommended global write-disable switch (REQ-SECURITY-002); when set
  (or when `backend.Writer` is `nil`), every Write item resolves to `xmlda.ErrAccessDenied` without invoking
  any backend code.
- No SOAP request/response body, and no item value, is logged by default; `telemetry.Logger` calls in this
  library's own code carry operation name, handle IDs, item counts, and durations only.
- TLS and authentication are the responsibility of the code that constructs the `http.Server`/mounts the
  `http.Handler` — this library never hard-codes a specific mechanism (REQ-SECURITY-001).
