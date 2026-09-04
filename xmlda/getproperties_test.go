package xmlda

import (
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/soap"
)

func TestGetPropertiesRequest_RoundTrip(t *testing.T) {
	path1 := "Loc/Item1"
	req := GetPropertiesRequest{
		ClientRequestHandle:  "CRH1",
		ReturnAllProperties:  false,
		ReturnPropertyValues: true,
		ItemIDs: []ItemIdentifier{
			{ItemPath: &path1, ItemName: "Item1"},
			{ItemName: "Item2"},
		},
		PropertyNames: []QName{StandardPropertyName(PropDescription), StandardPropertyName(PropEngineeringUnits)},
	}
	out, err := xmlMarshalNamed(t, "GetProperties", req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got GetPropertiesRequest
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v\ndoc: %s", err, out)
	}
	if got.ClientRequestHandle != "CRH1" || !got.ReturnPropertyValues {
		t.Fatalf("got %+v", got)
	}
	if len(got.ItemIDs) != 2 {
		t.Fatalf("got %d ItemIDs, want 2", len(got.ItemIDs))
	}
	if got.ItemIDs[0].ItemPath == nil || *got.ItemIDs[0].ItemPath != path1 || got.ItemIDs[0].ItemName != "Item1" {
		t.Fatalf("got %+v", got.ItemIDs[0])
	}
	if got.ItemIDs[1].ItemPath != nil || got.ItemIDs[1].ItemName != "Item2" {
		t.Fatalf("got %+v", got.ItemIDs[1])
	}
	if len(got.PropertyNames) != 2 {
		t.Fatalf("got %d PropertyNames, want 2", len(got.PropertyNames))
	}
}

func TestGetPropertiesResponse_PerItemResultID(t *testing.T) {
	value := NewString("engineering units string")
	resp := GetPropertiesResponse{
		Result: ReplyBase{ServerState: ServerStateRunning},
		PropertyLists: []PropertyReplyList{
			{
				ItemName: "Item1",
				Properties: []ItemProperty{
					{Name: StandardPropertyName(PropEngineeringUnits), Value: &value},
				},
			},
			{
				ItemName: "UnknownItem",
				ResultID: ErrUnknownItemName,
			},
		},
		Errors: DedupeErrors([]ErrorCode{ErrUnknownItemName}, nil),
	}
	out, err := xmlMarshalNamed(t, "GetPropertiesResponse", resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got GetPropertiesResponse
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v\ndoc: %s", err, out)
	}
	if len(got.PropertyLists) != 2 {
		t.Fatalf("got %d PropertyLists, want 2", len(got.PropertyLists))
	}
	if !got.PropertyLists[0].ResultID.IsZero() {
		t.Fatalf("item 0 should have no ResultID, got %+v", got.PropertyLists[0].ResultID)
	}
	if len(got.PropertyLists[0].Properties) != 1 {
		t.Fatalf("got %d properties, want 1", len(got.PropertyLists[0].Properties))
	}
	if got.PropertyLists[1].ResultID != ErrUnknownItemName {
		t.Fatalf("item 1: got ResultID=%+v, want E_UNKNOWNITEMNAME", got.PropertyLists[1].ResultID)
	}
	if len(got.Errors) != 1 {
		t.Fatalf("got %d Errors entries, want 1", len(got.Errors))
	}
}

func TestGetPropertiesRequest_ReturnAllPropertiesIgnoresNames(t *testing.T) {
	// REQ-PROPERTIES-001: PropertyNames is ignored if ReturnAllProperties
	// is true — this test only checks that both are representable and
	// round-trip; enforcing the "ignored" semantics is a server-layer
	// concern.
	req := GetPropertiesRequest{
		ReturnAllProperties: true,
		ItemIDs:             []ItemIdentifier{{ItemName: "Item1"}},
		PropertyNames:       []QName{StandardPropertyName(PropDescription)},
	}
	out, err := xmlMarshalNamed(t, "GetProperties", req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got GetPropertiesRequest
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !got.ReturnAllProperties {
		t.Fatalf("expected ReturnAllProperties=true")
	}
	if len(got.PropertyNames) != 1 {
		t.Fatalf("got %d PropertyNames, want 1 (still representable on the wire)", len(got.PropertyNames))
	}
}

// TestGetPropertiesRequest_RealFixture decodes the real captured request
// testdata/requests/getproperties_103.request.xml (REQ-PROPERTIES-001).
func TestGetPropertiesRequest_RealFixture(t *testing.T) {
	doc := readTestdata(t, "testdata", "requests", "getproperties_103.request.xml")
	var env soap.Envelope[GetPropertiesRequest]
	if err := Decode(doc, &env); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if env.Body.Fault != nil {
		t.Fatalf("unexpected fault: %+v", env.Body.Fault)
	}
	req := env.Body.Content
	if req == nil {
		t.Fatalf("expected non-nil request content")
	}
	if req.ClientRequestHandle != "TestClient" {
		t.Fatalf("got ClientRequestHandle=%q, want TestClient", req.ClientRequestHandle)
	}
	if !req.ReturnAllProperties || !req.ReturnPropertyValues {
		t.Fatalf("got %+v, want ReturnAllProperties/ReturnPropertyValues both true", req)
	}
	if len(req.ItemIDs) != 1 {
		t.Fatalf("got %d ItemIDs, want 1", len(req.ItemIDs))
	}
	if req.ItemIDs[0].ItemName != "Folder1/Folder4/Folder5/Item1" {
		t.Fatalf("got ItemName=%q, want Folder1/Folder4/Folder5/Item1", req.ItemIDs[0].ItemName)
	}
}

// TestGetPropertiesResponse_RealFixture decodes the real captured response
// testdata/responses/getproperties_116.response.xml — the full 7-property
// list the server returns for ReturnAllProperties=true (REQ-PROPERTIES-002,
// REQ-PROPERTIES-003).
func TestGetPropertiesResponse_RealFixture(t *testing.T) {
	doc := readTestdata(t, "testdata", "responses", "getproperties_116.response.xml")
	var env soap.Envelope[GetPropertiesResponse]
	if err := Decode(doc, &env); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if env.Body.Fault != nil {
		t.Fatalf("unexpected fault: %+v", env.Body.Fault)
	}
	resp := env.Body.Content
	if resp == nil {
		t.Fatalf("expected non-nil response content")
	}
	if resp.Result.ClientRequestHandle != "TestClient" {
		t.Fatalf("got ClientRequestHandle=%q, want TestClient", resp.Result.ClientRequestHandle)
	}
	if len(resp.PropertyLists) != 1 {
		t.Fatalf("got %d PropertyLists, want 1", len(resp.PropertyLists))
	}
	list := resp.PropertyLists[0]
	if list.ItemName != "Folder1/Folder4/Folder5/Item1" {
		t.Fatalf("got ItemName=%q, want Folder1/Folder4/Folder5/Item1", list.ItemName)
	}
	if !list.ResultID.IsZero() {
		t.Fatalf("expected no ResultID, got %+v", list.ResultID)
	}
	wantNames := []PropertyID{PropAccessRights, PropDataType, PropEUType, PropQuality, PropScanRate, PropTimestamp, PropValue}
	if len(list.Properties) != len(wantNames) {
		t.Fatalf("got %d properties, want %d", len(list.Properties), len(wantNames))
	}
	for i, id := range wantNames {
		p := list.Properties[i]
		if p.Name != StandardPropertyName(id) {
			t.Fatalf("property %d: got Name=%+v, want %+v", i, p.Name, StandardPropertyName(id))
		}
		if p.ItemName != "Item1" || p.ItemPath == nil || *p.ItemPath != "Folder1/Folder4/Folder5/" {
			t.Fatalf("property %d (%s): got ItemName=%q ItemPath=%v", i, p.Name.Local, p.ItemName, p.ItemPath)
		}
		if p.Value == nil {
			t.Fatalf("property %d (%s): nil Value", i, p.Name.Local)
		}
	}
	if s, err := list.Properties[0].Value.String(); err != nil || s != "readable" {
		t.Fatalf("accessRights: got %q, err=%v, want readable", s, err)
	}
	if qn, err := list.Properties[1].Value.QNameValue(); err != nil || qn != (QName{Space: XSDNamespace, Local: "int"}) {
		t.Fatalf("dataType: got %+v, err=%v, want xsd:int", qn, err)
	}
	if s, err := list.Properties[2].Value.String(); err != nil || s != "noEnum" {
		t.Fatalf("euType: got %q, err=%v, want noEnum", s, err)
	}
	// The quality property used to come back as KindUnknown — decoded as
	// opaque, round-trippable bytes, because OPCQuality was the one type
	// the specification puts in a <Value> position that Value could not
	// model. This capture is what a real server sends for it
	// (QualityField="good" LimitField="none" VendorField="0"), and it now
	// decodes into an inspectable OPCQuality.
	quality := list.Properties[3].Value
	if quality.Kind() != KindQuality {
		t.Fatalf("quality: got Kind=%v, want quality", quality.Kind())
	}
	if quality.TypeName() != (QName{Space: Namespace, Local: "OPCQuality"}) {
		t.Fatalf("quality: got TypeName=%+v, want opc:OPCQuality", quality.TypeName())
	}
	q, err := quality.Quality()
	if err != nil {
		t.Fatalf("quality: Quality(): %v", err)
	}
	if q.QualityField() != QualityGood || q.LimitField() != LimitNone || q.VendorField() != 0 {
		t.Fatalf("quality: got %v/%v/%d, want good/none/0", q.QualityField(), q.LimitField(), q.VendorField())
	}
	if f, err := list.Properties[4].Value.Float32(); err != nil || f != 2310 {
		t.Fatalf("scanRate: got %v, err=%v, want 2310", f, err)
	}
	wantTimestamp := time.Date(2026, 8, 24, 14, 56, 27, 613000000, time.UTC)
	if tm, err := list.Properties[5].Value.Time(); err != nil || !tm.Equal(wantTimestamp) {
		t.Fatalf("timestamp: got %v, err=%v, want %v", tm, err, wantTimestamp)
	}
	if n, err := list.Properties[6].Value.Int32(); err != nil || n != 51 {
		t.Fatalf("value: got %v, err=%v, want 51", n, err)
	}
}
