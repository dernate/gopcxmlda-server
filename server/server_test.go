package server

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock"
	"github.com/dernate/gopcxmlda-server/clock/clocktest"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// listenAndServe binds srv's underlying http.Server to an OS-assigned
// loopback port and serves it in a background goroutine, bypassing
// Start()/ListenAndServe() (which hides the chosen port) — the test needs
// the real address to send a real HTTP request. Unlike Start, this uses
// Serve(ln) directly; Shutdown/Serve's relationship to each other is
// exactly what Start()'s own doc comment describes, so this is a faithful
// substitute for it. Returns the listener's address; the server is closed
// by the test's own call to srv.Shutdown.
func listenAndServe(t *testing.T, srv *Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	go func() {
		// http.ErrServerClosed is the expected outcome of the test's own
		// Shutdown call; anything else is a real failure, but there is no
		// live *testing.T to report to from this goroutine — matching
		// doPostSOAP's own note on why background-goroutine assertions
		// must go through a channel instead.
		_ = srv.httpServer.Serve(ln)
	}()
	return ln.Addr().String()
}

// TestServer_Shutdown_UnblocksLongPollBeforeWaitingOnHTTP is a regression
// test for the ordering documented in architecture.md/README.md and
// server.go's own comment (BeginShutdown -> http.Server.Shutdown ->
// subs.Wait): no existing test previously drove Server.Shutdown itself
// against a real net/http.Server (TestServer_ShutdownDuringLongPoll only
// exercises Handler.Shutdown, a plain pass-through with no http.Server
// involved at all), so a future edit swapping that order would not have
// been caught by any test. A real in-flight long-poll SubscriptionPolledRefresh
// request, sent over a real TCP connection, must unblock promptly on
// Shutdown rather than have Shutdown hang until http.Server.Shutdown's own
// wait-for-in-flight-handlers phase is satisfied by the client's full
// (fake-clock) Hold time elapsing.
func TestServer_Shutdown_UnblocksLongPollBeforeWaitingOnHTTP(t *testing.T) {
	fake := clocktest.New(testEpoch)
	reader := newTestReader()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	be := backend.Backend{Status: newTestStatus(), Reader: reader}

	srv, err := NewServer("127.0.0.1:0", Deps{Backend: be, Clock: fake}, Config{MaxPolledRefreshWait: time.Hour})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	addr := listenAndServe(t, srv)
	client := &http.Client{Timeout: 5 * time.Second}

	subResp := postSOAPReal(t, client, addr, subscribeRequestBody("Item1", "CIH1", false))
	sub := decodeResponseBody[xmlda.SubscribeResponse](t, subResp)
	handle := sub.ServerSubHandle

	// A Hold time far beyond anything this test will wait out in real
	// wall-clock time — only Shutdown's own cancellation, not the Hold
	// timer's fake-clock expiry, may unblock the request below.
	holdTime := testEpoch.Add(30 * time.Minute).Format(time.RFC3339Nano)
	before := fake.PendingCount()

	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := postSOAPRealAsync(client, addr, polledRefreshRequestBody(handle, holdTime, 1000, false))
		done <- result{resp, err}
	}()

	if !fake.WaitForPending(before+1, 2*time.Second) {
		t.Fatalf("timed out waiting for the Hold timer to register")
	}

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		shutdownDone <- srv.Shutdown(ctx)
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("PolledRefresh request: %v", r.err)
		}
		if r.resp.StatusCode != http.StatusOK {
			t.Fatalf("got status %d, want 200 after shutdown unblocked the poll", r.resp.StatusCode)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("PolledRefresh did not unblock within 3s of Shutdown — shutdown must not wait out the client's Hold time")
	}

	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func postSOAPReal(t *testing.T, client *http.Client, addr, body string) *http.Response {
	t.Helper()
	resp, err := postSOAPRealAsync(client, addr, body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}

// postSOAPRealAsync has no *testing.T dependency so it is safe to call
// from a goroutine other than the test's own (see doPostSOAP's comment).
func postSOAPRealAsync(client *http.Client, addr, body string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/", bytes.NewReader([]byte(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	return client.Do(req)
}

func decodeResponseBody[T any](t *testing.T, resp *http.Response) *T {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	return decodeResponse[T](t, resp)
}

// TestServer_ListensAndShutsDown covers the convenience wrapper end to
// end on a real listener. Handler() and Start() had no test at all, and
// they are the entry points an application that does not assemble its own
// http.Server actually calls — including the detail that a graceful stop
// is reported as a nil error rather than http.ErrServerClosed, which is
// what makes `log.Fatal(srv.Start())` in a goroutine the trap
// docs/getting-started.md used to demonstrate.
func TestServer_ListensAndShutsDown(t *testing.T) {
	be, _, r := newRWBackend(t)
	r.Set(backend.ItemRef{ItemName: "good"}, xmlda.NewInt32(1))

	srv, err := NewServer("127.0.0.1:0", Deps{Backend: be, Clock: clock.Real{}}, Config{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv.Handler() == nil {
		t.Fatal("Handler() returned nil; an application mounting this into its own mux has nothing to mount")
	}

	// Bind explicitly so the test knows the port, then serve on it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/opcxmlda", srv.Handler())
	httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	})

	url := "http://" + ln.Addr().String() + "/opcxmlda"
	resp, err := http.Post(url, "text/xml", strings.NewReader(readRequestBody([]string{"good"})))
	if err != nil {
		t.Fatalf("POST to the mounted handler: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("mounted handler returned %d, want 200", resp.StatusCode)
	}

	// The wrapper's own Start/Shutdown pair: a graceful stop is nil, not
	// http.ErrServerClosed.
	started := make(chan error, 1)
	go func() { started <- srv.Start() }()
	// Give Start a moment to reach ListenAndServe before stopping it.
	time.Sleep(100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	select {
	case err := <-started:
		if err != nil {
			t.Errorf("Start returned %v after a graceful Shutdown, want nil — anything else makes "+
				"log.Fatal(srv.Start()) kill the process during shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Start did not return after Shutdown")
	}
}
