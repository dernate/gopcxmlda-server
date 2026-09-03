package server

import (
	"context"
	"net/http"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/soap"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

func (h *Handler) handleGetStatus(ctx context.Context, w http.ResponseWriter, doc *xmlda.Document, oc opContext) {
	var env soap.Envelope[xmlda.GetStatusRequest]
	if err := doc.Decode(&env); err != nil {
		h.metrics.IncRequestError("GetStatus", "parse")
		writeFault(w, soapVersion(doc), requestDecodeFault("GetStatus", err))
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
	// holds the right answer — and for this operation that first fetch is
	// always a live one, since ServeHTTP bypasses Config.StatusCacheTTL
	// for GetStatus specifically (see statusFor).
	status := oc.status
	revised := reviseLocale(req.LocaleID, status.SupportedLocaleIDs)
	if revised != "" && req.LocaleID != "" {
		localized, err := observeBackend(ctx, h.metrics, h.clk, "GetStatus", h.cfg.BackendTimeout, func() (backend.ServerStatus, error) {
			return h.backend.Status.GetStatus(ctx, revised)
		})
		if err != nil {
			h.metrics.IncRequestError("GetStatus", "backend_error")
			writeFault(w, soapVersion(doc), backendErrorFault(err))
			return
		}
		// normalizeStatus, not a bare assignment: this second fetch is a
		// separate backend call and can just as easily come back with an
		// empty State — and ServerState is use="required" in the schema,
		// so a bare assignment made every locale-carrying GetStatus reply
		// schema-invalid while the locale-less one stayed correct. The
		// normalization (and its once-only warning) belongs on every path
		// that takes a ServerStatus from the backend.
		status = h.normalizeStatus(localized)
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
	writeResponse(w, h.log, soapVersion(doc), resp)
}
