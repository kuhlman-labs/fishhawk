package releaseevidence_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/cost"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	"github.com/kuhlman-labs/fishhawk/backend/internal/releaseevidence"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// fakeResolver fakes the GitHub commit walk so the assembly tests run
// offline. It returns a fixed PR list (and optional error) regardless of
// the range.
type fakeResolver struct {
	prs []releaseevidence.MergedPR
	err error
}

func (f fakeResolver) MergedPRsInRange(_ context.Context, _ string, _, _ string) ([]releaseevidence.MergedPR, error) {
	return f.prs, f.err
}

// harness bundles the four real Postgres repos plus a seeding API. The
// assembly layer crosses all four (run / audit / concern / artifact), so
// the integration tests seed real rows for FK integrity and assert the
// assembled ChangeEvidence end-to-end.
type harness struct {
	t         *testing.T
	ctx       context.Context
	runRepo   run.Repository
	auditRepo audit.Repository
	concRepo  concern.Repository
	artRepo   artifact.Repository
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	url := pgtest.NewURL(t)
	if err := postgres.MigrateUp(url); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return &harness{
		t:         t,
		ctx:       context.Background(),
		runRepo:   run.NewPostgresRepository(pool),
		auditRepo: audit.NewPostgresRepository(pool),
		concRepo:  concern.NewPostgresRepository(pool),
		artRepo:   artifact.NewPostgresRepository(pool),
	}
}

func (h *harness) assembler(prs ...releaseevidence.MergedPR) *releaseevidence.Assembler {
	return &releaseevidence.Assembler{
		Runs:      h.runRepo,
		Audit:     h.auditRepo,
		Concerns:  h.concRepo,
		Artifacts: h.artRepo,
		PRs:       fakeResolver{prs: prs},
	}
}

func (h *harness) createRun(prURL string, parent *uuid.UUID) *run.Run {
	h.t.Helper()
	r, err := h.runRepo.CreateRun(h.ctx, run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
		ParentRunID:   parent,
	})
	if err != nil {
		h.t.Fatalf("create run: %v", err)
	}
	if prURL != "" {
		if _, err := h.runRepo.SetRunPullRequestURL(h.ctx, r.ID, prURL); err != nil {
			h.t.Fatalf("set pr url: %v", err)
		}
	}
	return r
}

func (h *harness) transitionRun(id uuid.UUID, to run.State) {
	h.t.Helper()
	if _, err := h.runRepo.TransitionRun(h.ctx, id, run.StateRunning); err != nil {
		h.t.Fatalf("transition to running: %v", err)
	}
	if _, err := h.runRepo.TransitionRun(h.ctx, id, to); err != nil {
		h.t.Fatalf("transition to %s: %v", to, err)
	}
}

func (h *harness) addStage(runID uuid.UUID, seq int, typ run.StageType) *run.Stage {
	h.t.Helper()
	st, err := h.runRepo.CreateStage(h.ctx, run.CreateStageParams{
		RunID:        runID,
		Sequence:     seq,
		Type:         typ,
		ExecutorKind: run.ExecutorAgent,
		ExecutorRef:  "fishhawk/runner@v1",
	})
	if err != nil {
		h.t.Fatalf("create %s stage: %v", typ, err)
	}
	return st
}

func hashContent(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// addPlanStage seeds a plan stage carrying a standard_v1 plan artifact
// with the given summary.
func (h *harness) addPlanStage(runID uuid.UUID, seq int, summary string) {
	h.t.Helper()
	st := h.addStage(runID, seq, run.StageTypePlan)
	content, _ := json.Marshal(map[string]any{"kind": "plan", "summary": summary})
	sv := "standard_v1"
	if _, err := h.artRepo.Create(h.ctx, artifact.CreateParams{
		StageID:       st.ID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       content,
		ContentHash:   hashContent(content),
	}); err != nil {
		h.t.Fatalf("create plan artifact: %v", err)
	}
}

func (h *harness) append(runID uuid.UUID, stageID uuid.UUID, category string, payload map[string]any) {
	h.t.Helper()
	raw, _ := json.Marshal(payload)
	ak := audit.ActorSystem
	if _, err := h.auditRepo.AppendChained(h.ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  category,
		ActorKind: &ak,
		Payload:   raw,
	}); err != nil {
		h.t.Fatalf("append %s: %v", category, err)
	}
}

func (h *harness) addReviewerVerdict(runID, stageID uuid.UUID, model, verdict string) {
	h.append(runID, stageID, "implement_reviewed", map[string]any{
		"reviewer_model": model,
		"verdict":        verdict,
	})
}

func (h *harness) addAcceptance(runID, stageID uuid.UUID, verdict, failureMode string) {
	h.append(runID, stageID, "acceptance_outcome_recorded", map[string]any{
		"verdict":      verdict,
		"failure_mode": failureMode,
	})
}

func (h *harness) addCost(runID, stageID uuid.UUID, usd float64, source string) {
	h.append(runID, stageID, "cost_recorded", map[string]any{
		"usd":    usd,
		"source": source,
	})
}

// addDeferredConcern inserts one concern and transitions it to the
// deferred terminal state.
func (h *harness) addDeferredConcern(runID, stageID uuid.UUID, seq int64, note string) {
	h.t.Helper()
	rows, err := h.concRepo.InsertRaised(h.ctx, concern.InsertRaisedParams{
		RunID:                runID,
		StageID:              stageID,
		StageKind:            concern.StageKindImplement,
		ReviewerModel:        "claude-opus-4-8",
		OriginReviewSequence: seq,
		Concerns:             []concern.RaisedConcern{{Severity: "low", Category: "style", Note: note}},
	})
	if err != nil {
		h.t.Fatalf("insert concern: %v", err)
	}
	if _, err := h.concRepo.ApplyResolution(h.ctx, rows[0].ID, concern.StateDeferred, "filed follow-up"); err != nil {
		h.t.Fatalf("defer concern: %v", err)
	}
}

// TestAssemble_LoopMergedChange (case a): a single loop-merged change
// surfaces the plan summary + link, both reviewer verdicts, and the
// acceptance outcome.
func TestAssemble_LoopMergedChange(t *testing.T) {
	h := newHarness(t)
	prURL := "https://github.com/kuhlman-labs/fishhawk/pull/100"
	r := h.createRun(prURL, nil)
	h.addPlanStage(r.ID, 1, "assemble merged-run evidence")
	impl := h.addStage(r.ID, 2, run.StageTypeImplement)
	h.addReviewerVerdict(r.ID, impl.ID, "claude-opus-4-8", "approve")
	h.addReviewerVerdict(r.ID, impl.ID, "gpt-5.5", "approve_with_concerns")
	acc := h.addStage(r.ID, 3, run.StageTypeAcceptance)
	h.addAcceptance(r.ID, acc.ID, "passed", "")
	h.addDeferredConcern(r.ID, impl.ID, 5, "rename later")
	h.addCost(r.ID, impl.ID, 1.25, "")
	h.transitionRun(r.ID, run.StateSucceeded)

	a := h.assembler(releaseevidence.MergedPR{URL: prURL, Number: 100, Title: "evidence assembly"})
	ev, err := a.Assemble(h.ctx, "kuhlman-labs/fishhawk", "v0.1.0", "HEAD")
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(ev.Changes) != 1 {
		t.Fatalf("len(Changes) = %d, want 1", len(ev.Changes))
	}
	ce := ev.Changes[0]
	if !ce.LoopMerged || ce.ReducedEvidence {
		t.Errorf("LoopMerged=%v ReducedEvidence=%v, want true/false", ce.LoopMerged, ce.ReducedEvidence)
	}
	if ce.PlanSummary != "assemble merged-run evidence" {
		t.Errorf("PlanSummary = %q", ce.PlanSummary)
	}
	if ce.PlanLink != prURL {
		t.Errorf("PlanLink = %q, want %q", ce.PlanLink, prURL)
	}
	if len(ce.ReviewerVerdicts) != 2 {
		t.Fatalf("ReviewerVerdicts = %+v, want 2", ce.ReviewerVerdicts)
	}
	gotVerdicts := map[string]string{}
	for _, v := range ce.ReviewerVerdicts {
		gotVerdicts[v.ReviewerModel] = v.Verdict
	}
	if gotVerdicts["claude-opus-4-8"] != "approve" || gotVerdicts["gpt-5.5"] != "approve_with_concerns" {
		t.Errorf("verdicts = %+v", gotVerdicts)
	}
	if ce.AcceptanceOutcome == nil || ce.AcceptanceOutcome.Verdict != "passed" {
		t.Errorf("AcceptanceOutcome = %+v, want verdict=passed", ce.AcceptanceOutcome)
	}
	if len(ce.DeferredConcerns) != 1 || ce.DeferredConcerns[0].Note != "rename later" {
		t.Errorf("DeferredConcerns = %+v", ce.DeferredConcerns)
	}
}

// TestAssemble_HumanLedReducedEvidence (case b): a PR in range with no
// matching run yields ReducedEvidence=true, a non-empty reason, and no
// fabricated verdicts / acceptance.
func TestAssemble_HumanLedReducedEvidence(t *testing.T) {
	h := newHarness(t)
	// No run seeded for this PR URL.
	humanURL := "https://github.com/kuhlman-labs/fishhawk/pull/200"
	a := h.assembler(releaseevidence.MergedPR{URL: humanURL, Number: 200, Title: "hotfix by hand"})
	ev, err := a.Assemble(h.ctx, "kuhlman-labs/fishhawk", "v0.1.0", "HEAD")
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(ev.Changes) != 1 {
		t.Fatalf("len(Changes) = %d, want 1", len(ev.Changes))
	}
	ce := ev.Changes[0]
	if !ce.ReducedEvidence || ce.LoopMerged {
		t.Errorf("ReducedEvidence=%v LoopMerged=%v, want true/false", ce.ReducedEvidence, ce.LoopMerged)
	}
	if ce.ReducedReason == "" {
		t.Error("ReducedReason is empty, want a non-empty reason")
	}
	if ce.ReviewerVerdicts != nil || ce.AcceptanceOutcome != nil {
		t.Errorf("fabricated evidence: verdicts=%+v acceptance=%+v", ce.ReviewerVerdicts, ce.AcceptanceOutcome)
	}
	if ce.PlanSummary != "" || ce.PlanLink != "" {
		t.Errorf("fabricated plan: summary=%q link=%q", ce.PlanSummary, ce.PlanLink)
	}
	if ce.CostUSD != 0 || ce.RunCount != 0 {
		t.Errorf("CostUSD=%v RunCount=%d, want 0/0", ce.CostUSD, ce.RunCount)
	}
}

// TestAssemble_CostParity (case c): the per-release total equals both the
// sum of per-PR ChangeEvidence.CostUSD and the independent
// cost.AggregateRunCost rollup over the seeded ledger entries.
func TestAssemble_CostParity(t *testing.T) {
	h := newHarness(t)
	pr1 := "https://github.com/kuhlman-labs/fishhawk/pull/301"
	pr2 := "https://github.com/kuhlman-labs/fishhawk/pull/302"

	r1 := h.createRun(pr1, nil)
	impl1 := h.addStage(r1.ID, 1, run.StageTypeImplement)
	h.addCost(r1.ID, impl1.ID, 1.00, "")
	h.addCost(r1.ID, impl1.ID, 0.50, "implement_review")
	h.transitionRun(r1.ID, run.StateSucceeded)

	r2 := h.createRun(pr2, nil)
	impl2 := h.addStage(r2.ID, 1, run.StageTypeImplement)
	h.addCost(r2.ID, impl2.ID, 2.25, "")
	h.transitionRun(r2.ID, run.StateSucceeded)

	// Independent rollup: what cost.AggregateRunCost yields directly.
	independent := cost.AggregateRunCost([]cost.RunCostEntry{
		{Source: "", USD: 1.00},
		{Source: "implement_review", USD: 0.50},
		{Source: "", USD: 2.25},
	}).TotalUSD

	a := h.assembler(
		releaseevidence.MergedPR{URL: pr1, Number: 301, Title: "one"},
		releaseevidence.MergedPR{URL: pr2, Number: 302, Title: "two"},
	)
	ev, err := a.Assemble(h.ctx, "kuhlman-labs/fishhawk", "v0.1.0", "HEAD")
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	var sumPerPR float64
	for _, ce := range ev.Changes {
		sumPerPR += ce.CostUSD
	}
	if !floatEq(ev.TotalCostUSD, sumPerPR) {
		t.Errorf("TotalCostUSD %v != sum per-PR %v", ev.TotalCostUSD, sumPerPR)
	}
	if !floatEq(ev.TotalCostUSD, independent) {
		t.Errorf("TotalCostUSD %v != independent AggregateRunCost %v", ev.TotalCostUSD, independent)
	}
	if !floatEq(independent, 3.75) {
		t.Fatalf("independent rollup %v, want 3.75 (fixture sanity)", independent)
	}
}

// TestAssemble_MultiRunRecoveryChild (binding condition 1): a PR with a
// FAILED parent run plus a SUCCEEDED plan-stage-less recovery child
// selects the child for verdicts/acceptance, resolves the plan via the
// parent_run_id walk, and sums cost across BOTH runs.
func TestAssemble_MultiRunRecoveryChild(t *testing.T) {
	h := newHarness(t)
	prURL := "https://github.com/kuhlman-labs/fishhawk/pull/400"

	// Failed parent: carries the approved plan and its own cost.
	parent := h.createRun(prURL, nil)
	h.addPlanStage(parent.ID, 1, "the parent plan summary")
	pimpl := h.addStage(parent.ID, 2, run.StageTypeImplement)
	h.addReviewerVerdict(parent.ID, pimpl.ID, "claude-opus-4-8", "reject")
	h.addCost(parent.ID, pimpl.ID, 3.00, "")
	h.transitionRun(parent.ID, run.StateFailed)

	// Succeeded recovery child: NO plan stage of its own; carries the
	// authoritative verdicts + acceptance and its own cost.
	pid := parent.ID
	child := h.createRun(prURL, &pid)
	cimpl := h.addStage(child.ID, 1, run.StageTypeImplement)
	h.addReviewerVerdict(child.ID, cimpl.ID, "claude-opus-4-8", "approve")
	h.addReviewerVerdict(child.ID, cimpl.ID, "gpt-5.5", "approve")
	cacc := h.addStage(child.ID, 2, run.StageTypeAcceptance)
	h.addAcceptance(child.ID, cacc.ID, "passed", "")
	h.addCost(child.ID, cimpl.ID, 1.50, "")
	h.transitionRun(child.ID, run.StateSucceeded)

	a := h.assembler(releaseevidence.MergedPR{URL: prURL, Number: 400, Title: "recovered change"})
	ev, err := a.Assemble(h.ctx, "kuhlman-labs/fishhawk", "v0.1.0", "HEAD")
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(ev.Changes) != 1 {
		t.Fatalf("len(Changes) = %d, want 1", len(ev.Changes))
	}
	ce := ev.Changes[0]
	if ce.RunCount != 2 {
		t.Errorf("RunCount = %d, want 2 (parent + child)", ce.RunCount)
	}
	// The child (succeeded) is selected: its verdicts win, not the parent's
	// reject.
	if len(ce.ReviewerVerdicts) != 2 {
		t.Fatalf("ReviewerVerdicts = %+v, want 2 from the child", ce.ReviewerVerdicts)
	}
	for _, v := range ce.ReviewerVerdicts {
		if v.Verdict != "approve" {
			t.Errorf("verdict = %q, want approve (child selected, not the failed parent)", v.Verdict)
		}
	}
	if ce.AcceptanceOutcome == nil || ce.AcceptanceOutcome.Verdict != "passed" {
		t.Errorf("AcceptanceOutcome = %+v, want passed (from the child)", ce.AcceptanceOutcome)
	}
	// Plan evidence resolves via the parent_run_id walk (child has no plan
	// stage).
	if ce.PlanSummary != "the parent plan summary" {
		t.Errorf("PlanSummary = %q, want the parent plan via the walk", ce.PlanSummary)
	}
	if ce.PlanLink != prURL {
		t.Errorf("PlanLink = %q, want %q", ce.PlanLink, prURL)
	}
	// Cost sums ALL runs (parent 3.00 + child 1.50).
	if !floatEq(ce.CostUSD, 4.50) {
		t.Errorf("CostUSD = %v, want 4.50 (sums the failed parent + child)", ce.CostUSD)
	}
	if !floatEq(ev.TotalCostUSD, 4.50) {
		t.Errorf("TotalCostUSD = %v, want 4.50", ev.TotalCostUSD)
	}
}

// TestAssemble_MultiAcceptanceLastWins seeds MULTIPLE
// acceptance_outcome_recorded entries for one run (a failed acceptance
// followed by a passing re-run) and asserts the LAST-appended outcome
// wins — exercising acceptanceOutcome()'s "entries are
// sequence-ascending, so the last decoded wins" assumption. A single
// entry would not distinguish last-wins from first-wins.
func TestAssemble_MultiAcceptanceLastWins(t *testing.T) {
	h := newHarness(t)
	prURL := "https://github.com/kuhlman-labs/fishhawk/pull/500"
	r := h.createRun(prURL, nil)
	impl := h.addStage(r.ID, 1, run.StageTypeImplement)
	h.addReviewerVerdict(r.ID, impl.ID, "claude-opus-4-8", "approve")
	acc := h.addStage(r.ID, 2, run.StageTypeAcceptance)
	// Two acceptance outcomes on the same run, appended in order: the
	// earlier failure, then the passing re-run. Last-appended must win.
	h.addAcceptance(r.ID, acc.ID, "failed", "criteria_unmet")
	h.addAcceptance(r.ID, acc.ID, "passed", "")
	h.transitionRun(r.ID, run.StateSucceeded)

	a := h.assembler(releaseevidence.MergedPR{URL: prURL, Number: 500, Title: "retried acceptance"})
	ev, err := a.Assemble(h.ctx, "kuhlman-labs/fishhawk", "v0.1.0", "HEAD")
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(ev.Changes) != 1 {
		t.Fatalf("len(Changes) = %d, want 1", len(ev.Changes))
	}
	ce := ev.Changes[0]
	if ce.AcceptanceOutcome == nil {
		t.Fatalf("AcceptanceOutcome = nil, want the last-appended outcome")
	}
	if ce.AcceptanceOutcome.Verdict != "passed" || ce.AcceptanceOutcome.FailureMode != "" {
		t.Errorf("AcceptanceOutcome = %+v, want the LAST entry (passed, no failure_mode)", ce.AcceptanceOutcome)
	}
}

// TestAssemble_ResolverError propagates a resolver failure (defensive
// branch: a GitHub walk failure is an error, not a silent empty release).
func TestAssemble_ResolverError(t *testing.T) {
	h := newHarness(t)
	a := &releaseevidence.Assembler{
		Runs:      h.runRepo,
		Audit:     h.auditRepo,
		Concerns:  h.concRepo,
		Artifacts: h.artRepo,
		PRs:       fakeResolver{err: context.DeadlineExceeded},
	}
	if _, err := a.Assemble(h.ctx, "kuhlman-labs/fishhawk", "a", "b"); err == nil {
		t.Fatal("Assemble returned nil error, want the resolver error propagated")
	}
}

// TestGitHubResolver_DedupByPRNumber (binding condition 2): two SHAs in
// the compare range both map to the same PR number, so the resolver
// yields ONE MergedPR.
func TestGitHubResolver_DedupByPRNumber(t *testing.T) {
	var mu sync.Mutex
	queried := map[string]int{}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/compare/{basehead...}",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"commits":[{"sha":"s1"},{"sha":"s2"}]}`)
		})
	// Both landing commits of the squash-merged PR report PR #42, so the
	// resolver must fan in across BOTH SHAs and de-dup by number. Recording
	// each queried SHA proves the fan-in: an implementation that queried
	// only the first commit would leave s2 unqueried and fail below, even
	// though it too would return a single PR.
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/pulls",
		func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			queried[r.PathValue("sha")]++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[{"number":42,"html_url":"https://github.com/x/y/pull/42","title":"squashed","merged_at":"2026-07-01T00:00:00Z"}]`)
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  stubTokenProvider{},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_app_jwt", nil },
	}
	resolver := &releaseevidence.GitHubResolver{Client: client, InstallationID: 7}
	prs, err := resolver.MergedPRsInRange(context.Background(), "x/y", "v0", "HEAD")
	if err != nil {
		t.Fatalf("MergedPRsInRange: %v", err)
	}
	// Fan-in: BOTH landing commits were queried, not just the first.
	if queried["s1"] == 0 || queried["s2"] == 0 {
		t.Errorf("expected both SHAs queried (fan-in), got %v", queried)
	}
	// De-dup: the shared PR #42 across both SHAs yields a single MergedPR.
	if len(prs) != 1 {
		t.Fatalf("len(prs) = %d, want 1 (de-duped by PR number) — got %+v", len(prs), prs)
	}
	if prs[0].Number != 42 {
		t.Errorf("pr number = %d, want 42", prs[0].Number)
	}
}

// TestGitHubResolver_BadRepo covers the parseRepoRef defensive branch: a
// repo string that is not owner/name errors before any GitHub call (the
// Client is never touched).
func TestGitHubResolver_BadRepo(t *testing.T) {
	resolver := &releaseevidence.GitHubResolver{Client: nil, InstallationID: 7}
	for _, bad := range []string{"noslash", "", "a/b/c", "/name", "owner/"} {
		if _, err := resolver.MergedPRsInRange(context.Background(), bad, "v0", "HEAD"); err == nil {
			t.Errorf("MergedPRsInRange(%q) returned nil error, want a parse error", bad)
		}
	}
}

type stubTokenProvider struct{}

func (stubTokenProvider) Token(_ context.Context, _ int64) (string, error) {
	return "ghs_test_token", nil
}

// floatEq compares two dollar figures with a tolerance that absorbs
// float64 summation noise.
func floatEq(a, b float64) bool {
	const eps = 1e-9
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}
