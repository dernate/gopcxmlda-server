package soap

import (
	"encoding/xml"
	"fmt"
	"strings"
)

// Fault is this package's normalized representation of a SOAP fault,
// regardless of which of the four observed wire shapes it came from (see
// testdata/faults/): spec-conformant SOAP 1.1 with a QName-qualified
// faultcode; a legacy SOAP 1.1 shape with unqualified literal text in
// faultcode/faultstring/detail; a generic SOAP 1.1 transport-level fault
// (e.g. an XML parse error) with a standard faultcode and no OPC content;
// and SOAP 1.2's Code/Reason/Detail structure. Producing code always
// builds this; MarshalXML always renders the spec-conformant SOAP 1.1
// shape (ADR-004).
type Fault struct {
	// Code is the fault's symbolic code. Zero Space means either no
	// meaningful namespace was resolvable (see resolveLenient) or this is
	// a transport-level fault with no OPC-specific code at all — Local
	// still holds whatever literal text was present.
	Code QName
	// Text is the human-readable fault description (faultstring / Reason/Text).
	Text string
	// Detail is opaque passthrough text (the spec's <detail> content
	// varies too much to structure further).
	Detail string
}

// Error implements the error interface. Safe to call on a nil *Fault (the
// classic typed-nil-in-error-interface trap), returning a placeholder
// string rather than panicking.
func (f *Fault) Error() string {
	if f == nil {
		return "soap fault: <nil>"
	}
	if f.Code.IsZero() {
		return fmt.Sprintf("soap fault: %s", f.Text)
	}
	return fmt.Sprintf("soap fault %s: %s", f.Code, f.Text)
}

// MarshalXML implements xml.Marshaler, always emitting the spec-conformant
// SOAP 1.1 shape: a QName-qualified faultcode (via a locally-declared
// "q0" prefix) when Code has a namespace, faultstring, and an (empty, if
// Detail=="") detail element.
func (f Fault) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Name = xml.Name{Local: "SOAP-ENV:Fault"}
	if err := e.EncodeToken(start); err != nil {
		return err
	}

	fcStart := xml.StartElement{Name: xml.Name{Local: "faultcode"}}
	fcText := f.Code.Local
	if f.Code.Space != "" {
		fcStart.Attr = append(fcStart.Attr, xml.Attr{Name: xml.Name{Local: "xmlns:q0"}, Value: f.Code.Space})
		fcText = "q0:" + f.Code.Local
	}
	if err := e.EncodeToken(fcStart); err != nil {
		return err
	}
	if fcText != "" {
		if err := e.EncodeToken(xml.CharData(fcText)); err != nil {
			return err
		}
	}
	if err := e.EncodeToken(fcStart.End()); err != nil {
		return err
	}

	if err := e.EncodeElement(f.Text, xml.StartElement{Name: xml.Name{Local: "faultstring"}}); err != nil {
		return err
	}

	detailStart := xml.StartElement{Name: xml.Name{Local: "detail"}}
	if f.Detail != "" {
		// Detail was captured via ,innerxml on decode (readDetail), so it
		// may hold literal XML markup (e.g. "<MyAppException>...") rather
		// than plain text. Re-encoding it through EncodeElement's normal
		// string handling would XML-escape that markup into unusable text,
		// corrupting a decode->encode->decode round trip. Encoding it back
		// through the same ,innerxml tag writes it verbatim, symmetric
		// with the decode side.
		holder := struct {
			InnerXML string `xml:",innerxml"`
		}{InnerXML: f.Detail}
		if err := e.EncodeElement(holder, detailStart); err != nil {
			return err
		}
	} else {
		if err := e.EncodeToken(detailStart); err != nil {
			return err
		}
		if err := e.EncodeToken(detailStart.End()); err != nil {
			return err
		}
	}
	return e.EncodeToken(start.End())
}

// UnmarshalXML implements xml.Unmarshaler, tolerantly parsing all four
// observed shapes by matching child elements by local name only
// (faultcode/faultstring/detail for SOAP 1.1, Code/Reason/Detail for
// SOAP 1.2), regardless of namespace or prefix.
func (f *Fault) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	for {
		tok, err := d.Token()
		if err != nil {
			return fmt.Errorf("soap: decoding Fault: %w", err)
		}
		switch t := tok.(type) {
		case xml.EndElement:
			return nil
		case xml.StartElement:
			switch t.Name.Local {
			case "faultcode":
				text, err := readText(d, t)
				if err != nil {
					return err
				}
				f.Code = resolveLenient(t.Attr, text)
			case "faultstring":
				text, err := readText(d, t)
				if err != nil {
					return err
				}
				f.Text = text
			case "detail", "Detail":
				text, err := readDetail(d, t)
				if err != nil {
					return err
				}
				f.Detail = text
			case "Code":
				text, attrs, err := readCode(d, t)
				if err != nil {
					return err
				}
				f.Code = resolveLenient(attrs, text)
			case "Reason":
				text, _, err := readNestedText(d, t, "Text")
				if err != nil {
					return err
				}
				f.Text = text
			default:
				if err := d.Skip(); err != nil {
					return err
				}
			}
		}
	}
}

// readText decodes start's character-data content (no children expected),
// trimmed of surrounding whitespace.
func readText(d *xml.Decoder, start xml.StartElement) (string, error) {
	var holder struct {
		Text string `xml:",chardata"`
	}
	if err := d.DecodeElement(&holder, &start); err != nil {
		return "", fmt.Errorf("soap: decoding <%s>: %w", start.Name.Local, err)
	}
	return strings.TrimSpace(holder.Text), nil
}

// readDetail decodes start's content via ,innerxml rather than ,chardata:
// <detail> is documented (Fault's own doc comment) as opaque,
// application-defined content that "varies too much to structure
// further" — in practice this is very often one or more child elements,
// not plain text (e.g. <detail><MyAppException>...</MyAppException></detail>
// is the SOAP spec's own suggested shape). A chardata-only decode would
// silently return "" for exactly that common case — no error, no signal
// that anything was dropped. innerxml preserves it verbatim (still
// XML-escaped, since it is not re-parsed) so a caller can inspect or log
// it even though this package makes no attempt to further structure it.
func readDetail(d *xml.Decoder, start xml.StartElement) (string, error) {
	var holder struct {
		InnerXML string `xml:",innerxml"`
	}
	if err := d.DecodeElement(&holder, &start); err != nil {
		return "", fmt.Errorf("soap: decoding <%s>: %w", start.Name.Local, err)
	}
	return strings.TrimSpace(holder.InnerXML), nil
}

// readCode decodes a SOAP 1.2 <Code> element (or, recursively, a <Subcode>,
// which shares the exact same Value(+nested Subcode) shape per the SOAP 1.2
// schema), returning the most specific available code and that element's
// own attributes (for resolveLenient).
//
// <Code>'s own direct <Value> is constrained by the SOAP 1.2 spec to one of
// five generic values (DataEncodingUnknown/MustUnderstand/Receiver/Sender/
// VersionMismatch) — the application-specific code a server actually wants
// to convey lives in <Subcode><Value>, recursing through any further-nested
// <Subcode> to the deepest one present. Previously the bare "Value" lookup
// skipped <Subcode> entirely via the default d.Skip() branch, silently
// discarding the one part of a SOAP 1.2 fault code that usually carries the
// real diagnostic information (e.g. a vendor's opc:E_INVALIDHANDLE).
func readCode(d *xml.Decoder, start xml.StartElement) (string, []xml.Attr, error) {
	var value string
	var valueAttrs []xml.Attr
	var subText string
	var subAttrs []xml.Attr
	var haveSub bool
	for {
		tok, err := d.Token()
		if err != nil {
			return "", nil, fmt.Errorf("soap: decoding <%s>: %w", start.Name.Local, err)
		}
		switch t := tok.(type) {
		case xml.EndElement:
			if haveSub {
				return subText, subAttrs, nil
			}
			return value, valueAttrs, nil
		case xml.StartElement:
			switch t.Name.Local {
			case "Value":
				v, err := readText(d, t)
				if err != nil {
					return "", nil, err
				}
				value, valueAttrs = v, t.Attr
			case "Subcode":
				v, attrs, err := readCode(d, t)
				if err != nil {
					return "", nil, err
				}
				subText, subAttrs, haveSub = v, attrs, true
			default:
				if err := d.Skip(); err != nil {
					return "", nil, err
				}
			}
		}
	}
}

// readNestedText consumes start's content looking for a child element
// named childLocal, returning its text and its own start attributes (for
// resolveLenient), while still consuming through start's own end tag
// regardless of whether childLocal was found.
func readNestedText(d *xml.Decoder, start xml.StartElement, childLocal string) (string, []xml.Attr, error) {
	var text string
	var attrs []xml.Attr
	for {
		tok, err := d.Token()
		if err != nil {
			return "", nil, fmt.Errorf("soap: decoding <%s>: %w", start.Name.Local, err)
		}
		switch t := tok.(type) {
		case xml.EndElement:
			return text, attrs, nil
		case xml.StartElement:
			if t.Name.Local == childLocal {
				v, err := readText(d, t)
				if err != nil {
					return "", nil, err
				}
				text, attrs = v, t.Attr
			} else if err := d.Skip(); err != nil {
				return "", nil, err
			}
		}
	}
}

// resolveLenient resolves raw (a possibly "prefix:local"-shaped string)
// against attrs — the xmlns declarations present on the SAME element the
// text came from (the pattern the specification's own example uses,
// §2.6 p.21: xmlns:q0 declared directly on <faultcode>). It never fails:
// an unresolvable prefix, or a bare local name, results in
// QName{Local: raw} with Space left empty — the original text is
// preserved for logging/matching even when it can't be fully resolved.
// This is a deliberately narrower scope than xmlda's whole-document
// resolveQName (see docs/specification/open-questions.md OQ-13): Fault
// decoding happens mid-stream without access to the full document's
// namespace declarations (e.g. one declared on the Envelope root rather
// than locally), so a prefix declared only on a remote ancestor will not
// resolve — an accepted limitation given no real captured fault fixture
// exercises that pattern.
func resolveLenient(attrs []xml.Attr, raw string) QName {
	raw = strings.TrimSpace(raw)
	prefix, local, hasPrefix := strings.Cut(raw, ":")
	if !hasPrefix {
		return QName{Local: raw}
	}
	for _, a := range attrs {
		if a.Name.Space == "xmlns" && a.Name.Local == prefix {
			return QName{Space: a.Value, Local: local}
		}
	}
	return QName{Local: raw}
}
