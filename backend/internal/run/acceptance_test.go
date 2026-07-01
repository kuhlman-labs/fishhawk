package run

import "testing"

// TestStageTypeAcceptance pins the acceptance stage-type wire value
// (ADR-049 / #1519). The constant and migration 0044 must ship together —
// the value here is the exact literal migration 0044 widens
// stages_type_check to admit, cross-checked end-to-end by
// TestPostgres_AcceptanceStage_PersistRoundTrip.
func TestStageTypeAcceptance(t *testing.T) {
	if StageTypeAcceptance != "acceptance" {
		t.Errorf("StageTypeAcceptance = %q, want %q", StageTypeAcceptance, "acceptance")
	}
}
