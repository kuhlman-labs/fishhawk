package plan

import (
	"encoding/json"
	"fmt"
	"strings"
)

// structuredOutputDroppedKeywords is the set of JSON Schema keywords the
// claude CLI's --json-schema structured-output subset rejects (verified
// against claude 2.1.186, 2026-06-23): when any of these survive, the CLI
// silently emits no structured_output and the model falls back to free text.
// $ref/$defs are eliminated by inlining (not listed here — they get special
// handling); the rest are stripped wherever they appear:
//   - format ............ (uri / date-time) — unsupported assertion keyword
//   - $schema / $id ..... dialect/identifier metadata the subset rejects
//   - x-coerce-principal / x-coerce-defaults — Fishhawk runtime-coercion
//     annotations (#537) with no meaning to the CLI
//
// oneOf is rejected too, but the canonical standard_v1 schema uses none, so
// there is no oneOf to strip — derive_test asserts none sneaks in.
var structuredOutputDroppedKeywords = map[string]bool{
	"format":             true,
	"$schema":            true,
	"$id":                true,
	"x-coerce-principal": true,
	"x-coerce-defaults":  true,
}

// maxInlineDepth bounds the recursive $ref inlining so a future cyclic
// schema (a $def that transitively references itself) degrades to a
// derivation error — caught by the runner's graceful-fallback path (#1325)
// — rather than recursing without bound. The canonical standard_v1 schema
// is acyclic and nests only a handful of levels, so this ceiling is far
// above any legitimate depth.
const maxInlineDepth = 64

// StructuredOutputSchema returns a claude-CLI-structured-output-compatible
// derivation of the embedded canonical standard_v1 plan schema (#1325). The
// derivation (a) recursively INLINES every $ref against $defs (sibling keys
// like description override the dereferenced target) and (b) STRIPS the
// keywords the CLI subset rejects (see structuredOutputDroppedKeywords, plus
// $ref/$defs eliminated by inlining). Everything the subset supports — nested
// objects, arrays-of-objects, enum, const, pattern, minLength/maxLength/
// minItems/minimum, additionalProperties:false, required — is preserved, so the
// derived schema still constrains the plan SHAPE faithfully.
//
// Deriving at runtime from the single embedded source is the strongest
// never-drift guarantee (#1324 discipline): there is no second committed schema
// file to fall out of sync. A derivation failure (a malformed embedded schema,
// a $ref that does not resolve, or a cycle exceeding maxInlineDepth) returns a
// non-nil error; the runner logs it and proceeds with an UNCONSTRAINED
// invocation, falling back to the TryCoerce+validate path — never a hard
// regression.
func StructuredOutputSchema() ([]byte, error) {
	const path = "schemas/plan-standard-v1.schema.json"
	data, err := schemaFS.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("plan: read embedded schema %s: %w", path, err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("plan: parse embedded schema %s: %w", path, err)
	}

	defs, _ := root["$defs"].(map[string]any)

	derived, err := inlineNode(root, defs, 0)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(derived)
	if err != nil {
		return nil, fmt.Errorf("plan: marshal derived schema: %w", err)
	}
	return out, nil
}

// inlineNode recursively resolves $refs against defs and strips the CLI-rejected
// keywords from one schema node. A map carrying a "$ref" is replaced by the
// inlined target with the node's own sibling keys layered on top (sibling
// wins); $defs and the dropped keywords are elided everywhere. Slices and
// scalars pass through with their elements recursively inlined.
func inlineNode(node any, defs map[string]any, depth int) (any, error) {
	if depth > maxInlineDepth {
		return nil, fmt.Errorf("plan: schema $ref inlining exceeded max depth %d (cycle?)", maxInlineDepth)
	}
	switch n := node.(type) {
	case map[string]any:
		// A $ref node: resolve the target, inline it, then overlay siblings.
		if ref, ok := n["$ref"].(string); ok {
			target, err := resolveRef(ref, defs)
			if err != nil {
				return nil, err
			}
			inlined, err := inlineNode(target, defs, depth+1)
			if err != nil {
				return nil, err
			}
			base, ok := inlined.(map[string]any)
			if !ok {
				// A non-object target (unusual) cannot carry siblings; return it.
				return inlined, nil
			}
			for k, v := range n {
				if k == "$ref" || k == "$defs" || structuredOutputDroppedKeywords[k] {
					continue
				}
				iv, err := inlineNode(v, defs, depth+1)
				if err != nil {
					return nil, err
				}
				base[k] = iv
			}
			return base, nil
		}
		out := make(map[string]any, len(n))
		for k, v := range n {
			if k == "$defs" || structuredOutputDroppedKeywords[k] {
				continue
			}
			iv, err := inlineNode(v, defs, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = iv
		}
		return out, nil
	case []any:
		out := make([]any, len(n))
		for i, v := range n {
			iv, err := inlineNode(v, defs, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = iv
		}
		return out, nil
	default:
		return node, nil
	}
}

// resolveRef dereferences a local "#/$defs/<name>" pointer against defs.
// Only the local $defs form the canonical schema uses is supported; any other
// form (an external ref, or a pointer outside $defs) is a derivation error so
// the runner falls back rather than emitting a schema with a dangling ref.
func resolveRef(ref string, defs map[string]any) (any, error) {
	const prefix = "#/$defs/"
	if !strings.HasPrefix(ref, prefix) {
		return nil, fmt.Errorf("plan: unsupported $ref %q (only %s<name> is inlined)", ref, prefix)
	}
	name := strings.TrimPrefix(ref, prefix)
	target, ok := defs[name]
	if !ok {
		return nil, fmt.Errorf("plan: $ref %q does not resolve in $defs", ref)
	}
	return target, nil
}
