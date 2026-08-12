package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

// stampFor returns a stamp closure that records the attempt it was called with
// and serializes a minimal budget payload. attemptSeen is written under no
// lock; callers use it only in the sequential cases.
func stampFor(attemptSeen *int) func(int) (json.RawMessage, error) {
	return func(attempt int) (json.RawMessage, error) {
		if attemptSeen != nil {
			*attemptSeen = attempt
		}
		return json.Marshal(map[string]any{"attempt": attempt})
	}
}

// TestPostgres_AppendChainedUnderBudget_SequentialGrantThenExhaust is case (i):
// with maxEntries=1 the first call grants (attempt 1, one row) and the second
// returns ErrRetryBudgetExhausted and writes nothing.
func TestPostgres_AppendChainedUnderBudget_SequentialGrantThenExhaust(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	appender := repo.(audit.RetryBudgetAppender)
	runID := makeRun(t, pool)
	ctx := context.Background()

	var attempt int
	first, err := appender.AppendChainedUnderBudget(ctx, audit.ChainAppendParams{
		RunID: runID, Timestamp: time.Now().UTC(), Category: "plan_scope_retry",
	}, 1, stampFor(&attempt))
	if err != nil {
		t.Fatalf("first AppendChainedUnderBudget: %v", err)
	}
	if first == nil {
		t.Fatal("first call returned a nil entry")
	}
	if attempt != 1 {
		t.Errorf("first stamp attempt = %d, want 1", attempt)
	}

	second, err := appender.AppendChainedUnderBudget(ctx, audit.ChainAppendParams{
		RunID: runID, Timestamp: time.Now().UTC(), Category: "plan_scope_retry",
	}, 1, stampFor(nil))
	if !errors.Is(err, audit.ErrRetryBudgetExhausted) {
		t.Errorf("second call error = %v, want ErrRetryBudgetExhausted", err)
	}
	if second != nil {
		t.Errorf("second call returned a non-nil entry %v; it must write nothing", second)
	}

	if n := countCategory(t, repo, runID, "plan_scope_retry"); n != 1 {
		t.Errorf("plan_scope_retry rows = %d, want 1 (exhaustion writes nothing)", n)
	}
}

// TestPostgres_AppendChainedUnderBudget_ConcurrentGrantsExactlyOne is case (ii)
// AND the m7 concurrency proof: N goroutines released by one start barrier all
// call AppendChainedUnderBudget for the SAME run and category with maxEntries=1.
// Exactly one returns a nil-error entry, exactly N-1 return
// ErrRetryBudgetExhausted, and exactly one row is committed.
//
// Counterfactuals (run manually during implementation, restored after):
//   - Deleting the `count >= maxEntries` guard in AppendChainedUnderBudgetTx
//     reddens this with N committed rows.
//   - Moving the count ABOVE LockRunForUpdate reddens this with more than one
//     row — the ordering proof, a COMMITTED-ROW count (not a -race report), so
//     it cannot be a fake-internal race artefact.
func TestPostgres_AppendChainedUnderBudget_ConcurrentGrantsExactlyOne(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	appender := repo.(audit.RetryBudgetAppender)
	runID := makeRun(t, pool)
	ctx := context.Background()

	const n = 8
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	results := make([]error, n)
	entries := make([]*audit.Entry, n)
	for i := 0; i < n; i++ {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait() // barrier: every goroutine blocks until the release.
			e, err := appender.AppendChainedUnderBudget(ctx, audit.ChainAppendParams{
				RunID: runID, Timestamp: time.Now().UTC(), Category: "plan_scope_retry",
			}, 1, stampFor(nil))
			entries[i] = e
			results[i] = err
		}(i)
	}
	start.Done() // release all goroutines at once.
	done.Wait()

	grants, exhausted, other := 0, 0, 0
	for i, err := range results {
		switch {
		case err == nil:
			grants++
			if entries[i] == nil {
				t.Errorf("goroutine %d granted but returned a nil entry", i)
			}
		case errors.Is(err, audit.ErrRetryBudgetExhausted):
			exhausted++
			if entries[i] != nil {
				t.Errorf("goroutine %d exhausted but returned a non-nil entry", i)
			}
		default:
			other++
			t.Errorf("goroutine %d returned an unexpected error: %v", i, err)
		}
	}
	if grants != 1 {
		t.Errorf("grants = %d, want exactly 1", grants)
	}
	if exhausted != n-1 {
		t.Errorf("exhausted = %d, want %d", exhausted, n-1)
	}
	if other != 0 {
		t.Errorf("unexpected errors = %d, want 0", other)
	}
	if got := countCategory(t, repo, runID, "plan_scope_retry"); got != 1 {
		t.Errorf("committed plan_scope_retry rows = %d, want exactly 1", got)
	}
}

// TestPostgres_AppendChainedUnderBudget_ChainIntegrity is case (iii) / m8: the
// new write path must not fork the chain. A prior chained entry seeds a
// non-nil prev_hash, then a concurrent budget burst adds exactly one entry;
// walking the run's entries and recomputing each prev_hash / entry_hash the way
// the verifier does must succeed for the whole chain.
func TestPostgres_AppendChainedUnderBudget_ChainIntegrity(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	appender := repo.(audit.RetryBudgetAppender)
	runID := makeRun(t, pool)
	ctx := context.Background()

	// Seed a prior entry so the budget entry chains onto a non-nil prev_hash.
	if _, err := repo.AppendChained(ctx, audit.ChainAppendParams{
		RunID: runID, Timestamp: time.Now().UTC(), Category: "plan_generated",
		Payload: json.RawMessage(`{"seed":true}`),
	}); err != nil {
		t.Fatalf("seed prior entry: %v", err)
	}

	const n = 8
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	for i := 0; i < n; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			_, _ = appender.AppendChainedUnderBudget(ctx, audit.ChainAppendParams{
				RunID: runID, Timestamp: time.Now().UTC(), Category: "plan_scope_retry",
			}, 1, stampFor(nil))
		}()
	}
	start.Done()
	done.Wait()

	all, err := repo.ListForRun(ctx, runID)
	if err != nil {
		t.Fatalf("ListForRun: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("chain length = %d, want 2 (one seed + one budget entry)", len(all))
	}
	var prev *string
	for i, e := range all {
		if (prev == nil) != (e.PrevHash == nil) || (prev != nil && *prev != *e.PrevHash) {
			t.Errorf("entry %d prev_hash = %v, want %v (chain link broken)", i, e.PrevHash, prev)
		}
		want, herr := audit.ComputeEntryHash(audit.HashInputs{
			RunID:        e.RunID,
			StageID:      e.StageID,
			Timestamp:    e.Timestamp,
			Category:     e.Category,
			ActorKind:    e.ActorKind,
			ActorSubject: e.ActorSubject,
			Payload:      e.Payload,
			PrevHash:     e.PrevHash,
		})
		if herr != nil {
			t.Fatalf("recompute entry %d hash: %v", i, herr)
		}
		if want != e.EntryHash {
			t.Errorf("entry %d hash mismatch: stored %s, recomputed %s", i, e.EntryHash, want)
		}
		prev = &e.EntryHash
	}
}

// TestPostgres_AppendChainedUnderBudget_StampError is case (iv): a stamp
// function that returns an error aborts the transaction and writes nothing.
func TestPostgres_AppendChainedUnderBudget_StampError(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	appender := repo.(audit.RetryBudgetAppender)
	runID := makeRun(t, pool)
	ctx := context.Background()

	stampErr := errors.New("cannot build payload")
	entry, err := appender.AppendChainedUnderBudget(ctx, audit.ChainAppendParams{
		RunID: runID, Timestamp: time.Now().UTC(), Category: "plan_scope_retry",
	}, 1, func(int) (json.RawMessage, error) { return nil, stampErr })
	if !errors.Is(err, stampErr) {
		t.Errorf("error = %v, want the stamp error", err)
	}
	if entry != nil {
		t.Errorf("entry = %v, want nil on stamp error", entry)
	}
	if n := countCategory(t, repo, runID, "plan_scope_retry"); n != 0 {
		t.Errorf("plan_scope_retry rows = %d, want 0 (a stamp error writes nothing)", n)
	}
}

// TestPostgres_AppendChainedUnderBudget_UnknownRun is case (v): an unknown
// run_id returns an error and writes nothing (the row lock finds no run).
func TestPostgres_AppendChainedUnderBudget_UnknownRun(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	appender := repo.(audit.RetryBudgetAppender)
	unknown := uuid.New()
	ctx := context.Background()

	entry, err := appender.AppendChainedUnderBudget(ctx, audit.ChainAppendParams{
		RunID: unknown, Timestamp: time.Now().UTC(), Category: "plan_scope_retry",
	}, 1, stampFor(nil))
	if err == nil {
		t.Error("error = nil, want a run-not-found error")
	}
	if errors.Is(err, audit.ErrRetryBudgetExhausted) {
		t.Errorf("error = ErrRetryBudgetExhausted, want a run-not-found error")
	}
	if entry != nil {
		t.Errorf("entry = %v, want nil for an unknown run", entry)
	}
}

// countCategory returns the number of committed audit rows for the run+category.
func countCategory(t *testing.T, repo audit.Repository, runID uuid.UUID, category string) int {
	t.Helper()
	entries, err := repo.ListForRunByCategory(context.Background(), runID, category)
	if err != nil {
		t.Fatalf("ListForRunByCategory(%s): %v", category, err)
	}
	return len(entries)
}
