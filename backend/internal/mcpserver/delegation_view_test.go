package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/delegationview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// fishhawk_delegation (E76.1 / #3747).

// runDelegationTool drives the REAL tool handler against the fixture backend.
func runDelegationTool(t *testing.T, res RepoDelegationResult, in DelegationInput) (DelegationOutput, *fakeBackend) {
	t.Helper()
	fb, srv := newFakeBackend(t)
	fb.delegationResp = res
	r := newResolver(srv, nil)
	_, out, err := r.delegation(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("delegation tool: %v", err)
	}
	return out, fb
}

func delegationMarshalLen(t *testing.T, out DelegationOutput) int {
	t.Helper()
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return len(raw)
}

func toolWorkflow(t *testing.T, out DelegationOutput, id string) delegationview.WorkflowDelegation {
	t.Helper()
	for _, wf := range out.Workflows {
		if wf.ID == id {
			return wf
		}
	}
	t.Fatalf("workflow %q absent from the tool output (%d workflows)", id, len(out.Workflows))
	return delegationview.WorkflowDelegation{}
}

func toolMatrixByAction(m []delegationview.Action) map[string]delegationview.Action {
	out := make(map[string]delegationview.Action, len(m))
	for _, a := range m {
		out[a.Action] = a
	}
	return out
}

// committedDelegationView builds the response body from the repository's OWN
// .fishhawk/workflows.yaml — the SAME projection of the SAME committed spec that
// backend/internal/server/delegation_view_test.go asserts through the REST
// handler. That is the CROSS-BOUNDARY pin (approval condition 5): both sides
// assert on the same content, so a json-tag rename on either one fails.
//
// The two Go packages cannot share a helper (server imports mcpserver, not the
// reverse), so the shared thing is the committed spec file plus the shared pure
// projection, which is the strongest available form of "the same body".
func committedDelegationView(t *testing.T) RepoDelegationResult {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", ".fishhawk", "workflows.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the committed workflow spec: %v", err)
	}
	parsed, err := spec.ParseBytes(raw)
	if err != nil {
		t.Fatalf("the committed workflow spec does not parse: %v", err)
	}
	ws := delegationview.Project(parsed)
	return RepoDelegationResult{
		Repo: "kuhlman-labs/fishhawk", Source: "ref", Ref: "main",
		WorkflowSHA: "blobsha", SpecVersion: parsed.Version, SchemaMajor: spec.VersionMajor(parsed.Version),
		Workflows: ws, ContentHash: delegationview.HashWorkflows(ws),
	}
}

// featureChangeEscalationPaths is the glob set the committed spec declares.
var featureChangeEscalationPaths = []string{
	"backend/internal/spec/**",
	"cli/internal/spec/**",
	"backend/internal/audit/**",
	"backend/internal/policy/**",
	"backend/internal/auth/**",
	"backend/internal/githubapp/**",
	"verifier/**",
}

// TestDelegationTool_CarriesTheCommittedSpecsDelegation is the CROSS-BOUNDARY
// assertion (approval condition 5): the decoded TOOL output carries
// feature_change's tier, its resolved matrix WITH provenance, and its escalation
// globs — projected from the same committed spec the REST test drives.
func TestDelegationTool_CarriesTheCommittedSpecsDelegation(t *testing.T) {
	res := committedDelegationView(t)
	out, fb := runDelegationTool(t, res, DelegationInput{
		Repo: "kuhlman-labs/fishhawk", Ref: "main", Source: "ref"})

	// The repo went into the PATH and the two params onto the query string.
	fb.mu.Lock()
	gotPath := fb.lastDelegationPath
	fb.mu.Unlock()
	if !strings.HasPrefix(gotPath, "/v0/repos/kuhlman-labs/fishhawk/delegation?") {
		t.Errorf("dialed %q, want /v0/repos/kuhlman-labs/fishhawk/delegation with a query", gotPath)
	}
	if !strings.Contains(gotPath, "ref=main") || !strings.Contains(gotPath, "source=ref") {
		t.Errorf("dialed %q without the ref/source params", gotPath)
	}

	if out.Source != "ref" || out.Ref != "main" {
		t.Errorf("source/ref echo = %q/%q, want ref/main", out.Source, out.Ref)
	}
	if out.SchemaMajor != 2 || out.ContentHash == "" {
		t.Errorf("envelope = schema_major %d content_hash %q", out.SchemaMajor, out.ContentHash)
	}

	fc := toolWorkflow(t, out, "feature_change")
	if fc.Autonomy != "high" {
		t.Errorf("feature_change autonomy = %q, want high", fc.Autonomy)
	}
	if fc.ContentHash == "" {
		t.Error("the feature_change entry lost its content_hash through the tool")
	}
	matrix := toolMatrixByAction(fc.Matrix)
	for class, want := range map[string]struct{ mode, source, condition string }{
		"approve": {"auto", "tier", "clean_dual_approval"},
		"fixup":   {"auto", "tier", "convergent_concerns"},
		"waive":   {"auto", "tier", "solo_low"},
		"retry":   {"auto", "tier", "infra_flake"},
		"merge":   {"auto", "tier", "gates_resolved_ci_green"},
	} {
		a, ok := matrix[class]
		if !ok {
			t.Fatalf("class %q absent from the tool's matrix", class)
		}
		if a.Mode != want.mode || a.Source != want.source || a.Condition != want.condition {
			t.Errorf("%s = {mode:%q source:%q condition:%q}, want {%q %q %q}",
				class, a.Mode, a.Source, a.Condition, want.mode, want.source, want.condition)
		}
	}
	if len(fc.MustPageHuman) == 0 {
		t.Error("must_page_human did not survive the tool boundary")
	}
	if len(fc.Escalations) != 1 {
		t.Fatalf("feature_change has %d escalations, want 1", len(fc.Escalations))
	}
	e := fc.Escalations[0]
	if !reflect.DeepEqual(e.Match.Paths, featureChangeEscalationPaths) {
		t.Errorf("escalations[0].match.paths =\n %v\nwant the committed spec's seven globs:\n %v",
			e.Match.Paths, featureChangeEscalationPaths)
	}
	if e.MaxAutonomy != "medium" {
		t.Errorf("max_autonomy = %q, want medium", e.MaxAutonomy)
	}
	ceiling := toolMatrixByAction(e.CeilingMatrix)
	for _, class := range []string{"waive", "merge"} {
		if a := ceiling[class]; a.Mode != "gated" || a.Source != "escalation" {
			t.Errorf("ceiling_matrix %s = {%q %q}, want {gated escalation}", class, a.Mode, a.Source)
		}
	}
}

// TestDelegationTool_InertUnderBudget is the not-a-control control: a response
// that fits passes through byte-identical with NO elisions block.
func TestDelegationTool_InertUnderBudget(t *testing.T) {
	res := committedDelegationView(t)
	out, _ := runDelegationTool(t, res, DelegationInput{Repo: "kuhlman-labs/fishhawk"})
	if out.Elisions != nil {
		t.Errorf("elisions block present on an under-budget response: %+v", out.Elisions)
	}
	a, _ := json.Marshal(DelegationOutput{View: res})
	b, _ := json.Marshal(out)
	if string(a) != string(b) {
		t.Errorf("bounded render differs from the unbounded one:\n a = %s\n b = %s", a, b)
	}
}

// bigDelegationView builds a synthetic oversized view: n workflows, each with
// escalations escalations carrying globs globs.
func bigDelegationView(n, escalations, globs int) RepoDelegationResult {
	matrix := []delegationview.Action{
		{Action: "approve", Mode: "auto", Condition: "clean_dual_approval", Source: "tier"},
		{Action: "fixup", Mode: "auto", Condition: "convergent_concerns", Source: "tier"},
		{Action: "waive", Mode: "auto", Condition: "solo_low", Source: "tier"},
		{Action: "retry", Mode: "auto", Condition: "infra_flake", Source: "tier"},
		{Action: "merge", Mode: "auto", Condition: "gates_resolved_ci_green", Source: "tier"},
	}
	clamped := make([]delegationview.Action, len(matrix))
	for i, a := range matrix {
		clamped[i] = a
		if a.Action == "waive" || a.Action == "merge" {
			clamped[i].Mode, clamped[i].Condition, clamped[i].Source = "gated", "", "escalation"
		}
	}
	out := RepoDelegationResult{
		Repo: "acme/widgets", Source: "ref", Ref: "main", WorkflowSHA: "blobsha",
		SpecVersion: "2", SchemaMajor: 2, ContentHash: strings.Repeat("c", 64),
	}
	for w := 0; w < n; w++ {
		wf := delegationview.WorkflowDelegation{
			ID:            fmt.Sprintf("workflow_%03d", w),
			Autonomy:      "high",
			Matrix:        matrix,
			MustPageHuman: []string{"gating_reviewer_reject", "plan_rejection", "scope_amendment"},
			ContentHash:   strings.Repeat("w", 64),
		}
		for e := 0; e < escalations; e++ {
			paths := make([]string, 0, globs)
			for g := 0; g < globs; g++ {
				paths = append(paths, fmt.Sprintf("backend/internal/package_%03d_%03d/**", e, g))
			}
			wf.Escalations = append(wf.Escalations, delegationview.Escalation{
				Match:         delegationview.EscalationMatch{Paths: paths},
				MaxAutonomy:   "medium",
				CeilingMatrix: clamped,
			})
		}
		out.Workflows = append(out.Workflows, wf)
	}
	return out
}

// TestDelegationTool_WithinByteBudget: an oversized view is brought under the
// effective budget with the truncation MARKED.
// COUNTERFACTUAL: delete the boundDelegationOutput call from the handler → the
// over-budget response is returned and this is RED on the measured size.
func TestDelegationTool_WithinByteBudget(t *testing.T) {
	out, _ := runDelegationTool(t, bigDelegationView(30, 4, 20),
		DelegationInput{Repo: "acme/widgets"})
	budget := mcpResponseByteBudgetDefault
	if n := delegationMarshalLen(t, out); n > budget {
		t.Fatalf("marshalled response = %d bytes, want <= the %d-byte budget", n, budget)
	}
	if out.Elisions == nil {
		t.Fatal("the response was reduced but carries NO elisions block — a silently partial posture")
	}
	if out.Source != "ref" {
		t.Errorf("the source echo was dropped: %q", out.Source)
	}
}

// TestDelegationTool_B1DropsCeilingMatrixAndCapsMatchLists pins the FIRST tier
// firing on its own: a view just over budget purely because of its derived
// ceiling matrices and oversized glob lists keeps every workflow AND every
// resolved matrix, with the ceilings recorded as a COMPUTED elision (nothing
// stores them) and the match truncation as a STORED one.
func TestDelegationTool_B1DropsCeilingMatrixAndCapsMatchLists(t *testing.T) {
	out, _ := runDelegationTool(t, bigDelegationView(3, 6, 60),
		DelegationInput{Repo: "acme/widgets"})
	if out.Elisions == nil {
		t.Fatal("no elisions block")
	}
	if out.Elisions.Tier != "B1" {
		t.Fatalf("tier = %q, want B1 (the fixture is over budget only on its escalations)", out.Elisions.Tier)
	}
	if len(out.Workflows) != 3 {
		t.Errorf("workflows = %d, want all 3 retained at B1", len(out.Workflows))
	}
	for _, wf := range out.Workflows {
		if len(wf.Matrix) != 5 {
			t.Errorf("%s matrix = %d classes, want the resolved 5 retained at B1", wf.ID, len(wf.Matrix))
		}
		for _, e := range wf.Escalations {
			if len(e.CeilingMatrix) != 0 {
				t.Errorf("%s kept a ceiling_matrix at B1: %+v", wf.ID, e.CeilingMatrix)
			}
			if e.MaxAutonomy != "medium" {
				t.Errorf("%s dropped max_autonomy; WHICH ceiling applies must be retained", wf.ID)
			}
			if len(e.Match.Paths) > delegationFloorListCap {
				t.Errorf("%s match.paths = %d entries, want <= %d", wf.ID, len(e.Match.Paths), delegationFloorListCap)
			}
		}
	}
	fields := map[string]ElidedField{}
	for _, f := range out.Elisions.Fields {
		fields[f.Field] = f
	}
	ceil, ok := fields["workflows[].escalations[].ceiling_matrix"]
	if !ok {
		t.Fatalf("no ceiling_matrix elision recorded: %+v", out.Elisions.Fields)
	}
	if ceil.Class != "computed" || ceil.Pointer != "" {
		t.Errorf("ceiling_matrix is DERIVED — it must be class computed with no pointer, got class %q pointer %q", ceil.Class, ceil.Pointer)
	}
	match, ok := fields["workflows[].escalations[].match"]
	if !ok {
		t.Fatalf("no match elision recorded: %+v", out.Elisions.Fields)
	}
	if match.Class != "stored" || !strings.Contains(match.Pointer, "/delegation") {
		t.Errorf("the match truncation is STORED — it must name the REST delegation surface, got class %q pointer %q", match.Class, match.Pointer)
	}
}

// TestDelegationTool_B2DropsWorkflowsFromTheTail pins the SECOND tier: a view
// whose per-workflow content alone exceeds the budget drops workflows and says
// so, from the tail of the id-ordered list.
func TestDelegationTool_B2DropsWorkflowsFromTheTail(t *testing.T) {
	res := bigDelegationView(40, 2, 6)
	out, _ := runDelegationTool(t, res, DelegationInput{Repo: "acme/widgets"})
	if out.Elisions == nil {
		t.Fatal("no elisions block")
	}
	if out.Elisions.Tier != "B2" && out.Elisions.Tier != floorTierName {
		t.Fatalf("tier = %q, want B2 or the floor", out.Elisions.Tier)
	}
	if len(out.Workflows) >= len(res.Workflows) {
		t.Errorf("workflows = %d of %d — nothing was dropped", len(out.Workflows), len(res.Workflows))
	}
	if len(out.Workflows) > 0 && out.Workflows[0].ID != res.Workflows[0].ID {
		t.Errorf("the retained PREFIX starts at %q, want %q — the drop must be from the TAIL",
			out.Workflows[0].ID, res.Workflows[0].ID)
	}
	if n := delegationMarshalLen(t, out); n > mcpResponseByteBudgetDefault {
		t.Errorf("marshalled response = %d bytes, want <= %d", n, mcpResponseByteBudgetDefault)
	}
}

// TestDelegationTool_FloorIsBoundedByAnOversizedView is counterfactual (6)'s
// vehicle: the fixture is 40 workflows each carrying 12 escalations with 50
// globs, so the un-elided body is far above any budget and the FLOOR must still
// measure under mcpConvergenceFloorBytes. With the floor's matrix/escalations
// elision made a pass-through, this is RED on the measured size.
func TestDelegationTool_FloorIsBoundedByAnOversizedView(t *testing.T) {
	res := bigDelegationView(40, 12, 50)
	fb, srv := newFakeBackend(t)
	fb.delegationResp = res
	r := newResolver(srv, nil)
	// Drive the floor directly by giving the ladder a budget it cannot satisfy
	// above the floor, then assert the floor's own guarantee.
	out, err := boundDelegationOutput(DelegationOutput{View: res},
		responseBudget{bytes: 1, source: sourceDefault})
	if err != nil {
		t.Fatalf("bound: %v", err)
	}
	if out.Elisions == nil || out.Elisions.Tier != floorTierName {
		t.Fatalf("tier = %+v, want the floor", out.Elisions)
	}
	if n := delegationMarshalLen(t, out); n > mcpConvergenceFloorBytes {
		t.Fatalf("floor = %d bytes, want <= the %d-byte convergence floor", n, mcpConvergenceFloorBytes)
	}
	// Whatever survived keeps id + autonomy + content_hash and NOTHING else, so
	// a confirmation can still be bound to what was projected.
	for _, wf := range out.Workflows {
		if wf.ID == "" || wf.Autonomy == "" || wf.ContentHash == "" {
			t.Errorf("floor entry lost an identifying field: %+v", wf)
		}
		if len(wf.Matrix) != 0 || len(wf.Escalations) != 0 || len(wf.MustPageHuman) != 0 || wf.ModelPolicy != nil {
			t.Errorf("floor entry %s retained content: %+v", wf.ID, wf)
		}
	}
	if out.ContentHash != res.ContentHash {
		t.Errorf("the view-level content_hash changed at the floor: %q vs %q", out.ContentHash, res.ContentHash)
	}
	if len(out.Elisions.Fields) != 1 || !out.Elisions.Fields[0].Aggregate {
		t.Errorf("the floor must record exactly ONE aggregate entry, got %+v", out.Elisions.Fields)
	}
	// And through the real handler at the default budget it converges too.
	_, viaTool, err := r.delegation(context.Background(), nil, DelegationInput{Repo: "acme/widgets"})
	if err != nil {
		t.Fatalf("delegation tool: %v", err)
	}
	if n := delegationMarshalLen(t, viaTool); n > mcpResponseByteBudgetDefault {
		t.Errorf("via the tool = %d bytes, want <= %d", n, mcpResponseByteBudgetDefault)
	}
}

// TestDelegationTool_BackendErrorIsAToolErrorNotAnEmptyView: a backend refusal
// must surface as an error. An empty workflows array reads as "this repository
// delegates nothing", which is the opposite of "we could not tell you".
func TestDelegationTool_BackendErrorIsAToolErrorNotAnEmptyView(t *testing.T) {
	for _, status := range []int{http.StatusUnprocessableEntity, http.StatusForbidden,
		http.StatusNotFound, http.StatusServiceUnavailable, http.StatusBadRequest} {
		fb, srv := newFakeBackend(t)
		fb.delegationStatus = status
		r := newResolver(srv, nil)
		_, out, err := r.delegation(context.Background(), nil, DelegationInput{Repo: "acme/widgets"})
		if err == nil {
			t.Errorf("status %d: the tool returned no error; out = %+v", status, out)
			continue
		}
		if len(out.Workflows) != 0 {
			t.Errorf("status %d: the error path returned workflows: %+v", status, out.Workflows)
		}
	}
}
