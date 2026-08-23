package appliesto

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// Pure-core coverage for the applies_to evaluation shared by all three seams
// (E53.3 / #2226, E53.10 / #2361). One assertion per enumerated branch — the
// control is a fail-closed governance gate, so "the happy path plus a subset"
// is exactly the shape that ships a control which rejects everything (or admits
// everything) and still passes (#1199).

// TestAppliesTo_PhaseSplit_IsExhaustive pins which criterion belongs to which
// phase. A criterion silently landing in neither half is a criterion that is
// declared and never enforced — the failure mode this whole control exists to
// prevent.
func TestAppliesTo_PhaseSplit_IsExhaustive(t *testing.T) {
	full := spec.Predicate{
		Paths:       []string{"docs/**"},
		Labels:      []string{"chore"},
		ChangeKinds: []string{"docs"},
		Triggers:    []spec.TriggerForm{spec.TriggerDiff},
	}
	adm, _ := PhasePredicate(full, PhaseAdmission)
	gate, _ := PhasePredicate(full, PhasePlanGate)
	if len(adm.Labels) == 0 || len(adm.Triggers) == 0 || len(adm.ChangeKinds) == 0 {
		t.Errorf("admission half = %+v, want labels + trigger + change_kind", adm)
	}
	if len(adm.Paths) != 0 {
		t.Error("admission half carries paths; paths is the deferred criterion")
	}
	if len(gate.Paths) == 0 {
		t.Errorf("plan-gate half = %+v, want paths", gate)
	}
	if len(gate.Labels) != 0 || len(gate.Triggers) != 0 || len(gate.ChangeKinds) != 0 {
		t.Errorf("plan-gate half = %+v, want paths ONLY (the rest are decided at admission)", gate)
	}

	// TRIPWIRE for a criterion the grammar gains LATER. The assertions above
	// pin today's four; PhasePredicate is a hand-maintained field-by-field copy
	// of spec.Predicate and its `constrains` check enumerates the same four by
	// name. A fifth criterion added by a sibling slice (#2227 escalations, #2211
	// review conventions) is picked up by the applies_to $ref automatically, but
	// would be copied into NEITHER half and counted by NEITHER `constrains` —
	// declared and silently never enforced, which is the exact failure mode this
	// function's own comment names for change_kind. So the "exhaustive over the
	// predicate grammar" claim is enforced against the STRUCT rather than against
	// a list restated here.
	//
	// The field COUNT is the cheap half: it fails on the specific edit that
	// opens the gap and on nothing else.
	predT := reflect.TypeOf(spec.Predicate{})
	if n := predT.NumField(); n != 4 {
		t.Fatalf("spec.Predicate has %d fields, want 4 (paths, labels, change_kind, trigger). "+
			"A criterion was added or removed: route it in PhasePredicate (into a phase half AND its `constrains` check), "+
			"extend FirstFailingCriterion so the rejection can name it, then update this count. "+
			"All THREE seams — server admission, server plan gate, and the webhook dispatcher — consume this split.", n)
	}

	// The load-bearing half: EVERY declared criterion must survive the split
	// into at least one phase. Populating `full` above is what feeds this, so a
	// new field left out of `full` fails here too — the author is pushed to
	// decide which phase owns it rather than to bump a number.
	fullV, admV, gateV := reflect.ValueOf(full), reflect.ValueOf(adm), reflect.ValueOf(gate)
	for i := range predT.NumField() {
		name := predT.Field(i).Name
		if predT.Field(i).Type.Kind() != reflect.Slice {
			t.Errorf("spec.Predicate.%s is not a slice; this tripwire assumes every criterion is a list — re-check the split by hand", name)
			continue
		}
		if fullV.Field(i).Len() == 0 {
			t.Errorf("the `full` fixture leaves %s unset, so the split is untested for it; populate it and route the criterion", name)
			continue
		}
		if admV.Field(i).Len() == 0 && gateV.Field(i).Len() == 0 {
			t.Errorf("spec.Predicate.%s is carried by NEITHER phase half: a declared criterion that is silently never enforced", name)
		}
	}
}

// --- SatisfyingWorkflows: enumeration, ordering, and the error exclusion ---

func TestSatisfyingWorkflows_EnumeratesAndExcludesUnevaluable(t *testing.T) {
	parsed := &spec.Spec{Workflows: map[string]spec.Workflow{
		"zeta":   {}, // no declaration — accepts anything
		"alpha":  {AppliesTo: &spec.Predicate{Labels: []string{"chore"}}},
		"beta":   {AppliesTo: &spec.Predicate{Labels: []string{"bug"}}},
		"broken": {AppliesTo: &spec.Predicate{Paths: []string{"["}}},
	}}

	// At ADMISSION the paths-only `broken` declaration does not constrain, so
	// it is a genuine alternative and its malformed glob is not yet evaluated
	// — the same deferral M7 pins. `beta`'s labels criterion excludes it.
	got := SatisfyingWorkflows(parsed, spec.Change{Labels: []string{"chore"}}, PhaseAdmission)
	if want := "alpha,broken,zeta"; strings.Join(got, ",") != want {
		t.Errorf("admission SatisfyingWorkflows = %v, want %s (name-ordered)", got, want)
	}

	// At the PLAN GATE the malformed glob DOES evaluate, and an unevaluable
	// declaration must never be recommended as a place to take the change.
	got = SatisfyingWorkflows(parsed, spec.Change{Paths: []string{"docs/x.md"}}, PhasePlanGate)
	for _, name := range got {
		if name == "broken" {
			t.Errorf("SatisfyingWorkflows = %v, want `broken` EXCLUDED: a declaration that errors cannot be recommended", got)
		}
	}
	if strings.Join(got, ",") != "alpha,beta,zeta" {
		t.Errorf("plan-gate SatisfyingWorkflows = %v, want the three workflows that do not constrain paths", got)
	}

	if SatisfyingWorkflows(nil, spec.Change{}, PhaseAdmission) != nil {
		t.Error("a nil spec must enumerate nothing rather than panic")
	}
}

// --- the ONE renderer: shape parity across all THREE seams ---

// TestRenderRejection_AllThreeSeamsCarryTheSameShape asserts the binding body
// shape is identical across SeamStartRun / SeamWebhook / SeamPlanGate and that
// the three tails differ. In particular the WEBHOOK tail must NOT advertise
// applies_to_override (an override that does not exist on a path carrying no
// operator request must not be advertised), while the other two do.
func TestRenderRejection_AllThreeSeamsCarryTheSameShape(t *testing.T) {
	// Every field EXCEPT Seam is held identical across the three renders, so the
	// ONLY thing that can make the three messages differ is the Seam-selected
	// tail. An earlier version perturbed ObservedLabel to "scope.files" for the
	// plan-gate render, which guaranteed inequality regardless of the tail and
	// made the "three tails differ" assertion below vacuous — it could no longer
	// catch a plan-gate tail that silently collapsed to the start_run tail.
	base := Rejection{
		WorkflowID:    "routine_change",
		Criterion:     "paths",
		Required:      []string{"docs/**"},
		Observed:      []string{"backend/internal/server/runs.go"},
		ObservedLabel: "scope.files",
		Satisfying:    []string{"feature_change"},
	}
	base.Seam = SeamStartRun
	base.Phase = PhaseAdmission
	startRun := RenderRejection(base)
	base.Seam = SeamWebhook
	webhook := RenderRejection(base)
	base.Seam = SeamPlanGate
	base.Phase = PhasePlanGate
	planGate := RenderRejection(base)

	// The binding shape — workflow, criterion, required, observed, satisfying,
	// and the observed-value label — is identical on every seam.
	for _, msg := range []string{startRun, webhook, planGate} {
		for _, want := range []string{"routine_change", "paths", "docs/**", "backend/internal/server/runs.go", "feature_change", "scope.files"} {
			if !strings.Contains(msg, want) {
				t.Errorf("message %q is missing %q — the binding shape must not vary by seam", msg, want)
			}
		}
	}

	// The tails: start_run and plan gate name applies_to_override; the webhook
	// tail must NOT (there is no operator request on that path to carry one).
	if !strings.Contains(startRun, "applies_to_override") {
		t.Errorf("start_run tail %q must advertise applies_to_override", startRun)
	}
	if !strings.Contains(planGate, "applies_to_override") {
		t.Errorf("plan-gate tail %q must advertise applies_to_override", planGate)
	}
	if strings.Contains(webhook, "applies_to_override") {
		t.Errorf("webhook tail %q must NOT advertise applies_to_override — a webhook trigger carries no way to pass one", webhook)
	}
	// The three tails are genuinely distinct.
	if startRun == webhook || startRun == planGate || webhook == planGate {
		t.Errorf("the three seam tails must differ:\n start_run=%q\n webhook=%q\n planGate=%q", startRun, webhook, planGate)
	}

	// A change no workflow accepts says so rather than trailing an empty list.
	base.Satisfying = nil
	if msg := RenderRejection(base); !strings.Contains(msg, "No workflow in this spec accepts this change") {
		t.Errorf("message %q must state that nothing accepts the change", msg)
	}
}

func TestFirstFailingCriterion_NamesTheCriterion(t *testing.T) {
	c := spec.Change{Labels: []string{"bug"}, Paths: []string{"backend/x.go"}, Trigger: spec.TriggerDiff}
	for _, tc := range []struct {
		name string
		sub  spec.Predicate
		want string
	}{
		{"paths", spec.Predicate{Paths: []string{"docs/**"}}, "paths"},
		{"labels", spec.Predicate{Labels: []string{"chore"}}, "labels"},
		{"change_kind", spec.Predicate{ChangeKinds: []string{"docs"}}, "change_kind"},
		{"trigger", spec.Predicate{Triggers: []spec.TriggerForm{spec.TriggerScheduled}}, "trigger"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, _, ok := FirstFailingCriterion(tc.sub, c)
			if !ok || got != tc.want {
				t.Errorf("FirstFailingCriterion = (%q, %v), want (%q, true)", got, ok, tc.want)
			}
		})
	}
	if _, _, _, ok := FirstFailingCriterion(spec.Predicate{Labels: []string{"bug"}}, c); ok {
		t.Error("FirstFailingCriterion reported a failure for a satisfied predicate")
	}
}

// TestTriggerFormForSource pins the documented trigger_source→TriggerForm
// mapping the M5 cases exercise through the handler, over the WHOLE source
// vocabulary (E54.22 / #2826). The mapping is a value correspondence that
// compilation does not enforce, so it is asserted exhaustively:
//
//   - the three ORIGIN sources and the empty value map to diff;
//   - on_demand maps to on_demand — the producer that makes ADR-065's
//     `trigger: [scheduled, on_demand]` grooming declaration selectable;
//   - an UNRECOGNIZED source stays diff-shaped (the conservative default arm:
//     every existing predicate is written against a diff-form change).
func TestTriggerFormForSource(t *testing.T) {
	cases := []struct {
		source string
		want   spec.TriggerForm
	}{
		{"github_issue", spec.TriggerDiff},
		{"cli", spec.TriggerDiff},
		{"ui", spec.TriggerDiff},
		{"", spec.TriggerDiff},
		{"nonsense", spec.TriggerDiff},
		// `scheduled` has NO producer, so no trigger_source maps to it; the
		// string is not a trigger source at all and takes the default arm.
		{"scheduled", spec.TriggerDiff},
		{"on_demand", spec.TriggerOnDemand},
	}
	for _, tc := range cases {
		if got := TriggerFormForSource(tc.source); got != tc.want {
			t.Errorf("TriggerFormForSource(%q) = %q, want %q", tc.source, got, tc.want)
		}
	}
	// The mapping is keyed off the run package's constants, not string
	// literals that could drift from them.
	if got := TriggerFormForSource(string(run.TriggerOnDemand)); got != spec.TriggerOnDemand {
		t.Errorf("TriggerFormForSource(run.TriggerOnDemand) = %q, want on_demand", got)
	}
}

// TestAdmissionChange_OnDemandMatchesNonDiffPredicate is AC1 + AC2 asserted
// through the Change BUILDER rather than by reading a constant: the shipped
// grooming declaration's predicate must MATCH an on-demand run and must NOT
// match a cli run. Both directions, because a mapping that made everything
// match would satisfy a one-sided test.
func TestAdmissionChange_OnDemandMatchesNonDiffPredicate(t *testing.T) {
	groomingPredicate := spec.Predicate{Triggers: []spec.TriggerForm{spec.TriggerScheduled, spec.TriggerOnDemand}}

	onDemand := AdmissionChange(string(run.TriggerOnDemand), nil)
	if onDemand.Trigger != spec.TriggerOnDemand {
		t.Fatalf("AdmissionChange(on_demand).Trigger = %q, want on_demand", onDemand.Trigger)
	}
	if ok, err := groomingPredicate.Match(onDemand); err != nil || !ok {
		t.Errorf("(%v, %v): trigger:[scheduled, on_demand] must MATCH an on_demand run — this is what makes the grooming workflow selectable", ok, err)
	}

	cli := AdmissionChange(string(run.TriggerCLI), nil)
	if ok, err := groomingPredicate.Match(cli); err != nil || ok {
		t.Errorf("(%v, %v): trigger:[scheduled, on_demand] must NOT match a cli run", ok, err)
	}

	// The converse invariance (AC3): a diff-only predicate keeps admitting the
	// three origin sources and now REFUSES on_demand. Widening the vocabulary
	// must not quietly re-route existing declarations.
	diffOnly := spec.Predicate{Triggers: []spec.TriggerForm{spec.TriggerDiff}}
	for _, src := range []run.TriggerSource{run.TriggerGitHubIssue, run.TriggerCLI, run.TriggerUI} {
		if ok, err := diffOnly.Match(AdmissionChange(string(src), nil)); err != nil || !ok {
			t.Errorf("(%v, %v): trigger:[diff] must still admit %q", ok, err, src)
		}
	}
	if ok, err := diffOnly.Match(onDemand); err != nil || ok {
		t.Errorf("(%v, %v): trigger:[diff] must REFUSE an on_demand run", ok, err)
	}
}

// TestAdmissionChange_EmptyLabelSetIsNotMatchAll pins the fail-closed reading
// of an absent issue context at the Change-builder level.
func TestAdmissionChange_EmptyLabelSetIsNotMatchAll(t *testing.T) {
	c := AdmissionChange("cli", nil)
	if len(c.Labels) != 0 {
		t.Errorf("labels = %v, want empty for an issue-less run", c.Labels)
	}
	if len(c.Paths) != 0 {
		t.Error("AdmissionChange supplied paths; paths must come only from the plan artifact, never a caller self-attestation")
	}
	ok, err := (spec.Predicate{Labels: []string{"chore"}}).Match(c)
	if err != nil || ok {
		t.Errorf("(%v, %v): an EMPTY label set must NOT satisfy a labels criterion", ok, err)
	}
	// A populated label set flows through.
	c = AdmissionChange("github_issue", []string{"docs"})
	if len(c.Labels) != 1 || c.Labels[0] != "docs" {
		t.Errorf("labels = %v, want [docs]", c.Labels)
	}
	// The fail-closed reading must not change with the new on_demand source
	// (#2826): it carries the new trigger FORM and still an EMPTY label set.
	c = AdmissionChange(string(run.TriggerOnDemand), nil)
	if c.Trigger != spec.TriggerOnDemand {
		t.Errorf("AdmissionChange(on_demand).Trigger = %q, want on_demand", c.Trigger)
	}
	if len(c.Labels) != 0 {
		t.Errorf("labels = %v, want empty for an on_demand run with no issue context", c.Labels)
	}
	if ok, err := (spec.Predicate{Labels: []string{"chore"}}).Match(c); err != nil || ok {
		t.Errorf("(%v, %v): an on_demand run's EMPTY label set must NOT satisfy a labels criterion", ok, err)
	}
}
