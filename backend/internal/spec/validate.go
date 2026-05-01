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
