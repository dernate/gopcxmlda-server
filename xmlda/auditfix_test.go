package xmlda

import (
	"encoding/xml"
	"math"
	"strings"
	"testing"
)

// TestDecodeArray_WrongChildElementStaysWithinTheItem pins the decoder's
// always-consumed invariant for the typed-array path. decodeScalarArray
// gave up with the offending child's start tag already read, so
// unmarshalXML's d.Skip() finished off that CHILD instead of the <Value>
// — leaving the decoder one element deep. ItemValue then read the value's
// own </Value> as its own end tag and the misalignment propagated to
// <ItemList>, failing the entire request with a transport-level Client
// fault. One item's mistyped array element must cost that item only.
func TestDecodeArray_WrongChildElementStaysWithinTheItem(t *testing.T) {
	raw := []byte(`<Wrap xmlns:opc="` + Namespace + `" xmlns:xsi="` + XSINamespace + `" xmlns:xsd="` + XSDNamespace + `">` +
		`<Items ItemName="A"><Value xsi:type="opc:ArrayOfInt"><double>1</double></Value></Items>` +
		`<Items ItemName="B"><Value xsi:type="xsd:int">7</Value></Items>` +
		`</Wrap>`)
	var w struct {
		XMLName xml.Name    `xml:"Wrap"`
		Items   []ItemValue `xml:"Items"`
	}
	if err := Decode(raw, &w); err != nil {
		t.Fatalf("the whole document failed to decode because of one bad array element: %v", err)
	}
	if len(w.Items) != 2 {
		t.Fatalf("decoder misaligned: got %d items, want 2", len(w.Items))
	}
	if w.Items[0].DecodeErr == nil {
		t.Error("the item with the mistyped array element carries no DecodeErr")
	}
	if got := ItemResultIDFor(w.Items[0].DecodeErr); got != ErrBadType {
		t.Errorf("bad array element maps to %v, want E_BADTYPE", got)
	}
	if w.Items[1].DecodeErr != nil {
		t.Errorf("the following, perfectly valid item was damaged: %v", w.Items[1].DecodeErr)
	}
	if w.Items[1].Value == nil {
		t.Fatal("the following item lost its value")
	}
	if n, err := w.Items[1].Value.Int32(); err != nil || n != 7 {
		t.Errorf("following item value = %v (err=%v), want 7", n, err)
	}
}

// TestValueTypeQualifier_RejectsAnInvalidLiteral pins that a qualifier
// may not retype a literal the target type cannot hold. Retyping without
// re-parsing moved the failure from the decoder to the encoder: the item
// decoded cleanly, reached the backend, and only the response failed —
// as a whole-operation E_FAIL, after the write had already happened.
func TestValueTypeQualifier_RejectsAnInvalidLiteral(t *testing.T) {
	decodeOne := func(t *testing.T, literal string) ItemValue {
		t.Helper()
		raw := []byte(`<Wrap xmlns:xsi="` + XSINamespace + `" xmlns:xsd="` + XSDNamespace + `">` +
			`<Items ItemName="A" ValueTypeQualifier="xsd:duration">` +
			`<Value xsi:type="xsd:string">` + literal + `</Value></Items></Wrap>`)
		var w struct {
			XMLName xml.Name    `xml:"Wrap"`
			Items   []ItemValue `xml:"Items"`
		}
		if err := Decode(raw, &w); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if len(w.Items) != 1 {
			t.Fatalf("got %d items, want 1", len(w.Items))
		}
		return w.Items[0]
	}

	bad := decodeOne(t, "hello")
	if bad.DecodeErr == nil {
		t.Fatal("a non-duration literal was accepted as an xsd:duration")
	}
	if got := ItemResultIDFor(bad.DecodeErr); got != ErrBadType {
		t.Errorf("ResultID = %v, want E_BADTYPE", got)
	}

	// A valid literal must still be retyped, or the qualifier stops
	// working for the peers that send it (OQ-12).
	good := decodeOne(t, "P1DT2H")
	if good.DecodeErr != nil {
		t.Fatalf("a valid duration literal was rejected: %v", good.DecodeErr)
	}
	if got := good.Value.Type(); got != TypeDuration {
		t.Errorf("type = %v, want duration", got)
	}
	// And what decodes must encode: that symmetry is the whole point.
	if _, err := xml.Marshal(*good.Value); err != nil {
		t.Errorf("a value accepted at decode time failed to encode: %v", err)
	}
}

// TestValueEqual_NaNComparesEqual pins change detection for a failed
// analog input. IEEE-754 says NaN != NaN, but Equal answers "is this the
// same value as before", and two consecutive NaN readings are. Comparing
// them with == reported a change on every poll: every long-poll returned
// immediately, the buffer filled, DataBufferOverflow was raised, and one
// broken sensor turned a subscription into a busy loop between client and
// server.
func TestValueEqual_NaNComparesEqual(t *testing.T) {
	if !NewFloat64(math.NaN()).Equal(NewFloat64(math.NaN())) {
		t.Error("two float64 NaN readings compare unequal, so a constant NaN reads as a change on every poll")
	}
	if !NewFloat32(float32(math.NaN())).Equal(NewFloat32(float32(math.NaN()))) {
		t.Error("two float32 NaN readings compare unequal")
	}
	// A transition into or out of NaN is still a change.
	if NewFloat64(math.NaN()).Equal(NewFloat64(1)) {
		t.Error("NaN and 1.0 compare equal")
	}
	if NewFloat64(1).Equal(NewFloat64(math.NaN())) {
		t.Error("1.0 and NaN compare equal")
	}
	// Infinities were never affected and must stay comparable.
	if !NewFloat64(math.Inf(1)).Equal(NewFloat64(math.Inf(1))) {
		t.Error("+Inf no longer compares equal to itself")
	}
	if NewFloat64(math.Inf(1)).Equal(NewFloat64(math.Inf(-1))) {
		t.Error("+Inf compares equal to -Inf")
	}
}

// TestArrayAccessors_ReturnACopy pins Value's value semantics. The
// constructors copy their input and Value.Bytes already copied, but the
// typed array accessors handed out the internal slice — so a caller could
// mutate a Value it merely read from. In the subscription engine that
// backing array is shared by an item's last reported sample, every
// buffered update referring to it, and the encoder writing the response.
func TestArrayAccessors_ReturnACopy(t *testing.T) {
	v := NewArrayValue(NewFloat64Array([]float64{1, 2, 3}))
	got, err := mustArray(t, v).Float64s()
	if err != nil {
		t.Fatal(err)
	}
	got[0] = 99
	again, err := mustArray(t, v).Float64s()
	if err != nil {
		t.Fatal(err)
	}
	if again[0] != 1 {
		t.Errorf("mutating an accessor's result changed the Value: got %v, want [1 2 3]", again)
	}

	// Same for the other element shapes a subscription actually carries.
	sv := NewArrayValue(NewStringArray([]string{"a", "b"}))
	ss, _ := mustArray(t, sv).Strings()
	ss[0] = "mutated"
	ss2, _ := mustArray(t, sv).Strings()
	if ss2[0] != "a" {
		t.Errorf("Strings() aliases internal storage: %v", ss2)
	}

	av := NewArrayValue(NewAnyArray([]Value{NewInt32(1)}))
	as, _ := mustArray(t, av).Any()
	as[0] = NewInt32(42)
	as2, _ := mustArray(t, av).Any()
	if n, err := as2[0].Int32(); err != nil || n != 1 {
		t.Errorf("Any() aliases internal storage: got %v (err=%v), want 1", n, err)
	}
}

func mustArray(t *testing.T, v Value) Array {
	t.Helper()
	a, err := v.Array()
	if err != nil {
		t.Fatalf("Array(): %v", err)
	}
	return a
}

// TestDecodeArray_NilledElementWithoutType pins the two element types the
// schema declares nillable="true" (ArrayOfAnyType's <anyType> and
// ArrayOfString's <string>). A nilled element carries no xsi:type — there
// is no value to type — and rejecting it cost the item E_BADTYPE and
// discarded the whole array around it.
func TestDecodeArray_NilledElementWithoutType(t *testing.T) {
	raw := []byte(`<Wrap xmlns:opc="` + Namespace + `" xmlns:xsi="` + XSINamespace + `" xmlns:xsd="` + XSDNamespace + `">` +
		`<Items ItemName="A"><Value xsi:type="opc:ArrayOfAnyType">` +
		`<anyType xsi:type="xsd:int">1</anyType>` +
		`<anyType xsi:nil="true"/>` +
		`</Value></Items></Wrap>`)
	var w struct {
		XMLName xml.Name    `xml:"Wrap"`
		Items   []ItemValue `xml:"Items"`
	}
	if err := Decode(raw, &w); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(w.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(w.Items))
	}
	if w.Items[0].DecodeErr != nil {
		t.Fatalf("a schema-legal nilled array element was rejected: %v", w.Items[0].DecodeErr)
	}
	arr, err := w.Items[0].Value.Array()
	if err != nil {
		t.Fatalf("Array(): %v", err)
	}
	elems, err := arr.Any()
	if err != nil {
		t.Fatalf("Any(): %v", err)
	}
	if len(elems) != 2 {
		t.Fatalf("got %d elements, want 2", len(elems))
	}
	if elems[0].IsNil() {
		t.Error("the typed element was decoded as nil")
	}
	if !elems[1].IsNil() {
		t.Error("the nilled element did not come back as nil")
	}
}

// TestDecode_NonUTF8Encodings pins the encodings a real peer sends.
// encoding/xml refuses any declared encoding but UTF-8 unless a
// CharsetReader is installed, so UTF-16 — which XML 1.0 §4.3.3 obliges
// every processor to accept — and ISO-8859-1, still emitted by legacy
// industrial clients for item names with umlauts, were rejected outright
// with a Go-internal message in the fault text.
func TestDecode_NonUTF8Encodings(t *testing.T) {
	body := func(decl, itemName string) string {
		return decl + `<Wrap xmlns:xsi="` + XSINamespace + `" xmlns:xsd="` + XSDNamespace + `">` +
			`<Items ItemName="` + itemName + `"><Value xsi:type="xsd:int">7</Value></Items></Wrap>`
	}
	type wrap struct {
		XMLName xml.Name    `xml:"Wrap"`
		Items   []ItemValue `xml:"Items"`
	}

	t.Run("ISO-8859-1", func(t *testing.T) {
		utf8Body := body(`<?xml version="1.0" encoding="ISO-8859-1"?>`, "Höhe")
		latin1 := make([]byte, 0, len(utf8Body))
		for _, r := range utf8Body {
			latin1 = append(latin1, byte(r)) // every rune here is < 256
		}
		var w wrap
		if err := Decode(latin1, &w); err != nil {
			t.Fatalf("an ISO-8859-1 document was rejected: %v", err)
		}
		if len(w.Items) != 1 || w.Items[0].ItemName != "Höhe" {
			t.Fatalf("item name did not survive transcoding: %+v", w.Items)
		}
	})

	for _, tc := range []struct {
		name      string
		bigEndian bool
		bom       []byte
	}{
		{"UTF-16 big-endian with BOM", true, []byte{0xFE, 0xFF}},
		{"UTF-16 little-endian with BOM", false, []byte{0xFF, 0xFE}},
		{"UTF-16 big-endian without BOM", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := body(`<?xml version="1.0" encoding="UTF-16"?>`, "Höhe")
			enc := append([]byte{}, tc.bom...)
			for _, r := range src {
				if tc.bigEndian {
					enc = append(enc, byte(r>>8), byte(r))
				} else {
					enc = append(enc, byte(r), byte(r>>8))
				}
			}
			var w wrap
			if err := Decode(enc, &w); err != nil {
				t.Fatalf("a UTF-16 document was rejected: %v", err)
			}
			if len(w.Items) != 1 || w.Items[0].ItemName != "Höhe" {
				t.Fatalf("item name did not survive transcoding: %+v", w.Items)
			}
		})
	}

	t.Run("unknown encoding is still an error", func(t *testing.T) {
		var w wrap
		if err := Decode([]byte(body(`<?xml version="1.0" encoding="EBCDIC-CP-BE"?>`, "A")), &w); err == nil {
			t.Error("an unsupported encoding was silently accepted")
		}
	})
}

// TestNewDocumentLimited_RejectsDeepNesting pins the resource limit that
// has to run before any protocol limit can. MaxItemsPerRequest and the
// rest only apply after a full decode, and nesting is charged
// super-linearly in encoding/xml's bookkeeping — so a 4 MiB body of
// nothing but nested start tags cost roughly 128 MiB of live heap per
// request, an amplification MaxRequestBodyBytes cannot see.
func TestNewDocumentLimited_RejectsDeepNesting(t *testing.T) {
	deep := func(n int) []byte {
		var b []byte
		b = append(b, []byte(`<Wrap>`)...)
		for range n {
			b = append(b, []byte(`<a>`)...)
		}
		for range n {
			b = append(b, []byte(`</a>`)...)
		}
		return append(b, []byte(`</Wrap>`)...)
	}
	if _, err := NewDocumentLimited(deep(10), 64); err != nil {
		t.Fatalf("an ordinarily-shaped document was rejected: %v", err)
	}
	if _, err := NewDocumentLimited(deep(200), 64); err == nil {
		t.Error("a document nested past the ceiling was accepted")
	}
	// Sibling elements are not nesting: the depth counter must come back
	// down, or a long ItemList would trip the limit.
	var wide []byte
	wide = append(wide, []byte(`<Wrap>`)...)
	for range 500 {
		wide = append(wide, []byte(`<Items ItemName="x"/>`)...)
	}
	wide = append(wide, []byte(`</Wrap>`)...)
	if _, err := NewDocumentLimited(wide, 64); err != nil {
		t.Errorf("500 sibling elements were mistaken for nesting: %v", err)
	}
	if _, err := NewDocumentLimited(deep(200), 0); err != nil {
		t.Errorf("a zero ceiling must disable the check: %v", err)
	}
}

// TestUnknownType_RoundTripPreservesNamespaces pins the fidelity promise
// KindUnknown exists for: docs/protocol-support.md says an unknown or
// vendor xsi:type is "preserved verbatim for round-trip". ,innerxml
// captures the bytes and nothing else, so a <v:inner> whose xmlns:v was
// declared on an ancestor came back out with its prefix intact and its
// binding gone — which is not the same value, and on a second decode the
// prefix resolves to whatever happens to be in scope instead.
func TestUnknownType_RoundTripPreservesNamespaces(t *testing.T) {
	raw := []byte(`<Wrap xmlns:xsi="` + XSINamespace + `" xmlns:v="urn:vendor" xmlns:w="urn:other">` +
		`<Items ItemName="A"><Value xsi:type="v:Weird"><v:inner w:attr="1">x</v:inner></Value></Items></Wrap>`)
	var w struct {
		XMLName xml.Name    `xml:"Wrap"`
		Items   []ItemValue `xml:"Items"`
	}
	if err := Decode(raw, &w); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(w.Items) != 1 || w.Items[0].Value == nil {
		t.Fatalf("got %d items", len(w.Items))
	}
	v := *w.Items[0].Value
	if v.Kind() != KindUnknown {
		t.Fatalf("kind = %v, want KindUnknown", v.Kind())
	}

	out, err := xml.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Parsed as a whole document — which is how a peer reads it — the
	// emitted element must resolve the same prefixes to the same URIs.
	// (Raw().InnerXML is by definition a fragment, so the declarations
	// live on the <Value> element around it, not inside it.)
	saw := false
	d := xml.NewDecoder(strings.NewReader(string(out)))
	for {
		tok, err := d.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "inner" {
			continue
		}
		saw = true
		if se.Name.Space != "urn:vendor" {
			t.Errorf("<inner> resolved to namespace %q, want %q — the binding did not survive "+
				"the round trip:\n%s", se.Name.Space, "urn:vendor", out)
		}
		for _, a := range se.Attr {
			if a.Name.Local == "attr" && a.Name.Space != "urn:other" {
				t.Errorf("attribute resolved to namespace %q, want %q:\n%s", a.Name.Space, "urn:other", out)
			}
		}
	}
	if !saw {
		t.Errorf("the captured <v:inner> element did not survive re-encoding at all:\n%s", out)
	}
}
