package server

import (
	"math"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// coerceToReqType attempts to convert v to the scalar type named by
// reqType (Read's optional per-item ReqType, REQ-TYPE-006). A nil reqType,
// or one naming "anyType" (or the zero QName), means no coercion is
// requested — v's own canonical type is returned unchanged.
//
// This implements numeric-to-numeric coercion with explicit range
// checking; any other conversion (including anything involving array or
// unknown-type values, or a non-numeric source/target) is not supported
// and fails rather than silently truncating or guessing — callers map a
// false return to E_BADTYPE (REQ-WRITE-004/REQ-READ per §3.1.3).
func coerceToReqType(v xmlda.Value, reqType *xmlda.QName) (xmlda.Value, bool) {
	if reqType == nil || reqType.IsZero() || reqType.Local == "anyType" {
		return v, true
	}
	// ReqType is an xsd:QName, so its namespace is part of its identity.
	// Matching on the local name alone accepted e.g. ReqType="vendor:int"
	// from any namespace at all and silently coerced to xsd:int — a type
	// this server does not actually know, which is E_BADTYPE. Scalars live
	// in the XSD namespace; anything else (including the OPC namespace's
	// own ArrayOf<X> types, which this coercion does not support) is
	// rejected below.
	if reqType.Space != xmlda.XSDNamespace {
		return xmlda.Value{}, false
	}
	target := xmlda.ScalarType(reqType.Local)
	if v.Kind() != xmlda.KindScalar {
		return xmlda.Value{}, false
	}
	if target == v.Type() {
		return v, true
	}
	// Integer-to-integer coercions go through exact integer arithmetic,
	// never float64: float64 only represents integers exactly up to 2^53,
	// so routing e.g. a large int64/uint64 through NumericAsFloat64 (as
	// the fallback below does) would silently produce a different, wrong
	// value instead of the documented "explicit range checking, never
	// silently truncating" behavior. Float64 is used as an intermediate
	// only when the source or target is itself a float/double, where that
	// imprecision is inherent to the target representation, not a bug.
	if isIntegerScalarType(v.Type()) && isIntegerScalarType(target) {
		return integerToScalar(v, target)
	}
	f, ok := v.NumericAsFloat64()
	if !ok {
		return xmlda.Value{}, false
	}
	return numericToScalar(f, target)
}

func isIntegerScalarType(t xmlda.ScalarType) bool {
	switch t {
	case xmlda.TypeLong, xmlda.TypeInt, xmlda.TypeShort, xmlda.TypeByte,
		xmlda.TypeUnsignedLong, xmlda.TypeUnsignedInt, xmlda.TypeUnsignedShort, xmlda.TypeUnsignedByte:
		return true
	default:
		return false
	}
}

// integerToScalar coerces an integer-typed v to target using exact 64-bit
// integer arithmetic (via int64ToScalar/uint64ToScalar), preserving values
// float64 cannot represent exactly.
func integerToScalar(v xmlda.Value, target xmlda.ScalarType) (xmlda.Value, bool) {
	switch v.Type() {
	case xmlda.TypeLong:
		i, err := v.Int64()
		if err != nil {
			return xmlda.Value{}, false
		}
		return int64ToScalar(i, target)
	case xmlda.TypeInt:
		i, err := v.Int32()
		if err != nil {
			return xmlda.Value{}, false
		}
		return int64ToScalar(int64(i), target)
	case xmlda.TypeShort:
		i, err := v.Int16()
		if err != nil {
			return xmlda.Value{}, false
		}
		return int64ToScalar(int64(i), target)
	case xmlda.TypeByte:
		i, err := v.Int8()
		if err != nil {
			return xmlda.Value{}, false
		}
		return int64ToScalar(int64(i), target)
	case xmlda.TypeUnsignedLong:
		u, err := v.Uint64()
		if err != nil {
			return xmlda.Value{}, false
		}
		return uint64ToScalar(u, target)
	case xmlda.TypeUnsignedInt:
		u, err := v.Uint32()
		if err != nil {
			return xmlda.Value{}, false
		}
		return uint64ToScalar(uint64(u), target)
	case xmlda.TypeUnsignedShort:
		u, err := v.Uint16()
		if err != nil {
			return xmlda.Value{}, false
		}
		return uint64ToScalar(uint64(u), target)
	case xmlda.TypeUnsignedByte:
		u, err := v.Uint8()
		if err != nil {
			return xmlda.Value{}, false
		}
		return uint64ToScalar(uint64(u), target)
	default:
		return xmlda.Value{}, false
	}
}

// int64ToScalar range-checks and converts a signed 64-bit source to
// target without an intermediate float64 conversion.
func int64ToScalar(i int64, target xmlda.ScalarType) (xmlda.Value, bool) {
	switch target {
	case xmlda.TypeLong:
		return xmlda.NewInt64(i), true
	case xmlda.TypeInt:
		if i < math.MinInt32 || i > math.MaxInt32 {
			return xmlda.Value{}, false
		}
		return xmlda.NewInt32(int32(i)), true
	case xmlda.TypeShort:
		if i < math.MinInt16 || i > math.MaxInt16 {
			return xmlda.Value{}, false
		}
		return xmlda.NewInt16(int16(i)), true
	case xmlda.TypeByte:
		if i < math.MinInt8 || i > math.MaxInt8 {
			return xmlda.Value{}, false
		}
		return xmlda.NewInt8(int8(i)), true
	case xmlda.TypeUnsignedLong:
		if i < 0 {
			return xmlda.Value{}, false
		}
		return xmlda.NewUint64(uint64(i)), true
	case xmlda.TypeUnsignedInt:
		if i < 0 || i > math.MaxUint32 {
			return xmlda.Value{}, false
		}
		return xmlda.NewUint32(uint32(i)), true
	case xmlda.TypeUnsignedShort:
		if i < 0 || i > math.MaxUint16 {
			return xmlda.Value{}, false
		}
		return xmlda.NewUint16(uint16(i)), true
	case xmlda.TypeUnsignedByte:
		if i < 0 || i > math.MaxUint8 {
			return xmlda.Value{}, false
		}
		return xmlda.NewUint8(uint8(i)), true
	default:
		return xmlda.Value{}, false
	}
}

// uint64ToScalar range-checks and converts an unsigned 64-bit source to
// target without an intermediate float64 conversion.
func uint64ToScalar(u uint64, target xmlda.ScalarType) (xmlda.Value, bool) {
	switch target {
	case xmlda.TypeUnsignedLong:
		return xmlda.NewUint64(u), true
	case xmlda.TypeUnsignedInt:
		if u > math.MaxUint32 {
			return xmlda.Value{}, false
		}
		return xmlda.NewUint32(uint32(u)), true
	case xmlda.TypeUnsignedShort:
		if u > math.MaxUint16 {
			return xmlda.Value{}, false
		}
		return xmlda.NewUint16(uint16(u)), true
	case xmlda.TypeUnsignedByte:
		if u > math.MaxUint8 {
			return xmlda.Value{}, false
		}
		return xmlda.NewUint8(uint8(u)), true
	case xmlda.TypeLong:
		if u > math.MaxInt64 {
			return xmlda.Value{}, false
		}
		return xmlda.NewInt64(int64(u)), true
	case xmlda.TypeInt:
		if u > math.MaxInt32 {
			return xmlda.Value{}, false
		}
		return xmlda.NewInt32(int32(u)), true
	case xmlda.TypeShort:
		if u > math.MaxInt16 {
			return xmlda.Value{}, false
		}
		return xmlda.NewInt16(int16(u)), true
	case xmlda.TypeByte:
		if u > math.MaxInt8 {
			return xmlda.Value{}, false
		}
		return xmlda.NewInt8(int8(u)), true
	default:
		return xmlda.Value{}, false
	}
}

func numericToScalar(f float64, target xmlda.ScalarType) (xmlda.Value, bool) {
	switch target {
	case xmlda.TypeFloat:
		// A finite double beyond ±math.MaxFloat32 becomes ±Inf under Go's
		// float64→float32 conversion. Handing that back as the item's
		// value — with an empty ResultID and the backend's quality, which
		// is usually good — is exactly the silent substitution every other
		// target in this function refuses: the client reads "INF" as a
		// valid measurement. NaN and an already-infinite source are
		// faithful representations rather than overflow and still pass
		// through; underflow to zero is ordinary rounding into the
		// target's representable range and stays allowed.
		g := float32(f)
		if math.IsInf(float64(g), 0) && !math.IsInf(f, 0) {
			return xmlda.Value{}, false
		}
		return xmlda.NewFloat32(g), true
	case xmlda.TypeDouble:
		return xmlda.NewFloat64(f), true
	}
	// Every remaining target below is an integer type. NaN compares false
	// against every bound check below (IEEE-754: any comparison involving
	// NaN is false), so without this guard NaN would fall through every
	// range check and reach e.g. int64(f) — whose result for a value with
	// no integer representation is documented by the Go spec as
	// implementation-dependent, silently producing a bogus in-range-looking
	// integer instead of failing as this function documents. +Inf/-Inf are
	// unaffected: comparisons against them behave normally, so they're
	// already correctly rejected by the bound checks below.
	if math.IsNaN(f) {
		return xmlda.Value{}, false
	}
	switch target {
	case xmlda.TypeLong:
		// math.MinInt64/MaxInt64, converted to float64 for comparison,
		// round to -2^63/+2^63 (float64 cannot represent 2^63-1 exactly at
		// this magnitude) — using them directly as bounds would let a
		// value exactly at the rounded boundary pass this check and then
		// silently wrap during int64(f) (e.g. f == 2^63 gives
		// int64(f) == math.MinInt64) instead of failing as documented.
		// -2^63 and +2^63 are themselves exact powers of two, so writing
		// them directly avoids the rounding entirely.
		if f < -9223372036854775808.0 || f >= 9223372036854775808.0 {
			return xmlda.Value{}, false
		}
		return xmlda.NewInt64(int64(f)), true
	case xmlda.TypeInt:
		if f < math.MinInt32 || f > math.MaxInt32 {
			return xmlda.Value{}, false
		}
		return xmlda.NewInt32(int32(f)), true
	case xmlda.TypeShort:
		if f < math.MinInt16 || f > math.MaxInt16 {
			return xmlda.Value{}, false
		}
		return xmlda.NewInt16(int16(f)), true
	case xmlda.TypeByte:
		if f < math.MinInt8 || f > math.MaxInt8 {
			return xmlda.Value{}, false
		}
		return xmlda.NewInt8(int8(f)), true
	case xmlda.TypeUnsignedLong:
		// See the TypeLong case above: 2^64 is an exact power of two,
		// used directly (rather than math.MaxUint64, which would round
		// the same way) so a value exactly at the boundary is correctly
		// rejected instead of silently truncating via uint64(f).
		if f < 0 || f >= 18446744073709551616.0 {
			return xmlda.Value{}, false
		}
		return xmlda.NewUint64(uint64(f)), true
	case xmlda.TypeUnsignedInt:
		if f < 0 || f > math.MaxUint32 {
			return xmlda.Value{}, false
		}
		return xmlda.NewUint32(uint32(f)), true
	case xmlda.TypeUnsignedShort:
		if f < 0 || f > math.MaxUint16 {
			return xmlda.Value{}, false
		}
		return xmlda.NewUint16(uint16(f)), true
	case xmlda.TypeUnsignedByte:
		if f < 0 || f > math.MaxUint8 {
			return xmlda.Value{}, false
		}
		return xmlda.NewUint8(uint8(f)), true
	default:
		return xmlda.Value{}, false
	}
}

// applyReqType applies a subscribed item's requested type to one outcome
// on its way to the wire, mirroring what handleRead does inline for a
// Read result.
//
// Coercion lives here rather than in the subscription engine on purpose:
// it is pure xmlda.Value logic with no bearing on scheduling, buffering
// or change detection, and the engine stores every sample in the
// backend's native type. Both Subscribe's initial values and every
// SubscriptionPolledRefresh entry pass through this one function, so a
// subscription cannot report one type at creation and another later.
//
// A value that cannot be converted becomes E_BADTYPE with no value, the
// same outcome Read produces (REQ-TYPE-006).
func applyReqType(sample backend.ItemSample, haveSample bool, resultID xmlda.ErrorCode, reqType *xmlda.QName) (backend.ItemSample, bool, xmlda.ErrorCode) {
	if !haveSample || reqType == nil {
		return sample, haveSample, resultID
	}
	// A Bad-quality item with no last-known value carries no value to
	// coerce — xmlda.NewNil(typ) by contract, or an unconstructed Value
	// from a backend that skipped that step. Coercion would fail on the
	// missing value and report E_BADTYPE, telling the client "wrong type"
	// where "sensor unreadable" is the truth, and discarding the Quality
	// that carried it. The requested type is simply not applicable here.
	if !sample.Value.IsValid() || sample.Value.IsNil() {
		return sample, haveSample, resultID
	}
	coerced, ok := coerceToReqType(sample.Value, reqType)
	if !ok {
		return backend.ItemSample{}, false, xmlda.ErrBadType
	}
	sample.Value = coerced
	return sample, true, resultID
}
