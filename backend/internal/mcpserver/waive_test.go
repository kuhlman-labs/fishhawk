package mcpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// --- fishhawk_waive_concern (E22.X / #984) ---

func TestWaiveConcern_HappyPath(t *testing.T) {
	// The waive transitions the concern to waived with the operator's
	// reason as state_reason; the reason must reach the backend body
	// verbatim (it is the audited rationale).
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	concernID := uuid.New()
	fb.waiveResp[concernID] = WaivedConcern{
		ID:          concernID.String(),
		RunID:       uuid.NewString(),
		StageID:     uuid.NewString(),
		StageKind:   "implement",
		Severity:    "medium",
		Category:    "scope",
		Note:        "touched an out-of-scope file",
		State:       "waived",
		StateReason: "accepted trade-off: the doc companion is intentional",
	}

	_, out, err := r.waiveConcern(context.Background(), nil, WaiveConcernInput{
		ConcernID: concernID.String(),
		Reason:    "accepted trade-off: the doc companion is intentional",
	})
	if err != nil {
		t.Fatalf("waiveConcern: %v", err)
	}
	if out.Concern.State != "waived" {
		t.Errorf("State = %q, want waived", out.Concern.State)
	}
	if out.Concern.ID != concernID.String() {
		t.Errorf("ID = %q, want %s", out.Concern.ID, concernID.String())
	}
	if out.Concern.StateReason != "accepted trade-off: the doc companion is intentional" {
		t.Errorf("StateReason = %q, want the operator reason", out.Concern.StateReason)
	}
	if fb.waiveCalledByID[concernID] != 1 {
		t.Errorf("waive called %d times, want 1", fb.waiveCalledByID[concernID])
	}
	if fb.waiveBody.Reason != "accepted trade-off: the doc companion is intentional" {
		t.Errorf("body reason = %q, want the threaded reason", fb.waiveBody.Reason)
	}
}

func TestWaiveConcern_InvalidUUID_FailsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.waiveConcern(context.Background(), nil, WaiveConcernInput{
		ConcernID: "not-a-uuid",
		Reason:    "some reason",
	})
	if err == nil {
		t.Fatal("expected validation error for bad UUID")
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("err = %v, want UUID parse error", err)
	}
	// Local validation short-circuits — backend never called.
	if len(fb.waiveCalledByID) != 0 {
		t.Errorf("backend waive called %d times, want 0", len(fb.waiveCalledByID))
	}
}

func TestWaiveConcern_EmptyReason_FailsLocally(t *testing.T) {
	// The reason is required and audited; the tool short-circuits before
	// the HTTP hop so the backend's 400 is never needed.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.waiveConcern(context.Background(), nil, WaiveConcernInput{
		ConcernID: uuid.NewString(),
		Reason:    "   ",
	})
	if err == nil {
		t.Fatal("expected validation error for empty reason")
	}
	if !strings.Contains(err.Error(), "reason is required") {
		t.Errorf("err = %v, want empty-reason error", err)
	}
	if len(fb.waiveCalledByID) != 0 {
		t.Errorf("backend waive called %d times, want 0", len(fb.waiveCalledByID))
	}
}

func TestWaiveConcern_NotFound_PropagatesAsToolError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.waiveStatus = http.StatusNotFound
	fb.waiveErrBody = `{"error":{"code":"concern_not_found","message":"no concern with that id"}}`
	r := newResolver(srv, nil)

	_, _, err := r.waiveConcern(context.Background(), nil, WaiveConcernInput{
		ConcernID: uuid.NewString(),
		Reason:    "false positive",
	})
	if err == nil {
		t.Fatal("expected error from backend 404; got nil")
	}
	if !strings.Contains(err.Error(), "concern_not_found") {
		t.Errorf("err = %v, want concern_not_found", err)
	}
}

func TestWaiveConcern_Conflict_PropagatesAs422(t *testing.T) {
	// Waiving an already-terminal concern surfaces the backend's distinct
	// concern_waive_conflict code carrying the from/to pair.
	fb, srv := newFakeBackend(t)
	fb.waiveStatus = http.StatusUnprocessableEntity
	fb.waiveErrBody = `{"error":{"code":"concern_waive_conflict","message":"concern: invalid transition waived -> waived","details":{"from":"waived","to":"waived"}}}`
	r := newResolver(srv, nil)

	_, _, err := r.waiveConcern(context.Background(), nil, WaiveConcernInput{
		ConcernID: uuid.NewString(),
		Reason:    "waive it again",
	})
	if err == nil {
		t.Fatal("expected error from backend 422; got nil")
	}
	if !strings.Contains(err.Error(), "concern_waive_conflict") {
		t.Errorf("err = %v, want concern_waive_conflict", err)
	}
}

func TestWaiveConcern_CrossRun_PropagatesAs403(t *testing.T) {
	// A run-bound MCP token may waive only its own run's concerns; the
	// backend's subject-binding guard returns 403 cross_run_waive.
	fb, srv := newFakeBackend(t)
	fb.waiveStatus = http.StatusForbidden
	fb.waiveErrBody = `{"error":{"code":"cross_run_waive","message":"mcp token may only waive concerns within its own run"}}`
	r := newResolver(srv, nil)

	_, _, err := r.waiveConcern(context.Background(), nil, WaiveConcernInput{
		ConcernID: uuid.NewString(),
		Reason:    "not my run",
	})
	if err == nil {
		t.Fatal("expected error from backend 403; got nil")
	}
	if !strings.Contains(err.Error(), "cross_run_waive") {
		t.Errorf("err = %v, want cross_run_waive", err)
	}
}

// --- fishhawk_waive_concerns, the BULK verb (E64.77 / #3318) ---

func TestWaiveConcerns_HappyPath(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	ids := []string{uuid.NewString(), uuid.NewString()}
	const reason = "answered by the approval condition; not blocking the merge"

	_, out, err := r.waiveConcerns(context.Background(), nil, WaiveConcernsInput{
		RunID:      runID.String(),
		ConcernIDs: ids,
		Reason:     reason,
	})
	if err != nil {
		t.Fatalf("waiveConcerns: %v", err)
	}
	if out.Result.Waived != 2 || out.Result.Failed != 0 {
		t.Errorf("waived/failed = %d/%d, want 2/0: %+v", out.Result.Waived, out.Result.Failed, out.Result)
	}
	if len(out.Result.Results) != 2 {
		t.Fatalf("results = %d, want 2: %+v", len(out.Result.Results), out.Result.Results)
	}
	for i, want := range ids {
		if out.Result.Results[i].ConcernID != want {
			t.Errorf("results[%d].concern_id = %s, want %s (request order)", i, out.Result.Results[i].ConcernID, want)
		}
	}
	// The ids and the single reason reach the backend body verbatim: the reason
	// is the audited rationale every concern_waived entry carries.
	if len(fb.bulkWaiveBody.ConcernIDs) != 2 || fb.bulkWaiveBody.ConcernIDs[0] != ids[0] {
		t.Errorf("body concern_ids = %v, want %v", fb.bulkWaiveBody.ConcernIDs, ids)
	}
	if fb.bulkWaiveBody.Reason != reason {
		t.Errorf("body reason = %q, want the threaded reason", fb.bulkWaiveBody.Reason)
	}
	if want := "/v0/runs/" + runID.String() + "/concerns/waive"; fb.bulkWaivePath != want {
		t.Errorf("path = %q, want %q (run-scoped)", fb.bulkWaivePath, want)
	}
}

// TestWaiveConcerns_PartialFailure_SurfacedPerItem: a mid-batch failure is
// reported honestly through the tool rather than collapsed into one status.
func TestWaiveConcerns_PartialFailure_SurfacedPerItem(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	ids := []string{uuid.NewString(), uuid.NewString()}
	fb.bulkWaiveResp = &BulkWaiveResult{
		RunID: runID.String(), Waived: 1, Failed: 1,
		Results: []BulkWaiveItem{
			{ConcernID: ids[0], Applied: true, State: "waived"},
			{ConcernID: ids[1], Applied: false, ErrorCode: "concern_waive_conflict", Error: "raced"},
		},
	}

	_, out, err := r.waiveConcerns(context.Background(), nil, WaiveConcernsInput{
		RunID: runID.String(), ConcernIDs: ids, Reason: "r",
	})
	if err != nil {
		t.Fatalf("waiveConcerns: %v", err)
	}
	if out.Result.Waived != 1 || out.Result.Failed != 1 {
		t.Errorf("waived/failed = %d/%d, want 1/1", out.Result.Waived, out.Result.Failed)
	}
	if out.Result.Results[1].ErrorCode != "concern_waive_conflict" {
		t.Errorf("results[1].error_code = %q, want concern_waive_conflict", out.Result.Results[1].ErrorCode)
	}
}

// TestWaiveConcerns_InvalidRunID_FailsLocally: the pre-flight refuses BEFORE
// the HTTP hop — asserted by the backend never being called, not merely by an
// error being returned (a reachable backend would also error).
func TestWaiveConcerns_InvalidRunID_FailsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.waiveConcerns(context.Background(), nil, WaiveConcernsInput{
		RunID: "not-a-uuid", ConcernIDs: []string{uuid.NewString()}, Reason: "r",
	})
	if err == nil {
		t.Fatal("waiveConcerns = nil error, want a local refusal on a malformed run_id")
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("error = %v, want it to name the malformed run_id", err)
	}
	if fb.bulkWaivePath != "" {
		t.Errorf("backend called at %q despite a local pre-flight refusal", fb.bulkWaivePath)
	}
}

func TestWaiveConcerns_EmptyConcernIDs_FailsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.waiveConcerns(context.Background(), nil, WaiveConcernsInput{
		RunID: uuid.NewString(), Reason: "r",
	})
	if err == nil {
		t.Fatal("waiveConcerns = nil error, want a local refusal on an empty concern_ids")
	}
	if !strings.Contains(err.Error(), "at least one concern") {
		t.Errorf("error = %v, want it to name the empty list", err)
	}
	if fb.bulkWaivePath != "" {
		t.Errorf("backend called at %q despite a local pre-flight refusal", fb.bulkWaivePath)
	}
}

func TestWaiveConcerns_BlankReason_FailsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.waiveConcerns(context.Background(), nil, WaiveConcernsInput{
		RunID: uuid.NewString(), ConcernIDs: []string{uuid.NewString()}, Reason: "   ",
	})
	if err == nil {
		t.Fatal("waiveConcerns = nil error, want a local refusal on a blank reason")
	}
	if !strings.Contains(err.Error(), "reason is required") {
		t.Errorf("error = %v, want it to name the missing reason", err)
	}
	if fb.bulkWaivePath != "" {
		t.Errorf("backend called at %q despite a local pre-flight refusal", fb.bulkWaivePath)
	}
}

// TestWaiveConcerns_BackendRefusal_Surfaced: a server-side refusal (here the
// cross-run 400 carrying details.rule concern_run_mismatch) reaches the caller
// as a tool error rather than an empty success.
func TestWaiveConcerns_BackendRefusal_Surfaced(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	fb.bulkWaiveStatus = http.StatusBadRequest
	fb.bulkWaiveErrBody = `{"error":{"code":"validation_failed","message":"concern_ids references a concern from a different run","details":{"rule":"concern_run_mismatch"}}}`

	_, _, err := r.waiveConcerns(context.Background(), nil, WaiveConcernsInput{
		RunID: uuid.NewString(), ConcernIDs: []string{uuid.NewString()}, Reason: "r",
	})
	if err == nil {
		t.Fatal("waiveConcerns = nil error, want the backend refusal surfaced")
	}
	if !strings.Contains(err.Error(), "validation_failed") {
		t.Errorf("error = %v, want it to carry the backend error code", err)
	}
}
