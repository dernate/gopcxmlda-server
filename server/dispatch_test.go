package server

import (
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock"
	"github.com/dernate/gopcxmlda-server/soap"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

func newMinimalBackend() (backend.Backend, *testStatus, *testReader) {
	status := newTestStatus()
	reader := newTestReader()
	return backend.Backend{Status: status, Reader: reader}, status, reader
}

func TestServeHTTP_MethodNotAllowed(t *testing.T) {
	be, _, _ := newMinimalBackend()
	h := newTestHandler(t, be, Config{}, clock.Real{})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got status %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestServeHTTP_MalformedXML(t *testing.T) {
	be, _, _ := newMinimalBackend()
	h := newTestHandler(t, be, Config{}, clock.Real{})

	resp := postSOAP(t, h, `<this is not well-formed`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got status %d, want %d", resp.StatusCode, http.StatusInternalServerError)
	}
	f := decodeFault(t, resp)
	if f == nil {
		t.Fatalf("expected a SOAP fault body")
	}
}

func TestServeHTTP_UnknownOperation(t *testing.T) {
	be, _, _ := newMinimalBackend()
	h := newTestHandler(t, be, Config{}, clock.Real{})

	body := `<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://schemas.xmlsoap.org/soap/envelope/"><SOAP-ENV:Body>` +
		`<SomeUnknownOperation xmlns="` + xmlda.Namespace + `"/></SOAP-ENV:Body></SOAP-ENV:Envelope>`
	resp := postSOAP(t, h, body)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got status %d, want %d", resp.StatusCode, http.StatusInternalServerError)
	}
	f := decodeFault(t, resp)
	if f == nil || f.Code.Local != "E_NOTSUPPORTED" {
		t.Fatalf("got %+v, want E_NOTSUPPORTED", f)
	}
}

// alwaysFailsMarshal is a minimal type whose MarshalXML always fails, for
// exercising writeResponse's encode-failure fallback.
type alwaysFailsMarshal struct{}

func (alwaysFailsMarshal) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	return errors.New("boom")
}

// TestWriteResponse_EncodeFailure_FallsBackToFault reproduces a response
// payload that fails to marshal (e.g. a Value with no declared type, from
// a non-conforming backend result). Before this was fixed, the encode
// error was silently discarded after http.ResponseWriter.WriteHeader(200)
// had already been called, reaching the client as a truncated, invalid
// XML body with a misleading 200 status. It must instead fall back to a
// clean fault, since nothing has been written to the ResponseWriter yet
// at the point the encode is attempted.
func TestWriteResponse_EncodeFailure_FallsBackToFault(t *testing.T) {
	rec := httptest.NewRecorder()
	writeResponse(rec, alwaysFailsMarshal{})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500", rec.Code)
	}
	var env soap.Envelope[struct{}]
	if err := xml.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("expected a well-formed SOAP fault body, got unmarshal error %v\nbody: %s", err, rec.Body.String())
	}
	if env.Body.Fault == nil {
		t.Fatalf("expected a Fault in the body, got %+v", env.Body)
	}
}

// TestServeHTTP_RecognizedOperationBadFieldDecode_ClientFault exercises
// bucket 3 of xmlda.IdentifyOperation's failure model: the operation is
// recognized, but a field inside it fails to decode (an invalid dateTime
// literal, mirroring testdata/faults/fault_soap12_invalid_datetime.response.xml).
// Before this was fixed, requestDecodeFault built a Fault with no Code at
// all, marshaling to an empty, non-conformant <faultcode> — this checks
// the emitted fault actually carries the standard SOAP "Client" code, not
// bucket 1's (well-formed-but-unrecognized) TestServeHTTP_MalformedXML,
// which never exercised this path at all.
func TestServeHTTP_RecognizedOperationBadFieldDecode_ClientFault(t *testing.T) {
	be, _, _ := newMinimalBackend()
	h := newTestHandler(t, be, Config{}, clock.Real{})

	body := soapEnvelopeOpen + `<Read xmlns="` + xmlda.Namespace + `"><Options ClientRequestHandle="CRH1" RequestDeadline="not-a-date"/>` +
		`<ItemList><Items ItemName="Item1"/></ItemList></Read>` + soapEnvelopeClose
	resp := postSOAP(t, h, body)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500", resp.StatusCode)
	}
	f := decodeFault(t, resp)
	if f == nil {
		t.Fatalf("expected a SOAP fault body")
	}
	if f.Code.IsZero() {
		t.Fatalf("got zero Code (empty <faultcode> on the wire), want a non-zero standard SOAP code")
	}
	if f.Code.Local != "Client" || f.Code.Space != soap.NS11 {
		t.Fatalf("got Code %+v, want {Space: %q, Local: %q}", f.Code, soap.NS11, "Client")
	}
}

// TestServeHTTP_RecognizedOperationBadFieldDecode_OtherOperations mirrors
// TestServeHTTP_RecognizedOperationBadFieldDecode_ClientFault for every
// other operation whose request struct has a field that can actually fail
// to decode (a *time.Time attribute, or a bool/uint parsed by hand) —
// before this test, only Read's version of this branch was ever
// exercised, even though every one of the 8 operation handlers has its
// own copy of the identical "if err := xmlda.Decode(...); err != nil"
// pattern in server/*.go, any one of which could silently diverge (a
// different status code, a missing fault) without being caught. GetStatus
// and SubscriptionCancel are not included: their request structs hold
// only plain string attributes, so encoding/xml's default decoding of
// them can never fail — there is no bad-field-decode branch to reach.
func TestServeHTTP_RecognizedOperationBadFieldDecode_OtherOperations(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			"Write: invalid Options.RequestDeadline",
			soapEnvelopeOpen + `<Write xmlns="` + xmlda.Namespace + `"><Options ClientRequestHandle="CRH1" RequestDeadline="not-a-date"/>` +
				`<ItemList><Items ItemName="Item1"><Value xmlns:xsd="` + xmlda.XSDNamespace + `" xmlns:xsi="` + xmlda.XSINamespace + `" xsi:type="xsd:int">1</Value></Items></ItemList></Write>` + soapEnvelopeClose,
		},
		{
			"Subscribe: invalid Options.RequestDeadline",
			soapEnvelopeOpen + `<Subscribe xmlns="` + xmlda.Namespace + `"><Options ClientRequestHandle="CRH1" RequestDeadline="not-a-date"/>` +
				`<ItemList><Items ItemName="Item1" ClientItemHandle="CIH1"/></ItemList></Subscribe>` + soapEnvelopeClose,
		},
		{
			"SubscriptionPolledRefresh: invalid HoldTime",
			soapEnvelopeOpen + `<SubscriptionPolledRefresh xmlns="` + xmlda.Namespace + `" HoldTime="not-a-date" WaitTime="0" ReturnAllItems="false">` +
				`<ServerSubHandles>h1</ServerSubHandles></SubscriptionPolledRefresh>` + soapEnvelopeClose,
		},
		{
			"Browse: invalid MaxElementsReturned",
			soapEnvelopeOpen + `<Browse xmlns="` + xmlda.Namespace + `" MaxElementsReturned="not-a-number"/>` + soapEnvelopeClose,
		},
		{
			"GetProperties: invalid ReturnAllProperties",
			soapEnvelopeOpen + `<GetProperties xmlns="` + xmlda.Namespace + `" ReturnAllProperties="not-a-bool">` +
				`<ItemIDs ItemName="Item1"/></GetProperties>` + soapEnvelopeClose,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			be, _, _ := newMinimalBackend()
			h := newTestHandler(t, be, Config{}, clock.Real{})

			resp := postSOAP(t, h, tc.body)
			if resp.StatusCode != http.StatusInternalServerError {
				t.Fatalf("got status %d, want 500", resp.StatusCode)
			}
			f := decodeFault(t, resp)
			if f == nil {
				t.Fatalf("expected a SOAP fault body")
			}
			if f.Code.IsZero() {
				t.Fatalf("got zero Code (empty <faultcode> on the wire), want a non-zero standard SOAP code")
			}
			if f.Code.Local != "Client" || f.Code.Space != soap.NS11 {
				t.Fatalf("got Code %+v, want {Space: %q, Local: %q}", f.Code, soap.NS11, "Client")
			}
		})
	}
}

func TestServeHTTP_BodyTooLarge(t *testing.T) {
	be, _, _ := newMinimalBackend()
	h := newTestHandler(t, be, Config{MaxRequestBodyBytes: 10}, clock.Real{})

	resp := postSOAP(t, h, strings.Repeat("x", 1000))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("got status %d, want %d", resp.StatusCode, http.StatusRequestEntityTooLarge)
	}
}

// TestServeHTTP_BackendPanic_RecoveredAsFault guards against a regression
// where a panic from a backend call (e.g. Reader.Read) propagated
// uncaught through ServeHTTP to net/http's own per-connection recover,
// aborting the connection with no SOAP Fault and no telemetry record
// instead of a clean 500 fault.
func TestServeHTTP_BackendPanic_RecoveredAsFault(t *testing.T) {
	be := backend.Backend{Status: newTestStatus(), Reader: panicReader{}}
	h := newTestHandler(t, be, Config{}, clock.Real{})

	resp := postSOAP(t, h, readRequestBody([]string{"Item1"}))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500", resp.StatusCode)
	}
	f := decodeFault(t, resp)
	if f == nil {
		t.Fatalf("expected a SOAP fault body, not a broken/empty response")
	}
	if f.Code.Local != "E_FAIL" {
		t.Fatalf("got %+v, want E_FAIL", f)
	}
}

func TestServeHTTP_ServerStateFailed_FaultsAllButGetStatus(t *testing.T) {
	be, status, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	status.SetState(xmlda.ServerStateFailed)
	h := newTestHandler(t, be, Config{}, clock.Real{})

	// GetStatus must still succeed even when ServerState=failed.
	resp := postSOAP(t, h, getStatusRequestBody("CRH1"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GetStatus: got status %d, want 200", resp.StatusCode)
	}

	// Read must fault.
	resp2 := postSOAP(t, h, readRequestBody([]string{"Item1"}))
	if resp2.StatusCode != http.StatusInternalServerError {
		t.Fatalf("Read under failed state: got status %d, want 500", resp2.StatusCode)
	}
	f := decodeFault(t, resp2)
	if f == nil || f.Code.Local != "E_SERVERSTATE" {
		t.Fatalf("got %+v, want E_SERVERSTATE", f)
	}
}

func TestServeHTTP_ServerStateSuspended_AllowsBrowse(t *testing.T) {
	be, status, _ := newMinimalBackend()
	status.SetState(xmlda.ServerStateSuspended)
	h := newTestHandler(t, be, Config{}, clock.Real{})

	// Browse is not one of Read/Write/Subscribe, so it must not fault
	// under Suspended (REQ-SERVER-002) — even though this backend has no
	// Browser configured, the ServerState check must run (and pass)
	// before that "not supported" condition is ever reached.
	resp := postSOAP(t, h, browseRequestBody())
	if resp.StatusCode == http.StatusInternalServerError {
		f := decodeFault(t, resp)
		if f != nil && f.Code.Local == "E_SERVERSTATE" {
			t.Fatalf("Browse must not be rejected by ServerState=suspended")
		}
	}
}
