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
