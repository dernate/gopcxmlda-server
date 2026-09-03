package server

import (
	"encoding/json"
	"net/http"
	"time"
)

// Stats is a snapshot of a Handler's live state, for a health or
// diagnostics endpoint.
//
// It exists because none of this was reachable from outside: the SOAP
// endpoint answers every GET with 405 — correctly, OPC XML-DA is
// POST-only — so a Kubernetes httpGet probe against it always failed, and
// an application had no way to build its own probe either. The numbers
// here are the ones an operator actually watches.
type Stats struct {
	// ActiveSubscriptions is the number of subscriptions currently held.
	ActiveSubscriptions int `json:"activeSubscriptions"`
	// InFlightRequests and MaxConcurrentRequests together say how close
	// the server is to answering E_BUSY. MaxConcurrentRequests is -1 when
	// the limit is disabled.
	InFlightRequests      int `json:"inFlightRequests"`
	MaxConcurrentRequests int `json:"maxConcurrentRequests"`
	// InFlightPolledRefresh and MaxConcurrentPolledRefresh are the same
	// pair for the long-poll class, which has its own budget.
	InFlightPolledRefresh      int `json:"inFlightPolledRefresh"`
	MaxConcurrentPolledRefresh int `json:"maxConcurrentPolledRefresh"`
	// ShuttingDown reports that Shutdown or BeginShutdown has been called.
	// A readiness probe should fail on this so the load balancer stops
	// sending work before the server starts refusing it.
	ShuttingDown bool `json:"shuttingDown"`
	// BackendReachable reports whether the most recent server-status fetch
	// succeeded, and BackendState the ServerState it reported. Both are
	// zero until the first request or health check.
	BackendReachable bool      `json:"backendReachable"`
	BackendState     string    `json:"backendState,omitempty"`
	BackendCheckedAt time.Time `json:"backendCheckedAt,omitzero"`
}

// Stats returns a snapshot of the handler's live state.
func (h *Handler) Stats() Stats {
	s := Stats{
		ActiveSubscriptions:        h.subs.Count(),
		ShuttingDown:               h.shuttingDown.Load(),
		MaxConcurrentRequests:      -1,
		MaxConcurrentPolledRefresh: -1,
	}
	if h.reqSem != nil {
		s.InFlightRequests, s.MaxConcurrentRequests = len(h.reqSem), cap(h.reqSem)
	}
	if h.pollSem != nil {
		s.InFlightPolledRefresh, s.MaxConcurrentPolledRefresh = len(h.pollSem), cap(h.pollSem)
	}
	select {
	case h.statusLock <- struct{}{}:
		s.BackendReachable, s.BackendCheckedAt = h.statusOK, h.statusFresh
		s.BackendState = string(h.statusVal.State)
		<-h.statusLock
	default:
		// A fetch is in flight; report what is knowable without waiting
		// for it, because a health endpoint that blocks on a stuck
		// backend is the one thing it must never do.
	}
	return s
}

// HealthHandler returns an http.Handler for liveness and readiness
// probes, to be mounted alongside the SOAP endpoint — never on the same
// path, since OPC XML-DA is POST-only and a probe is a GET.
//
//	mux := http.NewServeMux()
//	mux.Handle("/opcxmlda", opcHandler)
//	mux.Handle("/healthz", opcHandler.HealthHandler())
//
// It answers 200 while the server is serving and 503 once shutdown has
// begun, with the Stats snapshot as a JSON body either way. It never
// calls the backend and never blocks on one: a probe that hangs when the
// data source hangs turns one unreachable device into a restart loop.
func (h *Handler) HealthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", http.MethodGet+", "+http.MethodHead)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		stats := h.Stats()
		code := http.StatusOK
		if stats.ShuttingDown {
			code = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		if r.Method == http.MethodHead {
			return
		}
		_ = json.NewEncoder(w).Encode(stats)
	})
}
