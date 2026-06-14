package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/diagnostics"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

func newDiagServer(t *testing.T, stored *run.Run, stages []*run.Stage, af *scAuditFake) *Server {
	t.Helper()
	return New(Config{
		Addr:      "127.0.0.1:0",
		RunRepo:   &statusCommentRunRepo{stored: stored, stages: stages},
		AuditRepo: af,
	})
}

func TestGetRunDiagnostics_HappyPath(t *testing.T) {
	runID := uuid.New()
	failID := uuid.New()
	runRow := &run.Run{
		ID:          runID,
		Repo:        "kuhlman-labs/fishhawk",
		WorkflowID:  "feature_change",
		WorkflowSHA: "specsha123",
		RunnerKind:  run.RunnerKindLocal,
		State:       run.StateFailed,
	}
	stages := []*run.Stage{
		{ID: uuid.New(), Sequence: 0, Type: run.StageTypePlan, State: run.StageStateSucceeded},
		{
			ID:              failID,
			Sequence:        1,
			Type:            run.StageTypeImplement,
			State:           run.StageStateFailed,
			FailureCategory: failureCat(run.FailureA),
			FailureReason:   strPtr("SENSITIVE free text that must not leak"),
		},
	}
	af := &scAuditFake{allEntries: []*audit.Entry{
		{Sequence: 100, Category: "stage_dispatched"},
		{Sequence: 101, StageID: &failID, Category: "agent_failed"},
	}}
	s := newDiagServer(t, runRow, stages, af)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/diagnostics", runID), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}

	var b diagnostics.DiagnosticBundle
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b.RunID != runID.String() {
		t.Errorf("run_id = %q", b.RunID)
	}
	if b.WorkflowSpecHash != "specsha123" || b.RunnerKind != run.RunnerKindLocal {
		t.Errorf("spec/runner = %q/%q", b.WorkflowSpecHash, b.RunnerKind)
	}
	if len(b.Stages) != 2 {
		t.Fatalf("stages = %d, want 2", len(b.Stages))
	}
	if b.FailingStage == nil || b.FailingStage.FailureCategory != "A" {
		t.Fatalf("failing stage = %+v", b.FailingStage)
	}
	if b.FailingStage.FailureSurface != "agent_failed" {
		t.Errorf("failure_surface = %q, want agent_failed", b.FailingStage.FailureSurface)
	}
	if b.AuditSequenceRange == nil || b.AuditSequenceRange.Min != 100 || b.AuditSequenceRange.Max != 101 {
		t.Errorf("audit range = %+v", b.AuditSequenceRange)
	}
	// Versions are stamped from internal/version. In dev/test the values
	// are the literals; assert they are carried (non-empty), not the
	// specific value.
	if b.Versions.Fishhawkd.Version == "" {
		t.Errorf("fishhawkd version empty")
	}

	// The redaction boundary: no free text crosses by default.
	if strings.Contains(w.Body.String(), "SENSITIVE") || strings.Contains(w.Body.String(), "must not leak") {
		t.Errorf("bundle leaked free text: %s", w.Body.String())
	}
}

func TestGetRunDiagnostics_NotFound(t *testing.T) {
	s := newDiagServer(t, nil, nil, &scAuditFake{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/diagnostics", uuid.New()), nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestGetRunDiagnostics_BadUUID(t *testing.T) {
	s := newDiagServer(t, nil, nil, &scAuditFake{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/v0/runs/not-a-uuid/diagnostics", nil))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGetRunDiagnostics_NilRunRepo(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &scAuditFake{}})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/diagnostics", uuid.New()), nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestGetRunDiagnostics_NilAuditRepo(t *testing.T) {
	runID := uuid.New()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: &statusCommentRunRepo{stored: &run.Run{ID: runID}}})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/diagnostics", runID), nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}
