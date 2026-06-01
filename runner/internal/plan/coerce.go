package plan

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Coercion records one field-level coercion applied by TryCoerce.
//
// Mirrors backend/internal/plan.Coercion. The runner module cannot import
// backend packages (Go internal-package boundary), so the type is duplicated
// here. Keep field tags and semantics identical to the backend copy so the
// runner's structured logs match what compliance will see in the backend's
// plan_coerced audit entries.
type Coercion struct {
	FieldPath     string `json:"field_path"`
	OriginalType  string `json:"original_type"`
	OriginalValue any    `json:"original_value"`
	CoercedTo     any    `json:"coerced_to"`
}

// coercionEntry describes how to coerce a bare string at a given schema path.
type coercionEntry struct {
	isArrayItem bool
	principal   string         // field that receives the bare string value
	defaults    map[string]any // non-principal defaults; "<<runtime:now>>" is substituted at coercion time
}

// coercionAnnotation holds the x-coerce-* data extracted from a $defs entry.
type coercionAnnotation struct {
	principal string
	defaults  map[string]any
}

// coercionRegistry is the schema-derived set of paths that TryCoerce handles,
// built once at package init by parsing the embedded schema.
//
// Mirror of backend/internal/plan.coercionRegistry. Both packages must derive
// the same registry from the same embedded schema so runner-coerced bytes pass
// the backend's Validate cleanly (idempotent — backend re-coercion finds
// nothing to fix).
var coercionRegistry map[string]coercionEntry

// optionalTopLevelFields is the schema-derived set of top-level properties that
// are NOT in the schema's `required` array (properties \\ required), built once
// at package init. TryCoerce drops any of these whose value is JSON null,
// treating null-as-absent. Required fields are intentionally excluded so a null
// required field still fails Validate with a precise message.
//
// Mirror of backend/internal/plan.optionalTopLevelFields.
var optionalTopLevelFields map[string]struct{}

func init() {
	coercionRegistry = buildCoercionRegistry()
	optionalTopLevelFields = buildOptionalTopLevelFields()
}

// buildOptionalTopLevelFields parses the embedded schema and returns the set of
// top-level property names that are not listed in the schema's `required`
// array. Schema-derived so it auto-adapts as the schema evolves (today:
// decomposition, risks_and_assumptions).
func buildOptionalTopLevelFields() map[string]struct{} {
	const schemaPath = "schemas/plan-standard-v1.schema.json"
	data, err := schemaFS.ReadFile(schemaPath)
	if err != nil {
		panic(fmt.Sprintf("plan: coerce: read schema %s: %v", schemaPath, err))
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		panic(fmt.Sprintf("plan: coerce: parse schema %s: %v", schemaPath, err))
	}

	required := make(map[string]struct{})
	if reqRaw, ok := schema["required"].([]any); ok {
		for _, r := range reqRaw {
			if s, ok := r.(string); ok {
				required[s] = struct{}{}
			}
		}
	}

	optional := make(map[string]struct{})
	if props, ok := schema["properties"].(map[string]any); ok {
		for name := range props {
			if _, isRequired := required[name]; !isRequired {
				optional[name] = struct{}{}
			}
		}
	}
	return optional
}

func buildCoercionRegistry() map[string]coercionEntry {
	const schemaPath = "schemas/plan-standard-v1.schema.json"
	data, err := schemaFS.ReadFile(schemaPath)
	if err != nil {
		panic(fmt.Sprintf("plan: coerce: read schema %s: %v", schemaPath, err))
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		panic(fmt.Sprintf("plan: coerce: parse schema %s: %v", schemaPath, err))
	}

	defs, _ := schema["$defs"].(map[string]any)

	// Collect x-coerce-* annotations from $defs entries.
	annotByDef := make(map[string]coercionAnnotation)
	for name, raw := range defs {
		def, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		principal, ok := def["x-coerce-principal"].(string)
		if !ok {
			continue
		}
		defaultsRaw, _ := def["x-coerce-defaults"].(map[string]any)
		defaults := make(map[string]any, len(defaultsRaw))
		for k, v := range defaultsRaw {
			defaults[k] = v
		}
		annotByDef[name] = coercionAnnotation{principal, defaults}
	}

	reg := make(map[string]coercionEntry)
	if props, ok := schema["properties"].(map[string]any); ok {
		walkSchemaProps(props, "", defs, annotByDef, reg)
	}
	return reg
}

// walkSchemaProps recursively walks a JSON Schema properties map and populates
// reg with coercionEntry values for every annotated path discovered.
func walkSchemaProps(props map[string]any, prefix string, defs map[string]any, annotByDef map[string]coercionAnnotation, reg map[string]coercionEntry) {
	for key, propRaw := range props {
		prop, ok := propRaw.(map[string]any)
		if !ok {
			continue
		}
		path := prefix + "/" + key

		if ref, ok := prop["$ref"].(string); ok {
			defName := defNameFromRef(ref)
			if ann, ok := annotByDef[defName]; ok {
				reg[path] = coercionEntry{isArrayItem: false, principal: ann.principal, defaults: ann.defaults}
			} else if defName != "" {
				// Recurse into the referenced def's own properties.
				if def, ok := defs[defName].(map[string]any); ok {
					if nested, ok := def["properties"].(map[string]any); ok {
						walkSchemaProps(nested, path, defs, annotByDef, reg)
					}
				}
			}
			continue
		}

		propType, _ := prop["type"].(string)
		if propType == "array" {
			if items, ok := prop["items"].(map[string]any); ok {
				if ref, ok := items["$ref"].(string); ok {
					defName := defNameFromRef(ref)
					if ann, ok := annotByDef[defName]; ok {
						reg[path] = coercionEntry{isArrayItem: true, principal: ann.principal, defaults: ann.defaults}
					}
				}
			}
			continue
		}

		if nested, ok := prop["properties"].(map[string]any); ok {
			walkSchemaProps(nested, path, defs, annotByDef, reg)
		}
	}
}

// defNameFromRef extracts the $defs name from a JSON Pointer $ref.
// "#/$defs/ticket-reference" → "ticket-reference".
func defNameFromRef(ref string) string {
	const prefix = "#/$defs/"
	if strings.HasPrefix(ref, prefix) {
		return ref[len(prefix):]
	}
	return ""
}

// buildCoerced constructs the replacement object for a bare string, applying
// defaults and substituting the <<runtime:now>> sentinel with now.
func buildCoerced(entry coercionEntry, value string, now time.Time) map[string]any {
	coerced := make(map[string]any, len(entry.defaults)+1)
	for k, v := range entry.defaults {
		if s, ok := v.(string); ok && s == "<<runtime:now>>" {
			coerced[k] = now.UTC().Format(time.RFC3339)
		} else {
			coerced[k] = v
		}
	}
	coerced[entry.principal] = value
	return coerced
}

// navigateTo walks a path segment slice through a map[string]any tree and
// returns the value at the final segment's parent map plus the final key.
// Returns (nil, "", false) when any intermediate level is missing or not a map.
func navigateTo(m map[string]any, parts []string) (map[string]any, string, bool) {
	if len(parts) == 0 {
		return nil, "", false
	}
	parent := m
	for _, seg := range parts[:len(parts)-1] {
		child, ok := parent[seg]
		if !ok {
			return nil, "", false
		}
		childMap, ok := child.(map[string]any)
		if !ok {
			return nil, "", false
		}
		parent = childMap
	}
	return parent, parts[len(parts)-1], true
}

// CoercionRegistrySummary returns a human-readable description of the paths
// registered for coercion. Call at startup with the binary's configured logger
// to confirm the registry is populated without waiting for a misfire.
func CoercionRegistrySummary() string {
	paths := make([]string, 0, len(coercionRegistry))
	for path := range coercionRegistry {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return fmt.Sprintf("%d paths: %s", len(paths), strings.Join(paths, ", "))
}

// TryCoerce attempts to fix two known classes of plan schema violations:
// (1) the string-elision class — an agent emits a bare string where the schema
// expects an object; and (2) the null-optional class — an agent sets an
// optional top-level field to JSON null, which TryCoerce drops (treating
// null-as-absent). A null in a REQUIRED field is left in place so Validate
// still fails with a precise message. The set of coercible paths is derived at init time from
// the x-coerce-principal / x-coerce-defaults annotations in the embedded
// schema — no per-field code changes are required when the schema gains a new
// annotated $defs entry.
//
// Returns (coercedBytes, coercions, nil) when coercion produces a valid plan.
// Returns (nil, nil, nil) when no string-valued nested-object fields are
// detected AND the original data already validates — caller keeps original
// bytes (content_hash stability for the signed upload). Returns
// (coercedBytes, coercions, err) when coercions were applied but re-validation
// still fails — callers use coercedBytes to report the post-coercion
// violation rather than the original error (which may name a field already
// fixed by coercion). Returns (nil, nil, err) when no coercions apply and the
// original data is invalid.
//
// Mirror of backend/internal/plan.TryCoerce. Both packages must apply the same
// defaults so the runner-coerced bytes pass the backend's Validate cleanly
// (idempotent — backend re-coercion finds nothing to fix).
func TryCoerce(data []byte, now time.Time) ([]byte, []Coercion, error) {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, nil, nil
	}

	var coercions []Coercion

	// Drop top-level optional fields whose value is JSON null. encoding/json
	// stores a JSON null as a present key with a nil value, so an agent that
	// emits e.g. "decomposition": null lands here as (present, nil) and would
	// otherwise fail schema validation. Treat null-as-absent for optional fields
	// only; a null in a REQUIRED field is left in place so Validate still fails
	// with a precise message.
	for key := range optionalTopLevelFields {
		if v, ok := m[key]; ok && v == nil {
			delete(m, key)
			coercions = append(coercions, Coercion{
				FieldPath:     "/" + key,
				OriginalType:  "null",
				OriginalValue: nil,
				CoercedTo:     nil,
			})
		}
	}

	for path, entry := range coercionRegistry {
		parts := strings.Split(strings.TrimPrefix(path, "/"), "/")

		if entry.isArrayItem {
			parent, arrayKey, ok := navigateTo(m, parts)
			if !ok {
				continue
			}
			slice, ok := parent[arrayKey].([]any)
			if !ok {
				continue
			}
			for i, elem := range slice {
				s, ok := elem.(string)
				if !ok {
					continue
				}
				coerced := buildCoerced(entry, s, now)
				coercions = append(coercions, Coercion{
					FieldPath:     fmt.Sprintf("%s/%d", path, i),
					OriginalType:  "string",
					OriginalValue: s,
					CoercedTo:     coerced,
				})
				slice[i] = coerced
			}
			parent[arrayKey] = slice
		} else {
			if len(parts) != 1 {
				continue
			}
			v, ok := m[parts[0]]
			if !ok {
				continue
			}
			s, ok := v.(string)
			if !ok {
				continue
			}
			coerced := buildCoerced(entry, s, now)
			coercions = append(coercions, Coercion{
				FieldPath:     path,
				OriginalType:  "string",
				OriginalValue: s,
				CoercedTo:     coerced,
			})
			m[parts[0]] = coerced
		}
	}

	if len(coercions) == 0 {
		// No string-valued nested-object fields detected. If the original
		// data already validates, signal the caller to keep it unchanged
		// (content_hash stability). If invalid, propagate the failure.
		if err := Validate(data); err != nil {
			return nil, nil, err
		}
		return nil, nil, nil
	}

	coercedBytes, err := json.Marshal(m)
	if err != nil {
		return nil, nil, fmt.Errorf("coerce: marshal: %w", err)
	}

	if err := Validate(coercedBytes); err != nil {
		return coercedBytes, coercions, err
	}

	return coercedBytes, coercions, nil
}
