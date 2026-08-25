package xmlda

import (
	"bytes"
	"encoding/xml"
	"testing"
)

// xmlMarshalNamed marshals v wrapped in a root element named rootName —
// used by tests that need a stable, named root regardless of v's own Go
// type name (xml.Marshal on an unnamed/anonymous struct can fail; see
// docs/architecture/testing-strategy.md).
func xmlMarshalNamed(t *testing.T, rootName string, v any) ([]byte, error) {
	t.Helper()
	var buf []byte
	e := xml.NewEncoder(&testByteWriter{&buf})
	start := xml.StartElement{Name: xml.Name{Local: rootName}}
	if err := e.EncodeElement(v, start); err != nil {
		return nil, err
	}
	if err := e.Flush(); err != nil {
		return nil, err
	}
	return buf, nil
}

type testByteWriter struct{ buf *[]byte }

func (w *testByteWriter) Write(p []byte) (int, error) {
	*w.buf = append(*w.buf, p...)
	return len(p), nil
}

// xmlStartElementFor builds a StartElement with the given local name and
// attributes, for tests exercising attribute-level encode/decode helpers
// directly (e.g. encodeItemParamsAttrs/decodeItemParamsAttrs).
func xmlStartElementFor(local string, attrs []xml.Attr) xml.StartElement {
	return xml.StartElement{Name: xml.Name{Local: local}, Attr: attrs}
}

// xmlMarshalStart encodes a single self-closing element from a
// pre-built StartElement (attributes already assembled).
func xmlMarshalStart(t *testing.T, start xml.StartElement) ([]byte, error) {
	t.Helper()
	var buf []byte
	e := xml.NewEncoder(&testByteWriter{&buf})
	if err := e.EncodeToken(start); err != nil {
		return nil, err
	}
	if err := e.EncodeToken(start.End()); err != nil {
		return nil, err
	}
	if err := e.Flush(); err != nil {
		return nil, err
	}
	return buf, nil
}

// decodeItemParamsFromDoc decodes doc's root element's attributes via
// decodeItemParamsAttrs, with a properly registered namespace scope so
// resolveQName (used for ReqType) works.
func decodeItemParamsFromDoc(t *testing.T, doc []byte) ItemParams {
	t.Helper()
	table, err := buildPrefixTable(doc)
	if err != nil {
		t.Fatalf("buildPrefixTable: %v", err)
	}
	d := xml.NewDecoder(bytes.NewReader(doc))
	defer withScope(d, table)()
	tok, err := d.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	start, ok := tok.(xml.StartElement)
	if !ok {
		t.Fatalf("expected StartElement, got %T", tok)
	}
	p, err := decodeItemParamsAttrs(d, start.Attr)
	if err != nil {
		t.Fatalf("decodeItemParamsAttrs: %v", err)
	}
	return p
}
