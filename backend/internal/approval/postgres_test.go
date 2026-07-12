package approval_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

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
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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

// TestPostgres_Submit_ConcurrentQuorum is the #1734 read-after-write quorum
// regression guard. It fires N concurrent distinct-approver submissions against
// one stage and proves the liveness property that closes #1734 WITHOUT
// serialization: because Submit commits its row (autocommit) before returning
// and every count read hits the single primary under READ COMMITTED, the
// last-committing goroutine's post-Submit ListForStage always observes the full
// N-approver quorum. The reviewer's "each approver sees only its own row so the
// gate never advances" stall is a temporal contradiction and cannot occur.
//
// The in-memory fake (server.fakeApprovalRepo) serializes every Submit under a
// mutex and cannot reproduce Postgres commit semantics, so this MUST be
// pgtest-backed. Stress with `-race -count=50` to shake out any ordering flake.
func TestPostgres_Submit_ConcurrentQuorum(t *testing.T) {
	const n = 4
	pool := pgtest.NewPool(t)
	repo := approval.NewPostgresRepository(pool)
	_, stageID := seedRunAndStage(t, pool)

	// Each goroutine submits a DISTINCT approver, then reads back the stage's
	// approval count it observes immediately after its own commit. A released
	// start barrier maximizes interleaving so the submissions genuinely race.
	var wg sync.WaitGroup
	start := make(chan struct{})
	inserted := make([]bool, n)
	observed := make([]int, n)
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := repo.Submit(context.Background(), approval.SubmitParams{
				StageID:         stageID,
				ApproverSubject: fmt.Sprintf("@r%d", i),
				Decision:        approval.DecisionApprove,
			})
			if err != nil {
				errs[i] = err
				return
			}
			inserted[i] = res.Inserted
			// Read-after-write: the count this approver observes once its own
			// row is committed.
			rows, err := repo.ListForStage(context.Background(), stageID)
			if err != nil {
				errs[i] = err
				return
			}
			observed[i] = len(rows)
		}(i)
	}
	close(start)
	wg.Wait()

	// Every distinct approver inserted a fresh row.
	maxObserved := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if !inserted[i] {
			t.Errorf("goroutine %d: Inserted = false, want true (distinct approver)", i)
		}
		if observed[i] > maxObserved {
			maxObserved = observed[i]
		}
	}

	// Liveness / no-stall: the last-committing approver's post-Submit count
	// observed the FULL quorum. If concurrent counts could each see only their
	// own row (the #1734 stall), maxObserved would be < n.
	if maxObserved != n {
		t.Errorf("max observed post-Submit count = %d, want %d (last committer must see full quorum — no stall)", maxObserved, n)
	}

	// Durability: all N rows persisted.
	all, err := repo.ListForStage(context.Background(), stageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != n {
		t.Errorf("final ListForStage = %d rows, want %d (all concurrent submissions durable)", len(all), n)
	}
}

func TestPostgres_Submit_ValidationErrors(t *testing.T) {
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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
