package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan/planfixture"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// overlapRunRepo wraps orchestratorRepo so a test can fail ListStagesForRun
// for one run on a chosen 1-based call (the scope resolve is call 1, the
// current-stage read is call 2).
type overlapRunRepo struct {
	*orchestratorRepo
	failStagesRun  uuid.UUID
	failStagesCall int
	stagesCalls    int
}

func (r *overlapRunRepo) ListStagesForRun(ctx context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	if runID == r.failStagesRun {
		r.stagesCalls++
		if r.stagesCalls == r.failStagesCall {
			return nil, errors.New("injected stage list failure")
		}
	}
	return r.orchestratorRepo.ListStagesForRun(ctx, runID)
}

// failRunAmendmentRepo fails ListByRun for one run only.
type failRunAmendmentRepo struct {
	*fakeScopeAmendmentRepo
	failRun uuid.UUID
}

func (f *failRunAmendmentRepo) ListByRun(ctx context.Context, runID uuid.UUID) ([]*scopeamendment.Amendment, error) {
	if runID == f.failRun {
		return nil, errors.New("injected amendment list failure")
	}
	return f.fakeScopeAmendmentRepo.ListByRun(ctx, runID)
}

type overlapFixture struct {
	s   *Server
	rr  *overlapRunRepo
	art *fakeArtifactRepo
	au  *auditFake
	sa  *failRunAmendmentRepo
	log *bytes.Buffer
}

func newOverlapFixture(t *testing.T) *overlapFixture {
	t.Helper()
	f := &overlapFixture{
		rr:  &overlapRunRepo{orchestratorRepo: newOrchestratorRepo()},
		art: newFakeArtifactRepo(),
		au:  newAuditFake(),
		sa:  &failRunAmendmentRepo{fakeScopeAmendmentRepo: newFakeScopeAmendmentRepo()},
		log: &bytes.Buffer{},
	}
	f.s = New(Config{
		Addr:               "127.0.0.1:0",
		RunRepo:            f.rr,
		ArtifactRepo:       f.art,
		AuditRepo:          f.au,
		ScopeAmendmentRepo: f.sa,
		ConcernRepo:        newFakeConcernRepo(),
		Logger:             slog.New(slog.NewTextHandler(f.log, nil)),
	})
	return f
}

func overlapPlan(paths ...string) *plan.Plan {
	p := &plan.Plan{}
	for _, path := range paths {
		p.Scope.Files = append(p.Scope.Files, plan.ScopeFile{Path: path, Operation: "modify"})
	}
	return p
}

// seedPlannedRun seeds a run in state with an issue ref; a non-nil p gets a
// plan stage carrying it.
func (f *overlapFixture) seedPlannedRun(t *testing.T, state run.State, p *plan.Plan) *run.Run {
	t.Helper()
	r := f.rr.seedRun()
	r.State = state
	// Non-numeric on purpose: an all-digit id prefix would parse as an
	// issue number in guardPlanTicketReference and fail the plan upload.
	ref := "issue:run-" + r.ID.String()[:8]
	r.TriggerRef = &ref
	if p != nil {
		st := f.rr.seedStage(r.ID, 0, run.StageStateAwaitingApproval)
		seedBudgetPlanArtifact(t, f.art, st.ID, p)
	}
	return r
}

// planStageID returns the run's seeded plan stage id.
func (f *overlapFixture) planStageID(t *testing.T, runID uuid.UUID) uuid.UUID {
	t.Helper()
	stages, _ := f.rr.orchestratorRepo.ListStagesForRun(context.Background(), runID)
	if len(stages) == 0 {
		t.Fatalf("run %s has no plan stage", runID)
	}
	return stages[0].ID
}

func (f *overlapFixture) seedAmendment(t *testing.T, runID uuid.UUID, status scopeamendment.Status, paths ...string) {
	t.Helper()
	var entries []scopeamendment.PathEntry
	for _, p := range paths {
		entries = append(entries, scopeamendment.PathEntry{Path: p, Operation: "modify"})
	}
	a, err := f.sa.Create(context.Background(), scopeamendment.CreateParams{RunID: runID, StageID: uuid.New(), Paths: entries, Reason: "r"})
	if err != nil {
		t.Fatalf("seed amendment: %v", err)
	}
	f.sa.mu.Lock()
	f.sa.rows[a.ID].Status = status
	f.sa.mu.Unlock()
}

func (f *overlapFixture) compute(t *testing.T, self *run.Run) *scopeOverlapPayload {
	t.Helper()
	got, err := f.s.computeScopeOverlap(context.Background(), self)
	if err != nil {
		t.Fatalf("computeScopeOverlap: %v", err)
	}
	if got.Overlaps == nil {
		t.Fatal("Overlaps must be a non-nil array")
	}
	return got
}

func overlapFor(p *scopeOverlapPayload, runID uuid.UUID) *scopeOverlapRun {
	for i := range p.Overlaps {
		if p.Overlaps[i].RunID == runID.String() {
			return &p.Overlaps[i]
		}
	}
	return nil
}

func assertOverlapPaths(t *testing.T, p *scopeOverlapPayload, runID uuid.UUID, want ...string) {
	t.Helper()
	got := overlapFor(p, runID)
	if got == nil {
		t.Fatalf("run %s not reported; overlaps = %+v", runID, p.Overlaps)
	}
	if strings.Join(got.Paths, ",") != strings.Join(want, ",") {
		t.Fatalf("run %s paths = %v, want %v", runID, got.Paths, want)
	}
}

func assertNotReported(t *testing.T, p *scopeOverlapPayload, runID uuid.UUID, why string) {
	t.Helper()
	if got := overlapFor(p, runID); got != nil {
		t.Fatalf("%s: run %s reported with %v; overlaps = %+v", why, runID, got.Paths, p.Overlaps)
	}
}

func TestScopeOverlap_SharedPathReportedBothWays(t *testing.T) {
	f := newOverlapFixture(t)
	a := f.seedPlannedRun(t, run.StateRunning, overlapPlan("shared.go", "a.go"))
	b := f.seedPlannedRun(t, run.StatePending, overlapPlan("shared.go", "b.go"))
	c := f.seedPlannedRun(t, run.StateRunning, overlapPlan("c.go"))

	ga := f.compute(t, a)
	if !ga.SelfScopeResolved || ga.ScannedRuns != 2 || ga.UnscopedRuns != 0 || ga.Truncated {
		t.Fatalf("A payload = %+v, want resolved, scanned 2, unscoped 0, not truncated", ga)
	}
	assertOverlapPaths(t, ga, b.ID, "shared.go")
	assertNotReported(t, ga, c.ID, "disjoint scope")
	entry := overlapFor(ga, b.ID)
	if entry.IssueRef != *b.TriggerRef || entry.Stage != string(run.StageTypePlan) || entry.StageState != string(run.StageStateAwaitingApproval) {
		t.Errorf("B entry = %+v, want issue_ref %q, stage plan/awaiting_approval", entry, *b.TriggerRef)
	}
	assertOverlapPaths(t, f.compute(t, b), a.ID, "shared.go")
	if gc := f.compute(t, c); len(gc.Overlaps) != 0 {
		t.Fatalf("C overlaps = %+v, want empty", gc.Overlaps)
	}
	raw, _ := json.Marshal(f.compute(t, c))
	if !bytes.Contains(raw, []byte(`"overlaps":[]`)) {
		t.Errorf("empty overlaps must marshal as [], got %s", raw)
	}
}

func TestScopeOverlap_TerminalRunsNeverReported(t *testing.T) {
	f := newOverlapFixture(t)
	self := f.seedPlannedRun(t, run.StateRunning, overlapPlan("shared.go"))
	var terminal []*run.Run
	for _, st := range []run.State{run.StateSucceeded, run.StateFailed, run.StateCancelled} {
		terminal = append(terminal, f.seedPlannedRun(t, st, overlapPlan("shared.go")))
	}
	control := f.seedPlannedRun(t, run.StateRunning, overlapPlan("shared.go"))

	got := f.compute(t, self)
	assertOverlapPaths(t, got, control.ID, "shared.go")
	for _, r := range terminal {
		assertNotReported(t, got, r.ID, "terminal "+string(r.State))
	}
}

func TestScopeOverlap_ApprovedAmendmentOnlyOverlap(t *testing.T) {
	f := newOverlapFixture(t)
	a := f.seedPlannedRun(t, run.StateRunning, overlapPlan("a.go"))
	b := f.seedPlannedRun(t, run.StateRunning, overlapPlan("b.go"))
	f.seedAmendment(t, b.ID, scopeamendment.StatusApproved, "a.go")

	assertOverlapPaths(t, f.compute(t, a), b.ID, "a.go")
}

func TestScopeOverlap_PendingAndDeniedAmendmentsIgnored(t *testing.T) {
	f := newOverlapFixture(t)
	a := f.seedPlannedRun(t, run.StateRunning, overlapPlan("a.go", "a2.go"))
	b := f.seedPlannedRun(t, run.StateRunning, overlapPlan("b.go"))
	f.seedAmendment(t, b.ID, scopeamendment.StatusPending, "a.go")
	f.seedAmendment(t, b.ID, scopeamendment.StatusDenied, "a2.go")

	got := f.compute(t, a)
	assertNotReported(t, got, b.ID, "pending/denied amendments")
	if got.ScannedRuns != 1 {
		t.Fatalf("scanned = %d, want 1 (B scanned, not reported)", got.ScannedRuns)
	}
}

// decomposedFixture seeds parent P (sub_plans slice0[a.go], slice1[b.go]) and
// its two slice children, each carrying ParentRunID = DecomposedFrom = P and
// P's issue ref, with no plan stage of their own (they resolve P's plan by
// the parent walk).
func decomposedFixture(t *testing.T, f *overlapFixture) (p, c0, c1 *run.Run) {
	t.Helper()
	pp := overlapPlan("a.go", "b.go")
	pp.Decomposition = &plan.Decomposition{SubPlans: []plan.SubPlanSummary{
		{Title: "s0", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "a.go", Operation: "modify"}}}},
		{Title: "s1", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "b.go", Operation: "modify"}}}},
	}}
	p = f.seedPlannedRun(t, run.StateRunning, pp)
	child := func(idx int) *run.Run {
		c := f.seedPlannedRun(t, run.StateRunning, nil)
		c.DecomposedFrom, c.ParentRunID, c.SliceIndex, c.TriggerRef = &p.ID, &p.ID, &idx, p.TriggerRef
		f.rr.seedStage(c.ID, 0, run.StageStateRunning).Type = run.StageTypeImplement
		return c
	}
	return p, child(0), child(1)
}

func TestScopeOverlap_DecomposedChildSliceAttributed(t *testing.T) {
	f := newOverlapFixture(t)
	p, c0, c1 := decomposedFixture(t, f)
	r := f.seedPlannedRun(t, run.StateRunning, overlapPlan("a.go"))

	got := f.compute(t, r)
	assertOverlapPaths(t, got, c0.ID, "a.go")
	assertOverlapPaths(t, got, p.ID, "a.go")
	assertNotReported(t, got, c1.ID, "C1's slice is b.go only")
	e := overlapFor(got, c0.ID)
	if e.IssueRef != *p.TriggerRef || e.DecomposedFrom != p.ID.String() {
		t.Errorf("C0 entry = %+v, want issue_ref %q decomposed_from %s", e, *p.TriggerRef, p.ID)
	}
	if e.Stage != string(run.StageTypeImplement) || e.StageState != string(run.StageStateRunning) {
		t.Errorf("C0 stage = %s/%s, want implement/running", e.Stage, e.StageState)
	}
}

func TestScopeOverlap_DecompositionLineageExcluded(t *testing.T) {
	f := newOverlapFixture(t)
	p, c0, c1 := decomposedFixture(t, f)
	// A ParentRunID recovery pair re-executing one plan.
	orig := f.seedPlannedRun(t, run.StateRunning, overlapPlan("r.go"))
	rec := f.seedPlannedRun(t, run.StateRunning, overlapPlan("r.go"))
	rec.ParentRunID = &orig.ID

	for _, tc := range []struct {
		name string
		self *run.Run
		kin  []*run.Run
	}{
		{"parent", p, []*run.Run{c0, c1}},
		{"child0", c0, []*run.Run{p, c1}},
		{"child1", c1, []*run.Run{p, c0}},
		{"recovery original", orig, []*run.Run{rec}},
		{"recovery child", rec, []*run.Run{orig}},
	} {
		got := f.compute(t, tc.self)
		for _, k := range tc.kin {
			assertNotReported(t, got, k.ID, tc.name+": lineage")
		}
	}
}

// TestScopeOverlap_SliceRecoveryChildLineageExcluded pins approval condition 4:
// a recovery child of a decomposition slice (ParentRunID = slice, no
// DecomposedFrom) resolves the parent's FULL union scope, so without the one
// ParentRunID hop it would report its own decomposed parent and the sibling
// slice. An unrelated run sharing a path stays reported.
func TestScopeOverlap_SliceRecoveryChildLineageExcluded(t *testing.T) {
	f := newOverlapFixture(t)
	p, c0, c1 := decomposedFixture(t, f)
	rec := f.seedPlannedRun(t, run.StateRunning, nil)
	rec.ParentRunID = &c0.ID
	other := f.seedPlannedRun(t, run.StateRunning, overlapPlan("b.go"))
	// A run whose ParentRunID names a missing row: the hop read fails, so no
	// lineage is inferred and the shared path is still reported.
	ghost := uuid.New()
	orphan := f.seedPlannedRun(t, run.StateRunning, overlapPlan("a.go"))
	orphan.ParentRunID = &ghost

	got := f.compute(t, rec)
	assertNotReported(t, got, p.ID, "slice recovery vs decomposed parent")
	assertNotReported(t, got, c1.ID, "slice recovery vs sibling slice")
	assertNotReported(t, got, c0.ID, "slice recovery vs its slice")
	assertOverlapPaths(t, got, other.ID, "b.go")
	assertOverlapPaths(t, got, orphan.ID, "a.go")

	// Symmetric: the parent and the sibling never report the recovery child.
	assertNotReported(t, f.compute(t, p), rec.ID, "decomposed parent vs slice recovery")
	assertNotReported(t, f.compute(t, c1), rec.ID, "sibling slice vs slice recovery")
	assertOverlapPaths(t, f.compute(t, other), rec.ID, "b.go")
}

func TestScopeOverlap_OtherTenantNeverReported(t *testing.T) {
	f := newOverlapFixture(t)
	self := f.seedPlannedRun(t, run.StateRunning, overlapPlan("shared.go"))
	self.AccountID = "acct-a"
	foreign := f.seedPlannedRun(t, run.StateRunning, overlapPlan("shared.go"))
	foreign.AccountID = "acct-b"
	untenanted := f.seedPlannedRun(t, run.StateRunning, overlapPlan("shared.go"))

	got := f.compute(t, self)
	assertNotReported(t, got, foreign.ID, "other tenant")
	assertOverlapPaths(t, got, untenanted.ID, "shared.go")

	// An untenanted subject sees only untenanted runs.
	gotU := f.compute(t, untenanted)
	assertNotReported(t, gotU, foreign.ID, "untenanted self vs tenanted run")
	assertNotReported(t, gotU, self.ID, "untenanted self vs tenanted run")
}

func (f *overlapFixture) seedApproval(t *testing.T, runID uuid.UUID, field string, paths ...string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"decision": "approve", field: paths})
	if _, err := f.au.AppendChained(context.Background(), audit.ChainAppendParams{RunID: runID, Category: "approval_submitted", Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func TestScopeOverlap_ApprovalAddAndRemoveScopeFiles(t *testing.T) {
	f := newOverlapFixture(t)
	self := f.seedPlannedRun(t, run.StateRunning, overlapPlan("x.go", "y.go"))
	added := f.seedPlannedRun(t, run.StateRunning, overlapPlan("other.go"))
	f.seedApproval(t, added.ID, "add_scope_files", "x.go")
	removed := f.seedPlannedRun(t, run.StateRunning, overlapPlan("y.go"))
	f.seedApproval(t, removed.ID, "remove_scope_files", "y.go")

	got := f.compute(t, self)
	assertOverlapPaths(t, got, added.ID, "x.go")
	assertNotReported(t, got, removed.ID, "removed scope file")
}

func TestScopeOverlap_SelfUnresolvedNoListing(t *testing.T) {
	f := newOverlapFixture(t)
	self := f.seedPlannedRun(t, run.StateRunning, nil)
	f.seedPlannedRun(t, run.StateRunning, overlapPlan("a.go"))
	// A ListRuns call would fail; a nil error proves no listing happened.
	f.rr.listRunsErr = errors.New("ListRuns must not be called")

	got := f.compute(t, self)
	if got.SelfScopeResolved || len(got.Overlaps) != 0 || got.ScannedRuns != 0 {
		t.Fatalf("payload = %+v, want unresolved, empty, nothing scanned", got)
	}
}

func TestScopeOverlap_UnscopedCandidateCounted(t *testing.T) {
	f := newOverlapFixture(t)
	self := f.seedPlannedRun(t, run.StateRunning, overlapPlan("a.go"))
	noPlan := f.seedPlannedRun(t, run.StateRunning, nil)
	amendErr := f.seedPlannedRun(t, run.StateRunning, overlapPlan("a.go"))
	f.sa.failRun = amendErr.ID

	got := f.compute(t, self)
	if got.UnscopedRuns != 2 || got.ScannedRuns != 2 {
		t.Fatalf("scanned/unscoped = %d/%d, want 2/2", got.ScannedRuns, got.UnscopedRuns)
	}
	assertNotReported(t, got, noPlan.ID, "no plan")
	assertNotReported(t, got, amendErr.ID, "amendment list error")
}

func TestScopeOverlap_StageReadFailureKeepsEntry(t *testing.T) {
	f := newOverlapFixture(t)
	self := f.seedPlannedRun(t, run.StateRunning, overlapPlan("a.go"))
	other := f.seedPlannedRun(t, run.StateRunning, overlapPlan("a.go"))
	f.rr.failStagesRun, f.rr.failStagesCall = other.ID, 2

	got := f.compute(t, self)
	assertOverlapPaths(t, got, other.ID, "a.go")
	if e := overlapFor(got, other.ID); e.Stage != "" || e.StageState != "" {
		t.Errorf("stage = %q/%q, want empty on a stage read failure", e.Stage, e.StageState)
	}
}

// TestScopeOverlap_TruncatedAtListLimit seeds exactly scopeOverlapListLimit
// PENDING runs while self is RUNNING, so self never inflates the pending page.
func TestScopeOverlap_TruncatedAtListLimit(t *testing.T) {
	f := newOverlapFixture(t)
	self := f.seedPlannedRun(t, run.StateRunning, overlapPlan("a.go"))
	for i := 0; i < scopeOverlapListLimit; i++ {
		f.seedPlannedRun(t, run.StatePending, nil)
	}
	if got := f.compute(t, self); !got.Truncated {
		t.Fatalf("truncated = false with a full pending page (scanned %d)", got.ScannedRuns)
	}

	g := newOverlapFixture(t)
	self2 := g.seedPlannedRun(t, run.StateRunning, overlapPlan("a.go"))
	for i := 0; i < scopeOverlapListLimit-1; i++ {
		g.seedPlannedRun(t, run.StatePending, nil)
	}
	if got := g.compute(t, self2); got.Truncated {
		t.Fatal("truncated = true below the list limit")
	}
}

func TestScopeOverlap_ListRunsErrorReturned(t *testing.T) {
	f := newOverlapFixture(t)
	self := f.seedPlannedRun(t, run.StateRunning, overlapPlan("a.go"))
	f.rr.listRunsErr = errors.New("db down")
	if _, err := f.s.computeScopeOverlap(context.Background(), self); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("err = %v, want the ListRuns failure", err)
	}
}

func TestCurrentOverlapStage(t *testing.T) {
	if currentOverlapStage(nil) != nil {
		t.Fatal("empty list must yield nil")
	}
	done := &run.Stage{Sequence: 0, State: run.StageStateSucceeded}
	live1 := &run.Stage{Sequence: 1, State: run.StageStateRunning}
	live2 := &run.Stage{Sequence: 2, State: run.StageStatePending}
	if got := currentOverlapStage([]*run.Stage{live2, done, nil, live1}); got != live1 {
		t.Fatalf("got %+v, want the lowest-sequence live stage", got)
	}
	last := &run.Stage{Sequence: 3, State: run.StageStateSucceeded}
	if got := currentOverlapStage([]*run.Stage{done, last}); got != last {
		t.Fatalf("got %+v, want the last stage when all are terminal", got)
	}
}

func planScopeOverlapRows(au *auditFake, runID uuid.UUID) []planScopeOverlapAuditPayload {
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []planScopeOverlapAuditPayload
	for _, ap := range au.appended {
		if ap.Category != planScopeOverlapCategory || ap.RunID != runID {
			continue
		}
		var p planScopeOverlapAuditPayload
		_ = json.Unmarshal(ap.Payload, &p)
		out = append(out, p)
	}
	return out
}

// TestShipPlan_RecordsScopeOverlap drives the real signed ship-plan handler:
// a ship whose plan shares a path with a non-terminal run records exactly one
// plan_scope_overlap row naming it; a disjoint ship records none.
func TestShipPlan_RecordsScopeOverlap(t *testing.T) {
	for _, tc := range []struct {
		name     string
		other    string
		wantRows int
	}{
		{"overlap recorded", "a.go", 1},
		{"no overlap no row", "z.go", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOverlapFixture(t)
			sf := newSigningFake()
			f.s.cfg.SigningRepo = sf
			a := f.seedPlannedRun(t, run.StateRunning, overlapPlan(tc.other))
			b := f.seedPlannedRun(t, run.StateRunning, nil)
			st := f.rr.seedStage(b.ID, 0, run.StageStateRunning)
			priv, _ := sf.issue(t, b.ID)
			body, err := json.Marshal(planfixture.Valid()) // scope: a.go
			if err != nil {
				t.Fatal(err)
			}
			if w := shipPlanRequest(t, f.s, b.ID, st.ID, priv, body, ""); w.Code != http.StatusCreated {
				t.Fatalf("ship status = %d:\n%s", w.Code, w.Body.String())
			}
			rows := planScopeOverlapRows(f.au, b.ID)
			if len(rows) != tc.wantRows {
				t.Fatalf("plan_scope_overlap rows = %d, want %d", len(rows), tc.wantRows)
			}
			if tc.wantRows == 1 {
				if rows[0].StageID != st.ID.String() || len(rows[0].Overlaps) != 1 ||
					rows[0].Overlaps[0].RunID != a.ID.String() || strings.Join(rows[0].Overlaps[0].Paths, ",") != "a.go" {
					t.Fatalf("row = %+v, want stage %s naming %s [a.go]", rows[0], st.ID, a.ID)
				}
			}
		})
	}
}

func TestRecordPlanScopeOverlap_FailOpen(t *testing.T) {
	ctx := context.Background()
	t.Run("nil audit repo", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: newOrchestratorRepo()})
		if got := s.recordPlanScopeOverlap(ctx, uuid.New(), uuid.New()); got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
	})
	t.Run("get run error", func(t *testing.T) {
		f := newOverlapFixture(t)
		if got := f.s.recordPlanScopeOverlap(ctx, uuid.New(), uuid.New()); got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
		if !strings.Contains(f.log.String(), "plan scope overlap: get run failed") {
			t.Errorf("missing WARN: %s", f.log.String())
		}
	})
	t.Run("stage plan unreadable", func(t *testing.T) {
		f := newOverlapFixture(t)
		self := f.seedPlannedRun(t, run.StateRunning, overlapPlan("a.go"))
		f.seedPlannedRun(t, run.StateRunning, overlapPlan("a.go"))
		stageID := f.planStageID(t, self.ID)
		f.art.listErr = errors.New("artifact store down")
		got := f.s.recordPlanScopeOverlap(ctx, self.ID, stageID)
		if got == nil || got.SelfScopeResolved || len(planScopeOverlapRows(f.au, self.ID)) != 0 {
			t.Fatalf("got %+v, want unresolved and no row", got)
		}
	})
	t.Run("list runs error", func(t *testing.T) {
		f := newOverlapFixture(t)
		self := f.seedPlannedRun(t, run.StateRunning, overlapPlan("a.go"))
		f.rr.listRunsErr = errors.New("db down")
		if got := f.s.recordPlanScopeOverlap(ctx, self.ID, f.planStageID(t, self.ID)); got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
		if len(planScopeOverlapRows(f.au, self.ID)) != 0 || !strings.Contains(f.log.String(), "plan scope overlap: compute failed") {
			t.Errorf("want no row and a compute WARN; log: %s", f.log.String())
		}
	})
	t.Run("append error", func(t *testing.T) {
		f := newOverlapFixture(t)
		self := f.seedPlannedRun(t, run.StateRunning, overlapPlan("a.go"))
		f.seedPlannedRun(t, run.StateRunning, overlapPlan("a.go"))
		f.au.appendErrCategory = planScopeOverlapCategory
		got := f.s.recordPlanScopeOverlap(ctx, self.ID, f.planStageID(t, self.ID))
		if got == nil || len(got.Overlaps) != 1 {
			t.Fatalf("got %+v, want the computed overlap returned", got)
		}
		if !strings.Contains(f.log.String(), "plan scope overlap: append audit entry failed") {
			t.Errorf("missing WARN: %s", f.log.String())
		}
	})
}

func callScopeOverlap(s *Server, runIDText string, id Identity) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+runIDText+"/scope-overlap", nil)
	req.SetPathValue("run_id", runIDText)
	req = injectIdentity(req, id)
	s.handleGetRunScopeOverlap(w, req)
	return w
}

// assertNoOtherRunLeak fails when body names any other run's id, issue ref or
// path (approval condition 1).
func assertNoOtherRunLeak(t *testing.T, body string, other *run.Run, paths ...string) {
	t.Helper()
	for _, needle := range append([]string{other.ID.String(), *other.TriggerRef}, paths...) {
		if strings.Contains(body, needle) {
			t.Fatalf("run-bound caller saw %q:\n%s", needle, body)
		}
	}
}

// TestRecordPlanScopeOverlap_UnreadableStagePlan covers stagePlanForOverlap's
// schema filter and unmarshal fail-open legs: the shipped plan stage carries
// ONLY a plan artifact that is not a readable standard_v1 plan, so the self
// scope stays unresolved and no row is written, even though a live candidate
// shares the path. In (a) the Content is a VALID overlapping plan, so only the
// schema_version filter keeps it out; in (b) the Content is not JSON, so only
// the unmarshal error check keeps a zero plan (whose empty scope would still
// resolve) out. The control arm proves the fixture: the same content tagged
// standard_v1 does resolve and write the row.
func TestRecordPlanScopeOverlap_UnreadableStagePlan(t *testing.T) {
	ctx := context.Background()
	validPlan, err := json.Marshal(overlapPlan("a.go"))
	if err != nil {
		t.Fatal(err)
	}
	strp := func(s string) *string { return &s }
	cases := []struct {
		name          string
		schemaVersion *string
		content       []byte
		wantResolved  bool
		wantRows      int
	}{
		{"(a) non-standard_v1 schema", strp("standard_v0"), validPlan, false, 0},
		{"(a) nil schema", nil, validPlan, false, 0},
		{"(b) malformed standard_v1 content", strp("standard_v1"), []byte("{not json"), false, 0},
		{"control standard_v1 valid content", strp("standard_v1"), validPlan, true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newOverlapFixture(t)
			f.seedPlannedRun(t, run.StateRunning, overlapPlan("a.go"))
			self := f.seedPlannedRun(t, run.StateRunning, nil)
			st := f.rr.seedStage(self.ID, 0, run.StageStateAwaitingApproval)
			if _, err := f.art.Create(ctx, artifact.CreateParams{
				StageID:       st.ID,
				Kind:          artifact.KindPlan,
				SchemaVersion: tc.schemaVersion,
				Content:       tc.content,
				ContentHash:   "hash",
			}); err != nil {
				t.Fatalf("seed artifact: %v", err)
			}
			got := f.s.recordPlanScopeOverlap(ctx, self.ID, st.ID)
			if got == nil {
				t.Fatal("recordPlanScopeOverlap = nil, want a payload")
			}
			if got.SelfScopeResolved != tc.wantResolved {
				t.Fatalf("self_scope_resolved = %v, want %v (payload %+v)", got.SelfScopeResolved, tc.wantResolved, got)
			}
			if !tc.wantResolved && len(got.Overlaps) != 0 {
				t.Fatalf("overlaps = %+v, want none", got.Overlaps)
			}
			if rows := planScopeOverlapRows(f.au, self.ID); len(rows) != tc.wantRows {
				t.Fatalf("plan_scope_overlap rows = %d, want %d", len(rows), tc.wantRows)
			}
		})
	}
}

func TestHandleGetRunScopeOverlap(t *testing.T) {
	f := newOverlapFixture(t)
	self := f.seedPlannedRun(t, run.StateRunning, overlapPlan("shared.go"))
	other := f.seedPlannedRun(t, run.StateRunning, overlapPlan("shared.go"))
	op := gateViewReadIdentity()

	t.Run("200 payload", func(t *testing.T) {
		w := callScopeOverlap(f.s, self.ID.String(), op)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
		}
		var got scopeOverlapPayload
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.RunID != self.ID.String() || !got.SelfScopeResolved {
			t.Fatalf("payload = %+v", got)
		}
		assertOverlapPaths(t, &got, other.ID, "shared.go")
	})
	t.Run("400 bad uuid", func(t *testing.T) {
		if w := callScopeOverlap(f.s, "nope", op); w.Code != http.StatusBadRequest || !bodyHasCode(w, "validation_failed") {
			t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
		}
	})
	t.Run("404 unknown run", func(t *testing.T) {
		if w := callScopeOverlap(f.s, uuid.NewString(), op); w.Code != http.StatusNotFound || !bodyHasCode(w, "run_not_found") {
			t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
		}
	})
	t.Run("503 nil run repo", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		if w := callScopeOverlap(s, self.ID.String(), op); w.Code != http.StatusServiceUnavailable || !bodyHasCode(w, "run_repo_unconfigured") {
			t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
		}
	})
	t.Run("403 run-bound token, own and other run", func(t *testing.T) {
		for _, subject := range []string{"mcp:run:" + self.ID.String(), "mcp:run:" + uuid.NewString(), "mcp:run:malformed"} {
			// The token carries read:audit so the scope check cannot mask a
			// missing run-bound refusal: only that refusal keeps the leak out.
			w := callScopeOverlap(f.s, self.ID.String(), Identity{Subject: subject, TokenID: "tok-run", Scopes: []string{scopeGateViewRead}})
			if w.Code != http.StatusForbidden || !bodyHasCode(w, "run_token_forbidden") {
				t.Fatalf("%s: status = %d body = %s, want 403 run_token_forbidden", subject, w.Code, w.Body.String())
			}
			assertNoOtherRunLeak(t, w.Body.String(), other, "shared.go")
		}
	})
	t.Run("401 anonymous", func(t *testing.T) {
		if w := callScopeOverlap(f.s, self.ID.String(), anonIdentity()); w.Code != http.StatusUnauthorized || !bodyHasCode(w, "authentication_required") {
			t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
		}
	})
	t.Run("403 missing read scope", func(t *testing.T) {
		id := Identity{Subject: "github:op", TokenID: "tok-x", Scopes: []string{"write:runs"}}
		if w := callScopeOverlap(f.s, self.ID.String(), id); w.Code != http.StatusForbidden || !bodyHasCode(w, "insufficient_scope") {
			t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
		}
	})
	t.Run("500 get run failure", func(t *testing.T) {
		g := newOverlapFixture(t)
		g.s.cfg.RunRepo = &promptRunRepo{runErr: errors.New("db down")}
		if w := callScopeOverlap(g.s, self.ID.String(), op); w.Code != http.StatusInternalServerError || !bodyHasCode(w, "internal_error") {
			t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
		}
	})
	t.Run("500 listing failure", func(t *testing.T) {
		g := newOverlapFixture(t)
		s2 := g.seedPlannedRun(t, run.StateRunning, overlapPlan("a.go"))
		g.rr.listRunsErr = errors.New("db down")
		w := callScopeOverlap(g.s, s2.ID.String(), op)
		if w.Code != http.StatusInternalServerError || !bodyHasCode(w, "internal_error") || !strings.Contains(w.Body.String(), "list live runs for scope overlap failed") {
			t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
		}
	})
}

func TestGateView_CarriesScopeOverlap(t *testing.T) {
	decode := func(t *testing.T, w *httptest.ResponseRecorder) gateViewResponse {
		t.Helper()
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
		}
		var resp gateViewResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
	f := newOverlapFixture(t)
	self := f.seedPlannedRun(t, run.StateRunning, overlapPlan("shared.go"))
	other := f.seedPlannedRun(t, run.StateRunning, overlapPlan("shared.go"))

	t.Run("operator sees the overlap", func(t *testing.T) {
		resp := decode(t, callGateView(f.s, self.ID, "", gateViewReadIdentity()))
		if resp.ScopeOverlap == nil {
			t.Fatal("scope_overlap missing")
		}
		assertOverlapPaths(t, resp.ScopeOverlap, other.ID, "shared.go")
	})
	t.Run("run-bound caller gets no scope_overlap", func(t *testing.T) {
		w := callGateView(f.s, self.ID, "", Identity{Subject: "mcp:run:" + self.ID.String(), TokenID: "tok-run"})
		if resp := decode(t, w); resp.ScopeOverlap != nil {
			t.Fatalf("scope_overlap = %+v, want omitted for a run-bound caller", resp.ScopeOverlap)
		}
		if strings.Contains(w.Body.String(), `"scope_overlap"`) {
			t.Fatalf("scope_overlap key present:\n%s", w.Body.String())
		}
		assertNoOtherRunLeak(t, w.Body.String(), other, "shared.go")
	})
	t.Run("listing failure omits the field", func(t *testing.T) {
		g := newOverlapFixture(t)
		s2 := g.seedPlannedRun(t, run.StateRunning, overlapPlan("a.go"))
		g.rr.listRunsErr = errors.New("db down")
		if resp := decode(t, callGateView(g.s, s2.ID, "", gateViewReadIdentity())); resp.ScopeOverlap != nil {
			t.Fatalf("scope_overlap = %+v, want nil on a listing failure", resp.ScopeOverlap)
		}
		if !strings.Contains(g.log.String(), "scope overlap compute failed") {
			t.Errorf("missing WARN: %s", g.log.String())
		}
	})
}

func TestOpenAPI_ScopeOverlapRouteDocumented(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "docs", "api", "v0.openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi: %v", err)
	}
	doc := string(raw)
	for _, want := range []string{
		"/v0/runs/{run_id}/scope-overlap:",
		"operationId: getRunScopeOverlap",
		"    ScopeOverlap:",
		"    ScopeOverlapRun:",
		"run_token_forbidden",
		"scope_overlap:",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/api/v0.openapi.yaml missing %q", want)
		}
	}
}
