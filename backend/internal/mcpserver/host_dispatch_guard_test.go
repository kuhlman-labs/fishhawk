package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The host-dispatch runner_kind guardrail (#1355) has four enumerated branches.
// Each gets its own assertion here, driving guardHostDispatch directly through
// the real GET /v0/runs round-trip on the fake backend (api client -> MCP Run
// decode -> guard), so the read-surface wire contract is exercised end-to-end.

// (3) locked + github_actions => actionable error, no spawn-permission.
func TestGuardHostDispatch_LockedGitHubActions_Blocks(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	fb.getRunByID[runID] = Run{
		ID:                 runID.String(),
		State:              "running",
		RunnerKind:         "github_actions",
		RunnerKindResolved: true,
	}

	warnings, err := r.guardHostDispatch(context.Background(), runID)
	if err == nil {
		t.Fatal("expected a block error for a github_actions-locked run")
	}
	// Actionable error (approval condition 3): names the locked kind AND the
	// corrective action.
	msg := err.Error()
	if !strings.Contains(msg, "github_actions") {
		t.Errorf("error must name the locked kind: %v", err)
	}
	if !strings.Contains(msg, "runner_kind=local") {
		t.Errorf("error must name the corrective action (start a local run): %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("a hard block carries no warnings, got %v", warnings)
	}
}

// (locked + local) => allow: a host dispatch matches the resolved local channel.
func TestGuardHostDispatch_LockedLocal_Allows(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	fb.getRunByID[runID] = Run{
		ID:                 runID.String(),
		State:              "running",
		RunnerKind:         "local",
		RunnerKindResolved: true,
	}

	warnings, err := r.guardHostDispatch(context.Background(), runID)
	if err != nil {
		t.Fatalf("a local-locked run must be allowed, got %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("the allow path carries no warnings, got %v", warnings)
	}
}

// (1) un-resolved run (any kind) => allow, so first-dispatch auto-resolve still
// fires (#1346 decision-1). A premature block here re-creates the #1344 wedge.
func TestGuardHostDispatch_Unresolved_Allows(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	// runner_kind reads github_actions (the create-time default hint) but the
	// run is NOT yet locked — it must still be allowed to dispatch locally.
	fb.getRunByID[runID] = Run{
		ID:                 runID.String(),
		State:              "running",
		RunnerKind:         "github_actions",
		RunnerKindResolved: false,
	}

	warnings, err := r.guardHostDispatch(context.Background(), runID)
	if err != nil {
		t.Fatalf("an un-resolved run must be allowed (first dispatch auto-resolves), got %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("the allow path carries no warnings, got %v", warnings)
	}
}

// (2) GetRun error => FAIL OPEN: nil error + a warning, never strand a
// legitimate local dispatch (approval condition 2; defense-in-depth).
func TestGuardHostDispatch_GetRunError_FailsOpen(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	fb.getStatusByID[runID] = 500

	warnings, err := r.guardHostDispatch(context.Background(), runID)
	if err != nil {
		t.Fatalf("a GetRun error must FAIL OPEN (nil error), got %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("the fail-open path must surface a warning")
	}
	if !strings.Contains(strings.Join(warnings, " "), "guard skipped") {
		t.Errorf("warning should explain the guard was skipped, got %v", warnings)
	}
}

// (3, gitlab_ci) locked + gitlab_ci => actionable error, no spawn-permission.
// Once the gitlab_ci backend registers (#1861), gitlab_ci is a KNOWN non-host
// kind (KindHostDispatched reports (false, known=true)), so the guard's
// `known && !hostDispatched` block NOW fires — a host (local) dispatch against a
// gitlab_ci-locked run is a channel mismatch. This is the flip of the former
// unknown-kind ALLOW: a registry addition changed the posture deliberately, and
// this test pins the new BLOCK so it cannot silently regress.
func TestGuardHostDispatch_LockedGitLabCI_Blocks(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	fb.getRunByID[runID] = Run{
		ID:                 runID.String(),
		State:              "running",
		RunnerKind:         "gitlab_ci",
		RunnerKindResolved: true,
	}

	warnings, err := r.guardHostDispatch(context.Background(), runID)
	if err == nil {
		t.Fatal("expected a block error for a gitlab_ci-locked run")
	}
	msg := err.Error()
	if !strings.Contains(msg, "gitlab_ci") {
		t.Errorf("error must name the locked kind: %v", err)
	}
	if !strings.Contains(msg, "runner_kind=local") {
		t.Errorf("error must name the corrective action (start a local run): %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("a hard block carries no warnings, got %v", warnings)
	}
}

// The sibling-in-flight admission guard (#1872) has six enumerated branches;
// each gets its own assertion driving guardSiblingStageInFlight directly through
// the real GET /v0/runs/{run_id}/stages round-trip on the fake backend.

// A sibling stage in "running" blocks the dispatch (the incident shape:
// acceptance dispatched while implement was still shipping).
func TestGuardSiblingInFlight_SiblingRunning_Blocks(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	targetID := uuid.NewString()
	siblingID := uuid.NewString()
	fb.stagesByRun[runID] = []Stage{
		{ID: siblingID, RunID: runID.String(), Type: "implement", State: "running"},
		{ID: targetID, RunID: runID.String(), Type: "acceptance", State: "pending"},
	}

	warnings, err := r.guardSiblingStageInFlight(context.Background(), runID, targetID)
	if err == nil {
		t.Fatal("expected a block error when a sibling stage is running")
	}
	msg := err.Error()
	if !strings.Contains(msg, "implement") || !strings.Contains(msg, "running") {
		t.Errorf("error must name the in-flight sibling type and state: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("a hard block carries no warnings, got %v", warnings)
	}
}

// A sibling stage in "dispatched" blocks (a local runner is about to spawn).
func TestGuardSiblingInFlight_SiblingDispatched_Blocks(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	targetID := uuid.NewString()
	siblingID := uuid.NewString()
	fb.stagesByRun[runID] = []Stage{
		{ID: siblingID, RunID: runID.String(), Type: "implement", State: "dispatched"},
		{ID: targetID, RunID: runID.String(), Type: "acceptance", State: "pending"},
	}

	_, err := r.guardSiblingStageInFlight(context.Background(), runID, targetID)
	if err == nil {
		t.Fatal("expected a block error when a sibling stage is dispatched")
	}
	if !strings.Contains(err.Error(), "dispatched") {
		t.Errorf("error must name the sibling's dispatched state: %v", err)
	}
}

// The TARGET stage itself in "running" blocks (a live runner already owns it;
// a second spawn would double-drive). Since #2689 the WORDING branches on the
// host-liveness verdict, so this case pins the LIVE arm explicitly: a
// runner_kind=local run whose probe finds a live process keeps the pre-#2689
// double-drive text verbatim.
func TestGuardSiblingInFlight_TargetRunning_Blocks(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	targetID := uuid.NewString()
	fb.getRunByID[runID] = Run{ID: runID.String(), RunnerKind: driveRunnerKindLocal}
	fb.stagesByRun[runID] = []Stage{
		{ID: targetID, RunID: runID.String(), Type: "implement", State: "running"},
	}
	r.driveProbeRunnerLiveness = func(context.Context, string) runnerLivenessVerdict { return runnerLive }

	_, err := r.guardSiblingStageInFlight(context.Background(), runID, targetID)
	if err == nil {
		t.Fatal("expected a block error when the target stage is already running")
	}
	if !strings.Contains(err.Error(), "double-drive") {
		t.Errorf("error must explain the double-drive hazard: %v", err)
	}
}

// The TARGET stage merely "dispatched" with every sibling settled is ALLOWED —
// this is the local retry/fixup park-then-spawn state.
func TestGuardSiblingInFlight_TargetDispatchedSiblingsSettled_Allows(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	targetID := uuid.NewString()
	siblingID := uuid.NewString()
	fb.stagesByRun[runID] = []Stage{
		{ID: siblingID, RunID: runID.String(), Type: "plan", State: "succeeded"},
		{ID: targetID, RunID: runID.String(), Type: "implement", State: "dispatched"},
	}

	warnings, err := r.guardSiblingStageInFlight(context.Background(), runID, targetID)
	if err != nil {
		t.Fatalf("the target's own dispatched park state must be allowed (retry/fixup re-dispatch), got %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("the allow path carries no warnings, got %v", warnings)
	}
}

// #1912: a SIBLING parked at awaiting_host_dispatch is NOT in-flight (no spawn
// attempt exists yet), so it must NOT block the target dispatch — only
// {dispatched, running} siblings do.
func TestGuardSiblingInFlight_SiblingAwaitingHostDispatch_Allows(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	targetID := uuid.NewString()
	siblingID := uuid.NewString()
	fb.stagesByRun[runID] = []Stage{
		{ID: siblingID, RunID: runID.String(), Type: "acceptance", State: "awaiting_host_dispatch"},
		{ID: targetID, RunID: runID.String(), Type: "implement", State: "pending"},
	}

	warnings, err := r.guardSiblingStageInFlight(context.Background(), runID, targetID)
	if err != nil {
		t.Fatalf("a sibling at awaiting_host_dispatch is not in-flight and must not block, got %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("the allow path carries no warnings, got %v", warnings)
	}
}

// #1912: the target's OWN awaiting_host_dispatch park (the plan-approved / retry /
// fixup local park) is ALLOWED — it is exactly the state the host-dispatch verbs
// spawn from; blocking it would wedge every local dispatch.
func TestGuardSiblingInFlight_TargetAwaitingHostDispatch_Allows(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	targetID := uuid.NewString()
	siblingID := uuid.NewString()
	fb.stagesByRun[runID] = []Stage{
		{ID: siblingID, RunID: runID.String(), Type: "plan", State: "succeeded"},
		{ID: targetID, RunID: runID.String(), Type: "implement", State: "awaiting_host_dispatch"},
	}

	warnings, err := r.guardSiblingStageInFlight(context.Background(), runID, targetID)
	if err != nil {
		t.Fatalf("the target's own awaiting_host_dispatch park must be allowed, got %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("the allow path carries no warnings, got %v", warnings)
	}
}

// All stages settled (terminal / awaiting_approval) is ALLOWED — the happy
// await-review-then-dispatch-acceptance boundary once implement has settled.
func TestGuardSiblingInFlight_AllSettled_Allows(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	targetID := uuid.NewString()
	implementID := uuid.NewString()
	fb.stagesByRun[runID] = []Stage{
		{ID: implementID, RunID: runID.String(), Type: "implement", State: "awaiting_approval"},
		{ID: targetID, RunID: runID.String(), Type: "acceptance", State: "pending"},
	}

	_, err := r.guardSiblingStageInFlight(context.Background(), runID, targetID)
	if err != nil {
		t.Fatalf("all-settled siblings must allow the dispatch, got %v", err)
	}
}

// The TARGET stage itself parked "awaiting_children" is BLOCKED (#1891): it is
// a decomposed parent's implement stage waiting on its child slices; spawning a
// runner here 409s and the reaper report would destroy the park. The refusal
// must name fishhawk_run_children / fishhawk_consolidate_slices.
func TestGuardSiblingInFlight_TargetAwaitingChildren_Blocks(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	targetID := uuid.NewString()
	siblingID := uuid.NewString()
	fb.stagesByRun[runID] = []Stage{
		{ID: siblingID, RunID: runID.String(), Type: "plan", State: "succeeded"},
		{ID: targetID, RunID: runID.String(), Type: "implement", State: "awaiting_children"},
	}

	warnings, err := r.guardSiblingStageInFlight(context.Background(), runID, targetID)
	if err == nil {
		t.Fatal("expected a block error when the target stage is parked awaiting_children")
	}
	msg := err.Error()
	if !strings.Contains(msg, "awaiting_children") {
		t.Errorf("error must name the awaiting_children park: %v", err)
	}
	if !strings.Contains(msg, "fishhawk_run_children") {
		t.Errorf("error must name fishhawk_run_children as the correct verb: %v", err)
	}
	if !strings.Contains(msg, "fishhawk_consolidate_slices") {
		t.Errorf("error must name fishhawk_consolidate_slices for the final fan-in: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("a hard block carries no warnings, got %v", warnings)
	}
}

// A stage-list read error FAILS OPEN: nil error + a warning, mirroring the
// #1355 guardHostDispatch posture (the multi-key Verify fix is the backstop).
func TestGuardSiblingInFlight_ListError_FailsOpen(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	fb.stagesStatus = 500

	warnings, err := r.guardSiblingStageInFlight(context.Background(), runID, uuid.NewString())
	if err != nil {
		t.Fatalf("a stage-list read error must FAIL OPEN (nil error), got %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("the fail-open path must surface a warning")
	}
	if !strings.Contains(strings.Join(warnings, " "), "guard skipped") {
		t.Errorf("warning should explain the guard was skipped, got %v", warnings)
	}
}

// TestGuardSiblingStageInFlight_TargetRunningRefusesAcrossAllVerdicts is the
// counterfactual vehicle for the #2689 message branch AND the pin on the single
// most important safety property in that change: the target-'running' refusal is
// UNCONDITIONAL across live / dead / unknown. Only the WORDING moves, so a
// misclassified probe can change prose and never a permission — the guard can
// never ADMIT a dispatch it would otherwise block.
func TestGuardSiblingStageInFlight_TargetRunningRefusesAcrossAllVerdicts(t *testing.T) {
	cases := []struct {
		name        string
		runnerKind  string
		verdict     runnerLivenessVerdict
		wantProbed  bool
		wantsReap   bool
		wantPhrases []string
	}{
		{
			name: "live", runnerKind: driveRunnerKindLocal, verdict: runnerLive, wantProbed: true,
			wantsReap: false, wantPhrases: []string{"double-drive", "Wait for it to settle"},
		},
		{
			name: "dead", runnerKind: driveRunnerKindLocal, verdict: runnerDead, wantProbed: true,
			wantsReap: true, wantPhrases: []string{"STRANDED", "fishhawk_reap_stage", "fishhawk_retry_stage"},
		},
		{
			// A non-local runner_kind: classifyReapLiveness never probes and never
			// says dead, so the guard takes the UNKNOWN wording.
			name: "unknown", runnerKind: "github_actions", verdict: runnerDead, wantProbed: false,
			wantsReap: true, wantPhrases: []string{"could not be verified", "fishhawk_reap_stage", "fishhawk_retry_stage"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			r := newResolver(srv, nil)
			runID := uuid.New()
			targetID := uuid.NewString()
			fb.mu.Lock()
			fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", RunnerKind: tc.runnerKind}
			fb.stagesByRun[runID] = []Stage{
				{ID: targetID, RunID: runID.String(), Type: "implement", State: "running"},
			}
			fb.mu.Unlock()
			probed := 0
			r.driveProbeRunnerLiveness = func(context.Context, string) runnerLivenessVerdict {
				probed++
				return tc.verdict
			}

			warnings, err := r.guardSiblingStageInFlight(context.Background(), runID, targetID)
			// THE INVARIANT: refused in ALL three verdicts.
			if err == nil {
				t.Fatalf("verdict %s ADMITTED a dispatch against a 'running' target — the refusal must be unconditional", tc.name)
			}
			if len(warnings) != 0 {
				t.Errorf("a refusal carries no warnings; got %v", warnings)
			}
			if (probed > 0) != tc.wantProbed {
				t.Errorf("probe invoked %d times, wantProbed=%v", probed, tc.wantProbed)
			}
			for _, want := range tc.wantPhrases {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("verdict %s message must contain %q: %v", tc.name, want, err)
				}
			}
			// Only the dead/unknown wordings name the recovery; the live wording
			// must NOT, because re-dispatching or reaping a live runner is exactly
			// what the guard exists to prevent.
			if got := strings.Contains(err.Error(), "fishhawk_reap_stage"); got != tc.wantsReap {
				t.Errorf("verdict %s names fishhawk_reap_stage = %v, want %v: %v", tc.name, got, tc.wantsReap, err)
			}
		})
	}
}

// The push_and_open_pr=false decomposition-child guard (#2691) has five
// enumerated branches. Each gets its own assertion driving guardNoPRImplement
// directly, so the decision table is pinned independently of either call site.

// A decomposition child + implement + push_and_open_pr=false => BLOCK naming
// the remedy verb. This is the strand path: the child stamps
// push_to_shared_branch regardless of --no-pr, and the backend then waits
// forever for a /pull-request report the runner never sends.
func TestGuardNoPRImplement_DecomposedChild_Blocks(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	parent := uuid.New().String()
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "running", Repo: "x/y", DecomposedFrom: &parent}

	warnings, err := r.guardNoPRImplement(context.Background(), runID, "implement", false)
	if err == nil {
		t.Fatal("expected a block error for a decomposition child dispatched with push_and_open_pr=false")
	}
	msg := err.Error()
	if !strings.Contains(msg, "fishhawk_run_children") {
		t.Errorf("error must name the remedy verb: %v", err)
	}
	if !strings.Contains(msg, parent) {
		t.Errorf("error must name the parent run: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("a hard block carries no warnings, got %v", warnings)
	}
}

// A STANDALONE run + implement + push_and_open_pr=false => ALLOW. This is the
// E22.8/#406 commit-yourself flow: it stamps none of the three forward-gate
// flags, so its trace upload settles the stage. The whole value of scoping this
// guard is that it does not break this path.
func TestGuardNoPRImplement_Standalone_Allows(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "running", Repo: "x/y"}

	warnings, err := r.guardNoPRImplement(context.Background(), runID, "implement", false)
	if err != nil {
		t.Fatalf("a standalone --no-pr implement dispatch must be ALLOWED, got %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("an allowed standalone dispatch carries no warnings, got %v", warnings)
	}
}

// A decomposition child + PLAN stage + push_and_open_pr=false => ALLOW. Only an
// implement stage stamps a forward-gate flag.
func TestGuardNoPRImplement_NonImplementStage_Allows(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	parent := uuid.New().String()
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "running", Repo: "x/y", DecomposedFrom: &parent}

	warnings, err := r.guardNoPRImplement(context.Background(), runID, "plan", false)
	if err != nil {
		t.Fatalf("a plan stage must never be refused by this guard, got %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("got warnings %v, want none", warnings)
	}
}

// A decomposition child + implement + push_and_open_pr=TRUE => ALLOW with NO
// GetRun round-trip. The short-circuit is asserted through a counting backend
// rather than by the nil error alone: the fail-open branch would ALSO return a
// nil error, so only the request count discriminates "short-circuited" from
// "read the run and then failed open".
func TestGuardNoPRImplement_PushAndOpenPRTrue_ShortCircuitsWithoutRoundTrip(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	r := &runResolver{
		api:    newAPIClient(config{backendURL: srv.URL, apiToken: "tok-test"}),
		getenv: func(string) string { return "" },
	}

	warnings, err := r.guardNoPRImplement(context.Background(), uuid.New(), "implement", true)
	if err != nil {
		t.Fatalf("push_and_open_pr=true must be allowed, got %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("the short-circuit emits no warnings, got %v", warnings)
	}
	if calls != 0 {
		t.Errorf("backend requests = %d, want 0 — the common path must cost no round-trip", calls)
	}
}

// A GetRun error => FAIL OPEN: nil error plus exactly one warning naming the
// skipped guard. Deliberate (approval condition 1): on an unreadable run row
// this layer cannot know the stage is a child, and refusing would break
// legitimate dispatches during a backend hiccup. This is the ONE branch where a
// runner process may start — the runner-side refusal then fires before the
// agent is invoked, so no agent pass is burned.
func TestGuardNoPRImplement_GetRunError_FailsOpen(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	fb.getStatusByID[runID] = 500

	warnings, err := r.guardNoPRImplement(context.Background(), runID, "implement", false)
	if err != nil {
		t.Fatalf("a GetRun error must FAIL OPEN (nil error), got %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one fail-open warning", warnings)
	}
	if !strings.Contains(warnings[0], "guard skipped") {
		t.Errorf("warning should explain the guard was skipped, got %q", warnings[0])
	}
	if !strings.Contains(warnings[0], "backstop") {
		t.Errorf("warning should name the runner-side backstop, got %q", warnings[0])
	}
}

// --- Runner self-host bootstrap advisory (E64.5 / #3086) ---------------------
//
// guardRunnerSelfHost is ADVISORY-ONLY: it returns []string with NO error, so
// it can never block a dispatch. Each enumerated mode gets its own assertion,
// driving the guard through the real fake-backend round trip (api client -> MCP
// decode -> guard) so the plan-resolution read surface is exercised end to end.

// planScopeForRun seeds a run with a plan stage carrying a standard_v1 plan
// artifact whose scope.files are the given paths, so guardRunnerSelfHost can
// resolve them through the real ListRunStages -> ListStageArtifacts path.
func planScopeForRun(fb *fakeBackend, runID uuid.UUID, paths ...string) {
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
		{ID: uuid.New().String(), RunID: runID.String(), Type: "implement", State: "pending"},
	}
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "running", Repo: "x/y"}
	content := samplePlanContent()
	files := make([]PlanScopeFile, 0, len(paths))
	for _, p := range paths {
		files = append(files, PlanScopeFile{Path: p, Operation: "modify"})
	}
	content.Scope.Files = files
	seedPlanArtifact(fb, planStageID, content, time.Hour)
}

// (a) A scope naming a runner Go source file warns exactly once, and the single
// warning names runner/README.md AND the fix-up-budget consequence — asserted on
// the SHIPPED string, so a comment-only/no-op touch that keeps verify green
// still fails here.
func TestGuardRunnerSelfHost_RunnerScope_Warns(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	planScopeForRun(fb, runID, "runner/internal/agent/claudecode/claudecode.go")

	warnings := r.guardRunnerSelfHost(context.Background(), runID, "implement")
	if len(warnings) != 1 {
		t.Fatalf("want exactly one advisory, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "runner/README.md") {
		t.Errorf("advisory must point at runner/README.md, got %q", warnings[0])
	}
	if !strings.Contains(warnings[0], "fix-up budget") {
		t.Errorf("advisory must name the fix-up-budget consequence, got %q", warnings[0])
	}
	if !strings.Contains(warnings[0], "runner/internal/agent/claudecode/claudecode.go") {
		t.Errorf("advisory must name the offending scope path, got %q", warnings[0])
	}
}

// (b) A scope naming only backend/ and docs/ paths is silent.
func TestGuardRunnerSelfHost_NonRunnerScope_Silent(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	planScopeForRun(fb, runID,
		"backend/internal/mcpserver/host_dispatch_guard.go",
		"docs/ARCHITECTURE.md")

	warnings := r.guardRunnerSelfHost(context.Background(), runID, "implement")
	if len(warnings) != 0 {
		t.Errorf("a non-runner scope must be silent, got %v", warnings)
	}
}

// (c) A scope naming only runner/README.md and a runner *_test.go is silent —
// the deny-list mode (the issue's anti-noise criterion).
func TestGuardRunnerSelfHost_DocAndTestOnlyRunnerScope_Silent(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	planScopeForRun(fb, runID,
		"runner/README.md",
		"runner/cmd/fishhawk-runner/worktree_test.go")

	warnings := r.guardRunnerSelfHost(context.Background(), runID, "implement")
	if len(warnings) != 0 {
		t.Errorf("a doc+test-only runner scope must be silent (deny-list), got %v", warnings)
	}
}

// (d) A mixed scope (a runner test file plus one runner Go source file) warns
// exactly once — not once per matching file.
func TestGuardRunnerSelfHost_MixedScope_WarnsOnce(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	planScopeForRun(fb, runID,
		"runner/cmd/fishhawk-runner/worktree_test.go",
		"runner/cmd/fishhawk-runner/worktree.go")

	warnings := r.guardRunnerSelfHost(context.Background(), runID, "implement")
	if len(warnings) != 1 {
		t.Fatalf("a mixed scope must warn exactly once, got %d: %v", len(warnings), warnings)
	}
}

// (e) An artifact-list read error fails OPEN and silent: nil warnings, and the
// guard has no error return at all so a dispatch can never be stranded.
func TestGuardRunnerSelfHost_ArtifactReadError_FailsOpenSilent(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	planScopeForRun(fb, runID, "runner/cmd/fishhawk-runner/main.go")
	fb.artifactsStatus = http.StatusInternalServerError

	warnings := r.guardRunnerSelfHost(context.Background(), runID, "implement")
	if len(warnings) != 0 {
		t.Errorf("an artifact read error must fail open and silent, got %v", warnings)
	}
}

// (f) A run with no plan artifact at all is silent.
func TestGuardRunnerSelfHost_NoPlanArtifact_Silent(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	planStageID := uuid.New()
	// A plan stage exists but carries no plan artifact; the run has no parent.
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "running", Repo: "x/y"}

	warnings := r.guardRunnerSelfHost(context.Background(), runID, "implement")
	if len(warnings) != 0 {
		t.Errorf("a run with no plan artifact must be silent, got %v", warnings)
	}
}

// (g) A plan-stage dispatch is silent AND performs ZERO stage/artifact reads —
// the cost carve-out, pinned behaviorally on fb.stagesCalledByID.
func TestGuardRunnerSelfHost_PlanStage_SkipsReads(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	planScopeForRun(fb, runID, "runner/cmd/fishhawk-runner/main.go")

	warnings := r.guardRunnerSelfHost(context.Background(), runID, "plan")
	if len(warnings) != 0 {
		t.Errorf("a plan-stage dispatch must be silent, got %v", warnings)
	}
	fb.mu.Lock()
	reads := fb.stagesCalledByID[runID]
	fb.mu.Unlock()
	if reads != 0 {
		t.Errorf("a plan-stage dispatch must perform zero stage reads, got %d", reads)
	}
}

// (h) The run's own plan artifact is absent but its PARENT carries a
// runner-touching plan — the bounded parent walk resolves it and warns.
func TestGuardRunnerSelfHost_PlanOnParentRun_Warns(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parentID := uuid.New()
	childID := uuid.New()
	parentIDStr := parentID.String()
	// Child has only an implement stage (the CI-retry / decomposition-child shape).
	fb.stagesByRun[childID] = []Stage{
		{ID: uuid.New().String(), RunID: childID.String(), Type: "implement", State: "running"},
	}
	fb.getRunByID[childID] = Run{ID: childID.String(), ParentRunID: &parentIDStr, State: "running", Repo: "x/y"}
	// Parent carries the plan whose scope touches runner/.
	planScopeForRun(fb, parentID, "runner/internal/upload/upload.go")

	warnings := r.guardRunnerSelfHost(context.Background(), childID, "implement")
	if len(warnings) != 1 {
		t.Fatalf("the parent-walk must resolve the parent's runner-touching plan, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "runner/internal/upload/upload.go") {
		t.Errorf("advisory must name the parent plan's offending path, got %q", warnings[0])
	}
}

// TestRunnerBinaryAffectingScopePath pins the pure predicate: the runner/ prefix
// boundary (runnerx/main.go must NOT match), a runner Go source file (match), the
// deny-list (README.md and *_test.go, no match), and an embedded schema asset
// under runner/ (match — a //go:embed input genuinely changes the binary).
func TestRunnerBinaryAffectingScopePath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"runner/cmd/fishhawk-runner/main.go", true},
		{"runner/internal/upload/upload.go", true},
		{"runner/internal/plan/schemas/plan-standard-v1.schema.json", true},
		{"runnerx/main.go", false},
		{"runner-tools/main.go", false},
		{"runner/README.md", false},
		{"runner/cmd/fishhawk-runner/worktree_test.go", false},
		{"backend/internal/mcpserver/host_dispatch_guard.go", false},
		{"docs/ARCHITECTURE.md", false},
		{"./runner/cmd/fishhawk-runner/main.go", true},
	}
	for _, tc := range cases {
		if got := runnerBinaryAffectingScopePath(tc.path); got != tc.want {
			t.Errorf("runnerBinaryAffectingScopePath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// --- resolvePlanScopePathsForRun fail-open branches (E64.5 / #3086) -----------
//
// The resolver backs an ADVISORY that must never strand a dispatch, so EVERY
// error path returns nil with NO error. The artifact-list 500 mode is pinned by
// TestGuardRunnerSelfHost_ArtifactReadError_FailsOpenSilent above; these cases
// cover the remaining distinct fail-open branches the guard-boundary tests do not
// exercise, so a regression that turned any one of them into a non-nil error (or
// a panic) is caught here rather than surfacing as a stranded local dispatch.

// A stage-list read error (the ListRunStages failure inside tryGetPlanForRun)
// resolves to nil, not an error.
func TestResolvePlanScopePathsForRun_StageListError_ReturnsNil(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	fb.stagesStatusByRun[runID] = http.StatusInternalServerError

	if got := r.resolvePlanScopePathsForRun(context.Background(), runID); got != nil {
		t.Errorf("a stage-list read error must resolve to nil (fail-open), got %v", got)
	}
}

// A plan artifact whose content is valid JSON but does not decode into
// PlanContent (the json.Unmarshal failure inside tryGetPlanForRun) resolves to
// nil, not an error.
func TestResolvePlanScopePathsForRun_PlanDecodeError_ReturnsNil(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	planStageID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "running", Repo: "x/y"}
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	// scope must decode to an object; a string forces a json.Unmarshal type error
	// into PlanContent, exercising the decode-failure fail-open branch.
	v := "standard_v1"
	fb.mu.Lock()
	fb.artifactsByStage[planStageID] = []Artifact{{
		ID:            uuid.New().String(),
		StageID:       planStageID.String(),
		Kind:          "plan",
		SchemaVersion: &v,
		ContentHash:   "h",
		Content:       map[string]any{"plan_version": "standard_v1", "scope": "not-an-object"},
		CreatedAt:     time.Now().UTC(),
	}}
	fb.mu.Unlock()

	if got := r.resolvePlanScopePathsForRun(context.Background(), runID); got != nil {
		t.Errorf("a plan decode error must resolve to nil (fail-open), got %v", got)
	}
}

// A GetRun failure during the parent walk (the run has no plan of its own, so the
// walk reads its run row to find the parent, and that read 500s) resolves to nil.
func TestResolvePlanScopePathsForRun_ParentGetRunError_ReturnsNil(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	// No plan stage, so tryGetPlanForRun returns found=false and the walk reads
	// the run row next — which 500s.
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.NewString(), RunID: runID.String(), Type: "implement", State: "running"},
	}
	fb.getStatusByID[runID] = http.StatusInternalServerError

	if got := r.resolvePlanScopePathsForRun(context.Background(), runID); got != nil {
		t.Errorf("a parent-walk GetRun error must resolve to nil (fail-open), got %v", got)
	}
}

// An unparseable ParentRunID (the uuid.Parse failure in the walk) resolves to nil.
func TestResolvePlanScopePathsForRun_UnparseableParentID_ReturnsNil(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	bad := "not-a-uuid"
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.NewString(), RunID: runID.String(), Type: "implement", State: "running"},
	}
	fb.getRunByID[runID] = Run{ID: runID.String(), ParentRunID: &bad, State: "running", Repo: "x/y"}

	if got := r.resolvePlanScopePathsForRun(context.Background(), runID); got != nil {
		t.Errorf("an unparseable parent id must resolve to nil (fail-open), got %v", got)
	}
}

// A parent chain deeper than retryPlanChainDepth resolves to nil even though a
// runner-touching plan exists BEYOND the cap: the walk stops at the cap without
// reaching it, so the guard stays silent. This pins the cap-exhaustion branch
// specifically — a plan seeded one level past the cap is deliberately NOT found.
func TestResolvePlanScopePathsForRun_CapExhaustion_ReturnsNil(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	// Build a chain of retryPlanChainDepth+2 runs, each pointing at the next.
	ids := make([]uuid.UUID, retryPlanChainDepth+2)
	for i := range ids {
		ids[i] = uuid.New()
	}
	for i := 0; i < len(ids); i++ {
		fb.stagesByRun[ids[i]] = []Stage{
			{ID: uuid.NewString(), RunID: ids[i].String(), Type: "implement", State: "running"},
		}
		var parent *string
		if i+1 < len(ids) {
			p := ids[i+1].String()
			parent = &p
		}
		fb.getRunByID[ids[i]] = Run{ID: ids[i].String(), ParentRunID: parent, State: "running", Repo: "x/y"}
	}
	// Seed a runner-touching plan on the LAST run — one level beyond the cap, so
	// the walk from ids[0] must NOT reach it.
	last := ids[len(ids)-1]
	planScopeForRun(fb, last, "runner/internal/upload/upload.go")

	if got := r.resolvePlanScopePathsForRun(context.Background(), ids[0]); got != nil {
		t.Errorf("a chain deeper than retryPlanChainDepth must resolve to nil (cap-exhausted), got %v", got)
	}
}
