package xmlda

import "time"

// RequestOptions carries the common per-request options accepted by Read,
// Write, Subscribe, and SubscriptionPolledRefresh (§3.1.6, pp.34-36).
// Bool fields are pointers so "not specified on the wire" (apply the
// documented default) is distinguishable from "explicitly set to false".
type RequestOptions struct {
	// ReturnErrorText requests human-readable Errors text. Default: true.
	ReturnErrorText *bool `xml:"ReturnErrorText,attr"`
	// ReturnDiagnosticInfo requests verbose, non-deduplicated per-item
	// diagnostic text. Default: false.
	ReturnDiagnosticInfo *bool `xml:"ReturnDiagnosticInfo,attr"`
	// ReturnItemTime requests each item's Timestamp. Default: false.
	ReturnItemTime *bool `xml:"ReturnItemTime,attr"`
	// ReturnItemPath requests each item's ItemPath be echoed. Default: false.
	ReturnItemPath *bool `xml:"ReturnItemPath,attr"`
	// ReturnItemName requests each item's ItemName be echoed. Default: false.
	ReturnItemName *bool `xml:"ReturnItemName,attr"`
	// RequestDeadline is an absolute UTC deadline for the request.
	//
	// The omitempty tag is required, not cosmetic: encoding/xml calls
	// time.Time's value-receiver MarshalText via reflection for attr
	// fields, and without omitempty it does so even for a nil *time.Time,
	// panicking ("value method time.Time.MarshalText called using nil
	// *Time pointer") instead of skipping the field — confirmed against
	// go1.26.5. omitempty makes the encoder check isEmptyValue (which
	// correctly treats a nil pointer as empty) before ever calling
	// MarshalText.
	RequestDeadline *time.Time `xml:"RequestDeadline,attr,omitempty"`
	// ClientRequestHandle is echoed back in the response's ReplyBase.
	ClientRequestHandle string `xml:"ClientRequestHandle,attr,omitempty"`
	// LocaleID requests a specific locale for locale-sensitive text/values.
	LocaleID string `xml:"LocaleID,attr,omitempty"`
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
