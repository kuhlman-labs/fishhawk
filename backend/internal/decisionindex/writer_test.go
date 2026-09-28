package decisionindex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// decorate wraps the fixture's real repository with the indexing decorator
// over store + resolver, swaps it into the fixture so every f.appendEntry goes
// through it, and returns it with its log sink.
func decorate(t *testing.T, f *chainFixture, store *Store, resolver ContextResolver) (*IndexingRepository, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r, err := NewIndexingRepository(audit.NewPostgresRepository(f.pool), store, resolver, logger)
	if err != nil {
		t.Fatalf("NewIndexingRepository: %v", err)
	}
	f.repo = r
	return r, logs
}

// closedPool returns a pool onto the fixture's database that is already
// CLOSED — a store over it fails every write by construction, without stubbing
// the control under test.
func closedPool(t *testing.T, f *chainFixture) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), f.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	return p
}

func countIndexRows(t *testing.T, f *chainFixture) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM decision_index`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// assertEntryCommitted reads e back from audit_entries — the committed-state
// half of "an index failure never fails the decision".
func assertEntryCommitted(t *testing.T, f *chainFixture, e *audit.Entry) {
	t.Helper()
	var hash string
	if err := f.pool.QueryRow(context.Background(), `SELECT entry_hash FROM audit_entries WHERE sequence = $1`, e.Sequence).Scan(&hash); err != nil {
		t.Fatalf("decision entry %d not committed: %v", e.Sequence, err)
	}
	if hash != e.EntryHash {
		t.Fatalf("committed entry_hash = %q, returned %q", hash, e.EntryHash)
	}
}

func assertWarned(t *testing.T, logs *syncBuffer, e *audit.Entry, fragment string) {
	t.Helper()
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "decision index write failed") {
		t.Fatalf("no WARN logged for the swallowed index failure; logs:\n%s", out)
	}
	if !strings.Contains(out, "sequence="+strconv.FormatInt(e.Sequence, 10)) || !strings.Contains(out, "category="+e.Category) {
		t.Fatalf("WARN does not name sequence %d / category %s; logs:\n%s", e.Sequence, e.Category, out)
	}
	if !strings.Contains(out, fragment) {
		t.Fatalf("WARN does not name the failing step %q; logs:\n%s", fragment, out)
	}
}

// TestIndexingRepository_IndexesLikeBackfill is the writer's cross-layer
// proof: all nine decision classes plus interleaved non-decision entries are
// appended THROUGH the decorator over the real chain, and the rows the live
// writer produced must equal — complete rows — what a from-scratch Backfill
// rebuilds from the same chain. Non-decision entries produce no row.
//
// Deleting the index call from AppendChained's body leaves the table empty
// (RED on the count); a writer that resolved or extracted differently from the
// backfill is RED on the row comparison.
func TestIndexingRepository_IndexesLikeBackfill(t *testing.T) {
	f := newChainFixture(t)
	ctx := context.Background()
	s := NewStore(f.pool)
	_, logs := decorate(t, f, s, NewPoolResolver(f.pool))

	seqs := seedAllNine(t, f)
	written, err := s.List(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != len(seqs) {
		t.Fatalf("writer indexed %d rows (sequences %v), want %d — one per decision entry and none for the interleaved non-decision entries",
			len(written), seqsOf(written), len(seqs))
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("healthy writes logged a WARN:\n%s", logs.String())
	}

	if _, err := Backfill(ctx, f.pool, Options{Rebuild: true}); err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	rebuilt, err := s.List(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	assertRowsEqual(t, rebuilt, written)
}

// TestIndexingRepository_EscalationKeysThroughWriter drives #3730 approval
// condition 3 through the LIVE path: the writer's resolver takes the fired_keys
// of the latest escalation_fired entry on the decision's own stage, and an
// escalation on a DIFFERENT stage never lands.
func TestIndexingRepository_EscalationKeysThroughWriter(t *testing.T) {
	f := newChainFixture(t)
	ctx := context.Background()
	s := NewStore(f.pool)
	decorate(t, f, s, NewPoolResolver(f.pool))

	f.appendEntry(t, f.runA, &f.planStageA, "escalation_fired", map[string]any{"fired_keys": []string{"zeta", "alpha", "zeta"}})
	f.appendEntry(t, f.runA, &f.implStageA, "escalation_fired", map[string]any{"fired_keys": []string{"other-stage"}})
	onPlan := f.appendEntry(t, f.runA, &f.planStageA, "approval_submitted", map[string]any{"decision": "approve"})
	onB := f.appendEntry(t, f.runB, &f.planStgB, "approval_submitted", map[string]any{"decision": "approve"})

	rows, err := s.List(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64][]string{}
	for _, r := range rows {
		got[r.SourceSequence] = r.EscalationKeys
	}
	if want := []string{"alpha", "zeta"}; !reflect.DeepEqual(got[onPlan.Sequence], want) {
		t.Errorf("same-stage escalation keys = %v, want %v", got[onPlan.Sequence], want)
	}
	if k, ok := got[onB.Sequence]; !ok || len(k) != 0 {
		t.Errorf("decision on a stage with no escalation: keys = %v (indexed=%v), want empty", k, ok)
	}
}

// TestIndexingRepository_IndexFailureDoesNotFailAppend — ADR-082 rule 1. The
// store points at a CLOSED pool, so the upsert fails by construction. The
// append must still return the entry and a nil error, the entry must be
// COMMITTED (read back, not inferred from the return), and the failure must be
// logged at WARN naming the entry.
//
// Counterfactual: make index() propagate (have AppendChained return
// nil, err from the index step) — RED on the nil-error assertion.
func TestIndexingRepository_IndexFailureDoesNotFailAppend(t *testing.T) {
	f := newChainFixture(t)
	_, logs := decorate(t, f, NewStore(closedPool(t, f)), NewPoolResolver(f.pool))

	e, err := f.repo.AppendChained(context.Background(), approvalParams(t, f, map[string]any{"decision": "approve"}))
	if err != nil {
		t.Fatalf("an index failure failed the decision: %v", err)
	}
	if e == nil {
		t.Fatal("an index failure dropped the decision's entry")
	}
	assertEntryCommitted(t, f, e)
	assertWarned(t, logs, e, "upsert")
	if n := countIndexRows(t, f); n != 0 {
		t.Fatalf("decision_index rows = %d, want 0 (the upsert target was closed)", n)
	}
}

// TestIndexingRepository_IndexSurvivesCallerCancellation — the index write
// runs on a context detached from the caller's cancellation, so a request whose
// client disconnected AFTER its decision committed does not also lose the row.
// The entry is appended through the UNDECORATED repository, then indexed with an
// already-cancelled context; without context.WithoutCancel the resolve fails
// on the cancelled context and no row lands.
func TestIndexingRepository_IndexSurvivesCallerCancellation(t *testing.T) {
	f := newChainFixture(t)
	inner := f.repo
	d, logs := decorate(t, f, NewStore(f.pool), NewPoolResolver(f.pool))
	f.repo = inner

	e := f.appendEntry(t, f.runA, &f.planStageA, "approval_submitted", map[string]any{"decision": "approve"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.index(ctx, e)
	if n := countIndexRows(t, f); n != 1 {
		t.Fatalf("decision_index rows = %d after indexing under a cancelled caller context, want 1; logs:\n%s", n, logs.String())
	}
}

// failingResolver fails every resolution — the resolve step's failure mode.
type failingResolver struct{}

func (failingResolver) Resolve(context.Context, *audit.Entry) (RowContext, error) {
	return RowContext{}, errors.New("resolver down")
}

// TestIndexingRepository_ResolveFailureDoesNotFailAppend covers the resolve
// step's failure: decision committed, nil error, WARN naming the step.
func TestIndexingRepository_ResolveFailureDoesNotFailAppend(t *testing.T) {
	f := newChainFixture(t)
	_, logs := decorate(t, f, NewStore(f.pool), failingResolver{})

	e, err := f.repo.AppendChained(context.Background(), approvalParams(t, f, map[string]any{"decision": "approve"}))
	if err != nil || e == nil {
		t.Fatalf("a resolve failure failed the decision: entry=%v err=%v", e, err)
	}
	assertEntryCommitted(t, f, e)
	assertWarned(t, logs, e, "resolve context")
	if n := countIndexRows(t, f); n != 0 {
		t.Fatalf("decision_index rows = %d, want 0", n)
	}
}

// TestIndexingRepository_ExtractFailureDoesNotFailAppend covers the extract
// step's failure: a decision entry whose payload is valid JSON but not an
// object (Extract refuses it) is committed, the append succeeds, and the WARN
// names the extract step.
func TestIndexingRepository_ExtractFailureDoesNotFailAppend(t *testing.T) {
	f := newChainFixture(t)
	_, logs := decorate(t, f, NewStore(f.pool), NewPoolResolver(f.pool))

	p := approvalParams(t, f, nil)
	p.Payload = json.RawMessage(`[1]`)
	e, err := f.repo.AppendChained(context.Background(), p)
	if err != nil || e == nil {
		t.Fatalf("an extract failure failed the decision: entry=%v err=%v", e, err)
	}
	assertEntryCommitted(t, f, e)
	assertWarned(t, logs, e, "extract")
}

// TestIndexingRepository_InnerErrorPropagatesUnchanged — the decorator must
// not mask the INNER repository's failure: an append against a run that does
// not exist returns the inner error, and nothing is indexed.
func TestIndexingRepository_InnerErrorPropagatesUnchanged(t *testing.T) {
	f := newChainFixture(t)
	decorate(t, f, NewStore(f.pool), NewPoolResolver(f.pool))

	p := approvalParams(t, f, map[string]any{"decision": "approve"})
	p.RunID = uuid.New()
	p.StageID = nil
	e, err := f.repo.AppendChained(context.Background(), p)
	if err == nil {
		t.Fatalf("append against a missing run succeeded (entry %v)", e)
	}
	if e != nil {
		t.Fatalf("failed append returned an entry: %v", e)
	}
	if n := countIndexRows(t, f); n != 0 {
		t.Fatalf("decision_index rows = %d, want 0", n)
	}
}

// TestNewIndexingRepository_RefusesRepositoryLackingCapability — the
// constructor FAILS CLOSED on an inner repository without the optional
// capabilities, rather than returning a decorator that silently disables the
// server features type-asserting them (or claims ones it cannot honour).
func TestNewIndexingRepository_RefusesRepositoryLackingCapability(t *testing.T) {
	r, err := NewIndexingRepository(audit.BaseFake{}, NewStore(nil), failingResolver{}, nil)
	if !errors.Is(err, ErrMissingCapability) {
		t.Fatalf("err = %v, want ErrMissingCapability", err)
	}
	if r != nil {
		t.Fatalf("returned a decorator despite the refusal: %v", r)
	}
}

// TestNewIndexingRepository_NilLoggerDefaults — a nil logger must not panic
// the first swallowed failure.
func TestNewIndexingRepository_NilLoggerDefaults(t *testing.T) {
	r, err := NewIndexingRepository(audit.NewPostgresRepository(nil), NewStore(nil), failingResolver{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.logger == nil {
		t.Fatal("nil logger was not defaulted")
	}
}

func approvalParams(t *testing.T, f *chainFixture, payload map[string]any) audit.ChainAppendParams {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	kind := audit.ActorUser
	subject := "operator"
	return audit.ChainAppendParams{
		RunID:        f.runA,
		StageID:      &f.planStageA,
		Timestamp:    time.Now().UTC(),
		Category:     "approval_submitted",
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      raw,
	}
}
