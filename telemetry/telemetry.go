// Package telemetry defines small, optional Logger and Metrics
// interfaces so this library never forces a specific logging or metrics
// stack on an application. Both default to no-ops. This package has no
// dependency on server or subscription, specifically so both of those
// packages can depend on it without creating an import cycle over shared
// Logger/Metrics contracts.
package telemetry

import "time"

// Logger mirrors log/slog's leveled methods exactly, so *slog.Logger
// already satisfies this interface with zero adapter code, without this
// package depending on log/slog.
//
// Hard rule, enforced by convention across every package in this
// library: no log call anywhere logs a full SOAP request/response body
// or an item value by default — process data is sensitive. Default log
// lines carry operation name, handle IDs, item counts, and durations —
// never item values.
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// Metrics is a small set of optional hook points for request/error
// counts, active subscriptions, backend latency, and parse errors. It
// does not force any specific monitoring library — an application wires
// its own implementation.
type Metrics interface {
	// IncRequest counts one request for the named operation (e.g. "Read").
	IncRequest(operation string)
	// IncRequestError counts one request-level error for operation,
	// tagged with a cause such as "fault", "backend_error", or "timeout".
	IncRequestError(operation, cause string)
	// ObserveBackendLatency records how long a backend call for
	// operation took.
	ObserveBackendLatency(operation string, d time.Duration)
	// SetActiveSubscriptions reports the current subscription count.
	SetActiveSubscriptions(n int)
	// IncSubscriptionError counts one subscription-related error, tagged
	// with a cause such as "busy", "timeout", or "invalid_handle".
	IncSubscriptionError(cause string)
	// IncParseError counts one request body that failed to parse.
	IncParseError()

	// ObserveRequestLatency records how long the server took to answer one
	// request, measured from receipt to the moment the response is
	// written.
	//
	// ObserveBackendLatency answers "is the data source slow"; this one
	// answers the other half, "is the server slow", which without it could
	// not be asked at all. The two together are what separates a field
	// problem from a server problem — usually the first question after a
	// client complains.
	ObserveRequestLatency(operation string, d time.Duration)

	// IncDroppedSamples counts n subscription samples the server discarded
	// because a buffer limit was reached.
	//
	// This is the moment the server loses process data. It was previously
	// neither logged nor counted anywhere: the only trace was the
	// DataBufferOverflow flag on whichever reply happened to follow, which
	// tells a client something was lost but tells an operator nothing
	// about how often or where.
	IncDroppedSamples(n int)

	// ObservePollLag records how far behind schedule one poll-mode tick
	// ran — the difference between when it was due and when it actually
	// executed.
	//
	// A subscription promises the client a RevisedSamplingRate, and the
	// promise quietly stops being true once the poll semaphore saturates
	// or the backend slows down. Nothing else in this interface can see
	// that: the subscription keeps working, just slower than it said,
	// which is the failure mode hardest to notice and easiest to fix once
	// visible.
	ObservePollLag(d time.Duration)
}

type noopLogger struct{}

func (noopLogger) Debug(msg string, args ...any) {}
func (noopLogger) Info(msg string, args ...any)  {}
func (noopLogger) Warn(msg string, args ...any)  {}
func (noopLogger) Error(msg string, args ...any) {}

// NoopLogger returns a Logger that discards everything — the default
// when no Logger is configured.
func NoopLogger() Logger { return noopLogger{} }

type noopMetrics struct{}

func (noopMetrics) IncRequest(operation string)                             {}
func (noopMetrics) IncRequestError(operation, cause string)                 {}
func (noopMetrics) ObserveBackendLatency(operation string, d time.Duration) {}
func (noopMetrics) SetActiveSubscriptions(n int)                            {}
func (noopMetrics) IncSubscriptionError(cause string)                       {}
func (noopMetrics) IncParseError()                                          {}
func (noopMetrics) ObserveRequestLatency(operation string, d time.Duration) {}
func (noopMetrics) IncDroppedSamples(n int)                                 {}
func (noopMetrics) ObservePollLag(d time.Duration)                          {}

// NoopMetrics returns a Metrics that discards everything — the default
// when no Metrics is configured.
func NoopMetrics() Metrics { return noopMetrics{} }
