package xmlda

import (
	"strconv"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/soap"
)

func TestSubscriptionPolledRefreshRequest_RoundTrip(t *testing.T) {
	hold := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	req := SubscriptionPolledRefreshRequest{
		HoldTime:         &hold,
		WaitTime:         500,
		ReturnAllItems:   false,
		Options:          RequestOptions{ClientRequestHandle: "CRH1"},
		ServerSubHandles: []string{"Handle1", "Handle2"},
	}
	out, err := xmlMarshalNamed(t, "SubscriptionPolledRefresh", req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got SubscriptionPolledRefreshRequest
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v\ndoc: %s", err, out)
	}
	if got.HoldTime == nil || !got.HoldTime.Equal(hold) {
		t.Fatalf("HoldTime: got %v, want %v", got.HoldTime, hold)
	}
	if got.WaitTime != 500 {
		t.Fatalf("WaitTime: got %d, want 500", got.WaitTime)
	}
	if len(got.ServerSubHandles) != 2 || got.ServerSubHandles[0] != "Handle1" || got.ServerSubHandles[1] != "Handle2" {
		t.Fatalf("got %v", got.ServerSubHandles)
	}
}

func TestSubscriptionPolledRefreshRequest_NoHoldTime(t *testing.T) {
	// REQ-SUBSCRIPTION-005: if HoldTime is absent, WaitTime is ignored.
	// This test only checks the wire shape: nil HoldTime must round-trip
	// as nil, not panic (a real Go/encoding-xml gotcha caught in WP-4 —
	// see requestoptions.go's comment on RequestDeadline).
	req := SubscriptionPolledRefreshRequest{WaitTime: 0, ServerSubHandles: []string{"H1"}}
	out, err := xmlMarshalNamed(t, "SubscriptionPolledRefresh", req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got SubscriptionPolledRefreshRequest
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.HoldTime != nil {
		t.Fatalf("expected nil HoldTime, got %v", got.HoldTime)
	}
}

func TestSubscriptionPolledRefreshResponse_MultiSubscriptionAndOverflow(t *testing.T) {
	resp := SubscriptionPolledRefreshResponse{
		DataBufferOverflow:      true,
		Result:                  ReplyBase{ServerState: ServerStateRunning},
		InvalidServerSubHandles: []string{"UnknownHandle"},
		RItemList: []SubscriptionPolledRefreshReplyItemList{
			{SubscriptionHandle: "Handle1", Items: []ItemValue{{ItemName: "A"}}},
			{SubscriptionHandle: "Handle2", Items: []ItemValue{{ItemName: "B"}, {ItemName: "C"}}},
		},
	}
	out, err := xmlMarshalNamed(t, "SubscriptionPolledRefreshResponse", resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got SubscriptionPolledRefreshResponse
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v\ndoc: %s", err, out)
	}
	if !got.DataBufferOverflow {
		t.Fatalf("expected DataBufferOverflow=true")
	}
	if len(got.InvalidServerSubHandles) != 1 || got.InvalidServerSubHandles[0] != "UnknownHandle" {
		t.Fatalf("got %v", got.InvalidServerSubHandles)
	}
	if len(got.RItemList) != 2 {
		t.Fatalf("got %d RItemList entries, want 2", len(got.RItemList))
	}
	if got.RItemList[0].SubscriptionHandle != "Handle1" || len(got.RItemList[0].Items) != 1 {
		t.Fatalf("got %+v", got.RItemList[0])
	}
	if got.RItemList[1].SubscriptionHandle != "Handle2" || len(got.RItemList[1].Items) != 2 {
		t.Fatalf("got %+v", got.RItemList[1])
	}
}

func TestSubscriptionPolledRefreshResponse_NoChangesMeansNoEntry(t *testing.T) {
	// A subscription with nothing changed (ReturnAllItems=false) simply
	// has no RItemList entry at all — this test confirms an empty
	// RItemList round-trips as empty, not as a spurious entry.
	resp := SubscriptionPolledRefreshResponse{Result: ReplyBase{ServerState: ServerStateRunning}}
	out, err := xmlMarshalNamed(t, "SubscriptionPolledRefreshResponse", resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got SubscriptionPolledRefreshResponse
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got.RItemList) != 0 {
		t.Fatalf("got %d entries, want 0", len(got.RItemList))
	}
}

// TestSubscriptionPolledRefreshRequest_RealFixture decodes the real
// captured request testdata/requests/subscriptionpolledrefresh_226.request.xml
// (REQ-SUBSCRIPTION-004, REQ-SUBSCRIPTION-005).
func TestSubscriptionPolledRefreshRequest_RealFixture(t *testing.T) {
	doc := readTestdata(t, "testdata", "requests", "subscriptionpolledrefresh_226.request.xml")
	var env soap.Envelope[SubscriptionPolledRefreshRequest]
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
	wantHold := time.Date(2026, 8, 24, 18, 12, 18, 0, time.UTC)
	if req.HoldTime == nil || !req.HoldTime.Equal(wantHold) {
		t.Fatalf("got HoldTime=%v, want %v", req.HoldTime, wantHold)
	}
	if req.WaitTime != 500 {
		t.Fatalf("got WaitTime=%d, want 500", req.WaitTime)
	}
	if req.ReturnAllItems {
		t.Fatalf("expected ReturnAllItems=false")
	}
	if !req.Options.ReturnErrorTextOrDefault() || !req.Options.ReturnItemTimeOrDefault() {
		t.Fatalf("got Options=%+v, want ReturnErrorText/ReturnItemTime both true", req.Options)
	}
	if len(req.ServerSubHandles) != 1 || req.ServerSubHandles[0] != "163121520" {
		t.Fatalf("got ServerSubHandles=%v, want [163121520]", req.ServerSubHandles)
	}
}

// polledRefreshFixtureItems is the anonymized shape of the 31 changed items
// in testdata/responses/subscriptionpolledrefresh_232.response.xml, in
// document order — a spread of numeric scalar types including negative
// int and float values (REQ-SUBSCRIPTION-006, REQ-TYPE-001).
var polledRefreshFixtureItems = []struct {
	name string
	typ  ScalarType
	want string
	ts   string
}{
	{"Item1", TypeFloat, "306", "2026-08-24T16:12:16.726+00:00"},
	{"Item2", TypeFloat, "5.5", "2026-08-24T16:12:16.686+00:00"},
	{"Item3", TypeInt, "303", "2026-08-24T16:12:16.726+00:00"},
	{"Item4", TypeFloat, "1.3399999", "2026-08-24T16:12:16.686+00:00"},
	{"Item5", TypeInt, "439", "2026-08-24T16:12:16.133+00:00"},
	{"Item6", TypeUnsignedInt, "306", "2026-08-24T16:12:16.726+00:00"},
	{"Item7", TypeInt, "279", "2026-08-24T16:12:16.686+00:00"},
	{"Item8", TypeInt, "279", "2026-08-24T16:12:16.686+00:00"},
	{"Item9", TypeInt, "306", "2026-08-24T16:12:16.133+00:00"},
	{"Item10", TypeFloat, "5.0999999", "2026-08-24T16:12:16.686+00:00"},
	{"Item11", TypeUnsignedInt, "585", "2026-08-24T16:12:16.726+00:00"},
	{"Item12", TypeInt, "-22", "2026-08-24T16:12:16.726+00:00"},
	{"Item13", TypeInt, "440", "2026-08-24T16:12:16.133+00:00"},
	{"Item14", TypeFloat, "0.99737448", "2026-08-24T16:12:16.133+00:00"},
	{"Item15", TypeFloat, "49.98", "2026-08-24T16:12:16.133+00:00"},
	{"Item16", TypeFloat, "49.959999", "2026-08-24T16:12:16.686+00:00"},
	{"Item17", TypeInt, "439", "2026-08-24T16:12:16.133+00:00"},
	{"Item18", TypeUnsignedInt, "23889741", "2026-08-24T16:12:16.133+00:00"},
	{"Item19", TypeFloat, "11.57", "2026-08-24T16:12:16.133+00:00"},
	{"Item20", TypeInt, "303", "2026-08-24T16:12:16.133+00:00"},
	{"Item21", TypeInt, "-22", "2026-08-24T16:12:16.133+00:00"},
	{"Item22", TypeFloat, "-3.4000001", "2026-08-24T16:12:16.133+00:00"},
	{"Item23", TypeUnsignedInt, "585", "2026-08-24T16:12:16.726+00:00"},
	{"Item24", TypeFloat, "0.9993434", "2026-08-24T16:12:16.726+00:00"},
	{"Item25", TypeInt, "306", "2026-08-24T16:12:16.133+00:00"},
	{"Item26", TypeInt, "306", "2026-08-24T16:12:16.133+00:00"},
	{"Item27", TypeUnsignedInt, "230", "2026-08-24T16:12:16.133+00:00"},
	{"Item28", TypeFloat, "5.4500003", "2026-08-24T16:12:16.726+00:00"},
	{"Item29", TypeInt, "306", "2026-08-24T16:12:16.133+00:00"},
	{"Item30", TypeInt, "279", "2026-08-24T16:12:16.686+00:00"},
	{"Item31", TypeUnsignedInt, "585", "2026-08-24T16:12:16.726+00:00"},
}

// TestSubscriptionPolledRefreshResponse_RealFixture decodes the real
// captured response
// testdata/responses/subscriptionpolledrefresh_232.response.xml
// (REQ-SUBSCRIPTION-006, REQ-SUBSCRIPTION-012).
func TestSubscriptionPolledRefreshResponse_RealFixture(t *testing.T) {
	doc := readTestdata(t, "testdata", "responses", "subscriptionpolledrefresh_232.response.xml")
	var env soap.Envelope[SubscriptionPolledRefreshResponse]
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
	if resp.DataBufferOverflow {
		t.Fatalf("expected DataBufferOverflow=false")
	}
	if resp.Result.ServerState != ServerStateRunning {
		t.Fatalf("got ServerState=%q, want running", resp.Result.ServerState)
	}
	if len(resp.RItemList) != 1 {
		t.Fatalf("got %d RItemList entries, want 1", len(resp.RItemList))
	}
	list := resp.RItemList[0]
	if list.SubscriptionHandle != "163121520" {
		t.Fatalf("got SubscriptionHandle=%q, want 163121520", list.SubscriptionHandle)
	}
	if len(list.Items) != len(polledRefreshFixtureItems) {
		t.Fatalf("got %d items, want %d", len(list.Items), len(polledRefreshFixtureItems))
	}
	for i, want := range polledRefreshFixtureItems {
		it := list.Items[i]
		if it.ClientItemHandle != want.name {
			t.Fatalf("item %d: got ClientItemHandle=%q, want %q", i, it.ClientItemHandle, want.name)
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
		case TypeFloat:
			wantF, err := strconv.ParseFloat(want.want, 32)
			if err != nil {
				t.Fatalf("bad test float %q: %v", want.want, err)
			}
			gotVal, err := it.Value.Float32()
			if err != nil || gotVal != float32(wantF) {
				t.Fatalf("item %d (%s): got Float32=%v, err=%v, want %v", i, want.name, gotVal, err, float32(wantF))
			}
		case TypeInt:
			wantI, err := strconv.ParseInt(want.want, 10, 32)
			if err != nil {
				t.Fatalf("bad test int %q: %v", want.want, err)
			}
			gotVal, err := it.Value.Int32()
			if err != nil || int64(gotVal) != wantI {
				t.Fatalf("item %d (%s): got Int32=%v, err=%v, want %v", i, want.name, gotVal, err, wantI)
			}
		case TypeUnsignedInt:
			wantU, err := strconv.ParseUint(want.want, 10, 32)
			if err != nil {
				t.Fatalf("bad test uint %q: %v", want.want, err)
			}
			gotVal, err := it.Value.Uint32()
			if err != nil || uint64(gotVal) != wantU {
				t.Fatalf("item %d (%s): got Uint32=%v, err=%v, want %v", i, want.name, gotVal, err, wantU)
			}
		}
	}
}
