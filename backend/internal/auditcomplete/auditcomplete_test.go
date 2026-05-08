package auditcomplete_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

// happyPath builds a fully-complete run: plan + implement + review
// stages, all terminal, with the required artifacts and chained
// audit entries. Each test that expects a pass starts here and
// mutates one piece to assert the failure mode.
func happyPath(t *testing.T) (uuid.UUID, *fakeRuns, *fakeArtifacts, *fakeAudit) {
	t.Helper()
	runID := uuid.New()
	plan := mkStage(runID, 1, run.StageTypePlan, run.StageStateSucceeded)
	impl := mkStage(runID, 2, run.StageTypeImplement, run.StageStateSucceeded)
	rev := mkStage(runID, 3, run.StageTypeReview, run.StageStateAwaitingApproval)

	runs := &fakeRuns{stages: []*run.Stage{plan, impl, rev}}
	arts := &fakeArtifacts{
		byStage: map[uuid.UUID][]*artifact.Artifact{
			plan.ID: {planArtifact(plan.ID, "standard_v1")},
			impl.ID: {pullRequestArtifact(impl.ID)},
		},
	}
	auditRepo := &fakeAudit{}
	auditRepo.appendChained(t, runID, &plan.ID, "stage_dispatched", nil)
	auditRepo.appendChained(t, runID, &plan.ID, "trace_uploaded", traceVariantPayload("raw"))
	auditRepo.appendChained(t, runID, &plan.ID, "trace_uploaded", traceVariantPayload("redacted"))
	auditRepo.appendChained(t, runID, &impl.ID, "trace_uploaded", traceVariantPayload("raw"))
	auditRepo.appendChained(t, runID, &impl.ID, "trace_uploaded", traceVariantPayload("redacted"))

	return runID, runs, arts, auditRepo
}

func TestCompute_AllRulesPass(t *testing.T) {
	runID, runs, arts, ar := happyPath(t)
	state, missing, err := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if state != stagecheck.StatePass {
		t.Fatalf("state = %s want pass; missing=%v", state, missing)
	}
	if len(missing) != 0 {
		t.Fatalf("expected no missing items; got %+v", missing)
	}
}

func TestCompute_PendingWhenStageMidFlight(t *testing.T) {
	runID, runs, arts, ar := happyPath(t)
	// Implement stage hasn't terminated yet.
	runs.stages[1].State = run.StageStateRunning
	state, missing, err := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if state != stagecheck.StatePending {
		t.Fatalf("state = %s want pending", state)
	}
	if len(missing) != 0 {
		t.Fatalf("missing should be empty during pending; got %+v", missing)
	}
}

func TestCompute_FailWhenPlanMissing(t *testing.T) {
	runID, runs, arts, ar := happyPath(t)
	planID := runs.stages[0].ID
	delete(arts.byStage, planID)
	state, missing, _ := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if state != stagecheck.StateFail {
		t.Fatalf("state = %s want fail", state)
	}
	if !containsKind(missing, auditcomplete.MissingPlan) {
		t.Fatalf("missing did not include plan_missing: %+v", missing)
	}
}

func TestCompute_FailWhenPlanWrongSchemaVersion(t *testing.T) {
	runID, runs, arts, ar := happyPath(t)
	planID := runs.stages[0].ID
	arts.byStage[planID] = []*artifact.Artifact{planArtifact(planID, "draft_v0")}
	state, missing, _ := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if state != stagecheck.StateFail {
		t.Fatalf("state = %s want fail", state)
	}
	if !containsKind(missing, auditcomplete.MissingPlan) {
		t.Fatalf("missing did not include plan_missing: %+v", missing)
	}
}

func TestCompute_FailWhenRedactedTraceMissing(t *testing.T) {
	runID, runs, arts, ar := happyPath(t)
	implID := runs.stages[1].ID
	// Drop the implement stage's redacted trace entry.
	ar.dropEntry(func(e *audit.Entry) bool {
		if e.StageID == nil || *e.StageID != implID {
			return false
		}
		if e.Category != "trace_uploaded" {
			return false
		}
		return string(e.Payload) == string(traceVariantPayload("redacted"))
	})
	state, missing, _ := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if state != stagecheck.StateFail {
		t.Fatalf("state = %s want fail", state)
	}
	if !containsKind(missing, auditcomplete.MissingTrace) {
		t.Fatalf("missing did not include trace_missing: %+v", missing)
	}
}

func TestCompute_FailWhenStageHasNoTraceEntry(t *testing.T) {
	runID, runs, arts, ar := happyPath(t)
	implID := runs.stages[1].ID
	ar.dropEntry(func(e *audit.Entry) bool {
		return e.StageID != nil && *e.StageID == implID && e.Category == "trace_uploaded"
	})
	state, missing, _ := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if state != stagecheck.StateFail {
		t.Fatalf("state = %s want fail", state)
	}
	if !containsKind(missing, auditcomplete.MissingTrace) {
		t.Fatalf("missing did not include trace_missing: %+v", missing)
	}
}

func TestCompute_FailWhenPullRequestMissing(t *testing.T) {
	runID, runs, arts, ar := happyPath(t)
	implID := runs.stages[1].ID
	delete(arts.byStage, implID)
	state, missing, _ := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if state != stagecheck.StateFail {
		t.Fatalf("state = %s want fail", state)
	}
	if !containsKind(missing, auditcomplete.MissingPullRequest) {
		t.Fatalf("missing did not include pr_missing: %+v", missing)
	}
}

func TestCompute_FailWhenChainTampered(t *testing.T) {
	runID, runs, arts, ar := happyPath(t)
	// Mutate the second entry's payload after-the-fact: this
	// breaks the recomputed hash without changing the stored
	// EntryHash.
	ar.entries[1].Payload = json.RawMessage(`{"variant":"raw","tampered":true}`)
	state, missing, _ := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if state != stagecheck.StateFail {
		t.Fatalf("state = %s want fail", state)
	}
	if !containsKind(missing, auditcomplete.MissingChain) {
		t.Fatalf("missing did not include chain_invalid: %+v", missing)
	}
}

func TestCompute_PassWithoutPlanStage(t *testing.T) {
	// routine_change-shaped run: implement only, no plan, no review.
	runID := uuid.New()
	impl := mkStage(runID, 1, run.StageTypeImplement, run.StageStateSucceeded)
	runs := &fakeRuns{stages: []*run.Stage{impl}}
	arts := &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{
		impl.ID: {pullRequestArtifact(impl.ID)},
	}}
	ar := &fakeAudit{}
	ar.appendChained(t, runID, &impl.ID, "trace_uploaded", traceVariantPayload("raw"))
	ar.appendChained(t, runID, &impl.ID, "trace_uploaded", traceVariantPayload("redacted"))

	state, missing, err := auditcomplete.Compute(context.Background(), runID, deps(runs, arts, ar))
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if state != stagecheck.StatePass {
		t.Fatalf("state = %s want pass; missing=%+v", state, missing)
	}
}

func TestCompute_ListStagesError(t *testing.T) {
	runID := uuid.New()
	runs := &fakeRuns{listErr: errors.New("db down")}
	state, _, err := auditcomplete.Compute(context.Background(), runID, deps(runs, &fakeArtifacts{}, &fakeAudit{}))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if state != stagecheck.StatePending {
		t.Fatalf("state = %s want pending on error path", state)
	}
}

func TestCompute_NilDeps(t *testing.T) {
	_, _, err := auditcomplete.Compute(context.Background(), uuid.New(), auditcomplete.Deps{})
	if err == nil {
		t.Fatalf("expected error from nil deps")
	}
}

// --- helpers ---

func deps(r *fakeRuns, a *fakeArtifacts, au *fakeAudit) auditcomplete.Deps {
	return auditcomplete.Deps{Runs: r, Artifacts: a, Audit: au}
}

func mkStage(runID uuid.UUID, seq int, typ run.StageType, state run.StageState) *run.Stage {
	return &run.Stage{
		ID:       uuid.New(),
		RunID:    runID,
		Sequence: seq,
		Type:     typ,
		State:    state,
	}
}

func planArtifact(stageID uuid.UUID, schemaVersion string) *artifact.Artifact {
	v := schemaVersion
	return &artifact.Artifact{
		ID:            uuid.New(),
		StageID:       stageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &v,
		Content:       json.RawMessage(`{}`),
	}
}

func pullRequestArtifact(stageID uuid.UUID) *artifact.Artifact {
	return &artifact.Artifact{
		ID:      uuid.New(),
		StageID: stageID,
		Kind:    artifact.KindPullRequest,
		Content: json.RawMessage(`{}`),
	}
}

func traceVariantPayload(variant string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"variant": variant})
	return b
}

func containsKind(items []auditcomplete.MissingItem, kind auditcomplete.MissingKind) bool {
	for _, it := range items {
		if it.Kind == kind {
			return true
		}
	}
	return false
}

// --- fakes ---

type fakeRuns struct {
	run.Repository // embed for the methods we don't care about (panic on call is fine)
	stages         []*run.Stage
	listErr        error
}

func (f *fakeRuns) ListStagesForRun(_ context.Context, _ uuid.UUID) ([]*run.Stage, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.stages, nil
}

type fakeArtifacts struct {
	artifact.Repository
	byStage map[uuid.UUID][]*artifact.Artifact
}

func (f *fakeArtifacts) ListForStage(_ context.Context, stageID uuid.UUID) ([]*artifact.Artifact, error) {
	return f.byStage[stageID], nil
}

type fakeAudit struct {
	audit.Repository
	entries []*audit.Entry
}

// appendChained mirrors what the real audit.Repository.AppendChained
// does at the integrity layer: compute the canonical hash, link
// prev → entry. Tests use this so the synthetic chain is identical
// in shape to the production one and verifyChain agrees.
func (f *fakeAudit) appendChained(t *testing.T, runID uuid.UUID, stageID *uuid.UUID, category string, payload json.RawMessage) {
	t.Helper()
	if payload == nil {
		payload = json.RawMessage(`{}`)
	}
	var prev *string
	if n := len(f.entries); n > 0 {
		ph := f.entries[n-1].EntryHash
		prev = &ph
	}
	r := runID
	ts := time.Date(2026, 5, 7, 12, 0, int(len(f.entries)), 0, time.UTC)
	hash, err := audit.ComputeEntryHash(audit.HashInputs{
		RunID:        &r,
		StageID:      stageID,
		Timestamp:    ts,
		Category:     category,
		ActorKind:    nil,
		ActorSubject: nil,
		Payload:      payload,
		PrevHash:     prev,
	})
	if err != nil {
		t.Fatalf("ComputeEntryHash: %v", err)
	}
	f.entries = append(f.entries, &audit.Entry{
		ID:        uuid.New(),
		Sequence:  int64(len(f.entries) + 1),
		RunID:     &r,
		StageID:   stageID,
		Timestamp: ts,
		Category:  category,
		Payload:   payload,
		PrevHash:  prev,
		EntryHash: hash,
	})
}

func (f *fakeAudit) dropEntry(pred func(*audit.Entry) bool) {
	out := f.entries[:0]
	for _, e := range f.entries {
		if !pred(e) {
			out = append(out, e)
		}
	}
	f.entries = out
}

func (f *fakeAudit) ListForRun(_ context.Context, _ uuid.UUID) ([]*audit.Entry, error) {
	return f.entries, nil
}

func (f *fakeAudit) ListForRunByCategory(_ context.Context, _ uuid.UUID, category string) ([]*audit.Entry, error) {
	out := []*audit.Entry{}
	for _, e := range f.entries {
		if e.Category == category {
			out = append(out, e)
		}
	}
	return out, nil
}
