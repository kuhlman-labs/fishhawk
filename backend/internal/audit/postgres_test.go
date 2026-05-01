package audit_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

func startContainer(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	c, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("fishhawk"),
		tcpostgres.WithUsername("fishhawk"),
		tcpostgres.WithPassword("fishhawk"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		if isDockerUnavailable(err) {
			t.Skipf("Docker not available; skipping integration test: %v", err)
		}
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = c.Terminate(ctx)
	})

	url, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("conn string: %v", err)
	}
	if err := postgres.MigrateUp(url); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	pool, err := postgres.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func isDockerUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if os.Getenv("FISHHAWK_SKIP_INTEGRATION") != "" {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"cannot connect to the docker daemon",
		"docker: not found",
		"executable file not found",
		"dial unix /var/run/docker.sock",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// makeRun creates a parent run that the audit-entry tests can attach
// to (audit_entries has a non-nullable run_id FK).
func makeRun(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
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
	return r.ID
}

// makeStageInRun adds a stage under an existing run so audit
// entries can carry a non-nil StageID.
func makeStageInRun(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) uuid.UUID {
	t.Helper()
	repo := run.NewPostgresRepository(pool)
	s, err := repo.CreateStage(context.Background(), run.CreateStageParams{
		RunID:        runID,
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

func entryHash(seq int64, payload []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(time.Now().Format(time.RFC3339Nano)))
	_, _ = h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}

func appendEntry(t *testing.T, repo audit.Repository, runID uuid.UUID, category string, prev *string) *audit.Entry {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"event": category})
	e, err := repo.Append(context.Background(), audit.AppendParams{
		RunID:     runID,
		Timestamp: time.Now().UTC(),
		Category:  category,
		Payload:   body,
		PrevHash:  prev,
		EntryHash: entryHash(0, body),
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return e
}

func TestPostgres_AppendAndGet(t *testing.T) {
	pool := startContainer(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	first := appendEntry(t, repo, runID, "plan_generated", nil)
	if first.Sequence == 0 {
		t.Errorf("Sequence = 0, want positive bigserial value")
	}
	if first.Category != "plan_generated" {
		t.Errorf("Category = %q", first.Category)
	}

	got, err := repo.Get(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Sequence != first.Sequence {
		t.Errorf("Sequence mismatch: %d vs %d", got.Sequence, first.Sequence)
	}
}

func TestPostgres_Get_NotFound(t *testing.T) {
	pool := startContainer(t)
	repo := audit.NewPostgresRepository(pool)

	_, err := repo.Get(context.Background(), uuid.New())
	if !errors.Is(err, audit.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestPostgres_ListForRun_OrderedBySequence(t *testing.T) {
	pool := startContainer(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	prev := (*string)(nil)
	for i := 0; i < 5; i++ {
		e := appendEntry(t, repo, runID, "x", prev)
		eh := e.EntryHash
		prev = &eh
	}

	got, err := repo.ListForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListForRun: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("len = %d, want 5", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Sequence <= got[i-1].Sequence {
			t.Errorf("non-monotonic at %d: %d <= %d", i, got[i].Sequence, got[i-1].Sequence)
		}
	}
}

func TestPostgres_LastForRun(t *testing.T) {
	pool := startContainer(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	// Empty run → ErrNotFound.
	if _, err := repo.LastForRun(context.Background(), runID); !errors.Is(err, audit.ErrNotFound) {
		t.Fatalf("LastForRun on empty run: err = %v, want ErrNotFound", err)
	}

	a := appendEntry(t, repo, runID, "first", nil)
	b := appendEntry(t, repo, runID, "second", &a.EntryHash)

	last, err := repo.LastForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LastForRun: %v", err)
	}
	if last.ID != b.ID {
		t.Errorf("LastForRun returned %s, want last (%s)", last.ID, b.ID)
	}
}

func TestPostgres_ListForRunByCategory(t *testing.T) {
	pool := startContainer(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	appendEntry(t, repo, runID, "plan_generated", nil)
	appendEntry(t, repo, runID, "gate_passed", nil)
	appendEntry(t, repo, runID, "plan_generated", nil)
	appendEntry(t, repo, runID, "failure", nil)

	got, err := repo.ListForRunByCategory(context.Background(), runID, "plan_generated")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
	for _, e := range got {
		if e.Category != "plan_generated" {
			t.Errorf("Category = %q, want plan_generated", e.Category)
		}
	}
}

// TestPostgres_AppendWithActor exercises the ActorKind / ActorSubject
// fields that the simpler appendEntry helper leaves nil. Confirms
// they round-trip through the column NULL handling cleanly.
func TestPostgres_AppendWithActor(t *testing.T) {
	pool := startContainer(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	body, _ := json.Marshal(map[string]string{"who": "approved"})
	subj := "user@example.com"
	kind := audit.ActorUser
	e, err := repo.Append(context.Background(), audit.AppendParams{
		RunID:        runID,
		Timestamp:    time.Now().UTC(),
		Category:     "approval",
		ActorKind:    &kind,
		ActorSubject: &subj,
		Payload:      body,
		EntryHash:    entryHash(0, body),
	})
	if err != nil {
		t.Fatalf("Append with actor: %v", err)
	}

	got, err := repo.Get(context.Background(), e.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ActorKind == nil || *got.ActorKind != audit.ActorUser {
		t.Errorf("ActorKind = %v, want ActorUser", got.ActorKind)
	}
	if got.ActorSubject == nil || *got.ActorSubject != subj {
		t.Errorf("ActorSubject = %v, want %q", got.ActorSubject, subj)
	}
}

// TestPostgres_TriggerBlocksUpdate is the load-bearing assertion for
// audit_entries' append-only invariant. The Repository interface
// doesn't expose Update; this test goes around the API directly to
// the database to confirm the trigger fires regardless.
func TestPostgres_TriggerBlocksUpdate(t *testing.T) {
	pool := startContainer(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)
	e := appendEntry(t, repo, runID, "x", nil)

	_, err := pool.Exec(context.Background(),
		`UPDATE audit_entries SET category = 'tampered' WHERE id = $1`, e.ID)
	if err == nil {
		t.Fatal("UPDATE on audit_entries should be blocked by the trigger")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("trigger error = %v, want 'append-only' substring", err)
	}
}

// TestPostgres_TriggerBlocksDelete pairs with the UPDATE test —
// neither mutation is permitted on the audit log.
func TestPostgres_TriggerBlocksDelete(t *testing.T) {
	pool := startContainer(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)
	e := appendEntry(t, repo, runID, "x", nil)

	_, err := pool.Exec(context.Background(),
		`DELETE FROM audit_entries WHERE id = $1`, e.ID)
	if err == nil {
		t.Fatal("DELETE on audit_entries should be blocked by the trigger")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("trigger error = %v, want 'append-only' substring", err)
	}
}
