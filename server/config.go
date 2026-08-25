package server

import (
	"time"

	"github.com/dernate/gopcxmlda-server/subscription"
)

// Config holds every tunable limit and policy for a Handler/Server. The
// specification defines no numeric limits at all (REQ-LIMITS-001); every
// field below is an implementation default, not a specification
// requirement — see ADR-011.
type Config struct {
	// MaxItemsPerRequest bounds how many items a single Read/Write/
	// Subscribe/GetProperties request may carry. Zero (the Go zero value)
	// applies the built-in default (1000); a negative value explicitly
	// requests no limit — see checkItemCount.
	MaxItemsPerRequest int
	// MaxItemsPerSubscription bounds how many items one subscription may
	// hold. Zero applies the built-in default (1000); a negative value
	// explicitly requests no limit — see checkSubscriptionItemCount.
	MaxItemsPerSubscription int
	// MaxConcurrentSubscriptions bounds how many subscriptions may exist
	// at once across the whole server. Zero applies the built-in default
	// (10000); a negative value explicitly requests no limit. (An
	// embedding application that wants exactly the *previous* "0 means
	// unlimited" behavior must now use a negative value instead — zero
	// no longer bypasses the default, since a Config{} caller relying on
	// safe-by-default limits must not silently lose them.)
	MaxConcurrentSubscriptions int
	// MaxRequestBodyBytes bounds the size of an incoming HTTP request
	// body, enforced via http.MaxBytesReader before any XML parsing.
	MaxRequestBodyBytes int64
	// RequestTimeout bounds every non-subscription-poll operation.
	RequestTimeout time.Duration
	// MaxPolledRefreshWait caps the client-requested Hold+Wait duration
	// for SubscriptionPolledRefresh — deliberately somewhat above the
	// specification's own loose guidance ("generally no more than a
	// minute or two", §3.1.6) to give headroom.
	MaxPolledRefreshWait time.Duration
	// MaxConcurrentPolls bounds concurrent poll-mode backend calls
	// across all subscriptions (forwarded to subscription.Config).
	MaxConcurrentPolls int
	// ReapInterval and ReapGraceMultiplier control abandonment cleanup
	// (forwarded to subscription.Config).
	ReapInterval        time.Duration
	ReapGraceMultiplier float64
	// DefaultSubscriptionPingRate is substituted when a client sends
	// SubscriptionPingRate=0 (forwarded to subscription.Config).
	DefaultSubscriptionPingRate time.Duration
	// DefaultSamplingRate is substituted when a client requests
	// RequestedSamplingRate=0 (forwarded to subscription.Config).
	DefaultSamplingRate time.Duration
	// MaxBufferedSamplesPerItem bounds per-item buffered changes
	// (forwarded to subscription.Config).
	MaxBufferedSamplesPerItem int
	// PollTimeout bounds each poll-mode backend.Reader.Read call
	// (forwarded to subscription.Config).
	PollTimeout time.Duration
	// ReadOnly, if true, globally disables Write regardless of whether
	// the backend supplies a Writer — the specification's own
	// recommended policy hook (REQ-SECURITY-002, §2.8).
	ReadOnly bool

	// ReadHeaderTimeout bounds how long server.NewServer's http.Server
	// waits to finish reading a request's headers, mitigating a
	// slow-header ("slowloris") connection. RequestTimeout's
	// context.WithTimeout does not help here: that context is only
	// created after the full body has already been read (handler.go), so
	// it cannot bound header- or body-read time. This field only applies
	// to the server.NewServer convenience wrapper; a caller that mounts
	// Handler into their own http.Server must set connection-level
	// timeouts themselves. Zero or negative applies the built-in default
	// (10s).
	ReadHeaderTimeout time.Duration
	// ReadTimeout bounds how long server.NewServer's http.Server waits to
	// finish reading the full request (headers + body), mitigating a
	// slow-drip client that trickles bytes just under
	// MaxRequestBodyBytes to hold a connection (and its handling
	// goroutine) open indefinitely. If MaxRequestBodyBytes is raised well
	// above its default for legitimately large requests, raise
	// ReadTimeout accordingly. Only applies to server.NewServer. Zero or
	// negative applies the built-in default (30s).
	ReadTimeout time.Duration
	// IdleTimeout bounds how long server.NewServer's http.Server keeps an
	// idle keep-alive connection open between requests. Only applies to
	// server.NewServer. Zero or negative applies the built-in default
	// (120s).
	//
	// WriteTimeout is deliberately not configured here: it would bound
	// the entire response-write window including a long-poll
	// SubscriptionPolledRefresh's Hold+Wait, which can legitimately run
	// up to MaxPolledRefreshWait — a fixed WriteTimeout could either cut
	// those short or, set high enough not to, add no real protection
	// beyond what RequestTimeout/MaxPolledRefreshWait's context deadlines
	// already provide for handler execution time.
	IdleTimeout time.Duration
}

// WithDefaults returns a copy of c with every unset (zero-value) field
// replaced by its built-in default. server.New/server.NewServer call
// this internally, but it is also exported so an embedding application
// can inspect the effective, fully-resolved limits — e.g. for logging or
// a health/diagnostics endpoint — without constructing a real
// backend.Backend just to get a *Handler.
func (c Config) WithDefaults() Config {
	// Exactly zero (unset) gets the default; a negative value is a
	// deliberate "no limit" request and must survive WithDefaults
	// unchanged — checkItemCount/checkSubscriptionItemCount and
	// subscription.Create already treat <= 0 as unlimited downstream, but
	// only if WithDefaults doesn't clobber it first.
	if c.MaxItemsPerRequest == 0 {
		c.MaxItemsPerRequest = 1000
	}
	if c.MaxItemsPerSubscription == 0 {
		c.MaxItemsPerSubscription = 1000
	}
	if c.MaxConcurrentSubscriptions == 0 {
		c.MaxConcurrentSubscriptions = 10000
	}
	if c.MaxRequestBodyBytes <= 0 {
		c.MaxRequestBodyBytes = 4 << 20 // 4 MiB
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 30 * time.Second
	}
	if c.MaxPolledRefreshWait <= 0 {
		c.MaxPolledRefreshWait = 90 * time.Second
	}
	if c.ReadHeaderTimeout <= 0 {
		c.ReadHeaderTimeout = 10 * time.Second
	}
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = 30 * time.Second
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 120 * time.Second
	}
	return c
}

// subscriptionConfig maps Config's overlapping fields onto
// subscription.Config; fields not set (zero) are left for
// subscription.Config's own defaults to fill in.
func (c Config) subscriptionConfig() subscription.Config {
	return subscription.Config{
		MaxConcurrentSubscriptions:  c.MaxConcurrentSubscriptions,
		MaxConcurrentPolls:          c.MaxConcurrentPolls,
		ReapInterval:                c.ReapInterval,
		ReapGraceMultiplier:         c.ReapGraceMultiplier,
		DefaultSubscriptionPingRate: c.DefaultSubscriptionPingRate,
		DefaultSamplingRate:         c.DefaultSamplingRate,
		MaxBufferedSamplesPerItem:   c.MaxBufferedSamplesPerItem,
		PollTimeout:                 c.PollTimeout,
	}
}
