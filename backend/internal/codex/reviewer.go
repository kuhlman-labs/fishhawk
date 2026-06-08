package codex

import (
	"context"
	"fmt"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
)

// validVerdicts is the closed set of acceptable verdict strings per ADR-027.
// It mirrors claudecode.validVerdicts so the Codex adapter enforces the same
// shape guarantee as the Claude adapters.
var validVerdicts = map[planreview.Verdict]struct{}{
	planreview.VerdictApprove:             {},
	planreview.VerdictApproveWithConcerns: {},
	planreview.VerdictReject:              {},
}

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
func (r *Reviewer) Review(ctx context.Context, promptText string) (*planreview.ReviewVerdict, string, error) {
	responseText, modelName, usage, err := r.client.Inference(ctx, promptText)
	if err != nil {
		return nil, "", fmt.Errorf("codex: inference failed: %w", err)
	}

	verdict, err := planreview.DecodeVerdict([]byte(responseText))
	if err != nil {
		return nil, "", fmt.Errorf("codex: decode verdict JSON: %w", err)
	}

	if _, ok := validVerdicts[verdict.Verdict]; !ok {
		return nil, "", fmt.Errorf("codex: unknown verdict %q", verdict.Verdict)
	}

	// Attach token usage from the JSONL stream (not the agent-decoded JSON) so
	// the server can record reviewer agent cost (#681). Usage.Known is false
	// when no `turn.completed` usage line appeared — the server then records
	// the cost at usd=0 rather than guessing.
	verdict.Usage = usage

	return &verdict, modelName, nil
}
