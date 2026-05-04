package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
)

// apitokenAuthenticator is the slice of apitoken.Repository
// bearerAuth uses. Defining the interface here lets tests inject
// a stub directly without pulling in the whole repository.
type apitokenAuthenticator interface {
	Authenticate(ctx context.Context, plaintext string) (*apitoken.Token, error)
}

// ctxKey is unexported so callers must use the accessors below to
// pull values out of a request context.
type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyIdentity
)

// Identity is the authenticated principal for a request. Subject
// is the only field every code path can rely on; TokenID + Scopes
// are populated when the request authenticated via an API token,
// and the OAuth-session path (E4.2) will fill TokenID with the
// session id once it lands.
//
// Subject "anonymous" means no auth header was presented (or the
// presented credential didn't validate). Handlers that require an
// authenticated user check for that value and return 401.
type Identity struct {
	Subject string
	TokenID string
	Scopes  []string
}

// IsAnonymous reports whether i represents an unauthenticated
// caller. Equivalent to i.Subject == "" || i.Subject == "anonymous"
// — wrapping the check so every handler agrees on the convention.
func (i Identity) IsAnonymous() bool {
	return i.Subject == "" || i.Subject == "anonymous"
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

// bearerAuth resolves Authorization: Bearer <fhk_...> tokens to an
// Identity via the configured apitoken.Repository. Tokens that
// don't match any active row, or whose strings don't have the
// product prefix, fall through to the anonymous identity — the
// middleware does NOT 401 on its own. Per-handler logic decides
// whether anonymous is acceptable; this keeps the trace upload +
// signing-key endpoints (which use their own auth schemes) from
// being collateral-damaged.
//
// repo is allowed to be nil — the bootstrap path can run without a
// DB-backed token store, in which case every request is anonymous.
//
// The OAuth session path (E4.2) will layer on top of this: a
// session cookie also resolves to a non-anonymous Identity. Both
// auth methods set the same Identity shape so handlers don't need
// to know which one ran.
func bearerAuth(repo apitokenAuthenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := Identity{Subject: "anonymous"}
			if tok, ok := tokenFromHeader(r); ok && repo != nil {
				if rec, err := repo.Authenticate(r.Context(), tok); err == nil {
					id = Identity{
						Subject: rec.Subject,
						TokenID: rec.ID.String(),
						Scopes:  append([]string(nil), rec.Scopes...),
					}
				}
			}
			ctx := context.WithValue(r.Context(), ctxKeyIdentity, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// tokenFromHeader extracts a Fishhawk bearer token from the
// Authorization header. Returns ("", false) when no Bearer header
// is present or the scheme isn't "Bearer". Token shape (prefix,
// length) is the Authenticate path's job to validate.
func tokenFromHeader(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	return h[len(prefix):], true
}
