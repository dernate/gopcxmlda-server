package server

import (
	"context"
	"net/http"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/soap"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

func (h *Handler) handleGetProperties(ctx context.Context, w http.ResponseWriter, body []byte, state xmlda.ServerState) {
	var env soap.Envelope[xmlda.GetPropertiesRequest]
	if err := xmlda.Decode(body, &env); err != nil {
		h.metrics.IncRequestError("GetProperties", "parse")
		writeFaultWithStatus(w, requestDecodeFault("GetProperties", err), http.StatusBadRequest)
		return
	}
	req := env.Body.Content

	if h.backend.Properties == nil {
		writeFaultWithStatus(w, fault(xmlda.ErrNotSupported, "GetProperties is not supported by this server"), http.StatusInternalServerError)
		return
	}
	if !h.checkItemCount(len(req.ItemIDs)) {
		h.metrics.IncRequestError("GetProperties", "limit_exceeded")
		writeFaultWithStatus(w, limitExceededFault("too many items in one GetProperties request"), http.StatusBadRequest)
		return
	}

	refs := make([]backend.ItemRef, len(req.ItemIDs))
	reqs := make([]backend.PropertyRequest, len(req.ItemIDs))
	for i, id := range req.ItemIDs {
		ref := backend.ItemRef{ItemName: id.ItemName}
		switch {
		case id.ItemPath != nil:
			ref.ItemPath = *id.ItemPath
		case req.ItemPath != nil:
			ref.ItemPath = *req.ItemPath
		}
		refs[i] = ref
		var ids []xmlda.PropertyID
		// PropertyNames identifies properties by QName; translate any
		// standard-namespace ones back to PropertyID for the backend
		// contract (a vendor-namespace name has no PropertyID and is
		// simply not requestable via this path).
		for _, pn := range req.PropertyNames {
			if id, ok := standardPropertyIDFor(pn); ok {
				ids = append(ids, id)
			}
		}
		reqs[i] = backend.PropertyRequest{
			Ref:           ref,
			All:           req.ReturnAllProperties,
			PropertyIDs:   ids,
			IncludeValues: req.ReturnPropertyValues,
		}
	}

	results, err := h.backend.Properties.GetProperties(ctx, reqs)
	if err != nil {
		h.metrics.IncRequestError("GetProperties", "backend_error")
		writeFaultWithStatus(w, backendErrorFault(err), http.StatusInternalServerError)
		return
	}

	now := h.clk.Now()
	lists := make([]xmlda.PropertyReplyList, len(req.ItemIDs))
	var codes []xmlda.ErrorCode
	for i, id := range req.ItemIDs {
		// A conforming backend returns exactly one Result per requested
		// item, in the same order (docs/backend-implementation.md). A
		// backend that returns fewer resolves the missing tail to E_FAIL
		// rather than panicking on an out-of-range index.
		res := backend.Result[[]backend.Property]{ResultID: xmlda.ErrFail}
		if i < len(results) {
			res = results[i]
		}
		list := xmlda.PropertyReplyList{ItemName: id.ItemName, ResultID: res.ResultID}
		if id.ItemPath != nil {
			path := *id.ItemPath
			list.ItemPath = &path
		}
		for _, p := range res.Value {
			list.Properties = append(list.Properties, toItemProperty(p, req.ReturnPropertyValues))
		}
		lists[i] = list
		codes = append(codes, res.ResultID)
	}

	opts := xmlda.RequestOptions{
		ClientRequestHandle: req.ClientRequestHandle,
		LocaleID:            req.LocaleID,
		ReturnErrorText:     req.ReturnErrorText,
	}
	resp := xmlda.GetPropertiesResponse{
		Result: xmlda.ReplyBase{
			RcvTime:             now,
			ReplyTime:           now,
			ClientRequestHandle: req.ClientRequestHandle,
			RevisedLocaleID:     req.LocaleID,
			ServerState:         state,
		},
		PropertyLists: lists,
		Errors:        xmlda.DedupeErrors(codes, errorTextFunc(opts)),
	}
	writeResponse(w, resp)
}

func standardPropertyIDFor(qn xmlda.QName) (xmlda.PropertyID, bool) {
	for _, id := range []xmlda.PropertyID{
		xmlda.PropDataType, xmlda.PropValue, xmlda.PropQuality, xmlda.PropTimestamp,
		xmlda.PropAccessRights, xmlda.PropScanRate, xmlda.PropEUType, xmlda.PropEUInfo,
		xmlda.PropEngineeringUnits, xmlda.PropDescription, xmlda.PropHighEU, xmlda.PropLowEU,
		xmlda.PropHighIR, xmlda.PropLowIR, xmlda.PropCloseLabel, xmlda.PropOpenLabel, xmlda.PropTimeZone,
	} {
		if xmlda.StandardPropertyName(id) == qn {
			return id, true
		}
	}
	return 0, false
}
