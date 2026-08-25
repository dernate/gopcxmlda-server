package backend

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/xmlda"
)

// stubBackend is a trivial implementation of every interface, used only
// to confirm the interfaces are satisfiable by a minimal type — a
// compile-time check as much as a runtime one.
type stubBackend struct{}

func (stubBackend) GetStatus(ctx context.Context, locale string) (ServerStatus, error) {
	return ServerStatus{StartTime: time.Now(), SupportedLocaleIDs: []string{"en-US"}}, nil
}

func (stubBackend) Read(ctx context.Context, items []ReadRequestItem) ([]Result[ItemSample], error) {
	out := make([]Result[ItemSample], len(items))
	for i := range items {
		out[i] = Result[ItemSample]{Value: ItemSample{Value: xmlda.NewInt32(0), Quality: xmlda.NewGoodQuality()}}
	}
	return out, nil
}

func (stubBackend) Write(ctx context.Context, items []WriteRequestItem) ([]Result[WriteOutcome], error) {
	return make([]Result[WriteOutcome], len(items)), nil
}

func (stubBackend) Browse(ctx context.Context, req BrowseRequest) (BrowseResult, error) {
	return BrowseResult{}, nil
}

func (stubBackend) GetProperties(ctx context.Context, reqs []PropertyRequest) ([]Result[[]Property], error) {
	return make([]Result[[]Property], len(reqs)), nil
}

// pushBackend additionally implements ChangeNotifier, confirming the
// type-assertion-based detection pattern documented on ChangeNotifier
// works against a concrete type.
type pushBackend struct{ stubBackend }

func (pushBackend) WatchItems(ctx context.Context, items []WatchRequest) (<-chan ChangeEvent, error) {
	ch := make(chan ChangeEvent)
	close(ch)
	return ch, nil
}

func TestBackend_Validate(t *testing.T) {
	var s stubBackend

	if err := (Backend{}).Validate(); !errors.Is(err, ErrMissingStatus) {
		t.Fatalf("expected ErrMissingStatus for a completely empty Backend, got %v", err)
	}
	if err := (Backend{Status: s}).Validate(); !errors.Is(err, ErrMissingReader) {
		t.Fatalf("expected ErrMissingReader when Reader is missing, got %v", err)
	}
	if err := (Backend{Reader: s}).Validate(); !errors.Is(err, ErrMissingStatus) {
		t.Fatalf("expected ErrMissingStatus when Status is missing, got %v", err)
	}
	if err := (Backend{Status: s, Reader: s}).Validate(); err != nil {
		t.Fatalf("expected a minimal valid backend (Status+Reader) to validate, got %v", err)
	}
	// Optional capabilities remain nil and that is not an error.
	b := Backend{Status: s, Reader: s}
	if b.Writer != nil || b.Browser != nil || b.Properties != nil {
		t.Fatalf("expected optional capabilities to default to nil")
	}
}

func TestChangeNotifier_DetectedViaTypeAssertion(t *testing.T) {
	var r Reader = pushBackend{}
	if _, ok := r.(ChangeNotifier); !ok {
		t.Fatalf("expected pushBackend to satisfy ChangeNotifier via type assertion")
	}
	var r2 Reader = stubBackend{}
	if _, ok := r2.(ChangeNotifier); ok {
		t.Fatalf("expected stubBackend to NOT satisfy ChangeNotifier")
	}
}

func TestBackendError_ErrorAndUnwrap(t *testing.T) {
	inner := errors.New("connection refused")
	be := &BackendError{Fault: FaultBusy, Err: inner}
	if be.Error() != inner.Error() {
		t.Fatalf("got %q, want %q", be.Error(), inner.Error())
	}
	if !errors.Is(be, inner) {
		t.Fatalf("expected errors.Is to see through BackendError via Unwrap")
	}
	var target *BackendError
	if !errors.As(be, &target) || target.Fault != FaultBusy {
		t.Fatalf("expected errors.As to recover the BackendError, got %+v", target)
	}
}

// TestBackendError_NilErr_DoesNotPanic is a regression test: a backend
// author setting Fault but forgetting Err (an easy mistake, since Fault
// is the field that carries the interesting semantic content) must get a
// usable message, not a nil-pointer-dereference panic.
func TestBackendError_NilErr_DoesNotPanic(t *testing.T) {
	be := &BackendError{Fault: FaultBusy}
	got := be.Error()
	if got == "" {
		t.Fatalf("expected a non-empty error message")
	}
	if be.Unwrap() != nil {
		t.Fatalf("expected Unwrap() to return nil when Err was never set")
	}
}

func TestResult_ZeroResultIDMeansNoCondition(t *testing.T) {
	var r Result[ItemSample]
	if !r.ResultID.IsZero() {
		t.Fatalf("expected zero-value Result to have no abnormal condition")
	}
}
