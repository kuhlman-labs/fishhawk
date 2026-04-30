package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"
)

// ctxKey is unexported so callers must use the accessors below to
// pull values out of a request context.
type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyIdentity
)

// Identity is the authenticated principal for a request.
//
// Until E4 (#4) lands real auth, the authStub always sets
// Identity{Subject: "anonymous"}. Downstream code that switches on
// Subject must tolerate that value.
type Identity struct {
	Subject string
}

// RequestIDFrom returns the request ID set by the requestID middleware,
// or "" if the middleware did not run (e.g., direct handler tests).
func RequestIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyRequestID).(string)
	return v
}

// IdentityFrom returns the Identity set by the auth middleware. The
// zero value is returned if no auth middleware ran.
func IdentityFrom(ctx context.Context) Identity {
	v, _ := ctx.Value(ctxKeyIdentity).(Identity)
	return v
}

const requestIDMaxLen = 64

// requestID puts a per-request ID into the context and the
// X-Request-ID response header. A client-supplied X-Request-ID is
// honored if it's a non-empty string within length bounds; otherwise
// we generate 24 hex chars from crypto/rand.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" || len(id) > requestIDMaxLen {
			id = newRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func newRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read on Linux/macOS does not fail in practice; if
		// it does, return a constant rather than panic. Logging
		// middleware will surface the duplicate IDs, and the request
		// still completes.
		return "rngfail"
	}
	return hex.EncodeToString(b[:])
}

// statusRecorder captures the response status for the logging
// middleware. It assumes WriteHeader is called before any Write; if
// Write is called first, the recorded status stays at 200, which is
// what net/http would also report.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// logging emits one structured log line per request after the handler
// returns. Fields: method, path, status, duration_ms, request_id.
func logging(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			logger.LogAttrs(r.Context(), slog.LevelInfo, "request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
				slog.String("request_id", RequestIDFrom(r.Context())),
			)
		})
	}
}

// recovery turns panics into 500 responses and an error log line.
// A panic that has already produced any response bytes can't be
// converted to a clean 500; the response will be whatever was already
// written, plus a connection close.
func recovery(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				logger.LogAttrs(r.Context(), slog.LevelError, "panic",
					slog.Any("recovered", rec),
					slog.String("request_id", RequestIDFrom(r.Context())),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// authStub is a placeholder until E4 (#4) lands real authentication.
// It tags every request with an anonymous Identity so downstream code
// can be written assuming an Identity is always present in context.
func authStub(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), ctxKeyIdentity, Identity{Subject: "anonymous"})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
