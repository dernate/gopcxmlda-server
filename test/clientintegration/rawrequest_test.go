package clientintegration

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/examples/basic-server/memorybackend"
	"github.com/dernate/gopcxmlda-server/server"
	"github.com/dernate/gopcxmlda-server/soap"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// The tests in this file write their request bytes by hand instead of
// going through the reference client, and that is the point: they cover
// input a conforming client library cannot be made to produce — a
// malformed item attribute, an xsd:dateTime without the optional timezone
// offset, a tampered continuation point, a filter outside the schema's
// enumeration. It is still a real HTTP round trip against a real
// net/http server; only the request author differs.

// newRawTestServer starts a real HTTP server over a real memorybackend and
// returns its base URL, for tests that post hand-written request bodies.
// Shutdown follows this library's own documented ordering, and fails the
// test if a background goroutine outlives it.
func newRawTestServer(t *testing.T, cfg server.Config) string {
	t.Helper()
	be := memorybackend.New()
	h, err := server.New(server.Deps{
		Backend: backend.Backend{Status: be, Reader: be, Writer: be, Browser: be, Properties: be},
	}, cfg)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(func() {
		h.BeginShutdown()
		ts.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.Shutdown(ctx); err != nil {
			t.Errorf("Handler background goroutines did not exit: %v", err)
		}
		be.Close()
	})
	return ts.URL
}

const rawEnvelopeOpen = `<?xml version="1.0" encoding="UTF-8"?>` +

	`<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://schemas.xmlsoap.org/soap/envelope/"><SOAP-ENV:Body>`
const rawEnvelopeClose = `</SOAP-ENV:Body></SOAP-ENV:Envelope>`

// rawPost posts body to url over real HTTP and returns the status and
// response bytes.
func rawPost(t *testing.T, url, body string) (int, []byte) {
	t.Helper()
	status, data, err := doRawPost(url, body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return status, data
}

// doRawPost is rawPost without the *testing.T dependency, so it is safe to
// call from a goroutine other than the test's own.
func doRawPost(url, body string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, data, nil
}

// rawDecode decodes a successful response body into T, failing the test if
// the server answered with a SOAP fault instead.
func rawDecode[T any](t *testing.T, data []byte) *T {
	t.Helper()
	var env soap.Envelope[T]
	if err := xmlda.Decode(data, &env); err != nil {
		t.Fatalf("decoding response: %v\nbody: %s", err, data)
	}
	if env.Body.Fault != nil {
		t.Fatalf("unexpected fault %s: %s", env.Body.Fault.Code, env.Body.Fault.Text)
	}
	if env.Body.Content == nil {
		t.Fatal("response carried no content")
	}
	return env.Body.Content
}

// rawFault decodes a fault response body, failing the test if the server
// answered successfully instead.
func rawFault(t *testing.T, data []byte) *soap.Fault {
	t.Helper()
	var env soap.Envelope[struct{}]
	if err := xml.Unmarshal(data, &env); err != nil {
		t.Fatalf("decoding fault envelope: %v\nbody: %s", err, data)
	}
	if env.Body.Fault == nil {
		t.Fatalf("expected a SOAP fault, got: %s", data)
	}
	return env.Body.Fault
}

// pagingPropBrowser wraps the example backend's Browser to produce two
// things the example address space does not: real pagination (one element
// per page) and a per-property condition.
//
// Both are properties of the SERVER's plumbing, not of the backend — the
// continuation-point MAC and the Errors list are framework concerns
// (REQ-BROWSE-002, §3.8.2) — so exercising them needs a backend that hands
// the framework something to work with, without changing the example
// backend every other test in this module depends on.
type pagingPropBrowser struct {
	inner backend.Browser
}

func (b pagingPropBrowser) Browse(ctx context.Context, req backend.BrowseRequest) (backend.BrowseResult, error) {
	// Ask the real backend for everything, then page it ourselves so the
	// cursor is genuinely ours to hand back.
	full := req
	full.ContinuationPoint = ""
	full.MaxElementsReturned = 0
	res, err := b.inner.Browse(ctx, full)
	if err != nil {
		return backend.BrowseResult{}, err
	}

	offset := 0
	if req.ContinuationPoint != "" {
		// A cursor this backend did not issue must be rejected here, not
		// dereferenced — the contract backend.BrowseRequest.ContinuationPoint
		// documents even with the framework's MAC in place.
		if _, err := fmt.Sscanf(req.ContinuationPoint, "offset-%d", &offset); err != nil {
			return backend.BrowseResult{}, fmt.Errorf("unusable continuation point %q", req.ContinuationPoint)
		}
		if offset < 0 || offset > len(res.Elements) {
			return backend.BrowseResult{}, fmt.Errorf("continuation point %q is out of range", req.ContinuationPoint)
		}
	}

	page := res.Elements[offset:]
	if len(page) > 1 {
		page = page[:1]
	}
	// One failing property on every returned element, so the reply always
	// carries a per-property condition for the Errors list to report.
	out := make([]backend.BrowseElement, len(page))
	for i, el := range page {
		el.Properties = append(append([]backend.Property(nil), el.Properties...),
			backend.Property{ID: xmlda.PropHighEU, ResultID: xmlda.ErrInvalidPID})
		out[i] = el
	}

	next := ""
	if offset+len(page) < len(res.Elements) {
		next = fmt.Sprintf("offset-%d", offset+len(page))
	}
	return backend.BrowseResult{Elements: out, ContinuationPoint: next, MoreElements: next != ""}, nil
}

// newPagingTestServer is newRawTestServer with Browse replaced by
// pagingPropBrowser.
func newPagingTestServer(t *testing.T, cfg server.Config) string {
	t.Helper()
	be := memorybackend.New()
	h, err := server.New(server.Deps{
		Backend: backend.Backend{
			Status: be, Reader: be, Writer: be,
			Browser:    pagingPropBrowser{inner: be},
			Properties: be,
		},
	}, cfg)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(func() {
		h.BeginShutdown()
		ts.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.Shutdown(ctx); err != nil {
			t.Errorf("Handler background goroutines did not exit: %v", err)
		}
		be.Close()
	})
	return ts.URL
}

// --- one bad item costs that item, not the batch ---

// TestIntegration_MalformedItemDoesNotFailTheBatch is the case the finding
// was written about: a poller reading many tags in one batch used to lose
// the entire response to one bad attribute, going blind on all of them
// rather than on one.
func TestIntegration_MalformedItemDoesNotFailTheBatch(t *testing.T) {
	url := newRawTestServer(t, server.Config{})

	const n = 20
	const badIdx = 7
	var items strings.Builder
	for i := range n {
		_, _ = fmt.Fprintf(&items, `<Items ItemName="Demo/Temperature" ClientItemHandle="H%d"`, i)
		if i == badIdx {
			items.WriteString(` MaxAge="not-a-number"`)
		}
		items.WriteString(`/>`)
	}
	body := rawEnvelopeOpen + `<Read xmlns="` + xmlda.Namespace + `">` +
		`<Options ReturnItemName="true" ReturnDiagnosticInfo="true"/><ItemList>` +
		items.String() + `</ItemList></Read>` + rawEnvelopeClose

	status, data := rawPost(t, url, body)
	if status != http.StatusOK {
		t.Fatalf("got status %d, want 200 — one malformed item still faults the whole Read: %s", status, data)
	}
	out := rawDecode[xmlda.ReadResponse](t, data)
	if len(out.RItemList.Items) != n {
		t.Fatalf("got %d reply items, want %d", len(out.RItemList.Items), n)
	}
	good := 0
	for i, iv := range out.RItemList.Items {
		if i == badIdx {
			if iv.ResultID != xmlda.ErrFail {
				t.Errorf("item %d: ResultID = %v, want E_FAIL", i, iv.ResultID)
			}
			// DiagnosticInfo is a *string: §3.1.6 makes the element's
			// presence the answer to ReturnDiagnosticInfo, so nil means
			// "the client did not ask" and a pointer to "" means "asked,
			// nothing to report".
			if iv.DiagnosticInfo == nil {
				t.Errorf("item %d: no DiagnosticInfo element although the request asked for one", i)
			} else if !strings.Contains(*iv.DiagnosticInfo, "MaxAge") {
				t.Errorf("item %d: DiagnosticInfo does not name the field: %q", i, *iv.DiagnosticInfo)
			}
			continue
		}
		if !iv.ResultID.IsZero() || iv.Value == nil {
			t.Errorf("item %d lost its value over one unrelated bad item: %+v", i, iv)
			continue
		}
		good++
	}
	if good != n-1 {
		t.Errorf("got %d readable items, want %d", good, n-1)
	}
}

// --- an offsetless xsd:dateTime completes the exchange ---

// TestIntegration_OffsetlessHoldTimePolls pins the interop fix end to end:
// a client that omits xsd:dateTime's optional timezone offset can complete
// a full Subscribe → SubscriptionPolledRefresh cycle. Before, the poll
// faulted on the HoldTime attribute before any handle was looked at, so
// the subscription was simply unreachable.
func TestIntegration_OffsetlessHoldTimePolls(t *testing.T) {
	url := newRawTestServer(t, server.Config{MaxPolledRefreshWait: 2 * time.Second})

	subBody := rawEnvelopeOpen + `<Subscribe xmlns="` + xmlda.Namespace + `" ReturnValuesOnReply="true" SubscriptionPingRate="10000">` +
		`<Options ReturnItemName="true"/><ItemList>` +
		`<Items ItemName="Demo/Temperature" ClientItemHandle="CIH1"/>` +
		`</ItemList></Subscribe>` + rawEnvelopeClose
	status, data := rawPost(t, url, subBody)
	if status != http.StatusOK {
		t.Fatalf("Subscribe got status %d: %s", status, data)
	}
	sub := rawDecode[xmlda.SubscribeResponse](t, data)
	if sub.ServerSubHandle == "" {
		t.Fatal("no subscription handle")
	}

	// Deliberately no offset — legal xsd:dateTime, illegal RFC 3339.
	hold := time.Now().Add(120 * time.Millisecond).UTC().Format("2006-01-02T15:04:05.000")
	pollBody := rawEnvelopeOpen + `<SubscriptionPolledRefresh xmlns="` + xmlda.Namespace + `"` +
		` HoldTime="` + hold + `" WaitTime="0" ReturnAllItems="true">` +
		`<Options ReturnItemName="true" ReturnItemTime="true"/>` +
		`<ServerSubHandles>` + sub.ServerSubHandle + `</ServerSubHandles>` +
		`</SubscriptionPolledRefresh>` + rawEnvelopeClose

	status, data = rawPost(t, url, pollBody)
	if status != http.StatusOK {
		t.Fatalf("an offsetless HoldTime faulted the poll (status %d): %s", status, data)
	}
	poll := rawDecode[xmlda.SubscriptionPolledRefreshResponse](t, data)
	if len(poll.RItemList) != 1 || len(poll.RItemList[0].Items) != 1 {
		t.Fatalf("got %+v, want one item for one subscription", poll.RItemList)
	}
	if poll.RItemList[0].Items[0].Value == nil {
		t.Error("the polled item carried no value")
	}
}

// TestIntegration_OffsetlessWriteTimestamp pins the other half of the widening: a
// client writing REQ-WRITE-003's Value+Timestamp pair with an offsetless
// timestamp used to lose the whole Write.
func TestIntegration_OffsetlessWriteTimestamp(t *testing.T) {
	url := newRawTestServer(t, server.Config{})
	body := rawEnvelopeOpen + `<Write xmlns="` + xmlda.Namespace + `"` +
		` xmlns:xsi="` + xmlda.XSINamespace + `" xmlns:xsd="` + xmlda.XSDNamespace + `">` +
		`<Options ReturnItemName="true"/><ItemList>` +
		`<Items ItemName="Demo/Message" Timestamp="2026-08-30T12:00:00">` +
		`<Value xsi:type="xsd:string">written</Value></Items>` +
		`</ItemList></Write>` + rawEnvelopeClose

	status, data := rawPost(t, url, body)
	if status != http.StatusOK {
		t.Fatalf("an offsetless item Timestamp faulted the Write (status %d): %s", status, data)
	}
	out := rawDecode[xmlda.WriteResponse](t, data)
	if len(out.RItemList.Items) != 1 {
		t.Fatalf("got %d reply items, want 1", len(out.RItemList.Items))
	}
	// The backend may legitimately refuse a client-supplied timestamp
	// (REQ-WRITE-003 allows E_NOTSUPPORTED); what must NOT happen is the
	// whole request failing to parse.
	if code := out.RItemList.Items[0].ResultID; code == xmlda.ErrFail {
		t.Errorf("ResultID = %v, want either success or a specific per-item condition", code)
	}
}

// --- continuation-point authenticity and page-size independence ---

// TestIntegration_ContinuationPointIsAuthenticated pins that a client
// cannot mint its own Browse cursor over the wire, and that a legitimate
// one still pages — including across a page-size change.
func TestIntegration_ContinuationPointIsAuthenticated(t *testing.T) {
	url := newPagingTestServer(t, server.Config{})

	// Browsing into "Demo" rather than the root: the root holds a single
	// branch, so there would be no second page for a cursor to point at.
	browse := func(attrs string) (int, []byte) {
		return rawPost(t, url, rawEnvelopeOpen+
			`<Browse xmlns="`+xmlda.Namespace+`" ItemName="Demo" `+attrs+`/>`+rawEnvelopeClose)
	}

	// One element per page, so the backend really has to issue a cursor.
	status, data := browse(`MaxElementsReturned="1"`)
	if status != http.StatusOK {
		t.Fatalf("Browse got status %d: %s", status, data)
	}
	first := rawDecode[xmlda.BrowseResponse](t, data)
	if first.ContinuationPoint == "" {
		t.Fatal("the paging backend issued no continuation point; nothing to authenticate")
	}
	if !strings.HasSuffix(first.ContinuationPoint, ":offset-1") {
		t.Fatalf("the token does not end in the backend's own cursor: %q", first.ContinuationPoint)
	}

	// Replaying it verbatim works.
	if status, data := browse(`MaxElementsReturned="1" ContinuationPoint="` + first.ContinuationPoint + `"`); status != http.StatusOK {
		t.Fatalf("a legitimate continuation point was rejected (status %d): %s", status, data)
	}

	// Changing only the page size still works: a continuation point marks a
	// position, not a page size.
	if status, data := browse(`MaxElementsReturned="5" ContinuationPoint="` + first.ContinuationPoint + `"`); status != http.StatusOK {
		t.Fatalf("changing the page size mid-pagination was rejected (status %d): %s", status, data)
	}

	// Tampering with the cursor half is not accepted, even though the
	// client controls every input the filter digest is built from and can
	// therefore recompute the digest itself.
	idx := strings.LastIndex(first.ContinuationPoint, ":")
	forged := first.ContinuationPoint[:idx+1] + "forged-cursor"
	status, data = browse(`MaxElementsReturned="1" ContinuationPoint="` + forged + `"`)
	if status == http.StatusOK {
		t.Fatalf("a forged cursor was accepted: %s", data)
	}
	if f := rawFault(t, data); f.Code.Local != "E_INVALIDCONTINUATIONPOINT" {
		t.Errorf("got fault %s, want E_INVALIDCONTINUATIONPOINT", f.Code)
	}
}

// --- Browse filter validation and error reporting ---

// TestIntegration_BrowseInvalidFilterFaults pins filter validation over the wire:
// E_INVALIDFILTER was defined in the package and never emitted, so an
// unrecognized filter reached the backend with no vocabulary to say the
// request made no sense.
func TestIntegration_BrowseInvalidFilterFaults(t *testing.T) {
	url := newRawTestServer(t, server.Config{})
	status, data := rawPost(t, url, rawEnvelopeOpen+
		`<Browse xmlns="`+xmlda.Namespace+`" BrowseFilter="everything"/>`+rawEnvelopeClose)
	if status == http.StatusOK {
		t.Fatalf("an invalid BrowseFilter was accepted: %s", data)
	}
	if f := rawFault(t, data); f.Code.Local != "E_INVALIDFILTER" {
		t.Errorf("got fault %s, want E_INVALIDFILTER", f.Code)
	}

	// The three legal values, and an absent attribute, all still work.
	for _, attrs := range []string{``, `BrowseFilter="all"`, `BrowseFilter="branch"`, `BrowseFilter="item"`} {
		if status, data := rawPost(t, url, rawEnvelopeOpen+
			`<Browse xmlns="`+xmlda.Namespace+`" `+attrs+`/>`+rawEnvelopeClose); status != http.StatusOK {
			t.Errorf("Browse with %q got status %d: %s", attrs, status, data)
		}
	}
}

// TestIntegration_BrowsePropertyErrorsCarryText pins the invariant: every per-property
// condition a Browse reply carries must also appear in its Errors list.
// Browse was the only one of the six operations that never filled that
// list, so a property that failed to read arrived with a bare ResultID in
// a response a client reads as error-free.
func TestIntegration_BrowsePropertyErrorsCarryText(t *testing.T) {
	url := newPagingTestServer(t, server.Config{})
	// ReturnErrorText="true" explicitly. The Browse element's own schema
	// declaration gives this attribute default="false" — unlike
	// RequestOptions, which is where the true default lives — and with no
	// text there are no OPCError entries to assert on at all (§3.1.9:
	// "For each OPCError there will be a Text element"). The per-property
	// ResultIDs, checked below, are unaffected either way.
	body := rawEnvelopeOpen + `<Browse xmlns="` + xmlda.Namespace + `"` +
		` ReturnPropertyValues="true" ReturnAllProperties="true" ReturnErrorText="true"/>` +
		rawEnvelopeClose
	status, data := rawPost(t, url, body)
	if status != http.StatusOK {
		t.Fatalf("Browse got status %d: %s", status, data)
	}
	out := rawDecode[xmlda.BrowseResponse](t, data)

	inErrors := map[xmlda.ErrorCode]bool{}
	for _, e := range out.Errors {
		inErrors[e.ID] = true
		if e.Text == "" {
			t.Errorf("Errors entry %s carries no Text", e.ID.Local)
		}
	}
	reported := 0
	for _, el := range out.Elements {
		for _, p := range el.Properties {
			if p.ResultID.IsZero() {
				continue
			}
			reported++
			if !inErrors[p.ResultID] {
				t.Errorf("property %s reports %s but the response's Errors list does not mention it",
					p.Name, p.ResultID.Local)
			}
		}
	}
	if reported == 0 {
		t.Fatal("the reply carried no per-property condition, so this test asserted nothing")
	}
	if len(out.Errors) == 0 {
		t.Error("Errors is empty despite the reply carrying per-property conditions")
	}
	t.Logf("checked %d per-property conditions against %d Errors entries", reported, len(out.Errors))
}

// --- an over-long HoldTime is clamped ---

// TestIntegration_OverlongHoldTimeIsClamped pins the clamping: a client following
// the specification's own "a minute or two" guidance against a shorter
// server ceiling gets a valid, clamped reply instead of a fault on every
// single poll.
func TestIntegration_OverlongHoldTimeIsClamped(t *testing.T) {
	url := newRawTestServer(t, server.Config{MaxPolledRefreshWait: 200 * time.Millisecond})

	subBody := rawEnvelopeOpen + `<Subscribe xmlns="` + xmlda.Namespace + `" ReturnValuesOnReply="false" SubscriptionPingRate="10000">` +
		`<Options/><ItemList><Items ItemName="Demo/Temperature" ClientItemHandle="C1"/></ItemList>` +
		`</Subscribe>` + rawEnvelopeClose
	_, data := rawPost(t, url, subBody)
	sub := rawDecode[xmlda.SubscribeResponse](t, data)

	hold := time.Now().Add(2 * time.Minute).UTC().Format("2006-01-02T15:04:05.000Z")
	pollBody := rawEnvelopeOpen + `<SubscriptionPolledRefresh xmlns="` + xmlda.Namespace + `"` +
		` HoldTime="` + hold + `" WaitTime="0" ReturnAllItems="true">` +
		`<Options/><ServerSubHandles>` + sub.ServerSubHandle + `</ServerSubHandles>` +
		`</SubscriptionPolledRefresh>` + rawEnvelopeClose

	start := time.Now()
	status, data := rawPost(t, url, pollBody)
	elapsed := time.Since(start)
	if status != http.StatusOK {
		t.Fatalf("an over-long HoldTime faulted (status %d): %s", status, data)
	}
	if elapsed > 10*time.Second {
		t.Errorf("the hold was not clamped: took %v", elapsed)
	}
	rawDecode[xmlda.SubscriptionPolledRefreshResponse](t, data)
}

// --- in-flight requests are bounded ---

// TestIntegration_ConcurrentRequestLimit pins admission control over a real transport:
// past the configured limit a request is refused with E_BUSY rather than
// queued, while ordinary sequential traffic well past that number is
// untouched — a slot is held for one request, not reserved per client.
func TestIntegration_ConcurrentRequestLimit(t *testing.T) {
	url := newRawTestServer(t, server.Config{MaxConcurrentRequests: 4, MaxPolledRefreshWait: 2 * time.Second})

	readBody := rawEnvelopeOpen + `<Read xmlns="` + xmlda.Namespace + `"><Options/><ItemList>` +
		`<Items ItemName="Demo/Temperature"/></ItemList></Read>` + rawEnvelopeClose
	for i := range 20 {
		if status, data := rawPost(t, url, readBody); status != http.StatusOK {
			t.Fatalf("sequential request %d got status %d: %s", i, status, data)
		}
	}

	// Hold the slots with long-polls and prove the excess is refused
	// rather than queued.
	const subs = 8
	handles := make([]string, subs)
	for i := range handles {
		subBody := rawEnvelopeOpen + `<Subscribe xmlns="` + xmlda.Namespace + `" SubscriptionPingRate="30000">` +
			`<Options/><ItemList><Items ItemName="Demo/Temperature" ClientItemHandle="C` +
			fmt.Sprint(i) + `"/></ItemList></Subscribe>` + rawEnvelopeClose
		_, data := rawPost(t, url, subBody)
		handles[i] = rawDecode[xmlda.SubscribeResponse](t, data).ServerSubHandle
	}

	hold := time.Now().Add(900 * time.Millisecond).UTC().Format("2006-01-02T15:04:05.000Z")
	var wg sync.WaitGroup
	var mu sync.Mutex
	statuses := map[int]int{}
	busy := 0
	for _, handle := range handles {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			pollBody := rawEnvelopeOpen + `<SubscriptionPolledRefresh xmlns="` + xmlda.Namespace + `"` +
				` HoldTime="` + hold + `" WaitTime="0" ReturnAllItems="false">` +
				`<Options/><ServerSubHandles>` + h + `</ServerSubHandles>` +
				`</SubscriptionPolledRefresh>` + rawEnvelopeClose
			status, data, err := doRawPost(url, pollBody)
			if err != nil {
				return
			}
			mu.Lock()
			statuses[status]++
			if status == http.StatusInternalServerError && strings.Contains(string(data), "E_BUSY") {
				busy++
			}
			mu.Unlock()
		}(handle)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if busy == 0 {
		t.Errorf("%d concurrent long-polls against a limit of 4 produced no E_BUSY: %v", subs, statuses)
	}
	if statuses[http.StatusOK] == 0 {
		t.Errorf("the limit refused every request rather than admitting up to four: %v", statuses)
	}
	t.Logf("statuses: %v (E_BUSY: %d)", statuses, busy)
}
