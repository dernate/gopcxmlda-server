package server

import (
	"testing"

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
	if filterHash(build()) != filterHash(build()) {
		t.Fatalf("expected two structurally identical requests to hash the same")
	}
}
