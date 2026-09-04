package server

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
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

// --- #2381: delegated-approval may_approve pre-check + no-progress -----------

// TestAutoDriveRunGate_ApprovalsBlockGate_HumanQuorumRequired is FIX 1's F1: a
// delegated approve on a gate carrying an approvals block (a human count a
// delegated submission never satisfies) DECLINES with
// decision_required:human_quorum_required and records NOTHING — no approval row,
// no advance — rather than the pre-#2381 uncounted-vote-and-hold.
func TestAutoDriveRunGate_ApprovalsBlockGate_HumanQuorumRequired(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, stages := startAutoDriveRunWithSpec(t, s, repo, autoDriveEscalationSpecYAML(""))
	plan := stages[0]
	seedCleanPlanApproval(t, au, runID)

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if out.Acted {
		t.Fatalf("outcome = %+v, want NO action on a human-quorum gate", out)
	}
	if !out.DecisionRequired || out.DecisionState != operatorrole.DecisionStateHumanQuorumRequired || out.Action != delegation.ActionApprove {
		t.Fatalf("outcome = %+v, want decision_required human_quorum_required approve", out)
	}
	if n := countAudit(au, "approval_submitted"); n != 0 {
		t.Errorf("approval_submitted entries = %d, want 0 (the decline records no vote)", n)
	}
	if plan.State != run.StageStateAwaitingApproval {
		t.Errorf("plan stage = %q, want still awaiting_approval", plan.State)
	}
}

// TestAutoDriveRunGate_Approve_DuplicateIsNoProgress is FIX 2's server leg (F6):
// on a LEGACY no-approvals gate (which a delegated approve DOES advance), a
// SECOND delegated approve that comes back a duplicate reports
// decision_required:delegated_approval_no_progress rather than a no-op acted:true
// — so the endpoint appends no attribution row for the no-op.
func TestAutoDriveRunGate_Approve_DuplicateIsNoProgress(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, stages := startAutoDriveRun(t, s, repo) // autoDriveSpecYAML: legacy approvers gate
	plan := stages[0]
	seedReviewEntry(t, au, runID, 1, "plan_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	seedReviewEntry(t, au, runID, 2, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
	seedReviewEntry(t, au, runID, 3, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})

	// Pre-seed the approval row the campaign actor would insert, so this pass's
	// Submit returns Inserted=false (a duplicate).
	if _, err := s.cfg.ApprovalRepo.Submit(context.Background(), approval.SubmitParams{
		StageID: plan.ID, ApproverSubject: operatorrole.CampaignActorSubject,
		Decision: approval.DecisionApprove, Surface: approval.SurfaceAPI,
	}); err != nil {
		t.Fatalf("seed prior approval: %v", err)
	}

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if out.Acted {
		t.Fatalf("outcome = %+v, want NO acted:true on a duplicate (no-progress)", out)
	}
	if !out.DecisionRequired || out.DecisionState != operatorrole.DecisionStateDelegatedApprovalNoProgress {
		t.Fatalf("outcome = %+v, want decision_required delegated_approval_no_progress", out)
	}
}

// TestAutoDriveRunGate_Approve_CampaignSubjectNotRemapped is the campaign-identity
// control (#2381): a delegated approve by the campaign auto-driver
// (CampaignActorSubject, already agent-kind) is recorded under its OWN subject —
// NOT remapped to the operator-agent delegated identity — and carries NO
// on_behalf_of key, so the campaign attribution stays byte-identical to today.
func TestAutoDriveRunGate_Approve_CampaignSubjectNotRemapped(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, _ := startAutoDriveRun(t, s, repo) // legacy gate -> the approve advances
	seedReviewEntry(t, au, runID, 1, "plan_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	seedReviewEntry(t, au, runID, 2, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
	seedReviewEntry(t, au, runID, 3, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if !out.Acted || out.Action != delegation.ActionApprove {
		t.Fatalf("outcome = %+v, want acted approve (legacy gate)", out)
	}
	e := auditEntry(t, au, "approval_submitted")
	if e.ActorSubject == nil || *e.ActorSubject != operatorrole.CampaignActorSubject {
		t.Errorf("actor_subject = %v, want %q (NOT remapped)", e.ActorSubject, operatorrole.CampaignActorSubject)
	}
	var payload map[string]any
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if _, present := payload["on_behalf_of"]; present {
		t.Errorf("on_behalf_of present on a campaign (non-remapped) approval: %v", payload)
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

// TestAutoDriveRunGate_RouteFixup_PRBodyInstruction_AppendsAdvisoryAudit is the
// #2782 every-routing-path pin for the in-process campaign auto-driver: routing
// a PR-body-naming concern through autoFixup (NOT the HTTP handler) still writes
// the advisory fixup_pr_body_unsatisfiable audit entry inside fixupStageAs. This
// guards the documented every-path guarantee against a future refactor that
// moves recordFixupPRBodyUnsatisfiable out of fixupStageAs onto the HTTP handler
// — which would silently break this non-HTTP path with nothing going red.
//
// Counterfactual: move the recordFixupPRBodyUnsatisfiable call out of
// fixupStageAs (e.g. into handleFixupStage) and this goes RED — the auto-drive
// path then appends zero fixup_pr_body_unsatisfiable entries.
func TestAutoDriveRunGate_RouteFixup_PRBodyInstruction_AppendsAdvisoryAudit(t *testing.T) {
	s, repo, au, cr := newAutoDriveServer(t)
	runID, stages := startAutoDriveRun(t, s, repo)
	plan, impl := stages[0], stages[1]
	plan.State = run.StageStateSucceeded
	impl.State = run.StageStateAwaitingApproval

	seedReviewEntry(t, au, runID, 1, "implement_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	seedReviewEntry(t, au, runID, 2, "implement_reviewed", planreview.ImplementReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApproveWithConcerns})
	seedReviewEntry(t, au, runID, 3, "implement_reviewed", planreview.ImplementReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApproveWithConcerns})
	// A high-severity open concern (so convergent_concerns fires) whose note names
	// the PR body — the surface a fix-up pass cannot write.
	const prBodyNote = "Record the per-deletion counterfactual results in the PR body's ## Notes."
	seedOpenConcern(t, cr, runID, impl.ID, concern.StageKindImplement, "high", "correctness", prBodyNote)

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if !out.Acted || out.Action != delegation.ActionRouteFixup {
		t.Fatalf("outcome = %+v, want acted route_fixup", out)
	}
	// The advisory entry landed on the non-HTTP routing path.
	e := auditEntry(t, au, CategoryFixupPRBodyUnsatisfiable)
	var payload fixupPRBodyUnsatisfiablePayload
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		t.Fatalf("unmarshal fixup_pr_body_unsatisfiable payload: %v", err)
	}
	if payload.StageID != impl.ID.String() || payload.ObligationCount != 1 || len(payload.Obligations) != 1 {
		t.Fatalf("payload = %+v, want the implement stage id and one obligation", payload)
	}
	if payload.Obligations[0].ID != "ob-1" || payload.Obligations[0].Source != "concern" || payload.Obligations[0].TextExcerpt != prBodyNote {
		t.Errorf("obligation = %+v, want ob-1/concern with the PR-body excerpt", payload.Obligations[0])
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

// TestAutoFixup_CrashRefundAdmitsPass: the auto-driver's route_fixup arm must
// agree with the HTTP handler on the NEW #3085 category-A refund. One prior pass
// that died category-A having pushed nothing is refunded against the normal
// budget, so the delegated arm ACTS rather than parking the operator with
// decision_required fixup_budget_exhausted.
func TestAutoFixup_CrashRefundAdmitsPass(t *testing.T) {
	s, repo, au, cr := newAutoDriveServer(t)
	runID, impl := seedRouteFixupReady(t, s, repo, au, cr)
	seedFixupPass(t, au, runID, impl.ID, 1) // trigger at sequence 1000
	seedFixupRecoveredC(au, runID, impl.ID, run.FailureA, 1002)

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if !out.Acted || out.Action != delegation.ActionRouteFixup {
		t.Fatalf("outcome = %+v, want acted route_fixup (the category-A death refunds the pass)", out)
	}
	e := auditEntry(t, au, CategoryStageFixupTriggered)
	var payload map[string]any
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	if payload["refunded_passes"].(float64) != 1 {
		t.Errorf("refunded_passes = %v, want 1", payload["refunded_passes"])
	}
}

// TestAutoFixup_InfraRefundAdmitsPass exists SPECIFICALLY because an
// implementation could satisfy the category-A test above while still omitting
// category C: before #3085 the auto-driver counted ONLY the #967 no-change
// refund (countFixupNoChangeRefunds), so it already diverged from the HTTP
// handler by missing the #1957 category-C refund entirely. Routing it through
// the shared chokepoint fixes that pre-existing gap; this test is what detects a
// regression of it.
func TestAutoFixup_InfraRefundAdmitsPass(t *testing.T) {
	s, repo, au, cr := newAutoDriveServer(t)
	runID, impl := seedRouteFixupReady(t, s, repo, au, cr)
	seedFixupPass(t, au, runID, impl.ID, 1) // trigger at sequence 1000
	seedFixupRecoveredC(au, runID, impl.ID, run.FailureC, 1002)

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if !out.Acted || out.Action != delegation.ActionRouteFixup {
		t.Fatalf("outcome = %+v, want acted route_fixup (the category-C death refunds the pass)", out)
	}
	e := auditEntry(t, au, CategoryStageFixupTriggered)
	var payload map[string]any
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	if payload["refunded_passes"].(float64) != 1 {
		t.Errorf("refunded_passes = %v, want 1", payload["refunded_passes"])
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

// --- escalation autonomy ceiling at the AUTO-DRIVE gate (E53.4 / #2227) ------

// autoDriveEscalationSpecYAML declares merge and approve as EXPLICIT
// `mode: auto` overrides — the strongest form of delegation the grammar can
// express — with the escalations block supplied by the caller. The predicate
// matches on `trigger: [diff]`, which every v0 trigger source maps to, so the
// escalation fires without needing a plan artifact or an issue-label snapshot.
func autoDriveEscalationSpecYAML(escalations string) string {
	return `version: "2"
workflows:
  feature_change:
    autonomy: high
    actions:
      merge:
        mode: auto
        when: gates_resolved_ci_green
      approve:
        mode: auto
        when: clean_dual_approval
` + escalations + `
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
        produces:
          - artifact: pull_request
`
}

const autoDriveLowCeilingEscalation = `    escalations:
      - match:
          trigger: [diff]
        require:
          max_autonomy: low
`

// TestAutoDriveRunGate_EscalationCeiling_NoAutoMerge is operator condition A's
// site 4 — the construction site the earlier design left unwired, and the only
// one that acts with NO operator in the loop. An explicitly-declared
// `actions: {merge: {mode: auto}}` under a fired `max_autonomy: low` must
// produce NO auto-merge.
//
// The PAIRED CONTROL (the same run WITHOUT the escalation auto-merges) is what
// proves this test would have failed before the wiring, rather than passing
// because the harness never reached the merge arm.
func TestAutoDriveRunGate_EscalationCeiling_NoAutoMerge(t *testing.T) {
	setup := func(t *testing.T, specYAML string) (*Server, *autoDriveRepo, *fakeMerger, uuid.UUID) {
		t.Helper()
		s, repo, au, _ := newAutoDriveServer(t)
		runID, stages := startAutoDriveRunWithSpec(t, s, repo, specYAML)
		stages[0].State = run.StageStateSucceeded
		stages[1].State = run.StageStateSucceeded
		if _, err := repo.TransitionRun(context.Background(), runID, run.StateRunning); err != nil {
			t.Fatalf("TransitionRun -> running: %v", err)
		}
		if _, err := repo.SetRunPullRequestURL(context.Background(), runID, "https://github.com/x/y/pull/7"); err != nil {
			t.Fatalf("SetRunPullRequestURL: %v", err)
		}
		seedReviewEntry(t, au, runID, 5, drive.Category, drive.Advance{Rule: drive.RuleChecksGreenAwaitingMerge})
		return s, repo, &fakeMerger{}, runID
	}

	t.Run("control: no escalation declared, the explicit auto merge acts", func(t *testing.T) {
		s, repo, merger, runID := setup(t, autoDriveEscalationSpecYAML(""))
		out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), merger, nil)
		if err != nil {
			t.Fatalf("AutoDriveRunGate: %v", err)
		}
		if !out.Acted || out.Action != delegation.ActionMerge {
			t.Fatalf("outcome = %+v, want acted merge — without this control the ceiling assertion below proves nothing", out)
		}
		if merger.called != 1 {
			t.Errorf("merger called %d times, want 1", merger.called)
		}
	})

	t.Run("a fired low ceiling produces NO auto-merge", func(t *testing.T) {
		s, repo, merger, runID := setup(t, autoDriveEscalationSpecYAML(autoDriveLowCeilingEscalation))
		out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), merger, nil)
		if err != nil {
			t.Fatalf("AutoDriveRunGate: %v", err)
		}
		if out.Acted {
			t.Fatalf("outcome = %+v, want NO action — the auto-drive gate acted through a fired max_autonomy: low ceiling", out)
		}
		if merger.called != 0 {
			t.Errorf("merger called %d times, want 0", merger.called)
		}
	})
}

// TestAutoDriveRunGate_EscalationCeiling_NoAutoApprove is the same control at
// the APPROVE arm: an explicitly-declared auto approve under a fired
// `max_autonomy: low` never advances the plan gate.
func TestAutoDriveRunGate_EscalationCeiling_NoAutoApprove(t *testing.T) {
	setup := func(t *testing.T, specYAML string) (*Server, *autoDriveRepo, *auditFake, uuid.UUID) {
		t.Helper()
		s, repo, au, _ := newAutoDriveServer(t)
		runID, _ := startAutoDriveRunWithSpec(t, s, repo, specYAML)
		seedReviewEntry(t, au, runID, 1, "plan_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
		seedReviewEntry(t, au, runID, 2, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
		seedReviewEntry(t, au, runID, 3, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
		return s, repo, au, runID
	}

	t.Run("control: no escalation declared, the auto approve arm is reached", func(t *testing.T) {
		s, repo, au, runID := setup(t, autoDriveEscalationSpecYAML(""))
		out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
		if err != nil {
			t.Fatalf("AutoDriveRunGate: %v", err)
		}
		// #2381 changed this arm's outcome: the plan gate carries an approvals
		// block, so a delegated approve can never advance it (#1709
		// recorded-but-never-counted) — the may_approve arm now DECLINES with
		// decision_required:human_quorum_required instead of recording an uncounted
		// vote. What this control still establishes for the #2227 ceiling assertion
		// below is that the approve knob is MET / the arm is REACHED WITHOUT the
		// escalation — distinct from the clamped observe-only the ceiling subtest
		// asserts. The escalation's effect (clamp the knob so the arm is never
		// entered) is therefore still demonstrable, and no vote is recorded either
		// way, so the ceiling assertion is not vacuous.
		if !out.DecisionRequired || out.DecisionState != operatorrole.DecisionStateHumanQuorumRequired || out.Action != delegation.ActionApprove {
			t.Fatalf("outcome = %+v, want decision_required human_quorum_required approve", out)
		}
		if countAudit(au, "approval_submitted") != 0 {
			t.Errorf("approval_submitted entries = %d, want 0 (the #2381 decline records no vote)", countAudit(au, "approval_submitted"))
		}
	})

	t.Run("a fired low ceiling produces NO auto-approve", func(t *testing.T) {
		s, repo, au, runID := setup(t, autoDriveEscalationSpecYAML(autoDriveLowCeilingEscalation))
		out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
		if err != nil {
			t.Fatalf("AutoDriveRunGate: %v", err)
		}
		if out.Acted {
			t.Fatalf("outcome = %+v, want NO action under a fired max_autonomy: low ceiling", out)
		}
		// The escalation CLAMPS the knob so the may_approve arm is never entered —
		// observe-only, NOT the human_quorum_required DECLINE the control reaches.
		// Asserting !DecisionRequired keeps the #2227 clamp discrimination sharp now
		// that both outcomes record 0 approvals (#2381): the escalation changes the
		// observable outcome from a reached-arm decline to an unreached observe-only.
		if out.DecisionRequired {
			t.Fatalf("outcome = %+v, want observe-only (the ceiling clamps the knob), not a decision_required decline", out)
		}
		if n := countAudit(au, "approval_submitted"); n != 0 {
			t.Errorf("approval_submitted entries = %d, want 0 — an approval was recorded through the ceiling", n)
		}
	})
}

// TestAutoDriveRunGate_Merge_AcceptanceArbitrated_Merges pins the deliberate
// widening named in the plan's risk list (E66.37 / #2474): admitting
// acceptance_arbitrated in dispatchAcceptanceGatedMerge means a DELEGATED
// auto-drive merge can also land on an arbitrated gate. That is intended — the
// arbitration itself is operator-only and run-bound-token-forbidden, so the
// operator decision remains the gating act — and pinning it here makes the
// choice explicit rather than incidental.
func TestAutoDriveRunGate_Merge_AcceptanceArbitrated_Merges(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runRow := seedAcceptanceMergeRun(t, repo, au, run.StageStateSucceeded, acceptanceVerdictFailed)
	seedArbitration(au, runRow.ID, 7, 6)

	merger := &fakeMerger{}
	out, err := s.AutoDriveRunGate(context.Background(), runRow, campaignOperatorIdentity(), merger, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if !out.Acted || out.Action != delegation.ActionMerge {
		t.Fatalf("outcome = %+v, want acted merge on an arbitrated acceptance failure", out)
	}
	if merger.called != 1 {
		t.Errorf("merger called %d times, want 1 — an arbitrated acceptance must not block the delegated merge", merger.called)
	}
}

// TestAutoDriveRunGate_Merge_AcceptanceArbitrationSuperseded_ObserveOnly pins the
// other half: once an acceptance re-run records a NEWER verdict the prior
// arbitration no longer names it, so the delegated merge falls back to
// observe-only. The delegated path must not land a merge on a stale discharge.
func TestAutoDriveRunGate_Merge_AcceptanceArbitrationSuperseded_ObserveOnly(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runRow := seedAcceptanceMergeRun(t, repo, au, run.StageStateSucceeded, acceptanceVerdictFailed)
	seedArbitration(au, runRow.ID, 7, 6)
	seedAcceptanceOutcome(au, runRow.ID, 50, acceptanceVerdictFailed) // re-run

	merger := &fakeMerger{}
	out, err := s.AutoDriveRunGate(context.Background(), runRow, campaignOperatorIdentity(), merger, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if out.Acted || merger.called != 0 {
		t.Errorf("outcome=%+v merger.called=%d, want observe-only + 0 merges (superseded arbitration)", out, merger.called)
	}
}

// TestAutoDriveRunGate_Merge_AcceptanceUndecidable_Merges is the #2512 pin on
// the DELEGATED merge path (the operator endpoint has its own in
// merge_run_test.go, the drive presentation its own in server_test.go). The
// three sites are asserted separately on purpose: each is a distinct consumer
// of the acceptance gate, and a state admitted by two of them and refused by
// the third is the half-wired failure #2512's scope floor names. That they now
// share one predicate is what makes this cheap to keep true — it is not a
// reason to assert it once.
//
// Unlike acceptance_arbitrated, this widening needs NO operator act to clear:
// an undecidable row is not a defect, so there is nothing to arbitrate. The
// distinguishing signal is the state string and the next_actions reason, which
// ask the operator to acknowledge the undecided criterion in their merge
// verdict.
func TestAutoDriveRunGate_Merge_AcceptanceUndecidable_Merges(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runRow := seedAcceptanceMergeRun(t, repo, au, run.StageStateSucceeded, acceptanceVerdictUndecidable)

	merger := &fakeMerger{}
	out, err := s.AutoDriveRunGate(context.Background(), runRow, campaignOperatorIdentity(), merger, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if !out.Acted || out.Action != delegation.ActionMerge {
		t.Fatalf("outcome = %+v, want acted merge on an undecidable acceptance", out)
	}
	if merger.called != 1 {
		t.Errorf("merger called %d times, want 1 — an undecidable acceptance must not block the delegated merge (#2512)", merger.called)
	}
}

// --- report-mode per-class re-occurrence discriminators (E52.15 / #2337) -----

// seedStageRetry appends n retry audit entries of the given category keyed to
// the stage, so stageRetryCount reads back n prior retries — the durable
// record retry.go's writeRetryAudit / writeOverrideRetryAudit writes. The
// entries carry the stage id (stageRetryCount filters on StageID) but need no
// payload.
func seedStageRetry(t *testing.T, au *auditFake, runID, stageID uuid.UUID, category string, n int) {
	t.Helper()
	rid := runID
	sid := stageID
	for i := 0; i < n; i++ {
		au.seeded = append(au.seeded, &audit.Entry{
			RunID: &rid, StageID: &sid, Sequence: int64(2000 + i),
			Category: category, Payload: []byte("{}"),
			Timestamp: time.Now().UTC(),
		})
	}
}

// startRetryReportRun sets up a run declaring `retry: {mode: report}` (bare, so
// the delegated retry knob is empty and the evaluator does not read the review
// surface), with the implement stage FAILED on a retryable failure so the retry
// report gate is live. Returns the run id and the failed implement stage.
func startRetryReportRun(t *testing.T, s *Server, repo *autoDriveRepo) (uuid.UUID, *run.Stage) {
	t.Helper()
	runID, stages := startAutoDriveRunWithSpec(t, s, repo, v2ReportSpecYAML(`    actions:
      retry:
        mode: report`))
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
	return runID, impl
}

// TestAutoDrive_ReportMode_RetryResurfacesOnSecondFailure is the issue's
// done-means for the retry class: a stage that fails, is retried and fails
// AGAIN must re-surface the retry proposal. The first failure reports at the
// base key; a re-poll of the SAME occurrence adds nothing (the within-occurrence
// flooding guard); then the durable stage_retried record a real retry writes is
// seeded (count 1) while the stage is failed again — a genuinely distinct
// occurrence — and a SECOND act:report row is emitted. Exactly two rows.
func TestAutoDrive_ReportMode_RetryResurfacesOnSecondFailure(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, impl := startRetryReportRun(t, s, repo)

	// First failure occurrence: retry count 0 -> base key.
	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate pass 1: %v", err)
	}
	if !out.Reported || out.Action != delegation.ActionRetry {
		t.Fatalf("pass 1 outcome = %+v, want reported retry", out)
	}
	if n := countReportRows(t, au); n != 1 {
		t.Fatalf("act:report rows after pass 1 = %d, want 1", n)
	}
	// Re-poll the SAME occurrence: the flooding guard holds (no second row).
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

	// The stage was retried and failed AGAIN: the durable stage_retried record
	// persists (count 1) and the stage is failed once more. A DISTINCT occurrence.
	seedStageRetry(t, au, runID, impl.ID, CategoryStageRetried, 1)
	out, err = s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate pass 2: %v", err)
	}
	if !out.Reported {
		t.Fatalf("pass 2 outcome = %+v, want the proposal re-surfaced on the second failure", out)
	}
	if n := countReportRows(t, au); n != 2 {
		t.Errorf("act:report rows after second failure = %d, want 2 (one per occurrence; acts = %v)", n, autoDrivenActs(t, au))
	}
}

// TestAutoDrive_ReportMode_RetryResurfacesOnOverrideRetry is the same shape
// seeding stage_override_retried: an operator override re-dispatch re-opens the
// same stage, and its next failure is a genuinely-distinct occurrence that must
// re-surface.
func TestAutoDrive_ReportMode_RetryResurfacesOnOverrideRetry(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, impl := startRetryReportRun(t, s, repo)

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

	seedStageRetry(t, au, runID, impl.ID, CategoryStageOverrideRetried, 1)
	out, err = s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate pass 2: %v", err)
	}
	if !out.Reported {
		t.Fatalf("pass 2 outcome = %+v, want the proposal re-surfaced after an override retry", out)
	}
	if n := countReportRows(t, au); n != 2 {
		t.Errorf("act:report rows after override retry = %d, want 2 (acts = %v)", n, autoDrivenActs(t, au))
	}
}

// TestAutoDrive_ReportMode_RetrySiblingStageDoesNotAdvanceKey pins the
// stage-ID filter in stageRetryCount. ListForRunByCategory is run-scoped, so an
// UNFILTERED count would let a SIBLING stage's retry advance this stage's key
// and emit a second row inside one occurrence — the flooding direction. A
// stage_retried entry keyed to a DIFFERENT stage leaves the retry count 0, the
// key at the base, and emits no second row.
func TestAutoDrive_ReportMode_RetrySiblingStageDoesNotAdvanceKey(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, _ := startRetryReportRun(t, s, repo)

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

	// A retry of an UNRELATED sibling stage must not advance this stage's key.
	seedStageRetry(t, au, runID, uuid.New(), CategoryStageRetried, 1)
	out, err = s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate re-poll: %v", err)
	}
	if out.Reported {
		t.Fatalf("re-poll outcome = %+v, want no second report from a sibling stage's retry", out)
	}
	if n := countReportRows(t, au); n != 1 {
		t.Errorf("act:report rows after a sibling retry = %d, want still 1 (acts = %v)", n, autoDrivenActs(t, au))
	}
}

// TestAutoDrive_ReportMode_RetryCountAdvancesOnlyAfterStageLeavesFailed is the
// ordering-coupling guard (binding condition 1, as corrected by the routed
// reason). It supersedes the earlier RetriedWhileFailedDoesNotDoubleEmit, whose
// premise was wrong: that test seeded the retry receipt (count=1) BEFORE the
// first poll, so every poll keyed on :retry:1 and it only re-tested ordinary
// same-key dedup — it never drove the base -> :retry:1 transition. And the
// "a stage_retried row present WHILE the stage is failed must not double-emit"
// property it claimed is not true of the report code and must not be made true:
// a genuine count advance is exactly what re-surfaces a distinct occurrence
// (TestAutoDrive_ReportMode_RetryResurfacesOnSecondFailure).
//
// What IS true, and is what this pins, is the ORDERING that makes the count
// advance safe. retryStageAs calls run.RetryStage FIRST (retry.go:491-492),
// which flips the stage OUT of `failed`, and only THEN writes the stage_retried
// receipt (retry.go:509-518 — "Audit first" is about ordering relative to the
// orchestrator handoff, not the state flip). So the (failed, count>=1) state is
// unreachable: any poll that reads count=1 also sees a stage that has left
// `failed`, retryableFailedStage returns nil, and the retry gate is CLOSED. The
// count reaching 1 is therefore never a within-occurrence event — it is the
// signature of a SECOND failure (the stage came back to `failed`), which
// re-surfaces correctly and is covered separately.
//
// This test drives that real ordering: the base-key row is emitted while the
// stage is failed (count=0), a re-poll of that same occurrence adds nothing,
// then the retry is applied in retry.go's ORDER — the stage leaves `failed`
// FIRST, then the count-1 receipt lands — and the poll at that instant emits NO
// second row because the gate is closed. It is load-bearing: were the report
// path to surface a retry proposal without the liveness gate (or were retry.go
// reordered to write the receipt before the state flip), the count=1 poll would
// key on :retry:1 while the base occurrence is still open and emit a
// within-occurrence second row — the flood this issue closes.
func TestAutoDrive_ReportMode_RetryCountAdvancesOnlyAfterStageLeavesFailed(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, impl := startRetryReportRun(t, s, repo)

	// First failure occurrence, retry count 0 -> base key.
	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate pass 1: %v", err)
	}
	if !out.Reported || out.Action != delegation.ActionRetry {
		t.Fatalf("pass 1 outcome = %+v, want reported retry at the base key", out)
	}
	if n := countReportRows(t, au); n != 1 {
		t.Fatalf("act:report rows after pass 1 = %d, want 1", n)
	}
	// Re-poll the SAME failed occurrence (count still 0): the within-occurrence
	// flooding guard holds.
	out, err = s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate re-poll: %v", err)
	}
	if out.Reported {
		t.Fatalf("re-poll outcome = %+v, want no second row on the same occurrence", out)
	}
	if n := countReportRows(t, au); n != 1 {
		t.Fatalf("act:report rows after re-poll = %d, want still 1", n)
	}

	// Apply the retry the way retry.go does: run.RetryStage flips the stage out
	// of `failed` (to pending, retry.go:274) FIRST, and only THEN is the
	// stage_retried receipt written. Reproduce exactly that order — the stage
	// leaves `failed`, then the count-1 receipt lands.
	impl.State = run.StageStatePending
	seedStageRetry(t, au, runID, impl.ID, CategoryStageRetried, 1)

	// At the instant count=1 exists, the stage is no longer `failed`: the retry
	// gate is closed, so the advancing count produces NO within-occurrence
	// second row. (A genuine SECOND failure — the stage returning to `failed`
	// with count=1 — is a distinct occurrence and re-surfaces; see
	// TestAutoDrive_ReportMode_RetryResurfacesOnSecondFailure.)
	out, err = s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate post-retry: %v", err)
	}
	if out.Reported {
		t.Fatalf("post-retry outcome = %+v, want no report: the count advanced only after the stage left `failed`, closing the gate", out)
	}
	if n := countReportRows(t, au); n != 1 {
		t.Errorf("act:report rows after the retry transition = %d, want exactly 1 — the retry count advanced only after the gate closed (acts = %v)", n, autoDrivenActs(t, au))
	}
}

// startMergeReportRun sets up a run declaring `merge: {mode: report}` (bare)
// with both stages succeeded and a PR open, so the merge report gate is ready.
func startMergeReportRun(t *testing.T, s *Server, repo *autoDriveRepo) (uuid.UUID, []*run.Stage) {
	t.Helper()
	runID, stages := startAutoDriveRunWithSpec(t, s, repo, v2ReportSpecYAML(`    actions:
      merge:
        mode: report`))
	stages[0].State = run.StageStateSucceeded
	stages[1].State = run.StageStateSucceeded
	if _, err := repo.TransitionRun(context.Background(), runID, run.StateRunning); err != nil {
		t.Fatalf("TransitionRun -> running: %v", err)
	}
	if _, err := repo.SetRunPullRequestURL(context.Background(), runID, "https://github.com/x/y/pull/7"); err != nil {
		t.Fatalf("SetRunPullRequestURL: %v", err)
	}
	return runID, stages
}

// TestAutoDrive_ReportMode_MergeResurfacesOnReReadyGate is the issue's
// done-means for the merge class: a merge gate that becomes ready, un-becomes
// ready and re-becomes ready must re-surface the proposal. The first ready
// window reports; a re-poll adds nothing (the within-occurrence flooding
// guard); un-readying the gate reports nothing; then an approval_submitted row
// (the ready-transition proxy) plus a return to ready is a DISTINCT occurrence
// and emits a second row. Exactly two rows.
func TestAutoDrive_ReportMode_MergeResurfacesOnReReadyGate(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, stages := startMergeReportRun(t, s, repo)

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate pass 1: %v", err)
	}
	if !out.Reported || out.Action != delegation.ActionMerge {
		t.Fatalf("pass 1 outcome = %+v, want reported merge", out)
	}
	if n := countReportRows(t, au); n != 1 {
		t.Fatalf("act:report rows after pass 1 = %d, want 1", n)
	}
	// Re-poll the SAME ready window: the flooding guard holds (no second row).
	out, err = s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate re-poll: %v", err)
	}
	if out.Reported {
		t.Fatalf("re-poll outcome = %+v, want no second report inside one ready window", out)
	}
	if n := countReportRows(t, au); n != 1 {
		t.Fatalf("act:report rows after re-poll = %d, want still 1", n)
	}

	// The gate un-readies (a stage returns to awaiting_approval): no report.
	stages[0].State = run.StageStateAwaitingApproval
	out, err = s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate un-ready: %v", err)
	}
	if out.Reported {
		t.Fatalf("un-ready outcome = %+v, want no report while the merge gate is not ready", out)
	}
	if n := countReportRows(t, au); n != 1 {
		t.Fatalf("act:report rows while un-ready = %d, want still 1", n)
	}

	// An approval lands (the ready-transition proxy advances) and the gate
	// re-becomes ready: a DISTINCT occurrence, the proposal re-surfaces.
	seedReviewEntry(t, au, runID, 6, "approval_submitted", map[string]any{"decision": "approve"})
	stages[0].State = run.StageStateSucceeded
	out, err = s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate pass 2: %v", err)
	}
	if !out.Reported {
		t.Fatalf("pass 2 outcome = %+v, want the proposal re-surfaced on the re-ready gate", out)
	}
	if n := countReportRows(t, au); n != 2 {
		t.Errorf("act:report rows after re-ready = %d, want 2 (acts = %v)", n, autoDrivenActs(t, au))
	}
}

// reportCategoryErrAudit fails ListForRunByCategory for a specific SET of
// categories, passing every other category through to the embedded auditFake.
// Unlike reportDedupeErrAudit (which fails only run_auto_driven), this lets a
// test fail a discriminator's own read (a *_review_started / stage_retried /
// approval_submitted category) while leaving the dedupe read intact — so the
// discriminator's read-failure degrade branch is exercised, not an
// evaluation/dedupe failure upstream of it.
type reportCategoryErrAudit struct {
	*auditFake
	failCategories map[string]bool
	err            error
}

func (a *reportCategoryErrAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if a.failCategories[category] {
		return nil, a.err
	}
	return a.auditFake.ListForRunByCategory(ctx, runID, category)
}

// TestReviewRoundCount_DegradeBranches is a direct table over reviewRoundCount
// asserting ok=false on EACH of its four named degrade branches, so every one
// falls back to the base key (under-emission) rather than a flood.
func TestReviewRoundCount_DegradeBranches(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, _ := startAutoDriveRunWithSpec(t, s, repo, v2ReportSpecYAML(`    actions:
      approve:
        mode: report`))
	plan := mkAutoDriveStage(0, run.StageTypePlan, run.StageStateAwaitingApproval)

	t.Run("nil anchor", func(t *testing.T) {
		if n, ok := s.reviewRoundCount(context.Background(), runID, nil); ok || n != 0 {
			t.Errorf("reviewRoundCount(nil) = (%d, %v), want (0, false)", n, ok)
		}
	})
	t.Run("stage type with no review surface", func(t *testing.T) {
		review := mkAutoDriveStage(2, run.StageTypeReview, run.StageStateAwaitingApproval)
		if n, ok := s.reviewRoundCount(context.Background(), runID, review); ok || n != 0 {
			t.Errorf("reviewRoundCount(review stage) = (%d, %v), want (0, false)", n, ok)
		}
	})
	t.Run("zero review-started entries", func(t *testing.T) {
		if n, ok := s.reviewRoundCount(context.Background(), runID, plan); ok || n != 0 {
			t.Errorf("reviewRoundCount(no rounds) = (%d, %v), want (0, false)", n, ok)
		}
	})
	t.Run("audit read error", func(t *testing.T) {
		seedReviewEntry(t, au, runID, 1, "plan_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
		s.cfg.AuditRepo = &reportCategoryErrAudit{
			auditFake:      au,
			failCategories: map[string]bool{"plan_review_started": true, "implement_review_started": true},
			err:            errors.New("boom"),
		}
		if n, ok := s.reviewRoundCount(context.Background(), runID, plan); ok || n != 0 {
			t.Errorf("reviewRoundCount(read error) = (%d, %v), want (0, false)", n, ok)
		}
	})
}

// TestAutoDrive_ReportMode_ReviewRoundReadFailureFallsBackToBaseKey is the
// behavioural counterpart: with the review-started read failing FROM THE START,
// the approve report gate reports once at the BASE key, and a fresh review
// round seeded afterward does NOT re-surface the proposal — the discriminator
// degrades to the base key and under-emits (the documented safe direction),
// asserted on committed audit rows read back after each call. Because the
// approve entry is BARE (no `when`), the delegated approve knob is empty and the
// evaluator does not read the review surface, so this failure is the
// discriminator's own degrade branch and not an evaluation failure upstream.
//
// Counterfactual vehicle: make the fake's review-started read SUCCEED and the
// count advances across the fresh round, changing the key and emitting a second
// row — the test then fails on the 2-rows assertion.
func TestAutoDrive_ReportMode_ReviewRoundReadFailureFallsBackToBaseKey(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, _ := startAutoDriveRunWithSpec(t, s, repo, v2ReportSpecYAML(`    actions:
      approve:
        mode: report`))
	seedCleanPlanApproval(t, au, runID)
	s.cfg.AuditRepo = &reportCategoryErrAudit{
		auditFake:      au,
		failCategories: map[string]bool{"plan_review_started": true, "implement_review_started": true},
		err:            errors.New("boom"),
	}

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate pass 1: %v", err)
	}
	if !out.Reported {
		t.Fatalf("pass 1 outcome = %+v, want reported at the base key", out)
	}
	if n := countReportRows(t, au); n != 1 {
		t.Fatalf("act:report rows after pass 1 = %d, want 1", n)
	}

	// A fresh review round arrives, but the read still fails: the key stays at
	// the base and the proposal does NOT re-surface (under-emission).
	seedReviewEntry(t, au, runID, 4, "plan_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	out, err = s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate re-poll: %v", err)
	}
	if out.Reported {
		t.Fatalf("re-poll outcome = %+v, want no re-surface while the review read fails", out)
	}
	if n := countReportRows(t, au); n != 1 {
		t.Errorf("act:report rows after a fresh round with the read failing = %d, want still 1 (under-emission; acts = %v)", n, autoDrivenActs(t, au))
	}
}

// TestAutoDrive_ReportMode_RetryCountReadFailureFallsBackToBaseKey pins
// stageRetryCount's read-error degrade: with the stage_retried read failing
// from the start, the retry report gate reports once at the base key, and a
// seeded stage_retried record does NOT re-surface the proposal (under-emission).
// Counterfactual vehicle: make the fake's stage_retried read succeed and the
// count advances across the seeded retry, emitting a second row.
func TestAutoDrive_ReportMode_RetryCountReadFailureFallsBackToBaseKey(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, impl := startRetryReportRun(t, s, repo)
	s.cfg.AuditRepo = &reportCategoryErrAudit{
		auditFake:      au,
		failCategories: map[string]bool{CategoryStageRetried: true, CategoryStageOverrideRetried: true},
		err:            errors.New("boom"),
	}

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate pass 1: %v", err)
	}
	if !out.Reported {
		t.Fatalf("pass 1 outcome = %+v, want reported at the base key", out)
	}
	if n := countReportRows(t, au); n != 1 {
		t.Fatalf("act:report rows after pass 1 = %d, want 1", n)
	}

	seedStageRetry(t, au, runID, impl.ID, CategoryStageRetried, 1)
	out, err = s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate re-poll: %v", err)
	}
	if out.Reported {
		t.Fatalf("re-poll outcome = %+v, want no re-surface while the retry-count read fails", out)
	}
	if n := countReportRows(t, au); n != 1 {
		t.Errorf("act:report rows with the retry read failing = %d, want still 1 (under-emission; acts = %v)", n, autoDrivenActs(t, au))
	}
}

// TestAutoDrive_ReportMode_MergeCountReadFailureFallsBackToBaseKey pins
// mergeReadyRoundCount's read-error degrade: with the approval_submitted read
// failing from the start, the merge report gate reports once at the base key,
// and a re-ready cycle carrying a seeded approval does NOT re-surface the
// proposal (under-emission). Counterfactual vehicle: make the fake's
// approval_submitted read succeed and the count advances across the re-ready,
// emitting a second row.
func TestAutoDrive_ReportMode_MergeCountReadFailureFallsBackToBaseKey(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, stages := startMergeReportRun(t, s, repo)
	s.cfg.AuditRepo = &reportCategoryErrAudit{
		auditFake:      au,
		failCategories: map[string]bool{"approval_submitted": true},
		err:            errors.New("boom"),
	}

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate pass 1: %v", err)
	}
	if !out.Reported {
		t.Fatalf("pass 1 outcome = %+v, want reported at the base key", out)
	}
	if n := countReportRows(t, au); n != 1 {
		t.Fatalf("act:report rows after pass 1 = %d, want 1", n)
	}

	// Un-ready then re-ready the gate with an approval seeded: the read still
	// fails, so the key stays at the base and the proposal does not re-surface.
	stages[0].State = run.StageStateAwaitingApproval
	if _, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil); err != nil {
		t.Fatalf("AutoDriveRunGate un-ready: %v", err)
	}
	seedReviewEntry(t, au, runID, 6, "approval_submitted", map[string]any{"decision": "approve"})
	stages[0].State = run.StageStateSucceeded
	out, err = s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate re-ready: %v", err)
	}
	if out.Reported {
		t.Fatalf("re-ready outcome = %+v, want no re-surface while the approval read fails", out)
	}
	if n := countReportRows(t, au); n != 1 {
		t.Errorf("act:report rows with the approval read failing = %d, want still 1 (under-emission; acts = %v)", n, autoDrivenActs(t, au))
	}
}

// --- #3116: the delegated path must OBSERVE, not 500 ------------------------

// appendStageRow appends a stage row to the run BY CONSTRUCTION — the state is
// written directly rather than reached through the predicate under test, so a
// counterfactual RED lands on the behavioral assertion, not on fixture setup.
func appendStageRow(repo *autoDriveRepo, runID uuid.UUID, typ run.StageType, state run.StageState) *run.Stage {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	st := &run.Stage{ID: uuid.New(), RunID: runID, Type: typ, State: state, Sequence: len(repo.stagesByRun[runID]) + 1}
	repo.stagesByRun[runID] = append(repo.stagesByRun[runID], st)
	return st
}

// TestFixupEligibleState_PendingReviewNotEligible pins the #3116 predicate
// tightening: run.findOpenReviewStage admits a review stage at awaiting_approval
// ALONE, so a PENDING review stage must NOT read as fix-up-eligible here — the
// pending half could only ever produce ErrFixupNotApplicable, which
// handleAutoDrive maps to a 500.
func TestFixupEligibleState_PendingReviewNotEligible(t *testing.T) {
	succeeded := &run.Stage{Type: run.StageTypeImplement, State: run.StageStateSucceeded}
	pendingReview := []*run.Stage{{Type: run.StageTypeReview, State: run.StageStatePending}}
	if fixupEligibleState(succeeded, pendingReview) {
		t.Error("succeeded implement with a PENDING review stage must NOT be fixup-eligible: run.FixupStage refuses that state with ErrFixupNotApplicable (#3116)")
	}
	// The open-gate shape stays eligible — the tightening must not hide a legal
	// route-back.
	openReview := []*run.Stage{{Type: run.StageTypeReview, State: run.StageStateAwaitingApproval}}
	if !fixupEligibleState(succeeded, openReview) {
		t.Error("succeeded implement with a review stage at awaiting_approval must stay fixup-eligible")
	}
}

// TestAutoFixup_PendingReviewObservesInsteadOfFailing is the behavioral half:
// with the implement stage succeeded and the review stage still PENDING (the
// normal window in a workflow that orders acceptance before review), autoFixup
// must return dispatched=false with a NIL error — the observe-only outcome that
// keeps fishhawk_drive_run polling — rather than the raw ErrFixupNotApplicable
// that handleAutoDrive maps to a 500 auto_drive_dispatch_failed. The err==nil
// identity assertion is the whole point: a non-nil sentinel here IS the defect.
func TestAutoFixup_PendingReviewObservesInsteadOfFailing(t *testing.T) {
	s, repo, au, cr := newAutoDriveServer(t)
	runID, impl := seedRouteFixupReady(t, s, repo, au, cr)
	// By construction: implement succeeded (PR open), review stage parked at
	// pending because acceptance has not settled.
	impl.State = run.StageStateSucceeded
	appendStageRow(repo, runID, run.StageTypeAcceptance, run.StageStateAwaitingHostDispatch)
	appendStageRow(repo, runID, run.StageTypeReview, run.StageStatePending)

	stages, err := repo.ListStagesForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListStagesForRun: %v", err)
	}
	open, err := cr.ListOpenByRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListOpenByRun: %v", err)
	}
	if len(open) == 0 {
		t.Fatal("fixture seeded no open concern; the arm under test would be unreachable")
	}

	dispatched, err := s.autoFixup(context.Background(), campaignOperatorIdentity(), getRun(t, repo, runID), stages, open, "convergent_concerns")
	// Sentinel identity is checked FIRST so the branch is live rather than
	// unreachable behind the generic non-nil report: an ErrFixupNotApplicable
	// escaping autoFixup is the specific defect (handleAutoDrive maps it to a
	// 500 auto_drive_dispatch_failed), and it earns its own message.
	if errors.Is(err, run.ErrFixupNotApplicable) {
		t.Fatalf("autoFixup returned the raw run.ErrFixupNotApplicable sentinel: %v — handleAutoDrive maps it to a 500 auto_drive_dispatch_failed; want nil (observe-only)", err)
	}
	if err != nil {
		t.Fatalf("autoFixup err = %v, want nil (observe-only)", err)
	}
	if dispatched {
		t.Error("dispatched = true, want false — the endpoint refuses a fix-up while the review stage is pending")
	}
	if n := countAudit(au, CategoryStageFixupTriggered); n != 0 {
		t.Errorf("%d %s entries appended, want 0 (nothing was routed)", n, CategoryStageFixupTriggered)
	}
}

// --- E64.42 / #3159: pre-merge audit-check republish (delegated arm) --------
//
// dispatchAcceptanceGatedMerge is the SHARED merge-dispatch tail the delegated
// may_merge arm routes through. handleMergeRun does NOT route through it (it
// calls GateMerger.MergePullRequest directly and carries its own copy of the
// control, pinned in merge_run_test.go), which is why both sites need the call
// and both need their own counterfactual.
//
// The three tests below drive the seam DIRECTLY, one per enumerated branch.
// The HTTP route that reaches this seam through the real auto-drive handler is
// pinned separately by TestAutoDrive_Merge_RepublishesAuditCheckBeforeDispatch
// in autodrive_http_test.go.

// newGatedMergeRepublishServer builds a server with the audit stack and the
// Check Run publisher wired, both feeding the SAME ordered call log as the
// merge seam, and returns it with a merge-ready run and its stages.
//
// workflowSpec nil leaves the acceptance gate not-declared (admits the merge);
// specWithAcceptanceStage with no recorded outcome leaves it PENDING (refuses).
func newGatedMergeRepublishServer(t *testing.T, log *mergeOrderLog, workflowSpec []byte) (*Server, *run.Run, []*run.Stage) {
	t.Helper()
	rr := newOrchestratorRepo()
	au := newAuditCompleteAuditFake()
	arts := newFakeArtifactRepo()
	r := seedPublishableRun(t, rr, au, arts, "abc12345")
	prURL := "https://github.com/x/y/pull/7"
	r.PullRequestURL = &prURL
	r.WorkflowID = "feature_change"
	r.WorkflowSpec = workflowSpec

	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr,
		AuditRepo:      au,
		ArtifactRepo:   arts,
		StageCheckRepo: newFakeStageCheckRepo(),
		ExternalURL:    "https://app.fishhawk.example.com",
	})
	s.auditCheckPublisher = auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub:      &orderedPublishGitHub{log: log},
		Runs:        rr,
		Artifacts:   arts,
		Audit:       au,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	stages, err := rr.ListStagesForRun(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("ListStagesForRun: %v", err)
	}
	return s, r, stages
}

// TestDispatchAcceptanceGatedMerge_RepublishesBeforeMerge asserts ORDER on the
// shared log: the fishhawk_audit_complete republish is recorded strictly
// BEFORE merger.MergePullRequest. A publish after the dispatch would satisfy a
// presence-only assertion while healing nothing — the stranded in_progress
// check is exactly what makes the dispatch fail.
func TestDispatchAcceptanceGatedMerge_RepublishesBeforeMerge(t *testing.T) {
	log := &mergeOrderLog{}
	s, r, stages := newGatedMergeRepublishServer(t, log, nil)
	merger := &orderedLogMerger{log: log}

	outcome, gateState, err := s.dispatchAcceptanceGatedMerge(context.Background(), r, stages, merger)
	if err != nil {
		t.Fatalf("dispatchAcceptanceGatedMerge: %v", err)
	}
	if outcome != mergeDispatchMerged {
		t.Fatalf("outcome = %v (gate %q), want mergeDispatchMerged", outcome, gateState)
	}
	if merger.called != 1 {
		t.Fatalf("merge dispatched %d times, want 1", merger.called)
	}
	assertPublishPrecedesMerge(t, log)
}

// TestDispatchAcceptanceGatedMerge_AcceptanceNotReady_NoRepublish pins the
// fail-closed acceptance-gate refusal: the workflow declares an acceptance
// stage and no verdict has been recorded, so the gate reports pending, the
// function returns before any dispatch — and, critically, before any publish.
// The republish sits AFTER this guard on purpose: a genuinely mid-flight run
// recomputes to pending, and refusing it is the acceptance gate's job, not a
// Check Run's.
func TestDispatchAcceptanceGatedMerge_AcceptanceNotReady_NoRepublish(t *testing.T) {
	log := &mergeOrderLog{}
	s, r, stages := newGatedMergeRepublishServer(t, log, specWithAcceptanceStage)
	merger := &orderedLogMerger{log: log}

	outcome, gateState, err := s.dispatchAcceptanceGatedMerge(context.Background(), r, stages, merger)
	if err != nil {
		t.Fatalf("dispatchAcceptanceGatedMerge: %v", err)
	}
	if outcome != mergeDispatchAcceptanceNotReady {
		t.Fatalf("outcome = %v (gate %q), want mergeDispatchAcceptanceNotReady", outcome, gateState)
	}
	if got := log.snapshot(); len(got) != 0 {
		t.Fatalf("acceptance-gate refusal recorded %v, want nothing (no publish, no merge)", got)
	}
}

// TestDispatchAcceptanceGatedMerge_NilMerger_NoRepublish pins the second
// refusal path: the merge seam is unconfigured, so the function fails closed
// BEFORE the republish. A refusal must not pay for a publish.
func TestDispatchAcceptanceGatedMerge_NilMerger_NoRepublish(t *testing.T) {
	log := &mergeOrderLog{}
	s, r, stages := newGatedMergeRepublishServer(t, log, nil)

	outcome, gateState, err := s.dispatchAcceptanceGatedMerge(context.Background(), r, stages, nil)
	if err != nil {
		t.Fatalf("dispatchAcceptanceGatedMerge: %v", err)
	}
	if outcome != mergeDispatchNoMerger {
		t.Fatalf("outcome = %v (gate %q), want mergeDispatchNoMerger", outcome, gateState)
	}
	if got := log.snapshot(); len(got) != 0 {
		t.Fatalf("nil-merger fail-closed recorded %v, want nothing (no publish, no merge)", got)
	}
}
