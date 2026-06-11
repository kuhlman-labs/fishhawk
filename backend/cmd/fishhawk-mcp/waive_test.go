package main

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
