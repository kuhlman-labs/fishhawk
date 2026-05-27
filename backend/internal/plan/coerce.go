package plan

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Coercion records one field-level coercion applied by TryCoerce.
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
var coercionRegistry map[string]coercionEntry

func init() {
	coercionRegistry = buildCoercionRegistry()
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

// TryCoerce attempts to fix the known string-elision class of plan schema
// violations: cases where an agent emits a bare string where the schema
// expects an object. The set of coercible paths is derived at init time from
// the x-coerce-principal / x-coerce-defaults annotations in the embedded
// schema — no per-field code changes are required when the schema gains a new
// annotated $defs entry.
//
// Returns (coercedBytes, coercions, nil) when coercion produces a valid plan.
// Returns (nil, nil, nil) when no string-valued nested-object fields are
// detected AND the original data already validates — caller keeps original
// bytes. Returns (coercedBytes, coercions, err) when coercions were applied
// but re-validation still fails — callers use coercedBytes to report the
// post-coercion violation rather than the original error (which may name a
// field already fixed by coercion). Returns (nil, nil, err) when no coercions
// apply and the original data is invalid — caller falls through to the 400 path.
func TryCoerce(data []byte, now time.Time) ([]byte, []Coercion, error) {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, nil, nil
	}

	var coercions []Coercion

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
		// No string-valued nested-object fields were detected. If the
		// original data is already valid, signal the caller to keep it
		// unchanged. If invalid, propagate the failure so the caller can
		// fall through to the 400 path.
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
