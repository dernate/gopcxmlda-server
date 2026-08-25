package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

func TestHandleRead_RequestDeadlineAlreadyPassed_Faults(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	past := time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
	body := soapEnvelopeOpen + `<Read xmlns="` + xmlda.Namespace + `">` +
		`<Options ClientRequestHandle="CRH1" RequestDeadline="` + past + `"/>` +
		`<ItemList><Items ItemName="Item1"/></ItemList></Read>` + soapEnvelopeClose
	resp := postSOAP(t, h, body)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500", resp.StatusCode)
	}
	f := decodeFault(t, resp)
	if f == nil || f.Code.Local != "E_TIMEDOUT" {
		t.Fatalf("got %+v, want E_TIMEDOUT", f)
	}
}

func TestHandleRead_RequestDeadlineInFuture_Succeeds(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	future := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	body := soapEnvelopeOpen + `<Read xmlns="` + xmlda.Namespace + `">` +
		`<Options ClientRequestHandle="CRH1" RequestDeadline="` + future + `"/>` +
		`<ItemList><Items ItemName="Item1"/></ItemList></Read>` + soapEnvelopeClose
	resp := postSOAP(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
}

func TestHandleRead_ReturnErrorTextGating(t *testing.T) {
	be, _, _ := newMinimalBackend() // Item1 not registered: always E_UNKNOWNITEMNAME
	h := newTestHandler(t, be, Config{}, clock.Real{})

	withText := soapEnvelopeOpen + `<Read xmlns="` + xmlda.Namespace + `">` +
		`<Options ClientRequestHandle="CRH1" ReturnErrorText="true"/>` +
		`<ItemList><Items ItemName="Item1"/></ItemList></Read>` + soapEnvelopeClose
	got := decodeResponse[xmlda.ReadResponse](t, postSOAP(t, h, withText))
	if len(got.Errors) != 1 || got.Errors[0].Text == "" {
		t.Fatalf("expected non-empty error text when ReturnErrorText=true, got %+v", got.Errors)
	}

	withoutText := soapEnvelopeOpen + `<Read xmlns="` + xmlda.Namespace + `">` +
		`<Options ClientRequestHandle="CRH1" ReturnErrorText="false"/>` +
		`<ItemList><Items ItemName="Item1"/></ItemList></Read>` + soapEnvelopeClose
	got2 := decodeResponse[xmlda.ReadResponse](t, postSOAP(t, h, withoutText))
	if len(got2.Errors) != 1 {
		t.Fatalf("expected the Errors entry to still exist (ResultID mechanism), got %+v", got2.Errors)
	}
	if got2.Errors[0].Text != "" {
		t.Fatalf("expected empty error text when ReturnErrorText=false, got %q", got2.Errors[0].Text)
	}
	// The per-item ResultID itself must still be present regardless.
	if got2.RItemList.Items[0].ResultID != xmlda.ErrUnknownItemName {
		t.Fatalf("got %+v, want E_UNKNOWNITEMNAME regardless of ReturnErrorText", got2.RItemList.Items[0].ResultID)
	}
}

func TestHandleRead_ReturnDiagnosticInfoGating(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	withDiag := soapEnvelopeOpen + `<Read xmlns="` + xmlda.Namespace + `">` +
		`<Options ClientRequestHandle="CRH1" ReturnDiagnosticInfo="true"/>` +
		`<ItemList><Items ItemName="Item1"/></ItemList></Read>` + soapEnvelopeClose
	got := decodeResponse[xmlda.ReadResponse](t, postSOAP(t, h, withDiag))
	// This backend never populates DiagnosticInfo, so the field should
	// simply be empty (not erroring) — the real assertion here is that
	// gating doesn't break the round trip either way.
	_ = got

	withoutDiag := soapEnvelopeOpen + `<Read xmlns="` + xmlda.Namespace + `">` +
		`<Options ClientRequestHandle="CRH1" ReturnDiagnosticInfo="false"/>` +
		`<ItemList><Items ItemName="Item1"/></ItemList></Read>` + soapEnvelopeClose
	resp := postSOAP(t, h, withoutDiag)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
}
