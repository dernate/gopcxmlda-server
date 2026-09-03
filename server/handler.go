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
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
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
	//
	// statusLock is a buffered channel of capacity 1 rather than a
	// sync.Mutex, because the lock is deliberately held across the backend
	// call and a caller must be able to give up on it. sync.Mutex has no
	// cancellable Lock, so waiting on one with a context meant parking a
	// goroutine per waiter — and against a backend that ignores ctx and
	// never returns, those goroutines never came back either: the request
	// itself timed out and freed its MaxConcurrentRequests slot, so the
	// leak scaled with the request RATE and nothing bounded it. A channel
	// send composes with ctx.Done() in one select and costs no goroutine.
	statusLock  chan struct{}
	statusVal   backend.ServerStatus
	statusFresh time.Time
	statusOK    bool
	// statusWarnOnce keeps a backend that reports an invalid ServerStatus
	// from logging the same complaint on every single request.
	statusWarnOnce sync.Once

	// cpKey authenticates Browse continuation points. Generated per
	// Handler and never persisted — see continuation.go.
	cpKey []byte

	// shuttingDown flips once BeginShutdown/Shutdown has been called, so a
	// readiness probe can fail before the server starts refusing work —
	// otherwise the load balancer keeps sending requests until the last
	// moment. See Stats/HealthHandler.
	shuttingDown atomic.Bool

	// reqSem bounds in-flight requests (Config.MaxConcurrentRequests);
	// nil when the limit is disabled.
	reqSem chan struct{}
	// pollSem is the sub-budget SubscriptionPolledRefresh draws from on
	// top of reqSem (Config.MaxConcurrentPolledRefresh); nil when that
	// limit is disabled. A long poll holds its slot for up to
	// MaxPolledRefreshWait, so without a class of its own it starves
	// every operation that answers in milliseconds.
	pollSem chan struct{}
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
	return acquire(h.reqSem)
}

// acquireOperationSlot takes the per-class slot an operation needs on top
// of its general request slot. Only SubscriptionPolledRefresh has one:
// it is the only operation that holds its slot for a duration the client
// chooses (up to Config.MaxPolledRefreshWait), so it is the only one that
// can starve the rest.
func (h *Handler) acquireOperationSlot(opName string) (release func(), ok bool) {
	if opName != "SubscriptionPolledRefresh" {
		return func() {}, true
	}
	return acquire(h.pollSem)
}

// acquire takes one slot from sem, or reports false if none is free. A nil
// sem means the limit is disabled.
func acquire(sem chan struct{}) (release func(), ok bool) {
	if sem == nil {
		return func() {}, true
	}
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
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
		st, err := observeBackend(ctx, h.metrics, h.clk, "GetStatus", h.cfg.BackendTimeout, func() (backend.ServerStatus, error) {
			return h.backend.Status.GetStatus(ctx, "")
		})
		if err != nil {
			return backend.ServerStatus{}, err
		}
		st = h.normalizeStatus(st)
		// A live read supersedes whatever the cache holds: a state change
		// observed here must not be masked by an older entry.
		h.storeStatus(ctx, st)
		return st, nil
	}
	// Acquiring the cache lock is itself cancellable, so a client that has
	// already hung up is never parked behind a backend it will not hear
	// from.
	select {
	case h.statusLock <- struct{}{}:
	case <-ctx.Done():
		return backend.ServerStatus{}, ctx.Err()
	}
	now := h.clk.Now()
	if h.statusOK && now.Sub(h.statusFresh) < h.cfg.StatusCacheTTL {
		st := h.statusVal
		<-h.statusLock
		return st, nil
	}

	// The lock is released by the FETCH, not by whoever is waiting for it.
	//
	// That distinction is the whole point of holding it across the backend
	// call: it collapses a burst into one fetch. Releasing it when a
	// waiter gives up instead let the next request start a second fetch
	// against a backend that had not answered the first — so against a
	// backend that ignores ctx and never returns, every request rate
	// bought another permanently-blocked goroutine. Holding it until the
	// call actually completes means at most ONE such goroutine exists,
	// however long the backend stays stuck and however many requests
	// arrive meanwhile; the rest give up cheaply below.
	type fetch struct {
		st  backend.ServerStatus
		err error
	}
	done := make(chan fetch, 1)
	go func() {
		// Detached from the request's deadline on purpose. This fetch
		// serves whoever asks next as much as the request that started
		// it, and — more importantly — the lock must stay held until the
		// CALL is finished, not until the first waiter's 50 ms elapses.
		// Bounding it by the request context instead released the lock
		// while the backend was still stuck, so the next request started
		// another fetch, and the count of permanently-blocked goroutines
		// grew with the request rate. BackendTimeout is what bounds it.
		fetchCtx := context.WithoutCancel(ctx)
		st, err := observeBackend(fetchCtx, h.metrics, h.clk, "GetStatus", h.cfg.BackendTimeout,
			func() (backend.ServerStatus, error) {
				return h.backend.Status.GetStatus(fetchCtx, "")
			})
		if err == nil {
			st = h.normalizeStatus(st)
			h.statusVal, h.statusFresh, h.statusOK = st, now, true
		}
		done <- fetch{st, err}
		<-h.statusLock
	}()

	select {
	case f := <-done:
		if f.err != nil {
			return backend.ServerStatus{}, f.err
		}
		return f.st, nil
	case <-ctx.Done():
		return backend.ServerStatus{}, ctx.Err()
	}
}

// storeStatus records st as the current cached status, unless ctx is done
// first — the same cancellable acquisition statusFor uses, so a client
// that has hung up never waits on a backend it will not hear from.
func (h *Handler) storeStatus(ctx context.Context, st backend.ServerStatus) {
	select {
	case h.statusLock <- struct{}{}:
	case <-ctx.Done():
		return
	}
	h.statusVal, h.statusFresh, h.statusOK = st, h.clk.Now(), true
	<-h.statusLock
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
	var pollSem chan struct{}
	if cfg.MaxConcurrentPolledRefresh > 0 {
		pollSem = make(chan struct{}, cfg.MaxConcurrentPolledRefresh)
	}

	subs := subscription.NewManager(deps.Backend, clk, log, metrics, cfg.subscriptionConfig())

	return &Handler{
		cfg:        cfg,
		statusLock: make(chan struct{}, 1),
		backend:    deps.Backend,
		clk:        clk,
		log:        log,
		metrics:    metrics,
		subs:       subs,
		cpKey:      cpKey,
		reqSem:     reqSem,
		pollSem:    pollSem,
	}, nil
}

// Shutdown cancels every subscription (unblocking in-flight
// SubscriptionPolledRefresh calls immediately) and waits for background
// goroutines to exit, bounded by ctx. Callers embedding Handler in their
// own http.Server must call this before http.Server.Shutdown — see
// docs/architecture/subscription-model.md.
func (h *Handler) Shutdown(ctx context.Context) error {
	h.shuttingDown.Store(true)
	return h.subs.Shutdown(ctx)
}

// BeginShutdown cancels every subscription without waiting for
// background goroutines to exit — see subscription.Manager.BeginShutdown.
func (h *Handler) BeginShutdown() {
	h.shuttingDown.Store(true)
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
	// Until the document has been parsed there is nothing to mirror, and
	// SOAP 1.1 is what OPC XML-DA 1.0 is defined over.
	version := soap.Version11
	// Measured for EVERY request, including the ones rejected before an
	// operation is even known — a latency series with the failures cut out
	// hides exactly the requests an operator is looking for.
	defer func() {
		h.metrics.ObserveRequestLatency(opName, h.clk.Now().Sub(rcvTime))
	}()
	defer func() {
		if rec := recover(); rec != nil {
			// A panic raised inside a backend call is re-raised here by
			// callBounded, carrying the stack it actually happened on;
			// debug.Stack() at this point would only show the re-raise.
			stack := debug.Stack()
			if bp, ok := rec.(backendPanic); ok {
				rec, stack = bp.value, bp.stack
			}
			h.log.Error("panic recovered while handling request",
				"operation", opName, "panic", rec, "stack", string(stack))
			h.metrics.IncRequestError(opName, "panic")
			writeFault(w, version, fault(xmlda.ErrFail, xmlda.StandardErrorText(xmlda.ErrFail)))
		}
	}()

	// Transport-level rejections below emit no SOAP envelope at all, so
	// they use the HTTP status code that actually describes them. Every
	// response that *does* carry a SOAP Fault goes through writeFault,
	// which is fixed at 500 per the SOAP 1.1 HTTP binding.
	if r.Method != http.MethodPost {
		// RFC 9110 §15.5.6 makes the Allow header mandatory on a 405.
		w.Header().Set("Allow", http.MethodPost)
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
		// opName is still "unknown" here by construction: admission
		// control runs before the body is read, which is the point.
		h.metrics.IncRequestError(opName, "busy")
		writeFault(w, version, fault(xmlda.ErrBusy, xmlda.StandardErrorText(xmlda.ErrBusy)))
		return
	}
	defer release()

	// A Content-Type check, deliberately narrow. OPC XML-DA is SOAP 1.1
	// over HTTP, whose binding names text/xml; real clients also send
	// application/soap+xml, and some send nothing at all — all three are
	// accepted, because rejecting a well-formed request over a header is
	// the kind of strictness that breaks interoperability for no protocol
	// benefit. What is refused is a type that positively claims to be
	// something else: without any check the endpoint accepts a
	// cross-origin form post, which is a "simple request" the browser
	// sends without a preflight.
	if ct := r.Header.Get("Content-Type"); ct != "" && !acceptableContentType(ct) {
		h.metrics.IncRequestError(opName, "unsupported_media_type")
		w.Header().Set("Accept", "text/xml, application/soap+xml")
		http.Error(w, "unsupported media type: expected text/xml or application/soap+xml",
			http.StatusUnsupportedMediaType)
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
	doc, err := xmlda.NewDocumentLimited(body, h.cfg.MaxElementDepth)
	if err != nil {
		// Bucket 1: not well-formed XML/SOAP at all.
		h.metrics.IncParseError()
		writeFault(w, version, soapClientFault("malformed request: "+err.Error()))
		return
	}
	// From here on, answer in the version the peer spoke: a SOAP 1.2
	// client handed a 1.1 envelope discards it, losing the very payload or
	// error code it was waiting for.
	version = soapVersion(doc)
	op, ok, err := doc.IdentifyOperation()
	if err != nil {
		h.metrics.IncParseError()
		writeFault(w, version, soapClientFault("malformed request: "+err.Error()))
		return
	}
	if !ok {
		// Bucket 2: well-formed, but not one of the 8 known operations.
		h.metrics.IncRequestError("unknown", "unsupported_operation")
		writeFault(w, version, fault(xmlda.ErrNotSupported, xmlda.StandardErrorText(xmlda.ErrNotSupported)))
		return
	}

	opName = op.Name.Local
	h.metrics.IncRequest(opName)
	h.checkSOAPAction(r, op)

	// The per-class slot can only be taken once the operation is known,
	// which is after the body has been decoded far enough to name it. That
	// is deliberate: the general slot above is what protects the decode
	// itself, and this one protects the long, client-controlled wait that
	// only SubscriptionPolledRefresh performs.
	releaseOp, ok := h.acquireOperationSlot(opName)
	if !ok {
		h.metrics.IncRequestError(opName, "busy")
		writeFault(w, version, fault(xmlda.ErrBusy, xmlda.StandardErrorText(xmlda.ErrBusy)))
		return
	}
	defer releaseOp()

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
		writeFault(w, version, backendErrorFault(err))
		return
	}
	if needsFault, code := h.requiresFault(opName, status.State); needsFault {
		h.metrics.IncRequestError(opName, "server_state")
		writeFault(w, version, fault(code, xmlda.StandardErrorText(code)))
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
		writeFault(w, version, fault(xmlda.ErrNotSupported, xmlda.StandardErrorText(xmlda.ErrNotSupported)))
	}
}

// acceptableContentType reports whether ct names a media type this
// server will read a SOAP envelope out of. Parameters (charset, action)
// are ignored; a missing type is handled by the caller.
func acceptableContentType(ct string) bool {
	mediaType := ct
	if i := strings.IndexByte(mediaType, ';'); i >= 0 {
		mediaType = mediaType[:i]
	}
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "text/xml", "application/soap+xml", "application/xml":
		return true
	default:
		return false
	}
}

// soapVersion reports the SOAP version a decoded request was written in,
// so the response can be written in the same one.
func soapVersion(doc *xmlda.Document) soap.Version {
	if doc == nil {
		return soap.Version11
	}
	return soap.VersionOf(doc.EnvelopeNamespace())
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
// It reports whether the response actually reached the client. An encode
// failure is also logged: without that, a whole operation's result
// collapses into an opaque E_FAIL with nothing anywhere naming the
// response or the reason — and the cause is almost always a backend
// returning a value this library cannot represent, which the operator has
// no other way to find. The return value lets a caller that committed
// state before writing (handleSubscribe, which by then holds a live
// subscription the client is about to be told about) roll that state back
// instead of leaving it stranded.
func writeResponse[T any](w http.ResponseWriter, log telemetry.Logger, version soap.Version, resp T) bool {
	// The three namespaces every response element references are declared
	// once, on the Envelope, and the payload is told so. Each element
	// declaring its own is correct XML and is what keeps a standalone
	// xmlda.Value self-contained — but inside a full response it produced
	// 6004 xmlns declarations for a 1000-item Read, 62 % of the bytes on
	// the wire, on every poll cycle.
	ns := xmlda.ResponseNamespaces()
	byPrefix := make(map[string]string, len(ns))
	for uri, prefix := range ns {
		byPrefix[prefix] = uri
	}
	env := soap.Envelope[T]{Version: version, Body: soap.Body[T]{Content: &resp}, ExtraNamespaces: byPrefix}
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	defer xmlda.DeclareAncestorNamespaces(enc, ns)()
	err := enc.Encode(env)
	if err == nil {
		err = enc.Close()
	}
	if err != nil {
		log.Error("encoding the response failed; replying with a fault instead",
			"responseType", fmt.Sprintf("%T", resp), "error", err.Error())
		writeFault(w, version, fault(xmlda.ErrFail, xmlda.StandardErrorText(xmlda.ErrFail)))
		return false
	}
	w.Header().Set("Content-Type", version.ContentType())
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
	return true
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
func writeFault(w http.ResponseWriter, version soap.Version, f *soap.Fault) {
	env := soap.Envelope[struct{}]{Version: version, Body: soap.Body[struct{}]{Fault: f}}
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
	w.Header().Set("Content-Type", version.ContentType())
	// SOAP 1.1's HTTP binding fixes a fault at 500 (§6.2). SOAP 1.2 §7
	// splits it: a Sender fault — the client's own malformed input — is a
	// 400, everything else a 500. Answering a 1.2 client with 500 for its
	// own mistake is not wrong enough to matter, but it is wrong.
	status := http.StatusInternalServerError
	if version == soap.Version12 && f != nil && isSenderFault(f.Code) {
		status = http.StatusBadRequest
	}
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// isSenderFault reports whether code names a fault SOAP 1.2 classifies as
// the sender's, which its HTTP binding answers with 400 rather than 500.
func isSenderFault(code soap.QName) bool {
	if code.Space != soap.NS11 && code.Space != soap.NS12 {
		return false
	}
	switch code.Local {
	case "Client", "Sender", "MustUnderstand", "VersionMismatch":
		return true
	default:
		return false
	}
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
