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

// NoopMetrics returns a Metrics that discards everything — the default
// when no Metrics is configured.
func NoopMetrics() Metrics { return noopMetrics{} }
