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
