package server

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"mime"
	"net/http"
	"net/url"
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
	// /mcp (ADR-076 / #2390) is a bearer-authenticated, non-cookie,
	// non-browser JSON-RPC transport: the streamable-HTTP body carries no
	// form encoding a browser could forge, and handleMCP independently
	// REFUSES any cookie-session identity with 401 before the transport
	// runs. So the exemption cannot become a browser-driven CSRF path onto
	// the tool surface. Like the two webhook entries above it, this is
	// belt-and-braces: the middleware's `id.SessionID == ""` bypass
	// already lets bearer requests through.
	case "/mcp":
		return true
	// POST /v0/oauth/token (ADR-076 slice 3, #2436) is non-cookie,
	// form-encoded and public-client: handleOAuthToken ignores session
	// identity entirely and refuses ANY Authorization header, so the
	// exemption cannot become a browser-driven path onto anything the forger
	// does not already have to supply. NOTE: /v0/oauth/authorize is
	// deliberately NOT exempt — the consent POST is cookie-authenticated and
	// is exactly the request an attacker would forge, so it stays CSRF-
	// enforced through the form-field fallback below.
	case "/v0/oauth/token":
		return true
	}
	return false
}

// csrfFormFallbackMaxBytes bounds the consent-POST body the form-field fallback
// buffers before ParseForm runs (CONDITION 3, #2436). Reading the raw body
// bypasses the size protection ParseForm would otherwise apply, so the read is
// explicitly capped; a hidden-field OAuth form is a few hundred bytes, so 1 MiB
// is generous while foreclosing an unbounded allocation from an unauthenticated
// request.
const csrfFormFallbackMaxBytes = 1 << 20

// csrfFormFallbackPath reports whether p is the ONE path on which the CSRF
// middleware accepts a form-field token in lieu of the X-CSRF-Token header. It
// is a narrow exact-match list mirroring csrfExemptPath's reviewable-here
// convention: a plain HTML consent form cannot set a request header, so
// enforcement on /v0/oauth/authorize needs a form-field path. Every other route
// stays header-only.
func csrfFormFallbackPath(p string) bool {
	return p == "/v0/oauth/authorize"
}

// isFormURLEncoded reports whether ct is application/x-www-form-urlencoded.
func isFormURLEncoded(ct string) bool {
	if ct == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return mt == "application/x-www-form-urlencoded"
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

		// Form-field fallback (CONDITION 3, #2436): on the ONE fallback path,
		// with a form-encoded body and no header, read the csrf_token form
		// value and compare it exactly as the header would be. The body MUST be
		// buffered and RESTORED — ParseForm consumes it, so without the restore
		// the handler's own ParseForm sees an empty form. The read is bounded to
		// foreclose an unbounded allocation from an unauthenticated request.
		if header == "" && csrfFormFallbackPath(r.URL.Path) && isFormURLEncoded(r.Header.Get("Content-Type")) {
			buf, err := io.ReadAll(io.LimitReader(r.Body, csrfFormFallbackMaxBytes+1))
			if err != nil {
				s.writeError(w, r, http.StatusBadRequest, "validation_failed",
					"could not read the request body", nil)
				return
			}
			if int64(len(buf)) > csrfFormFallbackMaxBytes {
				s.writeError(w, r, http.StatusRequestEntityTooLarge, "request_too_large",
					"the request body exceeds the maximum size", nil)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(buf))
			if vals, perr := url.ParseQuery(string(buf)); perr == nil {
				header = vals.Get("csrf_token")
			}
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
