# OPC XML-DA 1.0 Specification Analysis

Source: `docs/OPCDataAccessXMLSpecification.pdf` — OPC XML-DA Specification, Version 1.0, Status: Released,
July 12, 2003, OPC Foundation. 100 pages; PDF page numbers and the printed page-footer numbers coincide
throughout (verified at multiple points), so all page references below are usable directly against the PDF.

This document is a structured analysis of the specification as it pertains to building a **server**. It is the
basis for `requirements.md` and `traceability-matrix.md`. Section numbers below match the specification's own
numbering.

## 1. Document structure

| # | Section | Pages |
|---|---|---|
| 1 | Introduction | 8-9 |
| 2 | Fundamental Concepts | 10-26 |
| 2.1 | SOAP | 10 |
| 2.2 | Name Space | 10 |
| 2.3 | OPC-XML-DA Server Detection | 10 |
| 2.4 | Locale IDs | 10-11 |
| 2.5 | Subscription Architecture | 11-20 |
| 2.5.1 | Basic Polled Refresh Approach | 12-13 |
| 2.5.2 | Advanced Polled Refresh Approach | 13-15 |
| 2.5.3 | Data Management Optimization | 15-17 |
| 2.5.4 | Buffered Data | 17-19 |
| 2.5.5 | Timestamps | 19-20 |
| 2.6 | Faults and Result Codes | 21-22 |
| 2.7 | Data Types for Item Values | 23-26 |
| 2.8 | Security | 25-26 |
| 2.9 | Compliance | 26 |
| 3 | OPC XML-DA Schema Reference | 27-79 |
| 3.1 | Base Schemas | 27-42 |
| 3.2 | GetStatus / GetStatusResponse | 43-46 |
| 3.3 | Read / ReadResponse | 47-50 |
| 3.4 | Write / WriteResponse | 51-55 |
| 3.5 | Subscribe / SubscribeResponse | 56-61 |
| 3.6 | SubscriptionPolledRefresh / …Response | 62-66 |
| 3.7 | SubscriptionCancel / …Response | 67-68 |
| 3.8 | Browse / BrowseResponse | 69-75 |
| 3.9 | GetProperties / GetPropertiesResponse | 76-79 |
| 4 | Transports | 80 |
| 5 | Appendix A — Patent Issues | 81-82 |
| 6 | Appendix B — Formal Schemas (WSDL) | 83-99 |

Appendix B restates section 3's structures plus the SOAP binding (`soap:operation style="document"`,
`soap:body use="literal"`, `soapAction="http://opcfoundation.org/webservices/XMLDA/1.0/<Operation>"` per
operation). The spec explicitly warns (p. 83) that this WSDL snapshot is for understanding only.

## 2. The 8 SOAP operations

General rule (p. 27): "All attributes are optional unless explicitly specified as required."

### GetStatus (§3.2, pp. 43-46)
No items. Request: `LocaleID`, `ClientRequestHandle` (both optional attributes). Response: `GetStatusResult`
(`ReplyBase`) + `Status` (`ServerStatus`: `StartTime` dateTime **required**, constant per server process;
`ProductVersion`; `StatusInfo`/`VendorInfo` locale-specific; `SupportedLocaleIDs` — at least 1 required;
`SupportedInterfaceVersions` — at least 1 required, only `"XML_DA_Version_1_0"` exists in v1.0). Faults:
`E_FAIL`, `E_OUTOFMEMORY`. No item-level codes (no items involved).

### Read (§3.3, pp. 47-50)
Request: `Options` (`RequestOptions`) + `ItemList` (at least one item, else whole-op `E_FAIL`); items carry
hierarchical `ItemPath`/`ReqType`/`MaxAge` (0/missing = most accurate/device read) plus `ItemName`,
`ClientItemHandle`. Synchronous, one-shot; server must preserve item order. Response: `ReadResult`
(`ReplyBase`) + `RItemList` (`ItemValue` per item, count matches request) + `Errors` (only if any ResultID
set). Per-item codes: `E_ACCESS_DENIED`, `E_BADTYPE`, `E_INVALIDITEMNAME`, `E_INVALIDITEMPATH`, `E_RANGE`,
`E_TIMEDOUT`, `E_UNKNOWNITEMNAME`, `E_UNKNOWNITEMPATH`, `E_WRITEONLY`. Faults: `E_FAIL`, `E_OUTOFMEMORY`,
`E_SERVERSTATE`, `E_TIMEDOUT`.

### Write (§3.4, pp. 51-55)
Request: `ReturnValuesOnReply` attribute + `Options` + `ItemList` (at least one item, else `E_FAIL`); each
item carries `Value` (required) and optionally `Quality`/`Timestamp`/`ValueTypeQualifier`. **Writing
Quality/Timestamp alongside Value must be atomic per item** — accept all or reject the whole item with
`E_NOTSUPPORTED`, never partial. Type coercion failure → `E_BADTYPE`. Response: `WriteResult` + `RItemList`
(`Value` only if `ReturnValuesOnReply=true`; `Timestamp` only if `ReturnItemTime=true`) + `Errors`. Per-item
codes: `E_ACCESS_DENIED`, `E_BADTYPE`, `E_INVALIDITEMID`, `E_INVALIDITEMNAME`, `E_INVALIDITEMPATH`,
`E_NOTSUPPORTED`, `E_RANGE`, `E_READONLY`, `E_TIMEDOUT`, `E_UNKNOWNITEMNAME`, `E_UNKNOWNITEMPATH`,
`E_WRITEONLY`, `S_CLAMP`. Faults: `E_FAIL`, `E_OUTOFMEMORY`, `E_SERVERSTATE`, `E_TIMEDOUT`.

### Subscribe (§3.5, pp. 56-61)
Request: `ReturnValuesOnReply` (required), `SubscriptionPingRate` (ms, 0 = server default liveness
algorithm) + `Options` + `ItemList` (at least one, else `E_FAIL`); items carry hierarchical `Deadband`
(0-100%, analog/array types, whole array re-reported if any element exceeds threshold), `RequestedSamplingRate`
(ms, 0 = fastest practical), `EnableBuffering`. Response: `ServerSubHandle` (empty string iff no item valid —
no subscription created); `SubscribeResult`; `RItemList` (`RevisedSamplingRate` at list/item level; values
only if `ReturnValuesOnReply=true`, `xsi:nil` if no value yet) + `Errors`. A subscription is created if **at
least one** item is valid. Per-item codes: `E_ACCESS_DENIED`, `E_BADTYPE`, `E_INVALIDITEMNAME`,
`E_INVALIDITEMPATH`, `E_RANGE`, `E_TIMEDOUT`, `E_UNKNOWNITEMNAME`, `E_UNKNOWNITEMPATH`, `E_WRITEONLY`,
`S_UNSUPPORTEDRATE`. Faults: `E_FAIL`, `E_OUTOFMEMORY`, `E_SERVERSTATE`, `E_TIMEDOUT`.

### SubscriptionPolledRefresh (§3.6, pp. 62-66; semantics also in §2.5.1-2.5.3, pp. 12-17)
Request: `HoldTime` (absolute dateTime; if absent, `WaitTime` is ignored), `WaitTime` (ms, additional wait
after HoldTime for a change), `ReturnAllItems` (true = ignore WaitTime, snapshot everything after HoldTime;
false = only changed items, early-return-on-change) + `Options` + `ServerSubHandles` (≥1 required; multiple
subscriptions pollable in one call). Response: `DataBufferOverflow` (bool), `InvalidServerSubHandles`,
`RItemList` (one per polled subscription, keyed by `SubscriptionHandle`; no entry at all if nothing changed
for that subscription and `ReturnAllItems=false`) + `Errors`. If `EnableBuffering=false`, only the latest
value per changed item; if `true`, all buffered changes. Server has a bounded buffer, purges **oldest**
first on overflow, always retains the Latest Changed Value per item. Per-item codes: `E_ACCESS_DENIED`,
`E_BADTYPE`, `E_RANGE`, `E_UNKNOWNITEMNAME`, `E_UNKNOWNITEMPATH`, `S_DATAQUEUEOVERFLOW`. Faults: `E_BUSY`
(subscription already being polled by another concurrent call), `E_FAIL`, `E_INVALIDHOLDTIME`,
`E_NOSUBSCRIPTION` (all handles invalid), `E_OUTOFMEMORY`, `E_SERVERSTATE`, `E_TIMEDOUT`.

### SubscriptionCancel (§3.7, pp. 67-68)
Request: `ServerSubHandle`, optional `ClientRequestHandle`. Response: echoed `ClientRequestHandle` only — no
`ReplyBase`, the leanest response in the spec. Cancelling a handle that is part of an in-flight, still-blocked
multi-handle `PolledRefresh` call causes that call to return immediately with the remaining handles' data.
Faults: `E_FAIL`, `E_OUTOFMEMORY`, `E_SERVERSTATE`.

### Browse (§3.8, pp. 69-75)
Single-level only (not recursive — client re-browses into a child's `ItemPath`/`ItemName`). Request:
`ItemPath`/`ItemName` (blank = root), `ContinuationPoint` (opaque, server-issued; must be echoed with
identical filters on follow-up, else `E_INVALIDCONTINUATIONPOINT`), `MaxElementsReturned` (0 = unlimited),
`BrowseFilter` (`all`/`branch`/`item`), `ElementNameFilter`, `VendorFilter` (undefined interaction with
`ElementNameFilter`, per spec), `ReturnAllProperties`/`ReturnPropertyValues`/`ReturnErrorText`,
`PropertyNames`. Response: `ContinuationPoint`, `MoreElements` (always present), `BrowseResult`, `Elements`
(`BrowseElement`: `Name`, `ItemPath`/`ItemName` — may be absent for a "hint" node, `IsItem` required,
`HasChildren` required — may conservatively report `true` if unknown) + `Errors`. A level with zero children,
or a filter yielding an empty set, is success, not an error. Faults: `E_FAIL`,
`E_INVALIDCONTINUATIONPOINT`, `E_INVALIDFILTER`, `E_INVALIDITEMNAME`, `E_INVALIDITEMPATH`, `E_OUTOFMEMORY`,
`E_SERVERSTATE`, `E_TIMEDOUT`, `E_UNKNOWNITEMNAME`, `E_UNKNOWNITEMPATH`.

### GetProperties (§3.9, pp. 76-79)
Request: `ItemIDs` (`ItemPath`+`ItemName` pairs), `PropertyNames`, `ReturnAllProperties`/
`ReturnPropertyValues`/`ReturnErrorText`. Response: `GetPropertiesResult` + `PropertyLists` (one per requested
item: `ItemPath`/`ItemName` echoed, `ResultID` if item unknown/invalid, `Properties`) + `Errors`. Per-item
codes: `E_FAIL`, `E_INVALIDITEMNAME`, `E_INVALIDITEMPATH`, `E_INVALIDPID`, `E_UNKNOWNITEMPATH`,
`E_UNKNOWNITEMNAME`, `E_WRITEONLY`. Faults: `E_FAIL`, `E_OUTOFMEMORY`, `E_SERVERSTATE`, `E_TIMEDOUT`.

## 3. Cross-cutting structures (§3.1, pp. 27-42)

- **Hierarchical parameters** (§3.1.1, p. 27): `ItemPath`, `ReqType`, `MaxAge`, `Deadband`,
  `RequestedSamplingRate`, `EnableBuffering` may be set at request/list/item level; the most specific
  non-nil value wins.
- **Null parameters** (§3.1.2, p. 28): `ItemPath=""` is a meaningful override, distinct from "not specified."
- **`RequestOptions`** (§3.1.6, pp. 34-36): `ReturnErrorText` (default true), `ReturnDiagnosticInfo` (default
  false), `ReturnItemTime` (default false), `ReturnItemPath` (default false), `ReturnItemName` (default
  false), `RequestDeadline` (optional dateTime — absolute; already-past deadline ⇒ whole-op `E_TIMEDOUT`
  fault; elapses mid-processing ⇒ per-item `E_TIMEDOUT`), `ClientRequestHandle`, `LocaleID`.
- **`ReplyBase`** (§3.1.8, p. 36): `RcvTime`/`ReplyTime` (both required dateTime), `ClientRequestHandle`
  (echoed), `RevisedLocaleID`, `ServerState` (required). Present on every response except
  `SubscriptionCancelResponse`.
- **`ServerState`** (§3.1.7, p. 36): `running`, `failed`, `noConfig`, `suspended`, `test`, `commFault`.
  `failed` ⇒ every call except GetStatus must fault; `suspended`/`noConfig` ⇒ data calls (Read/Write/
  Subscribe) must fault (§2.6, p. 21).
- **`ItemProperty`** (§3.1.10, pp. 38-42): `Name` (QName, required), `Description`, optional own
  `ItemPath`/`ItemName` (if directly addressable), `Value`, `ResultID`. Standard IDs 1-8 (`dataType`,
  `value`, `quality`, `timestamp`, `accessRights`, `scanRate`, `euType`, `euInfo`) and 100-108
  (`engineeringUnits`, `description`, `highEU`, `lowEU`, `highIR`, `lowIR`, `closeLabel`, `openLabel`,
  `timeZone`); 9-99/109-199 reserved for future OPC use, 300-399 reserved for Alarms & Events.

## 4. Error / result-code model (§2.6, pp. 21-22; §3.1.9, pp. 37-38)

Two independent channels:

1. **SOAP Fault** — whole-operation failure. Fault code is a QName in the XML-DA namespace (spec's own
   example on p. 21: `<faultcode xmlns:q0="...">q0:E_SERVERSTATE</faultcode>`), reusing the same E_/S_
   vocabulary as item-level codes.
2. **Per-item `ResultID`** (QName attribute on `ItemValue`/`ItemProperty`/`PropertyReplyList`) — operation
   succeeds overall, one item/property has a critical (`E_`) or non-critical-success (`S_`) condition.
   Absent = no abnormal condition. Responses carry a deduplicated `Errors` list (`OPCError` = `ID` + `Text`);
   multiple items sharing one code point to one `Errors` entry. Controlled by `RequestOptions.ReturnErrorText`
   (default true) and `ReturnDiagnosticInfo` (default false, adds a non-deduplicated verbose per-item
   `DiagnosticInfo` string).

Standard codes (p. 37-38): Success `S_CLAMP`, `S_DATAQUEUEOVERFLOW`, `S_UNSUPPORTEDRATE`. Error:
`E_ACCESS_DENIED`, `E_BUSY`, `E_FAIL`, `E_INVALIDCONTINUATIONPOINT`, `E_INVALIDFILTER`, `E_INVALIDHOLDTIME`,
`E_INVALIDITEMNAME`, `E_INVALIDITEMPATH`, `E_INVALIDPID`, `E_NOSUBSCRIPTION`, `E_NOTSUPPORTED`,
`E_OUTOFMEMORY`, `E_RANGE`, `E_READONLY`, `E_SERVERSTATE`, `E_TIMEDOUT`, `E_UNKNOWNITEMNAME`,
`E_UNKNOWNITEMPATH`, `E_WRITEONLY`. **Gap in the spec's own master table** (flagged, not invented):
`E_BADTYPE` and `E_INVALIDITEMID` are used in per-operation tables but never listed in §3.1.9 — treated here
as first-class standard codes anyway, consistent with their usage context.

## 5. Data type model (§2.7, pp. 23-26)

See `type-mapping.md` for the full table. Summary: XSD simple types map 1:1 to OPC Variant types
(`string`→VT_BSTR, … `dateTime`→VT_DATE). `Value` elements carry `xsi:type`. `time`/`date`/`duration` are
wire-transmitted as `dateTime`/`string` with a disambiguating `ValueTypeQualifier` QName attribute. Arrays use
named `ArrayOf<X>` types with repeated scalar-typed child elements (confirmed against
`testdata/responses/subscribe_680.response.xml`: `<Value xsi:type="ns1:ArrayOfUnsignedShort">
<unsignedShort>0</unsignedShort>...`) — **no `ArrayOfUnsignedByte`** exists; unsigned-byte sequences always
use `base64Binary`. `ArrayOfAnyType` allows heterogeneous/nested arrays, each element independently
`xsi:type`-tagged. Enumerations use the `euType`/`euInfo` `ItemProperty` mechanism, not `xsd:enumeration`.

## 6. Quality model — `OPCQuality` (§3.1.5, pp. 30-33)

`QualityField` (16-value enum, default `good`), `LimitField` (4-value enum, default `none`), `VendorField`
(unsignedByte, default 0, vendor-specific meaning). Behavior: Good quality may omit `QualityField` entirely;
Bad quality ⇒ omit `Value` unless a last-known-value exists (then `QualityField="badLastKnownValue"` + stale
value); Uncertain quality ⇒ must return a "reasonable" value. `Timestamp` presence follows the same
Good/Bad/Uncertain matrix **and** is gated by `RequestOptions.ReturnItemTime` — these are two independent
conditions, not one.

## 7. Timestamps and locale (§2.4 pp. 10-11; §2.5.5 pp. 19-20; §3.1.6 pp. 34-35; §3.1.8 p. 36)

`ItemValue.Timestamp`: most accurate time the server can associate with a value (device-acquisition or
cache-update time); one Timestamp per whole array value; only serialized if `ReturnItemTime=true`.
`ReplyBase.RcvTime`/`ReplyTime` always required — lets clients gauge clock skew/RTT, explicitly recommended
as the basis for `RequestDeadline`. `ServerStatus.StartTime` constant per process. `LocaleID` format
`<lang>[-<country>]` per RFC 3066/ISO 639/3166, fully independent of time-zone handling (which is instead
covered per-item via `ItemProperty` id 108 `timeZone`).

## 8. Namespaces (§2.2 p. 10; §Appendix B pp. 83-99)

XML-DA target namespace: `http://opcfoundation.org/webservices/XMLDA/1.0/` (also the WSDL retrieval URI).
SOAP namespaces: `http://schemas.xmlsoap.org/soap/envelope/` (1.1) is what the spec's own examples use;
real-world servers/clients (see `testdata/faults/`) also produce SOAP 1.2 faults and non-conformant legacy
shapes. `elementFormDefault="qualified"`; binding style `document`/`literal`; `soapAction` =
namespace + operation name. **Prefixes are not fixed identity** — confirmed in `testdata/responses/
subscribe_680.response.xml`, where the OPC namespace is bound both as the default `xmlns` on the response
root *and* as prefix `ns1` used only inside `xsi:type` attribute values in the same document.

## 9. Security (§2.8, pp. 25-26)

The spec delegates all authentication/transport-security to the transport layer ("the assumption ... is that
the transport will handle security, e.g., HTTPS"). No in-protocol auth is defined. Explicit recommendation:
servers should provide "a means to globally disable the Server's 'write' capabilities" — tied directly to
`E_ACCESS_DENIED` ("typically caused by Web Service security, e.g. globally disabled write capabilities").
No item-level ACL model is defined by the spec itself.

## 10. Explicitly unspecified by the spec (implementation-policy territory)

No mention anywhere of: max items per request, max message/request size, max concurrent subscriptions, max
items per subscription, or rate limiting. Only soft guidance exists ("server specific maximum time will
generally be no more than a minute or two," §3.1.6). These are implementation decisions for this library,
documented as such in `docs/architecture/decisions/` and `server.Config`, never presented as spec mandates.

## 11. Real-world fixture observations (`testdata/`)

- `testdata/requests/subscribe_679.request.xml` / `testdata/responses/subscribe_680.response.xml`: a real
  Subscribe request/response pair (client handles between the two files don't line up 1:1 despite sequential
  naming — treat as independently useful fixtures, not a strictly matched pair).
- Three fault fixtures showing real-world non-conformance: a SOAP 1.1 fault with unqualified,
  non-namespaced `faultcode`/`faultstring`/`detail` all set to the literal text `E_NOSUBSCRIPTION` (contrast
  with the spec's own QName-qualified example); a SOAP 1.1 fault for a pre-operation XML parse error
  (`faultcode=SOAP-ENV:Client`, generic text, no OPC content); a SOAP 1.2 fault (`Code`/`Reason`/`Detail`
  structure, different envelope namespace entirely) for a request deserialization failure (invalid
  `dateTime`). Implication: this library's fault **parsing** must tolerate all observed shapes; its fault
  **emission** picks one consistent, spec-conformant shape (see ADR-004).
