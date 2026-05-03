package approval_test

import (
	"context"
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

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// startPostgres mirrors the helper in internal/run/postgres_test —
// kept duplicated rather than DRY-extracted so each integration
// suite stays self-contained.
func startPostgres(t *testing.T) *pgxpool.Pool {
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
			t.Skipf("Docker not available; skipping: %v", err)
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
	return os.Getenv("FISHHAWK_SKIP_INTEGRATION") != ""
}

// seedRunAndStage inserts a run + stage so foreign-key constraints
// on approvals are satisfied. Approvals reference stages.id, which
// references runs.id, so we go top-down.
func seedRunAndStage(t *testing.T, pool *pgxpool.Pool) (uuid.UUID, uuid.UUID) {
	t.Helper()
	runRepo := run.NewPostgresRepository(pool)
	r, err := runRepo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "x/y", WorkflowID: "w", WorkflowSHA: "sha",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	st, err := runRepo.CreateStage(context.Background(), run.CreateStageParams{
		RunID: r.ID, Sequence: 0, Type: run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}
	return r.ID, st.ID
}

func TestPostgres_Submit_HappyPath(t *testing.T) {
	pool := startPostgres(t)
	repo := approval.NewPostgresRepository(pool)
	_, stageID := seedRunAndStage(t, pool)

	res, err := repo.Submit(context.Background(), approval.SubmitParams{
		StageID:         stageID,
		ApproverSubject: "@kuhlman-labs",
		Decision:        approval.DecisionApprove,
		Surface:         approval.SurfaceUI,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !res.Inserted {
		t.Errorf("Inserted = false, want true on first submit")
	}
	if res.Approval.Decision != approval.DecisionApprove {
		t.Errorf("Decision = %q", res.Approval.Decision)
	}
}

func TestPostgres_Submit_Idempotent(t *testing.T) {
	pool := startPostgres(t)
	repo := approval.NewPostgresRepository(pool)
	_, stageID := seedRunAndStage(t, pool)

	// First submission: inserted.
	first, err := repo.Submit(context.Background(), approval.SubmitParams{
		StageID:         stageID,
		ApproverSubject: "@a",
		Decision:        approval.DecisionApprove,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Inserted {
		t.Error("first Inserted = false")
	}

	// Re-submit (different decision, same approver) → returns
	// existing row, Inserted=false.
	second, err := repo.Submit(context.Background(), approval.SubmitParams{
		StageID:         stageID,
		ApproverSubject: "@a",
		Decision:        approval.DecisionReject,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Inserted {
		t.Error("second Inserted = true, want false")
	}
	if second.Approval.ID != first.Approval.ID {
		t.Errorf("ID changed: %s vs %s", second.Approval.ID, first.Approval.ID)
	}
	if second.Approval.Decision != approval.DecisionApprove {
		t.Errorf("Decision changed: %q (first decision should win)", second.Approval.Decision)
	}
}

func TestPostgres_Submit_DifferentApprovers(t *testing.T) {
	pool := startPostgres(t)
	repo := approval.NewPostgresRepository(pool)
	_, stageID := seedRunAndStage(t, pool)

	for _, who := range []string{"@a", "@b", "@c"} {
		res, err := repo.Submit(context.Background(), approval.SubmitParams{
			StageID: stageID, ApproverSubject: who,
			Decision: approval.DecisionApprove,
		})
		if err != nil {
			t.Fatalf("Submit(%s): %v", who, err)
		}
		if !res.Inserted {
			t.Errorf("Submit(%s).Inserted = false, want true", who)
		}
	}
	all, err := repo.ListForStage(context.Background(), stageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("ListForStage = %d, want 3", len(all))
	}
}

func TestPostgres_Submit_AppendOnlyEnforced(t *testing.T) {
	// The DB triggers refuse direct UPDATE / DELETE on approvals.
	// Repository surfaces neither method, but a hand-written UPDATE
	// should also fail at the DB layer.
	pool := startPostgres(t)
	repo := approval.NewPostgresRepository(pool)
	_, stageID := seedRunAndStage(t, pool)

	res, _ := repo.Submit(context.Background(), approval.SubmitParams{
		StageID: stageID, ApproverSubject: "@a",
		Decision: approval.DecisionApprove,
	})

	_, err := pool.Exec(context.Background(),
		`UPDATE approvals SET decision = 'reject' WHERE id = $1`, res.Approval.ID)
	if err == nil {
		t.Error("UPDATE on approvals succeeded; trigger should have refused")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("err = %v, want append-only refusal", err)
	}

	_, err = pool.Exec(context.Background(),
		`DELETE FROM approvals WHERE id = $1`, res.Approval.ID)
	if err == nil {
		t.Error("DELETE on approvals succeeded; trigger should have refused")
	}
}

func TestPostgres_Submit_ValidationErrors(t *testing.T) {
	pool := startPostgres(t)
	repo := approval.NewPostgresRepository(pool)
	cases := []struct {
		name string
		p    approval.SubmitParams
		want error
	}{
		{"bad decision", approval.SubmitParams{
			StageID: uuid.New(), ApproverSubject: "@a",
			Decision: "maybe",
		}, approval.ErrInvalidDecision},
		{"bad surface", approval.SubmitParams{
			StageID: uuid.New(), ApproverSubject: "@a",
			Decision: approval.DecisionApprove, Surface: "unknown",
		}, approval.ErrInvalidSurface},
		{"empty approver", approval.SubmitParams{
			StageID:  uuid.New(),
			Decision: approval.DecisionApprove,
		}, approval.ErrEmptyApprover},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := repo.Submit(context.Background(), tc.p)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestPostgres_ListForStage_OrderedBySubmitTime(t *testing.T) {
	pool := startPostgres(t)
	repo := approval.NewPostgresRepository(pool)
	_, stageID := seedRunAndStage(t, pool)

	for _, who := range []string{"@first", "@second", "@third"} {
		if _, err := repo.Submit(context.Background(), approval.SubmitParams{
			StageID: stageID, ApproverSubject: who,
			Decision: approval.DecisionApprove,
		}); err != nil {
			t.Fatal(err)
		}
		// Tiny pause so submitted_at is strictly increasing
		// without relying on default-microsecond clock resolution.
		time.Sleep(2 * time.Millisecond)
	}
	all, err := repo.ListForStage(context.Background(), stageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d, want 3", len(all))
	}
	want := []string{"@first", "@second", "@third"}
	for i, a := range all {
		if a.ApproverSubject != want[i] {
			t.Errorf("[%d] = %q, want %q", i, a.ApproverSubject, want[i])
		}
	}
}
