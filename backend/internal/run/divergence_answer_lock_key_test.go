package run

import (
	"testing"

	"github.com/google/uuid"
)

// TestDivergenceAnswerLockKey pins the per-question key scoping of the
// divergence answer lock (#3891): equal inputs derive one key, a different
// sequence on the same run or a different run at the same sequence derives
// another, and the domain prefix separates the key space from the
// consolidated-PR-open lock's. Counterfactual: dropping the sequence from the
// hash collapses (run, 1) and (run, 2) onto one key, RED on the
// distinct-sequence assertion.
func TestDivergenceAnswerLockKey(t *testing.T) {
	runID := uuid.MustParse("6b1f3c0e-8d2a-4e57-9a61-2f0c4b7d9e13")
	otherRun := uuid.MustParse("0e9d8c7b-6a5f-4e3d-8c2b-1a0f9e8d7c6b")

	if a, b := divergenceAnswerLockKey(runID, 1), divergenceAnswerLockKey(runID, 1); a != b {
		t.Fatalf("divergenceAnswerLockKey is not deterministic: %d != %d", a, b)
	}
	if a, b := divergenceAnswerLockKey(runID, 1), divergenceAnswerLockKey(runID, 2); a == b {
		t.Fatalf("(run, 1) and (run, 2) share key %d: distinct questions on one run would serialize", a)
	}
	if a, b := divergenceAnswerLockKey(runID, 1), divergenceAnswerLockKey(otherRun, 1); a == b {
		t.Fatalf("two runs at sequence 1 share key %d: distinct runs would serialize", a)
	}
	if a, b := divergenceAnswerLockKey(runID, 1), consolidatedPROpenLockKey(runID); a == b {
		t.Fatalf("divergence answer key equals the consolidated-PR-open key %d for the same run: the domains are not separated", a)
	}
}
