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
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// reviveFixture wires a real orchestrator over the in-memory
// orchestratorRepo with a FAILED run and a set of failed stages. It is
// the cross-boundary substrate: a revive POST exercises the HTTP handler
// → run.ReviveRun domain → RetryStage/RetryRun persistence → run_revived
// audit chain. The real orchestrator is wired precisely so a test can
// assert revive does NOT advance/dispatch (the #1700 no-dispatch
// invariant): if handleReviveRun called Advance, a re-parked pending
// implement stage would walk to dispatched; it must stay pending.
type reviveFixture struct {
	s    *Server
	repo *orchestratorRepo
	au   *approvalAuditFake
	rec  *pageClassRecorder
	run  *run.Run
}

func newReviveFixture(t *testing.T) *reviveFixture {
	t.Helper()
	repo := newOrchestratorRepo()
	au := newApprovalAuditFake()
	o := &orchestrator.Orchestrator{Runs: repo, Audit: au}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      repo,
		AuditRepo:    au,
		Orchestrator: o,
	})
	rec := &pageClassRecorder{}
	s.issueNotifier = rec

	rr := repo.seedRun()
	rr.State = run.StateFailed
	return &reviveFixture{s: s, repo: repo, au: au, rec: rec, run: rr}
}

// seedFailedStage adds a failed stage of the given type/category to the
// fixture's run and returns it.
func (f *reviveFixture) seedFailedStage(t *testing.T, seq int, typ run.StageType, cat run.FailureCategory, reason string) *run.Stage {
	t.Helper()
	st := f.repo.seedStage(f.run.ID, seq, run.StageStateFailed)
	st.Type = typ
	c := cat
	rs := reason
	st.FailureCategory = &c
	st.FailureReason = &rs
	return st
}

func postRevive(t *testing.T, s *Server, runID uuid.UUID, decorate func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID.String()+"/revive", nil)
	req.SetPathValue("run_id", runID.String())
	w := httptest.NewRecorder()
	s.handleReviveRun(w, decorate(req))
	return w
}

// TestReviveRun_EndToEnd is the cross-boundary integration test: a revive
// re-parks the failed A implement stage to pending and the failed D-timeout
// review stage to awaiting_approval, flips the run failed → running, and
// chains one run_revived audit entry listing both restored stages —
// WITHOUT dispatching either (m20, m21, m22, awaiting_approval/pending
// restore shapes, failed→running flip).
func TestReviveRun_EndToEnd(t *testing.T) {
	f := newReviveFixture(t)
	implement := f.seedFailedStage(t, 0, run.StageTypeImplement, run.FailureA, "agent crashed")
	review := f.seedFailedStage(t, 1, run.StageTypeReview, run.FailureD, "sla_timeout: 5h elapsed")

	w := postRevive(t, f.s, f.run.ID, withAuth)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	var body reviveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// failed → running flip.
	if body.Run.State != string(run.StateRunning) {
		t.Errorf("run state = %q, want running", body.Run.State)
	}
	// An ordinary revive (fresh re-parks) reports resumed=false (#1942).
	if body.Resumed {
		t.Errorf("resumed = true, want false (this revive performed fresh re-parks)")
	}
	// Response carries both restored stages with their restore shapes.
	if len(body.RestoredStages) != 2 {
		t.Fatalf("restored %d stages, want 2:\n%s", len(body.RestoredStages), w.Body.String())
	}
	byID := map[uuid.UUID]reviveRestoredStage{}
	for _, rs := range body.RestoredStages {
		byID[rs.StageID] = rs
	}
	if got := byID[implement.ID].RestoredState; got != string(run.StageStatePending) {
		t.Errorf("implement restored state = %q, want pending", got)
	}
	if got := byID[review.ID].RestoredState; got != string(run.StageStateAwaitingApproval) {
		t.Errorf("review restored state = %q, want awaiting_approval", got)
	}

	// m21 (the #1700 no-dispatch guard): revive performs NO Advance. With a
	// real orchestrator wired, a dispatched revive would have walked the
	// re-parked A implement stage pending → dispatched. It must remain
	// pending — proof that ZERO Advance calls fired on the revive path.
	gotImpl, _ := f.repo.GetStage(context.Background(), implement.ID)
	if gotImpl.State != run.StageStatePending {
		t.Errorf("implement stage = %q, want pending (revive must not dispatch — no Advance)", gotImpl.State)
	}
	if gotImpl.FailureCategory != nil {
		t.Errorf("implement stage still carries failure metadata after revive")
	}

	// m20: exactly one run_revived audit entry, listing both stages.
	var revived *audit.ChainAppendParams
	count := 0
	for i := range f.au.appended {
		if f.au.appended[i].Category == RunRevivedCategory {
			revived = &f.au.appended[i]
			count++
		}
	}
	if count != 1 {
		t.Fatalf("run_revived audit entries = %d, want exactly 1", count)
	}
	var payload map[string]any
	if err := json.Unmarshal(revived.Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	if sc, _ := payload["stage_count"].(float64); int(sc) != 2 {
		t.Errorf("audit stage_count = %v, want 2", payload["stage_count"])
	}
	stages, ok := payload["restored_stages"].([]any)
	if !ok || len(stages) != 2 {
		t.Errorf("audit restored_stages = %v, want 2 entries", payload["restored_stages"])
	}
	// The audit entry is a run-level action (no single stage id).
	if revived.StageID != nil {
		t.Errorf("run_revived audit StageID = %v, want nil (run-level action)", revived.StageID)
	}

	// m22: the sticky status comment fired.
	found := false
	for _, id := range f.rec.status {
		if id == f.run.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("notifyStatusUpdate did not fire for the revived run; status=%v", f.rec.status)
	}
}

// TestReviveRun_DecomposedParentRestoresToAwaitingChildren proves the
// #1891 restore shape crosses the HTTP boundary: a failed implement stage
// on a decomposition PARENT re-parks to awaiting_children, not pending.
func TestReviveRun_DecomposedParentRestoresToAwaitingChildren(t *testing.T) {
	f := newReviveFixture(t)
	implement := f.seedFailedStage(t, 0, run.StageTypeImplement, run.FailureA, "child fan-out failed")
	// Seed a decomposition child so ListRuns(DecomposedFrom) is non-empty.
	child := f.repo.seedRun()
	child.DecomposedFrom = &f.run.ID

	w := postRevive(t, f.s, f.run.ID, withAuth)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	gotImpl, _ := f.repo.GetStage(context.Background(), implement.ID)
	if gotImpl.State != run.StageStateAwaitingChildren {
		t.Errorf("implement stage = %q, want awaiting_children (decomposed-parent restore, #1891)", gotImpl.State)
	}
}

// TestReviveRun_ResumedShapeEndToEnd drives the interrupted-revive resume
// branch across the HTTP boundary (#1942): a failed run with ZERO failed
// stages but one stage parked pending (the shape a tail RetryRun failure
// leaves) is completed by revive — 200 with restored_stages empty and
// resumed true, run flipped to running, and the run_revived audit payload
// carrying resumed=true with stage_count 0.
func TestReviveRun_ResumedShapeEndToEnd(t *testing.T) {
	f := newReviveFixture(t)
	// One stage already re-parked pending (no failed stages) — the leftover
	// of an interrupted prior revive.
	parked := f.repo.seedStage(f.run.ID, 0, run.StageStatePending)
	parked.Type = run.StageTypeImplement

	w := postRevive(t, f.s, f.run.ID, withAuth)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	var body reviveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !body.Resumed {
		t.Errorf("resumed = false, want true (interrupted-revive resume)")
	}
	if len(body.RestoredStages) != 0 {
		t.Errorf("restored %d stages, want 0 (resume re-parks nothing)", len(body.RestoredStages))
	}
	if body.Run.State != string(run.StateRunning) {
		t.Errorf("run state = %q, want running", body.Run.State)
	}
	// The re-parked stage is untouched: still pending, no dispatch.
	gotStage, _ := f.repo.GetStage(context.Background(), parked.ID)
	if gotStage.State != run.StageStatePending {
		t.Errorf("parked stage = %q, want pending (resume must not re-park or dispatch)", gotStage.State)
	}

	// The run_revived audit records resumed=true with stage_count 0.
	var revived *audit.ChainAppendParams
	for i := range f.au.appended {
		if f.au.appended[i].Category == RunRevivedCategory {
			revived = &f.au.appended[i]
		}
	}
	if revived == nil {
		t.Fatalf("no run_revived audit entry")
	}
	var payload map[string]any
	if err := json.Unmarshal(revived.Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	if resumed, _ := payload["resumed"].(bool); !resumed {
		t.Errorf("audit resumed = %v, want true", payload["resumed"])
	}
	if sc, _ := payload["stage_count"].(float64); int(sc) != 0 {
		t.Errorf("audit stage_count = %v, want 0", payload["stage_count"])
	}
}

// m16: an MCP/agent subject-bound token may not revive any run — not even
// its own. Rejected 403 agent_token_forbidden with no mutation.
func TestReviveRun_AgentTokenRejected(t *testing.T) {
	f := newReviveFixture(t)
	f.seedFailedStage(t, 0, run.StageTypeImplement, run.FailureA, "agent crashed")

	withAgent := func(req *http.Request) *http.Request {
		id := Identity{
			Subject: "mcp:run:" + f.run.ID.String(),
			TokenID: "tok-agent",
			Scopes:  []string{"mcp:read", "write:retries"},
		}
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
	}

	w := postRevive(t, f.s, f.run.ID, withAgent)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "agent_token_forbidden") {
		t.Errorf("body missing agent_token_forbidden code: %s", w.Body.String())
	}
	got, _ := f.repo.GetRun(context.Background(), f.run.ID)
	if got.State != run.StateFailed {
		t.Errorf("run = %q, want failed (revive must not have run)", got.State)
	}
	for _, a := range f.au.appended {
		if a.Category == RunRevivedCategory {
			t.Errorf("run_revived audit written despite rejection")
		}
	}
}

// m17: an operator token missing both write scopes is refused 403.
func TestReviveRun_InsufficientScopeReturns403(t *testing.T) {
	f := newReviveFixture(t)
	f.seedFailedStage(t, 0, run.StageTypeImplement, run.FailureA, "agent crashed")

	withNoScope := func(req *http.Request) *http.Request {
		id := Identity{Subject: "github:ops", TokenID: "tok-ops", Scopes: []string{"read:runs"}}
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
	}

	w := postRevive(t, f.s, f.run.ID, withNoScope)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
}

// An operator bearer token carrying write:retries is accepted.
func TestReviveRun_OperatorScopedToken(t *testing.T) {
	f := newReviveFixture(t)
	f.seedFailedStage(t, 0, run.StageTypeImplement, run.FailureC, "runner OOM")

	withRetryScope := func(req *http.Request) *http.Request {
		id := Identity{Subject: "github:ops", TokenID: "tok-ops", Scopes: []string{"write:retries"}}
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
	}

	w := postRevive(t, f.s, f.run.ID, withRetryScope)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
}

// m18: an unknown run id is 404.
func TestReviveRun_NotFoundReturns404(t *testing.T) {
	f := newReviveFixture(t)
	w := postRevive(t, f.s, uuid.New(), withAuth)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// m19a: a run that is not failed is 422 revive_not_applicable.
func TestReviveRun_NotFailedReturns422(t *testing.T) {
	f := newReviveFixture(t)
	f.run.State = run.StateRunning
	f.seedFailedStage(t, 0, run.StageTypeImplement, run.FailureA, "agent crashed")

	w := postRevive(t, f.s, f.run.ID, withAuth)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "revive_not_applicable") {
		t.Errorf("body missing revive_not_applicable code: %s", w.Body.String())
	}
}

// m19b: a failed run with zero failed stages is 422.
func TestReviveRun_NoFailedStagesReturns422(t *testing.T) {
	f := newReviveFixture(t)
	// A succeeded stage only — nothing to re-park.
	st := f.repo.seedStage(f.run.ID, 0, run.StageStateSucceeded)
	st.Type = run.StageTypeImplement

	w := postRevive(t, f.s, f.run.ID, withAuth)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "revive_not_applicable") {
		t.Errorf("body missing revive_not_applicable code: %s", w.Body.String())
	}
}

// m19c: a failed run with a non-retryable (category-B) failed stage is 422,
// with NO partial mutation — the retryable sibling stays failed and the run
// stays failed.
func TestReviveRun_NonRetryableStageReturns422NoPartialMutation(t *testing.T) {
	f := newReviveFixture(t)
	stageA := f.seedFailedStage(t, 0, run.StageTypeImplement, run.FailureA, "agent crashed")
	f.seedFailedStage(t, 1, run.StageTypeReview, run.FailureB, "constraint violation")

	w := postRevive(t, f.s, f.run.ID, withAuth)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "revive_not_applicable") {
		t.Errorf("body missing revive_not_applicable code: %s", w.Body.String())
	}
	// No partial mutation.
	gotA, _ := f.repo.GetStage(context.Background(), stageA.ID)
	if gotA.State != run.StageStateFailed {
		t.Errorf("sibling A stage = %q, want failed (no partial re-park)", gotA.State)
	}
	gotRun, _ := f.repo.GetRun(context.Background(), f.run.ID)
	if gotRun.State != run.StateFailed {
		t.Errorf("run = %q, want failed (no reopen on refusal)", gotRun.State)
	}
	for _, a := range f.au.appended {
		if a.Category == RunRevivedCategory {
			t.Errorf("run_revived audit written despite refusal")
		}
	}
}

// TestReviveRun_AuditAppendFailure_BestEffort200 pins writeReviveAudit's
// deliberate best-effort contract: the re-park transitions are committed by
// run.ReviveRun BEFORE the audit entry is chained, so a failure appending the
// run_revived provenance record logs but does NOT unwind the reopen — the
// handler still returns 200 with the run flipped failed → running. Returning an
// error here would mislead the operator into thinking the revive did not happen
// when the run IS revived; the correct behavior is a logged, non-fatal
// audit-append failure. This test exists so that swallow is a pinned,
// intentional contract rather than an unobserved silent failure (the security
// review's "revive state changes returned unaudited" concern).
func TestReviveRun_AuditAppendFailure_BestEffort200(t *testing.T) {
	f := newReviveFixture(t)
	f.seedFailedStage(t, 0, run.StageTypeImplement, run.FailureA, "agent crashed")
	f.au.appendErr = errors.New("audit store down")

	w := postRevive(t, f.s, f.run.ID, withAuth)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (audit-append failure is best-effort, does not unwind the committed reopen):\n%s", w.Code, w.Body.String())
	}
	// The reopen is committed regardless of the audit failure.
	gotRun, _ := f.repo.GetRun(context.Background(), f.run.ID)
	if gotRun.State != run.StateRunning {
		t.Errorf("run = %q, want running (transitions commit before the audit append)", gotRun.State)
	}
	// No run_revived entry landed — the append failed — but the handler still
	// succeeded rather than 500ing on a committed state change.
	for _, a := range f.au.appended {
		if a.Category == RunRevivedCategory {
			t.Errorf("run_revived audit unexpectedly recorded despite injected append failure")
		}
	}
}

// An anonymous request is 401.
func TestReviveRun_AnonymousReturns401(t *testing.T) {
	f := newReviveFixture(t)
	w := postRevive(t, f.s, f.run.ID, func(req *http.Request) *http.Request { return req })
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

// A malformed run_id is 400.
func TestReviveRun_BadUUIDReturns400(t *testing.T) {
	f := newReviveFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/not-a-uuid/revive", nil)
	req.SetPathValue("run_id", "not-a-uuid")
	w := httptest.NewRecorder()
	f.s.handleReviveRun(w, withAuth(req))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// An unconfigured server (no run/audit repo) is 503.
func TestReviveRun_UnconfiguredReturns503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	w := postRevive(t, s, uuid.New(), withAuth)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}
