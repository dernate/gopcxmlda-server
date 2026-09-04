package xmlda

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ScalarType identifies an OPC XML-DA scalar wire type by its xsi:type
// local name (for array types, the type of one element). See
// docs/specification/type-mapping.md for the full XSD-to-Go mapping.
type ScalarType string

// Standard OPC XML-DA / XSD scalar types, per §2.7.1 of the specification.
const (
	TypeString        ScalarType = "string"        // VT_BSTR
	TypeBoolean       ScalarType = "boolean"       // VT_BOOL
	TypeFloat         ScalarType = "float"         // VT_R4
	TypeDouble        ScalarType = "double"        // VT_R8
	TypeDecimal       ScalarType = "decimal"       // VT_CY
	TypeLong          ScalarType = "long"          // VT_I8
	TypeInt           ScalarType = "int"           // VT_I4
	TypeShort         ScalarType = "short"         // VT_I2
	TypeByte          ScalarType = "byte"          // VT_I1
	TypeUnsignedLong  ScalarType = "unsignedLong"  // VT_UI8
	TypeUnsignedInt   ScalarType = "unsignedInt"   // VT_UI4
	TypeUnsignedShort ScalarType = "unsignedShort" // VT_UI2
	TypeUnsignedByte  ScalarType = "unsignedByte"  // VT_UI1
	TypeBase64Binary  ScalarType = "base64Binary"  // byte array
	TypeDateTime      ScalarType = "dateTime"      // VT_DATE
	TypeTime          ScalarType = "time"          // VT_DATE, see OQ-12
	TypeDate          ScalarType = "date"          // VT_DATE, see OQ-12
	TypeDuration      ScalarType = "duration"      // VT_BSTR
	TypeQName         ScalarType = "QName"         // no Variant equivalent
	TypeAnyType       ScalarType = "anyType"       // array element type only
)

// Kind identifies which shape a Value holds.
type Kind uint8

const (
	// KindScalar is a single scalar value of one ScalarType.
	KindScalar Kind = iota
	// KindArray is an ArrayOf<X> value; see Value.Array.
	KindArray
	// KindUnknown is a value whose xsi:type was not recognized (a vendor
	// or future type). Its exact wire bytes are preserved for round-trip;
	// see ADR-003.
	KindUnknown
	// KindQuality is an OPCQuality-typed value. It is the one complex
	// type the specification puts in a <Value> position: standard item
	// property 3, "Item Quality", has the data type OPCQuality
	// (§3.1.10 p.40). Without it a backend could serve every standard
	// property except that one. See Value.Quality and NewQualityValue.
	KindQuality
)

// String returns a human-readable name for k, used in error messages.
func (k Kind) String() string {
	switch k {
	case KindScalar:
		return "scalar"
	case KindArray:
		return "array"
	case KindUnknown:
		return "unknown"
	case KindQuality:
		return "quality"
	default:
		return "invalid"
	}
}

// decimalPattern validates the lexical form of xsd:decimal per the XSD
// Part 2 grammar: an optional sign, and digits on at least one side of an
// optional decimal point — a digit is required on only one side, not
// both, so "210." and ".5" are both legal (the specification's own
// worked examples).
var decimalPattern = regexp.MustCompile(`^[+-]?([0-9]+(\.[0-9]*)?|\.[0-9]+)$`)

// durationPattern validates the lexical form of xsd:duration per the XSD
// Part 2 grammar: an optional sign, a mandatory "P", at least one
// component, and a "T" separator required if and only if a time component
// follows. Without it any string at all was accepted as a duration and
// echoed straight back onto the wire — the one scalar type with no
// validation, while xsd:decimal right below has had it all along.
var durationPattern = regexp.MustCompile(
	`^-?P(?:\d+Y)?(?:\d+M)?(?:\d+D)?(?:T(?:\d+H)?(?:\d+M)?(?:\d+(?:\.\d+)?S)?)?$`)

// ValidDuration reports whether s is a well-formed xsd:duration literal.
// The regexp alone would accept the degenerate "P" and "PT", which the
// grammar forbids: at least one component is required.
func ValidDuration(s string) bool {
	if s == "P" || s == "PT" || s == "-P" || s == "-PT" {
		return false
	}
	if strings.HasSuffix(s, "T") {
		return false
	}
	return durationPattern.MatchString(s)
}

// Decimal preserves the exact lexical xsd:decimal wire text. VT_CY (the
// OPC Variant type decimal maps to) is fixed-point with no exact float64
// representation, so Decimal is a validated string, not a float64 — see
// ADR-002.
type Decimal string

// NewDecimal validates s as an xsd:decimal literal and returns it as a
// Decimal. It returns an error if s is not a valid decimal lexical form.
func NewDecimal(s string) (Decimal, error) {
	if !decimalPattern.MatchString(s) {
		return "", fmt.Errorf("xmlda: %q is not a valid xsd:decimal literal", s)
	}
	return Decimal(s), nil
}

// NewDecimalFromFloat64 formats f as a Decimal. Callers needing exact
// wire-format fidelity should prefer NewDecimal with the original literal
// text instead, since the float64-to-decimal-text conversion may not
// reproduce the exact digits of a value that originated as decimal text.
//
// It returns an error for NaN and \u00b1Inf: xsd:decimal has no lexical form
// for either (unlike xsd:double, which spells them "NaN"/"INF"), and
// strconv.FormatFloat would otherwise hand back "NaN"/"+Inf" \u2014 text that
// NewDecimal rejects and that nothing downstream revalidates before it
// reaches the wire.
func NewDecimalFromFloat64(f float64) (Decimal, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", fmt.Errorf("xmlda: %v has no xsd:decimal lexical representation", f)
	}
	return Decimal(strconv.FormatFloat(f, 'f', -1, 64)), nil
}

// Float64 parses d as a float64, accepting the precision loss inherent in
// that conversion.
func (d Decimal) Float64() (float64, error) {
	f, err := strconv.ParseFloat(string(d), 64)
	if err != nil {
		return 0, fmt.Errorf("xmlda: Decimal %q: %w", string(d), err)
	}
	return f, nil
}

// String returns the exact decimal literal text.
func (d Decimal) String() string { return string(d) }

// RawValue preserves an unrecognized <Value> element exactly as received:
// its declared xsi:type and its verbatim inner XML bytes. See ADR-003.
type RawValue struct {
	// TypeName is the xsi:type this value declared, resolved to a QName.
	TypeName QName
	// Namespaces are the prefix -> URI bindings the captured content
	// references but does not itself declare, collected from the document
	// it was decoded out of.
	//
	// Without them the round trip is not one: ,innerxml captures the
	// bytes and nothing else, so a <v:inner> whose xmlns:v was declared on
	// an ancestor came back out with the prefix intact and the binding
	// gone. A peer re-reading the value then resolved v against whatever
	// happened to be in scope — usually nothing, making the prefix an
	// unbound one — which is precisely the fidelity KindUnknown exists to
	// provide (docs/protocol-support.md: "unknown/vendor xsi:type
	// preserved verbatim for round-trip").
	Namespaces map[string]string
	// InnerXML is the exact captured child content (text and/or nested
	// elements), re-emitted unmodified on MarshalXML.
	InnerXML []byte
}

// TypeError reports that a Value or Array accessor was called for a type
// it does not actually hold — never a panic.
type TypeError struct {
	// Receiver is which type's accessor failed: "Value" or "Array". Left
	// empty, it defaults to "Value" (the common case).
	Receiver string
	// Op names the accessor that failed, e.g. "Int32" or "Strings".
	Op string
	// Kind is the Value's actual Kind (always KindArray for an
	// Array-sourced error).
	Kind Kind
	// Actual is the Value's (or Array's element) actual ScalarType (zero
	// if Kind is KindUnknown).
	Actual ScalarType
	// TypeName is the actual xsi:type, always populated.
	TypeName QName
	// Nil reports whether the Value was present but declared xsi:nil.
	// Never set for an Array-sourced error.
	Nil bool
}

// Error implements the error interface.
func (e *TypeError) Error() string {
	receiver := e.Receiver
	if receiver == "" {
		receiver = "Value"
	}
	if e.Nil {
		return fmt.Sprintf("xmlda: %s.%s: value is xsi:nil (declared type %s)", receiver, e.Op, e.TypeName)
	}
	if receiver == "Array" {
		return fmt.Sprintf("xmlda: Array.%s: array has element type %s, not the requested type", e.Op, e.Actual)
	}
	switch e.Kind {
	case KindUnknown:
		return fmt.Sprintf("xmlda: Value.%s: value has unrecognized type %s", e.Op, e.TypeName)
	case KindQuality:
		return fmt.Sprintf("xmlda: Value.%s: value is a quality (%s), not the requested type", e.Op, e.TypeName)
	case KindArray:
		return fmt.Sprintf("xmlda: Value.%s: value is an array of %s, not the requested scalar type", e.Op, e.Actual)
	default:
		return fmt.Sprintf("xmlda: Value.%s: value has scalar type %s, not the requested type", e.Op, e.Actual)
	}
}

// Value is the generic container for every OPC XML-DA <Value> element
// (ItemValue.Value, ItemProperty.Value, ...).
//
// The zero Value is not a usable value and is never produced by the
// constructors in this file or by decoding. Because KindScalar is Kind's
// own zero, it reports Kind() == KindScalar with an empty Type(), which
// IsUnknown() does not flag — so a zero Value cannot be detected by
// inspecting it, only by knowing it was never constructed. Marshaling one
// fails ("cannot marshal a Value with no declared type") rather than
// emitting a typeless element; that error is a signal that some code path
// passed along a Value it never built. Use NewNil to represent an
// explicit xsi:nil value of a known declared type, which is the correct
// way to express "no value" on the wire.
type Value struct {
	kind     Kind
	typ      ScalarType
	typeName QName
	isNil    bool
	scalar   any
	array    Array
	raw      RawValue
}

// Kind reports which shape v holds.
func (v Value) Kind() Kind { return v.kind }

// Type returns v's ScalarType (the element type, if v is an array). It is
// the zero ScalarType if v.Kind() is KindUnknown.
func (v Value) Type() ScalarType { return v.typ }

// TypeName returns v's exact xsi:type, resolved to a QName. It is always
// populated, for every Kind.
func (v Value) TypeName() QName { return v.typeName }

// IsNil reports whether v is present on the wire but declared xsi:nil
// (REQ-READ-005: e.g. a write-only item read back).
func (v Value) IsNil() bool { return v.isNil }

// IsUnknown reports whether v.Kind() is KindUnknown.
func (v Value) IsUnknown() bool { return v.kind == KindUnknown }

// IsValid reports whether v was actually constructed — that is, whether
// it has a declared type and can therefore be marshaled. It is false for
// exactly one thing: the zero Value, which no constructor and no decode
// ever produces (see Value's doc comment).
//
// Callers accepting a Value across the backend boundary should check this
// before putting one on the wire. A zero Value reaching MarshalXML fails
// the whole encode, which the server can only report as a
// whole-operation E_FAIL — turning one backend slip (a Property whose
// ResultID says "no value available", with Value left at its zero) into a
// discarded response for every other item in the same request.
func (v Value) IsValid() bool { return !v.typeName.IsZero() }

// Equal reports whether v and other represent the same value: same Kind,
// same declared type, same nil-ness, and equal content (using
// time.Time.Equal for time-typed scalars/arrays rather than ==, since a
// caller-constructed time.Time may carry a monotonic reading that would
// otherwise make two logically-equal times compare unequal). This is the
// only supported way to compare two Values — their fields are
// unexported specifically so callers cannot depend on their internal
// representation, only on typed accessors and this method.
func (v Value) Equal(other Value) bool {
	if v.kind != other.kind || v.typ != other.typ || v.isNil != other.isNil {
		return false
	}
	switch v.kind {
	case KindArray:
		return v.array.equal(other.array)
	case KindUnknown:
		return v.typeName == other.typeName && bytes.Equal(v.raw.InnerXML, other.raw.InnerXML)
	case KindQuality:
		// Compared through the accessors, not field by field: OPCQuality
		// holds its enum fields as pointers so an absent attribute is
		// distinguishable from a present one, and == on those compares
		// addresses. The accessors resolve absent to the schema default,
		// which is what makes an explicit QualityField="good" and an
		// omitted one compare equal — as they must, since they mean the
		// same quality.
		a, aok := v.scalar.(OPCQuality)
		b, bok := other.scalar.(OPCQuality)
		if !aok || !bok {
			return false
		}
		return a.QualityField() == b.QualityField() &&
			a.LimitField() == b.LimitField() &&
			a.VendorField() == b.VendorField()
	default: // KindScalar
		return scalarEqual(v.typ, v.scalar, other.scalar)
	}
}

func scalarEqual(t ScalarType, a, b any) bool {
	switch t {
	case TypeBase64Binary:
		ab, aok := a.([]byte)
		bb, bok := b.([]byte)
		return aok == bok && bytes.Equal(ab, bb)
	case TypeDateTime, TypeTime, TypeDate:
		at, aok := a.(time.Time)
		bt, bok := b.(time.Time)
		return aok == bok && at.Equal(bt)
	case TypeFloat, TypeDouble:
		// IEEE-754 says NaN != NaN, but the question this function
		// answers is "is this the same value as before", and two
		// consecutive NaN readings from a failed analog input are the
		// same reading. Comparing them with == reported a change on
		// every single poll: every long-poll returned immediately, the
		// buffer filled, DataBufferOverflow was raised, and one broken
		// sensor turned a subscription into a busy loop. The deadband
		// path already treats consecutive NaNs as unchanged
		// (subscription/poll.go's sampleChanged); this is the same rule
		// for the far more common no-deadband path.
		af, aok := numericAsFloat(a)
		bf, bok := numericAsFloat(b)
		if aok && bok && math.IsNaN(af) && math.IsNaN(bf) {
			return true
		}
		return a == b
	default:
		return a == b
	}
}

// numericAsFloat reports a's float value for the two floating-point
// scalar representations, so scalarEqual can ask about NaN without
// duplicating the type switch.
func numericAsFloat(a any) (float64, bool) {
	switch v := a.(type) {
	case float32:
		return float64(v), true
	case float64:
		return v, true
	default:
		return 0, false
	}
}

// equal compares two Arrays. Element-wise comparison uses scalarEqual for
// the time/bytes special cases; anyType arrays recurse through Value.Equal.
func (a Array) equal(other Array) bool {
	if a.elemType != other.elemType || a.Len() != other.Len() {
		return false
	}
	if a.elemType == TypeAnyType {
		av, _ := a.Any()
		bv, _ := other.Any()
		for i := range av {
			if !av[i].Equal(bv[i]) {
				return false
			}
		}
		return true
	}
	if a.elemType == TypeDateTime {
		at, _ := a.DateTimes()
		bt, _ := other.DateTimes()
		for i := range at {
			if !at[i].Equal(bt[i]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a.data, other.data)
}

// NewNil returns a Value representing an explicit xsi:nil="true" element
// of the given wire type — used when a server must convey "no value"
// while still declaring the item's data type. Kind() reflects what
// typeName actually denotes (array, scalar, or unrecognized), matching
// the xsi:nil decode path (decodeNilKind) — a nil ArrayOf<X>-typed value
// must still report KindArray, not be silently flattened to KindScalar.
func NewNil(typeName QName) Value {
	kind, typ := decodeNilKind(typeName)
	return Value{kind: kind, typ: typ, typeName: typeName, isNil: true}
}

// decodeNilKind resolves typeName to the Kind/ScalarType an xsi:nil value
// declaring it should report, shared by NewNil and the xsi:nil decode
// branch in unmarshalXML.
func decodeNilKind(typeName QName) (Kind, ScalarType) {
	if et, ok := arrayElemTypesByQName[typeName]; ok {
		return KindArray, et
	}
	if st, ok := scalarTypesByQName[typeName]; ok {
		return KindScalar, st
	}
	if typeName == qualityTypeName {
		return KindQuality, ""
	}
	return KindUnknown, ""
}

// qualityTypeName is the xsi:type an OPCQuality-typed Value declares.
var qualityTypeName = QName{Space: Namespace, Local: "OPCQuality"}

// NewQualityValue wraps q as a Value.
//
// OPCQuality is the one complex type the specification puts in a <Value>
// position: standard item property 3, "Item Quality", is declared with
// the data type OPCQuality (§3.1.10 p.40), so a backend serving the
// standard property set needs a way to express one. Every other <Value>
// this protocol carries is an XSD simple type or an ArrayOf<X> of them.
//
//	props = append(props, backend.Property{
//		ID:    xmlda.PropQuality,
//		Value: xmlda.NewQualityValue(sample.Quality),
//	})
//
// Note that this is a property VALUE, unrelated to ItemValue.Quality —
// that field is an OPCQuality directly, not a Value wrapping one.
func NewQualityValue(q OPCQuality) Value {
	return Value{kind: KindQuality, typeName: qualityTypeName, scalar: q}
}

// Quality returns v's OPCQuality, or a *TypeError if v does not hold one.
func (v Value) Quality() (OPCQuality, error) {
	if v.isNil || v.kind != KindQuality {
		return OPCQuality{}, &TypeError{
			Op: "Quality", Kind: v.kind, Actual: v.typ, TypeName: v.typeName, Nil: v.isNil,
		}
	}
	q, ok := v.scalar.(OPCQuality)
	if !ok {
		return OPCQuality{}, &TypeError{
			Op: "Quality", Kind: v.kind, Actual: v.typ, TypeName: v.typeName,
		}
	}
	return q, nil
}

// NewArrayValue wraps a as a Value, the way the NewX constructors below
// wrap a scalar. It is how a backend returns an array-typed item: the
// NewXArray constructors (NewFloat64Array, NewStringArray, ...) build an
// Array, but backend.ItemSample.Value — and every other place a value
// crosses the public API — takes a Value, and Value's fields are
// unexported by design.
//
// The ArrayOf<X> xsi:type is already carried by a (set by whichever
// NewXArray built it), so nothing needs to be named twice:
//
//	sample.Value = xmlda.NewArrayValue(xmlda.NewFloat64Array([]float64{1.5, 2.5}))
//	// → <Value xsi:type="opc:ArrayOfDouble"><double>1.5</double>…</Value>
//
// The zero Array (never built by a constructor) has no declared type and
// yields a Value that fails to marshal, exactly as the zero Value does —
// see Value's own doc comment.
//
// There is deliberately no NewUint8Array: a byte array is base64Binary on
// the wire, not an ArrayOf<X> (the schema defines no ArrayOfUnsignedByte),
// so use NewBytes for one.
func NewArrayValue(a Array) Value {
	return Value{kind: KindArray, typ: a.elemType, typeName: a.typeName, array: a}
}

// NewString returns a scalar Value of XSD type string.
func NewString(s string) Value {
	return Value{kind: KindScalar, typ: TypeString, typeName: QName{XSDNamespace, "string"}, scalar: s}
}

// NewBool returns a scalar Value of XSD type boolean.
func NewBool(b bool) Value {
	return Value{kind: KindScalar, typ: TypeBoolean, typeName: QName{XSDNamespace, "boolean"}, scalar: b}
}

// NewFloat32 returns a scalar Value of XSD type float.
func NewFloat32(f float32) Value {
	return Value{kind: KindScalar, typ: TypeFloat, typeName: QName{XSDNamespace, "float"}, scalar: f}
}

// NewFloat64 returns a scalar Value of XSD type double.
func NewFloat64(f float64) Value {
	return Value{kind: KindScalar, typ: TypeDouble, typeName: QName{XSDNamespace, "double"}, scalar: f}
}

// NewDecimalValue returns a scalar Value of XSD type decimal.
func NewDecimalValue(d Decimal) Value {
	return Value{kind: KindScalar, typ: TypeDecimal, typeName: QName{XSDNamespace, "decimal"}, scalar: d}
}

// NewInt64 returns a scalar Value of XSD type long.
func NewInt64(i int64) Value {
	return Value{kind: KindScalar, typ: TypeLong, typeName: QName{XSDNamespace, "long"}, scalar: i}
}

// NewInt32 returns a scalar Value of XSD type int.
func NewInt32(i int32) Value {
	return Value{kind: KindScalar, typ: TypeInt, typeName: QName{XSDNamespace, "int"}, scalar: i}
}

// NewInt16 returns a scalar Value of XSD type short.
func NewInt16(i int16) Value {
	return Value{kind: KindScalar, typ: TypeShort, typeName: QName{XSDNamespace, "short"}, scalar: i}
}

// NewInt8 returns a scalar Value of XSD type byte (signed).
func NewInt8(i int8) Value {
	return Value{kind: KindScalar, typ: TypeByte, typeName: QName{XSDNamespace, "byte"}, scalar: i}
}

// NewUint64 returns a scalar Value of XSD type unsignedLong.
func NewUint64(u uint64) Value {
	return Value{kind: KindScalar, typ: TypeUnsignedLong, typeName: QName{XSDNamespace, "unsignedLong"}, scalar: u}
}

// NewUint32 returns a scalar Value of XSD type unsignedInt.
func NewUint32(u uint32) Value {
	return Value{kind: KindScalar, typ: TypeUnsignedInt, typeName: QName{XSDNamespace, "unsignedInt"}, scalar: u}
}

// NewUint16 returns a scalar Value of XSD type unsignedShort.
func NewUint16(u uint16) Value {
	return Value{kind: KindScalar, typ: TypeUnsignedShort, typeName: QName{XSDNamespace, "unsignedShort"}, scalar: u}
}

// NewUint8 returns a scalar Value of XSD type unsignedByte.
func NewUint8(u uint8) Value {
	return Value{kind: KindScalar, typ: TypeUnsignedByte, typeName: QName{XSDNamespace, "unsignedByte"}, scalar: u}
}

// NewBytes returns a scalar Value of XSD type base64Binary. b is copied.
// A nil b and a non-nil empty b are preserved as distinct (see
// docs/specification/type-mapping.md): append(nil-dst, empty-src...)
// always yields nil in Go, so copying b's nil-ness must be done
// explicitly rather than via a single unconditional append.
func NewBytes(b []byte) Value {
	var cp []byte
	if b != nil {
		cp = append([]byte{}, b...)
	}
	return Value{kind: KindScalar, typ: TypeBase64Binary, typeName: QName{XSDNamespace, "base64Binary"}, scalar: cp}
}

// NewDateTime returns a scalar Value of XSD type dateTime.
func NewDateTime(t time.Time) Value {
	return Value{kind: KindScalar, typ: TypeDateTime, typeName: QName{XSDNamespace, "dateTime"}, scalar: t}
}

// NewTime returns a scalar Value of XSD type time. See
// docs/specification/open-questions.md OQ-12 for how this differs from the
// dateTime+ValueTypeQualifier encoding some peers use.
func NewTime(t time.Time) Value {
	return Value{kind: KindScalar, typ: TypeTime, typeName: QName{XSDNamespace, "time"}, scalar: t}
}

// NewDate returns a scalar Value of XSD type date. See OQ-12.
func NewDate(t time.Time) Value {
	return Value{kind: KindScalar, typ: TypeDate, typeName: QName{XSDNamespace, "date"}, scalar: t}
}

// NewDuration returns a scalar Value of XSD type duration, wrapping the
// given ISO-8601 duration literal (e.g. "P1D").
//
// The literal is stored as given; it is validated on the way to the wire
// (see formatScalar), so a malformed one fails the encode with a clear
// error rather than being shipped as an xsd:duration a peer cannot
// parse. Callers wanting the check up front can use ValidDuration.
func NewDuration(iso8601 string) Value {
	return Value{kind: KindScalar, typ: TypeDuration, typeName: QName{XSDNamespace, "duration"}, scalar: iso8601}
}

// NewQNameValue returns a scalar Value of XSD type QName, wrapping q as
// the value's own payload (distinct from v.TypeName, which remains
// {XSDNamespace, "QName"}).
func NewQNameValue(q QName) Value {
	return Value{kind: KindScalar, typ: TypeQName, typeName: QName{XSDNamespace, "QName"}, scalar: q}
}

// checkScalar returns a *TypeError if v is not a present scalar of type
// want, or nil if the access is valid.
func (v Value) checkScalar(op string, want ScalarType) error {
	if v.isNil {
		return &TypeError{Op: op, Kind: v.kind, Actual: v.typ, TypeName: v.typeName, Nil: true}
	}
	if v.kind != KindScalar || v.typ != want {
		return &TypeError{Op: op, Kind: v.kind, Actual: v.typ, TypeName: v.typeName}
	}
	return nil
}

// String returns v's value as a string, or a *TypeError if v is not a
// scalar string.
func (v Value) String() (string, error) {
	if err := v.checkScalar("String", TypeString); err != nil {
		return "", err
	}
	return v.scalar.(string), nil
}

// Bool returns v's value as a bool, or a *TypeError if v is not a scalar
// boolean.
func (v Value) Bool() (bool, error) {
	if err := v.checkScalar("Bool", TypeBoolean); err != nil {
		return false, err
	}
	return v.scalar.(bool), nil
}

// Float32 returns v's value as a float32, or a *TypeError if v is not a
// scalar float.
func (v Value) Float32() (float32, error) {
	if err := v.checkScalar("Float32", TypeFloat); err != nil {
		return 0, err
	}
	return v.scalar.(float32), nil
}

// Float64 returns v's value as a float64, or a *TypeError if v is not a
// scalar double.
func (v Value) Float64() (float64, error) {
	if err := v.checkScalar("Float64", TypeDouble); err != nil {
		return 0, err
	}
	return v.scalar.(float64), nil
}

// Decimal returns v's value as a Decimal, or a *TypeError if v is not a
// scalar decimal.
func (v Value) Decimal() (Decimal, error) {
	if err := v.checkScalar("Decimal", TypeDecimal); err != nil {
		return "", err
	}
	return v.scalar.(Decimal), nil
}

// Int64 returns v's value as an int64, or a *TypeError if v is not a
// scalar long.
func (v Value) Int64() (int64, error) {
	if err := v.checkScalar("Int64", TypeLong); err != nil {
		return 0, err
	}
	return v.scalar.(int64), nil
}

// Int32 returns v's value as an int32, or a *TypeError if v is not a
// scalar int.
func (v Value) Int32() (int32, error) {
	if err := v.checkScalar("Int32", TypeInt); err != nil {
		return 0, err
	}
	return v.scalar.(int32), nil
}

// Int16 returns v's value as an int16, or a *TypeError if v is not a
// scalar short.
func (v Value) Int16() (int16, error) {
	if err := v.checkScalar("Int16", TypeShort); err != nil {
		return 0, err
	}
	return v.scalar.(int16), nil
}

// Int8 returns v's value as an int8, or a *TypeError if v is not a scalar
// (signed) byte.
func (v Value) Int8() (int8, error) {
	if err := v.checkScalar("Int8", TypeByte); err != nil {
		return 0, err
	}
	return v.scalar.(int8), nil
}

// Uint64 returns v's value as a uint64, or a *TypeError if v is not a
// scalar unsignedLong.
func (v Value) Uint64() (uint64, error) {
	if err := v.checkScalar("Uint64", TypeUnsignedLong); err != nil {
		return 0, err
	}
	return v.scalar.(uint64), nil
}

// Uint32 returns v's value as a uint32, or a *TypeError if v is not a
// scalar unsignedInt.
func (v Value) Uint32() (uint32, error) {
	if err := v.checkScalar("Uint32", TypeUnsignedInt); err != nil {
		return 0, err
	}
	return v.scalar.(uint32), nil
}

// Uint16 returns v's value as a uint16, or a *TypeError if v is not a
// scalar unsignedShort.
func (v Value) Uint16() (uint16, error) {
	if err := v.checkScalar("Uint16", TypeUnsignedShort); err != nil {
		return 0, err
	}
	return v.scalar.(uint16), nil
}

// Uint8 returns v's value as a uint8, or a *TypeError if v is not a
// scalar unsignedByte.
func (v Value) Uint8() (uint8, error) {
	if err := v.checkScalar("Uint8", TypeUnsignedByte); err != nil {
		return 0, err
	}
	return v.scalar.(uint8), nil
}

// NumericAsFloat64 returns v's value converted to float64 if v is a
// present scalar of any numeric ScalarType (the signed/unsigned integer
// widths, float, double, or decimal), or ok=false otherwise (v is an
// array, an unknown-type value, xsi:nil, or a non-numeric scalar type
// such as string/boolean/dateTime). Unlike the individual typed
// accessors, this deliberately loses which exact numeric type v held —
// callers that need that distinction, or need exact-integer precision
// beyond float64's 2^53-exact range (int64/uint64), must use Type() plus
// the specific typed accessor instead (see server/coerce.go's
// integer-to-integer coercion path, which avoids this method for exactly
// that reason). Intended for callers that only need an approximate
// numeric comparison, e.g. subscription deadband evaluation.
func (v Value) NumericAsFloat64() (float64, bool) {
	if v.isNil || v.kind != KindScalar {
		return 0, false
	}
	switch v.typ {
	case TypeByte:
		return float64(v.scalar.(int8)), true
	case TypeUnsignedByte:
		return float64(v.scalar.(uint8)), true
	case TypeShort:
		return float64(v.scalar.(int16)), true
	case TypeUnsignedShort:
		return float64(v.scalar.(uint16)), true
	case TypeInt:
		return float64(v.scalar.(int32)), true
	case TypeUnsignedInt:
		return float64(v.scalar.(uint32)), true
	case TypeLong:
		return float64(v.scalar.(int64)), true
	case TypeUnsignedLong:
		return float64(v.scalar.(uint64)), true
	case TypeFloat:
		return float64(v.scalar.(float32)), true
	case TypeDouble:
		return v.scalar.(float64), true
	case TypeDecimal:
		f, err := v.scalar.(Decimal).Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// Bytes returns v's value as a []byte, or a *TypeError if v is not a
// scalar base64Binary. The returned slice is a copy.
func (v Value) Bytes() ([]byte, error) {
	if err := v.checkScalar("Bytes", TypeBase64Binary); err != nil {
		return nil, err
	}
	b := v.scalar.([]byte)
	if b == nil {
		return nil, nil
	}
	return append([]byte{}, b...), nil
}

// Time returns v's value as a time.Time, or a *TypeError if v is not a
// scalar dateTime, time, or date.
func (v Value) Time() (time.Time, error) {
	if v.isNil {
		return time.Time{}, &TypeError{Op: "Time", Kind: v.kind, Actual: v.typ, TypeName: v.typeName, Nil: true}
	}
	if v.kind != KindScalar || (v.typ != TypeDateTime && v.typ != TypeTime && v.typ != TypeDate) {
		return time.Time{}, &TypeError{Op: "Time", Kind: v.kind, Actual: v.typ, TypeName: v.typeName}
	}
	return v.scalar.(time.Time), nil
}

// Duration returns v's value as an ISO-8601 duration literal, or a
// *TypeError if v is not a scalar duration.
func (v Value) Duration() (string, error) {
	if err := v.checkScalar("Duration", TypeDuration); err != nil {
		return "", err
	}
	return v.scalar.(string), nil
}

// QNameValue returns v's value as a QName, or a *TypeError if v is not a
// scalar QName.
func (v Value) QNameValue() (QName, error) {
	if err := v.checkScalar("QNameValue", TypeQName); err != nil {
		return QName{}, err
	}
	return v.scalar.(QName), nil
}

// Array returns v as an Array, or a *TypeError if v is not an array.
func (v Value) Array() (Array, error) {
	if v.isNil {
		return Array{}, &TypeError{Op: "Array", Kind: v.kind, Actual: v.typ, TypeName: v.typeName, Nil: true}
	}
	if v.kind != KindArray {
		return Array{}, &TypeError{Op: "Array", Kind: v.kind, Actual: v.typ, TypeName: v.typeName}
	}
	return v.array, nil
}

// Raw returns v's captured raw content, or a *TypeError if v.Kind() is
// not KindUnknown.
func (v Value) Raw() (RawValue, error) {
	if v.kind != KindUnknown {
		return RawValue{}, &TypeError{Op: "Raw", Kind: v.kind, Actual: v.typ, TypeName: v.typeName}
	}
	return v.raw, nil
}

// Array is one ArrayOf<X> value: a homogeneous sequence of elements of one
// ScalarType (or, for ArrayOfAnyType, a heterogeneous sequence of
// independently-typed Values). Reachable only through the typed
// constructors/accessors below, so misuse produces a *TypeError instead of
// a panic.
type Array struct {
	elemType ScalarType
	typeName QName
	data     any
}

// The typed accessors below (Int32s, Float64s, Strings, Any, ...) return a
// COPY of the array's storage. That matches the constructors, which copy
// their input, and Value.Bytes, which already did. Handing out the
// internal slice made a Value mutable through an accessor — and in the
// subscription engine one backing array is shared by an item's last
// reported sample, every buffered update referring to it and the encoder
// writing the response, none of which the item lock can protect once a
// slice has escaped.

// ElemType returns the array's element ScalarType.
func (a Array) ElemType() ScalarType { return a.elemType }

// TypeName returns the array's declared ArrayOf<X> xsi:type, resolved to
// a QName.
func (a Array) TypeName() QName { return a.typeName }

// Len returns the number of elements in the array.
func (a Array) Len() int {
	switch d := a.data.(type) {
	case []int8:
		return len(d)
	case []int16:
		return len(d)
	case []uint16:
		return len(d)
	case []int32:
		return len(d)
	case []uint32:
		return len(d)
	case []int64:
		return len(d)
	case []uint64:
		return len(d)
	case []float32:
		return len(d)
	case []Decimal:
		return len(d)
	case []float64:
		return len(d)
	case []bool:
		return len(d)
	case []string:
		return len(d)
	case []time.Time:
		return len(d)
	case []Value:
		return len(d)
	default:
		return 0
	}
}

func (a Array) checkElem(op string, want ScalarType) error {
	if a.elemType != want {
		return &TypeError{Receiver: "Array", Op: op, Kind: KindArray, Actual: a.elemType, TypeName: a.typeName}
	}
	return nil
}

// NewInt8Array returns an ArrayOfByte value (signed byte elements).
func NewInt8Array(v []int8) Array {
	return Array{elemType: TypeByte, typeName: QName{Namespace, "ArrayOfByte"}, data: append([]int8(nil), v...)}
}

// NewInt16Array returns an ArrayOfShort value.
func NewInt16Array(v []int16) Array {
	return Array{elemType: TypeShort, typeName: QName{Namespace, "ArrayOfShort"}, data: append([]int16(nil), v...)}
}

// NewUint16Array returns an ArrayOfUnsignedShort value.
func NewUint16Array(v []uint16) Array {
	return Array{elemType: TypeUnsignedShort, typeName: QName{Namespace, "ArrayOfUnsignedShort"}, data: append([]uint16(nil), v...)}
}

// NewInt32Array returns an ArrayOfInt value.
func NewInt32Array(v []int32) Array {
	return Array{elemType: TypeInt, typeName: QName{Namespace, "ArrayOfInt"}, data: append([]int32(nil), v...)}
}

// NewUint32Array returns an ArrayOfUnsignedInt value.
func NewUint32Array(v []uint32) Array {
	return Array{elemType: TypeUnsignedInt, typeName: QName{Namespace, "ArrayOfUnsignedInt"}, data: append([]uint32(nil), v...)}
}

// NewInt64Array returns an ArrayOfLong value.
func NewInt64Array(v []int64) Array {
	return Array{elemType: TypeLong, typeName: QName{Namespace, "ArrayOfLong"}, data: append([]int64(nil), v...)}
}

// NewUint64Array returns an ArrayOfUnsignedLong value.
func NewUint64Array(v []uint64) Array {
	return Array{elemType: TypeUnsignedLong, typeName: QName{Namespace, "ArrayOfUnsignedLong"}, data: append([]uint64(nil), v...)}
}

// NewFloat32Array returns an ArrayOfFloat value.
func NewFloat32Array(v []float32) Array {
	return Array{elemType: TypeFloat, typeName: QName{Namespace, "ArrayOfFloat"}, data: append([]float32(nil), v...)}
}

// NewDecimalArray returns an ArrayOfDecimal value.
func NewDecimalArray(v []Decimal) Array {
	return Array{elemType: TypeDecimal, typeName: QName{Namespace, "ArrayOfDecimal"}, data: append([]Decimal(nil), v...)}
}

// NewFloat64Array returns an ArrayOfDouble value.
func NewFloat64Array(v []float64) Array {
	return Array{elemType: TypeDouble, typeName: QName{Namespace, "ArrayOfDouble"}, data: append([]float64(nil), v...)}
}

// NewBoolArray returns an ArrayOfBoolean value.
func NewBoolArray(v []bool) Array {
	return Array{elemType: TypeBoolean, typeName: QName{Namespace, "ArrayOfBoolean"}, data: append([]bool(nil), v...)}
}

// NewStringArray returns an ArrayOfString value.
func NewStringArray(v []string) Array {
	return Array{elemType: TypeString, typeName: QName{Namespace, "ArrayOfString"}, data: append([]string(nil), v...)}
}

// NewDateTimeArray returns an ArrayOfDateTime value.
func NewDateTimeArray(v []time.Time) Array {
	return Array{elemType: TypeDateTime, typeName: QName{Namespace, "ArrayOfDateTime"}, data: append([]time.Time(nil), v...)}
}

// NewAnyArray returns an ArrayOfAnyType value: a heterogeneous sequence
// where each element is independently typed (and may itself be an array).
// Note: there is deliberately no NewUint8Array/ArrayOfUnsignedByte —
// the specification defines no such array type; unsigned-byte sequences
// are always transmitted as scalar base64Binary (see NewBytes).
func NewAnyArray(v []Value) Array {
	return Array{elemType: TypeAnyType, typeName: QName{Namespace, "ArrayOfAnyType"}, data: append([]Value(nil), v...)}
}

// Int8s returns the array's elements as []int8, or a *TypeError if the
// array's element type is not byte.
func (a Array) Int8s() ([]int8, error) {
	if err := a.checkElem("Int8s", TypeByte); err != nil {
		return nil, err
	}
	return slices.Clone(a.data.([]int8)), nil
}

// Int16s returns the array's elements as []int16, or a *TypeError if the
// array's element type is not short.
func (a Array) Int16s() ([]int16, error) {
	if err := a.checkElem("Int16s", TypeShort); err != nil {
		return nil, err
	}
	return slices.Clone(a.data.([]int16)), nil
}

// Uint16s returns the array's elements as []uint16, or a *TypeError if the
// array's element type is not unsignedShort.
func (a Array) Uint16s() ([]uint16, error) {
	if err := a.checkElem("Uint16s", TypeUnsignedShort); err != nil {
		return nil, err
	}
	return slices.Clone(a.data.([]uint16)), nil
}

// Int32s returns the array's elements as []int32, or a *TypeError if the
// array's element type is not int.
func (a Array) Int32s() ([]int32, error) {
	if err := a.checkElem("Int32s", TypeInt); err != nil {
		return nil, err
	}
	return slices.Clone(a.data.([]int32)), nil
}

// Uint32s returns the array's elements as []uint32, or a *TypeError if the
// array's element type is not unsignedInt.
func (a Array) Uint32s() ([]uint32, error) {
	if err := a.checkElem("Uint32s", TypeUnsignedInt); err != nil {
		return nil, err
	}
	return slices.Clone(a.data.([]uint32)), nil
}

// Int64s returns the array's elements as []int64, or a *TypeError if the
// array's element type is not long.
func (a Array) Int64s() ([]int64, error) {
	if err := a.checkElem("Int64s", TypeLong); err != nil {
		return nil, err
	}
	return slices.Clone(a.data.([]int64)), nil
}

// Uint64s returns the array's elements as []uint64, or a *TypeError if the
// array's element type is not unsignedLong.
func (a Array) Uint64s() ([]uint64, error) {
	if err := a.checkElem("Uint64s", TypeUnsignedLong); err != nil {
		return nil, err
	}
	return slices.Clone(a.data.([]uint64)), nil
}

// Float32s returns the array's elements as []float32, or a *TypeError if
// the array's element type is not float.
func (a Array) Float32s() ([]float32, error) {
	if err := a.checkElem("Float32s", TypeFloat); err != nil {
		return nil, err
	}
	return slices.Clone(a.data.([]float32)), nil
}

// Decimals returns the array's elements as []Decimal, or a *TypeError if
// the array's element type is not decimal.
func (a Array) Decimals() ([]Decimal, error) {
	if err := a.checkElem("Decimals", TypeDecimal); err != nil {
		return nil, err
	}
	return slices.Clone(a.data.([]Decimal)), nil
}

// Float64s returns the array's elements as []float64, or a *TypeError if
// the array's element type is not double.
func (a Array) Float64s() ([]float64, error) {
	if err := a.checkElem("Float64s", TypeDouble); err != nil {
		return nil, err
	}
	return slices.Clone(a.data.([]float64)), nil
}

// Bools returns the array's elements as []bool, or a *TypeError if the
// array's element type is not boolean.
func (a Array) Bools() ([]bool, error) {
	if err := a.checkElem("Bools", TypeBoolean); err != nil {
		return nil, err
	}
	return slices.Clone(a.data.([]bool)), nil
}

// Strings returns the array's elements as []string, or a *TypeError if
// the array's element type is not string.
func (a Array) Strings() ([]string, error) {
	if err := a.checkElem("Strings", TypeString); err != nil {
		return nil, err
	}
	return slices.Clone(a.data.([]string)), nil
}

// DateTimes returns the array's elements as []time.Time, or a *TypeError
// if the array's element type is not dateTime.
func (a Array) DateTimes() ([]time.Time, error) {
	if err := a.checkElem("DateTimes", TypeDateTime); err != nil {
		return nil, err
	}
	return slices.Clone(a.data.([]time.Time)), nil
}

// Any returns the array's elements as []Value, or a *TypeError if the
// array's element type is not anyType.
func (a Array) Any() ([]Value, error) {
	if err := a.checkElem("Any", TypeAnyType); err != nil {
		return nil, err
	}
	return slices.Clone(a.data.([]Value)), nil
}

// NumericFloat64s renders a numeric array's elements as float64, or
// reports false for an array whose element type has no numeric reading
// (string, boolean, dateTime, anyType, ...).
//
// It is the array counterpart of Value.NumericAsFloat64, and exists for
// the same caller: the subscription engine's deadband comparison, which
// §3.5.1 requires to apply element-wise to array types ("The entire array
// is returned if any array element exceeds the deadband threshold").
// The conversion is lossy for the widest integer types in the same way
// NumericAsFloat64 is, which is inherent to comparing against a
// percentage.
func (a Array) NumericFloat64s() ([]float64, bool) {
	out := make([]float64, a.Len())
	for i := range out {
		e, err := elementAt(a, i)
		if err != nil {
			return nil, false
		}
		f, ok := numericAnyAsFloat64(e)
		if !ok {
			return nil, false
		}
		out[i] = f
	}
	return out, true
}

// numericAnyAsFloat64 converts one stored numeric element to float64.
func numericAnyAsFloat64(e any) (float64, bool) {
	switch v := e.(type) {
	case int8:
		return float64(v), true
	case int16:
		return float64(v), true
	case uint16:
		return float64(v), true
	case int32:
		return float64(v), true
	case uint32:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint64:
		return float64(v), true
	case float32:
		return float64(v), true
	case float64:
		return v, true
	default:
		return 0, false
	}
}

// elementAt returns element i of a's underlying storage as an any, for use
// by the generic array marshaler.
func elementAt(a Array, i int) (any, error) {
	switch d := a.data.(type) {
	case []int8:
		return d[i], nil
	case []int16:
		return d[i], nil
	case []uint16:
		return d[i], nil
	case []int32:
		return d[i], nil
	case []uint32:
		return d[i], nil
	case []int64:
		return d[i], nil
	case []uint64:
		return d[i], nil
	case []float32:
		return d[i], nil
	case []Decimal:
		return d[i], nil
	case []float64:
		return d[i], nil
	case []bool:
		return d[i], nil
	case []string:
		return d[i], nil
	case []time.Time:
		return d[i], nil
	default:
		return nil, fmt.Errorf("xmlda: array: unsupported element storage type %T", a.data)
	}
}

// --- scalar parsing/formatting ---

// dateTimeLayouts are the accepted lexical forms of an xsd:dateTime.
//
// The list is wider than RFC 3339 on purpose. xsd:dateTime's timezone
// offset is OPTIONAL (XSD Part 2 §3.2.7: a value with no offset denotes
// an unspecified/local zone), and a date alone is accepted as an interop
// tolerance for peers that shorten a midnight value. Go's own
// time.Time.UnmarshalText — which encoding/xml calls for any time.Time
// field — accepts only RFC 3339 and so rejects a conforming offsetless
// value outright; that is why no dateTime anywhere in this package is
// decoded through a plain time.Time field. See wireTime (replybase.go).
var dateTimeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02Z07:00",
	"2006-01-02",
}

// endOfDayPattern matches xsd:dateTime's end-of-day time component, which
// the grammar admits only with zero minutes and seconds.
var endOfDayPattern = regexp.MustCompile(`T24:00:00(\.0+)?`)

// normalizeEndOfDay rewrites the xsd:dateTime end-of-day form
// "<date>T24:00:00" into the equivalent "<date+1>T00:00:00", reporting
// whether it did. XSD Part 2 defines 24:00:00 as a synonym for midnight
// of the following day; Go's time.Parse rejects hour 24 outright, so a
// conforming peer using that form would otherwise fault the request.
//
// The rewrite only applies when the matched text is the whole time
// component — what follows must be either nothing or a timezone
// designator — so a value that merely contains those digits elsewhere is
// left alone.
func normalizeEndOfDay(s string) (string, bool) {
	loc := endOfDayPattern.FindStringIndex(s)
	if loc == nil {
		return s, false
	}
	rest := s[loc[1]:]
	if rest != "" && rest != "Z" && !strings.HasPrefix(rest, "+") && !strings.HasPrefix(rest, "-") {
		return s, false
	}
	return s[:loc[0]] + "T00:00:00" + rest, true
}

func parseXSDDateTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	base, endOfDay := normalizeEndOfDay(s)
	var lastErr error
	for _, layout := range dateTimeLayouts {
		if t, err := time.Parse(layout, base); err == nil {
			if endOfDay {
				t = t.AddDate(0, 0, 1)
			}
			return t, nil
		} else {
			lastErr = err
		}
	}
	return time.Time{}, fmt.Errorf("xmlda: %q is not a valid xsd:dateTime value: %w", s, lastErr)
}

// timeLayouts are the accepted lexical forms of a standalone xsd:time
// literal (OQ-12): an optional fractional-second component, and an
// optional timezone (either "Z" or a numeric offset) — tried most-specific
// first since time.Parse requires the whole string to match one layout
// exactly (no partial/leftover match).
var timeLayouts = []string{
	"15:04:05.999999999Z07:00",
	"15:04:05Z07:00",
	"15:04:05.999999999",
	"15:04:05",
}

// parseXSDTime parses a standalone xsd:time literal. Go's time.Parse fills
// in the fields absent from the layout (year 0, month 1, day 1, and UTC
// when no zone is present) — exactly the "drop the component this type
// doesn't carry" behavior xsd:time needs, with no separate zeroing step.
func parseXSDTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	var lastErr error
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		} else {
			lastErr = err
		}
	}
	return time.Time{}, fmt.Errorf("xmlda: %q is not a valid xsd:time value: %w", s, lastErr)
}

// dateLayouts are the accepted lexical forms of a standalone xsd:date
// literal (OQ-12): the calendar date, with an optional (and, per real
// traffic, non-conformant but tolerated) timezone suffix.
var dateLayouts = []string{
	"2006-01-02Z07:00",
	"2006-01-02",
}

// parseXSDDate parses a standalone xsd:date literal, dropping any
// time-of-day component the same way parseXSDTime drops the date.
func parseXSDDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	var lastErr error
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		} else {
			lastErr = err
		}
	}
	return time.Time{}, fmt.Errorf("xmlda: %q is not a valid xsd:date value: %w", s, lastErr)
}

func formatXSDFloat(f float64, bitSize int) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "INF"
	case math.IsInf(f, -1):
		return "-INF"
	default:
		return strconv.FormatFloat(f, 'g', -1, bitSize)
	}
}

func parseBytesLiteral(s string) ([]byte, error) {
	s = strings.Join(strings.Fields(s), "") // tolerate embedded whitespace/linebreaks
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("xmlda: invalid xsd:base64Binary literal: %w", err)
	}
	return b, nil
}

// parseScalar parses text as an xsd literal of the given ScalarType. It
// does not handle TypeQName, which needs decoder-scoped prefix
// resolution and is handled directly in decodeScalar.
func parseScalar(st ScalarType, text string) (any, error) {
	switch st {
	case TypeString:
		return text, nil
	case TypeBoolean:
		b, err := strconv.ParseBool(strings.TrimSpace(text))
		if err != nil {
			return nil, fmt.Errorf("xmlda: invalid xsd:boolean literal %q: %w", text, err)
		}
		return b, nil
	case TypeFloat:
		f, err := strconv.ParseFloat(strings.TrimSpace(text), 32)
		if err != nil {
			return nil, fmt.Errorf("xmlda: invalid xsd:float literal %q: %w", text, err)
		}
		return float32(f), nil
	case TypeDouble:
		f, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil {
			return nil, fmt.Errorf("xmlda: invalid xsd:double literal %q: %w", text, err)
		}
		return f, nil
	case TypeDecimal:
		return NewDecimal(strings.TrimSpace(text))
	case TypeLong:
		i, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("xmlda: invalid xsd:long literal %q: %w", text, err)
		}
		return i, nil
	case TypeInt:
		i, err := strconv.ParseInt(strings.TrimSpace(text), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("xmlda: invalid xsd:int literal %q: %w", text, err)
		}
		return int32(i), nil
	case TypeShort:
		i, err := strconv.ParseInt(strings.TrimSpace(text), 10, 16)
		if err != nil {
			return nil, fmt.Errorf("xmlda: invalid xsd:short literal %q: %w", text, err)
		}
		return int16(i), nil
	case TypeByte:
		i, err := strconv.ParseInt(strings.TrimSpace(text), 10, 8)
		if err != nil {
			return nil, fmt.Errorf("xmlda: invalid xsd:byte literal %q: %w", text, err)
		}
		return int8(i), nil
	case TypeUnsignedLong:
		u, err := strconv.ParseUint(strings.TrimSpace(text), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("xmlda: invalid xsd:unsignedLong literal %q: %w", text, err)
		}
		return u, nil
	case TypeUnsignedInt:
		u, err := strconv.ParseUint(strings.TrimSpace(text), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("xmlda: invalid xsd:unsignedInt literal %q: %w", text, err)
		}
		return uint32(u), nil
	case TypeUnsignedShort:
		u, err := strconv.ParseUint(strings.TrimSpace(text), 10, 16)
		if err != nil {
			return nil, fmt.Errorf("xmlda: invalid xsd:unsignedShort literal %q: %w", text, err)
		}
		return uint16(u), nil
	case TypeUnsignedByte:
		u, err := strconv.ParseUint(strings.TrimSpace(text), 10, 8)
		if err != nil {
			return nil, fmt.Errorf("xmlda: invalid xsd:unsignedByte literal %q: %w", text, err)
		}
		return uint8(u), nil
	case TypeBase64Binary:
		return parseBytesLiteral(text)
	case TypeDateTime:
		return parseXSDDateTime(text)
	case TypeTime:
		return parseXSDTime(text)
	case TypeDate:
		return parseXSDDate(text)
	case TypeDuration:
		d := strings.TrimSpace(text)
		if !ValidDuration(d) {
			return nil, fmt.Errorf("xmlda: %q is not a valid xsd:duration literal", d)
		}
		return d, nil
	default:
		return nil, fmt.Errorf("xmlda: unsupported scalar type %q", st)
	}
}

// formatScalar renders val (as produced by parseScalar or one of the NewX
// constructors) as its xsd literal text. It does not handle TypeQName.
func formatScalar(st ScalarType, val any) (string, error) {
	// Every assertion below is checked. An unchecked one would panic on a
	// Value whose declared ScalarType and stored payload disagree — which
	// no constructor or decode in this package produces, but which a
	// panic is the wrong way to report: it unwinds through the encoder
	// into ServeHTTP's recover and reaches the client as a bare E_FAIL
	// with the actual cause only in a stack trace.
	bad := func(want string) (string, error) {
		return "", fmt.Errorf("xmlda: value declared as %s holds %T, not %s (internal inconsistency)", st, val, want)
	}
	switch st {
	case TypeString:
		v, ok := val.(string)
		if !ok {
			return bad("string")
		}
		return v, nil
	case TypeBoolean:
		v, ok := val.(bool)
		if !ok {
			return bad("bool")
		}
		if v {
			return "true", nil
		}
		return "false", nil
	case TypeFloat:
		v, ok := val.(float32)
		if !ok {
			return bad("float32")
		}
		return formatXSDFloat(float64(v), 32), nil
	case TypeDouble:
		v, ok := val.(float64)
		if !ok {
			return bad("float64")
		}
		return formatXSDFloat(v, 64), nil
	case TypeDecimal:
		v, ok := val.(Decimal)
		if !ok {
			return bad("Decimal")
		}
		return v.String(), nil
	case TypeLong:
		v, ok := val.(int64)
		if !ok {
			return bad("int64")
		}
		return strconv.FormatInt(v, 10), nil
	case TypeInt:
		v, ok := val.(int32)
		if !ok {
			return bad("int32")
		}
		return strconv.FormatInt(int64(v), 10), nil
	case TypeShort:
		v, ok := val.(int16)
		if !ok {
			return bad("int16")
		}
		return strconv.FormatInt(int64(v), 10), nil
	case TypeByte:
		v, ok := val.(int8)
		if !ok {
			return bad("int8")
		}
		return strconv.FormatInt(int64(v), 10), nil
	case TypeUnsignedLong:
		v, ok := val.(uint64)
		if !ok {
			return bad("uint64")
		}
		return strconv.FormatUint(v, 10), nil
	case TypeUnsignedInt:
		v, ok := val.(uint32)
		if !ok {
			return bad("uint32")
		}
		return strconv.FormatUint(uint64(v), 10), nil
	case TypeUnsignedShort:
		v, ok := val.(uint16)
		if !ok {
			return bad("uint16")
		}
		return strconv.FormatUint(uint64(v), 10), nil
	case TypeUnsignedByte:
		v, ok := val.(uint8)
		if !ok {
			return bad("uint8")
		}
		return strconv.FormatUint(uint64(v), 10), nil
	case TypeBase64Binary:
		v, ok := val.([]byte)
		if !ok {
			return bad("[]byte")
		}
		return base64.StdEncoding.EncodeToString(v), nil
	case TypeDateTime:
		v, ok := val.(time.Time)
		if !ok {
			return bad("time.Time")
		}
		return formatWireTime(v), nil
	case TypeTime:
		v, ok := val.(time.Time)
		if !ok {
			return bad("time.Time")
		}
		// Only the time-of-day component is ever on the wire for xsd:time —
		// Format simply ignores the date fields of the stored time.Time
		// (which NewTime keeps as originally passed, e.g. a caller's
		// "now"), symmetric with parseXSDTime dropping them on decode.
		return v.Format("15:04:05.999999999"), nil
	case TypeDate:
		v, ok := val.(time.Time)
		if !ok {
			return bad("time.Time")
		}
		return v.Format("2006-01-02"), nil
	case TypeDuration:
		v, ok := val.(string)
		if !ok {
			return bad("string")
		}
		// Checked on the way out as well as on the way in: NewDuration
		// accepts any string (its signature has no error to return), so
		// this is the only place a caller-constructed duration is
		// validated before it becomes wire bytes.
		if !ValidDuration(v) {
			return "", fmt.Errorf("xmlda: %q is not a valid xsd:duration literal", v)
		}
		return v, nil
	default:
		return "", fmt.Errorf("xmlda: unsupported scalar type %q", st)
	}
}

// --- xsi:type attribute rendering ---

// typeAttrs returns the {xsi:type, xmlns:*} attributes needed to declare
// tn as an element's xsi:type. It always locally declares whatever
// namespace prefix it uses (a clean, conventional name — "xsd", "opc", or
// "ext" for anything else — never Go's auto-generated synthetic prefix),
// so the output is self-contained and correct whether or not it is
// embedded in a larger document that already declares a (possibly
// different) prefix for the same namespace.
//
// xsi:type is deliberately returned first, ahead of the xmlns:*
// declarations it depends on. XML attribute order carries no semantic
// meaning — a namespace declaration's scope covers its whole element
// regardless of where in the attribute list it appears — but at least
// one real-world OPC XML-DA client (github.com/dernate/gopcxmlda, as of
// v1.1.4) decodes a <Value> element's type by reading attribute index 0
// and splitting its value on ":", rather than resolving by attribute
// name. Putting xsi:type first costs nothing and happens to make that
// client's Read/Write/Subscribe/GetProperties value decoding work; see
// docs/interoperability.md.
func typeAttrs(e *xml.Encoder, existing []xml.Attr, tn QName) []xml.Attr {
	out := make([]xml.Attr, 0, 3)
	// xsi itself: declared here unless an ancestor already did, in which
	// case that binding is used and nothing is emitted.
	xsiPrefix, xsiInherited := ancestorPrefix(e, XSINamespace)
	if !xsiInherited {
		xsiPrefix = "xsi"
	}
	typeAttr := xml.Attr{Name: xml.Name{Local: xsiPrefix + ":type"}}
	if tn.Space == "" {
		typeAttr.Value = tn.Local
	} else if prefix, ok := ancestorPrefix(e, tn.Space); ok {
		typeAttr.Value = prefix + ":" + tn.Local
	} else {
		prefix := prefixIn(existing, tn.Space)
		typeAttr.Value = prefix + ":" + tn.Local
		out = append(out, xml.Attr{Name: xml.Name{Local: "xmlns:" + prefix}, Value: tn.Space})
	}
	out = append(out, typeAttr)
	if !xsiInherited {
		out = append(out, xml.Attr{Name: xml.Name{Local: "xmlns:xsi"}, Value: XSINamespace})
	}
	return out
}

func prefixForNamespace(space string) string {
	switch space {
	case XSDNamespace:
		return "xsd"
	case Namespace:
		return "opc"
	default:
		return "ext"
	}
}

// prefixIn returns the prefix to use for space on an element that already
// carries the attributes in existing: the conventional one from
// prefixForNamespace when it is free or already bound to this very URI,
// and a numbered variant ("ext2", "ext3", ...) when it is taken by a
// different URI.
//
// Every non-standard namespace shares the conventional prefix "ext", so
// one element declaring two QNames from two different vendor namespaces
// would otherwise emit xmlns:ext twice with different values — a
// duplicate attribute, and therefore a document no conforming parser
// accepts. That is not a contrived case: §3.1.9 requires a vendor result
// code to carry a vendor namespace and §3.1.10 requires the same of a
// vendor item property, and nothing obliges the two to be the same
// vendor. An ItemProperty naming one and failing with the other hits it.
//
// mergeAttrs cannot repair this on its own: by the time it sees the
// declaration, the attribute *value* ("ext:E_VENDOR") has already been
// built around the prefix, so the choice has to be made here, before the
// value is rendered.
func prefixIn(existing []xml.Attr, space string) string {
	base := prefixForNamespace(space)
	boundTo := func(p string) (string, bool) {
		for _, a := range existing {
			if a.Name.Local == "xmlns:"+p {
				return a.Value, true
			}
		}
		return "", false
	}
	if uri, taken := boundTo(base); !taken || uri == space {
		return base
	}
	for n := 2; ; n++ {
		cand := base + strconv.Itoa(n)
		if uri, taken := boundTo(cand); !taken || uri == space {
			return cand
		}
	}
}

// --- MarshalXML / UnmarshalXML ---

// MarshalXML implements xml.Marshaler. It renders v exactly as the
// specification's real-world wire format: scalars as element text, arrays
// as repeated scalar-typed child elements, and unknown types verbatim.
func (v Value) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	if v.typeName.IsZero() {
		return fmt.Errorf("xmlda: cannot marshal a Value with no declared type")
	}
	if v.kind == KindQuality && !v.isNil {
		// Delegated with start's attributes untouched, because
		// OPCQuality.MarshalXML writes the xsi:type itself. Adding it
		// here first would emit the attribute twice — a duplicate
		// attribute is not well-formed XML, and encoding/xml will not
		// stop it. A nilled quality falls through to the branch below,
		// which is the same shape every other nilled value takes.
		q, ok := v.scalar.(OPCQuality)
		if !ok {
			return fmt.Errorf("xmlda: Value declares %s but holds %T", v.typeName, v.scalar)
		}
		return q.MarshalXML(e, start)
	}
	base := append([]xml.Attr{}, start.Attr...)
	start.Attr = mergeAttrs(base, typeAttrs(e, base, v.typeName)...)
	if v.isNil {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "xsi:nil"}, Value: "true"})
		if err := e.EncodeToken(start); err != nil {
			return err
		}
		return e.EncodeToken(start.End())
	}
	switch v.kind {
	case KindUnknown:
		return v.marshalUnknown(e, start)
	case KindArray:
		return v.marshalArray(e, start)
	default:
		return v.marshalScalar(e, start)
	}
}

func (v Value) marshalUnknown(e *xml.Encoder, start xml.StartElement) error {
	// Re-declare whatever the captured content references but does not
	// declare itself, so the fragment means the same thing on the way out
	// as it did on the way in.
	for _, prefix := range slices.Sorted(maps.Keys(v.raw.Namespaces)) {
		start.Attr = mergeAttrs(start.Attr,
			xml.Attr{Name: xml.Name{Local: "xmlns:" + prefix}, Value: v.raw.Namespaces[prefix]})
	}
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	if err := writeRawInnerXML(e, v.raw.InnerXML); err != nil {
		return err
	}
	return e.EncodeToken(start.End())
}

func writeRawInnerXML(e *xml.Encoder, raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	d := xml.NewDecoder(strings.NewReader(string(raw)))
	// Declarations the fragment makes itself, so a name whose prefix the
	// tokenizer resolved can be written back under that same prefix.
	uriToPrefix := map[string]string{}
	for {
		tok, err := d.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("xmlda: re-encoding unknown value's inner XML: %w", err)
		}
		// Only content tokens are relayed. This inner XML came verbatim
		// from a peer (ADR-003 preserves an unrecognized xsi:type's bytes
		// exactly), and a Write with ReturnValuesOnReply echoes it
		// straight back into a response — so an xml.Directive or
		// xml.ProcInst in the request would be re-emitted mid-document,
		// producing invalid XML with no error to signal it, since the
		// encode itself succeeds. Comments are dropped for the same
		// reason: they carry no value and only widen the pass-through.
		switch t := tok.(type) {
		case xml.StartElement:
			for _, a := range t.Attr {
				if a.Name.Space == "xmlns" {
					uriToPrefix[a.Value] = a.Name.Local
				}
			}
			tok = flattenNames(t, uriToPrefix)
		case xml.EndElement:
			tok = xml.EndElement{Name: flatName(t.Name, uriToPrefix)}
		case xml.CharData:
		default:
			continue
		}
		if err := e.EncodeToken(xml.CopyToken(tok)); err != nil {
			return err
		}
	}
}

// flattenNames rewrites an element's own name and its attribute names into
// the flat "prefix:local" form this package uses everywhere it writes XML
// by hand.
//
// Relaying the decoded token unchanged corrupted the very bytes ADR-003
// exists to preserve. encoding/xml puts a resolved namespace URI in
// Name.Space — and an UNRESOLVED prefix there too, verbatim — while its
// encoder turns any non-empty Space into an xmlns declaration of its own.
// So <v:inner> from a document whose xmlns:v sat on an ancestor came back
// out as <inner xmlns="v">: the prefix promoted to a namespace URI, the
// real URI gone, and the value no longer the one the peer sent.
func flattenNames(t xml.StartElement, uriToPrefix map[string]string) xml.StartElement {
	out := xml.StartElement{Name: flatName(t.Name, uriToPrefix)}
	out.Attr = make([]xml.Attr, 0, len(t.Attr))
	for _, a := range t.Attr {
		switch {
		case a.Name.Space == "xmlns":
			out.Attr = append(out.Attr, xml.Attr{
				Name:  xml.Name{Local: "xmlns:" + a.Name.Local},
				Value: a.Value,
			})
		case a.Name.Space == "" && a.Name.Local == "xmlns":
			out.Attr = append(out.Attr, a)
		default:
			out.Attr = append(out.Attr, xml.Attr{Name: flatName(a.Name, uriToPrefix), Value: a.Value})
		}
	}
	return out
}

// flatName renders one name as it was written: a URI the fragment itself
// declared maps back to that declaration's prefix, and an unresolved
// prefix left in Space by the tokenizer IS the prefix.
func flatName(n xml.Name, uriToPrefix map[string]string) xml.Name {
	if n.Space == "" {
		return xml.Name{Local: n.Local}
	}
	prefix := n.Space
	if p, ok := uriToPrefix[n.Space]; ok {
		prefix = p
	}
	return xml.Name{Local: prefix + ":" + n.Local}
}

func (v Value) marshalScalar(e *xml.Encoder, start xml.StartElement) error {
	if v.typ == TypeQName {
		return v.marshalQNameScalar(e, start)
	}
	text, err := formatScalar(v.typ, v.scalar)
	if err != nil {
		return err
	}
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	if text != "" {
		if err := e.EncodeToken(xml.CharData(text)); err != nil {
			return err
		}
	}
	return e.EncodeToken(start.End())
}

func (v Value) marshalQNameScalar(e *xml.Encoder, start xml.StartElement) error {
	qn, ok := v.scalar.(QName)
	if !ok {
		return fmt.Errorf("xmlda: value declared as QName holds %T (internal inconsistency)", v.scalar)
	}
	text := qn.Local
	if qn.Space != "" {
		prefix := prefixIn(start.Attr, qn.Space)
		start.Attr = mergeAttrs(start.Attr, xml.Attr{Name: xml.Name{Local: "xmlns:" + prefix}, Value: qn.Space})
		text = prefix + ":" + qn.Local
	} else {
		// Explicitly declare "no default namespace" in this scope so an
		// unprefixed QName value round-trips unambiguously even when
		// nothing else in the document happens to declare a default
		// namespace (see resolveQName: an unprefixed value with no
		// default namespace in scope is otherwise unresolvable).
		start.Attr = mergeAttrs(start.Attr, xml.Attr{Name: xml.Name{Local: "xmlns"}, Value: ""})
	}
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	if err := e.EncodeToken(xml.CharData(text)); err != nil {
		return err
	}
	return e.EncodeToken(start.End())
}

func (v Value) marshalArray(e *xml.Encoder, start xml.StartElement) error {
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	elemLocal := string(v.array.elemType)
	n := v.array.Len()
	for i := range n {
		if v.array.elemType == TypeAnyType {
			elem := v.array.data.([]Value)[i]
			if err := elem.MarshalXML(e, xml.StartElement{Name: xml.Name{Local: "anyType"}}); err != nil {
				return err
			}
			continue
		}
		val, err := elementAt(v.array, i)
		if err != nil {
			return err
		}
		text, err := formatScalar(v.array.elemType, val)
		if err != nil {
			return err
		}
		childStart := xml.StartElement{Name: xml.Name{Local: elemLocal}}
		if err := e.EncodeToken(childStart); err != nil {
			return err
		}
		if text != "" {
			if err := e.EncodeToken(xml.CharData(text)); err != nil {
				return err
			}
		}
		if err := e.EncodeToken(childStart.End()); err != nil {
			return err
		}
	}
	return e.EncodeToken(start.End())
}

// scalarTypesByQName maps a standard XSD scalar xsi:type QName to its
// ScalarType.
var scalarTypesByQName = map[QName]ScalarType{
	{XSDNamespace, "string"}:        TypeString,
	{XSDNamespace, "boolean"}:       TypeBoolean,
	{XSDNamespace, "float"}:         TypeFloat,
	{XSDNamespace, "double"}:        TypeDouble,
	{XSDNamespace, "decimal"}:       TypeDecimal,
	{XSDNamespace, "long"}:          TypeLong,
	{XSDNamespace, "int"}:           TypeInt,
	{XSDNamespace, "short"}:         TypeShort,
	{XSDNamespace, "byte"}:          TypeByte,
	{XSDNamespace, "unsignedLong"}:  TypeUnsignedLong,
	{XSDNamespace, "unsignedInt"}:   TypeUnsignedInt,
	{XSDNamespace, "unsignedShort"}: TypeUnsignedShort,
	{XSDNamespace, "unsignedByte"}:  TypeUnsignedByte,
	{XSDNamespace, "base64Binary"}:  TypeBase64Binary,
	{XSDNamespace, "dateTime"}:      TypeDateTime,
	{XSDNamespace, "time"}:          TypeTime,
	{XSDNamespace, "date"}:          TypeDate,
	{XSDNamespace, "duration"}:      TypeDuration,
	{XSDNamespace, "QName"}:         TypeQName,
}

// arrayElemTypesByQName maps a standard OPC XML-DA ArrayOf<X> xsi:type
// QName to the ScalarType of its elements. Deliberately absent:
// ArrayOfUnsignedByte — see NewAnyArray's doc comment.
var arrayElemTypesByQName = map[QName]ScalarType{
	{Namespace, "ArrayOfByte"}:          TypeByte,
	{Namespace, "ArrayOfShort"}:         TypeShort,
	{Namespace, "ArrayOfUnsignedShort"}: TypeUnsignedShort,
	{Namespace, "ArrayOfInt"}:           TypeInt,
	{Namespace, "ArrayOfUnsignedInt"}:   TypeUnsignedInt,
	{Namespace, "ArrayOfLong"}:          TypeLong,
	{Namespace, "ArrayOfUnsignedLong"}:  TypeUnsignedLong,
	{Namespace, "ArrayOfFloat"}:         TypeFloat,
	{Namespace, "ArrayOfDecimal"}:       TypeDecimal,
	{Namespace, "ArrayOfDouble"}:        TypeDouble,
	{Namespace, "ArrayOfBoolean"}:       TypeBoolean,
	{Namespace, "ArrayOfString"}:        TypeString,
	{Namespace, "ArrayOfDateTime"}:      TypeDateTime,
	{Namespace, "ArrayOfAnyType"}:       TypeAnyType,
}

// maxAnyTypeArrayDepth bounds how many levels of nested ArrayOfAnyType a
// decode will follow before failing cleanly. Without it, decodeAnyTypeArray
// recurses back into Value's own decode logic once per nesting level with
// no bound, so a small but deeply-nested adversarial document could drive
// stack usage proportional to attacker-chosen depth. An implementation
// policy default (not spec-mandated), chosen generously above anything a
// legitimate OPC XML-DA message would plausibly nest.
const maxAnyTypeArrayDepth = 64

// UnmarshalXML implements xml.Unmarshaler. It never fails on an
// unrecognized xsi:type — see ADR-003 — and never panics on malformed
// input; all failures are returned as errors.
//
// Whatever it returns, it leaves the decoder positioned immediately after
// start's matching end tag. That invariant is what lets a caller treat a
// rejected <Value> as one item's own condition (ItemValue.DecodeErr,
// mapped to a per-item E_BADTYPE) and carry on decoding the items after
// it, instead of the whole request having to be abandoned because the
// token stream is no longer where the caller thinks it is.
func (v *Value) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	return v.unmarshalXML(d, start, 0)
}

// unmarshalXML is UnmarshalXML's real implementation, plus the depth
// counter threaded through ArrayOfAnyType recursion (see
// maxAnyTypeArrayDepth) that xml.Unmarshaler's fixed method signature has
// no room for.
//
// It enforces the always-consumed invariant documented on UnmarshalXML:
// decodeElement reports whether it got as far as this element's end tag,
// and anything that failed before that is finished off with d.Skip().
func (v *Value) unmarshalXML(d *xml.Decoder, start xml.StartElement, depth int) error {
	consumed, err := v.decodeElement(d, start, depth)
	if err != nil && !consumed {
		if skipErr := d.Skip(); skipErr != nil {
			return errors.Join(err, skipErr)
		}
	}
	return err
}

// decodeElement decodes start into v, reporting whether start's element
// was fully consumed from d (which, on the error paths, is what tells
// unmarshalXML whether it still has to skip the remainder).
func (v *Value) decodeElement(d *xml.Decoder, start xml.StartElement, depth int) (consumed bool, err error) {
	if depth > maxAnyTypeArrayDepth {
		return false, fmt.Errorf("xmlda: ArrayOfAnyType nesting exceeds the maximum depth of %d", maxAnyTypeArrayDepth)
	}
	isNil := false
	if nilAttr, ok := attrValue(start.Attr, xml.Name{Space: XSINamespace, Local: "nil"}); ok {
		isNil = strings.EqualFold(strings.TrimSpace(nilAttr), "true") || strings.TrimSpace(nilAttr) == "1"
	}

	rawType, ok := attrValue(start.Attr, xml.Name{Space: XSINamespace, Local: "type"})
	if !ok {
		// A nilled element needs no xsi:type: the schema declares
		// ArrayOfAnyType's and ArrayOfString's elements nillable="true",
		// and <anyType xsi:nil="true"/> is the shape a peer sends for a
		// missing element of unknown type. Rejecting it cost the item
		// E_BADTYPE and discarded the entire array value around it.
		// xsd:anyType is the honest declared type here — it is what the
		// schema says the element is — and IsNil() reports the absence.
		if isNil {
			v.kind, v.typ, v.typeName, v.isNil = KindUnknown, TypeAnyType, QName{XSDNamespace, "anyType"}, true
			return true, d.Skip()
		}
		return false, fmt.Errorf("xmlda: <%s> is missing a required xsi:type attribute", start.Name.Local)
	}
	tn, err := resolveQNameIn(d, start.Attr, rawType)
	if err != nil {
		return false, err
	}

	if isNil {
		kind, typ := decodeNilKind(tn)
		v.kind, v.typ, v.typeName, v.isNil = kind, typ, tn, true
		return true, d.Skip()
	}

	if st, ok := scalarTypesByQName[tn]; ok {
		return v.decodeScalar(d, start, st, tn)
	}
	if et, ok := arrayElemTypesByQName[tn]; ok {
		return v.decodeArray(d, et, tn, depth)
	}
	if tn == qualityTypeName {
		// Recognized rather than left to decodeUnknown: a quality that
		// round-trips as opaque bytes cannot be inspected by a client,
		// and this is a type the specification defines, not a vendor
		// extension.
		var q OPCQuality
		if err := q.UnmarshalXML(d, start); err != nil {
			return true, err
		}
		v.kind, v.typeName, v.scalar = KindQuality, tn, q
		return true, nil
	}
	return v.decodeUnknown(d, start, tn)
}

// decodeScalar always reports consumed=true: d.DecodeElement into a
// chardata-only holder reads through the element's end tag, and the only
// way it can fail is a token-level error, which means the document is
// malformed past this point anyway. Everything that can fail afterwards
// (QName resolution, literal parsing) happens with the element already
// behind us.
func (v *Value) decodeScalar(d *xml.Decoder, start xml.StartElement, st ScalarType, tn QName) (bool, error) {
	var holder struct {
		Text string `xml:",chardata"`
	}
	if err := d.DecodeElement(&holder, &start); err != nil {
		return true, fmt.Errorf("xmlda: decoding scalar %s: %w", tn, err)
	}
	if st == TypeQName {
		qn, err := resolveQNameIn(d, start.Attr, holder.Text)
		if err != nil {
			return true, err
		}
		v.kind, v.typ, v.typeName, v.scalar = KindScalar, st, tn, qn
		return true, nil
	}
	scalar, err := parseScalar(st, holder.Text)
	if err != nil {
		return true, err
	}
	v.kind, v.typ, v.typeName, v.scalar = KindScalar, st, tn, scalar
	return true, nil
}

func (v *Value) decodeArray(d *xml.Decoder, elemType ScalarType, tn QName, depth int) (bool, error) {
	if elemType == TypeAnyType {
		elems, err := decodeAnyTypeArray(d, depth)
		if err != nil {
			// A failure inside the element list stopped short of the
			// array's own end tag.
			return false, fmt.Errorf("xmlda: decoding array %s: %w", tn, err)
		}
		v.kind, v.typ, v.typeName, v.array = KindArray, elemType, tn, Array{elemType: elemType, typeName: tn, data: elems}
		return true, nil
	}

	wantLocal := string(elemType)
	var data any
	var err error
	switch elemType {
	case TypeByte:
		data, err = decodeScalarArray(d, tn, wantLocal, func(s string) (int8, error) {
			r, e := parseScalar(TypeByte, s)
			if e != nil {
				return 0, e
			}
			return r.(int8), nil
		})
	case TypeShort:
		data, err = decodeScalarArray(d, tn, wantLocal, func(s string) (int16, error) {
			r, e := parseScalar(TypeShort, s)
			if e != nil {
				return 0, e
			}
			return r.(int16), nil
		})
	case TypeUnsignedShort:
		data, err = decodeScalarArray(d, tn, wantLocal, func(s string) (uint16, error) {
			r, e := parseScalar(TypeUnsignedShort, s)
			if e != nil {
				return 0, e
			}
			return r.(uint16), nil
		})
	case TypeInt:
		data, err = decodeScalarArray(d, tn, wantLocal, func(s string) (int32, error) {
			r, e := parseScalar(TypeInt, s)
			if e != nil {
				return 0, e
			}
			return r.(int32), nil
		})
	case TypeUnsignedInt:
		data, err = decodeScalarArray(d, tn, wantLocal, func(s string) (uint32, error) {
			r, e := parseScalar(TypeUnsignedInt, s)
			if e != nil {
				return 0, e
			}
			return r.(uint32), nil
		})
	case TypeLong:
		data, err = decodeScalarArray(d, tn, wantLocal, func(s string) (int64, error) {
			r, e := parseScalar(TypeLong, s)
			if e != nil {
				return 0, e
			}
			return r.(int64), nil
		})
	case TypeUnsignedLong:
		data, err = decodeScalarArray(d, tn, wantLocal, func(s string) (uint64, error) {
			r, e := parseScalar(TypeUnsignedLong, s)
			if e != nil {
				return 0, e
			}
			return r.(uint64), nil
		})
	case TypeFloat:
		data, err = decodeScalarArray(d, tn, wantLocal, func(s string) (float32, error) {
			r, e := parseScalar(TypeFloat, s)
			if e != nil {
				return 0, e
			}
			return r.(float32), nil
		})
	case TypeDecimal:
		data, err = decodeScalarArray(d, tn, wantLocal, func(s string) (Decimal, error) {
			r, e := parseScalar(TypeDecimal, s)
			if e != nil {
				return "", e
			}
			return r.(Decimal), nil
		})
	case TypeDouble:
		data, err = decodeScalarArray(d, tn, wantLocal, func(s string) (float64, error) {
			r, e := parseScalar(TypeDouble, s)
			if e != nil {
				return 0, e
			}
			return r.(float64), nil
		})
	case TypeBoolean:
		data, err = decodeScalarArray(d, tn, wantLocal, func(s string) (bool, error) {
			r, e := parseScalar(TypeBoolean, s)
			if e != nil {
				return false, e
			}
			return r.(bool), nil
		})
	case TypeString:
		data, err = decodeScalarArray(d, tn, wantLocal, func(s string) (string, error) {
			return s, nil
		})
	case TypeDateTime:
		data, err = decodeScalarArray(d, tn, wantLocal, func(s string) (time.Time, error) {
			r, e := parseScalar(TypeDateTime, s)
			if e != nil {
				return time.Time{}, e
			}
			return r.(time.Time), nil
		})
	default:
		return false, fmt.Errorf("xmlda: array %s: unsupported element type %q", tn, elemType)
	}
	if err != nil {
		return false, fmt.Errorf("xmlda: decoding array %s: %w", tn, err)
	}
	v.kind, v.typ, v.typeName, v.array = KindArray, elemType, tn, Array{elemType: elemType, typeName: tn, data: data}
	return true, nil
}

// decodeScalarArray reads repeated <wantLocal>text</wantLocal> child
// elements until the array's end element, parsing each via parse.
func decodeScalarArray[T any](d *xml.Decoder, tn QName, wantLocal string, parse func(string) (T, error)) ([]T, error) {
	var out []T
	for {
		tok, err := d.Token()
		if err != nil {
			return nil, fmt.Errorf("array %s: %w", tn, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local != wantLocal {
				// Skip the offending child before giving up. Returning
				// with its start tag already consumed left the decoder
				// one element deep inside <Value>, and unmarshalXML's
				// d.Skip() then finished off that CHILD rather than the
				// value — so ItemValue read the value's own </Value> as
				// its own end tag, and the misalignment propagated all
				// the way up to <ItemList>, failing the WHOLE request
				// with a transport-level Client fault. One item's bad
				// array element must cost that item, not the request
				// (docs/limitations.md).
				if skipErr := d.Skip(); skipErr != nil {
					return nil, errors.Join(
						fmt.Errorf("array %s: unexpected child element %q, want %q", tn, t.Name.Local, wantLocal),
						skipErr)
				}
				return nil, fmt.Errorf("array %s: unexpected child element %q, want %q", tn, t.Name.Local, wantLocal)
			}
			var holder struct {
				Text string `xml:",chardata"`
			}
			if err := d.DecodeElement(&holder, &t); err != nil {
				return nil, err
			}
			val, err := parse(holder.Text)
			if err != nil {
				return nil, fmt.Errorf("array %s: element %d: %w", tn, len(out), err)
			}
			out = append(out, val)
		case xml.EndElement:
			return out, nil
		}
	}
}

// decodeAnyTypeArray reads child elements of an ArrayOfAnyType value,
// tolerating any child element local name (the .NET-style convention is
// "anyType", but only each child's own xsi:type actually matters). depth
// is this array's own nesting depth (see maxAnyTypeArrayDepth); each
// element is decoded one level deeper.
func decodeAnyTypeArray(d *xml.Decoder, depth int) ([]Value, error) {
	var elems []Value
	for {
		tok, err := d.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			var elem Value
			if err := elem.unmarshalXML(d, t, depth+1); err != nil {
				return nil, err
			}
			elems = append(elems, elem)
		case xml.EndElement:
			return elems, nil
		}
	}
}

// decodeUnknown always reports consumed=true, for the same reason
// decodeScalar does: an innerxml decode reads through the end tag.
func (v *Value) decodeUnknown(d *xml.Decoder, start xml.StartElement, tn QName) (bool, error) {
	var holder struct {
		Inner []byte `xml:",innerxml"`
	}
	if err := d.DecodeElement(&holder, &start); err != nil {
		return true, fmt.Errorf("xmlda: decoding unrecognized-type value %s: %w", tn, err)
	}
	v.kind, v.typeName, v.raw = KindUnknown, tn, RawValue{
		TypeName:   tn,
		InnerXML:   holder.Inner,
		Namespaces: inheritedNamespaces(d, start.Attr, holder.Inner),
	}
	return true, nil
}

// inheritedNamespaces collects the prefix bindings inner references but
// does not declare itself, resolving them against the element's own
// attributes first and the document's prefix table second — the same
// order resolveQNameIn uses.
func inheritedNamespaces(d *xml.Decoder, elemAttrs []xml.Attr, inner []byte) map[string]string {
	used := prefixesUsedIn(inner)
	if len(used) == 0 {
		return nil
	}
	out := make(map[string]string, len(used))
	for prefix := range used {
		if uri, ok := localPrefixBinding(elemAttrs, prefix); ok {
			out[prefix] = uri
			continue
		}
		if uri, ok := documentPrefix(d, prefix); ok {
			out[prefix] = uri
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// prefixesUsedIn lists the element and attribute prefixes appearing in a
// captured fragment, minus the ones the fragment declares itself.
func prefixesUsedIn(inner []byte) map[string]struct{} {
	used := map[string]struct{}{}
	declared := map[string]struct{}{}
	d := xml.NewDecoder(bytes.NewReader(inner))
	for {
		tok, err := d.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		// encoding/xml reports an unresolvable prefix by leaving it in
		// Name.Space, which is exactly the case being repaired here.
		if se.Name.Space != "" {
			used[se.Name.Space] = struct{}{}
		}
		for _, a := range se.Attr {
			if a.Name.Space == "xmlns" {
				declared[a.Name.Local] = struct{}{}
				continue
			}
			if a.Name.Space != "" && a.Name.Space != "xmlns" {
				used[a.Name.Space] = struct{}{}
			}
		}
	}
	for p := range declared {
		delete(used, p)
	}
	return used
}

// documentPrefix resolves prefix against the whole-document table
// registered for d, if there is one.
func documentPrefix(d *xml.Decoder, prefix string) (string, bool) {
	v, ok := decoderScopes.Load(d)
	if !ok {
		return "", false
	}
	uri, ok := v.(*prefixScope).table[prefix]
	return uri, ok
}
