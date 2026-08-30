package server

import (
	"context"
	"net/http"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/soap"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

func (h *Handler) handleRead(ctx context.Context, w http.ResponseWriter, doc *xmlda.Document, oc opContext) {
	var env soap.Envelope[xmlda.ReadRequest]
	if err := doc.Decode(&env); err != nil {
		h.metrics.IncRequestError("Read", "parse")
		writeFault(w, requestDecodeFault("Read", err))
		return
	}
	req := env.Body.Content

	if deadlinePassed(req.Options, h.clk.Now()) {
		h.metrics.IncRequestError("Read", "deadline_exceeded")
		writeFault(w, fault(xmlda.ErrTimedOut, xmlda.StandardErrorText(xmlda.ErrTimedOut)))
		return
	}
	if len(req.ItemList.Items) == 0 {
		h.metrics.IncRequestError("Read", "empty_item_list")
		writeFault(w, fault(xmlda.ErrFail, "at least one item is required"))
		return
	}
	if !h.checkItemCount(len(req.ItemList.Items)) {
		h.metrics.IncRequestError("Read", "limit_exceeded")
		writeFault(w, limitExceededFault("too many items in one Read request"))
		return
	}

	readItems := make([]backend.ReadRequestItem, len(req.ItemList.Items))
	merged := make([]xmlda.ItemParams, len(req.ItemList.Items))
	refs := make([]backend.ItemRef, len(req.ItemList.Items))
	for i, it := range req.ItemList.Items {
		p := xmlda.MergeItemParams(req.Params, req.ItemList.Params, it.Params)
		merged[i] = p
		ref := backend.ItemRef{ItemName: it.ItemName}
		if p.ItemPath != nil {
			ref.ItemPath = *p.ItemPath
		}
		refs[i] = ref
		// A negative MaxAge is legal xsd:int with no meaning for
		// "maximum acceptable age"; 0 ("most accurate / force a device
		// read", REQ-READ-004) is the conservative reading.
		var maxAge time.Duration
		if p.MaxAge != nil && *p.MaxAge > 0 {
			maxAge = time.Duration(*p.MaxAge) * time.Millisecond
		}
		readItems[i] = backend.ReadRequestItem{Ref: ref, MaxAge: maxAge}
	}

	results, err := observeBackend(h.metrics, h.clk, "Read", func() ([]backend.Result[backend.ItemSample], error) {
		return h.backend.Reader.Read(ctx, readItems)
	})
	if err != nil {
		h.metrics.IncRequestError("Read", "backend_error")
		writeFault(w, backendErrorFault(err))
		return
	}

	items := make([]xmlda.ItemValue, len(req.ItemList.Items))
	var codes []xmlda.ErrorCode
	for i, it := range req.ItemList.Items {
		// A conforming backend returns exactly one Result per requested
		// item, in the same order (docs/backend-implementation.md). A
		// backend that returns fewer resolves the missing tail to E_FAIL
		// rather than panicking on an out-of-range index.
		res := backend.Result[backend.ItemSample]{ResultID: xmlda.ErrFail}
		if i < len(results) {
			res = results[i]
		}
		resultID := res.ResultID
		sample := res.Value
		// A success-with-caveat code still carries a usable value: §2.6
		// is explicit that "in case of a critical error the returned
		// value may not be useful. For non-critical exceptions the
		// returned value IS useful, although the client may need to
		// react to an abnormal condition." Treating every non-zero
		// ResultID as "no sample" silently dropped the value for
		// S_CLAMP/S_DATAQUEUEOVERFLOW/S_UNSUPPORTEDRATE — the one case
		// where the client is entitled to both the code and the data.
		haveSample := hasUsableValue(resultID)

		if haveSample && merged[i].ReqType != nil {
			coerced, ok := coerceToReqType(sample.Value, merged[i].ReqType)
			if !ok {
				resultID = xmlda.ErrBadType
				haveSample = false
			} else {
				sample.Value = coerced
			}
		}

		items[i] = buildItemValue(refs[i], it.ClientItemHandle, sample, haveSample, resultID, res.DiagnosticInfo, req.Options)
		codes = append(codes, resultID)
	}

	resp := xmlda.ReadResponse{
		Result:    h.replyBase(oc, req.Options.ClientRequestHandle, req.Options.LocaleID),
		RItemList: xmlda.ItemValueList{Items: items},
		Errors:    xmlda.DedupeErrors(codes, h.errorTextFunc(req.Options, oc)),
	}
	writeResponse(w, resp)
}
