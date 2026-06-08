package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// --- fishhawk_reset_run_branch (ADR-035 remediation, #867) ---

// resetFakeBackend is a self-contained backend stub for the reset-branch
// tool: it serves only POST /v0/runs/{run_id}/reset-branch so this file
// owns its own fixtures (the shared fakeBackend lives in another test file).
//
// resetBody captures the last decoded request body so tests can assert the
// confirm flag + reason threading. resetResp seeds the rewind summary keyed
// by run id. resetStatus drives the HTTP status (default 200). resetErrBody,
// when set, is written verbatim — drives the error-path tests (403 / 422).
// resetCalledByID counts reset calls per run id.
type resetFakeBackend struct {
	mu              sync.Mutex
	resetBody       resetBranchRequest
	resetResp       map[uuid.UUID]ResetBranchResult
	resetStatus     int
	resetErrBody    string
	resetCalledByID map[uuid.UUID]int
}

func newResetFakeBackend(t *testing.T) (*resetFakeBackend, *httptest.Server) {
	fb := &resetFakeBackend{
		resetResp:       map[uuid.UUID]ResetBranchResult{},
		resetStatus:     http.StatusOK,
		resetCalledByID: map[uuid.UUID]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/reset-branch", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("run_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body resetBranchRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.resetCalledByID[id]++
		fb.resetBody = body
		status := fb.resetStatus
		errBody := fb.resetErrBody
		resp, ok := fb.resetResp[id]
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if !ok {
			resp = ResetBranchResult{RunID: id.String()}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fb, srv
}

func TestResetRunBranch_HappyPath_ThreadsConfirmAndReason(t *testing.T) {
	fb, srv := newResetFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	fb.resetResp[runID] = ResetBranchResult{
		RunID:               runID.String(),
		PRNumber:            42,
		Branch:              "fishhawk/run/x",
		DroppedOffendingSHA: "ffff",
		ResetToSHA:          "aaaa",
		PriorHeadSHA:        "ffff",
		RecoveryNote:        "recoverable from reflog",
	}

	_, out, err := r.resetRunBranch(context.Background(), nil, ResetRunBranchInput{
		RunID:   runID.String(),
		Reason:  "drop the foreign on-top commit",
		Confirm: true,
	})
	if err != nil {
		t.Fatalf("resetRunBranch: %v", err)
	}
	if out.Result.ResetToSHA != "aaaa" {
		t.Errorf("ResetToSHA = %q, want aaaa", out.Result.ResetToSHA)
	}
	if out.Result.DroppedOffendingSHA != "ffff" {
		t.Errorf("DroppedOffendingSHA = %q, want ffff", out.Result.DroppedOffendingSHA)
	}
	if fb.resetCalledByID[runID] != 1 {
		t.Errorf("reset called %d times, want 1", fb.resetCalledByID[runID])
	}
	// Confirm must reach the backend body true; reason verbatim.
	if !fb.resetBody.Confirm {
		t.Error("backend body confirm = false, want true")
	}
	if fb.resetBody.Reason != "drop the foreign on-top commit" {
		t.Errorf("body reason = %q, want the threaded reason", fb.resetBody.Reason)
	}
}

func TestResetRunBranch_InvalidUUID_FailsLocally(t *testing.T) {
	fb, srv := newResetFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.resetRunBranch(context.Background(), nil, ResetRunBranchInput{
		RunID:   "not-a-uuid",
		Confirm: true,
	})
	if err == nil || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Fatalf("err = %v, want UUID parse error", err)
	}
	if len(fb.resetCalledByID) != 0 {
		t.Errorf("backend reset called %d times, want 0", len(fb.resetCalledByID))
	}
}

func TestResetRunBranch_MissingConfirm_FailsLocally(t *testing.T) {
	// The destructive verb short-circuits before the HTTP hop when confirm
	// is not set, so the backend's 400 is never needed.
	fb, srv := newResetFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.resetRunBranch(context.Background(), nil, ResetRunBranchInput{
		RunID:   uuid.NewString(),
		Confirm: false,
	})
	if err == nil || !strings.Contains(err.Error(), "confirm must be true") {
		t.Fatalf("err = %v, want confirm-required error", err)
	}
	if len(fb.resetCalledByID) != 0 {
		t.Errorf("backend reset called %d times, want 0", len(fb.resetCalledByID))
	}
}

func TestResetRunBranch_OutOfScope_PropagatesAs422(t *testing.T) {
	fb, srv := newResetFakeBackend(t)
	fb.resetStatus = http.StatusUnprocessableEntity
	fb.resetErrBody = `{"error":{"code":"reset_out_of_scope","message":"the foreign commit is an ancestor, not on top"}}`
	r := newResolver(srv, nil)

	_, _, err := r.resetRunBranch(context.Background(), nil, ResetRunBranchInput{
		RunID:   uuid.NewString(),
		Confirm: true,
	})
	if err == nil || !strings.Contains(err.Error(), "reset_out_of_scope") {
		t.Fatalf("err = %v, want reset_out_of_scope", err)
	}
}

func TestResetRunBranch_NotApplicable_PropagatesAs422(t *testing.T) {
	fb, srv := newResetFakeBackend(t)
	fb.resetStatus = http.StatusUnprocessableEntity
	fb.resetErrBody = `{"error":{"code":"reset_not_applicable","message":"nothing on top to drop"}}`
	r := newResolver(srv, nil)

	_, _, err := r.resetRunBranch(context.Background(), nil, ResetRunBranchInput{
		RunID:   uuid.NewString(),
		Confirm: true,
	})
	if err == nil || !strings.Contains(err.Error(), "reset_not_applicable") {
		t.Fatalf("err = %v, want reset_not_applicable", err)
	}
}

func TestResetRunBranch_NotDeterminable_PropagatesAs422(t *testing.T) {
	fb, srv := newResetFakeBackend(t)
	fb.resetStatus = http.StatusUnprocessableEntity
	fb.resetErrBody = `{"error":{"code":"reset_not_determinable","message":"cannot classify with certainty"}}`
	r := newResolver(srv, nil)

	_, _, err := r.resetRunBranch(context.Background(), nil, ResetRunBranchInput{
		RunID:   uuid.NewString(),
		Confirm: true,
	})
	if err == nil || !strings.Contains(err.Error(), "reset_not_determinable") {
		t.Fatalf("err = %v, want reset_not_determinable", err)
	}
}

func TestResetRunBranch_CrossRun_PropagatesAs403(t *testing.T) {
	fb, srv := newResetFakeBackend(t)
	fb.resetStatus = http.StatusForbidden
	fb.resetErrBody = `{"error":{"code":"cross_run_reset","message":"mcp token may only reset its own run's branch"}}`
	r := newResolver(srv, nil)

	_, _, err := r.resetRunBranch(context.Background(), nil, ResetRunBranchInput{
		RunID:   uuid.NewString(),
		Confirm: true,
	})
	if err == nil || !strings.Contains(err.Error(), "cross_run_reset") {
		t.Fatalf("err = %v, want cross_run_reset", err)
	}
}
