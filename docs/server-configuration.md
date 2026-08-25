# Server Configuration

## `server.Deps`

```go
type Deps struct {
	Backend backend.Backend
	Clock   clock.Clock       // nil ⇒ clock.Real{}
	Logger  telemetry.Logger  // nil ⇒ telemetry.NoopLogger()
	Metrics telemetry.Metrics // nil ⇒ telemetry.NoopMetrics()
}
```

- **`Backend`** is required; `server.New`/`server.NewServer` call `Backend.Validate()` at construction time,
  so a missing `Status` or `Reader` fails fast rather than misbehaving at request time.
- **`Clock`** only needs overriding in tests (`clock/clocktest.Fake`) — production code should leave it nil.
- **`Logger`** is any type matching `slog`'s leveled methods (`Debug`/`Info`/`Warn`/`Error`, each
  `(msg string, args ...any)`) — a `*slog.Logger` satisfies it directly, with zero adapter code:

  ```go
  Deps{Logger: slog.New(slog.NewTextHandler(os.Stdout, nil))}
  ```

  **No log line this library emits by default includes a full SOAP body or an item value** — process data
  is treated as sensitive by convention across every package. Default log lines carry operation name,
  handle IDs, item counts, and durations only.
- **`Metrics`** is a small hook interface (`IncRequest`, `IncRequestError`, `ObserveBackendLatency`,
  `SetActiveSubscriptions`, `IncSubscriptionError`, `IncParseError`) — implement it against whatever
  monitoring stack you already use; no specific library is required or assumed.

## `server.Config`

Every field below is an **implementation default, not a specification requirement** — the OPC XML-DA 1.0
specification defines no numeric limits at all (see
[`docs/architecture/decisions/011-configuration-limits.md`](architecture/decisions/011-configuration-limits.md)).
All fields are optional; zero values fall back to the defaults listed here.

| Field | Default | Meaning |
|---|---|---|
| `MaxItemsPerRequest` | 1000 | Max items in a single Read/Write/Subscribe/GetProperties request. |
| `MaxItemsPerSubscription` | 1000 | Max items one subscription may hold. |
| `MaxConcurrentSubscriptions` | 10000 | Max subscriptions across the whole server; 0 = unlimited. |
| `MaxRequestBodyBytes` | 4 MiB | HTTP body size limit, enforced via `http.MaxBytesReader` before any XML parsing. |
| `RequestTimeout` | 30s | Bounds every non-`SubscriptionPolledRefresh` operation. |
| `MaxPolledRefreshWait` | 90s | Caps the client-requested Hold+Wait duration for `SubscriptionPolledRefresh`. |
| `MaxConcurrentPolls` | 32 | Bounds concurrent poll-mode backend calls across all subscriptions. |
| `ReapInterval` | 10s | How often the abandonment reaper sweeps for abandoned subscriptions. |
| `ReapGraceMultiplier` | 2.0 | Abandonment grace period = `SubscriptionPingRate × this`. |
| `DefaultSubscriptionPingRate` | 60s | Substituted when a client sends `SubscriptionPingRate=0`. |
| `DefaultSamplingRate` | 1s | Substituted when a client requests `RequestedSamplingRate=0`. |
| `MaxBufferedSamplesPerItem` | 100 | Per-item buffered-change limit before the oldest are purged. |
| `PollTimeout` | 30s | Bounds each individual poll-mode `backend.Reader.Read` call. |
| `ReadOnly` | `false` | If `true`, every `Write` item resolves to `E_ACCESS_DENIED` regardless of whether the backend has a `Writer` — the specification's own recommended policy hook (§2.8). |

Set only the fields you need to change:

```go
server.Config{
	MaxItemsPerRequest: 200,
	ReadOnly:           true,
}
```

## TLS and authentication

`server.Handler` and `server.Server` implement/wrap a plain `http.Handler` — this library has **no
knowledge of TLS or any specific authentication mechanism**, by design (see
[`docs/architecture/decisions/010-net-http.md`](architecture/decisions/010-net-http.md)). To add either,
don't modify this library — wrap or front it the way you would any other `http.Handler`:

```go
h, err := server.New(deps, cfg) // an http.Handler
mux := http.NewServeMux()
mux.Handle("/opcxmlda", authMiddleware(h))

httpSrv := &http.Server{
	Addr:      ":8443",
	Handler:   mux,
	TLSConfig: myTLSConfig,
}
log.Fatal(httpSrv.ListenAndServeTLS(certFile, keyFile))
```

If you go this route instead of `server.NewServer`, call `Handler.BeginShutdown()`/`Handler.Shutdown(ctx)`
yourself in the same order `server.Server.Shutdown` uses — cancel subscriptions *before* stopping the HTTP
server — so an in-flight long-poll `SubscriptionPolledRefresh` call doesn't block your shutdown for the
client's requested Hold+Wait duration. See
[`docs/architecture/subscription-model.md`](architecture/subscription-model.md).

A reverse proxy in front of either form works the same way — this library never assumes it owns the
outermost network boundary.
