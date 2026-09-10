package mcpserver

import (
	"fmt"
	"regexp"
)

/*
 * Acceptance preview bring-up surfacing (E68.43 / #3321).
 *
 * The operator's manual ritual — notice the needs_target park, run
 * `scripts/dev preview <head>` by hand, re-dispatch — lived in operator memory
 * and nowhere in the loop's own surfaces. This file holds the shared, PURE
 * renderers that put it on two surfaces (the needs_target refusal and the
 * get_run_status / run_stage next_actions block) plus the resolver that decides
 * whether the SPAWNED RUNNER is the one that provisions.
 *
 * The load-bearing split (#3321 operator constraint item 1): the GATE DECISION
 * and the DISPLAY RENDERING are separate concerns and must stay separate.
 *   - resolveAcceptancePreviewCmd is the GATE decision. Its empty third arm
 *     means "do not take the preview-cmd proceed branch and do not inject
 *     anything into the spawn env". Defaulting it would make every needs_target
 *     dispatch proceed while provisioning nothing.
 *   - acceptancePreviewDisplayCommand owns the DISPLAY default, so the most
 *     common configuration (no FISHHAWK_ACCEPTANCE_PREVIEW_CMD, auto_preview
 *     false) still renders a CONCRETE command instead of an empty string.
 */

// acceptancePreviewDefaultCmd is the built-in provision command: fishhawk's own
// dogfood default, the command the existing needs_target Remediation already
// names, and the one the operator types by hand today. A non-fishhawk stack
// overrides it by setting FISHHAWK_ACCEPTANCE_PREVIEW_CMD, which wins
// everywhere (docs/acceptance-preview.md).
const acceptancePreviewDefaultCmd = "scripts/dev preview"

// acceptancePreviewActionName is the next_actions / refusal action name for the
// preview bring-up. It is a named RITUAL STEP, not an MCP tool: the bring-up
// happens on the operator's host outside the MCP surface (ADR-038 keeps host
// mutation off that surface).
const acceptancePreviewActionName = "run_preview"

// acceptancePreviewSHAShape is the strict plausibility check applied to a head
// SHA before it is interpolated into a RENDERED shell command. The SHA reaches
// this renderer from agent-reported audit payloads and from the backend's
// admission response, so it is untrusted input for display purposes: anything
// that is not 7..64 hex characters is DROPPED and the command renders alone.
// This degrades the SHA, never the command.
var acceptancePreviewSHAShape = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// acceptancePreviewCommandOrDefault is the ONE place the built-in default
// lives: it returns cmd when non-empty, else acceptancePreviewDefaultCmd.
func acceptancePreviewCommandOrDefault(cmd string) string {
	if cmd != "" {
		return cmd
	}
	return acceptancePreviewDefaultCmd
}

// acceptancePreviewDisplayCommand renders the concrete command an operator can
// paste. It ALWAYS returns a non-empty command — a zero-valued cmd falls back
// to acceptancePreviewDefaultCmd — and appends the head SHA only when it passes
// acceptancePreviewSHAShape.
func acceptancePreviewDisplayCommand(cmd, sha string) string {
	base := acceptancePreviewCommandOrDefault(cmd)
	if !acceptancePreviewSHAShape.MatchString(sha) {
		return base
	}
	return base + " " + sha
}

// resolveAcceptancePreviewCmd is the GATE DECISION resolver — NOT the display
// renderer. It answers two coupled questions with one value: does the verb take
// the preview-cmd proceed branch, and does dispatch inject
// FISHHAWK_ACCEPTANCE_PREVIEW_CMD into the spawned runner's environment?
//
//   - operator-set FISHHAWK_ACCEPTANCE_PREVIEW_CMD -> (that value, "env"). The
//     operator's value ALWAYS wins, even under auto_preview.
//   - else autoPreview -> (acceptancePreviewDefaultCmd, "auto_preview"): the
//     caller injects the default into the spawn env and the runner provisions.
//   - else ("", ""): do NOT take the proceed branch and inject NOTHING.
//
// That empty third arm is deliberate and is NOT a display concern. DISPLAY
// rendering goes through acceptancePreviewDisplayCommand, which defaults — the
// two are separate so a command-less action can never be rendered while the
// gate's proceed decision stays byte-identical to its pre-#3321 behaviour
// (#3321 operator constraint item 1).
func resolveAcceptancePreviewCmd(getenv func(string) string, autoPreview bool) (cmd, source string) {
	if getenv != nil {
		if v := getenv(acceptancePreviewCmdEnv); v != "" {
			return v, "env"
		}
	}
	if autoPreview {
		return acceptancePreviewDefaultCmd, "auto_preview"
	}
	return "", ""
}

// acceptancePreviewAction builds the run_preview suggested action. Params
// ALWAYS carry a non-empty "command" (acceptancePreviewDisplayCommand
// defaults); sha, working_dir and target_host are omitted when empty.
func acceptancePreviewAction(cmd, sha, workingDir, targetHost string) SuggestedAction {
	params := map[string]string{"command": acceptancePreviewDisplayCommand(cmd, sha)}
	if acceptancePreviewSHAShape.MatchString(sha) {
		params["expected_head_sha"] = sha
	}
	if workingDir != "" {
		params["working_dir"] = workingDir
	}
	if targetHost != "" {
		params["target_host"] = targetHost
	}
	return SuggestedAction{
		Action:       acceptancePreviewActionName,
		Params:       params,
		Precondition: "the run is on the LOCAL runner kind and the acceptance stage declares an egress target host that must serve the merge candidate; the command runs on the dispatch host, in the run's working_dir",
		Consumes:     consumesNone,
		Reason: fmt.Sprintf(
			"the acceptance stage validates against a live target serving the merge candidate; bring it up with this command, then re-dispatch — or skip the manual step by calling fishhawk_dispatch_stage with auto_preview=true, which hands the same command to the spawned runner. Setting %s overrides the built-in default.",
			acceptancePreviewCmdEnv),
	}
}

// acceptancePreviewLedgerCategories are the audit categories whose entries
// carry a head_sha for THIS run's own commits. It mirrors the backend's
// lineageLedgerCategories (backend/internal/server/lineage.go) — the same set
// resolveAcceptanceExpectedHeadSHA advertises — so the rendered command names
// the head the admission would.
var acceptancePreviewLedgerCategories = map[string]bool{
	"pull_request_opened": true,
	"child_pushed":        true,
	"fixup_pushed":        true,
}

// latestReportedHeadSHA returns the head_sha on the newest reported-head ledger
// entry in the recent slice (time-descending, item 0 newest — the
// latestAcceptanceVerdict idiom). Returns "" on an empty window, a malformed
// payload, or no matching entry; the SHA is then omitted from the rendered
// command while the COMMAND itself stays concrete.
func latestReportedHeadSHA(recent []AuditEntry) string {
	for _, e := range recent {
		if !acceptancePreviewLedgerCategories[e.Category] {
			continue
		}
		if sha := acceptancePayloadString(e.Payload, "head_sha"); sha != "" {
			return sha
		}
	}
	return ""
}

// foldAcceptancePreviewAdvisory inserts the run_preview action IMMEDIATELY
// BEFORE the first suggested acceptance dispatch, because the preview is that
// dispatch's precondition. DISPLAY-ONLY and ADDITIVE, in the
// foldAcceptanceRedispatchAdvisory / foldLiveValidationAdvisory idiom: nil-safe,
// no round-trips, and it never gates a run.
//
// Wired at BOTH nextActionsFor call sites (tools.go's getRunStatus and
// run_stage.go's post-stage snapshot) so the two surfaces cannot diverge.
//
// No-op arms — leaving na.Actions deep-equal to its unfolded value — are
// exactly two: a nil run/na, and an actions list carrying NO acceptance
// dispatch. An EMPTY cmd is NOT a no-op arm: the renderer defaults it, so the
// action still names a concrete command (#3321 constraint item 1).
func foldAcceptancePreviewAdvisory(run *Run, cmd, sha string, na *NextActions) {
	if run == nil || na == nil {
		return
	}
	idx := -1
	for i, a := range na.Actions {
		if a.Action != "fishhawk_dispatch_stage" && a.Action != "fishhawk_run_stage" {
			continue
		}
		if a.Params["stage"] == "acceptance" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	action := acceptancePreviewAction(cmd, sha, run.WorkingDir, "")
	folded := make([]SuggestedAction, 0, len(na.Actions)+1)
	folded = append(folded, na.Actions[:idx]...)
	folded = append(folded, action)
	folded = append(folded, na.Actions[idx:]...)
	na.Actions = folded
}
