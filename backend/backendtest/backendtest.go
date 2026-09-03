// Package backendtest is a conformance suite for implementations of the
// interfaces in package backend.
//
// docs/backend-implementation.md states a dozen invariants a backend must
// hold — same length and order as the request, the separation of a
// whole-operation error from a per-item ResultID, xmlda.NewNil rather than
// a zero Value, atomic application of Value+Quality+Timestamp, a watch
// channel closed when its context is done, a continuation cursor treated
// as untrusted input. Every one of them was prose. This package turns them
// into assertions, so a backend author finds out from a test rather than
// from a server that answers E_FAIL to an entire Read because one item
// came back without a declared type.
//
// Usage, from the backend's own package:
//
//	func TestConformance(t *testing.T) {
//	    backendtest.Run(t, func(t *testing.T) backendtest.Fixture {
//	        be := newMyBackend(t)
//	        return backendtest.Fixture{
//	            Backend:      be.AsBackend(),
//	            ReadableItem: backend.ItemRef{ItemName: "Plant.Line1.Temp"},
//	            UnknownItem:  backend.ItemRef{ItemName: "no.such.item"},
//	        }
//	    })
//	}
//
// Run skips whatever the fixture does not describe: a backend with no
// Writer, or one that cannot name an unknown item, simply has those checks
// reported as skipped rather than failed.
package backendtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// Fixture describes one backend under test, plus the few concrete item
// references the suite needs to exercise it. Anything left zero disables
// the checks that need it.
type Fixture struct {
	// Backend is the implementation under test.
	Backend backend.Backend

	// ReadableItem must exist and be readable. Required.
	ReadableItem backend.ItemRef
	// UnknownItem must NOT exist. Optional: without it the suite cannot
	// check that an unknown item is a per-item ResultID rather than a
	// whole-operation error, which is the single most common backend
	// mistake.
	UnknownItem backend.ItemRef
	// WritableItem and WriteValue are used to check Writer. Optional; both
	// are needed together, and the value must be one the item accepts.
	WritableItem backend.ItemRef
	WriteValue   xmlda.Value

	// BrowseRoot is the reference a root-level Browse uses. The zero
	// value (blank ItemName) means "the address space root", which is what
	// the specification defines it as.
	BrowseRoot backend.ItemRef

	// Cleanup, if set, runs after the suite finishes.
	Cleanup func()
}

// Run executes the conformance suite. newFixture is called once per
// subtest, so a backend that cannot be shared across parallel tests can
// build a fresh one each time.
func Run(t *testing.T, newFixture func(t *testing.T) Fixture) {
	t.Helper()
	for _, tc := range []struct {
		name string
		fn   func(*testing.T, Fixture)
	}{
		{"Validate", testValidate},
		{"ReadResultShape", testReadResultShape},
		{"ReadUnknownItemIsPerItem", testReadUnknownItemIsPerItem},
		{"ReadEmptyRequest", testReadEmptyRequest},
		{"ReadValuesAreEncodable", testReadValuesAreEncodable},
		{"ReadHonorsContext", testReadHonorsContext},
		{"WriteResultShape", testWriteResultShape},
		{"WriteQualityAndTimestampAreAtomic", testWriteAtomicity},
		{"BrowseCursorIsValidatedAsInput", testBrowseCursorIsUntrusted},
		{"GetPropertiesResultShape", testGetPropertiesResultShape},
		{"WatchItemsClosesOnContextDone", testWatchClosesOnDone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if f.Cleanup != nil {
				t.Cleanup(f.Cleanup)
			}
			tc.fn(t, f)
		})
	}
}

func testValidate(t *testing.T, f Fixture) {
	if err := f.Backend.Validate(); err != nil {
		t.Fatalf("Backend.Validate: %v — Status and Reader are both required", err)
	}
}

// testReadResultShape checks the contract every per-item method shares:
// one Result per requested item, in the same order. The server does not
// reorder or re-pair them, so a backend that returns a different number
// silently mislabels every item after the discrepancy.
func testReadResultShape(t *testing.T, f Fixture) {
	items := []backend.ReadRequestItem{
		{Ref: f.ReadableItem},
		{Ref: f.ReadableItem, MaxAge: time.Second},
		{Ref: f.ReadableItem},
	}
	got, err := f.Backend.Reader.Read(context.Background(), items)
	if err != nil {
		t.Fatalf("Read of %d readable items returned a whole-operation error: %v", len(items), err)
	}
	if len(got) != len(items) {
		t.Fatalf("Read returned %d results for %d items; the server pairs them by index, "+
			"so a shorter or longer slice mislabels every item after the discrepancy", len(got), len(items))
	}
	for i, r := range got {
		if r.ResultID.IsError() {
			t.Errorf("result %d for a readable item reports %v", i, r.ResultID)
		}
	}
}

// testReadUnknownItemIsPerItem checks the separation docs/architecture/
// decisions/005-backend-error-mapping.md defines: an unknown item is that
// item's ResultID, never a whole-operation error, because the latter
// discards every other item in the request.
func testReadUnknownItemIsPerItem(t *testing.T, f Fixture) {
	if f.UnknownItem == (backend.ItemRef{}) {
		t.Skip("Fixture.UnknownItem not set")
	}
	items := []backend.ReadRequestItem{{Ref: f.ReadableItem}, {Ref: f.UnknownItem}}
	got, err := f.Backend.Reader.Read(context.Background(), items)
	if err != nil {
		t.Fatalf("Read returned a whole-operation error because one item was unknown: %v\n"+
			"That discards the other items' data. Report it as that item's Result.ResultID "+
			"(E_UNKNOWNITEMNAME) and return a nil error.", err)
	}
	if len(got) != 2 {
		t.Fatalf("Read returned %d results for 2 items", len(got))
	}
	if !got[1].ResultID.IsError() {
		t.Errorf("the unknown item reports ResultID %v, want an E_ code (E_UNKNOWNITEMNAME)", got[1].ResultID)
	}
	if got[0].ResultID.IsError() {
		t.Errorf("the readable item was damaged by the unknown one next to it: %v", got[0].ResultID)
	}
}

func testReadEmptyRequest(t *testing.T, f Fixture) {
	got, err := f.Backend.Reader.Read(context.Background(), nil)
	if err != nil {
		t.Fatalf("Read of an empty item list returned an error: %v — an empty request is legal "+
			"(both <ItemList> and its <Items> are minOccurs=\"0\") and must yield an empty result", err)
	}
	if len(got) != 0 {
		t.Errorf("Read of an empty item list returned %d results, want 0", len(got))
	}
}

// testReadValuesAreEncodable is the check that would have caught the
// single most damaging backend mistake: an ItemSample whose Value was
// never constructed. Such a Value has no declared type, so the server
// cannot put it on the wire — and because a response is encoded as one
// document, one such item used to cost the entire operation.
func testReadValuesAreEncodable(t *testing.T, f Fixture) {
	got, err := f.Backend.Reader.Read(context.Background(), []backend.ReadRequestItem{{Ref: f.ReadableItem}})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Read returned %d results for 1 item", len(got))
	}
	r := got[0]
	if r.ResultID.IsError() {
		t.Skipf("the readable item reports %v; nothing to check", r.ResultID)
	}
	if !r.Value.Value.IsValid() {
		t.Fatalf("the sample's Value has no declared type — it was never constructed.\n" +
			"Use one of xmlda's constructors (NewFloat64, NewInt32, ...) for a real value, and " +
			"xmlda.NewNil(theItemsDeclaredType) for \"Bad quality, no last-known value\". " +
			"A zero xmlda.Value cannot be encoded.")
	}
}

// testReadHonorsContext checks that a backend notices a cancelled
// context. The server bounds every call with Config.BackendTimeout, so a
// backend that never checks ctx does not hang the server any more — but it
// does keep a goroutine and whatever it holds (a connection, a
// transaction) alive with nobody waiting for the result.
func testReadHonorsContext(t *testing.T, f Fixture) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.Backend.Reader.Read(ctx, []backend.ReadRequestItem{{Ref: f.ReadableItem}})
	if err == nil {
		t.Log("NOTE: Read with an already-cancelled context returned successfully. That is " +
			"allowed — the server bounds the call itself — but a backend that checks ctx frees " +
			"its connection or transaction when the client has already gone.")
		return
	}
	if !errors.Is(err, context.Canceled) {
		t.Logf("NOTE: Read with a cancelled context failed with %v rather than context.Canceled; "+
			"backend.ErrorCodeFor maps context.DeadlineExceeded to E_TIMEDOUT and everything "+
			"else to E_FAIL", err)
	}
}

func testWriteResultShape(t *testing.T, f Fixture) {
	if f.Backend.Writer == nil {
		t.Skip("Backend has no Writer")
	}
	if f.WritableItem == (backend.ItemRef{}) || !f.WriteValue.IsValid() {
		t.Skip("Fixture.WritableItem/WriteValue not set")
	}
	items := []backend.WriteRequestItem{
		{Ref: f.WritableItem, Value: f.WriteValue},
		{Ref: f.WritableItem, Value: f.WriteValue},
	}
	got, err := f.Backend.Writer.Write(context.Background(), items)
	if err != nil {
		t.Fatalf("Write of %d writable items returned a whole-operation error: %v", len(items), err)
	}
	if len(got) != len(items) {
		t.Fatalf("Write returned %d results for %d items", len(got), len(items))
	}
}

// testWriteAtomicity checks REQ-WRITE-003: a backend that cannot apply
// Value, Quality and Timestamp together must reject the whole item with
// xmlda.ErrNotSupported rather than applying part of it. Partial
// application is invisible to the client and to the server alike.
func testWriteAtomicity(t *testing.T, f Fixture) {
	if f.Backend.Writer == nil {
		t.Skip("Backend has no Writer")
	}
	if f.WritableItem == (backend.ItemRef{}) || !f.WriteValue.IsValid() {
		t.Skip("Fixture.WritableItem/WriteValue not set")
	}
	q := xmlda.NewQuality(xmlda.QualityUncertain, xmlda.LimitNone, 0)
	ts := time.Now().UTC()
	got, err := f.Backend.Writer.Write(context.Background(), []backend.WriteRequestItem{
		{Ref: f.WritableItem, Value: f.WriteValue, Quality: &q, Timestamp: &ts},
	})
	if err != nil {
		t.Fatalf("Write with Quality+Timestamp returned a whole-operation error: %v — "+
			"a backend that cannot apply them rejects the ITEM with xmlda.ErrNotSupported", err)
	}
	if len(got) != 1 {
		t.Fatalf("Write returned %d results for 1 item", len(got))
	}
	switch {
	case got[0].ResultID == xmlda.ErrNotSupported:
		// Correct refusal.
	case got[0].ResultID.IsError():
		t.Logf("NOTE: Write with Quality+Timestamp reports %v. If the backend cannot apply all "+
			"three atomically, xmlda.ErrNotSupported is the code REQ-WRITE-003 asks for.", got[0].ResultID)
	default:
		t.Log("NOTE: Write with Quality+Timestamp succeeded. Make sure all three were applied — " +
			"applying the value and dropping the quality is a specification violation the server " +
			"cannot detect.")
	}
}

// testBrowseCursorIsUntrusted checks the warning on
// backend.BrowseRequest.ContinuationPoint: the framework authenticates the
// token, which proves the server issued it — not that it is still
// meaningful. A backend must range-check and existence-check it like any
// other client-supplied value.
func testBrowseCursorIsUntrusted(t *testing.T, f Fixture) {
	if f.Backend.Browser == nil {
		t.Skip("Backend has no Browser")
	}
	for _, cursor := range []string{"", "../../etc/passwd", "999999999999999999", "\x00\xff", "not a cursor"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Browse panicked on continuation point %q: %v\n"+
						"A cursor is untrusted input: the MAC proves this server issued it, not that "+
						"the address space it pointed into still looks the same.", cursor, r)
				}
			}()
			_, err := f.Backend.Browser.Browse(context.Background(), backend.BrowseRequest{
				Ref:               f.BrowseRoot,
				ContinuationPoint: cursor,
				Filter:            xmlda.BrowseFilterAll,
			})
			_ = err // an error (or an empty page) is the correct answer; a panic is not
		}()
	}
}

func testGetPropertiesResultShape(t *testing.T, f Fixture) {
	if f.Backend.Properties == nil {
		t.Skip("Backend has no PropertyReader")
	}
	reqs := []backend.PropertyRequest{
		{Ref: f.ReadableItem, All: true, IncludeValues: true},
		{Ref: f.ReadableItem, All: true},
	}
	got, err := f.Backend.Properties.GetProperties(context.Background(), reqs)
	if err != nil {
		t.Fatalf("GetProperties returned a whole-operation error: %v", err)
	}
	if len(got) != len(reqs) {
		t.Fatalf("GetProperties returned %d results for %d requests", len(got), len(reqs))
	}
	for i, r := range got {
		for j, p := range r.Value {
			if p.ResultID.IsError() {
				continue
			}
			if p.ID == 0 && p.Name == "" {
				t.Errorf("result %d property %d has neither a standard PropertyID nor a Name; "+
					"the wire format needs one to build the required Name attribute", i, j)
			}
			if p.ID == 0 && p.Name != "" && p.Namespace == "" {
				t.Errorf("result %d property %d is a vendor property (%q) with no Namespace. "+
					"§3.1.10 requires a vendor-specific namespace; without one the server cannot "+
					"emit a qualified name for it.", i, j, p.Name)
			}
		}
	}
}

// testWatchClosesOnDone checks ChangeNotifier's exit contract: the backend
// closes the channel when its context is done. The subscription engine
// selects on the context too, so a backend that never closes does not hang
// the server — but it does leak whatever the watch holds.
func testWatchClosesOnDone(t *testing.T, f Fixture) {
	cn, ok := f.Backend.Reader.(backend.ChangeNotifier)
	if !ok {
		t.Skip("Reader does not implement ChangeNotifier")
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := cn.WatchItems(ctx, []backend.WatchRequest{{Ref: f.ReadableItem, RequestedSamplingRate: time.Second}})
	if err != nil {
		cancel()
		t.Fatalf("WatchItems: %v", err)
	}
	if ch == nil {
		cancel()
		t.Fatal("WatchItems returned a nil channel and a nil error; the subscription would have " +
			"no update source at all. Return an error instead.")
	}
	cancel()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, open := <-ch:
			if !open {
				return // closed, as the contract requires
			}
		case <-deadline:
			t.Error("WatchItems' channel was not closed within 2s of its context being cancelled. " +
				"backend.ChangeNotifier requires the backend to close it when ctx is done.")
			return
		}
	}
}
