package backendtest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// A conformance suite that only ever passes is worthless. These tests
// drive it against backends that break each documented invariant on
// purpose, and assert that the corresponding check fails.

type okStatus struct{}

func (okStatus) GetStatus(context.Context, string) (backend.ServerStatus, error) {
	return backend.ServerStatus{
		State: xmlda.ServerStateRunning, StartTime: time.Unix(0, 0).UTC(),
		SupportedLocaleIDs: []string{"en-US"},
	}, nil
}

// goodReader is the baseline every broken variant below is a small
// deviation from.
type goodReader struct{}

func (goodReader) Read(_ context.Context, items []backend.ReadRequestItem) ([]backend.Result[backend.ItemSample], error) {
	out := make([]backend.Result[backend.ItemSample], len(items))
	for i, it := range items {
		if it.Ref.ItemName == "missing" {
			out[i] = backend.Result[backend.ItemSample]{ResultID: xmlda.ErrUnknownItemName}
			continue
		}
		out[i] = backend.Result[backend.ItemSample]{
			Value: backend.ItemSample{Value: xmlda.NewFloat64(1.5), Timestamp: time.Unix(0, 0).UTC()},
		}
	}
	return out, nil
}

type shortReader struct{ goodReader }

func (s shortReader) Read(ctx context.Context, items []backend.ReadRequestItem) ([]backend.Result[backend.ItemSample], error) {
	out, err := s.goodReader.Read(ctx, items)
	if len(out) > 0 {
		out = out[:len(out)-1]
	}
	return out, err
}

type wholeOpErrorReader struct{ goodReader }

func (w wholeOpErrorReader) Read(ctx context.Context, items []backend.ReadRequestItem) ([]backend.Result[backend.ItemSample], error) {
	for _, it := range items {
		if it.Ref.ItemName == "missing" {
			return nil, errUnknown
		}
	}
	return w.goodReader.Read(ctx, items)
}

var errUnknown = &backend.BackendError{Fault: backend.FaultNotSupported}

// No embedded goodReader: backend.Reader is a single-method interface
// and this type overrides that method, so the embedding was dead weight.
type valuelessReader struct{}

func (valuelessReader) Read(_ context.Context, items []backend.ReadRequestItem) ([]backend.Result[backend.ItemSample], error) {
	// The Value is never constructed — the single most damaging backend
	// mistake, because a Value with no declared type cannot be encoded.
	return make([]backend.Result[backend.ItemSample], len(items)), nil
}

func fixtureFor(r backend.Reader) Fixture {
	return Fixture{
		Backend:      backend.Backend{Status: okStatus{}, Reader: r},
		ReadableItem: backend.ItemRef{ItemName: "ok"},
		UnknownItem:  backend.ItemRef{ItemName: "missing"},
	}
}

// runCapturing runs one check against a fixture and reports whether it
// failed, without failing the enclosing test.
func runCapturing(t *testing.T, name string, fn func(*testing.T, Fixture), f Fixture) bool {
	t.Helper()
	var failed bool
	inner := func(t *testing.T) { fn(t, f) }
	failed = !testing.RunTests(func(string, string) (bool, error) { return true, nil },
		[]testing.InternalTest{{Name: name, F: inner}})
	return failed
}

func TestSuite_CatchesAShortResultSlice(t *testing.T) {
	if !runCapturing(t, "shape", testReadResultShape, fixtureFor(shortReader{})) {
		t.Error("the suite accepted a backend returning fewer results than items")
	}
}

func TestSuite_CatchesAWholeOperationErrorForOneItem(t *testing.T) {
	if !runCapturing(t, "perItem", testReadUnknownItemIsPerItem, fixtureFor(wholeOpErrorReader{})) {
		t.Error("the suite accepted a backend that fails the whole operation for one unknown item")
	}
}

func TestSuite_CatchesAnUnconstructedValue(t *testing.T) {
	if !runCapturing(t, "encodable", testReadValuesAreEncodable, fixtureFor(valuelessReader{})) {
		t.Error("the suite accepted a sample whose Value has no declared type")
	}
}

func TestSuite_AcceptsAConformingBackend(t *testing.T) {
	f := fixtureFor(goodReader{})
	for name, fn := range map[string]func(*testing.T, Fixture){
		"shape":     testReadResultShape,
		"perItem":   testReadUnknownItemIsPerItem,
		"empty":     testReadEmptyRequest,
		"encodable": testReadValuesAreEncodable,
	} {
		if runCapturing(t, name, fn, f) {
			t.Errorf("check %q failed against a conforming backend", name)
		}
	}
}

// TestSuite_CatchesAWatchThatIgnoresItsContext covers the one contract a
// server cannot enforce for the backend: closing the channel.
func TestSuite_CatchesAWatchThatIgnoresItsContext(t *testing.T) {
	f := fixtureFor(neverClosingWatcher{})
	if !runCapturing(t, "watch", testWatchClosesOnDone, f) {
		t.Error("the suite accepted a WatchItems that never closes its channel")
	}
}

type neverClosingWatcher struct{ goodReader }

func (neverClosingWatcher) WatchItems(context.Context, []backend.WatchRequest) (<-chan backend.ChangeEvent, error) {
	return make(chan backend.ChangeEvent), nil // never closed
}

func TestFixture_SkipsWhatItDoesNotDescribe(t *testing.T) {
	// A fixture that names no unknown item, no writer and no browser must
	// skip those checks rather than fail them — a backend implementing
	// only Status+Reader is a supported shape.
	f := Fixture{
		Backend:      backend.Backend{Status: okStatus{}, Reader: goodReader{}},
		ReadableItem: backend.ItemRef{ItemName: "ok"},
	}
	for name, fn := range map[string]func(*testing.T, Fixture){
		"perItem": testReadUnknownItemIsPerItem,
		"write":   testWriteResultShape,
		"browse":  testBrowseCursorIsUntrusted,
		"props":   testGetPropertiesResultShape,
		"watch":   testWatchClosesOnDone,
	} {
		if runCapturing(t, name, fn, f) {
			t.Errorf("check %q failed on a fixture that does not describe it; it should skip", name)
		}
	}
	_ = strings.TrimSpace("")
}
