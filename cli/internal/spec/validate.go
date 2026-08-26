package spec

import (
	"fmt"
	"sort"
	"strings"
)

// validateAgentVersions is the CLI's semantic sweep beyond JSON Schema — the
// checks the schema cannot express, run so `fishhawk validate` catches them
// locally instead of deferring them to the backend at dispatch (the same
// first-line-of-defense role the schema validation plays). The name is
// historical: the sweep began (E32.13 / #1743) as the agent_version check
// alone and has since taken the workflow- and stage-level semantic rules the
// backend enforces.
//
// It covers:
//
//   - agent_version compatibility ranges (#1743) — a plain string to the
//     schema, so a malformed range like ">=abc" passes schema validation but
//     is a spec authoring error;
//   - reviewers.authority with no agent reviewers (E53.2 / #2225);
//   - a workflow's applies_to routing predicate (E53.3 / #2226), including
//     the plan-stage rule on its `paths` criterion (E53.15 / #2377);
//   - a workflow's escalations block (E53.4 / #2227), minus the one check
//     that needs the v2 autonomy resolver — see checkEscalations.
//
// It operates on the yaml.v3-decoded map[string]any / []any tree (never
// structs — this package carries no typed decode; the stage-reference
// resolution added in E52.13 / #2323 lives in graphshape.go and works the same
// raw tree), tolerating any shape mismatch by skipping: the schema layer
// already rejected genuinely malformed structure, so a non-map/non-string node
// here is simply not a value to check.
func validateAgentVersions(raw any) error {
	var errs []ValidationErrorEntry
	root, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	workflows, ok := root["workflows"].(map[string]any)
	if !ok {
		return nil
	}
	for wfName, wfRaw := range workflows {
		wf, ok := wfRaw.(map[string]any)
		if !ok {
			continue
		}
		// applies_to is a WORKFLOW member, not a stage member, so it is
		// checked before the stage-shaped guard below rather than inside
		// the stage loop — a stages-less workflow must still have its
		// routing predicate reported.
		checkAppliesTo(wf, wfName, &errs)
		// escalations is likewise a WORKFLOW member and must be reported for
		// a stages-less workflow too.
		checkEscalations(wf, wfName, &errs)
		stages, ok := wf["stages"].([]any)
		if !ok {
			continue
		}
		for i, stRaw := range stages {
			st, ok := stRaw.(map[string]any)
			if !ok {
				continue
			}
			base := fmt.Sprintf("/workflows/%s/stages/%d", wfName, i)
			checkExecutorAgentVersion(st, base, &errs)
			checkReviewerAgentVersions(st, base, &errs)
			checkReviewerAuthority(st, base, &errs)
			checkStagePermissions(st, base, wfName, &errs)
		}
	}
	if len(errs) > 0 {
		return &ValidationError{Errors: errs}
	}
	return nil
}

// checkExecutorAgentVersion validates a stage's executor.agent_version range.
func checkExecutorAgentVersion(stage map[string]any, base string, errs *[]ValidationErrorEntry) {
	executor, ok := stage["executor"].(map[string]any)
	if !ok {
		return
	}
	appendRangeError(executor["agent_version"], base+"/executor/agent_version", errs)
}

// checkReviewerAgentVersions validates each reviewers.agents[].agent_version range.
func checkReviewerAgentVersions(stage map[string]any, base string, errs *[]ValidationErrorEntry) {
	reviewers, ok := stage["reviewers"].(map[string]any)
	if !ok {
		return
	}
	agents, ok := reviewers["agents"].([]any)
	if !ok {
		return
	}
	for j, agentRaw := range agents {
		agent, ok := agentRaw.(map[string]any)
		if !ok {
			continue
		}
		appendRangeError(agent["agent_version"], fmt.Sprintf("%s/reviewers/agents/%d/agent_version", base, j), errs)
	}
}

// checkReviewerAuthority mirrors the backend's step-5 semantic check
// (E53.2 / #2225, backend/internal/spec/validate.go): a declared
// reviewers.authority (EITHER "advisory" or "gating") is incoherent with
// zero agent reviewers — there is no agent verdict to gate on or to
// surface — so it is rejected naming the stage and the fix. Independent
// implementation over the raw yaml.v3 map (this package shares no code
// with the backend spec module); the paired spec_test.go assertions on
// both sides are what keep the two message strings from drifting. The
// schema enforces agents minItems:1, so `agents: []` is already a schema
// error; this layer owns the ABSENT-agents form. `authority` exists only
// in the v2 schema, so a v0/v1 document declaring it never reaches here.
func checkReviewerAuthority(stage map[string]any, base string, errs *[]ValidationErrorEntry) {
	reviewers, ok := stage["reviewers"].(map[string]any)
	if !ok {
		return
	}
	authority, ok := reviewers["authority"].(string)
	if !ok || authority == "" {
		return
	}
	if agents, ok := reviewers["agents"].([]any); ok && len(agents) > 0 {
		return
	}
	stageID, _ := stage["id"].(string)
	*errs = append(*errs, ValidationErrorEntry{
		Path: base + "/reviewers/authority",
		Message: fmt.Sprintf(
			"stage %q: reviewers.authority: %q declares agent-reviewer authority but the stage configures no agent reviewers; declare at least one entry under reviewers.agents, or remove reviewers.authority to fall back to the count-derived ADR-027 default",
			stageID, authority),
	})
}

// MsgFmtPermissionsEgressConflict is the rejection for declaring BOTH `egress`
// and `permissions.network` on one stage (E53.5 / #2228). Byte-identical to
// backend/internal/spec/permissions.go's constant of the same name — the two Go
// modules are deliberately separate (see this package's doc comment), so the
// duplication is by design and the paired assertions in both spec_test.go files
// (TestPermissionsMessageParity reads both source files) keep the strings in
// lockstep. The `write` glob rejection reuses the shared Predicate.Validate
// message from this package's byte-identical predicate.go, so it needs no
// constant; the executor-binding and shell-posture rejections stay backend-only
// (the schema enum catches shell, and the executor binding is a graph-shape
// rule the CLI leaves to the backend, as it does the deploy executor binding).
const MsgFmtPermissionsEgressConflict = "workflow %q stage %q: declares both `egress` and `permissions.network`; they are two spellings of one allowance, so declare exactly one — there is deliberately no precedence rule. permissions are validated, audited and surfaced but are NOT enforced until E51 (#2133); do not rely on this as containment"

// checkStagePermissions mirrors the two E53.5 permissions checks the JSON
// Schema cannot express (E53.5 / #2228, backend/internal/spec/permissions.go),
// so `fishhawk validate` and `fishhawk doctor` reject them locally instead of
// deferring to the backend at dispatch: the `egress` + `permissions.network`
// conflict, and `write` glob validity through the SHARED predicate (this
// package's byte-identical predicate.go). Shape mismatches are skipped, as
// everywhere in this file — the schema already rejected genuinely malformed
// structure. The unknown-shell posture is left to the schema's enum, and the
// executor binding to the backend.
func checkStagePermissions(stage map[string]any, base, wfName string, errs *[]ValidationErrorEntry) {
	perms, ok := stage["permissions"].(map[string]any)
	if !ok {
		return
	}
	stageID, _ := stage["id"].(string)
	if _, hasEgress := stage["egress"]; hasEgress {
		if _, hasNetwork := perms["network"]; hasNetwork {
			*errs = append(*errs, ValidationErrorEntry{
				Path:    base + "/permissions/network",
				Message: fmt.Sprintf(MsgFmtPermissionsEgressConflict, wfName, stageID),
			})
			return
		}
	}
	if writes := rawStringList(perms["write"]); len(writes) > 0 {
		ptr := base + "/permissions"
		p := Predicate{Paths: writes}
		if err := p.Validate(ptr); err != nil {
			*errs = append(*errs, ValidationErrorEntry{
				Path:    ptr + "/write",
				Message: strings.Replace(err.Error(), ptr+"/paths/", ptr+"/write/", 1),
			})
		}
	}
}

// MsgAppliesToChangeKindUnsupported is the rejection for a `change_kind`
// criterion inside a workflow's `applies_to` (E53.3 / #2226). Byte-identical
// to backend/internal/spec/validate.go's constant of the same name: the two
// Go modules are deliberately separate (see this package's doc comment), so
// the duplication is by design and the paired message assertions in both
// spec_test.go files are what keep the strings in lockstep — the same
// arrangement the eight v2removed messages and the reviewers.authority
// message (E53.2 / #2225) already use.
const MsgAppliesToChangeKindUnsupported = "applies_to does not accept the change_kind criterion: nothing produces a change kind today, so the criterion can never be satisfied and this workflow would be selectable by no run; route on paths, labels or trigger instead (the shared predicate keeps change_kind for its other consumers)"

// MsgFmtAppliesToPathsNoPlanStage is the rejection for a `paths` criterion
// inside the `applies_to` of a workflow that declares no plan stage
// (E53.15 / #2377). Argument: the workflow name. Byte-identical to
// backend/internal/spec/validate.go's constant of the same name: the two Go
// modules are deliberately separate (see this package's doc comment), so the
// duplication is by design and TestAppliesToPathsNoPlanStageMessageParity —
// which reads both source files over the repo root — is what keeps the
// strings in lockstep, exactly as MsgAppliesToChangeKindUnsupported is. It
// must therefore stay a single-line `const … = "…"` declaration.
//
// The rule is UNCONDITIONAL: there is no carve-out admitting the declaration
// because some other control (a stage's post-hoc `constraints.allowed_paths`,
// say) holds a similar envelope. Such a declaration is still never evaluated
// at routing time, which is the failure the rule closes.
const MsgFmtAppliesToPathsNoPlanStage = "workflow %q declares applies_to.paths but no plan stage: `paths` is evaluated at the plan gate against the approved plan's scope.files, so a workflow that produces no plan produces no path set to check it against and this criterion could never be evaluated — it is refused rather than silently accepted as a control that does something. Give this workflow a plan stage; or route on applies_to labels / trigger, which are evaluated at run admission on every workflow whatever its shape; or declare the stage's constraints.allowed_paths, a post-hoc envelope checked against the produced diff after the agent has run rather than at routing."

// appliesToPathsNoPlanStageMessage renders MsgFmtAppliesToPathsNoPlanStage,
// mirroring the backend's shared shape helper so the check and its tests
// assert one rendering rather than two hand-built strings.
func appliesToPathsNoPlanStageMessage(workflow string) string {
	return fmt.Sprintf(MsgFmtAppliesToPathsNoPlanStage, workflow)
}

// hasPlanStageFromRaw reports whether the RESOLVED workflow declares a plan
// stage, and whether its `stages` node was READABLE at all (the sweep runs
// after reuse resolution, so a stage inherited through `extends` is present).
// The two returns are separate because the caller must distinguish "no plan
// stage" from "no stages node to look at" — see checkAppliesTo's rung 3.
// Non-map entries are skipped, in the same shape-tolerant style as
// approvalGatesFromRaw.
func hasPlanStageFromRaw(wf map[string]any) (found, readable bool) {
	stages, ok := wf["stages"].([]any)
	if !ok {
		return false, false
	}
	for _, stRaw := range stages {
		st, ok := stRaw.(map[string]any)
		if !ok {
			continue
		}
		if stType, _ := st["type"].(string); stType == "plan" {
			return true, true
		}
	}
	return false, true
}

// checkAppliesTo mirrors the backend's semantic check on a workflow's
// `applies_to` routing predicate (E53.3 / #2226; E53.15 / #2377,
// backend/internal/spec/validate.go), so `fishhawk validate` does not accept
// a spec the backend refuses at dispatch. Three rungs, in a FIXED order that
// matches the backend's:
//
//  1. a `change_kind` criterion is rejected outright, naming the missing
//     producer — it is wrong regardless of what else the predicate declares,
//     and its message names a fix the generic predicate error cannot;
//  2. the SHARED Predicate.Validate runs at /workflows/<name>/applies_to, so
//     the empty-predicate, malformed-glob, empty-label and unknown-trigger
//     rules are the predicate's own rather than a re-implementation. This
//     package carries a byte-identical copy of predicate.go, so rung 2's
//     messages match the backend's by construction; only the two message
//     constants are hand-duplicated;
//  3. a `paths` criterion on a workflow declaring no plan stage is refused
//     (the structural-impossibility rung, last for the reason the backend's
//     doc comment gives). UNCONDITIONALLY: no other control admits it.
//
// ONE DELIBERATE DIVERGENCE from the typed backend, in rung 3: when `stages`
// is not a list this sweep SKIPS the rung rather than reporting no-plan-stage.
// This file's posture is that a node which is not the shape the schema
// requires was already rejected upstream, and stacking a semantic
// no-plan-stage error on a structural one would be a second, misleading
// diagnosis. The backend has no equivalent leg: a typed *Spec with zero
// stages genuinely HAS no plan stage, and the schema's minItems keeps that
// unreachable from a parsed document.
//
// A workflow declaring no `applies_to` is untouched — that is the
// accepts-any-change reading every pre-#2226 document gets. The projection
// below is shape-tolerant like every other check in this file: this sweep
// runs AFTER schema validation, so a criterion that is not the list of
// strings the schema requires was already rejected upstream.
func checkAppliesTo(wf map[string]any, wfName string, errs *[]ValidationErrorEntry) {
	appliesTo, ok := wf["applies_to"].(map[string]any)
	if !ok {
		return
	}
	ptr := fmt.Sprintf("/workflows/%s/applies_to", wfName)
	p := predicateFromRaw(appliesTo)
	if len(p.ChangeKinds) > 0 {
		*errs = append(*errs, ValidationErrorEntry{
			Path:    ptr + "/change_kind",
			Message: MsgAppliesToChangeKindUnsupported,
		})
		return
	}
	if err := p.Validate(ptr); err != nil {
		*errs = append(*errs, ValidationErrorEntry{Path: ptr, Message: err.Error()})
		return
	}
	if len(p.Paths) == 0 {
		return
	}
	if found, readable := hasPlanStageFromRaw(wf); readable && !found {
		*errs = append(*errs, ValidationErrorEntry{
			Path:    ptr + "/paths",
			Message: appliesToPathsNoPlanStageMessage(wfName),
		})
	}
}

// MsgEscalationChangeKindUnsupported is the rejection for a `change_kind`
// criterion inside an escalation's `match` (E53.4 / #2227). Byte-identical to
// backend/internal/spec/escalation.go's constant of the same name, as are the
// two format constants below: the two Go modules are deliberately separate
// (see this package's doc comment), so the duplication is by design and the
// paired message assertions in both spec_test.go files — plus
// TestEscalationMessageParity, which reads both source files — are what keep
// the strings in lockstep, exactly as MsgAppliesToChangeKindUnsupported is.
const MsgEscalationChangeKindUnsupported = "escalations does not accept the change_kind criterion in `match`: nothing produces a change kind today, so the criterion can never be satisfied and this escalation could never fire; match on paths, labels or trigger instead (the shared predicate keeps change_kind for its other consumers)"

// MsgFmtEscalationNoRaise is the ONE message shape every only-ever-raise
// rejection uses. Arguments, in order: workflow name, escalation index,
// dimension, the escalated value as written, the baseline it fails to raise,
// and the fix.
const MsgFmtEscalationNoRaise = "workflow %q escalation %d: require.%s: %s does not raise %s; an escalation may only ever RAISE a requirement, so one that changes nothing is refused rather than silently accepted as a control that does something. %s"

// MsgFmtEscalationApprovalsNoApprovalGate is the rejection for a
// `require.approvals` block on a workflow declaring no approval gate at all.
// Arguments: workflow name, escalation index.
const MsgFmtEscalationApprovalsNoApprovalGate = "workflow %q escalation %d: require.approvals raises the approval gate, but workflow %q declares no approval gate for it to raise; declare an approval gate on a stage of this workflow, or drop require.approvals (an escalation may still raise max_autonomy on a gate-less workflow)"

// MsgFmtEscalationPathsNoPlanStage is the rejection for a `paths` criterion
// inside an escalation's `match` on a workflow declaring no plan stage
// (E53.16 / #2382). Arguments: workflow name, escalation index. Byte-identical
// to backend/internal/spec/escalation.go's constant of the same name (the
// escalation constants live in escalation.go there, not validate.go): the two
// Go modules are deliberately separate (see this package's doc comment), so
// the duplication is by design and TestEscalationPathsNoPlanStageMessageParity
// — which reads both source files over the repo root — is what keeps the
// strings in lockstep. It must therefore stay a single-line `const … = "…"`
// declaration.
//
// The rule is UNCONDITIONAL: there is no carve-out admitting the declaration
// because some other control (a stage's post-hoc `constraints.allowed_paths`,
// say) holds a similar envelope. Such an escalation is still never evaluated
// at gate time, which is the failure the rule closes.
const MsgFmtEscalationPathsNoPlanStage = "workflow %q escalation %d declares match.paths but the workflow declares no plan stage: an escalation's paths are evaluated at the approval gate against the approved plan's scope.files, so a workflow that produces no plan produces no path set to match against and this escalation could never fire — it is refused rather than silently accepted as a control that does something. Give this workflow a plan stage; or match on labels / trigger, which are evaluated from the run's admission context on every workflow whatever its shape (the require dimensions that do not depend on paths — approvals, min_permission and max_autonomy under a non-paths match — still hold)."

// escalationNoRaiseMessage renders MsgFmtEscalationNoRaise — the shared shape
// helper, mirroring the backend's, so the four dimensions cannot drift into
// four differently actionable messages on either side.
func escalationNoRaiseMessage(workflow string, idx int, dimension, escalated, baseline, fix string) string {
	return fmt.Sprintf(MsgFmtEscalationNoRaise, workflow, idx, dimension, escalated, baseline, fix)
}

// escalationPathsNoPlanStageMessage renders MsgFmtEscalationPathsNoPlanStage,
// mirroring the backend's shared shape helper so the check and its tests
// assert one rendering rather than two hand-built strings.
func escalationPathsNoPlanStageMessage(workflow string, idx int) string {
	return fmt.Sprintf(MsgFmtEscalationPathsNoPlanStage, workflow, idx)
}

// checkEscalations mirrors the backend's only-ever-raise family on a
// workflow's `escalations` block (E53.4 / #2227; E53.16 / #2382,
// backend/internal/spec/escalation.go), so `fishhawk validate` and `fishhawk
// doctor` reject a no-op or lowering escalation — or a `match.paths` that a
// plan-less workflow can never fire — locally instead of deferring it to the
// backend at dispatch. The rungs and their ORDER match the backend's exactly:
// the `change_kind` refusal, the shared Predicate.Validate, the
// approvals-with-no-approval-gate rejection, the count, min_permission and
// member_of checks, then LAST the `match.paths`-on-a-plan-less-workflow refusal
// (the structural-impossibility rung, last for the reason the backend's doc
// comment gives). The plan-stage predicate is the reused hasPlanStageFromRaw.
//
// THE ONE CHECK THAT STAYS BACKEND-ONLY is `max_autonomy`'s no-op check: it
// needs the v2 tier expansion and a resolved-matrix comparison, and this
// package deliberately carries no v2autonomy.go — mirroring the resolver here
// would be precisely the duplicate-implementation drift E53 exists to prevent.
// The schema mirror still gives the CLI every STRUCTURAL rule for free
// (including `require: {}` and `require: {approvals: {}}`), so what the CLI
// misses is exactly one semantic no-op refusal, which the backend raises at
// dispatch.
//
// Unlike the backend this sweep COLLECTS every entry's first error rather than
// returning on the first, matching the file's existing accumulate-then-report
// posture. To keep the backend's ONE-ENTRY-PER-ESCALATION contract, the paths
// rung captures the error count before the require-side rungs and reports only
// when no earlier rung reported for THIS escalation. It also runs regardless
// of whether the `require` node was a readable map — `require` is
// schema-required, so an unreadable one means a document already rejected
// structurally, but the rung must not be silently dead on that leg.
//
// ONE DELIBERATE DIVERGENCE from the typed backend, in the paths rung: when
// `stages` is not a list (hasPlanStageFromRaw's second return is false) this
// sweep SKIPS the rung rather than stacking a semantic no-plan-stage diagnosis
// on the structural one already reported upstream. The typed backend has no
// equivalent leg — a typed *Spec with zero stages genuinely HAS no plan stage,
// and the schema's minItems keeps that unreachable from a parsed document.
//
// It runs AFTER schema validation, so a node that is not the shape the schema
// requires was already rejected upstream and is skipped here.
func checkEscalations(wf map[string]any, wfName string, errs *[]ValidationErrorEntry) {
	list, ok := wf["escalations"].([]any)
	if !ok {
		return
	}
	gates := approvalGatesFromRaw(wf)
	planStageFound, stagesReadable := hasPlanStageFromRaw(wf)
	for i, raw := range list {
		esc, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		ptr := fmt.Sprintf("/workflows/%s/escalations/%d", wfName, i)
		matchRaw, ok := esc["match"].(map[string]any)
		if !ok {
			continue
		}
		p := predicateFromRaw(matchRaw)
		if len(p.ChangeKinds) > 0 {
			*errs = append(*errs, ValidationErrorEntry{
				Path:    ptr + "/match/change_kind",
				Message: MsgEscalationChangeKindUnsupported,
			})
			continue
		}
		if err := p.Validate(ptr + "/match"); err != nil {
			*errs = append(*errs, ValidationErrorEntry{Path: ptr + "/match", Message: err.Error()})
			continue
		}
		// Capture the count before the require-side rungs so the paths rung
		// can preserve one-entry-per-escalation: it reports only when no
		// earlier rung reported for THIS escalation.
		before := len(*errs)
		if require, ok := esc["require"].(map[string]any); ok {
			checkEscalatedApprovals(require, wfName, i, ptr, gates, errs)
		}
		if len(*errs) == before && stagesReadable && !planStageFound && len(p.Paths) > 0 {
			*errs = append(*errs, ValidationErrorEntry{
				Path:    ptr + "/match/paths",
				Message: escalationPathsNoPlanStageMessage(wfName, i),
			})
		}
	}
}

// checkEscalatedApprovals mirrors the backend's approvals rungs for one
// escalation, appending the FIRST rejection it finds so the reported
// dimension order matches the backend's.
func checkEscalatedApprovals(require map[string]any, wfName string, idx int, ptr string, gates []map[string]any, errs *[]ValidationErrorEntry) {
	approvals, ok := require["approvals"].(map[string]any)
	if !ok {
		return
	}
	if len(gates) == 0 {
		*errs = append(*errs, ValidationErrorEntry{
			Path:    ptr + "/require/approvals",
			Message: fmt.Sprintf(MsgFmtEscalationApprovalsNoApprovalGate, wfName, idx, wfName),
		})
		return
	}
	if count, ok := rawInt(approvals["count"]); ok {
		baseline := baselineApprovalCountFromRaw(gates)
		if count <= baseline {
			*errs = append(*errs, ValidationErrorEntry{
				Path: ptr + "/require/approvals/count",
				Message: escalationNoRaiseMessage(wfName, idx, "approvals.count",
					fmt.Sprintf("count %d", count),
					fmt.Sprintf("the workflow's baseline of %d (the highest count any approval gate declares)", baseline),
					fmt.Sprintf("Declare a count above %d, or drop require.approvals.count.", baseline)),
			})
			return
		}
	}
	if perm, _ := approvals["min_permission"].(string); perm != "" {
		baseline := baselineMinPermissionFromRaw(gates)
		if permissionRank(perm) <= permissionRank(baseline) {
			*errs = append(*errs, ValidationErrorEntry{
				Path: ptr + "/require/approvals/min_permission",
				Message: escalationNoRaiseMessage(wfName, idx, "approvals.min_permission",
					fmt.Sprintf("min_permission %q", perm),
					fmt.Sprintf("the workflow's baseline of %q (the strictest tier any approval gate declares)", baseline),
					fmt.Sprintf("Declare a tier stricter than %q, or drop require.approvals.min_permission.", baseline)),
			})
			return
		}
	}
	if group, _ := approvals["member_of"].(string); group != "" && everyGateRequiresGroupRaw(gates, group) {
		*errs = append(*errs, ValidationErrorEntry{
			Path: ptr + "/require/approvals/member_of",
			Message: escalationNoRaiseMessage(wfName, idx, "approvals.member_of",
				fmt.Sprintf("member_of %q", group),
				fmt.Sprintf("the workflow's baseline: every approval gate already requires %q, and membership composes as a de-duplicated conjunction, so the composed set is identical to the baseline", group),
				"Name a group some approval gate does NOT already require, or drop require.approvals.member_of."),
		})
	}
}

// approvalGatesFromRaw collects every approval gate's `approvals` node in the
// RESOLVED workflow (the sweep runs after reuse resolution, so a gate
// inherited through `extends` is present). Shape mismatches are skipped, as
// everywhere in this file.
func approvalGatesFromRaw(wf map[string]any) []map[string]any {
	var out []map[string]any
	stages, ok := wf["stages"].([]any)
	if !ok {
		return nil
	}
	for _, stRaw := range stages {
		st, ok := stRaw.(map[string]any)
		if !ok {
			continue
		}
		gates, ok := st["gates"].([]any)
		if !ok {
			continue
		}
		for _, gRaw := range gates {
			g, ok := gRaw.(map[string]any)
			if !ok {
				continue
			}
			if gType, _ := g["type"].(string); gType != "approval" {
				continue
			}
			if approvals, ok := g["approvals"].(map[string]any); ok {
				out = append(out, approvals)
			}
		}
	}
	return out
}

// baselineApprovalCountFromRaw is the workflow's LEAST-RESTRICTIVE approval
// count — the MAX of each approval gate's EFFECTIVE count — mirroring the
// backend's baseline. A gate that declares no explicit `count` still requires
// ONE approval (the runtime default), so its effective count is 1, not 0:
// treating an omitted count as 0 would let an escalation declaring `count: 1`
// pass this raise-check while raising nothing at runtime (#2227 fixup).
func baselineApprovalCountFromRaw(gates []map[string]any) int {
	baseline := 0
	for _, a := range gates {
		c := 1
		if v, ok := rawInt(a["count"]); ok {
			c = v
		}
		if c > baseline {
			baseline = c
		}
	}
	return baseline
}

// baselineMinPermissionFromRaw is the STRICTEST min_permission declared over
// the workflow's approval gates, or "" when none declares one.
func baselineMinPermissionFromRaw(gates []map[string]any) string {
	baseline := ""
	for _, a := range gates {
		if p, _ := a["min_permission"].(string); permissionRank(p) > permissionRank(baseline) {
			baseline = p
		}
	}
	return baseline
}

// everyGateRequiresGroupRaw reports whether EVERY approval gate already names
// group — the condition under which an escalated member_of de-duplicates away
// and raises nothing.
func everyGateRequiresGroupRaw(gates []map[string]any, group string) bool {
	for _, a := range gates {
		if g, _ := a["member_of"].(string); g != group {
			return false
		}
	}
	return len(gates) > 0
}

// permissionOrder is the forge-neutral permission ladder, weakest first,
// mirroring the backend's. Index = strictness ordinal.
var permissionOrder = []string{"read", "triage", "write", "maintain", "admin"}

// permissionRank returns the strictness ordinal of a permission tier, or -1
// for an unrecognized / absent one (so any declared tier raises an absent
// baseline).
func permissionRank(p string) int {
	for i, name := range permissionOrder {
		if name == p {
			return i
		}
	}
	return -1
}

// rawInt projects a yaml.v3-decoded numeric node onto int. yaml.v3 decodes a
// plain integer as `int`, but a document round-tripped through JSON (as the
// backend's parse path does) carries float64, so both are accepted; anything
// else is not a count and reports absent rather than being coerced.
func rawInt(node any) (int, bool) {
	switch v := node.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	}
	return 0, false
}

// predicateFromRaw projects a yaml.v3-decoded predicate node onto the typed
// Predicate so the shared validator can run over it. A criterion whose value
// is not a list, or a list entry that is not a string, is skipped rather than
// coerced — the schema already rejected those shapes, and a coercion here
// would invent a criterion the author never wrote.
func predicateFromRaw(m map[string]any) Predicate {
	var p Predicate
	p.Paths = rawStringList(m["paths"])
	p.Labels = rawStringList(m["labels"])
	p.ChangeKinds = rawStringList(m["change_kind"])
	for _, s := range rawStringList(m["trigger"]) {
		p.Triggers = append(p.Triggers, TriggerForm(s))
	}
	return p
}

// rawStringList projects a decoded YAML node onto []string, returning nil for
// anything that is not a non-empty list of strings — including a list that is
// only PARTLY strings, which is projected as undeclared rather than half
// converted, so a criterion can never be evaluated against a subset of what
// the author wrote.
//
// Only the absent-key case is reachable through ValidateBytes: the schema
// runs first and enforces `minItems: 1` plus `items.type: string` on every
// predicate criterion, so a non-list value and a non-string entry are both
// rejected before this sweep. The guards stay as the file's shape-tolerant
// convention and to keep the projection total.
func rawStringList(node any) []string {
	list, ok := node.([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil
		}
		out = append(out, s)
	}
	return out
}

// appendRangeError validates a single agent_version node when it is a
// non-empty string, appending a ValidationErrorEntry on a malformed range.
// A non-string or empty value is skipped — absence is not an error, and a
// non-string shape was already rejected by the schema layer.
func appendRangeError(node any, path string, errs *[]ValidationErrorEntry) {
	s, ok := node.(string)
	if !ok || s == "" {
		return
	}
	if err := ValidAgentVersionRange(s); err != nil {
		*errs = append(*errs, ValidationErrorEntry{Path: path, Message: err.Error()})
	}
}

// Messages for the eight back-compat surfaces workflow-v2 REMOVES (E52.3 /
// #2215, E52.2 / #2214, E52.10 / #2222), RESHAPES (E52.6 / #2218) or RENAMES
// (E52.5 / #2217).
// Byte-identical to backend/internal/spec/v2removed.go's set: `fishhawk
// validate` is where a spec author most often meets these errors, so the CLI
// must not degrade to the generic schema message, and the two surfaces must
// not drift. The Go modules are deliberately separate (see this package's doc
// comment), so the duplication is by design; the message-content assertions on
// both sides are what keep the strings in lockstep.
const (
	msgV2RemovedReviewerReject          = `page event "reviewer_reject" was removed in workflow-v2: use "advisory_reviewer_reject" (an agent reject under advisory review authority) or "gating_reviewer_reject" (under gating authority)`
	msgV2RemovedReviewersAgent          = `reviewers.agent was removed in workflow-v2: declare agent reviewers with reviewers.agents[] (one {provider, model?} entry per reviewer); the effective agent count is len(agents)`
	msgV2RenamedDrive                   = `the workflow flag "drive" is spelled "auto_advance" in workflow-v2: rename the key; the semantics are unchanged (fishhawkd auto-advances mechanical transitions, judgment points still park), and v0/v1 keep the "drive" spelling`
	msgV2ReshapedConstraints            = `constraints is an OBJECT in workflow-v2, not a list: write the kinds as one object, e.g. constraints: {max_files_changed: 45, forbidden_paths: ["infra/**"]}; keys are unique, so the one-kind-per-entry list form is gone`
	msgV2RenamedBudgetMaxRuntimeMinutes = `budget.max_runtime_minutes was renamed to budget.max_runtime in workflow-v2: write the same value as a Go duration string parsed by time.ParseDuration — max_runtime_minutes: 15 becomes max_runtime: 15m; v0/v1 keep the max_runtime_minutes spelling`
	msgV2RemovedApprovers               = `the gate "approvers" role allow-list was removed in workflow-v2: declare the forge-neutral "approvals" block instead (a role allow-list becomes approvals: {count, members | member_of | min_permission}); the mapping is NOT mechanical, so review the translation rather than applying it blind`
	msgV2RemovedRolesMap                = `the top-level "roles" map was removed in workflow-v2: forge-neutral membership moves onto a gate's "approvals" block (approvals.member_of / approvals.members); there is no top-level role map at major 2`
	msgV2RemovedOperatorAgent           = `the "operator_agent" block was removed in workflow-v2: declare the action matrix (actions: {approve: {mode: auto, when: clean_dual_approval}, …}) or the tier shorthand (autonomy: low | medium | high) instead; may_approve -> actions.approve, may_route_fixup -> actions.fixup, route_fixup_min_severity -> actions.fixup.min_severity, may_waive -> actions.waive, may_retry -> actions.retry, may_merge -> actions.merge, must_page_human -> actions.page_human_on, model_policy -> actions.model_policy, and knob-absence -> mode: gated`
)

// legacyPageEventReviewerReject is the bare page-event token workflow-v2
// removed. v0/v1 still accept it; this package only names it to detect it.
const legacyPageEventReviewerReject = "reviewer_reject"

// validateV2RemovedForms sweeps a yaml.v3-decoded generic document for the
// eight forms workflow-v2 removed, reshaped or renamed and returns the first
// match as a *ValidationError naming the replacement surface, or nil. It
// mirrors the backend's checkV2RemovedForms exactly, including the ordering
// contract: it runs ONLY for a routed major >= 2 and BEFORE schema validation,
// so the actionable message wins over the generic
// `additional properties 'agent' not allowed` / enum / type message.
//
// Matching contract — read this before changing the walk. The sweep
// matches by KEY NAME at any depth: any `must_page_human` array carrying
// "reviewer_reject", any `reviewers` map carrying an `agent` key, any
// `drive` key, any `constraints` value that is a LIST, any `budget`
// map carrying a `max_runtime_minutes` key, any `approvers` map (the
// removed gate role allow-list, E52.2 / #2214), any `roles` map (the
// removed top-level role map), and any `operator_agent` key whatever its
// value shape (the removed delegation block, E52.10 / #2222). It is
// deliberately NOT position-aware and deliberately OVER-TRIGGERS in
// exchange for never missing a legacy form, so in an already-invalid
// document the legacy-form message may PRECEDE the genuine structural
// error. A position-aware sweep would re-encode the schema's structural
// knowledge in Go, which E52 is actively restructuring. Nodes that are
// neither maps nor arrays are skipped, as is a non-array
// `must_page_human`, a non-map `reviewers`, `budget`, `approvers` or
// `roles`, or a `constraints` that is neither a list nor an object —
// those are not legacy forms, and schema validation (which runs next)
// reports them.
func validateV2RemovedForms(raw any) error {
	if e := walkV2RemovedForms(raw, ""); e != nil {
		return &ValidationError{Errors: []ValidationErrorEntry{*e}}
	}
	return nil
}

// walkV2RemovedForms is validateV2RemovedForms' recursion, carrying the
// JSON pointer of the node under inspection. Map keys are visited in
// sorted order so the reported first match is deterministic across runs.
func walkV2RemovedForms(node any, ptr string) *ValidationErrorEntry {
	switch v := node.(type) {
	case map[string]any:
		if e := checkV2RemovedAtNode(v, ptr); e != nil {
			return e
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if e := walkV2RemovedForms(v[k], ptr+"/"+k); e != nil {
				return e
			}
		}
	case []any:
		for i, item := range v {
			if e := walkV2RemovedForms(item, fmt.Sprintf("%s/%d", ptr, i)); e != nil {
				return e
			}
		}
	}
	return nil
}

// checkV2RemovedAtNode reports a legacy form declared directly on this map
// node. The eight forms are checked in a FIXED order — page event,
// reviewers.agent, the two E52.6 reshapes (drive, list-form constraints),
// the E52.5 budget.max_runtime_minutes rename, then the two E52.2 removals
// (gate `approvers`, top-level `roles`), then the E52.10 `operator_agent`
// removal — so a document carrying several always reports the same one.
// Each new branch is appended LAST so the preceding report order stays
// byte-preserved.
func checkV2RemovedAtNode(m map[string]any, ptr string) *ValidationErrorEntry {
	if events, ok := m["must_page_human"].([]any); ok {
		for i, ev := range events {
			if s, ok := ev.(string); ok && s == legacyPageEventReviewerReject {
				return &ValidationErrorEntry{
					Path:    fmt.Sprintf("%s/must_page_human/%d", ptr, i),
					Message: msgV2RemovedReviewerReject,
				}
			}
		}
	}
	if reviewers, ok := m["reviewers"].(map[string]any); ok {
		if _, ok := reviewers["agent"]; ok {
			return &ValidationErrorEntry{
				Path:    ptr + "/reviewers/agent",
				Message: msgV2RemovedReviewersAgent,
			}
		}
	}
	if _, ok := m["drive"]; ok {
		return &ValidationErrorEntry{
			Path:    ptr + "/drive",
			Message: msgV2RenamedDrive,
		}
	}
	if _, ok := m["constraints"].([]any); ok {
		return &ValidationErrorEntry{
			Path:    ptr + "/constraints",
			Message: msgV2ReshapedConstraints,
		}
	}
	if budget, ok := m["budget"].(map[string]any); ok {
		if _, ok := budget["max_runtime_minutes"]; ok {
			return &ValidationErrorEntry{
				Path:    ptr + "/budget/max_runtime_minutes",
				Message: msgV2RenamedBudgetMaxRuntimeMinutes,
			}
		}
	}
	// E52.2 / #2214: the gate `approvers` role allow-list and the top-level
	// `roles` map were removed — `approvals` is the sole approval predicate at
	// major 2. Appended LAST (approvers before roles) so the five-form order
	// above is byte-preserved. A non-map `approvers`/`roles` is not a legacy
	// form and is left to schema validation.
	if _, ok := m["approvers"].(map[string]any); ok {
		return &ValidationErrorEntry{
			Path:    ptr + "/approvers",
			Message: msgV2RemovedApprovers,
		}
	}
	if _, ok := m["roles"].(map[string]any); ok {
		return &ValidationErrorEntry{
			Path:    ptr + "/roles",
			Message: msgV2RemovedRolesMap,
		}
	}
	// ADR-066 / E52.10 / #2222: the `operator_agent` delegation block was
	// removed — the `actions` matrix and the `autonomy` tier shorthand
	// replace it. Appended LAST so the seven-form order above is
	// byte-preserved. Matched on KEY PRESENCE regardless of value shape
	// (like `drive`, unlike the map-typed `approvers`/`roles` checks): the
	// key itself is gone at major 2, whatever it holds.
	if _, ok := m["operator_agent"]; ok {
		return &ValidationErrorEntry{
			Path:    ptr + "/operator_agent",
			Message: msgV2RemovedOperatorAgent,
		}
	}
	return nil
}
