package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// TestSurfacePatternsExistOnDisk is binding condition 1: every Trigger and
// Sibling path in the static registry must exist on disk. The registry
// hardcodes status_template.go, notifier.go, pullrequest.go, and
// docs/issue-comment-surfaces.md; a future rename would silently disable
// the sweep, so this makes a rename break loudly. Paths are repo-relative;
// this test runs from backend/internal/server, so the repo root is three
// levels up.
func TestSurfacePatternsExistOnDisk(t *testing.T) {
	const repoRoot = "../../.."
	seen := map[string]bool{}
	check := func(p string) {
		if seen[p] {
			return
		}
		seen[p] = true
		abs := filepath.Join(repoRoot, filepath.FromSlash(p))
		if _, err := os.Stat(abs); err != nil {
			t.Errorf("registry path %q does not exist on disk (%v) — a rename would silently disable the sweep", p, err)
		}
	}
	for _, pat := range surfacePatterns {
		for _, tr := range pat.Triggers {
			check(tr)
		}
		for _, sib := range pat.Siblings {
			check(sib)
		}
	}
}

func TestEvaluateSurfaceSweep(t *testing.T) {
	const (
		statusTemplate = "backend/internal/issuecomment/status_template.go"
		notifier       = "backend/internal/issuecomment/notifier.go"
		pullrequest    = "backend/internal/server/pullrequest.go"
		surfacesDoc    = "docs/issue-comment-surfaces.md"
		mcpTools       = "backend/cmd/fishhawk-mcp/tools.go"
		mcpToolsTest   = "backend/cmd/fishhawk-mcp/tools_test.go"
		mcpReadme      = "backend/cmd/fishhawk-mcp/README.md"
	)
	tests := []struct {
		name  string
		scope []string
		want  []SurfaceSweepFinding
	}{
		{
			name:  "render surface alone flags missing peer",
			scope: []string{statusTemplate},
			want: []SurfaceSweepFinding{
				{
					Pattern:         "actor @-mention render surfaces",
					TriggerPath:     statusTemplate,
					MissingSiblings: []string{notifier},
				},
			},
		},
		{
			name:  "both render surfaces no finding",
			scope: []string{statusTemplate, notifier, surfacesDoc},
			want:  nil,
		},
		{
			// Binding condition 2: notifier.go alone is a trigger for BOTH
			// patterns, so it fires twice — the missing render peer AND the
			// missing surfaces doc.
			name:  "notifier alone fires both patterns",
			scope: []string{notifier},
			want: []SurfaceSweepFinding{
				{
					Pattern:         "actor @-mention render surfaces",
					TriggerPath:     notifier,
					MissingSiblings: []string{statusTemplate},
				},
				{
					Pattern:         "audit kind requires surfaces doc",
					TriggerPath:     notifier,
					MissingSiblings: []string{surfacesDoc},
				},
			},
		},
		{
			name:  "audit emitter without surfaces doc flags doc",
			scope: []string{pullrequest},
			want: []SurfaceSweepFinding{
				{
					Pattern:         "audit kind requires surfaces doc",
					TriggerPath:     pullrequest,
					MissingSiblings: []string{surfacesDoc},
				},
			},
		},
		{
			name:  "audit emitter with surfaces doc no finding",
			scope: []string{pullrequest, surfacesDoc},
			want:  nil,
		},
		{
			// #873/#867: tools.go alone flags BOTH coupled siblings,
			// sorted (README.md before tools_test.go via sort.Strings).
			name:  "mcp tools.go alone flags count test and readme",
			scope: []string{mcpTools},
			want: []SurfaceSweepFinding{
				{
					Pattern:         "mcp tool registration requires count test + readme",
					TriggerPath:     mcpTools,
					MissingSiblings: []string{mcpReadme, mcpToolsTest},
				},
			},
		},
		{
			name:  "mcp tools.go with count test flags only missing readme",
			scope: []string{mcpTools, mcpToolsTest},
			want: []SurfaceSweepFinding{
				{
					Pattern:         "mcp tool registration requires count test + readme",
					TriggerPath:     mcpTools,
					MissingSiblings: []string{mcpReadme},
				},
			},
		},
		{
			name:  "mcp tools.go with both siblings no finding",
			scope: []string{mcpTools, mcpToolsTest, mcpReadme},
			want:  nil,
		},
		{
			// tools_test.go alone is not a trigger — the pattern is pinned
			// to the registration file, so this fires nothing.
			name:  "mcp count test alone no finding",
			scope: []string{mcpToolsTest},
			want:  nil,
		},
		{
			name:  "unrelated files no finding",
			scope: []string{"backend/internal/foo/foo.go", "README.md"},
			want:  nil,
		},
		{
			name:  "backslash path still matches via normalization",
			scope: []string{filepath.FromSlash(statusTemplate)},
			want: []SurfaceSweepFinding{
				{
					Pattern:         "actor @-mention render surfaces",
					TriggerPath:     statusTemplate,
					MissingSiblings: []string{notifier},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evaluateSurfaceSweep(tt.scope, surfacePatterns)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("evaluateSurfaceSweep() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// lastSurfaceSweepEntry decodes the single plan_surface_sweep payload the
// audit fake captured, failing the test when none was written.
func lastSurfaceSweepEntry(t *testing.T, au *auditFake) SurfaceSweepPayload {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var payloads []SurfaceSweepPayload
	for _, ap := range au.appended {
		if ap.Category != categoryPlanSurfaceSweep {
			continue
		}
		var p SurfaceSweepPayload
		if err := json.Unmarshal(ap.Payload, &p); err != nil {
			t.Fatalf("unmarshal surface sweep payload: %v", err)
		}
		payloads = append(payloads, p)
	}
	if len(payloads) != 1 {
		t.Fatalf("want exactly 1 plan_surface_sweep entry, got %d", len(payloads))
	}
	return payloads[0]
}

func countSurfaceSweepEntries(au *auditFake) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	n := 0
	for _, ap := range au.appended {
		if ap.Category == categoryPlanSurfaceSweep {
			n++
		}
	}
	return n
}

// TestRunSurfaceSweep_WritesFindings drives the Server-level writer and
// asserts a single plan_surface_sweep entry with the expected payload. It
// also exercises the empty-array-not-null contract via the marshalled
// payload (Findings is non-nil even when there are findings; the clean
// case is covered separately).
func TestRunSurfaceSweep_WritesFindings(t *testing.T) {
	s, au, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: "backend/internal/issuecomment/status_template.go", Operation: plan.FileOpModify},
	})

	s.runSurfaceSweep(context.Background(), runRow.ID, runRow.ID, body)

	got := lastSurfaceSweepEntry(t, au)
	if got.ScannedFiles != 1 {
		t.Errorf("ScannedFiles = %d, want 1", got.ScannedFiles)
	}
	if len(got.Findings) != 1 {
		t.Fatalf("want 1 finding, got %+v", got.Findings)
	}
	f := got.Findings[0]
	if f.Pattern != "actor @-mention render surfaces" {
		t.Errorf("Pattern = %q", f.Pattern)
	}
	if len(f.MissingSiblings) != 1 || f.MissingSiblings[0] != "backend/internal/issuecomment/notifier.go" {
		t.Errorf("MissingSiblings = %v, want [notifier.go]", f.MissingSiblings)
	}
}

// TestRunSurfaceSweep_CleanWritesEmptyFindings verifies the "checked and
// clean" contract: a plan touching no incomplete pattern still writes an
// entry, and Findings marshals as [] not null.
func TestRunSurfaceSweep_CleanWritesEmptyFindings(t *testing.T) {
	s, au, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: "backend/internal/foo/foo.go", Operation: plan.FileOpModify},
	})

	s.runSurfaceSweep(context.Background(), runRow.ID, runRow.ID, body)

	got := lastSurfaceSweepEntry(t, au)
	if len(got.Findings) != 0 {
		t.Fatalf("want zero findings on a clean sweep; got %+v", got.Findings)
	}

	// Assert the raw payload encodes Findings as [] not null.
	au.mu.Lock()
	defer au.mu.Unlock()
	for _, ap := range au.appended {
		if ap.Category != categoryPlanSurfaceSweep {
			continue
		}
		if !json.Valid(ap.Payload) {
			t.Fatalf("payload is not valid json: %s", ap.Payload)
		}
		// "findings":[] must appear; "findings":null must not.
		if got := string(ap.Payload); !strings.Contains(got, `"findings":[]`) {
			t.Errorf("payload should encode findings as []; got %s", got)
		}
	}
}

// TestRunSurfaceSweep_NilAuditRepoFailOpen verifies fail-open: a server
// with no AuditRepo writes nothing and never panics.
func TestRunSurfaceSweep_NilAuditRepoFailOpen(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: "backend/internal/issuecomment/status_template.go", Operation: plan.FileOpModify},
	})
	// Must not panic; AuditRepo is nil so nothing is written.
	if got := s.runSurfaceSweep(context.Background(), uuid.New(), uuid.New(), body); got != nil {
		t.Fatalf("fail-open must return a nil result (#963); got %+v", got)
	}
}

// TestRunSurfaceSweep_UnparseablePlanFailOpen verifies fail-open: an
// unparseable plan body writes no entry and never panics.
func TestRunSurfaceSweep_UnparseablePlanFailOpen(t *testing.T) {
	s, au, runRow := newScopePrecheckServer(t, specImplementPathConstraints)

	got := s.runSurfaceSweep(context.Background(), runRow.ID, runRow.ID, []byte("{not valid plan"))

	if n := countSurfaceSweepEntries(au); n != 0 {
		t.Fatalf("want no entry for an unparseable plan; got %d", n)
	}
	if got != nil {
		t.Fatalf("fail-open must return a nil result (#963); got %+v", got)
	}
}

// TestRunSurfaceSweep_ReturnsComputedPayload pins the #963 return
// contract: the function returns the same result payload it records in
// the audit entry, so handleShipPlan can thread it into the plan-review
// prompt without a read-back.
func TestRunSurfaceSweep_ReturnsComputedPayload(t *testing.T) {
	s, au, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: "backend/internal/issuecomment/status_template.go", Operation: plan.FileOpModify},
	})

	got := s.runSurfaceSweep(context.Background(), runRow.ID, runRow.ID, body)
	if got == nil {
		t.Fatal("want a non-nil result when the sweep ran")
	}

	recorded := lastSurfaceSweepEntry(t, au)
	gotJSON, _ := json.Marshal(got)
	recordedJSON, _ := json.Marshal(recorded)
	if string(gotJSON) != string(recordedJSON) {
		t.Errorf("returned result diverges from the recorded audit payload:\nreturned: %s\nrecorded: %s", gotJSON, recordedJSON)
	}
	if len(got.Findings) != 1 || got.Findings[0].Pattern != "actor @-mention render surfaces" {
		t.Errorf("returned result missing the expected finding: %+v", got.Findings)
	}
}
