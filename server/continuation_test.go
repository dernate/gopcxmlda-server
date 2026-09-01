package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// TestFilterHash_PropertyNamesAffectsHash reproduces the gap where
// filterHash ignored PropertyNames entirely: two Browse requests
// identical except for PropertyNames select different result shapes
// (which properties each returned element carries), so they must not
// hash the same — otherwise a page fetched under the second request's
// PropertyNames could be indexed by a continuation token issued for the
// first's.
func TestFilterHash_PropertyNamesAffectsHash(t *testing.T) {
	base := xmlda.BrowseRequest{ReturnAllProperties: true}
	withNames := base
	withNames.PropertyNames = []xmlda.QName{{Local: "description"}}

	if filterHash(base) == filterHash(withNames) {
		t.Fatalf("expected requests differing only in PropertyNames to hash differently")
	}
}

// TestFilterHash_PropertyNamesOrderMatters keeps the hash sensitive to
// PropertyNames order, matching every other hashed field's positional
// treatment — an implementation that instead sorted or set-ified
// PropertyNames before hashing would also be a valid choice, but a silent
// behavior change either way should be caught by a test, not discovered
// via a client's continuation token mysteriously failing.
func TestFilterHash_PropertyNamesOrderMatters(t *testing.T) {
	a := xmlda.BrowseRequest{PropertyNames: []xmlda.QName{{Local: "a"}, {Local: "b"}}}
	b := xmlda.BrowseRequest{PropertyNames: []xmlda.QName{{Local: "b"}, {Local: "a"}}}

	if filterHash(a) == filterHash(b) {
		t.Fatalf("expected PropertyNames in a different order to hash differently")
	}
}

// TestFilterHash_PropertyNamesLengthPrefixDisambiguates targets exactly
// the ambiguity filterHash's own doc comment calls out: without a
// length prefix per name, concatenating {"ab","c"} and {"a","bc"} would
// produce the identical byte string ("abc") and therefore the identical
// hash, letting a continuation token issued for one PropertyNames set be
// silently accepted as valid for the other.
func TestFilterHash_PropertyNamesLengthPrefixDisambiguates(t *testing.T) {
	a := xmlda.BrowseRequest{PropertyNames: []xmlda.QName{{Local: "ab"}, {Local: "c"}}}
	b := xmlda.BrowseRequest{PropertyNames: []xmlda.QName{{Local: "a"}, {Local: "bc"}}}

	if filterHash(a) == filterHash(b) {
		t.Fatalf("expected {ab,c} and {a,bc} to hash differently (naive concatenation would collide)")
	}
}

// TestFilterHash_PropertyNamesSpaceAlsoDisambiguates is the Space-half
// analogue of the same concern: two QNames whose Space+Local
// concatenation collides (e.g. {Space:"ab", Local:"c"} vs
// {Space:"a", Local:"bc"}) must still hash differently.
func TestFilterHash_PropertyNamesSpaceAlsoDisambiguates(t *testing.T) {
	a := xmlda.BrowseRequest{PropertyNames: []xmlda.QName{{Space: "ab", Local: "c"}}}
	b := xmlda.BrowseRequest{PropertyNames: []xmlda.QName{{Space: "a", Local: "bc"}}}

	if filterHash(a) == filterHash(b) {
		t.Fatalf("expected {Space:ab,Local:c} and {Space:a,Local:bc} to hash differently")
	}
}

// TestFilterHash_IdenticalRequestsMatch is the regression-safety
// companion: two structurally identical requests (including PropertyNames)
// must still hash identically, so a legitimate continuation call is not
// rejected.
func TestFilterHash_IdenticalRequestsMatch(t *testing.T) {
	build := func() xmlda.BrowseRequest {
		return xmlda.BrowseRequest{
			ItemName:            "root",
			ReturnAllProperties: true,
			PropertyNames:       []xmlda.QName{{Space: xmlda.Namespace, Local: "description"}, {Local: "vendorProp"}},
		}
	}
	first, second := filterHash(build()), filterHash(build())
	if first != second {
		t.Fatalf("expected two structurally identical requests to hash the same")
	}
}

// --- continuation-point authenticity, expiry and page-size independence ---

// pagingBrowser hands out a fixed cursor and records the cursor it was
// called with, so a test can prove what actually reached the backend.
type pagingBrowser struct {
	mu       sync.Mutex
	gotCurs  []string
	nextCurs string
}

func (b *pagingBrowser) Browse(_ context.Context, req backend.BrowseRequest) (backend.BrowseResult, error) {
	b.mu.Lock()
	b.gotCurs = append(b.gotCurs, req.ContinuationPoint)
	b.mu.Unlock()
	return backend.BrowseResult{
		Elements:          []backend.BrowseElement{{Name: "A", IsItem: true, Ref: &backend.ItemRef{ItemName: "A"}}},
		ContinuationPoint: b.nextCurs,
		MoreElements:      b.nextCurs != "",
	}, nil
}

func (b *pagingBrowser) Cursors() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.gotCurs...)
}

func browseBody(attrs string) string {
	return soapEnvelopeOpen + `<Browse xmlns="` + xmlda.Namespace + `" ` + attrs + `/>` + soapEnvelopeClose
}

// TestContinuationToken_ForgedCursorIsRejected pins the authenticity of the token. It used to
// be "<sha256(filters)>:<cursor>" with an unkeyed digest over fields the
// client itself supplies — so the client could compute the digest and pair
// it with any cursor it liked, and that cursor reached the backend looking
// exactly like one the backend had issued.
func TestContinuationToken_ForgedCursorIsRejected(t *testing.T) {
	st, r := newTestStatus(), newTestReader()
	br := &pagingBrowser{nextCurs: "page-2"}
	h := newTestHandler(t, backend.Backend{Status: st, Reader: r, Browser: br}, Config{}, clock.Real{})

	// A legitimate first page yields a real token.
	first := decodeResponse[xmlda.BrowseResponse](t, postSOAP(t, h, browseBody(`MaxElementsReturned="10"`)))
	token := first.ContinuationPoint
	if token == "" {
		t.Fatal("no continuation point was issued")
	}
	if !strings.HasSuffix(token, ":page-2") {
		t.Fatalf("the token does not end in the backend's own cursor: %q", token)
	}

	// Now forge one: keep the authenticated prefix, swap the cursor.
	forged := strings.TrimSuffix(token, "page-2") + "../../etc/passwd"
	resp := postSOAP(t, h, browseBody(`MaxElementsReturned="10" ContinuationPoint="`+forged+`"`))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("a forged cursor was accepted (status %d)", resp.StatusCode)
	}
	if f := decodeFault(t, resp); f == nil || f.Code.Local != "E_INVALIDCONTINUATIONPOINT" {
		t.Fatalf("got %+v, want E_INVALIDCONTINUATIONPOINT", f)
	}
	// And it never reached the backend.
	for _, c := range br.Cursors() {
		if strings.Contains(c, "etc/passwd") {
			t.Fatalf("the forged cursor reached the backend: %q", c)
		}
	}

	// A whole-token substitution (no valid MAC at all) is rejected too.
	for _, bogus := range []string{"page-2", "deadbeef:page-2", "deadbeef:9999999999:page-2", ":::"} {
		resp := postSOAP(t, h, browseBody(`MaxElementsReturned="10" ContinuationPoint="`+bogus+`"`))
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("bogus token %q was accepted", bogus)
		}
	}
}

// TestContinuationToken_RoundTrips pins that the tightening
// did not break paging: a token this server issued, replayed with the
// same filters, delivers the backend's own cursor back unchanged.
func TestContinuationToken_RoundTrips(t *testing.T) {
	st, r := newTestStatus(), newTestReader()
	br := &pagingBrowser{nextCurs: "page-2"}
	h := newTestHandler(t, backend.Backend{Status: st, Reader: r, Browser: br}, Config{}, clock.Real{})

	first := decodeResponse[xmlda.BrowseResponse](t, postSOAP(t, h,
		browseBody(`ItemName="Plant" BrowseFilter="item" MaxElementsReturned="10"`)))
	second := postSOAP(t, h, browseBody(
		`ItemName="Plant" BrowseFilter="item" MaxElementsReturned="10" ContinuationPoint="`+first.ContinuationPoint+`"`))
	if second.StatusCode != http.StatusOK {
		t.Fatalf("a valid continuation point was rejected: %+v", decodeFault(t, second))
	}
	curs := br.Cursors()
	if len(curs) != 2 || curs[0] != "" || curs[1] != "page-2" {
		t.Errorf("backend saw cursors %q, want [\"\", \"page-2\"]", curs)
	}
}

// TestContinuationToken_ChangedFilterInvalidates pins that filter
// binding survived the move to an HMAC: continuing with a different
// BrowseFilter still fails, because the digest is inside the MAC.
func TestContinuationToken_ChangedFilterInvalidates(t *testing.T) {
	st, r := newTestStatus(), newTestReader()
	br := &pagingBrowser{nextCurs: "page-2"}
	h := newTestHandler(t, backend.Backend{Status: st, Reader: r, Browser: br}, Config{}, clock.Real{})

	first := decodeResponse[xmlda.BrowseResponse](t, postSOAP(t, h, browseBody(`BrowseFilter="item"`)))
	resp := postSOAP(t, h, browseBody(`BrowseFilter="branch" ContinuationPoint="`+first.ContinuationPoint+`"`))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("continuing with a changed filter was accepted (status %d)", resp.StatusCode)
	}
	if f := decodeFault(t, resp); f == nil || f.Code.Local != "E_INVALIDCONTINUATIONPOINT" {
		t.Fatalf("got %+v, want E_INVALIDCONTINUATIONPOINT", f)
	}
}

// TestContinuationToken_PageSizeMayChangeMidPagination pins page-size independence. A continuation
// point denotes a POSITION in the result set, not a page size, and
// nothing in §3.8.1 ties the two — but MaxElementsReturned was in the
// filter digest, so a client legitimately switching page size (a UI going
// from 50 to 200 rows, or backing off after a large first page) got
// E_INVALIDCONTINUATIONPOINT for a perfectly valid continuation.
func TestContinuationToken_PageSizeMayChangeMidPagination(t *testing.T) {
	st, r := newTestStatus(), newTestReader()
	br := &pagingBrowser{nextCurs: "page-2"}
	h := newTestHandler(t, backend.Backend{Status: st, Reader: r, Browser: br}, Config{}, clock.Real{})

	first := decodeResponse[xmlda.BrowseResponse](t, postSOAP(t, h, browseBody(`MaxElementsReturned="50"`)))
	resp := postSOAP(t, h, browseBody(`MaxElementsReturned="200" ContinuationPoint="`+first.ContinuationPoint+`"`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("changing the page size mid-pagination was rejected: %+v", decodeFault(t, resp))
	}
	curs := br.Cursors()
	if len(curs) != 2 || curs[1] != "page-2" {
		t.Errorf("backend saw cursors %q, want the cursor preserved across the page-size change", curs)
	}
}

// TestContinuationToken_Expires pins the TTL: a token stops working
// once it has outlived Config.ContinuationPointTTL, bounding how long a
// replayable cursor stays usable.
func TestContinuationToken_Expires(t *testing.T) {
	st, r := newTestStatus(), newTestReader()
	br := &pagingBrowser{nextCurs: "page-2"}
	clk := &steppableClock{now: time.Date(2026, 3, 4, 9, 30, 0, 0, time.UTC)}
	h := newTestHandler(t, backend.Backend{Status: st, Reader: r, Browser: br},
		Config{ContinuationPointTTL: time.Minute}, clk)

	first := decodeResponse[xmlda.BrowseResponse](t, postSOAP(t, h, browseBody(`MaxElementsReturned="10"`)))

	// Still inside the TTL.
	clk.Advance(30 * time.Second)
	if resp := postSOAP(t, h, browseBody(`MaxElementsReturned="10" ContinuationPoint="`+first.ContinuationPoint+`"`)); resp.StatusCode != http.StatusOK {
		t.Fatalf("a token 30s into a 60s TTL was rejected: %+v", decodeFault(t, resp))
	}
	// Past it.
	clk.Advance(2 * time.Minute)
	resp := postSOAP(t, h, browseBody(`MaxElementsReturned="10" ContinuationPoint="`+first.ContinuationPoint+`"`))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("an expired token was accepted (status %d)", resp.StatusCode)
	}
	if f := decodeFault(t, resp); f == nil || f.Code.Local != "E_INVALIDCONTINUATIONPOINT" {
		t.Fatalf("got %+v, want E_INVALIDCONTINUATIONPOINT", f)
	}
}

// TestContinuationToken_NotPortableBetweenHandlers pins that the key is per
// Handler: a token from one server instance is not accepted by another,
// which is correct because a backend cursor is only meaningful to the live
// backend that issued it.
func TestContinuationToken_NotPortableBetweenHandlers(t *testing.T) {
	newH := func() *Handler {
		st, r := newTestStatus(), newTestReader()
		return newTestHandler(t, backend.Backend{Status: st, Reader: r, Browser: &pagingBrowser{nextCurs: "page-2"}},
			Config{}, clock.Real{})
	}
	a, b := newH(), newH()
	first := decodeResponse[xmlda.BrowseResponse](t, postSOAP(t, a, browseBody(`MaxElementsReturned="10"`)))
	resp := postSOAP(t, b, browseBody(`MaxElementsReturned="10" ContinuationPoint="`+first.ContinuationPoint+`"`))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("handler B accepted a token issued by handler A (status %d)", resp.StatusCode)
	}
}

func (b *filterRecordingBrowser) Browse(_ context.Context, req backend.BrowseRequest) (backend.BrowseResult, error) {
	b.mu.Lock()
	b.filter = req.Filter
	b.mu.Unlock()
	return backend.BrowseResult{}, nil
}
