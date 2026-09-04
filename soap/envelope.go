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
	"maps"
	"reflect"
	"slices"
	"strings"
)

// NS11 and NS12 are the SOAP 1.1 and SOAP 1.2 envelope namespace URIs.
// This package's own output always uses NS11; both are accepted on input.
const (
	NS11 = "http://schemas.xmlsoap.org/soap/envelope/"
	NS12 = "http://www.w3.org/2003/05/soap-envelope"
)

// Version identifies a SOAP envelope version.
type Version int

// The SOAP versions this package can read and write.
const (
	// Version11 is SOAP 1.1, the version OPC XML-DA 1.0 is defined over
	// and the default for a zero Version.
	Version11 Version = iota
	// Version12 is SOAP 1.2. Accepted on input and mirrored on output, so
	// a client that speaks it is answered in it rather than handed a 1.1
	// envelope its stack discards.
	Version12
)

// NS returns the envelope namespace URI for v.
func (v Version) NS() string {
	if v == Version12 {
		return NS12
	}
	return NS11
}

// ContentType returns the media type a response of this version is served
// with: SOAP 1.1 uses text/xml, SOAP 1.2 has its own type.
func (v Version) ContentType() string {
	if v == Version12 {
		return "application/soap+xml; charset=utf-8"
	}
	return "text/xml; charset=utf-8"
}

// VersionOf reports the SOAP version an envelope namespace URI denotes,
// defaulting to 1.1 for anything else — this package matches envelopes by
// local name and never rejects one over its namespace (ADR-004).
func VersionOf(envelopeNS string) Version {
	if envelopeNS == NS12 {
		return Version12
	}
	return Version11
}

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
	// Version selects the envelope shape MarshalXML writes. The zero value
	// is SOAP 1.1, which is what OPC XML-DA 1.0 is defined over; set it
	// from the request being answered (VersionOf(env.Name.Space)) so a
	// client that spoke 1.2 is answered in 1.2.
	Version Version
	// ExtraNamespaces are prefix -> URI declarations emitted on the
	// Envelope element itself, so the payload below need not repeat them
	// on every element it writes.
	//
	// This package knows nothing about those namespaces and does not use
	// them; it only writes them where XML says a declaration belongs — on
	// the outermost element that needs it. A caller setting this is
	// responsible for making its payload actually reference the prefixes
	// declared here (see xmlda.DeclareAncestorNamespaces).
	ExtraNamespaces map[string]string
}

// MarshalXML implements xml.Marshaler, always emitting the SOAP 1.1
// envelope shape (ADR-004): an empty Header, then Body.
func (e Envelope[T]) MarshalXML(enc *xml.Encoder, start xml.StartElement) error {
	start.Name = xml.Name{Local: "SOAP-ENV:Envelope"}
	start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "xmlns:SOAP-ENV"}, Value: e.Version.NS()})
	for _, prefix := range slices.Sorted(maps.Keys(e.ExtraNamespaces)) {
		start.Attr = append(start.Attr,
			xml.Attr{Name: xml.Name{Local: "xmlns:" + prefix}, Value: e.ExtraNamespaces[prefix]})
	}
	// A fault code is a QName in element CONTENT, and its prefix is bound
	// in TWO scopes: here on the envelope, and again on the element
	// carrying it (see Fault.MarshalXML). Both bindings name the same
	// URI, so the QName is the same either way and no parser can see a
	// conflict — the redundancy buys compatibility with two kinds of
	// reader that each miss one of the scopes.
	//
	// Locally, because that is the specification's own example (§2.6
	// p.21) and because this package's own fault decoder resolves
	// prefixes element-locally by design: soap must not depend on
	// xmlda's whole-document prefix scan (open-questions.md OQ-13), so
	// dropping the local binding would leave this library unable to read
	// the faults it writes.
	//
	// On the envelope, because a QName in content is the fragile case
	// for the other kind of parser: one built on a namespace-normalizing
	// DOM resolves prefixes from the scope it entered an element with,
	// and a declaration made ON that element is not yet in that scope.
	// Not hypothetical — mlabs-haskell/opc-xml-da-client resolves every
	// xsi:type this server sends, because those prefixes are declared up
	// here, yet answered a locally-declared fault code with "Namespace
	// not found: q0" and so could not read fault codes at all. For an OPC
	// client that is not cosmetic: §2.5.1's error-handling flow turns on
	// telling E_NOSUBSCRIPTION from E_BUSY from E_TIMEDOUT.
	if e.Body.Fault != nil && e.Body.Fault.Code.Space != "" {
		start.Attr = append(start.Attr,
			xml.Attr{Name: xml.Name{Local: "xmlns:" + FaultCodePrefix}, Value: e.Body.Fault.Code.Space})
	}
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
	body := e.Body
	body.version = e.Version
	if err := enc.EncodeElement(body, xml.StartElement{Name: xml.Name{Local: "SOAP-ENV:Body"}}); err != nil {
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
		Headers []Header  `xml:"Header"`
		Bodies  []Body[T] `xml:"Body"`
	}
	if err := d.DecodeElement(&shadow, &start); err != nil {
		return fmt.Errorf("soap: decoding envelope: %w", err)
	}
	// SOAP 1.1 §4 allows exactly one Body. Accepting more and using one of
	// them — encoding/xml keeps the LAST for a non-slice field — is a
	// request-smuggling shape: an intermediary that inspects the FIRST
	// Body (a proxy, an audit log, a policy filter) sees a different
	// operation than the one this server executes.
	if len(shadow.Bodies) != 1 {
		return fmt.Errorf("soap: envelope carries %d Body elements, want exactly 1", len(shadow.Bodies))
	}
	if len(shadow.Headers) > 1 {
		return fmt.Errorf("soap: envelope carries %d Header elements, want at most 1", len(shadow.Headers))
	}
	e.Name = QName{Space: start.Name.Space, Local: start.Name.Local}
	if len(shadow.Headers) == 1 {
		e.Header = &shadow.Headers[0]
	}
	e.Body = shadow.Bodies[0]
	return e.checkMustUnderstand()
}

// checkMustUnderstand enforces SOAP 1.1 §4.2.3: a header block flagged
// mustUnderstand that the recipient does not understand must be answered
// with a MustUnderstand fault, not processed as though the block were not
// there.
//
// This package understands no header blocks at all — OPC XML-DA defines
// none — so any flagged block is, by definition, one it does not
// understand. Ignoring them was not merely non-conformant: a deployment
// that puts authorization in a WS-Security header would have had that
// header dropped and the operation carried out anyway.
func (e *Envelope[T]) checkMustUnderstand() error {
	if e.Header == nil {
		return nil
	}
	var names []string
	for _, b := range e.Header.Blocks {
		if b.MustUnderstand() {
			names = append(names, QName{Space: b.XMLName.Space, Local: b.XMLName.Local}.String())
		}
	}
	if len(names) == 0 {
		return nil
	}
	return &MustUnderstandError{Blocks: names}
}

// MustUnderstandError reports header blocks flagged mustUnderstand that
// this package does not understand. The server layer turns it into a
// SOAP-ENV:MustUnderstand fault (§4.2.3, §4.4).
type MustUnderstandError struct {
	Blocks []string
}

func (e *MustUnderstandError) Error() string {
	return "soap: unsupported mustUnderstand header block(s): " + strings.Join(e.Blocks, ", ")
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
	// Blocks are the header's immediate children, decoded IN THE
	// DOCUMENT'S CONTEXT so their namespace prefixes resolve. Re-parsing
	// InnerXML on its own cannot do that: a mustUnderstand attribute is
	// qualified with the SOAP prefix, and that prefix is declared on the
	// Envelope, outside the captured fragment — so it comes back
	// unresolved and the flag reads as an unrelated attribute.
	Blocks []HeaderBlock `xml:",any"`
}

// HeaderBlock is one immediate child of a SOAP Header, carrying just
// enough to answer the only question this package asks of it: was the
// recipient told it has to understand this block?
type HeaderBlock struct {
	XMLName xml.Name
	// MustUnderstand11 and MustUnderstand12 are the same attribute in the
	// two envelope namespaces; a document uses one or the other.
	MustUnderstand11 string `xml:"http://schemas.xmlsoap.org/soap/envelope/ mustUnderstand,attr"`
	MustUnderstand12 string `xml:"http://www.w3.org/2003/05/soap-envelope mustUnderstand,attr"`
}

// MustUnderstand reports whether this block carries a true mustUnderstand
// flag in either envelope namespace. SOAP 1.1 spells it "1", SOAP 1.2
// also allows "true".
func (b HeaderBlock) MustUnderstand() bool {
	for _, v := range []string{b.MustUnderstand11, b.MustUnderstand12} {
		v = strings.TrimSpace(v)
		if v == "1" || strings.EqualFold(v, "true") {
			return true
		}
	}
	return false
}

// Body carries either a successful payload (Content) or a Fault, never
// both. UnmarshalXML peeks the first child's local element name to
// decide which — "Fault" (any namespace) means Fault, anything else
// means Content.
type Body[T any] struct {
	Content *T
	Fault   *Fault
	// version is threaded down from the Envelope so a Fault is written in
	// the same SOAP version as the envelope carrying it.
	version Version
}

// MarshalXML implements xml.Marshaler.
func (b Body[T]) MarshalXML(enc *xml.Encoder, start xml.StartElement) error {
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	switch {
	case b.Fault != nil:
		f := *b.Fault
		f.version = b.version
		if err := enc.Encode(f); err != nil {
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
