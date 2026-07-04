package planreview

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubInfer returns an InferFunc that yields the next responseText from texts on
// each call (the last entry repeats once exhausted), counts invocations into
// calls, and never returns an infer-stage error.
func stubInfer(calls *int, texts ...string) InferFunc {
	return func(_ context.Context) (string, string, Usage, error) {
		i := *calls
		*calls++
		if i >= len(texts) {
			i = len(texts) - 1
		}
		return texts[i], "stub-model", Usage{InputTokens: 1, OutputTokens: 2, Known: true}, nil
	}
}

const (
	goodVerdict = `{"verdict":"approve"}`
	// badVerdict is structurally-malformed: a missing comma between members —
	// the #901 class strict-then-repair DecodeVerdict cannot rescue.
	badVerdict = `{"verdict":"approve" "concerns":[]}`
)

// TestDecodeVerdictRetrying_FirstRollSucceeds asserts a well-formed first roll
// decodes with exactly one infer call and no re-roll.
func TestDecodeVerdictRetrying_FirstRollSucceeds(t *testing.T) {
	var calls int
	verdict, model, usage, err := DecodeVerdictRetrying(context.Background(), 3, stubInfer(&calls, goodVerdict))
	if err != nil {
		t.Fatalf("DecodeVerdictRetrying: %v", err)
	}
	if calls != 1 {
		t.Errorf("infer calls = %d, want 1 (a valid first roll must not re-roll)", calls)
	}
	if verdict.Verdict != VerdictApprove {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, VerdictApprove)
	}
	if model != "stub-model" {
		t.Errorf("model = %q, want stub-model", model)
	}
	if !usage.Known || usage.InputTokens != 1 || usage.OutputTokens != 2 {
		t.Errorf("usage = %+v, want {1 2 true}", usage)
	}
}

// TestDecodeVerdictRetrying_LateRollSucceeds asserts a malformed first roll is
// re-rolled and a later valid roll decodes, with the exact recovery count.
func TestDecodeVerdictRetrying_LateRollSucceeds(t *testing.T) {
	var calls int
	verdict, _, _, err := DecodeVerdictRetrying(context.Background(), 3, stubInfer(&calls, badVerdict, badVerdict, goodVerdict))
	if err != nil {
		t.Fatalf("DecodeVerdictRetrying: %v", err)
	}
	if calls != 3 {
		t.Errorf("infer calls = %d, want 3 (two malformed rolls + one recovery)", calls)
	}
	if verdict.Verdict != VerdictApprove {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, VerdictApprove)
	}
}

// TestDecodeVerdictRetrying_Exhausts asserts a persistently-malformed reviewer
// re-rolls up to maxRetries+1 rolls and returns a terminal error that (i) still
// carries the DecodeVerdict cause (both as an unwrappable %w cause and as a
// substring, preserving the 'invalid character ...' diagnostic) AND (ii) embeds
// a quoted snippet of the raw model output so the *_review_failed reason is
// diagnosable (#1576).
func TestDecodeVerdictRetrying_Exhausts(t *testing.T) {
	var calls int
	_, _, _, err := DecodeVerdictRetrying(context.Background(), 2, stubInfer(&calls, badVerdict))
	if err == nil {
		t.Fatal("expected a terminal decode error from a persistently-malformed reviewer, got nil")
	}
	if calls != 3 {
		t.Errorf("infer calls = %d, want 3 (maxRetries=2 => 3 rolls)", calls)
	}
	// The DecodeVerdict cause must remain both unwrappable (%w) and present as a
	// substring so callers matching on the decode text are unaffected.
	_, decodeErr := DecodeVerdict([]byte(badVerdict))
	if decodeErr == nil {
		t.Fatal("badVerdict must fail DecodeVerdict for this test to be meaningful")
	}
	if !errors.Is(err, decodeErr) && !strings.Contains(err.Error(), decodeErr.Error()) {
		t.Errorf("err = %v, want it to retain the DecodeVerdict cause (%v)", err, decodeErr)
	}
	// The terminal error must embed a quoted snippet of the raw output.
	if !strings.Contains(err.Error(), "raw output:") {
		t.Errorf("err = %q, want it to carry a 'raw output:' snippet", err)
	}
	// The snippet is strconv.Quote-escaped, so match distinctive tokens of the
	// badVerdict body rather than its raw (unescaped-quote) form.
	if !strings.Contains(err.Error(), "verdict") || !strings.Contains(err.Error(), "concerns") {
		t.Errorf("err = %q, want it to quote the offending badVerdict output", err)
	}
}

// TestDecodeVerdictRetrying_ExhaustsBoundsSnippet asserts the raw-output snippet
// wrapped onto the terminal error is length-bounded: a >200-char raw output is
// truncated to at most 200 runes plus a marker, so the reason never balloons to
// the full model output.
func TestDecodeVerdictRetrying_ExhaustsBoundsSnippet(t *testing.T) {
	// A long body with no '{' at all: firstJSONObject returns nil, the strict
	// decode fails on the leading 'A', and the loop exhausts — exercising the
	// snippet-truncation branch on the terminal error.
	longRaw := strings.Repeat("A", 500)
	var calls int
	_, _, _, err := DecodeVerdictRetrying(context.Background(), 0, stubInfer(&calls, longRaw))
	if err == nil {
		t.Fatal("expected a terminal decode error from a long undecodable output, got nil")
	}
	if !strings.Contains(err.Error(), "…") {
		t.Errorf("err = %q, want a truncation marker for an over-length snippet", err)
	}
	// The snippet caps the content at 200 runes, so the full 500-'A' body must
	// NOT appear verbatim — proving the reason is bounded, not the whole output.
	if strings.Contains(err.Error(), strings.Repeat("A", 300)) {
		t.Errorf("err = %q, want the raw-output snippet bounded well under 300 chars", err)
	}
}

// TestDecodeVerdictRetrying_InferErrorNotReRolled asserts an infer-stage error
// is returned immediately, unchanged, without a re-roll — the adapter's own
// crash-retry already ran, so re-rolling here would compound the bound.
func TestDecodeVerdictRetrying_InferErrorNotReRolled(t *testing.T) {
	var calls int
	sentinel := errors.New("inference failed")
	infer := func(_ context.Context) (string, string, Usage, error) {
		calls++
		return "", "", Usage{}, sentinel
	}
	_, _, _, err := DecodeVerdictRetrying(context.Background(), 3, infer)
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the sentinel infer error returned verbatim", err)
	}
	if calls != 1 {
		t.Errorf("infer calls = %d, want 1 (an infer error must not be re-rolled)", calls)
	}
}

// TestDecodeVerdictRetrying_CancelledCtxShortCircuits asserts an already-done
// context stops the loop after the first malformed roll rather than spending the
// budget on re-rolls.
func TestDecodeVerdictRetrying_CancelledCtxShortCircuits(t *testing.T) {
	var calls int
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, err := DecodeVerdictRetrying(ctx, 3, stubInfer(&calls, badVerdict))
	if err == nil {
		t.Fatal("expected a decode error, got nil")
	}
	if calls != 1 {
		t.Errorf("infer calls = %d, want 1 (a cancelled ctx must short-circuit re-rolls)", calls)
	}
}
