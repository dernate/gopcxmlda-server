package server

import (
	"context"
	"net/http"

	"github.com/dernate/gopcxmlda-server/soap"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

func (h *Handler) handleGetStatus(ctx context.Context, w http.ResponseWriter, doc *xmlda.Document, oc opContext) {
	var env soap.Envelope[xmlda.GetStatusRequest]
	if err := doc.Decode(&env); err != nil {
		h.metrics.IncRequestError("GetStatus", "parse")
		writeFault(w, requestDecodeFault("GetStatus", err))
		return
	}
	req := env.Body.Content

	// ServeHTTP fetched the status with locale "" — it had to, since the
	// request body (and with it the client's LocaleID) was not yet decoded
	// at that point. StatusInfo and VendorInfo are specified as
	// locale-specific, so a client that asked for a locale must have its
	// request actually reach the backend rather than be answered from the
	// locale-less fetch and then told, via RevisedLocaleID, that its
	// locale was honored. Only re-fetch when a locale was requested and
	// the backend claims to support it; otherwise the first fetch already
	// holds the right answer.
	status := oc.status
	revised := reviseLocale(req.LocaleID, status.SupportedLocaleIDs)
	if revised != "" && req.LocaleID != "" {
		localized, err := h.backend.Status.GetStatus(ctx, revised)
		if err != nil {
			h.metrics.IncRequestError("GetStatus", "backend_error")
			writeFault(w, backendErrorFault(err))
			return
		}
		status = localized
	}

	resp := xmlda.GetStatusResponse{
		Result: xmlda.ReplyBase{
			RcvTime:             oc.rcvTime,
			ReplyTime:           h.clk.Now(),
			ClientRequestHandle: req.ClientRequestHandle,
			RevisedLocaleID:     reviseLocale(req.LocaleID, status.SupportedLocaleIDs),
			// The freshly-fetched status is authoritative for the state
			// reported here, not the pre-decode one.
			ServerState: status.State,
		},
		Status: xmlda.Status{
			StartTime:                  status.StartTime,
			ProductVersion:             status.ProductVersion,
			StatusInfo:                 status.StatusInfo,
			VendorInfo:                 status.VendorInfo,
			SupportedLocaleIDs:         status.SupportedLocaleIDs,
			SupportedInterfaceVersions: []string{xmlda.InterfaceVersion10},
		},
	}
	writeResponse(w, resp)
}
