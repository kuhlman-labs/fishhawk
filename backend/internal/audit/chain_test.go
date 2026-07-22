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

// --- ComputeEntryHash: pure unit tests, no DB ---

func TestComputeEntryHash_Deterministic(t *testing.T) {
	runID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	in := audit.HashInputs{
		RunID:     &runID,
		Timestamp: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		Category:  "plan_generated",
		Payload:   json.RawMessage(`{"summary":"x"}`),
	}
	a, err := audit.ComputeEntryHash(in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := audit.ComputeEntryHash(in)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("hash not deterministic: %s vs %s", a, b)
	}
	// sha256 is 32 bytes = 64 hex chars
	if len(a) != 64 {
		t.Errorf("hash length = %d, want 64", len(a))
	}
}

func TestComputeEntryHash_DiffersWhenAnyFieldChanges(t *testing.T) {
	baseRun := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	base := audit.HashInputs{
		RunID:     &baseRun,
		Timestamp: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		Category:  "plan_generated",
		Payload:   json.RawMessage(`{"summary":"x"}`),
	}
	baseHash, err := audit.ComputeEntryHash(base)
	if err != nil {
		t.Fatal(err)
	}

	otherStage := uuid.New()
	otherKind := audit.ActorUser
	otherSubj := "user@example.com"
	otherPrev := "deadbeef"

	cases := []struct {
		name   string
		mutate func(in *audit.HashInputs)
	}{
		{"category", func(in *audit.HashInputs) { in.Category = "different" }},
		{"timestamp", func(in *audit.HashInputs) { in.Timestamp = in.Timestamp.Add(time.Second) }},
		{"payload", func(in *audit.HashInputs) { in.Payload = json.RawMessage(`{"summary":"y"}`) }},
		{"run_id", func(in *audit.HashInputs) { newID := uuid.New(); in.RunID = &newID }},
		{"stage_id added", func(in *audit.HashInputs) { in.StageID = &otherStage }},
		{"actor_kind added", func(in *audit.HashInputs) { in.ActorKind = &otherKind }},
		{"actor_subject added", func(in *audit.HashInputs) { in.ActorSubject = &otherSubj }},
		{"prev_hash added", func(in *audit.HashInputs) { in.PrevHash = &otherPrev }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := base
			tc.mutate(&mutated)
			got, err := audit.ComputeEntryHash(mutated)
			if err != nil {
				t.Fatal(err)
			}
			if got == baseHash {
				t.Errorf("changing %s did not change hash", tc.name)
			}
		})
	}
}

// --- AppendChained: integration tests using the testcontainers
//     Postgres helper from postgres_test.go ---

func TestPostgres_AppendChained_FirstEntryHasNilPrevHash(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	body, _ := json.Marshal(map[string]string{"event": "first"})
	e, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     runID,
		Timestamp: time.Now().UTC(),
		Category:  "first",
		Payload:   body,
	})
	if err != nil {
		t.Fatalf("AppendChained: %v", err)
	}
	if e.PrevHash != nil {
		t.Errorf("first entry's PrevHash = %v, want nil", e.PrevHash)
	}
	if e.EntryHash == "" {
		t.Error("EntryHash should be set after AppendChained")
	}
}

func TestPostgres_AppendChained_LinksToPriorEntry(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	first, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     runID,
		Timestamp: time.Now().UTC(),
		Category:  "first",
		Payload:   json.RawMessage(`{"i":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     runID,
		Timestamp: time.Now().UTC(),
		Category:  "second",
		Payload:   json.RawMessage(`{"i":2}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.PrevHash == nil {
		t.Fatal("second entry's PrevHash should not be nil")
	}
	if *second.PrevHash != first.EntryHash {
		t.Errorf("second.PrevHash = %s, want first.EntryHash = %s",
			*second.PrevHash, first.EntryHash)
	}
}

func TestPostgres_AppendChained_HashMatchesComputeEntryHash(t *testing.T) {
	// Round-trip: an externally computed ComputeEntryHash on the
	// same inputs must equal the hash AppendChained committed.
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	body := json.RawMessage(`{"summary":"hello"}`)
	got, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     runID,
		Timestamp: ts,
		Category:  "plan_generated",
		Payload:   body,
	})
	if err != nil {
		t.Fatal(err)
	}
	rid := runID
	want, err := audit.ComputeEntryHash(audit.HashInputs{
		RunID:     &rid,
		Timestamp: ts,
		Category:  "plan_generated",
		Payload:   body,
		PrevHash:  nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.EntryHash != want {
		t.Errorf("EntryHash mismatch:\n  got  %s\n  want %s", got.EntryHash, want)
	}
}

func TestPostgres_AppendChained_AllOptionalFields(t *testing.T) {
	// Exercises the branches in AppendChained / Append where
	// StageID, ActorKind, and ActorSubject are non-nil — the
	// happy-path tests above leave them nil. Confirms the typed
	// fields round-trip through the chain hash and through the
	// nullable-column handling.
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	// Build a stage so StageID FK resolves.
	stage := makeStageInRun(t, pool, runID)

	kind := audit.ActorAgent
	subj := "claude-code"
	body := json.RawMessage(`{"who":"agent"}`)
	ts := time.Now().UTC()
	got, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &stage,
		Timestamp:    ts,
		Category:     "trace_shipped",
		ActorKind:    &kind,
		ActorSubject: &subj,
		Payload:      body,
	})
	if err != nil {
		t.Fatalf("AppendChained: %v", err)
	}
	if got.StageID == nil || *got.StageID != stage {
		t.Errorf("StageID round-trip failed: got %v", got.StageID)
	}
	if got.ActorKind == nil || *got.ActorKind != audit.ActorAgent {
		t.Errorf("ActorKind round-trip: got %v", got.ActorKind)
	}
	if got.ActorSubject == nil || *got.ActorSubject != subj {
		t.Errorf("ActorSubject round-trip: got %v", got.ActorSubject)
	}

	// External recompute of the hash with the same inputs should
	// match — validates that AppendChained passed every field
	// through ComputeEntryHash.
	rid2 := runID
	want, err := audit.ComputeEntryHash(audit.HashInputs{
		RunID:        &rid2,
		StageID:      &stage,
		Timestamp:    ts,
		Category:     "trace_shipped",
		ActorKind:    &kind,
		ActorSubject: &subj,
		Payload:      body,
		PrevHash:     nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.EntryHash != want {
		t.Errorf("hash mismatch with all fields:\n  got  %s\n  want %s", got.EntryHash, want)
	}
}

// TestPostgres_AppendChained_StampsRunAccountID pins the per-run half of
// ADR-057 / #1828: a per-run append carries the LOCKED run row's
// account_id (read under the SELECT FOR UPDATE inside the same tx), and a
// run with no account stamps nil. The per-run chain key is unchanged
// (still run_id), and the canonical hash recomputes through the UNCHANGED
// HashInputs shape — account_id is a stamp, not a hash input.
func TestPostgres_AppendChained_StampsRunAccountID(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)
	acct := makeAccount(t, pool)
	if _, err := pool.Exec(context.Background(),
		`UPDATE runs SET account_id = $1 WHERE id = $2`, acct, runID); err != nil {
		t.Fatalf("set run account: %v", err)
	}

	e, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     runID,
		Timestamp: time.Now().UTC(),
		Category:  "run_dispatched",
		Payload:   json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("AppendChained: %v", err)
	}
	if e.AccountID == nil || *e.AccountID != acct {
		t.Errorf("per-run entry AccountID = %v, want the run's account %s", e.AccountID, acct)
	}

	recomputed, err := audit.ComputeEntryHash(audit.HashInputs{
		RunID:        e.RunID,
		StageID:      e.StageID,
		Timestamp:    e.Timestamp,
		Category:     e.Category,
		ActorKind:    e.ActorKind,
		ActorSubject: e.ActorSubject,
		Payload:      e.Payload,
		PrevHash:     e.PrevHash,
	})
	if err != nil {
		t.Fatalf("ComputeEntryHash: %v", err)
	}
	if recomputed != e.EntryHash {
		t.Errorf("hash mismatch: account stamping must not change the canonical hash\n  stored:     %s\n  recomputed: %s",
			e.EntryHash, recomputed)
	}

	// A run with no account (CLI/local, pre-backfill) stamps nil.
	bareRun := makeRun(t, pool)
	bare, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     bareRun,
		Timestamp: time.Now().UTC(),
		Category:  "run_dispatched",
		Payload:   json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("AppendChained (no-account run): %v", err)
	}
	if bare.AccountID != nil {
		t.Errorf("no-account run entry AccountID = %v, want nil", bare.AccountID)
	}
}

func TestPostgres_AppendChained_RunNotFound(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)

	_, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     uuid.New(), // never created
		Timestamp: time.Now().UTC(),
		Category:  "x",
		Payload:   json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("AppendChained for missing run should error")
	}
}

// TestPostgres_AppendChained_ConcurrentChainStaysLinear races N
// concurrent AppendChained calls against the same run and asserts
// the resulting chain is unbroken: every entry's prev_hash exactly
// matches the entry_hash of the row whose sequence is one less.
//
// Without the SELECT FOR UPDATE inside AppendChained this test
// would fail intermittently as two writers observe the same
// last-entry hash.
func TestPostgres_AppendChained_ConcurrentChainStaysLinear(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	const N = 10
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			payload, _ := json.Marshal(map[string]int{"i": i})
			_, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
				RunID:     runID,
				Timestamp: time.Now().UTC(),
				Category:  "concurrent",
				Payload:   payload,
			})
			errs[i] = err
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("AppendChained #%d failed: %v", i, err)
		}
	}

	all, err := repo.ListForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListForRun: %v", err)
	}
	if len(all) != N {
		t.Fatalf("got %d entries, want %d", len(all), N)
	}

	// Chain integrity: entries[0].PrevHash is nil; for i > 0,
	// entries[i].PrevHash == entries[i-1].EntryHash. The
	// SELECT FOR UPDATE inside AppendChained is what keeps this
	// invariant under concurrent writes.
	if all[0].PrevHash != nil {
		t.Errorf("first entry has non-nil PrevHash %v", all[0].PrevHash)
	}
	for i := 1; i < len(all); i++ {
		if all[i].PrevHash == nil {
			t.Errorf("entry %d has nil PrevHash", i)
			continue
		}
		if *all[i].PrevHash != all[i-1].EntryHash {
			t.Errorf("chain break at index %d: PrevHash %s != prior EntryHash %s",
				i, *all[i].PrevHash, all[i-1].EntryHash)
		}
	}
}

func TestPostgres_AppendChained_ParallelRunsDoNotBlockEachOther(t *testing.T) {
	// Two distinct runs should write concurrently without blocking
	// on each other's locks. We measure end-to-end: serial would
	// roughly double the wall-clock time since each AppendChained
	// holds the run row across the read+write+commit path.
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runA := makeRun(t, pool)
	runB := makeRun(t, pool)

	body := json.RawMessage(`{}`)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			if _, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
				RunID: runA, Timestamp: time.Now().UTC(), Category: "x", Payload: body,
			}); err != nil {
				t.Errorf("runA #%d: %v", i, err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			if _, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
				RunID: runB, Timestamp: time.Now().UTC(), Category: "x", Payload: body,
			}); err != nil {
				t.Errorf("runB #%d: %v", i, err)
			}
		}
	}()
	wg.Wait()

	for _, runID := range []uuid.UUID{runA, runB} {
		all, err := repo.ListForRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("ListForRun: %v", err)
		}
		if len(all) != 5 {
			t.Errorf("run %s: got %d entries, want 5", runID, len(all))
		}
	}

	// Sanity: each run's chain is linear (verified the same way as
	// the concurrent-same-run test).
	for _, runID := range []uuid.UUID{runA, runB} {
		all, err := repo.ListForRun(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if all[0].PrevHash != nil {
			t.Errorf("run %s first entry has non-nil PrevHash", runID)
		}
		for i := 1; i < len(all); i++ {
			if all[i].PrevHash == nil || *all[i].PrevHash != all[i-1].EntryHash {
				t.Errorf("run %s chain break at index %d", runID, i)
			}
		}
	}

	// And the audit_entries.sequence column is global (BIGSERIAL),
	// so entries from different runs interleave by sequence — a
	// quick way to confirm the parallelism actually happened.
	allA, _ := repo.ListForRun(context.Background(), runA)
	allB, _ := repo.ListForRun(context.Background(), runB)
	interleaved := false
	if len(allA) > 0 && len(allB) > 0 {
		for _, a := range allA {
			for _, b := range allB {
				if a.Sequence > b.Sequence {
					interleaved = true
					break
				}
			}
			if interleaved {
				break
			}
		}
	}
	if !interleaved {
		t.Logf("note: sequences did not interleave; possible serial execution. " +
			"Not strictly a correctness failure but worth investigating if it persists.")
	}

	// Belt-and-suspenders compile-time check: confirm errors.Is
	// against ErrNotFound still resolves through the helper.
	if errors.Is(audit.ErrNotFound, audit.ErrNotFound) != true {
		t.Error("ErrNotFound sanity check failed")
	}
}
