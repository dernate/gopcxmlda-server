# ADR-002: XSD-to-Go type mapping via a closed enum + fixed table, not reflection

## Status
Accepted

## Context
Every OPC XML-DA `Value` carries an `xsi:type` identifying its XSD type, which must map to a Go
representation for both decoding and encoding (REQ-TYPE-001..004). The reference client
(`gopcxmlda_only_for_reference_dont_add/`) uses `reflect`-based type inference on the write path
(`getOpcXmlDaType`) that is independent of, and inconsistent with, its switch-based decode path — concretely,
it decodes `byte`/`unsignedByte` into 16-bit Go types (`int16`/`uint16`) but infers them from 8-bit Go types
(`int8`/`uint8`) on write, so a value read from a server and written back changes width silently.

## Decision
A closed `ScalarType` string enum (§`type-mapping.md`), a fixed bidirectional Go-type table (correctly-sized
per direction — `int8`/`uint8` for byte/unsignedByte on *both* read and write), explicit typed constructors
(`NewInt32`, `NewBytes`, `NewDateTime`, …), and explicit typed accessors returning `(_, *TypeError)` — never
a runtime type-switch inferred from an arbitrary Go value via `reflect`.

## Alternatives considered
- **Reflection-based inference on write** (reference client's approach): rejected — the exact
  width-asymmetry bug above is the direct, demonstrated consequence of inference being independently
  maintained on each side instead of driven from one shared table.
- **Plain `any`/`interface{}` with no typed constructors**: rejected per the project directive to avoid
  modeling all values as untyped `any` without need; loses compile-time guidance for backend authors and
  pushes every type error to a confusing runtime panic instead of a `*TypeError`.
- **`decimal` as `float64`**: rejected specifically for this one type — VT_CY is a fixed-4-decimal-place
  currency type with no exact `float64` representation; a validated string-backed `Decimal` type preserves
  wire-exactness, with `Float64()` available for callers who accept the precision trade-off explicitly.

## Consequences
- Every scalar type has exactly one Go representation in both directions — no asymmetry is possible because
  there is only one table, not two independently-maintained mappings.
- Adding a new scalar type (unlikely, since XML-DA 1.0's set is closed) means updating one table and adding
  one constructor/accessor pair, not touching a `switch` on both `UnmarshalXML` and `MarshalXML`
  independently.
