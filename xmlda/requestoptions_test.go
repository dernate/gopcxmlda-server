package xmlda

import (
	"encoding/xml"
	"strings"
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

// TestRequestOptions_RequestDeadlineWithoutOffset pins the same widening
// for RequestDeadline, and that the round trip re-emits this library's
// canonical wire form rather than time.Time's own MarshalText output.
func TestRequestOptions_RequestDeadlineWithoutOffset(t *testing.T) {
	doc := `<Read xmlns="` + Namespace + `"><Options RequestDeadline="2026-08-30T12:00:00" LocaleID="de-DE"/><ItemList/></Read>`
	var req ReadRequest
	if err := Decode([]byte(doc), &req); err != nil {
		t.Fatalf("an offsetless RequestDeadline still fails the whole request: %v", err)
	}
	if req.Options.RequestDeadline == nil {
		t.Fatal("RequestDeadline was dropped")
	}
	if want := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC); !req.Options.RequestDeadline.Equal(want) {
		t.Errorf("RequestDeadline = %v, want %v", req.Options.RequestDeadline, want)
	}
	if req.Options.LocaleID != "de-DE" {
		t.Errorf("LocaleID = %q", req.Options.LocaleID)
	}

	out, err := xml.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `RequestDeadline="2026-08-30T12:00:00.000Z"`) {
		t.Errorf("RequestDeadline was not re-emitted in the canonical wire form: %s", out)
	}
}

// TestEmptyDateTimeAttributesAreAbsent pins the tolerance that made
// NothinRandom/pyopcxmlda's subscriptions reachable at all.
//
// xs:dateTime has no empty lexical form, so none of these attributes is
// schema-valid — but a client that assembles requests from string
// templates emits every attribute it knows and leaves the unset ones
// empty, and pyopcxmlda does exactly that on every Subscribe
// (RequestDeadline="") and every SubscriptionPolledRefresh (HoldTime="").
// Every request-side dateTime attribute in this protocol is optional, so
// "unset" is the only reading an empty one can have; faulting instead
// cost the client the whole operation.
//
// The absence has to be a real absence, not the zero time: a HoldTime of
// January year 1 is a hold that has already expired, and a
// RequestDeadline of January year 1 is a request that is already too
// late — both worse than the fault they replaced.
func TestEmptyDateTimeAttributesAreAbsent(t *testing.T) {
	t.Run("RequestOptions.RequestDeadline", func(t *testing.T) {
		doc := `<Read xmlns="` + Namespace + `"><Options RequestDeadline="" LocaleID="en-US"/><ItemList/></Read>`
		var req ReadRequest
		if err := Decode([]byte(doc), &req); err != nil {
			t.Fatalf("an empty RequestDeadline faulted the whole request: %v", err)
		}
		if req.Options.RequestDeadline != nil {
			t.Errorf("RequestDeadline = %v, want nil", *req.Options.RequestDeadline)
		}
		// The rest of the element must still decode.
		if req.Options.LocaleID != "en-US" {
			t.Errorf("LocaleID = %q, want en-US", req.Options.LocaleID)
		}
	})

	t.Run("SubscriptionPolledRefresh.HoldTime", func(t *testing.T) {
		doc := `<SubscriptionPolledRefresh xmlns="` + Namespace + `" HoldTime="" WaitTime="2000">` +
			`<ServerSubHandles>h1</ServerSubHandles></SubscriptionPolledRefresh>`
		var req SubscriptionPolledRefreshRequest
		if err := Decode([]byte(doc), &req); err != nil {
			t.Fatalf("an empty HoldTime faulted the whole request: %v", err)
		}
		if req.HoldTime != nil {
			t.Errorf("HoldTime = %v, want nil", *req.HoldTime)
		}
		if req.WaitTime != 2000 {
			t.Errorf("WaitTime = %d, want 2000", req.WaitTime)
		}
	})

	t.Run("ItemValue.Timestamp", func(t *testing.T) {
		doc := `<Items xmlns="` + Namespace + `" xmlns:xsi="` + XSINamespace + `" xmlns:xsd="` + XSDNamespace +
			`" ItemName="Tag" Timestamp=""><Value xsi:type="xsd:int">7</Value></Items>`
		var iv ItemValue
		if err := Decode([]byte(doc), &iv); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if iv.DecodeErr != nil {
			t.Fatalf("DecodeErr = %v, want nil: an empty Timestamp must not cost the item", iv.DecodeErr)
		}
		if iv.Timestamp != nil {
			t.Errorf("Timestamp = %v, want nil", *iv.Timestamp)
		}
		if iv.Value == nil {
			t.Error("Value was dropped along with the timestamp")
		}
	})

	t.Run("a non-empty malformed value still fails", func(t *testing.T) {
		// The tolerance is for emptiness only. Actual garbage in a
		// dateTime attribute must keep failing, or a typo becomes a
		// silently ignored deadline.
		doc := `<Read xmlns="` + Namespace + `"><Options RequestDeadline="not-a-date"/><ItemList/></Read>`
		var req ReadRequest
		if err := Decode([]byte(doc), &req); err == nil {
			t.Errorf("RequestDeadline=%q decoded cleanly as %v, want an error", "not-a-date", req.Options.RequestDeadline)
		}
	})
}
