package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/hostdispatcher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Host-dispatcher identity tests (ADR-104 Q4, E88.2 / #4526). Each control's
// hand-run counterfactual is recorded in the PR Notes and the sidecar; the
// short form sits on each test.

const hdSubject = "host-dispatcher/devbox"

// hdFixture is a server over a token store, a run repo seeded with one live
// run, and an audit fake that records chained appends.
type hdFixture struct {
	t      *testing.T
	s      *Server
	tokens *fakeTokenRepo
	runs   *fakeRepo
	aud    *foAuditFake
	oauth  *fakeOAuthStore
	runID  uuid.UUID
}

func newHDFixture(t *testing.T) *hdFixture {
	t.Helper()
	f := &hdFixture{t: t, tokens: newFakeTokenRepo(), runs: newFakeRepo(), aud: &foAuditFake{}, oauth: newFakeOAuthStore()}
	f.s = New(Config{
		Addr: "127.0.0.1:0", APITokenRepo: f.tokens, RunRepo: f.runs, AuditRepo: f.aud,
		OAuthASIssuer: testIssuer, OAuthStore: f.oauth, OAuthCIMDFetcher: newCIMDFetcher(newCIMD()),
	})
	f.runID = uuid.New()
	f.runs.mu.Lock()
	f.runs.runs[f.runID] = &run.Run{ID: f.runID, Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change",
		State: run.StateRunning, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	f.runs.mu.Unlock()
	return f
}

func (f *hdFixture) mint(subject string, scopes ...string) string {
	f.t.Helper()
	tok, err := f.tokens.Issue(context.Background(), subject, scopes)
	if err != nil {
		f.t.Fatalf("issue: %v", err)
	}
	return tok.PlainText
}

func (f *hdFixture) do(method, path, bearer, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(w, req)
	return w
}

func (f *hdFixture) recordAct(bearer, source string) *httptest.ResponseRecorder {
	f.t.Helper()
	body := `{"action":"dispatch_stage","stage":"plan","source":"` + source + `"}`
	return f.do(http.MethodPost, "/v0/runs/"+f.runID.String()+"/auto-drive/acts", bearer, body)
}

func hdErrCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	return env.Error.Code
}

// TestHostDispatcherToken_ClampDropsApprovals: a dispatcher token hand-minted
// WITH write:approvals still cannot approve a plan — 403 and NO approval row.
// The route allowlist refuses it first (host_dispatcher_route_forbidden); the
// clamp behind it is pinned by TestHostDispatcherToken_ClampedIdentity.
// Counterfactual run by hand: deleting the allowlist check AND the ClampScopes
// call let the record's own write:approvals through (approve 200, the row
// landed), RED; deleting only the allowlist check turned the code assertion
// RED (insufficient_scope from the clamp).
func TestHostDispatcherToken_ClampDropsApprovals(t *testing.T) {
	tokens, ar, rr, aud := newFakeTokenRepo(), newFakeApprovalRepo(), newApprovalRunRepo(), &foAuditFake{}
	s := New(Config{Addr: "127.0.0.1:0", APITokenRepo: tokens, ApprovalRepo: ar, RunRepo: rr, AuditRepo: aud,
		IdentityProvider: &fakeIdentityProvider{perm: identity.PermissionAdmin, member: true}})
	stage := rr.seedStage(run.StageStateAwaitingApproval)
	rr.seedRun(&run.Run{ID: stage.RunID, Repo: "kuhlman-labs/fishhawk", State: run.StateRunning})
	tok, err := tokens.Issue(context.Background(), hdSubject, []string{"write:approvals", "write:runs", "read:runs", "write:stages"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v0/stages/"+stage.ID.String()+"/approvals", strings.NewReader(`{"decision":"approve"}`))
	req.Header.Set("Authorization", "Bearer "+tok.PlainText)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden || hdErrCode(t, w) != "host_dispatcher_route_forbidden" {
		t.Fatalf("approve with a dispatcher token = %d %s, want 403 host_dispatcher_route_forbidden", w.Code, w.Body.String())
	}
	if n := len(ar.all); n != 0 {
		t.Fatalf("a dispatcher recorded %d approval(s)", n)
	}
}

// TestHostDispatcherToken_ClampedIdentity pins the clamped scope set the next
// handler sees, and that a non-dispatcher token is never clamped.
func TestHostDispatcherToken_ClampedIdentity(t *testing.T) {
	f := newHDFixture(t)
	tok := f.mint(hdSubject, "write:approvals", "write:runs", "read:runs", "write:deploy", "write:stages", "read:audit")
	id, status, body := hdDriveBearerAt(f.s, tok, http.MethodGet, "/v0/runs")
	if status != http.StatusOK || id.Subject != hdSubject {
		t.Fatalf("dispatcher token refused: %d %s", status, body)
	}
	if got, want := strings.Join(id.Scopes, ","), "write:runs,read:runs,write:stages,read:audit"; got != want {
		t.Fatalf("dispatcher scopes = %q, want %q", got, want)
	}
	human := f.mint("github:octocat", "write:approvals", "write:runs")
	hid, _, _ := driveBearerToken(f.s, f.tokens, nil, human)
	if got := strings.Join(hid.Scopes, ","); got != "write:approvals,write:runs" {
		t.Fatalf("human scopes = %q; the clamp must touch only dispatcher subjects", got)
	}
}

// TestHostDispatcherToken_InvalidSubjectRefused: a token-store record whose
// subject carries the prefix but a malformed label is refused 401
// invalid_token_subject before any handler runs. Counterfactual run by hand:
// deleting the ValidateSubject refusal let the request reach the handler, RED.
func TestHostDispatcherToken_InvalidSubjectRefused(t *testing.T) {
	f := newHDFixture(t)
	for _, subject := range []string{"host-dispatcher/", "host-dispatcher/a/b"} {
		tok := f.mint(subject, "write:runs", "read:runs")
		id, status, body := driveBearerToken(f.s, f.tokens, nil, tok)
		if status != http.StatusUnauthorized || !strings.Contains(body, "invalid_token_subject") {
			t.Fatalf("subject %q = %d %s, want 401 invalid_token_subject", subject, status, body)
		}
		if id.Subject != "" {
			t.Fatalf("subject %q reached the handler as %+v", subject, id)
		}
	}
	if n := len(f.aud.chainedOf(CategoryRunAutoDriven)); n != 0 {
		t.Fatalf("run_auto_driven rows = %d, want 0", n)
	}
}

// TestRecordAct_HostDispatcherAdmittedUnderWriteRuns: a dispatcher token with
// write:runs and NO write:approvals records a tagged dispatch act; the row is
// read back with the dispatcher subject and actor_kind agent. Counterfactual
// run by hand: deleting the admission branch returned 403 and no row, RED.
func TestRecordAct_HostDispatcherAdmittedUnderWriteRuns(t *testing.T) {
	f := newHDFixture(t)
	tok := f.mint(hdSubject, hostdispatcher.Scopes()...)
	w := f.recordAct(tok, hostdispatcher.SourceTag)
	if w.Code != http.StatusOK {
		t.Fatalf("record-act = %d %s, want 200", w.Code, w.Body.String())
	}
	rows := f.aud.chainedOf(CategoryRunAutoDriven)
	if len(rows) != 1 {
		t.Fatalf("run_auto_driven rows = %d, want 1", len(rows))
	}
	if rows[0].ActorSubject == nil || *rows[0].ActorSubject != hdSubject {
		t.Fatalf("actor subject = %v, want %s", rows[0].ActorSubject, hdSubject)
	}
	if rows[0].ActorKind == nil || *rows[0].ActorKind != audit.ActorAgent {
		t.Fatalf("actor kind = %v, want agent", rows[0].ActorKind)
	}
	var p map[string]any
	_ = json.Unmarshal(rows[0].Payload, &p)
	if p["source"] != hostdispatcher.SourceTag || p["stage"] != "plan" || p["act"] != autoDriveActDispatch {
		t.Fatalf("payload = %v", p)
	}

	// A dispatcher token WITHOUT write:runs is refused 403, no row.
	ro := f.mint("host-dispatcher/readonly", "read:runs", "write:stages")
	w = f.recordAct(ro, hostdispatcher.SourceTag)
	if w.Code != http.StatusForbidden || hdErrCode(t, w) != "insufficient_scope" {
		t.Fatalf("dispatcher without write:runs = %d %s, want 403 insufficient_scope", w.Code, w.Body.String())
	}
	if n := len(f.aud.chainedOf(CategoryRunAutoDriven)); n != 1 {
		t.Fatalf("run_auto_driven rows = %d after the refusal, want still 1", n)
	}
}

// TestRecordAct_NonDispatcherStillNeedsApprovals isolates the over-broad
// mutation: an operator-subject token with write:runs only stays 403, no row.
// Counterfactual run by hand: replacing the subject check with `true` admitted
// it (200, one row), RED.
func TestRecordAct_NonDispatcherStillNeedsApprovals(t *testing.T) {
	f := newHDFixture(t)
	tok := f.mint("github:octocat", "write:runs", "read:runs")
	w := f.recordAct(tok, hostdispatcher.SourceTag)
	if w.Code != http.StatusForbidden || hdErrCode(t, w) != "insufficient_scope" ||
		!strings.Contains(w.Body.String(), "write:approvals") {
		t.Fatalf("operator write:runs-only = %d %s, want 403 insufficient_scope naming write:approvals", w.Code, w.Body.String())
	}
	if n := len(f.aud.chainedOf(CategoryRunAutoDriven)); n != 0 {
		t.Fatalf("run_auto_driven rows = %d, want 0", n)
	}
}

// TestRecordAct_HostDispatcherSourceMustBeTagged: a dispatcher subject with
// source drive_run gets 400 validation_failed and no row. Counterfactual run
// by hand: deleting the source check appended a row (200), RED.
func TestRecordAct_HostDispatcherSourceMustBeTagged(t *testing.T) {
	f := newHDFixture(t)
	tok := f.mint(hdSubject, hostdispatcher.Scopes()...)
	w := f.recordAct(tok, "drive_run")
	if w.Code != http.StatusBadRequest || hdErrCode(t, w) != "validation_failed" {
		t.Fatalf("untagged source = %d %s, want 400 validation_failed", w.Code, w.Body.String())
	}
	if n := len(f.aud.chainedOf(CategoryRunAutoDriven)); n != 0 {
		t.Fatalf("run_auto_driven rows = %d, want 0", n)
	}
	// A non-dispatcher operator keeps any non-empty source (rule unchanged).
	op := f.mint("github:octocat", "write:approvals", "write:runs")
	if w := f.recordAct(op, "drive_run"); w.Code != http.StatusOK {
		t.Fatalf("operator drive_run source = %d %s, want 200", w.Code, w.Body.String())
	}
}

// TestBearerAuth_HostDispatcherSubjectOnlyViaTokenStore (R10a): an AS-issued
// fho_ token whose record subject is host-dispatcher/devbox, audience
// matching, scopes write:runs, resolves to ANONYMOUS, so record-act is 401 with
// no row. Without the post-resolution guard the OAuth branch would carry the
// subject and the admission branch would accept it. Counterfactual run by
// hand: deleting the guard returned 200 and appended a row, RED; mutating the
// guard to reset EVERY dispatcher identity turned the token-store half RED.
func TestBearerAuth_HostDispatcherSubjectOnlyViaTokenStore(t *testing.T) {
	f := newHDFixture(t)
	const fho = "fho_hostdispatcherhostdispatcher0001"
	f.oauth.seedAccessToken(fho, hdSubject, testResource, "", []string{"write:runs", "read:runs"})

	w := f.recordAct(fho, hostdispatcher.SourceTag)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("fho_ dispatcher subject = %d %s, want 401 (anonymous)", w.Code, w.Body.String())
	}
	if n := len(f.aud.chainedOf(CategoryRunAutoDriven)); n != 0 {
		t.Fatalf("run_auto_driven rows = %d, want 0", n)
	}
	id, _, _ := driveBearerToken(f.s, f.tokens, nil, fho)
	if !id.IsAnonymous() {
		t.Fatalf("fho_ dispatcher identity = %+v, want anonymous", id)
	}

	// A non-dispatcher fho_ token still resolves (the guard is dispatcher-only).
	const fhoHuman = "fho_humanhumanhumanhumanhumanhuman01"
	f.oauth.seedAccessToken(fhoHuman, "github:octocat", testResource, "", []string{"read:runs"})
	if hid, _, _ := driveBearerToken(f.s, f.tokens, nil, fhoHuman); hid.Subject != "github:octocat" {
		t.Fatalf("human fho_ identity = %+v, want github:octocat", hid)
	}

	// The token-store dispatcher token is still admitted.
	tok := f.mint(hdSubject, hostdispatcher.Scopes()...)
	if w := f.recordAct(tok, hostdispatcher.SourceTag); w.Code != http.StatusOK {
		t.Fatalf("token-store dispatcher = %d %s, want 200", w.Code, w.Body.String())
	}
}

// hdDriveBearerAt runs bearerAuth alone for method + path and returns the
// identity the next handler saw (empty when bearerAuth answered itself).
func hdDriveBearerAt(s *Server, tok, method, path string) (Identity, int, string) {
	var captured Identity
	h := s.bearerAuth(s.cfg.APITokenRepo, nil, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = IdentityFrom(r.Context())
	}))
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return captured, rec.Code, rec.Body.String()
}

// --- F4 + FO ruling: the dispatcher's reach is the route allowlist ---

// hdWantAllowlist is the pinned host-dispatcher route allowlist
// (hostdispatcher.Routes(), documented in backend/internal/hostdispatcher/
// README.md): exactly the routes `fishhawk-mcp host-dispatch` calls. Widening
// it turns TestHostDispatcherProfile_RouteReach RED: update this list, the
// README and the dispatcher's request pin
// (mcpserver TestHostDispatcher_RequestsStayOnServerAllowlist) together.
var hdWantAllowlist = []string{
	"GET /v0/runs",
	"GET /v0/runs/{run_id}",
	"GET /v0/runs/{run_id}/stages",
	"POST /v0/runs/{run_id}/auto-drive/acts",
	"POST /v0/runs/{run_id}/stages/{stage_id}/host-dispatch",
	"POST /v0/runs/{run_id}/stages/{stage_id}/reap-failure",
	"POST /v0/stages/{stage_id}/acceptance-admission",
}

var handleFuncRE = regexp.MustCompile(`mux\.HandleFunc\("((?:GET|POST|PUT|PATCH|DELETE) [^"]+)"`)
var pathParamRE = regexp.MustCompile(`\{([a-z_]+)\}`)

// TestHostDispatcherProfile_RouteReach (F4, FO fix-up ruling on 3fad0a0c /
// 939bae05) drives EVERY route registered in handlers.go, reads and writes,
// with a host-dispatcher token hand-minted with the full operator set, and
// asserts each one: an allowlisted route lets the dispatcher past bearerAuth
// and its scope gate (no 401/403); every other route answers 403
// host_dispatcher_route_forbidden. There is no unasserted bucket, so a new
// route cannot sit unclassified, and recover, the scope-amendment and
// crew-escalation decisions, convert-to-concern, specialist admission,
// concurrency promote and cancel are all pinned refused. Counterfactual run
// by hand: deleting the allowlist check in bearerAuth turned all 176
// non-allowlisted rows RED (GET /healthz answered 200, cancel 200, ...).
func TestHostDispatcherProfile_RouteReach(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	if got := hostdispatcher.Routes(); !slices.Equal(sortedCopy(got), sortedCopy(hdWantAllowlist)) {
		t.Fatalf("hostdispatcher.Routes() changed; update hdWantAllowlist, the hostdispatcher README and the mcpserver request pin together.\n got %v\nwant %v", got, hdWantAllowlist)
	}
	f := newHDFixture(t)
	dispatcher := f.mint(hdSubject, operatorProbeScopes...)

	seen := map[string]bool{}
	var admitted []string
	forbidden := 0
	for _, m := range handleFuncRE.FindAllStringSubmatch(string(src), -1) {
		route := m[1]
		if seen[route] {
			continue
		}
		seen[route] = true
		method, pattern, _ := strings.Cut(route, " ")
		path := pathParamRE.ReplaceAllStringFunc(pattern, func(p string) string {
			switch p {
			case "{run_id}", "{id}":
				if strings.HasPrefix(pattern, "/v0/runs/") {
					return f.runID.String()
				}
			case "{sequence}":
				return "1"
			case "{owner}", "{name}":
				return "probe"
			}
			return uuid.New().String()
		})
		path = strings.TrimSuffix(path, "{$}")
		w := f.do(method, path, dispatcher, `{}`)
		if slices.Contains(hdWantAllowlist, route) {
			admitted = append(admitted, route)
			if w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
				t.Errorf("allowlisted %s refused the dispatcher: %d %s", route, w.Code, w.Body.String())
			}
			continue
		}
		if w.Code != http.StatusForbidden || hdErrCode(t, w) != "host_dispatcher_route_forbidden" {
			t.Errorf("%s = %d %s; want 403 host_dispatcher_route_forbidden (not on the allowlist)", route, w.Code, w.Body.String())
			continue
		}
		forbidden++
	}
	sort.Strings(admitted)
	if !slices.Equal(admitted, sortedCopy(hdWantAllowlist)) {
		t.Errorf("allowlisted routes registered in handlers.go = %v; want every allowlist entry registered: %v", admitted, sortedCopy(hdWantAllowlist))
	}
	for _, must := range []string{
		"POST /v0/runs",
		"POST /v0/runs/{run_id}/recover",
		"POST /v0/runs/{run_id}/scope-amendments/{amendment_id}/decision",
		"POST /v0/crew-messages/{sequence}/escalation-decision",
		"POST /v0/crew-messages/{sequence}/convert-to-concern",
		"POST /v0/specialist-jobs/{job_id}/admission",
		"POST /v0/stages/{stage_id}/concurrency/promote",
		"POST /v0/runs/{run_id}/stages/{stage_id}/progress",
		"POST /v0/runs/{run_id}/cancel",
	} {
		if !seen[must] {
			t.Errorf("%s is no longer registered in handlers.go; re-check this pin", must)
		}
	}
	t.Logf("%d routes forbidden to the dispatcher, %d allowlisted", forbidden, len(admitted))
}

// TestHostDispatcherAllowlist_RefusesBeforeHandler reads committed state: a
// dispatcher's POST /v0/runs/{id}/cancel (write:runs, which the profile holds)
// leaves the run running, while an operator token with the same scope cancels
// it. Counterfactual run by hand: deleting the allowlist check in bearerAuth
// cancelled the run on the dispatcher's request, RED.
func TestHostDispatcherAllowlist_RefusesBeforeHandler(t *testing.T) {
	f := newHDFixture(t)
	dispatcher := f.mint(hdSubject, hostdispatcher.Scopes()...)
	path := "/v0/runs/" + f.runID.String() + "/cancel"
	w := f.do(http.MethodPost, path, dispatcher, `{"reason":"probe"}`)
	if w.Code != http.StatusForbidden || hdErrCode(t, w) != "host_dispatcher_route_forbidden" {
		t.Fatalf("dispatcher cancel = %d %s; want 403 host_dispatcher_route_forbidden", w.Code, w.Body.String())
	}
	state := func() run.State {
		f.runs.mu.Lock()
		defer f.runs.mu.Unlock()
		return f.runs.runs[f.runID].State
	}
	if got := state(); got != run.StateRunning {
		t.Fatalf("run state after the refused dispatcher cancel = %s; want running", got)
	}
	op := f.mint("github:octocat", "write:runs", "read:runs")
	if w := f.do(http.MethodPost, path, op, `{"reason":"probe"}`); w.Code >= 300 {
		t.Fatalf("operator cancel = %d %s; want success (the refusal is subject-specific)", w.Code, w.Body.String())
	}
	if got := state(); got != run.StateCancelled {
		t.Fatalf("run state after the operator cancel = %s; want cancelled", got)
	}
}

// operatorProbeScopes is the operator default the reach probe hand-mints the
// dispatcher token with, so a missing clamp would widen every row.
var operatorProbeScopes = []string{
	"read:runs", "read:audit", "write:runs", "write:approvals", "write:stages",
	"write:deploy", "write:campaigns", "read:audit-export",
}
