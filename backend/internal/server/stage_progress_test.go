package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// seedRunningStage creates a run + one stage and drives the stage to 'running'
// through the normal transitions, returning the ids. The stage is left
// mid-execution (non-terminal), the state the progress endpoint accepts.
func seedRunningStage(t *testing.T, repo run.Repository) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	r, err := repo.CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change",
		WorkflowSHA: "deadbeef", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	s, err := repo.CreateStage(ctx, run.CreateStageParams{
		RunID: r.ID, Sequence: 0, Type: run.StageTypeImplement,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}
	for _, to := range []run.StageState{run.StageStateDispatched, run.StageStateRunning} {
		if _, err := repo.TransitionStage(ctx, s.ID, to, nil); err != nil {
			t.Fatalf("transition to %s: %v", to, err)
		}
	}
	return r.ID, s.ID
}

func progressBody(t *testing.T, v any) *strings.Reader {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReader(string(raw))
}

// postProgressMux drives the request through the registered mux (auth
// middleware included) — an anonymous request against a run with no account is
// admitted at the memberWrite tier, so this is the wiring proof for the new
// route (approval condition 3).
func postProgressMux(t *testing.T, s *Server, runID, stageID uuid.UUID, body *strings.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/"+runID.String()+"/stages/"+stageID.String()+"/progress", body)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// postProgressDirect calls the handler with explicit path values, bypassing the
// mux/auth — used where the assertion is about the handler's own validation, not
// the route wiring.
func postProgressDirect(t *testing.T, s *Server, runID, stageID string, body *strings.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/"+runID+"/stages/"+stageID+"/progress", body)
	req.SetPathValue("run_id", runID)
	req.SetPathValue("stage_id", stageID)
	w := httptest.NewRecorder()
	s.handleReportStageProgress(w, req)
	return w
}

// TestReportStageProgress_EndToEndReachesStageRead (E1) drives a real
// pgtest-backed postgres repo through the REGISTERED MUX: POST the heartbeat,
// then GET /v0/stages/{id} and GET /v0/runs/{id}/stages and assert BOTH read
// surfaces return the identical last_event / turns_this_attempt /
// tokens_this_attempt.
func TestReportStageProgress_EndToEndReachesStageRead(t *testing.T) {
	repo := run.NewPostgresRepository(pgtest.NewPool(t))
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo})
	runID, stageID := seedRunningStage(t, repo)

	w := postProgressMux(t, s, runID, stageID, progressBody(t, stageProgressReport{
		LastEvent: "assistant", TurnsThisAttempt: 9, TokensThisAttempt: 13402,
	}))
	if w.Code != http.StatusNoContent {
		t.Fatalf("POST status = %d, want 204:\n%s", w.Code, w.Body.String())
	}

	// GET /v0/stages/{id}
	getReq := httptest.NewRequest(http.MethodGet, "/v0/stages/"+stageID.String(), nil)
	getW := httptest.NewRecorder()
	s.Handler().ServeHTTP(getW, getReq)
	if getW.Code != http.StatusOK {
		t.Fatalf("GET stage status = %d, want 200:\n%s", getW.Code, getW.Body.String())
	}
	var single stageResponse
	if err := json.Unmarshal(getW.Body.Bytes(), &single); err != nil {
		t.Fatal(err)
	}
	if single.Progress == nil {
		t.Fatal("GET /v0/stages/{id}: progress nil after a recorded heartbeat")
	}
	if single.Progress.LastEvent != "assistant" || single.Progress.TurnsThisAttempt != 9 || single.Progress.TokensThisAttempt != 13402 {
		t.Errorf("single-read progress = %+v", *single.Progress)
	}

	// GET /v0/runs/{id}/stages
	listReq := httptest.NewRequest(http.MethodGet, "/v0/runs/"+runID.String()+"/stages", nil)
	listW := httptest.NewRecorder()
	s.Handler().ServeHTTP(listW, listReq)
	if listW.Code != http.StatusOK {
		t.Fatalf("GET stages status = %d, want 200:\n%s", listW.Code, listW.Body.String())
	}
	var list struct {
		Items []stageResponse `json:"items"`
	}
	if err := json.Unmarshal(listW.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Progress == nil {
		t.Fatalf("list read: want 1 stage with progress, got %+v", list.Items)
	}
	if list.Items[0].Progress.LastEvent != "assistant" || list.Items[0].Progress.TurnsThisAttempt != 9 || list.Items[0].Progress.TokensThisAttempt != 13402 {
		t.Errorf("list-read progress = %+v", *list.Items[0].Progress)
	}
}

// TestReportStageProgress_CrossAccountForbidden closes the authorization gap the
// plan's risk section named ("Falsified by TestReportStageProgress_*
// authorization cases if wrong"). The mux-driven E1/siblings admit anonymous
// requests because their seeded run has no account, so the memberWrite tier's
// cross-run 403 was never exercised. Here a bearer token bound to account A
// POSTs a heartbeat to a run owned by account B and must be refused 403
// account_forbidden by the requireRunAccount(memberWrite, ...) middleware BEFORE
// the handler runs — the byte-identical wrapper the adjacent host-dispatch route
// carries. Driven through the full mux (Handler()) with a real Authorization
// header so the route REGISTRATION wiring is what enforces it.
func TestReportStageProgress_CrossAccountForbidden(t *testing.T) {
	fr, _, runB, _ := seedAuthzRuns()
	repo := &stubAPITokenRepo{tok: &apitoken.Token{
		ID:        uuid.New(),
		Subject:   "github:op",
		AccountID: authzAcctA,
		PlainText: "fhk_account_a_token",
	}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: fr, APITokenRepo: repo})

	// stage_id is any UUID: the middleware resolves the run via run_id and
	// refuses before the handler ever touches the stage.
	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/"+runB.ID.String()+"/stages/"+uuid.New().String()+"/progress",
		progressBody(t, stageProgressReport{LastEvent: "assistant", TurnsThisAttempt: 1}))
	req.Header.Set("Authorization", "Bearer "+repo.tok.PlainText)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "account_forbidden") {
		t.Errorf("body = %s, want account_forbidden", w.Body.String())
	}
}

// TestReportStageProgress_OversizedBodyRejected pins the http.MaxBytesReader
// body cap: a VALID-JSON but oversized payload (past progressMaxBodyBytes) is
// refused 400 at decode rather than being read wholesale into memory. Paired
// against valid JSON so that WITHOUT the cap the same body decodes to a 204 —
// the discriminating counterfactual for the size guard.
func TestReportStageProgress_OversizedBodyRejected(t *testing.T) {
	repo := run.NewPostgresRepository(pgtest.NewPool(t))
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo})
	runID, stageID := seedRunningStage(t, repo)

	// A single valid last_event string larger than the cap: the body is
	// well-formed JSON, so a 400 can only come from the byte cap, not a parse
	// error.
	huge := strings.Repeat("x", progressMaxBodyBytes+1024)
	w := postProgressMux(t, s, runID, stageID, progressBody(t, stageProgressReport{
		LastEvent: huge, TurnsThisAttempt: 1,
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body cap):\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "validation_failed") {
		t.Errorf("body = %s, want validation_failed", w.Body.String())
	}
}

// TestReportStageProgress_TerminalStageRefused (C1) pins the terminal-state
// predicate. The stage is driven terminal BY CONSTRUCTION, then a heartbeat is
// POSTed: it must 409 stage_terminal AND — because the control's effect is
// COMMITTED STATE — the progress column must still be nil on a re-read (a
// rolled-back write would return a byte-identical 409).
func TestReportStageProgress_TerminalStageRefused(t *testing.T) {
	ctx := context.Background()
	repo := run.NewPostgresRepository(pgtest.NewPool(t))
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo})
	runID, stageID := seedRunningStage(t, repo)
	if _, err := repo.TransitionStage(ctx, stageID, run.StageStateSucceeded, nil); err != nil {
		t.Fatalf("transition succeeded: %v", err)
	}

	w := postProgressMux(t, s, runID, stageID, progressBody(t, stageProgressReport{
		LastEvent: "assistant", TurnsThisAttempt: 1,
	}))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "stage_terminal") {
		t.Errorf("body = %s, want stage_terminal", w.Body.String())
	}
	// Committed state: the write was refused, not rolled back.
	store := repo.(run.StageProgressStore)
	got, err := store.StageProgressByID(ctx, stageID)
	if err != nil {
		t.Fatalf("StageProgressByID: %v", err)
	}
	if got != nil {
		t.Errorf("terminal stage progress = %+v, want nil (write refused)", *got)
	}
}

// TestReportStageProgress_LastEventClamped (C2) pins the last_event clamp on
// COMMITTED STATE: an oversized last_event is persisted truncated to
// progressLastEventMaxLen runes. Re-reads the row, not the response.
func TestReportStageProgress_LastEventClamped(t *testing.T) {
	ctx := context.Background()
	repo := run.NewPostgresRepository(pgtest.NewPool(t))
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo})
	runID, stageID := seedRunningStage(t, repo)

	oversized := strings.Repeat("x", progressLastEventMaxLen+50)
	w := postProgressMux(t, s, runID, stageID, progressBody(t, stageProgressReport{
		LastEvent: oversized, TurnsThisAttempt: 2,
	}))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204:\n%s", w.Code, w.Body.String())
	}
	store := repo.(run.StageProgressStore)
	got, err := store.StageProgressByID(ctx, stageID)
	if err != nil {
		t.Fatalf("StageProgressByID: %v", err)
	}
	if got == nil {
		t.Fatal("progress nil after a recorded heartbeat")
	}
	if n := len([]rune(got.LastEvent)); n != progressLastEventMaxLen {
		t.Errorf("persisted last_event length = %d, want %d (clamped)", n, progressLastEventMaxLen)
	}
}

// TestReportStageProgress_NegativeCountersRejected (C3) pins the non-negative
// guard: a negative counter is a 400 AND leaves the row untouched (committed
// state — the write never happened).
func TestReportStageProgress_NegativeCountersRejected(t *testing.T) {
	ctx := context.Background()
	repo := run.NewPostgresRepository(pgtest.NewPool(t))
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo})
	runID, stageID := seedRunningStage(t, repo)

	w := postProgressMux(t, s, runID, stageID, progressBody(t, stageProgressReport{
		LastEvent: "assistant", TurnsThisAttempt: -1, TokensThisAttempt: 5,
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "validation_failed") {
		t.Errorf("body = %s, want validation_failed", w.Body.String())
	}
	store := repo.(run.StageProgressStore)
	got, err := store.StageProgressByID(ctx, stageID)
	if err != nil {
		t.Fatalf("StageProgressByID: %v", err)
	}
	if got != nil {
		t.Errorf("row written despite a rejected negative counter: %+v", *got)
	}
}

// TestReportStageProgress_StoreUnsupportedReturns503 pins the store-assertion
// degrade: a RunRepo that satisfies run.Repository but NOT stageProgressStore
// answers 503 progress_unsupported rather than panicking.
func TestReportStageProgress_StoreUnsupportedReturns503(t *testing.T) {
	repo := &stageGetRepo{stagesRunRepo: newStagesRunRepo()}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo})
	w := postProgressDirect(t, s, uuid.New().String(), uuid.New().String(),
		progressBody(t, stageProgressReport{LastEvent: "assistant"}))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "progress_unsupported") {
		t.Errorf("body = %s, want progress_unsupported", w.Body.String())
	}
}

// TestReportStageProgress_StageRunMismatch404 pins the (run_id, stage_id) handle
// check: a stage that exists but belongs to a DIFFERENT run is 404 at this path.
func TestReportStageProgress_StageRunMismatch404(t *testing.T) {
	repo := run.NewPostgresRepository(pgtest.NewPool(t))
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo})
	_, stageID := seedRunningStage(t, repo)
	otherRun, _ := seedRunningStage(t, repo)

	w := postProgressDirect(t, s, otherRun.String(), stageID.String(),
		progressBody(t, stageProgressReport{LastEvent: "assistant"}))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "stage_not_found") {
		t.Errorf("body = %s, want stage_not_found", w.Body.String())
	}
}

// TestReportStageProgress_UnknownFieldRejected pins DisallowUnknownFields: an
// unexpected body field is a 400, so a runner sending a drifted schema fails
// loud rather than silently dropping it.
func TestReportStageProgress_UnknownFieldRejected(t *testing.T) {
	repo := run.NewPostgresRepository(pgtest.NewPool(t))
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo})
	runID, stageID := seedRunningStage(t, repo)

	w := postProgressMux(t, s, runID, stageID,
		strings.NewReader(`{"last_event":"assistant","turns_this_attempt":1,"elapsed_seconds":42}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "validation_failed") {
		t.Errorf("body = %s, want validation_failed", w.Body.String())
	}
}

// TestReportStageProgress_BadUUIDs pins the id validation branches.
func TestReportStageProgress_BadUUIDs(t *testing.T) {
	repo := run.NewPostgresRepository(pgtest.NewPool(t))
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo})
	w := postProgressDirect(t, s, "not-a-uuid", uuid.New().String(),
		progressBody(t, stageProgressReport{LastEvent: "assistant"}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
}

// TestReportStageProgress_NilRepoReturns503 pins the nil-RunRepo guard (distinct
// from the store-unsupported branch): no repo wired at all → 503.
func TestReportStageProgress_NilRepoReturns503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	w := postProgressDirect(t, s, uuid.New().String(), uuid.New().String(),
		progressBody(t, stageProgressReport{LastEvent: "assistant"}))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
}

// TestReportStageProgress_StoreErrorReturns500 pins the RecordStageProgress
// error branch: a store that errors (not a terminal zero-row) → 500.
func TestReportStageProgress_StoreErrorReturns500(t *testing.T) {
	stageID := uuid.New()
	runID := uuid.New()
	repo := &erroringProgressRepo{
		stageGetRepo: &stageGetRepo{stagesRunRepo: newStagesRunRepo()},
		stage:        &run.Stage{ID: stageID, RunID: runID, State: run.StageStateRunning},
	}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo})
	w := postProgressDirect(t, s, runID.String(), stageID.String(),
		progressBody(t, stageProgressReport{LastEvent: "assistant"}))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
}

// erroringProgressRepo satisfies both run.Repository (via stageGetRepo) AND
// stageProgressStore, returning a seeded stage from GetStage and an error from
// RecordStageProgress so the handler's 500 branch is reached.
type erroringProgressRepo struct {
	*stageGetRepo
	stage *run.Stage
}

func (r *erroringProgressRepo) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	if r.stage != nil && r.stage.ID == id {
		return r.stage, nil
	}
	return nil, run.ErrNotFound
}

func (r *erroringProgressRepo) RecordStageProgress(context.Context, uuid.UUID, run.StageProgress) (bool, error) {
	return false, errors.New("boom")
}

func (r *erroringProgressRepo) StageProgressByID(context.Context, uuid.UUID) (*run.StageProgress, error) {
	return nil, nil
}

func (r *erroringProgressRepo) StageProgressForRun(context.Context, uuid.UUID) (map[uuid.UUID]run.StageProgress, error) {
	return nil, nil
}
