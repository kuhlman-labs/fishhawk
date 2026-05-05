package server

import (
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
)

// userResponse mirrors the OpenAPI `User` schema. Surfaces only
// what the SPA / CLI needs.
type userResponse struct {
	ID          string  `json:"id"`
	GitHubLogin string  `json:"github_login"`
	Name        string  `json:"name"`
	Email       *string `json:"email"`
}

// handleGitHubLogin implements GET /v0/auth/github/login. Mints a
// state value, stores it in a short-lived browser cookie, and
// redirects to GitHub's authorize URL.
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

	user, sess, err := s.cfg.AuthRepo.SignIn(r.Context(), *profile)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"sign-in failed", map[string]any{"error": err.Error()})
		return
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

	s.cfg.Logger.Info("oauth sign-in",
		"user_id", user.ID,
		"github_login", user.GitHubLogin,
		"session_id", sess.ID,
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
	s.writeJSON(w, r, http.StatusOK, toUserResponse(user))
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

func toUserResponse(u *auth.User) userResponse {
	return userResponse{
		ID:          u.ID,
		GitHubLogin: u.GitHubLogin,
		Name:        u.Name,
		Email:       u.Email,
	}
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
