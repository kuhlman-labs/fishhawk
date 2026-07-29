package spec

import (
	"embed"
	"fmt"
)

// Workflow presets (ADR-048 / E29.1). The three canonical onboarding
// seeds (low/medium/high autonomy) live under docs/spec/ and are
// mirrored into presets/ by scripts/sync-schemas. The backend embeds
// them so the App-PR onboarding path (E29.7) can serve the same
// canonical bytes the CLI generator (cli/internal/spec) uses — the
// backend/cli module wall forbids importing the CLI generator, so each
// side embeds its own mirror. The backend generator itself is deferred
// to E29.7; this file gives E29.7 the embedded bytes and a validation
// test now.
//
//go:embed presets/workflow-preset-low.yaml presets/workflow-preset-medium.yaml presets/workflow-preset-high.yaml
var presetFS embed.FS

// Preset names an autonomy tier in the preset library. Each maps to a
// canonical workflow-v2 document under docs/spec/, mirrored into
// presets/ (see docs/spec/workflow-preset.md). The three documents are
// one shared base plus a single `autonomy:` line; the constant's value IS
// that line's tier, and the parser derives the OperatorAgent block from
// it via expandTier.
type Preset string

const (
	// PresetLow is human-led (`autonomy: low`): nothing is delegated,
	// every judgment point pages the human.
	PresetLow Preset = "low"
	// PresetMedium is the default (`autonomy: medium`): the operator
	// agent may approve, route fix-ups and retry under their named
	// conditions; waive and merge stay human.
	PresetMedium Preset = "medium"
	// PresetHigh (`autonomy: high`) adds waive (the sole open
	// low-severity concern) and merge (gates resolved, CI green) on top
	// of medium.
	PresetHigh Preset = "high"
)

// presetPaths maps each preset to its embedded canonical bytes path.
var presetPaths = map[Preset]string{
	PresetLow:    "presets/workflow-preset-low.yaml",
	PresetMedium: "presets/workflow-preset-medium.yaml",
	PresetHigh:   "presets/workflow-preset-high.yaml",
}

// PresetBytes returns the canonical embedded bytes for a preset.
// Returns an error for an unknown preset. Callers validate via
// ParseBytes (see preset_test.go); the bytes are the mirror of the
// docs/spec/ canonical, kept in lockstep by the schema-sync gate.
func PresetBytes(preset Preset) ([]byte, error) {
	path, ok := presetPaths[preset]
	if !ok {
		return nil, fmt.Errorf("spec: unknown preset %q (want one of low, medium, high)", preset)
	}
	data, err := presetFS.ReadFile(path)
	if err != nil {
		// Embedded at compile time via the //go:embed above, so a read
		// failure is a build-time invariant violation, not user input.
		return nil, fmt.Errorf("spec: read embedded preset %q: %w", path, err)
	}
	return data, nil
}
