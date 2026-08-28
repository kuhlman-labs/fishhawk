package spec

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

// --- tier-expansion corpus (E52.18 / #2341) ---------------------------------

// tierCorpusClass is one class entry from the shared tier-expansion fixture.
type tierCorpusClass struct {
	Mode string `json:"mode"`
	When string `json:"when,omitempty"`
}

// tierCorpusRow is one tier row from the shared fixture.
type tierCorpusRow struct {
	Tier        string                     `json:"tier"`
	Classes     map[string]tierCorpusClass `json:"classes"`
	PageHumanOn []string                   `json:"page_human_on"`
	Reason      string                     `json:"reason"`
}

type tierCorpusFile struct {
	Comment string          `json:"$comment"`
	Tiers   []tierCorpusRow `json:"tiers"`
}

// loadTierExpansionFixtures reads testdata/tier-expansion-fixtures.json and
// asserts the corpus carries exactly the three expected tiers (low, medium,
// high), each with exactly the five known classes. A truncated, emptied or
// mis-keyed corpus makes both corpus tests iterate zero rows and pass
// vacuously — the corpus would then guarantee nothing while looking green,
// which is the same blind-gate failure class this issue exists to close.
func loadTierExpansionFixtures(t *testing.T) []tierCorpusRow {
	t.Helper()
	b, err := os.ReadFile("testdata/tier-expansion-fixtures.json")
	if err != nil {
		t.Fatalf("loadTierExpansionFixtures: read: %v", err)
	}
	var f tierCorpusFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("loadTierExpansionFixtures: unmarshal: %v", err)
	}

	// Shape guard: corpus MUST carry exactly {low, medium, high}, each with
	// exactly {approve, fixup, waive, retry, merge}. Without this a zero-row
	// iteration produces a vacuous green — the corpus guarantees nothing.
	knownTiers := map[string]bool{"low": false, "medium": false, "high": false}
	knownClasses := []string{"approve", "fixup", "waive", "retry", "merge"}
	for _, row := range f.Tiers {
		if _, ok := knownTiers[row.Tier]; !ok {
			t.Fatalf("corpus carries unexpected tier %q; corpus must carry exactly low, medium, high", row.Tier)
		}
		if knownTiers[row.Tier] {
			t.Fatalf("corpus carries duplicate tier %q", row.Tier)
		}
		knownTiers[row.Tier] = true
		if len(row.Classes) != len(knownClasses) {
			t.Fatalf("tier %q: corpus carries %d classes, want 5 (approve, fixup, waive, retry, merge)", row.Tier, len(row.Classes))
		}
		for _, cls := range knownClasses {
			if _, ok := row.Classes[cls]; !ok {
				t.Fatalf("tier %q: corpus missing class %q", row.Tier, cls)
			}
		}
	}
	for tier, seen := range knownTiers {
		if !seen {
			t.Fatalf("corpus missing tier %q; corpus must carry exactly low, medium, high", tier)
		}
	}
	if len(f.Tiers) != 3 {
		t.Fatalf("corpus has %d tier rows, want exactly 3 (low, medium, high)", len(f.Tiers))
	}
	return f.Tiers
}

// TestTierExpansionCorpus_BackendMatches asserts both the SHIPPED behaviour
// (ResolveAutonomy on a real parsed v2 document) and the internal table
// (expandTier) against the shared docs/spec/tier-expansion-fixtures.json
// corpus. The corpus, frozenTierBlocks, tierPageList and expandTier are four
// independent surfaces: a solo edit to any one of them reddens at least one
// assertion here or in TestExpandTier_MatchesFrozenTierTable.
func TestTierExpansionCorpus_BackendMatches(t *testing.T) {
	rows := loadTierExpansionFixtures(t)
	for _, row := range rows {
		t.Run(row.Tier, func(t *testing.T) {
			tier := AutonomyTier(row.Tier)

			// (a) Shipped behaviour: parse a real v2 document with autonomy: <tier>
			// and assert the resolved matrix.
			wf := Workflow{Autonomy: tier}
			rm := ResolveAutonomy(&wf, nil)
			if rm == nil {
				t.Fatalf("ResolveAutonomy returned nil for tier %q", row.Tier)
			}
			for _, cls := range []string{ActionApprove, ActionFixup, ActionWaive, ActionRetry, ActionMerge} {
				wantEntry := row.Classes[cls]
				got := resolvedFor(t, rm, cls)
				if got.Mode != ActionMode(wantEntry.Mode) {
					t.Errorf("class %s: mode = %q, want %q", cls, got.Mode, wantEntry.Mode)
				}
				if got.Condition != DelegationCondition(wantEntry.When) {
					t.Errorf("class %s: condition = %q, want %q", cls, got.Condition, wantEntry.When)
				}
			}

			// Page list comparison. Backend convention: expandTier(TierLow) sets
			// PageHumanOn to nil (no field assigned), so resolveBlock carries nil
			// for low — compare nil/empty-tolerantly via len==0 short-circuit so a
			// []string{} vs nil mismatch cannot masquerade as corpus drift.
			if len(row.PageHumanOn) == 0 {
				if len(rm.PageHumanOn) != 0 {
					t.Errorf("tier %s: PageHumanOn = %v, want nil/empty (nothing to page on)", row.Tier, rm.PageHumanOn)
				}
			} else {
				if !reflect.DeepEqual(rm.PageHumanOn, row.PageHumanOn) {
					t.Errorf("tier %s: PageHumanOn = %v, want %v", row.Tier, rm.PageHumanOn, row.PageHumanOn)
				}
			}

			// (b) Internal table: expandTier agrees with the corpus row.
			expanded := expandTier(tier)
			for cls, wantEntry := range row.Classes {
				gotEntry, ok := expanded.Classes[cls]
				if !ok {
					t.Errorf("tier %s: expandTier has no class %q", row.Tier, cls)
					continue
				}
				if string(gotEntry.Mode) != wantEntry.Mode {
					t.Errorf("tier %s class %s: expandTier mode = %q, want %q", row.Tier, cls, gotEntry.Mode, wantEntry.Mode)
				}
				if string(gotEntry.When) != wantEntry.When {
					t.Errorf("tier %s class %s: expandTier when = %q, want %q", row.Tier, cls, gotEntry.When, wantEntry.When)
				}
			}

			// Anchor: corpus page list == tierPageList for medium and high.
			// A solo edit to tierPageList without updating the corpus (or vice
			// versa) reddens here before TestExpandTier_MatchesFrozenTierTable
			// can catch it.
			if row.Tier != "low" {
				if !reflect.DeepEqual(row.PageHumanOn, tierPageList) {
					t.Errorf("tier %s: corpus page list %v != tierPageList %v — corpus or frozen literal drifted", row.Tier, row.PageHumanOn, tierPageList)
				}
			}

			// Anchor: derived operator_agent block == frozenTierBlocks[tier].
			// This cross-checks the corpus against the pre-existing frozen literal,
			// so a corpus row authored wrong immediately reddens against frozenTierBlocks
			// rather than silently becoming the new truth.
			wantBlock := frozenTierBlocks[tier]
			if wantBlock == nil {
				t.Fatalf("no frozen block for tier %s", row.Tier)
			}
			gotBlock := DerivedOperatorAgent(rm)
			if !reflect.DeepEqual(gotBlock, wantBlock) {
				t.Errorf("tier %s: DerivedOperatorAgent = %+v, want the frozen block %+v", row.Tier, gotBlock, wantBlock)
			}
		})
	}
}

// --- workflow-v2 unified autonomy grammar (ADR-066 / E52.10 / #2222) --------
//
// These tests assert SHIPPED behaviour rather than the presence of an edit:
// the tier tables, the mode->knob projection and the page-event vocabulary
// are config-shaped values compilation cannot enforce.

// v2Doc wraps a workflow body in the minimal valid v2 envelope, so each test
// below reads as the grammar under test and nothing else.
func v2Doc(workflowBody string) []byte {
	return []byte(`version: "2"
workflows:
  feature_change:
` + workflowBody + `    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`)
}

// mustParse parses a document that is expected to be valid.
func mustParse(t *testing.T, doc []byte) *Spec {
	t.Helper()
	s, err := ParseBytes(doc)
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	return s
}

// resolvedFor looks up one action class in a resolved matrix.
func resolvedFor(t *testing.T, rm *ResolvedMatrix, action string) ResolvedAction {
	t.Helper()
	if rm == nil {
		t.Fatalf("ResolvedMatrix = nil, want a resolved block")
	}
	for _, a := range rm.Actions {
		if a.Action == action {
			return a
		}
	}
	t.Fatalf("resolved matrix has no %q entry (actions: %+v)", action, rm.Actions)
	return ResolvedAction{}
}

// delegatedKnobs lists the may_* knobs a derived block actually delegates.
// An empty result is the property that makes delegation.Evaluate produce ZERO
// decisions — the decision-set half of AC5 is asserted in the delegation
// package (which owns Evaluate and its repository fakes); here we assert the
// input that determines it.
func delegatedKnobs(oa *OperatorAgent) []string {
	if oa == nil {
		return nil
	}
	var out []string
	for _, k := range []struct {
		name string
		cond DelegationCondition
	}{
		{ActionApprove, oa.MayApprove},
		{ActionFixup, oa.MayRouteFixup},
		{ActionWaive, oa.MayWaive},
		{ActionRetry, oa.MayRetry},
		{ActionMerge, oa.MayMerge},
	} {
		if k.cond != "" {
			out = append(out, k.name)
		}
	}
	return out
}

// tierPageList is the seven-event page list medium and high both expand
// to, spelled out ONCE here and shared by the frozen blocks below. It is
// a literal, not a call into the code under test.
var tierPageList = []string{
	"gating_reviewer_reject",
	"plan_rejection",
	"scope_amendment",
	"budget_override",
	"policy_override",
	"exception_request",
	"requirement_arbitration",
}

// frozenTierBlocks is the FROZEN literal expectation for each tier's
// derived operator_agent block, written out longhand.
//
// It deliberately does NOT read the shipped presets. Since E52.8 the
// presets declare `autonomy: <tier>` and the parser DERIVES their block
// via expandTier, so comparing a preset-derived block against expandTier
// would compare the function under test against itself and prove nothing.
// This table is the independent answer; TestShippedPresetsDeclareTheirTier
// is its byte-level companion, pinning WHICH tier each preset chose. The
// two together prove the presets and the tier expansions agree without
// either side supplying the other's answer.
var frozenTierBlocks = map[AutonomyTier]*OperatorAgent{
	// Human-led: nothing delegated, and no page list — the low tier
	// excepts nothing because it absorbs nothing.
	TierLow: {},
	// Approve, fixup and retry delegated under their named conditions;
	// waive and merge stay human.
	TierMedium: {
		MayApprove:    "clean_dual_approval",
		MayRouteFixup: "convergent_concerns",
		MayRetry:      "infra_flake",
		MustPageHuman: tierPageList,
	},
	// Medium plus waive and merge, so the agent can carry a clean change
	// through to merge.
	TierHigh: {
		MayApprove:    "clean_dual_approval",
		MayRouteFixup: "convergent_concerns",
		MayWaive:      "solo_low",
		MayRetry:      "infra_flake",
		MayMerge:      "gates_resolved_ci_green",
		MustPageHuman: tierPageList,
	},
}

// presetOperatorAgent parses a shipped preset and returns the
// operator_agent block its `autonomy:` line derives.
func presetOperatorAgent(t *testing.T, p Preset) *OperatorAgent {
	t.Helper()
	b, err := PresetBytes(p)
	if err != nil {
		t.Fatalf("PresetBytes(%s): %v", p, err)
	}
	s, err := ParseBytes(b)
	if err != nil {
		t.Fatalf("ParseBytes(preset %s): %v", p, err)
	}
	return s.Workflows["feature_change"].OperatorAgent
}

// --- the partitioning unmarshaller (DEFECT-1's mechanism) -------------------

// TestActionMatrix_UnmarshalPartitionsReservedKeys pins the reserved-key
// partitioning DIRECTLY, and the symmetric marshaller with it. Ordinary
// struct decoding routes no arbitrary sibling property into a named field,
// so without the custom UnmarshalJSON the Classes map here would be EMPTY.
func TestActionMatrix_UnmarshalPartitionsReservedKeys(t *testing.T) {
	raw := []byte(`{
	  "approve": {"mode": "gated"},
	  "fixup": {"mode": "auto", "when": "convergent_concerns", "min_severity": "high"},
	  "promote": {"mode": "report"},
	  "page_human_on": ["gating_reviewer_reject"],
	  "model_policy": {"strategy": "explicit_defaults"}
	}`)
	var m ActionMatrix
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	want := map[string]ActionEntry{
		ActionApprove: {Mode: ModeGated},
		ActionFixup:   {Mode: ModeAuto, When: ConditionConvergentConcerns, MinSeverity: "high"},
		"promote":     {Mode: ModeReport},
	}
	if !reflect.DeepEqual(m.Classes, want) {
		t.Errorf("Classes = %+v, want %+v", m.Classes, want)
	}
	if got := m.PageHumanOn; len(got) != 1 || got[0] != PageEventGatingReviewerReject {
		t.Errorf("PageHumanOn = %v, want the one declared event", got)
	}
	if m.ModelPolicy == nil || m.ModelPolicy.Strategy != ModelPolicyExplicitDefaults {
		t.Errorf("ModelPolicy = %+v, want the declared strategy", m.ModelPolicy)
	}
	if _, ok := m.Classes["page_human_on"]; ok {
		t.Error("Classes carries the reserved key page_human_on — the partition leaked")
	}

	// Symmetric round-trip: classes re-emit as siblings of the reserved keys.
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back ActionMatrix
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("Unmarshal(round-trip): %v", err)
	}
	if !reflect.DeepEqual(back, m) {
		t.Errorf("round-trip = %+v, want %+v", back, m)
	}
}

// TestActionMatrix_MarshalDropsReservedKeyCollision covers the marshaller's
// defensive branch: UnmarshalJSON never routes a reserved key into Classes,
// but a hand-built matrix could collide, and the reserved key must win so the
// emitted document stays decodable.
func TestActionMatrix_MarshalDropsReservedKeyCollision(t *testing.T) {
	m := ActionMatrix{
		Classes:     map[string]ActionEntry{"page_human_on": {Mode: ModeGated}},
		PageHumanOn: []string{PageEventPlanRejection},
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back ActionMatrix
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("Unmarshal(round-trip): %v", err)
	}
	if len(back.Classes) != 0 {
		t.Errorf("Classes = %+v, want the colliding entry dropped", back.Classes)
	}
	if got := back.PageHumanOn; len(got) != 1 || got[0] != PageEventPlanRejection {
		t.Errorf("PageHumanOn = %v, want the reserved key to win", got)
	}
}

// TestActionMatrix_MarshalPreservesExplicitEmptyPageList pins the nil-vs-empty
// distinction the symmetric round-trip turns on. A NON-nil empty
// page_human_on is a deliberate "page on NOTHING" override that resolveBlock
// honours (it overrides the tier's list); guarding the marshaller on len() > 0
// would drop it, and the round trip would decode back to nil — silently
// falling through to the tier's page list and losing the override. A nil page
// list is still omitted (absence, not an override).
func TestActionMatrix_MarshalPreservesExplicitEmptyPageList(t *testing.T) {
	explicitEmpty := ActionMatrix{
		Classes:     map[string]ActionEntry{ActionMerge: {Mode: ModeAuto, When: ConditionGatesResolvedCIGreen}},
		PageHumanOn: []string{},
	}
	out, err := json.Marshal(explicitEmpty)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(out), "page_human_on") {
		t.Errorf("marshalled matrix = %s, want an explicit empty page_human_on emitted", out)
	}
	var back ActionMatrix
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("Unmarshal(round-trip): %v", err)
	}
	if back.PageHumanOn == nil {
		t.Error("round-trip PageHumanOn = nil, want a non-nil empty slice — the explicit override was lost")
	}
	if len(back.PageHumanOn) != 0 {
		t.Errorf("round-trip PageHumanOn = %v, want empty", back.PageHumanOn)
	}
	if !reflect.DeepEqual(back, explicitEmpty) {
		t.Errorf("round-trip = %+v, want %+v", back, explicitEmpty)
	}

	// A nil page list is absence, not an override, and stays omitted.
	nilList := ActionMatrix{Classes: map[string]ActionEntry{ActionApprove: {Mode: ModeGated}}}
	nilOut, err := json.Marshal(nilList)
	if err != nil {
		t.Fatalf("Marshal(nil list): %v", err)
	}
	if strings.Contains(string(nilOut), "page_human_on") {
		t.Errorf("marshalled matrix = %s, want no page_human_on for a nil list", nilOut)
	}
}

// TestActionMatrix_UnmarshalRejectsMalformed covers the three error branches
// of the partitioning decoder: a non-object matrix, a malformed reserved key
// and a malformed class entry, each reported naming the offending property.
func TestActionMatrix_UnmarshalRejectsMalformed(t *testing.T) {
	for name, tc := range map[string]struct{ raw, wantSubstr string }{
		"non-object matrix":   {`"gated"`, "cannot unmarshal"},
		"bad page_human_on":   {`{"page_human_on": 3}`, "actions.page_human_on"},
		"bad model_policy":    {`{"model_policy": []}`, "actions.model_policy"},
		"bad class entry":     {`{"approve": "gated"}`, "actions.approve"},
		"bad extension entry": {`{"promote": 7}`, "actions.promote"},
	} {
		t.Run(name, func(t *testing.T) {
			var m ActionMatrix
			err := json.Unmarshal([]byte(tc.raw), &m)
			if err == nil {
				t.Fatalf("Unmarshal(%s) = nil error, want a decode failure", tc.raw)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantSubstr)
			}
		})
	}
}

// --- F5: the DEFECT-1 regression pair ---------------------------------------

// TestParseV2_ExplicitEntryOverridesTierForThatClassOnly is the DEFECT-1
// regression: a workflow at `autonomy: medium` (which delegates approve)
// declaring an explicit `approve: {mode: gated}` must resolve GATED with
// Source=explicit and derive an EMPTY MayApprove — while retry, which the
// override does not name, keeps medium's auto. A tier-agreeing round-trip
// cannot make this assertion: if the explicit entry were silently discarded
// the workflow would still resolve to medium's `approve: auto` and the agent
// could self-approve.
func TestParseV2_ExplicitEntryOverridesTierForThatClassOnly(t *testing.T) {
	s := mustParse(t, v2Doc(`    autonomy: medium
    actions:
      approve:
        mode: gated
`))
	wf := s.Workflows["feature_change"]

	rm := ResolveAutonomy(&wf, nil)
	approve := resolvedFor(t, rm, ActionApprove)
	if approve.Mode != ModeGated {
		t.Errorf("approve mode = %q, want %q — the explicit entry must win over the tier", approve.Mode, ModeGated)
	}
	if approve.Source != SourceExplicit {
		t.Errorf("approve source = %q, want %q", approve.Source, SourceExplicit)
	}
	if approve.Condition != "" {
		t.Errorf("approve condition = %q, want empty — a gated class delegates nothing", approve.Condition)
	}
	if retry := resolvedFor(t, rm, ActionRetry); retry.Mode != ModeAuto || retry.Source != SourceTier {
		t.Errorf("retry = %+v, want the tier's auto/infra_flake — the override is per-class, not wholesale", retry)
	}

	if got := wf.OperatorAgent; got == nil || got.MayApprove != "" {
		t.Errorf("derived MayApprove = %+v, want EMPTY — mode: gated is knob-absence", got)
	}
	if got := wf.OperatorAgent.MayRetry; got != ConditionInfraFlake {
		t.Errorf("derived MayRetry = %q, want %q", got, ConditionInfraFlake)
	}
}

// TestParseV2_ExplicitEntrySurvivesTheTypedDecode is F5's companion, pinning
// the PARTITIONING at the parse layer rather than the resolution layer: after
// ParseBytes (which decodes under DisallowUnknownFields) the raw
// ActionMatrix.Classes map must be NON-EMPTY and carry the declared entry. An
// ordinary-struct decode would leave it empty and the test above would then be
// asserting resolution over an input that never arrived.
func TestParseV2_ExplicitEntrySurvivesTheTypedDecode(t *testing.T) {
	s := mustParse(t, v2Doc(`    autonomy: medium
    actions:
      approve:
        mode: gated
`))
	m := s.Workflows["feature_change"].Actions
	if m == nil {
		t.Fatal("Workflow.Actions = nil, want the declared matrix")
	}
	if len(m.Classes) == 0 {
		t.Fatal("Actions.Classes is EMPTY — the reserved-key partitioning dropped the explicit entry")
	}
	if got, ok := m.Classes[ActionApprove]; !ok || got.Mode != ModeGated {
		t.Errorf("Classes[approve] = %+v (present=%v), want {Mode: gated}", got, ok)
	}
}

// --- AC1 / AC8: the tier expansions -----------------------------------------

// TestParseV2_AutonomyMediumDerivesMediumPresetBlock is AC1: a v2 document
// declaring ONLY `autonomy: medium` derives an OperatorAgent deep-equal to
// the FROZEN medium block, and the shipped medium preset — which now
// declares that same one-word tier — derives the very same block end to end.
func TestParseV2_AutonomyMediumDerivesMediumPresetBlock(t *testing.T) {
	want := frozenTierBlocks[TierMedium]

	got := mustParse(t, v2Doc("    autonomy: medium\n")).Workflows["feature_change"].OperatorAgent
	if !reflect.DeepEqual(got, want) {
		t.Errorf("derived block = %+v, want the frozen medium block %+v", got, want)
	}
	if fromPreset := presetOperatorAgent(t, PresetMedium); !reflect.DeepEqual(fromPreset, want) {
		t.Errorf("shipped medium preset derived %+v, want the frozen medium block %+v", fromPreset, want)
	}
}

// TestExpandTier_MatchesFrozenTierTable is AC8: for EACH tier the
// tier-derived block equals the FROZEN literal block, and the shipped
// preset for that tier derives the same thing end to end.
//
// The expectation is a longhand literal rather than the shipped preset's
// authored block, because since E52.8 the presets declare `autonomy:
// <tier>` and the parser derives their block via expandTier — comparing
// against them would compare expandTier against itself. Low compares
// against an EMPTY block and additionally asserts a nil block and the
// derived empty block delegate the same (nothing) — the input that makes
// both produce zero delegation decisions.
func TestExpandTier_MatchesFrozenTierTable(t *testing.T) {
	for _, tc := range []struct {
		tier   AutonomyTier
		preset Preset
	}{
		{TierLow, PresetLow},
		{TierMedium, PresetMedium},
		{TierHigh, PresetHigh},
	} {
		t.Run(string(tc.tier), func(t *testing.T) {
			want := frozenTierBlocks[tc.tier]
			if want == nil {
				t.Fatalf("no frozen block for tier %s", tc.tier)
			}
			wf := Workflow{Autonomy: tc.tier}
			if got := DerivedOperatorAgent(ResolveAutonomy(&wf, nil)); !reflect.DeepEqual(got, want) {
				t.Errorf("tier %s derived %+v, want the frozen block %+v", tc.tier, got, want)
			}
			if got := presetOperatorAgent(t, tc.preset); !reflect.DeepEqual(got, want) {
				t.Errorf("shipped %s preset derived %+v, want the frozen %s block %+v", tc.preset, got, tc.tier, want)
			}
		})
	}

	// Low specifically: nil and the derived empty block delegate identically.
	low := DerivedOperatorAgent(ResolveAutonomy(&Workflow{Autonomy: TierLow}, nil))
	if got := delegatedKnobs(low); len(got) != 0 {
		t.Errorf("low tier delegates %v, want nothing", got)
	}
	if got := delegatedKnobs(nil); len(got) != 0 {
		t.Errorf("a nil block delegates %v, want nothing", got)
	}
}

// TestExpandTier_UnknownTierResolvesEveryClassGated covers expandTier's
// defensive branch: the schema enum is the real gate, but an unrecognized
// tier reaching resolution must fall to the fail-closed default for every
// class rather than delegating anything.
func TestExpandTier_UnknownTierResolvesEveryClassGated(t *testing.T) {
	rm := ResolveAutonomy(&Workflow{Autonomy: AutonomyTier("aggressive")}, nil)
	if rm == nil {
		t.Fatal("ResolveAutonomy = nil, want a resolved (all-gated) block")
	}
	for _, a := range rm.Actions {
		if a.Mode != ModeGated || a.Source != SourceDefault {
			t.Errorf("%s = %+v, want gated/default", a.Action, a)
		}
	}
	if got := delegatedKnobs(DerivedOperatorAgent(rm)); len(got) != 0 {
		t.Errorf("an unrecognized tier delegates %v, want nothing", got)
	}
}

// --- AC5: explicit `mode: gated` is knob-absence -----------------------------

// TestDerivedBlock_AllGatedMatchesKnobAbsentV0Block is AC5, narrowed per the
// operator's DEFECT-2 ruling: a v2 workflow declaring EXPLICIT `mode: gated`
// on all five classes plus a page list, and a v0 workflow whose
// operator_agent declares the same page list and NO may_* knobs, are asserted
// equal on the DERIVED *OperatorAgent block and on delegating nothing.
//
// It deliberately does NOT assert whole-Result equality: Tier and per-class
// Source provenance legitimately differ (the v2 side resolves explicit
// provenance, the v0 side has none). The delegation DECISION-set half of AC5
// is asserted in backend/internal/delegation, which owns Evaluate; the input
// that determines it — every may_* knob empty on both sides — is asserted
// here. The v2 side is EXPLICITLY gated rather than left unconfigured, so
// `mode: gated` itself is what is under test.
func TestDerivedBlock_AllGatedMatchesKnobAbsentV0Block(t *testing.T) {
	v2 := mustParse(t, v2Doc(`    actions:
      approve:
        mode: gated
      fixup:
        mode: gated
      waive:
        mode: gated
      retry:
        mode: gated
      merge:
        mode: gated
      page_human_on:
        - gating_reviewer_reject
        - budget_override
`))
	v0, err := ParseBytes([]byte(`version: "0.7"
workflows:
  feature_change:
    operator_agent:
      must_page_human:
        - gating_reviewer_reject
        - budget_override
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`))
	if err != nil {
		t.Fatalf("ParseBytes(v0): %v", err)
	}

	gotV2 := v2.Workflows["feature_change"].OperatorAgent
	gotV0 := v0.Workflows["feature_change"].OperatorAgent
	if !reflect.DeepEqual(gotV2, gotV0) {
		t.Errorf("v2 all-gated derived %+v, want the v0 knob-absent block %+v", gotV2, gotV0)
	}
	if got := delegatedKnobs(gotV2); len(got) != 0 {
		t.Errorf("an all-gated v2 matrix delegates %v, want nothing", got)
	}
	if got := delegatedKnobs(gotV0); len(got) != 0 {
		t.Errorf("a knob-absent v0 block delegates %v, want nothing", got)
	}
}

// --- F6: the gate block wins WHOLESALE --------------------------------------

// TestResolveAutonomy_GateBlockWinsWholesale is F6: an approval gate
// declaring `actions: {merge: auto}` on a workflow at `autonomy: low`
// supplies the WHOLE block — merge is auto and every other class is gated
// with Source=default, NOT inherited or merged from the workflow level. The
// companion assertion covers model_policy not crossing levels either.
func TestResolveAutonomy_GateBlockWinsWholesale(t *testing.T) {
	s := mustParse(t, []byte(`version: "2"
workflows:
  feature_change:
    autonomy: low
    actions:
      model_policy:
        strategy: follow_plan_recommendation
      page_human_on:
        - plan_rejection
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
        gates:
          - type: approval
            approvals:
              count: 1
            actions:
              merge:
                mode: auto
                when: gates_resolved_ci_green
`))
	wf := s.Workflows["feature_change"]
	gate := &wf.Stages[0].Gates[0]

	rm := ResolveAutonomy(&wf, gate)
	if rm.Tier != "" {
		t.Errorf("gate-resolved tier = %q, want empty — the gate declared no tier and inherits none", rm.Tier)
	}
	if merge := resolvedFor(t, rm, ActionMerge); merge.Mode != ModeAuto || merge.Source != SourceExplicit {
		t.Errorf("merge = %+v, want auto/explicit", merge)
	}
	for _, action := range []string{ActionApprove, ActionFixup, ActionWaive, ActionRetry} {
		got := resolvedFor(t, rm, action)
		if got.Mode != ModeGated || got.Source != SourceDefault {
			t.Errorf("%s = %+v, want gated/default — the gate block is wholesale, nothing is merged from the workflow", action, got)
		}
	}
	if rm.ModelPolicy != nil {
		t.Errorf("gate-resolved model_policy = %+v, want nil — it is never merged across levels", rm.ModelPolicy)
	}
	if rm.PageHumanOn != nil {
		t.Errorf("gate-resolved page_human_on = %v, want nil — never merged across levels", rm.PageHumanOn)
	}

	// The gate's own derived block reflects the wholesale override, and the
	// workflow level is untouched by it.
	if got := gate.OperatorAgent; got == nil || got.MayMerge != ConditionGatesResolvedCIGreen || got.MayApprove != "" {
		t.Errorf("gate derived block = %+v, want may_merge only", got)
	}
	if got := wf.OperatorAgent; got == nil || got.MayMerge != "" {
		t.Errorf("workflow derived block = %+v, want low's all-gated (no may_merge)", got)
	}
	if got := wf.OperatorAgent.ModelPolicy; got == nil || got.Strategy != ModelPolicyFollowPlanRecommendation {
		t.Errorf("workflow model_policy = %+v, want the declared strategy", got)
	}
}

// --- F1 / F2 / F3: `mode: auto` rejections ----------------------------------

// TestParseV2_RejectsInvalidAutoEntries covers the three fail-closed
// rejections around `mode: auto` (F1, F2, F3) plus the min_severity
// placement rule, each asserted on the SHIPPED message rather than the fact
// of rejection.
func TestParseV2_RejectsInvalidAutoEntries(t *testing.T) {
	for name, tc := range map[string]struct {
		body      string
		wantParts []string
	}{
		// F1: an extension class has no backend-evaluable condition at all.
		"extension class at auto": {
			body: `    actions:
      promote:
        mode: auto
`,
			wantParts: []string{`"promote"`, "extension class", "no backend-evaluable condition", "mode: gated", "approve, fixup, waive, retry, merge"},
		},
		// F2: a known class at auto with no `when`.
		"known class at auto with no when": {
			body: `    actions:
      approve:
        mode: auto
`,
			wantParts: []string{`"approve"`, "mode: auto", "when: clean_dual_approval"},
		},
		// F3: a known class at auto naming ANOTHER class's condition.
		"known class at auto with the wrong when": {
			body: `    actions:
      approve:
        mode: auto
        when: infra_flake
`,
			wantParts: []string{`"approve"`, "infra_flake", "clean_dual_approval"},
		},
		// min_severity tunes fixup's threshold and is accepted there only.
		"min_severity off the fixup class": {
			body: `    actions:
      waive:
        mode: gated
        min_severity: high
`,
			wantParts: []string{"min_severity", `"fixup"`, `"waive"`},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseBytes(v2Doc(tc.body))
			if err == nil {
				t.Fatal("ParseBytes = nil error, want a rejection")
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("error = %T (%v), want *ValidationError", err, err)
			}
			for _, want := range tc.wantParts {
				if !strings.Contains(ve.Message, want) {
					t.Errorf("message = %q, want it to contain %q", ve.Message, want)
				}
			}
			if !strings.Contains(ve.Path, "/actions/") {
				t.Errorf("path = %q, want it to locate the offending class entry", ve.Path)
			}
		})
	}
}

// TestParseV2_RejectsReportEntryWithForeignWhen pins the report-firing
// correctness rule: a `mode: report` entry MAY omit `when` (it fires on
// gate-live), but when it declares one the per-class binding applies — the
// `when` must name THIS class's own condition. A foreign or extension-class
// `when` is a predicate the report arm cannot evaluate against this gate;
// accepting it and silently dropping it at fire time would make the proposal
// never surface, so it is rejected at validation instead.
func TestParseV2_RejectsReportEntryWithForeignWhen(t *testing.T) {
	for name, tc := range map[string]struct {
		body      string
		wantParts []string
	}{
		"known class at report with the wrong when": {
			body: `    actions:
      approve:
        mode: report
        when: infra_flake
`,
			wantParts: []string{`"approve"`, "mode: report", "infra_flake", "clean_dual_approval"},
		},
		"extension class at report with a when": {
			body: `    actions:
      promote:
        mode: report
        when: clean_dual_approval
`,
			wantParts: []string{`"promote"`, "extension class", "no backend-evaluable condition", "gate-live"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseBytes(v2Doc(tc.body))
			if err == nil {
				t.Fatal("ParseBytes = nil error, want a rejection")
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("error = %T (%v), want *ValidationError", err, err)
			}
			for _, want := range tc.wantParts {
				if !strings.Contains(ve.Message, want) {
					t.Errorf("message = %q, want it to contain %q", ve.Message, want)
				}
			}
			if !strings.Contains(ve.Path, "/actions/") || !strings.Contains(ve.Path, "/when") {
				t.Errorf("path = %q, want it to locate the offending report `when`", ve.Path)
			}
		})
	}
}

// TestParseV2_AcceptsReportEntryWithOwnWhen is the positive companion: a
// report entry naming its class's OWN condition is legal (it fires on gate-live
// AND the condition met), and a bare report entry with no `when` is legal too.
func TestParseV2_AcceptsReportEntryWithOwnWhen(t *testing.T) {
	for name, body := range map[string]string{
		"own condition": `    actions:
      approve:
        mode: report
        when: clean_dual_approval
`,
		"no when": `    actions:
      approve:
        mode: report
`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseBytes(v2Doc(body)); err != nil {
				t.Fatalf("ParseBytes = %v, want the report entry accepted", err)
			}
		})
	}
}

// TestParseV2_RejectsInvalidAutoEntryOnAGate proves the validation reaches
// the gate placement level too — a workflow-level-only sweep would pass every
// case above and miss this one.
func TestParseV2_RejectsInvalidAutoEntryOnAGate(t *testing.T) {
	_, err := ParseBytes([]byte(`version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
        gates:
          - type: approval
            approvals:
              count: 1
            actions:
              merge:
                mode: auto
`))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %T (%v), want *ValidationError", err, err)
	}
	if !strings.Contains(ve.Path, "/gates/0/actions/merge") {
		t.Errorf("path = %q, want it to locate the gate-level entry", ve.Path)
	}
	if !strings.Contains(ve.Message, "gates_resolved_ci_green") {
		t.Errorf("message = %q, want it to name merge's legal condition", ve.Message)
	}
}

// --- F4 / F8 / F9 / F10: the remaining enumerated modes ----------------------

// TestParseV2_ExtensionClassContributesNoKnob is F4: an unknown class at
// `mode: gated` and at `mode: report` is ACCEPTED (ADR-065 extensibility) and
// the derived block is byte-identical to the same document without it.
func TestParseV2_ExtensionClassContributesNoKnob(t *testing.T) {
	base := mustParse(t, v2Doc("    autonomy: medium\n")).Workflows["feature_change"].OperatorAgent
	for _, mode := range []ActionMode{ModeGated, ModeReport} {
		t.Run(string(mode), func(t *testing.T) {
			s := mustParse(t, v2Doc(`    autonomy: medium
    actions:
      promote:
        mode: `+string(mode)+`
`))
			wf := s.Workflows["feature_change"]
			if !reflect.DeepEqual(wf.OperatorAgent, base) {
				t.Errorf("derived block with a %s extension class = %+v, want the same block as without it %+v", mode, wf.OperatorAgent, base)
			}
			// The class is still SURFACED — it just delegates nothing.
			got := resolvedFor(t, ResolveAutonomy(&wf, nil), "promote")
			if got.Mode != mode || got.Source != SourceExplicit {
				t.Errorf("promote = %+v, want %s/explicit", got, mode)
			}
		})
	}
}

// TestParseV2_NoAutonomyBlockDerivesNil is F8: a v2 workflow declaring
// NEITHER `autonomy` nor `actions` derives a nil OperatorAgent, so
// delegation stays unconfigured and the run-status delegation block stays
// omitted — byte-identical to today.
func TestParseV2_NoAutonomyBlockDerivesNil(t *testing.T) {
	s := mustParse(t, v2Doc(""))
	wf := s.Workflows["feature_change"]
	if wf.OperatorAgent != nil {
		t.Errorf("OperatorAgent = %+v, want nil for a workflow declaring no autonomy block", wf.OperatorAgent)
	}
	if rm := ResolveAutonomy(&wf, nil); rm != nil {
		t.Errorf("ResolveAutonomy = %+v, want nil", rm)
	}
	if got := DerivedOperatorAgent(nil); got != nil {
		t.Errorf("DerivedOperatorAgent(nil) = %+v, want nil", got)
	}
}

// TestParseV2_RejectsUnknownAutonomyTier is F9: `autonomy: aggressive` is a
// schema enum rejection naming the three legal tiers, not a Go-side error.
func TestParseV2_RejectsUnknownAutonomyTier(t *testing.T) {
	_, err := ParseBytes(v2Doc("    autonomy: aggressive\n"))
	se := requireSchemaError(t, err)
	for _, want := range []string{"low", "medium", "high"} {
		if !strings.Contains(se.Message, want) {
			t.Errorf("message = %q, want it to name the legal tier %q", se.Message, want)
		}
	}
	if !strings.Contains(se.Path, "autonomy") {
		t.Errorf("path = %q, want it to name the autonomy property", se.Path)
	}
}

// TestParseV2_ReportModeDerivesNoKnob is F10's spec-side half: `approve:
// {mode: report}` surfaces mode=report on the resolved matrix and derives an
// EMPTY MayApprove — report never acts. The AutoDriveRunGate arm that turns
// that surfaced entry into a run_auto_driven act:report row (and emits it at
// most once per gate occurrence) lives in backend/internal/server.
func TestParseV2_ReportModeDerivesNoKnob(t *testing.T) {
	s := mustParse(t, v2Doc(`    actions:
      approve:
        mode: report
`))
	wf := s.Workflows["feature_change"]
	if got := resolvedFor(t, ResolveAutonomy(&wf, nil), ActionApprove); got.Mode != ModeReport {
		t.Errorf("approve = %+v, want mode: report", got)
	}
	if got := wf.OperatorAgent; got == nil || got.MayApprove != "" {
		t.Errorf("derived MayApprove = %+v, want EMPTY — report never acts", got)
	}
	if got := delegatedKnobs(wf.OperatorAgent); len(got) != 0 {
		t.Errorf("a report-only matrix delegates %v, want nothing", got)
	}
}

// TestParseV2_ReportModeCarriesItsOptionalCondition pins the report-firing
// rule's data half (operator condition 1): a report entry MAY declare a
// `when`, and when it does the resolved matrix carries it so the auto-drive
// arm can require it; a bare report entry carries none and fires on
// gate-live.
func TestParseV2_ReportModeCarriesItsOptionalCondition(t *testing.T) {
	s := mustParse(t, v2Doc(`    actions:
      approve:
        mode: report
        when: clean_dual_approval
      merge:
        mode: report
`))
	wf := s.Workflows["feature_change"]
	rm := ResolveAutonomy(&wf, nil)
	if got := resolvedFor(t, rm, ActionApprove); got.Condition != ConditionCleanDualApproval {
		t.Errorf("approve = %+v, want the declared condition carried through", got)
	}
	if got := resolvedFor(t, rm, ActionMerge); got.Condition != "" {
		t.Errorf("merge = %+v, want no condition — a bare report entry fires on gate-live", got)
	}
}

// --- resolution plumbing ----------------------------------------------------

// TestResolveAutonomy_NilInputs covers the ladder's degenerate rungs: a nil
// workflow and a workflow declaring nothing both resolve to nil (fail-closed
// — nothing delegated).
func TestResolveAutonomy_NilInputs(t *testing.T) {
	if got := ResolveAutonomy(nil, nil); got != nil {
		t.Errorf("ResolveAutonomy(nil, nil) = %+v, want nil", got)
	}
	if got := ResolveAutonomy(&Workflow{}, &Gate{Type: GateTypeApproval}); got != nil {
		t.Errorf("ResolveAutonomy(empty, empty gate) = %+v, want nil", got)
	}
}

// TestResolveAutonomy_GateWithoutBlockFallsBackToWorkflow: a gate declaring
// NEITHER surface inherits the workflow block wholesale, as the
// operator_agent gate override always did.
func TestResolveAutonomy_GateWithoutBlockFallsBackToWorkflow(t *testing.T) {
	s := mustParse(t, []byte(`version: "2"
workflows:
  feature_change:
    autonomy: high
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
        gates:
          - type: approval
            approvals:
              count: 1
`))
	wf := s.Workflows["feature_change"]
	gate := &wf.Stages[0].Gates[0]
	if gate.OperatorAgent != nil {
		t.Errorf("gate OperatorAgent = %+v, want nil — the gate declared no block of its own", gate.OperatorAgent)
	}
	rm := ResolveAutonomy(&wf, gate)
	if rm == nil || rm.Tier != TierHigh {
		t.Fatalf("gate resolution = %+v, want the workflow's high tier", rm)
	}
	if got := resolvedFor(t, rm, ActionMerge); got.Mode != ModeAuto || got.Source != SourceTier {
		t.Errorf("merge = %+v, want high's auto/tier", got)
	}
}

// TestResolveAutonomy_OrdersActionsDeterministically pins the surfacing
// order: the five known classes in canonical order, then extension classes
// sorted by name. Go's map iteration is randomized, so an unordered result
// would make every downstream snapshot flaky.
func TestResolveAutonomy_OrdersActionsDeterministically(t *testing.T) {
	s := mustParse(t, v2Doc(`    autonomy: medium
    actions:
      zeta:
        mode: gated
      promote:
        mode: report
`))
	wf := s.Workflows["feature_change"]
	rm := ResolveAutonomy(&wf, nil)
	var got []string
	for _, a := range rm.Actions {
		got = append(got, a.Action)
	}
	want := []string{ActionApprove, ActionFixup, ActionWaive, ActionRetry, ActionMerge, "promote", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("action order = %v, want %v", got, want)
	}
}

// TestDerivedOperatorAgent_CarriesFixupMinSeverity: min_severity rides the
// fixup class and lands on the derived RouteFixupMinSeverity knob whatever
// the class's mode, because the threshold describes the concern set, not the
// delegation.
func TestDerivedOperatorAgent_CarriesFixupMinSeverity(t *testing.T) {
	s := mustParse(t, v2Doc(`    autonomy: medium
    actions:
      fixup:
        mode: auto
        when: convergent_concerns
        min_severity: high
`))
	oa := s.Workflows["feature_change"].OperatorAgent
	if oa == nil || oa.RouteFixupMinSeverity != "high" {
		t.Errorf("RouteFixupMinSeverity = %+v, want \"high\"", oa)
	}
	if oa.MayRouteFixup != ConditionConvergentConcerns {
		t.Errorf("MayRouteFixup = %q, want %q", oa.MayRouteFixup, ConditionConvergentConcerns)
	}
}

// TestParseV2_AutonomyFixtureGoldenParse parses the shipped fixture and pins
// the resolution its comments claim: workflow-level medium with two explicit
// overrides and an extension class, plus a wholesale gate block.
func TestParseV2_AutonomyFixtureGoldenParse(t *testing.T) {
	b, err := os.ReadFile("testdata/valid/autonomy-v2.yaml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	s := mustParse(t, b)
	wf := s.Workflows["feature_change"]

	rm := ResolveAutonomy(&wf, nil)
	if rm.Tier != TierMedium {
		t.Errorf("tier = %q, want medium", rm.Tier)
	}
	for _, want := range []ResolvedAction{
		{Action: ActionApprove, Mode: ModeGated, Source: SourceExplicit},
		{Action: ActionFixup, Mode: ModeAuto, Condition: ConditionConvergentConcerns, MinSeverity: "high", Source: SourceExplicit},
		{Action: ActionWaive, Mode: ModeGated, Source: SourceTier},
		{Action: ActionRetry, Mode: ModeAuto, Condition: ConditionInfraFlake, Source: SourceTier},
		{Action: ActionMerge, Mode: ModeGated, Source: SourceTier},
		{Action: "promote", Mode: ModeReport, Source: SourceExplicit},
	} {
		if got := resolvedFor(t, rm, want.Action); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %+v, want %+v", want.Action, got, want)
		}
	}
	if got := rm.PageHumanOn; len(got) != 2 || got[0] != PageEventGatingReviewerReject {
		t.Errorf("page_human_on = %v, want the block's own two events (not the tier's seven)", got)
	}
	if rm.ModelPolicy == nil || rm.ModelPolicy.Strategy != ModelPolicyExplicitDefaults {
		t.Errorf("model_policy = %+v, want the declared strategy", rm.ModelPolicy)
	}

	gate := &wf.Stages[0].Gates[0]
	gateRM := ResolveAutonomy(&wf, gate)
	if got := resolvedFor(t, gateRM, ActionMerge); got.Mode != ModeAuto || got.Source != SourceExplicit {
		t.Errorf("gate merge = %+v, want auto/explicit", got)
	}
	if got := resolvedFor(t, gateRM, ActionFixup); got.Mode != ModeGated || got.Source != SourceDefault {
		t.Errorf("gate fixup = %+v, want gated/default — the gate block is wholesale", got)
	}
}

// --- the escalation ceiling's projection onto the derived knobs (#2227) -----

// TestDerivedOperatorAgent_AfterClamp_EmptiesTheKnob is the assertion the
// enforcement sites actually depend on. Every one of them — delegation's five
// knob evaluators, the checkDelegation handlers, AutoDriveRunGate and the
// campaign driver — reads the DERIVED *OperatorAgent, not the ResolvedMatrix.
// So clamping the matrix is only half the control: the caller must RE-DERIVE
// the knob block from the clamped matrix, or a run-status response would show
// `gated` while the agent stayed authorized to act.
//
// The fixture is criterion 4's exact case: an EXPLICIT `actions` override
// declaring merge at auto — which beats the tier — under a fired `low`
// ceiling, asserted on the derived MayMerge knob.
func TestDerivedOperatorAgent_AfterClamp_EmptiesTheKnob(t *testing.T) {
	wf := &Workflow{
		Autonomy: TierMedium,
		Actions: &ActionMatrix{Classes: map[string]ActionEntry{
			ActionMerge: {Mode: ModeAuto, When: ConditionGatesResolvedCIGreen},
		}},
	}
	resolved := ResolveAutonomy(wf, nil)
	if got := resolvedFor(t, resolved, ActionMerge); got.Mode != ModeAuto || got.Source != SourceExplicit {
		t.Fatalf("pre-clamp merge = %+v, want the explicit auto override (the control this test rests on)", got)
	}
	if before := DerivedOperatorAgent(resolved); before.MayMerge != ConditionGatesResolvedCIGreen {
		t.Fatalf("pre-clamp MayMerge = %q, want the explicit condition", before.MayMerge)
	}

	clamped := ClampResolvedMatrix(resolved, TierLow)
	if got := resolvedFor(t, clamped, ActionMerge); got.Mode != ModeGated || got.Source != SourceEscalation {
		t.Errorf("clamped merge = %+v, want gated with source escalation", got)
	}
	derived := DerivedOperatorAgent(clamped)
	if derived.MayMerge != "" {
		t.Errorf("MayMerge = %q, want EMPTY — the ceiling must reach the knob the enforcement sites read", derived.MayMerge)
	}
	if derived.MayApprove != "" {
		t.Errorf("MayApprove = %q, want empty under a low ceiling", derived.MayApprove)
	}
	if derived.MayRetry != "" {
		t.Errorf("MayRetry = %q, want empty under a low ceiling", derived.MayRetry)
	}
}

// TestDerivedOperatorAgent_AfterMediumClamp_KeepsPermittedKnobs is the
// paired control: a ceiling clamps only what it must, so `medium` leaves
// approve/fixup/retry delegated while closing merge.
func TestDerivedOperatorAgent_AfterMediumClamp_KeepsPermittedKnobs(t *testing.T) {
	resolved := ResolveAutonomy(&Workflow{Autonomy: TierHigh}, nil)
	derived := DerivedOperatorAgent(ClampResolvedMatrix(resolved, TierMedium))
	if derived.MayMerge != "" {
		t.Errorf("MayMerge = %q, want empty — medium gates merge", derived.MayMerge)
	}
	if derived.MayWaive != "" {
		t.Errorf("MayWaive = %q, want empty — medium gates waive", derived.MayWaive)
	}
	if derived.MayApprove != ConditionCleanDualApproval {
		t.Errorf("MayApprove = %q, want it preserved — medium holds approve at auto", derived.MayApprove)
	}
	if derived.MayRetry != ConditionInfraFlake {
		t.Errorf("MayRetry = %q, want it preserved — medium holds retry at auto", derived.MayRetry)
	}
}

// --- backlog-grooming action classes (ADR-065 / E54.4 / #2236) ---------------
//
// Four registered grooming classes: `hygiene` is the ONE auto-eligible one
// (bound to objective_reversible), and `ordering` / `dedup` / `scoping` are
// known but NON-DELEGABLE. `mode: auto` on the latter three is refused at
// PARSE time by TWO independent controls, and the tests below pin each of
// them SEPARATELY (approval condition G1) — an exact-message test for the
// non-delegable branch, and a registry-invariant test for the condition
// absence the extension-class branch keys off. Neither control's deletion is
// masked by the other's.

// groomingMatrixDoc wraps an `actions` block declaring one grooming class.
func groomingMatrixDoc(class, body string) []byte {
	return v2Doc("    actions:\n      " + class + ":\n" + body)
}

// TestValidateAutonomy_GroomingHygieneAutoAccepted is the NON-VACUITY
// control: the registry must not reject everything it touches. hygiene at
// auto under its own condition parses and resolves to auto/explicit.
func TestValidateAutonomy_GroomingHygieneAutoAccepted(t *testing.T) {
	s := mustParse(t, groomingMatrixDoc(ActionGroomHygiene,
		"        mode: auto\n        when: objective_reversible\n"))
	wf := s.Workflows["feature_change"]
	got := resolvedFor(t, ResolveAutonomy(&wf, nil), ActionGroomHygiene)
	if got.Mode != ModeAuto {
		t.Errorf("hygiene mode = %q, want auto", got.Mode)
	}
	if got.Condition != ConditionObjectiveReversible {
		t.Errorf("hygiene condition = %q, want %q", got.Condition, ConditionObjectiveReversible)
	}
	if got.Source != SourceExplicit {
		t.Errorf("hygiene source = %q, want explicit", got.Source)
	}
}

// TestValidateAutonomy_GroomingHygieneAutoNoWhen: auto with no condition is
// rejected naming the class and its legal condition.
func TestValidateAutonomy_GroomingHygieneAutoNoWhen(t *testing.T) {
	_, err := ParseBytes(groomingMatrixDoc(ActionGroomHygiene, "        mode: auto\n"))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if want := "/workflows/feature_change/actions/hygiene"; ve.Path != want {
		t.Errorf("Path = %q, want %q", ve.Path, want)
	}
	const want = `action class "hygiene" declares mode: auto with no ` + "`when`" + ` condition; mode: auto requires when: objective_reversible`
	if ve.Message != want {
		t.Errorf("Message = %q, want %q", ve.Message, want)
	}
}

// TestValidateAutonomy_GroomingHygieneAutoForeignWhen: hygiene may not
// borrow another class's condition.
func TestValidateAutonomy_GroomingHygieneAutoForeignWhen(t *testing.T) {
	_, err := ParseBytes(groomingMatrixDoc(ActionGroomHygiene,
		"        mode: auto\n        when: clean_dual_approval\n"))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if want := "/workflows/feature_change/actions/hygiene/when"; ve.Path != want {
		t.Errorf("Path = %q, want %q", ve.Path, want)
	}
	const want = `action class "hygiene" declares mode: auto with when: clean_dual_approval, which is not its condition; "hygiene" accepts only when: objective_reversible`
	if ve.Message != want {
		t.Errorf("Message = %q, want %q", ve.Message, want)
	}
}

// TestValidateAutonomy_GroomingNonDelegableClassesRefuseAuto pins CONTROL 1:
// the explicit non-delegable branch. It asserts the EXACT shipped message and
// JSON-pointer path, not merely that an error occurred, because the adjacent
// extension-class branch would ALSO reject these documents — a presence-only
// assertion would discriminate nothing, and deleting the branch under test
// would leave the test green.
//
// COUNTERFACTUAL: delete the nonDelegableGroomingClasses branch in
// validateAutonomy and this test goes RED on the message (the extension-class
// branch rejects with different text).
func TestValidateAutonomy_GroomingNonDelegableClassesRefuseAuto(t *testing.T) {
	for _, class := range []string{ActionGroomOrdering, ActionGroomDedup, ActionGroomScoping, ActionGroomMilestone} {
		t.Run(class, func(t *testing.T) {
			// `when` is declared too: a non-delegable class must be refused for
			// WHAT IT IS, not for a missing condition it could never satisfy.
			_, err := ParseBytes(groomingMatrixDoc(class,
				"        mode: auto\n        when: objective_reversible\n"))
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError refusing mode: auto on %q", err, class)
			}
			wantPath := "/workflows/feature_change/actions/" + class + "/mode"
			if ve.Path != wantPath {
				t.Errorf("Path = %q, want %q", ve.Path, wantPath)
			}
			want := MsgNonDelegableGroomingClassAuto(class)
			if ve.Message != want {
				t.Errorf("Message = %q,\nwant the non-delegable-class message %q\n(the extension-class branch rejects with different text — this assertion is what discriminates the two)", ve.Message, want)
			}
			if !strings.Contains(ve.Message, class) {
				t.Errorf("Message %q does not name the offending class %q", ve.Message, class)
			}
		})
	}
}

// TestGroomingRegistry_NonDelegableClassesCarryNoCondition pins CONTROL 2:
// the ABSENCE of a classConditions entry, which is what makes the pre-existing
// extension-class branch the second, independent refusal of `<class>: auto`.
// The non-delegable branch fires first, so this control is invisible to a
// message assertion — it needs its own proof.
//
// COUNTERFACTUAL: add `ActionGroomScoping: ConditionObjectiveReversible` to
// classConditions and this test goes RED. Registering a condition for a
// destructive class is exactly the refactor drift the registry design exists
// to make impossible, and this is the assertion that catches it.
func TestGroomingRegistry_NonDelegableClassesCarryNoCondition(t *testing.T) {
	for _, class := range []string{ActionGroomOrdering, ActionGroomDedup, ActionGroomScoping, ActionGroomMilestone} {
		if _, nonDelegable := nonDelegableGroomingClasses[class]; !nonDelegable {
			t.Errorf("%q is missing from nonDelegableGroomingClasses; the explicit refusal branch would not fire for it", class)
		}
		if cond, ok := classConditions[class]; ok {
			t.Errorf("classConditions[%q] = %q, want NO entry: a non-delegable grooming class must carry no backend-evaluable condition, or `mode: auto` on it becomes writable", class, cond)
		}
	}
	// The paired positive: hygiene IS registered, so the two sets are disjoint
	// and the registry is not simply empty.
	if got := classConditions[ActionGroomHygiene]; got != ConditionObjectiveReversible {
		t.Errorf("classConditions[%q] = %q, want %q", ActionGroomHygiene, got, ConditionObjectiveReversible)
	}
	if _, bothWays := nonDelegableGroomingClasses[ActionGroomHygiene]; bothWays {
		t.Errorf("%q appears in BOTH registries; the two must stay disjoint", ActionGroomHygiene)
	}
}

// TestGroomingRegistry_MilestoneClassNotInKnownActionOrder pins that the
// fifth grooming class follows the same rule as the other four: it is absent
// from knownActionOrder, so a workflow that does NOT declare it emits no
// entry for it in its resolved matrix and its derived delegation block is
// byte-identical to today. Registering it in knownActionOrder would attach a
// permanently-gated milestone entry to every workflow for no safety gain.
func TestGroomingRegistry_MilestoneClassNotInKnownActionOrder(t *testing.T) {
	for _, name := range knownActionOrder {
		if name == ActionGroomMilestone {
			t.Fatalf("knownActionOrder must not contain %q — a grooming class attaches a permanently-gated entry to every workflow's matrix", ActionGroomMilestone)
		}
	}
	// An ordinary workflow declaring only a tier resolves NO milestone entry.
	s := mustParse(t, v2Doc("    autonomy: medium\n"))
	wf := s.Workflows["feature_change"]
	rm := ResolveAutonomy(&wf, nil)
	for _, a := range rm.Actions {
		if a.Action == ActionGroomMilestone {
			t.Errorf("resolved matrix carries a %q entry (%+v) for a workflow that never declared it", ActionGroomMilestone, a)
		}
	}
	// Declared explicitly, it IS surfaced — the class is known, just not default.
	s2 := mustParse(t, groomingMatrixDoc(ActionGroomMilestone, "        mode: report\n"))
	wf2 := s2.Workflows["feature_change"]
	if got := resolvedFor(t, ResolveAutonomy(&wf2, nil), ActionGroomMilestone); got.Mode != ModeReport {
		t.Errorf("declared milestone mode = %q, want report", got.Mode)
	}
}

// TestValidateAutonomy_GroomingClassesAcceptGatedAndReport is the second
// non-vacuity control: the non-delegable classes are KNOWN and declarable, so
// a refusal-of-everything implementation fails here.
func TestValidateAutonomy_GroomingClassesAcceptGatedAndReport(t *testing.T) {
	for _, class := range []string{ActionGroomOrdering, ActionGroomDedup, ActionGroomScoping, ActionGroomMilestone} {
		for _, mode := range []ActionMode{ModeGated, ModeReport} {
			t.Run(class+"/"+string(mode), func(t *testing.T) {
				s := mustParse(t, groomingMatrixDoc(class, "        mode: "+string(mode)+"\n"))
				wf := s.Workflows["feature_change"]
				got := resolvedFor(t, ResolveAutonomy(&wf, nil), class)
				if got.Mode != mode {
					t.Errorf("%s mode = %q, want %q", class, got.Mode, mode)
				}
			})
		}
	}
}

// TestDerivedOperatorAgent_GroomingClassesDeriveNoKnob is the AC7/authority
// invariant: NO grooming class — including the auto-eligible one and a
// report-mode one — reaches a may_* knob. This is what makes shipping
// objective_reversible with no evaluator fail-CLOSED rather than a gap.
//
// COUNTERFACTUAL: add a `case ActionGroomHygiene:` arm to
// DerivedOperatorAgent's switch and this test goes RED.
func TestDerivedOperatorAgent_GroomingClassesDeriveNoKnob(t *testing.T) {
	s := mustParse(t, v2Doc(`    actions:
      hygiene:
        mode: auto
        when: objective_reversible
      scoping:
        mode: report
`))
	wf := s.Workflows["feature_change"]
	rm := ResolveAutonomy(&wf, nil)
	// Both grooming classes really are present in the resolved matrix — the
	// assertion below must not pass because the entries vanished.
	if got := resolvedFor(t, rm, ActionGroomHygiene); got.Mode != ModeAuto {
		t.Fatalf("hygiene mode = %q, want auto (the fixture must actually declare auto)", got.Mode)
	}
	if got := resolvedFor(t, rm, ActionGroomScoping); got.Mode != ModeReport {
		t.Fatalf("scoping mode = %q, want report", got.Mode)
	}
	derived := DerivedOperatorAgent(rm)
	if derived == nil {
		t.Fatal("DerivedOperatorAgent = nil, want a block (the workflow declares actions)")
	}
	if got := delegatedKnobs(derived); len(got) != 0 {
		t.Errorf("delegated knobs = %v, want NONE — no grooming class may reach a may_* knob at any mode", got)
	}
}

// TestClampResolvedMatrix_LowCeilingClampsGroomingHygiene is AC3: a fired
// escalation's `max_autonomy: low` CEILING downgrades hygiene from auto to
// gated (low's expansion does not name it — the documented extension-class
// fail-closed arm), while a report-mode class is left alone because it
// delegates nothing to begin with.
//
// COUNTERFACTUAL: delete `out.Actions[i].Mode = ModeGated` in
// ClampResolvedMatrix and this test goes RED.
func TestClampResolvedMatrix_LowCeilingClampsGroomingHygiene(t *testing.T) {
	s := mustParse(t, v2Doc(`    actions:
      hygiene:
        mode: auto
        when: objective_reversible
      scoping:
        mode: report
`))
	wf := s.Workflows["feature_change"]
	clamped := ClampResolvedMatrix(ResolveAutonomy(&wf, nil), TierLow)

	hygiene := resolvedFor(t, clamped, ActionGroomHygiene)
	if hygiene.Mode != ModeGated {
		t.Errorf("hygiene mode = %q after a low ceiling, want gated", hygiene.Mode)
	}
	if hygiene.Condition != "" {
		t.Errorf("hygiene condition = %q after the clamp, want it CLEARED", hygiene.Condition)
	}
	if hygiene.Source != SourceEscalation {
		t.Errorf("hygiene source = %q, want escalation — the clamp must record its own provenance", hygiene.Source)
	}

	scoping := resolvedFor(t, clamped, ActionGroomScoping)
	if scoping.Mode != ModeReport {
		t.Errorf("scoping mode = %q after the clamp, want report UNCHANGED — report delegates nothing, so the ceiling has nothing to close", scoping.Mode)
	}
	if scoping.Source == SourceEscalation {
		t.Error("scoping source = escalation, want it untouched by the clamp")
	}
}

// TestGroomingRegistry_DispositionCaptureDoesNotWidenDelegation is the
// DELEGATION-WIDENING PIN for E54.30 / #2843.
//
// #2843 ships a CAPTURE surface for per-entry grooming dispositions: an
// operator records a verdict against an individual grooming-report entry and it
// persists as an auditable fact. It edits no spec file and no autonomy-registry
// file, and it must not — capture is not delegation. This assertion is what
// makes that a machine-checked property rather than a claim in a PR body: the
// three destructive classes stay non-delegable and still draw the exact
// MsgNonDelegableGroomingClassAuto refusal at `mode: auto`.
//
// If a future change widens delegation to route a destructive class through the
// disposition surface at `mode: auto`, this reddens on the shipped message.
func TestGroomingRegistry_DispositionCaptureDoesNotWidenDelegation(t *testing.T) {
	for _, class := range []string{ActionGroomOrdering, ActionGroomDedup, ActionGroomScoping} {
		t.Run(class, func(t *testing.T) {
			if _, nonDelegable := nonDelegableGroomingClasses[class]; !nonDelegable {
				t.Fatalf("%q left nonDelegableGroomingClasses; #2843 captures an operator's verdict and must not widen what an agent may decide", class)
			}
			_, err := ParseBytes(groomingMatrixDoc(class,
				"        mode: auto\n        when: objective_reversible\n"))
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError still refusing mode: auto on %q", err, class)
			}
			if want := MsgNonDelegableGroomingClassAuto(class); ve.Message != want {
				t.Errorf("Message = %q,\nwant the unchanged non-delegable-class refusal %q", ve.Message, want)
			}
		})
	}
}
