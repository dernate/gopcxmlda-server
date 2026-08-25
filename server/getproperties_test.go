package server

import (
	"context"
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
func TestHandleGetProperties_ReturnErrorText_DefaultsTrue(t *testing.T) {
	status := newTestStatus()
	reader := newTestReader()
	props := &testProperties{props: map[backend.ItemRef][]backend.Property{}} // Unknown item -> E_UNKNOWNITEMNAME
	be := backend.Backend{Status: status, Reader: reader, Properties: props}
	h := newTestHandler(t, be, Config{}, clock.Real{})

	// No ReturnErrorText attribute at all.
	body := soapEnvelopeOpen + `<GetProperties xmlns="` + xmlda.Namespace + `" ReturnAllProperties="true">` +
		`<ItemIDs ItemName="Unknown"/></GetProperties>` + soapEnvelopeClose
	got := decodeResponse[xmlda.GetPropertiesResponse](t, postSOAP(t, h, body))
	if len(got.Errors) != 1 || got.Errors[0].Text == "" {
		t.Fatalf("expected non-empty error text by default (ReturnErrorText omitted), got %+v", got.Errors)
	}

	// Explicit false must still suppress it.
	bodyFalse := soapEnvelopeOpen + `<GetProperties xmlns="` + xmlda.Namespace + `" ReturnAllProperties="true" ReturnErrorText="false">` +
		`<ItemIDs ItemName="Unknown"/></GetProperties>` + soapEnvelopeClose
	gotFalse := decodeResponse[xmlda.GetPropertiesResponse](t, postSOAP(t, h, bodyFalse))
	if len(gotFalse.Errors) != 1 || gotFalse.Errors[0].Text != "" {
		t.Fatalf("expected empty error text when ReturnErrorText=false, got %+v", gotFalse.Errors)
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
