package xmlda

import (
	"testing"
)

func TestStandardPropertyName(t *testing.T) {
	cases := []struct {
		id   PropertyID
		want string
	}{
		{PropDataType, "dataType"},
		{PropValue, "value"},
		{PropQuality, "quality"},
		{PropTimestamp, "timestamp"},
		{PropAccessRights, "accessRights"},
		{PropScanRate, "scanRate"},
		{PropEUType, "euType"},
		{PropEUInfo, "euInfo"},
		{PropEngineeringUnits, "engineeringUnits"},
		{PropDescription, "description"},
		{PropHighEU, "highEU"},
		{PropLowEU, "lowEU"},
		{PropHighIR, "highIR"},
		{PropLowIR, "lowIR"},
		{PropCloseLabel, "closeLabel"},
		{PropOpenLabel, "openLabel"},
		{PropTimeZone, "timeZone"},
	}
	for _, tc := range cases {
		qn := StandardPropertyName(tc.id)
		if qn.Space != Namespace || qn.Local != tc.want {
			t.Fatalf("PropertyID %d: got %+v, want {%s %s}", tc.id, qn, Namespace, tc.want)
		}
	}
}

func TestStandardPropertyName_Unknown(t *testing.T) {
	if got := StandardPropertyName(9999); !got.IsZero() {
		t.Fatalf("expected zero QName for an unrecognized property ID, got %+v", got)
	}
}

func TestItemProperty_RoundTrip(t *testing.T) {
	path := "Loc/Item"
	value := NewInt32(42)
	p := ItemProperty{
		Name:        StandardPropertyName(PropEngineeringUnits),
		Description: "Engineering units",
		ItemPath:    &path,
		ItemName:    "Loc/Item.EU",
		Value:       &value,
	}
	out, err := xmlMarshalNamed(t, "Properties", p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ItemProperty
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v\ndoc: %s", err, out)
	}
	if got.Name != p.Name {
		t.Fatalf("Name: got %+v, want %+v", got.Name, p.Name)
	}
	if got.Description != p.Description {
		t.Fatalf("Description: got %q, want %q", got.Description, p.Description)
	}
	if got.ItemPath == nil || *got.ItemPath != path {
		t.Fatalf("ItemPath: got %v, want %v", got.ItemPath, path)
	}
	if got.ItemName != p.ItemName {
		t.Fatalf("ItemName: got %q, want %q", got.ItemName, p.ItemName)
	}
	if got.Value == nil {
		t.Fatalf("expected non-nil Value")
	}
	i32, err := got.Value.Int32()
	if err != nil || i32 != 42 {
		t.Fatalf("Value: got (%d, %v), want (42, nil)", i32, err)
	}
}

func TestItemProperty_ResultID(t *testing.T) {
	p := ItemProperty{
		Name:     StandardPropertyName(PropDescription),
		ResultID: ErrInvalidPID,
	}
	out, err := xmlMarshalNamed(t, "Properties", p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ItemProperty
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v\ndoc: %s", err, out)
	}
	if got.ResultID != ErrInvalidPID {
		t.Fatalf("ResultID: got %+v, want %+v", got.ResultID, ErrInvalidPID)
	}
}

func TestItemProperty_MissingNameAttribute(t *testing.T) {
	doc := []byte(`<Properties/>`)
	var got ItemProperty
	if err := Decode(doc, &got); err == nil {
		t.Fatalf("expected a decode error for a Properties element with no Name attribute")
	}
}

func TestItemProperty_NoValuePresent(t *testing.T) {
	p := ItemProperty{Name: StandardPropertyName(PropAccessRights)}
	out, err := xmlMarshalNamed(t, "Properties", p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ItemProperty
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Value != nil {
		t.Fatalf("expected nil Value, got %+v", got.Value)
	}
}
