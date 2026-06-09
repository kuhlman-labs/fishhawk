package planreview

import (
	"context"
	"errors"
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
// re-rolls up to maxRetries+1 rolls and returns the last decode error verbatim.
func TestDecodeVerdictRetrying_Exhausts(t *testing.T) {
	var calls int
	_, _, _, err := DecodeVerdictRetrying(context.Background(), 2, stubInfer(&calls, badVerdict))
	if err == nil {
		t.Fatal("expected a terminal decode error from a persistently-malformed reviewer, got nil")
	}
	if calls != 3 {
		t.Errorf("infer calls = %d, want 3 (maxRetries=2 => 3 rolls)", calls)
	}
	// The error must be the raw DecodeVerdict failure, preserving its cause.
	if _, decodeErr := DecodeVerdict([]byte(badVerdict)); decodeErr == nil || err.Error() != decodeErr.Error() {
		t.Errorf("err = %v, want the verbatim DecodeVerdict error", err)
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
