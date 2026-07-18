package constraint

import (
	"strings"
	"testing"
)

func diff(files ...string) Diff {
	d := Diff{}
	for _, f := range files {
		// Default to modified status; tests that need other
		// statuses build the slice manually.
		d.ChangedFiles = append(d.ChangedFiles, ChangedFile{Path: f, Status: StatusModified})
	}
	return d
}

func TestEvaluate_Empty_NoConstraintsConfigured(t *testing.T) {
	v := Evaluate(diff("a.go"), Constraints{})
	if len(v) != 0 {
		t.Errorf("expected no violations, got %+v", v)
	}
}

func TestForbiddenPaths_Hit(t *testing.T) {
	d := diff("backend/main.go", "infra/terraform.tf")
	v := Evaluate(d, Constraints{ForbiddenPaths: []string{"infra/**"}})
	if len(v) != 1 {
		t.Fatalf("got %d violations, want 1: %+v", len(v), v)
	}
	if v[0].Constraint != "forbidden_paths" {
		t.Errorf("Constraint = %q", v[0].Constraint)
	}
	if len(v[0].Files) != 1 || v[0].Files[0] != "infra/terraform.tf" {
		t.Errorf("Files = %v, want [infra/terraform.tf]", v[0].Files)
	}
}

func TestForbiddenPaths_NoHit(t *testing.T) {
	d := diff("backend/main.go", "frontend/app.tsx")
	v := Evaluate(d, Constraints{ForbiddenPaths: []string{"infra/**", ".github/workflows/**"}})
	if len(v) != 0 {
		t.Errorf("expected no violations, got %+v", v)
	}
}

func TestForbiddenPaths_InvalidPattern(t *testing.T) {
	d := diff("a.go")
	// `[` opens a character class that's never closed → invalid.
	v := Evaluate(d, Constraints{ForbiddenPaths: []string{"[bad"}})
	if len(v) != 1 || !strings.Contains(v[0].Detail, "invalid") {
		t.Errorf("expected invalid-pattern violation, got %+v", v)
	}
}

func TestAllowedPaths_AllAllowed(t *testing.T) {
	d := diff("backend/main.go", "backend/internal/server/handlers.go")
	v := Evaluate(d, Constraints{AllowedPaths: []string{"backend/**"}})
	if len(v) != 0 {
		t.Errorf("expected no violations, got %+v", v)
	}
}

func TestAllowedPaths_OutsideAllowed(t *testing.T) {
	d := diff("backend/main.go", "frontend/app.tsx")
	v := Evaluate(d, Constraints{AllowedPaths: []string{"backend/**"}})
	if len(v) != 1 {
		t.Fatalf("got %d violations, want 1: %+v", len(v), v)
	}
	if len(v[0].Files) != 1 || v[0].Files[0] != "frontend/app.tsx" {
		t.Errorf("Files = %v", v[0].Files)
	}
}

func TestAllowedPaths_InvalidPattern(t *testing.T) {
	d := diff("a.go")
	v := Evaluate(d, Constraints{AllowedPaths: []string{"[bad"}})
	// Two violations: invalid-pattern note + the file matching nothing.
	if len(v) != 2 {
		t.Fatalf("got %d violations, want 2: %+v", len(v), v)
	}
	if !strings.Contains(v[0].Detail, "invalid") {
		t.Errorf("first violation = %+v, want invalid", v[0])
	}
}

func TestMaxFilesChanged_Under(t *testing.T) {
	d := diff("a.go", "b.go", "c.go")
	v := Evaluate(d, Constraints{MaxFilesChanged: 5})
	if len(v) != 0 {
		t.Errorf("expected no violations, got %+v", v)
	}
}

func TestMaxFilesChanged_Equal(t *testing.T) {
	d := diff("a.go", "b.go", "c.go")
	v := Evaluate(d, Constraints{MaxFilesChanged: 3})
	if len(v) != 0 {
		t.Errorf("equal-to-limit should pass, got %+v", v)
	}
}

func TestMaxFilesChanged_Over(t *testing.T) {
	d := diff("a.go", "b.go", "c.go", "d.go")
	v := Evaluate(d, Constraints{MaxFilesChanged: 3})
	if len(v) != 1 {
		t.Fatalf("got %d violations, want 1: %+v", len(v), v)
	}
	if !strings.Contains(v[0].Detail, "limit 3") {
		t.Errorf("Detail = %q", v[0].Detail)
	}
}

// TestIsGeneratedPath pins the generated/vendored allowlist (#2054):
// sqlc db packages (a .go file under a db/ directory) and vendored deps
// (vendor/) are exempt; hand-written non-db source is not. Mirrors the
// backend policy copy so the two verdicts stay in lockstep.
func TestIsGeneratedPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		// Matching: sqlc db packages.
		{"backend/internal/audit/db/queries.sql.go", true},
		{"backend/internal/audit/db/models.go", true},
		{"db/queries.sql.go", true}, // db/ at repo root
		// Matching: vendored deps.
		{"vendor/github.com/foo/bar/baz.go", true},
		{"backend/vendor/github.com/x/y.go", true},
		// Non-matching: hand-written source that merely mentions "db".
		{"backend/internal/db_helpers.go", false}, // db in filename, not a db/ dir
		{"backend/internal/server/handlers.go", false},
		{"backend/internal/db/notes.md", false}, // under db/ but not a .go file
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := IsGeneratedPath(tc.path); got != tc.want {
				t.Errorf("IsGeneratedPath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestMaxFilesChanged_GeneratedExempt covers the #2054 exemption at the
// max_files_changed gate in both named branches (sqlc db, vendored): a
// diff of only generated files never trips the cap, and a mixed diff
// counts only the non-generated files (reported in the Detail) and fires
// only when THAT count exceeds the cap.
func TestMaxFilesChanged_GeneratedExempt(t *testing.T) {
	// db-only diff under a tiny cap: zero counted files, no violation.
	dbOnly := diff("svc/db/queries.sql.go", "svc/db/models.go", "svc/db/db.go")
	if v := Evaluate(dbOnly, Constraints{MaxFilesChanged: 1}); len(v) != 0 {
		t.Errorf("db-only diff must be exempt under cap 1, got %+v", v)
	}
	// vendor-only diff under a tiny cap: zero counted files, no violation.
	vendorOnly := diff("vendor/a/a.go", "vendor/b/b.go", "vendor/c/c.go")
	if v := Evaluate(vendorOnly, Constraints{MaxFilesChanged: 1}); len(v) != 0 {
		t.Errorf("vendor-only diff must be exempt under cap 1, got %+v", v)
	}
	// N=3 non-generated + M=2 generated under cap 3: counted==3, no violation.
	mixed := diff(
		"backend/a.go", "backend/b.go", "backend/c.go", // 3 counted
		"backend/x/db/queries.sql.go", "vendor/lib/lib.go", // exempt
	)
	if v := Evaluate(mixed, Constraints{MaxFilesChanged: 3}); len(v) != 0 {
		t.Errorf("3 counted under cap 3 must pass despite 2 generated files, got %+v", v)
	}
	// Same mixed diff under cap 2: counted==3 > 2, fires; Detail reports 3.
	v := Evaluate(mixed, Constraints{MaxFilesChanged: 2})
	if len(v) != 1 || v[0].Constraint != "max_files_changed" {
		t.Fatalf("expected one max_files_changed violation, got %+v", v)
	}
	if !strings.Contains(v[0].Detail, "changed 3 files") {
		t.Errorf("Detail must report the exempted count 3, got %q", v[0].Detail)
	}
}

func TestRequiredOutcomes_TestsAddedOrUpdated_Pass(t *testing.T) {
	cases := []string{
		"backend/internal/server/handlers_test.go",
		"frontend/app.test.tsx",
		"tests/integration/api.py",
		"backend/internal/server/test/fixtures.go",
		"src/foo/spec/bar.rb",
		"py/test_thing.py",
		"scripts/test",     // shell test runner
		"scripts/test-dev", // hyphenated script test convention (#601)
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			v := Evaluate(diff(p), Constraints{RequiredOutcomes: []string{"tests_added_or_updated"}})
			if len(v) != 0 {
				t.Errorf("expected pass, got %+v", v)
			}
		})
	}
}

func TestRequiredOutcomes_TestsAddedOrUpdated_Fail(t *testing.T) {
	d := diff("backend/main.go", "frontend/app.tsx")
	v := Evaluate(d, Constraints{RequiredOutcomes: []string{"tests_added_or_updated"}})
	if len(v) != 1 || !strings.Contains(v[0].Detail, "no test files") {
		t.Errorf("expected tests-not-added violation, got %+v", v)
	}
}

// TestRequiredOutcomes_TestsAddedOrUpdated_NonCodeDiffPasses covers
// the #610 fix: a non-empty diff that touches only docs/scripts/config
// (no unit-testable source) is vacuously satisfied. The first case is
// the literal run 679b042c / #601 reproduction.
func TestRequiredOutcomes_TestsAddedOrUpdated_NonCodeDiffPasses(t *testing.T) {
	cases := []struct {
		name string
		d    Diff
	}{
		{"#601 repro: docs + scripts only", Diff{ChangedFiles: []ChangedFile{
			{Path: "CLAUDE.md", Status: StatusModified},
			{Path: "scripts/dev", Status: StatusModified},
			{Path: "scripts/test-dev", Status: StatusAdded},
		}}},
		{"docs-only", Diff{ChangedFiles: []ChangedFile{
			{Path: "docs/ARCHITECTURE.md", Status: StatusModified},
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := Evaluate(tc.d, Constraints{RequiredOutcomes: []string{"tests_added_or_updated"}})
			if len(v) != 0 {
				t.Errorf("expected pass for non-code diff, got %+v", v)
			}
		})
	}
}

// TestRequiredOutcomes_TestsAddedOrUpdated_SourceOnlyStillFails is the
// real-case regression guard: a diff that touches unit-testable source
// (a.go) but adds no test must still fail with the unchanged detail.
func TestRequiredOutcomes_TestsAddedOrUpdated_SourceOnlyStillFails(t *testing.T) {
	d := Diff{ChangedFiles: []ChangedFile{{Path: "a.go", Status: StatusAdded}}}
	v := Evaluate(d, Constraints{RequiredOutcomes: []string{"tests_added_or_updated"}})
	if len(v) != 1 || !strings.Contains(v[0].Detail, "no test files") {
		t.Errorf("expected tests-not-added violation for source-only diff, got %+v", v)
	}
}

func TestRequiredOutcomes_DeletedTestFileDoesntCount(t *testing.T) {
	// A pure-deletion of a test file shouldn't satisfy
	// "tests added or updated" — that's the opposite of what we
	// want.
	d := Diff{ChangedFiles: []ChangedFile{
		{Path: "x_test.go", Status: StatusDeleted},
		{Path: "x.go", Status: StatusModified},
	}}
	v := Evaluate(d, Constraints{RequiredOutcomes: []string{"tests_added_or_updated"}})
	if len(v) != 1 {
		t.Errorf("expected violation when only test was deleted, got %+v", v)
	}
}

func TestRequiredOutcomes_CIGreen_NoSignal(t *testing.T) {
	// When CIGreen is nil, the runner can't verify; recording the
	// gap honestly is the spec-mandated behavior (MVP_SPEC §6
	// "honesty about gaps beats fictional completeness").
	d := diff("a.go")
	v := Evaluate(d, Constraints{RequiredOutcomes: []string{"ci_green"}})
	if len(v) != 1 || !strings.Contains(v[0].Detail, "no signal") {
		t.Errorf("expected no-signal violation, got %+v", v)
	}
}

func TestRequiredOutcomes_CIGreen_True(t *testing.T) {
	green := true
	d := diff("a.go")
	v := Evaluate(d, Constraints{
		RequiredOutcomes: []string{"ci_green"},
		CIGreen:          &green,
	})
	if len(v) != 0 {
		t.Errorf("expected no violations, got %+v", v)
	}
}

func TestRequiredOutcomes_CIGreen_False(t *testing.T) {
	green := false
	d := diff("a.go")
	v := Evaluate(d, Constraints{
		RequiredOutcomes: []string{"ci_green"},
		CIGreen:          &green,
	})
	if len(v) != 1 || !strings.Contains(v[0].Detail, "not green") {
		t.Errorf("expected ci-not-green violation, got %+v", v)
	}
}

func TestRequiredOutcomes_UnknownOutcome(t *testing.T) {
	v := Evaluate(diff("a.go"), Constraints{RequiredOutcomes: []string{"sky_is_blue"}})
	if len(v) != 1 || !strings.Contains(v[0].Detail, "unknown outcome") {
		t.Errorf("expected unknown-outcome violation, got %+v", v)
	}
}

func TestEvaluate_MultipleConstraints(t *testing.T) {
	// The classic feature_change implement stage from MVP_SPEC §4.2.
	d := diff(
		"backend/internal/server/handlers.go",
		"backend/internal/server/handlers_test.go",
		"infra/main.tf", // forbidden
	)
	v := Evaluate(d, Constraints{
		ForbiddenPaths:   []string{"infra/**", ".github/workflows/**", "security/**", ".fishhawk/**"},
		MaxFilesChanged:  30,
		RequiredOutcomes: []string{"tests_added_or_updated"},
	})
	if len(v) != 1 {
		t.Fatalf("expected exactly 1 violation (forbidden_paths), got %+v", v)
	}
	if v[0].Constraint != "forbidden_paths" {
		t.Errorf("Constraint = %q, want forbidden_paths", v[0].Constraint)
	}
}

func TestViolation_String(t *testing.T) {
	v := Violation{Constraint: "k", Detail: "d"}
	if got := v.String(); got != "k: d" {
		t.Errorf("String() = %q", got)
	}
	v2 := Violation{Constraint: "k", Detail: "d", Files: []string{"x", "y"}}
	if got := v2.String(); got != "k: d [x, y]" {
		t.Errorf("String() with files = %q", got)
	}
}

func TestStatusConstants(t *testing.T) {
	// Pin to the git --name-status letters; CI parsers depend on
	// these matching upstream's output.
	pairs := map[Status]string{
		StatusAdded: "A", StatusModified: "M", StatusDeleted: "D",
		StatusRenamed: "R", StatusCopied: "C", StatusTypeChg: "T",
	}
	for s, want := range pairs {
		if string(s) != want {
			t.Errorf("Status %q = %q, want %q", s, string(s), want)
		}
	}
}
