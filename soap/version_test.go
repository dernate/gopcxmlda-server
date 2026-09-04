package soap

import (
	"encoding/xml"
	"errors"
	"strings"
	"testing"
)

// The SOAP 1.2 half of this package is reached from the server layer, so
// it was covered only as a side effect of a handler test. These exercise
// it directly: the version mapping, the fault shape, and the header
// inspection that decides whether a request can be served at all.

func TestVersion_NSAndContentType(t *testing.T) {
	for _, tc := range []struct {
		v         Version
		ns        string
		mediaType string
	}{
		{Version11, NS11, "text/xml"},
		{Version12, NS12, "application/soap+xml"},
		{Version(99), NS11, "text/xml"}, // anything unknown is 1.1, the version OPC XML-DA is defined over
	} {
		if got := tc.v.NS(); got != tc.ns {
			t.Errorf("Version(%d).NS() = %q, want %q", tc.v, got, tc.ns)
		}
		if got := tc.v.ContentType(); !strings.HasPrefix(got, tc.mediaType) {
			t.Errorf("Version(%d).ContentType() = %q, want it to start with %q", tc.v, got, tc.mediaType)
		}
		if got := tc.v.ContentType(); !strings.Contains(got, "charset=utf-8") {
			t.Errorf("Version(%d).ContentType() = %q, want a charset", tc.v, got)
		}
	}
}

func TestVersionOf(t *testing.T) {
	for ns, want := range map[string]Version{
		NS11:                 Version11,
		NS12:                 Version12,
		"":                   Version11,
		"urn:something-else": Version11, // never rejected over its namespace (ADR-004)
	} {
		if got := VersionOf(ns); got != want {
			t.Errorf("VersionOf(%q) = %v, want %v", ns, got, want)
		}
	}
}

// TestFault_MarshalsInBothVersions pins that the two fault shapes are
// genuinely different documents, not the same one with a different
// namespace. A SOAP 1.2 stack handed 1.1's faultcode/faultstring discards
// the reply — losing the very error code the fault existed to convey.
func TestFault_MarshalsInBothVersions(t *testing.T) {
	f := Fault{
		Code: QName{Space: "http://opcfoundation.org/webservices/XMLDA/1.0/", Local: "E_TIMEDOUT"},
		Text: "the operation timed out",
	}

	out11, err := xml.Marshal(f.WithVersion(Version11))
	if err != nil {
		t.Fatalf("marshaling a 1.1 fault: %v", err)
	}
	for _, want := range []string{"<faultcode", "<faultstring>", "E_TIMEDOUT"} {
		if !strings.Contains(string(out11), want) {
			t.Errorf("the 1.1 fault does not contain %q:\n%s", want, out11)
		}
	}

	out12, err := xml.Marshal(f.WithVersion(Version12))
	if err != nil {
		t.Fatalf("marshaling a 1.2 fault: %v", err)
	}
	for _, want := range []string{"Code", "Value", "Reason", "Text", "E_TIMEDOUT"} {
		if !strings.Contains(string(out12), want) {
			t.Errorf("the 1.2 fault does not contain %q:\n%s", want, out12)
		}
	}
	if strings.Contains(string(out12), "<faultcode") {
		t.Errorf("the 1.2 fault uses 1.1's element names:\n%s", out12)
	}
	// A vendor/OPC code keeps its own namespace in either version.
	if !strings.Contains(string(out12), "opcfoundation.org") {
		t.Errorf("the 1.2 fault lost the code's namespace:\n%s", out12)
	}
	for _, out := range [][]byte{out11, out12} {
		if err := xml.Unmarshal(out, new(struct{})); err != nil {
			t.Errorf("emitted fault is not well-formed: %v\n%s", err, out)
		}
	}
}

// TestSoap12CodeName pins the two codes SOAP renamed between versions.
// Emitting "Client" into a 1.2 envelope names a code that version does
// not define.
func TestSoap12CodeName(t *testing.T) {
	for in, want := range map[string]string{
		"Client":          "Sender",
		"Server":          "Receiver",
		"MustUnderstand":  "MustUnderstand",
		"VersionMismatch": "VersionMismatch",
	} {
		if got := soap12CodeName(in); got != want {
			t.Errorf("soap12CodeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFault_SOAPDefinedCodeIsRewrittenIn12 is the end of that thread: a
// Client fault must go out as SOAP-ENV:Sender in a 1.2 document.
func TestFault_SOAPDefinedCodeIsRewrittenIn12(t *testing.T) {
	f := Fault{Code: QName{Space: NS11, Local: "Client"}, Text: "malformed request"}
	out, err := xml.Marshal(f.WithVersion(Version12))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), "Sender") {
		t.Errorf("a Client fault was not renamed to Sender for SOAP 1.2:\n%s", out)
	}
}

// TestFault_DetailIsEscapedWhenItIsNotAFragment pins the fix for a field
// whose doc comment said "text" while MarshalXML wrote it as raw XML. A
// captured fragment must survive a round trip verbatim; a hand-built
// message containing & or < must not produce a document no parser accepts
// — in the one response whose entire job is to carry an error.
func TestFault_DetailIsEscapedWhenItIsNotAFragment(t *testing.T) {
	for _, tc := range []struct {
		name     string
		detail   string
		verbatim bool
	}{
		{"well-formed fragment is preserved", `<MyAppException code="7"><at>pump</at></MyAppException>`, true},
		{"plain text is escaped", `value < 3 && flag == "on"`, false},
		{"unbalanced markup is escaped", `<open>`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := Fault{Code: QName{Space: NS11, Local: "Server"}, Text: "x", Detail: tc.detail}
			out, err := xml.Marshal(f)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			// Whatever the input, the result has to parse.
			if err := xml.Unmarshal(out, new(struct{})); err != nil {
				t.Fatalf("emitted fault is not well-formed: %v\n%s", err, out)
			}
			if tc.verbatim && !strings.Contains(string(out), tc.detail) {
				t.Errorf("a well-formed detail fragment was not preserved verbatim:\n%s", out)
			}
			if !tc.verbatim && strings.Contains(string(out), tc.detail) {
				t.Errorf("plain-text detail was written as raw markup:\n%s", out)
			}
		})
	}
}

// TestHeaderBlock_MustUnderstand pins both spellings SOAP allows and the
// values that do NOT mean true — a header flagged "0" must not be
// mistaken for one the server has to honor.
func TestHeaderBlock_MustUnderstand(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block HeaderBlock
		want  bool
	}{
		{"1.1 spelling", HeaderBlock{MustUnderstand11: "1"}, true},
		{"1.2 spelling", HeaderBlock{MustUnderstand12: "true"}, true},
		{"1.2 uppercase", HeaderBlock{MustUnderstand12: "TRUE"}, true},
		{"surrounding space", HeaderBlock{MustUnderstand11: " 1 "}, true},
		{"explicitly off", HeaderBlock{MustUnderstand11: "0"}, false},
		{"false", HeaderBlock{MustUnderstand12: "false"}, false},
		{"absent", HeaderBlock{}, false},
	} {
		if got := tc.block.MustUnderstand(); got != tc.want {
			t.Errorf("%s: MustUnderstand() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestEnvelope_MustUnderstandRejection walks the whole path: a header
// block the server cannot honor must stop the request, and one without
// the flag must not.
func TestEnvelope_MustUnderstandRejection(t *testing.T) {
	decode := func(header string) error {
		doc := `<SOAP-ENV:Envelope xmlns:SOAP-ENV="` + NS11 + `">` + header +
			`<SOAP-ENV:Body><DummyResponse xmlns="urn:x" Foo="a"/></SOAP-ENV:Body></SOAP-ENV:Envelope>`
		var env Envelope[dummyContent]
		return xml.Unmarshal([]byte(doc), &env)
	}

	err := decode(`<SOAP-ENV:Header><t:Auth xmlns:t="urn:sec" SOAP-ENV:mustUnderstand="1">s</t:Auth></SOAP-ENV:Header>`)
	var mu *MustUnderstandError
	if err == nil {
		t.Fatal("a mustUnderstand header block was accepted")
	}
	// errors.As, not a type assertion: checkMustUnderstand's error
	// travels back up through Envelope.UnmarshalXML, which is free to
	// wrap it, and a bare assertion would start failing silently the
	// day it does.
	if !errors.As(err, &mu) {
		t.Fatalf("got %T (%v), want *MustUnderstandError", err, err)
	}
	if len(mu.Blocks) != 1 || !strings.Contains(mu.Blocks[0], "Auth") {
		t.Errorf("the error does not name the offending block: %v", mu.Blocks)
	}
	if !strings.Contains(mu.Error(), "Auth") {
		t.Errorf("Error() does not name the block: %q", mu.Error())
	}

	if err := decode(`<SOAP-ENV:Header><t:Hint xmlns:t="urn:x">fyi</t:Hint></SOAP-ENV:Header>`); err != nil {
		t.Errorf("an unflagged header block was rejected: %v", err)
	}
	if err := decode(``); err != nil {
		t.Errorf("an envelope with no header at all was rejected: %v", err)
	}
}
