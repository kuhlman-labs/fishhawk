package planreview

import "context"

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
		if attempt >= maxAttempts || ctx.Err() != nil {
			return ReviewVerdict{}, "", Usage{}, lastDecodeErr
		}
	}
}
