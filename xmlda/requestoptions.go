package xmlda

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"time"
)

// RequestOptions carries the common per-request options accepted by Read,
// Write, Subscribe, and SubscriptionPolledRefresh (§3.1.6, pp.34-36).
// Bool fields are pointers so "not specified on the wire" (apply the
// documented default) is distinguishable from "explicitly set to false".
type RequestOptions struct {
	// ReturnErrorText requests human-readable Errors text. Default: true.
	ReturnErrorText *bool
	// ReturnDiagnosticInfo requests verbose, non-deduplicated per-item
	// diagnostic text. Default: false.
	ReturnDiagnosticInfo *bool
	// ReturnItemTime requests each item's Timestamp. Default: false.
	ReturnItemTime *bool
	// ReturnItemPath requests each item's ItemPath be echoed. Default: false.
	ReturnItemPath *bool
	// ReturnItemName requests each item's ItemName be echoed. Default: false.
	ReturnItemName *bool
	// RequestDeadline is an absolute UTC deadline for the request.
	//
	// Neither this field nor any other dateTime in this package is
	// decoded through a plain time.Time struct field: encoding/xml would
	// route it through time.Time.UnmarshalText, which accepts only
	// RFC 3339 and so rejects a conforming xsd:dateTime that omits the
	// (optional) timezone offset. UnmarshalXML below goes through
	// wireTime instead.
	RequestDeadline *time.Time
	// ClientRequestHandle is echoed back in the response's ReplyBase.
	ClientRequestHandle string
	// LocaleID requests a specific locale for locale-sensitive text/values.
	LocaleID string
}

// MarshalXML implements xml.Marshaler.
//
// It exists so RequestDeadline goes out in this library's single
// canonical wire time form (formatWireTime: UTC, millisecond precision)
// rather than through time.Time's own MarshalText, which encoding/xml
// would otherwise call for an ",attr" field and which emits the process's
// local offset and nanosecond precision.
func (o RequestOptions) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	addBool := func(local string, p *bool) {
		if p != nil {
			start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: local}, Value: strconv.FormatBool(*p)})
		}
	}
	addBool("ReturnErrorText", o.ReturnErrorText)
	addBool("ReturnDiagnosticInfo", o.ReturnDiagnosticInfo)
	addBool("ReturnItemTime", o.ReturnItemTime)
	addBool("ReturnItemPath", o.ReturnItemPath)
	addBool("ReturnItemName", o.ReturnItemName)
	if o.RequestDeadline != nil {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "RequestDeadline"}, Value: formatWireTime(*o.RequestDeadline)})
	}
	if o.ClientRequestHandle != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "ClientRequestHandle"}, Value: o.ClientRequestHandle})
	}
	if o.LocaleID != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "LocaleID"}, Value: o.LocaleID})
	}
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	return e.EncodeToken(start.End())
}

// UnmarshalXML implements xml.Unmarshaler. RequestDeadline is read
// through wireTime so the full xsd:dateTime lexical space is accepted;
// every other attribute is a plain type a struct tag handles correctly.
func (o *RequestOptions) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	var shadow struct {
		ReturnErrorText      *bool     `xml:"ReturnErrorText,attr"`
		ReturnDiagnosticInfo *bool     `xml:"ReturnDiagnosticInfo,attr"`
		ReturnItemTime       *bool     `xml:"ReturnItemTime,attr"`
		ReturnItemPath       *bool     `xml:"ReturnItemPath,attr"`
		ReturnItemName       *bool     `xml:"ReturnItemName,attr"`
		RequestDeadline      *wireTime `xml:"RequestDeadline,attr"`
		ClientRequestHandle  string    `xml:"ClientRequestHandle,attr"`
		LocaleID             string    `xml:"LocaleID,attr"`
	}
	if err := d.DecodeElement(&shadow, &start); err != nil {
		return fmt.Errorf("xmlda: decoding <%s>: %w", start.Name.Local, err)
	}
	*o = RequestOptions{
		ReturnErrorText:      shadow.ReturnErrorText,
		ReturnDiagnosticInfo: shadow.ReturnDiagnosticInfo,
		ReturnItemTime:       shadow.ReturnItemTime,
		ReturnItemPath:       shadow.ReturnItemPath,
		ReturnItemName:       shadow.ReturnItemName,
		RequestDeadline:      shadow.RequestDeadline.timePtr(),
		ClientRequestHandle:  shadow.ClientRequestHandle,
		LocaleID:             shadow.LocaleID,
	}
	return nil
}

func boolOrDefault(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// returnErrorTextOrDefault returns the effective ReturnErrorText value: *p
// if set, this package's documented default (true) otherwise. Shared by
// RequestOptions, BrowseRequest, and GetPropertiesRequest — the three
// request types that carry their own ReturnErrorText field rather than
// embedding RequestOptions (see docs/architecture/decisions/006-*.md) —
// so the default lives in exactly one place instead of being repeated as
// a literal in each type's own ReturnErrorTextOrDefault method.
func returnErrorTextOrDefault(p *bool) bool {
	return boolOrDefault(p, true)
}

// ReturnErrorTextOrDefault returns ReturnErrorText, or its default (true)
// if unset.
func (o RequestOptions) ReturnErrorTextOrDefault() bool {
	return returnErrorTextOrDefault(o.ReturnErrorText)
}

// ReturnDiagnosticInfoOrDefault returns ReturnDiagnosticInfo, or its
// default (false) if unset.
func (o RequestOptions) ReturnDiagnosticInfoOrDefault() bool {
	return boolOrDefault(o.ReturnDiagnosticInfo, false)
}

// ReturnItemTimeOrDefault returns ReturnItemTime, or its default (false)
// if unset.
func (o RequestOptions) ReturnItemTimeOrDefault() bool {
	return boolOrDefault(o.ReturnItemTime, false)
}

// ReturnItemPathOrDefault returns ReturnItemPath, or its default (false)
// if unset.
func (o RequestOptions) ReturnItemPathOrDefault() bool {
	return boolOrDefault(o.ReturnItemPath, false)
}

// ReturnItemNameOrDefault returns ReturnItemName, or its default (false)
// if unset.
func (o RequestOptions) ReturnItemNameOrDefault() bool {
	return boolOrDefault(o.ReturnItemName, false)
}
