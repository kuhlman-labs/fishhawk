package main

// Merge-candidate verify pass (ADR-090 D3 / #4018).
//
// When the backend authorizes a verify-only pass for a head it did not see
// gated on the committed tree (a performed base merge, a conflict-resolution
// push, a fan-in integration), the implement stage is re-opened and the prompt
// response carries merge_candidate_verify plus the run branch, the expected
// head and the cause. The runner then runs ONLY the declared verify command, in
// its FULL form, in the existing ADR-063 isolated committed-tree gate, against
// the run-branch tip fetched fresh from the remote — and reports exactly one
// merge_candidate_verified outcome. No agent, no commit, no push.
//
// The pass is routed in run() BEFORE the lineage-worktree block and before
// pinBase, so #3973's declared-base seeding and cfg.pinnedBaseSHA are never
// reached from it, and CommitAndPush is never called.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// mergeCandidateVerifyRequest is the runner's view of the backend's verify-only
// instruction. It exists only in the fully-populated form:
// mergeCandidateVerifyFromPrompt returns nil for anything less.
type mergeCandidateVerifyRequest struct {
	Branch          string
	ExpectedHeadSHA string
	Cause           string
}

// mergeCandidateVerifyFromPrompt reads the instruction off a fetched prompt,
// returning nil when there is no pass to run. A HALF-populated instruction is
// treated as NO pass: a runner handed an empty branch or expected head would
// report a verdict about nothing, and the backend would bind it to a head it
// never verified.
func mergeCandidateVerifyFromPrompt(got *upload.FetchedPrompt) *mergeCandidateVerifyRequest {
	if got == nil || !got.MergeCandidateVerify {
		return nil
	}
	if got.MergeCandidateVerifyBranch == "" ||
		got.MergeCandidateVerifyExpectedHeadSHA == "" ||
		got.MergeCandidateVerifyCause == "" {
		return nil
	}
	return &mergeCandidateVerifyRequest{
		Branch:          got.MergeCandidateVerifyBranch,
		ExpectedHeadSHA: got.MergeCandidateVerifyExpectedHeadSHA,
		Cause:           got.MergeCandidateVerifyCause,
	}
}

// Result values the pass reports. They mirror the backend's closed set
// (backend/internal/server/merge_candidate_verify.go).
const (
	mergeCandidatePassed      = "passed"
	mergeCandidateFailed      = "failed"
	mergeCandidateNotExecuted = "not_executed"
)

// not_executed reasons. A not_executed result never binds a verdict to the
// head: the backend leaves it re-triggerable.
const (
	mcReasonNoVerifyCommand   = "no_verify_command"
	mcReasonTreeUnavailable   = "tree_unavailable"
	mcReasonBranchFetchFailed = "branch_fetch_failed"
	mcReasonHeadMoved         = "head_moved"
	mcReasonGateSkipped       = "verify_gate_skipped"
	mcReasonGateRefused       = "verify_gate_refused"
	mcReasonGateUnavailable   = "verify_gate_unavailable"
	mcReasonGateTimedOut      = "verify_gate_timed_out"
	mcReasonLockContended     = "verify_lock_contended"
)

// mergeCandidateDefaultVerifyTimeout is the fallback when neither the operator
// flag nor the spec resolved a verify timeout — the same 10m every other
// committed-tree gate site falls back to.
const mergeCandidateDefaultVerifyTimeout = 10 * time.Minute

// mergeCandidateReportTimeout bounds the detached terminal report so it
// outlives a cancellation of the pass: an unreported pass strands the stage in
// `running`.
var mergeCandidateReportTimeout = 2 * time.Minute

// fetchMergeCandidateTip fetches the run branch's live tip into
// refs/remotes/<remote>/<branch> WITHOUT moving any working tree or checkout.
// A package var for the same reason fetchConflictBranchTip is: tests must
// never fetch against the runner's own source repository, and they wrap it to
// record what was fetched.
var fetchMergeCandidateTip = gitops.FetchBaseTip

// mergeCandidateVerifyFn is the committed-tree verify seam. Production is
// runVerifyCommittedTree — the ADR-063 isolated gate — byte-identically; tests
// wrap it to count invocations and to record the scope set it was handed.
var mergeCandidateVerifyFn = runVerifyCommittedTree

// mergeCandidateOutcome is the pass's report: the result, a reason for a
// non-pass, and the redacted, bounded output tail.
type mergeCandidateOutcome struct {
	HeadSHA    string
	Result     string
	Reason     string
	OutputTail string
}

// runMergeCandidateVerifyStage is the runner's whole merge-candidate verify
// stage. It reports EXACTLY ONE merge_candidate_verified outcome on every path
// and returns the process exit code: exitOK after a delivered report (the
// verdict, red or green, is the backend's to act on), exitFailure when the
// report itself could not be delivered.
func runMergeCandidateVerifyStage(ctx context.Context, cfg config, req mergeCandidateVerifyRequest,
	client uploadClient, issued *upload.IssuedKey, logSink io.Writer) int {
	logEvent(logSink, "merge_candidate_verify_started", map[string]string{
		"branch": req.Branch, "expected_head_sha": req.ExpectedHeadSHA, "cause": req.Cause,
	})

	res := mergeCandidateVerify(ctx, cfg, req, client, issued, logSink)

	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mergeCandidateReportTimeout)
	defer cancel()
	if _, err := client.ShipPullRequest(reportCtx, upload.ShipPullRequestArgs{
		RunID:                    cfg.runID,
		StageID:                  cfg.stageID,
		PrivateKey:               issued.PrivateKey,
		Outcome:                  upload.OutcomeMergeCandidateVerified,
		Branch:                   req.Branch,
		HeadSHA:                  res.HeadSHA,
		MergeCandidateResult:     res.Result,
		MergeCandidateReason:     res.Reason,
		MergeCandidateOutputTail: res.OutputTail,
	}); err != nil {
		logEvent(logSink, "merge_candidate_verify_report_failed", map[string]string{"error": err.Error()})
		return exitFailure
	}
	logEvent(logSink, "merge_candidate_verify_reported", map[string]string{
		"branch": req.Branch, "head_sha": res.HeadSHA, "result": res.Result, "reason": res.Reason,
	})
	return exitOK
}

// mergeCandidateVerify performs the pass and returns its outcome. It never
// writes to any working tree, branch or remote: the only side effect is the
// remote-tracking-ref refresh of the run-branch fetch.
func mergeCandidateVerify(ctx context.Context, cfg config, req mergeCandidateVerifyRequest,
	client uploadClient, issued *upload.IssuedKey, logSink io.Writer) mergeCandidateOutcome {
	notExecuted := func(reason string) mergeCandidateOutcome {
		return mergeCandidateOutcome{HeadSHA: req.ExpectedHeadSHA, Result: mergeCandidateNotExecuted, Reason: reason}
	}

	if strings.TrimSpace(cfg.verifyCmd) == "" {
		return notExecuted(mcReasonNoVerifyCommand)
	}

	dispatchDir := cfg.workingDir
	if dispatchDir == "" {
		dispatchDir = "."
	}
	if !isGitWorkTree(ctx, dispatchDir) {
		return notExecuted(mcReasonTreeUnavailable + ": dispatch checkout " + dispatchDir + " is empty or not a git work tree")
	}

	// A mint failure degrades to ambient auth (#1951); it is never a refusal.
	token := mintBaseAuthToken(ctx, cfg, client, issued, logSink)
	tip, err := fetchMergeCandidateTip(ctx, dispatchDir, gitops.DefaultRemote, req.Branch, token)
	if err != nil {
		// A fetch error can echo a token-in-URL remote; redact before it is
		// shipped as the reason (#3338).
		detail, _ := redactString(mcReasonBranchFetchFailed + ": fetch run-branch tip of " + req.Branch + ": " + err.Error())
		return notExecuted(detail)
	}
	// EXPECTED-HEAD CHECK. A verdict binds to EXACTLY one SHA (D2); verifying a
	// tip the trigger did not name would bind the result to the wrong head.
	if tip != req.ExpectedHeadSHA {
		return notExecuted(fmt.Sprintf("%s: remote tip of %s is %s, the pass was anchored to %s",
			mcReasonHeadMoved, req.Branch, tip, req.ExpectedHeadSHA))
	}

	timeout := cfg.verifyTimeout
	if timeout == 0 {
		timeout = mergeCandidateDefaultVerifyTimeout
	}
	// FULL form: a nil scope set leaves FISHHAWK_VERIFY_PACKAGES absent. The
	// merge candidate is the combined tree, so no package narrowing applies.
	_, out, outcome, disp := mergeCandidateVerifyFn(ctx, cfg.verifyCmd, dispatchDir, tip, timeout, nil)
	tail := mergeCandidateTail(out)

	result, reason := classifyMergeCandidateVerify(outcome, disp, out)
	return mergeCandidateOutcome{HeadSHA: tip, Result: result, Reason: reason, OutputTail: tail}
}

// classifyMergeCandidateVerify maps the gate's (outcome, disposition) to the
// pass's result. A gate that never reached a verdict about the TREE — infra
// skip, isolation refusal, an unavailable host, the runner's own deadline, or
// verify-lock contention — is not_executed, NEVER failed: a failed result is
// head-bound and routes a fix-up, which would hand the fix agent a tree that
// was never judged.
func classifyMergeCandidateVerify(outcome string, disp gateDisposition, out string) (string, string) {
	switch {
	case disp == gateRefused:
		return mergeCandidateNotExecuted, mcReasonGateRefused
	case disp == gateUnavailable:
		return mergeCandidateNotExecuted, mcReasonGateUnavailable
	case disp == gateTimedOut:
		return mergeCandidateNotExecuted, mcReasonGateTimedOut
	case outcome == "skipped":
		return mergeCandidateNotExecuted, mcReasonGateSkipped
	case outcome == "passed":
		return mergeCandidatePassed, ""
	case isVerifyLockContended(out):
		return mergeCandidateNotExecuted, mcReasonLockContended
	default:
		return mergeCandidateFailed, "verify_failed"
	}
}

// mergeCandidateTail redacts the whole verify output and THEN keeps its last
// upload.MaxMergeCandidateOutputTailBytes bytes. Redacting before the cut
// matters: a secret split by the cut would no longer match its pattern and its
// surviving half would ship. The committed-gate output is truncated but not
// redacted on the ordinary ship path, so the pass redacts it explicitly.
func mergeCandidateTail(out string) string {
	redacted, _ := redactString(out)
	if len(redacted) <= upload.MaxMergeCandidateOutputTailBytes {
		return redacted
	}
	return strings.ToValidUTF8(redacted[len(redacted)-upload.MaxMergeCandidateOutputTailBytes:], "")
}
