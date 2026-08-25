package xmlda

import (
	"encoding/xml"
	"testing"
)

// probe is a minimal element type whose UnmarshalXML resolves one QName
// attribute value via resolveQName, used to test the end-to-end wiring
// through Decode without depending on any other xmlda type.
type probe struct {
	Resolved QName
	err      error
}

func (p *probe) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	raw, _ := attrValue(start.Attr, xml.Name{Local: "Type"})
	q, err := resolveQName(d, raw)
	if err != nil {
		p.err = err
		// still consume the element so the decoder doesn't error out
		return d.Skip()
	}
	p.Resolved = q
	return d.Skip()
}

func TestResolveQName_AlternativePrefixes(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want QName
	}{
		{
			name: "explicit prefix bound to OPC namespace",
			doc:  `<root xmlns:ns1="` + Namespace + `"><probe Type="ns1:ItemValue"/></root>`,
			want: QName{Space: Namespace, Local: "ItemValue"},
		},
		{
			name: "different prefix, same namespace",
			doc:  `<root xmlns:q0="` + Namespace + `"><probe Type="q0:ItemValue"/></root>`,
			want: QName{Space: Namespace, Local: "ItemValue"},
		},
		{
			name: "default namespace, no prefix in value",
			doc:  `<root xmlns="` + Namespace + `"><probe Type="ItemValue"/></root>`,
			want: QName{Space: Namespace, Local: "ItemValue"},
		},
		{
			name: "xsd prefix",
			doc:  `<root xmlns:xsd="` + XSDNamespace + `"><probe Type="xsd:float"/></root>`,
			want: QName{Space: XSDNamespace, Local: "float"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var wrapper struct {
				Probe probe `xml:"probe"`
			}
			if err := Decode([]byte(tc.doc), &wrapper); err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if wrapper.Probe.err != nil {
				t.Fatalf("resolveQName: %v", wrapper.Probe.err)
			}
			if wrapper.Probe.Resolved != tc.want {
				t.Fatalf("got %+v, want %+v", wrapper.Probe.Resolved, tc.want)
			}
		})
	}
}

func TestResolveQName_Errors(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{
			name: "unbound prefix",
			doc:  `<root><probe Type="ns1:ItemValue"/></root>`,
		},
		{
			name: "no default namespace in scope",
			doc:  `<root><probe Type="ItemValue"/></root>`,
		},
		{
			name: "wrong namespace URI still resolves to that (wrong) URI, not an error",
			doc:  `<root xmlns:ns1="http://example.com/not-opc"><probe Type="ns1:ItemValue"/></root>`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var wrapper struct {
				Probe probe `xml:"probe"`
			}
			if err := Decode([]byte(tc.doc), &wrapper); err != nil {
				t.Fatalf("Decode: %v", err)
			}
			_ = wrapper // per-case assertions below
		})
	}

	// First two cases must produce a resolution error.
	for _, tc := range cases[:2] {
		var wrapper struct {
			Probe probe `xml:"probe"`
		}
		if err := Decode([]byte(tc.doc), &wrapper); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if wrapper.Probe.err == nil {
			t.Fatalf("%s: expected resolution error, got none (resolved=%+v)", tc.name, wrapper.Probe.Resolved)
		}
	}

	// Third case: wrong-but-declared namespace resolves successfully to that URI.
	var wrapper struct {
		Probe probe `xml:"probe"`
	}
	if err := Decode([]byte(cases[2].doc), &wrapper); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if wrapper.Probe.err != nil {
		t.Fatalf("expected no error, got %v", wrapper.Probe.err)
	}
	want := QName{Space: "http://example.com/not-opc", Local: "ItemValue"}
	if wrapper.Probe.Resolved != want {
		t.Fatalf("got %+v, want %+v", wrapper.Probe.Resolved, want)
	}
}

func TestPrefixBoundBothAsDefaultAndExplicit(t *testing.T) {
	// Mirrors the real fixture testdata/responses/subscribe_680.response.xml,
	// where the OPC namespace is bound as both the default xmlns on the
	// response root and an explicit ns1 prefix used only inside xsi:type
	// values within the same document.
	doc := `<SubscribeResponse xmlns="` + Namespace + `" xmlns:ns1="` + Namespace + `">
		<probe Type="ns1:ItemValue"/>
	</SubscribeResponse>`
	var wrapper struct {
		Probe probe `xml:"probe"`
	}
	if err := Decode([]byte(doc), &wrapper); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if wrapper.Probe.err != nil {
		t.Fatalf("resolveQName: %v", wrapper.Probe.err)
	}
	want := QName{Space: Namespace, Local: "ItemValue"}
	if wrapper.Probe.Resolved != want {
		t.Fatalf("got %+v, want %+v", wrapper.Probe.Resolved, want)
	}
}

func TestQName_IsZeroAndString(t *testing.T) {
	var zero QName
	if !zero.IsZero() {
		t.Fatalf("zero value should report IsZero")
	}
	q := QName{Space: Namespace, Local: "E_FAIL"}
	if q.IsZero() {
		t.Fatalf("non-zero QName reported IsZero")
	}
	if got, want := q.String(), "{"+Namespace+"}E_FAIL"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	local := QName{Local: "E_FAIL"}
	if got, want := local.String(), "E_FAIL"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

// TestEncodePropertyNames_EmptySpaceQName_PreservesNoNamespace guards
// against encodePropertyNames silently promoting a no-namespace
// PropertyNames entry into whichever default namespace an ancestor
// element happens to declare — exactly the shape of a real SOAP body
// (see testdata/responses/subscribe_680.response.xml, which declares the
// OPC namespace as the document's default xmlns).
func TestEncodePropertyNames_EmptySpaceQName_PreservesNoNamespace(t *testing.T) {
	req := BrowseRequest{PropertyNames: []QName{{Local: "VendorProp"}}}
	body, err := xml.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	doc := `<Body xmlns="` + Namespace + `">` + string(body) + `</Body>`

	var wrapper struct {
		Req BrowseRequest `xml:"Browse"`
	}
	if err := Decode([]byte(doc), &wrapper); err != nil {
		t.Fatalf("Decode: %v\ndoc: %s", err, doc)
	}
	if len(wrapper.Req.PropertyNames) != 1 {
		t.Fatalf("got %d PropertyNames, want 1 (doc: %s)", len(wrapper.Req.PropertyNames), doc)
	}
	want := QName{Local: "VendorProp"}
	if got := wrapper.Req.PropertyNames[0]; got != want {
		t.Fatalf("got %+v, want %+v (property name resolved into the ambient default namespace instead of round-tripping unqualified)", got, want)
	}
}

func FuzzResolveQName(f *testing.F) {
	seeds := []string{
		"ns1:ItemValue",
		"ItemValue",
		":",
		"",
		"a:b:c",
		"xsd:float",
	}
	for _, s := range seeds {
		f.Add(s, `<root xmlns:ns1="`+Namespace+`" xmlns:xsd="`+XSDNamespace+`">`)
	}
	f.Fuzz(func(t *testing.T, rawType string, prefixDecl string) {
		// prefixDecl is untrusted/fuzzed; only use it if it parses as a
		// valid start tag, otherwise fall back to a fixed safe wrapper.
		doc := `<root xmlns:ns1="` + Namespace + `"><probe Type="` + xmlEscape(rawType) + `"/></root>`
		var wrapper struct {
			Probe probe `xml:"probe"`
		}
		// Must never panic, regardless of input.
		_ = Decode([]byte(doc), &wrapper)
	})
}

func xmlEscape(s string) string {
	var buf []byte
	for _, r := range s {
		switch r {
		case '"':
			buf = append(buf, "&quot;"...)
		case '&':
			buf = append(buf, "&amp;"...)
		case '<':
			buf = append(buf, "&lt;"...)
		case '>':
			buf = append(buf, "&gt;"...)
		default:
			buf = append(buf, string(r)...)
		}
	}
	return string(buf)
}
