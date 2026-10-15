package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

// Stranded fix-up commit pin (#4337).
//
// The incident (run af08d34e, PR #4329): a fix-up pass's CommitAndPush created
// the local commit 6ccdf30e under the runner identity, then a post-commit gate
// (the pushed-tree re-verify) failed category B. Nothing pinned that commit, so
// the next checkout left it unreachable, the pass recovered as "delivered
// nothing", and the only copy of its valid in-scope work was an object gc could
// prune. The overseer recovered it by hand.
//
// pinStrandedFixupCommit closes that at the `commit+push` failure site: when a
// fix-up pass's local HEAD advanced past the base tip it started from, the HEAD
// commit is pinned at refs/fishhawk/checkpoints/<run>/<stage>-fixup-<sha12> (the
// #4079 / #4190 durable-ref namespace, childpushresume.go) and the failure
// reason gains a trailing clause naming the SHA and the ref, so the
// pull_request_failed reason, the stage failure reason and
// stage_fixup_recovered.source_failure_reason all say where the work is.
//
// The ref is DELIBERATELY not released by releaseCheckpointRefs: a later pass
// that succeeds does not make this pass's commit worthless (it may carry items
// the later pass dropped), so the operator decides what to recover. At most one
// is written per failed fix-up pass, bounded by the per-stage fix-up ceiling.

// checkpointFixupSuffix distinguishes a stranded fix-up commit pin from the
// held-commit, -stash and -verify pins. Like those it is a SUFFIX on the leaf,
// so every pin of a stage is a sibling ref and none is a directory/file
// conflict for another. The pinned SHA's 12-character prefix follows it, so two
// failed fix-up passes of one stage never overwrite each other's pin.
const checkpointFixupSuffix = "-fixup-"

// Bounded fixup_stranded_commit_not_pinned reasons. no_local_commit is not
// among them: the pass created no commit, the common shape, and is silent.
const (
	// strandedNotPinnedBaseUnresolved: the pass's base tip was never resolved
	// (fixup_base_tip_unresolved already logged that), so there is no anchor to
	// prove the HEAD commit is this pass's own.
	strandedNotPinnedBaseUnresolved = "base_unresolved"
	// strandedNotPinnedHeadUnresolvable: HEAD could not be read, or read empty.
	strandedNotPinnedHeadUnresolvable = "head_unresolvable"
	// strandedNotPinnedNotDescendant: HEAD is not a descendant of the base tip,
	// so it is not this pass's commit and is never pinned (a foreign history),
	// or the ancestry probe itself failed.
	strandedNotPinnedNotDescendant = "not_descendant"
)

// Package-level seams, in the fixupland.go style, so the fake-pusher run()
// tests — whose repoDir "." is runTestMain's shared throwaway repository, or
// the package dir when that setup degrades — never probe or write a real
// repository unseamed. HEAD is read through fixupLocalHead (fixupland.go).
// fixupBaseTipHead is the read that captures the fix-up's base tip before
// CommitAndPush (main.go); it is seamed so a run()-level test can fix the base
// without depending on the process cwd being a git checkout.
var (
	fixupBaseTipHead = gitRevParseHEAD
	fixupIsAncestor  = gitIsAncestor
	fixupPinRef      = pinCheckpointRef
)

// gitIsAncestor reports whether ancestor is an ancestor of (or equal to)
// descendant via `git merge-base --is-ancestor`: exit 0 is true, exit 1 is
// false, anything else (an unknown object, no repository) is an error.
func gitIsAncestor(ctx context.Context, repoDir, ancestor, descendant string) (bool, error) {
	err := exec.CommandContext(ctx, "git", "-C", repoDir, "merge-base", "--is-ancestor", ancestor, descendant).Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor %s %s: %w", ancestor, descendant, err)
}

// checkpointFixupRef is the ref that pins a stranded fix-up commit of a stage.
func checkpointFixupRef(runID, stageID, sha string) string {
	short := sha
	if len(short) > 12 {
		short = short[:12]
	}
	return checkpointRef(runID, stageID) + checkpointFixupSuffix + short
}

// pinStrandedFixupCommit pins a fix-up pass's local commit that never reached
// the PR branch and returns the reason clause naming it, or "" when nothing was
// pinned. It is called ONLY on the `commit+push` error arm of a fix-up pass
// whose failure armed no #3621 checkpoint (that arm already pinned the commit).
//
// Ladder, in order; each refusal but no_local_commit logs one
// fixup_stranded_commit_not_pinned {reason} line and returns "":
//
//	(a) baseTipSHA == ""          -> base_unresolved
//	(b) HEAD read error, or empty -> head_unresolvable (before any ancestry
//	    probe, so a seam reporting "" never reaches git merge-base)
//	(c) head == baseTipSHA        -> no_local_commit, silent
//	(d) base not an ancestor of HEAD, or the probe failed -> not_descendant
//	(e) the pin failed            -> the existing checkpoint_ref_pin_failed line
//
// On success it logs fixup_stranded_commit_pinned {ref, sha, base_sha}.
func pinStrandedFixupCommit(ctx context.Context, cfg config, logSink io.Writer, repoDir, baseTipSHA string) string {
	notPinned := func(reason, detail string) string {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"fixup_stranded_commit_not_pinned","run_id":%q,"stage_id":%q,"reason":%q,"detail":%q}`+"\n",
			cfg.runID, cfg.stageID, reason, detail)
		return ""
	}
	if baseTipSHA == "" {
		return notPinned(strandedNotPinnedBaseUnresolved, "")
	}
	head, err := fixupLocalHead(ctx, repoDir)
	if err != nil {
		return notPinned(strandedNotPinnedHeadUnresolvable, err.Error())
	}
	if head == "" {
		return notPinned(strandedNotPinnedHeadUnresolvable, "HEAD resolved empty")
	}
	if head == baseTipSHA {
		return ""
	}
	descends, err := fixupIsAncestor(ctx, repoDir, baseTipSHA, head)
	if err != nil {
		return notPinned(strandedNotPinnedNotDescendant, err.Error())
	}
	if !descends {
		return notPinned(strandedNotPinnedNotDescendant, fmt.Sprintf("base %s is not an ancestor of HEAD %s", baseTipSHA, head))
	}
	ref := checkpointFixupRef(cfg.runID, cfg.stageID, head)
	if err := fixupPinRef(ctx, repoDir, ref, head); err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"checkpoint_ref_pin_failed","run_id":%q,"stage_id":%q,"ref":%q,"sha":%q,"detail":%q}`+"\n",
			cfg.runID, cfg.stageID, ref, head, err.Error())
		return ""
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"fixup_stranded_commit_pinned","run_id":%q,"stage_id":%q,"ref":%q,"sha":%q,"base_sha":%q}`+"\n",
		cfg.runID, cfg.stageID, ref, head, baseTipSHA)
	return fmt.Sprintf("; fix-up commit %s never reached the PR branch; its work is pinned at %s (inspect with `git show %s`, recover with `git cherry-pick %s`)",
		head, ref, head, head)
}
