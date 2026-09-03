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
		writeFault(w, soapVersion(doc), requestDecodeFault("Read", err))
		return
	}
	req := env.Body.Content

	if deadlinePassed(req.Options, h.clk.Now()) {
		h.metrics.IncRequestError("Read", "deadline_exceeded")
		writeFault(w, soapVersion(doc), fault(xmlda.ErrTimedOut, xmlda.StandardErrorText(xmlda.ErrTimedOut)))
		return
	}
	// An empty (or absent) ItemList is schema-legal: both the element and
	// its Items are minOccurs="0", and §3.3.1 only says "It is expected
	// that there are one or more Items per ItemList". A request that asks
	// for nothing gets an empty, successful reply rather than a
	// whole-operation fault — refusing it invents a requirement the
	// schema does not state, and a client assembling its item list
	// dynamically hits it for a perfectly ordinary reason.
	if !h.checkItemCount(len(req.ItemList.Items)) {
		h.metrics.IncRequestError("Read", "limit_exceeded")
		writeFault(w, soapVersion(doc), limitExceededFault("too many items in one Read request"))
		return
	}

	// An item whose own attributes could not be interpreted is never sent
	// to the backend: it resolves to a per-item ResultID directly
	// (xmlda.ItemDecodeError), so one malformed MaxAge or unresolvable
	// ReqType costs the client that item instead of the entire response.
	// readIdx maps each backend request slot back to its position in
	// req.ItemList.Items.
	readItems := make([]backend.ReadRequestItem, 0, len(req.ItemList.Items))
	readIdx := make([]int, 0, len(req.ItemList.Items))
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
		if it.DecodeErr != nil {
			continue
		}
		// A negative MaxAge is legal xsd:int with no meaning for
		// "maximum acceptable age"; 0 ("most accurate / force a device
		// read", REQ-READ-004) is the conservative reading.
		var maxAge time.Duration
		if p.MaxAge != nil && *p.MaxAge > 0 {
			maxAge = time.Duration(*p.MaxAge) * time.Millisecond
		}
		readItems = append(readItems, backend.ReadRequestItem{Ref: ref, MaxAge: maxAge})
		readIdx = append(readIdx, i)
	}

	results := make([]backend.Result[backend.ItemSample], len(req.ItemList.Items))
	for i := range results {
		// The default for a slot no backend result will fill: an item
		// rejected at decode time overwrites this below with its own
		// condition, and a conforming backend fills every other slot.
		results[i] = backend.Result[backend.ItemSample]{ResultID: xmlda.ErrFail}
	}
	if len(readItems) > 0 {
		backendResults, err := observeBackend(ctx, h.metrics, h.clk, "Read", h.cfg.BackendTimeout, func() ([]backend.Result[backend.ItemSample], error) {
			return h.backend.Reader.Read(ctx, readItems)
		})
		if err != nil {
			h.metrics.IncRequestError("Read", "backend_error")
			writeFault(w, soapVersion(doc), backendErrorFault(err))
			return
		}
		// A conforming backend returns exactly one Result per requested
		// item, in the same order (docs/backend-implementation.md). A
		// backend that returns fewer leaves the missing tail at the
		// E_FAIL default above rather than panicking on an out-of-range
		// index.
		for j, i := range readIdx {
			if j < len(backendResults) {
				results[i] = backendResults[j]
			}
		}
	}

	items := make([]xmlda.ItemValue, len(req.ItemList.Items))
	var codes []xmlda.ErrorCode
	for i, it := range req.ItemList.Items {
		if it.DecodeErr != nil {
			iv, code := buildItemDecodeFailure(refs[i], it.ClientItemHandle, it.DecodeErr, req.Options)
			items[i] = iv
			codes = append(codes, code)
			continue
		}
		res := results[i]
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

		// The value-presence guard mirrors applyReqType's: an item with no
		// value to coerce must keep its real condition rather than being
		// relabelled E_BADTYPE.
		if haveSample && merged[i].ReqType != nil && sample.Value.IsValid() && !sample.Value.IsNil() {
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
		Errors:    buildErrors(codes, h.errorTextFunc(req.Options, oc)),
	}
	writeResponse(w, h.log, soapVersion(doc), resp)
}
