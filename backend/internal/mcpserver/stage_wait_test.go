package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// waitBase is the fixed instant the stage-wait tests measure elapsed against.
// Every case that needs a started stage derives its started_at as
// waitBase.Add(-elapsed), so the arithmetic is exact and clock-independent.
var waitBase = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

// startedAgo returns a *time.Time d before waitBase — the started_at of a stage
// that has been running for exactly d.
func startedAgo(d time.Duration) *time.Time {
	t := waitBase.Add(-d)
	return &t
}

// TestSuggestedStageWaitPollIntervalSeconds pins the advertised cadence FLOOR.
// The 30s value is deliberately coarser than reviews' 15s (#878) because stages
// run minutes, not seconds — a change here is a contract change for every
// polling driver and should be a conscious edit. Since E48.62 / #2489 it is the
// floor of a derived cadence rather than the whole contract; the ceiling and
// divisor are pinned alongside it because all three are equally load-bearing on
// the advertised value.
func TestSuggestedStageWaitPollIntervalSeconds(t *testing.T) {
	if suggestedStageWaitPollIntervalSeconds != 30 {
		t.Fatalf("suggestedStageWaitPollIntervalSeconds = %d, want 30", suggestedStageWaitPollIntervalSeconds)
	}
	if stageWaitPollCeilingSeconds != 900 {
		t.Fatalf("stageWaitPollCeilingSeconds = %d, want 900", stageWaitPollCeilingSeconds)
	}
	if stageWaitPollDivisor != 4 {
		t.Fatalf("stageWaitPollDivisor = %d, want 4", stageWaitPollDivisor)
	}
}

// TestStageWaitPollIntervalSeconds is the derivation's arithmetic table
// (E48.62 / #2489): one case per named branch and clamp, each asserting the
// exact integer the surface will advertise.
//
// Every expectation is derived from the shipped formula — remaining branch
// (predictedMinutes*60 - elapsedSeconds) / 4, elapsed branch elapsedSeconds / 4
// — and then clamped to [30, 900]. The literals below are the derivation's
// output, never the other way round: if one of them disagrees with the formula,
// the LITERAL is the thing to fix.
func TestStageWaitPollIntervalSeconds(t *testing.T) {
	cases := []struct {
		name             string
		predictedMinutes int
		elapsed          time.Duration
		want             int
	}{
		{
			// The byte-identical-to-pre-#2489 case: nothing to derive from, so
			// the floor is what a caller sees, exactly as before.
			name: "no prediction, no elapsed -> floor", predictedMinutes: 0, elapsed: 0, want: 30,
		},
		{
			// Elapsed branch: 600 / 4.
			name: "no prediction, 600s elapsed", predictedMinutes: 0, elapsed: 600 * time.Second, want: 150,
		},
		{
			// The observed long plan stage from #2489. The plan stage can never
			// carry a prediction (no plan exists while it runs), so it always
			// takes this branch: 1304 / 4 = 326 instead of sitting at 30.
			name: "no prediction, 1304s elapsed (observed plan stage)", predictedMinutes: 0, elapsed: 1304 * time.Second, want: 326,
		},
		{
			// The motivating fan-out. Raw is (115*60 - 0) / 4 = 1725, which the
			// CEILING clamps to 900.
			name: "prediction 115, no elapsed -> ceiling", predictedMinutes: 115, elapsed: 0, want: 900,
		},
		{
			// Remaining branch, unclamped: (115*60 - 5400) / 4 = 1500 / 4 = 375.
			name: "prediction 115, 5400s elapsed", predictedMinutes: 115, elapsed: 5400 * time.Second, want: 375,
		},
		{
			// OVERRUN: the stage is past its own prediction, so remaining has
			// gone negative ((600 - 3000) / 4 = -600) and the floor applies. A
			// stage running over is the one worth polling tightly.
			name: "prediction 10, 3000s elapsed (overrun) -> floor", predictedMinutes: 10, elapsed: 3000 * time.Second, want: 30,
		},
		{
			// FLOOR clamp on the remaining branch: raw is (60 - 0) / 4 = 15.
			name: "prediction 1, no elapsed -> floor", predictedMinutes: 1, elapsed: 0, want: 30,
		},
		{
			// Clock skew handed straight to the core: -30 / 4 truncates to -7,
			// which the floor absorbs. (stageElapsed clamps this to zero one
			// layer up; the core is defensive on its own.)
			name: "negative elapsed (clock skew) -> floor", predictedMinutes: 0, elapsed: -30 * time.Second, want: 30,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stageWaitPollIntervalSeconds(tc.predictedMinutes, tc.elapsed); got != tc.want {
				t.Errorf("stageWaitPollIntervalSeconds(%d, %s) = %d, want %d",
					tc.predictedMinutes, tc.elapsed, got, tc.want)
			}
		})
	}
}

// TestStageElapsed covers the elapsed helper's degenerate branches: a stage
// that has not started (nil started_at) and a now that precedes started_at
// (clock skew) both yield zero, so the derivation never sees a negative span.
func TestStageElapsed(t *testing.T) {
	if got := stageElapsed(nil, waitBase); got != 0 {
		t.Errorf("stageElapsed(nil, now) = %s, want 0", got)
	}
	future := waitBase.Add(90 * time.Second)
	if got := stageElapsed(&future, waitBase); got != 0 {
		t.Errorf("stageElapsed(started_at after now) = %s, want 0 (clock skew clamped)", got)
	}
	if got := stageElapsed(startedAgo(600*time.Second), waitBase); got != 600*time.Second {
		t.Errorf("stageElapsed(600s ago) = %s, want 10m0s", got)
	}
}

// TestDerivedStageWaitPollInterval_NilInputs asserts the next-actions wrapper's
// three defensive branches — nil run, nil stage, and a stage that has not
// started — each yield exactly the floor rather than a zero or a panic.
func TestDerivedStageWaitPollInterval_NilInputs(t *testing.T) {
	run := &Run{PredictedRuntimeMinutes: 115}
	stage := &Stage{Type: "implement", State: "running", StartedAt: startedAgo(time.Hour)}

	if got := derivedStageWaitPollInterval(nil, stage); got != 30 {
		t.Errorf("derivedStageWaitPollInterval(nil run) = %d, want 30", got)
	}
	if got := derivedStageWaitPollInterval(run, nil); got != 30 {
		t.Errorf("derivedStageWaitPollInterval(nil stage) = %d, want 30", got)
	}
	// A never-started stage under a run that DOES carry a prediction: elapsed
	// is zero, so the raw value is the full prediction quarter (1725) and the
	// ceiling — not the floor — is what applies. Asserting 900 here proves the
	// nil started_at reaches the derivation rather than short-circuiting it.
	notStarted := &Stage{Type: "implement", State: "pending"}
	if got := derivedStageWaitPollInterval(run, notStarted); got != 900 {
		t.Errorf("derivedStageWaitPollInterval(nil started_at, prediction 115) = %d, want 900 (ceiling)", got)
	}
	// The same never-started stage under an UNSTAMPED run is the floor.
	if got := derivedStageWaitPollInterval(&Run{}, notStarted); got != 30 {
		t.Errorf("derivedStageWaitPollInterval(nil started_at, no prediction) = %d, want 30", got)
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
// the poll-interval rule: present while non-terminal, omitted on terminal.
// With no prediction and no elapsed the derived value is the floor, so this
// table is byte-identical to its pre-#2489 form.
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
		got := classifyStageWaitStatus("plan", tc.stageState, "", nil, 0, waitBase)
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

// TestClassifyStageWaitStatus_Derived pins the classification contract against
// the derived cadence (E48.62 / #2489): a NON-terminal stage carries the
// derived value, and a TERMINAL stage omits it entirely even under a large
// prediction — the derivation must never leak past the terminal guard.
func TestClassifyStageWaitStatus_Derived(t *testing.T) {
	started := startedAgo(5400 * time.Second)

	got := classifyStageWaitStatus("implement", "running", "running", started, 115, waitBase)
	if got.Status != "running" {
		t.Errorf("Status = %q, want running", got.Status)
	}
	if got.PollIntervalSeconds != 375 {
		t.Errorf("PollIntervalSeconds = %d, want 375 ((115*60 - 5400) / 4)", got.PollIntervalSeconds)
	}

	for _, terminal := range []string{"succeeded", "failed", "cancelled"} {
		got := classifyStageWaitStatus("implement", terminal, "running", started, 115, waitBase)
		if got.PollIntervalSeconds != 0 {
			t.Errorf("[%s] PollIntervalSeconds = %d, want 0 (terminal omits it even with a prediction)",
				terminal, got.PollIntervalSeconds)
		}
	}
}

// TestClassifyStageWaitStatus_RunTerminalBackstop asserts the ADR-036 (#874)
// backstop: a stage still 'running' under a run that has already gone terminal
// keeps its 'running' status but drops the poll interval, so a polling caller
// resolves the wait instead of advertising an unbounded poll. The backstop
// survives the derivation — a large prediction must not resurrect the interval.
func TestClassifyStageWaitStatus_RunTerminalBackstop(t *testing.T) {
	started := startedAgo(5400 * time.Second)
	for _, runState := range []string{"succeeded", "failed", "cancelled"} {
		got := classifyStageWaitStatus("implement", "running", runState, started, 115, waitBase)
		if got.Status != "running" {
			t.Errorf("run=%s: Status = %q, want running", runState, got.Status)
		}
		if got.PollIntervalSeconds != 0 {
			t.Errorf("run=%s: PollIntervalSeconds = %d, want 0 (backstop drops it)", runState, got.PollIntervalSeconds)
		}
	}
	// A non-terminal run leaves the interval in place.
	got := classifyStageWaitStatus("implement", "running", "running", nil, 0, waitBase)
	if got.PollIntervalSeconds != 30 {
		t.Errorf("non-terminal run: PollIntervalSeconds = %d, want 30", got.PollIntervalSeconds)
	}
}

// TestStageDeadlineRemaining_UnknownBudget is the counterfactual for the
// budget guard (#2540): an unknown budget (agent_timeout_seconds == 0) yields
// nil ("unknown"), NOT a bogus value. Deleting the `<= 0` guard in
// stageDeadlineRemaining returns a (0 - elapsed) value and reddens this.
func TestStageDeadlineRemaining_UnknownBudget(t *testing.T) {
	if got := stageDeadlineRemaining(0, 60*time.Second); got != nil {
		t.Errorf("stageDeadlineRemaining(0, 60s) = %v (%d), want nil", got, *got)
	}
	if got := stageDeadlineRemaining(-1, 60*time.Second); got != nil {
		t.Errorf("stageDeadlineRemaining(-1, 60s) = %v, want nil", got)
	}
}

// TestStageDeadlineRemaining_OverrunClampsToZero is the counterfactual for the
// clamp (#2540): an OVERRUNNING stage (elapsed > budget) reports 0, not a
// negative. Deleting the `remaining < 0` clamp reddens this.
func TestStageDeadlineRemaining_OverrunClampsToZero(t *testing.T) {
	got := stageDeadlineRemaining(3600, 3700*time.Second)
	if got == nil {
		t.Fatal("stageDeadlineRemaining(3600, 3700s) = nil, want 0 (clamped, budget is known)")
	}
	if *got != 0 {
		t.Errorf("stageDeadlineRemaining(3600, 3700s) = %d, want 0 (clamped)", *got)
	}
}

// TestStageDeadlineRemaining_LiveStage pins the ordinary case: a known budget
// with elapsed inside it yields budget - elapsed.
func TestStageDeadlineRemaining_LiveStage(t *testing.T) {
	got := stageDeadlineRemaining(3600, 600*time.Second)
	if got == nil || *got != 3000 {
		t.Errorf("stageDeadlineRemaining(3600, 600s) = %v, want 3000", got)
	}
}

// requireWaitDeadlineKeysAbsent marshals a StageWaitStatus and asserts the three
// #2540 deadline keys are absent from the WIRE bytes — not just zero-valued in
// the decoded struct. elapsed_seconds / agent_timeout_seconds are non-pointer
// omitempty ints, so a struct-field check cannot tell an omitted key from a
// present 0; only a raw-body check pins the byte-level omission (test-vacuity
// guard). Fails on any of the three keys appearing.
func requireWaitDeadlineKeysAbsent(t *testing.T, st *StageWaitStatus, label string) {
	t.Helper()
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("%s: marshal: %v", label, err)
	}
	for _, key := range []string{"elapsed_seconds", "agent_timeout_seconds", "deadline_seconds_remaining"} {
		if strings.Contains(string(b), key) {
			t.Errorf("%s: wire carries %q key, want omitted:\n%s", label, key, b)
		}
	}
}

// TestStageWaitStatusFor_DeadlineFields asserts the resolver folds the #2540
// deadline observability onto a NON-terminal status (elapsed + budget +
// remaining) and OMITS all three on a terminal stage and when the budget is
// unknown — mirroring how it already gates poll_interval_seconds.
func TestStageWaitStatusFor_DeadlineFields(t *testing.T) {
	stages := []Stage{
		{Type: "implement", State: "running", StartedAt: startedAgo(600 * time.Second), AgentTimeoutSeconds: 3600},
		{Type: "plan", State: "succeeded", StartedAt: startedAgo(600 * time.Second), AgentTimeoutSeconds: 3600},
		{Type: "acceptance", State: "running", StartedAt: startedAgo(600 * time.Second)}, // budget unknown (0)
	}

	// Non-terminal with a known budget: all three present, internally consistent.
	impl := stageWaitStatusFor(stages, "implement", "running", 0, waitBase)
	if impl == nil {
		t.Fatal("implement wait status nil")
	}
	if impl.ElapsedSeconds != 600 {
		t.Errorf("elapsed_seconds = %d, want 600", impl.ElapsedSeconds)
	}
	if impl.AgentTimeoutSeconds != 3600 {
		t.Errorf("agent_timeout_seconds = %d, want 3600", impl.AgentTimeoutSeconds)
	}
	if impl.DeadlineSecondsRemaining == nil || *impl.DeadlineSecondsRemaining != 3000 {
		t.Errorf("deadline_seconds_remaining = %v, want 3000", impl.DeadlineSecondsRemaining)
	}
	if impl.ElapsedSeconds+*impl.DeadlineSecondsRemaining != impl.AgentTimeoutSeconds {
		t.Errorf("elapsed + remaining != agent_timeout: %d + %d != %d",
			impl.ElapsedSeconds, *impl.DeadlineSecondsRemaining, impl.AgentTimeoutSeconds)
	}

	// Terminal stage: all three omitted (mirrors poll_interval_seconds).
	plan := stageWaitStatusFor(stages, "plan", "running", 0, waitBase)
	if plan == nil {
		t.Fatal("plan wait status nil")
	}
	if plan.DeadlineSecondsRemaining != nil || plan.AgentTimeoutSeconds != 0 || plan.ElapsedSeconds != 0 {
		t.Errorf("terminal stage carries deadline fields: %+v", plan)
	}
	// Byte-level: the terminal wire must omit all three keys, not merely carry 0s.
	requireWaitDeadlineKeysAbsent(t, plan, "terminal stage")

	// Non-terminal but UNKNOWN budget: omitted (fail-open to byte-identical wire).
	acc := stageWaitStatusFor(stages, "acceptance", "running", 0, waitBase)
	if acc == nil {
		t.Fatal("acceptance wait status nil")
	}
	if acc.DeadlineSecondsRemaining != nil || acc.AgentTimeoutSeconds != 0 || acc.ElapsedSeconds != 0 {
		t.Errorf("unknown-budget stage carries deadline fields: %+v", acc)
	}
	// Byte-level: the unknown-budget wire must omit all three keys too.
	requireWaitDeadlineKeysAbsent(t, acc, "unknown-budget stage")
}

// TestAmendmentPollWindowConstantsInSync is the cross-package drift guard
// (#2540 approval condition 4): the server's surfaced poll window
// (server.AmendmentPollWindowSeconds, 900) and this package's operator-facing
// figure (amendmentPollWindowMinutes, 15) state the SAME poll window on
// different surfaces. Both are OBSERVABILITY figures now (the server displays it
// on a request response as amendment_poll_window_seconds; this package renders it
// in tool docs), so a future edit to one but not the other would show an operator
// two different windows for the same thing — this asserts they stay equal,
// reddening on any drift.
func TestAmendmentPollWindowConstantsInSync(t *testing.T) {
	if server.AmendmentPollWindowSeconds != amendmentPollWindowMinutes*60 {
		t.Errorf("poll-window drift: server.AmendmentPollWindowSeconds = %d, but amendmentPollWindowMinutes*60 = %d (%d min); keep the copies in sync",
			server.AmendmentPollWindowSeconds, amendmentPollWindowMinutes*60, amendmentPollWindowMinutes)
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

	plan := stageWaitStatusFor(stages, "plan", "running", 0, waitBase)
	if plan == nil || plan.Status != "succeeded" || plan.PollIntervalSeconds != 0 {
		t.Errorf("plan = %+v, want succeeded/0", plan)
	}

	impl := stageWaitStatusFor(stages, "implement", "running", 0, waitBase)
	if impl == nil || impl.Status != "running" || impl.PollIntervalSeconds != 30 {
		t.Errorf("implement = %+v, want running/30", impl)
	}

	if review := stageWaitStatusFor(stages, "review", "running", 0, waitBase); review != nil {
		t.Errorf("review = %+v, want nil (no review stage)", review)
	}
}

// TestStageWaitStatusFor_PerStageElapsed asserts that the resolver reads the
// MATCHED stage's own started_at, not the first stage's: two non-terminal
// stages on one snapshot, started at different times, must advertise different
// derived cadences under the same run prediction and the same now.
func TestStageWaitStatusFor_PerStageElapsed(t *testing.T) {
	stages := []Stage{
		{Type: "implement", State: "running", StartedAt: startedAgo(5400 * time.Second)},
		{Type: "acceptance", State: "pending", StartedAt: startedAgo(600 * time.Second)},
	}

	impl := stageWaitStatusFor(stages, "implement", "running", 115, waitBase)
	if impl == nil || impl.PollIntervalSeconds != 375 {
		t.Errorf("implement = %+v, want poll 375 ((115*60 - 5400) / 4)", impl)
	}
	// (115*60 - 600) / 4 = 1575, clamped to the 900 ceiling.
	acc := stageWaitStatusFor(stages, "acceptance", "running", 115, waitBase)
	if acc == nil || acc.PollIntervalSeconds != 900 {
		t.Errorf("acceptance = %+v, want poll 900 (ceiling)", acc)
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
	acc := stageWaitStatusFor(stages, "acceptance", "running", 0, waitBase)
	if acc == nil || acc.Stage != "acceptance" || acc.Status != "running" || acc.PollIntervalSeconds != 30 {
		t.Errorf("acceptance = %+v, want acceptance/running/30", acc)
	}

	if none := stageWaitStatusFor([]Stage{{Type: "implement", State: "running"}}, "acceptance", "running", 0, waitBase); none != nil {
		t.Errorf("acceptance = %+v, want nil (no acceptance stage)", none)
	}
}
