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
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// --- E64.23 / #3125: fishhawk_rebase_run_branch ---

const (
	rebasePriorHeadSHA = "aaaa111111111111111111111111111111111111" // run branch tip before the advance
	rebaseNewHeadSHA   = "bbbb222222222222222222222222222222222222" // the merge commit / post-merge head
	rebaseBaseAdvance  = "cccc333333333333333333333333333333333333" // a commit the base advanced by
	rebaseBranchName   = "fishhawk/run/rebase-abc"
	rebaseBaseRef      = "main"
	rebasePRURL        = "https://github.com/x/y/pull/77"
)

// rebaseGitHub is rebase_branch_test's self-contained GitHub stub. It serves
// GET /pulls/{n} (head/base refs + a per-call head SHA sequence), GET
// /compare/{basehead...} (the BEHIND-PROBE) and POST /merges (the base
// advance), capturing the merge request body so the DIRECTION of the merge is
// assertable — an inversion would merge the run branch into the base branch,
// the worst failure available to this verb.
//
//   - headSHASeq, when non-empty, returns headSHASeq[min(i, len-1)] on the
//     i-th GET /pulls. Call 1 = the handler head read, call 2 = the lease
//     re-check, call 3 = the post-merge authoritative re-read. This drives
//     both the lease-change case and the post-merge head resolution.
//   - prStatusSeq mirrors it for status codes, so the post-merge re-read can
//     be failed independently of the earlier reads (condition 5).
//   - behindCommits is what the behind-probe compare returns: EMPTY means the
//     run branch already contains the base.
//   - mergeStatus / mergeBody drive the merges endpoint (409 = conflict,
//     500 = merge_failed, 201 with or without a `sha`).
type rebaseGitHub struct {
	baseRef       string
	headRef       string
	headSHA       string
	headSHASeq    []string
	prStatusSeq   []int
	behindCommits []string
	compareStatus int
	mergeStatus   int
	mergeBody     string

	mu          sync.Mutex
	prCallCount int
	mergeCalls  []rebaseMergeCall
}

// rebaseMergeCall is one captured POST /merges request body.
type rebaseMergeCall struct {
	Base          string `json:"base"`
	Head          string `json:"head"`
	CommitMessage string `json:"commit_message"`
}

// merges returns a snapshot of every captured POST /merges body, in order.
func (g *rebaseGitHub) merges() []rebaseMergeCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]rebaseMergeCall(nil), g.mergeCalls...)
}

func newRebaseGitHubClient(t *testing.T, stub *rebaseGitHub) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}",
		func(w http.ResponseWriter, _ *http.Request) {
			stub.mu.Lock()
			stub.prCallCount++
			idx := stub.prCallCount - 1
			head := stub.headSHA
			if len(stub.headSHASeq) > 0 {
				i := idx
				if i >= len(stub.headSHASeq) {
					i = len(stub.headSHASeq) - 1
				}
				head = stub.headSHASeq[i]
			}
			status := http.StatusOK
			if len(stub.prStatusSeq) > 0 {
				i := idx
				if i >= len(stub.prStatusSeq) {
					i = len(stub.prStatusSeq) - 1
				}
				status = stub.prStatusSeq[i]
			}
			ref, base := stub.headRef, stub.baseRef
			stub.mu.Unlock()
			if status != http.StatusOK {
				w.WriteHeader(status)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"node_id":"PR_x","state":"open","head":{"sha":%q,"ref":%q},"base":{"ref":%q}}`,
				head, ref, base)
		})
	mux.HandleFunc("GET /repos/{owner}/{repo}/compare/{basehead...}",
		func(w http.ResponseWriter, _ *http.Request) {
			stub.mu.Lock()
			status := stub.compareStatus
			commits := append([]string(nil), stub.behindCommits...)
			stub.mu.Unlock()
			if status != 0 && status != http.StatusOK {
				w.WriteHeader(status)
				return
			}
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
	mux.HandleFunc("POST /repos/{owner}/{repo}/merges",
		func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			var call rebaseMergeCall
			_ = json.Unmarshal(raw, &call)
			stub.mu.Lock()
			stub.mergeCalls = append(stub.mergeCalls, call)
			status, body := stub.mergeStatus, stub.mergeBody
			stub.mu.Unlock()
			if status == 0 {
				status = http.StatusCreated
			}
			if body == "" {
				body = `{"sha":"` + rebaseNewHeadSHA + `"}`
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
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

// withRebaseOperator injects an operator fhk_ token identity carrying
// write:stages — the only credential the rebase verb accepts. The shared
// withAuth helper is a scope-less cookie session, which rebase rejects
// (write:stages is enforced UNCONDITIONALLY with no cookie-session bypass).
func withRebaseOperator(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
		Subject: "github:ops", TokenID: "tok-op", Scopes: []string{"write:stages"},
	}))
}

// postRebaseBranch posts a rebase-branch request with the given identity
// mutator and JSON body.
func postRebaseBranch(t *testing.T, s *Server, runID uuid.UUID, body rebaseBranchRequest,
	withID func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	return postRebaseBranchRaw(t, s, runID, runID.String(), raw, withID)
}

// postRebaseBranchRaw posts an arbitrary (possibly malformed) body under an
// arbitrary run_id path value, so the decode-error and malformed-uuid paths
// are reachable.
func postRebaseBranchRaw(t *testing.T, s *Server, runID uuid.UUID, pathRunID string, raw []byte,
	withID func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+pathRunID+"/rebase-branch", bytes.NewReader(raw))
	req.SetPathValue("run_id", pathRunID)
	w := httptest.NewRecorder()
	s.handleRebaseRunBranch(w, withID(req))
	return w
}

// rebaseSeed is everything a rebase test asserts against.
type rebaseSeed struct {
	s       *Server
	stub    *rebaseGitHub
	au      *auditFake
	rr      *promptRunRepo
	creator *vouchCheckCreator
	runID   uuid.UUID
	review  *run.Stage
	// bearer is a REAL operator API token carrying write:stages, so the
	// full-span test can drive the shipped route through s.Handler() rather
	// than injecting an identity into the context by hand.
	bearer string
}

// rebaseOpts tweaks the seeded world.
type rebaseOpts struct {
	noReviewStage  bool
	noInstallation bool
	repo           string
	noPRURL        bool
	noPublisher    bool
}

// seedRebaseRun wires a run + GitHub stub + a REAL auditcheckpublisher over a
// fake CheckRunCreator, so one test observes the HTTP response, the forge
// calls, the appended audit entries and the published Check Run together.
func seedRebaseRun(t *testing.T, stub *rebaseGitHub, opt rebaseOpts) *rebaseSeed {
	t.Helper()
	gh := newRebaseGitHubClient(t, stub)
	runID := uuid.New()
	repoName := "x/y"
	if opt.repo != "" {
		repoName = opt.repo
	}
	prURL := rebasePRURL
	runRow := &run.Run{ID: runID, Repo: repoName, State: run.StateRunning,
		InstallationID: instID(99), PullRequestURL: &prURL}
	if opt.noInstallation {
		runRow.InstallationID = nil
	}
	if opt.noPRURL {
		runRow.PullRequestURL = nil
	}
	implStage := &run.Stage{ID: uuid.New(), RunID: runID, Type: run.StageTypeImplement,
		State: run.StageStateSucceeded}
	// Built directly rather than via newLineageServer so the APITokenRepo is
	// present in the Config New() consumes — the auth middleware is wired at
	// construction, so a repo assigned afterwards never authenticates and the
	// full-span test would only ever see a 401.
	tokenRepo, bearer := mcpTestTokens(t, "write:stages")
	au := newAuditFake()
	rr := newPromptRunRepo()
	rr.getStages[implStage.ID] = implStage
	rr.getRuns[runID] = runRow
	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  newSigningFake(),
		ArtifactRepo: newFakeArtifactRepo(),
		AuditRepo:    au,
		RunRepo:      rr,
		GitHub:       gh,
		APITokenRepo: tokenRepo,
	})

	stages := []*run.Stage{implStage}
	var review *run.Stage
	if !opt.noReviewStage {
		review = &run.Stage{ID: uuid.New(), RunID: runID, Type: run.StageTypeReview,
			State: run.StageStateAwaitingApproval, Sequence: 2}
		rr.getStages[review.ID] = review
		stages = append(stages, review)
	}
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: stages}

	var creator *vouchCheckCreator
	if !opt.noPublisher {
		creator = &vouchCheckCreator{}
		pub := auditcheckpublisher.New(auditcheckpublisher.Deps{
			GitHub:      creator,
			Runs:        rr,
			Artifacts:   s.cfg.ArtifactRepo,
			Audit:       au,
			ExternalURL: "https://app.fishhawk.example.com",
		})
		if pub == nil {
			t.Fatal("publisher nil")
		}
		s.auditCheckPublisher = pub
	}
	return &rebaseSeed{s: s, stub: stub, au: au, rr: rr, creator: creator, runID: runID, review: review, bearer: bearer}
}

// cleanRebaseStub is the standard BEHIND stub: the run branch is one commit
// behind the base, the merges endpoint succeeds, and the post-merge PR read
// reports the new head.
func cleanRebaseStub() *rebaseGitHub {
	return &rebaseGitHub{
		baseRef:       rebaseBaseRef,
		headRef:       rebaseBranchName,
		headSHA:       rebasePriorHeadSHA,
		headSHASeq:    []string{rebasePriorHeadSHA, rebasePriorHeadSHA, rebaseNewHeadSHA},
		behindCommits: []string{rebaseBaseAdvance},
	}
}

// upToDateRebaseStub is the standard ALREADY-CONTAINS-BASE stub: the
// behind-probe returns zero commits.
func upToDateRebaseStub() *rebaseGitHub {
	return &rebaseGitHub{
		baseRef:       rebaseBaseRef,
		headRef:       rebaseBranchName,
		headSHA:       rebaseNewHeadSHA,
		behindCommits: nil,
	}
}

// auditEntries returns every appended entry of the given category.
func auditEntries(au *auditFake, category string) []audit.ChainAppendParams {
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []audit.ChainAppendParams
	for i := range au.appended {
		if au.appended[i].Category == category {
			out = append(out, au.appended[i])
		}
	}
	return out
}

// branchRebasedAudit finds the branch_rebased audit entry, if any.
func branchRebasedAudit(au *auditFake) *audit.ChainAppendParams {
	if e := auditEntries(au, CategoryBranchRebased); len(e) > 0 {
		return &e[0]
	}
	return nil
}

// checkPublications returns a snapshot of every recorded Check Run creation.
func checkPublications(c *vouchCheckCreator) []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.calls))
	for _, p := range c.calls {
		out = append(out, p.HeadSHA)
	}
	return out
}

// assertNoWriteBeforeMerge is REFUSAL RULE (a), stated explicitly and
// separately from rule (b) below: EVERY refusal classified BEFORE a merge is
// attempted — the whole auth ladder, confirm, the determinability ladder, the
// behind-probe error and the lease re-check — must record ZERO merge POSTs,
// NO branch_rebased audit entry and ZERO check publications.
func assertNoWriteBeforeMerge(t *testing.T, sd *rebaseSeed) {
	t.Helper()
	if m := sd.stub.merges(); len(m) != 0 {
		t.Errorf("merge POSTs = %d, want 0 on a refusal classified before any merge: %+v", len(m), m)
	}
	if a := branchRebasedAudit(sd.au); a != nil {
		t.Error("branch_rebased audit entry written on a refusal classified before any merge")
	}
	if p := checkPublications(sd.creator); len(p) != 0 {
		t.Errorf("check publications = %v, want none on a refusal classified before any merge", p)
	}
}

// assertMergeAttemptedNothingWritten is REFUSAL RULE (b), the sibling of rule
// (a) and deliberately NOT the same assertion: rebase_conflict and
// rebase_merge_failed are DISCOVERED by the merges endpoint itself, so
// exactly ONE merge POST must have occurred. What must be absent is
// everything AFTER it: no branch_rebased entry, no check publication, no
// re-park.
func assertMergeAttemptedNothingWritten(t *testing.T, sd *rebaseSeed) {
	t.Helper()
	if m := sd.stub.merges(); len(m) != 1 {
		t.Errorf("merge POSTs = %d, want exactly 1 (the refusal is discovered BY the merges endpoint)", len(m))
	}
	if a := branchRebasedAudit(sd.au); a != nil {
		t.Error("branch_rebased audit entry written after a failed merge")
	}
	if p := checkPublications(sd.creator); len(p) != 0 {
		t.Errorf("check publications = %v, want none after a failed merge", p)
	}
	if transitionedTo(sd.rr, run.StageStatePending) {
		t.Error("the review gate was re-parked after a failed merge")
	}
}

// TestRebaseRunBranch_MergesBaseIntoRunBranch is the DIRECTION assertion. It
// reads the CAPTURED POST /merges body and requires base == the RUN BRANCH and
// head == the BASE REF. GitHub's merges API treats `base` as the branch that
// RECEIVES the merge, so an inversion here would merge the run branch into
// main — the worst failure available to this verb, and one no compiler and no
// status code would catch.
func TestRebaseRunBranch_MergesBaseIntoRunBranch(t *testing.T) {
	sd := seedRebaseRun(t, cleanRebaseStub(), rebaseOpts{})

	w := postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Reason: "base advanced", Confirm: true}, withRebaseOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	merges := sd.stub.merges()
	if len(merges) != 1 {
		t.Fatalf("merge POSTs = %d, want 1", len(merges))
	}
	if merges[0].Base != rebaseBranchName {
		t.Errorf("merge base = %q, want the RUN BRANCH %q (an inversion would merge the run branch into the base branch)",
			merges[0].Base, rebaseBranchName)
	}
	if merges[0].Head != rebaseBaseRef {
		t.Errorf("merge head = %q, want the BASE REF %q", merges[0].Head, rebaseBaseRef)
	}
	if !strings.Contains(merges[0].CommitMessage, rebaseBranchName) ||
		!strings.Contains(merges[0].CommitMessage, rebaseBaseRef) ||
		!strings.Contains(merges[0].CommitMessage, "fishhawk_rebase_run_branch") {
		t.Errorf("commit message must name the run, the base ref and the verb: %q", merges[0].CommitMessage)
	}

	var resp rebaseBranchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.PriorHeadSHA != rebasePriorHeadSHA || resp.NewHeadSHA != rebaseNewHeadSHA {
		t.Errorf("prior/new head = %q/%q, want %q/%q",
			resp.PriorHeadSHA, resp.NewHeadSHA, rebasePriorHeadSHA, rebaseNewHeadSHA)
	}
	if resp.AlreadyUpToDate {
		t.Error("already_up_to_date = true on a real advance")
	}
	if resp.BaseRef != rebaseBaseRef || resp.Branch != rebaseBranchName {
		t.Errorf("base_ref/branch = %q/%q", resp.BaseRef, resp.Branch)
	}
	// SHIPPED-MESSAGE PIN: the mechanism note states the merge-commit
	// mechanism, so the verb's name cannot mislead a reader of the response.
	if !strings.Contains(resp.MechanismNote, "merge commit") {
		t.Errorf("mechanism_note must state the merge-commit mechanism: %q", resp.MechanismNote)
	}
	if resp.ReparkedReviewStageID != sd.review.ID.String() {
		t.Errorf("reparked_review_stage_id = %q, want %q", resp.ReparkedReviewStageID, sd.review.ID)
	}
}

// TestRebaseRunBranch_AlreadyContainsBase_SkipsMergeEntirely is the
// BLOCKER-1 sibling: a zero-commit behind-probe short-circuits with NO POST
// /merges recorded at all, so "already contains the base" and "the merge
// returned no sha" are distinguished by the PROBE, never by MergeBranch's
// ambiguous ("", nil) return. It is STILL a 200 that re-parks and republishes.
func TestRebaseRunBranch_AlreadyContainsBase_SkipsMergeEntirely(t *testing.T) {
	sd := seedRebaseRun(t, upToDateRebaseStub(), rebaseOpts{})

	w := postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Reason: "retry", Confirm: true}, withRebaseOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if m := sd.stub.merges(); len(m) != 0 {
		t.Fatalf("merge POSTs = %d, want 0 — the behind-probe must short-circuit the merge entirely", len(m))
	}
	var resp rebaseBranchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.AlreadyUpToDate {
		t.Error("already_up_to_date = false on a zero-commit behind-probe")
	}
	if resp.MergeCommitSHA != "" {
		t.Errorf("merge_commit_sha = %q, want empty (no merge was attempted)", resp.MergeCommitSHA)
	}
	if resp.NewHeadSHA != rebaseNewHeadSHA {
		t.Errorf("new_head_sha = %q, want the current head %q", resp.NewHeadSHA, rebaseNewHeadSHA)
	}
	// The shared tail still ran: re-parked, audited AND republished.
	if !transitionedTo(sd.rr, run.StageStatePending) || !transitionedTo(sd.rr, run.StageStateAwaitingApproval) {
		t.Error("the already-contains-base arm must still re-park the review gate")
	}
	if branchRebasedAudit(sd.au) == nil {
		t.Error("the already-contains-base arm must still write a branch_rebased entry")
	}
	if got := checkPublications(sd.creator); len(got) != 1 || got[0] != rebaseNewHeadSHA {
		t.Errorf("check publications = %v, want exactly one at %q", got, rebaseNewHeadSHA)
	}
}

// TestRebaseRunBranch_UndecodableMergeSHAStillPublishesAtLiveHead is the
// BLOCKER-1 proof: the merges endpoint returns a 201 carrying NO `sha` (the
// deliberately benign ("", nil) shape pinned by githubclient's
// TestMergeBranch_MergedMissingSHAIsBenign), while the post-merge PR read
// reports the new head. The handler must publish at the LIVE head regardless,
// proving it never reads ("", nil) as a signal.
func TestRebaseRunBranch_UndecodableMergeSHAStillPublishesAtLiveHead(t *testing.T) {
	stub := cleanRebaseStub()
	stub.mergeBody = `{"no_sha_here":true}`
	sd := seedRebaseRun(t, stub, rebaseOpts{})

	w := postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Confirm: true}, withRebaseOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp rebaseBranchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.MergeCommitSHA != "" {
		t.Errorf("merge_commit_sha = %q, want empty on an undecodable 201", resp.MergeCommitSHA)
	}
	if resp.NewHeadSHA != rebaseNewHeadSHA {
		t.Errorf("new_head_sha = %q, want the LIVE head %q (the authoritative re-read, not the merge return)",
			resp.NewHeadSHA, rebaseNewHeadSHA)
	}
	if resp.AlreadyUpToDate {
		t.Error("already_up_to_date = true — an undecodable merge sha must NOT be read as already-up-to-date")
	}
	if got := checkPublications(sd.creator); len(got) != 1 || got[0] != rebaseNewHeadSHA {
		t.Errorf("check publications = %v, want exactly one at the live head %q", got, rebaseNewHeadSHA)
	}
}

// TestRebaseRunBranch_PublishFailsThenReinvokeRepublishesAtHead is the
// BLOCKER-2 control: the idempotent retry the republish warning advertises
// must be REAL. Invocation 1 merges successfully with the CheckRunCreator
// erroring (200, republished:false, warning naming re-invocation). The stub is
// then moved to the post-merge world (the branch contains the base, the head
// is the merge commit) and the creator recovers. Invocation 2 must issue NO
// second merge, report already_up_to_date, and PUBLISH the check at the
// post-merge head.
//
// COUNTERFACTUAL c1 is anchored here: deleting the already-contains-base arm's
// re-park + republish tail turns this RED on zero check publications after
// invocation 2 — a state read-back, not an error-identity assertion.
func TestRebaseRunBranch_PublishFailsThenReinvokeRepublishesAtHead(t *testing.T) {
	stub := cleanRebaseStub()
	sd := seedRebaseRun(t, stub, rebaseOpts{})
	sd.creator.mu.Lock()
	sd.creator.err = errBoom
	sd.creator.mu.Unlock()

	w1 := postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Reason: "advance", Confirm: true}, withRebaseOperator)
	if w1.Code != http.StatusOK {
		t.Fatalf("invocation 1 status = %d, want 200:\n%s", w1.Code, w1.Body.String())
	}
	var r1 rebaseBranchResponse
	if err := json.Unmarshal(w1.Body.Bytes(), &r1); err != nil {
		t.Fatalf("unmarshal 1: %v", err)
	}
	if r1.AuditCheckRepublished {
		t.Fatal("invocation 1 audit_check_republished = true, want false (the publish failed)")
	}
	// SHIPPED-MESSAGE PIN: the warning must name the re-invocation retry.
	if !strings.Contains(r1.AuditCheckRepublishWarning, "fishhawk_rebase_run_branch") {
		t.Errorf("republish warning must name the re-invoke retry: %q", r1.AuditCheckRepublishWarning)
	}

	// Move the world to the post-merge state: the branch now CONTAINS the
	// base and its head is the merge commit. Recover the publisher.
	stub.mu.Lock()
	stub.behindCommits = nil
	stub.headSHA = rebaseNewHeadSHA
	stub.headSHASeq = nil
	stub.mu.Unlock()
	sd.creator.mu.Lock()
	sd.creator.err = nil
	sd.creator.mu.Unlock()
	mergesAfterFirst := len(stub.merges())

	w2 := postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Reason: "retry the dropped check re-post", Confirm: true}, withRebaseOperator)
	if w2.Code != http.StatusOK {
		t.Fatalf("invocation 2 status = %d, want 200:\n%s", w2.Code, w2.Body.String())
	}
	var r2 rebaseBranchResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &r2); err != nil {
		t.Fatalf("unmarshal 2: %v", err)
	}
	if len(stub.merges()) != mergesAfterFirst {
		t.Errorf("invocation 2 issued a second merge POST (%d then %d); the behind-probe must short-circuit it",
			mergesAfterFirst, len(stub.merges()))
	}
	if !r2.AlreadyUpToDate {
		t.Error("invocation 2 already_up_to_date = false, want true")
	}
	if !r2.AuditCheckRepublished {
		t.Errorf("invocation 2 audit_check_republished = false — the advertised retry is not real:\n%s", w2.Body.String())
	}
	if r2.AuditCheckRepublishWarning != "" {
		t.Errorf("invocation 2 warning non-empty on success: %q", r2.AuditCheckRepublishWarning)
	}
	// STATE READ-BACK (the counterfactual vehicle): a Check Run WAS published
	// at the post-merge head on the retry.
	pubs := checkPublications(sd.creator)
	if len(pubs) == 0 || pubs[len(pubs)-1] != rebaseNewHeadSHA {
		t.Fatalf("check publications = %v, want the last one at the post-merge head %q", pubs, rebaseNewHeadSHA)
	}
}

// TestRebaseRunBranch_PostMergeHeadReadFails_NoPublication is BINDING
// CONDITION 5: when the merge SUCCEEDS but the post-merge head re-read FAILS,
// the handler must NOT fall back to publishing at "no override" — that
// resolves to the PRE-merge audit-recorded head, which is exactly the
// staleness this verb exists to remove, and would make the verb cause the bug
// it fixes. It must skip publication entirely, return 200, and name the
// re-invocation retry in the warning.
func TestRebaseRunBranch_PostMergeHeadReadFails_NoPublication(t *testing.T) {
	stub := cleanRebaseStub()
	// PR reads: 1 = handler head, 2 = lease re-check, 3+ = the post-merge
	// authoritative re-read, which now fails.
	stub.prStatusSeq = []int{http.StatusOK, http.StatusOK, http.StatusInternalServerError}
	sd := seedRebaseRun(t, stub, rebaseOpts{})
	// DISCRIMINATION, seeded BY CONSTRUCTION: the run carries a PRE-merge
	// head-report entry, so a fallback to "no override" would resolve to
	// rebasePriorHeadSHA and PUBLISH there. Without this seed the publisher's
	// head resolution finds nothing and publishes nothing anyway, and the
	// counterfactual comes back GREEN with the forbidden fallback restored —
	// i.e. the test would not be pinning the control at all.
	seedRunHeadEntry(sd.au, sd.runID, "pull_request_opened", rebasePriorHeadSHA, 1)

	w := postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Confirm: true}, withRebaseOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the merge already happened; a refusal would misreport a completed write):\n%s",
			w.Code, w.Body.String())
	}
	if m := sd.stub.merges(); len(m) != 1 {
		t.Fatalf("merge POSTs = %d, want 1", len(m))
	}
	var resp rebaseBranchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.NewHeadSHA != "" {
		t.Errorf("new_head_sha = %q, want empty (the head is unknown)", resp.NewHeadSHA)
	}
	if resp.AuditCheckRepublished {
		t.Error("audit_check_republished = true with an unknown head")
	}
	if !strings.Contains(resp.AuditCheckRepublishWarning, "fishhawk_rebase_run_branch") {
		t.Errorf("warning must name re-invocation: %q", resp.AuditCheckRepublishWarning)
	}
	// THE CONTROL, read back as committed state: NO publication happened at
	// all. A fallback to "no override" would have published at the PRE-merge
	// head, which is the staleness this verb removes.
	if pubs := checkPublications(sd.creator); len(pubs) != 0 {
		t.Errorf("check publications = %v, want NONE — publishing at an unknown head resolves to the stale PRE-merge head %q, which is exactly the staleness this verb exists to remove",
			pubs, rebasePriorHeadSHA)
	}
}

// --- REFUSAL RULE (a): classified BEFORE any merge ---

// TestRebaseRunBranch_RefusalsBeforeAnyMerge covers every refusal the handler
// classifies BEFORE attempting a merge. Each asserts its own error code AND
// rule (a): zero merge POSTs, no branch_rebased entry, zero check
// publications.
func TestRebaseRunBranch_RefusalsBeforeAnyMerge(t *testing.T) {
	otherRunID := uuid.New()

	cases := []struct {
		name     string
		stub     func() *rebaseGitHub
		opts     rebaseOpts
		withID   func(*http.Request) *http.Request
		body     *rebaseBranchRequest
		raw      []byte
		pathID   string
		nilForge bool
		want     int
		code     string
	}{
		{
			name: "anonymous",
			withID: func(req *http.Request) *http.Request {
				return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{}))
			},
			want: http.StatusUnauthorized, code: "authentication_required",
		},
		{
			// COUNTERFACTUAL c2 vehicle. Seeded BY CONSTRUCTION with a
			// DISTINCT unrelated run-bound subject, so deleting the guard
			// admits the request and the merge POST + branch_rebased entry
			// appear — the state read-back, not the status code.
			name: "run-bound agent token rejected outright",
			withID: func(req *http.Request) *http.Request {
				return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
					Subject: "mcp:run:" + otherRunID.String(),
					TokenID: "tok-agent",
					Scopes:  []string{"mcp:read", "write:stages"},
				}))
			},
			want: http.StatusForbidden, code: "run_token_forbidden",
		},
		{
			name: "token without write:stages",
			withID: func(req *http.Request) *http.Request {
				return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
					Subject: "github:ops", TokenID: "tok-x", Scopes: []string{"read:runs"},
				}))
			},
			want: http.StatusForbidden, code: "insufficient_scope",
		},
		{
			// The cookie-session shape (empty TokenID, no scopes) — the
			// sibling handlers wave it past their scope gate; this one must
			// NOT.
			name:   "cookie session with empty TokenID",
			withID: withAuth,
			want:   http.StatusForbidden, code: "insufficient_scope",
		},
		{
			// COUNTERFACTUAL c4 vehicle.
			name: "confirm absent",
			body: &rebaseBranchRequest{Reason: "no confirm"},
			want: http.StatusBadRequest, code: "confirmation_required",
		},
		{
			name: "confirm explicitly false",
			body: &rebaseBranchRequest{Reason: "no", Confirm: false},
			want: http.StatusBadRequest, code: "confirmation_required",
		},
		{
			name: "malformed run_id", pathID: "not-a-uuid",
			want: http.StatusBadRequest, code: "validation_failed",
		},
		{
			name: "undecodable body", raw: []byte(`{"confirm":`),
			want: http.StatusBadRequest, code: "validation_failed",
		},
		{
			name: "nil GitHub client", nilForge: true,
			want: http.StatusServiceUnavailable, code: "rebase_unconfigured",
		},
		{
			name: "run has no installation", opts: rebaseOpts{noInstallation: true},
			want: http.StatusUnprocessableEntity, code: "rebase_not_determinable",
		},
		{
			name: "unparseable repo", opts: rebaseOpts{repo: "not-an-owner-slash-name-at-all"},
			want: http.StatusUnprocessableEntity, code: "rebase_not_determinable",
		},
		{
			name: "no tracked pull request", opts: rebaseOpts{noPRURL: true},
			want: http.StatusUnprocessableEntity, code: "rebase_not_determinable",
		},
		{
			name: "GetPullRequest error",
			stub: func() *rebaseGitHub {
				st := cleanRebaseStub()
				st.prStatusSeq = []int{http.StatusInternalServerError}
				return st
			},
			want: http.StatusUnprocessableEntity, code: "rebase_not_determinable",
		},
		{
			name: "empty head sha",
			stub: func() *rebaseGitHub {
				st := cleanRebaseStub()
				st.headSHASeq = []string{""}
				return st
			},
			want: http.StatusUnprocessableEntity, code: "rebase_not_determinable",
		},
		{
			name: "empty head branch",
			stub: func() *rebaseGitHub {
				st := cleanRebaseStub()
				st.headRef = ""
				return st
			},
			want: http.StatusUnprocessableEntity, code: "rebase_not_determinable",
		},
		{
			name: "empty base ref",
			stub: func() *rebaseGitHub {
				st := cleanRebaseStub()
				st.baseRef = ""
				return st
			},
			want: http.StatusUnprocessableEntity, code: "rebase_not_determinable",
		},
		{
			name: "behind-probe compare error",
			stub: func() *rebaseGitHub {
				st := cleanRebaseStub()
				st.compareStatus = http.StatusInternalServerError
				return st
			},
			want: http.StatusUnprocessableEntity, code: "rebase_not_determinable",
		},
		{
			// COUNTERFACTUAL c3 vehicle. The head-SHA sequence differs BY
			// CONSTRUCTION at the lease re-check (call 2), so deleting the
			// re-check lets a POST /merges be recorded against a stale
			// classification.
			name: "lease re-check sees a concurrent push",
			stub: func() *rebaseGitHub {
				st := cleanRebaseStub()
				st.headSHASeq = []string{rebasePriorHeadSHA, "9999999999999999999999999999999999999999", rebaseNewHeadSHA}
				return st
			},
			want: http.StatusUnprocessableEntity, code: "rebase_not_determinable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mk := cleanRebaseStub
			if tc.stub != nil {
				mk = tc.stub
			}
			sd := seedRebaseRun(t, mk(), tc.opts)
			if tc.nilForge {
				sd.s.cfg.GitHub = nil
			}
			withID := withRebaseOperator
			if tc.withID != nil {
				withID = tc.withID
			}
			body := rebaseBranchRequest{Reason: "r", Confirm: true}
			if tc.body != nil {
				body = *tc.body
			}
			raw := tc.raw
			if raw == nil {
				raw, _ = json.Marshal(body)
			}
			pathID := sd.runID.String()
			if tc.pathID != "" {
				pathID = tc.pathID
			}
			w := postRebaseBranchRaw(t, sd.s, sd.runID, pathID, raw, withID)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d:\n%s", w.Code, tc.want, w.Body.String())
			}
			if !bytes.Contains(w.Body.Bytes(), []byte(tc.code)) {
				t.Errorf("body missing %q: %s", tc.code, w.Body.String())
			}
			assertNoWriteBeforeMerge(t, sd)
		})
	}
}

// TestRebaseRunBranch_RunNotFound covers the 404, which needs a run id absent
// from the repo rather than a seeded one.
func TestRebaseRunBranch_RunNotFound(t *testing.T) {
	sd := seedRebaseRun(t, cleanRebaseStub(), rebaseOpts{})
	missing := uuid.New()

	raw, _ := json.Marshal(rebaseBranchRequest{Confirm: true})
	w := postRebaseBranchRaw(t, sd.s, missing, missing.String(), raw, withRebaseOperator)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("run_not_found")) {
		t.Errorf("body missing run_not_found: %s", w.Body.String())
	}
	assertNoWriteBeforeMerge(t, sd)
}

// --- REFUSAL RULE (b): DISCOVERED by the merges endpoint ---

// TestRebaseRunBranch_Conflict is COUNTERFACTUAL c5's vehicle: the merges
// endpoint is hardwired to 409 BY CONSTRUCTION, so deleting the
// errors.Is(err, githubclient.ErrMergeConflict) branch collapses this to a
// 502 rebase_merge_failed and drops the fishhawk_reset_run_branch cross-link.
// Rule (b) applies: EXACTLY ONE merge POST occurred and nothing was written
// after it.
func TestRebaseRunBranch_Conflict(t *testing.T) {
	stub := cleanRebaseStub()
	stub.mergeStatus = http.StatusConflict
	stub.mergeBody = `{"message":"Merge conflict"}`
	sd := seedRebaseRun(t, stub, rebaseOpts{})

	w := postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Confirm: true}, withRebaseOperator)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "rebase_conflict") {
		t.Errorf("body missing rebase_conflict: %s", body)
	}
	// SHIPPED-MESSAGE / CROSS-LINK PINS — none of these is compiler-enforced.
	if !strings.Contains(body, "fishhawk_reset_run_branch") {
		t.Errorf("the conflict refusal must cross-link the sibling verb: %s", body)
	}
	if !strings.Contains(body, "#3202") {
		t.Errorf("the conflict refusal must name the deferred agent-resolution issue: %s", body)
	}
	if !strings.Contains(body, "NOTHING was written") {
		t.Errorf("the conflict refusal must state that nothing was written: %s", body)
	}
	assertMergeAttemptedNothingWritten(t, sd)
}

// TestRebaseRunBranch_MergeFailed: any non-conflict merge error → 502
// rebase_merge_failed. Rule (b) applies.
func TestRebaseRunBranch_MergeFailed(t *testing.T) {
	stub := cleanRebaseStub()
	stub.mergeStatus = http.StatusInternalServerError
	stub.mergeBody = `{"message":"boom"}`
	sd := seedRebaseRun(t, stub, rebaseOpts{})

	w := postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Confirm: true}, withRebaseOperator)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("rebase_merge_failed")) {
		t.Errorf("body missing rebase_merge_failed: %s", w.Body.String())
	}
	assertMergeAttemptedNothingWritten(t, sd)
}

// TestRebaseRunBranch_NoReviewStage: the commit-yourself shape (no separate
// review stage) — the merge, audit and republish still happen; the re-park is
// a tolerated no-op.
func TestRebaseRunBranch_NoReviewStage(t *testing.T) {
	sd := seedRebaseRun(t, cleanRebaseStub(), rebaseOpts{noReviewStage: true})

	w := postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Confirm: true}, withRebaseOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if len(sd.stub.merges()) != 1 {
		t.Error("the merge did not happen on the no-review shape")
	}
	if branchRebasedAudit(sd.au) == nil {
		t.Error("branch_rebased audit not written on the no-review shape")
	}
	if transitionedTo(sd.rr, run.StageStatePending) {
		t.Error("a stage was re-parked despite there being no review stage")
	}
	var resp rebaseBranchResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ReparkedReviewStageID != "" {
		t.Errorf("reparked_review_stage_id = %q, want empty", resp.ReparkedReviewStageID)
	}
}

// --- AUDITCOMPLETE SEQUENCE GUARD, both orderings ---

// TestRebaseRunBranch_AuditEntryPrecedesRepublish asserts the branch_rebased
// entry is APPENDED BEFORE the recompute/publish, so the recompute observes
// it. Ordering is read off the fake's own append/publish interleaving via a
// creator that snapshots the audit-entry count at publish time.
func TestRebaseRunBranch_AuditEntryPrecedesRepublish(t *testing.T) {
	sd := seedRebaseRun(t, cleanRebaseStub(), rebaseOpts{noPublisher: true})
	seen := make(chan int, 4)
	creator := &orderingCheckCreator{au: sd.au, seen: seen}
	pub := auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub:      creator,
		Runs:        sd.rr,
		Artifacts:   sd.s.cfg.ArtifactRepo,
		Audit:       sd.au,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	sd.s.auditCheckPublisher = pub

	w := postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Confirm: true}, withRebaseOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	select {
	case n := <-seen:
		if n < 1 {
			t.Errorf("the recompute observed %d branch_rebased entries; the entry must be appended BEFORE the publish", n)
		}
	default:
		t.Fatal("no check run was published")
	}
}

// orderingCheckCreator records how many branch_rebased entries were already on
// the chain at the moment the Check Run was created.
type orderingCheckCreator struct {
	au   *auditFake
	seen chan int
}

func (c *orderingCheckCreator) CreateCheckRun(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, _ forge.CreateCheckRunParams) (*forge.CreateCheckRunResult, error) {
	c.seen <- len(auditEntries(c.au, CategoryBranchRebased))
	return &forge.CreateCheckRunResult{ID: 1}, nil
}

// TestRebaseRunBranch_LaterFixupPushRepublishesAtFixupHead is the second
// ordering: a fix-up push landing AFTER a rebase must republish at the FIX-UP
// head, not the rebase head — the rebase's head override applies to the rebase
// invocation only and must not pin later publications.
func TestRebaseRunBranch_LaterFixupPushRepublishesAtFixupHead(t *testing.T) {
	const fixupHead = "dddd444444444444444444444444444444444444"
	sd := seedRebaseRun(t, cleanRebaseStub(), rebaseOpts{})

	w := postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Confirm: true}, withRebaseOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if got := checkPublications(sd.creator); len(got) != 1 || got[0] != rebaseNewHeadSHA {
		t.Fatalf("post-rebase publications = %v, want one at %q", got, rebaseNewHeadSHA)
	}

	// A fix-up push lands afterwards, reporting a NEW head.
	seedRunHeadEntry(sd.au, sd.runID, "fixup_pushed", fixupHead, 99)
	sd.s.recomputeAndPublishAuditComplete(context.Background(), sd.runID)

	pubs := checkPublications(sd.creator)
	if len(pubs) < 2 {
		t.Fatalf("publications = %v, want a second one after the fix-up push", pubs)
	}
	if last := pubs[len(pubs)-1]; last != fixupHead {
		t.Errorf("post-fixup publication head = %q, want the FIX-UP head %q (the rebase head must not pin later publications)",
			last, fixupHead)
	}
}

// --- CONDITION 3: the ADR-035 reported-head ledger ---

// TestRebaseRunBranch_LedgerAttributesTheMergeCommit is the executed anchor
// for the plan's highest-risk unknown. The merge commit this verb creates is
// authored by the App installation but appears in NO head-report audit
// category, so buildReportedHeadLedger would read it as FOREIGN and wedge the
// run this verb just un-wedged.
//
// It drives the REAL ledger recompute (ReverifyBranchLineage, which is what
// the merge reconciler calls) against the post-rebase branch state, rather
// than relying on the audit-complete publish as a proxy — the audit-complete
// recompute does NOT consult the ledger, so a publish assertion is structurally
// incapable of catching this and would have stayed GREEN. Deleting the
// writeRebaseLineageAttribution call turns this RED.
func TestRebaseRunBranch_LedgerAttributesTheMergeCommit(t *testing.T) {
	sd := seedRebaseRun(t, cleanRebaseStub(), rebaseOpts{})
	// The PR-open head is the run's only ledger member before the rebase.
	seedRunHeadEntry(sd.au, sd.runID, "pull_request_opened", rebasePriorHeadSHA, 1)

	w := postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Reason: "advance", Confirm: true}, withRebaseOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	// Move the stub to the post-rebase world: the branch tip IS the merge
	// commit, and a compare from the base ref enumerates both the run's own
	// commit and the merge commit.
	sd.stub.mu.Lock()
	sd.stub.headSHA = rebaseNewHeadSHA
	sd.stub.headSHASeq = nil
	sd.stub.behindCommits = []string{rebasePriorHeadSHA, rebaseNewHeadSHA}
	sd.stub.mu.Unlock()

	if !sd.s.ReverifyBranchLineage(context.Background(), sd.runID, 77) {
		t.Fatal("the ADR-035 ledger flagged the rebase merge commit as FOREIGN; " +
			"the verb would wedge the very run it un-wedged")
	}
}

// TestRebaseRunBranch_LedgerStillFlagsAnUnattributedForeignCommit is the
// fail-closed pair for the attribution above: attributing the merge commit
// must NOT launder an unrelated foreign commit. A commit neither reported nor
// attributed still violates.
func TestRebaseRunBranch_LedgerStillFlagsAnUnattributedForeignCommit(t *testing.T) {
	const trulyForeign = "eeee555555555555555555555555555555555555"
	sd := seedRebaseRun(t, cleanRebaseStub(), rebaseOpts{})
	seedRunHeadEntry(sd.au, sd.runID, "pull_request_opened", rebasePriorHeadSHA, 1)

	w := postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Confirm: true}, withRebaseOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	sd.stub.mu.Lock()
	sd.stub.headSHA = trulyForeign
	sd.stub.headSHASeq = nil
	sd.stub.behindCommits = []string{rebasePriorHeadSHA, rebaseNewHeadSHA, trulyForeign}
	sd.stub.mu.Unlock()

	if sd.s.ReverifyBranchLineage(context.Background(), sd.runID, 77) {
		t.Error("an UNattributed foreign commit must still violate; the rebase attribution must not launder it")
	}
}

// --- CONDITION 4: the rebase consumes no fix-up budget ---

// TestRebaseRunBranch_ConsumesNoFixupBudget is the blocking
// [rebase-does-not-consume-fixup-budget] criterion, decided from committed
// evidence rather than from reading the handler. It reads the run's stage list
// and the fix-up budget counter (the durable count of stage_fixup_triggered
// audit entries — there is no dedicated column, see run.FixupOptions) BEFORE
// and AFTER the call and asserts both unchanged.
func TestRebaseRunBranch_ConsumesNoFixupBudget(t *testing.T) {
	sd := seedRebaseRun(t, cleanRebaseStub(), rebaseOpts{})

	snapshot := func() ([]string, int) {
		stages, err := sd.rr.ListStagesForRun(context.Background(), sd.runID)
		if err != nil {
			t.Fatalf("list stages: %v", err)
		}
		out := make([]string, 0, len(stages))
		for _, st := range stages {
			out = append(out, fmt.Sprintf("%s/%s/%s", st.ID, st.Type, st.State))
		}
		return out, len(auditEntries(sd.au, CategoryStageFixupTriggered))
	}

	beforeStages, beforeFixups := snapshot()

	w := postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Reason: "advance", Confirm: true}, withRebaseOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	afterStages, afterFixups := snapshot()
	if afterFixups != beforeFixups {
		t.Errorf("fix-up budget counter changed: %d → %d; the rebase must consume no fix-up budget",
			beforeFixups, afterFixups)
	}
	if len(afterStages) != len(beforeStages) {
		t.Fatalf("stage list length changed: %d → %d; the rebase must add no stage",
			len(beforeStages), len(afterStages))
	}
	for i := range beforeStages {
		if beforeStages[i] != afterStages[i] {
			t.Errorf("stage %d changed: %q → %q; the re-park must land back on the same state",
				i, beforeStages[i], afterStages[i])
		}
	}
}

// --- FULL-SPAN CROSS-BOUNDARY ---

// TestRebaseRunBranch_ToCheckPublication_EndToEnd is the ONE case spanning
// every layer: it serves s.Handler() on an httptest.Server, drives the real
// HTTP route with an operator token, and asserts in that one case:
//
//   - the response decodes with new_head_sha / merge_commit_sha /
//     mechanism_note populated;
//   - the recorded POST /merges body has base=<run branch>, head=<base ref>;
//   - a branch_rebased entry was persisted carrying prior_head_sha and
//     new_head_sha (CONDITION 6a);
//   - the sticky status comment was refreshed (CONDITION 6b);
//   - exactly one Check Run was published at the post-merge live head.
func TestRebaseRunBranch_ToCheckPublication_EndToEnd(t *testing.T) {
	sd := seedRebaseRun(t, cleanRebaseStub(), rebaseOpts{})
	notifier := &pageClassRecorder{}
	sd.s.issueNotifier = notifier

	srv := httptest.NewServer(sd.s.Handler())
	t.Cleanup(srv.Close)

	raw, _ := json.Marshal(rebaseBranchRequest{Reason: "base advanced under review", Confirm: true})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		srv.URL+"/v0/runs/"+sd.runID.String()+"/rebase-branch", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sd.bearer)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", resp.StatusCode, string(bodyBytes))
	}
	var out rebaseBranchResponse
	if err := json.Unmarshal(bodyBytes, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.NewHeadSHA != rebaseNewHeadSHA || out.MergeCommitSHA != rebaseNewHeadSHA {
		t.Errorf("new_head_sha/merge_commit_sha = %q/%q, want %q", out.NewHeadSHA, out.MergeCommitSHA, rebaseNewHeadSHA)
	}
	if !strings.Contains(out.MechanismNote, "merge commit") {
		t.Errorf("mechanism_note = %q", out.MechanismNote)
	}

	merges := sd.stub.merges()
	if len(merges) != 1 || merges[0].Base != rebaseBranchName || merges[0].Head != rebaseBaseRef {
		t.Fatalf("merge calls = %+v, want one with base=%q head=%q", merges, rebaseBranchName, rebaseBaseRef)
	}

	// CONDITION 6a: the branch_rebased audit record.
	a := branchRebasedAudit(sd.au)
	if a == nil {
		t.Fatal("no branch_rebased audit entry persisted")
	}
	if a.ActorKind == nil || *a.ActorKind != audit.ActorUser {
		t.Errorf("audit actor = %v, want user", a.ActorKind)
	}
	var payload struct {
		PriorHeadSHA    string `json:"prior_head_sha"`
		NewHeadSHA      string `json:"new_head_sha"`
		MergeCommitSHA  string `json:"merge_commit_sha"`
		BaseRef         string `json:"base_ref"`
		AlreadyUpToDate bool   `json:"already_up_to_date"`
		MechanismNote   string `json:"mechanism_note"`
	}
	if err := json.Unmarshal(a.Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	if payload.PriorHeadSHA != rebasePriorHeadSHA || payload.NewHeadSHA != rebaseNewHeadSHA {
		t.Errorf("audit payload heads = %q → %q, want %q → %q",
			payload.PriorHeadSHA, payload.NewHeadSHA, rebasePriorHeadSHA, rebaseNewHeadSHA)
	}
	if payload.BaseRef != rebaseBaseRef || payload.AlreadyUpToDate {
		t.Errorf("audit payload = %+v", payload)
	}
	if !strings.Contains(payload.MechanismNote, "merge commit") {
		t.Errorf("audit payload mechanism_note = %q", payload.MechanismNote)
	}

	// CONDITION 6b: the sticky status comment was refreshed.
	if n := len(notifier.status); n == 0 {
		t.Error("the status comment was not refreshed after the base advance")
	}

	// Exactly one Check Run, at the post-merge live head.
	if got := checkPublications(sd.creator); len(got) != 1 || got[0] != rebaseNewHeadSHA {
		t.Errorf("check publications = %v, want exactly one at %q", got, rebaseNewHeadSHA)
	}
}

// --- POST-MERGE ATTRIBUTION RACE (fix-up pass 1, concerns 98e012bd + bf777c15) ---

// rebaseRacerSHA is a foreign commit pushed by someone else that lands in the
// window BETWEEN the merges POST and the post-merge PR re-read. It is not the
// commit this invocation created, and the ledger must keep saying so.
const rebaseRacerSHA = "dddd444444444444444444444444444444444444"

// attributedSHAs returns every sha admitted into the ADR-035 reported-head
// ledger by an operator_commit_vouched entry — read back as COMMITTED STATE
// rather than inferred from the response, because "was it attributed" is a
// question about what was persisted.
func attributedSHAs(au *auditFake) []string {
	var out []string
	for _, e := range auditEntries(au, CategoryOperatorCommitVouched) {
		var p struct {
			VouchedSHA string `json:"vouched_sha"`
		}
		if err := json.Unmarshal(e.Payload, &p); err == nil && p.VouchedSHA != "" {
			out = append(out, p.VouchedSHA)
		}
	}
	return out
}

// TestRebaseRunBranch_ConcurrentPushIntoPostMergeRead_IsNotAttributed is the
// PRIORITY 1 security control. The lease re-check runs only BEFORE the merge,
// so a foreign push landing between MergeBranch and the post-merge
// GetPullRequest becomes newHeadSHA. Attributing it would admit a commit this
// invocation did not create into the ADR-035 reported-head ledger — laundering
// exactly the foreign commit
// TestRebaseRunBranch_LedgerStillFlagsAnUnattributedForeignCommit exists to
// prove the ledger catches, and defeating the fail-closed property that makes
// reset-branch and vouch-commit meaningful.
//
// The race is seeded BY CONSTRUCTION, not by timing: the merges endpoint
// returns a known merge sha and the SUBSEQUENT PR read returns a DIFFERENT,
// unrelated sha. That is precisely the observable state a real concurrent push
// produces, and it is in-band detectable — a post-merge head that differs from
// the merge commit means something else landed.
//
// The assertions are committed-state read-backs, never error identity:
//
//  1. the ledger carries the merge commit and NOT the racer;
//  2. the REAL ReverifyBranchLineage recompute still flags the racer FOREIGN;
//  3. the response surfaces the divergence so the operator learns of it.
//
// COUNTERFACTUAL (recorded in the commit message): restoring the previous
// `for _, sha := range []string{mergeCommitSHA, newHeadSHA}` loop attributes
// the racer, ReverifyBranchLineage returns true, and assertions 1 and 2 both
// go RED.
func TestRebaseRunBranch_ConcurrentPushIntoPostMergeRead_IsNotAttributed(t *testing.T) {
	stub := cleanRebaseStub()
	// PR reads: 1 = handler head, 2 = lease re-check (still the prior head, so
	// the merge proceeds), 3 = the post-merge re-read, which now observes a
	// FOREIGN commit that raced in after the merge.
	stub.headSHASeq = []string{rebasePriorHeadSHA, rebasePriorHeadSHA, rebaseRacerSHA}
	sd := seedRebaseRun(t, stub, rebaseOpts{})
	seedRunHeadEntry(sd.au, sd.runID, "pull_request_opened", rebasePriorHeadSHA, 1)

	w := postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Reason: "advance", Confirm: true}, withRebaseOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the merge completed):\n%s", w.Code, w.Body.String())
	}
	var resp rebaseBranchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Preconditions of the race, so a GREEN here can never be a mis-seeded stub.
	if resp.MergeCommitSHA != rebaseNewHeadSHA {
		t.Fatalf("merge_commit_sha = %q, want the merge this call created %q", resp.MergeCommitSHA, rebaseNewHeadSHA)
	}
	if resp.NewHeadSHA != rebaseRacerSHA {
		t.Fatalf("new_head_sha = %q, want the raced-in foreign head %q — the race is not seeded",
			resp.NewHeadSHA, rebaseRacerSHA)
	}

	// (1) COMMITTED STATE: exactly the merge commit was attributed, and the
	// racer was not.
	got := attributedSHAs(sd.au)
	if len(got) != 1 || got[0] != rebaseNewHeadSHA {
		t.Errorf("attributed shas = %v, want exactly [%s]: only the merge commit this invocation created has provable provenance",
			got, rebaseNewHeadSHA)
	}
	for _, sha := range got {
		if sha == rebaseRacerSHA {
			t.Errorf("the raced-in foreign commit %s was attributed as run-authored; that launders exactly the commit the ADR-035 ledger exists to catch",
				rebaseRacerSHA)
		}
	}

	// (3) The operator is TOLD a concurrent push landed — an unreported
	// divergence is the same silent-success defect as an unreported append
	// failure.
	if !strings.Contains(resp.LineageAttributionWarning, rebaseRacerSHA) {
		t.Errorf("lineage_attribution_warning must name the divergent head %q: %q",
			rebaseRacerSHA, resp.LineageAttributionWarning)
	}
	if !strings.Contains(resp.LineageAttributionWarning, "fishhawk_vouch_commit") {
		t.Errorf("lineage_attribution_warning must name fishhawk_vouch_commit as the verb that can admit a legitimate pushed commit: %q",
			resp.LineageAttributionWarning)
	}

	// (2) THE REAL RECOMPUTE, driven against the actual ledger builder rather
	// than a stub: with the racer on the branch tip the run must STILL be
	// flagged foreign.
	sd.stub.mu.Lock()
	sd.stub.headSHA = rebaseRacerSHA
	sd.stub.headSHASeq = nil
	sd.stub.behindCommits = []string{rebasePriorHeadSHA, rebaseNewHeadSHA, rebaseRacerSHA}
	sd.stub.mu.Unlock()
	if sd.s.ReverifyBranchLineage(context.Background(), sd.runID, 77) {
		t.Error("the ADR-035 ledger ACCEPTED a foreign commit that raced into the post-merge read; the rebase attribution laundered it")
	}
}

// TestRebaseRunBranch_AttributionAppendFails_WarnsAndNamesVouch is PRIORITY
// 2(a). The attribution write is load-bearing — without it the
// installation-authored merge commit sits in no head-report category and
// buildReportedHeadLedger flags it FOREIGN — so "best-effort with only a Warn
// log" made the handler able to return 200, publish fishhawk_audit_complete
// and leave the run wedged with the operator told nothing.
//
// The failure is injected in ISOLATION (appendErrCategory targets only
// operator_commit_vouched), so the branch_rebased entry still lands and the
// test discriminates an attribution failure from a blanket audit outage.
//
// COUNTERFACTUAL: deleting the append-error warning branch (returning the
// bare `warning` instead of `appendWarning`) turns the two response
// assertions RED.
func TestRebaseRunBranch_AttributionAppendFails_WarnsAndNamesVouch(t *testing.T) {
	sd := seedRebaseRun(t, cleanRebaseStub(), rebaseOpts{})
	sd.au.appendErrCategory = CategoryOperatorCommitVouched

	w := postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Reason: "advance", Confirm: true}, withRebaseOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the merge already completed; a refusal would misreport a durable write):\n%s",
			w.Code, w.Body.String())
	}
	var resp rebaseBranchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// The discriminator: the base advance itself IS recorded, so the warning
	// is about the attribution alone.
	if branchRebasedAudit(sd.au) == nil {
		t.Fatal("no branch_rebased entry — the injected failure is not isolated to the attribution append")
	}
	// COMMITTED STATE: nothing was admitted into the ledger.
	if got := attributedSHAs(sd.au); len(got) != 0 {
		t.Fatalf("attributed shas = %v, want none — the append was injected to fail", got)
	}

	if resp.LineageAttributionWarning == "" {
		t.Fatal("a 200 with NO lineage_attribution_warning reports a clean recovery while the run is wedged on the lineage check")
	}
	if !strings.Contains(resp.LineageAttributionWarning, "fishhawk_vouch_commit") {
		t.Errorf("the warning must name fishhawk_vouch_commit as the required step: %q", resp.LineageAttributionWarning)
	}
	if !strings.Contains(resp.LineageAttributionWarning, "will NOT repair") {
		t.Errorf("the warning must say re-invoking this verb will NOT repair the attribution — advertising a retry that cannot deliver is the defect this closes: %q",
			resp.LineageAttributionWarning)
	}
}

// TestRebaseRunBranch_NoAttributableSHA_ThroughReinvocation_NamesVouch is
// PRIORITY 2(b): the COMBINED degraded path, driven THROUGH the reinvocation
// the response used to advertise.
//
// Invocation 1 merges with an undecodable-201 (no merge sha) AND a failing
// post-merge re-read, so there is no sha to attribute at all. The old response
// directed the operator only to re-invoke this verb. Invocation 2 proves that
// instruction was FALSE: the behind-probe short-circuits, the
// already-contains-base arm deliberately attributes nothing, the check
// publishes — and the run is STILL un-attributed and STILL flagged foreign by
// the real recompute.
//
// So the contract asserted here is that invocation 1 says so up front and
// names fishhawk_vouch_commit.
//
// COUNTERFACTUAL: deleting the `mergeCommitSHA == "" && newHeadSHA == ""`
// nothing-attributable branch turns the invocation-1 warning assertions RED.
func TestRebaseRunBranch_NoAttributableSHA_ThroughReinvocation_NamesVouch(t *testing.T) {
	stub := cleanRebaseStub()
	stub.mergeBody = `{"no_sha_here":true}`                                                // the benign undecodable 201
	stub.prStatusSeq = []int{http.StatusOK, http.StatusOK, http.StatusInternalServerError} // post-merge re-read fails
	sd := seedRebaseRun(t, stub, rebaseOpts{})
	seedRunHeadEntry(sd.au, sd.runID, "pull_request_opened", rebasePriorHeadSHA, 1)

	w1 := postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Reason: "advance", Confirm: true}, withRebaseOperator)
	if w1.Code != http.StatusOK {
		t.Fatalf("invocation 1 status = %d, want 200:\n%s", w1.Code, w1.Body.String())
	}
	var r1 rebaseBranchResponse
	if err := json.Unmarshal(w1.Body.Bytes(), &r1); err != nil {
		t.Fatalf("unmarshal 1: %v", err)
	}
	// Preconditions: neither sha is known, so nothing is attributable.
	if r1.MergeCommitSHA != "" || r1.NewHeadSHA != "" {
		t.Fatalf("invocation 1 merge_commit_sha/new_head_sha = %q/%q, want both empty — the combined degraded path is not seeded",
			r1.MergeCommitSHA, r1.NewHeadSHA)
	}
	if got := attributedSHAs(sd.au); len(got) != 0 {
		t.Fatalf("invocation 1 attributed %v, want nothing (no sha is known)", got)
	}
	if !strings.Contains(r1.LineageAttributionWarning, "fishhawk_vouch_commit") {
		t.Errorf("invocation 1 must name fishhawk_vouch_commit — re-invocation cannot repair this: %q",
			r1.LineageAttributionWarning)
	}
	if !strings.Contains(r1.LineageAttributionWarning, "will NOT repair") {
		t.Errorf("invocation 1 must state plainly that re-invoking this verb will NOT repair the attribution: %q",
			r1.LineageAttributionWarning)
	}

	// Move the world to the post-merge state and RE-INVOKE, exactly as an
	// operator following a bare "re-invoke to retry" instruction would.
	stub.mu.Lock()
	stub.behindCommits = nil
	stub.headSHA = rebaseNewHeadSHA
	stub.headSHASeq = nil
	stub.prStatusSeq = nil
	stub.mergeBody = ""
	stub.mu.Unlock()
	mergesAfterFirst := len(stub.merges())

	w2 := postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Reason: "retry", Confirm: true}, withRebaseOperator)
	if w2.Code != http.StatusOK {
		t.Fatalf("invocation 2 status = %d, want 200:\n%s", w2.Code, w2.Body.String())
	}
	var r2 rebaseBranchResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &r2); err != nil {
		t.Fatalf("unmarshal 2: %v", err)
	}
	if len(stub.merges()) != mergesAfterFirst || !r2.AlreadyUpToDate {
		t.Fatalf("invocation 2 merges %d→%d, already_up_to_date=%v; want no second merge and the short-circuit arm",
			mergesAfterFirst, len(stub.merges()), r2.AlreadyUpToDate)
	}

	// THE POINT, read back as committed state: the retry attributed NOTHING,
	// so invocation 1's warning was truthful and a bare "re-invoke this verb"
	// instruction would have been a false recovery contract.
	if got := attributedSHAs(sd.au); len(got) != 0 {
		t.Errorf("re-invocation attributed %v; the already-contains-base arm must attribute nothing, and if it ever does this test's premise must be revisited", got)
	}
	// THE REAL RECOMPUTE: the run is still wedged after the advertised retry.
	sd.stub.mu.Lock()
	sd.stub.behindCommits = []string{rebasePriorHeadSHA, rebaseNewHeadSHA}
	sd.stub.mu.Unlock()
	if sd.s.ReverifyBranchLineage(context.Background(), sd.runID, 77) {
		t.Error("the ledger accepted the un-attributed merge commit after re-invocation; " +
			"if that becomes true, invocation 1's fishhawk_vouch_commit instruction is no longer required and must be revisited")
	}
}
