package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
)

// #5172: a committed-tree verify gate whose pre-verify git plumbing (stage
// scope-only / throwaway commit / rev-parse) fails never ran the verify
// command, so it FAILS CLOSED, category C, in both the single-shot gate and the
// verify-fix loop. Each failure is seeded BY CONSTRUCTION through a seam that
// returns errPlumbingSeeded, so the gate stops at the plumbing arm before any
// verify runs. errPlumbingSeeded deliberately names no step, so every
// assertion that the reason names the step is the runner's own wording.

var errPlumbingSeeded = errors.New("fatal: Unable to create '.git/index.lock': File exists")

// failStageScoped swaps verifyGateStageScopedFn to fail every call.
func failStageScoped(t *testing.T) {
	t.Helper()
	orig := verifyGateStageScopedFn
	t.Cleanup(func() { verifyGateStageScopedFn = orig })
	verifyGateStageScopedFn = func(context.Context, string, []string) ([]string, error) {
		return nil, errPlumbingSeeded
	}
}

// failCommitWIPFrom swaps verifyGateCommitWIPFn so call n (1-based) and every
// later call fail; earlier calls make the REAL throwaway commit.
func failCommitWIPFrom(t *testing.T, n int) {
	t.Helper()
	orig := verifyGateCommitWIPFn
	t.Cleanup(func() { verifyGateCommitWIPFn = orig })
	calls := 0
	verifyGateCommitWIPFn = func(ctx context.Context, cfg config, repoDir string) (bool, error) {
		calls++
		if calls >= n {
			return false, errPlumbingSeeded
		}
		return orig(ctx, cfg, repoDir)
	}
}

// failRevParseHEAD swaps verifyGateRevParseHEADFn to fail every call. The real
// throwaway commit has already landed when it runs.
func failRevParseHEAD(t *testing.T) {
	t.Helper()
	orig := verifyGateRevParseHEADFn
	t.Cleanup(func() { verifyGateRevParseHEADFn = orig })
	verifyGateRevParseHEADFn = func(context.Context, string) (string, error) {
		return "", errPlumbingSeeded
	}
}

// assertGatePlumbingFailure checks the single-shot gate's category-C return
// for one plumbing step.
func assertGatePlumbingFailure(t *testing.T, evs []agent.Event, tree string, err error, step string) {
	t.Helper()
	if !errors.Is(err, gitops.ErrVerifyInfraFailure) || !errors.Is(err, errVerifyGateSkipped) {
		t.Fatalf("err = %v, want ErrVerifyInfraFailure + errVerifyGateSkipped", err)
	}
	if got := committedGateFailureCategory(err); got != "C" {
		t.Errorf("committedGateFailureCategory = %q, want C", got)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, verifyGateSkippedLead+": ") || !strings.Contains(msg, step) || !strings.Contains(msg, errPlumbingSeeded.Error()) {
		t.Errorf("error = %q, want a %s lead naming %s and the injected cause", msg, verifyGateSkippedLead, step)
	}
	if tree != "" {
		t.Errorf("tree = %q, want empty (no verdict)", tree)
	}
	if len(evs) != 1 || evs[0].Kind != "verify_run" || !strings.Contains(string(evs[0].Payload), `"outcome":"skipped"`) {
		t.Fatalf("events = %+v, want exactly one skipped verify_run", evs)
	}
	if !strings.Contains(string(evs[0].Payload), step+": ") {
		t.Errorf("skipped verify_run = %s, want its output to name %s", evs[0].Payload, step)
	}
}

func TestRunVerifyGateCommitted_StageScopedFailureIsCategoryC(t *testing.T) {
	repo, _, _ := verifiedTreeRepo(t)
	base := gitHead(t, repo)
	cmd, counter := exitSeqVerifyCmd(t, "verify ran", 0)
	failStageScoped(t)

	evs, tree, err := runVerifyGateCommitted(context.Background(), verifiedTreeCfg(repo, cmd), io.Discard)
	assertGatePlumbingFailure(t, evs, tree, err, "stage_scoped")
	if n := readInvocations(t, counter); n != 0 {
		t.Errorf("verify invocations = %d, want 0", n)
	}
	if got := gitHead(t, repo); got != base {
		t.Errorf("HEAD = %s, want unchanged %s", got, base)
	}
}

func TestRunVerifyGateCommitted_CommitWIPFailureIsCategoryC(t *testing.T) {
	repo, _, _ := verifiedTreeRepo(t)
	base := gitHead(t, repo)
	cmd, counter := exitSeqVerifyCmd(t, "verify ran", 0)
	failCommitWIPFrom(t, 1)

	evs, tree, err := runVerifyGateCommitted(context.Background(), verifiedTreeCfg(repo, cmd), io.Discard)
	assertGatePlumbingFailure(t, evs, tree, err, "commit_wip")
	if n := readInvocations(t, counter); n != 0 {
		t.Errorf("verify invocations = %d, want 0", n)
	}
	if got := gitHead(t, repo); got != base {
		t.Errorf("HEAD = %s, want unchanged %s", got, base)
	}
}

func TestRunVerifyGateCommitted_RevParseFailureIsCategoryC(t *testing.T) {
	repo, _, _ := verifiedTreeRepo(t)
	base := gitHead(t, repo)
	cmd, counter := exitSeqVerifyCmd(t, "verify ran", 0)
	failRevParseHEAD(t)

	evs, tree, err := runVerifyGateCommitted(context.Background(), verifiedTreeCfg(repo, cmd), io.Discard)
	assertGatePlumbingFailure(t, evs, tree, err, "rev_parse")
	if n := readInvocations(t, counter); n != 0 {
		t.Errorf("verify invocations = %d, want 0", n)
	}
	// The real throwaway commit landed and was undone: HEAD is back on the base
	// and a.txt is still staged for the real push.
	if got := gitHead(t, repo); got != base {
		t.Errorf("HEAD = %s, want the pre-gate base %s (throwaway undone)", got, base)
	}
	out, gerr := exec.Command("git", "-C", repo, "diff", "--cached", "--name-only").Output()
	if gerr != nil {
		t.Fatal(gerr)
	}
	if strings.TrimSpace(string(out)) != "a.txt" {
		t.Errorf("staged = %q, want a.txt (index preserved)", out)
	}
}

func TestRunVerifyFixLoop_PlumbingFailureIsCategoryC(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(t *testing.T)
		step string
	}{
		{"stage_scoped", failStageScoped, "stage scoped"},
		{"commit_wip", func(t *testing.T) { failCommitWIPFrom(t, 1) }, "commit wip"},
		{"rev_parse", failRevParseHEAD, "rev-parse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, _, _ := verifiedTreeRepo(t)
			cmd, counter := exitSeqVerifyCmd(t, "verify ran", 0)
			cfg := verifiedTreeCfg(repo, cmd)
			cfg.verifyMaxIterations = 1
			tc.seed(t)
			res := agent.Result{OK: true}
			invoker := &fakeInvoker{canned: agent.Result{OK: true}}
			var logSink strings.Builder

			_, tree, err := runVerifyFixLoop(context.Background(), &cfg, nil, "", invoker, agent.Invocation{}, &res, &logSink)
			if err != nil {
				t.Fatalf("err = %v, want nil (the demotion rides res)", err)
			}
			if tree != "" {
				t.Errorf("tree = %q, want empty", tree)
			}
			if res.OK || res.FailureCategory != "C" {
				t.Errorf("res OK=%t category=%q, want false/C", res.OK, res.FailureCategory)
			}
			if !strings.HasPrefix(res.FailureReason, verifyGateSkippedLead+": ") || !strings.Contains(res.FailureReason, tc.step) {
				t.Errorf("FailureReason = %q, want a %s lead naming %q", res.FailureReason, verifyGateSkippedLead, tc.step)
			}
			if invoker.callIdx != 0 {
				t.Errorf("fix invocations = %d, want 0", invoker.callIdx)
			}
			if n := readInvocations(t, counter); n != 0 {
				t.Errorf("verify invocations = %d, want 0", n)
			}
			var summary map[string]any
			for _, ev := range res.Events {
				if ev.Kind == "verify_summary" {
					if err := json.Unmarshal(ev.Payload, &summary); err != nil {
						t.Fatal(err)
					}
				}
			}
			if summary["outcome"] != "skipped" {
				t.Errorf("verify_summary = %v, want outcome skipped", summary)
			}
			if !strings.Contains(logSink.String(), `"event":"verify_fix_skipped"`) {
				t.Errorf("verify_fix_skipped not logged:\n%s", logSink.String())
			}
		})
	}
}

// runPlumbingStage drives run() over gateFailRepo with fakes it returns, so
// the caller can assert no push and no PR.
func runPlumbingStage(t *testing.T, repo string, fu *fakeUploader) (int, string, *fakePusher, *fakePROpener) {
	t.Helper()
	withFakeUploader(t, fu)
	fp, fpr := &fakePusher{}, &fakePROpener{}
	withFakeGitOps(t, fp, fpr)
	var stderr strings.Builder
	code := run(verifyFixRunArgs(repo, filepath.Join(t.TempDir(), "trace.jsonl.gz")), &stderr)
	return code, stderr.String(), fp, fpr
}

// assertPlumbingStageFailed checks run()'s 'stage does not succeed' contract:
// exitFailure, category C (never B), exactly one category-C failure report whose
// reason leads with verifyGateSkippedLead, and no push or PR.
func assertPlumbingStageFailed(t *testing.T, code int, log string, fu *fakeUploader, fp *fakePusher, fpr *fakePROpener) {
	t.Helper()
	if code != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", code, log)
	}
	if !strings.Contains(log, `"category":"C"`) || strings.Contains(log, `"category":"B"`) {
		t.Errorf("want category C and never B:\n%s", log)
	}
	if fp.calls != 0 || fp.gotArgs != nil || fpr.gotArgs != nil {
		t.Errorf("push calls=%d PR opened=%t, want no push and no PR", fp.calls, fpr.gotArgs != nil)
	}
	reports := failedReports(fu)
	if len(reports) != 1 {
		t.Fatalf("failed /pull-request reports = %d, want 1:\n%s", len(reports), log)
	}
	if got := reports[0]; got.Category != "C" || !strings.HasPrefix(got.Reason, verifyGateSkippedLead+": ") {
		t.Errorf("report category=%q reason=%.200q, want C with a %s lead", got.Category, got.Reason, verifyGateSkippedLead)
	}
}

func TestRun_VerifyGateCommitted_PlumbingFailureFailsStage(t *testing.T) {
	repo := gateFailRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{mirrorWorkingTreeFrom: repo, canned: agent.Result{OK: true}})
	cmd, _ := formVerifyCmd(t, "ok\n", 0, "ok\n", 0)
	fu := newFakeUploader(t)
	fu.promptResp = gateFailPrompt(cmd, 0) // single-shot committed gate
	failCommitWIPFrom(t, 1)

	code, log, fp, fpr := runPlumbingStage(t, repo, fu)
	assertPlumbingStageFailed(t, code, log, fu, fp, fpr)
	if reason := failedReports(fu)[0].Reason; !strings.Contains(reason, "commit_wip") {
		t.Errorf("reason = %.300q, want it to name commit_wip", reason)
	}
}

func TestRun_VerifyFixLoop_PlumbingFailureFailsStage(t *testing.T) {
	repo := gateFailRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{mirrorWorkingTreeFrom: repo, canned: agent.Result{OK: true}})
	cmd, _ := formVerifyCmd(t, "ok\n", 0, "ok\n", 0)
	fu := newFakeUploader(t)
	fu.promptResp = gateFailPrompt(cmd, 1) // the verify-fix loop
	failCommitWIPFrom(t, 1)

	code, log, fp, fpr := runPlumbingStage(t, repo, fu)
	assertPlumbingStageFailed(t, code, log, fu, fp, fpr)
	if reason := failedReports(fu)[0].Reason; !strings.Contains(reason, "commit wip") {
		t.Errorf("reason = %.300q, want it to name the commit wip step", reason)
	}
}

// TestRun_VerifyFixLoop_PlumbingFailureAfterRedArmsNoCheckpoint is approval
// condition C1: iteration 0's verify is RED, the fix re-invoke runs and edits
// a.go, then iteration 1's throwaway commit fails (the seam fails only its
// SECOND call). The stage fails category C, but the only committed verify head
// is the red one, so it must NOT be pinned or armed as a reverify checkpoint:
// a retry re-runs the agent rather than re-verifying a known-red commit.
func TestRun_VerifyFixLoop_PlumbingFailureAfterRedArmsNoCheckpoint(t *testing.T) {
	repo := gateFailRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	invoker := &fakeInvoker{
		mirrorWorkingTreeFrom: repo,
		canned:                agent.Result{OK: true},
		onInvoke: func(idx int, _ agent.Invocation) {
			if idx == 1 { // the fix re-invoke
				mustWrite(t, filepath.Join(repo, "pkg", "a", "a.go"), "package a\n\n// agent change\n// fix edit\n")
			}
		},
	}
	withFakeInvoker(t, invoker)
	cmd, calls := formVerifyCmd(t, "--- FAIL: TestA (0.00s)\nFAIL\n", 1, "--- FAIL: TestA (0.00s)\nFAIL\n", 1)
	fu := newFakeUploader(t)
	fu.promptResp = gateFailPrompt(cmd, 1) // SupportsPushResume: a checkpoint MAY be armed
	failCommitWIPFrom(t, 2)

	code, log, fp, fpr := runPlumbingStage(t, repo, fu)
	// Fixture preconditions: the fix re-invoke ran after exactly one red verify.
	if invoker.callIdx != 2 {
		t.Fatalf("fixture: agent invocations = %d, want 2 (agent pass + one fix re-invoke):\n%s", invoker.callIdx, log)
	}
	callsLog, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("fixture: no verify ran: %v\n%s", err, log)
	}
	if n := strings.Count(string(callsLog), "\n"); n != 1 {
		t.Fatalf("fixture: verify calls = %d, want 1 (iteration 0 only):\n%s", n, log)
	}
	assertPlumbingStageFailed(t, code, log, fu, fp, fpr)
	assertNoReverifyArmed(t, repo, fu, log, verifyPinSkipRedHeadUnjudged)
}

// TestVerifyHeadJudgedRedThenUnjudged pins the C1 predicate's rows: only a
// skipped summary over a red committed head withholds the pin.
func TestVerifyHeadJudgedRedThenUnjudged(t *testing.T) {
	vr := func(head, outcome string) agent.Event { return verifyRunEvent("v", head, "", 1, "", outcome) }
	summary := func(outcome string) agent.Event {
		return agent.Event{Kind: "verify_summary", Payload: agent.MakePayload(map[string]any{"outcome": outcome})}
	}
	for _, tc := range []struct {
		name   string
		events []agent.Event
		want   bool
	}{
		{"plumbing after red", []agent.Event{vr("h0", "failed"), summary("skipped")}, true},
		{"gate never ran (#3983)", []agent.Event{vr("h0", "failed"), vr("h1", "skipped"), summary("skipped")}, false},
		{"red verdict", []agent.Event{vr("h0", "failed"), summary("failed")}, false},
		{"plumbing on iteration 0", []agent.Event{summary("skipped")}, false},
		{"single-shot (no summary)", []agent.Event{vr("h0", "failed"), vr("", "skipped")}, false},
		{"headless skip ignored", []agent.Event{vr("h0", "failed"), vr("", "skipped"), summary("skipped")}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := verifyHeadJudgedRedThenUnjudged(tc.events); got != tc.want {
				t.Errorf("= %t, want %t", got, tc.want)
			}
		})
	}
}
