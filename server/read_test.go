package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// shortResultReader always returns one fewer Result than requested,
// modeling a non-conforming backend.Reader. The items it does cover get
// a real value, not a zero-value placeholder — this test's only concern
// is the missing tail, not incidentally exercising a separately-invalid
// "successful but empty" Result.
type shortResultReader struct{}

func (shortResultReader) Read(ctx context.Context, items []backend.ReadRequestItem) ([]backend.Result[backend.ItemSample], error) {
	if len(items) == 0 {
		return nil, nil
	}
	out := make([]backend.Result[backend.ItemSample], len(items)-1)
	for i := range out {
		out[i] = backend.Result[backend.ItemSample]{Value: backend.ItemSample{Value: xmlda.NewInt32(int32(i)), Quality: xmlda.NewGoodQuality(), Timestamp: testEpoch}}
	}
	return out, nil
}

// TestHandleRead_ShortBackendResultSlice_NoPanic reproduces a backend
// that violates the "exactly one Result per requested item" contract
// (docs/backend-implementation.md). Must resolve the missing tail to
// E_FAIL rather than panicking with an out-of-range index.
func TestHandleRead_ShortBackendResultSlice_NoPanic(t *testing.T) {
	be := backend.Backend{Status: newTestStatus(), Reader: shortResultReader{}}
	h := newTestHandler(t, be, Config{}, clock.Real{})

	resp := postSOAP(t, h, readRequestBody([]string{"Item1", "Item2"}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200 (no panic)", resp.StatusCode)
	}
	got := decodeResponse[xmlda.ReadResponse](t, resp)
	if len(got.RItemList.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(got.RItemList.Items))
	}
	if got.RItemList.Items[1].ResultID != xmlda.ErrFail {
		t.Fatalf("got %+v for the item the backend didn't return a Result for, want E_FAIL", got.RItemList.Items[1])
	}
}

func TestHandleRead_RoundTrip(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(42))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	resp := postSOAP(t, h, readRequestBody([]string{"Item1"}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	got := decodeResponse[xmlda.ReadResponse](t, resp)
	if len(got.RItemList.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(got.RItemList.Items))
	}
	item := got.RItemList.Items[0]
	if item.Value == nil {
		t.Fatalf("expected a non-nil Value")
	}
	i32, err := item.Value.Int32()
	if err != nil || i32 != 42 {
		t.Fatalf("got (%d, %v), want (42, nil)", i32, err)
	}
	if !item.ResultID.IsZero() {
		t.Fatalf("expected no error for a valid item, got %+v", item.ResultID)
	}
}

func TestHandleRead_OrderPreservedAcrossMultipleItems(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "A"}, xmlda.NewInt32(1))
	reader.Set(backend.ItemRef{ItemName: "B"}, xmlda.NewInt32(2))
	reader.Set(backend.ItemRef{ItemName: "C"}, xmlda.NewInt32(3))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	resp := postSOAP(t, h, readRequestBody([]string{"A", "B", "C"}))
	got := decodeResponse[xmlda.ReadResponse](t, resp)
	if len(got.RItemList.Items) != 3 {
		t.Fatalf("got %d items, want 3", len(got.RItemList.Items))
	}
	for i, want := range []int32{1, 2, 3} {
		v, err := got.RItemList.Items[i].Value.Int32()
		if err != nil || v != want {
			t.Fatalf("item %d: got (%d, %v), want (%d, nil)", i, v, err, want)
		}
	}
}

func TestHandleRead_PartialSuccess(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Good"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	resp := postSOAP(t, h, readRequestBody([]string{"Good", "Bad"}))
	got := decodeResponse[xmlda.ReadResponse](t, resp)
	if len(got.RItemList.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(got.RItemList.Items))
	}
	if !got.RItemList.Items[0].ResultID.IsZero() {
		t.Fatalf("item 0 (Good) should have no error, got %+v", got.RItemList.Items[0].ResultID)
	}
	if got.RItemList.Items[1].ResultID != xmlda.ErrUnknownItemName {
		t.Fatalf("item 1 (Bad): got %+v, want E_UNKNOWNITEMNAME", got.RItemList.Items[1].ResultID)
	}
	if len(got.Errors) != 1 || got.Errors[0].ID != xmlda.ErrUnknownItemName {
		t.Fatalf("got Errors=%+v", got.Errors)
	}
}

// TestHandleRead_EmptyItemListIsAnEmptySuccess pins that an empty item
// list is served, not refused. Both <ItemList> and its <Items> are
// minOccurs="0" in the schema, and §3.3.1 only goes as far as "It is
// expected that there are one or more Items per ItemList" — expectation,
// not requirement. Faulting invented a rule the schema does not state,
// and a client assembling its list dynamically hits it for an entirely
// ordinary reason.
func TestHandleRead_EmptyItemListIsAnEmptySuccess(t *testing.T) {
	be, _, _ := newRWBackend(t)
	h := newTestHandler(t, be, Config{}, clock.Real{})

	body := soapEnvelopeOpen + `<Read xmlns="` + xmlda.Namespace + `"><ItemList/></Read>` + soapEnvelopeClose
	resp := postSOAP(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	out := decodeResponseFrom[xmlda.ReadResponse](t, readBody(t, resp))
	if len(out.RItemList.Items) != 0 {
		t.Errorf("got %d items for an empty request, want 0", len(out.RItemList.Items))
	}
	if len(out.Errors) != 0 {
		t.Errorf("an empty request produced Errors entries: %+v", out.Errors)
	}
}

func TestHandleRead_ItemCountLimit(t *testing.T) {
	be, _, reader := newMinimalBackend()
	names := make([]string, 5)
	for i := range names {
		names[i] = "Item"
		reader.Set(backend.ItemRef{ItemName: "Item"}, xmlda.NewInt32(1))
	}
	h := newTestHandler(t, be, Config{MaxItemsPerRequest: 3}, clock.Real{})

	resp := postSOAP(t, h, readRequestBody(names))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500", resp.StatusCode)
	}
	f := decodeFault(t, resp)
	if f == nil || f.Code.Local != "E_OUTOFMEMORY" {
		t.Fatalf("got %+v, want E_OUTOFMEMORY", f)
	}
}

func TestHandleRead_ReturnItemNameGating(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	// readRequestBody's Options never sets ReturnItemName, so it must
	// default to false and the response must not echo ItemName.
	resp := postSOAP(t, h, readRequestBody([]string{"Item1"}))
	got := decodeResponse[xmlda.ReadResponse](t, resp)
	if got.RItemList.Items[0].ItemName != "" {
		t.Fatalf("expected ItemName to be omitted by default, got %q", got.RItemList.Items[0].ItemName)
	}
}

// --- one bad item costs that item, not the request ---

// TestHandleRead_MalformedItemIsPerItemCondition pins the end-to-end shape:
// three readable items and one with a malformed
// MaxAge used to produce a SOAP fault carrying nothing about any of them.
// Now the good items return their values and the bad one returns its own
// ResultID, which is what §2.6/§3.1.9's per-item Errors model is for.
func TestHandleRead_MalformedItemIsPerItemCondition(t *testing.T) {
	be, _, r := newRWBackend(t)
	for _, n := range []string{"ok1", "ok2", "ok3"} {
		r.Set(backend.ItemRef{ItemName: n}, xmlda.NewInt32(1))
	}
	h := newTestHandler(t, be, Config{}, clock.Real{})

	body := soapEnvelopeOpen + `<Read xmlns="` + xmlda.Namespace + `">` +
		`<Options ReturnItemName="true" ReturnDiagnosticInfo="true"/><ItemList>` +
		`<Items ItemName="ok1" ClientItemHandle="H1"/>` +
		`<Items ItemName="ok2" ClientItemHandle="H2" MaxAge="not-a-number"/>` +
		`<Items ItemName="ok3" ClientItemHandle="H3"/>` +
		`</ItemList></Read>` + soapEnvelopeClose

	resp := postSOAP(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200 — one malformed item still faults the whole Read", resp.StatusCode)
	}
	out := decodeResponse[xmlda.ReadResponse](t, resp)
	items := out.RItemList.Items
	if len(items) != 3 {
		t.Fatalf("got %d reply items, want one per request item", len(items))
	}

	for _, handle := range []string{"H1", "H3"} {
		iv := itemByHandle(t, items, handle)
		if !iv.ResultID.IsZero() {
			t.Errorf("%s: ResultID = %v, want none", handle, iv.ResultID)
		}
		if iv.Value == nil {
			t.Errorf("%s: value was dropped", handle)
		}
	}

	bad := itemByHandle(t, items, "H2")
	if bad.ResultID != xmlda.ErrFail {
		t.Errorf("H2: ResultID = %v, want E_FAIL", bad.ResultID)
	}
	if bad.Value != nil {
		t.Error("H2: a rejected item carries a value")
	}
	// The client must be able to tell WHICH field it got wrong: the
	// deduplicated Errors entry carries the code but not the item.
	if bad.DiagnosticInfo == nil {
		t.Fatal("H2: no DiagnosticInfo element at all, although the client asked for one")
	}
	if !strings.Contains(*bad.DiagnosticInfo, "MaxAge") {
		t.Errorf("H2: DiagnosticInfo does not name the field: %q", *bad.DiagnosticInfo)
	}
	if len(out.Errors) != 1 || out.Errors[0].ID != xmlda.ErrFail {
		t.Errorf("Errors = %+v, want a single E_FAIL entry", out.Errors)
	}
}

// --- a failing item must not claim good quality ---

// TestHandleRead_FailedItemReportsBadQuality pins the wire shape. The
// zero OPCQuality emits no attributes, and under the schema's own
// defaults (QualityField="good") that reads as good quality — so an item
// reporting E_UNKNOWNITEMNAME must not carry one, since for a client
// bridging this onto OPC DA's wQuality the quality is the half that
// reaches the process image. Omitting the element entirely has the same
// failure mode one step removed: the schema default applies to the
// missing element too. The specification resolves it by stating the
// quality outright — §2.6 p.22 shows
// <Items ResultID="E_UNKNOWNITEMNAME"><Quality QualityField="bad"/></Items>
// — which is what this asserts.
func TestHandleRead_FailedItemReportsBadQuality(t *testing.T) {
	be, _, r := newRWBackend(t)
	r.Set(backend.ItemRef{ItemName: "good"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	resp := postSOAP(t, h, readRequestBody([]string{"good", "missing"}))
	raw := readBody(t, resp)

	// Structural check on the decoded form...
	out := decodeResponseFrom[xmlda.ReadResponse](t, raw)
	items := out.RItemList.Items
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].Quality == nil {
		t.Error("a healthy item lost its Quality element")
	}
	if items[1].ResultID != xmlda.ErrUnknownItemName {
		t.Fatalf("items[1].ResultID = %v, want E_UNKNOWNITEMNAME", items[1].ResultID)
	}
	if items[1].Quality == nil {
		t.Fatal("a failing item carries no Quality at all; the schema default then reads as good")
	}
	if got := items[1].Quality.QualityField(); got != xmlda.QualityBad {
		t.Errorf("a failing item reports QualityField %q, want %q", got, xmlda.QualityBad)
	}

	// ...and on the bytes, since the defect was invisible in the decoded
	// form: Go's own decoder filled in the same zero value either way. The
	// failing item's quality must be spelled out, not left to the default.
	if n := strings.Count(raw, "<Quality"); n != 2 {
		t.Errorf("the response carries %d Quality elements, want 2 (one per item):\n%s", n, raw)
	}
	if !strings.Contains(raw, `QualityField="bad"`) {
		t.Errorf("the failing item's Quality does not spell out bad:\n%s", raw)
	}
}

// TestHandleRead_SuccessCodeKeepsValue pins the fix for haveSample having been
// computed as resultID.IsZero(), which dropped the value for every
// S_-prefixed code — the one class of result where the specification says
// the value is useful and the client needs both it and the code.
func TestHandleRead_SuccessCodeKeepsValue(t *testing.T) {
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

// --- ReqType's namespace is part of its identity ---

// TestHandleRead_ReqTypeFromForeignNamespaceIsBadType pins the fix for
// coerceToReqType having matched on the local name alone, which accepted
// e.g. "vendor:int" from any namespace and coerced it as if it were
// xsd:int — a type this server does not actually implement.
func TestHandleRead_ReqTypeFromForeignNamespaceIsBadType(t *testing.T) {
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
