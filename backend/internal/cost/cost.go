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
	InputTokens  int
	OutputTokens int
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
