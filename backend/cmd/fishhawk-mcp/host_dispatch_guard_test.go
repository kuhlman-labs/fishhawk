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
