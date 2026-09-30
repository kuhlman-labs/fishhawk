package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/precedent"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// gate_precedent_test.go pins the E75.4 / #3732 gate seam without Postgres:
// the pure gate-class ladder, and one behavioural test per named degrade of
// gatePrecedentFor / recordPrecedentSurfaced. The cross-boundary proof (real
// index, real chain, real HTTP) is gate_precedent_pg_test.go.

// gpRunRepo serves ListStagesForRun only.
type gpRunRepo struct {
	run.Repository
	stages []*run.Stage
	err    error
	calls  int
}

func (r *gpRunRepo) ListStagesForRun(_ context.Context, _ uuid.UUID) ([]*run.Stage, error) {
	r.calls++
	return r.stages, r.err
}

// gpAudit is an in-memory chain serving the three reads/writes the gate path
// makes. Mutex-guarded per the #3226 fake-bookkeeping rule.
type gpAudit struct {
	audit.Repository
	mu        sync.Mutex
	entries   []*audit.Entry
	listErr   error
	appendErr error
	appends   int
}

func (a *gpAudit) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.appends++
	if a.appendErr != nil {
		return nil, a.appendErr
	}
	rid := p.RunID
	e := &audit.Entry{Sequence: int64(len(a.entries) + 1), RunID: &rid, StageID: p.StageID,
		Category: p.Category, Payload: p.Payload}
	a.entries = append(a.entries, e)
	return e, nil
}

func (a *gpAudit) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.listErr != nil {
		return nil, a.listErr
	}
	var out []*audit.Entry
	for _, e := range a.entries {
		if e.RunID != nil && *e.RunID == runID && e.Category == category {
			out = append(out, e)
		}
	}
	return out, nil
}

func (a *gpAudit) ListForRun(_ context.Context, _ uuid.UUID) ([]*audit.Entry, error) {
	return nil, nil
}

func (a *gpAudit) surfaced() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, e := range a.entries {
		if e.Category == CategoryPrecedentSurfaced {
			n++
		}
	}
	return n
}

const gpRepo = "acme/widgets"

func gpStage(kind run.StageType, state run.StageState) *run.Stage {
	return &run.Stage{ID: uuid.New(), Type: kind, State: state}
}

func gpRows(n int) []decisionindex.Row {
	out := make([]decisionindex.Row, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, decisionindex.Row{
			SourceSequence: int64(100 + i), SourceEntryHash: "hash", RunID: uuid.New(),
			Repo: gpRepo, DecisionClass: decisionindex.ClassPlanApproval, StageKind: "plan",
			Outcome: "approve", TouchedPaths: []string{"a/b.go"}, EscalationKeys: []string{},
			DecidedAt: time.Date(2026, 9, 1, 0, 0, i, 0, time.UTC),
		})
	}
	return out
}

type gpHarness struct {
	s     *Server
	idx   *fakePrecedentIndex
	runs  *gpRunRepo
	audit *gpAudit
	ru    *run.Run
}

// newGPHarness wires a server whose run is parked at its PLAN gate and whose
// repository has n indexed plan_approval decisions.
func newGPHarness(n int) *gpHarness {
	h := &gpHarness{
		idx: &fakePrecedentIndex{rows: gpRows(n),
			gate: decisionindex.GateContext{Repo: gpRepo, StageKind: "plan", TouchedPaths: []string{"a/c.go"}}},
		runs: &gpRunRepo{stages: []*run.Stage{
			gpStage(run.StageTypePlan, run.StageStateAwaitingApproval),
			gpStage(run.StageTypeImplement, run.StageStatePending),
		}},
		audit: &gpAudit{},
		ru:    &run.Run{ID: uuid.New(), Repo: gpRepo},
	}
	h.s = New(Config{Addr: "127.0.0.1:0", PrecedentIndex: h.idx, RunRepo: h.runs, AuditRepo: h.audit})
	return h
}

func (h *gpHarness) compute() *gatePrecedentBlock {
	return h.s.gatePrecedentFor(context.Background(), h.ru, nil, true)
}

// TestGatePrecedentClass_Ladder is the Done-means behavioural test for the
// class mapping, which no compiler enforces.
func TestGatePrecedentClass_Ladder(t *testing.T) {
	older := &concern.Concern{ID: uuid.New(), StageID: uuid.New(), StageKind: "implement",
		OriginReviewSequence: 5, Severity: "low", Category: "Style", State: concern.StateRaised}
	newest := &concern.Concern{ID: uuid.New(), StageID: uuid.New(), StageKind: "implement",
		OriginReviewSequence: 9, Severity: "high", Category: "Test_Coverage", State: concern.StateRaised}
	settled := &concern.Concern{ID: uuid.New(), StageID: uuid.New(), StageKind: "implement",
		OriginReviewSequence: 20, Severity: "low", Category: "Security", State: concern.StateWaived}
	wantCategory, _ := decisionindex.NormalizeConcernCategory("Test_Coverage")

	scope := gpStage(run.StageTypeImplement, run.StageStateAwaitingScopeDecision)
	plan := gpStage(run.StageTypePlan, run.StageStateAwaitingApproval)
	review := gpStage(run.StageTypeReview, run.StageStateAwaitingApproval)

	cases := []struct {
		name          string
		stages        []*run.Stage
		concerns      []*concern.Concern
		concernsKnown bool
		wantOK        bool
		want          gatePrecedentGate
	}{
		{"scope decision wins over a plan gate",
			[]*run.Stage{plan, scope}, nil, true, true,
			gatePrecedentGate{Class: decisionindex.ClassScopeAmendment, GateStageID: scope.ID,
				ContextStageID: scope.ID, StageKind: "implement"}},
		{"plan awaiting approval",
			[]*run.Stage{plan, gpStage(run.StageTypeImplement, run.StageStatePending)}, nil, true, true,
			gatePrecedentGate{Class: decisionindex.ClassPlanApproval, GateStageID: plan.ID,
				ContextStageID: plan.ID, StageKind: "plan"}},
		{"review with an open concern -> concern_waive on the newest",
			[]*run.Stage{gpStage(run.StageTypePlan, run.StageStateSucceeded), review},
			[]*concern.Concern{older, newest, settled}, true, true,
			gatePrecedentGate{Class: decisionindex.ClassConcernWaive, GateStageID: review.ID,
				ContextStageID: newest.StageID, StageKind: "implement", ConcernCategory: wantCategory,
				Severity: "high", AlternateClass: decisionindex.ClassConcernDefer}},
		{"review with no open concern -> merge_verdict",
			[]*run.Stage{review}, []*concern.Concern{settled}, true, true,
			gatePrecedentGate{Class: decisionindex.ClassMergeVerdict, GateStageID: review.ID,
				ContextStageID: review.ID, StageKind: ""}},
		{"review with the concern read unknown -> no class",
			[]*run.Stage{review}, nil, false, false, gatePrecedentGate{}},
		{"every stage running -> no gate",
			[]*run.Stage{gpStage(run.StageTypePlan, run.StageStateRunning),
				gpStage(run.StageTypeImplement, run.StageStateRunning)}, []*concern.Concern{newest}, true, false,
			gatePrecedentGate{}},
		{"implement awaiting approval is not a ladder arm",
			[]*run.Stage{gpStage(run.StageTypeImplement, run.StageStateAwaitingApproval)}, nil, true, false,
			gatePrecedentGate{}},
		{"no stages -> no gate", nil, nil, true, false, gatePrecedentGate{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := gatePrecedentClass(tc.stages, tc.concerns, tc.concernsKnown)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got %+v)", ok, tc.wantOK, got)
			}
			if got != tc.want {
				t.Errorf("gate = %+v\nwant  %+v", got, tc.want)
			}
		})
	}
}

// TestGatePrecedent_SurfacesBoundedAndRecordsOnce is the happy path: a bounded
// block (3 of 5 candidates), a full_query pointer, exactly one entry across
// three sequential reads.
func TestGatePrecedent_SurfacesBoundedAndRecordsOnce(t *testing.T) {
	h := newGPHarness(5)
	var first *gatePrecedentBlock
	for i := 0; i < 3; i++ {
		b := h.compute()
		if b == nil {
			t.Fatalf("read %d: nil block", i)
		}
		if first == nil {
			first = b
		} else if b.Fingerprint != first.Fingerprint {
			t.Fatalf("read %d: fingerprint moved on an unchanged index", i)
		}
	}
	if len(first.Items) != gatePrecedentMaxItems {
		t.Errorf("items = %d, want the %d-item bound", len(first.Items), gatePrecedentMaxItems)
	}
	if first.DecisionClass != "plan_approval" || first.IndexVersion != precedent.IndexVersion {
		t.Errorf("block class/version = %q/%q", first.DecisionClass, first.IndexVersion)
	}
	if first.FullQuery.Tool != gatePrecedentTool || first.FullQuery.Repo != gpRepo || first.FullQuery.Endpoint == "" {
		t.Errorf("full_query = %+v", first.FullQuery)
	}
	if h.idx.lastFilter.StageKind != "plan" || !h.idx.lastFilter.AccountScoped {
		t.Errorf("candidate filter = %+v, want the plan hard filter AND an account-scoped read", h.idx.lastFilter)
	}
	if got := h.audit.surfaced(); got != 1 {
		t.Fatalf("precedent_surfaced entries after 3 sequential reads = %d, want exactly 1", got)
	}
}

func TestGatePrecedent_PrecedentIndexUnwired(t *testing.T) {
	h := newGPHarness(3)
	h.s = New(Config{Addr: "127.0.0.1:0", RunRepo: h.runs, AuditRepo: h.audit})
	if b := h.compute(); b != nil {
		t.Fatalf("block = %+v, want nil with no precedent index", b)
	}
	if h.audit.appends != 0 || h.runs.calls != 0 {
		t.Errorf("appends=%d stage reads=%d, want 0/0 — an unwired index must cost nothing", h.audit.appends, h.runs.calls)
	}
}

func TestGatePrecedent_StageReadFails(t *testing.T) {
	h := newGPHarness(3)
	h.runs.err = errors.New("stages down")
	if b := h.compute(); b != nil {
		t.Fatalf("block = %+v, want nil when the stage read fails", b)
	}
	if h.audit.appends != 0 || h.idx.filterCalls != 0 {
		t.Errorf("appends=%d index reads=%d, want 0/0", h.audit.appends, h.idx.filterCalls)
	}
}

func TestGatePrecedent_NoOpenGate(t *testing.T) {
	h := newGPHarness(3)
	h.runs.stages = []*run.Stage{
		gpStage(run.StageTypePlan, run.StageStateRunning),
		gpStage(run.StageTypeImplement, run.StageStateRunning),
	}
	if b := h.compute(); b != nil {
		t.Fatalf("block = %+v, want nil with no open human gate (the repository HAS indexed decisions)", b)
	}
	if h.audit.appends != 0 {
		t.Errorf("appends = %d, want 0", h.audit.appends)
	}
}

func TestGatePrecedent_RunAccountNotUUID(t *testing.T) {
	h := newGPHarness(3)
	h.ru.AccountID = "not-a-uuid"
	if b := h.compute(); b != nil {
		t.Fatalf("block = %+v, want nil on a corrupt run account id (never a widened read)", b)
	}
	if h.idx.filterCalls != 0 || h.audit.appends != 0 {
		t.Errorf("index reads=%d appends=%d, want 0/0", h.idx.filterCalls, h.audit.appends)
	}
}

func TestGatePrecedent_GateContextError(t *testing.T) {
	h := newGPHarness(3)
	h.idx.gateErr = errors.New("resolve failed")
	if b := h.compute(); b != nil {
		t.Fatalf("block = %+v, want nil when GateContext fails", b)
	}
	if h.idx.filterCalls != 0 || h.audit.appends != 0 {
		t.Errorf("index reads=%d appends=%d, want 0/0", h.idx.filterCalls, h.audit.appends)
	}
}

func TestGatePrecedent_ListError(t *testing.T) {
	h := newGPHarness(3)
	h.idx.listErr = errors.New("index down")
	if b := h.compute(); b != nil {
		t.Fatalf("block = %+v, want nil when the index List fails", b)
	}
	if h.audit.appends != 0 {
		t.Errorf("appends = %d, want 0", h.audit.appends)
	}
}

func TestGatePrecedent_ZeroIndexedRows(t *testing.T) {
	h := newGPHarness(0)
	if b := h.compute(); b != nil {
		t.Fatalf("block = %+v, want nil when the repository has no indexed decision of the class", b)
	}
	if h.audit.appends != 0 {
		t.Errorf("appends = %d, want 0 (nothing was surfaced)", h.audit.appends)
	}
}

func TestGatePrecedent_AuditAppendFails(t *testing.T) {
	h := newGPHarness(3)
	h.audit.appendErr = errors.New("chain down")
	b := h.compute()
	if b == nil || len(b.Items) != 3 {
		t.Fatalf("block = %+v, want the block still surfaced when the append fails", b)
	}
	if h.audit.appends != 1 {
		t.Errorf("append attempts = %d, want 1", h.audit.appends)
	}
}

func TestGatePrecedent_NoAuditRepoStillSurfaces(t *testing.T) {
	h := newGPHarness(3)
	h.s = New(Config{Addr: "127.0.0.1:0", PrecedentIndex: h.idx, RunRepo: h.runs})
	if b := h.compute(); b == nil {
		t.Fatal("block = nil, want it surfaced with no audit repository")
	}
}

// TestGatePrecedent_DedupeReadFails pins the fail-toward-visibility posture: an
// identical entry already exists, the de-duplication READ fails, and a second
// entry is emitted anyway.
func TestGatePrecedent_DedupeReadFails(t *testing.T) {
	h := newGPHarness(3)
	if h.compute() == nil || h.audit.surfaced() != 1 {
		t.Fatal("setup: the first read must record one entry")
	}
	h.audit.listErr = errors.New("read failed")
	if h.compute() == nil {
		t.Fatal("block = nil on a dedupe read failure")
	}
	if got := h.audit.surfaced(); got != 2 {
		t.Errorf("entries = %d, want 2 — a failed de-duplication read must emit anyway", got)
	}
}

// TestGatePrecedent_ChangedAnswerRecordsAgain: a genuinely different ranked set
// on the same gate is a new fingerprint and a second entry.
func TestGatePrecedent_ChangedAnswerRecordsAgain(t *testing.T) {
	h := newGPHarness(2)
	h.compute()
	h.idx.rows = gpRows(3)
	h.compute()
	if got := h.audit.surfaced(); got != 2 {
		t.Errorf("entries = %d, want 2 after the cited set changed", got)
	}
}

// TestGatePrecedent_PayloadCarriesNoReasonProse: the chain entry cites by
// sequence/hash and never copies the query-time excerpt (ADR-082 rule 1).
func TestGatePrecedent_PayloadCarriesNoReasonProse(t *testing.T) {
	h := newGPHarness(3)
	b := h.compute()
	b.Items[0].ReasonExcerpt = "SENTINEL-REASON"
	h.audit.entries = nil
	h.s.recordPrecedentSurfaced(context.Background(), h.ru.ID, uuid.New(), b)
	if len(h.audit.entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(h.audit.entries))
	}
	raw := string(h.audit.entries[0].Payload)
	if strings.Contains(raw, "SENTINEL-REASON") || strings.Contains(raw, "reason_excerpt") {
		t.Errorf("payload carries reason prose: %s", raw)
	}
	var p precedentSurfacedPayload
	if err := json.Unmarshal(h.audit.entries[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Cited) != 3 || p.Fingerprint != b.Fingerprint || p.IndexVersion != precedent.IndexVersion {
		t.Errorf("payload = %+v", p)
	}
}
