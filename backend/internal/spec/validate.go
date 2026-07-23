package spec

import (
	"fmt"
	"path"
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
//     the top level.
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
//
// Validate is exported so tests and Spec-builder code can exercise
// the semantic layer without the YAML→schema round trip.
func Validate(s *Spec) error {
	if s == nil {
		return &ValidationError{Path: "/", Message: "nil spec"}
	}
	for wfName, wf := range s.Workflows {
		if err := validateWorkflow(s, wfName, &wf); err != nil {
			return err
		}
	}
	return nil
}

func validateWorkflow(s *Spec, name string, wf *Workflow) error {
	stagePath := func(i int, suffix string) string {
		return fmt.Sprintf("/workflows/%s/stages/%d%s", name, i, suffix)
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
		for j, c := range stage.Constraints {
			if c.isPreflight() && !isDeploy {
				return &ValidationError{
					Path:    stagePath(i, fmt.Sprintf("/constraints/%d", j)),
					Message: fmt.Sprintf("pre-flight deploy constraint is valid only on a deploy stage, not a %q stage (ADR-038)", stage.Type),
				}
			}
			if c.isPostHoc() && isDeploy {
				return &ValidationError{
					Path:    stagePath(i, fmt.Sprintf("/constraints/%d", j)),
					Message: "post-hoc diff constraint is not valid on a deploy stage; a delegating deploy produces no reviewable diff (ADR-038)",
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
				if err := validRepoRelativePath(c.DiffCoverage.ReportPath); err != nil {
					return &ValidationError{
						Path:    stagePath(i, fmt.Sprintf("/constraints/%d/diff_coverage/report_path", j)),
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

		// The egress allowance (ADR-050 / #1532, v1.3) declares the target
		// host(s) an acceptance agent may reach through the runner's
		// default-deny proxy; only an acceptance stage runs under that proxy,
		// so declaring it elsewhere is a binding error. Mirror of the
		// artifact bindings above — the stage $def is shared across every
		// stage type, so this pairing is enforced here, not in the schema.
		if stage.Egress != nil && stage.Type != StageTypeAcceptance {
			return &ValidationError{
				Path: stagePath(i, "/egress"),
				Message: fmt.Sprintf(
					"egress allowance is valid only on an acceptance stage, not a %q stage (ADR-050)",
					stage.Type,
				),
			}
		}

		// Approver role refs must resolve.
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
