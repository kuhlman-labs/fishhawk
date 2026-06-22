package stagecheck_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

func ptr[T any](v T) *T { return &v }

// seedStage creates a run + stage in one shot so the stage_checks
// FK to stages.id resolves.
func seedStage(t *testing.T, pool *pgxpool.Pool) (runID, stageID uuid.UUID) {
	t.Helper()
	repo := run.NewPostgresRepository(pool)
	r, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	s, err := repo.CreateStage(context.Background(), run.CreateStageParams{
		RunID:        r.ID,
		Sequence:     0,
		Type:         run.StageTypeReview,
		ExecutorKind: run.ExecutorHuman,
		ExecutorRef:  "human",
		Gate: &run.Gate{
			Kind: run.GateKindApproval,
		},
	})
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}
	return r.ID, s.ID
}

func TestAppend_LatestForStageAndName_RoundTrip(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := stagecheck.NewPostgresRepository(pool)
	_, stageID := seedStage(t, pool)

	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	if _, err := repo.Append(context.Background(), stagecheck.AppendParams{
		StageID:    stageID,
		Name:       "ci_pass",
		Status:     "completed",
		Conclusion: ptr("success"),
		HeadSHA:    "abc123",
		Timestamp:  now,
		Payload:    json.RawMessage(`{"foo":"bar"}`),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := repo.LatestForStageAndName(context.Background(), stageID, "ci_pass")
	if err != nil {
		t.Fatalf("LatestForStageAndName: %v", err)
	}
	if got.State != stagecheck.StatePass {
		t.Errorf("State = %q, want pass", got.State)
	}
	if got.HeadSHA != "abc123" {
		t.Errorf("HeadSHA = %q", got.HeadSHA)
	}
	if !strings.Contains(string(got.Payload), `"foo"`) || !strings.Contains(string(got.Payload), `"bar"`) {
		t.Errorf("Payload not preserved: %s", got.Payload)
	}
}

func TestLatestForStageAndName_NotFound(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := stagecheck.NewPostgresRepository(pool)
	_, stageID := seedStage(t, pool)

	_, err := repo.LatestForStageAndName(context.Background(), stageID, "ci_pass")
	if !errors.Is(err, stagecheck.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestLatestForStageAndName_PicksMostRecent(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := stagecheck.NewPostgresRepository(pool)
	_, stageID := seedStage(t, pool)

	older := time.Date(2026, 5, 7, 11, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)

	// CI started in_progress, then completed as failure. Latest
	// row wins; the gate should refuse approval.
	if _, err := repo.Append(context.Background(), stagecheck.AppendParams{
		StageID: stageID, Name: "ci_pass",
		Status: "in_progress", HeadSHA: "abc", Timestamp: older,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Append(context.Background(), stagecheck.AppendParams{
		StageID: stageID, Name: "ci_pass",
		Status: "completed", Conclusion: ptr("failure"),
		HeadSHA: "abc", Timestamp: newer,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := repo.LatestForStageAndName(context.Background(), stageID, "ci_pass")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != stagecheck.StateFail {
		t.Errorf("State = %q, want fail (latest row wins)", got.State)
	}
}

func TestLatestForStage_OneRowPerCheckName(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := stagecheck.NewPostgresRepository(pool)
	_, stageID := seedStage(t, pool)

	now := time.Now().UTC()
	for _, name := range []string{"ci_pass", "fishhawk_audit_complete"} {
		for i := 0; i < 3; i++ {
			if _, err := repo.Append(context.Background(), stagecheck.AppendParams{
				StageID: stageID, Name: name,
				Status:     "completed",
				Conclusion: ptr("success"),
				HeadSHA:    "abc",
				Timestamp:  now.Add(time.Duration(i) * time.Second),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	out, err := repo.LatestForStage(context.Background(), stageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Errorf("len = %d, want 2 (ci_pass + fishhawk_audit_complete)", len(out))
	}
}

func TestFindMatchingStages_FiltersByPRAndCheck(t *testing.T) {
	pool := pgtest.NewPool(t)
	scRepo := stagecheck.NewPostgresRepository(pool)
	runRepo := run.NewPostgresRepository(pool)
	artRepo := artifact.NewPostgresRepository(pool)

	// Create a run with two stages: implement (carries the PR
	// artifact) + review (carries the gate).
	r, err := runRepo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "deadbeef", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatal(err)
	}
	implStage, err := runRepo.CreateStage(context.Background(), run.CreateStageParams{
		RunID: r.ID, Sequence: 0, Type: run.StageTypeImplement,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatal(err)
	}
	reviewStage, err := runRepo.CreateStage(context.Background(), run.CreateStageParams{
		RunID: r.ID, Sequence: 1, Type: run.StageTypeReview,
		ExecutorKind: run.ExecutorHuman, ExecutorRef: "human",
		Gate: &run.Gate{
			Kind: run.GateKindApproval,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	prBody, _ := json.Marshal(map[string]any{
		"pr_number":           42,
		"head_sha":            "abc123",
		"branch":              "feat",
		"base_sha":            "def456",
		"title":               "x",
		"files_changed_count": 1,
	})
	if _, err := artRepo.Create(context.Background(), artifact.CreateParams{
		StageID:     implStage.ID,
		Kind:        artifact.KindPlan, // see note below
		Content:     prBody,
		ContentHash: "hashplan",
	}); err != nil {
		// We expect this insert to be rejected if the kind is
		// validated; ignore the artifact-validation result and
		// proceed with the real (kind=pull_request) insert.
		_ = err
	}
	prArt, err := artRepo.Create(context.Background(), artifact.CreateParams{
		StageID:     implStage.ID,
		Kind:        artifact.KindPullRequest,
		Content:     prBody,
		ContentHash: "hashpr",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = prArt

	// Match by (pr=42, sha=abc123, check=ci_pass) → review stage.
	stages, err := scRepo.FindMatchingStages(context.Background(), 42, "abc123", "ci_pass")
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 1 || stages[0] != reviewStage.ID {
		t.Errorf("expected [%v], got %v", reviewStage.ID, stages)
	}

	// Wrong PR number: no match.
	stages, _ = scRepo.FindMatchingStages(context.Background(), 99, "abc123", "ci_pass")
	if len(stages) != 0 {
		t.Errorf("expected empty, got %v", stages)
	}
	// Wrong head_sha: no match.
	stages, _ = scRepo.FindMatchingStages(context.Background(), 42, "ffffff", "ci_pass")
	if len(stages) != 0 {
		t.Errorf("expected empty, got %v", stages)
	}
	// Empty check name is a no-op (the SQL guard against the empty
	// string from the migrated-away gate.blocking_checks shape).
	stages, _ = scRepo.FindMatchingStages(context.Background(), 42, "abc123", "")
	if len(stages) != 0 {
		t.Errorf("empty check_name should match nothing, got %v", stages)
	}

	// Post-#254 (ADR-017): the query matches by stage_type = 'review'
	// rather than the dropped gate.blocking_checks list. Any check
	// name reported against a review stage's PR head_sha records,
	// since branch protection is now the authority on which checks
	// matter — Fishhawk just records what GitHub tells us.
	stages, _ = scRepo.FindMatchingStages(context.Background(), 42, "abc123", "some_other")
	if len(stages) != 1 || stages[0] != reviewStage.ID {
		t.Errorf("post-#254 the check_name no longer filters; got %v want [%v]",
			stages, reviewStage.ID)
	}
}
