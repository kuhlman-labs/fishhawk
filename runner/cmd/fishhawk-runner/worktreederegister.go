package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// gitAdminRunner runs one git admin command against a fixed repository and
// returns its output. It is the seam deregisterWorktree runs every command
// through, so each call site keeps its own exec choke point (the conflict pass
// routes through gitOutRaw's HardeningArgs) and tests can stub the git side.
type gitAdminRunner func(ctx context.Context, args ...string) ([]byte, error)

// worktreeTeardownTimeout bounds a worktree teardown that runs on a context
// detached from the (possibly cancelled) stage context.
const worktreeTeardownTimeout = 2 * time.Minute

// detachedTeardownContext returns a context that survives the cancellation of
// ctx but is still bounded, so a cancelled run neither strands a worktree
// registration nor masks a deregistration failure (#4281).
func detachedTeardownContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), worktreeTeardownTimeout)
}

// deregisterWorktree is the targeted replacement for `git worktree prune`
// (#4281).
//
// (a) It deregisters exactly ONE entry, the worktree at path, and never runs a
// repository-wide prune.
//
// (b) Why: a blanket prune deletes the .git/worktrees/<id> admin dir of ANY
// registered worktree whose <path>/.git the calling process cannot see. From a
// partial or stale filesystem view (a sandbox, another mount namespace, a
// temporarily moved directory) that destroys live worktrees outside the repo,
// and git then fails in them with "not a git repository".
//
// (c) `--force --force` removes a LOCKED entry and one whose tree is already
// missing (git-worktree(1): "specify --force twice to remove a locked working
// tree").
//
// (d) It ALSO deletes the tree if it is still present, so callers pass only a
// path they own. A path that is not absolute is refused before any git call:
// git-worktree(1) resolves a relative argument by unique trailing path
// components, so it could match, and delete, a foreign worktree.
//
// When the remove fails, the registration is checked through the same runner.
// A path that is not registered returns nil: nothing is left to deregister. A
// failed listing, or one with no worktree records, returns an error (fail
// closed): empty or failed output never reads as "not registered".
func deregisterWorktree(ctx context.Context, git gitAdminRunner, path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("deregister worktree %q: path is not absolute", path)
	}
	var last error
	for _, p := range []string{path} {
		args := []string{"worktree", "remove", "--force", "--force", p}
		out, err := git(ctx, args...)
		if err == nil {
			return nil
		}
		last = fmt.Errorf("git worktree remove %s: %w: %s", p, err, strings.TrimSpace(string(out)))
	}
	out, err := git(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return fmt.Errorf("%w; git worktree list: %v: %s", last, err, strings.TrimSpace(string(out)))
	}
	registered, records := porcelainListsWorktree(string(out), path)
	if records == 0 {
		// A working listing always names at least the main worktree, so an
		// empty one is a failed read, not proof the entry is gone.
		return fmt.Errorf("%w; git worktree list returned no worktree records", last)
	}
	if !registered {
		return nil
	}
	return last
}

// porcelainListsWorktree reports whether `git worktree list --porcelain`
// output carries a `worktree <path>` record naming path, comparing both sides
// through canonWorktreePath. records counts every `worktree` record seen.
func porcelainListsWorktree(porcelain, path string) (registered bool, records int) {
	want := canonWorktreePath(path)
	for _, line := range strings.Split(porcelain, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			records++
			if canonWorktreePath(strings.TrimSpace(p)) == want {
				registered = true
			}
		}
	}
	return registered, records
}

// canonWorktreePath resolves symlinks in p. When the leaf is missing (the
// usual case after os.RemoveAll), it resolves the PARENT directory and rejoins
// the base name, walking further up while the parent is missing too, so the
// macOS alias /tmp -> /private/tmp still matches the path git registered.
func canonWorktreePath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	clean := filepath.Clean(p)
	suffix := filepath.Base(clean)
	for dir := filepath.Dir(clean); ; dir = filepath.Dir(dir) {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, suffix)
		}
		if next := filepath.Dir(dir); next == dir {
			return clean
		}
		suffix = filepath.Join(filepath.Base(dir), suffix)
	}
}
