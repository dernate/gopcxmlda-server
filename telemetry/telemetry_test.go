package telemetry

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

// var _ Logger = (*slog.Logger)(nil) is a compile-time assertion that
// *slog.Logger satisfies Logger with zero adapter code, as documented.
var _ Logger = (*slog.Logger)(nil)

func TestSlogLoggerSatisfiesInterface(t *testing.T) {
	var l Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	// Must not panic.
	l.Debug("debug", "k", "v")
	l.Info("info")
	l.Warn("warn")
	l.Error("error")
}

func TestNoopLogger_NeverPanics(t *testing.T) {
	l := NoopLogger()
	l.Debug("x")
	l.Info("x")
	l.Warn("x")
	l.Error("x")
}

func TestNoopMetrics_NeverPanics(t *testing.T) {
	m := NoopMetrics()
	m.IncRequest("Read")
	m.IncRequestError("Read", "timeout")
	m.ObserveBackendLatency("Read", time.Millisecond)
	m.SetActiveSubscriptions(3)
	m.IncSubscriptionError("busy")
	m.IncParseError()
}
