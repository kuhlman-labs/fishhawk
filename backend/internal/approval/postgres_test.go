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

// TestPostgres_Submit_ConcurrentQuorum is the liveness / no-stall regression
// for the deferred #1734 quorum read-after-write concern. It fires N concurrent
// DISTINCT-approver submissions against one stage and proves the last committer
// always observes the full quorum: every Submit inserts its own row, exactly N
// rows persist, and at least one goroutine's post-Submit ListForStage count
// observed all N.
//
// ApprovalRepo.Submit autocommits its INSERT (pool-backed, single statement)
// BEFORE the count reads it, and both run against a single Postgres primary
// under READ COMMITTED, so the reviewer's "two approvers each see only their
// own row" stall is a temporal contradiction and cannot occur (see the
// countDistinctEligibleApprovers doc in backend/internal/server/quorum.go).
// The in-memory fake serializes every Submit under a mutex and cannot
// reproduce Postgres commit interleaving, so this guard must be pgtest-backed.
func TestPostgres_Submit_ConcurrentQuorum(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := approval.NewPostgresRepository(pool)
	_, stageID := seedRunAndStage(t, pool)

	const n = 4
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		observed []int
		firstErr error
	)
	// A released start barrier maximizes the interleaving between the
	// concurrent Submit commits and the read-after-write counts.
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		subject := fmt.Sprintf("@approver-%d", i)
		wg.Add(1)
		go func(subject string) {
			defer wg.Done()
			<-start
			res, err := repo.Submit(context.Background(), approval.SubmitParams{
				StageID:         stageID,
				ApproverSubject: subject,
				Decision:        approval.DecisionApprove,
			})
			if err != nil || res == nil || !res.Inserted {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("Submit(%s): inserted=%v err=%v", subject, res != nil && res.Inserted, err)
				}
				mu.Unlock()
				return
			}
			// Read-after-write: this approver's own committed row plus every
			// row committed before this statement's snapshot started.
			after, err := repo.ListForStage(context.Background(), stageID)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("ListForStage(%s): %v", subject, err)
				}
				mu.Unlock()
				return
			}
			mu.Lock()
			observed = append(observed, len(after))
			mu.Unlock()
		}(subject)
	}
	close(start)
	wg.Wait()

	if firstErr != nil {
		t.Fatalf("concurrent submit/list: %v", firstErr)
	}
	// Every distinct approver durably persisted exactly one row.
	final, err := repo.ListForStage(context.Background(), stageID)
	if err != nil {
		t.Fatalf("final ListForStage: %v", err)
	}
	if len(final) != n {
		t.Errorf("final row count = %d, want %d (all distinct approvers persisted)", len(final), n)
	}
	// The last committer observes the full quorum: at least one goroutine's
	// post-Submit count saw all N. If the stall existed, every count would top
	// out below N.
	maxObserved := 0
	for _, c := range observed {
		if c > maxObserved {
			maxObserved = c
		}
	}
	if maxObserved < n {
		t.Errorf("max post-Submit observed count = %d, want %d (last committer must see full quorum — no stall)", maxObserved, n)
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
