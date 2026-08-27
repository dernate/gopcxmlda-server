package server

import (
	"context"
	"net/http"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/soap"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

func (h *Handler) handleBrowse(ctx context.Context, w http.ResponseWriter, doc *xmlda.Document, oc opContext) {
	var env soap.Envelope[xmlda.BrowseRequest]
	if err := doc.Decode(&env); err != nil {
		h.metrics.IncRequestError("Browse", "parse")
		writeFault(w, requestDecodeFault("Browse", err))
		return
	}
	req := env.Body.Content

	if h.backend.Browser == nil {
		writeFault(w, fault(xmlda.ErrNotSupported, "Browse is not supported by this server"))
		return
	}

	backendCursor, ok := parseContinuationToken(req.ContinuationPoint, *req)
	if !ok {
		writeFault(w, fault(xmlda.ErrInvalidContinuationPoint, xmlda.StandardErrorText(xmlda.ErrInvalidContinuationPoint)))
		return
	}

	ref := backend.ItemRef{ItemName: req.ItemName}
	if req.ItemPath != nil {
		ref.ItemPath = *req.ItemPath
	}

	bres, err := h.backend.Browser.Browse(ctx, backend.BrowseRequest{
		Ref:                  ref,
		ContinuationPoint:    backendCursor,
		MaxElementsReturned:  int(req.MaxElementsReturned),
		Filter:               req.BrowseFilter,
		ElementNameFilter:    req.ElementNameFilter,
		VendorFilter:         req.VendorFilter,
		ReturnAllProperties:  req.ReturnAllProperties,
		ReturnPropertyValues: req.ReturnPropertyValues,
		PropertyNames:        req.PropertyNames,
	})
	if err != nil {
		h.metrics.IncRequestError("Browse", "backend_error")
		writeFault(w, backendErrorFault(err))
		return
	}

	includeValues := req.ReturnPropertyValues
	elements := make([]xmlda.BrowseElement, len(bres.Elements))
	for i, el := range bres.Elements {
		be := xmlda.BrowseElement{Name: el.Name, IsItem: el.IsItem, HasChildren: el.HasChildren}
		if el.Ref != nil {
			be.ItemName = el.Ref.ItemName
			path := el.Ref.ItemPath
			be.ItemPath = &path
		}
		if len(el.Properties) > 0 {
			be.Properties = make([]xmlda.ItemProperty, len(el.Properties))
			for j, p := range el.Properties {
				be.Properties[j] = toItemProperty(p, includeValues)
			}
		}
		elements[i] = be
	}

	resp := xmlda.BrowseResponse{
		MoreElements:      bres.MoreElements,
		ContinuationPoint: buildContinuationToken(*req, bres.ContinuationPoint),
		Result:            h.replyBase(oc, req.ClientRequestHandle, req.LocaleID),
		Elements:          elements,
	}
	writeResponse(w, resp)
}

// toItemProperty converts a backend.Property to the wire xmlda.ItemProperty
// shape, including its Value only when the request asked for property
// values.
func toItemProperty(p backend.Property, includeValues bool) xmlda.ItemProperty {
	name := xmlda.StandardPropertyName(p.ID)
	if name.IsZero() && p.Name != "" {
		// A vendor-defined property (no standard PropertyID) must never
		// be assigned the OPC XML-DA namespace it didn't ask for — that
		// would mislabel it as a standard property on the wire. p.Namespace
		// is empty unless the backend explicitly set it, in which case the
		// QName is left unqualified rather than defaulting to xmlda.Namespace.
		name = xmlda.QName{Space: p.Namespace, Local: p.Name}
	}
	ip := xmlda.ItemProperty{
		Name:        name,
		Description: p.Description,
		ResultID:    p.ResultID,
	}
	if p.Ref != nil {
		ip.ItemName = p.Ref.ItemName
		path := p.Ref.ItemPath
		ip.ItemPath = &path
	}
	if includeValues {
		v := p.Value
		ip.Value = &v
	}
	return ip
}
