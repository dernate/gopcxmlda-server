package server

import (
	"math"
	"testing"

	"github.com/dernate/gopcxmlda-server/xmlda"
)

func reqTypeOf(local string) *xmlda.QName {
	qn := xmlda.QName{Space: xmlda.XSDNamespace, Local: local}
	return &qn
}

func TestCoerceToReqType_NilOrAnyType_Unchanged(t *testing.T) {
	v := xmlda.NewInt32(5)
	if got, ok := coerceToReqType(v, nil); !ok || !got.Equal(v) {
		t.Fatalf("nil ReqType: got (%v, %v), want (%v, true)", got, ok, v)
	}
	if got, ok := coerceToReqType(v, reqTypeOf("anyType")); !ok || !got.Equal(v) {
		t.Fatalf("ReqType=anyType: got (%v, %v), want (%v, true)", got, ok, v)
	}
}

func TestCoerceToReqType_SameType_Unchanged(t *testing.T) {
	v := xmlda.NewInt32(5)
	got, ok := coerceToReqType(v, reqTypeOf("int"))
	if !ok || !got.Equal(v) {
		t.Fatalf("got (%v, %v), want (%v, true)", got, ok, v)
	}
}

func TestCoerceToReqType_NonNumericTarget_Fails(t *testing.T) {
	v := xmlda.NewInt32(5)
	if _, ok := coerceToReqType(v, reqTypeOf("string")); ok {
		t.Fatalf("expected coercion to a non-numeric target to fail")
	}
}

func TestCoerceToReqType_NumericToNumeric_Succeeds(t *testing.T) {
	v := xmlda.NewFloat64(42.0)
	got, ok := coerceToReqType(v, reqTypeOf("int"))
	if !ok {
		t.Fatalf("expected double->int coercion to succeed")
	}
	i32, err := got.Int32()
	if err != nil || i32 != 42 {
		t.Fatalf("got (%d, %v), want (42, nil)", i32, err)
	}
}

// TestNumericToScalar_Int64Boundaries specifically targets the float64
// rounding gap: math.MaxInt64/MinInt64, converted to float64, round to
// +/-2^63 — a value exactly at that boundary must be rejected rather than
// silently wrapping via int64(f).
func TestNumericToScalar_Int64Boundaries(t *testing.T) {
	const (
		twoTo63    = 9223372036854775808.0 // 2^63, one past math.MaxInt64
		negTwoTo63 = -9223372036854775808.0
	)
	cases := []struct {
		name    string
		f       float64
		wantOK  bool
		wantI64 int64
	}{
		{"MinInt64 exactly (representable, valid)", negTwoTo63, true, -9223372036854775808},
		{"just above MinInt64", negTwoTo63 + 1024, true, 0}, // value itself not asserted, only ok
		{"just below 2^63 (valid)", twoTo63 - 2048, true, 0},
		{"exactly 2^63 (one past MaxInt64, must fail)", twoTo63, false, 0},
		{"far below MinInt64 (must fail)", negTwoTo63 * 2, false, 0},
		{"far above 2^63 (must fail)", twoTo63 * 2, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := numericToScalar(tc.f, xmlda.TypeLong)
			if ok != tc.wantOK {
				t.Fatalf("numericToScalar(%v, TypeLong) ok = %v, want %v (value %+v)", tc.f, ok, tc.wantOK, got)
			}
			if ok && tc.wantI64 != 0 {
				i64, err := got.Int64()
				if err != nil || i64 != tc.wantI64 {
					t.Fatalf("got (%d, %v), want (%d, nil)", i64, err, tc.wantI64)
				}
			}
		})
	}
}

// TestNumericToScalar_Uint64Boundaries mirrors the int64 case for
// TypeUnsignedLong: math.MaxUint64 converted to float64 rounds up to 2^64.
func TestNumericToScalar_Uint64Boundaries(t *testing.T) {
	const twoTo64 = 18446744073709551616.0 // 2^64, one past math.MaxUint64

	cases := []struct {
		name   string
		f      float64
		wantOK bool
	}{
		{"zero (valid)", 0, true},
		{"just below 2^64 (valid)", twoTo64 - 4096, true},
		{"exactly 2^64 (one past MaxUint64, must fail)", twoTo64, false},
		{"far above 2^64 (must fail)", twoTo64 * 2, false},
		{"negative (must fail)", -1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := numericToScalar(tc.f, xmlda.TypeUnsignedLong); ok != tc.wantOK {
				t.Fatalf("numericToScalar(%v, TypeUnsignedLong) ok = %v, want %v", tc.f, ok, tc.wantOK)
			}
		})
	}
}

// TestCoerceToReqType_LargeIntegerPrecision guards against a regression
// where integer-to-integer coercion routed through NumericAsFloat64
// (float64 only represents integers exactly up to 2^53) and silently
// produced a different, wrong value for large in-range int64/uint64
// values instead of preserving them exactly.
func TestCoerceToReqType_LargeIntegerPrecision(t *testing.T) {
	const big = int64(9223372036854774000) // within int64 range, >2^53

	v := xmlda.NewInt64(big)
	got, ok := coerceToReqType(v, reqTypeOf("unsignedLong"))
	if !ok {
		t.Fatalf("expected long->unsignedLong coercion to succeed")
	}
	u64, err := got.Uint64()
	if err != nil || u64 != uint64(big) {
		t.Fatalf("got (%d, %v), want (%d, nil) — precision lost in coercion", u64, err, big)
	}

	// Round-trip the other direction too: unsignedLong -> long.
	uv := xmlda.NewUint64(uint64(big))
	got2, ok := coerceToReqType(uv, reqTypeOf("long"))
	if !ok {
		t.Fatalf("expected unsignedLong->long coercion to succeed")
	}
	i64, err := got2.Int64()
	if err != nil || i64 != big {
		t.Fatalf("got (%d, %v), want (%d, nil) — precision lost in coercion", i64, err, big)
	}
}

// TestCoerceToReqType_IntegerOutOfRange_Fails exercises the exact-integer
// range checks in int64ToScalar/uint64ToScalar directly through
// coerceToReqType (integer -> integer path), independent of the
// float64-based numericToScalar path already covered above.
func TestCoerceToReqType_IntegerOutOfRange_Fails(t *testing.T) {
	if _, ok := coerceToReqType(xmlda.NewInt64(-1), reqTypeOf("unsignedLong")); ok {
		t.Fatalf("expected negative long -> unsignedLong to fail")
	}
	if _, ok := coerceToReqType(xmlda.NewInt32(256), reqTypeOf("unsignedByte")); ok {
		t.Fatalf("expected 256 (int) -> unsignedByte to fail")
	}
	if _, ok := coerceToReqType(xmlda.NewUint64(math.MaxUint64), reqTypeOf("long")); ok {
		t.Fatalf("expected MaxUint64 -> long to fail")
	}
	got, ok := coerceToReqType(xmlda.NewInt32(200), reqTypeOf("unsignedByte"))
	if !ok {
		t.Fatalf("expected 200 (int) -> unsignedByte to succeed")
	}
	u8, err := got.Uint8()
	if err != nil || u8 != 200 {
		t.Fatalf("got (%d, %v), want (200, nil)", u8, err)
	}
}

// TestNumericToScalar_NaN_RejectedForIntegerTargets reproduces the gap
// where every integer bound check has the form "f < min || f > max": for
// f = NaN, IEEE-754 makes both comparisons false, so the check never
// caught it and the code fell through to an implementation-defined
// float-to-int conversion (e.g. int32(NaN) == math.MinInt32) instead of
// failing as coerceToReqType's own doc comment promises ("never silently
// truncating or guessing").
func TestNumericToScalar_NaN_RejectedForIntegerTargets(t *testing.T) {
	integerTargets := []xmlda.ScalarType{
		xmlda.TypeLong, xmlda.TypeInt, xmlda.TypeShort, xmlda.TypeByte,
		xmlda.TypeUnsignedLong, xmlda.TypeUnsignedInt, xmlda.TypeUnsignedShort, xmlda.TypeUnsignedByte,
	}
	for _, target := range integerTargets {
		t.Run(string(target), func(t *testing.T) {
			if _, ok := numericToScalar(math.NaN(), target); ok {
				t.Fatalf("numericToScalar(NaN, %s): expected failure, got success", target)
			}
		})
	}
}

// TestNumericToScalar_NaN_AcceptedForFloatTargets is the control case: NaN
// is a valid xsd:float/xsd:double lexical value, so it must still be
// accepted (and preserved) when the target itself is a float type.
func TestNumericToScalar_NaN_AcceptedForFloatTargets(t *testing.T) {
	got, ok := numericToScalar(math.NaN(), xmlda.TypeDouble)
	if !ok {
		t.Fatalf("expected NaN -> double to succeed (NaN is a valid xsd:double lexical value)")
	}
	f, err := got.Float64()
	if err != nil || !math.IsNaN(f) {
		t.Fatalf("got (%v, %v), want NaN preserved", f, err)
	}
}

// TestNumericToScalar_Inf_StillRejectedForIntegerTargets is the other
// control case: +Inf/-Inf were never affected by the NaN bug (normal
// comparisons against them work fine), and must keep failing exactly as
// before.
func TestNumericToScalar_Inf_StillRejectedForIntegerTargets(t *testing.T) {
	if _, ok := numericToScalar(math.Inf(1), xmlda.TypeLong); ok {
		t.Fatalf("expected +Inf -> long to fail")
	}
	if _, ok := numericToScalar(math.Inf(-1), xmlda.TypeLong); ok {
		t.Fatalf("expected -Inf -> long to fail")
	}
}

func TestNumericToScalar_OtherWidths_RangeChecked(t *testing.T) {
	cases := []struct {
		name   string
		f      float64
		target xmlda.ScalarType
		wantOK bool
	}{
		{"int32 in range", 42, xmlda.TypeInt, true},
		{"int32 just out of range", 2147483648, xmlda.TypeInt, false},
		{"int16 in range", 42, xmlda.TypeShort, true},
		{"int16 just out of range", 32768, xmlda.TypeShort, false},
		{"int8 in range", 42, xmlda.TypeByte, true},
		{"int8 just out of range", 128, xmlda.TypeByte, false},
		{"uint32 in range", 42, xmlda.TypeUnsignedInt, true},
		{"uint32 just out of range", 4294967296, xmlda.TypeUnsignedInt, false},
		{"uint16 in range", 42, xmlda.TypeUnsignedShort, true},
		{"uint16 just out of range", 65536, xmlda.TypeUnsignedShort, false},
		{"uint8 in range", 42, xmlda.TypeUnsignedByte, true},
		{"uint8 just out of range", 256, xmlda.TypeUnsignedByte, false},
		{"unsupported target", 42, xmlda.TypeString, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := numericToScalar(tc.f, tc.target); ok != tc.wantOK {
				t.Fatalf("numericToScalar(%v, %s) ok = %v, want %v", tc.f, tc.target, ok, tc.wantOK)
			}
		})
	}
}

// TestIntegerToScalar_EveryTargetWidth_BoundaryChecked directly targets
// the exact-integer coercion path (integerToScalar/int64ToScalar/
// uint64ToScalar) — the boundary-heaviest code in this file, per
// testing-strategy.md's own emphasis on width-boundary coverage, but
// previously the least-tested part of it (16-29% function coverage):
// earlier tests only ever exercised a handful of the 8 source types x 8
// target types this path dispatches across. Every source-type switch
// branch in integerToScalar, and every target-type range check in
// int64ToScalar/uint64ToScalar, is hit at least once here, each with a
// boundary value one step inside and (where a failure mode exists) one
// step outside the legal range.
func TestIntegerToScalar_EveryTargetWidth_BoundaryChecked(t *testing.T) {
	cases := []struct {
		name   string
		v      xmlda.Value
		target string
		wantOK bool
	}{
		// --- signed source (int64ToScalar), every target ---
		{"long -> long: always fits", xmlda.NewInt64(math.MaxInt64), "long", true},
		{"long -> int: in range", xmlda.NewInt64(math.MaxInt32), "int", true},
		{"long -> int: over range fails", xmlda.NewInt64(math.MaxInt32 + 1), "int", false},
		{"long -> short: in range", xmlda.NewInt64(math.MinInt16), "short", true},
		{"long -> short: under range fails", xmlda.NewInt64(math.MinInt16 - 1), "short", false},
		{"long -> byte: in range", xmlda.NewInt64(math.MaxInt8), "byte", true},
		{"long -> byte: over range fails", xmlda.NewInt64(math.MaxInt8 + 1), "byte", false},
		{"long -> unsignedLong: negative fails", xmlda.NewInt64(-1), "unsignedLong", false},
		{"long -> unsignedLong: non-negative succeeds", xmlda.NewInt64(math.MaxInt64), "unsignedLong", true},
		{"long -> unsignedInt: in range", xmlda.NewInt64(math.MaxUint32), "unsignedInt", true},
		{"long -> unsignedInt: over range fails", xmlda.NewInt64(math.MaxUint32 + 1), "unsignedInt", false},
		{"long -> unsignedInt: negative fails", xmlda.NewInt64(-1), "unsignedInt", false},
		{"long -> unsignedShort: in range", xmlda.NewInt64(math.MaxUint16), "unsignedShort", true},
		{"long -> unsignedShort: over range fails", xmlda.NewInt64(math.MaxUint16 + 1), "unsignedShort", false},
		{"long -> unsignedByte: in range", xmlda.NewInt64(math.MaxUint8), "unsignedByte", true},
		{"long -> unsignedByte: over range fails", xmlda.NewInt64(math.MaxUint8 + 1), "unsignedByte", false},

		// --- remaining signed source widths (integerToScalar's dispatch) ---
		{"int -> long", xmlda.NewInt32(math.MaxInt32), "long", true},
		{"short -> long", xmlda.NewInt16(math.MinInt16), "long", true},
		{"byte -> long", xmlda.NewInt8(math.MinInt8), "long", true},

		// --- unsigned source (uint64ToScalar), every target ---
		{"unsignedLong -> unsignedLong: always fits", xmlda.NewUint64(math.MaxUint64), "unsignedLong", true},
		{"unsignedLong -> unsignedInt: in range", xmlda.NewUint64(math.MaxUint32), "unsignedInt", true},
		{"unsignedLong -> unsignedInt: over range fails", xmlda.NewUint64(math.MaxUint32 + 1), "unsignedInt", false},
		{"unsignedLong -> unsignedShort: in range", xmlda.NewUint64(math.MaxUint16), "unsignedShort", true},
		{"unsignedLong -> unsignedShort: over range fails", xmlda.NewUint64(math.MaxUint16 + 1), "unsignedShort", false},
		{"unsignedLong -> unsignedByte: in range", xmlda.NewUint64(math.MaxUint8), "unsignedByte", true},
		{"unsignedLong -> unsignedByte: over range fails", xmlda.NewUint64(math.MaxUint8 + 1), "unsignedByte", false},
		{"unsignedLong -> long: in range", xmlda.NewUint64(math.MaxInt64), "long", true},
		{"unsignedLong -> long: over range fails", xmlda.NewUint64(math.MaxInt64 + 1), "long", false},
		{"unsignedLong -> int: in range", xmlda.NewUint64(math.MaxInt32), "int", true},
		{"unsignedLong -> int: over range fails", xmlda.NewUint64(math.MaxInt32 + 1), "int", false},
		{"unsignedLong -> short: in range", xmlda.NewUint64(math.MaxInt16), "short", true},
		{"unsignedLong -> short: over range fails", xmlda.NewUint64(math.MaxInt16 + 1), "short", false},
		{"unsignedLong -> byte: in range", xmlda.NewUint64(math.MaxInt8), "byte", true},
		{"unsignedLong -> byte: over range fails", xmlda.NewUint64(math.MaxInt8 + 1), "byte", false},

		// --- remaining unsigned source widths (integerToScalar's dispatch) ---
		{"unsignedInt -> unsignedLong", xmlda.NewUint32(math.MaxUint32), "unsignedLong", true},
		{"unsignedShort -> unsignedLong", xmlda.NewUint16(math.MaxUint16), "unsignedLong", true},
		{"unsignedByte -> unsignedLong", xmlda.NewUint8(math.MaxUint8), "unsignedLong", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := coerceToReqType(tc.v, reqTypeOf(tc.target))
			if ok != tc.wantOK {
				t.Fatalf("coerceToReqType(%v, %s) ok = %v (got %v), want %v", tc.v, tc.target, ok, got, tc.wantOK)
			}
		})
	}
}
