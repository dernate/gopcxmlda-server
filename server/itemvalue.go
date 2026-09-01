package server

import (
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
		iv.DiagnosticInfo = diagnosticInfo
	}
	if !haveSample {
		// Quality is deliberately left nil, not set to the zero
		// OPCQuality: <Quality> is minOccurs="0" in the schema, and an
		// attribute-less <Quality/> element reads as QualityField="good"
		// under the schema's own defaults. Emitting one for an item that
		// has no sample at all reported e.g. an unknown item name as
		// good-quality-with-no-value — a direct contradiction of the
		// ResultID on the same element, and the half of it that reaches
		// the process image of any client mapping this onto OPC DA's
		// wQuality. See xmlda.ItemValue.Quality.
		return iv
	}
	q := sample.Quality
	iv.Quality = &q
	haveLastKnown := !sample.Value.IsNil()
	if xmlda.ResolveValuePresence(sample.Quality, haveLastKnown) {
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

// errorTextFunc returns the textOf function xmlda.DedupeErrors should use,
// honoring RequestOptions.ReturnErrorText — when false, Errors entries
// still identify which codes occurred (that's the core per-item result
// mechanism, not "error text"), but carry no human-readable Text.
//
// When text is wanted it goes through Handler.errorText, so an
// application-supplied Config.ErrorText sees both the code and the locale
// the server actually resolved for this request — the same locale it
// reports in ReplyBase.RevisedLocaleID. Answering "de-DE" there and then
// sending English text is the contradiction §2.6 asks servers to avoid.
func (h *Handler) errorTextFunc(opts xmlda.RequestOptions, oc opContext) func(xmlda.ErrorCode) string {
	if !opts.ReturnErrorTextOrDefault() {
		return func(xmlda.ErrorCode) string { return "" }
	}
	locale := reviseLocale(opts.LocaleID, oc.status.SupportedLocaleIDs)
	return func(c xmlda.ErrorCode) string { return h.errorText(c, locale) }
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
func observeBackend[T any](m telemetry.Metrics, clk clock.Clock, operation string, call func() (T, error)) (T, error) {
	start := clk.Now()
	v, err := call()
	m.ObserveBackendLatency(operation, clk.Now().Sub(start))
	return v, err
}
