package audit_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// makeRun creates a parent run that the audit-entry tests can attach
// to (audit_entries has a non-nullable run_id FK).
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

// makeStageInRun adds a stage under an existing run so audit
// entries can carry a non-nil StageID.
func makeStageInRun(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) uuid.UUID {
	t.Helper()
	repo := run.NewPostgresRepository(pool)
	s, err := repo.CreateStage(context.Background(), run.CreateStageParams{
		RunID:        runID,
		Sequence:     0,
		Type:         run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent,
		ExecutorRef:  "claude-code",
	})
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}
	return s.ID
}

// makeAccount inserts a tenant workspace account so audit entries can
// carry a resolvable account_id FK (ADR-057 / #1828).
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

func entryHash(seq int64, payload []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(time.Now().Format(time.RFC3339Nano)))
	_, _ = h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}

func appendEntry(t *testing.T, repo audit.Repository, runID uuid.UUID, category string, prev *string) *audit.Entry {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"event": category})
	rid := runID
	e, err := repo.Append(context.Background(), audit.AppendParams{
		RunID:     &rid,
		Timestamp: time.Now().UTC(),
		Category:  category,
		Payload:   body,
		PrevHash:  prev,
		EntryHash: entryHash(0, body),
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return e
}

func TestPostgres_AppendAndGet(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	first := appendEntry(t, repo, runID, "plan_generated", nil)
	if first.Sequence == 0 {
		t.Errorf("Sequence = 0, want positive bigserial value")
	}
	if first.Category != "plan_generated" {
		t.Errorf("Category = %q", first.Category)
	}

	got, err := repo.Get(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Sequence != first.Sequence {
		t.Errorf("Sequence mismatch: %d vs %d", got.Sequence, first.Sequence)
	}
}

func TestPostgres_Get_NotFound(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)

	_, err := repo.Get(context.Background(), uuid.New())
	if !errors.Is(err, audit.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestPostgres_ListForRun_OrderedBySequence(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	prev := (*string)(nil)
	for i := 0; i < 5; i++ {
		e := appendEntry(t, repo, runID, "x", prev)
		eh := e.EntryHash
		prev = &eh
	}

	got, err := repo.ListForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListForRun: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("len = %d, want 5", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Sequence <= got[i-1].Sequence {
			t.Errorf("non-monotonic at %d: %d <= %d", i, got[i].Sequence, got[i-1].Sequence)
		}
	}
}

func TestPostgres_LastForRun(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	// Empty run → ErrNotFound.
	if _, err := repo.LastForRun(context.Background(), runID); !errors.Is(err, audit.ErrNotFound) {
		t.Fatalf("LastForRun on empty run: err = %v, want ErrNotFound", err)
	}

	a := appendEntry(t, repo, runID, "first", nil)
	b := appendEntry(t, repo, runID, "second", &a.EntryHash)

	last, err := repo.LastForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LastForRun: %v", err)
	}
	if last.ID != b.ID {
		t.Errorf("LastForRun returned %s, want last (%s)", last.ID, b.ID)
	}
}

func TestPostgres_ListForRunByCategory(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	appendEntry(t, repo, runID, "plan_generated", nil)
	appendEntry(t, repo, runID, "gate_passed", nil)
	appendEntry(t, repo, runID, "plan_generated", nil)
	appendEntry(t, repo, runID, "failure", nil)

	got, err := repo.ListForRunByCategory(context.Background(), runID, "plan_generated")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
	for _, e := range got {
		if e.Category != "plan_generated" {
			t.Errorf("Category = %q, want plan_generated", e.Category)
		}
	}
}

// TestPostgres_AppendWithActor exercises the ActorKind / ActorSubject
// fields that the simpler appendEntry helper leaves nil. Confirms
// they round-trip through the column NULL handling cleanly.
func TestPostgres_AppendWithActor(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	body, _ := json.Marshal(map[string]string{"who": "approved"})
	subj := "user@example.com"
	kind := audit.ActorUser
	rid := runID
	e, err := repo.Append(context.Background(), audit.AppendParams{
		RunID:        &rid,
		Timestamp:    time.Now().UTC(),
		Category:     "approval",
		ActorKind:    &kind,
		ActorSubject: &subj,
		Payload:      body,
		EntryHash:    entryHash(0, body),
	})
	if err != nil {
		t.Fatalf("Append with actor: %v", err)
	}

	got, err := repo.Get(context.Background(), e.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ActorKind == nil || *got.ActorKind != audit.ActorUser {
		t.Errorf("ActorKind = %v, want ActorUser", got.ActorKind)
	}
	if got.ActorSubject == nil || *got.ActorSubject != subj {
		t.Errorf("ActorSubject = %v, want %q", got.ActorSubject, subj)
	}
}

// TestPostgres_TriggerBlocksUpdate is the load-bearing assertion for
// audit_entries' append-only invariant. The Repository interface
// doesn't expose Update; this test goes around the API directly to
// the database to confirm the trigger fires regardless.
func TestPostgres_TriggerBlocksUpdate(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)
	e := appendEntry(t, repo, runID, "x", nil)

	_, err := pool.Exec(context.Background(),
		`UPDATE audit_entries SET category = 'tampered' WHERE id = $1`, e.ID)
	if err == nil {
		t.Fatal("UPDATE on audit_entries should be blocked by the trigger")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("trigger error = %v, want 'append-only' substring", err)
	}
}

// TestPostgres_TriggerBlocksDelete pairs with the UPDATE test —
// neither mutation is permitted on the audit log.
func TestPostgres_TriggerBlocksDelete(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)
	e := appendEntry(t, repo, runID, "x", nil)

	_, err := pool.Exec(context.Background(),
		`DELETE FROM audit_entries WHERE id = $1`, e.ID)
	if err == nil {
		t.Fatal("DELETE on audit_entries should be blocked by the trigger")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("trigger error = %v, want 'append-only' substring", err)
	}
}

// --- Global chain tests (E2.7) ---

// TestPostgres_AppendChained_HashRoundTripsThroughDB is the
// regression test for #302: ComputeEntryHash must produce the same
// digest from the in-memory timestamp passed to AppendChained AND
// from the timestamp read back off the row. Before #302 the write
// hashed a nanosecond-precision UTC `time.Now()` value, but the
// stored row was microsecond-precision and pgx read it back in the
// connection's timezone — both sides of the difference broke the
// round-trip, so verifyChain in auditcomplete always reported
// chain_invalid on production runs.
//
// The fix normalizes the timestamp inside ComputeEntryHash
// (UTC, microsecond-truncated). This test exercises the full
// integration boundary the in-memory fakes don't reach.
func TestPostgres_AppendChained_HashRoundTripsThroughDB(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	// Use time.Now().UTC() — the same value the dispatcher passes
	// in production. Carries nanosecond precision (Go default) which
	// Postgres truncates to microsecond on INSERT.
	now := time.Now().UTC()
	subj := "github-webhook"
	kind := audit.ActorSystem
	e, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:        runID,
		Timestamp:    now,
		Category:     "run_dispatched",
		ActorKind:    &kind,
		ActorSubject: &subj,
		Payload:      json.RawMessage(`{"outcome":"dispatched"}`),
	})
	if err != nil {
		t.Fatalf("AppendChained: %v", err)
	}

	// Recompute the hash from the read-back row — that's what
	// auditcomplete.verifyChain does in production.
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
		t.Fatalf("hash mismatch after DB round-trip:\n  stored:     %s\n  recomputed: %s\n\n"+
			"This is the bug from #302 — write-time hashed in-memory time, read-back hashed truncated/TZ-shifted time.",
			e.EntryHash, recomputed)
	}
}

// TestPostgres_AppendChained_HashRoundTripsWithMultiKeyPayload is
// the regression test for #308: ComputeEntryHash must also produce
// the same digest when the payload has multiple keys. The earlier
// #302 round-trip test happened to use a single-key payload
// (`{"outcome":"dispatched"}`) where PG's JSONB re-serialization is
// a no-op vs Go's `json.Marshal` output, so the deeper byte-
// instability slipped through. Multi-key payloads trip PG's
// internal-order-plus-whitespace serialization on read; the fix is
// to canonicalize the payload inside ComputeEntryHash so both sides
// converge on the same bytes.
func TestPostgres_AppendChained_HashRoundTripsWithMultiKeyPayload(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	// Match the shape the dispatcher's writeDispatchAudit produces —
	// a 9-key payload that PG's JSONB will definitely re-order.
	payload, _ := json.Marshal(map[string]any{
		"event":          "issue_comment",
		"delivery_id":    "deadbeef-cafe-babe-feed-facefacefeed",
		"action":         "created",
		"sender":         "kuhlman-labs",
		"workflow_id":    "feature_change",
		"workflow_sha":   "1234567890abcdef1234567890abcdef12345678",
		"trigger_ref":    "issue:42",
		"trigger_source": "github_issue",
		"outcome":        "dispatched",
	})

	subj := "github-webhook"
	kind := audit.ActorSystem
	e, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:        runID,
		Timestamp:    time.Now().UTC(),
		Category:     "run_dispatched",
		ActorKind:    &kind,
		ActorSubject: &subj,
		Payload:      payload,
	})
	if err != nil {
		t.Fatalf("AppendChained: %v", err)
	}

	// pgx-read bytes WILL differ from the write bytes (PG's JSONB
	// re-serialization), so assert that up front — it's the exact
	// shape of the #308 bug and we want a clear failure mode if PG
	// ever changes this behaviour.
	if string(e.Payload) == string(payload) {
		t.Logf("PG returned the payload bytes unchanged — JSONB serialization changed; rest of the test still asserts hash stability")
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
		t.Fatalf("hash mismatch after DB round-trip with multi-key payload:\n  stored:     %s\n  recomputed: %s\n  write bytes (%d): %s\n  read bytes  (%d): %s\n\n"+
			"This is the bug from #308 — JSONB payload doesn't round-trip byte-equal.",
			e.EntryHash, recomputed,
			len(payload), payload,
			len(e.Payload), e.Payload)
	}
}

// TestComputeEntryHash_NormalizesTimestamp is the unit-test
// counterpart to the round-trip integration test above (#302). The
// same logical moment expressed as `time.Now()`, `time.Now().UTC()`,
// and the read-back-from-DB shape (truncated to microseconds, in
// a local timezone) MUST all hash to the same value — that's what
// makes the chain stable across the write/read boundary.
func TestComputeEntryHash_NormalizesTimestamp(t *testing.T) {
	runID := uuid.New()
	payload := json.RawMessage(`{"x":1}`)

	// Pick a moment with nonzero nanoseconds in a non-UTC timezone
	// so the normalization actually has something to do.
	loc := time.FixedZone("EDT", -4*3600)
	base := time.Date(2026, 5, 13, 8, 52, 53, 665435123, loc) // 123 ns past microsecond

	// Variants that all refer to the same logical moment but
	// differ in their in-memory time.Time representation.
	variants := []time.Time{
		base,                                    // local TZ, nano precision
		base.UTC(),                              // UTC, nano precision (dispatcher's typical input)
		base.UTC().Truncate(time.Microsecond),   // UTC, micro (post-DB-roundtrip, UTC connection)
		base.In(loc).Truncate(time.Microsecond), // local TZ, micro (post-DB-roundtrip, local connection)
	}

	hashes := make(map[string]string, len(variants))
	for i, v := range variants {
		h, err := audit.ComputeEntryHash(audit.HashInputs{
			RunID: &runID, Timestamp: v, Category: "x", Payload: payload,
		})
		if err != nil {
			t.Fatalf("variant %d: %v", i, err)
		}
		hashes[v.Format(time.RFC3339Nano)] = h
	}

	// Every variant must produce the same hash. A regression here
	// is the same bug #302 reported.
	var first string
	for k, h := range hashes {
		if first == "" {
			first = h
			continue
		}
		if h != first {
			t.Errorf("hash divergence for variant %q: got %s, want %s", k, h, first)
		}
	}
}

// TestComputeEntryHash_CanonicalizesPayload is the unit-test
// counterpart to the integration test below (#308). The
// audit_entries.payload column is JSONB, which doesn't preserve key
// order or whitespace — the dispatcher's `json.Marshal` produces
// alphabetically-sorted compact bytes, but pgx reads back the
// JSONB-emitted form (PG's internal order + spaces after colons).
// ComputeEntryHash must produce the same digest for every
// representation of the same semantic JSON, otherwise verifyChain
// fails on every entry with a multi-key payload.
func TestComputeEntryHash_CanonicalizesPayload(t *testing.T) {
	runID := uuid.New()
	ts := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)

	// Five forms of the same logical payload: differ in key order,
	// whitespace, and number representation. All must hash to the
	// same value after the new canonicalization.
	variants := map[string]json.RawMessage{
		"alphabetical-compact":         json.RawMessage(`{"a":1,"b":"x","c":true}`),
		"alphabetical-with-spaces":     json.RawMessage(`{"a": 1, "b": "x", "c": true}`),
		"reverse-order-compact":        json.RawMessage(`{"c":true,"b":"x","a":1}`),
		"reverse-order-with-spaces":    json.RawMessage(`{"c": true, "b": "x", "a": 1}`),
		"jsonb-style-mixed-whitespace": json.RawMessage(`{ "b":"x", "a":1, "c":true }`),
	}

	hashes := map[string]string{}
	for name, p := range variants {
		h, err := audit.ComputeEntryHash(audit.HashInputs{
			RunID: &runID, Timestamp: ts, Category: "x", Payload: p,
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		hashes[name] = h
	}

	var first string
	for name, h := range hashes {
		if first == "" {
			first = h
			continue
		}
		if h != first {
			t.Errorf("hash divergence for %q: got %s, want %s", name, h, first)
		}
	}
}

// TestComputeEntryHash_PayloadPreservesIntPrecision asserts that the
// payload canonicalization doesn't collapse JSON integers to
// float64. Without `dec.UseNumber()` in the canonicalizer, a payload
// like `{"pr_number":9999999999999999}` would parse to a float and
// re-marshal with precision loss — hash diverges across re-runs of
// the same input.
func TestComputeEntryHash_PayloadPreservesIntPrecision(t *testing.T) {
	runID := uuid.New()
	ts := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	payload := json.RawMessage(`{"pr_number":9999999999999999,"retry_attempt":3}`)

	h1, err := audit.ComputeEntryHash(audit.HashInputs{
		RunID: &runID, Timestamp: ts, Category: "x", Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	h2, err := audit.ComputeEntryHash(audit.HashInputs{
		RunID: &runID, Timestamp: ts, Category: "x", Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Errorf("hash not deterministic across recomputes: %s vs %s", h1, h2)
	}
	// 9999999999999999 is past float64's safe integer range. If the
	// canonicalizer collapsed the value, the re-marshaled bytes
	// would carry "1e+16" instead of the original; hashing the
	// reconstructed payload would still be deterministic but would
	// silently mutate semantic content. We re-marshal a json.Number
	// path explicitly to assert the value is preserved verbatim.
	if !bytes.Contains(payload, []byte("9999999999999999")) {
		t.Fatalf("test payload missing canary integer: %s", payload)
	}
}

func TestPostgres_AppendGlobalChained_FirstEntryHasNilPrevHash(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)

	subj := "github:42"
	kind := audit.ActorUser
	body, _ := json.Marshal(map[string]string{"event": "first"})
	e, err := repo.AppendGlobalChained(context.Background(), audit.GlobalChainAppendParams{
		Timestamp:    time.Now().UTC(),
		Category:     "api_token_issued",
		ActorKind:    &kind,
		ActorSubject: &subj,
		Payload:      body,
	})
	if err != nil {
		t.Fatalf("AppendGlobalChained: %v", err)
	}
	if e.RunID != nil {
		t.Errorf("global entry RunID = %v, want nil", e.RunID)
	}
	if e.StageID != nil {
		t.Errorf("global entry StageID = %v, want nil", e.StageID)
	}
	if e.PrevHash != nil {
		t.Errorf("first global entry PrevHash = %v, want nil", e.PrevHash)
	}
	if e.EntryHash == "" {
		t.Error("EntryHash should be set")
	}
}

func TestPostgres_AppendGlobalChained_LinksToPriorEntry(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)

	first, err := repo.AppendGlobalChained(context.Background(), audit.GlobalChainAppendParams{
		Timestamp: time.Now().UTC(),
		Category:  "api_token_issued",
		Payload:   json.RawMessage(`{"i":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := repo.AppendGlobalChained(context.Background(), audit.GlobalChainAppendParams{
		Timestamp: time.Now().UTC(),
		Category:  "api_token_revoked",
		Payload:   json.RawMessage(`{"i":2}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.PrevHash == nil || *second.PrevHash != first.EntryHash {
		t.Errorf("second.PrevHash = %v, want first.EntryHash %q",
			second.PrevHash, first.EntryHash)
	}
}

func TestPostgres_GlobalAndPerRunChainsAreIndependent(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	// Append one per-run entry.
	runEntry, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     runID,
		Timestamp: time.Now().UTC(),
		Category:  "trace_uploaded",
		Payload:   json.RawMessage(`{"i":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Append one global entry; its PrevHash must NOT be the
	// per-run entry's hash — the chains are independent.
	globalEntry, err := repo.AppendGlobalChained(context.Background(), audit.GlobalChainAppendParams{
		Timestamp: time.Now().UTC(),
		Category:  "api_token_issued",
		Payload:   json.RawMessage(`{"i":2}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if globalEntry.PrevHash != nil {
		t.Errorf("first global entry PrevHash = %v, want nil (independent of per-run chain)", globalEntry.PrevHash)
	}
	// Per-run chain unaffected.
	runLast, err := repo.LastForRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if runLast.ID != runEntry.ID {
		t.Errorf("LastForRun returned %s, want %s (global append shouldn't affect run chain)", runLast.ID, runEntry.ID)
	}
}

func TestPostgres_ListGlobal_ReturnsOnlyGlobalEntries(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	// Two global + one per-run.
	_, err := repo.AppendGlobalChained(context.Background(), audit.GlobalChainAppendParams{
		Timestamp: time.Now().UTC(),
		Category:  "api_token_issued",
		Payload:   json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.AppendGlobalChained(context.Background(), audit.GlobalChainAppendParams{
		Timestamp: time.Now().UTC(),
		Category:  "api_token_revoked",
		Payload:   json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     runID,
		Timestamp: time.Now().UTC(),
		Category:  "trace_uploaded",
		Payload:   json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := repo.ListGlobal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("ListGlobal returned %d entries, want 2 (per-run rows must be filtered out)", len(got))
	}
	for _, e := range got {
		if e.RunID != nil {
			t.Errorf("ListGlobal returned a row with RunID = %v, want nil", e.RunID)
		}
	}
}

// --- Per-account run-less chain tests (ADR-057 / #1828) ---

// appendGlobal is the per-account shorthand for AppendGlobalChained.
func appendGlobal(t *testing.T, repo audit.Repository, accountID *uuid.UUID, category string) *audit.Entry {
	t.Helper()
	e, err := repo.AppendGlobalChained(context.Background(), audit.GlobalChainAppendParams{
		Timestamp: time.Now().UTC(),
		Category:  category,
		Payload:   json.RawMessage(`{}`),
		AccountID: accountID,
	})
	if err != nil {
		t.Fatalf("AppendGlobalChained(%v): %v", accountID, err)
	}
	return e
}

// TestPostgres_AppendGlobalChained_PerAccountChainSeparation is the core
// #1828 assertion: interleaved run-less appends for accounts A and B each
// chain WITHIN their account — A's second entry links to A's first (not to
// B's, which was appended in between), and B is its own nil-prev_hash
// genesis. Also pins that account_id round-trips on the Entry and stays
// OUT of the canonical hash (the unchanged HashInputs recompute matches).
func TestPostgres_AppendGlobalChained_PerAccountChainSeparation(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	acctA, acctB := makeAccount(t, pool), makeAccount(t, pool)

	a1 := appendGlobal(t, repo, &acctA, "api_token_issued")
	b1 := appendGlobal(t, repo, &acctB, "api_token_issued")
	a2 := appendGlobal(t, repo, &acctA, "api_token_revoked")

	if a1.PrevHash != nil {
		t.Errorf("A genesis PrevHash = %v, want nil", a1.PrevHash)
	}
	if b1.PrevHash != nil {
		t.Errorf("B genesis PrevHash = %v, want nil (own partition, not chained to A)", b1.PrevHash)
	}
	if a2.PrevHash == nil || *a2.PrevHash != a1.EntryHash {
		t.Errorf("A second entry PrevHash = %v, want A's first EntryHash %q (NOT B's %q)",
			a2.PrevHash, a1.EntryHash, b1.EntryHash)
	}
	if a2.PrevHash != nil && *a2.PrevHash == b1.EntryHash {
		t.Error("A second entry chained to B's entry — partitions leaked")
	}
	if a1.AccountID == nil || *a1.AccountID != acctA {
		t.Errorf("A entry AccountID = %v, want %s", a1.AccountID, acctA)
	}
	if b1.AccountID == nil || *b1.AccountID != acctB {
		t.Errorf("B entry AccountID = %v, want %s", b1.AccountID, acctB)
	}

	// Frozen-HashInputs pin: recomputing through the UNCHANGED canonical
	// shape (no account field) must reproduce the stored hash — fails if
	// account_id ever leaks into the hash.
	recomputed, err := audit.ComputeEntryHash(audit.HashInputs{
		RunID:        a2.RunID,
		StageID:      a2.StageID,
		Timestamp:    a2.Timestamp,
		Category:     a2.Category,
		ActorKind:    a2.ActorKind,
		ActorSubject: a2.ActorSubject,
		Payload:      a2.Payload,
		PrevHash:     a2.PrevHash,
	})
	if err != nil {
		t.Fatalf("ComputeEntryHash: %v", err)
	}
	if recomputed != a2.EntryHash {
		t.Errorf("hash mismatch: account_id must not enter the canonical hash\n  stored:     %s\n  recomputed: %s",
			a2.EntryHash, recomputed)
	}
}

// TestPostgres_AppendGlobalChained_UntenantedPartitionIndependent pins the
// nil-AccountID fallback: untenanted appends chain within the account_id
// IS NULL partition (#1829 NULL-allow window) and are unaffected by
// tenanted appends interleaved between them.
func TestPostgres_AppendGlobalChained_UntenantedPartitionIndependent(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	acct := makeAccount(t, pool)

	u1 := appendGlobal(t, repo, nil, "api_token_issued")
	tenanted := appendGlobal(t, repo, &acct, "api_token_issued")
	u2 := appendGlobal(t, repo, nil, "api_token_revoked")

	if u1.PrevHash != nil {
		t.Errorf("untenanted genesis PrevHash = %v, want nil", u1.PrevHash)
	}
	if u1.AccountID != nil {
		t.Errorf("untenanted entry AccountID = %v, want nil", u1.AccountID)
	}
	if u2.PrevHash == nil || *u2.PrevHash != u1.EntryHash {
		t.Errorf("untenanted second entry PrevHash = %v, want untenanted first EntryHash %q (NOT the tenanted %q)",
			u2.PrevHash, u1.EntryHash, tenanted.EntryHash)
	}
}

// assertLinearPartition walks one run-less partition and asserts it is a
// single unforked chain: exactly one nil-prev_hash genesis, and every
// later entry's prev_hash equals its predecessor's entry_hash.
func assertLinearPartition(t *testing.T, repo audit.Repository, accountID *uuid.UUID, wantLen int) {
	t.Helper()
	entries, err := repo.ListGlobalByAccount(context.Background(), accountID)
	if err != nil {
		t.Fatalf("ListGlobalByAccount(%v): %v", accountID, err)
	}
	if len(entries) != wantLen {
		t.Fatalf("partition %v: got %d entries, want %d", accountID, len(entries), wantLen)
	}
	genesis := 0
	for i, e := range entries {
		if e.PrevHash == nil {
			genesis++
			continue
		}
		if i == 0 {
			t.Errorf("partition %v: first entry has non-nil PrevHash %q", accountID, *e.PrevHash)
			continue
		}
		if *e.PrevHash != entries[i-1].EntryHash {
			t.Errorf("partition %v: fork at index %d — PrevHash %s != prior EntryHash %s",
				accountID, i, *e.PrevHash, entries[i-1].EntryHash)
		}
	}
	if genesis != 1 {
		t.Errorf("partition %v: %d nil-prev_hash genesis entries, want exactly 1 (forked chain)", accountID, genesis)
	}
}

// TestPostgres_AppendGlobalChained_ConcurrentSameAccountNoFork is the
// binding concurrency assertion for the advisory-lock serialization:
// parallel first appends for the same fresh account must yield exactly one
// nil-prev genesis with every other entry linked linearly behind it —
// without pg_advisory_xact_lock, two writers both see the empty partition
// and both write a genesis (fork), since no unique constraint catches it.
func TestPostgres_AppendGlobalChained_ConcurrentSameAccountNoFork(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	acct := makeAccount(t, pool)

	const N = 8
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, err := repo.AppendGlobalChained(context.Background(), audit.GlobalChainAppendParams{
				Timestamp: time.Now().UTC(),
				Category:  "concurrent_global",
				Payload:   json.RawMessage(`{}`),
				AccountID: &acct,
			})
			errs[i] = err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent append #%d: %v", i, err)
		}
	}
	assertLinearPartition(t, repo, &acct, N)
}

// TestPostgres_AppendGlobalChained_ConcurrentUntenantedNoFork is the same
// no-fork assertion for the untenanted NULL partition, serialized by the
// fixed sentinel advisory-lock key. (pgtest hands every test its own
// database clone, so the partition is fresh here.)
func TestPostgres_AppendGlobalChained_ConcurrentUntenantedNoFork(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)

	const N = 8
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, err := repo.AppendGlobalChained(context.Background(), audit.GlobalChainAppendParams{
				Timestamp: time.Now().UTC(),
				Category:  "concurrent_global",
				Payload:   json.RawMessage(`{}`),
			})
			errs[i] = err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent untenanted append #%d: %v", i, err)
		}
	}
	assertLinearPartition(t, repo, nil, N)
}

// TestPostgres_ListGlobalByAccount pins the partition-listing contract:
// a non-nil account returns ONLY that account's run-less entries in
// append order; nil returns ONLY the untenanted partition; per-run rows
// never appear; and ListGlobal still returns the union.
func TestPostgres_ListGlobalByAccount(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)
	acctA, acctB := makeAccount(t, pool), makeAccount(t, pool)

	a1 := appendGlobal(t, repo, &acctA, "a_one")
	appendGlobal(t, repo, &acctB, "b_one")
	u1 := appendGlobal(t, repo, nil, "u_one")
	a2 := appendGlobal(t, repo, &acctA, "a_two")
	if _, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     runID,
		Timestamp: time.Now().UTC(),
		Category:  "per_run",
		Payload:   json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	gotA, err := repo.ListGlobalByAccount(context.Background(), &acctA)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotA) != 2 || gotA[0].ID != a1.ID || gotA[1].ID != a2.ID {
		t.Errorf("ListGlobalByAccount(A) = %d entries, want [a1, a2] in append order", len(gotA))
	}
	for _, e := range gotA {
		if e.AccountID == nil || *e.AccountID != acctA {
			t.Errorf("ListGlobalByAccount(A) leaked entry with AccountID %v", e.AccountID)
		}
	}

	gotU, err := repo.ListGlobalByAccount(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotU) != 1 || gotU[0].ID != u1.ID {
		t.Errorf("ListGlobalByAccount(nil) = %d entries, want only the untenanted entry", len(gotU))
	}

	all, err := repo.ListGlobal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Errorf("ListGlobal = %d entries, want 4 (union of all partitions, no per-run rows)", len(all))
	}
}

func TestPostgres_ListAll_MixesBothChainsTimeDesc(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	// Append in mixed order; ListAll's contract is ts DESC, not
	// insert order.
	earlier := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	later := time.Date(2026, 5, 2, 12, 30, 0, 0, time.UTC)

	if _, err := repo.AppendGlobalChained(context.Background(), audit.GlobalChainAppendParams{
		Timestamp: earlier,
		Category:  "api_token_issued",
		Payload:   json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     runID,
		Timestamp: later,
		Category:  "trace_uploaded",
		Payload:   json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := repo.ListAll(context.Background(), audit.ListAllParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ListAll returned %d entries, want 2 (mix of chains)", len(got))
	}
	if !got[0].Timestamp.After(got[1].Timestamp) && !got[0].Timestamp.Equal(got[1].Timestamp) {
		t.Errorf("ListAll order: %v then %v; want time-descending",
			got[0].Timestamp, got[1].Timestamp)
	}
	if got[0].RunID == nil {
		t.Errorf("ListAll[0] RunID = nil, want the per-run entry (later ts) on top")
	}
	if got[1].RunID != nil {
		t.Errorf("ListAll[1] RunID = %v, want the global entry (earlier ts) on bottom", got[1].RunID)
	}
}

func TestPostgres_ListAll_FiltersByCategory(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	// Two distinct categories on the run chain.
	if _, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     runID,
		Timestamp: time.Now().UTC(),
		Category:  "trace_uploaded",
		Payload:   json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     runID,
		Timestamp: time.Now().UTC(),
		Category:  "approval_granted",
		Payload:   json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	cat := "approval_granted"
	got, err := repo.ListAll(context.Background(), audit.ListAllParams{Category: &cat})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("ListAll(category=approval_granted) returned %d, want 1", len(got))
	}
	if got[0].Category != "approval_granted" {
		t.Errorf("filter leaked: got category %q", got[0].Category)
	}
}

func TestPostgres_ListAll_FiltersByRunID(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runIDA := makeRun(t, pool)
	runIDB := makeRun(t, pool)

	if _, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     runIDA,
		Timestamp: time.Now().UTC(),
		Category:  "trace_uploaded",
		Payload:   json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     runIDB,
		Timestamp: time.Now().UTC(),
		Category:  "trace_uploaded",
		Payload:   json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendGlobalChained(context.Background(), audit.GlobalChainAppendParams{
		Timestamp: time.Now().UTC(),
		Category:  "api_token_issued",
		Payload:   json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := repo.ListAll(context.Background(), audit.ListAllParams{RunID: &runIDA})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("ListAll(run_id=A) returned %d, want 1 (other run + global filtered out)", len(got))
	}
	if got[0].RunID == nil || *got[0].RunID != runIDA {
		t.Errorf("filter leaked: got RunID %v", got[0].RunID)
	}
}

// TestPostgres_ListAll_AccountFilter exercises ListAllParams.AccountID
// (ADR-057 / #1830): a set filter keeps same-account entries PLUS untenanted
// (NULL account_id) entries and excludes other accounts' entries; an empty
// filter is no constraint (the internal system readers' unnarrowed view);
// and a malformed non-empty value degrades to no constraint (accountIDArg's
// defensive nil mapping — the handler validates the account source).
func TestPostgres_ListAll_AccountFilter(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	ctx := context.Background()
	runID := makeRun(t, pool)

	acctA, acctB := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{acctA, acctB} {
		if _, err := pool.Exec(ctx, `INSERT INTO accounts (id, account_key) VALUES ($1, $2)`,
			id, "acct-"+id.String()[:8]); err != nil {
			t.Fatalf("insert account: %v", err)
		}
	}

	// audit_entries is append-only (UPDATE is trigger-forbidden), so the
	// tenanted fixtures are INSERTed directly with account_id set — no write
	// path populates the column yet (a later E44 child threads it).
	entryA, entryB, entryU := uuid.New(), uuid.New(), uuid.New()
	for id, acct := range map[uuid.UUID]*uuid.UUID{entryA: &acctA, entryB: &acctB, entryU: nil} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO audit_entries (id, run_id, ts, category, payload, entry_hash, account_id)
			 VALUES ($1, $2, now(), 'account_filter_fixture', '{}', $3, $4)`,
			id, runID, "hash-"+id.String()[:8], acct); err != nil {
			t.Fatalf("insert entry: %v", err)
		}
	}

	ids := func(es []*audit.Entry) map[uuid.UUID]bool {
		m := map[uuid.UUID]bool{}
		for _, e := range es {
			m[e.ID] = true
		}
		return m
	}

	// Account A: A's entry + the untenanted entry visible, B's excluded.
	got, err := repo.ListAll(ctx, audit.ListAllParams{AccountID: acctA.String()})
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	m := ids(got)
	if !m[entryA] || !m[entryU] || m[entryB] {
		t.Errorf("account A listing = %v; want entryA+entryU, not entryB", m)
	}

	// Empty filter: no constraint — all three visible (system reads unnarrowed).
	got, err = repo.ListAll(ctx, audit.ListAllParams{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	m = ids(got)
	if !m[entryA] || !m[entryB] || !m[entryU] {
		t.Errorf("unfiltered listing = %v; want all three entries", m)
	}

	// Malformed non-empty filter: degrades to no constraint rather than
	// erroring (defensive — the handler owns validating the source).
	got, err = repo.ListAll(ctx, audit.ListAllParams{AccountID: "not-a-uuid"})
	if err != nil {
		t.Fatalf("list malformed: %v", err)
	}
	if m = ids(got); !m[entryA] || !m[entryB] || !m[entryU] {
		t.Errorf("malformed-filter listing = %v; want all three entries (no constraint)", m)
	}
}

// mergeVerdictParams builds a merge_verdict_recorded ChainAppendParams for the
// given run — the per-run category the 0062 partial unique index gates to at
// most one row.
func mergeVerdictParams(runID uuid.UUID, verdict string) audit.ChainAppendParams {
	kind := audit.ActorUser
	subj := "github:ops"
	p, _ := json.Marshal(map[string]any{"verdict": verdict, "delegated": false})
	return audit.ChainAppendParams{
		RunID:        runID,
		Timestamp:    time.Now().UTC(),
		Category:     "merge_verdict_recorded",
		ActorKind:    &kind,
		ActorSubject: &subj,
		Payload:      p,
	}
}

// TestIsMergeVerdictDuplicate pins each recognition branch of the
// constraint-specific helper (binding condition 1, #1983): the sentinel and its
// wrapped form, a real pgconn 23505 on the merge-verdict index (bare and
// wrapped) → true; nil, an unrelated error, a 23505 on a DIFFERENT constraint,
// and a non-23505 on the index → false. A pure unit test (no Postgres).
func TestIsMergeVerdictDuplicate(t *testing.T) {
	idx := audit.MergeVerdictRecordedOnceIndex
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated error", errors.New("boom"), false},
		{"sentinel", audit.ErrMergeVerdictDuplicate, true},
		{"wrapped sentinel", fmt.Errorf("audit: append: %w", audit.ErrMergeVerdictDuplicate), true},
		{"pg 23505 on the index", &pgconn.PgError{Code: "23505", ConstraintName: idx}, true},
		{"wrapped pg 23505 on the index", fmt.Errorf("audit: append: %w", &pgconn.PgError{Code: "23505", ConstraintName: idx}), true},
		{"pg 23505 on a different constraint", &pgconn.PgError{Code: "23505", ConstraintName: "audit_entries_run_id_sequence_key"}, false},
		{"pg non-23505 on the index", &pgconn.PgError{Code: "23503", ConstraintName: idx}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := audit.IsMergeVerdictDuplicate(tt.err); got != tt.want {
				t.Errorf("IsMergeVerdictDuplicate(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestPostgres_AppendChained_MergeVerdictUniquePerRun is the done-means / real-DB
// behavioral assertion of the shipped 0062 partial unique index (#1983): a
// SECOND merge_verdict_recorded AppendChained for the same run is rejected with
// an audit.IsMergeVerdictDuplicate-recognized error, and exactly one row
// survives. A comment-only or no-op touch of the index cannot satisfy this.
func TestPostgres_AppendChained_MergeVerdictUniquePerRun(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	first, err := repo.AppendChained(context.Background(), mergeVerdictParams(runID, "first"))
	if err != nil {
		t.Fatalf("first AppendChained: %v", err)
	}
	_, err = repo.AppendChained(context.Background(), mergeVerdictParams(runID, "second"))
	if err == nil {
		t.Fatal("second AppendChained for the same run succeeded, want a merge-verdict duplicate error")
	}
	if !audit.IsMergeVerdictDuplicate(err) {
		t.Fatalf("second AppendChained error not recognized as a merge-verdict duplicate: %v", err)
	}

	rows, err := repo.ListForRunByCategory(context.Background(), runID, "merge_verdict_recorded")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("surviving merge_verdict_recorded rows = %d, want exactly 1", len(rows))
	}
	if rows[0].Sequence != first.Sequence {
		t.Errorf("surviving row sequence = %d, want the first append's %d", rows[0].Sequence, first.Sequence)
	}
}

// TestPostgres_AppendChained_MergeVerdictConcurrent is binding condition 2: the
// REAL concurrency proof. Two goroutines each AppendChained a
// merge_verdict_recorded entry for the SAME run; the FOR-UPDATE run-row
// serialization + the 0062 partial unique index make this deterministic —
// exactly one wins, the loser hits the constraint-specific duplicate, and no
// panic/deadlock. This pins the FOR-UPDATE-serialization assumption the
// compositional handler coverage rests on.
func TestPostgres_AppendChained_MergeVerdictConcurrent(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	const n = 2
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = repo.AppendChained(context.Background(), mergeVerdictParams(runID, fmt.Sprintf("g%d", i)))
		}(i)
	}
	wg.Wait()

	var success, dupes int
	for _, e := range errs {
		switch {
		case e == nil:
			success++
		case audit.IsMergeVerdictDuplicate(e):
			dupes++
		default:
			t.Fatalf("unexpected AppendChained error (want nil or a merge-verdict duplicate): %v", e)
		}
	}
	if success != 1 {
		t.Errorf("successful concurrent appends = %d, want exactly 1", success)
	}
	if dupes != 1 {
		t.Errorf("merge-verdict-duplicate losers = %d, want exactly 1", dupes)
	}

	rows, err := repo.ListForRunByCategory(context.Background(), runID, "merge_verdict_recorded")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("surviving merge_verdict_recorded rows = %d, want exactly 1", len(rows))
	}
}

// parentAwaitingParams builds a parent_awaiting_child_scope_decision
// ChainAppendParams keyed on childStageID — the value the 0067 index constrains
// via payload->>'child_stage_id'. The extra tag varies the rest of the payload
// so a duplicate-key collision is proven to key on child_stage_id, not on
// whole-payload equality.
func parentAwaitingParams(runID, childStageID uuid.UUID, tag string) audit.ChainAppendParams {
	kind := audit.ActorSystem
	p, _ := json.Marshal(map[string]any{
		"child_stage_id":       childStageID.String(),
		"shortfall_class":      "build_required",
		"build_required_paths": []string{tag},
	})
	return audit.ChainAppendParams{
		RunID:     runID,
		Timestamp: time.Now().UTC(),
		Category:  "parent_awaiting_child_scope_decision",
		ActorKind: &kind,
		Payload:   p,
	}
}

// TestIsParentAwaitingChildScopeDecisionDuplicate pins each recognition branch
// of the constraint-specific helper (#2594), mirroring TestIsMergeVerdictDuplicate:
// the sentinel and its wrapped form, a real pgconn 23505 on the 0067 index (bare
// and wrapped) → true; nil, an unrelated error, a 23505 on a DIFFERENT
// constraint, and a non-23505 on the index → false. A pure unit test (no Postgres).
func TestIsParentAwaitingChildScopeDecisionDuplicate(t *testing.T) {
	idx := audit.ParentAwaitingChildScopeDecisionOnceIndex
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated error", errors.New("boom"), false},
		{"sentinel", audit.ErrParentAwaitingChildScopeDecisionDuplicate, true},
		{"wrapped sentinel", fmt.Errorf("audit: append: %w", audit.ErrParentAwaitingChildScopeDecisionDuplicate), true},
		{"pg 23505 on the index", &pgconn.PgError{Code: "23505", ConstraintName: idx}, true},
		{"wrapped pg 23505 on the index", fmt.Errorf("audit: append: %w", &pgconn.PgError{Code: "23505", ConstraintName: idx}), true},
		{"pg 23505 on a different constraint", &pgconn.PgError{Code: "23505", ConstraintName: "audit_entries_run_id_sequence_key"}, false},
		{"pg non-23505 on the index", &pgconn.PgError{Code: "23503", ConstraintName: idx}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := audit.IsParentAwaitingChildScopeDecisionDuplicate(tt.err); got != tt.want {
				t.Errorf("IsParentAwaitingChildScopeDecisionDuplicate(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestPostgres_AppendChained_ParentAwaitingChildScopeDecisionUniquePerChildStage
// is the done-means / real-DB behavioral assertion of the shipped 0067 partial
// unique index (#2594): a SECOND parent_awaiting_child_scope_decision
// AppendChained for the SAME (run, child stage) is rejected with an
// audit.IsParentAwaitingChildScopeDecisionDuplicate-recognized error, and exactly
// one row survives. A comment-only or no-op touch of the index cannot satisfy this.
func TestPostgres_AppendChained_ParentAwaitingChildScopeDecisionUniquePerChildStage(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)
	childStage := uuid.New()

	first, err := repo.AppendChained(context.Background(), parentAwaitingParams(runID, childStage, "first"))
	if err != nil {
		t.Fatalf("first AppendChained: %v", err)
	}
	_, err = repo.AppendChained(context.Background(), parentAwaitingParams(runID, childStage, "second"))
	if err == nil {
		t.Fatal("second AppendChained for the same (run, child stage) succeeded, want a duplicate error")
	}
	if !audit.IsParentAwaitingChildScopeDecisionDuplicate(err) {
		t.Fatalf("second AppendChained error not recognized as a parent-awaiting-child-scope-decision duplicate: %v", err)
	}

	rows, err := repo.ListForRunByCategory(context.Background(), runID, "parent_awaiting_child_scope_decision")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("surviving parent_awaiting_child_scope_decision rows = %d, want exactly 1", len(rows))
	}
	if rows[0].Sequence != first.Sequence {
		t.Errorf("surviving row sequence = %d, want the first append's %d", rows[0].Sequence, first.Sequence)
	}
}

// TestPostgres_AppendChained_ParentAwaitingChildScopeDecisionDistinctChildStages
// is the done-means test for the index KEY (#2594): two DIFFERENT child stages
// under the SAME parent both persist. It goes RED if the index is mistakenly
// keyed on run_id alone — a shape the mere presence of a migration file would
// otherwise satisfy — because the second distinct-child append would then collide.
func TestPostgres_AppendChained_ParentAwaitingChildScopeDecisionDistinctChildStages(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	if _, err := repo.AppendChained(context.Background(), parentAwaitingParams(runID, uuid.New(), "childA")); err != nil {
		t.Fatalf("AppendChained for first child stage: %v", err)
	}
	if _, err := repo.AppendChained(context.Background(), parentAwaitingParams(runID, uuid.New(), "childB")); err != nil {
		t.Fatalf("AppendChained for a SECOND distinct child stage under the same parent: %v — the 0067 key must be (run_id, child_stage_id), not run_id alone", err)
	}

	rows, err := repo.ListForRunByCategory(context.Background(), runID, "parent_awaiting_child_scope_decision")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("parent_awaiting_child_scope_decision rows for two distinct child stages = %d, want 2 (index must not collapse distinct parked children)", len(rows))
	}
}

// TestPostgres_AppendChained_ParentAwaitingChildScopeDecisionConcurrent is the
// REAL concurrency proof (#2594). N goroutines each AppendChained a
// parent_awaiting_child_scope_decision entry for the SAME (parent run, child
// stage); the FOR-UPDATE run-row serialization + the 0067 partial unique index
// make this deterministic — exactly one wins, every loser hits the
// constraint-specific duplicate, no panic/deadlock — and the parent's chain
// re-verifies linearly via ComputeEntryHash, proving the rolled-back losers left
// no fork or gap. This single test covers BOTH racing pairs the issue names
// (two sibling-settle emitters, and park-time vs sibling-settle), because both
// converge on this AppendChained path.
func TestPostgres_AppendChained_ParentAwaitingChildScopeDecisionConcurrent(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)
	childStage := uuid.New()

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = repo.AppendChained(context.Background(), parentAwaitingParams(runID, childStage, fmt.Sprintf("g%d", i)))
		}(i)
	}
	wg.Wait()

	var success, dupes int
	for _, e := range errs {
		switch {
		case e == nil:
			success++
		case audit.IsParentAwaitingChildScopeDecisionDuplicate(e):
			dupes++
		default:
			t.Fatalf("unexpected AppendChained error (want nil or a parent-awaiting duplicate): %v", e)
		}
	}
	if success != 1 {
		t.Errorf("successful concurrent appends = %d, want exactly 1", success)
	}
	if dupes != n-1 {
		t.Errorf("parent-awaiting-duplicate losers = %d, want %d", dupes, n-1)
	}

	rows, err := repo.ListForRunByCategory(context.Background(), runID, "parent_awaiting_child_scope_decision")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("surviving parent_awaiting_child_scope_decision rows = %d, want exactly 1", len(rows))
	}

	// Chain integrity: the full parent chain re-verifies linearly — every entry
	// re-hashes via ComputeEntryHash over its own read-back fields, proving the
	// rolled-back losers' inserts left no fork or gap.
	all, err := repo.ListForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListForRun: %v", err)
	}
	var prev *string
	for i, e := range all {
		if (e.PrevHash == nil) != (prev == nil) || (e.PrevHash != nil && prev != nil && *e.PrevHash != *prev) {
			t.Fatalf("chain link broken at index %d: prev_hash=%v, want %v", i, e.PrevHash, prev)
		}
		recomputed, herr := audit.ComputeEntryHash(audit.HashInputs{
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
			t.Fatalf("ComputeEntryHash at index %d: %v", i, herr)
		}
		if recomputed != e.EntryHash {
			t.Fatalf("entry-hash mismatch at index %d after a rolled-back collision: stored %s recomputed %s", i, e.EntryHash, recomputed)
		}
		h := e.EntryHash
		prev = &h
	}
}

// approvalConditionsTruncatedParams builds an approval_conditions_truncated
// ChainAppendParams keyed on sourceEntryID — the value the 0068 index constrains
// via payload->>'source_entry_id'. The extra tag varies the rest of the payload
// so a duplicate-key collision is proven to key on source_entry_id, not on
// whole-payload equality.
func approvalConditionsTruncatedParams(runID, sourceEntryID uuid.UUID, tag string) audit.ChainAppendParams {
	kind := audit.ActorSystem
	p, _ := json.Marshal(map[string]any{
		"source_entry_id": sourceEntryID.String(),
		"source":          "approval_submitted",
		"original_bytes":  13000,
		"cap_bytes":       12000,
		"dropped_bytes":   1000,
		"tag":             tag,
	})
	return audit.ChainAppendParams{
		RunID:     runID,
		Timestamp: time.Now().UTC(),
		Category:  "approval_conditions_truncated",
		ActorKind: &kind,
		Payload:   p,
	}
}

// TestIsApprovalConditionsTruncatedDuplicate pins each recognition branch of the
// constraint-specific helper (#2622), mirroring TestIsMergeVerdictDuplicate and
// TestIsParentAwaitingChildScopeDecisionDuplicate: the sentinel and its wrapped
// form, a real pgconn 23505 on the 0068 index (bare and wrapped) → true; nil, an
// unrelated error, a 23505 on a DIFFERENT constraint, and a non-23505 on the
// index → false. A pure unit test (no Postgres).
func TestIsApprovalConditionsTruncatedDuplicate(t *testing.T) {
	idx := audit.ApprovalConditionsTruncatedOnceIndex
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated error", errors.New("boom"), false},
		{"sentinel", audit.ErrApprovalConditionsTruncatedDuplicate, true},
		{"wrapped sentinel", fmt.Errorf("audit: append: %w", audit.ErrApprovalConditionsTruncatedDuplicate), true},
		{"pg 23505 on the index", &pgconn.PgError{Code: "23505", ConstraintName: idx}, true},
		{"wrapped pg 23505 on the index", fmt.Errorf("audit: append: %w", &pgconn.PgError{Code: "23505", ConstraintName: idx}), true},
		{"pg 23505 on a different constraint", &pgconn.PgError{Code: "23505", ConstraintName: "audit_entries_run_id_sequence_key"}, false},
		{"pg non-23505 on the index", &pgconn.PgError{Code: "23503", ConstraintName: idx}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := audit.IsApprovalConditionsTruncatedDuplicate(tt.err); got != tt.want {
				t.Errorf("IsApprovalConditionsTruncatedDuplicate(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestPostgres_AppendChained_ApprovalConditionsTruncatedUniquePerSource is the
// done-means / real-DB behavioral assertion of the shipped 0068 partial unique
// index (#2622): a SECOND approval_conditions_truncated AppendChained for the
// SAME (run, source_entry_id) is rejected with an
// audit.IsApprovalConditionsTruncatedDuplicate-recognized error, and exactly one
// row survives. A comment-only or no-op touch of the index cannot satisfy this.
func TestPostgres_AppendChained_ApprovalConditionsTruncatedUniquePerSource(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)
	sourceEntry := uuid.New()

	first, err := repo.AppendChained(context.Background(), approvalConditionsTruncatedParams(runID, sourceEntry, "first"))
	if err != nil {
		t.Fatalf("first AppendChained: %v", err)
	}
	_, err = repo.AppendChained(context.Background(), approvalConditionsTruncatedParams(runID, sourceEntry, "second"))
	if err == nil {
		t.Fatal("second AppendChained for the same (run, source_entry_id) succeeded, want a duplicate error")
	}
	if !audit.IsApprovalConditionsTruncatedDuplicate(err) {
		t.Fatalf("second AppendChained error not recognized as an approval-conditions-truncated duplicate: %v", err)
	}

	rows, err := repo.ListForRunByCategory(context.Background(), runID, "approval_conditions_truncated")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("surviving approval_conditions_truncated rows = %d, want exactly 1", len(rows))
	}
	if rows[0].Sequence != first.Sequence {
		t.Errorf("surviving row sequence = %d, want the first append's %d", rows[0].Sequence, first.Sequence)
	}
}

// TestPostgres_AppendChained_ApprovalConditionsTruncatedDistinctSources is the
// done-means test for the index KEY (#2622): two DIFFERENT source_entry_id
// values under the SAME run both persist. It goes RED if the index is mistakenly
// keyed on run_id alone — a shape the mere presence of a migration file would
// otherwise satisfy — because the second distinct-source append would then
// collide, suppressing a genuinely second over-cap approve comment's truncation.
func TestPostgres_AppendChained_ApprovalConditionsTruncatedDistinctSources(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	if _, err := repo.AppendChained(context.Background(), approvalConditionsTruncatedParams(runID, uuid.New(), "commentA")); err != nil {
		t.Fatalf("AppendChained for first source entry: %v", err)
	}
	if _, err := repo.AppendChained(context.Background(), approvalConditionsTruncatedParams(runID, uuid.New(), "commentB")); err != nil {
		t.Fatalf("AppendChained for a SECOND distinct source entry under the same run: %v — the 0068 key must be (run_id, source_entry_id), not run_id alone", err)
	}

	rows, err := repo.ListForRunByCategory(context.Background(), runID, "approval_conditions_truncated")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("approval_conditions_truncated rows for two distinct source entries = %d, want 2 (index must not collapse distinct over-cap comments)", len(rows))
	}
}

// TestPostgres_AppendChained_ApprovalConditionsTruncatedConcurrent is the REAL
// concurrency proof (#2622). N goroutines each AppendChained an
// approval_conditions_truncated entry for the SAME (run, source_entry_id); the
// FOR-UPDATE run-row serialization + the 0068 partial unique index make this
// deterministic — exactly one wins, every loser hits the constraint-specific
// duplicate, no panic/deadlock.
func TestPostgres_AppendChained_ApprovalConditionsTruncatedConcurrent(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)
	sourceEntry := uuid.New()

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = repo.AppendChained(context.Background(), approvalConditionsTruncatedParams(runID, sourceEntry, fmt.Sprintf("g%d", i)))
		}(i)
	}
	wg.Wait()

	var success, dupes int
	for _, e := range errs {
		switch {
		case e == nil:
			success++
		case audit.IsApprovalConditionsTruncatedDuplicate(e):
			dupes++
		default:
			t.Fatalf("unexpected AppendChained error (want nil or an approval-conditions-truncated duplicate): %v", e)
		}
	}
	if success != 1 {
		t.Errorf("successful concurrent appends = %d, want exactly 1", success)
	}
	if dupes != n-1 {
		t.Errorf("approval-conditions-truncated-duplicate losers = %d, want %d", dupes, n-1)
	}

	rows, err := repo.ListForRunByCategory(context.Background(), runID, "approval_conditions_truncated")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("surviving approval_conditions_truncated rows = %d, want exactly 1", len(rows))
	}
}

// TestPostgres_AppendChained_ApprovalConditionsTruncatedChainIntact proves the
// rolled-back loser of a 0068 collision leaves the run's audit chain linear and
// re-verifiable (#2622): after a deliberate same-key collision, ListForRun over
// the run shows a linear prev_hash chain whose every entry re-computes via
// ComputeEntryHash — no fork, no gap.
func TestPostgres_AppendChained_ApprovalConditionsTruncatedChainIntact(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)
	sourceEntry := uuid.New()

	if _, err := repo.AppendChained(context.Background(), approvalConditionsTruncatedParams(runID, sourceEntry, "first")); err != nil {
		t.Fatalf("first AppendChained: %v", err)
	}
	// A second same-key append collides and rolls back inside its transaction.
	if _, err := repo.AppendChained(context.Background(), approvalConditionsTruncatedParams(runID, sourceEntry, "second")); err == nil {
		t.Fatal("second same-key AppendChained succeeded, want a duplicate collision")
	}
	// A further append under a DIFFERENT source key must still link cleanly onto
	// the surviving chain head (not onto the rolled-back loser).
	if _, err := repo.AppendChained(context.Background(), approvalConditionsTruncatedParams(runID, uuid.New(), "third")); err != nil {
		t.Fatalf("post-collision distinct-key AppendChained: %v", err)
	}

	all, err := repo.ListForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListForRun: %v", err)
	}
	var prev *string
	for i, e := range all {
		if (e.PrevHash == nil) != (prev == nil) || (e.PrevHash != nil && prev != nil && *e.PrevHash != *prev) {
			t.Fatalf("chain link broken at index %d: prev_hash=%v, want %v", i, e.PrevHash, prev)
		}
		recomputed, herr := audit.ComputeEntryHash(audit.HashInputs{
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
			t.Fatalf("ComputeEntryHash at index %d: %v", i, herr)
		}
		if recomputed != e.EntryHash {
			t.Fatalf("entry-hash mismatch at index %d after a rolled-back collision: stored %s recomputed %s", i, e.EntryHash, recomputed)
		}
		h := e.EntryHash
		prev = &h
	}
}

// --- AnchoredChainAppender (#2536) -------------------------------------------
//
// The atomic anchor-revalidate-and-append primitive. Every REFUSAL case asserts
// COMMITTED STATE (zero rows of the appended category) in addition to the error
// identity: the control's real effect is committed state, and a control that
// fires and rolls back returns a byte-identical error, so an error-identity-only
// assertion would stay green with the control deleted.

const (
	anchorCategory = "acceptance_outcome_recorded"
	anchoredCat    = "acceptance_triage_arbitrated"
	anchorKey      = "outcome_sequence"
)

// seedAnchor appends one anchor-category entry and returns its sequence.
func seedAnchor(t *testing.T, repo audit.Repository, runID uuid.UUID) int64 {
	t.Helper()
	e, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, Timestamp: time.Now().UTC(), Category: anchorCategory,
		Payload: json.RawMessage(`{"verdict":"failed"}`),
	})
	if err != nil {
		t.Fatalf("seed anchor: %v", err)
	}
	return e.Sequence
}

// arbitrationPayload renders the appended entry's payload binding it to seq.
func arbitrationPayload(seq int64) json.RawMessage {
	p, _ := json.Marshal(map[string]any{"reason": "operator discharge", anchorKey: seq})
	return p
}

// anchoredSpec is the acceptance-arbitration AnchorSpec at sequence seq.
func anchoredSpec(seq int64) audit.AnchorSpec {
	return audit.AnchorSpec{
		AnchorCategory:   anchorCategory,
		AnchorSequence:   seq,
		DedupePayloadKey: anchorKey,
		DedupeValue:      seq,
		ConstraintName:   audit.AcceptanceTriageArbitratedOnceIndex,
	}
}

// countAnchoredRows reports how many appended-category rows the run committed.
func countAnchoredRows(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_entries WHERE run_id = $1 AND category = $2`,
		runID, anchoredCat).Scan(&n); err != nil {
		t.Fatalf("count %s rows: %v", anchoredCat, err)
	}
	return n
}

// (a) happy path: the anchor matches, no duplicate exists, one chained entry
// lands and links to the run's prior entry.
func TestPostgres_AppendChainedAnchored_HappyPath(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	appender := repo.(audit.AnchoredChainAppender)
	runID := makeRun(t, pool)
	ctx := context.Background()

	seq := seedAnchor(t, repo, runID)
	entry, err := appender.AppendChainedAnchored(ctx, audit.ChainAppendParams{
		RunID: runID, Timestamp: time.Now().UTC(), Category: anchoredCat,
		Payload: arbitrationPayload(seq),
	}, anchoredSpec(seq))
	if err != nil {
		t.Fatalf("AppendChainedAnchored: %v", err)
	}
	if entry == nil || entry.Sequence <= seq {
		t.Fatalf("entry = %+v, want a chained entry above the anchor sequence %d", entry, seq)
	}
	if entry.PrevHash == nil {
		t.Error("PrevHash = nil, want the anchor entry's hash (the append must chain)")
	}
	if n := countAnchoredRows(t, pool, runID); n != 1 {
		t.Errorf("committed %s rows = %d, want 1", anchoredCat, n)
	}
}

// (b) anchor moved: a NEWER anchor entry commits before the call. Asserts the
// typed error AND — the load-bearing half — that ZERO rows committed.
func TestPostgres_AppendChainedAnchored_AnchorMoved(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	appender := repo.(audit.AnchoredChainAppender)
	runID := makeRun(t, pool)
	ctx := context.Background()

	stale := seedAnchor(t, repo, runID)
	newest := seedAnchor(t, repo, runID) // supersedes `stale`

	entry, err := appender.AppendChainedAnchored(ctx, audit.ChainAppendParams{
		RunID: runID, Timestamp: time.Now().UTC(), Category: anchoredCat,
		Payload: arbitrationPayload(stale),
	}, anchoredSpec(stale))
	if entry != nil {
		t.Errorf("entry = %+v, want nil on a moved anchor", entry)
	}
	var moved *audit.AnchorMovedError
	if !errors.As(err, &moved) {
		t.Fatalf("err = %v, want *audit.AnchorMovedError", err)
	}
	if moved.Expected != stale || moved.Current != newest || !moved.Recorded {
		t.Errorf("moved = %+v, want Expected=%d Current=%d Recorded=true", moved, stale, newest)
	}
	// COMMITTED STATE: the control's effect. A control that fired and rolled
	// back would return a byte-identical error, so this is what discriminates.
	if n := countAnchoredRows(t, pool, runID); n != 0 {
		t.Errorf("committed %s rows = %d, want 0 (a refused append must write nothing)", anchoredCat, n)
	}
}

// (b2) no anchor recorded at all: Recorded=false, nothing written.
func TestPostgres_AppendChainedAnchored_NoAnchorRecorded(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	appender := repo.(audit.AnchoredChainAppender)
	runID := makeRun(t, pool)

	entry, err := appender.AppendChainedAnchored(context.Background(), audit.ChainAppendParams{
		RunID: runID, Timestamp: time.Now().UTC(), Category: anchoredCat,
		Payload: arbitrationPayload(7),
	}, anchoredSpec(7))
	if entry != nil {
		t.Errorf("entry = %+v, want nil with no anchor recorded", entry)
	}
	var moved *audit.AnchorMovedError
	if !errors.As(err, &moved) {
		t.Fatalf("err = %v, want *audit.AnchorMovedError", err)
	}
	if moved.Recorded {
		t.Errorf("moved.Recorded = true, want false (no anchor entry exists)")
	}
	if n := countAnchoredRows(t, pool, runID); n != 0 {
		t.Errorf("committed %s rows = %d, want 0", anchoredCat, n)
	}
}

// (c) in-transaction dedupe hit: a prior entry already binds this anchor, so the
// scan inside the transaction returns it and nothing is appended.
func TestPostgres_AppendChainedAnchored_InTransactionDuplicate(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	appender := repo.(audit.AnchoredChainAppender)
	runID := makeRun(t, pool)
	ctx := context.Background()

	seq := seedAnchor(t, repo, runID)
	first, err := appender.AppendChainedAnchored(ctx, audit.ChainAppendParams{
		RunID: runID, Timestamp: time.Now().UTC(), Category: anchoredCat,
		Payload: arbitrationPayload(seq),
	}, anchoredSpec(seq))
	if err != nil {
		t.Fatalf("first append: %v", err)
	}

	entry, err := appender.AppendChainedAnchored(ctx, audit.ChainAppendParams{
		RunID: runID, Timestamp: time.Now().UTC(), Category: anchoredCat,
		Payload: arbitrationPayload(seq),
	}, anchoredSpec(seq))
	if entry != nil {
		t.Errorf("entry = %+v, want nil on a duplicate", entry)
	}
	var dup *audit.AnchoredDuplicateError
	if !errors.As(err, &dup) {
		t.Fatalf("err = %v, want *audit.AnchoredDuplicateError", err)
	}
	if dup.Existing == nil || dup.Existing.Sequence != first.Sequence {
		t.Errorf("dup.Existing = %+v, want the first entry at sequence %d", dup.Existing, first.Sequence)
	}
	if n := countAnchoredRows(t, pool, runID); n != 1 {
		t.Errorf("committed %s rows = %d, want 1", anchoredCat, n)
	}
}

// (d) GENUINE backstop-index collision at the repo layer, driven through the
// decode asymmetry rather than a race: a chain-valid prior entry writes the
// dedupe key as a JSON STRING, so payload->>'outcome_sequence' collides on the
// index while the typed in-transaction scan MISSES it. The append trips 23505,
// BeginFunc rolls back, and the out-of-transaction recovery — which matches on
// the INDEX's own TEXT semantics — finds the seeded row and returns it.
func TestPostgres_AppendChainedAnchored_IndexCollisionRecovery(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	appender := repo.(audit.AnchoredChainAppender)
	runID := makeRun(t, pool)
	ctx := context.Background()

	seq := seedAnchor(t, repo, runID)
	// String-typed dedupe key: same index key ('7'), invisible to the *int64 scan.
	stringTyped, _ := json.Marshal(map[string]any{
		"reason": "seeded with a string-typed sequence", anchorKey: fmt.Sprintf("%d", seq),
	})
	seeded, err := repo.AppendChained(ctx, audit.ChainAppendParams{
		RunID: runID, Timestamp: time.Now().UTC(), Category: anchoredCat,
		Payload: stringTyped,
	})
	if err != nil {
		t.Fatalf("seed string-typed prior arbitration: %v", err)
	}

	entry, err := appender.AppendChainedAnchored(ctx, audit.ChainAppendParams{
		RunID: runID, Timestamp: time.Now().UTC(), Category: anchoredCat,
		Payload: arbitrationPayload(seq),
	}, anchoredSpec(seq))
	if entry != nil {
		t.Errorf("entry = %+v, want nil on an index collision", entry)
	}
	var dup *audit.AnchoredDuplicateError
	if !errors.As(err, &dup) {
		t.Fatalf("err = %v, want *audit.AnchoredDuplicateError from the 23505 recovery", err)
	}
	if dup.Existing == nil || dup.Existing.Sequence != seeded.Sequence {
		t.Errorf("dup.Existing = %+v, want the SEEDED entry at sequence %d", dup.Existing, seeded.Sequence)
	}
	if n := countAnchoredRows(t, pool, runID); n != 1 {
		t.Errorf("committed %s rows = %d, want 1 (only the seeded row)", anchoredCat, n)
	}
}

// (e) unknown run: the run-row lock finds no row.
func TestPostgres_AppendChainedAnchored_UnknownRun(t *testing.T) {
	pool := pgtest.NewPool(t)
	appender := audit.NewPostgresRepository(pool).(audit.AnchoredChainAppender)

	entry, err := appender.AppendChainedAnchored(context.Background(), audit.ChainAppendParams{
		RunID: uuid.New(), Timestamp: time.Now().UTC(), Category: anchoredCat,
		Payload: arbitrationPayload(1),
	}, anchoredSpec(1))
	if entry != nil {
		t.Errorf("entry = %+v, want nil for an unknown run", entry)
	}
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want a run-not-found error", err)
	}
}

// concurrentAnchoredAppends fires n concurrent AppendChainedAnchored calls for
// the same (run, anchor) and returns how many succeeded.
//
// The workers are deliberately t-FREE (the testing package requires
// FailNow/Fatalf to run on the goroutine running the test, where a worker-side
// fatal can hang or misreport). Each loser's error is COLLECTED instead and
// reported here, after wg.Wait(), on the test goroutine: an infrastructure
// failure — a pool exhaustion, a lock timeout, a dropped connection — is then a
// named test failure rather than an indistinguishable "granted = 0", which the
// callers' `granted != 1` assertion would report as a concurrency defect.
func concurrentAnchoredAppends(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, seq int64, n int) int {
	t.Helper()
	appender := audit.NewPostgresRepository(pool).(audit.AnchoredChainAppender)
	ctx := context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	var unexpected []error
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, err := appender.AppendChainedAnchored(ctx, audit.ChainAppendParams{
				RunID: runID, Timestamp: time.Now().UTC(), Category: anchoredCat,
				Payload: arbitrationPayload(seq),
			}, anchoredSpec(seq))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil && e != nil:
				granted++
			case err != nil:
				// The only LEGAL loss is the duplicate branch — every other
				// error is infrastructure and must be named.
				var dup *audit.AnchoredDuplicateError
				if !errors.As(err, &dup) {
					unexpected = append(unexpected, err)
				}
			default:
				unexpected = append(unexpected, fmt.Errorf("nil entry with nil error"))
			}
		}()
	}
	wg.Wait()
	for _, err := range unexpected {
		t.Errorf("concurrent anchored append failed for a reason other than the duplicate branch: %v", err)
	}
	return granted
}

// (f) N concurrent appends for the same (run, anchor) commit EXACTLY ONE row.
func TestPostgres_AppendChainedAnchored_ConcurrentExactlyOne(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)
	seq := seedAnchor(t, repo, runID)

	if granted := concurrentAnchoredAppends(t, pool, runID, seq, 8); granted != 1 {
		t.Errorf("granted = %d, want exactly 1", granted)
	}
	if n := countAnchoredRows(t, pool, runID); n != 1 {
		t.Errorf("committed %s rows = %d, want exactly 1", anchoredCat, n)
	}
}

// (g) the INDEX-DROPPED variant of (f), and the point of the whole design: with
// the migration 0080 backstop index DROPPED in this test's OWN ephemeral
// database, N concurrent appends must STILL commit exactly one row. That proves
// the run-row lock + in-transaction dedupe scan is load-bearing on its own and
// the index is a backstop, not the only control.
func TestPostgres_AppendChainedAnchored_ConcurrentExactlyOne_IndexDropped(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)
	seq := seedAnchor(t, repo, runID)

	if _, err := pool.Exec(context.Background(),
		`DROP INDEX `+audit.AcceptanceTriageArbitratedOnceIndex); err != nil {
		t.Fatalf("drop backstop index: %v", err)
	}
	// Guard the SEAM: the index must actually be gone, or this test silently
	// re-runs (f) and asserts nothing about the lock+scan layer.
	var idx int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_indexes WHERE tablename = 'audit_entries' AND indexname = $1`,
		audit.AcceptanceTriageArbitratedOnceIndex).Scan(&idx); err != nil {
		t.Fatalf("verify index dropped: %v", err)
	}
	if idx != 0 {
		t.Fatalf("backstop index still present (count = %d) — this test would be vacuous", idx)
	}

	if granted := concurrentAnchoredAppends(t, pool, runID, seq, 8); granted != 1 {
		t.Errorf("granted = %d, want exactly 1 WITHOUT the backstop index", granted)
	}
	if n := countAnchoredRows(t, pool, runID); n != 1 {
		t.Errorf("committed %s rows = %d, want exactly 1 WITHOUT the backstop index — the lock + in-transaction scan must stand alone", anchoredCat, n)
	}
}

// TestIsAcceptanceArbitrationDuplicate pins the #2536 NARROWING: only the
// migration 0080 index's 23505 (or the fake sentinel) is the benign
// already-recorded collision. A 23505 on ANY other constraint the anchored
// insert can trip — the entry-hash or (run_id, sequence) uniqueness — must stay
// a HARD error, because swallowing it would report an unrelated integrity
// failure as a successful idempotent discharge.
func TestIsAcceptanceArbitrationDuplicate(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"sentinel", audit.ErrAcceptanceArbitrationDuplicate, true},
		{"wrapped sentinel", fmt.Errorf("append: %w", audit.ErrAcceptanceArbitrationDuplicate), true},
		{"23505 on the 0080 index", &pgconn.PgError{
			Code: "23505", ConstraintName: audit.AcceptanceTriageArbitratedOnceIndex}, true},
		{"wrapped 23505 on the 0080 index", fmt.Errorf("audit: append: %w", &pgconn.PgError{
			Code: "23505", ConstraintName: audit.AcceptanceTriageArbitratedOnceIndex}), true},
		{"23505 on the entry-hash constraint", &pgconn.PgError{
			Code: "23505", ConstraintName: "audit_entries_entry_hash_key"}, false},
		{"23505 on the (run_id, sequence) constraint", &pgconn.PgError{
			Code: "23505", ConstraintName: "audit_entries_run_id_sequence_key"}, false},
		{"23505 on the merge-verdict index", &pgconn.PgError{
			Code: "23505", ConstraintName: audit.MergeVerdictRecordedOnceIndex}, false},
		{"non-23505 on the 0080 index", &pgconn.PgError{
			Code: "23503", ConstraintName: audit.AcceptanceTriageArbitratedOnceIndex}, false},
		{"unrelated error", errors.New("boom"), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := audit.IsAcceptanceArbitrationDuplicate(tc.err); got != tc.want {
				t.Errorf("IsAcceptanceArbitrationDuplicate(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestAnchoredErrorMessages pins both typed errors' Error() strings, including
// the no-anchor-recorded and nil-Existing branches an operator can actually see
// in a 500 body or a server log.
func TestAnchoredErrorMessages(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want []string
	}{
		{"anchor moved", &audit.AnchorMovedError{Expected: 7, Current: 9, Recorded: true},
			[]string{"expected sequence 7", "current sequence 9"}},
		{"no anchor recorded", &audit.AnchorMovedError{Expected: 7, Recorded: false},
			[]string{"expected sequence 7", "no anchor entry recorded"}},
		{"duplicate with entry", &audit.AnchoredDuplicateError{Existing: &audit.Entry{Sequence: 42}},
			[]string{"duplicate", "sequence 42"}},
		{"duplicate with nil entry", &audit.AnchoredDuplicateError{},
			[]string{"anchored append duplicate"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := tc.err.Error()
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Errorf("Error() = %q, want it to contain %q", msg, want)
				}
			}
		})
	}
}

// TestPostgres_AppendChainedAnchored_DedupeScanIgnoresUnusableKeys pins the
// dedupe scan's three fail-to-match branches, each seeded BY CONSTRUCTION as a
// real committed row rather than by calling the scan in the setup: a payload
// that is not a JSON object at all, a payload missing the dedupe key, and a
// payload whose key holds a non-integer JSON value. None of them binds this
// anchor, so the append must SUCCEED — the scan must not treat "unreadable" as
// "matches" (which would refuse a legitimate first discharge) nor as a decode
// error that aborts the append.
//
// The string-typed key — the one case where this MISS diverges from the backstop
// index's TEXT key — is covered separately by
// TestPostgres_AppendChainedAnchored_IndexCollisionRecovery.
func TestPostgres_AppendChainedAnchored_DedupeScanIgnoresUnusableKeys(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
	}{
		{"payload is not a JSON object", `"just a string"`},
		{"payload missing the dedupe key", `{"reason":"no binding"}`},
		{"dedupe key holds a JSON object", `{"outcome_sequence":{"nested":1}}`},
		{"dedupe key holds JSON null", `{"outcome_sequence":null}`},
		{"dedupe key holds a boolean", `{"outcome_sequence":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := pgtest.NewPool(t)
			repo := audit.NewPostgresRepository(pool)
			appender := repo.(audit.AnchoredChainAppender)
			runID := makeRun(t, pool)
			ctx := context.Background()
			seq := seedAnchor(t, repo, runID)

			if _, err := repo.AppendChained(ctx, audit.ChainAppendParams{
				RunID: runID, Timestamp: time.Now().UTC(), Category: anchoredCat,
				Payload: json.RawMessage(tc.payload),
			}); err != nil {
				t.Fatalf("seed unusable-key row: %v", err)
			}

			entry, err := appender.AppendChainedAnchored(ctx, audit.ChainAppendParams{
				RunID: runID, Timestamp: time.Now().UTC(), Category: anchoredCat,
				Payload: arbitrationPayload(seq),
			}, anchoredSpec(seq))
			if err != nil {
				t.Fatalf("AppendChainedAnchored over an unusable-key row: %v", err)
			}
			if entry == nil {
				t.Fatal("entry = nil, want the fresh append to land")
			}
			if n := countAnchoredRows(t, pool, runID); n != 2 {
				t.Errorf("committed %s rows = %d, want 2 (the seed plus the fresh append)", anchoredCat, n)
			}
		})
	}
}
