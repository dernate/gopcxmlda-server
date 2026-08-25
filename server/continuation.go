package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/dernate/gopcxmlda-server/xmlda"
)

// Browse's ContinuationPoint enforcement is a framework concern, not a
// backend one (REQ-BROWSE-002): the wire-visible token is
// "<filterHash>:<backendCursor>", where filterHash covers every field
// that must remain identical across a continued Browse call. Backends
// only ever see/return their own private, opaque cursor half.

func filterHash(req xmlda.BrowseRequest) string {
	h := sha256.New()
	itemPath := ""
	if req.ItemPath != nil {
		itemPath = *req.ItemPath
	}
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%t\x00%t",
		req.ItemName, itemPath, req.BrowseFilter, req.ElementNameFilter, req.VendorFilter,
		req.MaxElementsReturned, req.ReturnAllProperties, req.ReturnPropertyValues)
	return hex.EncodeToString(h.Sum(nil))
}

// buildContinuationToken wraps a backend-issued cursor for the wire. An
// empty backendCursor (no more pages) yields an empty token.
func buildContinuationToken(req xmlda.BrowseRequest, backendCursor string) string {
	if backendCursor == "" {
		return ""
	}
	return filterHash(req) + ":" + backendCursor
}

// parseContinuationToken extracts the backend cursor from a client-
// supplied token, verifying it was issued for the identical filter set.
// An empty token (first call) is always valid, with an empty cursor.
func parseContinuationToken(token string, req xmlda.BrowseRequest) (backendCursor string, ok bool) {
	if token == "" {
		return "", true
	}
	hash, cursor, ok := strings.Cut(token, ":")
	if !ok {
		return "", false
	}
	if hash != filterHash(req) {
		return "", false
	}
	return cursor, true
}
