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

// Messages calls the Anthropic Messages API. systemText is placed in a
// system block with ephemeral cache_control; when empty, the system block is
// omitted. userText becomes the single user message. Returns the first text
// block from the response, the model name used, and the response's token
// usage (input/output) so the caller can attribute reviewer agent cost
// (#681). The SDK always returns a Usage block on a successful Messages
// call, so the token counts are authoritative on the happy path.
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
func (c *Client) Messages(ctx context.Context, systemText, userText string) (responseText, modelName string, inputTokens, outputTokens int, err error) {
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
		return "", "", 0, 0, err
	}
	inTok := int(msg.Usage.InputTokens)
	outTok := int(msg.Usage.OutputTokens)
	for _, block := range msg.Content {
		if block.Type == "text" {
			return block.Text, msg.Model, inTok, outTok, nil
		}
	}
	return "", msg.Model, inTok, outTok, nil
}
