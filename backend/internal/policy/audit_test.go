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

func (f *fakeAuditRepo) ListGlobalByAccount(ctx context.Context, accountID *uuid.UUID) ([]*audit.Entry, error) {
	return nil, nil
}

func (f *fakeAuditRepo) ListAll(ctx context.Context, p audit.ListAllParams) ([]*audit.Entry, error) {
	return nil, nil
}

func (f *fakeAuditRepo) ChainsByParent(_ context.Context, _ uuid.UUID, _ bool) ([]*audit.Entry, error) {
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

// TestEmitEvaluation_OldPathWireRoundTrip pins the #2398 OldPath audit-wire
// contract (fixup concern 4): a non-rename DiffEntry serializes WITHOUT an
// `old_path` key (omitempty keeps a legacy payload byte-identical and older
// audit rows decode to empty), while a rename (R) entry round-trips OldPath
// through the audit payload — the same omitempty / legacy-decode properties the
// bundle wire got, now pinned on the audit surface (EmitEvaluation payload).
func TestEmitEvaluation_OldPathWireRoundTrip(t *testing.T) {
	repo := &fakeAuditRepo{}
	runID := uuid.New()
	stageID := uuid.New()

	d := Diff{ChangedFiles: []ChangedFile{
		{Path: "backend/plain.go", Status: StatusModified},
		{Path: "internal/moved.go", OldPath: "moved.go", Status: StatusRenamed},
	}}
	if _, err := EmitEvaluation(
		context.Background(), repo, runID, stageID,
		"implement", d, Constraints{}, nil,
	); err != nil {
		t.Fatalf("EmitEvaluation: %v", err)
	}

	// (a) On-wire: the non-rename entry carries NO old_path key; the rename entry
	// does. Inspect the raw JSON so the omitempty contract is checked on-wire,
	// not merely after decode (where an absent key and an empty string collapse).
	var raw struct {
		Diff []map[string]json.RawMessage `json:"diff"`
	}
	if err := json.Unmarshal(repo.captured.Payload, &raw); err != nil {
		t.Fatalf("payload unmarshal (raw): %v", err)
	}
	if len(raw.Diff) != 2 {
		t.Fatalf("diff entries = %d, want 2", len(raw.Diff))
	}
	if _, ok := raw.Diff[0]["old_path"]; ok {
		t.Errorf("non-rename entry serialized an old_path key: %s", repo.captured.Payload)
	}
	if _, ok := raw.Diff[1]["old_path"]; !ok {
		t.Errorf("rename entry omitted its old_path key: %s", repo.captured.Payload)
	}

	// (b) OldPath round-trips through decode back into a DiffEntry.
	var got EvaluationPayload
	if err := json.Unmarshal(repo.captured.Payload, &got); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if got.Diff[0].OldPath != "" {
		t.Errorf("non-rename OldPath = %q, want empty", got.Diff[0].OldPath)
	}
	if got.Diff[1].OldPath != "moved.go" {
		t.Errorf("rename OldPath = %q, want moved.go (round-trip)", got.Diff[1].OldPath)
	}

	// (c) A legacy payload with no old_path key decodes to an empty OldPath.
	var legacy DiffEntry
	if err := json.Unmarshal([]byte(`{"path":"a.go","status":"M"}`), &legacy); err != nil {
		t.Fatalf("legacy decode: %v", err)
	}
	if legacy.OldPath != "" {
		t.Errorf("legacy DiffEntry.OldPath = %q, want empty", legacy.OldPath)
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

func TestEmitEvaluation_DeferredCIGreen_PassesAndRecordsDeferral(t *testing.T) {
	// Trace upload happens before CI runs against the just-opened
	// PR, so ci_green has no signal at evaluation time. Pre-#297
	// this emitted a "no signal available" violation; post-#297 the
	// outcome is deferred to branch protection (ADR-017 / #251) and
	// recorded on the payload's DeferredOutcomes list so the SPA
	// can render the deferral inline with the pass state.
	repo := &fakeAuditRepo{}
	runID := uuid.New()
	stageID := uuid.New()

	violations, err := EmitEvaluation(
		context.Background(), repo, runID, stageID,
		"implement",
		diff("backend/main.go", "backend/main_test.go"),
		Constraints{
			RequiredOutcomes: []string{"tests_added_or_updated", "ci_green"},
			// CIGreen is nil — the production trace-handler path.
		},
		nil,
	)
	if err != nil {
		t.Fatalf("EmitEvaluation: %v", err)
	}
	if len(violations) != 0 {
		t.Errorf("expected no violations (ci_green deferred), got %+v", violations)
	}
	var got EvaluationPayload
	if err := json.Unmarshal(repo.captured.Payload, &got); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if !got.Passed {
		t.Errorf("Passed = false, want true (ci_green deferral isn't a violation)")
	}
	if len(got.DeferredOutcomes) != 1 || got.DeferredOutcomes[0] != "ci_green" {
		t.Errorf("DeferredOutcomes = %+v, want [ci_green]", got.DeferredOutcomes)
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
