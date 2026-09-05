package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// acceptance_arbitration_pg_test.go drives POST
// /v0/runs/{run_id}/acceptance-arbitration against a REAL Postgres (#2536).
//
// A fake audit repository structurally cannot exercise what this issue is about:
// the run-row lock, the in-transaction anchor re-read and dedupe scan, the
// migration 0080 backstop index, and the out-of-transaction 23505 recovery all
// live in the store. So the whole seam is real — pgtest Postgres, the production
// run + chained audit repositories, a real orchestrator and Server.

// arbFixture is the pg-backed seam for the arbitration handler.
type arbFixture struct {
	s     *Server
	pool  *pgxpool.Pool
	audit audit.Repository
	runID uuid.UUID
	accID uuid.UUID
}

// newArbFixture seeds the canonical arbitrable state on real Postgres: a run
// whose workflow declares an acceptance stage, a terminal acceptance stage, a
// FAILED all-skip acceptance_outcome_recorded entry, and a CORRELATED paged
// acceptance_triage_decided entry above it. It returns the outcome's sequence.
func newArbFixture(t *testing.T) (*arbFixture, int64) {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	orch := &orchestrator.Orchestrator{Runs: runRepo, Audit: auditRepo}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: runRepo, AuditRepo: auditRepo, Orchestrator: orch})

	r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc",
		TriggerSource: run.TriggerCLI,
		WorkflowSpec:  []byte(autoDriveAcceptanceSpecYAML),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := runRepo.TransitionRun(ctx, r.ID, run.StateRunning); err != nil {
		t.Fatalf("run -> running: %v", err)
	}

	var accID uuid.UUID
	for i, st := range []run.StageType{run.StageTypePlan, run.StageTypeImplement, run.StageTypeAcceptance} {
		created, cerr := runRepo.CreateStage(ctx, run.CreateStageParams{
			RunID: r.ID, Sequence: i, Type: st,
			ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
		})
		if cerr != nil {
			t.Fatalf("create %s stage: %v", st, cerr)
		}
		settled := driveStageTo(t, runRepo, created, run.StageStateSucceeded)
		if st == run.StageTypeAcceptance {
			accID = settled.ID
		}
	}

	f := &arbFixture{s: s, pool: pool, audit: auditRepo, runID: r.ID, accID: accID}
	seq := f.appendOutcome(t, acceptanceVerdictFailed, 0, 3)
	f.appendTriageDecision(t)
	return f, seq
}

// appendOutcome appends one acceptance_outcome_recorded entry and returns its
// sequence — the ANCHOR the arbitration binds to.
func (f *arbFixture) appendOutcome(t *testing.T, verdict string, failed, skipped int) int64 {
	t.Helper()
	seq, err := f.tryAppendOutcome(verdict, failed, skipped)
	if err != nil {
		t.Fatalf("append acceptance outcome: %v", err)
	}
	return seq
}

// tryAppendOutcome is the t-FREE form of appendOutcome, for the concurrent tests
// that append from a spawned goroutine. The testing package requires
// FailNow/Fatalf to run on the goroutine running the test, so a fixture failure
// inside a worker must be RETURNED and reported after wg.Wait() rather than
// aborting the worker mid-flight (which can hang or misreport).
func (f *arbFixture) tryAppendOutcome(verdict string, failed, skipped int) (int64, error) {
	acc := f.accID
	p, err := json.Marshal(map[string]any{
		"run_id": f.runID.String(), "stage_id": acc.String(), "verdict": verdict,
		"criteria_failed": failed, "criteria_skipped": skipped,
	})
	if err != nil {
		return 0, fmt.Errorf("marshal acceptance outcome payload: %w", err)
	}
	e, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: f.runID, StageID: &acc, Timestamp: time.Now().UTC(),
		Category: CategoryAcceptanceOutcomeRecorded, Payload: p,
	})
	if err != nil {
		return 0, err
	}
	return e.Sequence, nil
}

// appendTriageDecision appends the correlated PAGED acceptance_triage_decided
// entry (its sequence lands above the newest outcome, as production writes it).
func (f *arbFixture) appendTriageDecision(t *testing.T) {
	t.Helper()
	acc := f.accID
	p, _ := json.Marshal(map[string]any{
		"class": acceptanceClass5, "disposition": acceptanceDispositionUnvalidatable,
	})
	if _, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: f.runID, StageID: &acc, Timestamp: time.Now().UTC(),
		Category: CategoryAcceptanceTriageDecided, Payload: p,
	}); err != nil {
		t.Fatalf("append triage decision: %v", err)
	}
}

// post drives one arbitration POST through the real handler. It is deliberately
// FailNow-free — postArbitration calls only t.Helper() — so the concurrent tests
// may call it from a spawned goroutine without violating the testing package's
// "FailNow must run on the test goroutine" contract. Keep it that way: any
// t.Fatalf reachable from here would have to move behind a t-free variant, as
// tryAppendOutcome does.
func (f *arbFixture) post(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return postArbitration(t, f.s, f.runID, acceptanceArbitrationRequest{Reason: class5Reason}, withMergeOperator)
}

// committedArbitrations returns every committed acceptance_triage_arbitrated row
// for the run, oldest first.
func (f *arbFixture) committedArbitrations(t *testing.T) []*audit.Entry {
	t.Helper()
	entries, err := f.audit.ListForRunByCategory(context.Background(), f.runID, CategoryAcceptanceTriageArbitrated)
	if err != nil {
		t.Fatalf("list arbitrations: %v", err)
	}
	return entries
}

// decodeArbitrationResponse parses a 200 body.
func decodeArbitrationResponse(t *testing.T, w *httptest.ResponseRecorder) acceptanceArbitrationResponse {
	t.Helper()
	var got acceptanceArbitrationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response %s: %v", w.Body.String(), err)
	}
	return got
}

// TestAcceptanceArbitration_PG_HappyPath is the real-store baseline: one POST
// against real Postgres records exactly one chained arbitration bound to the
// outcome, and the response reports it.
func TestAcceptanceArbitration_PG_HappyPath(t *testing.T) {
	f, seq := newArbFixture(t)

	w := f.post(t)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	got := decodeArbitrationResponse(t, w)
	if got.AlreadyRecorded {
		t.Error("already_recorded = true on the first POST")
	}
	if got.OutcomeSequence != seq {
		t.Errorf("outcome_sequence = %d, want %d", got.OutcomeSequence, seq)
	}
	rows := f.committedArbitrations(t)
	if len(rows) != 1 {
		t.Fatalf("committed arbitrations = %d, want 1", len(rows))
	}
	if got.ArbitrationSequence != rows[0].Sequence {
		t.Errorf("arbitration_sequence = %d, want the committed row's %d", got.ArbitrationSequence, rows[0].Sequence)
	}
}

// TestAcceptanceArbitration_PG_IndexCollisionRecovery is case 10(i): a GENUINE
// migration-0080 23505 driven through the handler with NO production test hook.
//
// The vehicle is a real, verified decode asymmetry. The index key is the TEXT
// expression payload->>'outcome_sequence', which yields '7' for a JSON number 7
// AND a JSON string "7"; the anchored append's in-transaction dedupe scan
// decodes into *int64 and therefore MISSES a string-typed row. So a chain-valid
// prior arbitration written with a string-typed sequence collides on the index
// while the Go scan does not see it: the insert trips 23505, BeginFunc rolls
// back, and the OUT-OF-TRANSACTION recovery — matching on the INDEX's own TEXT
// semantics in SQL, not on the typed decode that just missed it — finds the
// seeded row and the handler returns the idempotent 200.
//
// RESIDUAL: this vehicle exists only because that asymmetry exists. If a future
// change normalizes the payload type on read, the test loses its determinism and
// must be re-based on an explicit test seam.
func TestAcceptanceArbitration_PG_IndexCollisionRecovery(t *testing.T) {
	f, seq := newArbFixture(t)

	acc := f.accID
	stringTyped, _ := json.Marshal(map[string]any{
		"run_id": f.runID.String(), "reason": "seeded with a string-typed sequence",
		"outcome_sequence": fmt.Sprintf("%d", seq),
	})
	seeded, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: f.runID, StageID: &acc, Timestamp: time.Now().UTC(),
		Category: CategoryAcceptanceTriageArbitrated, Payload: stringTyped,
	})
	if err != nil {
		t.Fatalf("seed string-typed prior arbitration: %v", err)
	}

	w := f.post(t)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 via the 23505 recovery:\n%s", w.Code, w.Body.String())
	}
	got := decodeArbitrationResponse(t, w)
	if !got.AlreadyRecorded {
		t.Error("already_recorded = false, want true (the recovery reports the surviving row)")
	}
	if got.ArbitrationSequence != seeded.Sequence {
		t.Errorf("arbitration_sequence = %d, want the SEEDED entry's %d", got.ArbitrationSequence, seeded.Sequence)
	}
	if rows := f.committedArbitrations(t); len(rows) != 1 {
		t.Errorf("committed arbitrations = %d, want 1 (only the seeded row)", len(rows))
	}
}

// TestAcceptanceArbitration_PG_ConcurrentDuplicatePosts is case 10(iii) and the
// direct fix for mode 1: N concurrent identical POSTs commit EXACTLY ONE row.
// That row count is the load-bearing assertion and is asserted HARD — before
// #2536 the two controls both sat outside the append transaction, so N POSTs
// could each pass and each append.
//
// The per-RESPONSE assertion is a HARD 200 for EVERY POST, matching the wire
// contract docs/api/v0.openapi.yaml states ("two CONCURRENT first-POSTs behave
// the same way: exactly one row is committed and both responses report it").
// Reaching that required closing a third loser window in the handler: the
// idempotence fast path runs BEFORE the gate-state guard, so a loser whose
// fast-path read predated the winner's commit used to reach guard 4 after it,
// find the gate at acceptance_arbitrated, and get 409
// acceptance_arbitration_not_applicable for a discharge that IS on the chain.
// Guard 4 now re-runs the idempotence scan on an arbitrated gate and returns the
// same idempotent 200. So every response must be 200 naming the ONE committed
// row, with EXACTLY ONE of them already_recorded:false (the winner that actually
// appended) and every other one true.
func TestAcceptanceArbitration_PG_ConcurrentDuplicatePosts(t *testing.T) {
	f, _ := newArbFixture(t)

	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	bodies := make([]string, n)
	seqs := make([]int64, n)
	already := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := f.post(t)
			codes[i] = w.Code
			bodies[i] = w.Body.String()
			if w.Code == http.StatusOK {
				var got acceptanceArbitrationResponse
				_ = json.Unmarshal(w.Body.Bytes(), &got)
				seqs[i] = got.ArbitrationSequence
				already[i] = got.AlreadyRecorded
			}
		}(i)
	}
	wg.Wait()

	rows := f.committedArbitrations(t)
	if len(rows) != 1 {
		t.Fatalf("committed arbitrations = %d, want exactly 1 across %d concurrent POSTs", len(rows), n)
	}
	appended := 0
	for i := 0; i < n; i++ {
		if codes[i] != http.StatusOK {
			t.Errorf("POST %d status = %d, want 200: every concurrent duplicate POST must receive the idempotent 200 naming the surviving row:\n%s",
				i, codes[i], bodies[i])
			continue
		}
		if seqs[i] != rows[0].Sequence {
			t.Errorf("POST %d arbitration_sequence = %d, want the single committed row's %d", i, seqs[i], rows[0].Sequence)
		}
		if !already[i] {
			appended++
		}
	}
	if appended != 1 {
		t.Errorf("already_recorded:false responses = %d, want exactly 1 (only the winner appended the one committed row)", appended)
	}
}

// TestAcceptanceArbitration_ArbitrationFirstOrdering is the DETERMINISTIC pin
// for the arbitration-first ordering (binding condition 2). It orchestrates the
// ordering directly rather than racing for it:
//
//  1. arbitrate at outcome A and let it COMMIT;
//  2. THEN append outcome B (a newer acceptance_outcome_recorded entry).
//
// Both halves are then asserted. The arbitration row PERSISTS naming A — it was
// valid at append time, and the guarantee is explicitly not a permanent-newest
// property. And acceptanceGateState now resolves to acceptance_triage rather
// than acceptance_arbitrated, because the gate honours only an arbitration
// naming the NEWEST outcome: the newer outcome supersedes the discharge and
// re-wedges the gate so the operator arbitrates B. That re-wedge is the correct
// FAIL-CLOSED consequence of the legal ordering, not a defect.
func TestAcceptanceArbitration_ArbitrationFirstOrdering(t *testing.T) {
	ctx := context.Background()
	f, outcomeA := newArbFixture(t)

	// (1) Arbitrate at outcome A; let it commit.
	w := f.post(t)
	if w.Code != http.StatusOK {
		t.Fatalf("arbitration at outcome A: status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	rows := f.committedArbitrations(t)
	if len(rows) != 1 {
		t.Fatalf("committed arbitrations = %d, want 1", len(rows))
	}
	if seq, ok := arbitrationOutcomeSequence(rows[0].Payload); !ok || seq != outcomeA {
		t.Fatalf("arbitration names outcome_sequence %d (ok=%v), want A = %d", seq, ok, outcomeA)
	}

	runRow, err := f.s.cfg.RunRepo.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	stages, err := f.s.cfg.RunRepo.ListStagesForRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	before, err := f.s.acceptanceGateState(ctx, runRow, stages)
	if err != nil {
		t.Fatalf("gate state before outcome B: %v", err)
	}
	if before != acceptanceGateArbitrated {
		t.Fatalf("gate state after arbitrating A = %q, want %q", before, acceptanceGateArbitrated)
	}

	// (2) THEN a newer outcome B lands — the legal arbitration-first ordering.
	outcomeB := f.appendOutcome(t, acceptanceVerdictFailed, 0, 3)
	if outcomeB <= outcomeA {
		t.Fatalf("outcome B sequence %d must be above A %d", outcomeB, outcomeA)
	}

	// Half 1: the arbitration row PERSISTS, still naming A. It is not unwound,
	// rewritten or invalidated — it was valid at append time.
	after := f.committedArbitrations(t)
	if len(after) != 1 {
		t.Fatalf("committed arbitrations after outcome B = %d, want 1 (the row persists)", len(after))
	}
	if seq, ok := arbitrationOutcomeSequence(after[0].Payload); !ok || seq != outcomeA {
		t.Errorf("persisted arbitration names outcome_sequence %d (ok=%v), want A = %d unchanged", seq, ok, outcomeA)
	}
	if after[0].Sequence != rows[0].Sequence {
		t.Errorf("arbitration entry sequence changed: %d -> %d", rows[0].Sequence, after[0].Sequence)
	}

	// Half 2: the gate RE-WEDGES to acceptance_triage — fail-closed.
	got, err := f.s.acceptanceGateState(ctx, runRow, stages)
	if err != nil {
		t.Fatalf("gate state after outcome B: %v", err)
	}
	if got != acceptanceGateTriage {
		t.Errorf("gate state after outcome B = %q, want %q (a newer outcome supersedes the discharge and re-wedges the gate)", got, acceptanceGateTriage)
	}
}

// TestAcceptanceArbitration_PG_ConcurrentAnchorMove is the scheduling-INDEPENDENT
// invariant check over committed state. Per round it races one arbitration POST
// against one acceptance_outcome_recorded append for the same run and ACCEPTS
// BOTH legal orderings, asserting the AT-APPEND-TIME invariant in each:
//
//	for every persisted acceptance_triage_arbitrated entry E naming
//	outcome_sequence S, there is NO acceptance_outcome_recorded entry whose
//	sequence is > S and < E.Sequence
//
// i.e. no newer outcome had ALREADY committed when E was appended. Ordering A
// (outcome first) yields a 409 acceptance_outcome_superseded and no arbitration
// row; ordering B (arbitration first) yields one arbitration naming S with the
// newer outcome's sequence ABOVE E.Sequence — which satisfies the invariant and
// is a PASS, per TestAcceptanceArbitration_ArbitrationFirstOrdering.
//
// STATUS VALIDATION is load-bearing, not cosmetic: a concurrency regression that
// made every POST return 500 and append nothing would otherwise satisfy the
// invariant loop vacuously (no rows to check) and pass. So each round's status
// must be one of exactly THREE legal outcomes, and each 409 is matched on its
// specific error code before its ordering is counted:
//
//   - 200 — ordering B, the arbitration committed first;
//   - 409 acceptance_outcome_superseded — the outcome landed AFTER the handler
//     read it but BEFORE the append's in-transaction anchor re-read;
//   - 409 acceptance_arbitration_not_applicable — the outcome landed BEFORE the
//     handler's own reads, so the guards evaluate the NEW outcome, which carries
//     no correlated paged triage decision yet and is refused at guard 5. Also
//     ordering A, also fail-closed, also writes nothing — but a DIFFERENT branch,
//     so it is counted separately rather than folded into "superseded".
//
// Anything else FAILS the round. The both-orderings-observed check is a VACUITY
// GUARD on top of the deterministic pin, not the only evidence for ordering B, so
// it is reported rather than failed if a loaded runner always resolves the same
// way.
func TestAcceptanceArbitration_PG_ConcurrentAnchorMove(t *testing.T) {
	ctx := context.Background()
	const rounds = 20
	sawSuperseded, sawNotApplicable, sawArbitrated := 0, 0, 0

	for i := 0; i < rounds; i++ {
		f, _ := newArbFixture(t)
		// The two workers are NOT symmetric in cost: the append is one INSERT
		// while the POST first re-reads the run, its stages, the outcome, the
		// prior arbitrations and the triage decision. Fired simultaneously the
		// append wins essentially always, so half the rounds STAGGER it — the
		// POST gets a head start and the outcome lands inside its
		// guards-to-append window. That biases WHICH legal ordering occurs; it
		// does not weaken the invariant, which is asserted over committed state
		// and is scheduling-independent either way. The stagger is derived via
		// timescale.D so a loaded CI runner scales it with everything else.
		stagger := time.Duration(0)
		if i%2 == 1 {
			stagger = timescale.D(2 * time.Millisecond)
		}
		var wg sync.WaitGroup
		var code int
		var body string
		var appendErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			w := f.post(t)
			code, body = w.Code, w.Body.String()
		}()
		go func() {
			defer wg.Done()
			if stagger > 0 {
				time.Sleep(stagger)
			}
			// t.Fatalf is illegal off the test goroutine (testing package
			// contract): record and report after wg.Wait().
			_, appendErr = f.tryAppendOutcome(acceptanceVerdictFailed, 0, 3)
		}()
		wg.Wait()
		if appendErr != nil {
			t.Fatalf("round %d: concurrent outcome append: %v", i, appendErr)
		}

		switch {
		case code == http.StatusOK:
			sawArbitrated++
		case code == http.StatusConflict && strings.Contains(body, "acceptance_outcome_superseded"):
			sawSuperseded++
		case code == http.StatusConflict && strings.Contains(body, "acceptance_arbitration_not_applicable"):
			sawNotApplicable++
		default:
			t.Errorf("round %d: status = %d with body %s; want 200, 409 acceptance_outcome_superseded, or 409 acceptance_arbitration_not_applicable",
				i, code, body)
		}

		outcomes, err := f.audit.ListForRunByCategory(ctx, f.runID, CategoryAcceptanceOutcomeRecorded)
		if err != nil {
			t.Fatalf("round %d: list outcomes: %v", i, err)
		}
		arbs := f.committedArbitrations(t)
		if code == http.StatusConflict && len(arbs) != 0 {
			t.Errorf("round %d: 409 (%s) but %d arbitration rows committed — every refusal must write nothing", i, body, len(arbs))
		}
		if code == http.StatusOK && len(arbs) != 1 {
			t.Errorf("round %d: 200 but %d arbitration rows committed, want exactly 1", i, len(arbs))
		}
		for _, e := range arbs {
			s, ok := arbitrationOutcomeSequence(e.Payload)
			if !ok {
				t.Errorf("round %d: arbitration %s has no readable outcome_sequence", i, e.ID)
				continue
			}
			for _, o := range outcomes {
				if o.Sequence > s && o.Sequence < e.Sequence {
					t.Errorf("round %d: arbitration at sequence %d named outcome %d, but outcome %d had ALREADY committed before it — a superseded outcome was arbitrated at append time",
						i, e.Sequence, s, o.Sequence)
				}
			}
		}
	}

	t.Logf("interleavings observed across %d rounds: arbitrated(200)=%d superseded(409)=%d not_applicable(409)=%d",
		rounds, sawArbitrated, sawSuperseded, sawNotApplicable)
	// Vacuity guard, REPORTED not failed (the arbitration-first branch has its
	// own deterministic pin).
	if sawSuperseded+sawNotApplicable == 0 || sawArbitrated == 0 {
		t.Logf("only one interleaving observed across %d rounds (superseded=%d not_applicable=%d arbitrated=%d); the invariant still held, and the arbitration-first branch is pinned deterministically by TestAcceptanceArbitration_ArbitrationFirstOrdering",
			rounds, sawSuperseded, sawNotApplicable, sawArbitrated)
	}
}

// TestAcceptanceArbitration_PG_AnchorMovedRefuses is the deterministic
// anchor-moved case through the real store: a newer outcome commits BEFORE the
// POST, so the in-transaction re-read refuses with 409
// acceptance_outcome_superseded naming both sequences and writes nothing.
func TestAcceptanceArbitration_PG_AnchorMovedRefuses(t *testing.T) {
	f, stale := newArbFixture(t)
	// The fast-path idempotence scan and the guards all evaluate the NEWEST
	// outcome, so to reach the in-transaction anchor check with a STALE anchor
	// the newer outcome must land after the handler read it. Seeding it up front
	// instead exercises the guards; what this case pins is that the anchored
	// append refuses a stale anchor, so drive the primitive directly with the
	// stale sequence and assert zero committed rows.
	newest := f.appendOutcome(t, acceptanceVerdictFailed, 0, 3)
	if newest <= stale {
		t.Fatalf("newest %d must be above stale %d", newest, stale)
	}
	appender, ok := f.audit.(audit.AnchoredChainAppender)
	if !ok {
		t.Fatal("the production audit repository must implement audit.AnchoredChainAppender")
	}
	acc := f.accID
	payload, _ := json.Marshal(map[string]any{"outcome_sequence": stale, "reason": class5Reason})
	entry, err := appender.AppendChainedAnchored(context.Background(), audit.ChainAppendParams{
		RunID: f.runID, StageID: &acc, Timestamp: time.Now().UTC(),
		Category: CategoryAcceptanceTriageArbitrated, Payload: payload,
	}, audit.AnchorSpec{
		AnchorCategory: CategoryAcceptanceOutcomeRecorded, AnchorSequence: stale,
		DedupePayloadKey: "outcome_sequence", DedupeValue: stale,
		ConstraintName: audit.AcceptanceTriageArbitratedOnceIndex,
	})
	if entry != nil {
		t.Fatalf("entry = %+v, want nil on a moved anchor", entry)
	}
	// Assert the TYPED error, not merely "some error": a lock failure, a list
	// failure or a not-found would all satisfy `err != nil` and leave the
	// moved-anchor refusal unpinned at this layer.
	var moved *audit.AnchorMovedError
	if !errors.As(err, &moved) {
		t.Fatalf("err = %v (%T), want *audit.AnchorMovedError", err, err)
	}
	if moved.Expected != stale || moved.Current != newest || !moved.Recorded {
		t.Errorf("AnchorMovedError = {Expected:%d Current:%d Recorded:%v}, want {Expected:%d Current:%d Recorded:true}",
			moved.Expected, moved.Current, moved.Recorded, stale, newest)
	}
	if rows := f.committedArbitrations(t); len(rows) != 0 {
		t.Errorf("committed arbitrations = %d, want 0 (a stale anchor must write nothing)", len(rows))
	}
	// And the endpoint itself still refuses this run, DETERMINISTICALLY: the
	// fixture's only acceptance_triage_decided entry sits BELOW the newer
	// outcome, so no correlated paged disposition exists for it and guard 5
	// refuses. Asserted as an exact status + error code — the earlier
	// "200-without-already_recorded" conditional passed silently on any 4xx/5xx.
	w := f.post(t)
	if w.Code != http.StatusConflict {
		t.Fatalf("endpoint status = %d, want 409 for an untriaged newer outcome:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("acceptance_arbitration_not_applicable")) {
		t.Errorf("endpoint 409 is not acceptance_arbitration_not_applicable: %s", w.Body.String())
	}
	if rows := f.committedArbitrations(t); len(rows) != 0 {
		t.Errorf("committed arbitrations after the refused POST = %d, want 0", len(rows))
	}
}
