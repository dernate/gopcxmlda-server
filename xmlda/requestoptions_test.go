package xmlda

import (
	"testing"
	"time"
)

func TestRequestOptions_DefaultsWhenUnset(t *testing.T) {
	var o RequestOptions
	if !o.ReturnErrorTextOrDefault() {
		t.Fatalf("ReturnErrorText default should be true")
	}
	if o.ReturnDiagnosticInfoOrDefault() {
		t.Fatalf("ReturnDiagnosticInfo default should be false")
	}
	if o.ReturnItemTimeOrDefault() {
		t.Fatalf("ReturnItemTime default should be false")
	}
	if o.ReturnItemPathOrDefault() {
		t.Fatalf("ReturnItemPath default should be false")
	}
	if o.ReturnItemNameOrDefault() {
		t.Fatalf("ReturnItemName default should be false")
	}
}

func TestRequestOptions_RoundTripExplicitValues(t *testing.T) {
	deadline := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	o := RequestOptions{
		ReturnErrorText:      new(bool), // explicit false, distinct from "unset" (default true)
		ReturnItemTime:       boolPtr(true),
		ReturnItemPath:       boolPtr(true),
		ReturnItemName:       boolPtr(true),
		ReturnDiagnosticInfo: boolPtr(true),
		RequestDeadline:      &deadline,
		ClientRequestHandle:  "CRH1",
		LocaleID:             "en-US",
	}
	out, err := xmlMarshalNamed(t, "Options", o)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got RequestOptions
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v\ndoc: %s", err, out)
	}
	if got.ReturnErrorTextOrDefault() {
		t.Fatalf("expected explicit false to survive round-trip, got true")
	}
	if !got.ReturnItemTimeOrDefault() || !got.ReturnItemPathOrDefault() || !got.ReturnItemNameOrDefault() || !got.ReturnDiagnosticInfoOrDefault() {
		t.Fatalf("got %+v, want all true", got)
	}
	if got.RequestDeadline == nil || !got.RequestDeadline.Equal(deadline) {
		t.Fatalf("RequestDeadline: got %v, want %v", got.RequestDeadline, deadline)
	}
	if got.ClientRequestHandle != "CRH1" || got.LocaleID != "en-US" {
		t.Fatalf("got %+v", got)
	}
}

func TestRequestOptions_UnsetFieldsOmittedOnEncode(t *testing.T) {
	var o RequestOptions
	out, err := xmlMarshalNamed(t, "Options", o)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got RequestOptions
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.ReturnErrorText != nil || got.ReturnItemTime != nil || got.RequestDeadline != nil {
		t.Fatalf("expected unset fields to remain nil after round-trip, got %+v", got)
	}
}

func boolPtr(b bool) *bool { return &b }
