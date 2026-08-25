package xmlda

import "encoding/xml"

// Operation identifies one of the 8 OPC XML-DA operations.
type Operation struct {
	// Name is the operation's element QName (always in Namespace).
	Name QName
	// SOAPAction is the conventional SOAPAction header value for this
	// operation: Namespace + Name.Local.
	SOAPAction string
}

// operationLocalNames lists the 8 OPC XML-DA operations' local element
// names (REQ-SERVER-001).
var operationLocalNames = []string{
	"GetStatus",
	"Read",
	"Write",
	"Subscribe",
	"SubscriptionPolledRefresh",
	"SubscriptionCancel",
	"Browse",
	"GetProperties",
}

var operations = buildOperationRegistry()

func buildOperationRegistry() map[QName]Operation {
	reg := make(map[QName]Operation, len(operationLocalNames))
	for _, local := range operationLocalNames {
		qn := QName{Space: Namespace, Local: local}
		reg[qn] = Operation{Name: qn, SOAPAction: Namespace + local}
	}
	return reg
}

// peekOperation captures a SOAP Body's first child element's resolved
// name, regardless of namespace prefix, without decoding its content —
// used by IdentifyOperation to determine which of the 8 operations a raw
// document represents before committing to a concrete request type.
type peekOperation struct {
	// No tag here: Go auto-populates XMLName with the decoded element's
	// resolved name for any struct being unmarshaled from an element.
	// The ",any" wildcard belongs on the *wrapping* field (see
	// peekEnvelope.Body.Op below) — encoding/xml rejects ",any" directly
	// on an XMLName field ("invalid tag").
	XMLName xml.Name
}

type peekEnvelope struct {
	Body struct {
		Op peekOperation `xml:",any"`
	} `xml:"Body"`
}

// IdentifyOperation peeks raw — a full SOAP document's bytes — and
// reports which of the 8 operations its Body's first child represents,
// based purely on the child's resolved namespace URI and local name,
// never its prefix (REQ-NS-001/002).
//
// Three outcomes, matching the three fault shapes observed in
// testdata/faults/ (see docs/architecture/data-flow.md):
//  1. err != nil: raw is not well-formed XML/SOAP at all (e.g. a parse
//     error) — the caller has not identified any operation and should
//     emit a transport-level fault with no OPC-specific code.
//  2. err == nil, ok == false: raw is well-formed, but its Body's first
//     child is not one of the 8 known operations (or there is no Body at
//     all) — the caller should emit a clean fault, conventionally
//     ErrNotSupported (REQ-SERVER-003, open-questions.md OQ-2).
//  3. err == nil, ok == true: the operation is identified; the caller
//     should now attempt to decode raw into that operation's concrete
//     request type. A failure at that later stage (e.g. an invalid
//     dateTime) is a separate, third failure bucket the caller handles
//     itself — IdentifyOperation does not attempt that decode.
func IdentifyOperation(raw []byte) (Operation, bool, error) {
	var peek peekEnvelope
	if err := Decode(raw, &peek); err != nil {
		return Operation{}, false, err
	}
	qn := QName{Space: peek.Body.Op.XMLName.Space, Local: peek.Body.Op.XMLName.Local}
	op, ok := operations[qn]
	return op, ok, nil
}
