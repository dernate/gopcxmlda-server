package xmlda

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"time"
)

// SubscriptionPolledRefreshRequest is the request for the
// SubscriptionPolledRefresh operation (§3.6.1, p.62). At least one handle
// is required (enforced by the server layer, not this type).
type SubscriptionPolledRefreshRequest struct {
	XMLName xml.Name
	// HoldTime is an absolute server time to hold the response until; if
	// nil, WaitTime is ignored (REQ-SUBSCRIPTION-005).
	//
	// It is decoded through wireTime rather than as a plain time.Time
	// attribute field, which is why this type needs custom
	// Marshal/Unmarshal at all: xsd:dateTime's timezone offset is
	// optional and time.Time.UnmarshalText's is not, so a conforming
	// client sending HoldTime="2026-08-30T12:00:00" used to fault the
	// whole request — leaving it unable to poll its subscription at all,
	// since HoldTime is how long-polling is expressed.
	HoldTime *time.Time
	// WaitTime is milliseconds to additionally wait for a change after
	// HoldTime, ignored if HoldTime is nil. It is xsd:int on the wire —
	// signed — so a negative value decodes rather than faulting the
	// request; the server treats anything <= 0 as "do not wait".
	WaitTime int32
	// ReturnAllItems: true ignores WaitTime and returns a full snapshot
	// after HoldTime; false returns only changed items
	// (REQ-SUBSCRIPTION-006).
	ReturnAllItems bool
	Options        RequestOptions
	// ServerSubHandles lists the subscriptions to poll in this call
	// (REQ-SUBSCRIPTION-004).
	ServerSubHandles []string
}

// MarshalXML implements xml.Marshaler.
func (r SubscriptionPolledRefreshRequest) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Name = xml.Name{Local: "SubscriptionPolledRefresh"}
	if r.HoldTime != nil {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "HoldTime"}, Value: formatWireTime(*r.HoldTime)})
	}
	start.Attr = append(start.Attr,
		xml.Attr{Name: xml.Name{Local: "WaitTime"}, Value: strconv.FormatInt(int64(r.WaitTime), 10)},
		xml.Attr{Name: xml.Name{Local: "ReturnAllItems"}, Value: strconv.FormatBool(r.ReturnAllItems)},
	)
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	if err := e.EncodeElement(r.Options, xml.StartElement{Name: xml.Name{Local: "Options"}}); err != nil {
		return err
	}
	for _, h := range r.ServerSubHandles {
		if err := e.EncodeElement(h, xml.StartElement{Name: xml.Name{Local: "ServerSubHandles"}}); err != nil {
			return err
		}
	}
	return e.EncodeToken(start.End())
}

// UnmarshalXML implements xml.Unmarshaler.
func (r *SubscriptionPolledRefreshRequest) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	var shadow struct {
		HoldTime         *wireTime      `xml:"HoldTime,attr"`
		WaitTime         int32          `xml:"WaitTime,attr"`
		ReturnAllItems   bool           `xml:"ReturnAllItems,attr"`
		Options          RequestOptions `xml:"Options"`
		ServerSubHandles []string       `xml:"ServerSubHandles"`
	}
	if err := d.DecodeElement(&shadow, &start); err != nil {
		return fmt.Errorf("xmlda: decoding SubscriptionPolledRefresh request: %w", err)
	}
	r.XMLName = start.Name
	r.HoldTime = shadow.HoldTime.timePtr()
	r.WaitTime = shadow.WaitTime
	r.ReturnAllItems = shadow.ReturnAllItems
	r.Options = shadow.Options
	r.ServerSubHandles = shadow.ServerSubHandles
	return nil
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
