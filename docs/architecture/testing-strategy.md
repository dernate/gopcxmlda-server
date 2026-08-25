# Testing Strategy

Tests are written alongside implementation, not after (per project directive). Every test, where meaningful,
names the requirement ID(s) it covers in a comment or table-driven test-case name, so
`docs/specification/traceability-matrix.md` stays verifiable against actual test code, not just intent.

## Test data layout

```
testdata/
  requests/    real + synthetic OPC XML-DA requests
  responses/   real + synthetic OPC XML-DA responses
  faults/      real + synthetic SOAP faults (all 4 observed shapes)
  invalid/     malformed/adversarial XML for negative tests — currently unused/empty; negative-path tests
               (e.g. xmlda/dispatch_test.go) use inline Go string literals instead
  golden/      unused — originally planned to hold hand-reviewed expected-decode JSON per fixture (see
               below), but every golden-file test ended up asserting expected values inline in the test
               code instead (e.g. TestSubscribeRequest_RealFixture); no test reads from this directory,
               and it's currently empty
```

Real captured fixtures (`subscribe_679`/`680`, `getstatus_632`/`639`, `browse_653`/`662`, `browse_676`/`684`,
`getproperties_103`/`116`, `read_649`/`676`, `read_169`/`182`, `subscriptionpolledrefresh_226`/`232`,
`subscriptioncancel_448`/`460`, and the three fault files — item names, site names, and vendor/client
identifiers anonymized where the original capture named a real system) are the ground truth for the
golden-file tests; everything else is synthesized to cover cases the real captures don't (alternative
prefixes, missing namespace, wrong namespace URI, empty item lists, etc.).

## Test categories

1. **Unit tests** — one `_test.go` per source file, standard Go table-driven style.
2. **Table-driven tests** — used throughout for anything with a finite, enumerable case set (all
   `ScalarType`s, all `QualityField`/`LimitField` values, all standard `ErrorCode`s, the `BrowseFilter`
   truth table).
3. **XML marshal/unmarshal tests** — per `xmlda` type, both directions independently asserted.
4. **Round-trip tests** — decode → encode → decode, asserting the two decoded values are equal (not
   byte-equal — see golden-file note below). Run for every fixture and, for `Value`, over the full cross
   product of `ScalarType` × representative edge values (zero, negative, max-width, empty string, empty byte
   slice, 0-element and 3-element arrays, one `ArrayOfAnyType` with mixed/nested types, one `KindUnknown`
   value with arbitrary inner XML).
5. **Golden-file tests** — for each real fixture, a `*_RealFixture` test (e.g.
   `TestSubscribeRequest_RealFixture`, `xmlda/subscribe_test.go`) decodes the real captured XML and asserts
   the expected field values directly in the test code, hand-written once against a manually verified decode
   of the source XML. There is no separate expected-decode file and no `go test -update`-style mechanical
   regeneration — the assertions are independently-verified ground truth typed by hand, and a mechanical
   regenerate-and-commit would silently defeat that purpose. (`testdata/golden/` was an earlier plan for
   this — see the layout note above — but was never adopted; ignore it.) (Non-golden round-trip/table-driven
   tests may still use ordinary Go assertion helpers without this restriction.)
6. **Negative tests** — malformed XML, missing required fields, unknown operations, wrong/missing/alternative
   namespaces, invalid data types, invalid values, empty item lists.
7. **Boundary/limit tests** — numeric type boundaries (min/max per scalar width — directly targeting the
   reference client's known byte/unsignedByte width-asymmetry bug class), `Config` limits.
8. **Partial-success tests** — multi-item requests with a mix of successful and failing items, asserting
   correct per-item `ResultID` and shared `Errors` dedup.
9. **Subscription lifecycle tests** — driven entirely by `clock/clocktest.Fake`, no real sleeps: normal
   Hold+Wait return, early-return-on-change, `ReturnAllItems` snapshot vs. delta, abandonment reaper sweep,
   mid-hold cancellation, mid-hold shutdown.
10. **Concurrency tests** — `-race`-enabled stress tests: many goroutines concurrently
    Create/Cancel/PolledRefresh across overlapping and disjoint handle sets; assert `E_BUSY` correctness (no
    false positives on disjoint handles, no false negatives on true overlap) and no double-free of a handle.
11. **Race-detector-compatible tests** — every concurrency test above runs under `go test -race`.
12. **HTTP handler tests** — `httptest.NewServer`/`httptest.NewRequest` against `server.Handler`: body-size
    limiting, content-type validation, per-operation round trips, fault scenarios, shutdown during an
    in-flight long poll (using a fake clock wired through `server.Deps`).
13. **End-to-end tests** — real SOAP requests against an `httptest` server backed by the in-memory reference
    backend (`examples/basic-server/memorybackend`), exercising all 8 operations together.
14. **Fuzz tests** — `FuzzValueRoundTrip` seeded from the boundary-value table above, targeting (a)
    `resolveQName` against adversarial prefix/namespace combinations (unknown prefix, prefix redeclared at
    multiple depths, unprefixed value with no default namespace in scope — must error cleanly, never panic);
    (b) numeric literal decoding at scalar-width boundaries. Run in a development-appropriate bounded corpus
    (`go test -fuzz=FuzzValueRoundTrip -fuzztime=30s` locally / in CI, not unbounded).

    **Development-environment note**: in this project's sandboxed Windows dev environment, `go test -fuzz`
    fails to spawn its corpus-worker subprocess (`fork/exec ...: Zugriff verweigert`, i.e. access denied —
    a subprocess-execution restriction of the sandbox, not a code defect; reproduced identically via both
    the Bash and PowerShell tools, with and without sandbox bypass). Plain `go test` still runs every
    `FuzzXxx` function's seed corpus (`f.Add` entries) as ordinary subtests, so seed coverage is verified
    continuously; actual randomized fuzzing (`-fuzz`) should be run in CI or a normal local shell outside
    this sandbox before relying on it for a release.

## Development-environment note: `-race` requires cgo — resolved 2026-08-24

`go test -race` requires cgo, which requires a working C compiler. This was blocked for several milestones
(WP-8 through WP-11) because no `gcc` was resolvable on `PATH` even with `CGO_ENABLED=1`. It is now resolved:
`gcc` 16.2.0 was installed to `C:\Tools\winlibs\bin` and added to the machine `PATH`. The one remaining
wrinkle is that already-running shells in a session started before the install don't pick up the updated
`PATH` automatically — worked around by prepending `C:\Tools\winlibs\bin` to `$env:Path` for the session
running the tests, rather than needing a full restart. With that, `go test -race ./...` is clean across all
9 packages, and `go test -race -count=5 ./subscription/... ./server/...` (the two concurrency-sensitive
packages) is stable across repeated runs with no flakiness. `docs/development/tasks.md` and
`docs/development/final-review.md` record this as closed rather than outstanding.

## Per-milestone checks

After every milestone: `go fmt ./...`, `go vet ./...`, `go test ./...`; `go test -race ./...` at least once
per milestone from the point the `subscription` package exists onward. Goroutine-leak checks (e.g. via
`go.uber.org/goleak` in `subscription`'s test suite) run after every `Manager.Shutdown` in a lifecycle test.

`test/clientintegration` (see `README.md`) is **not** part of `go test ./...` (separate `go.mod`, by design)
and is not run automatically by anything — it must be run manually (`cd test/clientintegration && go test
./...`) before a release and after any change to `xmlda`/`soap` wire encoding, since it is the only check
against a real, independently-maintained client rather than this repository's own fixtures.

## Byte-exact vs. semantic-equivalence testing

Golden-file tests compare decoded Go structs (via `cmp.Diff` or equivalent), not raw bytes — semantically
equivalent XML is not required to be byte-identical (attribute order, whitespace, and namespace-prefix choice
may all legitimately vary). Where this library's own *output* needs to be pinned down precisely (e.g. "we
always emit SOAP 1.1 with a QName-qualified faultcode"), that is asserted by decoding the output back through
the same tolerant parser and checking the decoded *meaning*, plus a small number of explicit string-contains
assertions for wire-shape properties that can't be captured any other way (e.g. "the emitted fault uses
prefix `SOAP-ENV`" is not asserted — only "the emitted fault's Code resolves to the XML-DA namespace" is).
