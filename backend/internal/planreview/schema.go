package planreview

import (
	"encoding/json"
	"sort"
)

// VerdictSchema returns the JSON Schema (Draft 2020-12 compatible object) that
// describes the ReviewVerdict shape emitted by a review agent. It is the SINGLE
// source of truth for the verdict shape. The Anthropic adapter passes it
// verbatim as OutputConfig.Format.Schema, whose lenient server-side handling
// accepts the partial `required` arrays below. The codex adapter does NOT
// consume it directly: codex 0.140's `--output-schema` enforces OpenAI strict
// structured-output rules (every key in each object's `properties` must appear
// in `required`), so the codex path consumes StrictVerdictSchemaJSON() — the
// strict variant DERIVED from this one (every property required, originally-
// optional properties made nullable). DecodeVerdict (decode.go) stays the
// documented fallback for non-constrained paths (claudecode, error/unconstrained
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

// VerdictSchemaJSON returns VerdictSchema() marshaled to JSON bytes. This is the
// lenient variant consumed by the Anthropic structured-output path; the codex
// `--output-schema` path consumes StrictVerdictSchemaJSON() instead (#1324/#1330).
func VerdictSchemaJSON() ([]byte, error) {
	return json.Marshal(VerdictSchema())
}

// StrictVerdictSchema returns a strict-structured-output-compatible variant
// DERIVED from VerdictSchema() (the single source of truth). codex 0.140's
// `--output-schema` enforces OpenAI strict mode: each object's `required` must
// enumerate EVERY key in its `properties`, and a field that is semantically
// optional is expressed by widening its type to a nullable union ([T, "null"])
// rather than omitting it from `required`. VerdictSchema()'s partial `required`
// arrays (top-level [verdict], concerns.items [severity], concern_resolutions.items
// [id, resolution]) are legal under Anthropic's lenient handling but rejected by
// codex with HTTP 400 invalid_json_schema (#1330), so the codex adapter consumes
// this variant.
//
// The transform is a recursive deep copy: for every object node carrying both
// `properties` and `required` it rewrites `required` to list every property key
// (sorted, for deterministic output), and for each property NOT in the ORIGINAL
// `required` set it widens the declared `type` to include "null" (a bare string
// T becomes [T, "null"]; an existing type array gains "null"). A property that
// was already required keeps its plain type — so the required `severity` enum
// stays a plain enum string, never enum+null. VerdictSchema()'s map is never
// mutated (the transform copies every node) and a fresh map is returned on every
// call.
func StrictVerdictSchema() map[string]any {
	// strictTransform always returns a map[string]any for a map input
	// (VerdictSchema()'s top-level node is an object), so the assertion holds.
	strict, _ := strictTransform(VerdictSchema()).(map[string]any)
	return strict
}

// StrictVerdictSchemaJSON returns StrictVerdictSchema() marshaled to JSON bytes,
// the form the codex adapter writes to its `--output-schema <file>` temp file
// (#1330). The sorted `required` arrays make the output deterministic across
// calls so the written file is byte-stable.
func StrictVerdictSchemaJSON() ([]byte, error) {
	return json.Marshal(StrictVerdictSchema())
}

// strictTransform recursively deep-copies a JSON-schema node, applying the
// strict-mode rewrite to every object that carries both `properties` and
// `required`: `required` is rewritten to every property key (sorted) and each
// originally-optional property's type is made nullable. It recurses into every
// property value and array `items` so nested object schemas (concerns.items,
// concern_resolutions.items) are transformed too. The source node is read but
// never mutated; a freshly-allocated value is returned.
func strictTransform(node any) any {
	switch v := node.(type) {
	case map[string]any:
		props, hasProps := v["properties"].(map[string]any)
		reqList, hasReq := v["required"].([]any)
		strictObject := hasProps && hasReq

		out := make(map[string]any, len(v))
		for k, val := range v {
			switch {
			case k == "required" && strictObject:
				// Rewritten after the loop from the full property key set.
				continue
			case k == "properties" && strictObject:
				origRequired := make(map[string]bool, len(reqList))
				for _, r := range reqList {
					if s, ok := r.(string); ok {
						origRequired[s] = true
					}
				}
				newProps := make(map[string]any, len(props))
				for pk, pv := range props {
					transformed := strictTransform(pv)
					if !origRequired[pk] {
						transformed = makeNullable(transformed)
					}
					newProps[pk] = transformed
				}
				out[k] = newProps
			default:
				out[k] = strictTransform(val)
			}
		}
		if strictObject {
			keys := make([]string, 0, len(props))
			for pk := range props {
				keys = append(keys, pk)
			}
			sort.Strings(keys)
			req := make([]any, len(keys))
			for i, pk := range keys {
				req[i] = pk
			}
			out["required"] = req
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = strictTransform(e)
		}
		return out
	default:
		return v
	}
}

// makeNullable widens an already-deep-copied schema node's `type` to include
// "null": a bare string type T becomes [T, "null"]; an existing type array gains
// "null" if absent. A node without a `type` (or a non-string/array type) is
// returned unchanged. It mutates only the passed-in copy, never the source.
func makeNullable(node any) any {
	m, ok := node.(map[string]any)
	if !ok {
		return node
	}
	switch t := m["type"].(type) {
	case string:
		m["type"] = []any{t, "null"}
	case []any:
		has := false
		for _, e := range t {
			if s, ok := e.(string); ok && s == "null" {
				has = true
				break
			}
		}
		if !has {
			m["type"] = append(t, "null")
		}
	}
	return m
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
