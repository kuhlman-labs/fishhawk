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

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
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
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("5432/tcp"),
			),
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

// TestPostgres_Drive_RoundTrip exercises migration 0031: a run created
// with Drive=true reads back true, and the default path (params zero
// value) reads back false — the legacy-row semantics.
func TestPostgres_Drive_RoundTrip(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)

	driven, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
		Drive:         true,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if !driven.Drive {
		t.Errorf("created Drive = false, want true")
	}
	got, err := repo.GetRun(context.Background(), driven.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if !got.Drive {
		t.Errorf("read-back Drive = false, want true")
	}

	plain := makeRun(t, repo)
	if plain.Drive {
		t.Errorf("default Drive = true, want false")
	}
}

// TestPostgres_SliceIndex_RoundTrip exercises the nullable runs.slice_index
// column (E24.1 / #1141): a decomposed child persists and reads back its
// 0-based sub_plan position, while a run created without a SliceIndex
// round-trips as nil (the non-decomposed default).
func TestPostgres_SliceIndex_RoundTrip(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)

	idx := 2
	child, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
		SliceIndex:    &idx,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if child.SliceIndex == nil || *child.SliceIndex != 2 {
		t.Errorf("created SliceIndex = %v, want 2", child.SliceIndex)
	}
	got, err := repo.GetRun(context.Background(), child.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.SliceIndex == nil || *got.SliceIndex != 2 {
		t.Errorf("read-back SliceIndex = %v, want 2", got.SliceIndex)
	}

	// Slice 0 must round-trip as a non-nil pointer to 0, distinct from the
	// nil non-decomposed default — the runner reads slice_index only when
	// decomposed_from_run_id is set, so a child's 0 must survive persistence.
	zero := 0
	sliceZero, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
		SliceIndex:    &zero,
	})
	if err != nil {
		t.Fatalf("create slice-0 run: %v", err)
	}
	if sliceZero.SliceIndex == nil || *sliceZero.SliceIndex != 0 {
		t.Errorf("slice-0 SliceIndex = %v, want non-nil 0", sliceZero.SliceIndex)
	}

	// A run created without a SliceIndex (non-decomposed) reads back nil.
	plain := makeRun(t, repo)
	if plain.SliceIndex != nil {
		t.Errorf("default SliceIndex = %v, want nil", plain.SliceIndex)
	}
	gotPlain, err := repo.GetRun(context.Background(), plain.ID)
	if err != nil {
		t.Fatalf("get plain run: %v", err)
	}
	if gotPlain.SliceIndex != nil {
		t.Errorf("read-back default SliceIndex = %v, want nil", gotPlain.SliceIndex)
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

func TestPostgres_GetRunByIdempotencyKey_HappyPath(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)

	key := "abc123"
	created, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo:           "x/y",
		WorkflowID:     "feature_change",
		WorkflowSHA:    "deadbeef",
		TriggerSource:  run.TriggerCLI,
		IdempotencyKey: &key,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	got, err := repo.GetRunByIdempotencyKey(context.Background(), "x/y", key)
	if err != nil {
		t.Fatalf("GetRunByIdempotencyKey: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("got %s, want %s", got.ID, created.ID)
	}
	if got.IdempotencyKey == nil || *got.IdempotencyKey != key {
		t.Errorf("IdempotencyKey round-trip failed: %v", got.IdempotencyKey)
	}
}

func TestPostgres_GetRunByIdempotencyKey_NotFound(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)
	_, err := repo.GetRunByIdempotencyKey(context.Background(), "x/y", "nope")
	if !errors.Is(err, run.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestPostgres_DuplicateIdempotencyKey_ConflictsAtDB(t *testing.T) {
	// The unique partial index covers (repo, idempotency_key)
	// where idempotency_key IS NOT NULL. The handler checks for
	// the existing row before insert; this test pins the DB-level
	// guarantee that a race between two callers can't both
	// insert.
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)

	key := "shared"
	if _, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo:           "x/y",
		WorkflowID:     "w",
		WorkflowSHA:    "s",
		TriggerSource:  run.TriggerCLI,
		IdempotencyKey: &key,
	}); err != nil {
		t.Fatalf("first CreateRun: %v", err)
	}
	_, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo:           "x/y",
		WorkflowID:     "w",
		WorkflowSHA:    "s",
		TriggerSource:  run.TriggerCLI,
		IdempotencyKey: &key,
	})
	if err == nil {
		t.Fatal("expected duplicate-key error from DB")
	}
}

func TestPostgres_NullIdempotencyKey_DoesNotCollide(t *testing.T) {
	// Two runs with no idempotency_key (nil) should both succeed —
	// the partial index excludes NULLs so they don't conflict.
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)
	for i := 0; i < 2; i++ {
		if _, err := repo.CreateRun(context.Background(), run.CreateRunParams{
			Repo:          "x/y",
			WorkflowID:    "w",
			WorkflowSHA:   "s",
			TriggerSource: run.TriggerCLI,
		}); err != nil {
			t.Fatalf("CreateRun #%d: %v", i, err)
		}
	}
}

func TestPostgres_ListRuns(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)

	// Three runs across two repos and two states.
	r1, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "sha1",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "x/y", WorkflowID: "hotfix", WorkflowSHA: "sha2",
		TriggerSource: run.TriggerCLI,
	}); err != nil {
		t.Fatal(err)
	}
	r3, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "a/b", WorkflowID: "feature_change", WorkflowSHA: "sha3",
		TriggerSource: run.TriggerUI,
	})
	if err != nil {
		t.Fatal(err)
	}

	// No filter: returns all 3.
	all, err := repo.ListRuns(context.Background(), run.ListRunsFilter{Limit: 10})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("got %d, want 3", len(all))
	}

	// Repo filter.
	xy, err := repo.ListRuns(context.Background(), run.ListRunsFilter{Repo: "x/y", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(xy) != 2 {
		t.Errorf("got %d for repo x/y, want 2", len(xy))
	}

	// Workflow filter combined with repo filter.
	xyHotfix, err := repo.ListRuns(context.Background(),
		run.ListRunsFilter{Repo: "x/y", WorkflowID: "hotfix", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(xyHotfix) != 1 {
		t.Errorf("got %d for x/y+hotfix, want 1", len(xyHotfix))
	}

	// State filter (transition r1 → running first).
	if _, err := repo.TransitionRun(context.Background(), r1.ID, run.StateRunning); err != nil {
		t.Fatal(err)
	}
	running, err := repo.ListRuns(context.Background(),
		run.ListRunsFilter{State: string(run.StateRunning), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 1 || running[0].ID != r1.ID {
		t.Errorf("running filter broken: %+v", running)
	}

	// Limit + offset pagination — page 1 of 2 with limit=2.
	page1, err := repo.ListRuns(context.Background(), run.ListRunsFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 {
		t.Errorf("page1 = %d, want 2", len(page1))
	}
	page2, err := repo.ListRuns(context.Background(), run.ListRunsFilter{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 {
		t.Errorf("page2 = %d, want 1", len(page2))
	}

	// Bad limit / offset surface as errors.
	if _, err := repo.ListRuns(context.Background(), run.ListRunsFilter{Limit: 0}); err == nil {
		t.Error("expected error on limit=0")
	}
	if _, err := repo.ListRuns(context.Background(), run.ListRunsFilter{Limit: 1, Offset: -1}); err == nil {
		t.Error("expected error on negative offset")
	}

	_ = r3 // silence "declared and not used" if filters change.
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

// TestPostgres_RetryRun_ReopensFailed exercises the run-level reopen
// override (#698): a failed run goes back to running, and every other
// column is left intact (RetryRun reuses UpdateRunState; runs carry no
// failure metadata to clear).
func TestPostgres_RetryRun_ReopensFailed(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)

	r := makeRun(t, repo)
	if _, err := repo.TransitionRun(context.Background(), r.ID, run.StateRunning); err != nil {
		t.Fatalf("pending → running: %v", err)
	}
	failed, err := repo.TransitionRun(context.Background(), r.ID, run.StateFailed)
	if err != nil {
		t.Fatalf("running → failed: %v", err)
	}
	if failed.State != run.StateFailed {
		t.Fatalf("state = %q, want failed", failed.State)
	}

	reopened, err := repo.RetryRun(context.Background(), r.ID, run.StateRunning)
	if err != nil {
		t.Fatalf("RetryRun failed → running: %v", err)
	}
	if reopened.State != run.StateRunning {
		t.Errorf("state = %q, want running", reopened.State)
	}
	// Non-state columns untouched by the reopen.
	if reopened.Repo != r.Repo || reopened.WorkflowID != r.WorkflowID || reopened.WorkflowSHA != r.WorkflowSHA {
		t.Errorf("RetryRun clobbered a non-state column: %+v", reopened)
	}
	if reopened.MaxRetriesSnapshot != failed.MaxRetriesSnapshot {
		t.Errorf("max_retries_snapshot = %d, want %d", reopened.MaxRetriesSnapshot, failed.MaxRetriesSnapshot)
	}
}

// TestPostgres_RetryRun_RejectsNonFailed asserts the narrow retry
// table: only failed → running is permitted. A running run cannot be
// "reopened".
func TestPostgres_RetryRun_RejectsNonFailed(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)

	r := makeRun(t, repo) // pending
	_, err := repo.RetryRun(context.Background(), r.ID, run.StateRunning)
	var inv run.InvalidTransitionError
	if !errors.As(err, &inv) {
		t.Fatalf("err = %v, want InvalidTransitionError", err)
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

func TestPostgres_StageGate_RoundTripPreservesShape(t *testing.T) {
	// Per #213: the dispatcher writes the workflow-spec gate shape
	// onto the stages row so the review-stage UI can render it
	// without re-parsing the spec. Round-trip a populated Gate
	// (kind + blocking_checks + approvers) through CreateStage +
	// GetStage + ListStagesForRun and assert nothing's dropped.
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)
	r := makeRun(t, repo)

	s, err := repo.CreateStage(context.Background(), run.CreateStageParams{
		RunID:            r.ID,
		Sequence:         0,
		Type:             run.StageTypeReview,
		ExecutorKind:     run.ExecutorHuman,
		ExecutorRef:      "human",
		RequiresApproval: true,
		Gate: &run.Gate{
			Kind: run.GateKindApproval,
			Approvers: &run.GateApprovers{
				AnyOf: []string{"founder", "tech-lead"},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateStage: %v", err)
	}
	if s.Gate == nil {
		t.Fatal("CreateStage returned Gate=nil; want round-tripped shape")
	}
	if s.Gate.Kind != run.GateKindApproval {
		t.Errorf("Gate.Kind = %q, want approval", s.Gate.Kind)
	}
	if s.Gate.Approvers == nil || len(s.Gate.Approvers.AnyOf) != 2 {
		t.Fatalf("Gate.Approvers = %+v, want any_of with 2 entries", s.Gate.Approvers)
	}

	got, err := repo.GetStage(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.Gate == nil || got.Gate.Kind != run.GateKindApproval {
		t.Errorf("GetStage Gate = %+v", got.Gate)
	}

	stages, err := repo.ListStagesForRun(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("ListStagesForRun: %v", err)
	}
	if len(stages) != 1 || stages[0].Gate == nil {
		t.Fatalf("ListStagesForRun returned %d stages; first.Gate=%+v",
			len(stages), stages[0].Gate)
	}
}

func TestPostgres_ListReviewStagesAwaitingApproval_IncludesSLALess(t *testing.T) {
	// #725 regression at the persistence→repository seam: the merge
	// reconciler's listing must return review stages parked in
	// awaiting_approval REGARDLESS of gate_sla. The feature_change review
	// gate carries no sla, so the SLA ticker's gate_sla-filtered query
	// (ListStagesAwaitingApproval) hides it — which is exactly why the
	// reconciler needs its own SLA-independent query.
	//
	// The Go fake ignores gate_sla entirely, so only this real-SQL test
	// proves the WHERE clause is right (cf. #618: per-layer units pass
	// while the seam breaks).
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)
	ctx := context.Background()

	// park transitions one review stage through the lifecycle to
	// awaiting_approval and returns its id.
	park := func(gateSLA *string) uuid.UUID {
		r := makeRun(t, repo)
		s, err := repo.CreateStage(ctx, run.CreateStageParams{
			RunID:            r.ID,
			Sequence:         0,
			Type:             run.StageTypeReview,
			ExecutorKind:     run.ExecutorHuman,
			ExecutorRef:      "human",
			RequiresApproval: true,
			GateSLA:          gateSLA,
		})
		if err != nil {
			t.Fatalf("CreateStage: %v", err)
		}
		for _, to := range []run.StageState{run.StageStateDispatched, run.StageStateRunning, run.StageStateAwaitingApproval} {
			if _, err := repo.TransitionStage(ctx, s.ID, to, nil); err != nil {
				t.Fatalf("→%s: %v", to, err)
			}
		}
		return s.ID
	}

	sla := "4_business_hours"
	slaLessID := park(nil)     // the bug-defining case
	slaBearingID := park(&sla) // the SLA ticker's case

	// New query: BOTH review stages, SLA-independent.
	reviewStages, err := repo.ListReviewStagesAwaitingApproval(ctx)
	if err != nil {
		t.Fatalf("ListReviewStagesAwaitingApproval: %v", err)
	}
	got := map[uuid.UUID]bool{}
	for _, s := range reviewStages {
		got[s.ID] = true
	}
	if !got[slaLessID] {
		t.Errorf("ListReviewStagesAwaitingApproval dropped the SLA-less review stage %s (the #725 bug)", slaLessID)
	}
	if !got[slaBearingID] {
		t.Errorf("ListReviewStagesAwaitingApproval dropped the SLA-bearing review stage %s", slaBearingID)
	}

	// Control: the unchanged SLA ticker query still excludes the SLA-less
	// stage and includes only the SLA-bearing one.
	slaStages, err := repo.ListStagesAwaitingApproval(ctx)
	if err != nil {
		t.Fatalf("ListStagesAwaitingApproval: %v", err)
	}
	slaGot := map[uuid.UUID]bool{}
	for _, s := range slaStages {
		slaGot[s.ID] = true
	}
	if slaGot[slaLessID] {
		t.Errorf("ListStagesAwaitingApproval returned the SLA-less stage %s; its gate_sla filter should exclude it", slaLessID)
	}
	if !slaGot[slaBearingID] {
		t.Errorf("ListStagesAwaitingApproval dropped the SLA-bearing stage %s", slaBearingID)
	}
}

func TestPostgres_StageGate_NilWhenSpecHasNoGate(t *testing.T) {
	// The dispatcher passes Gate=nil for gateless stages (e.g.
	// implement). The persisted row's Gate must come back nil too,
	// otherwise the UI would mis-render an "approval coming" panel
	// for a stage that's never going to need one.
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)
	r := makeRun(t, repo)

	s, err := repo.CreateStage(context.Background(), run.CreateStageParams{
		RunID:        r.ID,
		Sequence:     0,
		Type:         run.StageTypeImplement,
		ExecutorKind: run.ExecutorAgent,
		ExecutorRef:  "claude-code",
		// Gate omitted.
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Gate != nil {
		t.Errorf("CreateStage Gate = %+v, want nil", s.Gate)
	}
	got, err := repo.GetStage(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Gate != nil {
		t.Errorf("GetStage Gate = %+v, want nil", got.Gate)
	}
}

func TestPostgres_TransitionStage_SameState_NoOp(t *testing.T) {
	// Pins the same-state no-op contract documented on
	// Repository.TransitionStage: when the stage is already in the
	// target state, the call must return the unchanged stage (nil
	// error) without bumping updated_at.
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)

	r := makeRun(t, repo)
	s := makeStage(t, repo, r.ID, 0)

	// Walk to succeeded via the normal lifecycle.
	if _, err := repo.TransitionStage(context.Background(), s.ID, run.StageStateDispatched, nil); err != nil {
		t.Fatalf("→dispatched: %v", err)
	}
	if _, err := repo.TransitionStage(context.Background(), s.ID, run.StageStateRunning, nil); err != nil {
		t.Fatalf("→running: %v", err)
	}
	first, err := repo.TransitionStage(context.Background(), s.ID, run.StageStateSucceeded, nil)
	if err != nil {
		t.Fatalf("→succeeded: %v", err)
	}

	// Call again with the same target state.
	second, err := repo.TransitionStage(context.Background(), s.ID, run.StageStateSucceeded, nil)
	if err != nil {
		t.Fatalf("same-state re-apply returned error: %v", err)
	}
	if second.State != run.StageStateSucceeded {
		t.Errorf("state = %q, want succeeded", second.State)
	}
	if first.UpdatedAt.UnixNano() != second.UpdatedAt.UnixNano() {
		t.Errorf("same-state re-apply mutated row (updated_at changed)")
	}
}

func TestPostgres_StageGate_CheckGateHasNoApprovers(t *testing.T) {
	// routine_change.workflows.yaml's review stage uses a check-only
	// gate (no approvers; just blocking_checks). The persisted Gate
	// must reflect Kind=check and Approvers=nil so the UI can
	// suppress the approval panel.
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)
	r := makeRun(t, repo)

	s, err := repo.CreateStage(context.Background(), run.CreateStageParams{
		RunID:        r.ID,
		Sequence:     0,
		Type:         run.StageTypeReview,
		ExecutorKind: run.ExecutorAgent,
		ExecutorRef:  "claude-code",
		Gate: &run.Gate{
			Kind: run.GateKindCheck,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Gate == nil || s.Gate.Kind != run.GateKindCheck {
		t.Fatalf("Gate = %+v", s.Gate)
	}
	if s.Gate.Approvers != nil {
		t.Errorf("Approvers = %+v, want nil for check gate", s.Gate.Approvers)
	}
}

// costRepo is the optional cost-rollup surface the trace handler
// consumes via capability assertion (#649 / #688). AddRunCost and
// SumWorkflowCostInRange are not part of run.Repository, so the test
// asserts for them the same way the server does.
type costRepo interface {
	AddRunCost(ctx context.Context, id uuid.UUID, deltaUSD float64, resolvedModel string) (*run.Run, error)
	SumWorkflowCostInRange(ctx context.Context, repo, workflowID string, from, to time.Time) (float64, error)
}

func TestPostgres_SumWorkflowCostInRange(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)
	cr, ok := repo.(costRepo)
	if !ok {
		t.Fatal("postgres repo does not implement AddRunCost/SumWorkflowCostInRange")
	}
	ctx := context.Background()

	// Two feature_change runs in the same repo + one routine_change run,
	// plus a feature_change run in a different repo. The window sum must
	// filter on (repo, workflow_id) and the [from, to) range.
	r1 := makeRun(t, repo) // feature_change @ kuhlman-labs/fishhawk
	r2 := makeRun(t, repo) // feature_change @ kuhlman-labs/fishhawk
	r3, err := repo.CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "routine_change",
		WorkflowSHA: "deadbeef", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create routine_change run: %v", err)
	}
	rOther, err := repo.CreateRun(ctx, run.CreateRunParams{
		Repo: "other/repo", WorkflowID: "feature_change",
		WorkflowSHA: "deadbeef", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create other-repo run: %v", err)
	}

	for _, c := range []struct {
		id  uuid.UUID
		usd float64
	}{
		{r1.ID, 10}, {r2.ID, 5}, {r3.ID, 100}, {rOther.ID, 7},
	} {
		if _, err := cr.AddRunCost(ctx, c.id, c.usd, "claude"); err != nil {
			t.Fatalf("add run cost: %v", err)
		}
	}

	now := time.Now()
	from := now.Add(-time.Hour)
	to := now.Add(time.Hour)

	// feature_change @ fishhawk over the current window: 10 + 5.
	got, err := cr.SumWorkflowCostInRange(ctx, "kuhlman-labs/fishhawk", "feature_change", from, to)
	if err != nil {
		t.Fatalf("sum feature_change: %v", err)
	}
	if got != 15 {
		t.Errorf("feature_change sum = %v, want 15", got)
	}

	// routine_change is summed separately (workflow_id filter).
	got, err = cr.SumWorkflowCostInRange(ctx, "kuhlman-labs/fishhawk", "routine_change", from, to)
	if err != nil {
		t.Fatalf("sum routine_change: %v", err)
	}
	if got != 100 {
		t.Errorf("routine_change sum = %v, want 100", got)
	}

	// A different repo's runs never leak into the sum (repo filter).
	got, err = cr.SumWorkflowCostInRange(ctx, "kuhlman-labs/fishhawk", "feature_change",
		now.Add(2*time.Hour), now.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("sum future window: %v", err)
	}
	if got != 0 {
		t.Errorf("future-window sum = %v, want 0 (range excludes all runs)", got)
	}

	// An empty match returns 0, never an error (COALESCE).
	got, err = cr.SumWorkflowCostInRange(ctx, "kuhlman-labs/fishhawk", "no_such_workflow", from, to)
	if err != nil {
		t.Fatalf("sum unknown workflow: %v", err)
	}
	if got != 0 {
		t.Errorf("unknown-workflow sum = %v, want 0", got)
	}
}

// resumeAppender is the optional combined-commit capability the
// clarification handler type-asserts on the concrete postgres repo
// (#1090). It is not part of run.Repository, so the test asserts it the
// same way the handler does.
type resumeAppender interface {
	ResumeAwaitingInputAndAppend(ctx context.Context, stageID uuid.UUID, p audit.ChainAppendParams) (*run.Stage, bool, error)
}

// seedAwaitingInputStage creates a run + plan stage and walks it
// pending → running → awaiting_input, the state a clarification_request
// parks a plan stage at.
func seedAwaitingInputStage(t *testing.T, repo run.Repository) (*run.Run, *run.Stage) {
	t.Helper()
	ctx := context.Background()
	r := makeRun(t, repo)
	s := makeStage(t, repo, r.ID, 0)
	for _, to := range []run.StageState{run.StageStateDispatched, run.StageStateRunning} {
		if _, err := repo.TransitionStage(ctx, s.ID, to, nil); err != nil {
			t.Fatalf("transition to %s: %v", to, err)
		}
	}
	parked, err := repo.TransitionStage(ctx, s.ID, run.StageStateAwaitingInput, nil)
	if err != nil {
		t.Fatalf("transition to awaiting_input: %v", err)
	}
	return r, parked
}

// clarificationParams builds a clarification_answered ChainAppendParams
// for the run/stage with the given JSON payload bytes.
func clarificationParams(runID, stageID uuid.UUID, payload []byte) audit.ChainAppendParams {
	sid := stageID
	subject := "operator@example.com"
	kind := audit.ActorKind("user")
	return audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &sid,
		Timestamp:    time.Now().UTC(),
		Category:     "clarification_answered",
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	}
}

func TestPostgres_ResumeAwaitingInputAndAppend_HappyPath(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)
	ra, ok := repo.(resumeAppender)
	if !ok {
		t.Fatal("postgres repo does not implement ResumeAwaitingInputAndAppend")
	}
	auditRepo := audit.NewPostgresRepository(pool)
	ctx := context.Background()

	r, parked := seedAwaitingInputStage(t, repo)
	payload := []byte(`{"conditions":"Q1: Postgres"}`)

	stage, won, err := ra.ResumeAwaitingInputAndAppend(ctx, parked.ID, clarificationParams(r.ID, parked.ID, payload))
	if err != nil {
		t.Fatalf("ResumeAwaitingInputAndAppend: %v", err)
	}
	if !won {
		t.Fatal("won = false, want true for the single resuming caller")
	}
	if stage.State != run.StageStatePending {
		t.Errorf("stage state = %q, want pending", stage.State)
	}

	// The stage really committed at pending.
	got, err := repo.GetStage(ctx, parked.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != run.StageStatePending {
		t.Errorf("persisted stage state = %q, want pending", got.State)
	}

	// Exactly one clarification_answered entry persisted in the same tx.
	entries, err := auditRepo.ListForRunByCategory(ctx, r.ID, "clarification_answered")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("clarification_answered entries = %d, want 1", len(entries))
	}
}

func TestPostgres_ResumeAwaitingInputAndAppend_AppendFailureRollsBack(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)
	ra, ok := repo.(resumeAppender)
	if !ok {
		t.Fatal("postgres repo does not implement ResumeAwaitingInputAndAppend")
	}
	auditRepo := audit.NewPostgresRepository(pool)
	ctx := context.Background()

	r, parked := seedAwaitingInputStage(t, repo)

	// PostgreSQL jsonb rejects the \u0000 unicode escape in string values,
	// so the audit INSERT fails AFTER the stage CAS — the whole transaction
	// must roll back, leaving the stage at awaiting_input.
	payload := []byte(`{"conditions":"\u0000"}`)
	stage, won, err := ra.ResumeAwaitingInputAndAppend(ctx, parked.ID, clarificationParams(r.ID, parked.ID, payload))
	if err == nil {
		t.Fatal("expected an error from the jsonb \\u0000 append, got nil")
	}
	if won {
		t.Error("won = true on a rolled-back transaction; want false")
	}
	if stage != nil {
		t.Errorf("stage = %+v on failure, want nil", stage)
	}

	// The stage stayed re-answerable at awaiting_input (rollback worked).
	got, err := repo.GetStage(ctx, parked.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != run.StageStateAwaitingInput {
		t.Errorf("stage state = %q after rollback, want awaiting_input", got.State)
	}

	// No orphaned audit entry.
	entries, err := auditRepo.ListForRunByCategory(ctx, r.ID, "clarification_answered")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("clarification_answered entries = %d after rollback, want 0", len(entries))
	}
}

func TestPostgres_ResumeAwaitingInputAndAppend_LoserCAS(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)
	ra, ok := repo.(resumeAppender)
	if !ok {
		t.Fatal("postgres repo does not implement ResumeAwaitingInputAndAppend")
	}
	auditRepo := audit.NewPostgresRepository(pool)
	ctx := context.Background()

	r, parked := seedAwaitingInputStage(t, repo)

	// First caller wins and moves the stage to pending.
	if _, won, err := ra.ResumeAwaitingInputAndAppend(ctx, parked.ID, clarificationParams(r.ID, parked.ID, []byte(`{"conditions":"first"}`))); err != nil || !won {
		t.Fatalf("first call: won=%v err=%v, want won=true err=nil", won, err)
	}

	// Second caller observes the stage already pending: won=false, no new
	// audit entry, no error.
	stage, won, err := ra.ResumeAwaitingInputAndAppend(ctx, parked.ID, clarificationParams(r.ID, parked.ID, []byte(`{"conditions":"second"}`)))
	if err != nil {
		t.Fatalf("second call err = %v, want nil", err)
	}
	if won {
		t.Error("second call won = true, want false (loser of the CAS)")
	}
	if stage == nil || stage.State != run.StageStatePending {
		t.Errorf("loser stage = %+v, want pending row", stage)
	}

	entries, err := auditRepo.ListForRunByCategory(ctx, r.ID, "clarification_answered")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("clarification_answered entries = %d, want 1 (loser must not append)", len(entries))
	}
}

func TestPostgres_ResumeAwaitingInputAndAppend_MissingStage(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)
	ra, ok := repo.(resumeAppender)
	if !ok {
		t.Fatal("postgres repo does not implement ResumeAwaitingInputAndAppend")
	}
	ctx := context.Background()

	_, won, err := ra.ResumeAwaitingInputAndAppend(ctx, uuid.New(), clarificationParams(uuid.New(), uuid.New(), []byte(`{}`)))
	if !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("err = %v, want run.ErrNotFound", err)
	}
	if won {
		t.Error("won = true for a missing stage, want false")
	}
}

// scopeCompletenessParker is the optional combined-commit capability the
// pull-request park handler type-asserts on the concrete postgres repo
// (#1231). Like resumeAppender it is not part of run.Repository, so the
// test asserts it the same way the handler does.
type scopeCompletenessParker interface {
	ParkScopeCompletenessAndAppend(ctx context.Context, stageID uuid.UUID, park run.ScopeCompletenessPark, p audit.ChainAppendParams) (*run.Stage, bool, error)
}

// seedRunningImplementStage creates a run + implement stage and walks it
// pending → dispatched → running, the state a missing-declared-scope-file
// park fires from.
func seedRunningImplementStage(t *testing.T, repo run.Repository) (*run.Run, *run.Stage) {
	t.Helper()
	ctx := context.Background()
	r := makeRun(t, repo)
	s, err := repo.CreateStage(ctx, run.CreateStageParams{
		RunID:        r.ID,
		Sequence:     0,
		Type:         run.StageTypeImplement,
		ExecutorKind: run.ExecutorAgent,
		ExecutorRef:  "claude-code",
	})
	if err != nil {
		t.Fatalf("create implement stage: %v", err)
	}
	for _, to := range []run.StageState{run.StageStateDispatched, run.StageStateRunning} {
		if _, err := repo.TransitionStage(ctx, s.ID, to, nil); err != nil {
			t.Fatalf("transition to %s: %v", to, err)
		}
	}
	running, err := repo.GetStage(ctx, s.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	return r, running
}

// parkParams builds a scope_completeness_parked ChainAppendParams.
func parkParams(runID, stageID uuid.UUID, payload []byte) audit.ChainAppendParams {
	sid := stageID
	subject := "system"
	kind := audit.ActorKind("system")
	return audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &sid,
		Timestamp:    time.Now().UTC(),
		Category:     "scope_completeness_parked",
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	}
}

func TestPostgres_ParkScopeCompletenessAndAppend_RoundTrip(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)
	pk, ok := repo.(scopeCompletenessParker)
	if !ok {
		t.Fatal("postgres repo does not implement ParkScopeCompletenessAndAppend")
	}
	auditRepo := audit.NewPostgresRepository(pool)
	ctx := context.Background()

	r, running := seedRunningImplementStage(t, repo)
	park := run.ScopeCompletenessPark{
		HeldCommitSHA:   "1111111111111111111111111111111111111111",
		RunBranch:       "fishhawk/run-aaa/slice-0",
		VerifiedTreeSHA: "2222222222222222222222222222222222222222",
		MissingPaths:    []string{"backend/internal/foo/foo_test.go", "docs/foo.md"},
	}

	stage, won, err := pk.ParkScopeCompletenessAndAppend(ctx, running.ID, park, parkParams(r.ID, running.ID, []byte(`{"k":"v"}`)))
	if err != nil {
		t.Fatalf("ParkScopeCompletenessAndAppend: %v", err)
	}
	if !won {
		t.Fatal("won = false, want true for the single parking caller")
	}
	if stage.State != run.StageStateAwaitingScopeDecision {
		t.Errorf("stage state = %q, want awaiting_scope_decision", stage.State)
	}

	// The park payload round-trips through the JSONB column on a fresh read.
	got, err := repo.GetStage(ctx, running.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != run.StageStateAwaitingScopeDecision {
		t.Errorf("persisted stage state = %q, want awaiting_scope_decision", got.State)
	}
	if got.ScopeCompletenessPark == nil {
		t.Fatal("persisted ScopeCompletenessPark = nil, want round-tripped payload")
	}
	if got.ScopeCompletenessPark.HeldCommitSHA != park.HeldCommitSHA ||
		got.ScopeCompletenessPark.RunBranch != park.RunBranch ||
		got.ScopeCompletenessPark.VerifiedTreeSHA != park.VerifiedTreeSHA ||
		len(got.ScopeCompletenessPark.MissingPaths) != 2 {
		t.Errorf("ScopeCompletenessPark = %+v, want %+v", got.ScopeCompletenessPark, park)
	}

	// Exactly one scope_completeness_parked entry persisted in the same tx.
	entries, err := auditRepo.ListForRunByCategory(ctx, r.ID, "scope_completeness_parked")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("scope_completeness_parked entries = %d, want 1", len(entries))
	}
}

func TestPostgres_ParkScopeCompletenessAndAppend_LoserCAS(t *testing.T) {
	pool := startPostgres(t)
	repo := run.NewPostgresRepository(pool)
	pk := repo.(scopeCompletenessParker)
	auditRepo := audit.NewPostgresRepository(pool)
	ctx := context.Background()

	r, running := seedRunningImplementStage(t, repo)
	park := run.ScopeCompletenessPark{HeldCommitSHA: "abc", RunBranch: "b", VerifiedTreeSHA: "t", MissingPaths: []string{"x"}}

	// First caller wins.
	if _, won, err := pk.ParkScopeCompletenessAndAppend(ctx, running.ID, park, parkParams(r.ID, running.ID, []byte(`{}`))); err != nil || !won {
		t.Fatalf("first park: won=%v err=%v, want won=true nil", won, err)
	}
	// Second caller observes the stage already left running → no-op, won=false,
	// and must NOT append a second audit entry.
	stage, won, err := pk.ParkScopeCompletenessAndAppend(ctx, running.ID, park, parkParams(r.ID, running.ID, []byte(`{}`)))
	if err != nil {
		t.Fatalf("second park err = %v, want nil", err)
	}
	if won {
		t.Error("won = true for the losing caller, want false")
	}
	if stage == nil || stage.State != run.StageStateAwaitingScopeDecision {
		t.Errorf("loser stage = %+v, want awaiting_scope_decision row", stage)
	}
	entries, err := auditRepo.ListForRunByCategory(ctx, r.ID, "scope_completeness_parked")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("scope_completeness_parked entries = %d, want 1 (loser must not append)", len(entries))
	}
}
