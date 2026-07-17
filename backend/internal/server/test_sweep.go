package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// categoryPlanTestSweep is the audit-log category for the entry
// runTestSweep writes when it evaluates a plan's scope.files against the
// repository's existing *_test.go files (#942). Like plan_scope_precheck
// and plan_surface_sweep it is written even on a clean sweep (empty
// Findings) so a reader can distinguish "checked and clean" from "never
// checked" (an older run predating this feature).
const categoryPlanTestSweep = "plan_test_sweep"

// Test-sweep rule identifiers (#942). Stable strings — they appear in
// audit payloads and the plan-review prompt's gate-evidence block.
const (
	// testSweepRuleStemSibling flags a scoped production dir/name.go whose
	// stem-sibling dir/name_test.go exists on the base ref but is absent
	// from scope.files (the #885 class: the plan scoped upload_test.go
	// while the changed behavior's tests live in upload_pullrequest_test.go
	// — here the inverse signal: the sibling that exists was not scoped).
	testSweepRuleStemSibling = "stem_sibling"
	// testSweepRuleNewTestInTestedPackage flags a scope.files CREATE of a
	// new *_test.go in a directory that already has existing *_test.go
	// files (the shared-harness class: lineage_test.go in #862, the
	// prEventsAuditRepo harness in #876 — a new test file in a tested
	// package usually extends an existing harness the plan must scope).
	testSweepRuleNewTestInTestedPackage = "new_test_in_tested_package"
	// testSweepRuleMigrationWalk flags a scoped migrations/*.sql whose
	// pinned migration-walk test (postgres_test.go pins the LATEST
	// migration) is absent from scope.files — the #1031 class, missed by
	// planners three times (migrations 0029/0030/0031).
	testSweepRuleMigrationWalk = "migration_walk"
)

// testSweepPathTriggerRule is one row of the path-trigger rule table: a
// scoped path matching TriggerGlob (path.Match semantics; '*' does not
// cross '/') requires every RequiredPaths entry in scope.files. Rows are
// curated data, not matcher logic — the extension point for future
// pinned-test patterns ahead of the per-repo test_conventions config
// (#1004). RequiredPaths is a slice so a future row can require multiple
// paths per trigger (e.g. the deferred schema-sync pair rule). Evaluation
// is purely scope-set based: no dirListings consultation, so a required
// path is not verified to exist on the base ref — a moved required file
// yields at worst a stale advisory until its row is updated.
type testSweepPathTriggerRule struct {
	Rule          string
	TriggerGlob   string
	RequiredPaths []string
}

// testSweepPathTriggerRules is the curated rule table. The single
// migrations glob covers both .up.sql and .down.sql.
var testSweepPathTriggerRules = []testSweepPathTriggerRule{
	{
		Rule:          testSweepRuleMigrationWalk,
		TriggerGlob:   "backend/internal/postgres/migrations/*.sql",
		RequiredPaths: []string{"backend/internal/postgres/postgres_test.go"},
	},
}

// testSweepMaxMissingTests caps the existing-test names a single rule-2
// finding carries; the remainder is reported via OmittedCount so
// test-heavy packages (backend/internal/server has ~40 *_test.go files)
// stay readable without losing the truncation signal.
const testSweepMaxMissingTests = 10

// testSweepMaxDirs caps the distinct directories runTestSweep lists via
// the Contents API per plan upload, bounding the network cost added to
// the upload path. Directories beyond the cap are WARN-skipped
// (fail-open: no finding, never a block).
//
// Counted AFTER candidate expansion (#1004): a data-driven convention
// with a parallel-tree candidate (e.g. tests/test_{name}.py,
// spec/{relpath}_spec.rb) points at directories other than the
// production file's own, so one production file can contribute several
// distinct directories. The cap is raised from the original Go-only 10
// to absorb that fan-out.
const testSweepMaxDirs = 20

// TestSweepFinding is one test-sweep result: the plan touches TriggerPath
// but omits existing test files (MissingTests) the named Rule associates
// with it. OmittedCount carries the number of additional existing test
// files truncated from MissingTests (rule 2's cap).
//
// SubPlanTitle attributes the finding to a decomposition sub-plan when the
// trigger came from that sub-plan's own scope.files rather than the flat
// parent scope (#1077). Empty for parent-scope findings.
type TestSweepFinding struct {
	Rule         string   `json:"rule"`
	TriggerPath  string   `json:"trigger_path"`
	MissingTests []string `json:"missing_tests"`
	OmittedCount int      `json:"omitted_count,omitempty"`
	SubPlanTitle string   `json:"sub_plan_title,omitempty"`
}

// TestSweepPayload is the audit-payload shape for a plan_test_sweep
// entry (#942). Findings is marshalled as an empty array (not null) on a
// clean sweep, mirroring scope_precheck's "checked and clean vs never
// checked" rationale. ScannedFiles is the count of scope.files evaluated;
// ListedDirs is the count of directories successfully listed via the
// Contents API (0 means every listing failed open — findings may be
// incomplete).
type TestSweepPayload struct {
	Findings     []TestSweepFinding `json:"findings"`
	ScannedFiles int                `json:"scanned_files"`
	ListedDirs   int                `json:"listed_dirs"`
}

// testConvention is one effective test-location convention used by the
// sweep matcher: production files whose repo-relative path matches Match
// (doublestar glob, `**` crosses '/') are expected to have a test at one
// of Candidates (path templates with {dir}/{name}/{ext}/{relpath}). It
// is the in-package mirror of spec.TestConvention; effectiveTestConventions
// converts the declared spec entries and appends them to the defaults.
type testConvention struct {
	Match      string
	Candidates []string
}

// defaultTestConventions reproduces #1003's hardcoded behavior as data:
// the Go stem-sibling rule plus colocated TypeScript. They are ALWAYS in
// effect (declared conventions append, never replace), so a repo that
// declares only Python/Ruby keeps Go + colocated TS covered, and a spec
// carrying no test_conventions is byte-identical to the pre-#1004 sweep.
var defaultTestConventions = []testConvention{
	{Match: "**/*.go", Candidates: []string{"{dir}/{name}_test.go"}},
	{Match: "**/*.{ts,tsx}", Candidates: []string{
		"{dir}/{name}.test.{ext}",
		"{dir}/{name}.spec.{ext}",
		"{dir}/__tests__/{name}.test.{ext}",
	}},
}

// effectiveTestConventions returns the built-in defaults with the run's
// declared conventions appended. Declared entries are additive (never
// replace the defaults) per the #1004 product-design choice.
func effectiveTestConventions(declared []spec.TestConvention) []testConvention {
	out := make([]testConvention, 0, len(defaultTestConventions)+len(declared))
	out = append(out, defaultTestConventions...)
	for _, d := range declared {
		out = append(out, testConvention{Match: d.Match, Candidates: d.Candidates})
	}
	return out
}

// expandCandidate substitutes the candidate-template variables for a
// production-file path p and returns a slash-normalized candidate path:
//
//   - {dir}     → path.Dir(p)               (the production file's directory)
//   - {name}    → basename minus final ext  (e.g. "upload" for upload.go)
//   - {ext}     → final extension, no dot    (e.g. "tsx" for Foo.tsx)
//   - {relpath} → full path minus final ext  (e.g. "lib/foo/bar" for lib/foo/bar.rb)
func expandCandidate(tmpl, p string) string {
	p = filepath.ToSlash(p)
	base := path.Base(p)
	ext := path.Ext(base)
	r := strings.NewReplacer(
		"{dir}", path.Dir(p),
		"{name}", strings.TrimSuffix(base, ext),
		"{ext}", strings.TrimPrefix(ext, "."),
		"{relpath}", strings.TrimSuffix(p, ext),
	)
	return path.Clean(r.Replace(tmpl))
}

// testFileRecognizers derives, from every convention's candidate
// templates, a deduped sorted set of basename globs that recognize a
// test file. Each candidate's final path segment with {name}/{ext}/
// {relpath} replaced by '*' is the recognizer (e.g. {dir}/{name}_test.go
// → *_test.go; tests/test_{name}.py → test_*.py). Basename-level so it
// matches names within a directory listing.
func testFileRecognizers(conventions []testConvention) []string {
	set := map[string]bool{}
	r := strings.NewReplacer("{name}", "*", "{ext}", "*", "{relpath}", "*")
	for _, c := range conventions {
		for _, tmpl := range c.Candidates {
			set[r.Replace(path.Base(tmpl))] = true
		}
	}
	out := make([]string, 0, len(set))
	for g := range set {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// isTestFileBasename reports whether name (a directory-listing basename)
// matches any recognizer. doublestar.Match errors only on a malformed
// pattern; recognizers are derived from curated/validated templates, so
// a bad one simply never matches (fail-open).
func isTestFileBasename(recognizers []string, name string) bool {
	for _, g := range recognizers {
		if ok, _ := doublestar.Match(g, name); ok {
			return true
		}
	}
	return false
}

// dedupTestSweepFindings collapses findings sharing (rule, trigger path,
// sub-plan title) to the first occurrence (#1004 amendment 2): an
// overlapping declared+default convention can otherwise double-report the
// same trigger. The candidate set is also deduped per production file
// (in the rule-1 loop) so MissingTests never lists a path twice.
func dedupTestSweepFindings(findings []TestSweepFinding) []TestSweepFinding {
	if len(findings) < 2 {
		return findings
	}
	type key struct{ rule, trigger, sub string }
	seen := make(map[key]bool, len(findings))
	out := findings[:0]
	for _, f := range findings {
		k := key{f.Rule, f.TriggerPath, f.SubPlanTitle}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, f)
	}
	return out
}

// evaluateTestSweep is the pure matcher (#942, generalized to data-driven
// conventions in #1004). dirListings maps a slash-normalized directory
// path to the base names of the files that exist in it on the base ref; a
// directory absent from the map was not listed (skipped or failed open)
// and produces no findings. conventions are the effective conventions
// (built-in defaults ++ the run's declared entries). Three deterministic
// rules:
//
//   - stem-sibling: a scoped production file matching a convention's
//     `match` (and not itself a test file) whose expanded candidate test
//     exists in its directory listing and is not in scope. The rule id
//     stays stem_sibling for audit/prompt stability.
//   - new-test-in-tested-package: a scoped CREATE whose basename is a
//     recognized test file, in a directory whose listing already has
//     other recognized test files — those existing files (minus any
//     already in scope) are reported sorted, capped at
//     testSweepMaxMissingTests with OmittedCount carrying the remainder.
//   - path-trigger table rows (testSweepPathTriggerRules, currently
//     migration_walk): a scoped path matching a row's trigger glob whose
//     required paths are not all in scope — evaluated against the scope
//     set only, never dirListings.
//
// Paths are slash-normalized like evaluateSurfaceSweep; a scoped test
// file never flags itself; findings are deduped by (rule, trigger path)
// and sorted (rule, then trigger path) for deterministic output.
//
// With no declared conventions the effective set is the built-in
// defaults, which reproduce #1003's Go (+ colocated TS) behavior
// byte-identically.
//
// This is NOT call-graph or behavior-coverage analysis: a plan changing
// behavior in package A whose tests live in package B is out of reach by
// design (#942 explicitly defers that), exactly as surface_sweep's
// registry is not call-graph analysis.
func evaluateTestSweep(scopeFiles []plan.ScopeFile, dirListings map[string][]string, conventions []testConvention) []TestSweepFinding {
	scope := make(map[string]bool, len(scopeFiles))
	for _, f := range scopeFiles {
		scope[filepath.ToSlash(f.Path)] = true
	}

	// listingHas reports whether the listed directory contains the base
	// name. Directories absent from dirListings report false for every
	// name (not listed — fail-open, no finding).
	listingHas := func(dir, name string) bool {
		for _, n := range dirListings[dir] {
			if n == name {
				return true
			}
		}
		return false
	}

	recognizers := testFileRecognizers(conventions)

	var findings []TestSweepFinding
	for _, f := range scopeFiles {
		p := filepath.ToSlash(f.Path)

		// Path-trigger rules run independent of the conventions: their
		// triggers (migration .sql files) are not production source files.
		// path.Match errors only on a malformed pattern; the table is
		// curated constants, so a bad row simply never matches (fail-open).
		for _, rule := range testSweepPathTriggerRules {
			if matched, _ := path.Match(rule.TriggerGlob, p); !matched {
				continue
			}
			var missing []string
			for _, req := range rule.RequiredPaths {
				if !scope[req] {
					missing = append(missing, req)
				}
			}
			if len(missing) > 0 {
				findings = append(findings, TestSweepFinding{
					Rule:         rule.Rule,
					TriggerPath:  p,
					MissingTests: missing,
				})
			}
		}

		base := path.Base(p)

		if !isTestFileBasename(recognizers, base) {
			// Rule 1: stem-sibling, generalized. For every convention the
			// production file matches, expand its candidates into a deduped
			// set; report those that exist on the base ref and aren't
			// scoped. Overlapping declared+default conventions collapse via
			// the candidate set, so one production file yields one finding.
			candSet := map[string]bool{}
			for _, c := range conventions {
				if ok, _ := doublestar.Match(c.Match, p); !ok {
					continue
				}
				for _, tmpl := range c.Candidates {
					candSet[expandCandidate(tmpl, p)] = true
				}
			}
			var missing []string
			for cand := range candSet {
				if cand == p {
					continue
				}
				if listingHas(path.Dir(cand), path.Base(cand)) && !scope[cand] {
					missing = append(missing, cand)
				}
			}
			if len(missing) > 0 {
				sort.Strings(missing)
				findings = append(findings, TestSweepFinding{
					Rule:         testSweepRuleStemSibling,
					TriggerPath:  p,
					MissingTests: missing,
				})
			}
			continue
		}

		// Rule 2: new-test-in-tested-package. Only a CREATE of a new test
		// file fires — modifying an existing test file is the plan doing
		// the right thing already.
		if f.Operation != plan.FileOpCreate {
			continue
		}
		dir := path.Dir(p)
		var existing []string
		for _, n := range dirListings[dir] {
			if n == base || !isTestFileBasename(recognizers, n) {
				continue
			}
			full := dir + "/" + n
			// A scoped test file never flags itself — an existing test
			// already in scope is exactly what the rule asks for.
			if scope[full] {
				continue
			}
			existing = append(existing, full)
		}
		if len(existing) == 0 {
			continue
		}
		sort.Strings(existing)
		omitted := 0
		if len(existing) > testSweepMaxMissingTests {
			omitted = len(existing) - testSweepMaxMissingTests
			existing = existing[:testSweepMaxMissingTests]
		}
		findings = append(findings, TestSweepFinding{
			Rule:         testSweepRuleNewTestInTestedPackage,
			TriggerPath:  p,
			MissingTests: existing,
			OmittedCount: omitted,
		})
	}

	findings = dedupTestSweepFindings(findings)
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Rule != findings[j].Rule {
			return findings[i].Rule < findings[j].Rule
		}
		return findings[i].TriggerPath < findings[j].TriggerPath
	})
	return findings
}

// runTestSweep evaluates an uploaded plan's scope.files against the
// repository's existing *_test.go files and records the result as an
// advisory plan_test_sweep audit entry (#942). It catches the class the
// static surface registry cannot: a plan that changes behavior whose
// tests live in an EXISTING test file not listed in scope.files, which
// the runner then scope_drift-excludes (silently dropping the test edit,
// as in #885) or reconciles late (#862/#876).
//
// fishhawkd has no local repo checkout, so the sweep consults the
// repository tree at plan time via the Contents API (ListDirectory) at
// the default-branch HEAD — an empty ref; run.Run carries no base-commit
// tree ref. A listing stale against a just-advanced main yields at worst
// one stale advisory finding, never a block.
//
// Advisory-only and fail-open, matching runScopePrecheck's degradation
// contract: it guards on RunRepo+AuditRepo, resolves the run first, and
// additionally fails open (WARN-log, nil) when no GitHub client is wired
// or the run has no installation (non-GitHub triggers and unwired
// deployments get no entry and no block). Per-directory listing failures
// fail open individually; the distinct scoped directories are capped at
// testSweepMaxDirs to bound plan-upload latency.
//
// Returns the computed result payload so handleShipPlan can thread it
// into the plan-review prompt's gate-evidence section; nil on every
// fail-open path (no result was computed). An audit-append failure still
// returns the computed result — the entry is observability, the
// evaluation itself succeeded.
func (s *Server) runTestSweep(ctx context.Context, runID, stageID uuid.UUID, planBody []byte) *TestSweepPayload {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return nil
	}

	// Resolve the run first so the sweep only records against a real,
	// resolvable run — matching runScopePrecheck's degradation contract.
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "test sweep: get run failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}

	// No GitHub client (unwired deployment) or no installation (non-GitHub
	// trigger): there is no tree to consult — fail open with no entry.
	if s.cfg.GitHub == nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "test sweep: no GitHub client configured; skipping",
			slog.String("run_id", runID.String()),
		)
		return nil
	}
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "test sweep: run has no installation id; skipping",
			slog.String("run_id", runID.String()),
		)
		return nil
	}
	repoRef, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "test sweep: parse repo failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	scope := forge.FromGitHubInstallationID(*runRow.InstallationID)

	// Validation already passed in handleShipPlan; a parse failure here is
	// an internal inconsistency — log and skip rather than block.
	parsedPlan, err := plan.Parse(planBody)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "test sweep: parse plan failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}

	// Resolve the effective test conventions from the run's cached
	// workflow spec (#1004): declared test_conventions append to the
	// built-in Go + colocated-TS defaults. Fail open to the defaults
	// only on an empty or unparseable spec (WARN-log, never block) —
	// matching the rest of the sweep's degradation contract.
	var declaredConventions []spec.TestConvention
	if len(runRow.WorkflowSpec) > 0 {
		if parsedSpec, perr := spec.ParseBytes(runRow.WorkflowSpec); perr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "test sweep: parse workflow spec failed; using default conventions",
				slog.String("run_id", runID.String()),
				slog.String("error", perr.Error()),
			)
		} else {
			declaredConventions = parsedSpec.TestConventions
		}
	}
	conventions := effectiveTestConventions(declaredConventions)
	recognizers := testFileRecognizers(conventions)

	// Collect the distinct directories the sweep must list, sorted so the
	// cap below is deterministic. For a production file matching a
	// convention, that is the directory of every EXPANDED candidate
	// (#1004): colocated candidates point at the file's own directory,
	// parallel-tree candidates (tests/, spec/) point elsewhere. For a
	// scoped test file (rule 2's trigger) it is the file's own directory.
	// Repo-root files are skipped: the Contents API addresses the root as
	// the empty path, which ListDirectory rejects. Collect from the parent
	// scope AND every decomposition sub-plan scope (#1077): an
	// under-scoped slice's directory must be listed so its coupling gaps
	// surface at the parent plan gate. The migration_walk path-trigger
	// rule needs no listing.
	dirSet := map[string]bool{}
	addDir := func(dir string) {
		if dir != "" && dir != "." {
			dirSet[dir] = true
		}
	}
	collectDirs := func(files []plan.ScopeFile) {
		for _, f := range files {
			p := filepath.ToSlash(f.Path)
			if isTestFileBasename(recognizers, path.Base(p)) {
				addDir(path.Dir(p))
				continue
			}
			for _, c := range conventions {
				if ok, _ := doublestar.Match(c.Match, p); !ok {
					continue
				}
				for _, tmpl := range c.Candidates {
					addDir(path.Dir(expandCandidate(tmpl, p)))
				}
			}
		}
	}
	collectDirs(parsedPlan.Scope.Files)
	if parsedPlan.Decomposition != nil {
		for _, sp := range parsedPlan.Decomposition.SubPlans {
			if sp.Scope != nil {
				collectDirs(sp.Scope.Files)
			}
		}
	}
	dirs := make([]string, 0, len(dirSet))
	for d := range dirSet {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	if len(dirs) > testSweepMaxDirs {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "test sweep: scoped directories exceed cap; skipping the rest",
			slog.String("run_id", runID.String()),
			slog.Int("dirs", len(dirs)),
			slog.Int("cap", testSweepMaxDirs),
		)
		dirs = dirs[:testSweepMaxDirs]
	}

	// List each directory at the default-branch HEAD (empty ref). Each
	// per-call failure fails open: the directory stays out of the map and
	// contributes no findings.
	dirListings := make(map[string][]string, len(dirs))
	listedDirs := 0
	for _, dir := range dirs {
		entries, lerr := s.cfg.GitHub.ListDirectory(ctx, scope, repoRef, dir, "")
		if lerr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "test sweep: list directory failed; skipping",
				slog.String("run_id", runID.String()),
				slog.String("dir", dir),
				slog.String("error", lerr.Error()),
			)
			continue
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.Type == "file" {
				names = append(names, e.Name)
			}
		}
		dirListings[dir] = names
		listedDirs++
	}

	// Every listing failed: no tree data was consulted, so recording a
	// "checked and clean" entry would be a lie — fail open with no entry
	// ("never checked"). A partial failure still records: ListedDirs <
	// the scoped-directory count flags the incompleteness.
	if len(dirs) > 0 && listedDirs == 0 {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "test sweep: every directory listing failed; skipping",
			slog.String("run_id", runID.String()),
			slog.Int("dirs", len(dirs)),
		)
		return nil
	}

	findings := evaluateTestSweep(parsedPlan.Scope.Files, dirListings, conventions)

	// Evaluate each decomposition sub-plan's own scope against the shared
	// dirListings (#1077), tagging findings with the sub-plan title. The
	// migration_walk rule is scope-set-only, so a migration slice fires
	// here automatically with no new rule.
	if parsedPlan.Decomposition != nil {
		for _, sp := range parsedPlan.Decomposition.SubPlans {
			if sp.Scope == nil {
				continue
			}
			for _, f := range evaluateTestSweep(sp.Scope.Files, dirListings, conventions) {
				f.SubPlanTitle = sp.Title
				findings = append(findings, f)
			}
		}
	}

	if findings == nil {
		// Marshal an empty array rather than null so the audit payload's
		// "checked and clean" state is explicit (a missing entry means
		// "never checked").
		findings = []TestSweepFinding{}
	}

	result := &TestSweepPayload{
		Findings:     findings,
		ScannedFiles: len(parsedPlan.Scope.Files),
		ListedDirs:   listedDirs,
	}
	payload, _ := json.Marshal(result)
	systemKind := audit.ActorKind("system")
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  categoryPlanTestSweep,
		ActorKind: &systemKind,
		Payload:   payload,
	}); aerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "test sweep: append audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", aerr.Error()),
		)
	}
	return result
}
