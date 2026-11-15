package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// GET /readyz is the readiness probe (#5224 G7). /healthz answers liveness
// only and never touches the database, so a daemon whose database is down
// keeps answering 200 there and a load balancer keeps routing traffic to it.
// /readyz adds the one dependency every repository route needs: a bounded
// database ping.
//
// The route is public, like /healthz, so the probe is built to be cheap
// under anonymous load. A cache miss starts at most ONE ping, every
// concurrent caller waits on that same ping, and its result (success or
// failure) is served from a cache for readyzCacheTTL. Readiness therefore
// holds at most one pool connection at a time, however many requests arrive.

const (
	// readyzCacheTTL is how long one ping's result answers every /readyz
	// request, success and failure alike.
	readyzCacheTTL = time.Second
	// readyzPingTimeout bounds the shared ping. It runs on a context
	// detached from any single request, so this is its only deadline.
	readyzPingTimeout = 2 * time.Second
)

// readinessState is the database check's closed vocabulary. It is both the
// wire value of checks.database and the class recorded in the ping-failure
// log line, which never carries the raw driver error (it can name the
// database host).
type readinessState string

const (
	readinessOK            readinessState = "ok"
	readinessUnreachable   readinessState = "unreachable"
	readinessTimeout       readinessState = "timeout"
	readinessNotConfigured readinessState = "not_configured"
)

// wire maps a state to the response value. A timed-out ping is reported as
// unreachable on the wire; only the log line distinguishes the two.
func (st readinessState) wire() readinessState {
	if st == readinessTimeout {
		return readinessUnreachable
	}
	return st
}

// readinessCall is one in-flight ping. result is written before done is
// closed, so a waiter that received from done reads it race-free.
type readinessCall struct {
	done   chan struct{}
	result readinessState
}

// readinessProbe coalesces and caches the /readyz database ping.
type readinessProbe struct {
	ping    func(context.Context) error
	now     func() time.Time
	ttl     time.Duration
	timeout time.Duration
	logger  *slog.Logger

	notConfiguredOnce sync.Once

	mu       sync.Mutex
	inflight *readinessCall
	cached   readinessState
	cachedAt time.Time
	hasCache bool
}

func newReadinessProbe(ping func(context.Context) error, now func() time.Time, logger *slog.Logger) *readinessProbe {
	return &readinessProbe{
		ping:    ping,
		now:     now,
		ttl:     readyzCacheTTL,
		timeout: readyzPingTimeout,
		logger:  logger,
	}
}

// check answers the database state. A fresh cached result is returned
// without a ping. Otherwise the caller joins the in-flight ping, starting it
// when none is running, and waits for it or for its own context.
func (p *readinessProbe) check(ctx context.Context) readinessState {
	if p == nil {
		return readinessNotConfigured
	}
	if p.ping == nil {
		p.notConfiguredOnce.Do(func() {
			p.logger.LogAttrs(ctx, slog.LevelWarn, "readiness probe: database check failed",
				slog.String("class", string(readinessNotConfigured)))
		})
		return readinessNotConfigured
	}
	p.mu.Lock()
	if p.hasCache && p.now().Sub(p.cachedAt) < p.ttl {
		st := p.cached
		p.mu.Unlock()
		return st
	}
	call := p.inflight
	if call == nil {
		call = &readinessCall{done: make(chan struct{})}
		p.inflight = call
		// Detached from this request: one client's disconnect must not
		// cancel the ping every other waiter is sharing.
		go p.run(context.WithoutCancel(ctx), call)
	}
	p.mu.Unlock()
	select {
	case <-call.done:
		return call.result
	case <-ctx.Done():
		// This caller gave up; the shared ping keeps running for the rest.
		return readinessUnreachable
	}
}

// run performs the one shared ping, caches its result and releases waiters.
func (p *readinessProbe) run(ctx context.Context, call *readinessCall) {
	pingCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	st := readinessOK
	if err := p.ping(pingCtx); err != nil {
		st = readinessUnreachable
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(pingCtx.Err(), context.DeadlineExceeded) {
			st = readinessTimeout
		}
		// The class only: a pgx error string can carry the database host.
		p.logger.LogAttrs(ctx, slog.LevelWarn, "readiness probe: database check failed",
			slog.String("class", string(st)))
	}
	p.mu.Lock()
	p.cached, p.cachedAt, p.hasCache = st, p.now(), true
	p.inflight = nil
	p.mu.Unlock()
	call.result = st
	close(call.done)
}

// readinessResponse is the /readyz body for both 200 and 503.
type readinessResponse struct {
	Status string          `json:"status"`
	Checks readinessChecks `json:"checks"`
}

type readinessChecks struct {
	Database readinessState `json:"database"`
}

// handleReady answers GET /readyz: 200 when the database ping succeeds, 503
// when the database is unreachable, the ping timed out, or no database is
// configured (every repository route already answers 503 in that posture).
// The driver error is never echoed.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	db := s.readiness.check(r.Context()).wire()
	resp := readinessResponse{Status: "ok", Checks: readinessChecks{Database: db}}
	code := http.StatusOK
	if db != readinessOK {
		resp.Status = "unavailable"
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelError, "encode readiness response",
			slog.String("error", err.Error()),
		)
	}
}
