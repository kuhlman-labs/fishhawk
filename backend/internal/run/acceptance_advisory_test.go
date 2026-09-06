package run

import (
	"strings"
	"testing"
)

// TestAcceptanceBlockerAdvisory_Shapes pins the EXACT rendered string for all
// four shapes (E64.63 / #3222). This renderer is the single owner of wording
// that ships on four surfaces, so an exact-string table is the right shape: a
// substring assertion would let a rewrite of the surrounding sentence pass.
func TestAcceptanceBlockerAdvisory_Shapes(t *testing.T) {
	const tail = " — the tail."
	cases := []struct {
		name     string
		short    string
		state    StageState
		reopened bool
		want     string
	}{
		{
			name:     "reopened dispatchable",
			short:    "abcd1234",
			state:    StageStatePending,
			reopened: true,
			want: "acceptance stage abcd1234 was re-opened by a fix-up push and has not been re-run; " +
				"the prior acceptance verdict is invalidated. Re-dispatch the acceptance stage " +
				"(fishhawk_dispatch_stage, stage acceptance) — the tail.",
		},
		{
			name:     "reopened dispatchable awaiting_host_dispatch",
			short:    "abcd1234",
			state:    StageStateAwaitingHostDispatch,
			reopened: true,
			want: "acceptance stage abcd1234 was re-opened by a fix-up push and has not been re-run; " +
				"the prior acceptance verdict is invalidated. Re-dispatch the acceptance stage " +
				"(fishhawk_dispatch_stage, stage acceptance) — the tail.",
		},
		{
			name:     "reopened in flight",
			short:    "abcd1234",
			state:    StageStateRunning,
			reopened: true,
			want: "acceptance stage abcd1234 was re-opened by a fix-up push and its re-run is already in flight " +
				"(state running); the prior acceptance verdict is invalidated. Wait for the re-run to settle — the tail.",
		},
		{
			name:  "generic dispatchable",
			short: "abcd1234",
			state: StageStatePending,
			want: "the acceptance stage abcd1234 (state pending) must settle first. Dispatch the acceptance stage " +
				"(fishhawk_dispatch_stage, stage acceptance) and let it settle — the tail.",
		},
		{
			name:  "generic in flight",
			short: "abcd1234",
			state: StageStateDispatched,
			want:  "the acceptance stage abcd1234 is already in flight (state dispatched). Wait for acceptance to settle — the tail.",
		},
		{
			name:  "empty short id omits the id clause, dispatchable",
			state: StageStatePending,
			want: "the acceptance stage (state pending) must settle first. Dispatch the acceptance stage " +
				"(fishhawk_dispatch_stage, stage acceptance) and let it settle — the tail.",
		},
		{
			name:  "empty short id omits the id clause, in flight",
			state: StageStateRunning,
			want:  "the acceptance stage is already in flight (state running). Wait for acceptance to settle — the tail.",
		},
		{
			name:     "empty short id omits the id clause, reopened",
			state:    StageStatePending,
			reopened: true,
			want: "acceptance stage was re-opened by a fix-up push and has not been re-run; " +
				"the prior acceptance verdict is invalidated. Re-dispatch the acceptance stage " +
				"(fishhawk_dispatch_stage, stage acceptance) — the tail.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AcceptanceBlockerAdvisory(tc.short, tc.state, tc.reopened, tail)
			if got != tc.want {
				t.Fatalf("AcceptanceBlockerAdvisory:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestAcceptanceBlockerAdvisory_InFlightNeverNamesADispatch is the #3222
// binding-condition-2 assertion at the SOURCE: an in-flight acceptance stage is
// one the operator cannot dispatch, so no in-flight shape may name a dispatch.
// Naming a remedy the operator cannot take sends them to redo work with false
// authority — the defect #3116/#3224 fixed on two other surfaces.
func TestAcceptanceBlockerAdvisory_InFlightNeverNamesADispatch(t *testing.T) {
	inFlight := []StageState{
		StageStateDispatched,
		StageStateRunning,
		StageStateAwaitingApproval,
		StageStateAwaitingChildren,
		StageStateAwaitingInput,
		StageStateAwaitingScopeDecision,
		StageStateAwaitingDeployApproval,
		StageStateAwaitingDeployment,
	}
	for _, st := range inFlight {
		for _, reopened := range []bool{true, false} {
			got := AcceptanceBlockerAdvisory("abcd1234", st, reopened, " — tail.")
			if got == "" {
				t.Fatalf("state %q reopened=%v: expected a non-empty advisory", st, reopened)
			}
			for _, banned := range []string{"fishhawk_dispatch_stage", "Dispatch", "Re-dispatch"} {
				if strings.Contains(got, banned) {
					t.Fatalf("state %q reopened=%v names %q in an in-flight advisory: %q", st, reopened, banned, got)
				}
			}
		}
	}
}

// TestAcceptanceBlockerAdvisory_TerminalIsEmpty is the byte-identity control at
// the source: a terminal acceptance stage blocks nothing, so every composing
// surface must emit exactly what it emitted before #3222.
func TestAcceptanceBlockerAdvisory_TerminalIsEmpty(t *testing.T) {
	for _, st := range []StageState{StageStateSucceeded, StageStateFailed, StageStateCancelled, StageStateSuperseded} {
		for _, reopened := range []bool{true, false} {
			if got := AcceptanceBlockerAdvisory("abcd1234", st, reopened, " — tail."); got != "" {
				t.Fatalf("state %q reopened=%v: want empty, got %q", st, reopened, got)
			}
		}
	}
}

// TestAcceptanceIsDispatchable covers EVERY StageState, so a new state added to
// run.go lands in the in-flight half by default (the safe direction: it never
// invents a dispatch the operator cannot take).
func TestAcceptanceIsDispatchable(t *testing.T) {
	want := map[StageState]bool{
		StageStatePending:                true,
		StageStateAwaitingHostDispatch:   true,
		StageStateDispatched:             false,
		StageStateRunning:                false,
		StageStateAwaitingApproval:       false,
		StageStateAwaitingChildren:       false,
		StageStateAwaitingInput:          false,
		StageStateAwaitingScopeDecision:  false,
		StageStateAwaitingDeployApproval: false,
		StageStateAwaitingDeployment:     false,
		StageStateSucceeded:              false,
		StageStateFailed:                 false,
		StageStateCancelled:              false,
		StageStateSuperseded:             false,
	}
	for st, expect := range want {
		if got := AcceptanceIsDispatchable(st); got != expect {
			t.Errorf("AcceptanceIsDispatchable(%q) = %v, want %v", st, got, expect)
		}
	}
	if AcceptanceIsDispatchable(StageState("not_a_real_state")) {
		t.Error("an unknown state must not be reported dispatchable")
	}
}
