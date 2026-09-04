package dockerintegration

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Independent-client interoperability.
//
// Every other test in this repository, and every test in
// test/clientintegration, drives the server with
// github.com/dernate/gopcxmlda — the reference client. It is separately
// maintained, which is worth something, but it shares an author with the
// server: what those suites prove is that the two agree with each other.
// A client that has never seen this repository is what proves the server
// speaks the protocol rather than a dialect of it.
//
// Three containers, each an independent implementation, each driving the
// same server over real HTTP on a shared Docker network so it is reached
// by name with no port mapping in between to explain away a failure:
//
//   - pyopcxmlda — a real OPC XML-DA client (Python, MIT). Knows item
//     lists, subscriptions and qualities; hand-builds every request and
//     parses every response with ElementTree.
//   - haskell — a real OPC XML-DA client (mlabs-haskell, MIT). Its
//     request construction and response parser are hand-written from
//     the specification and its parser is STRICT: content the
//     specification does not allow at a position is a hard decode error
//     rather than something quietly skipped. It decodes into a typed sum
//     type, so an ArrayOfDouble either arrives as a vector of doubles or
//     not at all, and it parses SOAP faults in both the 1.1 and 1.2
//     shapes.
//   - zeep — NOT an OPC client but a generic SOAP/XSD stack, generating
//     its proxy from testdata/schema/opcxmlda.wsdl and validating every
//     response against the schema strictly. It checks schema conformance
//     where Go's encoding/xml is lenient; it is not a second opinion on
//     OPC semantics, since the WSDL it reads was transcribed in this
//     repository.
//
// Each client prints one "CHECK <name> <ok|fail> <detail>" line per
// assertion, so a failure is attributed to the assertion that failed
// rather than to an opaque non-zero exit.
//
// The clients run sequentially against one shared server, deliberately
// not in parallel: two of them write the same items and assert the
// round trip, so concurrent runs would race over Speed and Label.
func TestForeignClients_DriveEveryOperation(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()

	net, err := tcnetwork.New(ctx)
	if err != nil {
		t.Fatalf("creating network: %v", err)
	}
	t.Cleanup(func() {
		nCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := net.Remove(nCtx); err != nil {
			t.Logf("removing network: %v", err)
		}
	})

	const serverAlias = "opcxmlda-server"
	server, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			FromDockerfile: testcontainers.FromDockerfile{
				Context:    "../..",
				Dockerfile: "test/dockerintegration/Dockerfile",
			},
			ExposedPorts:   []string{"8080/tcp"},
			Networks:       []string{net.Name},
			NetworkAliases: map[string][]string{net.Name: {serverAlias}},
			WaitingFor:     wait.ForListeningPort("8080/tcp").WithStartupTimeout(120 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("starting server container: %v", err)
	}
	t.Cleanup(func() {
		tCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Terminate(tCtx); err != nil {
			t.Logf("terminating server container: %v", err)
		}
	})

	for _, client := range foreignClients() {
		t.Run(client.name, func(t *testing.T) {
			if client.skip != "" {
				t.Skip(client.skip)
			}
			runForeignClient(t, net.Name, serverAlias, client)
		})
	}

	// A recovered panic is logged, never returned to the client, so a
	// server that survived one of these clients by accident still has to
	// be caught here rather than passing quietly.
	logs, err := server.Logs(ctx)
	if err != nil {
		t.Fatalf("reading server logs: %v", err)
	}
	defer logs.Close()
	raw, err := io.ReadAll(logs)
	if err != nil {
		t.Fatalf("reading server logs: %v", err)
	}
	if strings.Contains(string(raw), "panic") {
		t.Errorf("the server recovered a panic while serving the foreign clients:\n%s", raw)
	}
}

// foreignClientSpec describes one independent client container.
type foreignClientSpec struct {
	name       string
	dockerfile string
	// realClient distinguishes an OPC XML-DA implementation from a
	// generic SOAP stack, for the log line the subtest prints.
	realClient bool
	// exitTimeout bounds the client RUN, not the image build. The
	// drivers long-poll and sleep between subscription polls, so this is
	// well above their expected runtime.
	exitTimeout time.Duration
	// minChecks guards against a client that started, printed a couple
	// of lines and stopped: a green run has to have asserted at least
	// this much. Deliberately a floor, not the exact count, so adding a
	// check to a driver does not fail the Go test.
	minChecks int
	// skip, when set, is why this client does not run here.
	skip string
}

// foreignClients returns the client table, with skip reasons already
// filled in from DOCKERINTEGRATION_CLIENTS.
//
// That variable exists for one reason: the Haskell client's image
// compiles a GHC snapshot from scratch whenever its layer cache is cold,
// which is minutes of wall clock nobody wants while iterating on an
// unrelated change. Set it to a comma-separated list of client names to
// run only those (e.g. DOCKERINTEGRATION_CLIENTS=pyopcxmlda,zeep).
// Unset — the default, and what CI uses — runs all of them.
func foreignClients() []foreignClientSpec {
	var only map[string]bool
	if raw := strings.TrimSpace(os.Getenv("DOCKERINTEGRATION_CLIENTS")); raw != "" {
		only = map[string]bool{}
		for _, name := range strings.Split(raw, ",") {
			only[strings.TrimSpace(name)] = true
		}
	}

	specs := []foreignClientSpec{
		{
			name:        "pyopcxmlda",
			dockerfile:  "test/dockerintegration/clients/pyopcxmlda/Dockerfile",
			realClient:  true,
			exitTimeout: 180 * time.Second,
			minChecks:   40,
		},
		{
			name:       "haskell",
			dockerfile: "test/dockerintegration/clients/haskell/Dockerfile",
			realClient: true,
			// This one compiles a GHC snapshot. Measured: the whole
			// three-client test takes ~11 minutes on a cold layer cache
			// and ~1 minute warm, of which this client is ~44s. The
			// expensive layer depends only on stack.yaml and
			// driver.cabal, so editing the driver does not rebuild it.
			exitTimeout: 180 * time.Second,
			minChecks:   50,
		},
		{
			name:        "zeep",
			dockerfile:  "test/dockerintegration/clients/zeep/Dockerfile",
			exitTimeout: 180 * time.Second,
			minChecks:   25,
		},
	}

	if only != nil {
		for i := range specs {
			if !only[specs[i].name] {
				specs[i].skip = "not selected by DOCKERINTEGRATION_CLIENTS"
			}
		}
		for name := range only {
			if !slices.ContainsFunc(specs, func(s foreignClientSpec) bool { return s.name == name }) {
				// A typo here would otherwise silently run nothing.
				panic("DOCKERINTEGRATION_CLIENTS names an unknown client: " + name)
			}
		}
	}
	return specs
}

func runForeignClient(t *testing.T, netName, serverAlias string, spec foreignClientSpec) {
	t.Helper()
	ctx := context.Background()

	client, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			FromDockerfile: testcontainers.FromDockerfile{
				Context:    "../..",
				Dockerfile: spec.dockerfile,
				// A stable tag and KeepImage, rather than the default
				// throwaway UUID: without them testcontainers deletes the
				// image on Terminate and the next run builds every layer
				// again. That is a nuisance for the pip-based images and
				// prohibitive for the Haskell one, whose dependency layer
				// compiles a GHC snapshot — tens of minutes, from a layer
				// that depends only on its stack.yaml and driver.cabal and
				// so never actually needs rebuilding.
				Repo:      "gopcxmlda-interop-" + spec.name,
				Tag:       "latest",
				KeepImage: true,
			},
			Networks: []string{netName},
			Env: map[string]string{
				"OPCXMLDA_ENDPOINT": fmt.Sprintf("http://%s:8080/", serverAlias),
			},
			// The client is a one-shot program: run it and wait for it to
			// finish, rather than waiting for a port it never opens.
			WaitingFor: wait.ForExit().WithExitTimeout(spec.exitTimeout),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("running %s client container: %v", spec.name, err)
	}
	t.Cleanup(func() {
		tCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := client.Terminate(tCtx); err != nil {
			t.Logf("terminating %s client container: %v", spec.name, err)
		}
	})

	logs, err := client.Logs(ctx)
	if err != nil {
		t.Fatalf("reading %s client output: %v", spec.name, err)
	}
	defer logs.Close()
	raw, err := io.ReadAll(logs)
	if err != nil {
		t.Fatalf("reading %s client output: %v", spec.name, err)
	}
	out := string(raw)

	state, err := client.State(ctx)
	if err != nil {
		t.Fatalf("inspecting %s client container: %v", spec.name, err)
	}

	checks, failed := parseForeignChecks(out)
	for _, f := range failed {
		t.Errorf("%s check %q failed: %s", spec.name, f.name, f.detail)
	}
	if checks == 0 {
		t.Fatalf("the %s client produced no CHECK lines at all — it did not get far enough "+
			"to test anything. Output:\n%s", spec.name, out)
	}
	if checks < spec.minChecks {
		t.Errorf("the %s client ran only %d checks, want at least %d — it stopped early. Output:\n%s",
			spec.name, checks, spec.minChecks, out)
	}
	if state.ExitCode != 0 && len(failed) == 0 {
		t.Errorf("%s client exited with %d but reported no failed check; output:\n%s",
			spec.name, state.ExitCode, out)
	}
	if !strings.Contains(out, "ALL CHECKS PASSED") && len(failed) == 0 {
		t.Errorf("%s client did not run to completion; output:\n%s", spec.name, out)
	}

	kind := "a generic SOAP stack built from the specification's WSDL"
	if spec.realClient {
		kind = "an independent OPC XML-DA client implementation"
	}
	t.Logf("%d checks driven by %s (%s)", checks, spec.name, kind)
}

type foreignCheck struct{ name, detail string }

// parseForeignChecks reads the client's "CHECK <name> <ok|fail> <detail>"
// lines, returning how many ran and which failed.
func parseForeignChecks(out string) (int, []foreignCheck) {
	var failed []foreignCheck
	total := 0
	for _, line := range strings.Split(out, "\n") {
		// Docker multiplexes stdout/stderr with an 8-byte header per
		// frame, so the marker is not necessarily at the start of a line.
		i := strings.Index(line, "CHECK ")
		if i < 0 {
			continue
		}
		fields := strings.SplitN(strings.TrimSpace(line[i+len("CHECK "):]), " ", 3)
		if len(fields) < 2 {
			continue
		}
		total++
		if fields[1] != "ok" {
			detail := ""
			if len(fields) > 2 {
				detail = fields[2]
			}
			failed = append(failed, foreignCheck{name: fields[0], detail: detail})
		}
	}
	return total, failed
}

// TestParseForeignChecks guards the parser itself. It is the only thing
// standing between a foreign client that silently stopped after two
// checks and a green test — so it has to count what ran, not just notice
// the failures.
func TestParseForeignChecks(t *testing.T) {
	const out = "CHECK wsdl-parsed ok 8 operations\n" +
		"\x01\x00\x00\x00\x00\x00\x00\x2aCHECK read-order-preserved ok \n" + // docker's stream header
		"CHECK write-accepted fail opc:E_BADTYPE\n" +
		"noise that is not a check line\n" +
		"CHECK cancel-echoes-handle fail\n" + // no detail
		"ALL CHECKS PASSED\n"

	total, failed := parseForeignChecks(out)
	if total != 4 {
		t.Errorf("counted %d checks, want 4", total)
	}
	if len(failed) != 2 {
		t.Fatalf("found %d failures, want 2: %+v", len(failed), failed)
	}
	if failed[0].name != "write-accepted" || failed[0].detail != "opc:E_BADTYPE" {
		t.Errorf("first failure = %+v", failed[0])
	}
	if failed[1].name != "cancel-echoes-handle" || failed[1].detail != "" {
		t.Errorf("second failure = %+v", failed[1])
	}

	// A client that never produced a line at all must be visible as zero,
	// so the test can say "it did not get far enough to test anything"
	// rather than passing on an empty run.
	if n, f := parseForeignChecks("Traceback (most recent call last):\n"); n != 0 || len(f) != 0 {
		t.Errorf("a crashed client parsed as %d checks / %d failures, want 0/0", n, len(f))
	}
}
