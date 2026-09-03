package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// valuelessReader returns a formally well-shaped Result whose ItemSample
// was never given a Value — the Go zero value rather than the
// xmlda.NewNil(declaredType) the backend contract calls for. It is the
// single most common way a hand-written backend violates that contract,
// and it used to be catastrophic rather than local.
type valuelessReader struct{ quality xmlda.OPCQuality }

func (v valuelessReader) Read(_ context.Context, items []backend.ReadRequestItem) ([]backend.Result[backend.ItemSample], error) {
	out := make([]backend.Result[backend.ItemSample], len(items))
	for i := range out {
		out[i] = backend.Result[backend.ItemSample]{
			Value: backend.ItemSample{Quality: v.quality, Timestamp: time.Unix(0, 0).UTC()},
		}
	}
	return out, nil
}

// TestHandleRead_ValuelessBackendSampleDoesNotFailWholeResponse pins the
// blast radius of one malformed item. A Value that was never constructed
// carries no declared type, and xmlda's encoder rejects it — which, since
// the whole response is encoded as one document, turned a single bad item
// into a blanket E_FAIL fault for the entire operation, discarding up to
// MaxItemsPerRequest other items' perfectly good data. Property values
// were already gated this way (server/browse.go's toItemProperty); item
// values were the gap.
func TestHandleRead_ValuelessBackendSampleDoesNotFailWholeResponse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		quality xmlda.OPCQuality
	}{
		{"good quality", xmlda.NewGoodQuality()},
		{"bad quality", xmlda.NewQuality(xmlda.QualityBad, xmlda.LimitNone, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := backend.Backend{Status: newTestStatus(), Reader: valuelessReader{quality: tc.quality}}
			h := newTestHandler(t, be, Config{}, clock.Real{})

			resp := postSOAP(t, h, readRequestBody([]string{"A"}))
			raw := readBody(t, resp)
			if strings.Contains(raw, "Fault") {
				t.Fatalf("one valueless item turned the whole response into a fault:\n%s", raw)
			}
			out := decodeResponseFrom[xmlda.ReadResponse](t, raw)
			if len(out.RItemList.Items) != 1 {
				t.Fatalf("got %d items, want 1", len(out.RItemList.Items))
			}
			if out.RItemList.Items[0].Value != nil {
				t.Error("an item with no constructed Value must not carry a <Value> element")
			}
			if out.RItemList.Items[0].Quality == nil {
				t.Error("the item lost its Quality element")
			}
		})
	}
}

// TestHandleSubscribe_EncodeFailureLeavesNoOrphan pins the rollback. The
// subscription is created before the response is encoded, so an encode
// failure used to leave a live, backend-polling subscription whose handle
// the client never saw — unreachable for SubscriptionCancel and collected
// only by the abandonment reaper, after a grace period the client itself
// chooses via SubscriptionPingRate.
func TestHandleSubscribe_EncodeFailureLeavesNoOrphan(t *testing.T) {
	be := backend.Backend{Status: newTestStatus(), Reader: unencodableReader{}}
	h := newTestHandler(t, be, Config{}, clock.Real{})

	resp := postSOAP(t, h, subscribeRequestBody("A", "CIH", true))
	raw := readBody(t, resp)
	if !strings.Contains(raw, "Fault") {
		t.Fatalf("setup: expected the response to fail encoding, got:\n%s", raw)
	}
	if strings.Contains(raw, "ServerSubHandle=") {
		t.Fatalf("setup: the client did get a handle, so there is no orphan to test for:\n%s", raw)
	}
	if n := h.subs.Count(); n != 0 {
		t.Errorf("%d subscription(s) survived a Subscribe whose response never reached the client; "+
			"the client cannot cancel a handle it never saw", n)
	}
}

// unencodableReader hands back a Value that is well-formed as far as the
// type system is concerned — it has a declared type — but whose literal
// the encoder rejects. xmlda.NewDuration takes the ISO 8601 lexical form
// verbatim and does not validate it, so this is a mistake a real backend
// can make.
type unencodableReader struct{}

func (unencodableReader) Read(_ context.Context, items []backend.ReadRequestItem) ([]backend.Result[backend.ItemSample], error) {
	out := make([]backend.Result[backend.ItemSample], len(items))
	for i := range out {
		out[i] = backend.Result[backend.ItemSample]{
			Value: backend.ItemSample{Value: xmlda.NewDuration("not-a-duration"), Timestamp: time.Unix(0, 0).UTC()},
		}
	}
	return out, nil
}

// TestHandleGetStatus_LocalizedRefetchIsNormalized pins that the second,
// locale-carrying GetStatus call goes through normalizeStatus like every
// other path. ServerState is use="required" in the schema, so a backend
// that fills it in on the locale-less call and forgets it on the
// localized one made every locale-carrying reply schema-invalid — with no
// warning anywhere.
func TestHandleGetStatus_LocalizedRefetchIsNormalized(t *testing.T) {
	be := backend.Backend{Status: forgetfulStatus{}, Reader: newTestReader()}
	h := newTestHandler(t, be, Config{}, clock.Real{})

	for _, locale := range []string{"", "en-US"} {
		attr := ""
		if locale != "" {
			attr = ` LocaleID="` + locale + `"`
		}
		body := soapEnvelopeOpen + `<GetStatus xmlns="` + xmlda.Namespace + `"` + attr + `/>` + soapEnvelopeClose
		raw := readBody(t, postSOAP(t, h, body))
		if !strings.Contains(raw, `ServerState=`) {
			t.Errorf("LocaleID=%q: the reply carries no ServerState attribute, "+
				"which the schema declares use=\"required\":\n%s", locale, raw)
		}
	}
}

// forgetfulStatus fills ServerState in only when no locale was requested
// — the shape normalizeStatus exists to absorb.
type forgetfulStatus struct{}

func (forgetfulStatus) GetStatus(_ context.Context, locale string) (backend.ServerStatus, error) {
	st := backend.ServerStatus{
		StartTime:          time.Unix(0, 0).UTC(),
		SupportedLocaleIDs: []string{"en-US", "de"},
		ProductVersion:     "1.0",
	}
	if locale == "" {
		st.State = xmlda.ServerStateRunning
	}
	return st, nil
}

// TestHandleSubscribe_ReturnValuesOnReplyFalseLeavesRItemListEmpty pins
// §3.5.2: "If ReturnValuesOnReply is false and no errors are found,
// RItemList will be empty." Emitting one value-less, quality-less
// <ItemValue> per requested item instead is read by a DA bridge as "this
// item is reporting something" and lands an item with no quality in its
// process image.
func TestHandleSubscribe_ReturnValuesOnReplyFalseLeavesRItemListEmpty(t *testing.T) {
	be, _, r := newRWBackend(t)
	r.Set(backend.ItemRef{ItemName: "good"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{}, clock.Real{})
	t.Cleanup(func() { _ = h.Shutdown(context.Background()) })

	out := decodeResponseFrom[xmlda.SubscribeResponse](t,
		readBody(t, postSOAP(t, h, subscribeRequestBody("good", "CIH", false))))
	if len(out.RItemList.Items) != 0 {
		t.Errorf("RItemList carries %d entries for a healthy item with ReturnValuesOnReply=false, want 0",
			len(out.RItemList.Items))
	}
	if out.ServerSubHandle == "" {
		t.Error("no ServerSubHandle was issued")
	}

	// An item that DOES carry a condition is still reported — those are
	// the "errors" the sentence excludes.
	out2 := decodeResponseFrom[xmlda.SubscribeResponse](t,
		readBody(t, postSOAP(t, h, subscribeRequestBody("missing", "CIH", false))))
	if len(out2.RItemList.Items) != 1 {
		t.Fatalf("an item reporting a condition was dropped from RItemList: %+v", out2.RItemList.Items)
	}
	if out2.RItemList.Items[0].ItemValue.ResultID.IsZero() {
		t.Error("the reported item carries no ResultID")
	}
}

// TestHandleRead_ReturnDiagnosticInfoAlwaysEmitsTheElement pins §3.1.6's
// wording for ReturnDiagnosticInfo: "The server is required to return
// specific diagnostic information OR A BLANK STRING if diagnostic
// information is not available." Omitting the element when the backend
// supplied nothing left a client unable to tell "nothing to report" from
// "the server ignored my option".
func TestHandleRead_ReturnDiagnosticInfoAlwaysEmitsTheElement(t *testing.T) {
	be, _, r := newRWBackend(t)
	r.Set(backend.ItemRef{ItemName: "good"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	body := soapEnvelopeOpen + `<Read xmlns="` + xmlda.Namespace + `">` +
		`<Options ReturnDiagnosticInfo="true"/>` +
		`<ItemList><Items ItemName="good"/></ItemList></Read>` + soapEnvelopeClose
	raw := readBody(t, postSOAP(t, h, body))
	if !strings.Contains(raw, "<DiagnosticInfo>") {
		t.Errorf("ReturnDiagnosticInfo=true produced no <DiagnosticInfo> element for an item "+
			"the backend had nothing to say about:\n%s", raw)
	}

	// And with the option off, the element must not appear at all.
	bodyOff := soapEnvelopeOpen + `<Read xmlns="` + xmlda.Namespace + `">` +
		`<Options ReturnDiagnosticInfo="false"/>` +
		`<ItemList><Items ItemName="good"/></ItemList></Read>` + soapEnvelopeClose
	rawOff := readBody(t, postSOAP(t, h, bodyOff))
	if strings.Contains(rawOff, "<DiagnosticInfo") {
		t.Errorf("ReturnDiagnosticInfo=false still emitted a <DiagnosticInfo> element:\n%s", rawOff)
	}
}

// TestServeHTTP_MustUnderstandHeaderIsFaulted pins SOAP 1.1 §4.2.3. This
// package understands no header blocks — OPC XML-DA defines none — so any
// block flagged mustUnderstand is by definition one it cannot honor.
// Processing the request anyway was not merely non-conformant: a
// deployment that puts authorization in a WS-Security header had that
// header dropped and the operation carried out regardless.
func TestServeHTTP_MustUnderstandHeaderIsFaulted(t *testing.T) {
	be, _, r := newRWBackend(t)
	r.Set(backend.ItemRef{ItemName: "good"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	const envOpen = `<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://schemas.xmlsoap.org/soap/envelope/">`
	read := `<Read xmlns="` + xmlda.Namespace + `"><ItemList><Items ItemName="good"/></ItemList></Read>`

	withMU := envOpen +
		`<SOAP-ENV:Header><t:Auth xmlns:t="urn:x" SOAP-ENV:mustUnderstand="1">secret</t:Auth></SOAP-ENV:Header>` +
		`<SOAP-ENV:Body>` + read + `</SOAP-ENV:Body></SOAP-ENV:Envelope>`
	raw := readBody(t, postSOAP(t, h, withMU))
	if !strings.Contains(raw, "MustUnderstand") {
		t.Errorf("a mustUnderstand header block was processed instead of faulted:\n%s", raw)
	}

	// A header block WITHOUT the flag stays ignorable, as it always was.
	withoutMU := envOpen +
		`<SOAP-ENV:Header><t:Hint xmlns:t="urn:x">fyi</t:Hint></SOAP-ENV:Header>` +
		`<SOAP-ENV:Body>` + read + `</SOAP-ENV:Body></SOAP-ENV:Envelope>`
	rawOK := readBody(t, postSOAP(t, h, withoutMU))
	if strings.Contains(rawOK, "Fault") {
		t.Errorf("an unflagged header block was faulted:\n%s", rawOK)
	}
}

// TestServeHTTP_MultipleBodiesAreRejected pins SOAP 1.1 §4's "exactly one
// Body". encoding/xml keeps the LAST match for a non-slice field, so the
// server executed the last Body while an intermediary inspecting the
// first — a proxy, an audit log, a policy filter — saw a different
// operation. That divergence is the whole vulnerability.
func TestServeHTTP_MultipleBodiesAreRejected(t *testing.T) {
	be, _, r := newRWBackend(t)
	r.Set(backend.ItemRef{ItemName: "good"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	body := `<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://schemas.xmlsoap.org/soap/envelope/">` +
		`<SOAP-ENV:Body><Read xmlns="` + xmlda.Namespace + `"><ItemList><Items ItemName="good"/></ItemList></Read></SOAP-ENV:Body>` +
		`<SOAP-ENV:Body><Read xmlns="` + xmlda.Namespace + `"><ItemList><Items ItemName="other"/></ItemList></Read></SOAP-ENV:Body>` +
		`</SOAP-ENV:Envelope>`
	raw := readBody(t, postSOAP(t, h, body))
	if !strings.Contains(raw, "Fault") {
		t.Errorf("an envelope with two Body elements was executed:\n%s", raw)
	}
}

// TestServeHTTP_TransportGuards pins the two HTTP-level answers that are
// not SOAP faults: a wrong method must name what is allowed (RFC 9110
// §15.5.6 makes Allow mandatory), and a Content-Type that positively
// claims to be something else must be refused, so the endpoint is not
// reachable by a cross-origin form post — a "simple request" a browser
// sends with no preflight.
func TestServeHTTP_TransportGuards(t *testing.T) {
	be, _, r := newRWBackend(t)
	r.Set(backend.ItemRef{ItemName: "good"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{}, clock.Real{})
	body := readRequestBody([]string{"good"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET returned %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("405 carries Allow=%q, want %q", got, http.MethodPost)
	}

	for _, tc := range []struct {
		ct       string
		wantCode int
	}{
		{"text/xml; charset=utf-8", http.StatusOK},
		{"application/soap+xml", http.StatusOK},
		{"", http.StatusOK}, // absent: tolerated, real clients omit it
		{"application/x-www-form-urlencoded", http.StatusUnsupportedMediaType},
		{"text/plain", http.StatusUnsupportedMediaType},
		{"multipart/form-data; boundary=x", http.StatusUnsupportedMediaType},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		if tc.ct != "" {
			req.Header.Set("Content-Type", tc.ct)
		} else {
			req.Header.Del("Content-Type")
		}
		h.ServeHTTP(rec, req)
		if rec.Code != tc.wantCode {
			t.Errorf("Content-Type %q returned %d, want %d", tc.ct, rec.Code, tc.wantCode)
		}
	}
}

// TestHandleGetProperties_PropertyNamesAreBounded pins the amplification
// limit. ItemIDs was bounded and PropertyNames was not, and the two
// multiply: the response carries one ItemProperty per item AND per name,
// assembled in memory before a byte goes out, so a 215 KB request
// produced a 739 MB response.
func TestHandleGetProperties_PropertyNamesAreBounded(t *testing.T) {
	be := backend.Backend{Status: newTestStatus(), Reader: newTestReader(),
		Properties: &testProperties{props: map[backend.ItemRef][]backend.Property{}}}
	h := newTestHandler(t, be, Config{MaxItemsPerRequest: 100}, clock.Real{})

	var names strings.Builder
	for range 200 {
		names.WriteString(`<PropertyNames>opc:dataType</PropertyNames>`)
	}
	body := soapEnvelopeOpen + `<GetProperties xmlns="` + xmlda.Namespace + `" xmlns:opc="` + xmlda.Namespace + `">` +
		`<ItemIDs ItemName="A"/>` + names.String() + `</GetProperties>` + soapEnvelopeClose
	raw := readBody(t, postSOAP(t, h, body))
	if !strings.Contains(raw, "E_OUTOFMEMORY") {
		t.Errorf("200 property names passed a limit of 100:\n%s", raw[:min(len(raw), 400)])
	}

	// And the product of two individually-legal lists is bounded too.
	var names2 strings.Builder
	for range 20 {
		names2.WriteString(`<PropertyNames>opc:dataType</PropertyNames>`)
	}
	var items strings.Builder
	for range 20 {
		items.WriteString(`<ItemIDs ItemName="A"/>`)
	}
	body2 := soapEnvelopeOpen + `<GetProperties xmlns="` + xmlda.Namespace + `" xmlns:opc="` + xmlda.Namespace + `">` +
		items.String() + names2.String() + `</GetProperties>` + soapEnvelopeClose
	raw2 := readBody(t, postSOAP(t, h, body2))
	if !strings.Contains(raw2, "E_OUTOFMEMORY") {
		t.Errorf("20x20 = 400 item/property combinations passed a limit of 100:\n%s", raw2[:min(len(raw2), 400)])
	}
}

// TestServeHTTP_LongPollsDoNotStarveShortOperations pins the class
// separation. A long poll legitimately holds its slot for up to
// MaxPolledRefreshWait; a Read passes through in milliseconds. Sharing
// one semaphore meant enough concurrent long polls answered every other
// client's request with E_BUSY for minutes — and with no authentication
// in front of the server, "enough" is whatever one client opens.
func TestServeHTTP_LongPollsDoNotStarveShortOperations(t *testing.T) {
	be, _, r := newRWBackend(t)
	r.Set(backend.ItemRef{ItemName: "good"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{MaxConcurrentRequests: 4, MaxPolledRefreshWait: 5 * time.Second}, clock.Real{})
	t.Cleanup(func() { _ = h.Shutdown(context.Background()) })

	sub := decodeResponse[xmlda.SubscribeResponse](t, postSOAP(t, h, subscribeRequestBody("good", "CIH", false)))
	if sub.ServerSubHandle == "" {
		t.Fatal("setup: no subscription handle")
	}

	hold := time.Now().UTC().Add(3 * time.Second).Format("2006-01-02T15:04:05.000Z")
	poll := soapEnvelopeOpen + `<SubscriptionPolledRefresh xmlns="` + xmlda.Namespace + `" HoldTime="` + hold + `" WaitTime="0">` +
		`<ServerSubHandles>` + sub.ServerSubHandle + `</ServerSubHandles></SubscriptionPolledRefresh>` + soapEnvelopeClose

	// Occupy every slot the poll class is allowed to take.
	for range 8 {
		go func() {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(poll)))
		}()
	}
	time.Sleep(200 * time.Millisecond)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(getStatusRequestBody("CRH"))))
	if strings.Contains(rec.Body.String(), "E_BUSY") {
		t.Errorf("held long polls locked out a GetStatus; the two classes share no budget any more:\n%s",
			rec.Body.String())
	}
}

// TestHandleSubscribe_DeadbandOutOfRangeIsRejected pins §3.5.1: "The
// deadband value shall be in the range 0-100 percent." An out-of-range
// value was taken at face value, and anything above 100 silences the item
// almost completely — with the client never told, which is the one
// failure mode a deadband must not have.
func TestHandleSubscribe_DeadbandOutOfRangeIsRejected(t *testing.T) {
	be, _, r := newRWBackend(t)
	r.Set(backend.ItemRef{ItemName: "good"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{}, clock.Real{})
	t.Cleanup(func() { _ = h.Shutdown(context.Background()) })

	sub := func(deadband string) *xmlda.SubscribeResponse {
		t.Helper()
		body := soapEnvelopeOpen + `<Subscribe xmlns="` + xmlda.Namespace + `" ReturnValuesOnReply="true">` +
			`<ItemList><Items ItemName="good" ClientItemHandle="CIH" Deadband="` + deadband + `"/></ItemList>` +
			`</Subscribe>` + soapEnvelopeClose
		return decodeResponse[xmlda.SubscribeResponse](t, postSOAP(t, h, body))
	}

	for _, bad := range []string{"150", "-5", "100.1"} {
		out := sub(bad)
		if len(out.RItemList.Items) != 1 {
			t.Fatalf("Deadband=%s: got %d items, want 1", bad, len(out.RItemList.Items))
		}
		if got := out.RItemList.Items[0].ItemValue.ResultID; got != xmlda.ErrRange {
			t.Errorf("Deadband=%s: ResultID = %v, want E_RANGE", bad, got)
		}
	}
	for _, ok := range []string{"0", "50", "100"} {
		out := sub(ok)
		if len(out.RItemList.Items) != 1 {
			t.Fatalf("Deadband=%s: got %d items, want 1", ok, len(out.RItemList.Items))
		}
		if got := out.RItemList.Items[0].ItemValue.ResultID; !got.IsZero() {
			t.Errorf("Deadband=%s was rejected with %v, but it is in range", ok, got)
		}
	}
}

// TestReviseLocale_LanguageFallback pins §2.4's intermediate step. Jumping
// straight from "exact match" to "the server's default" answered a client
// asking for de-AT in English although the server speaks German — and
// RevisedLocaleID then reported en-US, so the client could not even tell
// it had been misunderstood.
func TestReviseLocale_LanguageFallback(t *testing.T) {
	for _, tc := range []struct {
		requested string
		supported []string
		want      string
	}{
		{"de-DE", []string{"en-US", "de-DE"}, "de-DE"}, // exact wins
		{"DE-de", []string{"en-US", "de-DE"}, "de-DE"}, // case-insensitive
		{"de-AT", []string{"en-US", "de"}, "de"},       // bare language
		{"de-AT", []string{"en-US", "de-DE"}, "de-DE"}, // another region, same language
		{"fr-FR", []string{"en-US", "de-DE"}, "en-US"}, // nothing close: default
		{"", []string{"en-US", "de-DE"}, "en-US"},      // unspecified: default
		{"de", []string{"en-US", "de-DE"}, "de-DE"},    // language asked, region offered
	} {
		if got := reviseLocale(tc.requested, tc.supported); got != tc.want {
			t.Errorf("reviseLocale(%q, %v) = %q, want %q", tc.requested, tc.supported, got, tc.want)
		}
	}
}

// TestContinuationPoint_NegativeTTLMeansNoExpiry pins the documented
// meaning of a negative Config.ContinuationPointTTL. Feeding it to
// Now().Add() dated every issued token into the past, so an operator who
// followed the documentation ("Negative = no expiry") disabled Browse
// pagination outright.
func TestContinuationPoint_NegativeTTLMeansNoExpiry(t *testing.T) {
	be, _, _ := newRWBackend(t)
	h := newTestHandler(t, be, Config{ContinuationPointTTL: -1}, clock.Real{})

	req := xmlda.BrowseRequest{ItemName: "Root"}
	token := h.buildContinuationToken(req, "cursor-42")
	if token == "" {
		t.Fatal("no token was issued for a non-empty backend cursor")
	}
	cursor, ok := h.parseContinuationToken(token, req)
	if !ok {
		t.Fatal("a token issued with a no-expiry TTL was refused immediately")
	}
	if cursor != "cursor-42" {
		t.Errorf("cursor = %q, want %q", cursor, "cursor-42")
	}

	// The MAC still covers everything it did: a token minted for one
	// filter set must not verify against another.
	if _, ok := h.parseContinuationToken(token, xmlda.BrowseRequest{ItemName: "Other"}); ok {
		t.Error("a token verified against a different filter set")
	}
}

// TestConfigWithDefaults_ResolvesForwardedFields pins the promise in
// WithDefaults' own doc comment. Seven fields are only forwarded to
// subscription.Config, and leaving them at zero made the "fully-resolved
// limits" this method is exported to expose wrong in exactly the places
// an operator reads them: a sizing calculation, a diagnostics endpoint, a
// startup log line.
func TestConfigWithDefaults_ResolvesForwardedFields(t *testing.T) {
	c := Config{}.WithDefaults()
	for _, f := range []struct {
		name string
		zero bool
	}{
		{"MaxConcurrentPolls", c.MaxConcurrentPolls == 0},
		{"ReapInterval", c.ReapInterval == 0},
		{"ReapGraceMultiplier", c.ReapGraceMultiplier == 0},
		{"DefaultSubscriptionPingRate", c.DefaultSubscriptionPingRate == 0},
		{"DefaultSamplingRate", c.DefaultSamplingRate == 0},
		{"MaxBufferedSamplesPerItem", c.MaxBufferedSamplesPerItem == 0},
		{"PollTimeout", c.PollTimeout == 0},
		{"MaxElementDepth", c.MaxElementDepth == 0},
		{"MaxConcurrentPolledRefresh", c.MaxConcurrentPolledRefresh == 0},
		{"BackendTimeout", c.BackendTimeout == 0},
	} {
		if f.zero {
			t.Errorf("Config{}.WithDefaults() left %s at zero", f.name)
		}
	}

	// An explicit value survives, and a deliberate "no limit" is not
	// clobbered into a default.
	custom := Config{MaxConcurrentPolls: 7, MaxItemsPerRequest: -1}.WithDefaults()
	if custom.MaxConcurrentPolls != 7 {
		t.Errorf("MaxConcurrentPolls = %d, want the caller's 7", custom.MaxConcurrentPolls)
	}
	if custom.MaxItemsPerRequest != -1 {
		t.Errorf("MaxItemsPerRequest = %d, want the caller's -1 (no limit)", custom.MaxItemsPerRequest)
	}
}

// TestServeHTTP_SOAP12RequestGetsA12Response pins the version mirroring.
// This library accepts either envelope version (ADR-004) and used to
// answer both in SOAP 1.1 — which a strict 1.2 stack discards, losing the
// payload or the error code it was waiting for. The OPC XML-DA body is
// identical either way; only the envelope and fault shapes differ.
func TestServeHTTP_SOAP12RequestGetsA12Response(t *testing.T) {
	be, _, r := newRWBackend(t)
	r.Set(backend.ItemRef{ItemName: "good"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	const ns12 = "http://www.w3.org/2003/05/soap-envelope"
	read := `<Read xmlns="` + xmlda.Namespace + `"><ItemList><Items ItemName="good"/></ItemList></Read>`
	body12 := `<?xml version="1.0"?><soap:Envelope xmlns:soap="` + ns12 + `"><soap:Body>` + read + `</soap:Body></soap:Envelope>`

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body12))
	req.Header.Set("Content-Type", "application/soap+xml")
	h.ServeHTTP(rec, req)

	out := rec.Body.String()
	if !strings.Contains(out, ns12) {
		t.Errorf("a SOAP 1.2 request was answered with a 1.1 envelope:\n%s", out)
	}
	if strings.Contains(out, "http://schemas.xmlsoap.org/soap/envelope/") {
		t.Errorf("the response carries the SOAP 1.1 namespace:\n%s", out)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/soap+xml") {
		t.Errorf("Content-Type = %q, want application/soap+xml for a SOAP 1.2 response", ct)
	}
	// The payload itself is version-independent.
	if !strings.Contains(out, "ReadResponse") {
		t.Errorf("the OPC XML-DA payload is missing:\n%s", out)
	}

	// A fault must take the 1.2 shape too: Code/Value and Reason/Text,
	// not faultcode/faultstring.
	bad12 := `<?xml version="1.0"?><soap:Envelope xmlns:soap="` + ns12 + `"><soap:Body>` +
		`<NotAnOperation xmlns="` + xmlda.Namespace + `"/></soap:Body></soap:Envelope>`
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(bad12))
	req2.Header.Set("Content-Type", "application/soap+xml")
	h.ServeHTTP(rec2, req2)
	fault := rec2.Body.String()
	if !strings.Contains(fault, "Reason") || !strings.Contains(fault, "Code") {
		t.Errorf("the fault does not use the SOAP 1.2 shape:\n%s", fault)
	}
	if strings.Contains(fault, "<faultstring>") {
		t.Errorf("the fault uses the SOAP 1.1 shape:\n%s", fault)
	}

	// And SOAP 1.1 must be entirely unaffected.
	raw11 := readBody(t, postSOAP(t, h, readRequestBody([]string{"good"})))
	if !strings.Contains(raw11, "http://schemas.xmlsoap.org/soap/envelope/") {
		t.Errorf("a SOAP 1.1 request no longer gets a 1.1 envelope:\n%s", raw11)
	}
}
