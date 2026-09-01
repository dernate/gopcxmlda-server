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
	"strings"
	"sync"
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

	// statusVal/statusFresh/statusOK memoize the pre-dispatch server
	// status for Config.StatusCacheTTL — see statusFor.
	statusMu    sync.Mutex
	statusVal   backend.ServerStatus
	statusFresh time.Time
	statusOK    bool
	// statusWarnOnce keeps a backend that reports an invalid ServerStatus
	// from logging the same complaint on every single request.
	statusWarnOnce sync.Once

	// cpKey authenticates Browse continuation points. Generated per
	// Handler and never persisted — see continuation.go.
	cpKey []byte

	// reqSem bounds in-flight requests (Config.MaxConcurrentRequests);
	// nil when the limit is disabled.
	reqSem chan struct{}
}

// normalizeStatus checks a backend-supplied ServerStatus for the
// invariants the wire format depends on, logs once if any is violated,
// and substitutes the one default that keeps the response schema-valid.
//
// backend.Backend.Validate only checks that the capabilities are non-nil;
// nothing validated what GetStatus actually returned. An empty State is
// the consequential one: ReplyBase omits the attribute when it is empty,
// and ServerState is use="required" in the schema — so a single forgotten
// field in a backend made EVERY response this server produced
// schema-invalid, with nothing anywhere reporting it. StartTime and
// SupportedLocaleIDs are reported but not substituted: no default this
// layer could invent would be true (REQ-STATUS-003 wants the real process
// start; REQ-STATUS-004 wants the locales the backend really supports).
func (h *Handler) normalizeStatus(st backend.ServerStatus) backend.ServerStatus {
	if problems := validateServerStatus(st); len(problems) > 0 {
		h.statusWarnOnce.Do(func() {
			h.log.Error("backend GetStatus returned an incomplete ServerStatus; "+
				"see backend.ServerStatus's field documentation",
				"problems", strings.Join(problems, "; "))
		})
	}
	if st.State == "" {
		st.State = xmlda.ServerStateRunning
	}
	return st
}

// validateServerStatus lists the ways st violates the invariants
// backend.ServerStatus documents, or nil if it holds none.
func validateServerStatus(st backend.ServerStatus) []string {
	var problems []string
	if st.State == "" {
		problems = append(problems, "State is empty (ServerState is required on every reply; assuming \"running\")")
	}
	if st.StartTime.IsZero() {
		problems = append(problems, "StartTime is the zero time (REQ-STATUS-003 requires the real process start time)")
	}
	if len(st.SupportedLocaleIDs) == 0 {
		problems = append(problems, "SupportedLocaleIDs is empty (REQ-STATUS-004 requires at least one entry)")
	}
	return problems
}

// requiresFault applies Config.RequiresFault, or xmlda.RequiresFault when
// the application supplied none.
func (h *Handler) requiresFault(op string, state xmlda.ServerState) (bool, xmlda.ErrorCode) {
	if h.cfg.RequiresFault != nil {
		return h.cfg.RequiresFault(op, state)
	}
	return xmlda.RequiresFault(op, state)
}

// acquireRequestSlot takes one of Config.MaxConcurrentRequests in-flight
// slots, reporting false if none is free. The returned release function is
// a no-op when the limit is disabled, so callers can defer it
// unconditionally.
func (h *Handler) acquireRequestSlot() (release func(), ok bool) {
	if h.reqSem == nil {
		return func() {}, true
	}
	select {
	case h.reqSem <- struct{}{}:
		return func() { <-h.reqSem }, true
	default:
		return func() {}, false
	}
}

// statusFor returns the server status ServeHTTP uses to evaluate
// xmlda.RequiresFault before dispatching opName (REQ-SERVER-002),
// reusing a recent fetch for up to Config.StatusCacheTTL.
//
// GetStatus is deliberately exempt: that operation *is* the status
// question, so it always gets a live read, which handleGetStatus then
// uses directly rather than re-fetching. Every other operation only
// needs State, which does not change on a millisecond scale — and
// without memoization each of them costs an extra backend GetStatus
// call, doubling the load on a backend that reaches a device to answer
// one.
//
// The lock is held across the backend call deliberately: that collapses
// a burst of concurrent requests into a single fetch, which is the point
// of the cache, rather than letting every miss start its own. A backend
// error is never cached — the next request retries.
func (h *Handler) statusFor(ctx context.Context, opName string) (backend.ServerStatus, error) {
	if h.cfg.StatusCacheTTL < 0 || opName == "GetStatus" {
		st, err := observeBackend(h.metrics, h.clk, "GetStatus", func() (backend.ServerStatus, error) {
			return h.backend.Status.GetStatus(ctx, "")
		})
		if err != nil {
			return backend.ServerStatus{}, err
		}
		st = h.normalizeStatus(st)
		// A live read supersedes whatever the cache holds: a state change
		// observed here must not be masked by an older entry.
		h.storeStatus(st)
		return st, nil
	}
	// Acquiring the cache lock is itself cancellable. Holding it across
	// the backend call is what collapses a burst into a single fetch, but
	// it also means a waiter can be parked here for as long as that call
	// takes — and a client that has already hung up should not be made to
	// wait for a backend it will never hear from.
	if err := lockContext(ctx, &h.statusMu); err != nil {
		return backend.ServerStatus{}, err
	}
	defer h.statusMu.Unlock()
	now := h.clk.Now()
	if h.statusOK && now.Sub(h.statusFresh) < h.cfg.StatusCacheTTL {
		return h.statusVal, nil
	}
	st, err := observeBackend(h.metrics, h.clk, "GetStatus", func() (backend.ServerStatus, error) {
		return h.backend.Status.GetStatus(ctx, "")
	})
	if err != nil {
		return backend.ServerStatus{}, err
	}
	st = h.normalizeStatus(st)
	h.statusVal, h.statusFresh, h.statusOK = st, now, true
	return st, nil
}

// lockContext acquires mu, or gives up if ctx is done first. sync.Mutex
// has no cancellable Lock, so the acquisition runs on its own goroutine;
// if ctx wins the race that goroutine still completes and immediately
// releases, leaving no leak and no lock held by a caller that has gone.
func lockContext(ctx context.Context, mu *sync.Mutex) error {
	// Fast path: an uncontended mutex — which, for the status cache, is
	// the overwhelmingly common case, since a cache HIT also comes through
	// here. Without it every request paid for a goroutine, a channel
	// allocation and a select just to discover the lock was free.
	if mu.TryLock() {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	acquired := make(chan struct{})
	go func() {
		mu.Lock()
		close(acquired)
	}()
	select {
	case <-acquired:
		return nil
	case <-ctx.Done():
		go func() {
			<-acquired
			mu.Unlock()
		}()
		return ctx.Err()
	}
}

// storeStatus records st as the current cached status.
func (h *Handler) storeStatus(st backend.ServerStatus) {
	h.statusMu.Lock()
	h.statusVal, h.statusFresh, h.statusOK = st, h.clk.Now(), true
	h.statusMu.Unlock()
}

// checkSOAPAction compares the request's SOAPAction header against the
// value conventional for the operation the body actually contains, and
// logs a mismatch at debug level.
//
// It never rejects. Dispatch is by body element name, which is the
// authoritative and more robust signal, and real clients are careless
// here: some omit the header, some send it unquoted, some send an empty
// string (which SOAP 1.1 explicitly permits as "no intent expressed").
// Faulting on any of that would break interoperability for no protocol
// benefit. But a header that names a *different* operation than the body
// is a genuine client bug worth being able to see, and until now
// xmlda.Operation.SOAPAction was computed for all eight operations and
// then never read by anything.
func (h *Handler) checkSOAPAction(r *http.Request, op xmlda.Operation) {
	raw := strings.TrimSpace(r.Header.Get("SOAPAction"))
	raw = strings.Trim(raw, `"`)
	if raw == "" || raw == op.SOAPAction {
		return
	}
	h.log.Debug("SOAPAction header does not match the operation in the request body",
		"operation", op.Name.Local, "header", raw, "expected", op.SOAPAction)
}

// errorText resolves the text for a result code in the response's Errors
// list, honoring Config.ErrorText when the embedding application supplied
// one (§2.6's locale-specific error text, and the only way a vendor code
// gets any text at all).
func (h *Handler) errorText(code xmlda.ErrorCode, locale string) string {
	if h.cfg.ErrorText != nil {
		return h.cfg.ErrorText(code, locale)
	}
	return xmlda.StandardErrorText(code)
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

	cpKey, err := newContinuationKey()
	if err != nil {
		return nil, err
	}

	var reqSem chan struct{}
	if cfg.MaxConcurrentRequests > 0 {
		reqSem = make(chan struct{}, cfg.MaxConcurrentRequests)
	}

	subs := subscription.NewManager(deps.Backend, clk, log, metrics, cfg.subscriptionConfig())

	return &Handler{
		cfg:     cfg,
		backend: deps.Backend,
		clk:     clk,
		log:     log,
		metrics: metrics,
		subs:    subs,
		cpKey:   cpKey,
		reqSem:  reqSem,
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

	// Admission control before the body is even read: a request this
	// server has no capacity for should not first be allowed to allocate
	// MaxRequestBodyBytes. E_BUSY is the specification's own code for
	// "already busy with something else"; a queued request would only
	// convert exhaustion into unbounded latency.
	release, ok := h.acquireRequestSlot()
	if !ok {
		h.metrics.IncRequestError(opName, "busy")
		writeFault(w, fault(xmlda.ErrBusy, xmlda.StandardErrorText(xmlda.ErrBusy)))
		return
	}
	defer release()

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
	h.checkSOAPAction(r, op)

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
	status, err := h.statusFor(ctx, opName)
	if err != nil {
		h.metrics.IncRequestError(opName, "backend_error")
		writeFault(w, backendErrorFault(err))
		return
	}
	if needsFault, code := h.requiresFault(opName, status.State); needsFault {
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
// back to a clean fault response instead. The encoder is closed as well
// as encoded to: Encode flushes on its own, but Close is what reports an
// element left unclosed, and that too must surface as a fault rather
// than as a truncated body.
func writeResponse[T any](w http.ResponseWriter, resp T) {
	env := soap.Envelope[T]{Body: soap.Body[T]{Content: &resp}}
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	if err := enc.Encode(env); err != nil {
		writeFault(w, fault(xmlda.ErrFail, xmlda.StandardErrorText(xmlda.ErrFail)))
		return
	}
	if err := enc.Close(); err != nil {
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
	enc := xml.NewEncoder(&buf)
	err := enc.Encode(env)
	if err == nil {
		err = enc.Close()
	}
	if err != nil {
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
// Config.MaxItemsPerRequest.
//
// The limit is unlimited only when negative. Zero does not mean unlimited
// here: Config.WithDefaults has already replaced an unset (zero) value
// with the built-in default by the time any request is served, so a zero
// reaching this function would be a deliberate one — and treating it as
// "no limit" would hand the most permissive setting to whoever wrote the
// least. The same convention governs MaxItemsPerSubscription,
// MaxConcurrentSubscriptions, MaxBrowseElements and
// MaxTotalSubscribedItems.
func (h *Handler) checkItemCount(n int) bool {
	return h.cfg.MaxItemsPerRequest <= 0 || n <= h.cfg.MaxItemsPerRequest
}

// checkSubscriptionItemCount rejects a Subscribe request whose item
// count exceeds Config.MaxItemsPerSubscription. Negative means unlimited;
// see checkItemCount on why zero does not.
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
