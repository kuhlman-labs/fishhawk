package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// The #3948 vocabulary for a committed-tree verify that `scripts/test` REFUSED
// because another live RUNNER verify held the per-repository verify lock past
// its wait budget, held in ONE place so the three committed-tree gate sites
// (runVerifyFixLoop, runVerifyGateCommitted and the #960 strict re-verify)
// recognise, bound, log and render it identically.
//
// Before #3948 every site read that refusal as an ordinary red verify: the
// strict re-verify turned it into ErrPushedTreeNotVerified (category B, which
// retry_stage and revive_run both refuse — stranding a decomposition child),
// and the verify-fix loop spent a fix-agent re-invoke on a tree that was never
// judged. Contention is a property of the HOST (two runs' verifies racing for
// one lock), never of the diff, so it is re-run in place a bounded number of
// times and then classified category C (retryable in place).
//
// The runner adds NO sleep or backoff between re-runs, deliberately. A
// RUNNER-kind `scripts/test verify` already polls the lock every 2s for up to
// 600s before it refuses (_acquire_verify_lock_at, VERIFY_LOCK_WAIT_SECONDS /
// VERIFY_LOCK_POLL_SECONDS), so each re-run IS a bounded wait. An idle runner
// sleep would only miss the brief release gap between a holder's back-to-back
// scoped, full and strict verifies — the very window a re-run exists to catch.

// verifyLockRefusalSignatures are the refusal literals a RUNNER-kind
// `scripts/test verify` prints when it cannot take the verify lock. Each is a
// substring of scripts/test's own text, pinned against the real file by
// TestVerifyLockRefusalSignaturesMatchScriptsTest and against its real
// executable behaviour by TestRealScriptsTestRunnerRefusalIsVerifyLockContended:
//
//   - "another RUNNER verify still holds the lock": the live-runner arm of
//     _acquire_verify_lock_at once the wait budget is spent — a runner never
//     displaces a peer runner, it refuses;
//   - "was retaken after a displacement": the lock was re-taken by another
//     invocation after this one displaced a wedged SHELL holder once, and the
//     script refuses rather than displace twice;
//   - "the verify lock never settled": VERIFY_LOCK_UNSETTLED_MSG, the restart
//     budget spent on a lock that kept changing hands.
//
// The SHELL-kind immediate refusal ("another verify already holds the lock")
// is deliberately NOT a member: the runner's committed-tree gate always runs
// scripts/test as RUNNER kind (verifyLockOwnerEnv is injected unconditionally
// in runVerifyCommittedTree), so that text can only reach a runner gate from
// output the change itself printed. Neither is golangci-lint's own lock
// ("parallel golangci-lint is running", golangciLintLockSignature): lint
// contention is serialized by --allow-serial-runners (#3962) and, where it
// still surfaces, stays the #2645 infra absorb's (isVerifyInfraFailure).
var verifyLockRefusalSignatures = []string{
	"another RUNNER verify still holds the lock",
	"was retaken after a displacement",
	"the verify lock never settled",
}

// verifyLockRefusalLinePrefix is the prefix every line scripts/test prints
// about its verify lock carries — the refusal itself and everything that can
// precede it (the "waiting up to …" announcement, a displacement warning).
const verifyLockRefusalLinePrefix = "scripts/test: "

// isVerifyLockContended reports whether a failed verify's output is a
// scripts/test verify-lock refusal.
//
// It is POSITION-ANCHORED, not a bare substring match: cmd_verify takes the
// lock BEFORE it runs any leg (lint, schema-sync, harnesses, tests), so a real
// refusal is printed before any lint or test output. The matcher therefore
// walks the output's non-empty lines in order and requires every line up to
// and including the one carrying a signature to begin with
// verifyLockRefusalLinePrefix. The first line that does not ends the search
// with false. A signature printed by a failing test — which can only appear
// after the lock was taken and lint has already run — is not recognised.
//
// The anchor assumes the verify command's own output begins with scripts/test's
// lock lines, which holds for this repository's `scripts/test verify`. A
// wrapper that prints its own preamble first (a `make` echo, a shell `set -x`)
// makes the matcher miss a genuine refusal. That degrades to the pre-#3948
// classification, the safe direction.
//
// RESIDUAL — the input is UNTRUSTED, exactly as isVerifyInfraFailure's
// residual states, and the anchor narrows it without closing it. A diff that
// makes the verify command's FIRST output a scripts/test-prefixed line carrying
// a signature (for example by editing scripts/test itself to print it before
// failing) steers its own red verify to category C. The real cost, named rather
// than left abstract: at the verify-fix loop that red tree NEVER reaches the fix
// agent — the loop spends verifyLockContentionMaxReruns fast-failing re-runs and
// ends category C `verify_lock_contended` instead of re-invoking the agent with
// the failure and, on exhaustion, category A. At the single-shot gate and the
// strict re-verify it trades a category-B park for a retryable C. It is NEVER a
// verified-tree bypass: every push still requires an explicit "passed" verify
// outcome, which this classifier never produces, and a retry re-runs the same
// command and re-fails. A stage that keeps ending verify_lock_contended with no
// other run's verify live on the host should be inspected, not retried.
func isVerifyLockContended(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, verifyLockRefusalLinePrefix) {
			return false
		}
		for _, sig := range verifyLockRefusalSignatures {
			if strings.Contains(line, sig) {
				return true
			}
		}
	}
	return false
}

// verifyLockContentionMaxReruns bounds the in-place re-runs ONE gate site
// spends on a contended verify, so a contended verify executes at most
// 1 + verifyLockContentionMaxReruns = 3 times per site before the stage ends
// category C. scripts/test's lock wait runs INSIDE the verify command, so each
// execution — wait and verify together — is bounded by executor.verify.timeout.
// The worst-case wall clock a site adds is therefore
// verifyLockContentionMaxReruns x executor.verify.timeout, and a stage can pay
// it at both the verify-fix loop (or the single-shot gate) and the strict
// re-verify. That is accepted against the alternative #3948 documents: an
// approved implement pass discarded as category B and a full re-plan.
const verifyLockContentionMaxReruns = 2

// verifyLockWaitBudget mirrors scripts/test's VERIFY_LOCK_WAIT_SECONDS=600 —
// how long a RUNNER-kind verify polls a live runner holder before refusing.
// The runner's gate env is a default-deny allow-list, so
// FISHHAWK_VERIFY_LOCK_WAIT_SECONDS never reaches the gate subprocess and
// scripts/test always uses this default there. Pinned against the script text
// by TestVerifyLockRefusalSignaturesMatchScriptsTest.
const verifyLockWaitBudget = 600 * time.Second

// errVerifyLockContended is the sentinel a committed-tree gate wraps
// (alongside gitops.ErrVerifyInfraFailure, which is what makes
// committedGateFailureCategory / pushFailureCategory resolve it to C) when the
// verify lock stayed contended: distinguishable from a timeout, a refusal, an
// unavailable container and a persistent infra signature with errors.Is.
var errVerifyLockContended = errors.New("verify lock contended")

// verifyLockContendedLead is the literal every contended FailureReason / error
// text leads with, so an operator — and any later failure-signature hint — can
// name the cause without reading the output.
const verifyLockContendedLead = "verify_lock_contended"

// verifyLockContendedReason renders the FailureReason / error LEAD for a
// contended gate.
func verifyLockContendedReason(verifyCmd string, attempts int) string {
	return fmt.Sprintf("%s: %q could not acquire the per-repository verify lock after %d attempt(s): scripts/test refused because another verify held it past its wait budget; the change was NOT judged. Retry the stage in place once the other run's verify has finished.",
		verifyLockContendedLead, verifyCmd, attempts)
}

// verifyLockRerunFits reports whether the remaining stage budget can cover one
// more contention re-run: a full lock wait (verifyLockWaitBudget) plus one
// verify (verifyTimeout, executor.verify.timeout — the only bound the runner
// holds on a verify's duration).
//
// The budget is the context's deadline: the runner holds no stage-wide wall
// clock of its own (cfg.timeout bounds each agent invocation, and every fix
// re-invoke gets it afresh), so a deadline on ctx is the only one that can cut
// a re-run short from outside. A re-run started without the budget to finish
// would end in a context cancellation mid-verify — a killed command
// classified as a red tree or a timeout, which is a worse verdict than the
// contention it was meant to clear. A context with no deadline always fits;
// the re-run count bound still applies. remaining is the time left on the
// deadline (zero when there is none).
func verifyLockRerunFits(ctx context.Context, verifyTimeout time.Duration) (bool, time.Duration) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return true, 0
	}
	remaining := time.Until(deadline)
	return remaining >= verifyLockWaitBudget+verifyTimeout, remaining
}

// verifyLockContention is one gate site's contention state: the re-runs
// already spent and, once it gives up, why.
type verifyLockContention struct {
	reruns      int
	budgetShort bool          // the count allowed another re-run but the stage budget did not
	remaining   time.Duration // the budget left when budgetShort was decided
}

// admit decides whether the site may re-run a contended verify once more:
// below the re-run bound AND inside the stage budget (verifyLockRerunFits). It
// counts the re-run on admission. On refusal for budget it records the
// remaining budget so the reason can name it.
func (c *verifyLockContention) admit(ctx context.Context, verifyTimeout time.Duration) bool {
	if c.reruns >= verifyLockContentionMaxReruns {
		return false
	}
	if fits, remaining := verifyLockRerunFits(ctx, verifyTimeout); !fits {
		c.budgetShort = true
		c.remaining = remaining
		return false
	}
	c.reruns++
	return true
}

// reason renders the contended FailureReason lead, adding a note when the
// last re-run was skipped because the stage budget could not cover it.
func (c *verifyLockContention) reason(verifyCmd string, attempts int, verifyTimeout time.Duration) string {
	r := verifyLockContendedReason(verifyCmd, attempts)
	if c.budgetShort {
		r += fmt.Sprintf(" No further in-place re-run was attempted: the remaining stage budget (%s) cannot cover a full verify-lock wait (%s) plus one verify (%s).",
			c.remaining.Round(time.Second), verifyLockWaitBudget, verifyTimeout)
	}
	return r
}

// logVerifyLockContendedRerun writes the verify_lock_contended_retry JSONL line
// a gate site emits before each in-place re-run of a contended verify, and
// returns the matching trace event.
func logVerifyLockContendedRerun(logSink io.Writer, cfg config, iteration, rerun int) agent.Event {
	_, _ = fmt.Fprintf(logSink,
		`{"event":"verify_lock_contended_retry","run_id":%q,"stage_id":%q,"iteration":%d,"rerun":%d,"max_reruns":%d}`+"\n",
		cfg.runID, cfg.stageID, iteration, rerun, verifyLockContentionMaxReruns)
	return agent.Event{
		Kind: "verify_lock_contended_retry",
		Payload: agent.MakePayload(map[string]any{
			"iteration":  iteration,
			"rerun":      rerun,
			"max_reruns": verifyLockContentionMaxReruns,
		}),
	}
}

// logVerifyLockContended writes the terminal verify_lock_contended JSONL line a
// gate site emits when it gives up on a contended verify, and returns the
// matching trace event. cause is "reruns_exhausted" or "budget_short".
func logVerifyLockContended(logSink io.Writer, cfg config, iteration, attempts int, c *verifyLockContention) agent.Event {
	cause := "reruns_exhausted"
	if c.budgetShort {
		cause = "budget_short"
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"verify_lock_contended","run_id":%q,"stage_id":%q,"iteration":%d,"attempts":%d,"cause":%q,"command":%q}`+"\n",
		cfg.runID, cfg.stageID, iteration, attempts, cause, cfg.verifyCmd)
	return agent.Event{
		Kind: "verify_lock_contended",
		Payload: agent.MakePayload(map[string]any{
			"iteration": iteration,
			"attempts":  attempts,
			"cause":     cause,
			"command":   cfg.verifyCmd,
		}),
	}
}

// reverifySkipGateInfraDetail decides whether a strict re-verify that ended
// outcome "skipped" was skipped for a GATE-INFRASTRUCTURE cause, and returns
// the runner-authored detail naming it.
//
// runVerifyCommittedTree yields "skipped" on exactly two paths, both with
// gateExecuted and both with a runner-authored reason as the verify_run
// payload's output: the throwaway temp dir could not be created
// ("worktree_tmp: …") or the throwaway clone could not be materialized
// ("clone: …"). The other dispositions never surface as "skipped": a refused
// selection, an unavailable container, a checkout the seed refused and a
// timed-out gate all return outcome "failed" with their own disposition. A
// CANCELLED context does surface as "skipped" — a cancelled `git clone` fails
// materialization — but it is the runner shutting down, not the gate's
// infrastructure, so it is excluded here and keeps its pre-#3948
// classification. Only a live context, gateExecuted and one of the two
// runner-authored prefixes qualify.
func reverifySkipGateInfraDetail(ctx context.Context, ev agent.Event, disp gateDisposition) (string, bool) {
	if ctx.Err() != nil || disp != gateExecuted {
		return "", false
	}
	var p struct {
		Output  string `json:"output"`
		Outcome string `json:"outcome"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil || p.Outcome != "skipped" {
		return "", false
	}
	if strings.HasPrefix(p.Output, "clone: ") || strings.HasPrefix(p.Output, "worktree_tmp: ") {
		return p.Output, true
	}
	return "", false
}
