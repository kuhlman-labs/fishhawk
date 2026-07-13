package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// autodrive_test.go is owned solely by E25.6 slice 2 (the gate-actor
// slice). It drives the campaign auto-driver (AutoDriveRunGate) across the
// real delegation-evaluation -> gate-action service method -> state
// transition -> audit seam, using the package's shared-backing fake
// harness (newDelegatedApprovalServer + startDriveE2ERun + the audit /
// concern fakes that read back their own writes) so the decision->action
// seam is exercised end-to-end in-process — the #618 concern — without a
// per-package Postgres. One BEHAVIORAL test per enumerated mode asserts
// that branch's observable effect (state change + exact audit category),
// plus the fail-closed observe-only modes and the double-gate derivations.

// autoDriveSpecYAML delegates every knob and lists both reviewer_reject
// (legacy bare token, maps to the gating class) and requirement_arbitration
// as must_page_human events. The implement stage has agent-only reviewers
// so its review authority is GATING (a reject pages); the plan stage is
// advisory (agent + human).
const autoDriveSpecYAML = `version: "0.5"
roles:
  tech_lead:
    members: ["@org/tech-leads"]
workflows:
  feature_change:
    operator_agent:
      may_approve: clean_dual_approval
      may_route_fixup: convergent_concerns
      may_waive: solo_low
      may_retry: infra_flake
      may_merge: gates_resolved_ci_green
      must_page_human: [reviewer_reject, requirement_arbitration]
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          agent: 2
          human: 1
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvers:
              any_of: [tech_lead]
      - id: implement
        type: implement
        executor:
          agent: claude-code
        reviewers:
          agent: 2
        produces:
          - artifact: pull_request
        gates:
          - type: approval
            approvers:
              any_of: [tech_lead]
`

// autoDriveRepo is driveE2ERepo plus a working stage-level RetryStage
// (the base fakeRepo errors on it) so run.RetryStage's failed → pending
// reopen lands — the auto-retry path needs it. Owned by this slice's test
// file; it does not touch the shared driveE2ERepo helper.
type autoDriveRepo struct {
	*driveE2ERepo
}

func (r *autoDriveRepo) RetryStage(_ context.Context, id uuid.UUID, to run.StageState) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, stages := range r.stagesByRun {
		for _, st := range stages {
			if st.ID == id {
				st.State = to
				st.UpdatedAt = time.Now().UTC()
				return st, nil
			}
		}
	}
	return nil, run.ErrNotFound
}

// newAutoDriveServer wires the delegation + gate-action + orchestrator
// stack over autoDriveRepo, mirroring newDelegatedApprovalServer but with
// the RetryStage-capable repo.
func newAutoDriveServer(t *testing.T) (*Server, *autoDriveRepo, *auditFake, *fakeConcernRepo) {
	t.Helper()
	repo := &autoDriveRepo{driveE2ERepo: &driveE2ERepo{fakeRepo: newFakeRepo()}}
	au := newAuditFake()
	cr := newFakeConcernRepo()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      repo,
		AuditRepo:    au,
		ConcernRepo:  cr,
		ApprovalRepo: newFakeApprovalRepo(),
		Orchestrator: &orchestrator.Orchestrator{Runs: repo},
	})
	return s, repo, au, cr
}

// fakeMerger records GitHubMerger.MergePullRequest calls and can inject a
// failure to exercise the dispatch-error path.
type fakeMerger struct {
	called int
	gotRun *run.Run
	err    error
}

func (m *fakeMerger) MergePullRequest(_ context.Context, r *run.Run) error {
	m.called++
	m.gotRun = r
	return m.err
}

// startAutoDriveRun creates the gated plan+implement run under
// autoDriveSpecYAML and returns the run id plus its two stages (plan,
// implement). The plan stage comes back at awaiting_approval (the create
// handler's gate); tests mutate stage/run state for the mode under test.
func startAutoDriveRun(t *testing.T, s *Server, repo *autoDriveRepo) (uuid.UUID, []*run.Stage) {
	t.Helper()
	runID, _ := startDriveE2ERun(t, s, repo.driveE2ERepo, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "cli", "workflow_spec": autoDriveSpecYAML,
	})
	return runID, repo.stagesFor(runID)
}

// getRun re-reads the run row so AutoDriveRunGate receives the same shape
// the driver would (post-mutation state, PR url).
func getRun(t *testing.T, repo *autoDriveRepo, runID uuid.UUID) *run.Run {
	t.Helper()
	r, err := repo.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	return r
}

// seedOpenConcern inserts one open concern with an explicit
// severity/category so the solo_low / requirement_arbitration / fix-up
// paths can be driven precisely.
func seedOpenConcern(t *testing.T, cr *fakeConcernRepo, runID, stageID uuid.UUID, stageKind, severity, category, note string) *concern.Concern {
	t.Helper()
	rows, err := cr.InsertRaised(context.Background(), concern.InsertRaisedParams{
		RunID:                runID,
		StageID:              stageID,
		StageKind:            stageKind,
		ReviewerModel:        "claude-opus-4-8",
		OriginReviewSequence: 1,
		Concerns:             []concern.RaisedConcern{{Severity: severity, Category: category, Note: note}},
	})
	if err != nil {
		t.Fatalf("seed concern: %v", err)
	}
	return rows[0]
}

// auditEntry returns the single appended entry of the given category, or
// fails if there is not exactly one.
func auditEntry(t *testing.T, au *auditFake, category string) audit.ChainAppendParams {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var match *audit.ChainAppendParams
	for i := range au.appended {
		if au.appended[i].Category == category {
			if match != nil {
				t.Fatalf("more than one %q entry appended", category)
			}
			match = &au.appended[i]
		}
	}
	if match == nil {
		t.Fatalf("no %q entry appended", category)
	}
	return *match
}

// countAudit returns how many appended entries carry the category.
func countAudit(au *auditFake, category string) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	n := 0
	for i := range au.appended {
		if au.appended[i].Category == category {
			n++
		}
	}
	return n
}

// assertOperatorActor asserts an audit entry was stamped as the campaign
// operator-agent acting (actor_kind=agent, the operator-agent/campaign
// subject) — the ADR-040 attribution the in-process auto-action carries.
func assertOperatorActor(t *testing.T, e audit.ChainAppendParams) {
	t.Helper()
	if e.ActorKind == nil || *e.ActorKind != audit.ActorAgent {
		t.Errorf("ActorKind = %v, want agent", e.ActorKind)
	}
	if e.ActorSubject == nil || *e.ActorSubject != operatorrole.CampaignActorSubject {
		t.Errorf("ActorSubject = %v, want %q", e.ActorSubject, operatorrole.CampaignActorSubject)
	}
}

func auditDelegatedRule(t *testing.T, e audit.ChainAppendParams) string {
	t.Helper()
	var p struct {
		Delegated string `json:"delegated"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return p.Delegated
}

// --- (a) may_approve(clean_dual_approval) -> auto-approve --------------------

func TestAutoDriveRunGate_Approve(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, stages := startAutoDriveRun(t, s, repo)
	plan := stages[0]
	// Clean dual approval: both plan reviewers approved, no concerns.
	seedReviewEntry(t, au, runID, 1, "plan_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	seedReviewEntry(t, au, runID, 2, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
	seedReviewEntry(t, au, runID, 3, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if !out.Acted || out.Action != delegation.ActionApprove {
		t.Fatalf("outcome = %+v, want acted approve", out)
	}
	if plan.State != run.StageStateSucceeded {
		t.Errorf("plan stage = %q, want succeeded (auto-advanced)", plan.State)
	}
	e := auditEntry(t, au, "approval_submitted")
	assertOperatorActor(t, e)
	if rule := auditDelegatedRule(t, e); rule != "clean_dual_approval" {
		t.Errorf("delegated rule = %q, want clean_dual_approval", rule)
	}
}

// --- (b) may_route_fixup(convergent_concerns) -> auto-route fix-up -----------

func TestAutoDriveRunGate_RouteFixup(t *testing.T) {
	s, repo, au, cr := newAutoDriveServer(t)
	runID, stages := startAutoDriveRun(t, s, repo)
	plan, impl := stages[0], stages[1]
	plan.State = run.StageStateSucceeded
	impl.State = run.StageStateAwaitingApproval

	// Implement review round complete with approve_with_concerns (no reject)
	// and one open implement concern -> convergent_concerns met.
	seedReviewEntry(t, au, runID, 1, "implement_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	seedReviewEntry(t, au, runID, 2, "implement_reviewed", planreview.ImplementReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApproveWithConcerns})
	seedReviewEntry(t, au, runID, 3, "implement_reviewed", planreview.ImplementReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApproveWithConcerns})
	seedOpenConcern(t, cr, runID, impl.ID, concern.StageKindImplement, "medium", "scope", "tighten the seam")

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if !out.Acted || out.Action != delegation.ActionRouteFixup {
		t.Fatalf("outcome = %+v, want acted route_fixup", out)
	}
	e := auditEntry(t, au, CategoryStageFixupTriggered)
	assertOperatorActor(t, e)
	if rule := auditDelegatedRule(t, e); rule != "convergent_concerns" {
		t.Errorf("delegated rule = %q, want convergent_concerns", rule)
	}
}

// --- (c) may_retry(infra_flake) -> auto-retry -------------------------------

func TestAutoDriveRunGate_Retry(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, stages := startAutoDriveRun(t, s, repo)
	plan, impl := stages[0], stages[1]
	plan.State = run.StageStateSucceeded
	impl.State = run.StageStateFailed
	cat := run.FailureA
	reason := "verify command \"scripts/test\" still failing: verify_infra_flake_retry"
	impl.FailureCategory = &cat
	impl.FailureReason = &reason
	if _, err := repo.TransitionRun(context.Background(), runID, run.StateFailed); err != nil {
		t.Fatalf("TransitionRun -> failed: %v", err)
	}

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if !out.Acted || out.Action != delegation.ActionRetry {
		t.Fatalf("outcome = %+v, want acted retry", out)
	}
	// The failed stage was re-opened (and the orchestrator then dispatched
	// it): it is no longer failed, and the run was un-terminalled.
	if impl.State == run.StageStateFailed {
		t.Errorf("implement stage = %q, want re-opened (not failed)", impl.State)
	}
	if rr := getRun(t, repo, runID); rr.State != run.StateRunning {
		t.Errorf("run state = %q, want running (un-terminalled by retry)", rr.State)
	}
	assertOperatorActor(t, auditEntry(t, au, CategoryStageRetried))
}

// --- (d) may_merge(gates_resolved_ci_green) -> enable auto-merge, NO settle --

func TestAutoDriveRunGate_Merge(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, stages := startAutoDriveRun(t, s, repo)
	stages[0].State = run.StageStateSucceeded
	stages[1].State = run.StageStateSucceeded
	if _, err := repo.TransitionRun(context.Background(), runID, run.StateRunning); err != nil {
		t.Fatalf("TransitionRun -> running: %v", err)
	}
	if _, err := repo.SetRunPullRequestURL(context.Background(), runID, "https://github.com/x/y/pull/7"); err != nil {
		t.Fatalf("SetRunPullRequestURL: %v", err)
	}
	// Latest drive auto-advance is checks_green_awaiting_merge.
	seedReviewEntry(t, au, runID, 5, drive.Category, drive.Advance{Rule: drive.RuleChecksGreenAwaitingMerge})

	merger := &fakeMerger{}
	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), merger, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if !out.Acted || out.Action != delegation.ActionMerge {
		t.Fatalf("outcome = %+v, want acted merge", out)
	}
	if merger.called != 1 {
		t.Errorf("merger called %d times, want 1", merger.called)
	}
	if merger.gotRun == nil || merger.gotRun.ID != runID {
		t.Errorf("merger got run %v, want %v", merger.gotRun, runID)
	}
	// may_merge only ENABLES GitHub auto-merge; the actor must NOT settle
	// the run in-process. pr_merged + completion are left to the
	// pull_request-closed webhook that fires when GitHub actually merges, so
	// no pr_merged entry is written on the auto-drive path itself.
	if countAudit(au, CategoryPRMerged) != 0 {
		t.Errorf("%q entry written; the actor settled the run before GitHub merged (auto-merge is only enabled, not confirmed)", CategoryPRMerged)
	}
}

// --- (d') acceptance gate on may_merge (E31.17 / #1568) ---------------------

// autoDriveAcceptanceSpecYAML declares an acceptance stage alongside the
// may_merge delegation, so the acceptance gate at the AutoDriveRunGate merge
// call site is exercisable. version 1.1 (workflow-v1) supports both the
// operator_agent block and the acceptance stage type.
const autoDriveAcceptanceSpecYAML = `version: "1.1"
workflows:
  feature_change:
    operator_agent:
      may_merge: gates_resolved_ci_green
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
      - id: acceptance
        type: acceptance
        executor:
          agent: claude-code
`

// seedAcceptanceMergeRun constructs an acceptance-declaring run whose delegation
// may_merge condition IS met (latest drive entry is checks_green_awaiting_merge,
// PR open, all approval gates resolved, no open concerns) — so the ONLY thing
// that can still block the merge is the call-site acceptance gate. The acceptance
// stage is materialized in accState; when verdict != "" an acceptance_outcome_recorded
// entry is seeded.
func seedAcceptanceMergeRun(t *testing.T, repo *autoDriveRepo, au *auditFake, accState run.StageState, verdict string) *run.Run {
	t.Helper()
	runID := uuid.New()
	pr := "https://github.com/x/y/pull/7"
	runRow := &run.Run{
		ID:             runID,
		State:          run.StateRunning,
		WorkflowID:     "feature_change",
		WorkflowSpec:   []byte(autoDriveAcceptanceSpecYAML),
		PullRequestURL: &pr,
	}
	repo.mu.Lock()
	repo.runs[runID] = runRow
	repo.stagesByRun[runID] = []*run.Stage{
		{ID: uuid.New(), RunID: runID, Sequence: 0, Type: run.StageTypePlan, State: run.StageStateSucceeded},
		{ID: uuid.New(), RunID: runID, Sequence: 1, Type: run.StageTypeImplement, State: run.StageStateSucceeded},
		{ID: uuid.New(), RunID: runID, Sequence: 2, Type: run.StageTypeAcceptance, State: accState},
	}
	repo.mu.Unlock()
	// Latest drive auto-advance is checks_green_awaiting_merge → may_merge Met.
	seedReviewEntry(t, au, runID, 5, drive.Category, drive.Advance{Rule: drive.RuleChecksGreenAwaitingMerge})
	if verdict != "" {
		seedAcceptanceOutcome(au, runID, 6, verdict)
	}
	return runRow
}

// TestAutoDriveRunGate_Merge_AcceptancePending_ObserveOnly pins that the
// call-site acceptance gate blocks the merge while the acceptance stage is
// pending: the delegation may_merge is Met, but the merger is NOT called.
func TestAutoDriveRunGate_Merge_AcceptancePending_ObserveOnly(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runRow := seedAcceptanceMergeRun(t, repo, au, run.StageStateRunning, "") // non-terminal, no verdict

	merger := &fakeMerger{}
	out, err := s.AutoDriveRunGate(context.Background(), runRow, campaignOperatorIdentity(), merger, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if out.Acted {
		t.Errorf("outcome = %+v, want observe-only (acceptance pending blocks the merge)", out)
	}
	if merger.called != 0 {
		t.Errorf("merger called %d times, want 0 — the acceptance gate must block the merge", merger.called)
	}
}

// TestAutoDriveRunGate_Merge_AcceptanceOutcomeUnknown_ObserveOnly pins the
// settled-outcome-unknown block: terminal acceptance stage, no verdict → no merge.
func TestAutoDriveRunGate_Merge_AcceptanceOutcomeUnknown_ObserveOnly(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runRow := seedAcceptanceMergeRun(t, repo, au, run.StageStateSucceeded, "") // terminal, no verdict

	merger := &fakeMerger{}
	out, err := s.AutoDriveRunGate(context.Background(), runRow, campaignOperatorIdentity(), merger, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if out.Acted || merger.called != 0 {
		t.Errorf("outcome=%+v merger.called=%d, want observe-only + 0 merges (outcome unknown blocks)", out, merger.called)
	}
}

// TestAutoDriveRunGate_Merge_AcceptanceFailed_ObserveOnly pins the failed-verdict
// block: a failed acceptance verdict → no merge.
func TestAutoDriveRunGate_Merge_AcceptanceFailed_ObserveOnly(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runRow := seedAcceptanceMergeRun(t, repo, au, run.StageStateSucceeded, acceptanceVerdictFailed)

	merger := &fakeMerger{}
	out, err := s.AutoDriveRunGate(context.Background(), runRow, campaignOperatorIdentity(), merger, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if out.Acted || merger.called != 0 {
		t.Errorf("outcome=%+v merger.called=%d, want observe-only + 0 merges (failed verdict blocks)", out, merger.called)
	}
}

// TestAutoDriveRunGate_Merge_AcceptancePassed_Merges pins the positive path:
// a passed acceptance verdict lets the auto-driver enable the merge.
func TestAutoDriveRunGate_Merge_AcceptancePassed_Merges(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runRow := seedAcceptanceMergeRun(t, repo, au, run.StageStateSucceeded, acceptanceVerdictPassed)

	merger := &fakeMerger{}
	out, err := s.AutoDriveRunGate(context.Background(), runRow, campaignOperatorIdentity(), merger, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if !out.Acted || out.Action != delegation.ActionMerge {
		t.Fatalf("outcome = %+v, want acted merge on a passed acceptance", out)
	}
	if merger.called != 1 {
		t.Errorf("merger called %d times, want 1 — a passed acceptance must not block the merge", merger.called)
	}
}

// TestAutoDriveRunGate_Merge_AcceptanceSkippedOutOfScope_Merges pins the E38.3 /
// #1877 admit: a terminal acceptance stage settled via the out-of-scope skip
// marker (no verdict) is merge-eligible, so the delegated may_merge proceeds to
// the merge seam exactly like a passed verdict.
func TestAutoDriveRunGate_Merge_AcceptanceSkippedOutOfScope_Merges(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runRow := seedAcceptanceMergeRun(t, repo, au, run.StageStateSucceeded, "") // terminal, no verdict
	repo.mu.Lock()
	accID := repo.stagesByRun[runRow.ID][2].ID // plan, implement, acceptance
	repo.mu.Unlock()
	seedAcceptanceSkipMarker(au, runRow.ID, accID)

	merger := &fakeMerger{}
	out, err := s.AutoDriveRunGate(context.Background(), runRow, campaignOperatorIdentity(), merger, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if !out.Acted || out.Action != delegation.ActionMerge {
		t.Fatalf("outcome = %+v, want acted merge on a skip-settled acceptance", out)
	}
	if merger.called != 1 {
		t.Errorf("merger called %d times, want 1 — a skip-settled acceptance must not block the merge", merger.called)
	}
}

// TestAutoDriveRunGate_Merge_AcceptanceReadError_ObserveOnly pins the
// fail-closed posture: an acceptance/audit read error never merges (the
// acceptanceGateState error and the evaluator error both resolve to
// observe-only; neither yields a merge).
func TestAutoDriveRunGate_Merge_AcceptanceReadError_ObserveOnly(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runRow := seedAcceptanceMergeRun(t, repo, au, run.StageStateSucceeded, acceptanceVerdictPassed)
	au.listByCategoryErr = errors.New("audit boom")

	merger := &fakeMerger{}
	out, err := s.AutoDriveRunGate(context.Background(), runRow, campaignOperatorIdentity(), merger, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if out.Acted || merger.called != 0 {
		t.Errorf("outcome=%+v merger.called=%d, want observe-only + 0 merges on a read error (fail-closed)", out, merger.called)
	}
}

// --- (e) must_page_human reviewer_reject -> NO action, page -----------------

func TestAutoDriveRunGate_Page_ReviewerReject(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, stages := startAutoDriveRun(t, s, repo)
	plan, impl := stages[0], stages[1]
	plan.State = run.StageStateSucceeded
	impl.State = run.StageStateAwaitingApproval

	// Gating implement review with a reject verdict.
	seedReviewEntry(t, au, runID, 1, "implement_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	seedReviewEntry(t, au, runID, 2, "implement_reviewed", planreview.ImplementReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
	seedReviewEntry(t, au, runID, 3, "implement_reviewed", planreview.ImplementReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictReject})

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if out.Acted || !out.Paged {
		t.Fatalf("outcome = %+v, want paged + not acted", out)
	}
	if impl.State != run.StageStateAwaitingApproval {
		t.Errorf("implement stage = %q, want unchanged awaiting_approval (no action)", impl.State)
	}
	if countAudit(au, "approval_submitted")+countAudit(au, CategoryStageFixupTriggered) != 0 {
		t.Error("a gate action was taken on a must_page_human reject")
	}
	e := auditEntry(t, au, CategoryCampaignGatePaged)
	assertOperatorActor(t, e)
}

// TestGatingImplementRejectPresent_ReadErrorPages is concern #1445's low-
// severity defense-in-depth fix: an audit-read failure on the implement-review
// categories (while the upstream Evaluate succeeded) makes the page detector
// return true — fail TOWARD paging — matching activePageEvent's documented
// "when in doubt the actor pages" contract. Before the fix it returned false
// (silent not-paging), the opposite of the stated intent.
func TestGatingImplementRejectPresent_ReadErrorPages(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, _ := startAutoDriveRun(t, s, repo)
	runRow := getRun(t, repo, runID)

	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	wf := parsed.Workflows[runRow.WorkflowID]

	// The implement stage has agent-only reviewers (gating authority), so the
	// detector reaches the audit read rather than short-circuiting on
	// authority. Injecting a read error must now page, not silently pass.
	au.listByCategoryErr = errors.New("audit read boom")
	if !s.gatingImplementRejectPresent(context.Background(), runRow, &wf) {
		t.Error("gatingImplementRejectPresent = false on an audit read error; want true (fail-toward-paging)")
	}
}

// --- (f) must_page_human requirement_arbitration -> NO action, page ---------

func TestAutoDriveRunGate_Page_RequirementArbitration(t *testing.T) {
	s, repo, au, cr := newAutoDriveServer(t)
	runID, stages := startAutoDriveRun(t, s, repo)
	plan, impl := stages[0], stages[1]
	plan.State = run.StageStateSucceeded
	impl.State = run.StageStateAwaitingApproval

	// A complete implement round with concerns would otherwise let
	// may_route_fixup fire; the requirement-category open concern pages
	// instead — must_page wins over the delegated knob.
	seedReviewEntry(t, au, runID, 1, "implement_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	seedReviewEntry(t, au, runID, 2, "implement_reviewed", planreview.ImplementReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApproveWithConcerns})
	seedReviewEntry(t, au, runID, 3, "implement_reviewed", planreview.ImplementReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApproveWithConcerns})
	seedOpenConcern(t, cr, runID, impl.ID, concern.StageKindImplement, "high", requirementConcernCategory, "the requirement itself is disputed")

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if out.Acted || !out.Paged || out.PageEvent != "requirement_arbitration" {
		t.Fatalf("outcome = %+v, want paged requirement_arbitration", out)
	}
	if countAudit(au, CategoryStageFixupTriggered) != 0 {
		t.Error("auto-routed a fix-up on a requirement_arbitration gate")
	}
	auditEntry(t, au, CategoryCampaignGatePaged)
}

// --- (g) fail-closed observe-only modes -------------------------------------

// knob unmet: clean_dual_approval with no reviewer verdicts -> observe-only.
func TestAutoDriveRunGate_FailClosed_KnobUnmet(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, stages := startAutoDriveRun(t, s, repo)
	plan := stages[0] // awaiting_approval, but no verdicts seeded

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if out.Acted || out.Paged {
		t.Fatalf("outcome = %+v, want observe-only", out)
	}
	if plan.State != run.StageStateAwaitingApproval {
		t.Errorf("plan stage = %q, want unchanged", plan.State)
	}
	if countAudit(au, "approval_submitted") != 0 {
		t.Error("approval written despite an unmet condition")
	}
}

// evaluation error: an injected audit read failure -> observe-only.
func TestAutoDriveRunGate_FailClosed_EvalError(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, stages := startAutoDriveRun(t, s, repo)
	plan := stages[0]
	au.listByCategoryErr = errBoom

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate returned error, want fail-closed observe-only: %v", err)
	}
	if out.Acted || out.Paged {
		t.Fatalf("outcome = %+v, want observe-only on evaluation error", out)
	}
	if plan.State != run.StageStateAwaitingApproval {
		t.Errorf("plan stage = %q, want unchanged", plan.State)
	}
}

// no operator_agent block configured -> observe-only.
func TestAutoDriveRunGate_FailClosed_NotConfigured(t *testing.T) {
	s, repo, _, _ := newAutoDriveServer(t)
	runID, _ := startDriveE2ERun(t, s, repo.driveE2ERepo, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "cli", "workflow_spec": gatedSpecYAML, // no operator_agent block
	})

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if out.Acted || out.Paged {
		t.Fatalf("outcome = %+v, want observe-only (nothing delegated)", out)
	}
}

// unmapped knob: may_waive(solo_low) met -> conservative no-op observe-only.
func TestAutoDriveRunGate_FailClosed_WaiveUnmapped(t *testing.T) {
	s, repo, au, cr := newAutoDriveServer(t)
	runID, stages := startAutoDriveRun(t, s, repo)
	stages[0].State = run.StageStateSucceeded
	stages[1].State = run.StageStateSucceeded
	// Exactly one low-severity open concern -> solo_low met; no other knob is.
	seedOpenConcern(t, cr, runID, stages[1].ID, concern.StageKindImplement, string(planreview.SeverityLow), "style", "minor nit")

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if out.Acted || out.Paged {
		t.Fatalf("outcome = %+v, want observe-only (auto-waive unmapped)", out)
	}
	if countAudit(au, "concern_waived") != 0 {
		t.Error("a concern was auto-waived; auto-waive is out of scope")
	}
}

// merge seam unconfigured: may_merge met but merger nil -> observe-only.
func TestAutoDriveRunGate_FailClosed_MergeSeamUnconfigured(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, stages := startAutoDriveRun(t, s, repo)
	stages[0].State = run.StageStateSucceeded
	stages[1].State = run.StageStateSucceeded
	if _, err := repo.TransitionRun(context.Background(), runID, run.StateRunning); err != nil {
		t.Fatalf("TransitionRun -> running: %v", err)
	}
	if _, err := repo.SetRunPullRequestURL(context.Background(), runID, "https://github.com/x/y/pull/9"); err != nil {
		t.Fatalf("SetRunPullRequestURL: %v", err)
	}
	seedReviewEntry(t, au, runID, 5, drive.Category, drive.Advance{Rule: drive.RuleChecksGreenAwaitingMerge})

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if out.Acted || out.Paged {
		t.Fatalf("outcome = %+v, want observe-only (no merge client)", out)
	}
	if countAudit(au, CategoryPRMerged) != 0 {
		t.Error("a merge was settled without a configured merge client")
	}
}

// dispatch error: the merge client errors -> the error is surfaced, not
// swallowed, and no settle runs.
func TestAutoDriveRunGate_MergeDispatchError(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, stages := startAutoDriveRun(t, s, repo)
	stages[0].State = run.StageStateSucceeded
	stages[1].State = run.StageStateSucceeded
	if _, err := repo.TransitionRun(context.Background(), runID, run.StateRunning); err != nil {
		t.Fatalf("TransitionRun -> running: %v", err)
	}
	if _, err := repo.SetRunPullRequestURL(context.Background(), runID, "https://github.com/x/y/pull/11"); err != nil {
		t.Fatalf("SetRunPullRequestURL: %v", err)
	}
	seedReviewEntry(t, au, runID, 5, drive.Category, drive.Advance{Rule: drive.RuleChecksGreenAwaitingMerge})

	merger := &fakeMerger{err: errBoom}
	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), merger, nil)
	if err == nil {
		t.Fatalf("AutoDriveRunGate err = nil, want the merge dispatch error; outcome=%+v", out)
	}
	if out.Acted {
		t.Errorf("outcome acted = true on a failed merge; want false")
	}
	if countAudit(au, CategoryPRMerged) != 0 {
		t.Error("post-merge settle ran despite a merge failure")
	}
}

// --- (h) campaign-level operator_agent override (E25.12 / #1451) -------------

// seedRequirementArbitrationState parks the implement stage at its approval
// gate with a COMPLETE review round (no gating reject) and one open
// requirement-category concern. Against the default autoDriveSpecYAML this is
// exactly TestAutoDriveRunGate_Page_RequirementArbitration's state: the workflow
// block PAGES (requirement_arbitration ∈ must_page_human) instead of routing the
// fix-up. The campaign-override tests reuse this single state and vary ONLY the
// campaign block, so the change in outcome is attributable to the override
// alone — the wholesale-override contract at the auto-driver.
func seedRequirementArbitrationState(t *testing.T, s *Server, repo *autoDriveRepo, au *auditFake, cr *fakeConcernRepo) uuid.UUID {
	t.Helper()
	runID, stages := startAutoDriveRun(t, s, repo)
	plan, impl := stages[0], stages[1]
	plan.State = run.StageStateSucceeded
	impl.State = run.StageStateAwaitingApproval
	seedReviewEntry(t, au, runID, 1, "implement_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	seedReviewEntry(t, au, runID, 2, "implement_reviewed", planreview.ImplementReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApproveWithConcerns})
	seedReviewEntry(t, au, runID, 3, "implement_reviewed", planreview.ImplementReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApproveWithConcerns})
	seedOpenConcern(t, cr, runID, impl.ID, concern.StageKindImplement, "high", requirementConcernCategory, "the requirement itself is disputed")
	return runID
}

// A RELAXING campaign override auto-acts where the workflow would PAGE: the
// override delegates may_route_fixup but does NOT list requirement_arbitration
// as a must_page_human event, so the open requirement concern is routed back as
// a fix-up instead of paging the human — the campaign block governs wholesale,
// never merged with the workflow block's must_page_human.
func TestAutoDriveRunGate_CampaignOverride_RelaxingAutoActs(t *testing.T) {
	s, repo, au, cr := newAutoDriveServer(t)
	runID := seedRequirementArbitrationState(t, s, repo, au, cr)

	override := []byte(`{"may_route_fixup":"convergent_concerns"}`)
	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, override)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if !out.Acted || out.Action != delegation.ActionRouteFixup {
		t.Fatalf("outcome = %+v, want acted route_fixup (relaxing override auto-acts where the workflow would page)", out)
	}
	if countAudit(au, CategoryCampaignGatePaged) != 0 {
		t.Error("paged despite a campaign override that does not list requirement_arbitration")
	}
	e := auditEntry(t, au, CategoryStageFixupTriggered)
	assertOperatorActor(t, e)
}

// A TIGHTENING campaign override pages where the workflow would AUTO-ACT: on the
// SAME state as the relaxing case (where the override would otherwise auto-route
// the fix-up), adding requirement_arbitration to the override's must_page_human
// makes the actor refuse and page — proving the campaign block's must_page_human
// replaces, and is honoured over, the per-workflow contract wholesale.
func TestAutoDriveRunGate_CampaignOverride_TighteningPages(t *testing.T) {
	s, repo, au, cr := newAutoDriveServer(t)
	runID := seedRequirementArbitrationState(t, s, repo, au, cr)

	override := []byte(`{"may_route_fixup":"convergent_concerns","must_page_human":["requirement_arbitration"]}`)
	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, override)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if out.Acted || !out.Paged || out.PageEvent != "requirement_arbitration" {
		t.Fatalf("outcome = %+v, want paged requirement_arbitration (tightening override pages where the workflow would auto-act)", out)
	}
	if countAudit(au, CategoryStageFixupTriggered) != 0 {
		t.Error("auto-routed a fix-up despite a tightening campaign override that pages")
	}
	auditEntry(t, au, CategoryCampaignGatePaged)
}

// Malformed campaign override bytes fail CLOSED to observe-only: on a state the
// workflow contract would auto-approve, an unparseable override makes the actor
// take NO action rather than auto-acting through a contract it cannot trust.
func TestAutoDriveRunGate_CampaignOverride_Malformed_FailClosed(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, stages := startAutoDriveRun(t, s, repo)
	plan := stages[0]
	// Clean dual approval: the workflow contract WOULD auto-approve here.
	seedReviewEntry(t, au, runID, 1, "plan_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	seedReviewEntry(t, au, runID, 2, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
	seedReviewEntry(t, au, runID, 3, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, []byte("{not json"))
	if err != nil {
		t.Fatalf("AutoDriveRunGate returned error, want fail-closed observe-only: %v", err)
	}
	if out.Acted || out.Paged {
		t.Fatalf("outcome = %+v, want observe-only on a malformed campaign override", out)
	}
	if plan.State != run.StageStateAwaitingApproval {
		t.Errorf("plan stage = %q, want unchanged (no action on a malformed override)", plan.State)
	}
	if countAudit(au, "approval_submitted") != 0 {
		t.Error("auto-approved through a malformed campaign override")
	}
}

// Empty (zero-length) campaign override bytes fall through to the workflow
// contract — byte-identical to no override: the clean-dual-approval state
// auto-approves exactly as TestAutoDriveRunGate_Approve does.
func TestAutoDriveRunGate_CampaignOverride_Empty_FallsThrough(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, stages := startAutoDriveRun(t, s, repo)
	plan := stages[0]
	seedReviewEntry(t, au, runID, 1, "plan_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	seedReviewEntry(t, au, runID, 2, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
	seedReviewEntry(t, au, runID, 3, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, []byte{})
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if !out.Acted || out.Action != delegation.ActionApprove {
		t.Fatalf("outcome = %+v, want acted approve (empty override falls through to the workflow contract)", out)
	}
	if plan.State != run.StageStateSucceeded {
		t.Errorf("plan stage = %q, want succeeded (auto-advanced via the workflow contract)", plan.State)
	}
}

// --- double-gate state derivations ------------------------------------------

func TestMergeGateReady(t *testing.T) {
	url := "https://github.com/x/y/pull/1"
	withPR := &run.Run{PullRequestURL: &url}
	noPR := &run.Run{}
	succeeded := []*run.Stage{{State: run.StageStateSucceeded}}
	gated := []*run.Stage{{State: run.StageStateAwaitingApproval}}

	if !mergeGateReady(withPR, succeeded) {
		t.Error("mergeGateReady = false for {PR open, no gate}, want true")
	}
	if mergeGateReady(noPR, succeeded) {
		t.Error("mergeGateReady = true with no PR, want false")
	}
	if mergeGateReady(withPR, gated) {
		t.Error("mergeGateReady = true with a stage awaiting approval, want false")
	}
}

func TestRetryableFailedStage(t *testing.T) {
	catA := run.FailureA
	catB := run.FailureB
	reason := "boom"
	a := &run.Stage{Sequence: 1, State: run.StageStateFailed, FailureCategory: &catA, FailureReason: &reason}
	b := &run.Stage{Sequence: 2, State: run.StageStateFailed, FailureCategory: &catB, FailureReason: &reason}

	if got := retryableFailedStage([]*run.Stage{a}); got != a {
		t.Errorf("retryableFailedStage = %v, want the category-A stage", got)
	}
	if got := retryableFailedStage([]*run.Stage{b}); got != nil {
		t.Errorf("retryableFailedStage = %v for a category-B failure, want nil", got)
	}
	if got := retryableFailedStage([]*run.Stage{{State: run.StageStateSucceeded}}); got != nil {
		t.Errorf("retryableFailedStage = %v with no failed stage, want nil", got)
	}
}

func TestGatedReviewStage(t *testing.T) {
	plan := &run.Stage{Sequence: 0, Type: run.StageTypePlan, State: run.StageStateAwaitingApproval}
	impl := &run.Stage{Sequence: 1, Type: run.StageTypeImplement, State: run.StageStateAwaitingApproval}
	if got := gatedReviewStage([]*run.Stage{impl, plan}); got != plan {
		t.Errorf("gatedReviewStage = %v, want the lowest-sequence (plan) gate", got)
	}
	if got := gatedReviewStage([]*run.Stage{{Type: run.StageTypePlan, State: run.StageStateSucceeded}}); got != nil {
		t.Errorf("gatedReviewStage = %v with no open gate, want nil", got)
	}
}

func TestFixupEligibleState(t *testing.T) {
	await := &run.Stage{Type: run.StageTypeImplement, State: run.StageStateAwaitingApproval}
	if !fixupEligibleState(await, nil) {
		t.Error("awaiting_approval implement should be fixup-eligible")
	}
	succeeded := &run.Stage{Type: run.StageTypeImplement, State: run.StageStateSucceeded}
	openReview := []*run.Stage{{Type: run.StageTypeReview, State: run.StageStateAwaitingApproval}}
	if !fixupEligibleState(succeeded, openReview) {
		t.Error("succeeded implement with an open review stage should be fixup-eligible")
	}
	if fixupEligibleState(succeeded, nil) {
		t.Error("succeeded implement with no open review stage should NOT be fixup-eligible")
	}
	pending := &run.Stage{Type: run.StageTypeImplement, State: run.StageStatePending}
	if fixupEligibleState(pending, nil) {
		t.Error("pending implement should NOT be fixup-eligible")
	}
}

var errBoom = errors.New("boom")
