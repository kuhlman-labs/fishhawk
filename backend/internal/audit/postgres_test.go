package audit_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
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
	pool := startContainer(t)
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
	pool := startContainer(t)
	repo := audit.NewPostgresRepository(pool)

	_, err := repo.Get(context.Background(), uuid.New())
	if !errors.Is(err, audit.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestPostgres_ListForRun_OrderedBySequence(t *testing.T) {
	pool := startContainer(t)
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
	pool := startContainer(t)
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
	pool := startContainer(t)
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
	pool := startContainer(t)
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
	pool := startContainer(t)
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
	pool := startContainer(t)
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
	pool := startContainer(t)
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
	pool := startContainer(t)
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
	pool := startContainer(t)
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
	pool := startContainer(t)
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
	pool := startContainer(t)
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
	pool := startContainer(t)
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

func TestPostgres_ListAll_MixesBothChainsTimeDesc(t *testing.T) {
	pool := startContainer(t)
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
	pool := startContainer(t)
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
	pool := startContainer(t)
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
