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
  per-operation implemented/tested status instead. Since 2026-08-30 the responses of all eight
  operations *are* validated against the specification's own schema (transcribed into
  `testdata/schema/opcxmlda.xsd`) by `xmllint` in CI. Read that precisely: it is **nine frozen golden
  documents**, one per operation plus a fault — not every response the server can produce. Shapes those
  nine do not contain (a nil value, a vendor `xsi:type`, a buffered subscription reply carrying several
  samples for one item) are covered only by Go-side round-trip tests, and Go's `encoding/xml` is lenient
  in exactly the places a conforming parser is not. `testdata/invalid/` is also still empty: no test
  asserts that malformed output is *rejected* by the schema, so the validation proves the golden
  documents pass, not that the check would catch a regression in an untested shape.

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
- **Namespace prefix resolution is element-local first, then one flat whole-document table** (OQ-6), not a
  fully correct nested-XML-namespace-scope resolver. A prefix declared on the element the QName itself came
  from — the shape the specification's own worked example (§2.6 p.21) and this library's own encoder both
  produce — now resolves correctly even when the same prefix is bound differently elsewhere in the
  document. What remains unhandled is a prefix declared only on an *ancestor* and rebound at two different
  depths: those still resolve "last declaration wins" against the flat table. Making that fully correct
  means a hand-written token-stream decoder for the whole package rather than `encoding/xml` struct
  decoding (ADR-001), which is not a trade this library takes for a case no captured traffic exhibits.
- **A malformed request ITEM is that item's condition, not the request's.** An item whose own attributes
  or `<Value>` this server cannot interpret (a non-numeric `MaxAge`, an unresolvable `ReqType` prefix, a
  literal that contradicts its declared `xsi:type`) resolves to that one item's `ResultID` — `E_BADTYPE`
  where the problem is a type, `E_FAIL` otherwise, with the offending field named in `DiagnosticInfo` — and
  every other item in the request is served normally. Only a *structural* failure (not well-formed XML, no
  SOAP `Body`, an unknown operation, or a malformed attribute at the request or item-LIST level, which
  applies to every item at once) is still a whole-operation fault. `E_FAIL` is an imperfect code for
  "this attribute did not parse"; the specification's per-item vocabulary has no better one, which is why
  `DiagnosticInfo` carries the detail.
- **`Browse` continuation points are authenticated, not validated.** The wire token carries an HMAC over
  the request's filter set, the cursor and an expiry, keyed with a per-process random key — so a token this
  server did not issue, for these filters, within `Config.ContinuationPointTTL`, is refused before the
  backend is called. That is an authenticity guarantee only: a token can still be replayed inside its
  lifetime, and the address space may have changed underneath it, so backends must go on validating the
  cursor as ordinary input (see `docs/backend-implementation.md`). Tokens also do not survive a restart or
  cross between instances, which is correct — a cursor is meaningful only to the live backend that issued
  it — but means clients must handle `E_INVALIDCONTINUATIONPOINT` by restarting the browse.
- **`RevisedSamplingRate` is only as truthful as the backend makes it.** A backend that implements
  `backend.SamplingRateReviser` can report the rate it will actually achieve, and an item whose rate was
  revised is reported with `S_UNSUPPORTEDRATE` (§3.5.2). A backend that does not implement it gets the
  previous behavior: the revised rate is exactly what the client requested, which for a device with a fixed
  scan cycle is a promise the server cannot keep. There is no way for this library to detect that on the
  backend's behalf.
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
- `Browse` filter application (REQ-BROWSE-004) — the server authenticates continuation points and rejects
  a `BrowseFilter` outside the schema's enumeration with `E_INVALIDFILTER` (substituting the schema's own
  `all` default for an absent one), but `ElementNameFilter`/`VendorFilter` pass through unchanged, and
  whether the backend's results actually honor any of them is not this library's concern.

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
- **The per-axis subscription limits multiply, so there are also server-wide ones.**
  `MaxConcurrentSubscriptions` (10000) and `MaxItemsPerSubscription` (1000) together would permit ten million
  live items, each holding its own last sample and up to `MaxBufferedSamplesPerItem` buffered ones.
  `Config.MaxTotalSubscribedItems` (200000) bounds the product, and `Config.MaxTotalBufferedSamples`
  (1000000) bounds the third axis the first two do not reach — without it, 200000 items × 100 buffered
  samples permits twenty million buffered entries, each holding a full `xmlda.Value`. A deployment
  expecting any axis near its maximum should size all four together rather than any one in isolation.
- **Buffered-sample loss under the server-wide budget.** `Config.MaxTotalBufferedSamples` bounds buffered
  samples across every subscription. When it is exhausted, a buffering item keeps only its Latest Changed
  Value and reports the loss through `DataBufferOverflow` — correct per REQ-SUBSCRIPTION-007, but it means
  a deployment that genuinely needs deep per-item buffering across many subscriptions has to raise the
  budget rather than discovering the degradation from an overflow flag.
- **Concurrency is bounded, but by request count rather than by resource.**
  `Config.MaxConcurrentRequests` (1024) caps requests in flight and refuses the excess with `E_BUSY`. It is
  a count, not a memory or connection budget: a deployment with a large `MaxRequestBodyBytes` or many
  simultaneous long-polls should size it against those, not against its request rate.

## Not implemented

- WSDL generation/discovery.
- Any transport other than HTTP(S) POST with a SOAP-enveloped body.
- OPC Alarms & Events (A&E) — out of scope for this library.
