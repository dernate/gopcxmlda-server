# Changelog

This project follows [Semantic Versioning](https://semver.org/). Until a
`v1.0.0` tag exists the exported API is **not** stable: `v0.x` releases may
change it, and each such change is listed here.

## Unreleased

Everything below comes from a full review of the library against the OPC
XML-DA 1.0 specification, with each item reproduced before it was changed
and covered by a regression test afterwards.

### Fixed — data loss and correctness

- **A success code no longer silences a subscribed item.** `applyUpdate`
  treated every non-zero `ResultID` as "no value", including the `S_`
  codes §2.6 defines as *non-critical exceptions whose value is useful*.
  An item reporting a persistent `S_CLAMP` — an entirely ordinary clamped
  analog value — delivered its condition once and then no value at all for
  the lifetime of the subscription, while `Read` on the same item returned
  the value correctly. `subscription/poll.go`, `subscription/refresh.go`.
- **A constant `NaN` no longer reads as a change on every poll.**
  `Value.Equal` compared floats with `==`, and IEEE-754 says `NaN != NaN`,
  so a failed analog input produced an endless change storm: every
  long-poll returned immediately, buffers filled, and `DataBufferOverflow`
  was raised for a value that never changed. `xmlda/value.go`.
- **`ReqType` narrowing to `xsd:float` no longer returns `INF` silently.**
  A finite `double` beyond `±MaxFloat32` was handed back as `INF` with an
  empty `ResultID` and the backend's quality, contradicting the documented
  "explicit range checking". It now fails the coercion (`E_BADTYPE`).
  `server/coerce.go`.
- **A deadband now applies to array items** (§3.5.1) and is **rejected
  outside 0–100 %** with `E_RANGE`. Arrays previously bypassed the deadband
  entirely, and a value above 100 silenced the item without telling anyone.
  `subscription/poll.go`, `server/subscribe.go`.
- **One malformed item no longer costs the whole response.** A backend
  sample whose `Value` was never constructed has no declared type and
  failed the encode, which turned the entire operation into an opaque
  `E_FAIL` — up to `MaxItemsPerRequest` items' data discarded over one.
  `server/itemvalue.go`.
- **A mistyped array element is now that item's `E_BADTYPE`**, not a
  whole-request fault. The typed-array decoder gave up with the offending
  child's start tag already consumed, misaligning the decoder all the way
  up to `<ItemList>`. `xmlda/value.go`.
- **`ValueTypeQualifier` now validates the literal it retypes.** A string
  retyped as `xsd:duration` decoded cleanly, was written to the backend,
  and only then failed to encode — collapsing the response after the write
  had happened. `xmlda/itemvalue.go`.

### Fixed — concurrency and resource handling

- **`Subscribe` racing `Shutdown` no longer risks a process panic.**
  `startPush` took its `WaitGroup` slots outside the `m.mu`/`rootCtx` gate
  `armTimer` established, which the race detector reports and
  `sync.WaitGroup` can panic on. `subscription/push.go`, `manager.go`.
- **The server-wide buffered-sample budget no longer leaks.** An update in
  flight when a subscription was cancelled or reaped acquired budget slots
  after they had been released; enough ordinary client churn eventually
  degraded every buffering subscription server-wide to "latest value only".
  `subscription/manager.go`, `poll.go`.
- **A hanging backend no longer leaks goroutines per request.** The status
  cache is held across the backend call by design; waiting on it with a
  context cost one parked goroutine per waiter, and the count grew with the
  request *rate*. The lock is now a cancellable channel released by the
  fetch itself, so at most one goroutine is ever parked in a stuck call.
  `server/handler.go`.
- **A backend that ignores `ctx` no longer wedges the server.**
  `RequestTimeout` was a request to stop, not a mechanism: four blocking
  calls occupied four `MaxConcurrentRequests` slots permanently and their
  clients got no answer at all. New `Config.BackendTimeout` bounds every
  backend call for real. `server/itemvalue.go`, `server/config.go`.
- **`Subscribe` rolls back when its response cannot be written**, instead
  of leaving a live subscription whose handle the client never saw.
  `server/subscribe.go`.
- **Lock hold times reduced**: the server-wide item budget is a counter
  rather than a scan of every subscription under the global write lock,
  and the reaper tears a subscription down outside that lock.
  `subscription/create.go`, `manager.go`, `reaper.go`.

### Fixed — protocol conformance

- A `mustUnderstand` SOAP header block is answered with a `MustUnderstand`
  fault (SOAP 1.1 §4.2.3) instead of being ignored — relevant wherever
  authorization travels in a header.
- An envelope with more than one `<Body>` is rejected (SOAP 1.1 §4).
  `encoding/xml` kept the last, so an intermediary inspecting the first saw
  a different operation than the server executed.
- Requests in **UTF-16** (required by XML 1.0 §4.3.3) and **ISO-8859-1** /
  **Windows-1252** are accepted instead of refused with a Go-internal
  message.
- A failing item now carries `<Quality QualityField="bad"/>`, as §2.6's own
  example shows; omitting the element let the schema default read as
  *good*.
- `ReturnDiagnosticInfo="true"` always emits the element, blank if there is
  nothing to say (§3.1.6).
- `ReturnErrorText="false"` omits the `Errors` list entirely rather than
  sending entries without the `Text` §3.1.9 says each one carries; the
  codes remain on each item's `ResultID`.
- `ReturnErrorText` defaults to **false** on `Browse` and `GetProperties`,
  which is what their own schema declarations say (only `RequestOptions`
  defaults to true).
- `RItemList` is empty when `ReturnValuesOnReply="false"` and no item
  reports a condition (§3.5.2).
- Locale resolution falls back to the language subtag before the server
  default (§2.4): `de-AT` now reaches a server offering `de` or `de-DE`.
- An empty or absent `<ItemList>` is served as an empty success; both are
  `minOccurs="0"` in the schema.
- Schema-legal nilled array elements (`<anyType xsi:nil="true"/>`) are
  accepted.
- An unqualified QName in an attribute no longer emits `xmlns=""`, which
  took its carrier element out of the OPC XML-DA namespace and made the
  whole response schema-invalid.
- `GetStatus` with a `LocaleID` runs its re-fetch through the same
  normalization as every other path, so `ServerState` cannot go missing.
- HTTP `405` carries an `Allow` header (RFC 9110 §15.5.6); a request whose
  `Content-Type` names something other than XML/SOAP is refused with `415`.

- **A successful `Write` no longer reports `Quality="bad"`.** With
  `ReturnValuesOnReply` at its default of false — the ordinary `Write` —
  every returned item carried the explicit Bad quality that belongs on a
  `Read` item which should have produced a value and could not,
  contradicting the empty `ResultID` beside it. A failed item still
  states Bad quality (§2.6 p.22). Found by driving the server with
  NothinRandom/pyopcxmlda, which reads the first typed child of `<Items>`
  as the item's data type and so reported every successful write as type
  `opc:OPCQuality`.

- **An empty optional `dateTime` attribute is treated as absent instead
  of faulting the whole request.** `RequestDeadline=""`, `HoldTime=""` and
  `Timestamp=""` are not schema-valid — `xs:dateTime` has no empty lexical
  form — but clients that assemble requests from string templates emit
  every attribute they know and leave the unset ones empty. Every
  request-side `dateTime` attribute in this protocol is optional, so
  "unset" is the only reading an empty one can have. pyopcxmlda sends
  `RequestDeadline=""` on every `Subscribe` and `HoldTime=""` on every
  `SubscriptionPolledRefresh`, so subscriptions were unreachable for it
  entirely.

- **A qualified fault code's namespace prefix is now bound on the
  envelope as well as locally.** A fault code is a QName in element
  content, and the binding was made only on the element carrying it, as
  the specification's own example does (§2.6 p.21). A parser built on a
  namespace-normalizing DOM resolves content QNames against the scope it
  *entered* the element with, so that binding is invisible to it:
  mlabs-haskell/opc-xml-da-client, which resolves every `xsi:type` this
  server sends, answered faults with `Namespace not found: q0` and could
  not read fault codes at all — and §2.5.1's error-handling flow turns on
  telling `E_NOSUBSCRIPTION` from `E_BUSY` from `E_TIMEDOUT`. The local
  binding is kept: this package's own fault decoder resolves
  element-locally by design (OQ-13). Both name the same URI.

- **`ValueTypeQualifier` may now stand in as a `<Value>`'s type when the
  value declares none.** A deliberate tolerance rather than a reading of
  the specification, which sides with the previous `E_BADTYPE` (§2.7.1
  presents the qualifier as an accompaniment to an already-typed value;
  §3.4 makes `Value` required). It only ever turns a rejected item into an
  accepted one, cannot change how a conforming request decodes — an
  explicit `xsi:type` still wins — and is restricted to qualifiers naming
  an XSD scalar type this library can decode. What it buys: pyopcxmlda
  spells the attribute `xsi:Type`, a different and meaningless attribute
  in a case-sensitive language, so `ValueTypeQualifier` is the only type
  its `Write` states and every write it issued came back `E_BADTYPE`.

### Fixed — resource limits

- **Element nesting is bounded** (`Config.MaxElementDepth`, default 64)
  during the first tokenizer pass. A 4 MiB body of nested start tags cost
  roughly 128 MiB of live heap and a second of CPU per request, before any
  protocol limit applied.
- **`GetProperties` bounds `PropertyNames`** and the product of items ×
  names. A 215 KB request produced a 739 MB response, assembled in memory.
- **Long polls have their own budget** (`Config.MaxConcurrentPolledRefresh`,
  default ¾ of `MaxConcurrentRequests`). Sharing one semaphore let enough
  concurrent long polls answer every other client with `E_BUSY` for
  minutes.
- A negative `Config.ContinuationPointTTL` means *no expiry*, as
  documented; it previously expired every token immediately and broke
  `Browse` pagination outright.

### Performance

- Namespace prefixes are declared once on the SOAP envelope instead of on
  every element. A 1000-item `Read` response went from **530 KB to 203 KB**
  (6004 `xmlns` declarations to 4).
- The operation name is read off the pass that builds the prefix table,
  removing a second full tokenization of every request.
- Together with the above, a 1000-item `Read` went from **16.3 ms / 6.8 MB
  / 54 300 allocations** to **5.5 ms / 4.7 MB / 41 300 allocations**.

### Added

- `Config.MaxElementDepth`, `Config.MaxConcurrentPolledRefresh`,
  `Config.BackendTimeout`.
- `Handler.Stats()` and `Handler.HealthHandler()` for liveness/readiness
  probes — the SOAP endpoint answers every `GET` with 405, correctly, so a
  probe had nowhere to point.
- `telemetry.Metrics` gained `ObserveRequestLatency`, `IncDroppedSamples`
  and `ObservePollLag`: server-side latency (only backend latency was
  measured), the moment process data is discarded, and how far behind
  schedule polling runs — the three an operator needs and none of which
  existed.
- `subscription.Manager.Count()`, `subscription.Config.WithDefaults()`,
  `xmlda.NewDocumentLimited`, `xmlda.DeclareAncestorNamespaces`,
  `xmlda.ResponseNamespaces`, `xmlda.Array.NumericFloat64s`,
  `soap.Envelope.ExtraNamespaces`, `soap.MustUnderstandError`,
  `soap.HeaderBlock`.
- `backend.ChangeEvent.DiagnosticInfo`.

- **`xmlda.KindQuality`, `xmlda.NewQualityValue` and `Value.Quality`.**
  `Value` modelled the XSD simple types and their arrays, which left one
  gap: §3.1.10 p.40 declares standard item property 3 (`quality`) with
  the data type `OPCQuality`, the one complex type this protocol puts in
  a `<Value>` position. A backend could therefore serve every standard
  property except that one, and an `OPCQuality` arriving from a peer
  decoded as `KindUnknown` — round-trippable opaque bytes, not something
  a client could inspect. Both directions now work; the real captured
  fixture in `testdata/responses/getproperties_116.response.xml` contains
  exactly this property and its assertion in `xmlda/getproperties_test.go`
  changed from "opaque unknown" to a decoded good/none/0 quality.

  `Kind` gained a variant, so a `switch` over it that used to be
  exhaustive no longer is, and an `OPCQuality` that used to arrive as
  `KindUnknown` now arrives as `KindQuality` — see *Changed — breaking*.

- `xmlda.AccessRightsUnknown` / `AccessRightsReadable` /
  `AccessRightsWritable` / `AccessRightsReadWritable` and
  `xmlda.EUTypeNoEnum` / `EUTypeAnalog` / `EUTypeEnumerated`, the legal
  values of standard properties 5 and 7. §3.1.10 p.40 does not merely
  suggest these spellings — "one of the following valid values must be
  used" — so leaving them as free strings invited backends to answer
  `read-write` or `RW`, which a conforming client cannot interpret.

- `soap.FaultCodePrefix`, the prefix a qualified fault code is written
  with, named once rather than spelled `"q0"` in four places.

### Changed — breaking

- **`xmlda.Kind` gained `KindQuality`, and an `OPCQuality`-typed
  `<Value>` no longer decodes as `KindUnknown`.** Code switching
  exhaustively over `Kind`, or relying on a quality property value being
  opaque bytes reachable through `Value.Raw`, has to handle the new
  variant. `Value.Quality` is how that value is read now. See *Added*
  for why the gap was worth closing.

These are source-incompatible for anyone already building against the
library. All of them are pre-`v1`.

- `telemetry.Metrics` has three new methods. Implementations must add
  them; embedding `telemetry.NoopMetrics()` is the cheapest way to stay
  compatible with future additions.
- `xmlda.ItemValue.DiagnosticInfo` is now `*string`. §3.1.6 makes the
  element's *presence* the answer to `ReturnDiagnosticInfo`, and a `string`
  could not distinguish "asked, nothing to say" from "not asked".
- `xmlda.Array`'s typed accessors return a copy of the array's storage.
  They previously handed out the internal slice, which made a `Value`
  mutable through an accessor.
- `xmlda.BrowseRequest.ReturnErrorTextOrDefault` and
  `xmlda.GetPropertiesRequest.ReturnErrorTextOrDefault` now default to
  `false` (their own schema default).
- A subscription's context is derived from the `Subscribe` request's
  context values (`context.WithoutCancel`) rather than from
  `context.Background()`, so an identity placed in the request context by
  middleware reaches poll-mode reads and `WatchItems`. Backends that
  authorize per call now see the caller on every poll; previously they saw
  one authorized call followed by anonymous ones forever.
- `backend.ChangeEvent.Err`'s documented behavior was wrong and is now
  described accurately: the engine reports the condition and keeps applying
  later events for that item, rather than silencing it.

### Testing

- **Independent clients now drive the server.** Both integration suites
  used github.com/dernate/gopcxmlda, which is independently maintained but
  shares an author with the server: what they prove is that the two agree
  with each other. `test/dockerintegration/clients/` runs three containers
  against the server on a shared Docker network, each printing one
  assertion per line so a failure names what failed:

  - `pyopcxmlda` — a **real OPC XML-DA client** (Python, MIT) that
    hand-builds every request and parses every response with ElementTree.
    44 checks.
  - `haskell` — a **real OPC XML-DA client**
    (mlabs-haskell/opc-xml-da-client, MIT) whose request construction and
    response parser are hand-written from the specification. Its parser is
    strict, it decodes into a typed sum type (so an `ArrayOfDouble` either
    arrives as a vector of doubles or not at all), and it parses SOAP
    faults in both shapes. 56 checks.
  - `zeep` — a **generic SOAP/XSD stack, not an OPC client**, building its
    proxy from `testdata/schema/opcxmlda.wsdl` — the specification's own
    WSDL, transcribed alongside the XSD that was already there — and
    validating every response against the schema strictly, which is
    precisely where Go's `encoding/xml` is lenient. Counted separately
    from the two above: that WSDL was transcribed in this repository, so
    it is no independent opinion on OPC semantics.

  The two real clients found the three protocol defects listed under
  *Fixed — protocol conformance* above and prompted the
  `ValueTypeQualifier` tolerance. Notably, both send a **SOAP 1.1 envelope
  with `Content-Type: application/soap+xml`**, independently of each
  other — a combination neither binding prescribes, and one a server
  keying its reply version off the Content-Type would answer wrongly.

  Writing it surfaced one thing worth knowing before building any
  WSDL-generated client: `ItemValue`'s `<Value>` is declared with no type,
  so the element's `xsi:type` IS the type, and a client passing a bare
  language-level number sends no type at all and gets `E_BADTYPE` —
  correctly. The value has to be typed explicitly.

- **The plant-backend fixture now enforces one canonical data type per
  item**, coercing a write to it or refusing it with `E_BADTYPE`. Storing
  the written value verbatim had two quiet consequences: an item's data
  type changed under it — an `int` written to the double `Speed` item made
  it report `xsi:type="xsd:int"` from then on, contradicting its own
  `dataType` property — and range checking stopped working, because the
  clamp could only read a value that was already a double, so a write in
  another numeric type bypassed the limit entirely. Every clamp assertion
  in the suite happened to send a double, so nothing noticed.
  `rawrequest_test.go` now covers all three directions (coerced, clamped,
  refused).

  It paid for itself immediately by rejecting an incoherent existing
  test: `TestDockerServer_AllOperations/WriteThenReadBack` wrote an
  `ArrayOfInt` into the scalar `Speed` item — because the reference
  client's *scalar* writes hit a known client-side `xsi:type` mismatch,
  the test used an array to get an observable write — and passed only
  because the fixture stored it, turning a double item into an array item
  for everything that ran afterwards. The fixture now has a writable
  `ArrayOfInt` item (`Tank1/Setpoints`, deliberately one the simulator
  does not tick, so a write is actually observable on read-back) and the
  test targets that instead. Writable-array coverage is new; there was
  none.

  The soak test's write/read-back worker had the same shape and moved to
  the same item. Worth noting what it had been measuring: it reported
  hundreds of successful round trips per window while writing an array
  into a scalar item, so the "value read back matches the value written"
  invariant held on a value the item should never have accepted.

- **Test-only fixes from the first CI run of this series**, which had
  never reached CI before being pushed: an `errorlint` violation (a bare
  type assertion on an error in `soap/version_test.go`, now `errors.As`,
  which is the exact class of defect that linter was enabled for) and a
  dead embedded field in `backend/backendtest/backendtest_test.go`. The
  three root-module CI jobs also pass `cache: false` to `setup-go`: the
  library depends on nothing outside the standard library, so there is no
  `go.sum` to key a cache on, and the default logged "Restore cache
  failed: Dependencies file is not found" — which reads like a broken
  workflow rather than the property it is.

- Coverage raised from 81.1% to 84.6% per package (87.0% counted across
  packages), concentrated on code that had none rather than on the number:
  the backend error-mapping contract that both the server and the
  subscription engine depend on (0% → 100%), the SOAP 1.2 fault shape and
  version mapping, the new health endpoint, the buffered-sample budget's
  exhaustion path, `clock.Real`, and the `Server` convenience wrapper's
  Start/Shutdown round trip.

### Documentation

- `docs/getting-started.md`'s graceful-shutdown example called
  `log.Fatal(srv.Start())` in a goroutine, which exits with status 1 at
  exactly the moment `Shutdown` starts working.
- The traceability matrix's coverage summary said 68 requirements with a
  distribution summing to 65; the table holds 75 rows.
- `docs/limitations.md` said every response is schema-validated in CI. It
  is nine frozen golden documents.
- The test count in the README is stated exactly rather than rounded up.
