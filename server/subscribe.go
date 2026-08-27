package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/soap"
	"github.com/dernate/gopcxmlda-server/subscription"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

func (h *Handler) handleSubscribe(ctx context.Context, w http.ResponseWriter, doc *xmlda.Document, oc opContext) {
	var env soap.Envelope[xmlda.SubscribeRequest]
	if err := doc.Decode(&env); err != nil {
		h.metrics.IncRequestError("Subscribe", "parse")
		writeFault(w, requestDecodeFault("Subscribe", err))
		return
	}
	req := env.Body.Content

	if deadlinePassed(req.Options, h.clk.Now()) {
		h.metrics.IncRequestError("Subscribe", "deadline_exceeded")
		writeFault(w, fault(xmlda.ErrTimedOut, xmlda.StandardErrorText(xmlda.ErrTimedOut)))
		return
	}
	if len(req.ItemList.Items) == 0 {
		h.metrics.IncRequestError("Subscribe", "empty_item_list")
		writeFault(w, fault(xmlda.ErrFail, "at least one item is required"))
		return
	}
	if !h.checkSubscriptionItemCount(len(req.ItemList.Items)) {
		h.metrics.IncRequestError("Subscribe", "limit_exceeded")
		writeFault(w, limitExceededFault("too many items in one Subscribe request"))
		return
	}

	items := make([]subscription.CreateItemRequest, len(req.ItemList.Items))
	refs := make([]backend.ItemRef, len(req.ItemList.Items))
	for i, it := range req.ItemList.Items {
		p := xmlda.MergeItemParams(req.Params, req.ItemList.Params, it.Params)
		ref := backend.ItemRef{ItemName: it.ItemName}
		if p.ItemPath != nil {
			ref.ItemPath = *p.ItemPath
		}
		refs[i] = ref

		var rate time.Duration
		if p.RequestedSamplingRate != nil {
			rate = time.Duration(*p.RequestedSamplingRate) * time.Millisecond
		}
		var deadband float64
		if p.Deadband != nil {
			deadband = *p.Deadband
		}
		var enableBuffering bool
		if p.EnableBuffering != nil {
			enableBuffering = *p.EnableBuffering
		}
		items[i] = subscription.CreateItemRequest{
			Ref:                   ref,
			ClientItemHandle:      it.ClientItemHandle,
			RequestedSamplingRate: rate,
			Deadband:              deadband,
			EnableBuffering:       enableBuffering,
		}
	}

	res, err := h.subs.Create(ctx, subscription.CreateRequest{
		Items:                items,
		ReturnValuesOnReply:  req.ReturnValuesOnReply,
		SubscriptionPingRate: time.Duration(req.SubscriptionPingRate) * time.Millisecond,
	})
	if err != nil {
		if errors.Is(err, subscription.ErrTooManySubscriptions) {
			h.metrics.IncRequestError("Subscribe", "limit_exceeded")
			writeFault(w, limitExceededFault(err.Error()))
			return
		}
		if errors.Is(err, subscription.ErrShuttingDown) {
			// A shutting-down server is a server-state condition, not the
			// generic E_FAIL a bare context.Canceled used to produce.
			h.metrics.IncRequestError("Subscribe", "server_state")
			writeFault(w, fault(xmlda.ErrServerState, xmlda.StandardErrorText(xmlda.ErrServerState)))
			return
		}
		h.metrics.IncRequestError("Subscribe", "backend_error")
		writeFault(w, backendErrorFault(err))
		return
	}

	listItems := make([]xmlda.SubscribeItemValue, len(res.Items))
	codes := make([]xmlda.ErrorCode, len(res.Items))
	for i, itemRes := range res.Items {
		iv := buildItemValue(refs[i], itemRes.ClientItemHandle, itemRes.Sample, itemRes.HaveSample, itemRes.ResultID, "", req.Options)
		listItems[i] = xmlda.SubscribeItemValue{
			RevisedSamplingRate: uint32(itemRes.RevisedSamplingRate / time.Millisecond),
			ItemValue:           iv,
		}
		codes[i] = itemRes.ResultID
	}

	resp := xmlda.SubscribeResponse{
		ServerSubHandle: string(res.Handle),
		Result:          h.replyBase(oc, req.Options.ClientRequestHandle, req.Options.LocaleID),
		RItemList:       xmlda.SubscribeReplyItemList{Items: listItems},
		Errors:          xmlda.DedupeErrors(codes, errorTextFunc(req.Options)),
	}
	writeResponse(w, resp)
}
