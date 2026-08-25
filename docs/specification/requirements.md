# Requirements

Extracted from `specification-analysis.md` (source: `docs/OPCDataAccessXMLSpecification.pdf`). Each
requirement gets a stable ID used throughout `traceability-matrix.md`, code comments, and test names.
Priority: **M**ust (spec mandates this for a conformant server), **S**hould (spec strongly recommends),
**O**ptional (spec allows but does not require).

## REQ-NS — Namespaces

| ID | Description | Source | Priority |
|---|---|---|---|
| REQ-NS-001 | All 8 operations' request/response elements live in one namespace `http://opcfoundation.org/webservices/XMLDA/1.0/`, document/literal SOAP binding, `soapAction` = namespace + operation name. | §2.2 p.10; §4 p.80; App. B pp.83-99 | M |
| REQ-NS-002 | Namespace identity must be resolved by URI, never by prefix, including prefixes that appear inside attribute *values* (e.g. `xsi:type`). | Observed in `testdata/responses/subscribe_680.response.xml`; general SOAP/XML correctness | M |

## REQ-SERVER — Server-wide behavior

| ID | Description | Source | Priority |
|---|---|---|---|
| REQ-SERVER-001 | Server implements 8 operations: GetStatus, Read, Write, Subscribe, SubscriptionPolledRefresh, SubscriptionCancel, Browse, GetProperties. | §3 pp.27-79 | M |
| REQ-SERVER-002 | `ServerState=failed` ⇒ every operation except GetStatus must return a SOAP fault. `ServerState∈{suspended,noConfig}` ⇒ Read/Write/Subscribe must fault. | §2.6 p.21 | M |
| REQ-SERVER-003 | Unknown/unrecognized operation in the SOAP Body produces a clean SOAP fault, never a crash or silent drop. | Implementation requirement (spec silent on the exact code) | M |

## REQ-STATUS — GetStatus

| ID | Description | Source | Priority |
|---|---|---|---|
| REQ-STATUS-001 | Request carries optional `LocaleID`, `ClientRequestHandle`. | §3.2 p.43 | M |
| REQ-STATUS-002 | Response includes `ReplyBase` + `ServerStatus`. | §3.2 p.44 | M |
| REQ-STATUS-003 | `StartTime` required, constant across the server process lifetime. | §3.2 p.44 | M |
| REQ-STATUS-004 | `SupportedLocaleIDs` must list at least 1 entry. | §3.2 p.45 | M |
| REQ-STATUS-005 | `SupportedInterfaceVersions` must list at least 1 entry; v1.0 library always includes `XML_DA_Version_1_0`. | §3.2 p.45 | M |

## REQ-READ — Read

| ID | Description | Source | Priority |
|---|---|---|---|
| REQ-READ-001 | Request items support hierarchical `ItemPath`/`ReqType`/`MaxAge` (request/list/item level, most specific wins). | §3.1.1 p.27; §3.3 p.47 | M |
| REQ-READ-002 | At least one item required; empty item list ⇒ whole-operation `E_FAIL`. | §3.3 p.47 | M |
| REQ-READ-003 | Response item order must match request item order, 1:1. | §3.3 p.48 | M |
| REQ-READ-004 | `MaxAge=0`/missing ⇒ most accurate/device read. | §3.3 p.47 | M |
| REQ-READ-005 | Per-item abnormal codes: `E_ACCESS_DENIED`, `E_BADTYPE`, `E_INVALIDITEMNAME`, `E_INVALIDITEMPATH`, `E_RANGE`, `E_TIMEDOUT`, `E_UNKNOWNITEMNAME`, `E_UNKNOWNITEMPATH`, `E_WRITEONLY`. | §3.3 pp.49-50 | M |
| REQ-READ-006 | Whole-operation faults: `E_FAIL`, `E_OUTOFMEMORY`, `E_SERVERSTATE`, `E_TIMEDOUT`. | §3.3 p.50 | M |

## REQ-WRITE — Write

| ID | Description | Source | Priority |
|---|---|---|---|
| REQ-WRITE-001 | `ReturnValuesOnReply` attribute controls whether written values are echoed back. | §3.4 p.51 | M |
| REQ-WRITE-002 | At least one item required; empty item list ⇒ whole-operation `E_FAIL`. | §3.4 p.51 | M |
| REQ-WRITE-003 | Writing Quality/Timestamp alongside Value must be atomic per item: accept all or reject the whole item with `E_NOTSUPPORTED`; never partially apply. | §3.4 p.52 | M |
| REQ-WRITE-004 | Type coercion failure ⇒ per-item `E_BADTYPE`. | §3.4 p.52 | M |
| REQ-WRITE-005 | Successful write with value clamped to valid range ⇒ per-item `S_CLAMP`, write still succeeds. | §3.4 p.54 | M |
| REQ-WRITE-006 | Per-item abnormal codes: `E_ACCESS_DENIED`, `E_BADTYPE`, `E_INVALIDITEMID`, `E_INVALIDITEMNAME`, `E_INVALIDITEMPATH`, `E_NOTSUPPORTED`, `E_RANGE`, `E_READONLY`, `E_TIMEDOUT`, `E_UNKNOWNITEMNAME`, `E_UNKNOWNITEMPATH`, `E_WRITEONLY`. | §3.4 pp.54-55 | M |
| REQ-WRITE-007 | Whole-operation faults: `E_FAIL`, `E_OUTOFMEMORY`, `E_SERVERSTATE`, `E_TIMEDOUT`. | §3.4 p.55 | M |

## REQ-SUBSCRIPTION — Subscribe / SubscriptionPolledRefresh / SubscriptionCancel

| ID | Description | Source | Priority |
|---|---|---|---|
| REQ-SUBSCRIPTION-001 | `ReturnValuesOnReply` (required) + `SubscriptionPingRate` (liveness/abandonment timer) on Subscribe request. | §3.5 pp.56-57 | M |
| REQ-SUBSCRIPTION-002 | Subscription created iff at least one item valid; `ServerSubHandle` empty string signals "no subscription created." | §3.5 p.58 | M |
| REQ-SUBSCRIPTION-003 | Hierarchical per-item `RequestedSamplingRate`, `EnableBuffering`, `Deadband` (0-100%, analog/array types, whole array re-reported if any element exceeds threshold); server echoes `RevisedSamplingRate`. | §2.5.3 pp.15-17; §3.5 pp.57-59 | M |
| REQ-SUBSCRIPTION-004 | `SubscriptionPolledRefresh` polls a *set* of `ServerSubHandles` in one call. | §3.6 p.62 | M |
| REQ-SUBSCRIPTION-005 | `HoldTime` (absolute) + `WaitTime` (relative, ignored if HoldTime absent) implement the Hold+Wait "simulated callback" pattern with early-return-on-change. | §2.5.2 pp.13-15; §3.6 p.62 | M |
| REQ-SUBSCRIPTION-006 | `ReturnAllItems=true` ⇒ ignore WaitTime, return full snapshot after HoldTime; `false` ⇒ changed items only. | §3.6 pp.62-63 | M |
| REQ-SUBSCRIPTION-007 | Buffering: bounded buffer, oldest data purged first on overflow, Latest Changed Value per item always retained; `DataBufferOverflow`/`S_DATAQUEUEOVERFLOW` signal this. | §2.5.4 pp.17-19; §3.6 p.65 | M |
| REQ-SUBSCRIPTION-008 | Unrecognized handles ⇒ `InvalidServerSubHandles`; all handles invalid ⇒ whole-op `E_NOSUBSCRIPTION`. | §3.6 pp.64,66 | M |
| REQ-SUBSCRIPTION-009 | A subscription must not be concurrently polled by two overlapping `PolledRefresh` calls ⇒ `E_BUSY`. | §3.6 p.66 | M |
| REQ-SUBSCRIPTION-010 | `SubscriptionCancel` frees resources, invalidates the handle; if part of an in-flight blocked multi-handle PolledRefresh, that call returns immediately with the remaining data. | §3.7 pp.67-68 | M |
| REQ-SUBSCRIPTION-011 | `SubscriptionCancelResponse` has no `ReplyBase` — only an echoed `ClientRequestHandle`. | §3.7 p.68 | M |
| REQ-SUBSCRIPTION-012 | Timestamp semantics differ for sampled vs. exception-based items, and again under `ReturnAllItems=true` (unchanged items get current timestamp with last-known value). | §2.5.5 pp.19-20 | M |
| REQ-SUBSCRIPTION-013 | Server may abandon/clean up a subscription the client hasn't polled within approximately `SubscriptionPingRate`. | §2.5.2 p.14; §3.5 p.57 | S |
| REQ-SUBSCRIPTION-014 | `SubscriptionCancel` on an unknown or already-cancelled handle is a safe, idempotent no-op success (no error slot exists in the response to report otherwise). | §3.7 p.68 (response shape); open-questions.md OQ-9 | M |
| REQ-SUBSCRIPTION-015 | `SubscriptionPingRate=0` means "use the server's own default," not a literal zero-duration interval; the resolved nonzero value is used for abandonment/reaper calculations. | §3.5.1 p.57; open-questions.md OQ-10 | M |

## REQ-BROWSE — Browse

| ID | Description | Source | Priority |
|---|---|---|---|
| REQ-BROWSE-001 | Single-level browse only; client re-browses into a child's `ItemPath`/`ItemName` to descend. | §3.8 p.71 | M |
| REQ-BROWSE-002 | `ContinuationPoint` must be echoed with identical filters on follow-up calls, else `E_INVALIDCONTINUATIONPOINT`. | §3.8 p.70 | M |
| REQ-BROWSE-003 | `MaxElementsReturned`/`MoreElements` pagination; `MoreElements` always present in the response. | §3.8 p.70 | M |
| REQ-BROWSE-004 | `BrowseFilter` (`all`/`branch`/`item`) filters results per the `IsItem`/`HasChildren` truth table. | §3.8 pp.70-71 | M |
| REQ-BROWSE-005 | `BrowseElement.IsItem` and `HasChildren` are both required booleans; `HasChildren` may conservatively report `true` if unknown. | §3.8 p.72 | M |
| REQ-BROWSE-006 | A level with zero children, or a filter yielding an empty set, is a successful empty result, not an error. | §3.8 p.71 | M |
| REQ-BROWSE-007 | Browse may inline `ItemProperty` data per element via `ReturnAllProperties`/`ReturnPropertyValues`. | §3.8 p.70 | O |
| REQ-BROWSE-008 | Whole-operation faults: `E_FAIL`, `E_INVALIDCONTINUATIONPOINT`, `E_INVALIDFILTER`, `E_INVALIDITEMNAME`, `E_INVALIDITEMPATH`, `E_OUTOFMEMORY`, `E_SERVERSTATE`, `E_TIMEDOUT`, `E_UNKNOWNITEMNAME`, `E_UNKNOWNITEMPATH`. | §3.8 p.75 | M |

## REQ-PROPERTIES — GetProperties

| ID | Description | Source | Priority |
|---|---|---|---|
| REQ-PROPERTIES-001 | Request carries `ItemIDs` (ItemPath+ItemName pairs) and optional `PropertyNames` filter. | §3.9 pp.76-77 | M |
| REQ-PROPERTIES-002 | Response has one `PropertyReplyList` per requested item, with its own `ResultID` if the item is unknown/invalid. | §3.9 p.78 | M |
| REQ-PROPERTIES-003 | Standard property IDs 1-8 (`dataType`,`value`,`quality`,`timestamp`,`accessRights`,`scanRate`,`euType`,`euInfo`) and 100-108 (`engineeringUnits`,`description`,`highEU`,`lowEU`,`highIR`,`lowIR`,`closeLabel`,`openLabel`,`timeZone`) are recognized. | §3.1.10 pp.38-42 | M |
| REQ-PROPERTIES-004 | Per-item abnormal codes: `E_FAIL`, `E_INVALIDITEMNAME`, `E_INVALIDITEMPATH`, `E_INVALIDPID`, `E_UNKNOWNITEMPATH`, `E_UNKNOWNITEMNAME`, `E_WRITEONLY`. | §3.9 p.79 | M |

## REQ-TYPE — Data types

| ID | Description | Source | Priority |
|---|---|---|---|
| REQ-TYPE-001 | XSD scalar types map 1:1 to OPC Variant types per the table in `type-mapping.md`. | §2.7.1 pp.23-24 | M |
| REQ-TYPE-002 | `time`/`date`/`duration` are wire-transmitted as `dateTime`/`string` with a disambiguating `ValueTypeQualifier` QName attribute. | §2.7.1 pp.23-24 | M |
| REQ-TYPE-003 | Arrays use named `ArrayOf<X>` types, repeated scalar-typed child elements (not a generic wrapper); no `ArrayOfUnsignedByte` (use `base64Binary` instead). | §2.7.3 pp.24-25; confirmed in `testdata/responses/subscribe_680.response.xml` | M |
| REQ-TYPE-004 | `ArrayOfAnyType` permits heterogeneous/nested arrays, each element independently `xsi:type`-tagged. | §2.7.3 p.25 | M |
| REQ-TYPE-005 | Enumerated values use the `euType`/`euInfo` `ItemProperty` mechanism, not `xsd:enumeration`. | §2.7.2 p.24 | M |
| REQ-TYPE-006 | `ReqType` on a Read request item asks the server to coerce the return value to a specific type; unsupported conversion ⇒ `E_BADTYPE`. | §3.1.3 p.29 | M |
| REQ-TYPE-007 | Unknown/vendor/custom `xsi:type` values must not crash the server; preserve for round-trip where feasible. | Implementation requirement (robustness) | M |
| REQ-TYPE-008 | A `Value` may be explicitly `xsi:nil="true"` while still declaring its type — representing "no value, but known type" distinct from "value entirely absent" (nil `*Value` at the `ItemValue` level, planned WP-4) and distinct from a present-but-empty value (e.g. empty string). | Reference client behavior for write-only items on Read; mega-prompt §9 "nil-Werte" | M |

## REQ-QUALITY — OPCQuality

| ID | Description | Source | Priority |
|---|---|---|---|
| REQ-QUALITY-001 | `QualityField` (16-value enum, default `good`), `LimitField` (4-value enum, default `none`), `VendorField` (unsignedByte, default 0). | §3.1.5 pp.30-31 | M |
| REQ-QUALITY-002 | Good quality may omit `QualityField` entirely on the wire. | §3.1.5 p.31 | M |
| REQ-QUALITY-003 | Bad quality ⇒ omit `Value` unless a last-known-value exists, in which case `QualityField=badLastKnownValue` plus the stale value. | §3.1.5 pp.32-33 | M |
| REQ-QUALITY-004 | Uncertain quality ⇒ server must still return a "reasonable" value. | §3.1.5 p.32 | M |
| REQ-QUALITY-005 | `Timestamp` presence is gated by both quality state and `RequestOptions.ReturnItemTime` — two independent conditions. | §3.1.5 pp.31-33 | M |

## REQ-ERROR — Error / result-code model

| ID | Description | Source | Priority |
|---|---|---|---|
| REQ-ERROR-001 | Whole-operation failures use SOAP Fault with a QName fault code in the XML-DA namespace. | §2.6 p.21 | M |
| REQ-ERROR-002 | Item/property-level conditions use the `ResultID` attribute; absence means no abnormal condition. | §2.6 p.22 | M |
| REQ-ERROR-003 | Responses carry a deduplicated `Errors` list (`OPCError` = ID + Text); items sharing a code share one entry. | §2.6 p.22; §3.1.9 | M |
| REQ-ERROR-004 | `ReturnErrorText` (default true) controls whether `Errors` text is populated; `ReturnDiagnosticInfo` (default false) adds non-deduplicated per-item diagnostic text. | §3.1.6 pp.34-35 | M |
| REQ-ERROR-005 | Standard E_/S_ code vocabulary per `error-mapping.md`; vendor codes must use a vendor namespace and the same E_/S_ convention. | §3.1.9 pp.37-38 | M |

## REQ-TIME — Time and locale

| ID | Description | Source | Priority |
|---|---|---|---|
| REQ-TIME-001 | `ReplyBase.RcvTime`/`ReplyTime` are always required on every response that has a `ReplyBase`. | §3.1.8 p.36 | M |
| REQ-TIME-002 | `RequestDeadline`, if already past at receipt, causes a whole-operation `E_TIMEDOUT` fault; if it elapses mid-processing, causes per-item `E_TIMEDOUT`. | §3.1.6 pp.34-35 | M |
| REQ-TIME-003 | `LocaleID` format `<lang>[-<country>]`; `RevisedLocaleID` echoes the locale actually used. | §2.4 pp.10-11 | M |

## REQ-SECURITY — Security

| ID | Description | Source | Priority |
|---|---|---|---|
| REQ-SECURITY-001 | The protocol defines no in-band authentication/encryption; security is delegated to the transport (e.g. HTTPS). The library must not hard-code a specific auth mechanism. | §2.8 pp.25-26 | M |
| REQ-SECURITY-002 | Server should provide a means to globally disable write capability (read-only mode), tied to `E_ACCESS_DENIED`. | §2.8 p.25 | S |

## REQ-CONFIG — Configuration (implementation policy, not spec-mandated)

| ID | Description | Source | Priority |
|---|---|---|---|
| REQ-CONFIG-001 | `Config.DefaultSubscriptionPingRate` supplies the nonzero ping rate substituted whenever a client sends `SubscriptionPingRate=0`. | Derived from REQ-SUBSCRIPTION-015; ADR-011 | — (implementation decision) |

## REQ-LIMITS — Resource limits (implementation policy, not spec-mandated)

| ID | Description | Source | Priority |
|---|---|---|---|
| REQ-LIMITS-001 | Max items per request/subscription, max concurrent subscriptions, max request body size, and rate limiting are all unspecified by the spec; this library defines conservative configurable defaults. | Spec silence, throughout | — (implementation decision, see ADR-011) |
