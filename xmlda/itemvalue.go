package xmlda

import (
	"encoding/xml"
	"fmt"
	"time"
)

// ItemValueList is the shared <RItemList> shape used by Read and Write
// responses: the specification's ReplyItemList.
//
// It is emitted with xsi:type="opc:ReplyItemList" and an empty Reserved
// attribute, matching the real captured traffic in
// testdata/responses/read_182.response.xml. Neither carries information:
// Reserved exists purely to stop certain WSDL code generators from
// flattening the wrapper into a bare array, and the xsi:type is
// redundant with the element name. Both are emitted anyway for the same
// reason ReplyBase, ItemValue and OPCQuality already declare theirs —
// strict and .NET-generated clients may expect the type annotation, and
// applying that convention to three of the four wrapper types while
// silently skipping the fourth is the kind of inconsistency that only
// shows up against a client nobody tested with.
type ItemValueList struct {
	Items []ItemValue
}

// MarshalXML implements xml.Marshaler.
func (l ItemValueList) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Attr = mergeAttrs(start.Attr, typeAttrs(start.Attr, QName{Space: Namespace, Local: "ReplyItemList"})...)
	start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "Reserved"}, Value: ""})
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	for _, it := range l.Items {
		if err := e.EncodeElement(it, xml.StartElement{Name: xml.Name{Local: "Items"}}); err != nil {
			return err
		}
	}
	return e.EncodeToken(start.End())
}

// UnmarshalXML implements xml.Unmarshaler.
func (l *ItemValueList) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	items, err := decodeRepeatedElements[ItemValue](d, "Items", "RItemList")
	if err != nil {
		return err
	}
	l.Items = items
	return nil
}

// ItemValue is the wire shape for a data-bearing item, used by
// Read/Write/Subscribe/SubscriptionPolledRefresh results (§3.1.5, pp.30-33)
// and, for Write, by the request as well.
type ItemValue struct {
	// ItemName identifies the item.
	ItemName string
	// ItemPath, together with ItemName, identifies the item. nil vs. a
	// pointer to "" is meaningful — see ItemParams's doc comment.
	ItemPath *string
	// ClientItemHandle is the client-assigned handle, echoed back.
	ClientItemHandle string
	// Value is nil if no <Value> element is present at all (e.g. a
	// write-only item on Read, or Bad quality with no last-known value).
	// A present Value that itself reports IsNil() true represents an
	// explicit xsi:nil="true" element (a known type with no value); these
	// are two distinct "no value" representations — see REQ-TYPE-008.
	Value *Value
	// Quality is this item's quality, or nil if it carries none.
	//
	// A pointer, not a value, for two independent reasons.
	//
	// On encode: an item that carries no sample at all (an unknown item
	// name, a write-only item read back) must not assert a quality.
	// <Quality> is minOccurs="0" in the schema, and the zero OPCQuality
	// emits no attributes at all — which, under the schema's OWN
	// attribute defaults, a conforming client reads as
	// QualityField="good". A failing item was therefore reported as
	// good-quality-with-no-value, contradicting its own
	// ResultID="opc:E_UNKNOWNITEMNAME"; for a client that maps this field
	// onto OPC DA's wQuality — the usual shape of a DA/XML-DA bridge —
	// the quality is the half that reaches the process image.
	//
	// On decode: nil distinguishes "no <Quality> element" from "<Quality/>
	// carrying no attributes", which is a legitimate way for a client to
	// write good/none/0 explicitly. Comparing a value-typed OPCQuality
	// against its zero could not tell those apart (its fields are
	// pointers, so the comparison is pointer identity), so an explicit
	// empty <Quality/> in a Write request was silently dropped instead of
	// being applied.
	Quality *OPCQuality
	// Timestamp is nil if not present (gated by quality and by
	// RequestOptions.ReturnItemTime — see ResolveValuePresence and
	// docs/architecture/data-flow.md).
	Timestamp *time.Time
	// ResultID is set if this item carries an abnormal condition; the
	// zero ErrorCode means none.
	ResultID ErrorCode
	// DiagnosticInfo is verbose, non-deduplicated per-item diagnostic
	// text, populated only if RequestOptions.ReturnDiagnosticInfo is true.
	DiagnosticInfo string
	// DecodeErr is non-nil when this item was structurally readable but
	// one of its own attributes or its <Value> could not be interpreted
	// (see ItemDecodeError). Used on the request side (Write): the server
	// layer turns it into this one item's ResultID rather than failing the
	// whole operation. Always an *ItemDecodeError when non-nil.
	DecodeErr error
}

// QualityOrDefault returns iv's quality, or the specification's wire
// default (good/none/0) when the item carried no <Quality> element —
// which is what a conforming client must assume for an absent one
// (REQ-QUALITY-002). Use this rather than dereferencing Quality, which is
// nil for an item that reports a condition instead of a value.
func (iv ItemValue) QualityOrDefault() OPCQuality {
	if iv.Quality == nil {
		return OPCQuality{}
	}
	return *iv.Quality
}

// MarshalXML implements xml.Marshaler. It always adds an xsi:type
// attribute declaring this element as ItemValue, matching the real-world
// wire pattern observed in testdata/responses/subscribe_680.response.xml
// (ItemValue, OPCQuality, and ReplyBase are all emitted this way — likely
// because the schema declares them as members of a polymorphic type,
// which strict/.NET-generated clients may expect xsi:type to disambiguate).
func (iv ItemValue) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Attr = mergeAttrs(start.Attr, typeAttrs(start.Attr, QName{Space: Namespace, Local: "ItemValue"})...)
	if iv.ItemName != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "ItemName"}, Value: iv.ItemName})
	}
	if iv.ItemPath != nil {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "ItemPath"}, Value: *iv.ItemPath})
	}
	if iv.ClientItemHandle != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "ClientItemHandle"}, Value: iv.ClientItemHandle})
	}
	if !iv.ResultID.IsZero() {
		start.Attr = mergeAttrs(start.Attr, qnameAttr(start.Attr, "ResultID", iv.ResultID.QName)...)
	}
	if iv.Timestamp != nil {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "Timestamp"}, Value: formatWireTime(*iv.Timestamp)})
	}
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	// Child order is the schema's own xsd:sequence — DiagnosticInfo,
	// Value, Quality (§3.1.5) — and a sequence is ordered, so a
	// schema-validating or sequence-position-decoding client rejects any
	// other order. The real captured traffic in
	// testdata/responses/subscribe_680.response.xml agrees: Value then
	// Quality.
	if iv.DiagnosticInfo != "" {
		// DiagnosticInfo is an ELEMENT in the schema, not an attribute:
		// <s:element minOccurs="0" maxOccurs="1" name="DiagnosticInfo"
		// type="s:string"/>. It used to be emitted as an attribute, which
		// no schema-bound client would find.
		if err := e.EncodeElement(iv.DiagnosticInfo, xml.StartElement{Name: xml.Name{Local: "DiagnosticInfo"}}); err != nil {
			return err
		}
	}
	if iv.Value != nil {
		if err := e.EncodeElement(*iv.Value, xml.StartElement{Name: xml.Name{Local: "Value"}}); err != nil {
			return err
		}
	}
	// Omitted entirely when nil: see Quality's doc comment on why an item
	// with no sample must not emit one.
	if iv.Quality != nil {
		if err := e.EncodeElement(*iv.Quality, xml.StartElement{Name: xml.Name{Local: "Quality"}}); err != nil {
			return err
		}
	}
	return e.EncodeToken(start.End())
}

// UnmarshalXML implements xml.Unmarshaler.
//
// It reads the children with its own token loop rather than through a
// struct-tag shadow, for two reasons that both come back to keeping one
// bad item from destroying a whole request. First, it lets a rejected
// <Value> or attribute be recorded in DecodeErr while decoding continues:
// encoding/xml's own field decoding aborts the enclosing element on the
// first field error, leaving the token stream mid-element so the
// surrounding <Items> loop cannot recover. Second, the loop consumes
// through this element's end tag on every path, which is the invariant
// the surrounding loop depends on (Value.UnmarshalXML and
// OPCQuality.UnmarshalXML guarantee the same for themselves).
//
// It additionally tolerates a peer that encodes time/date/duration as
// dateTime/string plus a ValueTypeQualifier attribute (an interop
// accommodation some toolchains use, per §2.7.1) rather than this
// library's own direct xsi:type encoding — see
// docs/specification/open-questions.md OQ-12.
func (iv *ItemValue) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	// --- attributes: all read from start.Attr, so nothing here can leave
	// the decoder in an unexpected position. ---
	iv.ItemName, _ = attrValue(start.Attr, xml.Name{Local: "ItemName"})
	if v, ok := attrValue(start.Attr, xml.Name{Local: "ItemPath"}); ok {
		iv.ItemPath = &v
	}
	iv.ClientItemHandle, _ = attrValue(start.Attr, xml.Name{Local: "ClientItemHandle"})
	// The attribute spelling of DiagnosticInfo is tolerated because this
	// library itself emitted that form until the encoder was corrected to
	// the schema's element, so a peer (or a stored fixture) may still
	// carry it. An element, if present, wins.
	iv.DiagnosticInfo, _ = attrValue(start.Attr, xml.Name{Local: "DiagnosticInfo"})

	var decodeErr error
	record := func(field string, code ErrorCode, err error) {
		if decodeErr == nil {
			decodeErr = &ItemDecodeError{Field: field, Code: code, Err: err}
		}
	}

	if raw, ok := attrValue(start.Attr, xml.Name{Local: "Timestamp"}); ok {
		// parseXSDDateTime, not a time.Time struct field: xsd:dateTime's
		// timezone offset is optional and time.Time.UnmarshalText's is
		// not, so a conforming Write carrying
		// Timestamp="2026-08-30T12:00:00" used to fault the entire
		// request. See wireTime (replybase.go).
		t, err := parseXSDDateTime(raw)
		if err != nil {
			record("Timestamp", ErrFail, err)
		} else {
			iv.Timestamp = &t
		}
	}
	if raw, ok := attrValue(start.Attr, xml.Name{Local: "ResultID"}); ok {
		rid, err := resolveQNameIn(d, start.Attr, raw)
		if err != nil {
			record("ResultID", ErrFail, err)
		} else {
			iv.ResultID = ErrorCode{rid}
		}
	}
	qualifierRaw, hasQualifier := attrValue(start.Attr, xml.Name{Local: "ValueTypeQualifier"})

	// --- children ---
	for done := false; !done; {
		tok, err := d.Token()
		if err != nil {
			return fmt.Errorf("xmlda: decoding ItemValue: %w", err)
		}
		switch t := tok.(type) {
		case xml.EndElement:
			done = true
		case xml.StartElement:
			switch t.Name.Local {
			case "DiagnosticInfo":
				text, err := elementText(d, t)
				if err != nil {
					return err
				}
				if text != "" {
					iv.DiagnosticInfo = text
				}
			case "Value":
				var v Value
				if err := v.UnmarshalXML(d, t); err != nil {
					// E_BADTYPE: a <Value> whose content does not match
					// its declared xsi:type, or that declares none at
					// all, is exactly the specification's bad-type
					// condition for that one item.
					record("Value", ErrBadType, err)
					continue
				}
				iv.Value = &v
			case "Quality":
				var q OPCQuality
				if err := q.UnmarshalXML(d, t); err != nil {
					record("Quality", ErrFail, err)
					continue
				}
				iv.Quality = &q
			default:
				if err := d.Skip(); err != nil {
					return err
				}
			}
		}
	}

	if hasQualifier && iv.Value != nil {
		if err := applyValueTypeQualifier(iv.Value, qualifierRaw, d, start.Attr); err != nil {
			record("ValueTypeQualifier", ErrBadType, err)
		}
	}
	iv.DecodeErr = decodeErr
	return nil
}

// applyValueTypeQualifier reinterprets v's semantic scalar type per a
// ValueTypeQualifier attribute value (OQ-12): a dateTime-typed v becomes
// time or date, and a string-typed v becomes duration, according to the
// qualifier's local name. Any other qualifier, or a v whose current type
// doesn't match the expected wire-base type for that qualifier, is left
// unchanged — this is a tolerant best-effort reinterpretation, not a
// strict validation.
func applyValueTypeQualifier(v *Value, qualifierRaw string, d *xml.Decoder, elemAttrs []xml.Attr) error {
	qn, err := resolveQNameIn(d, elemAttrs, qualifierRaw)
	if err != nil {
		return err
	}
	if qn.Space != XSDNamespace {
		return nil
	}
	switch qn.Local {
	case "time":
		if v.typ == TypeDateTime {
			v.typ = TypeTime
			v.typeName = QName{XSDNamespace, "time"}
		}
	case "date":
		if v.typ == TypeDateTime {
			v.typ = TypeDate
			v.typeName = QName{XSDNamespace, "date"}
		}
	case "duration":
		if v.typ == TypeString {
			v.typ = TypeDuration
			v.typeName = QName{XSDNamespace, "duration"}
		}
	}
	return nil
}
