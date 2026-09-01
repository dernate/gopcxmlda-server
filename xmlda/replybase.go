package xmlda

import (
	"encoding/xml"
	"fmt"
	"time"
)

// wireTimeLayout is the xsd:dateTime form this library emits everywhere:
// UTC, fixed millisecond precision, with Go's "Z" spelling of the zero
// offset (e.g. RcvTime="2026-08-24T18:22:45.921Z"). The real captured
// traffic under testdata/responses/ spells the same instant
// "…921+00:00"; both are the identical xsd:dateTime value, and "Z" is
// what Go's Z07:00 layout produces for UTC.
//
// The previous time.RFC3339Nano emitted the process's local offset and a
// variable-length fractional part ("…12:06:56.123718397+02:00"). Both are
// legal xsd:dateTime, but neither is what peers expect: the specification
// treats item timestamps as UTC throughout (property 108, timeZone, is
// defined as the offset "between the item's UTC Timestamp and…"), and
// clients that compare or subtract timestamps naively — without applying
// the offset — silently read a server in a non-UTC zone as hours off.
const wireTimeLayout = "2006-01-02T15:04:05.000Z07:00"

// formatWireTime renders t as an xsd:dateTime in this library's single
// canonical wire form. Used for every dateTime the server emits:
// ReplyBase's RcvTime/ReplyTime, ItemValue's Timestamp, Status's
// StartTime, and dateTime-typed Values.
func formatWireTime(t time.Time) string {
	return t.UTC().Format(wireTimeLayout)
}

// wireTime carries an xsd:dateTime that arrives as an XML *attribute*.
//
// It exists because encoding/xml decodes a time.Time attribute field
// through time.Time.UnmarshalText, which accepts only RFC 3339 — a
// strictly narrower grammar than xsd:dateTime, whose timezone offset is
// optional and whose lexical space also admits the end-of-day form
// "T24:00:00" and surrounding whitespace. A conforming peer sending
// HoldTime="2026-08-30T12:00:00" (no offset) therefore used to fault the
// whole request, which for SubscriptionPolledRefresh means the client
// cannot poll its subscription at all. parseXSDDateTime already accepted
// those forms for element content; this type is what routes attribute
// values through the same parser.
//
// It is unexported and used only inside the shadow structs the request
// types decode into, so the public API keeps plain *time.Time fields.
type wireTime struct{ t time.Time }

// UnmarshalXMLAttr implements xml.UnmarshalerAttr.
func (w *wireTime) UnmarshalXMLAttr(attr xml.Attr) error {
	t, err := parseXSDDateTime(attr.Value)
	if err != nil {
		return err
	}
	w.t = t
	return nil
}

// timePtr returns w's time as a fresh *time.Time, or nil if w is nil
// (the attribute was absent).
func (w *wireTime) timePtr() *time.Time {
	if w == nil {
		return nil
	}
	t := w.t
	return &t
}

// ServerState reflects the server's overall operating condition (§3.1.7).
type ServerState string

// Standard ServerState values.
const (
	ServerStateRunning   ServerState = "running"
	ServerStateFailed    ServerState = "failed"
	ServerStateNoConfig  ServerState = "noConfig"
	ServerStateSuspended ServerState = "suspended"
	ServerStateTest      ServerState = "test"
	ServerStateCommFault ServerState = "commFault"
)

// RequiresFault reports whether, given the server's current state, the
// named operation must be rejected with a whole-operation SOAP fault
// before any backend call is made (REQ-SERVER-002, §2.6 p.21):
// ServerStateFailed rejects every operation except GetStatus;
// ServerStateSuspended/NoConfig reject the three data operations (Read,
// Write, Subscribe). op is the operation's local name, e.g. "Read" —
// matching xmlda.Operation.Name.Local (see dispatch.go).
func RequiresFault(op string, state ServerState) (bool, ErrorCode) {
	switch state {
	case ServerStateFailed:
		if op != "GetStatus" {
			return true, ErrServerState
		}
	case ServerStateSuspended, ServerStateNoConfig:
		switch op {
		case "Read", "Write", "Subscribe":
			return true, ErrServerState
		}
	}
	return false, ErrorCode{}
}

// ReplyBase is used as each operation's named "…Result" element (e.g.
// ReadResult, SubscribeResult) for every response except
// SubscriptionCancelResponse (§3.1.8, p.36), which is the specification's
// deliberately leanest response and carries none of these fields.
type ReplyBase struct {
	// RcvTime is the time the server received the request (required).
	RcvTime time.Time
	// ReplyTime is the time the server sent the response (required).
	ReplyTime time.Time
	// ClientRequestHandle echoes the request's handle, if one was supplied.
	ClientRequestHandle string
	// RevisedLocaleID reports the locale actually used by the server.
	RevisedLocaleID string
	// ServerState is the server's state at the time of the reply (required).
	ServerState ServerState
}

// MarshalXML implements xml.Marshaler. It always adds an xsi:type
// attribute declaring this element as ReplyBase, matching the real-world
// wire pattern observed in testdata/responses/subscribe_680.response.xml
// (e.g. <SubscribeResult ... xsi:type="ns1:ReplyBase"/>) — the "…Result"
// element names differ per operation, but all share this one underlying
// type, which strict/.NET-generated clients may expect xsi:type to
// disambiguate.
func (r ReplyBase) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Attr = mergeAttrs(start.Attr, typeAttrs(start.Attr, QName{Space: Namespace, Local: "ReplyBase"})...)
	start.Attr = append(start.Attr,
		xml.Attr{Name: xml.Name{Local: "RcvTime"}, Value: formatWireTime(r.RcvTime)},
		xml.Attr{Name: xml.Name{Local: "ReplyTime"}, Value: formatWireTime(r.ReplyTime)},
	)
	if r.ClientRequestHandle != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "ClientRequestHandle"}, Value: r.ClientRequestHandle})
	}
	if r.RevisedLocaleID != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "RevisedLocaleID"}, Value: r.RevisedLocaleID})
	}
	if r.ServerState != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "ServerState"}, Value: string(r.ServerState)})
	}
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	return e.EncodeToken(start.End())
}

// UnmarshalXML implements xml.Unmarshaler. Any xsi:type attribute present
// is ignored on decode — the containing field's element name already
// disambiguates the operation, so the type declaration is redundant
// information for this library's own purposes.
func (r *ReplyBase) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	var shadow struct {
		RcvTime             time.Time   `xml:"RcvTime,attr"`
		ReplyTime           time.Time   `xml:"ReplyTime,attr"`
		ClientRequestHandle string      `xml:"ClientRequestHandle,attr"`
		RevisedLocaleID     string      `xml:"RevisedLocaleID,attr"`
		ServerState         ServerState `xml:"ServerState,attr"`
	}
	if err := d.DecodeElement(&shadow, &start); err != nil {
		return fmt.Errorf("xmlda: decoding <%s>: %w", start.Name.Local, err)
	}
	r.RcvTime = shadow.RcvTime
	r.ReplyTime = shadow.ReplyTime
	r.ClientRequestHandle = shadow.ClientRequestHandle
	r.RevisedLocaleID = shadow.RevisedLocaleID
	r.ServerState = shadow.ServerState
	return nil
}
