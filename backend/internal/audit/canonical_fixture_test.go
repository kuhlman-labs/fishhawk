package audit_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// TestComputeEntryHash_CanonicalFixture pins the backend to the
// same (input, expected hash) pair as
// verifier/internal/audit/chain_test.go. ADR-008 (#72) requires the
// external verifier to recompute hashes without trusting backend
// code; this fixture is the drift-detection mechanism. If you
// change either implementation, update both fixtures.
func TestComputeEntryHash_CanonicalFixture(t *testing.T) {
	in := audit.HashInputs{
		RunID:     uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		Timestamp: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		Category:  "plan_generated",
		Payload:   json.RawMessage(`{"summary":"canonical"}`),
		// StageID, ActorKind, ActorSubject, PrevHash all nil
	}
	const want = "63e470ec0555fabfce317bd4d2503b274d7aa5ce084ebdae063e0f102376f030"

	got, err := audit.ComputeEntryHash(in)
	if err != nil {
		t.Fatalf("ComputeEntryHash: %v", err)
	}
	if got != want {
		t.Errorf("hash drift detected:\n  got  %s\n  want %s\n\nIf this is an intentional algorithm change, update both this fixture and the matching test in verifier/internal/audit/.",
			got, want)
	}
}
