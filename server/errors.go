package server

import (
	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/soap"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// toSoapQName converts an xmlda.ErrorCode to the package-local soap.QName
// soap.Fault needs — see docs/architecture/package-structure.md for why
// soap cannot import xmlda directly.
func toSoapQName(c xmlda.ErrorCode) soap.QName {
	return soap.QName{Space: c.Space, Local: c.Local}
}

// fault builds a soap.Fault from an xmlda.ErrorCode and text.
func fault(code xmlda.ErrorCode, text string) *soap.Fault {
	return &soap.Fault{Code: toSoapQName(code), Text: text}
}

// backendErrorFault maps a whole-operation error from a backend call (or
// from subscription.Manager, which surfaces the same kind of plain Go
// errors for whole-operation failures) to a soap.Fault, per ADR-005.
//
// The error-to-code mapping itself lives in backend.ErrorCodeFor, because
// the subscription engine needs the identical mapping for the per-item
// ResultID of an asynchronously-failing subscribed item; keeping one copy
// is what stops the two from drifting. Internal error text is never
// included verbatim — only the fixed, generic description matching the
// resolved code — so backend implementation details never reach a client
// (docs/specification/error-mapping.md).
func backendErrorFault(err error) *soap.Fault {
	code := backend.ErrorCodeFor(err)
	return fault(code, xmlda.StandardErrorText(code))
}

// soapClientFault is a plain SOAP-standard fault (namespace soap.NS11,
// this library always emits SOAP 1.1 — testing-strategy.md), used for
// transport/decode-level failures that are the client's own malformed
// input rather than an OPC XML-DA condition: matches the real-world
// fault_soap11_xml_syntax_error.response.xml (SOAP-ENV:Client) and
// fault_soap12_invalid_datetime.response.xml (soap:Sender, SOAP 1.2's
// equivalent) shapes — see docs/specification/error-mapping.md's note
// that transport-level failures use a zero-XML-DA-namespace code, not an
// xmlda.ErrorCode. A Fault with a zero Code marshals to an empty,
// non-conformant <faultcode> element, so one must always be set.
func soapClientFault(text string) *soap.Fault {
	return &soap.Fault{Code: soap.QName{Space: soap.NS11, Local: "Client"}, Text: text}
}

// requestDecodeFault builds the fault for bucket 3 of
// xmlda.IdentifyOperation's documented failure model: the operation was
// recognized, but decoding its typed request body failed (e.g. an
// invalid dateTime). This text is about the client's own malformed
// input, not a server secret, so including the underlying parse error is
// appropriate here (mirrors the real-world fault text observed in
// testdata/faults/fault_soap12_invalid_datetime.response.xml).
func requestDecodeFault(opName string, err error) *soap.Fault {
	return soapClientFault("invalid " + opName + " request: " + err.Error())
}

// limitExceededFault builds the fault for a request that exceeds a
// configured Config limit (REQ-LIMITS-001; these limits are
// implementation policy, not spec-mandated — ADR-011). E_OUTOFMEMORY is
// the closest standard code for "the server declines due to a resource
// policy."
func limitExceededFault(text string) *soap.Fault {
	return fault(xmlda.ErrOutOfMemory, text)
}
