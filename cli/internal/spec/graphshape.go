package spec

import "fmt"

// Stage-reference resolution for `fishhawk validate` (E52.13 / #2323).
//
// This file is the CLI twin of TWO backend passes, ported onto the raw
// yaml.v3 map[string]any / []any tree this package already operates on
// (never structs — the package carries no typed decode):
//
//   - backend/internal/spec/v2shape.go's `needs:` expansion — the
//     needs-shorthand → inputs rewrite plus the one authoring error it can
//     detect, a `needs` referent whose stage type has no default input
//     artifact;
//   - backend/internal/spec/validate.go's duplicate-stage-id and
//     inputs[].from_stage resolution/ordering rules.
//
// The two Go modules share no code (a cross-module import would couple the
// CLI's release cadence to the backend's — see spec.go's package doc), so this
// is an INDEPENDENT implementation. Message- and path-parity with the backend
// is therefore held mechanically by TestGraphShapeMessageParity, which reads
// backend/internal/spec/{validate,v2shape}.go and this file over the repo root
// and requires the four Msg* and three Path* constants declared verbatim in
// each. If that test is weakened or a constant is inlined, the two copies can
// drift silently. The constants are byte-identical to the backend's; their
// authoritative documentation lives beside the backend declarations. Each is a
// single-line `const … = "…"` declaration because the parity test matches it
// verbatim.

// MsgFmtFromStageUnknown rejects an inputs[].from_stage naming no declared stage.
const MsgFmtFromStageUnknown = "from_stage %q does not match any stage id in workflow %q"

// MsgFmtFromStageNotEarlier rejects an inputs[].from_stage naming self or a later stage.
const MsgFmtFromStageNotEarlier = "from_stage %q must be a stage earlier in the workflow (got index %d, this stage is index %d)"

// MsgFmtDuplicateStageID rejects a workflow declaring two stages with one id.
const MsgFmtDuplicateStageID = "duplicate stage id %q (also at /workflows/%s/stages/%d/id)"

// MsgFmtNeedsNoDefaultArtifact rejects a `needs` referent whose type has no default input artifact.
const MsgFmtNeedsNoDefaultArtifact = "needs %q references a %q stage, which has no default input artifact; declare the wiring longhand with inputs: [{artifact: …, from_stage: %s}]"

// PathFmtFromStage is the reported path for a from_stage rejection (wf, stage, input index).
const PathFmtFromStage = "/workflows/%s/stages/%d/inputs/%d/from_stage"

// PathFmtStageID is the reported path for a duplicate-stage-id rejection (wf, stage index).
const PathFmtStageID = "/workflows/%s/stages/%d/id"

// PathFmtNeeds is the reported path for a no-default-artifact rejection (wf, stage, needs index).
const PathFmtNeeds = "/workflows/%s/stages/%d/needs/%d"

// LOAD-BEARING ORDER, mirroring the backend (ParseBytes: schema.Validate →
// normalizeV2Shapes → Validate). ValidateBytes runs, for the same document:
//
//	schema.Validate → validateAgentVersions → (major >= 2) expandNeeds → validateGraphShape
//
// expandNeeds folds `needs` into `inputs` so the from_stage rules see the
// resolved longhand, exactly as the backend's normalizer does before its typed
// Validate. It runs only for a routed major >= 2 (no v0/v1 schema declares
// `needs`). The duplicate-id and from_stage rules run for EVERY major, because
// the backend enforces them version-agnostically and the issue names the v0/v1
// from_stage divergence as pre-existing.
//
// Unlike the backend's first-error return, this file follows the package's
// collect-all convention — appending ValidationErrorEntry values — so a
// document defective in several places reports them together. Acceptance and
// rejection are identical to the backend; only ordering can differ, the same
// divergence the applies_to and escalations ports already carry.
//
// Every shape mismatch is tolerated by SKIPPING: the schema layer has already
// rejected genuinely malformed structure, so a non-map / non-slice node here is
// simply not something to normalize or check.
func validateGraphShape(raw any, major int) error {
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
		stages, ok := wf["stages"].([]any)
		if !ok {
			continue
		}
		if major >= 2 {
			types := stageTypesByID(stages)
			for i, stRaw := range stages {
				st, ok := stRaw.(map[string]any)
				if !ok {
					continue
				}
				if e := expandNeeds(st, types, wfName, i); e != nil {
					errs = append(errs, *e)
				}
			}
		}
		checkStageReferences(stages, wfName, &errs)
	}
	if len(errs) > 0 {
		return &ValidationError{Errors: errs}
	}
	return nil
}

// stageTypesByID maps each stage id to its declared type, for deriving a
// `needs` referent's default artifact. A stage whose id or type is missing or
// non-string is skipped, which makes its referent read as NOT FOUND — routing
// to the existing from_stage message rather than to a second error from here.
// Mirror of backend/internal/spec/v2shape.go's stageTypesByID.
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
// stage type contributes, and whether that type has one at all. The mapping is
// read straight off $defs/input's artifact enum, whose only members are `plan`
// and `pull_request`: a review stage produces no artifact, and the deployment /
// acceptance artifacts are not declarable as inputs. Mirror of the backend's,
// using string literals because this package carries no typed StageType /
// Artifact constants.
func defaultInputArtifact(stageType string) (string, bool) {
	switch stageType {
	case "plan":
		return "plan", true
	case "implement":
		return "pull_request", true
	default:
		return "", false
	}
}

// expandNeeds rewrites a stage's `needs` shorthand into `inputs` entries and
// deletes the key, so the from_stage rules see the resolved longhand form.
// Mirror of backend/internal/spec/v2shape.go's expandNeeds, returning the one
// authoring error the rewrite can detect (a referent whose type has no default
// input artifact) as a *ValidationErrorEntry instead of the backend's
// first-error *ValidationError, to fit this file's collect-all posture.
//
// Ordering and dedupe: the declared inputs keep their positions and the derived
// entries follow in `needs` order, and a derived entry whose (artifact,
// from_stage) pair already appears verbatim among the declared inputs is
// DROPPED — so combining `needs` with longhand `inputs` resolves to the
// identical input set however the author spelled it.
//
// A referent that does NOT exist is emitted with an EMPTY artifact and its
// from_stage set: the from_stage rules then report it with the unchanged
// "does not match any stage id" message. A self or later reference likewise
// falls through to the existing must-be-earlier rule.
func expandNeeds(stage map[string]any, types map[string]string, wfName string, stageIdx int) *ValidationErrorEntry {
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
				return &ValidationErrorEntry{
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

// inputsContain reports whether the declared inputs already carry an entry with
// exactly this (artifact, from_stage) pair. Mirror of the backend's.
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

// checkStageReferences ports backend/internal/spec/validate.go's two
// version-agnostic graph-shape rules over one workflow's stages: stage ids are
// unique, and every inputs[].from_stage resolves to a stage declared STRICTLY
// EARLIER in the same workflow. The duplicate-id sweep runs first (over all
// stages) so the `seen` index is complete before any from_stage lookup — the
// same order the backend uses. Non-map / non-string shapes are skipped.
func checkStageReferences(stages []any, wfName string, errs *[]ValidationErrorEntry) {
	seen := make(map[string]int, len(stages))
	for i, stRaw := range stages {
		st, ok := stRaw.(map[string]any)
		if !ok {
			continue
		}
		id, ok := st["id"].(string)
		if !ok {
			continue
		}
		if prev, dup := seen[id]; dup {
			*errs = append(*errs, ValidationErrorEntry{
				Path:    fmt.Sprintf(PathFmtStageID, wfName, i),
				Message: fmt.Sprintf(MsgFmtDuplicateStageID, id, wfName, prev),
			})
			continue
		}
		seen[id] = i
	}
	for i, stRaw := range stages {
		st, ok := stRaw.(map[string]any)
		if !ok {
			continue
		}
		inputs, ok := st["inputs"].([]any)
		if !ok {
			continue
		}
		for j, inRaw := range inputs {
			in, ok := inRaw.(map[string]any)
			if !ok {
				continue
			}
			from, ok := in["from_stage"].(string)
			if !ok || from == "" {
				continue
			}
			refIdx, known := seen[from]
			if !known {
				*errs = append(*errs, ValidationErrorEntry{
					Path:    fmt.Sprintf(PathFmtFromStage, wfName, i, j),
					Message: fmt.Sprintf(MsgFmtFromStageUnknown, from, wfName),
				})
				continue
			}
			if refIdx >= i {
				*errs = append(*errs, ValidationErrorEntry{
					Path:    fmt.Sprintf(PathFmtFromStage, wfName, i, j),
					Message: fmt.Sprintf(MsgFmtFromStageNotEarlier, from, refIdx, i),
				})
			}
		}
	}
}
