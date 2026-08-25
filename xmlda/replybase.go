package xmlda

import (
	"encoding/xml"
	"fmt"
	"time"
)

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
	start.Attr = append(start.Attr, typeAttrs(QName{Space: Namespace, Local: "ReplyBase"})...)
	start.Attr = append(start.Attr,
		xml.Attr{Name: xml.Name{Local: "RcvTime"}, Value: r.RcvTime.Format(time.RFC3339Nano)},
		xml.Attr{Name: xml.Name{Local: "ReplyTime"}, Value: r.ReplyTime.Format(time.RFC3339Nano)},
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
