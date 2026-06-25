package securityscan

import "testing"

// TestContractTokens pins the two cross-slice contract tokens so a downstream
// slice can rely on their exact string values (they appear in audit entries
// and the merge-gate MissingKind on the wire).
func TestContractTokens(t *testing.T) {
	if AuditCategorySecurityFindings != "implement_security_findings" {
		t.Errorf("AuditCategorySecurityFindings = %q, want implement_security_findings", AuditCategorySecurityFindings)
	}
	if SecurityFindingsUnresolved != "security_findings_unresolved" {
		t.Errorf("SecurityFindingsUnresolved = %q, want security_findings_unresolved", SecurityFindingsUnresolved)
	}
}

// TestFilterHighSeverity asserts only high/critical findings survive — the
// severities that hold the merge gate — and that lower severities never do.
// Matching is case-insensitive so a "High" from GitHub is not silently
// dropped.
func TestFilterHighSeverity(t *testing.T) {
	in := []Finding{
		{Number: 1, Severity: "critical"},
		{Number: 2, Severity: "high"},
		{Number: 3, Severity: "High"}, // case-insensitive
		{Number: 4, Severity: "medium"},
		{Number: 5, Severity: "low"},
		{Number: 6, Severity: "none"},
		{Number: 7, Severity: ""},
	}
	got := FilterHighSeverity(in)
	if len(got) != 3 {
		t.Fatalf("FilterHighSeverity kept %d findings, want 3 (got %+v)", len(got), got)
	}
	keptNums := map[int]bool{}
	for _, f := range got {
		keptNums[f.Number] = true
	}
	for _, n := range []int{1, 2, 3} {
		if !keptNums[n] {
			t.Errorf("expected finding #%d (high/critical) kept", n)
		}
	}
	for _, n := range []int{4, 5, 6, 7} {
		if keptNums[n] {
			t.Errorf("finding #%d (sub-high) must be dropped", n)
		}
	}
}

// TestFilterHighSeverity_NoneQualify asserts a nil slice when nothing
// qualifies, so a len-0 caller never blocks the gate on an empty set.
func TestFilterHighSeverity_NoneQualify(t *testing.T) {
	got := FilterHighSeverity([]Finding{{Severity: "low"}, {Severity: "medium"}})
	if len(got) != 0 {
		t.Fatalf("FilterHighSeverity = %+v, want empty", got)
	}
}

// TestFilterToDiffFiles asserts only findings on a changed file survive — an
// alert in an untouched file is not this run's concern and must not hold the
// gate.
func TestFilterToDiffFiles(t *testing.T) {
	in := []Finding{
		{Number: 1, Path: "backend/internal/foo/foo.go"},
		{Number: 2, Path: "backend/internal/bar/bar.go"},
		{Number: 3, Path: "untouched/elsewhere.go"},
	}
	changed := []string{"backend/internal/foo/foo.go", "backend/internal/bar/bar.go"}
	got := FilterToDiffFiles(in, changed)
	if len(got) != 2 {
		t.Fatalf("FilterToDiffFiles kept %d, want 2 (got %+v)", len(got), got)
	}
	for _, f := range got {
		if f.Path == "untouched/elsewhere.go" {
			t.Errorf("finding outside the diff must be dropped, got %+v", f)
		}
	}
}

// TestFilterToDiffFiles_EmptyInputs asserts both empty-input guards return a
// nil slice rather than aliasing or panicking.
func TestFilterToDiffFiles_EmptyInputs(t *testing.T) {
	if got := FilterToDiffFiles(nil, []string{"a.go"}); got != nil {
		t.Errorf("nil findings → %+v, want nil", got)
	}
	if got := FilterToDiffFiles([]Finding{{Path: "a.go"}}, nil); got != nil {
		t.Errorf("nil changedFiles → %+v, want nil", got)
	}
}

// TestFilterToDiffFiles_ExactMatch asserts matching is exact on the
// repo-relative path — a prefix/suffix overlap does not falsely intersect.
func TestFilterToDiffFiles_ExactMatch(t *testing.T) {
	in := []Finding{
		{Number: 1, Path: "a/b.go"},
		{Number: 2, Path: "a/b.go.bak"},
		{Number: 3, Path: "x/a/b.go"},
	}
	got := FilterToDiffFiles(in, []string{"a/b.go"})
	if len(got) != 1 || got[0].Number != 1 {
		t.Fatalf("FilterToDiffFiles exact match = %+v, want only #1", got)
	}
}
