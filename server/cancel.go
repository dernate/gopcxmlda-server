package server

import (
	"context"
	"net/http"

	"github.com/dernate/gopcxmlda-server/soap"
	"github.com/dernate/gopcxmlda-server/subscription"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// handleSubscriptionCancel implements SubscriptionCancel. Per
// REQ-SUBSCRIPTION-011, the response has no ReplyBase at all, so this
// handler — unlike every other operation — does not need the current
// ServerState.
func (h *Handler) handleSubscriptionCancel(ctx context.Context, w http.ResponseWriter, doc *xmlda.Document) {
	var env soap.Envelope[xmlda.SubscriptionCancelRequest]
	if err := doc.Decode(&env); err != nil {
		h.metrics.IncRequestError("SubscriptionCancel", "parse")
		writeFault(w, requestDecodeFault("SubscriptionCancel", err))
		return
	}
	req := env.Body.Content

	// Idempotent no-op on an unknown or already-cancelled handle —
	// REQ-SUBSCRIPTION-014; the return value is intentionally not
	// inspected, since the response has no field to report it in anyway.
	h.subs.Cancel(subscription.Handle(req.ServerSubHandle))

	writeResponse(w, xmlda.SubscriptionCancelResponse{ClientRequestHandle: req.ClientRequestHandle})
}
