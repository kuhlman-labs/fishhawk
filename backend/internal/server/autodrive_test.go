package server

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
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
//
// transitionStageErr, when set, forces TransitionStage to return a GENERIC
// (non-sentinel) error — the injection the #2091 route_fixup genuine-dispatch-
// error test uses to prove a non-sentinel autoFixup failure still propagates as
// the raw error (a 500) rather than being classified as a decision_required
// outcome. Default nil leaves every other fixture's transitions untouched.
type autoDriveRepo struct {
	*driveE2ERepo
	transitionStageErr error
}

func (r *autoDriveRepo) TransitionStage(ctx context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	if r.transitionStageErr != nil {
		return nil, r.transitionStageErr
	}
	return r.driveE2ERepo.TransitionStage(ctx, id, to, c)
}

// seedFixupPass appends prior stage_fixup_triggered audit entries keyed to the
// stage so countFixupPasses reads back n consumed passes — the durable fix-up
// budget counter the route_fixup arm's ErrFixupBudgetExhausted /
// ErrFixupCeilingReached sentinels are enforced against (#2091). The seeded
// entries carry the stage id (countFixupPasses filters on StageID) but need no
// payload.
func seedFixupPass(t *testing.T, au *auditFake, runID, stageID uuid.UUID, n int) {
	t.Helper()
	rid := runID
	sid := stageID
	for i := 0; i < n; i++ {
		au.seeded = append(au.seeded, &audit.Entry{
			RunID: &rid, StageID: &sid, Sequence: int64(1000 + i),
			Category: CategoryStageFixupTriggered, Payload: []byte("{}"),
			Timestamp: time.Now().UTC(),
		})
	}
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

// TestAutoDriveRunGate_RouteFixupParksOnAllLow is the #1964 done-means
// cross-layer assertion: a dual-approve implement round whose ONLY open
// concern is low-severity parks at the default medium threshold instead of
// spending a full fix-up pass. Observe-only, and ZERO stage_fixup_triggered
// entries — the no-budget-spent proof, crossing the spec-parse ->
// delegation-evaluate -> autodrive-dispatch seam.
func TestAutoDriveRunGate_RouteFixupParksOnAllLow(t *testing.T) {
	s, repo, au, cr := newAutoDriveServer(t)
	runID, stages := startAutoDriveRun(t, s, repo)
	plan, impl := stages[0], stages[1]
	plan.State = run.StageStateSucceeded
	impl.State = run.StageStateAwaitingApproval

	// Dual approve (no reject) with a single LOW open concern -> below the
	// default medium threshold, so convergent_concerns is UNMET.
	seedReviewEntry(t, au, runID, 1, "implement_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	seedReviewEntry(t, au, runID, 2, "implement_reviewed", planreview.ImplementReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
	seedReviewEntry(t, au, runID, 3, "implement_reviewed", planreview.ImplementReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
	seedOpenConcern(t, cr, runID, impl.ID, concern.StageKindImplement, string(planreview.SeverityLow), "style", "minor nit")

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if out.Acted || out.Paged {
		t.Fatalf("outcome = %+v, want observe-only (all-low round parks)", out)
	}
	if n := countAudit(au, CategoryStageFixupTriggered); n != 0 {
		t.Errorf("%d %s entries, want 0 (no fix-up budget spent on an all-low round)", n, CategoryStageFixupTriggered)
	}
}

// TestAutoDriveRunGate_RouteFixupThresholdLowRoutes proves the knob threads
// from campaign-override JSON through parseCampaignOverride's
// DisallowUnknownFields decode into the evaluator: the SAME all-low state
// that parks under the default auto-routes when the campaign override sets
// route_fixup_min_severity: low.
func TestAutoDriveRunGate_RouteFixupThresholdLowRoutes(t *testing.T) {
	s, repo, au, cr := newAutoDriveServer(t)
	runID, stages := startAutoDriveRun(t, s, repo)
	plan, impl := stages[0], stages[1]
	plan.State = run.StageStateSucceeded
	impl.State = run.StageStateAwaitingApproval

	seedReviewEntry(t, au, runID, 1, "implement_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	seedReviewEntry(t, au, runID, 2, "implement_reviewed", planreview.ImplementReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
	seedReviewEntry(t, au, runID, 3, "implement_reviewed", planreview.ImplementReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
	seedOpenConcern(t, cr, runID, impl.ID, concern.StageKindImplement, string(planreview.SeverityLow), "style", "minor nit")

	override := []byte(`{"may_route_fixup":"convergent_concerns","route_fixup_min_severity":"low"}`)
	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, override)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if !out.Acted || out.Action != delegation.ActionRouteFixup {
		t.Fatalf("outcome = %+v, want acted route_fixup (threshold low routes a low concern)", out)
	}
	e := auditEntry(t, au, CategoryStageFixupTriggered)
	assertOperatorActor(t, e)
	if rule := auditDelegatedRule(t, e); rule != "convergent_concerns" {
		t.Errorf("delegated rule = %q, want convergent_concerns", rule)
	}
}

// --- (b') route_fixup with an exhausted budget/ceiling -> decision_required --

// seedRouteFixupReady puts the run in the exact state TestAutoDriveRunGate_RouteFixup
// drives — implement parked awaiting_approval, a dual approve_with_concerns
// round, and one open medium implement concern so may_route_fixup(convergent_concerns)
// is Met — and returns the implement stage so the caller can seed prior passes.
func seedRouteFixupReady(t *testing.T, s *Server, repo *autoDriveRepo, au *auditFake, cr *fakeConcernRepo) (uuid.UUID, *run.Stage) {
	t.Helper()
	runID, stages := startAutoDriveRun(t, s, repo)
	plan, impl := stages[0], stages[1]
	plan.State = run.StageStateSucceeded
	impl.State = run.StageStateAwaitingApproval
	seedReviewEntry(t, au, runID, 1, "implement_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	seedReviewEntry(t, au, runID, 2, "implement_reviewed", planreview.ImplementReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApproveWithConcerns})
	seedReviewEntry(t, au, runID, 3, "implement_reviewed", planreview.ImplementReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApproveWithConcerns})
	seedOpenConcern(t, cr, runID, impl.ID, concern.StageKindImplement, "medium", "scope", "tighten the seam")
	return runID, impl
}

// TestAutoDriveRunGate_RouteFixupBudgetExhausted is the #2091 headline: when the
// delegated route_fixup arm's dispatch hits ErrFixupBudgetExhausted (the normal
// budget is spent), the actor returns a decision_required outcome (nil error) —
// NOT the raw sentinel that would map to a 500 — so the driver parks the
// operator instead of failing loud. NO fix-up is dispatched (the budget refused
// it), so ZERO NEW stage_fixup_triggered entries land beyond the seeded prior
// pass.
func TestAutoDriveRunGate_RouteFixupBudgetExhausted(t *testing.T) {
	s, repo, au, cr := newAutoDriveServer(t)
	runID, impl := seedRouteFixupReady(t, s, repo, au, cr)
	// One prior pass == the default normal budget (defaultMaxFixupPasses) with
	// no refunds -> FixupStage refuses with ErrFixupBudgetExhausted, and the
	// hard ceiling (3) still has headroom so it is NOT ErrFixupCeilingReached.
	seedFixupPass(t, au, runID, impl.ID, 1)

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if out.Acted || out.Paged {
		t.Fatalf("outcome = %+v, want a non-acted, non-paged decision_required", out)
	}
	if !out.DecisionRequired || out.DecisionState != "fixup_budget_exhausted" {
		t.Fatalf("outcome = %+v, want DecisionRequired with DecisionState=fixup_budget_exhausted", out)
	}
	if out.Action != delegation.ActionRouteFixup {
		t.Errorf("Action = %q, want %q (the arm that surfaced the decision)", out.Action, delegation.ActionRouteFixup)
	}
	// The budget refused the dispatch: no NEW trigger entry beyond the seeded
	// prior pass (seedFixupPass writes to au.seeded, not au.appended).
	if n := countAudit(au, CategoryStageFixupTriggered); n != 0 {
		t.Errorf("%d NEW %s entries appended, want 0 (budget refused the dispatch)", n, CategoryStageFixupTriggered)
	}
}

// TestAutoDriveRunGate_RouteFixupCeilingReached asserts the DISTINCT hard-ceiling
// sentinel maps to DecisionState=fixup_ceiling_reached (nil error) — the state
// the operator override can never push past — rather than being collapsed into
// the budget-exhausted branch.
func TestAutoDriveRunGate_RouteFixupCeilingReached(t *testing.T) {
	s, repo, au, cr := newAutoDriveServer(t)
	runID, impl := seedRouteFixupReady(t, s, repo, au, cr)
	// defaultFixupCeiling (3) prior passes -> FixupStage refuses with
	// ErrFixupCeilingReached BEFORE the budget check.
	seedFixupPass(t, au, runID, impl.ID, 3)

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if !out.DecisionRequired || out.DecisionState != "fixup_ceiling_reached" {
		t.Fatalf("outcome = %+v, want DecisionRequired with DecisionState=fixup_ceiling_reached", out)
	}
	if out.Acted || out.Paged {
		t.Errorf("outcome = %+v, want a non-acted, non-paged decision_required", out)
	}
}

// TestAutoDriveRunGate_RouteFixupGenuineErrorPropagates pins the untouched
// fail-loud path: a NON-sentinel autoFixup failure (here a repo TransitionStage
// error, a genuine dispatch failure) still bubbles as the raw NON-NIL error —
// NOT a decision_required outcome — so the HTTP layer maps it to a 500 exactly
// as before. This is the branch that must NOT be swallowed by the new
// sentinel classification.
func TestAutoDriveRunGate_RouteFixupGenuineErrorPropagates(t *testing.T) {
	s, repo, au, cr := newAutoDriveServer(t)
	runID, _ := seedRouteFixupReady(t, s, repo, au, cr)
	// No prior passes: the budget admits the fix-up, so FixupStage proceeds to
	// the stage transition — which the injected repo error fails. errors.Is on
	// neither sentinel matches, so the arm returns the raw error.
	repo.transitionStageErr = errors.New("stage store down")

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err == nil {
		t.Fatalf("err = nil, want the genuine dispatch error to propagate; outcome = %+v", out)
	}
	if out.DecisionRequired {
		t.Errorf("outcome = %+v, want NO decision_required on a genuine (non-sentinel) dispatch error", out)
	}
	if out.Acted {
		t.Errorf("outcome = %+v, want a non-acted outcome alongside the error", out)
	}
	if out.Action != delegation.ActionRouteFixup {
		t.Errorf("Action = %q, want %q", out.Action, delegation.ActionRouteFixup)
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

// TestAutoDriveRunGate_Merge_AcceptanceNotValidated_Merges pins the #2347 admit
// on the DELEGATED merge path (the operator endpoint has its own pin in
// merge_run_test.go): a short-circuited acceptance stage that verified ZERO
// criteria resolves to acceptanceGateNotValidated, which is merge-eligible, so
// the delegated may_merge reaches the seam exactly like a passed verdict. The
// two sites are asserted separately because each enumerates the admissible
// gate-state set independently — adding the state to one and not the other is
// precisely the drift this pair catches.
func TestAutoDriveRunGate_Merge_AcceptanceNotValidated_Merges(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runRow := seedAcceptanceMergeRun(t, repo, au, run.StageStateSucceeded, acceptanceVerdictNotValidated)

	merger := &fakeMerger{}
	out, err := s.AutoDriveRunGate(context.Background(), runRow, campaignOperatorIdentity(), merger, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if !out.Acted || out.Action != delegation.ActionMerge {
		t.Fatalf("outcome = %+v, want acted merge on a not-validated acceptance", out)
	}
	if merger.called != 1 {
		t.Errorf("merger called %d times, want 1 — a not-validated acceptance must not block the merge (#2347)", merger.called)
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

// --- mode: report (ADR-066 / E52.10 / #2222) --------------------------------

// v2ReportSpecYAML renders a workflow-v2 document whose workflow-level
// autonomy block is the caller's, so each report test declares exactly the
// grammar under test and the DERIVED delegation contract comes from the real
// parse pipeline.
func v2ReportSpecYAML(autonomyBlock string) string {
	return `version: "2"
workflows:
  feature_change:
` + autonomyBlock + `
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          agents:
            - provider: anthropic
            - provider: codex
          human: 1
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvals:
              count: 1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        reviewers:
          agents:
            - provider: anthropic
            - provider: codex
        produces:
          - artifact: pull_request
        gates:
          - type: approval
            approvals:
              count: 1
`
}

// startAutoDriveRunWithSpec is startAutoDriveRun over an explicit spec
// document.
func startAutoDriveRunWithSpec(t *testing.T, s *Server, repo *autoDriveRepo, specYAML string) (uuid.UUID, []*run.Stage) {
	t.Helper()
	runID, _ := startDriveE2ERun(t, s, repo.driveE2ERepo, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "cli", "workflow_spec": specYAML,
	})
	return runID, repo.stagesFor(runID)
}

// seedCleanPlanApproval seeds a settled, clean two-agent plan review round —
// the state `approve` would auto-act on if the class were at mode: auto.
func seedCleanPlanApproval(t *testing.T, au *auditFake, runID uuid.UUID) {
	t.Helper()
	seedReviewEntry(t, au, runID, 1, "plan_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	seedReviewEntry(t, au, runID, 2, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
	seedReviewEntry(t, au, runID, 3, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
}

// autoDrivenActs returns the act values of every appended run_auto_driven
// row, in order.
func autoDrivenActs(t *testing.T, au *auditFake) []string {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []string
	for i := range au.appended {
		if au.appended[i].Category != CategoryRunAutoDriven {
			continue
		}
		var p struct {
			Act string `json:"act"`
		}
		if err := json.Unmarshal(au.appended[i].Payload, &p); err != nil {
			t.Fatalf("unmarshal run_auto_driven payload: %v", err)
		}
		out = append(out, p.Act)
	}
	return out
}

// countReportRows counts appended run_auto_driven rows carrying act:report.
func countReportRows(t *testing.T, au *auditFake) int {
	t.Helper()
	n := 0
	for _, act := range autoDrivenActs(t, au) {
		if act == autoDriveActReport {
			n++
		}
	}
	return n
}

// TestAutoDriveRunGate_ReportMode_SurfacesProposalWithoutActing is the AC4 /
// F10 behavioural test: `approve: {mode: report}` on an otherwise medium-tier
// workflow, at a gate whose clean_dual_approval state WOULD have satisfied an
// auto approve. The actor emits the run_auto_driven act:report attribution
// row, dispatches nothing, pages nothing, and leaves the run parked exactly
// where it was — the driver keeps polling.
func TestAutoDriveRunGate_ReportMode_SurfacesProposalWithoutActing(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, stages := startAutoDriveRunWithSpec(t, s, repo, v2ReportSpecYAML(`    autonomy: medium
    actions:
      approve:
        mode: report`))
	plan := stages[0]
	seedCleanPlanApproval(t, au, runID)

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if !out.Reported || out.Action != delegation.ActionApprove {
		t.Fatalf("outcome = %+v, want reported approve", out)
	}
	if out.Acted || out.Paged || out.DecisionRequired {
		t.Errorf("outcome = %+v; a report acts on nothing and parks nothing", out)
	}
	if plan.State != run.StageStateAwaitingApproval {
		t.Errorf("plan stage = %q, want awaiting_approval (unchanged by a report)", plan.State)
	}
	if n := countAudit(au, "approval_submitted"); n != 0 {
		t.Errorf("approval_submitted rows = %d, want 0", n)
	}
	if n := countAudit(au, CategoryCampaignGatePaged); n != 0 {
		t.Errorf("campaign_gate_paged rows = %d, want 0 (a report is not a page)", n)
	}
	e := auditEntry(t, au, CategoryRunAutoDriven)
	assertOperatorActor(t, e)
	var p struct {
		Act        string `json:"act"`
		Action     string `json:"action"`
		Class      string `json:"class"`
		Occurrence string `json:"occurrence"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("unmarshal report payload: %v", err)
	}
	if p.Act != autoDriveActReport || p.Action != delegation.ActionApprove || p.Class != spec.ActionApprove {
		t.Errorf("report payload = %+v, want act report / action approve / class approve", p)
	}
	if p.Occurrence == "" {
		t.Error("report payload carries no occurrence key; the dedupe has nothing to match on")
	}
}

// TestAutoDrive_ReportMode_EmitsAtMostOncePerGateOccurrence is binding
// approval condition 2: the driver keeps POLLING a live gate, so an
// unguarded report arm would append a row on every pass and flood the audit
// trail. Three passes over the same live gate must leave exactly ONE
// act:report row. A single-invocation test cannot catch this.
func TestAutoDrive_ReportMode_EmitsAtMostOncePerGateOccurrence(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, _ := startAutoDriveRunWithSpec(t, s, repo, v2ReportSpecYAML(`    autonomy: medium
    actions:
      approve:
        mode: report`))
	seedCleanPlanApproval(t, au, runID)

	reported := 0
	for i := 0; i < 3; i++ {
		out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
		if err != nil {
			t.Fatalf("AutoDriveRunGate pass %d: %v", i+1, err)
		}
		if out.Reported {
			reported++
		}
		if out.Acted || out.Paged {
			t.Fatalf("pass %d outcome = %+v; a report gate must not act or page", i+1, out)
		}
	}
	if reported != 1 {
		t.Errorf("reported outcomes = %d over 3 passes, want 1", reported)
	}
	if n := countReportRows(t, au); n != 1 {
		t.Errorf("act:report rows = %d after 3 passes, want exactly 1 (acts = %v)", n, autoDrivenActs(t, au))
	}
}

// TestAutoDrive_ReportMode_ReopenedGateResurfaces is binding condition 2's
// counterpart to the flooding guard: the idempotency key holds ONE row per
// gate OCCURRENCE, not one for the life of the stage. A gate that closes and
// re-opens on a FRESH review round (a fix-up round trip) is a NEW occurrence
// and MUST re-surface the proposal — otherwise an operator relying on report
// mode never sees the proposal for the materially-changed round.
func TestAutoDrive_ReportMode_ReopenedGateResurfaces(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, _ := startAutoDriveRunWithSpec(t, s, repo, v2ReportSpecYAML(`    autonomy: medium
    actions:
      approve:
        mode: report`))
	seedCleanPlanApproval(t, au, runID)

	// First occurrence: the plan gate is live on its first review round.
	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate pass 1: %v", err)
	}
	if !out.Reported {
		t.Fatalf("pass 1 outcome = %+v, want reported", out)
	}
	if n := countReportRows(t, au); n != 1 {
		t.Fatalf("act:report rows after pass 1 = %d, want 1", n)
	}
	// A re-poll of the SAME occurrence adds nothing (the flooding guard).
	out, err = s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate re-poll: %v", err)
	}
	if out.Reported {
		t.Fatalf("re-poll outcome = %+v, want no second report on the same occurrence", out)
	}
	if n := countReportRows(t, au); n != 1 {
		t.Fatalf("act:report rows after re-poll = %d, want still 1", n)
	}

	// The gate closes and RE-OPENS on a fresh review round (a fix-up round
	// trip). The new round is a DISTINCT occurrence: the proposal re-surfaces.
	seedReviewEntry(t, au, runID, 4, "plan_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	out, err = s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate pass 2: %v", err)
	}
	if !out.Reported {
		t.Fatalf("pass 2 outcome = %+v, want the proposal re-surfaced on the new occurrence", out)
	}
	if n := countReportRows(t, au); n != 2 {
		t.Errorf("act:report rows after re-open = %d, want 2 (one per occurrence; acts = %v)", n, autoDrivenActs(t, au))
	}
}

// TestReportMatrixProposals_ConcurrentEmitsOnce is binding condition 2 under
// CONCURRENCY (the POST /auto-drive endpoint and the campaign driver are both
// in-process callers): many concurrent report evaluations of the same live
// gate must still leave exactly ONE act:report row. A read-then-append with no
// serialization lets two callers both observe "no row yet" and both append;
// the dedupe-check -> append section is stripe-locked per run, so the race
// resolves to a single row and a single Reported outcome.
func TestReportMatrixProposals_ConcurrentEmitsOnce(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, stages := startAutoDriveRunWithSpec(t, s, repo, v2ReportSpecYAML(`    autonomy: medium
    actions:
      approve:
        mode: report`))
	seedCleanPlanApproval(t, au, runID)
	runRow := getRun(t, repo, runID)
	res, _, ok := s.evaluateRunDelegation(context.Background(), runRow, nil)
	if !ok || res == nil {
		t.Fatalf("evaluateRunDelegation ok=%v res=%v", ok, res)
	}

	const drivers = 12
	var wg sync.WaitGroup
	reported := make([]bool, drivers)
	wg.Add(drivers)
	for i := 0; i < drivers; i++ {
		go func(i int) {
			defer wg.Done()
			out, _ := s.reportMatrixProposals(context.Background(), runRow, campaignOperatorIdentity(), res, stages, nil)
			reported[i] = out.Reported
		}(i)
	}
	wg.Wait()

	got := 0
	for _, r := range reported {
		if r {
			got++
		}
	}
	if got != 1 {
		t.Errorf("reported outcomes = %d across %d concurrent drivers, want 1", got, drivers)
	}
	if n := countReportRows(t, au); n != 1 {
		t.Errorf("act:report rows = %d after concurrent drivers, want exactly 1 (acts = %v)", n, autoDrivenActs(t, au))
	}
}

// TestAutoDriveRunGate_ReportMode_ExtensionClassNeverReports: an extension
// class has no backend-observable gate, so it can never be live and never
// reports — the same fail-closed-by-construction posture that rejects
// `mode: auto` on it at parse time.
func TestAutoDriveRunGate_ReportMode_ExtensionClassNeverReports(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, _ := startAutoDriveRunWithSpec(t, s, repo, v2ReportSpecYAML(`    actions:
      promote:
        mode: report`))
	seedCleanPlanApproval(t, au, runID)

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if out.Reported {
		t.Errorf("outcome = %+v, want no report for an extension class", out)
	}
	if n := countReportRows(t, au); n != 0 {
		t.Errorf("act:report rows = %d, want 0", n)
	}
}

// TestAutoDriveRunGate_ReportMode_GateNotLive: the firing rule is gate-LIVE,
// so a report class whose gate is not reachable surfaces nothing. Here the
// plan gate has been cleared, leaving no stage awaiting approval.
func TestAutoDriveRunGate_ReportMode_GateNotLive(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, stages := startAutoDriveRunWithSpec(t, s, repo, v2ReportSpecYAML(`    actions:
      approve:
        mode: report`))
	stages[0].State = run.StageStateSucceeded
	seedCleanPlanApproval(t, au, runID)

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if out.Reported {
		t.Errorf("outcome = %+v, want no report with no live gate", out)
	}
	if n := countReportRows(t, au); n != 0 {
		t.Errorf("act:report rows = %d, want 0", n)
	}
}

// TestAutoDriveRunGate_ReportMode_DeclaredConditionUnmet: when a report entry
// DOES declare a `when`, the condition must additionally be met. Here the
// gate is live but no reviewer verdicts landed, so clean_dual_approval is
// unmet and nothing is surfaced.
func TestAutoDriveRunGate_ReportMode_DeclaredConditionUnmet(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, _ := startAutoDriveRunWithSpec(t, s, repo, v2ReportSpecYAML(`    actions:
      approve:
        mode: report
        when: clean_dual_approval`))
	_ = runID

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if out.Reported {
		t.Errorf("outcome = %+v, want no report while the declared condition is unmet", out)
	}
	if n := countReportRows(t, au); n != 0 {
		t.Errorf("act:report rows = %d, want 0", n)
	}
}

// reportDedupeErrAudit fails ONLY the run_auto_driven category read the
// report dedupe performs, leaving the review-round reads the evaluator makes
// intact — so the test exercises the dedupe's read-failure branch and not an
// evaluation failure upstream of it.
type reportDedupeErrAudit struct {
	*auditFake
	err error
}

func (a *reportDedupeErrAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if category == CategoryRunAutoDriven {
		return nil, a.err
	}
	return a.auditFake.ListForRunByCategory(ctx, runID, category)
}

// TestAutoDriveRunGate_ReportMode_DedupeReadErrorSkips: an unreadable audit
// chain makes "have I already reported this occurrence" unanswerable, so the
// actor skips the report rather than risk flooding the trail — fail-closed
// toward silence, since a report grants no authority to begin with.
func TestAutoDriveRunGate_ReportMode_DedupeReadErrorSkips(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, _ := startAutoDriveRunWithSpec(t, s, repo, v2ReportSpecYAML(`    actions:
      approve:
        mode: report`))
	seedCleanPlanApproval(t, au, runID)
	s.cfg.AuditRepo = &reportDedupeErrAudit{auditFake: au, err: errors.New("boom")}

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if out.Reported {
		t.Errorf("outcome = %+v, want no report when the dedupe read failed", out)
	}
	if n := countReportRows(t, au); n != 0 {
		t.Errorf("act:report rows = %d, want 0", n)
	}
}

// TestAutoDriveRunGate_ReportMode_AppendFailureNotReported: a failed append
// must not be reported as if the row had landed — the outcome falls through
// to observe-only, so the driver's next pass retries the proposal.
func TestAutoDriveRunGate_ReportMode_AppendFailureNotReported(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, _ := startAutoDriveRunWithSpec(t, s, repo, v2ReportSpecYAML(`    actions:
      approve:
        mode: report`))
	seedCleanPlanApproval(t, au, runID)
	au.appendErrCategory = CategoryRunAutoDriven

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if out.Reported {
		t.Errorf("outcome = %+v, want no report when the append failed", out)
	}
	if n := countReportRows(t, au); n != 0 {
		t.Errorf("act:report rows = %d, want 0", n)
	}
}

// TestReportGateOccurrence_PerClassLiveness pins the report arm's liveness
// derivation for EVERY class, not just the approve path the end-to-end tests
// drive: each class is live only where the corresponding action could
// actually be taken, the occurrence key identifies THAT gate, and a class
// with no live gate reports nothing.
func TestReportGateOccurrence_PerClassLiveness(t *testing.T) {
	runRow := &run.Run{ID: uuid.New()}
	pr := "https://github.com/x/y/pull/7"
	planGated := mkAutoDriveStage(0, run.StageTypePlan, run.StageStateAwaitingApproval)
	implGated := mkAutoDriveStage(1, run.StageTypeImplement, run.StageStateAwaitingApproval)
	implFailed := mkAutoDriveStage(1, run.StageTypeImplement, run.StageStateFailed)
	catA := run.FailureCategory("A")
	reason := "verify_infra_flake_retry"
	implFailed.FailureCategory = &catA
	implFailed.FailureReason = &reason
	someConcern := []*concern.Concern{{ID: uuid.New(), Severity: "low"}}

	cases := []struct {
		name       string
		action     string
		runRow     *run.Run
		stages     []*run.Stage
		open       []*concern.Concern
		wantLive   bool
		wantAnchor *run.Stage
	}{
		{"approve at a gated review stage", delegation.ActionApprove, runRow, []*run.Stage{planGated}, nil, true, planGated},
		{"approve with no gate", delegation.ActionApprove, runRow, []*run.Stage{implFailed}, nil, false, nil},
		{"fixup with an eligible implement stage and a concern", delegation.ActionRouteFixup, runRow, []*run.Stage{implGated}, someConcern, true, implGated},
		{"fixup with no open concern", delegation.ActionRouteFixup, runRow, []*run.Stage{implGated}, nil, false, nil},
		{"waive at the approval gate with a concern", delegation.ActionWaive, runRow, []*run.Stage{planGated}, someConcern, true, planGated},
		{"waive with no concern", delegation.ActionWaive, runRow, []*run.Stage{planGated}, nil, false, nil},
		{"retry with a retryable failure", delegation.ActionRetry, runRow, []*run.Stage{implFailed}, nil, true, implFailed},
		{"retry with no failed stage", delegation.ActionRetry, runRow, []*run.Stage{planGated}, nil, false, nil},
		{"merge with an open PR and no pending gate", delegation.ActionMerge,
			&run.Run{ID: runRow.ID, PullRequestURL: &pr}, []*run.Stage{mkAutoDriveStage(0, run.StageTypePlan, run.StageStateSucceeded)}, nil, true, nil},
		{"merge with a gate still open", delegation.ActionMerge,
			&run.Run{ID: runRow.ID, PullRequestURL: &pr}, []*run.Stage{planGated}, nil, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			occ, anchor, live := reportGateOccurrence(tc.action, tc.runRow, tc.stages, tc.open)
			if live != tc.wantLive {
				t.Fatalf("live = %v, want %v (occurrence %q)", live, tc.wantLive, occ)
			}
			if !live {
				if occ != "" {
					t.Errorf("occurrence = %q on a non-live gate, want empty", occ)
				}
				if anchor != nil {
					t.Errorf("anchor = %+v on a non-live gate, want nil", anchor)
				}
				return
			}
			if anchor != tc.wantAnchor {
				t.Errorf("anchor = %+v, want %+v", anchor, tc.wantAnchor)
			}
			want := "run:" + tc.runRow.ID.String() + ":merge_ready"
			if tc.wantAnchor != nil {
				want = stageOccurrence(tc.wantAnchor)
			}
			if occ != want {
				t.Errorf("occurrence = %q, want %q", occ, want)
			}
		})
	}
}

// mkAutoDriveStage builds a stage row for the pure liveness table above.
func mkAutoDriveStage(seq int, typ run.StageType, state run.StageState) *run.Stage {
	return &run.Stage{ID: uuid.New(), Sequence: seq, Type: typ, State: state}
}

// TestReportMatrixProposals_NoAuditRepo: with no audit repository there is
// nowhere to record a proposal, so the arm reports nothing rather than
// claiming an unrecorded one.
func TestReportMatrixProposals_NoAuditRepo(t *testing.T) {
	s, repo, _, _ := newAutoDriveServer(t)
	s.cfg.AuditRepo = nil
	res := &delegation.Result{Matrix: []spec.ResolvedAction{{Action: spec.ActionApprove, Mode: spec.ModeReport, Source: spec.SourceExplicit}}}
	stages := []*run.Stage{mkAutoDriveStage(0, run.StageTypePlan, run.StageStateAwaitingApproval)}
	_ = repo

	out, reported := s.reportMatrixProposals(context.Background(), &run.Run{ID: uuid.New()},
		campaignOperatorIdentity(), res, stages, nil)
	if reported || out.Reported {
		t.Errorf("out = %+v reported = %v, want no report with no audit repository", out, reported)
	}
}
