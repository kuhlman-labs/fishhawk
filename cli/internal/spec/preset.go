package spec

// Preset generation (ADR-048 / E29.1). The onboarding path — `fishhawk
// init` (E29.3) and the App-PR flow (E29.7) — turns a chosen autonomy
// tier plus a few structured deltas into a schema-valid
// `.fishhawk/workflows.yaml`.
//
// The three canonical presets (low/medium/high) live under docs/spec/
// and are mirrored into presets/ by scripts/sync-schemas. They are
// embedded here so the generator is standalone — no backend round-trip.
// Generate applies deltas via yaml.v3 node edits (preserving comments
// and ordering rather than round-tripping through the typed struct),
// then runs every output through the existing ValidateBytes gate: any
// delta that breaks schema validity fails closed instead of emitting an
// invalid document.

import (
	"bytes"
	"embed"
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"
)

//go:embed presets/workflow-preset-low.yaml presets/workflow-preset-medium.yaml presets/workflow-preset-high.yaml
var presetFS embed.FS

// Preset names an autonomy tier in the preset library. Each maps to a
// canonical workflow-v1 document under docs/spec/, mirrored into
// presets/ (see docs/spec/workflow-preset.md).
type Preset string

const (
	// PresetLow is human-led: no operator_agent block, every judgment
	// point pages the human.
	PresetLow Preset = "low"
	// PresetMedium is the default: operator agent may approve/route
	// fixup/retry under their named conditions; waive and merge stay
	// human.
	PresetMedium Preset = "medium"
	// PresetHigh adds may_waive: solo_low and may_merge:
	// gates_resolved_ci_green on top of medium.
	PresetHigh Preset = "high"
)

// presetPaths maps each preset to its embedded canonical bytes path.
var presetPaths = map[Preset]string{
	PresetLow:    "presets/workflow-preset-low.yaml",
	PresetMedium: "presets/workflow-preset-medium.yaml",
	PresetHigh:   "presets/workflow-preset-high.yaml",
}

// codexReviewerProvider is the provider name of the second (Codex) agent
// reviewer; the SingleReviewer delta drops the entry with this provider.
const codexReviewerProvider = "codex"

// Deltas are the structured overrides applied to a preset by Generate.
// The zero value applies nothing — Generate returns the preset
// unchanged (bar a re-encode). Every delta is validated through
// ValidateBytes, so a delta that breaks schema validity fails closed.
type Deltas struct {
	// BudgetLimitUSD, when non-nil, overrides the feature_change
	// workflow's weekly advisory cost ceiling (budgets[0].limit_usd).
	BudgetLimitUSD *int
	// SingleReviewer, when true, drops the Codex (gpt-5.5) agent
	// reviewer from every stage's reviewers.agents, leaving Claude
	// alone.
	SingleReviewer bool
	// HumanGates, when non-nil, selects which stages keep their human
	// approval gate: a stage whose id is listed keeps its gates; any
	// stage with a gates block whose id is NOT listed has that block
	// removed. Nil leaves every gate as authored (the common case).
	// An empty non-nil slice removes every human gate.
	HumanGates []string
}

// PresetBytes returns the canonical embedded bytes for a preset without
// applying any deltas. Used by callers (and the drift-proof tests) that
// want the untouched document. Returns an error for an unknown preset.
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

// Generate loads the named preset, applies the deltas via yaml.v3 node
// edits, re-encodes, and validates the result through ValidateBytes
// before returning. It returns an error for an unknown preset, and a
// *ValidationError (from ValidateBytes) if a delta produced a document
// that no longer satisfies the workflow-v1 schema.
func Generate(preset Preset, deltas Deltas) ([]byte, error) {
	data, err := PresetBytes(preset)
	if err != nil {
		return nil, err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("spec: parse embedded preset %q: %w", preset, err)
	}

	root := documentRoot(&doc)
	if root == nil {
		return nil, fmt.Errorf("spec: embedded preset %q is not a mapping document", preset)
	}
	wf := featureChangeWorkflow(root)
	if wf == nil {
		return nil, fmt.Errorf("spec: embedded preset %q has no feature_change workflow", preset)
	}

	if deltas.BudgetLimitUSD != nil {
		if err := applyBudgetLimit(wf, *deltas.BudgetLimitUSD); err != nil {
			return nil, err
		}
	}
	if deltas.SingleReviewer {
		applySingleReviewer(wf)
	}
	if deltas.HumanGates != nil {
		applyHumanGates(wf, deltas.HumanGates)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2) // match the canonical presets' 2-space style
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("spec: re-encode preset %q: %w", preset, err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("spec: close encoder for preset %q: %w", preset, err)
	}

	out := buf.Bytes()
	// Fail closed: a delta that broke schema validity never returns.
	if err := ValidateBytes(out); err != nil {
		return nil, err
	}
	return out, nil
}

// documentRoot returns the top-level mapping node of a decoded document,
// or nil if the document is not a mapping.
func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		return mappingNode(doc.Content[0])
	}
	return mappingNode(doc)
}

func mappingNode(n *yaml.Node) *yaml.Node {
	if n != nil && n.Kind == yaml.MappingNode {
		return n
	}
	return nil
}

// featureChangeWorkflow navigates root -> workflows -> feature_change and
// returns its mapping node, or nil if the path is absent.
func featureChangeWorkflow(root *yaml.Node) *yaml.Node {
	workflows := mappingNode(mapValue(root, "workflows"))
	if workflows == nil {
		return nil
	}
	return mappingNode(mapValue(workflows, "feature_change"))
}

// mapValue returns the value node for key in a mapping node, or nil if
// absent. A mapping node's Content is [key0, val0, key1, val1, ...].
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// removeMapKey removes the key/value pair for key from a mapping node.
// No-op if the key is absent.
func removeMapKey(m *yaml.Node, key string) {
	if m == nil || m.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// applyBudgetLimit overrides budgets[0].limit_usd on the workflow. It
// returns an error if the budgets block or its limit_usd scalar is
// absent (a malformed preset), so the miss fails loud rather than
// silently no-opping.
func applyBudgetLimit(wf *yaml.Node, limit int) error {
	budgets := mapValue(wf, "budgets")
	if budgets == nil || budgets.Kind != yaml.SequenceNode || len(budgets.Content) == 0 {
		return fmt.Errorf("spec: preset has no budgets block to override")
	}
	entry := mappingNode(budgets.Content[0])
	limitNode := mapValue(entry, "limit_usd")
	if limitNode == nil || limitNode.Kind != yaml.ScalarNode {
		return fmt.Errorf("spec: preset budgets[0] has no limit_usd scalar to override")
	}
	limitNode.Tag = "!!int"
	limitNode.Value = strconv.Itoa(limit)
	return nil
}

// applySingleReviewer drops the Codex agent reviewer from every stage's
// reviewers.agents sequence, leaving the Claude reviewer alone.
func applySingleReviewer(wf *yaml.Node) {
	for _, stage := range stageNodes(wf) {
		reviewers := mappingNode(mapValue(stage, "reviewers"))
		if reviewers == nil {
			continue
		}
		agents := mapValue(reviewers, "agents")
		if agents == nil || agents.Kind != yaml.SequenceNode {
			continue
		}
		kept := agents.Content[:0:0]
		for _, agent := range agents.Content {
			provider := mapValue(mappingNode(agent), "provider")
			if provider != nil && provider.Value == codexReviewerProvider {
				continue
			}
			kept = append(kept, agent)
		}
		agents.Content = kept
	}
}

// applyHumanGates removes the gates block from any stage whose id is not
// in keep. A stage with no gates block is untouched.
func applyHumanGates(wf *yaml.Node, keep []string) {
	keepSet := make(map[string]bool, len(keep))
	for _, id := range keep {
		keepSet[id] = true
	}
	for _, stage := range stageNodes(wf) {
		if mapValue(stage, "gates") == nil {
			continue
		}
		idNode := mapValue(stage, "id")
		if idNode != nil && keepSet[idNode.Value] {
			continue
		}
		removeMapKey(stage, "gates")
	}
}

// stageNodes returns the mapping nodes of the workflow's stages
// sequence, skipping any non-mapping entries.
func stageNodes(wf *yaml.Node) []*yaml.Node {
	stages := mapValue(wf, "stages")
	if stages == nil || stages.Kind != yaml.SequenceNode {
		return nil
	}
	out := make([]*yaml.Node, 0, len(stages.Content))
	for _, s := range stages.Content {
		if m := mappingNode(s); m != nil {
			out = append(out, m)
		}
	}
	return out
}
