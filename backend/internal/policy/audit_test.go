package policy

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// fakeAuditRepo is the minimum surface EmitEvaluation needs. We
// don't exercise the full Repository interface here — the chained
// append path is covered by audit/postgres_test.go via testcontainers.
type fakeAuditRepo struct {
	appendChainedErr error
	captured         audit.ChainAppendParams
	called           int
}

func (f *fakeAuditRepo) Append(ctx context.Context, p audit.AppendParams) (*audit.Entry, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeAuditRepo) AppendGlobalChained(ctx context.Context, p audit.GlobalChainAppendParams) (*audit.Entry, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeAuditRepo) ListGlobal(ctx context.Context) ([]*audit.Entry, error) {
	return nil, nil
}

func (f *fakeAuditRepo) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	f.called++
	f.captured = p
	if f.appendChainedErr != nil {
		return nil, f.appendChainedErr
	}
	rid := p.RunID
	return &audit.Entry{
		ID:        uuid.New(),
		Sequence:  1,
		RunID:     &rid,
		StageID:   p.StageID,
		Timestamp: p.Timestamp,
		Category:  p.Category,
		ActorKind: p.ActorKind,
		Payload:   p.Payload,
		EntryHash: "deadbeef",
	}, nil
}

func (f *fakeAuditRepo) Get(ctx context.Context, id uuid.UUID) (*audit.Entry, error) {
	return nil, audit.ErrNotFound
}

func (f *fakeAuditRepo) ListForRun(ctx context.Context, runID uuid.UUID) ([]*audit.Entry, error) {
	return nil, nil
}

func (f *fakeAuditRepo) LastForRun(ctx context.Context, runID uuid.UUID) (*audit.Entry, error) {
	return nil, audit.ErrNotFound
}

func (f *fakeAuditRepo) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	return nil, nil
}

func TestEmitEvaluation_Pass(t *testing.T) {
	repo := &fakeAuditRepo{}
	runID := uuid.New()
	stageID := uuid.New()

	violations, err := EmitEvaluation(
		context.Background(), repo, runID, stageID,
		"implement",
		diff("backend/main.go", "backend/main_test.go"),
		Constraints{
			ForbiddenPaths:   []string{"infra/**"},
			MaxFilesChanged:  10,
			RequiredOutcomes: []string{"tests_added_or_updated"},
		},
		nil,
	)
	if err != nil {
		t.Fatalf("EmitEvaluation: %v", err)
	}
	if len(violations) != 0 {
		t.Errorf("expected no violations, got %+v", violations)
	}
	if repo.called != 1 {
		t.Fatalf("expected 1 audit append, got %d", repo.called)
	}
	if repo.captured.Category != CategoryPolicyEvaluated {
		t.Errorf("category = %q, want %q", repo.captured.Category, CategoryPolicyEvaluated)
	}
	if repo.captured.RunID != runID {
		t.Errorf("RunID mismatch")
	}
	if repo.captured.StageID == nil || *repo.captured.StageID != stageID {
		t.Errorf("StageID mismatch")
	}

	var got EvaluationPayload
	if err := json.Unmarshal(repo.captured.Payload, &got); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if !got.Passed {
		t.Errorf("Passed = false, want true")
	}
	if got.StageType != "implement" {
		t.Errorf("StageType = %q", got.StageType)
	}
	if len(got.Diff) != 2 {
		t.Errorf("Diff len = %d, want 2", len(got.Diff))
	}
	if got.Applied.MaxFilesChanged != 10 {
		t.Errorf("Applied.MaxFilesChanged round-trip failed: %d", got.Applied.MaxFilesChanged)
	}
}

func TestEmitEvaluation_Violations(t *testing.T) {
	repo := &fakeAuditRepo{}
	runID := uuid.New()
	stageID := uuid.New()

	violations, err := EmitEvaluation(
		context.Background(), repo, runID, stageID,
		"implement",
		diff("infra/main.tf", "backend/main.go"),
		Constraints{
			ForbiddenPaths:   []string{"infra/**"},
			RequiredOutcomes: []string{"tests_added_or_updated"},
		},
		nil,
	)
	if err != nil {
		t.Fatalf("EmitEvaluation: %v", err)
	}
	if len(violations) != 2 {
		t.Fatalf("expected 2 violations (forbidden + tests), got %+v", violations)
	}

	var got EvaluationPayload
	if err := json.Unmarshal(repo.captured.Payload, &got); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if got.Passed {
		t.Errorf("Passed = true, want false")
	}
	if len(got.Violations) != 2 {
		t.Errorf("payload.Violations len = %d", len(got.Violations))
	}
}

func TestEmitEvaluation_ActorSubjectPropagated(t *testing.T) {
	repo := &fakeAuditRepo{}
	runID := uuid.New()
	stageID := uuid.New()
	subject := "claude-code@anthropic"

	if _, err := EmitEvaluation(
		context.Background(), repo, runID, stageID,
		"plan",
		diff("docs/plan.md"),
		Constraints{},
		&subject,
	); err != nil {
		t.Fatalf("EmitEvaluation: %v", err)
	}
	if repo.captured.ActorSubject == nil || *repo.captured.ActorSubject != subject {
		t.Errorf("ActorSubject = %v, want %q", repo.captured.ActorSubject, subject)
	}
	if repo.captured.ActorKind == nil || *repo.captured.ActorKind != audit.ActorSystem {
		t.Errorf("ActorKind = %v, want system", repo.captured.ActorKind)
	}
}

func TestEmitEvaluation_AppendError(t *testing.T) {
	repo := &fakeAuditRepo{appendChainedErr: errors.New("db down")}
	runID := uuid.New()
	stageID := uuid.New()

	violations, err := EmitEvaluation(
		context.Background(), repo, runID, stageID,
		"implement",
		diff("a.go"),
		Constraints{},
		nil,
	)
	if err == nil {
		t.Fatal("expected error from append")
	}
	// Even on append failure the caller should still see the
	// computed violations so it can decide locally what to do.
	if len(violations) != 0 {
		t.Errorf("violations = %+v", violations)
	}
}

func TestEmitEvaluation_EmptyDiff(t *testing.T) {
	repo := &fakeAuditRepo{}
	runID := uuid.New()
	stageID := uuid.New()

	if _, err := EmitEvaluation(
		context.Background(), repo, runID, stageID,
		"plan",
		Diff{},
		Constraints{MaxFilesChanged: 5},
		nil,
	); err != nil {
		t.Fatalf("EmitEvaluation: %v", err)
	}

	var got EvaluationPayload
	if err := json.Unmarshal(repo.captured.Payload, &got); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if len(got.Diff) != 0 {
		t.Errorf("Diff = %+v, want empty", got.Diff)
	}
	if !got.Passed {
		t.Errorf("Passed = false, want true on empty diff with no constraints")
	}
}
