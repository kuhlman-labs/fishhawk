package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
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
// scoped to the implement stage — the durable signal the StagePushed carve-out
// reads. Since #4815 the fix-up path refuses instead of re-opening on a failed
// stage_fixup_triggered append, so this shape now comes only from chains
// written before that change; the carve-out (and this row) stay.
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
//
// The orchestrator's Audit is a ListForRun-SEQUENCING wrapper over the
// fixture's fake (M1), so the classifier compares real sequences: the run
// fails because the refusal CONSUMED the trigger (a not_executed
// merge_candidate_verified row after it) and no operator reap row exists, not
// because every row reads back at sequence 0.
func TestImplementInfraFailure_MergeCandidateRefusalRestoreFailureStillFailsRun(t *testing.T) {
	f, _ := newMCVStaleRunnerFixture(t)
	seq := &implParkListSeqAudit{auditFake: f.au}
	f.s.cfg.Orchestrator = &orchestrator.Orchestrator{Runs: f.repo, Audit: seq}
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
	// The trigger is CONSUMED: a not_executed verdict whose reason names the
	// capability refusal follows it on the chain, read back with distinct,
	// increasing sequences.
	entries, err := seq.ListForRun(t.Context(), f.runID)
	if err != nil {
		t.Fatal(err)
	}
	triggerSeq, verdictSeq := implParkRequireDistinctSequences(t, entries, f.impl.ID,
		CategoryStageMergeCandidateVerifyTriggered, CategoryMergeCandidateVerified)
	if verdictSeq <= triggerSeq {
		t.Errorf("merge_candidate_verified seq %d <= trigger seq %d; the refusal must consume the trigger", verdictSeq, triggerSeq)
	}
	verdicts := mcvVerdictRows(f.au)
	if len(verdicts) != 1 || verdicts[0].Result != mergeCandidateResultNotExecuted ||
		!strings.HasPrefix(verdicts[0].Reason, mergeCandidateReasonRunnerCapabilityMissing) {
		t.Errorf("verdicts = %+v, want one not_executed row whose reason starts %s", verdicts, mergeCandidateReasonRunnerCapabilityMissing)
	}
	if live := f.s.resolveMergeCandidateVerifyTrigger(t.Context(), f.runID, f.impl.ID); live != nil {
		t.Errorf("trigger still resolves (%+v); the refusal must consume it", live)
	}
}

// TestImplementInfraFailure_LineHolderReapedVerifyPassKeepsLine is T-MCQ-2
// inverted by #4816 (pgtest: real Postgres run, audit and merge-queue stores).
// The merge-candidate line holder's implement stage is re-opened for its
// verify pass; an OPERATOR runner_lost reap category C on that live pass parks
// the run instead of failing it — the Postgres CAS RETURNING row supplies the
// episode stamp and Advance reads the same ended_at back — so the pump keeps
// A's entry live (NOT dropped run_terminal) and B stays held. A retry through
// retryStageAs (the real RetryStageState) re-opens the stage with the live
// trigger still naming a1, and a passed verdict moves A's entry to passed with
// B still held.
func TestImplementInfraFailure_LineHolderReapedVerifyPassKeepsLine(t *testing.T) {
	f := newMCQFixture(t)
	a, b := f.wave()
	if tr := f.triggers(a); len(tr) == 0 || tr[len(tr)-1].ExpectedHeadSHA != "a1" {
		t.Fatalf("precondition: A's implement carries no merge-candidate trigger for a1: %+v", tr)
	}
	f.setStage(a.impl.ID, run.StageStateDispatched)
	f.srv.cfg.Orchestrator = &orchestrator.Orchestrator{Runs: f.repo, Audit: f.audit}

	rec := postReapFailure(t, f.srv, a.id, a.impl.ID,
		reapFailureRequest{Category: "C", Reason: run.InfraParkRunnerLostReason}, withReapOperator)
	if rec.Code != http.StatusOK {
		t.Fatalf("reap-failure = %d: %s", rec.Code, rec.Body.String())
	}

	if got := f.runState(a); got != run.StateRunning {
		t.Fatalf("run A state = %s, want running (an operator-reaped live verify pass parks)", got)
	}
	implParkRequireStageFailedC(t, f.repo, a.impl.ID)
	parks := f.rows(a, run.CategoryStageInfraParked)
	if len(parks) != 1 {
		t.Fatalf("stage_infra_parked rows = %d, want 1", len(parks))
	}
	var park map[string]any
	if err := json.Unmarshal(parks[0].Payload, &park); err != nil || park["reopened_pass"] != run.InfraParkReopenedPassMergeCandidateVerify {
		t.Errorf("park payload = %v (err %v), want reopened_pass %q", park, err, run.InfraParkReopenedPassMergeCandidateVerify)
	}

	f.srv.PumpMergeCandidateQueues(context.Background())
	if e := f.requireState(a, mergequeue.StateLive, ""); e.Phase != mergequeue.PhaseVerifying {
		t.Errorf("A entry phase = %q, want verifying (the parked holder keeps its line)", e.Phase)
	}
	f.requireState(b, mergequeue.StateHeld, "")

	parked, err := f.repo.GetStage(context.Background(), a.impl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.EndedAt == nil {
		t.Fatal("PRECONDITION: the reaped implement stage carries no ended_at")
	}
	op := Identity{Subject: "github:ops", TokenID: "tok-op", Scopes: []string{"write:stages"}}
	if _, err := f.srv.retryStageAs(context.Background(), op, retryActionParams{StageID: a.impl.ID}); err != nil {
		t.Fatalf("retry A's implement: %v", err)
	}
	// RetryStageState re-opens failed -> pending (clearing ended_at, bumping
	// self_retry_count); the retry's orchestrator handoff may then dispatch it.
	retried, err := f.repo.GetStage(context.Background(), a.impl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if (retried.State != run.StageStatePending && retried.State != run.StageStateDispatched) ||
		retried.SelfRetryCount != parked.SelfRetryCount+1 {
		t.Fatalf("A implement after retry = %s ordinal %d, want pending/dispatched at ordinal %d",
			retried.State, retried.SelfRetryCount, parked.SelfRetryCount+1)
	}
	if live := f.srv.resolveMergeCandidateVerifyTrigger(context.Background(), a.id, a.impl.ID); live == nil || live.ExpectedHeadSHA != "a1" {
		t.Fatalf("live trigger after retry = %+v, want one naming a1 (the retry replays the verify-only pass)", live)
	}

	f.verdict(a, mergeCandidateResultPassed)
	f.srv.PumpMergeCandidateQueues(context.Background())
	if e := f.requireState(a, mergequeue.StateLive, ""); e.Phase != mergequeue.PhasePassed {
		t.Errorf("A entry phase after the replayed pass = %q, want passed", e.Phase)
	}
	f.requireState(b, mergequeue.StateHeld, "")
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
	if got := run.InfraParkMergeCandidateVerifiedCategory(); got != CategoryMergeCandidateVerified {
		t.Errorf("run.InfraParkMergeCandidateVerifiedCategory() = %q, want %q", got, CategoryMergeCandidateVerified)
	}
	if got := run.InfraParkOperatorReapCategory(); got != CategoryDispatchReaperFailed {
		t.Errorf("run.InfraParkOperatorReapCategory() = %q, want %q", got, CategoryDispatchReaperFailed)
	}
	t.Run("trigger mirror matches server populated()", implParkRequireTriggerMirrorMatchesServer)
}

// implParkRequireTriggerMirrorMatchesServer binds the run
// package's trigger mirror (#4816) to the server's OWN populated(): a real
// server mergeCandidateVerifyTrigger, marshalled for every server cause
// constant and read with a stamped operator runner_lost reap through
// run.ReadInfraParkHistory, yields ReapedLiveMergeCandidatePass exactly when
// the server would serve it. Each single-anchor-blanked variant (populated()
// false on the server side) must not park. A rename of a trigger JSON key or a
// change to the cause enumeration on either side reddens here.
func implParkRequireTriggerMirrorMatchesServer(t *testing.T) {
	stageID := uuid.New()
	endedAt := time.Date(2026, 11, 11, 12, 0, 0, 123456000, time.UTC)
	cat := run.FailureC
	failed := &run.Stage{ID: stageID, Type: run.StageTypeImplement, State: run.StageStateFailed,
		FailureCategory: &cat, EndedAt: &endedAt, SelfRetryCount: 1}
	failedAt, ordinal := run.InfraParkEpisode(failed)
	reap, _ := json.Marshal(map[string]any{"reason": run.InfraParkRunnerLostReason, "failure_category": "C",
		"failed_at": failedAt, "retry_ordinal": ordinal})

	read := func(tr mergeCandidateVerifyTrigger) bool {
		t.Helper()
		raw, err := json.Marshal(tr)
		if err != nil {
			t.Fatal(err)
		}
		id := stageID
		entries := []*audit.Entry{
			{Sequence: 1, StageID: &id, Category: CategoryStageMergeCandidateVerifyTriggered, Payload: raw},
			{Sequence: 2, StageID: &id, Category: CategoryDispatchReaperFailed, Payload: reap},
		}
		return run.ReadInfraParkHistory(entries, failed).ReapedLiveMergeCandidatePass
	}
	full := func(cause string) mergeCandidateVerifyTrigger {
		return mergeCandidateVerifyTrigger{Branch: mcvBranch, BaseRef: mcvBaseRef, ExpectedHeadSHA: mcvHead,
			Cause: cause, VerifyCommand: mcvCommand, PriorState: string(run.StageStateSucceeded)}
	}
	for _, cause := range []string{mergeCandidateCauseBaseAdvance, mergeCandidateCauseConflictResolution, mergeCandidateCauseFanIn} {
		tr := full(cause)
		if !tr.populated() {
			t.Fatalf("PRECONDITION: server populated() = false for cause %q", cause)
		}
		if !read(tr) {
			t.Errorf("cause %q: ReapedLiveMergeCandidatePass = false, want true (the server serves this trigger)", cause)
		}
	}
	for name, blank := range map[string]func(*mergeCandidateVerifyTrigger){
		"branch":            func(tr *mergeCandidateVerifyTrigger) { tr.Branch = "" },
		"base_ref":          func(tr *mergeCandidateVerifyTrigger) { tr.BaseRef = "" },
		"expected_head_sha": func(tr *mergeCandidateVerifyTrigger) { tr.ExpectedHeadSHA = "" },
		"cause empty":       func(tr *mergeCandidateVerifyTrigger) { tr.Cause = "" },
		"cause invalid":     func(tr *mergeCandidateVerifyTrigger) { tr.Cause = "rebase" },
		"verify_command":    func(tr *mergeCandidateVerifyTrigger) { tr.VerifyCommand = "" },
	} {
		tr := full(mergeCandidateCauseBaseAdvance)
		blank(&tr)
		if tr.populated() {
			t.Fatalf("PRECONDITION: server populated() = true with %s blanked", name)
		}
		if read(tr) {
			t.Errorf("%s blanked: ReapedLiveMergeCandidatePass = true, want false (the server would not serve this trigger)", name)
		}
	}
}

// --- #4816: sequence- and episode-faithful fakes (M1) -------------------------

// implParkSeqListForRun returns a's appended rows for runID with the SAME
// 1-based append index ListForRunByCategory stamps (sequencedAuditFake, or
// auditFake.stampSequence). The shared fake's ListForRun leaves Sequence 0,
// which would collapse every newest-wins comparison ReadInfraParkHistory makes.
func implParkSeqListForRun(a *auditFake, runID uuid.UUID) []*audit.Entry {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*audit.Entry
	for i, p := range a.appended {
		if p.RunID != runID {
			continue
		}
		rid := p.RunID
		out = append(out, &audit.Entry{
			Sequence: int64(i + 1), RunID: &rid, StageID: p.StageID, Timestamp: p.Timestamp,
			Category: p.Category, ActorKind: p.ActorKind, Payload: p.Payload,
		})
	}
	return out
}

// implParkSeqAudit is sequencedAuditFake with a sequenced ListForRun and an
// optional per-category append refusal (failCategories), the fault the H2 and
// record-failed pins inject.
type implParkSeqAudit struct {
	*sequencedAuditFake
	failMu         sync.Mutex
	failCategories map[string]bool
}

func newImplParkSeqAudit() *implParkSeqAudit {
	return &implParkSeqAudit{sequencedAuditFake: newSequencedAuditFake()}
}

func (a *implParkSeqAudit) setFailCategories(categories ...string) {
	a.failMu.Lock()
	defer a.failMu.Unlock()
	a.failCategories = map[string]bool{}
	for _, c := range categories {
		a.failCategories[c] = true
	}
}

func (a *implParkSeqAudit) ListForRun(_ context.Context, runID uuid.UUID) ([]*audit.Entry, error) {
	return implParkSeqListForRun(a.auditFake, runID), nil
}

func (a *implParkSeqAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	a.failMu.Lock()
	refuse := a.failCategories[p.Category]
	a.failMu.Unlock()
	if refuse {
		return nil, fmt.Errorf("implParkSeqAudit: injected append refusal for %s", p.Category)
	}
	return a.sequencedAuditFake.AppendChained(ctx, p)
}

// implParkListSeqAudit adds the sequenced ListForRun to a plain auditFake (the
// stale-runner fixture's), leaving every other method unchanged.
type implParkListSeqAudit struct {
	*auditFake
}

func (a *implParkListSeqAudit) ListForRun(_ context.Context, runID uuid.UUID) ([]*audit.Entry, error) {
	return implParkSeqListForRun(a.auditFake, runID), nil
}

// implParkEpisodeRepo is fixupRecoveryRepo made episode-faithful: a terminal
// TransitionStage / TransitionStageFrom / TransitionStageFromAttempt stamps
// EndedAt (microsecond resolution, as Postgres stores it) and RetryStage bumps
// SelfRetryCount, mirroring UpdateStageState / RetryStageState. The shared
// fakes do neither, so every failure episode would read ("", 0).
type implParkEpisodeRepo struct {
	*fixupRecoveryRepo
}

func (r *implParkEpisodeRepo) stampEnded(st *run.Stage, from run.StageState) {
	if st == nil || st.State == from || !st.State.IsTerminal() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC().Truncate(time.Microsecond)
	st.EndedAt = &now
}

func (r *implParkEpisodeRepo) TransitionStage(ctx context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	prior, err := r.GetStage(ctx, id)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	from := prior.State
	r.mu.Unlock()
	st, err := r.fixupRecoveryRepo.TransitionStage(ctx, id, to, c)
	if err == nil {
		r.stampEnded(st, from)
	}
	return st, err
}

func (r *implParkEpisodeRepo) TransitionStageFrom(ctx context.Context, id uuid.UUID, from, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	st, err := r.orchestratorRepo.TransitionStageFrom(ctx, id, from, to, c)
	if err == nil {
		r.stampEnded(st, from)
	}
	return st, err
}

func (r *implParkEpisodeRepo) TransitionStageFromAttempt(ctx context.Context, id uuid.UUID, from, to run.StageState, expectedAttempt string, c *run.StageCompletion) (*run.Stage, error) {
	st, err := r.orchestratorRepo.TransitionStageFromAttempt(ctx, id, from, to, expectedAttempt, c)
	if err == nil {
		r.stampEnded(st, from)
	}
	return st, err
}

func (r *implParkEpisodeRepo) RetryStage(ctx context.Context, id uuid.UUID, to run.StageState) (*run.Stage, error) {
	st, err := r.orchestratorRepo.RetryStage(ctx, id, to)
	if err == nil {
		r.mu.Lock()
		st.SelfRetryCount++
		r.mu.Unlock()
	}
	return st, err
}

// implParkRequireDistinctSequences asserts (a PRECONDITION of every #4816 pin)
// that the newest row of each named category scoped to stageID reads back
// through ListForRun with a positive sequence, and that the sequences are
// pairwise distinct and increasing in the order given. It returns them.
func implParkRequireDistinctSequences(t *testing.T, entries []*audit.Entry, stageID uuid.UUID, categories ...string) (int64, int64) {
	t.Helper()
	seqs := make([]int64, len(categories))
	for i, c := range categories {
		for _, e := range entries {
			if e.Category == c && e.StageID != nil && *e.StageID == stageID && e.Sequence > seqs[i] {
				seqs[i] = e.Sequence
			}
		}
		if seqs[i] <= 0 {
			t.Fatalf("PRECONDITION: no %s row scoped to the stage reads back with a sequence", c)
		}
		if i > 0 && seqs[i] <= seqs[i-1] {
			t.Fatalf("PRECONDITION: %s seq %d is not after %s seq %d", c, seqs[i], categories[i-1], seqs[i-1])
		}
	}
	if len(seqs) < 2 {
		return seqs[0], 0
	}
	return seqs[0], seqs[len(seqs)-1]
}

// implParkReplayWorld is the fake-store world of the #4816 replay e2e: a
// feature_change run whose implement stage was re-opened for a LIVE
// merge-candidate verify-only pass (pull_request_opened, then a real populated
// trigger, implement dispatched, review re-parked pending), with one open
// implement-review concern, the signed /pull-request handler and a real
// orchestrator over the M1 fakes.
type implParkReplayWorld struct {
	s       *Server
	rr      *implParkEpisodeRepo
	au      *implParkSeqAudit
	cr      *fakeConcernRepo
	priv    []byte
	runID   uuid.UUID
	impl    *run.Stage
	review  *run.Stage
	concern uuid.UUID
}

func newImplParkReplayWorld(t *testing.T) *implParkReplayWorld {
	t.Helper()
	base := &fixupRecoveryRepo{orchestratorRepo: newOrchestratorRepo()}
	rr := &implParkEpisodeRepo{fixupRecoveryRepo: base}
	au := newImplParkSeqAudit()
	cr := newFakeConcernRepo()
	sf := newSigningFake()
	s := New(Config{
		Addr: "127.0.0.1:0", SigningRepo: sf, ArtifactRepo: newFakeArtifactRepo(),
		AuditRepo: au, RunRepo: rr, ConcernRepo: cr,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr, Audit: au},
	})
	runRow := base.seedRun()
	plan := base.seedStage(runRow.ID, 1, run.StageStateSucceeded)
	impl := base.seedStage(runRow.ID, 2, run.StageStateDispatched)
	review := base.seedStage(runRow.ID, 3, run.StageStatePending)
	base.mu.Lock()
	plan.Type = run.StageTypePlan
	impl.Type = run.StageTypeImplement
	impl.SelfRetryCount = 1
	review.Type = run.StageTypeReview
	base.mu.Unlock()

	ctx := context.Background()
	implID := impl.ID
	opened, _ := json.Marshal(map[string]any{"stage_id": implID.String(), "pr_number": 42})
	kind := audit.ActorKind("runner")
	if _, err := au.AppendChained(ctx, audit.ChainAppendParams{
		RunID: runRow.ID, StageID: &implID, Category: "pull_request_opened", ActorKind: &kind, Payload: opened,
	}); err != nil {
		t.Fatalf("seed pull_request_opened: %v", err)
	}
	if err := s.appendMergeCandidateTrigger(ctx, runRow.ID, impl.ID, mergeCandidateVerifyTrigger{
		Branch: mcvBranch, BaseRef: mcvBaseRef, ExpectedHeadSHA: mcvHead, Cause: mergeCandidateCauseBaseAdvance,
		VerifyCommand: mcvCommand, PriorState: string(run.StageStateSucceeded), ReparkedReviewStageID: review.ID.String(),
	}, mcvOperator()); err != nil {
		t.Fatalf("append trigger: %v", err)
	}
	c := seedConcernRow(t, cr, runRow.ID, impl.ID, concern.StageKindImplement, 10, "impl concern")
	priv, _ := sf.issue(t, runRow.ID)
	return &implParkReplayWorld{s: s, rr: rr, au: au, cr: cr, priv: priv, runID: runRow.ID,
		impl: impl, review: review, concern: c.ID}
}

func (w *implParkReplayWorld) reap(t *testing.T, category, reason string) {
	t.Helper()
	rec := postReapFailure(t, w.s, w.runID, w.impl.ID, reapFailureRequest{Category: category, Reason: reason}, withReapOperator)
	if rec.Code != http.StatusOK {
		t.Fatalf("reap-failure = %d: %s", rec.Code, rec.Body.String())
	}
	var resp reapFailureResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !resp.Transitioned {
		t.Fatalf("reap-failure response = %s (err %v), want transitioned", rec.Body.String(), err)
	}
}

// requireLiveReapPreconditions asserts the M1 preconditions: the trigger and
// the reap row read back with distinct increasing sequences, and the failed
// stage carries an ended_at.
func (w *implParkReplayWorld) requireLiveReapPreconditions(t *testing.T) {
	t.Helper()
	entries, err := w.au.ListForRun(t.Context(), w.runID)
	if err != nil {
		t.Fatal(err)
	}
	implParkRequireDistinctSequences(t, entries, w.impl.ID,
		"pull_request_opened", CategoryStageMergeCandidateVerifyTriggered, CategoryDispatchReaperFailed)
	if st, _ := w.rr.GetStage(t.Context(), w.impl.ID); st.EndedAt == nil {
		t.Fatal("PRECONDITION: the failed implement stage carries no ended_at")
	}
}

// TestImplementInfraFailure_ReapedLiveVerifyPassParksAndRetryReplays is #4816
// done-means 1 on the M1 fakes: an OPERATOR runner_lost reap of a LIVE
// merge-candidate verify-only pass crosses the reap handler (episode stamp) ->
// Orchestrator.Advance (audit-derived classification) and PARKS the run; the
// real HTTP retry handler then re-opens the stage keeping the reviewed tree's
// concern and serving the same pass; and the signed passed verdict settles
// the pass, restoring implement and the review gate and consuming the trigger.
func TestImplementInfraFailure_ReapedLiveVerifyPassParksAndRetryReplays(t *testing.T) {
	w := newImplParkReplayWorld(t)
	w.reap(t, "C", run.InfraParkRunnerLostReason)
	w.requireLiveReapPreconditions(t)

	implParkRequireStageFailedC(t, w.rr, w.impl.ID)
	implParkRequireRunState(t, w.rr.fixupRecoveryRepo, w.runID, run.StateRunning)
	parks := implParkRows(w.au.auditFake, w.impl.ID)
	if len(parks) != 1 {
		t.Fatalf("stage_infra_parked rows = %d, want 1", len(parks))
	}
	var park map[string]any
	if err := json.Unmarshal(parks[0].Payload, &park); err != nil || park["reopened_pass"] != run.InfraParkReopenedPassMergeCandidateVerify {
		t.Errorf("park payload = %v (err %v), want reopened_pass %q", park, err, run.InfraParkReopenedPassMergeCandidateVerify)
	}
	failedStage, _ := w.rr.GetStage(t.Context(), w.impl.ID)
	wantAt, wantOrdinal := run.InfraParkEpisode(failedStage)
	reaps := auditEntries(w.au.auditFake, CategoryDispatchReaperFailed)
	if len(reaps) != 1 {
		t.Fatalf("dispatch_reaper_failed rows = %d, want 1", len(reaps))
	}
	var reap map[string]any
	_ = json.Unmarshal(reaps[0].Payload, &reap)
	if reap["failed_at"] != wantAt || reap["retry_ordinal"] != float64(wantOrdinal) {
		t.Errorf("reap stamp = %v/%v, want the stage's episode %q/%d", reap["failed_at"], reap["retry_ordinal"], wantAt, wantOrdinal)
	}
	if live := w.s.resolveMergeCandidateVerifyTrigger(t.Context(), w.runID, w.impl.ID); live == nil || live.ExpectedHeadSHA != mcvHead {
		t.Fatalf("trigger after the park = %+v, want it still live at %s", live, mcvHead)
	}

	if rec := postRetry(t, w.s, w.impl.ID); rec.Code != http.StatusOK {
		t.Fatalf("retry = %d: %s", rec.Code, rec.Body.String())
	}
	retried, _ := w.rr.GetStage(t.Context(), w.impl.ID)
	if retried.State == run.StageStateFailed || retried.SelfRetryCount != wantOrdinal+1 {
		t.Fatalf("implement after retry = %s ordinal %d, want re-opened at ordinal %d", retried.State, retried.SelfRetryCount, wantOrdinal+1)
	}
	if got := concernState(t, w.cr, w.concern); got != concern.StateRaised {
		t.Errorf("implement-review concern after retry = %q, want raised (a replayed verify-only pass keeps the reviewed tree)", got)
	}
	if n := len(auditEntries(w.au.auditFake, "verified_tree_discarded")); n != 0 {
		t.Errorf("verified_tree_discarded rows = %d, want 0", n)
	}
	receipts := auditEntries(w.au.auditFake, CategoryStageRetried)
	if len(receipts) != 1 {
		t.Fatalf("stage_retried rows = %d, want 1", len(receipts))
	}
	var receipt map[string]any
	_ = json.Unmarshal(receipts[0].Payload, &receipt)
	if receipt["replay_kept_reviewed_tree"] != true {
		t.Errorf("stage_retried replay_kept_reviewed_tree = %v, want true", receipt["replay_kept_reviewed_tree"])
	}
	if live := w.s.resolveMergeCandidateVerifyTrigger(t.Context(), w.runID, w.impl.ID); live == nil || live.ExpectedHeadSHA != mcvHead {
		t.Fatalf("trigger after the retry = %+v, want the same pass served at %s", live, mcvHead)
	}

	// The runner picks the replayed pass up and ships a passed verdict.
	if retried.State == run.StageStatePending {
		if _, err := w.rr.TransitionStageFrom(t.Context(), w.impl.ID, run.StageStatePending, run.StageStateDispatched, nil); err != nil {
			t.Fatalf("dispatch the retried stage: %v", err)
		}
	}
	if cur, _ := w.rr.GetStage(t.Context(), w.impl.ID); cur.State == run.StageStateDispatched {
		if _, err := w.rr.TransitionStageFrom(t.Context(), w.impl.ID, run.StageStateDispatched, run.StageStateRunning, nil); err != nil {
			t.Fatalf("start the retried stage: %v", err)
		}
	}
	if rec := shipPRRequest(t, w.s, w.runID, w.impl.ID, w.priv, mcvReportBody(mcvHead, mergeCandidateResultPassed, "", "ok"), ""); rec.Code != http.StatusOK {
		t.Fatalf("merge_candidate_verified report = %d: %s", rec.Code, rec.Body.String())
	}
	if got, _ := w.rr.GetStage(t.Context(), w.impl.ID); got.State != run.StageStateSucceeded {
		t.Errorf("implement after the passed pass = %s, want succeeded (its pre-pass gate)", got.State)
	}
	// The settle restores implement to the trigger's prior state and re-walks
	// the re-parked review stage through Advance (the
	// TestShipPullRequest_MergeCandidateVerified_SettlesToPriorStateAndAdvances
	// contract), so the review leaves pending.
	if got, _ := w.rr.GetStage(t.Context(), w.review.ID); got.State == run.StageStatePending {
		t.Errorf("review after the passed pass = %s, want re-walked by Advance off pending", got.State)
	}
	verdicts := mcvVerdictRows(w.au.auditFake)
	if len(verdicts) != 1 || verdicts[0].Result != mergeCandidateResultPassed {
		t.Errorf("merge_candidate_verified rows = %+v, want one passed", verdicts)
	}
	if live := w.s.resolveMergeCandidateVerifyTrigger(t.Context(), w.runID, w.impl.ID); live != nil {
		t.Errorf("trigger after the verdict = %+v, want consumed", live)
	}
	implParkRequireRunState(t, w.rr.fixupRecoveryRepo, w.runID, run.StateRunning)
}

// TestImplementInfraFailure_OperatorNonRunnerLostReapOfLiveVerifyPassFailsRun
// pins approval condition C2 at the server: an OPERATOR category-C reap of a
// LIVE verify-only pass whose reason is NOT runner_lost — the detached
// reaper's non-zero-exit report (reason parsed from runner_failed, or
// synthesized), the zero-exit strand reaper, the concurrency-slot spawn
// compensation — is stamped with the current episode but still FAILS the run:
// only a runner-loss reap produces parking evidence.
func TestImplementInfraFailure_OperatorNonRunnerLostReapOfLiveVerifyPassFailsRun(t *testing.T) {
	for _, reason := range []string{
		"runner exited 1: verify: go test failed",
		"runner exited 0 without settling the stage (stage still dispatched)",
		"fishhawk_dispatch_stage: spawn failed after admission: exec: not found",
	} {
		t.Run(reason, func(t *testing.T) {
			w := newImplParkReplayWorld(t)
			w.reap(t, "C", reason)
			w.requireLiveReapPreconditions(t)
			reaps := auditEntries(w.au.auditFake, CategoryDispatchReaperFailed)
			var reap map[string]any
			_ = json.Unmarshal(reaps[len(reaps)-1].Payload, &reap)
			failedStage, _ := w.rr.GetStage(t.Context(), w.impl.ID)
			if at, _ := run.InfraParkEpisode(failedStage); reap["failed_at"] != at || at == "" {
				t.Fatalf("PRECONDITION: reap row stamp %v, want the current episode %q (only the reason may refuse it)", reap["failed_at"], at)
			}
			implParkRequireStageFailedC(t, w.rr, w.impl.ID)
			implParkRequireRunState(t, w.rr.fixupRecoveryRepo, w.runID, run.StateFailed)
			if n := len(implParkRows(w.au.auditFake, w.impl.ID)); n != 0 {
				t.Errorf("stage_infra_parked rows = %d, want 0 (a non-runner_lost reap never parks)", n)
			}
		})
	}
}
