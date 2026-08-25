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
