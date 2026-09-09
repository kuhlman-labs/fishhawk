package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// autoformatBinary is the formatter the absorb invokes. It is a package-level
// var so tests can substitute a stub script; production never reassigns it.
//
// `golangci-lint fmt` is the right formatter here rather than raw
// gofmt/goimports binaries: it is already a hard requirement of this repo's
// verify gate (scripts/test's _require_golangci_lint fails closed), it applies
// exactly the formatter set and settings declared in .golangci.yml (including
// goimports local-prefixes), and it runs correctly from the repository root
// even though this workspace has no root go.mod.
var autoformatBinary = "golangci-lint"

// formatOnlyLinters is the exact set of linter names whose findings the absorb
// can fix by running the formatter. Any other bullet in a summary block means
// a human/agent fix is required, so the classifier refuses.
var formatOnlyLinters = map[string]bool{
	"gofmt":     true,
	"goimports": true,
}

var (
	// lintIssueTotalRe matches golangci-lint v2's per-run summary header, e.g.
	// `2 issues:`. The passing rendering is `0 issues.` (a PERIOD), which does
	// not match — a clean run opens no block.
	lintIssueTotalRe = regexp.MustCompile(`^\s*(\d+) issues?:\s*$`)
	// lintLinterBulletRe matches one summary bullet, e.g. `* gofmt: 1`.
	lintLinterBulletRe = regexp.MustCompile(`^\s*\* ([a-z0-9][a-z0-9_-]*): (\d+)\s*$`)
)

// isFormatOnlyLintFailure reports whether a FAILED verify's output carries
// golangci-lint findings that are EXCLUSIVELY gofmt/goimports formatting
// findings — the one failure class a formatter can fix without an agent.
//
// It parses golangci-lint v2's deterministic per-module summary block: a
// `N issues:` header followed by one or more `* <linter>: <count>` bullets.
// EVERY such block in the output is collected, because `scripts/test lint`
// iterates the workspace's modules and each failing module prints its own
// block. Verified against golangci-lint v2.13.2 with this repo's
// .golangci.yml: a gofmt-only failure renders `1 issues:` / `* gofmt: 1`, a
// mixed formatter failure renders `2 issues:` / `* gofmt: 1` /
// `* goimports: 1`, and adding a revive finding adds a `* revive: 1` bullet.
//
// It returns true only when ALL of the following hold:
//
//   - at least one block was found;
//   - every bullet in every block names a linter in formatOnlyLinters;
//   - each block's bullet counts SUM to that block's declared issue total.
//
// The sum check is the fail-closed guard: a truncated or interleaved block —
// where a non-format bullet could have been cut off by an output cap or
// interleaved writer — does not add up, so the classifier returns false and
// the loop takes the unchanged agent re-invoke path. A header with no bullets
// at all is likewise refused.
//
// Because `scripts/test verify` runs its lint leg FIRST under
// `set -euo pipefail` and aborts before the test loop, a lint-leg failure's
// output never carries test output — so a true classification implies the
// ONLY thing wrong with the tree is formatting.
//
// The input is UNTRUSTED (verify output is influenced by the diff under
// test). See runVerifyFixLoop's absorb block for the accepted bound.
func isFormatOnlyLintFailure(output string) bool {
	blocks := 0
	for _, block := range parseLintSummaryBlocks(output) {
		if len(block.bullets) == 0 {
			// A header with no bullets is a truncated block: a non-format
			// finding could have been cut off. Fail closed.
			return false
		}
		sum := 0
		for linter, count := range block.bullets {
			if !formatOnlyLinters[linter] {
				return false
			}
			sum += count
		}
		if sum != block.total {
			// Counts do not reconcile with the declared total — bullets are
			// missing or the block is interleaved. Fail closed.
			return false
		}
		blocks++
	}
	return blocks > 0
}

// lintSummaryBlock is one parsed `N issues:` header plus its bullets.
type lintSummaryBlock struct {
	total   int
	bullets map[string]int
}

// parseLintSummaryBlocks extracts every golangci-lint summary block from
// output, in order. A header line opens a block; consecutive bullet lines
// belong to it; the first non-bullet line closes it.
func parseLintSummaryBlocks(output string) []lintSummaryBlock {
	var blocks []lintSummaryBlock
	var cur *lintSummaryBlock
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if m := lintIssueTotalRe.FindStringSubmatch(line); m != nil {
			if cur != nil {
				blocks = append(blocks, *cur)
			}
			total, err := strconv.Atoi(m[1])
			if err != nil {
				// Unparseable total (an integer wider than int). Close the
				// open block and skip this header rather than guessing.
				cur = nil
				continue
			}
			cur = &lintSummaryBlock{total: total, bullets: map[string]int{}}
			continue
		}
		if cur == nil {
			continue
		}
		if m := lintLinterBulletRe.FindStringSubmatch(line); m != nil {
			count, err := strconv.Atoi(m[2])
			if err != nil {
				// Unparseable count — record an impossible bullet so the
				// block's sum check cannot reconcile and the caller fails
				// closed.
				cur.bullets[m[1]] = -1
				continue
			}
			cur.bullets[m[1]] += count
			continue
		}
		blocks = append(blocks, *cur)
		cur = nil
	}
	if cur != nil {
		blocks = append(blocks, *cur)
	}
	return blocks
}

// eligibleAutoformatFiles returns the repo-relative paths of the stage's
// declared scope files the formatter may be handed: REGULAR, NON-SYMLINKED,
// on-disk `.go` files that resolve inside repoDir. Every other declared path
// is dropped and left to the agent.
//
// Filters, applied in order:
//
//	(a) LEXICAL escape — filepath.Clean the declared path and reject an
//	    absolute path or one that climbs out of repoDir.
//	(b) EXTENSION — the path must end in `.go`.
//	(c) SYMLINKED-PARENT walk — os.Lstat each ancestor directory from repoDir
//	    down to (but excluding) the leaf, rejecting the path on the first
//	    ancestor whose mode carries os.ModeSymlink. A symlinked parent escapes
//	    containment exactly as effectively as a symlinked leaf, and the lexical
//	    check in (a) operates on the path STRING, so it cannot see one.
//	(d) LEAF — os.Lstat, NEVER os.Stat. os.Stat FOLLOWS symbolic links, which
//	    is precisely how a scope path that IS a symlink would resolve to a
//	    regular file, pass the filter, and be handed to the formatter, which
//	    then rewrites the link TARGET — potentially outside the scope set or
//	    outside the repository. Reject a missing entry, a symlink, a directory
//	    and anything that is not a regular file.
//
// The existence filter in (d) is separately load-bearing: `golangci-lint fmt`
// exits 3 with `failed to process files: lstat <p>: no such file or directory`
// as soon as it reaches a missing path argument (verified against v2.13.2), so
// one deleted scope file would turn every absorb into an errored no-op.
//
// WHAT THE SYMLINK REJECTION BUYS, AND WHAT IT DOES NOT. It rejects a scope
// entry that IS a symlink, or that sits under a symlinked parent, AT THE
// MOMENT OF THE CHECK — it closes the STATIC symlink escape, and that is the
// whole of its guarantee. It does NOT defend against an adversarial CONCURRENT
// swap: eligibility finishes before runAutoformat hashes the files and well
// before the formatter subprocess reopens them, so a leaf — or a parent
// directory — replaced with a symlink inside that window can still redirect a
// write. Static fixtures can only establish protection against a path that was
// ALREADY a symlink, never against replacement.
//
// That residual is ACCEPTED rather than closed. The only actor able to perform
// such a swap is code from the diff under test, and that code already has
// direct write access to the worktree and to paths outside it; the formatter
// path therefore grants no capability the untrusted diff does not already
// have, so this is not a privilege boundary the absorb is responsible for
// holding. Making it TOCTOU-proof would need open file descriptors and
// /dev/fd paths handed to a formatter that does not accept them — a large,
// fragile change out of proportion to the risk. What the absorb IS
// responsible for — never turning a red tree green — is unaffected: the
// branch never sets `passed`, and the push still proceeds only on an explicit
// passing verify outcome.
func eligibleAutoformatFiles(repoDir string, scope []upload.ScopeFile) []string {
	var eligible []string
	for _, raw := range scopePaths(scope) {
		clean := filepath.Clean(raw)
		// (a) lexical escape.
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			continue
		}
		joined := filepath.Join(repoDir, clean)
		rel, err := filepath.Rel(repoDir, joined)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		// (b) extension.
		if !strings.HasSuffix(clean, ".go") {
			continue
		}
		// (c) symlinked-parent walk.
		if hasSymlinkedParent(repoDir, clean) {
			continue
		}
		// (d) leaf: os.Lstat, never os.Stat.
		fi, err := os.Lstat(joined)
		if err != nil || fi.Mode()&os.ModeSymlink != 0 || fi.IsDir() || !fi.Mode().IsRegular() {
			continue
		}
		eligible = append(eligible, clean)
	}
	return eligible
}

// hasSymlinkedParent reports whether any ancestor directory of the cleaned
// repo-relative path rel — from repoDir down to, but excluding, the leaf — is
// a symbolic link or cannot be stat'ed. An unreadable ancestor is treated as
// disqualifying: the walk cannot establish containment, so it refuses.
func hasSymlinkedParent(repoDir, rel string) bool {
	segments := strings.Split(rel, string(filepath.Separator))
	for i := 0; i < len(segments)-1; i++ {
		ancestor := filepath.Join(append([]string{repoDir}, segments[:i+1]...)...)
		fi, err := os.Lstat(ancestor)
		if err != nil || fi.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

// runAutoformat runs the formatter over files (repo-relative paths, already
// filtered by eligibleAutoformatFiles) in repoDir and returns the subset whose
// bytes actually changed.
//
// Change detection is a sha256 of each file's contents taken BEFORE and AFTER
// the run — the formatter reports nothing machine-readable about what it
// rewrote, and content hashing is what makes the loop's absorb condition
// ("the formatter actually edited something") observable.
//
// The formatter is executed through runBoundedGateArgv, the runner's ONE
// gate-containment path, with the paths as argv elements: no shell is
// involved, so a scope path containing shell metacharacters cannot inject a
// command. A fresh per-invocation lint cache directory is created and removed.
//
// A non-zero exit (including a missing binary) returns an error and an EMPTY
// changed set, so the caller falls through to the agent rather than repeating
// the verify iteration. Any partial edits the formatter made before failing
// stay in the working tree and fold into the stage's single scope-only commit,
// exactly as an agent edit would.
func runAutoformat(ctx context.Context, repoDir string, files []string, timeout time.Duration) ([]string, error) {
	if len(files) == 0 {
		return nil, nil
	}
	before := make(map[string]string, len(files))
	for _, f := range files {
		h, err := hashFileContents(filepath.Join(repoDir, f))
		if err != nil {
			return nil, fmt.Errorf("autoformat: hash %s before: %w", f, err)
		}
		before[f] = h
	}

	cacheParent, err := os.MkdirTemp("", "fishhawk-autoformat-*")
	if err != nil {
		return nil, fmt.Errorf("autoformat: lint cache dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(cacheParent) }()

	argv := append([]string{autoformatBinary, "fmt"}, files...)
	out, exitCode := runBoundedGateArgv(ctx, argv, repoDir,
		filepath.Join(cacheParent, "golangci-lint-cache"), timeout)
	if exitCode != 0 {
		return nil, fmt.Errorf("autoformat: %s fmt exited %d: %s",
			autoformatBinary, exitCode, verifyFailureExcerpt(out))
	}

	var changed []string
	for _, f := range files {
		h, err := hashFileContents(filepath.Join(repoDir, f))
		if err != nil {
			return nil, fmt.Errorf("autoformat: hash %s after: %w", f, err)
		}
		if h != before[f] {
			changed = append(changed, f)
		}
	}
	return changed, nil
}

// hashFileContents returns the hex sha256 of the file at path.
func hashFileContents(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // repo-relative path already contained by eligibleAutoformatFiles
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// logAutoformatSkipped emits the LOG-ONLY verify_autoformat_skipped line for
// one non-firing auto-format branch. It is deliberately not a trace event: the
// trace's verify_autoformatted kind must mean exactly "formatting was
// applied", so a skip must never be able to masquerade as one in the audit.
func logAutoformatSkipped(logSink io.Writer, cfg config, iteration int, reason, detail string) {
	_, _ = fmt.Fprintf(logSink,
		`{"event":"verify_autoformat_skipped","run_id":%q,"stage_id":%q,"iteration":%d,"reason":%q,"detail":%q}`+"\n",
		cfg.runID, cfg.stageID, iteration, reason, detail)
}
