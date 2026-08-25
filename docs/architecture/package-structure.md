# Package Structure

```
gopcxmlda-server/
  soap/                    SOAP 1.1/1.2 envelope + fault. No OPC vocabulary.
  xmlda/                   OPC XML-DA 1.0 wire vocabulary + the 8 operations + dispatch.
  backend/                 Small composable backend interfaces + domain types.
  clock/                   Clock/Timer abstraction + real implementation.
  clock/clocktest/         Fake clock for deterministic tests.
  telemetry/               Logger/Metrics interfaces + no-op defaults.
  subscription/            Subscription Manager (storage, scheduling, Hold+Wait, reaper).
  server/                  http.Handler + Server wrapper, Config, Deps, orchestration.
  examples/basic-server/   In-memory reference backend + runnable example.
  testdata/                Real and synthetic SOAP fixtures (requests/, responses/, faults/, invalid/, golden/).
```

Dependency graph (no cycles):

```
clock  telemetry            (leaves)
  ↑        ↑
backend    |
  ↑        |
subscription
  ↑
server ──→ xmlda ──→ soap
```

## Why this split and no finer

**`soap` vs. `xmlda` as two packages, not one.** SOAP envelope/fault handling (SOAP 1.1 vs. 1.2 shapes, plus
tolerant parsing of the two non-conformant legacy fault shapes actually observed in `testdata/faults/`) is
substantial, self-contained complexity with nothing to do with OPC semantics — it deserves its own tests
independent of any OPC-XML-DA round-trip test, and is genuinely reusable if this codebase or another ever
needs another document/literal SOAP protocol. `soap` never imports `xmlda`; `xmlda` instantiates
`soap.Envelope[T]` generically for each of its 8 operations, avoiding boilerplate duplication without
coupling the dependency the wrong way.

**`Value`/`OPCQuality`/`ErrorCode` stay inside `xmlda`, not a separate "vocabulary" package.** These types
have no independent existence outside an OPC XML-DA message — a `Value` outside that context means nothing.
Splitting them out would buy no real isolation (Go does not penalize a package for importing symbols it
doesn't use, so `backend` importing all of `xmlda` for these three types costs nothing at compile time or
at the API-stability level that actually matters: `Value`/`OPCQuality`/`ErrorCode` are stable regardless of
future churn in the operation-request/response structs) and would only add `xmlda.opcvalue.Value{...}`
stutter and import-graph noise. This was one explicit point of disagreement between the two original design
passes — the backend-focused pass initially wanted a hard split purely for import-direction hygiene — and
the resolution favors the mega-prompt's own instruction to avoid artificially fine-grained package structure
over a split that defends against a risk (accidental instability propagation) Go's own compiler behavior
doesn't actually create.

**`backend` has no HTTP/XML/SOAP awareness.** It depends only on `xmlda`'s vocabulary types and the standard
library. This is the one split that *is* worth enforcing: a third-party server implementation's backend code
should never need to know about `net/http`, `encoding/xml`, or SOAP envelopes — only about items, values,
quality, and OPC result codes.

**`subscription` has no HTTP/SOAP awareness either.** It is independently unit-testable and reusable if this
engine were ever fronted by a non-HTTP transport (not a near-term goal, but a natural consequence of the
split, not something built for its own sake).

**`clock`/`clock/clocktest` are split from each other** so production code can never accidentally import
test-only helpers — a real risk if the fake clock lived in the same package as `clock.Real`.

**`telemetry` is its own leaf package**, not folded into `server`, specifically to avoid an import cycle:
both `server` and `subscription` need to log/emit metrics, so the interfaces must live somewhere both can
depend on without either depending on the other.

**No umbrella package.** `server.New`/`server.NewServer` already are the library's single entry point; a
further wrapping package would be indirection without benefit.

## Package-by-package file layout (initial; grows as needed, not upfront)

```
soap/
  envelope.go       Envelope[T], Header, Body[T], package-local QName
  fault.go          Fault, tolerant UnmarshalXML for all 4 observed shapes, consistent MarshalXML
                    (no separate action.go: SOAPAction is trivial string concatenation, built where
                    server/dispatch needs it in WP-5/WP-9 rather than given its own file)

xmlda/
  namespace.go      QName, resolveQName, buildPrefixTable, decoderScopes
  value.go          Value, ScalarType, Kind, Array, RawValue, Decimal, constructors, accessors
  quality.go        OPCQuality, QualityField, LimitField, ResolveValuePresence
  errors.go         ErrorCode, OPCError, Errors, standard code vars, DedupeErrors
  replybase.go      ReplyBase, ServerState
  requestoptions.go RequestOptions
  itemparams.go     ItemParams, MergeItemParams
  itemproperty.go   ItemProperty, PropertyID constants, StandardPropertyName
  getstatus.go, read.go, write.go, subscribe.go, subscriptionpolledrefresh.go,
  subscriptioncancel.go, browse.go, getproperties.go
  dispatch.go       Operation registry, IdentifyOperation, RequiresFault

backend/
  backend.go        StatusProvider, Reader, Writer, Browser, PropertyReader, ChangeNotifier,
                     Backend, domain types (ItemRef, ItemSample, Result[T], BackendError)

clock/
  clock.go          Clock, Timer, Real
clock/clocktest/
  clocktest.go      Fake

telemetry/
  telemetry.go      Logger, Metrics, no-op defaults

subscription/
  manager.go        Manager, subState, storage, shutdown sequencing
  poll.go           poll-mode scheduling
  push.go           push-mode (ChangeNotifier) draining
  refresh.go        SubscriptionPolledRefresh Hold+Wait blocking
  reaper.go         abandonment sweep

server/
  server.go         Server (net/http wrapper), Start/Shutdown
  handler.go        Handler (http.Handler), dispatch/orchestration
  config.go         Config, defaults
  deps.go           Deps

examples/basic-server/
  main.go
  memorybackend/     in-memory Backend implementation, including ChangeNotifier (push mode)
```
