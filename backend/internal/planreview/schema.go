package planreview

import "encoding/json"

// VerdictSchema returns the JSON Schema (Draft 2020-12 compatible object) that
// describes the ReviewVerdict shape emitted by a review agent. It is the SINGLE
// source of truth consumed by both first-class structured-output backends
// (#1324): the Anthropic adapter passes it as OutputConfig.Format.Schema and
// the codex adapter writes VerdictSchemaJSON() to the file it hands
// `codex exec --output-schema`. DecodeVerdict (decode.go) stays the documented
// fallback for non-constrained paths (claudecode, error/unconstrained
// responses).
//
// The verdict and concern-severity enum arrays are built from the SAME
// exported constant lists the validation path consumes — AllVerdicts and
// AllConcernSeverities in review.go — so the schema cannot silently diverge
// from the accepted set (the binding #1324 enum-drift condition). The
// reflection drift test in schema_test.go pins both the field-shape (every
// non-`-` json tag is a property) and the enum equivalence.
//
// Every object sets additionalProperties:false (a closed object): Anthropic's
// server-side schema handling and codex strict mode both expect closed objects,
// and it keeps a reviewer from smuggling unmodeled keys. The ReviewVerdict.Usage
// field (json:"-") is adapter-populated from the API/CLI envelope, never emitted
// by the model, so it is intentionally absent from the schema.
//
// A fresh map is returned on every call so a caller (e.g. the Anthropic SDK,
// which may mutate the schema map in place during request marshaling) can never
// corrupt the shared definition.
func VerdictSchema() map[string]any {
	concernSchema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"severity"},
		"properties": map[string]any{
			"severity": map[string]any{
				"type": "string",
				"enum": severityEnum(),
			},
			"category": map[string]any{"type": "string"},
			"note":     map[string]any{"type": "string"},
			// SuggestedPatch (#1165): an optional unified diff for a mechanical fix.
			"suggested_patch": map[string]any{"type": "string"},
		},
	}

	concernResolutionSchema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"id", "resolution"},
		"properties": map[string]any{
			"id":         map[string]any{"type": "string"},
			"resolution": map[string]any{"type": "string"},
			"note":       map[string]any{"type": "string"},
		},
	}

	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"verdict"},
		"properties": map[string]any{
			"verdict": map[string]any{
				"type": "string",
				"enum": verdictEnum(),
			},
			"concerns": map[string]any{
				"type":  "array",
				"items": concernSchema,
			},
			"free_form": map[string]any{"type": "string"},
			"concern_resolutions": map[string]any{
				"type":  "array",
				"items": concernResolutionSchema,
			},
		},
	}
}

// VerdictSchemaJSON returns VerdictSchema() marshaled to JSON bytes, the form
// the codex adapter writes to its `--output-schema <file>` temp file (#1324).
func VerdictSchemaJSON() ([]byte, error) {
	return json.Marshal(VerdictSchema())
}

// verdictEnum is the schema `verdict` enum, built from the single AllVerdicts
// source of truth so it cannot diverge from the validated set (#1324).
func verdictEnum() []any {
	enum := make([]any, len(AllVerdicts))
	for i, v := range AllVerdicts {
		enum[i] = string(v)
	}
	return enum
}

// severityEnum is the schema concern `severity` enum, built from the single
// AllConcernSeverities source of truth.
func severityEnum() []any {
	enum := make([]any, len(AllConcernSeverities))
	for i, s := range AllConcernSeverities {
		enum[i] = string(s)
	}
	return enum
}
