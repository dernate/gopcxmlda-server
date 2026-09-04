package xmlda

import (
	"encoding/xml"
	"strings"
	"testing"
)

func marshalQuality(t *testing.T, q OPCQuality) []byte {
	t.Helper()
	out, err := xml.Marshal(struct {
		XMLName xml.Name
		Quality OPCQuality
	}{XMLName: xml.Name{Local: "root"}, Quality: q})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}

func unmarshalQuality(t *testing.T, doc []byte) OPCQuality {
	t.Helper()
	var wrapper struct {
		Quality OPCQuality `xml:"Quality"`
	}
	if err := Decode(doc, &wrapper); err != nil {
		t.Fatalf("Decode: %v\ndoc: %s", err, doc)
	}
	return wrapper.Quality
}

func TestOPCQuality_DefaultsAndRoundTrip(t *testing.T) {
	allQuality := []QualityField{
		QualityBad, QualityBadConfigurationError, QualityBadNotConnected, QualityBadDeviceFailure,
		QualityBadSensorFailure, QualityBadLastKnownValue, QualityBadCommFailure, QualityBadOutOfService,
		QualityBadWaitingForInitialData, QualityUncertain, QualityUncertainLastUsableValue,
		QualityUncertainSensorNotAccurate, QualityUncertainEUExceeded, QualityUncertainSubNormal,
		QualityGood, QualityGoodLocalOverride,
	}
	allLimit := []LimitField{LimitNone, LimitLow, LimitHigh, LimitConstant}

	for _, qf := range allQuality {
		for _, lf := range allLimit {
			q := NewQuality(qf, lf, 7)
			doc := marshalQuality(t, q)
			got := unmarshalQuality(t, doc)
			if got.QualityField() != qf {
				t.Fatalf("QualityField: got %s, want %s", got.QualityField(), qf)
			}
			if got.LimitField() != lf {
				t.Fatalf("LimitField: got %s, want %s", got.LimitField(), lf)
			}
			if got.VendorField() != 7 {
				t.Fatalf("VendorField: got %d, want 7", got.VendorField())
			}
		}
	}
}

func TestOPCQuality_GoodOmitsAttributesOnEncode(t *testing.T) {
	doc := marshalQuality(t, NewGoodQuality())
	s := string(doc)
	if strings.Contains(s, "QualityField=") {
		t.Fatalf("Good quality must omit QualityField attribute on encode, got: %s", s)
	}
	if strings.Contains(s, "LimitField=") {
		t.Fatalf("default (None) limit must omit LimitField attribute on encode, got: %s", s)
	}
	if strings.Contains(s, "VendorField=") {
		t.Fatalf("zero VendorField must be omitted on encode, got: %s", s)
	}
	got := unmarshalQuality(t, doc)
	if !got.IsGood() {
		t.Fatalf("expected IsGood() true for a Value with omitted QualityField")
	}
	if got.LimitField() != LimitNone {
		t.Fatalf("expected default LimitField=none, got %s", got.LimitField())
	}
	if got.VendorField() != 0 {
		t.Fatalf("expected default VendorField=0, got %d", got.VendorField())
	}
}

func TestOPCQuality_Predicates(t *testing.T) {
	cases := []struct {
		q              OPCQuality
		good, bad, unc bool
	}{
		{NewGoodQuality(), true, false, false},
		{NewQuality(QualityGoodLocalOverride, LimitNone, 0), true, false, false},
		{NewQuality(QualityBad, LimitNone, 0), false, true, false},
		{NewQuality(QualityBadDeviceFailure, LimitNone, 0), false, true, false},
		{NewQuality(QualityUncertain, LimitNone, 0), false, false, true},
		{NewQuality(QualityUncertainEUExceeded, LimitNone, 0), false, false, true},
	}
	for _, tc := range cases {
		if got := tc.q.IsGood(); got != tc.good {
			t.Fatalf("%s: IsGood() = %v, want %v", tc.q.QualityField(), got, tc.good)
		}
		if got := tc.q.IsBad(); got != tc.bad {
			t.Fatalf("%s: IsBad() = %v, want %v", tc.q.QualityField(), got, tc.bad)
		}
		if got := tc.q.IsUncertain(); got != tc.unc {
			t.Fatalf("%s: IsUncertain() = %v, want %v", tc.q.QualityField(), got, tc.unc)
		}
	}
}

func TestResolveValuePresence(t *testing.T) {
	cases := []struct {
		name          string
		q             OPCQuality
		haveLastKnown bool
		want          bool
	}{
		{"good always present", NewGoodQuality(), false, true},
		{"uncertain always present", NewQuality(QualityUncertain, LimitNone, 0), false, true},
		{"bad without last-known is absent", NewQuality(QualityBad, LimitNone, 0), false, false},
		{"bad with last-known is present", NewQuality(QualityBadLastKnownValue, LimitNone, 0), true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveValuePresence(tc.q, tc.haveLastKnown); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOPCQuality_RealFixtureShape(t *testing.T) {
	// testdata/responses/subscribe_680.response.xml:
	// <Quality LimitField="none" QualityField="good" VendorField="0" xsi:type="ns1:OPCQuality"/>
	doc := []byte(`<Quality xmlns:xsi="` + XSINamespace + `" xmlns:ns1="` + Namespace + `" LimitField="none" QualityField="good" VendorField="0" xsi:type="ns1:OPCQuality"/>`)
	var wrapper struct {
		Quality OPCQuality `xml:"Quality"`
	}
	if err := Decode(doc, &wrapper); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !wrapper.Quality.IsGood() {
		t.Fatalf("expected good quality")
	}
	if wrapper.Quality.LimitField() != LimitNone {
		t.Fatalf("got %s, want none", wrapper.Quality.LimitField())
	}
	if wrapper.Quality.VendorField() != 0 {
		t.Fatalf("got %d, want 0", wrapper.Quality.VendorField())
	}
}

// TestQualityValue_RoundTrip covers KindQuality, the Value shape added so
// a backend can serve standard item property 3.
//
// §3.1.10 p.40 declares that property's data type as OPCQuality — the one
// complex type this protocol puts in a <Value> position. Before
// KindQuality it decoded as KindUnknown (opaque bytes, round-trippable
// but not inspectable) and could not be constructed at all, so a backend
// could serve every standard property except that one.
func TestQualityValue_RoundTrip(t *testing.T) {
	q := NewQuality(QualityUncertain, LimitHigh, 7)
	v := NewQualityValue(q)

	if v.Kind() != KindQuality {
		t.Fatalf("Kind() = %v, want quality", v.Kind())
	}
	if !v.IsValid() {
		t.Error("IsValid() = false: a constructed quality value must carry its declared type")
	}
	if got := v.TypeName(); got != (QName{Space: Namespace, Local: "OPCQuality"}) {
		t.Errorf("TypeName() = %+v, want opc:OPCQuality", got)
	}

	out, err := xml.Marshal(struct {
		XMLName xml.Name
		Value   Value
	}{XMLName: xml.Name{Local: "root"}, Value: v})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wire := string(out)

	// The duplicate-attribute trap: OPCQuality.MarshalXML writes its own
	// xsi:type, so Value.MarshalXML must not add one as well. encoding/xml
	// happily emits the same attribute twice, and the result is not
	// well-formed XML — a class of defect this repository has shipped
	// before.
	if n := strings.Count(wire, "xsi:type="); n != 1 {
		t.Errorf("found %d xsi:type attributes, want exactly 1: %s", n, wire)
	}
	for _, want := range []string{
		`xsi:type="opc:OPCQuality"`,
		`QualityField="uncertain"`,
		`LimitField="high"`,
		`VendorField="7"`,
	} {
		if !strings.Contains(wire, want) {
			t.Errorf("missing %s in: %s", want, wire)
		}
	}

	// Decoded back out of the document it was written into, so the
	// prefix bindings the encoder emitted are the ones resolving the
	// xsi:type — the round trip a peer actually performs.
	var wrapper struct {
		Value Value `xml:"Value"`
	}
	if err := Decode(out, &wrapper); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	back := wrapper.Value
	if back.Kind() != KindQuality {
		t.Fatalf("decoded Kind() = %v, want quality", back.Kind())
	}
	gotQ, err := back.Quality()
	if err != nil {
		t.Fatalf("Quality(): %v", err)
	}
	if gotQ.QualityField() != QualityUncertain || gotQ.LimitField() != LimitHigh || gotQ.VendorField() != 7 {
		t.Errorf("decoded %v/%v/%d, want uncertain/high/7",
			gotQ.QualityField(), gotQ.LimitField(), gotQ.VendorField())
	}
	if !v.Equal(back) {
		t.Error("Equal() = false across a round trip")
	}
}

// TestQualityValue_EqualIgnoresAttributePresence pins that Equal compares
// what a quality MEANS, not how it was spelled: OPCQuality keeps its enum
// fields as pointers so an absent attribute stays distinguishable from a
// present one, and the schema's defaults make an omitted QualityField
// mean "good".
func TestQualityValue_EqualIgnoresAttributePresence(t *testing.T) {
	explicit := NewQualityValue(NewQuality(QualityGood, LimitNone, 0))

	// A peer that relies on the schema defaults sends no attributes at all.
	var bare Value
	doc := `<Value xmlns:xsi="` + XSINamespace + `" xmlns:opc="` + Namespace +
		`" xsi:type="opc:OPCQuality"/>`
	if err := Decode([]byte(doc), &bare); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if bare.Kind() != KindQuality {
		t.Fatalf("Kind() = %v, want quality", bare.Kind())
	}
	if !explicit.Equal(bare) {
		t.Error("an explicit good/none/0 quality and an attribute-less one compare unequal, " +
			"but the schema's defaults make them the same quality")
	}
}

// TestQualityValue_WrongAccessorsReportTypeError keeps the new Kind from
// silently answering accessors meant for other shapes.
func TestQualityValue_WrongAccessorsReportTypeError(t *testing.T) {
	v := NewQualityValue(NewQuality(QualityBad, LimitNone, 0))
	if _, err := v.String(); err == nil {
		t.Error("String() on a quality value returned no error")
	}
	if _, err := v.Int32(); err == nil {
		t.Error("Int32() on a quality value returned no error")
	}
	if _, err := v.Array(); err == nil {
		t.Error("Array() on a quality value returned no error")
	}

	// And the reverse: Quality() must refuse a value that is not one.
	if _, err := NewInt32(1).Quality(); err == nil {
		t.Error("Quality() on an int value returned no error")
	}
	// A nilled quality is a declared type with no value; Quality() must
	// not hand back a zero OPCQuality as though it were data.
	if _, err := NewNil(QName{Space: Namespace, Local: "OPCQuality"}).Quality(); err == nil {
		t.Error("Quality() on an xsi:nil quality returned no error")
	}
}
