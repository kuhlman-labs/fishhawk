package scopeamendment

import (
	"strings"
	"testing"
)

func TestValidatePaths_AcceptsModifyAndCreate(t *testing.T) {
	got, err := ValidatePaths([]PathEntry{
		{Path: " backend/internal/server/foo.go ", Operation: OperationModify},
		{Path: "docs/new-file.md", Operation: OperationCreate},
	})
	if err != nil {
		t.Fatalf("ValidatePaths: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Path != "backend/internal/server/foo.go" {
		t.Errorf("path not trimmed: %q", got[0].Path)
	}
	if got[1].Operation != OperationCreate {
		t.Errorf("operation = %q, want create", got[1].Operation)
	}
}

func TestValidatePaths_RejectsBadEntries(t *testing.T) {
	cases := []struct {
		name    string
		entries []PathEntry
		wantSub string
	}{
		{"empty set", nil, "at least one"},
		{"empty path", []PathEntry{{Path: "  ", Operation: OperationModify}}, "non-empty"},
		{"absolute", []PathEntry{{Path: "/etc/passwd", Operation: OperationModify}}, "repo-relative"},
		{"dotdot", []PathEntry{{Path: "../outside.go", Operation: OperationModify}}, ".."},
		{"bad operation", []PathEntry{{Path: "a.go", Operation: "delete"}}, "operation"},
		{"empty operation", []PathEntry{{Path: "a.go"}}, "operation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ValidatePaths(tc.entries); err == nil {
				t.Fatalf("ValidatePaths(%v) succeeded, want error containing %q", tc.entries, tc.wantSub)
			} else if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}
