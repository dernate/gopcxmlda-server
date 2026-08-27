package xmlda

import "testing"

// TestNewDocument_MalformedXML_Error confirms NewDocument surfaces a
// malformed-XML error rather than panicking or silently producing an
// unusable Document.
func TestNewDocument_MalformedXML_Error(t *testing.T) {
	if _, err := NewDocument([]byte(`<this is not well-formed`)); err == nil {
		t.Fatalf("expected an error for malformed XML")
	}
}

// TestDocument_Decode_ReusableAcrossMultipleCalls is Document's core
// promise: its namespace-prefix table is built once by NewDocument and
// must remain usable for more than one Decode call against different
// target types — the whole reason a server handling a request builds one
// Document instead of calling the package-level Decode twice (once to
// identify the operation, once into the concrete request type).
func TestDocument_Decode_ReusableAcrossMultipleCalls(t *testing.T) {
	doc := envelopeFor(`xmlns:ns1="`+Namespace+`"`, `<probe Type="ns1:ItemValue"/>`)
	d, err := NewDocument(doc)
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}

	type shadow struct {
		Body struct {
			P probe `xml:"probe"`
		} `xml:"Body"`
	}

	var first shadow
	if err := d.Decode(&first); err != nil {
		t.Fatalf("first Decode: %v", err)
	}
	want := QName{Space: Namespace, Local: "ItemValue"}
	if first.Body.P.Resolved != want {
		t.Fatalf("first decode: got %+v, want %+v", first.Body.P.Resolved, want)
	}

	// Decoded again, into a fresh target — the prefix scope built by
	// NewDocument must still resolve correctly, proving it was not
	// consumed or invalidated by the first Decode.
	var second shadow
	if err := d.Decode(&second); err != nil {
		t.Fatalf("second Decode: %v", err)
	}
	if second.Body.P.Resolved != want {
		t.Fatalf("second decode: got %+v, want %+v", second.Body.P.Resolved, want)
	}
}

// TestDocument_IdentifyOperation_MatchesPackageLevel confirms the new
// Document.IdentifyOperation method (used by server.Handler.ServeHTTP,
// which builds one Document per request) agrees with the package-level
// IdentifyOperation it now delegates to.
func TestDocument_IdentifyOperation_MatchesPackageLevel(t *testing.T) {
	raw := envelopeFor(`xmlns:ns1="`+Namespace+`"`, `<ns1:Read/>`)

	wantOp, wantOK, wantErr := IdentifyOperation(raw)
	if wantErr != nil || !wantOK {
		t.Fatalf("package-level IdentifyOperation: op=%+v ok=%v err=%v", wantOp, wantOK, wantErr)
	}

	d, err := NewDocument(raw)
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	gotOp, gotOK, gotErr := d.IdentifyOperation()
	if gotErr != nil {
		t.Fatalf("Document.IdentifyOperation: %v", gotErr)
	}
	if gotOK != wantOK || gotOp != wantOp {
		t.Fatalf("Document.IdentifyOperation = (%+v, %v), want (%+v, %v)", gotOp, gotOK, wantOp, wantOK)
	}
}

// TestDocument_IdentifyOperation_UnknownOperation is the regression-safety
// companion: the method form must still report ok=false for a
// well-formed-but-unrecognized operation, exactly like the package-level
// function.
func TestDocument_IdentifyOperation_UnknownOperation(t *testing.T) {
	raw := envelopeFor(``, `<SomeUnknownOperation xmlns="`+Namespace+`"/>`)
	d, err := NewDocument(raw)
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	_, ok, err := d.IdentifyOperation()
	if err != nil {
		t.Fatalf("Document.IdentifyOperation: %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false for an unrecognized operation")
	}
}
