package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock"
	"github.com/dernate/gopcxmlda-server/clock/clocktest"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

func TestHandleSubscribe_RoundTrip(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(42))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	resp := postSOAP(t, h, subscribeRequestBody("Item1", "CIH1", true))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	got := decodeResponse[xmlda.SubscribeResponse](t, resp)
	if got.ServerSubHandle == "" {
		t.Fatalf("expected a non-empty ServerSubHandle")
	}
	if len(got.RItemList.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(got.RItemList.Items))
	}
	iv := got.RItemList.Items[0].ItemValue
	if iv.Value == nil {
		t.Fatalf("expected a value since ReturnValuesOnReply=true")
	}
	v, err := iv.Value.Int32()
	if err != nil || v != 42 {
		t.Fatalf("got (%d, %v), want (42, nil)", v, err)
	}
}

func TestHandleSubscribe_AllInvalid_EmptyHandle(t *testing.T) {
	be, _, _ := newMinimalBackend()
	h := newTestHandler(t, be, Config{}, clock.Real{})

	got := decodeResponse[xmlda.SubscribeResponse](t, postSOAP(t, h, subscribeRequestBody("Unknown", "CIH1", false)))
	if got.ServerSubHandle != "" {
		t.Fatalf("expected empty ServerSubHandle, got %q", got.ServerSubHandle)
	}
	if len(got.Errors) != 1 || got.Errors[0].ID != xmlda.ErrUnknownItemName {
		t.Fatalf("got %+v", got.Errors)
	}
}

// TestHandleSubscribe_TooManySubscriptions_LimitFault guards against a
// regression where subscription.ErrTooManySubscriptions was mapped to a
// generic E_FAIL/500 fault instead of the same E_OUTOFMEMORY/400
// "configured limit exceeded" treatment every other Config limit
// violation in this package gets (see limitExceededFault).
func TestHandleSubscribe_TooManySubscriptions_LimitFault(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	reader.Set(backend.ItemRef{ItemName: "Item2"}, xmlda.NewInt32(2))
	h := newTestHandler(t, be, Config{MaxConcurrentSubscriptions: 1}, clock.Real{})

	first := postSOAP(t, h, subscribeRequestBody("Item1", "CIH1", false))
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first Subscribe: got status %d, want 200", first.StatusCode)
	}

	resp := postSOAP(t, h, subscribeRequestBody("Item2", "CIH2", false))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500", resp.StatusCode)
	}
	f := decodeFault(t, resp)
	if f == nil || f.Code.Local != "E_OUTOFMEMORY" {
		t.Fatalf("got %+v, want E_OUTOFMEMORY", f)
	}
}

func TestHandleSubscribe_EmptyItemList_Faults(t *testing.T) {
	be, _, _ := newMinimalBackend()
	h := newTestHandler(t, be, Config{}, clock.Real{})

	body := soapEnvelopeOpen + `<Subscribe xmlns="` + xmlda.Namespace + `" ReturnValuesOnReply="false" SubscriptionPingRate="0"><ItemList></ItemList></Subscribe>` + soapEnvelopeClose
	resp := postSOAP(t, h, body)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500", resp.StatusCode)
	}
}

func TestHandlePolledRefresh_Immediate(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	subResp := decodeResponse[xmlda.SubscribeResponse](t, postSOAP(t, h, subscribeRequestBody("Item1", "CIH1", false)))
	handle := subResp.ServerSubHandle

	// No HoldTime attribute: must return immediately with no changes.
	resp := postSOAP(t, h, polledRefreshRequestBody(handle, "", 0, false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	got := decodeResponse[xmlda.SubscriptionPolledRefreshResponse](t, resp)
	if len(got.RItemList) != 0 {
		t.Fatalf("expected no changes, got %+v", got.RItemList)
	}
}

func TestHandlePolledRefresh_ZeroHoldTime_InvalidHoldTimeFault(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	subResp := decodeResponse[xmlda.SubscribeResponse](t, postSOAP(t, h, subscribeRequestBody("Item1", "CIH1", false)))
	handle := subResp.ServerSubHandle

	// The exact zero time.Time value (year 1, month 1, day 1) is the
	// unmistakable signature of an uninitialized dateTime from a naive
	// client library, not a deliberate request — must be rejected as
	// E_INVALIDHOLDTIME rather than silently treated as "hold time already
	// elapsed, don't hold at all".
	resp := postSOAP(t, h, polledRefreshRequestBody(handle, "0001-01-01T00:00:00Z", 0, false))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500", resp.StatusCode)
	}
	f := decodeFault(t, resp)
	if f == nil || f.Code.Local != "E_INVALIDHOLDTIME" {
		t.Fatalf("got %+v, want E_INVALIDHOLDTIME", f)
	}
}

// TestHandlePolledRefresh_HoldTimeBeyondMaxWait_StrictRejects pins the
// Config.StrictHoldTime opt-in: a HoldTime further out than
// Config.MaxPolledRefreshWait is more than this server will ever block
// for, and an operator who would rather say so than silently grant a
// shorter hold gets E_INVALIDHOLDTIME.
//
// This is no longer the DEFAULT — see
// TestHandlePolledRefresh_HoldTimeBeyondMaxWait_ClampsByDefault below for
// why clamping is, and what it does instead.
func TestHandlePolledRefresh_HoldTimeBeyondMaxWait_StrictRejects(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{MaxPolledRefreshWait: time.Minute, StrictHoldTime: true}, clock.Real{})

	subResp := decodeResponse[xmlda.SubscribeResponse](t, postSOAP(t, h, subscribeRequestBody("Item1", "CIH1", false)))
	handle := subResp.ServerSubHandle

	holdTime := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	resp := postSOAP(t, h, polledRefreshRequestBody(handle, holdTime, 0, false))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500", resp.StatusCode)
	}
	f := decodeFault(t, resp)
	if f == nil || f.Code.Local != "E_INVALIDHOLDTIME" {
		t.Fatalf("got %+v, want E_INVALIDHOLDTIME", f)
	}
}

// TestHandlePolledRefresh_TooManyHandles_LimitFault reproduces the gap
// where ServerSubHandles was the only per-request collection in this
// package not bounded by Config.MaxItemsPerRequest (REQ-LIMITS-001): each
// valid handle costs fan-in goroutines for the call's duration, so an
// unbounded list is an amplification vector.
func TestHandlePolledRefresh_TooManyHandles_LimitFault(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	reader.Set(backend.ItemRef{ItemName: "Item2"}, xmlda.NewInt32(2))
	h := newTestHandler(t, be, Config{MaxItemsPerRequest: 1}, clock.Real{})

	sub1 := decodeResponse[xmlda.SubscribeResponse](t, postSOAP(t, h, subscribeRequestBody("Item1", "CIH1", false)))
	sub2 := decodeResponse[xmlda.SubscribeResponse](t, postSOAP(t, h, subscribeRequestBody("Item2", "CIH2", false)))

	body := soapEnvelopeOpen + `<SubscriptionPolledRefresh xmlns="` + xmlda.Namespace + `" WaitTime="0" ReturnAllItems="false">` +
		`<ServerSubHandles>` + sub1.ServerSubHandle + `</ServerSubHandles>` +
		`<ServerSubHandles>` + sub2.ServerSubHandle + `</ServerSubHandles>` +
		`</SubscriptionPolledRefresh>` + soapEnvelopeClose
	resp := postSOAP(t, h, body)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500", resp.StatusCode)
	}
	f := decodeFault(t, resp)
	if f == nil || f.Code.Local != "E_OUTOFMEMORY" {
		t.Fatalf("got %+v, want E_OUTOFMEMORY", f)
	}
}

// TestHandlePolledRefresh_RequestContextCancelled_ServerStateFault
// reproduces the gap where a caller's context being cancelled mid-Hold
// (e.g. the client disconnecting) mapped to a generic E_FAIL, rather than
// the more honest E_SERVERSTATE ("nobody left to read a fault, but this is
// still the truest available code").
func TestHandlePolledRefresh_RequestContextCancelled_ServerStateFault(t *testing.T) {
	fake := clocktest.New(testEpoch)
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	h, err := New(Deps{Backend: be, Clock: fake}, Config{MaxPolledRefreshWait: time.Hour})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = h.Shutdown(ctx)
	})

	subResp := decodeResponse[xmlda.SubscribeResponse](t, doPostSOAP(h, subscribeRequestBody("Item1", "CIH1", false)))
	handle := subResp.ServerSubHandle

	holdTime := testEpoch.Add(30 * time.Minute).Format(time.RFC3339Nano)
	before := fake.PendingCount()

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(polledRefreshRequestBody(handle, holdTime, 1000, false))).WithContext(ctx)
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	if !fake.WaitForPending(before+1, 2*time.Second) {
		t.Fatalf("timed out waiting for the Hold timer to register")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("ServeHTTP did not return promptly after request context cancellation")
	}

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500", rec.Code)
	}
	f := decodeFault(t, rec.Result())
	if f == nil || f.Code.Local != "E_SERVERSTATE" {
		t.Fatalf("got %+v, want E_SERVERSTATE", f)
	}
}

// TestHandleSubscribe_AfterBeginShutdown_ServerStateFault reproduces the
// gap where subscription.Manager.Create returning the raw
// context.Canceled it saw after BeginShutdown mapped to a generic E_FAIL —
// a shutting-down server is a server-state condition, and
// subscription.ErrShuttingDown now exists precisely so the server layer
// can report it as such.
func TestHandleSubscribe_AfterBeginShutdown_ServerStateFault(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	h.BeginShutdown()

	resp := postSOAP(t, h, subscribeRequestBody("Item1", "CIH1", false))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500", resp.StatusCode)
	}
	f := decodeFault(t, resp)
	if f == nil || f.Code.Local != "E_SERVERSTATE" {
		t.Fatalf("got %+v, want E_SERVERSTATE", f)
	}
}

func TestHandlePolledRefresh_InvalidHandle(t *testing.T) {
	be, _, _ := newMinimalBackend()
	h := newTestHandler(t, be, Config{}, clock.Real{})

	resp := postSOAP(t, h, polledRefreshRequestBody("bogus-handle", "", 0, false))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500", resp.StatusCode)
	}
	f := decodeFault(t, resp)
	if f == nil || f.Code.Local != "E_NOSUBSCRIPTION" {
		t.Fatalf("got %+v, want E_NOSUBSCRIPTION", f)
	}
}

func TestHandleSubscriptionCancel_RoundTrip(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	subResp := decodeResponse[xmlda.SubscribeResponse](t, postSOAP(t, h, subscribeRequestBody("Item1", "CIH1", false)))
	handle := subResp.ServerSubHandle

	resp := postSOAP(t, h, subscriptionCancelRequestBody(handle))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	got := decodeResponse[xmlda.SubscriptionCancelResponse](t, resp)
	_ = got

	// Cancelling again (already cancelled) must be a safe no-op, not an error.
	resp2 := postSOAP(t, h, subscriptionCancelRequestBody(handle))
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("double-cancel: got status %d, want 200", resp2.StatusCode)
	}

	// The now-cancelled handle must be reported invalid on a subsequent poll.
	resp3 := postSOAP(t, h, polledRefreshRequestBody(handle, "", 0, false))
	if resp3.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500 (E_NOSUBSCRIPTION for the cancelled handle)", resp3.StatusCode)
	}
}

// flakyPollReader is a controllable backend.Reader that returns Good
// samples until Fail() is called, after which every item resolves to
// E_UNKNOWNITEMNAME — modeling a poll-mode item that becomes unreadable
// partway through a subscription's life.
type flakyPollReader struct {
	mu     sync.Mutex
	values map[backend.ItemRef]xmlda.Value
	failed bool
}

func (r *flakyPollReader) Set(ref backend.ItemRef, v xmlda.Value) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.values == nil {
		r.values = map[backend.ItemRef]xmlda.Value{}
	}
	r.values[ref] = v
}

func (r *flakyPollReader) Fail() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = true
}

func (r *flakyPollReader) Read(ctx context.Context, items []backend.ReadRequestItem) ([]backend.Result[backend.ItemSample], error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]backend.Result[backend.ItemSample], len(items))
	for i, it := range items {
		if r.failed {
			out[i] = backend.Result[backend.ItemSample]{ResultID: xmlda.ErrUnknownItemName}
			continue
		}
		out[i] = backend.Result[backend.ItemSample]{Value: backend.ItemSample{Value: r.values[it.Ref], Quality: xmlda.NewGoodQuality()}}
	}
	return out, nil
}

// TestHandlePolledRefresh_ItemEntersErrorState_ValidResponseNoValue
// reproduces the bug fixed by RefreshItemResult.HaveSample: before it, a
// subscribed item reporting an abnormal ResultID (no sample) still had a
// wire Value built from its blank backend.ItemSample — a Good-quality,
// typeless value that then failed to encode, turning one failing item
// into a whole-operation E_FAIL for the entire subscription instead of a
// per-item condition in an otherwise-valid response.
func TestHandlePolledRefresh_ItemEntersErrorState_ValidResponseNoValue(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := &flakyPollReader{}
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	be := backend.Backend{Status: newTestStatus(), Reader: r}
	h, err := New(Deps{Backend: be, Clock: fake}, Config{ReapInterval: time.Hour, DefaultSamplingRate: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = h.Shutdown(ctx)
	})

	subResp := decodeResponse[xmlda.SubscribeResponse](t, doPostSOAP(h, subscribeRequestBody("Item1", "CIH1", false)))
	handle := subResp.ServerSubHandle

	r.Fail()
	fake.Advance(time.Second) // trigger a poll tick; the item now fails

	resp := doPostSOAP(h, polledRefreshRequestBody(handle, "", 0, false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200 (an item entering an error state must not break response encoding)", resp.StatusCode)
	}
	got := decodeResponse[xmlda.SubscriptionPolledRefreshResponse](t, resp)
	if len(got.RItemList) != 1 || len(got.RItemList[0].Items) != 1 {
		t.Fatalf("got %+v, want exactly one changed item", got.RItemList)
	}
	item := got.RItemList[0].Items[0]
	if item.ResultID != xmlda.ErrUnknownItemName {
		t.Fatalf("got ResultID=%v, want E_UNKNOWNITEMNAME", item.ResultID)
	}
	if item.Value != nil {
		t.Fatalf("got Value=%+v, want no Value alongside a reported error condition", item.Value)
	}
}

// TestServer_ShutdownDuringLongPoll confirms Handler.Shutdown unblocks an
// in-flight SubscriptionPolledRefresh call promptly, rather than waiting
// out the client's requested Hold time — the exact ordering documented in
// docs/architecture/subscription-model.md. Uses a fake clock so the "long
// hold" (30 minutes) never requires any real waiting.
func TestServer_ShutdownDuringLongPoll(t *testing.T) {
	fake := clocktest.New(testEpoch)
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	h, err := New(Deps{Backend: be, Clock: fake}, Config{MaxPolledRefreshWait: time.Hour})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	subResp := decodeResponse[xmlda.SubscribeResponse](t, doPostSOAP(h, subscribeRequestBody("Item1", "CIH1", false)))
	handle := subResp.ServerSubHandle

	holdTime := testEpoch.Add(30 * time.Minute).Format(time.RFC3339Nano)
	before := fake.PendingCount()

	done := make(chan *http.Response, 1)
	go func() {
		done <- doPostSOAP(h, polledRefreshRequestBody(handle, holdTime, 1000, false))
	}()

	if !fake.WaitForPending(before+1, 2*time.Second) {
		t.Fatalf("timed out waiting for the Hold timer to register")
	}

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		shutdownDone <- h.Shutdown(ctx)
	}()

	select {
	case resp := <-done:
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("got status %d, want 200 after shutdown unblocked the poll", resp.StatusCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("PolledRefresh did not unblock within 2s of Shutdown — shutdown must not wait out the client's Hold time")
	}

	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// TestHandleSubscribe_MalformedItemIsPerItemCondition pins the Subscribe
// path, where the rejected item must also be kept out of the subscription
// engine while still occupying its reply slot — the reply's item ORDER is
// how a client without ClientItemHandles matches items up at all.
func TestHandleSubscribe_MalformedItemIsPerItemCondition(t *testing.T) {
	be, _, r := newRWBackend(t)
	r.Set(backend.ItemRef{ItemName: "ok1"}, xmlda.NewInt32(1))
	r.Set(backend.ItemRef{ItemName: "ok2"}, xmlda.NewInt32(2))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	body := soapEnvelopeOpen + `<Subscribe xmlns="` + xmlda.Namespace + `" ReturnValuesOnReply="true">` +
		`<Options ReturnItemName="true"/><ItemList>` +
		`<Items ItemName="ok1" ClientItemHandle="H1"/>` +
		`<Items ItemName="bad" ClientItemHandle="HB" Deadband="not-a-float"/>` +
		`<Items ItemName="ok2" ClientItemHandle="H2"/>` +
		`</ItemList></Subscribe>` + soapEnvelopeClose

	resp := postSOAP(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	out := decodeResponse[xmlda.SubscribeResponse](t, resp)
	if out.ServerSubHandle == "" {
		t.Fatal("no subscription was created despite two valid items")
	}
	if len(out.RItemList.Items) != 3 {
		t.Fatalf("got %d reply items, want 3 in request order", len(out.RItemList.Items))
	}
	got := []string{
		out.RItemList.Items[0].ItemValue.ClientItemHandle,
		out.RItemList.Items[1].ItemValue.ClientItemHandle,
		out.RItemList.Items[2].ItemValue.ClientItemHandle,
	}
	want := []string{"H1", "HB", "H2"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("reply item %d has handle %q, want %q — request order was not preserved", i, got[i], want[i])
		}
	}
	if code := out.RItemList.Items[1].ItemValue.ResultID; code != xmlda.ErrFail {
		t.Errorf("the rejected item's ResultID = %v, want E_FAIL", code)
	}
	for _, i := range []int{0, 2} {
		iv := out.RItemList.Items[i].ItemValue
		if !iv.ResultID.IsZero() {
			t.Errorf("item %d: ResultID = %v, want none", i, iv.ResultID)
		}
		if iv.Value == nil {
			t.Errorf("item %d: ReturnValuesOnReply produced no value", i)
		}
	}
}

// TestHandleSubscribe_AllItemsMalformed_EmptyHandle pins the degenerate
// case: if every item is rejected at decode time there is nothing to
// subscribe, so no subscription is created and ServerSubHandle stays
// empty — the same outcome REQ-SUBSCRIPTION-002 defines for a request
// whose every item the backend rejected.
func TestHandleSubscribe_AllItemsMalformed_EmptyHandle(t *testing.T) {
	be, _, _ := newRWBackend(t)
	h := newTestHandler(t, be, Config{}, clock.Real{})

	body := soapEnvelopeOpen + `<Subscribe xmlns="` + xmlda.Namespace + `">` +
		`<Options/><ItemList>` +
		`<Items ItemName="a" Deadband="x"/><Items ItemName="b" MaxAge="y"/>` +
		`</ItemList></Subscribe>` + soapEnvelopeClose

	out := decodeResponse[xmlda.SubscribeResponse](t, postSOAP(t, h, body))
	if out.ServerSubHandle != "" {
		t.Errorf("ServerSubHandle = %q, want empty", out.ServerSubHandle)
	}
	if len(out.RItemList.Items) != 2 {
		t.Errorf("got %d reply items, want 2", len(out.RItemList.Items))
	}
}

// --- an offsetless HoldTime must not fault the request ---

// TestHandlePolledRefresh_OffsetlessHoldTime pins the widening end to end. The
// timezone offset is optional in xsd:dateTime and mandatory in RFC 3339,
// which time.Time.UnmarshalText enforces — so a conforming client could
// not poll its subscription at all.
func TestHandlePolledRefresh_OffsetlessHoldTime(t *testing.T) {
	be, _, r := newRWBackend(t)
	r.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{MaxPolledRefreshWait: 2 * time.Second}, clock.Real{})

	sub := decodeResponse[xmlda.SubscribeResponse](t, postSOAP(t, h, subscribeRequestBody("Item1", "CIH1", false)))

	// Deliberately no offset, and a short hold so the test stays fast.
	holdTime := time.Now().Add(80 * time.Millisecond).UTC().Format("2006-01-02T15:04:05.000")
	resp := postSOAP(t, h, polledRefreshRequestBody(sub.ServerSubHandle, holdTime, 0, true))
	if resp.StatusCode != http.StatusOK {
		f := decodeFault(t, resp)
		t.Fatalf("an offsetless HoldTime faulted the request: %+v", f)
	}
	out := decodeResponse[xmlda.SubscriptionPolledRefreshResponse](t, resp)
	if len(out.RItemList) != 1 {
		t.Errorf("got %d subscription result lists, want 1", len(out.RItemList))
	}
}

// --- an over-long HoldTime is clamped, not rejected ---

// TestHandlePolledRefresh_HoldTimeBeyondMaxWait_ClampsByDefault pins the clamping.
// The specification's guidance for HoldTime is a range ("generally no more
// than a minute or two", §3.1.6) while the ceiling is an exact number, so
// a client that reads that sentence and picks two minutes against a
// shorter ceiling used to fault on every single poll and never receive its
// subscription's data at all.
func TestHandlePolledRefresh_HoldTimeBeyondMaxWait_ClampsByDefault(t *testing.T) {
	be, _, r := newRWBackend(t)
	r.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{MaxPolledRefreshWait: 150 * time.Millisecond}, clock.Real{})

	sub := decodeResponse[xmlda.SubscribeResponse](t, postSOAP(t, h, subscribeRequestBody("Item1", "CIH1", false)))

	holdTime := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	start := time.Now()
	resp := postSOAP(t, h, polledRefreshRequestBody(sub.ServerSubHandle, holdTime, 0, true))
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an over-long HoldTime still faults by default: %+v", decodeFault(t, resp))
	}
	out := decodeResponse[xmlda.SubscriptionPolledRefreshResponse](t, resp)
	if len(out.RItemList) != 1 {
		t.Errorf("got %d subscription result lists, want 1", len(out.RItemList))
	}
	// It held for about the ceiling, not for the requested hour, and not
	// for no time at all.
	if elapsed > 5*time.Second {
		t.Errorf("the hold was not clamped: took %v", elapsed)
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("the hold was skipped entirely: took %v, want about the 150ms ceiling", elapsed)
	}
}

// --- Subscribe honors ReqType ---

// TestHandleSubscribe_HonorsReqType pins the fix for Subscribe having merged
// the hierarchical ReqType and then discarded it: a client subscribing an
// int item as xsd:double used to get int back, with neither the requested
// conversion nor the E_BADTYPE that would have said no.
func TestHandleSubscribe_HonorsReqType(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "IntItem"}, xmlda.NewInt32(7))
	h := newTestHandler(t, be, Config{}, nil)

	body := soapEnvelopeOpen +
		`<Subscribe xmlns="` + xmlda.Namespace + `" xmlns:xsd="` + xmlda.XSDNamespace + `" ` +
		`ReturnValuesOnReply="true"><Options ClientRequestHandle="CRH1"/>` +
		`<ItemList ReqType="xsd:double"><Items ItemName="IntItem" ClientItemHandle="CIH1"/></ItemList>` +
		`</Subscribe>` + soapEnvelopeClose
	got := decodeResponse[xmlda.SubscribeResponse](t, postSOAP(t, h, body))

	if len(got.RItemList.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(got.RItemList.Items))
	}
	v := got.RItemList.Items[0].ItemValue.Value
	if v == nil {
		t.Fatal("subscribed item carries no value")
	}
	if v.Type() != xmlda.TypeDouble {
		t.Fatalf("got type %q, want the requested xsd:double — ReqType was ignored", v.Type())
	}
	f, err := v.Float64()
	if err != nil || f != 7 {
		t.Fatalf("got %v (err %v), want 7", f, err)
	}
}

// TestHandleSubscribe_UnconvertibleReqTypeIsBadType is the other half: a type
// this server cannot convert to must be reported, not silently ignored.
func TestHandleSubscribe_UnconvertibleReqTypeIsBadType(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "StrItem"}, xmlda.NewString("not a number"))
	h := newTestHandler(t, be, Config{}, nil)

	body := soapEnvelopeOpen +
		`<Subscribe xmlns="` + xmlda.Namespace + `" xmlns:xsd="` + xmlda.XSDNamespace + `" ` +
		`ReturnValuesOnReply="true"><Options ClientRequestHandle="CRH1"/>` +
		`<ItemList><Items ItemName="StrItem" ClientItemHandle="CIH1" ReqType="xsd:int"/></ItemList>` +
		`</Subscribe>` + soapEnvelopeClose
	got := decodeResponse[xmlda.SubscribeResponse](t, postSOAP(t, h, body))

	if len(got.RItemList.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(got.RItemList.Items))
	}
	if id := got.RItemList.Items[0].ItemValue.ResultID; id != xmlda.ErrBadType {
		t.Fatalf("got ResultID %+v, want E_BADTYPE", id)
	}
}

// --- the server-wide subscribed-item budget ---

// TestHandleSubscribe_TotalItemBudget pins the fix for the per-axis limits
// multiplying: MaxConcurrentSubscriptions and MaxItemsPerSubscription
// together permitted a live item count neither limit alone suggests, with
// every item holding its own last sample.
func TestHandleSubscribe_TotalItemBudget(t *testing.T) {
	be, _, reader := newMinimalBackend()
	for _, n := range []string{"A", "B", "C"} {
		reader.Set(backend.ItemRef{ItemName: n}, xmlda.NewInt32(1))
	}
	h := newTestHandler(t, be, Config{MaxTotalSubscribedItems: 4}, nil)

	body := func(handle string) string {
		return soapEnvelopeOpen + `<Subscribe xmlns="` + xmlda.Namespace + `" ReturnValuesOnReply="false">` +
			`<Options ClientRequestHandle="` + handle + `"/><ItemList>` +
			`<Items ItemName="A"/><Items ItemName="B"/><Items ItemName="C"/>` +
			`</ItemList></Subscribe>` + soapEnvelopeClose
	}

	// First subscription: 3 items, within the budget of 4.
	first := decodeResponse[xmlda.SubscribeResponse](t, postSOAP(t, h, body("CRH1")))
	if first.ServerSubHandle == "" {
		t.Fatal("first Subscribe was rejected but should fit the budget")
	}

	// Second: 3 more would make 6, over the budget — rejected as a
	// whole-operation fault, not silently accepted.
	resp := postSOAP(t, h, body("CRH2"))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got HTTP %d, want a fault once the total item budget is exceeded", resp.StatusCode)
	}
	body2, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body2), "E_OUTOFMEMORY") {
		t.Fatalf("want an E_OUTOFMEMORY fault, got:\n%s", body2)
	}
}

// TestHandlePolledRefresh_NegativeWaitTime is the same for WaitTime, which
// needs a live subscription to reach.
func TestHandlePolledRefresh_NegativeWaitTime(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{}, nil)

	sub := decodeResponse[xmlda.SubscribeResponse](t,
		postSOAP(t, h, subscribeRequestBody("Item1", "CIH1", false)))
	if sub.ServerSubHandle == "" {
		t.Fatal("setup: no subscription handle")
	}
	resp := postSOAP(t, h, soapEnvelopeOpen+
		`<SubscriptionPolledRefresh xmlns="`+xmlda.Namespace+`" WaitTime="-1">`+
		`<Options ClientRequestHandle="CRH1"/>`+
		`<ServerSubHandles>`+sub.ServerSubHandle+`</ServerSubHandles>`+
		`</SubscriptionPolledRefresh>`+soapEnvelopeClose)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got HTTP %d, want 200 for a negative but schema-valid WaitTime:\n%s", resp.StatusCode, body)
	}
}
