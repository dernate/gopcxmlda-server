package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/telemetry"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

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

// TestGetProperties_ValuelessPropertyDoesNotFailWholeResponse pins the
// fix for the defect where a single property without a value turned the
// entire GetProperties response into an E_FAIL SOAP fault: toItemProperty
// attached the zero Value unconditionally whenever ReturnPropertyValues
// was set, and a zero Value has no declared type, so the encode failed
// and writeResponse fell back to a blanket fault — discarding every other
// item's data over one missing property value.
func TestGetProperties_ValuelessPropertyDoesNotFailWholeResponse(t *testing.T) {
	be, _, _ := newMinimalBackend()
	be.Properties = valuelessProperties{}
	h := newTestHandler(t, be, Config{}, nil)

	resp := postSOAP(t, h, soapEnvelopeOpen+
		`<GetProperties xmlns="`+xmlda.Namespace+`" ReturnAllProperties="true" ReturnPropertyValues="true">`+
		`<ItemIDs ItemName="Item1"/></GetProperties>`+soapEnvelopeClose)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got HTTP %d, want 200 — a valueless property must not fault the whole operation.\n%s",
			resp.StatusCode, body)
	}
	got := decodeResponse[xmlda.GetPropertiesResponse](t, resp)
	if len(got.PropertyLists) != 1 {
		t.Fatalf("got %d property lists, want 1", len(got.PropertyLists))
	}
	props := got.PropertyLists[0].Properties
	if len(props) != 2 {
		t.Fatalf("got %d properties, want both the readable one and the failing one: %+v", len(props), props)
	}
	// The readable property keeps its value; the failing one reports its
	// condition and simply carries no Value element.
	var readable, failing *xmlda.ItemProperty
	for i := range props {
		switch props[i].Name.Local {
		case "description":
			readable = &props[i]
		case "highEU":
			failing = &props[i]
		}
	}
	if readable == nil || readable.Value == nil {
		t.Fatalf("the readable property lost its value: %+v", props)
	}
	if failing == nil {
		t.Fatalf("the failing property vanished from the response: %+v", props)
	}
	if failing.Value != nil {
		t.Errorf("the failing property carries a Value element: %+v", failing.Value)
	}
	if failing.ResultID != xmlda.ErrInvalidPID {
		t.Errorf("got ResultID %+v, want E_INVALIDPID", failing.ResultID)
	}
}

// TestBrowse_ValuelessPropertyDoesNotFailWholeResponse is the same defect
// on the Browse path, which shares toItemProperty.
func TestBrowse_ValuelessPropertyDoesNotFailWholeResponse(t *testing.T) {
	be, _, _ := newMinimalBackend()
	be.Browser = &testBrowser{result: backend.BrowseResult{
		Elements: []backend.BrowseElement{{
			Name: "Item1", IsItem: true, Ref: &backend.ItemRef{ItemName: "Item1"},
			Properties: []backend.Property{{ID: xmlda.PropHighEU, ResultID: xmlda.ErrInvalidPID}},
		}},
	}}
	h := newTestHandler(t, be, Config{}, nil)

	resp := postSOAP(t, h, soapEnvelopeOpen+
		`<Browse xmlns="`+xmlda.Namespace+`" ReturnAllProperties="true" ReturnPropertyValues="true"/>`+
		soapEnvelopeClose)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got HTTP %d, want 200.\n%s", resp.StatusCode, body)
	}
	got := decodeResponse[xmlda.BrowseResponse](t, resp)
	if len(got.Elements) != 1 || len(got.Elements[0].Properties) != 1 {
		t.Fatalf("got %+v, want one element carrying one property", got.Elements)
	}
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

// TestRead_SuccessCodeKeepsValue pins the fix for haveSample having been
// computed as resultID.IsZero(), which dropped the value for every
// S_-prefixed code — the one class of result where the specification says
// the value is useful and the client needs both it and the code.
func TestRead_SuccessCodeKeepsValue(t *testing.T) {
	be, _, _ := newMinimalBackend()
	be.Reader = successCodeReader{}
	h := newTestHandler(t, be, Config{}, nil)

	got := decodeResponse[xmlda.ReadResponse](t, postSOAP(t, h, readRequestBody([]string{"Item1"})))
	if len(got.RItemList.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(got.RItemList.Items))
	}
	item := got.RItemList.Items[0]
	if item.ResultID != xmlda.SuccessClamp {
		t.Fatalf("got ResultID %+v, want S_CLAMP", item.ResultID)
	}
	if item.Value == nil {
		t.Fatal("S_CLAMP item carries no Value; a non-critical exception's value is still useful (§2.6)")
	}
	v, err := item.Value.Int32()
	if err != nil || v != 1000 {
		t.Fatalf("got value %v (err %v), want 1000", v, err)
	}
}

// --- M4: Subscribe honors ReqType ---

// TestSubscribe_HonorsReqType pins the fix for Subscribe having merged
// the hierarchical ReqType and then discarded it: a client subscribing an
// int item as xsd:double used to get int back, with neither the requested
// conversion nor the E_BADTYPE that would have said no.
func TestSubscribe_HonorsReqType(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "IntItem"}, xmlda.NewInt32(7))
	h := newTestHandler(t, be, Config{}, nil)

	body := soapEnvelopeOpen +
		`<Subscribe xmlns="` + xmlda.Namespace + `" xmlns:xsd="` + xmlda.XSDNamespace + `" ` +
		`ReturnValuesOnReply="true"><Options ClientRequestHandle="CRH1"/>` +
		`<ItemList ReqType="xsd:double"><Items ItemName="IntItem" ClientItemHandle="CIH1"/></ItemList>` +
		`</Subscribe>` + soapEnvelopeClose
	got := decodeResponse[xmlda.SubscribeResponse](t, postSOAP(t, h, body))

	if len(got.RItemList.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(got.RItemList.Items))
	}
	v := got.RItemList.Items[0].ItemValue.Value
	if v == nil {
		t.Fatal("subscribed item carries no value")
	}
	if v.Type() != xmlda.TypeDouble {
		t.Fatalf("got type %q, want the requested xsd:double — ReqType was ignored", v.Type())
	}
	f, err := v.Float64()
	if err != nil || f != 7 {
		t.Fatalf("got %v (err %v), want 7", f, err)
	}
}

// TestSubscribe_UnconvertibleReqTypeIsBadType is the other half: a type
// this server cannot convert to must be reported, not silently ignored.
func TestSubscribe_UnconvertibleReqTypeIsBadType(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "StrItem"}, xmlda.NewString("not a number"))
	h := newTestHandler(t, be, Config{}, nil)

	body := soapEnvelopeOpen +
		`<Subscribe xmlns="` + xmlda.Namespace + `" xmlns:xsd="` + xmlda.XSDNamespace + `" ` +
		`ReturnValuesOnReply="true"><Options ClientRequestHandle="CRH1"/>` +
		`<ItemList><Items ItemName="StrItem" ClientItemHandle="CIH1" ReqType="xsd:int"/></ItemList>` +
		`</Subscribe>` + soapEnvelopeClose
	got := decodeResponse[xmlda.SubscribeResponse](t, postSOAP(t, h, body))

	if len(got.RItemList.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(got.RItemList.Items))
	}
	if id := got.RItemList.Items[0].ItemValue.ResultID; id != xmlda.ErrBadType {
		t.Fatalf("got ResultID %+v, want E_BADTYPE", id)
	}
}

// --- M8: ReqType's namespace is part of its identity ---

// TestRead_ReqTypeFromForeignNamespaceIsBadType pins the fix for
// coerceToReqType having matched on the local name alone, which accepted
// e.g. "vendor:int" from any namespace and coerced it as if it were
// xsd:int — a type this server does not actually implement.
func TestRead_ReqTypeFromForeignNamespaceIsBadType(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(5))
	h := newTestHandler(t, be, Config{}, nil)

	body := soapEnvelopeOpen +
		`<Read xmlns="` + xmlda.Namespace + `" xmlns:vendor="http://example.com/vendor">` +
		`<Options ClientRequestHandle="CRH1"/>` +
		`<ItemList><Items ItemName="Item1" ReqType="vendor:int"/></ItemList></Read>` + soapEnvelopeClose
	got := decodeResponse[xmlda.ReadResponse](t, postSOAP(t, h, body))

	if len(got.RItemList.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(got.RItemList.Items))
	}
	if id := got.RItemList.Items[0].ResultID; id != xmlda.ErrBadType {
		t.Fatalf("got ResultID %+v, want E_BADTYPE for a ReqType outside the XSD namespace", id)
	}
}

// --- M9: Browse has a size ceiling ---

// TestBrowse_ClampsToMaxBrowseElements pins the fix for Browse having
// been the one operation with no size limit at all: MaxElementsReturned=0
// means "no limit" on the wire, the whole response is assembled in memory
// before anything is written, and a backend that ignored the limit was
// simply trusted.
func TestBrowse_ClampsToMaxBrowseElements(t *testing.T) {
	var elements []backend.BrowseElement
	for range 50 {
		elements = append(elements, backend.BrowseElement{
			Name: "Item", IsItem: true, Ref: &backend.ItemRef{ItemName: "Item"},
		})
	}
	be, _, _ := newMinimalBackend()
	// The backend deliberately ignores MaxElementsReturned and returns
	// everything, which is what the server must now defend against.
	be.Browser = &testBrowser{result: backend.BrowseResult{Elements: elements}}
	h := newTestHandler(t, be, Config{MaxBrowseElements: 10}, nil)

	got := decodeResponse[xmlda.BrowseResponse](t, postSOAP(t, h, browseRequestBody()))
	if len(got.Elements) != 10 {
		t.Fatalf("got %d elements, want them clamped to MaxBrowseElements=10", len(got.Elements))
	}
	if !got.MoreElements {
		t.Error("a truncated result must report MoreElements=true so the client knows to page")
	}
}

// --- M6: the server-wide subscribed-item budget ---

// TestSubscribe_TotalItemBudget pins the fix for the per-axis limits
// multiplying: MaxConcurrentSubscriptions and MaxItemsPerSubscription
// together permitted a live item count neither limit alone suggests, with
// every item holding its own last sample.
func TestSubscribe_TotalItemBudget(t *testing.T) {
	be, _, reader := newMinimalBackend()
	for _, n := range []string{"A", "B", "C"} {
		reader.Set(backend.ItemRef{ItemName: n}, xmlda.NewInt32(1))
	}
	h := newTestHandler(t, be, Config{MaxTotalSubscribedItems: 4}, nil)

	body := func(handle string) string {
		return soapEnvelopeOpen + `<Subscribe xmlns="` + xmlda.Namespace + `" ReturnValuesOnReply="false">` +
			`<Options ClientRequestHandle="` + handle + `"/><ItemList>` +
			`<Items ItemName="A"/><Items ItemName="B"/><Items ItemName="C"/>` +
			`</ItemList></Subscribe>` + soapEnvelopeClose
	}

	// First subscription: 3 items, within the budget of 4.
	first := decodeResponse[xmlda.SubscribeResponse](t, postSOAP(t, h, body("CRH1")))
	if first.ServerSubHandle == "" {
		t.Fatal("first Subscribe was rejected but should fit the budget")
	}

	// Second: 3 more would make 6, over the budget — rejected as a
	// whole-operation fault, not silently accepted.
	resp := postSOAP(t, h, body("CRH2"))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got HTTP %d, want a fault once the total item budget is exceeded", resp.StatusCode)
	}
	body2, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body2), "E_OUTOFMEMORY") {
		t.Fatalf("want an E_OUTOFMEMORY fault, got:\n%s", body2)
	}
}

// --- M3: error text is locale-aware and covers vendor codes ---

// TestErrorText_HookIsUsed pins the fix for Errors text having been
// hardwired to xmlda.StandardErrorText: the response reported
// RevisedLocaleID for a locale the backend claimed to support and then
// sent English text regardless, and a vendor result code got no <Text>
// element at all (StandardErrorText returns "" for codes it does not
// know, though §3.1.9 says every OPCError carries one).
func TestErrorText_HookIsUsed(t *testing.T) {
	be, status, _ := newMinimalBackend()
	status.SetLocales([]string{"en-US", "de-DE"})
	var gotLocale string
	h := newTestHandler(t, be, Config{
		ErrorText: func(code xmlda.ErrorCode, locale string) string {
			gotLocale = locale
			if code == xmlda.ErrUnknownItemName {
				return "Der angeforderte Elementname ist dem Server nicht bekannt."
			}
			return xmlda.StandardErrorText(code)
		},
	}, nil)

	body := soapEnvelopeOpen + `<Read xmlns="` + xmlda.Namespace + `">` +
		`<Options ClientRequestHandle="CRH1" LocaleID="de-DE"/>` +
		`<ItemList><Items ItemName="NoSuchItem"/></ItemList></Read>` + soapEnvelopeClose
	got := decodeResponse[xmlda.ReadResponse](t, postSOAP(t, h, body))

	if gotLocale != "de-DE" {
		t.Errorf("ErrorText was called with locale %q, want the resolved de-DE", gotLocale)
	}
	if len(got.Errors) != 1 {
		t.Fatalf("got %d Errors entries, want 1: %+v", len(got.Errors), got.Errors)
	}
	if got.Errors[0].Text != "Der angeforderte Elementname ist dem Server nicht bekannt." {
		t.Errorf("got text %q, want the locale-specific text from Config.ErrorText", got.Errors[0].Text)
	}
	if got.Result.RevisedLocaleID != "de-DE" {
		t.Errorf("got RevisedLocaleID %q, want de-DE", got.Result.RevisedLocaleID)
	}
}

// --- M7: the pre-dispatch status fetch is memoized ---

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

// --- N7: xsd:int wire fields accept negatives instead of faulting ---

// TestSignedWireFields_NegativeValuesAreNormalized pins the fix for
// WaitTime, SubscriptionPingRate, RequestedSamplingRate, MaxAge and
// MaxElementsReturned having been modeled as unsigned. They are xsd:int
// in the schema this library now ships, so a negative value is valid
// input a schema-checking client may legitimately send — and it used to
// be rejected with a whole-operation parse fault
// ("strconv.ParseUint: parsing \"-1\"").
func TestSignedWireFields_NegativeValuesAreNormalized(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	be.Browser = &testBrowser{}
	h := newTestHandler(t, be, Config{}, nil)

	cases := []struct {
		name, body string
	}{
		{"Read MaxAge", soapEnvelopeOpen + `<Read xmlns="` + xmlda.Namespace + `">` +
			`<Options ClientRequestHandle="CRH1"/><ItemList MaxAge="-1">` +
			`<Items ItemName="Item1"/></ItemList></Read>` + soapEnvelopeClose},
		{"Subscribe SubscriptionPingRate + RequestedSamplingRate",
			soapEnvelopeOpen + `<Subscribe xmlns="` + xmlda.Namespace + `" ` +
				`ReturnValuesOnReply="true" SubscriptionPingRate="-1">` +
				`<Options ClientRequestHandle="CRH1"/>` +
				`<ItemList RequestedSamplingRate="-5"><Items ItemName="Item1"/></ItemList>` +
				`</Subscribe>` + soapEnvelopeClose},
		{"Browse MaxElementsReturned", soapEnvelopeOpen +
			`<Browse xmlns="` + xmlda.Namespace + `" MaxElementsReturned="-1"/>` + soapEnvelopeClose},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postSOAP(t, h, tc.body)
			body, _ := io.ReadAll(resp.Body)
			if strings.Contains(string(body), "ParseUint") || strings.Contains(string(body), "ParseInt") {
				t.Fatalf("a negative xsd:int was rejected as a parse fault:\n%s", body)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("got HTTP %d, want 200 for schema-valid input:\n%s", resp.StatusCode, body)
			}
		})
	}
}

// TestPolledRefresh_NegativeWaitTime is the same for WaitTime, which
// needs a live subscription to reach.
func TestPolledRefresh_NegativeWaitTime(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{}, nil)

	sub := decodeResponse[xmlda.SubscribeResponse](t,
		postSOAP(t, h, subscribeRequestBody("Item1", "CIH1", false)))
	if sub.ServerSubHandle == "" {
		t.Fatal("setup: no subscription handle")
	}
	resp := postSOAP(t, h, soapEnvelopeOpen+
		`<SubscriptionPolledRefresh xmlns="`+xmlda.Namespace+`" WaitTime="-1">`+
		`<Options ClientRequestHandle="CRH1"/>`+
		`<ServerSubHandles>`+sub.ServerSubHandle+`</ServerSubHandles>`+
		`</SubscriptionPolledRefresh>`+soapEnvelopeClose)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got HTTP %d, want 200 for a negative but schema-valid WaitTime:\n%s", resp.StatusCode, body)
	}
}

// --- N1: backend latency is actually observed ---

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

// TestObserveBackendLatency_IsCalled pins the fix for
// telemetry.Metrics.ObserveBackendLatency having been declared and never
// invoked — every implementation had to stub a method that produced
// nothing.
func TestObserveBackendLatency_IsCalled(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	be.Writer = &testWriter{reader: reader}
	be.Browser = &testBrowser{}
	be.Properties = &testProperties{props: map[backend.ItemRef][]backend.Property{
		{ItemName: "Item1"}: {{ID: xmlda.PropDescription, Value: xmlda.NewString("d")}},
	}}
	m := &recordingMetrics{Metrics: telemetry.NoopMetrics()}
	h, err := New(Deps{Backend: be, Metrics: m}, Config{StatusCacheTTL: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Shutdown(context.Background()) })

	postSOAP(t, h, readRequestBody([]string{"Item1"}))
	postSOAP(t, h, writeRequestBody("Item1", "int", "5", false))
	postSOAP(t, h, browseRequestBody())
	postSOAP(t, h, getPropertiesRequestBody("Item1"))
	postSOAP(t, h, getStatusRequestBody("CRH1"))

	seen := m.seen()
	for _, want := range []string{"Read", "Write", "Browse", "GetProperties", "GetStatus"} {
		found := false
		for _, got := range seen {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no ObserveBackendLatency for %q; observed %v", want, seen)
		}
	}
}

// --- N11: the request-level ItemPath is echoed back ---

// TestGetProperties_EchoesRequestLevelItemPath pins the fix for
// PropertyReplyList having echoed only a per-item ItemPath. A client that
// set the path once for the whole request (§3.1.1's hierarchical
// parameters, which the server already honored when resolving the item)
// got its items back unqualified.
func TestGetProperties_EchoesRequestLevelItemPath(t *testing.T) {
	be, _, _ := newMinimalBackend()
	be.Properties = &testProperties{props: map[backend.ItemRef][]backend.Property{
		{ItemPath: "Plant/Line1", ItemName: "Item1"}: {
			{ID: xmlda.PropDescription, Value: xmlda.NewString("d")},
		},
	}}
	h := newTestHandler(t, be, Config{}, nil)

	got := decodeResponse[xmlda.GetPropertiesResponse](t, postSOAP(t, h, soapEnvelopeOpen+
		`<GetProperties xmlns="`+xmlda.Namespace+`" ItemPath="Plant/Line1" ReturnAllProperties="true">`+
		`<ItemIDs ItemName="Item1"/></GetProperties>`+soapEnvelopeClose))

	if len(got.PropertyLists) != 1 {
		t.Fatalf("got %d property lists, want 1", len(got.PropertyLists))
	}
	l := got.PropertyLists[0]
	if !l.ResultID.IsZero() {
		t.Fatalf("item not resolved: %+v — the request-level ItemPath was not applied", l.ResultID)
	}
	if l.ItemPath == nil || *l.ItemPath != "Plant/Line1" {
		t.Fatalf("got ItemPath %v, want the request-level Plant/Line1 echoed back", l.ItemPath)
	}
}
