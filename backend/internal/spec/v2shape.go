package spec

import (
	"fmt"
)

// The `needs`-referent no-default-artifact rejection (E52.13 / #2323), extracted
// to constants so the CLI's independent port in cli/internal/spec/graphshape.go
// can carry byte-identical copies. The two Go modules cannot share a package, so
// parity of BOTH the message text AND the reported JSON-pointer path is held
// mechanically by TestGraphShapeMessageParity, which reads validate.go, this
// file and graphshape.go over the repo root. They must therefore stay single-line
// `const … = "…"` declarations.
//
// MsgFmtNeedsNoDefaultArtifact's arguments are the referent id, its stage type
// and (again) the referent id, in the longhand-escape suggestion. PathFmtNeeds
// formats the reported path from the workflow name, the stage index and the
// `needs` entry index — the NEEDS index, deliberately distinct from the
// post-expansion inputs index the from_stage rules report at. Each is a
// single-line `const … = "…"` declaration because the parity test matches it
// verbatim.

// MsgFmtNeedsNoDefaultArtifact rejects a `needs` referent whose type has no default input artifact.
const MsgFmtNeedsNoDefaultArtifact = "needs %q references a %q stage, which has no default input artifact; declare the wiring longhand with inputs: [{artifact: …, from_stage: %s}]"

// PathFmtNeeds is the reported path for a no-default-artifact rejection (wf, stage, needs index).
const PathFmtNeeds = "/workflows/%s/stages/%d/needs/%d"

// workflow-v2 shape normalization (E52.6 / #2218).
//
// v2 reshapes three surfaces for legibility without changing what any of
// them MEANS:
//
//	constraints:  a LIST of single-kind objects -> ONE object keyed by kind
//	drive:        -> auto_advance
//	inputs:       needs: [<stage_id>] shorthand for the artifact-wiring case
//
// All three are rewritten HERE, on the raw decoded document, into the
// representation v0/v1 already use — so the typed Go structs, every
// constraint consumer, the runs.drive column, the per-run API override and
// every auto-advance / next_action read site are untouched. The reshape's
// blast radius outside this file is zero by construction.
//
// THE LOAD-BEARING DECISION is the constraints rewrite. A canonical merge
// rule collapsing the v0/v1 LIST into one object was rejected: the five
// constraint consumers fold a duplicate-kind document differently (concat,
// min-wins, max-wins, last-wins, first-wins) and the two deploy sites
// disagree with EACH OTHER today, so no single resolution preserves them.
// Rewriting in the other direction is exact rather than approximate — an
// object has unique keys, so it denotes exactly ONE Constraint, and a
// one-element slice is the single-entry case all five folds agree on. No
// v0/v1 document is ever re-resolved because this pass runs only for a
// routed major >= 2.
//
// ORDERING (see ParseBytes, and backend/internal/spec/README.md):
//
//	checkV2RemovedForms   -> before schema validation (actionable messages)
//	schema.Validate       -> the structural gate
//	normalizeV2Shapes     -> HERE: after validation, before the typed decode
//
// Running after validation means this pass never sees an unvalidated shape:
// a `constraints` value that is a string is the SCHEMA's error, not a
// normalizer panic. It runs before the typed decode because that decode
// uses DisallowUnknownFields, so `auto_advance` and `needs` must be gone by
// then. The walk is POSITIONAL (workflows -> stages -> stage) rather than
// key-name-based like the v2removed sweep: precision is safe here precisely
// because the schema has already validated the structure.

// normalizeV2Shapes rewrites a schema-validated workflow-v2 document into
// the v0/v1 representation the typed decode and every consumer expect.
// Called from ParseBytes for a routed major >= 2 only.
//
// It returns a *ValidationError for the one authoring error the rewrite can
// detect: a `needs` referent whose stage type has no default input artifact.
// Every other `needs` error — an unknown referent, a self or later reference
// — is deliberately left to the EXISTING from_stage graph-shape rules in
// validate.go, which report it with their unchanged messages rather than a
// second competing error.
//
// Shape mismatches are skipped rather than reported: the schema has already
// rejected genuinely malformed structure, so a non-map / non-slice node here
// is simply not something to normalize.
func normalizeV2Shapes(raw any) error {
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
		normalizeAutoAdvance(wf)
		stages, ok := wf["stages"].([]any)
		if !ok {
			continue
		}
		types := stageTypesByID(stages)
		for i, stRaw := range stages {
			st, ok := stRaw.(map[string]any)
			if !ok {
				continue
			}
			normalizeConstraintsObject(st)
			if err := expandNeeds(st, types, wfName, i); err != nil {
				return err
			}
		}
	}
	return nil
}

// normalizeAutoAdvance moves a workflow-level `auto_advance` to the `drive`
// key v0/v1 use and deletes it, so Workflow.Drive, the runs.drive column and
// every auto-advance read site see the identical field. A pure spec-surface
// rename: the value is carried across untouched.
func normalizeAutoAdvance(wf map[string]any) {
	v, ok := wf["auto_advance"]
	if !ok {
		return
	}
	wf["drive"] = v
	delete(wf, "auto_advance")
}

// normalizeConstraintsObject replaces a v2 `constraints` OBJECT with a
// ONE-ELEMENT list carrying it, the legacy representation every consumer
// already reads. A value that is already a list is left alone: the v2 schema
// rejects the list form earlier (and the v2removed sweep reports it with an
// actionable message before that), so this branch is unreachable for a v2
// document — a total function is simply cheaper to reason about than one
// with a reachability precondition.
func normalizeConstraintsObject(stage map[string]any) {
	obj, ok := stage["constraints"].(map[string]any)
	if !ok {
		return
	}
	stage["constraints"] = []any{obj}
}

// stageTypesByID maps each stage id to its declared type, for deriving a
// `needs` referent's default artifact. A stage whose id or type is missing
// or non-string is skipped, which makes its referent read as NOT FOUND —
// routing to validate.go's existing from_stage message rather than to a
// second error from here.
func stageTypesByID(stages []any) map[string]string {
	out := make(map[string]string, len(stages))
	for _, stRaw := range stages {
		st, ok := stRaw.(map[string]any)
		if !ok {
			continue
		}
		id, ok := st["id"].(string)
		if !ok {
			continue
		}
		typ, ok := st["type"].(string)
		if !ok {
			continue
		}
		out[id] = typ
	}
	return out
}

// defaultInputArtifact returns the artifact a `needs` referent of the given
// stage type contributes, and whether that type has one at all. The mapping
// is read straight off $defs/input's artifact enum, whose only members are
// `plan` and `pull_request`: a review stage produces no artifact, and the
// deployment / acceptance artifacts are not declarable as inputs.
func defaultInputArtifact(stageType string) (string, bool) {
	switch StageType(stageType) {
	case StageTypePlan:
		return string(ArtifactPlan), true
	case StageTypeImplement:
		return string(ArtifactPullRequest), true
	default:
		return "", false
	}
}

// expandNeeds rewrites a stage's `needs` shorthand into `inputs` entries and
// deletes the key, so the typed decode (which runs under
// DisallowUnknownFields) never sees it and validate.go's from_stage rules
// see the resolved longhand form.
//
// Ordering and dedupe: the declared inputs keep their positions and the
// derived entries follow in `needs` order, and a derived entry whose
// (artifact, from_stage) pair already appears verbatim among the declared
// inputs is DROPPED — so combining `needs` with longhand `inputs` resolves
// to the identical input set however the author spelled it.
//
// A referent that does NOT exist is emitted with an EMPTY artifact and its
// from_stage set: validate.go then reports it with the unchanged
// "from_stage %q does not match any stage id" message. A self or later
// reference likewise falls through to the existing must-be-earlier rule.
func expandNeeds(stage map[string]any, types map[string]string, wfName string, stageIdx int) error {
	needsRaw, ok := stage["needs"]
	if !ok {
		return nil
	}
	delete(stage, "needs")
	needs, ok := needsRaw.([]any)
	if !ok {
		return nil
	}

	declared, _ := stage["inputs"].([]any)
	derived := make([]any, 0, len(needs))
	for j, entryRaw := range needs {
		id, ok := entryRaw.(string)
		if !ok {
			continue
		}
		artifact := ""
		if typ, known := types[id]; known {
			a, hasDefault := defaultInputArtifact(typ)
			if !hasDefault {
				return &ValidationError{
					Path:    fmt.Sprintf(PathFmtNeeds, wfName, stageIdx, j),
					Message: fmt.Sprintf(MsgFmtNeedsNoDefaultArtifact, id, typ, id),
				}
			}
			artifact = a
		}
		if inputsContain(declared, artifact, id) {
			continue
		}
		derived = append(derived, map[string]any{
			"artifact":   artifact,
			"from_stage": id,
		})
	}
	if len(derived) == 0 {
		return nil
	}
	stage["inputs"] = append(append([]any{}, declared...), derived...)
	return nil
}

// inputsContain reports whether the declared inputs already carry an entry
// with exactly this (artifact, from_stage) pair.
func inputsContain(declared []any, artifact, fromStage string) bool {
	for _, inRaw := range declared {
		in, ok := inRaw.(map[string]any)
		if !ok {
			continue
		}
		a, _ := in["artifact"].(string)
		f, _ := in["from_stage"].(string)
		if a == artifact && f == fromStage {
			return true
		}
	}
	return false
}
