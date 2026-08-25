package server

import (
	"net/http"
	"testing"

	"github.com/dernate/gopcxmlda-server/clock"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

func TestHandleGetStatus_RoundTrip(t *testing.T) {
	be, status, _ := newMinimalBackend()
	_ = status
	h := newTestHandler(t, be, Config{}, clock.Real{})

	resp := postSOAP(t, h, getStatusRequestBody("CRH1"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	got := decodeResponse[xmlda.GetStatusResponse](t, resp)
	if got.Result.ClientRequestHandle != "CRH1" {
		t.Fatalf("got ClientRequestHandle=%q, want CRH1", got.Result.ClientRequestHandle)
	}
	if got.Result.ServerState != xmlda.ServerStateRunning {
		t.Fatalf("got ServerState=%q, want running", got.Result.ServerState)
	}
	if !got.Status.StartTime.Equal(testEpoch) {
		t.Fatalf("got StartTime=%v, want %v", got.Status.StartTime, testEpoch)
	}
	if len(got.Status.SupportedLocaleIDs) == 0 {
		t.Fatalf("expected at least one SupportedLocaleIDs entry (REQ-STATUS-004)")
	}
	if len(got.Status.SupportedInterfaceVersions) == 0 {
		t.Fatalf("expected at least one SupportedInterfaceVersions entry (REQ-STATUS-005)")
	}
}

func TestHandleGetStatus_StartTimeConstantAcrossCalls(t *testing.T) {
	be, _, _ := newMinimalBackend()
	h := newTestHandler(t, be, Config{}, clock.Real{})

	got1 := decodeResponse[xmlda.GetStatusResponse](t, postSOAP(t, h, getStatusRequestBody("CRH1")))
	got2 := decodeResponse[xmlda.GetStatusResponse](t, postSOAP(t, h, getStatusRequestBody("CRH2")))
	if !got1.Status.StartTime.Equal(got2.Status.StartTime) {
		t.Fatalf("StartTime changed across calls: %v vs %v", got1.Status.StartTime, got2.Status.StartTime)
	}
}
