package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock"
	"github.com/dernate/gopcxmlda-server/soap"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// TestHealthHandler_ServesAProbe pins the endpoint a Kubernetes httpGet
// probe can actually point at. The SOAP endpoint answers every GET with
// 405 — correctly, OPC XML-DA is POST-only — so before this there was
// nowhere for a probe to go and no way for an application to build one.
func TestHealthHandler_ServesAProbe(t *testing.T) {
	be, _, r := newRWBackend(t)
	r.Set(backend.ItemRef{ItemName: "good"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{MaxConcurrentRequests: 8}, clock.Real{})
	probe := h.HealthHandler()

	rec := httptest.NewRecorder()
	probe.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz returned %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store — a cached liveness answer is worse than none", cc)
	}

	var got Stats
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("the probe body is not the documented JSON: %v\n%s", err, rec.Body.String())
	}
	if got.ShuttingDown {
		t.Error("a freshly started server reports itself as shutting down")
	}
	if got.MaxConcurrentRequests != 8 {
		t.Errorf("MaxConcurrentRequests = %d, want the configured 8", got.MaxConcurrentRequests)
	}

	// HEAD must answer with the status and no body — probes use it.
	recHead := httptest.NewRecorder()
	probe.ServeHTTP(recHead, httptest.NewRequest(http.MethodHead, "/healthz", nil))
	if recHead.Code != http.StatusOK {
		t.Errorf("HEAD returned %d, want 200", recHead.Code)
	}
	if recHead.Body.Len() != 0 {
		t.Errorf("HEAD returned a body of %d bytes", recHead.Body.Len())
	}

	// Anything else is refused, with Allow, like any other endpoint.
	recPost := httptest.NewRecorder()
	probe.ServeHTTP(recPost, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if recPost.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST returned %d, want 405", recPost.Code)
	}
	if recPost.Header().Get("Allow") == "" {
		t.Error("405 from the probe carries no Allow header")
	}
}

// TestHealthHandler_FailsReadinessOnceShutdownBegins is the reason the
// endpoint exists at all: the load balancer has to stop sending work
// BEFORE the server starts refusing it.
func TestHealthHandler_FailsReadinessOnceShutdownBegins(t *testing.T) {
	be, _, _ := newRWBackend(t)
	h := newTestHandler(t, be, Config{}, clock.Real{})
	probe := h.HealthHandler()

	h.BeginShutdown()

	rec := httptest.NewRecorder()
	probe.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("after BeginShutdown the probe returned %d, want 503", rec.Code)
	}
	var got Stats
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("probe body: %v", err)
	}
	if !got.ShuttingDown {
		t.Error("Stats.ShuttingDown is false although shutdown has begun")
	}
}

// TestStats_ReflectsLiveState checks that the numbers an operator watches
// actually move.
func TestStats_ReflectsLiveState(t *testing.T) {
	be, _, r := newRWBackend(t)
	r.Set(backend.ItemRef{ItemName: "good"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{}, clock.Real{})
	t.Cleanup(func() { _ = h.Shutdown(context.Background()) })

	if n := h.Stats().ActiveSubscriptions; n != 0 {
		t.Fatalf("a fresh server reports %d subscriptions", n)
	}
	sub := decodeResponse[xmlda.SubscribeResponse](t, postSOAP(t, h, subscribeRequestBody("good", "CIH", false)))
	if sub.ServerSubHandle == "" {
		t.Fatal("setup: no subscription handle")
	}
	if n := h.Stats().ActiveSubscriptions; n != 1 {
		t.Errorf("ActiveSubscriptions = %d after one Subscribe, want 1", n)
	}

	// The backend state is picked up from the status the server already
	// fetched for its own dispatch check, without a call of its own.
	st := h.Stats()
	if !st.BackendReachable {
		t.Error("BackendReachable is false although every request has succeeded")
	}
	if st.BackendState != string(xmlda.ServerStateRunning) {
		t.Errorf("BackendState = %q, want %q", st.BackendState, xmlda.ServerStateRunning)
	}
	if st.BackendCheckedAt.IsZero() {
		t.Error("BackendCheckedAt is zero although a status has been fetched")
	}
}

// TestHealthHandler_DoesNotBlockOnAStuckBackend is the property that
// separates a useful probe from a harmful one: a probe that hangs when
// the data source hangs turns one unreachable device into a restart loop.
func TestHealthHandler_DoesNotBlockOnAStuckBackend(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	be := backend.Backend{Status: hangingStatus{release: release}, Reader: newTestReader()}
	h := newTestHandler(t, be, Config{RequestTimeout: 50 * time.Millisecond}, clock.Real{})

	// Get a request in flight so the status fetch is actually stuck.
	go postSOAP(t, h, readRequestBody([]string{"A"}))
	time.Sleep(150 * time.Millisecond)

	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		done <- rec.Code
	}()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Errorf("probe returned %d while the backend was stuck, want 200 — the server itself "+
				"is still serving", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the health probe blocked on a stuck backend")
	}
}

// TestSoapVersionHelpers covers the two small decisions that shape a
// response's envelope and status code.
func TestSoapVersionHelpers(t *testing.T) {
	if got := soapVersion(nil); got != 0 {
		t.Errorf("soapVersion(nil) = %v, want SOAP 1.1 (the version OPC XML-DA is defined over)", got)
	}
	doc, err := xmlda.NewDocument([]byte(`<soap:Envelope xmlns:soap="http://www.w3.org/2003/05/soap-envelope">` +
		`<soap:Body/></soap:Envelope>`))
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	if got := soapVersion(doc); got != 1 {
		t.Errorf("a SOAP 1.2 envelope resolved to %v, want SOAP 1.2", got)
	}
}

// TestIsSenderFault pins the status code a SOAP 1.2 client gets. §7 of
// SOAP 1.2 splits what 1.1 fixed at 500: a fault the SENDER caused is a
// 400, everything else a 500. Answering 500 for a client's own malformed
// request is not wrong enough to break anything, but it is wrong.
func TestIsSenderFault(t *testing.T) {
	for _, tc := range []struct {
		code soap.QName
		want bool
	}{
		{soap.QName{Space: soap.NS11, Local: "Client"}, true},
		{soap.QName{Space: soap.NS12, Local: "Sender"}, true},
		{soap.QName{Space: soap.NS11, Local: "MustUnderstand"}, true},
		{soap.QName{Space: soap.NS11, Local: "VersionMismatch"}, true},
		{soap.QName{Space: soap.NS11, Local: "Server"}, false},
		{soap.QName{Space: soap.NS12, Local: "Receiver"}, false},
		// An OPC XML-DA result code is not a SOAP-defined fault: the
		// server produced it, so it is a 500 either way.
		{soap.QName{Space: xmlda.Namespace, Local: "E_FAIL"}, false},
		{soap.QName{Space: xmlda.Namespace, Local: "E_BADTYPE"}, false},
		{soap.QName{}, false},
	} {
		if got := isSenderFault(tc.code); got != tc.want {
			t.Errorf("isSenderFault(%s) = %v, want %v", tc.code, got, tc.want)
		}
	}
}
