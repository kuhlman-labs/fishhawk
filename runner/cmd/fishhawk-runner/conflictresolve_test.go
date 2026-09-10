package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/conflictresolve"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// --- fixtures ---

func crGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func crGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

func crWrite(t *testing.T, dir, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// crRepo builds a real repository with a `main` base branch and a `feature`
// run branch whose edits to conflict.txt CONFLICT with main's. It leaves
// `feature` checked out and returns (repoDir, featureTipSHA).
//
// The repository is REAL: every assertion below reads git's own state back
// rather than a model of it, because the controls under test have their effect
// on committed repository state, not on a returned error.
func crRepo(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	crGit(t, repo, "init", "--initial-branch=main")
	crGit(t, repo, "config", "user.name", "test")
	crGit(t, repo, "config", "user.email", "test@example.com")
	crWrite(t, repo, "conflict.txt", "base\n")
	crWrite(t, repo, "quiet.txt", "untouched\n")
	crGit(t, repo, "add", "-A")
	crGit(t, repo, "commit", "-m", "initial")

	crGit(t, repo, "checkout", "-b", "feature")
	crWrite(t, repo, "conflict.txt", "ours\n")
	crGit(t, repo, "commit", "-am", "ours")

	crGit(t, repo, "checkout", "main")
	crWrite(t, repo, "conflict.txt", "theirs\n")
	crWrite(t, repo, "cleanly-advanced.txt", "new on base\n")
	crGit(t, repo, "add", "-A")
	crGit(t, repo, "commit", "-m", "theirs")

	crGit(t, repo, "checkout", "feature")
	return repo, crGitOut(t, repo, "rev-parse", "HEAD")
}

// crRequest builds the trigger for a repo whose base branch is local `main`.
func crRequest(head string) conflictResolutionRequest {
	return conflictResolutionRequest{Branch: "feature", BaseRef: "refs/heads/main", ExpectedHeadSHA: head}
}

// noAgent is the stub that changes nothing.
func noAgent(context.Context) error { return nil }

// crAssertRestored reads the REPOSITORY back and asserts the pre-merge tip and
// a clean worktree. This is the assertion that actually discriminates: every
// refusal returns a byte-identical-shaped error whether or not the recovery
// ran, so asserting error identity alone would stay green with recovery gone.
func crAssertRestored(t *testing.T, repo, preTip string) {
	t.Helper()
	if got := crGitOut(t, repo, "rev-parse", "HEAD"); got != preTip {
		t.Errorf("HEAD = %s, want pre-merge tip %s", got, preTip)
	}
	if st := crGitOut(t, repo, "status", "--porcelain"); st != "" {
		t.Errorf("worktree not clean after refusal:\n%s", st)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "MERGE_HEAD")); !os.IsNotExist(err) {
		t.Errorf(".git/MERGE_HEAD still present after refusal (err=%v)", err)
	}
}

// --- qualifyMergeRef ---

// TestQualifyMergeRef is the round-1 naming-bug regression. The naive
// "contains a slash → already qualified" rule left `release/1.2` unqualified,
// the merge failed to resolve it, and the ceiling-of-one budget burned on a
// naming bug rather than on a real conflict.
func TestQualifyMergeRef(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	crGit(t, repo, "init", "--initial-branch=main")
	crGit(t, repo, "remote", "add", "origin", repo)

	cases := []struct{ name, in, want string }{
		{"plain_branch", "main", "origin/main"},
		{"slash_bearing_branch", "release/1.2", "origin/release/1.2"},
		{"already_remote_qualified", "origin/main", "origin/main"},
		{"explicit_ref_path", "refs/heads/main", "refs/heads/main"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := qualifyMergeRef(context.Background(), repo, "origin", tc.in); got != tc.want {
				t.Errorf("qualifyMergeRef(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// --- conflictResolutionFromPrompt ---

// TestConflictResolutionFromPrompt covers the half-populated refusal: a serve
// missing ANY anchor must yield NO pass, because a runner handed an empty base
// ref merges nothing and then refuses naming the wrong cause.
func TestConflictResolutionFromPrompt(t *testing.T) {
	full := &upload.FetchedPrompt{
		ConflictResolution:                true,
		ConflictResolutionBranch:          "feature",
		ConflictResolutionBaseRef:         "main",
		ConflictResolutionExpectedHeadSHA: "abc",
	}
	if got := conflictResolutionFromPrompt(full); got == nil {
		t.Fatal("fully populated instruction yielded no pass")
	} else if got.Branch != "feature" || got.BaseRef != "main" || got.ExpectedHeadSHA != "abc" {
		t.Errorf("request = %+v, want the served anchors", got)
	}

	cases := []struct {
		name string
		mut  func(*upload.FetchedPrompt)
	}{
		{"nil_prompt", nil},
		{"flag_unset", func(p *upload.FetchedPrompt) { p.ConflictResolution = false }},
		{"no_branch", func(p *upload.FetchedPrompt) { p.ConflictResolutionBranch = "" }},
		{"no_base_ref", func(p *upload.FetchedPrompt) { p.ConflictResolutionBaseRef = "" }},
		{"no_expected_head", func(p *upload.FetchedPrompt) { p.ConflictResolutionExpectedHeadSHA = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mut == nil {
				if conflictResolutionFromPrompt(nil) != nil {
					t.Fatal("nil prompt yielded a pass")
				}
				return
			}
			p := *full
			tc.mut(&p)
			if got := conflictResolutionFromPrompt(&p); got != nil {
				t.Fatalf("half-populated instruction yielded a pass: %+v", got)
			}
		})
	}
}

// --- the pass, driven end to end against real repositories ---

// TestConflictResolutionPass_AcceptsResolvedHunk is the success control: an
// agent that resolves the hunk reaches the scoped add + single commit, and the
// repository carries a real merge commit with BOTH parents.
func TestConflictResolutionPass_AcceptsResolvedHunk(t *testing.T) {
	repo, head := crRepo(t)
	mainTip := crGitOut(t, repo, "rev-parse", "main")

	res := runConflictResolutionPass(context.Background(), repo, "origin", crRequest(head),
		func(context.Context) error { crWrite(t, repo, "conflict.txt", "ours\n"); return nil }, nil)

	if res.refused() {
		t.Fatalf("pass refused: %s (%s)", res.Reason, res.Detail)
	}
	if res.BaseSHA != head {
		t.Errorf("BaseSHA = %s, want %s", res.BaseSHA, head)
	}
	if got := crGitOut(t, repo, "rev-parse", "HEAD"); got != res.HeadSHA {
		t.Errorf("HEAD = %s, want the reported merge commit %s", got, res.HeadSHA)
	}
	parents := strings.Fields(crGitOut(t, repo, "rev-list", "--parents", "-n", "1", "HEAD"))
	if len(parents) != 3 || parents[1] != head || parents[2] != mainTip {
		t.Errorf("merge parents = %v, want [%s %s]", parents[1:], head, mainTip)
	}
	// The clean base change git auto-staged is AUTHORIZED and must have landed.
	if _, err := os.Stat(filepath.Join(repo, "cleanly-advanced.txt")); err != nil {
		t.Errorf("clean base change missing from the merge commit: %v", err)
	}
}

// TestConflictResolutionPass_Refusals drives ONE case per named refusal end to
// end. Every case asserts BOTH the named reason AND — by reading the repository
// back — that HEAD is the pre-merge tip with a clean worktree.
func TestConflictResolutionPass_Refusals(t *testing.T) {
	cases := []struct {
		name   string
		req    func(head string) conflictResolutionRequest
		agent  func(t *testing.T, repo string) func(context.Context) error
		reason string
	}{
		{
			name: "unexpected_head",
			req: func(string) conflictResolutionRequest {
				return conflictResolutionRequest{Branch: "feature", BaseRef: "refs/heads/main", ExpectedHeadSHA: strings.Repeat("d", 40)}
			},
			reason: reasonUnexpectedHead,
		},
		{
			name: "no_conflict",
			req: func(head string) conflictResolutionRequest {
				// Merge a ref that does NOT conflict: the base advanced past it.
				return conflictResolutionRequest{Branch: "feature", BaseRef: "refs/heads/clean", ExpectedHeadSHA: head}
			},
			reason: reasonNoConflict,
		},
		{
			name: "merge_failed",
			req: func(head string) conflictResolutionRequest {
				return conflictResolutionRequest{Branch: "feature", BaseRef: "refs/heads/does-not-exist", ExpectedHeadSHA: head}
			},
			reason: reasonMergeFailed,
		},
		{
			name: "agent_failed",
			req:  crRequest,
			agent: func(*testing.T, string) func(context.Context) error {
				return func(context.Context) error { return errors.New("boom") }
			},
			reason: reasonAgentFailed,
		},
		{
			name: "edit_outside_hunk",
			req:  crRequest,
			agent: func(t *testing.T, repo string) func(context.Context) error {
				return func(context.Context) error {
					crWrite(t, repo, "conflict.txt", "ours\n")
					crWrite(t, repo, "quiet.txt", "tampered\n")
					return nil
				}
			},
			reason: "conflict_resolution_unstaged_change_outside_set",
		},
		{
			name: "untracked_path_outside_set",
			req:  crRequest,
			agent: func(t *testing.T, repo string) func(context.Context) error {
				return func(context.Context) error {
					crWrite(t, repo, "conflict.txt", "ours\n")
					crWrite(t, repo, "smuggled.txt", "new\n")
					return nil
				}
			},
			reason: "conflict_resolution_untracked_path_outside_set",
		},
		{
			name: "agent_staged_the_path",
			req:  crRequest,
			agent: func(t *testing.T, repo string) func(context.Context) error {
				return func(context.Context) error {
					crWrite(t, repo, "conflict.txt", "ours\n")
					crGit(t, repo, "add", "conflict.txt")
					return nil
				}
			},
			reason: "conflict_resolution_conflicted_path_not_unmerged",
		},
		{
			name: "agent_committed",
			req:  crRequest,
			agent: func(t *testing.T, repo string) func(context.Context) error {
				return func(context.Context) error {
					crWrite(t, repo, "conflict.txt", "ours\n")
					crGit(t, repo, "add", "conflict.txt")
					crGit(t, repo, "commit", "--no-edit")
					return nil
				}
			},
			reason: "conflict_resolution_head_moved",
		},
		{
			name: "merge_head_rewritten",
			req:  crRequest,
			agent: func(t *testing.T, repo string) func(context.Context) error {
				return func(context.Context) error {
					crWrite(t, repo, "conflict.txt", "ours\n")
					crWrite(t, repo, ".git/MERGE_HEAD", strings.Repeat("a", 40)+"\n")
					return nil
				}
			},
			reason: "conflict_resolution_merge_head_changed",
		},
		{
			name: "merge_message_rewritten",
			req:  crRequest,
			agent: func(t *testing.T, repo string) func(context.Context) error {
				return func(context.Context) error {
					crWrite(t, repo, "conflict.txt", "ours\n")
					crWrite(t, repo, ".git/MERGE_MSG", "a message the operator never authorized\n")
					return nil
				}
			},
			reason: "conflict_resolution_merge_message_changed",
		},
		{
			name: "conflicted_mode_changed",
			req:  crRequest,
			agent: func(t *testing.T, repo string) func(context.Context) error {
				return func(context.Context) error {
					crWrite(t, repo, "conflict.txt", "ours\n")
					if err := os.Chmod(filepath.Join(repo, "conflict.txt"), 0o755); err != nil {
						t.Fatal(err)
					}
					return nil
				}
			},
			reason: "conflict_resolution_conflicted_mode_changed",
		},
		{
			name: "conflicted_path_deleted",
			req:  crRequest,
			agent: func(t *testing.T, repo string) func(context.Context) error {
				return func(context.Context) error {
					return os.Remove(filepath.Join(repo, "conflict.txt"))
				}
			},
			reason: "conflict_resolution_conflicted_path_missing",
		},
		{
			name:   "residual_marker",
			req:    crRequest,
			agent:  func(*testing.T, string) func(context.Context) error { return noAgent },
			reason: "conflict_resolution_residual_marker",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, head := crRepo(t)
			if tc.name == "no_conflict" {
				// A base branch that advanced WITHOUT touching conflict.txt.
				crGit(t, repo, "branch", "clean", "main~1")
				crGit(t, repo, "checkout", "clean")
				crWrite(t, repo, "elsewhere.txt", "clean advance\n")
				crGit(t, repo, "add", "-A")
				crGit(t, repo, "commit", "-m", "clean advance")
				crGit(t, repo, "checkout", "feature")
			}
			agent := noAgent
			if tc.agent != nil {
				agent = tc.agent(t, repo)
			}
			res := runConflictResolutionPass(context.Background(), repo, "origin", tc.req(head), agent, nil)

			if res.Reason != tc.reason {
				t.Fatalf("reason = %q (%s), want %q", res.Reason, res.Detail, tc.reason)
			}
			if !res.Recovered {
				t.Errorf("recovery not verified: %s", res.RecoveryDetail)
			}
			crAssertRestored(t, repo, head)
		})
	}
}

// TestConflictResolutionPass_BaselineCaptureFailure drives the
// baseline_capture_failed branch by removing .git/MERGE_HEAD between the merge
// and the capture — the one input the capture treats as mandatory.
func TestConflictResolutionPass_BaselineCaptureFailure(t *testing.T) {
	repo, head := crRepo(t)
	orig := conflictCaptureHook
	t.Cleanup(func() { conflictCaptureHook = orig })
	conflictCaptureHook = func(repoDir string) {
		_ = os.Remove(filepath.Join(repoDir, ".git", "MERGE_HEAD"))
	}
	res := runConflictResolutionPass(context.Background(), repo, "origin", crRequest(head), noAgent, nil)
	if res.Reason != reasonBaselineCaptureFailed {
		t.Fatalf("reason = %q (%s), want %q", res.Reason, res.Detail, reasonBaselineCaptureFailed)
	}
	crAssertRestored(t, repo, head)
}

// TestConflictResolutionPass_CommitFailure drives the commit_failed branch: the
// gate PASSES and the commit is then made to fail, proving the reason is not
// mis-attributed to the gate.
func TestConflictResolutionPass_CommitFailure(t *testing.T) {
	repo, head := crRepo(t)
	orig := conflictCommitHook
	t.Cleanup(func() { conflictCommitHook = orig })
	conflictCommitHook = func(repoDir string) {
		// An unreadable index makes `git add` fail while everything the gate
		// already decided stays true.
		_ = os.WriteFile(filepath.Join(repoDir, ".git", "index"), []byte("not an index"), 0o000)
	}
	res := runConflictResolutionPass(context.Background(), repo, "origin", crRequest(head),
		func(context.Context) error { crWrite(t, repo, "conflict.txt", "ours\n"); return nil }, nil)
	if res.Reason != reasonCommitFailed {
		t.Fatalf("reason = %q (%s), want %q", res.Reason, res.Detail, reasonCommitFailed)
	}
}

// TestConflictResolutionRecoveryOutlivesCancellation is the ROUND-2 recovery
// requirement. exec.CommandContext kills the child the instant the context is
// done, so a cancellation while the agent ran previously made the abort, the
// verification, the reset and the clean ALL fail instantly and left the
// repository mid-merge. The stub agent cancels the pass's OWN context, and the
// assertion reads the repository back.
func TestConflictResolutionRecoveryOutlivesCancellation(t *testing.T) {
	repo, head := crRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	res := runConflictResolutionPass(ctx, repo, "origin", crRequest(head), func(context.Context) error {
		crWrite(t, repo, "quiet.txt", "tampered mid-cancel\n")
		cancel()
		return context.Canceled
	}, nil)

	if !res.refused() {
		t.Fatal("cancelled pass was not refused")
	}
	if !res.Recovered {
		t.Errorf("recovery not verified after cancellation: %s", res.RecoveryDetail)
	}
	crAssertRestored(t, repo, head)
}

// --- stage-level publish + report ---

type crFakeUpload struct {
	uploadClient
	ships []upload.ShipPullRequestArgs
	err   error
}

func (f *crFakeUpload) ShipPullRequest(_ context.Context, args upload.ShipPullRequestArgs) (*upload.ShipPullRequestResult, error) {
	f.ships = append(f.ships, args)
	return &upload.ShipPullRequestResult{}, f.err
}

func (f *crFakeUpload) FetchInstallationToken(context.Context, upload.FetchInstallationTokenArgs) (*upload.FetchInstallationTokenResult, error) {
	return &upload.FetchInstallationTokenResult{Token: "tok"}, nil
}

func crStageCfg(repo string) config {
	return config{runID: "run-1", stageID: "stage-1", workingDir: repo, githubRepo: "acme/widgets"}
}

// TestConflictResolutionStage_PushesAndReports is the publish-and-report
// requirement: a passing gate must PUSH the merge commit through the authorized
// pusher seam and then report the TERMINAL outcome. Neither arm may leave the
// stage in `running`.
func TestConflictResolutionStage_PushesAndReports(t *testing.T) {
	repo, head := crRepo(t)
	fp := &fakePusher{}
	origPusher := newPusher
	newPusher = func() pusher { return fp }
	t.Cleanup(func() { newPusher = origPusher })

	origAgent := conflictResolutionAgentInvoker
	t.Cleanup(func() { conflictResolutionAgentInvoker = origAgent })
	conflictResolutionAgentInvoker = func(_ context.Context, cfg config, _ io.Writer) error {
		crWrite(t, cfg.workingDir, "conflict.txt", "ours\n")
		return nil
	}

	client := &crFakeUpload{}
	code := runConflictResolutionStage(context.Background(), crStageCfg(repo), crRequest(head),
		client, &upload.IssuedKey{PrivateKey: make([]byte, 64)}, io.Discard)

	if code != exitOK {
		t.Fatalf("exit code = %d, want %d", code, exitOK)
	}
	mergeSHA := crGitOut(t, repo, "rev-parse", "HEAD")
	if fp.pushCommittedArgs == nil {
		t.Fatal("the merge commit was never pushed")
	}
	if fp.pushCommittedArgs.HeadSHA != mergeSHA || fp.pushCommittedArgs.Branch != "feature" {
		t.Errorf("push args = %+v, want branch feature at %s", fp.pushCommittedArgs, mergeSHA)
	}
	if len(client.ships) != 1 {
		t.Fatalf("ships = %d, want exactly one terminal report", len(client.ships))
	}
	got := client.ships[0]
	if got.Outcome != "conflict_resolution_pushed" || got.HeadSHA != mergeSHA || got.BaseSHA != head {
		t.Errorf("report = %+v, want conflict_resolution_pushed at %s from %s", got, mergeSHA, head)
	}
}

// TestConflictResolutionStage_RefusalReportsNamedReason: a refused pass must
// still REPORT, carrying the named refusal reason, and must not push.
func TestConflictResolutionStage_RefusalReportsNamedReason(t *testing.T) {
	repo, head := crRepo(t)
	fp := &fakePusher{}
	origPusher := newPusher
	newPusher = func() pusher { return fp }
	t.Cleanup(func() { newPusher = origPusher })

	origAgent := conflictResolutionAgentInvoker
	conflictResolutionAgentInvoker = func(_ context.Context, cfg config, _ io.Writer) error {
		crWrite(t, cfg.workingDir, "quiet.txt", "tampered\n")
		crWrite(t, cfg.workingDir, "conflict.txt", "ours\n")
		return nil
	}
	t.Cleanup(func() { conflictResolutionAgentInvoker = origAgent })

	client := &crFakeUpload{}
	code := runConflictResolutionStage(context.Background(), crStageCfg(repo), crRequest(head),
		client, &upload.IssuedKey{PrivateKey: make([]byte, 64)}, io.Discard)

	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d", code, exitFailure)
	}
	if fp.pushCommittedArgs != nil {
		t.Fatal("a refused pass pushed anyway")
	}
	if len(client.ships) != 1 {
		t.Fatalf("ships = %d, want exactly one terminal failure report", len(client.ships))
	}
	got := client.ships[0]
	if got.Outcome != "failed" || got.Category != "B" {
		t.Errorf("report outcome/category = %q/%q, want failed/B", got.Outcome, got.Category)
	}
	if !strings.Contains(got.Reason, "conflict_resolution_unstaged_change_outside_set") {
		t.Errorf("reason = %q, want the NAMED refusal reason", got.Reason)
	}
	crAssertRestored(t, repo, head)
}

// TestConflictResolutionStage_PushFailureReportsCategoryC pins the push-failure
// branch: a transport failure is category C, not a judgment, and it still
// settles the stage.
func TestConflictResolutionStage_PushFailureReportsCategoryC(t *testing.T) {
	repo, head := crRepo(t)
	fp := &fakePusher{pushCommittedErr: errors.New("remote hung up")}
	origPusher := newPusher
	newPusher = func() pusher { return fp }
	t.Cleanup(func() { newPusher = origPusher })

	origAgent := conflictResolutionAgentInvoker
	conflictResolutionAgentInvoker = func(_ context.Context, cfg config, _ io.Writer) error {
		crWrite(t, cfg.workingDir, "conflict.txt", "ours\n")
		return nil
	}
	t.Cleanup(func() { conflictResolutionAgentInvoker = origAgent })

	client := &crFakeUpload{}
	if code := runConflictResolutionStage(context.Background(), crStageCfg(repo), crRequest(head),
		client, &upload.IssuedKey{PrivateKey: make([]byte, 64)}, io.Discard); code != exitFailure {
		t.Fatalf("exit code = %d, want %d", code, exitFailure)
	}
	if len(client.ships) != 1 || client.ships[0].Category != "C" {
		t.Fatalf("ships = %+v, want one category-C failure report", client.ships)
	}
	if !strings.Contains(client.ships[0].Reason, "conflict_resolution_push_failed") {
		t.Errorf("reason = %q, want the named push-failure reason", client.ships[0].Reason)
	}
}

// TestConflictResolutionStage_BadRepoSlugFailsClosed pins the owner/name guard:
// an unresolvable repo slug must be reported, never silently skipped.
func TestConflictResolutionStage_BadRepoSlugFailsClosed(t *testing.T) {
	repo, head := crRepo(t)
	origPusher := newPusher
	newPusher = func() pusher { return &fakePusher{} }
	t.Cleanup(func() { newPusher = origPusher })
	origAgent := conflictResolutionAgentInvoker
	conflictResolutionAgentInvoker = func(_ context.Context, cfg config, _ io.Writer) error {
		crWrite(t, cfg.workingDir, "conflict.txt", "ours\n")
		return nil
	}
	t.Cleanup(func() { conflictResolutionAgentInvoker = origAgent })
	t.Setenv("GITHUB_REPOSITORY", "")

	cfg := crStageCfg(repo)
	cfg.githubRepo = "not-a-slug"
	client := &crFakeUpload{}
	if code := runConflictResolutionStage(context.Background(), cfg, crRequest(head),
		client, &upload.IssuedKey{PrivateKey: make([]byte, 64)}, io.Discard); code != exitFailure {
		t.Fatalf("exit code = %d, want %d", code, exitFailure)
	}
	if len(client.ships) != 1 || !strings.Contains(client.ships[0].Reason, "is not owner/name") {
		t.Fatalf("ships = %+v, want one report naming the unresolvable slug", client.ships)
	}
}

// --- helpers pinned in isolation ---

// TestSplitNUL pins that path enumeration splits on NUL, not newline: a
// filename carrying a newline must stay ONE path. A line-oriented split would
// turn it into two and drop the real path out of the conflicted set.
func TestSplitNUL(t *testing.T) {
	got := splitNUL([]byte("a.txt\x00dir/we\nird.txt\x00"))
	if len(got) != 2 || got[1] != "dir/we\nird.txt" {
		t.Fatalf("splitNUL = %q, want the newline-bearing path intact", got)
	}
}

// TestLogEventQuotesValues pins that a git error carrying a quote cannot break
// the runner's JSON log record.
func TestLogEventQuotesValues(t *testing.T) {
	var sb strings.Builder
	logEvent(&sb, "conflict_resolution_refused", map[string]string{
		"reason": `he said "no"`, "empty": "",
	})
	var decoded map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(sb.String())), &decoded); err != nil {
		t.Fatalf("log line is not valid JSON: %v (%s)", err, sb.String())
	}
	if decoded["reason"] != `he said "no"` {
		t.Errorf("reason = %q, want the quoted value round-tripped", decoded["reason"])
	}
	if _, ok := decoded["empty"]; ok {
		t.Error("empty field was emitted")
	}
}

// TestConflictRecoveryTimeoutIsBounded pins that the detached recovery context
// is BOUNDED, not merely uncancellable: WithoutCancel alone would let a wedged
// git command hang the stage forever.
func TestConflictRecoveryTimeoutIsBounded(t *testing.T) {
	if conflictRecoveryTimeout <= 0 || conflictRecoveryTimeout > 10*time.Minute {
		t.Fatalf("conflictRecoveryTimeout = %v, want a bounded positive duration", conflictRecoveryTimeout)
	}
}

var _ = gitops.DefaultRemote

// crDeleteModifyRepo builds a real delete/modify conflict: `feature` modifies
// doomed.txt while `main` DELETES it. git leaves the path unmerged with only
// stage 2 present.
func crDeleteModifyRepo(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	crGit(t, repo, "init", "--initial-branch=main")
	crGit(t, repo, "config", "user.name", "test")
	crGit(t, repo, "config", "user.email", "test@example.com")
	crWrite(t, repo, "doomed.txt", "base\n")
	crGit(t, repo, "add", "-A")
	crGit(t, repo, "commit", "-m", "initial")

	crGit(t, repo, "checkout", "-b", "feature")
	crWrite(t, repo, "doomed.txt", "ours modified\n")
	crGit(t, repo, "commit", "-am", "ours")

	crGit(t, repo, "checkout", "main")
	crGit(t, repo, "rm", "doomed.txt")
	crGit(t, repo, "commit", "-m", "theirs deletes")

	crGit(t, repo, "checkout", "feature")
	return repo, crGitOut(t, repo, "rev-parse", "HEAD")
}

// TestConflictResolutionPass_DeleteModify_AcceptsOursSide: a delete/modify
// resolution must be one side's FULL content or the deletion. Keeping ours is
// accepted.
func TestConflictResolutionPass_DeleteModify_AcceptsOursSide(t *testing.T) {
	repo, head := crDeleteModifyRepo(t)
	res := runConflictResolutionPass(context.Background(), repo, "origin", crRequest(head), noAgent, nil)
	if res.refused() {
		t.Fatalf("keeping ours was refused: %s (%s)", res.Reason, res.Detail)
	}
	if got := crGitOut(t, repo, "rev-parse", "HEAD"); got != res.HeadSHA {
		t.Errorf("HEAD = %s, want the merge commit %s", got, res.HeadSHA)
	}
}

// TestConflictResolutionPass_DeleteModify_AcceptsTheDeletion: taking the
// deletion is the other accepted resolution.
func TestConflictResolutionPass_DeleteModify_AcceptsTheDeletion(t *testing.T) {
	repo, head := crDeleteModifyRepo(t)
	res := runConflictResolutionPass(context.Background(), repo, "origin", crRequest(head),
		func(context.Context) error { return os.Remove(filepath.Join(repo, "doomed.txt")) }, nil)
	if res.refused() {
		t.Fatalf("taking the deletion was refused: %s (%s)", res.Reason, res.Detail)
	}
	if _, err := os.Stat(filepath.Join(repo, "doomed.txt")); !os.IsNotExist(err) {
		t.Errorf("doomed.txt still present after the deletion resolution (err=%v)", err)
	}
}

// TestConflictResolutionPass_DeleteModify_RefusesInventedContent: anything that
// is neither side's full content nor the deletion is refused by name.
func TestConflictResolutionPass_DeleteModify_RefusesInventedContent(t *testing.T) {
	repo, head := crDeleteModifyRepo(t)
	res := runConflictResolutionPass(context.Background(), repo, "origin", crRequest(head),
		func(context.Context) error { crWrite(t, repo, "doomed.txt", "a third thing\n"); return nil }, nil)
	if res.Reason != "conflict_resolution_delete_modify_content" {
		t.Fatalf("reason = %q (%s), want conflict_resolution_delete_modify_content", res.Reason, res.Detail)
	}
	crAssertRestored(t, repo, head)
}

// TestConflictResolutionPass_BinaryConflictRefused: git leaves OURS on disk
// with NO markers for a binary conflict, so there is no hunk boundary to
// confine anything to and the pass refuses by name.
func TestConflictResolutionPass_BinaryConflictRefused(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	crGit(t, repo, "init", "--initial-branch=main")
	crGit(t, repo, "config", "user.name", "test")
	crGit(t, repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("blob.bin binary\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "blob.bin"), []byte{0, 1, 2, 3}, 0o644); err != nil {
		t.Fatal(err)
	}
	crGit(t, repo, "add", "-A")
	crGit(t, repo, "commit", "-m", "initial")

	crGit(t, repo, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo, "blob.bin"), []byte{9, 9, 9, 9}, 0o644); err != nil {
		t.Fatal(err)
	}
	crGit(t, repo, "commit", "-am", "ours")
	crGit(t, repo, "checkout", "main")
	if err := os.WriteFile(filepath.Join(repo, "blob.bin"), []byte{7, 7, 7, 7}, 0o644); err != nil {
		t.Fatal(err)
	}
	crGit(t, repo, "commit", "-am", "theirs")
	crGit(t, repo, "checkout", "feature")
	head := crGitOut(t, repo, "rev-parse", "HEAD")

	res := runConflictResolutionPass(context.Background(), repo, "origin", crRequest(head), noAgent, nil)
	if res.Reason != "conflict_resolution_binary_conflict" {
		t.Fatalf("reason = %q (%s), want conflict_resolution_binary_conflict", res.Reason, res.Detail)
	}
	crAssertRestored(t, repo, head)
}

// TestReadWorkingFile_Modes pins the filesystem→git mode derivation the gate's
// mode comparison depends on: 100644, 100755 and the symlink 120000. A wrong
// mapping would let a regular-file→symlink swap on a conflicted path through.
func TestReadWorkingFile_Modes(t *testing.T) {
	dir := t.TempDir()
	crWrite(t, dir, "plain.txt", "x\n")
	crWrite(t, dir, "exec.sh", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(dir, "exec.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("plain.txt", filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	for _, tc := range []struct{ path, mode, bytes string }{
		{"plain.txt", "100644", "x\n"},
		{"exec.sh", "100755", "#!/bin/sh\n"},
		{"link", "120000", "plain.txt"},
	} {
		got, err := readWorkingFile(dir, tc.path)
		if err != nil {
			t.Fatalf("readWorkingFile(%q): %v", tc.path, err)
		}
		if !got.Present {
			t.Errorf("%s: Present = false", tc.path)
		}
		if got.Mode != tc.mode {
			t.Errorf("%s: mode = %q, want %q", tc.path, got.Mode, tc.mode)
		}
		if string(got.Bytes) != tc.bytes {
			t.Errorf("%s: bytes = %q, want %q", tc.path, got.Bytes, tc.bytes)
		}
	}

	// An absent path is the zero value, NOT an error: a deleted conflicted path
	// is a gate decision (conflict_resolution_conflicted_path_missing), not a
	// read failure that would mask it behind observe_failed.
	got, err := readWorkingFile(dir, "nope.txt")
	if err != nil {
		t.Fatalf("absent path returned an error: %v", err)
	}
	if got.Present {
		t.Error("absent path reported Present")
	}
}

// TestConflictHelpers_FailClosedOutsideAGitRepo pins the git-error branch of
// every capture/observe helper: a directory that is not a work tree makes each
// one return an error rather than an empty-but-plausible value. An empty
// baseline would make the gate authorize everything.
func TestConflictHelpers_FailClosedOutsideAGitRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	ctx := context.Background()

	if _, err := readMergeMessage(ctx, dir); err == nil {
		t.Error("readMergeMessage outside a work tree returned nil error")
	}
	if _, err := readStageZeroIndex(ctx, dir); err == nil {
		t.Error("readStageZeroIndex outside a work tree returned nil error")
	}
	if _, err := captureConflictBaseline(ctx, dir, "head", map[string]bool{}); err == nil {
		t.Error("captureConflictBaseline outside a work tree returned nil error")
	}
	if _, err := observeConflictState(ctx, dir, conflictresolveBaselineForTest()); err == nil {
		t.Error("observeConflictState outside a work tree returned nil error")
	}
}

func conflictresolveBaselineForTest() conflictresolve.Baseline {
	return conflictresolve.Baseline{
		Index:      map[string]conflictresolve.IndexEntry{},
		Conflicted: map[string]conflictresolve.ConflictedFile{},
	}
}

// TestUnmergedStages_SkipsMalformedRecords pins the tolerance branches: a
// record with no tab, too few fields, or an unparseable stage number is
// SKIPPED rather than panicking or mis-attributing a stage — a wrong stage set
// would misclassify a content conflict as delete/modify.
func TestUnmergedStages_SkipsMalformedRecords(t *testing.T) {
	raw := []byte(strings.Join([]string{
		"no-tab-here",
		"100644 oid\ttoo-few-fields.txt",
		"100644 oid notanumber\tbad-stage.txt",
		"100644 oid 2\tgood.txt",
		"100644 oid 3\tgood.txt",
		"100644 oid 0\t",
	}, "\x00") + "\x00")

	got := unmergedStages(raw)
	if len(got) != 1 {
		t.Fatalf("stages = %v, want only the well-formed path", got)
	}
	if !got["good.txt"][2] || !got["good.txt"][3] {
		t.Errorf("good.txt stages = %v, want both 2 and 3", got["good.txt"])
	}
}

// TestUnmergedPathSet_DeduplicatesAcrossStages: one path appears once per stage
// present, and the conflicted set must carry it exactly once.
func TestUnmergedPathSet_DeduplicatesAcrossStages(t *testing.T) {
	raw := []byte("100644 a 1\tp.txt\x00100644 b 2\tp.txt\x00100644 c 3\tp.txt\x00")
	got := unmergedPathSet(raw)
	if len(got) != 1 || !got["p.txt"] {
		t.Fatalf("path set = %v, want exactly {p.txt}", got)
	}
}

// TestConflictResolutionStage_ReportShipFailures pins both report-failure
// branches. A report that cannot be delivered is the one case where the stage
// genuinely may strand, so it must exit non-zero and log rather than exit 0 and
// claim success.
func TestConflictResolutionStage_ReportShipFailures(t *testing.T) {
	cases := []struct {
		name   string
		agent  func(cfg config) error
		wantEv string
	}{
		{
			name:   "success_report_undeliverable",
			agent:  func(cfg config) error { crWrite(t, cfg.workingDir, "conflict.txt", "ours\n"); return nil },
			wantEv: "conflict_resolution_report_failed",
		},
		{
			name:   "refusal_report_undeliverable",
			agent:  func(cfg config) error { crWrite(t, cfg.workingDir, "quiet.txt", "tampered\n"); return nil },
			wantEv: "conflict_resolution_report_failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, head := crRepo(t)
			origPusher := newPusher
			newPusher = func() pusher { return &fakePusher{} }
			t.Cleanup(func() { newPusher = origPusher })
			origAgent := conflictResolutionAgentInvoker
			conflictResolutionAgentInvoker = func(_ context.Context, cfg config, _ io.Writer) error { return tc.agent(cfg) }
			t.Cleanup(func() { conflictResolutionAgentInvoker = origAgent })

			var sb strings.Builder
			client := &crFakeUpload{err: errors.New("backend unreachable")}
			if code := runConflictResolutionStage(context.Background(), crStageCfg(repo), crRequest(head),
				client, &upload.IssuedKey{PrivateKey: make([]byte, 64)}, &sb); code != exitFailure {
				t.Fatalf("exit code = %d, want %d", code, exitFailure)
			}
			if !strings.Contains(sb.String(), tc.wantEv) {
				t.Errorf("log = %q, want an event naming %q", sb.String(), tc.wantEv)
			}
		})
	}
}

// --- agent-writable .git state: neutralization + artifact verification (#3202) ---

// crWriteHook installs an executable hook under .git/hooks. The agent runs
// under a WORKING-TREE-EDITS-ONLY contract, but .git/hooks is outside the
// working tree, so nothing in the gate's observation sees this — which is
// exactly the bypass these tests pin.
func crWriteHook(t *testing.T, repo, name, script string) {
	t.Helper()
	dir := filepath.Join(repo, ".git", "hooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// crListDir returns the directory's entry names, sorted, for sentinel checks.
func crListDir(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// TestConflictResolutionPass_NeutralizesAgentWrittenHooks is the routed
// security concern's named test: an agent that legitimately resolves the
// conflicted file ALSO installs .git hooks that tamper with the artifact after
// the gate has already said yes.
//
// The pre-commit hook stages an out-of-set file (the tree the gate verified is
// then not the tree that gets pushed); prepare-commit-msg rewrites the merge
// message after conflict_resolution_merge_message_changed was checked — and
// `--no-verify` alone would NOT stop that one; post-commit proves whether any
// hook ran at all. The assertions read the COMMITTED REPOSITORY back, not a
// returned error: a hook that fired and was then papered over returns the same
// nil error as one that never ran.
func TestConflictResolutionPass_NeutralizesAgentWrittenHooks(t *testing.T) {
	repo, head := crRepo(t)
	sentinels := t.TempDir()

	res := runConflictResolutionPass(context.Background(), repo, "origin", crRequest(head),
		func(context.Context) error {
			crWrite(t, repo, "conflict.txt", "ours\n")
			crWriteHook(t, repo, "pre-commit", "#!/bin/sh\n"+
				"echo owned > evil.txt\n"+
				"git add evil.txt\n"+
				"touch "+filepath.Join(sentinels, "pre-commit")+"\n")
			crWriteHook(t, repo, "prepare-commit-msg", "#!/bin/sh\n"+
				"echo 'rewritten by the hook' > \"$1\"\n"+
				"touch "+filepath.Join(sentinels, "prepare-commit-msg")+"\n")
			crWriteHook(t, repo, "post-commit", "#!/bin/sh\n"+
				"touch "+filepath.Join(sentinels, "post-commit")+"\n")
			return nil
		}, nil)

	if got := crListDir(t, sentinels); len(got) != 0 {
		t.Errorf("agent-installed hooks RAN during the pass: %v", got)
	}
	if res.refused() {
		t.Fatalf("pass refused with the hooks neutralized: %s (%s)", res.Reason, res.Detail)
	}
	// The published artifact is the one the gate authorized: the out-of-set
	// file the hook would have staged is in no tree, and the merge message is
	// git's, not the hook's.
	tree := crGitOut(t, repo, "ls-tree", "--name-only", "-r", "HEAD")
	if strings.Contains(tree, "evil.txt") {
		t.Errorf("out-of-set file reached the committed tree:\n%s", tree)
	}
	if msg := crGitOut(t, repo, "log", "-1", "--format=%B", "HEAD"); strings.Contains(msg, "rewritten by the hook") {
		t.Errorf("merge message was rewritten by the hook: %q", msg)
	}
}

// TestConflictResolutionPass_RefusesCommitOffTheAuthorizedTree pins the
// mechanism-INDEPENDENT half: whatever stages content between the approved
// index and the commit, the commit that would be published is compared against
// the authorized tree and refused when it differs.
//
// The seam stands in for a hook the neutralization missed — the point of the
// check is that it does not depend on the deny-list being complete.
func TestConflictResolutionPass_RefusesCommitOffTheAuthorizedTree(t *testing.T) {
	repo, head := crRepo(t)
	orig := conflictPostAddHook
	t.Cleanup(func() { conflictPostAddHook = orig })
	conflictPostAddHook = func(repoDir string) {
		crWrite(t, repoDir, "evil.txt", "owned\n")
		crGit(t, repoDir, "add", "evil.txt")
	}

	res := runConflictResolutionPass(context.Background(), repo, "origin", crRequest(head),
		func(context.Context) error { crWrite(t, repo, "conflict.txt", "ours\n"); return nil }, nil)

	if res.Reason != reasonCommitTreeChanged {
		t.Fatalf("reason = %q (%s), want %q", res.Reason, res.Detail, reasonCommitTreeChanged)
	}
	// The refusal's effect is COMMITTED STATE: the unauthorized commit must be
	// gone, not merely reported.
	crAssertRestored(t, repo, head)
	if log := crGitOut(t, repo, "log", "--all", "--name-only", "--format="); strings.Contains(log, "evil.txt") {
		t.Errorf("the unauthorized commit survived the refusal:\n%s", log)
	}
}

// TestConflictResolutionPass_RefusesRewrittenCommitMessage pins the message
// half of the same check: the gate compared MERGE_MSG BEFORE the commit, so a
// rewrite DURING it is caught only by comparing what was actually recorded.
func TestConflictResolutionPass_RefusesRewrittenCommitMessage(t *testing.T) {
	repo, head := crRepo(t)
	orig := conflictPostAddHook
	t.Cleanup(func() { conflictPostAddHook = orig })
	conflictPostAddHook = func(repoDir string) {
		if err := os.WriteFile(filepath.Join(repoDir, ".git", "MERGE_MSG"),
			[]byte("a message the gate never saw\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res := runConflictResolutionPass(context.Background(), repo, "origin", crRequest(head),
		func(context.Context) error { crWrite(t, repo, "conflict.txt", "ours\n"); return nil }, nil)

	if res.Reason != reasonCommittedMessageChanged {
		t.Fatalf("reason = %q (%s), want %q", res.Reason, res.Detail, reasonCommittedMessageChanged)
	}
	crAssertRestored(t, repo, head)
}

// TestConflictResolutionPass_RefusesFilterMangledStagedContent pins the in-pass
// filter/attribute vector. The filter DRIVER is configured before the pass (so
// it is baseline config, not a config change), and the agent binds it to the
// conflicted path by writing `.git/info/attributes` — a file outside the working
// tree the gate observes. That binding CHANGES the conflicted path's effective
// attributes, which the attributes gate (#3339) catches at step 6, BEFORE the
// add, with its own named reason. Catching the attribute change is the earlier,
// more precise refusal than the staged-content check that would otherwise fire.
//
// Nothing here uses a seam: the filter and the attribute binding are real
// repository state and git runs them.
func TestConflictResolutionPass_RefusesFilterMangledStagedContent(t *testing.T) {
	repo, head := crRepo(t)
	crGit(t, repo, "config", "filter.mangle.clean", "sed s/ours/owned/")

	res := runConflictResolutionPass(context.Background(), repo, "origin", crRequest(head),
		func(context.Context) error {
			crWrite(t, repo, "conflict.txt", "ours\n")
			if err := os.WriteFile(filepath.Join(repo, ".git", "info", "attributes"),
				[]byte("conflict.txt filter=mangle\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return nil
		}, nil)

	if res.Reason != string(conflictresolve.ReasonAttributesChanged) {
		t.Fatalf("reason = %q (%s), want %q", res.Reason, res.Detail, conflictresolve.ReasonAttributesChanged)
	}
	if !strings.Contains(res.Detail, "conflict.txt: filter") {
		t.Errorf("detail does not name the changed attribute: %q", res.Detail)
	}
	if strings.Contains(res.Detail, "mangle") {
		t.Errorf("detail leaked the attribute VALUE: %q", res.Detail)
	}
	if got := crGitOut(t, repo, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD = %s, want the pre-merge tip %s", got, head)
	}
	// The filter is still configured, so `git status` re-runs it on every read
	// and reports the file dirty — the pass's own recovery verdict is therefore
	// conservatively "not restored", which is the fail-safe direction. Remove
	// the adversarial state (not its effect) before asserting cleanliness, so
	// this test measures recovery rather than the filter.
	if err := os.Remove(filepath.Join(repo, ".git", "info", "attributes")); err != nil {
		t.Fatal(err)
	}
	crGit(t, repo, "config", "--unset", "filter.mangle.clean")
	crAssertRestored(t, repo, head)
}

// TestConflictResolutionPass_RefusesWorkingTreeAttributeEdit is the sibling
// vector: instead of `.git/info/attributes`, the agent edits the COMMITTED,
// tracked working-tree `.gitattributes` to bind the pre-configured filter to the
// conflicted path. That edit changes the effective attributes AND is itself an
// out-of-set unstaged change, so `conflict_resolution_attributes_changed` is
// among the violations (it sorts first, being repository-level).
func TestConflictResolutionPass_RefusesWorkingTreeAttributeEdit(t *testing.T) {
	repo, head := crRepoWithAttributes(t, "conflict.txt eol=lf\n")
	crGit(t, repo, "config", "filter.mangle.clean", "sed s/ours/owned/")

	res := runConflictResolutionPass(context.Background(), repo, "origin", crRequest(head),
		func(context.Context) error {
			crWrite(t, repo, "conflict.txt", "ours\n")
			crWrite(t, repo, ".gitattributes", "conflict.txt eol=lf filter=mangle\n")
			return nil
		}, nil)

	if !crViolationsContain(res, conflictresolve.ReasonAttributesChanged) {
		t.Fatalf("violations = %+v, want one naming %q", res.Violations, conflictresolve.ReasonAttributesChanged)
	}
	crGit(t, repo, "config", "--unset", "filter.mangle.clean")
	crAssertRestored(t, repo, head)
}

// TestNormalizeCommitMessage pins the symmetry the committed-message comparison
// depends on: git strips comment lines and normalizes trailing blank lines when
// it records a merge, so a raw comparison against MERGE_MSG would refuse EVERY
// pass, while a normalization applied to only one side would miss a rewrite.
func TestNormalizeCommitMessage(t *testing.T) {
	mergeMsg := "Merge branch 'main' into feature\n\n# Conflicts:\n#\tconflict.txt\n"
	recorded := "Merge branch 'main' into feature\n\n# Conflicts:\n#\tconflict.txt\n\n"
	if normalizeCommitMessage(mergeMsg) != normalizeCommitMessage(recorded) {
		t.Errorf("git's own cleanup read as a rewrite: %q vs %q",
			normalizeCommitMessage(mergeMsg), normalizeCommitMessage(recorded))
	}
	if normalizeCommitMessage(mergeMsg) == normalizeCommitMessage("owned\n") {
		t.Error("a rewritten body normalized equal to the merge message")
	}
}

// --- the repository-config gate input (#3338) ---

// crResolve is the agent stub that resolves the conflict CORRECTLY. Every
// config-gate case below composes it, so the only difference from the accept
// control (TestConflictResolutionPass_AcceptsResolvedHunk) is the config write
// — which is what makes the refusal discriminating rather than blanket.
func crResolve(t *testing.T, repo string) func(context.Context) error {
	t.Helper()
	return func(context.Context) error {
		crWrite(t, repo, "conflict.txt", "ours\n")
		return nil
	}
}

// TestConflictResolutionPass_RefusesRepoConfigChange is the issue's case. The
// agent resolves the hunk exactly as required AND sets a
// `url.<decoy>.insteadOf` key that would silently redirect the runner's own
// push. `.git/config` is outside the working tree the agent's contract confines
// it to, so nothing else in the gate sees it.
//
// The second case writes an UNRELATED key: the control does not care WHICH key
// moved, which is the whole point of capturing the configuration instead of
// enumerating dangerous keys.
func TestConflictResolutionPass_RefusesRepoConfigChange(t *testing.T) {
	cases := []struct {
		name  string
		write func(t *testing.T, repo string)
	}{
		{"push_redirect_key", func(t *testing.T, repo string) {
			crGit(t, repo, "config", "url.https://decoy.example/.insteadOf", "https://origin.example/")
		}},
		{"unrelated_key", func(t *testing.T, repo string) {
			crGit(t, repo, "config", "fishhawk.probe", "1")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, head := crRepo(t)
			resolve := crResolve(t, repo)
			res := runConflictResolutionPass(context.Background(), repo, "origin", crRequest(head),
				func(ctx context.Context) error {
					if err := resolve(ctx); err != nil {
						return err
					}
					tc.write(t, repo)
					return nil
				}, nil)

			if res.Reason != string(conflictresolve.ReasonRepoConfigChanged) {
				t.Fatalf("reason = %q (%s), want %q", res.Reason, res.Detail,
					conflictresolve.ReasonRepoConfigChanged)
			}
			// The refusal's effect is COMMITTED STATE: no merge commit may
			// survive, and the repository must be back at the pre-merge tip.
			crAssertRestored(t, repo, head)
		})
	}
}

// TestConflictResolutionPass_ConfigReadFailures pins BOTH config reads' own
// fail-closed returns. Neither side may default to the empty string: an empty
// baseline config compares unequal to every observation, and an empty observed
// one would make a LATER change undetectable. Each surfaces as its OWN named
// reason rather than being mis-attributed.
func TestConflictResolutionPass_ConfigReadFailures(t *testing.T) {
	cases := []struct {
		name     string
		failCall int // 1 = the baseline capture's read, 2 = the observation's
		reason   string
	}{
		{"baseline_side", 1, reasonBaselineCaptureFailed},
		{"observe_side", 2, reasonObserveFailed},
		// The THIRD config read is step 7a's post-add re-verification: a failure
		// there is fail-closed as conflict_resolution_staged_content_changed,
		// because an unreadable config after the add cannot prove the transform
		// git applied is the one the gate decided on.
		{"post_add_side", 3, reasonStagedContentChanged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, head := crRepo(t)
			orig := readGitConfigFn
			t.Cleanup(func() { readGitConfigFn = orig })
			calls := 0
			readGitConfigFn = func(ctx context.Context, repoDir string) (string, error) {
				calls++
				if calls == tc.failCall {
					return "", errors.New("read git config: simulated failure")
				}
				return orig(ctx, repoDir)
			}

			res := runConflictResolutionPass(context.Background(), repo, "origin", crRequest(head),
				crResolve(t, repo), nil)

			if res.Reason != tc.reason {
				t.Fatalf("reason = %q (%s), want %q", res.Reason, res.Detail, tc.reason)
			}
			crAssertRestored(t, repo, head)
		})
	}
}

// TestConflictResolutionPass_RefusesIndentedCommentLineRewrite is the done-means
// test for narrowing normalizeCommitMessage to git's OWN first-byte comment
// rule.
//
// The committed message differs from the gate's merge message by exactly one
// INDENTED comment-like line. git treats that as ordinary message content (only
// a line whose FIRST byte is the comment character is a comment), so it lands in
// the recorded message — but the pre-change normalization trimmed leading
// whitespace before the test and deleted it from BOTH sides, so the comparison
// could not see it. This test is RED under that normalization, which is what
// makes it a control rather than a restatement.
func TestConflictResolutionPass_RefusesIndentedCommentLineRewrite(t *testing.T) {
	repo, head := crRepo(t)
	orig := conflictPostAddHook
	t.Cleanup(func() { conflictPostAddHook = orig })
	conflictPostAddHook = func(repoDir string) {
		msgPath := filepath.Join(repoDir, ".git", "MERGE_MSG")
		existing, err := os.ReadFile(msgPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(msgPath, append(existing, []byte("  # smuggled\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res := runConflictResolutionPass(context.Background(), repo, "origin", crRequest(head),
		crResolve(t, repo), nil)

	if res.Reason != reasonCommittedMessageChanged {
		t.Fatalf("reason = %q (%s), want %q", res.Reason, res.Detail, reasonCommittedMessageChanged)
	}
	crAssertRestored(t, repo, head)
}

// TestNormalizeCommitMessageFirstByteRule pins the narrowing at the unit level
// alongside the end-to-end case above: an indented comment-like line is CONTENT
// (git's rule is the first byte), while an unindented one is a comment.
func TestNormalizeCommitMessageFirstByteRule(t *testing.T) {
	const base = "Merge branch 'main'\n"
	if normalizeCommitMessage(base) == normalizeCommitMessage(base+"  # smuggled\n") {
		t.Error("an INDENTED comment-like line was stripped — git treats it as message content")
	}
	if normalizeCommitMessage(base) != normalizeCommitMessage(base+"# a real comment\n") {
		t.Error("an unindented comment line was NOT stripped")
	}
}

// --- the conflicted set's effective attributes as a gate input (#3339) ---

// crRepoWithAttributes is crRepo whose INITIAL commit also carries a
// `.gitattributes` binding, inherited by both branches. It is the fixture for
// the honest-transformation accept cases (a committed `text eol=crlf` makes
// `git add` legitimately clean CRLF→LF) and the staged-content detail contract.
func crRepoWithAttributes(t *testing.T, gitattributes string) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	crGit(t, repo, "init", "--initial-branch=main")
	crGit(t, repo, "config", "user.name", "test")
	crGit(t, repo, "config", "user.email", "test@example.com")
	crWrite(t, repo, ".gitattributes", gitattributes)
	crWrite(t, repo, "conflict.txt", "base\n")
	crWrite(t, repo, "quiet.txt", "untouched\n")
	crGit(t, repo, "add", "-A")
	crGit(t, repo, "commit", "-m", "initial")

	crGit(t, repo, "checkout", "-b", "feature")
	crWrite(t, repo, "conflict.txt", "ours\n")
	crGit(t, repo, "commit", "-am", "ours")

	crGit(t, repo, "checkout", "main")
	crWrite(t, repo, "conflict.txt", "theirs\n")
	crWrite(t, repo, "cleanly-advanced.txt", "new on base\n")
	crGit(t, repo, "add", "-A")
	crGit(t, repo, "commit", "-m", "theirs")

	crGit(t, repo, "checkout", "feature")
	return repo, crGitOut(t, repo, "rev-parse", "HEAD")
}

// crGitOutBytes runs git and returns raw, UNTRIMMED stdout — needed to assert on
// a blob whose exact trailing newline is the point (crGitOut trims it away).
func crGitOutBytes(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// crHashObject computes the OID `git add` would produce for content at path,
// with the path's own attributes applied — the by-construction expected value
// for the staged-content detail contract.
func crHashObject(t *testing.T, repo, path, content string) string {
	t.Helper()
	cmd := exec.Command("git", "hash-object", "--path="+path, "--stdin")
	cmd.Dir = repo
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git hash-object: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// crViolationsContain reports whether the refusal carried a violation with the
// given reason.
func crViolationsContain(res conflictResolutionResult, reason conflictresolve.Reason) bool {
	for _, v := range res.Violations {
		if v.Reason == reason {
			return true
		}
	}
	return false
}

// TestConflictResolutionPass_AcceptsAttributeResolution is the DONE-MEANS test
// (RED under the pre-change raw-bytes comparison): an honest resolution in a
// repository whose committed `.gitattributes` legitimately transforms bytes on
// `git add` is ACCEPTED, because step 7a compares the staged blob against the
// EXPECTED POST-CLEAN form of the gate-observed bytes rather than their raw
// bytes. Each row asserts the committed blob is the LF-normalized `ours\n`.
//
// The `text=auto` row: if it reddens the divergence is a REFUSE (fail-closed),
// which is a result to investigate, NEVER grounds to loosen the comparison
// (operator instruction).
func TestConflictResolutionPass_AcceptsAttributeResolution(t *testing.T) {
	cases := []struct{ name, attr, agentBytes string }{
		{"text_eol_crlf", "conflict.txt text eol=crlf\n", "ours\r\n"},
		{"text_auto", "conflict.txt text=auto\n", "ours\r\n"},
		{"eol_lf", "conflict.txt eol=lf\n", "ours\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, head := crRepoWithAttributes(t, tc.attr)
			res := runConflictResolutionPass(context.Background(), repo, "origin", crRequest(head),
				func(context.Context) error { crWrite(t, repo, "conflict.txt", tc.agentBytes); return nil }, nil)

			if res.refused() {
				t.Fatalf("honest attribute resolution refused — fail-closed direction, investigate, do NOT loosen: %s (%s)",
					res.Reason, res.Detail)
			}
			if blob := crGitOutBytes(t, repo, "cat-file", "blob", "HEAD:conflict.txt"); string(blob) != "ours\n" {
				t.Errorf("committed blob = %q, want the LF-normalized %q", blob, "ours\n")
			}
		})
	}
}

// TestConflictResolutionPass_AcceptsAutocrlfResolution is the config-driven
// sibling: `core.autocrlf=true` set BEFORE the pass (so it is baseline config)
// makes `git add` clean the agent's CRLF to LF, and the post-clean comparison
// accepts it.
func TestConflictResolutionPass_AcceptsAutocrlfResolution(t *testing.T) {
	repo, head := crRepo(t)
	crGit(t, repo, "config", "core.autocrlf", "true")

	res := runConflictResolutionPass(context.Background(), repo, "origin", crRequest(head),
		func(context.Context) error { crWrite(t, repo, "conflict.txt", "ours\r\n"); return nil }, nil)

	if res.refused() {
		t.Fatalf("autocrlf resolution refused: %s (%s)", res.Reason, res.Detail)
	}
	if blob := crGitOutBytes(t, repo, "cat-file", "blob", "HEAD:conflict.txt"); string(blob) != "ours\n" {
		t.Errorf("committed blob = %q, want the LF-normalized %q", blob, "ours\n")
	}
}

// TestConflictResolutionPass_RefusesAttributeChangeBetweenGateAndAdd pins step
// 7a part (i)'s attribute half: the gate passes, then the conflictCommitHook
// binds the pre-configured filter driver via `.git/info/attributes` between the
// gate and the `git add`. The post-add attribute re-read detects the drift and
// refuses conflict_resolution_staged_content_changed, naming attributes as the
// moved input.
func TestConflictResolutionPass_RefusesAttributeChangeBetweenGateAndAdd(t *testing.T) {
	repo, head := crRepo(t)
	crGit(t, repo, "config", "filter.mangle.clean", "sed s/ours/owned/")
	orig := conflictCommitHook
	t.Cleanup(func() { conflictCommitHook = orig })
	conflictCommitHook = func(repoDir string) {
		_ = os.WriteFile(filepath.Join(repoDir, ".git", "info", "attributes"),
			[]byte("conflict.txt filter=mangle\n"), 0o644)
	}

	res := runConflictResolutionPass(context.Background(), repo, "origin", crRequest(head),
		func(context.Context) error { crWrite(t, repo, "conflict.txt", "ours\n"); return nil }, nil)

	if res.Reason != reasonStagedContentChanged {
		t.Fatalf("reason = %q (%s), want %q", res.Reason, res.Detail, reasonStagedContentChanged)
	}
	if !strings.Contains(res.Detail, "attributes changed between the gate and the add") {
		t.Errorf("detail does not name ATTRIBUTES as the moved input: %q", res.Detail)
	}
	_ = os.Remove(filepath.Join(repo, ".git", "info", "attributes"))
	crGit(t, repo, "config", "--unset", "filter.mangle.clean")
	crAssertRestored(t, repo, head)
}

// TestConflictResolutionPass_RefusesConfigChangeBetweenGateAndAdd pins step 7a
// part (i)'s CONFIG half — the half the attribute sibling cannot cover (operator
// binding condition). A SUCCESSFUL config read returning CHANGED content between
// the gate and the add (distinct from the config-READ-FAILURE case) is refused
// conflict_resolution_staged_content_changed, naming config as the moved input.
func TestConflictResolutionPass_RefusesConfigChangeBetweenGateAndAdd(t *testing.T) {
	repo, head := crRepo(t)
	orig := conflictCommitHook
	t.Cleanup(func() { conflictCommitHook = orig })
	conflictCommitHook = func(repoDir string) {
		cmd := exec.Command("git", "config", "fishhawk.probe", "1")
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config: %v\n%s", err, out)
		}
	}

	res := runConflictResolutionPass(context.Background(), repo, "origin", crRequest(head),
		func(context.Context) error { crWrite(t, repo, "conflict.txt", "ours\n"); return nil }, nil)

	if res.Reason != reasonStagedContentChanged {
		t.Fatalf("reason = %q (%s), want %q", res.Reason, res.Detail, reasonStagedContentChanged)
	}
	if !strings.Contains(res.Detail, "configuration changed between the gate and the add") {
		t.Errorf("detail does not name CONFIG as the moved input: %q", res.Detail)
	}
	crAssertRestored(t, repo, head)
}

// TestConflictResolutionPass_RefusesContentChangeBetweenGateAndAdd pins step 7a
// part (ii) AND the staged-content detail contract. The fixture commits
// `.gitattributes` binding `text eol=crlf` (an honest transform) plus a custom
// `marker` attribute whose VALUE is secret-shaped but legal (gitattributes(5)).
// The agent resolves correctly (CRLF), the conflictCommitHook rewrites the file
// to the OTHER side between the gate and the add, and the refusal detail must
// carry the path, BOTH blob OIDs (computed by construction) and the attribute
// NAMES — never the planted value.
func TestConflictResolutionPass_RefusesContentChangeBetweenGateAndAdd(t *testing.T) {
	repo, head := crRepoWithAttributes(t, "conflict.txt text eol=crlf marker=hunter2-SECRET-VALUE\n")
	orig := conflictCommitHook
	t.Cleanup(func() { conflictCommitHook = orig })
	conflictCommitHook = func(repoDir string) {
		crWrite(t, repoDir, "conflict.txt", "theirs\r\n")
	}

	res := runConflictResolutionPass(context.Background(), repo, "origin", crRequest(head),
		func(context.Context) error { crWrite(t, repo, "conflict.txt", "ours\r\n"); return nil }, nil)

	if res.Reason != reasonStagedContentChanged {
		t.Fatalf("reason = %q (%s), want %q", res.Reason, res.Detail, reasonStagedContentChanged)
	}
	wantExpected := crHashObject(t, repo, "conflict.txt", "ours\r\n")
	wantStaged := crHashObject(t, repo, "conflict.txt", "theirs\r\n")
	if wantExpected == wantStaged {
		t.Fatalf("fixture is not discriminating: both OIDs are %s", wantStaged)
	}
	for _, want := range []string{"conflict.txt", wantStaged, wantExpected, "text", "eol", "marker"} {
		if !strings.Contains(res.Detail, want) {
			t.Errorf("detail missing %q: %q", want, res.Detail)
		}
	}
	for _, leak := range []string{"hunter2-SECRET-VALUE", "crlf"} {
		if strings.Contains(res.Detail, leak) {
			t.Errorf("detail leaked attribute VALUE %q: %q", leak, res.Detail)
		}
	}
	crAssertRestored(t, repo, head)
}

// TestConflictResolutionPass_ContentChangeDetailNamesNoneWithoutAttributes is
// the sibling asserting the `(attributes: none)` fallback on a repository with
// no `.gitattributes`.
func TestConflictResolutionPass_ContentChangeDetailNamesNoneWithoutAttributes(t *testing.T) {
	repo, head := crRepo(t)
	orig := conflictCommitHook
	t.Cleanup(func() { conflictCommitHook = orig })
	conflictCommitHook = func(repoDir string) { crWrite(t, repoDir, "conflict.txt", "theirs\n") }

	res := runConflictResolutionPass(context.Background(), repo, "origin", crRequest(head),
		func(context.Context) error { crWrite(t, repo, "conflict.txt", "ours\n"); return nil }, nil)

	if res.Reason != reasonStagedContentChanged {
		t.Fatalf("reason = %q (%s), want %q", res.Reason, res.Detail, reasonStagedContentChanged)
	}
	if !strings.Contains(res.Detail, "(attributes: none)") {
		t.Errorf("detail does not render the none fallback: %q", res.Detail)
	}
	crAssertRestored(t, repo, head)
}

// TestConflictResolutionPass_AttributeReadFailures pins all three attribute
// reads' own fail-closed returns via the seam, each surfacing as its OWN named
// reason: baseline capture, observe, and the step-7a post-add re-read.
func TestConflictResolutionPass_AttributeReadFailures(t *testing.T) {
	cases := []struct {
		name     string
		failCall int // 1 = baseline, 2 = observe, 3 = post-add re-read
		reason   string
	}{
		{"baseline_side", 1, reasonBaselineCaptureFailed},
		{"observe_side", 2, reasonObserveFailed},
		{"post_add_side", 3, reasonStagedContentChanged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, head := crRepo(t)
			orig := readConflictedAttributesFn
			t.Cleanup(func() { readConflictedAttributesFn = orig })
			calls := 0
			readConflictedAttributesFn = func(ctx context.Context, repoDir string, paths []string) (string, error) {
				calls++
				if calls == tc.failCall {
					return "", errors.New("read git attributes: simulated failure")
				}
				return orig(ctx, repoDir, paths)
			}

			res := runConflictResolutionPass(context.Background(), repo, "origin", crRequest(head),
				crResolve(t, repo), nil)

			if res.Reason != tc.reason {
				t.Fatalf("reason = %q (%s), want %q", res.Reason, res.Detail, tc.reason)
			}
			crAssertRestored(t, repo, head)
		})
	}
}
