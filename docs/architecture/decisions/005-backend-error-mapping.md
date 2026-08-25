# ADR-005: Backend errors map to OPC result codes via a structural, not procedural, boundary

## Status
Accepted

## Context
The spec defines two independent error channels (REQ-ERROR-001/002): whole-operation SOAP Faults and
per-item `ResultID`s. A backend implementation needs a way to report both kinds of failure, and the library
needs a deterministic, low-burden way to translate backend failures into the correct channel without forcing
every backend author to memorize the full OPC fault-code taxonomy.

## Decision
The boundary is structural: the *shape* of what a backend returns picks the channel, not a runtime decision
procedure.
- Every backend method serving a per-item operation (Read/Write/Subscribe validation/GetProperties) returns
  `[]backend.Result[T]` aligned 1:1 with the request. `Result[T].ResultID` (an `xmlda.ErrorCode`, zero value
  = no condition) is set directly by the backend for item-specific conditions (`E_UNKNOWNITEMNAME`,
  `E_RANGE`, `S_CLAMP`, …) — only the backend can know these.
- A non-nil top-level `error` from any backend method is always a whole-operation failure → always becomes
  a `soap.Fault`. The server applies a deterministic default (`errors.Is(err, context.DeadlineExceeded)` →
  `E_TIMEDOUT`, else `E_FAIL`) so a simple backend needs zero OPC vocabulary knowledge for infrastructure
  failures. An opt-in escape hatch, `backend.BackendError{Fault FaultCode, Err error}`, lets a backend that
  *does* want precision (e.g. explicit busy/access-denied at the whole-call level) signal it, detected via
  `errors.As`.

## Alternatives considered
- **Generic Go errors only, server infers OPC code by pattern-matching error strings/types**: rejected — too
  fragile and stringly-typed; this is exactly the anti-pattern the reference client's raw-string result
  codes represent (`TItem.Error string`, `OpcErrors.Id string`), which this design explicitly avoids
  repeating.
- **Require every backend to directly return exact `ResultID`/fault-code values for everything, including
  infra failures**: rejected as an unreasonable burden on simple backends that have no way to know, and no
  reason to care about, OPC's fault-code taxonomy for a plain database timeout.

## Consequences
- There is no runtime ambiguity: a backend cannot accidentally make a whole-call error look like a per-item
  condition (no per-item slot exists at that point in the call), and cannot accidentally make a per-item
  condition silently fault the whole call.
- `ServerState`-driven whole-call faults (REQ-SERVER-002, e.g. `E_SERVERSTATE`) are checked centrally by the
  server layer *before any operation-specific backend call* (`Reader.Read`, `Writer.Write`, etc.) — the
  backend's own logic is never involved in that decision. (Resolving `ServerState` itself does require one
  `Status.GetStatus` call, made by every operation regardless — see `docs/architecture/data-flow.md`.)
- `xmlda.DedupeErrors` remains genuinely a helper, called only by response constructors — backends and
  application code never build the `Errors` list by hand.
