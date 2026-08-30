package xmlda

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
)

// SubscribeRequestItem is one requested item in a Subscribe operation
// (§3.5.1). Params carries the item-level hierarchical parameters
// (Deadband, RequestedSamplingRate, EnableBuffering, ItemPath, ReqType).
type SubscribeRequestItem struct {
	Params           ItemParams
	ItemName         string
	ClientItemHandle string
}

// MarshalXML implements xml.Marshaler.
func (it SubscribeRequestItem) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	return marshalRequestItem(e, start, it.Params, it.ItemName, it.ClientItemHandle)
}

// UnmarshalXML implements xml.Unmarshaler.
func (it *SubscribeRequestItem) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	p, itemName, clientItemHandle, err := decodeRequestItem(d, start)
	if err != nil {
		return err
	}
	it.Params, it.ItemName, it.ClientItemHandle = p, itemName, clientItemHandle
	return nil
}

// SubscribeItemList is Subscribe's <ItemList> element. At least one item
// is required (REQ-SUBSCRIPTION-001), enforced by the server layer.
type SubscribeItemList struct {
	Params ItemParams
	Items  []SubscribeRequestItem
}

// MarshalXML implements xml.Marshaler.
func (l SubscribeItemList) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Attr = mergeAttrs(start.Attr, encodeItemParamsAttrs(l.Params)...)
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
func (l *SubscribeItemList) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	p, err := decodeItemParamsAttrs(d, start.Attr)
	if err != nil {
		return err
	}
	l.Params = p
	items, err := decodeRepeatedElements[SubscribeRequestItem](d, "Items", "ItemList")
	if err != nil {
		return err
	}
	l.Items = items
	return nil
}

// SubscribeRequest is the request for the Subscribe operation (§3.5.1,
// pp.56-57).
type SubscribeRequest struct {
	// ReturnValuesOnReply controls whether current values are inlined in
	// the response (REQ-SUBSCRIPTION-001).
	ReturnValuesOnReply bool
	// SubscriptionPingRate is the liveness/abandonment timer, in
	// milliseconds; 0 means "use the server's own default" — see
	// REQ-SUBSCRIPTION-015 and open-questions.md OQ-10. It is xsd:int on
	// the wire, so a negative value decodes and is then treated as 0
	// rather than faulting the request.
	SubscriptionPingRate int32
	Options              RequestOptions
	Params               ItemParams
	ItemList             SubscribeItemList
}

// MarshalXML implements xml.Marshaler.
func (r SubscribeRequest) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Name = xml.Name{Local: "Subscribe"}
	start.Attr = append(start.Attr,
		xml.Attr{Name: xml.Name{Local: "ReturnValuesOnReply"}, Value: strconv.FormatBool(r.ReturnValuesOnReply)},
		xml.Attr{Name: xml.Name{Local: "SubscriptionPingRate"}, Value: strconv.FormatInt(int64(r.SubscriptionPingRate), 10)},
	)
	start.Attr = mergeAttrs(start.Attr, encodeItemParamsAttrs(r.Params)...)
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
func (r *SubscribeRequest) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	if v, ok := attrValue(start.Attr, xml.Name{Local: "ReturnValuesOnReply"}); ok {
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("xmlda: invalid ReturnValuesOnReply %q: %w", v, err)
		}
		r.ReturnValuesOnReply = b
	}
	if v, ok := attrValue(start.Attr, xml.Name{Local: "SubscriptionPingRate"}); ok {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 32)
		if err != nil {
			return fmt.Errorf("xmlda: invalid SubscriptionPingRate %q: %w", v, err)
		}
		r.SubscriptionPingRate = int32(n)
	}
	p, err := decodeItemParamsAttrs(d, start.Attr)
	if err != nil {
		return err
	}
	r.Params = p
	var shadow struct {
		Options  RequestOptions    `xml:"Options"`
		ItemList SubscribeItemList `xml:"ItemList"`
	}
	if err := d.DecodeElement(&shadow, &start); err != nil {
		return fmt.Errorf("xmlda: decoding Subscribe request: %w", err)
	}
	r.Options = shadow.Options
	r.ItemList = shadow.ItemList
	return nil
}

// SubscribeItemValue is one item's value plus its revised sampling rate,
// as returned in a SubscribeResponse (§3.5.2). Matches the real wire
// shape observed in testdata/responses/subscribe_680.response.xml:
// <Items RevisedSamplingRate="999" xsi:type="opc:SubscribeItemValue">
//
//	<ItemValue .../>
//
// </Items>
type SubscribeItemValue struct {
	RevisedSamplingRate int32
	ItemValue           ItemValue
}

// MarshalXML implements xml.Marshaler.
func (s SubscribeItemValue) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Attr = mergeAttrs(start.Attr, typeAttrs(start.Attr, QName{Space: Namespace, Local: "SubscribeItemValue"})...)
	start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "RevisedSamplingRate"}, Value: strconv.FormatInt(int64(s.RevisedSamplingRate), 10)})
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	if err := e.EncodeElement(s.ItemValue, xml.StartElement{Name: xml.Name{Local: "ItemValue"}}); err != nil {
		return err
	}
	return e.EncodeToken(start.End())
}

// UnmarshalXML implements xml.Unmarshaler.
func (s *SubscribeItemValue) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	if v, ok := attrValue(start.Attr, xml.Name{Local: "RevisedSamplingRate"}); ok {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 32)
		if err != nil {
			return fmt.Errorf("xmlda: invalid RevisedSamplingRate %q: %w", v, err)
		}
		s.RevisedSamplingRate = int32(n)
	}
	var shadow struct {
		ItemValue ItemValue `xml:"ItemValue"`
	}
	if err := d.DecodeElement(&shadow, &start); err != nil {
		return fmt.Errorf("xmlda: decoding SubscribeItemValue: %w", err)
	}
	s.ItemValue = shadow.ItemValue
	return nil
}

// SubscribeReplyItemList is Subscribe's <RItemList> element.
type SubscribeReplyItemList struct {
	RevisedSamplingRate int32
	Items               []SubscribeItemValue
}

// MarshalXML implements xml.Marshaler.
func (l SubscribeReplyItemList) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Attr = mergeAttrs(start.Attr, typeAttrs(start.Attr, QName{Space: Namespace, Local: "SubscribeReplyItemList"})...)
	start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "RevisedSamplingRate"}, Value: strconv.FormatInt(int64(l.RevisedSamplingRate), 10)})
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
func (l *SubscribeReplyItemList) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	if v, ok := attrValue(start.Attr, xml.Name{Local: "RevisedSamplingRate"}); ok {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 32)
		if err != nil {
			return fmt.Errorf("xmlda: invalid RevisedSamplingRate %q: %w", v, err)
		}
		l.RevisedSamplingRate = int32(n)
	}
	items, err := decodeRepeatedElements[SubscribeItemValue](d, "Items", "RItemList")
	if err != nil {
		return err
	}
	l.Items = items
	return nil
}

// SubscribeResponse is the response for the Subscribe operation (§3.5.2,
// pp.58-60). ServerSubHandle is the empty string iff no requested item
// was valid (no subscription created) — REQ-SUBSCRIPTION-002.
type SubscribeResponse struct {
	XMLName         xml.Name               `xml:"http://opcfoundation.org/webservices/XMLDA/1.0/ SubscribeResponse"`
	ServerSubHandle string                 `xml:"ServerSubHandle,attr"`
	Result          ReplyBase              `xml:"SubscribeResult"`
	RItemList       SubscribeReplyItemList `xml:"RItemList"`
	Errors          Errors                 `xml:"Errors"`
}
