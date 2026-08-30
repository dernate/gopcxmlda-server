package xmlda

import (
	"encoding/xml"
	"time"
)

// InterfaceVersion10 is the only SupportedInterfaceVersions value defined
// by OPC XML-DA 1.0 (§3.2.2).
const InterfaceVersion10 = "XML_DA_Version_1_0"

// GetStatusRequest is the request for the GetStatus operation (§3.2.1,
// p.43). It carries no items.
type GetStatusRequest struct {
	XMLName             xml.Name `xml:"GetStatus"`
	LocaleID            string   `xml:"LocaleID,attr,omitempty"`
	ClientRequestHandle string   `xml:"ClientRequestHandle,attr,omitempty"`
}

// Status is the server status reported by GetStatus (§3.2.2, pp.44-45).
type Status struct {
	// StartTime is required and must be constant across the server
	// process's lifetime (REQ-STATUS-003).
	StartTime time.Time `xml:"StartTime,attr"`
	// ProductVersion is "Major.Minor.Build".
	ProductVersion string `xml:"ProductVersion,attr,omitempty"`
	// StatusInfo is a locale-specific human-readable status description.
	StatusInfo string `xml:"StatusInfo,omitempty"`
	// VendorInfo is locale-specific vendor information.
	VendorInfo string `xml:"VendorInfo,omitempty"`
	// SupportedLocaleIDs must list at least one entry (REQ-STATUS-004).
	SupportedLocaleIDs []string `xml:"SupportedLocaleIDs"`
	// SupportedInterfaceVersions must list at least one entry
	// (REQ-STATUS-005); this library always includes InterfaceVersion10.
	SupportedInterfaceVersions []string `xml:"SupportedInterfaceVersions"`
}

// MarshalXML implements xml.Marshaler. It exists only so StartTime goes
// out in this library's canonical wire time form (formatWireTime: UTC,
// millisecond precision) rather than through time.Time's own MarshalText,
// which encoding/xml would otherwise call for an ",attr" field and which
// emits the process's local offset and nanosecond precision.
func (s Status) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "StartTime"}, Value: formatWireTime(s.StartTime)})
	if s.ProductVersion != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "ProductVersion"}, Value: s.ProductVersion})
	}
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	// Child order follows the schema's xsd:sequence for ServerStatus:
	// StatusInfo, VendorInfo, SupportedLocaleIDs*, SupportedInterfaceVersions*.
	if s.StatusInfo != "" {
		if err := e.EncodeElement(s.StatusInfo, xml.StartElement{Name: xml.Name{Local: "StatusInfo"}}); err != nil {
			return err
		}
	}
	if s.VendorInfo != "" {
		if err := e.EncodeElement(s.VendorInfo, xml.StartElement{Name: xml.Name{Local: "VendorInfo"}}); err != nil {
			return err
		}
	}
	for _, l := range s.SupportedLocaleIDs {
		if err := e.EncodeElement(l, xml.StartElement{Name: xml.Name{Local: "SupportedLocaleIDs"}}); err != nil {
			return err
		}
	}
	for _, v := range s.SupportedInterfaceVersions {
		if err := e.EncodeElement(v, xml.StartElement{Name: xml.Name{Local: "SupportedInterfaceVersions"}}); err != nil {
			return err
		}
	}
	return e.EncodeToken(start.End())
}

// GetStatusResponse is the response for the GetStatus operation (§3.2.2).
// The schema's GetStatusResponse carries exactly GetStatusResult and
// Status — there is no Errors element, so this type has no Errors field:
// modeling one would invite emitting a schema-invalid response.
type GetStatusResponse struct {
	XMLName xml.Name  `xml:"http://opcfoundation.org/webservices/XMLDA/1.0/ GetStatusResponse"`
	Result  ReplyBase `xml:"GetStatusResult"`
	Status  Status    `xml:"Status"`
}
