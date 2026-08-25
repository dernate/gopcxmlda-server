# ADR-006: No separate DTO layer between the public API and the wire/protocol model

## Status
Accepted

## Context
A common design question for a protocol library is whether callers should ever see the wire-encoding types
directly, or whether there should be a translation boundary to a separate "friendly" API model. `xmlda`'s
exported structs (`ReadResponse`, `ItemValue`, `Value`, …) are simultaneously the wire model *and* what
`backend`/`subscription`/`server` (and, indirectly, backend implementers) consume.

## Decision
No separate DTO/translation-layer package. The translation boundary exists **at the type level, inside**
`xmlda`, not as a duplicated struct layer: `QName`, `ErrorCode`, `ScalarType`, and `Value`'s
constructor/accessor pattern absorb all `encoding/xml` mechanics (raw `xml.Name`, prefixed attribute-value
strings, token-walking) so that a caller's *public field types* never expose a raw `encoding/xml` artifact.
The clearest concrete example is `ValueTypeQualifier` handling: the wire-level quirk that `time`/`date`/
`duration` are transmitted as `dateTime`/`string` with a disambiguating attribute is fully hidden behind
`Value`'s semantic `ScalarType` and typed constructors/accessors — a caller never sees `ValueTypeQualifier`
as a field they need to manage themselves.

## Alternatives considered
- **A second, duplicated "friendly" struct layer** (e.g. a `model` package mirroring every `xmlda` struct
  with plain Go idioms): rejected — `backend`/`subscription`/`server` need direct field access to the wire
  types anyway (there is no independent business-logic representation these packages operate on instead),
  so a second layer would be pure duplication with no behavioral benefit, and would need to be kept in sync
  by hand forever.

## Consequences
- `xmlda`'s exported API surface *is* the public API for wire-adjacent concerns — its GoDoc quality and
  naming matter as much as any conventionally-"public-facing" package's would.
- Any future wire-format evolution (e.g. a hypothetical XML-DA 1.1) that needs a genuinely different public
  shape would be the point at which introducing a translation layer becomes justified — not before.
