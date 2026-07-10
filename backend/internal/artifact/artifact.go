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
// CHECK constraint (migration 0002, widened by 0037, 0045, and 0051):
// plan, pull_request, deployment, acceptance, release_notes.
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
	// KindAcceptance is ADR-049's signed acceptance-evidence record
	// (E31.3 / #1531): the durable artifact an acceptance stage emits
	// capturing the structured verdict + per-criterion results +
	// content_hash references to customer-side evidence blobs. Admitted by
	// migration 0045, which widens artifacts_kind_check; the constant and
	// migration ship together (a Create with this kind fails SQLSTATE 23514
	// against the un-widened CHECK). Written by the E31.6 outcome handler.
	KindAcceptance Kind = "acceptance"
	// KindReleaseNotes is E33's evidence-derived release-notes record
	// (E33.2 / #1587, ADR-051 option B): the durable artifact the release-notes
	// persist endpoint emits capturing the rendered markdown assembled from the
	// releaseevidence model (per-change summary, plan link, reviewer verdicts,
	// acceptance outcome, deferred concerns, and the per-release cost rollup).
	// Admitted by migration 0051, which widens artifacts_kind_check; the
	// constant and migration ship together (a Create with this kind fails
	// SQLSTATE 23514 against the un-widened CHECK), exactly as 0037 paired with
	// KindDeployment and 0045 with KindAcceptance.
	KindReleaseNotes Kind = "release_notes"
)
