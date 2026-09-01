# gopcxmlda-server

A Go base library for implementing **OPC XML-DA 1.0 servers**. It is not an application — it is a set of
small, composable packages you build a server on top of: SOAP/XML wire handling, OPC XML-DA vocabulary and
the 8 protocol operations, a small backend contract you implement against your own data source, a
subscription engine, and an `http.Handler`/`http.Server` composition layer.

The library has **no dependency on any concrete SCADA system, database, or vendor structure** — you plug in
your own process data by implementing a handful of small interfaces (see
[`docs/backend-implementation.md`](docs/backend-implementation.md)).

## Status

This library implements the 8 OPC XML-DA 1.0 operations (`GetStatus`, `Read`, `Write`, `Subscribe`,
`SubscriptionPolledRefresh`, `SubscriptionCancel`, `Browse`, `GetProperties`) against the specification in
`docs/OPCDataAccessXMLSpecification.pdf`, backed by 68 tracked requirements and 450+ tests (unit, golden-file,
round-trip, HTTP-handler, subscription-lifecycle, and real-concurrency stress tests), all clean under
`go test -race ./...`, plus two separate integration modules that drive the server over real HTTP — one
in-process, one against a real Docker container. It has **not** been run against an official OPC Foundation conformance test suite,
and does not claim full conformance — see [`docs/protocol-support.md`](docs/protocol-support.md) for the
precise, per-operation status and [`docs/limitations.md`](docs/limitations.md) for remaining known gaps.

## Packages

| Package | Role |
|---|---|
| `soap` | SOAP 1.1/1.2 envelope and fault handling. No OPC vocabulary. |
| `xmlda` | OPC XML-DA 1.0 wire vocabulary, the 8 operations, and request dispatch. |
| `backend` | The small interfaces you implement against your own data source. |
| `clock` / `clock/clocktest` | A `Clock` abstraction (`clock.Real` for production, `clocktest.Fake` for deterministic tests). |
| `telemetry` | Logger/Metrics interfaces (no-op by default; `*slog.Logger` satisfies `telemetry.Logger` directly). |
| `subscription` | The subscription engine: creation, poll/push scheduling, Hold+Wait, buffering, abandonment cleanup. |
| `server` | `http.Handler` + optional `http.Server` wrapper, `Config`, `Deps`, request orchestration. |
| `examples/basic-server` | A runnable example server over an in-memory backend. |

See [`docs/architecture/package-structure.md`](docs/architecture/package-structure.md) for the full
dependency graph and rationale.

## Quick start

Implement `backend.Backend`'s required interfaces against your data source (only `Status` and `Reader` are
mandatory — `Writer`/`Browser`/`Properties` are optional, and a `Reader` that also implements
`backend.ChangeNotifier` gets push-mode subscriptions instead of polling):

```go
be := backend.Backend{
    Status: myStatusProvider,
    Reader: myReader,
    Writer: myWriter, // optional; nil ⇒ every Write is rejected with E_ACCESS_DENIED
}

srv, err := server.NewServer(":8080", server.Deps{Backend: be}, server.Config{})
if err != nil {
    log.Fatal(err)
}
log.Fatal(srv.Start())
```

`server.Server.Shutdown(ctx)` performs the ordering required for a clean stop: it cancels every subscription
first (so an in-flight long-poll `SubscriptionPolledRefresh` call unblocks immediately, rather than the
shutdown hanging for the client's requested Hold+Wait duration), then stops the HTTP server, then waits for
background goroutines to exit.

See [`examples/basic-server/`](examples/basic-server/) for a complete, runnable server (in-memory backend,
graceful shutdown on `SIGINT`/`SIGTERM`) and [`docs/getting-started.md`](docs/getting-started.md) for a
walkthrough.

## Documentation

- [`docs/getting-started.md`](docs/getting-started.md) — build and run a minimal server, step by step.
- [`docs/backend-implementation.md`](docs/backend-implementation.md) — the backend contract in detail:
  what each interface must guarantee, the error-mapping mechanism, continuation-point handling, atomic
  writes.
- [`docs/server-configuration.md`](docs/server-configuration.md) — every `server.Config` field, `Deps`,
  logging/metrics hooks, TLS/auth integration.
- [`docs/protocol-support.md`](docs/protocol-support.md) — per-operation implementation/test status.
- [`docs/limitations.md`](docs/limitations.md) — known gaps and their tracking status.
- [`docs/interoperability.md`](docs/interoperability.md) — namespace-prefix independence, tolerant SOAP
  fault parsing, and other real-world interop accommodations.
- [`docs/architecture/`](docs/architecture/) — architecture, package structure, data flow, subscription
  model, testing strategy, and ADRs.
- [`docs/specification/`](docs/specification/) — the requirements traceability matrix, XSD↔OPC↔Go type
  mapping, error mapping, and documented open questions/conservative assumptions.

## Development

```sh
go build ./...
go vet ./...
go test ./...
```

No external dependencies — the standard library only (see `go.mod`).

### Real-client integration tests

[`test/clientintegration/`](test/clientintegration/) is a **separate Go module** (so the base library's own
`go.mod` stays dependency-free) that drives this server's real HTTP endpoint through
[`github.com/dernate/gopcxmlda`](https://github.com/dernate/gopcxmlda), an independently-maintained OPC
XML-DA client — not this library's own fixtures. Run it separately:

```sh
cd test/clientintegration
go test ./...
```

See [`docs/interoperability.md`](docs/interoperability.md) for what this surfaced: one real bug fixed in
this server (an `xsi:type` attribute-ordering issue, safe to fix since XML attribute order carries no
semantic meaning) and two client-side-only quirks that aren't fixable from this server's side.

[`test/dockerintegration/`](test/dockerintegration/) goes one step further: it builds the server into a real
Docker image from a four-level nested address space and drives the same client against a running container,
which is the only place the **shipped `server.Config{}` defaults** are exercised end to end. It also carries
a soak test that keeps the container under concurrent load for a sustained window (`DOCKERINTEGRATION_SOAK`,
default 30s; skipped by `-short`).

```sh
cd test/dockerintegration
go test ./...           # requires a working Docker daemon; skipped if none is reachable
```

**Both modules are excluded from the root `go test ./...`** (that's the whole point of the separate
`go.mod` files — the Docker-daemon and client dependencies stay out of the base library), but both **do**
run in CI as their own jobs. Run them locally whenever changing anything in `xmlda`/`soap` (encoding,
namespace handling, fault shape) and before cutting a release; the client module has already caught one
real bug that no fixture-based test in the main module would have (see above).
