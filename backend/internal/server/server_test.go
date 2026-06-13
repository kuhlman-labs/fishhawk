package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

// TestServer_FullStack drives a request through the entire middleware
// chain (recovery → requestID → logging → authStub → mux) by spinning
// up an httptest.Server with the real Server.Handler() output.
func TestServer_FullStack(t *testing.T) {
	s := New(Config{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Request-ID"); got == "" {
		t.Error("X-Request-ID header missing — requestID middleware did not run")
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json...", got)
	}

	var body healthResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
	if body.Version == "" {
		t.Error("version must not be empty")
	}
}

// TestServer_MethodMismatch confirms the Go 1.22 ServeMux's
// method-aware routing returns 405 for the wrong verb on a registered
// path, rather than 404.
func TestServer_MethodMismatch(t *testing.T) {
	s := New(Config{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/healthz", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

// TestServer_UnknownPath confirms unregistered paths return 404.
func TestServer_UnknownPath(t *testing.T) {
	s := New(Config{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/does-not-exist")
	if err != nil {
		t.Fatalf("GET unknown: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestServer_ShutdownIsClean asserts that Shutdown returns nil when
// the server has not been started and that the timeout from Config is
// honored.
func TestServer_ShutdownWithoutStart(t *testing.T) {
	s := New(Config{ShutdownTimeout: 100 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown without Start returned %v, want nil", err)
	}
}

// --- ObserveParkedReviewForDrive (#1023) ---------------------------------

// driveObserverHarness wires the run/audit/stage-check fakes the
// mergereconciler-invoked drive observer reads.
type driveObserverHarness struct {
	s     *Server
	repo  *approvalRunRepo
	au    *auditFake
	scs   *fakeStageCheckRepo
	stage *run.Stage
	runID uuid.UUID
}

func newDriveObserverHarness(t *testing.T, driveOn bool) *driveObserverHarness {
	t.Helper()
	repo := newApprovalRunRepo()
	au := newAuditFake()
	scs := newFakeStageCheckRepo()
	s := New(Config{
		Addr:           "127.0.0.1:0",
		RunRepo:        repo,
		AuditRepo:      au,
		StageCheckRepo: scs,
	})
	stage := repo.seedStage(run.StageStateAwaitingApproval)
	repo.mu.Lock()
	stage.Type = run.StageTypeReview
	repo.mu.Unlock()
	repo.seedRun(&run.Run{ID: stage.RunID, Drive: driveOn, State: run.StateRunning})
	return &driveObserverHarness{s: s, repo: repo, au: au, scs: scs, stage: stage, runID: stage.RunID}
}

// seedImplementReviewRound seeds an implement_review_started entry with
// the given configured-agent count plus n terminal implement_reviewed
// entries sequenced after it.
func (h *driveObserverHarness) seedImplementReviewRound(t *testing.T, configured, terminal int, baseSeq int64) {
	t.Helper()
	payload, _ := json.Marshal(planreview.ReviewStartedPayload{ConfiguredAgents: configured})
	rid := h.runID
	h.au.seeded = append(h.au.seeded, &audit.Entry{
		RunID: &rid, Sequence: baseSeq, Category: "implement_review_started", Payload: payload,
	})
	for i := 0; i < terminal; i++ {
		h.au.seeded = append(h.au.seeded, &audit.Entry{
			RunID: &rid, Sequence: baseSeq + 1 + int64(i), Category: "implement_reviewed", Payload: []byte(`{}`),
		})
	}
}

// driveAdvances decodes appended run_auto_advanced payloads.
func (h *driveObserverHarness) driveAdvances(t *testing.T) []drive.Advance {
	t.Helper()
	var out []drive.Advance
	for _, e := range h.au.appended {
		if e.Category != drive.Category {
			continue
		}
		var adv drive.Advance
		if err := json.Unmarshal(e.Payload, &adv); err != nil {
			t.Fatalf("run_auto_advanced payload unmarshal: %v", err)
		}
		out = append(out, adv)
	}
	return out
}

const driveObserverPRURL = "https://github.com/x/y/pull/42"

// TestObserveParkedReview_Settled2of2_StampsBothRules pins the
// heterogeneous dual-review shape (2-of-2 verdicts, live since
// 2026-06-09): once both implement reviews are terminal and no
// required checks are declared, ONE tick stamps reviews_settled_gate
// AND the derived checks_green_awaiting_merge.
func TestObserveParkedReview_Settled2of2_StampsBothRules(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 2, 2, 10)

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	advances := h.driveAdvances(t)
	if len(advances) != 2 {
		t.Fatalf("run_auto_advanced entries = %d, want 2 (%+v)", len(advances), advances)
	}
	if advances[0].Rule != drive.RuleReviewsSettledGate {
		t.Errorf("first rule = %q, want reviews_settled_gate", advances[0].Rule)
	}
	if advances[1].Rule != drive.RuleChecksGreenAwaitingMerge {
		t.Errorf("second rule = %q, want checks_green_awaiting_merge", advances[1].Rule)
	}
	if advances[1].To != "awaiting_merge" {
		t.Errorf("To = %q, want awaiting_merge", advances[1].To)
	}
	if advances[1].NextAction == nil || advances[1].NextAction.Action != "merge_pr" || advances[1].NextAction.PRURL != driveObserverPRURL {
		t.Errorf("NextAction = %+v, want merge_pr with PR URL", advances[1].NextAction)
	}
}

// TestObserveParkedReview_OneOfTwoVerdicts_NoStamp pins in-flight
// detection: a 2-of-2 round with one verdict landed stamps nothing —
// a reject from one reviewer never auto-resolves the gate either (the
// gate itself stays a judgment point; only settlement is detected).
func TestObserveParkedReview_OneOfTwoVerdicts_NoStamp(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 2, 1, 10)

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	if advances := h.driveAdvances(t); len(advances) != 0 {
		t.Errorf("run_auto_advanced entries = %+v, want none while a review is in flight", advances)
	}
}

// TestObserveParkedReview_FreshRoundAfterRepark_NotSettledByOldRound
// pins round delimiting: a settled FIRST round followed by a fix-up
// re-park's fresh started entry must not satisfy the gate.
func TestObserveParkedReview_FreshRoundAfterRepark_NotSettledByOldRound(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10) // first round settled
	h.seedImplementReviewRound(t, 1, 0, 20) // re-review round in flight

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	if advances := h.driveAdvances(t); len(advances) != 0 {
		t.Errorf("run_auto_advanced entries = %+v, want none: the re-review round is in flight", advances)
	}
}

// TestObserveParkedReview_ChecksNotGreen_NoAwaitingMerge pins the
// conservative checks gate: settled reviews but a non-pass required
// check stamps reviews_settled_gate only.
func TestObserveParkedReview_ChecksNotGreen_NoAwaitingMerge(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	h.repo.seedRun(&run.Run{
		ID: h.runID, Drive: true, State: run.StateRunning,
		RequiredChecksSnapshot: &run.RequiredChecksSnapshot{Contexts: []string{"ci_pass"}},
	})
	h.scs.seed(h.stage.ID, "ci_pass", stagecheck.StateFail)

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	advances := h.driveAdvances(t)
	if len(advances) != 1 || advances[0].Rule != drive.RuleReviewsSettledGate {
		t.Fatalf("run_auto_advanced = %+v, want only reviews_settled_gate", advances)
	}
}

// TestObserveParkedReview_ChecksGreen_StampsAwaitingMerge pins the
// full path with a declared required check that has passed.
func TestObserveParkedReview_ChecksGreen_StampsAwaitingMerge(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	h.repo.seedRun(&run.Run{
		ID: h.runID, Drive: true, State: run.StateRunning,
		RequiredChecksSnapshot: &run.RequiredChecksSnapshot{Contexts: []string{"ci_pass"}},
	})
	h.scs.seed(h.stage.ID, "ci_pass", stagecheck.StatePass)

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	advances := h.driveAdvances(t)
	if len(advances) != 2 || advances[1].Rule != drive.RuleChecksGreenAwaitingMerge {
		t.Fatalf("run_auto_advanced = %+v, want settled + awaiting_merge", advances)
	}
}

// TestObserveParkedReview_Idempotent pins the per-stage dedup: a
// second tick over an already-stamped stage appends nothing new.
func TestObserveParkedReview_Idempotent(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)
	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	if advances := h.driveAdvances(t); len(advances) != 2 {
		t.Errorf("run_auto_advanced entries = %d, want 2 after two ticks (dedup)", len(advances))
	}
}

// TestObserveParkedReview_NonDriveRun_NoOps is the control: the same
// settled evidence on a drive:false run stamps nothing.
func TestObserveParkedReview_NonDriveRun_NoOps(t *testing.T) {
	h := newDriveObserverHarness(t, false)
	h.seedImplementReviewRound(t, 1, 1, 10)

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	if advances := h.driveAdvances(t); len(advances) != 0 {
		t.Errorf("run_auto_advanced entries = %+v, want none on a non-drive run", advances)
	}
}

// TestObserveParkedReview_NoReviewDispatched_VacuouslyComplete pins
// the zero-configured-reviewers posture (#1060's must-still-advance
// direction): with no implement reviewers configured and no
// implement_review_started entry, the review evidence is vacuously
// complete — awaiting_merge stamps without a reviews_settled_gate entry
// (nothing settled). A reviewer-less run must never wedge at the gate.
func TestObserveParkedReview_NoReviewDispatched_VacuouslyComplete(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	// Spec with zero implement reviewers: configured==0, so a
	// never-dispatched round is vacuously terminal and may advance.
	h.repo.seedRun(&run.Run{
		ID:           h.runID,
		Drive:        true,
		State:        run.StateRunning,
		WorkflowID:   "feature_change",
		WorkflowSpec: specImplementReviewers(0),
	})

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	advances := h.driveAdvances(t)
	if len(advances) != 1 || advances[0].Rule != drive.RuleChecksGreenAwaitingMerge {
		t.Fatalf("run_auto_advanced = %+v, want only checks_green_awaiting_merge", advances)
	}
}

// TestObserveParkedReview_ReviewersConfiguredButUndispatched_Parks pins
// the #1060 drive safety fix: a run whose spec configures implement
// reviewers but whose review round was never dispatched (no
// implement_review_started entry) is NON-terminal evidence, not
// vacuously terminal — the decomposed-parent consolidated-review case
// where the gating review runs against the parent's consolidated diff.
// checks_green_awaiting_merge must NOT stamp even with no required
// checks (vacuously green), or a child-raised high never gates the
// parent merge.
func TestObserveParkedReview_ReviewersConfiguredButUndispatched_Parks(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	// Re-seed the run with a spec that configures one implement
	// reviewer; no implement_review_started entry is seeded, so the
	// round is configured-but-undispatched.
	h.repo.seedRun(&run.Run{
		ID:           h.runID,
		Drive:        true,
		State:        run.StateRunning,
		WorkflowID:   "feature_change",
		WorkflowSpec: specImplementReviewers(1),
	})

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	if advances := h.driveAdvances(t); len(advances) != 0 {
		t.Fatalf("run_auto_advanced = %+v, want none: reviewers are configured but no round was dispatched", advances)
	}
}

// TestObserveParkedReview_AuditReadError_SkipsQuietly pins the
// poll-friendly failure posture: a category read error stamps nothing
// and does not panic (the next tick retries).
func TestObserveParkedReview_AuditReadError_SkipsQuietly(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.au.listByCategoryErr = errors.New("db down")

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	if len(h.au.appended) != 0 {
		t.Errorf("appended = %+v, want none on a read error", h.au.appended)
	}
}

// TestNew_WiresConsolidatedReviewDispatcher pins the #1060 production
// wiring: server.New must set cfg.Orchestrator.ConsolidatedReview to the
// constructed Server so the parent consolidated implement review actually
// dispatches in the real binary (the e2e wires it manually; this guards
// the serve.go → server.New back-reference both reviewers flagged as the
// dropped-out-of-scope gap).
func TestNew_WiresConsolidatedReviewDispatcher(t *testing.T) {
	orch := &orchestrator.Orchestrator{}
	s := New(Config{Orchestrator: orch})
	if orch.ConsolidatedReview == nil {
		t.Fatal("server.New did not wire cfg.Orchestrator.ConsolidatedReview — consolidated review is inert in production")
	}
	if orch.ConsolidatedReview != s {
		t.Fatal("cfg.Orchestrator.ConsolidatedReview is not the constructed Server")
	}
}
