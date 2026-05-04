package audit

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// canonicalFixture is the shared test vector. Same inputs, same
// expected hash on both backend/internal/audit and
// verifier/internal/audit. If you change either implementation, the
// other side's test must change too — that's the drift-detection
// mechanism.
var canonicalRunID = uuid.MustParse("00000000-0000-0000-0000-000000000001")
var canonicalFixture = struct {
	in       HashInputs
	wantHash string
}{
	in: HashInputs{
		RunID:     &canonicalRunID,
		Timestamp: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		Category:  "plan_generated",
		Payload:   json.RawMessage(`{"summary":"canonical"}`),
		// StageID, ActorKind, ActorSubject, PrevHash all nil
	},
	wantHash: "63e470ec0555fabfce317bd4d2503b274d7aa5ce084ebdae063e0f102376f030",
}

// TestComputeEntryHash_CanonicalFixture pins the verifier to the
// same (input, output) pair as the backend. The expected hash is
// updated below at first run; afterward, any change fails CI on
// both sides.
func TestComputeEntryHash_CanonicalFixture(t *testing.T) {
	got, err := ComputeEntryHash(canonicalFixture.in)
	if err != nil {
		t.Fatalf("ComputeEntryHash: %v", err)
	}
	if got != canonicalFixture.wantHash {
		t.Errorf("hash drift detected:\n  got  %s\n  want %s\n\nIf this is an intentional algorithm change, update both this fixture and the matching test in backend/internal/audit/.",
			got, canonicalFixture.wantHash)
	}
}

func TestComputeEntryHash_Deterministic(t *testing.T) {
	in := canonicalFixture.in
	a, _ := ComputeEntryHash(in)
	b, _ := ComputeEntryHash(in)
	if a != b {
		t.Errorf("hash not deterministic: %s vs %s", a, b)
	}
}

func TestComputeEntryHash_DiffersWhenInputChanges(t *testing.T) {
	base := canonicalFixture.in
	baseHash, _ := ComputeEntryHash(base)

	mutated := base
	mutated.Category = "different"
	mutatedHash, _ := ComputeEntryHash(mutated)
	if mutatedHash == baseHash {
		t.Error("changing Category did not change hash")
	}
}
