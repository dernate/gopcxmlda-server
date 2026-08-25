package xmlda

import (
	"testing"
)

func envelopeFor(prefixDecl, bodyContent string) []byte {
	return []byte(`<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://schemas.xmlsoap.org/soap/envelope/" ` + prefixDecl + `>` +
		`<SOAP-ENV:Body>` + bodyContent + `</SOAP-ENV:Body></SOAP-ENV:Envelope>`)
}

func TestIdentifyOperation_AllEightOperations(t *testing.T) {
	for _, local := range operationLocalNames {
		t.Run(local, func(t *testing.T) {
			doc := envelopeFor(`xmlns:ns1="`+Namespace+`"`, `<ns1:`+local+`/>`)
			op, ok, err := IdentifyOperation(doc)
			if err != nil {
				t.Fatalf("IdentifyOperation: %v", err)
			}
			if !ok {
				t.Fatalf("expected operation %q to be recognized", local)
			}
			if op.Name.Local != local || op.Name.Space != Namespace {
				t.Fatalf("got %+v, want local=%s space=%s", op.Name, local, Namespace)
			}
			if op.SOAPAction != Namespace+local {
				t.Fatalf("got SOAPAction=%q, want %q", op.SOAPAction, Namespace+local)
			}
		})
	}
}

func TestIdentifyOperation_AlternativePrefixesAndDefaultNamespace(t *testing.T) {
	cases := []struct {
		name string
		doc  []byte
	}{
		{"prefix ns1", envelopeFor(`xmlns:ns1="`+Namespace+`"`, `<ns1:Read/>`)},
		{"prefix q0", envelopeFor(`xmlns:q0="`+Namespace+`"`, `<q0:Read/>`)},
		{"default namespace on Body child", envelopeFor(``, `<Read xmlns="`+Namespace+`"/>`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op, ok, err := IdentifyOperation(tc.doc)
			if err != nil {
				t.Fatalf("IdentifyOperation: %v", err)
			}
			if !ok || op.Name.Local != "Read" {
				t.Fatalf("got op=%+v ok=%v, want Read/true", op, ok)
			}
		})
	}
}

func TestIdentifyOperation_RealSubscribeFixture(t *testing.T) {
	doc := readTestdata(t, "testdata", "requests", "subscribe_679.request.xml")
	op, ok, err := IdentifyOperation(doc)
	if err != nil {
		t.Fatalf("IdentifyOperation: %v", err)
	}
	if !ok || op.Name.Local != "Subscribe" {
		t.Fatalf("got op=%+v ok=%v, want Subscribe/true", op, ok)
	}
}

func TestIdentifyOperation_RealGetStatusFixture(t *testing.T) {
	doc := readTestdata(t, "testdata", "requests", "getstatus_632.request.xml")
	op, ok, err := IdentifyOperation(doc)
	if err != nil {
		t.Fatalf("IdentifyOperation: %v", err)
	}
	if !ok || op.Name.Local != "GetStatus" {
		t.Fatalf("got op=%+v ok=%v, want GetStatus/true", op, ok)
	}
}

func TestIdentifyOperation_RealBrowseFixture(t *testing.T) {
	doc := readTestdata(t, "testdata", "requests", "browse_653.request.xml")
	op, ok, err := IdentifyOperation(doc)
	if err != nil {
		t.Fatalf("IdentifyOperation: %v", err)
	}
	if !ok || op.Name.Local != "Browse" {
		t.Fatalf("got op=%+v ok=%v, want Browse/true", op, ok)
	}
}

func TestIdentifyOperation_RealGetPropertiesFixture(t *testing.T) {
	doc := readTestdata(t, "testdata", "requests", "getproperties_103.request.xml")
	op, ok, err := IdentifyOperation(doc)
	if err != nil {
		t.Fatalf("IdentifyOperation: %v", err)
	}
	if !ok || op.Name.Local != "GetProperties" {
		t.Fatalf("got op=%+v ok=%v, want GetProperties/true", op, ok)
	}
}

func TestIdentifyOperation_RealReadFixture(t *testing.T) {
	doc := readTestdata(t, "testdata", "requests", "read_649.request.xml")
	op, ok, err := IdentifyOperation(doc)
	if err != nil {
		t.Fatalf("IdentifyOperation: %v", err)
	}
	if !ok || op.Name.Local != "Read" {
		t.Fatalf("got op=%+v ok=%v, want Read/true", op, ok)
	}
}

func TestIdentifyOperation_RealSubscriptionPolledRefreshFixture(t *testing.T) {
	doc := readTestdata(t, "testdata", "requests", "subscriptionpolledrefresh_226.request.xml")
	op, ok, err := IdentifyOperation(doc)
	if err != nil {
		t.Fatalf("IdentifyOperation: %v", err)
	}
	if !ok || op.Name.Local != "SubscriptionPolledRefresh" {
		t.Fatalf("got op=%+v ok=%v, want SubscriptionPolledRefresh/true", op, ok)
	}
}

func TestIdentifyOperation_RealSubscriptionCancelFixture(t *testing.T) {
	doc := readTestdata(t, "testdata", "requests", "subscriptioncancel_448.request.xml")
	op, ok, err := IdentifyOperation(doc)
	if err != nil {
		t.Fatalf("IdentifyOperation: %v", err)
	}
	if !ok || op.Name.Local != "SubscriptionCancel" {
		t.Fatalf("got op=%+v ok=%v, want SubscriptionCancel/true", op, ok)
	}
}

func TestIdentifyOperation_RealReadArrayOfDoubleFixture(t *testing.T) {
	doc := readTestdata(t, "testdata", "requests", "read_169.request.xml")
	op, ok, err := IdentifyOperation(doc)
	if err != nil {
		t.Fatalf("IdentifyOperation: %v", err)
	}
	if !ok || op.Name.Local != "Read" {
		t.Fatalf("got op=%+v ok=%v, want Read/true", op, ok)
	}
}

func TestIdentifyOperation_UnknownOperation(t *testing.T) {
	doc := envelopeFor(`xmlns:ns1="`+Namespace+`"`, `<ns1:SomeUnknownOperation/>`)
	op, ok, err := IdentifyOperation(doc)
	if err != nil {
		t.Fatalf("IdentifyOperation: %v", err)
	}
	if ok {
		t.Fatalf("expected an unrecognized operation, got %+v", op)
	}
}

func TestIdentifyOperation_WrongNamespace(t *testing.T) {
	// Same local name, wrong namespace URI — must not be recognized,
	// confirming namespace URI (not just local name) is checked.
	doc := envelopeFor(`xmlns:ns1="http://example.com/not-opc"`, `<ns1:Read/>`)
	_, ok, err := IdentifyOperation(doc)
	if err != nil {
		t.Fatalf("IdentifyOperation: %v", err)
	}
	if ok {
		t.Fatalf("expected Read in the wrong namespace to be unrecognized")
	}
}

func TestIdentifyOperation_NoBody(t *testing.T) {
	doc := []byte(`<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://schemas.xmlsoap.org/soap/envelope/"></SOAP-ENV:Envelope>`)
	_, ok, err := IdentifyOperation(doc)
	if err != nil {
		t.Fatalf("IdentifyOperation: %v", err)
	}
	if ok {
		t.Fatalf("expected no operation to be identified when there is no Body")
	}
}

func TestIdentifyOperation_MalformedXML(t *testing.T) {
	doc := []byte(`<SOAP-ENV:Envelope><this is not well-formed`)
	_, _, err := IdentifyOperation(doc)
	if err == nil {
		t.Fatalf("expected a decode error for malformed XML")
	}
}

func TestIdentifyOperation_EmptyBody(t *testing.T) {
	doc := envelopeFor(``, ``)
	_, ok, err := IdentifyOperation(doc)
	if err != nil {
		t.Fatalf("IdentifyOperation: %v", err)
	}
	if ok {
		t.Fatalf("expected no operation identified for an empty Body")
	}
}
