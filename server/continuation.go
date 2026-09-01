package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dernate/gopcxmlda-server/xmlda"
)

// Browse's ContinuationPoint enforcement is a framework concern, not a
// backend one (REQ-BROWSE-002). The wire-visible token is
//
//	<hex HMAC>:<expiry unix seconds>:<backendCursor>
//
// where the HMAC covers the expiry, the cursor, and a digest of every
// request field that must stay identical across a continued Browse call.
// Backends only ever see/return their own private, opaque cursor half.
//
// The MAC is keyed, not a bare digest, and that is the point. Every input
// to the filter digest comes from the request the client itself sends, so
// an unkeyed hash is one the client can compute — which made the cursor
// half freely substitutable and left a backend that trusted it (as a file
// name, a SQL OFFSET, a keyset bound) open to whatever the client felt
// like putting there. With a process-local random key, a token this
// server did not issue does not verify.
//
// The key is generated per Handler and never persisted: a Browse cursor
// is meaningful only to the live backend that issued it, so a token
// surviving a restart or crossing to another instance would be a bug, not
// a feature. Clients see E_INVALIDCONTINUATIONPOINT and restart the
// browse, which the specification already requires them to handle.

// continuationKeyLen is the HMAC key length in bytes — 32, matching
// SHA-256's block-independent output size.
const continuationKeyLen = 32

// newContinuationKey returns a fresh random HMAC key for continuation
// tokens.
func newContinuationKey() ([]byte, error) {
	key := make([]byte, continuationKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("server: generating continuation-point key: %w", err)
	}
	return key, nil
}

// filterHash digests every Browse request field that selects or orders
// the result set a continuation point indexes into.
//
// MaxElementsReturned is deliberately NOT part of it. A continuation
// point denotes a position in the result set, not a page size, and
// nothing in §3.8.1 ties the two — so a client that legitimately changes
// its page size between calls (a UI switching from 50 to 200 rows, or
// backing off after an unexpectedly large first page) must not have its
// continuation rejected. Including it meant exactly that: a spurious
// E_INVALIDCONTINUATIONPOINT for a perfectly valid continuation.
func filterHash(req xmlda.BrowseRequest) string {
	h := sha256.New()
	itemPath := ""
	if req.ItemPath != nil {
		itemPath = *req.ItemPath
	}
	// The error returns below are discarded deliberately: hash.Hash
	// documents that its Write never returns one, so there is no failure
	// here to handle or propagate.
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00%t\x00%t",
		req.ItemName, itemPath, req.BrowseFilter, req.ElementNameFilter, req.VendorFilter,
		req.ReturnAllProperties, req.ReturnPropertyValues)
	// PropertyNames belongs in the hash too: it selects which properties
	// each returned element carries, so changing it mid-pagination changes
	// the shape of the very result set the continuation point indexes
	// into. Length-prefixing each name keeps the digest unambiguous —
	// without it, {"ab","c"} and {"a","bc"} would hash identically.
	for _, pn := range req.PropertyNames {
		_, _ = fmt.Fprintf(h, "\x00%d:%s\x00%d:%s", len(pn.Space), pn.Space, len(pn.Local), pn.Local)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// continuationMAC computes the token's authentication tag over the
// request's filter digest, the token's expiry, and the backend cursor.
func (h *Handler) continuationMAC(req xmlda.BrowseRequest, expiry int64, backendCursor string) string {
	mac := hmac.New(sha256.New, h.cpKey)
	// Length-prefixed, unambiguous framing: without it a cursor could be
	// crafted to absorb part of the filter digest and authenticate a
	// different (filters, cursor) pair than the one it is presented as.
	_, _ = fmt.Fprintf(mac, "%d\x00%s\x00%d\x00%d\x00%s",
		expiry, filterHash(req), len(backendCursor), len(backendCursor), backendCursor)
	return hex.EncodeToString(mac.Sum(nil))
}

// buildContinuationToken wraps a backend-issued cursor for the wire. An
// empty backendCursor (no more pages) yields an empty token.
func (h *Handler) buildContinuationToken(req xmlda.BrowseRequest, backendCursor string) string {
	if backendCursor == "" {
		return ""
	}
	expiry := h.clk.Now().Add(h.cfg.ContinuationPointTTL).Unix()
	return h.continuationMAC(req, expiry, backendCursor) + ":" +
		strconv.FormatInt(expiry, 10) + ":" + backendCursor
}

// parseContinuationToken extracts the backend cursor from a client-
// supplied token, verifying that this server issued it, for this filter
// set, and that it has not expired. An empty token (first call) is always
// valid, with an empty cursor.
//
// A backend still validates the cursor it gets back as ordinary input —
// a token this server issued can be replayed within its lifetime, and the
// address space it pointed into may have changed underneath. What the MAC
// guarantees is only that the cursor is one this server handed out, not
// one the client invented.
func (h *Handler) parseContinuationToken(token string, req xmlda.BrowseRequest) (backendCursor string, ok bool) {
	if token == "" {
		return "", true
	}
	gotMAC, rest, found := strings.Cut(token, ":")
	if !found {
		return "", false
	}
	expiryText, cursor, found := strings.Cut(rest, ":")
	if !found {
		return "", false
	}
	expiry, err := strconv.ParseInt(expiryText, 10, 64)
	if err != nil {
		return "", false
	}
	want := h.continuationMAC(req, expiry, cursor)
	// hmac.Equal, not ==: constant-time comparison, so a rejected token
	// leaks nothing about how far it matched.
	if !hmac.Equal([]byte(gotMAC), []byte(want)) {
		return "", false
	}
	if h.clk.Now().After(time.Unix(expiry, 0)) {
		return "", false
	}
	return cursor, true
}
