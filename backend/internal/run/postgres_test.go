package run_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

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
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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

// TestPostgres_UpstreamRunID_RoundTrip exercises migration 0043 (#1417): a
// run created with UpstreamRunID set reads it back (proving the migration +
// sqlc regen + rowToRun mapping agree), and a run created without one reads
// back nil — the appended-deploy / legacy-row default. The upstream run is
// created first because upstream_run_id is an FK to runs(id).
func TestPostgres_UpstreamRunID_RoundTrip(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)

	upstream := makeRun(t, repo)
	deployRun, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "release",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
		UpstreamRunID: &upstream.ID,
	})
	if err != nil {
		t.Fatalf("create deploy run: %v", err)
	}
	if deployRun.UpstreamRunID == nil || *deployRun.UpstreamRunID != upstream.ID {
		t.Errorf("created UpstreamRunID = %v, want %v", deployRun.UpstreamRunID, upstream.ID)
	}
	got, err := repo.GetRun(context.Background(), deployRun.ID)
	if err != nil {
		t.Fatalf("get deploy run: %v", err)
	}
	if got.UpstreamRunID == nil || *got.UpstreamRunID != upstream.ID {
		t.Errorf("read-back UpstreamRunID = %v, want %v", got.UpstreamRunID, upstream.ID)
	}

	// A run created without an UpstreamRunID (appended-deploy / non-deploy)
	// reads back nil — today's current-run evaluation default.
	plain := makeRun(t, repo)
	if plain.UpstreamRunID != nil {
		t.Errorf("default UpstreamRunID = %v, want nil", plain.UpstreamRunID)
	}
	gotPlain, err := repo.GetRun(context.Background(), plain.ID)
	if err != nil {
		t.Fatalf("get plain run: %v", err)
	}
	if gotPlain.UpstreamRunID != nil {
		t.Errorf("read-back default UpstreamRunID = %v, want nil", gotPlain.UpstreamRunID)
	}
}

func TestPostgres_GetRun_NotFound(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)

	_, err := repo.GetRun(context.Background(), uuid.New())
	if !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestPostgres_GetRunByIdempotencyKey_HappyPath(t *testing.T) {
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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

// TestPostgres_ListRuns_ParentRunIDFilter proves the new ParentRunID filter
// (#1751) selects only recovery children minted with parent_run_id = the given
// run, and DOES NOT conflate them with decomposition children (decomposed_from)
// or unrelated runs. This is the cross-layer guard for the hand-edited
// positional-parameter renumber in db/queries.sql.go: a wrong $8/$9/$10 mapping
// fails here.
func TestPostgres_ListRuns_ParentRunIDFilter(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	ctx := context.Background()

	parent := makeRun(t, repo)

	// A recovery child carries parent_run_id = parent.
	recoveryChild, err := repo.CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "sha-rec",
		TriggerSource: run.TriggerCLI, ParentRunID: &parent.ID,
	})
	if err != nil {
		t.Fatalf("create recovery child: %v", err)
	}
	// A decomposition child carries decomposed_from = parent — a DISTINCT
	// lineage that the ParentRunID filter must NOT return.
	if _, err := repo.CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "sha-dec",
		TriggerSource: run.TriggerCLI, DecomposedFrom: &parent.ID,
	}); err != nil {
		t.Fatalf("create decomposition child: %v", err)
	}
	// An unrelated run with no lineage pointer at all.
	if _, err := repo.CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "sha-unrel",
		TriggerSource: run.TriggerCLI,
	}); err != nil {
		t.Fatalf("create unrelated run: %v", err)
	}

	got, err := repo.ListRuns(ctx, run.ListRunsFilter{ParentRunID: &parent.ID, Limit: 10})
	if err != nil {
		t.Fatalf("list runs by parent_run_id: %v", err)
	}
	if len(got) != 1 || got[0].ID != recoveryChild.ID {
		t.Fatalf("ParentRunID filter = %d runs %v, want exactly the recovery child %s", len(got), runIDs(got), recoveryChild.ID)
	}
	if got[0].ParentRunID == nil || *got[0].ParentRunID != parent.ID {
		t.Errorf("recovery child ParentRunID = %v, want %s", got[0].ParentRunID, parent.ID)
	}

	// A nil ParentRunID is no constraint: every run (parent + 3 children) is
	// returned, proving the filter defaults off.
	all, err := repo.ListRuns(ctx, run.ListRunsFilter{Limit: 10})
	if err != nil {
		t.Fatalf("list runs unfiltered: %v", err)
	}
	if len(all) != 4 {
		t.Errorf("unfiltered = %d, want 4 (parent + 3 children)", len(all))
	}
}

// runIDs projects run ids for test failure messages.
func runIDs(runs []*run.Run) []uuid.UUID {
	out := make([]uuid.UUID, len(runs))
	for i, r := range runs {
		out[i] = r.ID
	}
	return out
}

func TestPostgres_TransitionRun_HappyPath(t *testing.T) {
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)

	r := makeRun(t, repo) // pending
	_, err := repo.RetryRun(context.Background(), r.ID, run.StateRunning)
	var inv run.InvalidTransitionError
	if !errors.As(err, &inv) {
		t.Fatalf("err = %v, want InvalidTransitionError", err)
	}
}

func TestPostgres_TransitionRun_Idempotent(t *testing.T) {
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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

// TestPostgres_TransitionStageFrom_MismatchRefused pins the CAS refusal
// against real Postgres (#1903): with the stage parked awaiting_children, a
// TransitionStageFrom anchored to the stale `pending` expected state must
// refuse atomically under the FOR UPDATE row lock — returning a typed
// StageStateChangedError and leaving the park's row completely unchanged (no
// failure metadata, no ended_at stamped).
func TestPostgres_TransitionStageFrom_MismatchRefused(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	ctx := context.Background()

	r := makeRun(t, repo)
	s := makeStage(t, repo, r.ID, 0) // pending
	// Park it awaiting_children (pending → awaiting_children is a base edge).
	if _, err := repo.TransitionStage(ctx, s.ID, run.StageStateAwaitingChildren, nil); err != nil {
		t.Fatalf("park awaiting_children: %v", err)
	}

	cas, ok := repo.(run.StageCASTransitioner)
	if !ok {
		t.Fatal("postgres repo does not implement StageCASTransitioner")
	}
	cat := run.FailureC
	reason := "raced reap against a decomposed parent"
	_, err := cas.TransitionStageFrom(ctx, s.ID, run.StageStatePending, run.StageStateFailed,
		&run.StageCompletion{FailureCategory: &cat, FailureReason: &reason})
	if err == nil {
		t.Fatal("TransitionStageFrom against a stale expected state returned nil error")
	}
	var sce run.StageStateChangedError
	if !errors.As(err, &sce) {
		t.Fatalf("error = %v, want StageStateChangedError via errors.As", err)
	}
	if sce.Expected != run.StageStatePending || sce.Actual != run.StageStateAwaitingChildren {
		t.Errorf("StageStateChangedError = {expected:%q actual:%q}, want {pending awaiting_children}",
			sce.Expected, sce.Actual)
	}

	// The park row is intact: still awaiting_children, no failure metadata,
	// no ended_at stamped.
	cur, err := repo.GetStage(ctx, s.ID)
	if err != nil {
		t.Fatalf("get stage: %v", err)
	}
	if cur.State != run.StageStateAwaitingChildren {
		t.Errorf("state = %q, want awaiting_children (park preserved)", cur.State)
	}
	if cur.FailureCategory != nil || cur.FailureReason != nil {
		t.Errorf("failure metadata stamped on a refused CAS: cat=%v reason=%v",
			cur.FailureCategory, cur.FailureReason)
	}
	if cur.EndedAt != nil {
		t.Errorf("ended_at stamped on a refused CAS: %v", cur.EndedAt)
	}
}

// TestPostgres_TransitionStageFrom_MatchTransitions pins that when the
// expected from-state matches, TransitionStageFrom applies identical
// completion semantics to TransitionStage: the stage lands failed with the
// category/reason and a stamped ended_at (#1903).
func TestPostgres_TransitionStageFrom_MatchTransitions(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	ctx := context.Background()

	r := makeRun(t, repo)
	s := makeStage(t, repo, r.ID, 0) // pending
	if _, err := repo.TransitionStage(ctx, s.ID, run.StageStateDispatched, nil); err != nil {
		t.Fatalf("→dispatched: %v", err)
	}
	if _, err := repo.TransitionStage(ctx, s.ID, run.StageStateRunning, nil); err != nil {
		t.Fatalf("→running: %v", err)
	}

	cas := repo.(run.StageCASTransitioner)
	cat := run.FailureB
	reason := "policy violation post-trace"
	got, err := cas.TransitionStageFrom(ctx, s.ID, run.StageStateRunning, run.StageStateFailed,
		&run.StageCompletion{FailureCategory: &cat, FailureReason: &reason})
	if err != nil {
		t.Fatalf("TransitionStageFrom (matching expected): %v", err)
	}
	if got.State != run.StageStateFailed {
		t.Errorf("state = %q, want failed", got.State)
	}
	if got.FailureCategory == nil || *got.FailureCategory != run.FailureB {
		t.Errorf("failure category = %v, want B", got.FailureCategory)
	}
	if got.FailureReason == nil || *got.FailureReason != reason {
		t.Errorf("failure reason = %v, want %q", got.FailureReason, reason)
	}
	if got.EndedAt == nil {
		t.Error("ended_at not stamped on terminal CAS transition")
	}
}

func TestPostgres_ListStagesForRun_OrderedBySequence(t *testing.T) {
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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

func TestPostgres_DeployStage_PersistRoundTrip(t *testing.T) {
	// #1400 done-means: a real deploy stage row must be insertable and its
	// deploy-only type + states must round-trip through Postgres. Before
	// migration 0038 widened stages_type_check ('deploy') and
	// stages_state_check ('awaiting_deploy_approval' / 'awaiting_deployment'),
	// the CreateStage insert here failed with SQLSTATE 23514 (check_violation)
	// and the transitions were unreachable. The insert succeeding IS the
	// load-bearing assertion (the Go fake doesn't enforce the CHECK).
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	ctx := context.Background()
	r := makeRun(t, repo)

	s, err := repo.CreateStage(ctx, run.CreateStageParams{
		RunID:        r.ID,
		Sequence:     0,
		Type:         run.StageTypeDeploy,
		ExecutorKind: run.ExecutorAgent,
		ExecutorRef:  "claude-code",
	})
	if err != nil {
		t.Fatalf("CreateStage(deploy): %v", err)
	}
	if s.Type != run.StageTypeDeploy {
		t.Errorf("CreateStage Type = %q, want %q", s.Type, run.StageTypeDeploy)
	}

	// Walk the deploy lifecycle: pending → awaiting_deploy_approval (the
	// pre-execution gate park) → dispatched → running → awaiting_deployment
	// (the in-flight external-pipeline poll). Each transition's persisted
	// state must satisfy the widened stages_state_check.
	for _, to := range []run.StageState{
		run.StageStateAwaitingDeployApproval,
		run.StageStateDispatched,
		run.StageStateRunning,
		run.StageStateAwaitingDeployment,
	} {
		got, err := repo.TransitionStage(ctx, s.ID, to, nil)
		if err != nil {
			t.Fatalf("→%s: %v", to, err)
		}
		if got.State != to {
			t.Errorf("after transition State = %q, want %q", got.State, to)
		}
	}

	// GetStage confirms the deploy type + the in-flight deploy state survive
	// a fresh read from Postgres.
	got, err := repo.GetStage(ctx, s.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.Type != run.StageTypeDeploy {
		t.Errorf("GetStage Type = %q, want %q", got.Type, run.StageTypeDeploy)
	}
	if got.State != run.StageStateAwaitingDeployment {
		t.Errorf("GetStage State = %q, want %q", got.State, run.StageStateAwaitingDeployment)
	}
}

func TestPostgres_AcceptanceStage_PersistRoundTrip(t *testing.T) {
	// #1519 done-means: a real acceptance stage row must be insertable and its
	// type must round-trip through Postgres. This is the load-bearing test for
	// the "constant + migration MUST ship together" acceptance criterion:
	// before migration 0044 widened stages_type_check to admit 'acceptance',
	// the CreateStage insert here fails with SQLSTATE 23514 (check_violation) —
	// the Go fake doesn't enforce the CHECK, so only a real Postgres insert
	// proves the pairing (the exact #1390/#1399 deploy failure mode). Unlike
	// the deploy round-trip, acceptance adds NO new states: it walks the
	// existing agent-stage lifecycle exactly like review.
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	ctx := context.Background()
	r := makeRun(t, repo)

	s, err := repo.CreateStage(ctx, run.CreateStageParams{
		RunID:        r.ID,
		Sequence:     0,
		Type:         run.StageTypeAcceptance,
		ExecutorKind: run.ExecutorAgent,
		ExecutorRef:  "claude-code",
	})
	if err != nil {
		t.Fatalf("CreateStage(acceptance): %v", err)
	}
	if s.Type != run.StageTypeAcceptance {
		t.Errorf("CreateStage Type = %q, want %q", s.Type, run.StageTypeAcceptance)
	}

	// Walk the ordinary agent-stage lifecycle: pending → dispatched → running
	// → succeeded. Each persisted state must satisfy the UNCHANGED
	// stages_state_check (0044 widens only the type CHECK).
	for _, to := range []run.StageState{
		run.StageStateDispatched,
		run.StageStateRunning,
		run.StageStateSucceeded,
	} {
		got, err := repo.TransitionStage(ctx, s.ID, to, nil)
		if err != nil {
			t.Fatalf("→%s: %v", to, err)
		}
		if got.State != to {
			t.Errorf("after transition State = %q, want %q", got.State, to)
		}
	}

	// GetStage confirms the acceptance type survives a fresh read from Postgres.
	got, err := repo.GetStage(ctx, s.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.Type != run.StageTypeAcceptance {
		t.Errorf("GetStage Type = %q, want %q", got.Type, run.StageTypeAcceptance)
	}
	if got.State != run.StageStateSucceeded {
		t.Errorf("GetStage State = %q, want %q", got.State, run.StageStateSucceeded)
	}
}

// rollbackLister is the narrow type-assert for the rollback-pending listing,
// which is off the broad run.Repository interface by design (#1398).
type rollbackLister interface {
	ListDeployStagesRollbackPending(ctx context.Context) ([]*run.Stage, error)
}

func TestPostgres_ListDeployStagesRollbackPending(t *testing.T) {
	// #1398 done-means: the rollback scan must return a deploy stage with a
	// deployment_rollback_initiated audit entry and NO deployment_rollback_completed,
	// EXCLUDE one that has both, and EXCLUDE a non-deploy stage (the EXISTS /
	// NOT-EXISTS join across audit_entries is the crux — not compile-enforced).
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	lister, ok := repo.(rollbackLister)
	if !ok {
		t.Fatal("postgres repo does not implement ListDeployStagesRollbackPending")
	}
	auditRepo := audit.NewPostgresRepository(pool)
	ctx := context.Background()
	r := makeRun(t, repo)

	// deployStage parks a fresh deploy stage at a terminal (succeeded) state —
	// the realistic precondition for a rollback (only a settled deploy is
	// rolled back).
	deployStage := func(seq int) *run.Stage {
		s, err := repo.CreateStage(ctx, run.CreateStageParams{
			RunID: r.ID, Sequence: seq, Type: run.StageTypeDeploy,
			ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
		})
		if err != nil {
			t.Fatalf("CreateStage(deploy seq=%d): %v", seq, err)
		}
		// pending → awaiting_deploy_approval → dispatched → running →
		// awaiting_deployment → succeeded.
		for _, to := range []run.StageState{
			run.StageStateAwaitingDeployApproval, run.StageStateDispatched,
			run.StageStateRunning, run.StageStateAwaitingDeployment, run.StageStateSucceeded,
		} {
			if _, err := repo.TransitionStage(ctx, s.ID, to, nil); err != nil {
				t.Fatalf("→%s: %v", to, err)
			}
		}
		return s
	}
	appendAudit := func(stageID uuid.UUID, category string) {
		sid := stageID
		kind := audit.ActorKind("system")
		if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID:     r.ID,
			StageID:   &sid,
			Timestamp: time.Now().UTC(),
			Category:  category,
			ActorKind: &kind,
			Payload:   []byte(`{"rollback":true}`),
		}); err != nil {
			t.Fatalf("append %s: %v", category, err)
		}
	}

	// A: initiated, NOT completed → pending (should be returned).
	pending := deployStage(0)
	appendAudit(pending.ID, "deployment_rollback_initiated")

	// B: initiated AND completed → finalized (should be EXCLUDED).
	done := deployStage(1)
	appendAudit(done.ID, "deployment_rollback_initiated")
	appendAudit(done.ID, "deployment_rollback_completed")

	// C: a non-deploy (plan) stage carrying a rollback_initiated entry →
	// EXCLUDED by the stage_type = 'deploy' filter.
	nonDeploy := makeStage(t, repo, r.ID, 2)
	appendAudit(nonDeploy.ID, "deployment_rollback_initiated")

	got, err := lister.ListDeployStagesRollbackPending(ctx)
	if err != nil {
		t.Fatalf("ListDeployStagesRollbackPending: %v", err)
	}
	ids := map[uuid.UUID]bool{}
	for _, s := range got {
		ids[s.ID] = true
	}
	if !ids[pending.ID] {
		t.Errorf("rollback-pending listing dropped the initiated-only deploy stage %s", pending.ID)
	}
	if ids[done.ID] {
		t.Errorf("rollback-pending listing included the already-completed deploy stage %s", done.ID)
	}
	if ids[nonDeploy.ID] {
		t.Errorf("rollback-pending listing included the non-deploy stage %s", nonDeploy.ID)
	}
}

func TestPostgres_ListStagesAwaitingApproval_IncludesDeployStage(t *testing.T) {
	// The SLA ticker's gate_sla-filtered query (ListStagesAwaitingApproval)
	// was broadened (#1390 / #1399) to also surface deploy stages parked at
	// awaiting_deploy_approval with a non-null gate_sla. This real-SQL test
	// pins that a deploy stage with a gate_sla is returned — the insert and
	// the awaiting_deploy_approval park both depend on migration 0038.
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	ctx := context.Background()
	r := makeRun(t, repo)

	sla := "4_business_hours"
	s, err := repo.CreateStage(ctx, run.CreateStageParams{
		RunID:            r.ID,
		Sequence:         0,
		Type:             run.StageTypeDeploy,
		ExecutorKind:     run.ExecutorAgent,
		ExecutorRef:      "claude-code",
		RequiresApproval: true,
		GateSLA:          &sla,
	})
	if err != nil {
		t.Fatalf("CreateStage(deploy): %v", err)
	}
	// Park directly at the pre-execution gate (pending → awaiting_deploy_approval).
	if _, err := repo.TransitionStage(ctx, s.ID, run.StageStateAwaitingDeployApproval, nil); err != nil {
		t.Fatalf("→awaiting_deploy_approval: %v", err)
	}

	stages, err := repo.ListStagesAwaitingApproval(ctx)
	if err != nil {
		t.Fatalf("ListStagesAwaitingApproval: %v", err)
	}
	got := map[uuid.UUID]bool{}
	for _, st := range stages {
		got[st.ID] = true
	}
	if !got[s.ID] {
		t.Errorf("ListStagesAwaitingApproval dropped the awaiting_deploy_approval deploy stage %s (gate_sla set)", s.ID)
	}
}

func TestPostgres_StageGate_NilWhenSpecHasNoGate(t *testing.T) {
	// The dispatcher passes Gate=nil for gateless stages (e.g.
	// implement). The persisted row's Gate must come back nil too,
	// otherwise the UI would mis-render an "approval coming" panel
	// for a stage that's never going to need one.
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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

// runnerKindResolverRepo is the optional runner_kind reconciliation surface
// the trace handler consumes via capability assertion (#1346 / ADR-045).
// ResolveRunnerKind is not part of run.Repository (adding it would break the
// many hand-rolled fakes), so the test asserts for it the same way the
// server does.
type runnerKindResolverRepo interface {
	ResolveRunnerKind(ctx context.Context, runID uuid.UUID, observed string) (run.RunnerKindResolution, error)
}

func runnerKindResolver(t *testing.T, repo run.Repository) runnerKindResolverRepo {
	t.Helper()
	rk, ok := repo.(runnerKindResolverRepo)
	if !ok {
		t.Fatal("postgres repo does not implement ResolveRunnerKind")
	}
	return rk
}

// TestPostgres_ResolveRunnerKind_FirstReportFlipsAndLocks is the #1344 fix
// (done-means #1): an un-resolved run defaulting to github_actions, on its
// FIRST signed-manifest report of `local`, flips runner_kind to local and
// locks it (runner_kind_resolved=true).
func TestPostgres_ResolveRunnerKind_FirstReportFlipsAndLocks(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	rk := runnerKindResolver(t, repo)
	ctx := context.Background()

	r := makeRun(t, repo) // default runner_kind = github_actions, unresolved
	if r.RunnerKind != run.RunnerKindGitHubActions || r.RunnerKindResolved {
		t.Fatalf("seed: runner_kind=%q resolved=%v, want github_actions/false", r.RunnerKind, r.RunnerKindResolved)
	}

	res, err := rk.ResolveRunnerKind(ctx, r.ID, run.RunnerKindLocal)
	if err != nil {
		t.Fatalf("ResolveRunnerKind: %v", err)
	}
	if !res.Changed || res.Mismatch || res.Locked != run.RunnerKindLocal || res.Prior != run.RunnerKindGitHubActions {
		t.Errorf("resolution = %+v, want Changed locked=local prior=github_actions", res)
	}
	got, err := repo.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.RunnerKind != run.RunnerKindLocal || !got.RunnerKindResolved {
		t.Errorf("persisted runner_kind=%q resolved=%v, want local/true", got.RunnerKind, got.RunnerKindResolved)
	}
}

// TestPostgres_ResolveRunnerKind_FirstReportLocksSameValue is done-means #2:
// a first report of github_actions on the github_actions-default run locks
// it (resolved=true) but reports Changed=false (the hint was already right).
func TestPostgres_ResolveRunnerKind_FirstReportLocksSameValue(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	rk := runnerKindResolver(t, repo)
	ctx := context.Background()

	r := makeRun(t, repo)
	res, err := rk.ResolveRunnerKind(ctx, r.ID, run.RunnerKindGitHubActions)
	if err != nil {
		t.Fatalf("ResolveRunnerKind: %v", err)
	}
	if res.Changed || res.Mismatch || res.Locked != run.RunnerKindGitHubActions {
		t.Errorf("resolution = %+v, want locked=github_actions not changed not mismatch", res)
	}
	got, err := repo.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.RunnerKind != run.RunnerKindGitHubActions || !got.RunnerKindResolved {
		t.Errorf("persisted runner_kind=%q resolved=%v, want github_actions/true", got.RunnerKind, got.RunnerKindResolved)
	}
}

// TestPostgres_ResolveRunnerKind_MismatchLeavesRowUnchanged is done-means #3,
// BOTH guardrail directions: once a run is LOCKED, a later report disagreeing
// with the locked kind returns Mismatch and does NOT mutate the row.
func TestPostgres_ResolveRunnerKind_MismatchLeavesRowUnchanged(t *testing.T) {
	cases := []struct {
		name        string
		lockTo      string
		thenObserve string
	}{
		{name: "locked_local_observe_github", lockTo: run.RunnerKindLocal, thenObserve: run.RunnerKindGitHubActions},
		{name: "locked_github_observe_local", lockTo: run.RunnerKindGitHubActions, thenObserve: run.RunnerKindLocal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := pgtest.NewPool(t)
			repo := run.NewPostgresRepository(pool)
			rk := runnerKindResolver(t, repo)
			ctx := context.Background()

			r := makeRun(t, repo)
			// First report locks runner_kind to lockTo.
			if _, err := rk.ResolveRunnerKind(ctx, r.ID, tc.lockTo); err != nil {
				t.Fatalf("lock ResolveRunnerKind: %v", err)
			}
			// Disagreeing second report.
			res, err := rk.ResolveRunnerKind(ctx, r.ID, tc.thenObserve)
			if err != nil {
				t.Fatalf("mismatch ResolveRunnerKind: %v", err)
			}
			if !res.Mismatch || res.Changed {
				t.Errorf("resolution = %+v, want Mismatch not Changed", res)
			}
			if res.Prior != tc.lockTo || res.Observed != tc.thenObserve {
				t.Errorf("resolution prior/observed = %q/%q, want %q/%q", res.Prior, res.Observed, tc.lockTo, tc.thenObserve)
			}
			got, err := repo.GetRun(ctx, r.ID)
			if err != nil {
				t.Fatalf("GetRun: %v", err)
			}
			if got.RunnerKind != tc.lockTo || !got.RunnerKindResolved {
				t.Errorf("row mutated on mismatch: runner_kind=%q resolved=%v, want %q/true (unchanged)", got.RunnerKind, got.RunnerKindResolved, tc.lockTo)
			}
		})
	}
}

// TestPostgres_ResolveRunnerKind_UnrecognizedReportIsNoOp covers the
// fail-closed guard: an empty or non-ValidRunnerKind observed value never
// persists (the create-time hint stands) and returns the zero resolution.
func TestPostgres_ResolveRunnerKind_UnrecognizedReportIsNoOp(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	rk := runnerKindResolver(t, repo)
	ctx := context.Background()

	r := makeRun(t, repo)
	for _, observed := range []string{"", "kubernetes", "GITHUB_ACTIONS"} {
		res, err := rk.ResolveRunnerKind(ctx, r.ID, observed)
		if err != nil {
			t.Fatalf("ResolveRunnerKind(%q): %v", observed, err)
		}
		if res != (run.RunnerKindResolution{}) {
			t.Errorf("ResolveRunnerKind(%q) = %+v, want zero (no-op)", observed, res)
		}
	}
	got, err := repo.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.RunnerKind != run.RunnerKindGitHubActions || got.RunnerKindResolved {
		t.Errorf("row mutated by unrecognized report: runner_kind=%q resolved=%v, want github_actions/false", got.RunnerKind, got.RunnerKindResolved)
	}
}

// TestPostgres_ResolveRunnerKind_NotFound covers the missing-run error
// branch: a valid observed value against a non-existent run returns
// ErrNotFound (which the trace handler degrades to a WARN).
func TestPostgres_ResolveRunnerKind_NotFound(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	rk := runnerKindResolver(t, repo)

	_, err := rk.ResolveRunnerKind(context.Background(), uuid.New(), run.RunnerKindLocal)
	if !errors.Is(err, run.ErrNotFound) {
		t.Errorf("ResolveRunnerKind on missing run = %v, want ErrNotFound", err)
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
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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
	pool := pgtest.NewPool(t)
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
