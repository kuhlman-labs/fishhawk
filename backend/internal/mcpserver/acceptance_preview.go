package mcpserver

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
)

/*
 * Acceptance preview bring-up surfacing (E68.43 / #3321).
 *
 * The operator's manual ritual — notice the needs_target park, run
 * `scripts/dev preview <head>` by hand, re-dispatch — lived in operator memory
 * and nowhere in the loop's own surfaces. This file holds the shared, PURE
 * renderers that put it on two surfaces (the needs_target refusal and the
 * get_run_status / run_stage next_actions block) plus the two resolvers that
 * decide whether the SPAWNED RUNNER is the one that provisions AND what it runs
 * to tear the provisioned instance down afterwards (#3394).
 *
 * The load-bearing split (#3321 operator constraint item 1): the GATE DECISION
 * and the DISPLAY RENDERING are separate concerns and must stay separate.
 *   - resolveAcceptancePreviewCmd is the GATE decision. Its empty third arm
 *     means "do not take the preview-cmd proceed branch and do not inject
 *     anything into the spawn env". Defaulting it would make every needs_target
 *     dispatch proceed while provisioning nothing.
 *   - resolveAcceptancePreviewTeardownCmd is the GATE decision's other half
 *     (#3394): it keys the built-in `scripts/dev preview-down` on the PROVISION
 *     SOURCE, not on the raw auto_preview flag, so the default teardown is only
 *     ever paired with the default provision it is the counterpart of.
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

// acceptancePreviewTeardownCmdEnv is the runner's teardown hook
// (FISHHAWK_ACCEPTANCE_PREVIEW_TEARDOWN_CMD). It byte-mirrors the runner's
// previewTeardownCmdEnv (runner/cmd/fishhawk-runner/previewprobe.go): the two
// modules cannot import each other, so each package's tests pin the literal —
// the same diff-only substitute acceptancePreviewCmdEnv already relies on.
const acceptancePreviewTeardownCmdEnv = "FISHHAWK_ACCEPTANCE_PREVIEW_TEARDOWN_CMD"

// acceptancePreviewDefaultTeardownCmd is the built-in counterpart of
// acceptancePreviewDefaultCmd: what tears down the instance `scripts/dev
// preview` stood up. It is injected ONLY alongside the default provision
// command (#3394) — an operator who supplied their own provision hook keeps
// ownership of its teardown, because pairing a foreign provision with
// `scripts/dev preview-down` would tear down something fishhawk did not
// provision.
const acceptancePreviewDefaultTeardownCmd = "scripts/dev preview-down"

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

// resolveAcceptancePreviewTeardownCmd is the teardown half of the GATE
// decision (#3394). It answers: does dispatch inject
// FISHHAWK_ACCEPTANCE_PREVIEW_TEARDOWN_CMD into the spawned runner's env, and
// with what?
//
//   - operator-set FISHHAWK_ACCEPTANCE_PREVIEW_TEARDOWN_CMD -> (that value,
//     "env"). The operator's value ALWAYS wins, whatever the provision source.
//   - else provisionSource == "auto_preview" ->
//     (acceptancePreviewDefaultTeardownCmd, "auto_preview"): the provision
//     resolved to the built-in default, so its built-in counterpart is injected
//     next to it.
//   - else ("", ""): inject NOTHING.
//
// The default is keyed on the PROVISION SOURCE, deliberately not on the raw
// auto_preview flag: an operator-set FISHHAWK_ACCEPTANCE_PREVIEW_CMD resolves
// with source "env" and gets NO default teardown even under auto_preview:true,
// because `scripts/dev preview-down` only knows how to take down what
// `scripts/dev preview` stood up. That operator keeps today's behaviour (the
// runner warns acceptance_preview_teardown_missing when no teardown is set).
func resolveAcceptancePreviewTeardownCmd(getenv func(string) string, provisionSource string) (cmd, source string) {
	if getenv != nil {
		if v := getenv(acceptancePreviewTeardownCmdEnv); v != "" {
			return v, "env"
		}
	}
	if provisionSource == "auto_preview" {
		return acceptancePreviewDefaultTeardownCmd, "auto_preview"
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
// carry a head_sha for THIS run's own commits. It is BUILT FROM
// auditcomplete.ReportedHeadLedgerCategories, the same list the backend's
// lineageLedgerCategories (backend/internal/server/lineage.go) aliases, so the
// mirror cannot drift from the set acceptanceHeadAsOf seeds from (#4874).
var acceptancePreviewLedgerCategories = func() map[string]bool {
	m := make(map[string]bool, len(auditcomplete.ReportedHeadLedgerCategories))
	for _, c := range auditcomplete.ReportedHeadLedgerCategories {
		m[c] = true
	}
	return m
}()

// The categories the mirror reads beside the shared ledger list. The
// integration row is a SEED source read next to that list, never added to it,
// exactly as the server's acceptanceHeadSeed reads it (#4874).
const (
	acceptancePreviewIntegrationCategory = "integration_commit_recorded"
	acceptancePreviewBranchRebased       = "branch_rebased"
	acceptancePreviewBranchReset         = "branch_reset"
)

// latestReportedHeadSHA mirrors the server's acceptance head resolution
// (acceptanceHeadAsOf, unbounded) over the recent audit slice, for DISPLAY
// only: the admission's expected_head_sha stays the authority (#4874).
//
// Response order carries NO meaning. The seed is the highest-SEQUENCE ledger
// row with a non-empty head_sha, or the highest-sequence
// integration_commit_recorded merge_sha when that row is newer by chain
// sequence (a reported row wins only an equal-sequence tie). Inside one set
// an equal sequence keeps the row delivered first, so rows carrying no
// sequence keep the old first-delivered answer. The rows above the seed's
// sequence are then walked in sequence order applying the server's
// walkRecordedBaseMerges rules: a CLEAN branch_rebased row whose prior head is
// the current head advances to its merge commit, and a branch_reset to a head
// on the walked chain moves there, while a reset off the chain returns the
// seed. Returns "" on an empty window, a malformed payload, or no seed in the
// window (a seed aged out while a rebase row is inside degrades to ""); the
// SHA is then omitted from the rendered command while the COMMAND itself
// stays concrete.
func latestReportedHeadSHA(recent []AuditEntry) string {
	seed, sha := newestPreviewSeed(recent, func(c string) bool { return acceptancePreviewLedgerCategories[c] }, "head_sha")
	fan, fanSHA := newestPreviewSeed(recent, func(c string) bool { return c == acceptancePreviewIntegrationCategory }, "merge_sha")
	if fan != nil && (seed == nil || fan.Sequence > seed.Sequence) {
		seed, sha = fan, fanSHA
	}
	if seed == nil {
		return ""
	}
	var newer []AuditEntry
	for _, e := range recent {
		if e.Sequence > seed.Sequence {
			newer = append(newer, e)
		}
	}
	sort.SliceStable(newer, func(i, j int) bool { return newer[i].Sequence < newer[j].Sequence })
	return walkPreviewBaseMerges(newer, sha)
}

// newestPreviewSeed selects the highest-sequence row of a matching category
// whose field decodes to a non-empty string. An equal sequence keeps the row
// delivered first. No timestamp is read.
func newestPreviewSeed(recent []AuditEntry, match func(string) bool, field string) (*AuditEntry, string) {
	var seed *AuditEntry
	var seedSHA string
	for i := range recent {
		e := &recent[i]
		if !match(e.Category) {
			continue
		}
		v := acceptancePayloadString(e.Payload, field)
		if v == "" {
			continue
		}
		if seed == nil || e.Sequence > seed.Sequence {
			seed = e
			seedSHA = v
		}
	}
	return seed, seedSHA
}

// previewBranchRebased is the decoded slice of a branch_rebased payload the
// mirror's clean-advance check reads; it mirrors the server's
// branchRebasedAdvance (CleanVerified a pointer, so a legacy row with no key
// is not clean).
type previewBranchRebased struct {
	PriorHeadSHA    string `json:"prior_head_sha"`
	NewHeadSHA      string `json:"new_head_sha"`
	MergeCommitSHA  string `json:"merge_commit_sha"`
	AlreadyUpToDate bool   `json:"already_up_to_date"`
	CleanVerified   *bool  `json:"clean_verified"`
}

// decodePreviewPayload re-decodes a wire payload into a typed struct so the
// mirror's decode failures match the server's json.Unmarshal ones.
func decodePreviewPayload(payload any, into any) bool {
	if payload == nil {
		return false
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, into) == nil
}

// walkPreviewBaseMerges applies the server's walkRecordedBaseMerges rules to
// rows already filtered above the seed and ordered by sequence.
func walkPreviewBaseMerges(ordered []AuditEntry, seed string) string {
	head := seed
	onChain := map[string]bool{seed: true}
	for _, e := range ordered {
		switch e.Category {
		case acceptancePreviewBranchRebased:
			var p previewBranchRebased
			if !decodePreviewPayload(e.Payload, &p) {
				continue
			}
			clean := !p.AlreadyUpToDate && p.MergeCommitSHA != "" && p.NewHeadSHA == p.MergeCommitSHA &&
				p.CleanVerified != nil && *p.CleanVerified
			if clean && p.PriorHeadSHA == head {
				head = p.MergeCommitSHA
				onChain[head] = true
			}
		case acceptancePreviewBranchReset:
			var p struct {
				ResetToSHA string `json:"reset_to_sha"`
			}
			if !decodePreviewPayload(e.Payload, &p) || !onChain[p.ResetToSHA] {
				return seed
			}
			head = p.ResetToSHA
		}
	}
	return head
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
