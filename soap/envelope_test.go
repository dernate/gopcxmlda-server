package soap

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dummyContent is a minimal successful-response type used to exercise
// Envelope[T]/Body[T] independent of any real xmlda operation struct
// (those are tested against soap.Envelope in the xmlda package once
// built, per docs/development/tasks.md WP-5).
type dummyContent struct {
	XMLName xml.Name `xml:"DummyResponse"`
	Foo     string   `xml:"Foo,attr"`
}

func TestEnvelope_ContentRoundTrip(t *testing.T) {
	env := Envelope[dummyContent]{Body: Body[dummyContent]{Content: &dummyContent{Foo: "bar"}}}
	out, err := xml.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Envelope[dummyContent]
	if err := xml.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\ndoc: %s", err, out)
	}
	if got.Body.Content == nil {
		t.Fatalf("expected non-nil Content")
	}
	if got.Body.Content.Foo != "bar" {
		t.Fatalf("got %q, want %q", got.Body.Content.Foo, "bar")
	}
	if got.Body.Fault != nil {
		t.Fatalf("expected nil Fault, got %+v", got.Body.Fault)
	}
}

func TestEnvelope_FaultRoundTrip(t *testing.T) {
	env := Envelope[dummyContent]{Body: Body[dummyContent]{Fault: &Fault{
		Code: QName{Space: "http://example.com/ns", Local: "E_FAIL"},
		Text: "something went wrong",
	}}}
	out, err := xml.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Envelope[dummyContent]
	if err := xml.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\ndoc: %s", err, out)
	}
	if got.Body.Content != nil {
		t.Fatalf("expected nil Content, got %+v", got.Body.Content)
	}
	if got.Body.Fault == nil {
		t.Fatalf("expected non-nil Fault")
	}
	if got.Body.Fault.Code.Local != "E_FAIL" || got.Body.Fault.Code.Space != "http://example.com/ns" {
		t.Fatalf("got %+v", got.Body.Fault.Code)
	}
	if got.Body.Fault.Text != "something went wrong" {
		t.Fatalf("got %q", got.Body.Fault.Text)
	}
}

func TestEnvelope_AlwaysEmitsSOAP11(t *testing.T) {
	env := Envelope[dummyContent]{Body: Body[dummyContent]{Content: &dummyContent{Foo: "x"}}}
	out, err := xml.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, NS11) {
		t.Fatalf("expected SOAP 1.1 namespace in output, got: %s", s)
	}
	if strings.Contains(s, NS12) {
		t.Fatalf("did not expect SOAP 1.2 namespace in this library's own output, got: %s", s)
	}
}

func TestEnvelope_RejectsNonEnvelopeRoot(t *testing.T) {
	doc := []byte(`<NotAnEnvelope/>`)
	var got Envelope[dummyContent]
	if err := xml.Unmarshal(doc, &got); err == nil {
		t.Fatalf("expected an error for a non-Envelope root element")
	}
}

func TestEnvelope_AcceptsSOAP12Namespace(t *testing.T) {
	doc := []byte(`<soap:Envelope xmlns:soap="` + NS12 + `"><soap:Body><DummyResponse Foo="hi"/></soap:Body></soap:Envelope>`)
	var got Envelope[dummyContent]
	if err := xml.Unmarshal(doc, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Body.Content == nil || got.Body.Content.Foo != "hi" {
		t.Fatalf("got %+v", got.Body.Content)
	}
}

func TestEnvelope_AlternativePrefixesDoNotMatter(t *testing.T) {
	docs := []string{
		`<SOAP-ENV:Envelope xmlns:SOAP-ENV="` + NS11 + `"><SOAP-ENV:Body><DummyResponse Foo="a"/></SOAP-ENV:Body></SOAP-ENV:Envelope>`,
		`<env:Envelope xmlns:env="` + NS11 + `"><env:Body><DummyResponse Foo="a"/></env:Body></env:Envelope>`,
		`<Envelope xmlns="` + NS11 + `"><Body><DummyResponse Foo="a"/></Body></Envelope>`,
	}
	for _, doc := range docs {
		var got Envelope[dummyContent]
		if err := xml.Unmarshal([]byte(doc), &got); err != nil {
			t.Fatalf("unmarshal %q: %v", doc, err)
		}
		if got.Body.Content == nil || got.Body.Content.Foo != "a" {
			t.Fatalf("doc %q: got %+v", doc, got.Body.Content)
		}
	}
}

func TestEnvelope_EmptyBody(t *testing.T) {
	doc := []byte(`<Envelope xmlns="` + NS11 + `"><Body></Body></Envelope>`)
	var got Envelope[dummyContent]
	if err := xml.Unmarshal(doc, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Body.Content != nil || got.Body.Fault != nil {
		t.Fatalf("expected an empty Body, got %+v", got.Body)
	}
}

// TestBody_UnmarshalRejectsSecondChildElement covers skipToEnd's error
// path: a Body must contain exactly one child element (a payload or a
// Fault, never both, and never more than one of either) — a second
// sibling element must be rejected, not silently ignored or merged.
func TestBody_UnmarshalRejectsSecondChildElement(t *testing.T) {
	doc := []byte(`<Envelope xmlns="` + NS11 + `"><Body>` +
		`<DummyResponse Foo="a"/><DummyResponse Foo="b"/>` +
		`</Body></Envelope>`)
	var got Envelope[dummyContent]
	err := xml.Unmarshal(doc, &got)
	if err == nil {
		t.Fatalf("expected an error for a Body with two child elements, got nil")
	}
	if !strings.Contains(err.Error(), "unexpected additional element") {
		t.Fatalf("got error %q, want it to mention the unexpected additional element", err)
	}
}

// TestBody_MarshalNilPointerContent_Errors covers a T instantiated as a
// pointer type: Content (*T = **dummyContent here) can be non-nil while
// pointing at a nil T. Silently marshaling that as an empty <Body></Body>
// would violate Body's own "payload or Fault, never neither" invariant
// with no signal anything was wrong; it must error instead.
func TestBody_MarshalNilPointerContent_Errors(t *testing.T) {
	var nilContent *dummyContent
	b := Body[*dummyContent]{Content: &nilContent}
	if _, err := xml.Marshal(b); err == nil {
		t.Fatalf("expected an error marshaling a Body whose Content points at a nil value, got none")
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "testdata", "faults", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}
