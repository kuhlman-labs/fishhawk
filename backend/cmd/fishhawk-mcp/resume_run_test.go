package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// --- fishhawk_resume_run (#978) ---

func TestResumeRun_HappyPath_PostsBodyReturnsRun(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parentID := uuid.New()
	_, out, err := r.resumeRun(context.Background(), nil, ResumeRunInput{
		ParentRunID: parentID.String(),
		AddScopeFiles: []RecoverScopePath{
			{Path: "docs/extra.md"},
			{Path: "backend/new_file.go", Operation: "create"},
		},
		Reason: "fold the dropped doc companion",
	})
	if err != nil {
		t.Fatalf("resumeRun: %v", err)
	}
	if out.Run.ID == "" {
		t.Errorf("Run.ID empty; expected the fake to allocate one")
	}
	if out.Run.ParentRunID == nil || *out.Run.ParentRunID != parentID.String() {
		t.Errorf("Run.ParentRunID = %v, want %s", out.Run.ParentRunID, parentID)
	}
	if out.Idempotent {
		t.Errorf("Idempotent = true, want false (fresh create returns 201)")
	}
	if fb.recoverParentID != parentID {
		t.Errorf("backend got parent run id %s, want %s", fb.recoverParentID, parentID)
	}
	if len(fb.recoverBody.AddScopeFiles) != 2 ||
		fb.recoverBody.AddScopeFiles[0].Path != "docs/extra.md" ||
		fb.recoverBody.AddScopeFiles[1].Operation != "create" {
		t.Errorf("backend got AddScopeFiles = %+v", fb.recoverBody.AddScopeFiles)
	}
	if fb.recoverBody.Reason != "fold the dropped doc companion" {
		t.Errorf("backend got Reason = %q", fb.recoverBody.Reason)
	}
	if fb.recoverIdempKey != "" {
		t.Errorf("Idempotency-Key set without input: %q", fb.recoverIdempKey)
	}
}

// TestResumeRun_ExemptScopeFiles_RoundTrips pins the #1229 exempt_scope_files
// lever: the MCP input's ExemptScopeFiles reaches the backend recover request
// body byte-for-byte (path + reason), alongside add_scope_files.
func TestResumeRun_ExemptScopeFiles_RoundTrips(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parentID := uuid.New()
	_, _, err := r.resumeRun(context.Background(), nil, ResumeRunInput{
		ParentRunID: parentID.String(),
		ExemptScopeFiles: []RecoverExemptPath{
			{Path: "backend/internal/server/handlers.go", Reason: "no change needed on this slice"},
		},
		Reason: "recover with the declared file left unchanged",
	})
	if err != nil {
		t.Fatalf("resumeRun: %v", err)
	}
	if len(fb.recoverBody.ExemptScopeFiles) != 1 ||
		fb.recoverBody.ExemptScopeFiles[0].Path != "backend/internal/server/handlers.go" ||
		fb.recoverBody.ExemptScopeFiles[0].Reason != "no change needed on this slice" {
		t.Errorf("backend got ExemptScopeFiles = %+v", fb.recoverBody.ExemptScopeFiles)
	}
}

func TestResumeRun_InvalidUUID_FailsLocally(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.resumeRun(context.Background(), nil, ResumeRunInput{ParentRunID: "not-a-uuid"})
	if err == nil || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Fatalf("err = %v, want local UUID validation error", err)
	}
}

func TestResumeRun_IdempotencyKey_SetsHeaderAndFlagsReplay(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.recoverStatus = http.StatusOK // backend signals idempotent replay
	r := newResolver(srv, nil)

	_, out, err := r.resumeRun(context.Background(), nil, ResumeRunInput{
		ParentRunID:    uuid.NewString(),
		IdempotencyKey: "recover-once",
	})
	if err != nil {
		t.Fatalf("resumeRun: %v", err)
	}
	if fb.recoverIdempKey != "recover-once" {
		t.Errorf("Idempotency-Key header = %q, want recover-once", fb.recoverIdempKey)
	}
	if !out.Idempotent {
		t.Errorf("Idempotent = false, want true on a 200 replay")
	}
}

func TestResumeRun_NotEligible_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.recoverStatus = http.StatusConflict
	fb.recoverErrBody = `{"error":{"code":"recovery_not_eligible","message":"recovery requires a succeeded plan stage and an implement stage failed category-B","details":{"plan_state":"succeeded","implement_state":"failed","failure_category":"A"}}}`
	r := newResolver(srv, nil)

	_, _, err := r.resumeRun(context.Background(), nil, ResumeRunInput{ParentRunID: uuid.NewString()})
	if err == nil {
		t.Fatal("err = nil, want recovery_not_eligible mapping")
	}
	for _, want := range []string{"recovery_not_eligible", "failure_category=A", "fishhawk_retry_stage"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err.Error(), want)
		}
	}
}

// TestResumeRun_NotEligible_MentionsDecompositionChild pins the
// slice-2 messaging: the recovery_not_eligible mapping explains BOTH
// the top-level and the in-place decomposition-child eligibility legs,
// and surfaces the plan_resolved detail the child branch returns.
func TestResumeRun_NotEligible_MentionsDecompositionChild(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.recoverStatus = http.StatusConflict
	fb.recoverErrBody = `{"error":{"code":"recovery_not_eligible","message":"in-place recovery of a decomposition child requires the child's own implement stage failed category-B and an approved plan resolvable via the parent walk","details":{"implement_state":"failed","failure_category":"B","plan_resolved":false}}}`
	r := newResolver(srv, nil)

	_, _, err := r.resumeRun(context.Background(), nil, ResumeRunInput{ParentRunID: uuid.NewString()})
	if err == nil {
		t.Fatal("err = nil, want recovery_not_eligible mapping")
	}
	for _, want := range []string{"decomposition-child", "plan_resolved=false", "in-place"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err.Error(), want)
		}
	}
}

func TestResumeRun_Unsupported_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.recoverStatus = http.StatusUnprocessableEntity
	fb.recoverErrBody = `{"error":{"code":"recovery_unsupported","message":"parent run has no cached workflow spec; start a fresh run instead"}}`
	r := newResolver(srv, nil)

	_, _, err := r.resumeRun(context.Background(), nil, ResumeRunInput{ParentRunID: uuid.NewString()})
	if err == nil || !strings.Contains(err.Error(), "fishhawk_start_run") {
		t.Fatalf("err = %v, want recovery_unsupported mapping pointing at fishhawk_start_run", err)
	}
}

func TestResumeRun_NotFound_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.recoverStatus = http.StatusNotFound
	fb.recoverErrBody = `{"error":{"code":"run_not_found","message":"no run with that id"}}`
	r := newResolver(srv, nil)

	_, _, err := r.resumeRun(context.Background(), nil, ResumeRunInput{ParentRunID: uuid.NewString()})
	if err == nil || !strings.Contains(err.Error(), "fishhawk_list_runs") {
		t.Fatalf("err = %v, want run_not_found mapping pointing at fishhawk_list_runs", err)
	}
}
