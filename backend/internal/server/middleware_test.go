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
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mcptoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
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

// TestBearerAuth_CookiePathStampsAccountID pins the E44.3 identity
// stamping: the cookie path copies the session row's account binding
// onto Identity.AccountID (and leaves it empty for unbound sessions,
// which /v0/auth/me then refuses).
func TestBearerAuth_CookiePathStampsAccountID(t *testing.T) {
	s := newServer(t, newFakeRepo())
	const boundAccount = "11111111-2222-3333-4444-555555555555"
	sessions := stubSessionAuth{
		user: &auth.User{ID: "u-1", GitHubLogin: "octocat"},
		sess: &auth.Session{ID: "s-1", UserID: "u-1", AccountID: boundAccount},
	}

	var captured Identity
	h := s.bearerAuth(nil, nil, sessions)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = IdentityFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "fhs_deadbeef"})
	h.ServeHTTP(httptest.NewRecorder(), req)

	if captured.AccountID != boundAccount {
		t.Errorf("Identity.AccountID = %q, want %q", captured.AccountID, boundAccount)
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

// TestBearerAuth_StaticToken_ThreadsAuthMethod is the static-token
// regression (#1708): a valid static api_token (auth_method='static')
// authenticates, reaches the handler with a non-anonymous identity, and
// carries its auth_method onto the Identity so the approval audit can
// record the credential kind.
func TestBearerAuth_StaticToken_ThreadsAuthMethod(t *testing.T) {
	s := newServer(t, newFakeRepo())
	tok := &apitoken.Token{Subject: "github:42", Scopes: []string{"runs:read"}, AuthMethod: "static"}
	auth := stubAPITokenAuth{tok: tok}

	var captured Identity
	h := s.bearerAuth(auth, nil, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = IdentityFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer fhk_static_token_xxxx")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if captured.IsAnonymous() {
		t.Fatalf("expected non-anonymous identity, got %+v", captured)
	}
	if captured.Subject != "github:42" {
		t.Errorf("Subject = %q, want github:42", captured.Subject)
	}
	if captured.AuthMethod != "static" {
		t.Errorf("AuthMethod = %q, want static", captured.AuthMethod)
	}
}

// TestBearerAuth_APIToken_StampsAccountID pins the E44.5 identity stamping on
// the api_token bearer path: the resolved token's AccountID is threaded onto
// Identity.AccountID so the account-ownership middleware can bound the request.
func TestBearerAuth_APIToken_StampsAccountID(t *testing.T) {
	s := newServer(t, newFakeRepo())
	const acct = "22222222-3333-4444-5555-666666666666"
	tok := &apitoken.Token{Subject: "github:42", Scopes: []string{"read:runs"}, AccountID: acct}
	auth := stubAPITokenAuth{tok: tok}

	var captured Identity
	h := s.bearerAuth(auth, nil, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = IdentityFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer fhk_token_with_account")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if captured.AccountID != acct {
		t.Errorf("Identity.AccountID = %q, want %q", captured.AccountID, acct)
	}
}

// TestBearerAuth_MCPToken_StampsAccountIDFromRun pins the E44.5 identity
// stamping on the mcp:run path: the token's AccountID is loaded from its OWN
// run via GetRunAccountID — now a REQUIRED run.Repository method (E44.11 /
// #2074), called unconditionally — so an mcp:run token is bounded to its run's
// tenant account exactly as a bearer token bound to that account.
func TestBearerAuth_MCPToken_StampsAccountIDFromRun(t *testing.T) {
	fr := newFakeRepo()
	runID := uuid.New()
	const acct = "33333333-4444-5555-6666-777777777777"
	fr.runs[runID] = &run.Run{ID: runID, AccountID: acct}
	s := newServer(t, fr)

	mcpAuth := &stubMCPAuthenticator{token: &mcptoken.Token{
		ID:        uuid.New(),
		RunID:     runID,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
		Scopes:    []string{"mcp:read"},
	}}

	var captured Identity
	h := s.bearerAuth(nil, mcpAuth, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = IdentityFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+mcptoken.TokenPrefix+"matchingplaintext")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if captured.AccountID != acct {
		t.Errorf("mcp Identity.AccountID = %q, want %q (loaded from its run)", captured.AccountID, acct)
	}
}

// Compile-time assertion that run.Repository's method set includes
// GetRunAccountID (E44.11 / #2074). This is the done-means of the promotion
// from an OPTIONAL type-asserted capability to a required method: a future
// refactor pulling GetRunAccountID back off Repository must break the BUILD
// here rather than silently restore the `!ok` degrade that let a wiring gap
// produce an accountless — and therefore globally-visible — mcp:run identity.
var _ run.AccountGetter = run.Repository(nil)

// TestBearerAuth_MCPToken_NilRunRepo_Returns503 pins the guard that replaced
// the deleted type-assertion prelude for an UNCONFIGURED run repo. A nil repo
// used to be absorbed by the `!ok` branch and fall through to a RESOLVED but
// accountless mcp identity — the globally-visible identity #2074 describes. It
// now fails CLOSED with 503 exactly like a lookup error.
func TestBearerAuth_MCPToken_NilRunRepo_Returns503(t *testing.T) {
	s := New(Config{})
	mcpAuth := &stubMCPAuthenticator{token: &mcptoken.Token{
		ID:        uuid.New(),
		RunID:     uuid.New(),
		ExpiresAt: time.Now().UTC().Add(time.Hour),
		Scopes:    []string{"mcp:read"},
	}}

	var handlerRan bool
	h := s.bearerAuth(nil, mcpAuth, nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		handlerRan = true
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+mcptoken.TokenPrefix+"matchingplaintext")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (fail closed on an unconfigured run repo)", rec.Code)
	}
	if handlerRan {
		t.Error("handler should not run when the account cannot be resolved")
	}
}

// TestBearerAuth_MCPToken_UntenantedRun_AllowedWithEmptyAccount pins the
// untenanted HAPPY PATH of the now-unconditional mcp:run account lookup: a run
// with a NULL account_id resolves as ("", nil), which is NOT an error — the
// identity carries an empty AccountID and the request is ALLOWED through to the
// handler. Distinguishing this from the fail-closed error branches is the point:
// promoting AccountGetter to a required method must not turn untenanted runs
// into 503s.
func TestBearerAuth_MCPToken_UntenantedRun_AllowedWithEmptyAccount(t *testing.T) {
	fr := newFakeRepo()
	runID := uuid.New()
	fr.runs[runID] = &run.Run{ID: runID} // AccountID "" → untenanted NULL row
	s := newServer(t, fr)

	mcpAuth := &stubMCPAuthenticator{token: &mcptoken.Token{
		ID:        uuid.New(),
		RunID:     runID,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
		Scopes:    []string{"mcp:read"},
	}}

	var captured Identity
	var handlerRan bool
	h := s.bearerAuth(nil, mcpAuth, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		handlerRan = true
		captured = IdentityFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+mcptoken.TokenPrefix+"matchingplaintext")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !handlerRan {
		t.Fatalf("handler did not run for an untenanted run (status = %d, body=%s)", rec.Code, rec.Body.String())
	}
	if captured.AccountID != "" {
		t.Errorf("mcp Identity.AccountID = %q, want \"\" (untenanted run)", captured.AccountID)
	}
	if captured.Subject != "mcp:run:"+runID.String() {
		t.Errorf("mcp Identity.Subject = %q, want mcp:run:%s", captured.Subject, runID)
	}
}

// TestBearerAuth_MCPToken_AccountLookupError_FallsThrough covers the
// non-unavailable error branch of the mcp:run AccountID-population path: when
// GetRunAccountID returns an ordinary (not DB-unavailable) error, the lookup
// must FAIL CLOSED with 503 rather than fall through to a resolved accountless
// identity. An accountless-but-resolved mcp identity would gain
// accountless-operator GLOBAL visibility on the unwrapped collection/export
// endpoints (accountVisiblePage's `if acct == "" { return page }`
// short-circuit) — a run-scoped token promoted to the global operator tier. A
// token that cannot resolve its own run's account must never reach a handler.
func TestBearerAuth_MCPToken_AccountLookupError_FallsThrough(t *testing.T) {
	fr := newFakeRepo()
	runID := uuid.New()
	fr.runs[runID] = &run.Run{ID: runID, AccountID: "44444444-5555-6666-7777-888888888888"}
	// A plain error is NOT dberr.IsUnavailable, but GetRunAccountID's failure
	// must still fail closed with 503, not fall through to a global-visibility
	// accountless identity.
	fr.getErr = fmt.Errorf("account lookup failed")
	s := newServer(t, fr)

	mcpAuth := &stubMCPAuthenticator{token: &mcptoken.Token{
		ID:        uuid.New(),
		RunID:     runID,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
		Scopes:    []string{"mcp:read"},
	}}

	var handlerRan bool
	h := s.bearerAuth(nil, mcpAuth, nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		handlerRan = true
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+mcptoken.TokenPrefix+"matchingplaintext")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (fail closed on an account lookup error)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "service_unavailable") {
		t.Errorf("body missing service_unavailable code:\n%s", rec.Body.String())
	}
	if handlerRan {
		t.Error("handler should not run when the run-scoped token cannot resolve its account")
	}
}

// TestBearerAuth_MCPToken_AccountLookupDBUnavailable_Returns503 covers the
// DB-unavailable branch of the mcp:run AccountID-population path: when
// GetRunAccountID fails because the database is unreachable, the middleware
// short-circuits with 503 rather than masking the outage as an untenanted
// (empty-account) identity that would then be denied on its own tenanted run.
func TestBearerAuth_MCPToken_AccountLookupDBUnavailable_Returns503(t *testing.T) {
	fr := newFakeRepo()
	runID := uuid.New()
	fr.runs[runID] = &run.Run{ID: runID, AccountID: "44444444-5555-6666-7777-888888888888"}
	fr.getErr = fmt.Errorf("run: account lookup: %w", &pgconn.ConnectError{})
	s := newServer(t, fr)

	mcpAuth := &stubMCPAuthenticator{token: &mcptoken.Token{
		ID:        uuid.New(),
		RunID:     runID,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
		Scopes:    []string{"mcp:read"},
	}}

	var handlerRan bool
	h := s.bearerAuth(nil, mcpAuth, nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		handlerRan = true
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+mcptoken.TokenPrefix+"matchingplaintext")
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
