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
// WITH write:approvals still cannot approve a plan — 403 insufficient_scope and
// NO approval row. Counterfactual run by hand: deleting the ClampScopes call in
// bearerAuth let the record's own write:approvals through and the approval row
// landed, RED.
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
	if w.Code != http.StatusForbidden || hdErrCode(t, w) != "insufficient_scope" {
		t.Fatalf("approve with a dispatcher token = %d %s, want 403 insufficient_scope", w.Code, w.Body.String())
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
	id, status, body := driveBearerToken(f.s, f.tokens, nil, tok)
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

// --- F4: the profile's reach over every mutating route ---

// hdAdmittedRoutes is the ACCEPTED ADR-104 Q4 residual (documented in
// backend/internal/hostdispatcher/README.md): the mutating routes whose scope
// gate admits a host-dispatcher token holding the ratified profile
// (read:runs, read:audit, write:runs, write:stages). A route here refuses a
// read-only control token 403 insufficient_scope and lets the dispatcher past
// that gate. Adding a route to this set (or a new write:runs / write:stages
// route) turns TestHostDispatcherProfile_RouteReach RED: update this list AND
// the README residual table together.
var hdAdmittedRoutes = []string{
	"POST /v0/concerns/{concern_id}/defer",
	"POST /v0/concerns/{concern_id}/waive",
	"POST /v0/releases/cut",
	"POST /v0/releases/notes",
	"POST /v0/releases/publish",
	"POST /v0/runs/{run_id}/auto-drive/acts",
	"POST /v0/runs/{run_id}/cancel",
	"POST /v0/runs/{run_id}/concerns/waive",
	"POST /v0/runs/{run_id}/consolidate",
	"POST /v0/runs/{run_id}/divergence/{sequence}/answer",
	"POST /v0/runs/{run_id}/escapes",
	"POST /v0/runs/{run_id}/integrate-wave",
	"POST /v0/runs/{run_id}/merge-queue/promote",
	"POST /v0/runs/{run_id}/merge-queue/withdraw",
	"POST /v0/runs/{run_id}/product-reports",
	"POST /v0/runs/{run_id}/rebase-branch",
	"POST /v0/runs/{run_id}/reconcile-merge",
	"POST /v0/runs/{run_id}/record-merge-observation",
	"POST /v0/runs/{run_id}/recover",
	"POST /v0/runs/{run_id}/redrive",
	"POST /v0/runs/{run_id}/reset-branch",
	"POST /v0/runs/{run_id}/retrigger-ci",
	"POST /v0/runs/{run_id}/reviews/reconcile",
	"POST /v0/runs/{run_id}/reviews/rerun",
	"POST /v0/runs/{run_id}/revive",
	"POST /v0/runs/{run_id}/scope-completeness/decision",
	"POST /v0/runs/{run_id}/stages/{stage_id}/host-dispatch",
	"POST /v0/runs/{run_id}/stages/{stage_id}/reap-failure",
	"POST /v0/runs/{run_id}/status-comment",
	"POST /v0/runs/{run_id}/vouch-commit",
	"POST /v0/stages/{stage_id}/acceptance-admission",
	"POST /v0/stages/{stage_id}/fixup",
	"POST /v0/stages/{stage_id}/retry",
	"POST /v0/work-items",
}

// hdRefusedRoutes are the scope-gated mutating routes the dispatcher token is
// refused on (403 insufficient_scope, or a named agent refusal).
var hdRefusedRoutes = []string{
	"PATCH /v0/refinement/sessions/{session_id}/draft",
	"POST /v0/campaigns",
	"POST /v0/campaigns/preview",
	"POST /v0/captain/accept",
	"POST /v0/captain/claim",
	"POST /v0/captain/offer",
	"POST /v0/captain/relinquish",
	"POST /v0/captain/withdraw",
	"POST /v0/concerns/{concern_id}/label",
	"POST /v0/digest/mark-read",
	"POST /v0/first-officer-seats",
	"POST /v0/first-officer-seats/retire",
	"POST /v0/refinement/sessions",
	"POST /v0/refinement/sessions/{session_id}/decision",
	"POST /v0/refinement/sessions/{session_id}/file",
	"POST /v0/repos/{owner}/{name}/delegation/confirm",
	"POST /v0/repos/{owner}/{name}/delegation/lower",
	"POST /v0/runs/{run_id}/acceptance-arbitration",
	"POST /v0/runs/{run_id}/auto-drive",
	"POST /v0/runs/{run_id}/concerns/resolve",
	"POST /v0/runs/{run_id}/deployment/rollback",
	"POST /v0/runs/{run_id}/merge",
	"POST /v0/stages/{stage_id}/approvals",
	"POST /v0/stages/{stage_id}/clarification",
	"POST /v0/stages/{stage_id}/revise",
}

// hdSiblingSliceRoutes are classified but NOT pinned here: slice 1 of the same
// parent plan (E88.2 / #4526) refuses a host-dispatcher subject on POST
// /v0/runs with 403 host_dispatcher_cannot_start_runs and pins that in
// runs_create_test.go TestCreateRun_HostDispatcherSubjectRefused, so the route
// moves from admitted to refused when that slice lands.
var hdSiblingSliceRoutes = map[string]bool{"POST /v0/runs": true}

var handleFuncRE = regexp.MustCompile(`mux\.HandleFunc\("((?:POST|PUT|PATCH|DELETE) [^"]+)"`)
var pathParamRE = regexp.MustCompile(`\{([a-z_]+)\}`)

// TestHostDispatcherProfile_RouteReach (F4) drives EVERY mutating route
// registered in handlers.go with (a) a read-only control token and (b) a
// host-dispatcher token hand-minted with the full operator set (so the clamp
// is exercised on every route too), and pins which routes admit the
// dispatcher. Classification per route:
//
//   - admitted: the control is refused 403 insufficient_scope and the
//     dispatcher is not (it passed the scope gate);
//   - refused: both are refused (403) — the route needs a scope or subject the
//     profile withholds;
//   - not scope-gated on this probe: the control is not refused by scope (the
//     route has no scope gate, or validation answers first) — reported, not
//     pinned, so a fixture change cannot silently move a route.
func TestHostDispatcherProfile_RouteReach(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	f := newHDFixture(t)
	control := f.mint("github:probe-control", "read:runs", "read:audit")
	dispatcher := f.mint(hdSubject, operatorProbeScopes...)

	var admitted, refused, ungated []string
	seen := map[string]bool{}
	for _, m := range handleFuncRE.FindAllStringSubmatch(string(src), -1) {
		route := m[1]
		if seen[route] || hdSiblingSliceRoutes[route] {
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
			}
			return uuid.New().String()
		})
		path = strings.TrimSuffix(path, "{$}")
		cw := f.do(method, path, control, `{}`)
		dw := f.do(method, path, dispatcher, `{}`)
		controlScopeRefused := cw.Code == http.StatusForbidden && hdErrCode(t, cw) == "insufficient_scope"
		switch {
		case !controlScopeRefused:
			ungated = append(ungated, route)
		case dw.Code == http.StatusForbidden || dw.Code == http.StatusUnauthorized:
			refused = append(refused, route)
		default:
			admitted = append(admitted, route)
		}
	}
	sort.Strings(admitted)
	sort.Strings(refused)
	sort.Strings(ungated)
	t.Logf("not scope-gated on this probe (%d): %s", len(ungated), strings.Join(ungated, "\n  "))
	if !slices.Equal(admitted, sortedCopy(hdAdmittedRoutes)) {
		t.Errorf("routes admitting a host-dispatcher token changed; update hdAdmittedRoutes AND the hostdispatcher README residual.\n got (%d):\n  %s\nwant (%d):\n  %s",
			len(admitted), strings.Join(admitted, "\n  "), len(hdAdmittedRoutes), strings.Join(sortedCopy(hdAdmittedRoutes), "\n  "))
	}
	if !slices.Equal(refused, sortedCopy(hdRefusedRoutes)) {
		t.Errorf("routes refusing a host-dispatcher token changed; update hdRefusedRoutes.\n got (%d):\n  %s\nwant (%d):\n  %s",
			len(refused), strings.Join(refused, "\n  "), len(hdRefusedRoutes), strings.Join(sortedCopy(hdRefusedRoutes), "\n  "))
	}
	if !slices.Contains(admitted, "POST /v0/runs/{run_id}/auto-drive/acts") {
		t.Errorf("record-act must admit the dispatcher (its one write:runs act)")
	}
}

// operatorProbeScopes is the operator default the reach probe hand-mints the
// dispatcher token with, so a missing clamp would widen every row.
var operatorProbeScopes = []string{
	"read:runs", "read:audit", "write:runs", "write:approvals", "write:stages",
	"write:deploy", "write:campaigns", "read:audit-export",
}
