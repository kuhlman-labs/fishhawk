package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
)

func TestGenerateCSRFToken_LengthAndUniqueness(t *testing.T) {
	a, err := generateCSRFToken()
	if err != nil {
		t.Fatalf("generateCSRFToken: %v", err)
	}
	b, err := generateCSRFToken()
	if err != nil {
		t.Fatalf("generateCSRFToken: %v", err)
	}
	if len(a) != 2*csrfTokenBytes {
		t.Errorf("token length = %d, want %d", len(a), 2*csrfTokenBytes)
	}
	if a == b {
		t.Errorf("two consecutive tokens collided: %q", a)
	}
}

func TestCSRFSafeMethod(t *testing.T) {
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		if !csrfSafeMethod(m) {
			t.Errorf("%s should be csrf-safe", m)
		}
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if csrfSafeMethod(m) {
			t.Errorf("%s should NOT be csrf-safe", m)
		}
	}
}

func TestCSRFExemptPath(t *testing.T) {
	exempt := []string{
		"/v0/auth/github/login",
		"/v0/auth/github/callback",
		"/webhooks/github",
		"/webhooks/gitlab",
	}
	for _, p := range exempt {
		if !csrfExemptPath(p) {
			t.Errorf("%s should be exempt", p)
		}
	}
	notExempt := []string{
		"/v0/auth/me",
		"/v0/auth/logout",
		"/v0/runs",
		"/v0/stages/abc/approvals",
	}
	for _, p := range notExempt {
		if csrfExemptPath(p) {
			t.Errorf("%s should NOT be exempt", p)
		}
	}
}

// signInWithSession registers a fake GitHub identity, walks the
// session helper, and returns the cookies a real browser would have
// after a successful OAuth round-trip: session + CSRF.
func signInWithSession(t *testing.T) (s *Server, sessCookie, csrfCookie *http.Cookie) {
	t.Helper()
	srv, repo := newAuthServer(t)
	_, sess, err := repo.SignIn(context.Background(), auth.GitHubProfile{
		ID: 99, Login: "csrf-tester", Name: "Tester",
	}, uuid.New())
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	csrfTok, err := generateCSRFToken()
	if err != nil {
		t.Fatalf("generateCSRFToken: %v", err)
	}
	return srv,
		&http.Cookie{Name: auth.SessionCookieName, Value: sess.PlainText},
		&http.Cookie{Name: CSRFCookieName, Value: csrfTok}
}

func TestCSRF_GETBypasses(t *testing.T) {
	s, sessCookie, _ := signInWithSession(t)
	req := httptest.NewRequest(http.MethodGet, "/v0/auth/me", nil)
	req.AddCookie(sessCookie)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("GET /v0/auth/me with no CSRF header: status = %d, want 200", w.Code)
	}
}

func TestCSRF_AnonymousPOSTBypasses(t *testing.T) {
	// No identity → middleware does NOT 403; the handler 401s.
	// This protects "POST returns 401 (auth_required)" semantics
	// for unauthenticated callers, who don't need a CSRF token to
	// learn they're not signed in.
	srv, _ := newAuthServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v0/auth/logout", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous POST: status = %d, want 401 (handler), not 403 (csrf)", w.Code)
	}
}

func TestCSRF_SessionCookiePOSTWithoutHeaderRejected(t *testing.T) {
	s, sessCookie, csrfCookie := signInWithSession(t)
	req := httptest.NewRequest(http.MethodPost, "/v0/auth/logout", nil)
	req.AddCookie(sessCookie)
	req.AddCookie(csrfCookie)
	// Note: no X-CSRF-Token header.
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "csrf_required") {
		t.Errorf("body missing csrf_required: %s", w.Body.String())
	}
}

func TestCSRF_SessionCookiePOSTWithMismatchedHeaderRejected(t *testing.T) {
	s, sessCookie, csrfCookie := signInWithSession(t)
	req := httptest.NewRequest(http.MethodPost, "/v0/auth/logout", nil)
	req.AddCookie(sessCookie)
	req.AddCookie(csrfCookie)
	req.Header.Set(CSRFHeaderName, "different-value")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestCSRF_SessionCookiePOSTWithoutCookieRejected(t *testing.T) {
	// Header present, cookie missing — happens when the user has a
	// pre-CSRF-deploy session and JS sends a header it didn't read.
	s, sessCookie, csrfCookie := signInWithSession(t)
	req := httptest.NewRequest(http.MethodPost, "/v0/auth/logout", nil)
	req.AddCookie(sessCookie)
	req.Header.Set(CSRFHeaderName, csrfCookie.Value)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestCSRF_SessionCookiePOSTWithMatchingHeaderPasses(t *testing.T) {
	s, sessCookie, csrfCookie := signInWithSession(t)
	req := httptest.NewRequest(http.MethodPost, "/v0/auth/logout", nil)
	req.AddCookie(sessCookie)
	req.AddCookie(csrfCookie)
	req.Header.Set(CSRFHeaderName, csrfCookie.Value)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204:\n%s", w.Code, w.Body.String())
	}
}
