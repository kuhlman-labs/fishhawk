package agenteval

// JudgeCardSchema returns the JSON Schema (Draft 2020-12 compatible object) that
// constrains the Tier-B LLM-judge response to a schema-guaranteed JudgeCard, the
// companion to planreview.VerdictSchema() for the reviewer verdict (#1324, this
// issue #1326). The judge's only model-call seam (MessageSender) is satisfied in
// production by *anthropic.Client, whose Messages method passes this schema as
// OutputConfig.Format.Schema so the Anthropic Messages API constrains the
// model's output. There is no codex --output-schema / claude --json-schema JSON
// variant needed for this surface: the judge's only concrete backend is the
// Anthropic SDK Messages seam (the #1324 finding).
//
// The schema models the judgeCardWire wire shape, NOT JudgeCard — so Model
// (sourced from the sender's reported model name, json-absent on the wire) is
// intentionally excluded, exactly as VerdictSchema excludes the json:"-" Usage
// field. parseJudgeCard's decode + bounded-score validation + re-roll path stays
// the documented FALLBACK for any non-constrained or error path.
//
// Each dimension's score is {"type":"integer","enum":[...]} where the enum is
// BUILT by looping scoreMin..scoreMax — the SAME constants parseJudgeCard
// validates against — so the schema bound cannot silently diverge from the
// validated bound (the #1324 single-source-of-truth discipline). Every object is
// closed (additionalProperties:false): Anthropic's server-side schema handling
// expects closed objects and it keeps the model from smuggling unmodeled keys.
//
// A fresh map is returned on every call so a caller (e.g. the Anthropic SDK,
// which may mutate the schema map in place during request marshaling) can never
// corrupt a shared definition.
func JudgeCardSchema() map[string]any {
	dimensionSchema := func() map[string]any {
		return map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"score", "rationale"},
			"properties": map[string]any{
				"score": map[string]any{
					"type": "integer",
					"enum": scoreEnum(),
				},
				"rationale": map[string]any{"type": "string"},
			},
		}
	}

	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"meaningful_evidence", "honest_uncertainty", "reasoning_quality"},
		"properties": map[string]any{
			"meaningful_evidence": dimensionSchema(),
			"honest_uncertainty":  dimensionSchema(),
			"reasoning_quality":   dimensionSchema(),
		},
	}
}

// scoreEnum is the schema dimension `score` enum, built from the closed
// [scoreMin, scoreMax] integer range so it cannot diverge from the bound
// parseJudgeCard validates against (#1326 single source of truth).
func scoreEnum() []any {
	enum := make([]any, 0, scoreMax-scoreMin+1)
	for s := scoreMin; s <= scoreMax; s++ {
		enum = append(enum, s)
	}
	return enum
}
