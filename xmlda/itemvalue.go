package xmlda

import (
	"encoding/xml"
	"fmt"
	"time"
)

// ItemValueList is the shared <RItemList> shape used by Read and Write
// responses: a bare container of ItemValue. (The specification's
// ReplyItemList also carries a "Reserved" attribute solely to defeat
// certain WSDL code generators from flattening it into an array type —
// a tooling artifact with no semantic value, deliberately not modeled
// here.)
type ItemValueList struct {
	Items []ItemValue `xml:"Items"`
}

// ItemValue is the wire shape for a data-bearing item, used by
// Read/Write/Subscribe/SubscriptionPolledRefresh results (§3.1.5, pp.30-33).
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
	// Quality is this item's quality. The zero OPCQuality is Good/None/0.
	Quality OPCQuality
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
}

// MarshalXML implements xml.Marshaler. It always adds an xsi:type
// attribute declaring this element as ItemValue, matching the real-world
// wire pattern observed in testdata/responses/subscribe_680.response.xml
// (ItemValue, OPCQuality, and ReplyBase are all emitted this way — likely
// because the schema declares them as members of a polymorphic type,
// which strict/.NET-generated clients may expect xsi:type to disambiguate).
func (iv ItemValue) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Attr = append(start.Attr, typeAttrs(QName{Space: Namespace, Local: "ItemValue"})...)
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
		start.Attr = append(start.Attr, qnameAttr("ResultID", iv.ResultID.QName)...)
	}
	if iv.DiagnosticInfo != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "DiagnosticInfo"}, Value: iv.DiagnosticInfo})
	}
	if iv.Timestamp != nil {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "Timestamp"}, Value: iv.Timestamp.Format(time.RFC3339Nano)})
	}
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	if err := e.EncodeElement(iv.Quality, xml.StartElement{Name: xml.Name{Local: "Quality"}}); err != nil {
		return err
	}
	if iv.Value != nil {
		if err := e.EncodeElement(*iv.Value, xml.StartElement{Name: xml.Name{Local: "Value"}}); err != nil {
			return err
		}
	}
	return e.EncodeToken(start.End())
}

// UnmarshalXML implements xml.Unmarshaler. It additionally tolerates a
// peer that encodes time/date/duration as dateTime/string plus a
// ValueTypeQualifier attribute (an interop accommodation some toolchains
// use, per §2.7.1) rather than this library's own direct xsi:type
// encoding — see docs/specification/open-questions.md OQ-12.
func (iv *ItemValue) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	resultRaw, hasResult := attrValue(start.Attr, xml.Name{Local: "ResultID"})
	qualifierRaw, hasQualifier := attrValue(start.Attr, xml.Name{Local: "ValueTypeQualifier"})

	var shadow struct {
		ItemName         string     `xml:"ItemName,attr"`
		ItemPath         *string    `xml:"ItemPath,attr"`
		ClientItemHandle string     `xml:"ClientItemHandle,attr"`
		DiagnosticInfo   string     `xml:"DiagnosticInfo,attr"`
		Timestamp        *time.Time `xml:"Timestamp,attr"`
		Quality          OPCQuality `xml:"Quality"`
		Value            *Value     `xml:"Value"`
	}
	if err := d.DecodeElement(&shadow, &start); err != nil {
		return fmt.Errorf("xmlda: decoding ItemValue: %w", err)
	}

	iv.ItemName = shadow.ItemName
	iv.ItemPath = shadow.ItemPath
	iv.ClientItemHandle = shadow.ClientItemHandle
	iv.DiagnosticInfo = shadow.DiagnosticInfo
	iv.Timestamp = shadow.Timestamp
	iv.Quality = shadow.Quality
	iv.Value = shadow.Value

	if hasResult {
		rid, err := resolveQName(d, resultRaw)
		if err != nil {
			return err
		}
		iv.ResultID = ErrorCode{rid}
	}
	if hasQualifier && iv.Value != nil {
		if err := applyValueTypeQualifier(iv.Value, qualifierRaw, d); err != nil {
			return err
		}
	}
	return nil
}

// applyValueTypeQualifier reinterprets v's semantic scalar type per a
// ValueTypeQualifier attribute value (OQ-12): a dateTime-typed v becomes
// time or date, and a string-typed v becomes duration, according to the
// qualifier's local name. Any other qualifier, or a v whose current type
// doesn't match the expected wire-base type for that qualifier, is left
// unchanged — this is a tolerant best-effort reinterpretation, not a
// strict validation.
func applyValueTypeQualifier(v *Value, qualifierRaw string, d *xml.Decoder) error {
	qn, err := resolveQName(d, qualifierRaw)
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
