package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// --- fixtures ---

// mcvCheckScript is the done-means verify: it fails when two migrations share
// a 4-digit sequence prefix, so each branch adding ONE migration with the same
// number is green alone and only their COMBINATION is red.
const mcvCheckScript = `#!/bin/sh
dups=$(ls migrations/*.sql 2>/dev/null | sed 's#.*/##' | cut -c1-4 | sort | uniq -d)
if [ -n "$dups" ]; then
  echo "duplicate migration sequence: $dups"
  exit 1
fi
echo "migrations ok"
`

const mcvVerifyCmd = "sh check.sh"

// mcvRepo builds the production shape: a bare origin holding `main`, branch
// `a` (adds 0096_a.sql), branch `b` (adds 0096_b.sql) and branch `combined`
// (b with a merged in), and a dispatch clone checked out on `main` — the
// operator's checkout the local runner is dispatched against. It returns the
// dispatch dir and the three branch tips.
func mcvRepo(t *testing.T) (dispatch, tipA, tipB, tipCombined string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	seed := t.TempDir()
	crGit(t, seed, "init", "--initial-branch=main")
	crGit(t, seed, "config", "user.name", "test")
	crGit(t, seed, "config", "user.email", "test@example.com")
	if err := os.MkdirAll(filepath.Join(seed, "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	crWrite(t, seed, "check.sh", mcvCheckScript)
	crWrite(t, seed, "migrations/0095_base.sql", "-- base\n")
	crGit(t, seed, "add", "-A")
	crGit(t, seed, "commit", "-m", "initial")

	crGit(t, seed, "checkout", "-b", "a")
	crWrite(t, seed, "migrations/0096_a.sql", "-- a\n")
	crGit(t, seed, "add", "-A")
	crGit(t, seed, "commit", "-m", "a")

	crGit(t, seed, "checkout", "main")
	crGit(t, seed, "checkout", "-b", "b")
	crWrite(t, seed, "migrations/0096_b.sql", "-- b\n")
	crGit(t, seed, "add", "-A")
	crGit(t, seed, "commit", "-m", "b")

	crGit(t, seed, "checkout", "-b", "combined")
	crGit(t, seed, "merge", "--no-edit", "a")
	crGit(t, seed, "checkout", "main")

	bare := filepath.Join(t.TempDir(), "origin.git")
	crGit(t, seed, "clone", "--bare", seed, bare)
	dispatch = filepath.Join(t.TempDir(), "dispatch")
	crGit(t, seed, "clone", bare, dispatch)
	crGit(t, dispatch, "checkout", "main")
	return dispatch,
		crGitOut(t, dispatch, "rev-parse", "refs/remotes/origin/a"),
		crGitOut(t, dispatch, "rev-parse", "refs/remotes/origin/b"),
		crGitOut(t, dispatch, "rev-parse", "refs/remotes/origin/combined")
}

func mcvCfg(dispatch, verifyCmd string) config {
	return config{runID: "run-1", stageID: "stage-1", workingDir: dispatch,
		githubRepo: "acme/widgets", verifyCmd: verifyCmd, verifyTimeout: time.Minute}
}

// mcvVerifyCounter wraps the verify seam, counting invocations and recording
// the scope set each was handed.
type mcvVerifyCounter struct {
	calls  int
	scopes [][]string
}

func withMCVVerifyCounter(t *testing.T) *mcvVerifyCounter {
	t.Helper()
	c := &mcvVerifyCounter{}
	orig := mergeCandidateVerifyFn
	mergeCandidateVerifyFn = func(ctx context.Context, verifyCmd, repoDir, headSHA string, timeout time.Duration, scopePkgs []string) (agent.Event, string, string, gateDisposition) {
		c.calls++
		c.scopes = append(c.scopes, scopePkgs)
		return orig(ctx, verifyCmd, repoDir, headSHA, timeout, scopePkgs)
	}
	t.Cleanup(func() { mergeCandidateVerifyFn = orig })
	return c
}

// runMCV drives the stage and returns the exit code and the single shipped
// report (failing the test unless EXACTLY one ShipPullRequest call was made).
func runMCV(t *testing.T, cfg config, req mergeCandidateVerifyRequest) (int, upload.ShipPullRequestArgs) {
	t.Helper()
	installGateState(t, nil)
	client := &crFakeUpload{}
	code := runMergeCandidateVerifyStage(context.Background(), cfg, req, client,
		&upload.IssuedKey{PrivateKey: make([]byte, 64)}, io.Discard)
	if len(client.ships) != 1 {
		t.Fatalf("ShipPullRequest calls = %d, want exactly 1", len(client.ships))
	}
	return code, client.ships[0]
}

// mcvRepoState is everything the pass must leave byte-identical in the
// operator's checkout: HEAD, every ref, the worktree list and the status.
func mcvRepoState(t *testing.T, dir string) string {
	t.Helper()
	return strings.Join([]string{
		crGitOut(t, dir, "rev-parse", "HEAD"),
		crGitOut(t, dir, "for-each-ref"),
		crGitOut(t, dir, "worktree", "list", "--porcelain"),
		crGitOut(t, dir, "status", "--porcelain"),
	}, "\n--\n")
}

// --- the done-means fixture ---

// TestMergeCandidateVerify_DuplicateSequenceDoneMeans is ADR-090's done-means:
// two branches that each verify green alone combine into a red merge
// candidate, and the pass reports exactly that — passed, passed, failed —
// against the head it was anchored to, in the FULL verify form.
func TestMergeCandidateVerify_DuplicateSequenceDoneMeans(t *testing.T) {
	dispatch, tipA, tipB, tipCombined := mcvRepo(t)
	counter := withMCVVerifyCounter(t)

	cases := []struct {
		branch, tip, want string
	}{
		{"a", tipA, mergeCandidatePassed},
		{"b", tipB, mergeCandidatePassed},
		{"combined", tipCombined, mergeCandidateFailed},
	}
	for _, tc := range cases {
		code, ship := runMCV(t, mcvCfg(dispatch, mcvVerifyCmd),
			mergeCandidateVerifyRequest{Branch: tc.branch, ExpectedHeadSHA: tc.tip, Cause: "base_advance"})
		if code != exitOK {
			t.Errorf("%s: exit = %d, want exitOK after a delivered report", tc.branch, code)
		}
		if ship.Outcome != upload.OutcomeMergeCandidateVerified {
			t.Errorf("%s: outcome = %q, want %q", tc.branch, ship.Outcome, upload.OutcomeMergeCandidateVerified)
		}
		if ship.MergeCandidateResult != tc.want {
			t.Errorf("%s: result = %q (%s), want %q; tail:\n%s", tc.branch, ship.MergeCandidateResult,
				ship.MergeCandidateReason, tc.want, ship.MergeCandidateOutputTail)
		}
		if ship.HeadSHA != tc.tip || ship.Branch != tc.branch {
			t.Errorf("%s: shipped head/branch = %s/%s, want %s/%s", tc.branch, ship.HeadSHA, ship.Branch, tc.tip, tc.branch)
		}
	}
	if counter.calls != 3 {
		t.Errorf("verify invocations = %d, want 3", counter.calls)
	}
	for i, s := range counter.scopes {
		if s != nil {
			t.Errorf("verify %d ran the SCOPED form with %v, want the FULL form (nil scope)", i, s)
		}
	}
	// The red tail names the collision.
	_, ship := runMCV(t, mcvCfg(dispatch, mcvVerifyCmd),
		mergeCandidateVerifyRequest{Branch: "combined", ExpectedHeadSHA: tipCombined, Cause: "base_advance"})
	if !strings.Contains(ship.MergeCandidateOutputTail, "duplicate migration sequence: 0096") {
		t.Errorf("failed tail must carry the verify output naming the collision: %q", ship.MergeCandidateOutputTail)
	}
}

// TestMergeCandidateVerify_FullFormEnv: the verify child sees NO
// FISHHAWK_VERIFY_PACKAGES — the merge candidate is never scope-narrowed.
func TestMergeCandidateVerify_FullFormEnv(t *testing.T) {
	dispatch, tipA, _, _ := mcvRepo(t)
	cmd := `printf 'pkgs=[%s]\n' "${FISHHAWK_VERIFY_PACKAGES-unset}"`
	_, ship := runMCV(t, mcvCfg(dispatch, cmd),
		mergeCandidateVerifyRequest{Branch: "a", ExpectedHeadSHA: tipA, Cause: "base_advance"})
	if ship.MergeCandidateResult != mergeCandidatePassed {
		t.Fatalf("result = %q (%s)", ship.MergeCandidateResult, ship.MergeCandidateReason)
	}
	if !strings.Contains(ship.MergeCandidateOutputTail, "pkgs=[unset]") {
		t.Errorf("verify env carried a package set, want the full form: %q", ship.MergeCandidateOutputTail)
	}
}

// TestMergeCandidateVerify_HeadMovedNeverRunsVerify: a remote tip other than
// the anchored head reports not_executed(head_moved) and the verify seam is
// invoked ZERO times — a verdict must never bind to a head it did not judge.
func TestMergeCandidateVerify_HeadMovedNeverRunsVerify(t *testing.T) {
	dispatch, tipA, tipB, _ := mcvRepo(t)
	counter := withMCVVerifyCounter(t)
	code, ship := runMCV(t, mcvCfg(dispatch, mcvVerifyCmd),
		mergeCandidateVerifyRequest{Branch: "a", ExpectedHeadSHA: tipB, Cause: "base_advance"})
	if code != exitOK {
		t.Errorf("exit = %d, want exitOK", code)
	}
	if counter.calls != 0 {
		t.Errorf("verify invocations = %d, want 0 on a moved head", counter.calls)
	}
	if ship.MergeCandidateResult != mergeCandidateNotExecuted || !strings.HasPrefix(ship.MergeCandidateReason, mcReasonHeadMoved) {
		t.Errorf("result/reason = %q/%q, want not_executed/head_moved", ship.MergeCandidateResult, ship.MergeCandidateReason)
	}
	if !strings.Contains(ship.MergeCandidateReason, tipA) || ship.HeadSHA != tipB {
		t.Errorf("reason %q must name the live tip %s; shipped head %s must be the anchored head %s",
			ship.MergeCandidateReason, tipA, ship.HeadSHA, tipB)
	}
}

// TestMergeCandidateVerify_WritesNothing is the no-commit / no-push / #3973
// non-regression: the pass never reaches a pusher, and the operator's checkout
// (HEAD, every ref, the worktree list, the status) is byte-identical after it.
func TestMergeCandidateVerify_WritesNothing(t *testing.T) {
	dispatch, _, _, tipCombined := mcvRepo(t)
	fp := &fakePusher{}
	origPusher := newPusher
	newPusher = func() pusher { return fp }
	t.Cleanup(func() { newPusher = origPusher })

	before := mcvRepoState(t, dispatch)
	_, ship := runMCV(t, mcvCfg(dispatch, mcvVerifyCmd),
		mergeCandidateVerifyRequest{Branch: "combined", ExpectedHeadSHA: tipCombined, Cause: "base_advance"})
	if ship.MergeCandidateResult != mergeCandidateFailed {
		t.Fatalf("result = %q, want failed (fixture)", ship.MergeCandidateResult)
	}
	if fp.calls != 0 || fp.gotArgs != nil || fp.pushCommittedArgs != nil {
		t.Errorf("the pass reached the pusher (CommitAndPush calls=%d, PushCommittedBranch=%v), want no push", fp.calls, fp.pushCommittedArgs)
	}
	if after := mcvRepoState(t, dispatch); after != before {
		t.Errorf("the operator checkout changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestMergeCandidateVerify_RedactsAndBoundsTail: a token-shaped secret in the
// verify output never ships, and the tail is bounded to its LAST bytes.
func TestMergeCandidateVerify_RedactsAndBoundsTail(t *testing.T) {
	dispatch, tipA, _, _ := mcvRepo(t)
	secret := "ghp_" + strings.Repeat("A1b2C3d4E5", 4)[:36]
	cmd := `i=0; while [ $i -lt 400 ]; do echo "filler line $i ................"; i=$((i+1)); done; echo "token=` + secret + `"; echo "END-OF-OUTPUT"`
	_, ship := runMCV(t, mcvCfg(dispatch, cmd),
		mergeCandidateVerifyRequest{Branch: "a", ExpectedHeadSHA: tipA, Cause: "base_advance"})
	if strings.Contains(ship.MergeCandidateOutputTail, secret) {
		t.Errorf("the shipped tail carries the secret: %q", ship.MergeCandidateOutputTail)
	}
	if n := len(ship.MergeCandidateOutputTail); n > upload.MaxMergeCandidateOutputTailBytes || n == 0 {
		t.Errorf("tail length = %d, want 1..%d", n, upload.MaxMergeCandidateOutputTailBytes)
	}
	if !strings.Contains(ship.MergeCandidateOutputTail, "END-OF-OUTPUT") {
		t.Errorf("tail must keep the END of the output: %q", ship.MergeCandidateOutputTail)
	}
}

// TestMergeCandidateTail_RedactsBeforeTheCut: a secret straddling the bound is
// redacted on the whole output first, so no half of it survives the cut.
func TestMergeCandidateTail_RedactsBeforeTheCut(t *testing.T) {
	secret := "ghp_" + strings.Repeat("Z9y8X7w6V5", 4)[:36]
	pad := strings.Repeat("x", upload.MaxMergeCandidateOutputTailBytes-20)
	got := mergeCandidateTail(secret + pad)
	if strings.Contains(got, secret[18:]) {
		t.Errorf("the surviving half of a cut secret shipped: %q", got[:40])
	}
}

// TestMergeCandidateVerify_NotExecutedArms: every path that never judged the
// tree reports not_executed — never failed — and ships exactly once.
func TestMergeCandidateVerify_NotExecutedArms(t *testing.T) {
	dispatch, tipA, _, _ := mcvRepo(t)

	t.Run("no_verify_command", func(t *testing.T) {
		counter := withMCVVerifyCounter(t)
		_, ship := runMCV(t, mcvCfg(dispatch, ""),
			mergeCandidateVerifyRequest{Branch: "a", ExpectedHeadSHA: tipA, Cause: "base_advance"})
		if ship.MergeCandidateResult != mergeCandidateNotExecuted || ship.MergeCandidateReason != mcReasonNoVerifyCommand {
			t.Errorf("result/reason = %q/%q", ship.MergeCandidateResult, ship.MergeCandidateReason)
		}
		if counter.calls != 0 {
			t.Errorf("verify invocations = %d, want 0", counter.calls)
		}
	})
	t.Run("not_a_work_tree", func(t *testing.T) {
		_, ship := runMCV(t, mcvCfg(t.TempDir(), mcvVerifyCmd),
			mergeCandidateVerifyRequest{Branch: "a", ExpectedHeadSHA: tipA, Cause: "base_advance"})
		if ship.MergeCandidateResult != mergeCandidateNotExecuted || !strings.HasPrefix(ship.MergeCandidateReason, mcReasonTreeUnavailable) {
			t.Errorf("result/reason = %q/%q", ship.MergeCandidateResult, ship.MergeCandidateReason)
		}
	})
	t.Run("fetch_failed_redacted", func(t *testing.T) {
		secret := "ghp_" + strings.Repeat("Q1w2E3r4T5", 4)[:36]
		orig := fetchMergeCandidateTip
		fetchMergeCandidateTip = func(context.Context, string, string, string, string) (string, error) {
			return "", errors.New("fatal: could not read from https://x-access-token:" + secret + "@github.com/acme/widgets")
		}
		t.Cleanup(func() { fetchMergeCandidateTip = orig })
		counter := withMCVVerifyCounter(t)
		_, ship := runMCV(t, mcvCfg(dispatch, mcvVerifyCmd),
			mergeCandidateVerifyRequest{Branch: "a", ExpectedHeadSHA: tipA, Cause: "base_advance"})
		if ship.MergeCandidateResult != mergeCandidateNotExecuted || !strings.HasPrefix(ship.MergeCandidateReason, mcReasonBranchFetchFailed) {
			t.Errorf("result/reason = %q/%q", ship.MergeCandidateResult, ship.MergeCandidateReason)
		}
		if strings.Contains(ship.MergeCandidateReason, secret) {
			t.Errorf("the fetch error's credential shipped: %q", ship.MergeCandidateReason)
		}
		if counter.calls != 0 {
			t.Errorf("verify invocations = %d, want 0", counter.calls)
		}
	})
	for _, tc := range []struct {
		name    string
		outcome string
		disp    gateDisposition
		out     string
		want    string
	}{
		{"refused", "failed", gateRefused, "refused", mcReasonGateRefused},
		{"unavailable", "failed", gateUnavailable, "host", mcReasonGateUnavailable},
		{"timed_out", "failed", gateTimedOut, "killed", mcReasonGateTimedOut},
		{"skipped", "skipped", gateExecuted, "", mcReasonGateSkipped},
		{"lock_contended", "failed", gateExecuted, verifyLockRefusalLinePrefix + "another RUNNER verify still holds the lock\n", mcReasonLockContended},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := mergeCandidateVerifyFn
			mergeCandidateVerifyFn = func(context.Context, string, string, string, time.Duration, []string) (agent.Event, string, string, gateDisposition) {
				return agent.Event{}, tc.out, tc.outcome, tc.disp
			}
			t.Cleanup(func() { mergeCandidateVerifyFn = orig })
			_, ship := runMCV(t, mcvCfg(dispatch, mcvVerifyCmd),
				mergeCandidateVerifyRequest{Branch: "a", ExpectedHeadSHA: tipA, Cause: "base_advance"})
			if ship.MergeCandidateResult != mergeCandidateNotExecuted || ship.MergeCandidateReason != tc.want {
				t.Errorf("result/reason = %q/%q, want not_executed/%s", ship.MergeCandidateResult, ship.MergeCandidateReason, tc.want)
			}
		})
	}
}

// TestMergeCandidateVerify_ReportFailureExitsFailure: an undelivered report is
// the one exitFailure path, so the stage is reaped rather than silently "ok".
func TestMergeCandidateVerify_ReportFailureExitsFailure(t *testing.T) {
	dispatch, tipA, _, _ := mcvRepo(t)
	installGateState(t, nil)
	client := &crFakeUpload{err: errors.New("backend down")}
	code := runMergeCandidateVerifyStage(context.Background(), mcvCfg(dispatch, mcvVerifyCmd),
		mergeCandidateVerifyRequest{Branch: "a", ExpectedHeadSHA: tipA, Cause: "base_advance"},
		client, &upload.IssuedKey{PrivateKey: make([]byte, 64)}, io.Discard)
	if code != exitFailure {
		t.Errorf("exit = %d, want exitFailure on an undelivered report", code)
	}
	if len(client.ships) != 1 {
		t.Errorf("ShipPullRequest calls = %d, want 1", len(client.ships))
	}
}

// TestMergeCandidateVerifyFromPrompt: only a fully populated instruction is a
// pass; a half-populated one is NO pass.
func TestMergeCandidateVerifyFromPrompt(t *testing.T) {
	full := func() *upload.FetchedPrompt {
		return &upload.FetchedPrompt{MergeCandidateVerify: true, MergeCandidateVerifyBranch: "b",
			MergeCandidateVerifyExpectedHeadSHA: "abc", MergeCandidateVerifyCause: "fan_in"}
	}
	if got := mergeCandidateVerifyFromPrompt(full()); got == nil ||
		*got != (mergeCandidateVerifyRequest{Branch: "b", ExpectedHeadSHA: "abc", Cause: "fan_in"}) {
		t.Errorf("full instruction = %+v", got)
	}
	for name, mutate := range map[string]func(p *upload.FetchedPrompt){
		"flag_off":    func(p *upload.FetchedPrompt) { p.MergeCandidateVerify = false },
		"no_branch":   func(p *upload.FetchedPrompt) { p.MergeCandidateVerifyBranch = "" },
		"no_head":     func(p *upload.FetchedPrompt) { p.MergeCandidateVerifyExpectedHeadSHA = "" },
		"no_cause":    func(p *upload.FetchedPrompt) { p.MergeCandidateVerifyCause = "" },
		"nil_payload": nil,
	} {
		var p *upload.FetchedPrompt
		if mutate != nil {
			p = full()
			mutate(p)
		}
		if got := mergeCandidateVerifyFromPrompt(p); got != nil {
			t.Errorf("%s: got %+v, want nil", name, got)
		}
	}
}

// TestRun_MergeCandidateVerifyRoutesBeforeLineage drives the real run() with a
// served instruction: the pass reports once, the agent is NEVER invoked,
// nothing is pushed, and the lineage-worktree block is never reached — the
// operator checkout's worktree list and refs are byte-identical (#3973).
func TestRun_MergeCandidateVerifyRoutesBeforeLineage(t *testing.T) {
	dispatch, tipA, _, _ := mcvRepo(t)
	implementEnv(t, "acme/widgets", "main")
	installGateState(t, nil)
	withFakeInvoker(t, &fakeInvoker{
		canned:   agent.Result{OK: true},
		onInvoke: func(int, agent.Invocation) { t.Error("the agent was invoked on a verify-only pass") },
	})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID: "stage-mcv", StageType: "implement", Prompt: "implement", PromptHash: "h",
		VerifyCommand:                       mcvVerifyCmd,
		VerifyTimeoutSeconds:                60,
		MergeCandidateVerify:                true,
		MergeCandidateVerifyBranch:          "a",
		MergeCandidateVerifyExpectedHeadSHA: tipA,
		MergeCandidateVerifyCause:           "base_advance",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	origPusher := newPusher
	newPusher = func() pusher { return fp }
	t.Cleanup(func() { newPusher = origPusher })

	before := mcvRepoState(t, dispatch)
	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "stage-mcv",
		"--working-dir", dispatch,
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if len(fu.gotPRCalls) != 1 {
		t.Fatalf("ShipPullRequest calls = %d, want 1:\n%s", len(fu.gotPRCalls), stderr.String())
	}
	ship := fu.gotPRCalls[0]
	if ship.Outcome != upload.OutcomeMergeCandidateVerified || ship.MergeCandidateResult != mergeCandidatePassed || ship.HeadSHA != tipA {
		t.Errorf("shipped %s/%s at %s, want merge_candidate_verified/passed at %s",
			ship.Outcome, ship.MergeCandidateResult, ship.HeadSHA, tipA)
	}
	if fp.calls != 0 || fp.pushCommittedArgs != nil {
		t.Error("the verify-only pass reached the pusher")
	}
	if after := mcvRepoState(t, dispatch); after != before {
		t.Errorf("the operator checkout changed (lineage provisioning reached?):\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if lines := logLines(stderr.String(), "merge_candidate_verify_reported"); len(lines) != 1 {
		t.Errorf("merge_candidate_verify_reported lines = %d, want 1", len(lines))
	}
}
