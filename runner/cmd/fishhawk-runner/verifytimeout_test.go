package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// --- #3383 timed-out verify gate ----------------------------------------
//
// Every timed-out case below runs a REAL hanging process (`sleep` under
// `sh`), never a stub that merely prints the trailer or the lead: a control
// that inspected text could not pass them. Every deadline-competing duration
// (the verify timeout, the elapsed upper bound, the sleeper length) derives
// from scaledD (lockholder_test.go) per the #1984 rule; 20ms polls stay
// unscaled.

// hangVerifyTimeout is the verify timeout the hanging scripts are run under.
func hangVerifyTimeout() time.Duration { return scaledD(500 * time.Millisecond) }

// hangElapsedBound is the wall-clock bound a timed-out gate must return
// within — an order of magnitude over the timeout so a loaded host cannot
// slip past it, and two orders under the sleeper so a leaked sleeper is
// unmistakable.
func hangElapsedBound() time.Duration { return scaledD(15 * time.Second) }

// hangingVerifyScript writes a verify script that prints `prelude` (a
// partial fragment, possibly carrying an infra literal) and then sleeps far
// past every timeout in this file, so only the runner's process-group kill
// can end it.
func hangingVerifyScript(t *testing.T, prelude string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "hang.sh")
	mustWrite(t, script, "#!/bin/sh\ncat <<'FISHHAWK_VERIFY_EOF'\n"+prelude+"\nFISHHAWK_VERIFY_EOF\nsleep 300\n")
	return "sh " + script
}

// failThenHangVerifyCmd writes a verify script that prints `first` and exits
// 3 on its FIRST invocation (sentinel outside the worktree), then prints the
// partial `>> > ./runner` fragment and HANGS on every later invocation — the
// shape approval condition (1) names: an absorbable failure whose absorb
// re-run itself times out.
func failThenHangVerifyCmd(t *testing.T, first string) string {
	t.Helper()
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "failed-once")
	script := filepath.Join(dir, "verify.sh")
	mustWrite(t, script, "#!/bin/sh\n"+
		"if [ ! -e "+sentinel+" ]; then\n"+
		"  : > "+sentinel+"\n"+
		"  cat <<'FISHHAWK_VERIFY_EOF'\n"+first+"\nFISHHAWK_VERIFY_EOF\n"+
		"  exit 3\n"+
		"fi\n"+
		"echo '>> > ./runner'\n"+
		"sleep 300\n")
	return "sh " + script
}

// verifyRunPayload decodes the fields of a verify_run event this file asserts on.
type verifyRunPayload struct {
	ExitCode       int    `json:"exit_code"`
	Outcome        string `json:"outcome"`
	Output         string `json:"output"`
	TimedOut       bool   `json:"timed_out"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

func decodeVerifyRun(t *testing.T, ev agent.Event) verifyRunPayload {
	t.Helper()
	var p verifyRunPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatalf("decode verify_run payload: %v\n%s", err, ev.Payload)
	}
	return p
}

// (a) The seam under a REAL hanging grandchild: returns within the bound,
// code -1, timedOut true, and the partial line the child printed before the
// kill is retained — which also proves the group kill closed the grandchild's
// inherited pipe (a kill of the direct child alone would block CombinedOutput
// on the sleeper until the 300s expired, failing the bound).
func TestExecBoundedHostArgv_TimeoutKillsGroupAndReportsTimedOut(t *testing.T) {
	start := time.Now()
	out, code, timedOut := execBoundedHostArgv(context.Background(),
		[]string{"sh", "-c", `echo ">> > ./runner"; sleep 300`}, t.TempDir(), os.Environ(), hangVerifyTimeout())
	if elapsed := time.Since(start); elapsed > hangElapsedBound() {
		t.Fatalf("seam returned after %s, want within %s (the process group was not killed)", elapsed, hangElapsedBound())
	}
	if !timedOut {
		t.Errorf("timedOut = false, want true for the runner's own deadline")
	}
	if code != -1 {
		t.Errorf("exit code = %d, want -1 for a killed command", code)
	}
	if !strings.Contains(out, ">> > ./runner") {
		t.Errorf("partial output before the kill must be retained, got %q", out)
	}

	// Control row: a fast non-zero exit is NOT a timeout.
	out, code, timedOut = execBoundedHostArgv(context.Background(),
		[]string{"sh", "-c", "echo red; exit 3"}, t.TempDir(), os.Environ(), hangVerifyTimeout())
	if timedOut || code != 3 || strings.TrimSpace(out) != "red" {
		t.Errorf("fast exit 3 = (%q, %d, %v), want (red, 3, false)", out, code, timedOut)
	}
}

// (b) A PARENT-context cancellation mid-run (a runner shutdown) with a long
// timeout is (-1, false): only the runner's OWN per-exec deadline is a
// timeout.
func TestExecBoundedHostArgv_ParentCancelIsNotTimedOut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(scaledD(200 * time.Millisecond))
		cancel()
	}()
	start := time.Now()
	_, code, timedOut := execBoundedHostArgv(ctx,
		[]string{"sh", "-c", "sleep 300"}, t.TempDir(), os.Environ(), scaledD(time.Hour))
	if elapsed := time.Since(start); elapsed > hangElapsedBound() {
		t.Fatalf("seam returned after %s, want within %s", elapsed, hangElapsedBound())
	}
	if timedOut {
		t.Errorf("timedOut = true for a parent cancellation, want false")
	}
	if code != -1 {
		t.Errorf("exit code = %d, want -1", code)
	}
}

// (c) runVerifyCommittedTree on a hanging script: disposition gateTimedOut,
// outcome failed, the output ENDS with the trailer naming the timeout, the
// elapsed time, the form and the last captured line, and the verify_run
// payload carries timed_out:true / exit_code -1.
func TestRunVerifyCommittedTree_TimedOutDispositionAndTrailer(t *testing.T) {
	repo, _, _ := verifiedTreeRepo(t)
	head := commitAgentChange(t, repo)
	cmd := hangingVerifyScript(t, ">> > ./runner")
	start := time.Now()
	ev, out, outcome, disp := runVerifyCommittedTree(context.Background(), cmd, repo, head, hangVerifyTimeout(), nil)
	if elapsed := time.Since(start); elapsed > hangElapsedBound() {
		t.Fatalf("gate returned after %s, want within %s", elapsed, hangElapsedBound())
	}
	if disp != gateTimedOut {
		t.Fatalf("disposition = %s, want timed_out", disp)
	}
	if outcome != "failed" {
		t.Errorf("outcome = %q, want failed", outcome)
	}
	trailerStart := strings.Index(out, "--- fishhawk-runner: verify TERMINATED, no verdict ---")
	if trailerStart < 0 {
		t.Fatalf("output lacks the trailer marker:\n%s", out)
	}
	if !strings.HasPrefix(out, ">> > ./runner\n") {
		t.Errorf("the partial fragment must precede the trailer: %q", out)
	}
	trailer := out[trailerStart:]
	for _, want := range []string{
		verifyGateTimedOutLead + ": " + fmt.Sprintf("%q", cmd),
		"(full form)",
		"executor.verify.timeout (" + hangVerifyTimeout().String() + ")",
		`last captured line: ">> > ./runner"`,
		"NO verdict was reached",
		"output above is INCOMPLETE",
		"ABSENCE of FAIL lines proves nothing",
	} {
		if !strings.Contains(trailer, want) {
			t.Errorf("trailer lacks %q:\n%s", want, trailer)
		}
	}
	if !strings.HasSuffix(out, "\n") || strings.TrimSpace(out[trailerStart:]) == "" {
		t.Errorf("the trailer must be the LAST thing in the output: %q", out)
	}
	p := decodeVerifyRun(t, ev)
	if !p.TimedOut || p.ExitCode != -1 || p.Outcome != "failed" {
		t.Errorf("verify_run payload = %+v, want timed_out:true exit_code:-1 outcome:failed", p)
	}
	if p.TimeoutSeconds != int(hangVerifyTimeout()/time.Second) {
		t.Errorf("timeout_seconds = %d, want %d", p.TimeoutSeconds, int(hangVerifyTimeout()/time.Second))
	}
	if !strings.HasSuffix(p.Output, trailer) {
		t.Errorf("verify_run output must carry the trailer")
	}
}

// commitAgentChange commits the fixture's dirty in-scope edit and returns
// HEAD, so runVerifyCommittedTree has a committed SHA to clone.
func commitAgentChange(t *testing.T, repo string) string {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-m", "agent change", "--no-verify"}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	head, err := gitRevParseHEAD(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	return head
}

// assertFixLoopTimedOut holds the assertions every timed-out fix-loop case
// shares: category C, the timed-out lead, the fix agent never invoked, exactly
// wantVerifyRuns verify_run events, no absorb events after the timeout, one
// verify_gate_timed_out log line + trace event, verify_summary failed with the
// lead as detail, and an empty verified tree.
func assertFixLoopTimedOut(t *testing.T, res agent.Result, invoker *fakeInvoker, reinvoked bool, tree string, logs, form string, wantVerifyRuns int) {
	t.Helper()
	if invoker.callIdx != 0 {
		t.Errorf("fix agent invoked %d time(s), want 0 — a no-verdict output must never reach it", invoker.callIdx)
	}
	if reinvoked {
		t.Error("reinvoked = true, want false")
	}
	if tree != "" {
		t.Errorf("verified tree = %q, want empty", tree)
	}
	if res.OK {
		t.Error("res.OK = true, want false")
	}
	if res.FailureCategory != "C" {
		t.Errorf("FailureCategory = %q, want C", res.FailureCategory)
	}
	if !strings.HasPrefix(res.FailureReason, verifyGateTimedOutLead+": ") {
		t.Errorf("FailureReason must begin with %q, got %q", verifyGateTimedOutLead+": ", res.FailureReason)
	}
	if !strings.Contains(res.FailureReason, "NO verdict") {
		t.Errorf("FailureReason must carry the trailer's NO verdict statement:\n%s", res.FailureReason)
	}
	if !strings.Contains(res.FailureReason, "("+form+" form)") {
		t.Errorf("FailureReason must name the %s form:\n%s", form, res.FailureReason)
	}
	if n := countEvents(res.Events, "verify_run"); n != wantVerifyRuns {
		t.Errorf("verify_run events = %d, want %d", n, wantVerifyRuns)
	}
	if n := countEvents(res.Events, "verify_autoformatted"); n != 0 {
		t.Errorf("verify_autoformatted events = %d, want 0", n)
	}
	if n := countEvents(res.Events, "verify_gate_timed_out"); n != 1 {
		t.Errorf("verify_gate_timed_out trace events = %d, want 1", n)
	}
	if n := strings.Count(logs, `"event":"verify_gate_timed_out"`); n != 1 {
		t.Errorf("verify_gate_timed_out log lines = %d, want 1:\n%s", n, logs)
	}
	if !strings.Contains(logs, `"event":"verify_gate_timed_out"`) || !strings.Contains(logs, `"form":"`+form+`"`) {
		t.Errorf("verify_gate_timed_out log line must name form %q:\n%s", form, logs)
	}
	if strings.Contains(logs, `"event":"verify_fix_reinvoke"`) {
		t.Errorf("no verify_fix_reinvoke may be logged for a timed-out gate:\n%s", logs)
	}
	var summaries int
	for _, ev := range res.Events {
		if ev.Kind != "verify_summary" {
			continue
		}
		summaries++
		var s struct {
			Outcome string `json:"outcome"`
			Detail  string `json:"detail"`
		}
		_ = json.Unmarshal(ev.Payload, &s)
		if s.Outcome != "failed" {
			t.Errorf("verify_summary outcome = %q, want failed", s.Outcome)
		}
		if !strings.HasPrefix(s.Detail, verifyGateTimedOutLead+": ") {
			t.Errorf("verify_summary detail must begin with the lead, got %q", s.Detail)
		}
	}
	if summaries != 1 {
		t.Errorf("verify_summary events = %d, want exactly 1", summaries)
	}
}

// (e) The fix loop on a hanging verify: category C without ever invoking the
// fix agent, exactly ONE verify_run, no absorb, the lead + trailer in the
// reason. Counterfactual: deleting the `case gateTimedOut` break in
// runVerifyFixLoop invokes the fake and demotes category A.
func TestRunVerifyFixLoop_TimedOutIsCategoryCWithoutFixInvoke(t *testing.T) {
	repo, _, _ := verifiedTreeRepo(t)
	cfg := verifiedTreeCfg(repo, hangingVerifyScript(t, ">> > ./runner"))
	cfg.verifyMaxIterations = 1
	cfg.verifyTimeout = hangVerifyTimeout()
	res := agent.Result{OK: true}
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	var logSink strings.Builder
	start := time.Now()
	reinvoked, tree, err := runVerifyFixLoop(context.Background(), &cfg, nil, "", invoker, agent.Invocation{}, &res, &logSink)
	if elapsed := time.Since(start); elapsed > hangElapsedBound() {
		t.Fatalf("loop returned after %s, want within %s", elapsed, hangElapsedBound())
	}
	if err != nil {
		t.Fatalf("runVerifyFixLoop: %v\n%s", err, logSink.String())
	}
	assertFixLoopTimedOut(t, res, invoker, reinvoked, tree, logSink.String(), verifyFormFull, 1)
	if n := countEvents(res.Events, "verify_infra_flake_retry"); n != 0 {
		t.Errorf("verify_infra_flake_retry events = %d, want 0", n)
	}
	if !strings.Contains(res.FailureReason, "--- fishhawk-runner: verify TERMINATED, no verdict ---") {
		t.Errorf("FailureReason must end with the trailer:\n%s", res.FailureReason)
	}
}

// (f) The scoped/full asymmetry the issue documents: a Go scope whose SCOPED
// pre-pass passes and whose FULL re-verify hangs names form "full" in the
// reason and the log line.
func TestRunVerifyFixLoop_TimedOutFullFormNamesTheForm(t *testing.T) {
	cfg, _ := verifyFixLoopScopeFixture(t, verifyScopeGoFiles, 0, 0)
	script := filepath.Join(t.TempDir(), "scoped-ok-full-hangs.sh")
	mustWrite(t, script, "#!/bin/sh\nif [ -n \"$"+verifyPackagesEnvVar+"\" ]; then exit 0; fi\necho '>> > ./runner'\nsleep 300\n")
	cfg.verifyCmd = "sh " + script
	cfg.verifyTimeout = hangVerifyTimeout()
	res := agent.Result{OK: true}
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	var logSink strings.Builder
	reinvoked, tree, err := runVerifyFixLoop(context.Background(), &cfg, nil, "", invoker, agent.Invocation{}, &res, &logSink)
	if err != nil {
		t.Fatalf("runVerifyFixLoop: %v\n%s", err, logSink.String())
	}
	// Two verify_run events: the passing scoped pre-pass and the hanging full form.
	assertFixLoopTimedOut(t, res, invoker, reinvoked, tree, logSink.String(), verifyFormFull, 2)
	if strings.Contains(res.FailureReason, "(scoped form)") {
		t.Errorf("the deciding run was the FULL form; reason must not name scoped:\n%s", res.FailureReason)
	}
}

// (g) A timed-out output that HAPPENS to carry an infra literal (the #2645
// lint-lock signature the absorb predicate matches) still does not absorb:
// the timed-out break sits BEFORE the infra absorb, so exactly one verify_run.
func TestRunVerifyFixLoop_TimedOutOutputWithInfraLiteralDoesNotAbsorb(t *testing.T) {
	repo, _, _ := verifiedTreeRepo(t)
	cfg := verifiedTreeCfg(repo, hangingVerifyScript(t, lintLockOutput2645))
	cfg.verifyMaxIterations = 1
	cfg.verifyTimeout = hangVerifyTimeout()
	if !isVerifyInfraFailure(lintLockOutput2645) {
		t.Fatal("fixture precondition: the prelude must match the absorb predicate")
	}
	res := agent.Result{OK: true}
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	var logSink strings.Builder
	reinvoked, tree, err := runVerifyFixLoop(context.Background(), &cfg, nil, "", invoker, agent.Invocation{}, &res, &logSink)
	if err != nil {
		t.Fatalf("runVerifyFixLoop: %v\n%s", err, logSink.String())
	}
	assertFixLoopTimedOut(t, res, invoker, reinvoked, tree, logSink.String(), verifyFormFull, 1)
	if n := countEvents(res.Events, "verify_infra_flake_retry"); n != 0 {
		t.Errorf("verify_infra_flake_retry events = %d, want 0 — a timed-out fragment must never buy a second full-timeout run", n)
	}
}

// Approval condition (1), fix-loop INFRA absorb: the first verify fails with
// an infra literal (absorbed, re-run in place), and the absorb re-run itself
// TIMES OUT. The LAST execution's disposition governs: category C with the
// timed-out lead, the fix agent never invoked, two verify_run events, one
// verify_infra_flake_retry (the absorb DID fire) and one verify_gate_timed_out.
func TestRunVerifyFixLoop_InfraAbsorbRerunTimedOutIsCategoryC(t *testing.T) {
	repo, _, _ := verifiedTreeRepo(t)
	cfg := verifiedTreeCfg(repo, failThenHangVerifyCmd(t, lintLockOutput2645))
	cfg.verifyMaxIterations = 1
	cfg.verifyTimeout = hangVerifyTimeout()
	res := agent.Result{OK: true}
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	var logSink strings.Builder
	reinvoked, tree, err := runVerifyFixLoop(context.Background(), &cfg, nil, "", invoker, agent.Invocation{}, &res, &logSink)
	if err != nil {
		t.Fatalf("runVerifyFixLoop: %v\n%s", err, logSink.String())
	}
	assertFixLoopTimedOut(t, res, invoker, reinvoked, tree, logSink.String(), verifyFormFull, 2)
	if n := countEvents(res.Events, "verify_infra_flake_retry"); n != 1 {
		t.Errorf("verify_infra_flake_retry events = %d, want 1 (the first failure WAS absorbed)", n)
	}
}

// Approval condition (1), fix-loop AUTO-FORMAT absorb: the first verify fails
// with a format-only lint output (absorbed: the stubbed formatter reformats
// the in-scope file and the loop re-verifies), and that re-verify TIMES OUT.
// Category C, timed-out lead, fix agent never invoked, one verify_autoformatted
// event followed by the timed-out verify_run.
func TestRunVerifyFixLoop_AutoformatAbsorbRerunTimedOutIsCategoryC(t *testing.T) {
	_, cfg := autoformatRepo(t, failThenHangVerifyCmd(t, gofmtOnlyLintOutput))
	cfg.verifyTimeout = hangVerifyTimeout()
	count := stubFormatter(t, `printf 'package p\n' > fmtme.go; exit 0`)
	res := agent.Result{OK: true}
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	var logSink strings.Builder
	reinvoked, tree, err := runVerifyFixLoop(context.Background(), &cfg, nil, "", invoker, agent.Invocation{}, &res, &logSink)
	if err != nil {
		t.Fatalf("runVerifyFixLoop: %v\n%s", err, logSink.String())
	}
	if n := count(); n != 1 {
		t.Fatalf("formatter invocations = %d, want 1 (the format-only failure WAS absorbed)", n)
	}
	// assertFixLoopTimedOut asserts zero verify_autoformatted; here exactly one
	// is expected, so check the shared invariants by hand for that field.
	if invoker.callIdx != 0 || reinvoked || tree != "" || res.OK {
		t.Errorf("(invoked=%d reinvoked=%v tree=%q ok=%v), want (0 false \"\" false)", invoker.callIdx, reinvoked, tree, res.OK)
	}
	if res.FailureCategory != "C" || !strings.HasPrefix(res.FailureReason, verifyGateTimedOutLead+": ") {
		t.Errorf("category/reason = %q / %q, want C with the timed-out lead", res.FailureCategory, res.FailureReason)
	}
	if n := countEvents(res.Events, "verify_autoformatted"); n != 1 {
		t.Errorf("verify_autoformatted events = %d, want 1", n)
	}
	if n := countEvents(res.Events, "verify_run"); n != 2 {
		t.Errorf("verify_run events = %d, want 2", n)
	}
	if n := countEvents(res.Events, "verify_gate_timed_out"); n != 1 {
		t.Errorf("verify_gate_timed_out events = %d, want 1", n)
	}
}

// (h) The single-shot committed gate on a hanging verify whose partial output
// carries the testcontainers 'context deadline exceeded after' infra literal
// (approval condition 3: the fixture must REACH the guarded absorb branch, so
// deleting the `!timedOut` absorb guard yields a second verify_run): the error
// wraps ErrVerifyInfraFailure AND errVerifyGateTimedOut, NOT
// ErrCommittedTestsFailed; committedGateFailureCategory C; exactly one
// verify_run; no absorb event.
func TestRunVerifyGateCommitted_TimedOutIsCategoryCNoAbsorb(t *testing.T) {
	repo, _, _ := verifiedTreeRepo(t)
	prelude := ">>> ./backend\nwait until ready: mapped port: check target: retries: 9, elapsed: 30s: context deadline exceeded after 9 retries\n>> > ./runner"
	if !isVerifyInfraFailure(prelude) {
		t.Fatal("fixture precondition: the prelude must match the absorb predicate")
	}
	cfg := verifiedTreeCfg(repo, hangingVerifyScript(t, prelude))
	cfg.verifyTimeout = hangVerifyTimeout()
	var logSink strings.Builder
	start := time.Now()
	events, tree, err := runVerifyGateCommitted(context.Background(), cfg, &logSink)
	if elapsed := time.Since(start); elapsed > hangElapsedBound() {
		t.Fatalf("gate returned after %s, want within %s", elapsed, hangElapsedBound())
	}
	if !errors.Is(err, gitops.ErrVerifyInfraFailure) || !errors.Is(err, errVerifyGateTimedOut) {
		t.Fatalf("err = %v, want ErrVerifyInfraFailure + errVerifyGateTimedOut", err)
	}
	if errors.Is(err, gitops.ErrCommittedTestsFailed) {
		t.Errorf("a timed-out gate must NOT wrap ErrCommittedTestsFailed: %v", err)
	}
	if got := committedGateFailureCategory(err); got != "C" {
		t.Errorf("committedGateFailureCategory = %q, want C", got)
	}
	if tree != "" {
		t.Errorf("verified tree = %q, want empty", tree)
	}
	if !strings.Contains(err.Error(), verifyGateTimedOutLead+": ") {
		t.Errorf("error must carry the timed-out lead: %v", err)
	}
	if n := countEvents(events, "verify_run"); n != 1 {
		t.Errorf("verify_run events = %d, want exactly 1 (no absorb re-run)", n)
	}
	if n := countEvents(events, "verify_infra_flake_retry"); n != 0 {
		t.Errorf("verify_infra_flake_retry events = %d, want 0", n)
	}
	if n := countEvents(events, "verify_gate_timed_out"); n != 1 {
		t.Errorf("verify_gate_timed_out events = %d, want 1", n)
	}
	if !strings.Contains(logSink.String(), `"event":"verify_gate_timed_out"`) {
		t.Errorf("missing verify_gate_timed_out log line:\n%s", logSink.String())
	}
}

// Approval condition (1), single-shot gate: the first verify fails with an
// infra literal (absorbed), and the absorb re-run TIMES OUT. The LAST
// execution's disposition governs: ErrVerifyInfraFailure + errVerifyGateTimedOut,
// category C, two verify_run events plus the absorb event.
func TestRunVerifyGateCommitted_InfraAbsorbRerunTimedOutIsCategoryC(t *testing.T) {
	repo, _, _ := verifiedTreeRepo(t)
	cfg := verifiedTreeCfg(repo, failThenHangVerifyCmd(t, lintLockOutput2645))
	cfg.verifyTimeout = hangVerifyTimeout()
	var logSink strings.Builder
	events, _, err := runVerifyGateCommitted(context.Background(), cfg, &logSink)
	if !errors.Is(err, gitops.ErrVerifyInfraFailure) || !errors.Is(err, errVerifyGateTimedOut) {
		t.Fatalf("err = %v, want ErrVerifyInfraFailure + errVerifyGateTimedOut", err)
	}
	if errors.Is(err, gitops.ErrCommittedTestsFailed) {
		t.Errorf("must NOT wrap ErrCommittedTestsFailed: %v", err)
	}
	if got := committedGateFailureCategory(err); got != "C" {
		t.Errorf("committedGateFailureCategory = %q, want C", got)
	}
	if !strings.Contains(err.Error(), verifyGateTimedOutLead+": ") {
		t.Errorf("error must lead with the timed-out lead: %v", err)
	}
	if n := countEvents(events, "verify_run"); n != 2 {
		t.Errorf("verify_run events = %d, want 2", n)
	}
	if n := countEvents(events, "verify_infra_flake_retry"); n != 1 {
		t.Errorf("verify_infra_flake_retry events = %d, want 1", n)
	}
	if n := countEvents(events, "verify_gate_timed_out"); n != 1 {
		t.Errorf("verify_gate_timed_out events = %d, want 1", n)
	}
}

// (i) The #960 strict re-verify on the #2645 mismatch fixture with a HANGING
// re-verify — cross-layer: exec seam → disposition → gate classification →
// push decision. ErrVerifyInfraFailure (not ErrPushedTreeNotVerified),
// pushFailureCategory C, OpenPR never called, the branch absent from the bare
// remote, exactly ONE verify_run log record (no absorb re-run), and the lead
// names the timeout.
func TestOpenPRAndShipArtifact_VerifiedTreeMismatch_TimedOutReverifyClassifiesCategoryC(t *testing.T) {
	repo, bare, branch := verifiedTreeRepo(t)
	fpr := withFakePROpenerOnly(t)
	fu := newFakeUploader(t)
	issued, err := fu.IssueKey(context.Background(), verifiedTreeRunID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, verifiedTree, gerr := runVerifyGateCommitted(context.Background(), verifiedTreeCfg(repo, "true"), os.Stderr)
	if gerr != nil || verifiedTree == "" {
		t.Fatalf("gate: tree=%q err=%v", verifiedTree, gerr)
	}
	moveBareMain(t, bare)

	var logSink strings.Builder
	cfg := verifiedTreeCfg(repo, hangingVerifyScript(t, ">> > ./runner"))
	cfg.verifyTimeout = hangVerifyTimeout()
	err = openPRAndShipArtifact(context.Background(), cfg, &logSink, fu, issued, "", false, false, nil, false, verifiedTree, "", nil, nil, nil, nil)
	if !errors.Is(err, gitops.ErrVerifyInfraFailure) {
		t.Fatalf("err = %v, want ErrVerifyInfraFailure", err)
	}
	if errors.Is(err, gitops.ErrPushedTreeNotVerified) {
		t.Errorf("a timed-out re-verify must NOT wrap ErrPushedTreeNotVerified: %v", err)
	}
	if got := pushFailureCategory(err); got != "C" {
		t.Errorf("pushFailureCategory = %q, want C", got)
	}
	if !strings.Contains(err.Error(), verifyGateTimedOutLead+": ") || !strings.Contains(err.Error(), hangVerifyTimeout().String()) {
		t.Errorf("error must lead with the timed-out lead naming the timeout: %v", err)
	}
	logs := logSink.String()
	if n := strings.Count(logs, `"event":"verify_run"`); n != 1 {
		t.Errorf("verify_run log records = %d, want exactly 1 (no absorb re-run)", n)
	}
	if n := strings.Count(logs, `"event":"verify_infra_flake_retry"`); n != 0 {
		t.Errorf("verify_infra_flake_retry log lines = %d, want 0", n)
	}
	if n := strings.Count(logs, `"event":"verify_gate_timed_out"`); n != 1 {
		t.Errorf("verify_gate_timed_out log lines = %d, want 1:\n%s", n, logs)
	}
	if fpr.gotArgs != nil {
		t.Error("OpenPR must not run — the classification changes the recovery verb, never the push decision")
	}
	if out, rerr := exec.Command("git", "--git-dir="+bare, "rev-parse", "--verify", "refs/heads/"+branch).Output(); rerr == nil {
		t.Errorf("run branch %s reached the bare remote despite the blocked push (tip %s)", branch, strings.TrimSpace(string(out)))
	}
}

// Approval condition (1), #960 strict re-verify: the first re-verify fails
// with an infra literal (absorbed), and the absorb re-run TIMES OUT. The LAST
// execution's disposition governs: ErrVerifyInfraFailure with the timed-out
// lead, category C, two verify_run records, one absorb line, OpenPR never
// called, branch absent from the remote.
func TestOpenPRAndShipArtifact_VerifiedTreeMismatch_InfraAbsorbRerunTimedOutIsCategoryC(t *testing.T) {
	repo, bare, branch := verifiedTreeRepo(t)
	fpr := withFakePROpenerOnly(t)
	fu := newFakeUploader(t)
	issued, err := fu.IssueKey(context.Background(), verifiedTreeRunID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, verifiedTree, gerr := runVerifyGateCommitted(context.Background(), verifiedTreeCfg(repo, "true"), os.Stderr)
	if gerr != nil || verifiedTree == "" {
		t.Fatalf("gate: tree=%q err=%v", verifiedTree, gerr)
	}
	moveBareMain(t, bare)

	var logSink strings.Builder
	cfg := verifiedTreeCfg(repo, failThenHangVerifyCmd(t, lintLockOutput2645))
	cfg.verifyTimeout = hangVerifyTimeout()
	err = openPRAndShipArtifact(context.Background(), cfg, &logSink, fu, issued, "", false, false, nil, false, verifiedTree, "", nil, nil, nil, nil)
	if !errors.Is(err, gitops.ErrVerifyInfraFailure) {
		t.Fatalf("err = %v, want ErrVerifyInfraFailure", err)
	}
	if errors.Is(err, gitops.ErrPushedTreeNotVerified) {
		t.Errorf("must NOT wrap ErrPushedTreeNotVerified: %v", err)
	}
	if got := pushFailureCategory(err); got != "C" {
		t.Errorf("pushFailureCategory = %q, want C", got)
	}
	if !strings.Contains(err.Error(), verifyGateTimedOutLead+": ") {
		t.Errorf("error must lead with the timed-out lead: %v", err)
	}
	logs := logSink.String()
	if n := strings.Count(logs, `"event":"verify_run"`); n != 2 {
		t.Errorf("verify_run log records = %d, want 2", n)
	}
	if n := strings.Count(logs, `"event":"verify_infra_flake_retry"`); n != 1 {
		t.Errorf("verify_infra_flake_retry log lines = %d, want 1", n)
	}
	if n := strings.Count(logs, `"event":"verify_gate_timed_out"`); n != 1 {
		t.Errorf("verify_gate_timed_out log lines = %d, want 1:\n%s", n, logs)
	}
	if fpr.gotArgs != nil {
		t.Error("OpenPR must not run")
	}
	if out, rerr := exec.Command("git", "--git-dir="+bare, "rev-parse", "--verify", "refs/heads/"+branch).Output(); rerr == nil {
		t.Errorf("run branch %s reached the bare remote despite the blocked push (tip %s)", branch, strings.TrimSpace(string(out)))
	}
}

// (j) The WORKING-TREE gate driven through the production run() call site —
// the path plan / --no-pr stages take (approval condition 3): a spec-sourced
// verify command that hangs demotes the stage category C with the timed-out
// lead, and the bundle's verify_run carries timed_out:true; the control row
// (a fast `exit 3`) stays category A. Hard-coding `A` back at the call site
// reddens the first row.
func TestRunVerifyGate_TimedOutIsCategoryC(t *testing.T) {
	for _, tc := range []struct {
		name, cmd, wantCategory string
		wantTimedOut            bool
	}{
		{"hanging verify → C", `echo ">> > ./runner"; sleep 300`, "C", true},
		{"fast red verify → A", "echo red; exit 3", "A", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
			withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
			fu := newFakeUploader(t)
			fu.promptResp = &upload.FetchedPrompt{
				StageID:              "22222222-3333-4444-5555-666666666666",
				StageType:            "plan",
				Prompt:               "Hello agent.",
				PromptHash:           "deadbeef",
				VerifyCommand:        tc.cmd,
				VerifyTimeoutSeconds: int(scaledD(time.Second) / time.Second),
			}
			withFakeUploader(t, fu)

			var stderr strings.Builder
			start := time.Now()
			got := run([]string{
				"--run-id", "11111111-2222-3333-4444-555555555555",
				"--backend-url", "https://api.fishhawk.test",
				"--workflow", "feature_change", "--stage", "plan",
				"--stage-id", "22222222-3333-4444-5555-666666666666",
				"--fetch-prompt",
				"--bundle-out", bundlePath,
			}, &stderr)
			if elapsed := time.Since(start); elapsed > hangElapsedBound() {
				t.Fatalf("run returned after %s, want within %s", elapsed, hangElapsedBound())
			}
			if got != exitFailure {
				t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
			}
			logs := stderr.String()
			if !strings.Contains(logs, `"category":"`+tc.wantCategory+`"`) {
				t.Errorf("expected category %s in the completion line:\n%s", tc.wantCategory, logs)
			}
			if tc.wantTimedOut && !strings.Contains(logs, `"reason":"`+verifyGateTimedOutLead+`: `) {
				t.Errorf("completion reason must begin with the timed-out lead:\n%s", logs)
			}
			data, err := os.ReadFile(bundlePath)
			if err != nil {
				t.Fatalf("bundle not written: %v", err)
			}
			_, events, _, err := openBundleForTest(data)
			if err != nil {
				t.Fatalf("open bundle: %v", err)
			}
			var saw bool
			for _, ev := range events {
				if ev.Kind != "verify_run" {
					continue
				}
				saw = true
				var p verifyRunPayload
				_ = json.Unmarshal(ev.Data, &p)
				if p.TimedOut != tc.wantTimedOut || p.Outcome != "failed" {
					t.Errorf("verify_run payload = %+v, want timed_out=%v outcome=failed", p, tc.wantTimedOut)
				}
				if tc.wantTimedOut && !strings.Contains(p.Output, "--- fishhawk-runner: verify TERMINATED, no verdict ---") {
					t.Errorf("verify_run output must end with the trailer:\n%s", p.Output)
				}
			}
			if !saw {
				t.Errorf("no verify_run event in the bundle:\n%+v", events)
			}
		})
	}
}

// TestWorkingTreeGateFailureCategory pins the call-site mapping's table.
func TestWorkingTreeGateFailureCategory(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("%w: killed", errVerifyGateTimedOut), "C"},
		{errors.New("verify gate failed: false exited 1"), "A"},
	} {
		if got := workingTreeGateFailureCategory(tc.err); got != tc.want {
			t.Errorf("workingTreeGateFailureCategory(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// (k) Pure helpers.
func TestVerifyTimeoutTrailer(t *testing.T) {
	got := verifyTimeoutTrailer("scripts/test verify", "scoped", 40*time.Minute, 40*time.Minute+3*time.Second, "ok\n>> > ./runner\n\n")
	if !strings.HasPrefix(got, "\n--- fishhawk-runner: verify TERMINATED, no verdict ---\n"+verifyGateTimedOutLead+": ") {
		t.Errorf("trailer must open with the marker and the lead:\n%s", got)
	}
	for _, want := range []string{
		`"scripts/test verify" (scoped form)`,
		"after 40m3s",
		"executor.verify.timeout (40m0s)",
		`last captured line: ">> > ./runner"`,
		"NO verdict was reached",
		"output above is INCOMPLETE",
		"any FAIL/panic lines above are real",
		"ABSENCE of FAIL lines proves nothing",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("trailer lacks %q:\n%s", want, got)
		}
	}
	for _, banned := range []string{"NO test failed", "no test failed"} {
		if strings.Contains(got, banned) {
			t.Errorf("trailer must not claim %q (approval condition 2):\n%s", banned, got)
		}
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("trailer must end with a newline")
	}
}

func TestLastNonEmptyLine(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"a\nb\nc\n", "c"},
		{"a\nb\nc\n\n\n", "c"},
		{"a\r\nb\r\n", "b"},
		{"only", "only"},
		{"", ""},
		{"\n\n  \n", ""},
	} {
		if got := lastNonEmptyLine(tc.in); got != tc.want {
			t.Errorf("lastNonEmptyLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
