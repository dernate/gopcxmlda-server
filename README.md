# gopcxmlda-server

A Go base library for implementing **OPC XML-DA 1.0 servers**. It is not an application — it is a set of
small, composable packages you build a server on top of: SOAP/XML wire handling, OPC XML-DA vocabulary and
the 8 protocol operations, a small backend contract you implement against your own data source, a
subscription engine, and an `http.Handler`/`http.Server` composition layer.

The library has **no dependency on any concrete SCADA system, database, or vendor structure** — you plug in
your own process data by implementing a handful of small interfaces (see
[`docs/backend-implementation.md`](docs/backend-implementation.md)).

## Versioning and stability

The exported API is **not yet stable**. There is no `v1.0.0` tag; until
there is, treat every release as `v0.x` and expect source-incompatible
changes, each of which is listed in [`CHANGELOG.md`](CHANGELOG.md). Pin an
exact version.

The largest open question before a `v1` is the size of the exported
surface: `xmlda` alone exports well over two hundred symbols, a number of
which are implementation helpers rather than things an application needs.
Moving those behind `internal/` is a breaking change worth making before
the API is frozen, not after.

## Status

This library implements the 8 OPC XML-DA 1.0 operations (`GetStatus`, `Read`, `Write`, `Subscribe`,
`SubscriptionPolledRefresh`, `SubscriptionCancel`, `Browse`, `GetProperties`) against the specification in
`docs/OPCDataAccessXMLSpecification.pdf`, backed by a tracked requirements matrix and 485 tests across all three modules (460 in the base module) (unit, golden-file,
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

[`test/dockerintegration/clients/`](test/dockerintegration/clients/) answers the question neither of those
can: both drive the server with the same author's client, which proves the two agree with each other. Three
independent clients, each in its own container on a shared Docker network with the server, exercise all
eight operations and print one assertion per line so a failure names what failed:

| Client | What it is | What it proves |
|---|---|---|
| [`pyopcxmlda`](test/dockerintegration/clients/pyopcxmlda/) | A **real OPC XML-DA client** (Python, MIT) | Knows item lists, subscriptions and qualities. Hand-builds every request; a second opinion on protocol *semantics*, not just schema shape. |
| [`haskell`](test/dockerintegration/clients/haskell/) | A **real OPC XML-DA client** ([mlabs-haskell](https://github.com/mlabs-haskell/opc-xml-da-client), MIT) | Request construction *and* response parser hand-written from the specification, and the parser is **strict**: content the specification does not allow is a hard decode error. Decodes into a typed sum type (an `ArrayOfDouble` arrives as a vector of doubles or not at all) and parses SOAP faults in both the 1.1 and 1.2 shapes. |
| [`zeep`](test/dockerintegration/clients/zeep/) | A **generic SOAP/XSD stack** — *not* an OPC client | Builds its proxy from [`testdata/schema/opcxmlda.wsdl`](testdata/schema/opcxmlda.wsdl) and validates every response against the schema strictly, which is exactly where Go's `encoding/xml` is lenient. It is *not* an independent opinion on OPC semantics: that WSDL was transcribed in this repository, so an error in the transcription would be invisible to it. |

The two real clients found three defects in this server — all since fixed — and prompted one deliberate
tolerance; [`docs/interoperability.md`](docs/interoperability.md) lists each with the reasoning.

They run as part of the `dockerintegration` suite. The Haskell image compiles a GHC snapshot, so the whole
independent-client test takes ~11 minutes on a cold layer cache and ~1 minute once that layer is built —
it depends only on its `stack.yaml` and `driver.cabal`, so editing a driver does not rebuild it. To skip it
while iterating, name the clients you want:

```sh
DOCKERINTEGRATION_CLIENTS=pyopcxmlda,zeep go test -run TestForeignClients ./...
```

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
