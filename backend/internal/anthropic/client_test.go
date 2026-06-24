package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
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

	if _, _, _, _, err := c.Messages(context.Background(), "sys", "user"); err != nil {
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

	if _, _, _, _, err := c.Messages(context.Background(), "sys", "user"); err != nil {
		t.Fatalf("Messages: %v", err)
	}

	present, _, _ := outputConfigOf(t, *captured)
	if present {
		t.Error("nil-schema client carried output_config; want it omitted (unconstrained fallback)")
	}
}

// TestClient_MessagesWithCache_SurfacesCacheSplit pins the #1343 cache surface
// on the dedicated 7-return entry point: MessagesWithCache returns the SDK
// envelope's cache_read_input_tokens / cache_creation_input_tokens as the
// separate cacheReadTokens / cacheWriteTokens return values (okResp stamps
// 300 / 150), alongside the fresh input/output counts. The thin Messages
// delegator drops the cache split — verified by it compiling against the
// 5-return signature the agenteval MessageSender seam depends on.
func TestClient_MessagesWithCache_SurfacesCacheSplit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(okResp(`{"verdict":"approve"}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(testConfig(), option.WithBaseURL(srv.URL))
	_, _, inTok, outTok, cacheRead, cacheWrite, err := c.MessagesWithCache(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("MessagesWithCache: %v", err)
	}
	if inTok != 100 || outTok != 20 {
		t.Errorf("input/output = %d / %d, want 100 / 20 (fresh, cache-exclusive)", inTok, outTok)
	}
	if cacheRead != 300 || cacheWrite != 150 {
		t.Errorf("cache read/write = %d / %d, want 300 / 150 (SDK cache_read vs cache_creation)", cacheRead, cacheWrite)
	}

	// The thin Messages delegator returns the same fresh counts and drops the
	// cache split (its 5-return shape is what agenteval.MessageSender pins).
	_, _, dInTok, dOutTok, err := c.Messages(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if dInTok != 100 || dOutTok != 20 {
		t.Errorf("Messages input/output = %d / %d, want 100 / 20 (delegates to MessagesWithCache)", dInTok, dOutTok)
	}
}
