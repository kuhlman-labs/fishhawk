package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// --- fishhawk_defer_concern (E22.X / #1202) ---

func TestDeferConcern_HappyPath(t *testing.T) {
	// The defer files a follow-up and transitions the concern to deferred;
	// the title coordinates must reach the backend body verbatim, and the
	// filed issue + updated concern round-trip back.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	concernID := uuid.New()
	fb.deferResp[concernID] = DeferredConcernResult{
		Concern: DeferredConcern{
			ID:          concernID.String(),
			RunID:       uuid.NewString(),
			StageKind:   "implement",
			Severity:    "medium",
			Category:    "scope",
			State:       "deferred",
			StateReason: "deferred to #4242: split it out",
		},
		Issue: DeferFiledIssue{
			Type:   "chore",
			Title:  "[E22.4] split it out",
			Number: 4242,
			URL:    "https://github.com/kuhlman-labs/fishhawk/issues/4242",
		},
	}

	_, out, err := r.deferConcern(context.Background(), nil, DeferConcernInput{
		ConcernID:  concernID.String(),
		ParentEpic: "#1196",
		N:          "4",
		Note:       "split it out",
	})
	if err != nil {
		t.Fatalf("deferConcern: %v", err)
	}
	if out.Concern.State != "deferred" {
		t.Errorf("State = %q, want deferred", out.Concern.State)
	}
	if out.Issue.Number != 4242 || out.Issue.URL == "" {
		t.Errorf("filed issue not echoed: %+v", out.Issue)
	}
	if fb.deferCalledByID[concernID] != 1 {
		t.Errorf("defer called %d times, want 1", fb.deferCalledByID[concernID])
	}
	if fb.deferBody.ParentEpic != "#1196" || fb.deferBody.N != "4" || fb.deferBody.Note != "split it out" {
		t.Errorf("body = %+v, want the threaded coordinates", fb.deferBody)
	}
}

func TestDeferConcern_InvalidUUID_FailsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.deferConcern(context.Background(), nil, DeferConcernInput{
		ConcernID:  "not-a-uuid",
		ParentEpic: "#1196",
		N:          "4",
	})
	if err == nil {
		t.Fatal("expected validation error for bad UUID")
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("err = %v, want UUID parse error", err)
	}
	// Local validation short-circuits — backend never called.
	if len(fb.deferCalledByID) != 0 {
		t.Errorf("backend defer called %d times, want 0", len(fb.deferCalledByID))
	}
}

func TestDeferConcern_NotFound_PropagatesAsToolError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.deferStatus = http.StatusNotFound
	fb.deferErrBody = `{"error":{"code":"concern_not_found","message":"no concern with that id"}}`
	r := newResolver(srv, nil)

	_, _, err := r.deferConcern(context.Background(), nil, DeferConcernInput{
		ConcernID:  uuid.NewString(),
		ParentEpic: "#1196",
		N:          "4",
	})
	if err == nil {
		t.Fatal("expected error from backend 404; got nil")
	}
	if !strings.Contains(err.Error(), "concern_not_found") {
		t.Errorf("err = %v, want concern_not_found", err)
	}
}

func TestDeferConcern_Conflict_PropagatesAs422(t *testing.T) {
	// Deferring a non-open concern surfaces the backend's distinct
	// concern_defer_conflict code.
	fb, srv := newFakeBackend(t)
	fb.deferStatus = http.StatusUnprocessableEntity
	fb.deferErrBody = `{"error":{"code":"concern_defer_conflict","message":"concern is not in an open state","details":{"state":"waived"}}}`
	r := newResolver(srv, nil)

	_, _, err := r.deferConcern(context.Background(), nil, DeferConcernInput{
		ConcernID:  uuid.NewString(),
		ParentEpic: "#1196",
		N:          "4",
	})
	if err == nil {
		t.Fatal("expected error from backend 422; got nil")
	}
	if !strings.Contains(err.Error(), "concern_defer_conflict") {
		t.Errorf("err = %v, want concern_defer_conflict", err)
	}
}

func TestDeferConcern_CrossRun_PropagatesAs403(t *testing.T) {
	// A run-bound MCP token may defer only its own run's concerns; the
	// backend's subject-binding guard returns 403 cross_run_defer.
	fb, srv := newFakeBackend(t)
	fb.deferStatus = http.StatusForbidden
	fb.deferErrBody = `{"error":{"code":"cross_run_defer","message":"mcp token may only defer concerns within its own run"}}`
	r := newResolver(srv, nil)

	_, _, err := r.deferConcern(context.Background(), nil, DeferConcernInput{
		ConcernID:  uuid.NewString(),
		ParentEpic: "#1196",
		N:          "4",
	})
	if err == nil {
		t.Fatal("expected error from backend 403; got nil")
	}
	if !strings.Contains(err.Error(), "cross_run_defer") {
		t.Errorf("err = %v, want cross_run_defer", err)
	}
}

func TestDeferConcern_FilingFailure_PropagatesAs502(t *testing.T) {
	// A provider filing failure surfaces as 502 work_item_filing_failed —
	// the concern stays OPEN server-side (the tool only relays the error).
	fb, srv := newFakeBackend(t)
	fb.deferStatus = http.StatusBadGateway
	fb.deferErrBody = `{"error":{"code":"work_item_filing_failed","message":"provider could not file the work item"}}`
	r := newResolver(srv, nil)

	_, _, err := r.deferConcern(context.Background(), nil, DeferConcernInput{
		ConcernID:  uuid.NewString(),
		ParentEpic: "#1196",
		N:          "4",
	})
	if err == nil {
		t.Fatal("expected error from backend 502; got nil")
	}
	if !strings.Contains(err.Error(), "work_item_filing_failed") {
		t.Errorf("err = %v, want work_item_filing_failed", err)
	}
}
