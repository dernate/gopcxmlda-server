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
	// Captured before anything else, so ReplyBase.RcvTime is genuinely the
	// receipt time rather than "whenever this handler last looked at the
	// clock" (see opContext).
	rcvTime := h.clk.Now()

	opName := "unknown"
	defer func() {
		if rec := recover(); rec != nil {
			h.log.Error("panic recovered while handling request",
				"operation", opName, "panic", rec, "stack", string(debug.Stack()))
			h.metrics.IncRequestError(opName, "panic")
			writeFault(w, fault(xmlda.ErrFail, xmlda.StandardErrorText(xmlda.ErrFail)))
		}
	}()

	// Transport-level rejections below emit no SOAP envelope at all, so
	// they use the HTTP status code that actually describes them. Every
	// response that *does* carry a SOAP Fault goes through writeFault,
	// which is fixed at 500 per the SOAP 1.1 HTTP binding.
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed: OPC XML-DA is POST-only", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.cfg.MaxRequestBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.metrics.IncParseError()
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body exceeds the configured maximum size", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "unable to read request body", http.StatusBadRequest)
		return
	}

	// One Document for the whole request: its namespace-prefix table is
	// built once here and reused by the typed decode in each handler,
	// instead of being rebuilt from scratch per decode.
	doc, err := xmlda.NewDocument(body)
	if err != nil {
		// Bucket 1: not well-formed XML/SOAP at all.
		h.metrics.IncParseError()
		writeFault(w, soapClientFault("malformed request: "+err.Error()))
		return
	}
	op, ok, err := doc.IdentifyOperation()
	if err != nil {
		h.metrics.IncParseError()
		writeFault(w, soapClientFault("malformed request: "+err.Error()))
		return
	}
	if !ok {
		// Bucket 2: well-formed, but not one of the 8 known operations.
		h.metrics.IncRequestError("unknown", "unsupported_operation")
		writeFault(w, fault(xmlda.ErrNotSupported, xmlda.StandardErrorText(xmlda.ErrNotSupported)))
		return
	}

	opName = op.Name.Local
	h.metrics.IncRequest(opName)

	timeout := h.cfg.RequestTimeout
	if opName == "SubscriptionPolledRefresh" {
		// The operation's own Hold+Wait budget is capped at
		// MaxPolledRefreshWait (see handlePolledRefresh). This context
		// deadline is deliberately that budget *plus* headroom: were the
		// two equal, a client legitimately requesting the full budget
		// would race the context, and losing that race turns a complete,
		// data-bearing response into an E_TIMEDOUT fault that discards it.
		// The Hold+Wait cap is the authority; this is only a backstop
		// against a handler that somehow never returns.
		timeout = h.cfg.MaxPolledRefreshWait + polledRefreshGrace
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	// Locale "" here: this call exists only to obtain State for
	// RequiresFault, before the request body has been decoded and the
	// client's requested LocaleID is even known (REQ-SERVER-002).
	// GetStatus re-fetches with the requested locale when one was asked
	// for — see handleGetStatus.
	status, err := h.backend.Status.GetStatus(ctx, "")
	if err != nil {
		h.metrics.IncRequestError(opName, "backend_error")
		writeFault(w, backendErrorFault(err))
		return
	}
	if needsFault, code := xmlda.RequiresFault(opName, status.State); needsFault {
		h.metrics.IncRequestError(opName, "server_state")
		writeFault(w, fault(code, xmlda.StandardErrorText(code)))
		return
	}

	oc := opContext{rcvTime: rcvTime, status: status}

	switch opName {
	case "GetStatus":
		h.handleGetStatus(ctx, w, doc, oc)
	case "Read":
		h.handleRead(ctx, w, doc, oc)
	case "Write":
		h.handleWrite(ctx, w, doc, oc)
	case "Browse":
		h.handleBrowse(ctx, w, doc, oc)
	case "GetProperties":
		h.handleGetProperties(ctx, w, doc, oc)
	case "Subscribe":
		h.handleSubscribe(ctx, w, doc, oc)
	case "SubscriptionPolledRefresh":
		h.handlePolledRefresh(ctx, w, doc, oc)
	case "SubscriptionCancel":
		h.handleSubscriptionCancel(ctx, w, doc)
	default:
		// Unreachable: op came from xmlda's own registry, which only
		// contains these 8 names.
		writeFault(w, fault(xmlda.ErrNotSupported, xmlda.StandardErrorText(xmlda.ErrNotSupported)))
	}
}

// polledRefreshGrace is the headroom added to a SubscriptionPolledRefresh
// request's context deadline beyond Config.MaxPolledRefreshWait, so
// assembling and encoding the response cannot be cut short by the very
// deadline the Hold+Wait budget was sized against.
const polledRefreshGrace = 5 * time.Second

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
		writeFault(w, fault(xmlda.ErrFail, xmlda.StandardErrorText(xmlda.ErrFail)))
		return
	}
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// writeFault encodes f as a SOAP Fault response body.
//
// The status is always 500, as the SOAP 1.1 HTTP binding requires (§6.2:
// a response carrying a SOAP Fault "MUST" use 500) — and this library
// always emits the SOAP 1.1 shape (ADR-004). Distinguishing
// client-input faults with a 400 reads as more informative, but it makes
// the response non-conformant, and a strict SOAP 1.1 client is entitled
// to treat a 4xx as a transport failure and never parse the Fault body
// at all — losing the very error code the fault existed to convey.
// Failures that emit no SOAP envelope (wrong HTTP method, oversized body)
// are not SOAP faults and keep their own status codes; see ServeHTTP.
//
// Encoding goes into a buffer first for the same reason writeResponse
// does it: WriteHeader cannot be taken back, so an encode failure partway
// through must not reach the client as a truncated fault body.
func writeFault(w http.ResponseWriter, f *soap.Fault) {
	env := soap.Envelope[struct{}]{Body: soap.Body[struct{}]{Fault: f}}
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	if err := xml.NewEncoder(&buf).Encode(env); err != nil {
		// A Fault that cannot be encoded leaves nothing meaningful to
		// send; report the status alone rather than a malformed body.
		http.Error(w, "internal error encoding SOAP fault", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write(buf.Bytes())
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
