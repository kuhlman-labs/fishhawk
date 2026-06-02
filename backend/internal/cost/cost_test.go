package cost

import (
	"testing"

	"github.com/kuhlman-labs/fishhawk/pricing"
)

func TestFromManifest_KnownModel(t *testing.T) {
	// claude-opus list price is 15/75 $/1M tokens (see pricing table).
	// 1,000,000 input + 1,000,000 output → $15 + $75 = $90.
	got := FromManifest("claude-opus-4-8", 1_000_000, 1_000_000)
	if !got.KnownModel {
		t.Fatalf("KnownModel = false, want true for a known model id")
	}
	if got.USD != 90 {
		t.Errorf("USD = %v, want 90", got.USD)
	}
	if got.Model != "claude-opus-4-8" || got.InputTokens != 1_000_000 || got.OutputTokens != 1_000_000 {
		t.Errorf("echoed fields wrong: %+v", got)
	}
	if got.PricingAsOf != pricing.AsOf {
		t.Errorf("PricingAsOf = %q, want %q", got.PricingAsOf, pricing.AsOf)
	}
}

func TestFromManifest_AgreesWithPricing(t *testing.T) {
	// The rollup must never drift from pricing.Cost — it is the same
	// source of truth the runner span uses.
	want, ok := pricing.Cost("claude-sonnet-4-6", 1234, 5678)
	if !ok {
		t.Fatal("pricing.Cost returned ok=false for a known model")
	}
	got := FromManifest("claude-sonnet-4-6", 1234, 5678)
	if got.USD != want {
		t.Errorf("USD = %v, want %v (must match pricing.Cost)", got.USD, want)
	}
	if !got.KnownModel {
		t.Errorf("KnownModel = false, want true")
	}
}

func TestFromManifest_UnknownModel(t *testing.T) {
	got := FromManifest("some-unrecognized-model", 1000, 1000)
	if got.KnownModel {
		t.Errorf("KnownModel = true, want false for an unknown model")
	}
	if got.USD != 0 {
		t.Errorf("USD = %v, want 0 (unknown model is recorded unpriced, not guessed)", got.USD)
	}
	if got.PricingAsOf != pricing.AsOf {
		t.Errorf("PricingAsOf = %q, want %q even for unknown model", got.PricingAsOf, pricing.AsOf)
	}
}

func TestFromManifest_EmptyModel(t *testing.T) {
	got := FromManifest("", 500, 500)
	if got.KnownModel || got.USD != 0 {
		t.Errorf("empty model: got KnownModel=%v USD=%v, want false/0", got.KnownModel, got.USD)
	}
}
