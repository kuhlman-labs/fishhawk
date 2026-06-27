package delegation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	res, err := ev.Evaluate(context.Background(), runRow, wf)
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
	res, err := ev.Evaluate(context.Background(), newRun(), testWorkflow(nil, nil))
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
	res, err := ev.Evaluate(context.Background(), newRun(), wf)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if res != nil {
		t.Fatalf("Result = %+v, want nil (the implement gate carries no block and the workflow level has none)", res)
	}
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
		name       string
		reviewers  *spec.ReviewersConfig // implement-stage reviewers; nil = default advisory
		open       []*concern.Concern
		audit      *fakeAudit
		wantMet    bool
		wantReason string
	}{
		{
			name:  "advisory met: verdicts in, no reject, one open concern",
			open:  []*concern.Concern{openConcern("medium")},
			audit: settledWithConcerns, wantMet: true,
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
			wf := testWorkflow(&spec.OperatorAgent{MayRouteFixup: spec.ConditionConvergentConcerns}, nil)
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
			if _, err := tt.ev.Evaluate(context.Background(), newRun(), wf); !errors.Is(err, boom) {
				t.Errorf("Evaluate error = %v, want the injected store failure", err)
			}
		})
	}
}
