package xmlda

import (
	"testing"
	"time"
)

func TestItemValue_RoundTrip(t *testing.T) {
	path := "Loc/Item"
	ts := time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC)
	value := NewFloat64(4.5)
	iv := ItemValue{
		ItemName:         "Loc/Item.Value",
		ItemPath:         &path,
		ClientItemHandle: "CIH1",
		Value:            &value,
		Quality:          NewQuality(QualityUncertain, LimitHigh, 3),
		Timestamp:        &ts,
		ResultID:         SuccessUnsupportedRate,
		DiagnosticInfo:   "sampling rate reduced",
	}
	out, err := xmlMarshalNamed(t, "Items", iv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ItemValue
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v\ndoc: %s", err, out)
	}
	if got.ItemName != iv.ItemName {
		t.Fatalf("ItemName: got %q, want %q", got.ItemName, iv.ItemName)
	}
	if got.ItemPath == nil || *got.ItemPath != path {
		t.Fatalf("ItemPath: got %v, want %v", got.ItemPath, path)
	}
	if got.ClientItemHandle != iv.ClientItemHandle {
		t.Fatalf("ClientItemHandle: got %q, want %q", got.ClientItemHandle, iv.ClientItemHandle)
	}
	if got.Value == nil {
		t.Fatalf("expected non-nil Value")
	}
	f, err := got.Value.Float64()
	if err != nil || f != 4.5 {
		t.Fatalf("Value: got (%v, %v), want (4.5, nil)", f, err)
	}
	if got.Quality.QualityField() != QualityUncertain || got.Quality.LimitField() != LimitHigh || got.Quality.VendorField() != 3 {
		t.Fatalf("Quality: got %+v", got.Quality)
	}
	if got.Timestamp == nil || !got.Timestamp.Equal(ts) {
		t.Fatalf("Timestamp: got %v, want %v", got.Timestamp, ts)
	}
	if got.ResultID != iv.ResultID {
		t.Fatalf("ResultID: got %+v, want %+v", got.ResultID, iv.ResultID)
	}
	if got.DiagnosticInfo != iv.DiagnosticInfo {
		t.Fatalf("DiagnosticInfo: got %q, want %q", got.DiagnosticInfo, iv.DiagnosticInfo)
	}
}

func TestItemValue_AbsentValue(t *testing.T) {
	// e.g. a write-only item on Read, or Bad quality with no last-known
	// value: no <Value> element at all.
	iv := ItemValue{
		ItemName: "Loc/WriteOnly",
		Quality:  NewQuality(QualityBad, LimitNone, 0),
		ResultID: ErrWriteOnly,
	}
	out, err := xmlMarshalNamed(t, "Items", iv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ItemValue
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v\ndoc: %s", err, out)
	}
	if got.Value != nil {
		t.Fatalf("expected nil Value, got %+v", got.Value)
	}
	if !got.Quality.IsBad() {
		t.Fatalf("expected bad quality")
	}
	if got.ResultID != ErrWriteOnly {
		t.Fatalf("ResultID: got %+v, want %+v", got.ResultID, ErrWriteOnly)
	}
}

func TestItemValue_NoTimestampWhenNotRequested(t *testing.T) {
	value := NewInt32(1)
	iv := ItemValue{ItemName: "x", Value: &value}
	out, err := xmlMarshalNamed(t, "Items", iv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ItemValue
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Timestamp != nil {
		t.Fatalf("expected nil Timestamp, got %v", got.Timestamp)
	}
}

// TestItemValue_RealFixture decodes the first <Items><ItemValue>...
// element from testdata/responses/subscribe_680.response.xml and checks
// it matches the known real values.
func TestItemValue_RealFixture(t *testing.T) {
	doc := []byte(`<ItemValue xmlns:xsi="` + XSINamespace + `" xmlns:xsd="` + XSDNamespace + `" xmlns:ns1="` + Namespace + `" ClientItemHandle="Handle4" ItemName="Name2" ItemPath="" xsi:type="ns1:ItemValue"><Value xsi:type="xsd:float">4.5</Value><Quality LimitField="none" QualityField="good" VendorField="0" xsi:type="ns1:OPCQuality"/></ItemValue>`)
	var got ItemValue
	if err := Decode(doc, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.ClientItemHandle != "Handle4" {
		t.Fatalf("ClientItemHandle: got %q", got.ClientItemHandle)
	}
	if got.ItemName != "Name2" {
		t.Fatalf("ItemName: got %q", got.ItemName)
	}
	if got.ItemPath == nil || *got.ItemPath != "" {
		t.Fatalf("ItemPath: got %v, want a non-nil pointer to empty string", got.ItemPath)
	}
	if got.Value == nil {
		t.Fatalf("expected non-nil Value")
	}
	f, err := got.Value.Float32()
	if err != nil || f != 4.5 {
		t.Fatalf("Value: got (%v, %v), want (4.5, nil)", f, err)
	}
	if !got.Quality.IsGood() {
		t.Fatalf("expected good quality")
	}
}

func TestItemValue_ValueTypeQualifierTolerance(t *testing.T) {
	// A peer using the dateTime+ValueTypeQualifier interop encoding for
	// "time" (OQ-12) must still be interpreted correctly.
	doc := []byte(`<Items xmlns:xsi="` + XSINamespace + `" xmlns:xsd="` + XSDNamespace + `" ValueTypeQualifier="xsd:time"><Value xsi:type="xsd:dateTime">2024-01-01T13:45:00Z</Value></Items>`)
	var got ItemValue
	if err := Decode(doc, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Value == nil {
		t.Fatalf("expected non-nil Value")
	}
	if got.Value.Type() != TypeTime {
		t.Fatalf("got Type()=%s, want time (reinterpreted via ValueTypeQualifier)", got.Value.Type())
	}
	tm, err := got.Value.Time()
	if err != nil {
		t.Fatalf("Time: %v", err)
	}
	want := time.Date(2024, 1, 1, 13, 45, 0, 0, time.UTC)
	if !tm.Equal(want) {
		t.Fatalf("got %v, want %v", tm, want)
	}
}
