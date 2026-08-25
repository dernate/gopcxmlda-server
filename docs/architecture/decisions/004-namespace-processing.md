# ADR-004: Namespace processing by URI everywhere; tolerant fault parsing, consistent fault emission

## Status
Accepted

## Context
REQ-NS-002 requires namespace identity to be resolved by URI, never by prefix. Real captured traffic
(`testdata/responses/subscribe_680.response.xml`) shows the OPC namespace bound both as a default `xmlns` on
the response root *and* as an explicit prefix (`ns1`) used only inside `xsi:type` attribute *values* in the
same document. Three `encoding/xml` facts, verified experimentally, shape the solution:

1. Struct tags without an explicit namespace match by local name only, regardless of the element's actual
   namespace/prefix (confirmed identical behavior across SOAP 1.1, SOAP 1.2, and default-namespace
   variants).
2. `xml.Decoder` resolves prefixes used in element/attribute *names* (`xml.Name.Space` is always the resolved
   URI) but does **not** resolve prefixes appearing inside an attribute's *value* (e.g.
   `xsi:type="ns1:ArrayOfUnsignedShort"` — the attribute name resolves correctly, but the string
   `"ns1:ArrayOfUnsignedShort"` is handed over raw).
3. `xml.UnmarshalerAttr.UnmarshalXMLAttr` receives no decoder — so any attribute whose *value* is a QName
   (`xsi:type`, `ResultID`, `ItemProperty.Name`, `OPCError.ID`) cannot resolve itself there; resolution must
   happen one level up, in the containing element's `UnmarshalXML(d, start)`, which does get the decoder.

Separately, real fault traffic (`testdata/faults/`) shows four distinct shapes: spec-conformant SOAP 1.1
with a QName-qualified `faultcode`; a legacy SOAP 1.1 fault with unqualified literal text
(`E_NOSUBSCRIPTION`) in `faultcode`/`faultstring`/`detail`; a generic SOAP 1.1 parse-error fault
(`faultcode=SOAP-ENV:Client`, no OPC content); and a SOAP 1.2 `Code`/`Reason`/`Detail` structured fault.

## Decision
1. Match element/attribute *names* by resolved `(Space, Local)` everywhere — this is `encoding/xml`'s own
   default behavior for tag-less-of-namespace struct tags, so no extra code is needed for this part.
2. For QName-shaped attribute *values*, build one flat, whole-document prefix→URI table per decode
   (`buildPrefixTable`, "last declaration wins" if a prefix is redeclared at different depths — not observed
   in any real fixture, see `open-questions.md` OQ-6) and resolve every such value through one
   `resolveQName(d, raw)` helper, keyed to the `*xml.Decoder` instance via a `sync.Map` cleared immediately
   after the top-level decode.
3. QName-attribute resolution happens in the *containing element's* `UnmarshalXML`, never via
   `UnmarshalXMLAttr`.
4. On write, every element that needs an `xsi:type` locally and self-containedly declares whatever prefix
   it uses (`xmlns:xsi`, plus `xmlns:opc`/`xmlns:xsd`/`xmlns:ext` as needed) on that same element — response
   root elements themselves (`GetStatusResponse`, `ReadResponse`, ...) carry no namespace declaration at
   all, since matching by local name (point 1) makes one unnecessary. This differs from this ADR's original
   intent (an earlier draft additionally proposed declaring the OPC namespace as a default `xmlns` on every
   response root, mirroring `testdata/responses/subscribe_680.response.xml`'s exact shape) — corrected here
   to describe what `xmlda`'s `MarshalXML` implementations actually do, verified against real emitted output
   during a later interoperability review (see the Update below), rather than left inconsistent with the
   code.
5. Fault parsing tolerates all four observed shapes, normalizing into one internal `soap.Fault{Code, Text,
   Detail}`. Fault *emission* always uses exactly one style: spec-conformant SOAP 1.1 with a QName-qualified
   `faultcode` — chosen once, never varied per request.
6. Within `typeAttrs` (the `xsi:type`/`xmlns:*` attribute set attached to every `<Value>`, `ItemValue`,
   `ReplyBase`, etc. element), `xsi:type` is emitted *before* the `xmlns:*` declarations it depends on. XML
   attribute order carries no semantic meaning — a namespace declaration's scope covers its whole element
   regardless of where in the attribute list it textually appears — so this is a free, zero-risk
   serialization choice. See the Update below for why it was made.

## Alternatives considered
- **True nested XML namespace scoping** (a full scope stack mirroring XML's actual namespace-scoping rules):
  rejected as unnecessary complexity — no real OPC XML-DA traffic observed rebinds a prefix to different URIs
  at different depths within one document; the flat table is simpler and sufficient, with the limitation
  documented (OQ-6) so it can be revisited without touching any struct definition if ever needed.
- **Emit whichever fault shape happens to be easiest at each call site**: rejected — inconsistent output
  makes the library's own behavior harder to test and reason about; one shape, chosen deliberately, is
  emitted always.
- **Reject non-conformant faults on input**: rejected — real-world interop requires tolerating what real
  servers/clients actually send, not just what the spec's own examples show.

## Consequences
- Namespace prefix choice by a client never affects dispatch or decoding correctness.
- `resolveQName`/`buildPrefixTable` are the only two functions that ever interpret a `prefix:local` string —
  isolating this concern so OQ-6's simplification can be revisited in one place if ever needed.
- This library's own fault output is predictable and simple to golden-test, while its fault *input* handling
  is verified against all four real-world shapes in `testdata/faults/`.

## Update: `xsi:type` attribute ordering (found via real-client integration testing)

`test/clientintegration/` drives this server's real HTTP endpoint through the independently-maintained
`github.com/dernate/gopcxmlda` reference client (a separate Go module specifically so this doesn't become a
dependency of the base library). That client's `TValue.UnmarshalXML` decodes a `<Value>` element's type by
reading `start.Attr[0]` (attribute *index* 0) and splitting its value on `:`, rather than resolving by
attribute name — a client-side bug, not something this specification requires or forbids. Before decision
point 6 above, `typeAttrs` emitted `xmlns:xsi` first, so `start.Attr[0]` was always the namespace
declaration, not `xsi:type` — that real client could not decode *any* `<Value>` this server produced (every
`Read`, `GetProperties` value, `Subscribe`/`SubscriptionPolledRefresh` item, and `Write`'s echoed value).
Reordering `typeAttrs` to emit `xsi:type` first fixed this for that client with zero cost: XML attribute
order has no semantic meaning, so this is not a spec-conformance trade-off, just a serialization
convenience. Confirmed via `test/clientintegration`'s `TestRealClient_ReadInitialValue`,
`TestRealClient_GetProperties`, and `TestRealClient_SubscribeAndPolledRefresh` all passing after the change,
with no existing test in this repository depending on the old order (round-trip tests compare decoded
values, not raw bytes). See `docs/interoperability.md` for the two remaining, client-side-only findings this
same testing surfaced (a scalar-value `xsi:type` namespace mismatch, and `SubscriptionCancel`'s
success-swallowing return) that are not fixable from this server's side.
