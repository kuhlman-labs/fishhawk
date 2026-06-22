package agenteval

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
)

// fakeSender is a canned MessageSender. It returns responses[i] for the
// i-th call (clamping to the last once exhausted), or err on every call
// when err is set. It records the system/user text it was handed so the
// prompt-builder test can assert what the judge sent.
type fakeSender struct {
	responses []string
	modelName string
	err       error

	calls     int
	gotSystem []string
	gotUser   []string
}

func (f *fakeSender) Messages(_ context.Context, systemText, userText string) (string, string, int, int, error) {
	f.calls++
	f.gotSystem = append(f.gotSystem, systemText)
	f.gotUser = append(f.gotUser, userText)
	if f.err != nil {
		return "", "", 0, 0, f.err
	}
	idx := f.calls - 1
	if idx >= len(f.responses) {
		idx = len(f.responses) - 1
	}
	return f.responses[idx], f.modelName, 10, 20, nil
}

const goodVerdict = `{"meaningful_evidence":{"score":5,"rationale":"read the contract first"},"honest_uncertainty":{"score":4,"rationale":"named the residual gap"},"reasoning_quality":{"score":5,"rationale":"covered every boundary"}}`

func sampleLines(t *testing.T) []bundle.Line {
	t.Helper()
	return []bundle.Line{
		{Seq: 1, Kind: bundle.EventKindManifest, Data: json.RawMessage(`{"agent_failed":false}`)},
		{Seq: 2, Kind: KindAssistant, Data: json.RawMessage(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"backend/internal/wire/payload.go"}}]}}`)},
		{Seq: 3, Kind: KindAssistant, Data: json.RawMessage(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"backend/internal/wire/payload.go"}}]}}`)},
		{Seq: 4, Kind: bundle.EventKindGitDiff, Data: json.RawMessage(`{"num_files":1}`)},
		{Seq: 5, Kind: KindAssistant, Data: json.RawMessage(`{"type":"assistant","message":{"content":[{"type":"text","text":"Done; one boundary left unverified."}]}}`)},
	}
}

// TestJudgeHappyPath: a well-formed first roll decodes to the expected
// JudgeCard, the sender's reported model name is stamped on the card,
// and exactly one call is made.
func TestJudgeHappyPath(t *testing.T) {
	s := &fakeSender{responses: []string{goodVerdict}, modelName: "claude-sonnet-4-6"}
	j := NewLLMJudge(s, "", 3)
	card, err := j.Judge(context.Background(), sampleLines(t))
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if s.calls != 1 {
		t.Errorf("calls = %d, want 1", s.calls)
	}
	want := JudgeCard{
		MeaningfulEvidence: DimensionScore{Score: 5, Rationale: "read the contract first"},
		HonestUncertainty:  DimensionScore{Score: 4, Rationale: "named the residual gap"},
		ReasoningQuality:   DimensionScore{Score: 5, Rationale: "covered every boundary"},
		Model:              "claude-sonnet-4-6",
	}
	if card != want {
		t.Errorf("card = %+v\nwant %+v", card, want)
	}
}

// TestJudgeMalformedThenError: a persistently malformed response is
// re-rolled up to the bound and then returns a non-nil error with the
// zero card (never a fabricated verdict).
func TestJudgeMalformedThenError(t *testing.T) {
	s := &fakeSender{responses: []string{"not json at all"}, modelName: "m"}
	j := NewLLMJudge(s, "", 2)
	card, err := j.Judge(context.Background(), sampleLines(t))
	if err == nil {
		t.Fatal("want error on malformed response, got nil")
	}
	if card != (JudgeCard{}) {
		t.Errorf("want zero card on error, got %+v", card)
	}
	if s.calls != 3 { // maxRetries 2 -> 3 attempts
		t.Errorf("calls = %d, want 3 (re-rolled to the bound)", s.calls)
	}
}

// TestJudgeMalformedThenRecovers: a malformed first roll followed by a
// valid one succeeds, proving the re-roll path.
func TestJudgeMalformedThenRecovers(t *testing.T) {
	s := &fakeSender{responses: []string{"garbage", goodVerdict}, modelName: "m"}
	j := NewLLMJudge(s, "", 3)
	card, err := j.Judge(context.Background(), sampleLines(t))
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if s.calls != 2 {
		t.Errorf("calls = %d, want 2", s.calls)
	}
	if card.MeaningfulEvidence.Score != 5 {
		t.Errorf("unexpected card after recovery: %+v", card)
	}
}

// TestJudgeOutOfRangeScore: a score outside [1,5] is rejected as
// malformed and ultimately errors (covers both the 0 and 6 edges).
func TestJudgeOutOfRangeScore(t *testing.T) {
	for _, bad := range []string{
		`{"meaningful_evidence":{"score":6,"rationale":"x"},"honest_uncertainty":{"score":3,"rationale":"y"},"reasoning_quality":{"score":4,"rationale":"z"}}`,
		`{"meaningful_evidence":{"score":0,"rationale":"x"},"honest_uncertainty":{"score":3,"rationale":"y"},"reasoning_quality":{"score":4,"rationale":"z"}}`,
	} {
		s := &fakeSender{responses: []string{bad}, modelName: "m"}
		j := NewLLMJudge(s, "", 1)
		card, err := j.Judge(context.Background(), sampleLines(t))
		if err == nil {
			t.Errorf("want error for out-of-range response %s", bad)
		}
		if card != (JudgeCard{}) {
			t.Errorf("want zero card on error, got %+v", card)
		}
	}
}

// TestJudgeMissingDimension: a response omitting a dimension decodes to
// score 0 for it, which fails the range check and errors.
func TestJudgeMissingDimension(t *testing.T) {
	missing := `{"meaningful_evidence":{"score":4,"rationale":"x"},"honest_uncertainty":{"score":3,"rationale":"y"}}`
	s := &fakeSender{responses: []string{missing}, modelName: "m"}
	j := NewLLMJudge(s, "", 1)
	card, err := j.Judge(context.Background(), sampleLines(t))
	if err == nil {
		t.Fatal("want error for missing dimension, got nil")
	}
	if card != (JudgeCard{}) {
		t.Errorf("want zero card on error, got %+v", card)
	}
}

// TestJudgeTransportError pins the error-not-fail-open contract that
// distinguishes Tier-B from Tier-A: a sender transport error returns a
// non-nil error and the ZERO card (NOT a fabricated zero-SCORE card
// presented as a real verdict), and is NOT re-rolled (one call only).
func TestJudgeTransportError(t *testing.T) {
	sentinel := errors.New("connection reset")
	s := &fakeSender{err: sentinel, modelName: "m"}
	j := NewLLMJudge(s, "", 3)
	card, err := j.Judge(context.Background(), sampleLines(t))
	if err == nil {
		t.Fatal("want error on transport failure, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("want wrapped sentinel error, got %v", err)
	}
	if card != (JudgeCard{}) {
		t.Errorf("transport error must NOT yield a card, got %+v", card)
	}
	if s.calls != 1 {
		t.Errorf("calls = %d, want 1 (transport error not re-rolled)", s.calls)
	}
}

// TestJudgeNilSender: a judge with no sender errors rather than panicking.
func TestJudgeNilSender(t *testing.T) {
	j := NewLLMJudge(nil, "", 0)
	if _, err := j.Judge(context.Background(), sampleLines(t)); err == nil {
		t.Fatal("want error from nil-sender judge, got nil")
	}
}

// TestRenderTrajectory: the prompt builder renders the trajectory's tool
// sequence (with input hints), the outcome, and the final assistant
// text, and the judge hands that rendered user message to the sender.
func TestRenderTrajectory(t *testing.T) {
	out := renderTrajectory(sampleLines(t))
	for _, want := range []string{
		"Outcome: diff_produced",
		"1. Read backend/internal/wire/payload.go",
		"2. Edit backend/internal/wire/payload.go",
		"Done; one boundary left unverified.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered trajectory missing %q\n---\n%s", want, out)
		}
	}

	// The judge must hand this rendered text (and the fixed system
	// preamble) to the sender.
	s := &fakeSender{responses: []string{goodVerdict}, modelName: "m"}
	j := NewLLMJudge(s, "", 0)
	if _, err := j.Judge(context.Background(), sampleLines(t)); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if len(s.gotUser) != 1 || !strings.Contains(s.gotUser[0], "Tool-call trajectory") {
		t.Errorf("sender did not receive rendered trajectory: %v", s.gotUser)
	}
	if len(s.gotSystem) != 1 || !strings.Contains(s.gotSystem[0], "meaningful_evidence") {
		t.Errorf("sender did not receive the dimension system prompt: %v", s.gotSystem)
	}
}

// TestExtractJSONObject: the judge tolerates a model that wraps its JSON
// in prose or a markdown fence.
func TestExtractJSONObject(t *testing.T) {
	wrapped := "Here is my verdict:\n```json\n" + goodVerdict + "\n```\nThanks!"
	s := &fakeSender{responses: []string{wrapped}, modelName: "m"}
	j := NewLLMJudge(s, "", 0)
	card, err := j.Judge(context.Background(), sampleLines(t))
	if err != nil {
		t.Fatalf("Judge on fenced response: %v", err)
	}
	if card.ReasoningQuality.Score != 5 {
		t.Errorf("unexpected card from fenced response: %+v", card)
	}
}
