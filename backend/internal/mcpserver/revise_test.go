package mcpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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
