package xmlda

import (
	"testing"
	"time"
)

func TestValue_Equal(t *testing.T) {
	cases := []struct {
		name string
		a, b Value
		want bool
	}{
		{"equal ints", NewInt32(5), NewInt32(5), true},
		{"different ints", NewInt32(5), NewInt32(6), false},
		{"different types", NewInt32(5), NewInt64(5), false},
		{"equal strings", NewString("x"), NewString("x"), true},
		{"equal bytes", NewBytes([]byte{1, 2}), NewBytes([]byte{1, 2}), true},
		{"different bytes", NewBytes([]byte{1, 2}), NewBytes([]byte{1, 3}), false},
		{"nil vs present", NewNil(QName{XSDNamespace, "int"}), NewInt32(0), false},
		{"both nil same type", NewNil(QName{XSDNamespace, "int"}), NewNil(QName{XSDNamespace, "int"}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.Equal(tc.b); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValue_Equal_DateTimeIgnoresMonotonic(t *testing.T) {
	// time.Now() may carry a monotonic reading; two values constructed
	// from it a moment apart must still compare equal if the wall time
	// (after normalization) matches — Equal uses time.Time.Equal, not ==.
	now := time.Now()
	a := NewDateTime(now)
	b := NewDateTime(now.Round(0)) // strips monotonic reading
	if !a.Equal(b) {
		t.Fatalf("expected dateTime values with the same instant to compare equal regardless of monotonic reading")
	}
}

func TestValue_Equal_Arrays(t *testing.T) {
	a := Value{kind: KindArray, typ: TypeInt, typeName: QName{Namespace, "ArrayOfInt"}, array: NewInt32Array([]int32{1, 2, 3})}
	b := Value{kind: KindArray, typ: TypeInt, typeName: QName{Namespace, "ArrayOfInt"}, array: NewInt32Array([]int32{1, 2, 3})}
	c := Value{kind: KindArray, typ: TypeInt, typeName: QName{Namespace, "ArrayOfInt"}, array: NewInt32Array([]int32{1, 2, 4})}
	if !a.Equal(b) {
		t.Fatalf("expected equal arrays to compare equal")
	}
	if a.Equal(c) {
		t.Fatalf("expected different arrays to compare unequal")
	}
}

// TestValue_Equal_DateTimeArrays exercises Array.equal's TypeDateTime
// branch specifically — it uses time.Time.Equal element-wise rather than
// the reflect.DeepEqual fallback every other scalar array type uses, so a
// length-misalignment or index-off-by-one bug there would not be caught
// by TestValue_Equal_Arrays (which only exercises a TypeInt array).
func TestValue_Equal_DateTimeArrays(t *testing.T) {
	t1 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2024, 6, 15, 12, 30, 0, 0, time.UTC)
	a := Value{kind: KindArray, typ: TypeDateTime, typeName: QName{Namespace, "ArrayOfDateTime"}, array: NewDateTimeArray([]time.Time{t1, t2})}
	b := Value{kind: KindArray, typ: TypeDateTime, typeName: QName{Namespace, "ArrayOfDateTime"}, array: NewDateTimeArray([]time.Time{t1, t2})}
	c := Value{kind: KindArray, typ: TypeDateTime, typeName: QName{Namespace, "ArrayOfDateTime"}, array: NewDateTimeArray([]time.Time{t1, t1})}
	d := Value{kind: KindArray, typ: TypeDateTime, typeName: QName{Namespace, "ArrayOfDateTime"}, array: NewDateTimeArray([]time.Time{t1})}
	if !a.Equal(b) {
		t.Fatalf("expected equal dateTime arrays to compare equal")
	}
	if a.Equal(c) {
		t.Fatalf("expected dateTime arrays differing in one element to compare unequal")
	}
	if a.Equal(d) {
		t.Fatalf("expected dateTime arrays of different length to compare unequal")
	}
	// Same instant via a different Location (monotonic-reading style
	// pitfall, but for zones): must still use time.Time.Equal semantics,
	// not ==, element-wise.
	e := Value{kind: KindArray, typ: TypeDateTime, typeName: QName{Namespace, "ArrayOfDateTime"}, array: NewDateTimeArray([]time.Time{t1.In(time.FixedZone("x", 3600)), t2})}
	if !a.Equal(e) {
		t.Fatalf("expected dateTime arrays with the same instants in different zones to compare equal")
	}
}

// TestValue_Equal_AnyTypeArrays exercises Array.equal's TypeAnyType
// branch, which recurses through Value.Equal per element rather than
// using the reflect.DeepEqual fallback — an index-misalignment bug there
// (e.g. comparing av[i] against the wrong bv[j]) would not be caught by
// TestValue_Equal_Arrays (TypeInt only).
func TestValue_Equal_AnyTypeArrays(t *testing.T) {
	mk := func(strs ...string) Array {
		elems := make([]Value, len(strs))
		for i, s := range strs {
			elems[i] = NewString(s)
		}
		return NewAnyArray(elems)
	}
	a := Value{kind: KindArray, typ: TypeAnyType, typeName: QName{Namespace, "ArrayOfAnyType"}, array: mk("a", "b", "c")}
	b := Value{kind: KindArray, typ: TypeAnyType, typeName: QName{Namespace, "ArrayOfAnyType"}, array: mk("a", "b", "c")}
	c := Value{kind: KindArray, typ: TypeAnyType, typeName: QName{Namespace, "ArrayOfAnyType"}, array: mk("a", "c", "b")}
	d := Value{kind: KindArray, typ: TypeAnyType, typeName: QName{Namespace, "ArrayOfAnyType"}, array: mk("a", "b")}
	if !a.Equal(b) {
		t.Fatalf("expected equal anyType arrays to compare equal")
	}
	if a.Equal(c) {
		t.Fatalf("expected anyType arrays with elements in a different order to compare unequal")
	}
	if a.Equal(d) {
		t.Fatalf("expected anyType arrays of different length to compare unequal")
	}
}

func TestValue_Equal_UnknownType(t *testing.T) {
	doc := []byte(`<Value xmlns:vendor="http://example.com/vendor" xsi:type="vendor:Weird" xmlns:xsi="` + XSINamespace + `"><a>1</a></Value>`)
	v1 := unmarshalValue(t, doc)
	v2 := unmarshalValue(t, doc)
	if !v1.Equal(v2) {
		t.Fatalf("expected two decodes of the same unknown-type value to compare equal")
	}
}
