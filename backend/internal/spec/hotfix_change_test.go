package spec_test

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// hotfixChangeExamplePath is the committed incident hotfix preset (E35.5 /
// #1602). It is also the driver of the alert-admission seam test in
// backend/internal/server and the stanza the operator copies into the live
// spec, so every test below reads the SHIPPED bytes from disk.
const hotfixChangeExamplePath = "../../../docs/spec/examples/workflow-v2-hotfix-change.yaml"

// hotfixPageEvents is the full nine-event page_human_on closed set the preset
// declares: the medium tier's seven plus advisory_reviewer_reject and
// clarification_request.
var hotfixPageEvents = []string{
	spec.PageEventAdvisoryReviewerReject,
	spec.PageEventGatingReviewerReject,
	spec.PageEventPlanRejection,
	spec.PageEventScopeAmendment,
	spec.PageEventBudgetOverride,
	spec.PageEventPolicyOverride,
	spec.PageEventExceptionRequest,
	spec.PageEventRequirementArbitration,
	spec.PageEventClarificationRequest,
}

func readHotfixExample(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(hotfixChangeExamplePath)
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	return data
}

// parseHotfixWorkflow parses data and returns its hotfix_change workflow.
func parseHotfixWorkflow(t *testing.T, data []byte) spec.Workflow {
	t.Helper()
	parsed, err := spec.ParseBytes(data)
	if err != nil {
		t.Fatalf("ParseBytes(%s): %v", hotfixChangeExamplePath, err)
	}
	if parsed.Version != "2" {
		t.Errorf("Version = %q, want \"2\"", parsed.Version)
	}
	wf, ok := parsed.Workflows["hotfix_change"]
	if !ok {
		t.Fatalf("example declares no `hotfix_change` workflow; got %d workflows", len(parsed.Workflows))
	}
	return wf
}

// TestHotfixChangeExample_Parses pins the in-run shape: plan -> implement ->
// acceptance -> review, the plan gate retained, and the merge gate a human
// executor whose approval excludes the author and any agent.
func TestHotfixChangeExample_Parses(t *testing.T) {
	wf := parseHotfixWorkflow(t, readHotfixExample(t))

	var types []spec.StageType
	for _, st := range wf.Stages {
		types = append(types, st.Type)
	}
	want := []spec.StageType{spec.StageTypePlan, spec.StageTypeImplement, spec.StageTypeAcceptance, spec.StageTypeReview}
	if !slices.Equal(types, want) {
		t.Fatalf("stage types = %v, want %v", types, want)
	}
	if !hasApprovalGate(wf.Stages[0]) {
		t.Error("plan stage carries no approval gate; the hotfix preset retains the plan gate")
	}
	review := wf.Stages[3]
	if !review.Executor.Human {
		t.Errorf("review executor = %+v, want a human executor", review.Executor)
	}
	if err := excludesAuthorAndAgent(review); err != nil {
		t.Error(err)
	}
}

func hasApprovalGate(st spec.Stage) bool {
	for _, g := range st.Gates {
		if g.Type == spec.GateTypeApproval && g.Approvals != nil {
			return true
		}
	}
	return false
}

// excludesAuthorAndAgent reports an error unless st carries an approval gate
// whose predicate excludes both the author and any agent identity.
func excludesAuthorAndAgent(st spec.Stage) error {
	for _, g := range st.Gates {
		if g.Type != spec.GateTypeApproval || g.Approvals == nil {
			continue
		}
		if slices.Contains(g.Approvals.Not, "author") && slices.Contains(g.Approvals.Not, "agent") {
			return nil
		}
	}
	return fmt.Errorf("stage %q has no approval gate excluding [author, agent]", st.ID)
}

// TestHotfixChangeExample_AppliesToAdmitsDiff pins the E35.4 contract: an
// `alert` run is routed as spec.TriggerDiff, so the preset must declare it.
// The server seam test proves the same fact through StartAlertRun.
func TestHotfixChangeExample_AppliesToAdmitsDiff(t *testing.T) {
	wf := parseHotfixWorkflow(t, readHotfixExample(t))
	if wf.AppliesTo == nil {
		t.Fatal("applies_to absent; the preset must declare the trigger form an alert run is routed as")
	}
	if !slices.Contains(wf.AppliesTo.Triggers, spec.TriggerDiff) {
		t.Errorf("applies_to.trigger = %v, want it to include %q", wf.AppliesTo.Triggers, spec.TriggerDiff)
	}
	ok, err := wf.AppliesTo.Match(spec.Change{Trigger: spec.TriggerDiff})
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if !ok {
		t.Error("applies_to does not match an alert run's Change{Trigger: diff}")
	}
}

// checkMergeNeverDelegated asserts the preset's delegation contract on a
// parsed workflow and returns the first violation. It returns an error rather
// than failing so the mutated-document variants below can assert that a
// document WITHOUT the explicit entries fails it.
//
// The Source=explicit assertion is what isolates the explicit entries: under
// `autonomy: medium` the tier ALREADY gates waive and merge, so a mode-only
// assertion stays green when the explicit merge entry is deleted.
func checkMergeNeverDelegated(wf spec.Workflow) error {
	rm := spec.ResolveAutonomy(&wf, nil)
	if rm == nil {
		return errors.New("workflow resolves no autonomy block")
	}
	want := map[string]struct {
		mode      spec.ActionMode
		source    spec.ResolutionSource
		condition spec.DelegationCondition
	}{
		spec.ActionApprove: {spec.ModeGated, spec.SourceExplicit, ""},
		spec.ActionWaive:   {spec.ModeGated, spec.SourceExplicit, ""},
		spec.ActionMerge:   {spec.ModeGated, spec.SourceExplicit, ""},
		spec.ActionFixup:   {spec.ModeAuto, spec.SourceTier, spec.ConditionConvergentConcerns},
		spec.ActionRetry:   {spec.ModeAuto, spec.SourceTier, spec.ConditionInfraFlake},
	}
	got := map[string]spec.ResolvedAction{}
	for _, a := range rm.Actions {
		got[a.Action] = a
	}
	for _, class := range []string{spec.ActionApprove, spec.ActionFixup, spec.ActionWaive, spec.ActionRetry, spec.ActionMerge} {
		w, a := want[class], got[class]
		if a.Mode != w.mode || a.Source != w.source || a.Condition != w.condition {
			return fmt.Errorf("resolved %s = mode %q source %q condition %q, want %q/%q/%q",
				class, a.Mode, a.Source, a.Condition, w.mode, w.source, w.condition)
		}
	}
	if !slices.Equal(rm.PageHumanOn, hotfixPageEvents) {
		return fmt.Errorf("page_human_on = %v, want the nine-event closed set %v", rm.PageHumanOn, hotfixPageEvents)
	}
	oa := wf.OperatorAgent
	if oa == nil {
		return errors.New("derived OperatorAgent is nil")
	}
	if oa.MayApprove != "" || oa.MayWaive != "" || oa.MayMerge != "" {
		return fmt.Errorf("derived knobs may_approve=%q may_waive=%q may_merge=%q, want all empty", oa.MayApprove, oa.MayWaive, oa.MayMerge)
	}
	// Per gate: the RESOLVED mode first (what a gate-level override would
	// change), then the declaration itself — a gate block restating merge
	// gated resolves cleanly but is still forbidden by the preset's
	// convention, so each arm has its own isolating variant below.
	for _, st := range wf.Stages {
		for gi := range st.Gates {
			g := &st.Gates[gi]
			for _, a := range spec.ResolveAutonomy(&wf, g).Actions {
				if (a.Action == spec.ActionMerge || a.Action == spec.ActionApprove) && a.Mode != spec.ModeGated {
					return fmt.Errorf("stage %q gate %d resolves %s = %q, want gated", st.ID, gi, a.Action, a.Mode)
				}
			}
			if g.Autonomy != "" || g.Actions != nil {
				return fmt.Errorf("stage %q gate %d declares a gate-level autonomy/actions block; it replaces the workflow block wholesale", st.ID, gi)
			}
		}
	}
	return nil
}

// TestHotfixChangeExample_MergeNeverDelegated pins "merge is never delegated"
// as a convention with its enforcing knob named: the explicit `actions`
// entries for approve, waive and merge, which win over any tier.
//
// Counterfactuals (each run against the example, then restored):
//   - delete `merge: {mode: gated}` -> RED: "resolved merge = mode "gated"
//     source "tier"", and the tier-raise subtest RED on mode "auto";
//   - delete `approve: {mode: gated}` -> RED: approve resolves auto /
//     clean_dual_approval from the tier;
//   - drop advisory_reviewer_reject from page_human_on -> RED on the
//     nine-event set;
//   - add `actions: {merge: {mode: auto, when: gates_resolved_ci_green}}` to
//     the review approval gate -> RED: the review gate resolves merge auto
//     (the in-test variants below pin both gate arms of the checker).
func TestHotfixChangeExample_MergeNeverDelegated(t *testing.T) {
	data := readHotfixExample(t)
	if err := checkMergeNeverDelegated(parseHotfixWorkflow(t, data)); err != nil {
		t.Fatal(err)
	}

	// Raising the tier must not widen approve, waive or merge: the explicit
	// entries — not the tier — are the enforcing knob.
	t.Run("tier_raised_to_high", func(t *testing.T) {
		raised := mutateHotfix(t, data, "    autonomy: medium\n", "    autonomy: high\n")
		if err := checkMergeNeverDelegated(parseHotfixWorkflow(t, raised)); err != nil {
			t.Fatalf("with autonomy: high: %v", err)
		}
	})

	// The checker discriminates: the same raised tier WITHOUT the explicit
	// merge entry resolves merge auto, and the checker must say so.
	t.Run("checker_rejects_missing_explicit_merge", func(t *testing.T) {
		raised := mutateHotfix(t, data, "    autonomy: medium\n", "    autonomy: high\n")
		stripped := mutateHotfix(t, raised, "      merge:\n        mode: gated\n", "")
		err := checkMergeNeverDelegated(parseHotfixWorkflow(t, stripped))
		if err == nil || !strings.Contains(err.Error(), "resolved merge") {
			t.Fatalf("checker err = %v, want a resolved-merge violation", err)
		}
	})

	const reviewGate = "      - id: review\n        type: review\n        executor:\n          human: true\n        inputs:\n          - artifact: pull_request\n            from_stage: implement\n        gates:\n          - type: approval\n"

	// A gate-level block replaces the workflow block wholesale: merge auto
	// there resolves auto for that gate.
	t.Run("checker_rejects_gate_level_merge_auto", func(t *testing.T) {
		overridden := mutateHotfix(t, data, reviewGate,
			reviewGate+"            actions:\n              merge:\n                mode: auto\n                when: gates_resolved_ci_green\n")
		err := checkMergeNeverDelegated(parseHotfixWorkflow(t, overridden))
		if err == nil || !strings.Contains(err.Error(), `resolves merge = "auto"`) {
			t.Fatalf("checker err = %v, want the review gate's merge resolving auto", err)
		}
	})

	// A gate block that restates approve and merge gated resolves cleanly,
	// so only the declaration arm can refuse it.
	t.Run("checker_rejects_any_gate_level_block", func(t *testing.T) {
		restated := mutateHotfix(t, data, reviewGate,
			reviewGate+"            actions:\n              approve:\n                mode: gated\n              merge:\n                mode: gated\n")
		err := checkMergeNeverDelegated(parseHotfixWorkflow(t, restated))
		if err == nil || !strings.Contains(err.Error(), "gate-level") {
			t.Fatalf("checker err = %v, want a gate-level block violation", err)
		}
	})
}

// mutateHotfix replaces exactly one occurrence of old in data, failing when
// old is absent so a reshaped example cannot turn a variant into a no-op.
func mutateHotfix(t *testing.T, data []byte, old, replacement string) []byte {
	t.Helper()
	if n := strings.Count(string(data), old); n != 1 {
		t.Fatalf("mutation anchor %q occurs %d times in the example, want exactly 1", old, n)
	}
	return []byte(strings.Replace(string(data), old, replacement, 1))
}

// TestHotfixChangeExample_TighterThanFeatureChange compares the preset against
// the embedded medium preset's feature_change: the stage-runtime policy, the
// plan and implement budgets (runtime AND the enforced limit_usd) and the diff
// cap are each STRICTLY tighter; the acceptance limit_usd is a stated small
// value (the medium preset declares no acceptance stage); and the verify
// timeout is NOT reduced (#3383).
//
// Counterfactual: raise the implement budget max_runtime to 30m (or its
// limit_usd to 12) -> RED on the strict-less comparison.
func TestHotfixChangeExample_TighterThanFeatureChange(t *testing.T) {
	hot := parseHotfixWorkflow(t, readHotfixExample(t))
	presetBytes, err := spec.PresetBytes(spec.PresetMedium)
	if err != nil {
		t.Fatalf("PresetBytes(medium): %v", err)
	}
	preset, err := spec.ParseBytes(presetBytes)
	if err != nil {
		t.Fatalf("parse medium preset: %v", err)
	}
	feat, ok := preset.Workflows["feature_change"]
	if !ok {
		t.Fatal("medium preset declares no feature_change workflow")
	}

	if hot.Policy == nil || feat.Policy == nil {
		t.Fatalf("policy: hotfix %+v, feature_change %+v; both must declare max_stage_runtime", hot.Policy, feat.Policy)
	}
	if h, f := hot.Policy.MaxStageRuntime.Duration, feat.Policy.MaxStageRuntime.Duration; h <= 0 || h >= f {
		t.Errorf("policy.max_stage_runtime = %s, want > 0 and strictly below feature_change's %s", h, f)
	}

	for _, typ := range []spec.StageType{spec.StageTypePlan, spec.StageTypeImplement} {
		hs, fs := stageOfType(t, hot, typ), stageOfType(t, feat, typ)
		if hs.Budget == nil || fs.Budget == nil {
			t.Fatalf("%s budget: hotfix %+v, feature_change %+v; both must declare one", typ, hs.Budget, fs.Budget)
		}
		if h, f := hs.Budget.Runtime(), fs.Budget.Runtime(); h <= 0 || h >= f {
			t.Errorf("%s budget max_runtime = %s, want > 0 and strictly below feature_change's %s", typ, h, f)
		}
		if h, f := hs.Budget.LimitUSD, fs.Budget.LimitUSD; h <= 0 || h >= f {
			t.Errorf("%s budget limit_usd = %v, want > 0 and strictly below feature_change's %v", typ, h, f)
		}
	}

	// The medium preset declares no acceptance stage, so the acceptance
	// ceiling is pinned to the stated small value the docs name.
	acc := stageOfType(t, hot, spec.StageTypeAcceptance)
	if acc.Budget == nil || acc.Budget.LimitUSD <= 0 || acc.Budget.LimitUSD > 3 {
		t.Errorf("acceptance budget = %+v, want a limit_usd in (0, 3]", acc.Budget)
	}
	for _, st := range hot.Stages {
		if st.Executor.Human {
			continue
		}
		if st.Budget == nil || st.Budget.LimitUSD <= 0 {
			t.Errorf("agent stage %q declares no limit_usd; every hotfix stage budget carries the enforced ceiling", st.ID)
		}
	}

	hi, fi := stageOfType(t, hot, spec.StageTypeImplement), stageOfType(t, feat, spec.StageTypeImplement)
	if h, f := maxFilesChanged(hi), maxFilesChanged(fi); h <= 0 || h >= f {
		t.Errorf("implement max_files_changed = %d, want > 0 and strictly below feature_change's %d", h, f)
	}
	if hi.Executor.Verify == nil || fi.Executor.Verify == nil {
		t.Fatal("both implement stages must declare a verify gate")
	}
	if h, f := hi.Executor.Verify.Timeout.Duration, fi.Executor.Verify.Timeout.Duration; h < f {
		t.Errorf("verify timeout = %s, want NOT below feature_change's %s (a killed gate reads as a verify failure with no failing test, #3383)", h, f)
	}
	if !hasApprovalGate(stageOfType(t, hot, spec.StageTypePlan)) || !hasApprovalGate(stageOfType(t, feat, spec.StageTypePlan)) {
		t.Error("both workflows must carry a plan approval gate")
	}
}

func stageOfType(t *testing.T, wf spec.Workflow, typ spec.StageType) spec.Stage {
	t.Helper()
	for _, st := range wf.Stages {
		if st.Type == typ {
			return st
		}
	}
	t.Fatalf("no %s stage", typ)
	return spec.Stage{}
}

func maxFilesChanged(st spec.Stage) int {
	for _, c := range st.Constraints {
		if c.MaxFilesChanged != 0 {
			return c.MaxFilesChanged
		}
	}
	return 0
}

// TestHotfixChangeDocsCrossReference pins the prose sites that describe the
// preset to the shipped example, naming every site to update when one drifts.
func TestHotfixChangeDocsCrossReference(t *testing.T) {
	const exampleName = "workflow-v2-hotfix-change.yaml"
	sites := []struct {
		path string
		want []string
	}{
		{"../../../docs/spec/workflow-v2.md", []string{"hotfix_change", exampleName, "TriggerFormForSource", "#1601", "follow-on `release` run"}},
		{"../../../docs/METHODOLOGY.md", []string{"hotfix_change", exampleName, "follow-on `release` run"}},
		{"../alerttrigger/README.md", []string{"hotfix_change", exampleName}},
	}
	for _, s := range sites {
		raw, err := os.ReadFile(s.path)
		if err != nil {
			t.Fatalf("read %s: %v", s.path, err)
		}
		for _, w := range s.want {
			if !strings.Contains(string(raw), w) {
				t.Errorf("%s does not mention %q; the hotfix_change preset is documented at docs/spec/workflow-v2.md, docs/METHODOLOGY.md and backend/internal/alerttrigger/README.md — update every site", s.path, w)
			}
		}
	}
}
