package server

import (
	"context"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock/clocktest"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// This file is the wire-format safety net the rest of the suite could not
// provide, because everything else verifies this server's output by
// decoding it with the same encoding/xml that produced it — and Go's
// decoder is lenient in exactly the places the specification is not.
//
// Two defects lived through the entire suite, both real-world captured
// fixtures, the Dockerized soak test and CI because of that:
//
//   - Duplicate xmlns declarations on any element carrying both an
//     xsi:type and a QName-valued attribute (every ItemValue with a
//     ResultID — the most common per-item error shape there is). XML
//     forbids duplicate attribute names; Go's decoder silently keeps the
//     first, expat/libxml2/.NET/JAXP reject the document.
//   - Response payload elements emitted in no namespace at all, where the
//     schema's elementFormDefault="qualified" requires the OPC XML-DA one.
//     Go matches struct fields by local name, so nothing noticed.
//
// The checks below are pure Go and run everywhere. They are deliberately
// structural rather than golden-text comparisons wherever possible, so
// they keep holding as the encoder changes. The golden files
// (testdata/golden/) capture exact bytes on top of that, and the CI
// "schema-validate" job runs xmllint against testdata/schema/ for the
// external-parser verdict this package cannot produce on its own.

var updateGolden = flag.Bool("update-golden", false,
	"rewrite testdata/golden/*.response.xml from the current encoder output")

// goldenTime is the fixed instant every golden response is generated at,
// so the files are byte-stable across runs.
var goldenTime = time.Date(2026, 3, 4, 9, 30, 0, 0, time.UTC)

// checkWellFormed walks doc as a token stream and enforces the invariants
// a conforming XML parser enforces, plus the two the OPC XML-DA schema
// adds. It returns every violation found rather than the first, so one
// run reports the whole picture.
func checkWellFormed(t *testing.T, doc []byte) {
	t.Helper()

	d := xml.NewDecoder(strings.NewReader(string(doc)))
	var path []string
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tokenizing response: %v\ndoc: %s", err, doc)
		}
		switch tk := tok.(type) {
		case xml.StartElement:
			path = append(path, tk.Name.Local)
			where := strings.Join(path, "/")

			// Duplicate attribute names. Go's decoder does not reject
			// these, so this is checked by hand against the raw
			// attribute list — the exact defect that shipped.
			seen := make(map[string]string, len(tk.Attr))
			for _, a := range tk.Attr {
				// Go resolves xmlns:foo to {xmlns}foo and xmlns to
				// {}xmlns; both must be compared under the name that
				// actually appears in the document.
				name := a.Name.Local
				if a.Name.Space != "" {
					name = a.Name.Space + ":" + a.Name.Local
				}
				if prev, dup := seen[name]; dup {
					t.Errorf("%s: duplicate attribute %q (values %q and %q) — "+
						"XML forbids this and every parser but Go's rejects the document",
						where, name, prev, a.Value)
				}
				seen[name] = a.Value
			}

			// Namespace membership. Everything inside the SOAP Body is
			// OPC XML-DA payload and must be in that namespace
			// (elementFormDefault="qualified"), except a SOAP Fault and
			// its unqualified children.
			if len(path) > 2 && path[0] == "Envelope" && path[1] == "Body" && !inFault(path) {
				if tk.Name.Space != xmlda.Namespace {
					t.Errorf("%s: element is in namespace %q, want %q",
						where, tk.Name.Space, xmlda.Namespace)
				}
			}
		case xml.EndElement:
			if len(path) > 0 {
				path = path[:len(path)-1]
			}
		}
	}
}

// inFault reports whether path is inside a SOAP Fault element, whose own
// children (faultcode/faultstring/detail) are unqualified in SOAP 1.1.
func inFault(path []string) bool {
	return slices.Contains(path, "Fault")
}

// checkItemValueChildOrder enforces the schema's xsd:sequence for
// ItemValue — DiagnosticInfo, then Value, then Quality (§3.1.5). A
// sequence is ordered, so any other order is invalid for a
// schema-validating client, and the previous encoder emitted Quality
// before Value with DiagnosticInfo as an attribute entirely.
func checkItemValueChildOrder(t *testing.T, doc []byte) {
	t.Helper()
	const (
		diag = 1
		val  = 2
		qual = 3
	)
	rank := map[string]int{"DiagnosticInfo": diag, "Value": val, "Quality": qual}

	d := xml.NewDecoder(strings.NewReader(string(doc)))
	// depth of the ItemValue-shaped element currently open, and the
	// highest child rank seen inside it so far.
	type frame struct {
		depth int
		last  int
	}
	var open []frame
	depth := 0
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tokenizing response: %v", err)
		}
		switch tk := tok.(type) {
		case xml.StartElement:
			depth++
			if len(open) > 0 && open[len(open)-1].depth == depth-1 {
				if r, ok := rank[tk.Name.Local]; ok {
					f := &open[len(open)-1]
					if r < f.last {
						t.Errorf("ItemValue child <%s> appears after a later-ranked sibling; "+
							"the schema's sequence is DiagnosticInfo, Value, Quality", tk.Name.Local)
					}
					f.last = r
				}
			}
			// "Items" (Read/Write/PolledRefresh) and "ItemValue"
			// (Subscribe's wrapper) are the two element names that carry
			// ItemValue content.
			if tk.Name.Local == "Items" || tk.Name.Local == "ItemValue" {
				open = append(open, frame{depth: depth})
			}
		case xml.EndElement:
			if len(open) > 0 && open[len(open)-1].depth == depth {
				open = open[:len(open)-1]
			}
			depth--
		}
	}
}

// checkValueTypes asserts every <Value> element declares an xsi:type this
// library recognizes, in the namespace the specification puts it in:
// scalars in the XSD namespace, ArrayOf<X> in the OPC XML-DA one. It is
// the check the schema itself cannot make (the schema declares Value as
// untyped anyType).
func checkValueTypes(t *testing.T, doc []byte) {
	t.Helper()
	// Prefix bindings are collected as the document is walked, so a
	// declaration on an ANCESTOR counts — which is where the response
	// writer now puts them, once on the Envelope rather than once per
	// element. Requiring the declaration on the element itself was the
	// right check while every element carried its own; it is not a
	// property XML requires, and holding on to it would have pinned 62 %
	// of a large response's bytes in place.
	scope := map[string]string{}
	depth := 0
	declaredAt := map[string][]int{}
	d := xml.NewDecoder(strings.NewReader(string(doc)))
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tokenizing response: %v", err)
		}
		if _, ok := tok.(xml.EndElement); ok {
			for prefix, depths := range declaredAt {
				if n := len(depths); n > 0 && depths[n-1] == depth {
					declaredAt[prefix] = depths[:n-1]
					if len(declaredAt[prefix]) == 0 {
						delete(scope, prefix)
					}
				}
			}
			depth--
			continue
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		depth++
		for _, a := range se.Attr {
			if a.Name.Space == "xmlns" {
				scope[a.Name.Local] = a.Value
				declaredAt[a.Name.Local] = append(declaredAt[a.Name.Local], depth)
			}
		}
		if se.Name.Local != "Value" {
			continue
		}
		var raw string
		for _, a := range se.Attr {
			if a.Name.Space == xmlda.XSINamespace && a.Name.Local == "type" {
				raw = a.Value
			}
		}
		if raw == "" {
			t.Errorf("<Value> element has no xsi:type attribute")
			continue
		}
		prefix, local, hasPrefix := strings.Cut(raw, ":")
		if !hasPrefix {
			t.Errorf("<Value xsi:type=%q>: unprefixed type", raw)
			continue
		}
		// Resolvable from the element or any ancestor — the ordinary XML
		// scoping rule.
		uri := scope[prefix]
		switch uri {
		case xmlda.XSDNamespace, xmlda.Namespace:
			if local == "" {
				t.Errorf("<Value xsi:type=%q>: empty local name", raw)
			}
		case "":
			t.Errorf("<Value xsi:type=%q>: prefix %q is not declared on this element or any ancestor", raw, prefix)
		default:
			t.Errorf("<Value xsi:type=%q>: unexpected namespace %q", raw, uri)
		}
	}
}

// wireCase is one recorded request/response pair.
type wireCase struct {
	name string
	body string
}

// newWireHandler builds a handler over a backend rich enough to exercise
// every shape that has to survive encoding: a scalar value, an ARRAY
// value (the shape no example backend used, which is how the missing
// xmlda.NewArrayValue went unnoticed), a per-item E_ code, a per-item S_
// success code, diagnostic text, a property with no value at all, and a
// vendor property in its own namespace.
func newWireHandler(t *testing.T) (*Handler, *clocktest.Fake) {
	t.Helper()
	clk := clocktest.New(goldenTime)

	status := newTestStatus()
	status.SetLocales([]string{"en-US", "de-DE"})

	reader := &wireReader{results: map[backend.ItemRef]backend.Result[backend.ItemSample]{
		{ItemName: "Scalar"}: {Value: backend.ItemSample{
			Value:     xmlda.NewFloat64(21.5),
			Quality:   xmlda.NewGoodQuality(),
			Timestamp: goldenTime,
		}},
		{ItemName: "Array"}: {Value: backend.ItemSample{
			Value:     xmlda.NewArrayValue(xmlda.NewFloat64Array([]float64{1.5, -2, 3.25})),
			Quality:   xmlda.NewGoodQuality(),
			Timestamp: goldenTime,
		}},
		// A success-with-caveat code that still carries a value — the
		// combination Read used to drop the value for.
		{ItemName: "Clamped"}: {
			ResultID: xmlda.SuccessClamp,
			Value: backend.ItemSample{
				Value:     xmlda.NewInt32(100),
				Quality:   xmlda.NewQuality(xmlda.QualityUncertainEUExceeded, xmlda.LimitHigh, 0),
				Timestamp: goldenTime,
			},
		},
		{ItemName: "Unknown"}: {
			ResultID:       xmlda.ErrUnknownItemName,
			DiagnosticInfo: "no such tag in the address space",
		},
	}}

	be := backend.Backend{
		Status:  status,
		Reader:  reader,
		Writer:  &wireWriter{},
		Browser: &testBrowser{result: goldenBrowseResult()},
		Properties: &testProperties{props: map[backend.ItemRef][]backend.Property{
			{ItemName: "Scalar"}: {
				{ID: xmlda.PropDataType, Value: xmlda.NewQNameValue(xmlda.QName{Space: xmlda.XSDNamespace, Local: "double"})},
				{ID: xmlda.PropEUInfo, Value: xmlda.NewArrayValue(xmlda.NewStringArray([]string{"OPEN", "CLOSE"}))},
				// No Value at all, plus a per-property condition: the
				// shape that used to collapse the whole response into an
				// E_FAIL fault once ReturnPropertyValues was set.
				{ID: xmlda.PropHighEU, ResultID: xmlda.ErrInvalidPID},
				{Name: "vendorThing", Namespace: "http://example.com/vendor",
					Value: xmlda.NewString("vendor value")},
			},
		}},
	}

	h := newTestHandler(t, be, Config{}, clk)
	return h, clk
}

// wireReader serves fixed per-item Results, so a case can pin an
// arbitrary ResultID/DiagnosticInfo alongside a value — which the shared
// testReader (value-only) cannot express.
type wireReader struct {
	results map[backend.ItemRef]backend.Result[backend.ItemSample]
}

func (r *wireReader) Read(_ context.Context, items []backend.ReadRequestItem) ([]backend.Result[backend.ItemSample], error) {
	out := make([]backend.Result[backend.ItemSample], len(items))
	for i, it := range items {
		res, ok := r.results[it.Ref]
		if !ok {
			out[i] = backend.Result[backend.ItemSample]{ResultID: xmlda.ErrUnknownItemName}
			continue
		}
		out[i] = res
	}
	return out, nil
}

// wireWriter reports a clamped write, so the Write response carries an
// S_CLAMP result alongside an echoed value.
type wireWriter struct{}

func (w *wireWriter) Write(_ context.Context, items []backend.WriteRequestItem) ([]backend.Result[backend.WriteOutcome], error) {
	out := make([]backend.Result[backend.WriteOutcome], len(items))
	for i := range items {
		clamped := xmlda.NewFloat64(40)
		q := xmlda.NewGoodQuality()
		ts := goldenTime
		out[i] = backend.Result[backend.WriteOutcome]{
			Value: backend.WriteOutcome{Clamped: true, Value: &clamped, Quality: &q, Timestamp: &ts},
		}
	}
	return out, nil
}

func goldenBrowseResult() backend.BrowseResult {
	return backend.BrowseResult{
		Elements: []backend.BrowseElement{
			{Name: "Plant", IsItem: false, HasChildren: true, Ref: &backend.ItemRef{ItemName: "Plant"}},
			{
				Name: "Scalar", IsItem: true, HasChildren: false,
				Ref: &backend.ItemRef{ItemName: "Scalar"},
				Properties: []backend.Property{
					{ID: xmlda.PropDescription, Value: xmlda.NewString("a scalar item")},
					{ID: xmlda.PropHighEU, ResultID: xmlda.ErrInvalidPID},
				},
			},
		},
	}
}

// wireCases returns one request per operation, chosen so the response
// carries the awkward shapes rather than the easy ones.
func wireCases(t *testing.T, h *Handler) []wireCase {
	t.Helper()
	subHandle := createGoldenSubscription(t, h)
	return []wireCase{
		{"getstatus", getStatusRequestBody("CRH-status")},
		{"read", soapEnvelopeOpen + `<Read xmlns="` + xmlda.Namespace + `">` +
			`<Options ClientRequestHandle="CRH-read" LocaleID="de-DE" ReturnItemName="true" ` +
			`ReturnItemPath="true" ReturnItemTime="true" ReturnDiagnosticInfo="true"/>` +
			`<ItemList><Items ItemName="Scalar"/><Items ItemName="Array"/>` +
			`<Items ItemName="Clamped"/><Items ItemName="Unknown"/></ItemList>` +
			`</Read>` + soapEnvelopeClose},
		{"write", writeRequestBody("Scalar", "double", "42.5", true)},
		{"browse", soapEnvelopeOpen + `<Browse xmlns="` + xmlda.Namespace + `" ` +
			`ClientRequestHandle="CRH-browse" ReturnAllProperties="true" ReturnPropertyValues="true"/>` +
			soapEnvelopeClose},
		{"getproperties", soapEnvelopeOpen + `<GetProperties xmlns="` + xmlda.Namespace + `" ` +
			`ClientRequestHandle="CRH-props" ReturnAllProperties="true" ReturnPropertyValues="true">` +
			`<ItemIDs ItemName="Scalar"/><ItemIDs ItemName="Missing"/></GetProperties>` + soapEnvelopeClose},
		{"subscribe", subscribeRequestBody("Array", "CIH-array", true)},
		{"subscriptionpolledrefresh", soapEnvelopeOpen +
			`<SubscriptionPolledRefresh xmlns="` + xmlda.Namespace + `" ReturnAllItems="true" WaitTime="0">` +
			`<Options ClientRequestHandle="CRH-refresh" ReturnItemName="true" ReturnItemTime="true"/>` +
			`<ServerSubHandles>` + subHandle + `</ServerSubHandles>` +
			`</SubscriptionPolledRefresh>` + soapEnvelopeClose},
		{"subscriptioncancel", soapEnvelopeOpen +
			`<SubscriptionCancel xmlns="` + xmlda.Namespace + `" ServerSubHandle="` + subHandle + `" ` +
			`ClientRequestHandle="CRH-cancel"/>` + soapEnvelopeClose},
		// A fault, so the Fault shape is covered by the same checks.
		{"fault_unsupported_operation", soapEnvelopeOpen +
			`<NotAnOperation xmlns="` + xmlda.Namespace + `"/>` + soapEnvelopeClose},
	}
}

// createGoldenSubscription creates the subscription the polled-refresh and
// cancel cases operate on, returning its handle.
func createGoldenSubscription(t *testing.T, h *Handler) string {
	t.Helper()
	resp := decodeResponse[xmlda.SubscribeResponse](t,
		postSOAP(t, h, subscribeRequestBody("Scalar", "CIH-scalar", true)))
	if resp.ServerSubHandle == "" {
		t.Fatal("setup: Subscribe returned no ServerSubHandle")
	}
	return resp.ServerSubHandle
}

// serverSubHandlePattern matches a server-issued subscription handle
// where one actually appears: as the value of ServerSubHandle,
// SubscriptionHandle or InvalidServerSubHandles. The handle itself is 32
// lowercase hex characters (subscription.newHandle, 16 random bytes).
//
// Anchoring on the surrounding attribute or element rather than on the
// hex shape alone matters: a bare 32-hex pattern would also rewrite a
// legitimate item value that happened to look like one — an MD5 sum, a
// device serial — and silently make a real wire difference invisible in
// the golden comparison.
var serverSubHandlePattern = regexp.MustCompile(
	`(ServerSubHandle=")[0-9a-f]{32}(")|` +
		`(SubscriptionHandle=")[0-9a-f]{32}(")|` +
		`(<InvalidServerSubHandles>)[0-9a-f]{32}(</InvalidServerSubHandles>)`)

// normalizeGolden replaces the one thing in a response that legitimately
// differs per run — the server-issued subscription handle, which is
// random by design — with a fixed placeholder, so golden comparison
// stays meaningful.
//
// Matching by shape rather than by a handle the caller happens to know
// is deliberate: the Subscribe case mints its own handle, so threading
// known values through would silently miss it and leave that golden file
// failing on every run for a reason that has nothing to do with the wire
// format.
func normalizeGolden(doc []byte) []byte {
	return serverSubHandlePattern.ReplaceAll(doc, []byte("${1}${3}${5}SERVER-SUB-HANDLE${2}${4}${6}"))
}

// TestResponsesAreWellFormed is the regression test for the two defects
// described at the top of this file, run across every operation.
func TestResponsesAreWellFormed(t *testing.T) {
	h, _ := newWireHandler(t)
	for _, tc := range wireCases(t, h) {
		t.Run(tc.name, func(t *testing.T) {
			resp := postSOAP(t, h, tc.body)
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("reading response: %v", err)
			}
			checkWellFormed(t, body)
			checkItemValueChildOrder(t, body)
			checkValueTypes(t, body)
			if t.Failed() {
				t.Logf("response was:\n%s", body)
			}
		})
	}
}

// TestGoldenResponses pins the exact bytes of each operation's response.
// Regenerate with:
//
//	go test ./server -run TestGoldenResponses -update-golden
//
// A diff here is not automatically a failure of this test — it means the
// wire format changed, and the reviewer has to decide whether that change
// was intended. That is the point: the previous suite had no artifact in
// which a wire-format change was visible at all.
func TestGoldenResponses(t *testing.T) {
	h, _ := newWireHandler(t)
	cases := wireCases(t, h)

	dir := filepath.Join("..", "testdata", "golden")
	if *updateGolden {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postSOAP(t, h, tc.body)
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("reading response: %v", err)
			}
			got := normalizeGolden(body)
			path := filepath.Join(dir, tc.name+".response.xml")
			if *updateGolden {
				if err := os.WriteFile(path, append(got, '\n'), 0o644); err != nil {
					t.Fatalf("writing %s: %v", path, err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v (regenerate with -update-golden)", path, err)
			}
			if strings.TrimRight(string(want), "\n") != string(got) {
				t.Errorf("wire format changed.\n--- want (%s) ---\n%s\n--- got ---\n%s",
					path, want, got)
			}
		})
	}
}

// TestNoDuplicateAttributes is the narrow, always-on invariant behind
// TestResponsesAreWellFormed's broader sweep, stated directly against the
// marshalers rather than through the HTTP handler: every element every
// encoder in xmlda produces must have unique attribute names, whatever
// combination of xsi:type and QName-valued attributes it carries.
//
// It exists as its own test because that invariant is easy to break in a
// single marshaler without noticing — the merge that enforces it
// (xmlda.mergeAttrs) has to be used at every append site, and a new one
// added later would silently reintroduce the defect.
func TestNoDuplicateAttributes(t *testing.T) {
	ts := goldenTime
	arr := xmlda.NewArrayValue(xmlda.NewInt32Array([]int32{1, 2}))
	scalar := xmlda.NewFloat64(1.5)
	vendorQName := xmlda.NewQNameValue(xmlda.QName{Space: xmlda.Namespace, Local: "dataType"})
	xsdQName := xmlda.NewQNameValue(xmlda.QName{Space: xmlda.XSDNamespace, Local: "int"})
	bareQName := xmlda.NewQNameValue(xmlda.QName{Local: "unqualified"})

	cases := []struct {
		name string
		v    any
	}{
		// xsi:type (opc) + ResultID (opc): the shape that shipped broken.
		{"itemvalue_with_resultid", xmlda.ItemValue{
			ItemName: "A", ResultID: xmlda.ErrUnknownItemName, Quality: ptr(xmlda.NewGoodQuality()),
		}},
		{"itemvalue_full", xmlda.ItemValue{
			ItemName: "A", ItemPath: strPtr("Path"), ClientItemHandle: "H",
			Value: &arr, Quality: ptr(xmlda.NewQuality(xmlda.QualityUncertain, xmlda.LimitHigh, 3)),
			Timestamp: &ts, ResultID: xmlda.SuccessClamp, DiagnosticInfo: ptr("diag"),
		}},
		// Name (opc) + ResultID (opc).
		{"itemproperty_standard_with_resultid", xmlda.ItemProperty{
			Name: xmlda.StandardPropertyName(xmlda.PropDataType), ResultID: xmlda.ErrInvalidPID,
		}},
		// Both QNames unqualified: two xmlns="" declarations.
		{"itemproperty_vendor_unqualified", xmlda.ItemProperty{
			Name:     xmlda.QName{Local: "vendorProp"},
			ResultID: xmlda.ErrorCode{QName: xmlda.QName{Local: "E_VENDOR"}},
		}},
		// TWO DIFFERENT vendor namespaces on one element. Every
		// non-standard namespace shares the conventional prefix "ext", so
		// without prefixIn's numbered fallback this emits xmlns:ext twice
		// with different URIs — the same unparseable output the original
		// duplicate-attribute defect produced. It is spec-sanctioned
		// input: §3.1.9 requires a vendor result code to be namespaced and
		// §3.1.10 requires the same of a vendor property, and nothing says
		// they must be the same vendor.
		{"itemproperty_two_vendor_namespaces", xmlda.ItemProperty{
			Name:     xmlda.QName{Space: "http://vendor-a.example/props", Local: "myProp"},
			ResultID: xmlda.ErrorCode{QName: xmlda.QName{Space: "http://vendor-b.example/codes", Local: "E_VENDOR"}},
		}},
		// Three, to prove the fallback keeps counting rather than
		// colliding again on the second retry.
		{"itemproperty_three_vendor_namespaces", xmlda.ItemProperty{
			Name:     xmlda.QName{Space: "http://vendor-a.example/props", Local: "myProp"},
			ResultID: xmlda.ErrorCode{QName: xmlda.QName{Space: "http://vendor-b.example/codes", Local: "E_VENDOR"}},
			Value:    ptr(xmlda.NewQNameValue(xmlda.QName{Space: "http://vendor-c.example/types", Local: "someType"})),
		}},
		// An ItemValue whose xsi:type is OPC and whose ResultID is a
		// vendor code: two namespaces, two distinct conventional prefixes,
		// so no renaming should occur.
		{"itemvalue_vendor_resultid", xmlda.ItemValue{
			ItemName: "A", Quality: ptr(xmlda.NewGoodQuality()),
			ResultID: xmlda.ErrorCode{QName: xmlda.QName{Space: "http://vendor-a.example/codes", Local: "E_VENDOR"}},
		}},
		{"itemproperty_with_value", xmlda.ItemProperty{
			Name: xmlda.StandardPropertyName(xmlda.PropValue), Value: &scalar,
		}},
		// xsi:type is xsd:QName and the value is also XSD-namespaced:
		// two xmlns:xsd declarations.
		{"qname_value_xsd", &xsdQName},
		// xsi:type is xsd:QName, value in the OPC namespace.
		{"qname_value_opc", &vendorQName},
		// xsi:type xsd:QName plus an xmlns="" reset for the bare value.
		{"qname_value_bare", &bareQName},
		{"reply_base", xmlda.ReplyBase{
			RcvTime: ts, ReplyTime: ts, ClientRequestHandle: "H",
			RevisedLocaleID: "en-US", ServerState: xmlda.ServerStateRunning,
		}},
		{"quality", xmlda.NewQuality(xmlda.QualityBadCommFailure, xmlda.LimitLow, 7)},
		{"opcerror", xmlda.DedupeErrors([]xmlda.ErrorCode{xmlda.ErrTimedOut}, xmlda.StandardErrorText)},
		{"array_value", &arr},
		{"nil_value", ptr(xmlda.NewNil(xmlda.QName{Space: xmlda.Namespace, Local: "ArrayOfDouble"}))},
		{"subscribe_item_value", xmlda.SubscribeItemValue{
			RevisedSamplingRate: 1000,
			ItemValue: xmlda.ItemValue{
				ItemName: "A", ResultID: xmlda.SuccessUnsupportedRate, Quality: ptr(xmlda.NewGoodQuality()),
			},
		}},
		{"property_reply_list", xmlda.PropertyReplyList{
			ItemName: "A", ResultID: xmlda.ErrUnknownItemName,
			Properties: []xmlda.ItemProperty{{
				Name: xmlda.StandardPropertyName(xmlda.PropQuality), ResultID: xmlda.ErrInvalidPID,
			}},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := xml.Marshal(struct {
				XMLName xml.Name `xml:"Root"`
				V       any
			}{V: tc.v})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			assertUniqueAttrs(t, out)
		})
	}
}

func assertUniqueAttrs(t *testing.T, doc []byte) {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(string(doc)))
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("tokenizing %s: %v", doc, err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		seen := make(map[string]string, len(se.Attr))
		for _, a := range se.Attr {
			name := a.Name.Local
			if a.Name.Space != "" {
				name = a.Name.Space + ":" + a.Name.Local
			}
			if prev, dup := seen[name]; dup {
				var names []string
				for _, x := range se.Attr {
					names = append(names, fmt.Sprintf("%s=%q", x.Name.Local, x.Value))
				}
				sort.Strings(names)
				t.Fatalf("<%s>: duplicate attribute %q (%q vs %q)\nattributes: %s\ndoc: %s",
					se.Name.Local, name, prev, a.Value, strings.Join(names, " "), doc)
			}
			seen[name] = a.Value
		}
	}
}

func strPtr(s string) *string { return &s }

func ptr[T any](v T) *T { return &v }
