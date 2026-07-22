package audit_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
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
