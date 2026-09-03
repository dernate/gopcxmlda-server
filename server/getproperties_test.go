package server

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// shortResultProperties always returns one fewer Result than requested,
// modeling a non-conforming backend.PropertyReader.
type shortResultProperties struct{}

func (shortResultProperties) GetProperties(ctx context.Context, reqs []backend.PropertyRequest) ([]backend.Result[[]backend.Property], error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	return make([]backend.Result[[]backend.Property], len(reqs)-1), nil
}

// TestHandleGetProperties_ShortBackendResultSlice_NoPanic reproduces a
// backend that violates the "exactly one Result per requested item"
// contract (docs/backend-implementation.md). Must resolve the missing
// tail to E_FAIL rather than panicking with an out-of-range index.
func TestHandleGetProperties_ShortBackendResultSlice_NoPanic(t *testing.T) {
	be := backend.Backend{Status: newTestStatus(), Reader: newTestReader(), Properties: shortResultProperties{}}
	h := newTestHandler(t, be, Config{}, clock.Real{})

	body := soapEnvelopeOpen + `<GetProperties xmlns="` + xmlda.Namespace + `" ReturnAllProperties="true">` +
		`<ItemIDs ItemName="Item1"/><ItemIDs ItemName="Item2"/></GetProperties>` + soapEnvelopeClose
	resp := postSOAP(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200 (no panic)", resp.StatusCode)
	}
	got := decodeResponse[xmlda.GetPropertiesResponse](t, resp)
	if len(got.PropertyLists) != 2 {
		t.Fatalf("got %d PropertyLists, want 2", len(got.PropertyLists))
	}
	if got.PropertyLists[1].ResultID != xmlda.ErrFail {
		t.Fatalf("got %+v for the item the backend didn't return a Result for, want E_FAIL", got.PropertyLists[1])
	}
}

func TestHandleGetProperties_RoundTrip(t *testing.T) {
	status := newTestStatus()
	reader := newTestReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	props := &testProperties{props: map[backend.ItemRef][]backend.Property{
		ref: {{ID: xmlda.PropDescription, Value: xmlda.NewString("a description")}},
	}}
	be := backend.Backend{Status: status, Reader: reader, Properties: props}
	h := newTestHandler(t, be, Config{}, clock.Real{})

	resp := postSOAP(t, h, getPropertiesRequestBody("Item1"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	got := decodeResponse[xmlda.GetPropertiesResponse](t, resp)
	if len(got.PropertyLists) != 1 {
		t.Fatalf("got %d PropertyLists, want 1", len(got.PropertyLists))
	}
	if !got.PropertyLists[0].ResultID.IsZero() {
		t.Fatalf("expected no error, got %+v", got.PropertyLists[0].ResultID)
	}
	if len(got.PropertyLists[0].Properties) != 1 {
		t.Fatalf("got %d properties, want 1", len(got.PropertyLists[0].Properties))
	}
}

func TestHandleGetProperties_UnknownItem(t *testing.T) {
	status := newTestStatus()
	reader := newTestReader()
	props := &testProperties{props: map[backend.ItemRef][]backend.Property{}}
	be := backend.Backend{Status: status, Reader: reader, Properties: props}
	h := newTestHandler(t, be, Config{}, clock.Real{})

	got := decodeResponse[xmlda.GetPropertiesResponse](t, postSOAP(t, h, getPropertiesRequestBody("Unknown")))
	if got.PropertyLists[0].ResultID != xmlda.ErrUnknownItemName {
		t.Fatalf("got %+v, want E_UNKNOWNITEMNAME", got.PropertyLists[0].ResultID)
	}
}

// TestHandleGetProperties_ReturnErrorText_DefaultsTrue verifies
// GetProperties' error-text default matches every other operation's
// (Read/Write/Subscribe/SubscriptionPolledRefresh): omitting the
// ReturnErrorText attribute entirely must default to true, not false.
// Before this was fixed, GetPropertiesRequest.ReturnErrorText was a plain
// bool (Go zero value false) rather than a *bool, so "omitted" and
// "explicitly false" were indistinguishable and always resolved to no
// error text at all.
func TestHandleGetProperties_ReturnErrorText_DefaultsFalse(t *testing.T) {
	status := newTestStatus()
	reader := newTestReader()
	props := &testProperties{props: map[backend.ItemRef][]backend.Property{}} // Unknown item -> E_UNKNOWNITEMNAME
	be := backend.Backend{Status: status, Reader: reader, Properties: props}
	h := newTestHandler(t, be, Config{}, clock.Real{})

	// No ReturnErrorText attribute at all. The schema gives THIS element
	// default="false" — unlike RequestOptions, which is where the true
	// default lives — so an omitted attribute means no error text, and
	// therefore (§3.1.9) no Errors entries.
	body := soapEnvelopeOpen + `<GetProperties xmlns="` + xmlda.Namespace + `" ReturnAllProperties="true">` +
		`<ItemIDs ItemName="Unknown"/></GetProperties>` + soapEnvelopeClose
	got := decodeResponse[xmlda.GetPropertiesResponse](t, postSOAP(t, h, body))
	if len(got.Errors) != 0 {
		t.Fatalf("GetProperties without ReturnErrorText produced %d Errors entries; "+
			"the element's own schema default is false: %+v", len(got.Errors), got.Errors)
	}
	// The condition itself must still reach the client.
	if len(got.PropertyLists) != 1 || got.PropertyLists[0].ResultID != xmlda.ErrUnknownItemName {
		t.Fatalf("the per-item ResultID was lost: %+v", got.PropertyLists)
	}

	// Explicit true turns the list on.
	bodyTrue := soapEnvelopeOpen + `<GetProperties xmlns="` + xmlda.Namespace + `" ReturnAllProperties="true" ReturnErrorText="true">` +
		`<ItemIDs ItemName="Unknown"/></GetProperties>` + soapEnvelopeClose
	gotTrue := decodeResponse[xmlda.GetPropertiesResponse](t, postSOAP(t, h, bodyTrue))
	if len(gotTrue.Errors) != 1 || gotTrue.Errors[0].Text == "" {
		t.Fatalf("expected one Errors entry with text when ReturnErrorText=true, got %+v", gotTrue.Errors)
	}
}

// TestHandleGetProperties_UnknownPropertyName_InvalidPID reproduces the
// gap where a PropertyNames entry this server cannot resolve to a
// PropertyID was silently dropped from the response — a client asking for
// a property that does not exist here could not tell that apart from "it
// exists and has no value". It must instead be reported per-property as
// E_INVALIDPID.
func TestHandleGetProperties_UnknownPropertyName_InvalidPID(t *testing.T) {
	status := newTestStatus()
	reader := newTestReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	// The item itself is registered (so it resolves successfully) but with
	// zero real properties, isolating the E_INVALIDPID entry the server
	// appends for the unresolvable PropertyNames entry — testProperties,
	// unlike a real backend.PropertyReader, does not filter its returned
	// properties by the request's IDs/All at all.
	props := &testProperties{props: map[backend.ItemRef][]backend.Property{ref: {}}}
	be := backend.Backend{Status: status, Reader: reader, Properties: props}
	h := newTestHandler(t, be, Config{}, clock.Real{})

	body := soapEnvelopeOpen + `<GetProperties xmlns="` + xmlda.Namespace + `" ReturnAllProperties="false">` +
		`<ItemIDs ItemName="Item1"/>` +
		`<PropertyNames xmlns="">totallyUnknownProperty</PropertyNames>` +
		`</GetProperties>` + soapEnvelopeClose
	got := decodeResponse[xmlda.GetPropertiesResponse](t, postSOAP(t, h, body))

	if len(got.PropertyLists) != 1 {
		t.Fatalf("got %d PropertyLists, want 1", len(got.PropertyLists))
	}
	list := got.PropertyLists[0]
	if !list.ResultID.IsZero() {
		t.Fatalf("expected the item itself to resolve fine, got item-level %+v", list.ResultID)
	}
	if len(list.Properties) != 1 {
		t.Fatalf("got %d properties, want 1 (reporting the unresolvable name, not silently dropping it)", len(list.Properties))
	}
	if list.Properties[0].ResultID != xmlda.ErrInvalidPID {
		t.Fatalf("got %+v, want E_INVALIDPID for an unresolvable PropertyNames entry", list.Properties[0])
	}
}

// TestHandleGetProperties_UnknownPropertyName_IgnoredWhenReturnAllProperties
// is the regression-safety companion (REQ-PROPERTIES-001): PropertyNames
// is ignored entirely when ReturnAllProperties is set, so an unresolvable
// name in it must not surface as E_INVALIDPID in that case.
func TestHandleGetProperties_UnknownPropertyName_IgnoredWhenReturnAllProperties(t *testing.T) {
	status := newTestStatus()
	reader := newTestReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	props := &testProperties{props: map[backend.ItemRef][]backend.Property{
		ref: {{ID: xmlda.PropDescription, Value: xmlda.NewString("a description")}},
	}}
	be := backend.Backend{Status: status, Reader: reader, Properties: props}
	h := newTestHandler(t, be, Config{}, clock.Real{})

	body := soapEnvelopeOpen + `<GetProperties xmlns="` + xmlda.Namespace + `" ReturnAllProperties="true">` +
		`<ItemIDs ItemName="Item1"/>` +
		`<PropertyNames xmlns="">totallyUnknownProperty</PropertyNames>` +
		`</GetProperties>` + soapEnvelopeClose
	got := decodeResponse[xmlda.GetPropertiesResponse](t, postSOAP(t, h, body))

	if len(got.PropertyLists) != 1 || len(got.PropertyLists[0].Properties) != 1 {
		t.Fatalf("got %+v, want exactly the one real property, no E_INVALIDPID entry", got.PropertyLists)
	}
	for _, p := range got.PropertyLists[0].Properties {
		if p.ResultID == xmlda.ErrInvalidPID {
			t.Fatalf("got an E_INVALIDPID property entry while ReturnAllProperties=true, want PropertyNames ignored entirely")
		}
	}
}

func TestHandleGetProperties_NotSupportedWithoutPropertyReader(t *testing.T) {
	be, _, _ := newMinimalBackend() // no Properties configured
	h := newTestHandler(t, be, Config{}, clock.Real{})

	resp := postSOAP(t, h, getPropertiesRequestBody("Item1"))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500", resp.StatusCode)
	}
	f := decodeFault(t, resp)
	if f == nil || f.Code.Local != "E_NOTSUPPORTED" {
		t.Fatalf("got %+v, want E_NOTSUPPORTED", f)
	}
}

// TestHandleGetProperties_ValuelessPropertyDoesNotFailWholeResponse pins the
// fix for the defect where a single property without a value turned the
// entire GetProperties response into an E_FAIL SOAP fault: toItemProperty
// attached the zero Value unconditionally whenever ReturnPropertyValues
// was set, and a zero Value has no declared type, so the encode failed
// and writeResponse fell back to a blanket fault — discarding every other
// item's data over one missing property value.
func TestHandleGetProperties_ValuelessPropertyDoesNotFailWholeResponse(t *testing.T) {
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

// --- the request-level ItemPath is echoed back ---

// TestHandleGetProperties_EchoesRequestLevelItemPath pins the fix for
// PropertyReplyList having echoed only a per-item ItemPath. A client that
// set the path once for the whole request (§3.1.1's hierarchical
// parameters, which the server already honored when resolving the item)
// got its items back unqualified.
func TestHandleGetProperties_EchoesRequestLevelItemPath(t *testing.T) {
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
