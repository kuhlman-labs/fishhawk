// Package artifact persists typed stage outputs (plans, pull-request
// references) and looks them up by stage or by content hash. The
// canonical schema is in
// internal/postgres/migrations/0002_audit_log_schema.up.sql; this
// package owns the typed Go domain and the Postgres adapter that
// implements Repository.
//
// Small artifacts (plans validated against standard_v1, PR metadata)
// live inline as JSONB. Trace bundles ship to S3 and are tracked in
// a separate trace_bundles table under E2.2 (#23).
package artifact

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Artifact is a typed stage output persisted in the audit log.
type Artifact struct {
	ID            uuid.UUID
	StageID       uuid.UUID
	Kind          Kind
	SchemaVersion *string
	Content       json.RawMessage
	ContentHash   string
	CreatedAt     time.Time
}

// Kind enumerates the v0 artifact kinds. Closed set per the schema's
// CHECK constraint.
type Kind string

// Artifact kinds.
const (
	KindPlan        Kind = "plan"
	KindPullRequest Kind = "pull_request"
)
