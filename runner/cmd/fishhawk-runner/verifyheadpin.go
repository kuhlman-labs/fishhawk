package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
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
