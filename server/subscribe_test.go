package server

import (
	"context"
	"net/http"
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
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got status %d, want 400", resp.StatusCode)
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
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got status %d, want 400", resp.StatusCode)
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
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got status %d, want 400", resp.StatusCode)
	}
	f := decodeFault(t, resp)
	if f == nil || f.Code.Local != "E_INVALIDHOLDTIME" {
		t.Fatalf("got %+v, want E_INVALIDHOLDTIME", f)
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
