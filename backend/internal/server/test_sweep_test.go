package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
)

// Counterfactual record for #3203 (binding approval condition 3 — every
// counterfactual EXECUTED, with the observed RED recorded here rather than
// reasoned about). Each mutation was applied to the working tree, the named
// test run, the output below observed, and the file restored byte-identically.
//
//	(a) DELETED every generated_surface row from testSweepPathTriggerRules
//	    (backend/internal/server/test_sweep.go), kept the tests.
//	    go test ./internal/server/ -run TestEvaluateTestSweep -v
//	    RED — 6 subtests failed:
//	      openapi_document_without_the_api.md_region_flags_generated_surface
//	      work-management_schema_without_either_mirror_names_both_and_the_generator
//	      partially_scoped_mirror_set_reports_only_the_missing_mirror
//	      canonical_schema_feeding_two_generators_draws_one_finding_per_generator
//	      cmdinfo_inventory_flags_the_cli.md_region
//	      generated_surface_fires_with_an_empty_listings_map
//
//	(b) THE LOAD-BEARING ONE. Reverted dedupTestSweepFindings' key to
//	    (rule, trigger, sub) — dropping Generator — with every row left in
//	    place. go test ./internal/server/ -run
//	    'TestEvaluateTestSweep|TestShipPlan_TestSweep_GeneratedSurface_EndToEnd' -v
//	    RED on exactly the two-generator case and the end-to-end pin:
//	      --- FAIL: TestEvaluateTestSweep/canonical_schema_feeding_two_generators_draws_one_finding_per_generator
//	      --- FAIL: TestShipPlan_TestSweep_GeneratedSurface_EndToEnd
//	    Every other TestEvaluateTestSweep subtest stayed GREEN, which is the
//	    proof that the widened key is load-bearing for the new rule and inert
//	    for the three pre-existing ones.
//
//	(c) DELETED the `Generator: f.Generator` copy in planGateEvidence
//	    (backend/internal/server/plan.go).
//	    go test ./internal/server/ -run TestPlanGateEvidence_GeneratedSurfaceSeam -v
//	    RED: plan_test.go:4908: TestSweepFindingEvidence.Generator = "",
//	    want "scripts/gen-site-reference" (a drop in planGateEvidence)
//
//	(d) RENAMED one required path in a row (site/.../api.md ->
//	    site/.../api-RENAMED.md).
//	    go test ./internal/server/ -run TestGeneratedSurfaceRowsExistOnDisk -v
//	    RED: test_sweep_test.go:959: path-trigger row path
//	    "site/src/content/docs/reference/api-RENAMED.md" does not exist on
//	    disk (...) — a rename would silently disable the row
//
// Two further controls this pass ADDED are recorded next to their own
// tests: the prompt render branch (backend/internal/prompt/prompt_test.go)
// and the surface-sweep registry fix
// (backend/internal/server/surface_sweep_test.go).
func TestEvaluateTestSweep(t *testing.T) {
	const dir = "backend/internal/server"
	// manyTests builds n existing test-file names for the rule-2 cap case.
	manyTests := func(n int) []string {
		names := []string{"prod.go"}
		for i := 0; i < n; i++ {
			names = append(names, fmt.Sprintf("t%02d_test.go", i))
		}
		return names
	}
	tests := []struct {
		name string
		// conventions are the effective conventions passed to the matcher;
		// nil uses defaultTestConventions (the no-config byte-identical
		// path) so the pre-#1004 cases stay unchanged.
		conventions []testConvention
		scope       []plan.ScopeFile
		listings    map[string][]string
		want        []TestSweepFinding
	}{
		{
			// #885: the changed production file has an existing stem-sibling
			// test the plan did not scope.
			name:     "stem sibling exists and not in scope flags it",
			scope:    []plan.ScopeFile{{Path: dir + "/upload.go", Operation: plan.FileOpModify}},
			listings: map[string][]string{dir: {"upload.go", "upload_test.go"}},
			want: []TestSweepFinding{
				{
					Rule:         testSweepRuleStemSibling,
					TriggerPath:  dir + "/upload.go",
					MissingTests: []string{dir + "/upload_test.go"},
				},
			},
		},
		{
			name: "stem sibling in scope no finding",
			scope: []plan.ScopeFile{
				{Path: dir + "/upload.go", Operation: plan.FileOpModify},
				{Path: dir + "/upload_test.go", Operation: plan.FileOpModify},
			},
			listings: map[string][]string{dir: {"upload.go", "upload_test.go"}},
			want:     nil,
		},
		{
			name:     "no stem sibling on base ref no finding",
			scope:    []plan.ScopeFile{{Path: dir + "/upload.go", Operation: plan.FileOpModify}},
			listings: map[string][]string{dir: {"upload.go", "other_test.go"}},
			want:     nil,
		},
		{
			// A scoped test file is not a rule-1 trigger; modifying an
			// existing test never flags anything.
			name:     "scoped test file modify never flags itself",
			scope:    []plan.ScopeFile{{Path: dir + "/upload_test.go", Operation: plan.FileOpModify}},
			listings: map[string][]string{dir: {"upload.go", "upload_test.go", "other_test.go"}},
			want:     nil,
		},
		{
			// #862/#876: creating a new test file in a package that already
			// has tests surfaces the existing ones (the shared harness).
			name:     "new test in tested package flags existing tests sorted",
			scope:    []plan.ScopeFile{{Path: dir + "/feature_test.go", Operation: plan.FileOpCreate}},
			listings: map[string][]string{dir: {"zeta_test.go", "alpha_test.go", "prod.go"}},
			want: []TestSweepFinding{
				{
					Rule:         testSweepRuleNewTestInTestedPackage,
					TriggerPath:  dir + "/feature_test.go",
					MissingTests: []string{dir + "/alpha_test.go", dir + "/zeta_test.go"},
				},
			},
		},
		{
			name: "new test excludes existing tests already in scope",
			scope: []plan.ScopeFile{
				{Path: dir + "/feature_test.go", Operation: plan.FileOpCreate},
				{Path: dir + "/alpha_test.go", Operation: plan.FileOpModify},
			},
			listings: map[string][]string{dir: {"alpha_test.go", "prod.go"}},
			want:     nil,
		},
		{
			name:     "new test in untested package no finding",
			scope:    []plan.ScopeFile{{Path: dir + "/feature_test.go", Operation: plan.FileOpCreate}},
			listings: map[string][]string{dir: {"prod.go"}},
			want:     nil,
		},
		{
			// Rule-2 cap: 11 existing test files → 10 names + OmittedCount=1.
			name:     "rule 2 caps names and carries omitted count",
			scope:    []plan.ScopeFile{{Path: dir + "/feature_test.go", Operation: plan.FileOpCreate}},
			listings: map[string][]string{dir: manyTests(11)},
			want: []TestSweepFinding{
				{
					Rule:        testSweepRuleNewTestInTestedPackage,
					TriggerPath: dir + "/feature_test.go",
					MissingTests: []string{
						dir + "/t00_test.go", dir + "/t01_test.go", dir + "/t02_test.go",
						dir + "/t03_test.go", dir + "/t04_test.go", dir + "/t05_test.go",
						dir + "/t06_test.go", dir + "/t07_test.go", dir + "/t08_test.go",
						dir + "/t09_test.go",
					},
					OmittedCount: 1,
				},
			},
		},
		{
			// #1031: a scoped migration without the pinned migration-walk
			// test (postgres_test.go, where a new migration needs its own
			// reversal test there).
			name:     "migration sql without postgres_test flags migration_walk",
			scope:    []plan.ScopeFile{{Path: "backend/internal/postgres/migrations/0032_x.up.sql", Operation: plan.FileOpCreate}},
			listings: map[string][]string{},
			want: []TestSweepFinding{
				{
					Rule:         testSweepRuleMigrationWalk,
					TriggerPath:  "backend/internal/postgres/migrations/0032_x.up.sql",
					MissingTests: []string{"backend/internal/postgres/postgres_test.go"},
				},
			},
		},
		{
			name: "migration sql with postgres_test in scope no finding",
			scope: []plan.ScopeFile{
				{Path: "backend/internal/postgres/migrations/0032_x.up.sql", Operation: plan.FileOpCreate},
				{Path: "backend/internal/postgres/postgres_test.go", Operation: plan.FileOpModify},
			},
			listings: map[string][]string{},
			want:     nil,
		},
		{
			name:     "down-only migration sql also fires",
			scope:    []plan.ScopeFile{{Path: "backend/internal/postgres/migrations/0032_x.down.sql", Operation: plan.FileOpCreate}},
			listings: map[string][]string{},
			want: []TestSweepFinding{
				{
					Rule:         testSweepRuleMigrationWalk,
					TriggerPath:  "backend/internal/postgres/migrations/0032_x.down.sql",
					MissingTests: []string{"backend/internal/postgres/postgres_test.go"},
				},
			},
		},
		{
			// One finding per trigger path, deterministically sorted.
			name: "up and down migrations fire one finding each sorted",
			scope: []plan.ScopeFile{
				{Path: "backend/internal/postgres/migrations/0032_x.up.sql", Operation: plan.FileOpCreate},
				{Path: "backend/internal/postgres/migrations/0032_x.down.sql", Operation: plan.FileOpCreate},
			},
			listings: map[string][]string{},
			want: []TestSweepFinding{
				{
					Rule:         testSweepRuleMigrationWalk,
					TriggerPath:  "backend/internal/postgres/migrations/0032_x.down.sql",
					MissingTests: []string{"backend/internal/postgres/postgres_test.go"},
				},
				{
					Rule:         testSweepRuleMigrationWalk,
					TriggerPath:  "backend/internal/postgres/migrations/0032_x.up.sql",
					MissingTests: []string{"backend/internal/postgres/postgres_test.go"},
				},
			},
		},
		{
			// path.Match's '*' does not cross '/', so only direct children
			// of the migrations directory trigger.
			name:     "sql outside migrations dir does not fire",
			scope:    []plan.ScopeFile{{Path: "backend/internal/db/queries.sql", Operation: plan.FileOpModify}},
			listings: map[string][]string{},
			want:     nil,
		},
		{
			// Rule coexistence: migration_walk sorts before stem_sibling.
			name: "migration_walk coexists with stem_sibling in rule order",
			scope: []plan.ScopeFile{
				{Path: dir + "/upload.go", Operation: plan.FileOpModify},
				{Path: "backend/internal/postgres/migrations/0032_x.up.sql", Operation: plan.FileOpCreate},
			},
			listings: map[string][]string{dir: {"upload.go", "upload_test.go"}},
			want: []TestSweepFinding{
				{
					Rule:         testSweepRuleMigrationWalk,
					TriggerPath:  "backend/internal/postgres/migrations/0032_x.up.sql",
					MissingTests: []string{"backend/internal/postgres/postgres_test.go"},
				},
				{
					Rule:         testSweepRuleStemSibling,
					TriggerPath:  dir + "/upload.go",
					MissingTests: []string{dir + "/upload_test.go"},
				},
			},
		},
		{
			name:     "non-go files never trigger",
			scope:    []plan.ScopeFile{{Path: "docs/ARCHITECTURE.md", Operation: plan.FileOpModify}},
			listings: map[string][]string{"docs": {"ARCHITECTURE.md"}},
			want:     nil,
		},
		{
			name:     "unlisted directory produces no findings",
			scope:    []plan.ScopeFile{{Path: dir + "/upload.go", Operation: plan.FileOpModify}},
			listings: map[string][]string{},
			want:     nil,
		},
		{
			name:     "backslash path still matches via normalization",
			scope:    []plan.ScopeFile{{Path: filepath.FromSlash(dir + "/upload.go"), Operation: plan.FileOpModify}},
			listings: map[string][]string{dir: {"upload.go", "upload_test.go"}},
			want: []TestSweepFinding{
				{
					Rule:         testSweepRuleStemSibling,
					TriggerPath:  dir + "/upload.go",
					MissingTests: []string{dir + "/upload_test.go"},
				},
			},
		},
		{
			// Colocated TypeScript via the built-in defaults: Foo.tsx has an
			// existing Foo.test.tsx (colocated) and __tests__/Foo.test.tsx
			// (subdir) the plan didn't scope. One finding, MissingTests
			// sorted across both candidate directories.
			name:  "colocated ts via defaults flags existing sibling tests",
			scope: []plan.ScopeFile{{Path: "frontend/src/Foo.tsx", Operation: plan.FileOpModify}},
			listings: map[string][]string{
				"frontend/src":           {"Foo.tsx", "Foo.test.tsx"},
				"frontend/src/__tests__": {"Foo.test.tsx"},
			},
			want: []TestSweepFinding{
				{
					Rule:        testSweepRuleStemSibling,
					TriggerPath: "frontend/src/Foo.tsx",
					MissingTests: []string{
						"frontend/src/Foo.test.tsx",
						"frontend/src/__tests__/Foo.test.tsx",
					},
				},
			},
		},
		{
			// Declared Python parallel-tree convention: src/pkg/mod.py →
			// tests/test_mod.py. Exercises {name} and a candidate directory
			// other than the production file's own.
			name: "declared python parallel tree flags missing test",
			conventions: append(append([]testConvention{}, defaultTestConventions...),
				testConvention{Match: "src/**/*.py", Candidates: []string{"tests/test_{name}.py"}}),
			scope:    []plan.ScopeFile{{Path: "src/pkg/mod.py", Operation: plan.FileOpModify}},
			listings: map[string][]string{"tests": {"test_mod.py"}},
			want: []TestSweepFinding{
				{
					Rule:         testSweepRuleStemSibling,
					TriggerPath:  "src/pkg/mod.py",
					MissingTests: []string{"tests/test_mod.py"},
				},
			},
		},
		{
			// Declared Ruby convention exercising {relpath} (full path minus
			// final extension): lib/foo/bar.rb → spec/lib/foo/bar_spec.rb.
			name: "declared ruby relpath convention flags missing spec",
			conventions: append(append([]testConvention{}, defaultTestConventions...),
				testConvention{Match: "lib/**/*.rb", Candidates: []string{"spec/{relpath}_spec.rb"}}),
			scope:    []plan.ScopeFile{{Path: "lib/foo/bar.rb", Operation: plan.FileOpModify}},
			listings: map[string][]string{"spec/lib/foo": {"bar_spec.rb"}},
			want: []TestSweepFinding{
				{
					Rule:         testSweepRuleStemSibling,
					TriggerPath:  "lib/foo/bar.rb",
					MissingTests: []string{"spec/lib/foo/bar_spec.rb"},
				},
			},
		},
		{
			// Rule 2 for a non-Go shape: a declared Python convention makes
			// test_*.py a recognized test file, so a CREATE of a new
			// tests/test_new.py in a directory with other test_*.py files
			// surfaces the existing ones.
			name: "rule 2 fires for a non-go test shape",
			conventions: append(append([]testConvention{}, defaultTestConventions...),
				testConvention{Match: "src/**/*.py", Candidates: []string{"tests/test_{name}.py"}}),
			scope:    []plan.ScopeFile{{Path: "tests/test_new.py", Operation: plan.FileOpCreate}},
			listings: map[string][]string{"tests": {"test_alpha.py", "test_new.py", "conftest.py"}},
			want: []TestSweepFinding{
				{
					Rule:         testSweepRuleNewTestInTestedPackage,
					TriggerPath:  "tests/test_new.py",
					MissingTests: []string{"tests/test_alpha.py"},
				},
			},
		},
		// --- #3203 generated_surface rows. Each case is one shipped
		// behavior of the canonical-source -> derived-file table.
		{
			// AC1: the OpenAPI document is rendered into the site api.md
			// Reference region by scripts/gen-site-reference.
			name:     "openapi document without the api.md region flags generated_surface",
			scope:    []plan.ScopeFile{{Path: "docs/api/v0.openapi.yaml", Operation: plan.FileOpModify}},
			listings: map[string][]string{},
			want: []TestSweepFinding{
				{
					Rule:         testSweepRuleGeneratedSurface,
					TriggerPath:  "docs/api/v0.openapi.yaml",
					MissingTests: []string{"site/src/content/docs/reference/api.md"},
					Generator:    testSweepGeneratorSiteReference,
				},
			},
		},
		{
			// AC2: the work-management schema's sync-schemas case arm routes
			// TWO mirrors (backend since #1005, cli since E54.11 / #2801);
			// both must be named in one finding.
			name:     "work-management schema without either mirror names both and the generator",
			scope:    []plan.ScopeFile{{Path: "docs/spec/work-management-v0.schema.json", Operation: plan.FileOpModify}},
			listings: map[string][]string{},
			want: []TestSweepFinding{
				{
					Rule:        testSweepRuleGeneratedSurface,
					TriggerPath: "docs/spec/work-management-v0.schema.json",
					MissingTests: []string{
						"backend/internal/workmgmt/schemas/work-management-v0.schema.json",
						"cli/internal/spec/schemas/work-management-v0.schema.json",
					},
					Generator: testSweepGeneratorSyncSchemas,
				},
			},
		},
		{
			// AC3a: no false positive — a plan already scoping the derived
			// site region draws nothing.
			name: "openapi with the api.md region in scope draws no finding",
			scope: []plan.ScopeFile{
				{Path: "docs/api/v0.openapi.yaml", Operation: plan.FileOpModify},
				{Path: "site/src/content/docs/reference/api.md", Operation: plan.FileOpModify},
			},
			listings: map[string][]string{},
			want:     nil,
		},
		{
			// AC3b: a PARTIALLY scoped mirror set reports only the mirror
			// still missing, not the one already scoped.
			name: "partially scoped mirror set reports only the missing mirror",
			scope: []plan.ScopeFile{
				{Path: "docs/spec/work-management-v0.schema.json", Operation: plan.FileOpModify},
				{Path: "backend/internal/workmgmt/schemas/work-management-v0.schema.json", Operation: plan.FileOpModify},
			},
			listings: map[string][]string{},
			want: []TestSweepFinding{
				{
					Rule:         testSweepRuleGeneratedSurface,
					TriggerPath:  "docs/spec/work-management-v0.schema.json",
					MissingTests: []string{"cli/internal/spec/schemas/work-management-v0.schema.json"},
					Generator:    testSweepGeneratorSyncSchemas,
				},
			},
		},
		{
			// The DEDUP-KEY case: workflow-v2.schema.json feeds BOTH
			// generators, so one trigger legitimately draws TWO findings.
			// A (rule, trigger, sub-plan) dedup key would silently drop the
			// second and hide half the gap.
			name:     "canonical schema feeding two generators draws one finding per generator",
			scope:    []plan.ScopeFile{{Path: "docs/spec/workflow-v2.schema.json", Operation: plan.FileOpModify}},
			listings: map[string][]string{},
			want: []TestSweepFinding{
				{
					Rule:         testSweepRuleGeneratedSurface,
					TriggerPath:  "docs/spec/workflow-v2.schema.json",
					MissingTests: []string{"site/src/content/docs/reference/workflow-spec.md"},
					Generator:    testSweepGeneratorSiteReference,
				},
				{
					Rule:        testSweepRuleGeneratedSurface,
					TriggerPath: "docs/spec/workflow-v2.schema.json",
					MissingTests: []string{
						"backend/internal/spec/schemas/workflow-v2.schema.json",
						"cli/internal/spec/schemas/workflow-v2.schema.json",
					},
					Generator: testSweepGeneratorSyncSchemas,
				},
			},
		},
		{
			// The cmdinfo inventory row: the trigger is cmdinfo.go itself,
			// not the directory, so cmdinfo_test.go draws nothing.
			name:     "cmdinfo inventory flags the cli.md region",
			scope:    []plan.ScopeFile{{Path: "cli/internal/cmdinfo/cmdinfo.go", Operation: plan.FileOpModify}},
			listings: map[string][]string{},
			want: []TestSweepFinding{
				{
					Rule:         testSweepRuleGeneratedSurface,
					TriggerPath:  "cli/internal/cmdinfo/cmdinfo.go",
					MissingTests: []string{"site/src/content/docs/reference/cli.md"},
					Generator:    testSweepGeneratorSiteReference,
				},
			},
		},
		{
			// cmdinfo_test.go is a recognized Go test file, so it never
			// triggers the row; with no listing it draws nothing at all.
			name:     "cmdinfo test file is not a generated_surface trigger",
			scope:    []plan.ScopeFile{{Path: "cli/internal/cmdinfo/cmdinfo_test.go", Operation: plan.FileOpModify}},
			listings: map[string][]string{},
			want:     nil,
		},
		{
			// Scope-set-only: the path-trigger rules consult no directory
			// listing, so a generated_surface row fires with an EMPTY
			// listings map (every Contents API call having failed open).
			name:     "generated_surface fires with an empty listings map",
			scope:    []plan.ScopeFile{{Path: "docs/spec/operator-role-default.yaml", Operation: plan.FileOpModify}},
			listings: map[string][]string{},
			want: []TestSweepFinding{
				{
					Rule:         testSweepRuleGeneratedSurface,
					TriggerPath:  "docs/spec/operator-role-default.yaml",
					MissingTests: []string{"backend/internal/operatorrole/defaults/operator-role-default.yaml"},
					Generator:    testSweepGeneratorSyncSchemas,
				},
			},
		},
		{
			// A docs/spec/ path with no row draws no generated_surface
			// finding — the triggers are literals, not a docs/spec/* glob.
			name:     "unrelated docs/spec path draws no generated_surface finding",
			scope:    []plan.ScopeFile{{Path: "docs/spec/workflow-v2.md", Operation: plan.FileOpModify}},
			listings: map[string][]string{},
			want:     nil,
		},
		{
			// #1004 amendment 2: a declared convention overlapping a default
			// (same Go match+candidate) must not double-report — the
			// candidate set + finding dedup collapse it to a single finding.
			name: "declared convention overlapping a default yields single finding",
			conventions: append(append([]testConvention{}, defaultTestConventions...),
				testConvention{Match: "**/*.go", Candidates: []string{"{dir}/{name}_test.go"}}),
			scope:    []plan.ScopeFile{{Path: dir + "/upload.go", Operation: plan.FileOpModify}},
			listings: map[string][]string{dir: {"upload.go", "upload_test.go"}},
			want: []TestSweepFinding{
				{
					Rule:         testSweepRuleStemSibling,
					TriggerPath:  dir + "/upload.go",
					MissingTests: []string{dir + "/upload_test.go"},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conventions := tt.conventions
			if conventions == nil {
				conventions = defaultTestConventions
			}
			got := evaluateTestSweep(tt.scope, tt.listings, conventions)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("evaluateTestSweep() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestExpandCandidate covers all four template variables of the
// data-driven candidate generator (#1004).
func TestExpandCandidate(t *testing.T) {
	cases := []struct {
		name string
		tmpl string
		path string
		want string
	}{
		{"dir and name colocated go", "{dir}/{name}_test.go", "backend/internal/server/upload.go", "backend/internal/server/upload_test.go"},
		{"name and ext colocated ts", "{dir}/{name}.test.{ext}", "frontend/src/Foo.tsx", "frontend/src/Foo.test.tsx"},
		{"name into a parallel tree", "tests/test_{name}.py", "src/pkg/mod.py", "tests/test_mod.py"},
		{"relpath into a parallel tree", "spec/{relpath}_spec.rb", "lib/foo/bar.rb", "spec/lib/foo/bar_spec.rb"},
		{"subdir candidate", "{dir}/__tests__/{name}.test.{ext}", "frontend/src/Foo.tsx", "frontend/src/__tests__/Foo.test.tsx"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := expandCandidate(tc.tmpl, tc.path); got != tc.want {
				t.Errorf("expandCandidate(%q, %q) = %q, want %q", tc.tmpl, tc.path, got, tc.want)
			}
		})
	}
}

// contentsFake is an httptest fake of the GitHub Contents API directory
// listing: dirs maps a repo-relative directory path to the file names it
// contains; every other path 404s. It records the requested paths so
// tests can assert the directory cap.
type contentsFake struct {
	mu        sync.Mutex
	dirs      map[string][]string
	requested []string
	status    int // non-zero forces this status on every request
}

func (cf *contentsFake) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/contents/{path...}",
		func(w http.ResponseWriter, r *http.Request) {
			dir := r.PathValue("path")
			cf.mu.Lock()
			cf.requested = append(cf.requested, dir)
			names, ok := cf.dirs[dir]
			status := cf.status
			cf.mu.Unlock()
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			entries := make([]map[string]string, 0, len(names))
			for _, n := range names {
				entries = append(entries, map[string]string{
					"name": n, "path": dir + "/" + n, "type": "file",
				})
			}
			_ = json.NewEncoder(w).Encode(entries)
		})
	return mux
}

func (cf *contentsFake) requestCount() int {
	cf.mu.Lock()
	defer cf.mu.Unlock()
	return len(cf.requested)
}

// newTestSweepGitHub wires a githubclient.Client against the contents
// fake, mirroring lineage_test.go's stub-client shape.
func newTestSweepGitHub(t *testing.T, cf *contentsFake) *githubclient.Client {
	t.Helper()
	srv := httptest.NewServer(cf.handler())
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
}

// newTestSweepServer wires a Server whose run carries an installation id
// and whose cfg.GitHub points at the contents fake — the minimum
// runTestSweep needs beyond the scope-precheck server.
func newTestSweepServer(t *testing.T, cf *contentsFake) (*Server, *auditFake, uuid.UUID) {
	t.Helper()
	s, au, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
	instID := int64(42)
	runRow.InstallationID = &instID
	s.cfg.GitHub = newTestSweepGitHub(t, cf)
	return s, au, runRow.ID
}

// lastTestSweepEntry decodes the single plan_test_sweep payload the audit
// fake captured, failing the test when none was written.
func lastTestSweepEntry(t *testing.T, au *auditFake) TestSweepPayload {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var payloads []TestSweepPayload
	for _, ap := range au.appended {
		if ap.Category != categoryPlanTestSweep {
			continue
		}
		var p TestSweepPayload
		if err := json.Unmarshal(ap.Payload, &p); err != nil {
			t.Fatalf("unmarshal test sweep payload: %v", err)
		}
		payloads = append(payloads, p)
	}
	if len(payloads) != 1 {
		t.Fatalf("want exactly 1 plan_test_sweep entry, got %d", len(payloads))
	}
	return payloads[0]
}

func countTestSweepEntries(au *auditFake) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	n := 0
	for _, ap := range au.appended {
		if ap.Category == categoryPlanTestSweep {
			n++
		}
	}
	return n
}

// TestRunTestSweep_WritesFindings drives the Server-level writer and
// asserts a single plan_test_sweep entry with the expected stem-sibling
// finding and listing counters.
func TestRunTestSweep_WritesFindings(t *testing.T) {
	cf := &contentsFake{dirs: map[string][]string{
		"backend/internal/server": {"upload.go", "upload_test.go"},
	}}
	s, au, runID := newTestSweepServer(t, cf)
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: "backend/internal/server/upload.go", Operation: plan.FileOpModify},
	})

	got := s.runTestSweep(context.Background(), runID, runID, body)
	if got == nil {
		t.Fatal("want a non-nil result when the sweep ran")
	}

	recorded := lastTestSweepEntry(t, au)
	if recorded.ScannedFiles != 1 || recorded.ListedDirs != 1 {
		t.Errorf("ScannedFiles/ListedDirs = %d/%d, want 1/1", recorded.ScannedFiles, recorded.ListedDirs)
	}
	if len(recorded.Findings) != 1 {
		t.Fatalf("want 1 finding, got %+v", recorded.Findings)
	}
	f := recorded.Findings[0]
	if f.Rule != testSweepRuleStemSibling || f.TriggerPath != "backend/internal/server/upload.go" {
		t.Errorf("finding = %+v", f)
	}
	if len(f.MissingTests) != 1 || f.MissingTests[0] != "backend/internal/server/upload_test.go" {
		t.Errorf("MissingTests = %v, want [upload_test.go]", f.MissingTests)
	}

	// #963-style return contract: the returned payload equals the recorded
	// audit payload, so handleShipPlan can thread it without a read-back.
	gotJSON, _ := json.Marshal(got)
	recordedJSON, _ := json.Marshal(recorded)
	if string(gotJSON) != string(recordedJSON) {
		t.Errorf("returned result diverges from the recorded audit payload:\nreturned: %s\nrecorded: %s", gotJSON, recordedJSON)
	}
}

// TestRunTestSweep_CleanWritesEmptyFindings verifies the "checked and
// clean" contract: a plan whose scoped directories carry no missing tests
// still writes an entry, and Findings marshals as [] not null.
func TestRunTestSweep_CleanWritesEmptyFindings(t *testing.T) {
	cf := &contentsFake{dirs: map[string][]string{
		"backend/internal/server": {"upload.go"},
	}}
	s, au, runID := newTestSweepServer(t, cf)
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: "backend/internal/server/upload.go", Operation: plan.FileOpModify},
	})

	s.runTestSweep(context.Background(), runID, runID, body)

	got := lastTestSweepEntry(t, au)
	if len(got.Findings) != 0 {
		t.Fatalf("want zero findings on a clean sweep; got %+v", got.Findings)
	}

	au.mu.Lock()
	defer au.mu.Unlock()
	for _, ap := range au.appended {
		if ap.Category != categoryPlanTestSweep {
			continue
		}
		if got := string(ap.Payload); !strings.Contains(got, `"findings":[]`) {
			t.Errorf("payload should encode findings as []; got %s", got)
		}
	}
}

// TestRunTestSweep_FailOpen covers the no-entry degradation paths: nil
// GitHub client, nil installation id, and every listing failing. Each
// must produce a nil result, no audit entry, and no panic.
func TestRunTestSweep_FailOpen(t *testing.T) {
	body := func(t *testing.T) []byte {
		return scopePlanBody(t, []plan.ScopeFile{
			{Path: "backend/internal/server/upload.go", Operation: plan.FileOpModify},
		})
	}

	t.Run("nil github client", func(t *testing.T) {
		s, au, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
		instID := int64(42)
		runRow.InstallationID = &instID
		// cfg.GitHub stays nil.
		if got := s.runTestSweep(context.Background(), runRow.ID, runRow.ID, body(t)); got != nil {
			t.Fatalf("fail-open must return nil; got %+v", got)
		}
		if n := countTestSweepEntries(au); n != 0 {
			t.Fatalf("want no entry, got %d", n)
		}
	})

	t.Run("nil installation id", func(t *testing.T) {
		cf := &contentsFake{dirs: map[string][]string{}}
		s, au, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
		s.cfg.GitHub = newTestSweepGitHub(t, cf)
		// runRow.InstallationID stays nil (non-GitHub trigger).
		if got := s.runTestSweep(context.Background(), runRow.ID, runRow.ID, body(t)); got != nil {
			t.Fatalf("fail-open must return nil; got %+v", got)
		}
		if n := countTestSweepEntries(au); n != 0 {
			t.Fatalf("want no entry, got %d", n)
		}
	})

	t.Run("every listing fails", func(t *testing.T) {
		cf := &contentsFake{dirs: map[string][]string{}, status: http.StatusInternalServerError}
		s, au, runID := newTestSweepServer(t, cf)
		if got := s.runTestSweep(context.Background(), runID, runID, body(t)); got != nil {
			t.Fatalf("fail-open must return nil when no listing succeeded; got %+v", got)
		}
		if n := countTestSweepEntries(au); n != 0 {
			t.Fatalf("want no entry, got %d", n)
		}
	})

	t.Run("nil audit repo", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		if got := s.runTestSweep(context.Background(), uuid.New(), uuid.New(), body(t)); got != nil {
			t.Fatalf("fail-open must return nil; got %+v", got)
		}
	})
}

// TestRunTestSweep_PartialListingFailureStillRecords pins the per-call
// fail-open contract: one directory 404s, the other lists fine — the
// sweep records an entry whose ListedDirs reflects only the successful
// listing and whose findings come from it.
func TestRunTestSweep_PartialListingFailureStillRecords(t *testing.T) {
	cf := &contentsFake{dirs: map[string][]string{
		"backend/internal/server": {"upload.go", "upload_test.go"},
		// "backend/internal/gone" absent → 404.
	}}
	s, au, runID := newTestSweepServer(t, cf)
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: "backend/internal/server/upload.go", Operation: plan.FileOpModify},
		{Path: "backend/internal/gone/gone.go", Operation: plan.FileOpModify},
	})

	got := s.runTestSweep(context.Background(), runID, runID, body)
	if got == nil {
		t.Fatal("partial failure must still produce a result")
	}
	recorded := lastTestSweepEntry(t, au)
	if recorded.ListedDirs != 1 {
		t.Errorf("ListedDirs = %d, want 1 (one listing failed open)", recorded.ListedDirs)
	}
	if len(recorded.Findings) != 1 || recorded.Findings[0].TriggerPath != "backend/internal/server/upload.go" {
		t.Errorf("Findings = %+v, want the surviving directory's stem-sibling finding", recorded.Findings)
	}
}

// TestRunTestSweep_DirectoryCap asserts the directory beyond the cap is
// skipped without error: only testSweepMaxDirs listings are requested and
// the sweep still records.
func TestRunTestSweep_DirectoryCap(t *testing.T) {
	dirs := map[string][]string{}
	var files []plan.ScopeFile
	for i := 0; i < testSweepMaxDirs+1; i++ {
		d := fmt.Sprintf("backend/internal/p%02d", i)
		dirs[d] = []string{"x.go"}
		files = append(files, plan.ScopeFile{Path: d + "/x.go", Operation: plan.FileOpModify})
	}
	cf := &contentsFake{dirs: dirs}
	s, au, runID := newTestSweepServer(t, cf)

	got := s.runTestSweep(context.Background(), runID, runID, scopePlanBody(t, files))
	if got == nil {
		t.Fatal("capped sweep must still produce a result")
	}
	if n := cf.requestCount(); n != testSweepMaxDirs {
		t.Errorf("contents requests = %d, want %d (cap)", n, testSweepMaxDirs)
	}
	recorded := lastTestSweepEntry(t, au)
	if recorded.ListedDirs != testSweepMaxDirs {
		t.Errorf("ListedDirs = %d, want %d", recorded.ListedDirs, testSweepMaxDirs)
	}
}

// specPythonConventions declares a Python parallel-tree test convention
// (src/**/*.py → tests/test_{name}.py) on top of the built-in defaults,
// for the runTestSweep-level wiring tests.
var specPythonConventions = []byte(`version: "0.3"
test_conventions:
  - match: "src/**/*.py"
    candidates:
      - "tests/test_{name}.py"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`)

// TestRunTestSweep_DeclaredConventionDrivesFinding is the #1004 amendment-1
// cross-boundary integration test for the load-bearing seam
// runRow.WorkflowSpec → spec.ParseBytes → declared TestConventions →
// evaluateTestSweep. It seeds a WorkflowSpec declaring a Python convention
// and asserts a finding driven by the DECLARED convention (not a default),
// proving the parse+thread wiring works.
func TestRunTestSweep_DeclaredConventionDrivesFinding(t *testing.T) {
	cf := &contentsFake{dirs: map[string][]string{
		"tests": {"test_mod.py"},
	}}
	s, au, runRow := newScopePrecheckServer(t, specPythonConventions)
	instID := int64(42)
	runRow.InstallationID = &instID
	s.cfg.GitHub = newTestSweepGitHub(t, cf)
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: "src/pkg/mod.py", Operation: plan.FileOpModify},
	})

	got := s.runTestSweep(context.Background(), runRow.ID, runRow.ID, body)
	if got == nil {
		t.Fatal("want a non-nil result when the sweep ran")
	}
	recorded := lastTestSweepEntry(t, au)
	if len(recorded.Findings) != 1 {
		t.Fatalf("want 1 finding driven by the declared convention, got %+v", recorded.Findings)
	}
	f := recorded.Findings[0]
	if f.Rule != testSweepRuleStemSibling || f.TriggerPath != "src/pkg/mod.py" {
		t.Errorf("finding = %+v, want a stem_sibling for src/pkg/mod.py", f)
	}
	if len(f.MissingTests) != 1 || f.MissingTests[0] != "tests/test_mod.py" {
		t.Errorf("MissingTests = %v, want [tests/test_mod.py]", f.MissingTests)
	}
}

// TestRunTestSweep_FailsOpenToDefaultsOnBadSpec pins the #1004 amendment-1
// degradation contract: an empty or unparseable WorkflowSpec falls open to
// the built-in defaults (WARN-logged, never blocks), so the Go default
// stem-sibling rule still fires.
func TestRunTestSweep_FailsOpenToDefaultsOnBadSpec(t *testing.T) {
	cases := []struct {
		name string
		spec []byte
	}{
		{"empty workflow spec", nil},
		{"malformed workflow spec", []byte("version: \"0.3\"\nworkflows: [oops")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cf := &contentsFake{dirs: map[string][]string{
				"backend/internal/server": {"upload.go", "upload_test.go"},
			}}
			s, au, runRow := newScopePrecheckServer(t, tc.spec)
			instID := int64(42)
			runRow.InstallationID = &instID
			s.cfg.GitHub = newTestSweepGitHub(t, cf)
			body := scopePlanBody(t, []plan.ScopeFile{
				{Path: "backend/internal/server/upload.go", Operation: plan.FileOpModify},
			})

			got := s.runTestSweep(context.Background(), runRow.ID, runRow.ID, body)
			if got == nil {
				t.Fatal("sweep must still run on a bad spec (fail open to defaults)")
			}
			recorded := lastTestSweepEntry(t, au)
			if len(recorded.Findings) != 1 || recorded.Findings[0].Rule != testSweepRuleStemSibling {
				t.Fatalf("want the Go default stem_sibling finding, got %+v", recorded.Findings)
			}
		})
	}
}

// TestRunTestSweep_SubPlanScopeAttributed covers #1077: a decomposition
// sub-plan that scopes a migration without the pinned postgres_test.go
// yields a migration_walk finding attributed to that sub-plan, while the
// flat parent scope stays clean. migration_walk is scope-set-only, so the
// finding fires with no directory listing needed.
func TestRunTestSweep_SubPlanScopeAttributed(t *testing.T) {
	cf := &contentsFake{dirs: map[string][]string{}}
	s, au, runID := newTestSweepServer(t, cf)
	body := decomposedScopePlanBody(t,
		[]plan.ScopeFile{{Path: "docs/ARCHITECTURE.md", Operation: plan.FileOpModify}},
		[]subPlanScope{
			{
				title: "migration slice",
				files: []plan.ScopeFile{{Path: "backend/internal/postgres/migrations/0032_x.up.sql", Operation: plan.FileOpCreate}},
			},
			{
				title: "doc slice",
				files: []plan.ScopeFile{{Path: "README.md", Operation: plan.FileOpModify}},
			},
		},
	)

	got := s.runTestSweep(context.Background(), runID, runID, body)
	if got == nil {
		t.Fatal("want a non-nil result when the sweep ran")
	}
	recorded := lastTestSweepEntry(t, au)
	var found *TestSweepFinding
	for i := range recorded.Findings {
		if recorded.Findings[i].SubPlanTitle == "" {
			t.Errorf("unexpected parent-scope finding: %+v", recorded.Findings[i])
			continue
		}
		if recorded.Findings[i].SubPlanTitle == "migration slice" {
			found = &recorded.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("want a finding attributed to the migration sub-plan; got %+v", recorded.Findings)
	}
	if found.Rule != testSweepRuleMigrationWalk {
		t.Errorf("Rule = %q, want %s", found.Rule, testSweepRuleMigrationWalk)
	}
	if len(found.MissingTests) != 1 || found.MissingTests[0] != "backend/internal/postgres/postgres_test.go" {
		t.Errorf("MissingTests = %v", found.MissingTests)
	}
}

// TestGeneratedSurfaceRowsExistOnDisk is binding condition 4: EVERY
// trigger and EVERY required path of EVERY testSweepPathTriggerRules row
// must exist on disk (#3203). The rows are a hand-maintained mirror of
// two generators this package cannot import — cli/internal/docgen (cli
// module) and the scripts/sync-schemas shell script — so a rename of a
// canonical source, a docgen page constant or an embedded mirror would
// otherwise silently disable a row. Modelled on
// TestSurfacePatternsExistOnDisk; paths are repo-relative and this test
// runs from backend/internal/server, so the repo root is three levels up.
//
// Residual, stated rather than papered over: this catches a RENAME, not
// a canonical source ADDED to a generator with no matching row here.
func TestGeneratedSurfaceRowsExistOnDisk(t *testing.T) {
	const repoRoot = "../../.."
	seen := map[string]bool{}
	check := func(p string) {
		if seen[p] {
			return
		}
		seen[p] = true
		abs := filepath.Join(repoRoot, filepath.FromSlash(p))
		if _, err := os.Stat(abs); err != nil {
			t.Errorf("path-trigger row path %q does not exist on disk (%v) — a rename would silently disable the row", p, err)
		}
	}
	rows := 0
	for _, r := range testSweepPathTriggerRules {
		rows++
		// The migration_walk row's trigger is a GLOB (migrations/*.sql), the
		// only non-literal in the table; its required path is still checked.
		if !strings.ContainsAny(r.TriggerGlob, "*?[") {
			check(r.TriggerGlob)
		}
		for _, req := range r.RequiredPaths {
			check(req)
		}
	}
	if rows != len(testSweepPathTriggerRules) || rows == 0 {
		t.Fatalf("swept %d rows, table has %d", rows, len(testSweepPathTriggerRules))
	}
	// Every generated_surface row must carry a generator, and it must be
	// one of the two shipped commands — a row with an empty or unknown
	// generator would render as an unactionable finding.
	for _, r := range testSweepPathTriggerRules {
		if r.Rule != testSweepRuleGeneratedSurface {
			if r.Generator != "" {
				t.Errorf("non-generated_surface row %q carries generator %q — that would change its rendered line", r.TriggerGlob, r.Generator)
			}
			continue
		}
		switch r.Generator {
		case testSweepGeneratorSiteReference, testSweepGeneratorSyncSchemas:
		default:
			t.Errorf("generated_surface row %q has generator %q, want one of the two shipped commands", r.TriggerGlob, r.Generator)
		}
	}
}

// TestRunTestSweep_SubPlanGeneratedSurfaceAttributed is the sub-plan
// attribution case for the new rule (#3203): a decomposition slice that
// scopes the OpenAPI document without the derived site region draws a
// generated_surface finding tagged with the slice title. Like
// migration_walk the rule is scope-set-only, so it fires with no
// directory listing.
func TestRunTestSweep_SubPlanGeneratedSurfaceAttributed(t *testing.T) {
	cf := &contentsFake{dirs: map[string][]string{}}
	s, au, runID := newTestSweepServer(t, cf)
	body := decomposedScopePlanBody(t,
		[]plan.ScopeFile{{Path: "docs/ARCHITECTURE.md", Operation: plan.FileOpModify}},
		[]subPlanScope{
			{
				title: "api slice",
				files: []plan.ScopeFile{{Path: "docs/api/v0.openapi.yaml", Operation: plan.FileOpModify}},
			},
			{
				title: "doc slice",
				files: []plan.ScopeFile{{Path: "README.md", Operation: plan.FileOpModify}},
			},
		},
	)

	if got := s.runTestSweep(context.Background(), runID, runID, body); got == nil {
		t.Fatal("want a non-nil result when the sweep ran")
	}
	recorded := lastTestSweepEntry(t, au)
	var found *TestSweepFinding
	for i := range recorded.Findings {
		if recorded.Findings[i].SubPlanTitle == "api slice" {
			found = &recorded.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("want a finding attributed to the api sub-plan; got %+v", recorded.Findings)
	}
	if found.Rule != testSweepRuleGeneratedSurface {
		t.Errorf("Rule = %q, want %s", found.Rule, testSweepRuleGeneratedSurface)
	}
	if found.Generator != testSweepGeneratorSiteReference {
		t.Errorf("Generator = %q, want %s", found.Generator, testSweepGeneratorSiteReference)
	}
	if len(found.MissingTests) != 1 || found.MissingTests[0] != "site/src/content/docs/reference/api.md" {
		t.Errorf("MissingTests = %v", found.MissingTests)
	}
}

// TestShipPlan_TestSweep_EndToEnd is the #618-rule cross-boundary check
// for this feature: a plan POSTed through handleShipPlan, with an
// httptest-fake Contents API wired into cfg.GitHub, must (a) append a
// plan_test_sweep audit entry carrying the expected findings JSON, and
// (b) surface the rendered test-sweep gate-evidence block in the captured
// plan-review prompt — exercising the upload → sweep → audit → prompt
// seam end to end rather than per-layer units.
func TestShipPlan_TestSweep_EndToEnd(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-sonnet-4-6",
	}
	s, sf, _, au, rr := newPlanServerWithReviewer(t, runID, stageID, reviewer, specGatingReviewersWithConstraints)
	cf := &contentsFake{dirs: map[string][]string{
		"backend/internal/server": {"upload.go", "upload_test.go"},
	}}
	instID := int64(42)
	rr.getRuns[runID].InstallationID = &instID
	s.cfg.GitHub = newTestSweepGitHub(t, cf)
	priv, _ := sf.issue(t, runID)
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: "backend/internal/server/upload.go", Operation: plan.FileOpModify},
	})

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	// (a) the audit entry landed with the expected finding.
	recorded := lastTestSweepEntry(t, au)
	if len(recorded.Findings) != 1 || recorded.Findings[0].Rule != testSweepRuleStemSibling {
		t.Fatalf("Findings = %+v, want one stem_sibling finding", recorded.Findings)
	}

	// (b) the captured plan-review prompt renders the test-sweep block.
	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if len(reviewer.calls) != 1 {
		t.Fatalf("reviewer calls = %d, want 1", len(reviewer.calls))
	}
	got := reviewer.calls[0]
	wants := []string{
		"### Gate evidence (machine-verified — outranks text-level findings)",
		"Test sweep (existing *_test.go files adjacent to the planned change",
		"EXISTING TESTS NOT IN SCOPE (stem_sibling)",
		"backend/internal/server/upload_test.go",
		"the runner will scope_drift-exclude the agent's edits",
	}
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("plan-review prompt missing test-sweep element %q — threading seam broken:\n%s", want, got)
		}
	}
}

// planTestSweepWireFixturePath is the SHARED cross-boundary wire fixture
// for the #3203 generated_surface finding (binding approval condition 1,
// the #2660/#3014 precedent). This test writes nothing at run time: it
// asserts the RAW plan_test_sweep audit payload the server persisted
// equals the committed file, and
// backend/internal/mcpserver/tools_test.go reads the SAME file and
// decodes it through the real MCP wire types. One file, two readers: a
// json-tag rename or drop on EITHER side reddens the pair, which two
// hand-built fixtures hoping to agree cannot do.
const planTestSweepWireFixturePath = "testdata/plan_test_sweep_wire.json"

// rawTestSweepPayload returns the RAW bytes of the single plan_test_sweep
// audit payload the fake captured — not a decoded struct, because the
// fixture's whole point is that the persisted BYTES are what the MCP side
// decodes.
func rawTestSweepPayload(t *testing.T, au *auditFake) []byte {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var out [][]byte
	for _, ap := range au.appended {
		if ap.Category == categoryPlanTestSweep {
			out = append(out, ap.Payload)
		}
	}
	if len(out) != 1 {
		t.Fatalf("want exactly 1 plan_test_sweep entry, got %d", len(out))
	}
	return out[0]
}

// TestShipPlan_TestSweep_GeneratedSurface_EndToEnd is the cross-boundary
// half for #3203: a plan POSTed through handleShipPlan scoping the
// canonical workflow-v2 schema — the TWO-GENERATOR trigger — must
// (a) persist a plan_test_sweep payload carrying both generated_surface
// findings with their generators, (b) pin those exact bytes into the
// shared wire fixture the mcpserver decode test consumes, and (c) surface
// the generated_surface line, naming the derived file AND the generator,
// in the captured plan-review prompt. Upload -> sweep -> audit -> prompt
// and upload -> audit -> MCP, each asserted end to end.
func TestShipPlan_TestSweep_GeneratedSurface_EndToEnd(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-sonnet-4-6",
	}
	s, sf, _, au, rr := newPlanServerWithReviewer(t, runID, stageID, reviewer, specGatingReviewersWithConstraints)
	// No directory listings at all: the path-trigger rules are
	// scope-set-only, so the finding must fire regardless.
	cf := &contentsFake{dirs: map[string][]string{}}
	instID := int64(42)
	rr.getRuns[runID].InstallationID = &instID
	s.cfg.GitHub = newTestSweepGitHub(t, cf)
	priv, _ := sf.issue(t, runID)
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: "docs/spec/workflow-v2.schema.json", Operation: plan.FileOpModify},
	})

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	// (a) both generated_surface findings landed, one per generator.
	recorded := lastTestSweepEntry(t, au)
	if len(recorded.Findings) != 2 {
		t.Fatalf("Findings = %+v, want two generated_surface findings (one per generator)", recorded.Findings)
	}
	gens := map[string]string{}
	for _, f := range recorded.Findings {
		if f.Rule != testSweepRuleGeneratedSurface {
			t.Errorf("Rule = %q, want %s", f.Rule, testSweepRuleGeneratedSurface)
		}
		gens[f.Generator] = strings.Join(f.MissingTests, ",")
	}
	if got := gens[testSweepGeneratorSiteReference]; got != "site/src/content/docs/reference/workflow-spec.md" {
		t.Errorf("site-reference finding missing_tests = %q", got)
	}
	if got := gens[testSweepGeneratorSyncSchemas]; got != "backend/internal/spec/schemas/workflow-v2.schema.json,cli/internal/spec/schemas/workflow-v2.schema.json" {
		t.Errorf("sync-schemas finding missing_tests = %q", got)
	}

	// (b) the persisted BYTES equal the committed shared fixture, which is
	// the same file the mcpserver decode test reads.
	raw := rawTestSweepPayload(t, au)
	wantFixture, err := os.ReadFile(planTestSweepWireFixturePath)
	if err != nil {
		t.Fatalf("read wire fixture: %v", err)
	}
	var gotCompact, wantCompact bytes.Buffer
	if err := json.Compact(&gotCompact, raw); err != nil {
		t.Fatalf("compact persisted payload: %v", err)
	}
	if err := json.Compact(&wantCompact, wantFixture); err != nil {
		t.Fatalf("compact fixture: %v", err)
	}
	if gotCompact.String() != wantCompact.String() {
		t.Errorf("persisted plan_test_sweep payload does not match %s\n got: %s\nwant: %s",
			planTestSweepWireFixturePath, gotCompact.String(), wantCompact.String())
	}

	// (c) the plan-review prompt renders the generated_surface line with
	// the derived file and the generator.
	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if len(reviewer.calls) != 1 {
		t.Fatalf("reviewer calls = %d, want 1", len(reviewer.calls))
	}
	got := reviewer.calls[0]
	wants := []string{
		"### Gate evidence (machine-verified — outranks text-level findings)",
		"GENERATED SURFACE NOT IN SCOPE (generated_surface)",
		"site/src/content/docs/reference/workflow-spec.md (regenerate with `scripts/gen-site-reference`)",
		"cli/internal/spec/schemas/workflow-v2.schema.json (regenerate with `scripts/sync-schemas`)",
		"the implement stage would have to spend one of its two scope amendments mid-stage",
	}
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("plan-review prompt missing generated_surface element %q — threading seam broken:\n%s", want, got)
		}
	}
}

// TestShipPlan_TestSweep_FailOpenUploadStillSucceeds asserts the upload
// path never depends on the sweep: with no GitHub client wired the POST
// still returns 201 and no plan_test_sweep entry is written.
func TestShipPlan_TestSweep_FailOpenUploadStillSucceeds(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-sonnet-4-6",
	}
	s, sf, _, au, _ := newPlanServerWithReviewer(t, runID, stageID, reviewer, specGatingReviewersWithConstraints)
	// cfg.GitHub stays nil; the run keeps a nil InstallationID.
	priv, _ := sf.issue(t, runID)
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: "backend/internal/server/upload.go", Operation: plan.FileOpModify},
	})

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if n := countTestSweepEntries(au); n != 0 {
		t.Fatalf("want no plan_test_sweep entry on fail-open, got %d", n)
	}
}
