package pricing

import (
	"strings"
	"testing"
)

// fixtureNoDrift prices every family's reference id to match familyRates
// exactly (per-token). Includes a non-model key (sample_spec) and an
// unreferenced model to prove they are ignored. gpt-5.5 omits
// cache_creation_input_token_cost (LiteLLM leaves it null), matching reality.
const fixtureNoDrift = `{
  "sample_spec": {"note": "not a real model — must be ignored"},
  "claude-opus-4-7":   {"input_cost_per_token": 5e-6, "output_cost_per_token": 25e-6, "cache_read_input_token_cost": 0.5e-6, "cache_creation_input_token_cost": 6.25e-6},
  "claude-sonnet-4-6": {"input_cost_per_token": 3e-6, "output_cost_per_token": 15e-6, "cache_read_input_token_cost": 0.3e-6, "cache_creation_input_token_cost": 3.75e-6},
  "claude-haiku-4-5":  {"input_cost_per_token": 1e-6, "output_cost_per_token": 5e-6, "cache_read_input_token_cost": 0.1e-6, "cache_creation_input_token_cost": 1.25e-6},
  "gpt-5.5":           {"input_cost_per_token": 5e-6, "output_cost_per_token": 30e-6, "cache_read_input_token_cost": 0.5e-6},
  "some-unreferenced-model": {"input_cost_per_token": 9e-6, "output_cost_per_token": 9e-6}
}`

func TestCheckDrift_NoDrift(t *testing.T) {
	r, err := CheckDrift([]byte(fixtureNoDrift), "deadbeef", "2026-06-25T00:00:00Z")
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	// gpt-5.5 cache_write is priced by us (5/1M) but null in LiteLLM -> one
	// no_reference finding; everything else is within tolerance.
	for _, f := range r.Findings {
		if f.Family != "gpt-5.5" || f.Field != "cache_write" || f.Severity != SeverityNoReference {
			t.Errorf("unexpected finding on the no-drift fixture: %+v", f)
		}
	}
	if r.HighSeverity() {
		t.Error("HighSeverity() = true on the no-drift fixture, want false")
	}
	if r.HasDrift() {
		t.Error("HasDrift() = true on the no-drift fixture, want false (the only finding is a no_reference gap)")
	}
	if r.OKFields == 0 || r.CheckedFields == 0 {
		t.Errorf("CheckedFields=%d OKFields=%d, want both > 0", r.CheckedFields, r.OKFields)
	}
	if r.SourceSHA != "deadbeef" || r.PricingAsOf != AsOf {
		t.Errorf("provenance not stamped: sha=%q asOf=%q (AsOf=%q)", r.SourceSHA, r.PricingAsOf, AsOf)
	}
}

// fixtureDrift: opus input is +/- in the warn band (ours 5 vs 5.2 -> -3.85%,
// ours_lower) and opus output is in the high band (ours 25 vs 20 -> +25%,
// ours_higher). Other families match.
const fixtureDrift = `{
  "claude-opus-4-7":   {"input_cost_per_token": 5.2e-6, "output_cost_per_token": 20e-6, "cache_read_input_token_cost": 0.5e-6, "cache_creation_input_token_cost": 6.25e-6},
  "claude-sonnet-4-6": {"input_cost_per_token": 3e-6, "output_cost_per_token": 15e-6, "cache_read_input_token_cost": 0.3e-6, "cache_creation_input_token_cost": 3.75e-6},
  "claude-haiku-4-5":  {"input_cost_per_token": 1e-6, "output_cost_per_token": 5e-6, "cache_read_input_token_cost": 0.1e-6, "cache_creation_input_token_cost": 1.25e-6},
  "gpt-5.5":           {"input_cost_per_token": 5e-6, "output_cost_per_token": 30e-6, "cache_read_input_token_cost": 0.5e-6}
}`

func TestCheckDrift_WarnAndHigh(t *testing.T) {
	r, err := CheckDrift([]byte(fixtureDrift), "cafef00d", "2026-06-25T00:00:00Z")
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	byKey := map[string]DriftFinding{}
	for _, f := range r.Findings {
		byKey[f.Family+"/"+f.Field] = f
	}

	in, ok := byKey["claude-opus/input"]
	if !ok {
		t.Fatal("missing claude-opus/input finding")
	}
	if in.Severity != SeverityWarn {
		t.Errorf("opus input severity = %q, want warn (Δ=%.2f%%)", in.Severity, in.PctDelta)
	}
	if in.Direction != "ours_lower" {
		t.Errorf("opus input direction = %q, want ours_lower (under-billing)", in.Direction)
	}

	out, ok := byKey["claude-opus/output"]
	if !ok {
		t.Fatal("missing claude-opus/output finding")
	}
	if out.Severity != SeverityHigh {
		t.Errorf("opus output severity = %q, want high (Δ=%.2f%%)", out.Severity, out.PctDelta)
	}
	if out.Direction != "ours_higher" {
		t.Errorf("opus output direction = %q, want ours_higher (over-reporting)", out.Direction)
	}
	if !r.HighSeverity() {
		t.Error("HighSeverity() = false, want true (opus output is +25%)")
	}
	if !r.HasDrift() {
		t.Error("HasDrift() = false, want true (warn + high findings present)")
	}
	// A matching field (opus cache_read) must NOT appear as a finding.
	if _, present := byKey["claude-opus/cache_read"]; present {
		t.Error("opus cache_read (matching) should not be a finding")
	}
}

func TestCheckDrift_NoReferenceWhenIDAbsent(t *testing.T) {
	// Dataset prices only sonnet — opus/haiku/gpt-5.5 reference ids are absent.
	const onlySonnet = `{"claude-sonnet-4-6": {"input_cost_per_token": 3e-6, "output_cost_per_token": 15e-6, "cache_read_input_token_cost": 0.3e-6, "cache_creation_input_token_cost": 3.75e-6}}`
	r, err := CheckDrift([]byte(onlySonnet), "sha", "t")
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	got := map[string]bool{}
	for _, f := range r.Findings {
		if f.Severity == SeverityNoReference && f.Field == "*" {
			got[f.Family] = true
		}
	}
	for _, fam := range []string{"claude-opus", "claude-haiku", "gpt-5.5"} {
		if !got[fam] {
			t.Errorf("expected a whole-family no_reference finding for %q (id absent from dataset)", fam)
		}
	}
}

func TestCheckDrift_BadJSON(t *testing.T) {
	if _, err := CheckDrift([]byte("{not json"), "sha", "t"); err == nil {
		t.Error("CheckDrift on malformed JSON: err = nil, want a parse error")
	}
}

// TestDriftReferenceMapMatchesFamilies pins the operator-maintained reference
// map to familyRates: every family must have a reference, and the map must not
// reference a family that no longer exists. This catches a familyRates add/rename
// that forgets to update the drift reference (which would silently stop checking
// that family).
func TestDriftReferenceMapMatchesFamilies(t *testing.T) {
	for fam := range familyRates {
		if _, ok := familyToLiteLLM[fam]; !ok {
			t.Errorf("familyRates has %q but familyToLiteLLM has no reference id — add one (drift would skip it)", fam)
		}
	}
	for fam := range familyToLiteLLM {
		if _, ok := familyRates[fam]; !ok {
			t.Errorf("familyToLiteLLM references %q which is not a familyRates family — remove or fix it", fam)
		}
	}
}

func TestDriftReport_Markdown(t *testing.T) {
	r, _ := CheckDrift([]byte(fixtureDrift), "cafef00d", "2026-06-25T00:00:00Z")
	md := r.Markdown()
	for _, want := range []string{"Price drift-check", "high-severity", "claude-opus", "cafef00d", "ours_higher"} {
		if !strings.Contains(md, want) {
			t.Errorf("Markdown() missing %q\n---\n%s", want, md)
		}
	}
	// The no-drift report renders a clean verdict.
	clean, _ := CheckDrift([]byte(fixtureNoDrift), "sha", "t")
	if cm := clean.Markdown(); !strings.Contains(cm, "no drift") {
		t.Errorf("clean Markdown() should say 'no drift'\n---\n%s", cm)
	}
}
