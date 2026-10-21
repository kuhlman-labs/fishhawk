package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
)

// --- #3948 verify-lock contention, by exit code since #4022 ----------------
//
// Every contended case below seeds the refusal BY CONSTRUCTION: a scripted
// verify command exits 73 (verifyExitLockRefused) with SIGNATURE-FREE output,
// so only the exit-code disposition can classify it — no text fallback can
// mask a missing arm. TestRealScriptsTestRefusalExitsLockRefused binds the
// code to the REAL script's behaviour.

// lockRefusalFixtureOutput is what the fixture prints on a refused run. It
// carries no scripts/test refusal literal and matches no infra signature.
const lockRefusalFixtureOutput = "fixture: the shared verify lock could not be acquired"

// redactErr renders an error's text ("<nil>" for nil) for a failure message.
func redactErr(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// contendedNTimesThenPassVerifyCmd writes a verify script that prints the
// fixture output and exits 73 on its first n invocations, then exits 0. The
// invocation count is recorded in a counter file OUTSIDE the worktree (the
// gate runs the command in a throwaway clone), returned for the caller to read.
func contendedNTimesThenPassVerifyCmd(t *testing.T, n int) (cmd, counter string) {
	t.Helper()
	dir := t.TempDir()
	counter = filepath.Join(dir, "invocations")
	script := filepath.Join(dir, "verify.sh")
	mustWrite(t, script, "#!/bin/sh\n"+
		"c=$(cat "+counter+" 2>/dev/null || echo 0)\n"+
		"c=$((c + 1))\n"+
		"echo \"$c\" > "+counter+"\n"+
		"if [ \"$c\" -le "+strconv.Itoa(n)+" ]; then\n"+
		"  echo '"+lockRefusalFixtureOutput+"'\n"+
		"  exit "+strconv.Itoa(verifyExitLockRefused)+"\n"+
		"fi\n"+
		"exit 0\n")
	return "sh " + script, counter
}

// contendedCountingVerifyCmd is a verify that is contended on EVERY invocation.
func contendedCountingVerifyCmd(t *testing.T) (cmd, counter string) {
	t.Helper()
	return contendedNTimesThenPassVerifyCmd(t, 1<<20)
}

// readInvocations reads a counter file written by the scripts above (0 when
// the command never ran).
func readInvocations(t *testing.T, counter string) int {
	t.Helper()
	b, err := os.ReadFile(counter)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("read invocation counter: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("parse invocation counter %q: %v", b, err)
	}
	return n
}

// verifySummaryOf decodes the single verify_summary event in events.
func verifySummaryOf(t *testing.T, events []agent.Event) (outcome, detail string) {
	t.Helper()
	var n int
	for _, ev := range events {
		if ev.Kind != "verify_summary" {
			continue
		}
		n++
		var s struct {
			Outcome string `json:"outcome"`
			Detail  string `json:"detail"`
		}
		if err := json.Unmarshal(ev.Payload, &s); err != nil {
			t.Fatalf("decode verify_summary: %v", err)
		}
		outcome, detail = s.Outcome, s.Detail
	}
	if n != 1 {
		t.Fatalf("verify_summary events = %d, want exactly 1", n)
	}
	return outcome, detail
}

// --- (A) the exit-code pin ------------------------------------------------

// TestVerifyExitCodesMatchScriptsTest binds verifyExitLockRefused,
// verifyExitTempFail and verifyLockWaitBudget to the REAL scripts/test
// declarations: a renumbering on either side fails here.
func TestVerifyExitCodesMatchScriptsTest(t *testing.T) {
	root := repoRootForVerifyScopeTest(t)
	b, err := os.ReadFile(filepath.Join(root, "scripts", "test"))
	if err != nil {
		t.Fatalf("read scripts/test: %v", err)
	}
	for _, want := range []string{
		fmt.Sprintf("VERIFY_EXIT_TEMPFAIL=%d\n", verifyExitTempFail),
		fmt.Sprintf("VERIFY_EXIT_LOCK_REFUSED=%d\n", verifyExitLockRefused),
		fmt.Sprintf("VERIFY_LOCK_WAIT_SECONDS=%d\n", int(verifyLockWaitBudget/time.Second)),
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("scripts/test does not declare %q — the runner's verify exit-code contract no longer mirrors the script", strings.TrimSpace(want))
		}
	}
}

// --- (B) the executable pin -----------------------------------------------

// TestRealScriptsTestRefusalExitsLockRefused drives the REAL scripts/test
// (copied into the standard fixture by `scripts/test-verify-scope --fixture`)
// into its verify-lock refusal and asserts it exits 73, which
// verifyExitDisposition maps to gateLockRefused.
//
// Holder encoding, read from scripts/test: the lock is a symlink whose TARGET
// is `<pid>:<kind>`; _verify_lock_classify reports live-<kind> when `kill -0
// <pid>` succeeds. The script's own identity is `$$`, the bash process's pid —
// never this test process's — so os.Getpid() is a FOREIGN live runner holder
// by construction. FISHHAWK_VERIFY_LOCK_WAIT_SECONDS=0 makes the refusal
// immediate.
func TestRealScriptsTestRefusalExitsLockRefused(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not on PATH (%v); skipping the cross-boundary script test", err)
	}
	root := repoRootForVerifyScopeTest(t)
	harness := filepath.Join(root, "scripts", "test-verify-scope")
	fixture := t.TempDir()
	build := exec.Command("bash", harness, "--fixture", fixture) //nolint:gosec // fixed workspace-relative path
	build.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the standard fixture failed: %v\n%s", err, out)
	}
	goLog := filepath.Join(fixture, "logs", "go.log")
	if err := os.Truncate(goLog, 0); err != nil {
		t.Fatalf("truncating the go stub log: %v", err)
	}

	lock := filepath.Join(t.TempDir(), "fishhawk-verify.lock")
	holder := fmt.Sprintf("%d:runner", os.Getpid())
	if err := os.Symlink(holder, lock); err != nil {
		t.Fatalf("planting the live runner holder: %v", err)
	}

	base := []string{
		"PATH=" + filepath.Join(fixture, "bin") + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"FISHHAWK_TEST_P=4",
		"FISHHAWK_TEST_LEASE_DIR=" + filepath.Join(fixture, "leases"),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"FISHHAWK_VERIFY_LOCK_PATH=" + lock,
		"FISHHAWK_VERIFY_LOCK_WAIT_SECONDS=0",
	}
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		base = append(base, "TMPDIR="+tmp)
	}
	runVerify := func(env []string) (string, int) {
		cmd := exec.Command("bash", filepath.Join(fixture, "scripts", "test"), "verify") //nolint:gosec // fixture path
		cmd.Dir = fixture
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return string(out), ee.ExitCode()
		}
		if err != nil {
			t.Fatalf("running scripts/test verify: %v\n%s", err, out)
		}
		return string(out), 0
	}

	// RUNNER kind — what runVerifyCommittedTree injects — and SHELL kind: both
	// refusals exit 73.
	for _, kind := range []struct {
		name string
		env  []string
	}{{"runner", verifyLockOwnerEnv(base)}, {"shell", base}} {
		out, code := runVerify(kind.env)
		if code != verifyExitLockRefused {
			t.Errorf("%s-kind scripts/test verify against a live runner holder exited %d, want %d:\n%s", kind.name, code, verifyExitLockRefused, out)
		}
		if got := verifyExitDisposition(code, gateExecuted); got != gateLockRefused {
			t.Errorf("%s-kind refusal disposition = %s, want lock_refused", kind.name, got)
		}
	}
	if b, rerr := os.ReadFile(goLog); rerr != nil || len(strings.TrimSpace(string(b))) != 0 {
		t.Errorf("`go test` ran before the refusal (log %q, err %v) — the lock is taken before any leg", b, rerr)
	}
	if got, lerr := os.Readlink(lock); lerr != nil || got != holder {
		t.Errorf("the planted holder was disturbed (target %q, err %v) — a runner must refuse, never displace a peer runner", got, lerr)
	}
}

// --- (D) the verify-fix loop ----------------------------------------------

func TestRunVerifyFixLoop_VerifyLockContendedOnceThenPassNoFixInvoke(t *testing.T) {
	repo, _, _ := verifiedTreeRepo(t)
	cmd, counter := contendedNTimesThenPassVerifyCmd(t, 1)
	cfg := verifiedTreeCfg(repo, cmd)
	cfg.verifyMaxIterations = 1
	res := agent.Result{OK: true}
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	var logSink strings.Builder
	reinvoked, tree, err := runVerifyFixLoop(context.Background(), &cfg, nil, "", invoker, agent.Invocation{}, &res, &logSink)
	if err != nil {
		t.Fatalf("runVerifyFixLoop: %s", redactErr(err))
	}
	if tree == "" {
		t.Error("verified tree is empty, want the passing re-run's tree")
	}
	if reinvoked || invoker.callIdx != 0 {
		t.Errorf("reinvoked=%t fix invocations=%d, want false/0 — contention must never reach the fix agent", reinvoked, invoker.callIdx)
	}
	if !res.OK || res.FailureCategory != "" {
		t.Errorf("res = OK:%t cat:%q, want a clean pass", res.OK, res.FailureCategory)
	}
	if n := readInvocations(t, counter); n != 2 {
		t.Errorf("verify invocations = %d, want 2 (contended, then the passing re-run)", n)
	}
	logs := logSink.String()
	if n := strings.Count(logs, `"event":"verify_fix_reinvoke"`); n != 0 {
		t.Errorf("verify_fix_reinvoke log lines = %d, want 0", n)
	}
	if n := countEvents(res.Events, "verify_infra_flake_retry"); n != 0 {
		t.Errorf("verify_infra_flake_retry events = %d, want 0 — contention must not spend the infra absorb", n)
	}
	if n := countEvents(res.Events, "verify_lock_contended_retry"); n != 1 {
		t.Errorf("verify_lock_contended_retry events = %d, want 1", n)
	}
	if n := strings.Count(logs, `"event":"verify_lock_contended_retry"`); n != 1 {
		t.Errorf("verify_lock_contended_retry log lines = %d, want 1", n)
	}
	if outcome, _ := verifySummaryOf(t, res.Events); outcome != "passed" {
		t.Errorf("verify_summary outcome = %q, want passed", outcome)
	}
}

// The always-contended fix loop: category C verify_lock_contended, the fix
// agent never invoked, and EXACTLY three verify executions. The count is a
// hard-coded literal (approval condition 3): 1 initial + 2 re-runs. Raising
// the re-run bound by one (the `c.reruns >= verifyLockContentionMaxReruns`
// comparison in admit) makes it 4 and reddens this.
func TestRunVerifyFixLoop_VerifyLockAlwaysContendedIsCategoryC(t *testing.T) {
	repo, _, _ := verifiedTreeRepo(t)
	cmd, counter := contendedCountingVerifyCmd(t)
	cfg := verifiedTreeCfg(repo, cmd)
	cfg.verifyMaxIterations = 1
	res := agent.Result{OK: true}
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	var logSink strings.Builder
	reinvoked, tree, err := runVerifyFixLoop(context.Background(), &cfg, nil, "", invoker, agent.Invocation{}, &res, &logSink)
	if err != nil {
		t.Fatalf("runVerifyFixLoop: %s", redactErr(err))
	}
	if tree != "" {
		t.Error("a contended loop must not yield a verified tree")
	}
	if reinvoked || invoker.callIdx != 0 {
		t.Errorf("reinvoked=%t fix invocations=%d, want false/0", reinvoked, invoker.callIdx)
	}
	if res.OK || res.FailureCategory != "C" {
		t.Errorf("res = OK:%t cat:%q, want category C", res.OK, res.FailureCategory)
	}
	if !strings.HasPrefix(res.FailureReason, verifyLockContendedLead+": ") {
		t.Errorf("FailureReason must lead with %q, got:\n%s", verifyLockContendedLead+": ", res.FailureReason)
	}
	if strings.Contains(res.FailureReason, "No further in-place re-run") {
		t.Errorf("an unbounded context must not report a budget skip:\n%s", res.FailureReason)
	}
	if n := readInvocations(t, counter); n != 3 {
		t.Errorf("verify invocations = %d, want 3 (1 initial + 2 re-runs)", n)
	}
	if n := countEvents(res.Events, "verify_lock_contended_retry"); n != 2 {
		t.Errorf("verify_lock_contended_retry events = %d, want 2", n)
	}
	if n := countEvents(res.Events, "verify_lock_contended"); n != 1 {
		t.Errorf("verify_lock_contended events = %d, want 1", n)
	}
	if !strings.Contains(logSink.String(), `"cause":"reruns_exhausted"`) {
		t.Errorf("verify_lock_contended log line must name cause reruns_exhausted:\n%s", logSink.String())
	}
	if n := countEvents(res.Events, "verify_infra_flake_retry"); n != 0 {
		t.Errorf("verify_infra_flake_retry events = %d, want 0", n)
	}
	outcome, detail := verifySummaryOf(t, res.Events)
	if outcome != "failed" || !strings.HasPrefix(detail, verifyLockContendedLead+": ") {
		t.Errorf("verify_summary = %q / %q, want failed with the contended lead", outcome, detail)
	}
}

// Approval condition 4: a re-run is admitted only when the remaining stage
// budget (the context deadline) covers a full lock wait plus one verify. The
// deadlines here are compared against verifyLockWaitBudget + verifyTimeout,
// not raced against work, so they are deliberately NOT scaledD-derived: a 5x
// CI factor would lift the "short" row past the 11-minute threshold and
// invert it. The work itself finishes in well under a second.
func TestRunVerifyFixLoop_VerifyLockContentionHonoursStageBudget(t *testing.T) {
	cases := []struct {
		name        string
		deadline    time.Duration
		wantRuns    int
		wantRetries int
		wantShort   bool
	}{
		// 5m < 600s + 1m: the re-run cannot finish, so none is started.
		{"short budget skips the re-run", 5 * time.Minute, 1, 0, true},
		// 2h covers it: the count bound governs, exactly as with no deadline.
		{"ample budget admits the re-runs", 2 * time.Hour, 3, 2, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, _, _ := verifiedTreeRepo(t)
			cmd, counter := contendedCountingVerifyCmd(t)
			cfg := verifiedTreeCfg(repo, cmd)
			cfg.verifyMaxIterations = 1
			cfg.verifyTimeout = time.Minute
			ctx, cancel := context.WithTimeout(context.Background(), tc.deadline)
			defer cancel()
			res := agent.Result{OK: true}
			invoker := &fakeInvoker{canned: agent.Result{OK: true}}
			var logSink strings.Builder
			if _, _, err := runVerifyFixLoop(ctx, &cfg, nil, "", invoker, agent.Invocation{}, &res, &logSink); err != nil {
				t.Fatalf("runVerifyFixLoop: %s", redactErr(err))
			}
			if res.FailureCategory != "C" || !strings.HasPrefix(res.FailureReason, verifyLockContendedLead+": ") {
				t.Errorf("res = cat:%q reason:%q, want category C verify_lock_contended (never a timeout classification)",
					res.FailureCategory, res.FailureReason)
			}
			if strings.HasPrefix(res.FailureReason, verifyGateTimedOutLead) {
				t.Errorf("a budget skip must not read as a timed-out gate:\n%s", res.FailureReason)
			}
			if invoker.callIdx != 0 {
				t.Errorf("fix invocations = %d, want 0", invoker.callIdx)
			}
			if n := readInvocations(t, counter); n != tc.wantRuns {
				t.Errorf("verify invocations = %d, want %d", n, tc.wantRuns)
			}
			if n := countEvents(res.Events, "verify_lock_contended_retry"); n != tc.wantRetries {
				t.Errorf("verify_lock_contended_retry events = %d, want %d", n, tc.wantRetries)
			}
			short := strings.Contains(res.FailureReason, "No further in-place re-run was attempted")
			if short != tc.wantShort {
				t.Errorf("budget-skip note present = %t, want %t:\n%s", short, tc.wantShort, res.FailureReason)
			}
			wantCause := `"cause":"reruns_exhausted"`
			if tc.wantShort {
				wantCause = `"cause":"budget_short"`
			}
			if !strings.Contains(logSink.String(), wantCause) {
				t.Errorf("verify_lock_contended log line must carry %s:\n%s", wantCause, logSink.String())
			}
		})
	}
}

func TestVerifyLockRerunFits(t *testing.T) {
	if ok, _ := verifyLockRerunFits(context.Background(), time.Hour); !ok {
		t.Error("a context with no deadline must always fit")
	}
	short, cancel := context.WithTimeout(context.Background(), verifyLockWaitBudget)
	defer cancel()
	if ok, rem := verifyLockRerunFits(short, time.Minute); ok || rem <= 0 {
		t.Errorf("fits=%t remaining=%s with only the lock wait left, want false and a positive remainder", ok, rem)
	}
	ample, cancel2 := context.WithTimeout(context.Background(), verifyLockWaitBudget+2*time.Minute)
	defer cancel2()
	if ok, _ := verifyLockRerunFits(ample, time.Minute); !ok {
		t.Error("a deadline covering the lock wait plus one verify must fit")
	}
}

// gateClockWithRemaining builds a gate-phase stage clock with exactly rem of
// its verify budget left (verify.timeout = 1m, so the budget is 17m).
func gateClockWithRemaining(t *testing.T, rem time.Duration) *stagePhaseClock {
	t.Helper()
	c, mc := newTestPhaseClock(time.Hour, time.Minute)
	c.beginGate()
	c.startVerify()
	mc.advance(c.gateBudget - rem)
	c.endVerify()
	if got := c.gateRemaining(); got != rem {
		t.Fatalf("precondition: gate remaining = %s, want %s", got, rem)
	}
	return c
}

// TestVerifyLockRerunFits_GateBudgetBounds (CF-I vehicle): with NO ctx
// deadline the #4023 gate clock is the only bound, so a gate budget that
// cannot cover a lock wait plus one verify refuses the re-run, naming the
// gate budget; one that can admits it.
func TestVerifyLockRerunFits_GateBudgetBounds(t *testing.T) {
	short := withStagePhases(context.Background(), gateClockWithRemaining(t, verifyLockWaitBudget))
	if fits, rem, bound := verifyLockRerunBudget(short, time.Minute); fits || rem != verifyLockWaitBudget || bound != verifyLockBoundGateBudget {
		t.Errorf("short gate budget: (fits=%t, remaining=%s, bound=%q), want (false, %s, %q)", fits, rem, bound, verifyLockWaitBudget, verifyLockBoundGateBudget)
	}
	if fits, _ := verifyLockRerunFits(short, time.Minute); fits {
		t.Error("verifyLockRerunFits must refuse a re-run the gate budget cannot cover")
	}
	ample := withStagePhases(context.Background(), gateClockWithRemaining(t, verifyLockWaitBudget+2*time.Minute))
	if fits, _, bound := verifyLockRerunBudget(ample, time.Minute); !fits || bound != verifyLockBoundGateBudget {
		t.Errorf("ample gate budget: (fits=%t, bound=%q), want (true, %q)", fits, bound, verifyLockBoundGateBudget)
	}

	var lc verifyLockContention
	if lc.admit(short, time.Minute) {
		t.Fatal("admit must refuse when the gate budget is short")
	}
	r := lc.reason("scripts/test verify", 1, time.Minute)
	if !strings.Contains(r, "No further in-place re-run was attempted: the remaining gate budget") || !strings.Contains(r, "17 x executor.verify.timeout") {
		t.Errorf("reason = %q, want the note to name the gate budget", r)
	}
}

// TestVerifyLockRerunFits_NoClockKeepsCtxDeadline: without a clock the ctx
// deadline alone decides, as before #4023; with both, the TIGHTER wins and is
// the one named.
func TestVerifyLockRerunFits_NoClockKeepsCtxDeadline(t *testing.T) {
	if fits, rem, bound := verifyLockRerunBudget(context.Background(), time.Hour); !fits || rem != 0 || bound != verifyLockBoundNone {
		t.Errorf("no clock, no deadline: (%t, %s, %q), want (true, 0, none)", fits, rem, bound)
	}
	short, cancel := context.WithTimeout(context.Background(), verifyLockWaitBudget)
	defer cancel()
	if fits, _, bound := verifyLockRerunBudget(short, time.Minute); fits || bound != verifyLockBoundCtxDeadline {
		t.Errorf("no clock, short deadline: (fits=%t, bound=%q), want (false, %q)", fits, bound, verifyLockBoundCtxDeadline)
	}
	var lc verifyLockContention
	if lc.admit(short, time.Minute) {
		t.Fatal("admit must refuse when the ctx deadline is short")
	}
	if r := lc.reason("scripts/test verify", 1, time.Minute); !strings.Contains(r, "the remaining stage budget (the context deadline)") {
		t.Errorf("reason = %q, want the note to name the context deadline", r)
	}

	// Tight deadline, ample gate budget: the deadline binds.
	ampleGate := withStagePhases(short, gateClockWithRemaining(t, 10*time.Minute+verifyLockWaitBudget))
	if fits, _, bound := verifyLockRerunBudget(ampleGate, time.Minute); fits || bound != verifyLockBoundCtxDeadline {
		t.Errorf("short deadline + ample gate: (fits=%t, bound=%q), want (false, %q)", fits, bound, verifyLockBoundCtxDeadline)
	}
	// Ample deadline, tight gate budget: the gate binds.
	long, cancel2 := context.WithTimeout(context.Background(), time.Hour)
	defer cancel2()
	tightGate := withStagePhases(long, gateClockWithRemaining(t, time.Minute))
	if fits, rem, bound := verifyLockRerunBudget(tightGate, time.Minute); fits || rem != time.Minute || bound != verifyLockBoundGateBudget {
		t.Errorf("ample deadline + tight gate: (fits=%t, rem=%s, bound=%q), want (false, 1m, %q)", fits, rem, bound, verifyLockBoundGateBudget)
	}
}

// --- (E) the single-shot committed gate -----------------------------------

func TestRunVerifyGateCommitted_VerifyLockContendedOnceThenPass(t *testing.T) {
	repo, _, _ := verifiedTreeRepo(t)
	cmd, counter := contendedNTimesThenPassVerifyCmd(t, 1)
	var logSink strings.Builder
	events, tree, err := runVerifyGateCommitted(context.Background(), verifiedTreeCfg(repo, cmd), &logSink)
	if err != nil {
		t.Fatalf("runVerifyGateCommitted: %s", redactErr(err))
	}
	if tree == "" {
		t.Error("verified tree is empty, want the passing re-run's tree")
	}
	if n := readInvocations(t, counter); n != 2 {
		t.Errorf("verify invocations = %d, want 2", n)
	}
	if n := countEvents(events, "verify_lock_contended_retry"); n != 1 {
		t.Errorf("verify_lock_contended_retry events = %d, want 1", n)
	}
	if n := countEvents(events, "verify_infra_flake_retry"); n != 0 {
		t.Errorf("verify_infra_flake_retry events = %d, want 0", n)
	}
}

func TestRunVerifyGateCommitted_VerifyLockAlwaysContendedIsCategoryC(t *testing.T) {
	repo, _, _ := verifiedTreeRepo(t)
	cmd, counter := contendedCountingVerifyCmd(t)
	var logSink strings.Builder
	events, tree, err := runVerifyGateCommitted(context.Background(), verifiedTreeCfg(repo, cmd), &logSink)
	if !errors.Is(err, gitops.ErrVerifyInfraFailure) || !errors.Is(err, errVerifyLockContended) {
		t.Fatalf("err = %s, want ErrVerifyInfraFailure + errVerifyLockContended", redactErr(err))
	}
	if errors.Is(err, gitops.ErrCommittedTestsFailed) {
		t.Errorf("a contended gate must NOT wrap ErrCommittedTestsFailed: %s", redactErr(err))
	}
	if got := committedGateFailureCategory(err); got != "C" {
		t.Errorf("committedGateFailureCategory = %q, want C", got)
	}
	if !strings.Contains(err.Error(), verifyLockContendedLead+": ") {
		t.Errorf("error must carry the contended lead: %s", redactErr(err))
	}
	if tree != "" {
		t.Error("a contended gate must not yield a verified tree")
	}
	if n := readInvocations(t, counter); n != 3 {
		t.Errorf("verify invocations = %d, want 3", n)
	}
	if n := countEvents(events, "verify_run"); n != 3 {
		t.Errorf("verify_run events = %d, want 3", n)
	}
	if n := countEvents(events, "verify_infra_flake_retry"); n != 0 {
		t.Errorf("verify_infra_flake_retry events = %d, want 0 — contention must not spend the infra absorb", n)
	}
	if n := countEvents(events, "verify_lock_contended"); n != 1 {
		t.Errorf("verify_lock_contended events = %d, want 1", n)
	}
}

// --- (F)/(G) the #960 pre-push strict re-verify ----------------------------

// strictReverifyHarness gates the fixture with `true`, then moves the bare
// origin's main so the real commit's tree differs and the strict re-verify
// runs with verifyCmd. Callers install any seam BEFORE calling ship.
type strictReverifyHarness struct {
	repo, bare, branch string
	verifiedTree       string
	fpr                *fakePROpener
}

func newStrictReverifyHarness(t *testing.T) *strictReverifyHarness {
	t.Helper()
	repo, bare, branch := verifiedTreeRepo(t)
	h := &strictReverifyHarness{repo: repo, bare: bare, branch: branch, fpr: withFakePROpenerOnly(t)}
	_, tree, err := runVerifyGateCommitted(context.Background(), verifiedTreeCfg(repo, "true"), io.Discard)
	if err != nil || tree == "" {
		t.Fatalf("gate: tree=%q err=%s", tree, redactErr(err))
	}
	h.verifiedTree = tree
	moveBareMain(t, bare)
	return h
}

func (h *strictReverifyHarness) ship(t *testing.T, ctx context.Context, verifyCmd string) (string, error) {
	t.Helper()
	fu := newFakeUploader(t)
	issued, err := fu.IssueKey(context.Background(), verifiedTreeRunID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var logSink strings.Builder
	err = openPRAndShipArtifact(ctx, verifiedTreeCfg(h.repo, verifyCmd), &logSink, fu, issued, "", false, false, nil, false, h.verifiedTree, "", nil, nil, nil, nil)
	return logSink.String(), err
}

func (h *strictReverifyHarness) assertOriginUntouched(t *testing.T) {
	t.Helper()
	if h.fpr.gotArgs != nil {
		t.Error("OpenPR must not run — the classification changes the recovery verb, never the push decision")
	}
	if out, rerr := exec.Command("git", "--git-dir="+h.bare, "rev-parse", "--verify", "refs/heads/"+h.branch).Output(); rerr == nil {
		t.Errorf("run branch %s reached the bare remote despite the blocked push (tip %s)", h.branch, strings.TrimSpace(string(out)))
	}
}

// The issue's acceptance case: an always-contended strict re-verify ends
// category C (retryable in place), never ErrPushedTreeNotVerified, with
// origin untouched and the infra absorb unspent.
func TestOpenPRAndShipArtifact_VerifiedTreeMismatch_VerifyLockContendedIsCategoryC(t *testing.T) {
	h := newStrictReverifyHarness(t)
	cmd, counter := contendedCountingVerifyCmd(t)
	logs, err := h.ship(t, context.Background(), cmd)
	if !errors.Is(err, gitops.ErrVerifyInfraFailure) || !errors.Is(err, errVerifyLockContended) {
		t.Fatalf("err = %s, want ErrVerifyInfraFailure + errVerifyLockContended", redactErr(err))
	}
	if errors.Is(err, gitops.ErrPushedTreeNotVerified) {
		t.Errorf("a contended re-verify must NOT wrap ErrPushedTreeNotVerified: %s", redactErr(err))
	}
	if got := pushFailureCategory(err); got != "C" {
		t.Errorf("pushFailureCategory = %q, want C", got)
	}
	if !strings.Contains(err.Error(), verifyLockContendedLead) {
		t.Errorf("error must name %s: %s", verifyLockContendedLead, redactErr(err))
	}
	if n := strings.Count(logs, `"event":"verify_infra_flake_retry"`); n != 0 {
		t.Errorf("verify_infra_flake_retry log lines = %d, want 0", n)
	}
	if n := strings.Count(logs, `"event":"verify_lock_contended_retry"`); n != 2 {
		t.Errorf("verify_lock_contended_retry log lines = %d, want 2", n)
	}
	if n := strings.Count(logs, `"event":"verify_run"`); n != 3 {
		t.Errorf("re-verify verify_run records = %d, want 3 (every execution recorded)", n)
	}
	if n := readInvocations(t, counter); n != 3 {
		t.Errorf("re-verify invocations = %d, want 3", n)
	}
	h.assertOriginUntouched(t)
}

func TestOpenPRAndShipArtifact_VerifiedTreeMismatch_VerifyLockContendedOnceThenPushes(t *testing.T) {
	h := newStrictReverifyHarness(t)
	cmd, counter := contendedNTimesThenPassVerifyCmd(t, 1)
	logs, err := h.ship(t, context.Background(), cmd)
	if err != nil {
		t.Fatalf("openPRAndShipArtifact = %s, want nil (the contention must clear on the re-run)", redactErr(err))
	}
	if !strings.Contains(logs, `"event":"pushed_tree_reverified"`) {
		t.Error("expected pushed_tree_reverified after the re-run passed")
	}
	if n := readInvocations(t, counter); n != 2 {
		t.Errorf("re-verify invocations = %d, want 2", n)
	}
	if n := strings.Count(logs, `"event":"verify_infra_flake_retry"`); n != 0 {
		t.Errorf("verify_infra_flake_retry log lines = %d, want 0", n)
	}
	if h.fpr.gotArgs == nil {
		t.Error("OpenPR should have run after the re-verified push")
	}
	if _, rerr := exec.Command("git", "--git-dir="+h.bare, "rev-parse", "--verify", "refs/heads/"+h.branch).Output(); rerr != nil {
		t.Errorf("run branch %s missing from the bare remote: %v", h.branch, rerr)
	}
}

// swapMaterializer replaces materializeGateCheckoutFn for the test (no
// t.Parallel: the seam is package-level) and restores it on cleanup.
func swapMaterializer(t *testing.T, fn func(ctx context.Context, repoDir, headSHA, parent string) (string, error)) {
	t.Helper()
	prev := materializeGateCheckoutFn
	materializeGateCheckoutFn = fn
	t.Cleanup(func() { materializeGateCheckoutFn = prev })
}

// (G) A strict re-verify the gate infrastructure never ran: its throwaway
// clone cannot be materialized. One absorb re-run, then category C naming
// `outcome skipped` and the clone: detail, origin untouched. Before #3948 this
// was ErrPushedTreeNotVerified (category B).
func TestOpenPRAndShipArtifact_VerifiedTreeMismatch_SkippedReverifyIsCategoryC(t *testing.T) {
	h := newStrictReverifyHarness(t)
	calls := 0
	swapMaterializer(t, func(context.Context, string, string, string) (string, error) {
		calls++
		return "", errors.New("git clone --no-hardlinks: fatal: unable to create temporary file: No space left on device")
	})
	logs, err := h.ship(t, context.Background(), "true")
	if !errors.Is(err, gitops.ErrVerifyInfraFailure) {
		t.Fatalf("err = %s, want ErrVerifyInfraFailure", redactErr(err))
	}
	if errors.Is(err, gitops.ErrPushedTreeNotVerified) {
		t.Errorf("a skipped re-verify must NOT wrap ErrPushedTreeNotVerified: %s", redactErr(err))
	}
	if got := pushFailureCategory(err); got != "C" {
		t.Errorf("pushFailureCategory = %q, want C", got)
	}
	for _, want := range []string{"outcome skipped", "clone: ", "No space left on device"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q: %s", want, redactErr(err))
		}
	}
	if calls != 2 {
		t.Errorf("materializer calls = %d, want 2 (the skip and its one absorb re-run)", calls)
	}
	if n := strings.Count(logs, `"event":"verify_run"`); n != 2 {
		t.Errorf("re-verify verify_run records = %d, want 2", n)
	}
	if n := strings.Count(logs, `"event":"verify_infra_flake_retry"`); n != 1 {
		t.Errorf("verify_infra_flake_retry log lines = %d, want 1", n)
	}
	h.assertOriginUntouched(t)
}

// The skipped absorb re-run is a real re-run: a materialization failure that
// clears on the second attempt verifies and pushes.
func TestOpenPRAndShipArtifact_VerifiedTreeMismatch_SkippedReverifyRetriedThenPushes(t *testing.T) {
	h := newStrictReverifyHarness(t)
	calls := 0
	swapMaterializer(t, func(ctx context.Context, repoDir, headSHA, parent string) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("git clone --no-hardlinks: transient failure")
		}
		return materializeGateCheckout(ctx, repoDir, headSHA, parent)
	})
	logs, err := h.ship(t, context.Background(), "true")
	if err != nil {
		t.Fatalf("openPRAndShipArtifact = %s, want nil", redactErr(err))
	}
	if !strings.Contains(logs, `"event":"pushed_tree_reverified"`) {
		t.Error("expected pushed_tree_reverified after the absorbed skip")
	}
	if h.fpr.gotArgs == nil {
		t.Error("OpenPR should have run")
	}
}

// Approval condition 1: dispositions other than a gate-infrastructure skip
// keep their CURRENT (pre-#3948) classification at the strict re-verify and
// never spend the absorb. A cancelled context surfaces as "skipped" (a
// cancelled clone fails materialization) but is the runner shutting down; a
// refused selection and an unavailable container surface as "failed" with
// their own disposition. Each stays ErrPushedTreeNotVerified (category B).
func TestOpenPRAndShipArtifact_VerifiedTreeMismatch_NonInfraDispositionsNotReclassified(t *testing.T) {
	assertCurrentB := func(t *testing.T, h *strictReverifyHarness, err error, logs string) {
		t.Helper()
		if !errors.Is(err, gitops.ErrPushedTreeNotVerified) {
			t.Fatalf("err = %s, want the unchanged ErrPushedTreeNotVerified", redactErr(err))
		}
		if errors.Is(err, gitops.ErrVerifyInfraFailure) || errors.Is(err, errVerifyLockContended) {
			t.Errorf("must not be reclassified to category C: %s", redactErr(err))
		}
		if got := pushFailureCategory(err); got != "B" {
			t.Errorf("pushFailureCategory = %q, want B", got)
		}
		for _, never := range []string{`"event":"verify_infra_flake_retry"`, `"event":"verify_lock_contended_retry"`, `"event":"verify_lock_contended"`} {
			if strings.Contains(logs, never) {
				t.Errorf("%s fired — the disposition must not spend the absorb or a contention re-run", never)
			}
		}
		if n := strings.Count(logs, `"event":"verify_run"`); n != 1 {
			t.Errorf("re-verify verify_run records = %d, want 1", n)
		}
		h.assertOriginUntouched(t)
	}

	t.Run("cancelled context", func(t *testing.T) {
		h := newStrictReverifyHarness(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := 0
		swapMaterializer(t, func(ctx2 context.Context, repoDir, headSHA, parent string) (string, error) {
			calls++
			// Cancel at the precise site, then let the REAL materializer meet
			// the cancelled context — the production shape of a shutdown.
			cancel()
			return materializeGateCheckout(ctx2, repoDir, headSHA, parent)
		})
		logs, err := h.ship(t, ctx, "true")
		if calls != 1 {
			t.Errorf("materializer calls = %d, want 1 (no absorb re-run)", calls)
		}
		assertCurrentB(t, h, err, logs)
	})

	t.Run("refused gate isolation", func(t *testing.T) {
		h := newStrictReverifyHarness(t)
		installGateState(t, refusedState(io.Discard))
		logs, err := h.ship(t, context.Background(), "true")
		assertCurrentB(t, h, err, logs)
	})

	t.Run("unavailable gate container", func(t *testing.T) {
		h := newStrictReverifyHarness(t)
		unavailableContainerState(t)
		logs, err := h.ship(t, context.Background(), "true")
		assertCurrentB(t, h, err, logs)
	})
}

// reverifySkipGateInfraDetail's full decision table, including the
// worktree_tmp: arm no integration fixture can force without breaking the
// rest of the push path's temp-file use.
func TestReverifySkipGateInfraDetail(t *testing.T) {
	live := context.Background()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	skip := func(output string) agent.Event {
		return verifyRunEvent("scripts/test verify", "h", "t", -1, output, "skipped")
	}
	cases := []struct {
		name       string
		ctx        context.Context
		ev         agent.Event
		disp       gateDisposition
		wantOK     bool
		wantDetail string
	}{
		{"clone failure", live, skip("clone: fatal: boom"), gateExecuted, true, "clone: fatal: boom"},
		{"temp dir failure", live, skip("worktree_tmp: mkdir: permission denied"), gateExecuted, true, "worktree_tmp: mkdir: permission denied"},
		{"cancelled context", cancelled, skip("clone: context canceled"), gateExecuted, false, ""},
		{"refused disposition", live, skip("clone: x"), gateRefused, false, ""},
		{"unavailable disposition", live, skip("clone: x"), gateUnavailable, false, ""},
		{"unrecognised skip reason", live, skip("no scope-only changes to gate"), gateExecuted, false, ""},
		{"a failed outcome", live, verifyRunEvent("c", "h", "t", 1, "clone: printed by a test", "failed"), gateExecuted, false, ""},
		{"undecodable payload", live, agent.Event{Kind: "verify_run", Payload: json.RawMessage(`{`)}, gateExecuted, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detail, ok := reverifySkipGateInfraDetail(tc.ctx, tc.ev, tc.disp)
			if ok != tc.wantOK || detail != tc.wantDetail {
				t.Errorf("= (%q, %t), want (%q, %t)", detail, ok, tc.wantDetail, tc.wantOK)
			}
		})
	}
}

// The rendered reason the backend's retry-admission test copies byte-for-byte
// (backend/internal/run/retry_test.go, TestRetryStage_VerifyLockContendedIsRetryableInPlace).
// A wording change here must update that literal in the same change.
func TestVerifyLockContendedReasonRendering(t *testing.T) {
	const want = `verify_lock_contended: "scripts/test verify" could not acquire the per-repository verify lock after 3 attempt(s): scripts/test refused because another verify held it past its wait budget; the change was NOT judged. Retry the stage in place once the other run's verify has finished.`
	if got := verifyLockContendedReason("scripts/test verify", 3); got != want {
		t.Errorf("verifyLockContendedReason drifted from the backend retry test's copy:\n got %q\nwant %q\n(update backend/internal/run/retry_test.go in lockstep)", got, want)
	}
}
