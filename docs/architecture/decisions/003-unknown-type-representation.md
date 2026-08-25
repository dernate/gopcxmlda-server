# ADR-003: Unknown/custom `xsi:type` values round-trip verbatim, never error

## Status
Accepted

## Context
OPC XML-DA allows vendor-specific extension types (REQ-TYPE-007). A server built on this library may receive
a Write carrying a vendor type it has never heard of, or need to echo a Browse property value with a type
outside the standard table. The library must decide what happens when `Value.UnmarshalXML` encounters an
`xsi:type` it doesn't recognize.

## Decision
`Value{Kind: KindUnknown}` captures the exact `xsi:type` QName and the verbatim inner XML bytes
(`RawValue{TypeName, InnerXML}`). Decoding never fails and never guesses; `Value.Raw()` is the only accessor
that succeeds for such a value, every typed accessor (`Int32()`, `String()`, …) returns a `*TypeError`
naming the actual type. `MarshalXML` re-emits the captured bytes unmodified.

## Alternatives considered
- **Error out on unknown type**: rejected — a Write payload or Browse property value may legitimately carry
  a vendor extension type this library has no business rejecting; erroring would make the library unusable
  for any server needing to pass such values through (e.g. proxying, or echoing a Write back on Read).
- **Best-effort heuristic decode into `any`**: rejected — most likely to silently produce *wrong* data
  precisely for the types most likely to matter to a vendor (a type is custom because it doesn't fit the
  standard table; guessing at its shape risks corrupting exactly the data a vendor cares about being
  faithful).

## Consequences
- A server built on this library can safely forward/round-trip values it doesn't understand.
- Backends that specifically need to interpret a vendor type must do so themselves via `Value.Raw()` and
  their own parsing — this library provides the escape hatch, not vendor-specific interpretation.
