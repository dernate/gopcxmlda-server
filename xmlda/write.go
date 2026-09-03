package xmlda

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
)

// WriteItemList is Write's <ItemList> element. Items are represented as
// ItemValue directly — a write request item needs exactly ItemName,
// ItemPath, ClientItemHandle, Value (required), and optionally Quality/
// Timestamp (REQ-WRITE-003), which is precisely ItemValue's field set;
// ResultID/DiagnosticInfo (response-only concerns) are simply left at
// their zero value and omitted on encode. At least one item is required
// (REQ-WRITE-002), enforced by the server layer, not this type.
type WriteItemList struct {
	Params ItemParams
	Items  []ItemValue
}

// MarshalXML implements xml.Marshaler.
func (l WriteItemList) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Attr = mergeAttrs(start.Attr, encodeItemParamsAttrs(e, l.Params)...)
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
func (l *WriteItemList) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	p, err := decodeItemParamsAttrs(d, start.Attr)
	if err != nil {
		return err
	}
	l.Params = p
	items, err := decodeRepeatedElements[ItemValue](d, "Items", "ItemList")
	if err != nil {
		return err
	}
	l.Items = items
	return nil
}

// WriteRequest is the request for the Write operation (§3.4.1, p.51).
type WriteRequest struct {
	// ReturnValuesOnReply controls whether written values are echoed
	// back in the response (REQ-WRITE-001).
	ReturnValuesOnReply bool
	Options             RequestOptions
	ItemList            WriteItemList
}

// MarshalXML implements xml.Marshaler.
func (r WriteRequest) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Name = xml.Name{Local: "Write"}
	start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "ReturnValuesOnReply"}, Value: strconv.FormatBool(r.ReturnValuesOnReply)})
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	if err := e.EncodeElement(r.Options, xml.StartElement{Name: xml.Name{Local: "Options"}}); err != nil {
		return err
	}
	if err := e.EncodeElement(r.ItemList, xml.StartElement{Name: xml.Name{Local: "ItemList"}}); err != nil {
		return err
	}
	return e.EncodeToken(start.End())
}

// UnmarshalXML implements xml.Unmarshaler.
func (r *WriteRequest) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	if v, ok := attrValue(start.Attr, xml.Name{Local: "ReturnValuesOnReply"}); ok {
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("xmlda: invalid ReturnValuesOnReply %q: %w", v, err)
		}
		r.ReturnValuesOnReply = b
	}
	var shadow struct {
		Options  RequestOptions `xml:"Options"`
		ItemList WriteItemList  `xml:"ItemList"`
	}
	if err := d.DecodeElement(&shadow, &start); err != nil {
		return fmt.Errorf("xmlda: decoding Write request: %w", err)
	}
	r.Options = shadow.Options
	r.ItemList = shadow.ItemList
	return nil
}

// WriteResponse is the response for the Write operation (§3.4.2, pp.53-54).
// Value is present in RItemList's items only if ReturnValuesOnReply was
// true; Timestamp only if RequestOptions.ReturnItemTime was true.
type WriteResponse struct {
	XMLName   xml.Name      `xml:"http://opcfoundation.org/webservices/XMLDA/1.0/ WriteResponse"`
	Result    ReplyBase     `xml:"WriteResult"`
	RItemList ItemValueList `xml:"RItemList"`
	Errors    Errors        `xml:"Errors"`
}
