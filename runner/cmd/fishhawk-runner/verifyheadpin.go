package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kuhlman-labs/fishhawk/redaction"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// The verify-head pin (#4190).
//
// A committed-tree verify gate that turns a successful agent pass into a
// failure used to leave the agent's complete output reachable from nothing: the
// verify-fix loop's throwaway `fishhawk verify wip` commit is reset away, and a
// retry re-ran the whole agent. The helpers below pin the head of the LAST
// verify_run at refs/fishhawk/checkpoints/<run>/<stage>-verify, a sibling of the
// #4079 held-commit and stash pins (same LOCAL-ONLY namespace, shared by every
// worktree of the repository), and render the clause that names it in the
// failure reason. The reverify resume (reverifyresume.go) re-verifies that
// pinned commit instead of re-running the agent.

// checkpointVerifySuffix distinguishes the verify-head pin from the held-commit
// and stash pins: a leaf SUFFIX (like checkpointStashSuffix), so the three refs
// are siblings and none is a directory/file conflict for another.
const checkpointVerifySuffix = "-verify"

// checkpointVerifyRef is the ref that pins the head of a stage's last
// verify_run after the committed-tree gate failed it.
func checkpointVerifyRef(runID, stageID string) string {
	return checkpointRef(runID, stageID) + checkpointVerifySuffix
}

// lastVerifyRunHead returns the head_sha and tree_sha of the LAST verify_run
// event that carries a non-empty head_sha, or two empty strings when none does.
// The last one wins because the verify-fix loop's final iteration (the full-form
// re-verify, or the iteration whose failure ended the loop) is the one that
// judged the tree the stage failed on. Events with an empty head — the
// working-tree gate and the pre-commit skip paths — never commit anything, so
// they are skipped rather than allowed to blank out an earlier real head.
func lastVerifyRunHead(events []agent.Event) (headSHA, treeSHA string) {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "verify_run" {
			continue
		}
		var p struct {
			HeadSHA string `json:"head_sha"`
			TreeSHA string `json:"tree_sha"`
		}
		if err := json.Unmarshal(events[i].Payload, &p); err != nil || p.HeadSHA == "" {
			continue
		}
		return p.HeadSHA, p.TreeSHA
	}
	return "", ""
}

// firstVerifyRunHead is lastVerifyRunHead's mirror: the head_sha of the FIRST
// verify_run event carrying one — the committed gates' commit of the agent
// pass's own tree, before any fix iteration or absorb rewrote it.
func firstVerifyRunHead(events []agent.Event) string {
	for _, ev := range events {
		if ev.Kind != "verify_run" {
			continue
		}
		var p struct {
			HeadSHA string `json:"head_sha"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err == nil && p.HeadSHA != "" {
			return p.HeadSHA
		}
	}
	return ""
}

// verifyHeadNotAgentPass returns why the last verify_run head must NOT be
// pinned, or "" when it may be. Only a head whose change is exactly the
// git_diff already shipped in the trace may be pinned and armed as a reverify
// checkpoint: that git_diff is emitted from the agent pass's tree and is
// re-emitted only on a PASSING stage, and the reverify consume ships no trace,
// so any edit made after the agent pass would reach the PR without backend
// policy evaluation or the trace-time implement review. Two ways the head can
// diverge, both refused:
//
//   - scopeFolded: a scope amendment was folded (at agent exit or mid-loop),
//     so the committed tree carries paths the shipped git_diff omits;
//   - the last head's tree differs from the FIRST committed verify's tree: a
//     fix iteration (or the auto-format absorb) changed what is committed.
//
// A no-op fix iteration (#4036's flake re-run) leaves the tree byte-identical,
// so its head is still the agent pass's change and stays pinnable. A tree that
// cannot be resolved FAILS CLOSED (not pinned). No verify_run head at all
// returns "" so pinVerifyHead logs its own no_verify_run_head skip.
func verifyHeadNotAgentPass(ctx context.Context, repoDir string, events []agent.Event, scopeFolded bool) string {
	if scopeFolded {
		return "scope_amendment_folded"
	}
	first := firstVerifyRunHead(events)
	last, _ := lastVerifyRunHead(events)
	if first == "" || last == "" || first == last {
		return ""
	}
	firstTree, ferr := gitRevParseTreeOf(ctx, repoDir, first)
	lastTree, lerr := gitRevParseTreeOf(ctx, repoDir, last)
	if ferr != nil || lerr != nil || firstTree == "" || lastTree == "" {
		return "verify_tree_unresolvable"
	}
	if firstTree != lastTree {
		return "tree_changed_after_agent_pass"
	}
	return ""
}

// verifyHeadPin is what pinVerifyHead pinned: the ref, the verify_run head it
// points at, that head's parent (the base a reverify checkpoint records) and the
// tree the verify_run judged.
type verifyHeadPin struct {
	ref     string
	headSHA string
	baseSHA string
	treeSHA string
}

// pinVerifyHead pins the head of the last verify_run in events at
// checkpointVerifyRef and resolves its parent as the base. It is FAIL-OPEN: the
// pin only preserves work and arms a cheaper retry, so a failure never changes
// the stage's category or exit — it logs verify_head_pin_failed and returns
// ok=false. No verify_run head (the gate never committed a tree) logs
// verify_head_pin_skipped and returns ok=false.
//
// The base is resolved BEFORE the pin, so a returned ok=true always carries both
// coordinates a reverify checkpoint needs; a head whose parent cannot be read is
// not pinned at all.
func pinVerifyHead(ctx context.Context, cfg config, repoDir string, events []agent.Event, logSink io.Writer) (verifyHeadPin, bool) {
	headSHA, treeSHA := lastVerifyRunHead(events)
	if headSHA == "" {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"verify_head_pin_skipped","run_id":%q,"stage_id":%q,"reason":"no_verify_run_head"}`+"\n",
			cfg.runID, cfg.stageID)
		return verifyHeadPin{}, false
	}
	ref := checkpointVerifyRef(cfg.runID, cfg.stageID)
	failed := func(detail string) (verifyHeadPin, bool) {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"verify_head_pin_failed","run_id":%q,"stage_id":%q,"ref":%q,"head_sha":%q,"detail":%q}`+"\n",
			cfg.runID, cfg.stageID, ref, headSHA, detail)
		return verifyHeadPin{}, false
	}
	baseSHA, err := gitRevParseIn(ctx, repoDir, headSHA+"^{commit}^")
	if err != nil || baseSHA == "" {
		return failed(fmt.Sprintf("resolve parent of %s: %v", headSHA, err))
	}
	if err := pinCheckpointRef(ctx, repoDir, ref, headSHA); err != nil {
		return failed(err.Error())
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"verify_head_pinned","run_id":%q,"stage_id":%q,"ref":%q,"head_sha":%q,"base_sha":%q,"tree_sha":%q}`+"\n",
		cfg.runID, cfg.stageID, ref, headSHA, baseSHA, treeSHA)
	return verifyHeadPin{ref: ref, headSHA: headSHA, baseSHA: baseSHA, treeSHA: treeSHA}, true
}

// verifyHeadFailureSuffix renders the clause appended to a gate failure reason
// naming the pinned head and its ref. It is an APPENDED clause, never a lead,
// so every reason classifier that reads the leading token is unaffected, and
// upload.TruncateReason's head+tail truncation keeps it. Either argument empty
// renders nothing, so an unpinned failure's reason is byte-identical to before.
func verifyHeadFailureSuffix(headSHA, ref string) string {
	if headSHA == "" || ref == "" {
		return ""
	}
	return "; verified head " + headSHA + " pinned at " + ref
}

// releaseVerifyHeadRef deletes only the verify-head pin, for a stage whose
// ordinary push succeeded after an earlier attempt pinned one. Best-effort and
// log-only, exactly like releaseCheckpointRefs (which also releases this ref).
func releaseVerifyHeadRef(ctx context.Context, repoDir, runID, stageID string, logSink io.Writer) {
	releaseRefs(ctx, repoDir, runID, stageID, []string{checkpointVerifyRef(runID, stageID)}, logSink)
}

// The pre-trace verify pin (#5535).
//
// A push-gated implement stage whose committed verify PASSED used to hold its
// verified tree only in the working checkout until the push: when the backend
// then failed the stage at trace time (bundle_unreadable, no_diff_captured,
// verify_infra_tempfail, policy violations on re-evaluation), nothing named
// the verified commit. The helpers below pin it BEFORE the trace upload, at the
// same -verify ref the #4190 failure pin uses, and declare its SHA in the
// signed manifest (verify_pin_head_sha) so the backend's trace-time failure
// reason can name it. The pin is inspection-only: it never arms a reverify
// checkpoint. A successful push releases it (releaseVerifyHeadRef). The
// #4190 committed-gate failure pin and this one are disjoint: the first runs
// only when the gate failed, this one only when it passed.

// Trace 202 stage_state / stage_failure_kind values the runner acts on
// (upload.ShipResult). Both are positive lists: any other value, or an absent
// one from an older backend, keeps the pre-#5535 behaviour.
const (
	traceStageStateFailed  = "failed"
	traceStageStateRunning = "running"
	// traceFailureKindRejected is the backend refusing the stage at trace
	// time. implement_review_rejected (the #877 gating reject) is deliberately
	// NOT acted on: that stage still pushes, and the backend closes the PR.
	traceFailureKindRejected = "trace_rejected"
)

// errImplementNoChangesHeld is a standalone implement stage that produced no
// changes after the backend held it `running` for its push report (W4). The
// stage cannot advance without a /pull-request report, so run()'s push-failure
// arm reports it failed C (pushFailureCategory's default), keeps the pin, and
// exits non-zero. An unheld stage (an older backend) keeps the bare return.
var errImplementNoChangesHeld = errors.New("implement produced no changes after the backend held the stage for its push report")

// checkpointRefNamespace is the prefix every checkpoint pin shares. A reason
// that already names one gets no second clause.
const checkpointRefNamespace = "refs/fishhawk/checkpoints/"

// pinPassingVerifyHead is the pre-trace pin: it pins the last verify_run head
// of a stage whose committed verify passed. Silent when no verify_run carries
// a head (no committed verify ran), and FAIL-OPEN otherwise exactly like
// pinVerifyHead: a failure logs verify_head_pin_failed and the push proceeds.
func pinPassingVerifyHead(ctx context.Context, cfg config, events []agent.Event, logSink io.Writer) (verifyHeadPin, bool) {
	if head, _ := lastVerifyRunHead(events); head == "" {
		return verifyHeadPin{}, false
	}
	repoDir := cfg.workingDir
	if repoDir == "" {
		repoDir = "."
	}
	return pinVerifyHead(ctx, cfg, repoDir, events, logSink)
}

// preTracePinClause is the clause a post-trace failure report appends for the
// pre-trace pin: empty when nothing was pinned, when the report arms a
// checkpoint (its coordinates already name the work), or when the reason
// already names a checkpoint ref.
func preTracePinClause(reason string, pin verifyHeadPin, pinned, armsCheckpoint bool) string {
	if !pinned || armsCheckpoint || strings.Contains(reason, checkpointRefNamespace) {
		return ""
	}
	return verifyHeadFailureSuffix(pin.headSHA, pin.ref)
}

// heldReportReason is the reason a NEW post-trace failure report carries (Y4):
// acceptance-bound values are scrubbed and secrets redacted over the FULL
// text, then the result is bounded, then the (secret-free) pin clause is
// appended, so neither a cut nor redaction can drop or split the clause.
func heldReportReason(reason string, known *redaction.KnownValues, clause string) string {
	s, _ := scrubKnownString(reason, known)
	s, _ = redactString(s)
	return upload.TruncateReason(s, upload.MaxFailureReportReasonBytes-len(clause)) + clause
}

// manifestInfraFlakeMarker is appended to a bounded manifest failure reason
// whose FULL text carried an infra-flake signature (Y1). It leads with the
// literal verify_infra_flake_retry token, which
// backend/internal/delegation.hasInfraFlakeSignature (and the MCP next-actions
// classifier) accept on their own, so the classification survives the
// head+tail cut even when the signature sat in the elided middle.
const manifestInfraFlakeMarker = "; verify_infra_flake_retry: infra-flake signature in the elided verify output"

// backendInfraFlakeSignature mirrors the signature set
// backend/internal/delegation.hasInfraFlakeSignature recognizes: the literal
// marker, the testcontainers start and port-not-found signatures, the
// daemon-unreachable markers and a vanished Go build-cache entry. KEEP IN
// SYNC with that function; the golangci-lint lock signature
// isVerifyInfraFailure also matches is deliberately absent, because the
// backend classifier does not read it.
func backendInfraFlakeSignature(s string) bool {
	return strings.Contains(s, "verify_infra_flake_retry") ||
		isTestcontainersStartFlake(s) ||
		isTestcontainersPortFlake(s) ||
		isDockerDaemonUnavailable(s) ||
		isGoBuildCacheEntryVanished(s)
}

// manifestFailureReasons bounds the manifest's agent_failure_reason (#5535,
// the incident fix). scrubbed is the FULL reason with acceptance-bound values
// already removed. When it is over the bound and carries an infra-flake
// signature, the marker is inserted before any trailing #4190 pin clause, so
// the clause stays last. The redacted variant is redacted over the FULL text
// before its own cut (W6), so no secret straddling a cut leaves a fragment.
// Hits are counted on the full text.
func manifestFailureReasons(scrubbed string) (raw, redacted string, hits []redaction.Hit) {
	max := upload.MaxFailureReportReasonBytes
	if len(scrubbed) > max && backendInfraFlakeSignature(scrubbed) {
		head, clause := scrubbed, ""
		if i := strings.LastIndex(scrubbed, "; verified head "); i >= 0 &&
			strings.Contains(scrubbed[i:], " pinned at "+checkpointRefNamespace) && !strings.ContainsAny(scrubbed[i:], "\n") {
			head, clause = scrubbed[:i], scrubbed[i:]
		}
		scrubbed = head + manifestInfraFlakeMarker + clause
	}
	redactedFull, hits := redactString(scrubbed)
	return upload.TruncateReason(scrubbed, max), upload.TruncateReason(redactedFull, max), hits
}

// reportPackRefusal self-reports a bundle pack refusal (W10) through the
// reap-failure endpoint, constructing the upload client first when the trace
// was to be uploaded and none exists yet. Returns the client so the caller
// keeps it. The detail is scrubbed, redacted and bounded (Y4).
func reportPackRefusal(ctx context.Context, cfg config, client uploadClient, mcpToken, stageAttempt string, logSink io.Writer, known *redaction.KnownValues, reason string, err error) uploadClient {
	if cfg.uploadTrace && client == nil {
		client = newUploadClient(cfg.backendURL)
	}
	reportTerminalRunnerFailure(ctx, cfg, client, mcpToken, stageAttempt, logSink, reason, heldReportReason(err.Error(), known, ""), 1)
	return client
}
