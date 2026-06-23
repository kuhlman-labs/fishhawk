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

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
)

// Config holds the settings needed to create an Anthropic API client.
type Config struct {
	APIKey    string
	Model     string
	MaxTokens int
	Timeout   time.Duration
}

// Client wraps the Anthropic SDK for Messages API calls.
type Client struct {
	inner     anthropicsdk.Client
	model     string
	maxTokens int
}

// NewClient constructs a Client from cfg. The HTTP timeout is applied at
// construction time via option.WithHTTPClient. Extra opts (e.g.
// option.WithBaseURL for tests) are applied after the defaults.
func NewClient(cfg Config, opts ...option.RequestOption) *Client {
	allOpts := append([]option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		option.WithHTTPClient(&http.Client{Timeout: cfg.Timeout}),
	}, opts...)
	return &Client{
		inner:     anthropicsdk.NewClient(allOpts...),
		model:     cfg.Model,
		maxTokens: cfg.MaxTokens,
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
// The request carries OutputConfig.Format = json_schema with
// planreview.VerdictSchema() (#1324): the Messages API constrains the model's
// response to the single ReviewVerdict schema source of truth, so the verdict
// body arrives as schema-guaranteed JSON. planreview.DecodeVerdict's
// fence-strip + escape-repair stays the documented FALLBACK for any
// non-constrained or error path. The SDK's Type field defaults to
// "json_schema", so only Schema is set here.
func (c *Client) Messages(ctx context.Context, systemText, userText string) (responseText, modelName string, inputTokens, outputTokens int, err error) {
	params := anthropicsdk.MessageNewParams{
		Model:     c.model,
		MaxTokens: int64(c.maxTokens),
		Messages: []anthropicsdk.MessageParam{
			anthropicsdk.NewUserMessage(anthropicsdk.NewTextBlock(userText)),
		},
		OutputConfig: anthropicsdk.OutputConfigParam{
			Format: anthropicsdk.JSONOutputFormatParam{
				Schema: planreview.VerdictSchema(),
			},
		},
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
