package xmlda

import (
	"encoding/xml"
	"fmt"
)

// ReadRequestItem is one requested item in a Read operation (§3.1.4,
// §3.3.1). Params carries the item-level hierarchical parameters
// (ItemPath, ReqType, MaxAge); see ItemParams.
type ReadRequestItem struct {
	Params           ItemParams
	ItemName         string
	ClientItemHandle string
}

// MarshalXML implements xml.Marshaler.
func (it ReadRequestItem) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	return marshalRequestItem(e, start, it.Params, it.ItemName, it.ClientItemHandle)
}

// UnmarshalXML implements xml.Unmarshaler.
func (it *ReadRequestItem) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	p, itemName, clientItemHandle, err := decodeRequestItem(d, start)
	if err != nil {
		return err
	}
	it.Params, it.ItemName, it.ClientItemHandle = p, itemName, clientItemHandle
	return nil
}

// ReadItemList is Read's <ItemList> element: list-level hierarchical
// Params plus the requested items. At least one item is required
// (REQ-READ-002); an empty list is a request-validation concern handled
// by the server layer, not by this type.
type ReadItemList struct {
	Params ItemParams
	Items  []ReadRequestItem
}

// MarshalXML implements xml.Marshaler.
func (l ReadItemList) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Attr = append(start.Attr, encodeItemParamsAttrs(l.Params)...)
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
func (l *ReadItemList) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	p, err := decodeItemParamsAttrs(d, start.Attr)
	if err != nil {
		return err
	}
	l.Params = p
	items, err := decodeRepeatedElements[ReadRequestItem](d, "Items", "ItemList")
	if err != nil {
		return err
	}
	l.Items = items
	return nil
}

// ReadRequest is the request for the Read operation (§3.3.1, p.47). Params
// carries the request-level hierarchical parameters.
type ReadRequest struct {
	Params   ItemParams
	Options  RequestOptions
	ItemList ReadItemList
}

// MarshalXML implements xml.Marshaler.
func (r ReadRequest) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Name = xml.Name{Local: "Read"}
	start.Attr = append(start.Attr, encodeItemParamsAttrs(r.Params)...)
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
func (r *ReadRequest) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	p, err := decodeItemParamsAttrs(d, start.Attr)
	if err != nil {
		return err
	}
	r.Params = p
	var shadow struct {
		Options  RequestOptions `xml:"Options"`
		ItemList ReadItemList   `xml:"ItemList"`
	}
	if err := d.DecodeElement(&shadow, &start); err != nil {
		return fmt.Errorf("xmlda: decoding Read request: %w", err)
	}
	r.Options = shadow.Options
	r.ItemList = shadow.ItemList
	return nil
}

// ReadResponse is the response for the Read operation (§3.3.2, pp.48-50).
// RItemList's item count and order must match the request
// (REQ-READ-003).
type ReadResponse struct {
	XMLName   xml.Name      `xml:"ReadResponse"`
	Result    ReplyBase     `xml:"ReadResult"`
	RItemList ItemValueList `xml:"RItemList"`
	Errors    Errors        `xml:"Errors"`
}
