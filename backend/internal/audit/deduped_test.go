package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// deduped_test.go drives AppendChainedDeduped / AppendChainedDedupedTx (#3439)
// against a REAL Postgres, exactly as postgres_test.go drives the anchored
// primitive: the run-row lock and the in-transaction scan live in the store,
// so a fake cannot exercise them. makeRun / makeStageInRun come from
// postgres_test.go (same package).

const (
	dedupedCat = "acceptance_scenario_retirement_dropped"
	dedupedKey = "reason"
)

// dedupedPayload renders a payload carrying reason as a JSON string plus a
// per-call salt so two appends never share an entry hash.
func dedupedPayload(reason string) json.RawMessage {
	p, _ := json.Marshal(map[string]any{dedupedKey: reason, "salt": uuid.NewString()})
	return p
}

// dedupedSpec is the retirement-drop DedupeSpec for stageID + reason.
func dedupedSpec(stageID *uuid.UUID, reason string) audit.DedupeSpec {
	return audit.DedupeSpec{StageID: stageID, PayloadKey: dedupedKey, PayloadValue: reason}
}

// makeSecondStageInRun adds a SECOND stage (sequence 1) under runID —
// makeStageInRun (postgres_test.go) always creates sequence 0, and
// (run_id, sequence) is unique.
func makeSecondStageInRun(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) uuid.UUID {
	t.Helper()
	st, err := run.NewPostgresRepository(pool).CreateStage(context.Background(), run.CreateStageParams{
		RunID: runID, Sequence: 1, Type: run.StageTypeAcceptance,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create second stage: %v", err)
	}
	return st.ID
}

// countDedupedRows reports how many dedupedCat rows the run committed.
func countDedupedRows(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_entries WHERE run_id = $1 AND category = $2`,
		runID, dedupedCat).Scan(&n); err != nil {
		t.Fatalf("count %s rows: %v", dedupedCat, err)
	}
	return n
}

// appendDeduped is the one-call form over the production repo.
func appendDeduped(pool *pgxpool.Pool, runID uuid.UUID, stageID *uuid.UUID, reason string, payload json.RawMessage) (*audit.Entry, error) {
	appender := audit.NewPostgresRepository(pool).(audit.DedupedChainAppender)
	return appender.AppendChainedDeduped(context.Background(), audit.ChainAppendParams{
		RunID: runID, StageID: stageID, Timestamp: time.Now().UTC(), Category: dedupedCat,
		Payload: payload,
	}, dedupedSpec(stageID, reason))
}

// (a) happy path: no duplicate exists, one chained entry lands and links to
// the run's prior entry.
func TestPostgres_AppendChainedDeduped_HappyPath(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)
	stageID := makeStageInRun(t, pool, runID)

	// A prior entry of ANOTHER category so the append has something to chain to.
	prior, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, Timestamp: time.Now().UTC(), Category: "approval_submitted",
		Payload: json.RawMessage(`{"decision":"approve"}`),
	})
	if err != nil {
		t.Fatalf("seed prior entry: %v", err)
	}

	entry, err := appendDeduped(pool, runID, &stageID, "run_cancelled_before_acceptance", dedupedPayload("run_cancelled_before_acceptance"))
	if err != nil {
		t.Fatalf("AppendChainedDeduped: %v", err)
	}
	if entry == nil || entry.Sequence <= prior.Sequence {
		t.Fatalf("entry = %+v, want a chained entry above the prior sequence %d", entry, prior.Sequence)
	}
	if entry.PrevHash == nil || *entry.PrevHash != prior.EntryHash {
		t.Errorf("PrevHash = %v, want the prior entry's hash %s (the append must chain)", entry.PrevHash, prior.EntryHash)
	}
	if n := countDedupedRows(t, pool, runID); n != 1 {
		t.Errorf("committed %s rows = %d, want 1", dedupedCat, n)
	}
}

// (b) a prior COMMITTED row with the same stage + reason is a duplicate: the
// typed error names it, and — the load-bearing half — the count stays 1.
// Counterfactual: delete the step-2 scan in AppendChainedDedupedTx → the
// second call appends and this test fails on both assertions.
func TestPostgres_AppendChainedDeduped_InTransactionDuplicate(t *testing.T) {
	pool := pgtest.NewPool(t)
	runID := makeRun(t, pool)
	stageID := makeStageInRun(t, pool, runID)

	first, err := appendDeduped(pool, runID, &stageID, "persist_failed", dedupedPayload("persist_failed"))
	if err != nil {
		t.Fatalf("first append: %v", err)
	}
	second, err := appendDeduped(pool, runID, &stageID, "persist_failed", dedupedPayload("persist_failed"))
	var dup *audit.DedupedDuplicateError
	if !errors.As(err, &dup) {
		t.Fatalf("second append err = %v (entry %+v), want *DedupedDuplicateError", err, second)
	}
	if second != nil {
		t.Errorf("second append returned an entry %+v alongside the duplicate error", second)
	}
	if dup.Existing == nil || dup.Existing.ID != first.ID {
		t.Errorf("Existing = %+v, want the first committed entry %s", dup.Existing, first.ID)
	}
	if n := countDedupedRows(t, pool, runID); n != 1 {
		t.Errorf("committed %s rows = %d, want 1 (nothing written on the duplicate branch)", dedupedCat, n)
	}
}

// (c) the key is (stage, reason), both halves: same reason on ANOTHER stage
// appends; same stage with ANOTHER reason appends; and a nil-stage prior row
// is NOT a duplicate of a stage-scoped spec.
func TestPostgres_AppendChainedDeduped_DifferentStageOrReasonAppends(t *testing.T) {
	pool := pgtest.NewPool(t)
	runID := makeRun(t, pool)
	stageA := makeStageInRun(t, pool, runID)
	stageB := makeSecondStageInRun(t, pool, runID)

	// A nil-stage prior row carrying the reason under test.
	if _, err := appendDeduped(pool, runID, nil, "persist_failed", dedupedPayload("persist_failed")); err != nil {
		t.Fatalf("nil-stage seed: %v", err)
	}
	// Stage-scoped spec vs the nil-stage row: NOT a duplicate.
	if _, err := appendDeduped(pool, runID, &stageA, "persist_failed", dedupedPayload("persist_failed")); err != nil {
		t.Fatalf("stage A, same reason as the nil-stage row: %v, want an append", err)
	}
	// Same reason, other stage: appends.
	if _, err := appendDeduped(pool, runID, &stageB, "persist_failed", dedupedPayload("persist_failed")); err != nil {
		t.Fatalf("stage B, same reason as stage A: %v, want an append", err)
	}
	// Same stage, other reason: appends.
	if _, err := appendDeduped(pool, runID, &stageA, "no_run_branch", dedupedPayload("no_run_branch")); err != nil {
		t.Fatalf("stage A, other reason: %v, want an append", err)
	}
	if n := countDedupedRows(t, pool, runID); n != 4 {
		t.Errorf("committed %s rows = %d, want 4", dedupedCat, n)
	}
	// And the guard still bites on an exact (stage, reason) repeat.
	_, err := appendDeduped(pool, runID, &stageA, "no_run_branch", dedupedPayload("no_run_branch"))
	var dup *audit.DedupedDuplicateError
	if !errors.As(err, &dup) {
		t.Errorf("exact repeat err = %v, want *DedupedDuplicateError", err)
	}
	if n := countDedupedRows(t, pool, runID); n != 4 {
		t.Errorf("committed %s rows after the exact repeat = %d, want 4", dedupedCat, n)
	}
}

// (d) the scan SKIPS unusable rows: a malformed payload, an absent key, and a
// NUMBER-typed reason are none of them a duplicate of a string-keyed spec, so
// the append proceeds.
func TestPostgres_AppendChainedDeduped_ScanIgnoresUnusableKeys(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)
	stageID := makeStageInRun(t, pool, runID)

	// Seed three same-stage, same-category rows through the plain chained
	// append so the scan sees them. The audit_entries payload column is jsonb,
	// so "malformed" here is a valid JSON document that is not an object.
	for _, payload := range []json.RawMessage{
		json.RawMessage(`[1,2,3]`),
		json.RawMessage(`{"salt":"absent-key"}`),
		json.RawMessage(`{"reason":7}`),
	} {
		if _, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
			RunID: runID, StageID: &stageID, Timestamp: time.Now().UTC(), Category: dedupedCat,
			Payload: payload,
		}); err != nil {
			t.Fatalf("seed unusable row %s: %v", payload, err)
		}
	}
	if _, err := appendDeduped(pool, runID, &stageID, "7", dedupedPayload("7")); err != nil {
		t.Fatalf("append over unusable rows: %v, want an append (none of them is a string-keyed duplicate)", err)
	}
	if n := countDedupedRows(t, pool, runID); n != 4 {
		t.Errorf("committed %s rows = %d, want 4", dedupedCat, n)
	}
}

// (e) an unknown run is the not-found error, nil entry, nothing written.
func TestPostgres_AppendChainedDeduped_UnknownRun(t *testing.T) {
	pool := pgtest.NewPool(t)
	runID := uuid.New()
	entry, err := appendDeduped(pool, runID, nil, "persist_failed", dedupedPayload("persist_failed"))
	if err == nil || entry != nil {
		t.Fatalf("(%+v, %v), want (nil, not-found error)", entry, err)
	}
	want := fmt.Sprintf("audit: run %s not found", runID)
	if err.Error() != want {
		t.Errorf("err = %q, want %q", err.Error(), want)
	}
}

// (f) N concurrent appends for the same (run, stage, reason) commit EXACTLY
// ONE row. Workers are t-FREE (errors are collected and reported after
// wg.Wait), the ONLY legal loss is *DedupedDuplicateError, and the committed
// count is read by a direct query. Counterfactual: delete the step-2 scan →
// every worker appends (granted = 8, rows = 8); delete LockRunForUpdate →
// >1 rows (workers scan before a peer commits).
func TestPostgres_AppendChainedDeduped_ConcurrentExactlyOne(t *testing.T) {
	pool := pgtest.NewPool(t)
	runID := makeRun(t, pool)
	stageID := makeStageInRun(t, pool, runID)
	const n = 8

	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	var unexpected []error
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, err := appendDeduped(pool, runID, &stageID, "run_cancelled_before_acceptance", dedupedPayload("run_cancelled_before_acceptance"))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil && e != nil:
				granted++
			case err != nil:
				var dup *audit.DedupedDuplicateError
				if !errors.As(err, &dup) {
					unexpected = append(unexpected, err)
				}
			default:
				unexpected = append(unexpected, errors.New("nil entry with nil error"))
			}
		}()
	}
	wg.Wait()
	for _, err := range unexpected {
		t.Errorf("concurrent deduped append failed for a reason other than the duplicate branch: %v", err)
	}
	if granted != 1 {
		t.Errorf("granted = %d, want exactly 1", granted)
	}
	if rows := countDedupedRows(t, pool, runID); rows != 1 {
		t.Errorf("committed %s rows = %d, want exactly 1", dedupedCat, rows)
	}
}

// TestDedupedErrorMessages pins both Error() branches.
func TestDedupedErrorMessages(t *testing.T) {
	if got, want := (&audit.DedupedDuplicateError{}).Error(), "audit: deduped append duplicate"; got != want {
		t.Errorf("nil Existing: %q, want %q", got, want)
	}
	if got, want := (&audit.DedupedDuplicateError{Existing: &audit.Entry{Sequence: 42}}).Error(),
		"audit: deduped append duplicate (existing entry sequence 42)"; got != want {
		t.Errorf("with Existing: %q, want %q", got, want)
	}
}
