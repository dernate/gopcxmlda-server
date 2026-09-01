package xmlda

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
)

// ErrorCode is a standard or vendor OPC XML-DA result code (§2.6, §3.1.9),
// modeled as a QName so vendor extensions in their own namespace
// round-trip correctly. The zero ErrorCode means "no abnormal condition"
// (an absent ResultID attribute) — see REQ-ERROR-002.
type ErrorCode struct {
	QName
}

// IsZero reports whether c is the zero ErrorCode (no abnormal condition).
func (c ErrorCode) IsZero() bool { return c.QName.IsZero() }

// IsError reports whether c is a critical ("E_"-prefixed) code.
func (c ErrorCode) IsError() bool { return strings.HasPrefix(c.Local, "E_") }

// IsSuccess reports whether c is a non-critical, success-with-caveat
// ("S_"-prefixed) code.
func (c ErrorCode) IsSuccess() bool { return strings.HasPrefix(c.Local, "S_") }

// Standard OPC XML-DA result codes (§3.1.9, pp. 37-38), plus two codes
// used in per-operation abnormal-result tables but not listed in the
// spec's own master table — E_BADTYPE and E_INVALIDITEMID — included here
// as first-class standard codes; see docs/specification/open-questions.md
// OQ-1.
var (
	ErrAccessDenied             = ErrorCode{QName{Namespace, "E_ACCESS_DENIED"}}
	ErrBadType                  = ErrorCode{QName{Namespace, "E_BADTYPE"}}
	ErrBusy                     = ErrorCode{QName{Namespace, "E_BUSY"}}
	ErrFail                     = ErrorCode{QName{Namespace, "E_FAIL"}}
	ErrInvalidContinuationPoint = ErrorCode{QName{Namespace, "E_INVALIDCONTINUATIONPOINT"}}
	ErrInvalidFilter            = ErrorCode{QName{Namespace, "E_INVALIDFILTER"}}
	ErrInvalidHoldTime          = ErrorCode{QName{Namespace, "E_INVALIDHOLDTIME"}}
	ErrInvalidItemID            = ErrorCode{QName{Namespace, "E_INVALIDITEMID"}}
	ErrInvalidItemName          = ErrorCode{QName{Namespace, "E_INVALIDITEMNAME"}}
	ErrInvalidItemPath          = ErrorCode{QName{Namespace, "E_INVALIDITEMPATH"}}
	ErrInvalidPID               = ErrorCode{QName{Namespace, "E_INVALIDPID"}}
	ErrNoSubscription           = ErrorCode{QName{Namespace, "E_NOSUBSCRIPTION"}}
	ErrNotSupported             = ErrorCode{QName{Namespace, "E_NOTSUPPORTED"}}
	ErrOutOfMemory              = ErrorCode{QName{Namespace, "E_OUTOFMEMORY"}}
	ErrRange                    = ErrorCode{QName{Namespace, "E_RANGE"}}
	ErrReadOnly                 = ErrorCode{QName{Namespace, "E_READONLY"}}
	ErrServerState              = ErrorCode{QName{Namespace, "E_SERVERSTATE"}}
	ErrTimedOut                 = ErrorCode{QName{Namespace, "E_TIMEDOUT"}}
	ErrUnknownItemName          = ErrorCode{QName{Namespace, "E_UNKNOWNITEMNAME"}}
	ErrUnknownItemPath          = ErrorCode{QName{Namespace, "E_UNKNOWNITEMPATH"}}
	ErrWriteOnly                = ErrorCode{QName{Namespace, "E_WRITEONLY"}}
	SuccessClamp                = ErrorCode{QName{Namespace, "S_CLAMP"}}
	SuccessDataQueueOverflow    = ErrorCode{QName{Namespace, "S_DATAQUEUEOVERFLOW"}}
	SuccessUnsupportedRate      = ErrorCode{QName{Namespace, "S_UNSUPPORTEDRATE"}}
)

// standardErrorText supplies default human-readable text for the standard
// codes above, used by DedupeErrors when no application-specific textOf
// function is given. Kept intentionally generic and free of any
// implementation detail — see docs/specification/error-mapping.md's rule
// that internal details must never reach a client verbatim.
var standardErrorText = map[ErrorCode]string{
	ErrAccessDenied:             "Access to the requested item or operation was denied.",
	ErrBadType:                  "The requested data type is invalid or the value could not be converted to it.",
	ErrBusy:                     "The server is already processing another request affecting this subscription.",
	ErrFail:                     "The operation failed for an unspecified reason.",
	ErrInvalidContinuationPoint: "The supplied continuation point is invalid or its filters changed.",
	ErrInvalidFilter:            "The supplied browse filter combination is invalid.",
	ErrInvalidHoldTime:          "The supplied hold time is invalid.",
	ErrInvalidItemID:            "The supplied item identifier is invalid.",
	ErrInvalidItemName:          "The supplied item name is malformed.",
	ErrInvalidItemPath:          "The supplied item path is malformed.",
	ErrInvalidPID:               "The requested property ID is not recognized.",
	ErrNoSubscription:           "None of the supplied subscription handles are valid.",
	ErrNotSupported:             "The requested operation or combination of options is not supported.",
	ErrOutOfMemory:              "The server has insufficient resources to complete the operation.",
	ErrRange:                    "The supplied value is outside the item's valid range.",
	ErrReadOnly:                 "The item is read-only.",
	ErrServerState:              "The operation could not complete due to an abnormal server state.",
	ErrTimedOut:                 "The operation did not complete within the allowed time.",
	ErrUnknownItemName:          "The requested item name is not known to the server.",
	ErrUnknownItemPath:          "The requested item path is not known to the server.",
	ErrWriteOnly:                "The item is write-only.",
	SuccessClamp:                "The supplied value was clamped to the item's valid range.",
	SuccessDataQueueOverflow:    "Buffered data was discarded due to resource limits.",
	SuccessUnsupportedRate:      "The requested sampling rate is not supported; the closest supported rate was used.",
}

// StandardErrorText returns the default text for a standard ErrorCode, or
// "" if c is not one of the codes defined in this package (e.g. a vendor
// code, which the caller must supply its own text for).
func StandardErrorText(c ErrorCode) string {
	return standardErrorText[c]
}

// OPCError is one entry in a response's Errors list: an ErrorCode plus
// its human-readable text (§2.6, §3.1.9).
type OPCError struct {
	ID   ErrorCode
	Text string
}

// Errors is a response's deduplicated list of OPCError entries. Its wire
// representation is one <Errors> element per entry (REQ-ERROR-003).
type Errors []OPCError

// DedupeErrors builds a response's Errors list from the ErrorCodes
// attached to that response's items/properties. Codes are deduplicated:
// multiple items sharing one code produce a single Errors entry
// (REQ-ERROR-003). Zero-value codes ("no abnormal condition") are
// skipped. If textOf is nil, StandardErrorText is used; callers with
// vendor codes should supply a textOf that falls back to
// StandardErrorText for standard codes.
func DedupeErrors(codes []ErrorCode, textOf func(ErrorCode) string) Errors {
	if textOf == nil {
		textOf = StandardErrorText
	}
	seen := make(map[ErrorCode]bool, len(codes))
	var out Errors
	for _, c := range codes {
		if c.IsZero() || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, OPCError{ID: c, Text: textOf(c)})
	}
	return out
}

// MarshalXML implements xml.Marshaler, rendering <Errors ID="prefix:CODE">
// with a nested <Text> child, per §2.6's example.
func (e OPCError) MarshalXML(enc *xml.Encoder, start xml.StartElement) error {
	start.Attr = mergeAttrs(start.Attr, qnameAttr(start.Attr, "ID", e.ID.QName)...)
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	if e.Text != "" {
		if err := enc.EncodeElement(e.Text, xml.StartElement{Name: xml.Name{Local: "Text"}}); err != nil {
			return err
		}
	}
	return enc.EncodeToken(start.End())
}

// UnmarshalXML implements xml.Unmarshaler.
func (e *OPCError) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	idRaw, ok := attrValue(start.Attr, xml.Name{Local: "ID"})
	if !ok {
		return fmt.Errorf("xmlda: <Errors> element is missing a required ID attribute")
	}
	var shadow struct {
		Text string `xml:"Text"`
	}
	if err := d.DecodeElement(&shadow, &start); err != nil {
		return fmt.Errorf("xmlda: decoding <Errors ID=%q>: %w", idRaw, err)
	}
	id, err := resolveQNameIn(d, start.Attr, idRaw)
	if err != nil {
		return err
	}
	e.ID = ErrorCode{id}
	e.Text = shadow.Text
	return nil
}

// ItemDecodeError reports that one request item could not be interpreted:
// a malformed attribute value (MaxAge, Deadband, RequestedSamplingRate,
// EnableBuffering, Timestamp), an unresolvable ReqType prefix, or a
// <Value> whose content does not match its declared xsi:type.
//
// It is deliberately NOT returned from UnmarshalXML. OPC XML-DA models an
// item-level problem as that item's own ResultID, with the rest of the
// request still processed — the whole Errors list (§2.6, §3.1.9) exists
// for exactly that. Failing the decode instead turns one malformed
// attribute in a 1000-item batch into a whole-operation fault that
// reports nothing about any of the other 999 items, and names the bad
// value without saying which item carried it. So the per-item request
// types carry one of these in a DecodeErr field and the server layer maps
// it to Code.
//
// Structural XML failures (not well-formed, no SOAP Body, an unknown
// operation, a malformed list-level attribute that applies to every item
// at once) remain whole-operation faults: none of them is a condition a
// single item's ResultID could truthfully describe.
type ItemDecodeError struct {
	// Field names the attribute or child element that could not be read,
	// e.g. "MaxAge" or "Value".
	Field string
	// Code is the per-item ResultID this condition maps to.
	Code ErrorCode
	// Err is the underlying parse failure.
	Err error
}

// Error implements the error interface.
func (e *ItemDecodeError) Error() string {
	return fmt.Sprintf("xmlda: item %s: %v", e.Field, e.Err)
}

// Unwrap supports errors.Is/errors.As against the underlying failure.
func (e *ItemDecodeError) Unwrap() error { return e.Err }

// ItemResultIDFor maps a request item's DecodeErr to the per-item ResultID
// to report for it. An *ItemDecodeError carrying a code is honored
// precisely; anything else — including a nil error, which callers should
// not pass — falls back to E_FAIL.
func ItemResultIDFor(err error) ErrorCode {
	var ide *ItemDecodeError
	if errors.As(err, &ide) && !ide.Code.IsZero() {
		return ide.Code
	}
	return ErrFail
}

// ItemDiagnosticFor returns per-item DiagnosticInfo text for a decode
// failure: which field could not be read, and why.
//
// This text describes the client's own malformed input, never a server
// internal, so returning it verbatim is safe and is the only way the
// client learns WHICH of its items was rejected — the deduplicated Errors
// entry carries the code but not the item (docs/specification/error-mapping.md).
func ItemDiagnosticFor(err error) string {
	if err == nil {
		return ""
	}
	var ide *ItemDecodeError
	if errors.As(err, &ide) {
		return "could not read " + ide.Field + ": " + ide.Err.Error()
	}
	return err.Error()
}
