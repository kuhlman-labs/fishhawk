package run

import "testing"

// TestStageTypeDeploy pins the deploy stage-type value (ADR-038 / #1384).
func TestStageTypeDeploy(t *testing.T) {
	if StageTypeDeploy != "deploy" {
		t.Errorf("StageTypeDeploy = %q, want %q", StageTypeDeploy, "deploy")
	}
}

// TestDeployStageStates_Settled asserts the deploy stage's two non-terminal
// states classify correctly (#1384, operator binding condition 2):
// awaiting_deploy_approval is settled (parked for operator action);
// awaiting_deployment is NOT settled (executor polling the external pipeline,
// in-flight like dispatched/running). Neither is terminal.
func TestDeployStageStates_Settled(t *testing.T) {
	if !StageStateAwaitingDeployApproval.IsSettled() {
		t.Error("awaiting_deploy_approval must be settled (parked awaiting operator action)")
	}
	if StageStateAwaitingDeployment.IsSettled() {
		t.Error("awaiting_deployment must NOT be settled (executor polling in-flight; including it would release the stage long-poll mid-poll)")
	}
	if StageStateAwaitingDeployApproval.IsTerminal() {
		t.Error("awaiting_deploy_approval must not be terminal")
	}
	if StageStateAwaitingDeployment.IsTerminal() {
		t.Error("awaiting_deployment must not be terminal")
	}
}

// TestDeployOutcome_Valid pins the closed-set membership check across all
// four canonical outcomes plus an unknown value (ADR-038 / #1384).
func TestDeployOutcome_Valid(t *testing.T) {
	cases := map[DeployOutcome]bool{
		DeployOutcomeSucceeded:  true,
		DeployOutcomeFailed:     true,
		DeployOutcomePartial:    true,
		DeployOutcomeRolledBack: true,
		"":                      false,
		"unknown":               false,
		"SUCCEEDED":             false, // case-sensitive
	}
	for o, want := range cases {
		if got := o.Valid(); got != want {
			t.Errorf("DeployOutcome(%q).Valid() = %v, want %v", o, got, want)
		}
	}
}

// TestDeployOutcome_Description asserts a stable human label for each
// canonical outcome and the literal-passthrough for an unknown value
// (so bad data is surfaced, not masked) — mirroring FailureCategory.
func TestDeployOutcome_Description(t *testing.T) {
	for _, o := range []DeployOutcome{
		DeployOutcomeSucceeded, DeployOutcomeFailed,
		DeployOutcomePartial, DeployOutcomeRolledBack,
	} {
		if o.Description() == "" {
			t.Errorf("DeployOutcome(%q).Description() is empty", o)
		}
		if o.Description() == string(o) {
			t.Errorf("DeployOutcome(%q).Description() returned the literal value; want a human label", o)
		}
	}
	if got := DeployOutcome("mystery").Description(); got != "mystery" {
		t.Errorf("unknown DeployOutcome Description() = %q, want literal passthrough %q", got, "mystery")
	}
}

// TestDeployStage_CarriesOutcome asserts a deploy stage can hold EACH of the
// four DeployOutcome values via the in-memory Stage.DeployOutcome field
// (#1384, operator binding condition 3): partial / rolled_back are genuinely
// representable terminal dispositions in THIS slice, not an orphan type. The
// DB column + producing executor are downstream (E23.5/E23.6).
func TestDeployStage_CarriesOutcome(t *testing.T) {
	for _, o := range []DeployOutcome{
		DeployOutcomeSucceeded, DeployOutcomeFailed,
		DeployOutcomePartial, DeployOutcomeRolledBack,
	} {
		outcome := o
		st := &Stage{
			Type:          StageTypeDeploy,
			State:         StageStateFailed, // partial/rolled_back ride a terminal stage state
			DeployOutcome: &outcome,
		}
		if st.DeployOutcome == nil || *st.DeployOutcome != o {
			t.Errorf("deploy stage did not carry DeployOutcome %q", o)
		}
		if !st.DeployOutcome.Valid() {
			t.Errorf("carried DeployOutcome %q must be Valid()", o)
		}
	}
}
