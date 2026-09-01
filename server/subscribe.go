package server

import (
	"context"
	"errors"
	"math"
	"net/http"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/soap"
	"github.com/dernate/gopcxmlda-server/subscription"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// msInt32 renders a Duration as the millisecond count the wire's xsd:int
// fields carry, saturating instead of wrapping.
//
// The client's own RequestedSamplingRate arrives as an xsd:int and so
// cannot overflow on the way back out, but a server-side default
// (Config.DefaultSamplingRate) or a backend-revised rate is an arbitrary
// time.Duration — and a plain int32 conversion of one over ~24 days
// wraps to a negative RevisedSamplingRate, telling the client the server
// will sample in the past.
func msInt32(d time.Duration) int32 {
	ms := d / time.Millisecond
	if ms > math.MaxInt32 {
		return math.MaxInt32
	}
	if ms < 0 {
		return 0
	}
	return int32(ms)
}

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
	// Both limits, not just the per-subscription one: MaxItemsPerRequest is
	// documented as bounding "a single Read/Write/Subscribe/GetProperties
	// request", and a deployment that lowers it expects that to hold for
	// Subscribe too.
	if !h.checkItemCount(len(req.ItemList.Items)) || !h.checkSubscriptionItemCount(len(req.ItemList.Items)) {
		h.metrics.IncRequestError("Subscribe", "limit_exceeded")
		writeFault(w, limitExceededFault("too many items in one Subscribe request"))
		return
	}

	// An item whose own attributes could not be interpreted is never
	// offered to the subscription engine: it resolves to a per-item
	// ResultID directly (xmlda.ItemDecodeError), so one malformed
	// Deadband or unresolvable ReqType costs the client that item instead
	// of the whole subscription. engineIdx maps each engine request slot
	// back to its position in req.ItemList.Items.
	items := make([]subscription.CreateItemRequest, 0, len(req.ItemList.Items))
	engineIdx := make([]int, 0, len(req.ItemList.Items))
	refs := make([]backend.ItemRef, len(req.ItemList.Items))
	clientHandles := make([]string, len(req.ItemList.Items))
	decodeErrs := make([]error, len(req.ItemList.Items))
	for i, it := range req.ItemList.Items {
		p := xmlda.MergeItemParams(req.Params, req.ItemList.Params, it.Params)
		ref := backend.ItemRef{ItemName: it.ItemName}
		if p.ItemPath != nil {
			ref.ItemPath = *p.ItemPath
		}
		refs[i] = ref
		clientHandles[i] = it.ClientItemHandle
		if it.DecodeErr != nil {
			decodeErrs[i] = it.DecodeErr
			continue
		}

		// A negative RequestedSamplingRate is legal xsd:int that means
		// nothing here; treat it as 0 ("fastest practical"), which is
		// also what an absent attribute means.
		var rate time.Duration
		if p.RequestedSamplingRate != nil && *p.RequestedSamplingRate > 0 {
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
		items = append(items, subscription.CreateItemRequest{
			Ref:                   ref,
			ClientItemHandle:      it.ClientItemHandle,
			RequestedSamplingRate: rate,
			Deadband:              deadband,
			EnableBuffering:       enableBuffering,
			// ReqType is hierarchical for Subscribe exactly as it is for
			// Read (the schema carries it on SubscribeRequestItemList and
			// SubscribeRequestItem alike). It used to be merged here and
			// then dropped, so a client subscribing an unsignedShort item
			// as xsd:double silently got unsignedShort back — neither the
			// conversion it asked for nor the E_BADTYPE that would have
			// told it so.
			ReqType: p.ReqType,
		})
		engineIdx = append(engineIdx, i)
	}

	// Every item was rejected at decode time: there is nothing to
	// subscribe, so no subscription is created and ServerSubHandle stays
	// empty — the same outcome REQ-SUBSCRIPTION-002 defines for a request
	// whose every item the backend rejected.
	var res subscription.CreateResult
	if len(items) > 0 {
		var err error
		res, err = h.subs.Create(ctx, subscription.CreateRequest{
			Items:                items,
			ReturnValuesOnReply:  req.ReturnValuesOnReply,
			SubscriptionPingRate: msToDuration(req.SubscriptionPingRate),
		})
		if err != nil {
			if errors.Is(err, subscription.ErrTooManySubscriptions) || errors.Is(err, subscription.ErrTooManyItems) {
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
	}

	// Fan the engine's results back out to their original request
	// positions, so the reply's item order matches the request's — which
	// is how a client without ClientItemHandles matches them up at all.
	engineResults := make([]subscription.CreateItemResult, len(req.ItemList.Items))
	for j, i := range engineIdx {
		if j < len(res.Items) {
			engineResults[i] = res.Items[j]
			continue
		}
		engineResults[i] = subscription.CreateItemResult{
			ClientItemHandle: clientHandles[i],
			ResultID:         xmlda.ErrFail,
		}
	}

	listItems := make([]xmlda.SubscribeItemValue, len(req.ItemList.Items))
	codes := make([]xmlda.ErrorCode, 0, len(req.ItemList.Items))
	// listRate is the list-level RevisedSamplingRate: the slowest rate
	// among the items actually subscribed. The attribute used to go out as
	// a flat 0 for every subscription, which is not a rate any item was
	// given. The slowest is the honest aggregate — it is the only single
	// number the whole subscription actually honors.
	var listRate time.Duration
	for i := range req.ItemList.Items {
		if decodeErrs[i] != nil {
			iv, code := buildItemDecodeFailure(refs[i], clientHandles[i], decodeErrs[i], req.Options)
			listItems[i] = xmlda.SubscribeItemValue{ItemValue: iv}
			codes = append(codes, code)
			continue
		}
		itemRes := engineResults[i]
		sample, haveSample, resultID := applyReqType(itemRes.Sample, itemRes.HaveSample, itemRes.ResultID, itemRes.ReqType)
		iv := buildItemValue(refs[i], itemRes.ClientItemHandle, sample, haveSample, resultID, "", req.Options)
		listItems[i] = xmlda.SubscribeItemValue{
			RevisedSamplingRate: msInt32(itemRes.RevisedSamplingRate),
			ItemValue:           iv,
		}
		codes = append(codes, resultID)
		if hasUsableValue(resultID) && itemRes.RevisedSamplingRate > listRate {
			listRate = itemRes.RevisedSamplingRate
		}
	}

	resp := xmlda.SubscribeResponse{
		ServerSubHandle: string(res.Handle),
		Result:          h.replyBase(oc, req.Options.ClientRequestHandle, req.Options.LocaleID),
		RItemList: xmlda.SubscribeReplyItemList{
			RevisedSamplingRate: msInt32(listRate),
			Items:               listItems,
		},
		Errors: xmlda.DedupeErrors(codes, h.errorTextFunc(req.Options, oc)),
	}
	writeResponse(w, resp)
}
