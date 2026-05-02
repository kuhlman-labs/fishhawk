// Package gitdiff produces a constraint.Diff from a git working
// tree by running `git diff --name-status <baseRef>...HEAD`. It's
// a thin shim around os/exec so the constraint package itself
// stays pure and testable without touching real git state.
//
// The runner calls this after the agent finishes editing files
// (typically: agent commits to a branch, runner runs gitdiff
// against the merge-base with the workflow base ref, evaluates
// constraints).
package gitdiff

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/kuhlman-labs/fishhawk/runner/internal/constraint"
)

// Runner executes `git` to produce a Diff. Cmd defaults to
// exec.CommandContext but is overridable for tests so we can drive
// the parser without a real git working tree.
type Runner struct {
	// Binary is the git executable path. Empty defaults to "git".
	Binary string

	// Cmd builds the *exec.Cmd. nil defaults to exec.CommandContext.
	Cmd func(ctx context.Context, name string, args ...string) *exec.Cmd
}

// Run executes the diff command and parses the output.
//
// baseRef is the ref the stage's changes should be measured
// against (typically the workflow base branch); repoDir is the
// git working directory the agent edited.
//
// The form `<base>...HEAD` (three-dot) gives the diff at the
// merge base, which is what the workflow author intends — changes
// the stage actually introduced, not unrelated commits on the base
// branch.
func (r *Runner) Run(ctx context.Context, baseRef, repoDir string) (constraint.Diff, error) {
	if baseRef == "" {
		return constraint.Diff{}, fmt.Errorf("gitdiff: baseRef required")
	}
	if repoDir == "" {
		return constraint.Diff{}, fmt.Errorf("gitdiff: repoDir required")
	}

	binary := r.Binary
	if binary == "" {
		binary = "git"
	}
	cmdFn := r.Cmd
	if cmdFn == nil {
		cmdFn = exec.CommandContext
	}

	args := []string{"diff", "--name-status", "-z", fmt.Sprintf("%s...HEAD", baseRef)}
	cmd := cmdFn(ctx, binary, args...)
	cmd.Dir = repoDir

	out, err := cmd.Output()
	if err != nil {
		// Surface stderr if available so a typo in baseRef or a
		// missing repoDir produces an actionable error rather
		// than a generic exit-status message.
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return constraint.Diff{}, fmt.Errorf("gitdiff: %v: %s",
				err, strings.TrimSpace(string(ee.Stderr)))
		}
		return constraint.Diff{}, fmt.Errorf("gitdiff: %w", err)
	}

	diff, perr := Parse(out)
	if perr != nil {
		return constraint.Diff{}, fmt.Errorf("gitdiff: parse: %w", perr)
	}
	return diff, nil
}

// Parse interprets the NUL-separated output of `git diff
// --name-status -z`. Each entry is (status, path) for ordinary
// changes; renames and copies have a third field (oldPath).
//
// We use the -z (NUL-terminated) form to dodge filenames with
// special characters; the same parser handles the renamed/copied
// case by consuming three fields when status is R or C.
func Parse(raw []byte) (constraint.Diff, error) {
	var d constraint.Diff
	if len(raw) == 0 {
		return d, nil
	}

	// Build a token list; a NUL-terminated stream yields N tokens
	// where consecutive NULs would imply an empty token. -z output
	// from git always emits a status, then a path, then for R/C a
	// further oldPath. We don't see consecutive NULs in practice;
	// guard with a bounded loop anyway.
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	scanner.Split(splitNULs)

	for scanner.Scan() {
		status := scanner.Text()
		if status == "" {
			continue
		}
		// Status field for renames is "R<score>" (e.g. R100);
		// strip the trailing digits for the comparison below but
		// preserve the leading letter as the canonical status.
		statusLetter := status[:1]

		if !scanner.Scan() {
			return constraint.Diff{}, fmt.Errorf("missing path after status %q", status)
		}
		path := scanner.Text()

		if statusLetter == "R" || statusLetter == "C" {
			// Old path follows; we record the new path (the
			// destination) since constraints look at the resulting
			// tree.
			if !scanner.Scan() {
				return constraint.Diff{},
					fmt.Errorf("missing destination path after rename/copy from %q", path)
			}
			newPath := scanner.Text()
			d.ChangedFiles = append(d.ChangedFiles, constraint.ChangedFile{
				Path:   newPath,
				Status: constraint.Status(statusLetter),
			})
			continue
		}

		d.ChangedFiles = append(d.ChangedFiles, constraint.ChangedFile{
			Path:   path,
			Status: constraint.Status(statusLetter),
		})
	}
	if err := scanner.Err(); err != nil {
		return constraint.Diff{}, fmt.Errorf("scan: %w", err)
	}
	return d, nil
}

// splitNULs is a bufio.SplitFunc that splits on NUL bytes. The
// stdlib doesn't ship one; this keeps the parser dependency-free.
func splitNULs(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i, b := range data {
		if b == 0 {
			return i + 1, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil // request more data
}
