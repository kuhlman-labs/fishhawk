package digest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

const testRepo = "acme/widgets"

// fixture seeds a real chain through the migrated schema. Entries are appended
// through the RAW audit repository — not wrapped by decisionindex's indexing
// decorator — so a decision-bearing entry is UNINDEXED by construction until a
// test indexes it explicitly.
type fixture struct {
	pool  *pgxpool.Pool
	audit audit.Repository
	idx   *decisionindex.Store
	st    *Store
	run   uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := pgtest.NewPool(t)
	f := &fixture{pool: pool, audit: audit.NewPostgresRepository(pool), idx: decisionindex.NewStore(pool), st: NewStore(pool)}
	f.run = f.seedRun(t, testRepo, "running")
	return f
}

func (f *fixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("seed %q: %v", sql, err)
	}
}

func (f *fixture) seedRun(t *testing.T, repo, state string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.exec(t, `INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind)
	           VALUES ($1, $2, 'feature_change', 'sha', 'cli', $3, 'local')`, id, repo, state)
	return id
}

func (f *fixture) seedStage(t *testing.T, runID uuid.UUID, seq int, kind, state string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.exec(t, `INSERT INTO stages (id, run_id, sequence, stage_type, executor_kind, executor_ref, state)
	           VALUES ($1, $2, $3, $4, 'agent', 'claude-code', $5)`, id, runID, seq, kind, state)
	return id
}

func (f *fixture) appendEntry(t *testing.T, runID uuid.UUID, stageID *uuid.UUID, category string, payload map[string]any) *audit.Entry {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	kind := audit.ActorUser
	subject := "operator"
	e, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, StageID: stageID, Timestamp: time.Now().UTC(), Category: category,
		ActorKind: &kind, ActorSubject: &subject, Payload: raw,
	})
	if err != nil {
		t.Fatalf("append %s: %v", category, err)
	}
	return e
}

// mergeEntry appends a merge_verdict_recorded entry on a FRESH run in the
// repo: the category is once-per-run (audit_entries_merge_verdict_recorded_once_idx).
func (f *fixture) mergeEntry(t *testing.T) *audit.Entry {
	t.Helper()
	return f.appendEntry(t, f.seedRun(t, testRepo, "succeeded"), nil, "merge_verdict_recorded", map[string]any{})
}

// index writes the decision_index row citing e, as the live writer would.
func (f *fixture) index(t *testing.T, e *audit.Entry, class decisionindex.DecisionClass, mutate func(*decisionindex.Row)) decisionindex.Row {
	t.Helper()
	r := decisionindex.Row{
		SourceSequence: e.Sequence, SourceEntryHash: e.EntryHash, RunID: *e.RunID, StageID: e.StageID,
		Repo: testRepo, WorkflowID: "feature_change", DoctrineVersion: "sha", DecisionClass: class,
		StageKind: "implement", Outcome: "approve", ConcernCategoryRaw: "correctness", Severity: "high",
		DecidedAt: e.Timestamp, ReasonSequence: e.Sequence, ReasonKey: "reason",
	}
	if mutate != nil {
		mutate(&r)
	}
	if err := f.idx.Upsert(context.Background(), r); err != nil {
		t.Fatalf("index %d: %v", e.Sequence, err)
	}
	return r
}

// burnSequence consumes one audit_entries.sequence value WITHOUT writing an
// entry, so a later append lands above it: the by-construction way to reach
// an index row citing a sequence the chain does not hold (audit_entries is
// append-only, so the row cannot be deleted instead).
func (f *fixture) burnSequence(t *testing.T) int64 {
	t.Helper()
	var seq int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT nextval(pg_get_serial_sequence('audit_entries', 'sequence'))`).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

func (f *fixture) deps() Deps { return Deps{Store: f.st, Index: f.idx} }

func (f *fixture) build(t *testing.T, req Request) Digest {
	t.Helper()
	if req.Repo == "" {
		req.Repo = testRepo
	}
	if req.CaptainSubject == "" {
		req.CaptainSubject = "captain-a"
	}
	d, err := Build(context.Background(), f.deps(), req)
	if err != nil {
		t.Fatalf("Build(%+v): %v", req, err)
	}
	return d
}

func sectionOf(t *testing.T, d Digest, k SectionKind) Section {
	t.Helper()
	for _, s := range d.Sections {
		if s.Kind == k {
			return s
		}
	}
	t.Fatalf("digest has no %s section (sections %+v)", k, d.Sections)
	return Section{}
}

func itemSeqs(items []Item) []int64 {
	out := []int64{}
	for _, it := range items {
		out = append(out, it.SourceSequence)
	}
	return out
}

// TestChainHead_IsMaxSequenceForRepo pins the cross-run watermark premise:
// the head is the max sequence over EVERY run in the repo, excludes other
// repos' entries, and is 0 for a repo with no entries.
func TestChainHead_IsMaxSequenceForRepo(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if h, err := f.st.ChainHead(ctx, testRepo); err != nil || h != 0 {
		t.Fatalf("empty repo head = %d, %v; want 0", h, err)
	}
	other := f.seedRun(t, testRepo, "succeeded")
	elsewhere := f.seedRun(t, "acme/gadgets", "running")
	f.appendEntry(t, f.run, nil, "run_started", map[string]any{})
	last := f.appendEntry(t, other, nil, "run_started", map[string]any{})
	f.appendEntry(t, elsewhere, nil, "run_started", map[string]any{})
	h, err := f.st.ChainHead(ctx, testRepo)
	if err != nil {
		t.Fatal(err)
	}
	if h != last.Sequence {
		t.Errorf("head = %d, want %d (max over both runs in the repo, excluding acme/gadgets)", h, last.Sequence)
	}
}

// TestEntryHashes_OmitsAbsentSequences pins the citation check's input.
func TestEntryHashes_OmitsAbsentSequences(t *testing.T) {
	f := newFixture(t)
	e := f.appendEntry(t, f.run, nil, "run_started", map[string]any{})
	burned := f.burnSequence(t)
	got, err := f.st.EntryHashes(context.Background(), []int64{e.Sequence, burned})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[e.Sequence] != e.EntryHash {
		t.Errorf("hashes = %v, want only {%d: %s}", got, e.Sequence, e.EntryHash)
	}
	if empty, err := f.st.EntryHashes(context.Background(), nil); err != nil || len(empty) != 0 {
		t.Errorf("no sequences = %v, %v; want empty", empty, err)
	}
}
