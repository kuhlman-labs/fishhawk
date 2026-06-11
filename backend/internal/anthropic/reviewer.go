package anthropic

import (
	"context"
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

	// maxDecodeRetries bounds the Reviewer-layer decode-retry (#901): a roll
	// whose 200-response body is structurally-malformed verdict JSON re-rolls
	// the Messages call for fresh sampling, up to maxDecodeRetries+1 rolls. The
	// anthropic-sdk-go client does NOT retry a successful 200 with a malformed
	// body (its built-in retry covers only 408/409/429/5xx + connection
	// errors), so this resilience must live at the Reviewer layer. Unlike the
	// subprocess adapters there is no Client.MaxRetries crash budget to reuse,
	// so it is a dedicated field; NewReviewer defaults it to 1 (matching the
	// clients' zero->1 normalisation) and SetMaxRetries overrides it.
	maxDecodeRetries int
}

// NewReviewer returns a Reviewer backed by a new Client constructed from cfg.
// Extra opts (e.g. option.WithBaseURL for tests) are forwarded to the client.
// The decode-retry budget defaults to 1; serve.go overrides it via
// SetMaxRetries with the env-resolved FISHHAWKD_PLAN_REVIEW_MAX_RETRIES value.
func NewReviewer(cfg Config, opts ...option.RequestOption) *Reviewer {
	return &Reviewer{client: NewClient(cfg, opts...), maxDecodeRetries: 1}
}

// SetMaxRetries overrides the Reviewer-layer decode-retry budget, clamping a
// negative n to 0 (a single attempt, retry disabled). It mirrors the
// claudecode/codex setter surface serve.go drives so all three adapters honour
// the same env-resolved budget. (It governs the decode re-roll, not the SDK's
// own transport retry, which is configured separately at client construction.)
func (r *Reviewer) SetMaxRetries(n int) {
	if n < 0 {
		n = 0
	}
	r.maxDecodeRetries = n
}

// MaxDecodeRetries returns the configured decode-retry budget. It exists for
// test observability of the serve.go forwarding seam.
func (r *Reviewer) MaxDecodeRetries() int {
	return r.maxDecodeRetries
}

// Review invokes the Anthropic API with promptText, splits the prompt at
// prompt.PlanReviewSplitMarker for caching, JSON-decodes the response into a
// ReviewVerdict, and validates the verdict belongs to the closed set.
//
// Messages + decode route through planreview.DecodeVerdictRetrying so a
// structurally-malformed verdict body (#901) re-rolls the Messages call for
// fresh sampling, bounded by r.maxDecodeRetries. A Messages transport fault is
// NOT re-rolled here (the SDK already applied its own transport retry); the
// inferFailed flag routes it to the "messages call failed" wrapping while a
// decode failure keeps the "decode verdict JSON" wrapping.
func (r *Reviewer) Review(ctx context.Context, promptText string) (*planreview.ReviewVerdict, string, error) {
	systemText, userText := splitPrompt(promptText)

	var inferFailed bool
	infer := func(ctx context.Context) (string, string, planreview.Usage, error) {
		responseText, modelName, inputTokens, outputTokens, err := r.client.Messages(ctx, systemText, userText)
		if err != nil {
			inferFailed = true
			return "", "", planreview.Usage{}, fmt.Errorf("anthropic: messages call failed: %w", err)
		}
		// Attach token usage from the SDK envelope (not the agent-decoded
		// JSON) so the server can record reviewer agent cost (#681). The SDK
		// returns a Usage block on every successful Messages call, so Known is
		// true here. The Messages API usage.input_tokens already EXCLUDES
		// cache reads/writes (they arrive as separate cache_read_input_tokens
		// / cache_creation_input_tokens fields), so this adapter satisfies the
		// normalized cache-exclusive Usage contract (#1010) as-is;
		// CachedInputTokens stays 0 until the Messages client surfaces the
		// cache fields.
		return responseText, modelName, planreview.Usage{
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			Turns:        1, // single Messages call: exactly one turn
			Known:        true,
		}, nil
	}

	verdict, modelName, usage, err := planreview.DecodeVerdictRetrying(ctx, r.maxDecodeRetries, infer)
	if err != nil {
		if inferFailed {
			return nil, "", err
		}
		return nil, "", fmt.Errorf("anthropic: decode verdict JSON: %w", err)
	}

	if _, ok := validVerdicts[verdict.Verdict]; !ok {
		return nil, "", fmt.Errorf("anthropic: unknown verdict %q", verdict.Verdict)
	}

	verdict.Usage = usage

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
