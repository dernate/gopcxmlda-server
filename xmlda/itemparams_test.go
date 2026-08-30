package xmlda

import (
	"testing"
)

func TestMergeItemParams_Precedence(t *testing.T) {
	reqPath, listPath, itemPath := "req", "list", "item"
	reqRate, itemRate := int32(100), int32(50)

	request := ItemParams{ItemPath: &reqPath, RequestedSamplingRate: &reqRate}
	list := ItemParams{ItemPath: &listPath}
	item := ItemParams{ItemPath: &itemPath, RequestedSamplingRate: &itemRate}

	got := MergeItemParams(request, list, item)
	if got.ItemPath == nil || *got.ItemPath != "item" {
		t.Fatalf("ItemPath: got %v, want item (most specific wins)", got.ItemPath)
	}
	if got.RequestedSamplingRate == nil || *got.RequestedSamplingRate != 50 {
		t.Fatalf("RequestedSamplingRate: got %v, want 50 (item overrides request)", got.RequestedSamplingRate)
	}
}

func TestMergeItemParams_InheritanceWhenNotOverridden(t *testing.T) {
	reqPath := "req"
	request := ItemParams{ItemPath: &reqPath}
	list := ItemParams{}
	item := ItemParams{}

	got := MergeItemParams(request, list, item)
	if got.ItemPath == nil || *got.ItemPath != "req" {
		t.Fatalf("ItemPath: got %v, want req (inherited, since list/item didn't override)", got.ItemPath)
	}
}

func TestMergeItemParams_AllNilStaysNil(t *testing.T) {
	got := MergeItemParams(ItemParams{}, ItemParams{}, ItemParams{})
	if got.ItemPath != nil || got.ReqType != nil || got.MaxAge != nil || got.Deadband != nil ||
		got.RequestedSamplingRate != nil || got.EnableBuffering != nil {
		t.Fatalf("expected all-nil when nothing set at any level, got %+v", got)
	}
}

func TestMergeItemParams_EmptyStringItemPathIsMeaningful(t *testing.T) {
	// §3.1.2: ItemPath="" is an explicit override, not "absent".
	got := MergeItemParams(ItemParams{ItemPath: strPtr("root")}, ItemParams{ItemPath: new(string)})
	if got.ItemPath == nil {
		t.Fatalf("expected a non-nil ItemPath pointer (explicit empty override)")
	}
	if *got.ItemPath != "" {
		t.Fatalf("got %q, want empty string", *got.ItemPath)
	}
}

func TestItemParamsAttrs_RoundTrip(t *testing.T) {
	path := "my/item"
	maxAge := int32(1000)
	deadband := 5.5
	rate := int32(2000)
	enable := true
	reqType := QName{Space: XSDNamespace, Local: "int"}

	p := ItemParams{
		ItemPath:              &path,
		ReqType:               &reqType,
		MaxAge:                &maxAge,
		Deadband:              &deadband,
		RequestedSamplingRate: &rate,
		EnableBuffering:       &enable,
	}

	attrs := encodeItemParamsAttrs(p)
	start := xmlStartElementFor("Items", attrs)
	doc, err := xmlMarshalStart(t, start)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got := decodeItemParamsFromDoc(t, doc)
	if got.ItemPath == nil || *got.ItemPath != path {
		t.Fatalf("ItemPath: got %v, want %v", got.ItemPath, path)
	}
	if got.ReqType == nil || *got.ReqType != reqType {
		t.Fatalf("ReqType: got %v, want %v", got.ReqType, reqType)
	}
	if got.MaxAge == nil || *got.MaxAge != maxAge {
		t.Fatalf("MaxAge: got %v, want %v", got.MaxAge, maxAge)
	}
	if got.Deadband == nil || *got.Deadband != deadband {
		t.Fatalf("Deadband: got %v, want %v", got.Deadband, deadband)
	}
	if got.RequestedSamplingRate == nil || *got.RequestedSamplingRate != rate {
		t.Fatalf("RequestedSamplingRate: got %v, want %v", got.RequestedSamplingRate, rate)
	}
	if got.EnableBuffering == nil || *got.EnableBuffering != enable {
		t.Fatalf("EnableBuffering: got %v, want %v", got.EnableBuffering, enable)
	}
}

func strPtr(s string) *string { return &s }
