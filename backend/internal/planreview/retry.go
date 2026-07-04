package planreview

import (
	"context"
	"fmt"
	"strconv"
)

// InferFunc runs one inference roll for a reviewer adapter. It returns the raw
// model response text (to be decoded into a verdict), the model name, the
// invocation's token usage, and any infer-stage error (a subprocess crash /
// non-zero exit / timeout, or an SDK transport failure). It is the seam
// DecodeVerdictRetrying re-rolls on a DECODE failure.
type InferFunc func(ctx context.Context) (responseText, modelName string, usage Usage, err error)

// DecodeVerdictRetrying invokes infer and decodes its response into a
// ReviewVerdict, re-rolling the reviewer (re-invoking inference for fresh model
// sampling) on a DECODE failure only — a structurally-malformed verdict body
// the strict-then-repair DecodeVerdict cannot parse (#901). It is the shared
// resilience loop reused by all three reviewer adapters (claudecode, codex,
// anthropic).
//
// An infer-stage error is NOT a decode failure: a crash / non-zero exit /
// timeout already had the adapter's own crash-retry applied (Client.Inference
// for the subprocess adapters), so re-rolling it here would compound the bound.
// Such an error is returned immediately, unchanged.
//
// The loop runs at most maxRetries+1 rolls. On a decode failure it re-rolls
// unless the budget is spent OR the context is already done (mirroring
// client.go's outer-cancellation guard so an expired/cancelled ctx never wastes
// a roll). When the budget is exhausted the LAST decode error is returned
// verbatim so the caller's *_review_failed diagnostic keeps its precise
// 'invalid character ...' cause. DecodeVerdict itself stays strict — no
// structural-JSON repair lives here.
//
// On success it returns the decoded verdict (with Usage left zero — the caller
// attaches usage from the returned Usage value AFTER the closed-set check), the
// model name, and the roll's usage.
func DecodeVerdictRetrying(ctx context.Context, maxRetries int, infer InferFunc) (ReviewVerdict, string, Usage, error) {
	maxAttempts := maxRetries + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var lastDecodeErr error
	var lastResponseText string
	for attempt := 1; ; attempt++ {
		responseText, modelName, usage, err := infer(ctx)
		if err != nil {
			// Infer-stage fault (crash / non-zero exit / timeout / transport):
			// NOT a decode failure. The adapter's own crash-retry already ran;
			// return verbatim without re-rolling.
			return ReviewVerdict{}, "", Usage{}, err
		}

		verdict, decodeErr := DecodeVerdict([]byte(responseText))
		if decodeErr == nil {
			return verdict, modelName, usage, nil
		}

		// Structurally-malformed verdict body. Re-roll for fresh sampling
		// unless the budget is spent or the ctx is already done.
		lastDecodeErr = decodeErr
		lastResponseText = responseText
		if attempt >= maxAttempts || ctx.Err() != nil {
			// Wrap the terminal decode error with a bounded, quoted snippet of
			// the raw model output so the operator-visible *_review_failed reason
			// is diagnosable without trace archaeology (#1576). The `%w` verb
			// keeps the decode cause unwrappable (errors.Is / errors.Unwrap) and
			// preserves the 'invalid character ...' text as a substring, so
			// callers that match on the cause are unaffected.
			return ReviewVerdict{}, "", Usage{}, fmt.Errorf("%w (raw output: %s)", lastDecodeErr, rawSnippet(lastResponseText, 200))
		}
	}
}

// rawSnippet returns a strconv.Quote-quoted, rune-bounded snippet of s for
// embedding in a diagnostic error. Quoting renders control characters and
// newlines safely inline; the content is truncated to at most max runes and a
// trailing "…" marker (inside the quotes) is appended when s was longer, so the
// resulting reason stays length-bounded regardless of how large the raw model
// output was.
func rawSnippet(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return strconv.Quote(s)
	}
	// strconv.Quote first, then splice the marker inside the closing quote so
	// the value reads as a truncated quoted string ("...…").
	quoted := strconv.Quote(string(runes[:max]))
	return quoted[:len(quoted)-1] + "…\""
}
