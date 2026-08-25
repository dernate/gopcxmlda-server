# Protocol Support

Status against the OPC XML-DA 1.0 specification (`docs/OPCDataAccessXMLSpecification.pdf`). This library has
**not** been run against an official OPC Foundation conformance test suite — nothing here should be read as
a conformance claim. Status words used below:

- **implemented, tested** — implemented and covered by at least one automated test.
- **implemented, not separately tested** — implemented and exercised indirectly (e.g. via wire round-trip
  tests), but without a test targeting this specific behavior; usually because the behavior is inherently a
  backend-authoring responsibility this library only threads through (documented per row).
- **not implemented** — not built.

The authoritative, requirement-by-requirement version of this table is
[`docs/specification/traceability-matrix.md`](specification/traceability-matrix.md) (68 requirements: 60
tested, 4 implemented, 1 in progress, 0 not started). This document summarizes it per operation.

## Operations

| Operation | Status | Notes |
|---|---|---|
| `GetStatus` | implemented, tested | `ServerState` drives whole-operation faults for every *other* operation too — see [Cross-cutting](#cross-cutting-behavior). |
| `Read` | implemented, tested | Order/count preservation, partial success, `MaxAge` threaded to the backend (backend's own semantics, not tested by this library), `ReqType` numeric coercion (see [Type coercion](#type-coercion)). |
| `Write` | implemented, tested | Atomic Value+Quality+Timestamp application is a **backend contract**, not enforced by this library beyond passing the fields through — see `docs/backend-implementation.md`. `S_CLAMP` propagation is tested; a Write item missing its `<Value>` element resolves to E_BADTYPE (`server/write.go`, tested by `TestHandleWrite_MissingValueElement_NoPanic`). |
| `Browse` | implemented, tested | Single-level only (per spec — clients re-browse into children). Continuation-point *validation* (hash of filters) is implemented and tested by this library; filter *application* is the backend's own responsibility. |
| `GetProperties` | implemented, tested | Standard property IDs 1–8, 100–108. Per-item and per-property `ResultID`s both supported. |
| `Subscribe` | implemented, tested | Creates a subscription via either poll or push mode depending on whether the backend's `Reader` implements `ChangeNotifier`. |
| `SubscriptionPolledRefresh` | implemented, tested | Hold+Wait blocking with early-return-on-change (including a change occurring during the Hold phase itself), `ReturnAllItems` snapshot, buffering with oldest-purge-first overflow, `E_BUSY` on overlapping calls, immediate unblock on cancel/shutdown. One item, REQ-SUBSCRIPTION-012 (spec §2.5.5's more elaborate sampled-vs-exception-based timestamp *timing* rules), is implemented at a basic level but not separately tested against that specific rule set — tracked as "in progress" in the traceability matrix, not silently dropped. |
| `SubscriptionCancel` | implemented, tested | Idempotent: cancelling an unknown or already-cancelled handle is a safe no-op success (a deliberate, documented decision — see `docs/specification/open-questions.md` OQ-9), not an error. |

## Cross-cutting behavior

| Area | Status | Notes |
|---|---|---|
| Namespace handling | implemented, tested | Namespace-URI-based resolution, prefix-independent; tolerates alternative/missing prefixes on decode. One documented scope limitation: SOAP Fault QName resolution is element-local, not whole-document (OQ-13) — does not affect any currently-observed real fault fixture. |
| Type system (`xmlda.Value`) | implemented, tested | Full XSD scalar/array coverage per `docs/specification/type-mapping.md`; unknown/vendor `xsi:type` preserved verbatim for round-trip, never rejected. |
| Quality model (`OPCQuality`) | implemented, tested | Good/Bad/Uncertain, quality-driven value-presence rule (`ResolveValuePresence`) applied uniformly by the server layer. |
| Error model (Fault vs. `ResultID`) | implemented, tested | Two independent channels, selected automatically by return shape — see `docs/specification/error-mapping.md`. |
| Time/timestamps | implemented, tested | `RequestDeadline` enforced for Read/Write/Subscribe/SubscriptionPolledRefresh when already elapsed at receipt; a deadline elapsing *mid-processing* is not separately modeled (synchronous per-call handling makes that window negligible in practice — see `docs/limitations.md`). |
| Security (TLS/auth) | implemented, not separately tested | This library has no TLS/auth code and no hard dependency on any mechanism, by design (verified by design review, not a runnable test) — see `docs/server-configuration.md`. `Config.ReadOnly` (the one policy the spec itself recommends) is a separate, tested behavior. |
| Resource limits | implemented, tested | All numeric limits (`server.Config`) are documented implementation defaults, not specification mandates — see ADR-011. |

## Type coercion

`Read`'s optional `ReqType` (asking for a value back as a different XSD type than its native one) supports
a **numeric-to-numeric subset only** (`server/coerce.go`), with explicit range checking (out-of-range → the
value is not silently truncated) — tested in `server/coerce_test.go` (`TestCoerceToReqType_*`,
`TestNumericToScalar_*Boundaries`). String/array/unknown-type coercion is deliberately unsupported and maps
to `E_BADTYPE` rather than attempting a best-effort conversion.

## What this library does not implement

- OPC XML-DA's optional WSDL/discovery surface (`docs/OPCDataAccessXMLSpecification.pdf` describes the
  operations' SOAP shape, which this library implements directly; it does not generate or serve a WSDL
  document).
- Any transport other than HTTP(S) POST with a SOAP-enveloped body.
- Alarm & Events (OPC A&E) — out of scope; the reserved property-ID range 300–399 is not used.
