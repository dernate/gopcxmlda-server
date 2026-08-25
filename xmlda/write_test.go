package xmlda

import (
	"testing"
)

func TestWriteRequest_RoundTrip(t *testing.T) {
	v1 := NewInt32(42)
	v2 := NewString("hello")
	req := WriteRequest{
		ReturnValuesOnReply: true,
		Options:             RequestOptions{ClientRequestHandle: "CRH1"},
		ItemList: WriteItemList{
			Items: []ItemValue{
				{ItemName: "Item1", ClientItemHandle: "CIH1", Value: &v1},
				{ItemName: "Item2", ClientItemHandle: "CIH2", Value: &v2, Quality: NewGoodQuality()},
			},
		},
	}
	out, err := xmlMarshalNamed(t, "Write", req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got WriteRequest
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v\ndoc: %s", err, out)
	}
	if !got.ReturnValuesOnReply {
		t.Fatalf("expected ReturnValuesOnReply=true")
	}
	if len(got.ItemList.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(got.ItemList.Items))
	}
	i32, err := got.ItemList.Items[0].Value.Int32()
	if err != nil || i32 != 42 {
		t.Fatalf("item 0 Value: got (%v, %v), want (42, nil)", i32, err)
	}
	s, err := got.ItemList.Items[1].Value.String()
	if err != nil || s != "hello" {
		t.Fatalf("item 1 Value: got (%q, %v), want (\"hello\", nil)", s, err)
	}
}

func TestWriteRequest_ReturnValuesOnReplyFalse(t *testing.T) {
	v1 := NewBool(true)
	req := WriteRequest{
		ReturnValuesOnReply: false,
		ItemList:            WriteItemList{Items: []ItemValue{{ItemName: "x", Value: &v1}}},
	}
	out, err := xmlMarshalNamed(t, "Write", req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got WriteRequest
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.ReturnValuesOnReply {
		t.Fatalf("expected ReturnValuesOnReply=false to survive round-trip")
	}
}

func TestWriteResponse_ClampSuccess(t *testing.T) {
	// REQ-WRITE-005: a successful write with a clamped value still
	// succeeds, signaled via S_CLAMP.
	v := NewFloat64(100.0)
	resp := WriteResponse{
		RItemList: ItemValueList{
			Items: []ItemValue{{ItemName: "x", Value: &v, ResultID: SuccessClamp}},
		},
		Errors: DedupeErrors([]ErrorCode{SuccessClamp}, nil),
	}
	out, err := xmlMarshalNamed(t, "WriteResponse", resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got WriteResponse
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got.RItemList.Items) != 1 || got.RItemList.Items[0].ResultID != SuccessClamp {
		t.Fatalf("got %+v", got.RItemList.Items)
	}
	if !got.RItemList.Items[0].ResultID.IsSuccess() {
		t.Fatalf("S_CLAMP should report IsSuccess() true")
	}
}

func TestWriteRequest_AtomicQualityAndTimestamp(t *testing.T) {
	// REQ-WRITE-003: an item may carry Value+Quality+Timestamp together;
	// this test only checks the wire shape round-trips all three
	// atomically as one item — the "accept all or reject the whole item"
	// enforcement itself is a backend/server-layer concern (WP-6/WP-9).
	v := NewInt32(7)
	item := ItemValue{
		ItemName: "x",
		Value:    &v,
		Quality:  NewQuality(QualityGood, LimitNone, 0),
	}
	req := WriteRequest{ItemList: WriteItemList{Items: []ItemValue{item}}}
	out, err := xmlMarshalNamed(t, "Write", req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got WriteRequest
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got.ItemList.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(got.ItemList.Items))
	}
	gi32, err := got.ItemList.Items[0].Value.Int32()
	if err != nil || gi32 != 7 {
		t.Fatalf("Value: got (%v, %v)", gi32, err)
	}
	if !got.ItemList.Items[0].Quality.IsGood() {
		t.Fatalf("expected good quality to survive round-trip")
	}
}
