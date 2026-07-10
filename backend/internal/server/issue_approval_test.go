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
	"github.com/kuhlman-labs/fishhawk/backend/internal/role"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// Tests the slash-command approval flow end-to-end through the
// Server.HandleApprovalCommand path (#238). Uses real-ish fakes
// for the run, approval, audit, stagecheck, and orchestrator
// repos; swaps in a recording GitHub commenter so reply text can
// be asserted on.

func TestHandleApprovalCommand_PlanApprove_PostsSlashReplyAndStatus(t *testing.T) {
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

	// Two comments expected: the per-sender slash reply
	// (#377 unsuppressed it once the redundant broadcast went away)
	// plus the sticky-status seed (E20.4 / #330). The slash reply
	// names the approver + the stage + the run id so the actor
	// gets a typed-command confirmation; the status comment carries
	// the live run state for everyone else watching.
	got := gh.calls()
	if len(got) != 2 {
		t.Fatalf("expected 2 comments (slash reply + status); got %d bodies %v", len(got), commentBodies(got))
	}
	var reply, status string
	for _, c := range got {
		if strings.HasPrefix(c.body, "Approved by ") {
			reply = c.body
		} else if strings.Contains(c.body, "Fishhawk run") {
			status = c.body
		}
	}
	if reply == "" {
		t.Fatalf("expected a slash success reply; got bodies %v", commentBodies(got))
	}
	if status == "" {
		t.Fatalf("expected a sticky-status comment; got bodies %v", commentBodies(got))
	}
	if !strings.Contains(reply, "@alice") {
		t.Errorf("slash reply should mention approver: %q", reply)
	}
	// Regression guard for #305: the @login must not be backticked.
	if strings.Contains(reply, "`@") {
		t.Errorf("slash reply must not backtick-wrap the @mention: %q", reply)
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
	// Two comments expected: the slash reply on plan-reject (the
	// only path that doesn't suppress it in favor of a broadcast)
	// and the sticky-status update (E20.4 / #330).
	got := gh.calls()
	var reply, status string
	for _, c := range got {
		switch {
		case strings.HasPrefix(c.body, "Rejected by "):
			reply = c.body
		case strings.Contains(c.body, "Fishhawk run"):
			status = c.body
		}
	}
	if reply == "" {
		t.Fatalf("expected a 'Rejected by ' slash reply; got bodies %v", commentBodies(got))
	}
	if status == "" {
		t.Fatalf("expected a sticky-status comment; got bodies %v", commentBodies(got))
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
	// Three comments expected: the first attempt's plan-approved
	// broadcast (#274), the first attempt's sticky-status seed
	// (E20.4 / #330), and the second attempt's "no awaiting stage"
	// slash reply. The slash reply on the plan-approve path is
	// suppressed in favor of the broadcast (#304), so the first
	// attempt posts the broadcast + status; the second never
	// reaches the status hook because it returns early on the
	// missing-awaiting-stage branch.
	calls := gh.calls()
	if len(calls) != 3 {
		t.Fatalf("expected 3 comments (broadcast + status seed + 2nd-attempt reply); got %d: %v", len(calls), commentBodies(calls))
	}
	var noAwaitingReply string
	for _, c := range calls {
		if strings.Contains(c.body, "No stage on this issue's run is awaiting approval") {
			noAwaitingReply = c.body
		}
	}
	if noAwaitingReply == "" {
		t.Errorf("expected one comment explaining 'no awaiting stage'; got bodies %v", commentBodies(calls))
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

// --- reply-comment approval (E17.3 / #338) ---

// TestHandleApprovalCommand_ReplyComment_NoAwaitingStage_SkipsSilently
// locks in the core E17.3 contract: when source is ReplyComment, an
// "issue has no awaiting-approval plan stage" condition does NOT
// produce an unsolicited reply. The operator's "+1" might have been
// agreeing with another comment unrelated to Fishhawk.
func TestHandleApprovalCommand_ReplyComment_NoAwaitingStage_SkipsSilently(t *testing.T) {
	rr := newOrchestratorRepo()
	// No run for this issue — findAwaitingApprovalStage returns
	// found=false.
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
		Repo:           "x/y",
		IssueNumber:    42,
		InstallationID: 99,
		SenderLogin:    "alice",
		Decision:       webhook.MatchActionApprove,
		Source:         webhook.ApprovalSourceReplyComment,
	}); err != nil {
		t.Fatal(err)
	}
	if got := gh.calls(); len(got) != 0 {
		t.Errorf("reply-comment with no awaiting stage should produce no reply; got %d: %v",
			len(got), commentBodies(got))
	}
	if len(ar.all) != 0 {
		t.Errorf("no approval row should be written; got %+v", ar.all)
	}
}

// TestHandleApprovalCommand_ReplyComment_ReviewStage_SkipsSilently
// covers the second silent-skip branch: when the awaiting stage is
// a review (not plan), the reply-comment path skips rather than
// posting the review-stage help reply. ADR-018 owns the review
// approval surface; a generic "+1" on the issue thread isn't
// scoped to that.
func TestHandleApprovalCommand_ReplyComment_ReviewStage_SkipsSilently(t *testing.T) {
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
		Source:   webhook.ApprovalSourceReplyComment,
	}); err != nil {
		t.Fatal(err)
	}
	if got := gh.calls(); len(got) != 0 {
		t.Errorf("reply-comment on review stage should produce no reply; got %d: %v",
			len(got), commentBodies(got))
	}
	if len(ar.all) != 0 {
		t.Errorf("no approval row should be written; got %+v", ar.all)
	}
}

// TestHandleApprovalCommand_SlashCommand_NoAwaitingStage_StillReplies
// is the inverse regression: the silent-skip behavior is gated on
// Source=ReplyComment, NOT a behavior change for the slash path.
// Slash commands keep their explicit help replies.
func TestHandleApprovalCommand_SlashCommand_NoAwaitingStage_StillReplies(t *testing.T) {
	rr := newOrchestratorRepo()
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
		Source:   webhook.ApprovalSourceSlash, // explicit slash → loud reply
	}); err != nil {
		t.Fatal(err)
	}
	got := gh.calls()
	if len(got) != 1 {
		t.Fatalf("slash path should still post an explicit reply; got %d", len(got))
	}
	if !strings.Contains(got[0].body, "No stage on this issue's run is awaiting approval") {
		t.Errorf("reply should explain no-awaiting-stage: %q", got[0].body)
	}
}

// TestHandleApprovalCommand_ReplyComment_HappyPath covers the
// E17.4 / #339 happy path end-to-end: an authorized reviewer types
// "+1" → the matcher (E17.3) routes Source=reply_comment →
// HandleApprovalCommand writes the approval with
// SurfaceGitHubReplyComment, advances the stage, and posts NO
// inline slash reply because the typed `+1` is the user's
// confirmation. After #377, no plan-approved broadcast is posted
// either — the plan-comment edit (when a plan comment exists)
// carries the new approval state in its footer; the sticky-status
// comment surfaces it in the activity feed. In this fixture no
// plan artifact is seeded, so the plan-comment refire silent-skips
// and only the sticky-status seed comment fires.
func TestHandleApprovalCommand_ReplyComment_HappyPath(t *testing.T) {
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
		GitHub: gh, Runs: rr, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	if err := s.HandleApprovalCommand(context.Background(), webhook.ApprovalCommandParams{
		Repo: "x/y", IssueNumber: 42, InstallationID: 99, SenderLogin: "alice",
		Decision: webhook.MatchActionApprove,
		Source:   webhook.ApprovalSourceReplyComment,
	}); err != nil {
		t.Fatal(err)
	}

	// Stage transitioned.
	if stage.State != run.StageStateSucceeded {
		t.Errorf("stage state = %q, want succeeded", stage.State)
	}

	// Approval row carries the new Surface value.
	if len(ar.all) != 1 {
		t.Fatalf("expected 1 approval row; got %d", len(ar.all))
	}
	got := ar.all[0]
	if got.Surface != approval.SurfaceGitHubReplyComment {
		t.Errorf("Surface = %q, want %q", got.Surface, approval.SurfaceGitHubReplyComment)
	}
	if got.ApproverSubject != "alice" {
		t.Errorf("approver = %q, want alice", got.ApproverSubject)
	}
	if got.Decision != approval.DecisionApprove {
		t.Errorf("decision = %q", got.Decision)
	}

	// Only the sticky-status seed fires here: the per-call slash
	// reply is suppressed on the reply path (would echo "Approved"
	// back at a "+1"), the plan-approved broadcast was retired
	// (#377), and no plan artifact is seeded so the plan-comment
	// refire silent-skips.
	calls := gh.calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 comment (sticky status only); got %d bodies %v", len(calls), commentBodies(calls))
	}
	if !strings.Contains(calls[0].body, "Fishhawk run") {
		t.Errorf("expected sticky-status comment; got %q", calls[0].body)
	}
	for _, c := range calls {
		if strings.HasPrefix(c.body, "Approved by ") {
			t.Errorf("reply-comment path should not echo a per-call success reply: %q", c.body)
		}
	}
}

// TestHandleApprovalCommand_ReplyComment_NonApprover_SkipsSilently
// finishes the silent-skip contract from E17.3: a "+1" from a user
// not in the gate's approver list is dropped without a reply.
func TestHandleApprovalCommand_ReplyComment_NonApprover_SkipsSilently(t *testing.T) {
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	r.TriggerSource = run.TriggerGitHubIssue
	triggerRef := "issue:42"
	r.TriggerRef = &triggerRef
	r.InstallationID = ptrInt64(99)
	r.Repo = "x/y"
	r.WorkflowID = "feature_change"
	// Workflow spec restricts plan-stage approvers to the tech_lead
	// role; the stub team lister returns no members for that team,
	// so any subject (including "passerby") fails the role check.
	r.WorkflowSpec = []byte(`version: "0.3"
roles:
  tech_lead:
    members: ["@org/tech-leads"]
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        inputs:
          - source: github_issue
            required: true
        gates:
          - type: approval
            approvers:
              any_of: [tech_lead]
      - id: implement
        type: implement
        executor:
          agent: claude-code
`)

	stage := rr.seedStage(r.ID, 0, run.StageStateAwaitingApproval)
	stage.Type = run.StageTypePlan

	ar := newFakeApprovalRepo()
	au := newAuditCompleteAuditFake()
	gh := newSlashGitHubRecorder()
	resolver := role.NewResolver(&stubTeamLister{teamMembers: nil})

	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr, ApprovalRepo: ar, AuditRepo: au,
		RoleResolver: resolver,
		ExternalURL:  "https://app.fishhawk.example.com",
	})
	s.issueNotifier = issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: rr, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	if err := s.HandleApprovalCommand(context.Background(), webhook.ApprovalCommandParams{
		Repo: "x/y", IssueNumber: 42, InstallationID: 99,
		SenderLogin: "passerby", // not in any role
		Decision:    webhook.MatchActionApprove,
		Source:      webhook.ApprovalSourceReplyComment,
	}); err != nil {
		t.Fatal(err)
	}

	if len(ar.all) != 0 {
		t.Errorf("no approval row should be written for non-approver; got %+v", ar.all)
	}
	if stage.State != run.StageStateAwaitingApproval {
		t.Errorf("stage state advanced unexpectedly: %q", stage.State)
	}
	if calls := gh.calls(); len(calls) != 0 {
		t.Errorf("non-approver reply should not produce any GitHub comment; got %d: %v",
			len(calls), commentBodies(calls))
	}
}

// TestHandleApprovalCommand_ReplyComment_DuplicateIdempotent locks in
// the idempotency guarantee. A second "+1" from the same reviewer
// finds the existing approval row (dedup on (stage_id, subject)) and
// silently no-ops on the reply path.
func TestHandleApprovalCommand_ReplyComment_DuplicateIdempotent(t *testing.T) {
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
		Source:   webhook.ApprovalSourceReplyComment,
	}
	// Two "+1" replies from alice — the second is a re-fire after
	// the first stage transition has already settled.
	if err := s.HandleApprovalCommand(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	if err := s.HandleApprovalCommand(context.Background(), params); err != nil {
		t.Fatal(err)
	}

	if len(ar.all) != 1 {
		t.Errorf("approval row should be deduped to 1; got %d", len(ar.all))
	}
	// Body scan: only the first attempt's broadcast + sticky status
	// fire. The second attempt finds no awaiting stage (it
	// transitioned on the first call) and silent-skips.
	calls := gh.calls()
	for _, c := range calls {
		if strings.HasPrefix(c.body, "Approved by ") {
			t.Errorf("reply-comment duplicate must not echo a success reply: %q", c.body)
		}
		if strings.Contains(c.body, "already submitted") {
			t.Errorf("reply-comment duplicate must not surface the 'prior decision' reply: %q", c.body)
		}
	}
}

// --- helpers ---

// splitBroadcastAndStatus picks the plan-approved broadcast (the
// "Plan approved by" comment) and the sticky-status comment (the
// "Fishhawk run" header) out of a recorded call list. Returns the
// matched bodies, or empty strings when not found.
func splitBroadcastAndStatus(calls []slashCommentCall) (broadcast, status string) {
	for _, c := range calls {
		switch {
		case strings.Contains(c.body, "Plan approved by"):
			broadcast = c.body
		case strings.Contains(c.body, "Fishhawk run"):
			status = c.body
		}
	}
	return broadcast, status
}

// commentBodies returns just the body slice for diagnostics in
// failed assertions.
func commentBodies(calls []slashCommentCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.body)
	}
	return out
}

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
func (g *slashGitHubRecorder) CreateIssueComment(_ context.Context, _ int64, repo githubclient.RepoRef, issueNumber int, body string) (*githubclient.IssueComment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stored = append(g.stored, slashCommentCall{repo: repo.String(), issueNumber: issueNumber, body: body})
	return &githubclient.IssueComment{ID: int64(len(g.stored)), Body: body}, nil
}

// UpdateIssueComment satisfies the IssueCommenter interface
// extended in #328. Slash-approval tests don't exercise the
// update path; returning a happy response keeps the interface
// satisfied.
func (g *slashGitHubRecorder) UpdateIssueComment(_ context.Context, _ int64, _ githubclient.RepoRef, commentID int64, body string) (*githubclient.IssueComment, error) {
	return &githubclient.IssueComment{ID: commentID, Body: body}, nil
}

// CreateReview is a no-op stub for the IssueCommenter interface extension
// landed in #1785 (advisory COMMENT-type PR reviews). Slash-approval tests
// don't exercise the PR-review path; a happy response keeps the interface
// satisfied.
func (g *slashGitHubRecorder) CreateReview(_ context.Context, _ int64, _ githubclient.RepoRef, _ int, _ githubclient.CreateReviewParams) (*githubclient.CreateReviewResult, error) {
	return &githubclient.CreateReviewResult{}, nil
}

func (g *slashGitHubRecorder) ListIssueComments(_ context.Context, _ int64, _ githubclient.RepoRef, _ int) ([]githubclient.FetchedIssueComment, error) {
	return nil, nil
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
