package xmlda

import (
	"strings"
	"testing"

	"github.com/dernate/gopcxmlda-server/soap"
)

func TestSubscriptionCancelRequest_RoundTrip(t *testing.T) {
	req := SubscriptionCancelRequest{ServerSubHandle: "Handle1", ClientRequestHandle: "CRH1"}
	out, err := xmlMarshalNamed(t, "SubscriptionCancel", req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got SubscriptionCancelRequest
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.ServerSubHandle != "Handle1" || got.ClientRequestHandle != "CRH1" {
		t.Fatalf("got %+v", got)
	}
}

func TestSubscriptionCancelResponse_NoReplyBase(t *testing.T) {
	// REQ-SUBSCRIPTION-011: the response has no ReplyBase at all — assert
	// this at the wire level: no RcvTime/ReplyTime/ServerState attribute
	// appears anywhere in the encoded output.
	resp := SubscriptionCancelResponse{ClientRequestHandle: "CRH1"}
	out, err := xmlMarshalNamed(t, "SubscriptionCancelResponse", resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	for _, forbidden := range []string{"RcvTime", "ReplyTime", "ServerState"} {
		if strings.Contains(s, forbidden) {
			t.Fatalf("SubscriptionCancelResponse must not carry a ReplyBase field %q, got: %s", forbidden, s)
		}
	}
	var got SubscriptionCancelResponse
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.ClientRequestHandle != "CRH1" {
		t.Fatalf("got %q, want CRH1", got.ClientRequestHandle)
	}
}

// TestSubscriptionCancelRequest_RealFixture decodes the real captured
// request testdata/requests/subscriptioncancel_448.request.xml
// (REQ-SUBSCRIPTION-010).
func TestSubscriptionCancelRequest_RealFixture(t *testing.T) {
	doc := readTestdata(t, "testdata", "requests", "subscriptioncancel_448.request.xml")
	var env soap.Envelope[SubscriptionCancelRequest]
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
	if req.ServerSubHandle != "163121520" {
		t.Fatalf("got ServerSubHandle=%q, want 163121520", req.ServerSubHandle)
	}
	if req.ClientRequestHandle != "g6SG8duDHQtsuaEI" {
		t.Fatalf("got ClientRequestHandle=%q, want g6SG8duDHQtsuaEI", req.ClientRequestHandle)
	}
}

// TestSubscriptionCancelResponse_RealFixture decodes the real captured
// response testdata/responses/subscriptioncancel_460.response.xml
// (REQ-SUBSCRIPTION-011).
func TestSubscriptionCancelResponse_RealFixture(t *testing.T) {
	doc := readTestdata(t, "testdata", "responses", "subscriptioncancel_460.response.xml")
	var env soap.Envelope[SubscriptionCancelResponse]
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
	if resp.ClientRequestHandle != "g6SG8duDHQtsuaEI" {
		t.Fatalf("got ClientRequestHandle=%q, want g6SG8duDHQtsuaEI", resp.ClientRequestHandle)
	}
}
