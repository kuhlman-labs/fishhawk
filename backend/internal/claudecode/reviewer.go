package claudecode

import (
	"context"
	"fmt"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
)

// Reviewer implements server.PlanReviewer by shelling out to the `claude` CLI.
// It is the local-mode sibling of anthropic.Reviewer (#572): the SDK adapter
// calls the Messages API with a cached system/user split, whereas this adapter
// sends the full prompt as one `-p` argument to a subprocess — no prompt
// splitting or caching is available over the CLI.
type Reviewer struct {
	client *Client
}

// NewReviewer returns a Reviewer backed by a new Client constructed from cfg.
func NewReviewer(cfg Config) *Reviewer {
	return &Reviewer{client: NewClient(cfg)}
}

// SetMaxRetries overrides the retry budget on the underlying Client AFTER
// construction, assigning n directly and bypassing NewClient's zero->1
// normalisation. This is the explicit-override path the env wiring uses: Go
// cannot distinguish an unset field from an explicit 0, so NewClient always
// defaults a zero MaxRetries to 1 (production retries a transient crash once),
// and an operator who passes 0 to disable retry must route through this setter.
// A negative n is clamped to 0 (a single attempt, retry disabled). The caller
// owns the default — this mirrors the documented "set MaxRetries after
// NewClient" idiom on Config.MaxRetries.
func (r *Reviewer) SetMaxRetries(n int) {
	if n < 0 {
		n = 0
	}
	r.client.cfg.MaxRetries = n
}

// Review invokes the `claude` CLI with the full promptText, JSON-decodes the
// envelope's result text into a ReviewVerdict, and validates the verdict
// belongs to the closed set. Subprocess failure, non-JSON output, and an
// unknown verdict each map to a precise wrapped error.
//
// Inference + decode route through planreview.DecodeVerdictRetrying so a
// structurally-malformed verdict body (#901) re-rolls the reviewer for fresh
// sampling, bounded by the existing crash-retry budget (r.client.cfg.MaxRetries
// — no new field). An inference fault is NOT re-rolled (Client.Inference already
// applied its own crash-retry); the inferFailed flag routes it to the
// "inference failed" wrapping while a decode failure keeps the "decode verdict
// JSON" wrapping.
func (r *Reviewer) Review(ctx context.Context, promptText string) (*planreview.ReviewVerdict, string, error) {
	var inferFailed bool
	infer := func(ctx context.Context) (string, string, planreview.Usage, error) {
		responseText, modelName, usage, err := r.client.Inference(ctx, promptText)
		if err != nil {
			inferFailed = true
			return "", "", planreview.Usage{}, fmt.Errorf("claudecode: inference failed: %w", err)
		}
		return responseText, modelName, usage, nil
	}

	verdict, modelName, usage, err := planreview.DecodeVerdictRetrying(ctx, r.client.cfg.MaxRetries, infer)
	if err != nil {
		if inferFailed {
			return nil, "", err
		}
		return nil, "", fmt.Errorf("claudecode: decode verdict JSON: %w", err)
	}

	if !verdict.Verdict.Valid() {
		return nil, "", fmt.Errorf("claudecode: unknown verdict %q", verdict.Verdict)
	}

	// Attach token usage from the CLI envelope (not the agent-decoded JSON)
	// so the server can record reviewer agent cost (#681). Usage.Known is
	// false when the envelope carried no `usage` object — the server then
	// records the cost at usd=0 rather than guessing.
	verdict.Usage = usage

	return &verdict, modelName, nil
}
