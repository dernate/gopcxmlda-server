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
		h.metrics.IncRequestError("Browse", "not_supported")
		writeFault(w, fault(xmlda.ErrNotSupported, "Browse is not supported by this server"))
		return
	}

	// BrowseFilter is an enumeration in the schema, and E_INVALIDFILTER is
	// the specification's code for a value outside it. Forwarding an
	// unrecognized filter to the backend instead left it to guess, with no
	// vocabulary to say the request made no sense. (An absent attribute is
	// not this case: xmlda.BrowseRequest.UnmarshalXML substitutes the
	// schema's own default of "all".)
	if !req.BrowseFilter.IsValid() {
		h.metrics.IncRequestError("Browse", "invalid_filter")
		writeFault(w, fault(xmlda.ErrInvalidFilter, xmlda.StandardErrorText(xmlda.ErrInvalidFilter)))
		return
	}

	backendCursor, ok := h.parseContinuationToken(req.ContinuationPoint, *req)
	if !ok {
		h.metrics.IncRequestError("Browse", "invalid_continuation_point")
		writeFault(w, fault(xmlda.ErrInvalidContinuationPoint, xmlda.StandardErrorText(xmlda.ErrInvalidContinuationPoint)))
		return
	}

	if !h.checkItemCount(len(req.PropertyNames)) {
		h.metrics.IncRequestError("Browse", "limit_exceeded")
		writeFault(w, limitExceededFault("too many property names in one Browse request"))
		return
	}

	ref := backend.ItemRef{ItemName: req.ItemName}
	if req.ItemPath != nil {
		ref.ItemPath = *req.ItemPath
	}

	// Browse is the one operation whose response size the client can
	// leave unbounded (MaxElementsReturned=0 means "no limit"), and the
	// whole response is assembled in memory before a byte goes out. Clamp
	// the client's request to the server's own ceiling before the backend
	// call, and enforce it again afterwards for a backend that ignores it.
	maxElements := h.cfg.MaxBrowseElements
	requested := int(req.MaxElementsReturned) // negative or 0 both mean "no client limit"
	if requested > 0 && (maxElements <= 0 || requested < maxElements) {
		maxElements = requested
	}

	breq := backend.BrowseRequest{
		Ref:                  ref,
		ContinuationPoint:    backendCursor,
		MaxElementsReturned:  maxElements,
		Filter:               req.BrowseFilter,
		ElementNameFilter:    req.ElementNameFilter,
		VendorFilter:         req.VendorFilter,
		ReturnAllProperties:  req.ReturnAllProperties,
		ReturnPropertyValues: req.ReturnPropertyValues,
		PropertyNames:        req.PropertyNames,
	}
	bres, err := observeBackend(h.metrics, h.clk, "Browse", func() (backend.BrowseResult, error) {
		return h.backend.Browser.Browse(ctx, breq)
	})
	if err != nil {
		h.metrics.IncRequestError("Browse", "backend_error")
		writeFault(w, backendErrorFault(err))
		return
	}

	// A backend that returned more than it was asked for is truncated
	// here rather than trusted: MoreElements then tells the client the
	// result set is incomplete, which is exactly what it is. Without this
	// the ceiling above would be advisory only.
	moreElements := bres.MoreElements
	if maxElements > 0 && len(bres.Elements) > maxElements {
		h.log.Warn("backend returned more Browse elements than requested; truncating",
			"requested", maxElements, "returned", len(bres.Elements))
		bres.Elements = bres.Elements[:maxElements]
		moreElements = true
	}

	includeValues := req.ReturnPropertyValues
	elements := make([]xmlda.BrowseElement, len(bres.Elements))
	// Every per-property condition a returned element carries needs an
	// Errors entry, exactly as Read/Write/GetProperties produce for their
	// own items: §3.8.2 gives BrowseResponse an Errors list, and this
	// handler was the only one of the six that never filled it — so a
	// property that failed to read (E_INVALIDPID, E_ACCESS_DENIED) arrived
	// with a ResultID and no text, in a response a client reads as
	// error-free.
	var codes []xmlda.ErrorCode
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
				be.Properties[j] = h.toItemProperty(p, includeValues)
				codes = append(codes, p.ResultID)
			}
		}
		elements[i] = be
	}

	opts := xmlda.RequestOptions{
		ClientRequestHandle: req.ClientRequestHandle,
		LocaleID:            req.LocaleID,
		ReturnErrorText:     req.ReturnErrorText,
	}
	resp := xmlda.BrowseResponse{
		MoreElements:      moreElements,
		ContinuationPoint: h.buildContinuationToken(*req, bres.ContinuationPoint),
		Result:            h.replyBase(oc, req.ClientRequestHandle, req.LocaleID),
		Elements:          elements,
		Errors:            xmlda.DedupeErrors(codes, h.errorTextFunc(opts, oc)),
	}
	writeResponse(w, resp)
}

// toItemProperty converts a backend.Property to the wire xmlda.ItemProperty
// shape, including its Value only when the request asked for property
// values and the backend actually supplied one.
func (h *Handler) toItemProperty(p backend.Property, includeValues bool) xmlda.ItemProperty {
	name := xmlda.StandardPropertyName(p.ID)
	if name.IsZero() && p.Name != "" {
		// A vendor-defined property (no standard PropertyID) must never
		// be assigned the OPC XML-DA namespace it didn't ask for — that
		// would mislabel it as a standard property on the wire. p.Namespace
		// is empty unless the backend explicitly set it, in which case the
		// QName is left unqualified rather than defaulting to xmlda.Namespace.
		name = xmlda.QName{Space: p.Namespace, Local: p.Name}
		if p.Namespace == "" {
			// §3.1.10 requires vendor properties to be "qualified with a
			// vendor-specific namespace". An unqualified one is encoded
			// with a local xmlns="" reset so it does not become an OPC
			// name — which is correct for the QName but pulls that one
			// element out of the response's default namespace. Worth
			// telling the operator about rather than silently shipping a
			// schema-invalid element.
			h.log.Warn("backend returned a vendor item property with no namespace; "+
				"set backend.Property.Namespace to a vendor URI (see docs/specification/error-mapping.md)",
				"property", p.Name)
		}
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
	// IsValid, not just includeValues: a property the backend could not
	// read (ResultID set, e.g. E_INVALIDPID) leaves Value at its zero,
	// and a zero Value has no declared type. Handing one to the encoder
	// fails the whole document, which writeResponse can only report as a
	// blanket E_FAIL fault — discarding every other item's data over one
	// missing property value.
	if includeValues && p.Value.IsValid() {
		v := p.Value
		ip.Value = &v
	}
	return ip
}
