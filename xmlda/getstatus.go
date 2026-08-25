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

// GetStatusResponse is the response for the GetStatus operation.
type GetStatusResponse struct {
	XMLName xml.Name  `xml:"GetStatusResponse"`
	Result  ReplyBase `xml:"GetStatusResult"`
	Status  Status    `xml:"Status"`
	Errors  Errors    `xml:"Errors"`
}
