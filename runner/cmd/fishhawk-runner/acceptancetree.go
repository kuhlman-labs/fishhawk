package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

/*
 * Merge-candidate tree provisioning for the acceptance stage (#1881).
 *
 * The acceptance agent spawns in a fresh EMPTY temp dir (ADR-049 #4
 * diff-withholding): it has no repository checkout. But the acceptance
 * prompt's Posture B (backend/internal/prompt/prompt.go buildAcceptance)
 * sanctions bounded repository-local validation of the merge candidate
 * for repository-content criteria. Before this file existed, nothing
 * provided that tree, so a Posture B check would grep whatever checkout it
 * could find on the host — the operator's dispatch checkout or the run's
 * lineage worktree, either of which working_tree_restored may have
 * detached back to main (run 34eae492). Every reference the PR deletes
 * then appears to "remain" and a criterion the PR head verifiably
 * satisfies false-fails assertion_fail, burning the fix-up budget and
 * paging the operator.
 *
 * provisionAcceptanceTree materializes the ONE sanctioned tree the prompt
 * names: a disposable, read-only detached checkout of the merge-candidate
 * head (acceptanceExpectedHeadSHA — the exact identity the #1569 target
 * gate verifies the preview serves) at a run/stage-keyed path, created via
 * `git worktree add --detach` against the operator's dispatch checkout
 * (the established provisionLineageWorktree pattern). It is torn down after
 * the stage.
 *
 * EVERY provisioning failure warns and PROCEEDS — never a stage failure:
 * the prompt's skip rule turns the degraded case into an honest skipped
 * criterion routed to triage, which beats a false assertion_fail, and the
 * preview-target criteria are unaffected either way.
 */

// acceptanceTreeDir is the directory the run/stage-keyed merge-candidate
// checkout lives in. var (not const) so tests can redirect it to a t.TempDir
// path and avoid /tmp pollution / parallel-test races, mirroring
// acceptanceVerdictDir.
var acceptanceTreeDir = "/tmp"

// acceptanceTreePath is the run/stage-keyed path of the disposable
// merge-candidate checkout. Keyed by the FULL run id + stage id (#1881), the
// same keying AcceptanceVerdictPath uses. The acceptance prompt NAMES this exact
// path (backend/internal/prompt/prompt.go AcceptanceTreePath) as the only
// sanctioned tree for Posture B criteria, so the prompt's write target and the
// runner's checkout path agree. MUST stay byte-identical to the prompt's
// AcceptanceTreePath format string — the two are pinned from each side
// (TestAcceptanceTreePath in prompt_test.go and TestAcceptanceTreePath here)
// because they are not importable across the module boundary.
func acceptanceTreePath(runID, stageID string) string {
	return filepath.Join(acceptanceTreeDir, fmt.Sprintf("fishhawk-acceptance-tree-%s-%s", runID, stageID))
}

// isGitWorkTree reports whether dir is inside a git work tree. It tests the
// PRINTED `--is-inside-work-tree` VALUE (not just the exit status): a bare repo
// prints "false" yet exits 0, so an exit-status-only guard would misclassify it
// as a work tree. Mirrors the AGENTS.md not-a-work-tree guard discipline.
func isGitWorkTree(ctx context.Context, dir string) bool {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--is-inside-work-tree").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// provisionAcceptanceTree provisions the disposable merge-candidate checkout the
// acceptance prompt's Posture B names, returning a teardown func that removes it.
// It NEVER fails the stage: every provisioning failure emits an
// acceptance_tree_* warn event and returns a no-op teardown, so the caller can
// defer the teardown unconditionally (the returned func is always non-nil). The
// prompt's skip-when-absent rule makes the degraded case an honest skipped
// criterion, and the preview-target criteria still validate.
//
// repoDir is the operator's dispatch checkout — the same repo the lineage
// worktrees hang off — so `git worktree add` there follows the
// provisionLineageWorktree pattern. headSHA is acceptanceExpectedHeadSHA, the
// backend-resolved merge-candidate identity the #1569 target gate verifies.
func provisionAcceptanceTree(ctx context.Context, repoDir, headSHA, runID, stageID string, logSink io.Writer) (teardown func()) {
	noop := func() {}

	// (i) Degrade class: no merge-candidate identity (empty expectation — a
	// pre-#1569 backend or an unresolvable lineage ledger), no dispatch checkout,
	// or a dispatch dir that is not a git work tree (e.g. a GHA runner without a
	// local checkout — same degrade class as the #1746 hookDir fallback). Skip
	// with acceptance_tree_skipped; the prompt hard rule still prevents wrong-tree
	// evaluation, so criteria skip honestly.
	if headSHA == "" || repoDir == "" || !isGitWorkTree(ctx, repoDir) {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"acceptance_tree_skipped","run_id":%q,"stage_id":%q,"head_sha":%q,"repo_dir":%q}`+"\n",
			runID, stageID, headSHA, repoDir)
		return noop
	}

	target := acceptanceTreePath(runID, stageID)

	// (ii) Sweep any stale leftover at the keyed path from a SIGKILL'd prior run
	// (mirroring sweepStaleAcceptanceVerdict): a leftover directory would make
	// `git worktree add` refuse. os.RemoveAll clears the tree; a best-effort
	// `git worktree prune` clears any dangling admin registration.
	if _, statErr := os.Stat(target); statErr == nil {
		_ = os.RemoveAll(target)
		_ = exec.CommandContext(ctx, "git", "-C", repoDir, "worktree", "prune").Run()
		_, _ = fmt.Fprintf(logSink,
			`{"event":"acceptance_tree_stale_swept","run_id":%q,"stage_id":%q,"path":%q}`+"\n",
			runID, stageID, target)
	}

	// (iii) Ensure the merge-candidate object is present. On the local dogfood
	// loop the SHA is normally already in the dispatch checkout's object store
	// (it hosts the lineage worktrees that produced the commit); a bare-SHA fetch
	// covers the case where it is not. A fetch failure is not fatal — it flows
	// into the (iv) `worktree add` failure, which warns and proceeds.
	if err := exec.CommandContext(ctx, "git", "-C", repoDir,
		"cat-file", "-e", headSHA+"^{commit}").Run(); err != nil {
		if out, ferr := exec.CommandContext(ctx, "git", "-C", repoDir,
			"fetch", "origin", headSHA).CombinedOutput(); ferr != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"acceptance_tree_fetch_failed","run_id":%q,"stage_id":%q,"head_sha":%q,"detail":%q}`+"\n",
				runID, stageID, headSHA, strings.TrimSpace(string(out)))
		}
	}

	// (iv) `git worktree add --detach <keyed-path> <sha>` — a detached checkout of
	// the merge-candidate head at an arbitrary path, moving no branch (the same
	// pattern provisionLineageWorktree / the verify gate use). A failure warns and
	// returns a no-op teardown: warn-and-proceed, never a stage failure.
	if out, err := exec.CommandContext(ctx, "git", "-C", repoDir,
		"worktree", "add", "--detach", target, headSHA).CombinedOutput(); err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"acceptance_tree_failed","run_id":%q,"stage_id":%q,"head_sha":%q,"path":%q,"detail":%q}`+"\n",
			runID, stageID, headSHA, target, strings.TrimSpace(string(out)))
		return noop
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"acceptance_tree_provisioned","run_id":%q,"stage_id":%q,"path":%q,"head_sha":%q}`+"\n",
		runID, stageID, target, headSHA)

	// Teardown: `git worktree remove --force <path>`, with an os.RemoveAll +
	// `git worktree prune` fallback. The fallback is path-canonicalization-proof:
	// on macOS /tmp is a symlink to /private/tmp, so the path git REGISTERED for
	// the worktree can differ from the path passed to `worktree remove`, making
	// the remove miss — os.RemoveAll deletes the tree regardless and prune clears
	// the registration by scanning for a missing tree. Best-effort; never changes
	// the stage outcome.
	return func() {
		if out, err := exec.CommandContext(ctx, "git", "-C", repoDir,
			"worktree", "remove", "--force", target).CombinedOutput(); err != nil {
			_ = os.RemoveAll(target)
			if pout, perr := exec.CommandContext(ctx, "git", "-C", repoDir,
				"worktree", "prune").CombinedOutput(); perr != nil {
				_, _ = fmt.Fprintf(logSink,
					`{"event":"acceptance_tree_teardown_failed","run_id":%q,"stage_id":%q,"path":%q,"detail":%q}`+"\n",
					runID, stageID, target, strings.TrimSpace(string(out))+"; prune: "+strings.TrimSpace(string(pout)))
				return
			}
			_, _ = fmt.Fprintf(logSink,
				`{"event":"acceptance_tree_removed","run_id":%q,"stage_id":%q,"path":%q,"fallback":"rm_prune"}`+"\n",
				runID, stageID, target)
			return
		}
		_, _ = fmt.Fprintf(logSink,
			`{"event":"acceptance_tree_removed","run_id":%q,"stage_id":%q,"path":%q}`+"\n",
			runID, stageID, target)
	}
}
