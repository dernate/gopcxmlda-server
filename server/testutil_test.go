package server

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock"
	"github.com/dernate/gopcxmlda-server/soap"
	"github.com/dernate/gopcxmlda-server/telemetry"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

var testEpoch = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// testStatus is a controllable backend.StatusProvider. It also records
// every locale it was called with and can return locale-specific
// StatusInfo, so tests can verify a caller actually reaches the backend
// with the right locale rather than merely echoing the request.
type testStatus struct {
	mu            sync.Mutex
	state         xmlda.ServerState
	start         time.Time
	locales       []string // SupportedLocaleIDs; defaults to {"en-US"}
	infoByLocale  map[string]string
	calledLocales []string
}

func newTestStatus() *testStatus {
	return &testStatus{state: xmlda.ServerStateRunning, start: testEpoch, locales: []string{"en-US"}}
}

func (s *testStatus) GetStatus(ctx context.Context, locale string) (backend.ServerStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calledLocales = append(s.calledLocales, locale)
	return backend.ServerStatus{
		State:              s.state,
		StartTime:          s.start,
		ProductVersion:     "1.0.0",
		StatusInfo:         s.infoByLocale[locale],
		SupportedLocaleIDs: append([]string(nil), s.locales...),
	}, nil
}

func (s *testStatus) SetState(state xmlda.ServerState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
}

// SetLocales overrides SupportedLocaleIDs (default {"en-US"}).
func (s *testStatus) SetLocales(locales []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.locales = locales
}

// SetStatusInfo makes GetStatus(ctx, locale) return info for that exact
// locale string.
func (s *testStatus) SetStatusInfo(locale, info string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.infoByLocale == nil {
		s.infoByLocale = map[string]string{}
	}
	s.infoByLocale[locale] = info
}

// CalledLocales returns every locale GetStatus was called with, in order.
func (s *testStatus) CalledLocales() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calledLocales...)
}

// testReader is a controllable backend.Reader (also used as the target
// of testWriter's writes).
type testReader struct {
	mu      sync.Mutex
	values  map[backend.ItemRef]xmlda.Value
	quality map[backend.ItemRef]xmlda.OPCQuality
}

func newTestReader() *testReader {
	return &testReader{values: map[backend.ItemRef]xmlda.Value{}, quality: map[backend.ItemRef]xmlda.OPCQuality{}}
}

func (r *testReader) Set(ref backend.ItemRef, v xmlda.Value) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[ref] = v
	if _, ok := r.quality[ref]; !ok {
		r.quality[ref] = xmlda.NewGoodQuality()
	}
}

func (r *testReader) Read(ctx context.Context, items []backend.ReadRequestItem) ([]backend.Result[backend.ItemSample], error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]backend.Result[backend.ItemSample], len(items))
	for i, it := range items {
		v, ok := r.values[it.Ref]
		if !ok {
			out[i] = backend.Result[backend.ItemSample]{ResultID: xmlda.ErrUnknownItemName}
			continue
		}
		out[i] = backend.Result[backend.ItemSample]{Value: backend.ItemSample{Value: v, Quality: r.quality[it.Ref], Timestamp: testEpoch}}
	}
	return out, nil
}

// panicReader is a backend.Reader that always panics, for exercising
// ServeHTTP's panic-recovery path (a misbehaving backend must not crash
// the whole connection/server).
type panicReader struct{}

func (panicReader) Read(ctx context.Context, items []backend.ReadRequestItem) ([]backend.Result[backend.ItemSample], error) {
	panic("simulated backend panic")
}

// testWriter writes into an associated testReader's value map.
type testWriter struct {
	reader *testReader
}

func (w *testWriter) Write(ctx context.Context, items []backend.WriteRequestItem) ([]backend.Result[backend.WriteOutcome], error) {
	w.reader.mu.Lock()
	defer w.reader.mu.Unlock()
	out := make([]backend.Result[backend.WriteOutcome], len(items))
	for i, it := range items {
		if _, ok := w.reader.values[it.Ref]; !ok {
			out[i] = backend.Result[backend.WriteOutcome]{ResultID: xmlda.ErrUnknownItemName}
			continue
		}
		w.reader.values[it.Ref] = it.Value
		out[i] = backend.Result[backend.WriteOutcome]{}
	}
	return out, nil
}

// testBrowser returns a fixed backend.BrowseResult.
type testBrowser struct {
	result backend.BrowseResult
	err    error
}

func (b *testBrowser) Browse(ctx context.Context, req backend.BrowseRequest) (backend.BrowseResult, error) {
	return b.result, b.err
}

// testProperties returns fixed properties per item.
type testProperties struct {
	props map[backend.ItemRef][]backend.Property
}

func (p *testProperties) GetProperties(ctx context.Context, reqs []backend.PropertyRequest) ([]backend.Result[[]backend.Property], error) {
	out := make([]backend.Result[[]backend.Property], len(reqs))
	for i, r := range reqs {
		props, ok := p.props[r.Ref]
		if !ok {
			out[i] = backend.Result[[]backend.Property]{ResultID: xmlda.ErrUnknownItemName}
			continue
		}
		out[i] = backend.Result[[]backend.Property]{Value: props}
	}
	return out, nil
}

func newTestHandler(t *testing.T, be backend.Backend, cfg Config, clk clock.Clock) *Handler {
	t.Helper()
	h, err := New(Deps{Backend: be, Clock: clk}, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = h.Shutdown(ctx)
	})
	return h
}

// doPostSOAP has no *testing.T dependency, so it is safe to call from a
// goroutine other than the one running the test (Go's testing package
// forbids calling t.Fatal-family methods from any goroutine but the
// test's own — see e.g. TestServer_ShutdownDuringLongPoll).
func doPostSOAP(h http.Handler, body string) *http.Response {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func postSOAP(t *testing.T, h http.Handler, body string) *http.Response {
	t.Helper()
	return doPostSOAP(h, body)
}

const soapEnvelopeOpen = `<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://schemas.xmlsoap.org/soap/envelope/"><SOAP-ENV:Body>`
const soapEnvelopeClose = `</SOAP-ENV:Body></SOAP-ENV:Envelope>`

func getStatusRequestBody(crh string) string {
	return soapEnvelopeOpen + `<GetStatus xmlns="` + xmlda.Namespace + `" ClientRequestHandle="` + crh + `"/>` + soapEnvelopeClose
}

func readRequestBody(itemNames []string) string {
	var items strings.Builder
	for _, n := range itemNames {
		items.WriteString(`<Items ItemName="` + n + `"/>`)
	}
	return soapEnvelopeOpen + `<Read xmlns="` + xmlda.Namespace + `"><Options ClientRequestHandle="CRH1"/><ItemList>` +
		items.String() + `</ItemList></Read>` + soapEnvelopeClose
}

func writeRequestBody(itemName, xsiType, value string, returnValuesOnReply bool) string {
	return soapEnvelopeOpen + `<Write xmlns="` + xmlda.Namespace + `" ReturnValuesOnReply="` + strconv.FormatBool(returnValuesOnReply) + `">` +
		`<Options ClientRequestHandle="CRH1"/><ItemList><Items ItemName="` + itemName + `">` +
		`<Value xmlns:xsd="` + xmlda.XSDNamespace + `" xmlns:xsi="` + xmlda.XSINamespace + `" xsi:type="xsd:` + xsiType + `">` + value + `</Value>` +
		`</Items></ItemList></Write>` + soapEnvelopeClose
}

func browseRequestBody() string {
	return soapEnvelopeOpen + `<Browse xmlns="` + xmlda.Namespace + `"/>` + soapEnvelopeClose
}

func getPropertiesRequestBody(itemName string) string {
	return soapEnvelopeOpen + `<GetProperties xmlns="` + xmlda.Namespace + `" ReturnAllProperties="true">` +
		`<ItemIDs ItemName="` + itemName + `"/></GetProperties>` + soapEnvelopeClose
}

func subscribeRequestBody(itemName, clientItemHandle string, returnValuesOnReply bool) string {
	return soapEnvelopeOpen + `<Subscribe xmlns="` + xmlda.Namespace + `" ReturnValuesOnReply="` + strconv.FormatBool(returnValuesOnReply) + `" SubscriptionPingRate="0">` +
		`<Options ClientRequestHandle="CRH1"/><ItemList><Items ItemName="` + itemName + `" ClientItemHandle="` + clientItemHandle + `"/></ItemList></Subscribe>` + soapEnvelopeClose
}

func polledRefreshRequestBody(handle string, holdTime string, waitTimeMs int, returnAllItems bool) string {
	holdAttr := ""
	if holdTime != "" {
		holdAttr = ` HoldTime="` + holdTime + `"`
	}
	return soapEnvelopeOpen + `<SubscriptionPolledRefresh xmlns="` + xmlda.Namespace + `"` + holdAttr +
		` WaitTime="` + strconv.Itoa(waitTimeMs) + `" ReturnAllItems="` + strconv.FormatBool(returnAllItems) + `">` +
		`<ServerSubHandles>` + handle + `</ServerSubHandles></SubscriptionPolledRefresh>` + soapEnvelopeClose
}

func subscriptionCancelRequestBody(handle string) string {
	return soapEnvelopeOpen + `<SubscriptionCancel xmlns="` + xmlda.Namespace + `" ServerSubHandle="` + handle + `"/>` + soapEnvelopeClose
}

// subscriptionCancelRequestBodyWithHandle is the same request carrying a
// ClientRequestHandle, which §3.7.2 p.68 requires the response to echo.
func subscriptionCancelRequestBodyWithHandle(handle, clientRequestHandle string) string {
	return soapEnvelopeOpen + `<SubscriptionCancel xmlns="` + xmlda.Namespace +
		`" ServerSubHandle="` + handle + `" ClientRequestHandle="` + clientRequestHandle + `"/>` + soapEnvelopeClose
}

func decodeFault(t *testing.T, resp *http.Response) *soap.Fault {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	var env soap.Envelope[struct{}]
	if err := xml.Unmarshal(data, &env); err != nil {
		t.Fatalf("decoding response envelope: %v\nbody: %s", err, data)
	}
	return env.Body.Fault
}

func decodeResponse[T any](t *testing.T, resp *http.Response) *T {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	var env soap.Envelope[T]
	if err := xmlda.Decode(data, &env); err != nil {
		t.Fatalf("decoding response: %v\nbody: %s", err, data)
	}
	if env.Body.Fault != nil {
		t.Fatalf("unexpected fault: %+v", env.Body.Fault)
	}
	if env.Body.Content == nil {
		t.Fatalf("expected non-nil response content")
	}
	return env.Body.Content
}

// readBody reads resp's body as a string. Used by tests that assert on the
// response BYTES rather than the decoded form — the shape of defect that
// Go's own lenient decoder hides, since it fills in the same zero value
// whether an element was present or absent (see
// TestRead_FailedItemOmitsQuality).
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	return string(data)
}

// decodeResponseFrom is decodeResponse over bytes already read, for a test
// that needs to assert on both the bytes and the decoded form.
func decodeResponseFrom[T any](t *testing.T, raw string) *T {
	t.Helper()
	var env soap.Envelope[T]
	if err := xmlda.Decode([]byte(raw), &env); err != nil {
		t.Fatalf("decoding response: %v\nbody: %s", err, raw)
	}
	if env.Body.Fault != nil {
		t.Fatalf("unexpected fault: %+v", env.Body.Fault)
	}
	if env.Body.Content == nil {
		t.Fatal("expected non-nil response content")
	}
	return env.Body.Content
}

// runtimeNumGoroutine wraps runtime.NumGoroutine so goroutine-accounting
// assertions read as intent rather than as a stray runtime import.
func runtimeNumGoroutine() int { return runtime.NumGoroutine() }

func newRWBackend(t *testing.T) (backend.Backend, *testStatus, *testReader) {
	t.Helper()
	st := newTestStatus()
	r := newTestReader()
	return backend.Backend{Status: st, Reader: r, Writer: &testWriter{reader: r}}, st, r
}

// itemByHandle finds the reply item carrying clientItemHandle.
func itemByHandle(t *testing.T, items []xmlda.ItemValue, handle string) xmlda.ItemValue {
	t.Helper()
	for _, iv := range items {
		if iv.ClientItemHandle == handle {
			return iv
		}
	}
	t.Fatalf("no reply item with ClientItemHandle %q in %d items", handle, len(items))
	return xmlda.ItemValue{}
}

// --- shared helpers ---

// steppableClock is a clock.Clock whose Now a test advances by hand while
// timers still run on the real clock — the shape a test needs when it must
// control an expiry check without also freezing the HTTP handler's own
// waits.
type steppableClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *steppableClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func (c *steppableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *steppableClock) After(d time.Duration) <-chan time.Time { return clock.Real{}.After(d) }

func (c *steppableClock) NewTimer(d time.Duration) clock.Timer { return clock.Real{}.NewTimer(d) }

func (c *steppableClock) AfterFunc(d time.Duration, f func()) clock.Timer {
	return clock.Real{}.AfterFunc(d, f)
}

func (c *steppableClock) Sleep(d time.Duration) { clock.Real{}.Sleep(d) }

// Regression tests for the server-layer defects found in the wire-format
// review. Each one fails against the behavior that shipped before the
// corresponding fix; the comment on each says what that behavior was.

// --- H2: a property with no value must not destroy the response ---

// valuelessProperties returns one item whose properties include one that
// the backend could not read: a per-property ResultID with the Value left
// at its zero. That is a legitimate backend outcome and the single most
// likely one for E_INVALIDPID.
type valuelessProperties struct{}

func (valuelessProperties) GetProperties(_ context.Context, reqs []backend.PropertyRequest) ([]backend.Result[[]backend.Property], error) {
	out := make([]backend.Result[[]backend.Property], len(reqs))
	for i := range reqs {
		out[i] = backend.Result[[]backend.Property]{Value: []backend.Property{
			{ID: xmlda.PropDescription, Value: xmlda.NewString("readable")},
			{ID: xmlda.PropHighEU, ResultID: xmlda.ErrInvalidPID}, // no Value
		}}
	}
	return out, nil
}

// --- H4: success-with-caveat codes keep their value ---

// successCodeReader reports S_CLAMP together with a perfectly usable
// value, the combination §2.6 explicitly describes ("for non-critical
// exceptions the returned value is useful").
type successCodeReader struct{}

func (successCodeReader) Read(_ context.Context, items []backend.ReadRequestItem) ([]backend.Result[backend.ItemSample], error) {
	out := make([]backend.Result[backend.ItemSample], len(items))
	for i := range items {
		out[i] = backend.Result[backend.ItemSample]{
			ResultID: xmlda.SuccessClamp,
			Value: backend.ItemSample{
				Value:   xmlda.NewInt32(1000),
				Quality: xmlda.NewGoodQuality(),
			},
		}
	}
	return out, nil
}

// --- backend latency is actually observed ---

// recordingMetrics captures ObserveBackendLatency calls.
type recordingMetrics struct {
	telemetry.Metrics
	mu  sync.Mutex
	ops []string
}

func (m *recordingMetrics) ObserveBackendLatency(op string, _ time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ops = append(m.ops, op)
}

func (m *recordingMetrics) seen() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.ops...)
}
