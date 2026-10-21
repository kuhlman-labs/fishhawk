package main

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
)

// --- #4022 verify exit-code contract --------------------------------------
//
// Every case drives a REAL `sh` verify script through runVerifyCommittedTree's
// throwaway clone, so the exit code crosses the same boundary production does.
// The script output is SIGNATURE-FREE (it matches neither isVerifyInfraFailure
// nor any refusal literal), so only the exit-code disposition can classify it.

// exitSeqVerifyCmd writes a verify script whose k-th invocation exits
// codes[k-1] (the last code repeats) after printing a signature-free line. The
// invocation count is recorded in a counter file OUTSIDE the worktree.
func exitSeqVerifyCmd(t *testing.T, out string, codes ...int) (cmd, counter string) {
	t.Helper()
	dir := t.TempDir()
	counter = filepath.Join(dir, "invocations")
	script := filepath.Join(dir, "verify.sh")
	body := "#!/bin/sh\n" +
		"c=$(cat " + counter + " 2>/dev/null || echo 0)\n" +
		"c=$((c + 1))\n" +
		"echo \"$c\" > " + counter + "\n" +
		"echo '" + out + "'\n"
	for i, code := range codes[:len(codes)-1] {
		body += "if [ \"$c\" -eq " + strconv.Itoa(i+1) + " ]; then exit " + strconv.Itoa(code) + "; fi\n"
	}
	body += "exit " + strconv.Itoa(codes[len(codes)-1]) + "\n"
	mustWrite(t, script, body)
	return "sh " + script, counter
}

const signatureFreeOut = "fixture: verify ran"

func TestVerifyExitDisposition(t *testing.T) {
	for _, tc := range []struct {
		code int
		in   gateDisposition
		want gateDisposition
	}{
		{0, gateExecuted, gateExecuted},
		{1, gateExecuted, gateExecuted},
		{2, gateExecuted, gateExecuted},
		{verifyExitTempFail, gateExecuted, gateTempFail},
		{verifyExitLockRefused, gateExecuted, gateLockRefused},
		{74, gateExecuted, gateExecuted},
		{76, gateExecuted, gateExecuted},
		// Only an EXECUTED gate is remapped.
		{verifyExitTempFail, gateTimedOut, gateTimedOut},
		{verifyExitLockRefused, gateRefused, gateRefused},
		{verifyExitTempFail, gateUnavailable, gateUnavailable},
		{verifyExitLockRefused, gateCheckoutRefused, gateCheckoutRefused},
	} {
		if got := verifyExitDisposition(tc.code, tc.in); got != tc.want {
			t.Errorf("verifyExitDisposition(%d, %s) = %s, want %s", tc.code, tc.in, got, tc.want)
		}
	}
}

func TestRunVerifyCommittedTree_ExitCodesMapToDisposition(t *testing.T) {
	for _, tc := range []struct {
		code int
		want gateDisposition
	}{
		{verifyExitTempFail, gateTempFail},
		{verifyExitLockRefused, gateLockRefused},
		{1, gateExecuted},
	} {
		repo, _, _ := verifiedTreeRepo(t)
		cmd, _ := exitSeqVerifyCmd(t, signatureFreeOut, tc.code)
		head, err := gitRevParseHEAD(context.Background(), repo)
		if err != nil {
			t.Fatal(err)
		}
		_, _, outcome, disp := runVerifyCommittedTree(context.Background(), cmd, repo, head, time.Minute, nil)
		if outcome != "failed" || disp != tc.want {
			t.Errorf("exit %d: outcome/disp = %q/%s, want failed/%s", tc.code, outcome, disp, tc.want)
		}
	}
}

// runFixLoop runs the verify-fix loop over the standard verified-tree fixture.
func runFixLoop(t *testing.T, cmd string) (agent.Result, *fakeInvoker, bool, string, string) {
	t.Helper()
	repo, _, _ := verifiedTreeRepo(t)
	cfg := verifiedTreeCfg(repo, cmd)
	cfg.verifyMaxIterations = 1
	res := agent.Result{OK: true}
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	var logSink strings.Builder
	reinvoked, tree, err := runVerifyFixLoop(context.Background(), &cfg, nil, "", invoker, agent.Invocation{}, &res, &logSink)
	if err != nil {
		t.Fatalf("runVerifyFixLoop: %v", err)
	}
	return res, invoker, reinvoked, tree, logSink.String()
}

func TestRunVerifyFixLoop_ExitTempFailRetriedInPlaceNeverFixAgent(t *testing.T) {
	cmd, counter := exitSeqVerifyCmd(t, signatureFreeOut, verifyExitTempFail, 0)
	res, invoker, reinvoked, tree, logs := runFixLoop(t, cmd)
	if !res.OK || res.FailureCategory != "" || tree == "" {
		t.Errorf("res = OK:%t cat:%q tree:%q, want a clean pass with a verified tree", res.OK, res.FailureCategory, tree)
	}
	if reinvoked || invoker.callIdx != 0 {
		t.Errorf("reinvoked=%t fix invocations=%d, want false/0 — exit 75 must never reach the fix agent", reinvoked, invoker.callIdx)
	}
	if n := readInvocations(t, counter); n != 2 {
		t.Errorf("verify invocations = %d, want 2 (exit 75, then the passing re-run)", n)
	}
	if n := countEvents(res.Events, "verify_infra_tempfail_retry"); n != 1 {
		t.Errorf("verify_infra_tempfail_retry events = %d, want 1", n)
	}
	if n := strings.Count(logs, `"event":"verify_infra_tempfail_retry"`); n != 1 {
		t.Errorf("verify_infra_tempfail_retry log lines = %d, want 1", n)
	}
	if n := countEvents(res.Events, "verify_infra_flake_retry"); n != 0 {
		t.Errorf("verify_infra_flake_retry events = %d, want 0 — exit 75 must not spend the text absorb", n)
	}
	if outcome, _ := verifySummaryOf(t, res.Events); outcome != "passed" {
		t.Errorf("verify_summary outcome = %q, want passed", outcome)
	}
}

// Always 75: category C verify_infra_tempfail after EXACTLY three executions
// (1 + verifyTempFailMaxReruns, written as a literal so a bound change reddens
// it), the fix agent never invoked, no verified tree.
func TestRunVerifyFixLoop_ExitTempFailPersistentIsCategoryC(t *testing.T) {
	cmd, counter := exitSeqVerifyCmd(t, signatureFreeOut, verifyExitTempFail)
	res, invoker, reinvoked, tree, logs := runFixLoop(t, cmd)
	if res.OK || res.FailureCategory != "C" {
		t.Errorf("res = OK:%t cat:%q, want category C", res.OK, res.FailureCategory)
	}
	if !strings.HasPrefix(res.FailureReason, verifyTempFailLead+": ") {
		t.Errorf("FailureReason must lead with %q, got:\n%s", verifyTempFailLead, res.FailureReason)
	}
	if tree != "" {
		t.Error("a temp-failed loop must not yield a verified tree")
	}
	if reinvoked || invoker.callIdx != 0 {
		t.Errorf("reinvoked=%t fix invocations=%d, want false/0", reinvoked, invoker.callIdx)
	}
	if n := readInvocations(t, counter); n != 3 {
		t.Errorf("verify invocations = %d, want 3 (1 initial + 2 re-runs)", n)
	}
	if n := countEvents(res.Events, "verify_infra_tempfail_retry"); n != 2 {
		t.Errorf("verify_infra_tempfail_retry events = %d, want 2", n)
	}
	if n := countEvents(res.Events, "verify_infra_tempfail"); n != 1 {
		t.Errorf("verify_infra_tempfail events = %d, want 1", n)
	}
	if !strings.Contains(logs, `"event":"verify_infra_tempfail"`) {
		t.Error("missing the terminal verify_infra_tempfail log line")
	}
	outcome, detail := verifySummaryOf(t, res.Events)
	if outcome != "failed" || !strings.HasPrefix(detail, verifyTempFailLead+": ") {
		t.Errorf("verify_summary = %q / %q, want failed with the temp-fail lead", outcome, detail)
	}
}

// A plain non-zero exit is a red tree: it reaches the fix agent and, with the
// budget spent, demotes category A.
func TestRunVerifyFixLoop_PlainExitOneReachesFixAgent(t *testing.T) {
	cmd, _ := exitSeqVerifyCmd(t, signatureFreeOut, 1)
	res, invoker, reinvoked, _, _ := runFixLoop(t, cmd)
	if !reinvoked || invoker.callIdx != 1 {
		t.Errorf("reinvoked=%t fix invocations=%d, want true/1", reinvoked, invoker.callIdx)
	}
	if res.OK || res.FailureCategory != "A" {
		t.Errorf("res = OK:%t cat:%q, want category A", res.OK, res.FailureCategory)
	}
}

// The deleted #3948 text classifier: scripts/test's refusal TEXT on an exit
// other than 73 is an ordinary red tree that reaches the fix agent.
func TestRunVerifyFixLoop_RefusalTextWithoutExit73ReachesFixAgent(t *testing.T) {
	cmd, _ := exitSeqVerifyCmd(t, "scripts/test: another RUNNER verify still holds the lock at /l after 600s", 1)
	res, invoker, reinvoked, _, _ := runFixLoop(t, cmd)
	if !reinvoked || invoker.callIdx != 1 {
		t.Errorf("reinvoked=%t fix invocations=%d, want true/1 — refusal text alone is no longer contention", reinvoked, invoker.callIdx)
	}
	if res.FailureCategory != "A" || countEvents(res.Events, "verify_lock_contended_retry") != 0 {
		t.Errorf("cat=%q lock retries=%d, want category A and none", res.FailureCategory, countEvents(res.Events, "verify_lock_contended_retry"))
	}
}

// #3983 repro: a red verify carrying a testcontainers start-flake signature
// spends the infra absorb, and the absorb's re-run never executes (the
// throwaway clone cannot be materialized). Before #4022 the loop's
// `outcome != "failed"` test read that "skipped" as a PASS.
func TestRunVerifyFixLoop_FailedThenSkippedIsNotAPass(t *testing.T) {
	calls := 0
	swapMaterializer(t, func(ctx context.Context, repoDir, headSHA, parent string) (string, error) {
		calls++
		if calls == 1 {
			return materializeGateCheckout(ctx, repoDir, headSHA, parent)
		}
		return "", errors.New("git clone --no-hardlinks: fatal: unable to create temporary file: No space left on device")
	})
	cmd, _ := exitSeqVerifyCmd(t, "postgres: wait until ready: mapped port: context deadline exceeded", 1)
	res, invoker, reinvoked, tree, logs := runFixLoop(t, cmd)
	if res.OK || res.FailureCategory != "C" {
		t.Errorf("res = OK:%t cat:%q, want category C", res.OK, res.FailureCategory)
	}
	if !strings.HasPrefix(res.FailureReason, verifyGateSkippedLead+": ") || !strings.Contains(res.FailureReason, "No space left on device") {
		t.Errorf("FailureReason must lead with %q and name the clone cause, got:\n%s", verifyGateSkippedLead, res.FailureReason)
	}
	if tree != "" {
		t.Errorf("verified tree = %q, want empty — an unexecuted gate is not a pass", tree)
	}
	if reinvoked || invoker.callIdx != 0 {
		t.Errorf("reinvoked=%t fix invocations=%d, want false/0", reinvoked, invoker.callIdx)
	}
	if calls != 3 {
		t.Errorf("materializer calls = %d, want 3 (the red run, the absorb re-run, the one skip re-run)", calls)
	}
	if n := countEvents(res.Events, "verify_infra_flake_retry"); n != 1 {
		t.Errorf("verify_infra_flake_retry events = %d, want 1", n)
	}
	if n := strings.Count(logs, `"event":"verify_gate_skipped_retry"`); n != 1 {
		t.Errorf("verify_gate_skipped_retry log lines = %d, want 1", n)
	}
	if outcome, detail := verifySummaryOf(t, res.Events); outcome != "skipped" || !strings.HasPrefix(detail, verifyGateSkippedLead+": ") {
		t.Errorf("verify_summary = %q / %q, want skipped with the skipped lead", outcome, detail)
	}
}

// The one skipped-gate re-run is real: a materialization failure that clears
// on the re-run verifies and passes.
func TestRunVerifyFixLoop_SkippedOnceThenPasses(t *testing.T) {
	calls := 0
	swapMaterializer(t, func(ctx context.Context, repoDir, headSHA, parent string) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("git clone --no-hardlinks: transient failure")
		}
		return materializeGateCheckout(ctx, repoDir, headSHA, parent)
	})
	cmd, counter := exitSeqVerifyCmd(t, signatureFreeOut, 0)
	res, _, _, tree, _ := runFixLoop(t, cmd)
	if !res.OK || res.FailureCategory != "" || tree == "" {
		t.Errorf("res = OK:%t cat:%q tree:%q, want a clean pass", res.OK, res.FailureCategory, tree)
	}
	if n := readInvocations(t, counter); n != 1 {
		t.Errorf("verify invocations = %d, want 1", n)
	}
	if n := countEvents(res.Events, "verify_gate_skipped_retry"); n != 1 {
		t.Errorf("verify_gate_skipped_retry events = %d, want 1", n)
	}
}

// --- the single-shot committed gate ----------------------------------------

func TestRunVerifyGateCommitted_ExitTempFailIsCategoryC(t *testing.T) {
	repo, _, _ := verifiedTreeRepo(t)
	cmd, counter := exitSeqVerifyCmd(t, signatureFreeOut, verifyExitTempFail)
	var logSink strings.Builder
	events, tree, err := runVerifyGateCommitted(context.Background(), verifiedTreeCfg(repo, cmd), &logSink)
	if !errors.Is(err, gitops.ErrVerifyInfraFailure) || !errors.Is(err, errVerifyTempFail) {
		t.Fatalf("err = %v, want ErrVerifyInfraFailure + errVerifyTempFail", err)
	}
	if errors.Is(err, gitops.ErrCommittedTestsFailed) {
		t.Errorf("exit 75 must NOT wrap ErrCommittedTestsFailed: %v", err)
	}
	if got := committedGateFailureCategory(err); got != "C" {
		t.Errorf("committedGateFailureCategory = %q, want C", got)
	}
	if !strings.Contains(err.Error(), verifyTempFailLead+": ") {
		t.Errorf("error must carry the temp-fail lead: %v", err)
	}
	if tree != "" {
		t.Error("a temp-failed gate must not yield a verified tree")
	}
	if n := readInvocations(t, counter); n != 3 {
		t.Errorf("verify invocations = %d, want 3", n)
	}
	if n := countEvents(events, "verify_infra_tempfail_retry"); n != 2 {
		t.Errorf("verify_infra_tempfail_retry events = %d, want 2", n)
	}
	if n := countEvents(events, "verify_infra_flake_retry"); n != 0 {
		t.Errorf("verify_infra_flake_retry events = %d, want 0", n)
	}
}

func TestRunVerifyGateCommitted_ExitTempFailOnceThenPass(t *testing.T) {
	repo, _, _ := verifiedTreeRepo(t)
	cmd, counter := exitSeqVerifyCmd(t, signatureFreeOut, verifyExitTempFail, 0)
	_, tree, err := runVerifyGateCommitted(context.Background(), verifiedTreeCfg(repo, cmd), &strings.Builder{})
	if err != nil || tree == "" {
		t.Fatalf("tree=%q err=%v, want a pass with a verified tree", tree, err)
	}
	if n := readInvocations(t, counter); n != 2 {
		t.Errorf("verify invocations = %d, want 2", n)
	}
}

// A gate that never ran the command is category C after its one re-run —
// never the tolerant nil return it used to be (#3983).
func TestRunVerifyGateCommitted_SkippedIsCategoryC(t *testing.T) {
	calls := 0
	swapMaterializer(t, func(context.Context, string, string, string) (string, error) {
		calls++
		return "", errors.New("git clone --no-hardlinks: fatal: No space left on device")
	})
	repo, _, _ := verifiedTreeRepo(t)
	cmd, counter := exitSeqVerifyCmd(t, signatureFreeOut, 0)
	events, tree, err := runVerifyGateCommitted(context.Background(), verifiedTreeCfg(repo, cmd), &strings.Builder{})
	if !errors.Is(err, gitops.ErrVerifyInfraFailure) || !errors.Is(err, errVerifyGateSkipped) {
		t.Fatalf("err = %v, want ErrVerifyInfraFailure + errVerifyGateSkipped", err)
	}
	if got := committedGateFailureCategory(err); got != "C" {
		t.Errorf("committedGateFailureCategory = %q, want C", got)
	}
	if !strings.Contains(err.Error(), "No space left on device") {
		t.Errorf("error must name the clone cause: %v", err)
	}
	if tree != "" {
		t.Error("a skipped gate must not yield a verified tree")
	}
	if calls != 2 || readInvocations(t, counter) != 0 {
		t.Errorf("materializer calls = %d, verify invocations = %d, want 2 and 0", calls, readInvocations(t, counter))
	}
	if n := countEvents(events, "verify_gate_skipped_retry"); n != 1 {
		t.Errorf("verify_gate_skipped_retry events = %d, want 1", n)
	}
}

// --- the #960 strict re-verify ----------------------------------------------

func TestOpenPRAndShipArtifact_VerifiedTreeMismatch_ExitTempFailIsCategoryC(t *testing.T) {
	h := newStrictReverifyHarness(t)
	cmd, counter := exitSeqVerifyCmd(t, signatureFreeOut, verifyExitTempFail)
	logs, err := h.ship(t, context.Background(), cmd)
	if !errors.Is(err, gitops.ErrVerifyInfraFailure) || !errors.Is(err, errVerifyTempFail) {
		t.Fatalf("err = %v, want ErrVerifyInfraFailure + errVerifyTempFail", err)
	}
	if errors.Is(err, gitops.ErrPushedTreeNotVerified) {
		t.Errorf("exit 75 must NOT wrap ErrPushedTreeNotVerified: %v", err)
	}
	if got := pushFailureCategory(err); got != "C" {
		t.Errorf("pushFailureCategory = %q, want C", got)
	}
	if n := readInvocations(t, counter); n != 3 {
		t.Errorf("re-verify invocations = %d, want 3", n)
	}
	if n := strings.Count(logs, `"event":"verify_infra_tempfail_retry"`); n != 2 {
		t.Errorf("verify_infra_tempfail_retry log lines = %d, want 2", n)
	}
	if n := strings.Count(logs, `"event":"verify_infra_flake_retry"`); n != 0 {
		t.Errorf("verify_infra_flake_retry log lines = %d, want 0", n)
	}
	h.assertOriginUntouched(t)
}

func TestOpenPRAndShipArtifact_VerifiedTreeMismatch_ExitTempFailOnceThenPushes(t *testing.T) {
	h := newStrictReverifyHarness(t)
	cmd, counter := exitSeqVerifyCmd(t, signatureFreeOut, verifyExitTempFail, 0)
	logs, err := h.ship(t, context.Background(), cmd)
	if err != nil {
		t.Fatalf("openPRAndShipArtifact = %v, want nil", err)
	}
	if !strings.Contains(logs, `"event":"pushed_tree_reverified"`) {
		t.Error("expected pushed_tree_reverified after the re-run passed")
	}
	if n := readInvocations(t, counter); n != 2 {
		t.Errorf("re-verify invocations = %d, want 2", n)
	}
}
