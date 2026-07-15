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

// --- fishhawk_revive_run (#1915) ---

// reviveFakeBackend is a self-contained backend stub for the revive-run tool:
// it serves only POST /v0/runs/{run_id}/revive. reviveResp is the per-run
// success body; reviveStatus drives the HTTP status (default 200); reviveErrBody,
// when set, is written verbatim for the error-path tests. reviveCalledByID
// counts calls per run id so tests assert the tool did (or did not) reach the
// backend.
type reviveFakeBackend struct {
	mu               sync.Mutex
	reviveResp       map[uuid.UUID]ReviveRunResult
	reviveStatus     int
	reviveErrBody    string
	reviveCalledByID map[uuid.UUID]int
}

func newReviveFakeBackend(t *testing.T) (*reviveFakeBackend, *httptest.Server) {
	fb := &reviveFakeBackend{
		reviveResp:       map[uuid.UUID]ReviveRunResult{},
		reviveStatus:     http.StatusOK,
		reviveCalledByID: map[uuid.UUID]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/revive", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("run_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.reviveCalledByID[id]++
		status := fb.reviveStatus
		errBody := fb.reviveErrBody
		resp, ok := fb.reviveResp[id]
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if !ok {
			resp = ReviveRunResult{Run: Run{ID: id.String(), State: "running"}}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fb, srv
}

// TestReviveRun_HappyPath_SurfacesRunStagesAndHint asserts the tool -> client
// -> HTTP boundary: a successful revive returns the re-opened run (running),
// the per-stage re-park summary, and the constant no-dispatch next_step hint.
func TestReviveRun_HappyPath_SurfacesRunStagesAndHint(t *testing.T) {
	fb, srv := newReviveFakeBackend(t)
	runID := uuid.New()
	stageID := uuid.New()
	fb.reviveResp[runID] = ReviveRunResult{
		Run: Run{ID: runID.String(), State: "running"},
		RestoredStages: []ReviveRestoredStage{{
			StageID:       stageID.String(),
			Type:          "implement",
			PriorCategory: "A",
			PriorReason:   "agent failure",
			RestoredState: "pending",
		}},
	}
	r := newResolver(srv, nil)

	_, out, err := r.reviveRun(context.Background(), nil, ReviveRunInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("reviveRun: %v", err)
	}
	if out.Run.State != "running" {
		t.Errorf("Run.State = %q, want running", out.Run.State)
	}
	if len(out.RestoredStages) != 1 || out.RestoredStages[0].StageID != stageID.String() {
		t.Errorf("RestoredStages = %+v, want one restore for %s", out.RestoredStages, stageID)
	}
	if out.RestoredStages[0].RestoredState != "pending" {
		t.Errorf("restored_state = %q, want pending", out.RestoredStages[0].RestoredState)
	}
	if out.NextStep != reviveNextStepHint {
		t.Errorf("NextStep = %q, want the constant no-dispatch hint", out.NextStep)
	}
	// The hint must convey the load-bearing no-dispatch semantics.
	if !strings.Contains(strings.ToLower(out.NextStep), "without dispatching") {
		t.Errorf("NextStep does not convey the no-dispatch semantics: %q", out.NextStep)
	}
	if fb.reviveCalledByID[runID] != 1 {
		t.Errorf("revive called %d times, want 1", fb.reviveCalledByID[runID])
	}
}

// TestReviveRun_InvalidUUID_FailsLocally asserts the run_id UUID guard fails
// before any HTTP hop.
func TestReviveRun_InvalidUUID_FailsLocally(t *testing.T) {
	fb, srv := newReviveFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.reviveRun(context.Background(), nil, ReviveRunInput{RunID: "not-a-uuid"})
	if err == nil || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Fatalf("err = %v, want UUID parse error", err)
	}
	if len(fb.reviveCalledByID) != 0 {
		t.Errorf("backend revive called %d times, want 0", len(fb.reviveCalledByID))
	}
}

// TestReviveRun_NotApplicable_PropagatesAs422 asserts the backend's
// revive_not_applicable 422 surfaces as a tool error (the non-failed /
// non-retryable-stage refusal path).
func TestReviveRun_NotApplicable_PropagatesAs422(t *testing.T) {
	fb, srv := newReviveFakeBackend(t)
	fb.reviveStatus = http.StatusUnprocessableEntity
	fb.reviveErrBody = `{"error":{"code":"revive_not_applicable","message":"run is in state \"succeeded\" (only failed runs can be revived)"}}`
	r := newResolver(srv, nil)

	_, _, err := r.reviveRun(context.Background(), nil, ReviveRunInput{RunID: uuid.NewString()})
	if err == nil || !strings.Contains(err.Error(), "revive_not_applicable") {
		t.Fatalf("err = %v, want revive_not_applicable", err)
	}
}

// TestReviveRun_AgentTokenForbidden_PropagatesAs403 asserts the backend's
// operator-only guard (an agent/mcp token) surfaces as a tool error.
func TestReviveRun_AgentTokenForbidden_PropagatesAs403(t *testing.T) {
	fb, srv := newReviveFakeBackend(t)
	fb.reviveStatus = http.StatusForbidden
	fb.reviveErrBody = `{"error":{"code":"agent_token_forbidden","message":"revive is an operator action; agent (mcp) tokens may not revive any run"}}`
	r := newResolver(srv, nil)

	_, _, err := r.reviveRun(context.Background(), nil, ReviveRunInput{RunID: uuid.NewString()})
	if err == nil || !strings.Contains(err.Error(), "agent_token_forbidden") {
		t.Fatalf("err = %v, want agent_token_forbidden", err)
	}
}
