package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/invariantmonitor"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Synthetic commit ids. A failure line starting `setup:` is a fixture failure;
// one starting `assert:` is the behaviour under test (the ledger's RED rule).
const (
	rhBaseSHA  = "b000000000000000000000000000000000000000"
	rhH1       = "1111111111111111111111111111111111111111"
	rhH2       = "2222222222222222222222222222222222222222"
	rhH3       = "3333333333333333333333333333333333333333"
	rhOther    = "4444444444444444444444444444444444444444"
	rhBaseRef  = "main"
	rhPRBranch = "fishhawk/run-reported-head"
)

// rhForge is a stateful fake forge. Routes are named pull, ref, commit, compare,
// contents and check_run, and everything else is other. status answers a route
// with that code; calls logs every request, so a test can assert a route was or
// was not read; hook runs first on every request, outside the lock.
type rhForge struct {
	mu      sync.Mutex
	prHead  string
	prRef   string
	baseRef string
	tips    map[string]string
	parents map[string][]string
	compare map[string][]string
	changed []string
	files   map[string]string
	status  map[string]int
	calls   []string
	hook    func(route string, r *http.Request)
}

func newRHForge() *rhForge {
	return &rhForge{
		prRef: rhPRBranch, baseRef: rhBaseRef,
		tips: map[string]string{}, parents: map[string][]string{}, compare: map[string][]string{},
		files: map[string]string{}, status: map[string]int{},
	}
}

func (g *rhForge) set(fn func(*rhForge)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	fn(g)
}

func (g *rhForge) count(route string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.calls {
		if c == route {
			n++
		}
	}
	return n
}

func (g *rhForge) allCalls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

// enter logs the request and reports whether the route should answer normally.
func (g *rhForge) enter(route string, w http.ResponseWriter, r *http.Request) bool {
	g.mu.Lock()
	g.calls = append(g.calls, route)
	status, hook := g.status[route], g.hook
	g.mu.Unlock()
	if hook != nil {
		hook(route, r)
	}
	w.Header().Set("Content-Type", "application/json")
	if status != 0 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"injected"}`))
		return false
	}
	return true
}

func (g *rhForge) client(t *testing.T) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		if !g.enter("pull", w, r) {
			return
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node_id": "PR_x", "state": "open",
			"head": map[string]string{"sha": g.prHead, "ref": g.prRef},
			"base": map[string]string{"ref": g.baseRef},
		})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/ref/heads/{branch...}", func(w http.ResponseWriter, r *http.Request) {
		if !g.enter("ref", w, r) {
			return
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		tip, ok := g.tips[r.PathValue("branch")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": tip}})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/commits/{sha}", func(w http.ResponseWriter, r *http.Request) {
		if !g.enter("commit", w, r) {
			return
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		sha := r.PathValue("sha")
		parents, ok := g.parents[sha]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		rows := make([]map[string]string, 0, len(parents))
		for _, p := range parents {
			rows = append(rows, map[string]string{"sha": p})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sha": sha, "tree": map[string]string{"sha": "tree-" + sha}, "parents": rows,
		})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/compare/{basehead...}", func(w http.ResponseWriter, r *http.Request) {
		if !g.enter("compare", w, r) {
			return
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		base, _, _ := strings.Cut(r.PathValue("basehead"), "...")
		commits := []map[string]string{}
		for _, sha := range g.compare[base] {
			commits = append(commits, map[string]string{"sha": sha})
		}
		files := []map[string]any{}
		for _, c := range g.changed {
			files = append(files, map[string]any{"filename": c, "status": "modified", "changes": 2, "patch": "@@ -1 +1 @@\n-a\n+b"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_commits": len(commits), "commits": commits, "files": files})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
		if !g.enter("contents", w, r) {
			return
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		c, ok := g.files[r.URL.Query().Get("ref")+"|"+r.PathValue("path")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"path": r.PathValue("path"), "sha": "blob", "type": "file", "encoding": "base64",
			"content": base64.StdEncoding.EncodeToString([]byte(c)),
		})
	})
	mux.HandleFunc("POST /repos/{owner}/{repo}/check-runs", func(w http.ResponseWriter, r *http.Request) {
		if !g.enter("check_run", w, r) {
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if g.enter("other", w, r) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  driftTokens{},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
}

// rhRunRepo injects the repository read failures the check must not swallow.
type rhRunRepo struct {
	*fixupRecoveryRepo
	getRunErr     error
	listStagesErr error
}

func (r *rhRunRepo) GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error) {
	if r.getRunErr != nil {
		return nil, r.getRunErr
	}
	return r.fixupRecoveryRepo.GetRun(ctx, id)
}

func (r *rhRunRepo) ListStagesForRun(ctx context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	if r.listStagesErr != nil {
		return nil, r.listStagesErr
	}
	return r.fixupRecoveryRepo.ListStagesForRun(ctx, runID)
}

// rhFixture is a run with a tracked PR, a running implement stage, a pending
// review stage and a settled acceptance stage holding a verdict, wired so every
// effect later than the hook is visible if the hook runs after it: a check-run
// POST, a re-review backstop round, a permission-drift row and an acceptance
// reopen.
type rhFixture struct {
	t          *testing.T
	s          *Server
	au         *auditFake
	rr         *rhRunRepo
	forge      *rhForge
	runRow     *run.Run
	impl       *run.Stage
	review     *run.Stage
	acceptance *run.Stage
	sf         *signingFake
	logs       *bytes.Buffer
}

func newRHFixture(t *testing.T) *rhFixture {
	t.Helper()
	g := newRHForge()
	client := g.client(t)
	rr := &rhRunRepo{fixupRecoveryRepo: &fixupRecoveryRepo{orchestratorRepo: newOrchestratorRepo()}}
	au := newAuditFake()
	au.stampSequence = true
	art := newFakeArtifactRepo()
	sf := newSigningFake()
	logs := &bytes.Buffer{}

	runRow := rr.seedRun()
	runRow.WorkflowID = "feature_change"
	runRow.WorkflowSpec = specImplementAdvisoryReviewers
	runRow.Repo = "acme/widgets"
	runRow.InstallationID = instID(99)
	prURL := "https://github.com/acme/widgets/pull/42"
	runRow.PullRequestURL = &prURL

	planStage := rr.seedStage(runRow.ID, 0, run.StageStateSucceeded)
	seedBudgetPlanArtifact(t, art, planStage.ID, &plan.Plan{
		PlanVersion:                "standard_v1",
		Summary:                    "synthetic plan",
		PredictedRuntimeMinutes:    10,
		PredictedRuntimeConfidence: plan.RuntimeConfidenceMedium,
		Scope:                      plan.Scope{Files: []plan.ScopeFile{{Path: "x.go", Operation: plan.FileOpModify}}},
	})
	impl := rr.seedStage(runRow.ID, 1, run.StageStateRunning)
	impl.Type = run.StageTypeImplement
	impl.RequiresApproval = true
	review := rr.seedStage(runRow.ID, 2, run.StageStatePending)
	review.Type = run.StageTypeReview
	acceptance := rr.seedStage(runRow.ID, 3, run.StageStateSucceeded)
	acceptance.Type = run.StageTypeAcceptance

	artBody, _ := json.Marshal(map[string]any{"head_sha": rhH1, "pr_number": 42})
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID: impl.ID, Kind: artifact.KindPullRequest, Content: artBody,
	}); err != nil {
		t.Fatalf("setup: seed pull_request artifact: %v", err)
	}

	s := New(Config{
		Addr:          "127.0.0.1:0",
		SigningRepo:   sf,
		ArtifactRepo:  art,
		AuditRepo:     au,
		RunRepo:       rr,
		ConcernRepo:   newFakeConcernRepo(),
		GitHub:        client,
		PlanReviewers: singleReviewerSet{&fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"}},
		Logger:        slog.New(slog.NewJSONHandler(logs, nil)),
	})
	s.auditCheckPublisher = auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub:      client,
		Runs:        rr,
		Artifacts:   art,
		Audit:       au,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	f := &rhFixture{t: t, s: s, au: au, rr: rr, forge: g, runRow: runRow, impl: impl, review: review, acceptance: acceptance, sf: sf, logs: logs}
	f.seed(CategoryAcceptanceOutcomeRecorded, acceptance.ID, 5, map[string]any{"outcome": "passed"})
	// With the hook gone, a fix-up report would reach every later effect: the
	// lineage guard's compare answers only the reported head, whose ledger seed
	// admits it, and the pass delta widens a workflow permission.
	g.set(func(g *rhForge) {
		g.compare[rhBaseRef] = []string{rhH2}
		g.compare[rhBaseSHA] = []string{rhH2}
		g.changed = []string{driftWFPath}
		g.files[rhBaseSHA+"|"+driftWFPath] = wfBase
		g.files[rhH2+"|"+driftWFPath] = wfWidened
	})
	return f
}

func (f *rhFixture) untrack() { f.runRow.PullRequestURL = nil }

// seed writes a prior audit row at an explicit chain sequence.
func (f *rhFixture) seed(category string, stageID uuid.UUID, seq int64, payload any) {
	f.t.Helper()
	raw, ok := payload.(json.RawMessage)
	if !ok {
		var err error
		raw, err = json.Marshal(payload)
		if err != nil {
			f.t.Fatalf("setup: marshal %s: %v", category, err)
		}
	}
	rid, sid := f.runRow.ID, stageID
	f.au.mu.Lock()
	defer f.au.mu.Unlock()
	f.au.seeded = append(f.au.seeded, &audit.Entry{RunID: &rid, StageID: &sid, Category: category, Sequence: seq, Payload: raw})
}

// seedFixupTrigger records a fix-up trigger whose anchors restore the implement
// stage to succeeded and re-park the review stage.
func (f *rhFixture) seedFixupTrigger(seq int64) {
	f.t.Helper()
	f.seed(CategoryStageFixupTriggered, f.impl.ID, seq, map[string]any{
		"stage_id": f.impl.ID.String(), "prior_state": string(run.StageStateSucceeded),
		"reparked_review_stage_id": f.review.ID.String(), "concern_ids": []string{}, "pass_ordinal": 1,
	})
}

// seedConflictTrigger records a live conflict-resolution trigger.
func (f *rhFixture) seedConflictTrigger(seq int64) {
	f.t.Helper()
	f.seed(CategoryStageConflictResolutionTriggered, f.impl.ID, seq, conflictResolutionTrigger{
		Branch: rhPRBranch, BaseRef: rhBaseRef, ExpectedHeadSHA: rhH1, Pass: 1,
		PriorState: string(run.StageStateSucceeded), ReparkedReviewStageID: f.review.ID.String(),
	})
}

// seedVerifyTrigger records a live merge-candidate verify trigger, naming a
// superseded fix-up trigger when supersedes is non-zero.
func (f *rhFixture) seedVerifyTrigger(seq, supersedes int64) {
	f.t.Helper()
	f.seed(CategoryStageMergeCandidateVerifyTriggered, f.impl.ID, seq, mergeCandidateVerifyTrigger{
		Branch: rhPRBranch, BaseRef: rhBaseRef, ExpectedHeadSHA: rhH1, Cause: mergeCandidateCauseBaseAdvance,
		VerifyCommand: "scripts/verify", PriorState: string(run.StageStateSucceeded),
		ReparkedReviewStageID: f.review.ID.String(), SupersedesFixupTriggerSequence: supersedes,
	})
}

func (f *rhFixture) stageState(id uuid.UUID) *run.Stage {
	f.t.Helper()
	st, err := f.rr.GetStage(context.Background(), id)
	if err != nil {
		f.t.Fatalf("setup: GetStage: %v", err)
	}
	return st
}

// rowPayloads returns every appended row of a category, decoded.
func (f *rhFixture) rowPayloads(category string) []map[string]any {
	f.t.Helper()
	f.au.mu.Lock()
	defer f.au.mu.Unlock()
	var out []map[string]any
	for _, p := range f.au.appended {
		if p.Category != category {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(p.Payload, &m); err != nil {
			f.t.Fatalf("setup: decode %s: %v", category, err)
		}
		out = append(out, m)
	}
	return out
}

// mismatchRows returns the reported_head_mismatch evidence rows.
func (f *rhFixture) mismatchRows() []map[string]any {
	f.t.Helper()
	var out []map[string]any
	for _, m := range f.rowPayloads(invariantmonitor.CategoryInvariantViolation) {
		if m["kind"] == invariantmonitor.KindReportedHeadMismatch {
			out = append(out, m)
		}
	}
	return out
}

// rhKindCase names one of the three push reports.
type rhKindCase struct {
	name     string
	outcome  string
	category string
}

var (
	rhFixupKind      = rhKindCase{"fixup", "fixup_pushed", CategoryFixupPushed}
	rhConflictKind   = rhKindCase{"conflict", "conflict_resolution_pushed", CategoryConflictResolutionPushed}
	rhAcceptanceKind = rhKindCase{"acceptance", outcomeAcceptanceScenariosPushed, CategoryAcceptanceScenariosPushed}
	rhAllKinds       = []rhKindCase{rhFixupKind, rhConflictKind, rhAcceptanceKind}
	rhEdgeKinds      = []rhKindCase{rhConflictKind, rhAcceptanceKind}
)

// reportingStage is the stage a kind reports against, in its reporting state.
func (f *rhFixture) reportingStage(kc rhKindCase) *run.Stage {
	if kc == rhAcceptanceKind {
		f.acceptance.State = run.StageStateRunning
		return f.acceptance
	}
	return f.impl
}

// post sends a signed report through the real route and drains background work.
func (f *rhFixture) post(kc rhKindCase, head, base, branch string) *httptest.ResponseRecorder {
	f.t.Helper()
	body, err := json.Marshal(map[string]any{
		"outcome": kc.outcome, "branch": branch, "head_sha": head, "base_sha": base, "files_changed_count": 1,
	})
	if err != nil {
		f.t.Fatalf("setup: marshal report: %v", err)
	}
	priv, _ := f.sf.issue(f.t, f.runRow.ID)
	w := shipPRRequest(f.t, f.s, f.runRow.ID, f.reportingStage(kc).ID, priv, body, "")
	f.s.waitBackgroundReviews()
	return w
}

// assertRefused pins a confirmed-mismatch refusal: no row, one evidence row, and
// the kind's wire outcome (200 failed, or 409 for acceptance).
func (f *rhFixture) assertRefused(kc rhKindCase, w *httptest.ResponseRecorder, wantCategory, wantReason string) {
	f.t.Helper()
	if kc == rhAcceptanceKind {
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"reported_head_mismatch"`) {
			f.t.Errorf("assert: status = %d, want 409 reported_head_mismatch:\n%s", w.Code, w.Body.String())
		}
	} else {
		var resp pullRequestFailureResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if w.Code != http.StatusOK || resp.Outcome != "failed" || resp.Category != wantCategory {
			f.t.Errorf("assert: status = %d outcome = %q category = %q, want 200 failed %s:\n%s",
				w.Code, resp.Outcome, resp.Category, wantCategory, w.Body.String())
		}
	}
	if rows := f.rowPayloads(kc.category); len(rows) != 0 {
		f.t.Errorf("assert: %s rows = %d, want 0: %+v", kc.category, len(rows), rows)
	}
	// Errorf, not Fatalf: with a hook deleted, the later-effect assertions after
	// this one must still run, or they would never be shown to be live.
	rows := f.mismatchRows()
	if len(rows) != 1 {
		f.t.Errorf("assert: reported_head_mismatch rows = %d, want 1", len(rows))
		return
	}
	if rows[0]["outcome"] != kc.outcome || rows[0]["reason"] != wantReason {
		f.t.Errorf("assert: evidence outcome/reason = %v/%v, want %s/%s", rows[0]["outcome"], rows[0]["reason"], kc.outcome, wantReason)
	}
}

// assertUnverifiable pins the retryable refusal: 503, nothing recorded, no
// transition.
func (f *rhFixture) assertUnverifiable(kc rhKindCase, w *httptest.ResponseRecorder, stage *run.Stage, wantState run.StageState) {
	f.t.Helper()
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"reported_head_unverifiable"`) {
		f.t.Errorf("assert: status = %d, want 503 reported_head_unverifiable:\n%s", w.Code, w.Body.String())
	}
	if rows := f.rowPayloads(kc.category); len(rows) != 0 {
		f.t.Errorf("assert: %s rows = %d, want 0", kc.category, len(rows))
	}
	if rows := f.rowPayloads("pull_request_failed"); len(rows) != 0 {
		f.t.Errorf("assert: pull_request_failed rows = %d, want 0: %+v", len(rows), rows)
	}
	if rows := f.mismatchRows(); len(rows) != 0 {
		f.t.Errorf("assert: reported_head_mismatch rows = %d, want 0", len(rows))
	}
	if got := f.stageState(stage.ID).State; got != wantState {
		f.t.Errorf("assert: stage state = %s, want %s", got, wantState)
	}
}

// assertLoggedCause pins which undeterminable arm answered. The class reaches the
// operator log alone, so an arm masked by a later read still shows as a change.
func (f *rhFixture) assertLoggedCause(cause string) {
	f.t.Helper()
	if !strings.Contains(f.logs.String(), `"cause":"`+cause) {
		f.t.Errorf("assert: the operator log does not name cause %q:\n%s", cause, f.logs.String())
	}
}

// assertLaterEffectsAbsent pins that the hook ran before every later effect.
func (f *rhFixture) assertLaterEffectsAbsent() {
	f.t.Helper()
	if n := f.forge.count("check_run"); n != 0 {
		f.t.Errorf("assert: check-run POSTs = %d, want 0", n)
	}
	if rows := f.rowPayloads(permissionDriftDetectedCategory); len(rows) != 0 {
		f.t.Errorf("assert: %s rows = %d, want 0", permissionDriftDetectedCategory, len(rows))
	}
	if rows := f.rowPayloads("implement_review_started"); len(rows) != 0 {
		f.t.Errorf("assert: re-review backstop rows = %d, want 0", len(rows))
	}
}

// assertRestored pins recovery to the pre-pass gate.
func (f *rhFixture) assertRestored() {
	f.t.Helper()
	if got := f.stageState(f.impl.ID).State; got != run.StageStateSucceeded {
		f.t.Errorf("assert: implement state = %s, want succeeded", got)
	}
	if got := f.stageState(f.review.ID).State; got != run.StageStateAwaitingApproval {
		f.t.Errorf("assert: review state = %s, want awaiting_approval", got)
	}
}

// assertFailedCRetryable pins an unrecovered category-C failure a retry admits.
func (f *rhFixture) assertFailedCRetryable() {
	f.t.Helper()
	st := f.stageState(f.impl.ID)
	if st.State != run.StageStateFailed || st.FailureCategory == nil || *st.FailureCategory != run.FailureC {
		got := "none"
		if st.FailureCategory != nil {
			got = string(*st.FailureCategory)
		}
		f.t.Fatalf("assert: implement = %s category %s, want failed C", st.State, got)
	}
	if w := postRetry(f.t, f.s, f.impl.ID); w.Code != http.StatusOK {
		f.t.Errorf("assert: retry status = %d, want 200 on a category-C stage:\n%s", w.Code, w.Body.String())
	}
}

// failedRow returns the single pull_request_failed row.
func (f *rhFixture) failedRow() map[string]any {
	f.t.Helper()
	rows := f.rowPayloads("pull_request_failed")
	if len(rows) != 1 {
		f.t.Errorf("assert: pull_request_failed rows = %d, want 1", len(rows))
		return map[string]any{}
	}
	return rows[0]
}

// TestReportedHead_FixupPushed_Refused: a fix-up report naming a head the forge
// does not show, on a live trigger, keeps category-B recovery and its pass budget
// and reaches no later effect.
//
// COUNTERFACTUAL (CF1, the hook call replaced with false): the lineage guard is
// then the only control, and the fixture's compare answers only H2 with the
// ledger seeded by H2, so the guard passes, the row lands and zero-rows goes RED.
func TestReportedHead_FixupPushed_Refused(t *testing.T) {
	f := newRHFixture(t)
	f.seedFixupTrigger(10)
	f.forge.set(func(g *rhForge) {
		g.prHead = rhH1
		g.tips[rhPRBranch] = rhH1
	})

	w := f.post(rhFixupKind, rhH2, rhBaseSHA, rhPRBranch)

	f.assertRefused(rhFixupKind, w, "B", reportedHeadReasonHead)
	failed := f.failedRow()
	if failed["category"] != "B" {
		t.Errorf("assert: pull_request_failed category = %v, want B", failed["category"])
	}
	reason, _ := failed["reason"].(string)
	if !strings.Contains(reason, rhH1) || !strings.Contains(reason, rhH2) {
		t.Errorf("assert: pull_request_failed reason does not name both heads: %q", reason)
	}
	if _, ok := failed["push_checkpoint"]; ok {
		t.Errorf("assert: a refused head was recorded as a push checkpoint: %+v", failed)
	}
	if rows := f.rowPayloads(CategoryPushResumeCheckpoint); len(rows) != 0 {
		t.Errorf("assert: push-resume rows = %d, want 0", len(rows))
	}
	f.assertRestored()
	if got := f.stageState(f.acceptance.ID).State; got != run.StageStateSucceeded {
		t.Errorf("assert: settled acceptance stage = %s, want it left succeeded", got)
	}
	recovered := f.rowPayloads(CategoryStageFixupRecovered)
	if len(recovered) != 1 || recovered[0]["source_failure_category"] != "B" || recovered[0]["delivered_nothing"] != true {
		t.Errorf("assert: recovery rows = %+v, want one with source B and delivered_nothing", recovered)
	}
	ctx := context.Background()
	refunded, _, err := f.s.fixupRefundedPasses(ctx, f.runRow.ID, f.impl.ID)
	if err != nil || refunded != 0 {
		t.Errorf("assert: refunded passes = %d (%v), want 0: the pass stays consumed", refunded, err)
	}
	if passes, err := f.s.countFixupPasses(ctx, f.runRow.ID, f.impl.ID); err != nil || passes != 1 {
		t.Errorf("assert: fix-up passes = %d (%v), want 1", passes, err)
	}
	f.assertLaterEffectsAbsent()
}

// TestReportedHead_LiveFixupTrigger_CategoryB: each shape in which the newest
// trigger is still live keeps category B and restoration.
//
// COUNTERFACTUAL (M51, the settled comparison dropped): the first pass's push is
// then read as settling the second pass, so it fails category C → RED.
func TestReportedHead_LiveFixupTrigger_CategoryB(t *testing.T) {
	cases := map[string]func(f *rhFixture){
		"second_pass_after_settled_first": func(f *rhFixture) {
			f.seedFixupTrigger(10)
			f.seed(CategoryFixupPushed, f.impl.ID, 11, map[string]any{"head_sha": rhOther})
			f.seedFixupTrigger(12)
		},
		"reaper_death_then_retry": func(f *rhFixture) {
			f.seedFixupTrigger(10)
			f.seed(CategoryDispatchReaperFailed, f.impl.ID, 11, map[string]any{"failure_category": "C"})
		},
		"inert_supersession_marker": func(f *rhFixture) {
			f.seedFixupTrigger(10)
			f.seed(CategoryStageFixupSuperseded, f.impl.ID, 11, stageFixupSupersededPayload{TriggerSequence: 10})
		},
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRHFixture(t)
			seed(f)
			f.forge.set(func(g *rhForge) {
				g.prHead = rhH1
				g.tips[rhPRBranch] = rhH1
			})
			w := f.post(rhFixupKind, rhH2, rhBaseSHA, rhPRBranch)
			f.assertRefused(rhFixupKind, w, "B", reportedHeadReasonHead)
			if got := f.failedRow()["category"]; got != "B" {
				t.Errorf("assert: pull_request_failed category = %v, want B", got)
			}
			f.assertRestored()
		})
	}
}

// TestReportedHead_NoTrigger_FailedC: with no trigger of any kind the refusal
// fails the stage category C, and the same stage can be retried.
//
// COUNTERFACTUAL (M28, the category rule forced to B): the stage fails B, so the
// category and the retry both go RED.
func TestReportedHead_NoTrigger_FailedC(t *testing.T) {
	for _, kc := range []rhKindCase{rhFixupKind, rhConflictKind} {
		t.Run(kc.name, func(t *testing.T) {
			f := newRHFixture(t)
			f.forge.set(func(g *rhForge) {
				g.prHead = rhH1
				g.tips[rhPRBranch] = rhH1
				g.parents[rhH2] = []string{rhBaseSHA}
			})
			w := f.post(kc, rhH2, rhBaseSHA, rhPRBranch)
			f.assertRefused(kc, w, "C", reportedHeadReasonHead)
			if got := f.failedRow()["category"]; got != "C" {
				t.Errorf("assert: pull_request_failed category = %v, want C", got)
			}
			if rows := f.rowPayloads(CategoryStageFixupRecovered); len(rows) != 0 {
				t.Errorf("assert: stage_fixup_recovered rows = %d, want 0", len(rows))
			}
			f.assertFailedCRetryable()

			// The contrast the category exists for: a category-B stage answers 422.
			b := f.rr.seedStage(f.runRow.ID, 9, run.StageStateFailed)
			catB := run.FailureB
			b.Type = run.StageTypeImplement
			b.FailureCategory = &catB
			if w := postRetry(t, f.s, b.ID); w.Code != http.StatusUnprocessableEntity {
				t.Errorf("assert: retry of a category-B stage = %d, want 422", w.Code)
			}
		})
	}
}

// TestReportedHead_SpentFixupTrigger_FailedC: a historical trigger is not active.
// The refusal fails C and does not restore from the stale anchor.
//
// COUNTERFACTUALS: M36 (skipFixupRecovery false) restores from the spent anchor
// and re-parks review, which reddens stage state and the row count. M30 (settled
// comparison never true) reads the trigger as active, so category B goes RED.
func TestReportedHead_SpentFixupTrigger_FailedC(t *testing.T) {
	settling := map[string]string{
		"pushed":     CategoryFixupPushed,
		"no_changes": CategoryFixupNoChanges,
		"recovered":  CategoryStageFixupRecovered,
	}
	for name, category := range settling {
		t.Run(name, func(t *testing.T) {
			f := newRHFixture(t)
			f.seedFixupTrigger(10)
			f.seed(category, f.impl.ID, 11, map[string]any{"head_sha": rhOther})
			f.forge.set(func(g *rhForge) {
				g.prHead = rhH1
				g.tips[rhPRBranch] = rhH1
			})
			w := f.post(rhFixupKind, rhH2, rhBaseSHA, rhPRBranch)
			f.assertRefused(rhFixupKind, w, "C", reportedHeadReasonHead)
			if got := f.failedRow()["category"]; got != "C" {
				t.Errorf("assert: pull_request_failed category = %v, want C", got)
			}
			if rows := f.rowPayloads(CategoryStageFixupRecovered); len(rows) != 0 {
				t.Errorf("assert: new stage_fixup_recovered rows = %d, want 0", len(rows))
			}
			if got := f.stageState(f.review.ID).State; got != run.StageStatePending {
				t.Errorf("assert: review state = %s, want pending (unchanged)", got)
			}
			f.assertFailedCRetryable()
		})
	}

	// COUNTERFACTUAL (M31, the supersession result ignored): the trigger reads as
	// active, so the failed row's category B goes RED. The verify recovery settles
	// first either way.
	t.Run("confirmed_supersession", func(t *testing.T) {
		f := newRHFixture(t)
		f.seedFixupTrigger(10)
		f.seed(CategoryStageFixupSuperseded, f.impl.ID, 11, stageFixupSupersededPayload{TriggerSequence: 10})
		f.seedVerifyTrigger(12, 10)
		f.forge.set(func(g *rhForge) {
			g.prHead = rhH1
			g.tips[rhPRBranch] = rhH1
		})
		w := f.post(rhFixupKind, rhH2, rhBaseSHA, rhPRBranch)
		f.assertRefused(rhFixupKind, w, "C", reportedHeadReasonHead)
		if got := f.failedRow()["category"]; got != "C" {
			t.Errorf("assert: pull_request_failed category = %v, want C", got)
		}
		if rows := f.rowPayloads(CategoryMergeCandidateVerified); len(rows) != 1 || rows[0]["result"] != mergeCandidateResultNotExecuted {
			t.Errorf("assert: merge_candidate_verified rows = %+v, want one not_executed (the verify pass settles first)", rows)
		}
		f.assertRestored()
	})
}

// TestReportedHead_TriggerStateUnreadable_Is503: an unreadable or indeterminate
// required read answers 503 before any transition.
//
// COUNTERFACTUALS: M32 (the trigger read error swallowed) reads no active trigger
// and fails the stage. M33, M34 and M35 do the same for the malformed payload,
// the conflict reads and the verify reads.
func TestReportedHead_TriggerStateUnreadable_Is503(t *testing.T) {
	type tc struct {
		category string
		kind     rhKindCase
		seed     func(f *rhFixture)
	}
	liveFixup := func(f *rhFixture) { f.seedFixupTrigger(10) }
	liveVerify := func(f *rhFixture) { f.seedVerifyTrigger(10, 0) }
	cases := []tc{
		{CategoryStageFixupTriggered, rhFixupKind, nil},
		{CategoryFixupNoChanges, rhFixupKind, liveFixup},
		{CategoryStageFixupRecovered, rhFixupKind, liveFixup},
		{CategoryStageFixupSuperseded, rhFixupKind, liveFixup},
		{CategoryStageMergeCandidateVerifyTriggered, rhFixupKind, nil},
		{CategoryMergeCandidateVerified, rhFixupKind, liveVerify},
		{CategoryStageConflictResolutionTriggered, rhFixupKind, nil},
		{CategoryStageConflictResolutionFailed, rhFixupKind, nil},
		{CategoryFixupPushed, rhConflictKind, liveFixup},
		{CategoryConflictResolutionPushed, rhFixupKind, nil},
	}
	for _, c := range cases {
		t.Run(c.category+"/"+c.kind.name, func(t *testing.T) {
			f := newRHFixture(t)
			if c.seed != nil {
				c.seed(f)
			}
			f.forge.set(func(g *rhForge) {
				g.prHead = rhH1
				g.tips[rhPRBranch] = rhH1
				g.parents[rhH2] = []string{rhBaseSHA}
			})
			f.au.listByCategoryErrCategory = c.category
			w := f.post(c.kind, rhH2, rhBaseSHA, rhPRBranch)
			f.assertUnverifiable(c.kind, w, f.impl, run.StageStateRunning)
		})
	}
	t.Run("malformed_newest_trigger", func(t *testing.T) {
		f := newRHFixture(t)
		f.seedFixupTrigger(10)
		f.seed(CategoryStageFixupTriggered, f.impl.ID, 11, json.RawMessage(`{"prior_state":`))
		f.forge.set(func(g *rhForge) {
			g.prHead = rhH1
			g.tips[rhPRBranch] = rhH1
		})
		w := f.post(rhFixupKind, rhH2, rhBaseSHA, rhPRBranch)
		f.assertUnverifiable(rhFixupKind, w, f.impl, run.StageStateRunning)
	})
}

// TestReportedHead_ConflictResolutionPushed_Refused: a live conflict trigger is
// consumed and restored, with no row and no later effect.
//
// COUNTERFACTUAL (CF2, the hook call replaced with false): this handler has no
// other guard, so the row lands and zero-rows goes RED.
func TestReportedHead_ConflictResolutionPushed_Refused(t *testing.T) {
	f := newRHFixture(t)
	f.seedConflictTrigger(10)
	f.forge.set(func(g *rhForge) {
		g.prHead = rhH1
		g.tips[rhPRBranch] = rhH1
		g.parents[rhH2] = []string{rhBaseSHA}
	})

	w := f.post(rhConflictKind, rhH2, rhBaseSHA, rhPRBranch)

	f.assertRefused(rhConflictKind, w, "C", reportedHeadReasonHead)
	if rows := f.rowPayloads(CategoryStageConflictResolutionFailed); len(rows) != 1 {
		t.Errorf("assert: stage_conflict_resolution_failed rows = %d, want 1 (the trigger is consumed)", len(rows))
	}
	f.assertRestored()
	f.assertLaterEffectsAbsent()
}

// TestReportedHead_AcceptanceScenariosPushed_Refused: an acceptance mismatch
// answers 409 and transitions nothing.
//
// COUNTERFACTUAL (CF3, the hook call replaced with false): this handler has no
// other guard, so the row lands and zero-rows goes RED.
func TestReportedHead_AcceptanceScenariosPushed_Refused(t *testing.T) {
	f := newRHFixture(t)
	f.forge.set(func(g *rhForge) {
		g.prHead = rhH1
		g.tips[rhPRBranch] = rhH1
		g.parents[rhH2] = []string{rhBaseSHA}
	})

	w := f.post(rhAcceptanceKind, rhH2, rhBaseSHA, rhPRBranch)

	f.assertRefused(rhAcceptanceKind, w, "", reportedHeadReasonHead)
	if got := f.stageState(f.acceptance.ID).State; got != run.StageStateRunning {
		t.Errorf("assert: acceptance state = %s, want running (unchanged)", got)
	}
	if rows := f.rowPayloads("pull_request_failed"); len(rows) != 0 {
		t.Errorf("assert: pull_request_failed rows = %d, want 0", len(rows))
	}
	if got := f.stageState(f.impl.ID).State; got != run.StageStateRunning {
		t.Errorf("assert: implement state = %s, want running (unchanged)", got)
	}
}

// TestReportedHead_AncestorOfTipRefused: a commit that is merely reachable is
// refused, and no ancestry read happens.
//
// COUNTERFACTUAL (CF4, the tip comparison neutered): the fixture's PR head and
// tip are newer than the reported head, whose parents read true, so every handler
// writes the row and zero-rows goes RED.
func TestReportedHead_AncestorOfTipRefused(t *testing.T) {
	for _, kc := range rhAllKinds {
		t.Run(kc.name, func(t *testing.T) {
			f := newRHFixture(t)
			f.forge.set(func(g *rhForge) {
				g.prHead = rhH3
				g.tips[rhPRBranch] = rhH3
				g.parents[rhH3] = []string{rhH2}
				g.parents[rhH2] = []string{rhBaseSHA}
			})
			w := f.post(kc, rhH2, rhBaseSHA, rhPRBranch)
			f.assertRefused(kc, w, "C", reportedHeadReasonHead)
			if n := f.forge.count("compare"); n != 0 {
				t.Errorf("assert: compare calls = %d, want 0: reachability is never consulted", n)
			}
		})
	}
}

// rhEdgeArm puts the reported head on one acceptance arm of the head check, so
// only the first-parent check stands.
type rhEdgeArm struct {
	name    string
	noPR    bool
	prepare func(f *rhFixture, g *rhForge)
}

var rhEdgeArms = []rhEdgeArm{
	{name: "pr_head_arm", prepare: func(_ *rhFixture, g *rhForge) {
		g.prHead = rhH2
		g.tips[rhPRBranch] = rhH3
	}},
	{name: "tip_arm", prepare: func(_ *rhFixture, g *rhForge) {
		g.prHead = rhH1
		g.tips[rhPRBranch] = rhH2
	}},
	{name: "no_pr_tip_arm", noPR: true, prepare: func(f *rhFixture, g *rhForge) {
		g.tips[fixupBranchFor(f.runRow, f.impl)] = rhH2
	}},
}

// edgeArmsFor keeps the no-PR arm on acceptance, the only kind that reports with
// no tracked pull request.
func edgeArmsFor(kc rhKindCase) []rhEdgeArm {
	if kc == rhAcceptanceKind {
		return rhEdgeArms
	}
	return rhEdgeArms[:2]
}

// TestReportedHead_EdgeBaseNotFirstParent_Refused: on every arm that accepts the
// head, a base that is no parent or only the second parent is refused.
//
// COUNTERFACTUAL (CF5, the first-parent check neutered): the head check accepts
// by construction, so the row lands and zero-rows goes RED.
func TestReportedHead_EdgeBaseNotFirstParent_Refused(t *testing.T) {
	parents := map[string][]string{
		"not_a_parent":  {rhOther},
		"second_parent": {rhOther, rhBaseSHA},
		"no_parents":    {},
	}
	for _, kc := range rhEdgeKinds {
		for _, arm := range edgeArmsFor(kc) {
			for name, ps := range parents {
				t.Run(kc.name+"/"+arm.name+"/"+name, func(t *testing.T) {
					f := newRHFixture(t)
					if arm.noPR {
						f.untrack()
					}
					f.forge.set(func(g *rhForge) {
						arm.prepare(f, g)
						g.parents[rhH2] = ps
					})
					w := f.post(kc, rhH2, rhBaseSHA, rhPRBranch)
					f.assertRefused(kc, w, "C", reportedHeadReasonBaseEdge)
				})
			}
		}
	}
}

// TestReportedHead_EdgeFirstParent_Accepted is the positive pair on the same arms.
func TestReportedHead_EdgeFirstParent_Accepted(t *testing.T) {
	for _, kc := range rhEdgeKinds {
		for _, arm := range edgeArmsFor(kc) {
			t.Run(kc.name+"/"+arm.name, func(t *testing.T) {
				f := newRHFixture(t)
				if arm.noPR {
					f.untrack()
				}
				f.forge.set(func(g *rhForge) {
					arm.prepare(f, g)
					g.parents[rhH2] = []string{rhBaseSHA, rhOther}
				})
				w := f.post(kc, rhH2, rhBaseSHA, rhPRBranch)
				if w.Code != http.StatusOK {
					t.Fatalf("assert: status = %d, want 200:\n%s", w.Code, w.Body.String())
				}
				if rows := f.rowPayloads(kc.category); len(rows) != 1 || rows[0]["head_sha"] != rhH2 || rows[0]["base_sha"] != rhBaseSHA {
					t.Errorf("assert: %s rows = %+v, want one at the reported edge", kc.category, rows)
				}
				if rows := f.mismatchRows(); len(rows) != 0 {
					t.Errorf("assert: reported_head_mismatch rows = %d, want 0", len(rows))
				}
			})
		}
	}
}

// TestReportedHead_ForgeErrorWritesNoRow: a failed forge read is unknown, never a
// mismatch: 503, no row, no evidence, no transition.
//
// COUNTERFACTUAL (CF6, the undeterminable branch neutered): the failure enters the
// refusal path, so the 503 and still-running assertions go RED.
func TestReportedHead_ForgeErrorWritesNoRow(t *testing.T) {
	type tc struct {
		name  string
		kinds []rhKindCase
		cause string
		setup func(g *rhForge)
	}
	cases := []tc{
		{"pull_500", rhAllKinds, "pull_request_read", func(g *rhForge) { g.status["pull"] = http.StatusInternalServerError }},
		{"ref_500", rhAllKinds, "branch_ref_read", func(g *rhForge) {
			g.prHead = rhH1
			g.status["ref"] = http.StatusInternalServerError
		}},
		{"pull_empty_head_ref", rhAllKinds, "pull_request_head_ref_empty", func(g *rhForge) {
			g.prHead = rhH1
			g.prRef = ""
		}},
		{"commit_500", rhEdgeKinds, "commit_read", func(g *rhForge) {
			g.prHead = rhH2
			g.status["commit"] = http.StatusInternalServerError
		}},
		{"commit_404", rhEdgeKinds, "commit_read", func(g *rhForge) { g.prHead = rhH2 }},
	}
	for _, c := range cases {
		for _, kc := range c.kinds {
			t.Run(c.name+"/"+kc.name, func(t *testing.T) {
				f := newRHFixture(t)
				f.forge.set(c.setup)
				w := f.post(kc, rhH2, rhBaseSHA, rhPRBranch)
				f.assertUnverifiable(kc, w, f.reportingStage(kc), run.StageStateRunning)
				f.assertLoggedCause(c.cause)
			})
		}
	}
}

// TestReportedHead_BranchResolutionRepoErrorIs503: a repository failure resolving
// the branch is unknown, never a mismatch or a failure category.
//
// COUNTERFACTUAL (CF7, the child-probe error swallowed): the fallback per-stage
// branch has a tip other than the head, so the swallow produces a mismatch and a
// failed stage instead of 503.
func TestReportedHead_BranchResolutionRepoErrorIs503(t *testing.T) {
	boom := errors.New("repository down")
	cases := []struct {
		name  string
		kind  rhKindCase
		cause string
		setup func(f *rhFixture)
	}{
		{"unparseable_repository", rhFixupKind, "repository_unparseable", func(f *rhFixture) { f.runRow.Repo = "not-a-repository" }},
		{"get_run_error", rhFixupKind, "run_read", func(f *rhFixture) { f.rr.getRunErr = boom }},
		{"list_runs_error", rhFixupKind, "branch_resolution", func(f *rhFixture) {
			f.untrack()
			f.rr.listRunsErr = boom
			f.forge.set(func(g *rhForge) { g.tips[fixupBranchFor(f.runRow, f.impl)] = rhH1 })
		}},
		{"list_stages_error", rhAcceptanceKind, "branch_resolution", func(f *rhFixture) {
			f.untrack()
			f.rr.listStagesErr = boom
		}},
		{"no_implement_stage", rhAcceptanceKind, "branch_resolution", func(f *rhFixture) {
			f.untrack()
			f.impl.Type = run.StageTypeReview
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRHFixture(t)
			c.setup(f)
			stage := f.reportingStage(c.kind)
			w := f.post(c.kind, rhH2, rhBaseSHA, rhPRBranch)
			f.assertUnverifiable(c.kind, w, stage, run.StageStateRunning)
			f.assertLoggedCause(c.cause)
		})
	}
}

// TestReportedHead_DerivedBranchNames pins the branch each no-PR run shape checks.
func TestReportedHead_DerivedBranchNames(t *testing.T) {
	// A child needs no probe: a failing child probe cannot refuse its report.
	t.Run("decomposed_child", func(t *testing.T) {
		f := newRHFixture(t)
		f.untrack()
		parent := uuid.New()
		idx := 2
		f.runRow.DecomposedFrom = &parent
		f.runRow.SliceIndex = &idx
		f.rr.listRunsErr = errors.New("repository down")
		f.forge.set(func(g *rhForge) { g.tips[fixupBranchFor(f.runRow, f.impl)] = rhH2 })
		if w := f.post(rhFixupKind, rhH2, rhBaseSHA, rhPRBranch); w.Code != http.StatusOK || len(f.rowPayloads(CategoryFixupPushed)) != 1 {
			t.Errorf("assert: status = %d rows = %d, want an accepted report on the slice branch:\n%s",
				w.Code, len(f.rowPayloads(CategoryFixupPushed)), w.Body.String())
		}
	})
	t.Run("decomposed_parent", func(t *testing.T) {
		f := newRHFixture(t)
		f.untrack()
		child := f.rr.seedRun()
		child.DecomposedFrom = &f.runRow.ID
		branch, err := f.s.reportedHeadDerivedBranch(context.Background(), f.runRow, f.impl)
		if err != nil || !strings.HasSuffix(branch, "-consolidated") {
			t.Errorf("assert: derived branch = %q (%v), want the consolidated branch", branch, err)
		}
	})
	t.Run("current_implement_stage_names_branch", func(t *testing.T) {
		f := newRHFixture(t)
		f.untrack()
		newer := f.rr.seedStage(f.runRow.ID, 9, run.StageStateSucceeded)
		newer.Type = run.StageTypeImplement
		want := fixupBranchFor(f.runRow, f.impl)
		f.forge.set(func(g *rhForge) { g.tips[want] = rhH2 })
		w := f.post(rhFixupKind, rhH2, rhBaseSHA, rhPRBranch)
		if rows := f.rowPayloads(CategoryFixupPushed); w.Code != http.StatusOK || len(rows) != 1 || rows[0]["branch"] != want {
			t.Errorf("assert: status = %d rows = %+v, want one on %s:\n%s", w.Code, rows, want, w.Body.String())
		}
	})
	t.Run("newest_implement_stage_for_acceptance", func(t *testing.T) {
		f := newRHFixture(t)
		f.untrack()
		newer := f.rr.seedStage(f.runRow.ID, 9, run.StageStateSucceeded)
		newer.Type = run.StageTypeImplement
		want := fixupBranchFor(f.runRow, newer)
		f.forge.set(func(g *rhForge) {
			g.tips[fixupBranchFor(f.runRow, f.impl)] = rhOther
			g.tips[want] = rhH2
			g.parents[rhH2] = []string{rhBaseSHA}
		})
		w := f.post(rhAcceptanceKind, rhH2, rhBaseSHA, rhPRBranch)
		if rows := f.rowPayloads(CategoryAcceptanceScenariosPushed); w.Code != http.StatusOK || len(rows) != 1 || rows[0]["branch"] != want {
			t.Errorf("assert: status = %d rows = %+v, want one on %s:\n%s", w.Code, rows, want, w.Body.String())
		}
	})
}

// TestReportedHead_FixupTriggerActive_ConversionRead: the conversion read happens
// only when a marker exists, and its failure is indeterminate. A direct call,
// because the recovery-state read ahead of it fails first on the same category
// when a read error persists; only a read that recovers and then fails reaches it.
func TestReportedHead_FixupTriggerActive_ConversionRead(t *testing.T) {
	ctx := context.Background()
	t.Run("marker_with_unreadable_conversions_is_an_error", func(t *testing.T) {
		f := newRHFixture(t)
		f.seedFixupTrigger(10)
		f.seed(CategoryStageFixupSuperseded, f.impl.ID, 11, stageFixupSupersededPayload{TriggerSequence: 10})
		f.au.listByCategoryErrCategory = CategoryStageMergeCandidateVerifyTriggered
		if active, err := f.s.fixupTriggerActive(ctx, f.runRow.ID, f.impl.ID); err == nil || active {
			t.Errorf("assert: active = %v err = %v, want an error", active, err)
		}
	})
	t.Run("no_marker_skips_the_conversion_read", func(t *testing.T) {
		f := newRHFixture(t)
		f.seedFixupTrigger(10)
		f.au.listByCategoryErrCategory = CategoryStageMergeCandidateVerifyTriggered
		if active, err := f.s.fixupTriggerActive(ctx, f.runRow.ID, f.impl.ID); err != nil || !active {
			t.Errorf("assert: active = %v err = %v, want an active trigger and no read", active, err)
		}
	})
}

// TestReportedHead_AcceptanceKind_NeverFailsStage: an acceptance mismatch answers
// 409 even on a running implement stage. The route refuses that pairing before the
// hook, so this calls the handler directly, as the merge-queue e2e does.
func TestReportedHead_AcceptanceKind_NeverFailsStage(t *testing.T) {
	f := newRHFixture(t)
	f.forge.set(func(g *rhForge) {
		g.prHead = rhH1
		g.tips[rhPRBranch] = rhH1
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+f.runRow.ID.String()+"/pull-request", nil)
	subject := "runner"
	f.s.recordAcceptanceScenariosPushed(rec, req, f.runRow.ID, f.impl, &pullRequestBody{
		Outcome: outcomeAcceptanceScenariosPushed, Branch: rhPRBranch, HeadSHA: rhH2, BaseSHA: rhBaseSHA,
	}, "ed25519", audit.ActorSystem, &subject)
	if rec.Code != http.StatusConflict {
		t.Errorf("assert: status = %d, want 409:\n%s", rec.Code, rec.Body.String())
	}
	if got := f.stageState(f.impl.ID).State; got != run.StageStateRunning {
		t.Errorf("assert: implement state = %s, want running (no transition)", got)
	}
}

// TestReportedHead_PRHeadMatchAccepted: the forge PR head is the first permitted
// path, and needs no branch-ref read.
func TestReportedHead_PRHeadMatchAccepted(t *testing.T) {
	for _, kc := range rhAllKinds {
		t.Run(kc.name, func(t *testing.T) {
			f := newRHFixture(t)
			f.forge.set(func(g *rhForge) {
				g.prHead = rhH2
				g.tips[rhPRBranch] = rhOther
				g.parents[rhH2] = []string{rhBaseSHA}
			})
			w := f.post(kc, rhH2, rhBaseSHA, rhPRBranch)
			if w.Code != http.StatusOK {
				t.Fatalf("assert: status = %d, want 200:\n%s", w.Code, w.Body.String())
			}
			rows := f.rowPayloads(kc.category)
			if len(rows) != 1 || rows[0]["head_sha"] != rhH2 || rows[0]["branch"] != rhPRBranch {
				t.Errorf("assert: %s rows = %+v, want one at the reported head", kc.category, rows)
			}
			if n := f.forge.count("ref"); n != 0 {
				t.Errorf("assert: branch-ref reads = %d, want 0 on an exact PR-head match", n)
			}
		})
	}
}

// TestReportedHead_PRHeadLag_AcceptedViaBranchRef: a stale PR head with the tip at
// the reported head is the second permitted path.
func TestReportedHead_PRHeadLag_AcceptedViaBranchRef(t *testing.T) {
	for _, kc := range rhAllKinds {
		t.Run(kc.name, func(t *testing.T) {
			f := newRHFixture(t)
			f.forge.set(func(g *rhForge) {
				g.prHead = rhH1
				g.tips[rhPRBranch] = rhH2
				g.parents[rhH2] = []string{rhBaseSHA}
			})
			w := f.post(kc, rhH2, rhBaseSHA, rhPRBranch)
			if w.Code != http.StatusOK {
				t.Fatalf("assert: status = %d, want 200:\n%s", w.Code, w.Body.String())
			}
			if rows := f.rowPayloads(kc.category); len(rows) != 1 || rows[0]["head_sha"] != rhH2 {
				t.Errorf("assert: %s rows = %+v, want one at the reported head", kc.category, rows)
			}
		})
	}
}

// TestReportedHead_UnwiredOrNoInstallation_Skips: the documented residuals admit
// the report with no forge call. They are not universal forge verification.
//
// COUNTERFACTUAL (M21, the installation skip neutered): the fake logs every call
// and these runs carry no installation, so the report no longer lands → RED. The
// nil-forge arm is declared unverifiable by counterfactual: with no client a
// mutated arm dereferences nil and panics, which the RED rule rejects, so this
// positive subtest alone pins it.
func TestReportedHead_UnwiredOrNoInstallation_Skips(t *testing.T) {
	cases := map[string]func(f *rhFixture){
		"nil_forge_client": func(f *rhFixture) {
			f.s.cfg.GitHub = nil
			f.s.auditCheckPublisher = nil // this fixture's publisher holds its own client
		},
		"nil_installation":      func(f *rhFixture) { f.runRow.InstallationID = nil },
		"zero_installation":     func(f *rhFixture) { f.runRow.InstallationID = instID(0) },
		"gitlab_shaped_no_inst": func(f *rhFixture) { f.runRow.InstallationID = nil; f.runRow.Repo = "group/subgroup/project" },
	}
	for name, setup := range cases {
		for _, kc := range rhAllKinds {
			t.Run(name+"/"+kc.name, func(t *testing.T) {
				f := newRHFixture(t)
				setup(f)
				w := f.post(kc, rhH2, rhBaseSHA, rhPRBranch)
				if w.Code != http.StatusOK {
					t.Fatalf("assert: status = %d, want 200:\n%s", w.Code, w.Body.String())
				}
				if rows := f.rowPayloads(kc.category); len(rows) != 1 || rows[0]["branch"] != rhPRBranch {
					t.Errorf("assert: %s rows = %+v, want one keeping the reported branch", kc.category, rows)
				}
				if calls := f.forge.allCalls(); len(calls) != 0 {
					t.Errorf("assert: forge calls = %v, want none on a skipped run", calls)
				}
			})
		}
	}
}

// rhSinks reads every diagnostic sink a refusal can write.
func (f *rhFixture) rhSinks(w *httptest.ResponseRecorder) map[string]string {
	f.t.Helper()
	sinks := map[string]string{"response": w.Body.String(), "log": f.logs.String()}
	st := f.stageState(f.impl.ID)
	if st.FailureReason != nil {
		sinks["stage_failure_reason"] = *st.FailureReason
	}
	f.au.mu.Lock()
	defer f.au.mu.Unlock()
	for _, p := range f.au.appended {
		if p.Category == "pull_request_failed" || p.Category == invariantmonitor.CategoryInvariantViolation {
			sinks[p.Category] = string(p.Payload)
		}
	}
	return sinks
}

// TestReportedHead_DiagnosticsBoundUntrustedValues (approval condition C3): no
// refusal sink echoes a reported or forge value that is not a bounded object id or
// a clean branch name, while comparisons still see the raw value. Valid values are
// the positive controls.
//
// COUNTERFACTUAL (M52, each diagnostic formatter replaced with a raw echo,
// comparisons untouched): the untrusted value reaches the sinks → RED on the
// no-echo assertion.
func TestReportedHead_DiagnosticsBoundUntrustedValues(t *testing.T) {
	const marker = "SYNTHETICMARKER"
	longHex := strings.Repeat("a", reportedHeadMaxObjectIDLen+1)
	longBranch := "fishhawk/" + marker + strings.Repeat("b", reportedHeadMaxBranchLen)
	type vals struct{ head, base, branch, forgeHead, forgeRef string }
	clean := vals{head: rhH2, base: rhBaseSHA, branch: "fishhawk/reported", forgeHead: rhH1, forgeRef: rhPRBranch}
	untrusted := map[string]struct {
		mutate func(*vals)
		absent string
	}{
		"head_non_hex":        {func(v *vals) { v.head = "zz" + marker }, marker},
		"head_line_structure": {func(v *vals) { v.head = "abc\n" + marker }, marker},
		"head_overlength":     {func(v *vals) { v.head = longHex }, longHex},
		"base_non_hex":        {func(v *vals) { v.base = marker }, marker},
		"base_line_structure": {func(v *vals) { v.base = "abc\r\n" + marker }, marker},
		"base_overlength":     {func(v *vals) { v.base = longHex }, longHex},
		"branch_line":         {func(v *vals) { v.branch = "fishhawk/x\n" + marker }, marker},
		"branch_overlength":   {func(v *vals) { v.branch = longBranch }, marker},
		"forge_head_non_hex":  {func(v *vals) { v.forgeHead = marker }, marker},
		"forge_ref_line":      {func(v *vals) { v.forgeRef = "fishhawk/y\n" + marker }, marker},
	}
	failingAppends := []string{"", invariantmonitor.CategoryInvariantViolation, "pull_request_failed"}

	run1 := func(t *testing.T, v vals, kc rhKindCase, failAppend string) (*rhFixture, map[string]string) {
		t.Helper()
		f := newRHFixture(t)
		f.forge.set(func(g *rhForge) {
			g.prHead = v.forgeHead
			g.prRef = v.forgeRef
			g.tips[v.forgeRef] = rhOther
		})
		f.au.appendErrCategory = failAppend
		w := f.post(kc, v.head, v.base, v.branch)
		if w.Code == http.StatusServiceUnavailable {
			t.Fatalf("setup: report answered 503, not the mismatch path:\n%s", w.Body.String())
		}
		return f, f.rhSinks(w)
	}

	for name, c := range untrusted {
		for _, kc := range []rhKindCase{rhFixupKind, rhAcceptanceKind} {
			for _, failAppend := range failingAppends {
				t.Run(name+"/"+kc.name+"/append_fails_"+failAppend, func(t *testing.T) {
					v := clean
					c.mutate(&v)
					_, sinks := run1(t, v, kc, failAppend)
					for sink, text := range sinks {
						if strings.Contains(text, c.absent) {
							t.Errorf("assert: sink %s echoed an untrusted value:\n%s", sink, text)
						}
					}
					if !strings.Contains(sinks["log"], "(invalid-or-absent-") {
						t.Errorf("assert: the WARN does not carry the fixed placeholder:\n%s", sinks["log"])
					}
				})
			}
		}
	}

	// Positive controls: 40 and 64 hex characters and a clean branch reach every
	// sink, so the no-echo assertions above cannot pass on an empty sink.
	for name, head := range map[string]string{"sha1": rhH2, "sha256": strings.Repeat("c", reportedHeadMaxObjectIDLen)} {
		for _, kc := range []rhKindCase{rhFixupKind, rhAcceptanceKind} {
			t.Run("valid_"+name+"/"+kc.name, func(t *testing.T) {
				v := clean
				v.head = head
				f, sinks := run1(t, v, kc, "")
				want := []string{"response", "log", invariantmonitor.CategoryInvariantViolation}
				if kc == rhFixupKind {
					want = []string{"log", invariantmonitor.CategoryInvariantViolation, "pull_request_failed", "stage_failure_reason"}
				}
				for _, sink := range want {
					text, ok := sinks[sink]
					if !ok || !strings.Contains(text, head) || !strings.Contains(text, rhH1) {
						t.Errorf("assert: sink %s does not name both valid heads:\n%s", sink, text)
					}
				}
				evidence := f.mismatchRows()
				if len(evidence) != 1 || evidence[0]["reported_branch"] != clean.branch || evidence[0]["forge_branch"] != clean.forgeRef {
					t.Errorf("assert: evidence branches = %+v, want the clean names", evidence)
				}
			})
		}
	}

	// The comparison still sees the raw value: an untrusted reported head that
	// EXACTLY equals the forge head is admitted, and its row keeps the raw bytes
	// the handler has always recorded.
	t.Run("comparison_stays_exact", func(t *testing.T) {
		raw := "zz" + marker
		f := newRHFixture(t)
		f.forge.set(func(g *rhForge) {
			g.prHead = raw
			g.compare[rhBaseRef] = []string{raw}
		})
		w := f.post(rhFixupKind, raw, rhBaseSHA, rhPRBranch)
		if w.Code != http.StatusOK {
			t.Fatalf("assert: status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		if rows := f.rowPayloads(CategoryFixupPushed); len(rows) != 1 || rows[0]["head_sha"] != raw {
			t.Errorf("assert: fixup_pushed rows = %+v, want one at the exact head", rows)
		}
	})
}

// TestReportedHead_UnverifiableCarriesNoReportedValue: the 503 path has its own
// sinks. The body names only the run and stage, the operator log carries the cause
// class, and neither names a reported value, even when an untrusted head exactly
// matched the forge and so reached the commit read.
func TestReportedHead_UnverifiableCarriesNoReportedValue(t *testing.T) {
	const marker = "SYNTHETICMARKER"
	raw := "zz" + marker
	f := newRHFixture(t)
	f.forge.set(func(g *rhForge) {
		g.prHead = raw
		g.status["commit"] = http.StatusInternalServerError
	})
	w := f.post(rhConflictKind, raw, marker, rhPRBranch)
	f.assertUnverifiable(rhConflictKind, w, f.impl, run.StageStateRunning)
	if strings.Contains(w.Body.String(), "commit_read") {
		t.Errorf("assert: 503 body carries the internal cause:\n%s", w.Body.String())
	}
	if !strings.Contains(f.logs.String(), `"cause":"commit_read: `) {
		t.Errorf("assert: the operator log does not carry the cause class:\n%s", f.logs.String())
	}
	for sink, text := range f.rhSinks(w) {
		if strings.Contains(text, marker) {
			t.Errorf("assert: sink %s echoed a reported value:\n%s", sink, text)
		}
	}
}

// TestReportedHead_FormattersPinTheirBounds pins the boundary lengths the sink
// test drives by example.
func TestReportedHead_FormattersPinTheirBounds(t *testing.T) {
	for in, want := range map[string]string{
		"":                       reportedHeadObjectIDFallback,
		"AbC123":                 "AbC123",
		"g":                      reportedHeadObjectIDFallback,
		strings.Repeat("a", 64):  strings.Repeat("a", 64),
		strings.Repeat("a", 65):  reportedHeadObjectIDFallback,
		"abc def":                reportedHeadObjectIDFallback,
		"abc" + strconv.Itoa(12): "abc12",
	} {
		if got := formatReportedHeadObjectID(in); got != want {
			t.Errorf("assert: formatReportedHeadObjectID(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"":                        reportedHeadBranchFallback,
		"fishhawk/run-a":          "fishhawk/run-a",
		"a\tb":                    reportedHeadBranchFallback,
		"   ":                     reportedHeadBranchFallback,
		"a```b":                   reportedHeadBranchFallback,
		strings.Repeat("b", 255):  strings.Repeat("b", 255),
		strings.Repeat("b", 256):  reportedHeadBranchFallback,
		string([]byte{'a', 0xff}): reportedHeadBranchFallback,
	} {
		if got := formatReportedHeadBranch(in); got != want {
			t.Errorf("assert: formatReportedHeadBranch(%q) = %q, want %q", in, got, want)
		}
	}
	many := make([]string, reportedHeadMaxParents+3)
	for i := range many {
		many[i] = fmt.Sprintf("%040x", i)
	}
	d := newReportedHeadDiagnostics(&pullRequestBody{}, reportedHeadEvidence{parents: many})
	if len(d.forgeParents) != reportedHeadMaxParents {
		t.Errorf("assert: forge parents = %d, want capped at %d", len(d.forgeParents), reportedHeadMaxParents)
	}
}
