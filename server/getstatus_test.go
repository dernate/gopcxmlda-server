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

// TestHandleGetStatus_LocaleRequested_RefetchesWithRevisedLocale
// reproduces the gap where ServeHTTP's own status.GetStatus(ctx, "") call
// (needed to evaluate RequiresFault before the request body is even
// decoded) was reused as-is for the whole response — so a client
// requesting a supported, non-default locale got RevisedLocaleID set to
// that locale while StatusInfo/VendorInfo (specified as locale-specific,
// §3.1.6) silently stayed whatever the locale-less fetch happened to
// return. handleGetStatus must re-fetch with the resolved locale.
func TestHandleGetStatus_LocaleRequested_RefetchesWithRevisedLocale(t *testing.T) {
	be, status, _ := newMinimalBackend()
	status.SetLocales([]string{"en-US", "de-DE"})
	status.SetStatusInfo("de-DE", "Läuft normal")
	status.SetStatusInfo("en-US", "Running normally")
	h := newTestHandler(t, be, Config{}, clock.Real{})

	body := soapEnvelopeOpen + `<GetStatus xmlns="` + xmlda.Namespace + `" LocaleID="de-DE"/>` + soapEnvelopeClose
	got := decodeResponse[xmlda.GetStatusResponse](t, postSOAP(t, h, body))

	if got.Result.RevisedLocaleID != "de-DE" {
		t.Fatalf("got RevisedLocaleID=%q, want de-DE", got.Result.RevisedLocaleID)
	}
	if got.Status.StatusInfo != "Läuft normal" {
		t.Fatalf("got StatusInfo=%q, want the de-DE-specific text — the backend was not re-fetched with the requested locale", got.Status.StatusInfo)
	}

	// ServeHTTP's own pre-decode call (locale "") plus handleGetStatus's
	// re-fetch (locale "de-DE"): exactly two calls, in that order.
	calls := status.CalledLocales()
	if len(calls) != 2 || calls[0] != "" || calls[1] != "de-DE" {
		t.Fatalf("got backend GetStatus calls %v, want [\"\" \"de-DE\"]", calls)
	}
}

// TestHandleGetStatus_UnsupportedLocale_RefetchesWithFallback covers a
// requested locale the backend does not support at all: the server must
// not blindly re-fetch with the client's unresolvable literal locale, nor
// silently skip the re-fetch — it re-fetches with the same fallback
// locale RevisedLocaleID reports, so StatusInfo and RevisedLocaleID agree.
func TestHandleGetStatus_UnsupportedLocale_RefetchesWithFallback(t *testing.T) {
	be, status, _ := newMinimalBackend()
	status.SetLocales([]string{"en-US"})
	status.SetStatusInfo("en-US", "Fallback info")
	h := newTestHandler(t, be, Config{}, clock.Real{})

	body := soapEnvelopeOpen + `<GetStatus xmlns="` + xmlda.Namespace + `" LocaleID="fr-FR"/>` + soapEnvelopeClose
	got := decodeResponse[xmlda.GetStatusResponse](t, postSOAP(t, h, body))

	if got.Result.RevisedLocaleID != "en-US" {
		t.Fatalf("got RevisedLocaleID=%q, want the fallback en-US", got.Result.RevisedLocaleID)
	}
	if got.Status.StatusInfo != "Fallback info" {
		t.Fatalf("got StatusInfo=%q, want the fallback locale's text", got.Status.StatusInfo)
	}
	calls := status.CalledLocales()
	if len(calls) != 2 || calls[1] != "en-US" {
		t.Fatalf("got backend GetStatus calls %v, want the re-fetch to use the fallback locale en-US, not the unresolvable fr-FR", calls)
	}
}

// TestHandleGetStatus_NoLocaleRequested_NoRefetch is the regression-safety
// companion: when the client asks for no locale at all, the first
// (locale-"") fetch already holds the right answer, and a second backend
// call would be pure waste.
func TestHandleGetStatus_NoLocaleRequested_NoRefetch(t *testing.T) {
	be, status, _ := newMinimalBackend()
	status.SetLocales([]string{"en-US", "de-DE"})
	h := newTestHandler(t, be, Config{}, clock.Real{})

	got := decodeResponse[xmlda.GetStatusResponse](t, postSOAP(t, h, getStatusRequestBody("CRH1")))
	if got.Result.RevisedLocaleID != "en-US" {
		t.Fatalf("got RevisedLocaleID=%q, want the default en-US", got.Result.RevisedLocaleID)
	}
	if calls := status.CalledLocales(); len(calls) != 1 || calls[0] != "" {
		t.Fatalf("got backend GetStatus calls %v, want exactly one call with locale \"\"", calls)
	}
}
