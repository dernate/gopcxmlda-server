package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

func newWritableBackend() (backend.Backend, *testStatus, *testReader) {
	status := newTestStatus()
	reader := newTestReader()
	writer := &testWriter{reader: reader}
	return backend.Backend{Status: status, Reader: reader, Writer: writer}, status, reader
}

func TestHandleWrite_RoundTrip(t *testing.T) {
	be, _, reader := newWritableBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(0))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	resp := postSOAP(t, h, writeRequestBody("Item1", "int", "42", true))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	got := decodeResponse[xmlda.WriteResponse](t, resp)
	if len(got.RItemList.Items) != 1 || !got.RItemList.Items[0].ResultID.IsZero() {
		t.Fatalf("got %+v", got.RItemList.Items)
	}

	v, ok := reader.values[backend.ItemRef{ItemName: "Item1"}]
	if !ok {
		t.Fatalf("expected the backend value to be updated")
	}
	i32, err := v.Int32()
	if err != nil || i32 != 42 {
		t.Fatalf("got (%d, %v), want (42, nil)", i32, err)
	}
}

func TestHandleWrite_ReturnValuesOnReply(t *testing.T) {
	be, _, reader := newWritableBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(0))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	respTrue := decodeResponse[xmlda.WriteResponse](t, postSOAP(t, h, writeRequestBody("Item1", "int", "42", true)))
	if respTrue.RItemList.Items[0].Value == nil {
		t.Fatalf("expected Value to be echoed when ReturnValuesOnReply=true")
	}

	respFalse := decodeResponse[xmlda.WriteResponse](t, postSOAP(t, h, writeRequestBody("Item1", "int", "43", false)))
	if respFalse.RItemList.Items[0].Value != nil {
		t.Fatalf("expected no Value when ReturnValuesOnReply=false, got %+v", respFalse.RItemList.Items[0].Value)
	}
}

func TestHandleWrite_UnknownItem(t *testing.T) {
	be, _, _ := newWritableBackend()
	h := newTestHandler(t, be, Config{}, clock.Real{})

	got := decodeResponse[xmlda.WriteResponse](t, postSOAP(t, h, writeRequestBody("Unknown", "int", "1", false)))
	if got.RItemList.Items[0].ResultID != xmlda.ErrUnknownItemName {
		t.Fatalf("got %+v, want E_UNKNOWNITEMNAME", got.RItemList.Items[0].ResultID)
	}
}

func TestHandleWrite_ReadOnlyConfig_AccessDenied(t *testing.T) {
	be, _, reader := newWritableBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(0))
	h := newTestHandler(t, be, Config{ReadOnly: true}, clock.Real{})

	got := decodeResponse[xmlda.WriteResponse](t, postSOAP(t, h, writeRequestBody("Item1", "int", "42", false)))
	if got.RItemList.Items[0].ResultID != xmlda.ErrAccessDenied {
		t.Fatalf("got %+v, want E_ACCESS_DENIED", got.RItemList.Items[0].ResultID)
	}
	// The backend must not have been touched.
	v := reader.values[backend.ItemRef{ItemName: "Item1"}]
	i32, _ := v.Int32()
	if i32 != 0 {
		t.Fatalf("expected the backend value to remain unchanged under ReadOnly, got %d", i32)
	}
}

func TestHandleWrite_NilWriter_AccessDenied(t *testing.T) {
	status := newTestStatus()
	reader := newTestReader()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(0))
	be := backend.Backend{Status: status, Reader: reader} // no Writer at all
	h := newTestHandler(t, be, Config{}, clock.Real{})

	got := decodeResponse[xmlda.WriteResponse](t, postSOAP(t, h, writeRequestBody("Item1", "int", "42", false)))
	if got.RItemList.Items[0].ResultID != xmlda.ErrAccessDenied {
		t.Fatalf("got %+v, want E_ACCESS_DENIED", got.RItemList.Items[0].ResultID)
	}
}

// TestHandleWrite_MissingValueElement_NoPanic reproduces a Write item
// with no <Value> element at all — well-formed XML, but a semantic
// violation of REQ-WRITE-003. Must resolve to E_BADTYPE rather than
// dereferencing a nil *xmlda.Value and panicking the request goroutine
// (previously reachable with any number of requests against the Write
// endpoint).
func TestHandleWrite_MissingValueElement_NoPanic(t *testing.T) {
	be, _, reader := newWritableBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(0))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	body := soapEnvelopeOpen + `<Write xmlns="` + xmlda.Namespace + `" ReturnValuesOnReply="true">` +
		`<Options ClientRequestHandle="CRH1"/><ItemList><Items ItemName="Item1"></Items></ItemList></Write>` + soapEnvelopeClose
	resp := postSOAP(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200 (a per-item fault, not a whole-request failure)", resp.StatusCode)
	}
	got := decodeResponse[xmlda.WriteResponse](t, resp)
	if len(got.RItemList.Items) != 1 || got.RItemList.Items[0].ResultID != xmlda.ErrBadType {
		t.Fatalf("got %+v, want one item with E_BADTYPE", got.RItemList.Items)
	}
	// The backend must not have been called for this item at all.
	v := reader.values[backend.ItemRef{ItemName: "Item1"}]
	i32, _ := v.Int32()
	if i32 != 0 {
		t.Fatalf("expected the backend value to remain unchanged, got %d", i32)
	}
}

// shortResultWriter always returns one fewer Result than requested,
// modeling a non-conforming backend.Writer.
type shortResultWriter struct{}

func (shortResultWriter) Write(ctx context.Context, items []backend.WriteRequestItem) ([]backend.Result[backend.WriteOutcome], error) {
	if len(items) == 0 {
		return nil, nil
	}
	return make([]backend.Result[backend.WriteOutcome], len(items)-1), nil
}

// TestHandleWrite_ShortBackendResultSlice_NoPanic reproduces a backend
// that violates the "exactly one Result per requested item" contract
// (docs/backend-implementation.md) by returning a shorter slice. Must
// resolve the missing tail to E_FAIL rather than panicking with an
// out-of-range index.
func TestHandleWrite_ShortBackendResultSlice_NoPanic(t *testing.T) {
	status := newTestStatus()
	reader := newTestReader()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(0))
	reader.Set(backend.ItemRef{ItemName: "Item2"}, xmlda.NewInt32(0))
	be := backend.Backend{Status: status, Reader: reader, Writer: shortResultWriter{}}
	h := newTestHandler(t, be, Config{}, clock.Real{})

	body := soapEnvelopeOpen + `<Write xmlns="` + xmlda.Namespace + `" ReturnValuesOnReply="false">` +
		`<Options ClientRequestHandle="CRH1"/><ItemList>` +
		`<Items ItemName="Item1"><Value xmlns:xsd="` + xmlda.XSDNamespace + `" xmlns:xsi="` + xmlda.XSINamespace + `" xsi:type="xsd:int">1</Value></Items>` +
		`<Items ItemName="Item2"><Value xmlns:xsd="` + xmlda.XSDNamespace + `" xmlns:xsi="` + xmlda.XSINamespace + `" xsi:type="xsd:int">2</Value></Items>` +
		`</ItemList></Write>` + soapEnvelopeClose
	resp := postSOAP(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200 (no panic)", resp.StatusCode)
	}
	got := decodeResponse[xmlda.WriteResponse](t, resp)
	if len(got.RItemList.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(got.RItemList.Items))
	}
	if got.RItemList.Items[1].ResultID != xmlda.ErrFail {
		t.Fatalf("got %+v for the item the backend didn't return a Result for, want E_FAIL", got.RItemList.Items[1])
	}
}

// TestHandleWrite_ListLevelItemPath_AppliesToItemWithoutOwnPath
// reproduces the gap where handleWrite read only the per-item ItemPath,
// silently dropping a list-level one — a hierarchical parameter Write must
// honor exactly like Read and Subscribe (§3.1.1, REQ-READ-001). Without
// the fix, the write below resolves against ItemRef{ItemName: "Item1"}
// (no path), which is unknown, instead of the item actually registered at
// ItemPath "Folder1".
func TestHandleWrite_ListLevelItemPath_AppliesToItemWithoutOwnPath(t *testing.T) {
	be, _, reader := newWritableBackend()
	ref := backend.ItemRef{ItemName: "Item1", ItemPath: "Folder1"}
	reader.Set(ref, xmlda.NewInt32(0))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	body := soapEnvelopeOpen + `<Write xmlns="` + xmlda.Namespace + `" ReturnValuesOnReply="false">` +
		`<Options ClientRequestHandle="CRH1"/><ItemList ItemPath="Folder1">` +
		`<Items ItemName="Item1"><Value xmlns:xsd="` + xmlda.XSDNamespace + `" xmlns:xsi="` + xmlda.XSINamespace + `" xsi:type="xsd:int">7</Value></Items>` +
		`</ItemList></Write>` + soapEnvelopeClose
	got := decodeResponse[xmlda.WriteResponse](t, postSOAP(t, h, body))
	if len(got.RItemList.Items) != 1 || !got.RItemList.Items[0].ResultID.IsZero() {
		t.Fatalf("got %+v, want a successful write reaching Item1 at the list-level ItemPath", got.RItemList.Items)
	}
	v, ok := reader.values[ref]
	if !ok {
		t.Fatalf("expected the backend value at ItemPath=Folder1/Item1 to be updated — the write went to the wrong item")
	}
	i32, err := v.Int32()
	if err != nil || i32 != 7 {
		t.Fatalf("got (%d, %v), want (7, nil)", i32, err)
	}
}

func TestHandleWrite_EmptyItemList_Faults(t *testing.T) {
	be, _, _ := newWritableBackend()
	h := newTestHandler(t, be, Config{}, clock.Real{})

	body := soapEnvelopeOpen + `<Write xmlns="` + xmlda.Namespace + `" ReturnValuesOnReply="false"><Options/><ItemList></ItemList></Write>` + soapEnvelopeClose
	resp := postSOAP(t, h, body)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500", resp.StatusCode)
	}
}
