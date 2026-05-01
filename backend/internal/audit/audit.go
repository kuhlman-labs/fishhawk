// Package audit persists the append-only event log that is the
// product's central artifact (MVP_SPEC §4.4). The Repository
// interface deliberately exposes only Append and read methods —
// no Update, no Delete — so the type system enforces the
// append-only invariant at the Go API level. The schema
// (internal/postgres/migrations/0002_audit_log_schema.up.sql)
// adds matching triggers that block UPDATE/DELETE at the database
// level even if a buggy code path tried.
//
// Chain-hash population (prev_hash, entry_hash) is the caller's
// responsibility for now; E2.5 (#26) will layer a higher-level
// AppendChained helper that computes the hash from the previous
// entry within the run.
package audit

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Entry is one line in the audit log. Sequence is the monotonic
// per-table position assigned by Postgres at INSERT time.
type Entry struct {
	ID           uuid.UUID
	Sequence     int64
	RunID        uuid.UUID
	StageID      *uuid.UUID
	Timestamp    time.Time
	Category     string
	ActorKind    *ActorKind
	ActorSubject *string
	Payload      json.RawMessage
	PrevHash     *string
	EntryHash    string
}

// ActorKind enumerates who acted to produce the entry. Closed set
// per the schema CHECK; nil is also valid (system-emitted events
// with no clear actor).
type ActorKind string

// Actor kinds.
const (
	ActorAgent  ActorKind = "agent"
	ActorUser   ActorKind = "user"
	ActorSystem ActorKind = "system"
)
