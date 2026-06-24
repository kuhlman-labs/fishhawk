// Package pricing is the single source of truth for translating
// agent token counts into an estimated US-dollar cost.
//
// Both the runner (which stamps an estimated cost on its GenAI
// observability spans) and the backend (which rolls a per-run cost
// total and feeds the spend-alert detector) call Cost so the two
// sides can never drift to different numbers for the same usage.
//
// The table is a point-in-time snapshot keyed by model family,
// covering the Anthropic (claude-*) and OpenAI (gpt-*) families in
// active use. Model vendors change published prices over time, so any
// value derived from this table is an ESTIMATE — see AsOf for the
// snapshot date. An unknown model id returns ok=false and a zero cost
// rather than a guess, so a future model the table doesn't know about
// is recorded at 0 (estimated) instead of silently mispriced or
// panicking.
package pricing

import "strings"

// AsOf is the date the price table was last reconciled against
// published vendor pricing. Surfaced alongside any rolled cost so
// consumers can label the figure as an estimate of a known vintage.
const AsOf = "2026-06-24"

// rate is the per-token price for a single model family, in US
// dollars. Vendors publish per-million-token prices; we store the
// per-token form so Cost is a plain multiply with no scaling magic
// at the call site.
//
// cacheReadPerToken / cacheWritePerToken price the cache-served input
// split (#1343): a cache READ (a prefix-cache hit) is far cheaper than
// fresh input, and a cache WRITE (the first ingestion that populates the
// cache) carries a modest premium over fresh input. CostWithCache prices
// each bucket separately; Cost (fresh input + output only) ignores them.
type rate struct {
	inputPerToken      float64
	outputPerToken     float64
	cacheReadPerToken  float64
	cacheWritePerToken float64
}

// perMillion converts a published $/1M-tokens input/output price into the
// per-token rate stored in the table, with NO cache pricing (cache rates
// stay zero). Retained for any family whose cache pricing is not modeled;
// the active families use perMillionCache.
func perMillion(input, output float64) rate {
	return rate{
		inputPerToken:  input / 1_000_000,
		outputPerToken: output / 1_000_000,
	}
}

// perMillionCache converts published $/1M-tokens prices — fresh input,
// output, cache read, and cache write — into the per-token rate stored in
// the table. Keeps the literals below readable as the numbers vendors
// actually publish.
func perMillionCache(input, output, cacheRead, cacheWrite float64) rate {
	return rate{
		inputPerToken:      input / 1_000_000,
		outputPerToken:     output / 1_000_000,
		cacheReadPerToken:  cacheRead / 1_000_000,
		cacheWritePerToken: cacheWrite / 1_000_000,
	}
}

// familyRates maps a model-id prefix to its rate. Model ids carry a
// family + version + date suffix (e.g. "claude-opus-4-8",
// "claude-sonnet-4-6-20260201", "gpt-5.5-2026xxxx"), so we match on
// the family prefix and let every dated point release inherit the
// family price. Longest-prefix wins (see Cost) so a more specific
// key can override a broader one if a future release reprices.
//
// Prices are published vendor list prices as of AsOf, in $/1M-tokens:
// Opus 5/25 (non-fast tier), Sonnet 3/15, Haiku 1/5 (Anthropic);
// gpt-5.5 5/30 (OpenAI standard short-context).
//
// Cache pricing (#1343), verified against live vendor pages on AsOf:
//   - Anthropic (claude-*): cache READ = 0.1x base input, cache WRITE =
//     1.25x base input at the 5-minute-TTL baseline (the 1-hour-TTL 2x
//     write tier is out of scope this slice). Source: the Claude prompt-
//     caching docs pricing-multiplier table.
//   - OpenAI (gpt-5.5): cache READ = the published $0.50/1M cached-input
//     rate = 0.1x base input (a 90% discount; automatic prompt caching).
//     OpenAI charges NO separate cache-write premium, so cache WRITE is
//     set to the base input rate (5/1M). The codex adapter maps cache
//     writes to 0, so this gpt-5.5 write rate is effectively unexercised
//     in the reviewer path — it is set to the truthful no-premium value
//     rather than left zero.
var familyRates = map[string]rate{
	"claude-opus":   perMillionCache(5, 25, 0.5, 6.25), // read 0.1x5, write 1.25x5
	"claude-sonnet": perMillionCache(3, 15, 0.3, 3.75), // read 0.1x3, write 1.25x3
	"claude-haiku":  perMillionCache(1, 5, 0.1, 1.25),  // read 0.1x1, write 1.25x1
	"gpt-5.5":       perMillionCache(5, 30, 0.5, 5),    // read $0.50 (0.1x5), write = input (no premium)
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

// CostWithCache returns the estimated US-dollar cost of an invocation that
// consumed freshInput cache-exclusive input tokens, cacheRead cache-hit
// tokens, cacheWrite cache-population tokens, and output output tokens
// under the named model (#1343). It prices the fresh-input and output sides
// exactly as Cost, then ADDS cacheRead at the family's cache-read rate and
// cacheWrite at the cache-write rate. ok=true when the model id matched a
// known family; an unknown or empty model id returns (0, false) so the
// caller records usage at zero estimated cost rather than guessing.
//
// CostWithCache(model, in, 0, 0, out) reduces EXACTLY to Cost(model, in,
// out): the two cache terms contribute nothing, so non-cache-aware pricing
// is unaffected (back-compat). Each count is clamped to zero independently
// so a malformed usage block can never produce a negative ledger entry.
func CostWithCache(model string, freshInput, cacheRead, cacheWrite, output int) (usd float64, ok bool) {
	r, ok := lookup(model)
	if !ok {
		return 0, false
	}
	if freshInput < 0 {
		freshInput = 0
	}
	if cacheRead < 0 {
		cacheRead = 0
	}
	if cacheWrite < 0 {
		cacheWrite = 0
	}
	if output < 0 {
		output = 0
	}
	usd = float64(freshInput)*r.inputPerToken +
		float64(output)*r.outputPerToken +
		float64(cacheRead)*r.cacheReadPerToken +
		float64(cacheWrite)*r.cacheWritePerToken
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
