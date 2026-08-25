package server

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// TestBackendErrorFault_ADR005Mapping exercises backendErrorFault (and, via
// it, backendFaultCodeToErrorCode) end to end through a real handler call:
// no existing test in this package previously drove any backend method's
// err != nil return, so this whole mapping mechanism — the core of
// ADR-005's two-channel error model — ran with 0% coverage despite the
// package's own final-review.md treating it as verified. Browse is used
// as the vehicle since testBrowser is the one test double with an err
// field wired up for this purpose.
func TestBackendErrorFault_ADR005Mapping(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode xmlda.ErrorCode
	}{
		{"BackendError Busy", &backend.BackendError{Fault: backend.FaultBusy, Err: errors.New("busy")}, xmlda.ErrBusy},
		{"BackendError AccessDenied", &backend.BackendError{Fault: backend.FaultAccessDenied, Err: errors.New("denied")}, xmlda.ErrAccessDenied},
		{"BackendError ServerState", &backend.BackendError{Fault: backend.FaultServerState, Err: errors.New("state")}, xmlda.ErrServerState},
		{"BackendError OutOfMemory", &backend.BackendError{Fault: backend.FaultOutOfMemory, Err: errors.New("oom")}, xmlda.ErrOutOfMemory},
		{"BackendError TimedOut", &backend.BackendError{Fault: backend.FaultTimedOut, Err: errors.New("timeout")}, xmlda.ErrTimedOut},
		{"BackendError NotSupported", &backend.BackendError{Fault: backend.FaultNotSupported, Err: errors.New("nope")}, xmlda.ErrNotSupported},
		{"BackendError unrecognized FaultCode falls back to E_FAIL", &backend.BackendError{Fault: "something-new", Err: errors.New("?")}, xmlda.ErrFail},
		{"context.DeadlineExceeded maps to E_TIMEDOUT", context.DeadlineExceeded, xmlda.ErrTimedOut},
		{"wrapped context.DeadlineExceeded still maps to E_TIMEDOUT", errWrap(context.DeadlineExceeded), xmlda.ErrTimedOut},
		{"a plain error falls back to E_FAIL", errors.New("boom"), xmlda.ErrFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := newTestStatus()
			reader := newTestReader()
			browser := &testBrowser{err: tc.err}
			be := backend.Backend{Status: status, Reader: reader, Browser: browser}
			h := newTestHandler(t, be, Config{}, clock.Real{})

			resp := postSOAP(t, h, browseRequestBody())
			f := decodeFault(t, resp)
			if f == nil {
				t.Fatalf("expected a fault for backend error %v, got none", tc.err)
			}
			if f.Code.Local != tc.wantCode.Local || f.Code.Space != tc.wantCode.Space {
				t.Fatalf("got fault code %+v, want %+v", f.Code, tc.wantCode)
			}
			if f.Text != xmlda.StandardErrorText(tc.wantCode) {
				t.Fatalf("got fault text %q, want the standard text for %v: %q", f.Text, tc.wantCode, xmlda.StandardErrorText(tc.wantCode))
			}
		})
	}
}

// TestBackendErrorFault_NeverLeaksInternalErrorText guards ADR-005's
// stated privacy property: only the fixed, generic description for the
// resolved code reaches the client, never the backend's own internal
// error string.
func TestBackendErrorFault_NeverLeaksInternalErrorText(t *testing.T) {
	status := newTestStatus()
	reader := newTestReader()
	secret := "internal-db-connection-string-leaked"
	browser := &testBrowser{err: &backend.BackendError{Fault: backend.FaultBusy, Err: errors.New(secret)}}
	be := backend.Backend{Status: status, Reader: reader, Browser: browser}
	h := newTestHandler(t, be, Config{}, clock.Real{})

	resp := postSOAP(t, h, browseRequestBody())
	f := decodeFault(t, resp)
	if f == nil {
		t.Fatalf("expected a fault")
	}
	if f.Text == secret {
		t.Fatalf("backend's internal error text leaked verbatim into the client-facing fault: %q", f.Text)
	}
}

func errWrap(err error) error {
	return fmt.Errorf("wrapping: %w", err)
}
