package xmlda

import "encoding/xml"

// SubscriptionCancelRequest is the request for the SubscriptionCancel
// operation (§3.7.1, p.67).
type SubscriptionCancelRequest struct {
	XMLName             xml.Name `xml:"SubscriptionCancel"`
	ServerSubHandle     string   `xml:"ServerSubHandle,attr"`
	ClientRequestHandle string   `xml:"ClientRequestHandle,attr,omitempty"`
}

// SubscriptionCancelResponse is the response for the SubscriptionCancel
// operation (§3.7.2, p.68) — deliberately the specification's leanest
// response: no ReplyBase, just an echoed ClientRequestHandle
// (REQ-SUBSCRIPTION-011). Cancelling an unknown or already-cancelled
// handle is a safe, idempotent no-op success (REQ-SUBSCRIPTION-014,
// open-questions.md OQ-9) — this response shape has no field to report
// otherwise.
type SubscriptionCancelResponse struct {
	XMLName             xml.Name `xml:"http://opcfoundation.org/webservices/XMLDA/1.0/ SubscriptionCancelResponse"`
	ClientRequestHandle string   `xml:"ClientRequestHandle,attr,omitempty"`
}
