package server

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock"
	"github.com/dernate/gopcxmlda-server/telemetry"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// hasUsableValue reports whether an item carrying result code c may
// still carry a value on the wire. True for the zero code (no abnormal
// condition) and for every success-with-caveat "S_"-prefixed code, false
// only for critical "E_"-prefixed errors — §2.6's own distinction.
//
// Used everywhere an item's outcome is turned into a wire ItemValue, so
// Read, Write, Subscribe and SubscriptionPolledRefresh agree on it.
func hasUsableValue(c xmlda.ErrorCode) bool {
	return c.IsZero() || c.IsSuccess()
}

// buildItemValue constructs the wire ItemValue for one item's outcome,
// applying the quality-driven Value/Timestamp presence rule
// (xmlda.ResolveValuePresence) and the RequestOptions gating for
// ItemName/ItemPath/Timestamp/DiagnosticInfo — the single place this
// logic lives so every operation's response construction applies it
// identically (docs/architecture/data-flow.md). haveSample is false when
// the backend produced no sample at all for this item (e.g. the item is
// write-only, or the whole item was invalid).
func buildItemValue(ref backend.ItemRef, clientItemHandle string, sample backend.ItemSample, haveSample bool, resultID xmlda.ErrorCode, diagnosticInfo string, opts xmlda.RequestOptions) xmlda.ItemValue {
	iv := xmlda.ItemValue{
		ClientItemHandle: clientItemHandle,
		ResultID:         resultID,
	}
	if opts.ReturnItemNameOrDefault() {
		iv.ItemName = ref.ItemName
	}
	if opts.ReturnItemPathOrDefault() {
		path := ref.ItemPath
		iv.ItemPath = &path
	}
	if opts.ReturnDiagnosticInfoOrDefault() {
		// Always set, even when the backend supplied nothing: §3.1.6 makes
		// this a requirement — "specific diagnostic information OR A BLANK
		// STRING if diagnostic information is not available" — so a client
		// that asked can tell "nothing to report" from "the server ignored
		// my option".
		text := diagnosticInfo
		iv.DiagnosticInfo = &text
	}
	if !haveSample {
		// An explicit Bad quality, not a nil one and not the zero
		// OPCQuality. <Quality> is minOccurs="0" in the schema and an
		// attribute-less <Quality/> reads as QualityField="good" under the
		// schema's own defaults, so emitting either of those for an item
		// that has no sample would report e.g. an unknown item name as
		// good-quality-with-no-value — a direct contradiction of the
		// ResultID on the same element. Omitting the element entirely (the
		// previous behavior) has the same failure mode one step removed: a
		// client mapping this onto OPC DA's wQuality applies the schema
		// default to the missing element and lands on "good" again. The
		// specification's own normative example resolves it by stating the
		// quality outright — §2.6 p.22 shows
		// <Items ResultID="E_UNKNOWNITEMNAME"><Quality QualityField="bad"/></Items>.
		bad := xmlda.NewQuality(xmlda.QualityBad, xmlda.LimitNone, 0)
		iv.Quality = &bad
		return iv
	}
	q := sample.Quality
	iv.Quality = &q
	// IsValid, not just IsNil: a Value that was never constructed (a
	// backend leaving ItemSample.Value at its Go zero rather than using
	// xmlda.NewNil) carries no declared type at all, and handing one to the
	// encoder fails the WHOLE response — which writeResponse can only
	// report as a blanket E_FAIL fault, discarding every other item's data
	// over one malformed one. server/browse.go's toItemProperty already
	// gates property values this way; item values were the gap.
	haveLastKnown := sample.Value.IsValid() && !sample.Value.IsNil()
	if sample.Value.IsValid() && xmlda.ResolveValuePresence(sample.Quality, haveLastKnown) {
		v := sample.Value
		iv.Value = &v
		if opts.ReturnItemTimeOrDefault() {
			ts := sample.Timestamp
			iv.Timestamp = &ts
		}
	}
	return iv
}

// buildItemDecodeFailure builds the wire ItemValue for a request item this
// server could not interpret: no value, no quality, the per-item ResultID
// the condition maps to, and — when the client asked for diagnostics —
// which of the item's own fields was unreadable.
//
// It exists so Read, Write and Subscribe all report a malformed item the
// same way, and so that one malformed item costs the client that item
// rather than the whole request (xmlda.ItemDecodeError).
func buildItemDecodeFailure(ref backend.ItemRef, clientItemHandle string, decodeErr error, opts xmlda.RequestOptions) (xmlda.ItemValue, xmlda.ErrorCode) {
	code := xmlda.ItemResultIDFor(decodeErr)
	iv := buildItemValue(ref, clientItemHandle, backend.ItemSample{}, false, code,
		xmlda.ItemDiagnosticFor(decodeErr), opts)
	return iv, code
}

// errorTextFunc returns the textOf function buildErrors should use, or
// nil when RequestOptions.ReturnErrorText is false and no Errors list
// should be produced at all.
//
// When text is wanted it goes through Handler.errorText, so an
// application-supplied Config.ErrorText sees both the code and the locale
// the server actually resolved for this request — the same locale it
// reports in ReplyBase.RevisedLocaleID. Answering "de-DE" there and then
// sending English text is the contradiction §2.6 asks servers to avoid.
func (h *Handler) errorTextFunc(opts xmlda.RequestOptions, oc opContext) func(xmlda.ErrorCode) string {
	if !opts.ReturnErrorTextOrDefault() {
		// nil, not a function returning "": §3.1.9 states "For each
		// OPCError there will be a Text element", so an OPCError without
		// one is not a lesser OPCError — it is one the server should not
		// have sent. A client that switched the text off keeps every code
		// it needs in the per-item ResultIDs, which is the actual
		// per-item result mechanism; the Errors list exists to carry the
		// text. buildErrors turns a nil textOf into an empty list.
		return nil
	}
	locale := reviseLocale(opts.LocaleID, oc.status.SupportedLocaleIDs)
	return func(c xmlda.ErrorCode) string { return h.errorText(c, locale) }
}

// boolPtr returns a pointer to b, for the request-option fields whose
// pointer-ness distinguishes "the client said so" from "apply the
// default" — and where the applicable default differs by element.
func boolPtr(b bool) *bool { return &b }

// buildErrors assembles the response's deduplicated Errors list, or an
// empty one when the client switched the error text off — see
// errorTextFunc for why "no text" means "no entry" rather than "an entry
// without text".
func buildErrors(codes []xmlda.ErrorCode, textOf func(xmlda.ErrorCode) string) xmlda.Errors {
	if textOf == nil {
		return nil
	}
	return xmlda.DedupeErrors(codes, textOf)
}

// msToDuration converts a millisecond count from the wire to a Duration,
// mapping any non-positive value to zero.
//
// The fields it is used for (WaitTime, SubscriptionPingRate) are xsd:int
// on the wire, so a client may legitimately send a negative number — the
// schema permits it and this library now decodes it rather than faulting.
// None of them has a meaning below zero, and every one already treats 0
// as "unset / use the server's own policy", so folding negatives into
// zero is the reading that keeps a nonsensical value from becoming a
// negative timeout.
func msToDuration(ms int32) time.Duration {
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

// observeBackend times one backend call and reports it via
// telemetry.Metrics.ObserveBackendLatency, then returns whatever the call
// returned.
//
// The metric hook existed from the start and was never called, so every
// Metrics implementation had to stub a method that produced nothing. It
// is the one measurement that separates "this server is slow" from "the
// data source behind it is slow", which is usually the first question
// asked when a client complains, so wiring it is worth more than deleting
// it.
func observeBackend[T any](ctx context.Context, m telemetry.Metrics, clk clock.Clock, operation string, timeout time.Duration, call func() (T, error)) (T, error) {
	start := clk.Now()
	v, err := callBounded(ctx, timeout, call)
	m.ObserveBackendLatency(operation, clk.Now().Sub(start))
	return v, err
}

// backendPanic carries a panic recovered on a backend call's own
// goroutine back to the handler goroutine, together with the stack it was
// raised on — the caller re-panics with it so ServeHTTP's recover handles
// it as it always did, and the log names the backend's frames rather than
// the re-raise point.
type backendPanic struct {
	value any
	stack []byte
}

func (p backendPanic) String() string { return fmt.Sprint(p.value) }

// callBounded runs call and stops waiting for it once ctx is done or
// timeout elapses, whichever comes first. A timeout of zero or less waits
// indefinitely (the previous, purely cooperative behavior).
//
// This exists because a context deadline is a request to stop, not a
// mechanism that stops anything. A backend that reaches a device through
// a blocking library call and never consults ctx held its handler
// goroutine — and with it one of Config.MaxConcurrentRequests slots —
// for as long as the device stayed unresponsive; the client got no answer
// at all, and after enough such calls every other client got E_BUSY
// permanently. Running the call on its own goroutine and giving up on it
// does not cancel it (Go cannot), but it does hand the request back, so
// the server stays answerable while the device is not.
//
// The abandoned goroutine writes into a buffered channel and then exits,
// so nothing is retained beyond the call itself.
func callBounded[T any](ctx context.Context, timeout time.Duration, call func() (T, error)) (T, error) {
	if timeout <= 0 {
		return call()
	}
	type result struct {
		v          T
		err        error
		panicked   bool
		panicVal   any
		panicTrace []byte
	}
	done := make(chan result, 1)
	go func() {
		// The call no longer runs on the handler's own goroutine, so
		// net/http's recover — and ServeHTTP's — cannot see a panicking
		// backend any more. Catch it here and re-raise it on the caller's
		// goroutine, so the behavior a panicking backend produces is
		// exactly what it was: a recovered panic, logged with its stack,
		// counted, and answered with a fault.
		var r result
		defer func() {
			if rec := recover(); rec != nil {
				r = result{panicked: true, panicVal: rec, panicTrace: debug.Stack()}
			}
			done <- r
		}()
		r.v, r.err = call()
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		if r.panicked {
			panic(backendPanic{value: r.panicVal, stack: r.panicTrace})
		}
		return r.v, r.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	case <-timer.C:
		var zero T
		return zero, fmt.Errorf("%w: the backend did not return within %s", context.DeadlineExceeded, timeout)
	}
}
