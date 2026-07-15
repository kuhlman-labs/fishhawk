package diagnostics

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

func ptrCat(c run.FailureCategory) *run.FailureCategory { return &c }
func ptrStr(s string) *string                           { return &s }

func sampleVersions() VersionFacts {
	return VersionFacts{
		Fishhawkd:        Component{Version: "v0.4.1", GitSHA: "abc1234"},
		MinRunnerVersion: "v0.3.0",
	}
}

func TestCollect_ProductFactsOnly(t *testing.T) {
	runID := uuid.New()
	failStageID := uuid.New()
	planStageID := uuid.New()

	r := &run.Run{
		ID:          runID,
		WorkflowID:  "feature_change",
		WorkflowSHA: "deadbeefspec",
		RunnerKind:  run.RunnerKindLocal,
		State:       run.StateFailed,
	}
	stages := []*run.Stage{
		{ID: planStageID, Sequence: 0, Type: run.StageTypePlan, State: run.StageStateSucceeded},
		{
			ID:              failStageID,
			Sequence:        1,
			Type:            run.StageTypeImplement,
			State:           run.StageStateFailed,
			FailureCategory: ptrCat(run.FailureB),
			// Free text that MUST NOT leak into the bundle. It carries a
			// classifiable git-stderr shape (auth-401) AND the leak-canary
			// strings, so the class flows through while the reason text is
			// still proven absent from the serialized bundle below.
			FailureReason: ptrStr("fatal: unable to access '...': The requested URL returned error: 401 " +
				"(agent edited forbidden path /etc/secret and printed PROMPT_LEAK)"),
		},
	}
	entries := []*audit.Entry{
		{Sequence: 10, Category: "stage_dispatched"},
		{Sequence: 11, StageID: &failStageID, Category: "policy_evaluated"},
		{Sequence: 12, StageID: &failStageID, Category: "stage_failed"},
	}

	b := Collect(r, stages, entries, sampleVersions())

	if b.RunID != runID.String() {
		t.Errorf("RunID = %q, want %q", b.RunID, runID.String())
	}
	if b.WorkflowID != "feature_change" {
		t.Errorf("WorkflowID = %q", b.WorkflowID)
	}
	if b.WorkflowSpecHash != "deadbeefspec" {
		t.Errorf("WorkflowSpecHash = %q", b.WorkflowSpecHash)
	}
	if b.RunnerKind != run.RunnerKindLocal {
		t.Errorf("RunnerKind = %q", b.RunnerKind)
	}
	if b.RunState != string(run.StateFailed) {
		t.Errorf("RunState = %q", b.RunState)
	}
	if len(b.Stages) != 2 {
		t.Fatalf("Stages len = %d, want 2", len(b.Stages))
	}
	if b.Stages[0].Type != "plan" || b.Stages[1].Type != "implement" {
		t.Errorf("stage ordering wrong: %+v", b.Stages)
	}
	if b.FailingStage == nil {
		t.Fatal("FailingStage = nil, want the implement stage")
	}
	if b.FailingStage.Sequence != 1 || b.FailingStage.Type != "implement" {
		t.Errorf("FailingStage = %+v", b.FailingStage)
	}
	if b.FailingStage.FailureCategory != "B" {
		t.Errorf("FailureCategory = %q, want B", b.FailingStage.FailureCategory)
	}
	// Most-recent audit category scoped to the failing stage.
	if b.FailingStage.FailureSurface != "stage_failed" {
		t.Errorf("FailureSurface = %q, want stage_failed", b.FailingStage.FailureSurface)
	}
	// The detail class is derived from the free-text reason.
	if b.FailingStage.FailureDetailClass != "auth-401" {
		t.Errorf("FailureDetailClass = %q, want auth-401", b.FailingStage.FailureDetailClass)
	}
	if b.AuditSequenceRange == nil || b.AuditSequenceRange.Min != 10 || b.AuditSequenceRange.Max != 12 {
		t.Errorf("AuditSequenceRange = %+v, want {10,12}", b.AuditSequenceRange)
	}
	if b.Versions.Fishhawkd.Version != "v0.4.1" || b.Versions.Fishhawkd.GitSHA != "abc1234" {
		t.Errorf("Versions.Fishhawkd = %+v", b.Versions.Fishhawkd)
	}
	if b.Versions.MinRunnerVersion != "v0.3.0" {
		t.Errorf("MinRunnerVersion = %q", b.Versions.MinRunnerVersion)
	}

	// The hard requirement: no free text crosses the boundary. Serialize
	// the whole bundle and assert the FailureReason text is absent.
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, leak := range []string{"forbidden path", "/etc/secret", "PROMPT_LEAK", "FailureReason", "failure_reason"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("bundle leaked %q: %s", leak, raw)
		}
	}
}

func TestCollect_NoFailure(t *testing.T) {
	r := &run.Run{ID: uuid.New(), WorkflowID: "feature_change", State: run.StateSucceeded}
	stages := []*run.Stage{
		{ID: uuid.New(), Sequence: 0, Type: run.StageTypePlan, State: run.StageStateSucceeded},
		{ID: uuid.New(), Sequence: 1, Type: run.StageTypeImplement, State: run.StageStateSucceeded},
	}
	b := Collect(r, stages, nil, sampleVersions())
	if b.FailingStage != nil {
		t.Errorf("FailingStage = %+v, want nil for a succeeded run", b.FailingStage)
	}
	if b.AuditSequenceRange != nil {
		t.Errorf("AuditSequenceRange = %+v, want nil for no entries", b.AuditSequenceRange)
	}
}

func TestCollect_OrdersStagesBySequence(t *testing.T) {
	r := &run.Run{ID: uuid.New(), State: run.StateRunning}
	// Deliberately out of order.
	stages := []*run.Stage{
		{Sequence: 2, Type: run.StageTypeReview, State: run.StageStatePending},
		{Sequence: 0, Type: run.StageTypePlan, State: run.StageStateSucceeded},
		{Sequence: 1, Type: run.StageTypeImplement, State: run.StageStateRunning},
	}
	b := Collect(r, stages, nil, sampleVersions())
	want := []int{0, 1, 2}
	for i, s := range b.Stages {
		if s.Sequence != want[i] {
			t.Errorf("Stages[%d].Sequence = %d, want %d", i, s.Sequence, want[i])
		}
	}
}

func TestCollect_NilRunIsSafe(t *testing.T) {
	b := Collect(nil, nil, nil, sampleVersions())
	if b.RunID != "" || b.FailingStage != nil {
		t.Errorf("nil run should yield an empty-ish bundle, got %+v", b)
	}
	// Versions still carried.
	if b.Versions.Fishhawkd.Version != "v0.4.1" {
		t.Errorf("versions dropped on nil run")
	}
}

func TestSequenceRange_Unordered(t *testing.T) {
	entries := []*audit.Entry{
		{Sequence: 30},
		{Sequence: 5},
		{Sequence: 17},
	}
	rng := sequenceRange(entries)
	if rng == nil || rng.Min != 5 || rng.Max != 30 {
		t.Errorf("sequenceRange = %+v, want {5,30}", rng)
	}
}
