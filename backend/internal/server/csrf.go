package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
)

// CSRFCookieName is the cookie name for the double-submit token. The
// __Host- prefix forces Secure + Path=/ + no Domain; the browser
// rejects the cookie if any of those constraints are missed, which
// catches misconfigurations at set-time rather than at attack-time.
const CSRFCookieName = "__Host-csrf"

// CSRFHeaderName is the request header callers attach to mirror
// the cookie value on state-changing requests. Matches docs/api/v0
// security scheme `sessionCookie`.
const CSRFHeaderName = "X-CSRF-Token"

// csrfTokenBytes is the random material a token expands. 32 bytes
// (256 bits) hex-encoded gives a 64-char value — comfortably
// resistant to guessing without bloating the cookie.
const csrfTokenBytes = 32

// generateCSRFToken returns a fresh hex-encoded CSRF token. Callers
// hand the value to setCSRFCookie and expect the browser to submit
// it back via a JS-attached header on state-changing requests.
func generateCSRFToken() (string, error) {
	buf := make([]byte, csrfTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// setCSRFCookie writes a fresh CSRF cookie on the response. The
// cookie is intentionally NOT HttpOnly: the SPA's fetch wrapper
// reads it via document.cookie and mirrors the value back as the
// X-CSRF-Token header. SameSite=Strict means the browser won't
// auto-send the cookie on cross-site requests at all — the
// double-submit comparison is the inner check; SameSite is the
// outer one.
func setCSRFCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    token,
		Path:     "/",
		Secure:   true,
		HttpOnly: false,
		SameSite: http.SameSiteStrictMode,
	})
}

// clearCSRFCookie writes an expired cookie so the browser drops the
// stored value. Pair with the session-cookie clear at logout.
func clearCSRFCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    "",
		Path:     "/",
		Secure:   true,
		HttpOnly: false,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// csrfSafeMethod reports whether m is a non-state-changing HTTP
// method. The CSRF middleware only enforces on the unsafe set;
// safe methods are by definition not vulnerable to CSRF because
// they (per RFC 7231) have no observable side-effects.
func csrfSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// csrfExemptPath reports whether p is on the unauthenticated /
// runner-OIDC surface and therefore not subject to CSRF. The set is
// kept as a small exact-match list rather than a dynamic config so
// every exemption is reviewable here. New paths default to enforced.
func csrfExemptPath(p string) bool {
	// /v0/auth/github/* and /v0/auth/gitlab/* — the OAuth handshake
	// itself; the cookie doesn't exist yet on /login, and /callback's
	// POST-CSRF substitute is the OAuth `state` parameter (auth.go).
	if strings.HasPrefix(p, "/v0/auth/github/") || strings.HasPrefix(p, "/v0/auth/gitlab/") {
		return true
	}
	// Runner-facing endpoints authenticate via GitHub Actions OIDC
	// (signing-key, trace) or HMAC (webhook); they never carry a
	// session cookie, so CSRF doesn't apply. The csrf middleware
	// already bails when Identity has no SessionID, so these are
	// belt-and-braces.
	switch p {
	case "/webhooks/github", "/webhooks/gitlab":
		return true
	}
	return false
}

// csrf enforces the double-submit pattern. On any state-changing
// method (POST/PUT/PATCH/DELETE) for a session-cookie-authenticated
// request, the X-CSRF-Token header MUST equal the __Host-csrf
// cookie value. Bearer-token requests bypass — bearer tokens aren't
// vulnerable to CSRF in the first place — and so do anonymous
// requests, which the per-handler logic 401s on its own.
//
// The middleware sits after bearerAuth in the chain so it can read
// the resolved Identity. A missing or mismatched token returns 403
// with error code `csrf_required`.
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if csrfSafeMethod(r.Method) || csrfExemptPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		id := IdentityFrom(r.Context())
		// Only session-cookie identities are subject to CSRF. Bearer
		// tokens are immune (no auto-submission by the browser);
		// anonymous identities will fail auth in the handler.
		if id.SessionID == "" {
			next.ServeHTTP(w, r)
			return
		}

		header := r.Header.Get(CSRFHeaderName)
		var cookieValue string
		if c, err := r.Cookie(CSRFCookieName); err == nil {
			cookieValue = c.Value
		}

		if header == "" || cookieValue == "" ||
			subtle.ConstantTimeCompare([]byte(header), []byte(cookieValue)) != 1 {
			s.writeError(w, r, http.StatusForbidden, "csrf_required",
				"state-changing request from a cookie session must include a matching "+CSRFHeaderName+" header",
				nil)
			return
		}

		next.ServeHTTP(w, r)
	})
}
