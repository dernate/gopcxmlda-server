package xmlda

import (
	"encoding/xml"
	"fmt"
	"strconv"
)

// ItemIdentifier identifies one item by ItemPath+ItemName, used by
// GetProperties requests (§3.1.3-ish; §3.9.1).
type ItemIdentifier struct {
	ItemPath *string
	ItemName string
}

// GetPropertiesRequest is the request for the GetProperties operation
// (§3.9.1, pp.76-77).
type GetPropertiesRequest struct {
	LocaleID             string
	ClientRequestHandle  string
	ItemPath             *string // hierarchical default, overridable per ItemIdentifier
	ReturnAllProperties  bool
	ReturnPropertyValues bool
	// ReturnErrorText requests human-readable Errors text. Default: true
	// (see ReturnErrorTextOrDefault) — a pointer so "attribute absent" is
	// distinguishable from "explicitly false", the same reason
	// RequestOptions.ReturnErrorText is a pointer.
	ReturnErrorText *bool
	ItemIDs         []ItemIdentifier
	// PropertyNames filters which properties to return; ignored if
	// ReturnAllProperties is true.
	PropertyNames []QName
}

// ReturnErrorTextOrDefault returns ReturnErrorText, or its default (true)
// if unset.
func (r GetPropertiesRequest) ReturnErrorTextOrDefault() bool {
	return returnErrorTextOrDefault(r.ReturnErrorText)
}

// MarshalXML implements xml.Marshaler.
func (r GetPropertiesRequest) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Name = xml.Name{Local: "GetProperties"}
	if r.LocaleID != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "LocaleID"}, Value: r.LocaleID})
	}
	if r.ClientRequestHandle != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "ClientRequestHandle"}, Value: r.ClientRequestHandle})
	}
	if r.ItemPath != nil {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "ItemPath"}, Value: *r.ItemPath})
	}
	start.Attr = append(start.Attr,
		xml.Attr{Name: xml.Name{Local: "ReturnAllProperties"}, Value: strconv.FormatBool(r.ReturnAllProperties)},
		xml.Attr{Name: xml.Name{Local: "ReturnPropertyValues"}, Value: strconv.FormatBool(r.ReturnPropertyValues)},
	)
	if r.ReturnErrorText != nil {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "ReturnErrorText"}, Value: strconv.FormatBool(*r.ReturnErrorText)})
	}
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	for _, id := range r.ItemIDs {
		idStart := xml.StartElement{Name: xml.Name{Local: "ItemIDs"}}
		if id.ItemPath != nil {
			idStart.Attr = append(idStart.Attr, xml.Attr{Name: xml.Name{Local: "ItemPath"}, Value: *id.ItemPath})
		}
		if id.ItemName != "" {
			idStart.Attr = append(idStart.Attr, xml.Attr{Name: xml.Name{Local: "ItemName"}, Value: id.ItemName})
		}
		if err := e.EncodeToken(idStart); err != nil {
			return err
		}
		if err := e.EncodeToken(idStart.End()); err != nil {
			return err
		}
	}
	if err := encodePropertyNames(e, r.PropertyNames); err != nil {
		return err
	}
	return e.EncodeToken(start.End())
}

// UnmarshalXML implements xml.Unmarshaler.
func (r *GetPropertiesRequest) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	r.LocaleID, _ = attrValue(start.Attr, xml.Name{Local: "LocaleID"})
	r.ClientRequestHandle, _ = attrValue(start.Attr, xml.Name{Local: "ClientRequestHandle"})
	if v, ok := attrValue(start.Attr, xml.Name{Local: "ItemPath"}); ok {
		r.ItemPath = &v
	}
	var err error
	if r.ReturnAllProperties, r.ReturnPropertyValues, r.ReturnErrorText, err = decodeReturnFlags(start.Attr); err != nil {
		return err
	}

	for {
		tok, err := d.Token()
		if err != nil {
			return fmt.Errorf("xmlda: decoding GetProperties request: %w", err)
		}
		switch t := tok.(type) {
		case xml.EndElement:
			return nil
		case xml.StartElement:
			switch t.Name.Local {
			case "ItemIDs":
				var id ItemIdentifier
				if v, ok := attrValue(t.Attr, xml.Name{Local: "ItemPath"}); ok {
					id.ItemPath = &v
				}
				id.ItemName, _ = attrValue(t.Attr, xml.Name{Local: "ItemName"})
				if err := d.Skip(); err != nil {
					return err
				}
				r.ItemIDs = append(r.ItemIDs, id)
			case "PropertyNames":
				qn, err := decodePropertyNameElement(d, t)
				if err != nil {
					return err
				}
				r.PropertyNames = append(r.PropertyNames, qn)
			default:
				if err := d.Skip(); err != nil {
					return err
				}
			}
		}
	}
}

// PropertyReplyList is one requested item's property list in a
// GetPropertiesResponse (§3.9.2, pp.78-79). ResultID is set if the item
// itself is unknown/invalid (REQ-PROPERTIES-002); the zero ErrorCode
// means the item was found (individual properties may still carry their
// own ResultID — see ItemProperty).
type PropertyReplyList struct {
	ItemPath   *string
	ItemName   string
	ResultID   ErrorCode
	Properties []ItemProperty
}

// MarshalXML implements xml.Marshaler.
func (l PropertyReplyList) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	if l.ItemPath != nil {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "ItemPath"}, Value: *l.ItemPath})
	}
	if l.ItemName != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "ItemName"}, Value: l.ItemName})
	}
	if !l.ResultID.IsZero() {
		start.Attr = append(start.Attr, qnameAttr("ResultID", l.ResultID.QName)...)
	}
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	for _, p := range l.Properties {
		if err := e.EncodeElement(p, xml.StartElement{Name: xml.Name{Local: "Properties"}}); err != nil {
			return err
		}
	}
	return e.EncodeToken(start.End())
}

// UnmarshalXML implements xml.Unmarshaler.
func (l *PropertyReplyList) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	if v, ok := attrValue(start.Attr, xml.Name{Local: "ItemPath"}); ok {
		l.ItemPath = &v
	}
	l.ItemName, _ = attrValue(start.Attr, xml.Name{Local: "ItemName"})
	resultRaw, hasResult := attrValue(start.Attr, xml.Name{Local: "ResultID"})

	var shadow struct {
		Properties []ItemProperty `xml:"Properties"`
	}
	if err := d.DecodeElement(&shadow, &start); err != nil {
		return fmt.Errorf("xmlda: decoding PropertyLists: %w", err)
	}
	l.Properties = shadow.Properties

	if hasResult {
		rid, err := resolveQName(d, resultRaw)
		if err != nil {
			return err
		}
		l.ResultID = ErrorCode{rid}
	}
	return nil
}

// GetPropertiesResponse is the response for the GetProperties operation
// (§3.9.2, pp.78-79). PropertyLists has one entry per requested item.
type GetPropertiesResponse struct {
	XMLName       xml.Name            `xml:"GetPropertiesResponse"`
	Result        ReplyBase           `xml:"GetPropertiesResult"`
	PropertyLists []PropertyReplyList `xml:"PropertyLists"`
	Errors        Errors              `xml:"Errors"`
}
