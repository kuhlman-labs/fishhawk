package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan/planfixture"
)

// subPlanScope is one decomposition sub-plan's title + own scope.files for
// the decomposed-plan-body test helper (#1077).
type subPlanScope struct {
	title string
	files []plan.ScopeFile
}

// decomposedScopePlanBody builds a schema-valid decomposed standard_v1 plan
// body: the flat parent scope.files plus a decomposition whose sub-plans
// each carry their own scope.files. Used by the #1077 per-sub-plan sweep
// tests. Sub-plan titles must be distinct and their scopes disjoint (the
// schema + semantic checks reject duplicates / cross-slice shared files).
func decomposedScopePlanBody(t *testing.T, parentFiles []plan.ScopeFile, subs []subPlanScope) []byte {
	t.Helper()
	toMaps := func(files []plan.ScopeFile) []any {
		fm := make([]any, 0, len(files))
		for _, f := range files {
			fm = append(fm, map[string]any{"path": f.Path, "operation": string(f.Operation)})
		}
		return fm
	}
	subMaps := make([]any, 0, len(subs))
	for _, sp := range subs {
		subMaps = append(subMaps, map[string]any{
			"title":                        sp.title,
			"scope_hint":                   sp.title + " slice",
			"scope":                        map[string]any{"files": toMaps(sp.files)},
			"predicted_runtime_minutes":    10,
			"predicted_runtime_confidence": "medium",
		})
	}
	m := planfixture.Valid(func(p map[string]any) {
		p["scope"] = map[string]any{"files": toMaps(parentFiles)}
	})
	m["decomposition"] = map[string]any{
		"rationale": "scope exceeded single-stage budget",
		"sub_plans": subMaps,
	}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal decomposed plan: %v", err)
	}
	if err := plan.Validate(body); err != nil {
		t.Fatalf("fixture decomposed plan does not validate: %v", err)
	}
	return body
}

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

// TestSurfaceCouplingPatternsForPrompt_MapsRegistry pins the #763/#1797
// accessor: surfaceCouplingPatternsForPrompt() projects EVERY surfacePatterns
// entry into the prompt wire type with no drift (single source of truth), and
// in particular carries the notifier.go<->status_template.go actor-@-mention
// coupling — the exact miss #1797 targets.
func TestSurfaceCouplingPatternsForPrompt_MapsRegistry(t *testing.T) {
	got := surfaceCouplingPatternsForPrompt()
	if len(got) != len(surfacePatterns) {
		t.Fatalf("accessor mapped %d patterns, registry has %d — projection must be 1:1", len(got), len(surfacePatterns))
	}
	for i, src := range surfacePatterns {
		if got[i].Name != src.Name {
			t.Errorf("pattern %d name: got %q want %q", i, got[i].Name, src.Name)
		}
		if !reflect.DeepEqual(got[i].Triggers, src.Triggers) {
			t.Errorf("pattern %d triggers: got %v want %v", i, got[i].Triggers, src.Triggers)
		}
		if !reflect.DeepEqual(got[i].Siblings, src.Siblings) {
			t.Errorf("pattern %d siblings: got %v want %v", i, got[i].Siblings, src.Siblings)
		}
	}

	// The actor @-mention render pattern must be present with both members —
	// the notifier.go / status_template.go lockstep coupling #1797 targets.
	var found bool
	for _, p := range got {
		if p.Name != "actor @-mention render surfaces" {
			continue
		}
		found = true
		hasNotifier := containsStrSlice(p.Triggers, "backend/internal/issuecomment/notifier.go")
		hasTemplate := containsStrSlice(p.Siblings, "backend/internal/issuecomment/status_template.go")
		if !hasNotifier || !hasTemplate {
			t.Errorf("actor @-mention pattern missing lockstep members: triggers=%v siblings=%v", p.Triggers, p.Siblings)
		}
	}
	if !found {
		t.Errorf("accessor did not include the actor @-mention render surfaces pattern")
	}
}

func containsStrSlice(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestEvaluateSurfaceSweep(t *testing.T) {
	const (
		statusTemplate = "backend/internal/issuecomment/status_template.go"
		notifier       = "backend/internal/issuecomment/notifier.go"
		pullrequest    = "backend/internal/server/pullrequest.go"
		surfacesDoc    = "docs/issue-comment-surfaces.md"
		mcpTools       = "backend/internal/mcpserver/tools.go"
		mcpToolsTest   = "backend/internal/mcpserver/tools_test.go"
		mcpReadme      = "backend/internal/mcpserver/README.md"
	)
	tests := []struct {
		name        string
		scope       []string
		exemptions  []plan.SurfaceSweepExemption
		want        []SurfaceSweepFinding
		wantApplied []AppliedExemption
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
			// #1077: a canonical workflow schema edited with its backend
			// mirror but not the cli mirror flags the missing cli copy.
			name: "workflow schema without cli mirror flags it",
			scope: []string{
				"docs/spec/workflow-v0.schema.json",
				"backend/internal/spec/schemas/workflow-v0.schema.json",
			},
			want: []SurfaceSweepFinding{
				{
					Pattern:         "workflow schema requires every mirror",
					TriggerPath:     "docs/spec/workflow-v0.schema.json",
					MissingSiblings: []string{"cli/internal/spec/schemas/workflow-v0.schema.json"},
				},
			},
		},
		{
			// ADR-046 / #1381: a canonical workflow-v1 schema edited with its
			// backend mirror but not the cli mirror flags the missing cli copy
			// — the v1 mirror set is its own self-referential pattern.
			name: "workflow-v1 schema without cli mirror flags it",
			scope: []string{
				"docs/spec/workflow-v1.schema.json",
				"backend/internal/spec/schemas/workflow-v1.schema.json",
			},
			want: []SurfaceSweepFinding{
				{
					Pattern:         "workflow-v1 schema requires every mirror",
					TriggerPath:     "docs/spec/workflow-v1.schema.json",
					MissingSiblings: []string{"cli/internal/spec/schemas/workflow-v1.schema.json"},
				},
			},
		},
		{
			// ADR-067 / #2213: a canonical workflow-v2 schema edited with its
			// backend mirror but not the cli mirror flags the missing cli copy
			// — the v2 mirror set is its own self-referential pattern.
			name: "workflow-v2 schema without cli mirror flags it",
			scope: []string{
				"docs/spec/workflow-v2.schema.json",
				"backend/internal/spec/schemas/workflow-v2.schema.json",
			},
			want: []SurfaceSweepFinding{
				{
					Pattern:         "workflow-v2 schema requires every mirror",
					TriggerPath:     "docs/spec/workflow-v2.schema.json",
					MissingSiblings: []string{"cli/internal/spec/schemas/workflow-v2.schema.json"},
				},
			},
		},
		{
			// All three mirrors of a schema family present: no finding.
			name: "plan-standard schema all mirrors no finding",
			scope: []string{
				"docs/spec/plan-standard-v1.schema.json",
				"backend/internal/plan/schemas/plan-standard-v1.schema.json",
				"runner/internal/plan/schemas/plan-standard-v1.schema.json",
			},
			want: nil,
		},
		{
			// E54.3 / #2235: the grooming-report-v1 canonical scoped without
			// its embedded mirror is the self-referential lockstep miss the
			// registry entry exists to catch (scripts/sync-schemas routes the
			// canonical to exactly one backend mirror).
			name:  "grooming-report canonical without mirror flags",
			scope: []string{"docs/spec/grooming-report-v1.schema.json"},
			want: []SurfaceSweepFinding{
				{
					Pattern:         "grooming-report schema requires every mirror",
					TriggerPath:     "docs/spec/grooming-report-v1.schema.json",
					MissingSiblings: []string{"backend/internal/plan/schemas/grooming-report-v1.schema.json"},
				},
			},
		},
		{
			// Both members scoped: no finding.
			name: "grooming-report both mirrors no finding",
			scope: []string{
				"docs/spec/grooming-report-v1.schema.json",
				"backend/internal/plan/schemas/grooming-report-v1.schema.json",
			},
			want: nil,
		},
		{
			name:  "unrelated files no finding",
			scope: []string{"backend/internal/foo/foo.go", "README.md"},
			want:  nil,
		},
		{
			// A backslash-separated scope path must normalize and match on
			// EVERY runtime. A literal backslash string is used (not
			// filepath.FromSlash, a no-op on the Unix test runtime) so the
			// case actually exercises the backslash edge (#1544).
			name:  "backslash scope path still matches via normalization",
			scope: []string{`backend\internal\issuecomment\status_template.go`},
			want: []SurfaceSweepFinding{
				{
					Pattern:         "actor @-mention render surfaces",
					TriggerPath:     statusTemplate,
					MissingSiblings: []string{notifier},
				},
			},
		},
		{
			// #1544 regression: a schema-valid exemption whose `sibling` is
			// backslash-separated must normalize and match the slash-form
			// registry sibling on the Unix server runtime. A literal
			// backslash string (not filepath.FromSlash, a no-op here) proves
			// filepath.ToSlash's host-separator-only rewrite would leave this
			// unmatched and the exemption a silent no-op.
			name:  "backslash sibling exemption suppresses finding",
			scope: []string{statusTemplate},
			exemptions: []plan.SurfaceSweepExemption{
				{Pattern: "actor @-mention render surfaces", Sibling: `backend\internal\issuecomment\notifier.go`, Reason: "system-actor render, no @-mention"},
			},
			want: nil,
			wantApplied: []AppliedExemption{
				{Pattern: "actor @-mention render surfaces", Sibling: notifier, Reason: "system-actor render, no @-mention"},
			},
		},
		{
			// #1544 branch 1: status_template.go in scope with a matching
			// exemption naming the absent notifier.go sibling fully covers the
			// missing set — the finding is suppressed and the exemption is
			// recorded as applied (reviewer-visible), not silent.
			name:  "matching exemption suppresses finding and records applied",
			scope: []string{statusTemplate},
			exemptions: []plan.SurfaceSweepExemption{
				{Pattern: "actor @-mention render surfaces", Sibling: notifier, Reason: "system-actor render, no @-mention"},
			},
			want: nil,
			wantApplied: []AppliedExemption{
				{Pattern: "actor @-mention render surfaces", Sibling: notifier, Reason: "system-actor render, no @-mention"},
			},
		},
		{
			// #1544 branch 2: a partial exemption covering one of two absent
			// siblings still fires a finding listing the REMAINING uncovered
			// sibling, and records the one applied exemption. tools.go alone
			// misses README.md + tools_test.go; exempt only the readme.
			name:  "partial exemption still flags remaining sibling",
			scope: []string{mcpTools},
			exemptions: []plan.SurfaceSweepExemption{
				{Pattern: "mcp tool registration requires count test + readme", Sibling: mcpReadme, Reason: "no user-facing tool listing change"},
			},
			want: []SurfaceSweepFinding{
				{
					Pattern:         "mcp tool registration requires count test + readme",
					TriggerPath:     mcpTools,
					MissingSiblings: []string{mcpToolsTest},
				},
			},
			wantApplied: []AppliedExemption{
				{Pattern: "mcp tool registration requires count test + readme", Sibling: mcpReadme, Reason: "no user-facing tool listing change"},
			},
		},
		{
			// #1544 branch 5a: an exemption naming the WRONG pattern for the
			// sibling does not match, so nothing is suppressed and no applied
			// exemption is recorded — the finding fires as if undeclared.
			name:  "wrong-pattern exemption does not suppress",
			scope: []string{statusTemplate},
			exemptions: []plan.SurfaceSweepExemption{
				{Pattern: "audit kind requires surfaces doc", Sibling: notifier, Reason: "mislabeled pattern"},
			},
			want: []SurfaceSweepFinding{
				{
					Pattern:         "actor @-mention render surfaces",
					TriggerPath:     statusTemplate,
					MissingSiblings: []string{notifier},
				},
			},
			wantApplied: nil,
		},
		{
			// #1544 branch 5b: an exemption naming a NON-MEMBER sibling path
			// (not a sibling of the firing pattern) never matches, so the
			// finding fires unchanged and no applied exemption is recorded.
			name:  "non-member sibling exemption does not suppress",
			scope: []string{statusTemplate},
			exemptions: []plan.SurfaceSweepExemption{
				{Pattern: "actor @-mention render surfaces", Sibling: "backend/internal/foo/foo.go", Reason: "not a member"},
			},
			want: []SurfaceSweepFinding{
				{
					Pattern:         "actor @-mention render surfaces",
					TriggerPath:     statusTemplate,
					MissingSiblings: []string{notifier},
				},
			},
			wantApplied: nil,
		},
		{
			// #1544: an exemption for a non-firing pattern (its trigger is not
			// in scope) is a harmless no-op — never recorded as applied.
			name:  "exemption for non-firing pattern is a no-op",
			scope: []string{"backend/internal/foo/foo.go"},
			exemptions: []plan.SurfaceSweepExemption{
				{Pattern: "actor @-mention render surfaces", Sibling: notifier, Reason: "no-op"},
			},
			want:        nil,
			wantApplied: nil,
		},
		{
			// #1544: exempting a sibling that is ALSO scoped is not "applied"
			// — the sibling is present, so it was never in the missing set.
			// Both surfaces scoped + surfaces doc = clean, exemption unused.
			name:  "exemption for already-scoped sibling not applied",
			scope: []string{statusTemplate, notifier, surfacesDoc},
			exemptions: []plan.SurfaceSweepExemption{
				{Pattern: "actor @-mention render surfaces", Sibling: notifier, Reason: "already scoped"},
			},
			want:        nil,
			wantApplied: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, gotApplied := evaluateSurfaceSweep(tt.scope, surfacePatterns, tt.exemptions)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("evaluateSurfaceSweep() findings = %+v, want %+v", got, tt.want)
			}
			if !reflect.DeepEqual(gotApplied, tt.wantApplied) {
				t.Errorf("evaluateSurfaceSweep() applied = %+v, want %+v", gotApplied, tt.wantApplied)
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
		// cross_slice_findings must likewise marshal as [] not null on a
		// clean / non-decomposed plan (#1102).
		if got := string(ap.Payload); !strings.Contains(got, `"cross_slice_findings":[]`) {
			t.Errorf("payload should encode cross_slice_findings as []; got %s", got)
		}
		// applied_exemptions must marshal as [] not null when the plan
		// declared no exemption that applied (#1544).
		if got := string(ap.Payload); !strings.Contains(got, `"applied_exemptions":[]`) {
			t.Errorf("payload should encode applied_exemptions as []; got %s", got)
		}
	}
}

// exemptionPlanBody builds a schema-valid flat plan body with the given
// scope.files and top-level surface_sweep_exemptions (#1544).
func exemptionPlanBody(t *testing.T, files []plan.ScopeFile, exemptions []plan.SurfaceSweepExemption) []byte {
	t.Helper()
	fileMaps := make([]any, 0, len(files))
	for _, f := range files {
		fileMaps = append(fileMaps, map[string]any{"path": f.Path, "operation": string(f.Operation)})
	}
	exMaps := make([]any, 0, len(exemptions))
	for _, e := range exemptions {
		exMaps = append(exMaps, map[string]any{"pattern": e.Pattern, "sibling": e.Sibling, "reason": e.Reason})
	}
	m := planfixture.Valid(func(p map[string]any) {
		p["scope"] = map[string]any{"files": fileMaps}
		p["surface_sweep_exemptions"] = exMaps
	})
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal exemption plan: %v", err)
	}
	if err := plan.Validate(body); err != nil {
		t.Fatalf("fixture exemption plan does not validate: %v", err)
	}
	return body
}

// TestRunSurfaceSweep_AppliedExemptionsPopulated covers #1544 branch 6: a
// plan scoping a trigger with a matching exemption for the absent sibling
// suppresses the finding AND records the applied exemption (reviewer-
// visible), and the returned payload equals the recorded one.
func TestRunSurfaceSweep_AppliedExemptionsPopulated(t *testing.T) {
	s, au, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
	const (
		statusTemplate = "backend/internal/issuecomment/status_template.go"
		notifier       = "backend/internal/issuecomment/notifier.go"
	)
	body := exemptionPlanBody(t,
		[]plan.ScopeFile{{Path: statusTemplate, Operation: plan.FileOpModify}},
		[]plan.SurfaceSweepExemption{
			{Pattern: "actor @-mention render surfaces", Sibling: notifier, Reason: "system-actor render, no @-mention"},
		},
	)

	got := s.runSurfaceSweep(context.Background(), runRow.ID, runRow.ID, body)
	if got == nil {
		t.Fatal("want a non-nil result when the sweep ran")
	}
	if len(got.Findings) != 0 {
		t.Fatalf("matching exemption should suppress the finding; got %+v", got.Findings)
	}
	if len(got.AppliedExemptions) != 1 {
		t.Fatalf("want 1 applied exemption; got %+v", got.AppliedExemptions)
	}
	ae := got.AppliedExemptions[0]
	if ae.Pattern != "actor @-mention render surfaces" || ae.Sibling != notifier ||
		ae.Reason != "system-actor render, no @-mention" || ae.SubPlanTitle != "" {
		t.Errorf("applied exemption = %+v, want {pattern, notifier, reason, no subplan}", ae)
	}

	// The recorded payload must equal the returned one, and encode
	// applied_exemptions with the reason (not silent).
	recorded := lastSurfaceSweepEntry(t, au)
	gotJSON, _ := json.Marshal(got)
	recordedJSON, _ := json.Marshal(recorded)
	if string(gotJSON) != string(recordedJSON) {
		t.Errorf("returned result diverges from recorded payload:\nreturned: %s\nrecorded: %s", gotJSON, recordedJSON)
	}
}

// TestRunSurfaceSweep_SubPlanExemptionAttributed covers #1544 branch 7: a
// top-level exemption applies to a decomposition sub-plan's own scope, and
// the applied exemption is attributed to that sub-plan via SubPlanTitle.
func TestRunSurfaceSweep_SubPlanExemptionAttributed(t *testing.T) {
	s, au, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
	const (
		statusTemplate = "backend/internal/issuecomment/status_template.go"
		notifier       = "backend/internal/issuecomment/notifier.go"
	)
	// Build a decomposed body whose "render slice" scopes status_template.go
	// only, then attach a top-level exemption for the notifier.go sibling.
	m := planfixture.Valid(func(p map[string]any) {
		p["scope"] = map[string]any{"files": []any{
			map[string]any{"path": "backend/internal/foo/foo.go", "operation": "modify"},
		}}
		p["surface_sweep_exemptions"] = []any{
			map[string]any{"pattern": "actor @-mention render surfaces", "sibling": notifier, "reason": "system-actor render only"},
		}
		p["decomposition"] = map[string]any{
			"rationale": "scope exceeded single-stage budget",
			"sub_plans": []any{
				map[string]any{
					"title": "render slice", "scope_hint": "render slice",
					"scope":                        map[string]any{"files": []any{map[string]any{"path": statusTemplate, "operation": "modify"}}},
					"predicted_runtime_minutes":    10,
					"predicted_runtime_confidence": "medium",
				},
				map[string]any{
					"title": "unrelated slice", "scope_hint": "unrelated slice",
					"scope":                        map[string]any{"files": []any{map[string]any{"path": "backend/internal/bar/bar.go", "operation": "modify"}}},
					"predicted_runtime_minutes":    10,
					"predicted_runtime_confidence": "medium",
				},
			},
		}
	})
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal decomposed exemption plan: %v", err)
	}
	if err := plan.Validate(body); err != nil {
		t.Fatalf("fixture does not validate: %v", err)
	}

	s.runSurfaceSweep(context.Background(), runRow.ID, runRow.ID, body)

	got := lastSurfaceSweepEntry(t, au)
	// No finding should survive: the render slice's missing notifier.go
	// sibling is fully covered by the top-level exemption.
	for _, f := range got.Findings {
		if f.Pattern == "actor @-mention render surfaces" {
			t.Errorf("render-surface finding should be suppressed by the exemption; got %+v", f)
		}
	}
	var found *AppliedExemption
	for i := range got.AppliedExemptions {
		if got.AppliedExemptions[i].SubPlanTitle == "render slice" {
			found = &got.AppliedExemptions[i]
		}
	}
	if found == nil {
		t.Fatalf("want an applied exemption attributed to the render slice; got %+v", got.AppliedExemptions)
	}
	if found.Sibling != notifier || found.Reason != "system-actor render only" {
		t.Errorf("attributed exemption = %+v", *found)
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

// TestRunSurfaceSweep_SubPlanScopeAttributed covers #1077: a decomposition
// sub-plan that scopes a canonical schema without its cli mirror yields a
// SubPlanTitle-attributed surface-sweep finding, while the flat parent
// scope (unrelated files) stays clean.
func TestRunSurfaceSweep_SubPlanScopeAttributed(t *testing.T) {
	s, au, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
	body := decomposedScopePlanBody(t,
		[]plan.ScopeFile{{Path: "backend/internal/foo/foo.go", Operation: plan.FileOpModify}},
		[]subPlanScope{
			{
				title: "schema slice",
				files: []plan.ScopeFile{
					{Path: "docs/spec/workflow-v0.schema.json", Operation: plan.FileOpModify},
					{Path: "backend/internal/spec/schemas/workflow-v0.schema.json", Operation: plan.FileOpModify},
				},
			},
			{
				title: "unrelated slice",
				files: []plan.ScopeFile{{Path: "backend/internal/bar/bar.go", Operation: plan.FileOpModify}},
			},
		},
	)

	s.runSurfaceSweep(context.Background(), runRow.ID, runRow.ID, body)

	got := lastSurfaceSweepEntry(t, au)
	if got.ScannedFiles != 1 {
		t.Errorf("ScannedFiles = %d, want 1 (parent scope unchanged)", got.ScannedFiles)
	}
	var found *SurfaceSweepFinding
	for i := range got.Findings {
		if got.Findings[i].SubPlanTitle == "" {
			t.Errorf("unexpected parent-scope finding: %+v", got.Findings[i])
			continue
		}
		if got.Findings[i].SubPlanTitle == "schema slice" {
			found = &got.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("want a finding attributed to the schema sub-plan; got %+v", got.Findings)
	}
	if found.Pattern != "workflow schema requires every mirror" {
		t.Errorf("Pattern = %q", found.Pattern)
	}
	wantMissing := "cli/internal/spec/schemas/workflow-v0.schema.json"
	if len(found.MissingSiblings) != 1 || found.MissingSiblings[0] != wantMissing {
		t.Errorf("MissingSiblings = %v, want [%s]", found.MissingSiblings, wantMissing)
	}
}

// crossSlicePlan builds a *plan.Plan with a decomposition from the given
// sub-plans for the pure evaluateCrossSliceCoupling tests. A nil files slice
// declares no scope (Scope == nil → inherits parent → excluded).
func crossSlicePlan(subs []subPlanScope) *plan.Plan {
	subPlans := make([]plan.SubPlanSummary, 0, len(subs))
	for _, sp := range subs {
		summary := plan.SubPlanSummary{Title: sp.title}
		if sp.files != nil {
			summary.Scope = &plan.Scope{Files: sp.files}
		}
		subPlans = append(subPlans, summary)
	}
	return &plan.Plan{Decomposition: &plan.Decomposition{SubPlans: subPlans}}
}

const (
	wmCanonical = "docs/spec/work-management-v0.schema.json"
	wmMirror    = "backend/internal/workmgmt/schemas/work-management-v0.schema.json"
	wmCLIMirror = "cli/internal/spec/schemas/work-management-v0.schema.json"
)

// Counterfactual record for the #3203 registry rider (binding approval
// condition 3), EXECUTED not reasoned: the
// cli/internal/spec/schemas/work-management-v0.schema.json entry was
// REMOVED from both Triggers and Siblings in surface_sweep.go (restoring
// the stale pre-#3203 pattern), then restored byte-identically.
//
//	go test ./internal/server/ -run TestWorkManagementPatternCoversBothSyncSchemasMirrors -v
//	RED:
//	  surface_sweep_test.go:881: Triggers missing "cli/internal/spec/schemas/work-management-v0.schema.json" — a canonical edit skipping it would draw no finding
//	  surface_sweep_test.go:884: Siblings missing "cli/internal/spec/schemas/work-management-v0.schema.json" — a canonical edit skipping it would draw no finding
//	  surface_sweep_test.go:902: want a work-management finding naming the unscoped CLI mirror; got []
//
// The third line is the behavioral half: with the stale pattern the sweep
// returned NO finding at all for a plan scoping the canonical schema and
// the backend mirror but not the CLI one — which is the exact gap.
// TestWorkManagementPatternCoversBothSyncSchemasMirrors is the SEPARATE
// attestation of the in-loop rider ratified as binding approval condition
// 2 (#3203): the registry's "work-management schema requires every
// mirror" pattern was STALE — it carried only the backend mirror, while
// the scripts/sync-schemas case arm it mirrors,
//
//	work-management-v*.schema.json)
//	  cp "$schema" "$REPO_ROOT/backend/internal/workmgmt/schemas/$base"
//	  cp "$schema" "$REPO_ROOT/cli/internal/spec/schemas/$base"   # E54.11 / #2801
//
// routes TWO mirrors. Before this change a plan touching the canonical
// schema and the backend mirror but NOT the CLI mirror drew no
// missing-sibling finding. The assertion is on the ARM's own routing, so
// a future third mirror added to the arm without a registry update is
// what a reader is pointed at, not a count that happens to be 3.
func TestWorkManagementPatternCoversBothSyncSchemasMirrors(t *testing.T) {
	var pat *surfacePattern
	for i := range surfacePatterns {
		if surfacePatterns[i].Name == "work-management schema requires every mirror" {
			pat = &surfacePatterns[i]
		}
	}
	if pat == nil {
		t.Fatal("work-management pattern absent from the registry")
	}
	// Every path the sync-schemas work-management-v* arm writes, plus the
	// canonical source it reads, must be BOTH a trigger and a sibling —
	// the pattern is self-referential like its operator-role peers.
	for _, want := range []string{wmCanonical, wmMirror, wmCLIMirror} {
		if !slices.Contains(pat.Triggers, want) {
			t.Errorf("Triggers missing %q — a canonical edit skipping it would draw no finding", want)
		}
		if !slices.Contains(pat.Siblings, want) {
			t.Errorf("Siblings missing %q — a canonical edit skipping it would draw no finding", want)
		}
	}

	// Behavioral half: the canonical + backend mirror in scope, CLI mirror
	// absent, must now flag exactly the CLI mirror.
	got, _ := evaluateSurfaceSweep([]string{wmCanonical, wmMirror}, surfacePatterns, nil)
	var found bool
	for _, f := range got {
		if f.Pattern != "work-management schema requires every mirror" {
			continue
		}
		found = true
		if len(f.MissingSiblings) != 1 || f.MissingSiblings[0] != wmCLIMirror {
			t.Errorf("MissingSiblings = %v, want [%s]", f.MissingSiblings, wmCLIMirror)
		}
	}
	if !found {
		t.Errorf("want a work-management finding naming the unscoped CLI mirror; got %+v", got)
	}
}

// TestEvaluateCrossSliceCoupling is the pure detector test (#1102): a
// lockstep pattern split across slices is flagged (a); consolidated into one
// slice it is not (b); a slice with no declared scope is excluded (c); a
// single slice listing the same member twice collapses to one claimant (d).
func TestEvaluateCrossSliceCoupling(t *testing.T) {
	t.Run("split across two slices is flagged", func(t *testing.T) {
		p := crossSlicePlan([]subPlanScope{
			{title: "schema slice", files: []plan.ScopeFile{{Path: wmCanonical, Operation: plan.FileOpModify}}},
			{title: "wiring slice", files: []plan.ScopeFile{{Path: wmMirror, Operation: plan.FileOpModify}}},
		})
		got := evaluateCrossSliceCoupling(p, surfacePatterns)
		if len(got) != 1 {
			t.Fatalf("want 1 cross-slice finding, got %+v", got)
		}
		f := got[0]
		if f.Pattern != "work-management schema requires every mirror" {
			t.Errorf("Pattern = %q", f.Pattern)
		}
		if len(f.Slices) != 2 {
			t.Fatalf("want 2 slice claims, got %+v", f.Slices)
		}
		// Sorted by title: "schema slice" < "wiring slice".
		if f.Slices[0].SliceTitle != "schema slice" || f.Slices[1].SliceTitle != "wiring slice" {
			t.Errorf("slices not sorted by title: %+v", f.Slices)
		}
		if len(f.Slices[0].Files) != 1 || f.Slices[0].Files[0] != wmCanonical {
			t.Errorf("schema slice files = %v, want [%s]", f.Slices[0].Files, wmCanonical)
		}
		if len(f.Slices[1].Files) != 1 || f.Slices[1].Files[0] != wmMirror {
			t.Errorf("wiring slice files = %v, want [%s]", f.Slices[1].Files, wmMirror)
		}
	})

	t.Run("consolidated into one slice is not flagged", func(t *testing.T) {
		p := crossSlicePlan([]subPlanScope{
			{title: "schema slice", files: []plan.ScopeFile{
				{Path: wmCanonical, Operation: plan.FileOpModify},
				{Path: wmMirror, Operation: plan.FileOpModify},
			}},
			{title: "unrelated slice", files: []plan.ScopeFile{{Path: "backend/internal/bar/bar.go", Operation: plan.FileOpModify}}},
		})
		if got := evaluateCrossSliceCoupling(p, surfacePatterns); len(got) != 0 {
			t.Fatalf("want no cross-slice finding when consolidated, got %+v", got)
		}
	})

	t.Run("undeclared scope slice is excluded", func(t *testing.T) {
		// The mirror lives in a slice with no declared scope (inherits the
		// parent's full scope.files), so it cannot partition the pattern.
		p := crossSlicePlan([]subPlanScope{
			{title: "schema slice", files: []plan.ScopeFile{{Path: wmCanonical, Operation: plan.FileOpModify}}},
			{title: "inherits parent", files: nil},
		})
		if got := evaluateCrossSliceCoupling(p, surfacePatterns); len(got) != 0 {
			t.Fatalf("want no finding when the second slice declares no scope, got %+v", got)
		}
	})

	t.Run("same member listed twice in one slice collapses", func(t *testing.T) {
		p := crossSlicePlan([]subPlanScope{
			{title: "schema slice", files: []plan.ScopeFile{
				{Path: wmCanonical, Operation: plan.FileOpModify},
				{Path: wmCanonical, Operation: plan.FileOpModify},
			}},
		})
		if got := evaluateCrossSliceCoupling(p, surfacePatterns); len(got) != 0 {
			t.Fatalf("want no finding when one slice lists the same member twice, got %+v", got)
		}
	})

	t.Run("nil decomposition returns nil", func(t *testing.T) {
		if got := evaluateCrossSliceCoupling(&plan.Plan{}, surfacePatterns); got != nil {
			t.Fatalf("want nil for a non-decomposed plan, got %+v", got)
		}
	})
}

// TestRunSurfaceSweep_CrossSliceFindings is the end-to-end assertion: a
// decomposition that splits a lockstep pattern's members across two slices
// records the cross_slice_findings in the plan_surface_sweep audit payload.
func TestRunSurfaceSweep_CrossSliceFindings(t *testing.T) {
	s, au, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
	body := decomposedScopePlanBody(t,
		[]plan.ScopeFile{{Path: "backend/internal/foo/foo.go", Operation: plan.FileOpModify}},
		[]subPlanScope{
			{title: "schema slice", files: []plan.ScopeFile{{Path: wmCanonical, Operation: plan.FileOpModify}}},
			{title: "wiring slice", files: []plan.ScopeFile{{Path: wmMirror, Operation: plan.FileOpModify}}},
		},
	)

	got := s.runSurfaceSweep(context.Background(), runRow.ID, runRow.ID, body)
	if got == nil {
		t.Fatal("want a non-nil result when the sweep ran")
	}

	recorded := lastSurfaceSweepEntry(t, au)
	if len(recorded.CrossSliceFindings) != 1 {
		t.Fatalf("want 1 cross-slice finding in the audit payload, got %+v", recorded.CrossSliceFindings)
	}
	f := recorded.CrossSliceFindings[0]
	if f.Pattern != "work-management schema requires every mirror" {
		t.Errorf("Pattern = %q", f.Pattern)
	}
	if len(f.Slices) != 2 {
		t.Fatalf("want 2 slice claims, got %+v", f.Slices)
	}
	// Returned result matches the recorded payload (the #963 contract).
	gotJSON, _ := json.Marshal(got.CrossSliceFindings)
	recordedJSON, _ := json.Marshal(recorded.CrossSliceFindings)
	if string(gotJSON) != string(recordedJSON) {
		t.Errorf("returned cross-slice findings diverge from recorded:\nreturned: %s\nrecorded: %s", gotJSON, recordedJSON)
	}
}

// auditCategoryPlanBody builds a schema-valid flat plan whose approach names
// an audit category in the #3400 plan's verbatim phrasing, with the given
// scope.files and optional surface_sweep_exemptions (#3410).
func auditCategoryPlanBody(t *testing.T, files []plan.ScopeFile, exemptions []plan.SurfaceSweepExemption) []byte {
	t.Helper()
	fileMaps := make([]any, 0, len(files))
	for _, f := range files {
		fileMaps = append(fileMaps, map[string]any{"path": f.Path, "operation": string(f.Operation)})
	}
	m := planfixture.Valid(func(p map[string]any) {
		p["scope"] = map[string]any{"files": fileMaps}
		p["approach"] = []any{map[string]any{"step": 1, "description": "append an advisory `zz_never_registered_cat` audit row (system actor)"}}
		if len(exemptions) > 0 {
			exMaps := make([]any, 0, len(exemptions))
			for _, e := range exemptions {
				exMaps = append(exMaps, map[string]any{"pattern": e.Pattern, "sibling": e.Sibling, "reason": e.Reason})
			}
			p["surface_sweep_exemptions"] = exMaps
		}
	})
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if err := plan.Validate(body); err != nil {
		t.Fatalf("fixture plan does not validate: %v", err)
	}
	return body
}

// TestRunSurfaceSweep_AuditCategoryFinding is the #3410 done-means test on
// the REAL wiring: a plan whose prose names an unregistered audit category
// while scoping backend/ without backend/internal/audit/categories.go draws
// the finding through runSurfaceSweep AND the persisted plan_surface_sweep
// payload carries it; the same plan with the registry in scope draws none
// and the payload contains no "category" key (existing payloads stay
// byte-identical); the exemption form records an applied exemption.
func TestRunSurfaceSweep_AuditCategoryFinding(t *testing.T) {
	const registry = "backend/internal/audit/categories.go"
	foo := plan.ScopeFile{Path: "backend/internal/server/foo.go", Operation: plan.FileOpModify}

	t.Run("finding", func(t *testing.T) {
		s, au, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
		got := s.runSurfaceSweep(context.Background(), runRow.ID, runRow.ID, auditCategoryPlanBody(t, []plan.ScopeFile{foo}, nil))
		if got == nil {
			t.Fatal("want a non-nil result")
		}
		if len(got.Findings) != 1 {
			t.Fatalf("want 1 finding, got %+v", got.Findings)
		}
		f := got.Findings[0]
		if f.Pattern != "new audit category requires registry" || f.Category != "zz_never_registered_cat" ||
			f.SubPlanTitle != "" || len(f.MissingSiblings) != 1 || f.MissingSiblings[0] != registry ||
			f.TriggerPath != `audit category "zz_never_registered_cat" named at approach step 1` {
			t.Errorf("finding = %+v", f)
		}
		raw := lastSurfaceSweepRaw(t, au)
		for _, want := range []string{`"category":"zz_never_registered_cat"`, registry, `"pattern":"new audit category requires registry"`} {
			if !strings.Contains(raw, want) {
				t.Errorf("persisted payload missing %q:\n%s", want, raw)
			}
		}
	})
	t.Run("registry in scope → clean, no category key", func(t *testing.T) {
		s, au, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
		got := s.runSurfaceSweep(context.Background(), runRow.ID, runRow.ID,
			auditCategoryPlanBody(t, []plan.ScopeFile{foo, {Path: registry, Operation: plan.FileOpModify}}, nil))
		if got == nil || len(got.Findings) != 0 {
			t.Fatalf("want zero findings, got %+v", got)
		}
		if raw := lastSurfaceSweepRaw(t, au); strings.Contains(raw, `"category"`) {
			t.Errorf("clean payload must carry no category key:\n%s", raw)
		}
	})
	t.Run("exemption → applied, no finding", func(t *testing.T) {
		s, _, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
		got := s.runSurfaceSweep(context.Background(), runRow.ID, runRow.ID, auditCategoryPlanBody(t, []plan.ScopeFile{foo},
			[]plan.SurfaceSweepExemption{{Pattern: "new audit category requires registry", Sibling: registry, Reason: "it is a runner log event"}}))
		if got == nil || len(got.Findings) != 0 {
			t.Fatalf("want zero findings, got %+v", got)
		}
		if len(got.AppliedExemptions) != 1 || got.AppliedExemptions[0].Reason != "it is a runner log event" ||
			got.AppliedExemptions[0].Sibling != registry || got.AppliedExemptions[0].SubPlanTitle != "" {
			t.Errorf("applied = %+v", got.AppliedExemptions)
		}
	})
}

// lastSurfaceSweepRaw returns the single persisted plan_surface_sweep payload
// as raw JSON text so byte-level key presence can be asserted.
func lastSurfaceSweepRaw(t *testing.T, au *auditFake) string {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var raws []string
	for _, ap := range au.appended {
		if ap.Category == categoryPlanSurfaceSweep {
			raws = append(raws, string(ap.Payload))
		}
	}
	if len(raws) != 1 {
		t.Fatalf("want exactly 1 plan_surface_sweep entry, got %d", len(raws))
	}
	return raws[0]
}

// forbiddenCriterionPlanBody builds a schema-valid flat plan carrying the given
// scope.files, one acceptance criterion per statement, and optional
// surface_sweep_exemptions (#3620).
func forbiddenCriterionPlanBody(t *testing.T, files []plan.ScopeFile, statements []string, exemptions []plan.SurfaceSweepExemption) []byte {
	t.Helper()
	fileMaps := make([]any, 0, len(files))
	for _, f := range files {
		fileMaps = append(fileMaps, map[string]any{"path": f.Path, "operation": string(f.Operation)})
	}
	criteria := make([]any, 0, len(statements))
	for i, st := range statements {
		criteria = append(criteria, map[string]any{
			"id":          fmt.Sprintf("ac-%d", i+1),
			"statement":   st,
			"source":      "explicit",
			"verify_hint": "GET /v0/runs/{run_id}/audit carries the entry",
		})
	}
	m := planfixture.Valid(func(p map[string]any) {
		p["scope"] = map[string]any{"files": fileMaps}
		p["verification"] = map[string]any{
			"test_strategy":       "Run the tests.",
			"rollback_plan":       "Revert the PR.",
			"acceptance_criteria": criteria,
		}
		if len(exemptions) > 0 {
			exMaps := make([]any, 0, len(exemptions))
			for _, e := range exemptions {
				exMaps = append(exMaps, map[string]any{"pattern": e.Pattern, "sibling": e.Sibling, "reason": e.Reason})
			}
			p["surface_sweep_exemptions"] = exMaps
		}
	})
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if err := plan.Validate(body); err != nil {
		t.Fatalf("fixture plan does not validate: %v", err)
	}
	return body
}

// TestRunSurfaceSweep_ForbiddenCriterionFinding is the #3620 done-means test on
// the REAL wiring: a plan whose acceptance criterion names a path the run's
// cached workflow spec forbids the implement stage from touching draws the
// finding through runSurfaceSweep AND the PERSISTED plan_surface_sweep payload
// carries it with the forbidden_pattern / forbidden_path keys; a clean plan
// draws none and the payload carries NEITHER key (existing payloads stay
// byte-identical); a run with no workflow spec still records its entry with no
// forbidden-criterion finding (the fail-open contract).
func TestRunSurfaceSweep_ForbiddenCriterionFinding(t *testing.T) {
	foo := plan.ScopeFile{Path: "backend/internal/server/foo.go", Operation: plan.FileOpModify}

	t.Run("finding", func(t *testing.T) {
		s, au, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
		body := forbiddenCriterionPlanBody(t, []plan.ScopeFile{foo},
			[]string{"the workflow gate is wired in `.github/workflows/ci.yml`"}, nil)

		got := s.runSurfaceSweep(context.Background(), runRow.ID, runRow.ID, body)
		if got == nil {
			t.Fatal("want a non-nil result")
		}
		if len(got.Findings) != 1 {
			t.Fatalf("want 1 finding, got %+v", got.Findings)
		}
		f := got.Findings[0]
		if f.Pattern != "acceptance criterion requires a forbidden path" ||
			f.ForbiddenPath != ".github/workflows/ci.yml" ||
			f.ForbiddenPattern != ".github/workflows/**" ||
			f.TriggerPath != `path ".github/workflows/ci.yml" named at acceptance_criteria[ac-1].statement` ||
			f.SubPlanTitle != "" || f.Category != "" || len(f.MissingSiblings) != 0 {
			t.Errorf("finding = %+v", f)
		}
		raw := lastSurfaceSweepRaw(t, au)
		for _, want := range []string{
			`"pattern":"acceptance criterion requires a forbidden path"`,
			`"forbidden_pattern":".github/workflows/**"`,
			`"forbidden_path":".github/workflows/ci.yml"`,
			`acceptance_criteria[ac-1].statement`,
		} {
			if !strings.Contains(raw, want) {
				t.Errorf("persisted payload missing %q:\n%s", want, raw)
			}
		}
	})

	t.Run("clean plan → no finding, neither key in the payload", func(t *testing.T) {
		s, au, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
		body := forbiddenCriterionPlanBody(t, []plan.ScopeFile{foo},
			[]string{"GET /v0/runs/{run_id} returns the field wired in backend/internal/server/reads.go"}, nil)

		got := s.runSurfaceSweep(context.Background(), runRow.ID, runRow.ID, body)
		if got == nil || len(got.Findings) != 0 {
			t.Fatalf("want zero findings, got %+v", got)
		}
		raw := lastSurfaceSweepRaw(t, au)
		for _, absent := range []string{`"forbidden_pattern"`, `"forbidden_path"`} {
			if strings.Contains(raw, absent) {
				t.Errorf("clean payload must carry no %s key:\n%s", absent, raw)
			}
		}
		if want := `"findings":[]`; !strings.Contains(raw, want) {
			t.Errorf("clean payload missing %s:\n%s", want, raw)
		}
	})

	t.Run("exemption → applied, no finding", func(t *testing.T) {
		s, _, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
		body := forbiddenCriterionPlanBody(t, []plan.ScopeFile{foo},
			[]string{"the workflow gate is wired in `.github/workflows/ci.yml`"},
			[]plan.SurfaceSweepExemption{{
				Pattern: "acceptance criterion requires a forbidden path",
				Sibling: ".github/workflows/ci.yml",
				Reason:  "the criterion asserts the rendered doc NAMES the file",
			}})

		got := s.runSurfaceSweep(context.Background(), runRow.ID, runRow.ID, body)
		if got == nil || len(got.Findings) != 0 {
			t.Fatalf("want zero findings, got %+v", got)
		}
		if len(got.AppliedExemptions) != 1 ||
			got.AppliedExemptions[0].Pattern != "acceptance criterion requires a forbidden path" ||
			got.AppliedExemptions[0].Sibling != ".github/workflows/ci.yml" ||
			got.AppliedExemptions[0].Reason != "the criterion asserts the rendered doc NAMES the file" {
			t.Errorf("applied = %+v", got.AppliedExemptions)
		}
	})

	t.Run("fail-open: no workflow spec → entry recorded, no forbidden-criterion finding", func(t *testing.T) {
		s, au, runRow := newScopePrecheckServer(t, nil) // resolveImplementConstraints returns ok=false.
		body := forbiddenCriterionPlanBody(t, []plan.ScopeFile{foo},
			[]string{"the workflow gate is wired in `.github/workflows/ci.yml`"}, nil)

		got := s.runSurfaceSweep(context.Background(), runRow.ID, runRow.ID, body)
		if got == nil {
			t.Fatal("the sweep must still run when the spec read fails open")
		}
		if len(got.Findings) != 0 {
			t.Fatalf("want zero findings on the fail-open path, got %+v", got.Findings)
		}
		if raw := lastSurfaceSweepRaw(t, au); !strings.Contains(raw, `"findings":[]`) {
			t.Errorf("fail-open path must still record the entry:\n%s", raw)
		}
	})

	t.Run("fail-open: spec with no forbidden_paths → no finding", func(t *testing.T) {
		s, _, runRow := newScopePrecheckServer(t, specNoForbiddenPaths)
		body := forbiddenCriterionPlanBody(t, []plan.ScopeFile{foo},
			[]string{"the workflow gate is wired in `.github/workflows/ci.yml`"}, nil)

		got := s.runSurfaceSweep(context.Background(), runRow.ID, runRow.ID, body)
		if got == nil || len(got.Findings) != 0 {
			t.Fatalf("want zero findings when the implement stage configures no forbidden_paths, got %+v", got)
		}
	})
}

// specNoForbiddenPaths is a feature_change workflow whose implement stage
// resolves (ok=true) but configures NO forbidden_paths, so the
// forbidden-criterion rule is skipped on the empty-list branch.
var specNoForbiddenPaths = []byte(`version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - max_files_changed: 3
`)
