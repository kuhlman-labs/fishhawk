package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// snapshotRefPrefix is the local-only namespace a working-tree snapshot is
// stored under, one ref per key (the stage id). It is never pushed.
const snapshotRefPrefix = "refs/fishhawk/stash/"

// Snapshot names a working-tree snapshot: a stash-SHAPED commit stored at Ref
// instead of on the repository's shared refs/stash stack (ADR-115 D2, #4648).
// SHA is the snapshot commit itself, so `git stash apply <SHA>` restores it
// by hand exactly as it would a stash entry.
type Snapshot struct {
	Ref string
	SHA string
}

// SnapshotRef is the ref a snapshot for key is stored at.
func SnapshotRef(key string) string { return snapshotRefPrefix + key }

// SnapshotWorkTree saves the working tree's tracked changes, staged state and
// untracked (non-ignored) files as a stash-shaped commit at SnapshotRef(key),
// then reverts the working tree the way `git stash push --include-untracked`
// does. It never touches refs/stash: the stash stack is shared by every
// worktree of the repository, so a stash push/pop in one worktree can pop a
// sibling's (or the operator's) entry.
//
// The commit follows builtin stash's layout (git-stash(1) DISCUSSION) so
// `git stash apply <sha>` and `git stash show -p <sha>` work on it:
//
//   - H is HEAD;
//   - I (parent H) records the index, written from a PRIVATE copy of the
//     worktree's index file so the real index is only touched by the revert;
//   - U (no parent), present only when there are untracked files, records the
//     `ls-files --others --exclude-standard` set under a fresh private index —
//     untracked files are included and ignored ones are not;
//   - W (parents H, I[, U]) is I plus every tracked working-tree change.
//
// paths, when non-empty, limits both the capture and the revert to those
// pathspecs; files outside them are untouched. When there is nothing to save
// it returns created=false and writes no ref. All snapshot commits carry
// IdentityEnv(DefaultAuthorName, DefaultAuthorEmail) and are never signed:
// they are local-only and must not depend on an ambient identity or signing
// agent.
//
// A failure before the ref is written leaves the working tree unchanged and
// returns created=false. A failure reverting the working tree AFTER the ref is
// written returns the snapshot, created=true and the error, so the caller can
// still name the saved commit.
func (p *Pusher) SnapshotWorkTree(ctx context.Context, repoDir, key string, paths []string) (Snapshot, bool, error) {
	if key == "" {
		return Snapshot{}, false, errors.New("gitops: snapshot: empty key")
	}
	var pathspec []string
	if len(paths) > 0 {
		pathspec = append([]string{"--"}, paths...)
	}

	head, err := p.runOut(ctx, repoDir, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("gitops: snapshot: resolve HEAD: %w", err)
	}
	head = strings.TrimSpace(head)

	tmpDir, err := os.MkdirTemp("", "fishhawk-snapshot-*")
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("gitops: snapshot: private index dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	indexPath := filepath.Join(tmpDir, "index")
	if err := p.copyWorktreeIndex(ctx, repoDir, indexPath); err != nil {
		return Snapshot{}, false, err
	}
	indexEnv := []string{"GIT_INDEX_FILE=" + indexPath}
	// Refresh the private copy's stat data so a touched-but-unchanged file is
	// not mistaken for a change; the real index is never written here.
	if _, err := p.runOutEnv(ctx, repoDir, indexEnv, "update-index", "-q", "--refresh"); err != nil {
		return Snapshot{}, false, fmt.Errorf("gitops: snapshot: refresh private index: %w", err)
	}

	tracked, err := p.trackedChanges(ctx, repoDir, indexEnv, pathspec)
	if err != nil {
		return Snapshot{}, false, err
	}
	lsOut, err := p.runOut(ctx, repoDir, append([]string{"ls-files", "-z", "--others", "--exclude-standard"}, pathspec...)...)
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("gitops: snapshot: list untracked files: %w", err)
	}
	untracked := splitNUL(lsOut)
	if !tracked && len(untracked) == 0 {
		return Snapshot{}, false, nil
	}

	identEnv := IdentityEnv(DefaultAuthorName, DefaultAuthorEmail)
	msg := "fishhawk snapshot " + key

	iCommit, err := p.commitIndexTree(ctx, repoDir, indexEnv, identEnv, "index on "+msg, head)
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("gitops: snapshot: index commit: %w", err)
	}
	wParents := []string{head, iCommit}

	if len(untracked) > 0 {
		uEnv := []string{"GIT_INDEX_FILE=" + filepath.Join(tmpDir, "untracked-index")}
		if _, err := p.runStdinEnv(ctx, repoDir, uEnv, lsOut, "update-index", "-z", "--add", "--stdin"); err != nil {
			return Snapshot{}, false, fmt.Errorf("gitops: snapshot: stage untracked files: %w", err)
		}
		uCommit, err := p.commitIndexTree(ctx, repoDir, uEnv, identEnv, "untracked files on "+msg)
		if err != nil {
			return Snapshot{}, false, fmt.Errorf("gitops: snapshot: untracked commit: %w", err)
		}
		wParents = append(wParents, uCommit)
	}

	// W is I plus every tracked working-tree change, built on the same private
	// index the way builtin stash's stash_working_tree does: the paths that
	// differ from HEAD, fed to update-index. (`add -u -- <paths>` is not
	// usable here: it refuses a pathspec naming only untracked files.)
	changed, err := p.runOutEnv(ctx, repoDir, indexEnv, append([]string{"diff-index", "--name-only", "-z", "HEAD"}, pathspec...)...)
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("gitops: snapshot: list tracked changes: %w", err)
	}
	if changed != "" {
		if _, err := p.runStdinEnv(ctx, repoDir, indexEnv, changed, "update-index", "--ignore-skip-worktree-entries", "-z", "--add", "--remove", "--stdin"); err != nil {
			return Snapshot{}, false, fmt.Errorf("gitops: snapshot: stage tracked changes: %w", err)
		}
	}
	wCommit, err := p.commitIndexTree(ctx, repoDir, indexEnv, identEnv, msg, wParents...)
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("gitops: snapshot: working-tree commit: %w", err)
	}

	snap := Snapshot{Ref: SnapshotRef(key), SHA: wCommit}
	if err := p.refOp(ctx, repoDir, "update-ref", snap.Ref, snap.SHA); err != nil {
		return Snapshot{}, false, fmt.Errorf("gitops: snapshot: write %s: %w", snap.Ref, err)
	}

	if err := p.revertSnapshotted(ctx, repoDir, paths); err != nil {
		return snap, true, fmt.Errorf("gitops: snapshot: revert working tree (edits saved in snapshot commit %s at %s): %w", snap.SHA, snap.Ref, err)
	}
	return snap, true, nil
}

// copyWorktreeIndex copies the worktree's own index file (per-worktree, at
// `rev-parse --git-path index`) to dst. A worktree with no index file yet
// leaves dst absent, which git reads as an empty index.
func (p *Pusher) copyWorktreeIndex(ctx context.Context, repoDir, dst string) error {
	out, err := p.runOut(ctx, repoDir, "rev-parse", "--git-path", "index")
	if err != nil {
		return fmt.Errorf("gitops: snapshot: locate index: %w", err)
	}
	src := strings.TrimSpace(out)
	if !filepath.IsAbs(src) {
		src = filepath.Join(repoDir, src)
	}
	data, err := os.ReadFile(src)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("gitops: snapshot: read index: %w", err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return fmt.Errorf("gitops: snapshot: write private index: %w", err)
	}
	return nil
}

// trackedChanges reports whether the private index or the working tree
// differs from HEAD within pathspec (`diff-index --quiet HEAD`: exit 1 means
// a difference, 0 none, anything else is an error).
func (p *Pusher) trackedChanges(ctx context.Context, repoDir string, indexEnv, pathspec []string) (bool, error) {
	_, err := p.runOutEnv(ctx, repoDir, indexEnv, append([]string{"diff-index", "--quiet", "HEAD"}, pathspec...)...)
	if err == nil {
		return false, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return true, nil
	}
	return false, fmt.Errorf("gitops: snapshot: probe tracked changes: %w", err)
}

// commitIndexTree writes the tree of the index named by indexEnv and records
// it as an unsigned commit with the given parents.
func (p *Pusher) commitIndexTree(ctx context.Context, repoDir string, indexEnv, identEnv []string, message string, parents ...string) (string, error) {
	tree, err := p.runOutEnv(ctx, repoDir, indexEnv, "write-tree")
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", "--no-gpg-sign", strings.TrimSpace(tree), "-m", message}
	for _, parent := range parents {
		args = append(args, "-p", parent)
	}
	sha, err := p.runOutEnv(ctx, repoDir, identEnv, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(sha), nil
}

// revertSnapshotted returns the snapshotted part of the working tree to HEAD
// the way stash push does. With no paths: `clean -d` (no -x, so ignored files
// stay) then `reset --hard`. With paths: unstage them, clean the untracked
// ones, then check the modified/deleted ones out from the index — every file
// outside paths is untouched.
//
// The clean always runs BEFORE any tracked file is restored, so it judges
// "ignored" by the same working-tree rules the snapshot's untracked listing
// used. Restoring an edited .gitignore first would un-ignore a file the edit
// newly ignored — a file the snapshot excluded — and the clean would then
// delete it with no copy saved anywhere (builtin stash orders its clean
// before its reset for the same reason).
func (p *Pusher) revertSnapshotted(ctx context.Context, repoDir string, paths []string) error {
	if len(paths) == 0 {
		if err := p.run(ctx, repoDir, "clean", "--force", "--quiet", "-d"); err != nil {
			return err
		}
		return p.run(ctx, repoDir, "reset", "--hard", "-q")
	}
	pathspec := append([]string{"--"}, paths...)
	if err := p.run(ctx, repoDir, append([]string{"reset", "-q"}, pathspec...)...); err != nil {
		return err
	}
	if err := p.run(ctx, repoDir, append([]string{"clean", "--force", "--quiet", "-d"}, pathspec...)...); err != nil {
		return err
	}
	modified, err := p.runOut(ctx, repoDir, append([]string{"ls-files", "-z", "--modified"}, pathspec...)...)
	if err != nil {
		return err
	}
	if modified == "" {
		return nil
	}
	_, err = p.runStdinEnv(ctx, repoDir, nil, modified, "checkout-index", "-z", "--force", "--stdin")
	return err
}

// RestoreSnapshot reapplies snap onto the current HEAD with `git stash apply
// <sha>` — the same merge machinery a stash pop uses — and carries popStash's
// conflict contract (#866/#893/#989) unchanged:
//
//   - a failed apply whose unmerged probe itself fails gets a best-effort
//     `reset --hard` and an annotated error that is NOT ErrBaseRebaseConflict
//     (the conflict was never confirmed);
//   - a failed apply with no unmerged entries is a generic wrapped error;
//   - a real conflict captures the `git diff` hunks (they only exist before
//     the abort), aborts with `reset --hard`, captures `stash show -p <sha>`,
//     and returns *BaseRebaseConflictError.
//
// The snapshot ref is never deleted here; on a conflict it is what keeps the
// edits recoverable. A successful restore leaves dropping it to the caller
// (DropSnapshot).
func (p *Pusher) RestoreSnapshot(ctx context.Context, repoDir string, snap Snapshot) error {
	if snap.SHA == "" {
		return errors.New("gitops: restore snapshot: empty snapshot SHA")
	}
	applyErr := p.run(ctx, repoDir, "stash", "apply", snap.SHA)
	if applyErr == nil {
		return nil
	}
	unmerged, lsErr := p.runOut(ctx, repoDir, "ls-files", "--unmerged")
	if lsErr != nil {
		if resetErr := p.run(ctx, repoDir, "reset", "--hard"); resetErr != nil {
			return fmt.Errorf("gitops: restore snapshot %s (conflict detection via ls-files failed: %v; best-effort reset --hard also failed: %v): %w", snap.SHA, lsErr, resetErr, applyErr)
		}
		return fmt.Errorf("gitops: restore snapshot %s (conflict detection via ls-files failed: %v; working tree reset to HEAD): %w", snap.SHA, lsErr, applyErr)
	}
	if strings.TrimSpace(unmerged) == "" {
		return fmt.Errorf("gitops: restore snapshot %s: %w", snap.SHA, applyErr)
	}
	conflictPaths := unmergedConflictPaths(unmerged)
	var conflictHunks string
	if d, derr := p.runOut(ctx, repoDir, "diff"); derr == nil {
		conflictHunks = truncateCapture(d)
	}
	if err := p.run(ctx, repoDir, "reset", "--hard"); err != nil {
		return fmt.Errorf("gitops: reset --hard after conflicted snapshot restore %s: %w", snap.SHA, err)
	}
	var stashPatch string
	if sp, serr := p.runOut(ctx, repoDir, "stash", "show", "-p", snap.SHA); serr == nil {
		stashPatch = truncateCapture(sp)
	}
	return &BaseRebaseConflictError{
		ConflictPaths: conflictPaths,
		ConflictHunks: conflictHunks,
		StashPatch:    stashPatch,
		popErr:        applyErr,
	}
}

// DropSnapshot deletes snap's ref only while it still names snap's commit
// (`update-ref -d <ref> <sha>`), so it can never delete a ref another call
// re-pointed. The zero Snapshot (nothing was saved) is a no-op; a ref outside
// the snapshot namespace is refused.
func (p *Pusher) DropSnapshot(ctx context.Context, repoDir string, snap Snapshot) error {
	if snap == (Snapshot{}) {
		return nil
	}
	if snap.SHA == "" || !strings.HasPrefix(snap.Ref, snapshotRefPrefix) || len(snap.Ref) == len(snapshotRefPrefix) {
		return fmt.Errorf("gitops: drop snapshot: refusing incomplete or foreign snapshot (ref %q, sha %q)", snap.Ref, snap.SHA)
	}
	if err := p.refOp(ctx, repoDir, "update-ref", "-d", snap.Ref, snap.SHA); err != nil {
		return fmt.Errorf("gitops: drop snapshot %s: %w", snap.Ref, err)
	}
	return nil
}

// DropSnapshotRef deletes whatever SnapshotRef(key) holds — the stage-end
// sweep of a snapshot a conflict retained. An absent ref is success.
func (p *Pusher) DropSnapshotRef(ctx context.Context, repoDir, key string) error {
	if key == "" {
		return errors.New("gitops: drop snapshot ref: empty key")
	}
	ref := SnapshotRef(key)
	sha, code, err := p.probeOut(ctx, repoDir, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		if code == 1 && strings.TrimSpace(sha) == "" {
			return nil
		}
		return fmt.Errorf("gitops: drop snapshot ref %s: resolve: %w", ref, err)
	}
	return p.DropSnapshot(ctx, repoDir, Snapshot{Ref: ref, SHA: strings.TrimSpace(sha)})
}

// refOp runs a ref-writing git command through RetryLockContention under
// LockRetryLocaleEnv, so a sibling worktree briefly holding the ref's (or
// packed-refs') lock waits within the bound instead of failing the call.
func (p *Pusher) refOp(ctx context.Context, repoDir string, gitArgs ...string) error {
	_, err := RetryLockContention(ctx, func() ([]byte, error) {
		out, err := p.runOutEnv(ctx, repoDir, LockRetryLocaleEnv(), gitArgs...)
		return []byte(out), err
	})
	return err
}

// runStdinEnv is runOutEnv with stdin fed to the child, for the `--stdin` forms
// of update-index and checkout-index. It carries the same non-interactive and
// maintenance pins and goes through the same p.Cmd seam.
func (p *Pusher) runStdinEnv(ctx context.Context, dir string, env []string, stdin string, gitArgs ...string) (string, error) {
	base, err := AppendConfigEnv(append(os.Environ(), NonInteractiveEnv()...))
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(gitArgs, " "), err)
	}
	binary := p.Binary
	if binary == "" {
		binary = "git"
	}
	cmdFn := p.Cmd
	if cmdFn == nil {
		cmdFn = exec.CommandContext
	}
	cmd := cmdFn(ctx, binary, gitArgs...)
	cmd.Dir = dir
	cmd.Env = append(base, env...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w (stderr: %s)",
			strings.Join(gitArgs, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// splitNUL splits `-z` output into its non-empty entries.
func splitNUL(s string) []string {
	var out []string
	for _, f := range strings.Split(s, "\x00") {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}
