package backend_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// The error-mapping mechanism (ADR-005) is the single translation from an
// arbitrary Go error to the OPC result code a client sees, and it is used
// by two packages that must never disagree: the server layer turns it into
// a whole-operation SOAP fault, the subscription engine into the per-item
// ResultID of an asynchronously-failing item. It had no test of its own.

func TestErrorCodeFor(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want xmlda.ErrorCode
	}{
		{"nil is not an error condition", nil, xmlda.ErrFail},
		{"plain error falls back to E_FAIL", errors.New("boom"), xmlda.ErrFail},
		{"deadline becomes E_TIMEDOUT", context.DeadlineExceeded, xmlda.ErrTimedOut},
		{"wrapped deadline still becomes E_TIMEDOUT",
			fmt.Errorf("reading tag: %w", context.DeadlineExceeded), xmlda.ErrTimedOut},
		{"cancellation is not a timeout", context.Canceled, xmlda.ErrFail},
		{"BackendError is honored precisely",
			&backend.BackendError{Fault: backend.FaultAccessDenied}, xmlda.ErrAccessDenied},
		{"wrapped BackendError is still honored",
			fmt.Errorf("layer: %w", &backend.BackendError{Fault: backend.FaultBusy}), xmlda.ErrBusy},
		{"BackendError wins over the deadline it wraps",
			&backend.BackendError{Fault: backend.FaultServerState, Err: context.DeadlineExceeded},
			xmlda.ErrServerState},
		{"unknown FaultCode falls back to E_FAIL",
			&backend.BackendError{Fault: backend.FaultCode("nonsense")}, xmlda.ErrFail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := backend.ErrorCodeFor(tc.err); got != tc.want {
				t.Errorf("ErrorCodeFor(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestFaultCode_ErrorCode(t *testing.T) {
	for fc, want := range map[backend.FaultCode]xmlda.ErrorCode{
		backend.FaultBusy:         xmlda.ErrBusy,
		backend.FaultAccessDenied: xmlda.ErrAccessDenied,
		backend.FaultServerState:  xmlda.ErrServerState,
		backend.FaultOutOfMemory:  xmlda.ErrOutOfMemory,
		backend.FaultTimedOut:     xmlda.ErrTimedOut,
		backend.FaultNotSupported: xmlda.ErrNotSupported,
		backend.FaultCode(""):     xmlda.ErrFail,
		backend.FaultCode("???"):  xmlda.ErrFail,
	} {
		if got := fc.ErrorCode(); got != want {
			t.Errorf("FaultCode(%q).ErrorCode() = %v, want %v", fc, got, want)
		}
	}
}

// TestBackendError_SurvivesAForgottenErr pins the one thing a backend
// author gets wrong by accident: setting Fault and leaving Err nil. The
// type must not panic on the nil dereference, because the message is
// produced on the error path of a request that has already gone wrong.
func TestBackendError_SurvivesAForgottenErr(t *testing.T) {
	e := &backend.BackendError{Fault: backend.FaultBusy}
	if got := e.Error(); got == "" {
		t.Error("Error() on a BackendError with no wrapped error produced an empty message")
	}
	if e.Unwrap() != nil {
		t.Error("Unwrap() invented an error that was never set")
	}
	if got := backend.ErrorCodeFor(e); got != xmlda.ErrBusy {
		t.Errorf("ErrorCodeFor = %v, want E_BUSY even without a wrapped error", got)
	}

	inner := errors.New("device offline")
	wrapped := &backend.BackendError{Fault: backend.FaultTimedOut, Err: inner}
	if wrapped.Error() != inner.Error() {
		t.Errorf("Error() = %q, want the wrapped error's own message", wrapped.Error())
	}
	if !errors.Is(wrapped, inner) {
		t.Error("errors.Is cannot see through BackendError")
	}
}

// TestBackendValidate pins the fail-fast contract: a Backend missing a
// required capability is rejected at construction rather than panicking
// at request time.
func TestBackendValidate(t *testing.T) {
	var st backend.StatusProvider = stubStatus{}
	var rd backend.Reader = stubReader{}

	if err := (backend.Backend{}).Validate(); !errors.Is(err, backend.ErrMissingStatus) {
		t.Errorf("empty Backend: got %v, want ErrMissingStatus", err)
	}
	if err := (backend.Backend{Status: st}).Validate(); !errors.Is(err, backend.ErrMissingReader) {
		t.Errorf("Status only: got %v, want ErrMissingReader", err)
	}
	if err := (backend.Backend{Status: st, Reader: rd}).Validate(); err != nil {
		t.Errorf("Status+Reader is a complete Backend, got %v", err)
	}
}

type stubStatus struct{}

func (stubStatus) GetStatus(context.Context, string) (backend.ServerStatus, error) {
	return backend.ServerStatus{}, nil
}

type stubReader struct{}

func (stubReader) Read(context.Context, []backend.ReadRequestItem) ([]backend.Result[backend.ItemSample], error) {
	return nil, nil
}
