package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// The #3383 vocabulary for a verify gate the RUNNER's own timeout killed
// before it reached a verdict, held in ONE place so the four gate sites
// (runVerifyFixLoop, runVerifyGateCommitted, the #960 strict re-verify and
// the working-tree runVerifyGate) render the same lead, the same trailer and
// the same events.
//
// Classification never reads any of this text (#3448 discipline): every site
// decides on the out-of-band gateDisposition / errVerifyGateTimedOut sentinel.
// The trailer and the lead are operator-facing TEXT only, so a test that
// prints them cannot steer its own red tree to category C.

// errVerifyGateTimedOut is the sentinel a gate site wraps (alongside
// gitops.ErrVerifyInfraFailure at the committed-tree gates) when the verify
// command was killed by the runner's own deadline: distinguishable from a
// refusal, an unavailable container and a persistent infra signature with
// errors.Is.
var errVerifyGateTimedOut = errors.New("verify gate timed out")

// verifyGateTimedOutLead is the literal every timed-out FailureReason / error
// text BEGINS with. CROSS-MODULE STRING CONTRACT (#1548 convention): the
// backend's failure-signature catalog mirrors it as
// backend/internal/failuresig.AnchorVerifyGateTimedOut and requires the
// stage's failure category to be C alongside it. A wording change here
// silently stops that hint (fail-open, never a wrong hint) — update both.
const verifyGateTimedOutLead = "verify gate timed out"

// verifyTimeoutTrailer renders the explicit end-of-output marker
// runVerifyCommittedTree / runVerifyGate append to a timed-out verify's
// captured output, so the stage failure_reason, the trace event and the
// gate-evidence output_tail END with a runner-authored statement instead of
// silently mid-line (run 6db228d0 ended at `>> > ./runner` with no
// explanation and burned four fix iterations).
//
// It states what is KNOWN and no more: the gate was killed by the runner's
// timeout before reaching a verdict, the output above is INCOMPLETE, any
// FAIL/panic lines above are real, and the ABSENCE of FAIL lines proves
// nothing — it does NOT claim that no test failed.
func verifyTimeoutTrailer(command, form string, timeout, elapsed time.Duration, out string) string {
	return fmt.Sprintf("\n--- fishhawk-runner: verify TERMINATED, no verdict ---\n"+
		"%s: %q (%s form) was killed by the runner after %s: executor.verify.timeout (%s) expired before it finished. "+
		"The runner sent SIGKILL to the command's process group; the output above is INCOMPLETE and ends where the process died (last captured line: %q). "+
		"NO verdict was reached: any FAIL/panic lines above are real, but the ABSENCE of FAIL lines proves nothing — the tests that had not yet run were never judged. "+
		"Recovery: retry the stage in place (category C), or raise executor.verify.timeout if the full verify genuinely needs longer on this host.\n",
		verifyGateTimedOutLead, command, form, elapsed.Round(time.Second), timeout, lastNonEmptyLine(out))
}

// lastNonEmptyLine returns the last line of out that is not blank, with a
// trailing CR stripped; empty when there is none.
func lastNonEmptyLine(out string) string {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimRight(lines[i], "\r")
		if strings.TrimSpace(line) != "" {
			return line
		}
	}
	return ""
}

// verifyTimedOutReason renders the FailureReason / error LEAD for a timed-out
// gate: it begins with verifyGateTimedOutLead and names the command, the
// form, the configured timeout and the attempt count, so an operator (and
// the backend's failuresig catalog) can read the cause without opening the
// output.
func verifyTimedOutReason(command, form string, timeout time.Duration, iteration int) string {
	return fmt.Sprintf("%s: %q (%s form) was killed by the runner when executor.verify.timeout (%s) expired on attempt %d before it reached a verdict; the captured output is incomplete and the change was NOT judged — retry the stage in place or raise executor.verify.timeout",
		verifyGateTimedOutLead, command, form, timeout, iteration)
}

// verifyRunEventTimedOut is verifyRunEvent for a timed-out run: kind
// verify_run, outcome failed, exit_code -1, plus the ADDITIVE keys
// timed_out:true, timeout_seconds and elapsed_seconds so a consumer can tell
// a killed gate from a red one without parsing the trailer.
func verifyRunEventTimedOut(command, headSHA, treeSHA, output string, timeout, elapsed time.Duration) agent.Event {
	return agent.Event{
		Kind: "verify_run",
		Payload: agent.MakePayload(map[string]any{
			"command":         command,
			"head_sha":        headSHA,
			"tree_sha":        treeSHA,
			"exit_code":       -1,
			"output":          output,
			"outcome":         "failed",
			"timed_out":       true,
			"timeout_seconds": int(timeout / time.Second),
			"elapsed_seconds": int(elapsed / time.Second),
		}),
	}
}

// logVerifyGateTimedOut writes the verify_gate_timed_out JSONL line every
// gate site emits when its LAST verify execution timed out, and returns the
// matching trace event for the site to append to its event slice.
func logVerifyGateTimedOut(logSink io.Writer, cfg config, iteration int, form string, timeout time.Duration) agent.Event {
	_, _ = fmt.Fprintf(logSink,
		`{"event":"verify_gate_timed_out","run_id":%q,"stage_id":%q,"iteration":%d,"form":%q,"timeout_seconds":%d,"command":%q}`+"\n",
		cfg.runID, cfg.stageID, iteration, form, int(timeout/time.Second), cfg.verifyCmd)
	return agent.Event{
		Kind: "verify_gate_timed_out",
		Payload: agent.MakePayload(map[string]any{
			"iteration":       iteration,
			"form":            form,
			"timeout_seconds": int(timeout / time.Second),
			"command":         cfg.verifyCmd,
		}),
	}
}

// workingTreeGateFailureCategory maps the working-tree gate's (runVerifyGate,
// the plan / --no-pr paths) error to its failure category at the run() call
// site: C when the runner's own deadline killed the command (no verdict —
// retryable in place), A for every ordinary non-zero exit (#441).
func workingTreeGateFailureCategory(err error) string {
	if errors.Is(err, errVerifyGateTimedOut) {
		return "C"
	}
	return "A"
}
