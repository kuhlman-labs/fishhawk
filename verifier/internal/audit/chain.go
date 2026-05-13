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
	"bytes"
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
//
// RunID is *uuid.UUID (E2.7 / #138): nil for global-chain entries
// (token issue/revoke, OAuth events, etc.). The JSON encoding is
// `null` when the pointer is nil, the UUID string when set;
// external verifiers must marshal nil pointers as `null` to match.
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

// ComputeEntryHash returns lowercase-hex sha256 of HashInputs
// marshaled to JSON. Deterministic: same inputs produce the same
// output across implementations.
//
// Two normalizations apply so the canonical form is stable across
// the backend's write → DB → re-hash round-trip:
//
// Timestamp (#302): the backend writes audit rows from
// `time.Now()`-derived values (nanosecond precision, possibly local
// timezone). Postgres `timestamptz` stores microsecond precision
// and `pgx` reads back in the connection's timezone. We normalize
// to UTC, microsecond-truncated — whatever the database can
// round-trip losslessly.
//
// Payload (#308): `audit_entries.payload` is a JSONB column that
// doesn't preserve key order or whitespace. The bytes the backend's
// json.Marshal produced and the bytes pgx reads back are different
// shapes of the same semantic JSON, so a hash over the raw payload
// diverges between write and verify. We canonicalize via parse +
// re-marshal (UseNumber to preserve int precision) — both sides
// converge on the Go canonical form.
//
// The backend ships the same normalizations; ADR-008 / #72 keeps
// the two paths byte-equal.
func ComputeEntryHash(p HashInputs) (string, error) {
	p.Timestamp = p.Timestamp.UTC().Truncate(time.Microsecond)
	canonicalPayload, err := canonicalizeJSON(p.Payload)
	if err != nil {
		return "", fmt.Errorf("verifier: canonicalize payload: %w", err)
	}
	p.Payload = canonicalPayload
	canonical, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("verifier: marshal hash inputs: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// canonicalizeJSON returns a re-marshaled form of `raw` so the bytes
// used for hashing don't depend on the storage layer's serialization
// choices. Mirrors the backend's helper of the same name; intentional
// duplication per ADR-008 (no shared code between backend and verifier).
func canonicalizeJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		return nil, err
	}
	return json.Marshal(parsed)
}
