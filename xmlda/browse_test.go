package xmlda

import (
	"strings"
	"testing"

	"github.com/dernate/gopcxmlda-server/soap"
)

func TestBrowseRequest_RoundTrip(t *testing.T) {
	path := "Loc"
	req := BrowseRequest{
		ClientRequestHandle:  "CRH1",
		ItemName:             "Root",
		ItemPath:             &path,
		MaxElementsReturned:  10,
		BrowseFilter:         BrowseFilterBranch,
		ElementNameFilter:    "P*",
		ReturnAllProperties:  true,
		ReturnPropertyValues: true,
		PropertyNames:        []QName{StandardPropertyName(PropDescription), {Space: XSDNamespace, Local: "int"}},
	}
	out, err := xmlMarshalNamed(t, "Browse", req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got BrowseRequest
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v\ndoc: %s", err, out)
	}
	if got.ClientRequestHandle != "CRH1" || got.ItemName != "Root" {
		t.Fatalf("got %+v", got)
	}
	if got.ItemPath == nil || *got.ItemPath != path {
		t.Fatalf("ItemPath: got %v, want %v", got.ItemPath, path)
	}
	if got.MaxElementsReturned != 10 || got.BrowseFilter != BrowseFilterBranch || got.ElementNameFilter != "P*" {
		t.Fatalf("got %+v", got)
	}
	if !got.ReturnAllProperties || !got.ReturnPropertyValues {
		t.Fatalf("got %+v", got)
	}
	if len(got.PropertyNames) != 2 {
		t.Fatalf("got %d PropertyNames, want 2", len(got.PropertyNames))
	}
	if got.PropertyNames[0] != StandardPropertyName(PropDescription) {
		t.Fatalf("got %+v, want %+v", got.PropertyNames[0], StandardPropertyName(PropDescription))
	}
}

func TestBrowseRequest_RootBrowse(t *testing.T) {
	// Blank ItemName/ItemPath means "browse the address space root".
	req := BrowseRequest{}
	out, err := xmlMarshalNamed(t, "Browse", req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got BrowseRequest
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.ItemName != "" || got.ItemPath != nil {
		t.Fatalf("got %+v, want blank ItemName and nil ItemPath", got)
	}
}

func TestBrowseElement_RequiredBoolsAlwaysEmitted(t *testing.T) {
	// REQ-BROWSE-005: IsItem/HasChildren are required and must be emitted
	// even when false — never omitted the way an optional bool would be.
	el := BrowseElement{Name: "Branch1", IsItem: false, HasChildren: false}
	out, err := xmlMarshalNamed(t, "Elements", el)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `IsItem="false"`) || !strings.Contains(s, `HasChildren="false"`) {
		t.Fatalf("expected explicit IsItem/HasChildren=false in output, got: %s", s)
	}
	var got BrowseElement
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.IsItem || got.HasChildren {
		t.Fatalf("got %+v", got)
	}
}

func TestBrowseElement_HintNode(t *testing.T) {
	// An element with IsItem=true but no ItemPath/ItemName is a
	// non-actionable "hint" (§3.8.2).
	el := BrowseElement{Name: "SomeHint", IsItem: true, HasChildren: false}
	out, err := xmlMarshalNamed(t, "Elements", el)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got BrowseElement
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !got.IsItem {
		t.Fatalf("expected IsItem=true")
	}
	if got.ItemPath != nil || got.ItemName != "" {
		t.Fatalf("expected no ItemPath/ItemName for a hint node, got %+v", got)
	}
}

func TestBrowseResponse_EmptyResultIsSuccess(t *testing.T) {
	// REQ-BROWSE-006: a level with zero children is a successful empty
	// result, not an error.
	resp := BrowseResponse{Result: ReplyBase{ServerState: ServerStateRunning}}
	out, err := xmlMarshalNamed(t, "BrowseResponse", resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got BrowseResponse
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got.Elements) != 0 {
		t.Fatalf("got %d elements, want 0", len(got.Elements))
	}
	if len(got.Errors) != 0 {
		t.Fatalf("expected no Errors for a valid empty browse result")
	}
}

func TestBrowseResponse_ContinuationAndFiltering(t *testing.T) {
	itemPath := "Loc/Item1"
	resp := BrowseResponse{
		MoreElements:      true,
		ContinuationPoint: "opaque-cursor-1",
		Result:            ReplyBase{ServerState: ServerStateRunning},
		Elements: []BrowseElement{
			{Name: "Item1", ItemPath: &itemPath, ItemName: "Item1", IsItem: true, HasChildren: false},
			{Name: "Branch1", IsItem: false, HasChildren: true},
		},
	}
	out, err := xmlMarshalNamed(t, "BrowseResponse", resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got BrowseResponse
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !got.MoreElements || got.ContinuationPoint != "opaque-cursor-1" {
		t.Fatalf("got %+v", got)
	}
	if len(got.Elements) != 2 {
		t.Fatalf("got %d elements, want 2", len(got.Elements))
	}
	if !got.Elements[0].IsItem || got.Elements[0].HasChildren {
		t.Fatalf("element 0: got %+v", got.Elements[0])
	}
	if got.Elements[1].IsItem || !got.Elements[1].HasChildren {
		t.Fatalf("element 1: got %+v", got.Elements[1])
	}
}

func TestBrowseElement_InlineProperties(t *testing.T) {
	value := NewInt32(5)
	el := BrowseElement{
		Name:        "Item1",
		ItemName:    "Item1",
		IsItem:      true,
		HasChildren: false,
		Properties: []ItemProperty{
			{Name: StandardPropertyName(PropDataType), Value: &value},
		},
	}
	out, err := xmlMarshalNamed(t, "Elements", el)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got BrowseElement
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got.Properties) != 1 {
		t.Fatalf("got %d properties, want 1", len(got.Properties))
	}
	if got.Properties[0].Name != StandardPropertyName(PropDataType) {
		t.Fatalf("got %+v", got.Properties[0].Name)
	}
}

// TestBrowseRequest_RealFixture_Root decodes the real captured root-browse
// request testdata/requests/browse_653.request.xml (REQ-BROWSE-001).
func TestBrowseRequest_RealFixture_Root(t *testing.T) {
	doc := readTestdata(t, "testdata", "requests", "browse_653.request.xml")
	var env soap.Envelope[BrowseRequest]
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
	if req.ItemName != "" || req.ItemPath == nil || *req.ItemPath != "" {
		t.Fatalf("got ItemName=%q ItemPath=%v, want blank/blank (address space root)", req.ItemName, req.ItemPath)
	}
	if req.BrowseFilter != BrowseFilterAll {
		t.Fatalf("got BrowseFilter=%q, want all", req.BrowseFilter)
	}
	if !req.ReturnPropertyValues {
		t.Fatalf("expected ReturnPropertyValues=true")
	}
	if len(req.PropertyNames) != 2 {
		t.Fatalf("got %d PropertyNames, want 2", len(req.PropertyNames))
	}
	if req.PropertyNames[0] != StandardPropertyName(PropDataType) || req.PropertyNames[1] != StandardPropertyName(PropAccessRights) {
		t.Fatalf("got PropertyNames=%+v, want [dataType, accessRights]", req.PropertyNames)
	}
}

// TestBrowseResponse_RealFixture_Root decodes the real captured root-browse
// response testdata/responses/browse_662.response.xml (REQ-BROWSE-001,
// REQ-BROWSE-005).
func TestBrowseResponse_RealFixture_Root(t *testing.T) {
	doc := readTestdata(t, "testdata", "responses", "browse_662.response.xml")
	var env soap.Envelope[BrowseResponse]
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
	if resp.MoreElements {
		t.Fatalf("expected MoreElements=false")
	}
	wantNames := []string{"Folder1", "Folder2", "Folder3"}
	if len(resp.Elements) != len(wantNames) {
		t.Fatalf("got %d elements, want %d", len(resp.Elements), len(wantNames))
	}
	for i, el := range resp.Elements {
		if el.Name != wantNames[i] || el.ItemName != wantNames[i] {
			t.Fatalf("element %d: got Name=%q ItemName=%q, want %q", i, el.Name, el.ItemName, wantNames[i])
		}
		if el.IsItem || !el.HasChildren {
			t.Fatalf("element %d (%s): got IsItem=%v HasChildren=%v, want false/true (branch)", i, el.Name, el.IsItem, el.HasChildren)
		}
	}
}

// browseDeepFixtureItems is the anonymized shape of the 32 elements in
// testdata/responses/browse_684.response.xml, in document order: 26 leaf
// items (each with inline dataType/accessRights properties, exercising a
// spread of scalar types) followed by 6 branch elements (no inline
// properties).
var browseDeepFixtureItems = []struct {
	name        string
	isItem      bool
	hasChildren bool
	dataType    string // xsd local name; empty for branches
}{
	{"Item1", true, false, "unsignedInt"},
	{"Item2", true, false, "boolean"},
	{"Item3", true, false, "unsignedByte"},
	{"Item4", true, false, "string"},
	{"Item5", true, false, "string"},
	{"Item6", true, false, "unsignedShort"},
	{"Item7", true, false, "float"},
	{"Item8", true, false, "float"},
	{"Item9", true, false, "float"},
	{"Item10", true, false, "unsignedInt"},
	{"Item11", true, false, "unsignedInt"},
	{"Item12", true, false, "unsignedInt"},
	{"Item13", true, false, "int"},
	{"Item14", true, false, "float"},
	{"Item15", true, false, "int"},
	{"Item16", true, false, "unsignedInt"},
	{"Item17", true, false, "unsignedInt"},
	{"Item18", true, false, "unsignedInt"},
	{"Item19", true, false, "int"},
	{"Item20", true, false, "int"},
	{"Item21", true, false, "int"},
	{"Item22", true, false, "float"},
	{"Item23", true, false, "int"},
	{"Item24", true, false, "int"},
	{"Item25", true, false, "int"},
	{"Item26", true, false, "int"},
	{"Folder7", false, true, ""},
	{"Folder8", false, true, ""},
	{"Folder9", false, true, ""},
	{"Folder10", false, true, ""},
	{"Folder11", false, true, ""},
	{"Folder12", false, true, ""},
}

// TestBrowseRequest_RealFixture_Deep decodes the real captured deep-browse
// request testdata/requests/browse_676.request.xml (REQ-BROWSE-001).
func TestBrowseRequest_RealFixture_Deep(t *testing.T) {
	doc := readTestdata(t, "testdata", "requests", "browse_676.request.xml")
	var env soap.Envelope[BrowseRequest]
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
	if req.ItemName != "Folder1/Folder4/Folder6" {
		t.Fatalf("got ItemName=%q, want Folder1/Folder4/Folder6", req.ItemName)
	}
	if req.BrowseFilter != BrowseFilterAll {
		t.Fatalf("got BrowseFilter=%q, want all", req.BrowseFilter)
	}
}

// TestBrowseResponse_RealFixture_Deep decodes the real captured deep-browse
// response testdata/responses/browse_684.response.xml — 26 leaf items with
// inline dataType/accessRights properties spanning a wide range of scalar
// types, plus 6 branch elements with none (REQ-BROWSE-001, REQ-BROWSE-005,
// REQ-BROWSE-007).
func TestBrowseResponse_RealFixture_Deep(t *testing.T) {
	doc := readTestdata(t, "testdata", "responses", "browse_684.response.xml")
	var env soap.Envelope[BrowseResponse]
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
	if resp.MoreElements {
		t.Fatalf("expected MoreElements=false")
	}
	if len(resp.Elements) != len(browseDeepFixtureItems) {
		t.Fatalf("got %d elements, want %d", len(resp.Elements), len(browseDeepFixtureItems))
	}
	for i, want := range browseDeepFixtureItems {
		el := resp.Elements[i]
		if el.Name != want.name {
			t.Fatalf("element %d: got Name=%q, want %q", i, el.Name, want.name)
		}
		if el.IsItem != want.isItem || el.HasChildren != want.hasChildren {
			t.Fatalf("element %d (%s): got IsItem=%v HasChildren=%v, want %v/%v", i, want.name, el.IsItem, el.HasChildren, want.isItem, want.hasChildren)
		}
		if want.dataType == "" {
			if len(el.Properties) != 0 {
				t.Fatalf("element %d (%s): got %d properties, want 0 (branch)", i, want.name, len(el.Properties))
			}
			continue
		}
		if len(el.Properties) != 2 {
			t.Fatalf("element %d (%s): got %d properties, want 2 (dataType, accessRights)", i, want.name, len(el.Properties))
		}
		dt := el.Properties[0]
		if dt.Name != StandardPropertyName(PropDataType) {
			t.Fatalf("element %d (%s): properties[0] name=%+v, want dataType", i, want.name, dt.Name)
		}
		if dt.Value == nil {
			t.Fatalf("element %d (%s): dataType property has nil Value", i, want.name)
		}
		qn, err := dt.Value.QNameValue()
		if err != nil {
			t.Fatalf("element %d (%s): dataType QNameValue: %v", i, want.name, err)
		}
		if qn.Space != XSDNamespace || qn.Local != want.dataType {
			t.Fatalf("element %d (%s): got dataType=%+v, want {%s %s}", i, want.name, qn, XSDNamespace, want.dataType)
		}
		access := el.Properties[1]
		if access.Name != StandardPropertyName(PropAccessRights) {
			t.Fatalf("element %d (%s): properties[1] name=%+v, want accessRights", i, want.name, access.Name)
		}
		if access.Value == nil {
			t.Fatalf("element %d (%s): accessRights property has nil Value", i, want.name)
		}
		s, err := access.Value.String()
		if err != nil {
			t.Fatalf("element %d (%s): accessRights String: %v", i, want.name, err)
		}
		if s != "readable" {
			t.Fatalf("element %d (%s): got accessRights=%q, want readable", i, want.name, s)
		}
	}
}
