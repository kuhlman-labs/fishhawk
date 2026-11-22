package server

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ---------------------------------------------------------------------
// Fakes (V5f: every stall-watch fake lives in this file).
// ---------------------------------------------------------------------

// stallRunRepo is an in-memory run.Repository honouring the ListRuns filters
// the stall watch uses, in created_at DESC order like the Postgres query.
type stallRunRepo struct {
	run.BaseFake
	mu      sync.Mutex
	runs    map[uuid.UUID]*run.Run
	stages  map[uuid.UUID][]*run.Stage
	listErr error
}

func newStallRunRepo() *stallRunRepo {
	return &stallRunRepo{runs: map[uuid.UUID]*run.Run{}, stages: map[uuid.UUID][]*run.Stage{}}
}

func (f *stallRunRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	cp := *r
	return &cp, nil
}

func (f *stallRunRepo) ListStagesForRun(_ context.Context, id uuid.UUID) ([]*run.Stage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*run.Stage, 0, len(f.stages[id]))
	for _, st := range f.stages[id] {
		cp := *st
		out = append(out, &cp)
	}
	return out, nil
}

func (f *stallRunRepo) ListRuns(_ context.Context, flt run.ListRunsFilter) ([]*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var all []*run.Run
	for _, r := range f.runs {
		switch {
		case flt.NonTerminal && r.State.IsTerminal():
			continue
		case flt.State != "" && string(r.State) != flt.State:
			continue
		case flt.Repo != "" && r.Repo != flt.Repo:
			continue
		case flt.TriggerRef != nil && (r.TriggerRef == nil || *r.TriggerRef != *flt.TriggerRef):
			continue
		case flt.ParentRunID != nil && (r.ParentRunID == nil || *r.ParentRunID != *flt.ParentRunID):
			continue
		case flt.DecomposedFrom != nil && (r.DecomposedFrom == nil || *r.DecomposedFrom != *flt.DecomposedFrom):
			continue
		}
		cp := *r
		all = append(all, &cp)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
	if flt.Offset >= len(all) {
		return nil, nil
	}
	all = all[flt.Offset:]
	if flt.Limit > 0 && len(all) > flt.Limit {
		all = all[:flt.Limit]
	}
	return all, nil
}

// stallAuditRepo is an in-memory audit.Repository WITHOUT the
// DedupedChainAppender capability, so the list-then-append fallback is the
// only dedup in the path.
type stallAuditRepo struct {
	audit.BaseFake
	mu          sync.Mutex
	entries     []*audit.Entry
	failListFor map[uuid.UUID]bool
	failByCat   string
}

func (f *stallAuditRepo) seed(runID uuid.UUID, stageID *uuid.UUID, category string, kind audit.ActorKind, ts time.Time, payload any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := json.Marshal(payload)
	r := runID
	k := kind
	f.entries = append(f.entries, &audit.Entry{ID: uuid.New(), Sequence: int64(len(f.entries) + 1), RunID: &r,
		StageID: stageID, Category: category, ActorKind: &k, Timestamp: ts, Payload: body})
}

func (f *stallAuditRepo) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := p.RunID
	e := &audit.Entry{ID: uuid.New(), Sequence: int64(len(f.entries) + 1), RunID: &r, StageID: p.StageID,
		Category: p.Category, ActorKind: p.ActorKind, ActorSubject: p.ActorSubject, Timestamp: p.Timestamp, Payload: p.Payload}
	f.entries = append(f.entries, e)
	return e, nil
}

func (f *stallAuditRepo) ListForRun(_ context.Context, runID uuid.UUID) ([]*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failListFor[runID] {
		return nil, errors.New("stall fake audit: list refused")
	}
	var out []*audit.Entry
	for _, e := range f.entries {
		if e.RunID != nil && *e.RunID == runID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *stallAuditRepo) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if f.failByCat != "" && f.failByCat == category {
		return nil, errors.New("stall fake audit: category list refused")
	}
	all, err := f.ListForRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	var out []*audit.Entry
	for _, e := range all {
		if e.Category == category {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *stallAuditRepo) count(category string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.entries {
		if e.Category == category {
			n++
		}
	}
	return n
}

func (f *stallAuditRepo) rows(t *testing.T, category string) []map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, e := range f.entries {
		if e.Category != category {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			t.Fatalf("decode %s payload: %v", category, err)
		}
		out = append(out, m)
	}
	return out
}

// stallDedupAuditRepo implements DedupedChainAppender and reports every
// append as a duplicate.
type stallDedupAuditRepo struct {
	*stallAuditRepo
	calls int
}

func (f *stallDedupAuditRepo) AppendChainedDeduped(_ context.Context, _ audit.ChainAppendParams, _ audit.DedupeSpec) (*audit.Entry, error) {
	f.calls++
	return nil, &audit.DedupedDuplicateError{Existing: &audit.Entry{Sequence: 7}}
}

// stallNotifier counts NotifyPageClassForRun calls and optionally fails them.
// Only that method is called by the stall watch; the embedded nil Channel
// panics on any other, which would flag an unexpected call.
type stallNotifier struct {
	issuecomment.Channel
	mu    sync.Mutex
	calls []uuid.UUID
	err   error
}

func (n *stallNotifier) NotifyPageClassForRun(_ context.Context, runID uuid.UUID) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, runID)
	return n.err
}

func (n *stallNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.calls)
}

// ---------------------------------------------------------------------
// Fixture helpers.
// ---------------------------------------------------------------------

var stallNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

type stallFixture struct {
	s    *Server
	runs *stallRunRepo
	au   *stallAuditRepo
	ntf  *stallNotifier
}

func newStallFixture(t *testing.T) *stallFixture {
	t.Helper()
	f := &stallFixture{runs: newStallRunRepo(), au: &stallAuditRepo{}, ntf: &stallNotifier{}}
	f.s = New(Config{Addr: "127.0.0.1:0", RunRepo: f.runs, AuditRepo: f.au})
	f.s.issueNotifier = f.ntf
	return f
}

func stallParams(now time.Time) StallScanParams {
	return StallScanParams{Threshold: 30 * time.Minute, Lookback: 168 * time.Hour, Now: now}
}

func (f *stallFixture) addRun(state run.State, created time.Time, mut func(r *run.Run)) *run.Run {
	ref := "issue:5497"
	r := &run.Run{ID: uuid.New(), Repo: "acme/widgets", WorkflowID: "feature_change", TriggerSource: run.TriggerGitHubIssue,
		TriggerRef: &ref, State: state, CreatedAt: created, UpdatedAt: created}
	if mut != nil {
		mut(r)
	}
	f.runs.mu.Lock()
	f.runs.runs[r.ID] = r
	f.runs.mu.Unlock()
	return r
}

func (f *stallFixture) addStage(runID uuid.UUID, seq int, typ run.StageType, state run.StageState, mut func(st *run.Stage)) *run.Stage {
	st := &run.Stage{ID: uuid.New(), RunID: runID, Sequence: seq, Type: typ, State: state, CreatedAt: stallNow.Add(-72 * time.Hour)}
	if mut != nil {
		mut(st)
	}
	f.runs.mu.Lock()
	f.runs.stages[runID] = append(f.runs.stages[runID], st)
	f.runs.mu.Unlock()
	return st
}

// mutateStage edits the stored stage in place, as a repository transition
// would.
func (f *stallFixture) mutateStage(runID, stageID uuid.UUID, mut func(st *run.Stage)) {
	f.runs.mu.Lock()
	defer f.runs.mu.Unlock()
	for _, st := range f.runs.stages[runID] {
		if st.ID == stageID {
			mut(st)
		}
	}
}

func failedWith(cat run.FailureCategory, reason string, endedAt time.Time) func(st *run.Stage) {
	return func(st *run.Stage) {
		c := cat
		st.FailureCategory = &c
		if reason != "" {
			rs := reason
			st.FailureReason = &rs
		}
		e := endedAt
		st.EndedAt = &e
	}
}

func (f *stallFixture) scan(t *testing.T, now time.Time) StallScanReport {
	t.Helper()
	rep, err := f.s.ScanFailedStageStalls(context.Background(), stallParams(now))
	if err != nil {
		t.Fatalf("ScanFailedStageStalls: %v", err)
	}
	return rep
}

func (f *stallFixture) chain(t *testing.T, runID uuid.UUID) []*audit.Entry {
	t.Helper()
	es, err := f.au.ListForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListForRun: %v", err)
	}
	return es
}

func (f *stallFixture) liveStages(t *testing.T, runID uuid.UUID) []*run.Stage {
	t.Helper()
	sts, _ := f.runs.ListStagesForRun(context.Background(), runID)
	return sts
}

// parkedRunWithFailedImplement is the canonical page-eligible fixture: a
// running (parked) run whose implement stage failed category C endedAgo
// before stallNow.
func (f *stallFixture) parkedRunWithFailedImplement(endedAgo time.Duration) (*run.Run, *run.Stage) {
	r := f.addRun(run.StateRunning, stallNow.Add(-72*time.Hour), nil)
	f.addStage(r.ID, 0, run.StageTypePlan, run.StageStateSucceeded, nil)
	st := f.addStage(r.ID, 1, run.StageTypeImplement, run.StageStateFailed, failedWith(run.FailureC, "runner_lost", stallNow.Add(-endedAgo)))
	return r, st
}

// ---------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------

func TestScanFailedStageStalls_RetryablePastThresholdPagesOnce(t *testing.T) {
	f := newStallFixture(t)
	r, st := f.parkedRunWithFailedImplement(31 * time.Minute)

	first := f.scan(t, stallNow)
	f.scan(t, stallNow.Add(5*time.Minute))

	rows := f.au.rows(t, CategoryStageFailedAwaitingOperator)
	if len(rows) != 1 {
		t.Fatalf("stage_failed_awaiting_operator rows = %d after two scans, want exactly 1", len(rows))
	}
	if rows[0]["next_action"] != StallNextActionRetryStage || rows[0]["next_action_target"] != st.ID.String() {
		t.Errorf("row next action = %v -> %v, want %s -> %s", rows[0]["next_action"], rows[0]["next_action_target"], StallNextActionRetryStage, st.ID)
	}
	if rows[0]["stage_id"] != st.ID.String() || rows[0]["run_state"] != "running" || rows[0]["failure_category"] != "C" {
		t.Errorf("row payload = %v", rows[0])
	}
	if n := f.ntf.count(); n != 1 {
		t.Errorf("notifyPageClass calls = %d, want exactly 1", n)
	}
	if first.Paged != 1 || first.Scanned != 1 {
		t.Errorf("first report = %+v, want Scanned 1 Paged 1", first)
	}
	// The row is the system stall-watch actor on the failed stage.
	for _, e := range f.chain(t, r.ID) {
		if e.Category != CategoryStageFailedAwaitingOperator {
			continue
		}
		if e.ActorKind == nil || *e.ActorKind != audit.ActorSystem || e.ActorSubject == nil || *e.ActorSubject != StallWatchActorSubject {
			t.Errorf("row actor = %v/%v, want system/%s", e.ActorKind, e.ActorSubject, StallWatchActorSubject)
		}
		if e.StageID == nil || *e.StageID != st.ID {
			t.Errorf("row stage = %v, want %s", e.StageID, st.ID)
		}
	}
}

func TestScanFailedStageStalls_BelowThresholdNoPage(t *testing.T) {
	f := newStallFixture(t)
	f.parkedRunWithFailedImplement(29 * time.Minute)
	f.scan(t, stallNow)
	if n := f.au.count(CategoryStageFailedAwaitingOperator); n != 0 {
		t.Errorf("rows = %d for a 29m-old failure, want 0", n)
	}
	if n := f.ntf.count(); n != 0 {
		t.Errorf("notifier calls = %d, want 0", n)
	}
}

func TestScanFailedStageStalls_OperatorActRestartsClock(t *testing.T) {
	f := newStallFixture(t)
	r, _ := f.parkedRunWithFailedImplement(2 * time.Hour)
	f.au.seed(r.ID, nil, "approval_submitted", audit.ActorUser, stallNow.Add(-10*time.Minute), map[string]any{})
	f.scan(t, stallNow)
	if n := f.au.count(CategoryStageFailedAwaitingOperator); n != 0 {
		t.Errorf("rows = %d with a user act 10m ago, want 0", n)
	}
}

// retryAndRefail simulates RetryStageState (bump, clear EndedAt) followed by a
// fresh failure at endedAt.
func (f *stallFixture) retryAndRefail(runID, stageID uuid.UUID, endedAt time.Time) {
	f.mutateStage(runID, stageID, func(st *run.Stage) {
		st.SelfRetryCount++
		e := endedAt
		st.EndedAt = &e
		st.State = run.StageStateFailed
	})
}

func TestScanFailedStageStalls_NewEpisodePagesAgain(t *testing.T) {
	f := newStallFixture(t)
	r, st := f.parkedRunWithFailedImplement(31 * time.Minute)
	f.scan(t, stallNow)
	f.retryAndRefail(r.ID, st.ID, stallNow.Add(time.Hour))
	later := stallNow.Add(time.Hour + 31*time.Minute)
	f.scan(t, later)

	rows := f.au.rows(t, CategoryStageFailedAwaitingOperator)
	if len(rows) != 2 {
		t.Fatalf("rows = %d after a new episode, want 2", len(rows))
	}
	if rows[0]["episode_key"] == rows[1]["episode_key"] {
		t.Errorf("both rows carry episode_key %v, want distinct", rows[0]["episode_key"])
	}
}

func TestScanFailedStageStalls_ReEntryBelowThresholdNoPage(t *testing.T) {
	f := newStallFixture(t)
	f.ntf.err = errors.New("forge down")
	r, st := f.parkedRunWithFailedImplement(31 * time.Minute)
	f.scan(t, stallNow)
	if n := f.au.count(CategoryStageFailedAwaitingOperator); n != 1 {
		t.Fatalf("first scan rows = %d, want 1", n)
	}
	// Retried and re-failed 5m before the second scan.
	second := stallNow.Add(10 * time.Minute)
	f.retryAndRefail(r.ID, st.ID, second.Add(-5*time.Minute))
	f.scan(t, second)
	if n := f.au.count(CategoryStageFailedAwaitingOperator); n != 1 {
		t.Errorf("rows = %d after a below-threshold re-entry, want still 1", n)
	}
	if kinds := issuecomment.UnresolvedPageClassEventKinds(f.chain(t, r.ID), f.liveStages(t, r.ID)); len(kinds) != 0 {
		t.Errorf("unresolved page kinds = %v, want none (the old episode ended)", kinds)
	}
}

func TestScanFailedStageStalls_RevivableFailedRunPagesRevive(t *testing.T) {
	f := newStallFixture(t)
	r := f.addRun(run.StateFailed, stallNow.Add(-3*time.Hour), nil)
	f.addStage(r.ID, 0, run.StageTypeImplement, run.StageStateFailed, failedWith(run.FailureA, "", stallNow.Add(-2*time.Hour)))
	f.addStage(r.ID, 1, run.StageTypeAcceptance, run.StageStateFailed, failedWith(run.FailureC, "", stallNow.Add(-time.Hour)))
	f.scan(t, stallNow)

	rows := f.au.rows(t, CategoryStageFailedAwaitingOperator)
	if len(rows) != 1 || rows[0]["next_action"] != StallNextActionReviveRun || rows[0]["next_action_target"] != r.ID.String() {
		t.Fatalf("rows = %v, want one fishhawk_revive_run row targeting the run", rows)
	}
	if n := f.au.count(CategoryStageRetried); n != 0 {
		t.Errorf("stage_retried rows = %d, want 0 (the watch never retries)", n)
	}
}

func TestScanFailedStageStalls_CategoryBPagesNeverRetries(t *testing.T) {
	f := newStallFixture(t)
	parent := f.addRun(run.StateRunning, stallNow.Add(-5*time.Hour), nil)
	f.addStage(parent.ID, 1, run.StageTypeImplement, run.StageStateAwaitingChildren, nil)
	child := f.addRun(run.StateFailed, stallNow.Add(-4*time.Hour), func(r *run.Run) {
		r.DecomposedFrom = &parent.ID
		r.ParentRunID = &parent.ID
	})
	st := f.addStage(child.ID, 0, run.StageTypeImplement, run.StageStateFailed, failedWith(run.FailureB, "constraint", stallNow.Add(-time.Hour)))
	// A newer sibling slice on the same trigger_ref does not supersede it.
	f.addRun(run.StateRunning, stallNow.Add(-3*time.Hour), func(r *run.Run) {
		r.DecomposedFrom = &parent.ID
		r.ParentRunID = &parent.ID
	})
	f.scan(t, stallNow)

	rows := f.au.rows(t, CategoryStageFailedAwaitingOperator)
	if len(rows) != 1 || rows[0]["next_action"] != StallNextActionResumeRun {
		t.Fatalf("rows = %v, want one fishhawk_resume_run row", rows)
	}
	got := f.liveStages(t, child.ID)[0]
	if got.ID != st.ID || got.State != run.StageStateFailed {
		t.Errorf("child stage state = %s, want still failed", got.State)
	}
	if n := f.au.count(CategoryStageRetried) + f.au.count("run_auto_driven"); n != 0 {
		t.Errorf("retry/auto-drive rows = %d, want 0", n)
	}
}

func TestScanFailedStageStalls_DecomposedParentAcceptancePagesOnce(t *testing.T) {
	f := newStallFixture(t)
	r := f.addRun(run.StateRunning, stallNow.Add(-6*time.Hour), nil)
	f.au.seed(r.ID, nil, "plan_decomposed", audit.ActorSystem, stallNow.Add(-5*time.Hour), map[string]any{})
	f.addStage(r.ID, 1, run.StageTypeImplement, run.StageStateSucceeded, nil)
	f.addStage(r.ID, 2, run.StageTypeAcceptance, run.StageStateFailed, failedWith(run.FailureC, "", stallNow.Add(-31*time.Minute)))
	f.scan(t, stallNow)
	f.scan(t, stallNow.Add(5*time.Minute))
	rows := f.au.rows(t, CategoryStageFailedAwaitingOperator)
	if len(rows) != 1 || rows[0]["next_action"] != StallNextActionRetryStage || rows[0]["stage_type"] != "acceptance" {
		t.Fatalf("rows = %v, want one retry_stage row for the acceptance stage", rows)
	}
}

// A failed decomposed parent's slices carry parent_run_id = the parent, but a
// slice is not a recovery descendant and does not supersede it (#2549).
func TestScanFailedStageStalls_FailedParentWithSlicesNotSuperseded(t *testing.T) {
	f := newStallFixture(t)
	r := f.addRun(run.StateFailed, stallNow.Add(-6*time.Hour), nil)
	f.au.seed(r.ID, nil, "plan_decomposed", audit.ActorSystem, stallNow.Add(-5*time.Hour), map[string]any{})
	f.addStage(r.ID, 1, run.StageTypeImplement, run.StageStateSucceeded, nil)
	f.addStage(r.ID, 2, run.StageTypeAcceptance, run.StageStateFailed, failedWith(run.FailureC, "", stallNow.Add(-time.Hour)))
	f.addRun(run.StateSucceeded, stallNow.Add(-4*time.Hour), func(c *run.Run) {
		c.DecomposedFrom = &r.ID
		c.ParentRunID = &r.ID
	})
	f.scan(t, stallNow)
	rows := f.au.rows(t, CategoryStageFailedAwaitingOperator)
	if len(rows) != 1 || rows[0]["next_action"] != StallNextActionReviveRun {
		t.Fatalf("rows = %v, want one revive_run row (a slice is not a superseding run)", rows)
	}
}

func TestScanFailedStageStalls_Exclusions(t *testing.T) {
	cases := []struct {
		name  string
		build func(f *stallFixture)
	}{
		{"non-terminal D gate-rejected", func(f *stallFixture) {
			r := f.addRun(run.StateRunning, stallNow.Add(-3*time.Hour), nil)
			f.addStage(r.ID, 0, run.StageTypePlan, run.StageStateFailed, failedWith(run.FailureD, "gate_rejected", stallNow.Add(-time.Hour)))
		}},
		{"non-terminal B", func(f *stallFixture) {
			r := f.addRun(run.StateRunning, stallNow.Add(-3*time.Hour), nil)
			f.addStage(r.ID, 0, run.StageTypeImplement, run.StageStateFailed, failedWith(run.FailureB, "constraint", stallNow.Add(-time.Hour)))
		}},
		{"park budget exhausted", func(f *stallFixture) {
			r, st := f.parkedRunWithFailedImplement(time.Hour)
			for i := 0; i < run.InfraFailureParkRetryBudget; i++ {
				f.au.seed(r.ID, &st.ID, run.CategoryStageInfraParked, audit.ActorSystem, stallNow.Add(-5*time.Hour),
					map[string]any{"failed_at": "2026-11-01T00:00:00Z", "retry_ordinal": i})
			}
		}},
		{"decomposed-parent implement failure", func(f *stallFixture) {
			r, _ := f.parkedRunWithFailedImplement(time.Hour)
			f.au.seed(r.ID, nil, "plan_decomposed", audit.ActorSystem, stallNow.Add(-5*time.Hour), map[string]any{})
		}},
		{"cancelled run", func(f *stallFixture) {
			r := f.addRun(run.StateCancelled, stallNow.Add(-3*time.Hour), nil)
			f.addStage(r.ID, 0, run.StageTypeImplement, run.StageStateFailed, failedWith(run.FailureC, "", stallNow.Add(-time.Hour)))
		}},
		{"succeeded run", func(f *stallFixture) {
			r := f.addRun(run.StateSucceeded, stallNow.Add(-3*time.Hour), nil)
			f.addStage(r.ID, 0, run.StageTypeAcceptance, run.StageStateFailed, failedWith(run.FailureC, "", stallNow.Add(-time.Hour)))
		}},
		{"failed run whose failure is older than lookback", func(f *stallFixture) {
			r := f.addRun(run.StateFailed, stallNow.Add(-time.Hour), nil)
			f.addStage(r.ID, 0, run.StageTypeImplement, run.StageStateFailed, failedWith(run.FailureC, "", stallNow.Add(-200*time.Hour)))
		}},
		{"failed run created before lookback (documented created_at residual)", func(f *stallFixture) {
			r := f.addRun(run.StateFailed, stallNow.Add(-200*time.Hour), nil)
			f.addStage(r.ID, 0, run.StageTypeImplement, run.StageStateFailed, failedWith(run.FailureC, "", stallNow.Add(-time.Hour)))
		}},
		{"failed top-level B", func(f *stallFixture) {
			r := f.addRun(run.StateFailed, stallNow.Add(-3*time.Hour), nil)
			f.addStage(r.ID, 0, run.StageTypeImplement, run.StageStateFailed, failedWith(run.FailureB, "constraint", stallNow.Add(-time.Hour)))
		}},
		{"failed top-level D", func(f *stallFixture) {
			r := f.addRun(run.StateFailed, stallNow.Add(-3*time.Hour), nil)
			f.addStage(r.ID, 0, run.StageTypePlan, run.StageStateFailed, failedWith(run.FailureD, "gate_rejected", stallNow.Add(-time.Hour)))
		}},
		{"failed child B under a terminal parent", func(f *stallFixture) {
			parent := f.addRun(run.StateFailed, stallNow.Add(-300*time.Hour), nil)
			child := f.addRun(run.StateFailed, stallNow.Add(-4*time.Hour), func(r *run.Run) { r.DecomposedFrom = &parent.ID })
			f.addStage(child.ID, 0, run.StageTypeImplement, run.StageStateFailed, failedWith(run.FailureB, "constraint", stallNow.Add(-time.Hour)))
		}},
		{"failed run superseded by a newer run on the same trigger_ref", func(f *stallFixture) {
			r := f.addRun(run.StateFailed, stallNow.Add(-3*time.Hour), nil)
			f.addStage(r.ID, 0, run.StageTypeImplement, run.StageStateFailed, failedWith(run.FailureC, "", stallNow.Add(-time.Hour)))
			f.addRun(run.StateRunning, stallNow.Add(-30*time.Minute), nil)
		}},
		{"failed run with a recovery descendant", func(f *stallFixture) {
			r := f.addRun(run.StateFailed, stallNow.Add(-3*time.Hour), func(r *run.Run) { r.TriggerRef = nil })
			f.addStage(r.ID, 0, run.StageTypeImplement, run.StageStateFailed, failedWith(run.FailureC, "", stallNow.Add(-time.Hour)))
			f.addRun(run.StateSucceeded, stallNow.Add(-30*time.Minute), func(c *run.Run) { c.TriggerRef = nil; c.ParentRunID = &r.ID })
		}},
		{"failed stage without EndedAt", func(f *stallFixture) {
			r := f.addRun(run.StateRunning, stallNow.Add(-3*time.Hour), nil)
			f.addStage(r.ID, 0, run.StageTypeImplement, run.StageStateFailed, func(st *run.Stage) {
				c := run.FailureC
				st.FailureCategory = &c
			})
		}},
		{"failed stage without a FailureCategory", func(f *stallFixture) {
			r := f.addRun(run.StateRunning, stallNow.Add(-3*time.Hour), nil)
			f.addStage(r.ID, 0, run.StageTypeImplement, run.StageStateFailed, func(st *run.Stage) {
				e := stallNow.Add(-time.Hour)
				st.EndedAt = &e
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newStallFixture(t)
			tc.build(f)
			f.scan(t, stallNow)
			if n := f.au.count(CategoryStageFailedAwaitingOperator); n != 0 {
				t.Errorf("rows = %d, want 0: %v", n, f.au.rows(t, CategoryStageFailedAwaitingOperator))
			}
			if n := f.ntf.count(); n != 0 {
				t.Errorf("notifier calls = %d, want 0", n)
			}
		})
	}
}

func TestScanFailedStageStalls_DedupedAppenderDuplicateIsBenign(t *testing.T) {
	f := newStallFixture(t)
	dd := &stallDedupAuditRepo{stallAuditRepo: f.au}
	f.s.cfg.AuditRepo = dd
	f.parkedRunWithFailedImplement(time.Hour)
	rep, err := f.s.ScanFailedStageStalls(context.Background(), stallParams(stallNow))
	if err != nil {
		t.Fatalf("scan error = %v, want nil for a benign duplicate", err)
	}
	if dd.calls != 1 {
		t.Fatalf("AppendChainedDeduped calls = %d, want 1 (the capability path must be taken)", dd.calls)
	}
	if n := f.ntf.count(); n != 0 || rep.Paged != 0 || rep.Skipped != 0 {
		t.Errorf("notifier calls = %d, report = %+v; want no page and no skip", n, rep)
	}
}

func TestScanFailedStageStalls_FallbackListErrorWritesNothing(t *testing.T) {
	f := newStallFixture(t)
	f.au.failByCat = CategoryStageFailedAwaitingOperator
	f.parkedRunWithFailedImplement(time.Hour)
	rep := f.scan(t, stallNow)
	if n := f.au.count(CategoryStageFailedAwaitingOperator); n != 0 {
		t.Errorf("rows = %d after a dedup-list error, want 0 (no unguarded append)", n)
	}
	if rep.Skipped != 1 || f.ntf.count() != 0 {
		t.Errorf("report = %+v, notifier = %d; want one skip and no page", rep, f.ntf.count())
	}
}

func TestScanFailedStageStalls_ReadErrorSkipsOnlyThatRun(t *testing.T) {
	f := newStallFixture(t)
	x, _ := f.parkedRunWithFailedImplement(time.Hour)
	y, _ := f.parkedRunWithFailedImplement(time.Hour)
	f.au.failListFor = map[uuid.UUID]bool{x.ID: true}
	rep, err := f.s.ScanFailedStageStalls(context.Background(), stallParams(stallNow))
	if err != nil {
		t.Fatalf("scan error = %v, want nil", err)
	}
	if rep.Skipped != 1 || rep.Paged != 1 {
		t.Errorf("report = %+v, want Skipped 1 Paged 1", rep)
	}
	f.au.failListFor = nil
	if es := f.chain(t, y.ID); len(es) != 1 || es[0].Category != CategoryStageFailedAwaitingOperator {
		t.Errorf("run Y chain = %v, want its page row", es)
	}
}

func TestScanFailedStageStalls_RefusesDegenerateScan(t *testing.T) {
	cases := []struct {
		name string
		p    StallScanParams
		mut  func(f *stallFixture)
	}{
		{"zero threshold", StallScanParams{Threshold: 0, Lookback: time.Hour, Now: stallNow}, nil},
		{"lookback below threshold", StallScanParams{Threshold: time.Hour, Lookback: time.Minute, Now: stallNow}, nil},
		{"zero clock", StallScanParams{Threshold: time.Minute, Lookback: time.Hour}, nil},
		{"unwired audit repo", stallParams(stallNow), func(f *stallFixture) { f.s.cfg.AuditRepo = nil }},
		{"list error", stallParams(stallNow), func(f *stallFixture) { f.runs.listErr = errors.New("db down") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newStallFixture(t)
			f.parkedRunWithFailedImplement(time.Hour)
			if tc.mut != nil {
				tc.mut(f)
			}
			if _, err := f.s.ScanFailedStageStalls(context.Background(), tc.p); err == nil {
				t.Error("scan error = nil, want a refusal")
			}
			if n := f.au.count(CategoryStageFailedAwaitingOperator); n != 0 {
				t.Errorf("rows = %d, want 0", n)
			}
		})
	}
}

// Cross-boundary (persistence -> server -> projection): the persisted row
// projects to failed_stage_stalled and carries the shared episode key.
func TestStallWatch_PageRowProjectsToPageKind(t *testing.T) {
	f := newStallFixture(t)
	r, st := f.parkedRunWithFailedImplement(31 * time.Minute)
	f.scan(t, stallNow)

	entries := f.chain(t, r.ID)
	stages := f.liveStages(t, r.ID)
	if kinds := issuecomment.PageClassEventKinds(entries, stages); len(kinds) != 1 || kinds[0] != "failed_stage_stalled" {
		t.Fatalf("projected kinds = %v, want [failed_stage_stalled]", kinds)
	}
	if kinds := issuecomment.UnresolvedPageClassEventKinds(entries, stages); len(kinds) != 1 || kinds[0] != "failed_stage_stalled" {
		t.Fatalf("unresolved kinds = %v, want [failed_stage_stalled]", kinds)
	}
	want, ok := issuecomment.FailedStageStallEpisode(st)
	if !ok {
		t.Fatal("FailedStageStallEpisode(!ok) on the failed fixture stage")
	}
	rows := f.au.rows(t, CategoryStageFailedAwaitingOperator)
	if len(rows) != 1 || rows[0]["episode_key"] != want {
		t.Errorf("row episode_key = %v, want %q", rows, want)
	}
	if reason, _ := rows[0]["reason"].(string); strings.Contains(reason, "runner_lost") {
		t.Errorf("row reason carries the stage failure_reason: %q", reason)
	}
}

// M4 cross-boundary: after the run is cancelled with the stage stamps
// unchanged, the persisted row is resolved by the run state alone.
func TestStallWatch_CancelledRunResolvesPage(t *testing.T) {
	f := newStallFixture(t)
	f.ntf.err = errors.New("forge down")
	r, _ := f.parkedRunWithFailedImplement(31 * time.Minute)
	f.scan(t, stallNow)
	cancelled := *r
	cancelled.State = run.StateCancelled
	entries := f.chain(t, r.ID)
	stages := f.liveStages(t, r.ID)
	if kinds := issuecomment.UnresolvedPageClassEventKindsForRun(r, entries, stages); len(kinds) != 1 {
		t.Fatalf("unresolved kinds on the live run = %v, want one (precondition)", kinds)
	}
	if kinds := issuecomment.UnresolvedPageClassEventKindsForRun(&cancelled, entries, stages); len(kinds) != 0 {
		t.Errorf("unresolved kinds on the cancelled run = %v, want none", kinds)
	}
}
