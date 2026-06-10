package main

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// --- fishhawk_fixup_stage (E22.X / #762) ---

func TestFixupStage_HappyPath_ReopensToPending(t *testing.T) {
	// A fix-up re-opens the implement stage awaiting_approval → pending;
	// the orchestrator advances it (a backend-internal concern), so the
	// fixture returns State="pending". The selected concern indices and
	// reason must reach the backend body verbatim.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	stageID := uuid.New()
	runID := uuid.New()
	fb.fixupResp[stageID] = Stage{
		ID:    stageID.String(),
		RunID: runID.String(),
		Type:  "implement",
		State: "pending",
	}

	_, out, err := r.fixupStage(context.Background(), nil, FixupStageInput{
		StageID:  stageID.String(),
		Concerns: []int{0, 2},
		Reason:   "address the unhandled error path",
	})
	if err != nil {
		t.Fatalf("fixupStage: %v", err)
	}
	if out.Stage.State != "pending" {
		t.Errorf("State = %q, want pending", out.Stage.State)
	}
	if out.Stage.ID != stageID.String() {
		t.Errorf("ID = %q, want %s", out.Stage.ID, stageID.String())
	}
	if fb.fixupCalledByID[stageID] != 1 {
		t.Errorf("fixup called %d times, want 1", fb.fixupCalledByID[stageID])
	}
	if !reflect.DeepEqual(fb.fixupBody.Concerns, []int{0, 2}) {
		t.Errorf("body concerns = %v, want [0 2]", fb.fixupBody.Concerns)
	}
	if fb.fixupBody.Reason != "address the unhandled error path" {
		t.Errorf("body reason = %q, want the threaded reason", fb.fixupBody.Reason)
	}
}

func TestFixupStage_AllowCreate_ThreadsIntoBody(t *testing.T) {
	// allow_create (#823) must reach the backend request body verbatim so
	// the declared net-new files fold into the effective scope for the pass.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	stageID := uuid.New()
	runID := uuid.New()
	fb.fixupResp[stageID] = Stage{
		ID:    stageID.String(),
		RunID: runID.String(),
		Type:  "implement",
		State: "pending",
	}

	_, _, err := r.fixupStage(context.Background(), nil, FixupStageInput{
		StageID:     stageID.String(),
		Concerns:    []int{0},
		Reason:      "add the new helper file",
		AllowCreate: []string{"backend/internal/server/helper.go", "docs/api/v0.md"},
	})
	if err != nil {
		t.Fatalf("fixupStage: %v", err)
	}
	if !reflect.DeepEqual(fb.fixupBody.AllowCreate, []string{"backend/internal/server/helper.go", "docs/api/v0.md"}) {
		t.Errorf("body allow_create = %v, want the threaded paths", fb.fixupBody.AllowCreate)
	}
}

func TestFixupStage_ForceAdditionalPass_ThreadsIntoBody(t *testing.T) {
	// force_additional_pass (#860) must reach the backend request body so
	// the bounded operator override is honoured server-side.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	stageID := uuid.New()
	runID := uuid.New()
	fb.fixupResp[stageID] = Stage{
		ID:    stageID.String(),
		RunID: runID.String(),
		Type:  "implement",
		State: "pending",
	}

	_, _, err := r.fixupStage(context.Background(), nil, FixupStageInput{
		StageID:             stageID.String(),
		Concerns:            []int{0},
		Reason:              "grant one more pass",
		ForceAdditionalPass: true,
	})
	if err != nil {
		t.Fatalf("fixupStage: %v", err)
	}
	if !fb.fixupBody.ForceAdditionalPass {
		t.Errorf("body force_additional_pass = false, want true (threaded override)")
	}
}

func TestFixupStage_CeilingReached_PropagatesAs422(t *testing.T) {
	// At the hard ceiling the backend returns 422 with the DISTINCT code
	// fixup_ceiling_reached (#860). The MCP tool propagates it as a tool
	// error so the operator sees the hard stop, not budget_exhausted.
	fb, srv := newFakeBackend(t)
	fb.fixupStatus = http.StatusUnprocessableEntity
	fb.fixupErrBody = `{"error":{"code":"fixup_ceiling_reached","message":"fix-up ceiling reached","details":{"ceiling":3,"used":3}}}`
	r := newResolver(srv, nil)

	_, _, err := r.fixupStage(context.Background(), nil, FixupStageInput{
		StageID:  uuid.NewString(),
		Concerns: []int{0},
	})
	if err == nil {
		t.Fatal("expected error from backend 422; got nil")
	}
	if !strings.Contains(err.Error(), "fixup_ceiling_reached") {
		t.Errorf("err = %v, want fixup_ceiling_reached", err)
	}
}

func TestFixupStage_InvalidUUID_FailsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.fixupStage(context.Background(), nil, FixupStageInput{
		StageID:  "not-a-uuid",
		Concerns: []int{0},
	})
	if err == nil {
		t.Fatal("expected validation error for bad UUID")
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("err = %v, want UUID parse error", err)
	}
	// Local validation short-circuits — backend never called.
	if len(fb.fixupCalledByID) != 0 {
		t.Errorf("backend fixup called %d times, want 0", len(fb.fixupCalledByID))
	}
}

func TestFixupStage_EmptyConcerns_FailsLocally(t *testing.T) {
	// At least one concern must be selected; the tool short-circuits
	// before the HTTP hop so the backend's 400 is never needed.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.fixupStage(context.Background(), nil, FixupStageInput{
		StageID:  uuid.NewString(),
		Concerns: nil,
	})
	if err == nil {
		t.Fatal("expected validation error for empty concerns")
	}
	if !strings.Contains(err.Error(), "at least one") {
		t.Errorf("err = %v, want empty-concerns error", err)
	}
	if len(fb.fixupCalledByID) != 0 {
		t.Errorf("backend fixup called %d times, want 0", len(fb.fixupCalledByID))
	}
}

func TestFixupStage_NotFound_PropagatesAsToolError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.fixupStatus = http.StatusNotFound
	fb.fixupErrBody = `{"error":{"code":"stage_not_found","message":"no stage with that id"}}`
	r := newResolver(srv, nil)

	_, _, err := r.fixupStage(context.Background(), nil, FixupStageInput{
		StageID:  uuid.NewString(),
		Concerns: []int{0},
	})
	if err == nil {
		t.Fatal("expected error from backend 404; got nil")
	}
	if !strings.Contains(err.Error(), "stage_not_found") {
		t.Errorf("err = %v, want it to mention stage_not_found", err)
	}
}

func TestFixupStage_BudgetExhausted_PropagatesAs422(t *testing.T) {
	// The bound defaults to one pass; a second attempt surfaces as a 422
	// with code fixup_budget_exhausted. The MCP tool propagates the
	// error envelope verbatim so the operator sees the exhausted bound.
	fb, srv := newFakeBackend(t)
	fb.fixupStatus = http.StatusUnprocessableEntity
	fb.fixupErrBody = `{"error":{"code":"fixup_budget_exhausted","message":"fix-up budget exhausted","details":{"max_passes":1,"used":1}}}`
	r := newResolver(srv, nil)

	_, _, err := r.fixupStage(context.Background(), nil, FixupStageInput{
		StageID:  uuid.NewString(),
		Concerns: []int{0},
	})
	if err == nil {
		t.Fatal("expected error from backend 422; got nil")
	}
	if !strings.Contains(err.Error(), "fixup_budget_exhausted") {
		t.Errorf("err = %v, want fixup_budget_exhausted", err)
	}
}

func TestFixupStage_NotApplicable_PropagatesAs422(t *testing.T) {
	// A stage with no recorded approve_with_concerns implement-review
	// verdict has nothing to route back; the backend surfaces a 422 with
	// code fixup_not_applicable.
	fb, srv := newFakeBackend(t)
	fb.fixupStatus = http.StatusUnprocessableEntity
	fb.fixupErrBody = `{"error":{"code":"fixup_not_applicable","message":"no recorded approve_with_concerns implement-review concerns"}}`
	r := newResolver(srv, nil)

	_, _, err := r.fixupStage(context.Background(), nil, FixupStageInput{
		StageID:  uuid.NewString(),
		Concerns: []int{0},
	})
	if err == nil {
		t.Fatal("expected error from backend 422; got nil")
	}
	if !strings.Contains(err.Error(), "fixup_not_applicable") {
		t.Errorf("err = %v, want fixup_not_applicable", err)
	}
}

func TestFixupStage_CrossRun_PropagatesAs403(t *testing.T) {
	// A run-bound MCP token may fix up only stages within its own run;
	// the backend's subject-binding guard returns 403 cross_run_fixup,
	// which the tool propagates verbatim.
	fb, srv := newFakeBackend(t)
	fb.fixupStatus = http.StatusForbidden
	fb.fixupErrBody = `{"error":{"code":"cross_run_fixup","message":"mcp token may only fix up stages within its own run"}}`
	r := newResolver(srv, nil)

	_, _, err := r.fixupStage(context.Background(), nil, FixupStageInput{
		StageID:  uuid.NewString(),
		Concerns: []int{0},
	})
	if err == nil {
		t.Fatal("expected error from backend 403; got nil")
	}
	if !strings.Contains(err.Error(), "cross_run_fixup") {
		t.Errorf("err = %v, want cross_run_fixup", err)
	}
}

// --- stable concern-ID addressing (#964) ---

func TestFixupStage_ConcernIDs_ThreadIntoBody(t *testing.T) {
	// concern_ids (the PRIMARY addressing scheme) must reach the backend
	// request body verbatim, with the deprecated indices field absent.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	stageID := uuid.New()
	fb.fixupResp[stageID] = Stage{
		ID:    stageID.String(),
		RunID: uuid.NewString(),
		Type:  "implement",
		State: "pending",
	}
	id1, id2 := uuid.NewString(), uuid.NewString()

	_, out, err := r.fixupStage(context.Background(), nil, FixupStageInput{
		StageID:    stageID.String(),
		ConcernIDs: []string{id1, id2},
		Reason:     "route both by stable id",
	})
	if err != nil {
		t.Fatalf("fixupStage: %v", err)
	}
	if out.Stage.State != "pending" {
		t.Errorf("State = %q, want pending", out.Stage.State)
	}
	if !reflect.DeepEqual(fb.fixupBody.ConcernIDs, []string{id1, id2}) {
		t.Errorf("body concern_ids = %v, want [%s %s]", fb.fixupBody.ConcernIDs, id1, id2)
	}
	if len(fb.fixupBody.Concerns) != 0 {
		t.Errorf("body concerns = %v, want absent on the ID path", fb.fixupBody.Concerns)
	}
}

func TestFixupStage_BothAddressingForms_FailsLocally(t *testing.T) {
	// Mixed addressing short-circuits before the HTTP hop with the
	// deprecation messaging: concern_ids is primary, indices deprecated.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.fixupStage(context.Background(), nil, FixupStageInput{
		StageID:    uuid.NewString(),
		ConcernIDs: []string{uuid.NewString()},
		Concerns:   []int{0},
	})
	if err == nil {
		t.Fatal("expected validation error for mixed addressing")
	}
	if !strings.Contains(err.Error(), "not both") {
		t.Errorf("err = %v, want both-forms rejection", err)
	}
	if !strings.Contains(err.Error(), "deprecated") {
		t.Errorf("err = %v, want the deprecation called out", err)
	}
	if len(fb.fixupCalledByID) != 0 {
		t.Errorf("backend fixup called %d times, want 0", len(fb.fixupCalledByID))
	}
}

func TestFixupStage_NeitherAddressingForm_MessagePointsAtConcernIDs(t *testing.T) {
	// The empty-selection error steers the operator at concern_ids (the
	// primary scheme) and names the positional field as deprecated.
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.fixupStage(context.Background(), nil, FixupStageInput{
		StageID: uuid.NewString(),
	})
	if err == nil {
		t.Fatal("expected validation error for no concern selection")
	}
	if !strings.Contains(err.Error(), "concern_ids") {
		t.Errorf("err = %v, want concern_ids named as the primary scheme", err)
	}
	if !strings.Contains(err.Error(), "deprecated") {
		t.Errorf("err = %v, want the positional fallback marked deprecated", err)
	}
}
