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
//
// RunID is nullable so global-chain entries (E2.7) hash with a JSON
// `null` rather than the zero UUID. External verifiers MUST treat
// the nil RunID as `"run_id": null` to match.
type HashInputs struct {
	RunID        *uuid.UUID      `json:"run_id"`
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
//
// Timestamp normalization (#302): `time.Now()` returns nanosecond
// precision in some local timezone (`time.Now().UTC()` is UTC but
// keeps nanos). Postgres `timestamptz` stores microsecond precision
// and `pgx` reads the value back in the connection's timezone —
// neither matches the in-memory write-time value exactly, so a
// recomputation after the DB round-trip would hash a different
// JSON encoding (`-04:00 + microseconds` vs `Z + nanoseconds`). We
// truncate to microseconds and force UTC here so the canonical
// form is whatever the database can store losslessly, which makes
// the hash stable across `time.Now()` → INSERT → SELECT →
// re-hash. External verifiers MUST apply the same normalization.
func ComputeEntryHash(p HashInputs) (string, error) {
	p.Timestamp = p.Timestamp.UTC().Truncate(time.Microsecond)
	canonical, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("audit: marshal hash inputs: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// ChainAppendParams collects the inputs callers pass to
// AppendChained for a per-run chain entry. Differs from
// AppendParams by omitting PrevHash and EntryHash — those are
// computed inside the transactional AppendChained call, not by
// the caller.
type ChainAppendParams struct {
	RunID        uuid.UUID
	StageID      *uuid.UUID
	Timestamp    time.Time
	Category     string
	ActorKind    *ActorKind
	ActorSubject *string
	Payload      json.RawMessage
}

// GlobalChainAppendParams is the global-chain equivalent of
// ChainAppendParams (E2.7). RunID is implicit (nil) because
// global-chain events aren't tied to a workflow run; StageID is
// also omitted for the same reason. ActorSubject is the most
// meaningful "who did it" field for these events (e.g. the user
// minting an API token).
type GlobalChainAppendParams struct {
	Timestamp    time.Time
	Category     string
	ActorKind    *ActorKind
	ActorSubject *string
	Payload      json.RawMessage
}
