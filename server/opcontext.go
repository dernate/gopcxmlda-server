package server

import (
	"strings"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// opContext carries the per-request facts every operation handler needs
// but cannot derive for itself: when the request was received, and the
// server status ServeHTTP already fetched to evaluate
// xmlda.RequiresFault.
//
// It exists so RcvTime, ReplyTime, RevisedLocaleID and ServerState are
// assembled in exactly one place (replyBase) for all eight operations,
// rather than being re-derived — and previously derived inconsistently —
// in each handler.
type opContext struct {
	// rcvTime is when the request was received, captured before any
	// backend call. REQ-TIME: ReplyBase.RcvTime means the receipt time,
	// not "some time during processing".
	rcvTime time.Time
	// status is the server status as of this request.
	status backend.ServerStatus
}

// replyBase builds the operation's "…Result" element. ReplyTime is taken
// at call time — immediately before the response is encoded — so it
// genuinely differs from rcvTime by the request's processing duration,
// which is the only reason the specification carries both.
func (h *Handler) replyBase(oc opContext, clientRequestHandle, requestedLocale string) xmlda.ReplyBase {
	return xmlda.ReplyBase{
		RcvTime:             oc.rcvTime,
		ReplyTime:           h.clk.Now(),
		ClientRequestHandle: clientRequestHandle,
		RevisedLocaleID:     reviseLocale(requestedLocale, oc.status.SupportedLocaleIDs),
		ServerState:         oc.status.State,
	}
}

// reviseLocale resolves the locale the server will actually report having
// used. RevisedLocaleID is specified as exactly that — the locale used,
// not an echo of the request — so a requested locale the backend does not
// support must not be reflected back as though it had been honored.
//
// Matching is case-insensitive because locale IDs are conventionally
// written with an uppercase region subtag ("de-DE") but are not
// case-sensitive identifiers; the server's own spelling from
// SupportedLocaleIDs is what gets reported, so the client sees the
// canonical form. An unsupported (or empty) request falls back to the
// backend's first supported locale — its default — or to "" if the
// backend declared none, in which case ReplyBase omits the attribute
// entirely rather than asserting a locale that does not exist.
func reviseLocale(requested string, supported []string) string {
	if len(supported) == 0 {
		return ""
	}
	for _, s := range supported {
		if strings.EqualFold(requested, s) {
			return s
		}
	}
	return supported[0]
}
