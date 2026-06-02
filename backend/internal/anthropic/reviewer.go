package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
)

// validVerdicts is the closed set of acceptable verdict strings per ADR-027.
var validVerdicts = map[planreview.Verdict]struct{}{
	planreview.VerdictApprove:             {},
	planreview.VerdictApproveWithConcerns: {},
	planreview.VerdictReject:              {},
}

// Reviewer implements server.PlanReviewer by calling the Anthropic Messages
// API. It splits the prompt at prompt.PlanReviewSplitMarker so the stable
// role-constraint preamble is placed in the cached system block and the
// variable plan artifact + issue content is placed in the user message.
type Reviewer struct {
	client *Client
}

// NewReviewer returns a Reviewer backed by a new Client constructed from cfg.
// Extra opts (e.g. option.WithBaseURL for tests) are forwarded to the client.
func NewReviewer(cfg Config, opts ...option.RequestOption) *Reviewer {
	return &Reviewer{client: NewClient(cfg, opts...)}
}

// Review invokes the Anthropic API with promptText, splits the prompt at
// prompt.PlanReviewSplitMarker for caching, JSON-decodes the response into a
// ReviewVerdict, and validates the verdict belongs to the closed set.
func (r *Reviewer) Review(ctx context.Context, promptText string) (*planreview.ReviewVerdict, string, error) {
	systemText, userText := splitPrompt(promptText)

	responseText, modelName, inputTokens, outputTokens, err := r.client.Messages(ctx, systemText, userText)
	if err != nil {
		return nil, "", fmt.Errorf("anthropic: messages call failed: %w", err)
	}

	var verdict planreview.ReviewVerdict
	if err := json.Unmarshal([]byte(responseText), &verdict); err != nil {
		return nil, "", fmt.Errorf("anthropic: decode verdict JSON: %w", err)
	}

	if _, ok := validVerdicts[verdict.Verdict]; !ok {
		return nil, "", fmt.Errorf("anthropic: unknown verdict %q", verdict.Verdict)
	}

	// Attach token usage from the SDK envelope (not the agent-decoded JSON)
	// so the server can record reviewer agent cost (#681). The SDK returns a
	// Usage block on every successful Messages call, so Known is true here.
	verdict.Usage = planreview.Usage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		Known:        true,
	}

	return &verdict, modelName, nil
}

// splitPrompt splits promptText at prompt.PlanReviewSplitMarker. The text
// before the marker becomes the system block (stable preamble); everything
// from the marker onward becomes the user message. Falls back to placing the
// full text in the user message when the marker is absent.
func splitPrompt(promptText string) (systemText, userText string) {
	idx := strings.Index(promptText, prompt.PlanReviewSplitMarker)
	if idx < 0 {
		return "", promptText
	}
	return promptText[:idx], promptText[idx:]
}
