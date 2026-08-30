# Limitations

Known gaps, tracked rather than hidden. None of these are silent — each is either an open question
documented in [`docs/specification/open-questions.md`](specification/open-questions.md), a tracked task in
[`docs/development/tasks.md`](development/tasks.md), or a row in
[`docs/specification/traceability-matrix.md`](specification/traceability-matrix.md) marked `implemented` or
`in progress` rather than `tested`.

## Verification gaps in this development environment

- **`go test -race` has now been run successfully** (2026-08-24), once a C toolchain (`gcc` 16.2.0, via
  `C:\Tools\winlibs\bin`) became available on `PATH` with `CGO_ENABLED=1`. `go test -race ./...` is clean
  across all 9 packages, and `go test -race -count=5 ./subscription/... ./server/...` (the two
  concurrency-sensitive packages) is stable with no flakiness. This was previously blocked for lack of a C
  compiler; the concurrency-sensitive packages had been covered in the meantime by table-driven lifecycle
  tests using a fake, controllable clock (`clock/clocktest.Fake`) with no real sleeps, a goroutine-leak check
  after shutdown (`subscription/shutdown_test.go`), and two stress tests using the real clock and real
  concurrency (`subscription/stress_test.go`) — those remain in place as regression coverage, but `-race`
  itself is no longer an outstanding verification gap.
- **Unbounded `go test -fuzz` has not been run** in this sandbox (subprocess spawning for fuzz workers is
  restricted here). The fuzz tests' seed corpora do run via plain `go test`, exercising the same code paths
  without the fuzzing engine's exploration.
- **No official OPC Foundation conformance test suite has been run** against this library. Nothing in this
  repository should be read as a conformance claim — see `docs/protocol-support.md` for the precise,
  per-operation implemented/tested status instead. Since 2026-08-30 every response this server produces *is*
  validated against the specification's own schema (transcribed into `testdata/schema/opcxmlda.xsd`) by
  `xmllint` in CI, which is a real external check but still narrower than a conformance suite: it verifies
  document structure, not protocol behavior over time.

## Specification areas with a documented, deliberately conservative interpretation

These are not bugs — they are documented decisions where the specification was ambiguous, silent, or
internally inconsistent. Full rationale for each is in `docs/specification/open-questions.md`.

- **Deadband is measured as a percentage of the last reported value, not of the item's engineering-unit
  range.** The specification defines it against the EU range (highEU/lowEU), which is an item *property* the
  subscription layer has no access to. The comparison is against the last value actually reported to the
  client — not the last value read — so a value drifting by just under the band on every poll still
  eventually crosses it, rather than walking away unnoticed.
- **Deadband + buffer overflow interaction** (OQ-4): the specification itself flags that a deadband-filtered
  sample which was nonetheless buffered can still be purged under buffer pressure, potentially leaving only
  the final in-deadband value — described in the spec as a caveat, not resolved. This library implements
  best-effort fidelity (oldest-purged-first, the latest changed value per item always retained), not
  perfect device-polling fidelity.
- **`SubscriptionPolledRefresh`'s sampled-vs-exception-based timing rules** (spec §2.5.5, tracked as
  REQ-SUBSCRIPTION-012, status `in progress`): general timestamp propagation is implemented and exercised,
  but the specification's more elaborate rules for exactly when a sampled-mode vs. exception-mode timestamp
  should be taken are not separately tested against that specific rule set.
- **`RequestDeadline` elapsing mid-processing**: only a deadline already past *at receipt* is enforced (as
  a whole-operation `E_TIMEDOUT` fault). The specification's other case — the deadline elapsing while a
  request is still being processed, which would then produce per-item `E_TIMEDOUT` results for whatever
  wasn't yet handled — is not separately implemented, because this library's request handling is
  synchronous per call, making that window negligible in practice.
- **Namespace prefix resolution uses one flat, whole-document table** per decoded document (OQ-6), not a
  fully correct nested-XML-namespace-scope resolver. "Last declaration wins" if the same prefix is
  redeclared at multiple depths — a case not observed in any real captured traffic this library was built
  against, but theoretically possible in a document this library has not seen.
- **SOAP Fault `QName` resolution is element-local, not whole-document** (OQ-13): a fault code's namespace
  prefix must be declared on the `<faultcode>`/`<Code>` element itself (matching the specification's own
  worked example) — a prefix declared only on a remote ancestor (e.g. the `Envelope` root) will not resolve.
  None of the three real captured fault fixtures under `testdata/faults/` are affected by this.

## Behaviors that are the backend's responsibility, not this library's

This library threads several fields through to the backend without independently validating or testing
their *semantics*, because only the backend can know what they mean for its data source:

- `ReadRequestItem.MaxAge` (REQ-READ-004) — "how stale a cached value may be" is entirely backend-defined.
- `Write`'s atomic Value+Quality+Timestamp application (REQ-WRITE-003) — the *contract* is documented and
  the field plumbing is implemented, but this library cannot verify a third-party backend actually applies
  all three atomically; see `docs/backend-implementation.md`.
- `Browse` filter application (REQ-BROWSE-004) — the server validates continuation points but passes
  `BrowseFilter`/`ElementNameFilter`/`VendorFilter` through to the backend unchanged; whether the backend's
  results actually honor them is not this library's concern.

## Scale/performance notes (not defects)

- **Push-mode subscriptions cost one live goroutine per subscription** (the drain loop reading from the
  backend's `ChangeNotifier` channel) — a documented, inherent trade-off of push efficiency, not hidden and
  not a leak (each is bound to its subscription's own cancellable context). Poll-mode subscriptions cost
  effectively zero idle goroutines (a shared, self-rescheduling timer chain), so a server expecting very
  large subscription counts on a push-capable backend should account for this difference when sizing.
- **Deadband is applied against the last reported value** — see the entry above; the poll chain itself
  schedules each tick relative to when the previous one was *due*, not to when its backend call returned, so
  a slow backend no longer stretches every item's real sampling interval past the `RevisedSamplingRate` the
  client was told.
- Numeric `ReqType` coercion (`Read` and `Subscribe`) supports a **numeric-to-numeric subset only**, with
  explicit range checking. String/array/unknown-type coercion is deliberately unsupported (`E_BADTYPE`)
  rather than attempted best-effort — see `docs/protocol-support.md`. A `ReqType` outside the XSD namespace
  is `E_BADTYPE` too: the namespace is part of a QName's identity, so a vendor type that merely shares a
  local name with an XSD one is not that type.
- **The per-axis subscription limits multiply, so there is also a server-wide one.**
  `MaxConcurrentSubscriptions` (10000) and `MaxItemsPerSubscription` (1000) together would permit ten million
  live items, each holding its own last sample and up to `MaxBufferedSamplesPerItem` buffered ones.
  `Config.MaxTotalSubscribedItems` (200000) bounds the product; a deployment expecting either axis near its
  maximum should size all three together rather than any one in isolation.

## Not implemented

- WSDL generation/discovery.
- Any transport other than HTTP(S) POST with a SOAP-enveloped body.
- OPC Alarms & Events (A&E) — out of scope for this library.
