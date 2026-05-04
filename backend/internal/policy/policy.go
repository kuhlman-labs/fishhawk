// Package policy evaluates the closed set of workflow-spec
// constraints (forbidden_paths, allowed_paths, max_files_changed,
// required_outcomes) against a stage's actual output. One or more
// violations means the stage failed as category-B (constraint /
// policy violation) per MVP_SPEC §6.
//
// This package is the backend's source of truth. The runner runs
// the same checks in-line (runner/internal/constraint) so the
// agent gets immediate feedback, but the runner is operating on
// a customer machine and its report alone is not auditable. The
// backend re-evaluates every uploaded trace using this package
// and writes the result as a chained audit entry — that audit
// entry is what's quoted in compliance exports.
//
// Glob semantics follow gitignore-style: `**` matches any number
// of path segments, `*` matches within a segment, leading `/` is
// implicit (paths are repo-relative). Implementation delegates to
// github.com/bmatcuk/doublestar/v4.
package policy

import (
	"fmt"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Status is the per-file change kind. Values mirror the single
// letter `git diff --name-status` emits, so the runner's parsed
// diff round-trips through the trace bundle into this package
// without translation.
type Status string

// Status letter values mirroring `git diff --name-status` output.
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

// Diff is the input to Evaluate: every file the stage touched, in
// any order. An empty Diff is itself meaningful for some stages
// (e.g. plan stages that aren't supposed to change source).
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
	// "ci_green"; nil when no signal is available yet — in which
	// case the constraint is treated as a violation rather than
	// silently passing (MVP_SPEC §6: honesty about gaps beats
	// fictional completeness).
	CIGreen *bool
}

// Violation is one constraint failure. Constraint names match the
// workflow-spec keys so log scrapers and the audit log can index
// on them. Detail is human-readable; Files lists offending paths
// when relevant.
type Violation struct {
	Constraint string   `json:"constraint"`
	Detail     string   `json:"detail"`
	Files      []string `json:"files,omitempty"`
}

func (v Violation) String() string {
	if len(v.Files) == 0 {
		return fmt.Sprintf("%s: %s", v.Constraint, v.Detail)
	}
	return fmt.Sprintf("%s: %s [%s]", v.Constraint, v.Detail, strings.Join(v.Files, ", "))
}

// Evaluate runs every configured constraint against the diff and
// returns the list of violations in deterministic order (constraint
// name, then file). An empty slice means the diff satisfies every
// constraint.
//
// Bad glob patterns produce a Violation with Detail = "invalid
// pattern …" rather than crashing; the audit entry surfaces the
// typo so the workflow-spec author can fix it without reading
// backend logs.
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
		if hit.invalidPattern {
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

func checkMaxFiles(diff Diff, maxFiles int) []Violation {
	if len(diff.ChangedFiles) <= maxFiles {
		return nil
	}
	return []Violation{{
		Constraint: "max_files_changed",
		Detail:     fmt.Sprintf("changed %d files; limit %d", len(diff.ChangedFiles), maxFiles),
	}}
}

func checkRequiredOutcomes(diff Diff, outcomes []string, ciGreen *bool) []Violation {
	var v []Violation
	for _, o := range outcomes {
		switch o {
		case "tests_added_or_updated":
			if !diffTouchesTests(diff) {
				v = append(v, Violation{
					Constraint: "required_outcomes",
					Detail:     "no test files added or updated",
				})
			}
		case "ci_green":
			// Same semantics as the runner: nil signal records the
			// gap honestly rather than passing silently. The
			// orchestrator can re-evaluate once the CI signal lands.
			if ciGreen == nil {
				v = append(v, Violation{
					Constraint: "required_outcomes",
					Detail:     "ci_green required but no signal available",
				})
			} else if !*ciGreen {
				v = append(v, Violation{
					Constraint: "required_outcomes",
					Detail:     "ci is not green",
				})
			}
		default:
			// The schema enum bounds this; defense-in-depth catches
			// drift between schema and code.
			v = append(v, Violation{
				Constraint: "required_outcomes",
				Detail:     fmt.Sprintf("unknown outcome %q", o),
			})
		}
	}
	return v
}

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

func isTestPath(p string) bool {
	low := strings.ToLower(p)
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
		strings.HasPrefix(filepathBase(low), "test_"): // Python
		return true
	case strings.Contains(low, "/spec/"): // Ruby / Elixir
		return true
	}
	return false
}

func filepathBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// matchResult collects the per-pattern outcome of a forbidden-paths
// check. invalidPattern is true when doublestar rejected the
// pattern itself (separate from "valid pattern matched no files").
type matchResult struct {
	matched        []string
	invalidPattern bool
}

func matchAny(diff Diff, pattern string) matchResult {
	// Validate the pattern once against an empty path so we can
	// distinguish "bad pattern" from "pattern matched nothing."
	if _, err := doublestar.PathMatch(pattern, ""); err != nil {
		return matchResult{invalidPattern: true}
	}
	var r matchResult
	for _, f := range diff.ChangedFiles {
		ok, err := doublestar.PathMatch(pattern, f.Path)
		if err != nil {
			r.invalidPattern = true
			continue
		}
		if ok {
			r.matched = append(r.matched, f.Path)
		}
	}
	return r
}
