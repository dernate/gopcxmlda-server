# ADR-010: `net/http` directly, no routing framework, no custom transport

## Status
Accepted

## Context
OPC XML-DA over SOAP/HTTP is POST-only, single logical endpoint, SOAPAction/body-based operation dispatch.
The library needs to decide how to expose its HTTP-facing surface, and how TLS/authentication (explicitly
delegated to the transport layer by the spec, REQ-SECURITY-001) should relate to it.

## Decision
Build directly on `net/http`. The primary public surface is a plain `http.Handler` (`server.Handler`),
composable into any consumer's own router, middleware stack, TLS termination, or reverse proxy. An optional
`server.Server` wraps a `net/http.Server` for applications that don't want to assemble one themselves.
Neither TLS nor authentication is implemented or hard-coded inside this library — both are the
responsibility of whatever constructs the `http.Server`/mounts the `http.Handler`.

## Alternatives considered
- **A routing framework** (chi/gin/echo/etc.): rejected — a single POST endpoint with SOAPAction-based
  dispatch has no routing complexity a framework would meaningfully simplify, and adding one would impose a
  dependency and a stylistic choice on every downstream consumer for no benefit.
- **A hand-rolled listener/HTTP parser**: rejected — `net/http` already correctly handles everything needed
  (body limits via `http.MaxBytesReader`, timeouts via `context`, graceful shutdown via
  `http.Server.Shutdown`); reimplementing any of this would be pure risk with no upside.

## Consequences
- Any consumer can mount `server.Handler` into their own existing stack, choose their own TLS certificates
  and auth middleware, and this library never needs to be aware of either.
- Minimal dependency footprint for a base library — no framework dependency to track or version.
- `server.Server.Shutdown`'s ordering relative to `subscription.Manager.BeginShutdown` (see
  `docs/architecture/subscription-model.md`) relies specifically on `http.Server.Shutdown`'s documented
  graceful-drain behavior.
