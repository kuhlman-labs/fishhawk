package claudecode

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
)

// validVerdicts is the closed set of acceptable verdict strings per ADR-027.
// It mirrors anthropic.validVerdicts so the local-mode adapter enforces the
// same shape guarantee as the SDK adapter.
var validVerdicts = map[planreview.Verdict]struct{}{
	planreview.VerdictApprove:             {},
	planreview.VerdictApproveWithConcerns: {},
	planreview.VerdictReject:              {},
}

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

// Review invokes the `claude` CLI with the full promptText, JSON-decodes the
// envelope's result text into a ReviewVerdict, and validates the verdict
// belongs to the closed set. Subprocess failure, non-JSON output, and an
// unknown verdict each map to a precise wrapped error.
func (r *Reviewer) Review(ctx context.Context, promptText string) (*planreview.ReviewVerdict, string, error) {
	responseText, modelName, err := r.client.Inference(ctx, promptText)
	if err != nil {
		return nil, "", fmt.Errorf("claudecode: inference failed: %w", err)
	}

	var verdict planreview.ReviewVerdict
	if err := json.Unmarshal([]byte(responseText), &verdict); err != nil {
		return nil, "", fmt.Errorf("claudecode: decode verdict JSON: %w", err)
	}

	if _, ok := validVerdicts[verdict.Verdict]; !ok {
		return nil, "", fmt.Errorf("claudecode: unknown verdict %q", verdict.Verdict)
	}

	return &verdict, modelName, nil
}
