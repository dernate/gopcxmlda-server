package xmlda

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dernate/gopcxmlda-server/soap"
)

func readTestdata(t *testing.T, parts ...string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(append([]string{".."}, parts...)...))
	if err != nil {
		t.Fatalf("reading %v: %v", parts, err)
	}
	return data
}

// TestSubscribeRequest_RealFixture decodes the real captured request
// testdata/requests/subscribe_679.request.xml and checks it matches the
// known content exactly (REQ-SUBSCRIPTION-001).
func TestSubscribeRequest_RealFixture(t *testing.T) {
	doc := readTestdata(t, "testdata", "requests", "subscribe_679.request.xml")
	var env soap.Envelope[SubscribeRequest]
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
	if !req.ReturnValuesOnReply {
		t.Fatalf("expected ReturnValuesOnReply=true")
	}
	if req.SubscriptionPingRate != 3000 {
		t.Fatalf("got SubscriptionPingRate=%d, want 3000", req.SubscriptionPingRate)
	}
	if req.Options.ClientRequestHandle != "CRH1" {
		t.Fatalf("got ClientRequestHandle=%q, want CRH1", req.Options.ClientRequestHandle)
	}
	if !req.Options.ReturnErrorTextOrDefault() || !req.Options.ReturnItemNameOrDefault() || !req.Options.ReturnItemPathOrDefault() {
		t.Fatalf("got Options=%+v, want ReturnErrorText/ReturnItemName/ReturnItemPath all true", req.Options)
	}
	wantItems := map[string]string{ // ClientItemHandle -> ItemName
		"CIH3": "ItemName1",
		"CIH1": "ItemName2",
		"CIH2": "ItemName3",
	}
	if len(req.ItemList.Items) != len(wantItems) {
		t.Fatalf("got %d items, want %d", len(req.ItemList.Items), len(wantItems))
	}
	for _, it := range req.ItemList.Items {
		want, ok := wantItems[it.ClientItemHandle]
		if !ok {
			t.Fatalf("unexpected ClientItemHandle %q", it.ClientItemHandle)
		}
		if it.ItemName != want {
			t.Fatalf("handle %s: got ItemName %q, want %q", it.ClientItemHandle, it.ItemName, want)
		}
	}
}

// TestSubscribeResponse_RealFixture decodes the real captured response
// testdata/responses/subscribe_680.response.xml and checks it matches the
// known content, including the ArrayOfUnsignedShort value
// (REQ-SUBSCRIPTION-002, REQ-TYPE-003).
func TestSubscribeResponse_RealFixture(t *testing.T) {
	doc := readTestdata(t, "testdata", "responses", "subscribe_680.response.xml")
	var env soap.Envelope[SubscribeResponse]
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
	if resp.ServerSubHandle != "Handle1" {
		t.Fatalf("got ServerSubHandle=%q, want Handle1", resp.ServerSubHandle)
	}
	if resp.Result.ClientRequestHandle != "Handle3" {
		t.Fatalf("got ClientRequestHandle=%q, want Handle3", resp.Result.ClientRequestHandle)
	}
	if resp.Result.RevisedLocaleID != "en-us" {
		t.Fatalf("got RevisedLocaleID=%q, want en-us", resp.Result.RevisedLocaleID)
	}
	if resp.Result.ServerState != ServerStateRunning {
		t.Fatalf("got ServerState=%q, want running", resp.Result.ServerState)
	}
	if resp.RItemList.RevisedSamplingRate != 0 {
		t.Fatalf("got list RevisedSamplingRate=%d, want 0", resp.RItemList.RevisedSamplingRate)
	}
	if len(resp.RItemList.Items) != 3 {
		t.Fatalf("got %d items, want 3", len(resp.RItemList.Items))
	}

	byHandle := map[string]SubscribeItemValue{}
	for _, it := range resp.RItemList.Items {
		byHandle[it.ItemValue.ClientItemHandle] = it
	}

	h4, ok := byHandle["Handle4"]
	if !ok {
		t.Fatalf("missing item with ClientItemHandle Handle4")
	}
	if h4.RevisedSamplingRate != 999 {
		t.Fatalf("Handle4: got RevisedSamplingRate=%d, want 999", h4.RevisedSamplingRate)
	}
	if h4.ItemValue.ItemName != "Name2" {
		t.Fatalf("Handle4: got ItemName=%q, want Name2", h4.ItemValue.ItemName)
	}
	f, err := h4.ItemValue.Value.Float32()
	if err != nil || f != 4.5 {
		t.Fatalf("Handle4: Value: got (%v, %v), want (4.5, nil)", f, err)
	}
	if !h4.ItemValue.Quality.IsGood() {
		t.Fatalf("Handle4: expected good quality")
	}

	h5, ok := byHandle["Handle5"]
	if !ok {
		t.Fatalf("missing item with ClientItemHandle Handle5")
	}
	if h5.ItemValue.ItemName != "Name1" {
		t.Fatalf("Handle5: got ItemName=%q, want Name1", h5.ItemValue.ItemName)
	}
	i32, err := h5.ItemValue.Value.Int32()
	if err != nil || i32 != 1234 {
		t.Fatalf("Handle5: Value: got (%v, %v), want (1234, nil)", i32, err)
	}

	h2, ok := byHandle["Handle2"]
	if !ok {
		t.Fatalf("missing item with ClientItemHandle Handle2")
	}
	if h2.RevisedSamplingRate != 99 {
		t.Fatalf("Handle2: got RevisedSamplingRate=%d, want 99", h2.RevisedSamplingRate)
	}
	if h2.ItemValue.ItemName != "Name3" {
		t.Fatalf("Handle2: got ItemName=%q, want Name3", h2.ItemValue.ItemName)
	}
	arr, err := h2.ItemValue.Value.Array()
	if err != nil {
		t.Fatalf("Handle2: Array: %v", err)
	}
	got, err := arr.Uint16s()
	if err != nil {
		t.Fatalf("Handle2: Uint16s: %v", err)
	}
	want := []uint16{0, 0, 3, 11, 0, 0}
	if len(got) != len(want) {
		t.Fatalf("Handle2: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Handle2: element %d: got %d, want %d", i, got[i], want[i])
		}
	}
}

// TestSubscribeResponse_RoundTrip re-encodes the decoded real fixture and
// decodes it again, asserting semantic (not byte-exact) equivalence.
func TestSubscribeResponse_RoundTrip(t *testing.T) {
	doc := readTestdata(t, "testdata", "responses", "subscribe_680.response.xml")
	var env1 soap.Envelope[SubscribeResponse]
	if err := Decode(doc, &env1); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	out, err := xmlMarshalNamed(t, "SubscribeResponse", *env1.Body.Content)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var resp2 SubscribeResponse
	if err := Decode(out, &resp2); err != nil {
		t.Fatalf("Decode (round-trip): %v\ndoc: %s", err, out)
	}
	if resp2.ServerSubHandle != env1.Body.Content.ServerSubHandle {
		t.Fatalf("ServerSubHandle: got %q, want %q", resp2.ServerSubHandle, env1.Body.Content.ServerSubHandle)
	}
	if len(resp2.RItemList.Items) != len(env1.Body.Content.RItemList.Items) {
		t.Fatalf("got %d items, want %d", len(resp2.RItemList.Items), len(env1.Body.Content.RItemList.Items))
	}
}

func TestSubscribeRequest_MarshalUnmarshalRoundTrip(t *testing.T) {
	rate := int32(1000)
	deadband := 2.5
	req := SubscribeRequest{
		ReturnValuesOnReply:  true,
		SubscriptionPingRate: 5000,
		Options: RequestOptions{
			ClientRequestHandle: "CRH1",
			ReturnItemName:      boolPtr(true),
		},
		ItemList: SubscribeItemList{
			Items: []SubscribeRequestItem{
				{
					ItemName:         "Item1",
					ClientItemHandle: "CIH1",
					Params: ItemParams{
						RequestedSamplingRate: &rate,
						Deadband:              &deadband,
					},
				},
			},
		},
	}
	out, err := xmlMarshalNamed(t, "Subscribe", req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got SubscribeRequest
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v\ndoc: %s", err, out)
	}
	if got.ReturnValuesOnReply != req.ReturnValuesOnReply || got.SubscriptionPingRate != req.SubscriptionPingRate {
		t.Fatalf("got %+v, want %+v", got, req)
	}
	if len(got.ItemList.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(got.ItemList.Items))
	}
	item := got.ItemList.Items[0]
	if item.ItemName != "Item1" || item.ClientItemHandle != "CIH1" {
		t.Fatalf("got %+v", item)
	}
	if item.Params.RequestedSamplingRate == nil || *item.Params.RequestedSamplingRate != 1000 {
		t.Fatalf("got RequestedSamplingRate=%v, want 1000", item.Params.RequestedSamplingRate)
	}
	if item.Params.Deadband == nil || *item.Params.Deadband != 2.5 {
		t.Fatalf("got Deadband=%v, want 2.5", item.Params.Deadband)
	}
}

func TestSubscribeResponse_EmptyServerSubHandleOnAllInvalid(t *testing.T) {
	// REQ-SUBSCRIPTION-002: empty ServerSubHandle signals "no subscription
	// created" when every requested item was invalid.
	resp := SubscribeResponse{
		ServerSubHandle: "",
		Result:          ReplyBase{ServerState: ServerStateRunning},
		Errors:          DedupeErrors([]ErrorCode{ErrUnknownItemName}, nil),
	}
	out, err := xmlMarshalNamed(t, "SubscribeResponse", resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got SubscribeResponse
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.ServerSubHandle != "" {
		t.Fatalf("got ServerSubHandle=%q, want empty", got.ServerSubHandle)
	}
	if len(got.Errors) != 1 || got.Errors[0].ID != ErrUnknownItemName {
		t.Fatalf("got Errors=%+v", got.Errors)
	}
}
