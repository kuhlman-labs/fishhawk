package spec

import (
	"embed"
	"fmt"
)

// Workflow presets (ADR-048 / E29.1). The canonical onboarding seeds
// live under docs/spec/ and are mirrored into presets/ by
// scripts/sync-schemas. They vary on two axes: an autonomy tier
// (low/medium/high) and a repository shape (app/config-only), so six
// documents ship. The backend embeds them so the App-PR onboarding path
// (E29.7) can serve the same canonical bytes the CLI generator
// (cli/internal/spec) uses — the backend/cli module wall forbids
// importing the CLI generator, so each side embeds its own mirror. The
// backend generator itself is deferred to E29.7; this file gives E29.7
// the embedded bytes and a validation test now.
//
//go:embed presets/workflow-preset-low.yaml presets/workflow-preset-medium.yaml presets/workflow-preset-high.yaml presets/workflow-preset-low-config-only.yaml presets/workflow-preset-medium-config-only.yaml presets/workflow-preset-high-config-only.yaml
var presetFS embed.FS

// Preset names an autonomy tier in the preset library. Each maps to a
// canonical workflow-v2 document under docs/spec/, mirrored into
// presets/ (see docs/spec/workflow-preset.md). Within a shape the three
// documents are one shared base plus a single `autonomy:` line; the
// constant's value IS that line's tier, and the parser derives the
// OperatorAgent block from it via expandTier.
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

// Shape names a repository-shape axis in the preset library, orthogonal
// to the autonomy tier. Each (preset, shape) pair maps to a canonical
// workflow-v2 document. The zero value resolves to ShapeApp so every
// existing caller keeps the app shape.
type Shape string

const (
	// ShapeApp is the default shape: an application repository with a
	// test entrypoint. The implement stage runs an execution-grounded
	// verifier and requires tests to be added.
	ShapeApp Shape = "app"
	// ShapeConfigOnly is a config- or docs-only repository with no test
	// entrypoint: the implement stage omits the verifier, drops
	// tests_added_or_updated, keeps ci_green and raises max_files_changed.
	ShapeConfigOnly Shape = "config-only"
)

// presetKey identifies an embedded canonical document by its autonomy
// tier and repository shape.
type presetKey struct {
	preset Preset
	shape  Shape
}

// presetPaths maps each (preset, shape) pair to its embedded canonical
// bytes path.
var presetPaths = map[presetKey]string{
	{PresetLow, ShapeApp}:           "presets/workflow-preset-low.yaml",
	{PresetMedium, ShapeApp}:        "presets/workflow-preset-medium.yaml",
	{PresetHigh, ShapeApp}:          "presets/workflow-preset-high.yaml",
	{PresetLow, ShapeConfigOnly}:    "presets/workflow-preset-low-config-only.yaml",
	{PresetMedium, ShapeConfigOnly}: "presets/workflow-preset-medium-config-only.yaml",
	{PresetHigh, ShapeConfigOnly}:   "presets/workflow-preset-high-config-only.yaml",
}

// PresetBytes returns the canonical embedded bytes for a preset in the
// app shape. It is a thin wrapper over PresetShapeBytes(preset,
// ShapeApp) so every existing caller keeps its signature and the app
// shape. Returns an error for an unknown preset. Callers validate via
// ParseBytes (see preset_test.go); the bytes are the mirror of the
// docs/spec/ canonical, kept in lockstep by the schema-sync gate.
func PresetBytes(preset Preset) ([]byte, error) {
	return PresetShapeBytes(preset, ShapeApp)
}

// PresetShapeBytes returns the canonical embedded bytes for a (preset,
// shape) pair. An empty shape resolves to ShapeApp. Returns an error
// naming the valid values for an unknown preset or an unknown shape.
func PresetShapeBytes(preset Preset, shape Shape) ([]byte, error) {
	if shape == "" {
		shape = ShapeApp
	}
	if shape != ShapeApp && shape != ShapeConfigOnly {
		return nil, fmt.Errorf("spec: unknown shape %q (want one of app, config-only)", shape)
	}
	path, ok := presetPaths[presetKey{preset, shape}]
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
