package server

import (
	"context"
	"net/http"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/soap"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

func (h *Handler) handleWrite(ctx context.Context, w http.ResponseWriter, body []byte, state xmlda.ServerState) {
	var env soap.Envelope[xmlda.WriteRequest]
	if err := xmlda.Decode(body, &env); err != nil {
		h.metrics.IncRequestError("Write", "parse")
		writeFaultWithStatus(w, requestDecodeFault("Write", err), http.StatusBadRequest)
		return
	}
	req := env.Body.Content

	if deadlinePassed(req.Options, h.clk.Now()) {
		h.metrics.IncRequestError("Write", "deadline_exceeded")
		writeFaultWithStatus(w, fault(xmlda.ErrTimedOut, xmlda.StandardErrorText(xmlda.ErrTimedOut)), http.StatusInternalServerError)
		return
	}
	if len(req.ItemList.Items) == 0 {
		h.metrics.IncRequestError("Write", "empty_item_list")
		writeFaultWithStatus(w, fault(xmlda.ErrFail, "at least one item is required"), http.StatusBadRequest)
		return
	}
	if !h.checkItemCount(len(req.ItemList.Items)) {
		h.metrics.IncRequestError("Write", "limit_exceeded")
		writeFaultWithStatus(w, limitExceededFault("too many items in one Write request"), http.StatusBadRequest)
		return
	}

	now := h.clk.Now()
	readOnly := h.cfg.ReadOnly || h.backend.Writer == nil

	refs := make([]backend.ItemRef, len(req.ItemList.Items))
	for i, it := range req.ItemList.Items {
		ref := backend.ItemRef{ItemName: it.ItemName}
		if it.ItemPath != nil {
			ref.ItemPath = *it.ItemPath
		}
		refs[i] = ref
	}

	results := make([]backend.Result[backend.WriteOutcome], len(req.ItemList.Items))
	if readOnly {
		for i := range results {
			results[i] = backend.Result[backend.WriteOutcome]{ResultID: xmlda.ErrAccessDenied}
		}
	} else {
		// An item with no <Value> element (it.Value == nil, REQ-WRITE-003
		// requires one) can never be sent to the backend — it is excluded
		// from writeItems and resolved to E_BADTYPE directly, rather than
		// dereferencing a nil pointer. origIdx maps each writeItems slot
		// back to its position in req.ItemList.Items/results.
		writeItems := make([]backend.WriteRequestItem, 0, len(req.ItemList.Items))
		origIdx := make([]int, 0, len(req.ItemList.Items))
		for i, it := range req.ItemList.Items {
			if it.Value == nil {
				results[i] = backend.Result[backend.WriteOutcome]{ResultID: xmlda.ErrBadType}
				continue
			}
			wi := backend.WriteRequestItem{Ref: refs[i], Value: *it.Value, Timestamp: it.Timestamp}
			// Comparing against the zero OPCQuality distinguishes "no
			// <Quality> element in the request" (zero value, nil internal
			// pointers) from "a <Quality> element was present" (non-nil
			// pointers, even if it explicitly specified the defaults) —
			// see xmlda.OPCQuality's decode behavior. Quality and
			// Timestamp are tracked independently here so a client
			// writing only one of the two doesn't spuriously synthesize
			// the other.
			if it.Quality != (xmlda.OPCQuality{}) {
				q := it.Quality
				wi.Quality = &q
			}
			writeItems = append(writeItems, wi)
			origIdx = append(origIdx, i)
		}
		if len(writeItems) > 0 {
			backendResults, err := h.backend.Writer.Write(ctx, writeItems)
			if err != nil {
				h.metrics.IncRequestError("Write", "backend_error")
				writeFaultWithStatus(w, backendErrorFault(err), http.StatusInternalServerError)
				return
			}
			// A conforming backend returns exactly one Result per
			// requested item, in the same order (docs/backend-implementation.md).
			// A backend that returns fewer resolves the missing tail to
			// E_FAIL rather than panicking on an out-of-range index.
			for j, i := range origIdx {
				if j < len(backendResults) {
					results[i] = backendResults[j]
				} else {
					results[i] = backend.Result[backend.WriteOutcome]{ResultID: xmlda.ErrFail}
				}
			}
		}
	}

	items := make([]xmlda.ItemValue, len(req.ItemList.Items))
	var codes []xmlda.ErrorCode
	for i, it := range req.ItemList.Items {
		res := results[i]
		var sample backend.ItemSample
		haveSample := false
		if req.ReturnValuesOnReply && res.ResultID.IsZero() {
			if it.Value != nil {
				sample.Value = *it.Value
			}
			if res.Value.Value != nil {
				sample.Value = *res.Value.Value
			}
			if res.Value.Quality != nil {
				sample.Quality = *res.Value.Quality
			} else {
				sample.Quality = xmlda.NewGoodQuality()
			}
			if res.Value.Timestamp != nil {
				sample.Timestamp = *res.Value.Timestamp
			} else {
				sample.Timestamp = now
			}
			haveSample = true
		}
		resultID := res.ResultID
		if resultID.IsZero() && res.Value.Clamped {
			resultID = xmlda.SuccessClamp
		}
		items[i] = buildItemValue(refs[i], it.ClientItemHandle, sample, haveSample, resultID, res.DiagnosticInfo, req.Options)
		codes = append(codes, resultID)
	}

	resp := xmlda.WriteResponse{
		Result: xmlda.ReplyBase{
			RcvTime:             now,
			ReplyTime:           now,
			ClientRequestHandle: req.Options.ClientRequestHandle,
			RevisedLocaleID:     req.Options.LocaleID,
			ServerState:         state,
		},
		RItemList: xmlda.ItemValueList{Items: items},
		Errors:    xmlda.DedupeErrors(codes, errorTextFunc(req.Options)),
	}
	writeResponse(w, resp)
}
