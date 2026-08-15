package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- fishhawk_reap_stage (E67.47 / #2689) ---
//
// ONE TEST PER NAMED FAILURE MODE. Every refusal arm asserts the fake recorded
// ZERO reap-failure POSTs — an explicit never-dialed seam rather than an
// inference from a zero-value response — because the whole point of the local
// refusals is that they short-circuit BEFORE the HTTP hop.

// reapResolver builds a resolver whose liveness probe is INJECTED, so no test
// in this file depends on pgrep being present or on any real pid.
func reapResolver(t *testing.T, srv *httptest.Server, verdict runnerLivenessVerdict) (*runResolver, *int) {
	t.Helper()
	r := newResolver(srv, nil)
	calls := 0
	r.driveProbeRunnerLiveness = func(context.Context, string) runnerLivenessVerdict {
		calls++
		return verdict
	}
	return r, &calls
}

// seedLocalRun seeds a run row whose runner_kind is local — the ONLY arm in
// which classifyReapLiveness invokes the host probe.
func seedLocalRun(fb *fakeBackend, runID uuid.UUID) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", RunnerKind: driveRunnerKindLocal}
}

// seedStrandedStage seeds the target stage so the pre-POST state comparison is
// ARMED. Without a stage row the comparison is unarmed by design (fail open),
// so the race tests below must seed one.
func seedStrandedStage(fb *fakeBackend, runID uuid.UUID, stageID string, state string) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.stagesByRun[runID] = []Stage{{
		ID: stageID, RunID: runID.String(), Sequence: 1, Type: "implement", State: state,
	}}
}

// setStageState models the CONCURRENT dispatcher: it advances the seeded stage
// the way a host dispatch that spawned a runner would.
func setStageState(fb *fakeBackend, runID uuid.UUID, state string) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	for i := range fb.stagesByRun[runID] {
		fb.stagesByRun[runID][i].State = state
	}
}

func reapHops(fb *fakeBackend) int {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.reapFailureAttempts
}

// (A) TestReapStage_InvalidRunID: an unparseable run_id is a tool error naming
// the field, with no HTTP hop.
func TestReapStage_InvalidRunID(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r, probeCalls := reapResolver(t, srv, runnerDead)

	_, _, err := r.reapStage(context.Background(), nil, ReapStageInput{
		RunID: "not-a-uuid", StageID: uuid.NewString(),
	})
	if err == nil {
		t.Fatal("expected a tool error for an invalid run_id")
	}
	if !strings.Contains(err.Error(), "run_id") {
		t.Errorf("error must name run_id: %v", err)
	}
	if got := reapHops(fb); got != 0 {
		t.Errorf("reap-failure POSTs = %d, want 0 (the parse must short-circuit before the hop)", got)
	}
	if *probeCalls != 0 {
		t.Errorf("probe invoked %d times on an unparseable id, want 0", *probeCalls)
	}
}

// (A) TestReapStage_InvalidStageID: the same for stage_id.
func TestReapStage_InvalidStageID(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r, _ := reapResolver(t, srv, runnerDead)

	_, _, err := r.reapStage(context.Background(), nil, ReapStageInput{
		RunID: uuid.NewString(), StageID: "nope",
	})
	if err == nil {
		t.Fatal("expected a tool error for an invalid stage_id")
	}
	if !strings.Contains(err.Error(), "stage_id") {
		t.Errorf("error must name stage_id: %v", err)
	}
	if got := reapHops(fb); got != 0 {
		t.Errorf("reap-failure POSTs = %d, want 0", got)
	}
}

// (A) TestReapStage_RejectsCategoryOutsideBC is the counterfactual vehicle for
// the LOCAL category allow-list. The message is the verb's OWN — it is never
// compared to the server's 400 text, so the two strings stay independent.
func TestReapStage_RejectsCategoryOutsideBC(t *testing.T) {
	for _, category := range []string{"A", "X", "c", "b", "C ", "BC"} {
		t.Run(category, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			r, _ := reapResolver(t, srv, runnerDead)

			_, _, err := r.reapStage(context.Background(), nil, ReapStageInput{
				RunID: uuid.NewString(), StageID: uuid.NewString(), Category: category,
			})
			if err == nil {
				t.Fatalf("category %q was accepted; only B and C are allowed", category)
			}
			if !strings.Contains(err.Error(), `"B"`) || !strings.Contains(err.Error(), `"C"`) {
				t.Errorf("refusal must name both allowed categories: %v", err)
			}
			if got := reapHops(fb); got != 0 {
				t.Errorf("reap-failure POSTs = %d, want 0 (a bad category must never reach the backend)", got)
			}
		})
	}
}

// (A) TestReapStage_DefaultsCategoryAndReason reads the DEFAULTS off the
// recorded request body rather than off the output, so a default applied only
// in the response would not pass.
func TestReapStage_DefaultsCategoryAndReason(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r, _ := reapResolver(t, srv, runnerDead)
	runID, stageID := uuid.New(), uuid.New()

	if _, _, err := r.reapStage(context.Background(), nil, ReapStageInput{
		RunID: runID.String(), StageID: stageID.String(),
	}); err != nil {
		t.Fatalf("reapStage: %v", err)
	}

	fb.mu.Lock()
	bodies := fb.reapFailureByStage[stageID]
	fb.mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("recorded bodies = %d, want 1", len(bodies))
	}
	if bodies[0].Category != "C" {
		t.Errorf("category = %q, want the C default", bodies[0].Category)
	}
	if bodies[0].Reason != reapStageDefaultReason {
		t.Errorf("reason = %q, want %q", bodies[0].Reason, reapStageDefaultReason)
	}
}

// (A) TestReapStage_CategoryBAcceptedWithItsOwnNextStep pins the OTHER side of
// the allow-list boundary — the one TestReapStage_RejectsCategoryOutsideBC and
// TestReapStage_DefaultsCategoryAndReason leave untested. Two things are
// proven, and the second is the fix-up concern:
//
//	(1) B is genuinely ACCEPTED: it reaches the backend and lands on the wire
//	    verbatim, rather than being defaulted away or refused locally.
//	(2) It gets its OWN next_step. run.RetryableFailure classifies B
//	    (constraint/policy) as NOT retryable, so fishhawk_retry_stage refuses a
//	    B-reaped stage with 422 retry_not_applicable — the C guidance ("call
//	    fishhawk_retry_stage next") would send the operator at a verb that
//	    cannot admit the stage this call just produced.
//
// This is the counterfactual vehicle for reapStageNextStep's branch: the B and
// C hints are driven through the SAME handler and required to DIFFER, so
// collapsing the selector back to one constant reddens it whatever the wording.
func TestReapStage_CategoryBAcceptedWithItsOwnNextStep(t *testing.T) {
	reap := func(t *testing.T, category string) (ReapStageOutput, reapFailureRequest) {
		t.Helper()
		fb, srv := newFakeBackend(t)
		r, _ := reapResolver(t, srv, runnerDead)
		runID, stageID := uuid.New(), uuid.New()

		_, out, err := r.reapStage(context.Background(), nil, ReapStageInput{
			RunID: runID.String(), StageID: stageID.String(), Category: category,
		})
		if err != nil {
			t.Fatalf("category %q must be accepted: %v", category, err)
		}
		fb.mu.Lock()
		bodies := fb.reapFailureByStage[stageID]
		fb.mu.Unlock()
		if len(bodies) != 1 {
			t.Fatalf("recorded bodies = %d, want 1", len(bodies))
		}
		return out, bodies[0]
	}

	outB, bodyB := reap(t, "B")
	if bodyB.Category != "B" {
		t.Errorf("wire category = %q, want B (an accepted category must reach the backend verbatim)", bodyB.Category)
	}
	if outB.StageState != "failed" {
		t.Errorf("stage_state = %q, want failed", outB.StageState)
	}

	outC, _ := reap(t, "C")
	if outB.NextStep == outC.NextStep {
		t.Errorf("category B and C returned the SAME next_step (%q); B is not retryable, so its recovery path differs", outB.NextStep)
	}
	// The B guidance must not tell the operator to call the verb that refuses a
	// category-B stage; it must name the refusal and the override that admits it.
	if !strings.Contains(outB.NextStep, "retry_not_applicable") {
		t.Errorf("category-B next_step must name the refusal fishhawk_retry_stage answers with: %q", outB.NextStep)
	}
	if !strings.Contains(outB.NextStep, "override") {
		t.Errorf("category-B next_step must name the operator-only override that re-opens a B failure: %q", outB.NextStep)
	}
	if !strings.Contains(outC.NextStep, "fishhawk_retry_stage admits it") {
		t.Errorf("category-C next_step must still say fishhawk_retry_stage admits the stage: %q", outC.NextStep)
	}
}

// (A) TestReapStage_LiveRunnerRefused is the counterfactual vehicle for the
// LIVE refusal: a runner_kind=local run whose probe finds a live process is
// refused outright, with zero HTTP hops.
func TestReapStage_LiveRunnerRefused(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID, stageID := uuid.New(), uuid.New()
	seedLocalRun(fb, runID)
	r, probeCalls := reapResolver(t, srv, runnerLive)

	_, _, err := r.reapStage(context.Background(), nil, ReapStageInput{
		RunID: runID.String(), StageID: stageID.String(),
	})
	if err == nil {
		t.Fatal("a LIVE runner must refuse the reap")
	}
	if !strings.Contains(err.Error(), stageID.String()) {
		t.Errorf("refusal must name the stage: %v", err)
	}
	if got := reapHops(fb); got != 0 {
		t.Errorf("reap-failure POSTs = %d, want 0 (a live runner must never reach the backend)", got)
	}
	if *probeCalls != 1 {
		t.Errorf("probe invoked %d times, want 1", *probeCalls)
	}
}

// (B) TestReapStage_LivenessGatedOnRunnerKind is the counterfactual vehicle for
// the runner_kind GATE. Each non-local arm injects a runnerDead seam: with the
// gate deleted that verdict would classify a github_actions run DEAD, which is
// a fabricated claim about a process table that never ran the runner. The
// paired LOCAL control asserts the probe IS invoked and the verdict IS dead, so
// the test cannot pass by classifying everything unknown.
func TestReapStage_LivenessGatedOnRunnerKind(t *testing.T) {
	for _, kind := range []string{"github_actions", "gitlab_ci", "some_future_kind", ""} {
		name := kind
		if name == "" {
			name = "absent"
		}
		t.Run(name, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			runID, stageID := uuid.New(), uuid.New()
			fb.mu.Lock()
			fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", RunnerKind: kind}
			fb.mu.Unlock()
			r, probeCalls := reapResolver(t, srv, runnerDead)

			_, out, err := r.reapStage(context.Background(), nil, ReapStageInput{
				RunID: runID.String(), StageID: stageID.String(),
			})
			if err != nil {
				t.Fatalf("reapStage: %v", err)
			}
			// (1) the probe was NEVER invoked.
			if *probeCalls != 0 {
				t.Errorf("the host probe ran %d times for runner_kind=%q; a host process table says NOTHING about a non-local run", *probeCalls, kind)
			}
			// (2) the verdict is unknown and is never "dead".
			if out.RunnerLiveness != "unknown" {
				t.Errorf("runner_liveness = %q, want unknown for runner_kind=%q", out.RunnerLiveness, kind)
			}
			// (3) a warning names the kind and says liveness is unverifiable here.
			wantKind := kind
			if wantKind == "" {
				wantKind = "(absent)"
			}
			var warned bool
			for _, w := range out.Warnings {
				if strings.Contains(w, wantKind) && strings.Contains(w, "unverifiable from this host") {
					warned = true
				}
			}
			if !warned {
				t.Errorf("warnings %v must name runner_kind=%s and state liveness is unverifiable from this host", out.Warnings, wantKind)
			}
			// (4) the reap PROCEEDED.
			if !out.Transitioned || out.StageState != "failed" {
				t.Errorf("the reap did not proceed: transitioned=%v stage_state=%q", out.Transitioned, out.StageState)
			}
			if got := reapHops(fb); got != 1 {
				t.Errorf("reap-failure POSTs = %d, want 1", got)
			}
		})
	}

	// THE PAIRED CONTROL: the same runnerDead seam on a runner_kind=local run
	// MUST reach the probe and MUST report dead.
	t.Run("local_control", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		runID, stageID := uuid.New(), uuid.New()
		seedLocalRun(fb, runID)
		r, probeCalls := reapResolver(t, srv, runnerDead)

		_, out, err := r.reapStage(context.Background(), nil, ReapStageInput{
			RunID: runID.String(), StageID: stageID.String(),
		})
		if err != nil {
			t.Fatalf("reapStage: %v", err)
		}
		// Two calls: the classification, then confirmStrandBeforeReap's pre-POST
		// re-probe (the check/use-window narrowing).
		if *probeCalls != 2 {
			t.Errorf("probe invoked %d times for a local run, want 2", *probeCalls)
		}
		if out.RunnerLiveness != "dead" {
			t.Errorf("runner_liveness = %q, want dead for a probed local run", out.RunnerLiveness)
		}
		if len(out.Warnings) != 0 {
			t.Errorf("a conclusive DEAD verdict needs no warning; got %v", out.Warnings)
		}
	})
}

// (C) TestReapStage_GetRunErrorClassifiesUnknown: an unreadable run row means
// runner_kind is unreadable, so the verdict is UNKNOWN (never dead) and the
// reap still proceeds with a warning naming the run and the error.
func TestReapStage_GetRunErrorClassifiesUnknown(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID, stageID := uuid.New(), uuid.New()
	fb.mu.Lock()
	fb.getStatusByID[runID] = http.StatusInternalServerError
	fb.mu.Unlock()
	r, probeCalls := reapResolver(t, srv, runnerDead)

	_, out, err := r.reapStage(context.Background(), nil, ReapStageInput{
		RunID: runID.String(), StageID: stageID.String(),
	})
	if err != nil {
		t.Fatalf("an unreadable run row must not fail the reap: %v", err)
	}
	if out.RunnerLiveness != "unknown" {
		t.Errorf("runner_liveness = %q, want unknown", out.RunnerLiveness)
	}
	if *probeCalls != 0 {
		t.Errorf("probe invoked %d times without a readable runner_kind, want 0", *probeCalls)
	}
	var warned bool
	for _, w := range out.Warnings {
		if strings.Contains(w, runID.String()) && strings.Contains(w, "unreadable") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("warnings %v must name the run and the read failure", out.Warnings)
	}
	if !out.Transitioned {
		t.Error("the reap must still proceed on an unreadable run row")
	}
}

// (C) TestReapStage_ProbeUnknownWarns: a local run whose probe is inconclusive
// (pgrep absent / exit >=2 / timeout) warns and proceeds — it is never treated
// as live (which would refuse) nor asserted dead.
func TestReapStage_ProbeUnknownWarns(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID, stageID := uuid.New(), uuid.New()
	seedLocalRun(fb, runID)
	r, probeCalls := reapResolver(t, srv, runnerUnknown)

	_, out, err := r.reapStage(context.Background(), nil, ReapStageInput{
		RunID: runID.String(), StageID: stageID.String(),
	})
	if err != nil {
		t.Fatalf("an inconclusive probe must not fail the reap: %v", err)
	}
	// The classification plus the pre-POST re-probe; an inconclusive verdict is
	// re-checked exactly like a dead one.
	if *probeCalls != 2 {
		t.Errorf("probe invoked %d times, want 2", *probeCalls)
	}
	if out.RunnerLiveness != "unknown" {
		t.Errorf("runner_liveness = %q, want unknown", out.RunnerLiveness)
	}
	if len(out.Warnings) == 0 || !strings.Contains(out.Warnings[0], "inconclusive") {
		t.Errorf("warnings %v must disclose the inconclusive probe", out.Warnings)
	}
	if !out.Transitioned {
		t.Error("the reap must still proceed on an inconclusive probe")
	}
}

// (B) TestReapStage_RaceStageAdvancedUnderProbeRefused is the counterfactual
// vehicle for detector (1) of confirmStrandBeforeReap — the pre/post stage-state
// comparison.
//
// It drives the exact interleaving the fixed-verdict tests could not: the stage
// is 'dispatched', the probe answers DEAD (no runner exists yet), and DURING
// that probe a concurrent dispatch spawns a runner and advances the stage to
// 'running'. The endpoint accepts both states, so without the comparison the
// POST would fail the newly-live runner's stage.
//
// Detector (2) is deliberately UNARMED here (the probe answers dead on every
// call), so the refusal can only come from the state comparison.
func TestReapStage_RaceStageAdvancedUnderProbeRefused(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID, stageID := uuid.New(), uuid.New()
	seedLocalRun(fb, runID)
	seedStrandedStage(fb, runID, stageID.String(), "dispatched")

	r := newResolver(srv, nil)
	probeCalls := 0
	r.driveProbeRunnerLiveness = func(context.Context, string) runnerLivenessVerdict {
		probeCalls++
		if probeCalls == 1 {
			// The concurrent dispatch lands between the classification and the
			// reap: a runner is spawned and the stage advances.
			setStageState(fb, runID, "running")
		}
		return runnerDead
	}

	_, _, err := r.reapStage(context.Background(), nil, ReapStageInput{
		RunID: runID.String(), StageID: stageID.String(),
	})
	if err == nil {
		t.Fatal("a stage that advanced dispatched -> running under the probe must refuse the reap")
	}
	if !strings.Contains(err.Error(), `"dispatched"`) || !strings.Contains(err.Error(), `"running"`) {
		t.Errorf("the refusal must name both observed states: %v", err)
	}
	if got := reapHops(fb); got != 0 {
		t.Errorf("reap-failure POSTs = %d, want 0 (a stage that moved must never reach the backend)", got)
	}
	// COMMITTED STATE, not just the error identity: the stage the concurrent
	// dispatcher advanced is still 'running', never failed by this call.
	if state, ok := r.observeStageState(context.Background(), runID, stageID.String()); !ok || state != "running" {
		t.Errorf("stage state = %q (observed=%v), want the untouched running", state, ok)
	}
}

// (B) TestReapStage_RaceRunnerAppearedBeforePostRefused is the counterfactual
// vehicle for detector (2) — the re-probe.
//
// The stage state is 'running' for the WHOLE call (the state comparison is
// unarmed by construction: the observed value is paired with ITSELF), so the
// only thing that can refuse is the second probe finding the runner a
// concurrent dispatch spawned after the first probe answered dead.
func TestReapStage_RaceRunnerAppearedBeforePostRefused(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID, stageID := uuid.New(), uuid.New()
	seedLocalRun(fb, runID)
	seedStrandedStage(fb, runID, stageID.String(), "running")

	r := newResolver(srv, nil)
	probeCalls := 0
	r.driveProbeRunnerLiveness = func(context.Context, string) runnerLivenessVerdict {
		probeCalls++
		if probeCalls == 1 {
			return runnerDead
		}
		return runnerLive
	}

	_, _, err := r.reapStage(context.Background(), nil, ReapStageInput{
		RunID: runID.String(), StageID: stageID.String(),
	})
	if err == nil {
		t.Fatal("a runner that appeared between the classification and the reap must refuse it")
	}
	if !strings.Contains(err.Error(), stageID.String()) || !strings.Contains(err.Error(), "between the liveness classification and the reap") {
		t.Errorf("the refusal must name the stage and the window it closed: %v", err)
	}
	if probeCalls != 2 {
		t.Errorf("probe invoked %d times, want 2 (classification + the pre-POST re-probe)", probeCalls)
	}
	if got := reapHops(fb); got != 0 {
		t.Errorf("reap-failure POSTs = %d, want 0 (a live runner must never reach the backend)", got)
	}
	if state, ok := r.observeStageState(context.Background(), runID, stageID.String()); !ok || state != "running" {
		t.Errorf("stage state = %q (observed=%v), want the untouched running", state, ok)
	}
}

// (B) TestReapStage_UnracedStrandStillReaped is the PAIRED CONTROL for both
// detectors: a genuinely stranded stage — state unchanged across the call, both
// probes dead — is still reaped. Without it the two refusals above could be
// satisfied by a verb that refuses everything.
func TestReapStage_UnracedStrandStillReaped(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID, stageID := uuid.New(), uuid.New()
	seedLocalRun(fb, runID)
	seedStrandedStage(fb, runID, stageID.String(), "running")
	r, probeCalls := reapResolver(t, srv, runnerDead)

	_, out, err := r.reapStage(context.Background(), nil, ReapStageInput{
		RunID: runID.String(), StageID: stageID.String(),
	})
	if err != nil {
		t.Fatalf("an unraced strand must still be reapable: %v", err)
	}
	if !out.Transitioned || out.StageState != "failed" {
		t.Errorf("transitioned=%v stage_state=%q, want true/failed", out.Transitioned, out.StageState)
	}
	if *probeCalls != 2 {
		t.Errorf("probe invoked %d times, want 2 (classification + the pre-POST re-probe)", *probeCalls)
	}
	if got := reapHops(fb); got != 1 {
		t.Errorf("reap-failure POSTs = %d, want 1", got)
	}
}

// (B) TestReapStage_ReconfirmNeverProbesANonLocalRun: the pre-POST re-probe
// inherits the runner_kind GATE. Re-probing a github_actions run would
// fabricate exactly the host verdict classifyReapLivenessGated withholds.
func TestReapStage_ReconfirmNeverProbesANonLocalRun(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID, stageID := uuid.New(), uuid.New()
	fb.mu.Lock()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", RunnerKind: "github_actions"}
	fb.mu.Unlock()
	seedStrandedStage(fb, runID, stageID.String(), "running")
	r, probeCalls := reapResolver(t, srv, runnerLive)

	_, out, err := r.reapStage(context.Background(), nil, ReapStageInput{
		RunID: runID.String(), StageID: stageID.String(),
	})
	if err != nil {
		t.Fatalf("reapStage: %v", err)
	}
	if *probeCalls != 0 {
		t.Errorf("the host probe ran %d times for a github_actions run, want 0 on BOTH the classification and the re-confirmation", *probeCalls)
	}
	if out.RunnerLiveness != "unknown" {
		t.Errorf("runner_liveness = %q, want unknown", out.RunnerLiveness)
	}
	if got := reapHops(fb); got != 1 {
		t.Errorf("reap-failure POSTs = %d, want 1", got)
	}
}

// (B) TestReapStage_UnobservableStageFailsOpen: an unreadable stage list leaves
// the state comparison UNARMED and the reap proceeds — the documented fail-open
// posture, since refusing a recovery verb on a transient read error would
// re-strand the stage it exists to clear.
func TestReapStage_UnobservableStageFailsOpen(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID, stageID := uuid.New(), uuid.New()
	seedLocalRun(fb, runID)
	fb.mu.Lock()
	fb.stagesStatusByRun[runID] = http.StatusInternalServerError
	fb.mu.Unlock()
	r, _ := reapResolver(t, srv, runnerDead)

	_, out, err := r.reapStage(context.Background(), nil, ReapStageInput{
		RunID: runID.String(), StageID: stageID.String(),
	})
	if err != nil {
		t.Fatalf("an unreadable stage list must not refuse the reap: %v", err)
	}
	if !out.Transitioned || out.StageState != "failed" {
		t.Errorf("transitioned=%v stage_state=%q, want true/failed", out.Transitioned, out.StageState)
	}
}

// (A) TestReapStage_StablePendingRefused is the counterfactual vehicle for the
// stable-pending refusal (E67.52 / #2700). A stage seeded 'pending' — the bad
// state written BY CONSTRUCTION, so the RED lands on the behavioural assertion
// and not on fixture setup — is refused BEFORE the liveness probe and the HTTP
// hop: the refusal names 'pending' and fishhawk_dispatch_stage, the injected
// probe ran ZERO times (proving the guard precedes the classification, not
// merely the POST), and the never-dialed seam recorded zero POSTs. Deleting the
// guard lets the fake accept the POST and return transitioned:true — the
// documented RED.
func TestReapStage_StablePendingRefused(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID, stageID := uuid.New(), uuid.New()
	// A local run so, WITHOUT the guard, the classification would reach the probe
	// — making the "probe ran zero times" assertion a real discriminator.
	seedLocalRun(fb, runID)
	seedStrandedStage(fb, runID, stageID.String(), "pending")
	r, probeCalls := reapResolver(t, srv, runnerDead)

	_, _, err := r.reapStage(context.Background(), nil, ReapStageInput{
		RunID: runID.String(), StageID: stageID.String(),
	})
	if err == nil {
		t.Fatal("a stable pending stage must refuse the reap")
	}
	if !strings.Contains(err.Error(), `"pending"`) {
		t.Errorf("refusal must name the observed pending state: %v", err)
	}
	if !strings.Contains(err.Error(), "fishhawk_dispatch_stage") {
		t.Errorf("refusal must point at fishhawk_dispatch_stage to move a pending stage: %v", err)
	}
	// The claim is about the CURRENT ATTEMPT, not the stage's lifetime — the
	// refusal must not assert the stage "never started"/"never dispatched".
	if strings.Contains(err.Error(), "never dispatched") || strings.Contains(err.Error(), "never started") {
		t.Errorf("refusal must speak about the current attempt, not deny the stage's history: %v", err)
	}
	if *probeCalls != 0 {
		t.Errorf("probe invoked %d times, want 0 (the guard precedes the liveness classification)", *probeCalls)
	}
	if got := reapHops(fb); got != 0 {
		t.Errorf("reap-failure POSTs = %d, want 0 (a pending stage must never reach the backend)", got)
	}
}

// (B) TestReapStage_DispatchedAndRunningStillReaped is the PAIRED CONTROL that
// the pending guard is pending-SPECIFIC: the two genuinely-reapable strand
// states still reap to failed in exactly one POST. An over-broad guard (e.g.
// keying on != running instead of == pending) reddens the 'dispatched' arm.
func TestReapStage_DispatchedAndRunningStillReaped(t *testing.T) {
	for _, state := range []string{"dispatched", "running"} {
		t.Run(state, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			runID, stageID := uuid.New(), uuid.New()
			seedLocalRun(fb, runID)
			seedStrandedStage(fb, runID, stageID.String(), state)
			r, _ := reapResolver(t, srv, runnerDead)

			_, out, err := r.reapStage(context.Background(), nil, ReapStageInput{
				RunID: runID.String(), StageID: stageID.String(),
			})
			if err != nil {
				t.Fatalf("a %q strand must still be reapable: %v", state, err)
			}
			if !out.Transitioned || out.StageState != "failed" {
				t.Errorf("transitioned=%v stage_state=%q, want true/failed", out.Transitioned, out.StageState)
			}
			if got := reapHops(fb); got != 1 {
				t.Errorf("reap-failure POSTs = %d, want 1", got)
			}
		})
	}
}

// (C) TestReapStage_UnreadableStageDoesNotTriggerPendingRefusal pins the pending
// guard's FAIL-OPEN posture distinctly from the pre-existing
// TestReapStage_UnobservableStageFailsOpen (which pins the same posture for the
// race detectors — cross-referenced here rather than duplicated): an unreadable
// stage list leaves the observation unarmed (beforeKnown false), so the pending
// guard cannot fire and the reap still lands.
func TestReapStage_UnreadableStageDoesNotTriggerPendingRefusal(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID, stageID := uuid.New(), uuid.New()
	seedLocalRun(fb, runID)
	fb.mu.Lock()
	fb.stagesStatusByRun[runID] = http.StatusInternalServerError
	fb.mu.Unlock()
	r, _ := reapResolver(t, srv, runnerDead)

	_, out, err := r.reapStage(context.Background(), nil, ReapStageInput{
		RunID: runID.String(), StageID: stageID.String(),
	})
	if err != nil {
		t.Fatalf("an unreadable stage list must not refuse the reap: %v", err)
	}
	if !out.Transitioned || out.StageState != "failed" {
		t.Errorf("transitioned=%v stage_state=%q, want true/failed", out.Transitioned, out.StageState)
	}
}

// (D) TestReapStage_DescriptionStatesTrueReapAuthority is the done-means pin on a
// prose-correctness change compilation cannot enforce: the extracted
// reapStageDescription const must NAME 'pending' and the local refusal, and must
// NOT carry the old absolute authority phrasing. Per the operator's binding
// CONDITION 2 the assertions are LOOSE — they pin the CLAIM, not any whole
// sentence, so a future accurate rewording does not redden the test — and the
// ABSENCE half is load-bearing: it catches a partial correction that adds the
// new claim while leaving the old false one standing.
func TestReapStage_DescriptionStatesTrueReapAuthority(t *testing.T) {
	d := reapStageDescription
	if !strings.Contains(d, "pending") {
		t.Error("description must name the pending state the verb refuses")
	}
	if !strings.Contains(d, "locally") {
		t.Error("description must state the pending refusal is a LOCAL refusal")
	}
	// The absence half: neither the old absolute authority sentence nor its
	// {dispatched, running}-only variant may remain.
	for _, banned := range []string{"authority is exactly", "exactly {dispatched, running}"} {
		if strings.Contains(d, banned) {
			t.Errorf("description still carries the old absolute phrasing %q", banned)
		}
	}
}

// (D) TestReapStage_BackendRefusalSurfaces: the server stays the authority on
// what may be reaped, and each of its refusals reaches the operator carrying
// the backend's own code.
func TestReapStage_BackendRefusalSurfaces(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{
			name:   "protected_park",
			status: http.StatusConflict,
			body:   `{"error":{"code":"stage_not_reapable","message":"stage is parked awaiting_approval"}}`,
			want:   "stage_not_reapable",
		},
		{
			name:   "stage_not_found",
			status: http.StatusNotFound,
			body:   `{"error":{"code":"stage_not_found","message":"no such stage"}}`,
			want:   "stage_not_found",
		},
		{
			name:   "insufficient_scope",
			status: http.StatusForbidden,
			body:   `{"error":{"code":"insufficient_scope","message":"token lacks write:runs"}}`,
			want:   "insufficient_scope",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			runID, stageID := uuid.New(), uuid.New()
			fb.mu.Lock()
			fb.reapFailureStatus = tc.status
			fb.reapFailureErrBody = tc.body
			fb.mu.Unlock()
			r, _ := reapResolver(t, srv, runnerDead)

			_, _, err := r.reapStage(context.Background(), nil, ReapStageInput{
				RunID: runID.String(), StageID: stageID.String(),
			})
			if err == nil {
				t.Fatalf("expected the backend %d refusal to surface", tc.status)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error must carry the backend code %q: %v", tc.want, err)
			}
		})
	}
}

// (D) TestReapStage_IdempotentNoOp: the endpoint's already-terminal no-op is
// mirrored, not turned into an error — a second reap of an already-failed stage
// is a legitimate operator retry.
func TestReapStage_IdempotentNoOp(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID, stageID := uuid.New(), uuid.New()
	fb.mu.Lock()
	fb.reapFailureRespBody = `{"transitioned":false,"stage_state":"failed"}`
	fb.mu.Unlock()
	r, _ := reapResolver(t, srv, runnerDead)

	_, out, err := r.reapStage(context.Background(), nil, ReapStageInput{
		RunID: runID.String(), StageID: stageID.String(),
	})
	if err != nil {
		t.Fatalf("the idempotent no-op must not be an error: %v", err)
	}
	if out.Transitioned {
		t.Error("transitioned = true, want the mirrored false")
	}
	if out.StageState != "failed" {
		t.Errorf("stage_state = %q, want failed", out.StageState)
	}
	if out.NextStep == "" || !strings.Contains(out.NextStep, "fishhawk_retry_stage") {
		t.Errorf("next_step must name fishhawk_retry_stage: %q", out.NextStep)
	}
}

// (E) TestReapStage_EndToEndOverHTTP drives the REAL registered MCP tool ->
// the REAL apiClient -> a REAL HTTP server, asserting the request METHOD, PATH
// and BODY on the wire and the decoded result coming back.
//
// SCOPE OF WHAT THIS PROVES, stated plainly: this is the RECOVERY half only. It
// SEEDS a stage as already stranded in 'running'. The strand-PRODUCTION half —
// a real child exit producing that state — is driven by
// TestRunChildren_ChildExitsNonZeroWithoutSettling_ReapStageRecovers in
// run_children_test.go, which composes production and recovery in one test.
func TestReapStage_EndToEndOverHTTP(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	// The server's view of the stage: 'running' before the reap, 'failed' after
	// — so the test asserts the verb is what moves a stranded-running stage into
	// the state fishhawk_retry_stage admits.
	stageState := "running"
	type captured struct {
		method string
		path   string
		body   reapFailureRequest
	}
	var got captured
	mux := http.NewServeMux()
	mux.HandleFunc("/v0/runs/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/reap-failure") {
			got.method, got.path = r.Method, r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&got.body)
			stageState = "failed"
			_, _ = w.Write([]byte(`{"transitioned":true,"stage_state":"failed"}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/stages") {
			// The pre/post state reads confirmStrandBeforeReap takes cross HTTP
			// here too, serving the stage's live state.
			_, _ = w.Write([]byte(`{"items":[{"id":"` + stageID.String() +
				`","run_id":"` + runID.String() + `","type":"implement","state":"` + stageState + `"}]}`))
			return
		}
		// GET /v0/runs/{id}: a runner_kind=local run, so the gate reaches the
		// probe — which the resolver seam answers dead (no pgrep involved).
		_, _ = w.Write([]byte(`{"id":"` + runID.String() + `","runner_kind":"local","state":"running"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if stageState != "running" {
		t.Fatalf("fixture: stage must start stranded in running, got %q", stageState)
	}

	raw := callBoundedToolOverSDK(t, srv, nil, func(s *mcp.Server, res *runResolver) {
		res.driveProbeRunnerLiveness = func(context.Context, string) runnerLivenessVerdict { return runnerDead }
		registerReapStage(s, res)
	}, "fishhawk_reap_stage", map[string]any{
		"run_id":    runID.String(),
		"stage_id":  stageID.String(),
		"category":  "C",
		"reason":    "operator_reap_stranded_stage",
		"detail":    "runner exited 7 without settling",
		"exit_code": 7,
	})

	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	wantPath := "/v0/runs/" + runID.String() + "/stages/" + stageID.String() + "/reap-failure"
	if got.path != wantPath {
		t.Errorf("path = %q, want %q", got.path, wantPath)
	}
	if got.body.Category != "C" || got.body.Reason != "operator_reap_stranded_stage" {
		t.Errorf("body category/reason = %q/%q", got.body.Category, got.body.Reason)
	}
	if got.body.Detail != "runner exited 7 without settling" || got.body.ExitCode != 7 {
		t.Errorf("body detail/exit_code = %q/%d", got.body.Detail, got.body.ExitCode)
	}

	var out ReapStageOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Transitioned || out.StageState != "failed" {
		t.Errorf("transitioned=%v stage_state=%q, want true/failed", out.Transitioned, out.StageState)
	}
	if stageState != "failed" {
		t.Errorf("the server's stage is %q — the verb did not move the stranded stage", stageState)
	}
	if out.RunnerLiveness != "dead" {
		t.Errorf("runner_liveness = %q, want dead", out.RunnerLiveness)
	}
	if !strings.Contains(out.NextStep, "fishhawk_retry_stage") {
		t.Errorf("next_step must name fishhawk_retry_stage: %q", out.NextStep)
	}
}
