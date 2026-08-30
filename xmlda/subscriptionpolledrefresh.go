package xmlda

import (
	"encoding/xml"
	"time"
)

// SubscriptionPolledRefreshRequest is the request for the
// SubscriptionPolledRefresh operation (§3.6.1, p.62). At least one handle
// is required (enforced by the server layer, not this type). None of
// this request's fields are QName-shaped, so plain struct tags suffice —
// no custom Marshal/Unmarshal needed.
type SubscriptionPolledRefreshRequest struct {
	XMLName xml.Name `xml:"SubscriptionPolledRefresh"`
	// HoldTime is an absolute server time to hold the response until; if
	// nil, WaitTime is ignored (REQ-SUBSCRIPTION-005).
	HoldTime *time.Time `xml:"HoldTime,attr,omitempty"`
	// WaitTime is milliseconds to additionally wait for a change after
	// HoldTime, ignored if HoldTime is nil. It is xsd:int on the wire —
	// signed — so a negative value decodes rather than faulting the
	// request; the server treats anything <= 0 as "do not wait".
	WaitTime int32 `xml:"WaitTime,attr"`
	// ReturnAllItems: true ignores WaitTime and returns a full snapshot
	// after HoldTime; false returns only changed items
	// (REQ-SUBSCRIPTION-006).
	ReturnAllItems bool           `xml:"ReturnAllItems,attr"`
	Options        RequestOptions `xml:"Options"`
	// ServerSubHandles lists the subscriptions to poll in this call
	// (REQ-SUBSCRIPTION-004).
	ServerSubHandles []string `xml:"ServerSubHandles"`
}

// SubscriptionPolledRefreshReplyItemList is one polled subscription's
// changed (or, if ReturnAllItems, all) items (§3.6.2).
type SubscriptionPolledRefreshReplyItemList struct {
	SubscriptionHandle string      `xml:"SubscriptionHandle,attr"`
	Items              []ItemValue `xml:"Items"`
}

// SubscriptionPolledRefreshResponse is the response for the
// SubscriptionPolledRefresh operation (§3.6.2, pp.64-66). RItemList has
// one entry per polled subscription that has data to report; a
// subscription with no changes (and ReturnAllItems=false) has no entry at
// all.
type SubscriptionPolledRefreshResponse struct {
	XMLName xml.Name `xml:"http://opcfoundation.org/webservices/XMLDA/1.0/ SubscriptionPolledRefreshResponse"`
	// DataBufferOverflow signals that buffered data had to be purged due
	// to resource limits (REQ-SUBSCRIPTION-007).
	DataBufferOverflow bool      `xml:"DataBufferOverflow,attr"`
	Result             ReplyBase `xml:"SubscriptionPolledRefreshResult"`
	// InvalidServerSubHandles lists handles the server did not recognize
	// (REQ-SUBSCRIPTION-008).
	InvalidServerSubHandles []string                                 `xml:"InvalidServerSubHandles"`
	RItemList               []SubscriptionPolledRefreshReplyItemList `xml:"RItemList"`
	Errors                  Errors                                   `xml:"Errors"`
}
