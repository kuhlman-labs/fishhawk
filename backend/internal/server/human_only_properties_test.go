package server

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
)

// The four ADR-143 D1 properties (#5228), as five checks. D1 admits a
// server-check clear only on a `session` credential, so its soundness rests on
// a session being minted only by a human's forge sign-in and never being
// reachable from a tool surface:
//
//	(a) /mcp refuses a session-cookie request (with or without a bearer) with
//	    the 401 challenge, before any tool runs;
//	(b) auth.Repository.SignIn has exactly ONE production caller, the forge
//	    sign-in callback;
//	(c) no route mints a session from a bearer: the session cookie is written
//	    with a value only inside that callback, and a bearer GET to each
//	    callback route without a valid OAuth state sets no session cookie;
//	(d) neither MCP transport forwards a cookie to the backend;
//	(e) the CSRF double-submit check guards each of the four D2 routes.
//
// These are PROPERTY checks, not deletable controls: (a), (d) and (e) each
// carry a positive control (the same request minus the property-relevant part
// reaches the handler), so a refusal for an unrelated reason cannot pass them;
// (b) and (c) fail on a second caller or cookie writer.
//
// COUNTERFACTUALS (hand-run, #5228 slice 1; each restored byte-identically):
//
//  1. a second production SignIn caller (a temporary non-test file in this
//     package calling repo.SignIn): TestHumanOnlyProperty_SingleSignInCaller
//     went RED naming it.
//  2. a second session-cookie writer with a value (the same temporary file
//     building &http.Cookie{Name: auth.SessionCookieName, Value: v}):
//     TestHumanOnlyProperty_SessionMintedOnlyByCallback went RED naming it.
//  3. csrfExemptPath made to exempt "/v0/concerns/" and "/v0/runs/" prefixes:
//     TestHumanOnlyProperty_CSRFGuardsClearRoutes went RED (exempt=true, and
//     the token-less session request was admitted: 200, row cleared, one
//     clear entry appended).
//  4. handleMCP's `id.TokenID == ""` arm deleted (`if id.IsAnonymous() {`):
//     both cookie arms of TestHumanOnlyProperty_MCPRefusesSessionCookie went
//     RED — the session identity passed the bearer gate and got a 403 from the
//     per-tool scope gate instead of the 401 challenge, with no
//     WWW-Authenticate header.

// rawMCPPost sends one tools/call POST to the fixture's /mcp with the given
// bearer and Cookie header (either may be empty).
func rawMCPPost(t *testing.T, f *mcpFixture, bearer, cookie, body string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.ts.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	setStandardMCPHeaders(t, req, body)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /mcp response: %v", err)
	}
	return resp.StatusCode, resp.Header, string(raw)
}

// TestHumanOnlyProperty_MCPRefusesSessionCookie is check (a): a POST /mcp
// carrying a VALID session cookie — alone, or with a valid static bearer
// (bearerAuth resolves the cookie first, so the identity is the session) — is
// answered with the 401 challenge; no inner /v0/ call is made, the
// server_check row stays raised and no concern_waived row is appended.
// Positive control: the SAME body on the bearer alone reaches the clearing
// handler (its concern_requires_session refusal comes back in the tool
// result), so the 401 is the cookie's, not the body's.
func TestHumanOnlyProperty_MCPRefusesSessionCookie(t *testing.T) {
	fx := newConcernMCPFixture(t)
	runID := uuid.New()
	row := fx.seed(t, concernClearTools[0], runID)
	bearer := fx.staticBearer(t)
	body := callToolBody(1, "fishhawk_waive_concern", concernClearTools[0].args(row))
	sessionCookie := fx.session.Name + "=" + fx.session.Value

	for name, b := range map[string]string{"cookie_only": "", "cookie_and_bearer": bearer} {
		t.Run(name, func(t *testing.T) {
			fx.recorder.reset()
			status, hdr, resp := rawMCPPost(t, fx.mcpFixture, b, sessionCookie, body)
			if status != http.StatusUnauthorized || !strings.Contains(resp, "authentication_required") {
				t.Errorf("status = %d, want the 401 authentication_required challenge:\n%s", status, resp)
			}
			if hdr.Get("WWW-Authenticate") == "" {
				t.Errorf("401 carries no WWW-Authenticate challenge")
			}
			if calls := fx.recorder.snapshot(); len(calls) != 0 {
				t.Errorf("a session-cookie /mcp request reached %d inner /v0/ call(s): %+v", len(calls), calls)
			}
			fx.assertCommitted(t, row, concern.StateRaised, CategoryConcernWaived, 0)
		})
	}

	t.Run("positive_control_bearer_only", func(t *testing.T) {
		fx.recorder.reset()
		status, _, resp := rawMCPPost(t, fx.mcpFixture, bearer, "", body)
		if status != http.StatusOK || !strings.Contains(resp, errCodeConcernRequiresSession) {
			t.Fatalf("bearer-only /mcp = %d, want 200 carrying the handler's %s refusal:\n%s",
				status, errCodeConcernRequiresSession, resp)
		}
		lastInboundFor(t, fx.recorder.snapshot(), "/v0/concerns/"+row.ID.String()+"/waive")
		fx.assertCommitted(t, row, concern.StateRaised, CategoryConcernWaived, 0)
	})
}

// backendRoot is the backend module root, two levels above this package.
const backendRoot = "../.."

// walkBackendNonTest parses every non-test .go file under backend/ and calls
// visit with its backend-relative path. testdata and vendor trees are skipped.
func walkBackendNonTest(t *testing.T, visit func(rel string, f *ast.File)) int {
	t.Helper()
	fset := token.NewFileSet()
	n := 0
	err := filepath.WalkDir(backendRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || name == "vendor" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", p, perr)
		}
		rel, _ := filepath.Rel(backendRoot, p)
		visit(filepath.ToSlash(rel), f)
		n++
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", backendRoot, err)
	}
	// A wrong root would scan nothing and pass vacuously.
	if n < 200 {
		t.Fatalf("scanned only %d non-test files under %s; the walk root is wrong", n, backendRoot)
	}
	return n
}

// eachFunc visits each function body in f with its name.
func eachFunc(f *ast.File, visit func(fn string, body ast.Node)) {
	for _, decl := range f.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Body != nil {
			visit(fd.Name.Name, fd.Body)
		}
	}
	// Package-level var initialisers (a func literal in a var) count too, under
	// a synthetic name, so a writer cannot hide outside a FuncDecl.
	for _, decl := range f.Decls {
		if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.VAR {
			visit("<package var>", gd)
		}
	}
}

// signInCallbackSite is the ONE production site that creates a session.
const signInCallbackSite = "internal/server/auth.go:handleForgeCallback"

// TestHumanOnlyProperty_SingleSignInCaller is check (b): an AST scan of every
// non-test .go file under backend/ finds exactly ONE call to a SignIn method —
// auth.Repository.SignIn in the forge sign-in callback. A second caller (a
// route minting a session some other way) fails here naming it. See
// COUNTERFACTUAL 1.
func TestHumanOnlyProperty_SingleSignInCaller(t *testing.T) {
	var sites []string
	walkBackendNonTest(t, func(rel string, f *ast.File) {
		eachFunc(f, func(fn string, body ast.Node) {
			ast.Inspect(body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "SignIn" {
						sites = append(sites, rel+":"+fn)
					}
				}
				return true
			})
		})
	})
	if want := []string{signInCallbackSite}; !reflect.DeepEqual(sites, want) {
		t.Errorf("production SignIn callers = %v, want exactly %v: a session may be minted only by the forge "+
			"sign-in callback (ADR-143 D1 rests on it)", sites, want)
	}
}

// isSessionCookieName reports whether e names the session cookie: the
// auth.SessionCookieName constant (qualified or, inside package auth, bare) or
// its literal value.
func isSessionCookieName(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.SelectorExpr:
		return v.Sel.Name == "SessionCookieName"
	case *ast.Ident:
		return v.Name == "SessionCookieName"
	case *ast.BasicLit:
		s, err := strconv.Unquote(v.Value)
		return err == nil && s == auth.SessionCookieName
	}
	return false
}

// TestHumanOnlyProperty_SessionMintedOnlyByCallback is check (c). Static half:
// every http.Cookie literal naming the session cookie under backend/ carries an
// empty Value (a clear) except ONE, in the forge sign-in callback; and the
// cookie's literal name appears nowhere but its constant. Behavioural half: a
// bearer-authenticated GET to each forge callback route without a valid OAuth
// state (no state cookie, or one that does not match) sets no session cookie
// and creates no session; the positive control — the same route with a
// matching state and no bearer — does set one, so the observation channel is
// live. See COUNTERFACTUAL 2.
func TestHumanOnlyProperty_SessionMintedOnlyByCallback(t *testing.T) {
	var minting, literals []string
	walkBackendNonTest(t, func(rel string, f *ast.File) {
		eachFunc(f, func(fn string, body ast.Node) {
			ast.Inspect(body, func(n ast.Node) bool {
				cl, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				sel, ok := cl.Type.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Cookie" {
					return true
				}
				named, cleared := false, true
				for _, el := range cl.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, _ := kv.Key.(*ast.Ident)
					if key == nil {
						continue
					}
					switch key.Name {
					case "Name":
						named = isSessionCookieName(kv.Value)
					case "Value":
						lit, ok := kv.Value.(*ast.BasicLit)
						cleared = ok && lit.Value == `""`
					}
				}
				if named && !cleared {
					minting = append(minting, rel+":"+fn)
				}
				return true
			})
		})
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && isSessionCookieName(lit) && rel != "internal/auth/auth.go" {
				literals = append(literals, rel)
			}
			return true
		})
	})
	if want := []string{signInCallbackSite}; !reflect.DeepEqual(minting, want) {
		t.Errorf("session-cookie writers with a value = %v, want exactly %v", minting, want)
	}
	if len(literals) != 0 {
		t.Errorf("the session cookie's literal name %q appears outside its constant in %v; use auth.SessionCookieName",
			auth.SessionCookieName, literals)
	}

	authDB := newFakeAuthRepo()
	tokens := newFakeTokenRepo()
	_, gh := stubGitHubOAuthServer(t)
	_, gl, _ := stubGitLabOAuthServer(t, "gluser", nil)
	s := New(Config{
		Addr:           "127.0.0.1:0",
		AuthRepo:       authDB,
		APITokenRepo:   tokens,
		GitHubOAuth:    gh,
		GitLabOAuth:    gl,
		AuthMembership: &fakeMembershipResolver{ids: []uuid.UUID{testAccountID}},
	})
	tok, err := tokens.Issue(context.Background(), "github:alice", clearScopes)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	get := func(path, bearer string, stateCookie string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path+"?code=abc&state=state-xyz", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if stateCookie != "" {
			req.AddCookie(&http.Cookie{Name: auth.StateCookieName, Value: stateCookie})
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		return w
	}
	sessionSet := func(w *httptest.ResponseRecorder) bool {
		for _, c := range w.Result().Cookies() {
			if c.Name == auth.SessionCookieName && c.Value != "" {
				return true
			}
		}
		return false
	}
	sessions := func() int {
		authDB.mu.Lock()
		defer authDB.mu.Unlock()
		return len(authDB.sessions)
	}
	for _, route := range []string{"/v0/auth/github/callback", "/v0/auth/gitlab/callback"} {
		for name, state := range map[string]string{"no_state_cookie": "", "mismatched_state": "state-forged"} {
			t.Run(route+"/"+name, func(t *testing.T) {
				before := sessions()
				w := get(route, tok.PlainText, state)
				if sessionSet(w) {
					t.Errorf("a bearer GET without a valid OAuth state set a session cookie (status %d)", w.Code)
				}
				if got := sessions(); got != before {
					t.Errorf("sessions = %d after the bearer GET, want %d (none minted)", got, before)
				}
			})
		}
		t.Run(route+"/positive_control", func(t *testing.T) {
			before := sessions()
			w := get(route, "", "state-xyz")
			if !sessionSet(w) || sessions() != before+1 {
				t.Fatalf("a valid-state forge callback set no session (status %d, sessions %d→%d); "+
					"the refusal arms would pass vacuously:\n%s", w.Code, before, sessions(), w.Body.String())
			}
		})
	}
}

// TestHumanOnlyProperty_MCPRedialForwardsNoCookie is check (d): neither MCP
// transport forwards a cookie. HTTP /mcp: an outer request carrying a valid
// bearer plus an UNKNOWN session cookie (bearerAuth falls through to the
// bearer) re-dials the clearing route with the bearer and NO Cookie header.
// stdio: every request the mcpserver client sends to a recording backend that
// PLANTS a session cookie on each response carries no Cookie header, so the
// client holds no cookie jar. Positive controls show the recorder captures a
// Cookie header when one is sent, and that a jar-holding client WOULD replay
// the planted cookie.
func TestHumanOnlyProperty_MCPRedialForwardsNoCookie(t *testing.T) {
	t.Run("http_mcp", func(t *testing.T) {
		fx := newConcernMCPFixture(t)
		row := fx.seed(t, concernClearTools[0], uuid.New())
		bearer := fx.staticBearer(t)
		body := callToolBody(1, "fishhawk_waive_concern", concernClearTools[0].args(row))

		fx.recorder.reset()
		status, _, resp := rawMCPPost(t, fx.mcpFixture, bearer,
			auth.SessionCookieName+"=not-a-live-session; theme=dark", body)
		if status != http.StatusOK || !strings.Contains(resp, errCodeConcernRequiresSession) {
			t.Fatalf("bearer + unknown cookie /mcp = %d, want 200 carrying %s:\n%s", status, errCodeConcernRequiresSession, resp)
		}
		inner := lastInboundFor(t, fx.recorder.snapshot(), "/v0/concerns/"+row.ID.String()+"/waive")
		if inner.cookie != "" {
			t.Errorf("the /mcp inner re-dial carried Cookie %q, want none", inner.cookie)
		}
		if inner.auth != "Bearer "+bearer {
			t.Errorf("the /mcp inner re-dial carried %q, want the outer bearer", inner.auth)
		}
		fx.assertCommitted(t, row, concern.StateRaised, CategoryConcernWaived, 0)

		// Positive control: the recorder does capture a Cookie header.
		fx.recorder.reset()
		req, _ := http.NewRequest(http.MethodGet, fx.ts.URL+"/v0/runs", nil)
		req.Header.Set("Cookie", "probe=1")
		if r, err := http.DefaultClient.Do(req); err == nil {
			_ = r.Body.Close()
		}
		if got := lastInboundFor(t, fx.recorder.snapshot(), "/v0/runs").cookie; got != "probe=1" {
			t.Fatalf("recorder captured Cookie %q on a direct request, want probe=1; the re-dial assertion would be vacuous", got)
		}
	})

	t.Run("stdio", func(t *testing.T) {
		var mu sync.Mutex
		var cookies []string
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			cookies = append(cookies, r.Header.Get("Cookie"))
			mu.Unlock()
			http.SetCookie(w, &http.Cookie{Name: auth.SessionCookieName, Value: "planted-session", Path: "/"})
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"code":"concern_requires_session","message":"refused"}}`)
		}))
		t.Cleanup(backend.Close)

		cs := stdioSession(t, backend.URL, "fhk_stdio_probe")
		for i := 0; i < 2; i++ {
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "fishhawk_waive_concern",
				Arguments: map[string]any{"concern_id": uuid.NewString(), "reason": "r", "judgement": "no_defect"},
			})
			assertToolRefusedSession(t, "fishhawk_waive_concern", res, err)
		}
		mu.Lock()
		got := append([]string(nil), cookies...)
		mu.Unlock()
		if len(got) < 2 {
			t.Fatalf("recording backend saw %d request(s), want ≥2 (the second is the replay probe)", len(got))
		}
		for i, c := range got {
			if c != "" {
				t.Errorf("stdio request %d carried Cookie %q, want none", i, c)
			}
		}

		// Positive control: a jar-holding client replays the planted cookie.
		jar, _ := cookiejar.New(nil)
		client := &http.Client{Jar: jar}
		for i := 0; i < 2; i++ {
			if r, err := client.Get(backend.URL + "/v0/probe"); err == nil {
				_ = r.Body.Close()
			}
		}
		mu.Lock()
		last := cookies[len(cookies)-1]
		mu.Unlock()
		if !strings.Contains(last, "planted-session") {
			t.Fatalf("a jar client's second request carried Cookie %q, want the planted session; the stdio assertion would be vacuous", last)
		}
	})
}

// TestHumanOnlyProperty_CSRFGuardsClearRoutes is check (e): none of the four
// D2 routes is CSRF-exempt, and a SESSION request without X-CSRF-Token is
// refused 403 csrf_required by the middleware with the row in its start state
// and no clear entry; the same request WITH the token (the positive control)
// is admitted and clears the row. See COUNTERFACTUAL 3.
func TestHumanOnlyProperty_CSRFGuardsClearRoutes(t *testing.T) {
	for _, vc := range clearVerbCases {
		t.Run(vc.verb, func(t *testing.T) {
			fx := newSessionOnlyFixture(t)
			row := vc.seed(t, fx, uuid.New())
			path, body := vc.request(row, false)
			if csrfExemptPath(path) {
				t.Errorf("csrfExemptPath(%q) = true; a D2 route must be CSRF-enforced", path)
			}

			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(fx.session)
			req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: fx.csrf})
			w := httptest.NewRecorder()
			fx.s.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusForbidden || decodeErrorEnvelope(t, w).Code != "csrf_required" {
				t.Errorf("token-less session %s = %d %s, want 403 csrf_required", vc.verb, w.Code, w.Body.String())
			}
			fx.assertCommitted(t, row, vc.before, vc.category, 0)

			w = fx.do(t, path, body, "", true, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("positive control: session %s WITH the CSRF token = %d, want 200:\n%s", vc.verb, w.Code, w.Body.String())
			}
			fx.assertCommitted(t, row, vc.cleared, vc.category, 1)
		})
	}
}
