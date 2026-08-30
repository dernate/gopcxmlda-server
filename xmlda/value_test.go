package xmlda

import (
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"
)

// marshalValue marshals v with its own root element (Value.MarshalXML
// determines the element name via the caller-supplied start; xml.Marshal
// at the top level uses the type's name, "Value", matching every
// hand-constructed raw document below).
func marshalValue(t *testing.T, v Value) []byte {
	t.Helper()
	out, err := xml.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}

func unmarshalValue(t *testing.T, doc []byte) Value {
	t.Helper()
	var v Value
	if err := Decode(doc, &v); err != nil {
		t.Fatalf("Decode: %v\ndoc: %s", err, doc)
	}
	return v
}

func roundTrip(t *testing.T, v Value) Value {
	t.Helper()
	doc := marshalValue(t, v)
	return unmarshalValue(t, doc)
}

func TestValue_ScalarRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		v    Value
	}{
		{"string empty", NewString("")},
		{"string", NewString("hello <world> & stuff")},
		{"bool true", NewBool(true)},
		{"bool false", NewBool(false)},
		{"float32 zero", NewFloat32(0)},
		{"float32 negative", NewFloat32(-4.5)},
		{"float32 max", NewFloat32(math.MaxFloat32)},
		{"float32 NaN", NewFloat32(float32(math.NaN()))},
		{"float32 +Inf", NewFloat32(float32(math.Inf(1)))},
		{"float32 -Inf", NewFloat32(float32(math.Inf(-1)))},
		{"float64", NewFloat64(4.5)},
		{"float64 NaN", NewFloat64(math.NaN())},
		{"float64 +Inf", NewFloat64(math.Inf(1))},
		{"float64 -Inf", NewFloat64(math.Inf(-1))},
		{"int64 min", NewInt64(math.MinInt64)},
		{"int64 max", NewInt64(math.MaxInt64)},
		{"int32", NewInt32(1234)},
		{"int32 negative", NewInt32(-1234)},
		{"int32 min", NewInt32(math.MinInt32)},
		{"int32 max", NewInt32(math.MaxInt32)},
		{"int16 min", NewInt16(math.MinInt16)},
		{"int16 max", NewInt16(math.MaxInt16)},
		{"int8 min (byte)", NewInt8(math.MinInt8)},
		{"int8 max (byte)", NewInt8(math.MaxInt8)},
		{"uint64 max", NewUint64(math.MaxUint64)},
		{"uint32 max", NewUint32(math.MaxUint32)},
		{"uint16 max", NewUint16(math.MaxUint16)},
		{"uint8 zero (unsignedByte)", NewUint8(0)},
		{"uint8 max (unsignedByte)", NewUint8(math.MaxUint8)},
		{"bytes empty", NewBytes([]byte{})},
		{"bytes", NewBytes([]byte{0x00, 0x01, 0xFF, 0xAB})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := roundTrip(t, tc.v)
			assertValueEqual(t, tc.v, got)
		})
	}
}

func TestValue_ByteWidthSymmetry(t *testing.T) {
	// The reference client (gopcxmlda_only_for_reference_dont_add) decodes
	// byte/unsignedByte into 16-bit Go types but infers 8-bit types on
	// write, an asymmetry bug this library must not repeat (ADR-002).
	// Verify byte and unsignedByte round-trip through exactly int8/uint8,
	// with no width change in either direction.
	b := NewInt8(-100)
	got := roundTrip(t, b)
	i8, err := got.Int8()
	if err != nil {
		t.Fatalf("Int8: %v", err)
	}
	if i8 != -100 {
		t.Fatalf("got %d, want -100", i8)
	}
	if _, err := got.Int16(); err == nil {
		t.Fatalf("expected Int16() to fail for a byte-typed value")
	}

	ub := NewUint8(200)
	got2 := roundTrip(t, ub)
	u8, err := got2.Uint8()
	if err != nil {
		t.Fatalf("Uint8: %v", err)
	}
	if u8 != 200 {
		t.Fatalf("got %d, want 200", u8)
	}
	if _, err := got2.Uint16(); err == nil {
		t.Fatalf("expected Uint16() to fail for an unsignedByte-typed value")
	}
}

func TestValue_DecimalRoundTrip(t *testing.T) {
	d, err := NewDecimal("123.4500")
	if err != nil {
		t.Fatalf("NewDecimal: %v", err)
	}
	got := roundTrip(t, NewDecimalValue(d))
	gotD, err := got.Decimal()
	if err != nil {
		t.Fatalf("Decimal: %v", err)
	}
	if gotD.String() != "123.4500" {
		t.Fatalf("got %q, want %q (exact lexical form must survive)", gotD.String(), "123.4500")
	}
}

func TestNewDecimal_Invalid(t *testing.T) {
	for _, s := range []string{"not-a-number", "", ".", "+", "-", "1.2.3", "1e5"} {
		if _, err := NewDecimal(s); err == nil {
			t.Fatalf("NewDecimal(%q): expected error for invalid decimal literal", s)
		}
	}
}

// TestNewDecimal_ValidEdgeForms covers the XSD Part 2 grammar's edge-legal
// lexical forms — a digit is only required on one side of the decimal
// point, not both. "210." is one of the specification's own worked
// examples of a legal xsd:decimal.
func TestNewDecimal_ValidEdgeForms(t *testing.T) {
	for _, s := range []string{"210.", ".5", "-.5", "+5.", "0", "-0.0"} {
		d, err := NewDecimal(s)
		if err != nil {
			t.Fatalf("NewDecimal(%q): unexpected error: %v", s, err)
		}
		if d.String() != s {
			t.Fatalf("NewDecimal(%q).String() = %q, want %q", s, d.String(), s)
		}
	}
}

// TestNewDecimalFromFloat64 pins the invariant that makes the constructor
// safe to use at all: whatever it hands back must be text NewDecimal would
// have accepted. Nothing downstream revalidates a Decimal — marshalScalar
// stringifies it verbatim — so this constructor is the only gate between a
// float64 and the xsd:decimal element content on the wire.
func TestNewDecimalFromFloat64(t *testing.T) {
	for _, f := range []float64{0, 1, -1, 0.5, -0.125, 123.45, 1e21, -1e21, math.SmallestNonzeroFloat64, math.MaxFloat64} {
		d, err := NewDecimalFromFloat64(f)
		if err != nil {
			t.Fatalf("NewDecimalFromFloat64(%v): unexpected error: %v", f, err)
		}
		if _, err := NewDecimal(d.String()); err != nil {
			t.Errorf("NewDecimalFromFloat64(%v) produced %q, which NewDecimal rejects: %v", f, d, err)
		}
	}
}

// TestNewDecimalFromFloat64_NonFinite covers the three float64 values with
// no xsd:decimal lexical form at all. strconv.FormatFloat spells them
// "NaN", "+Inf" and "-Inf"; xsd:decimal's grammar admits none of the
// three, so they have to be refused here rather than silently emitted.
func TestNewDecimalFromFloat64_NonFinite(t *testing.T) {
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		d, err := NewDecimalFromFloat64(f)
		if err == nil {
			t.Fatalf("NewDecimalFromFloat64(%v) = %q, want an error: no such xsd:decimal literal exists", f, d)
		}
		if d != "" {
			t.Errorf("NewDecimalFromFloat64(%v) returned %q alongside its error, want the zero Decimal", f, d)
		}
	}
}

func TestValue_DateTimeRoundTrip(t *testing.T) {
	// Matches the real fixture's timestamp format: explicit UTC offset,
	// millisecond fraction. testdata/responses/subscribe_680.response.xml
	ref, err := time.Parse(time.RFC3339Nano, "2019-09-23T16:01:50.576+00:00")
	if err != nil {
		t.Fatalf("parsing reference time: %v", err)
	}
	got := roundTrip(t, NewDateTime(ref))
	gotT, err := got.Time()
	if err != nil {
		t.Fatalf("Time: %v", err)
	}
	if !gotT.Equal(ref) {
		t.Fatalf("got %v, want %v", gotT, ref)
	}
}

func TestValue_TimeAndDateRoundTrip(t *testing.T) {
	// xsd:time and xsd:date each carry only one of the two components —
	// the wire literal never carries the other (REQ-... see value.go's
	// formatScalar), so round-tripping a time.Time with both set must
	// correctly *drop* whichever component the declared type doesn't
	// carry on the wire, not silently preserve it.
	ref := time.Date(2024, 3, 15, 13, 45, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		v    Value
		want time.Time
	}{
		{"time", NewTime(ref), time.Date(0, 1, 1, 13, 45, 0, 0, time.UTC)},
		{"date", NewDate(ref), time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := roundTrip(t, tc.v)
			if got.Type() != tc.v.Type() {
				t.Fatalf("got type %s, want %s", got.Type(), tc.v.Type())
			}
			gotT, err := got.Time()
			if err != nil {
				t.Fatalf("Time: %v", err)
			}
			if !gotT.Equal(tc.want) {
				t.Fatalf("got %v, want %v", gotT, tc.want)
			}
		})
	}
}

// TestValue_TimeAndDate_ExternalLexicalForms decodes standalone xsd:time/
// xsd:date literals in every legal XSD lexical shape, not just this
// library's own (now-corrected) encoder output — the gap that let a
// prior bug (accepting only full dateTime-shaped strings) go unnoticed,
// since every other test only round-trips through this library's own
// encoder.
func TestValue_TimeAndDate_ExternalLexicalForms(t *testing.T) {
	cases := []struct {
		typ  ScalarType
		text string
		want time.Time
	}{
		{TypeTime, "13:45:00", time.Date(0, 1, 1, 13, 45, 0, 0, time.UTC)},
		{TypeTime, "13:45:00.576", time.Date(0, 1, 1, 13, 45, 0, 576000000, time.UTC)},
		{TypeTime, "13:45:00Z", time.Date(0, 1, 1, 13, 45, 0, 0, time.UTC)},
		{TypeTime, "13:45:00+00:00", time.Date(0, 1, 1, 13, 45, 0, 0, time.UTC)},
		{TypeDate, "2019-09-23", time.Date(2019, 9, 23, 0, 0, 0, 0, time.UTC)},
		{TypeDate, "2019-09-23Z", time.Date(2019, 9, 23, 0, 0, 0, 0, time.UTC)},
		{TypeDate, "2019-09-23+01:00", time.Date(2019, 9, 23, 0, 0, 0, 0, time.FixedZone("", 3600))},
	}
	for _, tc := range cases {
		t.Run(string(tc.typ)+"/"+tc.text, func(t *testing.T) {
			got, err := parseScalar(tc.typ, tc.text)
			if err != nil {
				t.Fatalf("parseScalar(%s, %q): %v", tc.typ, tc.text, err)
			}
			gotT, ok := got.(time.Time)
			if !ok {
				t.Fatalf("parseScalar(%s, %q) returned %T, want time.Time", tc.typ, tc.text, got)
			}
			if !gotT.Equal(tc.want) {
				t.Fatalf("parseScalar(%s, %q) = %v, want %v", tc.typ, tc.text, gotT, tc.want)
			}
		})
	}
}

func TestValue_DurationRoundTrip(t *testing.T) {
	got := roundTrip(t, NewDuration("P1D"))
	d, err := got.Duration()
	if err != nil {
		t.Fatalf("Duration: %v", err)
	}
	if d != "P1D" {
		t.Fatalf("got %q, want %q", d, "P1D")
	}
}

func TestValue_QNameRoundTrip(t *testing.T) {
	cases := []QName{
		{Space: Namespace, Local: "SomeType"},
		{Space: XSDNamespace, Local: "int"},
		{Local: "NoNamespace"},
	}
	for _, qn := range cases {
		got := roundTrip(t, NewQNameValue(qn))
		gotQN, err := got.QNameValue()
		if err != nil {
			t.Fatalf("QNameValue: %v", err)
		}
		if gotQN != qn {
			t.Fatalf("got %+v, want %+v", gotQN, qn)
		}
	}
}

func TestValue_Nil(t *testing.T) {
	v := NewNil(QName{Space: XSDNamespace, Local: "int"})
	got := roundTrip(t, v)
	if !got.IsNil() {
		t.Fatalf("expected IsNil() true")
	}
	if got.TypeName() != v.TypeName() {
		t.Fatalf("got type %+v, want %+v", got.TypeName(), v.TypeName())
	}
	if _, err := got.Int32(); err == nil {
		t.Fatalf("expected typed accessor to fail on a nil value")
	} else if te := (*TypeError)(nil); !errors.As(err, &te) || !te.Nil {
		t.Fatalf("expected *TypeError with Nil=true, got %v (%T)", err, err)
	}
}

func TestValue_UnknownTypePassthrough(t *testing.T) {
	// A vendor-specific type this library has never heard of must not
	// crash decoding, and must round-trip unmodified (ADR-003).
	doc := []byte(`<Value xmlns:vendor="http://example.com/vendor" xsi:type="vendor:WeirdThing" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"><a>1</a><b>2</b></Value>`)
	v := unmarshalValue(t, doc)
	if !v.IsUnknown() {
		t.Fatalf("expected IsUnknown() true, got Kind=%s", v.Kind())
	}
	want := QName{Space: "http://example.com/vendor", Local: "WeirdThing"}
	if v.TypeName() != want {
		t.Fatalf("got type %+v, want %+v", v.TypeName(), want)
	}
	raw, err := v.Raw()
	if err != nil {
		t.Fatalf("Raw: %v", err)
	}
	if string(raw.InnerXML) != "<a>1</a><b>2</b>" {
		t.Fatalf("got inner XML %q", raw.InnerXML)
	}

	// Round-trip: re-encode and re-decode, expect semantic equivalence.
	out := marshalValue(t, v)
	v2 := unmarshalValue(t, out)
	if !v2.IsUnknown() || v2.TypeName() != want {
		t.Fatalf("round-trip mismatch: %+v", v2)
	}
	raw2, _ := v2.Raw()
	if string(raw2.InnerXML) != string(raw.InnerXML) {
		t.Fatalf("inner XML changed on round-trip: got %q, want %q", raw2.InnerXML, raw.InnerXML)
	}

	// Every typed accessor must fail cleanly, never panic.
	if _, err := v.Int32(); err == nil {
		t.Fatalf("expected Int32() to fail on an unknown-type value")
	}
	if _, err := v.String(); err == nil {
		t.Fatalf("expected String() to fail on an unknown-type value")
	}
}

// TestValue_ArrayOfUnsignedShort_RealFixture decodes the exact array value
// observed in testdata/responses/subscribe_680.response.xml and confirms
// it decodes to []uint16{0,0,3,11,0,0}, then round-trips.
func TestValue_ArrayOfUnsignedShort_RealFixture(t *testing.T) {
	doc := []byte(`<Value xmlns:xsi="` + XSINamespace + `" xmlns:ns1="` + Namespace + `" xsi:type="ns1:ArrayOfUnsignedShort"><unsignedShort>0</unsignedShort><unsignedShort>0</unsignedShort><unsignedShort>3</unsignedShort><unsignedShort>11</unsignedShort><unsignedShort>0</unsignedShort><unsignedShort>0</unsignedShort></Value>`)
	v := unmarshalValue(t, doc)
	if v.Kind() != KindArray {
		t.Fatalf("got Kind=%s, want array", v.Kind())
	}
	arr, err := v.Array()
	if err != nil {
		t.Fatalf("Array: %v", err)
	}
	got, err := arr.Uint16s()
	if err != nil {
		t.Fatalf("Uint16s: %v", err)
	}
	want := []uint16{0, 0, 3, 11, 0, 0}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("element %d: got %d, want %d", i, got[i], want[i])
		}
	}

	// Round-trip
	v2 := roundTrip(t, v)
	arr2, err := v2.Array()
	if err != nil {
		t.Fatalf("Array (round-trip): %v", err)
	}
	got2, err := arr2.Uint16s()
	if err != nil {
		t.Fatalf("Uint16s (round-trip): %v", err)
	}
	for i := range want {
		if got2[i] != want[i] {
			t.Fatalf("round-trip element %d: got %d, want %d", i, got2[i], want[i])
		}
	}
}

func TestValue_ArrayRoundTrip_AllTypes(t *testing.T) {
	cases := []struct {
		name string
		arr  Array
	}{
		{"empty int32 array", NewInt32Array(nil)},
		{"int8 array", NewInt8Array([]int8{-128, 0, 127})},
		{"int16 array", NewInt16Array([]int16{-32768, 0, 32767})},
		{"uint16 array", NewUint16Array([]uint16{0, 3, 11})},
		{"int32 array", NewInt32Array([]int32{1, 2, 3})},
		{"uint32 array", NewUint32Array([]uint32{0, 4294967295})},
		{"int64 array", NewInt64Array([]int64{-1, 0, 1})},
		{"uint64 array", NewUint64Array([]uint64{0, 18446744073709551615})},
		{"float32 array", NewFloat32Array([]float32{1.5, -2.5})},
		{"decimal array", NewDecimalArray([]Decimal{"1.5", "-2.5"})},
		{"float64 array", NewFloat64Array([]float64{1.5, -2.5})},
		{"bool array", NewBoolArray([]bool{true, false, true})},
		{"string array", NewStringArray([]string{"a", "", "c"})},
		{"dateTime array", NewDateTimeArray([]time.Time{time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := Value{kind: KindArray, typ: tc.arr.elemType, typeName: arrayTypeNameFor(tc.arr.elemType), array: tc.arr}
			got := roundTrip(t, v)
			arr, err := got.Array()
			if err != nil {
				t.Fatalf("Array: %v", err)
			}
			if arr.ElemType() != tc.arr.elemType {
				t.Fatalf("got elem type %s, want %s", arr.ElemType(), tc.arr.elemType)
			}
			if arr.Len() != tc.arr.Len() {
				t.Fatalf("got len %d, want %d", arr.Len(), tc.arr.Len())
			}
			// Content, not just count/type: a decode bug that scrambles
			// element values (a copy-paste error, an off-by-one) while
			// preserving Len()/ElemType() would otherwise go undetected —
			// every typed accessor is exercised, not just the three
			// (Uint16s/Int32s/Float64s) already covered by other fixtures.
			if !arr.equal(tc.arr) {
				wantElems, wantErr := arrayElements(t, tc.arr)
				gotElems, gotErr := arrayElements(t, arr)
				t.Fatalf("round-tripped array content mismatch: got %v (err %v), want %v (err %v)", gotElems, gotErr, wantElems, wantErr)
			}
		})
	}
}

// arrayElements returns arr's elements as a plain []any via its typed
// accessor, for use in a test failure message.
func arrayElements(t *testing.T, arr Array) (any, error) {
	t.Helper()
	switch arr.ElemType() {
	case TypeByte:
		return arr.Int8s()
	case TypeShort:
		return arr.Int16s()
	case TypeUnsignedShort:
		return arr.Uint16s()
	case TypeInt:
		return arr.Int32s()
	case TypeUnsignedInt:
		return arr.Uint32s()
	case TypeLong:
		return arr.Int64s()
	case TypeUnsignedLong:
		return arr.Uint64s()
	case TypeFloat:
		return arr.Float32s()
	case TypeDecimal:
		return arr.Decimals()
	case TypeDouble:
		return arr.Float64s()
	case TypeBoolean:
		return arr.Bools()
	case TypeString:
		return arr.Strings()
	case TypeDateTime:
		return arr.DateTimes()
	default:
		return nil, fmt.Errorf("arrayElements: unhandled elem type %s", arr.ElemType())
	}
}

// arrayTypeNameFor is a test helper mapping an element ScalarType back to
// its ArrayOf<X> QName, mirroring arrayElemTypesByQName in reverse.
func arrayTypeNameFor(elemType ScalarType) QName {
	for qn, et := range arrayElemTypesByQName {
		if et == elemType {
			return qn
		}
	}
	return QName{}
}

// TestArray_TypedAccessors_ReturnCorrectElements is a regression test: 8 of
// Array's typed accessors (Int8s, Int16s, Uint32s, Int64s, Uint64s,
// Float32s, Decimals, Bools) were previously called only via
// arrayElements above, itself only reached from TestValue_ArrayRoundTrip_
// AllTypes's failure branch — which never runs while arr.equal succeeds.
// A bug scrambling one accessor's own returned slice (e.g. a copy-pasted
// switch case, a wrong type assertion) would ship with the full test
// suite green. Call each directly and compare its actual returned
// content, not just length/type/round-trip equality.
func TestArray_TypedAccessors_ReturnCorrectElements(t *testing.T) {
	cases := []struct {
		name string
		got  any
		want any
	}{
		{"Int8s", mustCall(NewInt8Array([]int8{-128, 0, 127}).Int8s), []int8{-128, 0, 127}},
		{"Int16s", mustCall(NewInt16Array([]int16{-32768, 0, 32767}).Int16s), []int16{-32768, 0, 32767}},
		{"Uint32s", mustCall(NewUint32Array([]uint32{0, 1, 4294967295}).Uint32s), []uint32{0, 1, 4294967295}},
		{"Int64s", mustCall(NewInt64Array([]int64{-1, 0, 1}).Int64s), []int64{-1, 0, 1}},
		{"Uint64s", mustCall(NewUint64Array([]uint64{0, 1, 18446744073709551615}).Uint64s), []uint64{0, 1, 18446744073709551615}},
		{"Float32s", mustCall(NewFloat32Array([]float32{1.5, -2.5}).Float32s), []float32{1.5, -2.5}},
		{"Decimals", mustCall(NewDecimalArray([]Decimal{"1.5", "-2.5"}).Decimals), []Decimal{"1.5", "-2.5"}},
		{"Bools", mustCall(NewBoolArray([]bool{true, false, true}).Bools), []bool{true, false, true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !reflect.DeepEqual(tc.got, tc.want) {
				t.Fatalf("%s() = %v, want %v", tc.name, tc.got, tc.want)
			}
		})
	}
}

// mustCall invokes a (value, error)-returning accessor and panics on a
// non-nil error, so a genuine accessor bug surfaces loudly in a table
// literal rather than silently comparing against a nil slice.
func mustCall[T any](fn func() (T, error)) T {
	v, err := fn()
	if err != nil {
		panic(err)
	}
	return v
}

// TestArray_TypedAccessors_WrongTypeErrors checks that each of the same
// accessors returns a *TypeError (rather than panicking on a bad type
// assertion) when called against an array of a different element type.
func TestArray_TypedAccessors_WrongTypeErrors(t *testing.T) {
	arr := NewInt32Array([]int32{1, 2, 3})
	checks := map[string]func() error{
		"Int8s":    func() error { _, err := arr.Int8s(); return err },
		"Int16s":   func() error { _, err := arr.Int16s(); return err },
		"Uint32s":  func() error { _, err := arr.Uint32s(); return err },
		"Int64s":   func() error { _, err := arr.Int64s(); return err },
		"Uint64s":  func() error { _, err := arr.Uint64s(); return err },
		"Float32s": func() error { _, err := arr.Float32s(); return err },
		"Decimals": func() error { _, err := arr.Decimals(); return err },
		"Bools":    func() error { _, err := arr.Bools(); return err },
	}
	for name, check := range checks {
		t.Run(name, func(t *testing.T) {
			err := check()
			if te := (*TypeError)(nil); !errors.As(err, &te) {
				t.Fatalf("%s() on an int32 array: got err %v (%T), want *TypeError", name, err, err)
			}
		})
	}
}

// nestedAnyTypeValue wraps leaf in depth levels of ArrayOfAnyType, each
// containing exactly the one Value beneath it.
func nestedAnyTypeValue(depth int, leaf Value) Value {
	v := leaf
	for range depth {
		v = Value{kind: KindArray, typ: TypeAnyType, typeName: QName{Namespace, "ArrayOfAnyType"}, array: NewAnyArray([]Value{v})}
	}
	return v
}

// TestValue_ArrayOfAnyType_ExceedsMaxDepth_ErrorsCleanly reproduces
// decoding a small but deeply nested ArrayOfAnyType payload (an
// ArrayOfAnyType containing an ArrayOfAnyType containing...) — without a
// depth limit, decodeAnyTypeArray recurses back into Value.UnmarshalXML
// once per level with no bound, so a document this small could otherwise
// drive stack usage proportional to attacker-chosen nesting depth. It
// must fail cleanly (a decode error), never panic or hang.
func TestValue_ArrayOfAnyType_ExceedsMaxDepth_ErrorsCleanly(t *testing.T) {
	doc := marshalValue(t, nestedAnyTypeValue(maxAnyTypeArrayDepth+10, NewInt32(1)))

	var v Value
	err := Decode(doc, &v)
	if err == nil {
		t.Fatalf("expected a decode error for nesting exceeding maxAnyTypeArrayDepth (%d), got none", maxAnyTypeArrayDepth)
	}
}

// TestValue_ArrayOfAnyType_WithinMaxDepth_RoundTrips is the companion
// sanity check: nesting comfortably within the limit must still decode
// successfully — the limit must not be so tight it rejects legitimate
// (if unusual) nested structures.
func TestValue_ArrayOfAnyType_WithinMaxDepth_RoundTrips(t *testing.T) {
	const depth = maxAnyTypeArrayDepth - 5
	got := roundTrip(t, nestedAnyTypeValue(depth, NewInt32(7)))

	// Unwrap `depth` levels of ArrayOfAnyType to reach the original leaf.
	cur := got
	for i := range depth {
		arr, err := cur.Array()
		if err != nil {
			t.Fatalf("level %d: Array: %v", i, err)
		}
		elems, err := arr.Any()
		if err != nil {
			t.Fatalf("level %d: Any: %v", i, err)
		}
		if len(elems) != 1 {
			t.Fatalf("level %d: got %d elements, want 1", i, len(elems))
		}
		cur = elems[0]
	}
	i32, err := cur.Int32()
	if err != nil || i32 != 7 {
		t.Fatalf("got (%d, %v), want (7, nil)", i32, err)
	}
}

func TestValue_ArrayOfAnyType_NestedAndHeterogeneous(t *testing.T) {
	inner := NewInt32Array([]int32{1, 2, 3})
	nested := Value{kind: KindArray, typ: TypeInt, typeName: QName{Namespace, "ArrayOfInt"}, array: inner}
	elems := []Value{
		NewInt32(42),
		NewString("hello"),
		nested,
	}
	v := Value{kind: KindArray, typ: TypeAnyType, typeName: QName{Namespace, "ArrayOfAnyType"}, array: NewAnyArray(elems)}

	got := roundTrip(t, v)
	arr, err := got.Array()
	if err != nil {
		t.Fatalf("Array: %v", err)
	}
	any_, err := arr.Any()
	if err != nil {
		t.Fatalf("Any: %v", err)
	}
	if len(any_) != 3 {
		t.Fatalf("got %d elements, want 3", len(any_))
	}
	i32, err := any_[0].Int32()
	if err != nil || i32 != 42 {
		t.Fatalf("element 0: got (%d, %v), want (42, nil)", i32, err)
	}
	s, err := any_[1].String()
	if err != nil || s != "hello" {
		t.Fatalf("element 1: got (%q, %v), want (\"hello\", nil)", s, err)
	}
	if any_[2].Kind() != KindArray {
		t.Fatalf("element 2: got Kind=%s, want array (nested array)", any_[2].Kind())
	}
	nestedArr, err := any_[2].Array()
	if err != nil {
		t.Fatalf("nested Array: %v", err)
	}
	nestedInts, err := nestedArr.Int32s()
	if err != nil {
		t.Fatalf("nested Int32s: %v", err)
	}
	if len(nestedInts) != 3 || nestedInts[0] != 1 || nestedInts[2] != 3 {
		t.Fatalf("got %v, want [1 2 3]", nestedInts)
	}
}

func TestValue_ArrayMismatchedElementName(t *testing.T) {
	// An ArrayOfInt whose actual child element name doesn't match "int"
	// must produce a decode error, never silently succeed with garbage.
	doc := []byte(`<Value xmlns:xsi="` + XSINamespace + `" xmlns:ns1="` + Namespace + `" xsi:type="ns1:ArrayOfInt"><wrongName>1</wrongName></Value>`)
	var v Value
	if err := Decode(doc, &v); err == nil {
		t.Fatalf("expected a decode error for mismatched array child element name")
	}
}

func TestValue_MissingXsiType(t *testing.T) {
	doc := []byte(`<Value>4.5</Value>`)
	var v Value
	if err := Decode(doc, &v); err == nil {
		t.Fatalf("expected a decode error for a Value with no xsi:type")
	}
}

func TestValue_EmptyStringIsAPresentValue(t *testing.T) {
	// An empty string is a legitimate value, distinct from "no value at
	// all" (which is represented by a nil *Value at the ItemValue level,
	// not by any Value constructed here).
	got := roundTrip(t, NewString(""))
	s, err := got.String()
	if err != nil {
		t.Fatalf("String: %v", err)
	}
	if s != "" {
		t.Fatalf("got %q, want empty string", s)
	}
	if got.IsNil() {
		t.Fatalf("an empty string must not be IsNil")
	}
}

func TestTypeError_Messages(t *testing.T) {
	v := NewInt32(5)
	_, err := v.String()
	if err == nil {
		t.Fatalf("expected error")
	}
	if err.Error() == "" {
		t.Fatalf("expected non-empty error message")
	}
}

func assertValueEqual(t *testing.T, want, got Value) {
	t.Helper()
	if want.Kind() != got.Kind() {
		t.Fatalf("Kind: got %s, want %s", got.Kind(), want.Kind())
	}
	if want.Type() != got.Type() {
		t.Fatalf("Type: got %s, want %s", got.Type(), want.Type())
	}
	// Compare via the appropriate typed accessor.
	switch want.Type() {
	case TypeString:
		w, _ := want.String()
		g, _ := got.String()
		if w != g {
			t.Fatalf("String: got %q, want %q", g, w)
		}
	case TypeBoolean:
		w, _ := want.Bool()
		g, _ := got.Bool()
		if w != g {
			t.Fatalf("Bool: got %v, want %v", g, w)
		}
	case TypeFloat:
		w, _ := want.Float32()
		g, _ := got.Float32()
		if !floatEqual64(float64(w), float64(g)) {
			t.Fatalf("Float32: got %v, want %v", g, w)
		}
	case TypeDouble:
		w, _ := want.Float64()
		g, _ := got.Float64()
		if !floatEqual64(w, g) {
			t.Fatalf("Float64: got %v, want %v", g, w)
		}
	case TypeLong:
		w, _ := want.Int64()
		g, _ := got.Int64()
		if w != g {
			t.Fatalf("Int64: got %v, want %v", g, w)
		}
	case TypeInt:
		w, _ := want.Int32()
		g, _ := got.Int32()
		if w != g {
			t.Fatalf("Int32: got %v, want %v", g, w)
		}
	case TypeShort:
		w, _ := want.Int16()
		g, _ := got.Int16()
		if w != g {
			t.Fatalf("Int16: got %v, want %v", g, w)
		}
	case TypeByte:
		w, _ := want.Int8()
		g, _ := got.Int8()
		if w != g {
			t.Fatalf("Int8: got %v, want %v", g, w)
		}
	case TypeUnsignedLong:
		w, _ := want.Uint64()
		g, _ := got.Uint64()
		if w != g {
			t.Fatalf("Uint64: got %v, want %v", g, w)
		}
	case TypeUnsignedInt:
		w, _ := want.Uint32()
		g, _ := got.Uint32()
		if w != g {
			t.Fatalf("Uint32: got %v, want %v", g, w)
		}
	case TypeUnsignedShort:
		w, _ := want.Uint16()
		g, _ := got.Uint16()
		if w != g {
			t.Fatalf("Uint16: got %v, want %v", g, w)
		}
	case TypeUnsignedByte:
		w, _ := want.Uint8()
		g, _ := got.Uint8()
		if w != g {
			t.Fatalf("Uint8: got %v, want %v", g, w)
		}
	case TypeBase64Binary:
		w, _ := want.Bytes()
		g, _ := got.Bytes()
		if string(w) != string(g) {
			t.Fatalf("Bytes: got %v, want %v", g, w)
		}
	default:
		t.Fatalf("assertValueEqual: unhandled type %s", want.Type())
	}
}

func floatEqual64(a, b float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	return a == b
}

// TestArray_TypeError_PopulatesTypeNameAndAttributesToArray guards against
// a *TypeError produced by an Array accessor (as opposed to a Value
// accessor) leaving TypeName unpopulated (violating TypeName's "always
// populated, for every Kind" doc comment) and mis-attributing its message
// to "Value.<method>" instead of "Array.<method>".
func TestArray_TypeError_PopulatesTypeNameAndAttributesToArray(t *testing.T) {
	arr := NewInt32Array([]int32{1, 2, 3})
	_, err := arr.Strings()
	if err == nil {
		t.Fatalf("expected an error calling Strings() on an int32 array")
	}
	var te *TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected *TypeError, got %T", err)
	}
	if te.TypeName.IsZero() {
		t.Fatalf("TypeError.TypeName was not populated for an Array-sourced error")
	}
	wantTypeName := QName{Namespace, "ArrayOfInt"}
	if te.TypeName != wantTypeName {
		t.Fatalf("TypeError.TypeName = %+v, want %+v", te.TypeName, wantTypeName)
	}
	wantMsg := "xmlda: Array.Strings: array has element type int, not the requested type"
	if te.Error() != wantMsg {
		t.Fatalf("Error() = %q, want %q", te.Error(), wantMsg)
	}
}

// TestNewBytes_NilVsEmpty guards the nil-vs-non-nil-empty distinction
// docs/specification/type-mapping.md documents as meaningful for
// base64Binary: NewBytes(nil) and NewBytes([]byte{}) must round-trip
// through Bytes() as nil and non-nil-empty respectively, not both
// collapsing to nil.
func TestNewBytes_NilVsEmpty(t *testing.T) {
	nilGot, err := NewBytes(nil).Bytes()
	if err != nil {
		t.Fatalf("Bytes() on NewBytes(nil): %v", err)
	}
	if nilGot != nil {
		t.Fatalf("NewBytes(nil).Bytes() = %#v, want nil", nilGot)
	}

	emptyGot, err := NewBytes([]byte{}).Bytes()
	if err != nil {
		t.Fatalf("Bytes() on NewBytes([]byte{}): %v", err)
	}
	if emptyGot == nil {
		t.Fatalf("NewBytes([]byte{}).Bytes() = nil, want non-nil empty slice")
	}
	if len(emptyGot) != 0 {
		t.Fatalf("NewBytes([]byte{}).Bytes() = %#v, want empty", emptyGot)
	}
}

// TestNewNil_ArrayTypedValue_ReportsArrayKind guards against NewNil (and
// the xsi:nil decode path) hardcoding KindScalar regardless of what the
// declared xsi:type actually denotes: a nil value declared as an
// ArrayOf<X> type must still report Kind() == KindArray.
func TestNewNil_ArrayTypedValue_ReportsArrayKind(t *testing.T) {
	arrayTypeName := QName{Namespace, "ArrayOfInt"}
	v := NewNil(arrayTypeName)
	if !v.IsNil() {
		t.Fatalf("expected IsNil() true")
	}
	if v.Kind() != KindArray {
		t.Fatalf("Kind() = %s, want %s", v.Kind(), KindArray)
	}
	if v.Type() != TypeInt {
		t.Fatalf("Type() = %s, want %s", v.Type(), TypeInt)
	}

	got := roundTrip(t, v)
	if !got.IsNil() {
		t.Fatalf("round-tripped value: expected IsNil() true")
	}
	if got.Kind() != KindArray {
		t.Fatalf("round-tripped value: Kind() = %s, want %s", got.Kind(), KindArray)
	}
	if got.TypeName() != arrayTypeName {
		t.Fatalf("round-tripped value: TypeName() = %+v, want %+v", got.TypeName(), arrayTypeName)
	}

	// A scalar-typed nil value must still report KindScalar (unchanged
	// behavior), and an unrecognized type must report KindUnknown.
	scalarNil := NewNil(QName{XSDNamespace, "int"})
	if scalarNil.Kind() != KindScalar {
		t.Fatalf("scalar-typed nil: Kind() = %s, want %s", scalarNil.Kind(), KindScalar)
	}
	vendorNil := NewNil(QName{"http://example.com/vendor", "WeirdThing"})
	if vendorNil.Kind() != KindUnknown {
		t.Fatalf("unrecognized-type nil: Kind() = %s, want %s", vendorNil.Kind(), KindUnknown)
	}
}
