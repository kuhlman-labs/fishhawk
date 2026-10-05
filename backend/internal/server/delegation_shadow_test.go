package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/crewmessage"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/escalation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// delegation_shadow_test.go pins the E82.1 / #3778 delegation shadow stamp:
// who is stamped (humans only), when (a fresh decision only, appended after
// the decision row), what is evaluated (the PRE-decision state, through the
// same condition code checkDelegation uses), and every degrade branch.

// shadowSpecYAML is a workflow-v2 spec at the given autonomy tier, with two
// agent plan reviewers (so clean_dual_approval counts two verdicts) and an
// optional escalations block.
func shadowSpecYAML(tier, escalations string) string {
	return `version: "2"
workflows:
  feature_change:
    autonomy: ` + tier + `
` + escalations + `    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          agents:
            - provider: anthropic
            - provider: codex
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

// newShadowServer wires every store the stamp and the handlers read.
func newShadowServer(t *testing.T) (*Server, *approvalRunRepo, *auditFake, *fakeConcernRepo) {
	t.Helper()
	rr := newApprovalRunRepo()
	au := newAuditFake()
	cr := newFakeConcernRepo()
	s := New(Config{
		Addr:             "127.0.0.1:0",
		ApprovalRepo:     newFakeApprovalRepo(),
		RunRepo:          rr,
		AuditRepo:        au,
		ConcernRepo:      cr,
		IdentityProvider: &fakeIdentityProvider{perm: identity.PermissionAdmin, member: true},
	})
	return s, rr, au, cr
}

// seedShadowRun stamps a run row carrying specYAML onto stage's run id.
func seedShadowRun(rr *approvalRunRepo, runID uuid.UUID, specYAML string) *run.Run {
	row := &run.Run{
		ID:           runID,
		State:        run.StateRunning,
		WorkflowID:   "feature_change",
		WorkflowSHA:  "sha-3778",
		WorkflowSpec: []byte(specYAML),
	}
	rr.seedRun(row)
	return row
}

// seedTwoApproveVerdicts records a settled two-reviewer plan round, both
// approve — clean_dual_approval's met shape when no concern is open.
func seedTwoApproveVerdicts(t *testing.T, au *auditFake, runID uuid.UUID) {
	t.Helper()
	seedReviewEntry(t, au, runID, 1, "plan_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	seedReviewEntry(t, au, runID, 2, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
	seedReviewEntry(t, au, runID, 3, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
}

// shadowStamps decodes every appended stamp, in append order.
func shadowStamps(t *testing.T, au *auditFake) []delegationShadowPayload {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []delegationShadowPayload
	for _, e := range au.appended {
		if e.Category != CategoryDelegationShadowEvaluated {
			continue
		}
		if e.ActorKind == nil || *e.ActorKind != audit.ActorKind("system") {
			t.Errorf("stamp actor_kind = %v, want system", e.ActorKind)
		}
		var p delegationShadowPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("unmarshal stamp: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// oneStamp asserts exactly one stamp was appended and returns it.
func oneStamp(t *testing.T, au *auditFake) delegationShadowPayload {
	t.Helper()
	stamps := shadowStamps(t, au)
	if len(stamps) != 1 {
		t.Fatalf("shadow stamps = %d, want exactly 1: %+v", len(stamps), stamps)
	}
	return stamps[0]
}

// appendIndex returns the append position of the first entry of category,
// or -1.
func appendIndex(au *auditFake, category string) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	for i, e := range au.appended {
		if e.Category == category {
			return i
		}
	}
	return -1
}

// seedPlanGate seeds a plan stage parked at the gate plus its implement
// sibling, on a run carrying specYAML.
func seedPlanGate(rr *approvalRunRepo, specYAML string) *run.Stage {
	stage := rr.seedStage(run.StageStateAwaitingApproval)
	rr.seedSiblingStage(stage.RunID, run.StageTypeImplement, run.ExecutorAgent, run.StageStatePending)
	seedShadowRun(rr, stage.RunID, specYAML)
	return stage
}

// TestDelegationShadow_HumanPlanApproveGatedRecordsOneStamp is AC1: a human
// plan approval on an autonomy:low run whose clean_dual_approval WOULD hold
// appends exactly one stamp — class approve, mode gated, verdict met — AFTER
// the approval_submitted row.
func TestDelegationShadow_HumanPlanApproveGatedRecordsOneStamp(t *testing.T) {
	s, rr, au, _ := newShadowServer(t)
	stage := seedPlanGate(rr, shadowSpecYAML("low", ""))
	seedTwoApproveVerdicts(t, au, stage.RunID)

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	st := oneStamp(t, au)
	if st.Action != delegation.ActionApprove || st.Class != spec.ActionApprove {
		t.Errorf("action/class = %q/%q, want approve/approve", st.Action, st.Class)
	}
	if st.Verdict != string(delegation.ShadowMet) || !st.Met || st.Reason != "" {
		t.Errorf("verdict = %q met=%v reason=%q, want met", st.Verdict, st.Met, st.Reason)
	}
	if st.Mode != string(spec.ModeGated) || st.Anchored {
		t.Errorf("mode = %q anchored=%v, want gated / not anchored", st.Mode, st.Anchored)
	}
	if st.Condition != string(spec.ConditionCleanDualApproval) {
		t.Errorf("condition = %q, want clean_dual_approval", st.Condition)
	}
	if st.HumanDecision != "approve" || st.DecisionCategory != "approval_submitted" {
		t.Errorf("decision = %q/%q, want approve/approval_submitted", st.HumanDecision, st.DecisionCategory)
	}
	if st.ActorSubject != testOperatorIdentity().Subject || st.ActorKind != string(audit.ActorUser) {
		t.Errorf("actor = %q/%q, want the human operator", st.ActorKind, st.ActorSubject)
	}
	if st.ResolvedMatrixHash == "" || !st.MatrixResolved || st.Tier != "low" {
		t.Errorf("matrix stratum = hash %q resolved=%v tier=%q, want a hashed low matrix", st.ResolvedMatrixHash, st.MatrixResolved, st.Tier)
	}
	if st.WorkflowSHA != "sha-3778" || st.StageID != stage.ID.String() || st.ShadowVersion != 1 {
		t.Errorf("workflow_sha=%q stage_id=%q version=%d", st.WorkflowSHA, st.StageID, st.ShadowVersion)
	}
	if dec, stamp := appendIndex(au, "approval_submitted"), appendIndex(au, CategoryDelegationShadowEvaluated); dec < 0 || stamp <= dec {
		t.Errorf("stamp at %d, approval_submitted at %d: the stamp must follow the decision row", stamp, dec)
	}
}

// TestDelegationShadow_HumanRejectRecordsDecision: a reject records the
// human's actual decision on the approve-class stamp.
func TestDelegationShadow_HumanRejectRecordsDecision(t *testing.T) {
	s, rr, au, _ := newShadowServer(t)
	stage := seedPlanGate(rr, shadowSpecYAML("low", ""))
	seedTwoApproveVerdicts(t, au, stage.RunID)

	w := submitApproval(t, s, stage.ID, `{"decision":"reject","comment":"wrong fork"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	st := oneStamp(t, au)
	if st.HumanDecision != "reject" || st.Verdict != string(delegation.ShadowMet) {
		t.Errorf("stamp = %+v, want human_decision reject over a met verdict", st)
	}
}

// TestDelegationShadow_Fixup: a human fix-up stamps route_fixup with the
// selected concern ids.
func TestDelegationShadow_Fixup(t *testing.T) {
	s, rr, au, cr := newShadowServer(t)
	stage := rr.seedGatelessStage(run.StageStateAwaitingApproval)
	seedShadowRun(rr, stage.RunID, shadowSpecYAML("low", ""))
	row := seedConcernRow(t, cr, stage.RunID, stage.ID, concern.StageKindImplement, 2, "tighten the test")

	w := postFixup(t, s, stage.ID, fixupRequest{ConcernIDs: []string{row.ID.String()}, Reason: "route it"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	st := oneStamp(t, au)
	if st.Action != delegation.ActionRouteFixup || st.Class != spec.ActionFixup || st.HumanDecision != "route_fixup" ||
		st.DecisionCategory != CategoryStageFixupTriggered {
		t.Errorf("stamp = %+v, want a route_fixup stamp", st)
	}
	if len(st.ConcernIDs) != 1 || st.ConcernIDs[0] != row.ID.String() {
		t.Errorf("concern_ids = %v, want [%s]", st.ConcernIDs, row.ID)
	}
	if st.Verdict != string(delegation.ShadowUnmet) || !strings.Contains(st.Reason, "no implement review round recorded") {
		t.Errorf("verdict/reason = %q/%q, want unmet on the missing review round", st.Verdict, st.Reason)
	}
	if appendIndex(au, CategoryDelegationShadowEvaluated) <= appendIndex(au, CategoryStageFixupTriggered) {
		t.Error("stamp must follow stage_fixup_triggered")
	}
}

// TestDelegationShadow_EvaluatedBeforeDecision: one open LOW concern, human
// waives it — the stamp is met (solo_low on the pre-decision state); after
// the decision there would be zero open concerns.
func TestDelegationShadow_EvaluatedBeforeDecision(t *testing.T) {
	s, rr, au, cr := newShadowServer(t)
	runID := uuid.New()
	seedShadowRun(rr, runID, shadowSpecYAML("low", ""))
	row := seedLowConcernRow(t, cr, runID, uuid.New())

	w := postWaive(t, s, row.ID.String(), waiveConcernRequest{Reason: "nit"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	st := oneStamp(t, au)
	if st.Verdict != string(delegation.ShadowMet) || st.Condition != string(spec.ConditionSoloLow) {
		t.Errorf("stamp = %+v, want solo_low met on the PRE-decision state", st)
	}
	if st.HumanDecision != "waive" || st.DecisionCategory != CategoryConcernWaived ||
		len(st.ConcernIDs) != 1 || st.ConcernIDs[0] != row.ID.String() || st.StageID != row.StageID.String() {
		t.Errorf("stamp decision fields = %+v", st)
	}
	if appendIndex(au, CategoryDelegationShadowEvaluated) <= appendIndex(au, CategoryConcernWaived) {
		t.Error("stamp must follow concern_waived")
	}
}

// TestDelegationShadow_BulkWaiveOneStamp: one stamp per bulk request,
// carrying the waived set; a batch where nothing landed stamps nothing.
func TestDelegationShadow_BulkWaiveOneStamp(t *testing.T) {
	s, rr, au, cr := newShadowServer(t)
	runID, stageID := uuid.New(), uuid.New()
	seedShadowRun(rr, runID, shadowSpecYAML("low", ""))
	a := seedLowConcernRow(t, cr, runID, stageID)
	b := seedLowConcernRow(t, cr, runID, stageID)

	w := postBulkWaive(t, s, runID.String(), bulkWaiveRequest{ConcernIDs: []string{a.ID.String(), b.ID.String()}, Reason: "nits"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	st := oneStamp(t, au)
	if len(st.ConcernIDs) != 2 || st.Verdict != string(delegation.ShadowUnmet) || !strings.Contains(st.Reason, "2 open concerns") {
		t.Errorf("stamp = %+v, want one unmet stamp over both waived ids", st)
	}

	t.Run("nothing landed stamps nothing", func(t *testing.T) {
		s, rr, au, cr := newShadowServer(t)
		runID := uuid.New()
		seedShadowRun(rr, runID, shadowSpecYAML("low", ""))
		c := seedLowConcernRow(t, cr, runID, uuid.New())
		au.appendErrCategory = CategoryConcernWaived
		w := postBulkWaive(t, s, runID.String(), bulkWaiveRequest{ConcernIDs: []string{c.ID.String()}, Reason: "nit"})
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		if n := len(shadowStamps(t, au)); n != 0 {
			t.Errorf("stamps = %d, want 0 when no waive landed", n)
		}
	})
}

// TestDelegationShadow_Retry: a human retry stamps the retry class; an
// override of a category-B failure names stage_override_retried.
func TestDelegationShadow_Retry(t *testing.T) {
	s, rr, au, _ := newShadowServer(t)
	stage := seedFailedStage(rr, run.FailureA, "agent crashed: SIGSEGV")
	seedShadowRun(rr, stage.RunID, shadowSpecYAML("low", ""))

	w := postRetryBody(t, s, stage.ID, `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	st := oneStamp(t, au)
	if st.Action != delegation.ActionRetry || st.DecisionCategory != CategoryStageRetried || st.Verdict != string(delegation.ShadowUnmet) {
		t.Errorf("stamp = %+v, want an unmet retry stamp after stage_retried", st)
	}
	if appendIndex(au, CategoryDelegationShadowEvaluated) <= appendIndex(au, CategoryStageRetried) {
		t.Error("stamp must follow stage_retried")
	}

	t.Run("override of B names the override category", func(t *testing.T) {
		s, rr, au, _ := newShadowServer(t)
		stage := seedFailedStage(rr, run.FailureB, "forbidden_paths violated")
		seedShadowRun(rr, stage.RunID, shadowSpecYAML("low", ""))
		w := postRetryBody(t, s, stage.ID, `{"override":true,"reason":"false positive"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		if st := oneStamp(t, au); st.DecisionCategory != CategoryStageOverrideRetried {
			t.Errorf("decision_category = %q, want stage_override_retried", st.DecisionCategory)
		}
	})
}

func TestRetryShadowDecisionCategory(t *testing.T) {
	a, b := run.FailureA, run.FailureB
	cases := []struct {
		stage    *run.Stage
		override bool
		want     string
	}{
		{&run.Stage{FailureCategory: &b}, true, CategoryStageOverrideRetried},
		{&run.Stage{FailureCategory: &a}, true, CategoryStageRetried},
		{&run.Stage{FailureCategory: &b}, false, CategoryStageRetried},
		{&run.Stage{}, true, CategoryStageRetried},
		{nil, true, CategoryStageRetried},
	}
	for i, c := range cases {
		if got := retryShadowDecisionCategory(c.stage, c.override); got != c.want {
			t.Errorf("case %d: got %q, want %q", i, got, c.want)
		}
	}
}

// TestDelegationShadow_MergeFreshVerdict: a fresh merge verdict stamps the
// merge class after merge_verdict_recorded; a re-POST (alreadyRecorded)
// stamps nothing more.
func TestDelegationShadow_MergeFreshVerdict(t *testing.T) {
	merger := &fakeMerger{}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := uuid.New()
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, nil)

	if w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "lgtm"}, withMergeOperator); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	st := oneStamp(t, au)
	if st.Action != delegation.ActionMerge || st.HumanDecision != "merge" || st.DecisionCategory != CategoryMergeVerdictRecorded ||
		st.ActorSubject != "github:ops" {
		t.Errorf("stamp = %+v, want a merge stamp", st)
	}
	if appendIndex(au, CategoryDelegationShadowEvaluated) <= appendIndex(au, CategoryMergeVerdictRecorded) {
		t.Error("stamp must follow merge_verdict_recorded")
	}
	if w := postMergeRun(t, s, runID, mergeRunRequest{Verdict: "again"}, withMergeOperator); w.Code != http.StatusOK {
		t.Fatalf("re-POST status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if n := len(shadowStamps(t, au)); n != 1 {
		t.Errorf("stamps after an alreadyRecorded re-POST = %d, want 1", n)
	}
}

// TestDelegationShadow_PageHumanOnNotDelegable is AC3: medium tier (waive
// gated, page list includes requirement_arbitration); the sole open concern
// is LOW with category requirement, so solo_low WOULD be met — the page
// event turns the stamp into not_delegable, never a would-be auto.
func TestDelegationShadow_PageHumanOnNotDelegable(t *testing.T) {
	s, rr, au, cr := newShadowServer(t)
	runID := uuid.New()
	seedShadowRun(rr, runID, shadowSpecYAML("medium", ""))
	rows, err := cr.InsertRaised(context.Background(), concern.InsertRaisedParams{
		RunID: runID, StageID: uuid.New(), StageKind: concern.StageKindImplement,
		ReviewerModel: "m", OriginReviewSequence: 1,
		Concerns: []concern.RaisedConcern{{Severity: "low", Category: "requirement", Note: "which criterion?"}},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	w := postWaive(t, s, rows[0].ID.String(), waiveConcernRequest{Reason: "decided"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	st := oneStamp(t, au)
	if st.Verdict != string(delegation.ShadowNotDelegable) || st.Met || st.PageEvent != spec.PageEventRequirementArbitration {
		t.Errorf("stamp = %+v, want not_delegable page_event requirement_arbitration", st)
	}
	if !strings.HasPrefix(st.Reason, "page_human_on: requirement_arbitration") {
		t.Errorf("reason = %q, want the page_human_on reason", st.Reason)
	}
}

// TestDelegationShadow_CrewEscalationNotDelegable: an unruled root crew
// escalation pages unconditionally — even on autonomy:low, whose page list
// is empty — so the met solo_low verdict becomes not_delegable.
func TestDelegationShadow_CrewEscalationNotDelegable(t *testing.T) {
	s, rr, au, cr := newShadowServer(t)
	runID := uuid.New()
	seedShadowRun(rr, runID, shadowSpecYAML("low", ""))
	row := seedLowConcernRow(t, cr, runID, uuid.New())
	rid := runID
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &rid, Sequence: 40, Category: crewmessage.CategorySent,
		Payload: []byte(`{"message":{"type":"escalation"}}`),
	})

	if w := postWaive(t, s, row.ID.String(), waiveConcernRequest{Reason: "nit"}); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	st := oneStamp(t, au)
	if st.Verdict != string(delegation.ShadowNotDelegable) || st.PageEvent != PageEventCrewEscalation {
		t.Errorf("stamp = %+v, want not_delegable page_event crew_escalation", st)
	}
}

// TestDelegationShadow_ParkedAwaitingInputNotDelegable: a run parked at
// awaiting_input is stamped not_delegable without consulting the page list.
func TestDelegationShadow_ParkedAwaitingInputNotDelegable(t *testing.T) {
	s, rr, au, cr := newShadowServer(t)
	stage := rr.seedStage(run.StageStateAwaitingInput)
	seedShadowRun(rr, stage.RunID, shadowSpecYAML("low", ""))
	seedLowConcernRow(t, cr, stage.RunID, uuid.New())
	// An unruled crew escalation is ALSO active: the parked not_delegable
	// verdict must stand as-is (the page override applies only to met/unmet).
	rid := stage.RunID
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &rid, Sequence: 40, Category: crewmessage.CategorySent,
		Payload: []byte(`{"message":{"type":"escalation"}}`),
	})
	p := s.captureDelegationShadow(context.Background(), stage.RunID, delegation.ActionWaive, "github:human", false)
	s.recordDelegationShadow(context.Background(), p, nil, "waive", CategoryConcernWaived, nil)
	st := oneStamp(t, au)
	if st.Verdict != string(delegation.ShadowNotDelegable) || st.PageEvent != "" || !strings.HasPrefix(st.Reason, "parked_awaiting_input") {
		t.Errorf("stamp = %+v, want not_delegable parked_awaiting_input", st)
	}
}

// parityCase is one class's run state, built on a fresh server.
type parityCase struct {
	name      string
	action    string
	condition spec.DelegationCondition
	met       bool
	// seed builds the run state and returns the run id.
	seed func(t *testing.T, rr *approvalRunRepo, au *auditFake, cr *fakeConcernRepo, specYAML string) uuid.UUID
}

func parityCases() []parityCase {
	planGate := func(withVerdicts bool) func(*testing.T, *approvalRunRepo, *auditFake, *fakeConcernRepo, string) uuid.UUID {
		return func(t *testing.T, rr *approvalRunRepo, au *auditFake, _ *fakeConcernRepo, specYAML string) uuid.UUID {
			stage := seedPlanGate(rr, specYAML)
			if withVerdicts {
				seedTwoApproveVerdicts(t, au, stage.RunID)
			}
			return stage.RunID
		}
	}
	fixupGate := func(withRound bool) func(*testing.T, *approvalRunRepo, *auditFake, *fakeConcernRepo, string) uuid.UUID {
		return func(t *testing.T, rr *approvalRunRepo, au *auditFake, cr *fakeConcernRepo, specYAML string) uuid.UUID {
			stage := rr.seedGatelessStage(run.StageStateAwaitingApproval)
			seedShadowRun(rr, stage.RunID, specYAML)
			if withRound {
				seedReviewEntry(t, au, stage.RunID, 1, "implement_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 1})
				seedReviewEntry(t, au, stage.RunID, 2, "implement_reviewed", planreview.ImplementReviewedPayload{
					ReviewerKind: "agent", Authority: planreview.AuthorityAdvisory, Verdict: planreview.VerdictApproveWithConcerns,
				})
			}
			seedConcernRow(t, cr, stage.RunID, stage.ID, concern.StageKindImplement, 2, "tighten")
			return stage.RunID
		}
	}
	waiveState := func(low bool) func(*testing.T, *approvalRunRepo, *auditFake, *fakeConcernRepo, string) uuid.UUID {
		return func(t *testing.T, rr *approvalRunRepo, _ *auditFake, cr *fakeConcernRepo, specYAML string) uuid.UUID {
			runID := uuid.New()
			seedShadowRun(rr, runID, specYAML)
			if low {
				seedLowConcernRow(t, cr, runID, uuid.New())
			} else {
				seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "medium")
			}
			return runID
		}
	}
	retryState := func(cat run.FailureCategory, reason string) func(*testing.T, *approvalRunRepo, *auditFake, *fakeConcernRepo, string) uuid.UUID {
		return func(_ *testing.T, rr *approvalRunRepo, _ *auditFake, _ *fakeConcernRepo, specYAML string) uuid.UUID {
			stage := seedFailedStage(rr, cat, reason)
			seedShadowRun(rr, stage.RunID, specYAML)
			return stage.RunID
		}
	}
	mergeState := func(advanced bool) func(*testing.T, *approvalRunRepo, *auditFake, *fakeConcernRepo, string) uuid.UUID {
		return func(t *testing.T, rr *approvalRunRepo, au *auditFake, _ *fakeConcernRepo, specYAML string) uuid.UUID {
			runID := uuid.New()
			row := seedShadowRun(rr, runID, specYAML)
			pr := mergePR
			row.PullRequestURL = &pr
			if advanced {
				seedReviewEntry(t, au, runID, 5, drive.Category, drive.Advance{Rule: drive.RuleChecksGreenAwaitingMerge})
			}
			return runID
		}
	}
	return []parityCase{
		{"approve/unmet", delegation.ActionApprove, spec.ConditionCleanDualApproval, false, planGate(false)},
		{"approve/met", delegation.ActionApprove, spec.ConditionCleanDualApproval, true, planGate(true)},
		{"route_fixup/unmet", delegation.ActionRouteFixup, spec.ConditionConvergentConcerns, false, fixupGate(false)},
		{"route_fixup/met", delegation.ActionRouteFixup, spec.ConditionConvergentConcerns, true, fixupGate(true)},
		{"waive/unmet", delegation.ActionWaive, spec.ConditionSoloLow, false, waiveState(false)},
		{"waive/met", delegation.ActionWaive, spec.ConditionSoloLow, true, waiveState(true)},
		{"retry/unmet", delegation.ActionRetry, spec.ConditionInfraFlake, false, retryState(run.FailureB, "forbidden_paths violated")},
		{"retry/met", delegation.ActionRetry, spec.ConditionInfraFlake, true, retryState(run.FailureA,
			"failed to start container: context deadline exceeded after 9 retries")},
		{"merge/unmet", delegation.ActionMerge, spec.ConditionGatesResolvedCIGreen, false, mergeState(false)},
		{"merge/met", delegation.ActionMerge, spec.ConditionGatesResolvedCIGreen, true, mergeState(true)},
	}
}

// TestDelegationShadow_ParityWithCheckDelegation is AC4 over all five
// classes: on ONE run state, checkDelegation under spec A (autonomy:high —
// the class auto) and the human-action stamp under spec B (autonomy:low —
// the class gated) answer the same condition, met bit and unmet reason. The
// stamp takes exactly the inputs checkDelegation takes (run id + action);
// neither reads request-specific inputs, so none are threaded.
func TestDelegationShadow_ParityWithCheckDelegation(t *testing.T) {
	for _, c := range parityCases() {
		t.Run(c.name, func(t *testing.T) {
			s, rr, au, cr := newShadowServer(t)
			runID := c.seed(t, rr, au, cr, shadowSpecYAML("high", ""))

			req := httptest.NewRequest(http.MethodPost, "/x", nil)
			w := httptest.NewRecorder()
			rule, ok := s.checkDelegation(w, withAuth(req), runID, c.action)
			var wantReason string
			if c.met {
				if !ok || rule != string(c.condition) {
					t.Fatalf("checkDelegation = (%q, %v), want met %q:\n%s", rule, ok, c.condition, w.Body.String())
				}
			} else {
				if ok || w.Code != http.StatusForbidden {
					t.Fatalf("checkDelegation = (%q, %v) code %d, want 403:\n%s", rule, ok, w.Code, w.Body.String())
				}
				errBody := decodeErrorEnvelope(t, w)
				if errBody.Code != "delegation_condition_unmet" || errBody.Details["condition"] != string(c.condition) {
					t.Fatalf("error = %+v, want delegation_condition_unmet on %s", errBody, c.condition)
				}
				wantReason, _ = errBody.Details["unmet_reason"].(string)
				if wantReason == "" {
					t.Fatal("refusal carried no unmet_reason")
				}
			}

			// Same state, spec B: the class is gated, the human acts.
			row, _ := rr.GetRun(context.Background(), runID)
			row.WorkflowSpec = []byte(shadowSpecYAML("low", ""))
			p := s.captureDelegationShadow(context.Background(), runID, c.action, "github:human", false)
			s.recordDelegationShadow(context.Background(), p, nil, "x", "x", nil)
			st := oneStamp(t, au)
			if st.Mode != string(spec.ModeGated) {
				t.Errorf("stamp mode = %q, want gated under spec B", st.Mode)
			}
			if st.Condition != string(c.condition) || st.Met != c.met || st.Reason != wantReason {
				t.Errorf("stamp = {condition %q met %v reason %q}, want {%q %v %q}", st.Condition, st.Met, st.Reason, c.condition, c.met, wantReason)
			}
		})
	}
}

// TestDelegationShadow_DelegatedActionNotStamped: a delegated approve that
// SUCCEEDS (approve auto, condition met) writes approval_submitted and zero
// stamps.
func TestDelegationShadow_DelegatedActionNotStamped(t *testing.T) {
	s, rr, au, _ := newShadowServer(t)
	stage := seedPlanGate(rr, shadowSpecYAML("high", ""))
	seedTwoApproveVerdicts(t, au, stage.RunID)

	w := submitApproval(t, s, stage.ID, `{"decision":"approve","delegated":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if appendIndex(au, "approval_submitted") < 0 {
		t.Fatal("the delegated approve wrote no approval_submitted row; the fixture does not reach the decision")
	}
	if n := len(shadowStamps(t, au)); n != 0 {
		t.Errorf("stamps = %d, want 0 for a delegated action", n)
	}
}

// TestDelegationShadow_AgentSubjectNotStamped: an operator-agent token's
// waive is an agent action — zero stamps.
func TestDelegationShadow_AgentSubjectNotStamped(t *testing.T) {
	s, rr, au, cr := newShadowServer(t)
	runID := uuid.New()
	seedShadowRun(rr, runID, shadowSpecYAML("low", ""))
	row := seedLowConcernRow(t, cr, runID, uuid.New())

	w := postWaiveAs(t, s, row.ID.String(), waiveConcernRequest{Reason: "nit"}, withOperatorAgentAuth)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if appendIndex(au, CategoryConcernWaived) < 0 {
		t.Fatal("the agent waive wrote no concern_waived row; the fixture does not reach the decision")
	}
	if n := len(shadowStamps(t, au)); n != 0 {
		t.Errorf("stamps = %d, want 0 for an agent subject", n)
	}
	if p := s.captureDelegationShadow(context.Background(), runID, delegation.ActionWaive, "mcp:run:"+runID.String(), false); p != nil {
		t.Error("an mcp:run subject must never be stamped")
	}
}

// TestDelegationShadow_NoStampOnDuplicateOrRefusal: a duplicate approval and
// a refused waive add no stamp.
func TestDelegationShadow_NoStampOnDuplicateOrRefusal(t *testing.T) {
	s, rr, au, cr := newShadowServer(t)
	stage := seedPlanGate(rr, shadowSpecYAML("low", ""))
	if w := submitApproval(t, s, stage.ID, `{"decision":"reject"}`); w.Code != http.StatusOK {
		t.Fatalf("first status = %d:\n%s", w.Code, w.Body.String())
	}
	// Re-park the stage so the duplicate reaches the capture point again.
	stage.State = run.StageStateAwaitingApproval
	if w := submitApproval(t, s, stage.ID, `{"decision":"reject"}`); w.Code != http.StatusOK {
		t.Fatalf("duplicate status = %d:\n%s", w.Code, w.Body.String())
	}
	if n := len(shadowStamps(t, au)); n != 1 {
		t.Errorf("stamps after a duplicate = %d, want 1", n)
	}

	row := seedLowConcernRow(t, cr, stage.RunID, uuid.New())
	row.State = concern.StateWaived
	if w := postWaive(t, s, row.ID.String(), waiveConcernRequest{Reason: "again"}); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("refused waive status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if n := len(shadowStamps(t, au)); n != 1 {
		t.Errorf("stamps after a refused waive = %d, want 1", n)
	}
}

// failNthConcernRepo fails ListOpenByRun from the nth call on, isolating the
// capture's own open-concern read (Shadow's read is the first).
type failNthConcernRepo struct {
	*fakeConcernRepo
	n     int32
	calls atomic.Int32
}

func (f *failNthConcernRepo) ListOpenByRun(ctx context.Context, runID uuid.UUID) ([]*concern.Concern, error) {
	if f.calls.Add(1) >= f.n {
		return nil, errors.New("concern store down")
	}
	return f.fakeConcernRepo.ListOpenByRun(ctx, runID)
}

// blockingRunRepo ignores its context and blocks GetRun until release.
type blockingRunRepo struct {
	*approvalRunRepo
	release chan struct{}
}

func (b *blockingRunRepo) GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error) {
	<-b.release
	return b.approvalRunRepo.GetRun(ctx, id)
}

// TestDelegationShadow_Unevaluable: every failure mode yields an
// unevaluable stamp naming the mode — never a skipped stamp and never a
// refused human action.
func TestDelegationShadow_Unevaluable(t *testing.T) {
	capture := func(t *testing.T, s *Server, runID uuid.UUID) delegationShadowPayload {
		t.Helper()
		p := s.captureDelegationShadow(context.Background(), runID, delegation.ActionWaive, "github:human", false)
		if p == nil {
			t.Fatal("capture returned nil; an unevaluable human action must still be stamped")
		}
		if p.payload.Verdict != string(delegation.ShadowUnevaluable) || p.payload.Met {
			t.Fatalf("verdict = %q, want unevaluable", p.payload.Verdict)
		}
		return p.payload
	}
	cases := []struct {
		name, mode string
		spec       string
		mutate     func(s *Server, rr *approvalRunRepo, cr *fakeConcernRepo, runID uuid.UUID)
	}{
		{name: "no cached spec", mode: "no_cached_spec", spec: ""},
		{name: "unparseable spec", mode: "spec_unparseable", spec: "version: [not yaml"},
		{name: "workflow missing", mode: "workflow_missing", spec: strings.Replace(shadowSpecYAML("low", ""), "feature_change:", "other_flow:", 1)},
		{name: "run read failed", mode: "run_read_failed", spec: shadowSpecYAML("low", ""),
			mutate: func(_ *Server, rr *approvalRunRepo, _ *fakeConcernRepo, runID uuid.UUID) { delete(rr.runs, runID) }},
		{name: "repositories unconfigured", mode: "repositories_unconfigured", spec: shadowSpecYAML("low", ""),
			mutate: func(s *Server, _ *approvalRunRepo, _ *fakeConcernRepo, _ uuid.UUID) { s.cfg.ConcernRepo = nil }},
		{name: "shadow evaluation failed", mode: "shadow_evaluation_failed", spec: shadowSpecYAML("low", ""),
			mutate: func(_ *Server, _ *approvalRunRepo, cr *fakeConcernRepo, _ uuid.UUID) {
				cr.listErr = errors.New("concern store down")
			}},
		{name: "open concern read failed", mode: "open_concern_read_failed", spec: shadowSpecYAML("low", ""),
			mutate: func(s *Server, _ *approvalRunRepo, cr *fakeConcernRepo, _ uuid.UUID) {
				s.cfg.ConcernRepo = &failNthConcernRepo{fakeConcernRepo: cr, n: 2}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, rr, _, cr := newShadowServer(t)
			runID := uuid.New()
			seedShadowRun(rr, runID, c.spec)
			if c.mutate != nil {
				c.mutate(s, rr, cr, runID)
			}
			if got := capture(t, s, runID); !strings.HasPrefix(got.Reason, c.mode+": ") {
				t.Errorf("reason = %q, want it to name %s", got.Reason, c.mode)
			}
		})
	}

	t.Run("human action still succeeds", func(t *testing.T) {
		s, rr, au, cr := newShadowServer(t)
		runID := uuid.New()
		seedShadowRun(rr, runID, "")
		row := seedLowConcernRow(t, cr, runID, uuid.New())
		if w := postWaive(t, s, row.ID.String(), waiveConcernRequest{Reason: "nit"}); w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		if st := oneStamp(t, au); st.Verdict != string(delegation.ShadowUnevaluable) || !strings.HasPrefix(st.Reason, "no_cached_spec: ") {
			t.Errorf("stamp = %+v, want unevaluable no_cached_spec", st)
		}
	})

	t.Run("capture timeout bounds the delay", func(t *testing.T) {
		delegationShadowCaptureTimeoutOverride.Store(int64(50 * time.Millisecond))
		t.Cleanup(func() { delegationShadowCaptureTimeoutOverride.Store(0) })
		s, rr, _, _ := newShadowServer(t)
		runID := uuid.New()
		seedShadowRun(rr, runID, shadowSpecYAML("low", ""))
		blk := &blockingRunRepo{approvalRunRepo: rr, release: make(chan struct{})}
		var once sync.Once
		t.Cleanup(func() { once.Do(func() { close(blk.release) }) })
		s.cfg.RunRepo = blk
		start := time.Now()
		got := capture(t, s, runID)
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("capture took %s, want it bounded by the 50ms timeout", elapsed)
		}
		if !strings.HasPrefix(got.Reason, "capture_timeout: ") {
			t.Errorf("reason = %q, want capture_timeout", got.Reason)
		}
	})
}

// TestDelegationShadow_NotStampedWithoutAuditRepo: no audit store, no stamp
// (and recordDelegationShadow is nil-safe).
func TestDelegationShadow_NotStampedWithoutAuditRepo(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	if p := s.captureDelegationShadow(context.Background(), uuid.New(), delegation.ActionWaive, "github:human", false); p != nil {
		t.Error("capture without an audit store must return nil")
	}
	s.recordDelegationShadow(context.Background(), nil, nil, "waive", CategoryConcernWaived, nil)
}

// TestDelegationShadow_EscalationStratumAndNoEmission: a label escalation
// clamping a high-tier run to low fires; the stamp records its RuleKey, mode
// gated / mode_source escalation, and the capture writes NO escalation_fired.
func TestDelegationShadow_EscalationStratumAndNoEmission(t *testing.T) {
	const esc = `    escalations:
      - match:
          labels: [security]
        require:
          max_autonomy: low
`
	specYAML := shadowSpecYAML("high", esc)
	s, rr, au, cr := newShadowServer(t)
	runID := uuid.New()
	row := seedShadowRun(rr, runID, specYAML)
	row.IssueContext = &run.IssueContext{Labels: []string{"security"}}
	c := seedLowConcernRow(t, cr, runID, uuid.New())

	if w := postWaive(t, s, c.ID.String(), waiveConcernRequest{Reason: "nit"}); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	parsed, err := spec.ParseBytes([]byte(specYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	wantKey := escalation.RuleKey(parsed.Workflows["feature_change"].Escalations[0])
	st := oneStamp(t, au)
	if len(st.EscalationKeys) != 1 || st.EscalationKeys[0] != wantKey || st.EscalationFingerprint == "" || st.MaxAutonomy != "low" {
		t.Errorf("escalation stratum = keys %v fp %q max %q, want [%s]", st.EscalationKeys, st.EscalationFingerprint, st.MaxAutonomy, wantKey)
	}
	if st.Mode != string(spec.ModeGated) || st.ModeSource != string(spec.SourceEscalation) {
		t.Errorf("mode = %q/%q, want gated/escalation", st.Mode, st.ModeSource)
	}
	if idx := auditEntriesByCategory(au, "escalation_fired"); len(idx) != 0 {
		t.Errorf("escalation_fired rows = %d, want 0 (the shadow resolver must not emit)", len(idx))
	}
}

// TestDelegationShadow_AppendFailureDoesNotFailHuman: a failed stamp append
// leaves the human's waive 200 and applied.
func TestDelegationShadow_AppendFailureDoesNotFailHuman(t *testing.T) {
	s, rr, au, cr := newShadowServer(t)
	runID := uuid.New()
	seedShadowRun(rr, runID, shadowSpecYAML("low", ""))
	row := seedLowConcernRow(t, cr, runID, uuid.New())
	au.appendErrCategory = CategoryDelegationShadowEvaluated

	if w := postWaive(t, s, row.ID.String(), waiveConcernRequest{Reason: "nit"}); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 despite the stamp append failing:\n%s", w.Code, w.Body.String())
	}
	if row.State != concern.StateWaived {
		t.Errorf("concern state = %q, want waived", row.State)
	}
}

// TestDelegationShadow_AppendSurvivesRequestCancellation: the stamp append
// is detached from request cancellation.
func TestDelegationShadow_AppendSurvivesRequestCancellation(t *testing.T) {
	s, rr, au, _ := newShadowServer(t)
	runID := uuid.New()
	seedShadowRun(rr, runID, shadowSpecYAML("low", ""))
	ctx, cancel := context.WithCancel(context.Background())
	p := s.captureDelegationShadow(ctx, runID, delegation.ActionWaive, "github:human", false)
	cancel()
	cctx := &cancelCheckingAudit{auditFake: au}
	s.cfg.AuditRepo = cctx
	s.recordDelegationShadow(ctx, p, nil, "waive", CategoryConcernWaived, nil)
	if cctx.sawCanceled {
		t.Error("the stamp append ran on a canceled context")
	}
	if n := len(shadowStamps(t, au)); n != 1 {
		t.Errorf("stamps = %d, want 1", n)
	}
}

// cancelCheckingAudit records whether AppendChained saw a canceled context.
type cancelCheckingAudit struct {
	*auditFake
	sawCanceled bool
}

func (c *cancelCheckingAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if ctx.Err() != nil {
		c.sawCanceled = true
	}
	return c.auditFake.AppendChained(ctx, p)
}

// TestDelegationShadow_SlashCommandApprove covers the issue slash-command
// approval channel (issue_approval.go), a human path outside approveStageAs:
// a /fishhawk approve stamps once after approval_submitted, and a duplicate
// command from the same approver adds no second stamp.
func TestDelegationShadow_SlashCommandApprove(t *testing.T) {
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	r.TriggerSource = run.TriggerGitHubIssue
	triggerRef := "issue:42"
	r.TriggerRef = &triggerRef
	r.InstallationID = ptrInt64(99)
	r.Repo = "x/y"
	r.WorkflowID = "feature_change"
	r.WorkflowSpec = []byte(shadowSpecYAML("low", ""))
	stage := rr.seedStage(r.ID, 0, run.StageStateAwaitingApproval)
	stage.Type = run.StageTypePlan

	au := newAuditFake()
	ar := newFakeApprovalRepo()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		ApprovalRepo: ar,
		AuditRepo:    au,
		ConcernRepo:  newFakeConcernRepo(),
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})
	gh := newSlashGitHubRecorder()
	s.issueNotifier = issuecomment.New(issuecomment.Deps{GitHub: gh, Runs: rr, Audit: au})
	cmd := webhook.ApprovalCommandParams{
		Repo: "x/y", IssueNumber: 42, InstallationID: 99,
		SenderLogin: "alice", Decision: webhook.MatchActionApprove,
	}
	if err := s.HandleApprovalCommand(context.Background(), cmd); err != nil {
		t.Fatalf("HandleApprovalCommand: %v", err)
	}
	st := oneStamp(t, au)
	if st.Action != delegation.ActionApprove || st.HumanDecision != "approve" || st.ActorSubject != "alice" ||
		st.Mode != string(spec.ModeGated) || st.Verdict != string(delegation.ShadowUnmet) {
		t.Errorf("stamp = %+v, want an unmet gated approve stamp for alice", st)
	}
	if appendIndex(au, CategoryDelegationShadowEvaluated) <= appendIndex(au, "approval_submitted") {
		t.Error("stamp must follow approval_submitted")
	}

	// Duplicate: a second run whose gate already holds alice's approval row,
	// so the command reaches Submit and takes the Inserted=false path — the
	// capture ran, but no fresh decision landed, so no stamp is recorded.
	r2 := rr.seedRun()
	r2.TriggerSource = run.TriggerGitHubIssue
	ref2 := "issue:43"
	r2.TriggerRef = &ref2
	r2.InstallationID = ptrInt64(99)
	r2.Repo = "x/y"
	r2.WorkflowID = "feature_change"
	r2.WorkflowSpec = []byte(shadowSpecYAML("low", ""))
	stage2 := rr.seedStage(r2.ID, 0, run.StageStateAwaitingApproval)
	stage2.Type = run.StageTypePlan
	if _, err := ar.Submit(context.Background(), approval.SubmitParams{
		StageID: stage2.ID, ApproverSubject: "alice", Decision: approval.DecisionApprove, Surface: approval.SurfaceGitHubComment,
	}); err != nil {
		t.Fatalf("seed prior approval: %v", err)
	}
	cmd.IssueNumber = 43
	if err := s.HandleApprovalCommand(context.Background(), cmd); err != nil {
		t.Fatalf("duplicate HandleApprovalCommand: %v", err)
	}
	var sawDuplicateReply bool
	for _, c := range gh.calls() {
		if strings.Contains(c.body, "already submitted") {
			sawDuplicateReply = true
		}
	}
	if !sawDuplicateReply {
		t.Fatal("the duplicate command did not reach the Inserted=false path; the fixture does not exercise the duplicate guard")
	}
	if n := len(shadowStamps(t, au)); n != 1 {
		t.Errorf("stamps after a duplicate slash command = %d, want 1", n)
	}
}

// assertDecisionThenOneStamp asserts the appended chain is exactly the
// decision row followed by ONE delegation_shadow_evaluated row — the shape a
// human decision on a delegable class writes since E82.1 / #3778. Shared by
// the handler tests that pin the full audit chain of a human action.
func assertDecisionThenOneStamp(t *testing.T, appended []audit.ChainAppendParams, decisionCategory string) {
	t.Helper()
	cats := make([]string, 0, len(appended))
	for _, e := range appended {
		cats = append(cats, e.Category)
	}
	if len(appended) != 2 || appended[0].Category != decisionCategory || appended[1].Category != CategoryDelegationShadowEvaluated {
		t.Fatalf("audit categories = %v, want [%s %s] (the decision row, then exactly one shadow stamp after it)",
			cats, decisionCategory, CategoryDelegationShadowEvaluated)
	}
}
