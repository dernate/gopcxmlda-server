# Request/Response Data Flow

## Inbound request

```
net/http request
   │
   ▼
server.Handler.ServeHTTP
   │  1. reject non-POST
   │  2. http.MaxBytesReader(w, r.Body, Config.MaxRequestBodyBytes)
   │  3. read full body (bounded in size by step 2; bounded in time, when using
   │     server.NewServer, by Config.ReadHeaderTimeout/ReadTimeout on the
   │     underlying http.Server — a Handler mounted into a caller's own
   │     http.Server relies on that caller's own connection-level timeouts)
   ▼
xmlda.IdentifyOperation(body)   — dispatch is body-based, not Content-Type/SOAPAction-based (see note below)
   │
   ├─ decode error (not well-formed XML/SOAP) ─────────────────────► bucket 1: soap.Fault{Code: zero,
   │                                                                  Text: wrapped decode error} — matches
   │                                                                  testdata/faults/fault_soap11_xml_syntax_error
   │
   ├─ well-formed, operation not recognized ───────────────────────► bucket 2: soap.Fault{Code:
   │                                                                  xmlda.ErrNotSupported, Text: "..."}
   │                                                                  (REQ-SERVER-003, OQ-2)
   │
   └─ operation recognized ─────────────┐
                                         ▼
                          decode into typed request struct (e.g. xmlda.ReadRequest)
                                         │
                                         ├─ decode error (e.g. bad dateTime) ─► bucket 3: soap.Fault
                                         │   referencing the identified operation — matches
                                         │   testdata/faults/fault_soap12_invalid_datetime
                                         │
                                         ▼
                          context.WithTimeout(r.Context(), Config.RequestTimeout)
                          (Config.MaxPolledRefreshWait for SubscriptionPolledRefresh)
                                         │
                                         ▼
                          backend.Backend.Status.GetStatus(ctx, "") — this is itself a
                          backend call, so it is not quite accurate to say ServerState is
                          resolved "before any backend call"; it is resolved before any
                          operation-specific backend call
                                         │
                                         ▼
                          xmlda.RequiresFault(op, currentServerState)?
                                         │
                                         ├─ yes ──────────────────────────────► soap.Fault{Code: E_SERVERSTATE}
                                         │                                      (REQ-SERVER-002)
                                         ▼
                          validate item-list size against Config limits
                                         │
                                         ├─ exceeds limit ────────────────────► soap.Fault (implementation
                                         │                                      policy, REQ-LIMITS-001)
                                         ▼
                          route to backend / subscription.Manager
                          (see per-operation flow below)
                                         │
                                         ▼
                          construct xmlda response struct
                          (ResolveValuePresence, DedupeErrors)
                                         │
                                         ▼
                          soap.Envelope[T]{Body: {Content: response}}.MarshalXML
                                         │
                                         ▼
                          HTTP 200 (spec: SOAP faults also typically HTTP 200,
                          document/literal convention — see soap package GoDoc)
```

**Content-Type/SOAPAction are not validated or used for dispatch.** An earlier draft of this document
described step 1 as also rejecting a wrong/missing `Content-Type`. The implementation instead dispatches
purely on the parsed body via `xmlda.IdentifyOperation` — this is deliberate, not an oversight: real SOAP
1.1/1.2 clients are inconsistent about `Content-Type`/`SOAPAction` in practice (see
`docs/interoperability.md`), and body-based dispatch is robust to that inconsistency without weakening any
actual validation (a request that isn't well-formed OPC XML-DA still fails at `IdentifyOperation` or the
typed-decode step regardless of what `Content-Type` claimed).

## Per-operation routing


| Operation | Handler routes to |
|---|---|
| GetStatus | `backend.Backend.Status.GetStatus` directly |
| Read | `backend.Backend.Reader.Read` directly |
| Write | `backend.Backend.Writer.Write` directly (or synthesizes `E_ACCESS_DENIED` for every item if `Writer == nil` or `Config.ReadOnly`) |
| Browse | `backend.Backend.Browser.Browse` directly (continuation-point hash check happens in `server` before the backend call) |
| GetProperties | `backend.Backend.Properties.GetProperties` directly |
| Subscribe | `subscription.Manager.Create` (which itself calls `backend.Reader.Read` for validation/initial values, and `reader.(backend.ChangeNotifier)` detection) |
| SubscriptionPolledRefresh | `subscription.Manager.PolledRefresh` |
| SubscriptionCancel | `subscription.Manager.Cancel` |


## Backend result → wire response translation

For every per-item outcome (`backend.Result[T]`):

1. If `ResultID` is the zero `xmlda.ErrorCode`, no abnormal condition — proceed normally.
2. `ResolveValuePresence(quality, haveLastKnown)` decides whether the item's `Value` element is included at
   all (REQ-QUALITY-002/003/004) — this is evaluated regardless of whether `ResultID` is set, since a
   quality-driven omission and an error condition are independent.
3. `Timestamp` inclusion is gated by `RequestOptions.ReturnItemTime` *and* the same
   quality-driven presence rule.
4. Every non-zero `ResultID` across the whole item list feeds `DedupeErrors` once, producing the response's
   `Errors` list; the item's own `ResultID` attribute is set independently of that shared list.

This translation logic lives in `server` (not `xmlda`, to avoid `xmlda` depending on `backend`, and not
`backend`, since backends should not need to know wire-serialization rules) — see `architecture.md`'s
package dependency graph.

## Outbound fault vs. per-item error — decision boundary

A backend method's top-level `error` return and its per-item `Result[T].ResultID` are structurally
independent slots. There is no runtime branching where the server "decides" whether something is a fault or
a per-item error — the backend's return shape already picked the channel:

- Returning a non-nil top-level `error` from `Reader.Read` (say, a database connection failure) can only ever
  become a `soap.Fault` — there is no per-item field it could be attached to instead.
- Setting `Result[T].ResultID` on one item can only ever become that item's `ResultID` plus a shared `Errors`
  entry — it cannot accidentally fault the whole call.

This is why `ServerState`-driven whole-call faults (REQ-SERVER-002) are checked *before any
operation-specific backend call* (i.e. before `Reader.Read`/`Writer.Write`/etc. — resolving `ServerState`
itself still requires the one `Status.GetStatus` call every operation makes regardless): the backend is
never given the opportunity to "half-apply" a state-driven fault through the per-item channel.
