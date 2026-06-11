package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- fishhawk_list_scope_amendments / fishhawk_decide_scope_amendment (#961) ---

func TestListScopeAmendments_HappyPath(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	fb.amendmentsByRun[runID] = []ScopeAmendmentItem{
		{
			ID:     uuid.New().String(),
			RunID:  runID.String(),
			Status: "pending",
			Reason: "the seam needs these",
			Paths: []ScopeAmendmentPath{
				{Path: "pkg/extra.go", Operation: "modify"},
				{Path: "pkg/newfile.go", Operation: "create"},
			},
		},
	}

	_, out, err := r.listScopeAmendments(context.Background(), nil, ListScopeAmendmentsInput{
		RunID: runID.String(),
	})
	if err != nil {
		t.Fatalf("listScopeAmendments: %v", err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(out.Items))
	}
	got := out.Items[0]
	if got.Status != "pending" || len(got.Paths) != 2 || got.Paths[1].Operation != "create" {
		t.Errorf("item = %+v", got)
	}
}

func TestListScopeAmendments_EmptyRun(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, out, err := r.listScopeAmendments(context.Background(), nil, ListScopeAmendmentsInput{
		RunID: uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("listScopeAmendments: %v", err)
	}
	if len(out.Items) != 0 {
		t.Errorf("items = %+v, want empty", out.Items)
	}
}

func TestListScopeAmendments_InvalidUUID(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.listScopeAmendments(context.Background(), nil, ListScopeAmendmentsInput{
		RunID: "not-a-uuid",
	})
	if err == nil || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("err = %v, want UUID validation error before the HTTP hop", err)
	}
}

func TestDecideScopeAmendment_ApproveThreadsBody(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	amendmentID := uuid.New()
	fb.decideAmendmentResp[amendmentID] = ScopeAmendmentItem{
		ID:             amendmentID.String(),
		RunID:          runID.String(),
		Status:         "approved",
		DecisionReason: "the seam is real",
		DecidedBy:      "github:operator",
	}

	_, out, err := r.decideScopeAmendment(context.Background(), nil, DecideScopeAmendmentInput{
		RunID:       runID.String(),
		AmendmentID: amendmentID.String(),
		Decision:    "approve",
		Reason:      "the seam is real",
	})
	if err != nil {
		t.Fatalf("decideScopeAmendment: %v", err)
	}
	if out.Amendment.Status != "approved" || out.Amendment.DecidedBy != "github:operator" {
		t.Errorf("amendment = %+v", out.Amendment)
	}
	if fb.decideCalledByID[amendmentID] != 1 {
		t.Errorf("decision called %d times, want 1", fb.decideCalledByID[amendmentID])
	}
	if fb.decideAmendmentBody.Decision != "approve" || fb.decideAmendmentBody.Reason != "the seam is real" {
		t.Errorf("body = %+v, want the threaded decision + reason", fb.decideAmendmentBody)
	}
}

func TestDecideScopeAmendment_RejectsBadDecisionBeforeHTTP(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	amendmentID := uuid.New()

	_, _, err := r.decideScopeAmendment(context.Background(), nil, DecideScopeAmendmentInput{
		RunID:       uuid.New().String(),
		AmendmentID: amendmentID.String(),
		Decision:    "maybe",
	})
	if err == nil || !strings.Contains(err.Error(), "approve") {
		t.Errorf("err = %v, want decision validation error", err)
	}
	if fb.decideCalledByID[amendmentID] != 0 {
		t.Errorf("backend hit despite invalid decision")
	}
}

func TestDecideScopeAmendment_BackendErrorsSurfaced(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		errBody string
		wantSub string
	}{
		{"already decided", http.StatusConflict,
			`{"error":{"code":"amendment_already_decided","message":"this scope amendment has already been decided"}}`,
			"amendment_already_decided"},
		{"self decision", http.StatusForbidden,
			`{"error":{"code":"self_decision","message":"a run-bound agent token may not decide a scope amendment"}}`,
			"self_decision"},
		{"not found", http.StatusNotFound,
			`{"error":{"code":"amendment_not_found","message":"no scope amendment with that id"}}`,
			"amendment_not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			fb.decideAmendmentState = tc.status
			fb.decideAmendmentErr = tc.errBody
			r := newResolver(srv, nil)

			_, _, err := r.decideScopeAmendment(context.Background(), nil, DecideScopeAmendmentInput{
				RunID:       uuid.New().String(),
				AmendmentID: uuid.New().String(),
				Decision:    "approve",
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v, want %q surfaced", err, tc.wantSub)
			}
		})
	}
}

func TestListScopeAmendments_BudgetExhaustedErrorSurfaced(t *testing.T) {
	// The amendment_budget_exhausted code originates on the agent's
	// POST, but an operator-side list against a failed backend must
	// surface the envelope code the same way.
	fb, srv := newFakeBackend(t)
	fb.amendmentsStatus = http.StatusUnprocessableEntity
	fb.amendmentsErrBody = `{"error":{"code":"amendment_budget_exhausted","message":"budget spent","details":{"max":2,"used":2}}}`
	r := newResolver(srv, nil)

	_, _, err := r.listScopeAmendments(context.Background(), nil, ListScopeAmendmentsInput{
		RunID: uuid.New().String(),
	})
	if err == nil || !strings.Contains(err.Error(), "amendment_budget_exhausted") {
		t.Errorf("err = %v, want amendment_budget_exhausted surfaced", err)
	}
}

// --- Scope-cap headroom fields + warning (#983) ---

func headroomInt(v int) *int { return &v }

func TestListScopeAmendments_OverCapHeadroom_RendersWarning(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	itemID := uuid.New().String()
	fb.amendmentsByRun[runID] = []ScopeAmendmentItem{
		{
			ID:     itemID,
			RunID:  runID.String(),
			Status: "pending",
			Reason: "the seam needs these",
			Paths: []ScopeAmendmentPath{
				{Path: "pkg/extra.go", Operation: "modify"},
			},
			EffectiveScopeFilesAfterApproval: headroomInt(31),
			MaxFilesChanged:                  headroomInt(30),
		},
	}

	meta, out, err := r.listScopeAmendments(context.Background(), nil, ListScopeAmendmentsInput{
		RunID: runID.String(),
	})
	if err != nil {
		t.Fatalf("listScopeAmendments: %v", err)
	}
	got := out.Items[0]
	if got.EffectiveScopeFilesAfterApproval == nil || *got.EffectiveScopeFilesAfterApproval != 31 {
		t.Errorf("EffectiveScopeFilesAfterApproval = %v, want 31", got.EffectiveScopeFilesAfterApproval)
	}
	if got.MaxFilesChanged == nil || *got.MaxFilesChanged != 30 {
		t.Errorf("MaxFilesChanged = %v, want 30", got.MaxFilesChanged)
	}
	if meta == nil || len(meta.Content) == 0 {
		t.Fatal("meta = nil, want an over-cap warning")
	}
	text := meta.Content[0].(*mcp.TextContent).Text
	for _, want := range []string{"WARNING", itemID, "31", "max_files_changed cap of 30"} {
		if !strings.Contains(text, want) {
			t.Errorf("warning missing %q: %s", want, text)
		}
	}
}

func TestListScopeAmendments_UnderCapOrFieldsAbsent_NoWarning(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	fb.amendmentsByRun[runID] = []ScopeAmendmentItem{
		{
			// Under cap: fields present, within bounds.
			ID: uuid.New().String(), RunID: runID.String(), Status: "pending",
			EffectiveScopeFilesAfterApproval: headroomInt(3),
			MaxFilesChanged:                  headroomInt(30),
		},
		{
			// Older-backend shape: fields absent entirely.
			ID: uuid.New().String(), RunID: runID.String(), Status: "pending",
		},
	}

	meta, out, err := r.listScopeAmendments(context.Background(), nil, ListScopeAmendmentsInput{
		RunID: runID.String(),
	})
	if err != nil {
		t.Fatalf("listScopeAmendments: %v", err)
	}
	if meta != nil {
		t.Errorf("meta = %+v, want nil (no over-cap items)", meta)
	}
	// Backward compat: the absent-fields item decodes with nils.
	if out.Items[1].EffectiveScopeFilesAfterApproval != nil || out.Items[1].MaxFilesChanged != nil {
		t.Errorf("absent fields must decode to nil; got %+v", out.Items[1])
	}
}

func TestDecideScopeAmendment_OverCapHeadroom_RendersWarning(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	amendmentID := uuid.New()
	fb.decideAmendmentResp[amendmentID] = ScopeAmendmentItem{
		ID:                               amendmentID.String(),
		RunID:                            runID.String(),
		Status:                           "approved",
		EffectiveScopeFilesAfterApproval: headroomInt(31),
		MaxFilesChanged:                  headroomInt(30),
	}

	meta, out, err := r.decideScopeAmendment(context.Background(), nil, DecideScopeAmendmentInput{
		RunID:       runID.String(),
		AmendmentID: amendmentID.String(),
		Decision:    "approve",
		Reason:      "forced",
	})
	if err != nil {
		t.Fatalf("decideScopeAmendment: %v", err)
	}
	if out.Amendment.Status != "approved" {
		t.Errorf("status = %q, want approved (over-cap approve is warn-only)", out.Amendment.Status)
	}
	if meta == nil || len(meta.Content) == 0 {
		t.Fatal("meta = nil, want an over-cap warning")
	}
	text := meta.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "max_files_changed cap of 30") {
		t.Errorf("warning text = %s", text)
	}
}

func TestDecideScopeAmendment_FieldsAbsent_NoWarning(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	amendmentID := uuid.New()
	fb.decideAmendmentResp[amendmentID] = ScopeAmendmentItem{
		ID:     amendmentID.String(),
		RunID:  runID.String(),
		Status: "denied",
	}

	meta, _, err := r.decideScopeAmendment(context.Background(), nil, DecideScopeAmendmentInput{
		RunID:       runID.String(),
		AmendmentID: amendmentID.String(),
		Decision:    "deny",
	})
	if err != nil {
		t.Fatalf("decideScopeAmendment: %v", err)
	}
	if meta != nil {
		t.Errorf("meta = %+v, want nil for an older-backend shape", meta)
	}
}
