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

// TestClient_Messages_ConfigBaseURLTargetsRegionalEndpoint pins the
// region-scoped inference plumbing (ADR-062 / E44.7): a Config carrying a
// BaseURL sends the Messages request to THAT endpoint, so a regional cell's
// prompt content reaches its in-region model endpoint rather than the SDK
// default. Asserted behaviorally — the request must arrive at the configured
// server — not by reading back a struct field.
func TestClient_Messages_ConfigBaseURLTargetsRegionalEndpoint(t *testing.T) {
	var gotPath string
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(okResp(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	cfg := testConfig()
	cfg.BaseURL = srv.URL
	c := NewClient(cfg)

	if _, _, _, _, _, _, err := c.Messages(context.Background(), "sys", "user"); err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if hits != 1 {
		t.Fatalf("configured-base-URL server hits = %d, want 1 (Config.BaseURL was not applied)", hits)
	}
	if !strings.Contains(gotPath, "/messages") {
		t.Errorf("request path = %q, want it to contain '/messages'", gotPath)
	}
}

// TestClient_Messages_ExplicitOptionBeatsConfigBaseURL pins the option-ordering
// contract: Config.BaseURL is applied BEFORE the variadic opts, so a caller's
// explicit option.WithBaseURL (every existing test points the client at an
// httptest server this way) still wins. A reversed order would silently break
// those callers.
func TestClient_Messages_ExplicitOptionBeatsConfigBaseURL(t *testing.T) {
	winnerHits, loserHits := 0, 0
	loser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		loserHits++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(okResp(`{"ok":true}`))
	}))
	t.Cleanup(loser.Close)
	winner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		winnerHits++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(okResp(`{"ok":true}`))
	}))
	t.Cleanup(winner.Close)

	cfg := testConfig()
	cfg.BaseURL = loser.URL
	c := NewClient(cfg, option.WithBaseURL(winner.URL))

	if _, _, _, _, _, _, err := c.Messages(context.Background(), "sys", "user"); err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if winnerHits != 1 || loserHits != 0 {
		t.Errorf("hits: explicit-option server = %d (want 1), Config.BaseURL server = %d (want 0)", winnerHits, loserHits)
	}
}

// TestClient_EmptyConfigBaseURLKeepsSDKDefault pins the unregionalized path: an
// empty Config.BaseURL adds no base-URL option, so a caller-supplied one is the
// only override and the pre-ADR-062 behavior is unchanged.
func TestClient_EmptyConfigBaseURLKeepsSDKDefault(t *testing.T) {
	srv, _ := captureSchemaServer(t)
	cfg := testConfig()
	if cfg.BaseURL != "" {
		t.Fatalf("testConfig().BaseURL = %q, want empty", cfg.BaseURL)
	}
	c := NewClient(cfg, option.WithBaseURL(srv.URL))
	if _, _, _, _, _, _, err := c.Messages(context.Background(), "sys", "user"); err != nil {
		t.Fatalf("Messages: %v", err)
	}
}
