package decisionindex

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

// chainFixture seeds a real chain through the migrated schema: two accounts,
// one run under each, plan + implement stages on run A and a plan stage on
// run B. Entries are appended through the real audit repository so their
// sequence, entry_hash and account stamp are the product's own.
type chainFixture struct {
	pool                             *pgxpool.Pool
	repo                             audit.Repository
	accountA, accountB               uuid.UUID
	runA, runB                       uuid.UUID
	planStageA, implStageA, planStgB uuid.UUID
}

func newChainFixture(t *testing.T) *chainFixture {
	t.Helper()
	pool := pgtest.NewPool(t)
	f := &chainFixture{
		pool:       pool,
		repo:       audit.NewPostgresRepository(pool),
		accountA:   uuid.New(),
		accountB:   uuid.New(),
		runA:       uuid.New(),
		runB:       uuid.New(),
		planStageA: uuid.New(),
		implStageA: uuid.New(),
		planStgB:   uuid.New(),
	}
	f.exec(t, `INSERT INTO accounts (id, account_key) VALUES ($1, 'di-account-a'), ($2, 'di-account-b')`,
		f.accountA, f.accountB)
	f.seedRun(t, f.runA, "acme/widgets", "sha-a", &f.accountA)
	f.seedRun(t, f.runB, "acme/gadgets", "sha-b", &f.accountB)
	f.seedStage(t, f.planStageA, f.runA, 0, "plan")
	f.seedStage(t, f.implStageA, f.runA, 1, "implement")
	f.seedStage(t, f.planStgB, f.runB, 0, "plan")
	return f
}

func (f *chainFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("seed %q: %v", sql, err)
	}
}

func (f *chainFixture) seedRun(t *testing.T, id uuid.UUID, repo, sha string, account *uuid.UUID) {
	t.Helper()
	f.exec(t, `INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind, account_id)
	           VALUES ($1, $2, 'feature_change', $3, 'cli', 'pending', 'local', $4)`, id, repo, sha, account)
}

func (f *chainFixture) seedStage(t *testing.T, id, runID uuid.UUID, seq int, kind string) {
	t.Helper()
	f.exec(t, `INSERT INTO stages (id, run_id, sequence, stage_type, executor_kind, executor_ref, state)
	           VALUES ($1, $2, $3, $4, 'agent', 'claude-code', 'pending')`, id, runID, seq, kind)
}

// seedPlan writes a plan artifact on stageID whose scope.files name paths.
func (f *chainFixture) seedPlan(t *testing.T, stageID uuid.UUID, createdAt time.Time, paths ...string) {
	t.Helper()
	files := make([]map[string]string, 0, len(paths))
	for _, p := range paths {
		files = append(files, map[string]string{"path": p, "operation": "modify"})
	}
	content, err := json.Marshal(map[string]any{"scope": map[string]any{"files": files}})
	if err != nil {
		t.Fatal(err)
	}
	f.seedPlanRaw(t, stageID, createdAt, content)
}

// seedPlanRaw writes a plan artifact with VERBATIM content, so a test can seed a
// historical plan shape the current standard_v1 writer would never emit.
func (f *chainFixture) seedPlanRaw(t *testing.T, stageID uuid.UUID, createdAt time.Time, content []byte) {
	t.Helper()
	f.exec(t, `INSERT INTO artifacts (id, stage_id, kind, content, content_hash, created_at)
	           VALUES ($1, $2, 'plan', $3, $4, $5)`, uuid.New(), stageID, content, "h-"+uuid.NewString(), createdAt)
}

// seedConcern writes a review_concerns row and returns its id.
func (f *chainFixture) seedConcern(t *testing.T, runID, stageID uuid.UUID, severity, category string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.exec(t, `INSERT INTO review_concerns (id, run_id, stage_id, stage_kind, origin_review_sequence, severity, category, note)
	           VALUES ($1, $2, $3, 'implement', 1, $4, $5, 'n')`, id, runID, stageID, severity, category)
	return id
}

// appendEntry appends one entry through the real chained audit path.
func (f *chainFixture) appendEntry(t *testing.T, runID uuid.UUID, stageID *uuid.UUID, category string, payload map[string]any) *audit.Entry {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	kind := audit.ActorUser
	subject := "operator"
	e, err := f.repo.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:        runID,
		StageID:      stageID,
		Timestamp:    time.Now().UTC(),
		Category:     category,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      raw,
	})
	if err != nil {
		t.Fatalf("append %s: %v", category, err)
	}
	return e
}

// insertOrphanEntry writes a decision-bearing entry whose run row does NOT
// exist, BY CONSTRUCTION: session_replication_role=replica disables the
// audit_entries run_id FK trigger for this one transaction (the admin test
// role is a superuser). It is the only way to reach the state, since
// audit_entries.run_id is ON DELETE RESTRICT.
func (f *chainFixture) insertOrphanEntry(t *testing.T, category string) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatalf("disable FK triggers: %v", err)
	}
	var seq int64
	if err := tx.QueryRow(ctx, `INSERT INTO audit_entries (id, run_id, category, payload, entry_hash)
		VALUES ($1, $2, $3, '{"decision":"approve"}', 'orphan-hash') RETURNING sequence`,
		uuid.New(), uuid.New(), category).Scan(&seq); err != nil {
		t.Fatalf("insert orphan entry: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return seq
}

func sampleRow(runID uuid.UUID, seq int64) Row {
	return Row{
		SourceSequence:  seq,
		SourceEntryHash: "hash",
		RunID:           runID,
		Repo:            "acme/widgets",
		WorkflowID:      "feature_change",
		DoctrineVersion: "sha-a",
		DecisionClass:   ClassPlanApproval,
		StageKind:       "plan",
		Outcome:         "approve",
		TouchedPaths:    []string{"a.go"},
		EscalationKeys:  []string{},
		DecidedAt:       time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		ReasonSequence:  seq,
	}
}

func countRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM decision_index`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestStore_UpsertIsIdempotentOnSourceSequence pins the ON CONFLICT DO UPDATE
// clause. MECHANISM: the second upsert differs only in a derived column, so a
// bare INSERT raises a duplicate-key error and DO NOTHING keeps the stale
// value. COUNTERFACTUAL: drop the ON CONFLICT clause → RED on the error.
func TestStore_UpsertIsIdempotentOnSourceSequence(t *testing.T) {
	f := newChainFixture(t)
	ctx := context.Background()
	s := NewStore(f.pool)
	r := sampleRow(f.runA, 101)
	if err := s.Upsert(ctx, r); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	r.Outcome = "reject"
	r.RejectClass = "scope"
	if err := s.Upsert(ctx, r); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	got, err := s.List(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("rows = %d, want exactly 1", len(got))
	}
	if got[0].Outcome != "reject" || got[0].RejectClass != "scope" {
		t.Errorf("row outcome/reject_class = %q/%q, want the second upsert's reject/scope", got[0].Outcome, got[0].RejectClass)
	}
}

// TestStore_UpsertBatchIsAtomic: a batch whose second row violates the run_id
// FK leaves NO row behind — the first row is rolled back with it.
func TestStore_UpsertBatchIsAtomic(t *testing.T) {
	f := newChainFixture(t)
	ctx := context.Background()
	s := NewStore(f.pool)
	if err := s.UpsertBatch(ctx, nil); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
	err := s.UpsertBatch(ctx, []Row{sampleRow(f.runA, 201), sampleRow(uuid.New(), 202)})
	if err == nil {
		t.Fatal("batch with a dangling run_id succeeded, want an FK error")
	}
	if n := countRows(t, f.pool); n != 0 {
		t.Errorf("rows after failed batch = %d, want 0 (the batch must be one transaction)", n)
	}
}

// TestStore_ListFiltersOnRepoClassKind pins ADR-082 rule 3's hard filter and
// the full round trip of every column.
func TestStore_ListFiltersOnRepoClassKind(t *testing.T) {
	f := newChainFixture(t)
	ctx := context.Background()
	s := NewStore(f.pool)
	stage := f.planStageA
	full := sampleRow(f.runA, 301)
	full.StageID = &stage
	full.AccountID = &f.accountA
	full.RejectClass = "approach"
	full.ConcernCategoryRaw, full.ConcernCategory, full.ConcernCategoryUnmapped = "Flaky", "flaky", true
	full.Severity = "high"
	full.EscalationKeys = []string{"k1", "k2"}
	full.Delegated = true
	full.ActorKind, full.ActorSubject = "user", "op"
	full.ReasonKey = "rejection_comment"
	other := sampleRow(f.runB, 302)
	other.Repo = "acme/gadgets"
	waive := sampleRow(f.runA, 303)
	waive.DecisionClass = ClassConcernWaive
	impl := sampleRow(f.runA, 304)
	impl.StageKind = "implement"
	for _, r := range []Row{full, other, waive, impl} {
		if err := s.Upsert(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.List(ctx, ListFilter{Repo: "acme/widgets", DecisionClass: ClassPlanApproval, StageKind: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("filtered rows = %d, want 1: %+v", len(got), got)
	}
	assertRowsEqual(t, []Row{full}, got)

	all, err := s.List(ctx, ListFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].SourceSequence != 301 || all[1].SourceSequence != 302 {
		t.Errorf("limited list = %v, want sequences 301,302", seqsOf(all))
	}
}

// TestStore_GapsReportsUnindexedDecisionEntries: a decision-bearing entry with
// no index row is a gap; a non-decision entry never is; indexing it closes it.
// MECHANISM: the entry is appended through the audit repo only, so the LEFT
// JOIN has a genuine unindexed row. COUNTERFACTUAL: Gaps body → empty report.
func TestStore_GapsReportsUnindexedDecisionEntries(t *testing.T) {
	f := newChainFixture(t)
	ctx := context.Background()
	s := NewStore(f.pool)
	e := f.appendEntry(t, f.runA, &f.planStageA, "approval_submitted", map[string]any{"decision": "approve"})
	f.appendEntry(t, f.runA, &f.planStageA, "run_started", map[string]any{})

	rep, err := s.Gaps(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.GapCount != 1 || len(rep.Gaps) != 1 {
		t.Fatalf("gaps = %d (%+v), want exactly the approval entry", rep.GapCount, rep.Gaps)
	}
	g := rep.Gaps[0]
	if g.SourceSequence != e.Sequence || g.Category != "approval_submitted" || g.RunID != f.runA {
		t.Errorf("gap = %+v, want sequence %d approval_submitted on run %s", g, e.Sequence, f.runA)
	}
	if rep.OrphanedCount != 0 {
		t.Errorf("orphaned = %d, want 0", rep.OrphanedCount)
	}

	if _, err := Backfill(ctx, f.pool, Options{}); err != nil {
		t.Fatal(err)
	}
	rep, err = s.Gaps(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.GapCount != 0 || len(rep.Gaps) != 0 {
		t.Errorf("gaps after indexing = %d (%+v), want none", rep.GapCount, rep.Gaps)
	}
}

// TestStore_GapsSeparatesOrphanedEntries pins #3730 approval condition 6: a
// decision-bearing entry whose run row does not exist is reported as ORPHANED,
// not as a gap. MECHANISM: the orphan is seeded by construction next to a real
// gap, so a Gaps that ignored the runs join would report two gaps.
func TestStore_GapsSeparatesOrphanedEntries(t *testing.T) {
	f := newChainFixture(t)
	ctx := context.Background()
	gap := f.appendEntry(t, f.runA, &f.planStageA, "concern_waived", map[string]any{"reason": "r"})
	orphan := f.insertOrphanEntry(t, "merge_verdict_recorded")

	rep, err := NewStore(f.pool).Gaps(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.GapCount != 1 || len(rep.Gaps) != 1 || rep.Gaps[0].SourceSequence != gap.Sequence {
		t.Errorf("gaps = %d %+v, want only sequence %d", rep.GapCount, rep.Gaps, gap.Sequence)
	}
	if rep.OrphanedCount != 1 || len(rep.OrphanedSequences) != 1 || rep.OrphanedSequences[0] != orphan {
		t.Errorf("orphaned = %d %v, want [%d]", rep.OrphanedCount, rep.OrphanedSequences, orphan)
	}
}

// TestStore_GapsLimitBoundsListsNotCounts: the limit truncates the returned
// lists, never the totals an operator alerts on.
func TestStore_GapsLimitBoundsListsNotCounts(t *testing.T) {
	f := newChainFixture(t)
	for i := 0; i < 3; i++ {
		f.appendEntry(t, f.runA, &f.planStageA, "clarification_answered", map[string]any{"comment": "c"})
		f.insertOrphanEntry(t, "approval_submitted")
	}
	rep, err := NewStore(f.pool).Gaps(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if rep.GapCount != 3 || len(rep.Gaps) != 2 {
		t.Errorf("gap count/list = %d/%d, want 3/2", rep.GapCount, len(rep.Gaps))
	}
	if rep.OrphanedCount != 3 || len(rep.OrphanedSequences) != 2 {
		t.Errorf("orphan count/list = %d/%d, want 3/2", rep.OrphanedCount, len(rep.OrphanedSequences))
	}
}

// TestPoolResolver_RunMissing: the per-entry resolver the live writer uses
// refuses an entry with no run id and one whose run row is gone, naming
// ErrRunMissing rather than returning an empty context that would index a row
// with a blank repo.
func TestPoolResolver_RunMissing(t *testing.T) {
	f := newChainFixture(t)
	ctx := context.Background()
	r := NewPoolResolver(f.pool)
	if _, err := r.Resolve(ctx, nil); !errors.Is(err, ErrNilEntry) {
		t.Errorf("nil entry err = %v, want ErrNilEntry", err)
	}
	if _, err := r.Resolve(ctx, &audit.Entry{Sequence: 1, Category: "approval_submitted"}); !errors.Is(err, ErrRunMissing) {
		t.Errorf("nil run id err = %v, want ErrRunMissing", err)
	}
	seq := f.insertOrphanEntry(t, "approval_submitted")
	gone := uuid.New()
	if _, err := r.Resolve(ctx, &audit.Entry{Sequence: seq, Category: "approval_submitted", RunID: &gone}); !errors.Is(err, ErrRunMissing) {
		t.Errorf("missing run row err = %v, want ErrRunMissing", err)
	}

	e := f.appendEntry(t, f.runA, &f.planStageA, "approval_submitted", map[string]any{"decision": "approve"})
	rc, err := r.Resolve(ctx, e)
	if err != nil {
		t.Fatalf("resolve live entry: %v", err)
	}
	if rc.Repo != "acme/widgets" || rc.DoctrineVersion != "sha-a" || rc.StageKind != "plan" {
		t.Errorf("context = %+v, want the run A / plan stage join", rc)
	}
}

func seqsOf(rows []Row) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.SourceSequence)
	}
	return out
}

// TestList_AccountNarrowing pins the E75.3 account predicate.
//
// MECHANISM / MASKING GUARD: the fixture runs as the admin test role, which
// BYPASSES RLS, so the SQL predicate is the ONLY thing in the path. Were this
// run under RLS the decision_index_tenant_isolation policy would mask a deleted
// WHERE arm and the case would prove nothing — which is exactly why it is
// deliberately run un-masked.
// COUNTERFACTUAL: delete the account arm from listSQL's WHERE → the second
// account's row is returned and the test is RED.
func TestList_AccountNarrowing(t *testing.T) {
	f := newChainFixture(t)
	ctx := context.Background()
	s := NewStore(f.pool)

	mine := sampleRow(f.runA, 301)
	mine.AccountID = &f.accountA
	foreign := sampleRow(f.runB, 302)
	foreign.Repo = "acme/widgets" // same repo, so ONLY the account arm can drop it
	foreign.AccountID = &f.accountB
	untenanted := sampleRow(f.runA, 303)
	untenanted.AccountID = nil
	if err := s.UpsertBatch(ctx, []Row{mine, foreign, untenanted}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := s.List(ctx, ListFilter{AccountID: &f.accountA})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for _, r := range got {
		seen[r.SourceSequence] = true
	}
	if !seen[301] {
		t.Error("the caller's OWN row (301) was not returned")
	}
	// Deliberate RLS parity (#3730: NULL-account rows stay visible, the #1829
	// window): a single-tenant deployment writes NULL-account rows and would
	// otherwise see nothing.
	if !seen[303] {
		t.Error("the NULL-account row (303) was not returned; the predicate must mirror the RLS policy")
	}
	if seen[302] {
		t.Error("ANOTHER ACCOUNT'S row (302) was returned")
	}

	// A nil AccountID is the un-narrowed backfill/CLI path: every row.
	all, err := s.List(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("nil-account listing returned %d rows, want all 3", len(all))
	}
}

// TestList_NewestFirstWindow: a bounded window is the NEWEST N.
// COUNTERFACTUAL: delete the DESC flip in listSQL → the OLDEST are returned.
func TestList_NewestFirstWindow(t *testing.T) {
	f := newChainFixture(t)
	ctx := context.Background()
	s := NewStore(f.pool)
	var rows []Row
	for seq := int64(401); seq <= 405; seq++ {
		rows = append(rows, sampleRow(f.runA, seq))
	}
	if err := s.UpsertBatch(ctx, rows); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := s.List(ctx, ListFilter{Newest: true, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("window size = %d, want 2", len(got))
	}
	if got[0].SourceSequence != 405 || got[1].SourceSequence != 404 {
		t.Errorf("window = %d,%d, want the newest 405,404", got[0].SourceSequence, got[1].SourceSequence)
	}

	oldest, err := s.List(ctx, ListFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if oldest[0].SourceSequence != 401 {
		t.Errorf("ascending window starts at %d, want 401 (the pre-#3731 order must be unchanged)", oldest[0].SourceSequence)
	}
}

// TestGateContext_DerivesFromRunStagePlanAndEscalation pins that a gate
// reference resolves through the SAME plan-artifact and escalation joins the
// indexer used — including that a DIFFERENT stage's escalation is not picked up.
func TestGateContext_DerivesFromRunStagePlanAndEscalation(t *testing.T) {
	f := newChainFixture(t)
	ctx := context.Background()
	s := NewStore(f.pool)

	f.seedPlan(t, f.planStageA, time.Now().UTC().Add(-time.Hour), "b/z.go", "a/x.go", "a/x.go")
	// A DIFFERENT stage's escalation must not be picked up...
	f.appendEntry(t, f.runA, &f.planStageA, "escalation_fired",
		map[string]any{"fired_keys": []string{"other_stage_rule"}})
	// ...and on the SAME stage, an EARLIER escalation must be SUPERSEDED by the
	// latest one, not unioned with it. Without a second same-stage entry, a
	// mutation replacing LatestEscalationKeys with a union over the candidates
	// would be unobservable (the candidate query already filters to one stage),
	// so this entry is what isolates the "latest" semantics.
	f.appendEntry(t, f.runA, &f.implStageA, "escalation_fired",
		map[string]any{"fired_keys": []string{"superseded_rule"}})
	f.appendEntry(t, f.runA, &f.implStageA, "escalation_fired",
		map[string]any{"fired_keys": []string{"scope_cap_exceeded", "autonomy_low"}})

	gc, err := s.GateContext(ctx, GateRef{RunID: f.runA, StageID: &f.implStageA, AccountID: &f.accountA})
	if err != nil {
		t.Fatalf("GateContext: %v", err)
	}
	if gc.Repo != "acme/widgets" || gc.WorkflowID != "feature_change" || gc.DoctrineVersion != "sha-a" {
		t.Errorf("run facts = %q/%q/%q, want acme/widgets/feature_change/sha-a", gc.Repo, gc.WorkflowID, gc.DoctrineVersion)
	}
	if gc.StageKind != "implement" {
		t.Errorf("stage kind = %q, want implement", gc.StageKind)
	}
	if len(gc.TouchedPaths) != 2 || gc.TouchedPaths[0] != "a/x.go" || gc.TouchedPaths[1] != "b/z.go" {
		t.Errorf("touched paths = %v, want the sorted de-duplicated [a/x.go b/z.go]", gc.TouchedPaths)
	}
	if len(gc.EscalationKeys) != 2 || gc.EscalationKeys[0] != "autonomy_low" || gc.EscalationKeys[1] != "scope_cap_exceeded" {
		t.Errorf("escalation keys = %v, want ONLY the LATEST same-stage entry's sorted keys", gc.EscalationKeys)
	}
	for _, k := range gc.EscalationKeys {
		if k == "superseded_rule" {
			t.Errorf("an EARLIER same-stage escalation's key leaked in: %v", gc.EscalationKeys)
		}
	}

	// No stage id: the plan still resolves, escalation keys are empty.
	noStage, err := s.GateContext(ctx, GateRef{RunID: f.runA, AccountID: &f.accountA})
	if err != nil {
		t.Fatalf("GateContext without a stage: %v", err)
	}
	if len(noStage.EscalationKeys) != 0 {
		t.Errorf("escalation keys without a stage = %v, want empty", noStage.EscalationKeys)
	}
	if len(noStage.TouchedPaths) != 2 {
		t.Errorf("touched paths without a stage = %v, want the run's plan paths", noStage.TouchedPaths)
	}
}

// TestGateContext_RunMissing covers the THREE indistinguishable misses (binding
// condition 1): a nonexistent run, a run in ANOTHER account, and a stage that is
// not on the run. Each returns the named ErrRunMissing and NO derived context.
//
// COUNTERFACTUALS: delete the account arm from gateContextSQL's WHERE → the
// foreign-account arm resolves and is RED; delete the stage EXISTS arm → the
// wrong-stage arm resolves (with an empty stage kind) and is RED.
func TestGateContext_RunMissing(t *testing.T) {
	f := newChainFixture(t)
	ctx := context.Background()
	s := NewStore(f.pool)
	f.seedPlan(t, f.planStageA, time.Now().UTC(), "a/x.go")

	cases := []struct {
		name string
		ref  GateRef
	}{
		{"nonexistent run", GateRef{RunID: uuid.New(), AccountID: &f.accountA}},
		{"run in another account", GateRef{RunID: f.runB, AccountID: &f.accountA}},
		{"stage not on the run", GateRef{RunID: f.runA, StageID: &f.planStgB, AccountID: &f.accountA}},
		{"tenanted run, account-less caller", GateRef{RunID: f.runA}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gc, err := s.GateContext(ctx, tc.ref)
			if !errors.Is(err, ErrRunMissing) {
				t.Fatalf("err = %v, want ErrRunMissing", err)
			}
			if gc.Repo != "" || len(gc.TouchedPaths) != 0 || len(gc.EscalationKeys) != 0 {
				t.Errorf("a miss returned derived context %+v, want the zero value", gc)
			}
		})
	}

	// Positive control for the account arm: the run's OWN account resolves, so
	// the misses above are the predicate biting and not a broken fixture.
	if _, err := s.GateContext(ctx, GateRef{RunID: f.runB, AccountID: &f.accountB}); err != nil {
		t.Fatalf("run B under account B: %v", err)
	}
}
