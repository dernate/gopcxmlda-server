package memorybackend

import (
	"context"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// TestClose_DoesNotHangOnOutstandingWatch reproduces the scenario where a
// WatchItems caller's own context is never cancelled (e.g. the owning
// subscription.Manager was never torn down, or an application calls
// Close directly without going through the usual server.Shutdown-before-
// Close ordering). Close must still return promptly, bounded by this
// Backend's own internal shutdown, not by the caller's context.
func TestClose_DoesNotHangOnOutstandingWatch(t *testing.T) {
	b := New()
	if _, err := b.WatchItems(context.Background(), []backend.WatchRequest{
		{Ref: backend.ItemRef{ItemName: "Demo/Counter"}},
	}); err != nil {
		t.Fatalf("WatchItems: %v", err)
	}

	done := make(chan struct{})
	go func() {
		b.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Close did not return within 2s while a WatchItems context.Background() call was still outstanding")
	}
}

func elementNames(els []backend.BrowseElement) map[string]bool {
	names := map[string]bool{}
	for _, e := range els {
		names[e.Name] = true
	}
	return names
}

func TestBrowse_FilterItem_OnlyActionableItems(t *testing.T) {
	b := New()
	defer b.Close()

	res, err := b.Browse(context.Background(), backend.BrowseRequest{
		Ref:    backend.ItemRef{ItemName: "Demo"},
		Filter: xmlda.BrowseFilterItem,
	})
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	for _, e := range res.Elements {
		if !e.IsItem {
			t.Fatalf("Filter=item returned a non-item element: %+v", e)
		}
	}
	names := elementNames(res.Elements)
	for _, want := range []string{"Counter", "Temperature", "Switch", "Message"} {
		if !names[want] {
			t.Fatalf("Filter=item missing expected item %q, got %+v", want, res.Elements)
		}
	}
}

func TestBrowse_FilterBranch_OnlyElementsWithChildren(t *testing.T) {
	b := New()
	defer b.Close()

	res, err := b.Browse(context.Background(), backend.BrowseRequest{
		Ref:    backend.ItemRef{ItemName: ""},
		Filter: xmlda.BrowseFilterBranch,
	})
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if len(res.Elements) != 1 || res.Elements[0].Name != "Demo" || !res.Elements[0].HasChildren {
		t.Fatalf("Filter=branch at root: got %+v, want exactly the \"Demo\" branch", res.Elements)
	}

	// None of Demo's children are themselves branches, so Filter=branch
	// one level down must yield nothing — success (empty set), not an
	// error (docs/specification/specification-analysis.md: "a filter
	// yielding an empty set is success, not an error").
	res2, err := b.Browse(context.Background(), backend.BrowseRequest{
		Ref:    backend.ItemRef{ItemName: "Demo"},
		Filter: xmlda.BrowseFilterBranch,
	})
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if len(res2.Elements) != 0 {
		t.Fatalf("Filter=branch under Demo: got %+v, want none", res2.Elements)
	}
}

func TestBrowse_ElementNameFilter_Wildcard(t *testing.T) {
	b := New()
	defer b.Close()

	res, err := b.Browse(context.Background(), backend.BrowseRequest{
		Ref:               backend.ItemRef{ItemName: "Demo"},
		ElementNameFilter: "temp*", // case-insensitive
	})
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if len(res.Elements) != 1 || res.Elements[0].Name != "Temperature" {
		t.Fatalf("ElementNameFilter=%q: got %+v, want only \"Temperature\"", "temp*", res.Elements)
	}
}

func TestBrowse_VendorFilter_HasNoEffect(t *testing.T) {
	b := New()
	defer b.Close()

	withoutFilter, err := b.Browse(context.Background(), backend.BrowseRequest{Ref: backend.ItemRef{ItemName: "Demo"}})
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	withFilter, err := b.Browse(context.Background(), backend.BrowseRequest{
		Ref:          backend.ItemRef{ItemName: "Demo"},
		VendorFilter: "whatever-a-real-vendor-filter-would-be",
	})
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if len(withoutFilter.Elements) != len(withFilter.Elements) {
		t.Fatalf("VendorFilter changed the result set: got %d elements, want %d (no effect, documented)", len(withFilter.Elements), len(withoutFilter.Elements))
	}
}

func TestBrowse_ReturnAllProperties_PopulatesProperties(t *testing.T) {
	b := New()
	defer b.Close()

	res, err := b.Browse(context.Background(), backend.BrowseRequest{
		Ref:                  backend.ItemRef{ItemName: "Demo"},
		ElementNameFilter:    "Counter",
		ReturnAllProperties:  true,
		ReturnPropertyValues: true,
	})
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if len(res.Elements) != 1 {
		t.Fatalf("got %d elements, want 1", len(res.Elements))
	}
	ids := map[xmlda.PropertyID]bool{}
	for _, p := range res.Elements[0].Properties {
		ids[p.ID] = true
	}
	if !ids[xmlda.PropDataType] || !ids[xmlda.PropDescription] || !ids[xmlda.PropValue] {
		t.Fatalf("got properties %+v, want dataType, description, and value (ReturnPropertyValues=true)", res.Elements[0].Properties)
	}
}

func TestBrowse_PropertyNames_FiltersToNamedPropertiesOnly(t *testing.T) {
	b := New()
	defer b.Close()

	res, err := b.Browse(context.Background(), backend.BrowseRequest{
		Ref:               backend.ItemRef{ItemName: "Demo"},
		ElementNameFilter: "Counter",
		PropertyNames:     []xmlda.QName{xmlda.StandardPropertyName(xmlda.PropDescription)},
	})
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if len(res.Elements) != 1 {
		t.Fatalf("got %d elements, want 1", len(res.Elements))
	}
	props := res.Elements[0].Properties
	if len(props) != 1 || props[0].ID != xmlda.PropDescription {
		t.Fatalf("got properties %+v, want only PropDescription", props)
	}
}

func TestBrowse_NoPropertiesRequested_PropertiesStaysEmpty(t *testing.T) {
	b := New()
	defer b.Close()

	res, err := b.Browse(context.Background(), backend.BrowseRequest{
		Ref:               backend.ItemRef{ItemName: "Demo"},
		ElementNameFilter: "Counter",
	})
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if len(res.Elements) != 1 {
		t.Fatalf("got %d elements, want 1", len(res.Elements))
	}
	if len(res.Elements[0].Properties) != 0 {
		t.Fatalf("got properties %+v, want none (neither ReturnAllProperties nor PropertyNames was set)", res.Elements[0].Properties)
	}
}

func TestBrowse_Pagination_ContinuationPointRoundTrip(t *testing.T) {
	b := New()
	defer b.Close()

	page1, err := b.Browse(context.Background(), backend.BrowseRequest{
		Ref:                 backend.ItemRef{ItemName: "Demo"},
		MaxElementsReturned: 2,
	})
	if err != nil {
		t.Fatalf("Browse (page 1): %v", err)
	}
	if len(page1.Elements) != 2 || !page1.MoreElements || page1.ContinuationPoint == "" {
		t.Fatalf("page 1: got %+v, want 2 elements, MoreElements=true, a non-empty ContinuationPoint", page1)
	}

	page2, err := b.Browse(context.Background(), backend.BrowseRequest{
		Ref:                 backend.ItemRef{ItemName: "Demo"},
		MaxElementsReturned: 2,
		ContinuationPoint:   page1.ContinuationPoint,
	})
	if err != nil {
		t.Fatalf("Browse (page 2): %v", err)
	}
	if len(page2.Elements) != 2 || page2.MoreElements || page2.ContinuationPoint != "" {
		t.Fatalf("page 2: got %+v, want the remaining 2 elements, MoreElements=false, no further ContinuationPoint", page2)
	}

	seen := elementNames(page1.Elements)
	for k := range elementNames(page2.Elements) {
		seen[k] = true
	}
	for _, want := range []string{"Counter", "Temperature", "Switch", "Message"} {
		if !seen[want] {
			t.Fatalf("paginating through all pages missed expected item %q, saw %v", want, seen)
		}
	}
}

func TestBrowse_InvalidContinuationPoint_Errors(t *testing.T) {
	b := New()
	defer b.Close()

	if _, err := b.Browse(context.Background(), backend.BrowseRequest{
		Ref:               backend.ItemRef{ItemName: "Demo"},
		ContinuationPoint: "not-a-number",
	}); err == nil {
		t.Fatalf("expected an error for a malformed continuation point")
	}
}

func TestGetProperties_PropertyIDs_FiltersWhenNotAll(t *testing.T) {
	b := New()
	defer b.Close()

	out, err := b.GetProperties(context.Background(), []backend.PropertyRequest{
		{Ref: backend.ItemRef{ItemName: "Demo/Counter"}, All: false, PropertyIDs: []xmlda.PropertyID{xmlda.PropDescription}},
	})
	if err != nil {
		t.Fatalf("GetProperties: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d results, want 1", len(out))
	}
	props := out[0].Value
	if len(props) != 1 || props[0].ID != xmlda.PropDescription {
		t.Fatalf("got properties %+v, want only PropDescription (All=false)", props)
	}
}

// TestGetProperties_UnrecognizedPropertyID_ReturnsInvalidPID reproduces
// docs/backend-implementation.md's documented PropertyReader contract:
// "E_INVALIDPID for one unrecognized property among several valid ones on
// an otherwise-known item" — a request mixing a valid PropertyID with an
// unrecognized one must return both: the valid one with its value, the
// unrecognized one flagged E_INVALIDPID, neither silently dropped.
func TestGetProperties_UnrecognizedPropertyID_ReturnsInvalidPID(t *testing.T) {
	b := New()
	defer b.Close()

	const unrecognized = xmlda.PropertyID(999)
	out, err := b.GetProperties(context.Background(), []backend.PropertyRequest{
		{Ref: backend.ItemRef{ItemName: "Demo/Counter"}, All: false, PropertyIDs: []xmlda.PropertyID{xmlda.PropDescription, unrecognized}},
	})
	if err != nil {
		t.Fatalf("GetProperties: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d results, want 1", len(out))
	}
	props := out[0].Value
	if len(props) != 2 {
		t.Fatalf("got properties %+v, want 2 (one valid, one E_INVALIDPID)", props)
	}
	var sawDescription, sawInvalid bool
	for _, p := range props {
		switch {
		case p.ID == xmlda.PropDescription && p.ResultID.IsZero():
			sawDescription = true
		case p.ID == unrecognized && p.ResultID == xmlda.ErrInvalidPID:
			sawInvalid = true
		}
	}
	if !sawDescription || !sawInvalid {
		t.Fatalf("got properties %+v, want PropDescription (success) and %d (E_INVALIDPID)", props, unrecognized)
	}
}

// TestWrite_OutOfRangeCounter_ClampsAndReportsOutcome reproduces
// backend.WriteOutcome.Clamped (REQ-WRITE-005): Demo/Counter is seeded
// with a valid range of [0, 1000], so a Write above that range must be
// clamped to the boundary and reported back as Clamped=true with the
// clamped value, not silently accepted or rejected.
func TestWrite_OutOfRangeCounter_ClampsAndReportsOutcome(t *testing.T) {
	b := New()
	defer b.Close()

	ref := backend.ItemRef{ItemName: "Demo/Counter"}
	out, err := b.Write(context.Background(), []backend.WriteRequestItem{
		{Ref: ref, Value: xmlda.NewInt32(5000)},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d results, want 1", len(out))
	}
	res := out[0]
	if !res.ResultID.IsZero() {
		t.Fatalf("got ResultID %+v, want success (a clamped write still succeeds)", res.ResultID)
	}
	if !res.Value.Clamped {
		t.Fatalf("got WriteOutcome %+v, want Clamped=true", res.Value)
	}
	if res.Value.Value == nil {
		t.Fatalf("got WriteOutcome %+v, want a non-nil clamped Value", res.Value)
	}
	if got, err := res.Value.Value.Int32(); err != nil || got != 1000 {
		t.Fatalf("got clamped value %v (err=%v), want 1000 (the upper bound)", got, err)
	}

	// The stored value itself must reflect the clamp, not the raw write.
	readOut, err := b.Read(context.Background(), []backend.ReadRequestItem{{Ref: ref}})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got, err := readOut[0].Value.Value.Int32(); err != nil || got != 1000 {
		t.Fatalf("stored value = %v (err=%v), want 1000", got, err)
	}
}
