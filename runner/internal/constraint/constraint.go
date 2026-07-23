// Package constraint evaluates the closed set of workflow-spec
// constraints (forbidden_paths, allowed_paths, max_files_changed,
// required_outcomes) against a stage's actual output. Returns a
// list of violations; one or more violations means the stage failed
// as category-B (constraint / policy violation) per MVP_SPEC §6.
//
// Designed to be agent-agnostic: the input is a Diff (list of
// changed files with status) and a Constraints struct. The runner
// produces the Diff via `git diff --name-status` after the agent
// has finished writing files; the backend will use the same
// package on ingest to re-verify what the runner reported.
//
// Glob semantics follow gitignore-style: `**` matches any number
// of path segments, `*` matches within a segment, leading `/` is
// implicit (paths are repo-relative). Implementation delegates to
// github.com/bmatcuk/doublestar/v4, which is the de-facto Go
// matcher for this style.
package constraint

import (
	"fmt"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Status is the per-file change kind. The values mirror the single
// letter `git diff --name-status` emits, so a thin parser can build
// a Diff without translation.
type Status string

// Status letter values mirroring `git diff --name-status` output;
// see git-diff(1) for the full meaning of each.
const (
	StatusAdded    Status = "A"
	StatusModified Status = "M"
	StatusDeleted  Status = "D"
	StatusRenamed  Status = "R"
	StatusCopied   Status = "C"
	StatusTypeChg  Status = "T"
)

// ChangedFile is one row of a stage's diff. Path is repo-relative
// and uses forward slashes regardless of platform.
type ChangedFile struct {
	Path   string
	Status Status
}

// Diff is the input to Evaluate: every file the agent touched in
// the stage, in any order. Empty Diff means the agent produced no
// changes (which is itself a constraint check for some stages).
type Diff struct {
	ChangedFiles []ChangedFile
}

// Constraints is the parsed shape of the workflow-spec stage's
// `constraints` block. Zero values mean "this constraint isn't
// configured" — Evaluate skips them.
type Constraints struct {
	// ForbiddenPaths: any changed file matching ANY pattern is a
	// violation. Patterns are gitignore-style globs.
	ForbiddenPaths []string
	// AllowedPaths: every changed file MUST match at least one
	// pattern. A file matching none is a violation.
	AllowedPaths []string
	// MaxFilesChanged: 0 means "no limit"; otherwise the diff's
	// file count must be <= this value.
	MaxFilesChanged int
	// RequiredOutcomes: closed set per the schema —
	// "tests_added_or_updated", "ci_green".
	RequiredOutcomes []string
	// CIGreen is what an upstream signal said about the customer
	// CI's outcome. Required only if RequiredOutcomes contains
	// "ci_green"; nil when the runner doesn't have a CI signal
	// available (the backend re-checks once CI completes).
	CIGreen *bool
}

// Violation is one constraint failure. Constraint names match the
// workflow-spec keys so log scrapers and the audit log can index
// on them. Detail is human-readable; Files lists the offending
// paths when relevant.
type Violation struct {
	Constraint string
	Detail     string
	Files      []string
}

func (v Violation) String() string {
	if len(v.Files) == 0 {
		return fmt.Sprintf("%s: %s", v.Constraint, v.Detail)
	}
	return fmt.Sprintf("%s: %s [%s]", v.Constraint, v.Detail, strings.Join(v.Files, ", "))
}

// Evaluate runs every configured constraint against the diff and
// returns the list of violations in deterministic order (by
// constraint name, then by file). An empty slice means the diff
// satisfies every constraint.
//
// Bad glob patterns produce a Violation with Constraint =
// "<original>" and Detail = "invalid pattern" rather than crashing;
// the runner's policy-event for the bundle should surface these so
// the workflow-spec author sees the typo without reading runner
// stderr.
func Evaluate(diff Diff, c Constraints) []Violation {
	var out []Violation

	if len(c.ForbiddenPaths) > 0 {
		out = append(out, checkForbidden(diff, c.ForbiddenPaths)...)
	}
	if len(c.AllowedPaths) > 0 {
		out = append(out, checkAllowed(diff, c.AllowedPaths)...)
	}
	if c.MaxFilesChanged > 0 {
		out = append(out, checkMaxFiles(diff, c.MaxFilesChanged)...)
	}
	if len(c.RequiredOutcomes) > 0 {
		out = append(out, checkRequiredOutcomes(diff, c.RequiredOutcomes, c.CIGreen)...)
	}

	return out
}

func checkForbidden(diff Diff, patterns []string) []Violation {
	var v []Violation
	for _, pat := range patterns {
		hit := matchAny(diff, pat)
		if len(hit.invalid) > 0 {
			v = append(v, Violation{
				Constraint: "forbidden_paths",
				Detail:     fmt.Sprintf("invalid pattern %q", pat),
			})
			continue
		}
		if len(hit.matched) > 0 {
			v = append(v, Violation{
				Constraint: "forbidden_paths",
				Detail:     fmt.Sprintf("pattern %q matched", pat),
				Files:      hit.matched,
			})
		}
	}
	return v
}

func checkAllowed(diff Diff, patterns []string) []Violation {
	// A file is allowed if ANY pattern matches. The violation list
	// names every file that matched none.
	var bad []string
	var hadInvalid bool
	for _, f := range diff.ChangedFiles {
		matched := false
		for _, pat := range patterns {
			ok, err := doublestar.PathMatch(pat, f.Path)
			if err != nil {
				hadInvalid = true
				continue
			}
			if ok {
				matched = true
				break
			}
		}
		if !matched {
			bad = append(bad, f.Path)
		}
	}
	var out []Violation
	if hadInvalid {
		out = append(out, Violation{
			Constraint: "allowed_paths",
			Detail:     "one or more patterns invalid",
		})
	}
	if len(bad) > 0 {
		out = append(out, Violation{
			Constraint: "allowed_paths",
			Detail:     "files outside allowed patterns",
			Files:      bad,
		})
	}
	return out
}

func checkMaxFiles(diff Diff, max int) []Violation {
	counted := CountedFileCount(diff)
	if counted <= max {
		return nil
	}
	return []Violation{{
		Constraint: "max_files_changed",
		Detail:     fmt.Sprintf("changed %d files; limit %d", counted, max),
	}}
}

// CountedFileCount returns the number of changed files that count
// toward the max_files_changed constraint: every ChangedFile whose
// Path is NOT a generated path (see IsGeneratedPath). Only
// max_files_changed uses this exempted count — checkForbidden and
// checkAllowed still operate on the full file set, so a generated file
// under a forbidden glob is still a violation. Mirrors
// backend/internal/policy.CountedFileCount so the runner's in-line
// verdict equals the backend re-verify.
func CountedFileCount(diff Diff) int {
	n := 0
	for _, f := range diff.ChangedFiles {
		if !IsGeneratedPath(f.Path) {
			n++
		}
	}
	return n
}

// IsGeneratedPath reports whether p is a generated or vendored path
// exempt from the max_files_changed file count. Two classes are
// exempt:
//
//   - sqlc-generated db packages — a `.go` file under a `db/`
//     directory. This mirrors CI's coverage exclusion, which drops
//     any path containing the substring `/db/` (scripts/check-coverage.py
//     `--exclude '/db/'`), narrowed to `.go` files per #2054's
//     `*/db/*.go` phrasing. A hand-written package under a `db/`
//     directory is exempted too, exactly as CI's coverage exclusion
//     already treats it.
//   - vendored dependencies — anything under a `vendor/` directory.
//
// Plain string matching (like isTestPath), not doublestar: these are
// fixed structural conventions, not author-supplied globs. Mirrors
// backend/internal/policy.IsGeneratedPath — change them together.
func IsGeneratedPath(p string) bool {
	// sqlc db packages: a .go file under a db/ directory.
	if strings.HasSuffix(p, ".go") && (strings.Contains(p, "/db/") || strings.HasPrefix(p, "db/")) {
		return true
	}
	// Vendored dependencies.
	if strings.HasPrefix(p, "vendor/") || strings.Contains(p, "/vendor/") {
		return true
	}
	return false
}

func checkRequiredOutcomes(diff Diff, outcomes []string, ciGreen *bool) []Violation {
	var v []Violation
	for _, o := range outcomes {
		switch o {
		case "tests_added_or_updated":
			switch {
			case diffTouchesTests(diff):
				// A recognized test file was added or updated — satisfied.
			case len(diff.ChangedFiles) > 0 && !diffTouchesTestableCode(diff):
				// Non-empty diff touching only docs/scripts/config: no
				// unit-testable source changed, so the outcome is
				// vacuously satisfied (#610). The len()>0 guard keeps an
				// EMPTY diff failing — that still signals "stage produced
				// nothing."
			default:
				v = append(v, Violation{
					Constraint: "required_outcomes",
					Detail:     "no test files added or updated",
				})
			}
		case "ci_green":
			// The runner can only check ci_green when an upstream
			// signal was provided. When CIGreen is nil, the runner
			// records that as 'unknown' and lets the backend
			// re-check post-hoc once CI completes — silence here
			// would falsely pass the constraint.
			if ciGreen == nil {
				v = append(v, Violation{
					Constraint: "required_outcomes",
					Detail:     "ci_green required but no signal available; backend will re-check",
				})
			} else if !*ciGreen {
				v = append(v, Violation{
					Constraint: "required_outcomes",
					Detail:     "ci is not green",
				})
			}
		case "verification_reported":
			// Backend-authoritative (#1886 / ADR-059): skipped here,
			// never a violation. This in-line constraint check fires
			// on the implement push path BEFORE either committed-tree
			// verify gate runs (the verify-fix loop and the
			// single-shot gate both come later in
			// runner/cmd/fishhawk-runner/main.go), so no verify
			// result exists yet locally — there is nothing truthful
			// the runner could assert. The backend re-evaluates the
			// outcome against the uploaded bundle's gate_evidence,
			// where the terminal verify result IS available.
			//
			// Without this case the default branch below would emit
			// `unknown outcome "verification_reported"` and fail
			// every opted-in run as category-B before the agent's
			// work was even verified.
		default:
			// The schema enum bounds this; defense-in-depth
			// catches drift between schema and code.
			v = append(v, Violation{
				Constraint: "required_outcomes",
				Detail:     fmt.Sprintf("unknown outcome %q", o),
			})
		}
	}
	return v
}

// diffTouchesTests reports whether any added or modified file in
// the diff looks like a test by path convention. Closed set of
// patterns covering the languages v0 customers most commonly use;
// extend per-language as needs surface.
func diffTouchesTests(diff Diff) bool {
	for _, f := range diff.ChangedFiles {
		if f.Status == StatusDeleted {
			continue
		}
		if isTestPath(f.Path) {
			return true
		}
	}
	return false
}

// isTestPath returns true for paths that match a recognized test
// convention. Kept as a single function so the heuristic set lives
// in one place; new frameworks add a clause here.
func isTestPath(p string) bool {
	low := strings.ToLower(p)
	base := filepathBase(low)
	switch {
	case strings.HasSuffix(low, "_test.go"): // Go
		return true
	case strings.HasSuffix(low, ".test.ts"),
		strings.HasSuffix(low, ".test.tsx"),
		strings.HasSuffix(low, ".test.js"),
		strings.HasSuffix(low, ".test.jsx"),
		strings.HasSuffix(low, ".spec.ts"),
		strings.HasSuffix(low, ".spec.tsx"),
		strings.HasSuffix(low, ".spec.js"),
		strings.HasSuffix(low, ".spec.jsx"): // JS / TS
		return true
	case strings.HasPrefix(low, "tests/") ||
		strings.Contains(low, "/tests/") ||
		strings.HasPrefix(low, "test/") ||
		strings.Contains(low, "/test/"): // Python / Rust / C++ test directories
		return true
	case strings.HasSuffix(low, "_test.py"),
		strings.HasPrefix(base, "test_"): // Python
		return true
	case strings.Contains(low, "/spec/"): // Ruby / Elixir
		return true
	case base == "test" || base == "tests": // shell/script test runner (e.g. scripts/test)
		return true
	case strings.HasPrefix(base, "test-"): // hyphenated script test convention (e.g. scripts/test-dev)
		return true
	case (strings.HasPrefix(low, "scripts/") || strings.Contains(low, "/scripts/")) &&
		strings.HasPrefix(base, "test"): // any scripts/test* helper (test, test-dev, test-coverage)
		return true
	}
	return false
}

// diffTouchesTestableCode reports whether the diff changes any file
// with a recognized source-code extension. Used to scope
// tests_added_or_updated: a non-empty diff that touches only docs,
// shell scripts, or config (no source extension in this set) has no
// unit-testable code, so the outcome is vacuously satisfied rather
// than failed. The allowlist is a heuristic, not exhaustive — an
// unrecognized future source language reads as "no testable code"
// and passes vacuously (fail open, never a new false-fail); extend
// the set here, the same one-function spot as isTestPath.
func diffTouchesTestableCode(diff Diff) bool {
	for _, f := range diff.ChangedFiles {
		if f.Status == StatusDeleted {
			continue
		}
		low := strings.ToLower(f.Path)
		for _, ext := range testableSourceExts {
			if strings.HasSuffix(low, ext) {
				return true
			}
		}
	}
	return false
}

// testableSourceExts is the set of file extensions that count as
// unit-testable source code for diffTouchesTestableCode. Docs (.md),
// shell scripts, and YAML/JSON config are deliberately excluded.
var testableSourceExts = []string{
	".go", ".ts", ".tsx", ".js", ".jsx", ".py", ".rb", ".rs",
	".java", ".kt", ".swift", ".scala", ".c", ".cc", ".cpp",
	".h", ".hpp", ".cs", ".php", ".ex", ".exs", ".clj",
}

// filepathBase trims directory components from p without importing
// path/filepath (keeps platform-independence). Equivalent to
// path.Base for forward-slash inputs.
func filepathBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// matchResult collects the per-pattern outcome of a forbidden-paths
// check: matched is the list of diff paths the pattern flagged,
// invalid records whether the pattern itself failed to compile.
type matchResult struct {
	matched []string
	invalid []string
}

func matchAny(diff Diff, pattern string) matchResult {
	var r matchResult
	for _, f := range diff.ChangedFiles {
		ok, err := doublestar.PathMatch(pattern, f.Path)
		if err != nil {
			r.invalid = append(r.invalid, f.Path)
			continue
		}
		if ok {
			r.matched = append(r.matched, f.Path)
		}
	}
	return r
}
