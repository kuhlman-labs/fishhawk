package spec

import (
	"fmt"
	"path"
	"strconv"
	"strings"
)

// Validate runs the semantic checks that the JSON Schema can't
// express. Schema-level validation (structure, enums, types) has
// already happened in Parse; this layer enforces graph-shape rules:
//
//   - Stage IDs are unique within a workflow.
//   - inputs[].from_stage references an existing stage in the same
//     workflow.
//   - approvers.any_of / approvers.all_of reference roles defined at
//     the top level. This rule is version-agnostic in code but
//     UNREACHABLE for a routed major >= 2 (E52.2 / #2214): workflow-v2
//     removed both the gate `approvers` allow-list and the top-level
//     `roles` map, so the v2removed sweep rejects either raw key before
//     a v2 document ever reaches Validate, leaving Gate.Approvers nil.
//     validateApproverRefs is RETAINED because Validate and the Spec
//     struct are shared across every major and v0/v1 documents still
//     carry both surfaces (deleting it would break the existing
//     TestParse_UndefinedApproverRole and the v0/v1 legacy-form pins).
//   - Plan-producing stages declare schema: standard_v1.
//   - type<->executor<->constraint binding (ADR-038 / #925): a deploy
//     stage must use a delegating executor and may carry only pre-flight
//     constraints; a non-deploy stage must not use either; and the
//     deployment artifact is deploy-only. These cross-member rules can't
//     live in the JSON Schema because the executor/constraint $defs are
//     shared across every stage type. The rules are version-agnostic —
//     they never fire on a v0 spec because the v0 schema rejects the
//     deploy members before Validate runs. The `acceptance` stage type
//     (ADR-049 / #1519, E31.2) is a non-deploy agent/human stage, so it
//     is covered by the SAME non-deploy branches with no acceptance-
//     specific code: a delegating executor, a pre-flight deploy
//     constraint, or the deployment artifact on an acceptance stage each
//     falls into the isDeploy==false else-branch below and is rejected
//     exactly as on any other non-deploy stage.
//   - workflow `applies_to` well-formedness (E53.3 / #2226; E53.15 / #2377):
//     the routing predicate is checked at its declaration site — `change_kind`
//     is rejected outright (no producer emits one), then the shared
//     Predicate.Validate runs at /workflows/<name>/applies_to, then a `paths`
//     criterion is refused on a workflow declaring no plan stage (the plan
//     gate is the only evaluator of `paths`, so such a declaration could never
//     be evaluated — the control-that-silently-does-nothing failure E53 exists
//     to prevent). All three rules are version-agnostic in code but
//     unreachable below major 2, because no v0/v1 schema declares the
//     property.
//   - post-hoc-constraint<->produced-artifact binding at major >= 2 (E52.7 /
//     #2219): a v2 stage carrying a post-hoc diff constraint must declare the
//     pull_request artifact. The stage TYPES are general (plan / implement /
//     review read as propose / apply / gate, ADR-067 §2), so validity binds to
//     what a stage produces, not to its name. v0/v1 keep the type-keyed rule.
//   - egress / permissions declaration binding (ADR-050 / #1532; generalized
//     E53.5 / #2228): BELOW major 2 an `egress` allowance is acceptance-stage-
//     only (frozen). AT major >= 2 an `egress` allowance (equivalently the
//     normalized `permissions.network` spelling) or a `permissions` block is
//     valid on any agent-executor stage and rejected on a human or delegate
//     executor; the `permissions` block's `write` globs and `shell` posture are
//     then validated. The block is DECLARATION-ONLY — validated, audited and
//     surfaced but not enforced until E51 #2133.
//
// Validate is exported so tests and Spec-builder code can exercise
// the semantic layer without the YAML→schema round trip.
func Validate(s *Spec) error {
	if s == nil {
		return &ValidationError{Path: "/", Message: "nil spec"}
	}
	major := specVersionMajor(s.Version)
	for wfName, wf := range s.Workflows {
		if err := validateWorkflow(s, wfName, &wf, major); err != nil {
			return err
		}
	}
	return nil
}

// specVersionMajor parses a spec version string's major component, the way
// ParseBytes' schemaForVersion routes. It is used ONLY to select the FORM of
// a reported constraint path (see constraintPath), never to gate a rule. A
// missing / unparseable version returns 0 — which is also what a
// programmatically built *Spec with an empty Version yields, so the legacy
// index form is preserved for the Validate-only callers that rely on it.
func specVersionMajor(version string) int {
	majorPart := version
	if idx := strings.IndexByte(majorPart, '.'); idx >= 0 {
		majorPart = majorPart[:idx]
	}
	major, err := strconv.Atoi(majorPart)
	if err != nil {
		return 0
	}
	return major
}

// constraintPath renders the path segment naming a constraint in a
// validation error. Below major 2 that is the LIST INDEX the author wrote
// (`/constraints/0`). At major 2 and above a v2 document spells constraints
// as a single OBJECT that normalizes to a one-element slice, so an index
// names nothing the author wrote — the KIND does (`/constraints/change_freeze`).
// An empty kind (defensive: a Constraint with no field set) falls back to the
// index form rather than emitting a dangling `/constraints/`.
func constraintPath(major, idx int, kind string) string {
	if major >= 2 && kind != "" {
		return "/constraints/" + kind
	}
	return fmt.Sprintf("/constraints/%d", idx)
}

func validateWorkflow(s *Spec, name string, wf *Workflow, major int) error {
	stagePath := func(i int, suffix string) string {
		return fmt.Sprintf("/workflows/%s/stages/%d%s", name, i, suffix)
	}

	if err := validateAppliesTo(name, wf); err != nil {
		return err
	}

	// workflow `escalations` well-formedness (E53.4 / #2227): the
	// only-ever-raise family, checked at the declaration site. Runs beside
	// applies_to and AFTER it, so a workflow wrong in both places reports
	// its routing declaration first (routing decides whether the workflow
	// is usable at all; an escalation only decides how strict it gets).
	// Version-agnostic in code but unreachable below major 2 — no v0/v1
	// schema declares the property.
	if err := validateEscalations(name, wf); err != nil {
		return err
	}

	seen := make(map[string]int, len(wf.Stages))
	for i, stage := range wf.Stages {
		if prev, ok := seen[stage.ID]; ok {
			return &ValidationError{
				Path: stagePath(i, "/id"),
				Message: fmt.Sprintf(
					"duplicate stage id %q (also at /workflows/%s/stages/%d/id)",
					stage.ID, name, prev,
				),
			}
		}
		seen[stage.ID] = i
	}

	for i, stage := range wf.Stages {
		isDeploy := stage.Type == StageTypeDeploy

		// type<->executor binding (ADR-038 / #925). The JSON Schema's
		// executor oneOf permits agent/human/delegate on ANY stage type
		// (the $def is shared across all types), so the deploy-vs-other
		// pairing is a graph-shape rule enforced here.
		if isDeploy {
			// A deploy stage MUST delegate (Fishhawk holds no deploy
			// logic/credentials) and MUST NOT run an agent or human.
			if stage.Executor.Delegate == nil {
				return &ValidationError{
					Path:    stagePath(i, "/executor"),
					Message: "deploy stage must use a delegating executor (executor.delegate); Fishhawk holds no deploy logic or credentials (ADR-038)",
				}
			}
			if stage.Executor.Agent != "" || stage.Executor.Human {
				return &ValidationError{
					Path:    stagePath(i, "/executor"),
					Message: "deploy stage must not use an agent or human executor; it delegates to an external pipeline via executor.delegate (ADR-038)",
				}
			}
		} else if stage.Executor.Delegate != nil {
			// A delegating executor is meaningless off a deploy stage. This
			// else-branch is type-generic: it fires for an acceptance stage
			// (ADR-049) exactly as for plan/implement/review — acceptance is an
			// agent/human stage, never delegating.
			return &ValidationError{
				Path:    stagePath(i, "/executor/delegate"),
				Message: fmt.Sprintf("delegating executor (executor.delegate) is valid only on a deploy stage, not a %q stage (ADR-038)", stage.Type),
			}
		}

		// type<->constraint binding (ADR-038 / #925). Pre-flight deploy
		// constraints (allowed_environments, change_freeze,
		// required_upstream) are valid ONLY on a deploy stage; the
		// post-hoc diff constraints (max_files_changed, forbidden_paths,
		// allowed_paths, required_outcomes, diff_coverage) are meaningless
		// for a delegating deploy. Presence of change_freeze is detected via
		// the *bool pointer, so `{change_freeze: false}` on a non-deploy
		// stage is correctly rejected; diff_coverage presence (#1888) is
		// detected the same way, so it is rejected on a deploy stage
		// identically to its four post-hoc siblings.
		//
		// The loop stays a slice walk: it is correct for BOTH shapes — a
		// v0/v1 list of single-kind entries and the one-element slice a v2
		// object normalizes to. Only the reported PATH is version-aware
		// (constraintPath); the three binding MESSAGES below are verbatim
		// unchanged, which #2219 depends on.
		//
		// A FOURTH rule joins them at major >= 2 (E52.7 / #2219): a post-hoc
		// diff constraint is valid only on a stage that actually PRODUCES a
		// diff, i.e. declares the pull_request artifact. The stage types are
		// general — plan / implement / review are conceptually propose / apply
		// / gate (ADR-067 §2, names deliberately retained) — so a grooming-
		// shaped workflow runs on those same types and produces no diff at
		// all; keying diff-constraint validity on the type name would have
		// silently accepted a constraint that can never be evaluated.
		//
		// ORDER within the loop is a CONTRACT, not an accident (both
		// directions pinned by tests):
		//
		//  1. pre-flight-off-deploy and post-hoc-on-deploy (ADR-038) run
		//     FIRST, so a deploy stage keeps its existing message and never
		//     sees the new one, and a mixed v2 constraints OBJECT declaring
		//     both families keeps reporting the pre-flight diagnosis — that
		//     constraint is wrong regardless of what the stage produces
		//     (TestValidate_MixedPreflightPostHocConstraintObject_BindingOrderPinned).
		//  2. The produced-artifact rule runs NEXT, so a non-diff-producing
		//     stage gets the general "declare produces / drop the constraint"
		//     message rather than the narrower diff_coverage one.
		//  3. The #1888 diff_coverage block runs LAST, staying reachable at v2
		//     for a stage that DOES produce pull_request but is typed other
		//     than implement.
		//
		// VERSION GATE: the produced-artifact rule fires only at major >= 2.
		// v0/v1 documents legitimately declare post-hoc constraints on an
		// implement stage with NO `produces` list at all (the v1.5 document in
		// TestParse_DiffCoverage_NoRegression is the concrete case), so
		// applying it below major 2 would newly reject valid specs. v2 is
		// where the generalization is licensed — and there, an absent or empty
		// `produces` list reads as "produces no diff" (see Stage.producesDiff
		// for why the permissive reading was rejected).
		for j, c := range stage.Constraints {
			if c.isPreflight() && !isDeploy {
				return &ValidationError{
					Path:    stagePath(i, constraintPath(major, j, c.preflightKindName())),
					Message: fmt.Sprintf("pre-flight deploy constraint is valid only on a deploy stage, not a %q stage (ADR-038)", stage.Type),
				}
			}
			if c.isPostHoc() && isDeploy {
				return &ValidationError{
					Path:    stagePath(i, constraintPath(major, j, c.postHocKindName())),
					Message: "post-hoc diff constraint is not valid on a deploy stage; a delegating deploy produces no reviewable diff (ADR-038)",
				}
			}
			if major >= 2 && c.isPostHoc() && !isDeploy && !stage.producesDiff() {
				kind := c.postHocKindName()
				return &ValidationError{
					Path: stagePath(i, constraintPath(major, j, kind)),
					Message: fmt.Sprintf(
						"post-hoc diff constraint %q is valid only on a stage that produces a diff; stage %q declares no pull_request artifact (ADR-067). Declare produces: [{artifact: pull_request}] on this stage, or remove the constraint.",
						kind, stage.ID,
					),
				}
			}
			// diff_coverage.report_path must stay inside the checkout
			// (#1888). The JSON Schema can express minLength but not
			// "repo-relative", and the runner reads the report by joining
			// this path onto a throwaway checkout of the committed tree —
			// so an absolute path or a `..` escape would read a file
			// outside the tree the measurement claims to describe. Reject
			// at parse time rather than as an opaque measurement failure.
			if c.DiffCoverage != nil {
				dcPath := constraintPath(major, j, "diff_coverage")
				if major < 2 {
					dcPath += "/diff_coverage"
				}
				// diff_coverage is an IMPLEMENT-stage constraint (#1888).
				// Only the implement path measures it: the runner's
				// measurement needs the staged, committed scope-only tree
				// the implement stage produces, and no other stage type
				// emits a diff_coverage signal. Because an ABSENT signal on
				// a declared constraint is by design a violation, declaring
				// it on (say) an acceptance stage whose bundle carries a
				// diff would be a GUARANTEED false category-B failure — the
				// exact false-RED this opt-in gate exists to avoid. Reject
				// it at parse time, where the spec author can act on it,
				// rather than at evaluation time on a real run.
				if stage.Type != "implement" {
					return &ValidationError{
						Path:    stagePath(i, dcPath),
						Message: fmt.Sprintf("diff_coverage is valid only on an implement stage, not a %q stage (#1888): the measurement is emitted by the implement runner, and a declared constraint with no measurement is a violation", stage.Type),
					}
				}
				if err := validRepoRelativePath(c.DiffCoverage.ReportPath); err != nil {
					return &ValidationError{
						Path:    stagePath(i, dcPath+"/report_path"),
						Message: err.Error(),
					}
				}
			}
		}

		// agent_version compatibility ranges (E32.13 / #1743) are plain
		// strings to the JSON Schema, so a malformed range like ">=abc"
		// passes schema validation. Validate the syntactic shape here — on
		// the executor's agent branch and each heterogeneous reviewer — so a
		// bad range is a spec authoring error caught at parse time rather
		// than an opaque dispatch-time failure. Empty = absent (no
		// constraint), skipped.
		if stage.Executor.AgentVersion != "" {
			if err := ValidAgentVersionRange(stage.Executor.AgentVersion); err != nil {
				return &ValidationError{
					Path:    stagePath(i, "/executor/agent_version"),
					Message: err.Error(),
				}
			}
		}
		if stage.Reviewers != nil {
			for j, ar := range stage.Reviewers.Agents {
				if ar.AgentVersion == "" {
					continue
				}
				if err := ValidAgentVersionRange(ar.AgentVersion); err != nil {
					return &ValidationError{
						Path:    stagePath(i, fmt.Sprintf("/reviewers/agents/%d/agent_version", j)),
						Message: err.Error(),
					}
				}
			}
			// An explicit reviewers.authority (E53.2 / #2225) declares a
			// policy about AGENT verdicts, so it is incoherent with zero
			// agent reviewers — there is no verdict to gate on or to
			// surface. Reject a declared authority (EITHER value) when the
			// stage configures no agent reviewers, naming the stage and the
			// fix. `gateless` is the zero-agent outcome, never a declarable
			// policy. The schema enforces agents minItems:1, so the literal
			// `agents: []` form is already a schema error; this layer owns
			// the ABSENT-agents form. Version-agnostic in code but
			// unreachable below major 2 — no v0/v1 schema accepts the
			// property — matching this file's convention for such rules.
			if stage.Reviewers.Authority != "" && stage.Reviewers.AgentCount() == 0 {
				return &ValidationError{
					Path: stagePath(i, "/reviewers/authority"),
					Message: fmt.Sprintf(
						"stage %q: reviewers.authority: %q declares agent-reviewer authority but the stage configures no agent reviewers; declare at least one entry under reviewers.agents, or remove reviewers.authority to fall back to the count-derived ADR-027 default",
						stage.ID, stage.Reviewers.Authority),
				}
			}
		}

		// inputs[].from_stage cross-references must resolve.
		for j, in := range stage.Inputs {
			if in.FromStage == "" {
				continue
			}
			if _, ok := seen[in.FromStage]; !ok {
				return &ValidationError{
					Path: stagePath(i, fmt.Sprintf("/inputs/%d/from_stage", j)),
					Message: fmt.Sprintf(
						"from_stage %q does not match any stage id in workflow %q",
						in.FromStage, name,
					),
				}
			}
			// Cannot reference self or a later stage; runs are
			// linear in v0.
			refIdx := seen[in.FromStage]
			if refIdx >= i {
				return &ValidationError{
					Path: stagePath(i, fmt.Sprintf("/inputs/%d/from_stage", j)),
					Message: fmt.Sprintf(
						"from_stage %q must be a stage earlier in the workflow (got index %d, this stage is index %d)",
						in.FromStage, refIdx, i,
					),
				}
			}
		}

		// Plan-producing stages must declare schema: standard_v1.
		// MVP_SPEC §4.3: plans are schema-versioned for forward
		// compatibility; a missing schema directive is a
		// permanent-data-loss risk.
		for j, p := range stage.Produces {
			if p.Artifact == ArtifactPlan && p.Schema != "standard_v1" {
				return &ValidationError{
					Path: stagePath(i, fmt.Sprintf("/produces/%d/schema", j)),
					Message: fmt.Sprintf(
						"plan-producing stage must declare schema: standard_v1, got %q",
						p.Schema,
					),
				}
			}
			// The deployment artifact (ADR-038 / #925) is emitted only by
			// a deploy stage; declaring it elsewhere is a binding error.
			if p.Artifact == ArtifactDeployment && !isDeploy {
				return &ValidationError{
					Path: stagePath(i, fmt.Sprintf("/produces/%d/artifact", j)),
					Message: fmt.Sprintf(
						"deployment artifact is valid only on a deploy stage, not a %q stage (ADR-038)",
						stage.Type,
					),
				}
			}
			// The acceptance artifact (ADR-049 / #1531) is emitted only by
			// an acceptance stage; declaring it elsewhere is a binding error.
			// Mirror of the deployment binding above — the produces $def is
			// shared across every stage type, so this stage-type pairing is a
			// graph-shape rule enforced here rather than in the JSON Schema.
			if p.Artifact == ArtifactAcceptance && stage.Type != StageTypeAcceptance {
				return &ValidationError{
					Path: stagePath(i, fmt.Sprintf("/produces/%d/artifact", j)),
					Message: fmt.Sprintf(
						"acceptance artifact is valid only on an acceptance stage, not a %q stage (ADR-049)",
						stage.Type,
					),
				}
			}
		}

		// Egress / permissions declaration binding (ADR-050 / #1532;
		// generalized E53.5 / #2228). BELOW major 2 the acceptance-only egress
		// rule is FROZEN exactly as shipped: egress is valid only on an
		// acceptance stage — only an acceptance stage runs under the runner's
		// default-deny proxy. AT major >= 2 the binding GENERALIZES — an
		// `egress` allowance (equivalently the normalized `permissions.network`
		// spelling) or a `permissions` block is valid on any stage whose
		// executor is the AGENT branch, and rejected on a human or delegate
		// executor, because a stage that runs no agent has no such declaration
		// to make. permissions.network has already been folded into stage.Egress
		// by normalizeStagePermissions, so both spellings reach the same check.
		if major < 2 {
			if stage.Egress != nil && stage.Type != StageTypeAcceptance {
				return &ValidationError{
					Path: stagePath(i, "/egress"),
					Message: fmt.Sprintf(
						"egress allowance is valid only on an acceptance stage, not a %q stage (ADR-050)",
						stage.Type,
					),
				}
			}
		} else {
			if (stage.Egress != nil || stage.Permissions != nil) && stage.Executor.Agent == "" {
				ptr := "/egress"
				if stage.Permissions != nil {
					ptr = "/permissions"
				}
				return &ValidationError{
					Path:    stagePath(i, ptr),
					Message: fmt.Sprintf(MsgFmtPermissionsExecutorBinding, name, stage.ID, executorBranchName(stage.Executor)),
				}
			}
			if err := validateStagePermissions(stagePath(i, "/permissions"), stage.Permissions); err != nil {
				return err
			}
		}

		// Approver role refs must resolve. Version-agnostic in code but
		// unreachable for major >= 2: a v2 gate can never carry Approvers
		// (the v2removed sweep rejects the raw `approvers` key upstream), so
		// this loop only ever fires for v0/v1 gates. Retained deliberately —
		// see the Validate header (E52.2 / #2214).
		for j, g := range stage.Gates {
			if g.Approvers == nil {
				continue
			}
			gatePath := stagePath(i, fmt.Sprintf("/gates/%d/approvers", j))
			if err := validateApproverRefs(s, gatePath, "any_of", g.Approvers.AnyOf); err != nil {
				return err
			}
			if err := validateApproverRefs(s, gatePath, "all_of", g.Approvers.AllOf); err != nil {
				return err
			}
		}
	}
	return nil
}

// MsgAppliesToChangeKindUnsupported is the rejection for a `change_kind`
// criterion inside a workflow's `applies_to` (E53.3 / #2226). It is
// exported so a test can assert the shipped text rather than a paraphrase,
// and it is duplicated BYTE-FOR-BYTE in cli/internal/spec/validate.go —
// the two Go modules cannot share a package, so parity is held by the
// paired message assertions in both spec_test.go files, exactly as the
// reviewers.authority message (E53.2 / #2225) and the v2removed set are.
//
// The rejection is narrow on purpose. `change_kind` stays in the shared
// $defs/predicate for #2227 and #2211; only THIS consumer refuses it,
// because no producer populates Change.ChangeKind today. A workflow
// declaring it would therefore match nothing and be selectable by no run
// — a state indistinguishable, from the operator's side, from the routing
// control being broken. Rejecting at declaration time turns that silent
// unusability into an authoring error naming the missing producer.
const MsgAppliesToChangeKindUnsupported = "applies_to does not accept the change_kind criterion: nothing produces a change kind today, so the criterion can never be satisfied and this workflow would be selectable by no run; route on paths, labels or trigger instead (the shared predicate keeps change_kind for its other consumers)"

// MsgFmtAppliesToPathsNoPlanStage is the rejection for a `paths` criterion
// inside the `applies_to` of a workflow that declares no plan stage
// (E53.15 / #2377). Argument: the workflow name.
//
// It is exported so a test can assert the shipped text rather than a
// paraphrase, and it is duplicated BYTE-FOR-BYTE in
// cli/internal/spec/validate.go — the two Go modules cannot share a package,
// so parity is held by TestAppliesToPathsNoPlanStageMessageParity, which
// reads both source files, exactly as MsgAppliesToChangeKindUnsupported's is.
// It must therefore stay a single-line `const … = "…"` declaration.
//
// The rule is UNCONDITIONAL. `applies_to.paths` is evaluated at the plan
// gate against the approved plan's `scope.files`; a workflow that ships no
// plan stage never produces that set, so the criterion is never evaluated —
// no rejection, no audit entry, no signal that the declaration did nothing.
// A declaration admitted because some OTHER control holds a similar envelope
// (a stage's post-hoc `constraints.allowed_paths`, say) is still never
// evaluated at routing time, so there is no carve-out for one: the routing
// criterion is refused at authoring time and the operator picks a control
// that actually holds.
const MsgFmtAppliesToPathsNoPlanStage = "workflow %q declares applies_to.paths but no plan stage: `paths` is evaluated at the plan gate against the approved plan's scope.files, so a workflow that produces no plan produces no path set to check it against and this criterion could never be evaluated — it is refused rather than silently accepted as a control that does something. Give this workflow a plan stage; or route on applies_to labels / trigger, which are evaluated at run admission on every workflow whatever its shape; or declare the stage's constraints.allowed_paths, a post-hoc envelope checked against the produced diff after the agent has run rather than at routing."

// appliesToPathsNoPlanStageMessage renders MsgFmtAppliesToPathsNoPlanStage —
// the shared shape helper (the escalationNoRaiseMessage precedent), so the
// check and its tests assert ONE rendering rather than two hand-built strings.
func appliesToPathsNoPlanStageMessage(workflow string) string {
	return fmt.Sprintf(MsgFmtAppliesToPathsNoPlanStage, workflow)
}

// hasPlanStage reports whether the RESOLVED workflow declares a plan stage.
// Resolved is the load-bearing word: parse.go folds workflow-v2 same-document
// reuse before Validate runs, so a plan stage inherited through `extends`
// counts — it is the resolved shape that decides whether a `scope.files`
// producer exists.
func hasPlanStage(wf *Workflow) bool {
	for _, st := range wf.Stages {
		if st.Type == StageTypePlan {
			return true
		}
	}
	return false
}

// validateAppliesTo checks a workflow's routing predicate at its own
// declaration site (E53.3 / #2226, extended E53.15 / #2377). A nil predicate
// — the workflow declared no `applies_to` — is valid and means "accepts any
// change", which is what leaves every pre-#2226 document unaffected. It takes
// the whole *Workflow rather than the predicate alone because rung 3 is a
// CROSS-MEMBER rule: a criterion's validity is conditioned on the workflow's
// stage list, which is exactly what the JSON Schema cannot express.
//
// ORDER IS A CONTRACT (pinned by TestValidate_AppliesTo_ChangeKindWinsOverGlob
// and TestValidate_AppliesTo_MalformedGlobWinsOverNoPlanStage), three rungs:
//
//  1. the change_kind rejection runs FIRST, so a predicate declaring both an
//     unsupported change_kind and some other malformed criterion reports the
//     change_kind diagnosis — that criterion is wrong regardless of what else
//     the predicate says, and its message names a fix the generic predicate
//     error cannot;
//  2. the SHARED Predicate.Validate, called at this workflow's pointer so the
//     empty-predicate, malformed-glob, empty-label and unknown-trigger-form
//     rules are the predicate's own rather than a re-implementation that could
//     drift from #2227's and #2211's reading of the same grammar. A malformed
//     glob is a grammar error the author must fix either way;
//  3. the STRUCTURAL-IMPOSSIBILITY rung last: `paths` on a workflow declaring
//     no plan stage. This is where validateEscalations puts its own structural
//     rung (require.approvals on a workflow with no approval gate runs after
//     Predicate.Validate), and the same reasoning applies — the diagnosis is
//     about the workflow's shape, not the criterion's spelling, so it is only
//     worth reporting once the criterion is well-formed.
//
// Rung 3 reads wf.Stages AFTER v2 reuse resolution (parse.go resolves reuse
// before calling Validate), so a plan stage inherited via `extends` satisfies
// it. Like its two siblings the rule is version-agnostic in code but
// unreachable below major 2: no v0/v1 schema declares `applies_to`.
func validateAppliesTo(name string, wf *Workflow) error {
	if wf == nil || wf.AppliesTo == nil {
		return nil
	}
	p := wf.AppliesTo
	ptr := fmt.Sprintf("/workflows/%s/applies_to", name)
	if len(p.ChangeKinds) > 0 {
		return &ValidationError{
			Path:    ptr + "/change_kind",
			Message: MsgAppliesToChangeKindUnsupported,
		}
	}
	if err := p.Validate(ptr); err != nil {
		return &ValidationError{Path: ptr, Message: err.Error()}
	}
	if len(p.Paths) > 0 && !hasPlanStage(wf) {
		return &ValidationError{
			Path:    ptr + "/paths",
			Message: appliesToPathsNoPlanStageMessage(name),
		}
	}
	return nil
}

// validRepoRelativePath rejects a path that does not name a location
// inside the repository checkout: an absolute path, a Windows-style
// drive/backslash path, or one that escapes upward via `..`. Used for
// diff_coverage.report_path (#1888), which the runner joins onto a
// throwaway checkout of the committed tree.
//
// The empty case is left to the schema's minLength, so this reports only
// on paths that are non-empty but out-of-tree.
func validRepoRelativePath(p string) error {
	if p == "" {
		return nil
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, `\`) ||
		(len(p) > 1 && p[1] == ':') {
		return fmt.Errorf("report_path %q must be repo-relative, not absolute", p)
	}
	// path.Clean collapses "a/../.." to ".." and "./x" to "x", so a
	// cleaned result of ".." or a "../" prefix is exactly the escape set.
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("report_path %q must stay inside the repository (no `..` escape)", p)
	}
	return nil
}

// executorBranchName names the non-agent executor branch a stage selected, for
// the E53.5 egress/permissions executor-binding rejection message. The agent
// branch never reaches this (the caller guards on Executor.Agent == ""); an
// executor-less stage is a schema error, so "non-agent" is defensive only.
func executorBranchName(e Executor) string {
	switch {
	case e.Human:
		return "human"
	case e.Delegate != nil:
		return "delegate"
	default:
		return "non-agent"
	}
}

func validateApproverRefs(s *Spec, gatePath, key string, refs []string) error {
	for k, role := range refs {
		if _, ok := s.Roles[role]; !ok {
			return &ValidationError{
				Path: fmt.Sprintf("%s/%s/%d", gatePath, key, k),
				Message: fmt.Sprintf(
					"approver role %q is not defined in the top-level roles map",
					role,
				),
			}
		}
	}
	return nil
}
