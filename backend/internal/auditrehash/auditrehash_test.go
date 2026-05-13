package auditrehash_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditrehash"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

func startContainer(t *testing.T) *pgxpool.Pool {
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

func isDockerUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if os.Getenv("FISHHAWK_SKIP_INTEGRATION") != "" {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"cannot connect to the docker daemon",
		"docker: not found",
		"executable file not found",
		"dial unix /var/run/docker.sock",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

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
	pool := startContainer(t)
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
	pool := startContainer(t)
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
	pool := startContainer(t)
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
