package xmlda

import (
	"strconv"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/soap"
)

func TestReadRequest_RoundTrip(t *testing.T) {
	maxAge := int32(500)
	req := ReadRequest{
		Options: RequestOptions{ClientRequestHandle: "CRH1", ReturnItemTime: boolPtr(true)},
		ItemList: ReadItemList{
			Items: []ReadRequestItem{
				{ItemName: "Item1", ClientItemHandle: "CIH1", Params: ItemParams{MaxAge: &maxAge}},
				{ItemName: "Item2", ClientItemHandle: "CIH2"},
			},
		},
	}
	out, err := xmlMarshalNamed(t, "Read", req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ReadRequest
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v\ndoc: %s", err, out)
	}
	if len(got.ItemList.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(got.ItemList.Items))
	}
	if got.ItemList.Items[0].Params.MaxAge == nil || *got.ItemList.Items[0].Params.MaxAge != 500 {
		t.Fatalf("got MaxAge=%v, want 500", got.ItemList.Items[0].Params.MaxAge)
	}
	if !got.Options.ReturnItemTimeOrDefault() {
		t.Fatalf("expected ReturnItemTime=true")
	}
}

func TestReadRequest_EmptyItemList(t *testing.T) {
	// REQ-READ-002: an empty item list is representable at the wire level;
	// rejecting it with E_FAIL is a server-layer concern (WP-9), not this
	// type's — this test only confirms the wire shape decodes cleanly.
	req := ReadRequest{}
	out, err := xmlMarshalNamed(t, "Read", req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ReadRequest
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got.ItemList.Items) != 0 {
		t.Fatalf("got %d items, want 0", len(got.ItemList.Items))
	}
}

func TestReadResponse_OrderPreservedAndPerItemErrors(t *testing.T) {
	ts := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	v1 := NewInt32(1)
	resp := ReadResponse{
		Result: ReplyBase{RcvTime: ts, ReplyTime: ts, ServerState: ServerStateRunning},
		RItemList: ItemValueList{
			Items: []ItemValue{
				{ItemName: "A", Value: &v1, Quality: NewGoodQuality()},
				{ItemName: "B", ResultID: ErrUnknownItemName, Quality: NewQuality(QualityBad, LimitNone, 0)},
			},
		},
		Errors: DedupeErrors([]ErrorCode{ErrUnknownItemName}, nil),
	}
	out, err := xmlMarshalNamed(t, "ReadResponse", resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ReadResponse
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v\ndoc: %s", err, out)
	}
	if len(got.RItemList.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(got.RItemList.Items))
	}
	if got.RItemList.Items[0].ItemName != "A" || got.RItemList.Items[1].ItemName != "B" {
		t.Fatalf("order not preserved: got %q, %q", got.RItemList.Items[0].ItemName, got.RItemList.Items[1].ItemName)
	}
	if !got.RItemList.Items[0].ResultID.IsZero() {
		t.Fatalf("item A should have no ResultID, got %+v", got.RItemList.Items[0].ResultID)
	}
	if got.RItemList.Items[1].ResultID != ErrUnknownItemName {
		t.Fatalf("item B: got ResultID=%+v, want E_UNKNOWNITEMNAME", got.RItemList.Items[1].ResultID)
	}
	if len(got.Errors) != 1 || got.Errors[0].ID != ErrUnknownItemName {
		t.Fatalf("got Errors=%+v", got.Errors)
	}
}

// TestReadRequest_RealFixture decodes the real captured request
// testdata/requests/read_649.request.xml — 19 items in one call
// (REQ-READ-001, REQ-READ-002).
func TestReadRequest_RealFixture(t *testing.T) {
	doc := readTestdata(t, "testdata", "requests", "read_649.request.xml")
	var env soap.Envelope[ReadRequest]
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
	if !req.Options.ReturnItemNameOrDefault() || !req.Options.ReturnItemTimeOrDefault() || !req.Options.ReturnErrorTextOrDefault() {
		t.Fatalf("got Options=%+v, want ReturnItemName/ReturnItemTime/ReturnErrorText all true", req.Options)
	}
	if len(req.ItemList.Items) != 19 {
		t.Fatalf("got %d items, want 19", len(req.ItemList.Items))
	}
	for i, it := range req.ItemList.Items {
		want := "Item" + strconv.Itoa(i+1)
		if it.ItemName != want || it.ClientItemHandle != want {
			t.Fatalf("item %d: got ItemName=%q ClientItemHandle=%q, want both %q", i, it.ItemName, it.ClientItemHandle, want)
		}
	}
}

// readDeepFixtureItems is the anonymized shape of the 19 items in
// testdata/responses/read_676.response.xml, in document order — a spread
// of scalar types (unsignedShort/string/dateTime/unsignedInt/unsignedByte)
// each carrying a per-item Timestamp (REQ-READ-003, REQ-QUALITY-005).
var readDeepFixtureItems = []struct {
	name string
	typ  ScalarType
	want string // canonical text form; dateTime values compared via time.Parse
	ts   string // RFC3339Nano
}{
	{"Item1", TypeUnsignedShort, "2065", "2026-07-17T13:20:06.000+00:00"},
	{"Item2", TypeString, "CS82", "2026-07-17T13:20:06.000+00:00"},
	{"Item3", TypeDateTime, "2020-04-27T22:00:00.000+00:00", "2026-07-17T13:20:06.000+00:00"},
	{"Item4", TypeUnsignedInt, "827553", "2026-07-17T13:20:06.000+00:00"},
	{"Item5", TypeUnsignedInt, "69", "2026-07-17T13:20:06.000+00:00"},
	{"Item6", TypeDateTime, "2011-03-30T22:00:00.000-01:00", "2026-07-17T13:20:06.000+00:00"},
	{"Item7", TypeUnsignedInt, "823191", "2026-07-17T13:20:06.000+00:00"},
	{"Item8", TypeDateTime, "2011-03-30T22:00:00.000-01:00", "2026-07-17T13:20:06.000+00:00"},
	{"Item9", TypeUnsignedShort, "2065", "2026-07-17T13:20:06.000+00:00"},
	{"Item10", TypeUnsignedInt, "823192", "2026-07-17T13:20:06.000+00:00"},
	{"Item11", TypeUnsignedShort, "2370", "2026-07-17T13:20:06.000+00:00"},
	{"Item12", TypeUnsignedShort, "9", "2026-07-17T13:20:06.000+00:00"},
	{"Item13", TypeUnsignedByte, "17", "2026-07-17T13:20:06.000+00:00"},
	{"Item14", TypeString, "CS82", "2026-07-17T13:20:06.000+00:00"},
	{"Item15", TypeUnsignedByte, "17", "2026-07-17T13:20:06.000+00:00"},
	{"Item16", TypeString, "Example Site", "2026-07-17T13:20:06.000+00:00"},
	{"Item17", TypeUnsignedInt, "6500", "2026-08-24T16:12:27.207+00:00"},
	{"Item18", TypeUnsignedByte, "17", "2026-07-17T13:20:06.000+00:00"},
	{"Item19", TypeString, "CS82", "2026-07-17T13:20:06.000+00:00"},
}

// TestReadResponse_RealFixture decodes the real captured response
// testdata/responses/read_676.response.xml (REQ-READ-003, REQ-QUALITY-005).
func TestReadResponse_RealFixture(t *testing.T) {
	doc := readTestdata(t, "testdata", "responses", "read_676.response.xml")
	var env soap.Envelope[ReadResponse]
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
	if resp.Result.ServerState != ServerStateRunning {
		t.Fatalf("got ServerState=%q, want running", resp.Result.ServerState)
	}
	if len(resp.RItemList.Items) != len(readDeepFixtureItems) {
		t.Fatalf("got %d items, want %d", len(resp.RItemList.Items), len(readDeepFixtureItems))
	}
	for i, want := range readDeepFixtureItems {
		it := resp.RItemList.Items[i]
		if it.ItemName != want.name || it.ClientItemHandle != want.name {
			t.Fatalf("item %d: got ItemName=%q ClientItemHandle=%q, want both %q", i, it.ItemName, it.ClientItemHandle, want.name)
		}
		if it.Value == nil {
			t.Fatalf("item %d (%s): nil Value", i, want.name)
		}
		if it.Value.Type() != want.typ {
			t.Fatalf("item %d (%s): got Type=%v, want %v", i, want.name, it.Value.Type(), want.typ)
		}
		wantTS, err := time.Parse(time.RFC3339Nano, want.ts)
		if err != nil {
			t.Fatalf("bad test timestamp %q: %v", want.ts, err)
		}
		if it.Timestamp == nil || !it.Timestamp.Equal(wantTS) {
			t.Fatalf("item %d (%s): got Timestamp=%v, want %v", i, want.name, it.Timestamp, wantTS)
		}
		switch want.typ {
		case TypeDateTime:
			wantVal, err := time.Parse(time.RFC3339Nano, want.want)
			if err != nil {
				t.Fatalf("bad test dateTime %q: %v", want.want, err)
			}
			gotVal, err := it.Value.Time()
			if err != nil || !gotVal.Equal(wantVal) {
				t.Fatalf("item %d (%s): got Time=%v, err=%v, want %v", i, want.name, gotVal, err, wantVal)
			}
		case TypeString:
			gotVal, err := it.Value.String()
			if err != nil || gotVal != want.want {
				t.Fatalf("item %d (%s): got String=%q, err=%v, want %q", i, want.name, gotVal, err, want.want)
			}
		case TypeUnsignedShort:
			gotVal, err := it.Value.Uint16()
			if err != nil || strconv.Itoa(int(gotVal)) != want.want {
				t.Fatalf("item %d (%s): got Uint16=%d, err=%v, want %q", i, want.name, gotVal, err, want.want)
			}
		case TypeUnsignedInt:
			gotVal, err := it.Value.Uint32()
			if err != nil || strconv.Itoa(int(gotVal)) != want.want {
				t.Fatalf("item %d (%s): got Uint32=%d, err=%v, want %q", i, want.name, gotVal, err, want.want)
			}
		case TypeUnsignedByte:
			gotVal, err := it.Value.Uint8()
			if err != nil || strconv.Itoa(int(gotVal)) != want.want {
				t.Fatalf("item %d (%s): got Uint8=%d, err=%v, want %q", i, want.name, gotVal, err, want.want)
			}
		}
	}
}

// TestReadRequest_RealFixture_ArrayOfDouble decodes the real captured
// request testdata/requests/read_169.request.xml — a single deeply-nested
// (7-segment) item path (REQ-READ-001).
func TestReadRequest_RealFixture_ArrayOfDouble(t *testing.T) {
	doc := readTestdata(t, "testdata", "requests", "read_169.request.xml")
	var env soap.Envelope[ReadRequest]
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
	if len(req.ItemList.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(req.ItemList.Items))
	}
	want := "Folder1/Folder2/Folder3/Folder4/Folder5/Folder6/Item1"
	if req.ItemList.Items[0].ItemName != want || req.ItemList.Items[0].ClientItemHandle != want {
		t.Fatalf("got ItemName=%q ClientItemHandle=%q, want both %q", req.ItemList.Items[0].ItemName, req.ItemList.Items[0].ClientItemHandle, want)
	}
}

// TestReadResponse_RealFixture_ArrayOfDouble decodes the real captured
// response testdata/responses/read_182.response.xml — the only real
// fixture exercising ArrayOfDouble/xsd:double, incl. negative values and
// full float64 precision artifacts (e.g. 5.4000000000000004) (REQ-TYPE-001,
// REQ-TYPE-003).
func TestReadResponse_RealFixture_ArrayOfDouble(t *testing.T) {
	doc := readTestdata(t, "testdata", "responses", "read_182.response.xml")
	var env soap.Envelope[ReadResponse]
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
	if len(resp.RItemList.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(resp.RItemList.Items))
	}
	it := resp.RItemList.Items[0]
	want := "Folder1/Folder2/Folder3/Folder4/Folder5/Folder6/Item1"
	if it.ItemName != want || it.ClientItemHandle != want {
		t.Fatalf("got ItemName=%q ClientItemHandle=%q, want both %q", it.ItemName, it.ClientItemHandle, want)
	}
	if it.Value == nil {
		t.Fatalf("nil Value")
	}
	arr, err := it.Value.Array()
	if err != nil {
		t.Fatalf("Array: %v", err)
	}
	if arr.ElemType() != TypeDouble {
		t.Fatalf("got ElemType=%v, want double", arr.ElemType())
	}
	wantVals := []float64{10075, 5.4000000000000004, 14.300000000000001, 10.300000000000001, 21.32,
		275, 2396, 155, 45370, -2, -69, 406, -191, 334, 310, 322, 287}
	gotVals, err := arr.Float64s()
	if err != nil {
		t.Fatalf("Float64s: %v", err)
	}
	if len(gotVals) != len(wantVals) {
		t.Fatalf("got %d values, want %d", len(gotVals), len(wantVals))
	}
	for i, want := range wantVals {
		if gotVals[i] != want {
			t.Fatalf("value %d: got %v, want %v", i, gotVals[i], want)
		}
	}
}
