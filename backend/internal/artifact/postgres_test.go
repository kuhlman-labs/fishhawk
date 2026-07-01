package artifact_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// makeStage creates a run + a stage, returning the stage's UUID so
// the artifact tests have a valid foreign-key target.
func makeStage(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
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
		Type:         run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent,
		ExecutorRef:  "claude-code",
	})
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}
	return s.ID
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestPostgres_CreateAndGetArtifact(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := artifact.NewPostgresRepository(pool)
	stageID := makeStage(t, pool)

	body, err := json.Marshal(map[string]any{
		"plan_version": "standard_v1",
		"summary":      "test plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	v := "standard_v1"
	created, err := repo.Create(context.Background(), artifact.CreateParams{
		StageID:       stageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &v,
		Content:       body,
		ContentHash:   sha256Hex(body),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Kind != artifact.KindPlan {
		t.Errorf("Kind = %q, want plan", created.Kind)
	}

	got, err := repo.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("id mismatch")
	}
	// Postgres normalizes JSONB whitespace; compare via decoded value
	// rather than raw bytes.
	var roundTripped map[string]any
	if err := json.Unmarshal(got.Content, &roundTripped); err != nil {
		t.Fatalf("decode round-tripped content: %v", err)
	}
	if roundTripped["plan_version"] != "standard_v1" || roundTripped["summary"] != "test plan" {
		t.Errorf("decoded content = %v, want {plan_version: standard_v1, summary: test plan}", roundTripped)
	}
}

// TestPostgres_CreateAndGet_DeploymentKind pins E23.5 / #1385: the
// `deployment` artifact kind (ADR-038) round-trips through Create + Get +
// ListForStage. This is the real-DB assertion that migration 0037 widened
// artifacts_kind_check to admit it — the pgtest template auto-applies 0037,
// so a Create with KindDeployment that would fail SQLSTATE 23514 against the
// un-migrated two-value CHECK succeeds here.
func TestPostgres_CreateAndGet_DeploymentKind(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := artifact.NewPostgresRepository(pool)
	stageID := makeStage(t, pool)

	body, err := json.Marshal(map[string]any{
		"environment":      "production",
		"ref":              "1111111111111111111111111111111111111111",
		"external_run_url": "https://github.com/kuhlman-labs/fishhawk/actions/runs/42",
		"outcome":          "succeeded",
		"rollback_handle":  "deploy-42",
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := repo.Create(context.Background(), artifact.CreateParams{
		StageID:     stageID,
		Kind:        artifact.KindDeployment,
		Content:     body,
		ContentHash: sha256Hex(body),
	})
	if err != nil {
		t.Fatalf("Create deployment artifact (migration 0037 must admit the kind): %v", err)
	}
	if created.Kind != artifact.KindDeployment {
		t.Errorf("Kind = %q, want deployment", created.Kind)
	}

	got, err := repo.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Kind != artifact.KindDeployment {
		t.Errorf("round-tripped Kind = %q, want deployment", got.Kind)
	}

	listed, err := repo.ListForStage(context.Background(), stageID)
	if err != nil {
		t.Fatalf("ListForStage: %v", err)
	}
	var found bool
	for _, a := range listed {
		if a.ID == created.ID && a.Kind == artifact.KindDeployment {
			found = true
		}
	}
	if !found {
		t.Errorf("ListForStage did not return the deployment artifact; got %d artifacts", len(listed))
	}
}

// TestPostgres_CreateAndGet_AcceptanceKind pins E31.3 / #1531: the
// `acceptance` artifact kind (ADR-049) round-trips through Create + Get +
// ListForStage. This is the real-DB assertion that migration 0045 widened
// artifacts_kind_check to admit it — the pgtest template auto-applies 0045,
// so a Create with KindAcceptance that would fail SQLSTATE 23514 against the
// un-migrated three-value CHECK succeeds here. The constant + migration
// coupling gate: if either is missing, this test fails.
func TestPostgres_CreateAndGet_AcceptanceKind(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := artifact.NewPostgresRepository(pool)
	stageID := makeStage(t, pool)

	body, err := json.Marshal(map[string]any{
		"outcome": "accepted",
		"criteria": []map[string]any{
			{"id": "AC-1", "passed": true, "evidence_hash": "abc123"},
			{"id": "AC-2", "passed": true, "evidence_hash": "def456"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := repo.Create(context.Background(), artifact.CreateParams{
		StageID:     stageID,
		Kind:        artifact.KindAcceptance,
		Content:     body,
		ContentHash: sha256Hex(body),
	})
	if err != nil {
		t.Fatalf("Create acceptance artifact (migration 0045 must admit the kind): %v", err)
	}
	if created.Kind != artifact.KindAcceptance {
		t.Errorf("Kind = %q, want acceptance", created.Kind)
	}

	got, err := repo.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Kind != artifact.KindAcceptance {
		t.Errorf("round-tripped Kind = %q, want acceptance", got.Kind)
	}

	listed, err := repo.ListForStage(context.Background(), stageID)
	if err != nil {
		t.Fatalf("ListForStage: %v", err)
	}
	var found bool
	for _, a := range listed {
		if a.ID == created.ID && a.Kind == artifact.KindAcceptance {
			found = true
		}
	}
	if !found {
		t.Errorf("ListForStage did not return the acceptance artifact; got %d artifacts", len(listed))
	}
}

func TestPostgres_GetArtifact_NotFound(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := artifact.NewPostgresRepository(pool)

	_, err := repo.Get(context.Background(), uuid.New())
	if !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestPostgres_GetByHash_DeduplicatesIdenticalContent(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := artifact.NewPostgresRepository(pool)
	stageID := makeStage(t, pool)

	body := []byte(`{"summary":"x"}`)
	hash := sha256Hex(body)
	first, err := repo.Create(context.Background(), artifact.CreateParams{
		StageID:     stageID,
		Kind:        artifact.KindPlan,
		Content:     body,
		ContentHash: hash,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := repo.GetByHash(context.Background(), stageID, hash)
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}
	if got.ID != first.ID {
		t.Errorf("dedup miss; expected first.ID = %s, got %s", first.ID, got.ID)
	}
}

func TestPostgres_GetByHash_NotFound(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := artifact.NewPostgresRepository(pool)
	stageID := makeStage(t, pool)

	_, err := repo.GetByHash(context.Background(), stageID, "no-such-hash")
	if !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestPostgres_ListForStage_OrderedByCreatedAt(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := artifact.NewPostgresRepository(pool)
	stageID := makeStage(t, pool)

	for i := 0; i < 3; i++ {
		body := []byte(`{"i":` + string(rune('0'+i)) + `}`)
		if _, err := repo.Create(context.Background(), artifact.CreateParams{
			StageID:     stageID,
			Kind:        artifact.KindPlan,
			Content:     body,
			ContentHash: sha256Hex(body),
		}); err != nil {
			t.Fatal(err)
		}
		// Tiny pause to ensure created_at differs at the
		// microsecond resolution Postgres uses for timestamptz.
		time.Sleep(2 * time.Millisecond)
	}

	got, err := repo.ListForStage(context.Background(), stageID)
	if err != nil {
		t.Fatalf("ListForStage: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].CreatedAt.Before(got[i-1].CreatedAt) {
			t.Errorf("ordering violation: created[%d] < created[%d]", i, i-1)
		}
	}
}
