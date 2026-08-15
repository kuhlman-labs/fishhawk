package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// reconcileEndpointIdentity is an authenticated operator token carrying
// write:runs — what the endpoint requires.
func reconcileEndpointIdentity() Identity {
	return Identity{Subject: "github:op", TokenID: uuid.NewString(), Scopes: []string{"write:runs"}}
}

// postReconcile drives the handler directly with the given identity and
// returns the recorder.
func postReconcile(t *testing.T, s *Server, runID string, id Identity) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID+"/reviews/reconcile", nil)
	req.SetPathValue("run_id", runID)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
	rec := httptest.NewRecorder()
	s.handleReconcileRunReviews(rec, req)
	return rec
}

// decodeReconcileBody decodes the 200 body.
func decodeReconcileBody(t *testing.T, rec *httptest.ResponseRecorder) reconcileRunReviewsResponse {
	t.Helper()
	var out reconcileRunReviewsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode reconcile body %q: %v", rec.Body.String(), err)
	}
	return out
}

// stageRow returns the named stage's row from a reconcile response.
func stageRow(t *testing.T, body reconcileRunReviewsResponse, stage string) reconciledStage {
	t.Helper()
	for _, st := range body.Stages {
		if st.Stage == stage {
			return st
		}
	}
	t.Fatalf("no %q stage row in %+v", stage, body.Stages)
	return reconciledStage{}
}

// newPGReconcileServer wires a Server over REAL Postgres repositories with an
// injected boot marker, returning the server plus the repos so a test can seed
// and read back real persisted audit rows.
func newPGReconcileServer(t *testing.T, processStart time.Time) (*Server, run.Repository, audit.Repository) {
	t.Helper()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: runRepo, AuditRepo: auditRepo, ProcessStart: processStart})
	return s, runRepo, auditRepo
}

// seedPGReviewRun creates a real run + plan stage and returns both ids.
func seedPGReviewRun(t *testing.T, runRepo run.Repository) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
		RunnerKind:    run.RunnerKindLocal,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	st, err := runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID: r.ID, Sequence: 0, Type: run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("CreateStage: %v", err)
	}
	return r.ID, st.ID
}

// appendPGAudit appends one real chained audit entry.
func appendPGAudit(t *testing.T, auditRepo audit.Repository, runID uuid.UUID, stageID *uuid.UUID, ts time.Time, category string, payload any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s payload: %v", category, err)
	}
	kind := audit.ActorKind("system")
	if _, err := auditRepo.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     runID,
		StageID:   stageID,
		Timestamp: ts,
		Category:  category,
		ActorKind: &kind,
		Payload:   raw,
	}); err != nil {
		t.Fatalf("AppendChained %s: %v", category, err)
	}
}

// TestReconcileRunReviews_Endpoint_PreservesLandedVerdicts is the SERVER-side
// integration control (#2712): the on-demand endpoint runs against REAL
// persisted audit rows and sequences, not a fake, and the assertion is made by
// READING THE AUDIT BACK after the call returns — the committed state, not the
// returned envelope.
//
// The seeded round is the observed failure: 2 configured reviewers, ONE landed
// verdict (claude-fable-5 / approve_with_concerns), dispatched before the
// serving daemon booted. Exactly ONE terminal entry may be synthesized, and the
// landed verdict must survive verbatim.
func TestReconcileRunReviews_Endpoint_PreservesLandedVerdicts(t *testing.T) {
	boot := time.Now().UTC()
	s, runRepo, auditRepo := newPGReconcileServer(t, boot)
	runID, stageID := seedPGReviewRun(t, runRepo)
	dispatched := boot.Add(-10 * time.Minute)

	appendPGAudit(t, auditRepo, runID, &stageID, dispatched, "plan_review_started",
		planreview.ReviewStartedPayload{ConfiguredAgents: 2, Authority: planreview.AuthorityAdvisory})
	appendPGAudit(t, auditRepo, runID, &stageID, dispatched.Add(time.Minute), "plan_reviewed",
		map[string]any{
			"reviewer_kind": "agent", "reviewer_model": "claude-fable-5",
			"authority": "advisory", "verdict": "approve_with_concerns",
			"free_form": "one open question about the boot marker",
		})

	rec := postReconcile(t, s, runID.String(), reconcileEndpointIdentity())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	body := decodeReconcileBody(t, rec)
	if !body.Terminated {
		t.Fatalf("terminated = false, want true; body %+v", body)
	}
	planRow := stageRow(t, body, "plan")
	if planRow.ConfiguredAgents != 2 || planRow.LandedBefore != 1 || planRow.Synthesized != 1 {
		t.Fatalf("plan row = %+v, want configured 2 / landed_before 1 / synthesized 1", planRow)
	}

	// COMMITTED STATE: read the audit back.
	ctx := context.Background()
	failed, err := auditRepo.ListForRunByCategory(ctx, runID, "plan_review_failed")
	if err != nil {
		t.Fatalf("ListForRunByCategory plan_review_failed: %v", err)
	}
	if len(failed) != 1 {
		t.Fatalf("persisted plan_review_failed = %d, want exactly 1 (2 configured - 1 landed)", len(failed))
	}
	reviewed, err := auditRepo.ListForRunByCategory(ctx, runID, "plan_reviewed")
	if err != nil {
		t.Fatalf("ListForRunByCategory plan_reviewed: %v", err)
	}
	if len(reviewed) != 1 {
		t.Fatalf("persisted plan_reviewed = %d, want the original verdict preserved", len(reviewed))
	}
	var landed struct {
		ReviewerModel string `json:"reviewer_model"`
		Verdict       string `json:"verdict"`
		FreeForm      string `json:"free_form"`
	}
	if err := json.Unmarshal(reviewed[0].Payload, &landed); err != nil {
		t.Fatalf("decode preserved verdict: %v", err)
	}
	if landed.ReviewerModel != "claude-fable-5" || landed.Verdict != "approve_with_concerns" ||
		landed.FreeForm != "one open question about the boot marker" {
		t.Fatalf("landed verdict = %+v, want the original claude-fable-5 approve_with_concerns row verbatim", landed)
	}

	// Idempotent: a second call synthesizes nothing more.
	rec2 := postReconcile(t, s, runID.String(), reconcileEndpointIdentity())
	if rec2.Code != http.StatusOK {
		t.Fatalf("second call status = %d, want 200", rec2.Code)
	}
	body2 := decodeReconcileBody(t, rec2)
	if body2.Terminated {
		t.Errorf("second call terminated = true, want false (idempotent no-op)")
	}
	if got := stageRow(t, body2, "plan"); got.SkipReason != reconcileSkipAlreadySettled {
		t.Errorf("second call plan skip_reason = %q, want %q", got.SkipReason, reconcileSkipAlreadySettled)
	}
	failedAfter, err := auditRepo.ListForRunByCategory(ctx, runID, "plan_review_failed")
	if err != nil {
		t.Fatalf("ListForRunByCategory after second call: %v", err)
	}
	if len(failedAfter) != 1 {
		t.Fatalf("persisted plan_review_failed after a second call = %d, want still 1", len(failedAfter))
	}
}

// TestReconcileRunReviews_Endpoint_ConcurrentCalls_SynthesizeOnlyMissing is the
// CONCURRENCY control for the count-and-append critical section: the recovery's
// idempotency rests on a read-then-append sequence (count this round's landed
// terminals, append exactly ConfiguredAgents-landed), which is not atomic on its
// own. Four simultaneous POSTs against the SAME orphaned round must persist
// exactly the missing count — not one shortfall per caller.
//
// The bad state is seeded BY CONSTRUCTION (a pre-boot started entry with 2
// configured reviewers and no landed verdict) and the assertion is made by
// READING THE AUDIT BACK over a real pgtest database, so deleting the stripe
// lock in reconcileRunOrphanedReviewsLocked lands RED on committed state.
func TestReconcileRunReviews_Endpoint_ConcurrentCalls_SynthesizeOnlyMissing(t *testing.T) {
	boot := time.Now().UTC()
	s, runRepo, auditRepo := newPGReconcileServer(t, boot)
	runID, stageID := seedPGReviewRun(t, runRepo)

	const configured = 2
	appendPGAudit(t, auditRepo, runID, &stageID, boot.Add(-10*time.Minute), "plan_review_started",
		planreview.ReviewStartedPayload{ConfiguredAgents: configured, Authority: planreview.AuthorityAdvisory})

	// Fire the callers from a common start gate so they contend on the same
	// round rather than serializing by launch order.
	const callers = 4
	start := make(chan struct{})
	recs := make([]*httptest.ResponseRecorder, callers)
	var wg sync.WaitGroup
	for i := range recs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			recs[i] = postReconcile(t, s, runID.String(), reconcileEndpointIdentity())
		}(i)
	}
	close(start)
	wg.Wait()

	terminated := 0
	for i, rec := range recs {
		if rec.Code != http.StatusOK {
			t.Fatalf("caller %d status = %d, want 200; body %s", i, rec.Code, rec.Body.String())
		}
		if decodeReconcileBody(t, rec).Terminated {
			terminated++
		}
	}

	// COMMITTED STATE: exactly the missing count was persisted, however the
	// callers interleaved.
	failed, err := auditRepo.ListForRunByCategory(context.Background(), runID, "plan_review_failed")
	if err != nil {
		t.Fatalf("ListForRunByCategory plan_review_failed: %v", err)
	}
	if len(failed) != configured {
		t.Fatalf("persisted plan_review_failed = %d, want exactly %d — %d concurrent callers each synthesized the same shortfall",
			len(failed), configured, callers)
	}
	// Under serialization exactly one caller can observe the shortfall; every
	// later one re-counts after the append and takes the settled skip.
	if terminated != 1 {
		t.Errorf("callers reporting terminated = %d, want exactly 1", terminated)
	}
}

// TestReconcileRunReviews_Endpoint_RefusesLiveRound is the boot-marker gate's
// counterfactual vehicle (control (a)): a round dispatched AFTER the serving
// daemon booted belongs to a LIVE reviewer, and the on-demand endpoint — which
// deliberately applies no run-state filter — must not terminate it.
//
// The bad state is seeded BY CONSTRUCTION: the test stamps the started entry's
// timestamp itself (after the injected Config.ProcessStart) rather than calling
// the gate to produce it, so deleting the gate lands RED on the behavioral
// assertion below, not on fixture setup.
func TestReconcileRunReviews_Endpoint_RefusesLiveRound(t *testing.T) {
	boot := time.Now().UTC().Add(-time.Hour)
	s, runRepo, auditRepo := newPGReconcileServer(t, boot)
	runID, stageID := seedPGReviewRun(t, runRepo)

	// Dispatched AFTER the boot marker: this process's own live review.
	appendPGAudit(t, auditRepo, runID, &stageID, boot.Add(time.Minute), "plan_review_started",
		planreview.ReviewStartedPayload{ConfiguredAgents: 2, Authority: planreview.AuthorityAdvisory})

	rec := postReconcile(t, s, runID.String(), reconcileEndpointIdentity())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	body := decodeReconcileBody(t, rec)
	if body.Terminated {
		t.Errorf("terminated = true, want false — a review THIS process dispatched is live, not orphaned")
	}
	if got := stageRow(t, body, "plan"); got.SkipReason != reconcileSkipInFlight {
		t.Errorf("plan skip_reason = %q, want %q", got.SkipReason, reconcileSkipInFlight)
	}

	// COMMITTED STATE: no terminal entry may have been written over the live
	// reviewer. This is the assertion the gate's deletion must redden.
	failed, err := auditRepo.ListForRunByCategory(context.Background(), runID, "plan_review_failed")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("persisted plan_review_failed = %d, want 0 — the endpoint terminated a LIVE review", len(failed))
	}
}

// TestReconcileRunReviews_Endpoint_UnknownRun404 covers the unknown-run
// refusal: reconciling an id that names nothing must be the operator's typo,
// not a vacuous "nothing to heal".
func TestReconcileRunReviews_Endpoint_UnknownRun404(t *testing.T) {
	s, _, _, _ := newReconcileServer(t)
	rec := postReconcile(t, s, uuid.NewString(), reconcileEndpointIdentity())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "run_not_found") {
		t.Errorf("body %s, want the run_not_found envelope", rec.Body.String())
	}
}

// TestReconcileRunReviews_Endpoint_SkipReasons pins each named skip branch on
// the wire: a 200 with terminated=false and the matching skip_reason.
func TestReconcileRunReviews_Endpoint_SkipReasons(t *testing.T) {
	cases := []struct {
		name       string
		seed       func(t *testing.T, au *auditFake, runID, stageID uuid.UUID)
		wantReason string
	}{
		{
			name:       "no started entry",
			seed:       func(*testing.T, *auditFake, uuid.UUID, uuid.UUID) {},
			wantReason: reconcileSkipNoStartedEntry,
		},
		{
			name: "zero configured agents",
			seed: func(t *testing.T, au *auditFake, runID, stageID uuid.UUID) {
				seedReviewAuditEntry(t, au, runID, stageID, 1, beforeBoot, "plan_review_started",
					planreview.ReviewStartedPayload{ConfiguredAgents: 0})
			},
			wantReason: reconcileSkipNoConfigured,
		},
		{
			name: "already settled",
			seed: func(t *testing.T, au *auditFake, runID, stageID uuid.UUID) {
				seedReviewAuditEntry(t, au, runID, stageID, 1, beforeBoot, "plan_review_started",
					planreview.ReviewStartedPayload{ConfiguredAgents: 1})
				seedReviewAuditEntry(t, au, runID, stageID, 2, beforeBoot, "plan_reviewed",
					map[string]any{"verdict": "approve"})
			},
			wantReason: reconcileSkipAlreadySettled,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, repo, au, _ := newReconcileServer(t)
			runID := seedRunningRun(t, repo)
			tc.seed(t, au, runID, uuid.New())

			rec := postReconcile(t, s, runID.String(), reconcileEndpointIdentity())
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
			}
			body := decodeReconcileBody(t, rec)
			if body.Terminated {
				t.Errorf("terminated = true, want false")
			}
			if got := stageRow(t, body, "plan"); got.SkipReason != tc.wantReason {
				t.Errorf("plan skip_reason = %q, want %q", got.SkipReason, tc.wantReason)
			}
		})
	}
}

// TestReconcileRunReviews_Endpoint_NilStageIDSkip covers the fourth gate on
// the wire: a started entry with no stage anchor.
func TestReconcileRunReviews_Endpoint_NilStageIDSkip(t *testing.T) {
	s, repo, au, _ := newReconcileServer(t)
	runID := seedRunningRun(t, repo)
	payload, err := json.Marshal(planreview.ReviewStartedPayload{ConfiguredAgents: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rid := runID
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &rid, Sequence: 1, Timestamp: beforeBoot,
		Category: "plan_review_started", Payload: payload,
	})

	rec := postReconcile(t, s, runID.String(), reconcileEndpointIdentity())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if got := stageRow(t, decodeReconcileBody(t, rec), "plan"); got.SkipReason != reconcileSkipNoStageID {
		t.Errorf("plan skip_reason = %q, want %q", got.SkipReason, reconcileSkipNoStageID)
	}
}

// TestReconcileRunReviews_Endpoint_AuthRefusals covers the two auth guards and
// the bad-UUID guard.
func TestReconcileRunReviews_Endpoint_AuthRefusals(t *testing.T) {
	s, repo, _, _ := newReconcileServer(t)
	runID := seedRunningRun(t, repo)

	t.Run("anonymous 401", func(t *testing.T) {
		rec := postReconcile(t, s, runID.String(), Identity{Subject: "anonymous"})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "authentication_required") {
			t.Errorf("body %s, want authentication_required", rec.Body.String())
		}
	})
	t.Run("token without write:runs 403", func(t *testing.T) {
		rec := postReconcile(t, s, runID.String(),
			Identity{Subject: "github:op", TokenID: uuid.NewString(), Scopes: []string{"read:runs"}})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "insufficient_scope") {
			t.Errorf("body %s, want insufficient_scope", rec.Body.String())
		}
	})
	t.Run("bad uuid 400", func(t *testing.T) {
		rec := postReconcile(t, s, "not-a-uuid", reconcileEndpointIdentity())
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
		}
	})
}

// TestReconcileRunReviews_Endpoint_Unconfigured covers the wiring guard: with
// no run/audit repo the endpoint answers 503 rather than panicking.
func TestReconcileRunReviews_Endpoint_Unconfigured(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", ProcessStart: bootMarker})
	rec := postReconcile(t, s, uuid.NewString(), reconcileEndpointIdentity())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "review_reconcile_unconfigured") {
		t.Errorf("body %s, want review_reconcile_unconfigured", rec.Body.String())
	}
}

// TestReconcileRunReviews_Endpoint_ReconcileErrorIs500 covers the per-run
// failure branch: an audit read error surfaces as a 500 envelope with the
// cause withheld from the client.
func TestReconcileRunReviews_Endpoint_ReconcileErrorIs500(t *testing.T) {
	s, repo, au, _ := newReconcileServer(t)
	runID := seedRunningRun(t, repo)
	au.listByCategoryErr = context.DeadlineExceeded

	rec := postReconcile(t, s, runID.String(), reconcileEndpointIdentity())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "review_reconcile_failed") {
		t.Errorf("body %s, want review_reconcile_failed", rec.Body.String())
	}
}
