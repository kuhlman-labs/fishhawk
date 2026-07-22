package auditrehash_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditrehash"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

func makeRun(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	repo := run.NewPostgresRepository(pool)
	r, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return r.ID
}

// insertNonCanonicalEntry simulates a pre-#302 audit row: hashed
// from the in-memory timestamp before any DB round-trip
// normalization. This is what the dispatcher used to write — a hash
// computed against the raw `time.Now().UTC()` value whose nanosecond
// precision and timezone get truncated/shifted on insert.
//
// We compute the "old" hash manually (canonicalizing intentionally
// SKIPPED) so the test rig faithfully reproduces the on-disk state
// the bug created. The new ComputeEntryHash will produce a
// different hash from the round-tripped row → exactly the
// chain_invalid signal #302 reported.
func insertNonCanonicalEntry(
	t *testing.T,
	pool *pgxpool.Pool,
	runID *uuid.UUID,
	stageID *uuid.UUID,
	ts time.Time,
	category string,
	payload []byte,
	prevHash *string,
) (uuid.UUID, string) {
	t.Helper()
	// Old-algorithm hash: marshal HashInputs verbatim without the
	// UTC/Truncate normalization. Mirrors the behavior of the
	// pre-#302 ComputeEntryHash exactly.
	type oldInputs struct {
		RunID        *uuid.UUID       `json:"run_id"`
		StageID      *uuid.UUID       `json:"stage_id"`
		Timestamp    time.Time        `json:"ts"`
		Category     string           `json:"category"`
		ActorKind    *audit.ActorKind `json:"actor_kind"`
		ActorSubject *string          `json:"actor_subject"`
		Payload      json.RawMessage  `json:"payload"`
		PrevHash     *string          `json:"prev_hash"`
	}
	canonical, err := json.Marshal(oldInputs{
		RunID: runID, StageID: stageID, Timestamp: ts,
		Category: category, Payload: payload, PrevHash: prevHash,
	})
	if err != nil {
		t.Fatalf("marshal old inputs: %v", err)
	}
	oldHash := sha256Hex(canonical)
	id := uuid.New()
	_, err = pool.Exec(context.Background(), `
		INSERT INTO audit_entries (id, run_id, stage_id, ts, category, actor_kind, actor_subject, payload, prev_hash, entry_hash)
		VALUES ($1, $2, $3, $4, $5, NULL, NULL, $6, $7, $8)`,
		id, runID, stageID,
		pgtype.Timestamptz{Time: ts, Valid: true},
		category, payload, prevHash, oldHash)
	if err != nil {
		t.Fatalf("insert non-canonical entry: %v", err)
	}
	return id, oldHash
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// verifyChainCanonical recomputes every entry's hash via the new
// (canonical) ComputeEntryHash and compares against the stored
// value. Returns the IDs of any rows whose recomputation diverges.
func verifyChainCanonical(t *testing.T, pool *pgxpool.Pool, runID *uuid.UUID) []uuid.UUID {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT id, run_id, stage_id, ts, category, actor_kind, actor_subject, payload, prev_hash, entry_hash
		FROM audit_entries
		WHERE ($1::uuid IS NULL AND run_id IS NULL) OR run_id = $1
		ORDER BY sequence ASC`, runID)
	if err != nil {
		t.Fatalf("verify select: %v", err)
	}
	defer rows.Close()

	var mismatches []uuid.UUID
	for rows.Next() {
		var (
			id           uuid.UUID
			rid, sid     *uuid.UUID
			ts           time.Time
			category     string
			actorKind    *string
			actorSubject *string
			payload      []byte
			prevHash     *string
			entryHash    string
		)
		if err := rows.Scan(&id, &rid, &sid, &ts, &category, &actorKind, &actorSubject,
			&payload, &prevHash, &entryHash); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var kind *audit.ActorKind
		if actorKind != nil {
			k := audit.ActorKind(*actorKind)
			kind = &k
		}
		got, err := audit.ComputeEntryHash(audit.HashInputs{
			RunID: rid, StageID: sid, Timestamp: ts, Category: category,
			ActorKind: kind, ActorSubject: actorSubject, Payload: payload, PrevHash: prevHash,
		})
		if err != nil {
			t.Fatalf("compute: %v", err)
		}
		if got != entryHash {
			mismatches = append(mismatches, id)
		}
	}
	return mismatches
}

func TestRehashAllChains_RewritesNonCanonicalEntriesAndLinksThemForward(t *testing.T) {
	// Build a chain of three pre-#302 entries that DO NOT verify
	// under the new canonical algorithm. After RehashAllChains the
	// chain must verify end-to-end and prev_hash on entries 2/3 must
	// point at the recomputed predecessor (not the original).
	pool := pgtest.NewPool(t)
	runID := makeRun(t, pool)

	// Nonzero nanoseconds in a non-UTC zone — the exact shape the
	// production dispatcher produces. After insert pgx reads back
	// the value with microsecond precision in the connection's TZ;
	// the canonical algorithm normalizes both, the old one didn't.
	loc := time.FixedZone("EDT", -4*3600)
	ts1 := time.Date(2026, 5, 13, 8, 52, 53, 665435123, loc)
	ts2 := ts1.Add(time.Second)
	ts3 := ts2.Add(time.Second)

	body := func(i int) []byte {
		return []byte(`{"i":` + intStr(i) + `}`)
	}
	id1, h1 := insertNonCanonicalEntry(t, pool, &runID, nil, ts1, "run_dispatched", body(1), nil)
	id2, h2 := insertNonCanonicalEntry(t, pool, &runID, nil, ts2, "trace_uploaded", body(2), &h1)
	id3, h3 := insertNonCanonicalEntry(t, pool, &runID, nil, ts3, "plan_generated", body(3), &h2)

	// Pre-rehash sanity: every entry's stored hash is the old
	// non-canonical form, so the new ComputeEntryHash disagrees on
	// every row. That's the exact #302 signal.
	pre := verifyChainCanonical(t, pool, &runID)
	if len(pre) != 3 {
		t.Fatalf("pre-rehash mismatches = %d, want 3 (every entry uses old algorithm)", len(pre))
	}

	summary, err := auditrehash.RehashAllChains(context.Background(), pool, false)
	if err != nil {
		t.Fatalf("RehashAllChains: %v", err)
	}
	if summary.EntriesChanged != 3 || summary.EntriesTotal != 3 {
		t.Errorf("summary = %+v, want 3 changed / 3 total", summary)
	}
	if summary.Chains != 1 {
		t.Errorf("Chains = %d, want 1", summary.Chains)
	}

	// Post-rehash: every entry's stored hash recomputes correctly
	// under the new algorithm. This is what flips
	// `fishhawk_audit_complete` back to passing.
	post := verifyChainCanonical(t, pool, &runID)
	if len(post) != 0 {
		t.Errorf("post-rehash mismatches = %d, want 0; ids=%v", len(post), post)
	}

	// Linkage: entry 2's prev_hash now points to entry 1's NEW hash;
	// entry 3's prev_hash points to entry 2's NEW hash. Otherwise
	// the chain integrity story is broken even though individual
	// rows match.
	var got2Prev, got3Prev *string
	if err := pool.QueryRow(context.Background(),
		`SELECT prev_hash FROM audit_entries WHERE id = $1`, id2).Scan(&got2Prev); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT prev_hash FROM audit_entries WHERE id = $1`, id3).Scan(&got3Prev); err != nil {
		t.Fatal(err)
	}
	var newH1, newH2 string
	if err := pool.QueryRow(context.Background(),
		`SELECT entry_hash FROM audit_entries WHERE id = $1`, id1).Scan(&newH1); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT entry_hash FROM audit_entries WHERE id = $1`, id2).Scan(&newH2); err != nil {
		t.Fatal(err)
	}
	if got2Prev == nil || *got2Prev != newH1 {
		t.Errorf("entry 2 prev_hash = %v, want entry 1's new hash %q", got2Prev, newH1)
	}
	if got3Prev == nil || *got3Prev != newH2 {
		t.Errorf("entry 3 prev_hash = %v, want entry 2's new hash %q", got3Prev, newH2)
	}
	if newH1 == h1 || newH2 == h2 {
		t.Errorf("expected new hashes to differ from old (h1=%q→%q, h2=%q→%q)",
			h1, newH1, h2, newH2)
	}
	_ = h3 // silence unused; h3 is exercised via verifyChainCanonical
}

func TestRehashAllChains_IsIdempotent(t *testing.T) {
	// Once a chain is canonical, a re-run reports zero changes and
	// commits nothing. Idempotency is what makes the migration safe
	// to re-run on a partially-completed batch.
	pool := pgtest.NewPool(t)
	runID := makeRun(t, pool)

	// Write entries via the production AppendChained path — those
	// already use the new algorithm and are canonical by definition.
	repo := audit.NewPostgresRepository(pool)
	for i := 0; i < 3; i++ {
		if _, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
			RunID:     runID,
			Timestamp: time.Now().UTC(),
			Category:  "x",
			Payload:   []byte(`{}`),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	summary, err := auditrehash.RehashAllChains(context.Background(), pool, false)
	if err != nil {
		t.Fatalf("RehashAllChains: %v", err)
	}
	if summary.EntriesTotal != 3 {
		t.Errorf("EntriesTotal = %d, want 3", summary.EntriesTotal)
	}
	if summary.EntriesChanged != 0 {
		t.Errorf("EntriesChanged = %d, want 0 (canonical chain shouldn't move)", summary.EntriesChanged)
	}
}

func TestRehashAllChains_DryRunReportsButDoesNotWrite(t *testing.T) {
	pool := pgtest.NewPool(t)
	runID := makeRun(t, pool)

	loc := time.FixedZone("EDT", -4*3600)
	ts := time.Date(2026, 5, 13, 8, 52, 53, 665435123, loc)
	id, oldHash := insertNonCanonicalEntry(t, pool, &runID, nil, ts, "x", []byte(`{}`), nil)

	summary, err := auditrehash.RehashAllChains(context.Background(), pool, true)
	if err != nil {
		t.Fatalf("RehashAllChains(dry-run): %v", err)
	}
	if summary.EntriesChanged != 1 {
		t.Errorf("EntriesChanged = %d, want 1 (would change)", summary.EntriesChanged)
	}

	// Verify the row was NOT touched.
	var stillStored string
	if err := pool.QueryRow(context.Background(),
		`SELECT entry_hash FROM audit_entries WHERE id = $1`, id).Scan(&stillStored); err != nil {
		t.Fatal(err)
	}
	if stillStored != oldHash {
		t.Errorf("dry-run mutated the row: stored %q, want unchanged %q", stillStored, oldHash)
	}
}

// makeAccount inserts a tenant workspace account so run-less audit
// entries can reference it (audit_entries.account_id FK, migration
// 0055).
func makeAccount(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO accounts (id, account_key) VALUES ($1, $2)`,
		id, "acct-"+id.String()[:8]); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	return id
}

// insertRunlessEntry inserts one run-less row with a CANONICAL hash
// computed over the given prev — the shape a pre-ADR-057 single
// interleaved global chain left on disk once account_id was
// backfilled: hashes verify row-by-row, but prev_hash links across
// account partitions. Returns the row id and its entry_hash.
func insertRunlessEntry(
	t *testing.T,
	pool *pgxpool.Pool,
	accountID *uuid.UUID,
	ts time.Time,
	category string,
	payload []byte,
	prevHash *string,
) (uuid.UUID, string) {
	t.Helper()
	hash, err := audit.ComputeEntryHash(audit.HashInputs{
		Timestamp: ts,
		Category:  category,
		Payload:   payload,
		PrevHash:  prevHash,
	})
	if err != nil {
		t.Fatalf("compute hash: %v", err)
	}
	id := uuid.New()
	_, err = pool.Exec(context.Background(), `
		INSERT INTO audit_entries (id, run_id, stage_id, ts, category, actor_kind, actor_subject, payload, prev_hash, entry_hash, account_id)
		VALUES ($1, NULL, NULL, $2, $3, NULL, NULL, $4, $5, $6, $7)`,
		id, pgtype.Timestamptz{Time: ts, Valid: true},
		category, payload, prevHash, hash, accountID)
	if err != nil {
		t.Fatalf("insert run-less entry: %v", err)
	}
	return id, hash
}

// verifyRunlessPartition walks ONE run-less account partition (nil =
// untenanted) in sequence order and fails the test unless it is an
// independent canonical chain: nil-prev_hash genesis, every
// prev_hash linking the in-partition predecessor's entry_hash, every
// entry_hash recomputing canonically. Returns the partition size.
func verifyRunlessPartition(t *testing.T, pool *pgxpool.Pool, accountID *uuid.UUID) int {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT ts, category, payload, prev_hash, entry_hash
		FROM audit_entries
		WHERE run_id IS NULL AND (($1::uuid IS NULL AND account_id IS NULL) OR account_id = $1)
		ORDER BY sequence ASC`, accountID)
	if err != nil {
		t.Fatalf("select partition: %v", err)
	}
	defer rows.Close()

	n := 0
	var prevEntryHash *string
	for rows.Next() {
		var (
			ts        time.Time
			category  string
			payload   []byte
			prevHash  *string
			entryHash string
		)
		if err := rows.Scan(&ts, &category, &payload, &prevHash, &entryHash); err != nil {
			t.Fatalf("scan partition row: %v", err)
		}
		if prevEntryHash == nil {
			if prevHash != nil {
				t.Errorf("partition %v entry %d: genesis prev_hash = %q, want nil", accountID, n, *prevHash)
			}
		} else if prevHash == nil || *prevHash != *prevEntryHash {
			t.Errorf("partition %v entry %d: prev_hash does not link the in-partition predecessor", accountID, n)
		}
		got, err := audit.ComputeEntryHash(audit.HashInputs{
			Timestamp: ts, Category: category, Payload: payload, PrevHash: prevHash,
		})
		if err != nil {
			t.Fatalf("recompute: %v", err)
		}
		if got != entryHash {
			t.Errorf("partition %v entry %d: entry_hash not canonical", accountID, n)
		}
		h := entryHash
		prevEntryHash = &h
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate partition: %v", err)
	}
	return n
}

func TestRehashAllChains_SegmentsRunLessChainPerAccount(t *testing.T) {
	// The legacy corpus shape after the account backfill and before
	// the re-anchor: ONE interleaved run-less chain whose per-row
	// hashes are canonical but whose prev_hash links cross account
	// partitions. The re-anchor must segment it into one independent
	// chain per account (plus the untenanted NULL partition), each
	// with its own nil-prev_hash genesis.
	pool := pgtest.NewPool(t)
	acctA, acctB := makeAccount(t, pool), makeAccount(t, pool)

	base := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	// Interleave A, B, untenanted, A, B as one single chain.
	_, h1 := insertRunlessEntry(t, pool, &acctA, base, "api_token_issued", []byte(`{"i":1}`), nil)
	_, h2 := insertRunlessEntry(t, pool, &acctB, base.Add(time.Second), "api_token_issued", []byte(`{"i":2}`), &h1)
	_, h3 := insertRunlessEntry(t, pool, nil, base.Add(2*time.Second), "oauth_signin", []byte(`{"i":3}`), &h2)
	_, h4 := insertRunlessEntry(t, pool, &acctA, base.Add(3*time.Second), "api_token_revoked", []byte(`{"i":4}`), &h3)
	_, _ = insertRunlessEntry(t, pool, &acctB, base.Add(4*time.Second), "api_token_revoked", []byte(`{"i":5}`), &h4)

	summary, err := auditrehash.RehashAllChains(context.Background(), pool, false)
	if err != nil {
		t.Fatalf("RehashAllChains: %v", err)
	}
	// Three partitions (A, B, untenanted), five rows. Entry 1 is
	// already A's genesis and stays put; the other four re-anchor.
	if summary.Chains != 3 {
		t.Errorf("Chains = %d, want 3 (A, B, untenanted)", summary.Chains)
	}
	if summary.EntriesTotal != 5 {
		t.Errorf("EntriesTotal = %d, want 5", summary.EntriesTotal)
	}
	if summary.EntriesChanged != 4 {
		t.Errorf("EntriesChanged = %d, want 4 (A's genesis already anchored)", summary.EntriesChanged)
	}

	if n := verifyRunlessPartition(t, pool, &acctA); n != 2 {
		t.Errorf("partition A size = %d, want 2", n)
	}
	if n := verifyRunlessPartition(t, pool, &acctB); n != 2 {
		t.Errorf("partition B size = %d, want 2", n)
	}
	if n := verifyRunlessPartition(t, pool, nil); n != 1 {
		t.Errorf("untenanted partition size = %d, want 1", n)
	}

	// Idempotent: a second pass over the segmented corpus changes
	// nothing.
	again, err := auditrehash.RehashAllChains(context.Background(), pool, false)
	if err != nil {
		t.Fatalf("RehashAllChains (second pass): %v", err)
	}
	if again.EntriesChanged != 0 {
		t.Errorf("second pass EntriesChanged = %d, want 0", again.EntriesChanged)
	}
	if again.Chains != 3 || again.EntriesTotal != 5 {
		t.Errorf("second pass summary = %+v, want 3 chains / 5 total", again)
	}
}

// failAfterDB injects a write failure after a fixed number of Execs
// so the abort path (deferred rollback) is exercised against the
// real database: the append-only triggers must come back and no row
// may have been mutated.
type failAfterDB struct {
	pool      *pgxpool.Pool
	execsLeft int
}

func (f *failAfterDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return f.pool.Query(ctx, sql, args...)
}

func (f *failAfterDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &failAfterTx{Tx: tx, execsLeft: &f.execsLeft}, nil
}

type failAfterTx struct {
	pgx.Tx
	execsLeft *int
}

func (t *failAfterTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if *t.execsLeft <= 0 {
		return pgconn.CommandTag{}, errors.New("injected write failure")
	}
	*t.execsLeft--
	return t.Tx.Exec(ctx, sql, args...)
}

func TestRehashAllChains_InjectedFailureRestoresTriggersAndWritesNothing(t *testing.T) {
	pool := pgtest.NewPool(t)

	// THREE non-canonical run-less rows in the same (untenanted)
	// partition, so the walk attempts three UPDATEs in sequence. A
	// single-row corpus cannot test rollback-after-partial-mutation:
	// if we fail on the first UPDATE, no write ever lands, so the
	// unchanged-row assertion would still pass even if a committed
	// earlier update escaped rollback. Here we let the FIRST UPDATE
	// succeed inside the tx and fail on the SECOND, then assert every
	// row — including the mutated-then-rolled-back first — retains its
	// original hash.
	loc := time.FixedZone("EDT", -4*3600)
	ts1 := time.Date(2026, 5, 13, 8, 52, 53, 665435123, loc)
	ts2 := ts1.Add(time.Second)
	ts3 := ts2.Add(time.Second)
	id1, oldHash1 := insertNonCanonicalEntry(t, pool, nil, nil, ts1, "api_token_issued", []byte(`{}`), nil)
	id2, oldHash2 := insertNonCanonicalEntry(t, pool, nil, nil, ts2, "api_token_issued", []byte(`{}`), &oldHash1)
	id3, oldHash3 := insertNonCanonicalEntry(t, pool, nil, nil, ts3, "api_token_revoked", []byte(`{}`), &oldHash2)

	// Allow two Execs — the trigger disable and the first row's UPDATE
	// — then fail on the second row's UPDATE. This drives a genuine
	// partial mutation: row 1 is rewritten inside the tx before the
	// abort.
	db := &failAfterDB{pool: pool, execsLeft: 2}
	if _, err := auditrehash.RehashAllChains(context.Background(), db, false); err == nil {
		t.Fatal("RehashAllChains succeeded, want injected failure")
	}

	// The abort rolled back the trigger disable: audit_entries is
	// still append-only.
	if _, err := pool.Exec(context.Background(),
		`UPDATE audit_entries SET category = category WHERE id = $1`, id1); err == nil {
		t.Error("UPDATE succeeded after failed rehash — append-only trigger not restored")
	}

	// And NO row was left mutated — including id1, whose UPDATE landed
	// inside the tx before the injected failure. If the earlier update
	// escaped rollback, id1's stored hash would be the recomputed
	// canonical value, not oldHash1.
	for _, want := range []struct {
		id   uuid.UUID
		hash string
	}{{id1, oldHash1}, {id2, oldHash2}, {id3, oldHash3}} {
		var stored string
		if err := pool.QueryRow(context.Background(),
			`SELECT entry_hash FROM audit_entries WHERE id = $1`, want.id).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != want.hash {
			t.Errorf("failed rehash mutated row %s: stored %q, want unchanged %q", want.id, stored, want.hash)
		}
	}
}

// --- helpers that would otherwise pull in their own imports ---

func intStr(i int) string {
	switch i {
	case 1:
		return "1"
	case 2:
		return "2"
	case 3:
		return "3"
	}
	return "0"
}
