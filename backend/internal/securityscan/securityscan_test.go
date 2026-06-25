package securityscan

import (
	"reflect"
	"testing"
)

// cloneFindings returns a deep-enough copy of findings for the
// input-not-mutated assertions (Finding is a flat value type, so a
// slice copy is a full copy).
func cloneFindings(in []Finding) []Finding {
	if in == nil {
		return nil
	}
	out := make([]Finding, len(in))
	copy(out, in)
	return out
}

func TestContractTokens(t *testing.T) {
	// The constant values are the cross-slice seam slices 2-5 import;
	// lock them so an accidental rename fails here, not in a consumer.
	if AuditCategorySecurityFindings != "implement_security_findings" {
		t.Errorf("AuditCategorySecurityFindings = %q, want %q",
			AuditCategorySecurityFindings, "implement_security_findings")
	}
	if MissingKind != "security_findings" {
		t.Errorf("MissingKind = %q, want %q", MissingKind, "security_findings")
	}
}

func TestFilterToDiffFiles(t *testing.T) {
	cases := []struct {
		name         string
		findings     []Finding
		changedPaths []string
		want         []Finding
	}{
		{
			name:         "finding on a changed file is kept",
			findings:     []Finding{{RuleID: "r1", Path: "a.go", StartLine: 3}},
			changedPaths: []string{"a.go"},
			want:         []Finding{{RuleID: "r1", Path: "a.go", StartLine: 3}},
		},
		{
			name:         "finding on an untouched file is dropped",
			findings:     []Finding{{RuleID: "r1", Path: "b.go"}},
			changedPaths: []string{"a.go"},
			want:         []Finding{},
		},
		{
			name:         "empty changedPaths drops everything",
			findings:     []Finding{{RuleID: "r1", Path: "a.go"}},
			changedPaths: []string{},
			want:         []Finding{},
		},
		{
			name:         "nil changedPaths drops everything",
			findings:     []Finding{{RuleID: "r1", Path: "a.go"}},
			changedPaths: nil,
			want:         []Finding{},
		},
		{
			name:         "nil findings returns empty",
			findings:     nil,
			changedPaths: []string{"a.go"},
			want:         []Finding{},
		},
		{
			name: "order is preserved across kept findings",
			findings: []Finding{
				{RuleID: "r1", Path: "a.go"},
				{RuleID: "r2", Path: "skip.go"},
				{RuleID: "r3", Path: "b.go"},
				{RuleID: "r4", Path: "a.go"},
			},
			changedPaths: []string{"a.go", "b.go"},
			want: []Finding{
				{RuleID: "r1", Path: "a.go"},
				{RuleID: "r3", Path: "b.go"},
				{RuleID: "r4", Path: "a.go"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := cloneFindings(tc.findings)
			got := FilterToDiffFiles(tc.findings, tc.changedPaths)
			if got == nil {
				t.Fatalf("FilterToDiffFiles returned nil, want non-nil slice")
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("FilterToDiffFiles = %+v, want %+v", got, tc.want)
			}
			if !reflect.DeepEqual(tc.findings, before) {
				t.Errorf("input mutated: got %+v, want %+v", tc.findings, before)
			}
		})
	}
}

func TestFilterHighSeverity(t *testing.T) {
	cases := []struct {
		name     string
		findings []Finding
		want     []Finding
	}{
		{
			name:     "high is kept",
			findings: []Finding{{RuleID: "r1", Severity: "high"}},
			want:     []Finding{{RuleID: "r1", Severity: "high"}},
		},
		{
			name:     "critical is kept",
			findings: []Finding{{RuleID: "r1", Severity: "critical"}},
			want:     []Finding{{RuleID: "r1", Severity: "critical"}},
		},
		{
			name:     "medium is dropped",
			findings: []Finding{{RuleID: "r1", Severity: "medium"}},
			want:     []Finding{},
		},
		{
			name:     "low is dropped",
			findings: []Finding{{RuleID: "r1", Severity: "low"}},
			want:     []Finding{},
		},
		{
			name:     "empty severity is dropped",
			findings: []Finding{{RuleID: "r1", Severity: ""}},
			want:     []Finding{},
		},
		{
			name:     "nil findings returns empty",
			findings: nil,
			want:     []Finding{},
		},
		{
			name: "mixed-case severity is handled and order preserved",
			findings: []Finding{
				{RuleID: "r1", Severity: "HIGH"},
				{RuleID: "r2", Severity: "Medium"},
				{RuleID: "r3", Severity: "Critical"},
			},
			want: []Finding{
				{RuleID: "r1", Severity: "HIGH"},
				{RuleID: "r3", Severity: "Critical"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := cloneFindings(tc.findings)
			got := FilterHighSeverity(tc.findings)
			if got == nil {
				t.Fatalf("FilterHighSeverity returned nil, want non-nil slice")
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("FilterHighSeverity = %+v, want %+v", got, tc.want)
			}
			if !reflect.DeepEqual(tc.findings, before) {
				t.Errorf("input mutated: got %+v, want %+v", tc.findings, before)
			}
		})
	}
}
