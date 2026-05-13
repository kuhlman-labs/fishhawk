package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// Tests the slash-command approval flow end-to-end through the
// Server.HandleApprovalCommand path (#238). Uses real-ish fakes
// for the run, approval, audit, stagecheck, and orchestrator
// repos; swaps in a recording GitHub commenter so reply text can
// be asserted on.

func TestHandleApprovalCommand_PlanApprove_PostsOnlyBroadcast(t *testing.T) {
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	r.TriggerSource = run.TriggerGitHubIssue
	triggerRef := "issue:42"
	r.TriggerRef = &triggerRef
	r.InstallationID = ptrInt64(99)
	r.Repo = "x/y"

	stage := rr.seedStage(r.ID, 0, run.StageStateAwaitingApproval)
	stage.Type = run.StageTypePlan

	ar := newFakeApprovalRepo()
	au := newAuditCompleteAuditFake()
	gh := newSlashGitHubRecorder()
	o := &orchestrator.Orchestrator{Runs: rr}

	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		ApprovalRepo: ar,
		AuditRepo:    au,
		Orchestrator: o,
		ExternalURL:  "https://app.fishhawk.example.com",
	})
	s.issueNotifier = issuecomment.New(issuecomment.Deps{
		GitHub:      gh,
		Runs:        rr,
		Audit:       au,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	if err := s.HandleApprovalCommand(context.Background(), webhook.ApprovalCommandParams{
		Repo:           "x/y",
		IssueNumber:    42,
		InstallationID: 99,
		SenderLogin:    "alice",
		Decision:       webhook.MatchActionApprove,
	}); err != nil {
		t.Fatalf("HandleApprovalCommand: %v", err)
	}

	if stage.State != run.StageStateSucceeded {
		t.Errorf("stage state = %q, want succeeded", stage.State)
	}
	if len(ar.all) != 1 {
		t.Fatalf("approval not recorded: %+v", ar.all)
	}
	if ar.all[0].Surface != approval.SurfaceGitHubComment {
		t.Errorf("approval.Surface = %q, want github_comment", ar.all[0].Surface)
	}
	if ar.all[0].ApproverSubject != "alice" {
		t.Errorf("approver = %q, want alice", ar.all[0].ApproverSubject)
	}

	// Exactly one comment expected: the plan-approved broadcast
	// (#274). The redundant slash reply is suppressed on the plan-
	// approve path (#304) so the issue thread sees a single canonical
	// confirmation.
	got := gh.calls()
	if len(got) != 1 {
		t.Fatalf("expected 1 comment (plan-approved broadcast only); got %d", len(got))
	}
	if !strings.Contains(got[0].body, "Plan approved by") {
		t.Errorf("broadcast should announce plan approval: %q", got[0].body)
	}
	if !strings.Contains(got[0].body, "@alice") {
		t.Errorf("plan-approved broadcast should mention approver: %q", got[0].body)
	}
	// Regression guard for #305: the @login must NOT be wrapped in
	// backticks — GitHub only fires a mention notification when the
	// handle is bare.
	if strings.Contains(got[0].body, "`@") {
		t.Errorf("plan-approved broadcast must not backtick-wrap the @mention (breaks GitHub notification): %q", got[0].body)
	}
}

func TestHandleApprovalCommand_Reject_FailsStageAndReplies(t *testing.T) {
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	r.TriggerSource = run.TriggerGitHubIssue
	triggerRef := "issue:7"
	r.TriggerRef = &triggerRef
	r.InstallationID = ptrInt64(99)
	r.Repo = "x/y"

	stage := rr.seedStage(r.ID, 0, run.StageStateAwaitingApproval)
	// Plan stage — review-stage rejects are refused on the slash path
	// per ADR-018 / #313 (covered by TestHandleApprovalCommand_ReviewStage_RejectAlsoRefused).
	// This test pins the reject end-to-end for plan stages.
	stage.Type = run.StageTypePlan

	ar := newFakeApprovalRepo()
	au := newAuditCompleteAuditFake()
	gh := newSlashGitHubRecorder()
	o := &orchestrator.Orchestrator{Runs: rr}

	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		ApprovalRepo: ar,
		AuditRepo:    au,
		Orchestrator: o,
		ExternalURL:  "https://app.fishhawk.example.com",
	})
	s.issueNotifier = issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: rr, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	if err := s.HandleApprovalCommand(context.Background(), webhook.ApprovalCommandParams{
		Repo:           "x/y",
		IssueNumber:    7,
		InstallationID: 99,
		SenderLogin:    "bob",
		Decision:       webhook.MatchActionReject,
		Comment:        "scope is too wide",
	}); err != nil {
		t.Fatal(err)
	}
	if stage.State != run.StageStateFailed {
		t.Errorf("stage state = %q, want failed", stage.State)
	}
	if got := ar.all[0].Decision; got != approval.DecisionReject {
		t.Errorf("decision = %q", got)
	}
	if got := ar.all[0].Comment; got == nil || *got != "scope is too wide" {
		t.Errorf("comment = %v, want 'scope is too wide'", got)
	}
	if !strings.Contains(gh.calls()[0].body, "Rejected") {
		t.Errorf("reply should say Rejected: %q", gh.calls()[0].body)
	}
}

// TestHandleApprovalCommand_PlanReject_StillPostsSlashReply locks in
// the contract from #304: the plan-approve case is the only path that
// suppresses the slash reply. A plan-stage reject has no companion
// broadcast (NotifyPlanApproved only fires on approve), so the slash
// reply remains the sole confirmation on the issue thread.
func TestHandleApprovalCommand_PlanReject_StillPostsSlashReply(t *testing.T) {
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	r.TriggerSource = run.TriggerGitHubIssue
	triggerRef := "issue:42"
	r.TriggerRef = &triggerRef
	r.InstallationID = ptrInt64(99)
	r.Repo = "x/y"

	stage := rr.seedStage(r.ID, 0, run.StageStateAwaitingApproval)
	stage.Type = run.StageTypePlan

	ar := newFakeApprovalRepo()
	au := newAuditCompleteAuditFake()
	gh := newSlashGitHubRecorder()
	o := &orchestrator.Orchestrator{Runs: rr}

	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr, ApprovalRepo: ar, AuditRepo: au,
		Orchestrator: o, ExternalURL: "https://app.fishhawk.example.com",
	})
	s.issueNotifier = issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: rr, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	if err := s.HandleApprovalCommand(context.Background(), webhook.ApprovalCommandParams{
		Repo: "x/y", IssueNumber: 42, InstallationID: 99, SenderLogin: "alice",
		Decision: webhook.MatchActionReject,
	}); err != nil {
		t.Fatal(err)
	}
	if stage.State != run.StageStateFailed {
		t.Errorf("stage state = %q, want failed", stage.State)
	}
	got := gh.calls()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 comment (slash reply on plan-reject); got %d: %+v", len(got), got)
	}
	if !strings.HasPrefix(got[0].body, "Rejected by ") {
		t.Errorf("slash reply should start with 'Rejected by ': %q", got[0].body)
	}
}

func TestHandleApprovalCommand_ReviewStage_PostsHelpReply(t *testing.T) {
	// ADR-018 / #313: review-stage approval moved to GitHub.
	// `/fishhawk approve` against a review stage replies with a
	// help message that names the PR; no approval row written,
	// no stage transition.
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	r.TriggerSource = run.TriggerGitHubIssue
	triggerRef := "issue:42"
	r.TriggerRef = &triggerRef
	r.InstallationID = ptrInt64(99)
	r.Repo = "x/y"
	prURL := "https://github.com/x/y/pull/42"
	r.PullRequestURL = &prURL

	stage := rr.seedStage(r.ID, 0, run.StageStateAwaitingApproval)
	stage.Type = run.StageTypeReview

	ar := newFakeApprovalRepo()
	au := newAuditCompleteAuditFake()
	gh := newSlashGitHubRecorder()

	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr, ApprovalRepo: ar, AuditRepo: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	s.issueNotifier = issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: rr, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	if err := s.HandleApprovalCommand(context.Background(), webhook.ApprovalCommandParams{
		Repo: "x/y", IssueNumber: 42, InstallationID: 99, SenderLogin: "alice",
		Decision: webhook.MatchActionApprove,
	}); err != nil {
		t.Fatal(err)
	}

	// No approval recorded — the prune runs before submit.
	if len(ar.all) != 0 {
		t.Errorf("approval should not be recorded; got %+v", ar.all)
	}
	// Stage stays awaiting_approval.
	if stage.State != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval", stage.State)
	}
	// Help reply posted with the PR URL.
	calls := gh.calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 reply; got %d", len(calls))
	}
	body := calls[0].body
	if !strings.Contains(body, "Review-stage approval is recorded from GitHub's PR surface") {
		t.Errorf("reply body should explain the GitHub-side move: %q", body)
	}
	if !strings.Contains(body, prURL) {
		t.Errorf("reply body should include PR URL: %q", body)
	}
}

func TestHandleApprovalCommand_ReviewStage_RejectAlsoRefused(t *testing.T) {
	// Reject command targeting a review stage gets the same help
	// reply. The surface is gone, not the verb.
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	r.TriggerSource = run.TriggerGitHubIssue
	triggerRef := "issue:42"
	r.TriggerRef = &triggerRef
	r.InstallationID = ptrInt64(99)
	r.Repo = "x/y"
	stage := rr.seedStage(r.ID, 0, run.StageStateAwaitingApproval)
	stage.Type = run.StageTypeReview

	ar := newFakeApprovalRepo()
	au := newAuditCompleteAuditFake()
	gh := newSlashGitHubRecorder()
	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr, ApprovalRepo: ar, AuditRepo: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	s.issueNotifier = issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: rr, Audit: au, ExternalURL: "https://app.fishhawk.example.com",
	})

	if err := s.HandleApprovalCommand(context.Background(), webhook.ApprovalCommandParams{
		Repo: "x/y", IssueNumber: 42, InstallationID: 99, SenderLogin: "alice",
		Decision: webhook.MatchActionReject,
	}); err != nil {
		t.Fatal(err)
	}
	if len(ar.all) != 0 {
		t.Errorf("approval should not be recorded; got %+v", ar.all)
	}
	if stage.State != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval", stage.State)
	}
	body := gh.calls()[0].body
	if !strings.Contains(body, "Review-stage approval is recorded from GitHub's PR surface") {
		t.Errorf("reject reply should also explain the move: %q", body)
	}
}

func TestHandleApprovalCommand_NoAwaitingStage_RepliesAndSkips(t *testing.T) {
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	r.TriggerSource = run.TriggerGitHubIssue
	triggerRef := "issue:1"
	r.TriggerRef = &triggerRef
	r.InstallationID = ptrInt64(99)
	r.Repo = "x/y"

	// Stage is in pending — not awaiting approval.
	rr.seedStage(r.ID, 0, run.StageStatePending)

	ar := newFakeApprovalRepo()
	au := newAuditCompleteAuditFake()
	gh := newSlashGitHubRecorder()

	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr, ApprovalRepo: ar, AuditRepo: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	s.issueNotifier = issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: rr, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	if err := s.HandleApprovalCommand(context.Background(), webhook.ApprovalCommandParams{
		Repo: "x/y", IssueNumber: 1, InstallationID: 99, SenderLogin: "alice",
		Decision: webhook.MatchActionApprove,
	}); err != nil {
		t.Fatal(err)
	}
	if len(ar.all) != 0 {
		t.Errorf("approval should not be recorded; got %+v", ar.all)
	}
	if !strings.Contains(gh.calls()[0].body, "No stage on this issue's run is awaiting approval") {
		t.Errorf("reply should explain no awaiting stage: %q", gh.calls()[0].body)
	}
}

// TestHandleApprovalCommand_ApproveSucceedsRegardlessOfCheckState
// pins the post-#253 (ADR-017) contract for the slash-command
// approval path: a failing blocking check no longer refuses approve.
// Reviewers approve based on plan + diff; GitHub branch protection
// blocks the merge until the required checks report green.
func TestHandleApprovalCommand_ApproveSucceedsRegardlessOfCheckState(t *testing.T) {
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	r.TriggerSource = run.TriggerGitHubIssue
	triggerRef := "issue:9"
	r.TriggerRef = &triggerRef
	r.InstallationID = ptrInt64(99)
	r.Repo = "x/y"

	stage := rr.seedStage(r.ID, 0, run.StageStateAwaitingApproval)
	// Plan stage — review-stage slash approvals are refused per
	// ADR-018 / #313. This test pins the ADR-017 "approve despite
	// failing check" contract for the plan-stage path; the
	// review-stage equivalent moved to GitHub's PR surface.
	stage.Type = run.StageTypePlan
	stage.Gate = &run.Gate{
		Kind: run.GateKindApproval,
	}

	ar := newFakeApprovalRepo()
	au := newAuditCompleteAuditFake()
	gh := newSlashGitHubRecorder()
	scs := newFakeStageCheckRepo()
	// A failing observed check used to refuse the slash approval; it
	// no longer does. Seeded to prove the dropped gate.
	scs.seed(stage.ID, "ci_pass", stagecheck.StateFail)
	o := &orchestrator.Orchestrator{Runs: rr}

	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr, ApprovalRepo: ar, AuditRepo: au,
		StageCheckRepo: scs, Orchestrator: o,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	s.issueNotifier = issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: rr, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	if err := s.HandleApprovalCommand(context.Background(), webhook.ApprovalCommandParams{
		Repo: "x/y", IssueNumber: 9, InstallationID: 99, SenderLogin: "alice",
		Decision: webhook.MatchActionApprove,
	}); err != nil {
		t.Fatal(err)
	}
	if stage.State != run.StageStateSucceeded {
		t.Errorf("stage should advance to succeeded; got %q", stage.State)
	}
	if len(ar.all) != 1 {
		t.Errorf("approval should be recorded; got %+v", ar.all)
	}
	body := gh.calls()[0].body
	if strings.Contains(body, "blocking checks have not passed") {
		t.Errorf("reply should not reference dropped gate wording: %q", body)
	}
}

func TestHandleApprovalCommand_RepeatAfterAdvance_RepliesNoAwaitingStage(t *testing.T) {
	// Once an approve transitions the stage to succeeded, a second
	// approve from the same reviewer on the same issue can't find
	// an awaiting-approval stage — the run has moved on. The reply
	// should explain that, not falsely claim success.
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	r.TriggerSource = run.TriggerGitHubIssue
	triggerRef := "issue:42"
	r.TriggerRef = &triggerRef
	r.InstallationID = ptrInt64(99)
	r.Repo = "x/y"

	rr.seedStage(r.ID, 0, run.StageStateAwaitingApproval)

	ar := newFakeApprovalRepo()
	au := newAuditCompleteAuditFake()
	gh := newSlashGitHubRecorder()
	o := &orchestrator.Orchestrator{Runs: rr}

	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr, ApprovalRepo: ar, AuditRepo: au,
		Orchestrator: o, ExternalURL: "https://app.fishhawk.example.com",
	})
	s.issueNotifier = issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: rr, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	params := webhook.ApprovalCommandParams{
		Repo: "x/y", IssueNumber: 42, InstallationID: 99, SenderLogin: "alice",
		Decision: webhook.MatchActionApprove,
	}
	if err := s.HandleApprovalCommand(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	if err := s.HandleApprovalCommand(context.Background(), params); err != nil {
		t.Fatal(err)
	}

	if len(ar.all) != 1 {
		t.Errorf("approval repo should have one row; got %d", len(ar.all))
	}
	// Two comments expected: the first attempt's plan-approved
	// broadcast (#274), plus the second attempt's "no awaiting
	// stage" slash reply. The slash reply on the plan-approve path
	// is suppressed in favor of the broadcast (#304), so the first
	// attempt posts only one comment.
	calls := gh.calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 comments (1st: broadcast, 2nd: reply); got %d", len(calls))
	}
	// Order-independent body scan — the second-attempt's reply
	// must explain that no awaiting stage exists.
	var noAwaitingReply string
	for _, c := range calls {
		if strings.Contains(c.body, "No stage on this issue's run is awaiting approval") {
			noAwaitingReply = c.body
		}
	}
	if noAwaitingReply == "" {
		t.Errorf("expected one comment explaining 'no awaiting stage'; got bodies %v",
			func() []string {
				out := []string{}
				for _, c := range calls {
					out = append(out, c.body)
				}
				return out
			}())
	}
}

func TestHandleApprovalCommand_NoNotifier_NoOpReply(t *testing.T) {
	// Without an issueNotifier wired, the slash-command handler
	// short-circuits at approvalCommandConfigured() and logs.
	// No reply, no approval written.
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	r.TriggerSource = run.TriggerGitHubIssue
	triggerRef := "issue:42"
	r.TriggerRef = &triggerRef
	r.InstallationID = ptrInt64(99)
	r.Repo = "x/y"

	stage := rr.seedStage(r.ID, 0, run.StageStateAwaitingApproval)
	_ = stage
	ar := newFakeApprovalRepo()
	au := newAuditCompleteAuditFake()

	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr, ApprovalRepo: ar, AuditRepo: au,
		// No ExternalURL → no issueNotifier.
	})
	if err := s.HandleApprovalCommand(context.Background(), webhook.ApprovalCommandParams{
		Repo: "x/y", IssueNumber: 42, InstallationID: 99, SenderLogin: "alice",
		Decision: webhook.MatchActionApprove,
	}); err != nil {
		t.Fatal(err)
	}
	if len(ar.all) != 0 {
		t.Errorf("approval should not be recorded without notifier; got %+v", ar.all)
	}
}

func TestHandleApprovalCommand_RepoLookupFailure_RepliesGracefully(t *testing.T) {
	rr := &orchestratorRepoFailingList{listErr: errors.New("db down")}
	ar := newFakeApprovalRepo()
	au := newAuditCompleteAuditFake()
	gh := newSlashGitHubRecorder()

	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr, ApprovalRepo: ar, AuditRepo: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	s.issueNotifier = issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: rr, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	if err := s.HandleApprovalCommand(context.Background(), webhook.ApprovalCommandParams{
		Repo: "x/y", IssueNumber: 42, InstallationID: 99, SenderLogin: "alice",
		Decision: webhook.MatchActionApprove,
	}); err != nil {
		t.Fatal(err)
	}
	if got := gh.calls(); len(got) != 1 || !strings.Contains(got[0].body, "Could not look up the run") {
		t.Errorf("expected look-up-failed reply; got %+v", got)
	}
}

// --- helpers ---

type slashCommentCall struct {
	repo        string
	issueNumber int
	body        string
}

type slashGitHubRecorder struct {
	mu     sync.Mutex
	stored []slashCommentCall
}

func newSlashGitHubRecorder() *slashGitHubRecorder { return &slashGitHubRecorder{} }

func (g *slashGitHubRecorder) calls() []slashCommentCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]slashCommentCall, len(g.stored))
	copy(out, g.stored)
	return out
}

// CreateIssueComment satisfies issuecomment.IssueCommenter.
func (g *slashGitHubRecorder) CreateIssueComment(_ context.Context, _ int64, repo githubclient.RepoRef, issueNumber int, body string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stored = append(g.stored, slashCommentCall{repo: repo.String(), issueNumber: issueNumber, body: body})
	return nil
}

// orchestratorRepoFailingList wraps orchestratorRepo to inject a
// failing ListRuns. The other methods come through the embedded
// pointer; the test only exercises the look-up path so anything
// past the ListRuns failure is unreachable.
type orchestratorRepoFailingList struct {
	*orchestratorRepo
	listErr error
}

func (r *orchestratorRepoFailingList) ListRuns(_ context.Context, _ run.ListRunsFilter) ([]*run.Run, error) {
	return nil, r.listErr
}
