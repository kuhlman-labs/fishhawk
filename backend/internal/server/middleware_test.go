package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
)

func TestRequestID_GeneratesWhenAbsent(t *testing.T) {
	var captured string
	h := requestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = RequestIDFrom(r.Context())
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if captured == "" {
		t.Fatal("expected a generated request ID on the context")
	}
	if got := rec.Header().Get("X-Request-ID"); got != captured {
		t.Errorf("X-Request-ID header = %q, want %q", got, captured)
	}
}

func TestRequestID_HonorsClientID(t *testing.T) {
	const supplied = "trace-1234"
	var captured string
	h := requestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = RequestIDFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", supplied)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if captured != supplied {
		t.Errorf("captured = %q, want %q", captured, supplied)
	}
}

func TestRequestID_RejectsOversizedClientID(t *testing.T) {
	huge := strings.Repeat("a", requestIDMaxLen+1)
	var captured string
	h := requestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = RequestIDFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", huge)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if captured == huge {
		t.Error("oversized client ID should have been replaced with a generated one")
	}
	if captured == "" {
		t.Error("expected a generated request ID after rejection")
	}
}

func TestRecovery_CatchesPanicAndLogs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := recovery(logger)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	logs := buf.String()
	if !strings.Contains(logs, `"msg":"panic"`) {
		t.Errorf("log missing panic event:\n%s", logs)
	}
	if !strings.Contains(logs, "boom") {
		t.Errorf("log missing recovered value:\n%s", logs)
	}
}

func TestBearerAuth_NoHeader_Anonymous(t *testing.T) {
	s := newServer(t, newFakeRepo())
	var captured Identity
	h := s.bearerAuth(nil, nil, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = IdentityFrom(r.Context())
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !captured.IsAnonymous() {
		t.Errorf("identity = %+v, want anonymous", captured)
	}
}

// stubAPITokenAuth is a minimal apitokenAuthenticator that returns a
// fixed (token, err) pair. Used to drive the dberr classification seam
// without a real repository.
type stubAPITokenAuth struct {
	tok *apitoken.Token
	err error
}

func (s stubAPITokenAuth) Authenticate(context.Context, string) (*apitoken.Token, error) {
	return s.tok, s.err
}

// TestBearerAuth_DBUnavailable_Returns503 is the cross-seam case
// (#764): the apitoken authenticator fails because the database is
// unreachable (an error wrapping *pgconn.ConnectError). The middleware
// must classify it via dberr and short-circuit with 503
// service_unavailable rather than masking the outage as a fall-through
// 401. Drives dberr → middleware → error envelope end-to-end (cf. #618).
func TestBearerAuth_DBUnavailable_Returns503(t *testing.T) {
	s := newServer(t, newFakeRepo())
	dbDown := fmt.Errorf("apitoken: lookup: %w", &pgconn.ConnectError{})
	auth := stubAPITokenAuth{err: dbDown}

	var handlerRan bool
	h := s.bearerAuth(auth, nil, nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		handlerRan = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer fhk_deadbeef")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "service_unavailable") {
		t.Errorf("body missing service_unavailable code:\n%s", rec.Body.String())
	}
	if handlerRan {
		t.Error("handler should not run on a 503 short-circuit")
	}
}

// stubSessionAuth implements sessionAuthenticator with a fixed return
// so the cookie path's dberr classification seam can be driven without a
// real session repository.
type stubSessionAuth struct {
	user *auth.User
	sess *auth.Session
	err  error
}

func (s stubSessionAuth) Authenticate(context.Context, string) (*auth.User, *auth.Session, error) {
	return s.user, s.sess, s.err
}

// TestBearerAuth_SessionDBUnavailable_Returns503 mirrors the apitoken
// 503 seam for the cookie path (#764): the session authenticator fails
// because the database is unreachable, so the middleware must
// short-circuit with 503 rather than fall through to anonymous. Without
// this test the cookie path's dberr guard could be dropped in a refactor
// with nothing to catch it.
func TestBearerAuth_SessionDBUnavailable_Returns503(t *testing.T) {
	s := newServer(t, newFakeRepo())
	dbDown := fmt.Errorf("auth: lookup: %w", &pgconn.ConnectError{})
	sessions := stubSessionAuth{err: dbDown}

	var handlerRan bool
	h := s.bearerAuth(nil, nil, sessions)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		handlerRan = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "fhs_deadbeef"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "service_unavailable") {
		t.Errorf("body missing service_unavailable code:\n%s", rec.Body.String())
	}
	if handlerRan {
		t.Error("handler should not run on a 503 short-circuit")
	}
}

// TestBearerAuth_MCPTokenDBUnavailable_Returns503 mirrors the apitoken
// 503 seam for the MCP-token (fhm_) path (#764): the MCP authenticator
// fails because the database is unreachable. Guards the third dberr
// short-circuit in bearerAuth against a silent refactor regression.
func TestBearerAuth_MCPTokenDBUnavailable_Returns503(t *testing.T) {
	s := newServer(t, newFakeRepo())
	dbDown := fmt.Errorf("mcptoken: lookup: %w", &pgconn.ConnectError{})
	mcpAuth := &stubMCPAuthenticator{err: dbDown}

	var handlerRan bool
	h := s.bearerAuth(nil, mcpAuth, nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		handlerRan = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer fhm_deadbeef")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "service_unavailable") {
		t.Errorf("body missing service_unavailable code:\n%s", rec.Body.String())
	}
	if handlerRan {
		t.Error("handler should not run on a 503 short-circuit")
	}
}

// TestBearerAuth_BadToken_HealthyDB_FallsThrough is the invariant the
// 503 change must NOT break: when the DB is healthy and the token is
// simply wrong (apitoken.ErrNotFound), the middleware falls through to
// the anonymous identity — the per-handler 401 is preserved, not a 503.
func TestBearerAuth_BadToken_HealthyDB_FallsThrough(t *testing.T) {
	s := newServer(t, newFakeRepo())
	auth := stubAPITokenAuth{err: apitoken.ErrNotFound}

	var captured Identity
	h := s.bearerAuth(auth, nil, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = IdentityFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer fhk_deadbeef")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (handler ran with anonymous identity)", rec.Code)
	}
	if !captured.IsAnonymous() {
		t.Errorf("identity = %+v, want anonymous fall-through on a bad token", captured)
	}
}

func TestLogging_EmitsStructuredEvent(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	// Wrap with requestID first so the log line carries one.
	h := requestID(logging(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "short and stout")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/foo", nil))

	out := buf.String()
	for _, want := range []string{
		`"msg":"request"`,
		`"method":"GET"`,
		`"path":"/foo"`,
		`"status":418`,
		`"request_id":`,
		`"duration_ms":`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %s:\n%s", want, out)
		}
	}
}
