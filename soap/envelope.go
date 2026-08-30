// Package soap implements generic SOAP 1.1/1.2 envelope and fault
// handling, with no OPC XML-DA vocabulary of its own — see
// docs/architecture/package-structure.md for why this is split from
// package xmlda. Output is always the spec-conformant SOAP 1.1 shape
// (see ADR-004); input tolerates SOAP 1.1, SOAP 1.2, and the
// non-conformant legacy fault shapes observed in testdata/faults/.
//
// Receivers on the wire types in this package are deliberately mixed:
// MarshalXML takes a value receiver and UnmarshalXML a pointer receiver.
// Neither half is a free choice. UnmarshalXML must be a pointer method to
// populate the receiver at all, and MarshalXML must be a value method
// because encoding/xml only consults the custom marshaler when the value
// it holds implements xml.Marshaler or is addressable — a Value reached
// through xml.Marshal, or as a field of a struct passed by value, is
// neither. Moving MarshalXML to a pointer receiver does not fail loudly:
// the encoder silently falls back to default struct encoding and emits
// <Value></Value> with no xsi:type. Static analysis flags the mix
// (staticcheck does not, GoLand's "mixed value and pointer receivers"
// does); it is correct that the mix exists and wrong that it is a defect.
package soap

import (
	"encoding/xml"
	"fmt"
	"reflect"
)

// NS11 and NS12 are the SOAP 1.1 and SOAP 1.2 envelope namespace URIs.
// This package's own output always uses NS11; both are accepted on input.
const (
	NS11 = "http://schemas.xmlsoap.org/soap/envelope/"
	NS12 = "http://www.w3.org/2003/05/soap-envelope"
)

// QName is a namespace URI plus a local name, package-local to soap so
// this package has no dependency on xmlda (see
// docs/architecture/package-structure.md). It is structurally identical
// to, but a distinct type from, xmlda.QName.
type QName struct {
	Space string
	Local string
}

// IsZero reports whether q is the zero QName.
func (q QName) IsZero() bool { return q.Space == "" && q.Local == "" }

// String returns a Clark-notation representation, for logging.
func (q QName) String() string {
	if q.Space == "" {
		return q.Local
	}
	return "{" + q.Space + "}" + q.Local
}

// Envelope wraps a request or response Body of type T. Header/Body are
// matched by local element name only, regardless of namespace or
// prefix — encoding/xml's own default behavior for tag-less-of-namespace
// struct tags — so the same Envelope decodes SOAP 1.1 and SOAP 1.2
// envelopes interchangeably (see ADR-004). MarshalXML always emits the
// SOAP 1.1 shape.
type Envelope[T any] struct {
	// Name is the resolved root element name observed on decode ({},
	// "Envelope" for a well-formed SOAP envelope of either version). It
	// is informational only; MarshalXML does not use it — this package's
	// own output always uses the SOAP 1.1 envelope shape.
	Name QName
	// Header carries the envelope's header content verbatim (OPC XML-DA
	// defines no header content this package acts on).
	Header *Header
	// Body carries either a successful payload or a Fault.
	Body Body[T]
}

// MarshalXML implements xml.Marshaler, always emitting the SOAP 1.1
// envelope shape (ADR-004): an empty Header, then Body.
func (e Envelope[T]) MarshalXML(enc *xml.Encoder, start xml.StartElement) error {
	start.Name = xml.Name{Local: "SOAP-ENV:Envelope"}
	start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "xmlns:SOAP-ENV"}, Value: NS11})
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	headerStart := xml.StartElement{Name: xml.Name{Local: "SOAP-ENV:Header"}}
	if err := enc.EncodeToken(headerStart); err != nil {
		return err
	}
	if err := enc.EncodeToken(headerStart.End()); err != nil {
		return err
	}
	if err := enc.EncodeElement(e.Body, xml.StartElement{Name: xml.Name{Local: "SOAP-ENV:Body"}}); err != nil {
		return err
	}
	return enc.EncodeToken(start.End())
}

// UnmarshalXML implements xml.Unmarshaler. It accepts any envelope
// namespace (SOAP 1.1, SOAP 1.2, or otherwise) as long as the root
// element's local name is "Envelope" — namespace identity is recorded in
// Name for informational use, never used to reject an otherwise
// well-formed envelope, consistent with this package's "match by local
// name" philosophy (ADR-004).
func (e *Envelope[T]) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	if start.Name.Local != "Envelope" {
		return fmt.Errorf("soap: root element %q is not a SOAP Envelope", start.Name.Local)
	}
	var shadow struct {
		Header *Header `xml:"Header"`
		Body   Body[T] `xml:"Body"`
	}
	if err := d.DecodeElement(&shadow, &start); err != nil {
		return fmt.Errorf("soap: decoding envelope: %w", err)
	}
	e.Name = QName{Space: start.Name.Space, Local: start.Name.Local}
	e.Header = shadow.Header
	e.Body = shadow.Body
	return nil
}

// Header carries a SOAP envelope's header content verbatim. OPC XML-DA
// defines no header content this package acts on; InnerXML is captured
// for completeness/round-trip on decode only (encoding/xml does not
// support re-emitting a ",innerxml" field on Marshal) — Envelope's own
// MarshalXML always emits an empty header instead of round-tripping this
// field, which is correct for this library's own output but means a
// decoded Header's content should not be assumed to survive a
// decode-then-encode round trip.
type Header struct {
	InnerXML []byte `xml:",innerxml"`
}

// Body carries either a successful payload (Content) or a Fault, never
// both. UnmarshalXML peeks the first child's local element name to
// decide which — "Fault" (any namespace) means Fault, anything else
// means Content.
type Body[T any] struct {
	Content *T
	Fault   *Fault
}

// MarshalXML implements xml.Marshaler.
func (b Body[T]) MarshalXML(enc *xml.Encoder, start xml.StartElement) error {
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	switch {
	case b.Fault != nil:
		if err := enc.Encode(*b.Fault); err != nil {
			return err
		}
	case b.Content != nil:
		// T may itself be a pointer or interface type; Content being a
		// non-nil *T does not guarantee *Content is non-nil. encoding/xml
		// silently no-ops on a nil pointer/interface value (no tokens, no
		// error), which would otherwise produce a bare, invariant-violating
		// <Body></Body> — neither a payload nor a Fault — with no signal
		// that anything went wrong.
		if rv := reflect.ValueOf(*b.Content); (rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface) && rv.IsNil() {
			return fmt.Errorf("soap: Body.Content is a non-nil *%T pointing at a nil value", *new(T))
		}
		if err := enc.Encode(*b.Content); err != nil {
			return err
		}
	}
	return enc.EncodeToken(start.End())
}

// UnmarshalXML implements xml.Unmarshaler.
func (b *Body[T]) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	for {
		tok, err := d.Token()
		if err != nil {
			return fmt.Errorf("soap: decoding Body: %w", err)
		}
		switch t := tok.(type) {
		case xml.EndElement:
			return nil // empty body
		case xml.StartElement:
			if t.Name.Local == "Fault" {
				var f Fault
				if err := f.UnmarshalXML(d, t); err != nil {
					return err
				}
				b.Fault = &f
			} else {
				var content T
				if err := d.DecodeElement(&content, &t); err != nil {
					return fmt.Errorf("soap: decoding Body content <%s>: %w", t.Name.Local, err)
				}
				b.Content = &content
			}
			return skipToEnd(d)
		}
	}
}

// skipToEnd consumes tokens until the current element's matching end tag,
// tolerating character data (whitespace) but rejecting an unexpected
// second child element — Body must contain exactly one child.
func skipToEnd(d *xml.Decoder) error {
	for {
		tok, err := d.Token()
		if err != nil {
			return fmt.Errorf("soap: decoding Body: %w", err)
		}
		switch tok.(type) {
		case xml.EndElement:
			return nil
		case xml.StartElement:
			return fmt.Errorf("soap: unexpected additional element in Body")
		}
	}
}
