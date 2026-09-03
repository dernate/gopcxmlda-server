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
	"maps"
	"slices"
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

// prefixScope is one decoded document's namespace-prefix context: the
// flattened whole-document table, used as the fallback when a QName's
// prefix is not declared on the element the value itself came from (see
// resolveQNameIn).
type prefixScope struct {
	table map[string]string
}

// decoderScopes associates a *xml.Decoder instance with the prefixScope
// built for the document it is decoding.
//
// Keying on the decoder pointer is forced by xml.Unmarshaler's signature:
// UnmarshalXML receives only (*xml.Decoder, xml.StartElement), with no
// room for caller-supplied context, and a nested value's xsi:type or
// ResultID cannot be resolved without the document's declarations. Two
// facts make it safe: encoding/xml passes the same *Decoder pointer
// through every nested UnmarshalXML call within one Decode invocation,
// and each top-level Decode uses a fresh *Decoder — so entries never
// bleed between unrelated or concurrent decodes. The entry is removed by
// the cleanup function withScope returns, which every entry point defers
// (so it runs on a panicking decode too, and no stale entry can outlive
// its decoder and be inherited by a later one allocated at the same
// address).
var decoderScopes sync.Map // map[*xml.Decoder]*prefixScope

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
func buildPrefixTable(raw []byte, maxDepth int) (map[string]string, xml.Name, string, error) {
	table := map[string]string{"xml": xmlNamespace}
	d := newDecoder(raw)
	depth := 0
	// path records the local names of the currently-open elements, so this
	// pass can also pick out the SOAP Body's first child — the operation
	// name. It is the only thing IdentifyOperation ever needed, and
	// tokenizing the entire document a second time to learn it cost 1.8 ms
	// and 293 KB on a 1000-item Read, roughly a ninth of the whole
	// request. This pass already visits every start element.
	var path []string
	var op xml.Name
	var envNS string
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, xml.Name{}, "", fmt.Errorf("xmlda: scanning namespace declarations: %w", err)
		}
		if _, ok := tok.(xml.EndElement); ok {
			depth--
			if len(path) > 0 {
				path = path[:len(path)-1]
			}
			continue
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		depth++
		if depth == 1 {
			envNS = se.Name.Space
		}
		// Envelope / Body / operation: the first grandchild of a Body that
		// is itself a child of the root. Matching on the local name alone
		// keeps this prefix-independent, exactly as the typed decode is.
		if op.Local == "" && len(path) == 2 && path[1] == "Body" {
			op = se.Name
		}
		path = append(path, se.Name.Local)
		// The depth check rides along on this pass because it is the first
		// thing that touches the document, and every protocol limit —
		// MaxItemsPerRequest above all — only applies after a full decode.
		// Nesting costs memory super-linearly in the decoder's own
		// bookkeeping: at the default 4 MiB body limit, ~600 000 nested
		// elements turned one request into ~128 MiB of live heap and a
		// second of CPU, which is an amplification MaxRequestBodyBytes
		// cannot see. No conforming OPC XML-DA document comes close to
		// the default ceiling.
		if maxDepth > 0 && depth > maxDepth {
			return nil, xml.Name{}, "", fmt.Errorf("xmlda: element nesting exceeds the maximum depth of %d", maxDepth)
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
	return table, op, envNS, nil
}

// encoderScopes associates an *xml.Encoder with the namespace prefixes an
// ANCESTOR element has already declared, so the elements below it need not
// declare them again.
//
// It is the encode-side twin of decoderScopes and exists for the same
// reason: xml.Marshaler's signature is MarshalXML(*xml.Encoder,
// xml.StartElement) and leaves no room for caller-supplied context, while
// the decision "does this element still have to declare xmlns:opc" needs
// exactly that. Keying on the encoder pointer is safe for the same two
// reasons as on the decoder side: encoding/xml threads one *Encoder
// through every nested MarshalXML of a single Encode, and each top-level
// Encode uses a fresh one.
//
// Without it every element declared its own prefixes, because none could
// know what its parent had done. That is correct XML and it is what makes
// a standalone xmlda.Value self-contained — but inside a full response it
// meant 6004 xmlns declarations for a 1000-item Read, 62 % of the bytes on
// the wire.
var encoderScopes sync.Map // map[*xml.Encoder]map[string]string  (URI -> prefix)

// DeclareAncestorNamespaces records that an enclosing element of whatever
// e writes next already declares decls (a URI -> prefix map), so this
// package's marshalers can reference those prefixes without redeclaring
// them. It returns a cleanup function the caller must defer.
//
// The caller is responsible for actually emitting the declarations on that
// ancestor; getting that wrong produces a document with unbound prefixes.
// In this library there is exactly one such caller — the response writer,
// which is also the one place that knows both the SOAP envelope and the
// OPC XML-DA namespaces.
func DeclareAncestorNamespaces(e *xml.Encoder, decls map[string]string) func() {
	if len(decls) == 0 {
		return func() {}
	}
	cp := make(map[string]string, len(decls))
	maps.Copy(cp, decls)
	encoderScopes.Store(e, cp)
	return func() { encoderScopes.Delete(e) }
}

// ancestorPrefix reports the prefix an ancestor already bound to space,
// if any.
func ancestorPrefix(e *xml.Encoder, space string) (string, bool) {
	if e == nil || space == "" {
		return "", false
	}
	v, ok := encoderScopes.Load(e)
	if !ok {
		return "", false
	}
	prefix, ok := v.(map[string]string)[space]
	return prefix, ok
}

// ResponseNamespaces is the set of namespaces this package's response
// marshalers reference, in the conventional prefixes they use. A caller
// that declares exactly these on an enclosing element and passes them to
// DeclareAncestorNamespaces gets the same document with each declaration
// written once instead of once per element.
func ResponseNamespaces() map[string]string {
	return map[string]string{
		Namespace:    "opc",
		XSDNamespace: "xsd",
		XSINamespace: "xsi",
	}
}

// withScope registers table as the prefix scope for d and returns a cleanup
// function that must be called (via defer) once the top-level decode using d
// has returned, to prevent the map from growing unboundedly across many
// decodes.
func withScope(d *xml.Decoder, table map[string]string) func() {
	decoderScopes.Store(d, &prefixScope{table: table})
	return func() { decoderScopes.Delete(d) }
}

// localPrefixBinding reports the URI attrs binds prefix to, if any.
// prefix "" asks about the default namespace (xmlns="…"), which may
// legitimately be bound to the empty URI as an explicit reset — hence the
// second return value rather than an empty-string sentinel.
func localPrefixBinding(attrs []xml.Attr, prefix string) (string, bool) {
	for _, a := range attrs {
		switch {
		case prefix != "" && a.Name.Space == "xmlns" && a.Name.Local == prefix:
			return a.Value, true
		case prefix == "" && a.Name.Space == "" && a.Name.Local == "xmlns":
			return a.Value, true
		}
	}
	return "", false
}

// resolveQNameIn resolves raw ("prefix:local", or a bare local name)
// against, in order: the namespace declarations carried on elemAttrs —
// the attribute list of the very element the value came from — and then
// the document's flattened prefix table.
//
// Element-local declarations win because that is what XML namespace
// scoping specifies, and because it is the shape both the specification's
// own worked example (§2.6 p.21: xmlns:q0 declared directly on the
// element) and this library's own encoder produce: typeAttrs, qnameAttr
// and marshalQNameScalar each declare the prefix they use locally. Before
// this, a prefix rebound on the element itself was resolved against the
// whole-document "last declaration wins" table instead, so two elements
// legitimately binding the same prefix to different vendor namespaces —
// which §3.1.9 and §3.1.10 positively invite, since a vendor result code
// and a vendor property need not come from the same vendor — both
// resolved to whichever declaration appeared last in the document.
//
// The flat table remains the fallback for a prefix declared on an
// ancestor; see docs/specification/open-questions.md OQ-6 for the
// residual limitation there.
func resolveQNameIn(d *xml.Decoder, elemAttrs []xml.Attr, raw string) (QName, error) {
	prefix, local, hasPrefix := strings.Cut(strings.TrimSpace(raw), ":")
	if !hasPrefix {
		local, prefix = prefix, ""
	}
	if uri, ok := localPrefixBinding(elemAttrs, prefix); ok {
		switch {
		case uri != "":
			return QName{Space: uri, Local: local}, nil
		case prefix == "":
			// xmlns="" — an explicit "no default namespace" reset, which
			// this library's own encoder emits for an unqualified QName.
			return QName{Local: local}, nil
		default:
			return QName{}, fmt.Errorf("xmlda: unresolvable QName %q: prefix %q is locally bound to the empty namespace", raw, prefix)
		}
	}
	return resolveQName(d, raw)
}

// resolveQName is the single place any "prefix:local" wire text is ever
// interpreted. It resolves raw (an xsi:type value, a ResultID value, an
// ItemProperty Name, ...) against the prefix scope registered for d via
// withScope. Everywhere else in this package, QName equality is by
// (Space, Local) only — never by comparing prefixed strings.
func resolveQName(d *xml.Decoder, raw string) (QName, error) {
	prefix, local, hasPrefix := strings.Cut(strings.TrimSpace(raw), ":")
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
	ps, ok := scope.(*prefixScope)
	if !ok {
		return QName{}, fmt.Errorf("xmlda: prefix scope for this decoder holds %T, not a *prefixScope "+
			"(internal inconsistency)", scope)
	}
	table := ps.table
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
	// operation is the resolved name of the SOAP Body's first child,
	// picked up by the same pass that built the prefix table. The zero
	// Name means the document has no Body child at all.
	operation xml.Name
	// envelopeNS is the namespace of the document's root element, from
	// the same pass. It is what tells a caller which SOAP version the
	// peer spoke; this package does not interpret it.
	envelopeNS string
}

// EnvelopeNamespace returns the namespace URI of the document's root
// element — for a SOAP envelope, the one that says which SOAP version the
// peer spoke. Empty when the root carries no namespace.
func (doc *Document) EnvelopeNamespace() string { return doc.envelopeNS }

// NewDocument scans raw's namespace declarations, returning an error only
// if raw is not well-formed XML at all.
func NewDocument(raw []byte) (*Document, error) {
	return NewDocumentLimited(raw, DefaultMaxElementDepth)
}

// DefaultMaxElementDepth is the element-nesting ceiling NewDocument
// applies. It is a resource limit, not a protocol one: no conforming OPC
// XML-DA document nests anywhere near this deep (the deepest shape in the
// schema is Envelope/Body/Operation/ItemList/Items/Value/element, seven
// levels), while a body of nothing but nested start tags costs memory
// super-linearly in encoding/xml's own bookkeeping — an amplification
// MaxRequestBodyBytes cannot see, because it bounds the input rather than
// what the input costs.
const DefaultMaxElementDepth = 64

// NewDocumentLimited is NewDocument with an explicit nesting ceiling.
// A maxDepth of zero or less disables the check.
func NewDocumentLimited(raw []byte, maxDepth int) (*Document, error) {
	table, op, envNS, err := buildPrefixTable(raw, maxDepth)
	if err != nil {
		return nil, err
	}
	return &Document{raw: raw, table: table, operation: op, envelopeNS: envNS}, nil
}

// Decode decodes doc into v, with doc's prefix scope available to every
// nested UnmarshalXML call (via resolveQName) for the decode's duration.
// It may be called more than once, with different target types.
func (doc *Document) Decode(v any) error {
	d := newDecoder(doc.raw)
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

// newDecoder returns the decoder every entry point in this package uses:
// one that can read the encodings a real peer sends, not only UTF-8 (see
// newCharsetReader).
func newDecoder(raw []byte) *xml.Decoder {
	d := xml.NewDecoder(bytes.NewReader(transcodeToUTF8(raw)))
	d.CharsetReader = newCharsetReader
	return d
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
func qnameAttr(e *xml.Encoder, existing []xml.Attr, local string, qn QName) []xml.Attr {
	if qn.Space == "" {
		return unqualifiedQNameAttr(local, qn.Local)
	}
	if prefix, ok := ancestorPrefix(e, qn.Space); ok {
		return []xml.Attr{{Name: xml.Name{Local: local}, Value: prefix + ":" + qn.Local}}
	}
	prefix := prefixIn(existing, qn.Space)
	return []xml.Attr{
		{Name: xml.Name{Local: "xmlns:" + prefix}, Value: qn.Space},
		{Name: xml.Name{Local: local}, Value: prefix + ":" + qn.Local},
	}
}

// unqualifiedQNameAttr renders a QName that carries no namespace, as an
// attribute value.
//
// No xmlns="" reset, and that is not a shortcut: an unprefixed QName in
// an ATTRIBUTE value has no namespace by definition — unlike one in
// element content, attributes are not covered by the default namespace
// (Namespaces in XML 1.0 §6.2, "the default namespace does not apply to
// attribute names", and by extension to QName-typed attribute values).
// So the reset changed nothing about how the value reads, while doing
// real damage: xmlns="" applies to the whole carrier element, so a single
// vendor property with no namespace pulled its <Properties> element out
// of the OPC XML-DA namespace and made the ENTIRE response fail schema
// validation. A vendor name still belongs in a vendor namespace
// (§3.1.10) — that is what the warning at the call site is for — but a
// backend that omits one must not cost every other item its response.
func unqualifiedQNameAttr(local, value string) []xml.Attr {
	return []xml.Attr{{Name: xml.Name{Local: local}, Value: value}}
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
	base = slices.Grow(base, len(add))
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

// elementText decodes start's character-data content, consuming through
// its end tag. Shared by every place this package needs a plain text
// element rather than a typed one.
func elementText(d *xml.Decoder, start xml.StartElement) (string, error) {
	var holder struct {
		Text string `xml:",chardata"`
	}
	if err := d.DecodeElement(&holder, &start); err != nil {
		return "", fmt.Errorf("xmlda: decoding <%s>: %w", start.Name.Local, err)
	}
	return holder.Text, nil
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
	return resolveQNameIn(d, start.Attr, holder.Text)
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
