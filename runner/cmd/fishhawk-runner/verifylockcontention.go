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

// The #3948 vocabulary for a committed-tree verify that could not acquire the
// per-repository verify lock, held in ONE place so the three committed-tree
// gate sites (runVerifyFixLoop, runVerifyGateCommitted and the #960 strict
// re-verify) bound, log and render it identically.
//
// Before #3948 every site read that refusal as an ordinary red verify: the
// strict re-verify turned it into ErrPushedTreeNotVerified (category B, which
// retry_stage and revive_run both refuse — stranding a decomposition child),
// and the verify-fix loop spent a fix-agent re-invoke on a tree that was never
// judged. Contention is a property of the HOST (two runs' verifies racing for
// one lock), never of the diff, so it is re-run in place a bounded number of
// times and then classified category C (retryable in place).
//
// RECOGNITION is by exit code since #4022: the verify command exits 73
// (EX_CANTCREAT) on a lock refusal and runVerifyCommittedTree maps that to
// gateLockRefused (verifyExitDisposition, verifyexit.go). The #3948 text
// classifier that scraped scripts/test's refusal literals out of the output is
// deleted; refusal TEXT on any other exit is an ordinary failure.
//
// The runner adds NO sleep or backoff between re-runs, deliberately. A
// RUNNER-kind `scripts/test verify` already polls the lock every 2s for up to
// 600s before it refuses (_acquire_verify_lock_at, VERIFY_LOCK_WAIT_SECONDS /
// VERIFY_LOCK_POLL_SECONDS), so each re-run IS a bounded wait. An idle runner
// sleep would only miss the brief release gap between a holder's back-to-back
// scoped, full and strict verifies — the very window a re-run exists to catch.

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
// by TestVerifyExitCodesMatchScriptsTest.
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
// contended gate (the command exited 73 on every attempt). The wording is
// copied byte-for-byte by backend/internal/run/retry_test.go, so it is kept
// stable.
func verifyLockContendedReason(verifyCmd string, attempts int) string {
	return fmt.Sprintf("%s: %q could not acquire the per-repository verify lock after %d attempt(s): scripts/test refused because another verify held it past its wait budget; the change was NOT judged. Retry the stage in place once the other run's verify has finished.",
		verifyLockContendedLead, verifyCmd, attempts)
}

// verifyLockRerunFits reports whether the remaining stage budget can cover one
// more contention re-run: a full lock wait (verifyLockWaitBudget) plus one
// verify (verifyTimeout, executor.verify.timeout — the only bound the runner
// holds on a single verify's duration).
//
// Two budgets can bound it, and the tighter one wins (verifyLockRerunBudget):
//
//   - the #4023 GATE BUDGET, when the stage clock rides ctx
//     (stagePhasesFrom): the verify-execution time the gate phase has left.
//     A re-run is a verify execution charged to it, so one the budget cannot
//     cover would be clipped and killed mid-wait.
//   - the context's deadline, when one is set (production sets none today;
//     cfg.timeout bounds each agent invocation only).
//
// A re-run started without the budget to finish would end killed mid-verify —
// classified as a red tree or a timeout, a worse verdict than the contention
// it was meant to clear. With neither budget present it always fits; the
// re-run count bound still applies. remaining is the binding budget's time
// left (zero when there is none).
func verifyLockRerunFits(ctx context.Context, verifyTimeout time.Duration) (bool, time.Duration) {
	fits, remaining, _ := verifyLockRerunBudget(ctx, verifyTimeout)
	return fits, remaining
}

// Which budget bound a verify-lock re-run admission (verifyLockRerunBudget).
const (
	verifyLockBoundNone        = ""
	verifyLockBoundGateBudget  = "gate_budget"
	verifyLockBoundCtxDeadline = "ctx_deadline"
)

// verifyLockRerunBudget is verifyLockRerunFits plus WHICH budget bound the
// decision: verifyLockBoundGateBudget, verifyLockBoundCtxDeadline, or
// verifyLockBoundNone when neither is present.
func verifyLockRerunBudget(ctx context.Context, verifyTimeout time.Duration) (bool, time.Duration, string) {
	var (
		remaining time.Duration
		bound     = verifyLockBoundNone
	)
	if deadline, ok := ctx.Deadline(); ok {
		remaining = time.Until(deadline)
		bound = verifyLockBoundCtxDeadline
	}
	if c := stagePhasesFrom(ctx); c != nil {
		if g := c.gateRemaining(); bound == verifyLockBoundNone || g < remaining {
			remaining = g
			bound = verifyLockBoundGateBudget
		}
	}
	if bound == verifyLockBoundNone {
		return true, 0, bound
	}
	return remaining >= verifyLockWaitBudget+verifyTimeout, remaining, bound
}

// verifyLockContention is one gate site's contention state: the re-runs
// already spent and, once it gives up, why.
type verifyLockContention struct {
	reruns      int
	budgetShort bool          // the count allowed another re-run but the stage budget did not
	remaining   time.Duration // the budget left when budgetShort was decided
	bound       string        // which budget decided budgetShort (verifyLockBound*)
}

// admit decides whether the site may re-run a contended verify once more:
// below the re-run bound AND inside the stage budget (verifyLockRerunBudget).
// It counts the re-run on admission. On refusal for budget it records the
// remaining budget, and which budget it was, so the reason can name it.
func (c *verifyLockContention) admit(ctx context.Context, verifyTimeout time.Duration) bool {
	if c.reruns >= verifyLockContentionMaxReruns {
		return false
	}
	if fits, remaining, bound := verifyLockRerunBudget(ctx, verifyTimeout); !fits {
		c.budgetShort = true
		c.remaining = remaining
		c.bound = bound
		return false
	}
	c.reruns++
	return true
}

// reason renders the contended FailureReason lead, adding a note when the
// last re-run was skipped because a budget could not cover it — naming the
// gate budget or the context deadline, whichever bound it.
func (c *verifyLockContention) reason(verifyCmd string, attempts int, verifyTimeout time.Duration) string {
	r := verifyLockContendedReason(verifyCmd, attempts)
	if c.budgetShort {
		budget := "stage budget (the context deadline)"
		if c.bound == verifyLockBoundGateBudget {
			budget = fmt.Sprintf("gate budget (%d x executor.verify.timeout of verify-execution time per gate phase)", gateBudgetVerifyForms)
		}
		r += fmt.Sprintf(" No further in-place re-run was attempted: the remaining %s, %s, cannot cover a full verify-lock wait (%s) plus one verify (%s).",
			budget, c.remaining.Round(time.Second), verifyLockWaitBudget, verifyTimeout)
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
