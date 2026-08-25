package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// shortResultReader always returns one fewer Result than requested,
// modeling a non-conforming backend.Reader. The items it does cover get
// a real value, not a zero-value placeholder — this test's only concern
// is the missing tail, not incidentally exercising a separately-invalid
// "successful but empty" Result.
type shortResultReader struct{}

func (shortResultReader) Read(ctx context.Context, items []backend.ReadRequestItem) ([]backend.Result[backend.ItemSample], error) {
	if len(items) == 0 {
		return nil, nil
	}
	out := make([]backend.Result[backend.ItemSample], len(items)-1)
	for i := range out {
		out[i] = backend.Result[backend.ItemSample]{Value: backend.ItemSample{Value: xmlda.NewInt32(int32(i)), Quality: xmlda.NewGoodQuality(), Timestamp: testEpoch}}
	}
	return out, nil
}

// TestHandleRead_ShortBackendResultSlice_NoPanic reproduces a backend
// that violates the "exactly one Result per requested item" contract
// (docs/backend-implementation.md). Must resolve the missing tail to
// E_FAIL rather than panicking with an out-of-range index.
func TestHandleRead_ShortBackendResultSlice_NoPanic(t *testing.T) {
	be := backend.Backend{Status: newTestStatus(), Reader: shortResultReader{}}
	h := newTestHandler(t, be, Config{}, clock.Real{})

	resp := postSOAP(t, h, readRequestBody([]string{"Item1", "Item2"}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200 (no panic)", resp.StatusCode)
	}
	got := decodeResponse[xmlda.ReadResponse](t, resp)
	if len(got.RItemList.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(got.RItemList.Items))
	}
	if got.RItemList.Items[1].ResultID != xmlda.ErrFail {
		t.Fatalf("got %+v for the item the backend didn't return a Result for, want E_FAIL", got.RItemList.Items[1])
	}
}

func TestHandleRead_RoundTrip(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(42))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	resp := postSOAP(t, h, readRequestBody([]string{"Item1"}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	got := decodeResponse[xmlda.ReadResponse](t, resp)
	if len(got.RItemList.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(got.RItemList.Items))
	}
	item := got.RItemList.Items[0]
	if item.Value == nil {
		t.Fatalf("expected a non-nil Value")
	}
	i32, err := item.Value.Int32()
	if err != nil || i32 != 42 {
		t.Fatalf("got (%d, %v), want (42, nil)", i32, err)
	}
	if !item.ResultID.IsZero() {
		t.Fatalf("expected no error for a valid item, got %+v", item.ResultID)
	}
}

func TestHandleRead_OrderPreservedAcrossMultipleItems(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "A"}, xmlda.NewInt32(1))
	reader.Set(backend.ItemRef{ItemName: "B"}, xmlda.NewInt32(2))
	reader.Set(backend.ItemRef{ItemName: "C"}, xmlda.NewInt32(3))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	resp := postSOAP(t, h, readRequestBody([]string{"A", "B", "C"}))
	got := decodeResponse[xmlda.ReadResponse](t, resp)
	if len(got.RItemList.Items) != 3 {
		t.Fatalf("got %d items, want 3", len(got.RItemList.Items))
	}
	for i, want := range []int32{1, 2, 3} {
		v, err := got.RItemList.Items[i].Value.Int32()
		if err != nil || v != want {
			t.Fatalf("item %d: got (%d, %v), want (%d, nil)", i, v, err, want)
		}
	}
}

func TestHandleRead_PartialSuccess(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Good"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	resp := postSOAP(t, h, readRequestBody([]string{"Good", "Bad"}))
	got := decodeResponse[xmlda.ReadResponse](t, resp)
	if len(got.RItemList.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(got.RItemList.Items))
	}
	if !got.RItemList.Items[0].ResultID.IsZero() {
		t.Fatalf("item 0 (Good) should have no error, got %+v", got.RItemList.Items[0].ResultID)
	}
	if got.RItemList.Items[1].ResultID != xmlda.ErrUnknownItemName {
		t.Fatalf("item 1 (Bad): got %+v, want E_UNKNOWNITEMNAME", got.RItemList.Items[1].ResultID)
	}
	if len(got.Errors) != 1 || got.Errors[0].ID != xmlda.ErrUnknownItemName {
		t.Fatalf("got Errors=%+v", got.Errors)
	}
}

func TestHandleRead_EmptyItemList_Faults(t *testing.T) {
	be, _, _ := newMinimalBackend()
	h := newTestHandler(t, be, Config{}, clock.Real{})

	resp := postSOAP(t, h, readRequestBody(nil))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got status %d, want 400", resp.StatusCode)
	}
	f := decodeFault(t, resp)
	if f == nil {
		t.Fatalf("expected a fault for an empty item list")
	}
}

func TestHandleRead_ItemCountLimit(t *testing.T) {
	be, _, reader := newMinimalBackend()
	names := make([]string, 5)
	for i := range names {
		names[i] = "Item"
		reader.Set(backend.ItemRef{ItemName: "Item"}, xmlda.NewInt32(1))
	}
	h := newTestHandler(t, be, Config{MaxItemsPerRequest: 3}, clock.Real{})

	resp := postSOAP(t, h, readRequestBody(names))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got status %d, want 400", resp.StatusCode)
	}
	f := decodeFault(t, resp)
	if f == nil || f.Code.Local != "E_OUTOFMEMORY" {
		t.Fatalf("got %+v, want E_OUTOFMEMORY", f)
	}
}

func TestHandleRead_ReturnItemNameGating(t *testing.T) {
	be, _, reader := newMinimalBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(1))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	// readRequestBody's Options never sets ReturnItemName, so it must
	// default to false and the response must not echo ItemName.
	resp := postSOAP(t, h, readRequestBody([]string{"Item1"}))
	got := decodeResponse[xmlda.ReadResponse](t, resp)
	if got.RItemList.Items[0].ItemName != "" {
		t.Fatalf("expected ItemName to be omitted by default, got %q", got.RItemList.Items[0].ItemName)
	}
}
