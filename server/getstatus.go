package server

import (
	"context"
	"net/http"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/soap"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

func (h *Handler) handleGetStatus(ctx context.Context, w http.ResponseWriter, body []byte, status backend.ServerStatus) {
	var env soap.Envelope[xmlda.GetStatusRequest]
	if err := xmlda.Decode(body, &env); err != nil {
		h.metrics.IncRequestError("GetStatus", "parse")
		writeFaultWithStatus(w, requestDecodeFault("GetStatus", err), http.StatusBadRequest)
		return
	}
	req := env.Body.Content

	now := h.clk.Now()
	resp := xmlda.GetStatusResponse{
		Result: xmlda.ReplyBase{
			RcvTime:             now,
			ReplyTime:           now,
			ClientRequestHandle: req.ClientRequestHandle,
			RevisedLocaleID:     req.LocaleID,
			ServerState:         status.State,
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
