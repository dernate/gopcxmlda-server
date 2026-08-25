# Error Mapping

How internal failures are mapped to OPC XML-DA's two independent error channels (REQ-ERROR-001..005). See
`docs/architecture/decisions/005-backend-error-mapping.md` for the ADR-level rationale.

## The two channels

1. **SOAP Fault** — the whole operation failed; no per-item results exist at all. Rendered by `soap.Fault`
   with a QName code in the XML-DA namespace (or a zero-namespace code for transport-level failures that
   happen before any OPC operation is identified, matching the real-world
   `testdata/faults/fault_soap11_xml_syntax_error.response.xml` / `fault_soap12_invalid_datetime.response.xml`
   shapes).
2. **Per-item `ResultID`** — the operation succeeded overall; one or more items/properties carry an
   abnormal condition (`E_...` critical, `S_...` non-critical/success). Deduplicated into the response's
   `Errors` list via `xmlda.DedupeErrors`.

A backend/server implementation never chooses between these two by writing conditional logic — the *shape*
of what it returns picks the channel automatically: a top-level `error` from a backend method always becomes
a fault (there is no per-item slot for it), while a `backend.Result[T].ResultID` always becomes a per-item
condition (there is no whole-operation slot it could occupy). See `docs/architecture/data-flow.md`.

## Standard OPC XML-DA codes (§3.1.9, pp. 37-38, plus OQ-1 additions)

| Code | Kind | Meaning | Typical trigger |
|---|---|---|---|
| `S_CLAMP` | success/non-critical | Write value clamped to valid range but write succeeded | Write, value outside item's engineering range |
| `S_DATAQUEUEOVERFLOW` | success/non-critical | Buffered data purged due to resource limits | SubscriptionPolledRefresh, buffer overflow |
| `S_UNSUPPORTEDRATE` | success/non-critical | Requested sampling rate not supported, closest used | Subscribe/SubscriptionPolledRefresh |
| `E_ACCESS_DENIED` | critical | Access denied (incl. global write-disable policy) | Write when `Config.ReadOnly`, or backend ACL |
| `E_BADTYPE` | critical | Value type invalid/unsupported for coercion | Write type mismatch, Read `ReqType` coercion failure |
| `E_BUSY` | critical (fault only) | Subscription already being polled by a concurrent call | Overlapping SubscriptionPolledRefresh calls |
| `E_FAIL` | critical | Unspecified failure | Generic backend/internal error fallback |
| `E_INVALIDCONTINUATIONPOINT` | critical (fault only) | Continuation point invalid or filters changed | Browse pagination |
| `E_INVALIDFILTER` | critical (fault only) | Browse filter combination invalid | Browse |
| `E_INVALIDHOLDTIME` | critical (fault only) | HoldTime invalid | SubscriptionPolledRefresh |
| `E_INVALIDITEMID` | critical | Invalid item identifier (OQ-1) | Write |
| `E_INVALIDITEMNAME` | critical | ItemName malformed | Read/Write/Subscribe/Browse/GetProperties |
| `E_INVALIDITEMPATH` | critical | ItemPath malformed | Read/Write/Subscribe/Browse/GetProperties |
| `E_INVALIDPID` | critical | Property ID not recognized | GetProperties |
| `E_NOSUBSCRIPTION` | critical (fault only) | All polled handles invalid | SubscriptionPolledRefresh |
| `E_NOTSUPPORTED` | critical | Operation/feature not supported (also used for unknown top-level operation, OQ-2) | Write atomic quality+timestamp unsupported combination; unrecognized SOAP Body element |
| `E_OUTOFMEMORY` | critical (fault only) | Server resource exhaustion | Any operation |
| `E_RANGE` | critical | Value out of range | Read/Write/Subscribe |
| `E_READONLY` | critical | Item is read-only | Write |
| `E_SERVERSTATE` | critical (fault only) | Server state forbids this operation | ServerState=failed/suspended/noConfig |
| `E_TIMEDOUT` | critical (both channels) | RequestDeadline exceeded | Any operation, whole-op or per-item depending on when the deadline elapsed |
| `E_UNKNOWNITEMNAME` | critical | Item name not found | Read/Write/Subscribe/Browse/GetProperties |
| `E_UNKNOWNITEMPATH` | critical | Item path not found | Read/Write/Subscribe/Browse/GetProperties |
| `E_WRITEONLY` | critical | Item is write-only | Read/GetProperties |

Vendor-specific codes must live in the vendor's own XML namespace (never the OPC XML-DA namespace) and must
still follow the `E_`/`S_` naming convention (`xmlda.ErrorCode` is a `QName`, so this is representable
without special-casing).

## Backend error → OPC code mapping mechanism

- **Item-level conditions**: the backend directly sets `backend.Result[T].ResultID` to the appropriate
  `xmlda.ErrorCode` — only the backend knows item-specific semantics (unknown/out-of-range/read-only/etc.),
  so no generic inference is attempted.
- **Whole-operation infrastructure failures**: the backend returns a plain Go `error`. The server applies a
  deterministic default: `errors.Is(err, context.DeadlineExceeded)` → `E_TIMEDOUT`; anything else →
  `E_FAIL`. A backend wanting more precision can return `*backend.BackendError{Fault, Err}`; the server
  checks for it via `errors.As` before falling back to the default. This keeps simple backends simple (no
  OPC vocabulary knowledge required for infrastructure errors) while letting sophisticated backends be
  exact.
- **ServerState-driven faults** (`E_SERVERSTATE`) are evaluated centrally by the server layer via
  `xmlda.RequiresFault(op, state)` before any backend call is even made — a backend never needs to check
  `ServerState` itself.

## What is deliberately *not* mapped

Internal implementation details — Go error strings, stack traces, driver-specific error codes — are never
passed through to a client verbatim. Every whole-operation fault's `Text` is either one of the fixed,
spec-defined descriptions or a generic message; anything more specific is only available via server-side
logging (`telemetry.Logger`), never via the wire response, per the security requirement that internal
details must not leak to clients.
