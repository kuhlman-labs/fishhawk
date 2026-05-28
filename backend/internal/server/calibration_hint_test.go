package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
)

// hintAuditRepo extends planAuditRepo with a working ListAll that returns
// seeded runtime_observed entries, filtered by the Category param when set.
type hintAuditRepo struct {
	planAuditRepo
	seeded []*audit.Entry
}

func (a *hintAuditRepo) ListAll(_ context.Context, p audit.ListAllParams) ([]*audit.Entry, error) {
	if p.Category == nil {
		return a.seeded, nil
	}
	var out []*audit.Entry
	for _, e := range a.seeded {
		if e.Category == *p.Category {
			out = append(out, e)
		}
	}
	return out, nil
}

func (a *hintAuditRepo) seedRuntimeObserved(runID uuid.UUID, predicted, actual float64) {
	a.seedRuntimeObservedConf(runID, predicted, actual, "medium")
}

func (a *hintAuditRepo) seedRuntimeObservedConf(runID uuid.UUID, predicted, actual float64, confidence string) {
	payload, _ := json.Marshal(runtimeObservedPayload{
		StageType:        "implement",
		PredictedMinutes: predicted,
		Confidence:       confidence,
		ActualMinutes:    actual,
		Outcome:          "success",
	})
	rid := runID
	a.seeded = append(a.seeded, &audit.Entry{
		ID:        uuid.New(),
		RunID:     &rid,
		Timestamp: time.Now().UTC(),
		Category:  "runtime_observed",
		Payload:   payload,
	})
}

// newCalibrationHintPromptServer wires a Server with hintAuditRepo,
// planPromptRunRepo, and signingFake for calibration-hint prompt tests.
func newCalibrationHintPromptServer(t *testing.T) (*Server, *planPromptRunRepo, *hintAuditRepo, *signingFake) {
	t.Helper()
	rr := newPlanPromptRunRepo()
	ar := &hintAuditRepo{}
	sf := newSigningFake()
	// Provide a stub issue so fillIssueContext doesn't dereference a nil
	// return from GetIssue (seedRunWithStages seeds a run with InstallationID
	// set, which triggers the GitHub fetch path).
	gh := &stubIssueGetter{issue: &githubclient.Issue{Number: 42, Title: "stub issue"}}
	s := New(Config{
		Addr:        "127.0.0.1:0",
		RunRepo:     rr,
		AuditRepo:   ar,
		SigningRepo: sf,
	})
	s.promptIssueGetterOverride = gh
	return s, rr, ar, sf
}

func TestPlanPrompt_CalibrationHint_BelowThreshold(t *testing.T) {
	s, rr, hintRepo, sf := newCalibrationHintPromptServer(t)
	runID, planStageID, _, _ := seedRunWithStages(rr)

	// Seed 4 entries — below the 5-sample minimum threshold.
	for range 4 {
		hintRepo.seedRuntimeObserved(runID, 10.0, 12.0)
	}

	priv, _ := sf.issue(t, runID)
	w := promptRequestForStage(t, s, runID, planStageID, priv)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resp.Prompt, "Calibration hint") {
		t.Errorf("plan prompt should not contain calibration hint below threshold:\n%s", resp.Prompt)
	}
}

func TestPlanPrompt_CalibrationHint_BandAdvisoryRendered(t *testing.T) {
	s, rr, hintRepo, sf := newCalibrationHintPromptServer(t)
	runID, planStageID, _, _ := seedRunWithStages(rr)

	// Seed 9 high-confidence entries outside 1.5x (actual=20, predicted=10 → ratio=2x)
	// and 1 high-confidence entry inside 1.5x → 1/10 = 10% ≤ 25% threshold.
	for range 9 {
		hintRepo.seedRuntimeObservedConf(runID, 10.0, 20.0, "high")
	}
	hintRepo.seedRuntimeObservedConf(runID, 10.0, 12.0, "high")

	priv, _ := sf.issue(t, runID)
	w := promptRequestForStage(t, s, runID, planStageID, priv)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Prompt, "### Calibration hint") {
		t.Errorf("plan prompt missing calibration hint section:\n%s", resp.Prompt)
	}
	if !strings.Contains(resp.Prompt, "LEAST accurate band historically") {
		t.Errorf("plan prompt missing high-band advisory:\n%s", resp.Prompt)
	}
	if !strings.Contains(resp.Prompt, "1/10 within 1.5x") {
		t.Errorf("plan prompt missing advisory X/Y count (1/10):\n%s", resp.Prompt)
	}
}

func TestPlanPrompt_CalibrationHint_AboveThreshold(t *testing.T) {
	s, rr, hintRepo, sf := newCalibrationHintPromptServer(t)
	runID, planStageID, _, _ := seedRunWithStages(rr)

	// Seed 10 entries with predicted=10.0, actual=12.0 → ratio 1.20x.
	for range 10 {
		hintRepo.seedRuntimeObserved(runID, 10.0, 12.0)
	}

	priv, _ := sf.issue(t, runID)
	w := promptRequestForStage(t, s, runID, planStageID, priv)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Prompt, "### Calibration hint") {
		t.Errorf("plan prompt missing calibration hint section:\n%s", resp.Prompt)
	}
	if !strings.Contains(resp.Prompt, "ratio = 1.20") {
		t.Errorf("plan prompt missing calibration ratio 1.20:\n%s", resp.Prompt)
	}
}
