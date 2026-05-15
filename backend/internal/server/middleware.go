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
	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mcptoken"
)

// apitokenAuthenticator is the slice of apitoken.Repository
// bearerAuth uses. Defining the interface here lets tests inject
// a stub directly without pulling in the whole repository.
type apitokenAuthenticator interface {
	Authenticate(ctx context.Context, plaintext string) (*apitoken.Token, error)
}

// mcptokenAuthenticator is the runner-side counterpart to
// apitokenAuthenticator (E19.8 / #348). The middleware routes by
// prefix — `fhm_` to this; `fhk_` to apitokenAuthenticator —
// so the two interfaces never conflict on a single token string.
type mcptokenAuthenticator interface {
	Authenticate(ctx context.Context, plaintext string) (*mcptoken.Token, error)
}

// sessionAuthenticator is the slice of auth.Repository the
// resolver uses for browser cookie-backed sessions. Same test-seam
// convention as apitokenAuthenticator.
type sessionAuthenticator interface {
	Authenticate(ctx context.Context, plaintext string) (*auth.User, *auth.Session, error)
}

// ctxKey is unexported so callers must use the accessors below to
// pull values out of a request context.
type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyIdentity
)

// Identity is the authenticated principal for a request. Subject
// is the only field every code path can rely on. The other fields
// vary by auth source:
//
//   - Bearer-token (CLI) flow: TokenID + Scopes are set; UserID +
//     SessionID stay empty.
//   - Cookie session (browser, E4.2) flow: UserID + SessionID
//     are set; Subject is "github:<login>"; TokenID + Scopes
//     stay empty.
//
// Subject "anonymous" means no auth credential was presented (or
// the presented one didn't validate). Handlers that require an
// authenticated user check for that value and return 401.
type Identity struct {
	Subject   string
	TokenID   string
	Scopes    []string
	UserID    string
	SessionID string
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

// bearerAuth resolves either a session cookie (browser flow,
// E4.2) or an Authorization: Bearer <fhk_...> token (CLI flow,
// E4.5) to an Identity. Tries the cookie first — if a browser is
// somehow carrying both, the cookie wins because it's bound to
// the user's GitHub identity rather than a long-lived secret.
// Absent / invalid credentials fall through to the anonymous
// identity; the middleware does NOT 401 on its own. Per-handler
// logic decides whether anonymous is acceptable.
//
// Either repo may be nil — the bootstrap path can run without
// either backend, in which case the corresponding credential
// never resolves.
func bearerAuth(tokens apitokenAuthenticator, mcpTokens mcptokenAuthenticator, sessions sessionAuthenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := Identity{Subject: "anonymous"}

			// Cookie session first. Tied to a real GitHub user,
			// so handlers that index on Subject get a stable
			// "github:<login>" value.
			if sessions != nil {
				if c, err := r.Cookie(auth.SessionCookieName); err == nil && c.Value != "" {
					if user, sess, err := sessions.Authenticate(r.Context(), c.Value); err == nil {
						id = Identity{
							Subject:   "github:" + user.GitHubLogin,
							UserID:    user.ID,
							SessionID: sess.ID,
						}
					}
				}
			}

			// Bearer token, only if no session resolved. Routes by
			// prefix: `fhm_` to the MCP authenticator, `fhk_` (or
			// anything else) to the apitoken authenticator. The
			// prefix check is cheap (string compare, no allocation)
			// so the routing decision doesn't cost a DB round-trip.
			if id.IsAnonymous() {
				if tok, ok := tokenFromHeader(r); ok {
					switch {
					case mcpTokens != nil && mcptoken.HasPrefix(tok):
						if rec, err := mcpTokens.Authenticate(r.Context(), tok); err == nil {
							id = Identity{
								// Subject encodes the run scope so
								// handlers that audit auth or
								// enforce per-run access can read
								// it directly. Format mirrors the
								// existing "github:<login>" /
								// "service:<name>" convention.
								Subject: "mcp:run:" + rec.RunID.String(),
								TokenID: rec.ID.String(),
								// All MCP tokens carry one
								// informational scope. Future per-
								// endpoint enforcement reads this.
								Scopes: []string{"mcp:read"},
							}
						}
					case tokens != nil:
						if rec, err := tokens.Authenticate(r.Context(), tok); err == nil {
							id = Identity{
								Subject: rec.Subject,
								TokenID: rec.ID.String(),
								Scopes:  append([]string(nil), rec.Scopes...),
							}
						}
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
