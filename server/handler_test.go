package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock"
	"github.com/dernate/gopcxmlda-server/telemetry"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// --- an incomplete backend ServerStatus is reported and repaired ---

// brokenStatus returns a ServerStatus missing everything a backend author
// can forget.
type brokenStatus struct{}

func (brokenStatus) GetStatus(context.Context, string) (backend.ServerStatus, error) {
	return backend.ServerStatus{}, nil // no State, no StartTime, no locales
}

// recordingLogger captures log lines so a test can assert the operator was
// actually told.
type recordingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *recordingLogger) add(level, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	parts := []string{level, msg}
	for _, a := range args {
		parts = append(parts, strings.TrimSpace(strings.Trim(stringify(a), "[]")))
	}
	l.lines = append(l.lines, strings.Join(parts, " "))
}

func stringify(a any) string {
	switch v := a.(type) {
	case string:
		return v
	default:
		return ""
	}
}

func (l *recordingLogger) Debug(msg string, args ...any) { l.add("DEBUG", msg, args...) }

func (l *recordingLogger) Info(msg string, args ...any) { l.add("INFO", msg, args...) }

func (l *recordingLogger) Warn(msg string, args ...any) { l.add("WARN", msg, args...) }

func (l *recordingLogger) Error(msg string, args ...any) { l.add("ERROR", msg, args...) }

func (l *recordingLogger) Lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// TestStatusFor_IncompleteBackendStatusIsRepairedAndLoggedOnce pins the validation. Nothing
// validated what GetStatus returned, and an empty State is the
// consequential case: ReplyBase omits the attribute when it is empty, and
// ServerState is use="required" in the schema — so one forgotten field in
// a backend made every response this server produced schema-invalid, with
// nothing anywhere reporting it.
func TestStatusFor_IncompleteBackendStatusIsRepairedAndLoggedOnce(t *testing.T) {
	log := &recordingLogger{}
	r := newTestReader()
	r.Set(backend.ItemRef{ItemName: "A"}, xmlda.NewInt32(1))
	h, err := New(Deps{
		Backend: backend.Backend{Status: brokenStatus{}, Reader: r},
		Clock:   clock.Real{},
		Logger:  log,
		Metrics: telemetry.NoopMetrics(),
	}, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = h.Shutdown(ctx)
	})

	raw := readBody(t, postSOAP(t, h, readRequestBody([]string{"A"})))
	if !strings.Contains(raw, `ServerState="running"`) {
		t.Errorf("the reply carries no ServerState, making it schema-invalid:\n%s", raw)
	}

	var found bool
	for _, line := range log.Lines() {
		if strings.HasPrefix(line, "ERROR") && strings.Contains(line, "ServerStatus") {
			found = true
		}
	}
	if !found {
		t.Errorf("the incomplete ServerStatus was repaired silently; lines: %v", log.Lines())
	}

	// And the complaint is logged once, not on every request.
	for range 5 {
		postSOAP(t, h, readRequestBody([]string{"A"}))
	}
	n := 0
	for _, line := range log.Lines() {
		if strings.Contains(line, "ServerStatus") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the ServerStatus complaint was logged %d times, want exactly once", n)
	}
}

// TestValidateServerStatus_NamesEveryProblem pins the diagnostic itself:
// an operator reading the log line must learn which fields are missing,
// not merely that something is.
func TestValidateServerStatus_NamesEveryProblem(t *testing.T) {
	problems := validateServerStatus(backend.ServerStatus{})
	if len(problems) != 3 {
		t.Fatalf("got %d problems, want 3 (State, StartTime, SupportedLocaleIDs): %v", len(problems), problems)
	}
	joined := strings.Join(problems, " ")
	for _, want := range []string{"State", "StartTime", "SupportedLocaleIDs"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the diagnostic does not mention %s: %v", want, problems)
		}
	}

	ok := backend.ServerStatus{
		State: xmlda.ServerStateRunning, StartTime: testEpoch, SupportedLocaleIDs: []string{"en-US"},
	}
	if got := validateServerStatus(ok); len(got) != 0 {
		t.Errorf("a valid status reported problems: %v", got)
	}
}

// blockingStatus parks GetStatus until gate is closed, so a test can hold
// a request in flight deterministically.
type blockingStatus struct {
	*testStatus
	gate    chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (b *blockingStatus) WaitEntered(d time.Duration) bool {
	b.once.Do(func() { b.entered = make(chan struct{}) })
	select {
	case <-b.entered:
		return true
	case <-time.After(d):
		return false
	}
}

func (b *blockingStatus) GetStatus(ctx context.Context, locale string) (backend.ServerStatus, error) {
	b.once.Do(func() { b.entered = make(chan struct{}) })
	select {
	case <-b.entered:
	default:
		close(b.entered)
	}
	select {
	case <-b.gate:
	case <-ctx.Done():
		return backend.ServerStatus{}, ctx.Err()
	}
	return b.testStatus.GetStatus(ctx, locale)
}

// --- the status cache must not leak goroutines against a stuck backend ---

// hangingStatus blocks in GetStatus until released, ignoring ctx — the
// behavior of a driver that reaches a field device over a blocking
// library call, which is the common case rather than the exotic one.
type hangingStatus struct{ release chan struct{} }

func (h hangingStatus) GetStatus(context.Context, string) (backend.ServerStatus, error) {
	<-h.release
	return backend.ServerStatus{
		State: xmlda.ServerStateRunning, StartTime: time.Unix(0, 0).UTC(),
		SupportedLocaleIDs: []string{"en-US"},
	}, nil
}

// TestStatusCache_HangingBackendLeaksNoGoroutines pins the cost of a
// waiter that gives up. statusFor holds the cache lock across the backend
// call deliberately — that is what collapses a burst into one fetch — so
// every other request in that burst waits on it. Waiting on a sync.Mutex
// with a context meant parking a goroutine per waiter, and against a
// backend that never returns those goroutines never returned either: the
// request itself timed out and released its MaxConcurrentRequests slot,
// so nothing bounded the growth and it scaled with the request RATE.
func TestStatusCache_HangingBackendLeaksNoGoroutines(t *testing.T) {
	be := hangingStatus{release: make(chan struct{})}
	h := newTestHandler(t, backend.Backend{Status: be, Reader: newTestReader()},
		Config{RequestTimeout: 50 * time.Millisecond}, clock.Real{})
	t.Cleanup(func() { close(be.release) })

	// Deliberately not waited on: the request that wins the cache lock is
	// inside the stuck backend call and never returns, which is the whole
	// premise. Every OTHER request must give up on its RequestTimeout and
	// leave nothing behind.
	body := readRequestBody([]string{"A"})
	fire := func(n int) {
		for range n {
			go func() {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
			}()
		}
		time.Sleep(250 * time.Millisecond) // long enough for each to hit RequestTimeout
	}

	fire(20) // warm-up: one of these takes the lock and keeps it
	runtime.GC()
	before := runtimeNumGoroutine()
	fire(40)
	runtime.GC()
	mid := runtimeNumGoroutine()
	fire(40)
	runtime.GC()
	after := runtimeNumGoroutine()
	t.Logf("goroutines: before=%d after 40=%d after 80=%d", before, mid, after)

	// A couple of goroutines of slack for the runtime; what must not
	// happen is growth proportional to the number of requests.
	// Exactly one goroutine may be parked in the stuck backend call,
	// however many requests arrive: the status lock is released by the
	// FETCH, so no second fetch starts while the first has not answered.
	// Growth proportional to the request count is the defect.
	if after-before > 5 {
		t.Errorf("80 requests against a stuck backend left %d extra goroutines (%d after 40, %d after 80); "+
			"the leak scales with the request rate and MaxConcurrentRequests does not bound it",
			after-before, mid, after)
	}
}

// --- the pre-dispatch status fetch is memoized ---

// TestStatusCache_OneFetchPerBurst pins the fix for every request having
// cost an extra backend GetStatus call. The state check before dispatch
// (REQ-SERVER-002) needs a status, but not a fresh one per request.
func TestStatusCache_OneFetchPerBurst(t *testing.T) {
	be, status, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{}, nil)

	for range 5 {
		postSOAP(t, h, readRequestBody([]string{"Item1"}))
	}
	if n := len(status.CalledLocales()); n != 1 {
		t.Fatalf("5 Reads caused %d backend GetStatus calls, want 1 (the rest served from the cache)", n)
	}
}

// TestStatusCache_GetStatusAlwaysFetches is the exemption that keeps the
// cache honest: the operation whose whole purpose is reporting the status
// must never answer from a cached one.
func TestStatusCache_GetStatusAlwaysFetches(t *testing.T) {
	be, status, _ := newMinimalBackend()
	h := newTestHandler(t, be, Config{}, nil)

	for range 3 {
		postSOAP(t, h, getStatusRequestBody("CRH1"))
	}
	if n := len(status.CalledLocales()); n != 3 {
		t.Fatalf("3 GetStatus requests caused %d backend calls, want 3 — GetStatus must not use the cache", n)
	}
}

// TestStatusCache_Disabled confirms the escape hatch works: a negative
// TTL restores a fresh fetch per request.
func TestStatusCache_Disabled(t *testing.T) {
	be, status, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{StatusCacheTTL: -1}, nil)

	for range 3 {
		postSOAP(t, h, readRequestBody([]string{"Item1"}))
	}
	if n := len(status.CalledLocales()); n != 3 {
		t.Fatalf("got %d backend GetStatus calls with caching disabled, want 3", n)
	}
}
