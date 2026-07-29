package delegation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// --- fakes ------------------------------------------------------------------

type fakeStages struct {
	stages []*run.Stage
	err    error
}

func (f *fakeStages) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	return f.stages, f.err
}

type fakeConcerns struct {
	open []*concern.Concern
	err  error
}

func (f *fakeConcerns) ListOpenByRun(context.Context, uuid.UUID) ([]*concern.Concern, error) {
	return f.open, f.err
}

type fakeAudit struct {
	entries map[string][]*audit.Entry
	err     error
}

func (f *fakeAudit) ListForRunByCategory(_ context.Context, _ uuid.UUID, category string) ([]*audit.Entry, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.entries[category], nil
}

// --- fixture builders --------------------------------------------------------

func mkStage(seq int, t run.StageType, state run.StageState) *run.Stage {
	return &run.Stage{ID: uuid.New(), Sequence: seq, Type: t, State: state}
}

func failedStage(seq int, cat run.FailureCategory, reason string) *run.Stage {
	st := mkStage(seq, run.StageTypeImplement, run.StageStateFailed)
	st.FailureCategory = &cat
	st.FailureReason = &reason
	return st
}

func openConcern(severity string) *concern.Concern {
	return &concern.Concern{ID: uuid.New(), Severity: severity, State: concern.StateRaised}
}

func entry(seq int64, payload any) *audit.Entry {
	b, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return &audit.Entry{Sequence: seq, Payload: b}
}

func startedEntry(seq int64, configured int) *audit.Entry {
	return entry(seq, planreview.ReviewStartedPayload{ConfiguredAgents: configured})
}

func verdictEntry(seq int64, v planreview.Verdict) *audit.Entry {
	return entry(seq, planreview.ImplementReviewedPayload{ReviewerKind: "agent", Verdict: v})
}

// testWorkflow builds a gated plan+implement workflow. block is the
// workflow-level operator_agent; gateBlock (optional) the plan gate's
// per-gate override.
func testWorkflow(block, gateBlock *spec.OperatorAgent) *spec.Workflow {
	return &spec.Workflow{
		OperatorAgent: block,
		Stages: []spec.Stage{
			{
				ID: "plan", Type: spec.StageTypePlan,
				Reviewers: &spec.ReviewersConfig{Agent: 2, Human: 1},
				Gates:     []spec.Gate{{Type: spec.GateTypeApproval, OperatorAgent: gateBlock}},
			},
			{
				ID: "implement", Type: spec.StageTypeImplement,
				Reviewers: &spec.ReviewersConfig{Agent: 2, Human: 1},
				Gates:     []spec.Gate{{Type: spec.GateTypeApproval}},
			},
		},
	}
}

func allKnobs() *spec.OperatorAgent {
	return &spec.OperatorAgent{
		MayApprove:    spec.ConditionCleanDualApproval,
		MayRouteFixup: spec.ConditionConvergentConcerns,
		MayWaive:      spec.ConditionSoloLow,
		MayRetry:      spec.ConditionInfraFlake,
		MayMerge:      spec.ConditionGatesResolvedCIGreen,
		MustPageHuman: []string{spec.PageEventReviewerReject},
	}
}

func evaluate(t *testing.T, ev *Evaluator, wf *spec.Workflow, runRow *run.Run) *Result {
	t.Helper()
	res, err := ev.Evaluate(context.Background(), runRow, wf, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return res
}

func decisionFor(t *testing.T, res *Result, action string) Decision {
	t.Helper()
	if res == nil {
		t.Fatal("Result is nil, want a configured evaluation")
	}
	for _, d := range res.Actions {
		if d.Action == action {
			return d
		}
	}
	t.Fatalf("no decision for action %q in %+v", action, res.Actions)
	return Decision{}
}

func newRun() *run.Run { return &run.Run{ID: uuid.New(), State: run.StateRunning} }

// --- Configured / fail-closed -----------------------------------------------

func TestConfigured(t *testing.T) {
	if Configured(nil) {
		t.Error("Configured(nil) = true")
	}
	if Configured(testWorkflow(nil, nil)) {
		t.Error("Configured = true for a workflow with no block anywhere")
	}
	if !Configured(testWorkflow(allKnobs(), nil)) {
		t.Error("Configured = false for a workflow-level block")
	}
	if !Configured(testWorkflow(nil, allKnobs())) {
		t.Error("Configured = false for a gate-level-only block")
	}
}

// TestEvaluate_NoBlock_FailClosed: a spec without an operator_agent
// block evaluates to nil — nothing delegated, no repository reads.
func TestEvaluate_NoBlock_FailClosed(t *testing.T) {
	ev := &Evaluator{
		Stages:   &fakeStages{err: errors.New("must not be called")},
		Concerns: &fakeConcerns{err: errors.New("must not be called")},
		Audit:    &fakeAudit{err: errors.New("must not be called")},
	}
	res, err := ev.Evaluate(context.Background(), newRun(), testWorkflow(nil, nil), nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if res != nil {
		t.Fatalf("Result = %+v, want nil (fail-closed)", res)
	}
}

// TestEvaluate_GateOverrideWinsWholesale: when the pending gate carries
// its own block, the workflow-level knobs are NOT merged in.
func TestEvaluate_GateOverrideWinsWholesale(t *testing.T) {
	gateBlock := &spec.OperatorAgent{
		MayWaive:      spec.ConditionSoloLow,
		MustPageHuman: []string{spec.PageEventPlanRejection},
	}
	wf := testWorkflow(allKnobs(), gateBlock)
	ev := &Evaluator{
		Stages:   &fakeStages{stages: []*run.Stage{mkStage(0, run.StageTypePlan, run.StageStateAwaitingApproval)}},
		Concerns: &fakeConcerns{open: []*concern.Concern{openConcern("low")}},
		Audit:    &fakeAudit{},
	}
	res := evaluate(t, ev, wf, newRun())
	if len(res.Actions) != 1 || res.Actions[0].Action != ActionWaive {
		t.Fatalf("Actions = %+v, want only the gate block's waive knob (wholesale override)", res.Actions)
	}
	if !res.Actions[0].Met {
		t.Errorf("waive decision = %+v, want met (one low-severity open concern)", res.Actions[0])
	}
	if len(res.MustPageHuman) != 1 || res.MustPageHuman[0] != spec.PageEventPlanRejection {
		t.Errorf("MustPageHuman = %v, want the gate block's list", res.MustPageHuman)
	}
}

// TestEvaluate_WorkflowBlockWhenNoGatePending: with no stage awaiting
// approval, the workflow-level block governs (a gate-level override
// only applies while its gate is the pending one).
func TestEvaluate_WorkflowBlockWhenNoGatePending(t *testing.T) {
	wf := testWorkflow(allKnobs(), &spec.OperatorAgent{MayWaive: spec.ConditionSoloLow})
	ev := &Evaluator{
		Stages:   &fakeStages{stages: []*run.Stage{mkStage(0, run.StageTypePlan, run.StageStateSucceeded)}},
		Concerns: &fakeConcerns{},
		Audit:    &fakeAudit{},
	}
	res := evaluate(t, ev, wf, newRun())
	if len(res.Actions) != 5 {
		t.Fatalf("Actions = %+v, want all five workflow-level knobs", res.Actions)
	}
}

// TestEvaluate_AwaitingInputParksHuman: a stage parked at awaiting_input
// (#1057, the planner's clarification_request gate) is a parked
// D-category judgment — the operator agent delegates nothing and pages
// the human. Even a lone low-severity open concern (which would
// otherwise satisfy solo_low/waive) must NOT yield a met action.
func TestEvaluate_AwaitingInputParksHuman(t *testing.T) {
	wf := testWorkflow(allKnobs(), nil)
	ev := &Evaluator{
		Stages:   &fakeStages{stages: []*run.Stage{mkStage(0, run.StageTypePlan, run.StageStateAwaitingInput)}},
		Concerns: &fakeConcerns{open: []*concern.Concern{openConcern("low")}},
		Audit:    &fakeAudit{},
	}
	res := evaluate(t, ev, wf, newRun())
	if res == nil {
		t.Fatal("Result is nil; want the must_page_human envelope while parked at awaiting_input")
	}
	if len(res.Actions) != 0 {
		t.Errorf("Actions = %+v, want none delegated while parked at awaiting_input", res.Actions)
	}
	if len(res.MustPageHuman) != 1 || res.MustPageHuman[0] != spec.PageEventReviewerReject {
		t.Errorf("MustPageHuman = %v, want the effective block's page list", res.MustPageHuman)
	}
}

// TestEvaluate_GateOnlyBlock_OtherGatePending: a gate-level-only block
// does not govern while NO gate (or a different stage's gate) is
// pending — fail-closed, nil result.
func TestEvaluate_GateOnlyBlock_NotPending_FailClosed(t *testing.T) {
	wf := testWorkflow(nil, allKnobs()) // block only on the plan gate
	ev := &Evaluator{
		Stages:   &fakeStages{stages: []*run.Stage{mkStage(0, run.StageTypePlan, run.StageStateSucceeded), mkStage(1, run.StageTypeImplement, run.StageStateAwaitingApproval)}},
		Concerns: &fakeConcerns{},
		Audit:    &fakeAudit{},
	}
	res, err := ev.Evaluate(context.Background(), newRun(), wf, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if res != nil {
		t.Fatalf("Result = %+v, want nil (the implement gate carries no block and the workflow level has none)", res)
	}
}

// --- campaign-level override precedence (E25.12 / #1451) ---------------------

// actionSet returns the set of delegated action names in a Result, so a
// precedence test can assert WHICH block's knobs govern without depending on
// per-knob met-ness.
func actionSet(res *Result) map[string]bool {
	got := make(map[string]bool, len(res.Actions))
	for _, d := range res.Actions {
		got[d.Action] = true
	}
	return got
}

// TestEvaluate_CampaignOverridePrecedence is the binding-condition assertion
// (E25.12): the campaign-level operator_agent override is the OUTERMOST rung of
// the resolution ladder — campaign > gate > workflow — and wins WHOLESALE
// (knobs are never merged across levels, matching EffectiveOperatorAgent). It
// also pins the unchanged-behavior contract: absent a campaign override, the
// per-workflow contract (gate-over-workflow) is inherited byte-identically.
//
// Each level carries a DISTINGUISHABLE single page-event + knob fingerprint so
// the asserted Result uniquely identifies which level governed:
//   - workflow level: all five knobs + reviewer_reject
//   - gate level:     only may_waive   + plan_rejection
//   - campaign level: only may_retry   + requirement_arbitration
func TestEvaluate_CampaignOverridePrecedence(t *testing.T) {
	gateBlock := &spec.OperatorAgent{
		MayWaive:      spec.ConditionSoloLow,
		MustPageHuman: []string{spec.PageEventPlanRejection},
	}
	campaignBlock := &spec.OperatorAgent{
		MayRetry:      spec.ConditionInfraFlake,
		MustPageHuman: []string{spec.PageEventRequirementArbitration},
	}
	// A plan stage parked at its approval gate, so the plan gate is the pending
	// gate and gateBlock is the workflow's EffectiveOperatorAgent absent a
	// campaign override.
	newEvaluator := func() *Evaluator {
		return &Evaluator{
			Stages:   &fakeStages{stages: []*run.Stage{mkStage(0, run.StageTypePlan, run.StageStateAwaitingApproval)}},
			Concerns: &fakeConcerns{},
			Audit:    &fakeAudit{},
		}
	}

	t.Run("campaign override wins over gate and workflow wholesale", func(t *testing.T) {
		wf := testWorkflow(allKnobs(), gateBlock)
		res, err := newEvaluator().Evaluate(context.Background(), newRun(), wf, campaignBlock)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if res == nil {
			t.Fatal("Result is nil, want the campaign block's evaluation")
		}
		got := actionSet(res)
		if len(got) != 1 || !got[ActionRetry] {
			t.Errorf("actions = %v, want only the campaign block's retry knob (campaign wins wholesale, never merged)", got)
		}
		if len(res.MustPageHuman) != 1 || res.MustPageHuman[0] != spec.PageEventRequirementArbitration {
			t.Errorf("MustPageHuman = %v, want the campaign block's list", res.MustPageHuman)
		}
	})

	t.Run("nil campaign override inherits gate-over-workflow unchanged", func(t *testing.T) {
		wf := testWorkflow(allKnobs(), gateBlock)
		res, err := newEvaluator().Evaluate(context.Background(), newRun(), wf, nil)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if res == nil {
			t.Fatal("Result is nil, want the gate block's evaluation")
		}
		got := actionSet(res)
		if len(got) != 1 || !got[ActionWaive] {
			t.Errorf("actions = %v, want only the gate block's waive knob (gate-over-workflow preserved when no campaign override)", got)
		}
		if len(res.MustPageHuman) != 1 || res.MustPageHuman[0] != spec.PageEventPlanRejection {
			t.Errorf("MustPageHuman = %v, want the gate block's list", res.MustPageHuman)
		}
	})

	t.Run("nil campaign override and no gate block inherits workflow contract unchanged", func(t *testing.T) {
		wf := testWorkflow(allKnobs(), nil)
		res, err := newEvaluator().Evaluate(context.Background(), newRun(), wf, nil)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if res == nil {
			t.Fatal("Result is nil, want the workflow block's evaluation")
		}
		if got := actionSet(res); len(got) != 5 {
			t.Errorf("actions = %v, want all five workflow-level knobs (per-workflow contract inherited unchanged)", got)
		}
		if len(res.MustPageHuman) != 1 || res.MustPageHuman[0] != spec.PageEventReviewerReject {
			t.Errorf("MustPageHuman = %v, want the workflow block's list", res.MustPageHuman)
		}
	})

	t.Run("campaign override governs a workflow with no block of its own", func(t *testing.T) {
		// The campaign override must apply even when the workflow declares
		// nothing (Configured(wf) == false) — the cheap short-circuit must not
		// suppress a campaign-governed run.
		wf := testWorkflow(nil, nil)
		res, err := newEvaluator().Evaluate(context.Background(), newRun(), wf, campaignBlock)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if res == nil {
			t.Fatal("Result is nil, want the campaign block to govern a blockless workflow")
		}
		if got := actionSet(res); len(got) != 1 || !got[ActionRetry] {
			t.Errorf("actions = %v, want only the campaign block's retry knob", got)
		}
		if len(res.MustPageHuman) != 1 || res.MustPageHuman[0] != spec.PageEventRequirementArbitration {
			t.Errorf("MustPageHuman = %v, want the campaign block's list", res.MustPageHuman)
		}
	})
}

// --- model_policy passthrough (#1421) ----------------------------------------

// samplePolicy is a fully-populated scenario-A model policy used by the
// passthrough tests.
func samplePolicy() *spec.ModelPolicy {
	return &spec.ModelPolicy{
		Strategy: spec.ModelPolicyExplicitDefaults,
		Defaults: &spec.ModelPolicyDefaults{Plan: "claude-opus-4-8", Implement: "claude-sonnet-4-6", Review: "gpt-5.5"},
		Allowed:  []string{"claude-opus-4-8", "claude-sonnet-4-6", "gpt-5.5"},
	}
}

// TestEvaluate_ModelPolicy_Passthrough asserts Evaluate surfaces the
// effective block's model_policy verbatim as static config (#1421) — no
// condition is evaluated — across the main path, the parked
// awaiting_input early-return path, and the wholesale gate override, and
// that it stays nil when the effective block declares none.
func TestEvaluate_ModelPolicy_Passthrough(t *testing.T) {
	t.Run("main path surfaces the workflow block's policy", func(t *testing.T) {
		block := allKnobs()
		block.ModelPolicy = samplePolicy()
		wf := testWorkflow(block, nil)
		ev := &Evaluator{
			Stages:   &fakeStages{stages: []*run.Stage{mkStage(0, run.StageTypePlan, run.StageStateSucceeded)}},
			Concerns: &fakeConcerns{},
			Audit:    &fakeAudit{},
		}
		res := evaluate(t, ev, wf, newRun())
		if res.ModelPolicy != block.ModelPolicy {
			t.Fatalf("ModelPolicy = %+v, want the workflow block's policy passed through by pointer", res.ModelPolicy)
		}
	})

	t.Run("parked awaiting_input still surfaces the policy", func(t *testing.T) {
		block := allKnobs()
		block.ModelPolicy = samplePolicy()
		wf := testWorkflow(block, nil)
		ev := &Evaluator{
			Stages:   &fakeStages{stages: []*run.Stage{mkStage(0, run.StageTypePlan, run.StageStateAwaitingInput)}},
			Concerns: &fakeConcerns{},
			Audit:    &fakeAudit{},
		}
		res := evaluate(t, ev, wf, newRun())
		if res.ModelPolicy != block.ModelPolicy {
			t.Fatalf("ModelPolicy = %+v, want the policy surfaced on the parked path", res.ModelPolicy)
		}
	})

	t.Run("gate override wins wholesale", func(t *testing.T) {
		wfBlock := allKnobs()
		wfBlock.ModelPolicy = samplePolicy()
		gateBlock := &spec.OperatorAgent{
			MayWaive:    spec.ConditionSoloLow,
			ModelPolicy: &spec.ModelPolicy{Strategy: spec.ModelPolicyFollowPlanRecommendation},
		}
		wf := testWorkflow(wfBlock, gateBlock)
		ev := &Evaluator{
			Stages:   &fakeStages{stages: []*run.Stage{mkStage(0, run.StageTypePlan, run.StageStateAwaitingApproval)}},
			Concerns: &fakeConcerns{},
			Audit:    &fakeAudit{},
		}
		res := evaluate(t, ev, wf, newRun())
		if res.ModelPolicy != gateBlock.ModelPolicy {
			t.Fatalf("ModelPolicy = %+v, want the gate block's policy (wholesale override, not the workflow's)", res.ModelPolicy)
		}
	})

	t.Run("nil when the effective block declares none", func(t *testing.T) {
		wf := testWorkflow(allKnobs(), nil) // allKnobs sets no ModelPolicy
		ev := &Evaluator{
			Stages:   &fakeStages{stages: []*run.Stage{mkStage(0, run.StageTypePlan, run.StageStateSucceeded)}},
			Concerns: &fakeConcerns{},
			Audit:    &fakeAudit{},
		}
		res := evaluate(t, ev, wf, newRun())
		if res.ModelPolicy != nil {
			t.Fatalf("ModelPolicy = %+v, want nil for an absent model_policy", res.ModelPolicy)
		}
	})
}

// --- clean_dual_approval ------------------------------------------------------

func cleanDualEvaluator(stages []*run.Stage, open []*concern.Concern, au *fakeAudit) *Evaluator {
	return &Evaluator{Stages: &fakeStages{stages: stages}, Concerns: &fakeConcerns{open: open}, Audit: au}
}

func TestCleanDualApproval(t *testing.T) {
	planGated := []*run.Stage{mkStage(0, run.StageTypePlan, run.StageStateAwaitingApproval)}
	bothApprove := &fakeAudit{entries: map[string][]*audit.Entry{
		"plan_review_started": {startedEntry(1, 2)},
		"plan_reviewed":       {verdictEntry(2, planreview.VerdictApprove), verdictEntry(3, planreview.VerdictApprove)},
	}}

	tests := []struct {
		name       string
		stages     []*run.Stage
		open       []*concern.Concern
		audit      *fakeAudit
		wantMet    bool
		wantReason string
	}{
		{
			name:   "met: all verdicts approve, zero open concerns",
			stages: planGated, audit: bothApprove, wantMet: true,
		},
		{
			name:   "unmet: no stage awaiting approval",
			stages: []*run.Stage{mkStage(0, run.StageTypePlan, run.StageStateRunning)},
			audit:  bothApprove, wantReason: "no stage is awaiting approval",
		},
		{
			name:   "unmet: partial verdicts",
			stages: planGated,
			audit: &fakeAudit{entries: map[string][]*audit.Entry{
				"plan_review_started": {startedEntry(1, 2)},
				"plan_reviewed":       {verdictEntry(2, planreview.VerdictApprove)},
			}},
			wantReason: "1 of 2 reviewer verdicts received",
		},
		{
			name:       "unmet: round not dispatched",
			stages:     planGated,
			audit:      &fakeAudit{},
			wantReason: "0 of 2 reviewer verdicts received (review round not dispatched)",
		},
		{
			name:   "unmet: approve_with_concerns is not a clean approve",
			stages: planGated,
			audit: &fakeAudit{entries: map[string][]*audit.Entry{
				"plan_review_started": {startedEntry(1, 2)},
				"plan_reviewed": {
					verdictEntry(2, planreview.VerdictApprove),
					verdictEntry(3, planreview.VerdictApproveWithConcerns),
				},
			}},
			wantReason: "reviewer verdict approve_with_concerns",
		},
		{
			name:   "unmet: open concern",
			stages: planGated, audit: bothApprove,
			open:       []*concern.Concern{openConcern("medium")},
			wantReason: "1 open concern(s)",
		},
		{
			name:   "unmet: fixup re-review supersedes the settled first round",
			stages: planGated,
			audit: &fakeAudit{entries: map[string][]*audit.Entry{
				"plan_review_started": {startedEntry(1, 2), startedEntry(10, 2)},
				"plan_reviewed":       {verdictEntry(2, planreview.VerdictApprove), verdictEntry(3, planreview.VerdictApprove)},
			}},
			wantReason: "0 of 2 reviewer verdicts received",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wf := testWorkflow(&spec.OperatorAgent{MayApprove: spec.ConditionCleanDualApproval}, nil)
			ev := cleanDualEvaluator(tt.stages, tt.open, tt.audit)
			d := decisionFor(t, evaluate(t, ev, wf, newRun()), ActionApprove)
			if d.Met != tt.wantMet {
				t.Errorf("Met = %v, want %v (reason %q)", d.Met, tt.wantMet, d.UnmetReason)
			}
			if !tt.wantMet && !strings.Contains(d.UnmetReason, tt.wantReason) {
				t.Errorf("UnmetReason = %q, want it to contain %q", d.UnmetReason, tt.wantReason)
			}
			if !tt.wantMet && !strings.HasPrefix(d.UnmetReason, string(spec.ConditionCleanDualApproval)+":") {
				t.Errorf("UnmetReason = %q, want the condition-name prefix", d.UnmetReason)
			}
		})
	}
}

// TestCleanDualApproval_NoReviewersConfigured: a gate with no agent
// reviewers can never satisfy clean_dual_approval (fail-closed — the
// condition requires verdicts to exist).
func TestCleanDualApproval_NoReviewersConfigured(t *testing.T) {
	wf := testWorkflow(&spec.OperatorAgent{MayApprove: spec.ConditionCleanDualApproval}, nil)
	wf.Stages[0].Reviewers = nil
	ev := cleanDualEvaluator(
		[]*run.Stage{mkStage(0, run.StageTypePlan, run.StageStateAwaitingApproval)}, nil, &fakeAudit{})
	d := decisionFor(t, evaluate(t, ev, wf, newRun()), ActionApprove)
	if d.Met || !strings.Contains(d.UnmetReason, "no agent reviewers configured") {
		t.Errorf("decision = %+v, want unmet naming the missing reviewers", d)
	}
}

// --- convergent_concerns -----------------------------------------------------

func TestConvergentConcerns(t *testing.T) {
	settledWithConcerns := &fakeAudit{entries: map[string][]*audit.Entry{
		"implement_review_started": {startedEntry(1, 2)},
		"implement_reviewed": {
			verdictEntry(2, planreview.VerdictApprove),
			verdictEntry(3, planreview.VerdictApproveWithConcerns),
		},
	}}
	rejectAudit := &fakeAudit{entries: map[string][]*audit.Entry{
		"implement_review_started": {startedEntry(1, 2)},
		"implement_reviewed": {
			verdictEntry(2, planreview.VerdictApprove),
			verdictEntry(3, planreview.VerdictReject),
		},
	}}
	// advisory is the default testWorkflow implement-stage authority
	// (agent:2, human:1 → AuthorityAdvisory); gating overrides to
	// agent-only (agent:1, human:0 → AuthorityGating).
	advisory := &spec.ReviewersConfig{Agent: 2, Human: 1}
	gating := &spec.ReviewersConfig{Agent: 1, Human: 0}
	tests := []struct {
		name        string
		reviewers   *spec.ReviewersConfig // implement-stage reviewers; nil = default advisory
		minSeverity string                // route_fixup_min_severity knob; "" = absent (default medium)
		open        []*concern.Concern
		audit       *fakeAudit
		wantMet     bool
		wantReason  string
	}{
		{
			name:  "advisory met: verdicts in, no reject, one medium concern at default threshold",
			open:  []*concern.Concern{openConcern("medium")},
			audit: settledWithConcerns, wantMet: true,
		},
		{
			// #1964: the run-6ecc1090 shape — dual approve with a single
			// low-severity concern parks at the default medium threshold
			// instead of auto-routing a full fix-up pass.
			name:       "unmet: dual approve + solo low concern parks at default medium threshold",
			open:       []*concern.Concern{openConcern("low")},
			audit:      settledWithConcerns,
			wantReason: "below the route_fixup_min_severity threshold (medium)",
		},
		{
			// #1964: a mixed-severity, no-reject round where the
			// max-aggregation itself decides routing — the medium concern
			// lifts maxRank to the default threshold while the low one alone
			// would park, so the round routes. Exercises the max loop on the
			// threshold path (not the reject-bypass path).
			name:  "met: mixed low+medium concerns route via max-aggregation at default threshold",
			open:  []*concern.Concern{openConcern("low"), openConcern("medium")},
			audit: settledWithConcerns, wantMet: true,
		},
		{
			// The mirror of the above on the park side: two below-threshold
			// concerns keep maxRank under the default medium bar, so the
			// max loop parks rather than routing.
			name:       "unmet: mixed low+low concerns park at default medium threshold",
			open:       []*concern.Concern{openConcern("low"), openConcern("low")},
			audit:      settledWithConcerns,
			wantReason: "below the route_fixup_min_severity threshold (medium)",
		},
		{
			// route_fixup_min_severity: low restores the legacy
			// route-on-any-concern behavior.
			name:        "met: threshold low routes a solo low concern (legacy behavior)",
			minSeverity: "low",
			open:        []*concern.Concern{openConcern("low")},
			audit:       settledWithConcerns, wantMet: true,
		},
		{
			// A stricter threshold parks a concern the default would route.
			name:        "unmet: threshold high parks a medium concern",
			minSeverity: "high",
			open:        []*concern.Concern{openConcern("medium")},
			audit:       settledWithConcerns,
			wantReason:  "below the route_fixup_min_severity threshold (high)",
		},
		{
			// A reject verdict (advisory authority) BYPASSES the threshold:
			// arbitration stays met even with only low concerns.
			name:    "advisory met: reject bypasses the threshold with all-low concerns",
			open:    []*concern.Concern{openConcern("low"), openConcern("low")},
			audit:   rejectAudit,
			wantMet: true,
		},
		{
			// A concern with an unrecognized severity ranks below low
			// (rank 0) and parks rather than spending a fix-up (fail-closed).
			name:       "unmet: unrecognized concern severity parks at default threshold",
			open:       []*concern.Concern{openConcern("cosmetic")},
			audit:      settledWithConcerns,
			wantReason: "below the route_fixup_min_severity threshold (medium)",
		},
		{
			// An out-of-enum route_fixup_min_severity value (reachable only
			// via campaign-override bytes) defaults defensively to medium,
			// so a medium concern still routes.
			name:        "met: out-of-enum threshold value defaults to medium",
			minSeverity: "cosmetic",
			open:        []*concern.Concern{openConcern("medium")},
			audit:       settledWithConcerns, wantMet: true,
		},
		{
			name:       "unmet: no review round",
			open:       []*concern.Concern{openConcern("medium")},
			audit:      &fakeAudit{},
			wantReason: "no implement review round recorded",
		},
		{
			name: "unmet: partial verdicts",
			open: []*concern.Concern{openConcern("medium")},
			audit: &fakeAudit{entries: map[string][]*audit.Entry{
				"implement_review_started": {startedEntry(1, 2)},
				"implement_reviewed":       {verdictEntry(2, planreview.VerdictApproveWithConcerns)},
			}},
			wantReason: "1 of 2 reviewer verdicts received",
		},
		{
			// Advisory authority (agent+human): an agent reject is
			// arbitrable, not a hard page. With an open concern the
			// condition stays met so route_fixup auto-arbitrates.
			name:    "advisory met: agent reject is arbitrable with an open concern",
			open:    []*concern.Concern{openConcern("medium")},
			audit:   rejectAudit,
			wantMet: true,
		},
		{
			// Gating authority (agent-only): an agent reject is a hard
			// reviewer_reject page — route_fixup is disqualified.
			name:       "gating unmet: agent reject pages the human (reviewer_reject)",
			reviewers:  gating,
			open:       []*concern.Concern{openConcern("medium")},
			audit:      rejectAudit,
			wantReason: "reviewer_reject",
		},
		{
			// Gating no-reject regression: the convergent path is
			// otherwise unchanged — verdicts in, no reject, open concern.
			name:      "gating met: no reject, one open concern",
			reviewers: gating,
			open:      []*concern.Concern{openConcern("medium")},
			audit: &fakeAudit{entries: map[string][]*audit.Entry{
				"implement_review_started": {startedEntry(1, 1)},
				"implement_reviewed":       {verdictEntry(2, planreview.VerdictApproveWithConcerns)},
			}},
			wantMet: true,
		},
		{
			name:       "unmet: zero open concerns to route",
			audit:      settledWithConcerns,
			wantReason: "0 open concerns to route",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wf := testWorkflow(&spec.OperatorAgent{
				MayRouteFixup:         spec.ConditionConvergentConcerns,
				RouteFixupMinSeverity: tt.minSeverity,
			}, nil)
			reviewers := advisory
			if tt.reviewers != nil {
				reviewers = tt.reviewers
			}
			implementStage(wf).Reviewers = reviewers
			ev := cleanDualEvaluator(nil, tt.open, tt.audit)
			d := decisionFor(t, evaluate(t, ev, wf, newRun()), ActionRouteFixup)
			if d.Met != tt.wantMet {
				t.Errorf("Met = %v, want %v (reason %q)", d.Met, tt.wantMet, d.UnmetReason)
			}
			if !tt.wantMet && !strings.Contains(d.UnmetReason, tt.wantReason) {
				t.Errorf("UnmetReason = %q, want it to contain %q", d.UnmetReason, tt.wantReason)
			}
		})
	}
}

// implementStage returns the implement-stage definition in a testWorkflow.
func implementStage(wf *spec.Workflow) *spec.Stage {
	for i := range wf.Stages {
		if wf.Stages[i].Type == spec.StageTypeImplement {
			return &wf.Stages[i]
		}
	}
	panic("testWorkflow has no implement stage")
}

// TestConvergentConcerns_GatelessReviewersGuard exercises
// implementReviewAuthority's nil-Reviewers guard: a stage with no
// reviewers block resolves to gateless authority, so a reject is NOT a
// gating reject and does not disqualify route_fixup — with an open
// concern the condition stays met. Fail-closed in the sense that the
// gateless path can never fire the reviewer_reject page (no agent
// authority gates the verdict).
func TestConvergentConcerns_GatelessReviewersGuard(t *testing.T) {
	wf := testWorkflow(&spec.OperatorAgent{MayRouteFixup: spec.ConditionConvergentConcerns}, nil)
	implementStage(wf).Reviewers = nil
	au := &fakeAudit{entries: map[string][]*audit.Entry{
		"implement_review_started": {startedEntry(1, 2)},
		"implement_reviewed": {
			verdictEntry(2, planreview.VerdictApprove),
			verdictEntry(3, planreview.VerdictReject),
		},
	}}
	ev := cleanDualEvaluator(nil, []*concern.Concern{openConcern("medium")}, au)
	d := decisionFor(t, evaluate(t, ev, wf, newRun()), ActionRouteFixup)
	if !d.Met {
		t.Errorf("Met = false (reason %q), want true: a gateless reject is not a gating reject", d.UnmetReason)
	}
}

// --- reviewer_reject_class (#1378) -------------------------------------------

// TestReviewerRejectClass_PerAuthority asserts the resolved reviewer-reject
// page-event class surfaced on Result for each of the three implement-review
// authority modes (the enumerated failure/branch set of reviewerRejectClass).
func TestReviewerRejectClass_PerAuthority(t *testing.T) {
	tests := []struct {
		name      string
		reviewers *spec.ReviewersConfig
		want      string
	}{
		{"gating: agent-only", &spec.ReviewersConfig{Agent: 1, Human: 0}, spec.PageEventGatingReviewerReject},
		{"advisory: agent+human", &spec.ReviewersConfig{Agent: 2, Human: 1}, spec.PageEventAdvisoryReviewerReject},
		{"gateless: nil reviewers", nil, ""},
		{"gateless: zero agents", &spec.ReviewersConfig{Agent: 0, Human: 1}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wf := testWorkflow(allKnobs(), nil)
			implementStage(wf).Reviewers = tt.reviewers
			ev := cleanDualEvaluator(nil, nil, &fakeAudit{})
			res := evaluate(t, ev, wf, newRun())
			if res == nil {
				t.Fatal("Result is nil; want a configured evaluation")
			}
			if res.ReviewerRejectClass != tt.want {
				t.Errorf("ReviewerRejectClass = %q, want %q", res.ReviewerRejectClass, tt.want)
			}
		})
	}
}

// TestReviewerRejectClass_ParkedAwaitingInput asserts the class is also
// populated on the parked awaiting_input return path, so the surfaced
// class is consistent regardless of run state.
func TestReviewerRejectClass_ParkedAwaitingInput(t *testing.T) {
	wf := testWorkflow(allKnobs(), nil)
	implementStage(wf).Reviewers = &spec.ReviewersConfig{Agent: 1, Human: 0} // gating
	ev := &Evaluator{
		Stages:   &fakeStages{stages: []*run.Stage{mkStage(0, run.StageTypePlan, run.StageStateAwaitingInput)}},
		Concerns: &fakeConcerns{},
		Audit:    &fakeAudit{},
	}
	res := evaluate(t, ev, wf, newRun())
	if res == nil {
		t.Fatal("Result is nil; want the must_page_human envelope while parked")
	}
	if res.ReviewerRejectClass != spec.PageEventGatingReviewerReject {
		t.Errorf("ReviewerRejectClass = %q, want %q on the parked path", res.ReviewerRejectClass, spec.PageEventGatingReviewerReject)
	}
}

// TestReviewerRejectClass_LegacyTokenBackCompat is the done-means
// no-behavior-change assertion: a config whose must_page_human lists the
// legacy bare reviewer_reject produces the IDENTICAL route_fixup outcome
// (met/unmet + reason) and the IDENTICAL resolved reviewer-reject class as
// a config listing the explicit gating_reviewer_reject. The page/auto
// decision stays authority-resolved; the explicit token only makes the
// class legible.
func TestReviewerRejectClass_LegacyTokenBackCompat(t *testing.T) {
	rejectAudit := &fakeAudit{entries: map[string][]*audit.Entry{
		"implement_review_started": {startedEntry(1, 1)},
		"implement_reviewed":       {verdictEntry(2, planreview.VerdictReject)},
	}}
	open := []*concern.Concern{openConcern("medium")}

	build := func(pageEvent string) *Result {
		wf := testWorkflow(&spec.OperatorAgent{
			MayRouteFixup: spec.ConditionConvergentConcerns,
			MustPageHuman: []string{pageEvent},
		}, nil)
		implementStage(wf).Reviewers = &spec.ReviewersConfig{Agent: 1, Human: 0} // gating
		ev := cleanDualEvaluator(nil, open, rejectAudit)
		return evaluate(t, ev, wf, newRun())
	}

	legacy := build(spec.PageEventReviewerReject)
	explicit := build(spec.PageEventGatingReviewerReject)

	legacyFixup := decisionFor(t, legacy, ActionRouteFixup)
	explicitFixup := decisionFor(t, explicit, ActionRouteFixup)
	if legacyFixup.Met != explicitFixup.Met || legacyFixup.UnmetReason != explicitFixup.UnmetReason {
		t.Errorf("route_fixup outcome differs: legacy %+v vs explicit %+v", legacyFixup, explicitFixup)
	}
	if legacyFixup.Met {
		t.Errorf("route_fixup Met = true, want false: a gating reject pages the human")
	}
	if legacy.ReviewerRejectClass != explicit.ReviewerRejectClass {
		t.Errorf("ReviewerRejectClass differs: legacy %q vs explicit %q", legacy.ReviewerRejectClass, explicit.ReviewerRejectClass)
	}
	if legacy.ReviewerRejectClass != spec.PageEventGatingReviewerReject {
		t.Errorf("ReviewerRejectClass = %q, want %q (legacy reviewer_reject resolves to the gating sense)", legacy.ReviewerRejectClass, spec.PageEventGatingReviewerReject)
	}
}

// TestReviewerRejectClass_LegacyV1TokenThroughDelegation is the end-to-end
// preservation proof for E52.3 / #2215. workflow-v2 REMOVED the bare
// reviewer_reject page-event token, but workflow-v0 and workflow-v1 still
// accept it and the shared Go constant is retained for exactly that reason
// — so the claim that needs proving is not "the constant still exists" but
// "a real v1 DOCUMENT carrying the bare token still resolves to the gating
// class through the delegation consumer".
//
// It closes the gap the previous coverage left open by splitting the claim
// across two half-proofs: a parse-only assertion in the spec package (the
// token survives ParseBytes) and a hand-built-struct assertion here (the
// mapping is gating). Neither alone shows the two halves still meet. This
// drives real v1 YAML through spec.ParseBytes and hands the PARSED workflow
// to the Evaluator.
func TestReviewerRejectClass_LegacyV1TokenThroughDelegation(t *testing.T) {
	s, err := spec.ParseBytes([]byte(`version: "1.6"
workflows:
  feature_change:
    operator_agent:
      may_route_fixup: convergent_concerns
      must_page_human:
        - reviewer_reject
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        reviewers:
          agent: 1
          human: 0
`))
	if err != nil {
		t.Fatalf("spec.ParseBytes(v1 with the legacy bare token): %v", err)
	}
	wfv := s.Workflows["feature_change"]
	wf := &wfv

	// The legacy token must survive the parse into the shared type — a v1
	// document is still entitled to it.
	if wf.OperatorAgent == nil {
		t.Fatal("parsed OperatorAgent = nil, want the declared block")
	}
	if got := wf.OperatorAgent.MustPageHuman; len(got) != 1 || got[0] != spec.PageEventReviewerReject {
		t.Fatalf("parsed MustPageHuman = %v, want [%s]", got, spec.PageEventReviewerReject)
	}
	// And the reviewers block must still carry the bare integer count that
	// v2 removed — the input implementReviewAuthority resolves on.
	st := implementStageOf(t, wf)
	if st.Reviewers == nil || st.Reviewers.Agent != 1 || st.Reviewers.Human != 0 {
		t.Fatalf("parsed implement Reviewers = %+v, want {Agent:1, Human:0}", st.Reviewers)
	}

	ev := cleanDualEvaluator(nil, []*concern.Concern{openConcern("medium")}, &fakeAudit{entries: map[string][]*audit.Entry{
		"implement_review_started": {startedEntry(1, 1)},
		"implement_reviewed":       {verdictEntry(2, planreview.VerdictReject)},
	}})
	res := evaluate(t, ev, wf, newRun())

	if res.ReviewerRejectClass != spec.PageEventGatingReviewerReject {
		t.Errorf("ReviewerRejectClass = %q, want %q (a v1 bare reviewer_reject still resolves to the gating sense)",
			res.ReviewerRejectClass, spec.PageEventGatingReviewerReject)
	}
	if len(res.MustPageHuman) != 1 || res.MustPageHuman[0] != spec.PageEventReviewerReject {
		t.Errorf("MustPageHuman = %v, want the legacy token surfaced verbatim", res.MustPageHuman)
	}
	if d := decisionFor(t, res, ActionRouteFixup); d.Met {
		t.Errorf("route_fixup Met = true, want false: a gating reject pages the human")
	}
}

// TestImplementReviewAuthority_NilReviewersBlockFromV2Spec is the RESOLVED
// half of #2215 acceptance criterion 4, which says a spec with no reviewers
// block "still defaults to {human: 1}". No consumer materializes that
// literal: Stage.Reviewers stays nil and implementReviewAuthority maps nil
// straight to gateless. What is TRUE — and what this asserts — is that nil
// and {Human:1} are OBSERVATIONALLY EQUIVALENT here, because with zero agent
// reviewers the human count cannot change the resolved mode. So the
// documented default is unobservable rather than applied, and this slice
// does not change that either way.
func TestImplementReviewAuthority_NilReviewersBlockFromV2Spec(t *testing.T) {
	s, err := spec.ParseBytes([]byte(`version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`))
	if err != nil {
		t.Fatalf("spec.ParseBytes(v2 without a reviewers block): %v", err)
	}
	wfv := s.Workflows["feature_change"]
	wf := &wfv
	if st := implementStageOf(t, wf); st.Reviewers != nil {
		t.Fatalf("parsed implement Reviewers = %+v, want nil for an absent block", st.Reviewers)
	}
	if got := implementReviewAuthority(wf); got != planreview.AuthorityGateless {
		t.Errorf("implementReviewAuthority(nil block) = %q, want %q", got, planreview.AuthorityGateless)
	}
	// The documented {human:1} default resolves identically, which is why
	// the discrepancy has never been observable.
	withDefault := wfv
	withDefault.Stages = append([]spec.Stage(nil), wf.Stages...)
	implementStageOf(t, &withDefault).Reviewers = &spec.ReviewersConfig{Human: 1}
	if got := implementReviewAuthority(&withDefault); got != planreview.AuthorityGateless {
		t.Errorf("implementReviewAuthority({human:1}) = %q, want %q — it must stay indistinguishable from nil", got, planreview.AuthorityGateless)
	}
	if reviewerRejectClass(wf) != reviewerRejectClass(&withDefault) {
		t.Errorf("reviewerRejectClass differs between nil (%q) and {human:1} (%q)",
			reviewerRejectClass(wf), reviewerRejectClass(&withDefault))
	}
}

// implementStageOf returns the workflow's implement stage, failing the test
// if there is none. Unlike implementStage it takes a parsed workflow, so a
// fixture that drifts fails loudly instead of panicking.
func implementStageOf(t *testing.T, wf *spec.Workflow) *spec.Stage {
	t.Helper()
	for i := range wf.Stages {
		if wf.Stages[i].Type == spec.StageTypeImplement {
			return &wf.Stages[i]
		}
	}
	t.Fatal("parsed workflow has no implement stage")
	return nil
}

// TestConvergentConcerns_GatingRejectReasonNamesClass asserts the gating
// unmet-reason names the explicit gating_reviewer_reject class so a reader
// need not cross-reference the authority resolver.
func TestConvergentConcerns_GatingRejectReasonNamesClass(t *testing.T) {
	wf := testWorkflow(&spec.OperatorAgent{MayRouteFixup: spec.ConditionConvergentConcerns}, nil)
	implementStage(wf).Reviewers = &spec.ReviewersConfig{Agent: 1, Human: 0} // gating
	au := &fakeAudit{entries: map[string][]*audit.Entry{
		"implement_review_started": {startedEntry(1, 1)},
		"implement_reviewed":       {verdictEntry(2, planreview.VerdictReject)},
	}}
	ev := cleanDualEvaluator(nil, []*concern.Concern{openConcern("medium")}, au)
	d := decisionFor(t, evaluate(t, ev, wf, newRun()), ActionRouteFixup)
	if d.Met {
		t.Fatalf("Met = true, want false for a gating reject")
	}
	if !strings.Contains(d.UnmetReason, spec.PageEventGatingReviewerReject) {
		t.Errorf("UnmetReason = %q, want it to name %q", d.UnmetReason, spec.PageEventGatingReviewerReject)
	}
}

// --- solo_low ----------------------------------------------------------------

func TestSoloLow(t *testing.T) {
	tests := []struct {
		name       string
		open       []*concern.Concern
		wantMet    bool
		wantReason string
	}{
		{name: "met: exactly one low concern", open: []*concern.Concern{openConcern("low")}, wantMet: true},
		{name: "unmet: zero open concerns", wantReason: "0 open concerns"},
		{
			name:       "unmet: two open concerns",
			open:       []*concern.Concern{openConcern("low"), openConcern("low")},
			wantReason: "2 open concerns",
		},
		{
			name:       "unmet: severity above low",
			open:       []*concern.Concern{openConcern("medium")},
			wantReason: "severity is medium",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wf := testWorkflow(&spec.OperatorAgent{MayWaive: spec.ConditionSoloLow}, nil)
			ev := cleanDualEvaluator(nil, tt.open, &fakeAudit{})
			d := decisionFor(t, evaluate(t, ev, wf, newRun()), ActionWaive)
			if d.Met != tt.wantMet {
				t.Errorf("Met = %v, want %v (reason %q)", d.Met, tt.wantMet, d.UnmetReason)
			}
			if !tt.wantMet && !strings.Contains(d.UnmetReason, tt.wantReason) {
				t.Errorf("UnmetReason = %q, want it to contain %q", d.UnmetReason, tt.wantReason)
			}
		})
	}
}

// --- infra_flake -------------------------------------------------------------

// realFlakeOutput is the verbatim #972 testcontainers start-timeout
// failure the runner's isTestcontainersStartFlake table test pins —
// the REAL emitted shape, not an assumption: a category-A verify
// exhaustion embeds the verify output into the stage's FailureReason
// via "verify command %q still failing after %d iteration(s):\n<out>"
// (runner/cmd/fishhawk-runner/main.go::runVerifyFixLoop).
const realFlakeOutput = `--- FAIL: TestPostgres_AppendChained (12.41s)
    postgres_test.go:135: start postgres: run postgres: generic container: start container: started hook: wait until ready: mapped port: check target: retries: 9, port: "invalid port", last err: get state: Get "http://%2Fvar%2Frun%2Fdocker.sock/v1.54/containers/dd45dc0863d386b8e4a5e6a6a0829b4be99e4b5da54e667a192f6a142dfe5baf/json": context deadline exceeded
FAIL`

func realFlakeFailureReason() string {
	return fmt.Sprintf("verify command %q still failing after %d iteration(s):\n%s", "scripts/test", 1, realFlakeOutput)
}

func TestInfraFlake(t *testing.T) {
	tests := []struct {
		name       string
		stages     []*run.Stage
		wantMet    bool
		wantReason string
	}{
		{
			name:    "met: category-A failure embedding the real #972 flake output",
			stages:  []*run.Stage{failedStage(1, run.FailureA, realFlakeFailureReason())},
			wantMet: true,
		},
		{
			name:    "met: failure reason citing the trace event by name",
			stages:  []*run.Stage{failedStage(1, run.FailureA, "verify aborted after verify_infra_flake_retry")},
			wantMet: true,
		},
		{
			name:       "unmet: no failed stage",
			stages:     []*run.Stage{mkStage(1, run.StageTypeImplement, run.StageStateRunning)},
			wantReason: "no failed stage",
		},
		{
			name:       "unmet: category B is not retryable as a flake",
			stages:     []*run.Stage{failedStage(1, run.FailureB, realFlakeFailureReason())},
			wantReason: "failed stage category is B",
		},
		{
			name: "unmet: ordinary deadline mention without container markers",
			stages: []*run.Stage{failedStage(1, run.FailureA,
				"verify command \"scripts/test\" still failing after 1 iteration(s):\nGet \"http://example.com\": context deadline exceeded")},
			wantReason: "no infra-flake signature",
		},
		{
			name:       "unmet: plain category-A agent failure",
			stages:     []*run.Stage{failedStage(1, run.FailureA, "agent invocation failed (no reason supplied)")},
			wantReason: "no infra-flake signature",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wf := testWorkflow(&spec.OperatorAgent{MayRetry: spec.ConditionInfraFlake}, nil)
			ev := cleanDualEvaluator(tt.stages, nil, &fakeAudit{})
			d := decisionFor(t, evaluate(t, ev, wf, newRun()), ActionRetry)
			if d.Met != tt.wantMet {
				t.Errorf("Met = %v, want %v (reason %q)", d.Met, tt.wantMet, d.UnmetReason)
			}
			if !tt.wantMet && !strings.Contains(d.UnmetReason, tt.wantReason) {
				t.Errorf("UnmetReason = %q, want it to contain %q", d.UnmetReason, tt.wantReason)
			}
		})
	}
}

// --- gates_resolved_ci_green ---------------------------------------------------

func checksGreenEntry(seq int64) *audit.Entry {
	return entry(seq, drive.Advance{Rule: drive.RuleChecksGreenAwaitingMerge, From: "review:awaiting_approval", To: "awaiting_merge"})
}

func TestGatesResolvedCIGreen(t *testing.T) {
	pr := "https://github.com/x/y/pull/7"
	greenAudit := func() *fakeAudit {
		return &fakeAudit{entries: map[string][]*audit.Entry{
			drive.Category: {checksGreenEntry(9)},
		}}
	}
	tests := []struct {
		name       string
		prURL      *string
		stages     []*run.Stage
		open       []*concern.Concern
		audit      *fakeAudit
		wantMet    bool
		wantReason string
	}{
		{
			name:  "met: checks green, PR open, no pending gate, no concerns",
			prURL: &pr, audit: greenAudit(), wantMet: true,
		},
		{
			name:  "unmet: no auto-advance recorded",
			prURL: &pr, audit: &fakeAudit{},
			wantReason: "no checks_green_awaiting_merge auto-advance recorded",
		},
		{
			name:  "unmet: checks_green superseded by a later transition",
			prURL: &pr,
			audit: &fakeAudit{entries: map[string][]*audit.Entry{
				drive.Category: {
					checksGreenEntry(5),
					entry(8, drive.Advance{Rule: drive.RuleFixupRereviewRepark, From: "review:awaiting_approval", To: "review:pending"}),
				},
			}},
			wantReason: "not checks_green_awaiting_merge",
		},
		{
			name:  "unmet: no PR on the run row",
			audit: greenAudit(), wantReason: "no pull request recorded",
		},
		{
			name:       "unmet: a stage is still awaiting approval",
			prURL:      &pr,
			stages:     []*run.Stage{mkStage(1, run.StageTypeImplement, run.StageStateAwaitingApproval)},
			audit:      greenAudit(),
			wantReason: "still awaiting approval",
		},
		{
			name:  "unmet: open concern",
			prURL: &pr,
			open:  []*concern.Concern{openConcern("low")},
			audit: greenAudit(), wantReason: "1 open concern(s)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wf := testWorkflow(&spec.OperatorAgent{MayMerge: spec.ConditionGatesResolvedCIGreen}, nil)
			ev := cleanDualEvaluator(tt.stages, tt.open, tt.audit)
			runRow := newRun()
			runRow.PullRequestURL = tt.prURL
			d := decisionFor(t, evaluate(t, ev, wf, runRow), ActionMerge)
			if d.Met != tt.wantMet {
				t.Errorf("Met = %v, want %v (reason %q)", d.Met, tt.wantMet, d.UnmetReason)
			}
			if !tt.wantMet && !strings.Contains(d.UnmetReason, tt.wantReason) {
				t.Errorf("UnmetReason = %q, want it to contain %q", d.UnmetReason, tt.wantReason)
			}
		})
	}
}

// --- error propagation ---------------------------------------------------------

// TestEvaluate_RepoFailuresPropagate: a repository read failure is an
// error, never a partial or fabricated answer — the caller owns the
// best-effort omit.
func TestEvaluate_RepoFailuresPropagate(t *testing.T) {
	wf := testWorkflow(allKnobs(), nil)
	boom := errors.New("store down")

	tests := []struct {
		name string
		ev   *Evaluator
	}{
		{"stage list failure", &Evaluator{Stages: &fakeStages{err: boom}, Concerns: &fakeConcerns{}, Audit: &fakeAudit{}}},
		{"concern list failure", &Evaluator{Stages: &fakeStages{}, Concerns: &fakeConcerns{err: boom}, Audit: &fakeAudit{}}},
		{"audit list failure", &Evaluator{Stages: &fakeStages{}, Concerns: &fakeConcerns{}, Audit: &fakeAudit{err: boom}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.ev.Evaluate(context.Background(), newRun(), wf, nil); !errors.Is(err, boom) {
				t.Errorf("Evaluate error = %v, want the injected store failure", err)
			}
		})
	}
}

// --- Decision lookup helper (E25.6 / ADR-047) ---------------------------------

// allKnobsEvaluator wires a gated-plan-stage evaluation so every may_*
// knob in allKnobs() produces a Decision the lookup helper can resolve.
func allKnobsEvaluator() (*Evaluator, *spec.Workflow, *run.Run) {
	wf := testWorkflow(allKnobs(), nil)
	ev := &Evaluator{
		Stages:   &fakeStages{stages: []*run.Stage{mkStage(0, run.StageTypePlan, run.StageStateAwaitingApproval)}},
		Concerns: &fakeConcerns{open: []*concern.Concern{openConcern("low")}},
		Audit:    &fakeAudit{},
	}
	return ev, wf, newRun()
}

// TestResultDecision_PerKnob: the lookup returns the right Decision for
// every configured knob and false for an action with no knob.
func TestResultDecision_PerKnob(t *testing.T) {
	ev, wf, runRow := allKnobsEvaluator()
	res := evaluate(t, ev, wf, runRow)

	for _, action := range []string{ActionApprove, ActionRouteFixup, ActionWaive, ActionRetry, ActionMerge} {
		d, ok := res.Decision(action)
		if !ok {
			t.Errorf("Decision(%q) ok = false, want true (knob configured)", action)
			continue
		}
		if d.Action != action {
			t.Errorf("Decision(%q).Action = %q, want %q", action, d.Action, action)
		}
		// The returned Decision must match the one in Actions verbatim.
		if want := decisionFor(t, res, action); d != want {
			t.Errorf("Decision(%q) = %+v, want %+v", action, d, want)
		}
	}

	if d, ok := res.Decision("not_a_knob"); ok {
		t.Errorf("Decision(unknown) = (%+v, true), want (zero, false)", d)
	}
}

// TestResultDecision_NilReceiver: a nil Result is "nothing delegated".
func TestResultDecision_NilReceiver(t *testing.T) {
	var res *Result
	if d, ok := res.Decision(ActionApprove); ok || d != (Decision{}) {
		t.Errorf("(*Result)(nil).Decision = (%+v, %v), want (zero, false)", d, ok)
	}
}

// TestResultDecision_ReadOnlyOverEvaluate is the slice's required
// assertion (binding plan step 5): the helper does not mutate the Result,
// and two Evaluate calls over the same state return identical Results — so
// the actor consulting the helper observes the same answer the advisory
// read surface does, with no side effects.
func TestResultDecision_ReadOnlyOverEvaluate(t *testing.T) {
	ev, wf, runRow := allKnobsEvaluator()

	res1, err := ev.Evaluate(context.Background(), runRow, wf, nil)
	if err != nil {
		t.Fatalf("Evaluate #1: %v", err)
	}
	// Snapshot before any helper call.
	before := *res1
	beforeActions := append([]Decision(nil), res1.Actions...)

	// Exercising the lookup must not mutate the Result.
	for _, action := range []string{ActionApprove, ActionRouteFixup, ActionWaive, ActionRetry, ActionMerge, "missing"} {
		res1.Decision(action)
	}
	if !reflect.DeepEqual(*res1, before) || !reflect.DeepEqual(res1.Actions, beforeActions) {
		t.Errorf("Decision mutated the Result:\n before = %+v\n after  = %+v", before, *res1)
	}

	// A second Evaluate over the same state returns an identical Result —
	// Evaluate (and therefore the helper that reads its output) is
	// read-only over run state.
	res2, err := ev.Evaluate(context.Background(), runRow, wf, nil)
	if err != nil {
		t.Fatalf("Evaluate #2: %v", err)
	}
	if !reflect.DeepEqual(res1, res2) {
		t.Errorf("two Evaluate calls diverged:\n first  = %+v\n second = %+v", res1, res2)
	}
}

// TestMergeCondition: the exposed merge condition is the may_merge knob's.
func TestMergeCondition(t *testing.T) {
	if got := MergeCondition(); got != spec.ConditionGatesResolvedCIGreen {
		t.Errorf("MergeCondition() = %q, want %q", got, spec.ConditionGatesResolvedCIGreen)
	}
}

// --- workflow-v2 autonomy matrix (ADR-066 / E52.10 / #2222) -----------------

// v2WorkflowYAML renders a workflow-v2 document whose workflow-level
// autonomy block is the caller's, so each test declares exactly the grammar
// it is about and the DERIVED OperatorAgent under test comes from the real
// parse pipeline rather than a hand-built struct.
func v2WorkflowYAML(autonomyBlock string) string {
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

// parseV2Workflow parses the rendered v2 document and returns the derived
// feature_change workflow.
func parseV2Workflow(t *testing.T, autonomyBlock string) *spec.Workflow {
	t.Helper()
	parsed, err := spec.ParseBytes([]byte(v2WorkflowYAML(autonomyBlock)))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	wf, ok := parsed.Workflows["feature_change"]
	if !ok {
		t.Fatal("feature_change absent from parsed spec")
	}
	return &wf
}

// matrixEntry returns the resolved matrix entry for a class, failing when
// absent.
func matrixEntry(t *testing.T, res *Result, class string) spec.ResolvedAction {
	t.Helper()
	e, ok := res.MatrixEntry(class)
	if !ok {
		t.Fatalf("no matrix entry for class %q in %+v", class, res.Matrix)
	}
	return e
}

// TestEvaluate_V2Matrix_TierAndProvenance: a v2 workflow's resolved tier and
// per-class provenance ride the Result, and the DECISION SET stays keyed to
// what is actually delegated — the explicitly gated `approve` class
// contributes a matrix entry but NO decision.
func TestEvaluate_V2Matrix_TierAndProvenance(t *testing.T) {
	wf := parseV2Workflow(t, `    autonomy: medium
    actions:
      approve:
        mode: gated`)
	ev := &Evaluator{
		Stages:   &fakeStages{stages: []*run.Stage{mkStage(0, run.StageTypePlan, run.StageStateAwaitingApproval)}},
		Concerns: &fakeConcerns{open: []*concern.Concern{openConcern("low")}},
		Audit:    &fakeAudit{},
	}
	res := evaluate(t, ev, wf, newRun())

	if res.Tier != spec.TierMedium {
		t.Errorf("Tier = %q, want %q", res.Tier, spec.TierMedium)
	}
	want := map[string]struct {
		mode   spec.ActionMode
		source spec.ResolutionSource
	}{
		spec.ActionApprove: {spec.ModeGated, spec.SourceExplicit},
		spec.ActionFixup:   {spec.ModeAuto, spec.SourceTier},
		spec.ActionRetry:   {spec.ModeAuto, spec.SourceTier},
		spec.ActionWaive:   {spec.ModeGated, spec.SourceTier},
		spec.ActionMerge:   {spec.ModeGated, spec.SourceTier},
	}
	for class, w := range want {
		got := matrixEntry(t, res, class)
		if got.Mode != w.mode || got.Source != w.source {
			t.Errorf("matrix[%s] = mode %q source %q, want mode %q source %q",
				class, got.Mode, got.Source, w.mode, w.source)
		}
	}

	// The decision set is what is DELEGATED: approve is explicitly gated, so
	// it must not appear even though the tier would have delegated it.
	if _, ok := res.Decision(ActionApprove); ok {
		t.Errorf("approve has a decision; explicit `mode: gated` must delegate nothing (actions = %+v)", res.Actions)
	}
	d, ok := res.Decision(ActionRouteFixup)
	if !ok {
		t.Fatalf("no route_fixup decision; medium delegates fixup (actions = %+v)", res.Actions)
	}
	if d.Mode != spec.ModeAuto || d.Source != spec.SourceTier {
		t.Errorf("route_fixup decision mode/source = %q/%q, want auto/tier", d.Mode, d.Source)
	}
}

// TestEvaluate_CampaignOverride_MatrixProjection: a campaign-level
// operator_agent override is projected onto a matrix with NO tier and every
// class Source=explicit — the campaign named the whole block itself.
func TestEvaluate_CampaignOverride_MatrixProjection(t *testing.T) {
	wf := testWorkflow(nil, nil)
	ev := &Evaluator{
		Stages:   &fakeStages{stages: []*run.Stage{mkStage(0, run.StageTypePlan, run.StageStateAwaitingApproval)}},
		Concerns: &fakeConcerns{open: []*concern.Concern{openConcern("low")}},
		Audit:    &fakeAudit{},
	}
	override := &spec.OperatorAgent{MayWaive: spec.ConditionSoloLow, MustPageHuman: []string{spec.PageEventPlanRejection}}
	res, err := ev.Evaluate(context.Background(), newRun(), wf, override)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if res.Tier != "" {
		t.Errorf("Tier = %q, want empty (an override is a delegation input, not a grammar version)", res.Tier)
	}
	for _, class := range []string{spec.ActionApprove, spec.ActionFixup, spec.ActionWaive, spec.ActionRetry, spec.ActionMerge} {
		e := matrixEntry(t, res, class)
		if e.Source != spec.SourceExplicit {
			t.Errorf("matrix[%s].Source = %q, want explicit", class, e.Source)
		}
		wantMode := spec.ModeGated
		if class == spec.ActionWaive {
			wantMode = spec.ModeAuto
		}
		if e.Mode != wantMode {
			t.Errorf("matrix[%s].Mode = %q, want %q", class, e.Mode, wantMode)
		}
	}
}

// TestEvaluate_ReportMode_Evaluated: a `mode: report` class declaring its own
// `when` is EVALUATED into Reports — and stays out of the decision set,
// because report delegates nothing.
func TestEvaluate_ReportMode_Evaluated(t *testing.T) {
	wf := parseV2Workflow(t, `    actions:
      waive:
        mode: report
        when: solo_low`)
	ev := &Evaluator{
		Stages:   &fakeStages{stages: []*run.Stage{mkStage(0, run.StageTypePlan, run.StageStateAwaitingApproval)}},
		Concerns: &fakeConcerns{open: []*concern.Concern{openConcern("low")}},
		Audit:    &fakeAudit{},
	}
	res := evaluate(t, ev, wf, newRun())

	d, ok := res.Report(ActionWaive)
	if !ok {
		t.Fatalf("no waive report decision (reports = %+v)", res.Reports)
	}
	if !d.Met || d.Condition != spec.ConditionSoloLow || d.Mode != spec.ModeReport {
		t.Errorf("waive report = %+v, want met solo_low at mode report", d)
	}
	if _, ok := res.Decision(ActionWaive); ok {
		t.Errorf("waive has a DECISION; report must delegate nothing (actions = %+v)", res.Actions)
	}
}

// TestEvaluate_ReportMode_ForeignConditionRefused: a report entry naming
// ANOTHER class's condition produces no report decision — fail-closed,
// because this evaluator answers the class's own predicate, not the one the
// author named.
//
// This is now DEFENSE IN DEPTH: parse-time validation rejects a foreign-`when`
// report entry outright (pinned in backend/internal/spec by
// TestParseV2_RejectsReportEntryWithForeignWhen), so a foreign-`when` report
// entry can no longer reach the evaluator through ParseBytes. The matrix here
// is therefore constructed DIRECTLY, bypassing validation, to keep the
// evaluator's own fail-closed branch covered.
func TestEvaluate_ReportMode_ForeignConditionRefused(t *testing.T) {
	wf := &spec.Workflow{
		Actions: &spec.ActionMatrix{Classes: map[string]spec.ActionEntry{
			spec.ActionWaive: {Mode: spec.ModeReport, When: spec.ConditionInfraFlake},
		}},
	}
	// A non-nil derived block makes the workflow delegation-configured so the
	// evaluator resolves the matrix rather than short-circuiting.
	wf.OperatorAgent = spec.DerivedOperatorAgent(spec.ResolveAutonomy(wf, nil))
	ev := &Evaluator{
		Stages:   &fakeStages{stages: []*run.Stage{mkStage(0, run.StageTypePlan, run.StageStateAwaitingApproval)}},
		Concerns: &fakeConcerns{open: []*concern.Concern{openConcern("low")}},
		Audit:    &fakeAudit{},
	}
	res := evaluate(t, ev, wf, newRun())

	if d, ok := res.Report(ActionWaive); ok {
		t.Errorf("waive report decision = %+v, want none (foreign condition is refused)", d)
	}
	if e := matrixEntry(t, res, spec.ActionWaive); e.Mode != spec.ModeReport {
		t.Errorf("matrix[waive].Mode = %q, want report (the entry is still surfaced)", e.Mode)
	}
}

// TestEvaluate_ReportMode_BareEntryHasNoReportDecision: a bare `report`
// entry declares no condition, so there is nothing to evaluate — it fires on
// gate-live alone (the auto-driver's rule), which is why Reports is empty
// while the matrix still carries the entry.
func TestEvaluate_ReportMode_BareEntryHasNoReportDecision(t *testing.T) {
	wf := parseV2Workflow(t, `    actions:
      approve:
        mode: report`)
	ev := &Evaluator{
		Stages:   &fakeStages{stages: []*run.Stage{mkStage(0, run.StageTypePlan, run.StageStateAwaitingApproval)}},
		Concerns: &fakeConcerns{},
		Audit:    &fakeAudit{},
	}
	res := evaluate(t, ev, wf, newRun())

	if len(res.Reports) != 0 {
		t.Errorf("Reports = %+v, want empty for a bare report entry", res.Reports)
	}
	if e := matrixEntry(t, res, spec.ActionApprove); e.Mode != spec.ModeReport || e.Condition != "" {
		t.Errorf("matrix[approve] = %+v, want mode report with no condition", e)
	}
}

// TestEvaluate_ParkedAwaitingInput_NoMatrix: a run parked for human
// direction delegates nothing AND proposes nothing — the matrix is
// deliberately omitted so the auto-driver's report arm cannot surface a
// proposal to a human the run is already blocked on.
func TestEvaluate_ParkedAwaitingInput_NoMatrix(t *testing.T) {
	wf := parseV2Workflow(t, `    autonomy: high`)
	ev := &Evaluator{
		Stages:   &fakeStages{stages: []*run.Stage{mkStage(0, run.StageTypePlan, run.StageStateAwaitingInput)}},
		Concerns: &fakeConcerns{open: []*concern.Concern{openConcern("low")}},
		Audit:    &fakeAudit{},
	}
	res := evaluate(t, ev, wf, newRun())

	if res.Matrix != nil || res.Reports != nil || res.Actions != nil {
		t.Errorf("parked result = %+v, want no matrix, no reports and no decisions", res)
	}
	if len(res.MustPageHuman) == 0 {
		t.Error("MustPageHuman is empty; the page envelope is still surfaced while parked")
	}
}

// TestActionForClass: the verb<->class mapping is total over the five known
// classes (route_fixup is the one place the vocabularies differ) and reports
// false for an extension class.
func TestActionForClass(t *testing.T) {
	want := map[string]string{
		spec.ActionApprove: ActionApprove,
		spec.ActionFixup:   ActionRouteFixup,
		spec.ActionWaive:   ActionWaive,
		spec.ActionRetry:   ActionRetry,
		spec.ActionMerge:   ActionMerge,
	}
	for class, action := range want {
		got, ok := ActionForClass(class)
		if !ok || got != action {
			t.Errorf("ActionForClass(%q) = %q, %v; want %q, true", class, got, ok, action)
		}
	}
	if got, ok := ActionForClass("promote"); ok {
		t.Errorf("ActionForClass(\"promote\") = %q, true; want false (extension class)", got)
	}
}
