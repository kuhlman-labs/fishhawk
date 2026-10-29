package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/childcompletion"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/defaultbranch"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// rollupRepo is the fan-in fixture's run repository (E94.1 / #4692): the
// shared orchestratorRepo plus the awaiting_children listing the
// child-completion sweeper reads (the base fake returns none).
type rollupRepo struct {
	*orchestratorRepo
	stagesErr error
}

func (r *rollupRepo) ListStagesAwaitingChildren(context.Context) ([]*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*run.Stage
	for _, st := range r.stagesByID {
		if st.State == run.StageStateAwaitingChildren {
			out = append(out, st)
		}
	}
	return out, nil
}

func (r *rollupRepo) ListStagesForRun(ctx context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	if r.stagesErr != nil {
		return nil, r.stagesErr
	}
	return r.orchestratorRepo.ListStagesForRun(ctx, runID)
}

// failingRollupAudit fails the concern_rolled_up append only.
type failingRollupAudit struct{ *auditCompleteAuditFake }

func (a *failingRollupAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if p.Category == CategoryConcernRolledUp {
		return nil, errors.New("audit store down")
	}
	return a.auditCompleteAuditFake.AppendChained(ctx, p)
}

type rollupFixture struct {
	s           *Server
	rr          *rollupRepo
	au          *auditCompleteAuditFake
	cr          *fakeConcernRepo
	o           *orchestrator.Orchestrator
	parent      *run.Run
	impl        *run.Stage
	children    []*run.Run
	childImpl   []*run.Stage
	logs        *bytes.Buffer
	gh          *consolidateGitHub
	auditRepoIf audit.Repository
}

// newRollupFixture seeds a decomposed parent on acme-widgets/storefront (plan
// succeeded, implement awaiting_children, review pending-human) and one child
// per spec, each with its own implement stage, behind the REAL orchestrator on
// the consolidate fixture's fake forge, with a fakeConcernRepo wired. The
// concern rows are seeded directly, so the late hook cannot fire unless a test
// drives the review-verdict path.
func newRollupFixture(t *testing.T, children []childSpec, opts ...func(*rollupFixture)) *rollupFixture {
	t.Helper()
	rr := &rollupRepo{orchestratorRepo: newOrchestratorRepo()}
	f := &rollupFixture{rr: rr, au: newAuditCompleteAuditFake(), cr: newFakeConcernRepo(), logs: &bytes.Buffer{}, gh: newConsolidateGitHub()}
	f.auditRepoIf = f.au
	for _, opt := range opts {
		opt(f)
	}

	parent := rr.seedRun()
	parent.Repo = "acme-widgets/storefront"
	inst := int64(42)
	parent.InstallationID = &inst
	plan := rr.seedStage(parent.ID, 0, run.StageStateSucceeded)
	plan.Type = run.StageTypePlan
	impl := rr.seedStage(parent.ID, 1, run.StageStateAwaitingChildren)
	impl.Type = run.StageTypeImplement
	review := rr.seedStage(parent.ID, 2, run.StageStatePending)
	review.Type = run.StageTypeReview
	review.ExecutorKind = run.ExecutorHuman

	for _, cs := range children {
		c := rr.seedRun()
		c.Repo = parent.Repo
		c.DecomposedFrom = &parent.ID
		idx := cs.sliceIndex
		c.SliceIndex = &idx
		c.State = cs.state
		st := rr.seedStage(c.ID, 0, run.StageStateSucceeded)
		st.Type = run.StageTypeImplement
		f.children = append(f.children, c)
		f.childImpl = append(f.childImpl, st)
	}

	arts := newFakeArtifactRepo()
	f.o = &orchestrator.Orchestrator{Runs: rr, GitHub: f.gh, Audit: f.auditRepoIf, Artifacts: arts, DefaultBranch: defaultbranch.Static("main")}
	logger := slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.s = New(Config{RunRepo: rr, AuditRepo: f.auditRepoIf, ArtifactRepo: arts, Orchestrator: f.o, ConcernRepo: f.cr, Logger: logger})
	f.o.ConsolidatedReview = nil
	f.parent, f.impl = parent, impl
	return f
}

// seedChildConcern inserts one raised implement concern on child i.
func (f *rollupFixture) seedChildConcern(t *testing.T, i int, severity, note string) *concern.Concern {
	t.Helper()
	rows, err := f.cr.InsertRaised(context.Background(), concern.InsertRaisedParams{
		RunID: f.children[i].ID, StageID: f.childImpl[i].ID, StageKind: concern.StageKindImplement,
		ReviewerModel: "gpt-6-astra", ReviewerRole: concern.ReviewerRoleStandard, OriginReviewSequence: int64(100 + i),
		Concerns: []concern.RaisedConcern{{Severity: severity, Category: "correctness", Note: note}},
	})
	if err != nil {
		t.Fatalf("seed child concern: %v", err)
	}
	return rows[0]
}

// parentCopies returns the parent rows that are rollup copies.
func (f *rollupFixture) parentCopies(t *testing.T) []*concern.Concern {
	t.Helper()
	rows, err := f.cr.ListByRun(context.Background(), f.parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out []*concern.Concern
	for _, r := range rows {
		if r.RolledUpFrom != nil {
			out = append(out, r)
		}
	}
	return out
}

func copyOf(copies []*concern.Concern, src *concern.Concern) *concern.Concern {
	for _, c := range copies {
		if c.RolledUpFrom != nil && *c.RolledUpFrom == src.ID {
			return c
		}
	}
	return nil
}

func (f *rollupFixture) rolledUpRows(t *testing.T) []*audit.Entry {
	t.Helper()
	entries, err := f.au.ListForRunByCategory(context.Background(), f.parent.ID, CategoryConcernRolledUp)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// childCompletionIntegrator adapts the orchestrator to the sweeper's
// Integrator seam, as cmd/fishhawkd/serve.go's childCompletionAdvancer does.
type childCompletionIntegrator struct{ o *orchestrator.Orchestrator }

func (a childCompletionIntegrator) Advance(ctx context.Context, runID uuid.UUID) error {
	_, err := a.o.Advance(ctx, runID)
	return err
}

func (a childCompletionIntegrator) IntegrateSlices(ctx context.Context, parentRunID uuid.UUID) (*childcompletion.SliceConflict, error) {
	c, err := a.o.IntegrateSlices(ctx, parentRunID)
	if err != nil || c == nil {
		return nil, err
	}
	return &childcompletion.SliceConflict{SliceIndex: c.SliceIndex}, nil
}

// TestSliceConcernRollup_VerbFanInImports: POST consolidate imports a child's
// open HIGH and MEDIUM as raised parent rows on the parent implement stage,
// sets the sources carried, and writes one concern_rolled_up row.
func TestSliceConcernRollup_VerbFanInImports(t *testing.T) {
	f := newRollupFixture(t, []childSpec{{0, run.StateSucceeded}, {1, run.StateSucceeded}})
	high := f.seedChildConcern(t, 0, "high", "checkout total ignores tax")
	medium := f.seedChildConcern(t, 1, "medium", "coupon path untested")

	if w := postConsolidate(t, f.s, f.parent.ID, withAuth); w.Code != http.StatusOK {
		t.Fatalf("consolidate status = %d:\n%s", w.Code, w.Body.String())
	}
	copies := f.parentCopies(t)
	if len(copies) != 2 {
		t.Fatalf("parent copies = %d, want 2", len(copies))
	}
	for _, src := range []*concern.Concern{high, medium} {
		cp := copyOf(copies, src)
		if cp == nil {
			t.Fatalf("no parent copy of %s", src.ID)
		}
		if cp.State != concern.StateRaised || cp.StageID != f.impl.ID || cp.StageKind != concern.StageKindImplement || cp.Severity != src.Severity || cp.Note != src.Note {
			t.Errorf("copy of %s = %+v, want raised on parent implement %s with the source's severity and note", src.ID, cp, f.impl.ID)
		}
		if src.Outcome != concern.OutcomeCarried {
			t.Errorf("source %s outcome = %q, want carried", src.ID, src.Outcome)
		}
	}
	rows := f.rolledUpRows(t)
	if len(rows) != 1 {
		t.Fatalf("concern_rolled_up rows = %d, want 1", len(rows))
	}
	var p concernRolledUpPayload
	if err := json.Unmarshal(rows[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Site != concernRollupSiteFanIn || p.Count != 2 || p.High != 1 || p.Medium != 1 || p.ParentStageID != f.impl.ID.String() {
		t.Errorf("payload = %+v, want site fan_in, count 2, high 1, medium 1 on %s", p, f.impl.ID)
	}
	if rows[0].StageID == nil || *rows[0].StageID != f.impl.ID {
		t.Errorf("concern_rolled_up stage = %v, want the parent implement stage", rows[0].StageID)
	}
	for _, c := range p.Concerns {
		if c.ChildRunID == "" || c.ChildStageID == "" || c.SliceIndex == nil {
			t.Errorf("payload concern %+v is missing its child run, stage or slice index", c)
		}
	}
	if strings.Contains(string(rows[0].Payload), "checkout total ignores tax") {
		t.Error("concern_rolled_up payload carries reviewer note prose")
	}
}

// TestSliceConcernRollup_SweeperFanInImports: childcompletion.Sweeper.Tick,
// integrating through the same orchestrator, imports through the sweeper site.
func TestSliceConcernRollup_SweeperFanInImports(t *testing.T) {
	f := newRollupFixture(t, []childSpec{{0, run.StateSucceeded}})
	high := f.seedChildConcern(t, 0, "high", "inventory decrement races")
	sw := &childcompletion.Sweeper{Runs: f.rr, Audit: f.au, Advance: childCompletionIntegrator{f.o}, Integrate: childCompletionIntegrator{f.o}, Logger: slog.New(slog.NewTextHandler(f.logs, nil))}
	if err := sw.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if copyOf(f.parentCopies(t), high) == nil {
		t.Fatalf("sweeper fan-in imported no copy of the child HIGH; logs:\n%s", f.logs.String())
	}
	if n := len(f.rolledUpRows(t)); n != 1 {
		t.Errorf("concern_rolled_up rows = %d, want 1", n)
	}
}

// TestSliceConcernRollup_BetweenWaveImport: integrate-wave imports wave 0's
// child; the later full fan-in imports wave 1's child and does not re-import
// wave 0's.
func TestSliceConcernRollup_BetweenWaveImport(t *testing.T) {
	f := newRollupFixture(t, []childSpec{{0, run.StateSucceeded}, {1, run.StateRunning}})
	w0 := f.seedChildConcern(t, 0, "high", "wave 0 defect")
	if w := postIntegrateWave(t, f.s, f.parent.ID, withAuth); w.Code != http.StatusOK {
		t.Fatalf("integrate-wave status = %d:\n%s", w.Code, w.Body.String())
	}
	if got := f.parentCopies(t); len(got) != 1 || copyOf(got, w0) == nil {
		t.Fatalf("copies after the wave = %d, want exactly the wave-0 copy", len(got))
	}
	f.children[1].State = run.StateSucceeded
	w1 := f.seedChildConcern(t, 1, "medium", "wave 1 defect")
	if w := postConsolidate(t, f.s, f.parent.ID, withAuth); w.Code != http.StatusOK {
		t.Fatalf("consolidate status = %d:\n%s", w.Code, w.Body.String())
	}
	got := f.parentCopies(t)
	if len(got) != 2 || copyOf(got, w0) == nil || copyOf(got, w1) == nil {
		t.Errorf("copies after the full fan-in = %d, want one per source (2)", len(got))
	}
	if n := len(f.rolledUpRows(t)); n != 2 {
		t.Errorf("concern_rolled_up rows = %d, want 2 (one per pass that inserted)", n)
	}
}

// TestSliceConcernRollup_RerunImportsNothingTwice: the shared fan-in entry the
// sweeper and the wave verb call, run twice, then the consolidate verb, leaves
// one copy per source and one concern_rolled_up row.
func TestSliceConcernRollup_RerunImportsNothingTwice(t *testing.T) {
	f := newRollupFixture(t, []childSpec{{0, run.StateSucceeded}})
	f.seedChildConcern(t, 0, "high", "a")
	f.seedChildConcern(t, 0, "medium", "b")
	for i := 0; i < 2; i++ {
		if _, err := f.o.IntegrateSlices(context.Background(), f.parent.ID); err != nil {
			t.Fatalf("IntegrateSlices #%d: %v", i, err)
		}
	}
	if w := postConsolidate(t, f.s, f.parent.ID, withAuth); w.Code != http.StatusOK {
		t.Fatalf("consolidate status = %d:\n%s", w.Code, w.Body.String())
	}
	if n := len(f.parentCopies(t)); n != 2 {
		t.Errorf("parent copies after three fan-in passes = %d, want 2", n)
	}
	if n := len(f.rolledUpRows(t)); n != 1 {
		t.Errorf("concern_rolled_up rows = %d, want 1 (re-runs insert nothing and write no row)", n)
	}
}

// TestSliceConcernRollup_LowNotImported: a LOW child concern is not copied and
// keeps an empty outcome.
func TestSliceConcernRollup_LowNotImported(t *testing.T) {
	f := newRollupFixture(t, []childSpec{{0, run.StateSucceeded}})
	low := f.seedChildConcern(t, 0, "low", "naming nit")
	if err := f.s.ImportSliceConcerns(context.Background(), f.parent.ID, []uuid.UUID{f.children[0].ID}); err != nil {
		t.Fatalf("ImportSliceConcerns: %v", err)
	}
	if n := len(f.parentCopies(t)); n != 0 {
		t.Errorf("parent copies = %d, want 0 (LOW does not roll up)", n)
	}
	if low.Outcome != "" {
		t.Errorf("LOW outcome = %q, want empty", low.Outcome)
	}
}

// TestSliceConcernRollup_ClosedChildConcernNotImported: a waived HIGH child
// concern is not copied.
func TestSliceConcernRollup_ClosedChildConcernNotImported(t *testing.T) {
	f := newRollupFixture(t, []childSpec{{0, run.StateSucceeded}})
	waived := f.seedChildConcern(t, 0, "high", "waived by the operator")
	if _, err := f.cr.ApplyResolution(context.Background(), waived.ID, concern.StateWaived, "operator waive"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.ImportSliceConcerns(context.Background(), f.parent.ID, []uuid.UUID{f.children[0].ID}); err != nil {
		t.Fatalf("ImportSliceConcerns: %v", err)
	}
	if n := len(f.parentCopies(t)); n != 0 {
		t.Errorf("parent copies = %d, want 0 (a closed child concern does not roll up)", n)
	}
}

// TestSliceConcernRollup_FanInImportBlocksMergeLeg: after the fan-in, the
// parent's merge-eligibility leg blocks on the copied HIGH. The only HIGH
// lives on the child and is seeded directly, so the parent leg blocks only if
// the fan-in copied it.
func TestSliceConcernRollup_FanInImportBlocksMergeLeg(t *testing.T) {
	f := newRollupFixture(t, []childSpec{{0, run.StateSucceeded}})
	high := f.seedChildConcern(t, 0, "high", "refund path double-credits")
	if w := postConsolidate(t, f.s, f.parent.ID, withAuth); w.Code != http.StatusOK {
		t.Fatalf("consolidate status = %d:\n%s", w.Code, w.Body.String())
	}
	cp := copyOf(f.parentCopies(t), high)
	reason, detail, err := f.s.mergeQueueConcernLeg(context.Background(), f.parent)
	if err != nil {
		t.Fatalf("mergeQueueConcernLeg: %v", err)
	}
	if reason != mergeQueueReasonBlockingConcernOpen || cp == nil || !strings.Contains(detail, cp.ID.String()) {
		t.Errorf("merge leg = %q / %q, want blocking_concern_open naming the copy", reason, detail)
	}
}

// runChildReview drives the PRODUCTION implement-review verdict path on child
// i's implement stage: one advisory reviewer raising a HIGH.
func (f *rollupFixture) runChildReview(i int, note string, roundSeq int64) {
	rv := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApproveWithConcerns,
		Concerns: []planreview.Concern{{Severity: planreview.SeverityHigh, Category: "correctness", Note: note}}}, model: "gpt-6-astra"}
	f.s.runImplementReviewInvocations(context.Background(), f.children[i].ID, f.childImpl[i].ID,
		[]reviewerInvocation{{reviewer: rv}}, planreview.AuthorityAdvisory, "prompt", "author-model", "", "",
		planreview.DefaultReviewBudget, "", roundSeq)
}

func (f *rollupFixture) copiesWithNote(t *testing.T, note string) []*concern.Concern {
	t.Helper()
	var out []*concern.Concern
	for _, c := range f.parentCopies(t) {
		if c.Note == note {
			out = append(out, c)
		}
	}
	return out
}

// TestSliceConcernRollup_LateChildReviewImported: a child review landing on a
// SUCCEEDED child after its fan-in is imported through the review-verdict path.
func TestSliceConcernRollup_LateChildReviewImported(t *testing.T) {
	f := newRollupFixture(t, []childSpec{{0, run.StateSucceeded}})
	if w := postConsolidate(t, f.s, f.parent.ID, withAuth); w.Code != http.StatusOK {
		t.Fatalf("consolidate status = %d:\n%s", w.Code, w.Body.String())
	}
	if n := len(f.parentCopies(t)); n != 0 {
		t.Fatalf("precondition: copies before the late review = %d, want 0", n)
	}
	f.runChildReview(0, "late: stock never restored on cancel", 0)
	got := f.copiesWithNote(t, "late: stock never restored on cancel")
	if len(got) != 1 || got[0].State != concern.StateRaised {
		t.Fatalf("late copies = %d, want 1 raised copy of the late HIGH", len(got))
	}
	rows := f.rolledUpRows(t)
	if len(rows) != 1 || !strings.Contains(string(rows[0].Payload), `"site":"child_review"`) {
		t.Errorf("concern_rolled_up rows = %d, want 1 with site child_review", len(rows))
	}
}

// TestSliceConcernRollup_LateHookGates: no late import for a non-child run, a
// child not yet succeeded, or a child of a terminal parent. Each fixture's
// review raises a HIGH and the parent has an implement stage, so an import
// would be observable if the gate were absent.
func TestSliceConcernRollup_LateHookGates(t *testing.T) {
	t.Run("non-child run", func(t *testing.T) {
		f := newRollupFixture(t, []childSpec{{0, run.StateSucceeded}})
		f.children[0].DecomposedFrom = nil
		if p := panicOf(func() { f.runChildReview(0, "not a child", 0) }); p != nil {
			t.Errorf("late hook on a non-child run panicked (%v), want a silent no-op", p)
		}
		if n := len(f.parentCopies(t)); n != 0 {
			t.Errorf("copies = %d, want 0 for a non-child run", n)
		}
	})
	t.Run("child not succeeded", func(t *testing.T) {
		f := newRollupFixture(t, []childSpec{{0, run.StateRunning}})
		f.runChildReview(0, "still running", 0)
		if n := len(f.parentCopies(t)); n != 0 {
			t.Errorf("copies = %d, want 0 for a running child (the fan-in reads it)", n)
		}
	})
	t.Run("terminal parent", func(t *testing.T) {
		f := newRollupFixture(t, []childSpec{{0, run.StateSucceeded}})
		f.parent.State = run.StateSucceeded
		f.runChildReview(0, "parent already merged", 0)
		if n := len(f.parentCopies(t)); n != 0 {
			t.Errorf("copies = %d, want 0 for a terminal parent", n)
		}
	})
}

// TestSliceConcernRollup_LateHookNilGuards (approval condition C3): the late
// hook returns before any read with a nil RunRepo or ConcernRepo.
func TestSliceConcernRollup_LateHookNilGuards(t *testing.T) {
	t.Run("nil RunRepo", func(t *testing.T) {
		cr := newFakeConcernRepo()
		cr.listErr = errors.New("must not be read")
		s := New(Config{Addr: "127.0.0.1:0", ConcernRepo: cr})
		if p := panicOf(func() { s.importLateSliceConcerns(context.Background(), uuid.New()) }); p != nil {
			t.Errorf("late hook with a nil RunRepo read it (%v), want a return before any read", p)
		}
	})
	t.Run("nil ConcernRepo", func(t *testing.T) {
		rr := &readCountingRunRepo{}
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})
		s.importLateSliceConcerns(context.Background(), uuid.New())
		if n := rr.reads(); n != 0 {
			t.Errorf("run reads with a nil ConcernRepo = %d, want 0 (return before any read)", n)
		}
	})
}

// panicOf runs fn and returns what it panicked with, or nil.
func panicOf(fn func()) (p any) {
	defer func() { p = recover() }()
	fn()
	return nil
}

// readCountingRunRepo counts every run read the C3 nil guards must not reach.
type readCountingRunRepo struct {
	run.Repository
	mu sync.Mutex
	n  int
}

func (r *readCountingRunRepo) reads() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

func (r *readCountingRunRepo) GetRun(context.Context, uuid.UUID) (*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	return nil, run.ErrNotFound
}

func (r *readCountingRunRepo) ListRuns(context.Context, run.ListRunsFilter) ([]*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	return nil, nil
}

// seedRetry appends a stage_retried row on the stage at a sequence above the
// review round's, so the verdict path reads the round as superseded.
func (f *rollupFixture) seedRetry(t *testing.T, i int) int64 {
	t.Helper()
	sid := f.childImpl[i].ID
	e, err := f.au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: f.children[i].ID, StageID: &sid, Timestamp: time.Now().UTC(), Category: CategoryStageRetried, Payload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return e.Sequence
}

// TestSliceConcernRollup_RetrySupersededLateVerdictNotImported is approval
// condition C1(c): a retried, succeeded child receives its OLD detached verdict
// (its round opened before the retry) with a HIGH. Driven through the
// production review-verdict path: the parent ledger holds no copy of it and the
// parent's merge-eligibility leg is not blocked.
func TestSliceConcernRollup_RetrySupersededLateVerdictNotImported(t *testing.T) {
	f := newRollupFixture(t, []childSpec{{0, run.StateSucceeded}})
	sid := f.childImpl[0].ID
	started, err := f.au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: f.children[0].ID, StageID: &sid, Timestamp: time.Now().UTC(), Category: "implement_review_started", Payload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.seedRetry(t, 0)
	f.runChildReview(0, "old attempt: discarded tree", started.Sequence)

	if got := f.copiesWithNote(t, "old attempt: discarded tree"); len(got) != 0 {
		t.Errorf("parent copies of the retry-superseded verdict = %d (states %v), want 0: a discarded attempt's concern must never reach the parent", len(got), copyStates(got))
	}
	reason, detail, err := f.s.mergeQueueConcernLeg(context.Background(), f.parent)
	if err != nil {
		t.Fatalf("mergeQueueConcernLeg: %v", err)
	}
	if reason != "" {
		t.Errorf("parent merge leg = %q / %q, want unblocked", reason, detail)
	}
}

func copyStates(cs []*concern.Concern) []concern.State {
	var out []concern.State
	for _, c := range cs {
		out = append(out, c.State)
	}
	return out
}

// TestSliceConcernRollup_RetrySweepSupersedesCopy is approval condition C1(b):
// when a retry sweep supersedes a child concern that already has a parent copy,
// the copy is superseded in the same pass with a reason naming the child
// supersession, so it no longer blocks the parent's merge leg.
func TestSliceConcernRollup_RetrySweepSupersedesCopy(t *testing.T) {
	f := newRollupFixture(t, []childSpec{{0, run.StateSucceeded}})
	high := f.seedChildConcern(t, 0, "high", "swept by a retry")
	if err := f.s.ImportSliceConcerns(context.Background(), f.parent.ID, []uuid.UUID{f.children[0].ID}); err != nil {
		t.Fatal(err)
	}
	cp := copyOf(f.parentCopies(t), high)
	if cp == nil || cp.State != concern.StateRaised {
		t.Fatalf("precondition: raised parent copy, got %+v", cp)
	}
	f.s.supersedeOpenImplementConcerns(context.Background(), f.children[0].ID, f.childImpl[0].ID, "superseded by stage retry (retry seq 9)", nil)
	if high.State != concern.StateSuperseded {
		t.Fatalf("precondition: child source state = %s, want superseded", high.State)
	}
	if cp.State != concern.StateSuperseded {
		t.Errorf("parent copy state = %s, want superseded with its source", cp.State)
	}
	if !strings.Contains(cp.StateReason, high.ID.String()) || !strings.Contains(cp.StateReason, "superseded by stage retry") {
		t.Errorf("parent copy state_reason = %q, want it to name the child concern and the child supersession", cp.StateReason)
	}
	if reason, detail, _ := f.s.mergeQueueConcernLeg(context.Background(), f.parent); reason != "" {
		t.Errorf("parent merge leg = %q / %q, want unblocked once the copy is superseded", reason, detail)
	}
}

// TestSliceConcernRollup_ImportFailureModes: one subtest per failure mode.
func TestSliceConcernRollup_ImportFailureModes(t *testing.T) {
	t.Run("nil ConcernRepo is a no-op", func(t *testing.T) {
		rr := &prEventsRunRepo{stagesErr: errors.New("must not be read")}
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})
		if err := s.ImportSliceConcerns(context.Background(), uuid.New(), []uuid.UUID{uuid.New()}); err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	})
	t.Run("parent with no implement stage", func(t *testing.T) {
		f := newRollupFixture(t, []childSpec{{0, run.StateSucceeded}})
		f.seedChildConcern(t, 0, "high", "x")
		f.impl.Type = run.StageTypeReview
		err := f.s.ImportSliceConcerns(context.Background(), f.parent.ID, []uuid.UUID{f.children[0].ID})
		if err == nil || !strings.Contains(err.Error(), "no implement stage") {
			t.Errorf("err = %v, want a no-implement-stage error", err)
		}
	})
	t.Run("parent stage list error", func(t *testing.T) {
		f := newRollupFixture(t, []childSpec{{0, run.StateSucceeded}})
		f.rr.stagesErr = errors.New("stages unreadable")
		if err := f.s.ImportSliceConcerns(context.Background(), f.parent.ID, []uuid.UUID{f.children[0].ID}); err == nil {
			t.Error("err = nil, want the stage-list error")
		}
	})
	t.Run("child list error fails the verb with no slices_integrated", func(t *testing.T) {
		f := newRollupFixture(t, []childSpec{{0, run.StateSucceeded}})
		f.cr.listErr = errors.New("concern store down")
		w := postConsolidate(t, f.s, f.parent.ID, withAuth)
		if w.Code == http.StatusOK {
			t.Errorf("consolidate status = 200, want a failure:\n%s", w.Body.String())
		}
		if n := consolidateAuditCount(t, f.au, f.parent.ID, "slices_integrated"); n != 0 {
			t.Errorf("slices_integrated rows = %d, want 0", n)
		}
		if f.impl.State != run.StageStateAwaitingChildren {
			t.Errorf("parent implement = %s, want still awaiting_children", f.impl.State)
		}
	})
	t.Run("ImportRolledUp error", func(t *testing.T) {
		f := newRollupFixture(t, []childSpec{{0, run.StateSucceeded}})
		f.seedChildConcern(t, 0, "high", "x")
		f.cr.importErr = errors.New("insert failed")
		if err := f.s.ImportSliceConcerns(context.Background(), f.parent.ID, []uuid.UUID{f.children[0].ID}); err == nil {
			t.Error("err = nil, want the import error")
		}
	})
	t.Run("audit append failure keeps the copies and returns nil", func(t *testing.T) {
		f := newRollupFixture(t, []childSpec{{0, run.StateSucceeded}}, func(f *rollupFixture) {
			f.auditRepoIf = &failingRollupAudit{f.au}
		})
		f.seedChildConcern(t, 0, "high", "x")
		if err := f.s.ImportSliceConcerns(context.Background(), f.parent.ID, []uuid.UUID{f.children[0].ID}); err != nil {
			t.Fatalf("err = %v, want nil (the copies are durable)", err)
		}
		if n := len(f.parentCopies(t)); n != 1 {
			t.Errorf("copies = %d, want 1", n)
		}
		logs := f.logs.String()
		if !strings.Contains(logs, "level=ERROR") || !strings.Contains(logs, "event="+concernRolledUpAuditLostEvent) {
			t.Errorf("want an ERROR log keyed %s, got:\n%s", concernRolledUpAuditLostEvent, logs)
		}
	})
}

// mergeStampFixture is a merged-poll fixture (the prEvents fakes) with a
// decomposition child and a wired concern store.
func mergeStampFixture(t *testing.T) (*Server, *prEventsRunRepo, *prEventsAuditRepo, *fakeConcernRepo, uuid.UUID, uuid.UUID, *bytes.Buffer) {
	t.Helper()
	runID, childID, reviewStageID := uuid.New(), uuid.New(), uuid.New()
	prURL := "https://github.com/acme-widgets/storefront/pull/7"
	rr := &prEventsRunRepo{
		listResult:       []*run.Run{{ID: runID, Repo: "acme-widgets/storefront", PullRequestURL: &prURL}},
		decomposedResult: []*run.Run{{ID: childID, Repo: "acme-widgets/storefront", DecomposedFrom: &runID}},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {{ID: reviewStageID, RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval}},
		},
	}
	ar := &prEventsAuditRepo{}
	cr := newFakeConcernRepo()
	logs := &bytes.Buffer{}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: ar, ConcernRepo: cr, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	return s, rr, ar, cr, runID, childID, logs
}

func seedRow(t *testing.T, cr *fakeConcernRepo, runID uuid.UUID, severity string, to ...concern.State) *concern.Concern {
	t.Helper()
	c := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "n")
	c.Severity = severity
	for _, st := range to {
		var err error
		if st == concern.StateAddressedPending {
			err = cr.MarkAddressedPending(context.Background(), []uuid.UUID{c.ID}, "routed")
		} else {
			_, err = cr.ApplyResolution(context.Background(), c.ID, st, "x")
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// TestSliceConcernRollup_MergeStampsOpenAtMerge: a merged poll stamps the
// parent's open and superseded rows and the child's open LOW; a carried child
// row keeps carried; a waived row stays empty.
func TestSliceConcernRollup_MergeStampsOpenAtMerge(t *testing.T) {
	s, _, _, cr, runID, childID, _ := mergeStampFixture(t)
	open := seedRow(t, cr, runID, "high")
	superseded := seedRow(t, cr, runID, "medium", concern.StateSuperseded)
	waived := seedRow(t, cr, runID, "high", concern.StateWaived)
	childLow := seedRow(t, cr, childID, "low")
	carried := seedRow(t, cr, childID, "high")
	carried.Outcome = concern.OutcomeCarried

	if err := s.ResolveReviewFromPollState(context.Background(), runID, true, "https://github.com/acme-widgets/storefront/pull/7"); err != nil {
		t.Fatalf("ResolveReviewFromPollState: %v", err)
	}
	for _, c := range []*concern.Concern{open, superseded, childLow} {
		if c.Outcome != concern.OutcomeOpenAtMerge {
			t.Errorf("%s (%s, run %s) outcome = %q, want open_at_merge", c.ID, c.State, c.RunID, c.Outcome)
		}
	}
	if carried.Outcome != concern.OutcomeCarried {
		t.Errorf("carried child row outcome = %q, want carried", carried.Outcome)
	}
	if waived.Outcome != "" {
		t.Errorf("waived row outcome = %q, want empty", waived.Outcome)
	}
}

// TestSliceConcernRollup_MergeStampFailureDoesNotUnwind: a stamp store error
// still writes pr_merged and succeeds the review stage, and logs at WARN with
// the stable key (approval condition C5).
func TestSliceConcernRollup_MergeStampFailureDoesNotUnwind(t *testing.T) {
	s, rr, ar, cr, runID, _, logs := mergeStampFixture(t)
	cr.stampErr = errors.New("stamp failed")
	if err := s.ResolveReviewFromPollState(context.Background(), runID, true, "https://github.com/acme-widgets/storefront/pull/7"); err != nil {
		t.Fatalf("ResolveReviewFromPollState: %v", err)
	}
	if findCategory(ar.appended, CategoryPRMerged) == nil {
		t.Error("pr_merged row missing after a stamp failure")
	}
	if len(rr.transitions) != 1 || rr.transitions[0].To != run.StageStateSucceeded {
		t.Errorf("transitions = %+v, want the review stage succeeded", rr.transitions)
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "event="+concernOpenAtMergeStampFailedEvent) || !strings.Contains(out, "run_id="+runID.String()) {
		t.Errorf("want a WARN keyed %s naming the run, got:\n%s", concernOpenAtMergeStampFailedEvent, out)
	}
}

// TestSliceConcernRollup_MergeStampNilGuards (approval condition C3): the
// merge stamp returns before any read with a nil ConcernRepo or RunRepo.
func TestSliceConcernRollup_MergeStampNilGuards(t *testing.T) {
	t.Run("nil ConcernRepo", func(t *testing.T) {
		rr := &readCountingRunRepo{}
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})
		func() {
			defer func() { _ = recover() }() // a guardless stamp dereferences the nil store AFTER its read
			s.stampConcernsOpenAtMerge(context.Background(), uuid.New())
		}()
		if n := rr.reads(); n != 0 {
			t.Errorf("run reads with a nil ConcernRepo = %d, want 0 (return before any read)", n)
		}
	})
	t.Run("nil RunRepo", func(t *testing.T) {
		cr := newFakeConcernRepo()
		c := seedRow(t, cr, uuid.New(), "high")
		s := New(Config{Addr: "127.0.0.1:0", ConcernRepo: cr})
		if p := panicOf(func() { s.stampConcernsOpenAtMerge(context.Background(), c.RunID) }); p != nil {
			t.Errorf("merge stamp with a nil RunRepo read it (%v), want a return before any read", p)
		}
		if c.Outcome != "" {
			t.Errorf("outcome = %q, want empty: a nil RunRepo returns before any write", c.Outcome)
		}
	})
}

// TestSliceConcernRollup_ReconcileMergeStamps: the reconcile-merge recovery
// verb stamps open_at_merge.
func TestSliceConcernRollup_ReconcileMergeStamps(t *testing.T) {
	f := newSupersedeFixture(t, parkedShape())
	cr := newFakeConcernRepo()
	f.s.cfg.ConcernRepo = cr
	open := seedRow(t, cr, f.runID, "high")
	f.observeMerge(t)
	if w := f.postReconcile(t); w.Code != http.StatusOK {
		t.Fatalf("reconcile status = %d:\n%s", w.Code, w.Body.String())
	}
	if open.Outcome != concern.OutcomeOpenAtMerge {
		t.Errorf("outcome after reconcile-merge = %q, want open_at_merge", open.Outcome)
	}
}

// TestSliceConcernRollup_ImportedConcernInReviewContextOnly: the parent's
// review context lists the copy as an ordinary open concern and carries none
// of the rollup vocabulary.
func TestSliceConcernRollup_ImportedConcernInReviewContextOnly(t *testing.T) {
	f := newRollupFixture(t, []childSpec{{0, run.StateSucceeded}})
	high := f.seedChildConcern(t, 0, "high", "shipping fee rounding")
	if err := f.s.ImportSliceConcerns(context.Background(), f.parent.ID, []uuid.UUID{f.children[0].ID}); err != nil {
		t.Fatal(err)
	}
	cp := copyOf(f.parentCopies(t), high)
	prior := f.s.priorConcernsForReview(context.Background(), f.parent.ID, f.impl.ID)
	found := false
	for _, p := range prior {
		if p.ID == cp.ID.String() && p.State == string(concern.StateRaised) && strings.Contains(p.Note, "shipping fee rounding") {
			found = true
		}
	}
	if !found {
		t.Errorf("prior concerns %+v do not list the copy %s as an open concern", prior, cp.ID)
	}
	b, _ := json.Marshal(prior)
	for _, bad := range []string{"carried", "open_at_merge", "rolled_up_from"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("review context carries %q: %s", bad, b)
		}
	}
}

// TestSliceConcernRollup_CopyCheckKeySuppressesParentRaise is approval
// condition C4: a copy keeps check_key verbatim, so a parent permission-drift
// raise for the same check_key after the import adds no second row.
func TestSliceConcernRollup_CopyCheckKeySuppressesParentRaise(t *testing.T) {
	f := newRollupFixture(t, []childSpec{{0, run.StateSucceeded}})
	const key = "permission_drift:surface:path:key:after"
	rows, err := f.cr.InsertRaised(context.Background(), concern.InsertRaisedParams{
		RunID: f.children[0].ID, StageID: f.childImpl[0].ID, StageKind: concern.StageKindImplement,
		Provenance: concern.ProvenanceServerCheck, OriginReviewSequence: 3,
		Concerns: []concern.RaisedConcern{{Severity: "high", Category: "permission_drift", Note: "widened", CheckKey: key}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.ImportSliceConcerns(context.Background(), f.parent.ID, []uuid.UUID{f.children[0].ID}); err != nil {
		t.Fatal(err)
	}
	cp := copyOf(f.parentCopies(t), rows[0])
	if cp == nil || cp.CheckKey != key || !cp.IsServerCheck() {
		t.Fatalf("copy = %+v, want check_key %q and server_check provenance verbatim", cp, key)
	}
	before, _ := f.cr.ListByRun(context.Background(), f.parent.ID)
	f.s.raisePermissionDrift(context.Background(), permissionDriftRequest{RunID: f.parent.ID, StageID: f.impl.ID, Base: "b", Head: "h", Trigger: "consolidated_review"},
		permissionDriftFindings{widenings: []permissionDriftWidening{{Surface: "s", Path: "p", Key: "k", Severity: "high", CheckKey: key}}})
	after, _ := f.cr.ListByRun(context.Background(), f.parent.ID)
	if len(after) != len(before) {
		t.Errorf("parent rows %d -> %d, want no second row for check_key %q", len(before), len(after), key)
	}
}

// TestSliceConcernRollup_PostgresCrossLayer crosses server -> concern store ->
// Postgres -> merge leg and the issue-comment renderer, with the real run,
// concern and audit Postgres repositories: the import runs twice, the merge leg
// blocks on the HIGH copy, the merge stamp reaches the child LOW only, the
// audit chain holds exactly one concern_rolled_up row, and that PERSISTED row,
// read back from the audit store, renders the promised count line through the
// production status-comment entry point (approval condition C2).
func TestSliceConcernRollup_PostgresCrossLayer(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	concernRepo := concern.NewPostgresRepository(pool)
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: runRepo, AuditRepo: auditRepo, ConcernRepo: concernRepo})

	mkRun := func(parent *uuid.UUID) (*run.Run, *run.Stage) {
		r, err := runRepo.CreateRun(ctx, run.CreateRunParams{Repo: "acme-widgets/storefront", WorkflowID: "feature_change", WorkflowSHA: "cafef00d", TriggerSource: run.TriggerCLI, DecomposedFrom: parent})
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		st, err := runRepo.CreateStage(ctx, run.CreateStageParams{RunID: r.ID, Sequence: 1, Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "fishhawk/runner@v1"})
		if err != nil {
			t.Fatalf("create stage: %v", err)
		}
		return r, st
	}
	parent, parentImpl := mkRun(nil)
	child, childImpl := mkRun(&parent.ID)
	if child.DecomposedFrom == nil {
		t.Fatal("precondition: child is not a decomposition child")
	}
	seeded, err := concernRepo.InsertRaised(ctx, concern.InsertRaisedParams{
		RunID: child.ID, StageID: childImpl.ID, StageKind: concern.StageKindImplement, ReviewerModel: "gpt-6-astra", OriginReviewSequence: 1,
		Concerns: []concern.RaisedConcern{{Severity: "high", Note: "h"}, {Severity: "medium", Note: "m"}, {Severity: "low", Note: "l"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.ImportSliceConcerns(ctx, parent.ID, []uuid.UUID{child.ID}); err != nil {
			t.Fatalf("ImportSliceConcerns #%d: %v", i, err)
		}
	}
	parentRows, err := concernRepo.ListByRun(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	var highCopy *concern.Concern
	for _, r := range parentRows {
		if r.RolledUpFrom != nil && *r.RolledUpFrom == seeded[0].ID {
			highCopy = r
		}
	}
	if len(parentRows) != 2 || highCopy == nil || highCopy.StageID != parentImpl.ID {
		t.Fatalf("parent rows = %d (high copy %v), want 2 copies with the HIGH on the parent implement stage", len(parentRows), highCopy)
	}
	reason, detail, err := s.mergeQueueConcernLeg(ctx, parent)
	if err != nil || reason != mergeQueueReasonBlockingConcernOpen || !strings.Contains(detail, highCopy.ID.String()) {
		t.Errorf("merge leg = %q / %q / %v, want blocking on the HIGH copy %s", reason, detail, err, highCopy.ID)
	}

	s.stampConcernsOpenAtMerge(ctx, parent.ID)
	childRows, err := concernRepo.ListByRun(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range childRows {
		want := concern.OutcomeCarried
		if r.Severity == "low" {
			want = concern.OutcomeOpenAtMerge
		}
		if r.Outcome != want {
			t.Errorf("child %s row outcome = %q, want %q", r.Severity, r.Outcome, want)
		}
	}

	entries, err := auditRepo.ListForRunByCategory(ctx, parent.ID, CategoryConcernRolledUp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("concern_rolled_up rows on the parent chain = %d, want 1", len(entries))
	}
	stages, err := runRepo.ListStagesForRun(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	body := issuecomment.RenderStatusBody(parent, stages, entries, "https://fishhawk.example", time.Now())
	if !strings.Contains(body, "Child-slice concerns imported into the parent: 2 (1 high, 1 medium)") {
		t.Errorf("rendered status body lacks the count line:\n%s", body)
	}
}
