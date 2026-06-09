package main

// http_transport.go adds the opt-in, localhost-only streamable-HTTP
// transport (ADR-033 option b / #927) alongside the stdio default.
//
// Security posture — this is a SINGLE-OPERATOR local shared endpoint,
// NOT a multi-tenant remote server (hosted/remote is #655):
//
//   - The listener is hard-pinned to a loopback address. A non-loopback
//     --addr fails fast (validateLoopbackAddr) before any bind, and a
//     hostname is rejected unless EVERY resolved IP is loopback, so a
//     'localhost' aliased to a routable address can't slip through.
//   - Every request must carry Authorization: Bearer <FISHHAWK_API_TOKEN>,
//     compared in constant time. Loopback is explicitly NOT treated as a
//     trust boundary — co-tenant local processes still need the token.
//
// The go-sdk's own DNS-rebinding protection (rejecting a non-loopback
// Host header) stays on by default; our loopback bind + bearer gate are
// independent of it.

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// httpShutdownTimeout bounds the graceful Shutdown on ctx cancellation
// so a wedged in-flight request can't hang the listener close forever.
const httpShutdownTimeout = 5 * time.Second

// validateLoopbackAddr returns a normalized host:port that is guaranteed
// to bind only to a loopback interface, or an error naming the offending
// host and the loopback-only constraint.
//
// An empty/missing host is clamped to 127.0.0.1 (so ":8765" binds
// loopback, not all interfaces). A literal IP is checked with
// net.IP.IsLoopback (authoritative for 127.0.0.0/8 and ::1). A hostname
// is resolved via lookupIP and accepted only when EVERY resolved address
// is loopback — a host aliased to a routable IP is rejected rather than
// silently binding off-loopback.
func validateLoopbackAddr(addr string, lookupIP func(host string) ([]net.IP, error)) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("invalid --addr %q: %w", addr, err)
	}
	if host == "" {
		host = "127.0.0.1"
	}

	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return "", fmt.Errorf("--addr host %q is not a loopback address; the HTTP transport binds loopback only (single-operator local endpoint, not remote — see #655)", host)
		}
		return net.JoinHostPort(host, port), nil
	}

	ips, err := lookupIP(host)
	if err != nil {
		return "", fmt.Errorf("--addr host %q could not be resolved: %w", host, err)
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("--addr host %q resolved to no addresses", host)
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return "", fmt.Errorf("--addr host %q resolves to non-loopback address %s; the HTTP transport binds loopback only (single-operator local endpoint, not remote — see #655)", host, ip)
		}
	}
	return net.JoinHostPort(host, port), nil
}

// bearerAuthMiddleware requires Authorization: Bearer <token> on every
// request, comparing with crypto/subtle.ConstantTimeCompare to avoid a
// timing side channel. A missing, malformed, or mismatched header gets a
// 401 with WWW-Authenticate: Bearer and a terse body that never echoes
// the expected token.
func bearerAuthMiddleware(next http.Handler, token string) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		got := r.Header.Get("Authorization")
		if len(got) <= len(prefix) || got[:len(prefix)] != prefix {
			unauthorized(w)
			return
		}
		if subtle.ConstantTimeCompare([]byte(got[len(prefix):]), want) != 1 {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, "missing or invalid bearer token", http.StatusUnauthorized)
}

// serveHTTP validates addr as loopback-only, builds the streamable-HTTP
// handler (constructing + registering a server per session via
// newServer, reusing the identical tool-registration path as stdio),
// wraps it in the bearer gate, and serves it on an http.Server bound to
// the validated loopback addr. It honors ctx cancellation with a bounded
// graceful Shutdown so the listener closes cleanly.
func serveHTTP(ctx context.Context, addr, token string, newServer func() *mcp.Server) error {
	bound, err := validateLoopbackAddr(addr, net.LookupIP)
	if err != nil {
		return err
	}

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return newServer()
	}, nil)

	srv := &http.Server{
		Addr:    bound,
		Handler: bearerAuthMiddleware(handler, token),
	}

	ln, err := net.Listen("tcp", bound)
	if err != nil {
		return fmt.Errorf("bind %s: %w", bound, err)
	}

	errc := make(chan error, 1)
	go func() {
		errc <- srv.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
