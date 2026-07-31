package spec

import (
	"fmt"
	"reflect"
	"sort"
)

// Per-path escalations (E53.4 / #2227).
//
// An escalation is one `{match: <predicate>, require: {...}}` rule that RAISES
// the requirements applying to a change the predicate matches. This file owns
// the DECLARATION side: the types, the strictest-composition rule, the
// only-ever-raise validation family, and the pure autonomy clamp. The two
// enforcement seams that CONSUME it — the approval gate and delegation
// resolution — live in backend/internal/server and backend/internal/delegation.
//
// THE `match:` WRAPPER IS STRUCTURALLY FORCED, not a style choice.
// $defs/predicate is additionalProperties:false, and JSON Schema Draft 2020-12
// evaluates additionalProperties against the WHOLE instance object rather than
// the referencing subschema's own scope, so an escalation cannot $ref the
// predicate and carry a sibling `require` key. Hoisting `paths:` to the
// escalation level would mean re-declaring the predicate's properties inline —
// the second-matcher drift E53.1 exists to prevent on a predicate that now
// backs applies_to across three seams (#2361).
//
// ONLY-EVER-RAISE is enforced at DECLARATION time, in four checks that are not
// all the same shape:
//
//	count           RAISE-check   must exceed the max count over every approval gate
//	min_permission  RAISE-check   must exceed the strictest tier over those gates
//	member_of       NO-OP check   refused only when EVERY applicable gate already
//	                              names it (the conjunction de-duplicates it)
//	max_autonomy    NO-OP check   refused when the clamp leaves the matrix identical
//
// The two no-op checks are NOT lowering checks, and the distinction matters in
// review. Membership composes as a CONJUNCTION over a de-duplicated set, so
// adding a group can only ever narrow the eligible approver set — no lowering
// is expressible. The clamp only ever downgrades auto->gated, so a ceiling is
// monotone-decreasing by construction — again no lowering is expressible. What
// IS expressible on both dimensions is an escalation that raises NOTHING, and
// that is the defect these two checks refuse.

// Escalation is one declared escalation rule: the change it applies to, and
// the requirements it raises for such a change.
type Escalation struct {
	// Match is the shared path predicate (E53.1 / #2224) — the same matcher
	// `applies_to` consumes, deliberately not a second one.
	Match Predicate `json:"match" yaml:"match"`
	// Require is the raised requirement block. The schema forbids an empty
	// one (minProperties 1 at BOTH levels), so a schema-valid escalation
	// always declares at least one dimension.
	Require EscalationRequirements `json:"require" yaml:"require"`
}

// EscalationRequirements is one escalation's raised requirements. Every
// dimension is individually optional; the schema's minProperties 1 forbids the
// empty block.
type EscalationRequirements struct {
	// Approvals raises the approval-gate predicate. Nil means this escalation
	// does not touch approvals.
	Approvals *EscalatedApprovals `json:"approvals,omitempty" yaml:"approvals,omitempty"`
	// MaxAutonomy is a CEILING on agent autonomy, applied LAST over the fully
	// resolved action matrix. Empty means no ceiling.
	MaxAutonomy AutonomyTier `json:"max_autonomy,omitempty" yaml:"max_autonomy,omitempty"`
}

// EscalatedApprovals is the raised approval-gate requirement. It is a strict
// SUBSET of Approvals: only the three dimensions an escalation may raise are
// expressible, so `not` and `members` cannot be restated (and therefore cannot
// be weakened) from an escalation.
//
// Count is a *int so an undeclared count is distinguishable from a declared
// zero — the schema's minimum is 1, so a schema-valid block never carries the
// latter, but the pointer keeps the distinction observable in a hand-built
// Escalation and is what makes the composition's "declared at all" test exact.
type EscalatedApprovals struct {
	Count         *int   `json:"count,omitempty" yaml:"count,omitempty"`
	MemberOf      string `json:"member_of,omitempty" yaml:"member_of,omitempty"`
	MinPermission string `json:"min_permission,omitempty" yaml:"min_permission,omitempty"`
}

// ComposedRequirements is the STRICTEST requirement across every escalation
// that fired for one change. It is the type both enforcement seams consume:
// the approval gate raises count / membership / permission from it, and
// delegation clamps its resolved matrix with MaxAutonomy.
//
// The zero value is "nothing raised", which is what a workflow declaring no
// escalations — and a change matching none of the ones it declares — resolves
// to, so the no-escalation case is a value rather than a special case.
type ComposedRequirements struct {
	// Count is the raised approval count, nil when no fired escalation
	// declared one.
	Count *int `json:"count,omitempty"`
	// MemberOf is the sorted, de-duplicated UNION of every fired
	// escalation's group. It is a CONJUNCTION: an approver must belong to
	// EVERY listed group. Two escalations naming disjoint groups therefore
	// produce a requirement no single approver can satisfy — the correct
	// fail-closed reading for a control that may only raise, surfacing as a
	// gate that cannot clear rather than one silently weakened.
	MemberOf []string `json:"member_of,omitempty"`
	// MinPermission is the strictest raised permission tier, empty when none
	// was declared.
	MinPermission string `json:"min_permission,omitempty"`
	// MaxAutonomy is the lowest (strictest) raised ceiling, empty when none
	// was declared.
	MaxAutonomy AutonomyTier `json:"max_autonomy,omitempty"`
}

// IsZero reports whether nothing was raised — the answer a resolver returns
// for a workflow that declares no escalations, or a change that matched none.
func (c ComposedRequirements) IsZero() bool {
	return c.Count == nil && len(c.MemberOf) == 0 && c.MinPermission == "" && c.MaxAutonomy == ""
}

// permissionOrder is the forge-neutral permission ladder, weakest first,
// mirroring backend/internal/identity.Permission minus `none` (a
// min_permission of none is meaningless, which is why the schema enum omits
// it). Index = strictness ordinal.
var permissionOrder = []string{"read", "triage", "write", "maintain", "admin"}

// permissionRank returns the strictness ordinal of a permission tier, or -1
// for an unrecognized / absent one (which therefore ranks below every declared
// tier — an absent baseline is no constraint, so any declared tier raises it).
func permissionRank(p string) int {
	for i, name := range permissionOrder {
		if name == p {
			return i
		}
	}
	return -1
}

// tierOrder is the autonomy ladder, most restrictive first. Index =
// permissiveness ordinal, so the LOWEST index is the strictest ceiling.
var tierOrder = []AutonomyTier{TierLow, TierMedium, TierHigh}

// tierRank returns the permissiveness ordinal of an autonomy tier, or -1 for
// an unrecognized / absent one.
func tierRank(t AutonomyTier) int {
	for i, name := range tierOrder {
		if name == t {
			return i
		}
	}
	return -1
}

// ComposeEscalations returns the STRICTEST requirement across every fired
// escalation, per dimension:
//
//	count           MAX
//	member_of       sorted de-duplicated UNION (a conjunction)
//	min_permission  strictest tier by ordinal
//	max_autonomy    lowest tier by ordinal
//
// ORDER-INDEPENDENCE IS STRUCTURAL, not asserted: max, min and set union are
// each commutative and associative, so shuffling the declarations cannot
// change the result. That is what makes "never last-match-wins" a property of
// the composition rather than a convention a later edit could break.
//
// An empty (or nil) fired list composes to the zero value — nothing raised.
func ComposeEscalations(fired []Escalation) ComposedRequirements {
	var out ComposedRequirements
	groups := make(map[string]struct{}, len(fired))
	for _, e := range fired {
		if a := e.Require.Approvals; a != nil {
			if a.Count != nil && (out.Count == nil || *a.Count > *out.Count) {
				c := *a.Count
				out.Count = &c
			}
			if a.MemberOf != "" {
				groups[a.MemberOf] = struct{}{}
			}
			if permissionRank(a.MinPermission) > permissionRank(out.MinPermission) {
				out.MinPermission = a.MinPermission
			}
		}
		if t := e.Require.MaxAutonomy; t != "" {
			if out.MaxAutonomy == "" || tierRank(t) < tierRank(out.MaxAutonomy) {
				out.MaxAutonomy = t
			}
		}
	}
	if len(groups) > 0 {
		out.MemberOf = make([]string, 0, len(groups))
		for g := range groups {
			out.MemberOf = append(out.MemberOf, g)
		}
		sort.Strings(out.MemberOf)
	}
	return out
}

// ClampResolvedMatrix applies an autonomy CEILING to a fully resolved matrix,
// returning a COPY — the input is never mutated, so a cached parsed spec
// cannot be clamped out from under another run.
//
// WHY THE RESOLVED MATRIX. The clamp is defined over the LAST artifact of
// resolution deliberately. Applying it to the tier, or to the declared
// `actions` entries, would let an explicit `actions: {merge: {mode: auto}}`
// re-widen the class afterwards — the exact failure the acceptance criteria
// name. Applied here, the ceiling wins over the tier AND over every explicit
// override, because both have already been resolved into this matrix. The
// caller must also RE-DERIVE the OperatorAgent knob block from the clamped
// matrix (DerivedOperatorAgent): every enforcement site reads the derived
// knobs, so clamping only the surfaced matrix would show `gated` on the run
// read while the agent stayed authorized to act.
//
// WHY THIS DIMENSION IS VALIDATED WITH A NO-OP CHECK RATHER THAN A
// RAISE-CHECK — the monotonicity argument, recorded here because this is where
// the reader meets it. The clamp only ever downgrades `auto` to `gated`; it
// never widens a class, never promotes a mode, and never adds a condition. A
// ceiling is therefore monotone-decreasing BY CONSTRUCTION and cannot lower
// any requirement, whatever the baseline is. A raise-check would in any case
// be ill-defined over the unified `actions` grammar: a workflow declaring
// `autonomy: low` with `actions: {merge: {mode: auto}}` has no single tier to
// compare a ceiling against, so a "highest tier declared" rule would reject
// ceilings that genuinely restrict and accept ones that do not. What remains
// expressible — and is refused at validation — is a ceiling that changes
// nothing, which is what validateEscalations' no-op check tests using exactly
// this function.
//
// A nil matrix or an empty ceiling is a no-op (the matrix is returned as-is).
// A class at ModeGated or ModeReport is untouched: neither delegates, and both
// already project to an EMPTY knob. An EXTENSION class — one the ceiling
// tier's expansion does not name — clamps to gated, fail-closed by
// construction rather than by enumeration.
func ClampResolvedMatrix(rm *ResolvedMatrix, ceiling AutonomyTier) *ResolvedMatrix {
	if rm == nil || ceiling == "" {
		return rm
	}
	allowed := expandTier(ceiling).Classes
	out := &ResolvedMatrix{
		Tier:        rm.Tier,
		Actions:     make([]ResolvedAction, len(rm.Actions)),
		PageHumanOn: rm.PageHumanOn,
		ModelPolicy: rm.ModelPolicy,
	}
	copy(out.Actions, rm.Actions)
	for i, a := range out.Actions {
		if a.Mode != ModeAuto {
			continue
		}
		if ceilingEntry, ok := allowed[a.Action]; ok && ceilingEntry.Mode == ModeAuto {
			continue
		}
		out.Actions[i].Mode = ModeGated
		out.Actions[i].Condition = ""
		out.Actions[i].Source = SourceEscalation
	}
	return out
}

// MsgEscalationChangeKindUnsupported is the rejection for a `change_kind`
// criterion inside an escalation's `match` (E53.4 / #2227). It is exported so
// a test can assert the shipped text rather than a paraphrase, and it is
// duplicated BYTE-FOR-BYTE in cli/internal/spec/validate.go — the two Go
// modules cannot share a package, so parity is held by the paired message
// assertions in both spec_test.go files, exactly as
// MsgAppliesToChangeKindUnsupported is.
//
// The reasoning is applies_to's verbatim: `change_kind` stays in the shared
// $defs/predicate for its other consumers, and only THIS consumer refuses it,
// because no producer populates Change.ChangeKind today. An escalation
// declaring it could never fire — indistinguishable, from the operator's side,
// from the escalation control being broken.
const MsgEscalationChangeKindUnsupported = "escalations does not accept the change_kind criterion in `match`: nothing produces a change kind today, so the criterion can never be satisfied and this escalation could never fire; match on paths, labels or trigger instead (the shared predicate keeps change_kind for its other consumers)"

// MsgFmtEscalationNoRaise is the ONE message shape every only-ever-raise
// rejection uses, so the four dimensions cannot drift into four differently
// actionable messages. Arguments, in order: workflow name, escalation index,
// dimension (e.g. "approvals.count"), the escalated value as written, the
// baseline it fails to raise, and the fix.
//
// Byte-identical in cli/internal/spec/validate.go; see
// MsgEscalationChangeKindUnsupported for why the constant is duplicated rather
// than shared.
const MsgFmtEscalationNoRaise = "workflow %q escalation %d: require.%s: %s does not raise %s; an escalation may only ever RAISE a requirement, so one that changes nothing is refused rather than silently accepted as a control that does something. %s"

// MsgFmtEscalationApprovalsNoApprovalGate is the rejection for a
// `require.approvals` block on a workflow that declares no approval gate at
// all: there is no gate for it to raise, anywhere, so it is inert wherever the
// escalation fires. Arguments: workflow name, escalation index.
//
// Byte-identical in cli/internal/spec/validate.go.
const MsgFmtEscalationApprovalsNoApprovalGate = "workflow %q escalation %d: require.approvals raises the approval gate, but workflow %q declares no approval gate for it to raise; declare an approval gate on a stage of this workflow, or drop require.approvals (an escalation may still raise max_autonomy on a gate-less workflow)"

// escalationNoRaiseMessage renders MsgFmtEscalationNoRaise. It is the shared
// shape helper the validation tests assert against, so "the same actionable
// shape" is mechanically true rather than eyeballed.
func escalationNoRaiseMessage(workflow string, idx int, dimension, escalated, baseline, fix string) string {
	return fmt.Sprintf(MsgFmtEscalationNoRaise, workflow, idx, dimension, escalated, baseline, fix)
}

// validateEscalations checks a workflow's `escalations` block at its own
// declaration site. Called from validateWorkflow beside validateAppliesTo. A
// workflow declaring none is untouched — nil escalations is the back-compat
// reading every pre-#2227 document gets.
//
// ORDER IS A CONTRACT so the reported error is deterministic for an escalation
// that is wrong in several ways at once. Per escalation, in index order:
//
//  1. `change_kind` inside `match`            — wrong regardless of the rest,
//     and its message names a fix
//     the generic predicate error
//     cannot;
//  2. the SHARED Predicate.Validate           — empty predicate, malformed
//     glob, empty label, unknown
//     trigger form;
//  3. require.approvals with no approval gate — the block is inert workflow-
//     wide, a stronger diagnosis
//     than any per-dimension one;
//  4. approvals.count raise-check;
//  5. approvals.min_permission raise-check;
//  6. approvals.member_of no-op check;
//  7. max_autonomy no-op check.
//
// Steps 4-6's BASELINE is the workflow's LEAST-RESTRICTIVE declaration per
// dimension — the max count and the strictest min_permission over EVERY
// approval gate. A workflow-level escalation applies to every approval gate,
// so one that raised gate A while leaving gate B untouched would not hold the
// only-ever-raise invariant workflow-wide. That is stricter than a per-gate
// reading, so the rejection message names the baseline explicitly rather than
// leaving the author to guess which gate they failed.
//
// Step 7 is backend-only: it needs the v2 tier expansion and the resolved
// matrix, which cli/internal/spec deliberately does not carry (mirroring the
// resolver there would be precisely the duplicate-implementation drift this
// epic exists to prevent). Every other check is mirrored onto the CLI's
// raw-tree validator with byte-identical messages.
func validateEscalations(name string, wf *Workflow) error {
	if wf == nil || len(wf.Escalations) == 0 {
		return nil
	}
	gates := approvalGatesOf(wf)
	for i := range wf.Escalations {
		e := wf.Escalations[i]
		ptr := fmt.Sprintf("/workflows/%s/escalations/%d", name, i)
		if len(e.Match.ChangeKinds) > 0 {
			return &ValidationError{
				Path:    ptr + "/match/change_kind",
				Message: MsgEscalationChangeKindUnsupported,
			}
		}
		if err := e.Match.Validate(ptr + "/match"); err != nil {
			return &ValidationError{Path: ptr + "/match", Message: err.Error()}
		}
		if err := validateEscalatedApprovals(name, i, ptr, e.Require.Approvals, gates); err != nil {
			return err
		}
		if err := validateEscalatedCeiling(name, i, ptr, e.Require.MaxAutonomy, wf); err != nil {
			return err
		}
	}
	return nil
}

// validateEscalatedApprovals runs steps 3-6 for one escalation.
func validateEscalatedApprovals(name string, idx int, ptr string, a *EscalatedApprovals, gates []*Approvals) error {
	if a == nil {
		return nil
	}
	if len(gates) == 0 {
		return &ValidationError{
			Path:    ptr + "/require/approvals",
			Message: fmt.Sprintf(MsgFmtEscalationApprovalsNoApprovalGate, name, idx, name),
		}
	}
	if a.Count != nil {
		baseline := baselineApprovalCount(gates)
		if *a.Count <= baseline {
			return &ValidationError{
				Path: ptr + "/require/approvals/count",
				Message: escalationNoRaiseMessage(name, idx, "approvals.count",
					fmt.Sprintf("count %d", *a.Count),
					fmt.Sprintf("the workflow's baseline of %d (the highest count any approval gate declares)", baseline),
					fmt.Sprintf("Declare a count above %d, or drop require.approvals.count.", baseline)),
			}
		}
	}
	if a.MinPermission != "" {
		baseline := baselineMinPermission(gates)
		if permissionRank(a.MinPermission) <= permissionRank(baseline) {
			return &ValidationError{
				Path: ptr + "/require/approvals/min_permission",
				Message: escalationNoRaiseMessage(name, idx, "approvals.min_permission",
					fmt.Sprintf("min_permission %q", a.MinPermission),
					fmt.Sprintf("the workflow's baseline of %q (the strictest tier any approval gate declares)", baseline),
					fmt.Sprintf("Declare a tier stricter than %q, or drop require.approvals.min_permission.", baseline)),
			}
		}
	}
	if a.MemberOf != "" && everyGateRequiresGroup(gates, a.MemberOf) {
		return &ValidationError{
			Path: ptr + "/require/approvals/member_of",
			Message: escalationNoRaiseMessage(name, idx, "approvals.member_of",
				fmt.Sprintf("member_of %q", a.MemberOf),
				fmt.Sprintf("the workflow's baseline: every approval gate already requires %q, and membership composes as a de-duplicated conjunction, so the composed set is identical to the baseline", a.MemberOf),
				"Name a group some approval gate does NOT already require, or drop require.approvals.member_of."),
		}
	}
	return nil
}

// validateEscalatedCeiling runs step 7: the max_autonomy NO-OP check.
//
// The ceiling is a no-op only when clamping changes NOTHING ANYWHERE it could
// apply. That is the workflow-level resolved matrix AND every gate that
// declares its own autonomy block — a gate block is a WHOLESALE override
// (ResolveAutonomy's ladder), so a ceiling that is inert at workflow level can
// still genuinely restrict a gate declaring `autonomy: high`. Testing the
// workflow-level matrix alone would reject such a ceiling as a no-op when it
// demonstrably raises the requirement at that gate, which is the one outcome
// an only-ever-raise validator must never produce.
func validateEscalatedCeiling(name string, idx int, ptr string, ceiling AutonomyTier, wf *Workflow) error {
	if ceiling == "" {
		return nil
	}
	for _, rm := range resolvedMatricesOf(wf) {
		if !reflect.DeepEqual(rm, ClampResolvedMatrix(rm, ceiling)) {
			return nil
		}
	}
	return &ValidationError{
		Path: ptr + "/require/max_autonomy",
		Message: escalationNoRaiseMessage(name, idx, "max_autonomy",
			fmt.Sprintf("a ceiling of %q", ceiling),
			"the workflow's resolved action matrix: clamping it with this ceiling leaves every action class exactly as resolved, at this workflow and at every gate declaring its own autonomy block",
			fmt.Sprintf("Declare a ceiling stricter than what the workflow already resolves to, or drop require.max_autonomy (a workflow that delegates nothing has nothing for a %q ceiling to restrict).", ceiling)),
	}
}

// resolvedMatricesOf returns every matrix a ceiling could apply to: the
// workflow-level resolution plus each gate that declares its own block. A
// level resolving to nil (declaring no autonomy at all) is skipped — there is
// nothing there for a ceiling to clamp, so it cannot be the matrix that saves
// the ceiling from the no-op check. When NO level declares a block the result
// is empty and the ceiling is correctly refused: the workflow delegates
// nothing, so no ceiling on it raises anything.
func resolvedMatricesOf(wf *Workflow) []*ResolvedMatrix {
	var out []*ResolvedMatrix
	if rm := ResolveAutonomy(wf, nil); rm != nil {
		out = append(out, rm)
	}
	for si := range wf.Stages {
		for gi := range wf.Stages[si].Gates {
			g := &wf.Stages[si].Gates[gi]
			if declaredAutonomy(g.Autonomy, g.Actions) == nil {
				continue
			}
			if rm := ResolveAutonomy(wf, g); rm != nil {
				out = append(out, rm)
			}
		}
	}
	return out
}

// approvalGatesOf collects every approval gate's forge-neutral Approvals block
// in the workflow. A gate with no Approvals is skipped: at major 2 the schema
// requires the block on an approval gate, so this only skips the v0/v1
// legacy-Approvers form, which an escalation (a v2-only surface) never meets.
func approvalGatesOf(wf *Workflow) []*Approvals {
	var out []*Approvals
	for si := range wf.Stages {
		for gi := range wf.Stages[si].Gates {
			g := &wf.Stages[si].Gates[gi]
			if g.Type != GateTypeApproval || g.Approvals == nil {
				continue
			}
			out = append(out, g.Approvals)
		}
	}
	return out
}

// baselineApprovalCount is the LEAST-RESTRICTIVE approval count the workflow
// declares — the MAX over its approval gates. An escalation must exceed it to
// raise the requirement at every gate.
func baselineApprovalCount(gates []*Approvals) int {
	baseline := 0
	for _, a := range gates {
		if a.Count != nil && *a.Count > baseline {
			baseline = *a.Count
		}
	}
	return baseline
}

// baselineMinPermission is the STRICTEST min_permission the workflow declares
// over its approval gates, or "" when none declares one (no constraint, so any
// declared tier raises it).
func baselineMinPermission(gates []*Approvals) string {
	baseline := ""
	for _, a := range gates {
		if permissionRank(a.MinPermission) > permissionRank(baseline) {
			baseline = a.MinPermission
		}
	}
	return baseline
}

// everyGateRequiresGroup reports whether EVERY approval gate already names
// group in its own member_of — the condition under which an escalated
// member_of de-duplicates away and composes to the identical set. A group some
// gate omits genuinely raises that gate and is accepted.
func everyGateRequiresGroup(gates []*Approvals, group string) bool {
	for _, a := range gates {
		if a.MemberOf != group {
			return false
		}
	}
	return len(gates) > 0
}
