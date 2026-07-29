package spec

import (
	"fmt"
	"sort"
	"strings"
)

// workflow-v2 same-document reuse: `defaults` and `extends` (E52.4 / #2216).
//
// v2 gains the two reuse primitives it never had, both SAME-DOCUMENT only
// (cross-file `include:` is deliberately out of scope, ADR-067):
//
//	defaults:   declarable at FILE level and at WORKFLOW level, scoped to the
//	            executor / reviewers / budget blocks
//	extends:    on a workflow, naming another workflow key in the same
//	            document as its base
//
// RESOLUTION LADDER — one documented order, later winning:
//
//	file `defaults` -> `extends` base -> workflow `defaults` -> the stage's
//	own declaration
//
// A workflow-level default therefore OVERRIDES a value an inherited base
// stage declared explicitly (which is what makes "extend the base but swap
// the agent everywhere" expressible), while a stage declared on the deriving
// workflow still wins over everything.
//
// MERGE SEMANTICS, decided rather than implicit:
//
//   - objects merge KEY-WISE, the winning side per the ladder taking each key
//   - ARRAYS REPLACE WHOLESALE, never concatenate. A governance file must
//     never accumulate an approver or a reviewer by inheritance the author
//     did not write (the GitLab `extends:` / Actions matrix-include pitfall).
//   - `stages` is the ONE keyed array: merged BY STAGE ID, base order
//     preserved, new ids appended. Reordering is not expressible.
//   - `reviewers` is taken WHOLE from exactly ONE rung and is NEVER blended.
//     See applyDefaults' doc comment for why this asymmetry is deliberate.
//   - an executor default is DROPPED WHOLESALE for a stage selecting a
//     different oneOf branch, and for ANY stage on the `human` or `delegate`
//     branch, which admit no key beyond their own. See mergeExecutor.
//
// ORDERING (see ParseBytes, and backend/internal/spec/README.md):
//
//	checkV2RemovedForms   -> actionable messages beat every generic one
//	resolveV2Reuse        -> HERE: BEFORE schema validation
//	schema.Validate       -> the structural gate, on the RESOLVED document
//	normalizeV2Shapes     -> after validation
//	stripV2Reuse          -> before the typed decode
//
// THE LOAD-BEARING DECISION is that resolution runs BEFORE schema.Validate,
// inverting normalizeV2Shapes' placement. The schema then validates the
// RESOLVED document, so (a) a stage inheriting its executor from `defaults`
// still satisfies $defs/stage's `required: [id, type, executor]` with NO
// schema relaxation, (b) a merged executor is checked against the real
// oneOf / unevaluatedProperties branch rules rather than a weakened copy of
// them re-implemented in Go, and (c) a deriving workflow may omit `stages`
// entirely and still satisfy $defs/workflow's `required: [stages]`.
//
// The cost of that inversion is that this pass sees an UNVALIDATED document.
// It is bounded deliberately: every shape mismatch is SKIPPED — never
// panicked on, never reported — because the schema validates the same
// document immediately afterwards and owns every structural message. A
// `defaults: "oops"` or a non-string `extends` is still a schema error, just
// one rung later. The only errors raised here are the three no later layer
// can diagnose: an `extends` naming an absent workflow, an `extends` cycle,
// and a duplicate stage id in a workflow's OWN stages list (which the keyed
// merge would otherwise collapse onto one base stage, hiding the authoring
// error from the post-resolution uniqueness check in validate.go).
//
// The reuse keys are left IN PLACE through schema validation, so the author's
// own declarations are validated too; stripV2Reuse deletes them afterwards,
// before the typed decode, which runs under DisallowUnknownFields.

// reuseDefaultKeys are the three stage-level blocks a `defaults` block can
// carry, in a fixed order so a resolved document is byte-stable.
var reuseDefaultKeys = []string{"executor", "reviewers", "budget"}

// executorBranchKeys are the three mutually exclusive executor branches from
// $defs/executor's oneOf, in the schema's declaration order.
var executorBranchKeys = []string{"agent", "human", "delegate"}

// resolveV2Reuse rewrites a raw yaml.v3-decoded workflow-v2 document in
// place, folding the file-level `defaults`, each workflow's `extends` base
// and its workflow-level `defaults` into every stage, per the ladder above.
// Called from ParseBytes for a routed major >= 2 only, BEFORE schema
// validation.
//
// Returns a *ValidationError for the three authoring errors no later layer
// can diagnose (unknown base, cycle, duplicate stage id); every other
// problem — including every shape mismatch — is left to the schema.
func resolveV2Reuse(raw any) error {
	root, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	workflows, ok := root["workflows"].(map[string]any)
	if !ok {
		return nil
	}
	fileDefaults, _ := root["defaults"].(map[string]any)

	// Workflow names are visited in SORTED order so the reported first
	// offender is deterministic across runs (Go randomizes map iteration),
	// matching the v2removed sweep's determinism contract.
	names := sortedKeys(workflows)

	// Duplicate stage ids are rejected BEFORE any merge. Every workflow in
	// the document is checked here, so a base reached only via `extends` is
	// covered by its own iteration of this loop.
	for _, name := range names {
		wf, ok := workflows[name].(map[string]any)
		if !ok {
			continue
		}
		if err := checkDuplicateStageIDs(wf, name); err != nil {
			return err
		}
	}

	r := &reuseResolver{
		workflows:    workflows,
		names:        names,
		fileDefaults: fileDefaults,
		memo:         make(map[string]map[string]any, len(names)),
		onStack:      make(map[string]bool, len(names)),
	}
	for _, name := range names {
		resolved, err := r.resolve(name, nil)
		if err != nil {
			return err
		}
		if resolved != nil {
			workflows[name] = resolved
		}
	}
	return nil
}

// reuseResolver carries the per-document state of the extends walk: the
// memoized resolution of each workflow (so a chain of any depth resolves
// transitively and a diamond resolves once) and the on-stack set the cycle
// detector reads.
type reuseResolver struct {
	workflows    map[string]any
	names        []string
	fileDefaults map[string]any
	memo         map[string]map[string]any
	onStack      map[string]bool
}

// resolve returns the fully resolved form of one workflow: its stages
// carrying the file defaults, its base's stages, its own defaults and its
// own declarations, folded per the ladder. chain carries the extends path
// walked so far, in declaration order, for the cycle message.
func (r *reuseResolver) resolve(name string, chain []string) (map[string]any, error) {
	if done, ok := r.memo[name]; ok {
		return done, nil
	}
	wf, ok := r.workflows[name].(map[string]any)
	if !ok {
		// Not an object: the schema reports it. Nothing to resolve.
		return nil, nil
	}
	if r.onStack[name] {
		return nil, cycleError(name, chain)
	}
	r.onStack[name] = true
	defer delete(r.onStack, name)

	wfDefaults, _ := wf["defaults"].(map[string]any)
	stagesRaw, declaresStages := wf["stages"]
	ownStages, stagesWellFormed := stagesRaw.([]any)

	// A `stages` key that is PRESENT but not a list is a shape mismatch this
	// pass skips like every other — but skipping it must not also mean
	// OVERWRITING it. Under `extends` the resolved base stages would be
	// written into this key, replacing the author's invalid declaration with
	// a valid inherited list, so `stages: nope` / `stages: null` would parse
	// as though the key had been omitted entirely and an invalid document
	// would execute. The malformed value is left exactly as authored instead,
	// and the schema — which validates the resolved document immediately
	// afterwards — reports it at /workflows/<name>/stages.
	malformedStages := declaresStages && !stagesWellFormed

	var stages []any
	if baseName, ok := wf["extends"].(string); ok {
		base, err := r.resolveBase(name, baseName, chain)
		if err != nil {
			return nil, err
		}
		if !malformedStages {
			baseStages, _ := base["stages"].([]any)
			// The base's stages already carry the file defaults and the BASE's
			// own workflow defaults. This workflow's defaults sit ABOVE them on
			// the ladder, so they are applied OVER each inherited stage.
			inherited := make([]any, 0, len(baseStages))
			for _, bs := range baseStages {
				st := deepCopyAny(bs)
				if m, ok := st.(map[string]any); ok {
					applyDefaults(m, wfDefaults, true /* defaults win */)
				}
				inherited = append(inherited, st)
			}
			// This workflow's own stage declarations are highest of all.
			stages = mergeStagesByID(inherited, ownStages, mergeDefaultsBlocks(r.fileDefaults, wfDefaults))
		}
	} else if ownStages != nil {
		effective := mergeDefaultsBlocks(r.fileDefaults, wfDefaults)
		stages = make([]any, 0, len(ownStages))
		for _, os := range ownStages {
			st := deepCopyAny(os)
			if m, ok := st.(map[string]any); ok {
				applyDefaults(m, effective, false /* the stage wins */)
			}
			stages = append(stages, st)
		}
	}

	out := make(map[string]any, len(wf))
	for k, v := range wf {
		out[k] = v
	}
	// A workflow that declared no `stages` and extends nothing keeps the key
	// ABSENT, so the schema reports its own required-property error rather
	// than a fabricated empty list failing minItems. A workflow whose
	// `stages` is present but malformed keeps that value verbatim, per
	// malformedStages above.
	if stages != nil {
		out["stages"] = stages
	}
	r.memo[name] = out
	return out, nil
}

// resolveBase resolves the workflow this one extends, rejecting an unknown
// base with a message that names the missing workflow AND lists the ones the
// document does define, sorted.
func (r *reuseResolver) resolveBase(name, baseName string, chain []string) (map[string]any, error) {
	if _, ok := r.workflows[baseName]; !ok {
		return nil, &ValidationError{
			Path: fmt.Sprintf("/workflows/%s/extends", name),
			Message: fmt.Sprintf(
				"extends names workflow %q, which this document does not define; defined workflows: %s",
				baseName, strings.Join(r.names, ", "),
			),
		}
	}
	base, err := r.resolve(baseName, append(append([]string{}, chain...), name))
	if err != nil {
		return nil, err
	}
	if base == nil {
		// The key exists but its value is not an object. There is nothing to
		// inherit; the schema reports the malformed base with its own message.
		return map[string]any{}, nil
	}
	return base, nil
}

// cycleError names the full extends chain in declaration order, e.g.
// "a -> b -> a". The self-reference case (a -> a) falls out of the same
// detector rather than needing its own branch.
func cycleError(name string, chain []string) error {
	// Trim the chain to the cycle itself: everything from the first
	// occurrence of the repeated name onwards, then close the loop.
	start := 0
	for i, n := range chain {
		if n == name {
			start = i
			break
		}
	}
	cycle := append(append([]string{}, chain[start:]...), name)
	return &ValidationError{
		Path: fmt.Sprintf("/workflows/%s/extends", name),
		Message: fmt.Sprintf(
			"extends forms a cycle: %s; a workflow cannot inherit from itself, directly or transitively",
			strings.Join(cycle, " -> "),
		),
	}
}

// checkDuplicateStageIDs rejects a workflow whose OWN stages list declares
// the same id more than once, naming the id and both positions.
//
// This runs BEFORE the keyed merge, not after, because the merge would
// silently swallow the authoring error: two deriving stages both with id
// "implement" would both merge onto the one base "implement" stage and
// collapse into a single resolved stage, so the post-resolution uniqueness
// check in validate.go would see one id and pass. The merge has no sensible
// meaning for two entries targeting one base stage — there is no defensible
// order between them — so it refuses rather than inventing one.
func checkDuplicateStageIDs(wf map[string]any, name string) error {
	stages, ok := wf["stages"].([]any)
	if !ok {
		return nil
	}
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
			return &ValidationError{
				Path: fmt.Sprintf("/workflows/%s/stages", name),
				Message: fmt.Sprintf(
					"duplicate stage id %q declared at positions %d and %d; two entries targeting one stage have no defined merge order under extends, so the document is rejected rather than resolved",
					id, prev, i,
				),
			}
		}
		seen[id] = i
	}
	return nil
}

// mergeStagesByID is the ONE keyed-array exception to wholesale array
// replacement. Base stages keep their order and positions; a deriving stage
// whose `id` matches merges key-wise onto the base stage IN THE BASE'S
// POSITION; a deriving stage with a new id is appended in declaration order,
// with newDefaults folded UNDER it (it has no base rung to inherit from).
// Reordering is deliberately not expressible.
func mergeStagesByID(baseStages []any, ownStages []any, newDefaults map[string]any) []any {
	if len(ownStages) == 0 {
		return baseStages
	}
	pos := make(map[string]int, len(baseStages))
	for i, bs := range baseStages {
		st, ok := bs.(map[string]any)
		if !ok {
			continue
		}
		if id, ok := st["id"].(string); ok {
			pos[id] = i
		}
	}
	out := append([]any{}, baseStages...)
	for _, osRaw := range ownStages {
		own, ok := deepCopyAny(osRaw).(map[string]any)
		if !ok {
			out = append(out, deepCopyAny(osRaw))
			continue
		}
		id, _ := own["id"].(string)
		if i, matched := pos[id]; matched && id != "" {
			if base, ok := out[i].(map[string]any); ok {
				out[i] = mergeStage(base, own)
				continue
			}
		}
		applyDefaults(own, newDefaults, false /* the stage wins */)
		out = append(out, own)
	}
	return out
}

// mergeStage folds a deriving stage onto its matching base stage. Every key
// merges key-wise with the deriving side winning, EXCEPT the three blocks
// applyDefaults documents: `executor` carries the branch rule, `budget`
// merges key-wise, and `reviewers` is taken WHOLE from whichever side
// declared it, deriving first.
func mergeStage(base, own map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(own))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range own {
		switch k {
		case "reviewers":
			// Block-level: the deriving side declared it, so it is taken
			// whole, never blended with the base's block.
			out[k] = v
		case "executor":
			// The DERIVING declaration is the authoritative branch here:
			// it is the most specific thing the author wrote about this
			// stage, so a base stage selecting a different branch is
			// dropped rather than merged into it.
			out[k] = mergeExecutor(out[k], v, v)
		default:
			out[k] = mergeKeyWise(out[k], v)
		}
	}
	return out
}

// applyDefaults folds one `defaults` block into ONE stage, in place.
//
// defaultsWin selects the ladder direction: false when the defaults sit
// BELOW the stage (the normal case — the stage's own declaration is highest),
// true when they sit ABOVE it (a deriving workflow's `defaults` over a stage
// inherited from its `extends` base, per the ladder).
//
// THE PRINCIPLE governing which of the three blocks merges and which does
// not: a block that determines review AUTHORITY is taken as authored or not
// at all; blocks that carry only execution parameters merge key-wise.
//
// `reviewers` is the authority block, so it is taken WHOLE from exactly one
// rung and NEVER blended. A recursive key-wise merge of file defaults
// {human: 1, agents: [a, b]} into a stage declaring {agents: [c]} would
// resolve to {human: 1, agents: [c]} — the stage's agents correctly replace
// the default's, but `human: 1` is SUPPLEMENTED from a block the author did
// not write on that stage. That is not cosmetic: planreview.ResolveAuthority
// keys on AgentCount() > 0 && Human == 0 for GATING versus ADVISORY, so the
// supplemented human silently converts a GATING stage into an arbitrable
// ADVISORY one and nothing in the document the author wrote says so. It is
// the same hazard the array-replace rule exists to prevent, one level up.
//
// `executor` and `budget` keep the KEY-WISE merge. They carry no authority
// semantics, and block-level replacement would make a file-level
// defaults.executor.timeout useless for every stage that declares an
// executor — which is nearly all of them — gutting the de-duplication this
// change exists to enable. The asymmetry is deliberate, not an oversight.
func applyDefaults(stage map[string]any, defaults map[string]any, defaultsWin bool) {
	if len(defaults) == 0 {
		return
	}
	if dv, ok := defaults["reviewers"]; ok {
		// Block-level: replaced whole when the defaults rung wins, inherited
		// whole when the stage declared none, never merged either way.
		if defaultsWin || stage["reviewers"] == nil {
			stage[reviewersKey] = deepCopyAny(dv)
		}
	}
	if dv, ok := defaults["executor"]; ok {
		// The STAGE's own executor is the authoritative branch in both
		// directions: a default must never re-brand the executor its author
		// wrote, which is the whole point of the branch rule.
		own := stage["executor"]
		if defaultsWin {
			stage["executor"] = mergeExecutor(own, deepCopyAny(dv), own)
		} else {
			stage["executor"] = mergeExecutor(deepCopyAny(dv), own, own)
		}
	}
	if dv, ok := defaults["budget"]; ok {
		if defaultsWin {
			stage["budget"] = mergeKeyWise(stage["budget"], deepCopyAny(dv))
		} else {
			stage["budget"] = mergeKeyWise(deepCopyAny(dv), stage["budget"])
		}
	}
}

// reviewersKey is the authority block's key name, named once so the
// block-level rule above reads as a rule rather than as a string literal.
const reviewersKey = "reviewers"

// mergeExecutor merges two executor fragments key-wise with `over` winning
// per key, and carries the BRANCH RULE. The rule has two halves, and both
// collapse the merge WHOLESALE to `authored` — the fragment whose shape the
// resolved stage must keep — dropping the other side entirely, branchless
// keys and all:
//
//  1. DIFFERENT BRANCH. Both sides select one of the three $defs/executor
//     oneOf branches (agent / human / delegate) and those branches DIFFER.
//  2. CLOSED BRANCH. `authored` selects `human` or `delegate` and the merge
//     would introduce ANY key beyond that branch key. Those two oneOf arms
//     declare only their own property, so anything else fails the oneOf —
//     including a BRANCHLESS default the first half does not catch.
//
// `authored` is the stage's own executor when a `defaults` block is being
// folded in (from either ladder direction), and the DERIVING stage's executor
// when a deriving stage merges onto its base. In both cases it is the most
// specific thing the author wrote about that stage.
//
// Without the first half a file-level defaults.executor {agent: claude-code,
// timeout: 30m} grafts `agent` — plus every agent-branch-only key — onto each
// `human: true` gate stage and each DELEGATING deploy stage. Draft 2020-12
// collects unevaluatedProperties against every successfully-evaluated
// subschema (JSON Schema Core §11.3), so the merged executor then fails
// $defs/executor's oneOf and the document is rejected though its author wrote
// nothing wrong. That would make file-level executor defaults unusable in
// exactly the preset-shaped workflows this change exists to de-duplicate.
//
// Without the SECOND half the identical failure returns one case over, for the
// bare {timeout: 30m} default the schema's own `defaults.executor` description
// advertises: it declares no branch, so the first half never fires, and it
// merges key-wise into a `human: true` gate stage to produce
// {human: true, timeout: 30m} — which the human arm rejects just as surely.
// Essentially every real workflow carries a human gate stage, so the
// advertised branchless default would reject nearly every realistic document
// with an opaque oneOf error. Both halves fail the same way (the default is
// dropped, never reinterpreted) and for the same reason: an execution-parameter
// default must never change what a stage's authored executor IS.
func mergeExecutor(under, over, authored any) any {
	um, uok := under.(map[string]any)
	om, ook := over.(map[string]any)
	if !uok || !ook {
		return mergeKeyWise(under, over)
	}
	if ub, ob := executorBranch(um), executorBranch(om); ub != "" && ob != "" && ub != ob {
		return authored
	}
	if am, ok := authored.(map[string]any); ok {
		if ab := executorBranch(am); executorClosedBranches[ab] && mergeWidensBeyond(ab, um, om) {
			return authored
		}
	}
	return mergeKeyWise(um, om)
}

// executorClosedBranches are the two $defs/executor oneOf arms that declare
// NO property beyond their own branch key. With unevaluatedProperties: false
// at the executor root, a stage on one of these branches admits exactly one
// key, so ANY fragment merged into it — branchless or not — breaks the oneOf.
var executorClosedBranches = map[string]bool{"human": true, "delegate": true}

// mergeWidensBeyond reports whether merging the two fragments would yield any
// key other than branch.
func mergeWidensBeyond(branch string, fragments ...map[string]any) bool {
	for _, m := range fragments {
		for k := range m {
			if k != branch {
				return true
			}
		}
	}
	return false
}

// executorBranch reports which of the three oneOf branches an executor
// fragment selects, or "" for a branchless partial like {timeout: 30m}.
func executorBranch(m map[string]any) string {
	for _, k := range executorBranchKeys {
		if _, ok := m[k]; ok {
			return k
		}
	}
	return ""
}

// mergeKeyWise merges two decoded values with `over` winning.
//
// Objects merge RECURSIVELY, key by key. Every other value — scalar, and
// crucially ARRAY — is REPLACED WHOLESALE. Arrays are never concatenated:
// that is the GitLab `extends:` / GitHub Actions matrix-include pitfall, and
// concatenating a `reviewers.agents` or `approvals` list would let a
// governance file accumulate a reviewer or an approver by inheritance that
// the author never wrote.
func mergeKeyWise(under, over any) any {
	um, uok := under.(map[string]any)
	om, ook := over.(map[string]any)
	if !uok || !ook {
		if over == nil {
			return under
		}
		return over
	}
	out := make(map[string]any, len(um)+len(om))
	for k, v := range um {
		out[k] = v
	}
	for k, v := range om {
		out[k] = mergeKeyWise(out[k], v)
	}
	return out
}

// mergeDefaultsBlocks folds a workflow-level `defaults` block onto the
// file-level one, producing the single effective defaults block a stage with
// no base rung sees. The workflow level wins, block by block, under the same
// asymmetry: `reviewers` is taken WHOLE from whichever level declared it
// (workflow first, never blended), `executor` and `budget` merge key-wise,
// and the workflow rung is the authoritative executor branch — it is the more
// specific of the two declarations.
func mergeDefaultsBlocks(file, workflow map[string]any) map[string]any {
	if len(workflow) == 0 {
		return file
	}
	if len(file) == 0 {
		return workflow
	}
	out := make(map[string]any, len(reuseDefaultKeys))
	for _, key := range reuseDefaultKeys {
		fv, fok := file[key]
		wv, wok := workflow[key]
		switch {
		case fok && wok:
			f, w := deepCopyAny(fv), deepCopyAny(wv)
			switch key {
			case reviewersKey:
				out[key] = w
			case "executor":
				out[key] = mergeExecutor(f, w, w)
			default:
				out[key] = mergeKeyWise(f, w)
			}
		case wok:
			out[key] = deepCopyAny(wv)
		case fok:
			out[key] = deepCopyAny(fv)
		}
	}
	return out
}

// stripV2Reuse deletes the reuse keys from a RESOLVED, schema-validated
// document: the file-level `defaults` and each workflow's `defaults` and
// `extends`. It runs after schema validation (so the author's own
// declarations are validated) and before the typed decode, which uses
// DisallowUnknownFields — the same delete-before-decode discipline
// expandNeeds already uses for `needs`.
func stripV2Reuse(raw any) {
	root, ok := raw.(map[string]any)
	if !ok {
		return
	}
	delete(root, "defaults")
	workflows, ok := root["workflows"].(map[string]any)
	if !ok {
		return
	}
	for _, wfRaw := range workflows {
		wf, ok := wfRaw.(map[string]any)
		if !ok {
			continue
		}
		delete(wf, "defaults")
		delete(wf, "extends")
	}
}

// deepCopyAny clones a decoded document subtree, so a base workflow's stages
// can be folded into a deriving workflow without the two aliasing — a merge
// into a shared map would mutate the base a second deriving workflow still
// has to read.
func deepCopyAny(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = deepCopyAny(vv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = deepCopyAny(vv)
		}
		return out
	default:
		return v
	}
}

// sortedKeys returns a map's keys in ascending order, for the determinism
// contract the reported first offender depends on.
func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
