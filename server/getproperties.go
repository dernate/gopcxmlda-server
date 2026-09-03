package server

import (
	"context"
	"net/http"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/soap"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

func (h *Handler) handleGetProperties(ctx context.Context, w http.ResponseWriter, doc *xmlda.Document, oc opContext) {
	var env soap.Envelope[xmlda.GetPropertiesRequest]
	if err := doc.Decode(&env); err != nil {
		h.metrics.IncRequestError("GetProperties", "parse")
		writeFault(w, soapVersion(doc), requestDecodeFault("GetProperties", err))
		return
	}
	req := env.Body.Content

	if h.backend.Properties == nil {
		h.metrics.IncRequestError("GetProperties", "not_supported")
		writeFault(w, soapVersion(doc), fault(xmlda.ErrNotSupported, "GetProperties is not supported by this server"))
		return
	}
	// PropertyNames is bounded like every other client-supplied list —
	// Browse already did this for the same field. Both lists multiply:
	// the response carries one ItemProperty per item AND per name, and
	// the whole document is assembled in memory before a byte goes out,
	// so two individually-legal lists can still ask for a response
	// neither limit describes. A 215 KB request produced 739 MB.
	if !h.checkItemCount(len(req.PropertyNames)) {
		h.metrics.IncRequestError("GetProperties", "limit_exceeded")
		writeFault(w, soapVersion(doc), limitExceededFault("too many property names in one GetProperties request"))
		return
	}
	if n, m := len(req.ItemIDs), len(req.PropertyNames); h.cfg.MaxItemsPerRequest > 0 && m > 0 &&
		n > h.cfg.MaxItemsPerRequest/m {
		h.metrics.IncRequestError("GetProperties", "limit_exceeded")
		writeFault(w, soapVersion(doc), limitExceededFault("too many item/property combinations in one GetProperties request"))
		return
	}
	if !h.checkItemCount(len(req.ItemIDs)) {
		h.metrics.IncRequestError("GetProperties", "limit_exceeded")
		writeFault(w, soapVersion(doc), limitExceededFault("too many items in one GetProperties request"))
		return
	}

	// PropertyNames identifies properties by QName; translate the
	// standard-namespace ones back to PropertyID for the backend contract.
	// Resolved once for the whole request, not once per item: the list is
	// request-level, so re-deriving it inside the item loop produced the
	// identical slice len(ItemIDs) times.
	//
	// PropertyNames is ignored entirely when ReturnAllProperties is set
	// (REQ-PROPERTIES-001), so unresolvable names are only a condition
	// worth reporting when it isn't.
	var ids []xmlda.PropertyID
	var unknownNames []xmlda.QName
	if !req.ReturnAllProperties {
		for _, pn := range req.PropertyNames {
			if id, ok := standardPropertyIDFor(pn); ok {
				ids = append(ids, id)
				continue
			}
			// A name this server cannot resolve to a property is
			// E_INVALIDPID, reported per-property in the reply. Dropping
			// it silently left the client unable to tell "that property
			// does not exist here" from "that property exists and has no
			// value" — the property simply vanished from the response.
			unknownNames = append(unknownNames, pn)
		}
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
		reqs[i] = backend.PropertyRequest{
			Ref:           ref,
			All:           req.ReturnAllProperties,
			PropertyIDs:   ids,
			IncludeValues: req.ReturnPropertyValues,
		}
	}

	results, err := observeBackend(ctx, h.metrics, h.clk, "GetProperties", h.cfg.BackendTimeout, func() ([]backend.Result[[]backend.Property], error) {
		return h.backend.Properties.GetProperties(ctx, reqs)
	})
	if err != nil {
		h.metrics.IncRequestError("GetProperties", "backend_error")
		writeFault(w, soapVersion(doc), backendErrorFault(err))
		return
	}

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
		// refs[i] already carries the effective path — the item's own
		// ItemPath if it had one, the request-level default otherwise
		// (§3.1.1's hierarchical parameters). Echoing only id.ItemPath
		// dropped the request-level value, so a client that set the path
		// once for the whole request got its items back unqualified and
		// could not match them against what it asked for.
		list := xmlda.PropertyReplyList{ItemName: id.ItemName, ResultID: res.ResultID}
		if id.ItemPath != nil || req.ItemPath != nil {
			path := refs[i].ItemPath
			list.ItemPath = &path
		}
		for _, p := range res.Value {
			list.Properties = append(list.Properties, h.toItemProperty(p, req.ReturnPropertyValues))
		}
		// Only for an item the backend could resolve at all: an item that
		// itself failed (e.g. E_UNKNOWNITEMNAME) reports no properties,
		// and appending per-property conditions to it would contradict
		// PropertyReader's documented per-ITEM/per-PROPERTY split.
		if res.ResultID.IsZero() {
			for _, pn := range unknownNames {
				list.Properties = append(list.Properties, xmlda.ItemProperty{
					Name:     pn,
					ResultID: xmlda.ErrInvalidPID,
				})
				codes = append(codes, xmlda.ErrInvalidPID)
			}
		}
		lists[i] = list
		codes = append(codes, res.ResultID)
	}

	opts := xmlda.RequestOptions{
		ClientRequestHandle: req.ClientRequestHandle,
		LocaleID:            req.LocaleID,
		// Resolved through the request's OWN accessor, then passed on as an
		// explicit value: Browse and GetProperties declare
		// ReturnErrorText with default="false" while RequestOptions
		// declares default="true", so handing the raw pointer to a
		// RequestOptions would silently apply the wrong default whenever
		// the client omits the attribute.
		ReturnErrorText: boolPtr(req.ReturnErrorTextOrDefault()),
	}
	resp := xmlda.GetPropertiesResponse{
		Result:        h.replyBase(oc, req.ClientRequestHandle, req.LocaleID),
		PropertyLists: lists,
		Errors:        buildErrors(codes, h.errorTextFunc(opts, oc)),
	}
	writeResponse(w, h.log, soapVersion(doc), resp)
}

// standardPropertyIDs are the standard property IDs this server can
// resolve a requested QName back to: the specification's 1-8 and 100-108
// (§4.2). Built into a reverse lookup table once at init rather than
// re-scanned linearly per requested name.
var standardPropertyIDs = []xmlda.PropertyID{
	xmlda.PropDataType, xmlda.PropValue, xmlda.PropQuality, xmlda.PropTimestamp,
	xmlda.PropAccessRights, xmlda.PropScanRate, xmlda.PropEUType, xmlda.PropEUInfo,
	xmlda.PropEngineeringUnits, xmlda.PropDescription, xmlda.PropHighEU, xmlda.PropLowEU,
	xmlda.PropHighIR, xmlda.PropLowIR, xmlda.PropCloseLabel, xmlda.PropOpenLabel, xmlda.PropTimeZone,
}

var standardPropertyIDByName = func() map[xmlda.QName]xmlda.PropertyID {
	m := make(map[xmlda.QName]xmlda.PropertyID, len(standardPropertyIDs))
	for _, id := range standardPropertyIDs {
		m[xmlda.StandardPropertyName(id)] = id
	}
	return m
}()

func standardPropertyIDFor(qn xmlda.QName) (xmlda.PropertyID, bool) {
	id, ok := standardPropertyIDByName[qn]
	return id, ok
}
