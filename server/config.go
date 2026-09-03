package server

import (
	"time"

	"github.com/dernate/gopcxmlda-server/subscription"
	"github.com/dernate/gopcxmlda-server/xmlda"
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
	// MaxElementDepth bounds how deeply a request document may nest its
	// elements, checked on the first tokenizer pass — before any protocol
	// limit, all of which apply only after a full decode.
	//
	// It is the companion MaxRequestBodyBytes needs: that field bounds the
	// input, this one bounds what the input costs. Nesting is charged
	// super-linearly in encoding/xml's own bookkeeping, so a body of
	// nothing but nested start tags turns 4 MiB into roughly 128 MiB of
	// live heap and a second of CPU per request. Zero applies the built-in
	// default (xmlda.DefaultMaxElementDepth, 64 — the deepest conforming
	// shape in the schema is seven levels); a negative value explicitly
	// requests no limit.
	MaxElementDepth int
	// MaxConcurrentPolledRefresh bounds how many SubscriptionPolledRefresh
	// calls may be in flight at once, as a sub-budget of
	// MaxConcurrentRequests.
	//
	// The two operation classes have nothing in common but the semaphore
	// they used to share: a Read passes through in milliseconds, while a
	// long poll legitimately holds its slot for up to
	// MaxPolledRefreshWait. Without a separate budget, enough concurrent
	// long polls answer every other client's Read, Write and GetStatus
	// with E_BUSY for minutes at a time — and with no authentication in
	// front of the server, "enough" is whatever one client chooses to
	// open. Zero applies the built-in default (three quarters of the
	// effective MaxConcurrentRequests, leaving a quarter that short
	// operations can always reach); a negative value explicitly requests
	// no separate limit.
	MaxConcurrentPolledRefresh int
	// BackendTimeout bounds how long the server waits for one
	// backend.Backend call before answering E_TIMEDOUT, enforced rather
	// than merely requested.
	//
	// RequestTimeout and PollTimeout are context deadlines, and a context
	// deadline is a request to stop, not a mechanism that stops anything:
	// a backend that reaches a device through a blocking call and never
	// consults ctx holds its handler goroutine — and its
	// MaxConcurrentRequests slot — for as long as the device stays
	// unresponsive. Measured: four such calls against a stuck backend and
	// the fifth request gets E_BUSY, permanently, while the four clients
	// never receive an answer at all.
	//
	// With this set, the call runs on its own goroutine and the handler
	// stops waiting when the timeout elapses. The abandoned goroutine
	// still runs — Go cannot cancel it — but it no longer holds the
	// request, so the server stays answerable. Zero applies the built-in
	// default (RequestTimeout plus a small grace); a negative value
	// restores the previous cooperative-only behavior.
	BackendTimeout time.Duration
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
	// MaxBrowseElements bounds how many elements one Browse response may
	// carry. It is both a clamp on the client's own MaxElementsReturned
	// and a ceiling enforced on the backend's answer: Browse is the only
	// operation whose result size the client may leave unbounded
	// (MaxElementsReturned=0 means "no limit"), and the whole response is
	// assembled in memory before anything is written. A truncated result
	// is reported with MoreElements=true, so a client can page through it
	// normally. Zero applies the built-in default (5000); a negative value
	// explicitly requests no limit.
	MaxBrowseElements int
	// MaxTotalSubscribedItems bounds the number of subscribed items across
	// all subscriptions at once. Without it, MaxConcurrentSubscriptions
	// and MaxItemsPerSubscription multiply: the per-axis defaults alone
	// permit ten million live items, each holding its own last sample.
	// Zero applies the built-in default (200000); a negative value
	// explicitly requests no limit.
	MaxTotalSubscribedItems int
	// MaxTotalBufferedSamples bounds how many buffered (undelivered)
	// samples may be held across every subscribed item at once — the third
	// axis of the same multiplication MaxTotalSubscribedItems addresses.
	//
	// MaxTotalSubscribedItems (200000) and MaxBufferedSamplesPerItem (100)
	// together permit twenty million buffered entries, each holding a full
	// xmlda.Value; the per-item cap says nothing about the total. When the
	// budget is exhausted, a buffering item falls back to retaining only
	// its Latest Changed Value (which REQ-SUBSCRIPTION-007 preserves
	// regardless) and the next reply sets DataBufferOverflow, so the loss
	// is reported rather than silent. Zero applies the built-in default
	// (1000000); a negative value explicitly requests no limit.
	MaxTotalBufferedSamples int

	// MaxConcurrentRequests bounds how many requests may be in flight at
	// once across the whole server; a request arriving when the limit is
	// reached is rejected with an E_BUSY fault rather than queued.
	//
	// Every other limit here is per request or per subscription, and none
	// of them bounds concurrency. A SubscriptionPolledRefresh legitimately
	// holds its handler goroutine, its TCP connection and its response
	// buffer for up to MaxPolledRefreshWait, so without this a client can
	// pin an unbounded number of them at once — the cheapest way to
	// exhaust a server that is otherwise carefully bounded everywhere
	// else. Zero applies the built-in default (1024); a negative value
	// explicitly requests no limit.
	//
	// Size it above the number of concurrent long-polls the deployment
	// expects, not to its request rate: short operations pass through in
	// milliseconds and barely occupy a slot.
	MaxConcurrentRequests int

	// ContinuationPointTTL is how long a Browse continuation point this
	// server issued remains usable. Zero applies the built-in default
	// (10 minutes); a negative value means the token never expires on its
	// own (it still stops working when the process restarts, since the key
	// that authenticates it is generated per Handler — see
	// continuation.go).
	ContinuationPointTTL time.Duration

	// StrictHoldTime rejects a SubscriptionPolledRefresh whose HoldTime is
	// further out than MaxPolledRefreshWait with an E_INVALIDHOLDTIME
	// fault, instead of the default behavior of clamping the hold to that
	// ceiling and answering normally.
	//
	// Clamping is the default because the specification's own guidance for
	// HoldTime is loose ("generally no more than a minute or two", §3.1.6)
	// while this ceiling is an exact number: a client that reads that
	// sentence and picks 120s against a lower ceiling would otherwise
	// fault on every single poll and never receive its subscription's data
	// at all. Set this only when a client silently getting a shorter hold
	// than it asked for is the worse failure.
	StrictHoldTime bool

	// RequiresFault decides whether an operation must be rejected with a
	// whole-operation fault given the server's current state, before any
	// backend call is made (REQ-SERVER-002). nil applies
	// xmlda.RequiresFault, this library's own reading of §2.6.
	//
	// It is a hook because the reading is a policy, not a protocol
	// constant, and the specification's serverState descriptions leave
	// room: xmlda.RequiresFault lets SubscriptionPolledRefresh through
	// under "suspended", and treats "commFault" and "test" as fully
	// operational — all defensible, none obligatory. An operator who
	// wants writes blocked while the data source is unreachable, say,
	// needs to be able to say so without forking the library.
	RequiresFault func(op string, state xmlda.ServerState) (bool, xmlda.ErrorCode)

	// ReadOnly, if true, globally disables Write regardless of whether
	// the backend supplies a Writer — the specification's own
	// recommended policy hook (REQ-SECURITY-002, §2.8).
	ReadOnly bool

	// ErrorText supplies the human-readable text for a result code in the
	// response's Errors list. It receives the code and the locale the
	// server resolved for the request (ReplyBase.RevisedLocaleID), and
	// should fall back to xmlda.StandardErrorText for codes it does not
	// handle.
	//
	// The specification asks for exactly this (§2.6: "The XML-DA server
	// should also return the error text for the LocaleID specified in the
	// request"), and it is also the only way to give a vendor result code
	// any text at all — xmlda.StandardErrorText returns "" for codes it
	// does not know, which omits the <Text> element that §3.1.9 says
	// every OPCError carries.
	//
	// nil keeps the built-in behavior: xmlda.StandardErrorText, English,
	// locale-independent.
	ErrorText func(code xmlda.ErrorCode, locale string) string

	// StatusCacheTTL is how long the ServerStatus fetched for a request's
	// state check (REQ-SERVER-002 evaluates xmlda.RequiresFault before
	// every operation) may be reused across requests. Without it every
	// single request — Read, Write, Cancel, all of them — costs one extra
	// backend GetStatus call, which for a backend that reaches a device to
	// answer it doubles the load and adds its latency to every operation.
	//
	// ServerState does not change on a millisecond scale, so a short TTL
	// costs nothing in correctness. GetStatus's own response is never
	// served from this cache: that operation always re-fetches, so a
	// client explicitly asking for the status still gets a live answer.
	// Zero applies the built-in default (250ms); a negative value disables
	// caching entirely (a fresh fetch per request, the previous behavior).
	StatusCacheTTL time.Duration

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

// backendTimeoutGrace is how far BackendTimeout's default sits beyond
// RequestTimeout: the request's own context deadline should be what a
// cooperative backend observes, and this hard ceiling only catches the
// ones that do not.
const backendTimeoutGrace = 5 * time.Second

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
	if c.MaxBrowseElements == 0 {
		c.MaxBrowseElements = 5000
	}
	if c.MaxTotalSubscribedItems == 0 {
		c.MaxTotalSubscribedItems = 200000
	}
	if c.MaxTotalBufferedSamples == 0 {
		c.MaxTotalBufferedSamples = 1000000
	}
	if c.MaxConcurrentRequests == 0 {
		c.MaxConcurrentRequests = 1024
	}
	if c.ContinuationPointTTL == 0 {
		c.ContinuationPointTTL = 10 * time.Minute
	}
	if c.StatusCacheTTL == 0 {
		c.StatusCacheTTL = 250 * time.Millisecond
	}
	if c.MaxRequestBodyBytes <= 0 {
		c.MaxRequestBodyBytes = 4 << 20 // 4 MiB
	}
	if c.MaxElementDepth == 0 {
		c.MaxElementDepth = xmlda.DefaultMaxElementDepth
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 30 * time.Second
	}
	if c.MaxConcurrentPolledRefresh == 0 && c.MaxConcurrentRequests > 0 {
		// Three quarters, so a saturated long-poll population still leaves
		// a quarter of the capacity for the operations that answer in
		// milliseconds. At least one, so a tiny MaxConcurrentRequests does
		// not disable polling outright.
		c.MaxConcurrentPolledRefresh = max(1, c.MaxConcurrentRequests*3/4)
	}
	if c.BackendTimeout == 0 {
		c.BackendTimeout = c.RequestTimeout + backendTimeoutGrace
	}
	if c.MaxPolledRefreshWait <= 0 {
		// 120s, not 90s: the specification's own guidance is "generally no
		// more than a minute or two" (§3.1.6), and a client that reads it
		// literally picks two minutes. A ceiling below the number clients
		// actually choose turns every poll into a clamp (or, with
		// StrictHoldTime, a fault).
		c.MaxPolledRefreshWait = 120 * time.Second
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
	// The seven fields this struct only forwards to subscription.Config
	// are resolved here too, from that package's own defaults. WithDefaults
	// promises "every unset field replaced by its built-in default" and is
	// exported precisely so an application can inspect the effective
	// limits — for a log line, a diagnostics endpoint, a sizing
	// calculation. Leaving these seven at zero made that inspection wrong
	// in exactly the places where the numbers matter.
	c.applySubscriptionDefaults()
	return c
}

// applySubscriptionDefaults fills in the fields whose real defaults live
// in subscription.Config, by asking that package rather than restating
// its numbers here — two copies of a default is how they drift.
func (c *Config) applySubscriptionDefaults() {
	d := subscription.Config{
		MaxConcurrentPolls:          c.MaxConcurrentPolls,
		ReapInterval:                c.ReapInterval,
		ReapGraceMultiplier:         c.ReapGraceMultiplier,
		DefaultSubscriptionPingRate: c.DefaultSubscriptionPingRate,
		DefaultSamplingRate:         c.DefaultSamplingRate,
		MaxBufferedSamplesPerItem:   c.MaxBufferedSamplesPerItem,
		PollTimeout:                 c.PollTimeout,
	}.WithDefaults()
	c.MaxConcurrentPolls = d.MaxConcurrentPolls
	c.ReapInterval = d.ReapInterval
	c.ReapGraceMultiplier = d.ReapGraceMultiplier
	c.DefaultSubscriptionPingRate = d.DefaultSubscriptionPingRate
	c.DefaultSamplingRate = d.DefaultSamplingRate
	c.MaxBufferedSamplesPerItem = d.MaxBufferedSamplesPerItem
	c.PollTimeout = d.PollTimeout
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
		MaxTotalSubscribedItems:     c.MaxTotalSubscribedItems,
		MaxTotalBufferedSamples:     c.MaxTotalBufferedSamples,
	}
}
