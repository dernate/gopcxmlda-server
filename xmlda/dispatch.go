package xmlda

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
	doc, err := NewDocument(raw)
	if err != nil {
		return Operation{}, false, err
	}
	return doc.IdentifyOperation()
}

// IdentifyOperation reports which of the 8 operations doc's Body's first
// child represents, with the same three outcomes as the package-level
// IdentifyOperation (minus its parse-error case, already resolved when the
// Document was built).
func (doc *Document) IdentifyOperation() (Operation, bool, error) {
	// Read off the scan NewDocument already performed, rather than
	// tokenizing the whole document a second time for a single element
	// name: on a 1000-item Read that second pass cost 1.8 ms and 293 KB,
	// about a ninth of the entire request.
	qn := QName{Space: doc.operation.Space, Local: doc.operation.Local}
	op, ok := operations[qn]
	return op, ok, nil
}
