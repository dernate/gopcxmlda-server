package xmlda

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
)

// BrowseFilter filters Browse results by element kind (§3.8.1, p.70).
type BrowseFilter string

// Standard BrowseFilter values. "all" is the schema's own default for an
// absent BrowseFilter attribute.
const (
	BrowseFilterAll    BrowseFilter = "all"
	BrowseFilterBranch BrowseFilter = "branch"
	BrowseFilterItem   BrowseFilter = "item"
)

// IsValid reports whether f is one of the three values the schema's
// browseFilter enumeration admits.
//
// The empty BrowseFilter is valid and means "all": UnmarshalXML
// substitutes the schema's own default for an absent attribute, so a
// backend never has to guess what "" means, and a decoded-then-encoded
// request keeps the same meaning. Anything else is E_INVALIDFILTER,
// which the server layer rejects rather than forwarding to a backend
// that has no way to make sense of it.
func (f BrowseFilter) IsValid() bool {
	switch f {
	case "", BrowseFilterAll, BrowseFilterBranch, BrowseFilterItem:
		return true
	default:
		return false
	}
}

// BrowseRequest is the request for the Browse operation (§3.8.1,
// pp.69-71). A blank ItemName/ItemPath means "browse the address space
// root". Single-level only — the client re-browses into a child's
// ItemPath/ItemName to descend (REQ-BROWSE-001).
type BrowseRequest struct {
	LocaleID             string
	ClientRequestHandle  string
	ItemName             string
	ItemPath             *string
	ContinuationPoint    string
	MaxElementsReturned  int32
	BrowseFilter         BrowseFilter
	ElementNameFilter    string
	VendorFilter         string
	ReturnAllProperties  bool
	ReturnPropertyValues bool
	// ReturnErrorText requests human-readable Errors text. Default: true
	// (see ReturnErrorTextOrDefault) — a pointer so "attribute absent" is
	// distinguishable from "explicitly false", the same reason
	// RequestOptions.ReturnErrorText is a pointer.
	ReturnErrorText *bool
	// PropertyNames filters which properties to inline-return; ignored if
	// ReturnAllProperties is true.
	PropertyNames []QName
}

// ReturnErrorTextOrDefault returns ReturnErrorText, or its default (true)
// if unset.
func (r BrowseRequest) ReturnErrorTextOrDefault() bool {
	// false, not RequestOptions' true: the schema gives this attribute a
	// different default on this element. RequestOptions declares
	// default="true" (§3.1.6, "If TRUE (default) …"), while the Browse and
	// GetProperties elements both declare default="false" and their prose
	// deliberately drops the "(default)" — see the WSDL in
	// docs/OPCDataAccessXMLSpecification.pdf and testdata/schema/opcxmlda.xsd.
	return boolOrDefault(r.ReturnErrorText, false)
}

// MarshalXML implements xml.Marshaler.
func (r BrowseRequest) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Name = xml.Name{Local: "Browse"}
	if r.LocaleID != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "LocaleID"}, Value: r.LocaleID})
	}
	if r.ClientRequestHandle != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "ClientRequestHandle"}, Value: r.ClientRequestHandle})
	}
	if r.ItemName != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "ItemName"}, Value: r.ItemName})
	}
	if r.ItemPath != nil {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "ItemPath"}, Value: *r.ItemPath})
	}
	if r.ContinuationPoint != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "ContinuationPoint"}, Value: r.ContinuationPoint})
	}
	if r.MaxElementsReturned != 0 {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "MaxElementsReturned"}, Value: strconv.FormatInt(int64(r.MaxElementsReturned), 10)})
	}
	if r.BrowseFilter != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "BrowseFilter"}, Value: string(r.BrowseFilter)})
	}
	if r.ElementNameFilter != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "ElementNameFilter"}, Value: r.ElementNameFilter})
	}
	if r.VendorFilter != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "VendorFilter"}, Value: r.VendorFilter})
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
	if err := encodePropertyNames(e, r.PropertyNames); err != nil {
		return err
	}
	return e.EncodeToken(start.End())
}

// UnmarshalXML implements xml.Unmarshaler.
func (r *BrowseRequest) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	r.LocaleID, _ = attrValue(start.Attr, xml.Name{Local: "LocaleID"})
	r.ClientRequestHandle, _ = attrValue(start.Attr, xml.Name{Local: "ClientRequestHandle"})
	r.ItemName, _ = attrValue(start.Attr, xml.Name{Local: "ItemName"})
	if v, ok := attrValue(start.Attr, xml.Name{Local: "ItemPath"}); ok {
		r.ItemPath = &v
	}
	r.ContinuationPoint, _ = attrValue(start.Attr, xml.Name{Local: "ContinuationPoint"})
	if v, ok := attrValue(start.Attr, xml.Name{Local: "MaxElementsReturned"}); ok {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 32)
		if err != nil {
			return fmt.Errorf("xmlda: invalid MaxElementsReturned %q: %w", v, err)
		}
		r.MaxElementsReturned = int32(n)
	}
	// The schema declares BrowseFilter with default="all", so an absent
	// attribute means "all" — substituted here rather than left as "" for
	// every backend to rediscover. An unrecognized value is kept verbatim
	// so the server layer can reject it with E_INVALIDFILTER instead of
	// silently passing nonsense down to the backend; see IsValid.
	r.BrowseFilter = BrowseFilterAll
	if v, ok := attrValue(start.Attr, xml.Name{Local: "BrowseFilter"}); ok {
		r.BrowseFilter = BrowseFilter(strings.TrimSpace(v))
	}
	r.ElementNameFilter, _ = attrValue(start.Attr, xml.Name{Local: "ElementNameFilter"})
	r.VendorFilter, _ = attrValue(start.Attr, xml.Name{Local: "VendorFilter"})
	var err error
	if r.ReturnAllProperties, r.ReturnPropertyValues, r.ReturnErrorText, err = decodeReturnFlags(start.Attr); err != nil {
		return err
	}

	for {
		tok, err := d.Token()
		if err != nil {
			return fmt.Errorf("xmlda: decoding Browse request: %w", err)
		}
		switch t := tok.(type) {
		case xml.EndElement:
			return nil
		case xml.StartElement:
			if t.Name.Local != "PropertyNames" {
				if err := d.Skip(); err != nil {
					return err
				}
				continue
			}
			qn, err := decodePropertyNameElement(d, t)
			if err != nil {
				return err
			}
			r.PropertyNames = append(r.PropertyNames, qn)
		}
	}
}

// BrowseElement is one entry in a BrowseResponse (§3.8.2, pp.72-74).
type BrowseElement struct {
	Name string `xml:"Name,attr"`
	// ItemPath and ItemName together identify this element, or ItemName
	// alone if ItemPath is empty. Both absent (ItemPath nil, ItemName
	// "") means this is a non-actionable "hint" node — see IsItem's doc.
	ItemPath *string `xml:"ItemPath,attr"`
	ItemName string  `xml:"ItemName,attr,omitempty"`
	// IsItem is required (REQ-BROWSE-005). true with no ItemPath/ItemName
	// means this element is a hint, not a directly readable/writable/
	// subscribable item.
	IsItem bool `xml:"IsItem,attr"`
	// HasChildren is required; may conservatively report true if the
	// server cannot cheaply determine whether children exist.
	HasChildren bool           `xml:"HasChildren,attr"`
	Properties  []ItemProperty `xml:"Properties"`
}

// BrowseResponse is the response for the Browse operation (§3.8.2,
// pp.72-75).
type BrowseResponse struct {
	XMLName xml.Name `xml:"http://opcfoundation.org/webservices/XMLDA/1.0/ BrowseResponse"`
	// MoreElements is always present; true if more elements exist beyond
	// MaxElementsReturned (REQ-BROWSE-003).
	MoreElements bool `xml:"MoreElements,attr"`
	// ContinuationPoint is set when the result set was truncated and the
	// server supports pagination.
	ContinuationPoint string          `xml:"ContinuationPoint,attr,omitempty"`
	Result            ReplyBase       `xml:"BrowseResult"`
	Elements          []BrowseElement `xml:"Elements"`
	Errors            Errors          `xml:"Errors"`
}
