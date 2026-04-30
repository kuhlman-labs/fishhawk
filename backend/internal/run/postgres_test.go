package run_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// startPostgres spins up a throwaway Postgres container, applies the
// embedded migrations, and returns a pgxpool.Pool. Skips the test if
// Docker isn't reachable (so devs without Docker still pass `go test`
// for the rest of the suite).
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

// isDockerUnavailable returns true when the testcontainers error is
// a missing docker daemon. Lets local dev runs without Docker skip
// rather than fail.
func isDockerUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range []string{
		"Cannot connect to the Docker daemon",
		"docker: not found",
		"executable file not found",
		"dial unix /var/run/docker.sock",
	} {
		if containsIgnoreCase(msg, marker) {
			return true
		}
	}
	// Honor an explicit opt-out for CI configs that don't have Docker.
	return os.Getenv("FISHHAWK_SKIP_INTEGRATION") != ""
}

func containsIgnoreCase(haystack, needle string) bool {
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			h := haystack[i+j]
			n := needle[j]
			if h >= 'A' && h <= 'Z' {
				h += 'a' - 'A'
			}
			if n >= 'A' && n <= 'Z' {
				n += 'a' - 'A'
			}
			if h != n {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// makeRun is a small helper that creates a run with sensible defaults.
func makeRun(t *testing.T, repo run.Repository) *run.Run {
	t.Helper()
	r, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return r
}

func makeStage(t *testing.T, repo run.Repository, runID uuid.UUID, seq int) *run.Stage {
	t.Helper()
	s, err := repo.CreateStage(context.Background(), run.CreateStageParams{
		RunID:        runID,
		Sequence:     seq,
		Type:         run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent,
		ExecutorRef:  "claude-code",
	})
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}
	return s
}

func TestPostgres_CreateAndGetRun(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)

	created := makeRun(t, repo)
	if created.State != run.StatePending {
		t.Errorf("initial state = %q, want pending", created.State)
	}

	got, err := repo.GetRun(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("id mismatch")
	}
	if got.Repo != "kuhlman-labs/fishhawk" {
		t.Errorf("repo = %q", got.Repo)
	}
}

func TestPostgres_GetRun_NotFound(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)

	_, err := repo.GetRun(context.Background(), uuid.New())
	if !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestPostgres_TransitionRun_HappyPath(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)

	r := makeRun(t, repo)

	moved, err := repo.TransitionRun(context.Background(), r.ID, run.StateRunning)
	if err != nil {
		t.Fatalf("transition pending → running: %v", err)
	}
	if moved.State != run.StateRunning {
		t.Errorf("state = %q, want running", moved.State)
	}

	moved, err = repo.TransitionRun(context.Background(), r.ID, run.StateSucceeded)
	if err != nil {
		t.Fatalf("transition running → succeeded: %v", err)
	}
	if moved.State != run.StateSucceeded {
		t.Errorf("state = %q, want succeeded", moved.State)
	}
}

func TestPostgres_TransitionRun_Idempotent(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)

	r := makeRun(t, repo)
	first, err := repo.TransitionRun(context.Background(), r.ID, run.StateRunning)
	if err != nil {
		t.Fatalf("first transition: %v", err)
	}
	second, err := repo.TransitionRun(context.Background(), r.ID, run.StateRunning)
	if err != nil {
		t.Fatalf("idempotent re-apply: %v", err)
	}
	if first.UpdatedAt.UnixNano() != second.UpdatedAt.UnixNano() {
		t.Errorf("idempotent re-apply mutated row (updated_at changed)")
	}
}

func TestPostgres_TransitionRun_InvalidRejected(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)

	r := makeRun(t, repo)
	_, err := repo.TransitionRun(context.Background(), r.ID, run.StateSucceeded)
	var ite run.InvalidTransitionError
	if !errors.As(err, &ite) {
		t.Fatalf("err = %v, want InvalidTransitionError", err)
	}
	if ite.Kind != "run" || ite.From != "pending" || ite.To != "succeeded" {
		t.Errorf("ite = %+v", ite)
	}
}

func TestPostgres_TransitionRun_AfterTerminal(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)

	r := makeRun(t, repo)
	if _, err := repo.TransitionRun(context.Background(), r.ID, run.StateRunning); err != nil {
		t.Fatalf("→running: %v", err)
	}
	if _, err := repo.TransitionRun(context.Background(), r.ID, run.StateSucceeded); err != nil {
		t.Fatalf("→succeeded: %v", err)
	}
	_, err := repo.TransitionRun(context.Background(), r.ID, run.StateRunning)
	var ite run.InvalidTransitionError
	if !errors.As(err, &ite) {
		t.Fatalf("err = %v, want InvalidTransitionError when leaving terminal", err)
	}
}

func TestPostgres_ConcurrentTransition_ExactlyOneWins(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)

	r := makeRun(t, repo)

	const goroutines = 10
	results := make([]error, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, err := repo.TransitionRun(context.Background(), r.ID, run.StateCancelled)
			results[i] = err
		}()
	}
	wg.Wait()

	// All goroutines target the same target state. With FOR UPDATE
	// + idempotent same-state re-apply, every goroutine should
	// either be the first to succeed or observe the prior cancelled
	// state and no-op. None should fail with InvalidTransitionError.
	for i, err := range results {
		if err != nil {
			t.Errorf("goroutine %d failed: %v", i, err)
		}
	}

	final, err := repo.GetRun(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("get final run: %v", err)
	}
	if final.State != run.StateCancelled {
		t.Errorf("final state = %q, want cancelled", final.State)
	}
}

func TestPostgres_ConcurrentDifferentTargets_ExactlyOneWins(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)

	r := makeRun(t, repo)
	if _, err := repo.TransitionRun(context.Background(), r.ID, run.StateRunning); err != nil {
		t.Fatalf("→running: %v", err)
	}

	// Race two concurrent transitions: one wants succeeded, the
	// other wants failed. Exactly one should succeed; the other
	// must see the now-terminal state and fail with
	// InvalidTransitionError (terminal → terminal is forbidden).
	type result struct {
		state run.State
		err   error
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, err := repo.TransitionRun(context.Background(), r.ID, run.StateSucceeded)
		results[0] = result{run.StateSucceeded, err}
	}()
	go func() {
		defer wg.Done()
		_, err := repo.TransitionRun(context.Background(), r.ID, run.StateFailed)
		results[1] = result{run.StateFailed, err}
	}()
	wg.Wait()

	winners, losers := 0, 0
	for _, r := range results {
		if r.err == nil {
			winners++
			continue
		}
		var ite run.InvalidTransitionError
		if !errors.As(r.err, &ite) {
			t.Errorf("non-winner err = %v, want InvalidTransitionError", r.err)
			continue
		}
		losers++
	}
	if winners != 1 || losers != 1 {
		t.Errorf("winners=%d losers=%d, want 1 of each", winners, losers)
	}
}

func TestPostgres_StageLifecycle(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)

	r := makeRun(t, repo)
	s := makeStage(t, repo, r.ID, 0)

	if s.State != run.StageStatePending {
		t.Errorf("initial state = %q, want pending", s.State)
	}
	if s.StartedAt != nil {
		t.Errorf("StartedAt should be nil before running")
	}

	dispatched, err := repo.TransitionStage(context.Background(), s.ID, run.StageStateDispatched, nil)
	if err != nil {
		t.Fatalf("→dispatched: %v", err)
	}
	if dispatched.StartedAt != nil {
		t.Errorf("StartedAt should still be nil after dispatched")
	}

	running, err := repo.TransitionStage(context.Background(), s.ID, run.StageStateRunning, nil)
	if err != nil {
		t.Fatalf("→running: %v", err)
	}
	if running.StartedAt == nil {
		t.Errorf("StartedAt should be stamped on first entry to running")
	}

	succeeded, err := repo.TransitionStage(context.Background(), s.ID, run.StageStateSucceeded, nil)
	if err != nil {
		t.Fatalf("→succeeded: %v", err)
	}
	if succeeded.EndedAt == nil {
		t.Errorf("EndedAt should be stamped on terminal transition")
	}
}

func TestPostgres_StageFailureRequiresCompletion(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)

	r := makeRun(t, repo)
	s := makeStage(t, repo, r.ID, 0)
	if _, err := repo.TransitionStage(context.Background(), s.ID, run.StageStateDispatched, nil); err != nil {
		t.Fatalf("→dispatched: %v", err)
	}
	if _, err := repo.TransitionStage(context.Background(), s.ID, run.StageStateRunning, nil); err != nil {
		t.Fatalf("→running: %v", err)
	}

	// Missing completion → adapter rejects.
	_, err := repo.TransitionStage(context.Background(), s.ID, run.StageStateFailed, nil)
	if err == nil {
		t.Fatal("expected error when failing without StageCompletion")
	}

	// With completion → succeeds and persists category + reason.
	cat := run.FailureA
	reason := "agent invoked /quit unexpectedly"
	failed, err := repo.TransitionStage(context.Background(), s.ID, run.StageStateFailed, &run.StageCompletion{
		FailureCategory: &cat,
		FailureReason:   &reason,
	})
	if err != nil {
		t.Fatalf("transition with completion: %v", err)
	}
	if failed.FailureCategory == nil || *failed.FailureCategory != run.FailureA {
		t.Errorf("FailureCategory = %v, want A", failed.FailureCategory)
	}
	if failed.FailureReason == nil || *failed.FailureReason != reason {
		t.Errorf("FailureReason = %v, want %q", failed.FailureReason, reason)
	}
}

func TestPostgres_ListStagesForRun_OrderedBySequence(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)

	r := makeRun(t, repo)
	// Insert out of order; expect listing returns sorted by sequence.
	makeStage(t, repo, r.ID, 2)
	makeStage(t, repo, r.ID, 0)
	makeStage(t, repo, r.ID, 1)

	stages, err := repo.ListStagesForRun(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stages) != 3 {
		t.Fatalf("len = %d, want 3", len(stages))
	}
	for i, s := range stages {
		if s.Sequence != i {
			t.Errorf("stages[%d].Sequence = %d, want %d", i, s.Sequence, i)
		}
	}
}
