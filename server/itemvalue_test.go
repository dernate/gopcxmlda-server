package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/telemetry"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// --- xsd:int wire fields accept negatives instead of faulting ---

// TestSignedWireFields_NegativeValuesAreNormalized pins the fix for
// WaitTime, SubscriptionPingRate, RequestedSamplingRate, MaxAge and
// MaxElementsReturned having been modeled as unsigned. They are xsd:int
// in the schema this library now ships, so a negative value is valid
// input a schema-checking client may legitimately send — and it used to
// be rejected with a whole-operation parse fault
// ("strconv.ParseUint: parsing \"-1\"").
func TestSignedWireFields_NegativeValuesAreNormalized(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	be.Browser = &testBrowser{}
	h := newTestHandler(t, be, Config{}, nil)

	cases := []struct {
		name, body string
	}{
		{"Read MaxAge", soapEnvelopeOpen + `<Read xmlns="` + xmlda.Namespace + `">` +
			`<Options ClientRequestHandle="CRH1"/><ItemList MaxAge="-1">` +
			`<Items ItemName="Item1"/></ItemList></Read>` + soapEnvelopeClose},
		{"Subscribe SubscriptionPingRate + RequestedSamplingRate",
			soapEnvelopeOpen + `<Subscribe xmlns="` + xmlda.Namespace + `" ` +
				`ReturnValuesOnReply="true" SubscriptionPingRate="-1">` +
				`<Options ClientRequestHandle="CRH1"/>` +
				`<ItemList RequestedSamplingRate="-5"><Items ItemName="Item1"/></ItemList>` +
				`</Subscribe>` + soapEnvelopeClose},
		{"Browse MaxElementsReturned", soapEnvelopeOpen +
			`<Browse xmlns="` + xmlda.Namespace + `" MaxElementsReturned="-1"/>` + soapEnvelopeClose},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postSOAP(t, h, tc.body)
			body, _ := io.ReadAll(resp.Body)
			if strings.Contains(string(body), "ParseUint") || strings.Contains(string(body), "ParseInt") {
				t.Fatalf("a negative xsd:int was rejected as a parse fault:\n%s", body)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("got HTTP %d, want 200 for schema-valid input:\n%s", resp.StatusCode, body)
			}
		})
	}
}

// TestObserveBackendLatency_IsCalled pins the fix for
// telemetry.Metrics.ObserveBackendLatency having been declared and never
// invoked — every implementation had to stub a method that produced
// nothing.
func TestObserveBackendLatency_IsCalled(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	be.Writer = &testWriter{reader: reader}
	be.Browser = &testBrowser{}
	be.Properties = &testProperties{props: map[backend.ItemRef][]backend.Property{
		{ItemName: "Item1"}: {{ID: xmlda.PropDescription, Value: xmlda.NewString("d")}},
	}}
	m := &recordingMetrics{Metrics: telemetry.NoopMetrics()}
	h, err := New(Deps{Backend: be, Metrics: m}, Config{StatusCacheTTL: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Shutdown(context.Background()) })

	postSOAP(t, h, readRequestBody([]string{"Item1"}))
	postSOAP(t, h, writeRequestBody("Item1", "int", "5", false))
	postSOAP(t, h, browseRequestBody())
	postSOAP(t, h, getPropertiesRequestBody("Item1"))
	postSOAP(t, h, getStatusRequestBody("CRH1"))

	seen := m.seen()
	for _, want := range []string{"Read", "Write", "Browse", "GetProperties", "GetStatus"} {
		found := false
		for _, got := range seen {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no ObserveBackendLatency for %q; observed %v", want, seen)
		}
	}
}
