package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
	"github.com/kuhlman-labs/fishhawk/backend/internal/dberr"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mcptoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
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

	// AccountID is the workspace account the session's membership
	// gate resolved at sign-in (E44.3). Set only on the cookie path,
	// from the sessions row; empty for bearer-token identities (their
	// account enforcement is E44.5) and for sessions whose account
	// binding is gone (deleted account → /v0/auth/me denies).
	AccountID string

	// AuthMethod records how a bearer api_token was authenticated at
	// issue time: "static" for operator-minted tokens, "oauth" for tokens
	// minted through the OAuth device flow (E39.3 / #1708). Populated only
	// on the api_token bearer path from the authenticated token's
	// auth_method; empty for cookie-session and MCP-token identities. The
	// approval audit records it (approvals.go) so a decision's provenance
	// includes which credential kind acted.
	AuthMethod string
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

// requireWriteScope checks that the caller is authenticated and holds
// scope. Returns true when the caller may proceed. Returns false and
// writes a 401 (anonymous caller) or 403 (authenticated but missing
// scope) response when the check fails. Cookie-session callers
// (TokenID == "") bypass scope enforcement — they authenticate via
// GitHub OAuth and carry no explicit scope list. Despite the name the
// check is scope-agnostic: the read:audit-export export gate
// (E9.5 / #1608) enforces through it too.
func (s *Server) requireWriteScope(w http.ResponseWriter, r *http.Request, scope string) bool {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, 401, "authentication_required",
			"an authenticated token is required", nil)
		return false
	}
	if id.TokenID != "" && !hasScope(id, scope) {
		s.writeError(w, r, 403, "insufficient_scope",
			"token is missing required scope: "+scope,
			map[string]any{"required_scope": scope})
		return false
	}
	return true
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
// The one exception is a database-UNAVAILABLE condition (#764): if
// the authenticator for the credential actually presented fails
// because the database is unreachable (dberr.IsUnavailable), the
// middleware short-circuits with 503 service_unavailable instead of
// falling through to anonymous. Falling through would mask an outage
// as a per-handler 401 — telling the caller their credential is bad
// when in fact the lookup never ran. An ordinary bad-credential
// error (no matching row) is NOT unavailable, so it still falls
// through and the per-handler 401 is preserved when the DB is healthy.
//
// It is a Server method (not a free function) so it can reach
// s.writeError to emit the standard error envelope on the 503 path.
//
// Either repo may be nil — the bootstrap path can run without
// either backend, in which case the corresponding credential
// never resolves.
func (s *Server) bearerAuth(tokens apitokenAuthenticator, mcpTokens mcptokenAuthenticator, sessions sessionAuthenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := Identity{Subject: "anonymous"}

			// Cookie session first. Tied to a real GitHub user,
			// so handlers that index on Subject get a stable
			// "github:<login>" value.
			if sessions != nil {
				if c, err := r.Cookie(auth.SessionCookieName); err == nil && c.Value != "" {
					user, sess, err := sessions.Authenticate(r.Context(), c.Value)
					if dberr.IsUnavailable(err) {
						s.writeDBUnavailable(w, r)
						return
					}
					if err == nil {
						id = Identity{
							Subject:   "github:" + user.GitHubLogin,
							UserID:    user.ID,
							SessionID: sess.ID,
							AccountID: sess.AccountID,
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
						rec, err := mcpTokens.Authenticate(r.Context(), tok)
						if dberr.IsUnavailable(err) {
							s.writeDBUnavailable(w, r)
							return
						}
						if err == nil {
							id = Identity{
								// Subject encodes the run scope so
								// handlers that audit auth or
								// enforce per-run access can read
								// it directly. Format mirrors the
								// existing "github:<login>" /
								// "service:<name>" convention.
								Subject: "mcp:run:" + rec.RunID.String(),
								TokenID: rec.ID.String(),
								Scopes:  append([]string(nil), rec.Scopes...),
							}
							// Populate AccountID from the token's own run
							// (ADR-057 / E44.5): an mcp:run token acts within
							// its run's tenant account, so the ownership
							// middleware bounds it exactly as a bearer token
							// bound to that account. The lookup is
							// UNCONDITIONAL: run.AccountGetter is a REQUIRED
							// run.Repository method (E44.11 / #2074), so no
							// wiring gap can produce an accountless mcp identity
							// — the branch that used to skip resolution when a
							// repo didn't implement the capability is gone by
							// construction. The
							// untenanted-run happy path is GetRunAccountID
							// returning "" with NO error → empty AccountID
							// (allowed). Any lookup ERROR fails CLOSED with 503:
							// a run-scoped token that cannot resolve its own
							// run's account must never fall through to a
							// resolved accountless identity, which
							// accountVisiblePage/handleListRuns would promote to
							// the global operator view (a run-scoped token
							// escalated to global read). A DB-unavailable error
							// is the same 503; an ordinary error mirrors it via
							// writeDBUnavailable rather than proceeding.
							if s.cfg.RunRepo == nil {
								// An UNCONFIGURED run repo cannot resolve the
								// account either, and the deleted type
								// assertion used to absorb it into the
								// accountless-identity fall-through. Same
								// fail-CLOSED posture as a lookup error.
								s.writeDBUnavailable(w, r)
								return
							}
							acct, aerr := s.cfg.RunRepo.GetRunAccountID(r.Context(), rec.RunID)
							if aerr != nil {
								s.writeDBUnavailable(w, r)
								return
							}
							id.AccountID = acct
						}
					case tokens != nil:
						rec, err := tokens.Authenticate(r.Context(), tok)
						if dberr.IsUnavailable(err) {
							s.writeDBUnavailable(w, r)
							return
						}
						if err == nil {
							id = Identity{
								Subject:    rec.Subject,
								TokenID:    rec.ID.String(),
								Scopes:     append([]string(nil), rec.Scopes...),
								AuthMethod: rec.AuthMethod,
								// The token's own tenant account (ADR-057 /
								// E44.5): the ownership middleware bounds a
								// bearer request to it. Empty for untenanted /
								// operator tokens (NULL account_id).
								AccountID: rec.AccountID,
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

// writeDBUnavailable emits the 503 service_unavailable envelope used
// when an auth lookup fails because the database is unreachable
// (#764). Centralized so the three credential paths in bearerAuth
// stay identical.
func (s *Server) writeDBUnavailable(w http.ResponseWriter, r *http.Request) {
	s.writeError(w, r, http.StatusServiceUnavailable, "service_unavailable",
		"database unavailable; retry shortly", nil)
}

// --- Account-ownership authorization (ADR-057 / E44.5, #1829) ---

// AccountRoles resolves a cookie-session caller's role within its tenant
// account. Satisfied by *account.Store (accountdb-backed) in production and a
// fake in tests. Kept as an interface here so the middleware never imports the
// account package directly.
type AccountRoles interface {
	MemberRole(ctx context.Context, accountID, provider, subject string) (string, error)
}

// accountTier classifies a run-scoped route for centralized account
// enforcement. Every tier carries the OWNERSHIP check (a tenanted run may be
// touched only by its own account); the two write tiers additionally carry
// cookie ROLE-BOUNDING. adminWrite is the destructive/admin surface (an admin
// role is required); memberWrite is the operator-decision surface (member or
// admin); readAccess carries ownership only, so a resolved cookie reading an
// untenanted run stays allowed.
type accountTier int

const (
	readAccess accountTier = iota
	memberWrite
	adminWrite
)

// providerFromSubject extracts the forge discriminator from an identity
// subject: "github:<login>" → "github", "gitlab:<user>" → "gitlab". A subject
// with no ":" is returned verbatim.
func providerFromSubject(subject string) string {
	if i := strings.IndexByte(subject, ':'); i >= 0 {
		return subject[:i]
	}
	return subject
}

// enforceAccount applies the ownership + (write-tier) cookie role-bounding
// checks for an already-resolved run. Returns true when the request may
// proceed; on denial it writes the error envelope and returns false.
//
// (a) OWNERSHIP (all tiers): a tenanted run (AccountID != "") whose account
// disagrees with the caller's Identity.AccountID → 403 account_forbidden. An
// untenanted run (AccountID == "") is allowed — the NULL-allow window #1830
// closes once every row is populated.
//
// (b) COOKIE ROLE-BOUNDING (write tiers only, resolved OAuth cookie only —
// SessionID != "" && TokenID == ""): an empty AccountID on a write is 403
// account_unresolved (a pre-gate / de-tenanted session must not write). With a
// role provider wired, an adminWrite tier requires the admin role (else 403
// insufficient_role); memberWrite admits member/admin/NULL-role. Bearer and mcp
// identities carry a TokenID, so role-bounding never fires for them — they are
// bounded by ownership alone. A nil AccountRoles is the untenanted-allow
// posture: role-bounding is skipped (ownership still applies).
func (s *Server) enforceAccount(w http.ResponseWriter, r *http.Request, tier accountTier, runRow *run.Run) bool {
	id := IdentityFrom(r.Context())

	// (a) Ownership.
	if runRow.AccountID != "" && id.AccountID != runRow.AccountID {
		s.writeError(w, r, http.StatusForbidden, "account_forbidden",
			"this run belongs to a different workspace account", nil)
		return false
	}

	// (b) Cookie role-bounding, write tiers only.
	if tier == readAccess {
		return true
	}
	if id.SessionID == "" || id.TokenID != "" {
		// Not a resolved OAuth cookie (bearer / mcp / anonymous): ownership
		// alone governs.
		return true
	}
	if id.AccountID == "" {
		s.writeError(w, r, http.StatusForbidden, "account_unresolved",
			"session is not bound to a workspace account; sign in again", nil)
		return false
	}
	if s.cfg.AccountRoles == nil {
		// No role provider wired (untenanted-allow): skip role-bounding.
		return true
	}
	role, err := s.cfg.AccountRoles.MemberRole(r.Context(), id.AccountID,
		providerFromSubject(id.Subject), id.Subject)
	if err != nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "service_unavailable",
			"could not resolve workspace account role; retry shortly", nil)
		return false
	}
	if tier == adminWrite && role != account.RoleAdmin {
		s.writeError(w, r, http.StatusForbidden, "insufficient_role",
			"this action requires the admin role in the workspace account", nil)
		return false
	}
	return true
}

// requireRunAccount wraps a run-scoped handler ({run_id} in the path) with the
// tiered account enforcement. It resolves the run WITH its account and enforces
// before calling next. When the run can't be resolved (no repo, bad UUID, not
// found, load error) it FALLS THROUGH to next unchanged, so the handler
// produces its own 503/400/404 — the wrapper never alters a request's
// non-authz error surface.
func (s *Server) requireRunAccount(tier accountTier, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runRow, ok := s.resolveRunForAuthz(r, r.PathValue("run_id"))
		if ok && !s.enforceAccount(w, r, tier, runRow) {
			return
		}
		next(w, r)
	}
}

// requireStageAccount wraps a stage-scoped handler ({stage_id}): stage_id ->
// stage.RunID -> run -> account. Same fall-through-on-unresolvable contract as
// requireRunAccount.
func (s *Server) requireStageAccount(tier accountTier, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runRow, ok := s.resolveRunForStage(r, r.PathValue("stage_id"))
		if ok && !s.enforceAccount(w, r, tier, runRow) {
			return
		}
		next(w, r)
	}
}

// requireConcernAccount wraps a concern-scoped handler ({concern_id}):
// concern_id -> concern.RunID -> run -> account. Same fall-through contract.
func (s *Server) requireConcernAccount(tier accountTier, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runRow, ok := s.resolveRunForConcern(r, r.PathValue("concern_id"))
		if ok && !s.enforceAccount(w, r, tier, runRow) {
			return
		}
		next(w, r)
	}
}

// resolveRunForAuthz loads the run named by rawRunID WITH its account. ok=false
// (fall through to the handler) when no run repo is wired, the id is not a
// UUID, or the load fails/404s — the wrapper must not change those surfaces.
func (s *Server) resolveRunForAuthz(r *http.Request, rawRunID string) (*run.Run, bool) {
	if s.cfg.RunRepo == nil {
		return nil, false
	}
	id, err := uuid.Parse(rawRunID)
	if err != nil {
		return nil, false
	}
	rn, err := s.cfg.RunRepo.GetRun(r.Context(), id)
	if err != nil {
		return nil, false
	}
	return rn, true
}

// resolveRunForStage resolves stage_id -> stage.RunID -> run. ok=false on any
// unresolvable step (fall through).
func (s *Server) resolveRunForStage(r *http.Request, rawStageID string) (*run.Run, bool) {
	if s.cfg.RunRepo == nil {
		return nil, false
	}
	sid, err := uuid.Parse(rawStageID)
	if err != nil {
		return nil, false
	}
	st, err := s.cfg.RunRepo.GetStage(r.Context(), sid)
	if err != nil {
		return nil, false
	}
	rn, err := s.cfg.RunRepo.GetRun(r.Context(), st.RunID)
	if err != nil {
		return nil, false
	}
	return rn, true
}

// resolveRunForConcern resolves concern_id -> concern.RunID -> run. ok=false on
// any unresolvable step (no concern/run repo, bad UUID, not found).
func (s *Server) resolveRunForConcern(r *http.Request, rawConcernID string) (*run.Run, bool) {
	if s.cfg.ConcernRepo == nil || s.cfg.RunRepo == nil {
		return nil, false
	}
	cid, err := uuid.Parse(rawConcernID)
	if err != nil {
		return nil, false
	}
	cs, err := s.cfg.ConcernRepo.GetByIDs(r.Context(), []uuid.UUID{cid})
	if err != nil || len(cs) == 0 {
		return nil, false
	}
	rn, err := s.cfg.RunRepo.GetRun(r.Context(), cs[0].RunID)
	if err != nil {
		return nil, false
	}
	return rn, true
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
