// Package server provides the HTTP-facing composition root for an OPC
// XML-DA server built on this library: an http.Handler (Handler) plus an
// optional net/http.Server convenience wrapper (Server). This is the only
// package that imports both soap and the full xmlda operation-struct
// surface — see docs/architecture/package-structure.md.
package server

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock"
	"github.com/dernate/gopcxmlda-server/soap"
	"github.com/dernate/gopcxmlda-server/subscription"
	"github.com/dernate/gopcxmlda-server/telemetry"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// Handler is an http.Handler implementing the OPC XML-DA SOAP endpoint.
type Handler struct {
	cfg     Config
	backend backend.Backend
	clk     clock.Clock
	log     telemetry.Logger
	metrics telemetry.Metrics
	subs    *subscription.Manager
}

// New constructs a Handler. It validates deps.Backend (Status and Reader
// are required — backend.Backend.Validate) so a misconfigured backend
// fails fast at construction rather than behaving unpredictably at
// request time.
func New(deps Deps, cfg Config) (*Handler, error) {
	if err := deps.Backend.Validate(); err != nil {
		return nil, err
	}
	cfg = cfg.WithDefaults()

	clk := deps.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	log := deps.Logger
	if log == nil {
		log = telemetry.NoopLogger()
	}
	metrics := deps.Metrics
	if metrics == nil {
		metrics = telemetry.NoopMetrics()
	}

	subs := subscription.NewManager(deps.Backend, clk, log, metrics, cfg.subscriptionConfig())

	return &Handler{
		cfg:     cfg,
		backend: deps.Backend,
		clk:     clk,
		log:     log,
		metrics: metrics,
		subs:    subs,
	}, nil
}

// Shutdown cancels every subscription (unblocking in-flight
// SubscriptionPolledRefresh calls immediately) and waits for background
// goroutines to exit, bounded by ctx. Callers embedding Handler in their
// own http.Server must call this before http.Server.Shutdown — see
// docs/architecture/subscription-model.md.
func (h *Handler) Shutdown(ctx context.Context) error {
	return h.subs.Shutdown(ctx)
}

// BeginShutdown cancels every subscription without waiting for
// background goroutines to exit — see subscription.Manager.BeginShutdown.
func (h *Handler) BeginShutdown() {
	h.subs.BeginShutdown()
}

// ServeHTTP implements http.Handler.
//
// A panic anywhere below (most plausibly inside an application-supplied
// backend.Backend method) is recovered here rather than propagating to
// net/http's own per-connection recover, which would abort the
// connection with no SOAP Fault, no metrics, and no telemetry.Logger
// record — degrading every other in-flight request on that connection
// for a failure isolated to one call.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	opName := "unknown"
	defer func() {
		if rec := recover(); rec != nil {
			h.log.Error("panic recovered while handling request",
				"operation", opName, "panic", rec, "stack", string(debug.Stack()))
			h.metrics.IncRequestError(opName, "panic")
			writeFaultWithStatus(w, fault(xmlda.ErrFail, xmlda.StandardErrorText(xmlda.ErrFail)), http.StatusInternalServerError)
		}
	}()

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed: OPC XML-DA is POST-only", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.cfg.MaxRequestBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.metrics.IncParseError()
		var maxErr *http.MaxBytesError
		status := http.StatusBadRequest
		text := "unable to read request body"
		if errors.As(err, &maxErr) {
			status = http.StatusRequestEntityTooLarge
			text = "request body exceeds the configured maximum size"
		}
		writeFaultWithStatus(w, soapClientFault(text), status)
		return
	}

	op, ok, err := xmlda.IdentifyOperation(body)
	if err != nil {
		// Bucket 1: not well-formed XML/SOAP at all.
		h.metrics.IncParseError()
		writeFaultWithStatus(w, soapClientFault("malformed request: "+err.Error()), http.StatusBadRequest)
		return
	}
	if !ok {
		// Bucket 2: well-formed, but not one of the 8 known operations.
		h.metrics.IncRequestError("unknown", "unsupported_operation")
		writeFaultWithStatus(w, fault(xmlda.ErrNotSupported, xmlda.StandardErrorText(xmlda.ErrNotSupported)), http.StatusBadRequest)
		return
	}

	opName = op.Name.Local
	h.metrics.IncRequest(opName)

	timeout := h.cfg.RequestTimeout
	if opName == "SubscriptionPolledRefresh" {
		timeout = h.cfg.MaxPolledRefreshWait
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	status, err := h.backend.Status.GetStatus(ctx, "")
	if err != nil {
		h.metrics.IncRequestError(opName, "backend_error")
		writeFaultWithStatus(w, backendErrorFault(err), http.StatusInternalServerError)
		return
	}
	if needsFault, code := xmlda.RequiresFault(opName, status.State); needsFault {
		h.metrics.IncRequestError(opName, "server_state")
		writeFaultWithStatus(w, fault(code, xmlda.StandardErrorText(code)), http.StatusInternalServerError)
		return
	}

	switch opName {
	case "GetStatus":
		h.handleGetStatus(ctx, w, body, status)
	case "Read":
		h.handleRead(ctx, w, body, status.State)
	case "Write":
		h.handleWrite(ctx, w, body, status.State)
	case "Browse":
		h.handleBrowse(ctx, w, body, status.State)
	case "GetProperties":
		h.handleGetProperties(ctx, w, body, status.State)
	case "Subscribe":
		h.handleSubscribe(ctx, w, body, status.State)
	case "SubscriptionPolledRefresh":
		h.handlePolledRefresh(ctx, w, body, status.State)
	case "SubscriptionCancel":
		h.handleSubscriptionCancel(ctx, w, body)
	default:
		// Unreachable: op came from xmlda's own registry, which only
		// contains these 8 names.
		writeFaultWithStatus(w, fault(xmlda.ErrNotSupported, xmlda.StandardErrorText(xmlda.ErrNotSupported)), http.StatusBadRequest)
	}
}

// writeResponse encodes resp as a successful SOAP response body. It is a
// package-level generic function, not a method, because Go does not
// allow type parameters on methods.
//
// Encoding happens into a buffer first, not directly against w: once
// http.ResponseWriter.WriteHeader has been called, the status code and
// any bytes already written cannot be taken back, so an encode error
// partway through would otherwise reach the client as a truncated,
// invalid XML body with a misleading 200 status and no error signal at
// all. Buffering first means a genuine encode failure (e.g. a Value with
// no declared type, from a non-conforming backend result) can still fall
// back to a clean fault response instead.
func writeResponse[T any](w http.ResponseWriter, resp T) {
	env := soap.Envelope[T]{Body: soap.Body[T]{Content: &resp}}
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	if err := xml.NewEncoder(&buf).Encode(env); err != nil {
		writeFaultWithStatus(w, fault(xmlda.ErrFail, xmlda.StandardErrorText(xmlda.ErrFail)), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// writeFaultWithStatus encodes f as a SOAP Fault response body with the
// given HTTP status. Client-input-driven faults (malformed/unrecognized
// requests, configured-limit violations) use 400; server-condition
// faults (ServerState, backend errors, busy, timeout) use 500 — an
// implementation choice (the specification does not mandate HTTP status
// codes), applied consistently.
func writeFaultWithStatus(w http.ResponseWriter, f *soap.Fault, status int) {
	env := soap.Envelope[struct{}]{Body: soap.Body[struct{}]{Fault: f}}
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(env)
}

// checkItemCount rejects a request whose item count exceeds
// Config.MaxItemsPerRequest (0 = unlimited).
func (h *Handler) checkItemCount(n int) bool {
	return h.cfg.MaxItemsPerRequest <= 0 || n <= h.cfg.MaxItemsPerRequest
}

// checkSubscriptionItemCount rejects a Subscribe request whose item
// count exceeds Config.MaxItemsPerSubscription (0 = unlimited).
func (h *Handler) checkSubscriptionItemCount(n int) bool {
	return h.cfg.MaxItemsPerSubscription <= 0 || n <= h.cfg.MaxItemsPerSubscription
}

// deadlinePassed reports whether opts.RequestDeadline is set and has
// already elapsed as of now — REQ-TIME-002: a deadline already past at
// receipt is a whole-operation E_TIMEDOUT fault. (The spec's other
// RequestDeadline case — the deadline elapsing mid-processing, which
// then produces per-item E_TIMEDOUT results for whatever wasn't yet
// processed — is not separately implemented: this library's request
// handling is synchronous per call, making that window negligible in
// practice; documented in docs/limitations.md.) Read, Write, Subscribe,
// and SubscriptionPolledRefresh are the only operations whose request
// shape carries RequestDeadline at all; Browse and GetProperties's own
// E_TIMEDOUT fault (which they do list) arises from the general
// request-timeout-to-backend-error mapping instead (backendErrorFault).
func deadlinePassed(opts xmlda.RequestOptions, now time.Time) bool {
	return opts.RequestDeadline != nil && !opts.RequestDeadline.After(now)
}
