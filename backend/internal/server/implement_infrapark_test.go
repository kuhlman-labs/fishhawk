package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// #4426 slice 2: the server-side producers of a category-C implement failure
// against run.ClassifyInfraFailurePark's implement extension. A first-execution
// failure parks the run (stage failed/C, run running, retry-eligible); a
// failure inside a re-opened pass (fix-up, conflict-resolution or
// merge-candidate-verify, or a stage that already produced a PR) keeps today's
// fail-the-run path (R4, approval condition C1).

// implementParkRun is the feature_change shape on the shared fixupRecoveryRepo
// fake: plan succeeded, implement running, acceptance pending, review pending.
func implementParkRun(rr *fixupRecoveryRepo) (*run.Run, *run.Stage, *run.Stage) {
	runRow := rr.seedRun()
	plan := rr.seedStage(runRow.ID, 1, run.StageStateSucceeded)
	impl := rr.seedStage(runRow.ID, 2, run.StageStateRunning)
	acc := rr.seedStage(runRow.ID, 3, run.StageStatePending)
	review := rr.seedStage(runRow.ID, 4, run.StageStatePending)
	rr.mu.Lock()
	plan.Type = run.StageTypePlan
	impl.Type = run.StageTypeImplement
	acc.Type = run.StageTypeAcceptance
	review.Type = run.StageTypeReview
	rr.mu.Unlock()
	return runRow, impl, acc
}

// newImplementParkPRServer wires the signed /pull-request handler with a REAL
// orchestrator whose Audit is the sequenced fake, so failPullRequestStage's
// Advance reads the run's park history.
func newImplementParkPRServer(t *testing.T) (*Server, *fixupRecoveryRepo, *sequencedAuditFake, *signingFake) {
	t.Helper()
	rr := &fixupRecoveryRepo{orchestratorRepo: newOrchestratorRepo()}
	au := newSequencedAuditFake()
	sf := newSigningFake()
	s := New(Config{
		Addr: "127.0.0.1:0", SigningRepo: sf, ArtifactRepo: newFakeArtifactRepo(),
		AuditRepo: au, RunRepo: rr, Orchestrator: &orchestrator.Orchestrator{Runs: rr, Audit: au},
	})
	return s, rr, au, sf
}

func implParkRequireRunState(t *testing.T, rr *fixupRecoveryRepo, runID uuid.UUID, want run.State) {
	t.Helper()
	got, err := rr.GetRun(t.Context(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.State != want {
		t.Fatalf("run state = %s, want %s", got.State, want)
	}
}

func implParkRequireStageFailedC(t *testing.T, repo run.Repository, stageID uuid.UUID) {
	t.Helper()
	got, err := repo.GetStage(t.Context(), stageID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != run.StageStateFailed || got.FailureCategory == nil || *got.FailureCategory != run.FailureC {
		t.Fatalf("implement stage = %s/%v, want failed/C", got.State, got.FailureCategory)
	}
}

// implParkRows returns the stage_infra_parked rows scoped to stageID.
func implParkRows(au *auditFake, stageID uuid.UUID) []audit.ChainAppendParams {
	var out []audit.ChainAppendParams
	for _, e := range auditEntries(au, run.CategoryStageInfraParked) {
		if e.StageID != nil && *e.StageID == stageID {
			out = append(out, e)
		}
	}
	return out
}

// TestImplementInfraFailure_PullRequestFailureParksAndRetries is T-PR-1: a
// signed first-execution PR-open failure report crosses failPullRequestStage ->
// Orchestrator.Advance (Audit wired) -> stage and run state -> audit. No pass
// trigger and no prior PR row exist, so the failure parks: the run stays
// running, the implement stage stays failed/C with exactly one stage-scoped
// park row, and the stage is retry-eligible.
func TestImplementInfraFailure_PullRequestFailureParksAndRetries(t *testing.T) {
	s, rr, au, sf := newImplementParkPRServer(t)
	runRow, impl, acc := implementParkRun(rr)

	priv, _ := sf.issue(t, runRow.ID)
	body := []byte(`{"outcome":"failed","category":"C","reason":"open PR: 503 from the forge"}`)
	if w := shipPRRequest(t, s, runRow.ID, impl.ID, priv, body, ""); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	implParkRequireRunState(t, rr, runRow.ID, run.StateRunning)
	implParkRequireStageFailedC(t, rr, impl.ID)
	rows := implParkRows(au.auditFake, impl.ID)
	if len(rows) != 1 {
		t.Fatalf("stage_infra_parked rows scoped to implement = %d, want 1", len(rows))
	}
	var p struct {
		StageType string `json:"stage_type"`
	}
	if err := json.Unmarshal(rows[0].Payload, &p); err != nil || p.StageType != string(run.StageTypeImplement) {
		t.Errorf("park payload stage_type = %q (err %v), want implement", p.StageType, err)
	}
	if got, _ := rr.GetStage(t.Context(), acc.ID); got.State != run.StageStatePending {
		t.Errorf("acceptance stage = %s, want pending (nothing downstream dispatched)", got.State)
	}

	if _, err := run.RetryStage(t.Context(), rr, impl.ID, run.RetryOptions{}); err != nil {
		t.Fatalf("RetryStage on the parked implement stage: %v", err)
	}
	if got, _ := rr.GetStage(t.Context(), impl.ID); got.State != run.StageStatePending {
		t.Fatalf("implement stage after retry = %s, want pending", got.State)
	}
	implParkRequireRunState(t, rr, runRow.ID, run.StateRunning)
}

// TestImplementInfraFailure_PassRecoveryFallbackStillFailsRun is T-PR-2 (R4,
// approval condition C1): a category-C PR-open failure inside a re-opened pass
// whose recovery could NOT restore falls through to Advance, and the
// re-opened-pass carve-out keeps today's fail-the-run path. Rows (a) and (b)
// seed a pass trigger whose re-parked review stage id names NO stage, so
// RestoreFixupStage's review re-park fails before the implement stage is
// un-failed. Row (c) seeds NO trigger row, only a prior pull_request_opened
// scoped to the implement stage — the durable signal that survives a lost
// stage_fixup_triggered append (#4815).
func TestImplementInfraFailure_PassRecoveryFallbackStillFailsRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		// seed appends the re-opened-pass evidence and returns the category
		// that must read back scoped to the implement stage.
		seed func(t *testing.T, au *sequencedAuditFake, runID, implID uuid.UUID) string
	}{
		{"fixup trigger, unrestorable review anchor", func(t *testing.T, au *sequencedAuditFake, runID, implID uuid.UUID) string {
			missing := uuid.New()
			seedFixupTriggered(t, au.storingAuditFake, runID, implID, run.StageStateSucceeded, &missing)
			return CategoryStageFixupTriggered
		}},
		{"conflict-resolution trigger, unrestorable review anchor", func(t *testing.T, au *sequencedAuditFake, runID, implID uuid.UUID) string {
			missing := uuid.New()
			seedConflictResolutionTriggered(t, au, runID, implID, "feature", "main", "head1", run.StageStateSucceeded, &missing)
			return CategoryStageConflictResolutionTriggered
		}},
		{"no trigger row, prior pull_request_opened (C1 durable signal)", func(t *testing.T, au *sequencedAuditFake, runID, implID uuid.UUID) string {
			payload, _ := json.Marshal(map[string]any{"stage_id": implID.String(), "pr_number": 42})
			kind := audit.ActorKind("runner")
			if _, err := au.AppendChained(context.Background(), audit.ChainAppendParams{
				RunID: runID, StageID: &implID, Category: "pull_request_opened", ActorKind: &kind, Payload: payload,
			}); err != nil {
				t.Fatalf("seed pull_request_opened: %v", err)
			}
			return "pull_request_opened"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, rr, au, sf := newImplementParkPRServer(t)
			runRow, impl, _ := implementParkRun(rr)
			category := tc.seed(t, au, runRow.ID, impl.ID)

			// Precondition: the run's history carries the re-opened-pass row
			// scoped to the implement stage, so the carve-out (not an empty
			// history) is what this exercises.
			entries, err := au.ListForRun(t.Context(), runRow.ID)
			if err != nil {
				t.Fatal(err)
			}
			scoped := false
			for _, e := range entries {
				if e.Category == category && e.StageID != nil && *e.StageID == impl.ID {
					scoped = true
				}
			}
			if !scoped {
				t.Fatalf("precondition: no %s row scoped to the implement stage in ListForRun", category)
			}
			if category == "pull_request_opened" {
				for _, c := range run.InfraParkReopenedPassCategories() {
					if n := len(auditEntries(au.auditFake, c)); n != 0 {
						t.Fatalf("precondition: %d %s rows, want 0 (the durable row must stand alone)", n, c)
					}
				}
			}

			priv, _ := sf.issue(t, runRow.ID)
			body := []byte(`{"outcome":"failed","category":"C","reason":"push failed"}`)
			if w := shipPRRequest(t, s, runRow.ID, impl.ID, priv, body, ""); w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
			}

			implParkRequireStageFailedC(t, rr, impl.ID)
			implParkRequireRunState(t, rr, runRow.ID, run.StateFailed)
			if n := len(implParkRows(au.auditFake, impl.ID)); n != 0 {
				t.Fatalf("stage_infra_parked rows = %d, want 0 (a re-opened pass never parks)", n)
			}
		})
	}
}

// TestImplementInfraFailure_MergeCandidateRefusalRestoreFailureStillFailsRun is
// T-MCV-1 (R4): refuseMergeCandidatePassToIncapableRunner's restore-failed arm
// falls through to Advance. The stock stale-runner fixture wires no Audit, so
// the park is unreachable there; this wires a real Audit so the carve-out is
// the only thing keeping the run on the fail path.
func TestImplementInfraFailure_MergeCandidateRefusalRestoreFailureStillFailsRun(t *testing.T) {
	f, _ := newMCVStaleRunnerFixture(t)
	f.s.cfg.Orchestrator = &orchestrator.Orchestrator{Runs: f.repo, Audit: f.au}
	if n := len(auditEntries(f.au, CategoryStageMergeCandidateVerifyTriggered)); n != 1 {
		t.Fatalf("precondition: merge-candidate triggers = %d, want 1", n)
	}

	f.repo.refuseTo = run.StageStateAwaitingApproval
	refusal := mcvRefusal(t, promptRequest(t, f.s, f.runID, f.impl.ID, f.priv, ""))
	f.repo.refuseTo = ""

	if refusal.Details["settled"] != false {
		t.Errorf("settled = %v, want false", refusal.Details["settled"])
	}
	if got := f.stageState(t, f.impl.ID); got != run.StageStateFailed {
		t.Errorf("implement state = %q, want failed (the restore was refused)", got)
	}
	if got := f.runRow(t).State; got != run.StateFailed {
		t.Errorf("run state = %q, want failed (a re-opened pass never parks)", got)
	}
	if n := len(implParkRows(f.au, f.impl.ID)); n != 0 {
		t.Errorf("stage_infra_parked rows = %d, want 0", n)
	}
}

// TestImplementInfraFailure_LineHolderReopenedPassStillDropsRunTerminal is
// T-MCQ-2 (pgtest): the merge-candidate line holder's implement stage is
// re-opened for its verify pass; an operator reap category C on that pass
// stage reaches Advance with the trigger scoped to the stage, so the run fails
// (no park) and the pump drops A run_terminal and admits B.
func TestImplementInfraFailure_LineHolderReopenedPassStillDropsRunTerminal(t *testing.T) {
	f := newMCQFixture(t)
	a, b := f.wave()
	if tr := f.triggers(a); len(tr) == 0 {
		t.Fatal("precondition: A's implement carries no merge-candidate trigger")
	}
	f.setStage(a.impl.ID, run.StageStateDispatched)
	f.srv.cfg.Orchestrator = &orchestrator.Orchestrator{Runs: f.repo, Audit: f.audit}

	rec := postReapFailure(t, f.srv, a.id, a.impl.ID, reapFailureRequest{Category: "C", Reason: "runner_lost"}, withReapOperator)
	if rec.Code != http.StatusOK {
		t.Fatalf("reap-failure = %d: %s", rec.Code, rec.Body.String())
	}

	if got := f.runState(a); got != run.StateFailed {
		t.Fatalf("run A state = %s, want failed (a re-opened pass never parks)", got)
	}
	if n := len(f.rows(a, run.CategoryStageInfraParked)); n != 0 {
		t.Fatalf("stage_infra_parked rows = %d, want 0", n)
	}

	f.srv.PumpMergeCandidateQueues(context.Background())

	f.requireState(a, mergequeue.StateDropped, mergeCandidateQueueReasonRunTerminal)
	f.requireState(b, mergequeue.StateLive, "")
}

// implParkCampaignWorld is T-CAMP-1's real-store world: one running campaign whose one
// item is linked to a running feature_change run.
type implParkCampaignWorld struct {
	s         *Server
	runs      run.Repository
	auditRepo audit.Repository
	campaign  *campaign.Campaign
	runID     uuid.UUID
	impl      *run.Stage
}

func newImplParkCampaignWorld(t *testing.T) *implParkCampaignWorld {
	t.Helper()
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	campaigns := campaign.NewPostgresRepository(pool)
	runs := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	// The status read runs under testOperatorIdentity, whose account id stamps
	// the global campaign_issue_settled append; seed that account so the append
	// lands and a zero row count is not vacuous.
	if _, err := pool.Exec(ctx, `INSERT INTO accounts (id, account_key) VALUES ($1, $2)`,
		testOperatorAccountID, "implement-infrapark-test"); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	c, err := campaigns.CreateCampaign(ctx, campaign.CreateCampaignParams{
		Repo: "kuhlman-labs/fishhawk", EpicRef: "issue:4426", PausePolicy: campaign.PausePolicyPauseCampaign,
	})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	item, err := campaigns.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{CampaignID: c.ID, IssueRef: "issue:1"})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}
	if _, err := campaigns.TransitionCampaign(ctx, c.ID, campaign.StateRunning); err != nil {
		t.Fatalf("campaign -> running: %v", err)
	}
	ref := "issue:1"
	r, err := runs.CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "deadbeef",
		TriggerSource: run.TriggerGitHubIssue, TriggerRef: &ref,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := campaigns.SetCampaignItemRun(ctx, item.ID, &r.ID); err != nil {
		t.Fatalf("link item run: %v", err)
	}
	if _, err := campaigns.TransitionCampaignItem(ctx, item.ID, campaign.ItemStateRunning); err != nil {
		t.Fatalf("item -> running: %v", err)
	}
	if _, err := runs.TransitionRun(ctx, r.ID, run.StateRunning); err != nil {
		t.Fatalf("run -> running: %v", err)
	}
	var impl *run.Stage
	for _, st := range []struct {
		seq   int
		typ   run.StageType
		state run.StageState
	}{
		{1, run.StageTypePlan, run.StageStateSucceeded},
		{2, run.StageTypeImplement, run.StageStateDispatched},
		{3, run.StageTypeAcceptance, run.StageStatePending},
		{4, run.StageTypeReview, run.StageStatePending},
	} {
		s, err := runs.CreateStage(ctx, run.CreateStageParams{
			RunID: r.ID, Sequence: st.seq, Type: st.typ, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
		})
		if err != nil {
			t.Fatalf("create stage: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE stages SET state = $1 WHERE id = $2`, string(st.state), s.ID); err != nil {
			t.Fatalf("set stage state: %v", err)
		}
		if st.typ == run.StageTypeImplement {
			impl = s
		}
	}
	s := New(Config{
		Addr: "127.0.0.1:0", CampaignRepo: campaigns, RunRepo: runs, AuditRepo: auditRepo,
		Orchestrator: &orchestrator.Orchestrator{Runs: runs, Audit: auditRepo},
	})
	return &implParkCampaignWorld{s: s, runs: runs, auditRepo: auditRepo, campaign: c, runID: r.ID, impl: impl}
}

func (w *implParkCampaignWorld) reapC(t *testing.T) {
	t.Helper()
	rec := postReapFailure(t, w.s, w.runID, w.impl.ID, reapFailureRequest{Category: "C", Reason: "runner_lost"}, withReapOperator)
	if rec.Code != http.StatusOK {
		t.Fatalf("reap-failure = %d: %s", rec.Code, rec.Body.String())
	}
}

func (w *implParkCampaignWorld) runState(t *testing.T) run.State {
	t.Helper()
	r, err := w.runs.GetRun(context.Background(), w.runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	return r.State
}

func (w *implParkCampaignWorld) settledRows(t *testing.T) int {
	t.Helper()
	cat := categoryCampaignIssueSettled
	es, err := w.auditRepo.ListAll(context.Background(), audit.ListAllParams{Category: &cat})
	if err != nil {
		t.Fatalf("list campaign_issue_settled: %v", err)
	}
	return len(es)
}

// TestImplementInfraFailure_CampaignItemStaysRunning is T-CAMP-1 (R3; pgtest
// with real run, audit and campaign repositories and a real orchestrator): reap
// handler -> Advance -> Postgres -> reconcile-on-read -> campaign status. A
// parked implement keeps the run non-terminal, so reconcile-on-read never
// settles the item. The budget twin (three earlier-episode parks) fails the run
// and the same read settles the item failed.
func TestImplementInfraFailure_CampaignItemStaysRunning(t *testing.T) {
	t.Run("parked", func(t *testing.T) {
		w := newImplParkCampaignWorld(t)
		w.reapC(t)
		if got := w.runState(t); got != run.StateRunning {
			t.Fatalf("run state = %s, want running (parked)", got)
		}
		st := getCampaignStatusBody(t, w.s, w.campaign.ID)
		if len(st.Items) != 1 || st.Items[0].State != string(campaign.ItemStateRunning) {
			t.Fatalf("items = %+v, want one running item", st.Items)
		}
		if !reflect.DeepEqual(st.Rollup.Running, []string{"issue:1"}) {
			t.Errorf("rollup.running = %v, want [issue:1]", st.Rollup.Running)
		}
		if st.Campaign.State != string(campaign.StateRunning) {
			t.Errorf("campaign state = %s, want running", st.Campaign.State)
		}
		if n := w.settledRows(t); n != 0 {
			t.Errorf("campaign_issue_settled rows = %d, want 0", n)
		}
	})
	t.Run("budget exhausted", func(t *testing.T) {
		w := newImplParkCampaignWorld(t)
		sys := audit.ActorSystem
		for i := 0; i < run.InfraFailureParkRetryBudget; i++ {
			payload, _ := json.Marshal(map[string]any{"failed_at": fmt.Sprintf("2026-01-0%dT00:00:00Z", i+1), "retry_ordinal": i})
			stageID := w.impl.ID
			if _, err := w.auditRepo.AppendChained(context.Background(), audit.ChainAppendParams{
				RunID: w.runID, StageID: &stageID, Timestamp: time.Now().UTC(),
				Category: run.CategoryStageInfraParked, ActorKind: &sys, Payload: payload,
			}); err != nil {
				t.Fatal(err)
			}
		}
		w.reapC(t)
		if got := w.runState(t); got != run.StateFailed {
			t.Fatalf("run state = %s, want failed (budget exhausted)", got)
		}
		st := getCampaignStatusBody(t, w.s, w.campaign.ID)
		if len(st.Items) != 1 || st.Items[0].State != string(campaign.ItemStateFailed) {
			t.Fatalf("items = %+v, want one failed item", st.Items)
		}
		if len(st.Rollup.Running) != 0 {
			t.Errorf("rollup.running = %v, want empty", st.Rollup.Running)
		}
		if n := w.settledRows(t); n != 1 {
			t.Errorf("campaign_issue_settled rows = %d, want 1", n)
		}
	})
}

// TestInfraParkReopenedPassCategoriesMatchServer is T-MIRROR: the run package
// mirrors the server's three pass-trigger categories (it cannot import the
// server), so a rename on either side fails here.
func TestInfraParkReopenedPassCategoriesMatchServer(t *testing.T) {
	want := []string{
		CategoryStageFixupTriggered,
		CategoryStageConflictResolutionTriggered,
		CategoryStageMergeCandidateVerifyTriggered,
	}
	if got := run.InfraParkReopenedPassCategories(); !reflect.DeepEqual(got, want) {
		t.Fatalf("run.InfraParkReopenedPassCategories() = %v, want %v", got, want)
	}
}
