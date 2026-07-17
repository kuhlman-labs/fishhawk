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
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
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

// TestServer_DefaultsNoOpIdentityProvider asserts New defaults a nil
// Config.IdentityProvider to the deny-by-default NoOp (so every existing
// server test that omits the field stays green and an OAuth-unconfigured
// backend fails closed) and preserves an explicitly injected provider.
func TestServer_DefaultsNoOpIdentityProvider(t *testing.T) {
	// Nil → NoOp default.
	s := New(Config{})
	if s.cfg.IdentityProvider == nil {
		t.Fatal("New left Config.IdentityProvider nil; want NoOp default")
	}
	if _, ok := s.cfg.IdentityProvider.(*identity.NoOpIdentityProvider); !ok {
		t.Errorf("default IdentityProvider = %T, want *identity.NoOpIdentityProvider", s.cfg.IdentityProvider)
	}

	// Explicit injection is preserved (not overwritten by the default).
	injected := identity.NewGitHubIdentityProvider("client-id", nil)
	s2 := New(Config{IdentityProvider: injected})
	if s2.cfg.IdentityProvider != injected {
		t.Errorf("New overwrote an injected IdentityProvider: got %#v, want %#v",
			s2.cfg.IdentityProvider, injected)
	}
}

// TestServer_MethodMismatch confirms the Go 1.22 ServeMux's
// method-aware routing returns 405 for the wrong verb on a registered
// path, rather than 404.
// TestServer_WiresImplementModelConfig asserts the implement-model deployment
// config (#1013) is threaded from Config onto the Server: the default rung and
// the per-adapter allowed-model policy are reachable for the prompt resolver
// and the approval gate respectively.
func TestServer_WiresImplementModelConfig(t *testing.T) {
	policy := ParseAllowedModels("claudecode=claude-opus-4-8")
	s := New(Config{
		ImplementModelDefault:  "claude-sonnet-4-6",
		ImplementAllowedModels: policy,
	})
	if s.cfg.ImplementModelDefault != "claude-sonnet-4-6" {
		t.Errorf("ImplementModelDefault = %q, want claude-sonnet-4-6", s.cfg.ImplementModelDefault)
	}
	if !s.cfg.ImplementAllowedModels.IsAllowed("claudecode", "claude-opus-4-8") {
		t.Error("allowed-model policy not threaded onto the server")
	}
	if s.cfg.ImplementAllowedModels.IsAllowed("claudecode", "gpt-5.5") {
		t.Error("threaded policy should still reject an unlisted model")
	}
}

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

// TestObserveParkedReview_ChecksPending_NoAwaitingMergeNoCIFailed pins
// the conservative checks gate from both sides: settled reviews but a
// still-running (StatePending) required check stamps reviews_settled_gate
// only — neither awaiting_merge (not green) nor ci_failed (not red), so
// an in-flight check can never trip either derived status.
func TestObserveParkedReview_ChecksPending_NoAwaitingMergeNoCIFailed(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	h.repo.seedRun(&run.Run{
		ID: h.runID, Drive: true, State: run.StateRunning,
		RequiredChecksSnapshot: &run.RequiredChecksSnapshot{Contexts: []string{"ci_pass"}},
	})
	h.scs.seed(h.stage.ID, "ci_pass", stagecheck.StatePending)

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	advances := h.driveAdvances(t)
	if len(advances) != 1 || advances[0].Rule != drive.RuleReviewsSettledGate {
		t.Fatalf("run_auto_advanced = %+v, want only reviews_settled_gate", advances)
	}
}

// TestObserveParkedReview_ChecksFailed_StampsCIFailed pins the negative
// mirror (#1045): settled reviews with a red (StateFail) required check
// stamp reviews_settled_gate + ci_failed, the ci_failed entry naming the
// failed check and carrying the classify next action. Idempotent across
// two ticks (single stamp per stage).
func TestObserveParkedReview_ChecksFailed_StampsCIFailed(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	h.repo.seedRun(&run.Run{
		ID: h.runID, Drive: true, State: run.StateRunning,
		RequiredChecksSnapshot: &run.RequiredChecksSnapshot{Contexts: []string{"ci_pass"}},
	})
	h.scs.seed(h.stage.ID, "ci_pass", stagecheck.StateFail)

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)
	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	advances := h.driveAdvances(t)
	if len(advances) != 2 {
		t.Fatalf("run_auto_advanced = %+v, want settled + ci_failed (idempotent across two ticks)", advances)
	}
	if advances[1].Rule != drive.RuleCIFailed || advances[1].To != "ci_failed" {
		t.Fatalf("second entry = %+v, want ci_failed -> ci_failed", advances[1])
	}
	if !strings.Contains(advances[1].Event, "ci_pass") {
		t.Errorf("Event = %q, want it to name the failed check ci_pass", advances[1].Event)
	}
	if advances[1].NextAction == nil || advances[1].NextAction.Action != "classify_ci_failure" || advances[1].NextAction.PRURL != driveObserverPRURL {
		t.Errorf("NextAction = %+v, want classify_ci_failure with PR URL", advances[1].NextAction)
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

// --- acceptance-aware drive gate (E31.17 / #1568) -------------------------

// seedAcceptanceObserverRun re-seeds the harness run with the acceptance
// workflow spec and (when accState is non-nil) materializes an acceptance stage
// row in the given state so ListStagesForRun surfaces it to acceptanceGateState.
func (h *driveObserverHarness) seedAcceptanceObserverRun(accState *run.StageState) {
	h.repo.seedRun(&run.Run{
		ID:           h.runID,
		Drive:        true,
		State:        run.StateRunning,
		WorkflowID:   "feature_change",
		WorkflowSpec: specWithAcceptanceStage,
	})
	if accState != nil {
		st := acceptanceStage(h.runID, *accState)
		h.repo.mu.Lock()
		h.repo.stages[st.ID] = st
		h.repo.mu.Unlock()
	}
}

func stageStatePtr(s run.StageState) *run.StageState { return &s }

// TestObserveParkedReview_AcceptancePassed_StampsAwaitingMerge pins that a
// passed acceptance verdict lets the awaiting_merge presentation status fire —
// the merge is no longer acceptance-blocked (ADR-049 decision #6).
func TestObserveParkedReview_AcceptancePassed_StampsAwaitingMerge(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	h.seedAcceptanceObserverRun(stageStatePtr(run.StageStateSucceeded))
	seedAcceptanceOutcome(h.au, h.runID, 30, acceptanceVerdictPassed)

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	advances := h.driveAdvances(t)
	if len(advances) != 2 || advances[1].Rule != drive.RuleChecksGreenAwaitingMerge {
		t.Fatalf("run_auto_advanced = %+v, want settled + checks_green_awaiting_merge on a passed acceptance", advances)
	}
	if advances[1].NextAction == nil || advances[1].NextAction.Action != "merge_pr" {
		t.Errorf("NextAction = %+v, want merge_pr", advances[1].NextAction)
	}
}

// TestObserveParkedReview_AcceptancePending_ParksNoMerge pins the pending arm:
// review evidence green but the acceptance stage has not settled → the run
// parks with await_acceptance, NEVER merge_pr.
func TestObserveParkedReview_AcceptancePending_ParksNoMerge(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	h.seedAcceptanceObserverRun(stageStatePtr(run.StageStateRunning)) // non-terminal, no verdict

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	advances := h.driveAdvances(t)
	if len(advances) != 2 || advances[1].Rule != drive.RuleAcceptancePending {
		t.Fatalf("run_auto_advanced = %+v, want settled + acceptance_pending", advances)
	}
	if advances[1].To != "acceptance_pending" || advances[1].NextAction == nil || advances[1].NextAction.Action != "await_acceptance" {
		t.Errorf("entry = %+v, want acceptance_pending / await_acceptance", advances[1])
	}
	for _, a := range advances {
		if a.Rule == drive.RuleChecksGreenAwaitingMerge {
			t.Fatal("checks_green_awaiting_merge must NOT stamp while acceptance is pending")
		}
	}
}

// TestObserveParkedReview_AcceptanceOutcomeUnknown_ParksNoMerge pins the
// settled-outcome-unknown arm: the acceptance stage is terminal but no verdict
// is recorded → park with read_acceptance_audit, never merge_pr.
func TestObserveParkedReview_AcceptanceOutcomeUnknown_ParksNoMerge(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	h.seedAcceptanceObserverRun(stageStatePtr(run.StageStateSucceeded)) // terminal, no verdict

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	advances := h.driveAdvances(t)
	if len(advances) != 2 || advances[1].Rule != drive.RuleAcceptanceOutcomeUnknown {
		t.Fatalf("run_auto_advanced = %+v, want settled + acceptance_settled_outcome_unknown", advances)
	}
	if advances[1].To != "acceptance_settled_outcome_unknown" || advances[1].NextAction == nil || advances[1].NextAction.Action != "read_acceptance_audit" {
		t.Errorf("entry = %+v, want acceptance_settled_outcome_unknown / read_acceptance_audit", advances[1])
	}
}

// TestObserveParkedReview_AcceptanceTriage_ParksNoMerge pins the failed-verdict
// arm: a failed acceptance verdict parks with read_acceptance_triage, never
// merge_pr.
func TestObserveParkedReview_AcceptanceTriage_ParksNoMerge(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	h.seedAcceptanceObserverRun(stageStatePtr(run.StageStateSucceeded))
	seedAcceptanceOutcome(h.au, h.runID, 30, acceptanceVerdictFailed)

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	advances := h.driveAdvances(t)
	if len(advances) != 2 || advances[1].Rule != drive.RuleAcceptanceTriage {
		t.Fatalf("run_auto_advanced = %+v, want settled + acceptance_triage", advances)
	}
	if advances[1].To != "acceptance_triage" || advances[1].NextAction == nil || advances[1].NextAction.Action != "read_acceptance_triage" {
		t.Errorf("entry = %+v, want acceptance_triage / read_acceptance_triage", advances[1])
	}
}

// TestObserveParkedReview_AcceptanceSkippedOutOfScope_StampsAwaitingMerge pins
// the E38.3 / #1877 arm: a terminal acceptance stage settled via the
// out-of-scope skip marker (no verdict) is a legitimate merge-eligible
// disposition, so the drive observer falls through to checks_green_awaiting_merge
// / merge_pr — NOT the acceptance_settled_outcome_unknown park.
func TestObserveParkedReview_AcceptanceSkippedOutOfScope_StampsAwaitingMerge(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	h.repo.seedRun(&run.Run{
		ID:           h.runID,
		Drive:        true,
		State:        run.StateRunning,
		WorkflowID:   "feature_change",
		WorkflowSpec: specWithAcceptanceStage,
	})
	acc := acceptanceStage(h.runID, run.StageStateSucceeded)
	h.repo.mu.Lock()
	h.repo.stages[acc.ID] = acc
	h.repo.mu.Unlock()
	seedAcceptanceSkipMarker(h.au, h.runID, acc.ID)

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	advances := h.driveAdvances(t)
	if len(advances) != 2 || advances[1].Rule != drive.RuleChecksGreenAwaitingMerge {
		t.Fatalf("run_auto_advanced = %+v, want settled + checks_green_awaiting_merge on a skip-settled acceptance", advances)
	}
	if advances[1].NextAction == nil || advances[1].NextAction.Action != "merge_pr" {
		t.Errorf("NextAction = %+v, want merge_pr", advances[1].NextAction)
	}
	for _, a := range advances {
		if a.Rule == drive.RuleAcceptanceOutcomeUnknown {
			t.Fatal("acceptance_settled_outcome_unknown must NOT stamp for a skip-settled acceptance stage")
		}
	}
}

// TestObserveParkedReview_NoAcceptanceStage_StampsAwaitingMerge is the
// regression: a workflow that declares NO acceptance stage must still reach
// checks_green_awaiting_merge (the acceptance gate is a pure off-switch there).
func TestObserveParkedReview_NoAcceptanceStage_StampsAwaitingMerge(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	h.repo.seedRun(&run.Run{
		ID:           h.runID,
		Drive:        true,
		State:        run.StateRunning,
		WorkflowID:   "feature_change",
		WorkflowSpec: specNoAcceptanceStage,
	})

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	advances := h.driveAdvances(t)
	if len(advances) != 2 || advances[1].Rule != drive.RuleChecksGreenAwaitingMerge {
		t.Fatalf("run_auto_advanced = %+v, want settled + checks_green_awaiting_merge (no acceptance stage declared)", advances)
	}
}

// TestObserveParkedReview_AcceptancePendingIdempotent pins per-stage dedup for
// the new pending rule: two ticks stamp acceptance_pending once.
func TestObserveParkedReview_AcceptancePendingIdempotent(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	h.seedAcceptanceObserverRun(stageStatePtr(run.StageStateRunning))

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)
	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	advances := h.driveAdvances(t)
	pending := 0
	for _, a := range advances {
		if a.Rule == drive.RuleAcceptancePending {
			pending++
		}
	}
	if pending != 1 {
		t.Fatalf("acceptance_pending stamps = %d, want 1 (idempotent across two ticks)", pending)
	}
}

// TestObserveParkedReview_StageListError_NoMerge pins BINDING approval
// condition 1: a ListStagesForRun error on an acceptance-declaring run must NOT
// fall through to checks_green_awaiting_merge / merge_pr — the observer skips
// advancing (only the earlier reviews_settled_gate stamp remains).
func TestObserveParkedReview_StageListError_NoMerge(t *testing.T) {
	repo := newApprovalRunRepo()
	au := newAuditFake()
	wrapped := &stageListErrRepo{approvalRunRepo: repo, err: errors.New("list stages boom")}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: wrapped, AuditRepo: au})

	stage := repo.seedStage(run.StageStateAwaitingApproval)
	repo.mu.Lock()
	stage.Type = run.StageTypeReview
	repo.mu.Unlock()
	repo.seedRun(&run.Run{
		ID: stage.RunID, Drive: true, State: run.StateRunning,
		WorkflowID: "feature_change", WorkflowSpec: specWithAcceptanceStage,
	})
	rid := stage.RunID
	payload, _ := json.Marshal(planreview.ReviewStartedPayload{ConfiguredAgents: 1})
	au.seeded = append(au.seeded,
		&audit.Entry{RunID: &rid, Sequence: 10, Category: "implement_review_started", Payload: payload},
		&audit.Entry{RunID: &rid, Sequence: 11, Category: "implement_reviewed", Payload: []byte(`{}`)},
	)

	s.ObserveParkedReviewForDrive(context.Background(), stage, driveObserverPRURL)

	for _, e := range au.appended {
		if e.Category != drive.Category {
			continue
		}
		var adv drive.Advance
		if err := json.Unmarshal(e.Payload, &adv); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if adv.Rule == drive.RuleChecksGreenAwaitingMerge {
			t.Fatal("checks_green_awaiting_merge must NOT stamp when ListStagesForRun errors (fail-closed)")
		}
		if adv.NextAction != nil && adv.NextAction.Action == "merge_pr" {
			t.Fatal("merge_pr must never be the next action when the stage read errored")
		}
	}
}

// stageListErrRepo wraps approvalRunRepo, overriding only ListStagesForRun to
// return an injected error so the acceptance-gate fail-closed branch is
// exercisable.
type stageListErrRepo struct {
	*approvalRunRepo
	err error
}

func (r *stageListErrRepo) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	return nil, r.err
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

// TestNew_WiresAnchorPlanArtifactLister pins the #1069 production wiring:
// server.New must thread cfg.ArtifactRepo into issuecomment.New so the
// living anchor (#1054) renders its plan section in the real binary.
// Without it loadAnchorPlans short-circuits on a nil lister and the anchor
// silently drops the plan despite a green e2e — the same constructor-seam
// regression class as #1060. The notifier is built when GitHub / Runs / Audit
// are non-nil (since #1787 an empty ExternalURL no longer suppresses it), so
// the Config sets all of them plus ArtifactRepo.
func TestNew_WiresAnchorPlanArtifactLister(t *testing.T) {
	s := New(Config{
		RunRepo:      newOrchestratorRepo(),
		AuditRepo:    newAuditCompleteAuditFake(),
		ArtifactRepo: newFakeArtifactRepo(),
		ExternalURL:  "https://app.fishhawk.example.com",
		GitHub:       &githubclient.Client{},
	})
	if s.issueNotifier == nil {
		t.Fatal("server.New did not construct the issue notifier with GitHub/Runs/Audit/ExternalURL set")
	}
	if !s.issueNotifier.ArtifactListerWired() {
		t.Fatal("server.New did not thread cfg.ArtifactRepo into issuecomment.New — the living anchor renders no plan in production (#1069)")
	}
}

// TestNew_WiresIssueNotifierWithEmptyExternalURL pins the #1787 cross-boundary
// wiring: with GitHub / Runs / Audit wired but ExternalURL EMPTY, server.New
// must still construct a non-nil issue notifier (the dropped issuecomment.New
// bail), so link-less comments post under the dogfood posture that leaves the
// base URL unset. Before #1787 the empty ExternalURL made issuecomment.New
// return nil and s.issueNotifier stayed a nil interface, silencing every
// comment surface.
func TestNew_WiresIssueNotifierWithEmptyExternalURL(t *testing.T) {
	s := New(Config{
		RunRepo:      newOrchestratorRepo(),
		AuditRepo:    newAuditCompleteAuditFake(),
		ArtifactRepo: newFakeArtifactRepo(),
		// ExternalURL deliberately empty.
		GitHub: &githubclient.Client{},
	})
	if s.issueNotifier == nil {
		t.Fatal("server.New must construct the issue notifier even with an empty ExternalURL (#1787)")
	}
}

// TestWebhookGitLab_FullStack_CSRFExempt drives POST /webhooks/gitlab
// through the ENTIRE middleware chain (recovery → requestID → logging →
// auth → csrf → mux), proving the GitLab receiver is both routed and
// CSRF-exempt end to end (E45.6 / #1860): a configured server returns
// 202 for a valid delivery rather than a 403 csrf_required or a 404.
func TestWebhookGitLab_FullStack_CSRFExempt(t *testing.T) {
	s := New(Config{
		GitLabWebhookSecret: []byte("gl-token"),
		WebhookDeliveries:   webhook.NewMemoryStore(0),
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	body := `{"object_kind":"issue","user":{"username":"root"},
		"project":{"id":1,"path_with_namespace":"g/p"},
		"object_attributes":{"iid":1,"action":"close"}}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/webhooks/gitlab", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Gitlab-Token", "gl-token")
	req.Header.Set("X-Gitlab-Event", "Issue Hook")
	req.Header.Set("X-Gitlab-Event-UUID", "fullstack-uuid")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /webhooks/gitlab: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 202 (routed + CSRF-exempt); body=%s", resp.StatusCode, b)
	}
}
