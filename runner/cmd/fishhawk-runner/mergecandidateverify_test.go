package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	return runMCVWithState(t, cfg, req, nil)
}

// runMCVWithState is runMCV with st installed as the gate isolation state
// (nil = the unconfigured host exec runMCV uses).
func runMCVWithState(t *testing.T, cfg config, req mergeCandidateVerifyRequest, st *gateIsolationState) (int, upload.ShipPullRequestArgs) {
	t.Helper()
	installGateState(t, st)
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

// TestMergeCandidateEvidence_RedactsBeforeTheCut: a secret straddling the
// tail bound is redacted on the whole output first, so no half of it survives
// the cut.
func TestMergeCandidateEvidence_RedactsBeforeTheCut(t *testing.T) {
	secret := "ghp_" + strings.Repeat("Z9y8X7w6V5", 4)[:36]
	pad := strings.Repeat("x", upload.MaxMergeCandidateOutputTailBytes-20)
	got := mergeCandidateEvidence(secret+pad, mergeCandidateFailed).tail
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
		// The exit-code dispositions (#4022) classify with SIGNATURE-FREE output,
		// so only the disposition arm can make them not_executed: without it
		// both fall to the default verify_failed.
		{"temp_fail_exit_75", "failed", gateTempFail, "", mcReasonTempFail},
		{"lock_refused_exit_73", "failed", gateLockRefused, "", mcReasonLockContended},
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
	// The #3948 text classifier is deleted (#4022): scripts/test's refusal
	// TEXT from an executed gate that did NOT exit 73 is an ordinary failure,
	// so a regression that re-adds text scraping reddens this.
	t.Run("refusal_text_without_exit_73_is_verify_failed", func(t *testing.T) {
		result, reason := classifyMergeCandidateVerify("failed", gateExecuted,
			"scripts/test: another RUNNER verify still holds the lock at /l after 600s\n")
		if result != mergeCandidateFailed || reason != "verify_failed" {
			t.Errorf("result/reason = %q/%q, want %s/verify_failed", result, reason, mergeCandidateFailed)
		}
	})
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

// --- #4250: failure evidence from the WHOLE output ---

// withMCVVerifyOutput stubs the verify seam to return out with the given
// gate outcome.
func withMCVVerifyOutput(t *testing.T, out, outcome string) {
	t.Helper()
	orig := mergeCandidateVerifyFn
	mergeCandidateVerifyFn = func(context.Context, string, string, string, time.Duration, []string) (agent.Event, string, string, gateDisposition) {
		return agent.Event{}, out, outcome, gateExecuted
	}
	t.Cleanup(func() { mergeCandidateVerifyFn = orig })
}

// mcvEarlyFailOutput is shaped like `scripts/test verify`: an EARLY module's
// FAIL block, then ~100 KB of later-module ok lines, then the module summary.
func mcvEarlyFailOutput(assertion string) string {
	var b strings.Builder
	b.WriteString(">>> ./backend\n")
	b.WriteString("--- FAIL: TestEarlyThing (0.01s)\n")
	b.WriteString("    " + assertion + "\n")
	b.WriteString("FAIL\n")
	b.WriteString("FAIL\tgithub.com/x/backend/internal/early\t0.5s\n")
	for b.Len() < 100*1024 {
		b.WriteString("ok  \tgithub.com/x/runner/internal/later\t0.1s\n")
	}
	b.WriteString("scripts/test: 1 of 8 modules failed:\n  - ./backend\n")
	return b.String()
}

// TestMergeCandidateVerify_FailuresNameEarlyFailureUnderLateOkLines is the
// #4250 done-means: an early module's FAIL scrolls out of the 4 KiB tail, and
// the SINGLE shipped report still names the failing test, its assertion line
// and its package in MergeCandidateFailures, and in the parsed summary.
func TestMergeCandidateVerify_FailuresNameEarlyFailureUnderLateOkLines(t *testing.T) {
	dispatch, tipA, _, _ := mcvRepo(t)
	withMCVVerifyOutput(t, mcvEarlyFailOutput("early_test.go:42: want 1, got 2"), "failed")
	_, ship := runMCV(t, mcvCfg(dispatch, mcvVerifyCmd),
		mergeCandidateVerifyRequest{Branch: "a", ExpectedHeadSHA: tipA, Cause: "base_advance"})
	if ship.MergeCandidateResult != mergeCandidateFailed {
		t.Fatalf("result = %q (%s), want failed", ship.MergeCandidateResult, ship.MergeCandidateReason)
	}
	// Precondition: the fixture reproduces the incident.
	if strings.Contains(ship.MergeCandidateOutputTail, "TestEarlyThing") ||
		!strings.HasSuffix(ship.MergeCandidateOutputTail, "  - ./backend\n") {
		t.Fatalf("precondition: the 4 KiB tail must exclude the early FAIL and end with the module summary")
	}
	for _, want := range []string{"--- FAIL: TestEarlyThing", "early_test.go:42: want 1, got 2", "FAIL\tgithub.com/x/backend/internal/early"} {
		if !strings.Contains(ship.MergeCandidateFailures, want) {
			t.Errorf("MergeCandidateFailures missing %q:\n%s", want, ship.MergeCandidateFailures)
		}
	}
	if n := len(ship.MergeCandidateFailures); n > upload.MaxMergeCandidateFailuresBytes {
		t.Errorf("MergeCandidateFailures = %d bytes, over %d", n, upload.MaxMergeCandidateFailuresBytes)
	}
	want := &upload.MergeCandidateFailureSummary{
		Packages: []string{"github.com/x/backend/internal/early"}, Tests: []string{"TestEarlyThing"},
	}
	if !reflect.DeepEqual(ship.MergeCandidateFailureSummary, want) {
		t.Errorf("summary = %+v, want %+v", ship.MergeCandidateFailureSummary, want)
	}
}

// TestMergeCandidateVerify_BuildFailedKeepsCompilerError is approval condition
// C1: a package fails `[build failed]` and its multi-line compiler error sits
// FAR above the FAIL line (parallel builds); the shipped excerpt retains the
// compiler error lines and the summary lists the package as build-failed.
func TestMergeCandidateVerify_BuildFailedKeepsCompilerError(t *testing.T) {
	dispatch, tipA, _, _ := mcvRepo(t)
	compile1 := "internal/early/early.go:12:2: undefined: frobnicate"
	compile2 := "internal/early/early.go:13:9: cannot use x (variable of type int) as string value in return statement"
	out := ">>> ./backend\n" +
		"# github.com/x/backend/internal/early\n" + compile1 + "\n" + compile2 + "\n" +
		strings.Repeat("ok  \tgithub.com/x/backend/internal/other\t0.1s\n", 200) +
		"FAIL\tgithub.com/x/backend/internal/early [build failed]\n" +
		strings.Repeat("ok  \tgithub.com/x/runner/internal/later\t0.1s\n", 200) +
		"scripts/test: 1 of 8 modules failed:\n  - ./backend\n"
	withMCVVerifyOutput(t, out, "failed")
	_, ship := runMCV(t, mcvCfg(dispatch, mcvVerifyCmd),
		mergeCandidateVerifyRequest{Branch: "a", ExpectedHeadSHA: tipA, Cause: "base_advance"})
	for _, want := range []string{compile1, compile2, "FAIL\tgithub.com/x/backend/internal/early [build failed]"} {
		if !strings.Contains(ship.MergeCandidateFailures, want) {
			t.Errorf("MergeCandidateFailures missing %q:\n%s", want, ship.MergeCandidateFailures)
		}
	}
	if s := ship.MergeCandidateFailureSummary; s == nil || !reflect.DeepEqual(s.BuildFailed, []string{"github.com/x/backend/internal/early"}) {
		t.Errorf("summary = %+v, want build_failed naming the package", s)
	}
}

// TestMergeCandidateVerify_FailuresPerMode: a passed pass ships no failure
// evidence even when its output carries a FAIL-shaped line; a failed pass
// whose output has no signal line ships none either, with the tail unchanged.
func TestMergeCandidateVerify_FailuresPerMode(t *testing.T) {
	dispatch, tipA, _, _ := mcvRepo(t)
	req := mergeCandidateVerifyRequest{Branch: "a", ExpectedHeadSHA: tipA, Cause: "base_advance"}

	t.Run("passed ships none", func(t *testing.T) {
		withMCVVerifyOutput(t, "--- FAIL: TestLooksRed (0.00s)\nFAIL\tm/x\t0.1s\nok\n", "passed")
		_, ship := runMCV(t, mcvCfg(dispatch, mcvVerifyCmd), req)
		if ship.MergeCandidateResult != mergeCandidatePassed {
			t.Fatalf("result = %q", ship.MergeCandidateResult)
		}
		if ship.MergeCandidateFailures != "" || ship.MergeCandidateFailureSummary != nil {
			t.Errorf("a passed pass shipped failure evidence: %q / %+v", ship.MergeCandidateFailures, ship.MergeCandidateFailureSummary)
		}
	})
	t.Run("failed without a signal line ships none", func(t *testing.T) {
		out := "scripts/test: schema-sync drift: backend/internal/spec/schemas/workflow-v2.schema.json differs\n"
		withMCVVerifyOutput(t, out, "failed")
		_, ship := runMCV(t, mcvCfg(dispatch, mcvVerifyCmd), req)
		if ship.MergeCandidateResult != mergeCandidateFailed {
			t.Fatalf("result = %q", ship.MergeCandidateResult)
		}
		if ship.MergeCandidateFailures != "" || ship.MergeCandidateFailureSummary != nil {
			t.Errorf("no-signal failure shipped evidence: %q / %+v", ship.MergeCandidateFailures, ship.MergeCandidateFailureSummary)
		}
		if ship.MergeCandidateOutputTail != out {
			t.Errorf("tail = %q, want the whole short output", ship.MergeCandidateOutputTail)
		}
	})
}

// TestMergeCandidateVerify_FailuresAreRedacted: a token on the assertion line —
// inside the excerpt's context window — never ships in MergeCandidateFailures
// or the summary; only redaction-before-extraction removes it.
func TestMergeCandidateVerify_FailuresAreRedacted(t *testing.T) {
	dispatch, tipA, _, _ := mcvRepo(t)
	secret := "ghp_" + strings.Repeat("M1n2B3v4C5", 4)[:36]
	withMCVVerifyOutput(t, mcvEarlyFailOutput("early_test.go:42: token="+secret+" rejected"), "failed")
	_, ship := runMCV(t, mcvCfg(dispatch, mcvVerifyCmd),
		mergeCandidateVerifyRequest{Branch: "a", ExpectedHeadSHA: tipA, Cause: "base_advance"})
	if !strings.Contains(ship.MergeCandidateFailures, "early_test.go:42: token=") {
		t.Fatalf("precondition: the assertion line must be inside the excerpt:\n%s", ship.MergeCandidateFailures)
	}
	if strings.Contains(ship.MergeCandidateFailures, secret) {
		t.Errorf("the shipped failures excerpt carries the secret")
	}
}

// TestMergeCandidateEvidence_SummaryTestsAreRedacted: a token carried in a
// `t.Run` subtest NAME lands in the summary's Tests list, so the summary must be
// parsed over the REDACTED output, never the raw one.
func TestMergeCandidateEvidence_SummaryTestsAreRedacted(t *testing.T) {
	secret := "ghp_" + strings.Repeat("K1j2H3g4F5", 4)[:36]
	out := "--- FAIL: TestLogin/" + secret + " (0.00s)\n    login_test.go:7: rejected\n" +
		"FAIL\tgithub.com/x/backend/internal/login\t0.1s\n"
	if red, _ := redactString(out); strings.Contains(red, secret) {
		t.Fatalf("precondition: the default redaction patterns must match the fixture token")
	}
	ev := mergeCandidateEvidence(out, mergeCandidateFailed)
	if ev.summary == nil || len(ev.summary.Tests) != 1 || !strings.HasPrefix(ev.summary.Tests[0], "TestLogin/") {
		t.Fatalf("precondition: the summary must name the subtest, got %+v", ev.summary)
	}
	for _, name := range ev.summary.Tests {
		if strings.Contains(name, secret) {
			t.Errorf("summary test name %q carries the raw token", name)
		}
	}
}

// TestMergeCandidateEvidence_SummarySurvivesTheBudget is approval condition
// C2: when the failure blocks overrun the 8 KiB excerpt, a LATER failing
// package is omitted from the excerpt but still named by the summary, which is
// parsed over the WHOLE redacted output.
func TestMergeCandidateEvidence_SummarySurvivesTheBudget(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "--- FAIL: TestBulk%03d/%s (0.00s)\n    bulk_test.go:%d: want ok\n", i, strings.Repeat("case_", 20), i)
	}
	b.WriteString("FAIL\tgithub.com/x/backend/internal/bulk\t0.5s\n")
	b.WriteString("FAIL\tgithub.com/x/runner/internal/late\t0.2s\n")
	ev := mergeCandidateEvidence(b.String(), mergeCandidateFailed)
	if strings.Contains(ev.failures, "internal/late") {
		t.Fatalf("precondition: the late package must be past the excerpt budget")
	}
	if !strings.Contains(ev.failures, "omitted over the") {
		t.Errorf("an overrun excerpt must end with the omitted marker")
	}
	if ev.summary == nil || !reflect.DeepEqual(ev.summary.Packages,
		[]string{"github.com/x/backend/internal/bulk", "github.com/x/runner/internal/late"}) || !ev.summary.Lossy {
		t.Errorf("summary = %+v, want both packages named and lossy (200 tests > the 10-entry cap)", ev.summary)
	}
}

// TestMergeCandidateVerify_ContainerWorkTreePreflightIsNotExecuted drives the
// REAL chain — runMergeCandidateVerifyStage → mergeCandidateVerify →
// runVerifyCommittedTree → runBoundedGateArgvDisposed → runGateInContainer —
// against the dispatch clone of a real origin, on the container path with
// both runtime seams scripted (#4392). When git cannot see /work (the run
// df957692 shape: the preflight exits 128 for dubious ownership) the gate
// never runs and the pass reports a re-triggerable not_executed /
// verify_gate_unavailable, never failed / verify_failed. The gate_red arm is
// the control: with a healthy preflight the same fixture's red gate reports
// failed / verify_failed, so the first arm is not vacuous. Dropping
// withGitWorkTreeRequired at runVerifyCommittedTreeEnv's seam call turns the
// preflight_fails arm red (the gate's exit 1 is an executed red).
func TestMergeCandidateVerify_ContainerWorkTreePreflightIsNotExecuted(t *testing.T) {
	dispatch, tipA, _, _ := mcvRepo(t)
	redGate := func(label string, n int) (string, int, bool) {
		if label == "gate_run" {
			return "fatal: /work is not a git work tree\n", 1, false
		}
		return serviceHappyPath(label, n)
	}
	for _, tc := range []struct {
		name       string
		preflight  func(int) (string, int, bool)
		wantResult string
		wantReason string
		wantRuns   int
	}{
		{name: "preflight_fails", preflight: func(int) (string, int, bool) {
			return "fatal: detected dubious ownership in repository at '/work'\n", 128, false
		}, wantResult: mergeCandidateNotExecuted, wantReason: mcReasonGateUnavailable, wantRuns: 0},
		{name: "gate_red", wantResult: mergeCandidateFailed, wantReason: "verify_failed", wantRuns: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := containerState("img:1", "/nonexistent/daemon.sock", io.Discard)
			g := scriptGateExec(t, redGate)
			g.preflight = tc.preflight
			code, ship := runMCVWithState(t, mcvCfg(dispatch, mcvVerifyCmd),
				mergeCandidateVerifyRequest{Branch: "a", ExpectedHeadSHA: tipA, Cause: "base_advance"}, st)
			if code != exitOK {
				t.Errorf("exit = %d, want exitOK", code)
			}
			if ship.MergeCandidateResult != tc.wantResult || ship.MergeCandidateReason != tc.wantReason {
				t.Errorf("result/reason = %q/%q, want %s/%s", ship.MergeCandidateResult, ship.MergeCandidateReason, tc.wantResult, tc.wantReason)
			}
			if g.counts["worktree_preflight"] != 1 || g.counts["gate_run"] != tc.wantRuns {
				t.Errorf("preflight runs = %d, gate runs = %d, want 1 / %d: %q", g.counts["worktree_preflight"], g.counts["gate_run"], tc.wantRuns, g.order)
			}
		})
	}
}
