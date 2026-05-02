// Package audit provides the canonical chain-hash and Ed25519
// verification logic used by the external verifier.
//
// This is intentionally a re-implementation of
// backend/internal/audit.ComputeEntryHash, NOT an import. ADR-008
// (#72) is explicit: "the external verification tool reads the
// (run_id, public_key) pair from an audit-log export and verifies
// the corresponding bundle against the public key. No backend trust
// required." Sharing code with the backend would defeat that — a
// compromised backend could ship a tampered hash function alongside
// the tampered audit log and the verifier would happily agree.
//
// Drift between backend and verifier is caught by a canonical
// fixture test that lives in both packages with the same (input,
// expected hash) pair. Updating one without the other fails CI.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ActorKind enumerates who acted to produce an entry. Closed set
// per the schema CHECK; nil is also valid.
type ActorKind string

// Actor kinds.
const (
	ActorAgent  ActorKind = "agent"
	ActorUser   ActorKind = "user"
	ActorSystem ActorKind = "system"
)

// HashInputs is the canonical set of fields that contribute to an
// entry's hash. Field order, JSON tags, and types must match
// backend/internal/audit.HashInputs exactly. Treat changes here as
// breaking — every audit log already in the wild was hashed with
// the prior shape.
type HashInputs struct {
	RunID        uuid.UUID       `json:"run_id"`
	StageID      *uuid.UUID      `json:"stage_id"`
	Timestamp    time.Time       `json:"ts"`
	Category     string          `json:"category"`
	ActorKind    *ActorKind      `json:"actor_kind"`
	ActorSubject *string         `json:"actor_subject"`
	Payload      json.RawMessage `json:"payload"`
	PrevHash     *string         `json:"prev_hash"`
}

// ComputeEntryHash returns lowercase-hex sha256 of HashInputs
// marshaled to JSON. Deterministic: same inputs produce the same
// output across implementations.
func ComputeEntryHash(p HashInputs) (string, error) {
	canonical, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("verifier: marshal hash inputs: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
