package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// newSurfaceSweepServer wires a Server with only an AuditRepo (no RunRepo)
// to prove runSurfaceSweep guards on AuditRepo alone (binding condition 3):
// the sweep matches purely on scope.files paths and reads nothing from the
// run, so a server without RunRepo still runs the sweep.
func newSurfaceSweepServer(t *testing.T) (*Server, *auditFake) {
	t.Helper()
	au := newAuditFake()
	s := New(Config{
		Addr:      "127.0.0.1:0",
		AuditRepo: au,
	})
	return s, au
}

// lastSurfaceSweepEntry decodes the single plan_surface_sweep payload the
// audit fake captured, failing the test when not exactly one was written.
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

// findingFor returns the finding whose Pattern matches name, or nil.
func findingFor(p SurfaceSweepPayload, name string) *SurfaceSweepFinding {
	for i := range p.Findings {
		if p.Findings[i].Pattern == name {
			return &p.Findings[i]
		}
	}
	return nil
}

// TestSurfaceSweep_RegistryPathsExistOnDisk is binding condition 1: every
// trigger AND sibling path in surfacePatterns must exist on disk. The
// registry hardcodes repo-relative paths; a future rename/move would
// silently disable the sweep with no signal. This os.Stat over every path
// makes such a rename break loudly here instead.
func TestSurfaceSweep_RegistryPathsExistOnDisk(t *testing.T) {
	// Resolve repo root from this test file's location:
	// backend/internal/server/surface_sweep_test.go -> three levels up.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate repo root")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")

	seen := map[string]bool{}
	for _, pat := range surfacePatterns {
		for _, p := range append(append([]string{}, pat.Triggers...), pat.Siblings...) {
			if seen[p] {
				continue
			}
			seen[p] = true
			abs := filepath.Join(repoRoot, filepath.FromSlash(p))
			if _, err := os.Stat(abs); err != nil {
				t.Errorf("registry path %q does not exist on disk (%v) — a rename/move silently disabled the sweep; update surfacePatterns", p, err)
			}
		}
	}
}

// TestSurfaceSweep_StatusTemplateWithoutNotifier: touching status_template.go
// but not notifier.go flags the @-mention notifier sibling.
func TestSurfaceSweep_StatusTemplateWithoutNotifier(t *testing.T) {
	s, au := newSurfaceSweepServer(t)
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: "backend/internal/issuecomment/status_template.go", Operation: plan.FileOpModify},
	})

	s.runSurfaceSweep(context.Background(), uuid.New(), uuid.New(), body)

	got := lastSurfaceSweepEntry(t, au)
	f := findingFor(got, "actor @-mention render surfaces")
	if f == nil {
		t.Fatalf("want an @-mention finding; got %+v", got.Findings)
	}
	if len(f.MissingSiblings) != 1 || f.MissingSiblings[0] != "backend/internal/issuecomment/notifier.go" {
		t.Errorf("MissingSiblings = %v, want [notifier.go]", f.MissingSiblings)
	}
}

// TestSurfaceSweep_NotifierAloneFlagsBothPatterns is binding condition 2
// (multi-finding coverage): notifier.go is a trigger for BOTH registry
// patterns, so a plan touching notifier.go ALONE (missing both
// status_template.go and docs/issue-comment-surfaces.md) must produce TWO
// simultaneous findings. This guards against an accidental short-circuit in
// the registry-matching loop.
func TestSurfaceSweep_NotifierAloneFlagsBothPatterns(t *testing.T) {
	s, au := newSurfaceSweepServer(t)
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: "backend/internal/issuecomment/notifier.go", Operation: plan.FileOpModify},
	})

	s.runSurfaceSweep(context.Background(), uuid.New(), uuid.New(), body)

	got := lastSurfaceSweepEntry(t, au)
	if len(got.Findings) != 2 {
		t.Fatalf("want 2 findings (both patterns trigger on notifier.go), got %d: %+v", len(got.Findings), got.Findings)
	}
	mention := findingFor(got, "actor @-mention render surfaces")
	if mention == nil || len(mention.MissingSiblings) != 1 || mention.MissingSiblings[0] != "backend/internal/issuecomment/status_template.go" {
		t.Errorf("@-mention finding wrong: %+v", mention)
	}
	doc := findingFor(got, "audit kind requires surfaces doc")
	if doc == nil || len(doc.MissingSiblings) != 1 || doc.MissingSiblings[0] != "docs/issue-comment-surfaces.md" {
		t.Errorf("surfaces-doc finding wrong: %+v", doc)
	}
}

// TestSurfaceSweep_AllSiblingsPresentNoFindings: a plan touching every
// sibling of every matched pattern produces zero findings but a present
// entry (checked and clean).
func TestSurfaceSweep_AllSiblingsPresentNoFindings(t *testing.T) {
	s, au := newSurfaceSweepServer(t)
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: "backend/internal/issuecomment/status_template.go", Operation: plan.FileOpModify},
		{Path: "backend/internal/issuecomment/notifier.go", Operation: plan.FileOpModify},
		{Path: "docs/issue-comment-surfaces.md", Operation: plan.FileOpModify},
	})

	s.runSurfaceSweep(context.Background(), uuid.New(), uuid.New(), body)

	got := lastSurfaceSweepEntry(t, au)
	if len(got.Findings) != 0 {
		t.Fatalf("want zero findings with all siblings present; got %+v", got.Findings)
	}
	if got.ScannedFiles != 3 {
		t.Errorf("ScannedFiles = %d, want 3", got.ScannedFiles)
	}
}

// TestSurfaceSweep_NoTriggerEmptyButPresent: a plan touching none of the
// triggers writes an empty-but-present entry (distinguishable from "never
// checked").
func TestSurfaceSweep_NoTriggerEmptyButPresent(t *testing.T) {
	s, au := newSurfaceSweepServer(t)
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: "backend/internal/foo/foo.go", Operation: plan.FileOpModify},
	})

	s.runSurfaceSweep(context.Background(), uuid.New(), uuid.New(), body)

	got := lastSurfaceSweepEntry(t, au)
	if len(got.Findings) != 0 {
		t.Fatalf("want zero findings; got %+v", got.Findings)
	}
	if got.ScannedFiles != 1 {
		t.Errorf("ScannedFiles = %d, want 1", got.ScannedFiles)
	}
}

// TestSurfaceSweep_FindingsMarshalAsEmptyArray asserts the clean entry's
// findings serialize as [] not null, so the MCP read side decodes "checked
// and clean" distinctly from "never checked".
func TestSurfaceSweep_FindingsMarshalAsEmptyArray(t *testing.T) {
	s, au := newSurfaceSweepServer(t)
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: "backend/internal/foo/foo.go", Operation: plan.FileOpModify},
	})

	s.runSurfaceSweep(context.Background(), uuid.New(), uuid.New(), body)

	au.mu.Lock()
	defer au.mu.Unlock()
	for _, ap := range au.appended {
		if ap.Category != categoryPlanSurfaceSweep {
			continue
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(ap.Payload, &raw); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if string(raw["findings"]) != "[]" {
			t.Errorf("findings = %s, want [] (not null)", raw["findings"])
		}
		return
	}
	t.Fatal("no plan_surface_sweep entry written")
}

// TestSurfaceSweep_NilAuditRepoNoEntry: a server without an AuditRepo writes
// nothing and never panics (fail-open guard).
func TestSurfaceSweep_NilAuditRepoNoEntry(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: "backend/internal/issuecomment/notifier.go", Operation: plan.FileOpModify},
	})
	// Must not panic; nothing to assert beyond no crash.
	s.runSurfaceSweep(context.Background(), uuid.New(), uuid.New(), body)
}

// TestSurfaceSweep_ParseErrorNoEntry: an unparseable plan body writes no
// entry (fail-open — validation already passed in handleShipPlan, so this
// is an internal inconsistency, not a block).
func TestSurfaceSweep_ParseErrorNoEntry(t *testing.T) {
	s, au := newSurfaceSweepServer(t)
	s.runSurfaceSweep(context.Background(), uuid.New(), uuid.New(), []byte("{not valid json"))

	if n := countSurfaceSweepEntries(au); n != 0 {
		t.Fatalf("want no entry written on a parse error; got %d", n)
	}
}
