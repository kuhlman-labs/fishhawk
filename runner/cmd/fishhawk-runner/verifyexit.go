package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// The verify exit-code contract (#4022, docs/spec/workflow-v2.md § "Verify
// exit-code contract"): a project's verify command classifies its own
// non-verdict outcomes by EXIT CODE, so the runner never scrapes tool text to
// tell "the tree is red" from "the gate could not judge the tree".
//
//	0       passed — the ONLY exit that passes;
//	75      sysexits(3) EX_TEMPFAIL: an infrastructure / temporary failure the
//	        tree did not cause. Re-run in place (verifyTempFailMaxReruns), never
//	        handed to the fix agent, then category C verify_infra_tempfail;
//	73      sysexits(3) EX_CANTCREAT: the shared verify lock could not be
//	        acquired. Bounded in-place re-run (verifyLockContentionMaxReruns),
//	        then category C verify_lock_contended;
//	other   the change failed.
//
// This repository's scripts/test exits VERIFY_EXIT_LOCK_REFUSED=73 on every
// verify-lock refusal and declares VERIFY_EXIT_TEMPFAIL=75;
// TestVerifyExitCodesMatchScriptsTest pins both against the script.
//
// RESIDUAL: the exit code is chosen by the command the diff can edit, exactly
// as the deleted #3948 text classifier's input was. A diff that makes its own
// red verify exit 73 or 75 steers it to category C instead of the fix agent.
// It is never a verified-tree bypass: every push still requires an explicit
// "passed" outcome, which neither code produces.
const (
	verifyExitTempFail    = 75
	verifyExitLockRefused = 73
)

// verifyExitDisposition maps the verify command's exit code onto the gate
// disposition. Only gateExecuted is remapped: a refused, unavailable,
// checkout-refused or timed-out gate keeps its own disposition whatever code
// the process reported (a container runtime's own failure code, a killed
// process's -1).
func verifyExitDisposition(exitCode int, disp gateDisposition) gateDisposition {
	if disp != gateExecuted {
		return disp
	}
	switch exitCode {
	case verifyExitTempFail:
		return gateTempFail
	case verifyExitLockRefused:
		return gateLockRefused
	}
	return disp
}

// verifyTempFailMaxReruns bounds the in-place re-runs ONE gate site spends on
// an exit-75 verify: at most 1 + verifyTempFailMaxReruns = 3 executions per
// site before the stage ends category C. No sleep between re-runs: the verify
// command owns any backoff its infrastructure needs.
const verifyTempFailMaxReruns = 2

// verifyTempFail is one gate site's temporary-failure re-run count.
type verifyTempFail struct {
	reruns int
}

// admit decides whether the site may re-run an exit-75 verify once more,
// counting the re-run on admission.
func (t *verifyTempFail) admit() bool {
	if t.reruns >= verifyTempFailMaxReruns {
		return false
	}
	t.reruns++
	return true
}

// errVerifyTempFail is wrapped (alongside gitops.ErrVerifyInfraFailure, which
// resolves the stage to category C) when the verify command kept exiting 75.
var errVerifyTempFail = errors.New("verify infrastructure temporary failure")

// errVerifyGateSkipped is wrapped (alongside gitops.ErrVerifyInfraFailure)
// when the gate never ran the verify command (outcome "skipped": its
// throwaway checkout could not be materialized) across its one re-run. An
// unexecuted gate never passes (#3983).
var errVerifyGateSkipped = errors.New("verify gate skipped")

// verifyTempFailLead / verifyGateSkippedLead lead the matching FailureReason /
// error text so the cause is named without reading the output.
const (
	verifyTempFailLead    = "verify_infra_tempfail"
	verifyGateSkippedLead = "verify_gate_skipped"
)

// verifyTempFailReason renders the FailureReason / error lead for a verify that
// kept exiting 75.
func verifyTempFailReason(verifyCmd string, attempts int) string {
	return fmt.Sprintf("%s: %q exited %d (EX_TEMPFAIL: an infrastructure / temporary failure) on each of %d attempt(s); the change was NOT judged. Retry the stage in place once the infrastructure recovers.",
		verifyTempFailLead, verifyCmd, verifyExitTempFail, attempts)
}

// verifyGateSkippedReason renders the FailureReason / error lead for a gate
// that never ran the verify command; detail is the runner-authored cause.
func verifyGateSkippedReason(verifyCmd, detail string) string {
	return fmt.Sprintf("%s: the committed-tree gate never ran %q (outcome skipped: %s); an unexecuted gate is not a pass. Retry the stage in place.",
		verifyGateSkippedLead, verifyCmd, detail)
}

// logVerifyTempFailRerun writes the verify_infra_tempfail_retry JSONL line a
// gate site emits before each in-place re-run of an exit-75 verify, and
// returns the matching trace event.
func logVerifyTempFailRerun(logSink io.Writer, cfg config, iteration, rerun int) agent.Event {
	_, _ = fmt.Fprintf(logSink,
		`{"event":"verify_infra_tempfail_retry","run_id":%q,"stage_id":%q,"iteration":%d,"rerun":%d,"max_reruns":%d}`+"\n",
		cfg.runID, cfg.stageID, iteration, rerun, verifyTempFailMaxReruns)
	return agent.Event{
		Kind: "verify_infra_tempfail_retry",
		Payload: agent.MakePayload(map[string]any{
			"iteration":  iteration,
			"rerun":      rerun,
			"max_reruns": verifyTempFailMaxReruns,
		}),
	}
}

// logVerifyTempFailed writes the terminal verify_infra_tempfail JSONL line a
// gate site emits when it gives up on an exit-75 verify, and returns the
// matching trace event.
func logVerifyTempFailed(logSink io.Writer, cfg config, iteration, attempts int) agent.Event {
	_, _ = fmt.Fprintf(logSink,
		`{"event":"verify_infra_tempfail","run_id":%q,"stage_id":%q,"iteration":%d,"attempts":%d,"command":%q}`+"\n",
		cfg.runID, cfg.stageID, iteration, attempts, cfg.verifyCmd)
	return agent.Event{
		Kind: "verify_infra_tempfail",
		Payload: agent.MakePayload(map[string]any{
			"iteration": iteration,
			"attempts":  attempts,
			"command":   cfg.verifyCmd,
		}),
	}
}

// logVerifyGateSkippedRerun writes the verify_gate_skipped_retry JSONL line a
// gate site emits before its one re-run of a gate that never ran the command,
// and returns the matching trace event.
func logVerifyGateSkippedRerun(logSink io.Writer, cfg config, iteration int, detail string) agent.Event {
	_, _ = fmt.Fprintf(logSink,
		`{"event":"verify_gate_skipped_retry","run_id":%q,"stage_id":%q,"iteration":%d,"detail":%q}`+"\n",
		cfg.runID, cfg.stageID, iteration, detail)
	return agent.Event{
		Kind: "verify_gate_skipped_retry",
		Payload: agent.MakePayload(map[string]any{
			"iteration": iteration,
			"detail":    detail,
		}),
	}
}

// skippedVerifyRunOutput returns the runner-authored reason a "skipped"
// verify_run event carries as its output ("" when undecodable).
func skippedVerifyRunOutput(ev agent.Event) string {
	var p struct {
		Output string `json:"output"`
	}
	_ = json.Unmarshal(ev.Payload, &p)
	return p.Output
}
