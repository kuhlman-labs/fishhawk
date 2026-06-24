// Package anthropic provides an Anthropic SDK adapter that satisfies
// server.PlanReviewer. It is constructed in serve.go when
// FISHHAWKD_ANTHROPIC_API_KEY is set.
package anthropic

import (
	"context"
	"net/http"
	"time"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Config holds the settings needed to create an Anthropic API client.
type Config struct {
	APIKey    string
	Model     string
	MaxTokens int
	Timeout   time.Duration
	// Schema, when non-nil, is the JSON Schema (Draft 2020-12 object) the
	// Messages call constrains the model's response to via
	// OutputConfig.Format (#1324/#1326). It is per-construction so each caller
	// pins the right output shape: NewReviewer sets planreview.VerdictSchema()
	// and the agenteval judge's client sets agenteval.JudgeCardSchema(). When
	// nil the Messages request omits output_config entirely (unconstrained),
	// leaving the caller's decode path as the fallback.
	Schema map[string]any
}

// Client wraps the Anthropic SDK for Messages API calls.
type Client struct {
	inner     anthropicsdk.Client
	model     string
	maxTokens int
	schema    map[string]any
}

// NewClient constructs a Client from cfg. The HTTP timeout is applied at
// construction time via option.WithHTTPClient. Extra opts (e.g.
// option.WithBaseURL for tests) are applied after the defaults. cfg.Schema (when
// non-nil) is carried onto the Client and constrains every Messages response.
func NewClient(cfg Config, opts ...option.RequestOption) *Client {
	allOpts := append([]option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		option.WithHTTPClient(&http.Client{Timeout: cfg.Timeout}),
	}, opts...)
	return &Client{
		inner:     anthropicsdk.NewClient(allOpts...),
		model:     cfg.Model,
		maxTokens: cfg.MaxTokens,
		schema:    cfg.Schema,
	}
}

// Messages calls the Anthropic Messages API and returns the response text,
// model name, and the fresh input/output token counts. It is the
// cache-agnostic entry point retained verbatim for the agenteval
// MessageSender seam (backend/internal/agenteval/judge.go), whose interface
// mirrors this exact 5-return signature so an *anthropic.Client satisfies it
// without agenteval importing this package. It delegates to MessagesWithCache
// and drops the cache split — keeping the judge seam stable while the
// reviewer path (#1343) gets the cache counts from MessagesWithCache. See
// MessagesWithCache for the schema/caching semantics.
func (c *Client) Messages(ctx context.Context, systemText, userText string) (responseText, modelName string, inputTokens, outputTokens int, err error) {
	responseText, modelName, inputTokens, outputTokens, _, _, err = c.MessagesWithCache(ctx, systemText, userText)
	return responseText, modelName, inputTokens, outputTokens, err
}

// MessagesWithCache calls the Anthropic Messages API. systemText is placed in
// a system block with ephemeral cache_control; when empty, the system block is
// omitted. userText becomes the single user message. Returns the first text
// block from the response, the model name used, and the response's token
// usage (input/output plus the cache read/write split) so the caller can
// attribute reviewer agent cost (#681) and price cached tokens at their
// distinct rates (#1343). The SDK always returns a Usage block on a
// successful Messages call, so the token counts are authoritative on the
// happy path. The SDK's usage.input_tokens already EXCLUDES cache reads and
// writes — those arrive as the separate CacheReadInputTokens /
// CacheCreationInputTokens members surfaced here as cacheReadTokens /
// cacheWriteTokens — so the caller satisfies the normalized cache-exclusive
// Usage contract (#1010) with no boundary arithmetic.
//
// It is a SEPARATE method from Messages (rather than a signature change to
// Messages) deliberately: the agenteval judge depends on Messages' 5-return
// shape via its MessageSender interface, so widening Messages would ripple
// into the out-of-scope agenteval package (#1343 slice boundary). Keeping
// Messages as a thin delegator leaves that seam untouched.
//
// When c.schema is non-nil the request carries OutputConfig.Format =
// json_schema with that per-client schema (#1324/#1326): the Messages API
// constrains the model's response to the caller's schema source of truth, so the
// body arrives as schema-guaranteed JSON (NewReviewer pins
// planreview.VerdictSchema(); the agenteval judge's client pins
// agenteval.JudgeCardSchema()). The caller's decode path (e.g.
// planreview.DecodeVerdict's fence-strip + escape-repair, or the judge's
// parseJudgeCard) stays the documented FALLBACK for any non-constrained or error
// path. The SDK's Type field defaults to "json_schema", so only Schema is set.
//
// When c.schema is nil the OutputConfig is OMITTED entirely so the response is
// unconstrained — the caller's decode path is then the only validation.
func (c *Client) MessagesWithCache(ctx context.Context, systemText, userText string) (responseText, modelName string, inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int, err error) {
	params := anthropicsdk.MessageNewParams{
		Model:     c.model,
		MaxTokens: int64(c.maxTokens),
		Messages: []anthropicsdk.MessageParam{
			anthropicsdk.NewUserMessage(anthropicsdk.NewTextBlock(userText)),
		},
	}
	if c.schema != nil {
		params.OutputConfig = anthropicsdk.OutputConfigParam{
			Format: anthropicsdk.JSONOutputFormatParam{
				Schema: c.schema,
			},
		}
	}
	if systemText != "" {
		params.System = []anthropicsdk.TextBlockParam{{
			Text:         systemText,
			CacheControl: anthropicsdk.NewCacheControlEphemeralParam(),
		}}
	}

	msg, err := c.inner.Messages.New(ctx, params)
	if err != nil {
		return "", "", 0, 0, 0, 0, err
	}
	inTok := int(msg.Usage.InputTokens)
	outTok := int(msg.Usage.OutputTokens)
	cacheReadTok := int(msg.Usage.CacheReadInputTokens)
	cacheWriteTok := int(msg.Usage.CacheCreationInputTokens)
	for _, block := range msg.Content {
		if block.Type == "text" {
			return block.Text, msg.Model, inTok, outTok, cacheReadTok, cacheWriteTok, nil
		}
	}
	return "", msg.Model, inTok, outTok, cacheReadTok, cacheWriteTok, nil
}
