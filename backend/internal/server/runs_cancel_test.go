package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
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

// TestCancelRun_RecordsDroppedScenarioRetirements is the operator-cancel sink
// proof (#3389): POST /v0/runs/{id}/cancel on a run carrying an approved
// retire_scenario whose acceptance stage is still pending returns 200 with
// the run cancelled AND appends exactly one
// acceptance_scenario_retirement_dropped row (cancel_source operator_cancel),
// read back through the REST audit feed an operator uses. The idempotent
// second cancel returns 200 and the count stays 1. Counterfactual (A):
// deleting the helper call in handleCancelRun leaves zero rows.
func TestCancelRun_RecordsDroppedScenarioRetirements(t *testing.T) {
	c := newCancelDropSeam(t, run.StageStatePending, true, cancelDropRetired)
	cancel := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v0/runs/%s/cancel", c.runID), nil)
		req.SetPathValue("run_id", c.runID.String())
		w := httptest.NewRecorder()
		c.s.handleCancelRun(w, withAuth(req))
		return w
	}
	w := cancel()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var got runResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.State != string(run.StateCancelled) {
		t.Fatalf("State = %q, want cancelled", got.State)
	}
	items := auditFeedItems(t, c.s, c.runID, CategoryAcceptanceScenarioRetirementDropped)
	if len(items) != 1 {
		t.Fatalf("run-audit acceptance_scenario_retirement_dropped items = %d, want exactly 1", len(items))
	}
	var p struct {
		Reason       string   `json:"reason"`
		CancelSource string   `json:"cancel_source"`
		ScenarioIDs  []string `json:"scenario_ids"`
	}
	if err := json.Unmarshal(items[0].Payload, &p); err != nil {
		t.Fatalf("decode payload: %v\n%s", err, items[0].Payload)
	}
	if p.Reason != acceptanceRetirementDropReasonRunCancelled || p.CancelSource != cancelSourceOperator {
		t.Errorf("payload = %s, want reason run_cancelled_before_acceptance + cancel_source operator_cancel", items[0].Payload)
	}
	if len(p.ScenarioIDs) != 1 || p.ScenarioIDs[0] != "scenario:issue-101/crit-b" {
		t.Errorf("scenario_ids = %v", p.ScenarioIDs)
	}
	// Idempotent: the already-cancelled 200 re-enters the sink and appends
	// nothing new.
	if w := cancel(); w.Code != http.StatusOK {
		t.Fatalf("second cancel status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if n := len(auditFeedItems(t, c.s, c.runID, CategoryAcceptanceScenarioRetirementDropped)); n != 1 {
		t.Errorf("items after the idempotent second cancel = %d, want 1", n)
	}
}

// TestCancelRun_NoRetirements_NoDropRow: a cancel of a run whose approval
// carried no retire_scenario appends nothing (the common case).
func TestCancelRun_NoRetirements_NoDropRow(t *testing.T) {
	c := newCancelDropSeam(t, run.StageStatePending, true, nil)
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v0/runs/%s/cancel", c.runID), nil)
	req.SetPathValue("run_id", c.runID.String())
	w := httptest.NewRecorder()
	c.s.handleCancelRun(w, withAuth(req))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if n := len(c.dropRows()); n != 0 {
		t.Errorf("drop rows = %d, want 0", n)
	}
}

// TestCancelRun_SweepsRunBranches drives the REAL handleCancelRun on a
// decomposed parent against an httptest GitHub behind a real githubclient
// (E68.67 / #3562): the consolidated branch and the SLASHED slice branch are
// DELETEd on their hierarchical ref paths after the response, and the
// detached sweep persists one run_branches_swept row.
func TestCancelRun_SweepsRunBranches(t *testing.T) {
	inst := int64(42)
	parent := &run.Run{ID: sweepRunID, Repo: "x/y", State: run.StateRunning, InstallationID: &inst}
	rr := &prEventsRunRepo{
		listResult:       []*run.Run{parent},
		decomposedResult: []*run.Run{childRun(intp(0))},
	}
	ar := &prEventsAuditRepo{}
	gh, client := newGitHubSweepStub(t, sweepConsol, sweepSlice0, "main")
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: ar, GitHub: client})

	if w := cancelRun(t, s, parent.ID); w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	s.waitBranchSweeps()

	want := []string{
		"/repos/x/y/git/refs/heads/" + sweepConsol,
		"/repos/x/y/git/refs/heads/fishhawk/run-11111111/slice-0",
	}
	if got := gh.deletePaths(); !reflect.DeepEqual(got, want) {
		t.Fatalf("DELETE paths = %v, want %v", got, want)
	}
	ar.mu.Lock()
	row := sweptAuditRow(t, ar.appended)
	ar.mu.Unlock()
	if row.Trigger != sweepTriggerCancelled || !reflect.DeepEqual(row.Deleted, []string{sweepConsol, sweepSlice0}) {
		t.Errorf("run_branches_swept = %+v", row)
	}
}

// TestCancelRun_ReleasesMergeCandidateQueue pins the ADR-092 cancel hook
// (#4200) over the REAL queue store, run and audit repositories on one pgtest
// pool: cancelling a HELD run drops its entry run_cancelled and leaves the live
// run holding the line (the next held run moves up to position 1); cancelling
// the LIVE run drops it run_cancelled and admits the next held run, whose pass
// starts (live, trigger appended) — before the 200. Counterfactual: deleting
// the releaseMergeCandidateQueueForRun call in handleCancelRun leaves B held
// and C held behind a cancelled A → RED on both arms.
func TestCancelRun_ReleasesMergeCandidateQueue(t *testing.T) {
	f := newMCQFixture(t)
	a, b := f.wave()
	c := f.seed("c", "c0", []byte(mcvUndelegatedSpecYAML))
	f.behind(c, "c0", "c1")
	if resp := f.rebase(c); resp.MergeCandidateVerifyState != mergeCandidateStateHeld {
		t.Fatalf("C rebase state = %q, want held", resp.MergeCandidateVerifyState)
	}

	// Cancel the HELD run: dropped, the line is untouched, C moves up.
	if w := cancelRun(t, f.srv, b.id); w.Code != http.StatusOK {
		t.Fatalf("cancel B = %d:\n%s", w.Code, w.Body.String())
	}
	f.srv.waitBranchSweeps()
	f.requireState(b, mergequeue.StateDropped, mergeCandidateQueueReasonRunCancelled)
	f.requireState(a, mergequeue.StateLive, "")
	if st, _ := f.srv.mergeCandidateQueueForRun(context.Background(), c.id); st == nil || st.Position != 1 {
		t.Fatalf("C queue status after B's cancel = %+v, want held at position 1", st)
	}

	// Cancel the LIVE run: dropped, and C is admitted before the response.
	if w := cancelRun(t, f.srv, a.id); w.Code != http.StatusOK {
		t.Fatalf("cancel A = %d:\n%s", w.Code, w.Body.String())
	}
	f.srv.waitBranchSweeps()
	f.requireState(a, mergequeue.StateDropped, mergeCandidateQueueReasonRunCancelled)
	if e := f.requireState(c, mergequeue.StateLive, ""); e.Phase != mergequeue.PhaseVerifying {
		t.Fatalf("C phase after A's cancel = %q, want verifying", e.Phase)
	}
	if tr := f.triggers(c); len(tr) != 1 || tr[0].ExpectedHeadSHA != "c1" {
		t.Fatalf("C triggers = %+v, want one for c1", tr)
	}
	settled := f.rows(a, CategoryMergeCandidateQueueSettled)
	if len(settled) == 0 {
		t.Fatal("no merge_candidate_queue_settled row for the cancelled live run")
	}

	// Idempotent: the already-cancelled re-entry settles nothing new.
	if w := cancelRun(t, f.srv, a.id); w.Code != http.StatusOK {
		t.Fatalf("re-cancel A = %d:\n%s", w.Code, w.Body.String())
	}
	f.srv.waitBranchSweeps()
	if n := len(f.rows(a, CategoryMergeCandidateQueueSettled)); n != len(settled) {
		t.Fatalf("settled rows after re-cancel = %d, want %d", n, len(settled))
	}
	f.requireState(c, mergequeue.StateLive, "")
}
