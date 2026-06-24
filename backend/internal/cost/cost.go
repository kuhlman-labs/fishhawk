// Package cost rolls the estimated US-dollar cost of a run's model
// usage from the signed trace-bundle manifest's token counts.
//
// The backend computes cost from the manifest (authoritative,
// control-plane-side) rather than trusting a runner-emitted span cost
// attribute: a dropped or tampered span can't corrupt the ledger.
// Both the runner's GenAI span and this rollup call the shared
// `pricing` table, so the two figures can never drift for the same
// usage.
package cost

import "github.com/kuhlman-labs/fishhawk/pricing"

// Record is the estimated cost of one bundle's worth of model usage.
type Record struct {
	// Model is the resolved agent model id from the manifest, e.g.
	// "claude-opus-4-8". Empty when the runner didn't stamp one.
	Model string
	// InputTokens / OutputTokens are the agent-reported token split.
	// InputTokens is the FRESH (cache-exclusive) input when the record came
	// from FromManifestWithCache; the flat FromManifest path leaves the cache
	// fields zero and InputTokens carries the whole input side.
	InputTokens  int
	OutputTokens int
	// CacheReadInputTokens / CacheWriteInputTokens are the prompt-cache split
	// of the input side (ADR-044 / #1349): cache-served reads (priced at the
	// family discount) and cache-creation writes (priced at the premium). Zero
	// on a FromManifest record and on any bundle without cache usage.
	CacheReadInputTokens  int
	CacheWriteInputTokens int
	// USD is the estimated cost. Always >= 0; 0 for an unknown model.
	USD float64
	// KnownModel reports whether pricing recognized Model. False means
	// the cost is recorded at 0 (unpriced) rather than guessed, so a
	// model the table doesn't know about is surfaced honestly instead
	// of silently mispriced.
	KnownModel bool
	// PricingAsOf is the price-table snapshot date (pricing.AsOf), so
	// every recorded figure carries its vintage and reads as an
	// estimate of a known point in time.
	PricingAsOf string
}

// FromManifest computes the estimated cost of a bundle's model usage
// from its manifest token counts. It never errors: an unknown or
// empty model id yields USD=0 with KnownModel=false.
func FromManifest(model string, inputTokens, outputTokens int) Record {
	usd, ok := pricing.Cost(model, inputTokens, outputTokens)
	return Record{
		Model:        model,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		USD:          usd,
		KnownModel:   ok,
		PricingAsOf:  pricing.AsOf,
	}
}

// FromManifestWithCache computes the estimated cost of a bundle's model
// usage when the manifest carries the prompt-cache split (ADR-044 / #1349):
// freshInput fresh (cache-exclusive) input tokens, cacheRead cache-served
// input tokens, cacheWrite cache-creation input tokens, and outputTokens
// output. It prices via pricing.CostWithCache — fresh input + output at the
// flat rates, plus cache read at the family discount and cache write at the
// premium — and populates all four token buckets on the Record.
//
// Like FromManifest it never errors: an unknown or empty model id yields
// USD=0 with KnownModel=false. Because pricing.CostWithCache(model, in, 0, 0,
// out) reduces exactly to pricing.Cost(model, in, out),
// FromManifestWithCache(model, in, 0, 0, out) reduces exactly to
// FromManifest(model, in, out) — so a cache-less bundle is priced identically
// and non-cache-aware callers can stay on FromManifest unchanged.
func FromManifestWithCache(model string, freshInput, cacheRead, cacheWrite, outputTokens int) Record {
	usd, ok := pricing.CostWithCache(model, freshInput, cacheRead, cacheWrite, outputTokens)
	return Record{
		Model:                 model,
		InputTokens:           freshInput,
		OutputTokens:          outputTokens,
		CacheReadInputTokens:  cacheRead,
		CacheWriteInputTokens: cacheWrite,
		USD:                   usd,
		KnownModel:            ok,
		PricingAsOf:           pricing.AsOf,
	}
}
