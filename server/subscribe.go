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

func (h *Handler) handleSubscribe(ctx context.Context, w http.ResponseWriter, body []byte, state xmlda.ServerState) {
	var env soap.Envelope[xmlda.SubscribeRequest]
	if err := xmlda.Decode(body, &env); err != nil {
		h.metrics.IncRequestError("Subscribe", "parse")
		writeFaultWithStatus(w, requestDecodeFault("Subscribe", err), http.StatusBadRequest)
		return
	}
	req := env.Body.Content

	if deadlinePassed(req.Options, h.clk.Now()) {
		h.metrics.IncRequestError("Subscribe", "deadline_exceeded")
		writeFaultWithStatus(w, fault(xmlda.ErrTimedOut, xmlda.StandardErrorText(xmlda.ErrTimedOut)), http.StatusInternalServerError)
		return
	}
	if len(req.ItemList.Items) == 0 {
		h.metrics.IncRequestError("Subscribe", "empty_item_list")
		writeFaultWithStatus(w, fault(xmlda.ErrFail, "at least one item is required"), http.StatusBadRequest)
		return
	}
	if !h.checkSubscriptionItemCount(len(req.ItemList.Items)) {
		h.metrics.IncRequestError("Subscribe", "limit_exceeded")
		writeFaultWithStatus(w, limitExceededFault("too many items in one Subscribe request"), http.StatusBadRequest)
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
			writeFaultWithStatus(w, limitExceededFault(err.Error()), http.StatusBadRequest)
			return
		}
		h.metrics.IncRequestError("Subscribe", "backend_error")
		writeFaultWithStatus(w, backendErrorFault(err), http.StatusInternalServerError)
		return
	}

	now := h.clk.Now()
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
		Result: xmlda.ReplyBase{
			RcvTime:             now,
			ReplyTime:           now,
			ClientRequestHandle: req.Options.ClientRequestHandle,
			RevisedLocaleID:     req.Options.LocaleID,
			ServerState:         state,
		},
		RItemList: xmlda.SubscribeReplyItemList{Items: listItems},
		Errors:    xmlda.DedupeErrors(codes, errorTextFunc(req.Options)),
	}
	writeResponse(w, resp)
}
