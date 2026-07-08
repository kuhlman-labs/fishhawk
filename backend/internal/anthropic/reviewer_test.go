package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
)

// malformedVerdict is structurally-malformed verdict JSON: a missing comma
// between members (`"approve" "concerns"`), the #901 class strict-then-repair
// planreview.DecodeVerdict cannot rescue. The SDK envelope carrying it as the
// content text is valid JSON; only the nested verdict body is malformed.
const malformedVerdict = `{"verdict":"approve" "concerns":[]}`

// fakeAnthropicResp is the minimal Anthropic Messages API response envelope.
type fakeAnthropicResp struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

func testConfig() Config {
	return Config{
		APIKey:    "test-key",
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
		Timeout:   5 * time.Second,
	}
}

func okResp(verdictJSON string) fakeAnthropicResp {
	return fakeAnthropicResp{
		ID:   "msg_test",
		Type: "message",
		Role: "assistant",
		Content: []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{{Type: "text", Text: verdictJSON}},
		Model:      "claude-sonnet-4-6",
		StopReason: "end_turn",
		Usage: struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		}{InputTokens: 100, OutputTokens: 20, CacheReadInputTokens: 70, CacheCreationInputTokens: 30},
	}
}

// TestReviewer_HappyPath asserts the review succeeds and the prompt is split
// correctly: system block contains the role-constraint preamble and NOT the
// plan artifact body; user message contains the plan content.
func TestReviewer_HappyPath(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(okResp(`{"verdict":"approve"}`))
	}))
	defer srv.Close()

	reviewer := NewReviewer(testConfig(), option.WithBaseURL(srv.URL))

	promptText := "ROLE CONSTRAINT preamble text" + prompt.PlanReviewSplitMarker + "Plan artifact body text"
	verdict, model, err := reviewer.Review(context.Background(), promptText)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if verdict.Verdict != planreview.VerdictApprove {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, planreview.VerdictApprove)
	}
	if model != "claude-sonnet-4-6" {
		t.Errorf("model = %q, want %q", model, "claude-sonnet-4-6")
	}

	// Parse the captured HTTP request body to verify the split AND the
	// structured-outputs constraint (#1324).
	var reqBody struct {
		System []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
		OutputConfig struct {
			Format struct {
				Type   string         `json:"type"`
				Schema map[string]any `json:"schema"`
			} `json:"format"`
		} `json:"output_config"`
	}
	if err := json.Unmarshal(captured, &reqBody); err != nil {
		t.Fatalf("parse captured request body: %v", err)
	}

	// (c) the request carries output_config.format.type == "json_schema" and a
	// schema deep-equal to the single planreview.VerdictSchema() source of truth.
	// VerdictSchema() is normalized through a json round-trip so map ordering and
	// value types match the decoded request body exactly.
	if got := reqBody.OutputConfig.Format.Type; got != "json_schema" {
		t.Errorf("output_config.format.type = %q, want %q (structured outputs not requested)", got, "json_schema")
	}
	wantSchemaBytes, err := json.Marshal(planreview.VerdictSchema())
	if err != nil {
		t.Fatalf("marshal VerdictSchema: %v", err)
	}
	var wantSchema map[string]any
	if err := json.Unmarshal(wantSchemaBytes, &wantSchema); err != nil {
		t.Fatalf("normalize VerdictSchema: %v", err)
	}
	if !reflect.DeepEqual(reqBody.OutputConfig.Format.Schema, wantSchema) {
		t.Errorf("output_config.format.schema = %#v, want planreview.VerdictSchema() = %#v", reqBody.OutputConfig.Format.Schema, wantSchema)
	}

	// (a) system block contains preamble substring and NOT the plan artifact body.
	if len(reqBody.System) == 0 {
		t.Fatal("system block is empty; expected preamble text")
	}
	sysText := reqBody.System[0].Text
	if !strings.Contains(sysText, "ROLE CONSTRAINT") {
		t.Errorf("system block should contain preamble; got: %q", sysText)
	}
	if strings.Contains(sysText, "Plan artifact body text") {
		t.Errorf("system block should NOT contain plan artifact body; got: %q", sysText)
	}

	// (b) user message contains the plan content.
	if len(reqBody.Messages) == 0 || len(reqBody.Messages[0].Content) == 0 {
		t.Fatal("messages block is empty")
	}
	userText := reqBody.Messages[0].Content[0].Text
	if !strings.Contains(userText, "Plan artifact body text") {
		t.Errorf("user block should contain plan artifact body; got: %q", userText)
	}
}

// TestSplitPrompt_ImplementReview_CollisionSelectsDiffBoundary locks the #1725
// binding condition: an implement-review prompt contains BOTH the plan-review
// marker "### Plan artifact" (now in the cached approved-plan section of the
// stable prefix) AND the implement marker "### Diff under review" — and the plan
// marker appears FIRST. splitPrompt must nonetheless split at the diff boundary
// (by review-kind discriminator, not marker-order precedence), so the whole
// stable prefix — including the plan artifact — lands in the cached system block.
func TestSplitPrompt_ImplementReview_CollisionSelectsDiffBoundary(t *testing.T) {
	// Reordered implement-review layout: stable prefix (schema + plan artifact,
	// plan marker present) THEN the diff boundary THEN the variable diff body.
	stablePrefix := "ROLE CONSTRAINT preamble\n\n### Verdict schema\n\nshape\n" +
		prompt.PlanReviewSplitMarker + "plan body\n\n### Originating issue\n\nissue text\n"
	variableTail := prompt.ImplementReviewSplitMarker + "diff body hunks"
	promptText := stablePrefix + variableTail

	// Sanity: both markers are present and the plan marker precedes the diff
	// marker, so a naive "split at whichever marker" would land at the wrong one.
	planIdx := strings.Index(promptText, prompt.PlanReviewSplitMarker)
	diffIdx := strings.Index(promptText, prompt.ImplementReviewSplitMarker)
	if planIdx < 0 || diffIdx < 0 || planIdx >= diffIdx {
		t.Fatalf("fixture must have both markers with plan before diff (planIdx=%d diffIdx=%d)", planIdx, diffIdx)
	}

	systemText, userText := splitPrompt(promptText)
	if systemText != stablePrefix {
		t.Errorf("system block should be the full stable prefix up to the diff boundary\n got: %q\nwant: %q", systemText, stablePrefix)
	}
	if userText != variableTail {
		t.Errorf("user block should start at the diff boundary\n got: %q\nwant: %q", userText, variableTail)
	}
	// The cached system block MUST include the plan artifact (a co-present plan
	// marker must NOT pull the split to the plan boundary) and MUST NOT include
	// the diff body.
	if !strings.Contains(systemText, "### Plan artifact") {
		t.Errorf("cached system block should contain the plan artifact:\n%q", systemText)
	}
	if strings.Contains(systemText, "### Diff under review") {
		t.Errorf("cached system block must NOT contain the diff section:\n%q", systemText)
	}
	if !strings.Contains(userText, "### Diff under review") || !strings.Contains(userText, "diff body hunks") {
		t.Errorf("user block should carry the diff section and body:\n%q", userText)
	}
}

// TestSplitPrompt_PlanReview_SplitsAtPlanBoundary confirms a plan-review prompt
// (no "### Diff under review" marker) still splits at the plan-artifact boundary,
// unchanged by the #1725 discriminator.
func TestSplitPrompt_PlanReview_SplitsAtPlanBoundary(t *testing.T) {
	system := "ROLE CONSTRAINT plan-review preamble"
	tail := prompt.PlanReviewSplitMarker + "plan artifact body"
	systemText, userText := splitPrompt(system + tail)
	if systemText != system {
		t.Errorf("plan-review system block = %q, want %q", systemText, system)
	}
	if userText != tail {
		t.Errorf("plan-review user block = %q, want %q", userText, tail)
	}
}

// TestReviewer_TransportFailure asserts that a 500 response causes Review
// to return a non-nil error.
func TestReviewer_TransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `{"type":"error","error":{"type":"api_error","message":"internal server error"}}`)
	}))
	defer srv.Close()

	reviewer := NewReviewer(testConfig(), option.WithBaseURL(srv.URL))
	_, _, err := reviewer.Review(context.Background(), "test prompt")
	if err == nil {
		t.Fatal("expected error from 500 response, got nil")
	}
}

// TestReviewer_InvalidJSON asserts that a 200 response with non-JSON verdict
// text causes Review to return a non-nil error.
func TestReviewer_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(okResp("this is not valid json"))
	}))
	defer srv.Close()

	reviewer := NewReviewer(testConfig(), option.WithBaseURL(srv.URL))
	_, _, err := reviewer.Review(context.Background(), "test prompt")
	if err == nil {
		t.Fatal("expected error from non-JSON verdict text, got nil")
	}
}

// TestReviewer_InvalidVerdictShape asserts that a valid JSON response with
// an unrecognised verdict value causes Review to return a non-nil error.
func TestReviewer_InvalidVerdictShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(okResp(`{"verdict":"unknown_value"}`))
	}))
	defer srv.Close()

	reviewer := NewReviewer(testConfig(), option.WithBaseURL(srv.URL))
	_, _, err := reviewer.Review(context.Background(), "test prompt")
	if err == nil {
		t.Fatal("expected error from invalid verdict value, got nil")
	}
}

// TestReviewer_InvalidEscapeRegexDecodes drives the #739 bug through the SDK
// backend: the model-emitted verdict text quotes a regex containing a lone
// `\-` (illegal as a JSON string escape). The SDK envelope is valid JSON; the
// nested verdict text carries the lone backslash, which must decode to a
// verdict rather than a "decode verdict JSON" error, with the regex preserved.
func TestReviewer_InvalidEscapeRegexDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The json encoder escapes the lone backslash to `\\-` on the wire;
		// the SDK decodes it back to `\-` in the text field, reproducing the
		// invalid-escape verdict text the model emitted.
		_ = json.NewEncoder(w).Encode(okResp(`{"verdict":"reject","free_form":"redact ghs_[A-Za-z0-9_.\-]{36,}"}`))
	}))
	defer srv.Close()

	reviewer := NewReviewer(testConfig(), option.WithBaseURL(srv.URL))
	verdict, _, err := reviewer.Review(context.Background(), "review this plan")
	if err != nil {
		t.Fatalf("Review: got error for a verdict carrying a regex escape, want a decoded verdict: %v", err)
	}
	if verdict.Verdict != planreview.VerdictReject {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, planreview.VerdictReject)
	}
	if !strings.Contains(verdict.FreeForm, `ghs_[A-Za-z0-9_.\-]{36,}`) {
		t.Errorf("FreeForm = %q, want it to contain the regex verbatim", verdict.FreeForm)
	}
}

// TestReviewer_FlakyDecodeRetries drives the #901 decode-retry through the SDK
// backend: request 1 returns a 200 whose verdict body is structurally-malformed
// JSON; request 2 returns a valid approve verdict. The Reviewer-layer
// decode-retry must re-roll the Messages call and decode the second response.
// Exactly 2 inbound HTTP requests prove (a) the decode-retry re-rolled and (b)
// the anthropic SDK did NOT itself retry the malformed 200 (its built-in retry
// covers only 408/409/429/5xx + connection errors) — a SDK retry would push the
// count above 2, and no re-roll at all would leave it at 1 with a decode error.
func TestReviewer_FlakyDecodeRetries(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_ = json.NewEncoder(w).Encode(okResp(malformedVerdict))
			return
		}
		_ = json.NewEncoder(w).Encode(okResp(`{"verdict":"approve"}`))
	}))
	defer srv.Close()

	// NewReviewer defaults the decode-retry budget to 1 → 2 rolls allowed.
	reviewer := NewReviewer(testConfig(), option.WithBaseURL(srv.URL))
	verdict, _, err := reviewer.Review(context.Background(), "review this plan")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if verdict.Verdict != planreview.VerdictApprove {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, planreview.VerdictApprove)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("inbound HTTP requests = %d, want exactly 2 (one malformed roll + one recovery; the SDK must not retry a malformed 200)", got)
	}
}

// TestReviewer_PersistentBadJSONExhausts asserts a Messages endpoint that returns
// a structurally-malformed 200 on every roll terminates as a "decode verdict
// JSON" error after the bounded budget — SetMaxRetries(1) => exactly 2 inbound
// requests (the ADR-036 backstop: no unbounded re-roll).
func TestReviewer_PersistentBadJSONExhausts(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(okResp(malformedVerdict))
	}))
	defer srv.Close()

	reviewer := NewReviewer(testConfig(), option.WithBaseURL(srv.URL))
	reviewer.SetMaxRetries(1)
	_, _, err := reviewer.Review(context.Background(), "review this plan")
	if err == nil {
		t.Fatal("expected a terminal decode error from a persistently-malformed reviewer, got nil")
	}
	if !strings.Contains(err.Error(), "decode verdict JSON") {
		t.Errorf("error = %q, want a 'decode verdict JSON' terminal error", err)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("inbound HTTP requests = %d, want exactly 2 (SetMaxRetries(1) => 2 rolls)", got)
	}
}

// TestReviewer_PopulatesUsage asserts the SDK response's Usage block is
// surfaced on the returned verdict (#681): the adapter attaches token usage
// from the API envelope (not the agent JSON), with Known=true on the happy
// path since the SDK always returns a Usage block on a successful call.
func TestReviewer_PopulatesUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// okResp stamps Usage{InputTokens:100, OutputTokens:20}.
		_ = json.NewEncoder(w).Encode(okResp(`{"verdict":"approve"}`))
	}))
	defer srv.Close()

	reviewer := NewReviewer(testConfig(), option.WithBaseURL(srv.URL))
	verdict, _, err := reviewer.Review(context.Background(), "review this plan")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if !verdict.Usage.Known {
		t.Error("Usage.Known = false, want true (SDK always returns usage)")
	}
	if verdict.Usage.InputTokens != 100 || verdict.Usage.OutputTokens != 20 {
		t.Errorf("Usage = %+v, want {InputTokens:100 OutputTokens:20 Known:true}", verdict.Usage)
	}
	if verdict.Usage.Turns != 1 {
		t.Errorf("Usage.Turns = %d, want 1 (single Messages call)", verdict.Usage.Turns)
	}
	// The SDK Usage block's cache split surfaces into the read/write buckets
	// (#1343) — no longer discarded/0. okResp stamps cache_read=70,
	// cache_creation=30; the adapter maps read→CacheReadInputTokens (cheaper),
	// creation→CacheWriteInputTokens (premium). If a future SDK renames these
	// fields, this assertion fails rather than silently zeroing the cache cost.
	if verdict.Usage.CacheReadInputTokens != 70 || verdict.Usage.CacheWriteInputTokens != 30 {
		t.Errorf("Usage cache split = read %d / write %d, want 70/30 (surfaced from SDK envelope, not 0)",
			verdict.Usage.CacheReadInputTokens, verdict.Usage.CacheWriteInputTokens)
	}
	// The summed accessor equals read+write (back-compat for the former field).
	if got := verdict.Usage.CachedInputTokens(); got != 100 {
		t.Errorf("Usage.CachedInputTokens() = %d, want 100 (read 70 + write 30)", got)
	}
}
