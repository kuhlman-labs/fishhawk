package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

func TestGetRun_HappyPath(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	// Pre-seed a run by calling the repo directly.
	got, _ := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "x/y", WorkflowID: "w", WorkflowSHA: "s",
		TriggerSource: run.TriggerCLI,
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/runs/%s", got.ID), nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp runResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ID != got.ID {
		t.Errorf("ID = %s, want %s", resp.ID, got.ID)
	}
}

// TestGetRun_EchoesDrive asserts GET /v0/runs/{id} echoes the run's
// persisted drive flag (#1023) and that the field is always present
// on the wire (false for legacy / non-drive rows).
func TestGetRun_EchoesDrive(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	seeded, _ := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "x/y", WorkflowID: "w", WorkflowSHA: "s",
		TriggerSource: run.TriggerCLI,
	})
	// The fake returns the stored pointer; stamp the flag directly to
	// model a row persisted with drive=true.
	seeded.Drive = true

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/runs/%s", seeded.ID), nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp runResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.Drive {
		t.Errorf("Drive = false, want true")
	}

	// A non-drive run still carries the field explicitly (no omitempty).
	plain, _ := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "x/y", WorkflowID: "w", WorkflowSHA: "s",
		TriggerSource: run.TriggerCLI,
	})
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/runs/%s", plain.ID), nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"drive":false`) {
		t.Errorf("body should carry an explicit drive:false: %s", w.Body.String())
	}
}

func TestGetRun_NotFound(t *testing.T) {
	s := newServer(t, newFakeRepo())
	id := uuid.New()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/runs/%s", id), nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"run_not_found"`) {
		t.Errorf("body missing run_not_found code: %s", w.Body.String())
	}
}

func TestGetRun_BadUUID(t *testing.T) {
	s := newServer(t, newFakeRepo())
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/not-a-uuid", nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGetRun_RepoError(t *testing.T) {
	repo := newFakeRepo()
	repo.getErr = errors.New("connection lost")
	s := newServer(t, repo)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/runs/%s", uuid.New()), nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestGetRun_NilRepoConfigured(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/runs/%s", uuid.New()), nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestRoundTrip_CreateThenGet(t *testing.T) {
	s := newServer(t, newFakeRepo())

	createBody := `{"repo":"x/y","workflow_id":"w","workflow_sha":"abc","trigger_source":"ui"}`
	wCreate := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	s.handleCreateRun(wCreate, withAuth(createReq))
	if wCreate.Code != http.StatusCreated {
		t.Fatalf("create status = %d:\n%s", wCreate.Code, wCreate.Body.String())
	}
	var created runResponse
	if err := json.Unmarshal(wCreate.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	w2, body2 := requestPath(t, s, http.MethodGet, "/v0/runs/"+created.ID.String(), nil)
	if w2.Code != http.StatusOK {
		t.Fatalf("get status = %d:\n%s", w2.Code, body2)
	}
	var fetched runResponse
	if err := json.Unmarshal(body2, &fetched); err != nil {
		t.Fatal(err)
	}
	if fetched.ID != created.ID {
		t.Errorf("ID round-trip mismatch: %s vs %s", fetched.ID, created.ID)
	}
}

// requestPath is a tiny helper for round-tripping a raw body through
// the server and asserting status + decoded JSON.
func requestPath(t *testing.T, s *Server, method, path string, body any) (*httptest.ResponseRecorder, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	s.Handler().ServeHTTP(w, req)
	return w, w.Body.Bytes()
}

// TestGetRun_SurfacesOpenConcerns (#964): the single-run read attaches
// the open-concern summary — count, by_state breakdown, and the stable
// IDs fixup's concern_ids addressing needs — listing OPEN concerns only.
func TestGetRun_SurfacesOpenConcerns(t *testing.T) {
	repo := newFakeRepo()
	cr := newFakeConcernRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, ConcernRepo: cr})

	got, _ := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "x/y", WorkflowID: "w", WorkflowSHA: "s",
		TriggerSource: run.TriggerCLI,
	})
	implStageID := uuid.New()
	open1 := seedConcernRow(t, cr, got.ID, implStageID, "implement", 10, "open concern A")
	open2 := seedConcernRow(t, cr, got.ID, uuid.New(), "plan", 5, "open plan concern")
	resolved := seedConcernRow(t, cr, got.ID, implStageID, "implement", 11, "already resolved")
	if err := cr.MarkAddressedPending(context.Background(), []uuid.UUID{resolved.ID}, "routed"); err != nil {
		t.Fatalf("MarkAddressedPending: %v", err)
	}
	if _, err := cr.ApplyResolution(context.Background(), resolved.ID, concern.StateAddressed, "confirmed"); err != nil {
		t.Fatalf("ApplyResolution: %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/runs/%s", got.ID), nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Concerns == nil {
		t.Fatalf("concerns block missing:\n%s", w.Body.String())
	}
	if resp.Concerns.Open != 2 {
		t.Errorf("concerns.open = %d, want 2 (resolved concern excluded)", resp.Concerns.Open)
	}
	if resp.Concerns.ByState["raised"] != 2 {
		t.Errorf("by_state[raised] = %d, want 2", resp.Concerns.ByState["raised"])
	}
	ids := map[uuid.UUID]string{}
	for _, item := range resp.Concerns.Items {
		ids[item.ID] = item.StageKind
	}
	if ids[open1.ID] != "implement" || ids[open2.ID] != "plan" {
		t.Errorf("items = %+v, want stable IDs for both open concerns with their stage kinds", resp.Concerns.Items)
	}
	if _, present := ids[resolved.ID]; present {
		t.Error("resolved (addressed) concern must not be listed")
	}
}

// TestGetRun_NoConcernsOmitsBlock: a run with nothing open carries no
// concerns key at all (omitempty), and a nil ConcernRepo behaves the same.
func TestGetRun_NoConcernsOmitsBlock(t *testing.T) {
	repo := newFakeRepo()
	cr := newFakeConcernRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, ConcernRepo: cr})
	got, _ := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "x/y", WorkflowID: "w", WorkflowSHA: "s",
		TriggerSource: run.TriggerCLI,
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/runs/%s", got.ID), nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var raw map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	if _, present := raw["concerns"]; present {
		t.Errorf("concerns key present on a run with no open concerns:\n%s", w.Body.String())
	}
}

// TestGetRun_ConcernListFailureOmitsBlock: a concern-store failure is
// best-effort — the run read succeeds with the block omitted, never 500s.
func TestGetRun_ConcernListFailureOmitsBlock(t *testing.T) {
	repo := newFakeRepo()
	cr := newFakeConcernRepo()
	cr.listErr = errors.New("store down")
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, ConcernRepo: cr})
	got, _ := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "x/y", WorkflowID: "w", WorkflowSHA: "s",
		TriggerSource: run.TriggerCLI,
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/runs/%s", got.ID), nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (best-effort):\n%s", w.Code, w.Body.String())
	}
	var raw map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	if _, present := raw["concerns"]; present {
		t.Error("concerns key present despite the store failure")
	}
}

// TestListRuns_OmitsConcernsBlock pins the binding clarification: the
// list endpoint never gains a per-row concern query — even when a run
// HAS open concerns, the list items carry no concerns key (read the
// single-run endpoint for the block).
func TestListRuns_OmitsConcernsBlock(t *testing.T) {
	repo := newFakeRepo()
	cr := newFakeConcernRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, ConcernRepo: cr})
	got, _ := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "x/y", WorkflowID: "w", WorkflowSHA: "s",
		TriggerSource: run.TriggerCLI,
	})
	seedConcernRow(t, cr, got.ID, uuid.New(), "implement", 10, "open concern")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs", nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(resp.Items))
	}
	if _, present := resp.Items[0]["concerns"]; present {
		t.Error("list item carries a concerns key — the list path must stay free of the per-row concern query")
	}
}

// --- Drive read surfaces (#1023) ------------------------------------------

// seedAutoAdvance appends one run_auto_advanced entry to the audit
// fake's seeded history with the given sequence + timestamp.
func seedAutoAdvance(t *testing.T, au *auditFake, runID uuid.UUID, seq int64, ts time.Time, adv drive.Advance) {
	t.Helper()
	payload, err := json.Marshal(adv)
	if err != nil {
		t.Fatalf("marshal advance: %v", err)
	}
	rid := runID
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &rid, Sequence: seq, Category: drive.Category,
		Payload: payload, Timestamp: ts,
	})
}

// newDriveGetServer wires a run repo + audit fake server and seeds one
// drive-enabled running run, returning the seeded row for mutation.
func newDriveGetServer(t *testing.T) (*Server, *fakeRepo, *auditFake, *run.Run) {
	t.Helper()
	repo := newFakeRepo()
	au := newAuditFake()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au})
	seeded, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "x/y", WorkflowID: "w", WorkflowSHA: "s",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// The fake returns the stored pointer; stamp drive + running
	// directly to model the persisted row mid-run.
	seeded.Drive = true
	seeded.State = run.StateRunning
	return s, repo, au, seeded
}

func getRunResponse(t *testing.T, s *Server, runID uuid.UUID) (runResponse, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/runs/%s", runID), nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var raw map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	return resp, raw
}

// TestGetRun_Drive_SurfacesAutoAdvancedAndNextAction is the read-side
// happy path (#1023): the auto_advanced list distills every
// run_auto_advanced entry oldest-first, next_action comes from the most
// recent entry, and the derived awaiting_merge presentation status
// appears when the latest rule is checks_green_awaiting_merge on a
// non-terminal run with an open PR.
func TestGetRun_Drive_SurfacesAutoAdvancedAndNextAction(t *testing.T) {
	s, _, au, seeded := newDriveGetServer(t)
	pr := "https://github.com/x/y/pull/7"
	seeded.PullRequestURL = &pr

	t0 := time.Now().UTC().Add(-10 * time.Minute)
	seedAutoAdvance(t, au, seeded.ID, 5, t0, drive.Advance{
		Rule: drive.RulePlanApprovedDispatch, From: "plan:approved", To: "implement:dispatched",
		Event: "plan gate approved",
	})
	seedAutoAdvance(t, au, seeded.ID, 9, t0.Add(8*time.Minute), drive.Advance{
		Rule: drive.RuleChecksGreenAwaitingMerge, From: "review:awaiting_approval", To: "awaiting_merge",
		Event:      "review evidence terminal and required PR checks green",
		NextAction: &drive.NextAction{Action: "merge_pr", Detail: "review and merge the PR", PRURL: pr},
	})

	resp, _ := getRunResponse(t, s, seeded.ID)
	if len(resp.AutoAdvanced) != 2 {
		t.Fatalf("auto_advanced = %+v, want 2 entries", resp.AutoAdvanced)
	}
	if resp.AutoAdvanced[0].Rule != string(drive.RulePlanApprovedDispatch) {
		t.Errorf("auto_advanced[0].rule = %q, want plan_approved_dispatch (oldest first)", resp.AutoAdvanced[0].Rule)
	}
	if resp.AutoAdvanced[1].Rule != string(drive.RuleChecksGreenAwaitingMerge) ||
		resp.AutoAdvanced[1].From != "review:awaiting_approval" || resp.AutoAdvanced[1].To != "awaiting_merge" {
		t.Errorf("auto_advanced[1] = %+v, want the checks_green transition", resp.AutoAdvanced[1])
	}
	if resp.AutoAdvanced[1].Timestamp.IsZero() {
		t.Error("auto_advanced[1].ts is zero, want the audit entry timestamp")
	}
	if resp.NextAction == nil || resp.NextAction.Action != "merge_pr" || resp.NextAction.PRURL != pr {
		t.Errorf("next_action = %+v, want merge_pr with the PR URL", resp.NextAction)
	}
	if resp.DerivedStatus != "awaiting_merge" {
		t.Errorf("derived_status = %q, want awaiting_merge", resp.DerivedStatus)
	}
}

// TestGetRun_Drive_TerminalRun_OmitsNextAction pins the staleness
// guard: a terminal run keeps its auto_advanced history but surfaces
// no next_action / derived_status — the recorded next step is history,
// not an instruction.
func TestGetRun_Drive_TerminalRun_OmitsNextAction(t *testing.T) {
	s, _, au, seeded := newDriveGetServer(t)
	pr := "https://github.com/x/y/pull/7"
	seeded.PullRequestURL = &pr
	seeded.State = run.StateSucceeded

	seedAutoAdvance(t, au, seeded.ID, 9, time.Now().UTC(), drive.Advance{
		Rule: drive.RuleChecksGreenAwaitingMerge, From: "review:awaiting_approval", To: "awaiting_merge",
		NextAction: &drive.NextAction{Action: "merge_pr", PRURL: pr},
	})

	resp, raw := getRunResponse(t, s, seeded.ID)
	if len(resp.AutoAdvanced) != 1 {
		t.Fatalf("auto_advanced = %+v, want the history preserved on a terminal run", resp.AutoAdvanced)
	}
	if _, present := raw["next_action"]; present {
		t.Error("next_action present on a terminal run")
	}
	if _, present := raw["derived_status"]; present {
		t.Error("derived_status present on a terminal run")
	}
}

// TestGetRun_Drive_NoPR_OmitsDerivedStatus: awaiting_merge requires an
// open PR on the row — the checks_green stamp alone is not enough.
func TestGetRun_Drive_NoPR_OmitsDerivedStatus(t *testing.T) {
	s, _, au, seeded := newDriveGetServer(t)
	seedAutoAdvance(t, au, seeded.ID, 9, time.Now().UTC(), drive.Advance{
		Rule: drive.RuleChecksGreenAwaitingMerge, From: "review:awaiting_approval", To: "awaiting_merge",
		NextAction: &drive.NextAction{Action: "merge_pr"},
	})

	resp, raw := getRunResponse(t, s, seeded.ID)
	if _, present := raw["derived_status"]; present {
		t.Error("derived_status present without a PR on the run row")
	}
	if resp.NextAction == nil {
		t.Error("next_action should still surface (it does not require the PR row stamp)")
	}
}

// TestGetRun_Drive_SupersededChecksGreen_OmitsDerivedStatus pins
// supersession: a fix-up re-park recorded AFTER checks_green means the
// run is no longer awaiting merge — only the LATEST entry derives the
// presentation status.
func TestGetRun_Drive_SupersededChecksGreen_OmitsDerivedStatus(t *testing.T) {
	s, _, au, seeded := newDriveGetServer(t)
	pr := "https://github.com/x/y/pull/7"
	seeded.PullRequestURL = &pr

	t0 := time.Now().UTC().Add(-5 * time.Minute)
	seedAutoAdvance(t, au, seeded.ID, 5, t0, drive.Advance{
		Rule: drive.RuleChecksGreenAwaitingMerge, From: "review:awaiting_approval", To: "awaiting_merge",
		NextAction: &drive.NextAction{Action: "merge_pr", PRURL: pr},
	})
	seedAutoAdvance(t, au, seeded.ID, 8, t0.Add(2*time.Minute), drive.Advance{
		Rule: drive.RuleFixupRereviewRepark, From: "review:awaiting_approval", To: "review:pending",
	})

	resp, raw := getRunResponse(t, s, seeded.ID)
	if _, present := raw["derived_status"]; present {
		t.Errorf("derived_status present though checks_green was superseded by a fix-up re-park: %+v", resp)
	}
	if len(resp.AutoAdvanced) != 2 {
		t.Errorf("auto_advanced = %+v, want both transitions listed", resp.AutoAdvanced)
	}
}

// TestGetRun_Drive_CorruptPayloadSkipped: a corrupt run_auto_advanced
// payload degrades to the readable entries, never a 500.
func TestGetRun_Drive_CorruptPayloadSkipped(t *testing.T) {
	s, _, au, seeded := newDriveGetServer(t)
	rid := seeded.ID
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &rid, Sequence: 3, Category: drive.Category,
		Payload: []byte(`{not json`), Timestamp: time.Now().UTC(),
	})
	seedAutoAdvance(t, au, seeded.ID, 4, time.Now().UTC(), drive.Advance{
		Rule: drive.RulePlanApprovedDispatch, From: "plan:approved", To: "implement:dispatched",
	})

	resp, _ := getRunResponse(t, s, seeded.ID)
	if len(resp.AutoAdvanced) != 1 || resp.AutoAdvanced[0].Rule != string(drive.RulePlanApprovedDispatch) {
		t.Errorf("auto_advanced = %+v, want only the readable entry", resp.AutoAdvanced)
	}
}

// TestGetRun_Drive_AuditReadFailure_OmitsDriveSurfaces: best-effort —
// an audit-store failure omits the drive fields, never fails the read.
func TestGetRun_Drive_AuditReadFailure_OmitsDriveSurfaces(t *testing.T) {
	s, _, au, seeded := newDriveGetServer(t)
	au.listByCategoryErr = errors.New("store down")

	_, raw := getRunResponse(t, s, seeded.ID)
	for _, key := range []string{"auto_advanced", "next_action", "derived_status"} {
		if _, present := raw[key]; present {
			t.Errorf("%s present despite the audit read failure", key)
		}
	}
}

// TestGetRun_NonDrive_OmitsDriveSurfaces is the mandatory control: a
// drive:false run surfaces none of the drive fields even when (stray)
// run_auto_advanced entries exist — the read is gated on the flag.
func TestGetRun_NonDrive_OmitsDriveSurfaces(t *testing.T) {
	s, _, au, seeded := newDriveGetServer(t)
	seeded.Drive = false
	seedAutoAdvance(t, au, seeded.ID, 4, time.Now().UTC(), drive.Advance{
		Rule: drive.RulePlanApprovedDispatch, From: "plan:approved", To: "implement:dispatched",
	})

	_, raw := getRunResponse(t, s, seeded.ID)
	for _, key := range []string{"auto_advanced", "next_action", "derived_status"} {
		if _, present := raw[key]; present {
			t.Errorf("%s present on a non-drive run", key)
		}
	}
}

// TestListRuns_OmitsDriveSurfaces pins the list-path posture (mirrors
// TestListRuns_OmitsConcernsBlock): the list endpoint never pays a
// per-row audit query, so its items carry no drive read surfaces.
func TestListRuns_OmitsDriveSurfaces(t *testing.T) {
	s, _, au, seeded := newDriveGetServer(t)
	seedAutoAdvance(t, au, seeded.ID, 4, time.Now().UTC(), drive.Advance{
		Rule: drive.RulePlanApprovedDispatch, From: "plan:approved", To: "implement:dispatched",
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs", nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(resp.Items))
	}
	for _, key := range []string{"auto_advanced", "next_action", "derived_status"} {
		if _, present := resp.Items[0][key]; present {
			t.Errorf("list item carries %s — the list path must stay free of the per-row audit query", key)
		}
	}
}

// --- Drive end-to-end (#1023) ----------------------------------------------

// driveE2ERepo composes the create-capable fakeRepo with the
// transition + listing methods the approval handler and orchestrator
// need, so one test can cross POST /v0/runs → plan-gate approval →
// orchestrator dispatch → GET /v0/runs/{id} on a single repository
// fake. CreateRun additionally stamps Drive from the params (fakeRepo
// predates the flag and doesn't thread it).
type driveE2ERepo struct{ *fakeRepo }

func (r *driveE2ERepo) CreateRun(ctx context.Context, p run.CreateRunParams) (*run.Run, error) {
	created, err := r.fakeRepo.CreateRun(ctx, p)
	if err != nil {
		return nil, err
	}
	created.Drive = p.Drive
	return created, nil
}

func (r *driveE2ERepo) ListStagesForRun(_ context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*run.Stage, len(r.stagesByRun[runID]))
	copy(out, r.stagesByRun[runID])
	return out, nil
}

func (r *driveE2ERepo) TransitionStage(_ context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, stages := range r.stagesByRun {
		for _, st := range stages {
			if st.ID != id {
				continue
			}
			if !run.ValidStageTransition(st.State, to) && !run.ValidStageFixupTransition(st.State, to) {
				return nil, run.InvalidTransitionError{Kind: "stage", From: string(st.State), To: string(to)}
			}
			st.State = to
			if c != nil {
				st.FailureCategory = c.FailureCategory
				st.FailureReason = c.FailureReason
			}
			st.UpdatedAt = time.Now().UTC()
			return st, nil
		}
	}
	return nil, run.ErrNotFound
}

// startDriveE2ERun POSTs a gated two-stage run through the real create
// handler and walks the plan stage to awaiting_approval (standing in
// for the runner's trace upload, which is out of this seam's scope).
// Returns the created run id and the plan stage.
func startDriveE2ERun(t *testing.T, s *Server, repo *driveE2ERepo, body map[string]any) (uuid.UUID, *run.Stage) {
	t.Helper()
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d:\n%s", w.Code, w.Body.String())
	}
	var created runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	stages := repo.stagesFor(created.ID)
	if len(stages) != 2 || stages[0].Type != run.StageTypePlan {
		t.Fatalf("stages = %+v, want [plan implement]", stages)
	}
	stages[0].State = run.StageStateAwaitingApproval
	return created.ID, stages[0]
}

// newDriveE2EServer wires the full approval + orchestrator + audit
// stack over the composite repo.
func newDriveE2EServer(t *testing.T) (*Server, *driveE2ERepo, *auditFake) {
	t.Helper()
	repo := &driveE2ERepo{fakeRepo: newFakeRepo()}
	au := newAuditFake()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      repo,
		AuditRepo:    au,
		ApprovalRepo: newFakeApprovalRepo(),
		Orchestrator: &orchestrator.Orchestrator{Runs: repo},
	})
	return s, repo, au
}

// TestDriveRun_EndToEnd_GitHubActions is the cross-boundary seam test
// the plan requires (#1023, cf. #618/#627): one flow crossing
// API → domain → persistence → audit → render. POST /v0/runs with
// drive:true, approve the plan gate, then assert (a) the orchestrator
// auto-dispatched the implement stage with no operator call, (b) a
// run_auto_advanced entry landed naming plan_approved_dispatch, and
// (c) GET /v0/runs/{id} renders drive:true + the auto_advanced list.
// runner_kind github_actions executes the advance, so no next_action.
func TestDriveRun_EndToEnd_GitHubActions(t *testing.T) {
	s, repo, au := newDriveE2EServer(t)
	runID, planStage := startDriveE2ERun(t, s, repo, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "cli", "workflow_spec": gatedSpecYAML, "drive": true,
	})

	w := submitApproval(t, s, planStage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("approval status = %d:\n%s", w.Code, w.Body.String())
	}

	// (a) The implement stage auto-dispatched — no operator call between
	// the approval and the dispatch.
	stages := repo.stagesFor(runID)
	if stages[1].State != run.StageStateDispatched {
		t.Errorf("implement state = %q, want dispatched", stages[1].State)
	}

	// (b) The advance is attributable in the audit trail.
	var advances []drive.Advance
	for _, e := range au.appended {
		if e.Category != drive.Category {
			continue
		}
		var adv drive.Advance
		if err := json.Unmarshal(e.Payload, &adv); err != nil {
			t.Fatalf("run_auto_advanced payload unmarshal: %v", err)
		}
		advances = append(advances, adv)
	}
	if len(advances) != 1 || advances[0].Rule != drive.RulePlanApprovedDispatch {
		t.Fatalf("run_auto_advanced = %+v, want one plan_approved_dispatch entry", advances)
	}

	// (c) The GET surface renders the drive view.
	resp, raw := getRunResponse(t, s, runID)
	if !resp.Drive {
		t.Error("drive = false, want true")
	}
	if len(resp.AutoAdvanced) != 1 || resp.AutoAdvanced[0].Rule != string(drive.RulePlanApprovedDispatch) {
		t.Fatalf("auto_advanced = %+v, want [plan_approved_dispatch]", resp.AutoAdvanced)
	}
	if resp.AutoAdvanced[0].Parked {
		t.Error("parked = true, want false: github_actions dispatch executes the advance")
	}
	if _, present := raw["next_action"]; present {
		t.Error("next_action present — an executed github_actions dispatch leaves nothing for the operator")
	}
}

// TestDriveRun_EndToEnd_LocalRunner: same seam, runner_kind local —
// the backend cannot spawn the host-side runner (ADR-024), so the rule
// parks and GET renders the distilled ready-to-run next_action.
func TestDriveRun_EndToEnd_LocalRunner(t *testing.T) {
	s, repo, _ := newDriveE2EServer(t)
	runID, planStage := startDriveE2ERun(t, s, repo, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "cli", "runner_kind": "local",
		"workflow_spec": gatedSpecYAML, "drive": true,
	})

	w := submitApproval(t, s, planStage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("approval status = %d:\n%s", w.Code, w.Body.String())
	}

	resp, _ := getRunResponse(t, s, runID)
	if len(resp.AutoAdvanced) != 1 || !resp.AutoAdvanced[0].Parked {
		t.Fatalf("auto_advanced = %+v, want one parked plan_approved_dispatch entry", resp.AutoAdvanced)
	}
	if resp.NextAction == nil || resp.NextAction.Action != "run_implement_stage" {
		t.Fatalf("next_action = %+v, want run_implement_stage", resp.NextAction)
	}
}

// TestDriveRun_EndToEnd_NonDriveControl is the mandatory control: the
// identical flow with drive:false stamps no run_auto_advanced entry
// and GET renders none of the drive read surfaces.
func TestDriveRun_EndToEnd_NonDriveControl(t *testing.T) {
	s, repo, au := newDriveE2EServer(t)
	runID, planStage := startDriveE2ERun(t, s, repo, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "cli", "workflow_spec": gatedSpecYAML, "drive": false,
	})

	w := submitApproval(t, s, planStage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("approval status = %d:\n%s", w.Code, w.Body.String())
	}

	for _, e := range au.appended {
		if e.Category == drive.Category {
			t.Fatalf("run_auto_advanced entry on a non-drive run: %s", e.Payload)
		}
	}
	resp, raw := getRunResponse(t, s, runID)
	if resp.Drive {
		t.Error("drive = true, want false")
	}
	for _, key := range []string{"auto_advanced", "next_action", "derived_status"} {
		if _, present := raw[key]; present {
			t.Errorf("%s present on a non-drive run", key)
		}
	}
	// The legacy behavior is otherwise unchanged: the approval still
	// dispatched the implement stage (drive changes attribution +
	// surfaces, not the orchestrator handoff).
	stages := repo.stagesFor(runID)
	if stages[1].State != run.StageStateDispatched {
		t.Errorf("implement state = %q, want dispatched (legacy path unchanged)", stages[1].State)
	}
}
