package mcpserver

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestRetryStageStateParks_AllStateClasses covers every state class the
// E45.89 / #3624 derivation enumerates: the two un-driven shapes park, the
// three driven/gate shapes do not, and an unrecognised state fails QUIET
// (not parked) rather than emitting a pointer derived from a state the
// predicate does not understand.
func TestRetryStageStateParks_AllStateClasses(t *testing.T) {
	cases := []struct {
		state string
		want  bool
	}{
		{"awaiting_host_dispatch", true}, // the #1912 explicit local park
		{"pending", true},                // orchestrator absent / Advance errored
		{"dispatched", false},            // CI kinds' workflow_dispatch fired
		{"awaiting_approval", false},     // category-D SLA-timeout gate re-open
		{"awaiting_children", false},     // #1891 decomposed-parent restore
		{"some_future_state", false},     // unrecognised -> fail quiet
		{"", false},                      // empty -> fail quiet
	}
	for _, tc := range cases {
		if got := retryStageStateParks(tc.state); got != tc.want {
			t.Errorf("retryStageStateParks(%q) = %v, want %v", tc.state, got, tc.want)
		}
	}
}

// parkStage builds a post-retry Stage in the given state. Seeded BY
// CONSTRUCTION so a counterfactual RED lands on the behavioral assertion and
// never on fixture setup.
func parkStage(state string) Stage {
	return Stage{
		ID:    uuid.NewString(),
		RunID: uuid.NewString(),
		Type:  "implement",
		State: state,
	}
}

// TestRetryStagePark_AwaitingHostDispatch_StandaloneRun is branch (1): the
// local park on a non-decomposed run points at fishhawk_dispatch_stage keyed
// on (run_id, stage).
func TestRetryStagePark_AwaitingHostDispatch_StandaloneRun(t *testing.T) {
	stage := parkStage("awaiting_host_dispatch")
	runRow := &Run{ID: stage.RunID, RunnerKind: "local"}

	parked, next, warnings := retryStagePark(stage, runRow, nil)

	if !parked {
		t.Fatal("parked = false, want true (awaiting_host_dispatch is the local park)")
	}
	if next == nil {
		t.Fatal("next_step = nil, want the fishhawk_dispatch_stage pointer")
	}
	if next.Action != "fishhawk_dispatch_stage" {
		t.Errorf("action = %q, want fishhawk_dispatch_stage", next.Action)
	}
	if next.Params["run_id"] != stage.RunID {
		t.Errorf("params[run_id] = %q, want %q", next.Params["run_id"], stage.RunID)
	}
	if next.Params["stage"] != "implement" {
		t.Errorf("params[stage] = %q, want implement", next.Params["stage"])
	}
	if next.Consumes != "none" {
		t.Errorf("consumes = %q, want none", next.Consumes)
	}
	if !strings.Contains(next.Reason, "awaiting_host_dispatch") {
		t.Errorf("reason = %q, want it to name the park state", next.Reason)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none on a successful run read", warnings)
	}
}

// TestRetryStagePark_ChildSelectsRunChildren is branch (2) AND the named
// counterfactual vehicle: a parked stage on a DECOMPOSED CHILD run selects
// fishhawk_run_children keyed on the PARENT run id, because
// fishhawk_dispatch_stage checks out main and so cannot see a depends_on
// slice's dependency. Delete the runRow.DecomposedFrom branch in
// retry_park.go and this goes RED on the Action assertion.
func TestRetryStagePark_ChildSelectsRunChildren(t *testing.T) {
	stage := parkStage("awaiting_host_dispatch")
	parentID := uuid.NewString()
	runRow := &Run{ID: stage.RunID, RunnerKind: "local", DecomposedFrom: &parentID}

	parked, next, warnings := retryStagePark(stage, runRow, nil)

	if !parked {
		t.Fatal("parked = false, want true")
	}
	if next == nil {
		t.Fatal("next_step = nil, want the fishhawk_run_children pointer")
	}
	if next.Action != "fishhawk_run_children" {
		t.Errorf("action = %q, want fishhawk_run_children", next.Action)
	}
	if next.Params["parent_run_id"] != parentID {
		t.Errorf("params[parent_run_id] = %q, want %q", next.Params["parent_run_id"], parentID)
	}
	if _, has := next.Params["run_id"]; has {
		t.Errorf("params = %v, want no run_id on the run_children pointer", next.Params)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}

// TestRetryStagePark_PendingIsParked is branch (3): the DEGRADED shape (no
// Orchestrator wired, or its Advance errored and retryStageAs logged without
// failing the request) is equally un-driven, so it gets the same dispatch
// pointer.
func TestRetryStagePark_PendingIsParked(t *testing.T) {
	stage := parkStage("pending")
	runRow := &Run{ID: stage.RunID, RunnerKind: "local"}

	parked, next, _ := retryStagePark(stage, runRow, nil)

	if !parked {
		t.Fatal("parked = false, want true (pending is un-driven)")
	}
	if next == nil || next.Action != "fishhawk_dispatch_stage" {
		t.Fatalf("next_step = %+v, want the fishhawk_dispatch_stage pointer", next)
	}
	if !strings.Contains(next.Reason, "pending") {
		t.Errorf("reason = %q, want it to name the pending park", next.Reason)
	}
}

// TestRetryStagePark_DispatchedEmitsNoNextStep is branch (4) AND the named
// counterfactual vehicle for the classification itself: the CI kinds'
// workflow_dispatch genuinely fired, so nothing must be emitted. Mutate
// retryStageStateParks in retry_park.go to map dispatched -> parked (add
// stageStateDispatched to the parked case) and this goes RED on both the
// Parked and NextStep assertions.
func TestRetryStagePark_DispatchedEmitsNoNextStep(t *testing.T) {
	stage := parkStage("dispatched")
	runRow := &Run{ID: stage.RunID, RunnerKind: "github_actions"}

	parked, next, warnings := retryStagePark(stage, runRow, nil)

	if parked {
		t.Error("parked = true, want false (the orchestrator fired a fresh workflow_dispatch)")
	}
	if next != nil {
		t.Errorf("next_step = %+v, want nil — a dispatched stage needs no follow-on call", next)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}

// TestRetryStagePark_GateReopensAreNotParked covers branches (5) and (6): a
// category-D SLA-timeout re-open is a GATE, and a decomposed-parent restore
// re-engages the fan-in sweeper — neither is a missing host dispatch.
func TestRetryStagePark_GateReopensAreNotParked(t *testing.T) {
	for _, state := range []string{"awaiting_approval", "awaiting_children"} {
		stage := parkStage(state)
		runRow := &Run{ID: stage.RunID, RunnerKind: "local"}

		parked, next, warnings := retryStagePark(stage, runRow, nil)

		if parked {
			t.Errorf("state %q: parked = true, want false", state)
		}
		if next != nil {
			t.Errorf("state %q: next_step = %+v, want nil", state, next)
		}
		if len(warnings) != 0 {
			t.Errorf("state %q: warnings = %v, want none", state, warnings)
		}
	}
}

// TestRetryStagePark_UnknownStateFailsQuiet is branch (7): an unrecognised
// state emits NO pointer and NO warning — fail quiet rather than hand the
// caller a bogus follow-on call derived from a state this code does not
// understand.
func TestRetryStagePark_UnknownStateFailsQuiet(t *testing.T) {
	stage := parkStage("some_future_state")
	runRow := &Run{ID: stage.RunID, RunnerKind: "local"}

	parked, next, warnings := retryStagePark(stage, runRow, nil)

	if parked || next != nil || len(warnings) != 0 {
		t.Errorf("parked/next/warnings = %v/%+v/%v, want false/nil/none", parked, next, warnings)
	}
}

// TestRetryStagePark_RunReadErrorFailsOpen is branch (8) AND a named
// counterfactual vehicle: the park is already PROVEN by the stage state, so a
// run-read failure must still emit the fishhawk_dispatch_stage pointer plus
// exactly ONE warning naming the skipped decomposition-child check. Delete
// the runReadErr fail-open arm in retry_park.go and this goes RED (the nil
// runRow would then dereference or return no pointer).
func TestRetryStagePark_RunReadErrorFailsOpen(t *testing.T) {
	stage := parkStage("awaiting_host_dispatch")

	parked, next, warnings := retryStagePark(stage, nil, errors.New("get run: 500 internal"))

	if !parked {
		t.Fatal("parked = false, want true — the stage state already proves the park")
	}
	if next == nil || next.Action != "fishhawk_dispatch_stage" {
		t.Fatalf("next_step = %+v, want the fishhawk_dispatch_stage pointer (fail OPEN)", next)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly 1", warnings)
	}
	if !strings.Contains(warnings[0], "fishhawk_run_children") {
		t.Errorf("warning = %q, want it to name the skipped decomposition-child check", warnings[0])
	}
}

// TestRetryStagePark_NilRunRowWithoutErrorFailsOpen pins the OTHER fail-open
// input shape: a nil run row with a nil error (a backend that answered 200
// with no body the mirror could populate) takes the same arm, so the caller
// still gets a pointer instead of silence.
func TestRetryStagePark_NilRunRowWithoutErrorFailsOpen(t *testing.T) {
	stage := parkStage("pending")

	parked, next, warnings := retryStagePark(stage, nil, nil)

	if !parked {
		t.Fatal("parked = false, want true")
	}
	if next == nil || next.Action != "fishhawk_dispatch_stage" {
		t.Fatalf("next_step = %+v, want the fishhawk_dispatch_stage pointer", next)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly 1", warnings)
	}
}
