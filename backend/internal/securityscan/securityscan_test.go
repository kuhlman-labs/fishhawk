package securityscan

import (
	"reflect"
	"testing"
)

// TestContractTokens pins the literal VALUES of the cross-slice contract
// constants so a downstream drift (a webhook or gate that hardcodes a
// different string) fails here rather than silently never matching at
// runtime. These values are imported unchanged by waves 1-2.
func TestContractTokens(t *testing.T) {
	if got, want := AuditCategorySecurityFindings, "implement_security_findings"; got != want {
		t.Errorf("AuditCategorySecurityFindings = %q, want %q", got, want)
	}
	if got, want := GateMissingKind, "security_findings_unresolved"; got != want {
		t.Errorf("GateMissingKind = %q, want %q", got, want)
	}
}

func TestIsHighSeverity(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"high", true},
		{"critical", true}, // critical gates alongside high
		{"HIGH", true},     // case-insensitive
		{"  high  ", true}, // whitespace-tolerant
		{"Critical", true},
		{"medium", false},
		{"low", false},
		{"note", false}, // rule.severity values never gate
		{"warning", false},
		{"", false}, // no security severity (non-security query)
		{"unknown", false},
	}
	for _, tc := range cases {
		if got := IsHighSeverity(tc.in); got != tc.want {
			t.Errorf("IsHighSeverity(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestFilterHighSeverity(t *testing.T) {
	cases := []struct {
		name string
		in   []Finding
		want []Finding
	}{
		{
			name: "high held, critical held, medium/low/note dropped",
			in: []Finding{
				{Number: 1, Severity: "high"},
				{Number: 2, Severity: "medium"},
				{Number: 3, Severity: "critical"},
				{Number: 4, Severity: "low"},
				{Number: 5, Severity: "note"},
				{Number: 6, Severity: ""},
			},
			want: []Finding{
				{Number: 1, Severity: "high"},
				{Number: 3, Severity: "critical"},
			},
		},
		{
			name: "none held",
			in:   []Finding{{Number: 1, Severity: "low"}, {Number: 2, Severity: "medium"}},
			want: nil,
		},
		{
			name: "nil input yields nil",
			in:   nil,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FilterHighSeverity(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("FilterHighSeverity = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFilterHighSeverity_NoMutation guards the "pure: never mutates the
// input" contract.
func TestFilterHighSeverity_NoMutation(t *testing.T) {
	in := []Finding{{Number: 1, Severity: "high"}, {Number: 2, Severity: "low"}}
	before := append([]Finding(nil), in...)
	_ = FilterHighSeverity(in)
	if !reflect.DeepEqual(in, before) {
		t.Errorf("input mutated: got %v, want %v", in, before)
	}
}

func TestFilterToDiffFiles(t *testing.T) {
	findings := []Finding{
		{Number: 1, Path: "backend/a.go"},
		{Number: 2, Path: "backend/b.go"},
		{Number: 3, Path: ""}, // no location → can't intersect
		{Number: 4, Path: "frontend/c.ts"},
	}
	cases := []struct {
		name      string
		findings  []Finding
		diffFiles []string
		want      []Finding
	}{
		{
			name:      "keeps only findings whose path is in the diff",
			findings:  findings,
			diffFiles: []string{"backend/a.go", "frontend/c.ts"},
			want: []Finding{
				{Number: 1, Path: "backend/a.go"},
				{Number: 4, Path: "frontend/c.ts"},
			},
		},
		{
			name:      "finding outside the diff is dropped",
			findings:  findings,
			diffFiles: []string{"docs/x.md"},
			want:      nil,
		},
		{
			name:      "empty-path finding never intersects",
			findings:  []Finding{{Number: 3, Path: ""}},
			diffFiles: []string{""},
			want:      nil,
		},
		{
			name:      "empty diff set drops everything",
			findings:  findings,
			diffFiles: nil,
			want:      nil,
		},
		{
			name:      "empty findings yields nil",
			findings:  nil,
			diffFiles: []string{"backend/a.go"},
			want:      nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FilterToDiffFiles(tc.findings, tc.diffFiles)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("FilterToDiffFiles = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFilterToDiffFiles_NoMutation(t *testing.T) {
	in := []Finding{{Number: 1, Path: "a.go"}, {Number: 2, Path: "b.go"}}
	before := append([]Finding(nil), in...)
	_ = FilterToDiffFiles(in, []string{"a.go"})
	if !reflect.DeepEqual(in, before) {
		t.Errorf("input mutated: got %v, want %v", in, before)
	}
}

// TestFilters_Compose exercises the webhook's intended pipeline: reduce a
// raw alert list to high-severity findings intersecting the implement
// diff. Order of application is irrelevant for a pure pipeline, but this
// pins the end-to-end reduction the gate depends on.
func TestFilters_Compose(t *testing.T) {
	raw := []Finding{
		{Number: 1, Severity: "high", Path: "backend/a.go"},     // gates
		{Number: 2, Severity: "high", Path: "untouched.go"},     // high but off-diff
		{Number: 3, Severity: "low", Path: "backend/a.go"},      // on-diff but low
		{Number: 4, Severity: "critical", Path: "backend/b.go"}, // gates
	}
	diff := []string{"backend/a.go", "backend/b.go"}
	got := FilterToDiffFiles(FilterHighSeverity(raw), diff)
	want := []Finding{
		{Number: 1, Severity: "high", Path: "backend/a.go"},
		{Number: 4, Severity: "critical", Path: "backend/b.go"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("composed filters = %v, want %v", got, want)
	}
}
