package basedelta

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/procgroup"
	"github.com/kuhlman-labs/fishhawk/backend/internal/reviewsandbox"
)

// DefaultGitTimeout bounds each git child GitDelta and MaterializeTree run.
const DefaultGitTimeout = 30 * time.Second

// gitStderrTail bounds the stderr tail a failed git child's error carries.
const gitStderrTail = 256

// Typed sentinels for GitDelta and MaterializeTree. InputsErrorCode maps each
// to its closed reason code.
var (
	// ErrInvalidObjectID: an input is not a full 40- or 64-hex lowercase
	// object id. Refused before any git child runs.
	ErrInvalidObjectID = errors.New("basedelta: not a full object id")
	// ErrUnexpectedParents: the candidate is not a two-parent merge whose
	// first parent is the prior head.
	ErrUnexpectedParents = errors.New("basedelta: candidate is not a two-parent merge of the prior head")
	// ErrNotAncestor: the root head is not an ancestor of the prior head.
	ErrNotAncestor = errors.New("basedelta: root head is not an ancestor of the prior head")
	// ErrAmbiguousMergeBase: the root head and B' have more than one merge base
	// (criss-cross history).
	ErrAmbiguousMergeBase = errors.New("basedelta: more than one merge base")
	// ErrNoMergeBase: the root head and B' share no history.
	ErrNoMergeBase = errors.New("basedelta: no merge base")
	// ErrIncompleteTree: the commit's tree holds an entry that cannot be
	// materialized (a submodule gitlink, an entry whose parent is a symlink),
	// or exceeds the materialize limits, so a workspace would be incomplete.
	ErrIncompleteTree = errors.New("basedelta: tree cannot be materialized completely")
	// ErrSymlinkEscape: a tree symlink resolves outside the repository (an
	// absolute target, a .. above the root, or a loop), so what it aliases
	// cannot be classified.
	ErrSymlinkEscape = errors.New("basedelta: symlink resolves outside the repository")
	// ErrUnsafeTreePath: a tree entry's path is empty, absolute, has a ., ..
	// or empty component, or names .git.
	ErrUnsafeTreePath = errors.New("basedelta: unsafe tree path")
	// ErrGit: a git child failed (non-zero exit, start failure, timeout or
	// malformed output). The error carries a stderr tail of at most 256 bytes.
	ErrGit = errors.New("basedelta: git failed")
)

// InputsErrorCode maps a GitDelta / MaterializeTree error to a closed reason
// code: deadline_exceeded (the caller's context deadline), invalid_object_id,
// unexpected_parents, not_ancestor, ambiguous_merge_base, no_merge_base,
// incomplete_tree, unsafe_tree_path, symlink_escape, git, or unknown.
func InputsErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, ErrInvalidObjectID):
		return "invalid_object_id"
	case errors.Is(err, ErrUnexpectedParents):
		return "unexpected_parents"
	case errors.Is(err, ErrNotAncestor):
		return "not_ancestor"
	case errors.Is(err, ErrAmbiguousMergeBase):
		return "ambiguous_merge_base"
	case errors.Is(err, ErrNoMergeBase):
		return "no_merge_base"
	case errors.Is(err, ErrIncompleteTree):
		return "incomplete_tree"
	case errors.Is(err, ErrUnsafeTreePath):
		return "unsafe_tree_path"
	case errors.Is(err, ErrSymlinkEscape):
		return "symlink_escape"
	case errors.Is(err, ErrGit):
		return "git"
	}
	return "unknown"
}

// DeltaInputs are the classifier's path inputs for one merge candidate.
type DeltaInputs struct {
	// FromBase is the unique merge base of the root head and ToBase.
	FromBase string
	// ToBase is B', the candidate's second parent.
	ToBase string
	// DeltaPaths are FromBase..ToBase, both sides of a rename.
	DeltaPaths []string
	// PRPaths are the de-duplicated union of FromBase..rootHead and
	// ToBase..candidate, both sides of a rename: the second diff carries an
	// edit made only in the merge commit and any commit between rootHead and
	// the prior head.
	PRPaths []string
}

// gitRunner runs one git child in repoDir and returns its stdout. The seam
// lets a test count children (TestGitDelta_NonSHAInputsRefused).
type gitRunner func(ctx context.Context, repoDir string, args ...string) ([]byte, error)

// GitDelta reads the classifier inputs for candidate, the merge commit a clean
// base advance produced on top of priorHead, from the LOCAL repository at
// repoDir (no fetch). rootHead is the head whose recorded pass anchors the
// transfer chain (priorHead itself when its verdict was recorded).
//
//  1. Every id must be a full object id (ErrInvalidObjectID), checked before
//     any git child runs.
//  2. `rev-list --parents -n 1 candidate` must name exactly two parents with
//     parent 1 == priorHead (ErrUnexpectedParents); ToBase is parent 2.
//  3. `merge-base --is-ancestor rootHead priorHead` (ErrNotAncestor).
//  4. `merge-base --all rootHead ToBase` must yield exactly one base
//     (ErrAmbiguousMergeBase, ErrNoMergeBase); FromBase is that base.
//  5. DeltaPaths = FromBase..ToBase; PRPaths = the de-duplicated union of
//     FromBase..rootHead and ToBase..candidate; all via `diff -z
//     --no-renames --name-only`, so a rename reports both sides.
func GitDelta(ctx context.Context, repoDir, candidate, priorHead, rootHead string) (DeltaInputs, error) {
	return gitDelta(ctx, runGit, repoDir, candidate, priorHead, rootHead)
}

func gitDelta(ctx context.Context, git gitRunner, repoDir, candidate, priorHead, rootHead string) (DeltaInputs, error) {
	for _, id := range []struct{ name, v string }{{"candidate", candidate}, {"prior head", priorHead}, {"root head", rootHead}} {
		if !isFullObjectID(id.v) {
			return DeltaInputs{}, fmt.Errorf("%w: %s %q", ErrInvalidObjectID, id.name, boundTail(id.v))
		}
	}
	if repoDir == "" {
		return DeltaInputs{}, fmt.Errorf("%w: empty repository dir", ErrGit)
	}

	out, err := git(ctx, repoDir, "rev-list", "--parents", "-n", "1", candidate)
	if err != nil {
		return DeltaInputs{}, err
	}
	fields := strings.Fields(string(out))
	if len(fields) != 3 || fields[0] != candidate {
		return DeltaInputs{}, fmt.Errorf("%w: %s has %d parent(s)", ErrUnexpectedParents, candidate, max(len(fields)-1, 0))
	}
	if fields[1] != priorHead {
		return DeltaInputs{}, fmt.Errorf("%w: first parent %s, want %s", ErrUnexpectedParents, fields[1], priorHead)
	}
	toBase := fields[2]
	if !isFullObjectID(toBase) {
		return DeltaInputs{}, fmt.Errorf("%w: rev-list: malformed parent %q", ErrGit, boundTail(toBase))
	}

	if _, err := git(ctx, repoDir, "merge-base", "--is-ancestor", rootHead, priorHead); err != nil {
		if exitCode(err) == 1 {
			return DeltaInputs{}, fmt.Errorf("%w: %s is not an ancestor of %s", ErrNotAncestor, rootHead, priorHead)
		}
		return DeltaInputs{}, err
	}

	out, err = git(ctx, repoDir, "merge-base", "--all", rootHead, toBase)
	if err != nil {
		if exitCode(err) == 1 && len(bytes.TrimSpace(out)) == 0 {
			return DeltaInputs{}, fmt.Errorf("%w: %s and %s", ErrNoMergeBase, rootHead, toBase)
		}
		return DeltaInputs{}, err
	}
	bases := strings.Fields(string(out))
	switch {
	case len(bases) == 0:
		return DeltaInputs{}, fmt.Errorf("%w: %s and %s", ErrNoMergeBase, rootHead, toBase)
	case len(bases) > 1:
		return DeltaInputs{}, fmt.Errorf("%w: %s and %s have %d", ErrAmbiguousMergeBase, rootHead, toBase, len(bases))
	case !isFullObjectID(bases[0]):
		return DeltaInputs{}, fmt.Errorf("%w: merge-base: malformed base %q", ErrGit, boundTail(bases[0]))
	}
	fromBase := bases[0]

	delta, err := diffNames(ctx, git, repoDir, fromBase, toBase)
	if err != nil {
		return DeltaInputs{}, err
	}
	pr, err := diffNames(ctx, git, repoDir, fromBase, rootHead)
	if err != nil {
		return DeltaInputs{}, err
	}
	merged, err := diffNames(ctx, git, repoDir, toBase, candidate)
	if err != nil {
		return DeltaInputs{}, err
	}
	return DeltaInputs{FromBase: fromBase, ToBase: toBase, DeltaPaths: delta, PRPaths: appendUnique(pr, merged...)}, nil
}

// appendUnique appends each of add not already in list, keeping order.
func appendUnique(list []string, add ...string) []string {
	seen := make(map[string]bool, len(list)+len(add))
	for _, p := range list {
		seen[p] = true
	}
	for _, p := range add {
		if !seen[p] {
			seen[p] = true
			list = append(list, p)
		}
	}
	return list
}

// maxLinkHops bounds symlink resolution; a longer chain is a loop.
const maxLinkHops = 40

// ExpandSymlinkAliases extends in.DeltaPaths and in.PRPaths with every
// symlink, from any of trees (link path -> target, as MaterializeTreeLinks
// returns), whose fully resolved target equals or contains one of their
// paths: a change to the target is a change to the link's path too, which the
// classifier sees only as a path. Resolution is chained and component-wise
// within each tree; a link resolving outside the repository is
// ErrSymlinkEscape, so the verdict is undecidable rather than independent.
func ExpandSymlinkAliases(in DeltaInputs, trees ...map[string]string) (DeltaInputs, error) {
	type alias struct{ link, target string }
	var aliases []alias
	for _, links := range trees {
		for link := range links {
			target, err := resolveTreePath(links, link)
			if err != nil {
				return DeltaInputs{}, err
			}
			aliases = append(aliases, alias{link: link, target: target})
		}
	}
	expand := func(paths []string) []string {
		var add []string
		for _, a := range aliases {
			for _, p := range paths {
				if a.target == "" || p == a.target || strings.HasPrefix(p, a.target+"/") {
					add = append(add, a.link)
					break
				}
			}
		}
		return appendUnique(paths, add...)
	}
	in.DeltaPaths = expand(in.DeltaPaths)
	in.PRPaths = expand(in.PRPaths)
	return in, nil
}

// resolveTreePath resolves p ("" is the repository root) through the tree's
// symlinks one component at a time, as the filesystem would.
func resolveTreePath(links map[string]string, p string) (string, error) {
	pending := strings.Split(p, "/")
	var done []string
	hops := 0
	for len(pending) > 0 {
		c := pending[0]
		pending = pending[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			if len(done) == 0 {
				return "", fmt.Errorf("%w: %q", ErrSymlinkEscape, boundTail(p))
			}
			done = done[:len(done)-1]
			continue
		}
		target, isLink := links[path.Join(append(done, c)...)]
		if !isLink {
			done = append(done, c)
			continue
		}
		if hops++; hops > maxLinkHops || target == "" || strings.HasPrefix(target, "/") {
			return "", fmt.Errorf("%w: %q", ErrSymlinkEscape, boundTail(p))
		}
		pending = append(strings.Split(target, "/"), pending...)
	}
	return strings.Join(done, "/"), nil
}

// diffNames lists the paths changed between two commits. --no-renames makes a
// rename report both its old and new path; --no-ext-diff / --no-textconv keep
// repo-configured diff drivers out (defence in depth: --name-only runs none);
// --no-relative pins root-relative paths against a diff.relative config.
func diffNames(ctx context.Context, git gitRunner, repoDir, from, to string) ([]string, error) {
	out, err := git(ctx, repoDir, "diff", "-z", "--no-renames", "--no-ext-diff", "--no-textconv",
		"--no-relative", "--no-color", "--name-only", from, to)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// MaterializeTree writes the tracked tree of commit into dest (an existing,
// empty directory) from the LOCAL repository at repoDir. It reads `ls-tree -r
// -l` and `cat-file --batch` and writes the bytes itself, so — unlike `git
// archive` (reviewsandbox.ExportTree) — it ignores export-ignore and keeps
// symlinks, runs no filter/textconv driver, and never touches the
// repository's index or work tree.
//
// Containment: every write goes through an os.Root on dest; before each
// directory or entry is created, every existing parent component is Lstat'ed
// and a symlink one is refused (ErrIncompleteTree), so nothing is ever
// created THROUGH a link, even on a case-folding filesystem where `a/x`
// resolves through a symlink `A`. Symlinks are created after every regular
// file and files are created O_EXCL, so a case-folding collision fails.
//
// Bound: the entry count and the total blob bytes (symlink blobs included)
// are checked against reviewsandbox.DefaultLimits BEFORE anything is
// written; an oversized tree is ErrIncompleteTree. A submodule gitlink is
// ErrIncompleteTree and a hostile path ErrUnsafeTreePath. Any of these means
// the workspace must not be classified.
func MaterializeTree(ctx context.Context, repoDir, commit, dest string) error {
	_, err := MaterializeTreeLinks(ctx, repoDir, commit, dest)
	return err
}

// MaterializeTreeLinks is MaterializeTree that also returns the tree's
// symlinks (path -> target), the input ExpandSymlinkAliases reads.
func MaterializeTreeLinks(ctx context.Context, repoDir, commit, dest string) (map[string]string, error) {
	return materializeTree(ctx, repoDir, commit, dest, reviewsandbox.DefaultLimits())
}

func materializeTree(ctx context.Context, repoDir, commit, dest string, limits reviewsandbox.Limits) (map[string]string, error) {
	if !isFullObjectID(commit) {
		return nil, fmt.Errorf("%w: commit %q", ErrInvalidObjectID, boundTail(commit))
	}
	if repoDir == "" {
		return nil, fmt.Errorf("%w: empty repository dir", ErrGit)
	}
	out, err := runGit(ctx, repoDir, "ls-tree", "-r", "-z", "-l", "--full-tree", commit)
	if err != nil {
		return nil, err
	}
	entries, err := parseLsTree(out)
	if err != nil {
		return nil, err
	}
	if err := checkTreeLimits(entries, limits); err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	var stdin strings.Builder
	for _, e := range entries {
		stdin.WriteString(e.oid)
		stdin.WriteByte('\n')
	}

	cctx, cancel := context.WithTimeout(ctx, DefaultGitTimeout)
	defer cancel()
	cmd := gitCommand(cctx, repoDir, "cat-file", "--batch")
	cmd.Stdin = strings.NewReader(stdin.String())
	stderr := &tailBuffer{max: gitStderrTail}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("%w: cat-file: %w", ErrGit, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%w: cat-file: %w", ErrGit, err)
	}
	links, werr := writeBatch(bufio.NewReader(stdout), entries, dest)
	if werr != nil {
		cancel()
	}
	_, _ = io.Copy(io.Discard, stdout)
	if err := cmd.Wait(); err != nil && werr == nil {
		werr = gitError(ctx, cctx, []string{"cat-file"}, err, stderr)
	}
	if werr != nil {
		return nil, werr
	}
	return links, nil
}

type treeEntry struct {
	mode, oid, path string
	size            int64
}

// checkTreeLimits refuses a tree whose entry count or total blob bytes
// (symlink blobs included) exceeds limits, before anything is written.
func checkTreeLimits(entries []treeEntry, limits reviewsandbox.Limits) error {
	d := reviewsandbox.DefaultLimits()
	if limits.MaxFiles <= 0 {
		limits.MaxFiles = d.MaxFiles
	}
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = d.MaxBytes
	}
	if len(entries) > limits.MaxFiles {
		return fmt.Errorf("%w: %d entries exceed the limit of %d", ErrIncompleteTree, len(entries), limits.MaxFiles)
	}
	var total int64
	for _, e := range entries {
		total += e.size
		if total > limits.MaxBytes {
			return fmt.Errorf("%w: blob bytes exceed the limit of %d", ErrIncompleteTree, limits.MaxBytes)
		}
	}
	return nil
}

// parseLsTree decodes `ls-tree -r -l -z` records
// ("<mode> <type> <oid> <size>\t<path>").
func parseLsTree(out []byte) ([]treeEntry, error) {
	var entries []treeEntry
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		meta, p, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 4 {
			return nil, fmt.Errorf("%w: ls-tree: malformed record %q", ErrGit, boundTail(rec))
		}
		if err := checkTreePath(p); err != nil {
			return nil, err
		}
		switch {
		case f[1] != "blob" || (f[0] != "100644" && f[0] != "100755" && f[0] != "120000"):
			// A submodule gitlink (160000 commit) lands here.
			return nil, fmt.Errorf("%w: %q has mode %s type %s", ErrIncompleteTree, boundTail(p), f[0], f[1])
		case !isFullObjectID(f[2]):
			return nil, fmt.Errorf("%w: ls-tree: malformed oid %q", ErrGit, boundTail(f[2]))
		}
		size, err := strconv.ParseInt(f[3], 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("%w: ls-tree: bad size %q", ErrGit, boundTail(f[3]))
		}
		entries = append(entries, treeEntry{mode: f[0], oid: f[2], path: p, size: size})
	}
	return entries, nil
}

// checkTreePath refuses a path that could leave dest or land in a .git dir.
func checkTreePath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return fmt.Errorf("%w: %q", ErrUnsafeTreePath, boundTail(p))
	}
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." || c == ".." || strings.EqualFold(c, ".git") {
			return fmt.Errorf("%w: %q", ErrUnsafeTreePath, boundTail(p))
		}
	}
	return nil
}

// writeBatch reads one `cat-file --batch` record per entry, in order, writing
// regular files immediately and symlinks after the last file, every write
// through an os.Root on dest. It returns the symlinks (path -> target).
func writeBatch(r *bufio.Reader, entries []treeEntry, dest string) (map[string]string, error) {
	root, err := os.OpenRoot(dest)
	if err != nil {
		return nil, fmt.Errorf("basedelta: materialize: %w", err)
	}
	defer func() { _ = root.Close() }()
	links := map[string]string{}
	var order []string
	for _, e := range entries {
		header, err := r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("%w: cat-file: truncated output at %q", ErrGit, boundTail(e.path))
		}
		f := strings.Fields(header)
		if len(f) != 3 || f[0] != e.oid || f[1] != "blob" {
			return nil, fmt.Errorf("%w: cat-file: unexpected header %q for %q", ErrGit, boundTail(header), boundTail(e.path))
		}
		size, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("%w: cat-file: bad size %q", ErrGit, boundTail(f[2]))
		}
		if e.mode == "120000" {
			var target bytes.Buffer
			if _, err := io.CopyN(&target, r, size); err != nil {
				return nil, fmt.Errorf("%w: cat-file: %w", ErrGit, err)
			}
			links[e.path] = target.String()
			order = append(order, e.path)
		} else if err := writeBlob(root, r, size, e.path, e.mode == "100755"); err != nil {
			return nil, err
		}
		if b, err := r.ReadByte(); err != nil || b != '\n' {
			return nil, fmt.Errorf("%w: cat-file: missing record terminator after %q", ErrGit, boundTail(e.path))
		}
	}
	for _, p := range order {
		if err := mkdirParents(root, p); err != nil {
			return nil, err
		}
		if err := root.Symlink(links[p], filepath.FromSlash(p)); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrIncompleteTree, err)
		}
	}
	return links, nil
}

// mkdirParents creates rel's parent directories inside root one component at
// a time. Each component is Lstat'ed first: a symlink is refused
// (ErrIncompleteTree), so no directory and no entry is ever created through a
// link — not one planted in dest, and not one a case-folding filesystem
// aliases (`a` resolving to a symlink `A`).
func mkdirParents(root *os.Root, rel string) error {
	dir := path.Dir(rel)
	if dir == "." {
		return nil
	}
	cur := ""
	for _, c := range strings.Split(dir, "/") {
		cur = path.Join(cur, c)
		fi, err := root.Lstat(filepath.FromSlash(cur))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := root.Mkdir(filepath.FromSlash(cur), 0o755); err != nil {
				return fmt.Errorf("basedelta: materialize: %w", err)
			}
		case err != nil:
			return fmt.Errorf("basedelta: materialize: %w", err)
		case fi.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("%w: parent %q of %q is a symlink", ErrIncompleteTree, boundTail(cur), boundTail(rel))
		case !fi.IsDir():
			return fmt.Errorf("basedelta: materialize: parent %q of %q is not a directory", boundTail(cur), boundTail(rel))
		}
	}
	return nil
}

func writeBlob(root *os.Root, r io.Reader, size int64, rel string, executable bool) error {
	if err := mkdirParents(root, rel); err != nil {
		return err
	}
	perm := os.FileMode(0o644)
	if executable {
		perm = 0o755
	}
	f, err := root.OpenFile(filepath.FromSlash(rel), os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrIncompleteTree, err)
	}
	_, cerr := io.CopyN(f, r, size)
	if err := f.Close(); cerr == nil {
		cerr = err
	}
	if cerr != nil {
		return fmt.Errorf("basedelta: materialize %s: %w", rel, cerr)
	}
	return nil
}

// gitEnv scrubs the child environment to reviewsandbox.BaseAllow (no GIT_DIR,
// GIT_INDEX_FILE, token or credential variable survives), then pins: no
// prompt, no system or global config, no replace refs (a local refs/replace
// could forge parents), no optional locks.
func gitEnv() []string {
	return append(reviewsandbox.Env(os.Environ(), reviewsandbox.BaseAllow, nil),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_OPTIONAL_LOCKS=0",
	)
}

// gitCommand builds one local-only, process-group-hardened git child.
func gitCommand(ctx context.Context, repoDir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoDir}, args...)...)
	cmd.Env = gitEnv()
	procgroup.Harden(cmd, killGrace)
	return cmd
}

// runGit runs one git child bounded by DefaultGitTimeout. On a non-zero exit
// it still returns stdout beside the error (merge-base --all exits 1 with no
// output when there is no base).
func runGit(ctx context.Context, repoDir string, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, DefaultGitTimeout)
	defer cancel()
	cmd := gitCommand(cctx, repoDir, args...)
	var stdout bytes.Buffer
	stderr := &tailBuffer{max: gitStderrTail}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), gitError(ctx, cctx, args, err, stderr)
	}
	return stdout.Bytes(), nil
}

// gitExitError keeps the exit code reachable for exitCode.
type gitExitError struct {
	code int
	msg  string
}

func (e *gitExitError) Error() string { return e.msg }

// gitError wraps ErrGit with the subcommand and a single-line stderr tail of
// at most gitStderrTail bytes. A done context wraps the context error too.
func gitError(parent, child context.Context, args []string, err error, stderr *tailBuffer) error {
	sub := args[0]
	if cerr := parent.Err(); cerr != nil {
		return fmt.Errorf("%w: git %s: %w", ErrGit, sub, cerr)
	}
	if cerr := child.Err(); cerr != nil {
		return fmt.Errorf("%w: git %s: timed out after %s: %w", ErrGit, sub, DefaultGitTimeout, cerr)
	}
	tail := strings.Join(strings.Fields(stderr.String()), " ")
	if tail == "" {
		tail = "(no stderr)"
	}
	code := -1
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	return fmt.Errorf("%w: git %s: %w", ErrGit, sub, &gitExitError{code: code, msg: err.Error() + ": " + tail})
}

// exitCode returns a git child's exit status, or -1.
func exitCode(err error) int {
	var ge *gitExitError
	if errors.As(err, &ge) {
		return ge.code
	}
	return -1
}

// isFullObjectID reports a lowercase 40-hex (SHA-1) or 64-hex (SHA-256) id.
func isFullObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// boundTail caps an untrusted value quoted into an error.
func boundTail(s string) string {
	if len(s) > 64 {
		return s[:64] + "…"
	}
	return s
}
