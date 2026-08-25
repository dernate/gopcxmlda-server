# Traceability Matrix

Maps each requirement in `requirements.md` to its target Go package and planned/actual tests. Status values:
`not started`, `in progress`, `implemented`, `tested`, `blocked` (see `open-questions.md` /
`docs/development/tasks.md` for blockers). This file is updated at the end of every milestone — see
`docs/development/implementation-plan.md`.

| REQ ID | Package | Planned test(s) | Status |
|---|---|---|---|
| REQ-NS-001 | xmlda | `xmlda/dispatch_test.go` — dispatch by URI across alternative prefixes | tested |
| REQ-NS-002 | xmlda | `xmlda/namespace_test.go` — resolveQName with alternative/missing/wrong-URI prefixes | tested |
| REQ-SERVER-001 | xmlda, server | `xmlda/*_test.go` wire round-trip; `server/*_test.go` — HTTP round trip for all 8 operations | tested |
| REQ-SERVER-002 | xmlda, server | `xmlda/replybase_test.go` — `RequiresFault` matrix; `server/dispatch_test.go` — ServerState fault matrix | tested |
| REQ-SERVER-003 | xmlda, server | `xmlda/dispatch_test.go`, `server/dispatch_test.go` — unknown operation | tested |
| REQ-STATUS-001 | xmlda, server | `xmlda/getstatus_test.go` (incl. `TestGetStatusRequest_RealFixture`); `server/getstatus_test.go` | tested |
| REQ-STATUS-002 | xmlda, backend, server | `xmlda/getstatus_test.go` (incl. `TestGetStatusResponse_RealFixture`); `server/getstatus_test.go` | tested |
| REQ-STATUS-003 | server | `server/getstatus_test.go` — `TestHandleGetStatus_StartTimeConstantAcrossCalls` | tested |
| REQ-STATUS-004 | xmlda | `xmlda/getstatus_test.go` — wire round-trip; "at least 1" is a backend-authoring contract, not separately enforced by this library | tested (wire-level) |
| REQ-STATUS-005 | xmlda, server | `xmlda/getstatus_test.go`; `server/getstatus_test.go` — always includes `InterfaceVersion10` | tested |
| REQ-READ-001 | xmlda | `xmlda/itemparams_test.go` — hierarchical override precedence; real-fixture 7-segment item path in `TestReadRequest_RealFixture_ArrayOfDouble` | tested |
| REQ-READ-002 | xmlda, server | `xmlda/read_test.go` (incl. `TestReadRequest_RealFixture`); `server/read_test.go` — `TestHandleRead_EmptyItemList_Faults` | tested |
| REQ-READ-003 | xmlda, server | `xmlda/read_test.go` (incl. `TestReadResponse_RealFixture`); `server/read_test.go` — `TestHandleRead_OrderPreservedAcrossMultipleItems` | tested |
| REQ-READ-004 | backend, server | MaxAge is threaded through to `backend.ReadRequestItem.MaxAge`; no dedicated test of backend-side MaxAge *semantics* (that's the backend's own responsibility, not this library's) | implemented |
| REQ-READ-005 | xmlda, server | `xmlda/read_test.go`; `server/read_test.go` — `TestHandleRead_PartialSuccess` | tested |
| REQ-READ-006 | server | `server/dispatch_test.go` — ServerState fault matrix covers Read | tested |
| REQ-WRITE-001 | xmlda, server | `xmlda/write_test.go`; `server/write_test.go` — `TestHandleWrite_ReturnValuesOnReply` | tested |
| REQ-WRITE-002 | xmlda, server | `xmlda/write_test.go`; `server/write_test.go` — `TestHandleWrite_EmptyItemList_Faults` | tested |
| REQ-WRITE-003 | xmlda, backend, server | `xmlda/write_test.go` wire round-trip; atomic Quality/Timestamp detection fixed and used in `server/write.go` (independent tracking bug caught and fixed — see tasks.md) | implemented |
| REQ-WRITE-004 | server | `server/write.go` — a Write item with no `<Value>` element (a semantic REQ-WRITE-003 violation) resolves to E_BADTYPE; `server/write_test.go` — `TestHandleWrite_MissingValueElement_NoPanic` | tested |
| REQ-WRITE-005 | xmlda, server | `xmlda/write_test.go`; `server/write.go` propagates `WriteOutcome.Clamped` ⇒ S_CLAMP | tested |
| REQ-WRITE-006 | server | `server/write_test.go` — `TestHandleWrite_UnknownItem` | tested |
| REQ-WRITE-007 | server | `server/dispatch_test.go` — ServerState fault matrix covers Write | tested |
| REQ-SUBSCRIPTION-001 | xmlda, subscription, server | `xmlda/subscribe_test.go` golden fixture; `subscription/create_test.go`; `server/subscribe_test.go` | tested |
| REQ-SUBSCRIPTION-002 | xmlda, subscription, server | `subscription/create_test.go` — `TestCreate_AllInvalidItems_EmptyHandle`; `server/subscribe_test.go` | tested |
| REQ-SUBSCRIPTION-003 | subscription | `subscription/refresh_test.go` — buffering/deadband tests exercise these fields end-to-end | tested |
| REQ-SUBSCRIPTION-004 | xmlda, subscription | `xmlda/subscriptionpolledrefresh_test.go` (incl. `TestSubscriptionPolledRefreshRequest_RealFixture`); `subscription/refresh_test.go` — `TestPolledRefresh_MultipleHandlesInOneCall` | tested |
| REQ-SUBSCRIPTION-005 | subscription, server | `subscription/refresh_test.go` — Hold+Wait, early-return-on-change, change-during-hold; `server/subscribe_test.go` shutdown ordering; real-fixture request coverage in `TestSubscriptionPolledRefreshRequest_RealFixture` | tested |
| REQ-SUBSCRIPTION-006 | subscription | `subscription/refresh_test.go` — `TestPolledRefresh_ReturnAllItems_IgnoresWaitTime`; real-fixture response coverage in `TestSubscriptionPolledRefreshResponse_RealFixture` | tested |
| REQ-SUBSCRIPTION-007 | xmlda, subscription | `subscription/refresh_test.go` — buffering enabled/disabled/overflow | tested |
| REQ-SUBSCRIPTION-008 | xmlda, subscription, server | `subscription/refresh_test.go`; `server/subscribe_test.go` — `TestHandlePolledRefresh_InvalidHandle` | tested |
| REQ-SUBSCRIPTION-009 | subscription | `subscription/refresh_test.go` + `stress_test.go` — E_BUSY incl. disjoint-handle non-false-positive; `-race` clean (re-verified 2026-08-24, see tasks.md) | tested |
| REQ-SUBSCRIPTION-010 | subscription | `subscription/refresh_test.go` — mid-hold cancel/shutdown; real-fixture coverage in `TestSubscriptionCancelRequest_RealFixture` | tested |
| REQ-SUBSCRIPTION-011 | xmlda, server | `xmlda/subscriptioncancel_test.go` — no ReplyBase (incl. `TestSubscriptionCancelResponse_RealFixture`); `server/subscribe_test.go` | tested |
| REQ-SUBSCRIPTION-012 | subscription | `subscription/refresh_test.go` — general Timestamp propagation exercised; the spec's more elaborate sampled-vs-exception-based timing rules (§2.5.5) are not separately tested | in progress |
| REQ-SUBSCRIPTION-013 | subscription | `subscription/reaper_test.go` — fake-clock reaper sweep (abandons/spares/selective) | tested |
| REQ-SUBSCRIPTION-014 | subscription, server | `subscription/create_test.go` — `TestCancel_Idempotent`; `server/subscribe_test.go` — double cancel | tested |
| REQ-SUBSCRIPTION-015 | subscription | `subscription/create_test.go` — `TestCreate_PingRateZero_ResolvesToDefault` | tested |
| REQ-BROWSE-001 | xmlda, server | `xmlda/browse_test.go` (incl. `TestBrowseRequest/Response_RealFixture_Root/Deep`); `server/browse_test.go` | tested |
| REQ-BROWSE-002 | server | `server/browse_test.go` — `TestHandleBrowse_ContinuationPointMismatch`/`SameFilters` | tested |
| REQ-BROWSE-003 | xmlda, server | `xmlda/browse_test.go`; `server/browse_test.go` | tested |
| REQ-BROWSE-004 | xmlda, backend, server | `xmlda/browse_test.go` wire round-trip; filter application is the backend's own responsibility (server passes `Filter` through unchanged) | implemented |
| REQ-BROWSE-005 | xmlda | `xmlda/browse_test.go` — required bool fields always present; real-fixture coverage in `TestBrowseResponse_RealFixture_Root/Deep` | tested |
| REQ-BROWSE-006 | xmlda, server | `xmlda/browse_test.go`; `server/browse_test.go` — `TestHandleBrowse_EmptyResultIsSuccess` | tested |
| REQ-BROWSE-007 | xmlda, server | `xmlda/browse_test.go`; `server/browse_test.go` — `TestHandleBrowse_RoundTrip` inline properties; real-fixture coverage in `TestBrowseResponse_RealFixture_Deep` | tested |
| REQ-BROWSE-008 | server | `server/browse_test.go` — `TestHandleBrowse_NotSupportedWithoutBrowser`; ServerState fault matrix | tested |
| REQ-PROPERTIES-001 | xmlda, server | `xmlda/getproperties_test.go` (incl. `TestGetPropertiesRequest_RealFixture`); `server/getproperties_test.go` | tested |
| REQ-PROPERTIES-002 | xmlda, server | `xmlda/getproperties_test.go` (incl. `TestGetPropertiesResponse_RealFixture`); `server/getproperties_test.go` — `TestHandleGetProperties_UnknownItem` | tested |
| REQ-PROPERTIES-003 | xmlda | `xmlda/itemproperty_test.go` — standard property ID table | tested |
| REQ-PROPERTIES-004 | server | `server/getproperties_test.go` — `TestHandleGetProperties_UnknownItem` | tested |
| REQ-TYPE-001 | xmlda | `xmlda/value_test.go` — table-driven scalar round-trip; real `double` coverage in `TestReadResponse_RealFixture_ArrayOfDouble` | tested |
| REQ-TYPE-002 | xmlda | `xmlda/value_test.go` — time/date/duration (direct xsi:type per OQ-12) | tested |
| REQ-TYPE-003 | xmlda | `xmlda/value_test.go` — array round-trip incl. golden fixture (`ArrayOfUnsignedShort`); real `ArrayOfDouble` coverage in `TestReadResponse_RealFixture_ArrayOfDouble` | tested |
| REQ-TYPE-004 | xmlda | `xmlda/value_test.go` — ArrayOfAnyType nested/heterogeneous | tested |
| REQ-TYPE-005 | xmlda | `xmlda/itemproperty_test.go` — standard property IDs incl. euType/euInfo (7/8) | tested |
| REQ-TYPE-006 | server | `server/coerce.go` — numeric-to-numeric coercion with range checks; `server/coerce_test.go` — `TestCoerceToReqType_NilOrAnyType_Unchanged`, `_SameType_Unchanged`, `_NonNumericTarget_Fails`, `_NumericToNumeric_Succeeds`, `TestNumericToScalar_Int64Boundaries`, `_Uint64Boundaries`, `_OtherWidths_RangeChecked` | tested |
| REQ-TYPE-007 | xmlda | `xmlda/value_test.go` — unknown xsi:type round-trip, fuzz seeds | tested |
| REQ-TYPE-008 | xmlda | `xmlda/value_test.go` — TestValue_Nil | tested |
| REQ-QUALITY-001 | xmlda | `xmlda/quality_test.go` — enum coverage | tested |
| REQ-QUALITY-002 | xmlda | `xmlda/quality_test.go` — Good omits QualityField on encode | tested |
| REQ-QUALITY-003 | xmlda, server | `xmlda/quality_test.go`; `server/itemvalue.go` applies `ResolveValuePresence` in every response path | tested |
| REQ-QUALITY-004 | xmlda | `xmlda/quality_test.go` — ResolveValuePresence Uncertain | tested |
| REQ-QUALITY-005 | server | `server/itemvalue.go`'s `buildItemValue` gates Timestamp by `ReturnItemTime`; no single dedicated cross-operation gating test | implemented |
| REQ-ERROR-001 | soap, xmlda | `soap/fault_test.go` | tested |
| REQ-ERROR-002 | xmlda | `xmlda/errors_test.go` | tested |
| REQ-ERROR-003 | xmlda | `xmlda/errors_test.go` — DedupeErrors | tested |
| REQ-ERROR-004 | xmlda, server | `server/options_test.go` — `TestHandleRead_ReturnErrorTextGating`/`ReturnDiagnosticInfoGating` | tested |
| REQ-ERROR-005 | xmlda | `xmlda/errors_test.go` — standard + vendor codes | tested |
| REQ-TIME-001 | xmlda | `xmlda/replybase_test.go` | tested |
| REQ-TIME-002 | server | `server/options_test.go` — `TestHandleRead_RequestDeadlineAlreadyPassed_Faults`/`InFuture_Succeeds` | tested |
| REQ-TIME-003 | xmlda | `xmlda/requestoptions_test.go` | tested |
| REQ-SECURITY-001 | server | Design review: `server` never imports or hard-codes any auth mechanism; TLS/auth are the caller's responsibility (mounting `Handler` into their own stack) | implemented |
| REQ-SECURITY-002 | server | `server/write_test.go` — `TestHandleWrite_ReadOnlyConfig_AccessDenied`, `TestHandleWrite_NilWriter_AccessDenied` | tested |
| REQ-LIMITS-001 | subscription, server | `subscription/create_test.go` — `MaxConcurrentSubscriptions`; `server/read_test.go` — `TestHandleRead_ItemCountLimit`; `server/dispatch_test.go` — body size | tested |
| REQ-CONFIG-001 | subscription | `subscription/create_test.go`, `reaper_test.go` — default + explicit override both tested | tested |

## Coverage summary

- Total requirements: 68
- Must: 62, Should: 3, Optional: 1, Implementation-policy: 2
- Status distribution (as of WP-9 completion, corrected 2026-08-24 for REQ-TYPE-006/REQ-WRITE-004 — both were
  actually already covered by dedicated tests that predated this row's last update, see individual rows): 60
  tested, 4 implemented (documented as backend-authoring responsibilities or awaiting a dedicated test — see
  individual rows), 1 in progress, 0 not started, 0 blocked. `-race` re-verification for the
  concurrency-sensitive rows was completed 2026-08-24 (tracked in `docs/development/tasks.md`);
  REQ-SUBSCRIPTION-009 above is now unconditionally `tested`.
