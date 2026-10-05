package delegationview_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/delegationview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// specWith wraps one workflow body into a parseable workflow-v2 document.
func specWith(t *testing.T, body string) *spec.Spec {
	t.Helper()
	doc := "version: \"2\"\nworkflows:\n  wf:\n" + body
	parsed, err := spec.ParseBytes([]byte(doc))
	if err != nil {
		t.Fatalf("parse fixture spec: %v\n---\n%s", err, doc)
	}
	return parsed
}

// only returns the sole projected workflow, failing when the projection is not
// exactly one entry.
func only(t *testing.T, ws []delegationview.WorkflowDelegation) delegationview.WorkflowDelegation {
	t.Helper()
	if len(ws) != 1 {
		t.Fatalf("projected %d workflows, want exactly 1", len(ws))
	}
	return ws[0]
}

// byAction indexes a projected matrix by action class.
func byAction(m []delegationview.Action) map[string]delegationview.Action {
	out := make(map[string]delegationview.Action, len(m))
	for _, a := range m {
		out[a.Action] = a
	}
	return out
}

// minimalStages is the smallest stage list a v2 workflow needs to validate,
// and it declares a PLAN stage because an escalation matching on `paths` is
// refused on a plan-less workflow (spec.MsgFmtEscalationPathsNoPlanStage).
const minimalStages = `    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
`

// TestProject_TierPresetsResolveWithProvenance is the table over the three
// tier shorthands: each class's mode AND the provenance of that mode.
func TestProject_TierPresetsResolveWithProvenance(t *testing.T) {
	type want struct {
		mode      string
		source    string
		condition string
	}
	cases := map[string]map[string]want{
		"low": {
			"approve": {"gated", "tier", ""},
			"fixup":   {"gated", "tier", ""},
			"waive":   {"gated", "tier", ""},
			"retry":   {"gated", "tier", ""},
			"merge":   {"gated", "tier", ""},
		},
		"medium": {
			"approve": {"auto", "tier", "clean_dual_approval"},
			"fixup":   {"auto", "tier", "convergent_concerns"},
			"waive":   {"gated", "tier", ""},
			"retry":   {"auto", "tier", "infra_flake"},
			"merge":   {"gated", "tier", ""},
		},
		"high": {
			"approve": {"auto", "tier", "clean_dual_approval"},
			"fixup":   {"auto", "tier", "convergent_concerns"},
			"waive":   {"auto", "tier", "solo_low"},
			"retry":   {"auto", "tier", "infra_flake"},
			"merge":   {"auto", "tier", "gates_resolved_ci_green"},
		},
	}
	for tier, wants := range cases {
		t.Run(tier, func(t *testing.T) {
			wf := only(t, delegationview.Project(specWith(t,
				"    autonomy: "+tier+"\n"+minimalStages)))
			if wf.Autonomy != tier {
				t.Errorf("autonomy = %q, want %q", wf.Autonomy, tier)
			}
			got := byAction(wf.Matrix)
			if len(got) != len(wants) {
				t.Errorf("matrix has %d classes, want %d: %+v", len(got), len(wants), wf.Matrix)
			}
			for class, w := range wants {
				a, ok := got[class]
				if !ok {
					t.Fatalf("class %q absent from the resolved matrix", class)
				}
				if a.Mode != w.mode || a.Source != w.source || a.Condition != w.condition {
					t.Errorf("%s = {mode:%q source:%q condition:%q}, want {%q %q %q}",
						class, a.Mode, a.Source, a.Condition, w.mode, w.source, w.condition)
				}
			}
			// The page list is the tier's: absent at low, present above it.
			if tier == "low" && len(wf.MustPageHuman) != 0 {
				t.Errorf("low declares no page list, got %v", wf.MustPageHuman)
			}
			if tier != "low" && len(wf.MustPageHuman) == 0 {
				t.Errorf("%s must surface the tier page list", tier)
			}
		})
	}
}

// TestProject_ExplicitOverrideWinsForThatClassOnly: an `actions` block with no
// tier gives the named class source=explicit and every OTHER known class the
// fail-closed default (mode gated, source default) — never a tier value.
func TestProject_ExplicitOverrideWinsForThatClassOnly(t *testing.T) {
	wf := only(t, delegationview.Project(specWith(t, `    actions:
      merge:
        mode: gated
`+minimalStages)))
	if wf.Autonomy != "" {
		t.Errorf("autonomy = %q, want empty (only `actions` was declared)", wf.Autonomy)
	}
	got := byAction(wf.Matrix)
	if a := got["merge"]; a.Mode != "gated" || a.Source != "explicit" {
		t.Errorf("merge = {%q %q}, want {gated explicit}", a.Mode, a.Source)
	}
	for _, class := range []string{"approve", "fixup", "waive", "retry"} {
		a, ok := got[class]
		if !ok {
			t.Fatalf("known class %q absent from the resolved matrix", class)
		}
		if a.Mode != "gated" || a.Source != "default" {
			t.Errorf("%s = {%q %q}, want {gated default}", class, a.Mode, a.Source)
		}
	}
}

// highWithMediumCeiling is the escalation fixture. The tier is deliberately
// `high`: on a `low` workflow waive and merge are ALREADY gated, so the clamp
// would be unobservable and the test would pass with
// spec.ClampResolvedMatrix removed (counterfactual trap (d)).
const highWithMediumCeiling = `    autonomy: high
    escalations:
      - match:
          paths:
            - "backend/internal/spec/**"
        require:
          max_autonomy: medium
` + minimalStages

// TestProject_EscalationCeilingClampsWaiveAndMerge is counterfactual (4)'s
// vehicle: with spec.ClampResolvedMatrix removed from projectEscalation the
// ceiling matrix reports waive/merge still auto and this is RED.
func TestProject_EscalationCeilingClampsWaiveAndMerge(t *testing.T) {
	wf := only(t, delegationview.Project(specWith(t, highWithMediumCeiling)))
	if len(wf.Escalations) != 1 {
		t.Fatalf("projected %d escalations, want 1", len(wf.Escalations))
	}
	e := wf.Escalations[0]
	if e.MaxAutonomy != "medium" {
		t.Errorf("max_autonomy = %q, want medium", e.MaxAutonomy)
	}
	if got, want := e.Match.Paths, []string{"backend/internal/spec/**"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("match.paths = %v, want %v", got, want)
	}
	// The workflow matrix is UNCLAMPED: the projection is declarative, so the
	// ceiling shows up in ceiling_matrix and nowhere else.
	base := byAction(wf.Matrix)
	for _, class := range []string{"waive", "merge"} {
		if a := base[class]; a.Mode != "auto" || a.Source != "tier" {
			t.Errorf("workflow matrix %s = {%q %q}, want {auto tier} — the ceiling must not clamp the workflow matrix", class, a.Mode, a.Source)
		}
	}
	ceiling := byAction(e.CeilingMatrix)
	if len(ceiling) != len(base) {
		t.Errorf("ceiling_matrix has %d classes, want the matrix's %d", len(ceiling), len(base))
	}
	for _, class := range []string{"waive", "merge"} {
		a := ceiling[class]
		if a.Mode != "gated" || a.Source != "escalation" {
			t.Errorf("ceiling_matrix %s = {mode:%q source:%q}, want {gated escalation}", class, a.Mode, a.Source)
		}
		if a.Condition != "" {
			t.Errorf("ceiling_matrix %s kept condition %q; a clamped class delegates nothing", class, a.Condition)
		}
	}
	for _, class := range []string{"approve", "fixup", "retry"} {
		if a := ceiling[class]; a.Mode != "auto" || a.Source != "tier" {
			t.Errorf("ceiling_matrix %s = {%q %q}, want {auto tier} — a medium ceiling keeps these delegated", class, a.Mode, a.Source)
		}
	}
}

// TestProject_NoAutonomyBlockIsEmptyMatrixNotPanic: a workflow declaring
// neither `autonomy` nor `actions` resolves to nil and must project an EMPTY
// matrix (nothing delegated), never a nil dereference.
func TestProject_NoAutonomyBlockIsEmptyMatrixNotPanic(t *testing.T) {
	wf := only(t, delegationview.Project(specWith(t, minimalStages)))
	if wf.Autonomy != "" {
		t.Errorf("autonomy = %q, want empty", wf.Autonomy)
	}
	if len(wf.Matrix) != 0 {
		t.Errorf("matrix = %+v, want empty — an undeclared block delegates nothing", wf.Matrix)
	}
	raw, err := json.Marshal(wf.Matrix)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != "[]" {
		t.Errorf("matrix marshals to %s, want [] (never null)", raw)
	}
	if wf.ContentHash == "" {
		t.Error("an unconfigured workflow still needs a content hash")
	}
}

// TestProject_IsSortedByWorkflowID pins the ordering the hash rests on: a map
// range would randomize it.
func TestProject_IsSortedByWorkflowID(t *testing.T) {
	parsed, err := spec.ParseBytes([]byte(`version: "2"
workflows:
  zeta:
    autonomy: low
` + minimalStages + `  alpha:
    autonomy: high
` + minimalStages + `  mu:
    autonomy: medium
` + minimalStages))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := delegationview.Project(parsed)
	want := []string{"alpha", "mu", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("projected %d workflows, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("workflows[%d].id = %q, want %q", i, got[i].ID, id)
		}
	}
	// Re-projecting the SAME spec must produce a byte-identical view: the
	// determinism claim, not just the first ordering.
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(delegationview.Project(parsed))
	if string(a) != string(b) {
		t.Errorf("two projections of one spec differ:\n a = %s\n b = %s", a, b)
	}
}

// TestContentHash_StableAcrossRefAndSource is counterfactual (5)'s vehicle.
// ONE parsed spec is projected once and placed into TWO views carrying
// different (ref, workflow_sha, source) triples — seeded by construction, so
// hashing the whole View instead of the workflow slice makes the two differ.
func TestContentHash_StableAcrossRefAndSource(t *testing.T) {
	parsed := specWith(t, highWithMediumCeiling)
	ws := delegationview.Project(parsed)

	a := delegationview.View{
		Repo: "acme/app", Source: "ref", Ref: "main",
		WorkflowSHA: "aaaaaaa", SpecVersion: "2", SchemaMajor: 2,
		Workflows: ws, ContentHash: delegationview.HashWorkflows(ws),
	}
	b := delegationview.View{
		Repo: "other/repo", Source: "run_cache", Ref: "release/1.2",
		WorkflowSHA: "bbbbbbb", SpecVersion: "2", SchemaMajor: 2,
		Workflows: ws, ContentHash: delegationview.HashWorkflows(ws),
	}
	if a.ContentHash == "" {
		t.Fatal("content hash is empty")
	}
	if a.ContentHash != b.ContentHash {
		t.Errorf("content hash moved with the ref/source/sha triple:\n a = %s (ref %q source %q sha %q)\n b = %s (ref %q source %q sha %q)",
			a.ContentHash, a.Ref, a.Source, a.WorkflowSHA,
			b.ContentHash, b.Ref, b.Source, b.WorkflowSHA)
	}
	if a.Workflows[0].ContentHash != b.Workflows[0].ContentHash {
		t.Error("the per-workflow hash moved with the volatile fields too")
	}
	// Determinism: re-hashing the same projection twice is byte-identical.
	if again := delegationview.HashWorkflows(delegationview.Project(parsed)); again != a.ContentHash {
		t.Errorf("re-hashing one spec gave %s, want %s", again, a.ContentHash)
	}
}

// TestContentHash_ChangesOnTierAndEscalation is the OTHER direction of the
// pair: a constant-return hash would pass the stability test above, so this
// one flips the tier and appends a glob and requires the hash to MOVE.
func TestContentHash_ChangesOnTierAndEscalation(t *testing.T) {
	base := delegationview.HashWorkflows(delegationview.Project(specWith(t,
		"    autonomy: low\n"+minimalStages)))
	tierMoved := delegationview.HashWorkflows(delegationview.Project(specWith(t,
		"    autonomy: medium\n"+minimalStages)))
	if base == tierMoved {
		t.Errorf("low and medium hash identically (%s): the hash does not cover the tier", base)
	}

	oneGlob := delegationview.HashWorkflows(delegationview.Project(specWith(t, highWithMediumCeiling)))
	twoGlobs := delegationview.HashWorkflows(delegationview.Project(specWith(t, `    autonomy: high
    escalations:
      - match:
          paths:
            - "backend/internal/spec/**"
            - "backend/internal/audit/**"
        require:
          max_autonomy: medium
`+minimalStages)))
	if oneGlob == twoGlobs {
		t.Errorf("adding an escalation path glob left the hash at %s: the hash does not cover the escalation match", oneGlob)
	}
}

// TestProject_NilSpecIsEmptySlice: the degenerate input projects to `[]`, not
// nil, so the JSON surface never carries a null workflows array.
func TestProject_NilSpecIsEmptySlice(t *testing.T) {
	got := delegationview.Project(nil)
	if got == nil {
		t.Fatal("Project(nil) returned a nil slice")
	}
	if len(got) != 0 {
		t.Errorf("Project(nil) = %+v, want empty", got)
	}
}

// TestHashMatrix_DeterministicAndSensitive pins HashMatrix (E82.1 / #3778):
// equal content hashes equal, a change to one class's mode or source changes
// the digest, a nil and an empty matrix share one stable non-empty digest, and
// the digest IS sha256 over the E76.1 wire mirror — the same bytes the
// delegation read's projected `matrix` marshals to — so a shadow stamp's hash
// and this read cannot silently diverge.
func TestHashMatrix_DeterministicAndSensitive(t *testing.T) {
	parsed := specWith(t, "    autonomy: medium\n"+minimalStages)
	wf := parsed.Workflows["wf"]
	resolved := spec.ResolveAutonomy(&wf, nil)
	if resolved == nil || len(resolved.Actions) == 0 {
		t.Fatal("fixture resolved no matrix; every assertion below would be vacuous")
	}

	base := delegationview.HashMatrix(resolved.Actions)
	if base == "" {
		t.Fatal("HashMatrix returned an empty digest")
	}
	clone := append([]spec.ResolvedAction(nil), resolved.Actions...)
	if got := delegationview.HashMatrix(clone); got != base {
		t.Errorf("equal content hashed %s, want %s", got, base)
	}

	modeChanged := append([]spec.ResolvedAction(nil), resolved.Actions...)
	modeChanged[0].Mode = spec.ModeReport
	if delegationview.HashMatrix(modeChanged) == base {
		t.Errorf("a changed mode on %q did not change the digest", modeChanged[0].Action)
	}
	sourceChanged := append([]spec.ResolvedAction(nil), resolved.Actions...)
	sourceChanged[0].Source = spec.SourceEscalation
	if delegationview.HashMatrix(sourceChanged) == base {
		t.Errorf("a changed source on %q did not change the digest", sourceChanged[0].Action)
	}

	// The E76.1 mirror pin: the projected workflow's `matrix` is the wire
	// mirror of the same resolved actions, so sha256 over its JSON must be
	// the HashMatrix digest byte for byte.
	projected := only(t, delegationview.Project(parsed))
	raw, err := json.Marshal(projected.Matrix)
	if err != nil {
		t.Fatalf("marshal projected matrix: %v", err)
	}
	sum := sha256.Sum256(raw)
	if want := hex.EncodeToString(sum[:]); base != want {
		t.Errorf("HashMatrix = %s, want sha256 over the projected wire matrix %s", base, want)
	}

	empty := delegationview.HashMatrix(nil)
	if empty == "" {
		t.Error("HashMatrix(nil) is empty; 'no matrix' must be a stable non-empty digest")
	}
	if got := delegationview.HashMatrix([]spec.ResolvedAction{}); got != empty {
		t.Errorf("HashMatrix(empty) = %s, want the nil-matrix digest %s", got, empty)
	}
	if empty == base {
		t.Error("the no-matrix digest equals a resolved matrix's digest")
	}
}
