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

func (h *Handler) handlePolledRefresh(ctx context.Context, w http.ResponseWriter, doc *xmlda.Document, oc opContext) {
	var env soap.Envelope[xmlda.SubscriptionPolledRefreshRequest]
	if err := doc.Decode(&env); err != nil {
		h.metrics.IncRequestError("SubscriptionPolledRefresh", "parse")
		writeFault(w, requestDecodeFault("SubscriptionPolledRefresh", err))
		return
	}
	req := env.Body.Content

	now := h.clk.Now()
	if deadlinePassed(req.Options, now) {
		h.metrics.IncRequestError("SubscriptionPolledRefresh", "deadline_exceeded")
		writeFault(w, fault(xmlda.ErrTimedOut, xmlda.StandardErrorText(xmlda.ErrTimedOut)))
		return
	}
	if code, bad := h.checkHoldTime(req.HoldTime, now); bad {
		h.metrics.IncRequestError("SubscriptionPolledRefresh", "invalid_hold_time")
		writeFault(w, fault(code, xmlda.StandardErrorText(code)))
		return
	}

	// The handle list is bounded like every other per-request collection
	// (REQ-LIMITS-001): each valid handle costs fan-in goroutines for the
	// call's duration, so an unbounded list is an amplification vector.
	if !h.checkItemCount(len(req.ServerSubHandles)) {
		h.metrics.IncRequestError("SubscriptionPolledRefresh", "limit_exceeded")
		writeFault(w, limitExceededFault("too many subscription handles in one SubscriptionPolledRefresh request"))
		return
	}

	handles := make([]subscription.Handle, len(req.ServerSubHandles))
	for i, s := range req.ServerSubHandles {
		handles[i] = subscription.Handle(s)
	}

	// Hold and Wait share one budget: capping each separately still let
	// their sum reach twice the cap, which then raced the request's own
	// context deadline.
	waitTime := min(msToDuration(req.WaitTime), h.cfg.MaxPolledRefreshWait)
	if req.HoldTime != nil {
		remaining := h.cfg.MaxPolledRefreshWait - max(req.HoldTime.Sub(now), 0)
		waitTime = min(waitTime, max(remaining, 0))
	}

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
		case errors.Is(err, context.Canceled):
			// The client hung up, or the server is shutting down; either
			// way there is nobody left to read a fault, but E_SERVERSTATE
			// is the honest code rather than a generic E_FAIL.
			code = xmlda.ErrServerState
		}
		h.metrics.IncSubscriptionError(code.Local)
		writeFault(w, fault(code, xmlda.StandardErrorText(code)))
		return
	}

	invalidHandles := make([]string, len(res.InvalidHandles))
	for i, hd := range res.InvalidHandles {
		invalidHandles[i] = string(hd)
	}

	var rItemLists []xmlda.SubscriptionPolledRefreshReplyItemList
	var codes []xmlda.ErrorCode
	for _, subRes := range res.Subscriptions {
		items := make([]xmlda.ItemValue, len(subRes.Items))
		for i, it := range subRes.Items {
			// HaveSample, not a hardcoded true: an entry reporting an
			// abnormal condition (ResultID set) carries no sample, and
			// building a wire Value from its blank one produced a
			// Good-quality, typeless value that then failed to encode —
			// turning one failing item into a whole-operation E_FAIL for
			// the entire subscription.
			sample, haveSample, resultID := applyReqType(it.Sample, it.HaveSample, it.ResultID, it.ReqType)
			items[i] = buildItemValue(it.Ref, it.ClientItemHandle, sample, haveSample, resultID, "", req.Options)
			codes = append(codes, resultID)
		}
		rItemLists = append(rItemLists, xmlda.SubscriptionPolledRefreshReplyItemList{
			SubscriptionHandle: string(subRes.Handle),
			Items:              items,
		})
	}

	resp := xmlda.SubscriptionPolledRefreshResponse{
		DataBufferOverflow:      res.DataBufferOverflow,
		Result:                  h.replyBase(oc, req.Options.ClientRequestHandle, req.Options.LocaleID),
		InvalidServerSubHandles: invalidHandles,
		RItemList:               rItemLists,
		Errors:                  xmlda.DedupeErrors(codes, h.errorTextFunc(req.Options, oc)),
	}
	writeResponse(w, resp)
}

// checkHoldTime validates a requested HoldTime, reporting the fault code
// to emit if it is unusable (§3.1.9's E_INVALIDHOLDTIME).
//
// Two cases are rejected. The exact zero time.Time (year 1, month 1, day
// 1) is not a value any client would legitimately request — it is the
// unmistakable signature of an uninitialized dateTime from a naive client
// library, which "hold until a time already past" would otherwise
// silently accept as "do not hold at all". And a HoldTime further out
// than Config.MaxPolledRefreshWait is more than this server will block
// for: rejecting it says so, where letting it run would hit the request's
// own context deadline and return E_TIMEDOUT after discarding the
// subscription's buffered changes.
func (h *Handler) checkHoldTime(holdTime *time.Time, now time.Time) (xmlda.ErrorCode, bool) {
	if holdTime == nil {
		return xmlda.ErrorCode{}, false
	}
	if holdTime.IsZero() || holdTime.Sub(now) > h.cfg.MaxPolledRefreshWait {
		return xmlda.ErrInvalidHoldTime, true
	}
	return xmlda.ErrorCode{}, false
}
