package xmlda

import (
	"encoding/xml"
	"errors"
	"strings"
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
		Quality:          qualityPtr(NewQuality(QualityUncertain, LimitHigh, 3)),
		Timestamp:        &ts,
		ResultID:         SuccessUnsupportedRate,
		DiagnosticInfo:   strPtrIV("sampling rate reduced"),
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
	if got.DiagnosticInfo == nil || iv.DiagnosticInfo == nil || *got.DiagnosticInfo != *iv.DiagnosticInfo {
		t.Fatalf("DiagnosticInfo: got %v, want %v", got.DiagnosticInfo, iv.DiagnosticInfo)
	}
}

func TestItemValue_AbsentValue(t *testing.T) {
	// e.g. a write-only item on Read, or Bad quality with no last-known
	// value: no <Value> element at all.
	iv := ItemValue{
		ItemName: "Loc/WriteOnly",
		Quality:  qualityPtr(NewQuality(QualityBad, LimitNone, 0)),
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

// TestItemValue_TimestampWithoutOffset pins the Write-request path: a
// client writing Value+Timestamp together (REQ-WRITE-003's atomic triple)
// with an offsetless timestamp used to lose the entire Write.
func TestItemValue_TimestampWithoutOffset(t *testing.T) {
	doc := `<Items xmlns:xsi="` + XSINamespace + `" xmlns:xsd="` + XSDNamespace + `"` +
		` ItemName="Tag" Timestamp="2026-08-30T12:00:00">` +
		`<Value xsi:type="xsd:double">3.5</Value></Items>`
	var iv ItemValue
	if err := Decode([]byte(doc), &iv); err != nil {
		t.Fatalf("an offsetless item Timestamp still fails the decode: %v", err)
	}
	if iv.DecodeErr != nil {
		t.Fatalf("unexpected DecodeErr: %v", iv.DecodeErr)
	}
	if iv.Timestamp == nil {
		t.Fatal("Timestamp was dropped")
	}
	if want := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC); !iv.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %v, want %v", iv.Timestamp, want)
	}
	if iv.Value == nil {
		t.Fatal("Value was dropped")
	}
}

// --- an item with no sample must not assert a quality ---

// TestItemValue_QualityOmittedWhenNil pins that <Quality> is left out
// entirely when the item carries none. The zero OPCQuality emits no
// attributes, which under the schema's own defaults
// (QualityField="good") a conforming client reads as good quality — so a
// failing item was reported as good-quality-with-no-value, contradicting
// its own ResultID on the same element.
func TestItemValue_QualityOmittedWhenNil(t *testing.T) {
	failed := ItemValue{ItemName: "Unknown", ResultID: ErrUnknownItemName}
	out, err := xmlMarshalNamed(t, "Items", failed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "Quality") {
		t.Errorf("an item with no sample still emits a Quality element: %s", out)
	}

	ok := ItemValue{ItemName: "Good", Quality: qualityPtr(NewQuality(QualityUncertain, LimitHigh, 2))}
	out, err = xmlMarshalNamed(t, "Items", ok)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `QualityField="uncertain"`) {
		t.Errorf("a real quality was not emitted: %s", out)
	}
}

// TestItemValue_QualityNilVsEmptyElement pins the decode-side half: nil
// distinguishes "no <Quality> element" from "<Quality/> with no
// attributes". The previous value-typed field could not draw that line
// (OPCQuality's own fields are pointers, so comparing against the zero
// value compared pointer identity), so a client explicitly writing
// good/none/0 had its quality silently dropped.
func TestItemValue_QualityNilVsEmptyElement(t *testing.T) {
	var absent ItemValue
	if err := Decode([]byte(`<Items ItemName="A"/>`), &absent); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if absent.Quality != nil {
		t.Error("an item with no <Quality> element decoded to a non-nil Quality")
	}
	if got := absent.QualityOrDefault().QualityField(); got != QualityGood {
		t.Errorf("QualityOrDefault = %v, want the wire default good", got)
	}

	var present ItemValue
	if err := Decode([]byte(`<Items ItemName="A"><Quality/></Items>`), &present); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if present.Quality == nil {
		t.Fatal("an explicit empty <Quality/> decoded to nil: the client's intent was dropped")
	}
	if got := present.Quality.QualityField(); got != QualityGood {
		t.Errorf("QualityField = %v, want good", got)
	}
}

// --- H1: DiagnosticInfo is an element, and children are ordered ---

// TestItemValue_DiagnosticInfoIsElementInSequence pins the fix for
// DiagnosticInfo having been emitted as an attribute (the schema declares
// an element) and for Quality having preceded Value (the schema's
// xsd:sequence is DiagnosticInfo, Value, Quality).
func TestItemValue_DiagnosticInfoIsElementInSequence(t *testing.T) {
	v := NewInt32(42)
	out, err := xmlMarshalNamed(t, "Items", ItemValue{
		ItemName: "A", Value: &v, Quality: qualityPtr(NewGoodQuality()), DiagnosticInfo: strPtrIV("why it failed"),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	doc := string(out)

	if strings.Contains(doc, `DiagnosticInfo="`) {
		t.Errorf("DiagnosticInfo was emitted as an attribute; the schema declares an element: %s", doc)
	}
	if !strings.Contains(doc, "<DiagnosticInfo>why it failed</DiagnosticInfo>") {
		t.Errorf("DiagnosticInfo element missing: %s", doc)
	}
	di := strings.Index(doc, "<DiagnosticInfo>")
	val := strings.Index(doc, "<Value")
	qual := strings.Index(doc, "<Quality")
	if !(di < val && val < qual) {
		t.Errorf("children are out of schema sequence order (want DiagnosticInfo, Value, Quality): %s", doc)
	}
}

// TestItemValue_DecodesDiagnosticInfoBothWays confirms the decode-side
// tolerance: this library emitted the attribute form itself until the
// encoder was corrected, so a peer or a stored document may still carry
// it.
func TestItemValue_DecodesDiagnosticInfoBothWays(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"element", `<Items xmlns="` + Namespace + `"><DiagnosticInfo>d</DiagnosticInfo></Items>`},
		{"attribute", `<Items xmlns="` + Namespace + `" DiagnosticInfo="d"/>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var iv ItemValue
			doc, err := NewDocument([]byte(tc.body))
			if err != nil {
				t.Fatalf("NewDocument: %v", err)
			}
			if err := doc.Decode(&iv); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if iv.DiagnosticInfo == nil || *iv.DiagnosticInfo != "d" {
				t.Fatalf("got DiagnosticInfo %v, want %q", iv.DiagnosticInfo, "d")
			}
		})
	}
}

// --- RItemList carries the type annotation its siblings do ---

// TestItemValueList_CarriesTypeAndReserved pins the fix for RItemList
// having been the one wrapper type emitted without xsi:type, while
// ReplyBase, ItemValue and OPCQuality all declared theirs for the
// documented benefit of strict and .NET-generated clients. The real
// captured traffic emits both this and Reserved.
func TestItemValueList_CarriesTypeAndReserved(t *testing.T) {
	v := NewInt32(1)
	out, err := xmlMarshalNamed(t, "RItemList", ItemValueList{
		Items: []ItemValue{{ItemName: "A", Value: &v, Quality: qualityPtr(NewGoodQuality())}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	doc := string(out)
	if !strings.Contains(doc, `xsi:type="opc:ReplyItemList"`) {
		t.Errorf("RItemList carries no xsi:type: %s", doc)
	}
	if !strings.Contains(doc, `Reserved=""`) {
		t.Errorf("RItemList carries no Reserved attribute: %s", doc)
	}
	// It still round-trips.
	var back struct {
		XMLName xml.Name      `xml:"Wrap"`
		L       ItemValueList `xml:"RItemList"`
	}
	wrapped := `<Wrap>` + doc + `</Wrap>`
	if err := Decode([]byte(wrapped), &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(back.L.Items) != 1 || back.L.Items[0].ItemName != "A" {
		t.Fatalf("round trip lost the items: %+v", back.L)
	}
}

// strPtrIV is the local helper for ItemValue.DiagnosticInfo, whose
// pointer-ness distinguishes "the client asked and there is nothing to
// say" (a blank string, §3.1.6) from "the client did not ask".
func strPtrIV(s string) *string { return &s }

// TestItemValue_QualifierTypesAnUntypedValue pins the interop tolerance
// added for NothinRandom/pyopcxmlda, which spells the type attribute
// xsi:Type. XML attribute names are case-sensitive, so that is a
// different and meaningless attribute and the request states no type
// except through ValueTypeQualifier. Before the tolerance every Write
// that client issued came back E_BADTYPE.
//
// The subtests bound the tolerance in both directions: it must not touch
// a value that types itself, and it must not invent a type out of a
// qualifier this package cannot decode.
func TestItemValue_QualifierTypesAnUntypedValue(t *testing.T) {
	const ns = ` xmlns:xsi="` + XSINamespace + `" xmlns:xsd="` + XSDNamespace +
		`" xmlns:opc="` + Namespace + `"`

	t.Run("qualifier is the only type stated", func(t *testing.T) {
		// Verbatim the shape pyopcxmlda puts on the wire, capital T and all.
		doc := []byte(`<Items` + ns + ` ItemName="Tag" ValueTypeQualifier="xsd:int">` +
			`<Value xsi:Type="xsd:int">1234</Value></Items>`)
		var got ItemValue
		if err := Decode(doc, &got); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if got.DecodeErr != nil {
			t.Fatalf("DecodeErr = %v, want nil", got.DecodeErr)
		}
		if got.Value == nil {
			t.Fatal("Value is nil")
		}
		if got.Value.Type() != TypeInt {
			t.Fatalf("Type() = %s, want int", got.Value.Type())
		}
		n, err := got.Value.Int32()
		if err != nil {
			t.Fatalf("Int32: %v", err)
		}
		if n != 1234 {
			t.Fatalf("Int32() = %d, want 1234", n)
		}
	})

	t.Run("an explicit xsi:type still wins", func(t *testing.T) {
		// The qualifier names a different type than xsi:type does. The
		// tolerance must stay out of the way entirely: a conforming
		// request has to decode exactly as it did before.
		doc := []byte(`<Items` + ns + ` ItemName="Tag" ValueTypeQualifier="xsd:int">` +
			`<Value xsi:type="xsd:string">1234</Value></Items>`)
		var got ItemValue
		if err := Decode(doc, &got); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if got.Value == nil {
			t.Fatal("Value is nil")
		}
		if got.Value.Type() != TypeString {
			t.Fatalf("Type() = %s, want string (xsi:type must win over the qualifier)", got.Value.Type())
		}
	})

	t.Run("a qualifier this package cannot decode stays E_BADTYPE", func(t *testing.T) {
		for _, qualifier := range []string{
			"opc:Vendorish",       // not an XSD type at all
			"xsd:int",             // sanity anchor, handled above
			"opc:ArrayOfInt",      // an array type, not a scalar
			"xsd:nosuchtypeatall", // XSD namespace, unknown local name
		} {
			doc := []byte(`<Items` + ns + ` ItemName="Tag" ValueTypeQualifier="` + qualifier + `">` +
				`<Value>1234</Value></Items>`)
			var got ItemValue
			if err := Decode(doc, &got); err != nil {
				t.Fatalf("Decode(%s): %v", qualifier, err)
			}
			wantOK := qualifier == "xsd:int"
			if wantOK {
				if got.DecodeErr != nil {
					t.Errorf("qualifier %s: DecodeErr = %v, want nil", qualifier, got.DecodeErr)
				}
				continue
			}
			if got.DecodeErr == nil {
				t.Errorf("qualifier %s: decoded cleanly, want a bad-type item error", qualifier)
				continue
			}
			var ide *ItemDecodeError
			if !errors.As(got.DecodeErr, &ide) {
				t.Errorf("qualifier %s: DecodeErr = %T, want *ItemDecodeError", qualifier, got.DecodeErr)
				continue
			}
			if ide.Code != ErrBadType {
				t.Errorf("qualifier %s: Code = %v, want E_BADTYPE", qualifier, ide.Code)
			}
		}
	})
}
