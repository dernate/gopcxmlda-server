// Package xmlda implements the OPC XML-DA 1.0 wire protocol: the eight SOAP
// operations, the generic Value container, quality and error models, and the
// cross-cutting structures shared by every operation.
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
package xmlda

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// Namespace is the single XML namespace used by every OPC XML-DA 1.0
// request, response, and standard result code, per §2.2 of the
// specification.
const Namespace = "http://opcfoundation.org/webservices/XMLDA/1.0/"

// XSDNamespace and XSINamespace are the standard XML Schema namespaces used
// for scalar Value types (XSDNamespace) and the xsi:type/xsi:nil attributes
// (XSINamespace).
const (
	XSDNamespace = "http://www.w3.org/2001/XMLSchema"
	XSINamespace = "http://www.w3.org/2001/XMLSchema-instance"
)

const xmlNamespace = "http://www.w3.org/XML/1998/namespace"

// QName is a namespace URI plus a local name. Unlike a "prefix:local"
// string, a QName's identity never depends on which prefix a particular
// document happened to bind to a namespace — see
// docs/architecture/decisions/004-namespace-processing.md.
type QName struct {
	Space string
	Local string
}

// IsZero reports whether q is the zero QName (no namespace, no local name).
func (q QName) IsZero() bool { return q.Space == "" && q.Local == "" }

// String returns a Clark-notation representation ("{space}local") suitable
// for logging and error messages. It is never used for equality checks.
func (q QName) String() string {
	if q.Space == "" {
		return q.Local
	}
	return "{" + q.Space + "}" + q.Local
}

// decoderScopes associates a *xml.Decoder instance with the flattened
// prefix table built for the document it is decoding. encoding/xml passes
// the same *Decoder pointer through every nested UnmarshalXML call within
// one Decode invocation, and each top-level Decode uses a fresh *Decoder, so
// entries never bleed between unrelated decodes. Entries are removed via the
// cleanup function returned by withScope immediately after the top-level
// decode returns.
var decoderScopes sync.Map // map[*xml.Decoder]map[string]string

// buildPrefixTable scans raw for every namespace declaration
// (xmlns="uri", the default namespace, keyed by "", and xmlns:prefix="uri")
// anywhere in the document and returns one flattened prefix -> URI map.
//
// This is a deliberate simplification over a true nested XML namespace
// scope stack: if the same prefix is legitimately rebound to different URIs
// at different depths, this table uses "last declaration wins" rather than
// depth-correct shadowing. No real OPC XML-DA traffic observed during this
// project's specification analysis does that; see
// docs/specification/open-questions.md OQ-6 for the accepted limitation.
func buildPrefixTable(raw []byte) (map[string]string, error) {
	table := map[string]string{"xml": xmlNamespace}
	d := xml.NewDecoder(bytes.NewReader(raw))
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("xmlda: scanning namespace declarations: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		for _, a := range se.Attr {
			switch {
			case a.Name.Space == "xmlns":
				table[a.Name.Local] = a.Value
			case a.Name.Space == "" && a.Name.Local == "xmlns":
				table[""] = a.Value
			}
		}
	}
	return table, nil
}

// withScope registers table as the prefix scope for d and returns a cleanup
// function that must be called (via defer) once the top-level decode using d
// has returned, to prevent the map from growing unboundedly across many
// decodes.
func withScope(d *xml.Decoder, table map[string]string) func() {
	decoderScopes.Store(d, table)
	return func() { decoderScopes.Delete(d) }
}

// resolveQName is the single place any "prefix:local" wire text is ever
// interpreted. It resolves raw (an xsi:type value, a ResultID value, an
// ItemProperty Name, ...) against the prefix scope registered for d via
// withScope. Everywhere else in this package, QName equality is by
// (Space, Local) only — never by comparing prefixed strings.
func resolveQName(d *xml.Decoder, raw string) (QName, error) {
	prefix, local, hasPrefix := strings.Cut(raw, ":")
	if !hasPrefix {
		// No colon: local name only, resolved against the default namespace
		// in scope (which may itself be "" if no default namespace applies).
		local, prefix = prefix, ""
	}
	scope, ok := decoderScopes.Load(d)
	if !ok {
		// Not an internal error: this is what a caller sees who reached
		// for encoding/xml directly. Say so, and name the fix — a QName
		// value like xsi:type or ResultID cannot be resolved without the
		// document's prefix declarations, which only xmlda's own entry
		// points collect.
		return QName{}, fmt.Errorf("xmlda: cannot resolve QName %q: this type must be decoded via "+
			"xmlda.Decode or (*xmlda.Document).Decode, not encoding/xml's Unmarshal/Decode, "+
			"which cannot supply the document's namespace-prefix scope", raw)
	}
	table, ok := scope.(map[string]string)
	if !ok {
		return QName{}, fmt.Errorf("xmlda: prefix scope for this decoder holds %T, not a prefix table "+
			"(internal inconsistency)", scope)
	}
	uri, ok := table[prefix]
	if !ok {
		if prefix == "" {
			return QName{}, fmt.Errorf("xmlda: unresolvable QName %q: no default namespace in scope", raw)
		}
		return QName{}, fmt.Errorf("xmlda: unresolvable QName %q: unbound prefix %q", raw, prefix)
	}
	return QName{Space: uri, Local: local}, nil
}

// Document is a raw OPC XML-DA/SOAP document together with its
// namespace-prefix table, so that table is built once and reused across
// however many decodes a caller performs on the same bytes.
//
// A server handling a request decodes the same document at least twice —
// once to identify the operation, then again into that operation's
// concrete request type — and buildPrefixTable is itself a full
// token-level scan. Going through a Document turns four whole-document
// parses per request into two.
type Document struct {
	raw   []byte
	table map[string]string
}

// NewDocument scans raw's namespace declarations, returning an error only
// if raw is not well-formed XML at all.
func NewDocument(raw []byte) (*Document, error) {
	table, err := buildPrefixTable(raw)
	if err != nil {
		return nil, err
	}
	return &Document{raw: raw, table: table}, nil
}

// Decode decodes doc into v, with doc's prefix scope available to every
// nested UnmarshalXML call (via resolveQName) for the decode's duration.
// It may be called more than once, with different target types.
func (doc *Document) Decode(v any) error {
	d := xml.NewDecoder(bytes.NewReader(doc.raw))
	defer withScope(d, doc.table)()
	return d.Decode(v)
}

// Decode is the top-level entry point for decoding a full OPC XML-DA/SOAP
// document into v. It builds the document's namespace-prefix scope once and
// makes it available to every nested UnmarshalXML call (via resolveQName)
// for the duration of the decode. Callers decoding the same bytes more
// than once should build a Document instead, to reuse that scope.
func Decode(raw []byte, v any) error {
	doc, err := NewDocument(raw)
	if err != nil {
		return err
	}
	return doc.Decode(v)
}

// attrValue returns the value of the first attribute in attrs whose
// resolved name matches name, and whether it was found.
func attrValue(attrs []xml.Attr, name xml.Name) (string, bool) {
	for _, a := range attrs {
		if a.Name.Local == name.Local && (name.Space == "" || a.Name.Space == name.Space) {
			return a.Value, true
		}
	}
	return "", false
}

// qnameAttr renders qn as a plain (non-namespace-prefixed-by-Go) attribute
// named local, e.g. ID="opc:E_FAIL". It always locally declares whatever
// namespace prefix it uses — the same self-contained, conventional-prefix
// approach as typeAttrs (see value.go), so the output is correct whether
// or not the surrounding document already declares a (possibly different)
// prefix for the same namespace. Used for every QName-shaped attribute
// value in this package: OPCError.ID, ItemValue/ItemProperty ResultID,
// ItemProperty.Name, and similar.
func qnameAttr(existing []xml.Attr, local string, qn QName) []xml.Attr {
	if qn.Space == "" {
		return []xml.Attr{
			{Name: xml.Name{Local: "xmlns"}, Value: ""},
			{Name: xml.Name{Local: local}, Value: qn.Local},
		}
	}
	prefix := prefixIn(existing, qn.Space)
	return []xml.Attr{
		{Name: xml.Name{Local: "xmlns:" + prefix}, Value: qn.Space},
		{Name: xml.Name{Local: local}, Value: prefix + ":" + qn.Local},
	}
}

// mergeAttrs appends add to base, dropping any namespace declaration
// (xmlns or xmlns:prefix) that base already carries with the identical
// URI. It is how every element in this package assembles its attribute
// list, and it exists because XML forbids duplicate attribute names on
// one element: typeAttrs, qnameAttr and marshalQNameScalar each declare
// whatever prefix they need locally (deliberately — see their doc
// comments), so an element needing two of them would otherwise emit e.g.
// xmlns:opc twice and produce a document no conforming parser accepts.
// That is not hypothetical: an ItemValue carrying both
// xsi:type="opc:ItemValue" and ResultID="opc:E_UNKNOWNITEMNAME" — the
// single most common per-item error shape in the protocol — hit it on
// every response. Go's own encoding/xml decoder tolerates duplicate
// attributes, which is why round-trip tests never caught it; expat,
// libxml2, .NET and JAXP all reject the document outright.
//
// A prefix already bound to a *different* URI is a genuine conflict
// rather than a redundancy, so it is passed through untouched: dropping
// it would silently change the QName's meaning. Preventing that conflict
// is prefixIn's job, not this function's — by the time a declaration
// reaches here the attribute value ("ext:E_VENDOR") has already been
// built around the prefix, so a rename at this point would break the
// value it belongs to. See prefixIn (value.go) for why the conflict is
// reachable at all: every non-standard namespace shares the prefix
// "ext".
//
// TestNoDuplicateAttributes (server/wireformat_test.go) enforces the
// invariant across every response shape, including the two-vendor-
// namespace case.
func mergeAttrs(base []xml.Attr, add ...xml.Attr) []xml.Attr {
	for _, a := range add {
		if isNamespaceDecl(a) && hasIdenticalDecl(base, a) {
			continue
		}
		base = append(base, a)
	}
	return base
}

// isNamespaceDecl reports whether a is an xmlns="…" or xmlns:prefix="…"
// declaration. This package builds those as a flat Local name (e.g.
// Local: "xmlns:opc") rather than via Name.Space, because encoding/xml's
// encoder writes attribute names verbatim from Local and would otherwise
// invent its own prefix.
func isNamespaceDecl(a xml.Attr) bool {
	return a.Name.Local == "xmlns" || strings.HasPrefix(a.Name.Local, "xmlns:")
}

// hasIdenticalDecl reports whether attrs already binds want's exact
// prefix to want's exact URI.
func hasIdenticalDecl(attrs []xml.Attr, want xml.Attr) bool {
	for _, a := range attrs {
		if a.Name.Local == want.Name.Local && a.Value == want.Value {
			return true
		}
	}
	return false
}

// encodePropertyNames writes each name in names as a "PropertyNames"
// element carrying its (possibly namespace-prefixed) text — the wire
// representation shared by BrowseRequest and GetPropertiesRequest.
func encodePropertyNames(e *xml.Encoder, names []QName) error {
	for _, pn := range names {
		pnStart := xml.StartElement{Name: xml.Name{Local: "PropertyNames"}}
		text := pn.Local
		if pn.Space != "" {
			prefix := prefixForNamespace(pn.Space)
			pnStart.Attr = append(pnStart.Attr, xml.Attr{Name: xml.Name{Local: "xmlns:" + prefix}, Value: pn.Space})
			text = prefix + ":" + pn.Local
		} else {
			// Explicitly declare "no default namespace" in this scope, the
			// same reason marshalQNameScalar (value.go) and qnameAttr above
			// do: without it, an unprefixed name here would resolve on
			// decode against whatever default namespace an ancestor
			// element happens to declare, silently changing this QName's
			// identity instead of round-tripping it unchanged.
			pnStart.Attr = append(pnStart.Attr, xml.Attr{Name: xml.Name{Local: "xmlns"}, Value: ""})
		}
		if err := e.EncodeToken(pnStart); err != nil {
			return err
		}
		if err := e.EncodeToken(xml.CharData(text)); err != nil {
			return err
		}
		if err := e.EncodeToken(pnStart.End()); err != nil {
			return err
		}
	}
	return nil
}

// decodePropertyNameElement decodes one <PropertyNames> element's text
// content into a QName, resolving any namespace prefix against d's scope.
// Shared by BrowseRequest.UnmarshalXML and GetPropertiesRequest.UnmarshalXML,
// the two operations that carry a PropertyNames list.
func decodePropertyNameElement(d *xml.Decoder, start xml.StartElement) (QName, error) {
	var holder struct {
		Text string `xml:",chardata"`
	}
	if err := d.DecodeElement(&holder, &start); err != nil {
		return QName{}, err
	}
	return resolveQName(d, strings.TrimSpace(holder.Text))
}

// decodeReturnFlags reads the ReturnAllProperties/ReturnPropertyValues/
// ReturnErrorText boolean attributes shared by BrowseRequest and
// GetPropertiesRequest. errText is a *bool (not bool, unlike all/values)
// because RequestOptions.ReturnErrorText's documented default is true —
// an absent attribute must be distinguishable from an explicit "false" so
// the default can actually apply, the same reason RequestOptions itself
// uses *bool fields.
func decodeReturnFlags(attrs []xml.Attr) (all, values bool, errText *bool, err error) {
	if v, ok := attrValue(attrs, xml.Name{Local: "ReturnAllProperties"}); ok {
		if all, err = strconv.ParseBool(strings.TrimSpace(v)); err != nil {
			return false, false, nil, fmt.Errorf("xmlda: invalid ReturnAllProperties %q: %w", v, err)
		}
	}
	if v, ok := attrValue(attrs, xml.Name{Local: "ReturnPropertyValues"}); ok {
		if values, err = strconv.ParseBool(strings.TrimSpace(v)); err != nil {
			return false, false, nil, fmt.Errorf("xmlda: invalid ReturnPropertyValues %q: %w", v, err)
		}
	}
	if v, ok := attrValue(attrs, xml.Name{Local: "ReturnErrorText"}); ok {
		b, perr := strconv.ParseBool(strings.TrimSpace(v))
		if perr != nil {
			return false, false, nil, fmt.Errorf("xmlda: invalid ReturnErrorText %q: %w", v, perr)
		}
		errText = &b
	}
	return all, values, errText, nil
}
