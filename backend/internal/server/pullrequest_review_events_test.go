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
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
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
}

type prEventsTransition struct {
	StageID uuid.UUID
	To      run.StageState
}

func (r *prEventsRunRepo) ListRuns(_ context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f.PullRequestURL != nil {
		r.listURLs = append(r.listURLs, *f.PullRequestURL)
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

// prEventsAuditRepo captures AppendChained calls so tests can assert
// on category + payload shape.
type prEventsAuditRepo struct {
	audit.Repository
	mu       sync.Mutex
	appended []audit.ChainAppendParams
	err      error
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
