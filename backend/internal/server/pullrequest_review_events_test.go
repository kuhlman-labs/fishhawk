package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// prEventsRunRepo is the run.Repository surface the
// pull_request.closed / pull_request_review.submitted handlers use.
// Records ListRuns + TransitionStage calls so tests can assert on
// both the lookup filter and the side effects.
type prEventsRunRepo struct {
	run.Repository
	mu          sync.Mutex
	listURLs    []string
	listResult  []*run.Run
	listErr     error
	stages      map[uuid.UUID][]*run.Stage
	stagesErr   error
	transitions []prEventsTransition
	transErr    error
	curState    map[uuid.UUID]run.StageState // models the same-state no-op
	runStates   map[uuid.UUID]run.State      // terminal run state recorded by TransitionRun

	// exactURLResult, when non-nil, models the indexed, non-recency-windowed
	// PullRequestURL DB filter: a ListRuns call carrying f.PullRequestURL
	// returns this slice instead of listResult, so a test can distinguish
	// the project-scoped windowed scan (which truncates at Limit) from the
	// exact-URL supplement (which is not windowed). Left nil, ListRuns
	// returns listResult for every filter — every existing consumer of this
	// fake is unaffected.
	exactURLResult []*run.Run

	// decomposedResult, when non-nil, models the DecomposedFrom lineage filter
	// the economics lineage rollup uses (#2100): a ListRuns call carrying
	// f.DecomposedFrom returns this slice (the parent's decomposition slice
	// children). Left nil, a DecomposedFrom query returns no children — the
	// single-run economics path every existing consumer of this fake relies on.
	decomposedResult []*run.Run

	// beforeCAS, when non-nil, runs inside TransitionStageFrom BEFORE the
	// compare-and-swap evaluates its expected-vs-current check, holding r.mu.
	// It is how the transition-first-then-audit ordering test seeds a CAS
	// refusal BY CONSTRUCTION: the hook flips the stage's current state in the
	// window between the sweep's classification and its CAS, exactly as a
	// concurrent writer would, so the RED lands on the behavioral assertion
	// (zero audit rows) and never on a fixture-setup failure. Mirrors
	// run.casMemRepo's hook in failure_test.go.
	beforeCAS func(id uuid.UUID)
}

type prEventsTransition struct {
	StageID uuid.UUID
	To      run.StageState
}

func (r *prEventsRunRepo) ListRuns(_ context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f.DecomposedFrom != nil {
		// Decomposition lineage walk (#2100): return the seeded children (nil by
		// default, so the single-run economics path is preserved).
		return r.decomposedResult, r.listErr
	}
	if f.PullRequestURL != nil {
		r.listURLs = append(r.listURLs, *f.PullRequestURL)
		if r.exactURLResult != nil {
			return r.exactURLResult, r.listErr
		}
	}
	return r.listResult, r.listErr
}

// GetRun searches the seeded runs by ID. Used by
// ResolveReviewFromPollState (the merge-reconciler poll entrypoint).
func (r *prEventsRunRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rn := range r.listResult {
		if rn.ID == id {
			return rn, nil
		}
	}
	return nil, run.ErrNotFound
}

// ListStagesForRun overlays any state recorded by TransitionStage onto
// the seeded stage fixtures so a caller reading stages AFTER a review
// transition (the orchestrator's completeRun stage scan) observes the
// resolved state — without this overlay the static slice would still
// show the review stage as awaiting_approval and completeRun would
// mis-compute the run's terminal state.
func (r *prEventsRunRepo) ListStagesForRun(_ context.Context, id uuid.UUID) ([]*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stagesErr != nil {
		return nil, r.stagesErr
	}
	src := r.stages[id]
	out := make([]*run.Stage, len(src))
	for i, st := range src {
		cp := *st
		if cur, ok := r.curState[st.ID]; ok {
			cp.State = cur
		}
		out[i] = &cp
	}
	return out, nil
}

// TransitionRun records the run's target State (and updates the seeded
// run in place so a subsequent GetRun is consistent), modelling the
// idempotent same-state allowance. Used by the orchestrator's
// completeRun when the regression tests wire a real Orchestrator.
func (r *prEventsRunRepo) TransitionRun(_ context.Context, id uuid.UUID, to run.State) (*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runStates == nil {
		r.runStates = map[uuid.UUID]run.State{}
	}
	r.runStates[id] = to
	for _, rn := range r.listResult {
		if rn.ID == id {
			rn.State = to
			return rn, nil
		}
	}
	return &run.Run{ID: id, State: to}, nil
}

// TransitionStage models the real repo's same-state allowance: a
// transition to the state the stage is already in is a no-op and is NOT
// recorded. This is the basis for webhook+poll idempotency — the second
// resolver firing on an already-terminal stage produces no duplicate
// effective transition. Current state is seeded from the stage fixtures
// on first touch.
func (r *prEventsRunRepo) TransitionStage(_ context.Context, id uuid.UUID, to run.StageState, _ *run.StageCompletion) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.transErr != nil {
		return nil, r.transErr
	}
	if r.curState == nil {
		r.curState = map[uuid.UUID]run.StageState{}
	}
	cur, ok := r.curState[id]
	if !ok {
		cur = r.seedStateLocked(id)
		r.curState[id] = cur
	}
	if cur == to {
		// Same-state no-op: not recorded as an effective transition.
		return &run.Stage{ID: id, State: to}, nil
	}
	if cur.IsTerminal() {
		// Terminal is terminal (E64.2 / #3083). The real repository's
		// ValidStageTransition has no arc OUT of a terminal state, so a fake
		// that silently relabels one is more permissive than production —
		// and that permissiveness is not neutral: it makes a reordering that
		// supersedes a stage the merge path is about to mark `succeeded`
		// invisible to every test here, because the later relabel would
		// quietly paper over it. Refusing keeps the fake honest.
		return nil, run.InvalidTransitionError{Kind: "stage", From: string(cur), To: string(to)}
	}
	r.curState[id] = to
	r.transitions = append(r.transitions, prEventsTransition{StageID: id, To: to})
	return &run.Stage{ID: id, State: to}, nil
}

// seedStateLocked finds the seeded state of stage id from the fixtures.
// Caller holds r.mu.
func (r *prEventsRunRepo) seedStateLocked(id uuid.UUID) run.StageState {
	for _, sts := range r.stages {
		for _, st := range sts {
			if st.ID == id {
				return st.State
			}
		}
	}
	return ""
}

// TransitionStageFrom is the compare-and-swap sibling of TransitionStage,
// modelling run.StageCASTransitioner (repository.go): it refuses with a typed
// run.StageStateChangedError when the row's current state is not the `from`
// the caller pinned, and applies the move otherwise. The merge-supersede sweep
// (merge_supersede.go) REQUIRES this capability — a repo that does not
// implement it sweeps nothing — so the fake must provide it for the sweep to
// be exercised at all.
func (r *prEventsRunRepo) TransitionStageFrom(_ context.Context, id uuid.UUID, from, to run.StageState, _ *run.StageCompletion) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.beforeCAS != nil {
		// Concurrent-writer window: runs BEFORE the expected-vs-current check.
		r.beforeCAS(id)
	}
	if r.transErr != nil {
		return nil, r.transErr
	}
	if r.curState == nil {
		r.curState = map[uuid.UUID]run.StageState{}
	}
	cur, ok := r.curState[id]
	if !ok {
		cur = r.seedStateLocked(id)
		r.curState[id] = cur
	}
	if cur != from {
		return nil, run.StageStateChangedError{StageID: id, Expected: from, Actual: cur}
	}
	r.curState[id] = to
	r.transitions = append(r.transitions, prEventsTransition{StageID: id, To: to})
	return &run.Stage{ID: id, State: to}, nil
}

// compile-time proof that the fake carries the capability the sweep requires.
// Without it supersedeParkedStagesOnMerge warn-logs and sweeps NOTHING, so
// every sweep assertion below would pass vacuously against an inert sweep.
var _ run.StageCASTransitioner = (*prEventsRunRepo)(nil)

// prEventsAuditRepo captures AppendChained calls so tests can assert
// on category + payload shape. It also serves a seeded chain via ListForRun
// for the merge-time economics stamp (#1702).
type prEventsAuditRepo struct {
	audit.Repository
	mu            sync.Mutex
	appended      []audit.ChainAppendParams
	err           error
	listForRun    []*audit.Entry
	listForRunErr error
	// byCategory serves per-run cost_recorded ledgers for the economics lineage
	// rollup's per-child audit read (#2100), keyed by run id. Nil-safe: an
	// un-seeded run id returns no entries.
	byCategory map[uuid.UUID][]*audit.Entry
}

func (r *prEventsAuditRepo) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	r.appended = append(r.appended, p)
	return &audit.Entry{ID: uuid.New()}, nil
}

// ListForRun serves the seeded chain the economics stamp folds. Returns
// (nil, nil) by default so tests that never wire GitHub (and thus never reach
// the stamp) are unaffected.
func (r *prEventsAuditRepo) ListForRun(_ context.Context, _ uuid.UUID) ([]*audit.Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listForRun, r.listForRunErr
}

// ListForRunByCategory serves the per-run cost ledger the economics lineage
// rollup reads for each decomposition child (#2100). Returns the seeded slice
// for the run id (nil when unseeded); category is always cost_recorded here.
func (r *prEventsAuditRepo) ListForRunByCategory(_ context.Context, runID uuid.UUID, _ string) ([]*audit.Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byCategory == nil {
		return nil, nil
	}
	return r.byCategory[runID], nil
}

func prEventsTestServer(t *testing.T, rr *prEventsRunRepo, ar *prEventsAuditRepo) *Server {
	t.Helper()
	return New(Config{
		Addr:      "127.0.0.1:0",
		RunRepo:   rr,
		AuditRepo: ar,
	})
}

// findCategory returns the first audit row matching category, or nil
// if none. Lets a test assert "pr_merged row exists" without
// caring about row order.
func findCategory(rows []audit.ChainAppendParams, category string) *audit.ChainAppendParams {
	for i := range rows {
		if rows[i].Category == category {
			return &rows[i]
		}
	}
	return nil
}

// --- pull_request.closed ---

func TestPullRequestClosed_Merged_TransitionsReviewStageAndAudits(t *testing.T) {
	runID := uuid.New()
	reviewStageID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: runID, PullRequestURL: &prURL}},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {
				{ID: uuid.New(), RunID: runID, Type: run.StageTypeImplement, State: run.StageStateSucceeded},
				{ID: reviewStageID, RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval},
			},
		},
	}
	ar := &prEventsAuditRepo{}
	s := prEventsTestServer(t, rr, ar)

	payload, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{
			"html_url":  prURL,
			"number":    42,
			"merged":    true,
			"merged_by": map[string]any{"login": "alice"},
			"head":      map[string]any{"sha": "headsha"},
			"base":      map[string]any{"sha": "basesha"},
		},
		"sender": map[string]any{"login": "alice"},
	})
	s.handlePullRequestClosed(context.Background(), payload)

	// Review stage transitioned to succeeded.
	if len(rr.transitions) != 1 {
		t.Fatalf("transitions = %d, want 1", len(rr.transitions))
	}
	if rr.transitions[0].StageID != reviewStageID {
		t.Errorf("transition stage_id = %s, want %s (review)", rr.transitions[0].StageID, reviewStageID)
	}
	if rr.transitions[0].To != run.StageStateSucceeded {
		t.Errorf("transition.To = %q, want succeeded", rr.transitions[0].To)
	}

	// pr_merged audit row written against the run + review stage.
	row := findCategory(ar.appended, CategoryPRMerged)
	if row == nil {
		t.Fatalf("missing pr_merged audit row; got categories %v", auditCategories(ar.appended))
	}
	if row.RunID != runID {
		t.Errorf("audit RunID = %s, want %s", row.RunID, runID)
	}
	if row.StageID == nil || *row.StageID != reviewStageID {
		t.Errorf("audit StageID = %v, want %s", row.StageID, reviewStageID)
	}
	if row.ActorSubject == nil || *row.ActorSubject != "alice" {
		t.Errorf("audit ActorSubject = %v, want alice", row.ActorSubject)
	}
	var body map[string]any
	if err := json.Unmarshal(row.Payload, &body); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if body["head_sha"] != "headsha" || body["merger"] != "alice" {
		t.Errorf("audit payload missing expected fields: %+v", body)
	}
}

func TestPullRequestClosed_NotMerged_CancelsReviewStageAndAudits(t *testing.T) {
	// ADR-018 follow-up (#316): PR closed without merging signals
	// the work was abandoned. Cancel the review stage + write a
	// pr_closed_without_merge audit row naming the closer. The
	// run-level state becomes `cancelled` once every stage is
	// terminal (existing state-machine behavior; not asserted
	// here since the test uses the in-memory fake).
	runID := uuid.New()
	reviewStageID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: runID, PullRequestURL: &prURL}},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {{ID: reviewStageID, RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval}},
		},
	}
	ar := &prEventsAuditRepo{}
	s := prEventsTestServer(t, rr, ar)

	payload, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{
			"html_url": prURL,
			"merged":   false,
			"head":     map[string]any{"sha": "headsha"},
			"base":     map[string]any{"sha": "basesha"},
		},
		"sender": map[string]any{"login": "alice"},
	})
	s.handlePullRequestClosed(context.Background(), payload)

	// Review stage transitioned to cancelled.
	if len(rr.transitions) != 1 {
		t.Fatalf("transitions = %d, want 1", len(rr.transitions))
	}
	if rr.transitions[0].StageID != reviewStageID {
		t.Errorf("transition stage_id = %s, want %s (review)",
			rr.transitions[0].StageID, reviewStageID)
	}
	if rr.transitions[0].To != run.StageStateCancelled {
		t.Errorf("transition.To = %q, want cancelled", rr.transitions[0].To)
	}

	// pr_closed_without_merge audit row recorded against the run +
	// review stage.
	row := findCategory(ar.appended, CategoryPRClosedWithoutMerge)
	if row == nil {
		t.Fatalf("missing pr_closed_without_merge audit row; got %v", auditCategories(ar.appended))
	}
	if row.RunID != runID {
		t.Errorf("audit RunID = %s, want %s", row.RunID, runID)
	}
	if row.StageID == nil || *row.StageID != reviewStageID {
		t.Errorf("audit StageID = %v, want %s", row.StageID, reviewStageID)
	}
	if row.ActorSubject == nil || *row.ActorSubject != "alice" {
		t.Errorf("audit ActorSubject = %v, want alice", row.ActorSubject)
	}
	var body map[string]any
	if err := json.Unmarshal(row.Payload, &body); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if body["head_sha"] != "headsha" || body["closer"] != "alice" {
		t.Errorf("audit payload missing expected fields: %+v", body)
	}
	// No pr_merged row written.
	if findCategory(ar.appended, CategoryPRMerged) != nil {
		t.Errorf("unexpected pr_merged row on a non-merge close: %v", auditCategories(ar.appended))
	}
}

func TestPullRequestClosed_NotMerged_NoReviewStage_AuditOnlyNoTransition(t *testing.T) {
	// routine_change-shape runs are implement-only. A close-without-
	// merge still records the close in the audit log; there's no
	// review stage to cancel.
	runID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: runID, PullRequestURL: &prURL}},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {{ID: uuid.New(), RunID: runID, Type: run.StageTypeImplement, State: run.StageStateSucceeded}},
		},
	}
	ar := &prEventsAuditRepo{}
	s := prEventsTestServer(t, rr, ar)

	payload, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{
			"html_url": prURL,
			"merged":   false,
			"head":     map[string]any{"sha": "h"},
			"base":     map[string]any{"sha": "b"},
		},
		"sender": map[string]any{"login": "alice"},
	})
	s.handlePullRequestClosed(context.Background(), payload)

	if len(rr.transitions) != 0 {
		t.Errorf("transitions = %d, want 0 (no review stage)", len(rr.transitions))
	}
	row := findCategory(ar.appended, CategoryPRClosedWithoutMerge)
	if row == nil {
		t.Fatalf("expected pr_closed_without_merge row for implement-only run")
	}
	if row.StageID != nil {
		t.Errorf("audit StageID = %v, want nil (no review stage)", row.StageID)
	}
}

func TestPullRequestClosed_NotMerged_TransitionFailureLogged_AuditStillWritten(t *testing.T) {
	// State-machine rejection (e.g., reviewer manually cancelled
	// the stage first; close webhook lands after) must NOT drop
	// the pr_closed_without_merge audit row. The close happened
	// on GitHub regardless of whether Fishhawk can advance the
	// stage.
	runID := uuid.New()
	reviewStageID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: runID, PullRequestURL: &prURL}},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {{ID: reviewStageID, RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval}},
		},
		transErr: errors.New("state machine refusal"),
	}
	ar := &prEventsAuditRepo{}
	s := prEventsTestServer(t, rr, ar)

	payload, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{
			"html_url": prURL,
			"merged":   false,
			"head":     map[string]any{"sha": "h"},
			"base":     map[string]any{"sha": "b"},
		},
		"sender": map[string]any{"login": "alice"},
	})
	s.handlePullRequestClosed(context.Background(), payload)

	if findCategory(ar.appended, CategoryPRClosedWithoutMerge) == nil {
		t.Errorf("pr_closed_without_merge audit row should be written even when transition fails")
	}
}

func TestPullRequestClosed_NoMatchingRun_NoOp(t *testing.T) {
	// PR isn't Fishhawk-managed (ListRuns returns empty). Handler
	// short-circuits without touching the audit log or the state
	// machine.
	rr := &prEventsRunRepo{listResult: nil}
	ar := &prEventsAuditRepo{}
	s := prEventsTestServer(t, rr, ar)

	payload, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{
			"html_url":  "https://github.com/x/y/pull/42",
			"merged":    true,
			"merged_by": map[string]any{"login": "alice"},
		},
	})
	s.handlePullRequestClosed(context.Background(), payload)

	if len(rr.transitions) != 0 || len(ar.appended) != 0 {
		t.Errorf("unexpected side effects: transitions=%d audit=%d",
			len(rr.transitions), len(ar.appended))
	}
}

func TestPullRequestClosed_Merged_NoReviewStage_AuditOnlyNoTransition(t *testing.T) {
	// routine_change-style workflows are implement-only; merging
	// should still record the merge in the audit log but has no
	// review stage to transition.
	runID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: runID, PullRequestURL: &prURL}},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {{ID: uuid.New(), RunID: runID, Type: run.StageTypeImplement, State: run.StageStateSucceeded}},
		},
	}
	ar := &prEventsAuditRepo{}
	s := prEventsTestServer(t, rr, ar)

	payload, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{
			"html_url":  prURL,
			"merged":    true,
			"merged_by": map[string]any{"login": "alice"},
			"head":      map[string]any{"sha": "h"},
			"base":      map[string]any{"sha": "b"},
		},
	})
	s.handlePullRequestClosed(context.Background(), payload)

	if len(rr.transitions) != 0 {
		t.Errorf("transitions = %d, want 0 (no review stage)", len(rr.transitions))
	}
	row := findCategory(ar.appended, CategoryPRMerged)
	if row == nil {
		t.Fatalf("expected pr_merged audit row for implement-only run")
	}
	if row.StageID != nil {
		t.Errorf("audit StageID = %v, want nil (no review stage)", row.StageID)
	}
}

func TestPullRequestClosed_TransitionFailureLogged_AuditStillWritten(t *testing.T) {
	// State-machine rejection (e.g., review stage already in a
	// terminal state from a manual intervention) must NOT drop the
	// pr_merged audit row. The merge happened on GitHub; the chain
	// records it regardless of whether Fishhawk can advance the
	// stage.
	runID := uuid.New()
	reviewStageID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: runID, PullRequestURL: &prURL}},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {{ID: reviewStageID, RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval}},
		},
		transErr: errors.New("state machine refusal"),
	}
	ar := &prEventsAuditRepo{}
	s := prEventsTestServer(t, rr, ar)

	payload, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{
			"html_url":  prURL,
			"merged":    true,
			"merged_by": map[string]any{"login": "alice"},
			"head":      map[string]any{"sha": "h"},
			"base":      map[string]any{"sha": "b"},
		},
	})
	s.handlePullRequestClosed(context.Background(), payload)

	if findCategory(ar.appended, CategoryPRMerged) == nil {
		t.Errorf("pr_merged audit row should be written even when transition fails")
	}
}

// --- pull_request_review.submitted ---

func TestPullRequestReviewSubmitted_Approved_WritesApprovedAuditRow(t *testing.T) {
	runID := uuid.New()
	reviewStageID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: runID, PullRequestURL: &prURL}},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {{ID: reviewStageID, RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval}},
		},
	}
	ar := &prEventsAuditRepo{}
	s := prEventsTestServer(t, rr, ar)

	payload, _ := json.Marshal(map[string]any{
		"review": map[string]any{
			"user":  map[string]any{"login": "bob"},
			"state": "approved",
			"body":  "LGTM",
		},
		"pull_request": map[string]any{"html_url": prURL, "number": 42},
	})
	s.handlePullRequestReviewSubmitted(context.Background(), payload)

	row := findCategory(ar.appended, CategoryPRApprovedOnGitHub)
	if row == nil {
		t.Fatalf("expected pr_approved_on_github row; got %v", auditCategories(ar.appended))
	}
	if row.ActorSubject == nil || *row.ActorSubject != "bob" {
		t.Errorf("audit ActorSubject = %v, want bob", row.ActorSubject)
	}
	if row.StageID == nil || *row.StageID != reviewStageID {
		t.Errorf("audit StageID = %v, want %s", row.StageID, reviewStageID)
	}
	if len(rr.transitions) != 0 {
		t.Errorf("transitions = %d, want 0 (approve doesn't advance stage per ADR-018)", len(rr.transitions))
	}
}

func TestPullRequestReviewSubmitted_NonApprove_WritesGenericAuditRow(t *testing.T) {
	// changes_requested / commented / dismissed all land as
	// pr_review_submitted (the catch-all category). Lets the SPA
	// render the right verb without losing the event.
	runID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: runID, PullRequestURL: &prURL}},
		stages:     map[uuid.UUID][]*run.Stage{runID: nil},
	}
	for _, state := range []string{"changes_requested", "commented", "dismissed"} {
		t.Run(state, func(t *testing.T) {
			ar := &prEventsAuditRepo{}
			s := prEventsTestServer(t, rr, ar)
			payload, _ := json.Marshal(map[string]any{
				"review": map[string]any{
					"user":  map[string]any{"login": "bob"},
					"state": state,
					"body":  "comment body",
				},
				"pull_request": map[string]any{"html_url": prURL},
			})
			s.handlePullRequestReviewSubmitted(context.Background(), payload)
			row := findCategory(ar.appended, CategoryPRReviewSubmitted)
			if row == nil {
				t.Fatalf("expected pr_review_submitted row for state=%q; got %v",
					state, auditCategories(ar.appended))
			}
			if findCategory(ar.appended, CategoryPRApprovedOnGitHub) != nil {
				t.Errorf("non-approve state %q wrote an approve row", state)
			}
		})
	}
}

func TestPullRequestReviewSubmitted_NoMatchingRun_NoOp(t *testing.T) {
	rr := &prEventsRunRepo{listResult: nil}
	ar := &prEventsAuditRepo{}
	s := prEventsTestServer(t, rr, ar)

	payload, _ := json.Marshal(map[string]any{
		"review":       map[string]any{"user": map[string]any{"login": "bob"}, "state": "approved"},
		"pull_request": map[string]any{"html_url": "https://github.com/x/y/pull/42"},
	})
	s.handlePullRequestReviewSubmitted(context.Background(), payload)

	if len(ar.appended) != 0 {
		t.Errorf("audit rows = %d, want 0 (PR not Fishhawk-managed)", len(ar.appended))
	}
}

func TestPullRequestReviewSubmitted_LongBodyTruncated(t *testing.T) {
	runID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: runID, PullRequestURL: &prURL}},
		stages:     map[uuid.UUID][]*run.Stage{runID: nil},
	}
	ar := &prEventsAuditRepo{}
	s := prEventsTestServer(t, rr, ar)

	longBody := strings.Repeat("x", 1000)
	payload, _ := json.Marshal(map[string]any{
		"review": map[string]any{
			"user":  map[string]any{"login": "bob"},
			"state": "approved",
			"body":  longBody,
		},
		"pull_request": map[string]any{"html_url": prURL},
	})
	s.handlePullRequestReviewSubmitted(context.Background(), payload)

	row := findCategory(ar.appended, CategoryPRApprovedOnGitHub)
	if row == nil {
		t.Fatal("expected pr_approved_on_github row")
	}
	var body map[string]any
	_ = json.Unmarshal(row.Payload, &body)
	got, _ := body["body"].(string)
	if len(got) > reviewBodyExcerptMax+3 { // +3 for "..."
		t.Errorf("body excerpt len = %d, want <= %d", len(got), reviewBodyExcerptMax+3)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncated body should end with ellipsis; got %q", got[len(got)-10:])
	}
}

// --- ResolveReviewFromPollState (merge-reconciler poll path) ---

func TestResolveReviewFromPollState_Merged_TransitionsSucceeded(t *testing.T) {
	runID := uuid.New()
	reviewStageID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: runID, PullRequestURL: &prURL}},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {{ID: reviewStageID, RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval}},
		},
	}
	ar := &prEventsAuditRepo{}
	s := prEventsTestServer(t, rr, ar)

	if err := s.ResolveReviewFromPollState(context.Background(), runID, true, prURL); err != nil {
		t.Fatalf("ResolveReviewFromPollState: %v", err)
	}
	if len(rr.transitions) != 1 || rr.transitions[0].To != run.StageStateSucceeded {
		t.Fatalf("transitions = %+v, want one to succeeded", rr.transitions)
	}
	// The poll records the system marker, not a user login, but the
	// category is unchanged so consumers render identically.
	row := findCategory(ar.appended, CategoryPRMerged)
	if row == nil {
		t.Fatalf("missing pr_merged row; got %v", auditCategories(ar.appended))
	}
	if row.ActorSubject == nil || *row.ActorSubject != mergeReconcilerActor {
		t.Errorf("audit ActorSubject = %v, want %q", row.ActorSubject, mergeReconcilerActor)
	}
}

func TestResolveReviewFromPollState_ClosedUnmerged_TransitionsCancelled(t *testing.T) {
	runID := uuid.New()
	reviewStageID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: runID, PullRequestURL: &prURL}},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {{ID: reviewStageID, RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval}},
		},
	}
	ar := &prEventsAuditRepo{}
	s := prEventsTestServer(t, rr, ar)

	if err := s.ResolveReviewFromPollState(context.Background(), runID, false, prURL); err != nil {
		t.Fatalf("ResolveReviewFromPollState: %v", err)
	}
	if len(rr.transitions) != 1 || rr.transitions[0].To != run.StageStateCancelled {
		t.Fatalf("transitions = %+v, want one to cancelled", rr.transitions)
	}
	if findCategory(ar.appended, CategoryPRClosedWithoutMerge) == nil {
		t.Errorf("missing pr_closed_without_merge row; got %v", auditCategories(ar.appended))
	}
}

// TestResolveReviewFromPollState_Merged_DrivesRunToSucceeded is the
// seam regression for #727: resolveReviewStageOnMerge transitioned the
// review stage but never completed the RUN, leaving it {review
// succeeded, run running} forever. The guard wires ONE repo instance
// into BOTH Config.RunRepo and the Orchestrator and asserts the RUN
// reaches terminal succeeded — a per-layer unit on the stage transition
// alone passes while the bug is live.
func TestResolveReviewFromPollState_Merged_DrivesRunToSucceeded(t *testing.T) {
	runID := uuid.New()
	reviewStageID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: runID, State: run.StateRunning, PullRequestURL: &prURL}},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {
				{ID: uuid.New(), RunID: runID, Type: run.StageTypePlan, State: run.StageStateSucceeded},
				{ID: uuid.New(), RunID: runID, Type: run.StageTypeImplement, State: run.StageStateSucceeded},
				{ID: reviewStageID, RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval},
			},
		},
	}
	ar := &prEventsAuditRepo{}
	// Same rr instance into both surfaces — the seam under test.
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		AuditRepo:    ar,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})

	if err := s.ResolveReviewFromPollState(context.Background(), runID, true, prURL); err != nil {
		t.Fatalf("ResolveReviewFromPollState: %v", err)
	}

	// Review stage transitioned to succeeded.
	if len(rr.transitions) != 1 || rr.transitions[0].To != run.StageStateSucceeded {
		t.Fatalf("transitions = %+v, want one to succeeded", rr.transitions)
	}
	// AND the RUN reached terminal succeeded (the bug: it stayed running).
	if got := rr.runStates[runID]; got != run.StateSucceeded {
		t.Errorf("run state = %q, want succeeded (run must complete, not just the stage)", got)
	}
}

// TestResolveReviewFromPollState_ClosedUnmerged_DrivesRunToCancelled is
// the symmetric seam guard: a closed-unmerged PR cancels the review
// stage AND must drive the run to terminal cancelled.
func TestResolveReviewFromPollState_ClosedUnmerged_DrivesRunToCancelled(t *testing.T) {
	runID := uuid.New()
	reviewStageID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: runID, State: run.StateRunning, PullRequestURL: &prURL}},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {
				{ID: uuid.New(), RunID: runID, Type: run.StageTypePlan, State: run.StageStateSucceeded},
				{ID: uuid.New(), RunID: runID, Type: run.StageTypeImplement, State: run.StageStateSucceeded},
				{ID: reviewStageID, RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval},
			},
		},
	}
	ar := &prEventsAuditRepo{}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		AuditRepo:    ar,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})

	if err := s.ResolveReviewFromPollState(context.Background(), runID, false, prURL); err != nil {
		t.Fatalf("ResolveReviewFromPollState: %v", err)
	}

	if len(rr.transitions) != 1 || rr.transitions[0].To != run.StageStateCancelled {
		t.Fatalf("transitions = %+v, want one to cancelled", rr.transitions)
	}
	if got := rr.runStates[runID]; got != run.StateCancelled {
		t.Errorf("run state = %q, want cancelled", got)
	}
}

func TestResolveReviewFromPollState_RunNotFound_Errors(t *testing.T) {
	rr := &prEventsRunRepo{} // no seeded runs → GetRun returns ErrNotFound
	ar := &prEventsAuditRepo{}
	s := prEventsTestServer(t, rr, ar)

	if err := s.ResolveReviewFromPollState(context.Background(), uuid.New(), true, "https://github.com/x/y/pull/1"); err == nil {
		t.Fatal("expected an error when the run does not exist")
	}
}

// --- cross-path idempotency (webhook + poll on the SAME review stage) ---

func TestResolveReview_WebhookThenPoll_Merged_SingleEffectiveTransition(t *testing.T) {
	// Cross-boundary integration (#618 discipline): the pull_request.closed
	// webhook and the merge-reconciler poll share resolveReviewStageOnMerge,
	// so resolving the same review stage twice must yield exactly one
	// effective transition to succeeded.
	runID := uuid.New()
	reviewStageID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: runID, PullRequestURL: &prURL}},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {{ID: reviewStageID, RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval}},
		},
	}
	ar := &prEventsAuditRepo{}
	s := prEventsTestServer(t, rr, ar)

	payload, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{
			"html_url":  prURL,
			"merged":    true,
			"merged_by": map[string]any{"login": "alice"},
			"head":      map[string]any{"sha": "h"},
			"base":      map[string]any{"sha": "b"},
		},
		"sender": map[string]any{"login": "alice"},
	})
	s.handlePullRequestClosed(context.Background(), payload)
	if err := s.ResolveReviewFromPollState(context.Background(), runID, true, prURL); err != nil {
		t.Fatalf("ResolveReviewFromPollState: %v", err)
	}

	if len(rr.transitions) != 1 {
		t.Fatalf("transitions = %d, want 1 (webhook+poll idempotent)", len(rr.transitions))
	}
	if rr.transitions[0].To != run.StageStateSucceeded {
		t.Errorf("transition.To = %q, want succeeded", rr.transitions[0].To)
	}
}

func TestResolveReview_PollThenWebhook_ClosedUnmerged_SingleCancelled(t *testing.T) {
	// Reverse order + closed-unmerged: poll first, webhook second; both
	// must resolve to cancelled and only one effective transition lands.
	runID := uuid.New()
	reviewStageID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: runID, PullRequestURL: &prURL}},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {{ID: reviewStageID, RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval}},
		},
	}
	ar := &prEventsAuditRepo{}
	s := prEventsTestServer(t, rr, ar)

	if err := s.ResolveReviewFromPollState(context.Background(), runID, false, prURL); err != nil {
		t.Fatalf("ResolveReviewFromPollState: %v", err)
	}
	payload, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{
			"html_url": prURL,
			"merged":   false,
			"head":     map[string]any{"sha": "h"},
			"base":     map[string]any{"sha": "b"},
		},
		"sender": map[string]any{"login": "alice"},
	})
	s.handlePullRequestClosed(context.Background(), payload)

	if len(rr.transitions) != 1 {
		t.Fatalf("transitions = %d, want 1 (poll+webhook idempotent)", len(rr.transitions))
	}
	if rr.transitions[0].To != run.StageStateCancelled {
		t.Errorf("transition.To = %q, want cancelled", rr.transitions[0].To)
	}
}

// --- post_merge_observed (#1370) -------------------------------------------
//
// The run lifecycle owns its post-merge tail: resolveReviewStageOnMerge emits a
// post_merge_observed audit row once per ACTUALLY-resolved merge (review-gated
// and no-review alike), and NEVER for a merge held by the implement-review gate
// or a closed-without-merge resolution. next_actions keys the succeeded_merged
// state off that exact category string, so these tests pin the server end of
// the cross-binary seam.

// countCategory counts captured audit rows of the given category.
func countCategory(rows []audit.ChainAppendParams, category string) int {
	n := 0
	for _, r := range rows {
		if r.Category == category {
			n++
		}
	}
	return n
}

// (a) a review-gated merge resolution writes exactly one post_merge_observed
// row carrying the expected payload and the resolved review stage id.
func TestResolveReviewOnMerge_ReviewGated_WritesPostMergeObserved(t *testing.T) {
	runID := uuid.New()
	reviewStageID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: runID, PullRequestURL: &prURL}},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {
				{ID: uuid.New(), RunID: runID, Type: run.StageTypeImplement, State: run.StageStateSucceeded},
				{ID: reviewStageID, RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval},
			},
		},
	}
	ar := &prEventsAuditRepo{}
	s := prEventsTestServer(t, rr, ar)

	payload, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{
			"html_url":  prURL,
			"merged":    true,
			"merged_by": map[string]any{"login": "alice"},
			"head":      map[string]any{"sha": "headsha"},
			"base":      map[string]any{"sha": "basesha"},
		},
		"sender": map[string]any{"login": "alice"},
	})
	s.handlePullRequestClosed(context.Background(), payload)

	if n := countCategory(ar.appended, CategoryPostMergeObserved); n != 1 {
		t.Fatalf("post_merge_observed rows = %d, want exactly 1; got categories %v", n, auditCategories(ar.appended))
	}
	row := findCategory(ar.appended, CategoryPostMergeObserved)
	if row.RunID != runID {
		t.Errorf("audit RunID = %s, want %s", row.RunID, runID)
	}
	if row.StageID == nil || *row.StageID != reviewStageID {
		t.Errorf("audit StageID = %v, want the resolved review stage %s", row.StageID, reviewStageID)
	}
	if row.ActorKind == nil || *row.ActorKind != audit.ActorSystem {
		t.Errorf("audit ActorKind = %v, want system (lifecycle observation, not a user action)", row.ActorKind)
	}
	var body map[string]any
	if err := json.Unmarshal(row.Payload, &body); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if body["pr_url"] != prURL || body["merger"] != "alice" || body["head_sha"] != "headsha" || body["base_sha"] != "basesha" {
		t.Errorf("post_merge_observed payload missing expected fields: %+v", body)
	}
}

// (b) a no-review (implement-only) merge resolution writes one
// post_merge_observed row, carrying no stage id (no review stage on the shape).
func TestResolveReviewOnMerge_NoReviewStage_WritesPostMergeObserved(t *testing.T) {
	runID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: runID, PullRequestURL: &prURL}},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {{ID: uuid.New(), RunID: runID, Type: run.StageTypeImplement, State: run.StageStateSucceeded}},
		},
	}
	ar := &prEventsAuditRepo{}
	s := prEventsTestServer(t, rr, ar)

	payload, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{
			"html_url":  prURL,
			"merged":    true,
			"merged_by": map[string]any{"login": "alice"},
			"head":      map[string]any{"sha": "h"},
			"base":      map[string]any{"sha": "b"},
		},
	})
	s.handlePullRequestClosed(context.Background(), payload)

	if n := countCategory(ar.appended, CategoryPostMergeObserved); n != 1 {
		t.Fatalf("post_merge_observed rows = %d, want exactly 1; got %v", n, auditCategories(ar.appended))
	}
	if row := findCategory(ar.appended, CategoryPostMergeObserved); row.StageID != nil {
		t.Errorf("audit StageID = %v, want nil (no review stage)", row.StageID)
	}
}

// (c) a merge HELD by the unsettled implement-review gate writes NO
// post_merge_observed row — the run stays parked, so nothing resolved. Drives
// the full gate-plus-resolver seam via the implement-review-gate harness.
func TestResolveReviewOnMerge_HeldByReviewGate_NoPostMergeObserved(t *testing.T) {
	s, _, ar, runID, _ := newImplementReviewGateRun(t, 1)
	ar.seedCategory("implement_review_started", time.Now().UTC())

	if err := s.ResolveReviewFromPollState(context.Background(), runID, true, implementReviewGatePRURL); err != nil {
		t.Fatalf("ResolveReviewFromPollState: %v", err)
	}
	for _, e := range ar.appended {
		if e.Category == CategoryPostMergeObserved {
			t.Fatalf("post_merge_observed written while the merge is held pending implement review; want none")
		}
	}
}

// (d) a closed-without-merge resolution writes NO post_merge_observed row —
// the tail event fires only on an actually-merged resolution.
func TestResolveReviewOnMerge_ClosedWithoutMerge_NoPostMergeObserved(t *testing.T) {
	runID := uuid.New()
	reviewStageID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: runID, PullRequestURL: &prURL}},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {{ID: reviewStageID, RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval}},
		},
	}
	ar := &prEventsAuditRepo{}
	s := prEventsTestServer(t, rr, ar)

	payload, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{
			"html_url": prURL,
			"merged":   false,
			"head":     map[string]any{"sha": "h"},
			"base":     map[string]any{"sha": "b"},
		},
		"sender": map[string]any{"login": "alice"},
	})
	s.handlePullRequestClosed(context.Background(), payload)

	if n := countCategory(ar.appended, CategoryPostMergeObserved); n != 0 {
		t.Fatalf("post_merge_observed rows = %d, want 0 on a closed-without-merge resolution; got %v", n, auditCategories(ar.appended))
	}
}

// TestResolveReviewOnMerge_NoReviewStage_EmitsRunMergedBoardTransition is the
// #1815 done-means assertion for step 1: an implement-only (no review stage)
// merge must advance the work item to Done via the run_merged board edge —
// exactly like the review-path sibling. It drives the full webhook-resolver ->
// board-sync hook -> registered Transitioner seam and asserts BOTH the single
// run_merged/Done Transition call AND a work_item_transitioned audit row.
// Against the pre-fix code (no notifyBoardTransition in the no-review branch)
// it fails: neither the Transition call nor the audit row is emitted.
func TestResolveReviewOnMerge_NoReviewStage_EmitsRunMergedBoardTransition(t *testing.T) {
	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) { return workmgmt.Default(), nil }
	t.Cleanup(func() { conventionsLoader = prev })

	fp := &fakeTransitionProvider{}
	registerTransitionProvider(t, fp)

	runID := uuid.New()
	prURL := "https://github.com/kuhlman-labs/fishhawk/pull/42"
	inst := int64(99)
	ref := "issue:1815"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{
			ID:             runID,
			Repo:           "kuhlman-labs/fishhawk",
			PullRequestURL: &prURL,
			TriggerRef:     &ref,
			InstallationID: &inst,
		}},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {{ID: uuid.New(), RunID: runID, Type: run.StageTypeImplement, State: run.StageStateSucceeded}},
		},
	}
	ar := &prEventsAuditRepo{}
	s := prEventsTestServer(t, rr, ar)

	payload, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{
			"html_url":  prURL,
			"merged":    true,
			"merged_by": map[string]any{"login": "alice"},
			"head":      map[string]any{"sha": "h"},
			"base":      map[string]any{"sha": "b"},
		},
	})
	s.handlePullRequestClosed(context.Background(), payload)

	if len(fp.calls) != 1 {
		t.Fatalf("Transition calls = %d, want exactly 1 (the run_merged/Done board move)", len(fp.calls))
	}
	got := fp.calls[0]
	if got.Trigger != lifecycleRunMerged {
		t.Errorf("trigger = %q, want %q", got.Trigger, lifecycleRunMerged)
	}
	if got.CanonicalState != workmgmt.CanonicalStateDone {
		t.Errorf("canonical state = %q, want %q", got.CanonicalState, workmgmt.CanonicalStateDone)
	}
	if got.IssueNumber != 1815 {
		t.Errorf("issue number = %d, want 1815", got.IssueNumber)
	}
	if n := countCategory(ar.appended, categoryWorkItemTransitioned); n != 1 {
		t.Fatalf("work_item_transitioned rows = %d, want exactly 1; got %v", n, auditCategories(ar.appended))
	}
}

// auditCategories returns the categories of the captured audit
// rows for use in failure-message context. Tiny helper; saves the
// caller from inlining the loop in every assert.
func auditCategories(rows []audit.ChainAppendParams) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Category)
	}
	return out
}

// --- economics stamp (#1702) ---

// stampGitHub is a minimal GitHub stub for the merge-time economics stamp: it
// serves the PR body on GET and captures the PATCH body on edit.
type stampGitHub struct {
	mu          sync.Mutex
	getBody     string
	getStatus   int
	getCalled   bool
	patchStatus int
	patchCalled bool
	patchBody   string
}

func newStampGitHubClient(t *testing.T, stub *stampGitHub) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}",
		func(w http.ResponseWriter, _ *http.Request) {
			stub.mu.Lock()
			stub.getCalled = true
			body, st := stub.getBody, stub.getStatus
			stub.mu.Unlock()
			if st != 0 && st != http.StatusOK {
				w.WriteHeader(st)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			raw, _ := json.Marshal(map[string]any{
				"node_id": "PR_x", "state": "closed", "merged": true,
				"body": body,
				"head": map[string]any{"sha": "h"},
				"base": map[string]any{"ref": "main"},
			})
			_, _ = w.Write(raw)
		})
	mux.HandleFunc("PATCH /repos/{owner}/{repo}/pulls/{number}",
		func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			var p struct {
				Body string `json:"body"`
			}
			_ = json.Unmarshal(raw, &p)
			stub.mu.Lock()
			stub.patchCalled = true
			stub.patchBody = p.Body
			st := stub.patchStatus
			stub.mu.Unlock()
			if st != 0 && st != http.StatusOK {
				w.WriteHeader(st)
				return
			}
			w.WriteHeader(http.StatusOK)
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
}

// stampChain seeds a run chain with a cost_recorded row and the plan-approval
// gate boundary + a pr_merged terminal, so the derived economics block is
// non-empty.
func stampChain(runID uuid.UUID) []*audit.Entry {
	mk := func(seq int64, cat string, ts int64, payload map[string]any) *audit.Entry {
		var raw json.RawMessage
		if payload != nil {
			raw, _ = json.Marshal(payload)
		}
		rid := runID
		return &audit.Entry{RunID: &rid, Sequence: seq, Category: cat, Timestamp: time.Unix(ts, 0).UTC(), Payload: raw}
	}
	return []*audit.Entry{
		mk(1, "plan_generated", 100, nil),
		mk(2, "approval_submitted", 2800, map[string]any{"decision": "approve"}), // plan approval = 2700
		mk(3, "cost_recorded", 2900, map[string]any{"usd": 0.30, "model": "claude-opus-4-8", "input_tokens": 500}),
		mk(4, "pr_merged", 7900, nil), // wall clock = 7800
	}
}

func stampMergedPayload(prURL string) []byte {
	payload, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{
			"html_url":  prURL,
			"number":    42,
			"merged":    true,
			"merged_by": map[string]any{"login": "alice"},
			"head":      map[string]any{"sha": "headsha"},
			"base":      map[string]any{"sha": "basesha"},
		},
		"sender": map[string]any{"login": "alice"},
	})
	return payload
}

// TestEconomicsStamp_EditsPRBodyOnMerge is the happy path: an observed merge
// splices the economics section into the PR body via EditPullRequest.
func TestEconomicsStamp_EditsPRBodyOnMerge(t *testing.T) {
	runID := uuid.New()
	reviewStageID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	instID := int64(99)
	runRow := &run.Run{
		ID:             runID,
		Repo:           "x/y",
		PullRequestURL: &prURL,
		InstallationID: &instID,
		CreatedAt:      time.Unix(100, 0).UTC(),
		CostUSDTotal:   0.30,
	}
	rr := &prEventsRunRepo{
		listResult: []*run.Run{runRow},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {
				{ID: uuid.New(), RunID: runID, Type: run.StageTypeImplement, State: run.StageStateSucceeded},
				{ID: reviewStageID, RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval},
			},
		},
	}
	ar := &prEventsAuditRepo{listForRun: stampChain(runID)}
	stub := &stampGitHub{}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: ar, GitHub: newStampGitHubClient(t, stub)})

	s.handlePullRequestClosed(context.Background(), stampMergedPayload(prURL))

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if !stub.patchCalled {
		t.Fatal("EditPullRequest was not called on merge")
	}
	for _, want := range []string{economicsMarkerBegin, economicsMarkerEnd, "**Economics**", "Total cost"} {
		if !strings.Contains(stub.patchBody, want) {
			t.Errorf("PATCH body missing %q:\n%s", want, stub.patchBody)
		}
	}
}

// TestEconomicsStamp_EditErrorDoesNotBlockResolution is the best-effort
// defensive branch: an EditPullRequest failure must NOT unwind the review-gate
// resolution — the review stage still transitions to succeeded.
func TestEconomicsStamp_EditErrorDoesNotBlockResolution(t *testing.T) {
	runID := uuid.New()
	reviewStageID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	instID := int64(99)
	runRow := &run.Run{
		ID:             runID,
		Repo:           "x/y",
		PullRequestURL: &prURL,
		InstallationID: &instID,
		CreatedAt:      time.Unix(100, 0).UTC(),
		CostUSDTotal:   0.30,
	}
	rr := &prEventsRunRepo{
		listResult: []*run.Run{runRow},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {
				{ID: reviewStageID, RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval},
			},
		},
	}
	ar := &prEventsAuditRepo{listForRun: stampChain(runID)}
	stub := &stampGitHub{patchStatus: http.StatusInternalServerError}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: ar, GitHub: newStampGitHubClient(t, stub)})

	s.handlePullRequestClosed(context.Background(), stampMergedPayload(prURL))

	// Despite the PATCH failure, the review stage resolved to succeeded.
	if len(rr.transitions) != 1 || rr.transitions[0].StageID != reviewStageID || rr.transitions[0].To != run.StageStateSucceeded {
		t.Fatalf("review stage did not resolve to succeeded despite best-effort stamp failure: %+v", rr.transitions)
	}
	// The pr_merged audit row is still present.
	if findCategory(ar.appended, CategoryPRMerged) == nil {
		t.Errorf("pr_merged audit row missing; stamp failure must not unwind the merge")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if !stub.patchCalled {
		t.Error("expected EditPullRequest to have been attempted")
	}
}

// TestEconomicsStamp_IdempotentOnReObservedMerge asserts a re-observed merge
// whose PR body already carries the identical economics section skips the
// PATCH entirely — replace-not-duplicate.
func TestEconomicsStamp_IdempotentOnReObservedMerge(t *testing.T) {
	runID := uuid.New()
	reviewStageID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	instID := int64(99)
	runRow := &run.Run{
		ID:             runID,
		Repo:           "x/y",
		PullRequestURL: &prURL,
		InstallationID: &instID,
		CreatedAt:      time.Unix(100, 0).UTC(),
		CostUSDTotal:   0.30,
	}
	chain := stampChain(runID)
	// Precompute the block the stamp would derive, and seed the PR body as if
	// a prior stamp already wrote it.
	block := issuecomment.RenderEconomicsBlock(*issuecomment.BuildRunEconomics(runRow, chain, nil))
	if block == "" {
		t.Fatal("precondition: derived block must be non-empty")
	}
	existing := spliceEconomicsSection("Original description.", block)

	rr := &prEventsRunRepo{
		listResult: []*run.Run{runRow},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {{ID: reviewStageID, RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval}},
		},
	}
	ar := &prEventsAuditRepo{listForRun: chain}
	stub := &stampGitHub{getBody: existing}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: ar, GitHub: newStampGitHubClient(t, stub)})

	s.handlePullRequestClosed(context.Background(), stampMergedPayload(prURL))

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.patchCalled {
		t.Errorf("re-observed merge with identical section must skip the PATCH; got body:\n%s", stub.patchBody)
	}
}

// childCostChain builds a single-entry cost_recorded ledger for a decomposition
// slice child fixture.
func childCostChain(runID uuid.UUID, usd float64, cacheRead int) []*audit.Entry {
	rid := runID
	raw, _ := json.Marshal(map[string]any{
		"usd": usd, "source": "agent", "model": "claude-opus-4-8",
		"input_tokens": 100, "output_tokens": 50, "cache_read_input_tokens": cacheRead,
	})
	return []*audit.Entry{{RunID: &rid, Sequence: 1, Category: "cost_recorded", Timestamp: time.Unix(3000, 0).UTC(), Payload: raw}}
}

// TestDeriveEconomicsBlock_RollsUpLineageMatchesNotifier is the cross-boundary
// same-figures assertion (criterion 5 / #2100): the server's merge-time
// deriveEconomicsBlock rolls the decomposition lineage in from the SAME walk
// the living anchor uses, so its rendered block is byte-identical to the
// notifier-path block for the same parent + children fixture, and reflects the
// rolled total (parent + both slices).
func TestDeriveEconomicsBlock_RollsUpLineageMatchesNotifier(t *testing.T) {
	parentID := uuid.New()
	sliceZero := 0
	sliceOne := 1
	child0 := &run.Run{ID: uuid.New(), CreatedAt: time.Unix(120, 0).UTC(), CostUSDTotal: 40.00, DecomposedFrom: &parentID, SliceIndex: &sliceZero}
	child1 := &run.Run{ID: uuid.New(), CreatedAt: time.Unix(130, 0).UTC(), CostUSDTotal: 44.54, DecomposedFrom: &parentID, SliceIndex: &sliceOne}
	parent := &run.Run{ID: parentID, Repo: "x/y", CreatedAt: time.Unix(100, 0).UTC(), CostUSDTotal: 9.29}

	parentChain := stampChain(parentID)
	rr := &prEventsRunRepo{
		listResult:       []*run.Run{parent},
		decomposedResult: []*run.Run{child0, child1},
	}
	ar := &prEventsAuditRepo{
		listForRun: parentChain,
		byCategory: map[uuid.UUID][]*audit.Entry{
			child0.ID: childCostChain(child0.ID, 40.00, 3000),
			child1.ID: childCostChain(child1.ID, 44.54, 2000),
		},
	}
	s := prEventsTestServer(t, rr, ar)

	got := s.deriveEconomicsBlock(context.Background(), parent)

	// Independent notifier-path render over the SAME lineage.
	children, err := issuecomment.LoadChildRunEconomics(context.Background(), rr, ar, parentID)
	if err != nil {
		t.Fatalf("LoadChildRunEconomics: %v", err)
	}
	want := issuecomment.RenderEconomicsBlock(*issuecomment.BuildRunEconomics(parent, parentChain, children))

	if got != want {
		t.Errorf("server stamp block diverges from the notifier-path block:\n--- server ---\n%s\n--- notifier ---\n%s", got, want)
	}
	// The rolled total (9.29 + 40.00 + 44.54) and the per-run breakdown are present.
	for _, sub := range []string{"**Total cost**: $93.83", "- **By run**:", "parent (", "slice 1 (", "slice 2 ("} {
		if !strings.Contains(got, sub) {
			t.Errorf("rolled block missing %q:\n%s", sub, got)
		}
	}
}

// TestDeriveEconomicsBlock_ChildListError_DegradesToSingleRun covers the
// server-seam lineage-load failure branch (#2100): when the DecomposedFrom walk
// itself errors, deriveEconomicsBlock warn-logs and falls back to the
// single-run block (children=nil) — the parent's own figures with NO By-run
// breakdown — rather than dropping the economics stamp. This is the one error
// path the change introduces at the server call site.
func TestDeriveEconomicsBlock_ChildListError_DegradesToSingleRun(t *testing.T) {
	parentID := uuid.New()
	parent := &run.Run{ID: parentID, Repo: "x/y", CreatedAt: time.Unix(100, 0).UTC(), CostUSDTotal: 9.29}
	parentChain := stampChain(parentID)
	rr := &prEventsRunRepo{
		listResult: []*run.Run{parent},
		listErr:    errors.New("list boom"), // fails the DecomposedFrom walk
	}
	ar := &prEventsAuditRepo{listForRun: parentChain}
	s := prEventsTestServer(t, rr, ar)

	got := s.deriveEconomicsBlock(context.Background(), parent)

	// Byte-identical to the single-run block over the parent's own chain.
	want := issuecomment.RenderEconomicsBlock(*issuecomment.BuildRunEconomics(parent, parentChain, nil))
	if got != want {
		t.Errorf("degraded block != single-run block:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if strings.Contains(got, "**By run**") {
		t.Errorf("a child-list error must degrade to single-run (no By-run breakdown):\n%s", got)
	}
	if !strings.Contains(got, "$9.29") {
		t.Errorf("expected the parent's own total in the degraded block:\n%s", got)
	}
}

// TestSpliceEconomicsSection covers the splice branches directly: append into
// a plain body, replace an existing section (idempotent identity), append into
// an empty body, and recover from a corrupted begin-marker-without-end.
func TestSpliceEconomicsSection(t *testing.T) {
	block := "FIRSTBLK"
	section := economicsMarkerBegin + "\n" + block + "\n" + economicsMarkerEnd

	// Append into a plain body.
	appended := spliceEconomicsSection("Hello.", block)
	if !strings.Contains(appended, "Hello.") || !strings.Contains(appended, section) {
		t.Errorf("append lost original or section:\n%s", appended)
	}

	// Idempotent: re-splicing the already-stamped body yields the identical body.
	again := spliceEconomicsSection(appended, block)
	if again != appended {
		t.Errorf("re-splice not idempotent:\nfirst:  %q\nsecond: %q", appended, again)
	}

	// A changed block replaces the section in place (single section, no dup).
	replaced := spliceEconomicsSection(appended, "SECONDBLK")
	if strings.Count(replaced, economicsMarkerBegin) != 1 {
		t.Errorf("replace must keep exactly one section:\n%s", replaced)
	}
	if !strings.Contains(replaced, "SECONDBLK") || strings.Contains(replaced, "FIRSTBLK") {
		t.Errorf("replace did not swap the block content:\n%s", replaced)
	}

	// Empty body → just the section.
	if got := spliceEconomicsSection("", block); got != section {
		t.Errorf("empty body should render just the section; got %q", got)
	}

	// Corrupted: begin marker without an end. The splice truncates from the
	// begin marker so a single well-formed section results.
	corrupted := "Body.\n\n" + economicsMarkerBegin + "\nleftover"
	fixed := spliceEconomicsSection(corrupted, block)
	if strings.Count(fixed, economicsMarkerBegin) != 1 || !strings.Contains(fixed, economicsMarkerEnd) {
		t.Errorf("corrupted body should recover to one well-formed section:\n%s", fixed)
	}
	if strings.Contains(fixed, "leftover") {
		t.Errorf("corrupted trailing content should be dropped:\n%s", fixed)
	}
}

// TestEconomicsStamp_EmptyBlockSkipsGitHub is the empty-block guard: a run with
// no economics signal derives an empty block, so the stamp returns before any
// GitHub call.
func TestEconomicsStamp_EmptyBlockSkipsGitHub(t *testing.T) {
	runID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	instID := int64(99)
	runRow := &run.Run{
		ID: runID, Repo: "x/y", PullRequestURL: &prURL, InstallationID: &instID,
		CreatedAt: time.Unix(100, 0).UTC(),
	}
	rr := &prEventsRunRepo{
		listResult: []*run.Run{runRow},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {{ID: uuid.New(), RunID: runID, Type: run.StageTypeImplement, State: run.StageStateSucceeded}},
		},
	}
	// A chain with no cost / gate signal → empty block.
	ar := &prEventsAuditRepo{listForRun: []*audit.Entry{{Sequence: 1, Category: "stage_dispatched", Timestamp: time.Unix(200, 0).UTC()}}}
	stub := &stampGitHub{}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: ar, GitHub: newStampGitHubClient(t, stub)})

	s.handlePullRequestClosed(context.Background(), stampMergedPayload(prURL))

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.getCalled || stub.patchCalled {
		t.Errorf("empty block must skip all GitHub calls; get=%v patch=%v", stub.getCalled, stub.patchCalled)
	}
}

// TestEconomicsStamp_GetPRErrorDoesNotBlock is the read-failure branch: a
// GetPullRequest error warn-logs and returns without a PATCH, and the
// review-gate resolution is unaffected.
func TestEconomicsStamp_GetPRErrorDoesNotBlock(t *testing.T) {
	runID := uuid.New()
	reviewStageID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	instID := int64(99)
	runRow := &run.Run{
		ID: runID, Repo: "x/y", PullRequestURL: &prURL, InstallationID: &instID,
		CreatedAt: time.Unix(100, 0).UTC(), CostUSDTotal: 0.30,
	}
	rr := &prEventsRunRepo{
		listResult: []*run.Run{runRow},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {{ID: reviewStageID, RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval}},
		},
	}
	ar := &prEventsAuditRepo{listForRun: stampChain(runID)}
	stub := &stampGitHub{getStatus: http.StatusInternalServerError}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: ar, GitHub: newStampGitHubClient(t, stub)})

	s.handlePullRequestClosed(context.Background(), stampMergedPayload(prURL))

	if len(rr.transitions) != 1 || rr.transitions[0].To != run.StageStateSucceeded {
		t.Fatalf("GET failure must not block resolution: %+v", rr.transitions)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.patchCalled {
		t.Error("PATCH must not fire after a GET failure")
	}
}

// TestStampEconomicsIntoPRBody_MissingPRURLSkips is the missing-coordinates
// guard: a run without a PR URL never touches GitHub.
func TestStampEconomicsIntoPRBody_MissingPRURLSkips(t *testing.T) {
	runID := uuid.New()
	instID := int64(99)
	runRow := &run.Run{ID: runID, Repo: "x/y", InstallationID: &instID, CreatedAt: time.Unix(100, 0).UTC(), CostUSDTotal: 0.30}
	ar := &prEventsAuditRepo{listForRun: stampChain(runID)}
	stub := &stampGitHub{}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: &prEventsRunRepo{}, AuditRepo: ar, GitHub: newStampGitHubClient(t, stub)})

	s.stampEconomicsIntoPRBody(context.Background(), runRow)

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.getCalled || stub.patchCalled {
		t.Errorf("run without PR URL must skip GitHub; get=%v patch=%v", stub.getCalled, stub.patchCalled)
	}
}

// --- E64.2 / #3083: the merge-supersede sweep on the merged paths ---
//
// A merge can leave a stage permanently unreachable rather than failed: a
// fix-up pass re-parks acceptance at awaiting_host_dispatch, the PR then
// merges, nothing re-dispatches the stage, and Orchestrator.completeRun's
// #968 guard correctly refuses to complete the run around it. The sweep
// terminalizes exactly the default-deny-table-admissible parked stages as
// `superseded` so the Advance on the same pass completes the run.

// The supersede assertions below reuse countCategory (above) and are
// EXACTLY-ONE assertions, not at-least-one: a duplicated row is as much a
// defect as a missing one.

// stageStateByID reads the fake repo's live stage state, which is what the
// transitions actually committed — asserting on COMMITTED STATE rather than
// on a returned error, per the counterfactual contract's trap (a).
func stageStateByID(t *testing.T, rr *prEventsRunRepo, runID, stageID uuid.UUID) run.StageState {
	t.Helper()
	sts, err := rr.ListStagesForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListStagesForRun: %v", err)
	}
	for _, st := range sts {
		if st.ID == stageID {
			return st.State
		}
	}
	t.Fatalf("stage %s not found on run %s", stageID, runID)
	return ""
}

// mergeSweepFixture seeds the shape the issue describes and returns a server
// wired with a REAL orchestrator, so the completion re-evaluation the sweep
// exists to unblock is genuinely exercised rather than stubbed.
func mergeSweepFixture(t *testing.T, stages []*run.Stage) (*Server, *prEventsRunRepo, *prEventsAuditRepo, uuid.UUID) {
	t.Helper()
	runID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	for _, st := range stages {
		st.RunID = runID
	}
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: runID, State: run.StateRunning, PullRequestURL: &prURL}},
		stages:     map[uuid.UUID][]*run.Stage{runID: stages},
	}
	ar := &prEventsAuditRepo{}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		AuditRepo:    ar,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})
	return s, rr, ar, runID
}

// TestResolveReviewStageOnMerge_SupersedesParkedAcceptanceAndCompletesRun is
// THE primary shape — the exact situation that stranded four live runs. It
// asserts across every layer the sweep touches in one pass: the acceptance
// stage's COMMITTED state is `superseded`, exactly one
// stage_superseded_by_merge row names it with its from-state and the
// merge_observed reason, and the RUN reached terminal succeeded on the same
// pass (which is only possible because the sweep ran BEFORE the Advance).
func TestResolveReviewStageOnMerge_SupersedesParkedAcceptanceAndCompletesRun(t *testing.T) {
	acceptanceID := uuid.New()
	reviewID := uuid.New()
	s, rr, ar, runID := mergeSweepFixture(t, []*run.Stage{
		{ID: uuid.New(), Type: run.StageTypePlan, State: run.StageStateSucceeded},
		{ID: uuid.New(), Type: run.StageTypeImplement, State: run.StageStateSucceeded},
		{ID: acceptanceID, Type: run.StageTypeAcceptance, State: run.StageStateAwaitingHostDispatch},
		{ID: reviewID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval},
	})

	if err := s.ResolveReviewFromPollState(context.Background(), runID,
		true, "https://github.com/x/y/pull/42"); err != nil {
		t.Fatalf("ResolveReviewFromPollState: %v", err)
	}

	if got := stageStateByID(t, rr, runID, acceptanceID); got != run.StageStateSuperseded {
		t.Errorf("acceptance stage state = %q, want superseded", got)
	}
	if n := countCategory(ar.appended, CategoryStageSupersededByMerge); n != 1 {
		t.Fatalf("stage_superseded_by_merge rows = %d, want exactly 1", n)
	}
	row := findCategory(ar.appended, CategoryStageSupersededByMerge)
	if row.StageID == nil || *row.StageID != acceptanceID {
		t.Errorf("audit row stage_id = %v, want the acceptance stage %s", row.StageID, acceptanceID)
	}
	var payload map[string]any
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatalf("unmarshal supersede payload: %v", err)
	}
	if payload["stage_type"] != string(run.StageTypeAcceptance) {
		t.Errorf("payload stage_type = %v, want acceptance", payload["stage_type"])
	}
	if payload["from_state"] != string(run.StageStateAwaitingHostDispatch) {
		t.Errorf("payload from_state = %v, want awaiting_host_dispatch", payload["from_state"])
	}
	if payload["reason"] != supersedeReasonMergeObserved {
		t.Errorf("payload reason = %v, want %s", payload["reason"], supersedeReasonMergeObserved)
	}
	// THE POINT: the run completes on this same pass. Without the sweep the
	// #968 guard refuses around the parked acceptance stage and the run stays
	// `running` with a merged PR — the defect this change closes.
	if got := rr.runStates[runID]; got != run.StateSucceeded {
		t.Errorf("run state = %q, want succeeded", got)
	}
}

// TestResolveReviewStageOnMerge_ReviewStageSucceededNotSuperseded pins that
// the review stage this path resolves itself ends `succeeded`, never
// `superseded` — the two are NOT interchangeable, one says the change was
// accepted and the other says the gate was dissolved.
//
// COUNTERFACTUAL RECORD (approval condition 2(b)). The operator was right,
// and it was settled empirically rather than by argument: an earlier draft
// passed the resolved review stage's id as skipStageID, and DELETING that
// argument left this test GREEN. The sweep runs AFTER the review stage has
// already transitioned to `succeeded`, and the default-deny table admits
// review only at `awaiting_approval`, so the classification denies the stage
// whether or not it was skipped. The dead argument was therefore NOT shipped
// — the call site passes nil.
//
// This test survives that removal because it pins the OUTCOME, not the
// mechanism, and it is attainable against two live mutations:
//
//  1. Hoisting the sweep ABOVE the review-stage transition in
//     resolveReviewStageOnMerge. That is the one reordering that makes the
//     stage classifiable (review@awaiting_approval IS a table row): the sweep
//     supersedes it, the subsequent transition to `succeeded` is then an
//     invalid transition out of a terminal state, and this goes RED.
//  2. Widening the sweep's default-deny check (merge_supersede.go's
//     run.MergeSupersedable call) to a state-only or unconditional predicate,
//     which claims the review stage too. That is the mutation the sibling
//     TestResolveReviewStageOnMerge_LeavesRunningImplementStageUntouched
//     pins from the other side.
func TestResolveReviewStageOnMerge_ReviewStageSucceededNotSuperseded(t *testing.T) {
	acceptanceID := uuid.New()
	reviewID := uuid.New()
	s, rr, _, runID := mergeSweepFixture(t, []*run.Stage{
		{ID: uuid.New(), Type: run.StageTypeImplement, State: run.StageStateSucceeded},
		{ID: acceptanceID, Type: run.StageTypeAcceptance, State: run.StageStateAwaitingHostDispatch},
		{ID: reviewID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval},
	})

	if err := s.ResolveReviewFromPollState(context.Background(), runID,
		true, "https://github.com/x/y/pull/42"); err != nil {
		t.Fatalf("ResolveReviewFromPollState: %v", err)
	}

	if got := stageStateByID(t, rr, runID, reviewID); got != run.StageStateSucceeded {
		t.Errorf("review stage state = %q, want succeeded (the merge path resolves it; the sweep must not claim it)", got)
	}
	// The sibling stage the sweep IS meant to take, so a vacuously-inert
	// sweep cannot green the assertion above.
	if got := stageStateByID(t, rr, runID, acceptanceID); got != run.StageStateSuperseded {
		t.Errorf("acceptance stage state = %q, want superseded (sweep must be live for this test to discriminate)", got)
	}
}

// TestResolveReviewStageOnMerge_LeavesRunningImplementStageUntouched is the
// #968 invariant pinned FROM THE SWEEP SIDE. A merge must never terminalize a
// stage that is genuinely still working: an implement stage at `running` is
// not unreachable, it is in flight, and superseding it would let completeRun
// stamp the run `succeeded` around work that never finished.
//
// Discriminating mutation: widening the default-deny table (or dropping the
// run.MergeSupersedable check in the sweep) to admit `running`.
func TestResolveReviewStageOnMerge_LeavesRunningImplementStageUntouched(t *testing.T) {
	implementID := uuid.New()
	reviewID := uuid.New()
	s, rr, ar, runID := mergeSweepFixture(t, []*run.Stage{
		{ID: uuid.New(), Type: run.StageTypePlan, State: run.StageStateSucceeded},
		{ID: implementID, Type: run.StageTypeImplement, State: run.StageStateRunning},
		{ID: reviewID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval},
	})

	if err := s.ResolveReviewFromPollState(context.Background(), runID,
		true, "https://github.com/x/y/pull/42"); err != nil {
		t.Fatalf("ResolveReviewFromPollState: %v", err)
	}

	if got := stageStateByID(t, rr, runID, implementID); got != run.StageStateRunning {
		t.Errorf("implement stage state = %q, want running (a merge must not terminalize in-flight work)", got)
	}
	if n := countCategory(ar.appended, CategoryStageSupersededByMerge); n != 0 {
		t.Errorf("stage_superseded_by_merge rows = %d, want 0", n)
	}
	// And the run correctly does NOT complete: the #968 guard still refuses
	// around the non-terminal implement stage.
	if got := rr.runStates[runID]; got == run.StateSucceeded {
		t.Errorf("run state = %q, want the completion guard to still refuse around the running implement stage", got)
	}
}

// TestResolveReviewStageOnMerge_NoAuditRowWhenCASRefuses pins TRANSITION-FIRST
// ORDERING. The audit chain is append-only, so a row written before a
// compare-and-swap that then refuses would be an immutable record of a
// supersession that never happened. The failure mode must be a MISSING row,
// never a false one.
//
// The refusal is seeded BY CONSTRUCTION, not by calling the control: the
// beforeCAS hook flips the acceptance stage to `failed` inside
// TransitionStageFrom, in exactly the window a concurrent writer would use,
// so the CAS finds a current state that is not the `from` the sweep pinned.
//
// Discriminating mutation: moving appendStageSupersededAudit ahead of the
// TransitionStageFrom call in merge_supersede.go.
func TestResolveReviewStageOnMerge_NoAuditRowWhenCASRefuses(t *testing.T) {
	acceptanceID := uuid.New()
	reviewID := uuid.New()
	s, rr, ar, runID := mergeSweepFixture(t, []*run.Stage{
		{ID: uuid.New(), Type: run.StageTypeImplement, State: run.StageStateSucceeded},
		{ID: acceptanceID, Type: run.StageTypeAcceptance, State: run.StageStateAwaitingHostDispatch},
		{ID: reviewID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval},
	})
	rr.beforeCAS = func(id uuid.UUID) {
		if id != acceptanceID {
			return
		}
		// Concurrent writer lands between the sweep's classification and
		// its CAS. Caller holds r.mu, so write curState directly.
		if rr.curState == nil {
			rr.curState = map[uuid.UUID]run.StageState{}
		}
		rr.curState[acceptanceID] = run.StageStateFailed
	}

	if err := s.ResolveReviewFromPollState(context.Background(), runID,
		true, "https://github.com/x/y/pull/42"); err != nil {
		t.Fatalf("ResolveReviewFromPollState: %v", err)
	}

	if n := countCategory(ar.appended, CategoryStageSupersededByMerge); n != 0 {
		t.Fatalf("stage_superseded_by_merge rows = %d, want 0 — a refused CAS must leave a MISSING row, never a false one", n)
	}
	if got := stageStateByID(t, rr, runID, acceptanceID); got != run.StageStateFailed {
		t.Errorf("acceptance stage state = %q, want failed (the CAS must refuse rather than overwrite the concurrent writer)", got)
	}
	// The merge itself still resolved — the sweep is a best-effort tail and
	// never unwinds the resolution or its audit row.
	if findCategory(ar.appended, "pr_merged") == nil {
		t.Error("pr_merged row missing: a refused sweep must not unwind the merge resolution")
	}
}

// TestResolveReviewStageOnMerge_ClosedWithoutMergeSweepsNothing pins that the
// sweep is bound to MERGE, not to PR closure. A change that was closed
// without merging leaves no stage unreachable-because-merged: the run
// resolves to cancelled and the parked acceptance stage must keep its state,
// so a reopened-and-remerged change is still dispatchable.
//
// Discriminating mutation: hoisting the sweep out of the merged branch to the
// top of resolveReviewStageOnMerge (or adding it to the closed-unmerged tail).
func TestResolveReviewStageOnMerge_ClosedWithoutMergeSweepsNothing(t *testing.T) {
	acceptanceID := uuid.New()
	reviewID := uuid.New()
	s, rr, ar, runID := mergeSweepFixture(t, []*run.Stage{
		{ID: uuid.New(), Type: run.StageTypeImplement, State: run.StageStateSucceeded},
		{ID: acceptanceID, Type: run.StageTypeAcceptance, State: run.StageStateAwaitingHostDispatch},
		{ID: reviewID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval},
	})

	if err := s.ResolveReviewFromPollState(context.Background(), runID,
		false, "https://github.com/x/y/pull/42"); err != nil {
		t.Fatalf("ResolveReviewFromPollState: %v", err)
	}

	if got := stageStateByID(t, rr, runID, acceptanceID); got != run.StageStateAwaitingHostDispatch {
		t.Errorf("acceptance stage state = %q, want awaiting_host_dispatch (a close without merge sweeps nothing)", got)
	}
	if n := countCategory(ar.appended, CategoryStageSupersededByMerge); n != 0 {
		t.Errorf("stage_superseded_by_merge rows = %d, want 0", n)
	}
	if got := stageStateByID(t, rr, runID, reviewID); got != run.StageStateCancelled {
		t.Errorf("review stage state = %q, want cancelled", got)
	}
}

// TestResolveReviewStageOnMerge_NoReviewStage_SupersedesParkedAcceptance
// covers the SECOND merged path — the implement-only shape (routine_change
// and friends) that has no review stage at all. It can still hold an
// acceptance stage re-parked at awaiting_host_dispatch by a fix-up pass, and
// it strands for exactly the same reason, so the same sweep runs there with a
// nil skipStageID (this path owns no stage).
func TestResolveReviewStageOnMerge_NoReviewStage_SupersedesParkedAcceptance(t *testing.T) {
	acceptanceID := uuid.New()
	s, rr, ar, runID := mergeSweepFixture(t, []*run.Stage{
		{ID: uuid.New(), Type: run.StageTypeImplement, State: run.StageStateSucceeded},
		{ID: acceptanceID, Type: run.StageTypeAcceptance, State: run.StageStateAwaitingHostDispatch},
	})

	if err := s.ResolveReviewFromPollState(context.Background(), runID,
		true, "https://github.com/x/y/pull/42"); err != nil {
		t.Fatalf("ResolveReviewFromPollState: %v", err)
	}

	if got := stageStateByID(t, rr, runID, acceptanceID); got != run.StageStateSuperseded {
		t.Errorf("acceptance stage state = %q, want superseded", got)
	}
	if n := countCategory(ar.appended, CategoryStageSupersededByMerge); n != 1 {
		t.Errorf("stage_superseded_by_merge rows = %d, want exactly 1", n)
	}
	// The conditional Advance fired because the sweep moved something, so the
	// run completes on this same pass rather than waiting for an operator.
	if got := rr.runStates[runID]; got != run.StateSucceeded {
		t.Errorf("run state = %q, want succeeded", got)
	}
}

// TestResolveReviewStageOnMerge_NoReviewStage_NothingSwept_NoAdvance pins the
// CONDITIONAL on that path's Advance. The no-review-stage branch has never
// driven the orchestrator — an implement-only run completes when its last
// stage settles — so an unconditional Advance here would be a behavior change
// for every implement-only merge. Gating it on a non-empty sweep keeps the
// untouched shape byte-identical.
//
// Discriminating mutation: dropping the `if len(moved) > 0` guard so the
// Advance runs unconditionally — this run's stages are all terminal, so the
// orchestrator would complete it and rr.runStates would gain an entry,
// turning the assertion below RED.
func TestResolveReviewStageOnMerge_NoReviewStage_NothingSwept_NoAdvance(t *testing.T) {
	s, rr, ar, runID := mergeSweepFixture(t, []*run.Stage{
		{ID: uuid.New(), Type: run.StageTypeImplement, State: run.StageStateSucceeded},
	})

	if err := s.ResolveReviewFromPollState(context.Background(), runID,
		true, "https://github.com/x/y/pull/42"); err != nil {
		t.Fatalf("ResolveReviewFromPollState: %v", err)
	}

	if n := countCategory(ar.appended, CategoryStageSupersededByMerge); n != 0 {
		t.Errorf("stage_superseded_by_merge rows = %d, want 0 (nothing was supersedable)", n)
	}
	if _, advanced := rr.runStates[runID]; advanced {
		t.Errorf("orchestrator Advance ran on a merge that swept nothing (run state recorded as %q); the implement-only shape must stay byte-identical", rr.runStates[runID])
	}
	// The merge itself still resolved.
	if findCategory(ar.appended, "pr_merged") == nil {
		t.Error("pr_merged row missing on the no-review-stage merged path")
	}
}

// TestResolveReviewStageOnMerge_NoReviewStage_AlreadySuperseded_ReEvaluatesToCompletion
// pins the #3083 fix-up regression fix: the no-review-stage merged path must
// re-drive a run to completion on a merge REDELIVERY even when this pass sweeps
// nothing, provided the run already holds a superseded stage.
//
// The stranding scenario: an earlier invocation committed the
// awaiting_host_dispatch → superseded transition and then stopped BEFORE the
// Advance (a crash, or an Advance error). The stage is now terminal but the run
// is still `running`. A merge redelivery sees the stage already superseded
// (not a pair-table state), so the sweep moves NOTHING — and the prior
// `if len(moved) > 0` gate skipped the Advance, leaving the run stranded in
// `running` forever until an operator ran reconcile-merge. The fix gates the
// Advance on `moved > 0 OR the run has a superseded stage`, so the redelivery
// re-evaluates the run to completion automatically.
//
// COUNTERFACTUAL (approval condition — run by deletion). Reverting the gate in
// pullrequest_review_events.go to `if len(moved) > 0` makes the redelivery
// sweep nothing, skip the Advance, and leave rr.runStates without a succeeded
// entry for the run — turning the completion assertion RED. Observed:
//
//	run state = "running", want succeeded (already-superseded run must
//	re-evaluate to completion on a merge redelivery)
func TestResolveReviewStageOnMerge_NoReviewStage_AlreadySuperseded_ReEvaluatesToCompletion(t *testing.T) {
	acceptanceID := uuid.New()
	// The residue a partial prior invocation left: the acceptance stage is
	// ALREADY superseded (terminal), the run is still running, and no supersede
	// sweep runs this pass because superseded is not a pair-table state.
	s, rr, ar, runID := mergeSweepFixture(t, []*run.Stage{
		{ID: uuid.New(), Type: run.StageTypeImplement, State: run.StageStateSucceeded},
		{ID: acceptanceID, Type: run.StageTypeAcceptance, State: run.StageStateSuperseded},
	})

	if err := s.ResolveReviewFromPollState(context.Background(), runID,
		true, "https://github.com/x/y/pull/42"); err != nil {
		t.Fatalf("ResolveReviewFromPollState: %v", err)
	}

	// The sweep moved nothing (the stage was already superseded), so no NEW
	// supersede row is written on the redelivery.
	if n := countCategory(ar.appended, CategoryStageSupersededByMerge); n != 0 {
		t.Errorf("stage_superseded_by_merge rows = %d, want 0 (the stage was already superseded; the redelivery moves nothing)", n)
	}
	// THE POINT: the run is driven to completion anyway, because it holds a
	// superseded stage. Without the fix the Advance is skipped and the run
	// stays `running` with a merged PR — stranded.
	if got := rr.runStates[runID]; got != run.StateSucceeded {
		t.Errorf("run state = %q, want succeeded (an already-superseded run must re-evaluate to completion on a merge redelivery)", got)
	}
	// The stage stays superseded — the redelivery re-drives completion, it does
	// not re-transition the stage.
	if got := stageStateByID(t, rr, runID, acceptanceID); got != run.StageStateSuperseded {
		t.Errorf("acceptance stage state = %q, want superseded (unchanged by the redelivery)", got)
	}
}

// --- E64.42 / #3159: the closed-WITHOUT-merge arm publishes nothing ---------

// TestResolveReviewStageOnMerge_ClosedWithoutMerge_DoesNotRepublish pins the
// arm the #3159 republish is DELIBERATELY absent from.
//
// The republish exists because merging removes the run from the merge
// reconciler's heal sweep while leaving a required check stranded at
// in_progress on a head that is now on the base branch. A PR closed WITHOUT
// merging strands nothing: the change was not accepted, the head is not on the
// base branch, and no branch-protection gate is waiting on the context. So
// resolveReviewStageOnMerge's closed-without-merge arm (and the
// checkImplementReviewSettled early return, pinned separately) must publish
// NOTHING — a republish there would post a terminal conclusion onto an
// abandoned head for no gate.
//
// This is a per-branch control, not a happy-path variant: adding the call to
// the third arm leaves every other #3159 test green and only fails here.
func TestResolveReviewStageOnMerge_ClosedWithoutMerge_DoesNotRepublish(t *testing.T) {
	s, rr, _, gh, r, impl, _ := fixupRepublishFixture(t, true)
	ctx := context.Background()
	impl.State = run.StageStateSucceeded

	raw, err := json.Marshal(map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"html_url": fixupRepublishPRURL,
			"number":   1,
			"merged":   false,
			"head":     map[string]any{"sha": fixupHeadSHA},
			"base":     map[string]any{"sha": "base0"},
		},
		"sender": map[string]any{"login": "operator"},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	s.handlePullRequestClosed(ctx, raw)

	// The arm genuinely ran — the review stage was cancelled — so a zero-call
	// assertion below cannot pass vacuously.
	if got := stageStateOnOrchestratorRepo(t, rr, r.ID, findReviewStageID(t, rr, r.ID)); got != run.StageStateCancelled {
		t.Fatalf("review stage state = %q, want cancelled (the closed-without-merge arm did not run)", got)
	}
	if got := gh.calls(); len(got) != 0 {
		t.Fatalf("closed-without-merge published %d check runs, want 0; statuses=%v",
			len(got), publishStatuses(got))
	}
}

// findReviewStageID returns the run's review stage id from the fake repo.
func findReviewStageID(t *testing.T, rr *orchestratorRepo, runID uuid.UUID) uuid.UUID {
	t.Helper()
	sts, err := rr.ListStagesForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListStagesForRun: %v", err)
	}
	for _, st := range sts {
		if st.Type == run.StageTypeReview {
			return st.ID
		}
	}
	t.Fatalf("no review stage on run %s", runID)
	return uuid.Nil
}
