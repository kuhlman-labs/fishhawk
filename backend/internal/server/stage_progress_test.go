package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mcptoken"
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

// newProgressServer wires repo with a fake MCP token repository (so a
// run-bound fhm_ bearer authenticates through the real bearerAuth middleware,
// subject mcp:run:<uuid>, the production runner shape) and the shared
// write:runs / read:runs api-token stub (#4483).
func newProgressServer(repo run.Repository) *Server {
	return New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      repo,
		MCPTokenRepo: newFakeMCPTokenRepo(),
		APITokenRepo: writeScopeTokenRepo{},
	})
}

// runBoundBearer mints a run-bound fhm_ token for runID on s's MCP token repo,
// carrying only mcp:read as the runner's heartbeat bearer does (it never holds
// write:runs), and returns its plaintext.
func runBoundBearer(t *testing.T, s *Server, runID uuid.UUID) string {
	t.Helper()
	tok, err := s.cfg.MCPTokenRepo.Issue(context.Background(), mcptoken.IssueParams{
		RunID: runID, Scopes: []string{"mcp:read"},
	})
	if err != nil {
		t.Fatalf("issue run-bound token: %v", err)
	}
	return tok.PlainText
}

// postProgressMux drives the request through the registered mux (auth
// middleware included) carrying the run-bound bearer of runID itself — the
// runner's own heartbeat shape, admitted by authorizeStageProgress's own-run
// entitlement. This is the wiring proof for the route.
func postProgressMux(t *testing.T, s *Server, runID, stageID uuid.UUID, body *strings.Reader) *httptest.ResponseRecorder {
	t.Helper()
	return postProgressAs(t, s, runID, stageID, body, runBoundBearer(t, s, runID))
}

// postProgressAs drives the request through the registered mux with an
// explicit bearer; an empty bearer sends no Authorization header (anonymous).
func postProgressAs(t *testing.T, s *Server, runID, stageID uuid.UUID, body *strings.Reader, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/"+runID.String()+"/stages/"+stageID.String()+"/progress", body)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// postProgressDirect calls the handler with explicit path values, bypassing the
// mux/auth — used where the assertion is about the handler's own validation, not
// the route wiring. It injects the production runner identity for the PATH run
// (subject mcp:run:<runID>, a non-empty TokenID, mcp:read only), so the
// handler's own-run entitlement admits it exactly as it admits the runner.
func postProgressDirect(t *testing.T, s *Server, runID, stageID string, body *strings.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/"+runID+"/stages/"+stageID+"/progress", body)
	req.SetPathValue("run_id", runID)
	req.SetPathValue("stage_id", stageID)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
		Subject: "mcp:run:" + runID, TokenID: "tok", Scopes: []string{"mcp:read"},
	}))
	w := httptest.NewRecorder()
	s.handleReportStageProgress(w, req)
	return w
}

// TestReportStageProgress_EndToEndReachesStageRead (E1) drives a real
// pgtest-backed postgres repo through the REGISTERED MUX: POST the heartbeat
// carrying the run's OWN run-bound fhm_ bearer (the production runner shape,
// through the real bearerAuth middleware, #4483), then GET /v0/stages/{id} and GET /v0/runs/{id}/stages and assert BOTH read
// surfaces return the identical last_event / turns_this_attempt /
// tokens_this_attempt.
func TestReportStageProgress_EndToEndReachesStageRead(t *testing.T) {
	repo := run.NewPostgresRepository(pgtest.NewPool(t))
	s := newProgressServer(repo)
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

// assertProgressUnrecorded READS the stage's committed progress back and fails
// when a heartbeat landed: the refusal controls below are about committed
// state, so a status code alone would not prove the write never happened.
func assertProgressUnrecorded(t *testing.T, repo run.Repository, stageID uuid.UUID) {
	t.Helper()
	got, err := repo.(run.StageProgressStore).StageProgressByID(context.Background(), stageID)
	if err != nil {
		t.Fatalf("StageProgressByID: %v", err)
	}
	if got != nil {
		t.Errorf("stage progress = %+v, want nil (the refused heartbeat must not be recorded)", *got)
	}
}

// progressErrorCode decodes the error envelope, checks its code and returns
// its details.
func progressErrorCode(t *testing.T, w *httptest.ResponseRecorder, want string) map[string]any {
	t.Helper()
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (%s)", err, w.Body.String())
	}
	if env.Error.Code != want {
		t.Errorf("error code = %q, want %q (body %s)", env.Error.Code, want, w.Body.String())
	}
	return env.Error.Details
}

// TestReportStageProgress_ForeignRunBoundToken_Refused pins the own-run
// entitlement (#4483): a run-bound token minted for a DIFFERENT run (a second
// seeded run, non-matching by construction) posting a heartbeat to a running
// stage of this run is 403 run_not_entitled, and the stage carries no
// progress afterwards. With the own-run comparison deleted, the token is
// admitted, RecordStageProgress applies, and the test reads progress back.
func TestReportStageProgress_ForeignRunBoundToken_Refused(t *testing.T) {
	repo := run.NewPostgresRepository(pgtest.NewPool(t))
	s := newProgressServer(repo)
	runID, stageID := seedRunningStage(t, repo)
	otherRun, _ := seedRunningStage(t, repo)

	w := postProgressAs(t, s, runID, stageID, progressBody(t, stageProgressReport{
		LastEvent: "assistant", TurnsThisAttempt: 3,
	}), runBoundBearer(t, s, otherRun))
	// Committed state first, so a missing control reports BOTH the recorded
	// heartbeat and the wrong status.
	assertProgressUnrecorded(t, repo, stageID)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	details := progressErrorCode(t, w, "run_not_entitled")
	if details["run_id"] != runID.String() {
		t.Errorf("details.run_id = %v, want %s", details["run_id"], runID)
	}
}

// TestReportStageProgress_ReadOnlyToken_Refused pins the write:runs fallback
// for a non-run-bound bearer (#4483): a read:runs-only api token (TokenID set
// by bearerAuth, github: subject) is 403 insufficient_scope naming write:runs,
// and the stage carries no progress.
func TestReportStageProgress_ReadOnlyToken_Refused(t *testing.T) {
	repo := run.NewPostgresRepository(pgtest.NewPool(t))
	s := newProgressServer(repo)
	runID, stageID := seedRunningStage(t, repo)

	w := postProgressAs(t, s, runID, stageID, progressBody(t, stageProgressReport{
		LastEvent: "assistant", TurnsThisAttempt: 1,
	}), readRunsBearer)
	// Committed state first, so a missing control reports BOTH the recorded
	// heartbeat and the wrong status.
	assertProgressUnrecorded(t, repo, stageID)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if details := progressErrorCode(t, w, "insufficient_scope"); details["required_scope"] != "write:runs" {
		t.Errorf("details.required_scope = %v, want write:runs", details["required_scope"])
	}
}

// TestReportStageProgress_Anonymous_Unauthorized pins the anonymous refusal,
// which requireWriteScope itself answers (no separate anonymous arm exists to
// be masked): no Authorization header → 401, no progress recorded.
func TestReportStageProgress_Anonymous_Unauthorized(t *testing.T) {
	repo := run.NewPostgresRepository(pgtest.NewPool(t))
	s := newProgressServer(repo)
	runID, stageID := seedRunningStage(t, repo)

	w := postProgressAs(t, s, runID, stageID, progressBody(t, stageProgressReport{
		LastEvent: "assistant", TurnsThisAttempt: 1,
	}), "")
	// Committed state first, so a missing control reports BOTH the recorded
	// heartbeat and the wrong status.
	assertProgressUnrecorded(t, repo, stageID)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401:\n%s", w.Code, w.Body.String())
	}
	progressErrorCode(t, w, "authentication_required")
}

// TestReportStageProgress_WriteRunsOperator_Admitted pins the preservation
// arm: a non-run-bound operator bearer holding write:runs is admitted (204) and
// its heartbeat is recorded.
func TestReportStageProgress_WriteRunsOperator_Admitted(t *testing.T) {
	ctx := context.Background()
	repo := run.NewPostgresRepository(pgtest.NewPool(t))
	s := newProgressServer(repo)
	runID, stageID := seedRunningStage(t, repo)

	w := postProgressAs(t, s, runID, stageID, progressBody(t, stageProgressReport{
		LastEvent: "tool_use", TurnsThisAttempt: 5,
	}), writeRunsBearer)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204:\n%s", w.Code, w.Body.String())
	}
	got, err := repo.(run.StageProgressStore).StageProgressByID(ctx, stageID)
	if err != nil {
		t.Fatalf("StageProgressByID: %v", err)
	}
	if got == nil || got.LastEvent != "tool_use" || got.TurnsThisAttempt != 5 {
		t.Errorf("stage progress = %+v, want the operator's heartbeat recorded", got)
	}
}

// TestReportStageProgress_OversizedBodyRejected pins the http.MaxBytesReader
// body cap: a VALID-JSON but oversized payload (past progressMaxBodyBytes) is
// refused 400 at decode rather than being read wholesale into memory. Paired
// against valid JSON so that WITHOUT the cap the same body decodes to a 204 —
// the discriminating counterfactual for the size guard.
func TestReportStageProgress_OversizedBodyRejected(t *testing.T) {
	repo := run.NewPostgresRepository(pgtest.NewPool(t))
	s := newProgressServer(repo)
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
	s := newProgressServer(repo)
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
	s := newProgressServer(repo)
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
	s := newProgressServer(repo)
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
	s := newProgressServer(repo)
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
	s := newProgressServer(repo)
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
	s := newProgressServer(repo)
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

// TestReportStageProgress_TerminalRaceReportsObservedState pins the
// transition-between-read-and-write race: the stage is 'running' at the handler's
// handle-verification GetStage, then settles terminal before the atomic UPDATE,
// which refuses the write (applied=false). The 409 details must carry the state
// OBSERVED AFTER the refusal (succeeded), not the stale pre-update read
// (running). raceProgressRepo drives exactly that ordering — first GetStage
// returns running, RecordStageProgress refuses, second GetStage returns
// succeeded.
//
// Counterfactual: reverting the handler to report the pre-update stage.State
// makes the body say "running" and reddens the two state assertions below, while
// the terminal-refusal 409 status itself (byte-identical either way) does not
// discriminate — which is why this asserts on the reported STATE, not just the
// code.
func TestReportStageProgress_TerminalRaceReportsObservedState(t *testing.T) {
	stageID := uuid.New()
	runID := uuid.New()
	repo := &raceProgressRepo{
		stageGetRepo: &stageGetRepo{stagesRunRepo: newStagesRunRepo()},
		stageID:      stageID,
		runID:        runID,
	}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo})
	w := postProgressDirect(t, s, runID.String(), stageID.String(),
		progressBody(t, stageProgressReport{LastEvent: "assistant", TurnsThisAttempt: 1}))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "stage_terminal") {
		t.Errorf("body = %s, want stage_terminal", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"state":"succeeded"`) {
		t.Errorf("body = %s, want the state observed after the refusal (succeeded)", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"state":"running"`) {
		t.Errorf("body = %s, reports the STALE pre-update running state", w.Body.String())
	}
	// The handler read the handle once (verification) and once again after the
	// refusal — proving the reported state came from a post-refusal re-read.
	if repo.getCalls < 2 {
		t.Errorf("GetStage called %d times, want >= 2 (handle verify + post-refusal re-read)", repo.getCalls)
	}
}

// raceProgressRepo satisfies run.Repository (via stageGetRepo) AND
// stageProgressStore. Its GetStage returns 'running' on the first call (the
// handler's handle verification) and 'succeeded' on every later call (the stage
// settled before the write), while RecordStageProgress always refuses
// (applied=false), modelling a stage that goes terminal between the read and the
// write.
type raceProgressRepo struct {
	*stageGetRepo
	stageID  uuid.UUID
	runID    uuid.UUID
	getCalls int
}

func (r *raceProgressRepo) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	if id != r.stageID {
		return nil, run.ErrNotFound
	}
	r.getCalls++
	state := run.StageStateSucceeded
	if r.getCalls == 1 {
		state = run.StageStateRunning
	}
	return &run.Stage{ID: r.stageID, RunID: r.runID, State: state}, nil
}

func (r *raceProgressRepo) RecordStageProgress(context.Context, uuid.UUID, run.StageProgress) (bool, error) {
	return false, nil
}

func (r *raceProgressRepo) StageProgressByID(context.Context, uuid.UUID) (*run.StageProgress, error) {
	return nil, nil
}

func (r *raceProgressRepo) StageProgressForRun(context.Context, uuid.UUID) (map[uuid.UUID]run.StageProgress, error) {
	return nil, nil
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

// TestReportStageProgress_HandlerContributesNoProcessTimestamp closes the
// HANDLER leg of the #3084 seam. The stamp now comes from the DATABASE
// (RecordStageProgress's jsonb_set(now())), so re-adding a time.Now() to the
// handler's run.StageProgress literal would be behaviourally INVISIBLE — the SQL
// overwrites it, and no end-to-end assertion could go red. This capturing fake
// records what the handler actually passed and asserts ReportedAt is the ZERO
// value, making the handler's non-contribution a STRUCTURAL pin.
func TestReportStageProgress_HandlerContributesNoProcessTimestamp(t *testing.T) {
	stageID := uuid.New()
	runID := uuid.New()
	repo := &capturingProgressRepo{
		stageGetRepo: &stageGetRepo{stagesRunRepo: newStagesRunRepo()},
		stage:        &run.Stage{ID: stageID, RunID: runID, State: run.StageStateRunning},
	}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo})
	w := postProgressDirect(t, s, runID.String(), stageID.String(),
		progressBody(t, stageProgressReport{LastEvent: "assistant", TurnsThisAttempt: 4, TokensThisAttempt: 99}))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204:\n%s", w.Code, w.Body.String())
	}
	if repo.calls != 1 {
		t.Fatalf("RecordStageProgress called %d times, want 1", repo.calls)
	}
	if !repo.captured.ReportedAt.IsZero() {
		t.Errorf("handler passed ReportedAt = %v, want the ZERO value — the process clock must contribute nothing; the database stamps reported_at (#3084)", repo.captured.ReportedAt)
	}
	// The fields the handler IS responsible for still arrive, so a zero
	// ReportedAt cannot be an artifact of the handler passing an empty struct.
	if repo.captured.LastEvent != "assistant" || repo.captured.TurnsThisAttempt != 4 || repo.captured.TokensThisAttempt != 99 {
		t.Errorf("captured progress = %+v, want the decoded body fields", repo.captured)
	}
}

// capturingProgressRepo satisfies run.Repository (via stageGetRepo) AND
// stageProgressStore, recording the run.StageProgress the handler passed so a
// test can assert on the handler's OWN contribution rather than on persisted
// state the SQL would have overwritten.
type capturingProgressRepo struct {
	*stageGetRepo
	stage    *run.Stage
	captured run.StageProgress
	calls    int
}

func (r *capturingProgressRepo) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	if r.stage != nil && r.stage.ID == id {
		return r.stage, nil
	}
	return nil, run.ErrNotFound
}

func (r *capturingProgressRepo) RecordStageProgress(_ context.Context, _ uuid.UUID, p run.StageProgress) (bool, error) {
	r.calls++
	r.captured = p
	return true, nil
}

func (r *capturingProgressRepo) StageProgressByID(context.Context, uuid.UUID) (*run.StageProgress, error) {
	return nil, nil
}

func (r *capturingProgressRepo) StageProgressForRun(context.Context, uuid.UUID) (map[uuid.UUID]run.StageProgress, error) {
	return nil, nil
}

// stageProgressPhaseGoldenBytes reads the SHARED cross-module golden
// testdata/wire/stage_progress_phase.json (#4023) — the SAME bytes the runner's
// upload test asserts its marshalled body JSON-equal to.
func stageProgressPhaseGoldenBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "wire", "stage_progress_phase.json"))
	if err != nil {
		t.Fatalf("read shared stage-progress phase golden: %v", err)
	}
	return bytes.TrimSpace(b)
}

// TestReportStageProgress_GatePhaseGoldenPersisted POSTs the runner-shaped
// golden bytes through the REGISTERED mux (real DisallowUnknownFields decoder,
// real validation, real pgtest store) and reads every phase field back from
// the store — the backend half of the cross-module golden (#4023).
func TestReportStageProgress_GatePhaseGoldenPersisted(t *testing.T) {
	ctx := context.Background()
	repo := run.NewPostgresRepository(pgtest.NewPool(t))
	s := newProgressServer(repo)
	runID, stageID := seedRunningStage(t, repo)

	w := postProgressMux(t, s, runID, stageID, strings.NewReader(string(stageProgressPhaseGoldenBytes(t))))
	if w.Code != http.StatusNoContent {
		t.Fatalf("POST golden status = %d, want 204:\n%s", w.Code, w.Body.String())
	}
	got, err := repo.(run.StageProgressStore).StageProgressByID(ctx, stageID)
	if err != nil || got == nil {
		t.Fatalf("StageProgressByID = %+v, %v; want the recorded golden", got, err)
	}
	if got.LastEvent != "gate:verify" || got.TurnsThisAttempt != 9 || got.TokensThisAttempt != 13402 {
		t.Errorf("legacy fields = %+v", *got)
	}
	if got.Phase != "gate" || !got.GateVerifying {
		t.Errorf("phase = %q gate_verifying = %v, want gate/true", got.Phase, got.GateVerifying)
	}
	for name, pair := range map[string]struct {
		got  *int
		want int
	}{
		"phase_elapsed_seconds":  {got.PhaseElapsedSeconds, 312},
		"gate_budget_seconds":    {got.GateBudgetSeconds, 5400},
		"gate_remaining_seconds": {got.GateRemainingSeconds, 4980},
	} {
		if pair.got == nil || *pair.got != pair.want {
			t.Errorf("%s = %v, want %d", name, pair.got, pair.want)
		}
	}
}

// assertPhaseFieldRejected seeds a VALID heartbeat, POSTs body (which differs
// from the seed in last_event, so an accepted write is observable), and asserts
// both the 400 validation_failed naming field AND the committed state: the
// stored progress still reads back as the seed.
func assertPhaseFieldRejected(t *testing.T, field string, body string) {
	t.Helper()
	ctx := context.Background()
	repo := run.NewPostgresRepository(pgtest.NewPool(t))
	s := newProgressServer(repo)
	runID, stageID := seedRunningStage(t, repo)

	seed := postProgressMux(t, s, runID, stageID, progressBody(t, stageProgressReport{
		LastEvent: "seed", TurnsThisAttempt: 1, TokensThisAttempt: 2,
	}))
	if seed.Code != http.StatusNoContent {
		t.Fatalf("seed POST status = %d, want 204:\n%s", seed.Code, seed.Body.String())
	}

	w := postProgressMux(t, s, runID, stageID, strings.NewReader(body))
	// Status and committed state are asserted INDEPENDENTLY (Errorf, not
	// Fatalf), so a deleted control reports both.
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}

	got, err := repo.(run.StageProgressStore).StageProgressByID(ctx, stageID)
	if err != nil || got == nil {
		t.Fatalf("StageProgressByID = %+v, %v; want the seeded heartbeat", got, err)
	}
	if got.LastEvent != "seed" || got.TurnsThisAttempt != 1 || got.TokensThisAttempt != 2 ||
		got.Phase != "" || got.PhaseElapsedSeconds != nil || got.GateBudgetSeconds != nil ||
		got.GateRemainingSeconds != nil || got.GateVerifying {
		t.Errorf("stored progress changed despite a rejected %s: %+v", field, *got)
	}

	if w.Code == http.StatusBadRequest {
		details := progressErrorCode(t, w, "validation_failed")
		if details["field"] != field {
			t.Errorf("details.field = %v, want %q (body %s)", details["field"], field, w.Body.String())
		}
	}
}

// TestReportStageProgress_InvalidPhaseRejected pins the phase enum (#4023): a
// phase other than "", "agent" or "gate" is 400 naming phase, and nothing is
// written.
func TestReportStageProgress_InvalidPhaseRejected(t *testing.T) {
	assertPhaseFieldRejected(t, "phase",
		`{"last_event":"bad","turns_this_attempt":3,"tokens_this_attempt":4,"phase":"verify"}`)
}

// TestReportStageProgress_NegativePhaseElapsedRejected pins the non-negative
// phase_elapsed_seconds check.
func TestReportStageProgress_NegativePhaseElapsedRejected(t *testing.T) {
	assertPhaseFieldRejected(t, "phase_elapsed_seconds",
		`{"last_event":"bad","turns_this_attempt":3,"tokens_this_attempt":4,"phase":"agent","phase_elapsed_seconds":-1}`)
}

// TestReportStageProgress_NegativeGateBudgetRejected pins the non-negative
// gate_budget_seconds check.
func TestReportStageProgress_NegativeGateBudgetRejected(t *testing.T) {
	assertPhaseFieldRejected(t, "gate_budget_seconds",
		`{"last_event":"bad","turns_this_attempt":3,"tokens_this_attempt":4,"phase":"gate","gate_budget_seconds":-5}`)
}

// TestReportStageProgress_NegativeGateRemainingRejected pins the non-negative
// gate_remaining_seconds check.
func TestReportStageProgress_NegativeGateRemainingRejected(t *testing.T) {
	assertPhaseFieldRejected(t, "gate_remaining_seconds",
		`{"last_event":"bad","turns_this_attempt":3,"tokens_this_attempt":4,"phase":"gate","gate_budget_seconds":5400,"gate_remaining_seconds":-1}`)
}

// TestReportStageProgress_AgentPhaseAccepted pins the "agent" arm of the
// phase enum and the zero-valued pointer: phase_elapsed_seconds 0 is a valid,
// PRESENT value (not absent) and round-trips as such.
func TestReportStageProgress_AgentPhaseAccepted(t *testing.T) {
	ctx := context.Background()
	repo := run.NewPostgresRepository(pgtest.NewPool(t))
	s := newProgressServer(repo)
	runID, stageID := seedRunningStage(t, repo)

	w := postProgressMux(t, s, runID, stageID, strings.NewReader(
		`{"last_event":"agent_start","turns_this_attempt":0,"tokens_this_attempt":0,"phase":"agent","phase_elapsed_seconds":0}`))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204:\n%s", w.Code, w.Body.String())
	}
	got, err := repo.(run.StageProgressStore).StageProgressByID(ctx, stageID)
	if err != nil || got == nil {
		t.Fatalf("StageProgressByID = %+v, %v", got, err)
	}
	if got.Phase != "agent" || got.PhaseElapsedSeconds == nil || *got.PhaseElapsedSeconds != 0 {
		t.Errorf("progress = %+v, want phase agent with phase_elapsed_seconds 0 present", *got)
	}
	if got.GateBudgetSeconds != nil || got.GateRemainingSeconds != nil || got.GateVerifying {
		t.Errorf("unsent gate fields materialised: %+v", *got)
	}
}
