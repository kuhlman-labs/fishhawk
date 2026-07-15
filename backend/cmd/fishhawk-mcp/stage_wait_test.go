package main

import "testing"

// TestSuggestedStageWaitPollIntervalSeconds pins the advertised cadence. The
// 30s value is deliberately coarser than reviews' 15s (#878) because stages
// run minutes, not seconds — a change here is a contract change for every
// polling driver and should be a conscious edit.
func TestSuggestedStageWaitPollIntervalSeconds(t *testing.T) {
	if suggestedStageWaitPollIntervalSeconds != 30 {
		t.Fatalf("suggestedStageWaitPollIntervalSeconds = %d, want 30", suggestedStageWaitPollIntervalSeconds)
	}
}

// TestStageStateIsTerminal pins the terminal set to exactly
// {succeeded, failed, cancelled}. A new backend stage state added without
// updating this mapping fails here (and in TestClassifyStageWaitStatus).
func TestStageStateIsTerminal(t *testing.T) {
	terminal := map[string]bool{
		"pending":                false,
		"awaiting_host_dispatch": false, // #1912: parked for a host spawn — actionable, non-terminal
		"dispatched":             false,
		"running":                false,
		"awaiting_approval":      false,
		"awaiting_children":      false,
		"succeeded":              true,
		"failed":                 true,
		"cancelled":              true,
	}
	for state, want := range terminal {
		if got := stageStateIsTerminal(state); got != want {
			t.Errorf("stageStateIsTerminal(%q) = %v, want %v", state, got, want)
		}
	}
}

// TestClassifyStageWaitStatus walks the documented backend stage states
// (run.StageState constants) and asserts the status mapping plus
// the poll-interval rule: present (==30) while non-terminal, omitted on
// terminal.
func TestClassifyStageWaitStatus(t *testing.T) {
	cases := []struct {
		stageState   string
		wantStatus   string
		wantInterval int // 0 means omitted
	}{
		{"pending", "pending", 30},
		{"awaiting_host_dispatch", "pending", 30}, // #1912: maps to the actionable pending bucket
		{"dispatched", "pending", 30},
		{"awaiting_approval", "pending", 30},
		{"awaiting_children", "pending", 30},
		{"running", "running", 30},
		{"succeeded", "succeeded", 0},
		{"failed", "failed", 0},
		{"cancelled", "cancelled", 0},
	}
	for _, tc := range cases {
		got := classifyStageWaitStatus("plan", tc.stageState, "")
		if got.Stage != "plan" {
			t.Errorf("[%s] Stage = %q, want plan", tc.stageState, got.Stage)
		}
		if got.Status != tc.wantStatus {
			t.Errorf("[%s] Status = %q, want %q", tc.stageState, got.Status, tc.wantStatus)
		}
		if got.PollIntervalSeconds != tc.wantInterval {
			t.Errorf("[%s] PollIntervalSeconds = %d, want %d", tc.stageState, got.PollIntervalSeconds, tc.wantInterval)
		}
	}
}

// TestClassifyStageWaitStatus_RunTerminalBackstop asserts the ADR-036 (#874)
// backstop: a stage still 'running' under a run that has already gone terminal
// keeps its 'running' status but drops the poll interval, so a polling caller
// resolves the wait instead of advertising an unbounded poll.
func TestClassifyStageWaitStatus_RunTerminalBackstop(t *testing.T) {
	for _, runState := range []string{"succeeded", "failed", "cancelled"} {
		got := classifyStageWaitStatus("implement", "running", runState)
		if got.Status != "running" {
			t.Errorf("run=%s: Status = %q, want running", runState, got.Status)
		}
		if got.PollIntervalSeconds != 0 {
			t.Errorf("run=%s: PollIntervalSeconds = %d, want 0 (backstop drops it)", runState, got.PollIntervalSeconds)
		}
	}
	// A non-terminal run leaves the interval in place.
	got := classifyStageWaitStatus("implement", "running", "running")
	if got.PollIntervalSeconds != 30 {
		t.Errorf("non-terminal run: PollIntervalSeconds = %d, want 30", got.PollIntervalSeconds)
	}
}

// TestStageWaitStatusFor resolves a stage of the requested type from an
// already-fetched slice (no backend round-trip) and returns nil when no stage
// of that type exists — mirroring ReviewStatus's 'none' shape.
func TestStageWaitStatusFor(t *testing.T) {
	stages := []Stage{
		{Type: "plan", State: "succeeded"},
		{Type: "implement", State: "running"},
	}

	plan := stageWaitStatusFor(stages, "plan", "running")
	if plan == nil || plan.Status != "succeeded" || plan.PollIntervalSeconds != 0 {
		t.Errorf("plan = %+v, want succeeded/0", plan)
	}

	impl := stageWaitStatusFor(stages, "implement", "running")
	if impl == nil || impl.Status != "running" || impl.PollIntervalSeconds != 30 {
		t.Errorf("implement = %+v, want running/30", impl)
	}

	if review := stageWaitStatusFor(stages, "review", "running"); review != nil {
		t.Errorf("review = %+v, want nil (no review stage)", review)
	}
}

// TestStageWaitStatusFor_Acceptance pins the E31.9 resolution: stageWaitStatusFor
// resolves an acceptance-typed stage row via the same generic helper, and omits
// it (nil) when the run declares none.
func TestStageWaitStatusFor_Acceptance(t *testing.T) {
	stages := []Stage{
		{Type: "implement", State: "succeeded"},
		{Type: "acceptance", State: "running"},
	}
	acc := stageWaitStatusFor(stages, "acceptance", "running")
	if acc == nil || acc.Stage != "acceptance" || acc.Status != "running" || acc.PollIntervalSeconds != 30 {
		t.Errorf("acceptance = %+v, want acceptance/running/30", acc)
	}

	if none := stageWaitStatusFor([]Stage{{Type: "implement", State: "running"}}, "acceptance", "running"); none != nil {
		t.Errorf("acceptance = %+v, want nil (no acceptance stage)", none)
	}
}
