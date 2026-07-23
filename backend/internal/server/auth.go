package server

import (
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
)

// userResponse mirrors the OpenAPI `User` schema. Surfaces only
// what the SPA / CLI needs. AccountID is the workspace account the
// session's membership gate resolved (E44.3); null for bearer-token
// identities.
type userResponse struct {
	ID          string  `json:"id"`
	GitHubLogin string  `json:"github_login"`
	Name        string  `json:"name"`
	Email       *string `json:"email"`
	AccountID   *string `json:"account_id"`
}

// handleGitHubLogin implements GET /v0/auth/github/login. Mints a
// state value, stores it in a short-lived browser cookie, and
// redirects to GitHub's authorize URL.
//
// Optionally accepts ?next=<relative-path>. When a valid relative
// path is supplied, it's stored in fishhawk_oauth_next so the
// callback can route the user back to the page they originally
// asked for (E7.2.1 #153). Anything that fails the open-redirect
// validation is dropped silently — the configured default applies
// instead.
func (s *Server) handleGitHubLogin(w http.ResponseWriter, r *http.Request) {
	if s.cfg.GitHubOAuth == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "oauth_unconfigured",
			"GitHub OAuth not configured", nil)
		return
	}
	state, err := auth.GenerateState()
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not generate OAuth state", map[string]any{"error": err.Error()})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.StateCookieName,
		Value:    state,
		Path:     "/v0/auth/github/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(auth.StateCookieTTL),
		MaxAge:   int(auth.StateCookieTTL.Seconds()),
	})

	if next := r.URL.Query().Get("next"); next != "" && isSafeRelativeRedirect(next) {
		http.SetCookie(w, &http.Cookie{
			Name:     auth.NextCookieName,
			Value:    next,
			Path:     "/v0/auth/github/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
			Expires:  time.Now().Add(auth.StateCookieTTL),
			MaxAge:   int(auth.StateCookieTTL.Seconds()),
		})
	}

	http.Redirect(w, r, s.cfg.GitHubOAuth.AuthorizeURL(state), http.StatusFound)
}

// handleGitHubCallback implements GET /v0/auth/github/callback.
// Verifies state, exchanges code, fetches the GitHub profile,
// upserts the user + creates a session, sets the session cookie,
// and redirects to the SPA.
func (s *Server) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	if s.cfg.GitHubOAuth == nil || s.cfg.AuthRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "oauth_unconfigured",
			"GitHub OAuth not configured", nil)
		return
	}

	stateParam := r.URL.Query().Get("state")
	stateCookie, err := r.Cookie(auth.StateCookieName)
	if err != nil || stateParam == "" || stateCookie.Value != stateParam {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"OAuth state did not validate", nil)
		return
	}
	// Single-use: clear the state cookie.
	http.SetCookie(w, &http.Cookie{
		Name:     auth.StateCookieName,
		Value:    "",
		Path:     "/v0/auth/github/",
		HttpOnly: true,
		Secure:   true,
		MaxAge:   -1,
	})

	// E7.2.1: read the post-login redirect target the SPA stashed
	// at /login time. Re-validate (defense in depth) and clear; if
	// missing or invalid, the configured default applies.
	var nextRedirect string
	if c, err := r.Cookie(auth.NextCookieName); err == nil && c.Value != "" {
		if isSafeRelativeRedirect(c.Value) {
			nextRedirect = c.Value
		}
		http.SetCookie(w, &http.Cookie{
			Name:     auth.NextCookieName,
			Value:    "",
			Path:     "/v0/auth/github/",
			HttpOnly: true,
			Secure:   true,
			MaxAge:   -1,
		})
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"missing code parameter", nil)
		return
	}

	accessToken, err := s.cfg.GitHubOAuth.ExchangeCode(r.Context(), code)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "oauth_exchange_failed",
			"GitHub rejected the authorization code",
			map[string]any{"error": err.Error()})
		return
	}
	profile, err := s.cfg.GitHubOAuth.FetchProfile(r.Context(), accessToken)
	if err != nil {
		s.writeError(w, r, http.StatusBadGateway, "oauth_profile_fetch_failed",
			"could not fetch GitHub profile",
			map[string]any{"error": err.Error()})
		return
	}

	// Workspace-membership gate (E44.3 / ADR-057 Amendment A2): the
	// profile fetch succeeding is NOT admission. Resolve which
	// account(s) admit this user — invited account_members rows
	// DB-only, auto-join via the live org-list bootstrap — BEFORE any
	// session exists. Fail closed on every branch: no resolver and no
	// match both deny with no session cookie.
	if s.cfg.AuthMembership == nil {
		s.cfg.Logger.Warn("oauth sign-in denied: no membership resolver configured",
			"github_login", profile.Login)
		http.Redirect(w, r, s.accessDeniedRedirect(), http.StatusFound)
		return
	}
	accountIDs, err := s.cfg.AuthMembership.ResolveAccounts(r.Context(), "github", accessToken, *profile)
	if err != nil {
		s.writeError(w, r, http.StatusBadGateway, "membership_resolution_failed",
			"could not resolve workspace membership; sign-in denied",
			map[string]any{"error": err.Error()})
		return
	}
	if len(accountIDs) == 0 {
		s.cfg.Logger.Info("oauth sign-in denied: no admitting account",
			"github_login", profile.Login)
		http.Redirect(w, r, s.accessDeniedRedirect(), http.StatusFound)
		return
	}
	// Deterministic-first: the resolver returns a sorted set; a
	// multi-account picker is out of scope for v0.
	accountID := accountIDs[0]

	user, sess, err := s.cfg.AuthRepo.SignIn(r.Context(), *profile, accountID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"sign-in failed", map[string]any{"error": err.Error()})
		return
	}

	// Login-time mirror purge (ADR-057 Amendment A2, E44.10 / #2071). Drop
	// every mirrored repo permission for this subject so the first read after
	// sign-in re-resolves from the forge. Signing in is the one moment we know
	// the user is present and expects their current access to apply, so it is
	// the cheapest place to collapse the staleness window to zero.
	//
	// NON-FATAL, deliberately. Note precisely what a failed purge does and
	// does not cost: the previously cached entries — INCLUDING GRANTS —
	// survive until their TTL expires. It is NOT true that a failed purge
	// merely leaves the mirror needing a re-resolve. What IS true is that the
	// exposure is exactly the baseline this design already accepts everywhere
	// else: any permission revoked on the forge mid-TTL stays visible until
	// the entry expires. So the failure is BOUNDED BY repoacl.DefaultTTL, not
	// unbounded, and it is the same bound that applies to a user who simply
	// does not sign in again. Failing sign-in closed on a transient DB blip
	// would be the worse trade — it converts a bounded, already-accepted
	// staleness window into a total outage of the login path.
	if s.cfg.RepoVisibility != nil {
		if err := s.cfg.RepoVisibility.InvalidateSubject(r.Context(), "github", profile.Login); err != nil {
			s.cfg.Logger.Warn("repo-acl mirror purge failed at sign-in; cached repo permissions for this subject survive until their TTL expires (bounded staleness, sign-in continues)",
				"github_login", profile.Login,
				"error", err.Error(),
				"ref", "#2071")
		}
	}

	// HttpOnly + Secure + SameSite=Lax per ADR-005. Path "/" so
	// every backend route sees it.
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    sess.PlainText,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Expires:  sess.AbsoluteExpiresAt,
		MaxAge:   int(time.Until(sess.AbsoluteExpiresAt).Seconds()),
	})

	// Mint a CSRF token alongside the session cookie. The SPA reads
	// __Host-csrf via document.cookie and mirrors it back as the
	// X-CSRF-Token header on state-changing requests; the csrf
	// middleware enforces the double-submit comparison. (E4.6 #152)
	csrfTok, err := generateCSRFToken()
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not mint CSRF token", map[string]any{"error": err.Error()})
		return
	}
	setCSRFCookie(w, csrfTok)

	redirect := s.cfg.AuthRedirectAfterLogin
	if redirect == "" {
		redirect = "/"
	}
	if !isSafeRelativeRedirect(redirect) {
		// Defense in depth: refuse open-redirect targets even if
		// configured. The redirect target is operator-set; we
		// validate to keep config typos from becoming a vector.
		redirect = "/"
	}
	// Per-request next overrides the operator-configured default.
	// Already validated above; nextRedirect is empty when the
	// validation rejected it.
	if nextRedirect != "" {
		redirect = nextRedirect
	}

	s.cfg.Logger.Info("oauth sign-in",
		"user_id", user.ID,
		"github_login", user.GitHubLogin,
		"session_id", sess.ID,
		"account_id", accountID.String(),
	)
	http.Redirect(w, r, redirect, http.StatusFound)
}

// handleGetMe implements GET /v0/auth/me. Returns the authenticated
// user (via the session resolved by the auth middleware) or 401.
func (s *Server) handleGetMe(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AuthRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "auth_unconfigured",
			"auth endpoint requires AuthRepo to be configured", nil)
		return
	}
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() || id.UserID == "" {
		s.writeError(w, r, http.StatusUnauthorized, "auth_required",
			"sign in to call this endpoint", nil)
		return
	}
	uid, err := uuid.Parse(id.UserID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"identity carried an invalid user id",
			map[string]any{"got": id.UserID})
		return
	}
	user, err := s.cfg.AuthRepo.GetUser(r.Context(), uid)
	if err != nil {
		s.writeError(w, r, http.StatusUnauthorized, "auth_required",
			"user not found", nil)
		return
	}
	// A session identity must carry a resolvable account (E44.3).
	// Defense in depth: an account deleted after sign-in nulls
	// sessions.account_id (ON DELETE SET NULL), and pre-gate sessions
	// never had one — both deny rather than render another tenant's
	// data. Bearer-token identities (no SessionID) pass with a null
	// account_id; their enforcement is E44.5.
	if id.SessionID != "" && id.AccountID == "" {
		s.writeError(w, r, http.StatusForbidden, "account_unresolved",
			"session is not bound to a workspace account; sign in again", nil)
		return
	}
	s.writeJSON(w, r, http.StatusOK, toUserResponse(user, id.AccountID))
}

// handleLogout implements POST /v0/auth/logout. Revokes the
// caller's session and clears the cookie. Idempotent: a logout
// without a session returns 401, which clients may quietly accept.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AuthRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "auth_unconfigured",
			"auth endpoint requires AuthRepo to be configured", nil)
		return
	}
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() || id.SessionID == "" {
		s.writeError(w, r, http.StatusUnauthorized, "auth_required",
			"no active session", nil)
		return
	}
	sid, err := uuid.Parse(id.SessionID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"identity carried an invalid session id", nil)
		return
	}
	if err := s.cfg.AuthRepo.Revoke(r.Context(), sid); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"revoke session failed", map[string]any{"error": err.Error()})
		return
	}
	// Clear both cookies so the browser stops sending them. The CSRF
	// cookie is cleared even though logout itself was authorized
	// against the session-bound CSRF (E4.6 #152) — once revoke
	// succeeds, neither cookie is meaningful.
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		MaxAge:   -1,
	})
	clearCSRFCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func toUserResponse(u *auth.User, accountID string) userResponse {
	resp := userResponse{
		ID:          u.ID,
		GitHubLogin: u.GitHubLogin,
		Name:        u.Name,
		Email:       u.Email,
	}
	if accountID != "" {
		resp.AccountID = &accountID
	}
	return resp
}

// accessDeniedRedirect is the safe relative target the callback sends
// membership-denied users to. Falls back to /access-denied on an
// empty or unsafe configured value (same defense-in-depth as the
// post-login redirect).
func (s *Server) accessDeniedRedirect() string {
	target := s.cfg.AuthAccessDeniedRedirect
	if target == "" || !isSafeRelativeRedirect(target) {
		return "/access-denied"
	}
	return target
}

// isSafeRelativeRedirect rejects URLs that look like open-redirect
// vectors (absolute URL, scheme-relative URL, or Windows-path
// fragments). A bare "/path" or "/" passes.
func isSafeRelativeRedirect(target string) bool {
	if target == "" {
		return false
	}
	if target[0] != '/' {
		return false
	}
	// "//evil.example.com/" or "/\evil.example.com/" can be parsed
	// as an absolute URL by some clients; reject any second char
	// that's a slash or backslash.
	if len(target) > 1 && (target[1] == '/' || target[1] == '\\') {
		return false
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return false
	}
	return parsed.Scheme == "" && parsed.Host == ""
}
