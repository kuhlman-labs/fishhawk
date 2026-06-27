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

// Kind enumerates the artifact kinds. Closed set per the schema's
// CHECK constraint (migration 0002, widened by 0037): plan,
// pull_request, deployment.
type Kind string

// Artifact kinds.
const (
	KindPlan        Kind = "plan"
	KindPullRequest Kind = "pull_request"
	// KindDeployment is ADR-038's signed deploy record (E23.5 / #1385):
	// the governance artifact a delegating deploy stage emits capturing
	// {environment, ref/sha, external_run_url, outcome, rollback_handle}.
	// Admitted by migration 0037, which widens artifacts_kind_check.
	KindDeployment Kind = "deployment"
)
