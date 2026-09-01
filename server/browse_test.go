package server

import (
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// TestToItemProperty_VendorProperty_NeverGetsStandardNamespace checks a
// vendor-defined property (no standard PropertyID) is never mislabeled
// with the OPC XML-DA namespace it didn't ask for — vendor-specific
// names/codes must live in the vendor's own namespace, never the OPC one
// (docs/specification/error-mapping.md's rule for ErrorCode applies
// equally here). Before this was fixed, toItemProperty always defaulted
// an unrecognized property to xmlda.Namespace regardless of what (if
// anything) the backend actually declared.
func TestToItemProperty_VendorProperty_NeverGetsStandardNamespace(t *testing.T) {
	// toItemProperty is a Handler method now (it logs a warning for a
	// vendor property with no namespace), so it needs a Handler.
	h := newTestHandler(t, backend.Backend{Status: newTestStatus(), Reader: newTestReader()}, Config{}, nil)

	// No Namespace set at all: must come back unqualified, not silently
	// promoted to the OPC XML-DA namespace.
	p := backend.Property{Name: "myVendorProp", Description: "a vendor property"}
	ip := h.toItemProperty(p, false)
	if ip.Name.Space == xmlda.Namespace {
		t.Fatalf("vendor property with no declared Namespace got the OPC XML-DA namespace: %+v", ip.Name)
	}
	if ip.Name.Space != "" || ip.Name.Local != "myVendorProp" {
		t.Fatalf("got %+v, want {Space: \"\", Local: \"myVendorProp\"}", ip.Name)
	}

	// An explicitly declared vendor namespace must be honored as-is.
	p2 := backend.Property{Name: "myVendorProp", Namespace: "http://example.com/vendor"}
	ip2 := h.toItemProperty(p2, false)
	if ip2.Name.Space != "http://example.com/vendor" || ip2.Name.Local != "myVendorProp" {
		t.Fatalf("got %+v, want the declared vendor namespace preserved", ip2.Name)
	}

	// A standard property (recognized PropertyID) must still resolve to
	// the OPC XML-DA namespace regardless of Namespace/Name.
	p3 := backend.Property{ID: xmlda.PropDescription}
	ip3 := h.toItemProperty(p3, false)
	if ip3.Name.Space != xmlda.Namespace {
		t.Fatalf("standard property: got %+v, want Space %q", ip3.Name, xmlda.Namespace)
	}
}

func TestHandleBrowse_RoundTrip(t *testing.T) {
	status := newTestStatus()
	reader := newTestReader()
	browser := &testBrowser{result: backend.BrowseResult{
		Elements: []backend.BrowseElement{
			{Name: "Item1", Ref: &backend.ItemRef{ItemName: "Item1"}, IsItem: true, HasChildren: false},
			{Name: "Branch1", IsItem: false, HasChildren: true},
		},
	}}
	be := backend.Backend{Status: status, Reader: reader, Browser: browser}
	h := newTestHandler(t, be, Config{}, clock.Real{})

	resp := postSOAP(t, h, browseRequestBody())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	got := decodeResponse[xmlda.BrowseResponse](t, resp)
	if len(got.Elements) != 2 {
		t.Fatalf("got %d elements, want 2", len(got.Elements))
	}
	if !got.Elements[0].IsItem || got.Elements[0].ItemName != "Item1" {
		t.Fatalf("element 0: got %+v", got.Elements[0])
	}
	if got.Elements[1].IsItem || !got.Elements[1].HasChildren {
		t.Fatalf("element 1: got %+v", got.Elements[1])
	}
}

func TestHandleBrowse_EmptyResultIsSuccess(t *testing.T) {
	status := newTestStatus()
	reader := newTestReader()
	browser := &testBrowser{result: backend.BrowseResult{}}
	be := backend.Backend{Status: status, Reader: reader, Browser: browser}
	h := newTestHandler(t, be, Config{}, clock.Real{})

	resp := postSOAP(t, h, browseRequestBody())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected a valid empty Browse result to succeed, got status %d", resp.StatusCode)
	}
	got := decodeResponse[xmlda.BrowseResponse](t, resp)
	if len(got.Elements) != 0 {
		t.Fatalf("got %d elements, want 0", len(got.Elements))
	}
}

func TestHandleBrowse_NotSupportedWithoutBrowser(t *testing.T) {
	be, _, _ := newMinimalBackend() // no Browser configured
	h := newTestHandler(t, be, Config{}, clock.Real{})

	resp := postSOAP(t, h, browseRequestBody())
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500", resp.StatusCode)
	}
	f := decodeFault(t, resp)
	if f == nil || f.Code.Local != "E_NOTSUPPORTED" {
		t.Fatalf("got %+v, want E_NOTSUPPORTED", f)
	}
}

func TestHandleBrowse_ContinuationPointMismatch(t *testing.T) {
	status := newTestStatus()
	reader := newTestReader()
	browser := &testBrowser{result: backend.BrowseResult{ContinuationPoint: "page2", MoreElements: true}}
	be := backend.Backend{Status: status, Reader: reader, Browser: browser}
	h := newTestHandler(t, be, Config{}, clock.Real{})

	// First call establishes a continuation token.
	got := decodeResponse[xmlda.BrowseResponse](t, postSOAP(t, h, browseRequestBody()))
	if got.ContinuationPoint == "" {
		t.Fatalf("expected a non-empty ContinuationPoint")
	}

	// Reusing the token with DIFFERENT filters must be rejected.
	body := soapEnvelopeOpen + `<Browse xmlns="` + xmlda.Namespace + `" ContinuationPoint="` + got.ContinuationPoint + `" ElementNameFilter="changed"/>` + soapEnvelopeClose
	resp := postSOAP(t, h, body)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500", resp.StatusCode)
	}
	f := decodeFault(t, resp)
	if f == nil || f.Code.Local != "E_INVALIDCONTINUATIONPOINT" {
		t.Fatalf("got %+v, want E_INVALIDCONTINUATIONPOINT", f)
	}
}

func TestHandleBrowse_ContinuationPointSameFilters(t *testing.T) {
	status := newTestStatus()
	reader := newTestReader()
	browser := &testBrowser{result: backend.BrowseResult{ContinuationPoint: "page2", MoreElements: true}}
	be := backend.Backend{Status: status, Reader: reader, Browser: browser}
	h := newTestHandler(t, be, Config{}, clock.Real{})

	got := decodeResponse[xmlda.BrowseResponse](t, postSOAP(t, h, browseRequestBody()))

	body := soapEnvelopeOpen + `<Browse xmlns="` + xmlda.Namespace + `" ContinuationPoint="` + got.ContinuationPoint + `"/>` + soapEnvelopeClose
	resp := postSOAP(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200 (same filters, valid continuation)", resp.StatusCode)
	}
}

// TestHandleBrowse_ContinuationPointPropertyNamesMismatch reproduces the
// gap where the continuation token's filter hash ignored PropertyNames:
// resuming a paged Browse with a DIFFERENT PropertyNames set changes the
// shape of the very result set the token indexes into, and must be
// rejected exactly like changing ElementNameFilter is.
func TestHandleBrowse_ContinuationPointPropertyNamesMismatch(t *testing.T) {
	status := newTestStatus()
	reader := newTestReader()
	browser := &testBrowser{result: backend.BrowseResult{ContinuationPoint: "page2", MoreElements: true}}
	be := backend.Backend{Status: status, Reader: reader, Browser: browser}
	h := newTestHandler(t, be, Config{}, clock.Real{})

	firstBody := soapEnvelopeOpen + `<Browse xmlns="` + xmlda.Namespace + `" ReturnAllProperties="true">` +
		`<PropertyNames xmlns="">description</PropertyNames></Browse>` + soapEnvelopeClose
	got := decodeResponse[xmlda.BrowseResponse](t, postSOAP(t, h, firstBody))
	if got.ContinuationPoint == "" {
		t.Fatalf("expected a non-empty ContinuationPoint")
	}

	mismatchBody := soapEnvelopeOpen + `<Browse xmlns="` + xmlda.Namespace + `" ReturnAllProperties="true" ContinuationPoint="` + got.ContinuationPoint + `">` +
		`<PropertyNames xmlns="">engineeringUnits</PropertyNames></Browse>` + soapEnvelopeClose
	resp := postSOAP(t, h, mismatchBody)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500", resp.StatusCode)
	}
	f := decodeFault(t, resp)
	if f == nil || f.Code.Local != "E_INVALIDCONTINUATIONPOINT" {
		t.Fatalf("got %+v, want E_INVALIDCONTINUATIONPOINT", f)
	}

	// The identical PropertyNames set, replayed, must still be accepted.
	sameBody := soapEnvelopeOpen + `<Browse xmlns="` + xmlda.Namespace + `" ReturnAllProperties="true" ContinuationPoint="` + got.ContinuationPoint + `">` +
		`<PropertyNames xmlns="">description</PropertyNames></Browse>` + soapEnvelopeClose
	resp2 := postSOAP(t, h, sameBody)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200 (identical PropertyNames, valid continuation)", resp2.StatusCode)
	}
}

// --- Browse reports its property conditions ---

// TestHandleBrowse_ReportsPropertyErrors pins the gap. BrowseResponse has an Errors
// list (§3.8.2) and this handler was the only one of the six that never
// filled it, so a property that failed to read arrived with a ResultID and
// no text, in a response a client reads as error-free.
func TestHandleBrowse_ReportsPropertyErrors(t *testing.T) {
	st, r := newTestStatus(), newTestReader()
	br := &testBrowser{result: backend.BrowseResult{Elements: []backend.BrowseElement{{
		Name: "Scalar", IsItem: true, Ref: &backend.ItemRef{ItemName: "Scalar"},
		Properties: []backend.Property{
			{ID: xmlda.PropDescription, Value: xmlda.NewString("ok")},
			{ID: xmlda.PropHighEU, ResultID: xmlda.ErrInvalidPID},
			{ID: xmlda.PropLowEU, ResultID: xmlda.ErrAccessDenied},
		},
	}}}}
	h := newTestHandler(t, backend.Backend{Status: st, Reader: r, Browser: br}, Config{}, clock.Real{})

	out := decodeResponse[xmlda.BrowseResponse](t, postSOAP(t, h, browseRequestBody()))
	got := map[xmlda.ErrorCode]string{}
	for _, e := range out.Errors {
		got[e.ID] = e.Text
	}
	for _, want := range []xmlda.ErrorCode{xmlda.ErrInvalidPID, xmlda.ErrAccessDenied} {
		text, ok := got[want]
		if !ok {
			t.Errorf("Errors has no entry for %v; the client sees a ResultID with no text", want.Local)
			continue
		}
		if text == "" {
			t.Errorf("%v: Errors entry carries no Text", want.Local)
		}
	}
	if len(out.Errors) != 2 {
		t.Errorf("got %d Errors entries, want 2 deduplicated codes: %+v", len(out.Errors), out.Errors)
	}
}

// TestHandleBrowse_ReturnErrorTextFalseSuppressesText pins that Browse honors
// its own ReturnErrorText attribute the way every other operation honors
// RequestOptions.ReturnErrorText: the codes still identify what happened,
// the human-readable text is dropped.
func TestHandleBrowse_ReturnErrorTextFalseSuppressesText(t *testing.T) {
	st, r := newTestStatus(), newTestReader()
	br := &testBrowser{result: backend.BrowseResult{Elements: []backend.BrowseElement{{
		Name: "Scalar", IsItem: true,
		Properties: []backend.Property{{ID: xmlda.PropHighEU, ResultID: xmlda.ErrInvalidPID}},
	}}}}
	h := newTestHandler(t, backend.Backend{Status: st, Reader: r, Browser: br}, Config{}, clock.Real{})

	out := decodeResponse[xmlda.BrowseResponse](t, postSOAP(t, h, browseBody(`ReturnErrorText="false"`)))
	if len(out.Errors) != 1 {
		t.Fatalf("got %d Errors entries, want 1", len(out.Errors))
	}
	if out.Errors[0].ID != xmlda.ErrInvalidPID {
		t.Errorf("ID = %v, want E_INVALIDPID", out.Errors[0].ID)
	}
	if out.Errors[0].Text != "" {
		t.Errorf("Text = %q, want empty with ReturnErrorText=false", out.Errors[0].Text)
	}
}

// --- BrowseFilter is validated and defaulted ---

// TestHandleBrowse_InvalidFilterIsRejected pins the server half: E_INVALIDFILTER
// was the one standard code in the package the server never emitted, and
// an unrecognized filter was forwarded to a backend with no vocabulary to
// say the request made no sense.
func TestHandleBrowse_InvalidFilterIsRejected(t *testing.T) {
	st, r := newTestStatus(), newTestReader()
	br := &testBrowser{}
	h := newTestHandler(t, backend.Backend{Status: st, Reader: r, Browser: br}, Config{}, clock.Real{})

	resp := postSOAP(t, h, browseBody(`BrowseFilter="junk"`))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("an invalid BrowseFilter was accepted (status %d)", resp.StatusCode)
	}
	if f := decodeFault(t, resp); f == nil || f.Code.Local != "E_INVALIDFILTER" {
		t.Fatalf("got %+v, want E_INVALIDFILTER", f)
	}
}

// TestHandleBrowse_AbsentFilterReachesBackendAsAll pins the other half: the
// schema gives BrowseFilter default="all", so a backend must never have to
// guess what "" means.
func TestHandleBrowse_AbsentFilterReachesBackendAsAll(t *testing.T) {
	st, r := newTestStatus(), newTestReader()
	br := &filterRecordingBrowser{}
	h := newTestHandler(t, backend.Backend{Status: st, Reader: r, Browser: br}, Config{}, clock.Real{})

	postSOAP(t, h, browseRequestBody())
	if got := br.Filter(); got != xmlda.BrowseFilterAll {
		t.Errorf("the backend saw BrowseFilter %q, want the schema default %q", got, xmlda.BrowseFilterAll)
	}
}

type filterRecordingBrowser struct {
	mu     sync.Mutex
	filter xmlda.BrowseFilter
}

func (b *filterRecordingBrowser) Filter() xmlda.BrowseFilter {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.filter
}

// TestHandleBrowse_ValuelessPropertyDoesNotFailWholeResponse is the same defect
// on the Browse path, which shares toItemProperty.
func TestHandleBrowse_ValuelessPropertyDoesNotFailWholeResponse(t *testing.T) {
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

// --- Browse has a size ceiling ---

// TestHandleBrowse_ClampsToMaxBrowseElements pins the fix for Browse having
// been the one operation with no size limit at all: MaxElementsReturned=0
// means "no limit" on the wire, the whole response is assembled in memory
// before anything is written, and a backend that ignored the limit was
// simply trusted.
func TestHandleBrowse_ClampsToMaxBrowseElements(t *testing.T) {
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
