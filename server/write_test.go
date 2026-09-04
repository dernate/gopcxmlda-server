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

// TestHandleWrite_EmptyItemListIsAnEmptySuccess pins that an empty item
// list is served, not refused. Both <ItemList> and its <Items> are
// minOccurs="0" in the schema, and §3.3.1 only goes as far as "It is
// expected that there are one or more Items per ItemList" — expectation,
// not requirement. Faulting invented a rule the schema does not state,
// and a client assembling its list dynamically hits it for an entirely
// ordinary reason.
func TestHandleWrite_EmptyItemListIsAnEmptySuccess(t *testing.T) {
	be, _, _ := newRWBackend(t)
	h := newTestHandler(t, be, Config{}, clock.Real{})

	body := soapEnvelopeOpen + `<Write xmlns="` + xmlda.Namespace + `"><ItemList/></Write>` + soapEnvelopeClose
	resp := postSOAP(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	out := decodeResponseFrom[xmlda.WriteResponse](t, readBody(t, resp))
	if len(out.RItemList.Items) != 0 {
		t.Errorf("got %d items for an empty request, want 0", len(out.RItemList.Items))
	}
	if len(out.Errors) != 0 {
		t.Errorf("an empty request produced Errors entries: %+v", out.Errors)
	}
}

// TestHandleWrite_MalformedValueIsPerItemCondition pins the same for Write,
// where the malformed part is the item's <Value> content rather than an
// attribute — and asserts the healthy item was really written, i.e. that
// the rejected one was excluded from the backend call rather than sent
// with a zero value.
func TestHandleWrite_MalformedValueIsPerItemCondition(t *testing.T) {
	be, _, r := newRWBackend(t)
	r.Set(backend.ItemRef{ItemName: "ok"}, xmlda.NewInt32(0))
	r.Set(backend.ItemRef{ItemName: "bad"}, xmlda.NewInt32(0))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	body := soapEnvelopeOpen + `<Write xmlns="` + xmlda.Namespace + `"` +
		` xmlns:xsi="` + xmlda.XSINamespace + `" xmlns:xsd="` + xmlda.XSDNamespace + `">` +
		`<Options ReturnItemName="true"/><ItemList>` +
		`<Items ItemName="bad" ClientItemHandle="HB"><Value xsi:type="xsd:int">not-an-int</Value></Items>` +
		`<Items ItemName="ok" ClientItemHandle="HO"><Value xsi:type="xsd:int">42</Value></Items>` +
		`</ItemList></Write>` + soapEnvelopeClose

	resp := postSOAP(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	out := decodeResponse[xmlda.WriteResponse](t, resp)
	if got := itemByHandle(t, out.RItemList.Items, "HB").ResultID; got != xmlda.ErrBadType {
		t.Errorf("HB: ResultID = %v, want E_BADTYPE", got)
	}
	if got := itemByHandle(t, out.RItemList.Items, "HO").ResultID; !got.IsZero() {
		t.Errorf("HO: ResultID = %v, want none", got)
	}

	// The healthy write really landed.
	readResp := postSOAP(t, h, readRequestBody([]string{"ok"}))
	read := decodeResponse[xmlda.ReadResponse](t, readResp)
	v, err := read.RItemList.Items[0].Value.Int32()
	if err != nil || v != 42 {
		t.Errorf("the healthy item was not written: got %v (err %v), want 42", v, err)
	}
}

// TestHandleWrite_OffsetlessItemTimestamp pins the same widening on
// the Write path, where the offsetless dateTime is an item Timestamp.
func TestHandleWrite_OffsetlessItemTimestamp(t *testing.T) {
	be, _, r := newRWBackend(t)
	r.Set(backend.ItemRef{ItemName: "Tag"}, xmlda.NewFloat64(0))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	body := soapEnvelopeOpen + `<Write xmlns="` + xmlda.Namespace + `"` +
		` xmlns:xsi="` + xmlda.XSINamespace + `" xmlns:xsd="` + xmlda.XSDNamespace + `">` +
		`<Options/><ItemList><Items ItemName="Tag" Timestamp="2026-08-30T12:00:00">` +
		`<Value xsi:type="xsd:double">3.5</Value></Items></ItemList></Write>` + soapEnvelopeClose

	resp := postSOAP(t, h, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an offsetless item Timestamp faulted the Write: %+v", decodeFault(t, resp))
	}
	out := decodeResponse[xmlda.WriteResponse](t, resp)
	if got := out.RItemList.Items[0].ResultID; !got.IsZero() {
		t.Errorf("ResultID = %v, want none", got)
	}
}

// TestHandleWrite_SuccessfulAckStatesNoQuality pins the distinction
// between "no value available" and "no value requested".
//
// ReturnValuesOnReply defaults to false, so the ordinary Write returns
// items that carry no value at all. buildItemValue's explicit Bad quality
// exists for the Read case — an item that should have produced a value
// and could not, where a missing <Quality> would be read back as "good"
// via the schema default (§2.6 p.22). Applying it here told every
// successful write that its data was bad, contradicting the empty
// ResultID on the same element.
//
// Found by driving the server with NothinRandom/pyopcxmlda, which reads
// the first typed child of <Items> as the item's data type and so
// reported every successful write as type opc:OPCQuality.
func TestHandleWrite_SuccessfulAckStatesNoQuality(t *testing.T) {
	be, _, reader := newWritableBackend()
	reader.Set(backend.ItemRef{ItemName: "Item1"}, xmlda.NewInt32(0))
	h := newTestHandler(t, be, Config{}, clock.Real{})

	t.Run("success without values requested carries no Quality", func(t *testing.T) {
		got := decodeResponse[xmlda.WriteResponse](t, postSOAP(t, h, writeRequestBody("Item1", "int", "42", false)))
		item := got.RItemList.Items[0]
		if !item.ResultID.IsZero() {
			t.Fatalf("ResultID = %v, want empty", item.ResultID)
		}
		if item.Quality != nil {
			t.Errorf("Quality = %+v, want nil: the write succeeded and returned no value, "+
				"so there is no quality to state", *item.Quality)
		}
	})

	t.Run("a failed item still states Bad quality", func(t *testing.T) {
		// The §2.6 argument is untouched where it applies: this item has
		// a ResultID, and a client applying the schema default to a
		// missing <Quality> would read it as good.
		got := decodeResponse[xmlda.WriteResponse](t, postSOAP(t, h, writeRequestBody("Unknown", "int", "1", false)))
		item := got.RItemList.Items[0]
		if item.ResultID != xmlda.ErrUnknownItemName {
			t.Fatalf("ResultID = %v, want E_UNKNOWNITEMNAME", item.ResultID)
		}
		if item.Quality == nil {
			t.Fatal("Quality = nil, want an explicit Bad quality on a failed item")
		}
		if got := item.Quality.QualityField(); got != xmlda.QualityBad {
			t.Errorf("QualityField() = %v, want bad", got)
		}
	})

	t.Run("values requested carries the real quality", func(t *testing.T) {
		got := decodeResponse[xmlda.WriteResponse](t, postSOAP(t, h, writeRequestBody("Item1", "int", "44", true)))
		item := got.RItemList.Items[0]
		if item.Value == nil {
			t.Fatal("Value = nil, want the written value echoed back")
		}
		if item.Quality == nil {
			t.Fatal("Quality = nil, want the quality of the value being reported")
		}
		if got := item.Quality.QualityField(); got != xmlda.QualityGood {
			t.Errorf("QualityField() = %v, want good", got)
		}
	})
}
