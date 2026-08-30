package xmlda

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

// Regression tests for the xmlda-layer defects found in the wire-format
// review.

// --- K3: an array can be turned into a Value ---

// TestNewArrayValue_RoundTrip pins the fix for there having been no way
// at all to construct an array-typed Value through the public API. The
// NewXArray constructors return an Array, backend.ItemSample.Value takes
// a Value, and Value's fields are unexported — so a backend simply could
// not report an ArrayOf<X> item, even though the decode path had
// supported them from the start and the repository's own captured
// fixtures contain one.
func TestNewArrayValue_RoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		value    Value
		typeName QName
		wantXML  string
	}{
		{"double", NewArrayValue(NewFloat64Array([]float64{1.5, -2})),
			QName{Namespace, "ArrayOfDouble"}, "<double>1.5</double><double>-2</double>"},
		{"int", NewArrayValue(NewInt32Array([]int32{7})),
			QName{Namespace, "ArrayOfInt"}, "<int>7</int>"},
		{"string", NewArrayValue(NewStringArray([]string{"a", "b"})),
			QName{Namespace, "ArrayOfString"}, "<string>a</string><string>b</string>"},
		{"boolean", NewArrayValue(NewBoolArray([]bool{true, false})),
			QName{Namespace, "ArrayOfBoolean"}, "<boolean>true</boolean><boolean>false</boolean>"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.value.Kind() != KindArray {
				t.Fatalf("got Kind %v, want KindArray", tc.value.Kind())
			}
			if tc.value.TypeName() != tc.typeName {
				t.Fatalf("got TypeName %v, want %v", tc.value.TypeName(), tc.typeName)
			}
			if !tc.value.IsValid() {
				t.Fatal("a constructed array Value reports IsValid() == false")
			}

			out, err := xmlMarshalNamed(t, "Value", tc.value)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !strings.Contains(string(out), tc.wantXML) {
				t.Fatalf("got %s,\nwant it to contain %s", out, tc.wantXML)
			}

			// And it decodes back to an equal Value.
			wrapped := `<Wrap xmlns:opc="` + Namespace + `" xmlns:xsi="` + XSINamespace + `">` +
				string(out) + `</Wrap>`
			var outer struct {
				XMLName xml.Name `xml:"Wrap"`
				V       Value    `xml:"Value"`
			}
			if err := Decode([]byte(wrapped), &outer); err != nil {
				t.Fatalf("decode: %v\ndoc: %s", err, wrapped)
			}
			if !outer.V.Equal(tc.value) {
				t.Fatalf("round trip changed the value:\ngot  %+v\nwant %+v", outer.V, tc.value)
			}
		})
	}
}

// TestValue_IsValid distinguishes a constructed Value from the zero one,
// which is the check the server layer uses before putting a
// backend-supplied Value on the wire.
func TestValue_IsValid(t *testing.T) {
	if (Value{}).IsValid() {
		t.Error("the zero Value reports IsValid() == true")
	}
	if !NewInt32(1).IsValid() {
		t.Error("a constructed scalar reports IsValid() == false")
	}
	if !NewNil(QName{XSDNamespace, "int"}).IsValid() {
		t.Error("an xsi:nil Value of a declared type reports IsValid() == false")
	}
}

// --- M11: wire timestamps are UTC with millisecond precision ---

// TestWireTime_IsUTCMilliseconds pins the fix for timestamps having gone
// out via time.RFC3339Nano, which emits the server process's local offset
// and a variable-length fractional part. Both are legal xsd:dateTime, but
// the real captured traffic is UTC with milliseconds, and a client that
// subtracts timestamps without applying the offset reads a server in a
// non-UTC zone as hours off.
func TestWireTime_IsUTCMilliseconds(t *testing.T) {
	berlin := time.FixedZone("CEST", 2*60*60)
	ts := time.Date(2026, 3, 4, 11, 30, 0, 123456789, berlin)

	if got, want := formatWireTime(ts), "2026-03-04T09:30:00.123Z"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// The same form reaches the wire through ReplyBase and ItemValue.
	out, err := xmlMarshalNamed(t, "Result", ReplyBase{
		RcvTime: ts, ReplyTime: ts, ServerState: ServerStateRunning,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `RcvTime="2026-03-04T09:30:00.123Z"`) {
		t.Fatalf("ReplyBase did not emit a UTC millisecond timestamp: %s", out)
	}

	iv := ItemValue{ItemName: "A", Timestamp: &ts, Quality: NewGoodQuality()}
	out, err = xmlMarshalNamed(t, "Items", iv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `Timestamp="2026-03-04T09:30:00.123Z"`) {
		t.Fatalf("ItemValue did not emit a UTC millisecond timestamp: %s", out)
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
		ItemName: "A", Value: &v, Quality: NewGoodQuality(), DiagnosticInfo: "why it failed",
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
			if iv.DiagnosticInfo != "d" {
				t.Fatalf("got DiagnosticInfo %q, want %q", iv.DiagnosticInfo, "d")
			}
		})
	}
}

// --- M13: an unknown value's inner XML is content-only on the way back ---

// TestUnknownValue_DropsNonContentTokens pins the fix for
// writeRawInnerXML having relayed every token it found, including
// directives and processing instructions. That inner XML arrives verbatim
// from a peer (ADR-003 preserves an unrecognized xsi:type's bytes), and a
// Write with ReturnValuesOnReply echoes it straight back into a response —
// so a directive in the request became a directive in the middle of the
// response document, making it invalid with no error to signal it, since
// the encode itself succeeds.
func TestUnknownValue_DropsNonContentTokens(t *testing.T) {
	body := `<Value xmlns:v="http://example.com/vendor" xmlns:xsi="` + XSINamespace + `" ` +
		`xsi:type="v:WeirdType">text<!-- a comment --><?pi target?><child>kept</child></Value>`

	var v Value
	doc, err := NewDocument([]byte(body))
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	if err := doc.Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !v.IsUnknown() {
		t.Fatalf("got Kind %v, want KindUnknown for an unrecognized xsi:type", v.Kind())
	}

	out, err := xmlMarshalNamed(t, "Value", v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(out)
	if strings.Contains(got, "<!--") || strings.Contains(got, "<?") {
		t.Errorf("a comment or processing instruction was relayed from peer input into the response: %s", got)
	}
	// The actual content still round-trips.
	if !strings.Contains(got, "text") || !strings.Contains(got, "kept") {
		t.Errorf("content was lost along with the non-content tokens: %s", got)
	}
}

// --- M10: the decoder-scope error names the fix ---

// TestResolveQName_ErrorNamesTheEntryPoint pins the improved message for
// the trap a caller falls into by reaching for encoding/xml directly:
// QName-valued attributes need the document's prefix declarations, which
// only xmlda's own entry points collect.
func TestResolveQName_ErrorNamesTheEntryPoint(t *testing.T) {
	var iv ItemValue
	err := xml.Unmarshal([]byte(`<Items xmlns:opc="`+Namespace+`" ResultID="opc:E_FAIL"/>`), &iv)
	if err == nil {
		t.Fatal("decoding through encoding/xml directly unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "xmlda.Decode") {
		t.Errorf("the error does not name the supported entry point: %v", err)
	}
}

// --- N9: xsd:duration is validated on both paths ---

// TestDuration_Validated pins the fix for xsd:duration having been the
// one scalar type accepted without any lexical check — any string at all
// decoded as a duration and was echoed straight back onto the wire, while
// xsd:decimal right beside it had been validated all along.
func TestDuration_Validated(t *testing.T) {
	valid := []string{"P1D", "PT1H30M", "-P1Y2M3DT4H5M6.7S", "P1Y", "PT0.5S", "P10675199DT2H48M5.4775807S"}
	invalid := []string{"", "P", "PT", "1D", "P1X", "hello", "P1DT", "-P", "1 day"}

	for _, s := range valid {
		if !ValidDuration(s) {
			t.Errorf("ValidDuration(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if ValidDuration(s) {
			t.Errorf("ValidDuration(%q) = true, want false", s)
		}
	}

	// Decode rejects a malformed literal instead of storing it.
	body := `<Value xmlns:xsd="` + XSDNamespace + `" xmlns:xsi="` + XSINamespace + `" xsi:type="xsd:duration">not a duration</Value>`
	var v Value
	doc, err := NewDocument([]byte(body))
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	if err := doc.Decode(&v); err == nil {
		t.Error("decoding a malformed xsd:duration succeeded; want an error")
	}

	// And a caller-constructed one is caught on the way out, since
	// NewDuration has no error to return.
	if _, err := xmlMarshalNamed(t, "Value", NewDuration("nonsense")); err == nil {
		t.Error("marshaling a malformed xsd:duration succeeded; want an error")
	}
	if _, err := xmlMarshalNamed(t, "Value", NewDuration("P1D")); err != nil {
		t.Errorf("marshaling a valid duration failed: %v", err)
	}
}

// --- N5: a mistyped Value reports an error rather than panicking ---

// TestFormatScalar_MistypedValueErrors pins the fix for formatScalar's
// eighteen unchecked type assertions. A Value whose declared ScalarType
// and stored payload disagree is an internal inconsistency no constructor
// produces — but a panic is the wrong way to say so: it unwinds through
// the encoder into ServeHTTP's recover and reaches the client as a bare
// E_FAIL, with the actual cause visible only in a stack trace.
func TestFormatScalar_MistypedValueErrors(t *testing.T) {
	cases := []struct {
		name string
		typ  ScalarType
		val  any
	}{
		{"int declared, string stored", TypeInt, "not an int"},
		{"double declared, int stored", TypeDouble, int32(1)},
		{"dateTime declared, string stored", TypeDateTime, "2026-01-01"},
		{"base64Binary declared, string stored", TypeBase64Binary, "abc"},
		{"boolean declared, nil stored", TypeBoolean, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("formatScalar panicked instead of returning an error: %v", r)
				}
			}()
			if _, err := formatScalar(tc.typ, tc.val); err == nil {
				t.Error("got nil error, want an internal-inconsistency error")
			}
		})
	}
}

// --- N12: RItemList carries the type annotation its siblings do ---

// TestItemValueList_CarriesTypeAndReserved pins the fix for RItemList
// having been the one wrapper type emitted without xsi:type, while
// ReplyBase, ItemValue and OPCQuality all declared theirs for the
// documented benefit of strict and .NET-generated clients. The real
// captured traffic emits both this and Reserved.
func TestItemValueList_CarriesTypeAndReserved(t *testing.T) {
	v := NewInt32(1)
	out, err := xmlMarshalNamed(t, "RItemList", ItemValueList{
		Items: []ItemValue{{ItemName: "A", Value: &v, Quality: NewGoodQuality()}},
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
