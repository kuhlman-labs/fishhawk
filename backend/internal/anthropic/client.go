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
// block from the response and the model name used.
func (c *Client) Messages(ctx context.Context, systemText, userText string) (responseText, modelName string, err error) {
	params := anthropicsdk.MessageNewParams{
		Model:     c.model,
		MaxTokens: int64(c.maxTokens),
		Messages: []anthropicsdk.MessageParam{
			anthropicsdk.NewUserMessage(anthropicsdk.NewTextBlock(userText)),
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
		return "", "", err
	}
	for _, block := range msg.Content {
		if block.Type == "text" {
			return block.Text, msg.Model, nil
		}
	}
	return "", msg.Model, nil
}
