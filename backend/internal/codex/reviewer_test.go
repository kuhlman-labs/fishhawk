package codex

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
)

func reviewerWithMode(mode string) *Reviewer {
	r := NewReviewer(testConfig())
	r.client.Cmd = helperCommand(mode)
	return r
}

// TestReviewer_HappyPath asserts a well-formed transcript decodes to an approve
// verdict, returns the configured model, and carries summed usage.
func TestReviewer_HappyPath(t *testing.T) {
	verdict, model, err := reviewerWithMode("happy").Review(context.Background(), "review this plan")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if verdict.Verdict != planreview.VerdictApprove {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, planreview.VerdictApprove)
	}
	if model != "gpt-5-codex" {
		t.Errorf("model = %q, want gpt-5-codex", model)
	}
	// Fresh input = raw 1234 - cached 100 = 1134, the #1010 cache-exclusive
	// contract applied at the adapter boundary.
	if !verdict.Usage.Known || verdict.Usage.InputTokens != 1134 || verdict.Usage.OutputTokens != 600 {
		t.Errorf("Usage = %+v, want {1134 600 true}", verdict.Usage)
	}
	// The #995/#1343 instrumentation rides the same pass-through: codex's single
	// cached total reaches the cache READ bucket (write 0), and turn count
	// reaches the server contract unmodified.
	if verdict.Usage.CacheReadInputTokens != 100 || verdict.Usage.CacheWriteInputTokens != 0 || verdict.Usage.Turns != 1 {
		t.Errorf("Usage = %+v, want CacheReadInputTokens=100 CacheWriteInputTokens=0 Turns=1", verdict.Usage)
	}
	if got := verdict.Usage.CachedInputTokens(); got != 100 {
		t.Errorf("Usage.CachedInputTokens() = %d, want 100 (read 100 + write 0)", got)
	}
}

// TestReviewer_SubprocessError asserts a non-zero exit surfaces as an error.
func TestReviewer_SubprocessError(t *testing.T) {
	r := NewReviewer(testConfig())
	r.client.cfg.MaxRetries = 0
	r.client.Cmd = helperCommand("error")
	_, _, err := r.Review(context.Background(), "review this plan")
	if err == nil {
		t.Fatal("expected error from non-zero subprocess exit, got nil")
	}
}

// TestReviewer_NonJSONVerdict asserts an agent_message body that is not JSON
// surfaces as a decode error (the #574 silent-skip class the server then
// applies, not a crash).
func TestReviewer_NonJSONVerdict(t *testing.T) {
	_, _, err := reviewerWithMode("non_json_verdict").Review(context.Background(), "review this plan")
	if err == nil {
		t.Fatal("expected error from non-JSON verdict body, got nil")
	}
	if !strings.Contains(err.Error(), "decode verdict") {
		t.Errorf("error = %q, want a decode-verdict error", err)
	}
}

// TestReviewer_UnknownVerdict asserts a valid JSON verdict outside the closed
// set surfaces as an error.
func TestReviewer_UnknownVerdict(t *testing.T) {
	_, _, err := reviewerWithMode("bad_verdict").Review(context.Background(), "review this plan")
	if err == nil {
		t.Fatal("expected error from unknown verdict value, got nil")
	}
	if !strings.Contains(err.Error(), "unknown verdict") {
		t.Errorf("error = %q, want an unknown-verdict error", err)
	}
}

// TestReviewer_FencedEscapeRegexDecodes drives the #739/#889 path: a verdict
// body Codex wrapped in a ```json fence and containing a lone `\-` regex escape
// must still decode through planreview.DecodeVerdict, not error.
func TestReviewer_FencedEscapeRegexDecodes(t *testing.T) {
	verdict, _, err := reviewerWithMode("fenced_escape_regex").Review(context.Background(), "review this plan")
	if err != nil {
		t.Fatalf("Review: got error for a fenced verdict carrying a regex escape, want a decoded verdict: %v", err)
	}
	if verdict.Verdict != planreview.VerdictReject {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, planreview.VerdictReject)
	}
	if !strings.Contains(verdict.FreeForm, `ghs_[A-Za-z0-9_.\-]{36,}`) {
		t.Errorf("FreeForm = %q, want it to contain the regex verbatim", verdict.FreeForm)
	}
}

// TestReviewer_UsageAbsentDegrades asserts a transcript with no usage line
// decodes with Known=false rather than erroring (#681).
func TestReviewer_UsageAbsentDegrades(t *testing.T) {
	verdict, _, err := reviewerWithMode("happy_no_usage").Review(context.Background(), "review this plan")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if verdict.Usage.Known {
		t.Errorf("Usage.Known = true, want false when no usage line appeared; got %+v", verdict.Usage)
	}
}

// TestReviewer_TimeoutMapsToError asserts a per-attempt timeout maps to a
// non-retryable timeout error through the Reviewer surface.
func TestReviewer_TimeoutMapsToError(t *testing.T) {
	var attempts int
	cfg := testConfig()
	cfg.Timeout = 50 * time.Millisecond
	r := NewReviewer(cfg)
	r.client.Cmd = countingHelperCommand("slow", &attempts)

	_, _, err := r.Review(context.Background(), "review this plan")
	if err == nil {
		t.Fatal("expected error from per-attempt timeout, got nil")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (a timeout must not be retried)", attempts)
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error = %q, want it labelled a timeout", err)
	}
}

// TestReviewer_SetMaxRetriesDisablesRetry asserts SetMaxRetries(0) yields a
// single attempt even on the retryable crash class, bypassing the zero->1
// normalisation.
func TestReviewer_SetMaxRetriesDisablesRetry(t *testing.T) {
	var attempts int
	r := NewReviewer(testConfig())
	r.SetMaxRetries(0)
	r.client.Cmd = countingHelperCommand("killed", &attempts)

	_, _, err := r.Review(context.Background(), "review this plan")
	if err == nil {
		t.Fatal("expected error from crashing subprocess, got nil")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (SetMaxRetries(0) disables retry)", attempts)
	}
}

// TestReviewer_SetMaxRetriesClampsNegative asserts a negative budget clamps to 0
// (a single attempt) rather than looping unbounded.
func TestReviewer_SetMaxRetriesClampsNegative(t *testing.T) {
	var attempts int
	r := NewReviewer(testConfig())
	r.SetMaxRetries(-1)
	r.client.Cmd = countingHelperCommand("killed", &attempts)

	_, _, err := r.Review(context.Background(), "review this plan")
	if err == nil {
		t.Fatal("expected error from crashing subprocess, got nil")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (negative budget clamps to 0)", attempts)
	}
}

// TestReviewer_FlakyDecodeRetries asserts a first-roll structurally-malformed
// verdict body re-rolls the reviewer and the second roll's valid approve verdict
// is returned (#901), in exactly two attempts.
func TestReviewer_FlakyDecodeRetries(t *testing.T) {
	var attempts int
	r := NewReviewer(testConfig())
	r.client.Cmd = flakyDecodeHelperCommand(&attempts)

	verdict, _, err := r.Review(context.Background(), "review this plan")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if verdict.Verdict != planreview.VerdictApprove {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, planreview.VerdictApprove)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (one malformed roll + one recovery)", attempts)
	}
}

// TestReviewer_PersistentBadJSONExhausts asserts a reviewer that emits a
// structurally-malformed verdict on every roll terminates as a "decode verdict
// JSON" error after the bounded budget — SetMaxRetries(1) => exactly 2 attempts
// (the ADR-036 backstop: no unbounded re-roll).
func TestReviewer_PersistentBadJSONExhausts(t *testing.T) {
	var attempts int
	r := NewReviewer(testConfig())
	r.SetMaxRetries(1)
	r.client.Cmd = countingHelperCommand("flaky_decode_bad", &attempts)

	_, _, err := r.Review(context.Background(), "review this plan")
	if err == nil {
		t.Fatal("expected a terminal decode error from a persistently-malformed reviewer, got nil")
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (SetMaxRetries(1) => 2 rolls)", attempts)
	}
	if !strings.Contains(err.Error(), "decode verdict JSON") {
		t.Errorf("error = %q, want a 'decode verdict JSON' terminal error", err)
	}
}

// TestReviewer_ImplementsPlanReviewer is a compile-time assertion that *Reviewer
// satisfies the server.PlanReviewer contract (Review signature). A drift in the
// interface would fail to compile here rather than at the serve.go wiring.
func TestReviewer_ImplementsPlanReviewer(t *testing.T) {
	var _ interface {
		Review(ctx context.Context, promptText string) (*planreview.ReviewVerdict, string, error)
	} = NewReviewer(testConfig())
}

// TestReviewer_ReviewerBinaryPath pins the #1768 binary-path provenance
// accessor: it returns the resolved binary the reviewer shells out to — the
// DefaultBinary ("codex") when Config.Binary is empty (NewClient normalises
// it), and the operator's override path verbatim when set. This is the value
// the server stamps as reviewer_binary on every plan_reviewed /
// implement_reviewed audit entry.
func TestReviewer_ReviewerBinaryPath(t *testing.T) {
	// Empty Config.Binary defaults to DefaultBinary via NewClient.
	r := NewReviewer(Config{})
	if got := r.ReviewerBinaryPath(); got != DefaultBinary {
		t.Errorf("ReviewerBinaryPath() = %q, want the default %q", got, DefaultBinary)
	}

	// An explicit override path is returned truthfully (the FISHHAWKD_CODEX_BINARY
	// resolved provenance).
	const override = "/opt/fishhawk/codex-pinned"
	ro := NewReviewer(Config{Binary: override})
	if got := ro.ReviewerBinaryPath(); got != override {
		t.Errorf("ReviewerBinaryPath() = %q, want the override %q", got, override)
	}
}
