package server

import (
	"context"
	"net/http"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/soap"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

func (h *Handler) handleWrite(ctx context.Context, w http.ResponseWriter, doc *xmlda.Document, oc opContext) {
	var env soap.Envelope[xmlda.WriteRequest]
	if err := doc.Decode(&env); err != nil {
		h.metrics.IncRequestError("Write", "parse")
		writeFault(w, soapVersion(doc), requestDecodeFault("Write", err))
		return
	}
	req := env.Body.Content

	if deadlinePassed(req.Options, h.clk.Now()) {
		h.metrics.IncRequestError("Write", "deadline_exceeded")
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
		h.metrics.IncRequestError("Write", "limit_exceeded")
		writeFault(w, soapVersion(doc), limitExceededFault("too many items in one Write request"))
		return
	}

	now := h.clk.Now()
	readOnly := h.cfg.ReadOnly || h.backend.Writer == nil

	refs := make([]backend.ItemRef, len(req.ItemList.Items))
	for i, it := range req.ItemList.Items {
		// ItemPath obeys the hierarchical-parameter precedence (§3.1.1,
		// REQ-READ-001) for Write exactly as it does for Read and
		// Subscribe: a path given once on <ItemList> applies to every item
		// in it, and a per-item ItemPath overrides it. Reading only
		// it.ItemPath silently dropped the list-level value that
		// WriteItemList.Params had already decoded, sending the write to
		// the wrong (usually unknown) item. Write carries no
		// request-level Params element, so the list is the outermost
		// level here.
		p := xmlda.MergeItemParams(req.ItemList.Params, xmlda.ItemParams{ItemPath: it.ItemPath})
		ref := backend.ItemRef{ItemName: it.ItemName}
		if p.ItemPath != nil {
			ref.ItemPath = *p.ItemPath
		}
		refs[i] = ref
	}

	results := make([]backend.Result[backend.WriteOutcome], len(req.ItemList.Items))
	if readOnly {
		for i := range results {
			// A malformed item is reported as malformed even on a
			// read-only server: E_ACCESS_DENIED would tell the client its
			// item was fine and only the permission was missing, sending
			// it to fix the wrong thing.
			if err := req.ItemList.Items[i].DecodeErr; err != nil {
				results[i] = backend.Result[backend.WriteOutcome]{
					ResultID:       xmlda.ItemResultIDFor(err),
					DiagnosticInfo: xmlda.ItemDiagnosticFor(err),
				}
				continue
			}
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
			// An item this server could not interpret at all (a malformed
			// Timestamp, a <Value> whose content contradicts its declared
			// xsi:type) never reaches the backend: it resolves to its own
			// per-item ResultID, so one bad item costs the client that
			// item rather than the whole Write.
			if it.DecodeErr != nil {
				results[i] = backend.Result[backend.WriteOutcome]{
					ResultID:       xmlda.ItemResultIDFor(it.DecodeErr),
					DiagnosticInfo: xmlda.ItemDiagnosticFor(it.DecodeErr),
				}
				continue
			}
			if it.Value == nil {
				results[i] = backend.Result[backend.WriteOutcome]{ResultID: xmlda.ErrBadType}
				continue
			}
			wi := backend.WriteRequestItem{Ref: refs[i], Value: *it.Value, Timestamp: it.Timestamp}
			// A nil Quality means "no <Quality> element in the request";
			// a non-nil one means the element was present, even when it
			// carried no attributes and so specified the wire defaults
			// explicitly. That distinction is why ItemValue.Quality is a
			// pointer: the previous comparison against the zero
			// OPCQuality could not draw it (OPCQuality's fields are
			// themselves pointers, making the comparison pointer
			// identity), so an explicit <Quality/> was silently dropped
			// instead of being written. Quality and Timestamp are tracked
			// independently so a client writing only one of the two
			// doesn't spuriously synthesize the other.
			if it.Quality != nil {
				q := *it.Quality
				wi.Quality = &q
			}
			writeItems = append(writeItems, wi)
			origIdx = append(origIdx, i)
		}
		if len(writeItems) > 0 {
			backendResults, err := observeBackend(ctx, h.metrics, h.clk, "Write", h.cfg.BackendTimeout, func() ([]backend.Result[backend.WriteOutcome], error) {
				return h.backend.Writer.Write(ctx, writeItems)
			})
			if err != nil {
				h.metrics.IncRequestError("Write", "backend_error")
				writeFault(w, soapVersion(doc), backendErrorFault(err))
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
		Result:    h.replyBase(oc, req.Options.ClientRequestHandle, req.Options.LocaleID),
		RItemList: xmlda.ItemValueList{Items: items},
		Errors:    buildErrors(codes, h.errorTextFunc(req.Options, oc)),
	}
	writeResponse(w, h.log, soapVersion(doc), resp)
}
