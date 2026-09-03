package dockerintegration

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestForeignClient_DrivesEveryOperation is the interoperability check the
// rest of this repository cannot make.
//
// Both integration suites — this module's other tests and
// test/clientintegration — drive the server with github.com/dernate/gopcxmlda,
// the reference client. It is independently maintained, which is worth
// something, but it shares an author with the server: what those suites
// prove is that the two agree with each other. A stack that has never seen
// this repository, building its proxy from the OPC XML-DA specification
// alone, is what proves the server speaks the protocol rather than a
// dialect of it.
//
// So: Python's zeep, pinned, in its own container, generating a client
// from testdata/schema/opcxmlda.wsdl (transcribed from the specification's
// appendix) and validating every response against the schema strictly —
// zeep raises rather than ignoring content the schema does not allow,
// which is precisely where Go's own encoding/xml is lenient. The two
// containers share a network so the client reaches the server by name,
// with no port mapping in between to explain away a failure.
//
// The client prints one "CHECK <name> <ok|fail> <detail>" line per
// assertion; this test attributes each failure to its check rather than
// reporting one opaque non-zero exit.
func TestForeignClient_DrivesEveryOperation(t *testing.T) {
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

	client, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			FromDockerfile: testcontainers.FromDockerfile{
				Context:    "../..",
				Dockerfile: "test/dockerintegration/foreignclient/Dockerfile",
			},
			Networks: []string{net.Name},
			Env: map[string]string{
				"OPCXMLDA_ENDPOINT": fmt.Sprintf("http://%s:8080/", serverAlias),
			},
			// The client is a one-shot program: run it and wait for it to
			// finish, rather than waiting for a port it never opens.
			WaitingFor: wait.ForExit().WithExitTimeout(180 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("running foreign client container: %v", err)
	}
	t.Cleanup(func() {
		tCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := client.Terminate(tCtx); err != nil {
			t.Logf("terminating client container: %v", err)
		}
	})

	logs, err := client.Logs(ctx)
	if err != nil {
		t.Fatalf("reading foreign client output: %v", err)
	}
	defer logs.Close()
	raw, err := io.ReadAll(logs)
	if err != nil {
		t.Fatalf("reading foreign client output: %v", err)
	}
	out := string(raw)

	state, err := client.State(ctx)
	if err != nil {
		t.Fatalf("inspecting foreign client container: %v", err)
	}

	checks, failed := parseForeignChecks(out)
	for _, f := range failed {
		t.Errorf("foreign client check %q failed: %s", f.name, f.detail)
	}
	if checks == 0 {
		t.Fatalf("the foreign client produced no CHECK lines at all — it did not get far enough "+
			"to test anything. Output:\n%s", out)
	}
	if state.ExitCode != 0 && len(failed) == 0 {
		t.Errorf("foreign client exited with %d but reported no failed check; output:\n%s",
			state.ExitCode, out)
	}
	if !strings.Contains(out, "ALL CHECKS PASSED") && len(failed) == 0 {
		t.Errorf("foreign client did not run to completion; output:\n%s", out)
	}
	t.Logf("%d checks driven by an independent SOAP stack (zeep) built from the specification's WSDL", checks)
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
