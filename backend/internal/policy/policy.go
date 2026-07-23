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

	// Patch is the full unified-diff hunk text for the stage, carried
	// for downstream content consumers (the implement-review prompt,
	// #585) ONLY. It is deliberately NOT read by Evaluate or any check
	// function — constraints operate purely on ChangedFiles, so
	// constraint evaluation is identical whether or not Patch is set.
	// Empty for older bundles that predate the patch field and for
	// stages whose runner could not compute the patch.
	Patch string
}

// Constraints is the parsed shape of the workflow-spec stage's
// `constraints` block. Zero values mean "this constraint isn't
// configured" — Evaluate skips them.
//
// JSON tags pin the audit-payload shape (#233): the SPA's policy
// section reads `applied_constraints.<key>` from the
// `policy_evaluated` audit entry, so the keys are part of the
// public contract. Stay snake_case for consistency with every
// other audit payload.
type Constraints struct {
	// ForbiddenPaths: any changed file matching ANY pattern is a
	// violation. Patterns are gitignore-style globs.
	ForbiddenPaths []string `json:"forbidden_paths,omitempty"`
	// AllowedPaths: every changed file MUST match at least one
	// pattern. A file matching none is a violation.
	AllowedPaths []string `json:"allowed_paths,omitempty"`
	// MaxFilesChanged: 0 means "no limit"; otherwise the diff's
	// file count must be <= this value.
	MaxFilesChanged int `json:"max_files_changed,omitempty"`
	// RequiredOutcomes: closed set per the schema —
	// "tests_added_or_updated", "ci_green",
	// "verification_reported".
	RequiredOutcomes []string `json:"required_outcomes,omitempty"`
	// CIGreen is what an upstream signal said about the customer
	// CI's outcome. Required only if RequiredOutcomes contains
	// "ci_green"; nil when no signal is available yet — in which
	// case the constraint is treated as a violation rather than
	// silently passing (MVP_SPEC §6: honesty about gaps beats
	// fictional completeness).
	CIGreen *bool `json:"ci_green,omitempty"`
	// Verification is what the stage's committed-tree verify gate
	// actually reported. Required only if RequiredOutcomes contains
	// "verification_reported"; nil means "no verification signal was
	// available at evaluation time" — which, unlike CIGreen, is a
	// VIOLATION rather than a deferral (#1886 / ADR-059): the gate
	// exists to assert that verification actually ran and passed, so
	// an absent signal cannot be read as a pass.
	//
	// The json tag is load-bearing. EvaluationPayload.Applied is the
	// audit-payload shape the post-CI re-evaluation decodes and
	// re-emits (backend/internal/server/policy_reeval.go), so an
	// untagged or unexported field would silently drop the signal on
	// re-eval and flip a satisfied outcome into a violation.
	Verification *VerificationSignal `json:"verification,omitempty"`
}

// VerificationSignal is the digest of what a stage's committed-tree
// verify gate ran and reported, derived from the runner's
// `gate_evidence` trace event. Outcome carries the runner's
// vocabulary verbatim: "passed" | "failed" | "skipped".
type VerificationSignal struct {
	Outcome  string                `json:"outcome"`
	Commands []VerificationCommand `json:"commands,omitempty"`
}

// VerificationCommand is one verify invocation the gate ran, kept to
// the machine-checkable facts only (no output tails) so the audit
// payload stays bounded.
type VerificationCommand struct {
	Command  string `json:"command"`
	ExitCode int    `json:"exit_code"`
	Outcome  string `json:"outcome"`
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
		out = append(out, checkRequiredOutcomes(diff, c.RequiredOutcomes, c.CIGreen, c.Verification)...)
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
	counted := CountedFileCount(diff)
	if counted <= maxFiles {
		return nil
	}
	return []Violation{{
		Constraint: "max_files_changed",
		Detail:     fmt.Sprintf("changed %d files; limit %d", counted, maxFiles),
	}}
}

// CountedFileCount returns the number of changed files that count
// toward the max_files_changed constraint: every ChangedFile whose
// Path is NOT a generated path (see IsGeneratedPath). Only
// max_files_changed uses this exempted count — checkForbidden and
// checkAllowed still operate on the full file set, so a generated file
// under a forbidden glob is still a violation.
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
// fixed structural conventions, not author-supplied globs.
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

func checkRequiredOutcomes(diff Diff, outcomes []string, ciGreen *bool, verification *VerificationSignal) []Violation {
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
			// Branch protection (ADR-017 / #251) is the source of
			// truth for CI completion at merge time: the required-
			// status-checks snapshot on the run row enumerates the
			// contexts the PR must pass, and GitHub itself blocks
			// the merge until they're green. The policy engine
			// evaluates at trace-upload time — before CI has even
			// started on the just-opened PR — so the only signal
			// available is nil. Pre-#297 we emitted a violation in
			// that case, which made every Fishhawk-managed PR fail
			// `fishhawk_audit_complete` on a false-positive.
			//
			// Now: a nil signal at evaluation time defers the
			// outcome to branch protection. DeferredRequiredOutcomes
			// surfaces the same list to the audit payload (#297) so
			// the SPA can render a note ("ci_green deferred to
			// branch protection"). A non-nil-and-false signal still
			// fails — preserves correctness for a future code path
			// that re-evaluates after CI lands.
			if ciGreen == nil {
				continue
			}
			if !*ciGreen {
				v = append(v, Violation{
					Constraint: "required_outcomes",
					Detail:     "ci is not green",
				})
			}
		case "verification_reported":
			// Substance-aware sibling of tests_added_or_updated
			// (#1886 / ADR-059 Option C.2). It gates on what the
			// stage actually RAN and whether it PASSED, read from
			// the runner's machine-verified gate evidence — NOT on
			// whether the diff contains a test-shaped filename.
			//
			// Deliberately fail-closed and asymmetric with
			// tests_added_or_updated: there is NO filename
			// inspection (isTestPath / diffTouchesTests are not
			// consulted, so a diff whose only change is a
			// test-NAMED file does not satisfy this) and NO
			// docs-only vacuous-satisfaction branch. A missing
			// signal, a failed gate, and a SKIPPED gate are each a
			// violation — a skipped verify gate is not a passed
			// gate. That asymmetry is the entire point of the
			// outcome, so do not "fix" it by borrowing a branch
			// from above.
			//
			// Correspondingly this outcome is NOT deferrable (see
			// DeferredRequiredOutcomes): deferring it would
			// reconstruct the vacuous pass it exists to remove.
			switch {
			case verification == nil:
				v = append(v, Violation{
					Constraint: "required_outcomes",
					Detail:     "no verification evidence in trace",
				})
			case verification.Outcome != "passed":
				v = append(v, Violation{
					Constraint: "required_outcomes",
					Detail:     verificationNotPassedDetail(verification),
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

// verificationNotPassedDetail renders the violation detail for a
// non-passing verification signal: the reported outcome, plus the
// first non-passing command when the signal carried one, so the
// audit entry names WHAT failed rather than just that something did.
func verificationNotPassedDetail(v *VerificationSignal) string {
	outcome := v.Outcome
	if outcome == "" {
		outcome = "unknown"
	}
	for _, c := range v.Commands {
		if c.Outcome != "passed" && c.Command != "" {
			return fmt.Sprintf("verification outcome %q (command %q exited %d)",
				outcome, c.Command, c.ExitCode)
		}
	}
	return fmt.Sprintf("verification outcome %q, want \"passed\"", outcome)
}

// DeferredRequiredOutcomes returns the names of required_outcomes
// whose evaluation was skipped because no signal was available at
// evaluation time (#297). At trace-upload time the only deferrable
// outcome is `ci_green`: CI hasn't run on the just-opened PR yet,
// so branch protection (#251 / ADR-017) is the actual gate at
// merge time. Returns an empty slice when nothing was deferred.
//
// `verification_reported` is deliberately NOT deferrable (#1886): a
// missing verification signal is a violation, not a deferral, so
// adding it here would reconstruct exactly the vacuous pass that
// outcome exists to remove.
//
// Callers persist this list in the policy_evaluated audit payload
// so reviewers can see which outcomes the policy engine declined
// to assert on. The SPA renders the list as an info note next to
// the pass state.
func DeferredRequiredOutcomes(c Constraints) []string {
	var out []string
	for _, o := range c.RequiredOutcomes {
		if o == "ci_green" && c.CIGreen == nil {
			out = append(out, o)
		}
	}
	return out
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
