package spec

import "testing"

// allPresets is the closed set the library ships.
var allPresets = []Preset{PresetLow, PresetMedium, PresetHigh}

// parsePreset loads and fully validates a preset (ParseBytes runs schema
// + semantic validation), returning the typed *Spec.
func parsePreset(t *testing.T, p Preset) *Spec {
	t.Helper()
	data, err := PresetBytes(p)
	if err != nil {
		t.Fatalf("PresetBytes(%q): %v", p, err)
	}
	s, err := ParseBytes(data)
	if err != nil {
		t.Fatalf("preset %q failed ParseBytes (schema + semantic): %v", p, err)
	}
	return s
}

// TestPresetsParseAndValidate is the drift-proof gate mirroring the CLI
// one: every mirrored preset must pass ParseBytes (schema + semantic)
// through the backend's embedded workflow-v1 schema. The same bytes
// validating through both the CLI and backend embed copies is the
// cross-boundary check that the docs/spec canonical and both mirrors
// stay in lockstep.
func TestPresetsParseAndValidate(t *testing.T) {
	for _, p := range allPresets {
		p := p
		t.Run(string(p), func(t *testing.T) {
			s := parsePreset(t, p)
			// Re-run the semantic pass explicitly for good measure.
			if err := Validate(s); err != nil {
				t.Fatalf("preset %q failed semantic Validate: %v", p, err)
			}
			if _, ok := s.Workflows["feature_change"]; !ok {
				t.Fatalf("preset %q has no feature_change workflow", p)
			}
		})
	}
}

// operatorAgentOf returns the feature_change workflow's operator_agent
// block from a parsed preset (nil if absent).
func operatorAgentOf(t *testing.T, s *Spec) *OperatorAgent {
	t.Helper()
	wf, ok := s.Workflows["feature_change"]
	if !ok {
		t.Fatal("no feature_change workflow")
	}
	return wf.OperatorAgent
}

// TestPresetOperatorAgentPerTier is the done-means assertion (per
// #1169): it pins the SHIPPED operator_agent knobs of each parsed
// preset, so a no-op / comment-only preset edit fails even though the
// scope path was touched.
func TestPresetOperatorAgentPerTier(t *testing.T) {
	t.Run("low has no operator_agent block", func(t *testing.T) {
		if oa := operatorAgentOf(t, parsePreset(t, PresetLow)); oa != nil {
			t.Fatalf("low preset must have no operator_agent block, got %+v", oa)
		}
	})

	t.Run("medium has three knobs + 7 page events, no waive/merge", func(t *testing.T) {
		oa := operatorAgentOf(t, parsePreset(t, PresetMedium))
		if oa == nil {
			t.Fatal("medium preset must have an operator_agent block")
		}
		if oa.MayApprove != ConditionCleanDualApproval {
			t.Errorf("may_approve = %q, want %q", oa.MayApprove, ConditionCleanDualApproval)
		}
		if oa.MayRouteFixup != ConditionConvergentConcerns {
			t.Errorf("may_route_fixup = %q, want %q", oa.MayRouteFixup, ConditionConvergentConcerns)
		}
		if oa.MayRetry != ConditionInfraFlake {
			t.Errorf("may_retry = %q, want %q", oa.MayRetry, ConditionInfraFlake)
		}
		if oa.MayWaive != "" {
			t.Errorf("medium must NOT set may_waive, got %q", oa.MayWaive)
		}
		if oa.MayMerge != "" {
			t.Errorf("medium must NOT set may_merge, got %q", oa.MayMerge)
		}
		assertPageEvents(t, oa)
	})

	t.Run("high adds may_waive and may_merge", func(t *testing.T) {
		oa := operatorAgentOf(t, parsePreset(t, PresetHigh))
		if oa == nil {
			t.Fatal("high preset must have an operator_agent block")
		}
		if oa.MayApprove != ConditionCleanDualApproval {
			t.Errorf("may_approve = %q, want %q", oa.MayApprove, ConditionCleanDualApproval)
		}
		if oa.MayRouteFixup != ConditionConvergentConcerns {
			t.Errorf("may_route_fixup = %q, want %q", oa.MayRouteFixup, ConditionConvergentConcerns)
		}
		if oa.MayRetry != ConditionInfraFlake {
			t.Errorf("may_retry = %q, want %q", oa.MayRetry, ConditionInfraFlake)
		}
		if oa.MayWaive != ConditionSoloLow {
			t.Errorf("may_waive = %q, want %q", oa.MayWaive, ConditionSoloLow)
		}
		if oa.MayMerge != ConditionGatesResolvedCIGreen {
			t.Errorf("may_merge = %q, want %q", oa.MayMerge, ConditionGatesResolvedCIGreen)
		}
		assertPageEvents(t, oa)
	})
}

// assertPageEvents pins the shared 7-event must_page_human list.
func assertPageEvents(t *testing.T, oa *OperatorAgent) {
	t.Helper()
	want := []string{
		"reviewer_reject", "plan_rejection", "scope_amendment",
		"budget_override", "policy_override", "exception_request",
		"requirement_arbitration",
	}
	if len(oa.MustPageHuman) != len(want) {
		t.Fatalf("must_page_human has %d events, want %d: %v", len(oa.MustPageHuman), len(want), oa.MustPageHuman)
	}
	for i, w := range want {
		if oa.MustPageHuman[i] != w {
			t.Errorf("must_page_human[%d] = %q, want %q", i, oa.MustPageHuman[i], w)
		}
	}
}

// TestPresetBytesUnknown covers the unknown-preset error branch.
func TestPresetBytesUnknown(t *testing.T) {
	if _, err := PresetBytes(Preset("bogus")); err == nil {
		t.Fatal("PresetBytes with unknown preset must error")
	}
}
