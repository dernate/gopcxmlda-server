package xmlda

import (
	"testing"
)

func TestErrorCode_IsErrorIsSuccess(t *testing.T) {
	cases := []struct {
		code        ErrorCode
		wantError   bool
		wantSuccess bool
	}{
		{ErrFail, true, false},
		{ErrUnknownItemName, true, false},
		{SuccessClamp, false, true},
		{SuccessDataQueueOverflow, false, true},
		{ErrorCode{}, false, false}, // zero value: neither
	}
	for _, tc := range cases {
		if got := tc.code.IsError(); got != tc.wantError {
			t.Fatalf("%v: IsError() = %v, want %v", tc.code, got, tc.wantError)
		}
		if got := tc.code.IsSuccess(); got != tc.wantSuccess {
			t.Fatalf("%v: IsSuccess() = %v, want %v", tc.code, got, tc.wantSuccess)
		}
	}
}

func TestErrorCode_IsZero(t *testing.T) {
	var zero ErrorCode
	if !zero.IsZero() {
		t.Fatalf("zero value should report IsZero")
	}
	if ErrFail.IsZero() {
		t.Fatalf("ErrFail should not report IsZero")
	}
}

func TestStandardCodes_AllInOPCNamespace(t *testing.T) {
	codes := []ErrorCode{
		ErrAccessDenied, ErrBadType, ErrBusy, ErrFail, ErrInvalidContinuationPoint, ErrInvalidFilter,
		ErrInvalidHoldTime, ErrInvalidItemID, ErrInvalidItemName, ErrInvalidItemPath, ErrInvalidPID,
		ErrNoSubscription, ErrNotSupported, ErrOutOfMemory, ErrRange, ErrReadOnly, ErrServerState,
		ErrTimedOut, ErrUnknownItemName, ErrUnknownItemPath, ErrWriteOnly,
		SuccessClamp, SuccessDataQueueOverflow, SuccessUnsupportedRate,
	}
	for _, c := range codes {
		if c.Space != Namespace {
			t.Fatalf("%s: expected namespace %s, got %s", c.Local, Namespace, c.Space)
		}
		if StandardErrorText(c) == "" {
			t.Fatalf("%s: expected non-empty standard text", c.Local)
		}
	}
}

func TestStandardErrorText_UnknownCodeReturnsEmpty(t *testing.T) {
	vendorCode := ErrorCode{QName{Space: "http://example.com/vendor", Local: "E_WEIRD"}}
	if got := StandardErrorText(vendorCode); got != "" {
		t.Fatalf("expected empty text for a vendor code, got %q", got)
	}
}

func TestDedupeErrors(t *testing.T) {
	codes := []ErrorCode{
		ErrUnknownItemName,
		{}, // no condition, must be skipped
		ErrUnknownItemName,
		ErrRange,
		ErrUnknownItemName,
	}
	errs := DedupeErrors(codes, nil)
	if len(errs) != 2 {
		t.Fatalf("got %d entries, want 2 (deduplicated): %+v", len(errs), errs)
	}
	seen := map[string]bool{}
	for _, e := range errs {
		seen[e.ID.Local] = true
		if e.Text == "" {
			t.Fatalf("expected non-empty default text for standard code %s", e.ID.Local)
		}
	}
	if !seen["E_UNKNOWNITEMNAME"] || !seen["E_RANGE"] {
		t.Fatalf("got %+v, want E_UNKNOWNITEMNAME and E_RANGE", errs)
	}
}

func TestDedupeErrors_AllZero(t *testing.T) {
	errs := DedupeErrors([]ErrorCode{{}, {}, {}}, nil)
	if len(errs) != 0 {
		t.Fatalf("expected no Errors entries when every code is zero, got %+v", errs)
	}
}

func TestDedupeErrors_CustomTextOf(t *testing.T) {
	vendorCode := ErrorCode{QName{Space: "http://example.com/vendor", Local: "E_WEIRD"}}
	errs := DedupeErrors([]ErrorCode{vendorCode, ErrFail}, func(c ErrorCode) string {
		if t := StandardErrorText(c); t != "" {
			return t
		}
		return "vendor-specific text"
	})
	if len(errs) != 2 {
		t.Fatalf("got %d entries, want 2", len(errs))
	}
	for _, e := range errs {
		if e.Text == "" {
			t.Fatalf("expected non-empty text for %s", e.ID.Local)
		}
	}
}

func TestOPCError_MarshalUnmarshalRoundTrip(t *testing.T) {
	cases := []OPCError{
		{ID: ErrUnknownItemName, Text: "Item 'Foo' is not known to the server."},
		{ID: ErrorCode{QName{Space: "http://example.com/vendor", Local: "E_WEIRD"}}, Text: "vendor text"},
	}
	for _, tc := range cases {
		out, err := marshalErrorsSlice(t, Errors{tc})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		got := unmarshalErrorsSlice(t, out)
		if len(got) != 1 {
			t.Fatalf("got %d entries, want 1", len(got))
		}
		if got[0].ID != tc.ID {
			t.Fatalf("ID: got %+v, want %+v", got[0].ID, tc.ID)
		}
		if got[0].Text != tc.Text {
			t.Fatalf("Text: got %q, want %q", got[0].Text, tc.Text)
		}
	}
}

func TestOPCError_MissingIDAttribute(t *testing.T) {
	doc := []byte(`<root><Errors><Text>oops</Text></Errors></root>`)
	var w struct {
		Errors Errors `xml:"Errors"`
	}
	if err := Decode(doc, &w); err == nil {
		t.Fatalf("expected a decode error for an <Errors> element with no ID attribute")
	}
}

// marshalErrorsSlice / unmarshalErrorsSlice exercise Errors as it is
// actually used: a slice field on a containing response element,
// producing one repeated <Errors> element per entry.
func marshalErrorsSlice(t *testing.T, errs Errors) ([]byte, error) {
	t.Helper()
	return xmlMarshalNamed(t, "root", struct {
		Errors Errors `xml:"Errors"`
	}{Errors: errs})
}

func unmarshalErrorsSlice(t *testing.T, doc []byte) Errors {
	t.Helper()
	var w struct {
		Errors Errors `xml:"Errors"`
	}
	if err := Decode(doc, &w); err != nil {
		t.Fatalf("Decode: %v\ndoc: %s", err, doc)
	}
	return w.Errors
}
