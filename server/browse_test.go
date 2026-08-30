package server

import (
	"net/http"
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
