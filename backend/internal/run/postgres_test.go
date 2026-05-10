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
