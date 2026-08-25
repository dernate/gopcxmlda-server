# Interoperability

Real-world OPC XML-DA traffic is less uniform than the specification's own examples. This library was
built against a set of real captured fixtures (`testdata/`) alongside the specification text, and makes
several deliberate accommodations so it interoperates with peers that don't match the spec's examples
byte-for-byte. This document collects them in one place; each is also documented at its point of decision
in `docs/specification/open-questions.md` (referenced below as OQ-N) and
`docs/architecture/decisions/` (ADR-N).

## Validated against a real, independent client, not just this library's own fixtures

`test/clientintegration/` (a separate Go module, so the base library's own `go.mod` stays dependency-free)
drives this server's real HTTP endpoint through `github.com/dernate/gopcxmlda`, an independently-maintained
OPC XML-DA client — not this repository's reference-only `gopcxmlda_only_for_reference_dont_add/` copy, and
not this library's own hand-built test fixtures. This caught one real bug in *this* server (fixed
immediately, see below) and surfaced two client-side-only quirks (not fixable from this server's side,
documented below and in the test file itself):

- **Fixed here**: `xmlda.Value`'s `<Value xsi:type="...">` elements used to emit `xmlns:xsi` as their first
  attribute. `github.com/dernate/gopcxmlda`'s decoder reads attribute *index 0* and splits its value on `:`
  instead of resolving by attribute name — so it could not decode *any* value this server produced. Fixed
  by reordering `typeAttrs` to emit `xsi:type` first; XML attribute order carries no semantic meaning, so
  this cost nothing and broke no existing test. See ADR-004's "Update" section for the full story.
- **Client-side, not fixable here**: that same client's Write path emits a scalar value's `xsi:type` in the
  OPC XML-DA namespace (e.g. `xsi:type="ns0:boolean"`) instead of the XSD namespace the specification uses
  for scalars (`xsi:type="xsd:boolean"`) — confirmed by reading `getOpcXmlDaType`/`buildWriteItems` in the
  client's source. This server correctly treats that as an unrecognized type (`Kind: Unknown`, verbatim
  round-trip per ADR-003) rather than guessing — the practical effect is that a scalar value written through
  that specific client arrives as an opaque blob, not a typed value. Array values are unaffected: the
  client's `xsi:type="ns0:ArrayOfInt"` happens to be correct, since arrays *do* belong in the OPC namespace
  per this server's (and the real fixture's) convention. See
  `test/clientintegration/client_test.go`'s `TestRealClient_WriteScalar_TypeNamespaceMismatch` and
  `TestRealClient_WriteArray_DecodesCorrectly`.
- **Client-side, not fixable here**: that client's `SubscriptionCancel` method unconditionally returns
  `(true, nil)` — its last statement ignores whatever fault/error text it accumulated while decoding the
  response. `TestRealClient_SubscriptionCancel` verifies this server's actual cancellation behavior
  independently (by polling the same handle again afterward and confirming the server reports it invalid),
  rather than trusting that client's return value.
- Two more client quirks, not bugs, worth knowing if you write code against this client specifically: it
  never resolves a QName-shaped attribute's namespace prefix — `TItem.Error` and `TProperties.Name` capture
  the literal wire text verbatim (e.g. `"opc:E_UNKNOWNITEMNAME"`, `"opc:dataType"`), using whatever prefix
  *this server* chose (always `"opc"`, deterministically); and its `SubscriptionPolledRefresh` method
  hardcodes `WaitTime=500` and `ReturnAllItems=false` with no way to override either through its public API.

## Namespace prefixes are never semantically significant — only the URI is

`xmlda.resolveQName` resolves every `xsi:type`, `ResultID`, `ItemProperty.Name`, and `OPCError.ID` value by
its **resolved namespace URI**, never by its literal prefix string. A peer is free to bind the OPC XML-DA
namespace to any prefix it likes (`opc:`, `ns1:`, no prefix via a default `xmlns=`, or even redundantly to
two different prefixes in the same document — observed in real traffic) and this library resolves it
identically either way. Conversely, this library's own emitted output always locally declares whichever
prefix it uses (`xsd`/`opc`/`ext`) on the element that needs it, so its output is self-contained and correct
regardless of what a containing document might already declare (ADR-004).

**Known scope limitation** (OQ-6): resolution uses one flat, whole-document prefix table per decoded
document, not a fully correct nested-XML-namespace-scope resolver. This has not caused a problem against
any real fixture; a hypothetical document that rebinds the same prefix to different URIs at different
depths would resolve to the *last* declaration seen, not the innermost-in-scope one.

## SOAP fault shapes: tolerant on input, one consistent shape on output

Real captured traffic includes four distinct fault shapes, only one of which matches the specification's
own worked example (§2.6):

1. Spec-conformant SOAP 1.1, QName-qualified `faultcode` (e.g. `q0:E_FAIL` with `xmlns:q0` declared on the
   `<faultcode>` element itself).
2. Legacy SOAP 1.1 with an **unqualified** OPC error text in `faultcode` (`testdata/faults/fault_legacy_unqualified_e_nosubscription.response.xml`).
3. Generic SOAP 1.1 fault for a transport-level parse error, with no OPC-specific code at all
   (`testdata/faults/fault_soap11_xml_syntax_error.response.xml`).
4. SOAP 1.2's structured `Code`/`Reason`/`Detail` shape (`testdata/faults/fault_soap12_invalid_datetime.response.xml`).

`soap.Fault.UnmarshalXML` decodes all four by matching element/field local names only, regardless of SOAP
version namespace. `soap.Fault.MarshalXML` always emits shape 1 — this library never emits shapes 2–4 as
its own faults (ADR-004 / OQ-7). If you're writing a *client* against another OPC XML-DA server (not this
library), expect to see all four in the wild.

**Known scope limitation** (OQ-13): unlike `xmlda.resolveQName`'s whole-document prefix table, a fault's
`faultcode`/`Code` QName is resolved using only that element's own `xmlns:*` attributes — a prefix declared
solely on an ancestor (e.g. the `Envelope` root, as some real SOAP 1.2 traffic does) will not resolve, and
the code falls back to its literal text with an empty namespace. This matches the specification's own
worked example (where the prefix is declared directly on `<faultcode>`) and does not affect any of the
three real fault fixtures above, none of which carry a resolvable OPC-namespace-qualified code in the first
place.

## `time`/`date`/`duration`: direct `xsi:type`, with tolerance for the `dateTime`+`ValueTypeQualifier` form

The specification notes (§2.7.1) that `time`/`date` are "not fully supported by .NET tools," which some
peers work around by transmitting them as `dateTime` plus a `ValueTypeQualifier` attribute rather than a
direct `xsi:type="xsd:time"`/`"xsd:date"`. This library's own encoding always uses the direct, symmetric
form (simpler, and still spec-legal — the type-mapping table lists these as real XSD types). On **decode**,
`ItemValue.UnmarshalXML` additionally recognizes a `ValueTypeQualifier` attribute with local name
`time`/`date`/`duration` alongside a `dateTime`/`string`-typed `Value` and reinterprets it accordingly — see
OQ-12. This is a decode-side tolerance only; it never affects what this library itself sends.

## Correctly-sized byte types, both directions

A [known] asymmetry in at least one existing OPC XML-DA client implementation infers `byte`/`unsignedByte`
width differently on read vs. write. This library's `xmlda.Value` uses the correctly-sized Go type
(`int8`/`uint8`) for `byte`/`unsignedByte` on **both** paths — there is exactly one representation per XSD
type, not two inference paths that can disagree.

## `decimal` (VT_CY) is a validated string, not a float64

`xmlda.Decimal` preserves the exact lexical `xsd:decimal` text a peer sent, rather than round-tripping
through `float64` (which cannot exactly represent VT_CY's fixed-point semantics and would silently alter
digits a strict peer might compare byte-for-byte). Use `Decimal.Float64()` only when you specifically accept
that precision loss; prefer round-tripping the `Decimal` value itself when relaying data unchanged.

## Unknown / vendor `xsi:type` values never fail to parse

A `<Value>` (or `ResultID`, or `ItemProperty.Name`) declaring an `xsi:type` this library doesn't recognize
(a future spec addition, or a vendor extension in its own namespace) is preserved verbatim — captured as
`Kind: Unknown` with its exact inner XML bytes retained for round-trip — rather than rejected as an error.
Vendor error codes and vendor item properties work the same way, as long as they live in their own XML
namespace (never the OPC XML-DA namespace) and follow the `E_`/`S_` naming convention for error codes.

## Real fixtures this library was validated against

`testdata/requests/subscribe_679.request.xml` and `testdata/responses/subscribe_680.response.xml` are a
real captured `Subscribe` request/response pair (including an `ArrayOfUnsignedShort` value), golden-tested
verbatim in `xmlda/subscribe_test.go`. The three fault fixtures listed above are golden-tested in
`soap/fault_test.go`.

Four more real captures cover `GetStatus`, `Browse` (a root-level browse and a deeper browse whose result
carries inline `dataType`/`accessRights` properties across a wide spread of scalar types), and `GetProperties`
(a full `ReturnAllProperties=true` reply): `testdata/requests/getstatus_632.request.xml` /
`testdata/responses/getstatus_639.response.xml`, `testdata/requests/browse_653.request.xml` /
`testdata/responses/browse_662.response.xml`, `testdata/requests/browse_676.request.xml` /
`testdata/responses/browse_684.response.xml`, and `testdata/requests/getproperties_103.request.xml` /
`testdata/responses/getproperties_116.response.xml` — golden-tested in `xmlda/getstatus_test.go`,
`xmlda/browse_test.go`, and `xmlda/getproperties_test.go` respectively. As with the `Subscribe` fixture,
item names, folder hierarchy, and the vendor/client identifier strings from the original capture were
replaced with generic placeholders; everything that matters for decoding (types, quality, timestamps,
structure) is unchanged.

Three more real captures — this time from an independent Go OPC XML-DA client
(`github.com/dernate/gopcxmlda`, SOAP 1.2-framed as bare `application/soap+xml` with no `SOAPAction` header
at all, identified purely by the SOAP Body's child element the same way every other fixture is) talking to
the same real gSOAP/2.6 server — cover `Read` (19 items, a spread of scalar types incl. a negative-UTC-offset
`dateTime`), `SubscriptionPolledRefresh` (31 changed items, incl. negative `int`/`float` values), and
`SubscriptionCancel`: `testdata/requests/read_649.request.xml` /
`testdata/responses/read_676.response.xml`,
`testdata/requests/subscriptionpolledrefresh_226.request.xml` /
`testdata/responses/subscriptionpolledrefresh_232.response.xml`, and
`testdata/requests/subscriptioncancel_448.request.xml` /
`testdata/responses/subscriptioncancel_460.response.xml` — golden-tested in `xmlda/read_test.go`,
`xmlda/subscriptionpolledrefresh_test.go`, and `xmlda/subscriptioncancel_test.go` respectively. The source
capture contained many more near-duplicate polls of the same subscription; only one representative
request/response pair per operation was kept. As with the other real fixtures, item names and one real site
name were replaced with generic placeholders; opaque client-generated correlation strings and subscription
handles were left as captured since they carry no identifying information.

A fourth capture, from a third distinct real client (`python-requests`, a plain Python script) against the
same server, added one more real fixture: `testdata/requests/read_169.request.xml` /
`testdata/responses/read_182.response.xml` — a `Read` of a single, deeply-nested (7-segment) item path whose
value is an `ArrayOfDouble`, the only real fixture exercising `xsd:double`/`ArrayOfDouble` at all (incl.
negative values and full float64 precision artifacts like `5.4000000000000004`), golden-tested in
`xmlda/read_test.go`. That capture also repeated `GetStatus`/`Browse`/single-scalar `Read` calls already
covered by existing fixtures (same shapes, nothing new) and hundreds of near-duplicate single-item `Read`
polls — none of those were kept.

Real-capture golden coverage does not yet include `Write` — that operation is exercised only by
synthetic/hand-written fixtures today (it did not appear in any of the available captures).

If you're integrating against a peer implementation and hit a shape this library doesn't tolerate, that's a
gap worth reporting with a captured fixture — the pattern used for all of the above is to add the real bytes
under `testdata/` and extend the relevant `UnmarshalXML` to tolerate the new shape without changing what this
library itself emits.
