package spec_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// --- per-path escalations (E53.4 / #2227) ---
//
// This file owns the DECLARATION side's proof: the grammar round-trips, the
// only-ever-raise family rejects every weakened-or-no-op dimension with an
// actionable message in ONE shape, composition is the strictest per dimension
// and therefore order-independent, and the autonomy clamp only ever downgrades.
// The two ENFORCEMENT seams (the approval gate and delegation resolution) are
// pinned next to the enforcement points, not here.

// escalationV2Doc renders a v2 workflow with a workflow-level `autonomy: high`
// block and ONE approval gate declaring count 2 / min_permission write, plus
// whatever `escalations` block the caller supplies (already indented four
// spaces). The baselines are deliberately mid-ladder so a test can straddle
// them from both sides.
func escalationV2Doc(escalations string) []byte {
	return []byte(`
version: "2"
workflows:
  feature_change:
    autonomy: high
` + escalations + `
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvals:
              count: 2
              min_permission: write
`)
}

// escalationBlock indents a YAML escalations list under a workflow.
func escalationBlock(body string) string {
	return "    escalations:\n" + body
}

// assertNoRaiseShape asserts msg was rendered from spec.MsgFmtEscalationNoRaise
// for this workflow / index / dimension, MECHANICALLY: it renders the shared
// constant with sentinel values for the three free-form arguments, splits on
// the sentinels, and requires every LITERAL segment of the shape — including
// the workflow name, index and dimension — to appear in order. A rejection
// that grew its own bespoke wording therefore fails here rather than being
// eyeballed as "the same shape".
func assertNoRaiseShape(t *testing.T, msg, workflow string, idx int, dimension string) {
	t.Helper()
	const sentinel = "\x00"
	shape := fmt.Sprintf(spec.MsgFmtEscalationNoRaise, workflow, idx, dimension, sentinel, sentinel, sentinel)
	rest := msg
	for _, segment := range strings.Split(shape, sentinel) {
		if segment == "" {
			continue
		}
		i := strings.Index(rest, segment)
		if i < 0 {
			t.Fatalf("message %q does not follow the shared no-raise shape: missing segment %q (in order)", msg, segment)
		}
		rest = rest[i+len(segment):]
	}
}

// TestParse_Escalations_V2RoundTrip is the positive control AND the
// DisallowUnknownFields proof: every property the schema declares under
// `escalations` survives the typed decode onto the exported types.
func TestParse_Escalations_V2RoundTrip(t *testing.T) {
	s, err := spec.ParseBytes(escalationV2Doc(escalationBlock(`      - match:
          paths: ["infra/**", "**/*.tf"]
          labels: [security]
          trigger: [diff]
        require:
          approvals:
            count: 3
            member_of: platform/security
            min_permission: admin
          max_autonomy: low`)))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	esc := s.Workflows["feature_change"].Escalations
	if len(esc) != 1 {
		t.Fatalf("Escalations = %#v, want exactly one entry", esc)
	}
	got := esc[0]
	if !reflect.DeepEqual(got.Match.Paths, []string{"infra/**", "**/*.tf"}) {
		t.Errorf("match.paths = %v, want the two declared globs", got.Match.Paths)
	}
	if !reflect.DeepEqual(got.Match.Labels, []string{"security"}) {
		t.Errorf("match.labels = %v, want [security]", got.Match.Labels)
	}
	if !reflect.DeepEqual(got.Match.Triggers, []spec.TriggerForm{spec.TriggerDiff}) {
		t.Errorf("match.trigger = %v, want [diff]", got.Match.Triggers)
	}
	if got.Require.Approvals == nil {
		t.Fatal("require.approvals is nil, want the declared block")
	}
	if got.Require.Approvals.Count == nil || *got.Require.Approvals.Count != 3 {
		t.Errorf("require.approvals.count = %v, want 3", got.Require.Approvals.Count)
	}
	if got.Require.Approvals.MemberOf != "platform/security" {
		t.Errorf("require.approvals.member_of = %q, want platform/security", got.Require.Approvals.MemberOf)
	}
	if got.Require.Approvals.MinPermission != "admin" {
		t.Errorf("require.approvals.min_permission = %q, want admin", got.Require.Approvals.MinPermission)
	}
	if got.Require.MaxAutonomy != spec.TierLow {
		t.Errorf("require.max_autonomy = %q, want low", got.Require.MaxAutonomy)
	}
}

// TestParse_Escalations_Absent_IsNil pins the back-compat reading: a workflow
// declaring no `escalations` parses to a NIL slice, which both enforcement
// seams read as "nothing raised" and short-circuit on.
func TestParse_Escalations_Absent_IsNil(t *testing.T) {
	s, err := spec.ParseBytes(escalationV2Doc(""))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if got := s.Workflows["feature_change"].Escalations; got != nil {
		t.Errorf("Escalations = %#v, want nil for a workflow declaring none", got)
	}
}

// TestParse_Escalations_ChangeKind_Rejected is the no-producer rejection,
// mirroring applies_to's: the shared predicate keeps `change_kind` for its
// other consumers, and only this consumer refuses it.
func TestParse_Escalations_ChangeKind_Rejected(t *testing.T) {
	_, err := spec.ParseBytes(escalationV2Doc(escalationBlock(`      - match:
          change_kind: [refactor]
        require:
          approvals:
            count: 3`)))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError for change_kind inside an escalation match", err)
	}
	if !strings.Contains(err.Error(), "/workflows/feature_change/escalations/0/match/change_kind") {
		t.Errorf("error = %q, want it to name the escalation's match/change_kind pointer", err.Error())
	}
	if !strings.Contains(err.Error(), spec.MsgEscalationChangeKindUnsupported) {
		t.Errorf("error = %q, want it to carry MsgEscalationChangeKindUnsupported", err.Error())
	}
}

// TestParse_Escalations_ChangeKindWinsOverMalformedGlob pins the ORDER
// contract: the change_kind refusal runs before the shared predicate
// validation, so an escalation wrong in both ways reports the diagnosis whose
// message names a fix the generic predicate error cannot.
func TestParse_Escalations_ChangeKindWinsOverMalformedGlob(t *testing.T) {
	_, err := spec.ParseBytes(escalationV2Doc(escalationBlock(`      - match:
          change_kind: [refactor]
          paths: ["infra/["]
        require:
          approvals:
            count: 3`)))
	if err == nil || !strings.Contains(err.Error(), spec.MsgEscalationChangeKindUnsupported) {
		t.Fatalf("err = %v, want the change_kind rejection to win over the malformed glob", err)
	}
}

// TestParse_Escalations_MalformedGlob_Rejected covers the shared
// Predicate.Validate rung reached at the escalation's own pointer.
func TestParse_Escalations_MalformedGlob_Rejected(t *testing.T) {
	_, err := spec.ParseBytes(escalationV2Doc(escalationBlock(`      - match:
          paths: ["infra/["]
        require:
          approvals:
            count: 3`)))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError for a malformed escalation glob", err)
	}
	if !strings.Contains(err.Error(), "/workflows/feature_change/escalations/0/match") {
		t.Errorf("error = %q, want it reported at the escalation's match pointer", err.Error())
	}
}

// TestValidate_Escalations_MustRaise is criterion 2: one case per
// weakened-or-no-op dimension, each asserting the reported PATH names
// /workflows/<n>/escalations/<i>/… and the message follows the ONE shared
// shape (workflow, index, dimension, escalated value, baseline, fix).
func TestValidate_Escalations_MustRaise(t *testing.T) {
	cases := []struct {
		name      string
		require   string
		wantPath  string
		dimension string
	}{
		{
			name:      "count equal to the baseline raises nothing",
			require:   "          approvals:\n            count: 2",
			wantPath:  "/workflows/feature_change/escalations/0/require/approvals/count",
			dimension: "approvals.count",
		},
		{
			name:      "count below the baseline is a lowering",
			require:   "          approvals:\n            count: 1",
			wantPath:  "/workflows/feature_change/escalations/0/require/approvals/count",
			dimension: "approvals.count",
		},
		{
			name:      "min_permission equal to the baseline raises nothing",
			require:   "          approvals:\n            min_permission: write",
			wantPath:  "/workflows/feature_change/escalations/0/require/approvals/min_permission",
			dimension: "approvals.min_permission",
		},
		{
			name:      "min_permission below the baseline is a lowering",
			require:   "          approvals:\n            min_permission: read",
			wantPath:  "/workflows/feature_change/escalations/0/require/approvals/min_permission",
			dimension: "approvals.min_permission",
		},
		{
			name:      "max_autonomy at the workflow's own tier clamps nothing",
			require:   "          max_autonomy: high",
			wantPath:  "/workflows/feature_change/escalations/0/require/max_autonomy",
			dimension: "max_autonomy",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := spec.ParseBytes(escalationV2Doc(escalationBlock(
				"      - match:\n          paths: [\"infra/**\"]\n        require:\n" + tc.require)))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError for a non-raising escalation", err)
			}
			if ve.Path != tc.wantPath {
				t.Errorf("path = %q, want %q", ve.Path, tc.wantPath)
			}
			assertNoRaiseShape(t, ve.Message, "feature_change", 0, tc.dimension)
		})
	}
}

// noCountGateSpec builds a v2 *Spec whose ONE approval gate declares an
// approvals block with NO explicit count, plus one paths-matched escalation
// whose require.approvals.count is `escCount`. The gate is built directly
// rather than parsed BECAUSE the v2 schema requires `count` on an approvals
// block: a countless gate cannot survive ParseBytes, so this is the
// defence-in-depth path for a spec that reached validation having bypassed the
// schema (a hand-built struct, or an older binary's cached bytes) — the same
// posture TestParse_Escalations_MalformedGlob uses at the resolver seam. The
// effective count of a countless gate is the runtime default of 1
// (effectiveApprovals starts out.count at 1), so an escalation count of 1
// raises nothing and must still be refused.
func noCountGateSpec(escCount int) *spec.Spec {
	c := escCount
	return &spec.Spec{
		Version: "2",
		Workflows: map[string]spec.Workflow{
			"feature_change": {
				Autonomy: spec.TierHigh,
				Escalations: []spec.Escalation{{
					Match:   spec.Predicate{Paths: []string{"infra/**"}},
					Require: spec.EscalationRequirements{Approvals: &spec.EscalatedApprovals{Count: &c}},
				}},
				Stages: []spec.Stage{{
					ID:       "plan",
					Type:     spec.StageTypePlan,
					Executor: spec.Executor{Agent: "claude-code"},
					Gates: []spec.Gate{{
						Type:      spec.GateTypeApproval,
						Approvals: &spec.Approvals{}, // no Count → effective count 1
					}},
				}},
			},
		},
	}
}

// TestValidate_Escalations_OmittedCountBaseline is the #2227-fixup regression:
// a gate declaring an approvals block but NO explicit count still requires ONE
// approval, so an escalation declaring `count: 1` raises nothing at runtime and
// must be refused — the only-ever-raise violation the earlier baseline-of-0
// computation let through (it treated an omitted count as 0, so `1 > 0` passed).
// The raising control (count 2) proves the check is "must raise above the
// effective baseline of 1", not "reject every count".
func TestValidate_Escalations_OmittedCountBaseline(t *testing.T) {
	t.Run("count 1 against a no-explicit-count gate raises nothing", func(t *testing.T) {
		err := spec.Validate(noCountGateSpec(1))
		var ve *spec.ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("err = %v, want *ValidationError — count 1 does not raise a gate whose effective count is already 1", err)
		}
		if ve.Path != "/workflows/feature_change/escalations/0/require/approvals/count" {
			t.Errorf("path = %q, want the count pointer", ve.Path)
		}
		assertNoRaiseShape(t, ve.Message, "feature_change", 0, "approvals.count")
	})

	t.Run("count 2 against a no-explicit-count gate genuinely raises", func(t *testing.T) {
		if err := spec.Validate(noCountGateSpec(2)); err != nil {
			t.Fatalf("Validate: %v, want count 2 accepted — it raises the effective baseline of 1", err)
		}
	})
}

// TestValidate_Escalations_RaisingValuesAccepted is the POSITIVE control that
// pins the rule as "must RAISE" rather than "must differ": a genuinely raising
// value on every dimension parses clean.
func TestValidate_Escalations_RaisingValuesAccepted(t *testing.T) {
	cases := []struct {
		name    string
		require string
	}{
		{name: "count above the baseline", require: "          approvals:\n            count: 3"},
		{name: "min_permission above the baseline", require: "          approvals:\n            min_permission: admin"},
		{name: "member_of no gate declares", require: "          approvals:\n            member_of: platform/security"},
		{name: "max_autonomy below the workflow tier", require: "          max_autonomy: low"},
		{name: "max_autonomy one rung below the workflow tier", require: "          max_autonomy: medium"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := spec.ParseBytes(escalationV2Doc(escalationBlock(
				"      - match:\n          paths: [\"infra/**\"]\n        require:\n" + tc.require))); err != nil {
				t.Fatalf("ParseBytes: %v, want a genuinely raising escalation to be accepted", err)
			}
		})
	}
}

// TestValidate_Escalations_ApprovalsWithNoApprovalGate is the workflow-wide
// inertness rejection: `require.approvals` on a workflow with no approval gate
// raises nothing ANYWHERE, which is a stronger diagnosis than any per-dimension
// one, so it reports at the block rather than at a dimension.
func TestValidate_Escalations_ApprovalsWithNoApprovalGate(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "2"
workflows:
  gateless:
    autonomy: high
    escalations:
      - match:
          paths: ["infra/**"]
        require:
          approvals:
            count: 5
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError for require.approvals with no approval gate", err)
	}
	if ve.Path != "/workflows/gateless/escalations/0/require/approvals" {
		t.Errorf("path = %q, want the require/approvals pointer", ve.Path)
	}
	want := fmt.Sprintf(spec.MsgFmtEscalationApprovalsNoApprovalGate, "gateless", 0, "gateless")
	if ve.Message != want {
		t.Errorf("message = %q, want %q", ve.Message, want)
	}
}

// TestValidate_Escalations_MaxAutonomyOnGatelessWorkflowAccepted is the
// no-approval-gate rejection's boundary: the message says a gate-less workflow
// may still raise max_autonomy, and it can.
func TestValidate_Escalations_MaxAutonomyOnGatelessWorkflowAccepted(t *testing.T) {
	if _, err := spec.ParseBytes([]byte(`
version: "2"
workflows:
  gateless:
    autonomy: high
    escalations:
      - match:
          paths: ["infra/**"]
        require:
          max_autonomy: low
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
`)); err != nil {
		t.Fatalf("ParseBytes: %v, want a max_autonomy-only escalation on a gate-less workflow to be accepted", err)
	}
}

// memberOfDoc renders a workflow with TWO approval gates whose member_of
// declarations the caller chooses, plus one member_of escalation.
func memberOfDoc(gateOne, gateTwo, escalated string) []byte {
	memberLine := func(group string) string {
		if group == "" {
			return ""
		}
		return "\n              member_of: " + group
	}
	return []byte(`
version: "2"
workflows:
  feature_change:
    escalations:
      - match:
          paths: ["infra/**"]
        require:
          approvals:
            member_of: ` + escalated + `
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvals:
              count: 1` + memberLine(gateOne) + `
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
        gates:
          - type: approval
            approvals:
              count: 1` + memberLine(gateTwo) + `
`)
}

// TestValidate_Escalations_MemberOfBoundaryPair is CONDITION C and its
// boundary: the check is a NO-OP check, not a lowering check. A group EVERY
// applicable approval gate already requires de-duplicates away under the
// conjunction and is refused; the SAME group named by only one of two gates
// genuinely raises the other and is accepted; a group no gate names is the
// ordinary raising case.
func TestValidate_Escalations_MemberOfBoundaryPair(t *testing.T) {
	t.Run("every approval gate already requires the group", func(t *testing.T) {
		_, err := spec.ParseBytes(memberOfDoc("platform/security", "platform/security", "platform/security"))
		var ve *spec.ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("err = %v, want *ValidationError for a member_of every gate already requires", err)
		}
		if ve.Path != "/workflows/feature_change/escalations/0/require/approvals/member_of" {
			t.Errorf("path = %q, want the member_of pointer", ve.Path)
		}
		assertNoRaiseShape(t, ve.Message, "feature_change", 0, "approvals.member_of")
	})

	t.Run("only one of two gates requires the group", func(t *testing.T) {
		if _, err := spec.ParseBytes(memberOfDoc("platform/security", "", "platform/security")); err != nil {
			t.Fatalf("ParseBytes: %v, want acceptance — the escalation raises the gate that omits the group", err)
		}
	})

	t.Run("no gate requires the group", func(t *testing.T) {
		if _, err := spec.ParseBytes(memberOfDoc("", "", "platform/security")); err != nil {
			t.Fatalf("ParseBytes: %v, want acceptance for the ordinary raising case", err)
		}
	})

	t.Run("a different group than the one both gates require", func(t *testing.T) {
		if _, err := spec.ParseBytes(memberOfDoc("platform/security", "platform/security", "org/founders")); err != nil {
			t.Fatalf("ParseBytes: %v, want acceptance — the escalated group narrows the composed conjunction", err)
		}
	})
}

// TestValidate_Escalations_MaxAutonomyNoOpAcrossGateBlocks pins the ceiling
// no-op check's scope. A gate declaring its own autonomy block overrides the
// workflow's WHOLESALE, so a ceiling inert at workflow level can still
// genuinely restrict that gate — and must not be rejected as a no-op.
func TestValidate_Escalations_MaxAutonomyNoOpAcrossGateBlocks(t *testing.T) {
	doc := func(workflowTier, gateTier, ceiling string) []byte {
		return []byte(`
version: "2"
workflows:
  feature_change:
    autonomy: ` + workflowTier + `
    escalations:
      - match:
          paths: ["infra/**"]
        require:
          max_autonomy: ` + ceiling + `
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            autonomy: ` + gateTier + `
            approvals:
              count: 1
`)
	}

	t.Run("inert at workflow level but clamps a gate block", func(t *testing.T) {
		if _, err := spec.ParseBytes(doc("low", "high", "medium")); err != nil {
			t.Fatalf("ParseBytes: %v, want acceptance — the ceiling clamps the gate's own high block", err)
		}
	})

	t.Run("inert everywhere is refused", func(t *testing.T) {
		_, err := spec.ParseBytes(doc("low", "low", "medium"))
		var ve *spec.ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("err = %v, want *ValidationError for a ceiling that clamps nothing anywhere", err)
		}
		assertNoRaiseShape(t, ve.Message, "feature_change", 0, "max_autonomy")
	})

	t.Run("a workflow declaring no autonomy at all has nothing to clamp", func(t *testing.T) {
		_, err := spec.ParseBytes([]byte(`
version: "2"
workflows:
  feature_change:
    escalations:
      - match:
          paths: ["infra/**"]
        require:
          max_autonomy: low
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
`))
		var ve *spec.ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("err = %v, want *ValidationError — a workflow delegating nothing has nothing for a ceiling to restrict", err)
		}
		assertNoRaiseShape(t, ve.Message, "feature_change", 0, "max_autonomy")
	})
}

// TestValidate_Escalations_SecondEntryReportsItsOwnIndex pins that the
// reported index is the offending entry's, not always zero.
func TestValidate_Escalations_SecondEntryReportsItsOwnIndex(t *testing.T) {
	_, err := spec.ParseBytes(escalationV2Doc(escalationBlock(`      - match:
          paths: ["infra/**"]
        require:
          approvals:
            count: 3
      - match:
          labels: [security]
        require:
          approvals:
            count: 1`)))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError for the second entry", err)
	}
	if !strings.Contains(ve.Path, "/escalations/1/") {
		t.Errorf("path = %q, want it to name escalation index 1", ve.Path)
	}
	assertNoRaiseShape(t, ve.Message, "feature_change", 1, "approvals.count")
}

// TestParse_Escalations_ExtendsDoesNotInherit is CONDITION 3's pin, sited in
// THIS file deliberately (an edit to v2reuse_test.go would fall outside this
// change's declared scope and the pin would vanish from the diff). `extends`
// folds STAGES only, so a deriving workflow does NOT inherit its base's
// escalations — matching `applies_to` and every other non-stage member.
func TestParse_Escalations_ExtendsDoesNotInherit(t *testing.T) {
	s, err := spec.ParseBytes([]byte(`
version: "2"
workflows:
  base:
    autonomy: high
    escalations:
      - match:
          paths: ["infra/**"]
        require:
          max_autonomy: low
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
  derived:
    extends: base
`))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if got := len(s.Workflows["base"].Escalations); got != 1 {
		t.Fatalf("base.Escalations = %d entries, want 1", got)
	}
	if got := s.Workflows["derived"].Escalations; got != nil {
		t.Errorf("derived.Escalations = %#v, want nil — extends folds stages only", got)
	}
	if got := len(s.Workflows["derived"].Stages); got != 1 {
		t.Errorf("derived.Stages = %d, want the base's 1 stage (the control that extends resolved at all)", got)
	}
}

// TestParse_Escalations_ExtendsReportsAgainstTheDeclaringWorkflow is the
// error-attribution half of the same pin: a base's invalid escalation is
// reported against the BASE, never attributed to the deriving workflow.
func TestParse_Escalations_ExtendsReportsAgainstTheDeclaringWorkflow(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "2"
workflows:
  base:
    escalations:
      - match:
          change_kind: [refactor]
        require:
          max_autonomy: low
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
  derived:
    extends: base
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError naming the base's escalation", err)
	}
	if !strings.Contains(ve.Path, "/workflows/base/escalations/0/") {
		t.Errorf("path = %q, want it reported against the declaring workflow (base)", ve.Path)
	}
	if strings.Contains(ve.Path, "/workflows/derived/escalations") {
		t.Errorf("path = %q, want no escalation error against derived", ve.Path)
	}
}

// --- escalation match.paths on a plan-less workflow (E53.16 / #2382) ---
//
// An escalation's `match.paths` is evaluated at the approval gate against the
// run's approved-plan scope.files (planGateScopePaths via
// resolveStageEscalations). A workflow declaring no plan stage never ships a
// plan, so the escalation is never evaluated — it never FIRES, with no
// rejection and no audit entry to signal it did nothing. This is the
// escalation twin of applies_to's plan-stage rule (#2377), and worse: the
// control fails to RAISE rather than merely fails to route. The rule below
// refuses that declaration at authoring time, UNCONDITIONALLY: nothing admits
// it, and in particular a stage's post-hoc `constraints.allowed_paths` does
// not, because the escalation is still never evaluated.

// escalationPlanlessWorkflowV2 renders a minimal v2 document whose single
// workflow declares implement + review stages and NO plan stage — the shape
// three of this repository's four workflows actually have — plus a
// workflow-level `autonomy: high` block so a `require: {max_autonomy: low}`
// escalation genuinely RAISES (the only-ever-raise family then passes and the
// paths rung is the candidate for the error). The caller supplies the
// `escalations` block already indented four spaces (via escalationBlock).
func escalationPlanlessWorkflowV2(escalations string) []byte {
	return []byte(`
version: "2"
workflows:
  trivial:
    autonomy: high
` + escalations + `
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
      - id: review
        type: review
        executor:
          human: true
`)
}

// planlessRaisingEscalation is the shared escalation body for the plan-less
// fixture: one paths (or otherwise) match plus a genuinely-raising
// `max_autonomy: low` require, so the only-ever-raise rungs pass and rung 8 is
// the sole candidate for a rejection.
func planlessRaisingEscalation(match string) string {
	return escalationBlock(`      - match:
` + match + `
        require:
          max_autonomy: low`)
}

// TestParse_Escalations_PathsNoPlanStage_Rejected is the refusal itself,
// asserted at the exact pointer and against the SHIPPED constant rather than a
// paraphrase (a comment-only or no-op touch of escalation.go fails here where a
// scope-presence gate would pass, #1169).
func TestParse_Escalations_PathsNoPlanStage_Rejected(t *testing.T) {
	_, err := spec.ParseBytes(escalationPlanlessWorkflowV2(planlessRaisingEscalation(`          paths: ["**/crypto/**"]`)))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError for match.paths on a plan-less workflow", err)
	}
	if ve.Path != "/workflows/trivial/escalations/0/match/paths" {
		t.Errorf("Path = %q, want /workflows/trivial/escalations/0/match/paths", ve.Path)
	}
	want := fmt.Sprintf(spec.MsgFmtEscalationPathsNoPlanStage, "trivial", 0)
	if ve.Message != want {
		t.Errorf("Message = %q, want the shipped rendering %q", ve.Message, want)
	}
}

// TestParse_Escalations_PathsNoPlanStage_MessageNamesFixes is the done-means
// assertion behind the operator's message-shape requirement: the rendered text
// names the WORKFLOW, the missing plan stage as the REASON, both admission-time
// alternatives (labels / trigger) and the non-paths require dimensions that
// still hold. A vaguely-worded constant satisfies a substring gate structurally
// while failing the reader.
func TestParse_Escalations_PathsNoPlanStage_MessageNamesFixes(t *testing.T) {
	msg := fmt.Sprintf(spec.MsgFmtEscalationPathsNoPlanStage, "routine_change", 2)
	for _, want := range []string{
		"routine_change",
		"plan stage",
		"labels",
		"trigger",
		"approvals",
		"min_permission",
		"max_autonomy",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message = %q, want it to name %q", msg, want)
		}
	}
}

// TestParse_Escalations_PlanlessLabelsOnlyAccepted is a POSITIVE CONTROL the
// issue names as mattering most: a `labels`-only escalation match still
// validates on a plan-less workflow, so the fix removes no escalation form
// this repository's three plan-less workflows can carry.
func TestParse_Escalations_PlanlessLabelsOnlyAccepted(t *testing.T) {
	if _, err := spec.ParseBytes(escalationPlanlessWorkflowV2(planlessRaisingEscalation(`          labels: ["area:auth"]`))); err != nil {
		t.Fatalf("ParseBytes = %v, want nil for a labels-only escalation on a plan-less workflow", err)
	}
}

// TestParse_Escalations_PlanlessTriggerOnlyAccepted is the second POSITIVE
// CONTROL: a `trigger`-only escalation match validates on a plan-less workflow.
func TestParse_Escalations_PlanlessTriggerOnlyAccepted(t *testing.T) {
	if _, err := spec.ParseBytes(escalationPlanlessWorkflowV2(planlessRaisingEscalation(`          trigger: [diff]`))); err != nil {
		t.Fatalf("ParseBytes = %v, want nil for a trigger-only escalation on a plan-less workflow", err)
	}
}

// TestParse_Escalations_PathsWithPlanStage_Accepted is the positive control on
// the plan-BEARING side: the identical paths match validates clean when the
// workflow declares a plan stage, so the rule is "rejects exactly this" rather
// than "rejects something".
func TestParse_Escalations_PathsWithPlanStage_Accepted(t *testing.T) {
	if _, err := spec.ParseBytes(escalationV2Doc(escalationBlock(`      - match:
          paths: ["infra/**"]
        require:
          approvals:
            count: 3`))); err != nil {
		t.Fatalf("ParseBytes = %v, want nil for paths on a plan-bearing workflow", err)
	}
}

// TestParse_Escalations_PlanlessLabelsPlusPaths_Rejected pins that the rule
// keys on the PRESENCE of `paths`, not on exclusivity: an escalation otherwise
// satisfiable through `labels` is still refused, at the paths pointer, because
// the paths half of it would never be evaluated.
func TestParse_Escalations_PlanlessLabelsPlusPaths_Rejected(t *testing.T) {
	_, err := spec.ParseBytes(escalationPlanlessWorkflowV2(planlessRaisingEscalation(`          labels: ["area:auth"]
          paths: ["**/crypto/**"]`)))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError; labels alongside paths must not admit the escalation", err)
	}
	if ve.Path != "/workflows/trivial/escalations/0/match/paths" {
		t.Errorf("Path = %q, want /workflows/trivial/escalations/0/match/paths", ve.Path)
	}
}

// TestParse_Escalations_PathsWithInheritedPlanStage_Accepted proves the rung
// reads the RESOLVED stage list: a deriving workflow inheriting its only plan
// stage through `extends` DOES produce a scope.files set, so its paths
// escalation is evaluable and accepted. It doubles as the reuse-resolution seam
// test.
func TestParse_Escalations_PathsWithInheritedPlanStage_Accepted(t *testing.T) {
	if _, err := spec.ParseBytes([]byte(`
version: "2"
workflows:
  base:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
  derived:
    extends: base
    autonomy: high
    escalations:
      - match:
          paths: ["infra/**"]
        require:
          max_autonomy: low
`)); err != nil {
		t.Fatalf("ParseBytes = %v, want nil; the inherited plan stage makes the escalation evaluable", err)
	}
}

// TestParse_Escalations_PlanlessBaseAttribution is the attribution pin: a
// plan-less BASE's own bad escalation is reported against the BASE, never the
// deriving workflow — `extends` folds stages only (the sibling of
// TestParse_Escalations_ExtendsReportsAgainstTheDeclaringWorkflow already sited
// in this file).
func TestParse_Escalations_PlanlessBaseAttribution(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "2"
workflows:
  base:
    autonomy: high
    escalations:
      - match:
          paths: ["infra/**"]
        require:
          max_autonomy: low
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
  derived:
    extends: base
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError naming the base's escalation", err)
	}
	if ve.Path != "/workflows/base/escalations/0/match/paths" {
		t.Errorf("Path = %q, want it reported against the declaring workflow (base)", ve.Path)
	}
	if strings.Contains(ve.Message, "derived") {
		t.Errorf("Message = %q, want no attribution to derived; extends folds stages only", ve.Message)
	}
}

// TestParse_Escalations_PlanlessPathsNotAdmittedByPostHocEnvelope is the
// LOAD-BEARING regression test for the rule's unconditionality, and the only
// case in this block a reintroduced carve-out would fail.
//
// The rejected exemption was: admit a plan-less `match.paths` when some stage
// already declares a post-hoc path envelope (`constraints.allowed_paths` /
// `forbidden_paths`). It is wrong on the merits — the two controls answer
// different questions at different times. The escalation's paths decide, at the
// approval gate against the approved plan's scope.files, WHETHER THE ESCALATION
// FIRES; a stage constraint checks the PRODUCED DIFF after the agent has run. A
// plan-less workflow still never produces the set the escalation is read
// against, so it still never fires — the silently-inert control #2382 closes.
// Every OTHER plan-less fixture in this block declares no constraints, so a
// carve-out keyed on one would leave them all green; this test declares the
// envelope explicitly and requires the refusal to stand.
func TestParse_Escalations_PlanlessPathsNotAdmittedByPostHocEnvelope(t *testing.T) {
	// The stage produces pull_request, so the E52.7 post-hoc-constraint
	// binding rule is satisfied and the escalation rule is the only candidate
	// for the error these cases must still report.
	planless := func(constraints string) []byte {
		return []byte(`
version: "2"
workflows:
  trivial:
    autonomy: high
    escalations:
      - match:
          paths: ["infra/**"]
        require:
          max_autonomy: low
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
` + constraints + `        produces:
          - artifact: pull_request
`)
	}
	cases := []struct {
		name        string
		constraints string
	}{
		{name: "allowed_paths", constraints: "          allowed_paths: [\"infra/**\"]\n"},
		{name: "forbidden_paths", constraints: "          forbidden_paths: [\"backend/**\"]\n"},
		{name: "both", constraints: "          allowed_paths: [\"infra/**\"]\n          forbidden_paths: [\"backend/**\"]\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := spec.ParseBytes(planless(tc.constraints))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want the match.paths refusal to stand; a stage's post-hoc path envelope must NOT admit an escalation that still never fires (#2382)", err)
			}
			if ve.Path != "/workflows/trivial/escalations/0/match/paths" {
				t.Fatalf("Path = %q, want /workflows/trivial/escalations/0/match/paths", ve.Path)
			}
			want := fmt.Sprintf(spec.MsgFmtEscalationPathsNoPlanStage, "trivial", 0)
			if ve.Message != want {
				t.Errorf("Message = %q, want the unchanged shipped rendering %q", ve.Message, want)
			}
		})
	}
}

// TestValidate_Escalations_ChangeKindWinsOverNoPlanStage is the rung-1 ORDER
// contract: a plan-less workflow whose escalation declares both an unsupported
// change_kind and a paths criterion reports the change_kind diagnosis, which
// names a fix the plan-stage message cannot.
func TestValidate_Escalations_ChangeKindWinsOverNoPlanStage(t *testing.T) {
	_, err := spec.ParseBytes(escalationPlanlessWorkflowV2(planlessRaisingEscalation(`          change_kind: [refactor]
          paths: ["infra/**"]`)))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if ve.Path != "/workflows/trivial/escalations/0/match/change_kind" {
		t.Errorf("Path = %q, want the change_kind pointer", ve.Path)
	}
	if ve.Message != spec.MsgEscalationChangeKindUnsupported {
		t.Errorf("Message = %q, want the change_kind rejection to win over the plan-stage rule", ve.Message)
	}
}

// TestValidate_Escalations_MalformedGlobWinsOverNoPlanStage is the rung-2 ORDER
// contract: a malformed glob on a plan-less workflow reports the SHARED
// predicate grammar error at the match pointer, proving the paths rung runs
// last.
func TestValidate_Escalations_MalformedGlobWinsOverNoPlanStage(t *testing.T) {
	_, err := spec.ParseBytes(escalationPlanlessWorkflowV2(planlessRaisingEscalation(`          paths: ["infra/["]`)))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if ve.Path != "/workflows/trivial/escalations/0/match" {
		t.Errorf("Path = %q, want the predicate's own pointer, not .../match/paths", ve.Path)
	}
	if !strings.Contains(ve.Message, "malformed path glob") {
		t.Errorf("Message = %q, want the shared predicate grammar error to win over the plan-stage rule", ve.Message)
	}
}

// TestValidate_Escalations_NoRaiseWinsOverNoPlanStage pins the LAST-rung
// placement as a reviewable decision rather than an emergent property of
// statement order: a no-raise `require.approvals.count` alongside an inert
// paths match on a plan-less workflow reports the only-ever-raise diagnosis
// (a require-side rung, 4), not the paths refusal. The plan-less fixture here
// declares an approval gate on a NON-plan stage so a count baseline exists.
func TestValidate_Escalations_NoRaiseWinsOverNoPlanStage(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "2"
workflows:
  trivial:
    autonomy: high
    escalations:
      - match:
          paths: ["infra/**"]
        require:
          approvals:
            count: 1
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
        gates:
          - type: approval
            approvals:
              count: 2
      - id: review
        type: review
        executor:
          human: true
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if ve.Path != "/workflows/trivial/escalations/0/require/approvals/count" {
		t.Errorf("Path = %q, want the count raise-check to win over the plan-stage rule", ve.Path)
	}
	assertNoRaiseShape(t, ve.Message, "trivial", 0, "approvals.count")
}

// TestValidate_Escalations_PathsNoPlanStageGoLayerBranch drives the rung
// through the exported Validate over a hand-built typed Spec whose workflow has
// zero plan stages, so the fail-closed code is proved reachable rather than
// assumed live behind a parse. An empty require passes the only-ever-raise
// rungs, leaving the paths rung as the sole candidate.
func TestValidate_Escalations_PathsNoPlanStageGoLayerBranch(t *testing.T) {
	s := &spec.Spec{
		Version: "2",
		Workflows: map[string]spec.Workflow{"routed": {
			Escalations: []spec.Escalation{{Match: spec.Predicate{Paths: []string{"crypto/**"}}}},
		}},
	}
	err := spec.Validate(s)
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError from the Go layer", err)
	}
	if ve.Path != "/workflows/routed/escalations/0/match/paths" {
		t.Errorf("Path = %q, want /workflows/routed/escalations/0/match/paths", ve.Path)
	}
	want := fmt.Sprintf(spec.MsgFmtEscalationPathsNoPlanStage, "routed", 0)
	if ve.Message != want {
		t.Errorf("Message = %q, want %q", ve.Message, want)
	}
}

// TestParse_Escalations_MultipleEntriesReportsOffendingIndex pins index
// correctness through both the pointer and the rendered message: a clean entry
// 0 (labels) followed by a paths-bearing entry 1 reports index 1 in BOTH.
func TestParse_Escalations_MultipleEntriesReportsOffendingIndex(t *testing.T) {
	_, err := spec.ParseBytes(escalationPlanlessWorkflowV2(escalationBlock(`      - match:
          labels: ["area:auth"]
        require:
          max_autonomy: low
      - match:
          paths: ["**/crypto/**"]
        require:
          max_autonomy: low`)))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError for the second entry", err)
	}
	if ve.Path != "/workflows/trivial/escalations/1/match/paths" {
		t.Errorf("Path = %q, want it to name escalation index 1", ve.Path)
	}
	want := fmt.Sprintf(spec.MsgFmtEscalationPathsNoPlanStage, "trivial", 1)
	if ve.Message != want {
		t.Errorf("Message = %q, want the rendering for index 1 %q", ve.Message, want)
	}
}

// --- composition ---

// intPtr is a local helper for the *int count dimension.
func intPtr(v int) *int { return &v }

// composeFixture is criterion 3's multi-match fixture: three escalations
// differing on EVERY dimension, so the composed result can only be right if
// each dimension composes independently.
func composeFixture() []spec.Escalation {
	return []spec.Escalation{
		{Require: spec.EscalationRequirements{
			Approvals:   &spec.EscalatedApprovals{Count: intPtr(2), MemberOf: "platform/security", MinPermission: "write"},
			MaxAutonomy: spec.TierMedium,
		}},
		{Require: spec.EscalationRequirements{
			Approvals:   &spec.EscalatedApprovals{Count: intPtr(4), MemberOf: "org/founders", MinPermission: "maintain"},
			MaxAutonomy: spec.TierHigh,
		}},
		{Require: spec.EscalationRequirements{
			Approvals:   &spec.EscalatedApprovals{Count: intPtr(3), MemberOf: "platform/security", MinPermission: "admin"},
			MaxAutonomy: spec.TierLow,
		}},
	}
}

func TestComposeEscalations_Strictest(t *testing.T) {
	got := spec.ComposeEscalations(composeFixture())
	if got.Count == nil || *got.Count != 4 {
		t.Errorf("Count = %v, want the max, 4", got.Count)
	}
	if !reflect.DeepEqual(got.MemberOf, []string{"org/founders", "platform/security"}) {
		t.Errorf("MemberOf = %v, want the sorted de-duplicated union", got.MemberOf)
	}
	if got.MinPermission != "admin" {
		t.Errorf("MinPermission = %q, want the strictest tier admin", got.MinPermission)
	}
	if got.MaxAutonomy != spec.TierLow {
		t.Errorf("MaxAutonomy = %q, want the lowest tier low", got.MaxAutonomy)
	}
}

// TestComposeEscalations_OrderIndependent is the structural refutation of
// last-match-wins: the same fixture in reversed and shuffled declaration order
// composes identically, because max / min / set-union are commutative.
func TestComposeEscalations_OrderIndependent(t *testing.T) {
	want := spec.ComposeEscalations(composeFixture())
	orders := [][]int{
		{2, 1, 0},
		{1, 2, 0},
		{0, 2, 1},
		{2, 0, 1},
	}
	for _, order := range orders {
		src := composeFixture()
		shuffled := make([]spec.Escalation, 0, len(order))
		for _, i := range order {
			shuffled = append(shuffled, src[i])
		}
		if got := spec.ComposeEscalations(shuffled); !reflect.DeepEqual(got, want) {
			t.Errorf("order %v composed to %+v, want the order-independent %+v", order, got, want)
		}
	}
}

// TestComposeEscalations_Empty pins the zero case both seams short-circuit on.
func TestComposeEscalations_Empty(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fired []spec.Escalation
	}{
		{name: "nil", fired: nil},
		{name: "empty", fired: []spec.Escalation{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := spec.ComposeEscalations(tc.fired)
			if !got.IsZero() {
				t.Errorf("ComposeEscalations(%v) = %+v, want the zero requirement", tc.fired, got)
			}
		})
	}
}

// TestComposedRequirements_IsZero pins the accessor both seams branch on.
func TestComposedRequirements_IsZero(t *testing.T) {
	cases := []struct {
		name string
		req  spec.ComposedRequirements
		want bool
	}{
		{name: "zero value", req: spec.ComposedRequirements{}, want: true},
		{name: "count raised", req: spec.ComposedRequirements{Count: intPtr(2)}, want: false},
		{name: "membership raised", req: spec.ComposedRequirements{MemberOf: []string{"org/x"}}, want: false},
		{name: "permission raised", req: spec.ComposedRequirements{MinPermission: "admin"}, want: false},
		{name: "ceiling raised", req: spec.ComposedRequirements{MaxAutonomy: spec.TierLow}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.req.IsZero(); got != tc.want {
				t.Errorf("IsZero() = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- the autonomy clamp ---

// resolvedByAction indexes a matrix's actions for assertion.
func resolvedByAction(rm *spec.ResolvedMatrix) map[string]spec.ResolvedAction {
	out := make(map[string]spec.ResolvedAction, len(rm.Actions))
	for _, a := range rm.Actions {
		out[a.Action] = a
	}
	return out
}

// TestClampResolvedMatrix_EveryModeAndTier is criterion 4's exhaustive table:
// every (declared mode, ceiling tier) pair over the merge class, which `high`
// holds at auto and `low`/`medium` do not.
func TestClampResolvedMatrix_EveryModeAndTier(t *testing.T) {
	modes := []struct {
		name string
		mode spec.ActionMode
		cond spec.DelegationCondition
	}{
		{name: "auto", mode: spec.ModeAuto, cond: spec.ConditionGatesResolvedCIGreen},
		{name: "gated", mode: spec.ModeGated},
		{name: "report", mode: spec.ModeReport},
	}
	ceilings := []spec.AutonomyTier{spec.TierLow, spec.TierMedium, spec.TierHigh}

	for _, m := range modes {
		for _, ceiling := range ceilings {
			t.Run(fmt.Sprintf("%s under %s", m.name, ceiling), func(t *testing.T) {
				in := &spec.ResolvedMatrix{Actions: []spec.ResolvedAction{{
					Action:    spec.ActionMerge,
					Mode:      m.mode,
					Condition: m.cond,
					Source:    spec.SourceExplicit,
				}}}
				got := resolvedByAction(spec.ClampResolvedMatrix(in, ceiling))[spec.ActionMerge]

				// The ceiling clamps ONLY an auto class the ceiling tier does
				// not itself hold at auto. `high` holds merge at auto; low and
				// medium do not.
				clamped := m.mode == spec.ModeAuto && ceiling != spec.TierHigh
				if clamped {
					if got.Mode != spec.ModeGated {
						t.Errorf("mode = %q, want gated under ceiling %q", got.Mode, ceiling)
					}
					if got.Condition != "" {
						t.Errorf("condition = %q, want it dropped on a clamped class", got.Condition)
					}
					if got.Source != spec.SourceEscalation {
						t.Errorf("source = %q, want %q", got.Source, spec.SourceEscalation)
					}
					return
				}
				if got.Mode != m.mode {
					t.Errorf("mode = %q, want the declared %q left untouched", got.Mode, m.mode)
				}
				if got.Condition != m.cond {
					t.Errorf("condition = %q, want the declared %q preserved", got.Condition, m.cond)
				}
				if got.Source != spec.SourceExplicit {
					t.Errorf("source = %q, want the original %q preserved", got.Source, spec.SourceExplicit)
				}
			})
		}
	}
}

// TestClampResolvedMatrix_NeverWidens is the monotonicity proof the
// max_autonomy no-op check rests on: over every tier expansion and every
// ceiling, no class is ever promoted toward auto.
func TestClampResolvedMatrix_NeverWidens(t *testing.T) {
	rank := map[spec.ActionMode]int{spec.ModeGated: 0, spec.ModeReport: 1, spec.ModeAuto: 2}
	for _, declared := range []spec.AutonomyTier{spec.TierLow, spec.TierMedium, spec.TierHigh} {
		for _, ceiling := range []spec.AutonomyTier{spec.TierLow, spec.TierMedium, spec.TierHigh} {
			wf := &spec.Workflow{Autonomy: declared}
			before := spec.ResolveAutonomy(wf, nil)
			after := resolvedByAction(spec.ClampResolvedMatrix(before, ceiling))
			for _, a := range before.Actions {
				if rank[after[a.Action].Mode] > rank[a.Mode] {
					t.Errorf("tier %q under ceiling %q widened %q from %q to %q",
						declared, ceiling, a.Action, a.Mode, after[a.Action].Mode)
				}
			}
		}
	}
}

// TestClampResolvedMatrix_ExtensionClassClampsToGated pins the fail-closed
// branch: a class no ceiling tier expansion names has no auto entry to
// compare, so it clamps rather than being waved through by enumeration.
func TestClampResolvedMatrix_ExtensionClassClampsToGated(t *testing.T) {
	in := &spec.ResolvedMatrix{Actions: []spec.ResolvedAction{{
		Action: "publish_release", Mode: spec.ModeAuto, Condition: "some_condition", Source: spec.SourceExplicit,
	}}}
	got := resolvedByAction(spec.ClampResolvedMatrix(in, spec.TierHigh))["publish_release"]
	if got.Mode != spec.ModeGated || got.Source != spec.SourceEscalation || got.Condition != "" {
		t.Errorf("extension class = %+v, want gated with the condition dropped and source escalation", got)
	}
}

// TestClampResolvedMatrix_DoesNotMutateInput is the shared-spec safety pin: a
// cached parsed spec must never be clamped out from under another run.
func TestClampResolvedMatrix_DoesNotMutateInput(t *testing.T) {
	in := spec.ResolveAutonomy(&spec.Workflow{Autonomy: spec.TierHigh}, nil)
	before := resolvedByAction(in)[spec.ActionMerge]
	out := spec.ClampResolvedMatrix(in, spec.TierLow)
	after := resolvedByAction(in)[spec.ActionMerge]
	if !reflect.DeepEqual(before, after) {
		t.Errorf("input matrix mutated: %+v -> %+v", before, after)
	}
	if got := resolvedByAction(out)[spec.ActionMerge]; got.Mode != spec.ModeGated {
		t.Errorf("returned copy merge mode = %q, want gated", got.Mode)
	}
}

// TestClampResolvedMatrix_NoOpInputs pins the two documented no-op branches.
func TestClampResolvedMatrix_NoOpInputs(t *testing.T) {
	if got := spec.ClampResolvedMatrix(nil, spec.TierLow); got != nil {
		t.Errorf("ClampResolvedMatrix(nil, low) = %+v, want nil", got)
	}
	in := spec.ResolveAutonomy(&spec.Workflow{Autonomy: spec.TierHigh}, nil)
	if got := spec.ClampResolvedMatrix(in, ""); !reflect.DeepEqual(got, in) {
		t.Errorf("an empty ceiling changed the matrix: %+v, want %+v", got, in)
	}
}

// TestClampResolvedMatrix_PreservesBlockMembers pins that the clamp touches
// action modes ONLY — the page list, the model policy and the declared tier
// survive it, so a clamped matrix is still a complete block.
func TestClampResolvedMatrix_PreservesBlockMembers(t *testing.T) {
	in := spec.ResolveAutonomy(&spec.Workflow{Autonomy: spec.TierHigh}, nil)
	out := spec.ClampResolvedMatrix(in, spec.TierLow)
	if out.Tier != in.Tier {
		t.Errorf("Tier = %q, want the declared %q preserved", out.Tier, in.Tier)
	}
	if !reflect.DeepEqual(out.PageHumanOn, in.PageHumanOn) {
		t.Errorf("PageHumanOn = %v, want %v", out.PageHumanOn, in.PageHumanOn)
	}
	if len(out.Actions) != len(in.Actions) {
		t.Errorf("clamped matrix has %d actions, want the input's %d", len(out.Actions), len(in.Actions))
	}
}
