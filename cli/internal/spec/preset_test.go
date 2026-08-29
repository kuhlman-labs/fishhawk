package spec

import (
	"regexp"
	"strings"
	"testing"

	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"
)

// allPresets is the closed set the generator ships.
var allPresets = []Preset{PresetLow, PresetMedium, PresetHigh}

// TestPresetsValidateByConstruction is the schema-valid-by-construction
// gate: every embedded preset must pass ValidateBytes (removed-form
// sweep + same-document reuse resolution + the workflow-v2 schema) so a
// preset that drifted out of schema fails the build here, not at a
// user's `fishhawk init`.
func TestPresetsValidateByConstruction(t *testing.T) {
	for _, p := range allPresets {
		p := p
		t.Run(string(p), func(t *testing.T) {
			data, err := PresetBytes(p)
			if err != nil {
				t.Fatalf("PresetBytes(%q): %v", p, err)
			}
			if err := ValidateBytes(data); err != nil {
				t.Fatalf("preset %q does not validate: %v", p, err)
			}
		})
	}
}

// TestGeneratedPresetsAreGeneric is the done-means (behavioral, per
// #1639) assertion: the SHIPPED bytes of every generated preset must
// carry NONE of the fishhawk-repo-specific substrings the ticket names
// (@kuhlman-labs, scripts/dev, scripts/test, localhost:8090) and must
// still pass ValidateBytes. A comment-only / no-op touch of a preset
// that left the maintainer handle or the private test entrypoint in
// place would fail here even though the scope path was touched.
func TestGeneratedPresetsAreGeneric(t *testing.T) {
	forbidden := []string{"@kuhlman-labs", "scripts/dev", "scripts/test", "localhost:8090"}
	for _, p := range allPresets {
		p := p
		t.Run(string(p), func(t *testing.T) {
			data, err := Generate(p, Deltas{})
			if err != nil {
				t.Fatalf("Generate(%q): %v", p, err)
			}
			s := string(data)
			for _, sub := range forbidden {
				if strings.Contains(s, sub) {
					t.Errorf("generated preset %q still contains fishhawk-specific substring %q:\n%s", p, sub, s)
				}
			}
			if err := ValidateBytes(data); err != nil {
				t.Fatalf("generic preset %q does not validate: %v", p, err)
			}
		})
	}
}

// TestPresetsHaveNoHandlePlaceholder is the E39.2 / #1707 done-means at
// the string level (the cli spec package has no typed Gate struct): the
// SHIPPED bytes of every preset must carry NO `@your-github-handle`
// placeholder and NO top-level `roles:` map — the approval gates use the
// forge-neutral `approvals` block, which workflow-v2 makes the SOLE
// approval predicate. A comment-only / no-op preset touch that left the
// handle or the roles map in place fails here even though the scope path
// was touched.
func TestPresetsHaveNoHandlePlaceholder(t *testing.T) {
	for _, p := range allPresets {
		p := p
		t.Run(string(p), func(t *testing.T) {
			data, err := PresetBytes(p)
			if err != nil {
				t.Fatalf("PresetBytes(%q): %v", p, err)
			}
			text := string(data)
			if strings.Contains(text, "@your-github-handle") {
				t.Errorf("preset %q still carries the @your-github-handle placeholder:\n%s", p, text)
			}
			for _, line := range strings.Split(text, "\n") {
				if strings.HasPrefix(line, "roles:") {
					t.Errorf("preset %q still declares a top-level roles map:\n%s", p, text)
				}
			}
			// The forge-neutral predicate must be present.
			if !strings.Contains(text, "approvals:") {
				t.Errorf("preset %q does not use the forge-neutral approvals block:\n%s", p, text)
			}
		})
	}
}

// --- workflow-v2 preset shape (E52.8 / #2220) --------------------------------

// TestPresetsAreWorkflowV2 is the done-means on the SHIPPED bytes: each
// preset declares `version: "2"`, carries the tier shorthand `autonomy:
// <its own tier>`, and declares NO `operator_agent` block — the v0/v1
// key workflow-v2 removed. A comment-only preset edit fails here.
func TestPresetsAreWorkflowV2(t *testing.T) {
	for _, p := range allPresets {
		p := p
		t.Run(string(p), func(t *testing.T) {
			text := string(presetText(t, p))
			if !strings.Contains(text, "version: \"2\"\n") {
				t.Errorf("preset %q does not declare version: \"2\":\n%s", p, text)
			}
			if strings.Contains(text, "operator_agent:") {
				t.Errorf("preset %q still declares an operator_agent block (removed at workflow-v2):\n%s", p, text)
			}
			if want := "\n    autonomy: " + string(p) + "\n"; !strings.Contains(text, want) {
				t.Errorf("preset %q does not declare %q:\n%s", p, strings.TrimSpace(want), text)
			}
			// The v0/v1 spellings v2 renamed must be gone too.
			for _, legacy := range []string{"drive:", "max_runtime_minutes:", "must_page_human:"} {
				if strings.Contains(text, legacy) {
					t.Errorf("preset %q still uses the v0/v1 spelling %q:\n%s", p, legacy, text)
				}
			}
		})
	}
}

// TestPresetsDeclareReviewAuthority is criterion 6's CLI-mirror text half
// (E53.2 / #2225): every preset spells `authority: advisory` on its plan and
// implement reviewers blocks (two occurrences), so the embedded CLI mirror and
// the backend presets stay in step. The parsed-value-vs-ResolveAuthority half
// lives on the backend (planreview imports spec; the CLI spec package has no
// typed reviewers struct).
func TestPresetsDeclareReviewAuthority(t *testing.T) {
	for _, p := range allPresets {
		p := p
		t.Run(string(p), func(t *testing.T) {
			text := string(presetText(t, p))
			if got := strings.Count(text, "authority: advisory"); got != 2 {
				t.Errorf("preset %q declares `authority: advisory` %d times, want 2 (plan + implement):\n%s", p, got, text)
			}
		})
	}
}

// TestPresetsDeclarePerStageExecutors asserts the executor stays declared
// ON EACH STAGE and is NOT hoisted into a file- or workflow-level
// `defaults` block. Both Go validators resolve same-document reuse before
// schema validation, so a hoisted executor validates — but a shipped
// governance document is also read by tooling that does no reuse
// resolution (a bare check-jsonschema run), and such a reader must see
// an executor on every stage (`fishhawk doctor`'s execution-path check
// now resolves reuse first, #2340, so it is no longer such a reader).
// So: every agent
// stage names its own `agent: claude-code`, the implement stage keeps its
// `verify:` sub-block, and the review stage declares `human: true`.
func TestPresetsDeclarePerStageExecutors(t *testing.T) {
	for _, p := range allPresets {
		p := p
		t.Run(string(p), func(t *testing.T) {
			doc := decodePresetDoc(t, presetText(t, p))
			if doc.Defaults != nil && doc.Defaults.Executor != nil {
				t.Errorf("preset %q hoisted the executor into the file-level defaults: %v", p, doc.Defaults.Executor)
			}
			if wf := doc.Workflows["feature_change"]; wf.Defaults != nil && wf.Defaults.Executor != nil {
				t.Errorf("preset %q hoisted the executor into the workflow-level defaults: %v", p, wf.Defaults.Executor)
			}
			byID := stagesByID(t, doc)
			for _, id := range []string{"plan", "implement"} {
				ex := byID[id].Executor
				if ex == nil {
					t.Fatalf("preset %q stage %q declares no executor block", p, id)
				}
				if got := ex["agent"]; got != "claude-code" {
					t.Errorf("preset %q stage %q executor.agent = %v, want claude-code", p, id, got)
				}
			}
			if _, ok := byID["implement"].Executor["verify"]; !ok {
				t.Errorf("preset %q implement stage lost its verify block: %v", p, byID["implement"].Executor)
			}
			if got := byID["review"].Executor["human"]; got != true {
				t.Errorf("preset %q review stage executor = %v, want {human: true}", p, byID["review"].Executor)
			}
		})
	}
}

// TestPresetsDoNotHoistReviewers is the not-hoisted assertion. `reviewers`
// is taken WHOLE from exactly one rung and determines review AUTHORITY
// (ADR-027): a file- or workflow-level reviewers default would be
// inherited by the review stage, which deliberately declares none, and
// would silently give it agent reviewers its author never wrote. A future
// edit that hoists the block fails here.
func TestPresetsDoNotHoistReviewers(t *testing.T) {
	for _, p := range allPresets {
		p := p
		t.Run(string(p), func(t *testing.T) {
			doc := decodePresetDoc(t, presetText(t, p))
			if doc.Defaults != nil && doc.Defaults.Reviewers != nil {
				t.Errorf("preset %q hoisted reviewers into the file-level defaults: %v", p, doc.Defaults.Reviewers)
			}
			wf := doc.Workflows["feature_change"]
			if wf.Defaults != nil && wf.Defaults.Reviewers != nil {
				t.Errorf("preset %q hoisted reviewers into the workflow-level defaults: %v", p, wf.Defaults.Reviewers)
			}
			byID := stagesByID(t, doc)
			for _, id := range []string{"plan", "implement"} {
				if byID[id].Reviewers == nil {
					t.Errorf("preset %q stage %q declares no reviewers block of its own", p, id)
				}
			}
			if byID["review"].Reviewers != nil {
				t.Errorf("preset %q review stage declares reviewers %v — it must declare none", p, byID["review"].Reviewers)
			}
		})
	}
}

// TestPresetsBudgetsUseV2Units pins the per-stage budget reshape: the
// runtime cap is a Go duration (`max_runtime`), and each agent stage
// carries the calibrated USD ceiling.
func TestPresetsBudgetsUseV2Units(t *testing.T) {
	want := map[string]struct {
		runtime string
		usd     float64
	}{
		"plan":      {"15m", 4},
		"implement": {"30m", 12},
	}
	for _, p := range allPresets {
		p := p
		t.Run(string(p), func(t *testing.T) {
			byID := stagesByID(t, decodePresetDoc(t, presetText(t, p)))
			for id, w := range want {
				b := byID[id].Budget
				if b == nil {
					t.Fatalf("preset %q stage %q has no budget block", p, id)
				}
				if b.MaxRuntime != w.runtime {
					t.Errorf("preset %q stage %q max_runtime = %q, want %q", p, id, b.MaxRuntime, w.runtime)
				}
				if b.MaxRuntimeMinutes != 0 {
					t.Errorf("preset %q stage %q still declares max_runtime_minutes = %d", p, id, b.MaxRuntimeMinutes)
				}
				if b.LimitUSD != w.usd {
					t.Errorf("preset %q stage %q limit_usd = %v, want %v", p, id, b.LimitUSD, w.usd)
				}
			}
		})
	}
}

// TestPresetsConstraintsAreObjects pins the v2 constraints reshape: the
// v0/v1 LIST of single-kind maps folded into one object keyed by kind.
func TestPresetsConstraintsAreObjects(t *testing.T) {
	for _, p := range allPresets {
		p := p
		t.Run(string(p), func(t *testing.T) {
			raw := stagesByID(t, decodePresetDoc(t, presetText(t, p)))["implement"].Constraints
			m, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("preset %q implement constraints = %T, want the v2 object form", p, raw)
			}
			for _, kind := range []string{"max_files_changed", "forbidden_paths", "required_outcomes"} {
				if _, ok := m[kind]; !ok {
					t.Errorf("preset %q implement constraints lost the %q kind: %v", p, kind, m)
				}
			}
		})
	}
}

// issueRefRe matches an issue reference (#1234), an ADR reference
// (ADR-067) or an epic/child reference (E52.8) — the provenance markers
// AC5 moves out of the preset comments and into
// docs/spec/workflow-preset.md.
var issueRefRe = regexp.MustCompile(`#[0-9]+|ADR-[0-9]+|\bE[0-9]+\.[0-9]+`)

// TestPresetCommentsArePolicyOnly is AC5: a preset comment explains what
// the operator is choosing, never which issue shipped it. A reader
// scaffolding a fresh repo has no access to this tracker.
func TestPresetCommentsArePolicyOnly(t *testing.T) {
	for _, p := range allPresets {
		p := p
		t.Run(string(p), func(t *testing.T) {
			for i, line := range strings.Split(string(presetText(t, p)), "\n") {
				if !strings.HasPrefix(strings.TrimSpace(line), "#") {
					continue
				}
				if m := issueRefRe.FindString(line); m != "" {
					t.Errorf("preset %q line %d carries the provenance reference %q: %s", p, i+1, m, line)
				}
			}
		})
	}
}

// --- the lockstep invariant --------------------------------------------------

// TestPresetsAreLockstepBaseAndTierDelta is the machine-enforced
// statement of the base-plus-deltas invariant. Cross-FILE inclusion is
// deliberately out of ADR scope, so each preset must remain a standalone
// document `fishhawk init` writes whole; this test — not a generator — is
// what keeps the three in lockstep.
//
// It strips ONLY the leading header comment block (the one deliberately
// tier-specific prose block) and the single `autonomy:` line, then
// compares the remainders BYTE FOR BYTE. Every other comment
// participates: stripping all comments would leave the test unable to
// detect exactly the tier-specific drift it exists to prevent.
//
// The vacuity guard is that the line it stripped from each preset must
// have been that preset's OWN autonomy line, so the test cannot pass on
// three files that share no autonomy key at all.
func TestPresetsAreLockstepBaseAndTierDelta(t *testing.T) {
	normalized := make(map[Preset]string, len(allPresets))
	for _, p := range allPresets {
		body, headerLines := stripLeadingCommentBlock(string(presetText(t, p)))
		if headerLines == 0 {
			t.Fatalf("preset %q has no leading header comment block to strip", p)
		}
		rest, tier, n := stripAutonomyLine(body)
		if n != 1 {
			t.Fatalf("preset %q declares %d autonomy lines, want exactly 1", p, n)
		}
		if tier != string(p) {
			t.Fatalf("preset %q stripped `autonomy: %s`, want its own tier — the comparison below would be vacuous", p, tier)
		}
		normalized[p] = rest
	}
	base := normalized[PresetMedium]
	for _, p := range []Preset{PresetLow, PresetHigh} {
		if normalized[p] != base {
			t.Errorf("preset %q is not the medium base plus its one autonomy line.\n--- %s ---\n%s\n--- medium ---\n%s", p, p, normalized[p], base)
		}
	}
	// Load-bearing guard: the shared base must be substantial, so an
	// accidental over-strip cannot make three empty strings compare equal.
	if len(base) < 1000 {
		t.Fatalf("normalized base is only %d bytes — the lockstep comparison is not load-bearing", len(base))
	}
}

// stripLeadingCommentBlock removes the contiguous run of comment and
// blank lines at the top of a document, returning the remainder and how
// many lines it dropped.
func stripLeadingCommentBlock(text string) (string, int) {
	lines := strings.Split(text, "\n")
	i := 0
	for i < len(lines) {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			break
		}
		i++
	}
	return strings.Join(lines[i:], "\n"), i
}

// stripAutonomyLine removes every `autonomy: <tier>` line, returning the
// remainder, the last tier value seen, and how many lines matched.
func stripAutonomyLine(text string) (string, string, int) {
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	tier := ""
	n := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "autonomy:") {
			tier = strings.TrimSpace(strings.TrimPrefix(trimmed, "autonomy:"))
			n++
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n"), tier, n
}

// --- decoding helpers ---------------------------------------------------------

// presetText returns a preset's shipped bytes, failing the test on a
// read error.
func presetText(t *testing.T, p Preset) []byte {
	t.Helper()
	data, err := PresetBytes(p)
	if err != nil {
		t.Fatalf("PresetBytes(%q): %v", p, err)
	}
	return data
}

// presetDoc is the shape these tests read. The cli spec package performs
// no typed decode of its own (validation is schema-only), so the tests
// decode exactly the keys they assert on.
type presetDoc struct {
	Version   string                    `yaml:"version"`
	Defaults  *presetDefaults           `yaml:"defaults"`
	Workflows map[string]presetWorkflow `yaml:"workflows"`
}

type presetDefaults struct {
	Executor  map[string]any `yaml:"executor"`
	Reviewers map[string]any `yaml:"reviewers"`
}

type presetWorkflow struct {
	Autonomy string          `yaml:"autonomy"`
	Defaults *presetDefaults `yaml:"defaults"`
	Stages   []presetStage   `yaml:"stages"`
}

type presetStage struct {
	ID          string         `yaml:"id"`
	Executor    map[string]any `yaml:"executor"`
	Reviewers   map[string]any `yaml:"reviewers"`
	Constraints any            `yaml:"constraints"`
	Budget      *presetBudget  `yaml:"budget"`
}

type presetBudget struct {
	MaxTokens         int     `yaml:"max_tokens"`
	MaxRuntime        string  `yaml:"max_runtime"`
	MaxRuntimeMinutes int     `yaml:"max_runtime_minutes"`
	LimitUSD          float64 `yaml:"limit_usd"`
}

func decodePresetDoc(t *testing.T, data []byte) presetDoc {
	t.Helper()
	var doc presetDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal preset: %v", err)
	}
	return doc
}

func stagesByID(t *testing.T, doc presetDoc) map[string]presetStage {
	t.Helper()
	wf, ok := doc.Workflows["feature_change"]
	if !ok {
		t.Fatal("document has no feature_change workflow")
	}
	out := make(map[string]presetStage, len(wf.Stages))
	for _, s := range wf.Stages {
		out[s.ID] = s
	}
	return out
}

// --- the generator deltas -----------------------------------------------------

// TestGenerateAutonomyPerPreset is the done-means (behavioral, per
// #1169) assertion on GENERATED bytes: the tier shorthand survives the
// node-edit round trip, and no generated preset resurrects the removed
// operator_agent block.
func TestGenerateAutonomyPerPreset(t *testing.T) {
	for _, p := range allPresets {
		p := p
		t.Run(string(p), func(t *testing.T) {
			data, err := Generate(p, Deltas{})
			if err != nil {
				t.Fatalf("Generate(%q): %v", p, err)
			}
			doc := decodePresetDoc(t, data)
			if doc.Version != "2" {
				t.Errorf("generated preset %q version = %q, want \"2\"", p, doc.Version)
			}
			if got := doc.Workflows["feature_change"].Autonomy; got != string(p) {
				t.Errorf("generated preset %q autonomy = %q, want %q", p, got, p)
			}
			if strings.Contains(string(data), "operator_agent:") {
				t.Errorf("generated preset %q resurrected the operator_agent block:\n%s", p, data)
			}
		})
	}
}

// TestGenerateBudgetCeilingDelta covers the budget-ceiling delta branch:
// the emitted limit_usd changes AND the output still validates. The
// delta targets the WORKFLOW-level budgets[0].limit_usd, which v2 leaves
// unchanged — the new per-stage limit_usd is a different field.
func TestGenerateBudgetCeilingDelta(t *testing.T) {
	limit := 250
	data, err := Generate(PresetMedium, Deltas{BudgetLimitUSD: &limit})
	if err != nil {
		t.Fatalf("Generate with budget delta: %v", err)
	}
	if err := ValidateBytes(data); err != nil {
		t.Fatalf("budget-delta output does not validate: %v", err)
	}
	got := budgetLimitOf(t, data)
	if got != 250 {
		t.Errorf("limit_usd = %v, want 250", got)
	}
	// And confirm the un-delta'd default really was different, so the
	// assertion above is load-bearing.
	base, err := Generate(PresetMedium, Deltas{})
	if err != nil {
		t.Fatalf("Generate(medium): %v", err)
	}
	if def := budgetLimitOf(t, base); def == 250 {
		t.Fatalf("default limit_usd already 250 — budget delta assertion is not load-bearing")
	}
	// The per-stage ceilings are a DIFFERENT field and must be untouched.
	if stage := stagesByID(t, decodePresetDoc(t, data))["implement"].Budget; stage == nil || stage.LimitUSD != 12 {
		t.Errorf("implement stage limit_usd = %+v, want the untouched per-stage ceiling 12", stage)
	}
}

func budgetLimitOf(t *testing.T, data []byte) float64 {
	t.Helper()
	var doc struct {
		Workflows map[string]struct {
			Budgets []struct {
				LimitUSD float64 `yaml:"limit_usd"`
			} `yaml:"budgets"`
		} `yaml:"workflows"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	fc := doc.Workflows["feature_change"]
	if len(fc.Budgets) == 0 {
		t.Fatal("no budgets block in generated bytes")
	}
	return fc.Budgets[0].LimitUSD
}

// TestGenerateSingleReviewerDelta covers the single-vs-dual-reviewers
// delta branch: the Codex reviewer is dropped from every stage AND the
// output still validates. It also re-proves the delta still finds its
// target now that reviewers stay declared PER STAGE rather than hoisted.
func TestGenerateSingleReviewerDelta(t *testing.T) {
	data, err := Generate(PresetMedium, Deltas{SingleReviewer: true})
	if err != nil {
		t.Fatalf("Generate with single-reviewer delta: %v", err)
	}
	if err := ValidateBytes(data); err != nil {
		t.Fatalf("single-reviewer output does not validate: %v", err)
	}
	if strings.Contains(string(data), "codex") || strings.Contains(string(data), "gpt-5.5") {
		t.Errorf("single-reviewer output still names the codex reviewer:\n%s", data)
	}
	// The Claude reviewer must survive.
	if !strings.Contains(string(data), "claudecode") {
		t.Errorf("single-reviewer output dropped the claude reviewer too:\n%s", data)
	}
	// Structural check: every stage's reviewers.agents has exactly one entry.
	counts := reviewerCountsByStage(t, data)
	if len(counts) == 0 {
		t.Fatal("no stage declares reviewers — the delta assertion is not load-bearing")
	}
	for stageID, agents := range counts {
		if agents != 1 {
			t.Errorf("stage %q has %d reviewer agents, want 1", stageID, agents)
		}
	}
}

func reviewerCountsByStage(t *testing.T, data []byte) map[string]int {
	t.Helper()
	var doc struct {
		Workflows map[string]struct {
			Stages []struct {
				ID        string `yaml:"id"`
				Reviewers *struct {
					Agents []any `yaml:"agents"`
				} `yaml:"reviewers"`
			} `yaml:"stages"`
		} `yaml:"workflows"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := map[string]int{}
	for _, s := range doc.Workflows["feature_change"].Stages {
		if s.Reviewers != nil {
			out[s.ID] = len(s.Reviewers.Agents)
		}
	}
	return out
}

// TestGenerateHumanGatesDelta covers the human-gates delta branch:
// naming only the plan stage keeps its gate and removes the review
// stage's gate AND the output still validates.
func TestGenerateHumanGatesDelta(t *testing.T) {
	data, err := Generate(PresetMedium, Deltas{HumanGates: []string{"plan"}})
	if err != nil {
		t.Fatalf("Generate with human-gates delta: %v", err)
	}
	if err := ValidateBytes(data); err != nil {
		t.Fatalf("human-gates output does not validate: %v", err)
	}
	gates := gatesByStage(t, data)
	if !gates["plan"] {
		t.Errorf("plan stage should keep its human gate, got none")
	}
	if gates["review"] {
		t.Errorf("review stage should have its gate removed, still present")
	}
}

func gatesByStage(t *testing.T, data []byte) map[string]bool {
	t.Helper()
	var doc struct {
		Workflows map[string]struct {
			Stages []struct {
				ID    string `yaml:"id"`
				Gates []any  `yaml:"gates"`
			} `yaml:"stages"`
		} `yaml:"workflows"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := map[string]bool{}
	for _, s := range doc.Workflows["feature_change"].Stages {
		out[s.ID] = len(s.Gates) > 0
	}
	return out
}

// TestGenerateUnknownPreset covers the unknown-preset error branch.
func TestGenerateUnknownPreset(t *testing.T) {
	if _, err := Generate(Preset("bogus"), Deltas{}); err == nil {
		t.Fatal("Generate with unknown preset must error")
	}
	if _, err := PresetBytes(Preset("bogus")); err == nil {
		t.Fatal("PresetBytes with unknown preset must error")
	}
}

// TestGenerateMediumRoundTrip is the round-trip check: generate medium,
// parse it back, and assert its autonomy tier + reviewers match the
// current feature_change semantics — against hardcoded expected values,
// not a cross-module read of .fishhawk/workflows.yaml.
func TestGenerateMediumRoundTrip(t *testing.T) {
	data, err := Generate(PresetMedium, Deltas{})
	if err != nil {
		t.Fatalf("Generate(medium): %v", err)
	}
	if got := decodePresetDoc(t, data).Workflows["feature_change"].Autonomy; got != "medium" {
		t.Errorf("medium autonomy tier drifted from feature_change: %q", got)
	}
	// Both stages that carry reviewers must run the Claude+Codex pair.
	counts := reviewerCountsByStage(t, data)
	for _, stageID := range []string{"plan", "implement"} {
		if counts[stageID] != 2 {
			t.Errorf("stage %q reviewer agents = %d, want 2 (claude+codex)", stageID, counts[stageID])
		}
	}
}

// --- control-surface starters (E53.6 capstone) -------------------------------

// uncommentStarters returns the preset text with the two COMMENTED
// control-surface starter blocks (the lines strictly between each
// `fishhawk:starter-begin`/`-end` sentinel) uncommented in place, so the
// shipped starter SHAPES can be run through the real validator. The
// sentinel lines themselves stay comments. This is what proves a shipped
// starter is not merely present but still parses and validates: comments
// are discarded at parse, so nothing else would catch a starter that
// drifted out of schema.
func uncommentStarters(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	in := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "# fishhawk:starter-begin"):
			in = true
			out = append(out, line)
		case strings.HasPrefix(trimmed, "# fishhawk:starter-end"):
			in = false
			out = append(out, line)
		case in:
			out = append(out, strings.Replace(line, "# ", "", 1))
		default:
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// TestShippedPresetsCarryControlSurfaceStarters is the done-means for the
// E53.6 capstone's preset half, one assertion per named failure mode:
// every shipped preset carries the two COMMENTED control-surface starter
// blocks (applies_to and escalations); the starters stay INERT (never a
// live key); and — per the operator's binding condition that a commented
// starter nothing validates is unacceptable — the starter shapes still
// parse and validate when uncommented, so a starter drifted out of schema
// fails the build here rather than the first operator who uncomments it.
// A comment-only / no-op touch of a preset path fails (a) or (b).
func TestShippedPresetsCarryControlSurfaceStarters(t *testing.T) {
	for _, p := range allPresets {
		p := p
		t.Run(string(p), func(t *testing.T) {
			text := string(presetText(t, p))

			// (a) the commented applies_to starter is present.
			if !strings.Contains(text, "# fishhawk:starter-begin applies_to") {
				t.Errorf("preset %q is missing the commented applies_to starter block:\n%s", p, text)
			}
			// (b) the commented escalations starter is present.
			if !strings.Contains(text, "# fishhawk:starter-begin escalations") {
				t.Errorf("preset %q is missing the commented escalations starter block:\n%s", p, text)
			}
			// (c) the starters stay INERT: no line, trimmed of leading
			// whitespace, begins with a LIVE applies_to:/escalations: key.
			// A future edit that uncomments one fails here rather than
			// silently shipping a routing/escalation policy into every
			// scaffolded repo.
			for i, line := range strings.Split(text, "\n") {
				tl := strings.TrimSpace(line)
				if strings.HasPrefix(tl, "applies_to:") || strings.HasPrefix(tl, "escalations:") {
					t.Errorf("preset %q line %d ships a LIVE control-surface key, want it commented: %s", p, i+1, line)
				}
			}
			// (d) the shipped preset validates, AND the starter shapes
			// validate once uncommented (CONDITION 2(a)): comments are
			// discarded at parse, so this is the only machine check of the
			// starter shape.
			if err := ValidateBytes([]byte(text)); err != nil {
				t.Fatalf("shipped preset %q does not validate: %v", p, err)
			}
			uc := uncommentStarters(text)
			if !strings.Contains(uc, "\n    applies_to:\n") || !strings.Contains(uc, "\n    escalations:\n") {
				t.Fatalf("preset %q uncomment did not surface both starter keys — the sentinels did not match:\n%s", p, uc)
			}
			if err := ValidateBytes([]byte(uc)); err != nil {
				t.Fatalf("preset %q starter shapes do not validate once uncommented: %v\n%s", p, err, uc)
			}
		})
	}
}

// TestGenerateBudgetDeltaOnMalformedPreset asserts applyBudgetLimit
// fails loud when the budgets block is missing. Constructed inline
// rather than via an embedded preset, since every shipped preset has a
// budgets block by construction.
func TestApplyBudgetLimitMissingBlock(t *testing.T) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("stages: []\n"), &doc); err != nil {
		t.Fatalf("setup unmarshal: %v", err)
	}
	wf := documentRoot(&doc)
	if wf == nil {
		t.Fatal("setup: expected a mapping root")
	}
	if err := applyBudgetLimit(wf, 100); err == nil {
		t.Fatal("applyBudgetLimit must error when budgets block is absent")
	}
}

// freshWorktreeCaveatSentence is the caveat every shipped preset must carry
// immediately above its verify `command:`. It is wrapped across two comment
// lines in the YAML, so assertions normalize comment markers and whitespace
// before matching (see normalizeComments).
const freshWorktreeCaveatSentence = "This runs in a fresh worktree — gitignored build artifacts " +
	"and downloaded dependencies will not be present."

// normalizeComments strips the leading `#` marker from every line and
// collapses all runs of whitespace to a single space, so a sentence wrapped
// across consecutive comment lines matches as one string.
func normalizeComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		trimmed = strings.TrimPrefix(trimmed, "#")
		b.WriteString(strings.TrimSpace(trimmed))
		b.WriteString(" ")
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// TestGeneratedPresetsCarryFreshWorktreeCaveat is the done-means for the
// E48.58 / #2485 preset change (a comment-only edit compiles to nothing, so
// a shipped-bytes assertion is the only thing that can enforce it): the
// SHIPPED bytes of every generated preset must explain that the verify
// command runs in a fresh worktree where gitignored artifacts are absent,
// and must still pass ValidateBytes. A no-op touch of a preset path that
// satisfied the scope-completeness gate fails here.
func TestGeneratedPresetsCarryFreshWorktreeCaveat(t *testing.T) {
	for _, p := range allPresets {
		p := p
		t.Run(string(p), func(t *testing.T) {
			data, err := Generate(p, Deltas{})
			if err != nil {
				t.Fatalf("Generate(%q): %v", p, err)
			}
			if !strings.Contains(normalizeComments(string(data)), freshWorktreeCaveatSentence) {
				t.Errorf("generated preset %q does not carry the fresh-worktree caveat %q:\n%s",
					p, freshWorktreeCaveatSentence, data)
			}
			if err := ValidateBytes(data); err != nil {
				t.Fatalf("preset %q with the caveat does not validate: %v", p, err)
			}
		})
	}
}

// --- the forge-neutral self-modification guard --------------------------------

// forbiddenPathsOfImplement returns the implement stage's forbidden_paths
// list from a shipped preset. This package validates a raw `any` tree
// rather than a typed Spec, so it walks the decoded document with the
// same helpers TestPresetsConstraintsAreObjects uses. It fails LOUDLY —
// not vacuously — when the constraints object, the forbidden_paths kind,
// or its string-list shape is absent, so a preset that dropped the guard
// cannot silently satisfy an emptiness-tolerant assertion.
func forbiddenPathsOfImplement(t *testing.T, p Preset) []string {
	t.Helper()
	raw := stagesByID(t, decodePresetDoc(t, presetText(t, p)))["implement"].Constraints
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("preset %q implement constraints = %T, want the v2 object form", p, raw)
	}
	v, ok := m["forbidden_paths"]
	if !ok {
		t.Fatalf("preset %q implement constraints declare no forbidden_paths kind: %v", p, m)
	}
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("preset %q forbidden_paths = %#v, want a non-empty list", p, v)
	}
	out := make([]string, 0, len(list))
	for i, e := range list {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("preset %q forbidden_paths[%d] = %#v, want a string", p, i, e)
		}
		out = append(out, s)
	}
	return out
}

// TestPresetsForbidBothForgeCIEntryPoints mirrors the backend done-means
// assertion on this embed side: the shipped implement stage may not
// rewrite its own executor configuration on EITHER forge. Holding BOTH
// mirror sets to the contract is what makes a divergent mirror fail on
// the side that drifted — a contract enforced on one side only is how a
// mirror survives having diverged.
func TestPresetsForbidBothForgeCIEntryPoints(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want string
	}{
		{"github entry point", ".github/workflows/**"},
		{"gitlab entry point", ".gitlab-ci.yml"},
		{"gitlab include fragments", ".gitlab/ci/**"},
		{"gitlab agent ci_access config", ".gitlab/agents/**"},
		{"additive: governance survived", ".fishhawk/**"},
		{"additive: LICENSE survived", "LICENSE"},
		{"additive: NOTICE survived", "NOTICE"},
	} {
		tc := tc
		for _, p := range allPresets {
			p := p
			t.Run(string(p)+"/"+tc.mode, func(t *testing.T) {
				got := forbiddenPathsOfImplement(t, p)
				for _, entry := range got {
					if entry == tc.want {
						return
					}
				}
				t.Errorf("preset %q implement forbidden_paths is missing %q (%s): %v", p, tc.want, tc.mode, got)
			})
		}
	}
}

// forgeGuardMustBlock / forgeGuardMustNotBlock are the shipped-behavior
// table, identical to the backend mirror's — a contract enforced on one
// side only is how a mirror survives having diverged. The MUST-BLOCK rows
// now cover THREE of the four rationale families
// (docs/spec/workflow-preset.md): self-execution (the .github / .gitlab-ci
// / .gitlab/ci / .gitlab/agents rows), self-governance (.fishhawk) and
// legal (LICENSE / NOTICE).
//
// The two .gitlab/agents rows are the E45.34 (#2962) addition: GitLab reads
// the agent-for-Kubernetes config from .gitlab/agents/<name>/config.yaml,
// whose `ci_access` block grants the stage's own pipeline jobs cluster
// access, so it is a capability-granting file on the agent's execution
// surface. .gitlab/agents/deploy/config.yaml pins the exact documented path
// shape; .gitlab/agents/nested/deep/config.yaml pins that the `**` suffix
// reaches arbitrary depth (mirroring the .gitlab/ci/nested row).
//
// The two .gitlab/ template rows and the nested sub/project row are
// load-bearing, not decorative, and stay load-bearing AFTER the .gitlab/agents
// addition. The templates pin the ratified `.gitlab/ci/**`-not-`.gitlab/**`
// breadth decision — the agent rows added a NARROW second .gitlab/ prefix,
// not a widening to .gitlab/**, so the template rows must still stay
// UNBLOCKED or a future widening to .gitlab/** would slip in silently. The
// nested row pins the root-anchoring claim the guard's residual analysis
// rests on: a bare-filename pattern must NOT match a nested path of the same
// name, because GitLab reads the pipeline definition from the repository root
// and a nested file of that name executes nothing.
var (
	forgeGuardMustBlock = []string{
		".github/workflows/fishhawk.yml",
		".gitlab-ci.yml",
		".gitlab/ci/build.yml",
		".gitlab/ci/nested/fragment.yml",
		".gitlab/agents/deploy/config.yaml",
		".gitlab/agents/nested/deep/config.yaml",
		".fishhawk/workflows.yaml",
		"LICENSE",
		"NOTICE",
	}
	forgeGuardMustNotBlock = []string{
		"backend/internal/spec/preset.go",
		"docs/deploy/gitlab.md",
		"site/src/content/docs/concepts/constraint.md",
		".gitlab/issue_templates/bug.md",
		".gitlab/merge_request_templates/default.md",
		"sub/project/.gitlab-ci.yml",
	}
)

// matchesAnyForbidden evaluates a path against a forbidden_paths list
// with doublestar.PathMatch — the IDENTICAL call the runner's
// enforcement makes in runner/internal/constraint/constraint.go
// (checkForbidden), so the constraint VALUE declared in the spec
// document is checked through the SAME matcher production enforces
// with, rather than each side being assumed correct alone.
func matchesAnyForbidden(t *testing.T, pats []string, path string) (bool, string) {
	t.Helper()
	for _, pat := range pats {
		ok, err := doublestar.PathMatch(pat, path)
		if err != nil {
			t.Fatalf("doublestar.PathMatch(%q, %q): %v", pat, path, err)
		}
		if ok {
			return true, pat
		}
	}
	return false, ""
}

// TestPresetForbiddenPathsBlockBothForgeEntryPoints asserts the SHIPPED
// OBSERVABLE BEHAVIOR of the guard — which files the implement stage is
// actually refused — rather than the presence of an edited line. A
// comment-only or no-op preset touch that satisfied the scope
// completeness gate fails here.
func TestPresetForbiddenPathsBlockBothForgeEntryPoints(t *testing.T) {
	for _, p := range allPresets {
		p := p
		t.Run(string(p), func(t *testing.T) {
			pats := forbiddenPathsOfImplement(t, p)
			for _, path := range forgeGuardMustBlock {
				if ok, _ := matchesAnyForbidden(t, pats, path); !ok {
					t.Errorf("MUST-BLOCK %q is not matched by any forbidden_paths pattern of preset %q: %v", path, p, pats)
				}
			}
			for _, path := range forgeGuardMustNotBlock {
				if ok, pat := matchesAnyForbidden(t, pats, path); ok {
					t.Errorf("MUST-NOT-BLOCK %q is blocked by pattern %q of preset %q — the guard is over-broad", path, pat, p)
				}
			}
		})
	}
}
