package server

import (
	"testing"
	"time"
)

// TestConfig_WithDefaults_ZeroMeansDefault_NegativeMeansUnlimited checks
// the "0 = unlimited" sentinel documented on MaxConcurrentSubscriptions/
// MaxItemsPerRequest/MaxItemsPerSubscription (and on
// checkItemCount/checkSubscriptionItemCount's own doc comments) is
// actually reachable: before this was fixed, WithDefaults unconditionally
// replaced any value <= 0 with the built-in default, so an application
// that deliberately set 0 to opt into "no limit" silently got capped
// anyway. The sentinel for "explicitly unlimited" is now a negative
// value; 0 (the Go zero value, i.e. "unset") still gets the default.
func TestConfig_WithDefaults_ZeroMeansDefault_NegativeMeansUnlimited(t *testing.T) {
	def := Config{}.WithDefaults()
	if def.MaxItemsPerRequest != 1000 {
		t.Fatalf("MaxItemsPerRequest: got %d, want default 1000", def.MaxItemsPerRequest)
	}
	if def.MaxItemsPerSubscription != 1000 {
		t.Fatalf("MaxItemsPerSubscription: got %d, want default 1000", def.MaxItemsPerSubscription)
	}
	if def.MaxConcurrentSubscriptions != 10000 {
		t.Fatalf("MaxConcurrentSubscriptions: got %d, want default 10000", def.MaxConcurrentSubscriptions)
	}

	unlimited := Config{
		MaxItemsPerRequest:         -1,
		MaxItemsPerSubscription:    -1,
		MaxConcurrentSubscriptions: -1,
	}.WithDefaults()
	if unlimited.MaxItemsPerRequest >= 0 {
		t.Fatalf("MaxItemsPerRequest: got %d, want a negative (unlimited) value to survive WithDefaults", unlimited.MaxItemsPerRequest)
	}
	if unlimited.MaxItemsPerSubscription >= 0 {
		t.Fatalf("MaxItemsPerSubscription: got %d, want a negative (unlimited) value to survive WithDefaults", unlimited.MaxItemsPerSubscription)
	}
	if unlimited.MaxConcurrentSubscriptions >= 0 {
		t.Fatalf("MaxConcurrentSubscriptions: got %d, want a negative (unlimited) value to survive WithDefaults", unlimited.MaxConcurrentSubscriptions)
	}

	// And the consuming checks actually treat that negative value as
	// unlimited, not just WithDefaults leaving it alone.
	h := &Handler{cfg: unlimited}
	if !h.checkItemCount(1_000_000) {
		t.Fatalf("checkItemCount: expected a negative MaxItemsPerRequest to mean unlimited")
	}
	if !h.checkSubscriptionItemCount(1_000_000) {
		t.Fatalf("checkSubscriptionItemCount: expected a negative MaxItemsPerSubscription to mean unlimited")
	}
}

// TestConfig_WithDefaults_PositiveValuePreserved checks an explicit
// positive limit is left untouched by WithDefaults.
func TestConfig_WithDefaults_PositiveValuePreserved(t *testing.T) {
	got := Config{MaxItemsPerRequest: 5, MaxConcurrentSubscriptions: 3}.WithDefaults()
	if got.MaxItemsPerRequest != 5 {
		t.Fatalf("MaxItemsPerRequest: got %d, want 5", got.MaxItemsPerRequest)
	}
	if got.MaxConcurrentSubscriptions != 3 {
		t.Fatalf("MaxConcurrentSubscriptions: got %d, want 3", got.MaxConcurrentSubscriptions)
	}
}

// TestConfig_WithDefaults_ConnectionTimeouts checks the http.Server-level
// timeout defaults (ReadHeaderTimeout/ReadTimeout/IdleTimeout) that
// NewServer relies on to mitigate a slow-header/slow-body connection
// holding a handling goroutine open indefinitely.
func TestConfig_WithDefaults_ConnectionTimeouts(t *testing.T) {
	def := Config{}.WithDefaults()
	if def.ReadHeaderTimeout != 10*time.Second {
		t.Fatalf("ReadHeaderTimeout: got %v, want 10s default", def.ReadHeaderTimeout)
	}
	if def.ReadTimeout != 30*time.Second {
		t.Fatalf("ReadTimeout: got %v, want 30s default", def.ReadTimeout)
	}
	if def.IdleTimeout != 120*time.Second {
		t.Fatalf("IdleTimeout: got %v, want 120s default", def.IdleTimeout)
	}

	custom := Config{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}.WithDefaults()
	if custom.ReadHeaderTimeout != 5*time.Second || custom.ReadTimeout != 15*time.Second || custom.IdleTimeout != 60*time.Second {
		t.Fatalf("got %+v, want explicit values preserved", custom)
	}
}

// TestNewServer_WiresConnectionTimeoutsFromConfig guards against a
// regression where server.NewServer's http.Server set none of Go's
// connection-level timeouts, leaving a slow-drip client able to hold a
// connection (and its handling goroutine) open indefinitely regardless
// of Config.RequestTimeout (which only bounds handler execution time,
// starting after the full body is already read).
func TestNewServer_WiresConnectionTimeoutsFromConfig(t *testing.T) {
	be, _, _ := newMinimalBackend()
	s, err := NewServer("127.0.0.1:0", Deps{Backend: be}, Config{
		ReadHeaderTimeout: 7 * time.Second,
		ReadTimeout:       21 * time.Second,
		IdleTimeout:       42 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if s.httpServer.ReadHeaderTimeout != 7*time.Second {
		t.Fatalf("ReadHeaderTimeout: got %v, want 7s", s.httpServer.ReadHeaderTimeout)
	}
	if s.httpServer.ReadTimeout != 21*time.Second {
		t.Fatalf("ReadTimeout: got %v, want 21s", s.httpServer.ReadTimeout)
	}
	if s.httpServer.IdleTimeout != 42*time.Second {
		t.Fatalf("IdleTimeout: got %v, want 42s", s.httpServer.IdleTimeout)
	}
}
