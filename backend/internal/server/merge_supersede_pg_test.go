package server

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// merge_supersede_pg_test.go proves the CROSS-PROCESS half of the
// exactly-one-stage_superseded_by_merge-row guarantee (E64.29 / #3133).
//
// The pre-existing TestReconcileMerge_ConcurrentRepairsWriteExactlyOneRow races
// two GOROUTINES inside one process, so it cannot discriminate the in-process
// supersedeRepairMu from a durable database constraint — which is precisely why
// the multi-process residual survived #3083's fix-up. The vehicle here is TWO
// INDEPENDENT pgxpool pools against ONE database, each with its own audit
// repository and its own Server: that is what a second fishhawkd replica
// actually presents to the store, and nothing in process memory is shared
// between the two appends except the package mutex, which the tests below
// deliberately do not rely on.

// twoPoolFixture is one database, two independent pools, two Servers — the
// second fishhawkd replica modelled at the seam every repair path funnels
// through (appendStageSupersededAudit has exactly two call sites: the sweep and
// the repair scan).
type twoPoolFixture struct {
	url     string
	poolA   *pgxpool.Pool
	poolB   *pgxpool.Pool
	serverA *Server
	serverB *Server
	// readAudit is poolA's audit repository, used for the committed-state reads
	// the assertions are made over.
	readAudit audit.Repository
	runID     uuid.UUID
	stageID   uuid.UUID
}

// newTwoPoolFixture seeds one run plus one stage on a fresh pgtest database and
// opens two independent pools against it. pgtest.NewURL hands back a plain URL
// for a freshly migrated per-test database, so two pools need no harness change.
func newTwoPoolFixture(t *testing.T) *twoPoolFixture {
	t.Helper()
	ctx := context.Background()
	url := pgtest.NewURL(t)

	open := func(which string) *pgxpool.Pool {
		p, err := postgres.Connect(ctx, url)
		if err != nil {
			t.Fatalf("connect pool %s: %v", which, err)
		}
		t.Cleanup(p.Close)
		return p
	}
	poolA, poolB := open("A"), open("B")

	newServer := func(pool *pgxpool.Pool) (*Server, run.Repository, audit.Repository) {
		runRepo := run.NewPostgresRepository(pool)
		auditRepo := audit.NewPostgresRepository(pool)
		orch := &orchestrator.Orchestrator{Runs: runRepo, Audit: auditRepo}
		return New(Config{Addr: "127.0.0.1:0", RunRepo: runRepo, AuditRepo: auditRepo, Orchestrator: orch}), runRepo, auditRepo
	}
	serverA, runRepoA, auditA := newServer(poolA)
	serverB, _, _ := newServer(poolB)

	r, err := runRepoA.CreateRun(ctx, run.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := runRepoA.TransitionRun(ctx, r.ID, run.StateRunning); err != nil {
		t.Fatalf("run -> running: %v", err)
	}
	st, err := runRepoA.CreateStage(ctx, run.CreateStageParams{
		RunID: r.ID, Sequence: 0, Type: run.StageTypeAcceptance,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}

	return &twoPoolFixture{
		url: url, poolA: poolA, poolB: poolB,
		serverA: serverA, serverB: serverB,
		readAudit: auditA, runID: r.ID, stageID: st.ID,
	}
}

// rec is the supersession both replicas try to record.
func (f *twoPoolFixture) rec() supersededStage {
	return supersededStage{
		StageID:   f.stageID,
		StageType: string(run.StageTypeAcceptance),
		FromState: string(run.StageStateAwaitingHostDispatch),
		Reason:    supersedeReasonRepair,
	}
}

// raceAppend drives both Servers' appendStageSupersededAudit for the SAME
// (run, stage) from a released-together barrier and returns both errors in
// launch order. It deliberately does NOT go through repairMissingSupersedeRows:
// supersedeRepairMu is package-level and shared by every Server in this process,
// so routing through the repair scan would serialize the two "replicas" on the
// in-process mutex and the index would never be the thing under test.
func (f *twoPoolFixture) raceAppend(t *testing.T) []error {
	t.Helper()
	ctx := context.Background()
	servers := []*Server{f.serverA, f.serverB}
	errs := make([]error, len(servers))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, srv := range servers {
		wg.Add(1)
		go func(i int, srv *Server) {
			defer wg.Done()
			<-start
			errs[i] = srv.appendStageSupersededAudit(ctx, f.runID, f.rec())
		}(i, srv)
	}
	close(start)
	wg.Wait()
	return errs
}

// committedRows counts the committed stage_superseded_by_merge rows naming this
// fixture's stage. Read over COMMITTED STATE, not over the returned errors: a
// control that fires and is then rolled back returns an error either way, so the
// row count is what actually discriminates.
func (f *twoPoolFixture) committedRows(t *testing.T) int {
	t.Helper()
	entries, err := f.readAudit.ListForRunByCategory(context.Background(), f.runID, CategoryStageSupersededByMerge)
	if err != nil {
		t.Fatalf("list supersede rows: %v", err)
	}
	n := 0
	for _, e := range entries {
		if e != nil && e.StageID != nil && *e.StageID == f.stageID {
			n++
		}
	}
	return n
}

// dropOnceIndex removes migration 0081's index from THIS test's database. It is
// the counterfactual mutation, executed by the test rather than reasoned about.
func (f *twoPoolFixture) dropOnceIndex(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.poolA.Exec(ctx,
		"DROP INDEX "+audit.StageSupersededByMergeOnceIndex); err != nil {
		t.Fatalf("drop %s: %v", audit.StageSupersededByMergeOnceIndex, err)
	}
	// Prove the MUTATION LANDED before reading its result: a DROP that silently
	// did nothing would leave the counterfactual green for the wrong reason.
	var n int
	if err := f.poolA.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE tablename = 'audit_entries' AND indexname = $1`,
		audit.StageSupersededByMergeOnceIndex,
	).Scan(&n); err != nil {
		t.Fatalf("verify index dropped: %v", err)
	}
	if n != 0 {
		t.Fatalf("index %s is still present after DROP (count = %d); the counterfactual mutation did not land",
			audit.StageSupersededByMergeOnceIndex, n)
	}
}

// TestSupersedeAudit_PG_TwoPoolsWriteExactlyOneRow is the durable cross-process
// guarantee (#3133): two INDEPENDENT pools — two audit repositories, two Servers
// — appending the same (run, stage) supersession concurrently commit EXACTLY ONE
// row, and the loser's error is recognized as the benign already-recorded
// collision by audit.IsStageSupersededByMergeDuplicate.
//
// COUNTERFACTUAL: TestSupersedeAudit_PG_ExactlyOneRowRequiresTheIndex below runs
// this identical race with migration 0081's index DROPPED and asserts TWO rows
// land — the empirical proof that the index, and nothing else, is the control.
func TestSupersedeAudit_PG_TwoPoolsWriteExactlyOneRow(t *testing.T) {
	f := newTwoPoolFixture(t)
	errs := f.raceAppend(t)

	if got := f.committedRows(t); got != 1 {
		t.Fatalf("committed stage_superseded_by_merge rows for the stage = %d, want exactly 1 across two independent pools (two fishhawkd replicas); errors were %v", got, errs)
	}

	// Exactly one winner (nil) and one recognized duplicate. Asserting the
	// ERROR IDENTITY is meaningful here only BECAUSE the committed-state count
	// above already carries the exactly-one claim: this second assertion pins
	// that the loser is refused by the 0081 index specifically, not by some
	// other integrity failure the emitter would (correctly) have kept as a hard
	// error.
	winners, dupes := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
		case audit.IsStageSupersededByMergeDuplicate(err):
			dupes++
		default:
			t.Errorf("append %d returned %v, want nil or an audit.IsStageSupersededByMergeDuplicate-recognized error", i, err)
		}
	}
	if winners != 1 || dupes != 1 {
		t.Errorf("append outcomes = %d winner(s) / %d duplicate(s), want exactly 1 each: %v", winners, dupes, errs)
	}
}

// TestSupersedeAudit_PG_ExactlyOneRowRequiresTheIndex is the INDEX-DROPPED
// counterfactual, run inside the harness rather than asserted in prose.
//
// The assertion is the STRONG outcome the operator's binding condition demands:
// with the index dropped the same two-pool race must commit TWO rows. A weaker
// "both land OR no duplicate error was raised" would pass when the losing append
// failed for an UNRELATED reason — an audit-chain hash conflict, the (run_id,
// sequence) constraint — and would credit that as evidence the index was doing
// the work. A counterfactual that can succeed for the wrong reason proves
// nothing.
//
// If two rows do NOT land, this test FAILS LOUDLY rather than leniently: the
// exactly-one property would then be enforced by something OTHER than the index
// and the control has not been identified. That failure is informative, and the
// expected result is genuinely two: a chained append takes SELECT ... FOR UPDATE
// on the run row, so the two appends SERIALIZE — serialization means they take
// turns, not that one is refused, so absent the index the second turn writes its
// own row.
func TestSupersedeAudit_PG_ExactlyOneRowRequiresTheIndex(t *testing.T) {
	f := newTwoPoolFixture(t)
	f.dropOnceIndex(t)

	errs := f.raceAppend(t)

	got := f.committedRows(t)
	if got != 2 {
		t.Fatalf("with %s DROPPED, the two-pool race committed %d row(s) for the stage, want 2 — the index-dropped race did NOT produce a duplicate, so the exactly-one property is being enforced by something OTHER than migration 0081's index and the control has NOT been identified. Appends returned: %v (note: a chained append takes FOR UPDATE on the run row, so the two appends SERIALIZE — taking turns, not refusing each other — and two rows is the expected result)",
			audit.StageSupersededByMergeOnceIndex, got, errs)
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("with the index dropped, append %d returned %v, want nil (nothing should refuse the second write)", i, err)
		}
	}
}

// TestSupersedeAudit_PG_IndexPermitsASecondStageOfTheSameRun pins the KEY CHOICE
// (run_id, stage_id) rather than run_id alone: ONE run legitimately supersedes
// SEVERAL stages — a merge can strand an `acceptance` stage at
// awaiting_host_dispatch AND a `review` stage at awaiting_approval. A
// run_id-only key would refuse the second, legitimate supersession, and this
// test is what goes RED if the migration is ever narrowed that way.
func TestSupersedeAudit_PG_IndexPermitsASecondStageOfTheSameRun(t *testing.T) {
	ctx := context.Background()
	f := newTwoPoolFixture(t)

	if err := f.serverA.appendStageSupersededAudit(ctx, f.runID, f.rec()); err != nil {
		t.Fatalf("first stage append: %v", err)
	}
	second, err := run.NewPostgresRepository(f.poolA).CreateStage(ctx, run.CreateStageParams{
		RunID: f.runID, Sequence: 1, Type: run.StageTypeReview,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create second stage: %v", err)
	}
	if err := f.serverA.appendStageSupersededAudit(ctx, f.runID, supersededStage{
		StageID:   second.ID,
		StageType: string(run.StageTypeReview),
		FromState: string(run.StageStateAwaitingApproval),
		Reason:    supersedeReasonOperatorReconcile,
	}); err != nil {
		t.Fatalf("second stage append was REFUSED (%v); the index key must be (run_id, stage_id), not run_id alone — one run legitimately supersedes several stages", err)
	}

	entries, lerr := f.readAudit.ListForRunByCategory(ctx, f.runID, CategoryStageSupersededByMerge)
	if lerr != nil {
		t.Fatalf("list supersede rows: %v", lerr)
	}
	if len(entries) != 2 {
		t.Errorf("stage_superseded_by_merge rows for the run = %d, want 2 (one per distinct stage)", len(entries))
	}
}
