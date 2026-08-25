# Backend Implementation

Your backend is a plain Go value — no interface it implements ever needs to know about HTTP, SOAP, or XML
encoding. Package `backend` defines the contract; this document describes what each part must guarantee
beyond what the Go doc comments already say. Read `backend/backend.go`'s doc comments alongside this — they
are kept in sync.

## The `backend.Backend` struct

```go
type Backend struct {
	Status     StatusProvider // required
	Reader     Reader         // required; MAY additionally implement ChangeNotifier
	Writer     Writer         // optional; nil ⇒ every Write item → E_ACCESS_DENIED
	Browser    Browser        // optional; nil ⇒ Browse faults E_NOTSUPPORTED
	Properties PropertyReader // optional; nil ⇒ GetProperties faults E_NOTSUPPORTED
}
```

`Backend.Validate()` (called by `server.New`) only checks `Status` and `Reader` are non-nil. A nil
`Writer`/`Browser`/`Properties` is a normal, well-defined feature-detection signal, not a misconfiguration —
you don't need a stub implementation just to leave a capability unsupported.

## `StatusProvider` — required

```go
GetStatus(ctx context.Context, locale string) (ServerStatus, error)
```

`ServerStatus.State` is consulted by the server layer **on every request**, not just `GetStatus` — before
any other backend call is made, the server checks `xmlda.RequiresFault(operation, state)` and faults with
`E_SERVERSTATE` if the current state forbids that operation. `StartTime` must stay constant across the
server process's lifetime (a client may poll it to detect a restart). `SupportedLocaleIDs` must list at
least one entry.

## `Reader` — required

```go
Read(ctx context.Context, items []ReadRequestItem) ([]Result[ItemSample], error)
```

- The returned slice must have **exactly one `Result` per requested item, in the same order** — the server
  does not reorder or re-pair them.
- A non-nil `error` return is interpreted as a **whole-operation** failure (a SOAP Fault) — use it only for
  infrastructure problems (backend unreachable, context deadline exceeded), never for a single bad item.
- Per-item conditions (unknown item, access denied, out of range, ...) go in that item's own
  `Result.ResultID` — see [Error mapping](#error-mapping-mechanism) below.
- `Reader` is also called by the subscription engine: once at `Subscribe` time (to validate items and seed
  initial values) and, for poll-mode subscriptions, on every sampling tick.
- `Value.IsNil()` (not the Go zero value) is how you signal "Bad quality, no last-known value at all" as
  opposed to a stale-but-present value — set `Value` to `xmlda.NewNil(declaredType)` for the former.

### `ChangeNotifier` — optional enhancement of `Reader`

```go
WatchItems(ctx context.Context, items []WatchRequest) (<-chan ChangeEvent, error)
```

Detected via a type assertion (`reader.(backend.ChangeNotifier)`), the same idiom as `http.Flusher` off
`http.ResponseWriter` — not a separate `Backend` field. If your `Reader` implements this, the subscription
engine pushes changes through it instead of polling `Read` on a schedule. Implement it only if your data
source can genuinely notify on change; a `Reader` without it is polled at
`ItemParams.RequestedSamplingRate` (or `Config.DefaultSamplingRate` if the client requested "fastest
practical"). See `examples/basic-server/memorybackend/backend.go` for a complete implementation (one drain
goroutine per subscription, and one cleanup goroutine per `WatchItems` call — both bounded by `ctx` with no
other exit path, which is the pattern to follow for your own implementation: **never start a goroutine here
without a documented, ctx-bound way for it to stop**).

## `Writer` — optional

```go
Write(ctx context.Context, items []WriteRequestItem) ([]Result[WriteOutcome], error)
```

If `WriteRequestItem.Quality` and/or `.Timestamp` are non-nil, you **must apply Value+Quality+Timestamp
atomically**: accept all three, or reject the whole item with `xmlda.ErrNotSupported`. Partially applying
them (e.g. writing the value but silently dropping the supplied quality) is a specification violation, not
a graceful degradation.

`WriteOutcome.Clamped` reports whether you clamped an out-of-range value to the item's valid range — a
clamped write still counts as success, surfaced to the client as `S_CLAMP`, not an error.

A nil `Writer`, or `server.Config.ReadOnly = true`, makes every `Write` item resolve to `E_ACCESS_DENIED`
**without your backend being called at all** — the global read-only switch the specification itself
recommends (§2.8).

## `Browser` — optional

```go
Browse(ctx context.Context, req BrowseRequest) (BrowseResult, error)
```

`BrowseRequest.ContinuationPoint`/`BrowseResult.ContinuationPoint` are **your own private, opaque cursor** —
never re-implement continuation-point *filter-consistency* validation yourself. The server wraps your cursor
with a SHA-256 hash of the request's filter fields (`ItemName`/`ItemPath`/`Filter`/`ElementNameFilter`/
`VendorFilter`/`MaxElementsReturned`/`ReturnAllProperties`/`ReturnPropertyValues`) before it ever reaches the
wire, and rejects a continued call whose filters changed with `E_INVALIDCONTINUATIONPOINT` before your
`Browse` is even called (see `server/continuation.go`). You only ever see your own cursor half, exactly as
you issued it — **but that cursor half itself is not authenticated.** The filter hash is unkeyed (there is
no server secret in it) and only checks that the *filters* match the current request; it says nothing about
whether the cursor text was ever actually issued by your `Browse` implementation. A client can construct its
own filter hash (trivial, since it controls every field that goes into it) and pair it with an arbitrary
cursor string of its choosing, and the server will pass that cursor straight through to your `Browse` call
as `ContinuationPoint`, looking exactly like a token you issued yourself. If your own cursor format could
misbehave on unexpected input (e.g. it's an index/offset you'd blindly trust, or a value you'd use to index
into a slice), validate it defensively on your side — do not assume "the server validated this continuation
point" covers anything beyond filter consistency.

A `BrowseElement` with `Ref == nil` is a non-actionable "hint" node (`IsItem` true but no addressable
identity) — set it only if your address space genuinely has such nodes; most backends never need to.

## `PropertyReader` — optional

```go
GetProperties(ctx context.Context, reqs []PropertyRequest) ([]Result[[]Property], error)
```

The outer `Result.ResultID` is the per-**item** condition (e.g. `E_UNKNOWNITEMNAME` ⇒ no properties at
all); each `Property.ResultID` is a per-**property** condition (e.g. `E_INVALIDPID` for one unrecognized
property among several valid ones on an otherwise-known item). Use `xmlda.StandardPropertyName(id)` /
the `xmlda.PropX` constants for the standard property IDs (1–8, 100–108); see
[`docs/specification/type-mapping.md`](specification/type-mapping.md).

## Error-mapping mechanism

The two OPC XML-DA error channels are picked automatically by the *shape* of what your backend method
returns — you never choose between them with conditional logic:

- A **plain Go `error`** return from any backend method is always a whole-operation SOAP Fault (there is no
  per-item slot for it). The server applies a deterministic default: `context.DeadlineExceeded` →
  `E_TIMEDOUT`, anything else → `E_FAIL`.
- A **`backend.Result[T].ResultID`** is always a per-item condition (there is no whole-operation slot it
  could occupy) — set it directly to the `xmlda.ErrorCode` that matches the situation (`xmlda.ErrUnknownItemName`,
  `xmlda.ErrReadOnly`, `xmlda.ErrRange`, ...).
- If a whole-operation failure needs to be more specific than the `E_TIMEDOUT`/`E_FAIL` default (e.g.
  "busy" or "access denied" at the whole-operation level), return `&backend.BackendError{Fault: backend.FaultBusy, Err: err}`
  instead of a bare `error` — the server checks for this via `errors.As` before falling back to the default.

See [`docs/specification/error-mapping.md`](specification/error-mapping.md) for the full standard-code table
and [`docs/architecture/decisions/005-backend-error-mapping.md`](architecture/decisions/005-backend-error-mapping.md)
for the rationale. Never return an error whose message you'd be uncomfortable showing a client verbatim —
whole-operation fault text is always one of the fixed, spec-defined descriptions or a generic message; put
anything more specific into your own logging instead.

## A complete reference implementation

`examples/basic-server/memorybackend/backend.go` implements every interface above (including
`ChangeNotifier`) against a handful of in-memory items, and `examples/basic-server/e2e_test.go` exercises
all of them through a real HTTP round trip. Reading both together is the fastest way to see this contract
satisfied end to end.
