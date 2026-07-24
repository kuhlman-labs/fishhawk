// Package anthropic provides an Anthropic SDK adapter that satisfies
// server.PlanReviewer. It is constructed in serve.go when
// FISHHAWKD_ANTHROPIC_API_KEY is set.
package anthropic

import (
	"context"
	"fmt"
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

	// BaseURL, when non-empty, overrides the SDK's default api.anthropic.com
	// endpoint with a region-scoped inference endpoint (ADR-062, E44.7 /
	// #1831). It pairs with APIKey: a regional cell is configured with the
	// base URL AND the key that endpoint accepts, so plan-review and
	// implement-review inference for that cell's accounts never leaves the
	// region. Empty keeps the SDK default, which is what a single-cell
	// deployment gets. Selection is process-level per ADR-062 Q3(a) — there
	// is no per-account endpoint registry.
	BaseURL string
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
//
// A non-empty cfg.BaseURL is applied as a DEFAULT (before opts), so the
// region-scoped endpoint governs production while a test's explicit
// option.WithBaseURL still wins.
//
// Empty-key boundary invariant (#2108): when cfg.APIKey == "" the client
// appends option.WithoutEnvironmentDefaults(), which makes anthropicsdk.NewClient
// skip DefaultClientOptions() (only the hardcoded production base-URL default is
// kept). This suppresses the SDK's credential autoloader — ANTHROPIC_API_KEY /
// ANTHROPIC_AUTH_TOKEN / ANTHROPIC_PROFILE / env-federation — so an empty
// explicit key means "present no credential from ANY source", not "empty
// X-Api-Key layered over whatever ambient Authorization header the autoloader
// set". The withhold posture (#2108: FISHHAWKD_MODEL_BASE_URL set with no
// FISHHAWKD_MODEL_API_KEY) relies on THIS to keep an operator shell's ambient
// Anthropic credential from reaching the operator-configured region endpoint —
// not on the empty explicit key alone. Our explicit option.WithBaseURL(cfg.BaseURL)
// (always non-empty in that withhold posture) still routes the call, since
// WithoutEnvironmentDefaults keeps only the base-URL default and our option is
// applied after it.
func NewClient(cfg Config, opts ...option.RequestOption) *Client {
	defaults := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		option.WithHTTPClient(&http.Client{Timeout: cfg.Timeout}),
	}
	if cfg.BaseURL != "" {
		defaults = append(defaults, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.APIKey == "" {
		// Neutralize every ambient SDK credential source so an empty explicit
		// key contributes NO credential from the process environment (#2108).
		defaults = append(defaults, option.WithoutEnvironmentDefaults())
	}
	allOpts := append(defaults, opts...)
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
// usage — fresh input, output, and the cache read / cache write split
// (#681/#1343) — so the caller can attribute reviewer agent cost including
// cache-aware pricing. The SDK always returns a Usage block on a successful
// Messages call, so the token counts are authoritative on the happy path.
// Refusal, max_tokens truncation, and no-text-block responses return a typed
// error rather than ("", nil); token counts are surfaced on these paths (not
// zeroed) so cost attribution is preserved. A transport-level error (non-nil
// err from Messages.New) zeros all four counts.
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
func (c *Client) Messages(ctx context.Context, systemText, userText string) (responseText, modelName string, inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int, err error) {
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
	// The Messages response Usage block carries the cache split alongside
	// the cache-exclusive input_tokens (#1343): cache_read_input_tokens and
	// cache_creation_input_tokens (int64 on message.go Usage, anthropic-sdk-go
	// v1.50.1). Surface them so the reviewer cost path prices cache-read at
	// the discount and cache-write at the premium instead of discarding them.
	cacheReadTok := int(msg.Usage.CacheReadInputTokens)
	cacheWriteTok := int(msg.Usage.CacheCreationInputTokens)
	// Inspect stop_reason before the content loop. A truncated or refusing
	// response may carry a partial text block, but a JSON-schema-constrained
	// verdict truncated mid-object is unparseable — failing loud is strictly
	// better than surfacing an unparseable fragment as ("", nil).
	if msg.StopReason == anthropicsdk.StopReasonRefusal {
		return "", msg.Model, inTok, outTok, cacheReadTok, cacheWriteTok,
			fmt.Errorf("anthropic: model refused to respond (stop_reason=refusal)")
	}
	if msg.StopReason == anthropicsdk.StopReasonMaxTokens {
		return "", msg.Model, inTok, outTok, cacheReadTok, cacheWriteTok,
			fmt.Errorf("anthropic: response truncated at max_tokens (%d tokens); increase max_tokens", outTok)
	}
	for _, block := range msg.Content {
		if block.Type == "text" {
			return block.Text, msg.Model, inTok, outTok, cacheReadTok, cacheWriteTok, nil
		}
	}
	return "", msg.Model, inTok, outTok, cacheReadTok, cacheWriteTok,
		fmt.Errorf("anthropic: response carried no text block (stop_reason=%s)", msg.StopReason)
}
