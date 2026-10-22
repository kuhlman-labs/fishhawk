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

// merge_overlap_guard_test.go pins the merge overlap guard (E83.61 / #4119):
// one behavioral test per refusal and fail-closed mode, the override and its
// record-before-commit contract, and the guard's placement ahead of the
// ADR-093 enqueue arm. Every refusal test reads COMMITTED state (no verdict
// row, no override row, no dispatch), not only the error code.

const overlapHead = "h0000000000000000000000000000000000000001"

// overlapGitHub serves the PR (head overlapHead, base main, clean), its empty
// review listing, and two compares: main...<head> (the guard's changed-path
// read, files configurable) and <head>...main (the merge-candidate gate's
// behind probe, zero commits).
type overlapGitHub struct {
	mu            sync.Mutex
	merged        bool
	prStatus      int
	files         []map[string]any
	compareStatus int
	prCompares    int
}

func (g *overlapGitHub) client(t *testing.T) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, _ *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.prStatus != 0 {
			w.WriteHeader(g.prStatus)
			return
		}
		state := "open"
		if g.merged {
			state = "closed"
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"node_id":"PR_x","state":%q,"merged":%v,"mergeable":true,"mergeable_state":"clean","head":{"sha":%q,"ref":"fishhawk/run-a"},"base":{"ref":"main"}}`,
			state, g.merged, overlapHead)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/reviews", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/compare/{basehead...}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.PathValue("basehead") != "main..."+overlapHead {
			_, _ = w.Write([]byte(`{"commits":[]}`))
			return
		}
		g.prCompares++
		if g.compareStatus != 0 {
			w.WriteHeader(g.compareStatus)
			return
		}
		files := g.files
		if files == nil {
			files = []map[string]any{}
		}
		raw, _ := json.Marshal(map[string]any{"commits": []map[string]string{{"sha": overlapHead}}, "files": files})
		_, _ = w.Write(raw)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
}

func (g *overlapGitHub) setFiles(paths ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.files = nil
	for _, p := range paths {
		g.files = append(g.files, map[string]any{"filename": p, "status": "modified", "changes": 1, "patch": "@@ -1 +1 @@"})
	}
}

func (g *overlapGitHub) prCompareCalls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.prCompares
}

// mergeOverlapFixture is an overlapFixture (orchestratorRepo + artifact +
// amendment fakes) with run A made merge-ready: PR url, repo x/y,
// installation 42, a GateMerger and the overlapGitHub stub. A declares no
// workflow spec, so the acceptance gate is not-declared and no merge-candidate
// verify is required: with the guard absent, every fixture admits. A's
// WorkflowSHA is cleared so it NEVER recorded a spec (no ADR-112 forge-approval
// gate declared); a recorded SHA with no cached spec is the legacy row shape
// the fold treats as undetermined (C4).
type mergeOverlapFixture struct {
	*overlapFixture
	gh     *overlapGitHub
	merger *fakeMerger
	a      *run.Run
}

func newMergeOverlapFixture(t *testing.T) *mergeOverlapFixture {
	t.Helper()
	f := newOverlapFixture(t)
	gh := &overlapGitHub{}
	merger := &fakeMerger{}
	f.s.cfg.GitHub = gh.client(t)
	f.s.cfg.GateMerger = merger
	a := f.rr.seedRun()
	a.WorkflowSHA = ""
	pr := mergePR
	ref := "issue:self"
	a.PullRequestURL = &pr
	a.InstallationID = instID(42)
	a.TriggerRef = &ref
	gh.setFiles("shared.go")
	return &mergeOverlapFixture{overlapFixture: f, gh: gh, merger: merger, a: a}
}

// seedImplementOn adds an implement-type stage in state to an existing run.
func (f *mergeOverlapFixture) seedImplementOn(runID uuid.UUID, state run.StageState) *run.Stage {
	st := f.rr.seedStage(runID, 1, state)
	f.rr.mu.Lock()
	st.Type = run.StageTypeImplement
	f.rr.mu.Unlock()
	return st
}

// seedOther seeds run B (same repo, running) whose plan scopes paths and
// whose implement stage is in state.
func (f *mergeOverlapFixture) seedOther(t *testing.T, state run.StageState, paths ...string) (*run.Run, *run.Stage) {
	t.Helper()
	b := f.seedPlannedRun(t, run.StateRunning, overlapPlan(paths...))
	return b, f.seedImplementOn(b.ID, state)
}

// post merges as an operator holding write:approvals AND read:audit, so a
// refusal carries the full cross-run details (#4582); postAs takes the
// identity explicitly.
func (f *mergeOverlapFixture) post(t *testing.T, body mergeRunRequest) *httptest.ResponseRecorder {
	t.Helper()
	return f.postAs(t, body, withMergeOperatorAuditReader)
}

func (f *mergeOverlapFixture) postAs(t *testing.T, body mergeRunRequest, withID func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	if body.Verdict == "" {
		body.Verdict = "go"
	}
	return postMergeRun(t, f.s, f.a.ID, body, withID)
}

// withMergeOperatorAuditReader injects the operator token carrying
// write:approvals plus read:audit — the scope that unlocks the overlap
// refusal's cross-run details (overlapDetailsVisible).
func withMergeOperatorAuditReader(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
		Subject: "github:ops", TokenID: "tok-op", Scopes: []string{"write:approvals", scopeGateViewRead},
	}))
}

func overlapRows(au *auditFake, category string) []audit.ChainAppendParams {
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []audit.ChainAppendParams
	for _, a := range au.appended {
		if a.Category == category {
			out = append(out, a)
		}
	}
	return out
}

func overlapAppendIndex(au *auditFake, category string) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	for i, a := range au.appended {
		if a.Category == category {
			return i
		}
	}
	return -1
}

// assertRefused asserts status + code AND committed state: no verdict row, no
// override row, no dispatch.
func (f *mergeOverlapFixture) assertRefused(t *testing.T, w *httptest.ResponseRecorder, status int, code string) mergeErrEnvelope {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d:\n%s", w.Code, status, w.Body.String())
	}
	env := decodeMergeErr(t, w)
	if env.Error.Code != code {
		t.Fatalf("error code = %q, want %q:\n%s", env.Error.Code, code, w.Body.String())
	}
	if n := len(overlapRows(f.au, CategoryMergeVerdictRecorded)); n != 0 {
		t.Errorf("merge_verdict_recorded rows = %d, want 0", n)
	}
	if n := len(overlapRows(f.au, CategoryMergeOverlapOverridden)); n != 0 {
		t.Errorf("merge_overlap_overridden rows = %d, want 0", n)
	}
	if f.merger.called != 0 {
		t.Errorf("merger called %d times, want 0", f.merger.called)
	}
	return env
}

func (f *mergeOverlapFixture) assertAdmitted(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the guard admits):\n%s", w.Code, w.Body.String())
	}
	if n := len(overlapRows(f.au, CategoryMergeVerdictRecorded)); n != 1 {
		t.Errorf("merge_verdict_recorded rows = %d, want 1", n)
	}
	if f.merger.called != 1 {
		t.Errorf("merger called %d times, want 1", f.merger.called)
	}
}

func assertUndeterminedReason(t *testing.T, env mergeErrEnvelope, reason string) {
	t.Helper()
	if env.Error.Details["reason"] != reason {
		t.Fatalf("details.reason = %v, want %q (details %v)", env.Error.Details["reason"], reason, env.Error.Details)
	}
}

// TestMergeRun_OverlappingImplementRefused: another run's implement stage
// dispatched or running on a path this PR changes refuses 409
// overlapping_implement_in_flight naming it, with nothing recorded.
// Counterfactual: replacing the guard call with `overlap := mergeOverlapResult{}`
// admits (200 with a verdict row) → RED.
func TestMergeRun_OverlappingImplementRefused(t *testing.T) {
	for _, state := range []run.StageState{run.StageStateDispatched, run.StageStateRunning} {
		t.Run(string(state), func(t *testing.T) {
			f := newMergeOverlapFixture(t)
			b, st := f.seedOther(t, state, "shared.go", "b.go")
			env := f.assertRefused(t, f.post(t, mergeRunRequest{}), http.StatusConflict, mergeCodeOverlappingImplement)
			d := env.Error.Details
			if d["next_step"] != "fishhawk_await_stage" || d["blocking_run_id"] != b.ID.String() || d["stage_id"] != st.ID.String() || d["head_sha"] != overlapHead {
				t.Fatalf("details = %v", d)
			}
			blocking, _ := d["blocking"].([]any)
			if len(blocking) != 1 {
				t.Fatalf("blocking = %v, want one entry", d["blocking"])
			}
			first := blocking[0].(map[string]any)
			if first["run_id"] != b.ID.String() || first["stage_state"] != string(state) || first["stage_type"] != "implement" ||
				fmt.Sprint(first["paths"]) != "[shared.go]" || first["issue_ref"] != *b.TriggerRef {
				t.Fatalf("blocking[0] = %v", first)
			}
			if !strings.Contains(env.Error.Message, "No new merge verdict was recorded") || !strings.Contains(env.Error.Message, "allow_overlap") {
				t.Errorf("message = %q", env.Error.Message)
			}
		})
	}
}

// TestMergeRun_OverlapNonLiveStageAdmits: a planned or parked run does not
// block, even though its scope intersects. Counterfactual: widening
// overlapBlockingStageState to accept pending refuses the pending subcase → RED.
func TestMergeRun_OverlapNonLiveStageAdmits(t *testing.T) {
	for _, state := range []run.StageState{
		run.StageStatePending, run.StageStateAwaitingHostDispatch, run.StageStateAwaitingApproval,
		run.StageStateAwaitingScopeDecision, run.StageStateSucceeded, run.StageStateFailed, run.StageStateCancelled,
	} {
		t.Run(string(state), func(t *testing.T) {
			f := newMergeOverlapFixture(t)
			f.seedOther(t, state, "shared.go")
			f.assertAdmitted(t, f.post(t, mergeRunRequest{}))
		})
	}
}

func TestMergeRun_OverlapDisjointAdmits(t *testing.T) {
	f := newMergeOverlapFixture(t)
	f.seedOther(t, run.StageStateRunning, "shared.go")
	f.gh.setFiles("other.go")
	f.assertAdmitted(t, f.post(t, mergeRunRequest{}))
	if n := f.gh.prCompareCalls(); n != 1 {
		t.Errorf("main...head compares = %d, want 1 (a live candidate exists)", n)
	}
}

// TestMergeRun_OverlapNoLiveCandidateSkipsForge: with no live implement
// elsewhere the guard admits before any forge read.
func TestMergeRun_OverlapNoLiveCandidateSkipsForge(t *testing.T) {
	f := newMergeOverlapFixture(t)
	f.assertAdmitted(t, f.post(t, mergeRunRequest{}))
	if n := f.gh.prCompareCalls(); n != 0 {
		t.Fatalf("main...head compares = %d, want 0 (no live candidate, no forge read)", n)
	}
}

// TestMergeRun_OverlapEmptyStageListAdmits (approval condition C8): the
// candidate listing is non-empty but the candidate's stage list is EMPTY — the
// guard admits because there is no implement stage, and the stage read
// demonstrably happened (it was not a swallowed error; the stage_read 502 test
// below shows an error refuses).
func TestMergeRun_OverlapEmptyStageListAdmits(t *testing.T) {
	f := newMergeOverlapFixture(t)
	b := f.seedPlannedRun(t, run.StateRunning, nil)
	f.rr.failStagesRun = b.ID // count calls, never fail (failStagesCall 0)
	f.assertAdmitted(t, f.post(t, mergeRunRequest{}))
	if f.rr.stagesCalls < 1 {
		t.Fatalf("stage reads for the candidate = %d, want >= 1", f.rr.stagesCalls)
	}
	if n := f.gh.prCompareCalls(); n != 0 {
		t.Fatalf("main...head compares = %d, want 0", n)
	}
}

// TestMergeRun_OverlapLineageExcluded: A's own decomposition child running on
// shared.go is lineage, not an overlap.
func TestMergeRun_OverlapLineageExcluded(t *testing.T) {
	f := newMergeOverlapFixture(t)
	child := f.rr.seedDecomposedChild(f.a.ID, 0, run.StateRunning)
	plan := f.rr.seedStage(child.ID, 0, run.StageStateSucceeded)
	seedBudgetPlanArtifact(t, f.art, plan.ID, overlapPlan("shared.go"))
	f.seedImplementOn(child.ID, run.StageStateRunning)
	f.assertAdmitted(t, f.post(t, mergeRunRequest{}))
}

// TestMergeRun_OverlapRenameSourceCounts: a rename's SOURCE path is a changed
// path.
func TestMergeRun_OverlapRenameSourceCounts(t *testing.T) {
	f := newMergeOverlapFixture(t)
	f.seedOther(t, run.StageStateRunning, "shared.go")
	f.gh.mu.Lock()
	f.gh.files = []map[string]any{{"filename": "new.go", "status": "renamed", "previous_filename": "shared.go", "changes": 0}}
	f.gh.mu.Unlock()
	f.assertRefused(t, f.post(t, mergeRunRequest{}), http.StatusConflict, mergeCodeOverlappingImplement)
}

// TestMergeRun_OverlapRenameSourceUnknownFailsClosed (approval condition C2):
// a renamed file with an EMPTY previous_filename is "source unknown" and
// refuses 409 overlap_undetermined rename_source_unknown. Counterfactual:
// skipping the empty-source check admits (new.go is disjoint) → RED.
func TestMergeRun_OverlapRenameSourceUnknownFailsClosed(t *testing.T) {
	f := newMergeOverlapFixture(t)
	b, _ := f.seedOther(t, run.StageStateRunning, "shared.go")
	f.gh.mu.Lock()
	f.gh.files = []map[string]any{{"filename": "new.go", "status": "renamed", "changes": 0}}
	f.gh.mu.Unlock()
	env := f.assertRefused(t, f.post(t, mergeRunRequest{}), http.StatusConflict, mergeCodeOverlapUndetermined)
	assertUndeterminedReason(t, env, overlapReasonRenameSourceUnknown)
	und, _ := env.Error.Details["undetermined"].([]any)
	if len(und) != 1 || und[0].(map[string]any)["run_id"] != b.ID.String() {
		t.Fatalf("undetermined = %v, want B", env.Error.Details["undetermined"])
	}
}

// TestMergeRun_OverlapForgeReadFailures: a PR read or a compare read failing
// refuses 502 overlap_check_failed forge_read; the 5xx details carry only
// allow-listed keys. Counterfactual: each branch, deleted, admits → RED.
func TestMergeRun_OverlapForgeReadFailures(t *testing.T) {
	for name, set := range map[string]func(g *overlapGitHub){
		"get_pull_request": func(g *overlapGitHub) { g.prStatus = http.StatusInternalServerError },
		"compare":          func(g *overlapGitHub) { g.compareStatus = http.StatusInternalServerError },
	} {
		t.Run(name, func(t *testing.T) {
			f := newMergeOverlapFixture(t)
			f.seedOther(t, run.StageStateRunning, "shared.go")
			set(f.gh)
			env := f.assertRefused(t, f.post(t, mergeRunRequest{}), http.StatusBadGateway, mergeCodeOverlapCheckFailed)
			if env.Error.Details["reason"] != overlapReasonForgeRead || env.Error.Details["retryable"] != true {
				t.Fatalf("details = %v", env.Error.Details)
			}
			for k := range env.Error.Details {
				if k != "reason" && k != "retryable" && k != "run_id" {
					t.Errorf("5xx detail key %q leaked past the allow-list", k)
				}
			}
		})
	}
}

// TestMergeRun_OverlapCompareTruncatedFailsClosed (approval condition C6): a
// comparison the forge truncated — the 300-file cap or an omitted oversized
// patch, both read from ComparePatchResult.Truncated — refuses 409
// overlap_undetermined changed_files_capped even though no listed file is
// shared. Counterfactual: ignoring Truncated admits → RED.
func TestMergeRun_OverlapCompareTruncatedFailsClosed(t *testing.T) {
	capped := make([]map[string]any, 0, 300)
	for i := 0; i < 300; i++ {
		capped = append(capped, map[string]any{"filename": fmt.Sprintf("gen/f%03d.go", i), "status": "modified", "changes": 1, "patch": "@@"})
	}
	for name, files := range map[string][]map[string]any{
		"file_cap":        capped,
		"oversized_patch": {{"filename": "other.go", "status": "modified", "changes": 5000}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMergeOverlapFixture(t)
			f.seedOther(t, run.StageStateRunning, "shared.go", "extra.go")
			f.gh.mu.Lock()
			f.gh.files = files
			f.gh.mu.Unlock()
			env := f.assertRefused(t, f.post(t, mergeRunRequest{}), http.StatusConflict, mergeCodeOverlapUndetermined)
			assertUndeterminedReason(t, env, overlapReasonChangedFilesCapped)
		})
	}
}

// TestMergeRun_OverlapChangedPathsUnavailable: a run whose changed paths this
// deployment cannot read (no installation; no GitHub client) refuses while a
// live implement exists elsewhere.
func TestMergeRun_OverlapChangedPathsUnavailable(t *testing.T) {
	for name, set := range map[string]func(f *mergeOverlapFixture){
		"no_installation": func(f *mergeOverlapFixture) { f.a.InstallationID = nil },
		"no_github":       func(f *mergeOverlapFixture) { f.s.cfg.GitHub = nil },
	} {
		t.Run(name, func(t *testing.T) {
			f := newMergeOverlapFixture(t)
			f.seedOther(t, run.StageStateRunning, "shared.go")
			set(f)
			env := f.assertRefused(t, f.post(t, mergeRunRequest{}), http.StatusConflict, mergeCodeOverlapUndetermined)
			assertUndeterminedReason(t, env, overlapReasonChangedPathsUnavailable)
		})
	}
}

func TestMergeRun_OverlapRunListError(t *testing.T) {
	f := newMergeOverlapFixture(t)
	f.seedOther(t, run.StageStateRunning, "shared.go")
	f.rr.listRunsErr = errors.New("db down")
	env := f.assertRefused(t, f.post(t, mergeRunRequest{}), http.StatusBadGateway, mergeCodeOverlapCheckFailed)
	if env.Error.Details["reason"] != overlapReasonRunList {
		t.Fatalf("details = %v", env.Error.Details)
	}
	if strings.Contains(w2s(env), "db down") {
		t.Fatalf("raw cause leaked: %v", env)
	}
}

func w2s(env mergeErrEnvelope) string {
	raw, _ := json.Marshal(env)
	return string(raw)
}

func TestMergeRun_OverlapRunListTruncated(t *testing.T) {
	f := newMergeOverlapFixture(t)
	for i := 0; i < scopeOverlapListLimit; i++ {
		f.rr.seedRun()
	}
	env := f.assertRefused(t, f.post(t, mergeRunRequest{}), http.StatusConflict, mergeCodeOverlapUndetermined)
	assertUndeterminedReason(t, env, overlapReasonRunListTruncated)
}

func TestMergeRun_OverlapStageReadError(t *testing.T) {
	f := newMergeOverlapFixture(t)
	b, _ := f.seedOther(t, run.StageStateRunning, "shared.go")
	f.rr.failStagesRun, f.rr.failStagesCall = b.ID, 1
	env := f.assertRefused(t, f.post(t, mergeRunRequest{}), http.StatusBadGateway, mergeCodeOverlapCheckFailed)
	if env.Error.Details["reason"] != overlapReasonStageRead {
		t.Fatalf("details = %v", env.Error.Details)
	}
}

func TestMergeRun_OverlapScopeUnresolved(t *testing.T) {
	f := newMergeOverlapFixture(t)
	b := f.seedPlannedRun(t, run.StateRunning, nil)
	f.seedImplementOn(b.ID, run.StageStateRunning)
	env := f.assertRefused(t, f.post(t, mergeRunRequest{}), http.StatusConflict, mergeCodeOverlapUndetermined)
	assertUndeterminedReason(t, env, overlapReasonScopeUnresolved)
}

// TestMergeRun_OverlapMergedPRAdmits: a PR the forge reports merged is not
// refused by the guard (the #3622 already_merged resume must still answer).
func TestMergeRun_OverlapMergedPRAdmits(t *testing.T) {
	f := newMergeOverlapFixture(t)
	f.seedOther(t, run.StageStateRunning, "shared.go")
	f.gh.mu.Lock()
	f.gh.merged = true
	f.gh.mu.Unlock()
	w := f.post(t, mergeRunRequest{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if n := f.gh.prCompareCalls(); n != 0 {
		t.Fatalf("main...head compares = %d, want 0 (merged PR admits before the compare)", n)
	}
}

type overlapOverridePayload struct {
	RunID            string                `json:"run_id"`
	HeadSHA          string                `json:"head_sha"`
	OverriddenCode   string                `json:"overridden_code"`
	OverriddenReason string                `json:"overridden_reason"`
	Blocking         []mergeOverlapBlocker `json:"blocking"`
	Undetermined     []mergeOverlapBlocker `json:"undetermined"`
	OverlapReason    string                `json:"overlap_reason"`
	Fingerprint      string                `json:"fingerprint"`
}

func decodeOverride(t *testing.T, p audit.ChainAppendParams) overlapOverridePayload {
	t.Helper()
	var out overlapOverridePayload
	if err := json.Unmarshal(p.Payload, &out); err != nil {
		t.Fatalf("decode override payload: %v", err)
	}
	return out
}

// TestMergeRun_OverlapOverrideMerges: allow_overlap + overlap_reason merges,
// recording ONE merge_overlap_overridden row BEFORE the verdict row (first
// POST), with overlap_reason stored byte-for-byte — leading/trailing
// whitespace included (approval condition C3).
func TestMergeRun_OverlapOverrideMerges(t *testing.T) {
	f := newMergeOverlapFixture(t)
	b, st := f.seedOther(t, run.StageStateRunning, "shared.go")
	reason := "  judged safe <script>\n"
	f.assertAdmitted(t, f.post(t, mergeRunRequest{AllowOverlap: true, OverlapReason: reason}))
	rows := overlapRows(f.au, CategoryMergeOverlapOverridden)
	if len(rows) != 1 {
		t.Fatalf("override rows = %d, want 1", len(rows))
	}
	p := decodeOverride(t, rows[0])
	if p.OverlapReason != reason {
		t.Fatalf("overlap_reason = %q, want %q byte-for-byte", p.OverlapReason, reason)
	}
	if p.OverriddenCode != mergeCodeOverlappingImplement || p.HeadSHA != overlapHead || p.RunID != f.a.ID.String() ||
		len(p.Blocking) != 1 || p.Blocking[0].RunID != b.ID.String() || p.Blocking[0].StageID != st.ID.String() ||
		strings.Join(p.Blocking[0].Paths, ",") != "shared.go" || p.Fingerprint == "" {
		t.Fatalf("override payload = %+v", p)
	}
	if rows[0].ActorSubject == nil || *rows[0].ActorSubject != "github:ops" {
		t.Errorf("actor subject = %v", rows[0].ActorSubject)
	}
	if oi, vi := overlapAppendIndex(f.au, CategoryMergeOverlapOverridden), overlapAppendIndex(f.au, CategoryMergeVerdictRecorded); oi < 0 || vi < 0 || oi >= vi {
		t.Fatalf("override index %d, verdict index %d: the override must precede the verdict", oi, vi)
	}
}

// TestMergeRun_OverlapOverrideDedupes: an identical re-POST appends no second
// row; a changed overlap_reason is a new judgement and appends one.
func TestMergeRun_OverlapOverrideDedupes(t *testing.T) {
	f := newMergeOverlapFixture(t)
	f.seedOther(t, run.StageStateRunning, "shared.go")
	for i := 0; i < 2; i++ {
		if w := f.post(t, mergeRunRequest{AllowOverlap: true, OverlapReason: "safe"}); w.Code != http.StatusOK {
			t.Fatalf("post %d = %d:\n%s", i, w.Code, w.Body.String())
		}
	}
	if n := len(overlapRows(f.au, CategoryMergeOverlapOverridden)); n != 1 {
		t.Fatalf("override rows after two identical posts = %d, want 1", n)
	}
	if w := f.post(t, mergeRunRequest{AllowOverlap: true, OverlapReason: "safe, re-judged"}); w.Code != http.StatusOK {
		t.Fatalf("third post = %d:\n%s", w.Code, w.Body.String())
	}
	if n := len(overlapRows(f.au, CategoryMergeOverlapOverridden)); n != 2 {
		t.Fatalf("override rows after a changed reason = %d, want 2", n)
	}
}

// TestMergeRun_OverlapOverrideAppendFailureRefuses: the override append fails
// (only that category, so a verdict append would succeed if reached) → 503
// overlap_override_unrecorded, no verdict, no merge. Counterfactual: replacing
// `return appendErr` with `_ = appendErr` reaches the verdict append and the
// dispatch (200) → RED.
func TestMergeRun_OverlapOverrideAppendFailureRefuses(t *testing.T) {
	f := newMergeOverlapFixture(t)
	f.seedOther(t, run.StageStateRunning, "shared.go")
	f.au.appendErrCategory = CategoryMergeOverlapOverridden
	env := f.assertRefused(t, f.post(t, mergeRunRequest{AllowOverlap: true, OverlapReason: "safe"}),
		http.StatusServiceUnavailable, mergeCodeOverlapOverrideUnrecorded)
	if strings.Contains(env.Error.Message, "injected") {
		t.Fatalf("raw cause leaked into the message: %q", env.Error.Message)
	}
}

// TestMergeRun_OverlapOverrideReadFailureRefuses: the dedupe read fails → 503,
// nothing recorded (fail closed).
func TestMergeRun_OverlapOverrideReadFailureRefuses(t *testing.T) {
	f := newMergeOverlapFixture(t)
	f.seedOther(t, run.StageStateRunning, "shared.go")
	f.au.listByCategoryErrCategory = CategoryMergeOverlapOverridden
	f.assertRefused(t, f.post(t, mergeRunRequest{AllowOverlap: true, OverlapReason: "safe"}),
		http.StatusServiceUnavailable, mergeCodeOverlapOverrideUnrecorded)
}

// TestMergeRun_OverlapOverrideOnUndetermined: an override of an undetermined
// state merges and records which code and reason it overrode.
func TestMergeRun_OverlapOverrideOnUndetermined(t *testing.T) {
	f := newMergeOverlapFixture(t)
	f.seedOther(t, run.StageStateRunning, "shared.go")
	f.a.InstallationID = nil
	f.assertAdmitted(t, f.post(t, mergeRunRequest{AllowOverlap: true, OverlapReason: "GitLab-style blind merge, judged safe"}))
	rows := overlapRows(f.au, CategoryMergeOverlapOverridden)
	if len(rows) != 1 {
		t.Fatalf("override rows = %d, want 1", len(rows))
	}
	p := decodeOverride(t, rows[0])
	if p.OverriddenCode != mergeCodeOverlapUndetermined || p.OverriddenReason != overlapReasonChangedPathsUnavailable || len(p.Undetermined) != 1 {
		t.Fatalf("override payload = %+v", p)
	}
}

// TestMergeRun_OverlapAdmittedOverrideRecordsNothing: allow_overlap when the
// guard admits writes no override row.
func TestMergeRun_OverlapAdmittedOverrideRecordsNothing(t *testing.T) {
	f := newMergeOverlapFixture(t)
	f.assertAdmitted(t, f.post(t, mergeRunRequest{AllowOverlap: true, OverlapReason: "safe"}))
	if n := len(overlapRows(f.au, CategoryMergeOverlapOverridden)); n != 0 {
		t.Fatalf("override rows = %d, want 0 (nothing was overridden)", n)
	}
}

// TestMergeRun_OverlapOverrideOnCheckFailed: allow_overlap + overlap_reason
// overrides a 502 overlap_check_failed (a forge read or a stage read failing)
// and merges, recording ONE merge_overlap_overridden row naming the overridden
// code and reason with EMPTY blocking / undetermined lists, before the verdict.
func TestMergeRun_OverlapOverrideOnCheckFailed(t *testing.T) {
	for name, tc := range map[string]struct {
		reason string
		set    func(f *mergeOverlapFixture, b *run.Run)
	}{
		"forge_read": {overlapReasonForgeRead, func(f *mergeOverlapFixture, _ *run.Run) { f.gh.compareStatus = http.StatusInternalServerError }},
		"stage_read": {overlapReasonStageRead, func(f *mergeOverlapFixture, b *run.Run) { f.rr.failStagesRun, f.rr.failStagesCall = b.ID, 1 }},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMergeOverlapFixture(t)
			b, _ := f.seedOther(t, run.StageStateRunning, "shared.go")
			tc.set(f, b)
			reason := "forge outage, judged safe"
			f.assertAdmitted(t, f.post(t, mergeRunRequest{AllowOverlap: true, OverlapReason: reason}))
			rows := overlapRows(f.au, CategoryMergeOverlapOverridden)
			if len(rows) != 1 {
				t.Fatalf("override rows = %d, want 1", len(rows))
			}
			raw := string(rows[0].Payload)
			if !strings.Contains(raw, `"blocking":[]`) || !strings.Contains(raw, `"undetermined":[]`) {
				t.Fatalf("override payload = %s, want empty blocking and undetermined arrays", raw)
			}
			p := decodeOverride(t, rows[0])
			if p.OverriddenCode != mergeCodeOverlapCheckFailed || p.OverriddenReason != tc.reason || p.OverlapReason != reason {
				t.Fatalf("override payload = %+v", p)
			}
			if oi, vi := overlapAppendIndex(f.au, CategoryMergeOverlapOverridden), overlapAppendIndex(f.au, CategoryMergeVerdictRecorded); oi < 0 || vi < 0 || oi >= vi {
				t.Fatalf("override index %d, verdict index %d: the override must precede the verdict", oi, vi)
			}
		})
	}
}

// TestMergeRun_OverlapDetailsWithheldWithoutReadAudit (#4582): a caller
// holding write:approvals but not read:audit gets the same 409 code with
// counts in place of other runs' ids, stages and paths. Counterfactuals (b)
// predicate `return true` and (c) `shown = overlap` render the full details
// → RED.
func TestMergeRun_OverlapDetailsWithheldWithoutReadAudit(t *testing.T) {
	for name, tc := range map[string]struct {
		code                     string
		blocking, undetermined   float64
		set                      func(f *mergeOverlapFixture)
		wantNextStep, wantReason string
	}{
		mergeCodeOverlappingImplement: {mergeCodeOverlappingImplement, 1, 0, func(*mergeOverlapFixture) {}, "fishhawk_await_stage", ""},
		mergeCodeOverlapUndetermined: {mergeCodeOverlapUndetermined, 0, 1, func(f *mergeOverlapFixture) { f.a.InstallationID = nil }, "",
			overlapReasonChangedPathsUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMergeOverlapFixture(t)
			b, st := f.seedOther(t, run.StageStateRunning, "shared.go")
			tc.set(f)
			w := f.postAs(t, mergeRunRequest{}, withMergeOperator)
			env := f.assertRefused(t, w, http.StatusConflict, tc.code)
			d := env.Error.Details
			if d["blocking_count"] != tc.blocking || d["undetermined_count"] != tc.undetermined ||
				d["details_withheld"] != true || d["required_scope"] != scopeGateViewRead {
				t.Fatalf("details = %v", d)
			}
			if tc.wantNextStep != "" && d["next_step"] != tc.wantNextStep {
				t.Errorf("next_step = %v, want %q", d["next_step"], tc.wantNextStep)
			}
			if tc.wantReason != "" && d["reason"] != tc.wantReason {
				t.Errorf("reason = %v, want %q", d["reason"], tc.wantReason)
			}
			for _, k := range []string{"blocking", "undetermined", "blocking_run_id", "stage_id"} {
				if _, ok := d[k]; ok {
					t.Errorf("details carry withheld key %q: %v", k, d)
				}
			}
			assertNamesNoOtherRun(t, "response body", w.Body.String(), b, st)
		})
	}
}

// TestMergeRun_OverlapDetailsVisibleWithReadAudit: the same blocker with
// read:audit names B's run, stage and shared path, and is not marked withheld.
func TestMergeRun_OverlapDetailsVisibleWithReadAudit(t *testing.T) {
	f := newMergeOverlapFixture(t)
	b, st := f.seedOther(t, run.StageStateRunning, "shared.go")
	env := f.assertRefused(t, f.post(t, mergeRunRequest{}), http.StatusConflict, mergeCodeOverlappingImplement)
	d := env.Error.Details
	blocking, _ := d["blocking"].([]any)
	if len(blocking) != 1 {
		t.Fatalf("blocking = %v", d["blocking"])
	}
	first := blocking[0].(map[string]any)
	if first["run_id"] != b.ID.String() || first["stage_id"] != st.ID.String() || fmt.Sprint(first["paths"]) != "[shared.go]" {
		t.Fatalf("blocking[0] = %v", first)
	}
	if d["blocking_run_id"] != b.ID.String() || d["stage_id"] != st.ID.String() {
		t.Fatalf("details = %v", d)
	}
	if _, ok := d["details_withheld"]; ok {
		t.Fatalf("details_withheld present for a read:audit caller: %v", d)
	}
}

func TestOverlapDetailsVisible(t *testing.T) {
	for name, tc := range map[string]struct {
		id   Identity
		want bool
	}{
		"cookie_session":       {Identity{Subject: "github:ops"}, true},
		"token_read_audit":     {Identity{Subject: "github:ops", TokenID: "t", Scopes: []string{"write:approvals", scopeGateViewRead}}, true},
		"token_approvals_only": {Identity{Subject: "github:ops", TokenID: "t", Scopes: []string{"write:approvals"}}, false},
		"run_bound_read_audit": {Identity{Subject: "mcp:run:" + uuid.NewString(), TokenID: "t", Scopes: []string{scopeGateViewRead}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := overlapDetailsVisible(tc.id); got != tc.want {
				t.Fatalf("overlapDetailsVisible(%+v) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}

// TestMergeOverlapResult_WithheldLeavesCheckFailedAndAdmitUnchanged: the 502
// and the admitting zero value carry no cross-run detail, so withheld returns
// them as-is (the 5xx allow-list stays reason / retryable).
func TestMergeOverlapResult_WithheldLeavesCheckFailedAndAdmitUnchanged(t *testing.T) {
	failed := overlapCheckFailed(overlapReasonForgeRead, errors.New("boom"))
	got := failed.withheld()
	if got.Code != mergeCodeOverlapCheckFailed || got.Message != failed.Message || fmt.Sprint(got.Details) != fmt.Sprint(failed.Details) {
		t.Fatalf("withheld 502 = %+v, want unchanged %+v", got, failed)
	}
	if (mergeOverlapResult{}).withheld().refused() {
		t.Fatal("withheld zero value refuses")
	}
}

func TestMergeOverlapGuard_ZeroValueAdmits(t *testing.T) {
	if (mergeOverlapResult{}).refused() {
		t.Fatal("the zero mergeOverlapResult must admit")
	}
	if !overlapBlockingStageState(run.StageStateRunning) || !overlapBlockingStageState(run.StageStateDispatched) ||
		overlapBlockingStageState(run.StageStatePending) {
		t.Fatal("overlapBlockingStageState drifted")
	}
}

// --- ADR-093 enqueue arm (real Postgres): the guard precedes the enqueue ---

// seedOverlapLiveRun adds a second run in the mcq fixture's repo whose
// implement stage is running. No artifact repository is wired, so its scope
// cannot resolve: the guard refuses scope_unresolved.
func seedOverlapLiveRun(t *testing.T, f *mcqFixture) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	r, err := f.repo.CreateRun(ctx, run.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "deadbeef", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	f.exec(`UPDATE runs SET state = 'running' WHERE id = $1`, r.ID)
	st, err := f.repo.CreateStage(ctx, run.CreateStageParams{
		RunID: r.ID, Sequence: 1, Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}
	f.setStage(st.ID, run.StageStateRunning)
	return r.ID
}

// TestMergeRun_OverlapRefusedBeforeEnqueue (approval condition C10): an
// overlap-blocked, otherwise merge-queue-ELIGIBLE behind run is refused, not
// enqueued: no merge_queue_entered row and no queue entry.
func TestMergeRun_OverlapRefusedBeforeEnqueue(t *testing.T) {
	f, r, merger := mqMergeFixture(t, true)
	seedOverlapLiveRun(t, f)
	w := f.mergeRun(r)
	if w.Code != http.StatusConflict || decodeMergeErr(t, w).Error.Code != mergeCodeOverlapUndetermined {
		t.Fatalf("merge = %d, want 409 %s:\n%s", w.Code, mergeCodeOverlapUndetermined, w.Body.String())
	}
	if n := len(f.rows(r, CategoryMergeQueueEntered)); n != 0 {
		t.Fatalf("merge_queue_entered rows = %d, want 0", n)
	}
	if got := f.intentAt(r); len(got) != 0 {
		t.Fatalf("queue entries = %v, want none", got)
	}
	if merger.called != 0 {
		t.Fatalf("merger called %d times", merger.called)
	}
}

// TestMergeRun_OverlapOverrideRecordedBeforeMergeIntent: an overridden
// overlap on the enqueue path records merge_overlap_overridden BEFORE the
// merge_queue_entered row.
func TestMergeRun_OverlapOverrideRecordedBeforeMergeIntent(t *testing.T) {
	f, r, _ := mqMergeFixture(t, true)
	seedOverlapLiveRun(t, f)
	w := postMergeRun(t, f.srv, r.id, mergeRunRequest{Verdict: "ship it", AllowOverlap: true, OverlapReason: "safe"}, withMergeOperator)
	if w.Code != http.StatusAccepted {
		t.Fatalf("merge = %d, want 202:\n%s", w.Code, w.Body.String())
	}
	f.srv.bgMergeQueuePump.Wait()
	over, entered := f.rows(r, CategoryMergeOverlapOverridden), f.rows(r, CategoryMergeQueueEntered)
	if len(over) != 1 || len(entered) != 1 {
		t.Fatalf("override rows = %d, merge_queue_entered rows = %d, want 1 and 1", len(over), len(entered))
	}
	if over[0].Sequence >= entered[0].Sequence {
		t.Fatalf("override sequence %d, intent sequence %d: the override must precede the intent", over[0].Sequence, entered[0].Sequence)
	}
}

// mqFailOverrideAudit fails every merge_overlap_overridden append.
type mqFailOverrideAudit struct {
	audit.Repository
}

func (a mqFailOverrideAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if p.Category == CategoryMergeOverlapOverridden {
		return nil, errors.New("audit chain unavailable")
	}
	return a.Repository.AppendChained(ctx, p)
}

// TestMergeRun_OverlapOverrideUnrecordedNeverEnqueues: a failed override
// record on the enqueue path answers 503 with no merge intent and no entry.
// Counterfactual: dropping the preCommit check in recordMergeQueueIntent
// enqueues (202) → RED.
func TestMergeRun_OverlapOverrideUnrecordedNeverEnqueues(t *testing.T) {
	f, r, _ := mqMergeFixture(t, true)
	seedOverlapLiveRun(t, f)
	f.srv.cfg.AuditRepo = mqFailOverrideAudit{f.audit}
	w := postMergeRun(t, f.srv, r.id, mergeRunRequest{Verdict: "ship it", AllowOverlap: true, OverlapReason: "safe"}, withMergeOperator)
	if w.Code != http.StatusServiceUnavailable || decodeMergeErr(t, w).Error.Code != mergeCodeOverlapOverrideUnrecorded {
		t.Fatalf("merge = %d, want 503 %s:\n%s", w.Code, mergeCodeOverlapOverrideUnrecorded, w.Body.String())
	}
	if n := len(f.rows(r, CategoryMergeQueueEntered)); n != 0 {
		t.Fatalf("merge_queue_entered rows = %d, want 0", n)
	}
	if got := f.intentAt(r); len(got) != 0 {
		t.Fatalf("queue entries = %v, want none", got)
	}
}
