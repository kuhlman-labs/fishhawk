package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/delegationview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// pkgSrcDir is this package's SOURCE directory, captured once at package
// initialization (#4179): `go test` runs the test binary from within the
// package's source directory (go help testflag), and package-level variables
// initialize before any test runs, so this holds even if a test later changes
// the process cwd. Fixtures anchor here, never on runtime.Caller, whose file
// name is module-relative under -trimpath and so misses every fixture. The
// repo-wide guard is backend/internal/testanchor.
var pkgSrcDir = mustGetwd()

// mustGetwd returns os.Getwd() and panics on an error, so an unresolvable
// package dir fails the test binary closed at init.
func mustGetwd() string {
	dir, err := os.Getwd()
	if err != nil {
		panic(fmt.Sprintf("pkgSrcDir: os.Getwd: %v", err))
	}
	return dir
}

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
// handler. That is the CONTENT half of the cross-boundary pin (approval
// condition 5): both surfaces are asserted to carry feature_change's tier, its
// resolved matrix WITH provenance and its escalation globs, projected from one
// committed spec, so neither can be satisfied by a comment-only touch.
//
// WHAT THIS DOES NOT PIN, stated plainly because the first draft claimed it did:
// a json-tag rename. Both sides serialize AND decode the same shared type
// (delegationview.View; RepoDelegationResult is an alias of it and the REST
// body type IS it), so a renamed tag renames both halves of every round trip
// and no assertion made on Go FIELDS can see it. The tag NAMES are pinned
// separately, by TestDelegationTool_SerializedOutputCarriesTheDeclaredWireFieldNames
// below, which reads the registered tool's SERIALIZED output against field
// names written out as literals — and because the REST handler serializes that
// same shared type, that one test pins the names for both surfaces.
//
// The two Go packages cannot share a helper (server imports mcpserver, not the
// reverse), so the shared thing is the committed spec file plus the shared pure
// projection, which is the strongest available form of "the same body".
func committedDelegationView(t *testing.T) RepoDelegationResult {
	t.Helper()
	path := filepath.Join(pkgSrcDir, "..", "..", "..", ".fishhawk", "workflows.yaml")
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

// TestDelegationTool_CarriesTheCommittedSpecsDelegation is the CONTENT half of
// the cross-boundary assertion (approval condition 5): the decoded TOOL output
// carries feature_change's tier, its resolved matrix WITH provenance, and its
// escalation globs — projected from the same committed spec the REST test
// drives. It asserts Go fields, so it does NOT discriminate a json-tag rename;
// the wire NAMES are pinned by
// TestDelegationTool_SerializedOutputCarriesTheDeclaredWireFieldNames.
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

// ---------------------------------------------------------------------------
// WIRE-NAME EVIDENCE (approval condition 5, corrected)
// ---------------------------------------------------------------------------
//
// WHY A SECOND TEST RATHER THAN A STRONGER ASSERTION ON THE ONE ABOVE. The
// implement review was right that neither content test discriminates a json-tag
// rename. Both sides serialize and decode the SAME shared type
// (delegationview.View — RepoDelegationResult is an alias of it, the REST
// handler's body type IS it), and a Go field's tag applies to both halves of
// every round trip through it, so a rename is invisible to any assertion made
// on Go FIELDS. Value assertions there are meaningful as CONTENT parity; they
// are not wire-contract protection, and the comment on
// committedDelegationView says so.
//
// The pin has to read the SERIALIZED output against field names written HERE as
// independent literals. That is what the tests below do, and because the REST
// handler serializes the same shared type, one set of literals pins the tag
// names for BOTH surfaces.

// callDelegationToolWire drives the REGISTERED fishhawk_delegation tool over a
// real MCP client session and returns its structured output as GENERIC JSON —
// no production type is used to decode it, so every key below is a literal.
func callDelegationToolWire(t *testing.T, res RepoDelegationResult, args map[string]any) map[string]any {
	t.Helper()
	ctx := context.Background()
	fb, srv := newFakeBackend(t)
	fb.delegationResp = res

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0"}, nil)
	registerDelegation(server, newResolver(srv, nil))
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()
	clientSession, cerr := client.Connect(ctx, clientTransport, nil)
	if cerr != nil {
		t.Fatalf("client connect: %v", cerr)
	}
	defer clientSession.Close()

	out, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_delegation", Arguments: args})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if out.IsError {
		t.Fatalf("CallTool returned IsError; content %+v", out.Content)
	}
	if out.StructuredContent == nil {
		t.Fatal("StructuredContent is nil; the typed output did not serialize")
	}
	raw, merr := json.Marshal(out.StructuredContent)
	if merr != nil {
		t.Fatalf("marshal StructuredContent: %v", merr)
	}
	var generic map[string]any
	if uerr := json.Unmarshal(raw, &generic); uerr != nil {
		t.Fatalf("decode the tool output as generic JSON: %v; raw %s", uerr, raw)
	}
	return generic
}

// wireObj / wireArr / wireStr read one LITERAL key out of generic JSON. An
// ABSENT key is a distinct failure from a present-but-empty one, which is the
// whole point: a renamed json tag makes the key absent and these name it.
func wireObj(t *testing.T, v any, where string) map[string]any {
	t.Helper()
	o, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want a JSON object", where, v)
	}
	return o
}

func wireArr(t *testing.T, o map[string]any, key, where string) []any {
	t.Helper()
	v, present := o[key]
	if !present {
		t.Fatalf("%s carries NO %q key; the tool's serialized field names are: %v",
			where, key, wireKeys(o))
	}
	a, ok := v.([]any)
	if !ok {
		t.Fatalf("%s.%s is %T, want a JSON array", where, key, v)
	}
	return a
}

func wireStr(t *testing.T, o map[string]any, key, where string) string {
	t.Helper()
	v, present := o[key]
	if !present {
		t.Fatalf("%s carries NO %q key; the tool's serialized field names are: %v",
			where, key, wireKeys(o))
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("%s.%s is %T, want a JSON string", where, key, v)
	}
	return s
}

func wireKeys(o map[string]any) []string {
	keys := make([]string, 0, len(o))
	for k := range o {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// wireWorkflow finds one workflow entry by its literal "id" key.
func wireWorkflow(t *testing.T, root map[string]any, id string) map[string]any {
	t.Helper()
	for i, raw := range wireArr(t, root, "workflows", "the tool output") {
		wf := wireObj(t, raw, fmt.Sprintf("workflows[%d]", i))
		if wireStr(t, wf, "id", fmt.Sprintf("workflows[%d]", i)) == id {
			return wf
		}
	}
	t.Fatalf("no workflow with id %q in the tool's serialized output", id)
	return nil
}

// wireMatrix indexes a serialized matrix array by its literal "action" key.
func wireMatrix(t *testing.T, wf map[string]any, key, where string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for i, raw := range wireArr(t, wf, key, where) {
		a := wireObj(t, raw, fmt.Sprintf("%s.%s[%d]", where, key, i))
		out[wireStr(t, a, "action", fmt.Sprintf("%s.%s[%d]", where, key, i))] = a
	}
	return out
}

// TestDelegationTool_SerializedOutputCarriesTheDeclaredWireFieldNames is the
// wire-contract pin condition 5 actually needs: it reads the REGISTERED tool's
// SERIALIZED output as generic JSON and asserts each field by a name written
// here as a literal, against the same committed spec the REST test drives.
//
// COUNTERFACTUAL: rename any of these json tags on delegationview.View,
// WorkflowDelegation, Action, Escalation or EscalationMatch and this is RED,
// naming the key that went missing — while every Go-field assertion in this
// file and in backend/internal/server/delegation_view_test.go stays GREEN,
// because both of those decode through the renamed type.
func TestDelegationTool_SerializedOutputCarriesTheDeclaredWireFieldNames(t *testing.T) {
	root := callDelegationToolWire(t, committedDelegationView(t), map[string]any{
		"repo": "kuhlman-labs/fishhawk", "ref": "main", "source": "ref"})

	// The envelope, by literal name.
	if got := wireStr(t, root, "repo", "the tool output"); got != "kuhlman-labs/fishhawk" {
		t.Errorf(`"repo" = %q`, got)
	}
	if got := wireStr(t, root, "source", "the tool output"); got != "ref" {
		t.Errorf(`"source" = %q, want ref — the source is always echoed`, got)
	}
	if got := wireStr(t, root, "ref", "the tool output"); got != "main" {
		t.Errorf(`"ref" = %q`, got)
	}
	if got := wireStr(t, root, "workflow_sha", "the tool output"); got == "" {
		t.Error(`"workflow_sha" is empty`)
	}
	if got := wireStr(t, root, "spec_version", "the tool output"); got != "2" {
		t.Errorf(`"spec_version" = %q, want 2`, got)
	}
	if _, present := root["schema_major"]; !present {
		t.Errorf(`the tool output carries NO "schema_major" key; keys are %v`, wireKeys(root))
	}
	if got := wireStr(t, root, "content_hash", "the tool output"); got == "" {
		t.Error(`"content_hash" is empty`)
	}

	// feature_change, by literal name, carrying the SAME tier / matrix
	// provenance / escalation globs the REST test asserts on the same spec.
	fc := wireWorkflow(t, root, "feature_change")
	if got := wireStr(t, fc, "autonomy", "feature_change"); got != "high" {
		t.Errorf(`"autonomy" = %q, want high`, got)
	}
	if got := wireStr(t, fc, "content_hash", "feature_change"); got == "" {
		t.Error(`feature_change "content_hash" is empty`)
	}
	if len(wireArr(t, fc, "must_page_human", "feature_change")) == 0 {
		t.Error(`"must_page_human" is empty on the wire`)
	}
	matrix := wireMatrix(t, fc, "matrix", "feature_change")
	for class, want := range map[string]struct{ mode, source, condition string }{
		"approve": {"auto", "tier", "clean_dual_approval"},
		"fixup":   {"auto", "tier", "convergent_concerns"},
		"waive":   {"auto", "tier", "solo_low"},
		"retry":   {"auto", "tier", "infra_flake"},
		"merge":   {"auto", "tier", "gates_resolved_ci_green"},
	} {
		a, ok := matrix[class]
		if !ok {
			t.Fatalf("class %q absent from the serialized matrix (classes %v)", class, wireKeys(matrix2any(matrix)))
		}
		where := "feature_change.matrix[" + class + "]"
		if got := wireStr(t, a, "mode", where); got != want.mode {
			t.Errorf(`%s "mode" = %q, want %q`, where, got, want.mode)
		}
		if got := wireStr(t, a, "source", where); got != want.source {
			t.Errorf(`%s "source" = %q, want %q`, where, got, want.source)
		}
		if got := wireStr(t, a, "condition", where); got != want.condition {
			t.Errorf(`%s "condition" = %q, want %q`, where, got, want.condition)
		}
	}

	esc := wireArr(t, fc, "escalations", "feature_change")
	if len(esc) != 1 {
		t.Fatalf(`"escalations" has %d entries on the wire, want 1`, len(esc))
	}
	e := wireObj(t, esc[0], "feature_change.escalations[0]")
	if got := wireStr(t, e, "max_autonomy", "feature_change.escalations[0]"); got != "medium" {
		t.Errorf(`"max_autonomy" = %q, want medium`, got)
	}
	rawMatch, present := e["match"]
	if !present {
		t.Fatalf(`the escalation carries NO "match" key; keys are %v`, wireKeys(e))
	}
	match := wireObj(t, rawMatch, "feature_change.escalations[0].match")
	var paths []string
	for _, p := range wireArr(t, match, "paths", "feature_change.escalations[0].match") {
		s, ok := p.(string)
		if !ok {
			t.Fatalf(`"paths" element is %T, want a string`, p)
		}
		paths = append(paths, s)
	}
	if !reflect.DeepEqual(paths, featureChangeEscalationPaths) {
		t.Errorf(`"paths" =%v, want the committed spec's seven globs %v`, paths, featureChangeEscalationPaths)
	}
	ceiling := wireMatrix(t, e, "ceiling_matrix", "feature_change.escalations[0]")
	for _, class := range []string{"waive", "merge"} {
		a, ok := ceiling[class]
		if !ok {
			t.Fatalf("class %q absent from the serialized ceiling_matrix", class)
		}
		where := "feature_change.escalations[0].ceiling_matrix[" + class + "]"
		if got := wireStr(t, a, "mode", where); got != "gated" {
			t.Errorf(`%s "mode" = %q, want gated`, where, got)
		}
		if got := wireStr(t, a, "source", where); got != "escalation" {
			t.Errorf(`%s "source" = %q, want escalation`, where, got)
		}
	}
}

// matrix2any adapts a matrix index for wireKeys' diagnostic.
func matrix2any(m map[string]map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
