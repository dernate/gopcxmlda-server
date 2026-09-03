package xmlda

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
)

// TestArray_NumericFloat64s covers every element type the deadband
// comparison can meet. It is exercised through float64 arrays elsewhere;
// the integer widths are the ones a real PLC actually sends, and each has
// its own case in the conversion.
func TestArray_NumericFloat64s(t *testing.T) {
	for name, a := range map[string]Array{
		"byte":          NewInt8Array([]int8{-1, 2}),
		"short":         NewInt16Array([]int16{-300, 300}),
		"unsignedShort": NewUint16Array([]uint16{0, 65535}),
		"int":           NewInt32Array([]int32{-70000, 70000}),
		"unsignedInt":   NewUint32Array([]uint32{0, 4000000000}),
		"long":          NewInt64Array([]int64{-5, 5}),
		"unsignedLong":  NewUint64Array([]uint64{0, 9}),
		"float":         NewFloat32Array([]float32{1.5, -2.5}),
		"double":        NewFloat64Array([]float64{1.5, -2.5}),
	} {
		got, ok := a.NumericFloat64s()
		if !ok {
			t.Errorf("%s array reported no numeric reading", name)
			continue
		}
		if len(got) != a.Len() {
			t.Errorf("%s array: got %d values for %d elements", name, len(got), a.Len())
		}
	}

	// Types with no numeric reading must say so rather than guess, so the
	// deadband comparison falls back to plain equality for them.
	for name, a := range map[string]Array{
		"string":  NewStringArray([]string{"a"}),
		"boolean": NewBoolArray([]bool{true}),
		"anyType": NewAnyArray([]Value{NewInt32(1)}),
	} {
		if _, ok := a.NumericFloat64s(); ok {
			t.Errorf("%s array claimed a numeric reading", name)
		}
	}

	// The values must be the values, not merely the right count.
	got, _ := NewInt32Array([]int32{-70000, 70000}).NumericFloat64s()
	if len(got) != 2 || got[0] != -70000 || got[1] != 70000 {
		t.Errorf("int array converted to %v, want [-70000 70000]", got)
	}
}

// TestValue_NumericAsFloat64 is the scalar counterpart, and the reason
// the deadband can compare a percentage at all.
func TestValue_NumericAsFloat64(t *testing.T) {
	for name, tc := range map[string]struct {
		v    Value
		want float64
		ok   bool
	}{
		"int32":   {NewInt32(42), 42, true},
		"uint64":  {NewUint64(7), 7, true},
		"float32": {NewFloat32(1.5), 1.5, true},
		"float64": {NewFloat64(-2.5), -2.5, true},
		"string":  {NewString("42"), 0, false},
		"bool":    {NewBool(true), 0, false},
		"array":   {NewArrayValue(NewInt32Array([]int32{1})), 0, false},
	} {
		got, ok := tc.v.NumericAsFloat64()
		if ok != tc.ok {
			t.Errorf("%s: ok = %v, want %v", name, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
	nan, ok := NewFloat64(math.NaN()).NumericAsFloat64()
	if !ok || !math.IsNaN(nan) {
		t.Errorf("NaN did not survive the numeric reading: %v (ok=%v)", nan, ok)
	}
}

// TestValue_TypedAccessorsRejectTheWrongType pins that an accessor for the
// wrong type reports it rather than returning a plausible zero — a
// backend reading a value it did not construct depends on that.
func TestValue_TypedAccessorsRejectTheWrongType(t *testing.T) {
	s, err := NewString("hello").String()
	if err != nil || s != "hello" {
		t.Errorf("String() on a string value: %q, %v", s, err)
	}
	if _, err := NewInt32(1).String(); err == nil {
		t.Error("String() accepted an int value")
	}
	f, err := NewFloat64(1.25).Float64()
	if err != nil || f != 1.25 {
		t.Errorf("Float64() on a double value: %v, %v", f, err)
	}
	if _, err := NewString("x").Float64(); err == nil {
		t.Error("Float64() accepted a string value")
	}
	var te *TypeError
	if _, err := NewString("x").Float64(); !errors.As(err, &te) {
		t.Errorf("the wrong-type error is %T, want *TypeError so callers can distinguish it", err)
	}
	if _, err := NewInt32(1).Array(); err == nil {
		t.Error("Array() accepted a scalar value")
	}
	if _, err := NewArrayValue(NewInt32Array([]int32{1})).Array(); err != nil {
		t.Errorf("Array() rejected an array value: %v", err)
	}
	if got := NewArrayValue(NewInt32Array(nil)).TypeName(); got.Local != "ArrayOfInt" {
		t.Errorf("array TypeName = %v, want ArrayOfInt", got)
	}
}

// TestItemDecodeError pins the error type that turns an unreadable request
// item into that item's own ResultID.
func TestItemDecodeError(t *testing.T) {
	inner := errors.New("not a number")
	e := &ItemDecodeError{Field: "MaxAge", Code: ErrFail, Err: inner}
	if !strings.Contains(e.Error(), "MaxAge") {
		t.Errorf("Error() does not name the field: %q", e.Error())
	}
	if !errors.Is(e, inner) {
		t.Error("errors.Is cannot see the underlying parse failure")
	}
	if got := ItemResultIDFor(e); got != ErrFail {
		t.Errorf("ItemResultIDFor = %v, want the code the error carries", got)
	}
	if got := ItemDiagnosticFor(e); !strings.Contains(got, "MaxAge") {
		t.Errorf("ItemDiagnosticFor = %q, want it to name the field", got)
	}
	// Anything that is not an *ItemDecodeError still maps to something
	// usable rather than panicking.
	if got := ItemResultIDFor(errors.New("plain")); got.IsZero() {
		t.Error("a plain error mapped to the zero ResultID, which reads as success")
	}
	// A nil error is documented as something callers should not pass; the
	// fallback is E_FAIL, deliberately, so a caller that passes one gets a
	// reportable condition rather than a zero code that reads as success.
	if got := ItemResultIDFor(nil); got != ErrFail {
		t.Errorf("a nil error mapped to %v, want the documented E_FAIL fallback", got)
	}
	// An ItemDecodeError with no code of its own falls back the same way.
	if got := ItemResultIDFor(&ItemDecodeError{Field: "X", Err: inner}); got != ErrFail {
		t.Errorf("a codeless ItemDecodeError mapped to %v, want E_FAIL", got)
	}
}

// TestEncoderScope pins the mechanism that lets a response declare its
// namespaces once on the envelope: a registered ancestor binding must be
// used, and the registration must not outlive its encoder.
func TestEncoderScope(t *testing.T) {
	ns := ResponseNamespaces()
	if ns[Namespace] != "opc" || ns[XSDNamespace] != "xsd" || ns[XSINamespace] != "xsi" {
		t.Fatalf("ResponseNamespaces returned %v", ns)
	}

	var withScope, withoutScope bytes.Buffer
	enc := xml.NewEncoder(&withScope)
	cleanup := DeclareAncestorNamespaces(enc, ns)
	if err := enc.EncodeElement(NewFloat64(1.5), xml.StartElement{Name: xml.Name{Local: "Value"}}); err != nil {
		t.Fatalf("encode with scope: %v", err)
	}
	_ = enc.Close()
	cleanup()

	enc2 := xml.NewEncoder(&withoutScope)
	if err := enc2.EncodeElement(NewFloat64(1.5), xml.StartElement{Name: xml.Name{Local: "Value"}}); err != nil {
		t.Fatalf("encode without scope: %v", err)
	}
	_ = enc2.Close()

	if strings.Contains(withScope.String(), "xmlns:xsd") {
		t.Errorf("the element redeclared a prefix its ancestor already binds:\n%s", withScope.String())
	}
	if !strings.Contains(withScope.String(), `xsi:type="xsd:double"`) {
		t.Errorf("the element lost its type attribute:\n%s", withScope.String())
	}
	// Standalone — no ancestor to inherit from — it must still be
	// self-contained, or a Value marshaled on its own is unreadable.
	if !strings.Contains(withoutScope.String(), "xmlns:xsd") {
		t.Errorf("a standalone Value did not declare its own prefix:\n%s", withoutScope.String())
	}
	// And the registration is gone once cleanup ran.
	if _, ok := ancestorPrefix(enc, XSDNamespace); ok {
		t.Error("the encoder scope outlived its cleanup function")
	}
	if _, ok := ancestorPrefix(nil, XSDNamespace); ok {
		t.Error("a nil encoder resolved a prefix")
	}
}

// TestDocument_EnvelopeNamespace is what tells the server which SOAP
// version to answer in.
func TestDocument_EnvelopeNamespace(t *testing.T) {
	for doc, want := range map[string]string{
		`<soap:Envelope xmlns:soap="http://www.w3.org/2003/05/soap-envelope"><soap:Body/></soap:Envelope>`: "http://www.w3.org/2003/05/soap-envelope",
		`<S:Envelope xmlns:S="http://schemas.xmlsoap.org/soap/envelope/"><S:Body/></S:Envelope>`:           "http://schemas.xmlsoap.org/soap/envelope/",
		`<Envelope><Body/></Envelope>`: "",
	} {
		d, err := NewDocument([]byte(doc))
		if err != nil {
			t.Fatalf("NewDocument(%q): %v", doc, err)
		}
		if got := d.EnvelopeNamespace(); got != want {
			t.Errorf("EnvelopeNamespace() = %q, want %q", got, want)
		}
	}
}

// TestCharsetReader_RejectsWhatItCannotDecode pins the boundary of the
// tolerance added for legacy clients: encodings that exist are decoded,
// and an unknown label is still an error rather than a guess at bytes.
func TestCharsetReader_RejectsWhatItCannotDecode(t *testing.T) {
	for _, label := range []string{"utf-8", "UTF-8", "ISO-8859-1", "iso_8859_1", "windows-1252", "US-ASCII"} {
		r, err := newCharsetReader(label, strings.NewReader("<a/>"))
		if err != nil {
			t.Errorf("%q was refused: %v", label, err)
			continue
		}
		if _, err := io.ReadAll(r); err != nil {
			t.Errorf("%q: reading the transcoded stream: %v", label, err)
		}
	}
	for _, label := range []string{"EBCDIC-CP-BE", "shift_jis", "", "not-an-encoding"} {
		if _, err := newCharsetReader(label, strings.NewReader("<a/>")); err == nil {
			t.Errorf("unsupported encoding %q was silently accepted", label)
		}
	}
	// Latin-1 maps every byte to the code point of the same value.
	r, err := newCharsetReader("ISO-8859-1", bytes.NewReader([]byte{0xF6}))
	if err != nil {
		t.Fatalf("latin-1: %v", err)
	}
	got, _ := io.ReadAll(r)
	if string(got) != "ö" {
		t.Errorf("0xF6 decoded to %q, want %q", got, "ö")
	}
}

// TestKindString pins the names that appear in the errors a backend
// author actually reads when an accessor rejects a value.
func TestKindString(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range []Kind{KindScalar, KindArray, KindUnknown} {
		s := k.String()
		if s == "" {
			t.Errorf("Kind(%d) has no name", k)
		}
		if seen[s] {
			t.Errorf("Kind(%d) reuses the name %q", k, s)
		}
		seen[s] = true
	}
	if got := Kind(99).String(); got == "" {
		t.Error("an unknown Kind has no name, so an error message about it would be blank")
	}
	// The name has to reach the error, or it is decoration.
	_, err := NewInt32(1).Array()
	if err == nil || !strings.Contains(err.Error(), KindScalar.String()) {
		t.Errorf("the wrong-kind error does not name the kind: %v", err)
	}
}

// TestDecimalFloat64 pins the lossy reading of xsd:decimal. The type
// keeps the literal so no precision is lost on the wire; this is the
// escape hatch for callers that want a number.
func TestDecimalFloat64(t *testing.T) {
	d, err := NewDecimal("1.25")
	if err != nil {
		t.Fatalf("NewDecimal: %v", err)
	}
	f, err := d.Float64()
	if err != nil || f != 1.25 {
		t.Errorf("Float64() = %v, %v; want 1.25", f, err)
	}
	// A Decimal built by hand from something unparseable must report it
	// rather than returning a plausible zero.
	if _, err := Decimal("not a number").Float64(); err == nil {
		t.Error("Float64() accepted a non-numeric decimal literal")
	}
	if _, err := NewDecimal("nonsense"); err == nil {
		t.Error("NewDecimal accepted a non-numeric literal")
	}
	// The literal survives the round trip through the wire form exactly,
	// which is the reason the type exists.
	big, err := NewDecimal("123456789012345678901234567890.5")
	if err != nil {
		t.Fatalf("NewDecimal(big): %v", err)
	}
	if string(big) != "123456789012345678901234567890.5" {
		t.Errorf("the literal was rewritten to %q", string(big))
	}
}

// TestArrayTypeName pins the declared ArrayOf<X> name each array reports
// — the value that becomes its xsi:type on the wire.
func TestArrayTypeName(t *testing.T) {
	for _, tc := range []struct {
		a    Array
		want string
	}{
		{NewInt32Array(nil), "ArrayOfInt"},
		{NewFloat64Array(nil), "ArrayOfDouble"},
		{NewStringArray(nil), "ArrayOfString"},
		{NewAnyArray(nil), "ArrayOfAnyType"},
	} {
		got := tc.a.TypeName()
		if got.Local != tc.want {
			t.Errorf("TypeName().Local = %q, want %q", got.Local, tc.want)
		}
		if got.Space != Namespace {
			t.Errorf("TypeName().Space = %q, want the OPC XML-DA namespace", got.Space)
		}
	}
}

// TestUnqualifiedQNameAttr pins the fix that keeps one namespace-less
// vendor property from invalidating a whole response: an unprefixed QName
// in an ATTRIBUTE value has no namespace by definition, so the xmlns=""
// reset changed nothing about how it reads — while applying to the entire
// carrier element and pulling it out of the OPC XML-DA namespace.
func TestUnqualifiedQNameAttr(t *testing.T) {
	attrs := unqualifiedQNameAttr("Name", "vendorThing")
	if len(attrs) != 1 {
		t.Fatalf("got %d attributes, want just the QName itself: %+v", len(attrs), attrs)
	}
	if attrs[0].Name.Local != "Name" || attrs[0].Value != "vendorThing" {
		t.Errorf("got %+v", attrs[0])
	}
	for _, a := range attrs {
		if strings.HasPrefix(a.Name.Local, "xmlns") {
			t.Errorf("an unqualified QName attribute still emits %q, which applies to the whole element",
				a.Name.Local)
		}
	}
	// And through the real entry point, with and without a namespace.
	if got := qnameAttr(nil, nil, "ResultID", QName{Local: "bare"}); len(got) != 1 {
		t.Errorf("qnameAttr for a namespace-less QName produced %+v", got)
	}
	qualified := qnameAttr(nil, nil, "ResultID", QName{Space: Namespace, Local: "E_FAIL"})
	if len(qualified) != 2 {
		t.Fatalf("a qualified QName produced %+v, want a declaration plus the attribute", qualified)
	}
}
