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

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// goldenMergeRunRequestJSON is the EXACT request body the fishhawk_merge_run
// MCP tool serializes and this handler accepts — the shared wire contract
// (binding approval condition 2). The MCP client test (sibling slice) must
// replay this byte-for-byte and parse a response matching mergeRunResponse, so
// the verdict/pr_url boundary cannot drift while both suites pass.
const goldenMergeRunRequestJSON = `{"verdict":"lgtm — checks green, approved"}`

// goldenMergeRunVerdict is the verdict text inside goldenMergeRunRequestJSON,
// asserted against the recorded audit payload + the response echo.
const goldenMergeRunVerdict = "lgtm — checks green, approved"

// mergeRunPost issues POST /v0/runs/{run_id}/merge against handleMergeRun with
// an injected identity (the autodrive convention: the auth middleware is
// bypassed so the handler's own auth ladder is exercised directly).
func mergeRunPost(t *testing.T, s *Server, runID uuid.UUID, body string, id Identity) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID.String()+"/merge", strings.NewReader(body))
	req.SetPathValue("run_id", runID.String())
	req = injectIdentity(req, id)
	s.handleMergeRun(w, req)
	return w
}

// verdictObservingMerger records, at dispatch time, whether the
// merge_verdict_recorded row was already durable — proving the handler
// appends the verdict BEFORE it queues the merge.
type verdictObservingMerger struct {
	au               *auditFake
	called           int
	sawRowAtDispatch bool
	err              error
}

func (m *verdictObservingMerger) MergePullRequest(_ context.Context, _ *run.Run) error {
	m.called++
	if countAudit(m.au, CategoryMergeVerdictRecorded) > 0 {
		m.sawRowAtDispatch = true
	}
	return m.err
}

// --- route registration (condition 3: handlers_test.go untouched) -----------

func TestMergeRunRouteRegistered(t *testing.T) {
	s := New(Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/00000000-0000-0000-0000-000000000000/merge", strings.NewReader("{}"))
	s.Handler().ServeHTTP(rec, req)
	// An UNregistered route 404s from the mux; the handler's anonymous guard
	// 401s — so a 401 proves the route reached handleMergeRun.
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (route reaches handler)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "authentication_required") {
		t.Errorf("body = %s, want authentication_required", rec.Body.String())
	}
}

// --- auth ladder -------------------------------------------------------------

func TestMergeRun_Unauthenticated(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := seedMergeReadyRun(t, s, repo, au)
	w := mergeRunPost(t, s, runID, goldenMergeRunRequestJSON, anonIdentity())
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if countAudit(au, CategoryMergeVerdictRecorded) != 0 {
		t.Error("verdict row appended on an unauthenticated request")
	}
}

func TestMergeRun_RunBoundToken_Forbidden(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := seedMergeReadyRun(t, s, repo, au)
	// A run-bound agent token, even carrying write:approvals, may not merge.
	id := Identity{Subject: "mcp:run:" + uuid.New().String(), TokenID: "tok-run", Scopes: []string{"write:approvals"}}
	w := mergeRunPost(t, s, runID, goldenMergeRunRequestJSON, id)
	assertScopeError(t, w, http.StatusForbidden, "run_token_forbidden")
	if countAudit(au, CategoryMergeVerdictRecorded) != 0 {
		t.Error("verdict row appended for a run-bound token")
	}
}

func TestMergeRun_MissingScope_Forbidden(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := seedMergeReadyRun(t, s, repo, au)
	id := Identity{Subject: "github:op", TokenID: "tok-x", Scopes: []string{"mcp:read"}}
	w := mergeRunPost(t, s, runID, goldenMergeRunRequestJSON, id)
	assertScopeError(t, w, http.StatusForbidden, "insufficient_scope")
}

// --- body validation ---------------------------------------------------------

func TestMergeRun_EmptyVerdict_Rejected(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := seedMergeReadyRun(t, s, repo, au)
	for _, body := range []string{`{}`, `{"verdict":"   "}`} {
		w := mergeRunPost(t, s, runID, body, autoDriveOperatorIdentity())
		assertScopeError(t, w, http.StatusBadRequest, "validation_failed")
	}
	if countAudit(au, CategoryMergeVerdictRecorded) != 0 {
		t.Error("verdict row appended on an empty verdict")
	}
}

// --- run-state guards --------------------------------------------------------

func TestMergeRun_UnknownRun_NotFound(t *testing.T) {
	s, _, _ := newAutoDriveMergeServer(t, &fakeMerger{})
	w := mergeRunPost(t, s, uuid.New(), goldenMergeRunRequestJSON, autoDriveOperatorIdentity())
	assertScopeError(t, w, http.StatusNotFound, "run_not_found")
}

func TestMergeRun_NoPullRequest_Conflict(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID, stages := startAutoDriveRun(t, s, repo)
	stages[0].State = run.StageStateSucceeded
	stages[1].State = run.StageStateSucceeded
	if _, err := repo.TransitionRun(context.Background(), runID, run.StateRunning); err != nil {
		t.Fatalf("TransitionRun: %v", err)
	}
	// No SetRunPullRequestURL — PullRequestURL stays nil.
	w := mergeRunPost(t, s, runID, goldenMergeRunRequestJSON, autoDriveOperatorIdentity())
	assertScopeError(t, w, http.StatusConflict, "no_pull_request")
	if countAudit(au, CategoryMergeVerdictRecorded) != 0 {
		t.Error("verdict row appended on a PR-less run")
	}
}

func TestMergeRun_TerminalRun_Conflict(t *testing.T) {
	for _, state := range []run.State{run.StateFailed, run.StateCancelled} {
		t.Run(string(state), func(t *testing.T) {
			s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
			runID := seedMergeReadyRun(t, s, repo, au)
			if _, err := repo.TransitionRun(context.Background(), runID, state); err != nil {
				t.Fatalf("TransitionRun -> %s: %v", state, err)
			}
			w := mergeRunPost(t, s, runID, goldenMergeRunRequestJSON, autoDriveOperatorIdentity())
			assertScopeError(t, w, http.StatusConflict, "run_not_mergeable")
			if countAudit(au, CategoryMergeVerdictRecorded) != 0 {
				t.Errorf("verdict row appended on a %s run", state)
			}
		})
	}
}

// --- acceptance gate (one case per state) ------------------------------------

func TestMergeRun_AcceptanceGate(t *testing.T) {
	t.Run("pending -> 409", func(t *testing.T) {
		s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
		runRow := seedAcceptanceMergeRun(t, repo, au, run.StageStateRunning, "") // non-terminal, no verdict
		w := mergeRunPost(t, s, runRow.ID, goldenMergeRunRequestJSON, autoDriveOperatorIdentity())
		assertScopeError(t, w, http.StatusConflict, "acceptance_gate_not_passed")
		if countAudit(au, CategoryMergeVerdictRecorded) != 0 {
			t.Error("verdict row appended while acceptance pending")
		}
	})
	t.Run("failed -> 409", func(t *testing.T) {
		s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
		runRow := seedAcceptanceMergeRun(t, repo, au, run.StageStateSucceeded, acceptanceVerdictFailed)
		w := mergeRunPost(t, s, runRow.ID, goldenMergeRunRequestJSON, autoDriveOperatorIdentity())
		assertScopeError(t, w, http.StatusConflict, "acceptance_gate_not_passed")
	})
	t.Run("read-error -> fail-closed 409", func(t *testing.T) {
		s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
		runRow := seedAcceptanceMergeRun(t, repo, au, run.StageStateSucceeded, acceptanceVerdictPassed)
		au.listByCategoryErr = errors.New("audit boom")
		w := mergeRunPost(t, s, runRow.ID, goldenMergeRunRequestJSON, autoDriveOperatorIdentity())
		assertScopeError(t, w, http.StatusConflict, "acceptance_gate_not_passed")
	})
	t.Run("passed -> proceed", func(t *testing.T) {
		merger := &fakeMerger{}
		s, repo, au := newAutoDriveMergeServer(t, merger)
		runRow := seedAcceptanceMergeRun(t, repo, au, run.StageStateSucceeded, acceptanceVerdictPassed)
		w := mergeRunPost(t, s, runRow.ID, goldenMergeRunRequestJSON, autoDriveOperatorIdentity())
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		if merger.called != 1 {
			t.Errorf("merger called %d times, want 1 on a passed acceptance", merger.called)
		}
	})
	t.Run("skipped-out-of-scope -> proceed", func(t *testing.T) {
		merger := &fakeMerger{}
		s, repo, au := newAutoDriveMergeServer(t, merger)
		runRow := seedAcceptanceMergeRun(t, repo, au, run.StageStateSucceeded, "") // terminal, no verdict
		repo.mu.Lock()
		accID := repo.stagesByRun[runRow.ID][2].ID
		repo.mu.Unlock()
		seedAcceptanceSkipMarker(au, runRow.ID, accID)
		w := mergeRunPost(t, s, runRow.ID, goldenMergeRunRequestJSON, autoDriveOperatorIdentity())
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		if merger.called != 1 {
			t.Errorf("merger called %d times, want 1 on a skip-settled acceptance", merger.called)
		}
	})
}

// errStagesRepo wraps the merge repo so ListStagesForRun fails, exercising the
// stages-read fail-closed branch (acceptance_gate_unverified) — GetRun and every
// other method still delegate to the embedded repo.
type errStagesRepo struct {
	*autoDriveRepo
}

func (errStagesRepo) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	return nil, errors.New("stages read down")
}

func TestMergeRun_StagesReadError_FailsClosed(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := seedMergeReadyRun(t, s, repo, au)
	s.cfg.RunRepo = errStagesRepo{autoDriveRepo: repo}

	w := mergeRunPost(t, s, runID, goldenMergeRunRequestJSON, autoDriveOperatorIdentity())
	assertScopeError(t, w, http.StatusConflict, "acceptance_gate_unverified")
	if countAudit(au, CategoryMergeVerdictRecorded) != 0 {
		t.Error("verdict row appended despite an unverifiable acceptance gate")
	}
}

// --- 503 nil GateMerger, NO write (fail-closed ordering) ---------------------

func TestMergeRun_NilMerger_Unavailable_NoWrite(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, nil) // no GateMerger
	runID := seedMergeReadyRun(t, s, repo, au)
	w := mergeRunPost(t, s, runID, goldenMergeRunRequestJSON, autoDriveOperatorIdentity())
	assertScopeError(t, w, http.StatusServiceUnavailable, "merge_unconfigured")
	if countAudit(au, CategoryMergeVerdictRecorded) != 0 {
		t.Error("verdict row appended before the nil-merger 503 (fail-closed ordering violated)")
	}
}

// --- happy path: row appended BEFORE dispatch, response shape ---------------

func TestMergeRun_HappyPath_RowBeforeDispatch(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, nil)
	merger := &verdictObservingMerger{au: au}
	s.cfg.GateMerger = merger
	runID := seedMergeReadyRun(t, s, repo, au)

	w := mergeRunPost(t, s, runID, goldenMergeRunRequestJSON, autoDriveOperatorIdentity())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp mergeRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// (verdict_sequence is populated from the appended entry's Sequence in
	// production; the audit fake returns 0, so we assert only the boolean shape.)
	if !resp.MergeQueued || resp.AlreadyRecorded {
		t.Errorf("resp = %+v, want merge_queued + not already_recorded", resp)
	}
	if resp.Verdict != goldenMergeRunVerdict || resp.PullRequestURL == "" {
		t.Errorf("resp verdict/pr_url = %+v", resp)
	}
	if merger.called != 1 {
		t.Fatalf("merger called %d times, want 1", merger.called)
	}
	if !merger.sawRowAtDispatch {
		t.Error("merge dispatched BEFORE the verdict row was durable (ordering violated)")
	}

	// The chained merge_verdict_recorded row carries category + ActorUser +
	// verdict + pr_url.
	e := auditEntry(t, au, CategoryMergeVerdictRecorded)
	if e.ActorKind == nil || *e.ActorKind != audit.ActorUser {
		t.Errorf("actor kind = %v, want user", e.ActorKind)
	}
	var fields map[string]any
	if err := json.Unmarshal(e.Payload, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["verdict"] != goldenMergeRunVerdict {
		t.Errorf("payload verdict = %v, want %q", fields["verdict"], goldenMergeRunVerdict)
	}
	if fields["pr_url"] == "" || fields["pr_url"] == nil {
		t.Errorf("payload pr_url missing: %v", fields)
	}
	if fields["delegated"] != false {
		t.Errorf("payload delegated = %v, want false", fields["delegated"])
	}
}

// --- 502 on merger error, verdict row durable -------------------------------

func TestMergeRun_MergerError_502_VerdictDurable(t *testing.T) {
	merger := &fakeMerger{err: errors.New("queue boom")}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := seedMergeReadyRun(t, s, repo, au)

	w := mergeRunPost(t, s, runID, goldenMergeRunRequestJSON, autoDriveOperatorIdentity())
	assertScopeError(t, w, http.StatusBadGateway, "merge_queue_failed")
	if countAudit(au, CategoryMergeVerdictRecorded) != 1 {
		t.Errorf("verdict rows = %d, want 1 (the verdict is durable even when the queue fails)",
			countAudit(au, CategoryMergeVerdictRecorded))
	}
}

// --- review stage at awaiting_approval does NOT block (divergence from
// mergeGateReady) ------------------------------------------------------------

func TestMergeRun_ReviewAwaitingApproval_StillQueues(t *testing.T) {
	merger := &fakeMerger{}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := seedMergeReadyRun(t, s, repo, au)
	// Park a review stage at awaiting_approval — mergeGateReady would refuse,
	// but the operator merge deliberately does not (resolveReviewStageOnMerge
	// settles it on merge).
	repo.mu.Lock()
	repo.stagesByRun[runID] = append(repo.stagesByRun[runID], &run.Stage{
		ID: uuid.New(), RunID: runID, Sequence: 2, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval,
	})
	repo.mu.Unlock()

	w := mergeRunPost(t, s, runID, goldenMergeRunRequestJSON, autoDriveOperatorIdentity())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (awaiting_approval review must not block the operator merge):\n%s", w.Code, w.Body.String())
	}
	if merger.called != 1 {
		t.Errorf("merger called %d times, want 1", merger.called)
	}
}

// --- idempotence (condition 1): repeated POST == one row, second dispatch ----

func TestMergeRun_RepeatedPost_OneRowTwoDispatches(t *testing.T) {
	merger := &fakeMerger{}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := seedMergeReadyRun(t, s, repo, au)

	first := mergeRunPost(t, s, runID, goldenMergeRunRequestJSON, autoDriveOperatorIdentity())
	if first.Code != http.StatusOK {
		t.Fatalf("first POST status = %d:\n%s", first.Code, first.Body.String())
	}
	second := mergeRunPost(t, s, runID, goldenMergeRunRequestJSON, autoDriveOperatorIdentity())
	if second.Code != http.StatusOK {
		t.Fatalf("second POST status = %d:\n%s", second.Code, second.Body.String())
	}
	var resp mergeRunResponse
	if err := json.Unmarshal(second.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.AlreadyRecorded {
		t.Error("second POST: already_recorded = false, want true")
	}
	if got := countAudit(au, CategoryMergeVerdictRecorded); got != 1 {
		t.Errorf("verdict rows = %d, want exactly 1 (no duplicate on re-POST)", got)
	}
	if merger.called != 2 {
		t.Errorf("merger dispatched %d times, want 2 (re-POST always re-queues)", merger.called)
	}
}

// --- 502-then-reinvoke: the merge is re-queued by construction (condition 1) -

func TestMergeRun_502ThenReinvoke_ReQueues(t *testing.T) {
	merger := &fakeMerger{err: errors.New("queue 502")}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := seedMergeReadyRun(t, s, repo, au)

	// First POST: verdict recorded, merge queue fails -> 502.
	failed := mergeRunPost(t, s, runID, goldenMergeRunRequestJSON, autoDriveOperatorIdentity())
	assertScopeError(t, failed, http.StatusBadGateway, "merge_queue_failed")
	if merger.called != 1 {
		t.Fatalf("first dispatch count = %d, want 1", merger.called)
	}

	// Re-invoke after the transient queue failure clears: the MCP tool re-POSTs
	// with no client-side skip, so the merge is re-queued by construction while
	// the verdict is NOT re-recorded.
	merger.err = nil
	ok := mergeRunPost(t, s, runID, goldenMergeRunRequestJSON, autoDriveOperatorIdentity())
	if ok.Code != http.StatusOK {
		t.Fatalf("re-invoke status = %d, want 200:\n%s", ok.Code, ok.Body.String())
	}
	if merger.called != 2 {
		t.Errorf("dispatch count = %d, want 2 (merge re-queued on re-invoke)", merger.called)
	}
	if got := countAudit(au, CategoryMergeVerdictRecorded); got != 1 {
		t.Errorf("verdict rows = %d, want 1 (not re-recorded on retry)", got)
	}
}

// --- wire parity (condition 2): golden request replayed, golden response -----

func TestMergeRun_WireParity_GoldenFixture(t *testing.T) {
	merger := &fakeMerger{}
	s, repo, au := newAutoDriveMergeServer(t, merger)
	runID := seedMergeReadyRun(t, s, repo, au)

	// Replay the EXACT bytes the fishhawk_merge_run tool serializes.
	w := mergeRunPost(t, s, runID, goldenMergeRunRequestJSON, autoDriveOperatorIdentity())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	// The response MUST deserialize into mergeRunResponse with the verdict/pr_url
	// boundary intact — the sibling MCP client test parses the same shape.
	var resp mergeRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not a valid mergeRunResponse: %v\n%s", err, w.Body.String())
	}
	if resp.RunID != runID.String() || resp.Verdict != goldenMergeRunVerdict {
		t.Errorf("resp run_id/verdict = %+v", resp)
	}
	if !resp.MergeQueued || resp.PullRequestURL == "" {
		t.Errorf("resp = %+v, want merge_queued + non-empty pr_url", resp)
	}
}
