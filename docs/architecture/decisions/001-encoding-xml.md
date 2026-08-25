# ADR-001: Use `encoding/xml`, augmented with custom Marshaler/Unmarshaler only where needed

## Status
Accepted

## Context
The wire format needs: namespace-correct SOAP/OPC XML-DA marshaling and unmarshaling, an XML-DA-specific
generic `Value` type whose encoding depends on run-time type information (`xsi:type`), and QName-attribute
resolution that plain struct-tag mapping can't express (see ADR-004). Options: hand-built string
concatenation (the approach taken by the reference client, `gopcxmlda_only_for_reference_dont_add/`), a
generic XML tree library (e.g. `etree`), XSD-driven code generation, or `encoding/xml` with targeted custom
`Marshaler`/`Unmarshaler` implementations.

## Decision
Use `encoding/xml` as the primary mechanism, with custom `MarshalXML`/`UnmarshalXML` on exactly the types
that need it: `xmlda.Value`, `soap.Fault`, and any element carrying a QName-shaped attribute (`ItemProperty`,
`ItemValue`/`SubscribeItemValue` for `ResultID`, `OPCError` for `ID`).

## Alternatives considered
- **Hand-built string concatenation** (reference client's approach): rejected — forfeits automatic escaping
  and namespace-correct name resolution, the two things hardest to get right by hand and that `encoding/xml`
  gets right by construction.
- **Generic XML tree library**: rejected as the primary mechanism — would mean re-implementing struct-tag
  style typed mapping on top of it for no net benefit. Retained conceptually only for the narrow
  `Value{Kind: KindUnknown}` passthrough case, and even there raw `innerxml` capture via `encoding/xml`
  itself is sufficient — no separate library needed.
- **XSD code generation**: rejected — the spec's XSD does not cleanly express the hierarchical-parameter
  override semantics (§3.1.1) or the Good/Bad/Uncertain value-presence rules (§3.1.5); those need
  hand-written Go regardless, so code-gen would only cover the easy portion of the problem.

## Consequences
- Verified experimentally (small throwaway programs) that `encoding/xml` struct tags without an explicit
  namespace match by local name only, regardless of actual namespace/prefix — this is exploited directly for
  namespace-independent operation dispatch (ADR-004).
- `encoding/xml` does *not* resolve prefixes inside attribute *values*, and `UnmarshalXMLAttr` receives no
  decoder — both addressed by ADR-004's prefix-table mechanism.
- No third-party XML dependency is introduced.
