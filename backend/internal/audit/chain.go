package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// HashInputs is the canonical set of fields that contribute to an
// entry's content hash. Ordering and JSON tag names are part of the
// public contract: any external verifier must marshal these fields
// in this exact shape to recompute the chain.
//
// Sequence is intentionally NOT in the hash. Postgres assigns it on
// INSERT, after the hash has been computed and committed; the
// chain's prev_hash linkage already encodes ordering.
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

// ComputeEntryHash returns the lowercase-hex sha256 of HashInputs
// marshaled to JSON. Deterministic for the same inputs.
//
// The external verifier (E2.6 / #27) recomputes via this exact path
// when checking a stored chain. Changes to the canonical
// representation are breaking and must coincide with a chain-format
// version bump.
func ComputeEntryHash(p HashInputs) (string, error) {
	canonical, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("audit: marshal hash inputs: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// ChainAppendParams collects the inputs callers pass to
// AppendChained. Differs from AppendParams by omitting PrevHash and
// EntryHash — those are computed inside the transactional
// AppendChained call, not by the caller.
type ChainAppendParams struct {
	RunID        uuid.UUID
	StageID      *uuid.UUID
	Timestamp    time.Time
	Category     string
	ActorKind    *ActorKind
	ActorSubject *string
	Payload      json.RawMessage
}
