// Package diffcov measures how much of a stage's ADDED source lines a
// coverage report shows as executed (workflow-v1.6 `diff_coverage`,
// ADR-059 / #1888).
//
// It is deliberately PURE and injectable: the git invocation is a function
// field, so every parser and the measurement itself test without a real
// repository. The runner owns execution and evidence emission; the backend
// owns the verdict.
//
// Long-form contract: runner/internal/diffcov/README.md.
package diffcov

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ErrEmptyBaseRef is returned when the base ref handed to ChangedLines is
// empty. An empty ref must NEVER reach git: `git diff ...HEAD` with an
// empty argument does not mean "the default branch", it means a malformed
// revision range whose failure would be reported as an opaque git error
// rather than the real cause. The runner resolves the workflow's omitted
// `base_ref` to the run's base branch BEFORE calling here; this guard is
// the fail-closed backstop for a resolution that nonetheless yielded
// nothing (#1888 condition 5).
var ErrEmptyBaseRef = errors.New("diffcov: base ref is empty (unresolved base branch)")

// ChangedFiles maps a repo-relative path to the set of line numbers this
// stage ADDED, as `git diff -U0 <base>...HEAD` reports them.
type ChangedFiles map[string]map[int]bool

// GitRunner runs a git command and returns its stdout. Injected so the
// diff parsing tests without a real repository.
type GitRunner func(ctx context.Context, args ...string) (string, error)

// ExecGit is the production GitRunner: it shells out to git in repoDir.
// It carries no bounded-exec contract of its own because it runs GIT, not
// the untrusted customer command — the customer command goes through the
// runner's existing gate-exec path.
//
// Exit status 1 from `git diff` is NOT an error: git-diff(1) uses it to
// mean "differences were found", which is the normal outcome under
// --no-index (and under --exit-code). Only status >= 2 is a real failure.
// Treating 1 as fatal would make every untracked-file diff below fail.
func ExecGit(repoDir string) GitRunner {
	return func(ctx context.Context, args ...string) (string, error) {
		full := append([]string{"-C", repoDir}, args...)
		out, err := exec.CommandContext(ctx, "git", full...).Output()
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				if args[0] == "diff" && exitErr.ExitCode() == 1 {
					return string(out), nil
				}
				return "", fmt.Errorf("git %s: %s", strings.Join(args, " "),
					strings.TrimSpace(string(exitErr.Stderr)))
			}
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return string(out), nil
	}
}

// ChangedLines returns the lines this stage added relative to baseRef.
//
// The diff is taken MERGE-BASE → WORK TREE, the same framing the repo's
// own patch-coverage gate uses (scripts/check-coverage.py, ADR-059):
//
//   - The MERGE BASE, resolved explicitly rather than via the `A...B`
//     three-dot shorthand, keeps commits that landed on the base branch
//     AFTER the run branched out of the denominator — they are not this
//     stage's new lines. (`git diff A...B` is defined as
//     `git diff $(git merge-base A B) B`, git-diff(1).)
//   - The WORK TREE, not HEAD, because the coverage report describes the
//     tree the coverage command just executed against. Diffing HEAD
//     instead would attribute coverage and lines to two different
//     snapshots, and the measurement would be meaningless.
//
// `-U0` makes every hunk header describe exactly the changed lines with
// no context, which is what the added-line set needs.
//
// An empty baseRef returns ErrEmptyBaseRef WITHOUT invoking git: the
// caller resolves the workflow's omitted `base_ref` to the run's base
// branch first, and a resolution that yielded nothing must be reported
// as a named failure rather than shelling out with an empty argument.
func ChangedLines(ctx context.Context, git GitRunner, baseRef string) (ChangedFiles, error) {
	if strings.TrimSpace(baseRef) == "" {
		return nil, ErrEmptyBaseRef
	}
	mb, err := git(ctx, "merge-base", baseRef, "HEAD")
	if err != nil {
		return nil, err
	}
	mb = strings.TrimSpace(mb)
	if mb == "" {
		return nil, fmt.Errorf("diffcov: no merge base between %q and HEAD", baseRef)
	}
	out, err := git(ctx, "diff", "-U0", "--no-color", mb)
	if err != nil {
		return nil, err
	}
	changed, err := ParseUnifiedDiff(out)
	if err != nil {
		return nil, err
	}

	// `git diff` sees only TRACKED files, so a never-`git add`ed new file
	// would contribute zero added lines and bypass the gate outright —
	// exactly the blind spot the repo's own patch-coverage gate enumerates
	// around (ADR-059). Fold every line of each untracked file in.
	//
	// -z + NUL splitting is required: git C-quotes a path carrying a
	// quote, backslash, control character, or non-ASCII byte, and a
	// newline inside a name would split one path into two.
	others, err := git(ctx, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	for _, p := range strings.Split(others, "\x00") {
		if p == "" {
			continue
		}
		// --no-index against /dev/null renders the whole file as added
		// lines in the unified format ParseUnifiedDiff already reads.
		d, derr := git(ctx, "diff", "-U0", "--no-color", "--no-index", "--", os.DevNull, p)
		if derr != nil {
			return nil, derr
		}
		untracked, perr := ParseUnifiedDiff(d)
		if perr != nil {
			return nil, perr
		}
		for path, ls := range untracked {
			if _, ok := changed[path]; !ok {
				changed[path] = map[int]bool{}
			}
			for ln := range ls {
				changed[path][ln] = true
			}
		}
	}
	return changed, nil
}

// ParseUnifiedDiff extracts the added-line set from a `git diff -U0`
// output. Pure, so every hunk-header shape is a table case.
//
// Handled shapes:
//
//   - `+++ b/<path>` names the file; a `+++ /dev/null` (the file was
//     DELETED) closes the current file with no added lines.
//   - `@@ -a,b +c,d @@` is the general hunk header; the `,d` count is
//     OMITTED when it equals 1, so `@@ -0,0 +7 @@` adds exactly line 7.
//     Mishandling the omitted count silently drops added lines.
//   - `+c,0` is a pure-DELETION hunk: zero added lines.
//   - A path containing spaces survives because the `+++ b/` prefix is
//     stripped positionally and the remainder is taken whole (only the
//     trailing tab-separated metadata git may append is cut).
//   - A C-quoted path (git quotes a path with a quote, backslash,
//     control character, or non-ASCII byte) is decoded, so a
//     non-ASCII-named file is attributed to its real name rather than to
//     a literal `"src/caf\303\251.go"` key that could never match a
//     coverage report entry.
func ParseUnifiedDiff(diff string) (ChangedFiles, error) {
	out := ChangedFiles{}
	current := ""
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ "):
			target := strings.TrimPrefix(line, "+++ ")
			// git appends "\t<timestamp>" in some configurations; the
			// path itself never contains a raw tab (it would be quoted).
			if i := strings.IndexByte(target, '\t'); i >= 0 {
				target = target[:i]
			}
			if target == "/dev/null" {
				// Deleted file: no added lines to attribute.
				current = ""
				continue
			}
			p, err := decodeDiffPath(strings.TrimPrefix(target, "b/"))
			if err != nil {
				return nil, err
			}
			current = p
			if _, ok := out[current]; !ok {
				out[current] = map[int]bool{}
			}
		case strings.HasPrefix(line, "@@"):
			if current == "" {
				// A hunk before any +++ header: not something git emits;
				// skip rather than inventing an attribution.
				continue
			}
			start, count, err := parseHunkHeader(line)
			if err != nil {
				return nil, err
			}
			for i := 0; i < count; i++ {
				out[current][start+i] = true
			}
		}
	}
	// Drop files that ended up with no added lines (pure deletions and
	// mode-only changes) so the caller's file set means "files with new
	// lines" rather than "files git mentioned".
	for p, lines := range out {
		if len(lines) == 0 {
			delete(out, p)
		}
	}
	return out, nil
}

// parseHunkHeader parses the `+c,d` half of `@@ -a,b +c,d @@`, returning
// the first added line and how many lines follow it. An omitted `,d`
// means a count of 1 (git-diff(1) unified format).
func parseHunkHeader(line string) (start, count int, err error) {
	i := strings.IndexByte(line, '+')
	if i < 0 {
		return 0, 0, fmt.Errorf("diffcov: hunk header %q has no '+' range", line)
	}
	rest := line[i+1:]
	if j := strings.IndexAny(rest, " \t"); j >= 0 {
		rest = rest[:j]
	}
	startStr, countStr := rest, ""
	if c := strings.IndexByte(rest, ','); c >= 0 {
		startStr, countStr = rest[:c], rest[c+1:]
	}
	start, err = strconv.Atoi(startStr)
	if err != nil {
		return 0, 0, fmt.Errorf("diffcov: hunk header %q has a non-numeric start", line)
	}
	count = 1
	if countStr != "" {
		count, err = strconv.Atoi(countStr)
		if err != nil {
			return 0, 0, fmt.Errorf("diffcov: hunk header %q has a non-numeric count", line)
		}
	}
	if count < 0 {
		return 0, 0, fmt.Errorf("diffcov: hunk header %q has a negative count", line)
	}
	if count == 0 {
		// Pure-deletion hunk: nothing added. start is the line BEFORE
		// which the deletion occurred and must not be attributed.
		return start, 0, nil
	}
	return start, count, nil
}

// decodeDiffPath decodes a git C-quoted path ("...\303\251...") back to
// its real bytes. An unquoted path is returned verbatim.
func decodeDiffPath(p string) (string, error) {
	if len(p) < 2 || p[0] != '"' || p[len(p)-1] != '"' {
		return p, nil
	}
	body := p[1 : len(p)-1]
	var b strings.Builder
	for i := 0; i < len(body); {
		if body[i] != '\\' {
			b.WriteByte(body[i])
			i++
			continue
		}
		i++
		if i >= len(body) {
			return "", fmt.Errorf("diffcov: path %s ends in a dangling escape", p)
		}
		switch c := body[i]; c {
		case 'n':
			b.WriteByte('\n')
			i++
		case 't':
			b.WriteByte('\t')
			i++
		case 'r':
			b.WriteByte('\r')
			i++
		case '"', '\\':
			b.WriteByte(c)
			i++
		default:
			// Three-digit octal byte.
			if i+3 > len(body) {
				return "", fmt.Errorf("diffcov: path %s has a truncated octal escape", p)
			}
			v, err := strconv.ParseUint(body[i:i+3], 8, 8)
			if err != nil {
				return "", fmt.Errorf("diffcov: path %s has an invalid escape", p)
			}
			b.WriteByte(byte(v))
			i += 3
		}
	}
	return b.String(), nil
}

// Result is one diff-coverage measurement.
type Result struct {
	// NewLines is the number of added lines that the coverage report
	// could speak to at all — i.e. added lines in files the report
	// measured. Added lines in a file the report never mentions (a
	// README, a generated file, a language the tool does not cover) are
	// EXCLUDED from the denominator: counting them would make every
	// docs-touching stage fail an opted-in gate, which is the false-RED
	// failure mode this measurement exists to avoid.
	NewLines int
	// CoveredNewLines is how many of those had a non-zero hit count.
	CoveredNewLines int
	// Percent is CoveredNewLines*100/NewLines, or 0 when NewLines is 0.
	Percent float64
	// UncoveredFiles names the files carrying uncovered new lines, sorted
	// for determinism.
	UncoveredFiles []string
	// UnnormalizablePaths names report paths that could not be resolved
	// into the repository (an absolute path outside repoDir, or one that
	// escapes via `..`). Surfaced EXPLICITLY in the evidence reason
	// rather than silently counted as uncovered — a path the measurement
	// could not place is a fact the operator needs, not a zero.
	UnnormalizablePaths []string
}

// Measure intersects the report's per-line coverage with the stage's
// added lines and returns the new-line coverage.
//
// PATH NORMALIZATION is the load-bearing part (#1888 condition 2).
// Coverage producers spell `SF:` paths inconsistently — absolute
// (/build/src/app.go), `./`-prefixed (./src/app.go), or carrying
// redundant separators (src//app.go) — while the diff parser yields
// clean repo-relative paths. Intersecting the two key sets verbatim would
// classify covered new lines as uncovered and fail EVERY opted-in
// customer run with a false RED. Both sides are therefore normalized to
// clean, slash-separated, repo-relative form before intersection.
//
// repoDir is the absolute path of the checkout the coverage command ran
// in; it is what an absolute SF path is made relative to. When repoDir is
// empty, absolute report paths cannot be placed and are reported as
// unnormalizable rather than guessed at.
func Measure(repoDir string, changed ChangedFiles, coverage Coverage) Result {
	var res Result

	// Normalize the coverage side, folding any collisions (two spellings
	// of one file) by summing hits.
	norm := map[string]FileCoverage{}
	var bad []string
	for p, lines := range coverage {
		n, err := NormalizePath(repoDir, p)
		if err != nil {
			bad = append(bad, p)
			continue
		}
		if _, ok := norm[n]; !ok {
			norm[n] = FileCoverage{}
		}
		for ln, hits := range lines {
			norm[n][ln] += hits
		}
	}
	sort.Strings(bad)
	res.UnnormalizablePaths = bad

	uncovered := map[string]bool{}
	for p, lines := range changed {
		n, err := NormalizePath(repoDir, p)
		if err != nil {
			// A diff path that will not normalize is not measurable; it
			// is reported alongside the report-side ones rather than
			// silently counted either way.
			res.UnnormalizablePaths = append(res.UnnormalizablePaths, p)
			continue
		}
		fileCov, measured := norm[n]
		if !measured {
			// The report never mentions this file — out of denominator.
			continue
		}
		for ln := range lines {
			hits, known := fileCov[ln]
			if !known {
				// The report measured the file but not this line (a
				// blank line, a comment, a brace): not a coverable
				// statement, so out of denominator.
				continue
			}
			res.NewLines++
			if hits > 0 {
				res.CoveredNewLines++
			} else {
				uncovered[n] = true
			}
		}
	}
	sort.Strings(res.UnnormalizablePaths)

	for f := range uncovered {
		res.UncoveredFiles = append(res.UncoveredFiles, f)
	}
	sort.Strings(res.UncoveredFiles)

	if res.NewLines > 0 {
		res.Percent = float64(res.CoveredNewLines) * 100 / float64(res.NewLines)
	}
	return res
}

// NormalizePath resolves p into a clean, slash-separated, repo-relative
// path. It accepts the spellings coverage producers actually emit:
//
//   - "src/app.go"        already relative — cleaned only
//   - "./src/app.go"      leading "./" stripped by Clean
//   - "src//app.go"       redundant separator collapsed by Clean
//   - "/repo/src/app.go"  absolute — made relative to repoDir
//
// It returns an error — never a guess — when p cannot be placed inside
// the repository: an absolute path outside repoDir, an absolute path with
// no repoDir to resolve against, or a relative path that escapes upward
// via "..". Callers surface those explicitly in the evidence reason.
func NormalizePath(repoDir, p string) (string, error) {
	if p == "" {
		return "", errors.New("empty path")
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
		if repoDir == "" {
			return "", fmt.Errorf("absolute path %q with no repo root to resolve against", p)
		}
		rel, err := relInside(repoDir, p)
		if err != nil {
			// A coverage tool running INSIDE the checkout reports the
			// symlink-resolved path (on macOS /var/... resolves to
			// /private/var/...), which would not be relative to the
			// repoDir spelling the runner holds. Retry against the
			// resolved root before declaring the path unplaceable.
			resolved, rerr := filepath.EvalSymlinks(repoDir)
			if rerr != nil {
				return "", err
			}
			rel, err = relInside(resolved, p)
			if err != nil {
				return "", err
			}
		}
		p = rel
	}
	clean := path.Clean(filepath.ToSlash(p))
	if clean == ".." || strings.HasPrefix(clean, "../") || clean == "." {
		return "", fmt.Errorf("path %q resolves outside the repository", p)
	}
	return clean, nil
}

// relInside returns p relative to root, erroring when p is not actually
// under root. filepath.Rel happily produces a "../.." answer for a path
// outside root, so the escape check is what makes this a containment
// test rather than a string transform.
func relInside(root, p string) (string, error) {
	rel, err := filepath.Rel(root, filepath.Clean(p))
	if err != nil {
		return "", fmt.Errorf("absolute path %q is not under the repo root %q: %w", p, root, err)
	}
	slash := filepath.ToSlash(rel)
	if slash == ".." || strings.HasPrefix(slash, "../") {
		return "", fmt.Errorf("absolute path %q resolves outside the repo root %q", p, root)
	}
	return slash, nil
}
