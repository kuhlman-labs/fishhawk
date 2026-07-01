package spec

import "fmt"

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
		// allowed_paths, required_outcomes) are meaningless for a
		// delegating deploy. Presence of change_freeze is detected via
		// the *bool pointer, so `{change_freeze: false}` on a non-deploy
		// stage is correctly rejected.
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
