package codex

import (
	"context"
	"fmt"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
)

// Reviewer implements server.PlanReviewer by shelling out to the `codex` CLI.
// It is the Codex sibling of claudecode.Reviewer (#575): structurally identical,
// it sends the full prompt as one positional argument to a `codex exec`
// subprocess and decodes the agent-message verdict body the CLI emits.
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
// A negative n is clamped to 0 (a single attempt, retry disabled). Mirrors the
// claudecode.Reviewer surface serve.go drives.
func (r *Reviewer) SetMaxRetries(n int) {
	if n < 0 {
		n = 0
	}
	r.client.cfg.MaxRetries = n
}

// Review invokes the `codex` CLI with the full promptText, decodes the
// agent-message verdict body into a ReviewVerdict via planreview.DecodeVerdict
// (reusing the #739 fence-strip + invalid-escape tolerance), and validates the
// verdict belongs to the closed set. Subprocess failure, an undecodable verdict
// body, and an unknown verdict each map to a precise wrapped error.
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
			return "", "", planreview.Usage{}, fmt.Errorf("codex: inference failed: %w", err)
		}
		return responseText, modelName, usage, nil
	}

	verdict, modelName, usage, err := planreview.DecodeVerdictRetrying(ctx, r.client.cfg.MaxRetries, infer)
	if err != nil {
		if inferFailed {
			return nil, "", err
		}
		return nil, "", fmt.Errorf("codex: decode verdict JSON: %w", err)
	}

	if !verdict.Verdict.Valid() {
		return nil, "", fmt.Errorf("codex: unknown verdict %q", verdict.Verdict)
	}

	// Attach token usage from the JSONL stream (not the agent-decoded JSON) so
	// the server can record reviewer agent cost (#681). Usage.Known is false
	// when no `turn.completed` usage line appeared — the server then records
	// the cost at usd=0 rather than guessing.
	verdict.Usage = usage

	return &verdict, modelName, nil
}
