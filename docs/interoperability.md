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

## Responses are namespace-qualified, as the schema requires

The specification's WSDL declares its schema with `elementFormDefault="qualified"` and
`targetNamespace="http://opcfoundation.org/webservices/XMLDA/1.0/"`, so every element in a response —
`ReadResponse`, `ReadResult`, `RItemList`, `Items`, `Value`, `Quality`, `Errors`, and an array value's
`<double>` children — belongs to the OPC XML-DA namespace. This server opens the operation element with
`xmlns="http://opcfoundation.org/webservices/XMLDA/1.0/"` and lets every descendant inherit it, matching the
real captured traffic in `testdata/responses/` byte for byte in shape.

This was not always true. Until 2026-08-30 the payload went out in *no* namespace at all, which a
match-by-local-name client (this library's own decoder, and `github.com/dernate/gopcxmlda`) accepts without
complaint and a WSDL-generated proxy (.NET WCF/ASMX, JAX-WS, gSOAP, Python zeep) rejects outright with
"element ReadResponse from namespace '' was not expected". If you built a client against this server before
that date and worked around a missing namespace, remove the workaround.

The same fix window also corrected two other things a schema-bound client would have tripped over:
`ItemValue`'s children are now emitted in the schema's `xsd:sequence` order (`DiagnosticInfo`, `Value`,
`Quality` — `Value` before `Quality`, not after), and `DiagnosticInfo` is an element rather than an
attribute. Decoding still accepts the old attribute spelling, since a peer may still send it.

## Signed wire fields accept negative values instead of faulting

`WaitTime`, `SubscriptionPingRate`, `RequestedSamplingRate`, `MaxAge` and `MaxElementsReturned` are all
`xsd:int` in the schema — signed. They used to be decoded as unsigned, so a client sending `WaitTime="-1"`
(a common spelling of "no preference", and valid against the schema this repository now ships) got a
whole-operation parse fault. They are decoded as signed now and normalized at the server boundary: a
non-positive value means the same as an absent one, which for every one of these fields is "use the server's
own policy".

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

## Driven by two independent OPC XML-DA client implementations

Fixtures prove this library reads what real servers wrote. They cannot prove a real *client* can read what
this server writes, and neither can `test/clientintegration` or the rest of `test/dockerintegration`: those
drive the server with `github.com/dernate/gopcxmlda`, which shares an author with it, so what they
establish is that the two agree with each other.

`test/dockerintegration/clients/` closes that gap with two implementations that have never seen this
repository, each in its own container:

- **[`pyopcxmlda`](https://github.com/NothinRandom/pyopcxmlda)** (Python, MIT) — a real OPC XML-DA client:
  it hand-builds every request from the specification and parses every response with ElementTree. 44 checks.
- **[`opc-xml-da-client`](https://github.com/mlabs-haskell/opc-xml-da-client)** (Haskell, MIT) — a real OPC
  XML-DA client whose request construction *and* response parser are hand-written from the specification.
  Its parser is **strict** (content the specification does not allow at a position is a hard decode error,
  not something quietly skipped), it decodes values into a typed sum type, and it parses SOAP faults in
  both the 1.1 and 1.2 shapes. 56 checks.

A third container runs **zeep**, which is deliberately *not* counted among these: it is a generic SOAP/XSD
stack driven from `testdata/schema/opcxmlda.wsdl`, and that WSDL was transcribed from the specification's
appendix *in this repository*. It is a strong check that responses satisfy the schema — strictly, where
Go's `encoding/xml` is lenient — and no check at all on OPC semantics or on the transcription itself.

Both real clients send a **SOAP 1.1 envelope with `Content-Type: application/soap+xml`**, independently of
each other. That combination is not what either SOAP binding prescribes, and a server keying its reply
version off the Content-Type would answer both with a 1.2 envelope they do not expect. This server keys off
the envelope namespace and accepts either media type, which is what lets both through.

### What they found

Three defects in this server, each now fixed and pinned by a regression test:

1. **A successful `Write` reported `Quality="bad"`.** `ReturnValuesOnReply` defaults to false, so the
   ordinary `Write` returns items carrying no value — and the explicit Bad quality that belongs on a
   `Read` item that *should* have produced a value was being applied to them too, contradicting the empty
   `ResultID` on the same element. Not cosmetic: pyopcxmlda reads the first typed child of `<Items>` as the
   item's data type and reported every successful write as type `opc:OPCQuality`. Fixed in `server/write.go`
   (`TestHandleWrite_SuccessfulAckStatesNoQuality`); a *failed* item still states Bad quality, which is what
   §2.6 p.22 asks for.
2. **An empty optional `dateTime` attribute faulted the whole request.** `xs:dateTime` has no empty lexical
   form, so `RequestDeadline=""` is not schema-valid — but clients that assemble requests from string
   templates emit every attribute they know and leave the unset ones empty, and pyopcxmlda does exactly
   that on every `Subscribe` and every `SubscriptionPolledRefresh`. Subscriptions were therefore
   unreachable for it. Every request-side `dateTime` attribute in this protocol is optional
   (`RequestOptions.RequestDeadline`, `SubscriptionPolledRefresh.HoldTime`, `ItemValue.Timestamp`), so
   "unset" is the only reading an empty one can have. Fixed in `xmlda/replybase.go` (`wireTime`) and
   `xmlda/itemvalue.go`. The `absent` flag matters: `encoding/xml` allocates the pointer field before
   calling `UnmarshalXMLAttr`, so simply not erroring would have decoded to January of year 1 — a
   `RequestDeadline` that has already passed, worse than the fault it replaced.
3. **Fault codes were unreadable to a namespace-normalizing parser.** A fault code is a QName in element
   *content*, and its prefix was bound only on the element carrying it — as the specification's own example
   does (§2.6 p.21). A parser built on a namespace-normalizing DOM resolves content QNames against the
   scope it *entered* the element with, so a binding made on that element is invisible: the Haskell client,
   which resolves every `xsi:type` this server sends, answered a fault with `Namespace not found: q0` and
   could not read fault codes at all. For an OPC client that is not cosmetic — §2.5.1's whole
   error-handling flow turns on telling `E_NOSUBSCRIPTION` from `E_BUSY` from `E_TIMEDOUT`. The binding is
   now made in **both** scopes: on the envelope for that kind of parser, and locally because this package's
   own fault decoder resolves element-locally by design (OQ-13) and would otherwise be unable to read the
   faults it writes. Both name the same URI, so the QName is identical either way
   (`TestFault_CodeNamespaceDeclaredInBothScopes`).

And one deliberate tolerance, where the specification sides with this server but interoperating cost
nothing:

4. **`ValueTypeQualifier` may stand in as a `<Value>`'s type.** pyopcxmlda writes the type attribute as
   `xsi:Type`, which in a case-sensitive language is a different and meaningless attribute, leaving
   `ValueTypeQualifier` as the only type its `Write` states. §2.7.1 presents that attribute as an
   accompaniment to an already-typed value (a `dateTime` carrying "this is really an `xsd:time`") and §3.4
   makes `Value` required, so `E_BADTYPE` was the correct answer — and still is whenever the qualifier
   cannot stand in. What justifies bending: it only ever turns a rejected item into an accepted one, it
   cannot change how a conforming request decodes (an explicit `xsi:type` still wins, and the qualifier's
   narrowing pass still runs afterwards), and it is restricted to qualifiers naming an XSD scalar type this
   library can decode — a vendor QName or an array type stays the error it was
   (`TestItemValue_QualifierTypesAnUntypedValue`).

### What is the clients' own, not this server's

Recorded so the next reader does not mistake them for open defects:

- **pyopcxmlda treats two schema-defaulted attributes as mandatory** and crashes outright on either.
  `clients/pyopcxmlda/relax-optional-attributes.py` relaxes exactly those two expressions in its image,
  leaving all of its parsing logic intact, and fails the build loudly if either expression is no longer
  there. The two:
  - an `xsi:type` on every `BrowseElement`. Nothing in OPC XML-DA asks for it: `BrowseResponse` declares
    its `Elements` children as `BrowseElement` (Appendix B), a type that is not polymorphic, so there is
    nothing for `xsi:type` to disambiguate. Every other attribute that parser demands — `Name`,
    `ItemPath`, `ItemName`, `IsItem`, `HasChildren` — this server does send.
  - a `QualityField` on the `quality` property's value. `OPCQuality.QualityField` carries a schema default
    of `good` and §3.1.5 is explicit that good quality may omit it, which is what keeps a reply with one
    `<Quality>` per item from repeating `QualityField="good"` on every one.
- **pyopcxmlda's `SubscriptionPolledRefresh` parser has no fault branch,** so `E_NOSUBSCRIPTION` on a dead
  handle reaches it as an empty list. The server does send that fault; `rawrequest_test.go` and the Haskell
  client both assert it.
- **The Haskell client reads `SubscriptionCancelResponse`'s `ClientRequestHandle` as a child element,**
  where §3.7.2 p.68 and the schema both declare an attribute, so it always reports it absent. The server
  echoes it correctly (`TestHandleSubscriptionCancel_RoundTrip`, and the `subscriptioncancel` golden
  response).
- **Neither Python client decodes an `ArrayOf<X>` value.** The Haskell client does, and asserts an
  `ArrayOfDouble` arrives as three doubles.

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
