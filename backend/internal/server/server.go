// Package server is the HTTP control plane for fishhawkd.
//
// It owns the http.Server lifecycle (Start, Shutdown), the middleware
// stack, and the route registration. Handlers are deliberately thin —
// real workflow logic lives in adjacent packages as it's added under
// epic E3 (#3).
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// Config holds the values needed to construct a Server. Zero-valued
// fields fall back to safe defaults.
type Config struct {
	// Addr is the listen address (e.g. ":8080" or "127.0.0.1:0").
	Addr string

	// Logger is the structured logger used by middleware. Defaults to
	// slog.Default() when nil.
	Logger *slog.Logger

	// ShutdownTimeout caps Shutdown's wait for in-flight requests to
	// drain before forcing closure.
	ShutdownTimeout time.Duration

	// RunRepo persists workflow runs and stages. Wired by the
	// /v0/runs handlers; nil leaves those handlers returning 503.
	// Tests inject in-memory fakes; production wires the Postgres
	// adapter (run.NewPostgresRepository).
	RunRepo run.Repository

	// GitHubWebhookSecret is the shared secret GitHub uses to
	// HMAC-sign webhook deliveries. Empty disables the
	// /webhooks/github endpoint (handler returns 503).
	GitHubWebhookSecret []byte

	// WebhookDeliveries dedups GitHub webhook deliveries by their
	// X-GitHub-Delivery UUID across the GitHub retry window. nil
	// disables the /webhooks/github endpoint.
	WebhookDeliveries webhook.DeliveryStore
}

// Server wraps an http.Server with the routes and middleware stack
// that make up fishhawkd's API surface.
type Server struct {
	cfg  Config
	http *http.Server
}

// New builds a Server. It does not start listening; call Start.
func New(cfg Config) *Server {
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 15 * time.Second
	}

	s := &Server{cfg: cfg}
	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.buildHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// Handler returns the wrapped http.Handler. Exposed so tests can drive
// the full middleware stack without binding a port.
func (s *Server) Handler() http.Handler {
	return s.http.Handler
}

// Start begins serving on the configured address. It blocks until
// Shutdown is called or the listener errors. http.ErrServerClosed
// is reported as a clean shutdown, not an error.
func (s *Server) Start() error {
	s.cfg.Logger.Info("fishhawkd listening", slog.String("addr", s.cfg.Addr))
	if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen: %w", err)
	}
	return nil
}

// Shutdown gracefully drains in-flight requests, capped by
// ShutdownTimeout from the parent context.
func (s *Server) Shutdown(ctx context.Context) error {
	shutdownCtx, cancel := context.WithTimeout(ctx, s.cfg.ShutdownTimeout)
	defer cancel()
	return s.http.Shutdown(shutdownCtx)
}

// buildHandler wires the route mux and the middleware chain.
//
// Middleware order, outermost first:
//
//	recovery → requestID → logging → authStub → mux
//
// Recovery is outermost so panics in any later layer become 500s.
// Request ID is set before logging so log lines can carry it. Auth
// runs after logging so the request is logged even if auth rejects.
func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()
	s.registerRoutes(mux)

	var h http.Handler = mux
	h = authStub(h)
	h = logging(s.cfg.Logger)(h)
	h = requestID(h)
	h = recovery(s.cfg.Logger)(h)
	return h
}
