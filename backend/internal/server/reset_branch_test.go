package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

const (
	resetAuthoredSHA = "1111111111111111111111111111111111111111" // last run-authored HEAD (ledger member)
	resetForeignSHA  = "ffffffffffffffffffffffffffffffffffffffff" // foreign commit on top (live tip)
	resetBranchName  = "fishhawk/run/abc"
	resetPRURL       = "https://github.com/x/y/pull/42"
)

// resetGitHub is reset_branch_test's self-contained GitHub stub. It serves
// GET /pulls/{n} (head/base refs + a per-call head SHA sequence), PATCH
// /git/refs (force-update capture), and GET /compare. Kept separate from
// lineage_test's lineageGitHub so this file owns its own reset fixtures.
//
//   - headRef is the PR head branch the force-update targets.
//   - headSHASeq, when non-empty, returns headSHASeq[min(i, len-1)] on the
//     i-th GET /pulls — drives the lease-change case (a racing push between
//     classification and the lease re-check).
//   - compareStatus != 200 exercises the CompareCommits error (fail-closed).
//   - forceCalled / forcedBranch / forcedSHA capture the PATCH ref update.
type resetGitHub struct {
	baseRef       string
	headRef       string
	headSHA       string
	headSHASeq    []string
	commitsByBase map[string][]string
	compareStatus int

	mu           sync.Mutex
	prCallCount  int
	forceCalled  bool
	forcedBranch string
	forcedSHA    string
}

func newResetGitHubClient(t *testing.T, stub *resetGitHub) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}",
		func(w http.ResponseWriter, _ *http.Request) {
			stub.mu.Lock()
			stub.prCallCount++
			head := stub.headSHA
			if len(stub.headSHASeq) > 0 {
				idx := stub.prCallCount - 1
				if idx >= len(stub.headSHASeq) {
					idx = len(stub.headSHASeq) - 1
				}
				head = stub.headSHASeq[idx]
			}
			ref := stub.headRef
			stub.mu.Unlock()
			if head == "" {
				head = "H"
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"node_id":"PR_x","state":"open","head":{"sha":%q,"ref":%q},"base":{"ref":%q}}`, head, ref, stub.baseRef)
		})
	mux.HandleFunc("PATCH /repos/{owner}/{repo}/git/refs/heads/{branch...}",
		func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			var body struct {
				SHA   string `json:"sha"`
				Force bool   `json:"force"`
			}
			_ = json.Unmarshal(raw, &body)
			stub.mu.Lock()
			stub.forceCalled = true
			stub.forcedBranch = r.PathValue("branch")
			stub.forcedSHA = body.SHA
			stub.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"ref":"refs/heads/%s","object":{"sha":%q}}`, r.PathValue("branch"), body.SHA)
		})
	mux.HandleFunc("GET /repos/{owner}/{repo}/compare/{basehead...}",
		func(w http.ResponseWriter, r *http.Request) {
			basehead := r.PathValue("basehead")
			base := basehead
			if i := strings.Index(basehead, "..."); i >= 0 {
				base = basehead[:i]
			}
			if stub.compareStatus != 0 && stub.compareStatus != http.StatusOK {
				w.WriteHeader(stub.compareStatus)
				return
			}
			stub.mu.Lock()
			commits := stub.commitsByBase[base]
			stub.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			var sb strings.Builder
			sb.WriteString(`{"commits":[`)
			for i, sha := range commits {
				if i > 0 {
					sb.WriteString(",")
				}
				fmt.Fprintf(&sb, `{"sha":%q}`, sha)
			}
			sb.WriteString(`]}`)
			_, _ = w.Write([]byte(sb.String()))
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
}

// postResetBranch posts a reset-branch request with the given identity-
// applying request mutator (e.g. withAuth) and decoded JSON body.
func postResetBranch(t *testing.T, s *Server, runID uuid.UUID, body resetBranchRequest,
	withID func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID.String()+"/reset-branch", bytes.NewReader(raw))
	req.SetPathValue("run_id", runID.String())
	w := httptest.NewRecorder()
	s.handleResetRunBranch(w, withID(req))
	return w
}

// seedResetRun wires a run + GitHub stub for the reset handler: a run with
// an open PR whose live tip is foreignTip, a base ref "main", and the given
// compare commit list. authoredSHA is seeded as a pull_request_opened
// ledger member so it is recognized as run-authored. Returns the server,
// stub, audit fake, and run repo.
func seedResetRun(t *testing.T, foreignTip string, commits []string,
	reviewState run.StageState, withReview bool) (*Server, *resetGitHub, *auditFake, *promptRunRepo, uuid.UUID) {
	t.Helper()
	runID := uuid.New()
	stub := &resetGitHub{
		baseRef:       "main",
		headRef:       resetBranchName,
		headSHA:       foreignTip,
		commitsByBase: map[string][]string{"main": commits},
	}
	gh := newResetGitHubClient(t, stub)
	prURL := resetPRURL
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning,
		InstallationID: instID(99), PullRequestURL: &prURL}
	implStage := &run.Stage{ID: uuid.New(), RunID: runID, Type: run.StageTypeImplement,
		State: run.StageStateSucceeded}
	s, _, au, rr := newLineageServer(t, gh, runRow, implStage)
	// Seed authoredSHA as a run-authored ledger member.
	au.seeded = append(au.seeded, &audit.Entry{
		RunID:    &runID,
		Category: "pull_request_opened",
		Payload:  json.RawMessage(`{"head_sha":"` + resetAuthoredSHA + `"}`),
	})
	stages := []*run.Stage{implStage}
	if withReview {
		review := &run.Stage{ID: uuid.New(), RunID: runID, Type: run.StageTypeReview,
			State: reviewState, Sequence: 2}
		rr.getStages[review.ID] = review
		stages = append(stages, review)
	}
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: stages}
	return s, stub, au, rr, runID
}

// branchResetAudit finds the branch_reset audit entry, if any.
func branchResetAudit(au *auditFake) *audit.ChainAppendParams {
	au.mu.Lock()
	defer au.mu.Unlock()
	for i := range au.appended {
		if au.appended[i].Category == CategoryBranchReset {
			return &au.appended[i]
		}
	}
	return nil
}

// TestResetRunBranch_HappyPath is the cross-boundary end-to-end: a foreign
// commit on top → the handler computes the last run-authored HEAD, the fake
// GitHub records a force-update of the branch ref to that SHA, a branch_reset
// audit row is written, the review stage is re-parked, and a follow-up
// ReverifyBranchLineage on the rewound tip returns clean.
func TestResetRunBranch_HappyPath(t *testing.T) {
	// commits oldest→newest: authored (member) then foreign (on top).
	s, stub, au, rr, runID := seedResetRun(t, resetForeignSHA,
		[]string{resetAuthoredSHA, resetForeignSHA}, run.StageStateAwaitingApproval, true)

	w := postResetBranch(t, s, runID, resetBranchRequest{Reason: "drop the on-top commit", Confirm: true}, withAuth)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	var resp resetBranchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ResetToSHA != resetAuthoredSHA {
		t.Errorf("reset_to_sha = %q, want %q", resp.ResetToSHA, resetAuthoredSHA)
	}
	if resp.DroppedOffendingSHA != resetForeignSHA {
		t.Errorf("dropped_offending_sha = %q, want %q", resp.DroppedOffendingSHA, resetForeignSHA)
	}
	if resp.PriorHeadSHA != resetForeignSHA {
		t.Errorf("prior_head_sha = %q, want %q", resp.PriorHeadSHA, resetForeignSHA)
	}

	// The fake GitHub recorded a force-update of the run branch to the
	// last run-authored HEAD.
	stub.mu.Lock()
	if !stub.forceCalled {
		t.Error("no force-update of the branch ref was recorded")
	}
	if stub.forcedBranch != resetBranchName {
		t.Errorf("forced branch = %q, want %q", stub.forcedBranch, resetBranchName)
	}
	if stub.forcedSHA != resetAuthoredSHA {
		t.Errorf("forced sha = %q, want %q", stub.forcedSHA, resetAuthoredSHA)
	}
	stub.mu.Unlock()

	// A branch_reset audit row was written (operator actor).
	a := branchResetAudit(au)
	if a == nil {
		t.Fatal("no branch_reset audit entry written")
	}
	if a.ActorKind == nil || *a.ActorKind != audit.ActorUser {
		t.Errorf("audit actor = %v, want user", a.ActorKind)
	}
	var payload struct {
		DroppedOffendingSHA string `json:"dropped_offending_sha"`
		ResetToSHA          string `json:"reset_to_sha"`
		RecoveryNote        string `json:"recovery_note"`
	}
	if err := json.Unmarshal(a.Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	if payload.DroppedOffendingSHA != resetForeignSHA || payload.ResetToSHA != resetAuthoredSHA {
		t.Errorf("audit payload = %+v, want dropped=%s reset_to=%s", payload, resetForeignSHA, resetAuthoredSHA)
	}
	if payload.RecoveryNote == "" {
		t.Error("audit payload missing recovery_note")
	}

	// The review stage was re-parked (awaiting_approval → pending →
	// awaiting_approval).
	if !transitionedTo(rr, run.StageStatePending) || !transitionedTo(rr, run.StageStateAwaitingApproval) {
		t.Errorf("review gate was not re-parked; transitions = %+v", rr.transitionStageCalls)
	}

	// Follow-up: ReverifyBranchLineage on the rewound tip is clean (the
	// foreign commit is gone; the tip is the run-authored member).
	stub.mu.Lock()
	stub.headSHA = resetAuthoredSHA
	stub.commitsByBase = map[string][]string{"main": {resetAuthoredSHA}}
	stub.mu.Unlock()
	if !s.ReverifyBranchLineage(context.Background(), runID, 42) {
		t.Error("ReverifyBranchLineage on the rewound tip should be clean")
	}
}

// TestResetRunBranch_AncestorRefusal: a foreign commit BELOW a ledger
// member (ancestor/interleaved shape) → 422 reset_out_of_scope, NO
// force-update, NO audit.
func TestResetRunBranch_AncestorRefusal(t *testing.T) {
	// commits oldest→newest: foreign (ancestor) then authored (member).
	s, stub, au, _, runID := seedResetRun(t, resetAuthoredSHA,
		[]string{resetForeignSHA, resetAuthoredSHA}, run.StageStateAwaitingApproval, true)

	w := postResetBranch(t, s, runID, resetBranchRequest{Confirm: true}, withAuth)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("reset_out_of_scope")) {
		t.Errorf("body missing reset_out_of_scope: %s", w.Body.String())
	}
	stub.mu.Lock()
	if stub.forceCalled {
		t.Error("force-update happened on an ancestor refusal")
	}
	stub.mu.Unlock()
	if a := branchResetAudit(au); a != nil {
		t.Error("branch_reset audit written on an ancestor refusal")
	}
}

// TestResetRunBranch_NotApplicable: a clean branch (tip == last authored,
// no foreign on top) → 422 reset_not_applicable, no force-update.
func TestResetRunBranch_NotApplicable(t *testing.T) {
	s, stub, au, _, runID := seedResetRun(t, resetAuthoredSHA,
		[]string{resetAuthoredSHA}, run.StageStateAwaitingApproval, true)

	w := postResetBranch(t, s, runID, resetBranchRequest{Confirm: true}, withAuth)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("reset_not_applicable")) {
		t.Errorf("body missing reset_not_applicable: %s", w.Body.String())
	}
	stub.mu.Lock()
	if stub.forceCalled {
		t.Error("force-update happened on a clean branch")
	}
	stub.mu.Unlock()
	if a := branchResetAudit(au); a != nil {
		t.Error("branch_reset audit written on a no-op")
	}
}

// TestResetRunBranch_NotDeterminable_CompareError: a CompareCommits error
// → 422 reset_not_determinable, no force-update (fail-CLOSED for the
// destructive action — opposite of detection's fail-open).
func TestResetRunBranch_NotDeterminable_CompareError(t *testing.T) {
	s, stub, au, _, runID := seedResetRun(t, resetForeignSHA,
		[]string{resetAuthoredSHA, resetForeignSHA}, run.StageStateAwaitingApproval, true)
	stub.compareStatus = http.StatusInternalServerError

	w := postResetBranch(t, s, runID, resetBranchRequest{Confirm: true}, withAuth)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("reset_not_determinable")) {
		t.Errorf("body missing reset_not_determinable: %s", w.Body.String())
	}
	stub.mu.Lock()
	if stub.forceCalled {
		t.Error("force-update happened despite an uncertain classification (fail-closed violated)")
	}
	stub.mu.Unlock()
	if a := branchResetAudit(au); a != nil {
		t.Error("branch_reset audit written on a not-determinable refusal")
	}
}

// TestResetRunBranch_LeaseChange: the live head changes between
// classification and the lease re-check (concurrent push) → 422
// reset_not_determinable, no force-update. headSHASeq returns the foreign
// tip for the first two GET /pulls calls (handler + classify) and a
// different SHA on the lease re-check.
func TestResetRunBranch_LeaseChange(t *testing.T) {
	const racingSHA = "2222222222222222222222222222222222222222"
	s, stub, au, _, runID := seedResetRun(t, resetForeignSHA,
		[]string{resetAuthoredSHA, resetForeignSHA}, run.StageStateAwaitingApproval, true)
	stub.mu.Lock()
	// Calls: 1 = handler head read, 2 = classify base-ref read, 3 = lease
	// re-check (now sees a racing push).
	stub.headSHASeq = []string{resetForeignSHA, resetForeignSHA, racingSHA}
	stub.mu.Unlock()

	w := postResetBranch(t, s, runID, resetBranchRequest{Confirm: true}, withAuth)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("reset_not_determinable")) {
		t.Errorf("body missing reset_not_determinable: %s", w.Body.String())
	}
	stub.mu.Lock()
	if stub.forceCalled {
		t.Error("force-update happened despite a lease change")
	}
	stub.mu.Unlock()
	if a := branchResetAudit(au); a != nil {
		t.Error("branch_reset audit written on a lease-change refusal")
	}
}

// TestResetRunBranch_MissingConfirm: confirm omitted → 400
// confirmation_required, no force-update.
func TestResetRunBranch_MissingConfirm(t *testing.T) {
	s, stub, _, _, runID := seedResetRun(t, resetForeignSHA,
		[]string{resetAuthoredSHA, resetForeignSHA}, run.StageStateAwaitingApproval, true)

	w := postResetBranch(t, s, runID, resetBranchRequest{Confirm: false}, withAuth)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("confirmation_required")) {
		t.Errorf("body missing confirmation_required: %s", w.Body.String())
	}
	stub.mu.Lock()
	if stub.forceCalled {
		t.Error("force-update happened without confirmation")
	}
	stub.mu.Unlock()
}

// TestResetRunBranch_MissingScope: a token without write:runs → 403.
func TestResetRunBranch_MissingScope(t *testing.T) {
	s, _, _, _, runID := seedResetRun(t, resetForeignSHA,
		[]string{resetAuthoredSHA, resetForeignSHA}, run.StageStateAwaitingApproval, true)

	withScopeless := func(req *http.Request) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
			Subject: "github:ops", TokenID: "tok-x", Scopes: []string{"read:runs"},
		}))
	}
	w := postResetBranch(t, s, runID, resetBranchRequest{Confirm: true}, withScopeless)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("insufficient_scope")) {
		t.Errorf("body missing insufficient_scope: %s", w.Body.String())
	}
}

// TestResetRunBranch_CrossRunToken: a run-bound mcp token may reset only
// its own run's branch → 403 cross_run_reset.
func TestResetRunBranch_CrossRunToken(t *testing.T) {
	s, _, _, _, runID := seedResetRun(t, resetForeignSHA,
		[]string{resetAuthoredSHA, resetForeignSHA}, run.StageStateAwaitingApproval, true)

	otherRunID := uuid.New()
	withForeignToken := func(req *http.Request) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
			Subject: "mcp:run:" + otherRunID.String(),
			TokenID: "tok-agent",
			Scopes:  []string{"mcp:read", "write:runs"},
		}))
	}
	w := postResetBranch(t, s, runID, resetBranchRequest{Confirm: true}, withForeignToken)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("cross_run_reset")) {
		t.Errorf("body missing cross_run_reset: %s", w.Body.String())
	}
}

// TestResetRunBranch_NoReviewStage: the commit-yourself shape (no separate
// review stage) → the force-update + audit still happen; the re-park is a
// tolerated no-op.
func TestResetRunBranch_NoReviewStage(t *testing.T) {
	s, stub, au, rr, runID := seedResetRun(t, resetForeignSHA,
		[]string{resetAuthoredSHA, resetForeignSHA}, run.StageStateAwaitingApproval, false)

	w := postResetBranch(t, s, runID, resetBranchRequest{Confirm: true}, withAuth)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	stub.mu.Lock()
	if !stub.forceCalled || stub.forcedSHA != resetAuthoredSHA {
		t.Errorf("force-update did not happen on the no-review shape (forced=%v sha=%q)", stub.forceCalled, stub.forcedSHA)
	}
	stub.mu.Unlock()
	if a := branchResetAudit(au); a == nil {
		t.Error("branch_reset audit not written on the no-review shape")
	}
	// Re-park is a tolerated no-op: no review stage was transitioned.
	if transitionedTo(rr, run.StageStatePending) {
		t.Error("a stage was re-parked despite there being no review stage")
	}
	var resp resetBranchResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ReparkedReviewStageID != "" {
		t.Errorf("reparked_review_stage_id = %q, want empty on the no-review shape", resp.ReparkedReviewStageID)
	}
}
