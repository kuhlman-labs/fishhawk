package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
)

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
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
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
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		}{InputTokens: 100, OutputTokens: 20},
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

	// Parse the captured HTTP request body to verify the split.
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
	}
	if err := json.Unmarshal(captured, &reqBody); err != nil {
		t.Fatalf("parse captured request body: %v", err)
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
}
