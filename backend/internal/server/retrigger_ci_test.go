package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// retriggerGitHubFake is the httptest GitHub the CI re-trigger tests drive. It
// serves the three endpoints handleRetriggerCI is allowed to touch and
// RECORDS EVERY REQUEST (method + path), so a test can assert both what was
// re-run and that nothing else — no PR write, no ref write, no commit — was
// ever sent.
type retriggerGitHubFake struct {
	mu       sync.Mutex
	requests []string

	// prStatus, when non-zero, answers the PR read with that status.
	prStatus int
	prState  string // default "open"
	prHead   string // default "aaa"; "-" serves an empty head sha
	// runsStatus, when non-zero, answers the workflow-runs listing with it.
	runsStatus int
	runs       []map[string]any
	// rerunStatus maps a workflow run id to the status its rerun POST
	// answers; absent ids answer 201.
	rerunStatus map[int64]int
}

func (f *retriggerGitHubFake) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// reruns returns the workflow run ids a rerun POST was recorded for.
func (f *retriggerGitHubFake) reruns() []string {
	var out []string
	for _, r := range f.recorded() {
		if strings.HasPrefix(r, "POST ") && strings.HasSuffix(r, "/rerun") {
			out = append(out, r)
		}
	}
	return out
}

func newRetriggerGitHub(t *testing.T, f *retriggerGitHubFake) *githubclient.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/x/y/pulls/7":
			if f.prStatus != 0 {
				w.WriteHeader(f.prStatus)
				return
			}
			state := f.prState
			if state == "" {
				state = "open"
			}
			head := f.prHead
			switch head {
			case "":
				head = "aaa"
			case "-":
				head = ""
			}
			_, _ = fmt.Fprintf(w, `{"node_id":"PR_x","state":%q,"head":{"sha":%q,"ref":"run/b"},"base":{"ref":"main"}}`, state, head)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/x/y/actions/runs":
			if f.runsStatus != 0 {
				w.WriteHeader(f.runsStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(f.runs), "workflow_runs": f.runs})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/repos/x/y/actions/runs/") &&
			strings.HasSuffix(r.URL.Path, "/rerun"):
			status := http.StatusCreated
			for id, st := range f.rerunStatus {
				if r.URL.Path == fmt.Sprintf("/repos/x/y/actions/runs/%d/rerun", id) {
					status = st
				}
			}
			w.WriteHeader(status)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
}

// wfRun renders one workflow run of the listing.
func wfRun(id int64, event, status, conclusion string) map[string]any {
	m := map[string]any{"id": id, "event": event, "status": status, "head_sha": "aaa",
		"html_url": fmt.Sprintf("https://github.com/x/y/actions/runs/%d", id)}
	if conclusion != "" {
		m["conclusion"] = conclusion
	} else {
		m["conclusion"] = nil
	}
	return m
}

type retriggerFixture struct {
	s      *Server
	au     *auditFake
	rr     *promptRunRepo
	gh     *retriggerGitHubFake
	runID  uuid.UUID
	runRow *run.Run
}

// newRetriggerFixture seeds a running GitHub run with an open PR #7 whose head
// carries one failed pull_request CI run (id 11) unless the caller overrides
// gh.runs.
func newRetriggerFixture(t *testing.T, mutate func(*run.Run, *retriggerGitHubFake)) retriggerFixture {
	t.Helper()
	gh := &retriggerGitHubFake{runs: []map[string]any{wfRun(11, "pull_request", "completed", "failure")}}
	runID := uuid.New()
	prURL := "https://github.com/x/y/pull/7"
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning, InstallationID: instID(99), PullRequestURL: &prURL}
	if mutate != nil {
		mutate(runRow, gh)
	}
	stage := &run.Stage{ID: uuid.New(), RunID: runID, Type: run.StageTypeReview}
	s, _, au, rr := newLineageServer(t, newRetriggerGitHub(t, gh), runRow, stage)
	return retriggerFixture{s: s, au: au, rr: rr, gh: gh, runID: runID, runRow: runRow}
}

func postRetriggerCI(t *testing.T, s *Server, runID string, withID func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID+"/retrigger-ci", nil)
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	s.handleRetriggerCI(w, withID(req))
	return w
}

// retriggerAudit returns every ci_retriggered entry the audit fake recorded.
func retriggerAudit(au *auditFake) []audit.ChainAppendParams {
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []audit.ChainAppendParams
	for _, a := range au.appended {
		if a.Category == CategoryCIRetriggered {
			out = append(out, a)
		}
	}
	return out
}

func decodeRetrigger(t *testing.T, w *httptest.ResponseRecorder) retriggerCIResponse {
	t.Helper()
	var resp retriggerCIResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, w.Body.String())
	}
	return resp
}

func wantErrorCode(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d:\n%s", w.Code, status, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"`+code+`"`) {
		t.Fatalf("body missing %s: %s", code, w.Body.String())
	}
}

// TestRetriggerCI_HappyPath re-runs exactly the completed, non-successful CI
// runs at the PR head, reports every other run as skipped with a reason, and
// records one ci_retriggered entry. The recorded request list proves the
// "no PR-state change" done-means: only GET pulls/7, GET actions/runs and the
// rerun POSTs were sent — no PATCH/PUT/DELETE, no git/refs or git/commits write.
func TestRetriggerCI_HappyPath(t *testing.T) {
	f := newRetriggerFixture(t, func(_ *run.Run, gh *retriggerGitHubFake) {
		gh.runs = []map[string]any{
			wfRun(11, "pull_request", "completed", "failure"),
			wfRun(12, "push", "completed", "cancelled"),
			wfRun(13, "pull_request_target", "completed", "timed_out"),
			wfRun(14, "workflow_dispatch", "completed", "failure"),
			wfRun(15, "pull_request", "in_progress", ""),
			wfRun(16, "pull_request", "completed", "success"),
			wfRun(17, "schedule", "completed", "failure"),
		}
	})
	w := postRetriggerCI(t, f.s, f.runID.String(), withVouchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	resp := decodeRetrigger(t, w)
	if resp.RunID != f.runID.String() || resp.PRURL != "https://github.com/x/y/pull/7" || resp.HeadSHA != "aaa" {
		t.Errorf("response header fields = %+v", resp)
	}
	var rerun []int64
	for _, r := range resp.Rerun {
		rerun = append(rerun, r.ID)
	}
	if fmt.Sprint(rerun) != "[11 12 13]" {
		t.Errorf("rerun = %v, want [11 12 13]", rerun)
	}
	skipped := map[int64]string{}
	for _, s := range resp.Skipped {
		skipped[s.ID] = s.Reason
	}
	wantSkipped := map[int64]string{
		14: retriggerSkipEventExcluded, 15: retriggerSkipNotCompleted,
		16: retriggerSkipSucceeded, 17: retriggerSkipEventExcluded,
	}
	if fmt.Sprint(skipped) != fmt.Sprint(wantSkipped) {
		t.Errorf("skipped = %v, want %v", skipped, wantSkipped)
	}
	if len(resp.Failed) != 0 {
		t.Errorf("failed = %+v, want none", resp.Failed)
	}

	wantRequests := []string{
		"GET /repos/x/y/pulls/7",
		"GET /repos/x/y/actions/runs",
		"POST /repos/x/y/actions/runs/11/rerun",
		"POST /repos/x/y/actions/runs/12/rerun",
		"POST /repos/x/y/actions/runs/13/rerun",
	}
	got := f.gh.recorded()
	if strings.Join(got, "\n") != strings.Join(wantRequests, "\n") {
		t.Errorf("recorded requests =\n%s\nwant exactly\n%s", strings.Join(got, "\n"), strings.Join(wantRequests, "\n"))
	}
	for _, r := range got {
		if strings.HasPrefix(r, "PATCH ") || strings.HasPrefix(r, "PUT ") || strings.HasPrefix(r, "DELETE ") ||
			strings.Contains(r, "/git/refs") || strings.Contains(r, "/git/commits") {
			t.Errorf("handler sent a PR/ref/commit write: %s", r)
		}
	}

	entries := retriggerAudit(f.au)
	if len(entries) != 1 {
		t.Fatalf("ci_retriggered entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.RunID != f.runID || e.ActorKind == nil || *e.ActorKind != audit.ActorUser ||
		e.ActorSubject == nil || *e.ActorSubject != "github:ops" {
		t.Errorf("audit actor/run = %+v", e)
	}
	var payload struct {
		PRURL   string           `json:"pr_url"`
		HeadSHA string           `json:"head_sha"`
		Rerun   []int64          `json:"rerun"`
		Skipped []retriggerCIRun `json:"skipped"`
		Failed  []retriggerCIRun `json:"failed"`
	}
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.PRURL != "https://github.com/x/y/pull/7" || payload.HeadSHA != "aaa" ||
		fmt.Sprint(payload.Rerun) != "[11 12 13]" || len(payload.Skipped) != 4 || len(payload.Failed) != 0 {
		t.Errorf("payload = %+v", payload)
	}
}

// TestRetriggerCI_WorkflowDispatchExcluded is the named criterion test for the
// workflow_dispatch exclusion (approval condition C3): a completed, failed
// workflow_dispatch run (a Fishhawk runner dispatch) and a completed, failed
// pull_request run share the head; only the pull_request run is re-run.
//
// Counterfactual: delete the event allow-list check — the fake records a rerun
// POST for the dispatch run (id 21), RED.
func TestRetriggerCI_WorkflowDispatchExcluded(t *testing.T) {
	f := newRetriggerFixture(t, func(_ *run.Run, gh *retriggerGitHubFake) {
		gh.runs = []map[string]any{
			wfRun(21, "workflow_dispatch", "completed", "failure"),
			wfRun(22, "pull_request", "completed", "failure"),
		}
	})
	w := postRetriggerCI(t, f.s, f.runID.String(), withVouchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	reruns := f.gh.reruns()
	if strings.Join(reruns, ",") != "POST /repos/x/y/actions/runs/22/rerun" {
		t.Fatalf("rerun POSTs = %v, want only run 22 (the workflow_dispatch run 21 must never be re-run)", reruns)
	}
	resp := decodeRetrigger(t, w)
	if len(resp.Skipped) != 1 || resp.Skipped[0].ID != 21 || resp.Skipped[0].Reason != retriggerSkipEventExcluded {
		t.Errorf("skipped = %+v, want run 21 event_excluded", resp.Skipped)
	}
}

// TestRetriggerCI_PullRequestNotOpen is the named criterion test for the 409
// pull_request_not_open refusal (approval condition C3): a closed PR with a
// rerunnable run at its head is refused, and nothing is re-run or recorded.
//
// Counterfactual: delete the PR-open check — a rerun POST is recorded, RED.
func TestRetriggerCI_PullRequestNotOpen(t *testing.T) {
	f := newRetriggerFixture(t, func(_ *run.Run, gh *retriggerGitHubFake) { gh.prState = "closed" })
	w := postRetriggerCI(t, f.s, f.runID.String(), withVouchOperator)
	wantErrorCode(t, w, http.StatusConflict, "pull_request_not_open")
	if !strings.Contains(w.Body.String(), "10 minutes") {
		t.Errorf("refusal does not name the reopen-revive window: %s", w.Body.String())
	}
	if r := f.gh.reruns(); len(r) != 0 {
		t.Errorf("rerun POSTs = %v, want none for a closed PR", r)
	}
	if n := len(retriggerAudit(f.au)); n != 0 {
		t.Errorf("ci_retriggered entries = %d, want 0", n)
	}
}

// TestRetriggerCI_RunTokenForbidden: a run-bound agent token — even for its own
// run and even carrying write:stages — is refused before any forge call.
//
// Counterfactual: delete the run-token refusal — the identity's write:stages
// then passes the scope gate, the handler reaches the GitHub fake and a rerun
// POST is recorded, RED.
func TestRetriggerCI_RunTokenForbidden(t *testing.T) {
	f := newRetriggerFixture(t, nil)
	withRunToken := func(req *http.Request) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
			Subject: "mcp:run:" + f.runID.String(), TokenID: "tok-agent",
			Scopes: []string{"mcp:read", "write:stages"},
		}))
	}
	w := postRetriggerCI(t, f.s, f.runID.String(), withRunToken)
	wantErrorCode(t, w, http.StatusForbidden, "run_token_forbidden")
	if r := f.gh.recorded(); len(r) != 0 {
		t.Errorf("forge requests = %v, want none", r)
	}
}

// TestRetriggerCI_MissingScope: an operator identity without write:stages is
// refused before any forge call.
func TestRetriggerCI_MissingScope(t *testing.T) {
	f := newRetriggerFixture(t, nil)
	withScopeless := func(req *http.Request) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
			Subject: "github:ops", TokenID: "tok-x", Scopes: []string{"read:runs"},
		}))
	}
	w := postRetriggerCI(t, f.s, f.runID.String(), withScopeless)
	wantErrorCode(t, w, http.StatusForbidden, "insufficient_scope")
	if r := f.gh.recorded(); len(r) != 0 {
		t.Errorf("forge requests = %v, want none", r)
	}
}

func TestRetriggerCI_Anonymous(t *testing.T) {
	f := newRetriggerFixture(t, nil)
	w := postRetriggerCI(t, f.s, f.runID.String(), func(r *http.Request) *http.Request { return r })
	wantErrorCode(t, w, http.StatusUnauthorized, "authentication_required")
	if r := f.gh.recorded(); len(r) != 0 {
		t.Errorf("forge requests = %v, want none", r)
	}
}

func TestRetriggerCI_BadRunID(t *testing.T) {
	f := newRetriggerFixture(t, nil)
	w := postRetriggerCI(t, f.s, "not-a-uuid", withVouchOperator)
	wantErrorCode(t, w, http.StatusBadRequest, "validation_failed")
}

// TestRetriggerCI_Unconfigured: each missing dependency answers 503
// retrigger_unconfigured.
func TestRetriggerCI_Unconfigured(t *testing.T) {
	gh := newRetriggerGitHub(t, &retriggerGitHubFake{})
	for name, cfg := range map[string]Config{
		"no github":     {Addr: "127.0.0.1:0", RunRepo: newPromptRunRepo(), AuditRepo: newAuditFake()},
		"no audit repo": {Addr: "127.0.0.1:0", RunRepo: newPromptRunRepo(), GitHub: gh},
		"no run repo":   {Addr: "127.0.0.1:0", AuditRepo: newAuditFake(), GitHub: gh},
	} {
		t.Run(name, func(t *testing.T) {
			w := postRetriggerCI(t, New(cfg), uuid.NewString(), withVouchOperator)
			wantErrorCode(t, w, http.StatusServiceUnavailable, "retrigger_unconfigured")
		})
	}
}

func TestRetriggerCI_RunNotFound(t *testing.T) {
	f := newRetriggerFixture(t, nil)
	w := postRetriggerCI(t, f.s, uuid.NewString(), withVouchOperator)
	wantErrorCode(t, w, http.StatusNotFound, "run_not_found")
}

func TestRetriggerCI_GetRunError(t *testing.T) {
	f := newRetriggerFixture(t, nil)
	f.rr.runErr = errors.New("db down")
	w := postRetriggerCI(t, f.s, f.runID.String(), withVouchOperator)
	wantErrorCode(t, w, http.StatusInternalServerError, "internal_error")
	if r := f.gh.recorded(); len(r) != 0 {
		t.Errorf("forge requests = %v, want none", r)
	}
}

// TestRetriggerCI_RunHasNoPullRequest: no tracked PR (nil URL, and a URL with
// no /pull/<n> number) → 409 before any forge call.
func TestRetriggerCI_RunHasNoPullRequest(t *testing.T) {
	noNumber := "https://github.com/x/y"
	for name, url := range map[string]*string{"nil url": nil, "url without a pr number": &noNumber} {
		t.Run(name, func(t *testing.T) {
			f := newRetriggerFixture(t, func(rn *run.Run, _ *retriggerGitHubFake) { rn.PullRequestURL = url })
			w := postRetriggerCI(t, f.s, f.runID.String(), withVouchOperator)
			wantErrorCode(t, w, http.StatusConflict, "run_has_no_pull_request")
			if r := f.gh.recorded(); len(r) != 0 {
				t.Errorf("forge requests = %v, want none", r)
			}
		})
	}
}

// TestRetriggerCI_UnsupportedForge: a run with no GitHub installation (the
// GitLab shape) is refused 422 before any forge call.
func TestRetriggerCI_UnsupportedForge(t *testing.T) {
	for name, inst := range map[string]*int64{"nil installation": nil, "zero installation": instID(0)} {
		t.Run(name, func(t *testing.T) {
			f := newRetriggerFixture(t, func(rn *run.Run, _ *retriggerGitHubFake) { rn.InstallationID = inst })
			w := postRetriggerCI(t, f.s, f.runID.String(), withVouchOperator)
			wantErrorCode(t, w, http.StatusUnprocessableEntity, "retrigger_unsupported_forge")
			if r := f.gh.recorded(); len(r) != 0 {
				t.Errorf("forge requests = %v, want none", r)
			}
		})
	}
}

func TestRetriggerCI_UnparseableRepo(t *testing.T) {
	f := newRetriggerFixture(t, func(rn *run.Run, _ *retriggerGitHubFake) { rn.Repo = "no-slash" })
	w := postRetriggerCI(t, f.s, f.runID.String(), withVouchOperator)
	wantErrorCode(t, w, http.StatusInternalServerError, "internal_error")
	if r := f.gh.recorded(); len(r) != 0 {
		t.Errorf("forge requests = %v, want none", r)
	}
}

// TestRetriggerCI_ForgeError covers the three 502 forge_error arms: the PR read
// fails, the PR carries no head sha, and the workflow-runs listing fails. None
// re-runs anything.
// Each arm asserts its OWN message, so a deleted arm cannot hide behind a
// later forge_error (an empty sha would otherwise fail the listing instead).
func TestRetriggerCI_ForgeError(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*run.Run, *retriggerGitHubFake)
		msg    string
	}{
		"pr read fails":  {func(_ *run.Run, gh *retriggerGitHubFake) { gh.prStatus = http.StatusNotFound }, "reading the run's pull request failed"},
		"empty head sha": {func(_ *run.Run, gh *retriggerGitHubFake) { gh.prHead = "-" }, "returned an empty head sha"},
		"list fails":     {func(_ *run.Run, gh *retriggerGitHubFake) { gh.runsStatus = http.StatusForbidden }, "listing the workflow runs"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRetriggerFixture(t, tc.mutate)
			w := postRetriggerCI(t, f.s, f.runID.String(), withVouchOperator)
			wantErrorCode(t, w, http.StatusBadGateway, "forge_error")
			if !strings.Contains(w.Body.String(), tc.msg) {
				t.Errorf("body = %s, want the %q arm", w.Body.String(), tc.msg)
			}
			if r := f.gh.reruns(); len(r) != 0 {
				t.Errorf("rerun POSTs = %v, want none", r)
			}
		})
	}
}

// TestRetriggerCI_NoCIRunsAtHead: only non-CI runs (a workflow_dispatch and a
// schedule run) exist at the head → 409 naming the push + vouch remediation.
func TestRetriggerCI_NoCIRunsAtHead(t *testing.T) {
	f := newRetriggerFixture(t, func(_ *run.Run, gh *retriggerGitHubFake) {
		gh.runs = []map[string]any{
			wfRun(31, "workflow_dispatch", "completed", "failure"),
			wfRun(32, "schedule", "completed", "failure"),
		}
	})
	w := postRetriggerCI(t, f.s, f.runID.String(), withVouchOperator)
	wantErrorCode(t, w, http.StatusConflict, "no_ci_runs_at_head")
	if !strings.Contains(w.Body.String(), "fishhawk_vouch_commit") {
		t.Errorf("refusal does not name the push + vouch remediation: %s", w.Body.String())
	}
	if r := f.gh.reruns(); len(r) != 0 {
		t.Errorf("rerun POSTs = %v, want none", r)
	}
	if n := len(retriggerAudit(f.au)); n != 0 {
		t.Errorf("ci_retriggered entries = %d, want 0", n)
	}
}

// TestRetriggerCI_AllFailed: every rerun POST answers 403 → 502
// retrigger_failed and NO ci_retriggered entry.
//
// Counterfactual: delete the all-failed rule — the handler appends a
// ci_retriggered entry, which this test reads back from the audit fake, RED.
func TestRetriggerCI_AllFailed(t *testing.T) {
	f := newRetriggerFixture(t, func(_ *run.Run, gh *retriggerGitHubFake) {
		gh.runs = []map[string]any{
			wfRun(41, "pull_request", "completed", "failure"),
			wfRun(42, "push", "completed", "failure"),
		}
		gh.rerunStatus = map[int64]int{41: http.StatusForbidden, 42: http.StatusForbidden}
	})
	w := postRetriggerCI(t, f.s, f.runID.String(), withVouchOperator)
	wantErrorCode(t, w, http.StatusBadGateway, "retrigger_failed")
	if r := f.gh.reruns(); len(r) != 2 {
		t.Errorf("rerun POSTs = %v, want both attempted", r)
	}
	if n := len(retriggerAudit(f.au)); n != 0 {
		t.Errorf("ci_retriggered entries = %d, want 0 when every re-run failed", n)
	}
}

// TestRetriggerCI_PartialFailure: one rerun answers 403 and one 201 → 200, the
// failed list names the refused run, and the audit entry is written.
func TestRetriggerCI_PartialFailure(t *testing.T) {
	f := newRetriggerFixture(t, func(_ *run.Run, gh *retriggerGitHubFake) {
		gh.runs = []map[string]any{
			wfRun(51, "pull_request", "completed", "failure"),
			wfRun(52, "push", "completed", "failure"),
		}
		gh.rerunStatus = map[int64]int{51: http.StatusForbidden}
	})
	w := postRetriggerCI(t, f.s, f.runID.String(), withVouchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	resp := decodeRetrigger(t, w)
	if len(resp.Rerun) != 1 || resp.Rerun[0].ID != 52 {
		t.Errorf("rerun = %+v, want run 52", resp.Rerun)
	}
	if len(resp.Failed) != 1 || resp.Failed[0].ID != 51 || resp.Failed[0].Error == "" {
		t.Errorf("failed = %+v, want run 51 with an error", resp.Failed)
	}
	if n := len(retriggerAudit(f.au)); n != 1 {
		t.Errorf("ci_retriggered entries = %d, want 1", n)
	}
}

// TestRetriggerCI_NothingEligible: every CI run at the head is in progress or
// succeeded → 200 with an empty rerun list, the skips reported, nothing
// POSTed and no audit entry (no re-run happened).
func TestRetriggerCI_NothingEligible(t *testing.T) {
	f := newRetriggerFixture(t, func(_ *run.Run, gh *retriggerGitHubFake) {
		gh.runs = []map[string]any{
			wfRun(61, "pull_request", "queued", ""),
			wfRun(62, "push", "completed", "success"),
		}
	})
	w := postRetriggerCI(t, f.s, f.runID.String(), withVouchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	resp := decodeRetrigger(t, w)
	if len(resp.Rerun) != 0 || len(resp.Failed) != 0 || len(resp.Skipped) != 2 {
		t.Errorf("response = %+v, want 0 rerun / 0 failed / 2 skipped", resp)
	}
	if !strings.Contains(w.Body.String(), `"rerun":[]`) {
		t.Errorf("rerun must encode as [] (never null): %s", w.Body.String())
	}
	if r := f.gh.reruns(); len(r) != 0 {
		t.Errorf("rerun POSTs = %v, want none", r)
	}
	if n := len(retriggerAudit(f.au)); n != 0 {
		t.Errorf("ci_retriggered entries = %d, want 0", n)
	}
}

// TestRetriggerCI_AuditAppendFails: the re-runs went out but the audit append
// failed → 500 naming the re-run ids, so the operator does not read it as a
// no-op.
func TestRetriggerCI_AuditAppendFails(t *testing.T) {
	f := newRetriggerFixture(t, nil)
	f.au.appendErrCategory = CategoryCIRetriggered
	w := postRetriggerCI(t, f.s, f.runID.String(), withVouchOperator)
	wantErrorCode(t, w, http.StatusInternalServerError, "internal_error")
	if !strings.Contains(w.Body.String(), "re-runs were requested") {
		t.Errorf("body does not say the re-runs went out: %s", w.Body.String())
	}
	if r := f.gh.reruns(); len(r) != 1 {
		t.Errorf("rerun POSTs = %v, want 1", r)
	}
}

// TestRetriggerCIRouteRegistered guards the route table: an anonymous POST to
// /v0/runs/{run_id}/retrigger-ci reaches the auth ladder (401) instead of the
// mux's 404/405.
func TestRetriggerCIRouteRegistered(t *testing.T) {
	s := New(Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+uuid.NewString()+"/retrigger-ci", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (route reaches the auth ladder):\n%s", rec.Code, rec.Body.String())
	}
}
