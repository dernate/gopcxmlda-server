package xmlda

import (
	"encoding/xml"
	"fmt"
)

// PropertyID identifies a standard OPC item property (§3.1.10, pp.38-42).
type PropertyID int

// Standard property IDs. 9-99 and 109-199 are reserved for future OPC
// use; 300-399 are reserved for OPC Alarms & Events (REQ-PROPERTIES-003).
const (
	PropDataType     PropertyID = 1
	PropValue        PropertyID = 2
	PropQuality      PropertyID = 3
	PropTimestamp    PropertyID = 4
	PropAccessRights PropertyID = 5
	PropScanRate     PropertyID = 6
	PropEUType       PropertyID = 7
	PropEUInfo       PropertyID = 8

	PropEngineeringUnits PropertyID = 100
	PropDescription      PropertyID = 101
	PropHighEU           PropertyID = 102
	PropLowEU            PropertyID = 103
	PropHighIR           PropertyID = 104
	PropLowIR            PropertyID = 105
	PropCloseLabel       PropertyID = 106
	PropOpenLabel        PropertyID = 107
	PropTimeZone         PropertyID = 108
)

var standardPropertyLocalNames = map[PropertyID]string{
	PropDataType:         "dataType",
	PropValue:            "value",
	PropQuality:          "quality",
	PropTimestamp:        "timestamp",
	PropAccessRights:     "accessRights",
	PropScanRate:         "scanRate",
	PropEUType:           "euType",
	PropEUInfo:           "euInfo",
	PropEngineeringUnits: "engineeringUnits",
	PropDescription:      "description",
	PropHighEU:           "highEU",
	PropLowEU:            "lowEU",
	PropHighIR:           "highIR",
	PropLowIR:            "lowIR",
	PropCloseLabel:       "closeLabel",
	PropOpenLabel:        "openLabel",
	PropTimeZone:         "timeZone",
}

// StandardPropertyName returns the QName (in the OPC XML-DA namespace)
// for a standard property ID, or the zero QName if id is not one of the
// standard IDs defined above.
func StandardPropertyName(id PropertyID) QName {
	local, ok := standardPropertyLocalNames[id]
	if !ok {
		return QName{}
	}
	return QName{Space: Namespace, Local: local}
}

// ItemProperty is one property of an item, used by both Browse and
// GetProperties (§3.1.10).
type ItemProperty struct {
	// Name identifies the property (required).
	Name QName
	// Description is a human-readable description of the property.
	Description string
	// ItemPath, together with ItemName, identifies this property as
	// itself a directly readable/writable/subscribable item, if
	// applicable. nil means the property has no such identity.
	ItemPath *string
	// ItemName is the property's own item name, if ItemPath is non-nil.
	ItemName string
	// Value is the property's value, or nil if not requested/returned.
	Value *Value
	// ResultID is set if this property could not be retrieved (e.g.
	// E_INVALIDPID); the zero ErrorCode means no abnormal condition.
	ResultID ErrorCode
}

// MarshalXML implements xml.Marshaler.
func (p ItemProperty) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Attr = mergeAttrs(start.Attr, qnameAttr(e, start.Attr, "Name", p.Name)...)
	if p.Description != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "Description"}, Value: p.Description})
	}
	if p.ItemPath != nil {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "ItemPath"}, Value: *p.ItemPath})
	}
	if p.ItemName != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "ItemName"}, Value: p.ItemName})
	}
	if !p.ResultID.IsZero() {
		start.Attr = mergeAttrs(start.Attr, qnameAttr(e, start.Attr, "ResultID", p.ResultID.QName)...)
	}
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	if p.Value != nil {
		if err := e.EncodeElement(*p.Value, xml.StartElement{Name: xml.Name{Local: "Value"}}); err != nil {
			return err
		}
	}
	return e.EncodeToken(start.End())
}

// UnmarshalXML implements xml.Unmarshaler.
func (p *ItemProperty) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	nameRaw, ok := attrValue(start.Attr, xml.Name{Local: "Name"})
	if !ok {
		return fmt.Errorf("xmlda: <%s> is missing a required Name attribute", start.Name.Local)
	}
	resultRaw, hasResult := attrValue(start.Attr, xml.Name{Local: "ResultID"})

	var shadow struct {
		Description string  `xml:"Description,attr"`
		ItemPath    *string `xml:"ItemPath,attr"`
		ItemName    string  `xml:"ItemName,attr"`
		Value       *Value  `xml:"Value"`
	}
	if err := d.DecodeElement(&shadow, &start); err != nil {
		return fmt.Errorf("xmlda: decoding <%s Name=%q>: %w", start.Name.Local, nameRaw, err)
	}

	name, err := resolveQNameIn(d, start.Attr, nameRaw)
	if err != nil {
		return err
	}
	p.Name = name
	p.Description = shadow.Description
	p.ItemPath = shadow.ItemPath
	p.ItemName = shadow.ItemName
	p.Value = shadow.Value

	if hasResult {
		rid, err := resolveQNameIn(d, start.Attr, resultRaw)
		if err != nil {
			return err
		}
		p.ResultID = ErrorCode{rid}
	}
	return nil
}
