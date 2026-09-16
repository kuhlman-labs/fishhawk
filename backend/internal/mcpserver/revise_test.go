package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
)

// --- fishhawk_revise_plan (E22.X / #1099) ---

func TestRevisePlan_HappyPath_ReopensPlanStage(t *testing.T) {
	// A revise resolves the plan stage from the run id, re-opens it
	// awaiting_approval → pending, and threads the constraint into the
	// request body verbatim.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "awaiting_approval"}}
	fb.reviseResp[planStageID] = Stage{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "pending"}

	_, out, err := r.revisePlan(context.Background(), nil, RevisePlanInput{
		RunID:      runID.String(),
		Constraint: "use the existing retry helper, do not add a new backoff package",
	})
	if err != nil {
		t.Fatalf("revisePlan: %v", err)
	}
	if out.Stage.State != "pending" {
		t.Errorf("State = %q, want pending", out.Stage.State)
	}
	if out.StageID != planStageID.String() {
		t.Errorf("StageID = %q, want resolved plan stage %s", out.StageID, planStageID)
	}
	if fb.reviseCalledByID[planStageID] != 1 {
		t.Errorf("revise called %d times, want 1", fb.reviseCalledByID[planStageID])
	}
	if fb.reviseBody.Constraint != "use the existing retry helper, do not add a new backoff package" {
		t.Errorf("body constraint = %q, want the threaded constraint", fb.reviseBody.Constraint)
	}
}

func TestRevisePlan_ForceAdditionalPass_ThreadsIntoBody(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "awaiting_approval"}}

	_, _, err := r.revisePlan(context.Background(), nil, RevisePlanInput{
		RunID:               runID.String(),
		Constraint:          "one more tweak",
		ForceAdditionalPass: true,
	})
	if err != nil {
		t.Fatalf("revisePlan: %v", err)
	}
	if !fb.reviseBody.ForceAdditionalPass {
		t.Errorf("body force_additional_pass = false, want true (threaded override)")
	}
}

func TestRevisePlan_EmptyConstraint_FailsLocally(t *testing.T) {
	// An empty constraint is rejected before the HTTP hop — the run is
	// never even resolved.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "awaiting_approval"}}

	_, _, err := r.revisePlan(context.Background(), nil, RevisePlanInput{
		RunID:      runID.String(),
		Constraint: "   ",
	})
	if err == nil {
		t.Fatal("expected a local error for an empty constraint; got nil")
	}
	if fb.reviseCalledByID[planStageID] != 0 {
		t.Errorf("revise endpoint called %d times; want 0 (rejected before the hop)", fb.reviseCalledByID[planStageID])
	}
}

func TestRevisePlan_NoPlanStage_FailsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	fb.stagesByRun[runID] = nil // no plan stage

	_, _, err := r.revisePlan(context.Background(), nil, RevisePlanInput{
		RunID:      runID.String(),
		Constraint: "anything",
	})
	if err == nil {
		t.Fatal("expected an error when the run has no plan stage; got nil")
	}
	if !strings.Contains(err.Error(), "no plan stage") {
		t.Errorf("err = %v, want a no-plan-stage error", err)
	}
}

func TestRevisePlan_BudgetExhausted_PropagatesAs409(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.reviseStatus = http.StatusConflict
	fb.reviseErrBody = `{"error":{"code":"revise_budget_exhausted","message":"revise budget exhausted","details":{"max_passes":1,"used":1}}}`
	r := newResolver(srv, nil)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "awaiting_approval"}}

	_, _, err := r.revisePlan(context.Background(), nil, RevisePlanInput{
		RunID:      runID.String(),
		Constraint: "another tweak",
	})
	if err == nil {
		t.Fatal("expected error from backend 409; got nil")
	}
	if !strings.Contains(err.Error(), "revise_budget_exhausted") {
		t.Errorf("err = %v, want revise_budget_exhausted", err)
	}
}

func TestRevisePlan_CeilingReached_PropagatesAs409(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.reviseStatus = http.StatusConflict
	fb.reviseErrBody = `{"error":{"code":"revise_ceiling_reached","message":"revise ceiling reached","details":{"ceiling":3,"used":3}}}`
	r := newResolver(srv, nil)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "awaiting_approval"}}

	_, _, err := r.revisePlan(context.Background(), nil, RevisePlanInput{
		RunID:      runID.String(),
		Constraint: "past the ceiling",
	})
	if err == nil {
		t.Fatal("expected error from backend 409; got nil")
	}
	if !strings.Contains(err.Error(), "revise_ceiling_reached") {
		t.Errorf("err = %v, want revise_ceiling_reached", err)
	}
}

// TestRevisePlanDescription_DocumentsScopeRefusal pins the #2516 description
// change on the WIRE-VISIBLE tool description: the old text warned that a
// narrowly-scoped constraint can "silently DROP" files the prior plan scoped,
// which is no longer true — the gate REFUSES an undeclared narrowing and
// admits one only when the plan declares it in scope_removals. Without this
// pin the description could silently drift back to describing the old
// accepted-drop behaviour, which is the operator-facing surface that decides
// whether a driving agent trusts a revise with the rest of its scope.
func TestRevisePlanDescription_DocumentsScopeRefusal(t *testing.T) {
	ctx := context.Background()
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok"}
	srv := buildServer(cfg)
	resolver := &runResolver{api: newAPIClient(cfg), getenv: envFuncFromMap(nil)}
	registerTools(srv, resolver)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSession.Close()

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var desc string
	for _, tool := range res.Tools {
		if tool.Name == "fishhawk_revise_plan" {
			desc = tool.Description
			break
		}
	}
	if desc == "" {
		t.Fatal("fishhawk_revise_plan not registered/visible over ListTools")
	}
	lower := strings.ToLower(desc)
	for _, want := range []string{
		"refuses",        // the gate refuses, it does not merely surface
		"scope_removals", // the machine-readable declaration channel
		"carry-forward",  // the enumerated set the re-dispatch carries
		"zero reviewer",  // no reviewer pass is spent on a refused plan
		"budget",         // the residual one-shot refusal budget
	} {
		if !strings.Contains(lower, strings.ToLower(want)) {
			t.Errorf("fishhawk_revise_plan description missing %q (scope-refusal contract, #2516):\n%s", want, desc)
		}
	}
	// The stale claim must be gone: a drop is no longer silent.
	if strings.Contains(lower, "silently drop") {
		t.Errorf("description still claims a revise can 'silently DROP' files; the gate now refuses:\n%s", desc)
	}
}

// TestRevisePlanDescription_DocumentsConstraintCap is the done-means test for a
// DOCUMENTED CONSTANT (#2871): the 12000-byte cap and its refuse-not-truncate
// posture are values no compiler enforces, so a comment-only touch of revise.go
// would satisfy the scope-completeness gate while leaving the operator-facing
// surface describing behavior the server no longer has.
//
// It asserts BOTH operator-facing surfaces — the tool DESCRIPTION and the
// Constraint field's INPUT SCHEMA — because a driving agent reads whichever one
// its client renders, and the two desynchronising is exactly how an operator
// comes to believe a 20 KB constraint was accepted whole.
func TestRevisePlanDescription_DocumentsConstraintCap(t *testing.T) {
	ctx := context.Background()
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok"}
	srv := buildServer(cfg)
	resolver := &runResolver{api: newAPIClient(cfg), getenv: envFuncFromMap(nil)}
	registerTools(srv, resolver)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSession.Close()

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var tool *mcp.Tool
	for _, tl := range res.Tools {
		if tl.Name == "fishhawk_revise_plan" {
			tool = tl
			break
		}
	}
	if tool == nil {
		t.Fatal("fishhawk_revise_plan not registered/visible over ListTools")
	}

	capStr := strconv.Itoa(prompt.MaxRevisionConstraintBytes)
	for _, want := range []string{
		capStr,              // the concrete cap, keyed to the shared constant
		"byte",              // measured in bytes, not characters
		"refused",           // over-cap is refused …
		"validation_failed", // … with this error identity
		"never silently",    // … and never cut (the word wraps in the description)
		"truncated",         // … stated with the word an operator will grep for
	} {
		if !strings.Contains(strings.ToLower(tool.Description), strings.ToLower(want)) {
			t.Errorf("fishhawk_revise_plan description missing %q (constraint-cap contract, #2871):\n%s", want, tool.Description)
		}
	}

	// The Constraint property's own schema description must carry it too. The
	// wire-level InputSchema is opaque (any), so decode it the way a client
	// would rather than reaching into a Go struct the SDK does not expose here.
	if tool.InputSchema == nil {
		t.Fatal("fishhawk_revise_plan has no input schema")
	}
	rawSchema, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("marshal input schema: %v", err)
	}
	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(rawSchema, &schema); err != nil {
		t.Fatalf("decode input schema: %v (%s)", err, rawSchema)
	}
	prop, ok := schema.Properties["constraint"]
	if !ok {
		t.Fatalf("input schema has no 'constraint' property: %s", rawSchema)
	}
	for _, want := range []string{capStr, "byte", "refused", "validation_failed"} {
		if !strings.Contains(strings.ToLower(prop.Description), strings.ToLower(want)) {
			t.Errorf("constraint input-schema description missing %q (#2871):\n%s", want, prop.Description)
		}
	}
}

// --- revision-base elision surfaced at revise time (#3442) ---------------

// elidedRevisionBase seeds an over-cap digest assessment with two named
// elisions — the shape the backend returns for a >60 KB prior plan.
func elidedRevisionBase() *prompt.RevisionBaseAssessment {
	return &prompt.RevisionBaseAssessment{
		OriginalBytes: 63761, CapBytes: prompt.MaxRevisionBasePlanBytes, Elided: true,
		Mode: prompt.RevisionBaseModeDigest, RenderedBytes: 41000, ElidedBytes: 22761,
		Elisions: []prompt.RevisionBaseElision{
			{Field: "summary", BytesShown: 2000, BytesDropped: 3073},
			{Field: "approach step 4 body", BytesShown: 2000, BytesDropped: 900},
		},
		UnrenderedKeys: []string{"generated_by", "ticket_reference"},
	}
}

// firstText returns the first TextContent of a CallToolResult, "" when the
// result or its content is absent.
func firstText(res *mcp.CallToolResult) string {
	if res == nil || len(res.Content) == 0 {
		return ""
	}
	if tc, ok := res.Content[0].(*mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}

// TestRevisePlan_ElidedBase_SurfacesWarning: an elided revision_base on the
// backend's 200 body is surfaced THREE ways — the structured revision_base
// field, one warnings line, and a CallToolResult text WARNING equal to it —
// naming the byte accounting and the first named elision. Deleting the
// Elided branch in revisePlan reddens the warnings and result assertions.
func TestRevisePlan_ElidedBase_SurfacesWarning(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "awaiting_approval"}}
	fb.reviseResp[planStageID] = Stage{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "pending"}
	fb.reviseRevisionBase[planStageID] = elidedRevisionBase()

	res, out, err := r.revisePlan(context.Background(), nil, RevisePlanInput{RunID: runID.String(), Constraint: "keep it additive"})
	if err != nil {
		t.Fatalf("revisePlan: %v", err)
	}
	if out.Stage.State != "pending" {
		t.Errorf("State = %q, want pending (the stage still rides the output)", out.Stage.State)
	}
	if out.RevisionBase == nil || !out.RevisionBase.Elided || out.RevisionBase.Mode != prompt.RevisionBaseModeDigest {
		t.Fatalf("out.RevisionBase = %+v, want the elided digest assessment", out.RevisionBase)
	}
	if out.RevisionBase.OriginalBytes != 63761 || len(out.RevisionBase.Elisions) != 2 {
		t.Errorf("out.RevisionBase decoded (%d bytes, %d elisions), want (63761, 2)", out.RevisionBase.OriginalBytes, len(out.RevisionBase.Elisions))
	}
	if len(out.Warnings) != 1 {
		t.Fatalf("len(Warnings) = %d, want 1: %v", len(out.Warnings), out.Warnings)
	}
	w := out.Warnings[0]
	for _, want := range []string{
		"63761 bytes", fmt.Sprintf("%d-byte revision-base cap", prompt.MaxRevisionBasePlanBytes),
		"step-complete digest", "summary: 2000 of 5073 bytes shown", "generated_by",
		"risks_and_assumptions", "fishhawk_get_plan", "force_additional_pass", "reject",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("warning missing %q:\n%s", want, w)
		}
	}
	if got := firstText(res); got != w {
		t.Errorf("CallToolResult text = %q, want the warning verbatim %q", got, w)
	}
}

// TestRevisePlan_CutModeBase_WarningSaysCutNotDigest pins approval condition
// 1 on the MCP surface: a cut-mode assessment (a malformed or zero-step
// prior artifact) draws the distinct byte-cut sentence and never claims a
// step-complete digest.
func TestRevisePlan_CutModeBase_WarningSaysCutNotDigest(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "awaiting_approval"}}
	fb.reviseRevisionBase[planStageID] = &prompt.RevisionBaseAssessment{
		OriginalBytes: 70000, CapBytes: prompt.MaxRevisionBasePlanBytes, Elided: true,
		Mode: prompt.RevisionBaseModeCut, RenderedBytes: prompt.MaxRevisionBasePlanBytes, ElidedBytes: 10000,
	}

	res, out, err := r.revisePlan(context.Background(), nil, RevisePlanInput{RunID: runID.String(), Constraint: "keep it additive"})
	if err != nil {
		t.Fatalf("revisePlan: %v", err)
	}
	if len(out.Warnings) != 1 {
		t.Fatalf("len(Warnings) = %d, want 1", len(out.Warnings))
	}
	w := out.Warnings[0]
	if !strings.Contains(w, "byte-cut prefix") || !strings.Contains(w, "NOT a digest") {
		t.Errorf("cut-mode warning lacks the cut wording:\n%s", w)
	}
	if strings.Contains(w, "step-complete digest") {
		t.Errorf("cut-mode warning claims a step-complete digest the planner does not receive:\n%s", w)
	}
	if firstText(res) != w {
		t.Errorf("CallToolResult text differs from the warning")
	}
}

// TestRevisePlan_WholeBase_NoWarning: a present-but-not-elided revision_base
// rides the output with NO warning and a nil CallToolResult — the
// pre-#3442 result shape.
func TestRevisePlan_WholeBase_NoWarning(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "awaiting_approval"}}
	fb.reviseRevisionBase[planStageID] = &prompt.RevisionBaseAssessment{
		OriginalBytes: 18000, CapBytes: prompt.MaxRevisionBasePlanBytes, Mode: prompt.RevisionBaseModeWhole, RenderedBytes: 18000,
	}

	res, out, err := r.revisePlan(context.Background(), nil, RevisePlanInput{RunID: runID.String(), Constraint: "keep it additive"})
	if err != nil {
		t.Fatalf("revisePlan: %v", err)
	}
	if out.RevisionBase == nil || out.RevisionBase.Elided || out.RevisionBase.Mode != prompt.RevisionBaseModeWhole {
		t.Errorf("out.RevisionBase = %+v, want whole/not elided", out.RevisionBase)
	}
	if out.Warnings != nil {
		t.Errorf("Warnings = %v, want nil for a whole base", out.Warnings)
	}
	if res != nil {
		t.Errorf("CallToolResult = %+v, want nil for a whole base", res)
	}
}

// TestRevisePlan_LegacyBackend_NoRevisionBase: a backend serving the
// pre-#3442 Stage-only body degrades to a nil RevisionBase with no warning.
func TestRevisePlan_LegacyBackend_NoRevisionBase(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "awaiting_approval"}}

	res, out, err := r.revisePlan(context.Background(), nil, RevisePlanInput{RunID: runID.String(), Constraint: "keep it additive"})
	if err != nil {
		t.Fatalf("revisePlan: %v", err)
	}
	if out.RevisionBase != nil || out.Warnings != nil || res != nil {
		t.Errorf("legacy body yielded (revision_base=%+v, warnings=%v, result=%+v), want all nil", out.RevisionBase, out.Warnings, res)
	}
}

// TestRevisionBaseElisionWarning_BoundsNamedElisions: seven named elisions
// render exactly five plus "2 more"; the omitted counter from the bounded
// assessment list is folded into the count.
func TestRevisionBaseElisionWarning_BoundsNamedElisions(t *testing.T) {
	a := elidedRevisionBase()
	a.Elisions = nil
	for i := 1; i <= 7; i++ {
		a.Elisions = append(a.Elisions, prompt.RevisionBaseElision{Field: fmt.Sprintf("approach step %d body", i), BytesShown: 2000, BytesDropped: 500})
	}
	w := revisionBaseElisionWarning(a)
	for i := 1; i <= 5; i++ {
		if !strings.Contains(w, fmt.Sprintf("approach step %d body: 2000 of 2500 bytes shown", i)) {
			t.Errorf("warning missing named elision %d:\n%s", i, w)
		}
	}
	for i := 6; i <= 7; i++ {
		if strings.Contains(w, fmt.Sprintf("approach step %d body", i)) {
			t.Errorf("warning names elision %d past the %d bound:\n%s", i, maxRevisionBaseWarningElisions, w)
		}
	}
	if !strings.Contains(w, "; 2 more.") {
		t.Errorf("warning does not count the 2 unnamed elisions:\n%s", w)
	}
	a.ElisionsOmitted = 3
	if w := revisionBaseElisionWarning(a); !strings.Contains(w, "; 5 more.") {
		t.Errorf("warning does not fold elisions_omitted into the count:\n%s", w)
	}
	if revisionBaseElisionWarning(&prompt.RevisionBaseAssessment{Mode: prompt.RevisionBaseModeWhole}) != "" {
		t.Errorf("a whole base produced a warning")
	}
}

// TestRevisePlanDescription_DocumentsRevisionBaseElision pins the #3442
// operator-facing contract on the wire-visible tool description: the
// revision_base field, the 60000-byte cap, and that the two elided shapes
// (digest vs cut) are described distinctly.
func TestRevisePlanDescription_DocumentsRevisionBaseElision(t *testing.T) {
	ctx := context.Background()
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok"}
	srv := buildServer(cfg)
	resolver := &runResolver{api: newAPIClient(cfg), getenv: envFuncFromMap(nil)}
	registerTools(srv, resolver)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSession.Close()

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var tool *mcp.Tool
	for _, tl := range res.Tools {
		if tl.Name == "fishhawk_revise_plan" {
			tool = tl
			break
		}
	}
	if tool == nil {
		t.Fatal("fishhawk_revise_plan not registered/visible over ListTools")
	}
	lower := strings.ToLower(tool.Description)
	for _, want := range []string{
		"revision_base", strconv.Itoa(prompt.MaxRevisionBasePlanBytes),
		"step-complete digest", "byte-cut prefix", "not a digest", "plan_revised",
	} {
		if !strings.Contains(lower, strings.ToLower(want)) {
			t.Errorf("fishhawk_revise_plan description missing %q (#3442):\n%s", want, tool.Description)
		}
	}
	// The OUTPUT schema's revision_base description carries the mode split.
	if tool.OutputSchema == nil {
		t.Fatal("fishhawk_revise_plan has no output schema")
	}
	rawSchema, err := json.Marshal(tool.OutputSchema)
	if err != nil {
		t.Fatalf("marshal output schema: %v", err)
	}
	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(rawSchema, &schema); err != nil {
		t.Fatalf("decode output schema: %v (%s)", err, rawSchema)
	}
	prop, ok := schema.Properties["revision_base"]
	if !ok {
		t.Fatalf("output schema has no 'revision_base' property: %s", rawSchema)
	}
	for _, want := range []string{strconv.Itoa(prompt.MaxRevisionBasePlanBytes), "step-complete digest", "byte-cut prefix", "'cut'"} {
		if !strings.Contains(strings.ToLower(prop.Description), strings.ToLower(want)) {
			t.Errorf("revision_base output-schema description missing %q (#3442):\n%s", want, prop.Description)
		}
	}
}
