package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// The reverify resume's consume arm (#4190).
//
// run() intercepts a served `reverify` held-commit checkpoint at the
// held-commit early return (before the prompt file is read and the agent
// invoker is wired), runs reverifyPrecheck, and — when the held commit can be
// resumed — hands it to runReverifyResume below. No agent runs on this path.
// A precheck miss never reaches here: run() logs the fallback and the ordinary
// agent pass runs in the same dispatch.
//
// runReverifyResume, in order:
//
//  1. reverifyPrePushGates — the reproducible pre-push gates an ordinary push
//     runs (#980 scope assertion, #1151 scope completeness, #1171 binding
//     assertions). A failure is PERMANENT category B: no checkpoint, no agent
//     fallback, nothing published.
//  2. reverifyHeldCommit — ONE full-form committed-tree verify (plus one
//     infra-signature re-run). A failure is category C: a transient token
//     re-arms the reverify checkpoint, a permanent one records none.
//  3. reissueSigningKeyForTerminalUpload — the verify can outlive the
//     stage-start key's TTL (#1182).
//  4. synthesizeCommitOnParent — a DCO-signed commit with the held tree on the
//     held parent, pinned at checkpointRef so gc cannot prune it.
//  5. openHeldCommitPR with the PUSH kind and the held tree as the verified
//     tree — the existing #3621 publish + PR-open tail. Any later failure there
//     re-arms the cheaper push kind; its success releases every checkpoint ref.
//
// The one ordinary-path gate this path does not reproduce is
// created-out-of-scope (#818), which reads the agent's WORKING tree; see
// reverifyresume.go's header for why the committed-snapshot scope assertion
// covers it.

// reverifySynthesisFailed is the bounded token for a synthesizeCommitOnParent
// failure on a held commit that passed every gate and the verify. Transient:
// the held commit is intact, so the reverify checkpoint is re-armed.
const reverifySynthesisFailed = "reverify_synthesis_failed"

// reverifyFailureReason renders a reverify failure reason: the bounded TOKEN
// first (operators and tests assert its identity), then the detail, then a
// tail of the verify output when one exists — redacted BEFORE it is bounded
// (gate evidence's own order and bounds: the last gateEvidenceTailLines lines
// and gateEvidenceTailBytes bytes), so a cut can never expose half a secret.
func reverifyFailureReason(v reverifyVerdict, output string) string {
	reason := v.token + ": " + v.detail
	if output == "" {
		return reason
	}
	tail, _ := boundEvidenceTail(redactEvidenceText(output))
	if tail == "" {
		return reason
	}
	return reason + "\n" + tail
}

// runReverifyResume is the reverify consume arm. heldBranch is the served run
// branch; servedPRTitle / servedPRBody the served (footer-free) PR text. It
// returns the process exit code.
func runReverifyResume(ctx context.Context, cfg config, h reverifyHeld, heldBranch, servedPRTitle, servedPRBody string, logSink io.Writer, client uploadClient, issued *upload.IssuedKey) int {
	// report sends the category-B/C failure. rearm re-carries the reverify
	// checkpoint (same coordinates and PR text), so a transient failure leaves
	// the NEXT retry resumable; a permanent one records nothing and the next
	// retry is an ordinary agent pass.
	report := func(v reverifyVerdict, output string, rearm bool) int {
		reason := reverifyFailureReason(v, output)
		_, _ = fmt.Fprintf(logSink,
			`{"event":"reverify_resume_failed","run_id":%q,"stage_id":%q,"token":%q,"category":%q,"rearmed":%t,"held_sha":%q}`+"\n",
			cfg.runID, cfg.stageID, v.token, v.category, rearm, h.heldSHA)
		_, _ = fmt.Fprintf(logSink,
			`{"event":"runner_failed","reason":"reverify_resume","detail":%q}`+"\n",
			upload.TruncateReason(reason, upload.MaxFailureReportReasonBytes))
		var cp *pushCheckpoint
		if rearm {
			cp = &pushCheckpoint{
				branch: heldBranch, headSHA: h.heldSHA, baseSHA: h.baseSHA, armed: true,
				resumeKind: resumeKindReverify,
				prTitle:    servedPRTitle, prBody: servedPRBody,
			}
		}
		_ = reportPullRequestFailure(ctx, cfg, logSink, client, issued, v.category, reason, cp)
		return exitFailure
	}

	// (1) Pre-push gate parity: refuse, before any verify runs, a held tree an
	// ordinary push would have refused. Permanent category B, never re-armed.
	if v := reverifyPrePushGates(ctx, h); !v.ok() {
		return report(v, "", false)
	}

	// (2) ONE full-form committed-tree verify over the held commit.
	timeout := cfg.verifyTimeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	rr := reverifyHeldCommit(ctx, h, timeout)
	_, _ = fmt.Fprintf(logSink,
		`{"event":"reverify_held_commit_verified","run_id":%q,"stage_id":%q,"held_sha":%q,"runs":%d,"token":%q}`+"\n",
		cfg.runID, cfg.stageID, h.heldSHA, rr.runs, rr.verdict.token)

	// (3) The verify can outlive the stage-start key's TTL; refresh it before
	// any terminal signed egress (#1182). Best-effort: the helper returns the
	// existing key on failure.
	if client != nil && issued != nil {
		issued = reissueSigningKeyForTerminalUpload(ctx, client, cfg, logSink, issued)
	}
	if !rr.verdict.ok() {
		return report(rr.verdict, rr.output, !rr.verdict.permanent)
	}

	// (4) Replace the bot-identity WIP commit with a DCO-signed commit of the
	// SAME tree on the SAME parent, so `git patch-id --stable` is unchanged.
	heldTree, err := gitRevParseIn(ctx, h.repoDir, h.heldSHA+"^{tree}")
	if err != nil || heldTree == "" {
		return report(reverifyVerdict{token: reverifySynthesisFailed, category: "C",
			detail: fmt.Sprintf("resolve tree of held commit %s: %v", h.heldSHA, err)}, "", true)
	}
	msg := servedPRTitle
	if servedPRBody != "" {
		msg += "\n\n" + servedPRBody
	}
	if msg == "" {
		// The stage placeholder title, built directly: prTitleAndBodyParts
		// would consume the agent PR handoff openHeldCommitPR reads next.
		msg = fmt.Sprintf("chore: fishhawk implement stage %s", shortID(cfg.stageID))
	}
	synth, err := synthesizeCommitOnParent(ctx, h.repoDir, heldTree, h.baseSHA, msg, cfg.commitAuthorName, cfg.commitAuthorEmail)
	if err != nil {
		return report(reverifyVerdict{token: reverifySynthesisFailed, category: "C", detail: err.Error()}, "", true)
	}
	// Pin the synthesized commit: nothing else references it until it is
	// published, and a later push failure re-arms the push kind at it.
	// Fail-open like pinPushFailureCheckpoint: a pin failure never changes the
	// outcome.
	ref := checkpointRef(cfg.runID, cfg.stageID)
	if perr := pinCheckpointRef(ctx, h.repoDir, ref, synth); perr != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"checkpoint_ref_pin_failed","run_id":%q,"stage_id":%q,"ref":%q,"sha":%q,"detail":%q}`+"\n",
			cfg.runID, cfg.stageID, ref, synth, perr.Error())
	} else {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"checkpoint_ref_pinned","run_id":%q,"stage_id":%q,"ref":%q,"sha":%q}`+"\n",
			cfg.runID, cfg.stageID, ref, synth)
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"reverify_commit_synthesized","run_id":%q,"stage_id":%q,"held_sha":%q,"commit_sha":%q,"base_sha":%q,"tree_sha":%q}`+"\n",
		cfg.runID, cfg.stageID, h.heldSHA, synth, h.baseSHA, heldTree)

	// (5) The existing push-resume publish + PR-open tail, with the held tree
	// as the verified tree the tail re-proves against.
	return openHeldCommitPR(ctx, cfg, synth, heldBranch, h.baseSHA, resumeKindPush, heldTree, servedPRTitle, servedPRBody, logSink, client, issued)
}
