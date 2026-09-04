# Open Questions and Conservative Assumptions

Points where the specification is ambiguous, silent, or internally inconsistent. Each entry states the
conservative assumption made, why, and how it is encapsulated so it can be revisited without a wide-reaching
change. None of these block implementation.

## OQ-1: `E_BADTYPE` and `E_INVALIDITEMID` are not in the master error table

**Ambiguity**: §3.1.9 (pp. 37-38) lists the "standard" error codes, but `E_BADTYPE` (used by Read/Write/
Subscribe abnormal-code tables) and `E_INVALIDITEMID` (used by Write) never appear there.

**Assumption**: treat both as first-class standard codes in the XML-DA namespace, consistent with their
usage context. **Encapsulation**: both are defined as ordinary `xmlda.ErrorCode` package-level vars
alongside the rest of the standard vocabulary in `xmlda/errors.go` — no special-casing needed if the OPC
Foundation later clarifies their status.

## OQ-2: No error code defined for "unknown/unsupported operation"

**Ambiguity**: the spec defines all 8 operations but gives no guidance for what a server should do when a
client's SOAP Body contains something else.

**Assumption**: emit a SOAP fault with `xmlda.ErrNotSupported` ("the data type or operation is not
supported" is the closest semantic match among standard codes). **Encapsulation**: this mapping lives in one
place, `xmlda.IdentifyOperation`'s failure-bucket handling — see `docs/architecture/data-flow.md`.

## OQ-3: No spec-mandated resource limits

**Ambiguity**: max items per request/subscription, max concurrent subscriptions, max message size, and rate
limiting are never mentioned. Only soft guidance exists ("server specific maximum time will generally be no
more than a minute or two," §3.1.6, p.35).

**Assumption**: conservative, documented, configurable defaults (see `server.Config` and ADR-011).
**Encapsulation**: every limit is a `Config` field with an explicit "implementation default, not
spec-mandated" comment; none are hard-coded in protocol logic.

## OQ-4: Deadband + buffer overflow interaction

**Ambiguity**: the spec itself (§2.5.4, p.19) flags that deadband-filtered-out samples which were
nonetheless buffered can still be purged under buffer pressure, potentially leaving only the final
in-deadband value — described as a caveat, not resolved.

**Assumption**: implement with best-effort fidelity (oldest-purged-first, Latest Changed Value per item
always retained) and document the residual imprecision in `docs/limitations.md` rather than attempting
perfect device-polling fidelity.

## OQ-5: `RequestList`/`RequestItem.ClientItemHandle` — required or optional?

**Ambiguity**: the field table on p.30 (§3.1.4) marks `ClientItemHandle` as a "required attribute," but every
per-operation usage in §3 treats it as optional (freely omittable), and the real fixture
`testdata/requests/subscribe_679.request.xml` supplies it consistently but nothing in the spec's text
demands it must always be present.

**Assumption**: treat as optional on the wire (matches actual operational text and real traffic); a missing
`ClientItemHandle` is never itself an error condition. **Encapsulation**: `ItemName`/`ClientItemHandle`
fields use `omitempty`/pointer semantics consistent with every other genuinely-optional field, not a required
validation rule.

## OQ-6: Namespace prefix scoping — flat, whole-document table vs. true nested scopes

**Ambiguity**: `encoding/xml` does not resolve prefixes appearing inside attribute *values* (`xsi:type`,
`ResultID`, etc.); implementing a fully correct nested-scope resolver (mirroring XML namespace scoping rules
exactly, including a prefix rebound to different URIs at different depths) is more complex than anything
observed in real OPC XML-DA traffic.

**Assumption**: build one flat, whole-document prefix→URI table per decode ("last declaration wins" if a
prefix is redeclared at multiple depths — not observed in any real fixture). **Encapsulation**: isolated
entirely inside `xmlda.resolveQName`/`buildPrefixTable`; if a future document needs true nested scoping, only
this one function pair changes, with no ripple into any struct definition.

## OQ-7: SOAP fault emission style

**Ambiguity**: real-world captured traffic shows four distinct fault shapes (spec-conformant SOAP 1.1
QName-qualified, legacy SOAP 1.1 unqualified-text, generic SOAP 1.1 parse-error, SOAP 1.2 structured) — the
spec itself only shows the first.

**Decision** (see ADR-004): parse all four tolerantly; always emit the spec-conformant SOAP 1.1,
QName-qualified shape. Not treated as "open" for output — only input tolerance is variable.

## OQ-9: `SubscriptionCancel` on an unknown or already-cancelled handle

**Ambiguity**: `SubscriptionCancelResponse` (§3.7, p.68) has no `ReplyBase` and no `ResultID`/error slot at
all — just an echoed `ClientRequestHandle`. The spec's fault list for this operation (`E_FAIL`,
`E_OUTOFMEMORY`, `E_SERVERSTATE`) has no code analogous to `E_NOSUBSCRIPTION` (which *is* defined for
`SubscriptionPolledRefresh` when handles are invalid). Section 11 of the project directive explicitly
requires testing "doppelte Cancellation" (double cancellation), implying this must be a defined, safe
behavior, not a crash.

**Decision**: treat cancelling an unknown or already-cancelled `ServerSubHandle` as an idempotent no-op
success — the response still just echoes `ClientRequestHandle`. This is consistent with the response shape
having no error slot to put a failure in, and makes double-cancellation (including a benign race between a
client's cancel and the reaper independently reaping the same handle) safe by construction rather than by
special-cased detection. **Encapsulation**: `subscription.Manager.Cancel` looks up the handle and simply
returns success whether or not it was found — see REQ-SUBSCRIPTION-014, tested explicitly in
`subscription/manager_test.go`.

## OQ-10: `SubscriptionPingRate=0` must not mean "reap immediately"

**Ambiguity**: the spec states `SubscriptionPingRate=0` means "the server's own algorithm decides" (§3.5.1,
p.57), i.e. it is a sentinel for "use the server default," not a literal zero-duration ping interval. The
reaper's grace-period check (`docs/architecture/subscription-model.md`) is
`now - lastPolledAt > pingRate * ReapGraceMultiplier` — if `pingRate` were used literally as `0`, this
would evaluate true almost immediately after subscription creation, incorrectly reaping a brand-new,
never-yet-polled subscription. **This was caught during the Phase 4 self-review** (see
`docs/development/plan-review.md`) before any code was written.

**Decision**: `subscription.Manager` substitutes `server.Config.DefaultSubscriptionPingRate`
(implementation default, e.g. 60s — not spec-mandated) whenever a Subscribe request's
`SubscriptionPingRate` is `0`, and uses that resolved value for both the reaper grace calculation and the
`RevisedSamplingRate`-style echo-back semantics. **Encapsulation**: the substitution happens once, at
subscription creation, inside `subscription.Manager.Create` — `pingRate` stored in `subState` is always the
already-resolved, nonzero value; no other code needs to special-case zero.

## OQ-12: `time`/`date`/`duration` — direct `xsi:type` vs. `dateTime`+`ValueTypeQualifier`

**Ambiguity**: §2.7.1 (pp. 23-24) notes `time`/`date` are "not fully supported by .NET tools; transmitted as
`dateTime` with a `ValueTypeQualifier`," which reads as an interop workaround for one toolchain's
limitations, not a hard protocol requirement — the type-mapping table still lists `time`/`date`/`duration`
as legitimate XSD types in their own right, each with a direct `xsi:type` name.

**Decision**: `xmlda.Value` decodes and encodes `xsi:type="xsd:time"`, `"xsd:date"`, `"xsd:duration"`
directly and symmetrically — self-contained, with no coordination needed with the containing `ItemValue`
element. This is simpler than the `dateTime`+`ValueTypeQualifier` indirection and remains spec-legal (the
mapping table lists these as valid types; the qualifier is presented as an interop accommodation, not a
requirement).

**Consequence — implemented, not outstanding.** For tolerance of peers that *do* send the
`dateTime`+`ValueTypeQualifier` form (as the spec's own text suggests some implementations will),
`applyValueTypeQualifier` (`xmlda/itemvalue.go`) recognizes a `ValueTypeQualifier` whose local name is
`time`/`date`/`duration` alongside a `dateTime`/`string`-typed `Value` and reinterprets accordingly.
`TestItemValue_ValueTypeQualifierTolerance` pins it. This is decode-side only: `Value`'s own encoding
always uses the direct, symmetric form.

A **second**, separate tolerance was added later, for a different problem — see
`docs/interoperability.md`. `valueTypeFromQualifier` lets the qualifier supply the type when the `<Value>`
declares none at all, which is what makes NothinRandom/pyopcxmlda's writes decode (it spells the attribute
`xsi:Type`, a different and meaningless attribute in a case-sensitive language). The two do not overlap:
this one only fires when there is no `xsi:type` to reinterpret, and the reinterpretation above still runs
afterwards.

## OQ-13: SOAP Fault QName resolution scope is element-local, not whole-document

**Ambiguity/limitation**: `xmlda.resolveQName` does a whole-document prefix-table pre-scan (OQ-6), but
`soap.Fault` decoding happens mid-stream, inside `Body[T].UnmarshalXML`, without access to the full document
bytes — only the live `*xml.Decoder`. Building a "soap" package that depends on `xmlda`'s whole-document scan
would violate the layering (`soap` must not import `xmlda`, and duplicating a second whole-document scan
mechanism inside `soap` was judged not worth it for this one narrow case).

**Decision**: `soap.Fault`'s own resolver (`resolveLenient`) only looks at the *current* element's own
`xmlns:*` attributes — which matches the specification's own worked example (§2.6, p.21), where `xmlns:q0`
is declared directly on `<faultcode>` itself, not on an ancestor. A prefix declared only on a remote
ancestor (e.g. the Envelope root, as real captured SOAP 1.2 traffic does for its `soap:` prefix) will not
resolve; the fallback (`QName{Local: rawText}`, `Space` left empty) still preserves the literal text for
logging/matching. **Consequence**: none of the three real captured fault fixtures under `testdata/faults/`
actually carry a resolvable OPC-namespace-qualified fault code, so this limitation does not affect any
current test; it is accepted as a documented scope boundary rather than solved with a second full-document
scanner duplicated inside `soap`.

## OQ-11: Module import path

**Resolved**: `github.com/dernate/gopcxmlda-server`, matching the repository's git remote
(`https://github.com/dernate/gopcxmlda-server.git`) and the existing `github.com/dernate/gopcxmlda` reference
client's naming convention.
