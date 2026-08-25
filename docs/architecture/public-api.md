# Public API

The exported surface an application actually needs to use. Everything else exported (e.g. `xmlda`'s
per-operation structs) exists because it appears in a type signature something below needs, not because
applications are expected to construct it directly.

## Constructing a server

```go
package server

type Deps struct {
    Backend backend.Backend
    Clock   clock.Clock       // nil -> clock.Real{}
    Logger  telemetry.Logger  // nil -> no-op
    Metrics telemetry.Metrics // nil -> no-op
}

type Config struct {
    MaxItemsPerRequest         int
    MaxItemsPerSubscription    int
    MaxConcurrentSubscriptions int
    MaxRequestBodyBytes        int64
    RequestTimeout             time.Duration
    MaxPolledRefreshWait       time.Duration
    MaxConcurrentPolls         int
    ReapInterval               time.Duration
    ReapGraceMultiplier        float64
    DefaultSubscriptionPingRate time.Duration // substituted when a client sends SubscriptionPingRate=0
    DefaultSamplingRate        time.Duration // substituted when a client requests RequestedSamplingRate=0
    MaxBufferedSamplesPerItem  int           // bounds per-item buffered changes
    PollTimeout                time.Duration // bounds each poll-mode backend.Reader.Read call
    ReadOnly                   bool
}

func New(deps Deps, cfg Config) (*Handler, error)             // returns an http.Handler
func NewServer(addr string, deps Deps, cfg Config) (*Server, error)

type Handler struct{ /* implements http.Handler */ }

type Server struct{ /* ... */ }
func (s *Server) Start() error
func (s *Server) Shutdown(ctx context.Context) error
```

`Handler` is a plain `http.Handler` — mountable into any router, behind any TLS/auth middleware the
application chooses. `Server` is a convenience for applications that don't want to assemble their own
`net/http.Server`.

## Implementing a backend

```go
package backend

type Backend struct {
    Status     StatusProvider // required
    Reader     Reader         // required; may additionally implement ChangeNotifier
    Writer     Writer         // optional — nil means read-only
    Browser    Browser        // optional
    Properties PropertyReader // optional
}

type StatusProvider interface {
    GetStatus(ctx context.Context, locale string) (ServerStatus, error)
}

type Reader interface {
    Read(ctx context.Context, items []ReadRequestItem) ([]Result[ItemSample], error)
}

type Writer interface {
    Write(ctx context.Context, items []WriteRequestItem) ([]Result[WriteOutcome], error)
}

type Browser interface {
    Browse(ctx context.Context, req BrowseRequest) (BrowseResult, error)
}

type PropertyReader interface {
    GetProperties(ctx context.Context, reqs []PropertyRequest) ([]Result[[]Property], error)
}

// Optional: type-asserted off Reader (reader.(ChangeNotifier)), not a separate Backend field.
type ChangeNotifier interface {
    WatchItems(ctx context.Context, items []WatchRequest) (<-chan ChangeEvent, error)
}
```

Full field-level detail for `ItemRef`, `ItemSample`, `Result[T]`, `ReadRequestItem`, `WriteRequestItem`,
`WriteOutcome`, `BrowseRequest`/`BrowseResult`/`BrowseElement`, `PropertyRequest`/`Property`,
`WatchRequest`/`ChangeEvent`, and `BackendError` lives in the `backend` package's GoDoc — this document
tracks the interface shapes an application implements against, not every field.

## Wire vocabulary an application will see

A backend implementation constructs and reads these `xmlda` types (never SOAP envelope or dispatch types):

```go
package xmlda

type Value struct{ /* opaque; NewInt32/NewString/... constructors, Int32()/String()/... accessors */ }
type OPCQuality struct{ /* opaque; NewGoodQuality/NewQuality constructors, QualityField()/... accessors */ }
type ErrorCode struct{ QName }
```

Standard `ErrorCode` values (`xmlda.ErrAccessDenied`, `xmlda.ErrUnknownItemName`, `xmlda.SuccessClamp`, …)
are exported package-level vars — see `docs/specification/error-mapping.md` for the full list.

## Testing support

```go
package clock

type Clock interface {
    Now() time.Time
    After(d time.Duration) <-chan time.Time
    NewTimer(d time.Duration) Timer
    AfterFunc(d time.Duration, f func()) Timer
    Sleep(d time.Duration)
}
```

```go
package clocktest // clock/clocktest

type Fake struct{ /* ... */ }
func New(start time.Time) *Fake
func (f *Fake) Advance(d time.Duration)
func (f *Fake) Set(t time.Time)
```

An application can construct its own `server.Deps{Clock: fakeClock}` to get the same deterministic,
no-real-sleep testing this library uses internally.

## What is deliberately not public

- Anything in `soap` beyond what's needed to read a `Fault`'s `Code`/`Text` for logging purposes — envelope
  marshaling internals are not meant to be used directly by applications.
- `xmlda`'s dispatch machinery (`IdentifyOperation`, the operation registry) — internal to `server.Handler`.
- `subscription.Manager` internals beyond what `server` needs — applications interact with subscriptions
  only through the OPC XML-DA protocol itself (Subscribe/SubscriptionPolledRefresh/SubscriptionCancel), not
  through a Go API, since subscriptions are a protocol-level concept with no meaning outside a client
  session.
