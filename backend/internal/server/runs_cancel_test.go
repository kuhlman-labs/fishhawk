package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

func TestCancelRun_HappyPath(t *testing.T) {
	repo := newFakeRepo()
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	r := seedRun(repo, "x/y", "w", run.StatePending, t0)
	s := newServer(t, repo)

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v0/runs/%s/cancel", r.ID), nil)
	req.SetPathValue("run_id", r.ID.String())
	w := httptest.NewRecorder()
	s.handleCancelRun(w, withAuth(req))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var got runResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.State != string(run.StateCancelled) {
		t.Errorf("State = %q, want cancelled", got.State)
	}
}

func TestCancelRun_Idempotent(t *testing.T) {
	repo := newFakeRepo()
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	r := seedRun(repo, "x/y", "w", run.StateCancelled, t0)
	s := newServer(t, repo)

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v0/runs/%s/cancel", r.ID), nil)
	req.SetPathValue("run_id", r.ID.String())
	w := httptest.NewRecorder()
	s.handleCancelRun(w, withAuth(req))
	if w.Code != http.StatusOK {
		t.Errorf("idempotent cancel status = %d, want 200", w.Code)
	}
}

func TestCancelRun_TerminalStateConflict(t *testing.T) {
	repo := newFakeRepo()
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	r := seedRun(repo, "x/y", "w", run.StateSucceeded, t0)
	s := newServer(t, repo)

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v0/runs/%s/cancel", r.ID), nil)
	req.SetPathValue("run_id", r.ID.String())
	w := httptest.NewRecorder()
	s.handleCancelRun(w, withAuth(req))
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"invalid_state_transition"`) {
		t.Errorf("body missing invalid_state_transition: %s", w.Body.String())
	}
}

func TestCancelRun_NotFound(t *testing.T) {
	s := newServer(t, newFakeRepo())
	runID := uuid.New()
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v0/runs/%s/cancel", runID), nil)
	req.SetPathValue("run_id", runID.String())
	w := httptest.NewRecorder()
	s.handleCancelRun(w, withAuth(req))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestCancelRun_BadUUID(t *testing.T) {
	s := newServer(t, newFakeRepo())
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/not-a-uuid/cancel", nil)
	req.SetPathValue("run_id", "not-a-uuid")
	w := httptest.NewRecorder()
	s.handleCancelRun(w, withAuth(req))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestCancelRun_RepoError(t *testing.T) {
	repo := newFakeRepo()
	repo.transitionErr = errors.New("db down")
	s := newServer(t, repo)
	runID := uuid.New()
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v0/runs/%s/cancel", runID), nil)
	req.SetPathValue("run_id", runID.String())
	w := httptest.NewRecorder()
	s.handleCancelRun(w, withAuth(req))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestCancelRun_NilRepo(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	runID := uuid.New()
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v0/runs/%s/cancel", runID), nil)
	req.SetPathValue("run_id", runID.String())
	w := httptest.NewRecorder()
	s.handleCancelRun(w, withAuth(req))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}
