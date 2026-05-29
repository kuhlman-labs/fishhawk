// Package gitdiff produces a constraint.Diff from a git working
// tree by running `git diff --cached --name-status <baseRef>`. It's
// a thin shim around os/exec so the constraint package itself
// stays pure and testable without touching real git state.
//
// `--cached <base>` compares <base>'s tree to the index — so the
// caller is responsible for staging everything the diff should
// see (typically `git add -A` after the agent finishes). Staging
// is what lets a new (untracked) file appear in the diff at all;
// `git diff <base>` against the working tree silently skips
// untracked files. The runner stages with `git add -A` in
// `computeAndEmitDiff` (#296 / E16 demo loop). Pre-#296 the form
// was `<base>...HEAD` which only saw COMMITTED changes; agents
// leaving edits unstaged produced empty diff events that silently
// failed every `tests_added_or_updated` and `max_files_changed`
// constraint at the backend's policy re-evaluation step.
package gitdiff

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/kuhlman-labs/fishhawk/runner/internal/constraint"
)

// maxPatchBytes caps the unified-diff hunk text captured by RunPatch
// into the git_diff event's patch field. 256 KiB stays well under
// bundle.ReadEvents' 4 MiB per-line scanner buffer (so a capped patch
// can never trip the line-length limit) and the 64 MiB MaxBundleBytes.
// The patch is content for the implement-review prompt only; it is NOT
// read by the policy engine, so truncation never affects enforcement.
const maxPatchBytes = 256 * 1024

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
// git working directory the agent edited. The caller is expected
// to have staged any agent-produced changes (`git add -A`) before
// calling Run — `--cached <base>` compares <base> to the index,
// which is the only form that reliably captures both modified
// and freshly-created files. See the package doc for why.
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

	args := []string{"diff", "--cached", "--name-status", "-z", baseRef}
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

// RunPatch executes `git diff --cached <baseRef>` WITHOUT
// --name-status to produce the full unified-diff (hunk) text, mirroring
// Run's binary/Cmd overridability for tests. It diffs the same staged
// index Run sees, so the returned hunks describe exactly the files the
// name-status list enumerates (the caller stages with `git add -A`
// before calling either — see the package doc).
//
// The result is the content the implement-review agent inspects for
// content-level review (plan adherence, regressions). It is additive
// trace payload only and is deliberately never read by the policy
// engine — ChangedFiles remains the sole constraint input.
//
// The patch is capped at maxPatchBytes: when the raw output exceeds the
// cap, RunPatch truncates at the cap, appends a clear marker line, and
// returns truncated=true. Callers degrade gracefully on error (emit the
// git_diff event without a patch) since name-status is the load-bearing
// input.
func (r *Runner) RunPatch(ctx context.Context, baseRef, repoDir string) (patch string, truncated bool, err error) {
	if baseRef == "" {
		return "", false, fmt.Errorf("gitdiff: baseRef required")
	}
	if repoDir == "" {
		return "", false, fmt.Errorf("gitdiff: repoDir required")
	}

	binary := r.Binary
	if binary == "" {
		binary = "git"
	}
	cmdFn := r.Cmd
	if cmdFn == nil {
		cmdFn = exec.CommandContext
	}

	args := []string{"diff", "--cached", baseRef}
	cmd := cmdFn(ctx, binary, args...)
	cmd.Dir = repoDir

	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return "", false, fmt.Errorf("gitdiff: patch: %v: %s",
				err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", false, fmt.Errorf("gitdiff: patch: %w", err)
	}

	if len(out) > maxPatchBytes {
		marker := fmt.Sprintf("\n[patch truncated: %d bytes total, capped at %d KiB]\n",
			len(out), maxPatchBytes/1024)
		return string(out[:maxPatchBytes]) + marker, true, nil
	}
	return string(out), false, nil
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
