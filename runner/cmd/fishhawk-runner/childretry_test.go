package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// TestChildRetryLifecycle_PushFailureRetryRedispatch_NeverOpensPR is the
// runner half of the E55.16 / #3916 DONE-MEANS: a decomposition child whose
// push transport fails is retried and re-dispatched by run_children, and no
// pull request is ever opened for its slice branch.
//
// PASS 1 is the first dispatch's tail: the verified commit sits on HEAD, the
// push fails, the push-kind checkpoint arms on the SLICE branch and the
// failure is reported exactly as run() reports it. PASS 2 is the retry's
// re-dispatch: run() with the argv run_children spawns, fed the prompt the
// backend serves for pass 1's REPORTED coordinates (not fixture constants).
// It must take the held-commit short-circuit (agent never invoked), publish
// the slice branch at the held head, report `pushed` once, and never call the
// PR opener in either pass. The backend half is
// backend/internal/server's TestChildPushFailure_RetryRedispatch_NeverOpensPR.
func TestChildRetryLifecycle_PushFailureRetryRedispatch_NeverOpensPR(t *testing.T) {
	repo, _, heldBranch, headSHA, treeSHA := pushResumeRepo(t)
	cprGit(t, repo, "checkout", heldBranch) // CommitAndPush leaves the commit on HEAD when only the push fails
	origSlice := runSliceIndex
	runSliceIndex = 0
	t.Cleanup(func() { runSliceIndex = origSlice })
	sliceBranch := childSliceBranch(childResumeParentRunID, 0)

	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, invoker)
	fp := withFakePusher(t)
	fp.err = fmt.Errorf("gitops: push: %w", gitops.ErrPushFailed)
	fpr := withFakePROpenerOnly(t)

	// PASS 1 — the push-transport failure, reported as run() reports it.
	fu1 := newFakeUploader(t)
	issued, err := fu1.IssueKey(context.Background(), verifiedTreeRunID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cfg := childResumeCfg(repo)
	cp := pushCheckpoint{backendSupportsPushResume: true}
	var log1 strings.Builder
	err = openPRAndShipArtifact(context.Background(), cfg, &log1, fu1, issued, "", false, false, nil, false, treeSHA, "", nil, nil, nil, &cp)
	if !errors.Is(err, gitops.ErrPushFailed) {
		t.Fatalf("pass 1: err = %v, want ErrPushFailed\n%s", err, log1.String())
	}
	if !cp.armed || cp.resumeKind != resumeKindPush || cp.branch != sliceBranch || cp.headSHA != headSHA {
		t.Fatalf("pass 1: checkpoint = %+v, want an armed push kind on %s at %s\n%s", cp, sliceBranch, headSHA, log1.String())
	}
	if rerr := reportPullRequestFailure(context.Background(), cfg, &log1, fu1, issued, "C", err.Error(), &cp); rerr != nil {
		t.Fatalf("pass 1: reportPullRequestFailure: %v", rerr)
	}
	reported := reportedFailureBody(t, fu1)
	if reported.Outcome != "failed" || reported.ResumeKind != resumeKindPush || reported.Branch != sliceBranch || reported.HeadSHA != headSHA {
		t.Fatalf("pass 1: reported %+v, want a failed push-kind report on %s at %s", reported, sliceBranch, headSHA)
	}
	if fpr.gotArgs != nil {
		t.Fatalf("pass 1: a child must never open a PR, got %+v", fpr.gotArgs)
	}

	// PASS 2 — the retry's re-dispatch through run().
	fu2 := newFakeUploader(t)
	fu2.promptResp = &upload.FetchedPrompt{
		StageID:                   verifiedTreeStageID,
		StageType:                 "implement",
		Prompt:                    "implement",
		PromptHash:                "h",
		DecomposedFromRunID:       childResumeParentRunID,
		SliceIndex:                0,
		SupportsPushResume:        true,
		OpenPRFromHeldCommit:      true,
		HeldCommitSHA:             reported.HeadSHA,
		HeldCommitBranch:          reported.Branch,
		HeldCommitBaseSHA:         reported.BaseSHA,
		HeldCommitVerifiedTreeSHA: reported.TreeSHA,
		HeldCommitResumeKind:      reported.ResumeKind,
	}
	withFakeUploader(t, fu2)
	implementEnv(t, "test-owner/test-repo", "main")
	var log2 strings.Builder
	code := run([]string{
		"--run-id", verifiedTreeRunID,
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", verifiedTreeStageID,
		"--working-dir", repo,
		"--github-repo", "test-owner/test-repo",
		"--base-branch", "main",
		"--fetch-prompt", "--upload-trace",
	}, &log2)
	if code != exitOK {
		t.Fatalf("pass 2: run = %d, want exitOK\n%s", code, log2.String())
	}
	if invoker.callIdx != 0 {
		t.Errorf("pass 2: agent invoked %d time(s), want 0 (the held-commit short-circuit)", invoker.callIdx)
	}
	if fpr.gotArgs != nil {
		t.Errorf("a decomposition child must NEVER open a pull request, got head %q base %q", fpr.gotArgs.Head, fpr.gotArgs.Base)
	}
	if fp.pushCommittedArgs == nil || fp.pushCommittedArgs.Branch != sliceBranch || fp.pushCommittedArgs.HeadSHA != headSHA {
		t.Fatalf("pass 2: published %+v, want %s on %s", fp.pushCommittedArgs, headSHA, sliceBranch)
	}
	outcomes := make([]string, 0, len(fu2.gotPRCalls))
	for _, c := range fu2.gotPRCalls {
		outcome := c.Outcome
		if outcome == "" {
			outcome = "<success artifact>"
		}
		outcomes = append(outcomes, outcome)
	}
	if len(outcomes) != 1 || outcomes[0] != "pushed" {
		t.Fatalf("pass 2: pull-request reports = %v, want exactly [pushed]", outcomes)
	}
	if !strings.Contains(log2.String(), `"event":"push_resume_child_pushed"`) {
		t.Errorf("pass 2: missing push_resume_child_pushed:\n%s", log2.String())
	}
	for _, ev := range []string{`"event":"pr_open_resume_pr_opened"`, `"event":"pull_request_opened"`, `"event":"pull_request_uploaded"`} {
		if strings.Contains(log2.String(), ev) {
			t.Errorf("pass 2: a child resume must log no PR-open event, got %s:\n%s", ev, log2.String())
		}
	}
}
