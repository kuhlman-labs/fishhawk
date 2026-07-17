package main

import (
	"context"
	"strings"
	"testing"

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

// Unknown-kind posture (E45.7): a run LOCKED to a kind fishhawkd does not yet
// recognize (a future gitlab_ci before its backend registers) keeps ALLOWING
// here — the opposite posture from the host-dispatch endpoint, which rejects
// unknown resolved kinds. KindHostDispatched reports (false, known=false) for
// such a kind, so the guard's `known && !hostDispatched` block does not fire.
// This pins that a future registry addition cannot silently flip the MCP guard.
func TestGuardHostDispatch_UnknownLockedKind_Allows(t *testing.T) {
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
	if err != nil {
		t.Fatalf("an unknown locked kind must be allowed (opposite posture from the endpoint), got %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("the allow path carries no warnings, got %v", warnings)
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
// a second spawn would double-drive).
func TestGuardSiblingInFlight_TargetRunning_Blocks(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	targetID := uuid.NewString()
	fb.stagesByRun[runID] = []Stage{
		{ID: targetID, RunID: runID.String(), Type: "implement", State: "running"},
	}

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
