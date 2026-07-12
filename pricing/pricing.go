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
const AsOf = "2026-07-12"

// rate is the per-token price for a single model family, in US
// dollars. Vendors publish per-million-token prices; we store the
// per-token form so Cost is a plain multiply with no scaling magic
// at the call site.
//
// cacheReadPerToken / cacheWritePerToken price the prompt-cache
// portions of the input side separately (ADR-044 / #1343): a cache
// READ is the cache-served (cheaper) input, a cache WRITE is the
// cache-creation (premium) input. They are only consulted by
// CostWithCache; the flat Cost(model,in,out) path is unchanged and
// never touches them.
type rate struct {
	inputPerToken      float64
	outputPerToken     float64
	cacheReadPerToken  float64
	cacheWritePerToken float64
}

// perMillion converts a published $/1M-tokens price into the
// per-token rate stored in the table. Keeps the literals below
// readable as the numbers vendors actually publish. The cache rates
// are left zero — call perMillionCache when a family prices cache.
func perMillion(input, output float64) rate {
	return rate{
		inputPerToken:  input / 1_000_000,
		outputPerToken: output / 1_000_000,
	}
}

// perMillionCache converts published $/1M-tokens prices for all four
// dimensions — fresh input, output, cache read, cache write — into the
// per-token rates stored in the table. Used for every family in active
// use so cache-aware pricing (CostWithCache) has the read/write rates;
// perMillion stays for any family that does not price cache separately.
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
// Prices are published vendor list prices as of AsOf, in $/1M-tokens
// (input / output / cache-read / cache-write):
//
//	Opus          5   / 25 / 0.5  / 6.25   (Anthropic, non-fast tier)
//	Fable         10  / 50 / 1.0  / 12.5   (Anthropic, premium tier)
//	Sonnet        3   / 15 / 0.3  / 3.75   (Anthropic)
//	Haiku         1   / 5  / 0.1  / 1.25   (Anthropic)
//	gpt-5.5       5   / 30 / 0.5  / 5      (OpenAI standard short-context)
//	gpt-5.6-sol   5   / 30 / 0.5  / 6.25   (OpenAI 5.6 flagship)
//	gpt-5.6-terra 2.5 / 15 / 0.25 / 3.125  (OpenAI 5.6 balanced)
//	gpt-5.6-luna  1   / 6  / 0.1  / 1.25   (OpenAI 5.6 cost-optimized)
//
// The Anthropic cache rates are the 5-minute-TTL baseline (#1343):
// cache READ = 0.1x the family input rate, cache WRITE = 1.25x the
// family input rate — verified against the Anthropic prompt-caching
// pricing (the 1-hour-TTL 2x write tier is intentionally out of scope
// this slice). Fable follows the same convention at the premium tier
// ($10/1M input → 1.0 read / 12.5 write). The OpenAI cache READ is the
// published 90%-cached-input discount (0.1x input). gpt-5.5's cache
// WRITE is set to the input rate because that model charges no separate
// cache-write premium; the entire gpt-5.6 family (Sol / Terra / Luna)
// DID introduce the premium (1.25x input, same multiplier as Anthropic),
// so their cacheWrite carries the real rate. Either way the codex adapter
// maps cache writes to 0, so every gpt cacheWrite rate is effectively
// unexercised in the reviewer path. Note gpt-5.6-sol's headline
// input/output ($5/$30) matches gpt-5.5 but its cache WRITE differs
// (6.25 vs 5) because 5.6 added the write premium.
//
// Family-key convention: Anthropic keys are the tier stem (claude-opus,
// claude-fable) so every dated point release inherits the tier price.
// OpenAI's gpt-5.6 splits into distinctly-priced tiers, so each is keyed
// by its full tier id (gpt-5.6-sol / gpt-5.6-terra / gpt-5.6-luna) — a
// bare gpt-5.6 would mis-price the other two. Only gpt-5.6-terra is
// dispatched today (the codex reviewer); Sol and Luna are priced so a
// future model swap can't silently record $0.
var familyRates = map[string]rate{
	"claude-opus":   perMillionCache(5, 25, 0.5, 6.25),
	"claude-fable":  perMillionCache(10, 50, 1, 12.5),
	"claude-sonnet": perMillionCache(3, 15, 0.3, 3.75),
	"claude-haiku":  perMillionCache(1, 5, 0.1, 1.25),
	"gpt-5.5":       perMillionCache(5, 30, 0.5, 5),
	"gpt-5.6-sol":   perMillionCache(5, 30, 0.5, 6.25),
	"gpt-5.6-terra": perMillionCache(2.5, 15, 0.25, 3.125),
	"gpt-5.6-luna":  perMillionCache(1, 6, 0.1, 1.25),
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

// CostWithCache returns the estimated US-dollar cost of an invocation
// that consumed freshInput fresh (cache-exclusive) input tokens,
// cacheRead cache-served input tokens, cacheWrite cache-creation input
// tokens, and output output tokens under the named model, plus ok=true
// when the model id matched a known family (ADR-044 / #1343).
//
// It prices fresh input and output exactly as Cost — so
// CostWithCache(model, in, 0, 0, out) reduces to Cost(model, in, out) —
// and adds the cache portions at the family's separate cache-read and
// cache-write rates. An unknown or empty model id returns (0, false),
// the same fail-open contract as Cost. Each count is clamped at zero so
// a malformed usage block can never produce a negative ledger entry.
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
		float64(cacheRead)*r.cacheReadPerToken +
		float64(cacheWrite)*r.cacheWritePerToken +
		float64(output)*r.outputPerToken
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
