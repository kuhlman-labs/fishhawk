package main

import (
	"fmt"
	"io"
)

// #5410: rebuild a non-composed first-pass PR body from the run-branch commit
// message.
//
// #3012 made a skipped PR-description handoff LOUD — the placeholder body carries
// a marker and the runner emits pr_body_not_composed — but the shipped body still
// had no summary at all. When the agent skipped the PR handoff yet DID write its
// initial-implement commit-message sidecar (#1686), the runner already holds a
// real summary of the change: the commit message the run branch carries. These
// helpers turn that message into the PR body instead of shipping the bare
// placeholder.
//
// Composition stays PURE (the #3012 rule): prBodyRebuiltMarker and
// rebuildPRTextFromCommitMessage return text and emit nothing; the ordinary call
// site in openPRAndShipArtifact emits pr_body_rebuilt (and, on both branches,
// pr_body_not_composed) once it has decided what ships.

// prBodyRebuiltMarker is prBodyNotComposedMarker's variant for a body the runner
// rebuilt from the commit message. The FIRST line is byte-identical, so a grep or
// a reader keyed on "was not composed by the implement agent" still matches; the
// run/stage/reason and handoff lines are unchanged too. It then names the source
// and states what the RUNNER did not add — deliberately asserting nothing about
// the copied commit text itself, which may or may not carry a closing reference
// (approval condition U1).
func prBodyRebuiltMarker(cfg config, reason prBodyReason, checkedPath, sidecarPath string) string {
	return fmt.Sprintf(
		"> **This pull-request body was not composed by the implement agent.**\n"+
			"> Run `%s`, stage `%s`. Reason: `%s`.\n"+
			"> Handoff checked: `%s`.\n"+
			"> Rebuilt by the runner from the run-branch commit message (commit-message sidecar `%s`): the summary below is that commit message.\n"+
			"> What this costs: the runner added no test plan and no `Closes #N` reference, "+
			"so unless the summary below carries a closing reference, merging will not auto-close the trigger issue — "+
			"check it and close the issue by hand if needed.\n\n",
		cfg.runID, cfg.stageID, reason, checkedPath, sidecarPath,
	)
}

// rebuildPRTextFromCommitMessage composes the PR title + body from the
// initial-implement commit message when the PR-description handoff fell back
// (reason != none, checked by the caller). It returns (title, "", false) — the
// caller keeps today's placeholder + marker — unless the commit-message SIDECAR
// supplied the message: on the title + body fallback the commit message IS the
// placeholder, so rebuilding from it would add nothing.
//
// The rebuilt body is the rebuilt marker, a `## Summary` section carrying the
// commit subject and (when non-empty) body, and exactly one attribution footer.
// The title is replaced with the commit subject only when it is the runner's
// placeholder; a title-only handoff (body_absent) is agent-authored, so the
// agent's title is kept.
func rebuildPRTextFromCommitMessage(cfg config, branch, title string, agentAuthored bool, reason prBodyReason, handoffPath string, cm commitMessageSource) (newTitle, newBody string, rebuilt bool) {
	if !cm.fromSidecar {
		return title, "", false
	}
	summary := cm.subject
	if cm.body != "" {
		summary += "\n\n" + cm.body
	}
	newBody = prBodyRebuiltMarker(cfg, reason, handoffPath, implementCommitMessagePath(cfg.runID, cfg.stageID)) +
		"## Summary\n\n" + summary + prAttributionFooter(cfg, branch)
	newTitle = title
	if !agentAuthored {
		newTitle = cm.subject
	}
	return newTitle, newBody, true
}

// emitPRBodyRebuilt records that the runner COMPOSED a rebuilt PR body from the
// commit message. It does not mean a PR shipped with it: a decomposed child, a
// park or a resume can reach this point without opening a PR, exactly as
// pr_body_not_composed already can.
func emitPRBodyRebuilt(logSink io.Writer, cfg config, reason prBodyReason, titleRebuilt bool) {
	_, _ = fmt.Fprintf(logSink,
		`{"event":"pr_body_rebuilt","run_id":%q,"stage_id":%q,"reason":%q,"source":"commit_message","title_rebuilt":%t}`+"\n",
		cfg.runID, cfg.stageID, string(reason), titleRebuilt)
}
