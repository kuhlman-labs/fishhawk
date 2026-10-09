package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
)

// Durable checkpoint refs and the decomposed-child push resume (#4079).
//
// The #4079 incident: a decomposition child passed its agent pass, then
// CommitAndPush's FreshFetchBase arm stashed the agent's edits and the fetch of
// the consolidated base failed. The work survived only as an anonymous entry on
// the repository's SHARED stash stack, and the operator rescued it by hand. The
// helpers below make every recovery point a NAMED ref and let a child's
// gate-verified work resume without re-running the agent.
//
// The ref namespace is refs/fishhawk/checkpoints/<run_id>/<stage_id> (the held,
// gate-verified commit a push-kind checkpoint resumes from) plus the same path
// with a "-stash" suffix (the stash commit holding stranded edits). Both are
// LOCAL-ONLY: they are never pushed and are invisible to branch-based tooling.
// They exist so git gc (which prunes only unreachable objects), a stash drop by
// another session, or a worktree teardown cannot orphan the work. refs/fishhawk/*
// is shared by every worktree of the repository (only refs/worktree/, refs/bisect/
// and refs/rewritten/ are per-worktree), so a retry dispatched into a different
// worktree of the same repository still sees them.

// checkpointRefPrefix is the namespace every checkpoint ref lives under.
const checkpointRefPrefix = "refs/fishhawk/checkpoints/"

// checkpointStashSuffix distinguishes the stash pin from the held-commit pin. A
// SUFFIX on the leaf (not a child path) keeps the two refs siblings, so neither
// is a directory/file conflict for the other.
const checkpointStashSuffix = "-stash"

// Bounded push_checkpoint_not_armed reasons new in #4079.
const (
	// notArmedNotDecomposedChild: the base-fetch-failure arm synthesizes a
	// commit parented on HEAD, which is the runner-controlled
	// child_base_checkout base only for a decomposition child. A standalone run
	// deliberately cuts from a freshly fetched base (ADR-035 / #861 / #797), so
	// parenting on its ambient HEAD could launder a foreign commit into the
	// branch base. Fix-ups commit onto their PR branch and are excluded too.
	notArmedNotDecomposedChild = "not_decomposed_child"
	// notArmedSynthesisFailed: `git commit-tree` failed (for example the
	// verified tree object is not in this object store) or the synthesized
	// commit's tree/parent did not re-prove.
	notArmedSynthesisFailed = "synthesis_failed"
)

// checkpointRef is the ref that pins a stage's held, gate-verified commit.
func checkpointRef(runID, stageID string) string {
	return checkpointRefPrefix + runID + "/" + stageID
}

// checkpointStashRef is the ref that pins the stash commit holding a stage's
// stranded agent edits.
func checkpointStashRef(runID, stageID string) string {
	return checkpointRef(runID, stageID) + checkpointStashSuffix
}

// pinCheckpointRef points ref at sha with `git update-ref`. An empty sha or an
// invalid ref name is git's own refusal.
func pinCheckpointRef(ctx context.Context, repoDir, ref, sha string) error {
	out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "update-ref", ref, sha).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git update-ref %s %s: %w (%s)", ref, sha, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// releaseCheckpointRefs deletes both checkpoint refs of a stage once its work is
// published. Best-effort and log-only: a stale ref costs one retained commit,
// never a wrong outcome, so a failure never changes the caller's result. Only
// refs that existed are named on the checkpoint_refs_released line.
func releaseCheckpointRefs(ctx context.Context, repoDir, runID, stageID string, logSink io.Writer) {
	var released []string
	for _, ref := range []string{checkpointRef(runID, stageID), checkpointStashRef(runID, stageID)} {
		if exec.CommandContext(ctx, "git", "-C", repoDir, "rev-parse", "--verify", "--quiet", ref).Run() != nil {
			continue // absent: nothing to release
		}
		if out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "update-ref", "-d", ref).CombinedOutput(); err != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"checkpoint_ref_release_failed","run_id":%q,"stage_id":%q,"ref":%q,"detail":%q}`+"\n",
				runID, stageID, ref, strings.TrimSpace(err.Error()+" "+string(out)))
			continue
		}
		released = append(released, ref)
	}
	if len(released) > 0 {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"checkpoint_refs_released","run_id":%q,"stage_id":%q,"refs":%q}`+"\n",
			runID, stageID, strings.Join(released, ","))
	}
}

// synthesizeVerifiedCommit builds a commit whose tree IS the gate-verified tree
// and whose parent is HEAD, without touching the working tree, the index or any
// branch: `git commit-tree <tree> -p HEAD`. It is the base-fetch-failure arm's
// stand-in for the commit CommitAndPush never reached.
//
// DCO: `git commit-tree` has no --signoff, so the message gets an explicit
// `Signed-off-by: <author name> <author email>` trailer through
// `git interpret-trailers` (which appends it to an existing trailer block rather
// than opening a new paragraph). The author and committer identity is the
// stage's App bot identity with the same gitops.DefaultAuthorName/Email
// fallback CommitAndPush uses.
//
// The result is RE-PROVED before it is returned: the new commit's tree must
// equal verifiedTreeSHA and its parent must equal the HEAD it was parented on.
func synthesizeVerifiedCommit(ctx context.Context, repoDir, verifiedTreeSHA, commitMessage, authorName, authorEmail string) (commitSHA, parentSHA string, err error) {
	if verifiedTreeSHA == "" {
		return "", "", errors.New("synthesize: no verified tree")
	}
	if strings.TrimSpace(commitMessage) == "" {
		return "", "", errors.New("synthesize: empty commit message")
	}
	if authorName == "" {
		authorName = gitops.DefaultAuthorName
	}
	if authorEmail == "" {
		authorEmail = gitops.DefaultAuthorEmail
	}
	parentSHA, err = gitRevParseIn(ctx, repoDir, "HEAD^{commit}")
	if err != nil || parentSHA == "" {
		return "", "", fmt.Errorf("synthesize: resolve HEAD: %v", err)
	}

	trailer := exec.CommandContext(ctx, "git", "-C", repoDir, "interpret-trailers",
		"--if-exists", "addIfDifferent", "--trailer", "Signed-off-by: "+authorName+" <"+authorEmail+">")
	trailer.Stdin = strings.NewReader(strings.TrimRight(commitMessage, "\n") + "\n")
	msg, err := trailer.Output()
	if err != nil {
		return "", "", fmt.Errorf("synthesize: add Signed-off-by trailer: %w", err)
	}

	commit := exec.CommandContext(ctx, "git", "-C", repoDir, "commit-tree", verifiedTreeSHA, "-p", parentSHA)
	commit.Stdin = strings.NewReader(string(msg))
	commit.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME="+authorName, "GIT_AUTHOR_EMAIL="+authorEmail,
		"GIT_COMMITTER_NAME="+authorName, "GIT_COMMITTER_EMAIL="+authorEmail)
	var stderr strings.Builder
	commit.Stderr = &stderr
	out, err := commit.Output()
	if err != nil {
		return "", "", fmt.Errorf("synthesize: git commit-tree %s: %w (%s)", verifiedTreeSHA, err, strings.TrimSpace(stderr.String()))
	}
	commitSHA = strings.TrimSpace(string(out))

	if tree, terr := gitRevParseIn(ctx, repoDir, commitSHA+"^{tree}"); terr != nil || tree != verifiedTreeSHA {
		return "", "", fmt.Errorf("synthesize: commit %s tree %q != verified tree %q (err %v)", commitSHA, tree, verifiedTreeSHA, terr)
	}
	if parent, perr := gitRevParseIn(ctx, repoDir, commitSHA+"^"); perr != nil || parent != parentSHA {
		return "", "", fmt.Errorf("synthesize: commit %s parent %q != HEAD %q (err %v)", commitSHA, parent, parentSHA, perr)
	}
	return commitSHA, parentSHA, nil
}

// checkpointFailureSuffix renders the clause appended to a `commit+push:` error
// naming each recovery point actually pinned, so the reason that reaches the
// pull_request_failed audit row says where the work is. armedRef is the
// held-commit pin of an ARMED push-kind checkpoint; stashRef the stash pin.
// Either may be empty; both empty renders nothing, so every other failure's
// reason is byte-identical to before.
func checkpointFailureSuffix(armedRef, stashRef string) string {
	var b strings.Builder
	if armedRef != "" {
		b.WriteString("; gate-verified work pinned at " + armedRef +
			"; fishhawk_retry_stage resumes it without re-running the agent")
	}
	if stashRef != "" {
		b.WriteString("; stashed agent edits pinned at " + stashRef +
			" (recover with `git stash apply " + stashRef + "`)")
	}
	return b.String()
}

// maybeArmBaseFetchFailureCheckpoint is the #4079 sibling of
// maybeArmPushFailureCheckpoint, called at the same `commit+push` error site.
// It is SILENT unless the error is gitops.ErrBaseFetchFailed: CommitAndPush
// stashed the agent's edits and then failed fetching (or checking out) the base,
// so no commit exists and #3621's ErrPushFailed arm cannot fire.
//
// It does two independent things and returns the refs it pinned:
//
//  1. STASH PIN, unconditionally: whenever the error names a stash commit, that
//     commit is pinned at checkpointStashRef — even with no verified tree, the
//     incident's exact shape (its verify was skipped, not passed). This alone
//     keeps the edits durable.
//  2. PUSH-KIND ARM, only when every precondition holds, each refusal named on
//     a push_checkpoint_not_armed line: a decomposition child that is not a
//     fix-up (not_decomposed_child); the backend advertised push-resume
//     (backend_capability_absent); a non-empty verifiedTreeSHA
//     (no_verified_tree); and synthesizeVerifiedCommit succeeds
//     (synthesis_failed). On arm the checkpoint records branch = the slice
//     branch, head = the synthesized commit, base = HEAD, and the head is
//     pinned at checkpointRef.
//
// For a child, HEAD here is the runner-controlled child_base_checkout base the
// verify ran on (the failed fetch/checkout moved nothing), so a commit of the
// verified tree on HEAD is the commit CommitAndPush would have pushed had the
// fetch of that same base succeeded. The resume still runs all four consume
// guards (verified tree present, object present, tree equality, non-clobber).
func maybeArmBaseFetchFailureCheckpoint(ctx context.Context, cfg config, logSink io.Writer, checkpoint *pushCheckpoint, supportsPushResume bool, commitErr error, repoDir, branch, verifiedTreeSHA, commitMessage, prTitle, prBody string) (armedRef, stashRef string) {
	if !errors.Is(commitErr, gitops.ErrBaseFetchFailed) {
		return "", ""
	}
	pin := func(ref, sha string) bool {
		if err := pinCheckpointRef(ctx, repoDir, ref, sha); err != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"checkpoint_ref_pin_failed","run_id":%q,"stage_id":%q,"ref":%q,"sha":%q,"detail":%q}`+"\n",
				cfg.runID, cfg.stageID, ref, sha, err.Error())
			return false
		}
		_, _ = fmt.Fprintf(logSink,
			`{"event":"checkpoint_ref_pinned","run_id":%q,"stage_id":%q,"ref":%q,"sha":%q}`+"\n",
			cfg.runID, cfg.stageID, ref, sha)
		return true
	}
	var bfe *gitops.BaseFetchError
	if errors.As(commitErr, &bfe) && bfe.StashSHA != "" {
		if ref := checkpointStashRef(cfg.runID, cfg.stageID); pin(ref, bfe.StashSHA) {
			stashRef = ref
		}
	}
	if checkpoint == nil {
		return "", stashRef
	}
	notArmed := func(reason string) {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"push_checkpoint_not_armed","run_id":%q,"stage_id":%q,"reason":%q,"source":"base_fetch_failure"}`+"\n",
			cfg.runID, cfg.stageID, reason)
	}
	if cfg.decomposedFromRunID == "" || cfg.fixup {
		notArmed(notArmedNotDecomposedChild)
		return "", stashRef
	}
	if !supportsPushResume {
		notArmed(notArmedBackendCapabilityAbsent)
		return "", stashRef
	}
	if verifiedTreeSHA == "" {
		notArmed(notArmedNoVerifiedTree)
		return "", stashRef
	}
	headSHA, baseSHA, err := synthesizeVerifiedCommit(ctx, repoDir, verifiedTreeSHA, commitMessage, cfg.commitAuthorName, cfg.commitAuthorEmail)
	if err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"verified_commit_synthesis_failed","run_id":%q,"stage_id":%q,"detail":%q}`+"\n",
			cfg.runID, cfg.stageID, err.Error())
		notArmed(notArmedSynthesisFailed)
		return "", stashRef
	}
	*checkpoint = pushCheckpoint{
		branch:                    branch,
		headSHA:                   headSHA,
		baseSHA:                   baseSHA,
		verifiedTreeSHA:           verifiedTreeSHA,
		armed:                     true,
		resumeKind:                resumeKindPush,
		backendSupportsPushResume: supportsPushResume,
		prTitle:                   prTitle,
		prBody:                    prBody,
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"push_checkpoint_armed","run_id":%q,"stage_id":%q,"branch":%q,"head_sha":%q,"base_sha":%q,"verified_tree_sha":%q,"resume_kind":%q,"source":"base_fetch_failure"}`+"\n",
		cfg.runID, cfg.stageID, branch, headSHA, baseSHA, verifiedTreeSHA, resumeKindPush)
	if ref := checkpointRef(cfg.runID, cfg.stageID); pin(ref, headSHA) {
		armedRef = ref
	}
	return armedRef, stashRef
}

// pinPushFailureCheckpoint pins the held commit of a push-kind checkpoint the
// #3621 ErrPushFailed arm just armed, returning the ref on success and "" when
// nothing was armed or the pin failed (logged). Kept separate from
// maybeArmPushFailureCheckpoint so that function's signature, and every
// existing test call site, is unchanged.
func pinPushFailureCheckpoint(ctx context.Context, cfg config, logSink io.Writer, checkpoint *pushCheckpoint, repoDir string) string {
	if checkpoint == nil || !checkpoint.armed || checkpoint.resumeKind != resumeKindPush {
		return ""
	}
	ref := checkpointRef(cfg.runID, cfg.stageID)
	if err := pinCheckpointRef(ctx, repoDir, ref, checkpoint.headSHA); err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"checkpoint_ref_pin_failed","run_id":%q,"stage_id":%q,"ref":%q,"sha":%q,"detail":%q}`+"\n",
			cfg.runID, cfg.stageID, ref, checkpoint.headSHA, err.Error())
		return ""
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"checkpoint_ref_pinned","run_id":%q,"stage_id":%q,"ref":%q,"sha":%q}`+"\n",
		cfg.runID, cfg.stageID, ref, checkpoint.headSHA)
	return ref
}

// heldCommitFilesChanged counts the paths the held commit changes against its
// recorded base, for the child resume's pushed report. The resume never checks
// the held commit out, so the index-vs-base diff childPushFilesChanged uses on
// the ordinary child arm would count the WRONG tree here; this diffs the two
// commits directly. Best-effort and advisory, like childPushFilesChanged: a
// failure returns 0 and emits child_push_files_changed_unavailable.
func heldCommitFilesChanged(ctx context.Context, repoDir, baseSHA, headSHA, runID, stageID string, logSink io.Writer) int {
	out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "diff", "--name-only", "-z", baseSHA, headSHA).Output()
	if err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"child_push_files_changed_unavailable","run_id":%q,"stage_id":%q,"base":%q,"detail":%q}`+"\n",
			runID, stageID, baseSHA, err.Error())
		return 0
	}
	n := 0
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			n++
		}
	}
	return n
}
