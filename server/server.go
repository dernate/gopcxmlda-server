package server

import (
	"context"
	"errors"
	"net/http"
)

// Server is an optional convenience wrapper around net/http.Server for
// applications that don't want to assemble their own. Applications that
// already have an http.Server/router can instead call New directly and
// mount the resulting Handler wherever they choose.
type Server struct {
	httpServer *http.Server
	handler    *Handler
}

// NewServer constructs a Server listening on addr.
func NewServer(addr string, deps Deps, cfg Config) (*Server, error) {
	h, err := New(deps, cfg)
	if err != nil {
		return nil, err
	}
	return &Server{
		httpServer: &http.Server{
			Addr:              addr,
			Handler:           h,
			ReadHeaderTimeout: h.cfg.ReadHeaderTimeout,
			ReadTimeout:       h.cfg.ReadTimeout,
			IdleTimeout:       h.cfg.IdleTimeout,
		},
		handler: h,
	}, nil
}

// Handler returns the underlying http.Handler.
func (s *Server) Handler() http.Handler { return s.handler }

// Start begins serving and blocks until the server stops (typically via
// Shutdown) or a listener error occurs. A graceful Shutdown is reported
// as a nil error, not http.ErrServerClosed.
func (s *Server) Start() error {
	err := s.httpServer.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown gracefully stops the server. Ordering matters: subscriptions
// are cancelled first — unblocking every in-flight SubscriptionPolledRefresh
// call immediately — before http.Server.Shutdown starts waiting for
// in-flight handlers, so shutdown does not hang for the duration of a
// client's requested Hold+Wait window. See
// docs/architecture/subscription-model.md.
func (s *Server) Shutdown(ctx context.Context) error {
	s.handler.BeginShutdown()
	httpErr := s.httpServer.Shutdown(ctx)
	subErr := s.handler.subs.Wait(ctx)
	return errors.Join(httpErr, subErr)
}
