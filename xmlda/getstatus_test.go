package xmlda

import (
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/soap"
)

func TestGetStatusRequest_RoundTrip(t *testing.T) {
	req := GetStatusRequest{LocaleID: "en-US", ClientRequestHandle: "CRH1"}
	out, err := xmlMarshalNamed(t, "GetStatus", req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got GetStatusRequest
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.LocaleID != req.LocaleID || got.ClientRequestHandle != req.ClientRequestHandle {
		t.Fatalf("got %+v, want %+v", got, req)
	}
}

func TestGetStatusResponse_RoundTrip(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	resp := GetStatusResponse{
		Result: ReplyBase{
			RcvTime:     start,
			ReplyTime:   start,
			ServerState: ServerStateRunning,
		},
		Status: Status{
			StartTime:                  start,
			ProductVersion:             "1.0.0",
			SupportedLocaleIDs:         []string{"en-US"},
			SupportedInterfaceVersions: []string{InterfaceVersion10},
		},
	}
	out, err := xmlMarshalNamed(t, "GetStatusResponse", resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got GetStatusResponse
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v\ndoc: %s", err, out)
	}
	if !got.Status.StartTime.Equal(start) {
		t.Fatalf("StartTime: got %v, want %v", got.Status.StartTime, start)
	}
	if got.Status.ProductVersion != "1.0.0" {
		t.Fatalf("got ProductVersion=%q", got.Status.ProductVersion)
	}
	if len(got.Status.SupportedLocaleIDs) != 1 || got.Status.SupportedLocaleIDs[0] != "en-US" {
		t.Fatalf("got SupportedLocaleIDs=%v", got.Status.SupportedLocaleIDs)
	}
	if len(got.Status.SupportedInterfaceVersions) != 1 || got.Status.SupportedInterfaceVersions[0] != InterfaceVersion10 {
		t.Fatalf("got SupportedInterfaceVersions=%v", got.Status.SupportedInterfaceVersions)
	}
	if got.Result.ServerState != ServerStateRunning {
		t.Fatalf("got ServerState=%q", got.Result.ServerState)
	}
}

// TestGetStatusRequest_RealFixture decodes the real captured request
// testdata/requests/getstatus_632.request.xml (REQ-STATUS-001).
func TestGetStatusRequest_RealFixture(t *testing.T) {
	doc := readTestdata(t, "testdata", "requests", "getstatus_632.request.xml")
	var env soap.Envelope[GetStatusRequest]
	if err := Decode(doc, &env); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if env.Body.Fault != nil {
		t.Fatalf("unexpected fault: %+v", env.Body.Fault)
	}
	req := env.Body.Content
	if req == nil {
		t.Fatalf("expected non-nil request content")
	}
	if req.LocaleID != "en" {
		t.Fatalf("got LocaleID=%q, want en", req.LocaleID)
	}
	if req.ClientRequestHandle != "TestClient" {
		t.Fatalf("got ClientRequestHandle=%q, want TestClient", req.ClientRequestHandle)
	}
}

// TestGetStatusResponse_RealFixture decodes the real captured response
// testdata/responses/getstatus_639.response.xml (REQ-STATUS-002,
// REQ-STATUS-003, REQ-STATUS-004, REQ-STATUS-005).
func TestGetStatusResponse_RealFixture(t *testing.T) {
	doc := readTestdata(t, "testdata", "responses", "getstatus_639.response.xml")
	var env soap.Envelope[GetStatusResponse]
	if err := Decode(doc, &env); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if env.Body.Fault != nil {
		t.Fatalf("unexpected fault: %+v", env.Body.Fault)
	}
	resp := env.Body.Content
	if resp == nil {
		t.Fatalf("expected non-nil response content")
	}
	if resp.Result.ClientRequestHandle != "TestClient" {
		t.Fatalf("got ClientRequestHandle=%q, want TestClient", resp.Result.ClientRequestHandle)
	}
	if resp.Result.RevisedLocaleID != "en-us" {
		t.Fatalf("got RevisedLocaleID=%q, want en-us", resp.Result.RevisedLocaleID)
	}
	if resp.Result.ServerState != ServerStateRunning {
		t.Fatalf("got ServerState=%q, want running", resp.Result.ServerState)
	}
	wantStart := time.Date(2026, 7, 23, 17, 43, 54, 890000000, time.UTC)
	if !resp.Status.StartTime.Equal(wantStart) {
		t.Fatalf("got StartTime=%v, want %v", resp.Status.StartTime, wantStart)
	}
	if resp.Status.ProductVersion != "V1.00" {
		t.Fatalf("got ProductVersion=%q, want V1.00", resp.Status.ProductVersion)
	}
	if resp.Status.VendorInfo != "Example OPC XML-DA Server" {
		t.Fatalf("got VendorInfo=%q", resp.Status.VendorInfo)
	}
	if len(resp.Status.SupportedLocaleIDs) != 1 || resp.Status.SupportedLocaleIDs[0] != "en-us" {
		t.Fatalf("got SupportedLocaleIDs=%v, want [en-us]", resp.Status.SupportedLocaleIDs)
	}
	if len(resp.Status.SupportedInterfaceVersions) != 1 || resp.Status.SupportedInterfaceVersions[0] != InterfaceVersion10 {
		t.Fatalf("got SupportedInterfaceVersions=%v, want [%s]", resp.Status.SupportedInterfaceVersions, InterfaceVersion10)
	}
}
