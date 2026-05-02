package audit

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
)

// ExportSchemaV1 is the schema string the verifier currently
// understands. Future versions of the export format land as a new
// constant; the verifier rejects anything else explicitly so silent
// upgrades don't pass the wrong fields through unchecked.
const ExportSchemaV1 = "v1"

// Export is the JSON shape the backend's compliance export (E9)
// produces. Each run is keyed by UUID and carries its public key
// plus the full audit chain.
type Export struct {
	Schema     string             `json:"schema"`
	ExportedAt time.Time          `json:"exported_at"`
	Runs       map[string]RunData `json:"runs"`
}

// RunData is one run's portion of an export.
type RunData struct {
	SigningKey   *SigningKey `json:"signing_key,omitempty"`
	AuditEntries []Entry     `json:"audit_entries"`
}

// SigningKey is the persisted public-half record. PublicKey is
// base64-encoded so JSON exports stay text-safe.
type SigningKey struct {
	PublicKey string    `json:"public_key"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Entry mirrors backend's audit_entries row in JSON-friendly
// shape. Field types align with HashInputs so we can reconstruct
// the hash inputs from a parsed entry.
type Entry struct {
	ID           uuid.UUID       `json:"id"`
	Sequence     int64           `json:"sequence"`
	RunID        uuid.UUID       `json:"run_id"`
	StageID      *uuid.UUID      `json:"stage_id"`
	Timestamp    time.Time       `json:"ts"`
	Category     string          `json:"category"`
	ActorKind    *ActorKind      `json:"actor_kind"`
	ActorSubject *string         `json:"actor_subject"`
	Payload      json.RawMessage `json:"payload"`
	PrevHash     *string         `json:"prev_hash"`
	EntryHash    string          `json:"entry_hash"`
}

// ToHashInputs returns the HashInputs an entry's hash was computed
// over. Identity transformation; useful so verify.go can recompute.
func (e Entry) ToHashInputs() HashInputs {
	return HashInputs{
		RunID:        e.RunID,
		StageID:      e.StageID,
		Timestamp:    e.Timestamp,
		Category:     e.Category,
		ActorKind:    e.ActorKind,
		ActorSubject: e.ActorSubject,
		Payload:      e.Payload,
		PrevHash:     e.PrevHash,
	}
}

// DecodePublicKey converts the base64-encoded PublicKey string to
// raw bytes (32 bytes for Ed25519 per ADR-008).
func (k SigningKey) DecodePublicKey() ([]byte, error) {
	pub, err := base64.StdEncoding.DecodeString(k.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("audit: decode public key: %w", err)
	}
	return pub, nil
}

// ParseExport reads a JSON export from r and validates the schema
// version. Strict on unknown top-level fields so a typo or
// upgraded-but-not-here format produces a clear error rather than
// a silent skip.
func ParseExport(r io.Reader) (*Export, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var ex Export
	if err := dec.Decode(&ex); err != nil {
		return nil, fmt.Errorf("audit: parse export: %w", err)
	}
	if ex.Schema != ExportSchemaV1 {
		return nil, fmt.Errorf("audit: unsupported export schema %q (want %q)", ex.Schema, ExportSchemaV1)
	}
	if ex.Runs == nil {
		return nil, fmt.Errorf("audit: export missing 'runs' object")
	}
	return &ex, nil
}
