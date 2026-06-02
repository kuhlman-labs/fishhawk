// Package pricing is the single source of truth for translating
// agent token counts into an estimated US-dollar cost.
//
// Both the runner (which stamps an estimated cost on its GenAI
// observability spans) and the backend (which rolls a per-run cost
// total and feeds the spend-alert detector) call Cost so the two
// sides can never drift to different numbers for the same usage.
//
// The table is a point-in-time snapshot keyed by model family. Model
// vendors change published prices over time, so any value derived
// from this table is an ESTIMATE — see AsOf for the snapshot date.
// An unknown model id returns ok=false and a zero cost rather than a
// guess, so a future model the table doesn't know about is recorded
// at 0 (estimated) instead of silently mispriced or panicking.
package pricing

import "strings"

// AsOf is the date the price table was last reconciled against
// published vendor pricing. Surfaced alongside any rolled cost so
// consumers can label the figure as an estimate of a known vintage.
const AsOf = "2026-06-02"

// rate is the per-token price for a single model family, in US
// dollars. Vendors publish per-million-token prices; we store the
// per-token form so Cost is a plain multiply with no scaling magic
// at the call site.
type rate struct {
	inputPerToken  float64
	outputPerToken float64
}

// perMillion converts a published $/1M-tokens price into the
// per-token rate stored in the table. Keeps the literals below
// readable as the numbers vendors actually publish.
func perMillion(input, output float64) rate {
	return rate{
		inputPerToken:  input / 1_000_000,
		outputPerToken: output / 1_000_000,
	}
}

// familyRates maps a model-id prefix to its rate. Claude model ids
// carry a family + version + date suffix (e.g.
// "claude-opus-4-8", "claude-sonnet-4-6-20260201"), so we match on
// the family prefix and let every dated point release inherit the
// family price. Longest-prefix wins (see Cost) so a more specific
// key can override a broader one if a future release reprices.
//
// Prices are published Anthropic list prices as of AsOf, in
// $/1M-tokens: Opus 15/75, Sonnet 3/15, Haiku 0.80/4.
var familyRates = map[string]rate{
	"claude-opus":   perMillion(15, 75),
	"claude-sonnet": perMillion(3, 15),
	"claude-haiku":  perMillion(0.80, 4),
}

// Cost returns the estimated US-dollar cost of an invocation that
// consumed inputTokens input and outputTokens output tokens under
// the named model, plus ok=true when the model id matched a known
// family. An unknown or empty model id returns (0, false): the
// caller records the usage at zero estimated cost rather than
// guessing. Negative token counts are clamped to zero so a malformed
// usage block can never produce a negative ledger entry.
func Cost(model string, inputTokens, outputTokens int) (usd float64, ok bool) {
	r, ok := lookup(model)
	if !ok {
		return 0, false
	}
	if inputTokens < 0 {
		inputTokens = 0
	}
	if outputTokens < 0 {
		outputTokens = 0
	}
	usd = float64(inputTokens)*r.inputPerToken + float64(outputTokens)*r.outputPerToken
	return usd, true
}

// lookup resolves a model id to its rate by longest matching family
// prefix. Exact-length prefix ties can't happen (map keys are
// distinct), so the only ambiguity is nesting — handled by keeping
// the longest matching key.
func lookup(model string) (rate, bool) {
	var (
		best    rate
		bestLen = -1
	)
	for prefix, r := range familyRates {
		if strings.HasPrefix(model, prefix) && len(prefix) > bestLen {
			best, bestLen = r, len(prefix)
		}
	}
	if bestLen < 0 {
		return rate{}, false
	}
	return best, true
}
