package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
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
func (r *prEventsRunRepo) ListStagesForRun(_ context.Context, id uuid.UUID) ([]*run.Stage, error) {
	return r.stages[id], r.stagesErr
}
func (r *prEventsRunRepo) TransitionStage(_ context.Context, id uuid.UUID, to run.StageState, _ *run.StageCompletion) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.transErr != nil {
		return nil, r.transErr
	}
	r.transitions = append(r.transitions, prEventsTransition{StageID: id, To: to})
	return &run.Stage{ID: id, State: to}, nil
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

func TestPullRequestClosed_NotMerged_NoTransitionNoAudit(t *testing.T) {
	// Closed-without-merging: leave the run alone. The labeler /
	// operator decides whether to manually intervene.
	runID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: runID, PullRequestURL: &prURL}},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {{ID: uuid.New(), RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval}},
		},
	}
	ar := &prEventsAuditRepo{}
	s := prEventsTestServer(t, rr, ar)

	payload, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{
			"html_url": prURL,
			"merged":   false,
			"head":     map[string]any{"sha": "headsha"},
		},
		"sender": map[string]any{"login": "alice"},
	})
	s.handlePullRequestClosed(context.Background(), payload)

	if len(rr.transitions) != 0 {
		t.Errorf("transitions = %d, want 0 (no auto-action on non-merge close)", len(rr.transitions))
	}
	if len(ar.appended) != 0 {
		t.Errorf("audit rows = %d, want 0 (no auto-action on non-merge close)", len(ar.appended))
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
