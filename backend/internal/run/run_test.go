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

// TestValidRunnerKinds_AdmitsGitLabCI pins the E45.8 / #1861 addition of the
// `gitlab_ci` runner backend. The const carries the exact wire value
// 'gitlab_ci' (persisted to runs.runner_kind and echoed in audit payloads
// forever) and is a member of the closed-set membership check alongside the
// two prior backends. This is a behavioral done-means assertion (#1169):
// compilation alone does not enforce ValidRunnerKinds membership, so a bare
// const add without the map entry would slip through without this test.
func TestValidRunnerKinds_AdmitsGitLabCI(t *testing.T) {
	if got := RunnerKindGitLabCI; got != "gitlab_ci" {
		t.Errorf("RunnerKindGitLabCI = %q, want gitlab_ci", got)
	}
	if _, ok := ValidRunnerKinds[RunnerKindGitLabCI]; !ok {
		t.Error("ValidRunnerKinds must admit gitlab_ci (E45.8 / #1861)")
	}
	// The two prior backends remain members — the widening is additive.
	if _, ok := ValidRunnerKinds[RunnerKindGitHubActions]; !ok {
		t.Error("ValidRunnerKinds must still admit github_actions")
	}
	if _, ok := ValidRunnerKinds[RunnerKindLocal]; !ok {
		t.Error("ValidRunnerKinds must still admit local")
	}
	// A bogus value is not admitted — the set stays closed.
	if _, ok := ValidRunnerKinds["gitlab"]; ok {
		t.Error("ValidRunnerKinds must not admit a bogus 'gitlab' value")
	}
}
