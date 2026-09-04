package soap

import (
	"encoding/xml"
	"strings"
	"testing"
)

// TestFault_RealFixtures decodes the three real captured SOAP fault
// documents under testdata/faults/ and checks each normalizes to the
// documented shape (see docs/specification/specification-analysis.md
// §11 and docs/architecture/decisions/004-namespace-processing.md).
func TestFault_RealFixtures(t *testing.T) {
	t.Run("legacy unqualified SOAP 1.1 (E_NOSUBSCRIPTION)", func(t *testing.T) {
		doc := readFixture(t, "fault_legacy_unqualified_e_nosubscription.response.xml")
		var env Envelope[dummyContent]
		if err := xml.Unmarshal(doc, &env); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if env.Body.Fault == nil {
			t.Fatalf("expected a Fault")
		}
		f := env.Body.Fault
		// Unqualified literal text: Space left empty, Local holds the
		// whole raw string, per the documented lenient fallback.
		if f.Code.Space != "" {
			t.Fatalf("expected empty Code.Space (unqualified), got %q", f.Code.Space)
		}
		if f.Code.Local != "E_NOSUBSCRIPTION" {
			t.Fatalf("got Code.Local %q, want E_NOSUBSCRIPTION", f.Code.Local)
		}
		if f.Text != "E_NOSUBSCRIPTION" {
			t.Fatalf("got Text %q, want E_NOSUBSCRIPTION", f.Text)
		}
		if f.Detail != "E_NOSUBSCRIPTION" {
			t.Fatalf("got Detail %q, want E_NOSUBSCRIPTION", f.Detail)
		}
	})

	t.Run("generic SOAP 1.1 parse error (no OPC content)", func(t *testing.T) {
		doc := readFixture(t, "fault_soap11_xml_syntax_error.response.xml")
		var env Envelope[dummyContent]
		if err := xml.Unmarshal(doc, &env); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if env.Body.Fault == nil {
			t.Fatalf("expected a Fault")
		}
		f := env.Body.Fault
		// "SOAP-ENV:Client" — prefix "SOAP-ENV" is declared on the
		// envelope root, not locally on faultcode, so per OQ-13 this
		// resolves via the lenient fallback: Space empty, Local holds
		// the whole raw text.
		if f.Code.Local != "SOAP-ENV:Client" {
			t.Fatalf("got Code.Local %q, want the literal unresolved text %q", f.Code.Local, "SOAP-ENV:Client")
		}
		if f.Text != "XML syntax error" {
			t.Fatalf("got Text %q, want %q", f.Text, "XML syntax error")
		}
		if f.Detail != "" {
			t.Fatalf("expected no Detail, got %q", f.Detail)
		}
	})

	t.Run("SOAP 1.2 structured fault (invalid dateTime)", func(t *testing.T) {
		doc := readFixture(t, "fault_soap12_invalid_datetime.response.xml")
		var env Envelope[dummyContent]
		if err := xml.Unmarshal(doc, &env); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if env.Body.Fault == nil {
			t.Fatalf("expected a Fault")
		}
		f := env.Body.Fault
		// "soap:Sender" — prefix declared on the envelope root, not
		// locally on Code/Value — same OQ-13 fallback.
		if f.Code.Local != "soap:Sender" {
			t.Fatalf("got Code.Local %q, want %q", f.Code.Local, "soap:Sender")
		}
		wantSubstr := "not a valid AllXsd value"
		if !strings.Contains(f.Text, wantSubstr) {
			t.Fatalf("got Text %q, want it to contain %q", f.Text, wantSubstr)
		}
		if f.Detail != "" {
			t.Fatalf("expected empty Detail (soap:Detail was empty), got %q", f.Detail)
		}
	})
}

func TestFault_SpecConformantExample(t *testing.T) {
	// Synthesized from the specification's own worked example (§2.6,
	// p.21): xmlns:q0 declared locally on <faultcode> itself.
	doc := []byte(`<SOAP-ENV:Envelope xmlns:SOAP-ENV="` + NS11 + `"><SOAP-ENV:Body><SOAP-ENV:Fault>` +
		`<faultcode xmlns:q0="http://opcfoundation.org/webservices/XMLDA/1.0/">q0:E_SERVERSTATE</faultcode>` +
		`<faultstring>The operation could not complete due to an abnormal server state.</faultstring>` +
		`<detail/>` +
		`</SOAP-ENV:Fault></SOAP-ENV:Body></SOAP-ENV:Envelope>`)
	var env Envelope[dummyContent]
	if err := xml.Unmarshal(doc, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	f := env.Body.Fault
	if f == nil {
		t.Fatalf("expected a Fault")
	}
	want := QName{Space: "http://opcfoundation.org/webservices/XMLDA/1.0/", Local: "E_SERVERSTATE"}
	if f.Code != want {
		t.Fatalf("got %+v, want %+v (locally-declared xmlns:q0 must resolve)", f.Code, want)
	}
}

// TestFault_StructuredDetail decodes a <detail> containing child elements
// — an ordinary, spec-legal SOAP fault-detail shape (the specification's
// own <detail> is meant to carry application-defined elements) — and
// checks the content is preserved rather than silently dropped to "".
func TestFault_StructuredDetail(t *testing.T) {
	doc := []byte(`<SOAP-ENV:Envelope xmlns:SOAP-ENV="` + NS11 + `"><SOAP-ENV:Body><SOAP-ENV:Fault>` +
		`<faultcode>SOAP-ENV:Server</faultcode>` +
		`<faultstring>disk full</faultstring>` +
		`<detail><MyAppException code="7"><msg>disk full</msg></MyAppException></detail>` +
		`</SOAP-ENV:Fault></SOAP-ENV:Body></SOAP-ENV:Envelope>`)
	var env Envelope[dummyContent]
	if err := xml.Unmarshal(doc, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	f := env.Body.Fault
	if f == nil {
		t.Fatalf("expected a Fault")
	}
	if f.Detail == "" {
		t.Fatalf("expected structured <detail> content to be preserved, got empty Detail")
	}
	if !strings.Contains(f.Detail, "MyAppException") || !strings.Contains(f.Detail, "disk full") {
		t.Fatalf("got Detail %q, want it to contain the structured child element's content", f.Detail)
	}
}

// TestFault_SOAP12_Subcode reproduces the gap where <Code><Subcode> was
// silently discarded by the generic d.Skip() branch: <Code>'s own direct
// <Value> is constrained by the SOAP 1.2 spec to one of five generic
// values ("soap:Sender" here), while the application-specific code a
// server actually wants to convey lives in <Subcode><Value> — a shape real
// .NET/WCF-based OPC XML-DA servers commonly emit.
func TestFault_SOAP12_Subcode(t *testing.T) {
	doc := []byte(`<soap:Envelope xmlns:soap="` + NS12 + `"><soap:Body><soap:Fault>` +
		`<soap:Code><soap:Value>soap:Sender</soap:Value>` +
		`<soap:Subcode><soap:Value>m:E_INVALIDHANDLE</soap:Value></soap:Subcode>` +
		`</soap:Code>` +
		`<soap:Reason><soap:Text xml:lang="en">bad handle</soap:Text></soap:Reason>` +
		`</soap:Fault></soap:Body></soap:Envelope>`)
	var env Envelope[dummyContent]
	if err := xml.Unmarshal(doc, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	f := env.Body.Fault
	if f == nil {
		t.Fatalf("expected a Fault")
	}
	if f.Code.Local != "m:E_INVALIDHANDLE" {
		t.Fatalf("got Code.Local %q, want the Subcode's Value %q (Code's own generic Value must not win)", f.Code.Local, "m:E_INVALIDHANDLE")
	}
}

// TestFault_SOAP12_NestedSubcode verifies that the deepest of several
// nested Subcodes wins, and that it resolves via a namespace declared
// locally on it (same pattern as TestFault_SpecConformantExample).
func TestFault_SOAP12_NestedSubcode(t *testing.T) {
	doc := []byte(`<soap:Envelope xmlns:soap="` + NS12 + `"><soap:Body><soap:Fault>` +
		`<soap:Code><soap:Value>soap:Receiver</soap:Value>` +
		`<soap:Subcode><soap:Value>m:Outer</soap:Value>` +
		`<soap:Subcode><soap:Value xmlns:m="http://example.com/">m:Inner</soap:Value></soap:Subcode>` +
		`</soap:Subcode>` +
		`</soap:Code>` +
		`<soap:Reason><soap:Text>deep</soap:Text></soap:Reason>` +
		`</soap:Fault></soap:Body></soap:Envelope>`)
	var env Envelope[dummyContent]
	if err := xml.Unmarshal(doc, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	f := env.Body.Fault
	if f == nil {
		t.Fatalf("expected a Fault")
	}
	want := QName{Space: "http://example.com/", Local: "Inner"}
	if f.Code != want {
		t.Fatalf("got %+v, want %+v (the deepest nested Subcode must win)", f.Code, want)
	}
}

func TestFault_MarshalAlwaysSOAP11QNameQualified(t *testing.T) {
	f := Fault{Code: QName{Space: "http://opcfoundation.org/webservices/XMLDA/1.0/", Local: "E_SERVERSTATE"}, Text: "bad state"}
	out, err := xml.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Fault
	if err := xml.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\ndoc: %s", err, out)
	}
	if got.Code != f.Code {
		t.Fatalf("got %+v, want %+v", got.Code, f.Code)
	}
	if got.Text != f.Text {
		t.Fatalf("got %q, want %q", got.Text, f.Text)
	}
}

func TestFault_ErrorInterface(t *testing.T) {
	f := &Fault{Code: QName{Space: "ns", Local: "E_FAIL"}, Text: "boom"}
	var err error = f
	if err.Error() == "" {
		t.Fatalf("expected non-empty error message")
	}
	plain := &Fault{Text: "boom"}
	if plain.Error() == "" {
		t.Fatalf("expected non-empty error message even with zero Code")
	}
}

// TestFault_ErrorInterface_NilFault is a regression test: a nil *Fault
// held in an error interface is the classic typed-nil trap — calling
// Error() on it must not panic.
func TestFault_ErrorInterface_NilFault(t *testing.T) {
	var f *Fault
	var err error = f
	if got := err.Error(); got == "" {
		t.Fatalf("expected a non-empty placeholder message, got %q", got)
	}
}

// TestFault_StructuredDetail_RoundTripsThroughMarshal is a regression test
// for a Detail corruption bug: readDetail captures <detail>'s structured
// child-element content verbatim via ,innerxml, but MarshalXML previously
// re-encoded that string through the ordinary chardata path, which
// XML-escapes it into unusable text on the way back out. A
// decode->encode->decode cycle must preserve the structured content.
func TestFault_StructuredDetail_RoundTripsThroughMarshal(t *testing.T) {
	f := &Fault{
		Code:   QName{Space: "ns", Local: "E_FAIL"},
		Text:   "disk full",
		Detail: `<MyAppException code="7"><msg>disk full</msg></MyAppException>`,
	}
	out, err := xml.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "&lt;MyAppException") {
		t.Fatalf("Detail's structured markup was XML-escaped on marshal instead of written verbatim: %s", out)
	}

	var got Fault
	if err := xml.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\ndoc: %s", err, out)
	}
	if !strings.Contains(got.Detail, "<MyAppException") || !strings.Contains(got.Detail, "disk full") {
		t.Fatalf("got Detail %q after round trip, want the structured child element preserved", got.Detail)
	}
}

// TestFault_StructuredDetail_SOAP12 mirrors TestFault_StructuredDetail but
// for SOAP 1.2's capitalized <Detail> element with real content, so both
// spellings of the structured-detail case are exercised symmetrically.
func TestFault_StructuredDetail_SOAP12(t *testing.T) {
	doc := []byte(`<soap:Envelope xmlns:soap="` + NS12 + `"><soap:Body><soap:Fault>` +
		`<soap:Code><soap:Value>soap:Sender</soap:Value></soap:Code>` +
		`<soap:Reason><soap:Text>bad request</soap:Text></soap:Reason>` +
		`<soap:Detail><MyAppException code="7"><msg>bad request</msg></MyAppException></soap:Detail>` +
		`</soap:Fault></soap:Body></soap:Envelope>`)
	var env Envelope[dummyContent]
	if err := xml.Unmarshal(doc, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	f := env.Body.Fault
	if f == nil {
		t.Fatalf("expected a Fault")
	}
	if !strings.Contains(f.Detail, "MyAppException") || !strings.Contains(f.Detail, "bad request") {
		t.Fatalf("got Detail %q, want it to contain the structured child element's content", f.Detail)
	}
}

// TestFault_CodeNamespaceDeclaredInBothScopes pins a deliberate
// redundancy: the prefix binding for a qualified fault code is written
// both on the envelope and on the element carrying the code.
//
// Two kinds of reader each miss one of those scopes. This package's own
// fault decoder resolves prefixes element-locally, because soap must not
// depend on xmlda's whole-document prefix scan (open-questions.md
// OQ-13) — drop the local binding and the library can no longer read
// the faults it writes. A parser built on a namespace-normalizing DOM
// has the opposite blind spot: it resolves content QNames against the
// scope it entered the element with, so a binding declared ON that
// element is invisible. mlabs-haskell/opc-xml-da-client is the second
// kind, and reported "Namespace not found: q0" until the envelope
// carried the binding too.
//
// Both bindings name the same URI, so the QName is identical either way
// and no conforming parser can see a conflict.
func TestFault_CodeNamespaceDeclaredInBothScopes(t *testing.T) {
	const ns = "http://opcfoundation.org/webservices/XMLDA/1.0/"

	for _, tc := range []struct {
		name    string
		version Version
		// element is the tag the code QName sits in for this version.
		element string
	}{
		{"soap 1.1", Version11, "faultcode"},
		{"soap 1.2", Version12, "SOAP-ENV:Value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &Fault{Code: QName{Space: ns, Local: "E_NOSUBSCRIPTION"}, Text: "gone"}
			env := Envelope[struct{}]{Version: tc.version, Body: Body[struct{}]{Fault: f}}
			out, err := xml.Marshal(env)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			got := string(out)

			decl := `xmlns:` + FaultCodePrefix + `="` + ns + `"`
			if n := strings.Count(got, decl); n != 2 {
				t.Errorf("found %d declarations of %s, want 2 (envelope and code element):\n%s", n, decl, got)
			}
			// One of them has to be on the envelope, before the Body.
			envEnd := strings.Index(got, "<SOAP-ENV:Body")
			if envEnd < 0 {
				t.Fatalf("no Body in output:\n%s", got)
			}
			if !strings.Contains(got[:envEnd], decl) {
				t.Errorf("envelope element does not declare %s:\n%s", decl, got[:envEnd])
			}
			// And one on the element carrying the code itself.
			if !strings.Contains(got, "<"+tc.element+" "+decl+">") {
				t.Errorf("<%s> does not declare %s locally:\n%s", tc.element, decl, got)
			}
			if !strings.Contains(got, ">"+FaultCodePrefix+":E_NOSUBSCRIPTION<") {
				t.Errorf("code is not written with the %q prefix:\n%s", FaultCodePrefix, got)
			}

			// The round trip is the point of keeping the local binding:
			// this package must still resolve its own fault codes.
			var back Envelope[struct{}]
			if err := xml.Unmarshal(out, &back); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if back.Body.Fault == nil {
				t.Fatal("decoded envelope carries no Fault")
			}
			if got := back.Body.Fault.Code; got.Space != ns || got.Local != "E_NOSUBSCRIPTION" {
				t.Errorf("decoded Code = %v, want {%s E_NOSUBSCRIPTION}", got, ns)
			}
		})
	}
}
