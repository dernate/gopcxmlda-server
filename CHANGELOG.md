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

### Changed — breaking

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

### Documentation

- `docs/getting-started.md`'s graceful-shutdown example called
  `log.Fatal(srv.Start())` in a goroutine, which exits with status 1 at
  exactly the moment `Shutdown` starts working.
- The traceability matrix's coverage summary said 68 requirements with a
  distribution summing to 65; the table holds 75 rows.
- `docs/limitations.md` said every response is schema-validated in CI. It
  is nine frozen golden documents.
- The test count in the README is stated exactly rather than rounded up.
