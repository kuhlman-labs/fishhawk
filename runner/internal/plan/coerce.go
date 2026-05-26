package plan

import (
	"encoding/json"
	"fmt"
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

// TryCoerce attempts to fix the known string-elision class of plan schema
// violations: cases where an agent emits a bare string where the schema
// expects an object at /generated_by, /scope/files[], or
// /decomposition/sub_plans[].
//
// Returns (coercedBytes, coercions, nil) when coercion produces a valid plan.
// Returns (nil, nil, nil) when no string-valued nested-object fields are
// detected AND the original data already validates — caller keeps original
// bytes (content_hash stability for the signed upload). Returns (nil, nil, err)
// when coercions were applied but re-validation still fails, or when no
// coercions apply and the original data is invalid — either way the caller
// should fall through to the existing rejection path.
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

	// /generated_by: coerce bare string to canonical object shape.
	if v, ok := m["generated_by"]; ok {
		if s, ok := v.(string); ok {
			coerced := map[string]any{
				"agent":     s,
				"model":     "unknown",
				"timestamp": now.UTC().Format(time.RFC3339),
			}
			coercions = append(coercions, Coercion{
				FieldPath:     "/generated_by",
				OriginalType:  "string",
				OriginalValue: s,
				CoercedTo:     coerced,
			})
			m["generated_by"] = coerced
		}
	}

	// /scope/files[]: coerce each bare string element to {path, operation}.
	if scope, ok := m["scope"].(map[string]any); ok {
		if files, ok := scope["files"].([]any); ok {
			for i, f := range files {
				if s, ok := f.(string); ok {
					coerced := map[string]any{
						"path":      s,
						"operation": "modify",
					}
					coercions = append(coercions, Coercion{
						FieldPath:     fmt.Sprintf("/scope/files/%d", i),
						OriginalType:  "string",
						OriginalValue: s,
						CoercedTo:     coerced,
					})
					files[i] = coerced
				}
			}
			scope["files"] = files
		}
	}

	// /decomposition/sub_plans[]: coerce each bare string element to the
	// sentinel default shape. scope_hint intentionally empty — the coercion
	// is a robustness aid, not a way to hide missing agent output.
	if decomp, ok := m["decomposition"].(map[string]any); ok {
		if subPlans, ok := decomp["sub_plans"].([]any); ok {
			for i, sp := range subPlans {
				if s, ok := sp.(string); ok {
					coerced := map[string]any{
						"title":                        s,
						"scope_hint":                   "",
						"predicted_runtime_minutes":    1,
						"predicted_runtime_confidence": "low",
					}
					coercions = append(coercions, Coercion{
						FieldPath:     fmt.Sprintf("/decomposition/sub_plans/%d", i),
						OriginalType:  "string",
						OriginalValue: s,
						CoercedTo:     coerced,
					})
					subPlans[i] = coerced
				}
			}
			decomp["sub_plans"] = subPlans
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
		return nil, nil, err
	}

	return coercedBytes, coercions, nil
}
