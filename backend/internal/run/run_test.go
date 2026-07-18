package run

import "testing"

// TestStageStateAwaitingHostDispatch_Classification pins the #1912 split of the
// conflated local 'dispatched' state. The new parked-for-host-dispatch state
// carries the exact wire value 'awaiting_host_dispatch' (audit log + JSON
// payloads carry it forever) and is classified settled-but-not-terminal — a
// parked judgment awaiting a host/operator spawn, mirroring awaiting_approval.
// These are behavioral done-means assertions (compilation does not enforce the
// classifier tables), companion to the exhaustive IsSettled/IsTerminal tables
// in transition_test.go.
func TestStageStateAwaitingHostDispatch_Classification(t *testing.T) {
	if got := string(StageStateAwaitingHostDispatch); got != "awaiting_host_dispatch" {
		t.Errorf("StageStateAwaitingHostDispatch = %q, want awaiting_host_dispatch", got)
	}
	if !StageStateAwaitingHostDispatch.IsSettled() {
		t.Error("awaiting_host_dispatch must be settled (parked for a host/operator spawn, #1912)")
	}
	if StageStateAwaitingHostDispatch.IsTerminal() {
		t.Error("awaiting_host_dispatch must NOT be terminal (a spawn/cancel still moves it forward)")
	}
	// It is distinct from 'dispatched', which now unambiguously means a spawn
	// attempt exists and stays in-flight (not settled).
	if StageStateAwaitingHostDispatch == StageStateDispatched {
		t.Error("awaiting_host_dispatch and dispatched must be distinct states")
	}
	if StageStateDispatched.IsSettled() {
		t.Error("dispatched must remain in-flight (not settled) after the #1912 split")
	}
}

// TestRunnerKindGitLabCI_Membership pins the additive gitlab_ci runner_kind
// member (ADR-058 / E45.8, #1861). The value carries the exact wire string
// 'gitlab_ci' (audit log + JSON payloads carry it forever) and is a member of
// the closed ValidRunnerKinds set alongside github_actions and local. This is
// the done-means assertion for the enum half of the plumbing slice; the
// migration 0054 CHECK widening that makes it persistable is pinned in
// postgres_test.go.
func TestRunnerKindGitLabCI_Membership(t *testing.T) {
	if RunnerKindGitLabCI != "gitlab_ci" {
		t.Errorf("RunnerKindGitLabCI = %q, want gitlab_ci", RunnerKindGitLabCI)
	}
	if _, ok := ValidRunnerKinds[RunnerKindGitLabCI]; !ok {
		t.Error("gitlab_ci must be a member of ValidRunnerKinds")
	}
	// The prior two kinds remain members (the widening is additive).
	if _, ok := ValidRunnerKinds[RunnerKindGitHubActions]; !ok {
		t.Error("github_actions must remain a member of ValidRunnerKinds")
	}
	if _, ok := ValidRunnerKinds[RunnerKindLocal]; !ok {
		t.Error("local must remain a member of ValidRunnerKinds")
	}
	// A bogus kind is not admitted (fail-closed membership).
	if _, ok := ValidRunnerKinds["gitlab_pipeline"]; ok {
		t.Error("ValidRunnerKinds must not admit an out-of-set kind")
	}
}
