package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/dernate/gopcxmlda-server/soap"
	"github.com/dernate/gopcxmlda-server/subscription"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

func (h *Handler) handlePolledRefresh(ctx context.Context, w http.ResponseWriter, body []byte, state xmlda.ServerState) {
	var env soap.Envelope[xmlda.SubscriptionPolledRefreshRequest]
	if err := xmlda.Decode(body, &env); err != nil {
		h.metrics.IncRequestError("SubscriptionPolledRefresh", "parse")
		writeFaultWithStatus(w, requestDecodeFault("SubscriptionPolledRefresh", err), http.StatusBadRequest)
		return
	}
	req := env.Body.Content

	if deadlinePassed(req.Options, h.clk.Now()) {
		h.metrics.IncRequestError("SubscriptionPolledRefresh", "deadline_exceeded")
		writeFaultWithStatus(w, fault(xmlda.ErrTimedOut, xmlda.StandardErrorText(xmlda.ErrTimedOut)), http.StatusInternalServerError)
		return
	}
	// A HoldTime of the exact zero time.Time value (year 1, month 1, day
	// 1) is not a value any client would legitimately request — it is
	// the unmistakable signature of an uninitialized/default dateTime
	// slipping through from a naive client library rather than a
	// deliberate "hold since the distant past" request (which
	// holdDur := max(req.HoldTime.Sub(now), 0) would otherwise silently
	// accept as "don't hold at all"). E_INVALIDHOLDTIME (§3.1.9) is the
	// standard code for exactly this condition.
	if req.HoldTime != nil && req.HoldTime.IsZero() {
		h.metrics.IncRequestError("SubscriptionPolledRefresh", "invalid_hold_time")
		writeFaultWithStatus(w, fault(xmlda.ErrInvalidHoldTime, xmlda.StandardErrorText(xmlda.ErrInvalidHoldTime)), http.StatusBadRequest)
		return
	}

	handles := make([]subscription.Handle, len(req.ServerSubHandles))
	for i, s := range req.ServerSubHandles {
		handles[i] = subscription.Handle(s)
	}

	waitTime := min(time.Duration(req.WaitTime)*time.Millisecond, h.cfg.MaxPolledRefreshWait)

	res, err := h.subs.PolledRefresh(ctx, subscription.RefreshRequest{
		Handles:        handles,
		HoldTime:       req.HoldTime,
		WaitTime:       waitTime,
		ReturnAllItems: req.ReturnAllItems,
	})
	if err != nil {
		code := xmlda.ErrFail
		switch {
		case errors.Is(err, subscription.ErrBusy):
			code = xmlda.ErrBusy
		case errors.Is(err, subscription.ErrNoSubscription):
			code = xmlda.ErrNoSubscription
		case errors.Is(err, context.DeadlineExceeded):
			code = xmlda.ErrTimedOut
		}
		h.metrics.IncSubscriptionError(code.Local)
		writeFaultWithStatus(w, fault(code, xmlda.StandardErrorText(code)), http.StatusInternalServerError)
		return
	}

	now := h.clk.Now()
	invalidHandles := make([]string, len(res.InvalidHandles))
	for i, hd := range res.InvalidHandles {
		invalidHandles[i] = string(hd)
	}

	var rItemLists []xmlda.SubscriptionPolledRefreshReplyItemList
	var codes []xmlda.ErrorCode
	for _, subRes := range res.Subscriptions {
		items := make([]xmlda.ItemValue, len(subRes.Items))
		for i, it := range subRes.Items {
			items[i] = buildItemValue(it.Ref, it.ClientItemHandle, it.Sample, true, it.ResultID, "", req.Options)
			codes = append(codes, it.ResultID)
		}
		rItemLists = append(rItemLists, xmlda.SubscriptionPolledRefreshReplyItemList{
			SubscriptionHandle: string(subRes.Handle),
			Items:              items,
		})
	}

	resp := xmlda.SubscriptionPolledRefreshResponse{
		DataBufferOverflow: res.DataBufferOverflow,
		Result: xmlda.ReplyBase{
			RcvTime:             now,
			ReplyTime:           now,
			ClientRequestHandle: req.Options.ClientRequestHandle,
			RevisedLocaleID:     req.Options.LocaleID,
			ServerState:         state,
		},
		InvalidServerSubHandles: invalidHandles,
		RItemList:               rItemLists,
		Errors:                  xmlda.DedupeErrors(codes, errorTextFunc(req.Options)),
	}
	writeResponse(w, resp)
}
