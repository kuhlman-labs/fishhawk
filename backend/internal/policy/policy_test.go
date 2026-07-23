package policy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func diff(files ...string) Diff {
	d := Diff{}
	for _, f := range files {
		d.ChangedFiles = append(d.ChangedFiles, ChangedFile{Path: f, Status: StatusModified})
	}
	return d
}

// TestEvaluate_IgnoresPatch asserts the Patch field (additive content
// for the implement-review prompt, #585) does not influence constraint
// evaluation: the violations are byte-for-byte identical whether or not
// Patch is set. Patch is for downstream consumers ONLY; ChangedFiles is
// the sole constraint input.
func TestEvaluate_IgnoresPatch(t *testing.T) {
	c := Constraints{
		ForbiddenPaths:   []string{"secrets/**"},
		MaxFilesChanged:  2,
		RequiredOutcomes: []string{"tests_added_or_updated"},
	}
	base := diff("a.go", "b.go", "secrets/key.pem")

	withoutPatch := Evaluate(base, c)

	withPatch := base
	withPatch.Patch = "diff --git a/a.go b/a.go\n@@ -1 +1 @@\n-x\n+y\n"
	withPatchViolations := Evaluate(withPatch, c)

	if len(withoutPatch) != len(withPatchViolations) {
		t.Fatalf("violation count differs: without=%d with=%d", len(withoutPatch), len(withPatchViolations))
	}
	for i := range withoutPatch {
		if withoutPatch[i].String() != withPatchViolations[i].String() {
			t.Errorf("violation %d differs:\n without: %s\n with:    %s",
				i, withoutPatch[i].String(), withPatchViolations[i].String())
		}
	}
}

func TestEvaluate_Empty_NoConstraintsConfigured(t *testing.T) {
	v := Evaluate(diff("a.go"), Constraints{})
	if len(v) != 0 {
		t.Errorf("expected no violations, got %+v", v)
	}
}

func TestEvaluate_EmptyDiff_NoConstraints(t *testing.T) {
	v := Evaluate(Diff{}, Constraints{})
	if len(v) != 0 {
		t.Errorf("expected no violations, got %+v", v)
	}
}

func TestEvaluate_EmptyDiff_WithMaxFiles(t *testing.T) {
	v := Evaluate(Diff{}, Constraints{MaxFilesChanged: 10})
	if len(v) != 0 {
		t.Errorf("empty diff under limit should pass, got %+v", v)
	}
}

func TestEvaluate_EmptyDiff_WithRequiredOutcomes(t *testing.T) {
	v := Evaluate(Diff{}, Constraints{RequiredOutcomes: []string{"tests_added_or_updated"}})
	if len(v) != 1 {
		t.Errorf("empty diff should fail tests-added requirement, got %+v", v)
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
	if len(v) != 1 || !strings.Contains(v[0].Detail, "invalid pattern") {
		t.Errorf("expected invalid-pattern violation, got %+v", v)
	}
}

func TestForbiddenPaths_MultiplePatterns(t *testing.T) {
	d := diff("infra/main.tf", ".github/workflows/ci.yml", "backend/main.go")
	v := Evaluate(d, Constraints{ForbiddenPaths: []string{"infra/**", ".github/workflows/**"}})
	if len(v) != 2 {
		t.Fatalf("expected 2 violations (one per matching pattern), got %+v", v)
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
// (vendor/) are exempt; hand-written non-db source is not. Parity with
// CI's coverage --exclude '/db/' (scripts/check-coverage.py), narrowed
// to .go files.
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

func TestRequiredOutcomes_TestsAddedOrUpdated_Fail(t *testing.T) {
	d := diff("backend/main.go", "frontend/app.tsx")
	v := Evaluate(d, Constraints{RequiredOutcomes: []string{"tests_added_or_updated"}})
	if len(v) != 1 || !strings.Contains(v[0].Detail, "no test files") {
		t.Errorf("expected tests-not-added violation, got %+v", v)
	}
}

func TestRequiredOutcomes_DeletedTestFileDoesntCount(t *testing.T) {
	d := Diff{ChangedFiles: []ChangedFile{
		{Path: "x_test.go", Status: StatusDeleted},
		{Path: "x.go", Status: StatusModified},
	}}
	v := Evaluate(d, Constraints{RequiredOutcomes: []string{"tests_added_or_updated"}})
	if len(v) != 1 {
		t.Errorf("expected violation when only test was deleted, got %+v", v)
	}
}

func TestRequiredOutcomes_CIGreen_NoSignal_Defers(t *testing.T) {
	// Pre-#297 a nil CIGreen produced a "no signal available"
	// violation. That false-positive fired on every Fishhawk-managed
	// PR because trace upload happens before CI runs. The new
	// behavior defers to branch protection: no violation, the outcome
	// is recorded in DeferredRequiredOutcomes instead.
	c := Constraints{RequiredOutcomes: []string{"ci_green"}}
	v := Evaluate(diff("a.go"), c)
	if len(v) != 0 {
		t.Errorf("expected no violation for ci_green when signal is nil, got %+v", v)
	}
	got := DeferredRequiredOutcomes(c)
	if len(got) != 1 || got[0] != "ci_green" {
		t.Errorf("expected DeferredRequiredOutcomes = [ci_green], got %+v", got)
	}
}

func TestDeferredRequiredOutcomes_OnlyDefersCIGreenWhenSignalAbsent(t *testing.T) {
	// Other outcomes are never deferred: tests_added_or_updated is
	// always evaluable against the diff at upload time. ci_green
	// only defers when the signal is nil — once the signal is
	// populated (future re-eval path) it evaluates normally.
	cases := []struct {
		name string
		c    Constraints
		want []string
	}{
		{"no required outcomes", Constraints{}, nil},
		{"tests_added_or_updated only", Constraints{RequiredOutcomes: []string{"tests_added_or_updated"}}, nil},
		{"ci_green with nil signal", Constraints{RequiredOutcomes: []string{"ci_green"}}, []string{"ci_green"}},
		{"ci_green with true signal", Constraints{RequiredOutcomes: []string{"ci_green"}, CIGreen: ptrBool(true)}, nil},
		{"ci_green with false signal", Constraints{RequiredOutcomes: []string{"ci_green"}, CIGreen: ptrBool(false)}, nil},
		{"both outcomes, ci_green nil", Constraints{RequiredOutcomes: []string{"tests_added_or_updated", "ci_green"}}, []string{"ci_green"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DeferredRequiredOutcomes(tc.c)
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			for i, w := range tc.want {
				if got[i] != w {
					t.Errorf("got[%d] = %q, want %q", i, got[i], w)
				}
			}
		})
	}
}

func ptrBool(b bool) *bool { return &b }

func TestRequiredOutcomes_CIGreen_True(t *testing.T) {
	green := true
	v := Evaluate(diff("a.go"), Constraints{
		RequiredOutcomes: []string{"ci_green"},
		CIGreen:          &green,
	})
	if len(v) != 0 {
		t.Errorf("expected no violations, got %+v", v)
	}
}

func TestRequiredOutcomes_CIGreen_False(t *testing.T) {
	green := false
	v := Evaluate(diff("a.go"), Constraints{
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
	// Mirror the classic feature_change implement stage from
	// MVP_SPEC §4.2.
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

// TestRequiredOutcomes_VerificationReported covers every enumerated
// mode of the substance-aware verification_reported outcome (#1886 /
// ADR-059). It is fail-closed in all three absent-or-negative modes: a
// nil signal, a failed outcome, and a skipped outcome each violate.
func TestRequiredOutcomes_VerificationReported(t *testing.T) {
	cases := []struct {
		name         string
		d            Diff
		sig          *VerificationSignal
		wantViolate  bool
		wantContains string
	}{
		{
			name:         "nil signal violates",
			d:            diff("a.go"),
			sig:          nil,
			wantViolate:  true,
			wantContains: "no verification evidence in trace",
		},
		{
			name:         "failed outcome violates",
			d:            diff("a.go"),
			sig:          &VerificationSignal{Outcome: "failed"},
			wantViolate:  true,
			wantContains: `"failed"`,
		},
		{
			name: "failed outcome names the failing command",
			d:    diff("a.go"),
			sig: &VerificationSignal{
				Outcome: "failed",
				Commands: []VerificationCommand{
					{Command: "scripts/test verify", ExitCode: 1, Outcome: "failed"},
				},
			},
			wantViolate:  true,
			wantContains: "scripts/test verify",
		},
		{
			// A skipped verify gate is not a passed gate.
			name:         "skipped outcome violates",
			d:            diff("a.go"),
			sig:          &VerificationSignal{Outcome: "skipped"},
			wantViolate:  true,
			wantContains: `"skipped"`,
		},
		{
			name:        "passed outcome satisfies",
			d:           diff("a.go"),
			sig:         &VerificationSignal{Outcome: "passed"},
			wantViolate: false,
		},
		{
			// Anti-vacuity: exactly the diff shape that satisfies
			// tests_added_or_updated must NOT satisfy this outcome.
			// Fails if the case were wired to diffTouchesTests.
			name:         "test-named file only, nil signal, still violates",
			d:            diff("backend/internal/policy/foo_test.go"),
			sig:          nil,
			wantViolate:  true,
			wantContains: "no verification evidence in trace",
		},
		{
			// The docs-only vacuous-satisfaction branch of
			// tests_added_or_updated is deliberately NOT inherited.
			name:         "docs-only diff, nil signal, still violates",
			d:            diff("README.md", "docs/ARCHITECTURE.md"),
			sig:          nil,
			wantViolate:  true,
			wantContains: "no verification evidence in trace",
		},
		{
			name:         "empty outcome string violates",
			d:            diff("a.go"),
			sig:          &VerificationSignal{},
			wantViolate:  true,
			wantContains: "unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := Evaluate(tc.d, Constraints{
				RequiredOutcomes: []string{"verification_reported"},
				Verification:     tc.sig,
			})
			if !tc.wantViolate {
				if len(v) != 0 {
					t.Fatalf("expected no violations, got %+v", v)
				}
				return
			}
			if len(v) != 1 {
				t.Fatalf("expected exactly 1 violation, got %+v", v)
			}
			if v[0].Constraint != "required_outcomes" {
				t.Errorf("Constraint = %q, want required_outcomes", v[0].Constraint)
			}
			if !strings.Contains(v[0].Detail, tc.wantContains) {
				t.Errorf("Detail = %q, want it to contain %q", v[0].Detail, tc.wantContains)
			}
		})
	}
}

// TestRequiredOutcomes_VerificationReported_IndependentOfTestsOutcome
// asserts the two outcomes evaluate independently when declared
// together: a test-file diff satisfies tests_added_or_updated while a
// nil verification signal still violates verification_reported, and a
// passing signal with a source-only diff leaves exactly the
// tests_added_or_updated violation.
func TestRequiredOutcomes_VerificationReported_IndependentOfTestsOutcome(t *testing.T) {
	both := []string{"tests_added_or_updated", "verification_reported"}

	v := Evaluate(diff("a.go", "a_test.go"), Constraints{RequiredOutcomes: both})
	if len(v) != 1 || !strings.Contains(v[0].Detail, "no verification evidence") {
		t.Errorf("test-file diff + nil signal: got %+v, want only the verification violation", v)
	}

	v = Evaluate(diff("a.go"), Constraints{
		RequiredOutcomes: both,
		Verification:     &VerificationSignal{Outcome: "passed"},
	})
	if len(v) != 1 || !strings.Contains(v[0].Detail, "no test files") {
		t.Errorf("source-only diff + passing signal: got %+v, want only the tests violation", v)
	}
}

// TestRequiredOutcomes_VerificationReported_NotDeferred pins binding
// condition 2: the outcome is never deferrable. Deferring it would
// reconstruct the vacuous pass this outcome exists to remove.
func TestRequiredOutcomes_VerificationReported_NotDeferred(t *testing.T) {
	c := Constraints{RequiredOutcomes: []string{"verification_reported", "ci_green"}}
	got := DeferredRequiredOutcomes(c)
	if len(got) != 1 || got[0] != "ci_green" {
		t.Errorf("DeferredRequiredOutcomes = %+v, want only [ci_green]", got)
	}
}

// TestConstraints_VerificationRoundTrip pins binding condition 5: the
// signal must survive the marshal/unmarshal round-trip through the
// audit payload's applied_constraints, which is what the post-CI policy
// re-evaluation decodes and re-emits. An untagged or unexported field
// would drop the signal and flip a satisfied outcome into a violation.
func TestConstraints_VerificationRoundTrip(t *testing.T) {
	in := Constraints{
		RequiredOutcomes: []string{"verification_reported"},
		Verification: &VerificationSignal{
			Outcome: "passed",
			Commands: []VerificationCommand{
				{Command: "scripts/test verify", ExitCode: 0, Outcome: "passed"},
			},
		},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"verification"`) {
		t.Fatalf("marshalled constraints = %s, want a `verification` member", raw)
	}
	var out Constraints
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Verification == nil || out.Verification.Outcome != "passed" {
		t.Fatalf("round-tripped Verification = %+v, want outcome passed", out.Verification)
	}
	if len(out.Verification.Commands) != 1 || out.Verification.Commands[0].Command != "scripts/test verify" {
		t.Errorf("round-tripped Commands = %+v", out.Verification.Commands)
	}
	// The decoded signal still satisfies the outcome.
	if v := Evaluate(diff("a.go"), out); len(v) != 0 {
		t.Errorf("re-evaluation of round-tripped constraints = %+v, want no violations", v)
	}
	// omitempty: a nil signal leaves the payload byte-identical to
	// pre-#1886 entries.
	bare, err := json.Marshal(Constraints{RequiredOutcomes: []string{"ci_green"}})
	if err != nil {
		t.Fatalf("marshal bare: %v", err)
	}
	if strings.Contains(string(bare), "verification") {
		t.Errorf("bare constraints = %s, want no verification member", bare)
	}
}

// diffCoverageCfg is the declared constraint the diff-coverage tests
// evaluate against: coverage.sh writes coverage.lcov, minimum 80%.
func diffCoverageCfg() *DiffCoverageConfig {
	return &DiffCoverageConfig{
		Command:            "coverage.sh",
		ReportPath:         "coverage.lcov",
		Format:             "lcov",
		MinNewLineCoverage: 80,
	}
}

// TestDiffCoverage covers every enumerated mode of the diff_coverage
// constraint (#1888 / ADR-059). Like verification_reported it is
// fail-closed on an absent measurement, and unlike it there are two
// SATISFIED states — at-or-above threshold, and the vacuous
// zero-new-lines pass.
func TestDiffCoverage(t *testing.T) {
	cases := []struct {
		name          string
		sig           *DiffCoverageSignal
		wantViolate   bool
		wantContains  []string
		wantFileNames []string
	}{
		{
			// Fail-closed: the runner ALWAYS emits a signal when the
			// constraint is configured, so absence means it never ran.
			name:         "nil signal violates",
			sig:          nil,
			wantViolate:  true,
			wantContains: []string{"no diff-coverage evidence in trace", "coverage.sh", "coverage.lcov", "80%"},
		},
		{
			name: "command exited non-zero violates and names the exit code",
			sig: &DiffCoverageSignal{
				Outcome:  "failed",
				Command:  "coverage.sh",
				ExitCode: 2,
				Reason:   "coverage command exited 2: FAIL ./pkg",
			},
			wantViolate:  true,
			wantContains: []string{`"failed"`, "coverage.sh", "exited 2", "FAIL ./pkg"},
		},
		{
			name: "unparseable report violates and names the report path",
			sig: &DiffCoverageSignal{
				Outcome:    "failed",
				Command:    "coverage.sh",
				ReportPath: "coverage.lcov",
				Reason:     `coverage report "coverage.lcov" could not be parsed as lcov: truncated`,
			},
			wantViolate:  true,
			wantContains: []string{"coverage.lcov", "could not be parsed"},
		},
		{
			name: "missing report violates and names the report path",
			sig: &DiffCoverageSignal{
				Outcome:    "failed",
				Command:    "coverage.sh",
				ReportPath: "coverage.lcov",
				Reason:     `coverage command exited 0 but its report "coverage.lcov" could not be read: no such file`,
			},
			wantViolate:  true,
			wantContains: []string{"coverage.lcov", "could not be read"},
		},
		{
			name: "unresolvable base ref violates with a named reason",
			sig: &DiffCoverageSignal{
				Outcome:  "failed",
				Command:  "coverage.sh",
				ExitCode: -1,
				Reason:   "could not determine the stage's added lines: diffcov: base ref is empty (unresolved base branch)",
			},
			wantViolate:  true,
			wantContains: []string{"base ref is empty"},
		},
		{
			name:         "empty outcome string violates",
			sig:          &DiffCoverageSignal{},
			wantViolate:  true,
			wantContains: []string{"unknown"},
		},
		{
			name: "below threshold violates naming covered/total/percent/threshold",
			sig: &DiffCoverageSignal{
				Outcome:         "measured",
				Command:         "coverage.sh",
				ReportPath:      "coverage.lcov",
				NewLines:        4,
				CoveredNewLines: 3,
				Percent:         75,
				UncoveredFiles:  []string{"src/app.go"},
			},
			wantViolate:   true,
			wantContains:  []string{"75.0%", "below the required 80%", "3 of 4", "coverage.sh", "coverage.lcov"},
			wantFileNames: []string{"src/app.go"},
		},
		{
			// Boundary: >= comparison, so exactly AT the threshold passes.
			name: "exactly at threshold satisfies",
			sig: &DiffCoverageSignal{
				Outcome: "measured", NewLines: 5, CoveredNewLines: 4, Percent: 80,
			},
			wantViolate: false,
		},
		{
			// The positive criterion: comfortably above the threshold
			// passes. Without this an implementation that rejects every
			// declared constraint would satisfy the rest of the list.
			name: "above threshold satisfies",
			sig: &DiffCoverageSignal{
				Outcome: "measured", NewLines: 10, CoveredNewLines: 10, Percent: 100,
			},
			wantViolate: false,
		},
		{
			// One below the boundary must still fail.
			name: "one below threshold violates",
			sig: &DiffCoverageSignal{
				Outcome: "measured", NewLines: 100, CoveredNewLines: 79, Percent: 79,
			},
			wantViolate:  true,
			wantContains: []string{"79.0%", "below the required 80%"},
		},
		{
			// The documented vacuous pass: a diff that added no coverable
			// lines cannot be under-covered. This is why the runner emits
			// measured-with-zero instead of emitting nothing.
			name: "measured with zero new lines satisfies",
			sig: &DiffCoverageSignal{
				Outcome:  "measured",
				NewLines: 0,
				Reason:   `no added lines against "main"; nothing to measure`,
			},
			wantViolate: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := Evaluate(diff("a.go"), Constraints{
				DiffCoverage:       diffCoverageCfg(),
				DiffCoverageSignal: tc.sig,
			})
			if !tc.wantViolate {
				if len(v) != 0 {
					t.Fatalf("expected no violations, got %+v", v)
				}
				return
			}
			if len(v) != 1 {
				t.Fatalf("expected exactly one violation, got %+v", v)
			}
			if v[0].Constraint != "diff_coverage" {
				t.Errorf("constraint = %q, want diff_coverage", v[0].Constraint)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(v[0].Detail, want) {
					t.Errorf("detail %q does not name %q", v[0].Detail, want)
				}
			}
			if tc.wantFileNames != nil && !reflect.DeepEqual(v[0].Files, tc.wantFileNames) {
				t.Errorf("files = %v, want %v", v[0].Files, tc.wantFileNames)
			}
		})
	}
}

// TestDiffCoverage_NotConfiguredIsNoOp is the no-regression pin: a
// workflow that does not declare the constraint takes no new code path,
// whatever signal happens to be present.
func TestDiffCoverage_NotConfiguredIsNoOp(t *testing.T) {
	sigs := []*DiffCoverageSignal{
		nil,
		{Outcome: "failed", Reason: "command exploded"},
		{Outcome: "measured", NewLines: 10, CoveredNewLines: 0, Percent: 0},
	}
	for i, sig := range sigs {
		if v := Evaluate(diff("a.go"), Constraints{DiffCoverageSignal: sig}); len(v) != 0 {
			t.Errorf("signal %d: got %+v, want no violations when the constraint is absent", i, v)
		}
	}
}

// TestDiffCoverage_NotDeferred pins that diff_coverage never appears in
// the deferred set. Deferring it would reconstruct the vacuous pass the
// constraint exists to remove.
func TestDiffCoverage_NotDeferred(t *testing.T) {
	c := Constraints{
		RequiredOutcomes: []string{"ci_green"},
		DiffCoverage:     diffCoverageCfg(),
	}
	for _, got := range DeferredRequiredOutcomes(c) {
		if strings.Contains(got, "diff_coverage") {
			t.Errorf("DeferredRequiredOutcomes = %+v, want no diff_coverage member", DeferredRequiredOutcomes(c))
		}
	}
}

// TestConstraints_DiffCoverageRoundTrip pins the audit-payload round trip
// the post-CI re-evaluation depends on: BOTH the declared constraint and
// its signal must survive marshal/unmarshal through
// applied_constraints. An untagged field would drop one of them and flip
// a satisfied constraint into a violation on re-eval.
func TestConstraints_DiffCoverageRoundTrip(t *testing.T) {
	in := Constraints{
		DiffCoverage: diffCoverageCfg(),
		DiffCoverageSignal: &DiffCoverageSignal{
			Outcome: "measured", NewLines: 10, CoveredNewLines: 9, Percent: 90,
		},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"diff_coverage"`, `"diff_coverage_signal"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("marshalled constraints = %s, want a %s member", raw, want)
		}
	}
	var out Constraints
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.DiffCoverage == nil || out.DiffCoverage.MinNewLineCoverage != 80 {
		t.Fatalf("round-tripped DiffCoverage = %+v, want threshold 80", out.DiffCoverage)
	}
	if out.DiffCoverageSignal == nil || out.DiffCoverageSignal.Percent != 90 {
		t.Fatalf("round-tripped signal = %+v, want percent 90", out.DiffCoverageSignal)
	}
	// The decoded pair still satisfies the constraint.
	if v := Evaluate(diff("a.go"), out); len(v) != 0 {
		t.Errorf("re-evaluation of round-tripped constraints = %+v, want no violations", v)
	}
	// omitempty: a non-declaring stage's payload stays byte-identical to
	// pre-#1888 entries.
	bare, err := json.Marshal(Constraints{RequiredOutcomes: []string{"ci_green"}})
	if err != nil {
		t.Fatalf("marshal bare: %v", err)
	}
	if strings.Contains(string(bare), "diff_coverage") {
		t.Errorf("bare constraints = %s, want no diff_coverage member", bare)
	}
}
