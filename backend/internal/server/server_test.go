package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
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
	cr    *fakeConcernRepo
	stage *run.Stage
	runID uuid.UUID
}

func newDriveObserverHarness(t *testing.T, driveOn bool) *driveObserverHarness {
	t.Helper()
	repo := newApprovalRunRepo()
	au := newAuditFake()
	scs := newFakeStageCheckRepo()
	cr := newFakeConcernRepo()
	s := New(Config{
		Addr:           "127.0.0.1:0",
		RunRepo:        repo,
		AuditRepo:      au,
		StageCheckRepo: scs,
		// A wired ConcernRepo with zero concerns keeps every existing
		// drive-observer case on the clean advisory branch (#2487) — its
		// exact-string assertions stay green because the clean branch
		// returns today's phrasing byte-for-byte.
		ConcernRepo: cr,
	})
	stage := repo.seedStage(run.StageStateAwaitingApproval)
	repo.mu.Lock()
	stage.Type = run.StageTypeReview
	repo.mu.Unlock()
	repo.seedRun(&run.Run{ID: stage.RunID, Drive: driveOn, State: run.StateRunning})
	return &driveObserverHarness{s: s, repo: repo, au: au, scs: scs, cr: cr, stage: stage, runID: stage.RunID}
}

// seedImplementReviewRound seeds an implement_review_started entry with
// the given configured-agent count plus n terminal implement_reviewed
// entries (empty payloads — no reject) sequenced after it.
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

// seedImplementReviewRoundWithRejects seeds an implement_review_started
// entry plus `terminal` implement_reviewed entries sequenced after it, of
// which the first `rejects` carry a VerdictReject payload and the rest a
// clean approve payload (#2487). Rejects are seeded BY CONSTRUCTION so the
// derivation under test — not the test's own setup — decides the count.
func (h *driveObserverHarness) seedImplementReviewRoundWithRejects(t *testing.T, configured, terminal, rejects int, baseSeq int64) {
	t.Helper()
	payload, _ := json.Marshal(planreview.ReviewStartedPayload{ConfiguredAgents: configured})
	rid := h.runID
	h.au.seeded = append(h.au.seeded, &audit.Entry{
		RunID: &rid, Sequence: baseSeq, Category: "implement_review_started", Payload: payload,
	})
	for i := 0; i < terminal; i++ {
		verdict := planreview.VerdictApprove
		if i < rejects {
			verdict = planreview.VerdictReject
		}
		vp, _ := json.Marshal(planreview.ImplementReviewedPayload{Verdict: verdict})
		h.au.seeded = append(h.au.seeded, &audit.Entry{
			RunID: &rid, Sequence: baseSeq + 1 + int64(i), Category: "implement_reviewed", Payload: vp,
		})
	}
}

// seedImplementReviewRoundMalformed seeds an implement_review_started
// entry plus one implement_reviewed entry carrying a payload that fails
// to decode as ImplementReviewedPayload (#2487, binding condition 1) — the
// truncated-review-returned-success failure mode. The round is otherwise
// terminal (one verdict landed), so the gate would reach awaiting_merge;
// the malformed payload must force the read-first hedge.
func (h *driveObserverHarness) seedImplementReviewRoundMalformed(t *testing.T, configured int, baseSeq int64) {
	t.Helper()
	payload, _ := json.Marshal(planreview.ReviewStartedPayload{ConfiguredAgents: configured})
	rid := h.runID
	h.au.seeded = append(h.au.seeded,
		&audit.Entry{RunID: &rid, Sequence: baseSeq, Category: "implement_review_started", Payload: payload},
		// A JSON value that is not an object cannot decode into the
		// struct — the truncated/garbled payload shape.
		&audit.Entry{RunID: &rid, Sequence: baseSeq + 1, Category: "implement_reviewed", Payload: []byte(`"truncated`)},
	)
}

// seedOpenConcerns seeds n open (raised) concerns on the fake ConcernRepo
// for the harness run, so ListOpenByRun reports them run-wide (#2487).
func (h *driveObserverHarness) seedOpenConcerns(n int) {
	h.cr.mu.Lock()
	defer h.cr.mu.Unlock()
	for i := 0; i < n; i++ {
		h.cr.rows = append(h.cr.rows, &concern.Concern{
			ID: uuid.New(), RunID: h.runID, State: concern.StateRaised,
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

// --- Advisory-qualified awaiting_merge detail (#2487) ----------------------

// Legacy clean strings kept byte-for-byte by the clean advisory branch.
const (
	cleanMergeDetailResolved   = "all gates resolved and required checks are green; review and merge the PR"
	cleanMergeDetailUnresolved = "all gates resolved; required checks were not resolved for this run (no protection snapshot) — verify CI on the PR before merging"
)

// awaitingMergeDetail runs the observer, asserts the no-behaviour-change
// invariant (Rule/To/Action unchanged — the merge gate keys on the rule,
// not the prose), and returns the stamped merge detail.
func (h *driveObserverHarness) awaitingMergeDetail(t *testing.T) string {
	t.Helper()
	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)
	advances := h.driveAdvances(t)
	if len(advances) != 2 || advances[1].Rule != drive.RuleChecksGreenAwaitingMerge {
		t.Fatalf("run_auto_advanced = %+v, want settled + checks_green_awaiting_merge", advances)
	}
	adv := advances[1]
	if adv.To != "awaiting_merge" {
		t.Errorf("To = %q, want awaiting_merge (prose-only change; state unchanged)", adv.To)
	}
	if adv.NextAction == nil || adv.NextAction.Action != "merge_pr" {
		t.Fatalf("NextAction = %+v, want merge_pr (prose-only change; action unchanged)", adv.NextAction)
	}
	return adv.NextAction.Detail
}

// seedResolvedZeroChecks makes reviewChecksResolution report (green,
// resolved) = (true, true): a present snapshot declaring zero required
// checks. Used by the checks-resolved branch cases.
func (h *driveObserverHarness) seedResolvedZeroChecks() {
	h.repo.seedRun(&run.Run{
		ID: h.runID, Drive: true, State: run.StateRunning,
		RequiredChecksSnapshot: &run.RequiredChecksSnapshot{Contexts: []string{}},
	})
}

// TestObserveParkedReview_CleanResolved_KeepsLegacyDetail pins branch (1):
// zero rejects, zero concerns, checks resolved → today's clean string
// byte-for-byte (asserted by EQUALITY, not substring).
func TestObserveParkedReview_CleanResolved_KeepsLegacyDetail(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRoundWithRejects(t, 1, 1, 0, 10)
	h.seedResolvedZeroChecks()

	if got := h.awaitingMergeDetail(t); got != cleanMergeDetailResolved {
		t.Errorf("detail = %q, want the legacy resolved string byte-for-byte", got)
	}
}

// TestObserveParkedReview_CleanUnresolved_KeepsLegacyDetail pins branch (2):
// zero rejects, zero concerns, checks unresolved (nil snapshot) → today's
// legacy unresolved string by EQUALITY.
func TestObserveParkedReview_CleanUnresolved_KeepsLegacyDetail(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRoundWithRejects(t, 1, 1, 0, 10)

	if got := h.awaitingMergeDetail(t); got != cleanMergeDetailUnresolved {
		t.Errorf("detail = %q, want the legacy unresolved string byte-for-byte", got)
	}
}

// TestObserveParkedReview_OneReject_QualifiesDetail pins branch (3): one
// round-scoped reject, zero concerns → the SINGULAR "1 advisory reject",
// never "all gates resolved", the "all blocking gates resolved" lead, and
// the fix-up / waive dispositions.
func TestObserveParkedReview_OneReject_QualifiesDetail(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRoundWithRejects(t, 1, 1, 1, 10)

	got := h.awaitingMergeDetail(t)
	if !strings.Contains(got, "1 advisory reject") {
		t.Errorf("detail = %q, want it to state the singular '1 advisory reject'", got)
	}
	if strings.Contains(got, "advisory rejects") {
		t.Errorf("detail = %q, want the SINGULAR reject, not the plural", got)
	}
	if strings.Contains(got, "all gates resolved") {
		t.Errorf("detail = %q, must NOT read 'all gates resolved' with a reject outstanding", got)
	}
	if !strings.Contains(got, "all blocking gates resolved") {
		t.Errorf("detail = %q, want the 'all blocking gates resolved' lead", got)
	}
	if !strings.Contains(got, "fishhawk_fixup_stage") || !strings.Contains(got, "fishhawk_waive_concern") {
		t.Errorf("detail = %q, want it to name fishhawk_fixup_stage and fishhawk_waive_concern", got)
	}
}

// TestObserveParkedReview_TwoConcerns_QualifiesDetail pins branch (4):
// zero rejects, two run-wide open concerns → "2 unresolved concerns" and
// never "all gates resolved".
func TestObserveParkedReview_TwoConcerns_QualifiesDetail(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRoundWithRejects(t, 1, 1, 0, 10)
	h.seedOpenConcerns(2)

	got := h.awaitingMergeDetail(t)
	if !strings.Contains(got, "2 unresolved concerns") {
		t.Errorf("detail = %q, want it to state '2 unresolved concerns'", got)
	}
	if strings.Contains(got, "all gates resolved") {
		t.Errorf("detail = %q, must NOT read 'all gates resolved' with concerns outstanding", got)
	}
}

// TestObserveParkedReview_RejectsAndConcerns_QualifiesBoth pins branch (5):
// two rejects + three concerns → BOTH counts present, each with its own
// scope, and never "all gates resolved".
func TestObserveParkedReview_RejectsAndConcerns_QualifiesBoth(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRoundWithRejects(t, 2, 2, 2, 10)
	h.seedOpenConcerns(3)

	got := h.awaitingMergeDetail(t)
	if !strings.Contains(got, "2 advisory rejects") {
		t.Errorf("detail = %q, want '2 advisory rejects'", got)
	}
	if !strings.Contains(got, "3 unresolved concerns") {
		t.Errorf("detail = %q, want '3 unresolved concerns'", got)
	}
	if strings.Contains(got, "all gates resolved") {
		t.Errorf("detail = %q, must NOT read 'all gates resolved'", got)
	}
}

// TestObserveParkedReview_NilConcernRepo_HedgesTowardRead pins branch (6):
// a nil ConcernRepo cannot read the advisory state → the read-first hedge.
// Asserts POSITIVELY (binding condition 3): NOT "all gates resolved" AND
// names the gate-view read.
func TestObserveParkedReview_NilConcernRepo_HedgesTowardRead(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRoundWithRejects(t, 1, 1, 0, 10)
	h.s.cfg.ConcernRepo = nil

	got := h.awaitingMergeDetail(t)
	if strings.Contains(got, "all gates resolved") {
		t.Errorf("detail = %q, must NOT emit the clean all-clear when the advisory state is unreadable", got)
	}
	if !strings.Contains(got, "fishhawk_get_gate_view") {
		t.Errorf("detail = %q, want the read-first hedge to name fishhawk_get_gate_view", got)
	}
}

// TestObserveParkedReview_ConcernRepoError_HedgesTowardRead pins branch (7):
// a ListOpenByRun error takes the SAME hedge as the nil repo — the two
// Known=false modes are tested separately, not collapsed.
func TestObserveParkedReview_ConcernRepoError_HedgesTowardRead(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRoundWithRejects(t, 1, 1, 0, 10)
	h.cr.listErr = errors.New("boom")

	got := h.awaitingMergeDetail(t)
	if strings.Contains(got, "all gates resolved") {
		t.Errorf("detail = %q, must NOT emit the clean all-clear when ListOpenByRun errors", got)
	}
	if !strings.Contains(got, "fishhawk_get_gate_view") {
		t.Errorf("detail = %q, want the read-first hedge to name fishhawk_get_gate_view", got)
	}
}

// TestObserveParkedReview_MalformedVerdict_HedgesTowardRead pins branch (1')
// (binding condition 1): an UNDECODABLE implement_reviewed payload must NOT
// contribute zero rejects and fall through to the clean all-clear — the
// precise defect this issue exists to prevent, arriving through the read
// path. It forces the SAME read-first hedge the nil/erroring ConcernRepo
// takes, even though the ConcernRepo here is wired and clean.
func TestObserveParkedReview_MalformedVerdict_HedgesTowardRead(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRoundMalformed(t, 1, 10)

	got := h.awaitingMergeDetail(t)
	if strings.Contains(got, "all gates resolved") {
		t.Errorf("detail = %q, must NOT emit the clean all-clear when a verdict payload is unreadable", got)
	}
	if !strings.Contains(got, "fishhawk_get_gate_view") {
		t.Errorf("detail = %q, want the read-first hedge to name fishhawk_get_gate_view", got)
	}
}

// TestObserveParkedReview_SupersededReject_NotCounted pins branch (8):
// round-scoping. A reject in a settled EARLIER round followed by a clean
// newer round must yield the CLEAN string — a superseded reject can never
// re-qualify a resolved run.
func TestObserveParkedReview_SupersededReject_NotCounted(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRoundWithRejects(t, 1, 1, 1, 10) // earlier round: one reject
	h.seedImplementReviewRoundWithRejects(t, 1, 1, 0, 20) // newer round: clean
	h.seedResolvedZeroChecks()

	if got := h.awaitingMergeDetail(t); got != cleanMergeDetailResolved {
		t.Errorf("detail = %q, want the clean resolved string: the earlier round's reject is superseded", got)
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

// TestObserveParkedReview_AcceptanceNotValidated_StampsAwaitingMerge pins the
// #2347 DEFAULT-ARM fall-through: acceptanceGateNotValidated is not one of the
// switch's parking cases, so it must reach RuleChecksGreenAwaitingMerge and
// stamp awaiting_merge / merge_pr — NOT park in acceptance_settled_outcome_unknown
// (which is where an unhandled verdict would have landed before the gate learned
// the state, wedging every no-live-target run). The derived status is
// deliberately identical to a pass here; the operator-visible distinction is
// carried by the MCP next_actions state, not by this presentation stamp.
func TestObserveParkedReview_AcceptanceNotValidated_StampsAwaitingMerge(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	h.seedAcceptanceObserverRun(stageStatePtr(run.StageStateSucceeded))
	seedAcceptanceOutcome(h.au, h.runID, 30, acceptanceVerdictNotValidated)

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	advances := h.driveAdvances(t)
	if len(advances) != 2 || advances[1].Rule != drive.RuleChecksGreenAwaitingMerge {
		t.Fatalf("run_auto_advanced = %+v, want settled + checks_green_awaiting_merge on a not-validated acceptance", advances)
	}
	if advances[1].NextAction == nil || advances[1].NextAction.Action != "merge_pr" {
		t.Errorf("NextAction = %+v, want merge_pr", advances[1].NextAction)
	}
	for _, a := range advances {
		if a.To == "acceptance_settled_outcome_unknown" {
			t.Error("a not_validated verdict must NOT park in acceptance_settled_outcome_unknown (#2347)")
		}
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

// TestObserveParkedReview_AcceptanceArbitrated_StampsAwaitingMerge pins the
// E66.37 / #2474 arm on the DRIVE presentation surface: once an operator
// arbitration discharges the failed verdict the gate reads acceptance_arbitrated,
// which is NOT one of the switch's parking cases, so the observer must fall
// through to checks_green_awaiting_merge / merge_pr instead of re-stamping the
// acceptance_triage park. Without this the run's derived_status would keep
// telling the operator to arbitrate a triage they already discharged.
func TestObserveParkedReview_AcceptanceArbitrated_StampsAwaitingMerge(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	h.seedAcceptanceObserverRun(stageStatePtr(run.StageStateSucceeded))
	seedAcceptanceOutcome(h.au, h.runID, 30, acceptanceVerdictFailed)
	seedArbitration(h.au, h.runID, 31, 30)

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	advances := h.driveAdvances(t)
	if len(advances) != 2 || advances[1].Rule != drive.RuleChecksGreenAwaitingMerge {
		t.Fatalf("run_auto_advanced = %+v, want settled + checks_green_awaiting_merge on an arbitrated acceptance", advances)
	}
	if advances[1].NextAction == nil || advances[1].NextAction.Action != "merge_pr" {
		t.Errorf("NextAction = %+v, want merge_pr", advances[1].NextAction)
	}
	for _, a := range advances {
		if a.To == "acceptance_triage" {
			t.Error("an arbitrated acceptance failure must NOT re-park in acceptance_triage (#2474)")
		}
	}
}

// TestObserveParkedReview_AcceptanceArbitrationSuperseded_ParksTriage pins the
// invalidation on the same surface: an acceptance re-run recorded a NEWER failed
// verdict the prior arbitration does not name, so the observer parks at
// acceptance_triage again rather than presenting a merge the gate would refuse.
func TestObserveParkedReview_AcceptanceArbitrationSuperseded_ParksTriage(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	h.seedAcceptanceObserverRun(stageStatePtr(run.StageStateSucceeded))
	seedAcceptanceOutcome(h.au, h.runID, 30, acceptanceVerdictFailed)
	seedArbitration(h.au, h.runID, 31, 30)
	seedAcceptanceOutcome(h.au, h.runID, 40, acceptanceVerdictFailed) // re-run

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	advances := h.driveAdvances(t)
	if len(advances) != 2 || advances[1].Rule != drive.RuleAcceptanceTriage {
		t.Fatalf("run_auto_advanced = %+v, want settled + acceptance_triage after a superseding re-run", advances)
	}
	for _, a := range advances {
		if a.Rule == drive.RuleChecksGreenAwaitingMerge {
			t.Fatal("awaiting_merge must NOT stamp on a superseded arbitration")
		}
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

// seedRunAutoAdvanced seeds a run_auto_advanced audit entry naming rule at the
// given Sequence, so LatestRuleIs (and applyDriveSurfaces) can pick the latest
// unambiguously — the auditFake assigns no Sequence to appended entries.
func (h *driveObserverHarness) seedRunAutoAdvanced(seq int64, rule drive.Rule) {
	payload, _ := json.Marshal(drive.Advance{Rule: rule})
	rid := h.runID
	h.au.seeded = append(h.au.seeded, &audit.Entry{
		RunID: &rid, Sequence: seq, Category: drive.Category, Payload: payload,
	})
}

// TestObserveParkedReview_AcceptancePending_RestampsAfterFixupRepark pins the
// exact #2122 shape: a prior RuleAcceptancePending stamp superseded by a LATER
// RuleFixupRereviewRepark entry (the fix-up re-park) is no longer the run's
// latest run_auto_advanced entry, so derived_status is stale. Because the
// acceptance-pending arm is now idempotent on Engine.LatestRuleIs (run-wide
// latest) rather than Engine.Recorded (per-(run,stage) ever-recorded), the
// observer RE-STAMPS acceptance_pending — re-asserting the current derived
// status so GET /v0/runs derived_status returns to acceptance_pending and the
// MCP drive loop dispatches acceptance instead of parking (#1961).
func TestObserveParkedReview_AcceptancePending_RestampsAfterFixupRepark(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	h.seedAcceptanceObserverRun(stageStatePtr(run.StageStateRunning)) // non-terminal, no verdict
	// Model the completed fix-up round: a prior acceptance_pending stamp, then a
	// LATER fixup_rereview_repark that superseded it as the latest entry.
	h.seedRunAutoAdvanced(30, drive.RuleAcceptancePending)
	h.seedRunAutoAdvanced(40, drive.RuleFixupRereviewRepark)

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	advances := h.driveAdvances(t)
	restamped := 0
	for _, a := range advances {
		if a.Rule == drive.RuleAcceptancePending {
			restamped++
			if a.To != "acceptance_pending" || a.NextAction == nil || a.NextAction.Action != "await_acceptance" {
				t.Errorf("re-stamped entry = %+v, want acceptance_pending / await_acceptance", a)
			}
		}
	}
	if restamped != 1 {
		t.Fatalf("acceptance_pending re-stamps = %d, want 1 (LatestRuleIs returned false after the repark superseded the prior stamp)", restamped)
	}
}

// TestObserveParkedReview_AcceptancePending_NoRestampWhenAlreadyLatest is the
// anti-oscillation control: with a prior RuleAcceptancePending already the
// LATEST run_auto_advanced entry and no intervening repark, LatestRuleIs returns
// true, so a fresh observation appends NO duplicate acceptance_pending.
func TestObserveParkedReview_AcceptancePending_NoRestampWhenAlreadyLatest(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	h.seedAcceptanceObserverRun(stageStatePtr(run.StageStateRunning))
	h.seedRunAutoAdvanced(30, drive.RuleAcceptancePending) // already the latest entry

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	advances := h.driveAdvances(t)
	for _, a := range advances {
		if a.Rule == drive.RuleAcceptancePending {
			t.Fatalf("acceptance_pending re-stamped when it was already the latest entry; want no duplicate (%+v)", advances)
		}
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

// TestNewServer_MCPRouteStateFromZeroConfig pins the zero-value default:
// New() applies the ":8080" Addr default, which is NOT loopback, so a
// literally-zero Config must yield the 403 posture rather than an enabled
// route. An explicit loopback Addr yields the derived, enabled state.
func TestNewServer_MCPRouteStateFromZeroConfig(t *testing.T) {
	zero := New(Config{MCPServerFactory: testMCPServerFactory})
	if zero.mcpRoute.mode != mcpRouteNotLoopback {
		t.Errorf("zero Config mode = %v, want mcpRouteNotLoopback (the :8080 default binds all interfaces)", zero.mcpRoute.mode)
	}
	if zero.mcpHandler != nil {
		t.Error("zero Config built an MCP handler for a route that refuses every request")
	}

	loopback := New(Config{Addr: "127.0.0.1:8080", MCPServerFactory: testMCPServerFactory})
	if loopback.mcpRoute.mode != mcpRouteEnabled {
		t.Fatalf("loopback mode = %v, want mcpRouteEnabled", loopback.mcpRoute.mode)
	}
	if loopback.mcpRoute.selfURL != "http://127.0.0.1:8080" {
		t.Errorf("selfURL = %q, want the derived http://127.0.0.1:8080", loopback.mcpRoute.selfURL)
	}
	if loopback.mcpHandler == nil {
		t.Error("an enabled route has no handler")
	}
}

// TestServer_OAuthASServesMetadataWhenConfigured is the full-stack case: an
// AS-configured Config serves the RFC 8414 document through the whole
// middleware stack, and an unconfigured one leaves all four patterns on the
// 503 oauth_as_unconfigured path (ADR-076 slice 3, #2436).
func TestServer_OAuthASServesMetadataWhenConfigured(t *testing.T) {
	srv := newEnabledOAuthServer(newFakeOAuthStore(), newCIMDFetcher(newCIMD()))
	req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("metadata: status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"client_id_metadata_document_supported":true`) {
		t.Fatalf("metadata body missing CIMD gate: %s", rr.Body.String())
	}

	off := New(Config{})
	for _, p := range []struct{ method, path string }{
		{http.MethodGet, "/.well-known/oauth-authorization-server"},
		{http.MethodGet, "/v0/oauth/authorize"},
		{http.MethodPost, "/v0/oauth/authorize"},
		{http.MethodPost, "/v0/oauth/token"},
	} {
		req := httptest.NewRequest(p.method, p.path, nil)
		if p.method == http.MethodPost {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		rr := httptest.NewRecorder()
		off.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: status = %d, want 503", p.method, p.path, rr.Code)
		}
	}
}

// TestNew_WiresCIMDLimiterOnBuiltHandler is the full-stack wiring assertion
// (Config -> New -> a limiter live on the BUILT handler, #2441): the limiter is
// non-nil after New, and driving the real authorize route through Handler() past
// the source burst yields a 429 — proving the seam is reached through the whole
// middleware chain, not just a direct handler call.
func TestNew_WiresCIMDLimiterOnBuiltHandler(t *testing.T) {
	store := newFakeOAuthStore()
	srv := newEnabledOAuthServer(store, newCIMDFetcher(newCIMD()))
	if srv.oauthCIMDLimiter == nil {
		t.Fatal("New must construct a non-nil oauthCIMDLimiter")
	}
	drive := func(clientID string) int {
		q := url.Values{}
		q.Set("client_id", clientID)
		q.Set("redirect_uri", "https://app.example/cb")
		q.Set("response_type", "code")
		q.Set("resource", testResource)
		req := httptest.NewRequest(http.MethodGet, "/v0/oauth/authorize?"+q.Encode(), nil)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		return rr.Code
	}
	for i := 0; i < defaultOAuthCIMDSourceBurst; i++ {
		if code := drive("https://wired" + strconv.Itoa(i) + ".example/cimd"); code == http.StatusTooManyRequests {
			t.Fatalf("request %d within burst was rate limited on the built handler", i)
		}
	}
	if code := drive("https://wired-over.example/cimd"); code != http.StatusTooManyRequests {
		t.Fatalf("over-burst status through Handler() = %d, want 429 (limiter not live on the built handler)", code)
	}
}

// TestServer_LiftedListenAddrBindsConfigAddr pins the lifted-posture bind
// contract (#2391) by driving the REAL bind + verify sequence: a non-loopback
// UNSPECIFIED bind with the OAuth AS enabled LIFTS the /mcp loopback refusal and
// binds cfg.Addr VERBATIM (listenAddr() returns it unchanged, unlike the unlifted
// route which pins the resolved loopback literal), and Start SUCCEEDS because the
// real net.Listen binds and verifyMCPListenerLoopback passes on a lift carrying a
// resource. An earlier form asserted only listenAddr()/verifyMCPListenerLoopback
// directly and so passed regardless of whether Start's real bind/verify sequence
// worked — the vacuity this now closes. TestMCPRoute_StartBindsPinnedLoopbackIP
// covers the unlifted pinned-literal direction; the specific-host pin (where the
// lifted bind IS pinned) is TestResolveMCPRouteState_LiftedBindPinnedToSelfURLHost.
func TestServer_LiftedListenAddrBindsConfigAddr(t *testing.T) {
	// An unspecified non-loopback bind with the OAuth AS enabled is the lifted
	// posture; :0 takes an ephemeral port so the real bind is deterministic.
	s := New(Config{
		Addr:             "0.0.0.0:0",
		MCPServerFactory: testMCPServerFactory,
		OAuthASIssuer:    testIssuer, OAuthStore: newFakeOAuthStore(), OAuthCIMDFetcher: newCIMDFetcher(newCIMD()),
	})
	if s.mcpRoute.mode != mcpRouteEnabled || !s.mcpRoute.authenticatedOnly {
		t.Fatalf("route not lifted: mode=%v authOnly=%v (reason %q)", s.mcpRoute.mode, s.mcpRoute.authenticatedOnly, s.mcpRoute.reason)
	}
	// An unspecified bind pins nothing (net.Listen binds every interface, which
	// includes the loopback the selfURL dials), so listenAddr() is cfg.Addr
	// verbatim.
	if s.mcpRoute.listenAddr != "" {
		t.Errorf("lifted unspecified listenAddr = %q, want empty so listenAddr() returns cfg.Addr verbatim", s.mcpRoute.listenAddr)
	}
	if got := s.listenAddr(); got != "0.0.0.0:0" {
		t.Errorf("listenAddr() = %q, want the configured 0.0.0.0:0 verbatim in the lifted posture", got)
	}

	// Drive the REAL bind + verify: Start calls net.Listen then
	// verifyMCPListenerLoopback before Serve, so a bind failure or a verify
	// refusal surfaces here as a non-nil Start error. A resourced lift must bind
	// off-host cleanly.
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start() }()
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Start = %v; the lifted off-host bind failed its real bind/verify sequence", err)
	}
}

// --- #2497: required-checks resolution (nil vs empty snapshot) -------------

// seedSnapshotRun re-seeds the harness run with the given required-checks
// snapshot pointer (nil = absent = unresolved). Keeps Drive on and the
// running state so ObserveParkedReviewForDrive processes it.
func (h *driveObserverHarness) seedSnapshotRun(snap *run.RequiredChecksSnapshot) {
	h.repo.seedRun(&run.Run{
		ID: h.runID, Drive: true, State: run.StateRunning,
		RequiredChecksSnapshot: snap,
	})
}

// lastAdvance returns the final drive advance stamped this tick.
func (h *driveObserverHarness) lastAdvance(t *testing.T) drive.Advance {
	t.Helper()
	advances := h.driveAdvances(t)
	if len(advances) == 0 {
		t.Fatal("no drive advances stamped")
	}
	return advances[len(advances)-1]
}

// TestObserveParkedReview_NilSnapshot_StampsUnresolvedDetail is the wording
// counterfactual vehicle (#2497). A nil snapshot (the common local-loop path)
// still stamps checks_green_awaiting_merge with action merge_pr — so the local
// loop is NOT wedged and delegation's may_merge input is preserved — but the
// Event and next_action.Detail carry the unresolved phrasing and make no
// "green" claim. Deleting the unresolved branch of checksMergeDetail /
// checksEvidencePhrase (hardcoding the green string) turns these assertions
// RED.
func TestObserveParkedReview_NilSnapshot_StampsUnresolvedDetail(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	h.seedSnapshotRun(nil) // absent snapshot: protection never looked up

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	adv := h.lastAdvance(t)
	if adv.Rule != drive.RuleChecksGreenAwaitingMerge {
		t.Fatalf("rule = %q, want checks_green_awaiting_merge (local loop must not wedge)", adv.Rule)
	}
	if adv.To != "awaiting_merge" {
		t.Errorf("To = %q, want awaiting_merge", adv.To)
	}
	if adv.NextAction == nil || adv.NextAction.Action != "merge_pr" {
		t.Fatalf("NextAction = %+v, want merge_pr (delegation may_merge input preserved)", adv.NextAction)
	}
	if strings.Contains(adv.NextAction.Detail, "green") {
		t.Errorf("Detail = %q, must not claim checks are green on an unresolved run", adv.NextAction.Detail)
	}
	if !strings.Contains(adv.NextAction.Detail, "not resolved") {
		t.Errorf("Detail = %q, want the unresolved-checks wording", adv.NextAction.Detail)
	}
	if strings.Contains(adv.Event, "green") {
		t.Errorf("Event = %q, must not claim green on an unresolved run", adv.Event)
	}
	if !strings.Contains(adv.Event, "not resolved") {
		t.Errorf("Event = %q, want the unresolved-checks wording", adv.Event)
	}
}

// TestObserveParkedReview_EmptySnapshot_StampsGreenPhrasing pins the
// non-regression the issue's done-means requires: a present-but-EMPTY snapshot
// (a repo that genuinely declares zero required checks) is vacuously green and
// advances normally with the green phrasing — the empty case must NOT inherit
// the unresolved wording.
func TestObserveParkedReview_EmptySnapshot_StampsGreenPhrasing(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	h.seedSnapshotRun(&run.RequiredChecksSnapshot{Contexts: nil})

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	adv := h.lastAdvance(t)
	if adv.Rule != drive.RuleChecksGreenAwaitingMerge {
		t.Fatalf("rule = %q, want checks_green_awaiting_merge", adv.Rule)
	}
	if !strings.Contains(adv.NextAction.Detail, "required checks are green") {
		t.Errorf("Detail = %q, want the green phrasing for a genuinely-zero-required-checks repo", adv.NextAction.Detail)
	}
	if strings.Contains(adv.NextAction.Detail, "not resolved") {
		t.Errorf("Detail = %q, an empty (present) snapshot must not carry the unresolved wording", adv.NextAction.Detail)
	}
}

// TestObserveParkedReview_AllChecksPass_StampsGreenPhrasing pins a present
// snapshot whose single required context is recorded StatePass: green phrasing.
func TestObserveParkedReview_AllChecksPass_StampsGreenPhrasing(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	h.seedSnapshotRun(&run.RequiredChecksSnapshot{Contexts: []string{"ci_pass"}})
	h.scs.seed(h.stage.ID, "ci_pass", stagecheck.StatePass)

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	adv := h.lastAdvance(t)
	if adv.Rule != drive.RuleChecksGreenAwaitingMerge {
		t.Fatalf("rule = %q, want checks_green_awaiting_merge", adv.Rule)
	}
	if !strings.Contains(adv.NextAction.Detail, "required checks are green") {
		t.Errorf("Detail = %q, want the green phrasing on an all-pass snapshot", adv.NextAction.Detail)
	}
}

// TestObserveParkedReview_UnreportedRequiredCheck_StampsNothing pins the
// total-Actions-outage posture the issue reports, once a snapshot EXISTS: a
// present snapshot whose required context has no row at all is unknown (not
// green, not failed) — the observer stamps only reviews_settled_gate, never
// awaiting_merge or ci_failed.
func TestObserveParkedReview_UnreportedRequiredCheck_StampsNothing(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	h.seedSnapshotRun(&run.RequiredChecksSnapshot{Contexts: []string{"ci_pass"}})
	// No row seeded for ci_pass: unreported.

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	advances := h.driveAdvances(t)
	if len(advances) != 1 || advances[0].Rule != drive.RuleReviewsSettledGate {
		t.Fatalf("run_auto_advanced = %+v, want only reviews_settled_gate on an unreported required check", advances)
	}
}

// TestObserveParkedReview_FailedRequiredCheck_StampsCIFailed pins that the
// resolved-and-red mirror survives the `resolved && !green` refactor: a present
// snapshot whose required context is StateFail still stamps ci_failed.
func TestObserveParkedReview_FailedRequiredCheck_StampsCIFailed(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	h.seedSnapshotRun(&run.RequiredChecksSnapshot{Contexts: []string{"ci_pass"}})
	h.scs.seed(h.stage.ID, "ci_pass", stagecheck.StateFail)

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	adv := h.lastAdvance(t)
	if adv.Rule != drive.RuleCIFailed {
		t.Fatalf("rule = %q, want ci_failed on a red required check", adv.Rule)
	}
	if adv.NextAction == nil || adv.NextAction.Action != "classify_ci_failure" {
		t.Errorf("NextAction = %+v, want classify_ci_failure", adv.NextAction)
	}
}

// TestObserveParkedReview_NilSnapshot_AcceptancePending_UnresolvedDetail pins
// binding approval condition 1: a nil-snapshot run on an acceptance-declaring
// workflow with a pending acceptance stage stamps acceptance_pending whose
// Event and Detail carry the unresolved phrasing (no "green" claim). Every
// local run has a nil snapshot, so this is the common local acceptance path.
func TestObserveParkedReview_NilSnapshot_AcceptancePending_UnresolvedDetail(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	// seedAcceptanceObserverRun re-seeds WITHOUT a snapshot (nil).
	h.seedAcceptanceObserverRun(stageStatePtr(run.StageStateRunning))

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	adv := h.lastAdvance(t)
	if adv.Rule != drive.RuleAcceptancePending {
		t.Fatalf("rule = %q, want acceptance_pending", adv.Rule)
	}
	if adv.NextAction == nil || adv.NextAction.Action != "await_acceptance" {
		t.Fatalf("NextAction = %+v, want await_acceptance", adv.NextAction)
	}
	if strings.Contains(adv.NextAction.Detail, "green") {
		t.Errorf("Detail = %q, must not claim green on an unresolved acceptance park", adv.NextAction.Detail)
	}
	if !strings.Contains(adv.NextAction.Detail, "not resolved") {
		t.Errorf("Detail = %q, want the unresolved-checks wording", adv.NextAction.Detail)
	}
	if strings.Contains(adv.Event, "green") {
		t.Errorf("Event = %q, must not claim green on an unresolved acceptance park", adv.Event)
	}
}

// TestObserveParkedReview_NilSnapshot_AcceptanceTriage_UnresolvedDetail and
// _OutcomeUnknown_ pin condition 1 across the other two acceptance parks: on a
// nil-snapshot run each carries the unresolved phrasing in its Detail.
func TestObserveParkedReview_NilSnapshot_AcceptanceTriage_UnresolvedDetail(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	h.seedAcceptanceObserverRun(stageStatePtr(run.StageStateSucceeded))
	seedAcceptanceOutcome(h.au, h.runID, 30, acceptanceVerdictFailed)

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	adv := h.lastAdvance(t)
	if adv.Rule != drive.RuleAcceptanceTriage {
		t.Fatalf("rule = %q, want acceptance_triage", adv.Rule)
	}
	if strings.Contains(adv.NextAction.Detail, "green") {
		t.Errorf("Detail = %q, must not claim green on an unresolved acceptance_triage park", adv.NextAction.Detail)
	}
	if !strings.Contains(adv.NextAction.Detail, "not resolved") {
		t.Errorf("Detail = %q, want the unresolved-checks wording", adv.NextAction.Detail)
	}
}

func TestObserveParkedReview_NilSnapshot_AcceptanceOutcomeUnknown_UnresolvedDetail(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	h.seedAcceptanceObserverRun(stageStatePtr(run.StageStateSucceeded)) // terminal, no verdict

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	adv := h.lastAdvance(t)
	if adv.Rule != drive.RuleAcceptanceOutcomeUnknown {
		t.Fatalf("rule = %q, want acceptance_settled_outcome_unknown", adv.Rule)
	}
	if strings.Contains(adv.NextAction.Detail, "green") {
		t.Errorf("Detail = %q, must not claim green on an unresolved acceptance_outcome_unknown park", adv.NextAction.Detail)
	}
	if !strings.Contains(adv.NextAction.Detail, "not resolved") {
		t.Errorf("Detail = %q, want the unresolved-checks wording", adv.NextAction.Detail)
	}
}

// TestObserveParkedReview_AcceptanceUndecidable_StampsAwaitingMerge is the
// #2512 pin on the DRIVE PRESENTATION surface — the third merge consumer. The
// switch's parking cases are the pending / triage / outcome-unknown arms;
// acceptanceGateUndecidable is in none of them, so the observer must reach
// RuleChecksGreenAwaitingMerge and stamp awaiting_merge / merge_pr.
//
// The two negative assertions are the load-bearing half. Parking an undecidable
// run in acceptance_settled_outcome_unknown would wedge it behind a read arm
// that has nothing to read, and parking it in acceptance_triage would tell the
// operator to arbitrate a disposition that was never written — an undecidable
// row is not a defect and routes no acceptance_triage_decided at all.
func TestObserveParkedReview_AcceptanceUndecidable_StampsAwaitingMerge(t *testing.T) {
	h := newDriveObserverHarness(t, true)
	h.seedImplementReviewRound(t, 1, 1, 10)
	h.seedAcceptanceObserverRun(stageStatePtr(run.StageStateSucceeded))
	seedAcceptanceOutcome(h.au, h.runID, 30, acceptanceVerdictUndecidable)

	h.s.ObserveParkedReviewForDrive(context.Background(), h.stage, driveObserverPRURL)

	advances := h.driveAdvances(t)
	if len(advances) != 2 || advances[1].Rule != drive.RuleChecksGreenAwaitingMerge {
		t.Fatalf("run_auto_advanced = %+v, want settled + checks_green_awaiting_merge on an undecidable acceptance", advances)
	}
	if advances[1].NextAction == nil || advances[1].NextAction.Action != "merge_pr" {
		t.Errorf("NextAction = %+v, want merge_pr", advances[1].NextAction)
	}
	for _, a := range advances {
		if a.To == "acceptance_settled_outcome_unknown" {
			t.Error("an undecidable verdict must NOT park in acceptance_settled_outcome_unknown (#2512)")
		}
		if a.To == "acceptance_triage" {
			t.Error("an undecidable verdict must NOT park in acceptance_triage — it routes no triage disposition to arbitrate (#2512)")
		}
	}
}

// -------- forge-keyed identity provider enumeration (E66.4 / #2392,
// binding constraint 8) --------

// TestNew_NilIdentityProvidersMap_ExcludesNoOp drives the NIL-map path
// SPECIFICALLY — Config{} and Config{OAuthClientID: …} with
// IdentityProviders LEFT NIL, which is a different branch in New than an
// explicitly-empty map. A bare server must enumerate ZERO providers and
// answer GET /v0/tokens/login 503 tokens_unconfigured: the NoOp installed
// by New's nil-provider default is deny-by-default precisely BECAUSE no
// forge is configured, so advertising it would advertise a provider that
// cannot authenticate anyone.
func TestNew_NilIdentityProvidersMap_ExcludesNoOp(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"bare", Config{Addr: "127.0.0.1:0"}},
		{"client id without provider", Config{Addr: "127.0.0.1:0", OAuthClientID: "Iv1.abc"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(tc.cfg)
			if got := s.configuredIdentityProviders(); len(got) != 0 {
				t.Fatalf("configuredIdentityProviders() = %v, want empty", got)
			}
			w := tokenRequest(t, s, http.MethodGet, "/v0/tokens/login", "", nil)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("GET /v0/tokens/login status = %d, want 503:\n%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "tokens_unconfigured") {
				t.Errorf("body missing tokens_unconfigured:\n%s", w.Body.String())
			}
		})
	}
}

// TestNew_EmptyIdentityProvidersMap_ExcludesNoOp is the sibling for the
// explicitly-EMPTY map, which takes a different branch in New's seeding
// (the range copies nothing but the map is non-nil), plus the positive
// case: a real non-NoOp provider WITH a device client id IS enumerated.
func TestNew_EmptyIdentityProvidersMap_ExcludesNoOp(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0",
		IdentityProviders:       map[string]identity.IdentityProvider{},
		IdentityDeviceClientIDs: map[string]string{},
	})
	if got := s.configuredIdentityProviders(); len(got) != 0 {
		t.Fatalf("configuredIdentityProviders() = %v, want empty", got)
	}

	// An explicitly-wired NoOp is excluded too — the type check, not the
	// seeding order, is what rejects this one.
	sNoOp := New(Config{Addr: "127.0.0.1:0",
		IdentityProviders:       map[string]identity.IdentityProvider{identity.ProviderGitHub: identity.NewNoOp()},
		IdentityDeviceClientIDs: map[string]string{identity.ProviderGitHub: "Iv1.abc"},
	})
	if got := sNoOp.configuredIdentityProviders(); len(got) != 0 {
		t.Fatalf("explicitly-wired NoOp enumerated: %v, want empty", got)
	}

	// Positive control: a real provider with a device client id IS configured.
	sReal := New(Config{Addr: "127.0.0.1:0",
		IdentityProvider: &fakeIdentityProvider{subject: "github:octocat"},
		OAuthClientID:    "Iv1.abc",
	})
	if got := sReal.configuredIdentityProviders(); len(got) != 1 {
		t.Fatalf("configuredIdentityProviders() = %v, want exactly github", got)
	}
}

// TestNew_ProviderWithoutDeviceClientID_NotConfigured pins the SECOND
// exclusion in configuredIdentityProviders: discovery exists to tell the
// CLI which client_id to drive the device flow with, so a provider it
// cannot drive must not be advertised.
func TestNew_ProviderWithoutDeviceClientID_NotConfigured(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0",
		IdentityProviders: map[string]identity.IdentityProvider{
			identity.ProviderGitLab: &fakeIdentityProvider{subject: "gitlab:alice"},
		},
	})
	if got := s.configuredIdentityProviders(); len(got) != 0 {
		t.Fatalf("provider without a device client id enumerated: %v", got)
	}
}

// TestNew_RawIdentityProvidersMap_NeverContainsNoOp isolates the SEEDING
// ORDER invariant, which the filtered enumeration cannot observe (BC3): a
// NoOp seeded after the default is excluded by type, so
// TestNew_NilIdentityProvidersMap_ExcludesNoOp stays green either way.
// This reads the RAW, UNFILTERED cfg.IdentityProviders map instead — moving
// New's seeding block after the nil-provider default makes cfg.IdentityProvider
// non-nil (the freshly installed NoOp) at seeding time, so the map gains a
// github→NoOp entry and this assertion goes red. It is the only observation
// in the suite that distinguishes the two layers.
func TestNew_RawIdentityProvidersMap_NeverContainsNoOp(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"bare", Config{Addr: "127.0.0.1:0"}},
		{"client id without provider", Config{Addr: "127.0.0.1:0", OAuthClientID: "Iv1.abc"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(tc.cfg)
			if len(s.cfg.IdentityProviders) != 0 {
				t.Fatalf("raw IdentityProviders = %#v, want no entry at all: the NoOp default must never be seeded", s.cfg.IdentityProviders)
			}
		})
	}
}

// TestConfiguredIdentityProviderNames_DeterministicOrder pins the
// github-then-gitlab order the discovery response and its legacy
// first-entry mirror depend on, against Go's randomized map iteration.
func TestConfiguredIdentityProviderNames_DeterministicOrder(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0",
		IdentityProviders: map[string]identity.IdentityProvider{
			identity.ProviderGitLab:  &fakeIdentityProvider{subject: "gitlab:alice"},
			identity.ProviderGitHub:  &fakeIdentityProvider{subject: "github:octocat"},
			"zzz-future-forge-still": &fakeIdentityProvider{subject: "zzz:bob"},
		},
		IdentityDeviceClientIDs: map[string]string{
			identity.ProviderGitLab:  "gl-device",
			identity.ProviderGitHub:  "Iv1.abc",
			"zzz-future-forge-still": "zz-device",
		},
	})
	want := []string{identity.ProviderGitHub, identity.ProviderGitLab, "zzz-future-forge-still"}
	for i := 0; i < 20; i++ {
		if got := s.configuredIdentityProviderNames(); !reflect.DeepEqual(got, want) {
			t.Fatalf("configuredIdentityProviderNames() = %v, want %v", got, want)
		}
	}
}
