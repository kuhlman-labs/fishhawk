package invariantmonitor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// specPR is a minimal valid workflow whose implement stage produces a
// pull_request artifact — i.e. the run intends to open a PR.
const specPR = `version: "0.3"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
      - id: review
        type: review
        executor:
          human: true
`

// specNoPR is a minimal valid workflow with NO pull_request artifact —
// a commit-yourself / non-PR workflow. A null PR is its legitimate
// normal state and must NOT be flagged.
const specNoPR = `version: "0.3"
workflows:
  commit_yourself:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
      - id: review
        type: review
        executor:
          human: true
`

// fakeRuns embeds run.BaseFake and serves a fixed run set + per-run
// stage lists.
type fakeRuns struct {
	run.BaseFake
	runs       []*run.Run
	stages     map[uuid.UUID][]*run.Stage
	listErr    error
	stagesCall int
}

func (f *fakeRuns) ListRuns(_ context.Context, _ run.ListRunsFilter) ([]*run.Run, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.runs, nil
}

func (f *fakeRuns) ListStagesForRun(_ context.Context, id uuid.UUID) ([]*run.Stage, error) {
	f.stagesCall++
	return f.stages[id], nil
}

// fakeAudit embeds audit.BaseFake and records every AppendChained call.
type fakeAudit struct {
	audit.BaseFake
	mu      sync.Mutex
	entries []audit.ChainAppendParams
}

func (f *fakeAudit) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, p)
	return &audit.Entry{}, nil
}

func (f *fakeAudit) violations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.entries {
		if e.Category == CategoryInvariantViolation {
			n++
		}
	}
	return n
}

func strptr(s string) *string { return &s }

// reviewRun builds a running run with a single review stage parked in
// awaiting_approval and the given workflow spec + PR URL.
func reviewRun(spec string, prURL *string) (*run.Run, []*run.Stage) {
	id := uuid.New()
	r := &run.Run{
		ID:             id,
		WorkflowID:     workflowKey(spec),
		WorkflowSpec:   []byte(spec),
		PullRequestURL: prURL,
		State:          run.StateRunning,
	}
	stages := []*run.Stage{
		{ID: uuid.New(), RunID: id, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval},
	}
	return r, stages
}

// workflowKey returns the single workflow id keyed in the test specs.
func workflowKey(spec string) string {
	switch spec {
	case specPR:
		return "feature_change"
	default:
		return "commit_yourself"
	}
}

func newTicker(t *testing.T, runs *fakeRuns, aud *fakeAudit, reconcile func(context.Context) (int, error)) *Ticker {
	t.Helper()
	return &Ticker{
		Runs:      runs,
		Audit:     aud,
		Reconcile: reconcile,
		Now:       func() time.Time { return time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC) },
	}
}

// TestTick_Invariant1_ReconcileInvoked asserts the auto-reconcile path
// is called once per tick.
func TestTick_Invariant1_ReconcileInvoked(t *testing.T) {
	runs := &fakeRuns{stages: map[uuid.UUID][]*run.Stage{}}
	aud := &fakeAudit{}
	var calls int
	tk := newTicker(t, runs, aud, func(context.Context) (int, error) {
		calls++
		return 0, nil
	})

	tk.Tick(context.Background())

	if calls != 1 {
		t.Fatalf("reconcile called %d times, want 1", calls)
	}
}

// TestTick_Invariant2_FlagsPRRunWithNullPR is the positive case: a
// push-and-open-pr run parked at its review gate with a null PR emits
// exactly one invariant_violation and is not mutated.
func TestTick_Invariant2_FlagsPRRunWithNullPR(t *testing.T) {
	r, stages := reviewRun(specPR, nil)
	runs := &fakeRuns{runs: []*run.Run{r}, stages: map[uuid.UUID][]*run.Stage{r.ID: stages}}
	aud := &fakeAudit{}
	tk := newTicker(t, runs, aud, nil)

	tk.Tick(context.Background())

	if got := aud.violations(); got != 1 {
		t.Fatalf("invariant_violation count = %d, want 1", got)
	}
	if aud.entries[0].RunID != r.ID {
		t.Errorf("violation RunID = %s, want %s", aud.entries[0].RunID, r.ID)
	}
}

// TestTick_Invariant2_SilentForNonPRRun is the condition-1 case: a
// non-push-and-open-pr workflow with a null PR is the LEGITIMATE normal
// state and must emit ZERO invariant_violation entries.
func TestTick_Invariant2_SilentForNonPRRun(t *testing.T) {
	r, stages := reviewRun(specNoPR, nil)
	runs := &fakeRuns{runs: []*run.Run{r}, stages: map[uuid.UUID][]*run.Stage{r.ID: stages}}
	aud := &fakeAudit{}
	tk := newTicker(t, runs, aud, nil)

	tk.Tick(context.Background())

	if got := aud.violations(); got != 0 {
		t.Fatalf("invariant_violation count = %d, want 0 (non-PR run must not be flagged)", got)
	}
}

// TestTick_Invariant2_SilentWhenPRPresent confirms a healthy PR-bearing
// run emits nothing.
func TestTick_Invariant2_SilentWhenPRPresent(t *testing.T) {
	r, stages := reviewRun(specPR, strptr("https://github.com/o/r/pull/1"))
	runs := &fakeRuns{runs: []*run.Run{r}, stages: map[uuid.UUID][]*run.Stage{r.ID: stages}}
	aud := &fakeAudit{}
	tk := newTicker(t, runs, aud, nil)

	tk.Tick(context.Background())

	if got := aud.violations(); got != 0 {
		t.Fatalf("invariant_violation count = %d, want 0 (healthy run)", got)
	}
}

// TestRun_RequiresRepos asserts the construction guards.
func TestRun_RequiresRepos(t *testing.T) {
	if err := (&Ticker{Audit: &fakeAudit{}}).Run(context.Background()); err == nil {
		t.Error("expected error when Runs is nil")
	}
	if err := (&Ticker{Runs: &fakeRuns{}}).Run(context.Background()); err == nil {
		t.Error("expected error when Audit is nil")
	}
}

// TestRun_TicksThenStopsOnCancel drives Run with an already-cancelled
// context: it fires the immediate Tick and returns nil.
func TestRun_TicksThenStopsOnCancel(t *testing.T) {
	var calls int
	tk := &Ticker{
		Runs:      &fakeRuns{stages: map[uuid.UUID][]*run.Stage{}},
		Audit:     &fakeAudit{},
		Reconcile: func(context.Context) (int, error) { calls++; return 1, nil },
		Interval:  time.Hour,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tk.Run(ctx); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
	if calls != 1 {
		t.Fatalf("immediate Tick ran reconcile %d times, want 1", calls)
	}
}

// TestTick_ReconcileError_DoesNotPanic confirms a reconcile failure is
// logged and the invariant-2 sweep still runs.
func TestTick_ReconcileError_DoesNotPanic(t *testing.T) {
	r, stages := reviewRun(specPR, nil)
	runs := &fakeRuns{runs: []*run.Run{r}, stages: map[uuid.UUID][]*run.Stage{r.ID: stages}}
	aud := &fakeAudit{}
	tk := newTicker(t, runs, aud, func(context.Context) (int, error) {
		return 0, context.DeadlineExceeded
	})

	tk.Tick(context.Background())

	if got := aud.violations(); got != 1 {
		t.Fatalf("invariant_violation count = %d, want 1 (sweep runs despite reconcile error)", got)
	}
}

// TestTick_ListRunsError_AbortsCleanly drives a ListRuns failure: the
// invariant-2 sweep is best-effort by design, so a paging error must be
// logged and the sweep must abort without panicking, without emitting any
// violation, and without proceeding to the per-run stage walk. Invariant
// 1's reconcile still runs (it precedes the sweep).
func TestTick_ListRunsError_AbortsCleanly(t *testing.T) {
	runs := &fakeRuns{
		stages:  map[uuid.UUID][]*run.Stage{},
		listErr: context.DeadlineExceeded,
	}
	aud := &fakeAudit{}
	var reconciled int
	tk := newTicker(t, runs, aud, func(context.Context) (int, error) {
		reconciled++
		return 0, nil
	})

	tk.Tick(context.Background())

	if got := aud.violations(); got != 0 {
		t.Fatalf("invariant_violation count = %d, want 0 on a list failure", got)
	}
	if runs.stagesCall != 0 {
		t.Errorf("ListStagesForRun called %d times, want 0 (sweep must abort before the per-run walk)", runs.stagesCall)
	}
	if reconciled != 1 {
		t.Errorf("reconcile ran %d times, want 1 (invariant 1 precedes the sweep)", reconciled)
	}
}

// TestRunIntendsPR_NilOrInvalidSpec keeps the monitor silent when the
// run's PR intent can't be determined.
func TestRunIntendsPR_NilOrInvalidSpec(t *testing.T) {
	if runIntendsPR(&run.Run{}) {
		t.Error("nil WorkflowSpec must not be treated as PR-intending")
	}
	if runIntendsPR(&run.Run{WorkflowSpec: []byte("not: valid: yaml: ["), WorkflowID: "x"}) {
		t.Error("unparseable WorkflowSpec must not be treated as PR-intending")
	}
	if runIntendsPR(&run.Run{WorkflowSpec: []byte(specPR), WorkflowID: "absent"}) {
		t.Error("a workflow id absent from the spec must not be treated as PR-intending")
	}
}

// TestTick_Invariant2_SilentWhenNoReviewStage confirms a PR-intending
// run whose review stage is not parked emits nothing.
func TestTick_Invariant2_SilentWhenNoReviewStage(t *testing.T) {
	r, _ := reviewRun(specPR, nil)
	// Override stages: implement still running, no parked review.
	stages := []*run.Stage{
		{ID: uuid.New(), RunID: r.ID, Type: run.StageTypeImplement, State: run.StageStateRunning},
	}
	runs := &fakeRuns{runs: []*run.Run{r}, stages: map[uuid.UUID][]*run.Stage{r.ID: stages}}
	aud := &fakeAudit{}
	tk := newTicker(t, runs, aud, nil)

	tk.Tick(context.Background())

	if got := aud.violations(); got != 0 {
		t.Fatalf("invariant_violation count = %d, want 0 (no parked review)", got)
	}
}
