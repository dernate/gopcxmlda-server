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
		writeFault(w, soapVersion(doc), requestDecodeFault("SubscriptionPolledRefresh", err))
		return
	}
	req := env.Body.Content

	now := h.clk.Now()
	if deadlinePassed(req.Options, now) {
		h.metrics.IncRequestError("SubscriptionPolledRefresh", "deadline_exceeded")
		writeFault(w, soapVersion(doc), fault(xmlda.ErrTimedOut, xmlda.StandardErrorText(xmlda.ErrTimedOut)))
		return
	}
	holdTime, code, bad := h.resolveHoldTime(req.HoldTime, now)
	if bad {
		h.metrics.IncRequestError("SubscriptionPolledRefresh", "invalid_hold_time")
		writeFault(w, soapVersion(doc), fault(code, xmlda.StandardErrorText(code)))
		return
	}

	// The handle list is bounded like every other per-request collection
	// (REQ-LIMITS-001): each valid handle costs fan-in goroutines for the
	// call's duration, so an unbounded list is an amplification vector.
	if !h.checkItemCount(len(req.ServerSubHandles)) {
		h.metrics.IncRequestError("SubscriptionPolledRefresh", "limit_exceeded")
		writeFault(w, soapVersion(doc), limitExceededFault("too many subscription handles in one SubscriptionPolledRefresh request"))
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
	if holdTime != nil {
		remaining := h.cfg.MaxPolledRefreshWait - max(holdTime.Sub(now), 0)
		waitTime = min(waitTime, max(remaining, 0))
	}

	res, err := h.subs.PolledRefresh(ctx, subscription.RefreshRequest{
		Handles:        handles,
		HoldTime:       holdTime,
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
		writeFault(w, soapVersion(doc), fault(code, xmlda.StandardErrorText(code)))
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
			items[i] = buildItemValue(it.Ref, it.ClientItemHandle, sample, haveSample, resultID,
				it.DiagnosticInfo, req.Options)
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
		Errors:                  buildErrors(codes, h.errorTextFunc(req.Options, oc)),
	}
	writeResponse(w, h.log, soapVersion(doc), resp)
}

// resolveHoldTime validates a requested HoldTime and returns the hold
// this server will actually honor, or the fault code to emit if the
// request is unusable (§3.1.9's E_INVALIDHOLDTIME).
//
// The exact zero time.Time (year 1, month 1, day 1) is always rejected:
// it is not a value any client would legitimately request, but the
// unmistakable signature of an uninitialized dateTime from a naive client
// library — which "hold until a time already past" would otherwise
// silently accept as "do not hold at all".
//
// A HoldTime beyond Config.MaxPolledRefreshWait is CLAMPED to that
// ceiling by default, not rejected. Rejecting it reads as the stricter,
// more honest answer, and it is what this used to do — but the
// specification's own guidance is a range ("generally no more than a
// minute or two", §3.1.6) while the ceiling is an exact number, so a
// client that picks two minutes against a shorter ceiling faults on every
// single poll and never receives its subscription's data at all. Clamping
// gives it a shorter hold and a valid reply; the request's own context
// deadline (MaxPolledRefreshWait + polledRefreshGrace) then cannot be
// raced, because the clamped hold is by construction inside it.
// Config.StrictHoldTime restores the rejecting behavior.
func (h *Handler) resolveHoldTime(holdTime *time.Time, now time.Time) (*time.Time, xmlda.ErrorCode, bool) {
	if holdTime == nil {
		return nil, xmlda.ErrorCode{}, false
	}
	if holdTime.IsZero() {
		return nil, xmlda.ErrInvalidHoldTime, true
	}
	ceiling := now.Add(h.cfg.MaxPolledRefreshWait)
	if holdTime.After(ceiling) {
		if h.cfg.StrictHoldTime {
			return nil, xmlda.ErrInvalidHoldTime, true
		}
		h.log.Debug("SubscriptionPolledRefresh HoldTime clamped to MaxPolledRefreshWait",
			"requested", holdTime.String(), "granted", ceiling.String())
		return &ceiling, xmlda.ErrorCode{}, false
	}
	return holdTime, xmlda.ErrorCode{}, false
}
