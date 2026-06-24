package cost

import (
	"testing"

	"github.com/kuhlman-labs/fishhawk/pricing"
)

func TestFromManifest_KnownModel(t *testing.T) {
	// claude-opus list price is 5/25 $/1M tokens (see pricing table).
	// 1,000,000 input + 1,000,000 output → $5 + $25 = $30.
	got := FromManifest("claude-opus-4-8", 1_000_000, 1_000_000)
	if !got.KnownModel {
		t.Fatalf("KnownModel = false, want true for a known model id")
	}
	if got.USD != 30 {
		t.Errorf("USD = %v, want 30", got.USD)
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

// TestFromManifestWithCache_PricesCacheSplit pins the load-bearing #1349
// behavior: cache reads are priced at the family DISCOUNT and cache writes at
// the PREMIUM — distinct line items, NOT the flat input rate. claude-opus
// rates: input 5, output 25, cache-read 0.5, cache-write 6.25 $/1M.
//
//	1M fresh input  → $5
//	1M cache read   → $0.5  (discount, not $5)
//	1M cache write  → $6.25 (premium, not $5)
//	1M output       → $25
//	total           = $36.75
func TestFromManifestWithCache_PricesCacheSplit(t *testing.T) {
	got := FromManifestWithCache("claude-opus-4-8", 1_000_000, 1_000_000, 1_000_000, 1_000_000)
	if !got.KnownModel {
		t.Fatalf("KnownModel = false, want true for a known model id")
	}
	if got.USD != 36.75 {
		t.Errorf("USD = %v, want 36.75 (read at discount, write at premium)", got.USD)
	}
	// The split must be carried on the Record (read != write line item).
	if got.CacheReadInputTokens != 1_000_000 || got.CacheWriteInputTokens != 1_000_000 {
		t.Errorf("cache buckets = (read %d, write %d), want both 1,000,000",
			got.CacheReadInputTokens, got.CacheWriteInputTokens)
	}
	if got.InputTokens != 1_000_000 || got.OutputTokens != 1_000_000 {
		t.Errorf("fresh/output = (%d,%d), want both 1,000,000", got.InputTokens, got.OutputTokens)
	}
	// Falsifier: if cache were priced at the flat input rate, USD would be
	// 5 + 5 + 5 + 25 = 40, not 36.75 — the discount/premium are distinct.
	flat, _ := pricing.Cost("claude-opus-4-8", 3_000_000, 1_000_000) // (fresh+read+write) at input rate
	if got.USD == flat {
		t.Errorf("cache priced at the flat input rate (USD=%v); read discount / write premium not applied", got.USD)
	}
}

// TestFromManifestWithCache_ReducesToFromManifest pins the reduction contract:
// FromManifestWithCache(model, in, 0, 0, out) is byte-for-byte FromManifest's
// USD — a no-cache bundle is priced identically, so the non-cache-aware
// FromManifest caller is unaffected (#1349).
func TestFromManifestWithCache_ReducesToFromManifest(t *testing.T) {
	flat := FromManifest("claude-sonnet-4-6", 1234, 5678)
	cached := FromManifestWithCache("claude-sonnet-4-6", 1234, 0, 0, 5678)
	if cached.USD != flat.USD {
		t.Errorf("USD = %v, want %v (no-cache must reduce to FromManifest)", cached.USD, flat.USD)
	}
	if cached.CacheReadInputTokens != 0 || cached.CacheWriteInputTokens != 0 {
		t.Errorf("cache buckets = (read %d, write %d), want both 0",
			cached.CacheReadInputTokens, cached.CacheWriteInputTokens)
	}
	if !cached.KnownModel {
		t.Error("KnownModel = false, want true")
	}
}

// TestFromManifestWithCache_UnknownModel pins the fail-open contract: an
// unknown model id yields USD=0 with KnownModel=false, never a guessed figure
// (#1349, symmetric with FromManifest).
func TestFromManifestWithCache_UnknownModel(t *testing.T) {
	got := FromManifestWithCache("some-unrecognized-model", 1000, 1000, 1000, 1000)
	if got.KnownModel {
		t.Errorf("KnownModel = true, want false for an unknown model")
	}
	if got.USD != 0 {
		t.Errorf("USD = %v, want 0 (unknown model recorded unpriced, not guessed)", got.USD)
	}
	if got.PricingAsOf != pricing.AsOf {
		t.Errorf("PricingAsOf = %q, want %q even for unknown model", got.PricingAsOf, pricing.AsOf)
	}
}
