package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
)

// captureSchemaServer returns an httptest server that records the request body
// and replies with a minimal valid Messages envelope, plus a pointer to the
// captured bytes.
func captureSchemaServer(t *testing.T) (*httptest.Server, *[]byte) {
	t.Helper()
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(okResp(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &captured
}

// outputConfigOf parses the captured request body's output_config block.
// present reports whether the body carried an output_config key at all (the
// nil-schema branch must omit it).
func outputConfigOf(t *testing.T, body []byte) (present bool, formatType string, schema map[string]any) {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("parse captured request body: %v", err)
	}
	ocRaw, ok := raw["output_config"]
	if !ok {
		return false, "", nil
	}
	var oc struct {
		Format struct {
			Type   string         `json:"type"`
			Schema map[string]any `json:"schema"`
		} `json:"format"`
	}
	if err := json.Unmarshal(ocRaw, &oc); err != nil {
		t.Fatalf("parse output_config: %v", err)
	}
	return true, oc.Format.Type, oc.Format.Schema
}

// TestClient_Messages_SendsSchemaWhenSet is branch A: a Client built with a
// non-nil schema sends output_config.format.type=="json_schema" carrying that
// exact schema deep-equal over the wire.
func TestClient_Messages_SendsSchemaWhenSet(t *testing.T) {
	srv, captured := captureSchemaServer(t)

	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"score"},
		"properties": map[string]any{
			"score": map[string]any{"type": "integer"},
		},
	}
	cfg := testConfig()
	cfg.Schema = schema
	c := NewClient(cfg, option.WithBaseURL(srv.URL))

	if _, _, _, _, _, _, err := c.Messages(context.Background(), "sys", "user"); err != nil {
		t.Fatalf("Messages: %v", err)
	}

	present, formatType, gotSchema := outputConfigOf(t, *captured)
	if !present {
		t.Fatal("non-nil-schema client omitted output_config; want it present")
	}
	if formatType != "json_schema" {
		t.Errorf("output_config.format.type = %q, want %q", formatType, "json_schema")
	}
	// Normalize the want schema through a json round-trip so map ordering and
	// value types match the decoded request body exactly.
	wantBytes, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	var want map[string]any
	if err := json.Unmarshal(wantBytes, &want); err != nil {
		t.Fatalf("normalize schema: %v", err)
	}
	if !reflect.DeepEqual(gotSchema, want) {
		t.Errorf("output_config.format.schema = %#v, want %#v", gotSchema, want)
	}
}

// TestClient_Messages_OmitsOutputConfigWhenNil is branch B: a Client built with
// a nil Schema OMITS output_config from the request body entirely (the
// unconstrained fallback branch).
func TestClient_Messages_OmitsOutputConfigWhenNil(t *testing.T) {
	srv, captured := captureSchemaServer(t)

	cfg := testConfig() // Schema left nil
	c := NewClient(cfg, option.WithBaseURL(srv.URL))

	if _, _, _, _, _, _, err := c.Messages(context.Background(), "sys", "user"); err != nil {
		t.Fatalf("Messages: %v", err)
	}

	present, _, _ := outputConfigOf(t, *captured)
	if present {
		t.Error("nil-schema client carried output_config; want it omitted (unconstrained fallback)")
	}
}

// fakeRespServer returns an httptest server that replies with the given
// fakeAnthropicResp envelope. It mirrors captureSchemaServer but provides
// response control rather than request capture, letting tests set arbitrary
// StopReason and content-block lists.
func fakeRespServer(t *testing.T, resp fakeAnthropicResp) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestClient_Messages_RefusalReturnsError asserts that a response with
// stop_reason=refusal causes Messages to return a non-nil typed error
// containing "refused" and "stop_reason=refusal", and that responseText is empty.
func TestClient_Messages_RefusalReturnsError(t *testing.T) {
	resp := fakeAnthropicResp{
		ID:         "msg_test",
		Type:       "message",
		Role:       "assistant",
		Model:      "claude-sonnet-4-6",
		StopReason: "refusal",
		Usage: struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		}{InputTokens: 10, OutputTokens: 0},
	}
	srv := fakeRespServer(t, resp)

	c := NewClient(testConfig(), option.WithBaseURL(srv.URL))
	text, _, _, _, _, _, err := c.Messages(context.Background(), "sys", "user")
	if err == nil {
		t.Fatal("Messages: got nil error for stop_reason=refusal, want non-nil")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("error = %q, want it to contain 'refused'", err)
	}
	if !strings.Contains(err.Error(), "stop_reason=refusal") {
		t.Errorf("error = %q, want it to contain 'stop_reason=refusal'", err)
	}
	if text != "" {
		t.Errorf("responseText = %q, want empty string on refusal error", text)
	}
}

// TestClient_Messages_MaxTokensReturnsError asserts that a response with
// stop_reason=max_tokens (carrying a partial text block) causes Messages to
// return a non-nil typed error containing "truncated at max_tokens" and
// "increase max_tokens", and that the partial fragment is NOT returned as the
// success responseText.
func TestClient_Messages_MaxTokensReturnsError(t *testing.T) {
	resp := fakeAnthropicResp{
		ID:   "msg_test",
		Type: "message",
		Role: "assistant",
		Content: []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{{Type: "text", Text: `{"partial`}}, // truncated mid-object
		Model:      "claude-sonnet-4-6",
		StopReason: "max_tokens",
		Usage: struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		}{InputTokens: 100, OutputTokens: 1024},
	}
	srv := fakeRespServer(t, resp)

	c := NewClient(testConfig(), option.WithBaseURL(srv.URL))
	text, _, _, _, _, _, err := c.Messages(context.Background(), "sys", "user")
	if err == nil {
		t.Fatal("Messages: got nil error for stop_reason=max_tokens, want non-nil")
	}
	if !strings.Contains(err.Error(), "truncated at max_tokens") {
		t.Errorf("error = %q, want it to contain 'truncated at max_tokens'", err)
	}
	if !strings.Contains(err.Error(), "increase max_tokens") {
		t.Errorf("error = %q, want it to contain 'increase max_tokens'", err)
	}
	if text != "" {
		t.Errorf("responseText = %q, want empty string (partial fragment must not be returned as success)", text)
	}
}

// TestClient_Messages_NoTextBlockReturnsError asserts that a response with
// stop_reason=end_turn and no text block in Content causes Messages to return
// a non-nil typed error containing "no text block" and the stop reason.
func TestClient_Messages_NoTextBlockReturnsError(t *testing.T) {
	resp := fakeAnthropicResp{
		ID:         "msg_test",
		Type:       "message",
		Role:       "assistant",
		Model:      "claude-sonnet-4-6",
		StopReason: "end_turn",
		Usage: struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		}{InputTokens: 10, OutputTokens: 0},
	}
	srv := fakeRespServer(t, resp)

	c := NewClient(testConfig(), option.WithBaseURL(srv.URL))
	text, _, _, _, _, _, err := c.Messages(context.Background(), "sys", "user")
	if err == nil {
		t.Fatal("Messages: got nil error for end_turn with no text block, want non-nil")
	}
	if !strings.Contains(err.Error(), "no text block") {
		t.Errorf("error = %q, want it to contain 'no text block'", err)
	}
	if !strings.Contains(err.Error(), "end_turn") {
		t.Errorf("error = %q, want it to name the stop_reason 'end_turn'", err)
	}
	if text != "" {
		t.Errorf("responseText = %q, want empty string on no-text-block error", text)
	}
}

// textContent builds a single-text-block Content list for a fakeAnthropicResp.
func textContent(text string) []struct {
	Type string `json:"type"`
	Text string `json:"text"`
} {
	return []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{{Type: "text", Text: text}}
}

// usage100x20 is the shared non-zero Usage block: 100 input / 20 output. Tests
// on the error paths assert these survive so cost attribution is preserved.
func usage100x20() struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
} {
	return struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	}{InputTokens: 100, OutputTokens: 20}
}

// TestClient_Messages_ContextWindowExceededWithPartialTextReturnsError is test
// (a): stop_reason=model_context_window_exceeded WITH a partial text block must
// return the context-window error (naming the input-token count) with an empty
// responseText — the truncated fragment must NOT surface as success — while the
// non-zero token counts survive so cost attribution is preserved.
func TestClient_Messages_ContextWindowExceededWithPartialTextReturnsError(t *testing.T) {
	resp := fakeAnthropicResp{
		ID:         "msg_test",
		Type:       "message",
		Role:       "assistant",
		Content:    textContent(`{"partial`), // truncated mid-object
		Model:      "claude-sonnet-4-6",
		StopReason: "model_context_window_exceeded",
		Usage:      usage100x20(),
	}
	srv := fakeRespServer(t, resp)

	c := NewClient(testConfig(), option.WithBaseURL(srv.URL))
	text, _, inTok, outTok, _, _, err := c.Messages(context.Background(), "sys", "user")
	if err == nil {
		t.Fatal("Messages: got nil error for stop_reason=model_context_window_exceeded, want non-nil")
	}
	// Error IDENTITY, not mere existence: the fail-closed catch-all also returns
	// AN error under a deletion of this branch, so assert the specific
	// context-window substrings so the counterfactual lands here (condition 3).
	if !strings.Contains(err.Error(), "context window exceeded") {
		t.Errorf("error = %q, want it to contain 'context window exceeded'", err)
	}
	if !strings.Contains(err.Error(), "reduce prompt size") {
		t.Errorf("error = %q, want it to contain 'reduce prompt size'", err)
	}
	if !strings.Contains(err.Error(), "100") {
		t.Errorf("error = %q, want it to name the input-token count 100", err)
	}
	if text != "" {
		t.Errorf("responseText = %q, want empty string (partial fragment must not surface as success)", text)
	}
	if inTok != 100 || outTok != 20 {
		t.Errorf("token counts = in %d / out %d, want 100/20 (cost attribution must survive the error path)", inTok, outTok)
	}
}

// TestClient_Messages_ContextWindowExceededNoContentReturnsError is test (b):
// the same stop reason with NO content blocks must return the context-window
// error specifically, NOT the misattributing "no text block" message (the
// triage-misattribution mode #2199 names).
func TestClient_Messages_ContextWindowExceededNoContentReturnsError(t *testing.T) {
	resp := fakeAnthropicResp{
		ID:         "msg_test",
		Type:       "message",
		Role:       "assistant",
		Model:      "claude-sonnet-4-6",
		StopReason: "model_context_window_exceeded",
		Usage:      usage100x20(),
	}
	srv := fakeRespServer(t, resp)

	c := NewClient(testConfig(), option.WithBaseURL(srv.URL))
	text, _, _, _, _, _, err := c.Messages(context.Background(), "sys", "user")
	if err == nil {
		t.Fatal("Messages: got nil error for stop_reason=model_context_window_exceeded, want non-nil")
	}
	if !strings.Contains(err.Error(), "context window exceeded") {
		t.Errorf("error = %q, want the context-window error", err)
	}
	if strings.Contains(err.Error(), "no text block") {
		t.Errorf("error = %q, must NOT be the misattributing 'no text block' error", err)
	}
	if text != "" {
		t.Errorf("responseText = %q, want empty string", text)
	}
}

// TestClient_Messages_UnrecognizedStopReasonReturnsError is test (c): an
// unrecognized stop reason ("pause_turn") WITH a partial text block must return
// the fail-closed catch-all error naming that stop reason, with an empty
// responseText so the fragment is never surfaced as success.
func TestClient_Messages_UnrecognizedStopReasonReturnsError(t *testing.T) {
	resp := fakeAnthropicResp{
		ID:         "msg_test",
		Type:       "message",
		Role:       "assistant",
		Content:    textContent(`{"partial`),
		Model:      "claude-sonnet-4-6",
		StopReason: "pause_turn",
		Usage:      usage100x20(),
	}
	srv := fakeRespServer(t, resp)

	c := NewClient(testConfig(), option.WithBaseURL(srv.URL))
	text, _, _, _, _, _, err := c.Messages(context.Background(), "sys", "user")
	if err == nil {
		t.Fatal("Messages: got nil error for an unrecognized stop_reason, want non-nil")
	}
	if !strings.Contains(err.Error(), "pause_turn") {
		t.Errorf("error = %q, want it to name the unrecognized stop_reason 'pause_turn'", err)
	}
	if text != "" {
		t.Errorf("responseText = %q, want empty string (fragment must not surface as success)", text)
	}
}

// TestClient_Messages_EmptyStopReasonFallsThrough is test (d): an EMPTY
// stop_reason with a well-formed text block falls through the catch-all to the
// content loop and returns the text with a nil error — pinning the deliberate
// gateway carve-out as a positive assertion so it cannot be silently widened or
// narrowed later.
func TestClient_Messages_EmptyStopReasonFallsThrough(t *testing.T) {
	resp := fakeAnthropicResp{
		ID:         "msg_test",
		Type:       "message",
		Role:       "assistant",
		Content:    textContent(`{"verdict":"approve"}`),
		Model:      "claude-sonnet-4-6",
		StopReason: "", // gateway omitted the field
		Usage:      usage100x20(),
	}
	srv := fakeRespServer(t, resp)

	c := NewClient(testConfig(), option.WithBaseURL(srv.URL))
	text, _, _, _, _, _, err := c.Messages(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("Messages: got error for empty stop_reason, want the text returned (carve-out): %v", err)
	}
	if text != `{"verdict":"approve"}` {
		t.Errorf("responseText = %q, want the well-formed text returned through the carve-out", text)
	}
}

// TestClient_Messages_RefusalErrorNamesCategory is test (e): stop_reason=refusal
// carrying stop_details.category="general_harms" must return an error containing
// "general_harms" alongside the pre-existing "refused" / "stop_reason=refusal"
// substrings, so a refused review is triageable.
func TestClient_Messages_RefusalErrorNamesCategory(t *testing.T) {
	resp := fakeAnthropicResp{
		ID:         "msg_test",
		Type:       "message",
		Role:       "assistant",
		Model:      "claude-sonnet-4-6",
		StopReason: "refusal",
		StopDetails: &struct {
			Category string `json:"category"`
		}{Category: "general_harms"},
		Usage: usage100x20(),
	}
	srv := fakeRespServer(t, resp)

	c := NewClient(testConfig(), option.WithBaseURL(srv.URL))
	_, _, _, _, _, _, err := c.Messages(context.Background(), "sys", "user")
	if err == nil {
		t.Fatal("Messages: got nil error for stop_reason=refusal, want non-nil")
	}
	if !strings.Contains(err.Error(), "general_harms") {
		t.Errorf("error = %q, want it to name the refusal category 'general_harms'", err)
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("error = %q, want it to contain 'refused'", err)
	}
	if !strings.Contains(err.Error(), "stop_reason=refusal") {
		t.Errorf("error = %q, want it to contain 'stop_reason=refusal'", err)
	}
}

// observedRequest is what the region-scoped inference endpoint saw.
type observedRequest struct {
	host   string
	apiKey string
	auth   string
	system string
	user   string
}

// regionEndpoint stands in for a cell's in-region inference endpoint. It
// records the host, the credential headers, and the system/user split of every
// Messages call, and replies with a valid approve verdict so Reviewer.Review
// completes.
func regionEndpoint(t *testing.T) (*httptest.Server, *[]observedRequest) {
	t.Helper()
	var seen []observedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			System []struct {
				Text string `json:"text"`
			} `json:"system"`
			Messages []struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		obs := observedRequest{
			host:   r.Host,
			apiKey: r.Header.Get("x-api-key"),
			auth:   r.Header.Get("Authorization"),
		}
		if len(req.System) > 0 {
			obs.system = req.System[0].Text
		}
		if len(req.Messages) > 0 && len(req.Messages[0].Content) > 0 {
			obs.user = req.Messages[0].Content[0].Text
		}
		seen = append(seen, obs)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(okResp(`{"verdict":"approve"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// TestRegionScopedInference_BothReviewPaths is the done-means behavioral
// assertion for the region-scoped inference config (ADR-062, E44.7 / #1831).
// It does NOT check that a field is populated: it drives the real Reviewer with
// a plan-review prompt AND an implement-review prompt and asserts that the
// endpoint the requests actually reached is the configured region base URL, and
// that the credential they carried is the region-scoped API key.
//
// Both review paths share one Reviewer (they differ only in prompt shape), so
// asserting only one would leave the other unproven against a future
// per-path client.
func TestRegionScopedInference_BothReviewPaths(t *testing.T) {
	srv, seen := regionEndpoint(t)

	const regionKey = "eu-region-scoped-key"
	cfg := testConfig()
	cfg.APIKey = regionKey
	cfg.BaseURL = srv.URL

	// No option.WithBaseURL override here — the point is that cfg.BaseURL
	// alone routes the call.
	reviewer := NewReviewer(cfg)

	planPrompt := "review criteria" + prompt.PlanReviewSplitMarker + "the plan artifact"
	implementPrompt := "review criteria" + prompt.PlanReviewSplitMarker + "the approved plan" +
		prompt.ImplementReviewSplitMarker + "the diff"

	for _, tc := range []struct {
		name   string
		prompt string
	}{
		{"plan review", planPrompt},
		{"implement review", implementPrompt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := reviewer.Review(context.Background(), tc.prompt); err != nil {
				t.Fatalf("Review: %v", err)
			}
		})
	}

	if len(*seen) != 2 {
		t.Fatalf("region endpoint saw %d requests, want 2 (one per review path)", len(*seen))
	}
	wantHost := strings.TrimPrefix(srv.URL, "http://")
	for i, obs := range *seen {
		if obs.host != wantHost {
			t.Errorf("request %d reached host %q, want the region base URL host %q", i, obs.host, wantHost)
		}
		if obs.apiKey != regionKey && obs.auth != "Bearer "+regionKey {
			t.Errorf("request %d credential = x-api-key:%q Authorization:%q, want the region-scoped key %q in one of them",
				i, obs.apiKey, obs.auth, regionKey)
		}
	}
	// Sanity: the two requests really were the two different review shapes,
	// so this is not the same path asserted twice.
	if (*seen)[0].user == (*seen)[1].user {
		t.Errorf("both requests carried the same user block %q; the plan and implement paths must differ", (*seen)[0].user)
	}
}

// TestNewClient_EmptyKeyNeutralizesAmbientCredentials proves the empty-key
// boundary invariant (#2108): when cfg.APIKey is empty, NewClient appends
// option.WithoutEnvironmentDefaults() so the SDK's credential autoloader
// contributes NOTHING from the process environment. Each sub-case SETS a
// distinct ambient sentinel via t.Setenv (never clears it) and drives a real
// Messages call to a custom endpoint, then asserts that neither sentinel appears
// in the observed x-api-key / Authorization headers. Header-ABSENCE on the wire
// is the security invariant — NOT a request count or an error shape: with the
// autoloader suppressed a request may legitimately reach the endpoint carrying
// no credential at all, and either outcome (no request, or a credential-less
// request) satisfies absence. Without the fix, the ambient sentinel would be
// autoloaded onto the request and this test would catch it.
func TestNewClient_EmptyKeyNeutralizesAmbientCredentials(t *testing.T) {
	const (
		ambientAPIKey    = "ambient-api-key-sentinel"
		ambientAuthToken = "ambient-auth-token-sentinel"
	)
	// One behavioral assertion per named ambient source: ANTHROPIC_API_KEY
	// only, ANTHROPIC_AUTH_TOKEN only, and both.
	for _, tc := range []struct {
		name         string
		apiKeyEnv    string
		authTokenEnv string
	}{
		{"ANTHROPIC_API_KEY only", ambientAPIKey, ""},
		{"ANTHROPIC_AUTH_TOKEN only", "", ambientAuthToken},
		{"both ambient sources", ambientAPIKey, ambientAuthToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ANTHROPIC_API_KEY", tc.apiKeyEnv)
			t.Setenv("ANTHROPIC_AUTH_TOKEN", tc.authTokenEnv)

			srv, seen := regionEndpoint(t)
			cfg := testConfig()
			cfg.APIKey = "" // the withhold posture: no explicit credential
			cfg.BaseURL = srv.URL
			c := NewClient(cfg)

			// The call MAY reach the endpoint with no credential or MAY error
			// before opening a connection — both are legitimate with the
			// autoloader suppressed, so the return is deliberately ignored. The
			// invariant is what any observed request carried on the wire.
			_, _, _, _, _, _, _ = c.Messages(context.Background(), "sys", "user")

			for i, obs := range *seen {
				if strings.Contains(obs.apiKey, ambientAPIKey) || strings.Contains(obs.auth, ambientAPIKey) {
					t.Errorf("request %d carried the ambient ANTHROPIC_API_KEY sentinel to the endpoint (x-api-key=%q Authorization=%q); an empty explicit key must neutralize it",
						i, obs.apiKey, obs.auth)
				}
				if strings.Contains(obs.apiKey, ambientAuthToken) || strings.Contains(obs.auth, ambientAuthToken) {
					t.Errorf("request %d carried the ambient ANTHROPIC_AUTH_TOKEN sentinel to the endpoint (x-api-key=%q Authorization=%q); an empty explicit key must neutralize it",
						i, obs.apiKey, obs.auth)
				}
			}
		})
	}

	// Positive control (no-regression): with a NON-empty explicit key the guard
	// is inert — the autoloader is NOT suppressed, yet the explicit key still
	// wins and is presented on the wire. Proves the neutralization bites only on
	// the empty-key path, leaving every existing non-empty-key caller unchanged.
	t.Run("non-empty key still presented", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", ambientAPIKey)

		srv, seen := regionEndpoint(t)
		const explicitKey = "explicit-region-key"
		cfg := testConfig()
		cfg.APIKey = explicitKey
		cfg.BaseURL = srv.URL
		c := NewClient(cfg)

		if _, _, _, _, _, _, err := c.Messages(context.Background(), "sys", "user"); err != nil {
			t.Fatalf("Messages: %v", err)
		}
		if len(*seen) != 1 {
			t.Fatalf("endpoint saw %d requests, want 1", len(*seen))
		}
		obs := (*seen)[0]
		if obs.apiKey != explicitKey && obs.auth != "Bearer "+explicitKey {
			t.Errorf("explicit key not presented: x-api-key=%q Authorization=%q, want the explicit key %q in one of them",
				obs.apiKey, obs.auth, explicitKey)
		}
		if strings.Contains(obs.apiKey, ambientAPIKey) || strings.Contains(obs.auth, ambientAPIKey) {
			t.Errorf("the ambient ANTHROPIC_API_KEY sentinel shadowed the explicit key (x-api-key=%q Authorization=%q)",
				obs.apiKey, obs.auth)
		}
	})
}

// TestNewClient_EmptyBaseURLKeepsSDKDefault asserts the single-cell posture:
// an unset BaseURL adds no override, so an explicit option.WithBaseURL (what
// every other test here relies on) still governs.
func TestNewClient_EmptyBaseURLKeepsSDKDefault(t *testing.T) {
	srv, captured := captureSchemaServer(t)

	cfg := testConfig() // BaseURL left empty
	c := NewClient(cfg, option.WithBaseURL(srv.URL))
	if _, _, _, _, _, _, err := c.Messages(context.Background(), "sys", "user"); err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(*captured) == 0 {
		t.Error("no request reached the test server; an empty cfg.BaseURL must not redirect the call")
	}
}

// TestNewClient_ExplicitOptionOverridesConfigBaseURL asserts the precedence
// documented on NewClient: cfg.BaseURL is a DEFAULT, so a caller-supplied
// option.WithBaseURL wins.
func TestNewClient_ExplicitOptionOverridesConfigBaseURL(t *testing.T) {
	srv, captured := captureSchemaServer(t)

	cfg := testConfig()
	cfg.BaseURL = "http://unreachable.invalid"
	c := NewClient(cfg, option.WithBaseURL(srv.URL))
	if _, _, _, _, _, _, err := c.Messages(context.Background(), "sys", "user"); err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(*captured) == 0 {
		t.Error("no request reached the test server; the explicit option must override cfg.BaseURL")
	}
}
