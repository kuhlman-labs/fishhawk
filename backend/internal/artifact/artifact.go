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
// CHECK constraint (migration 0002, widened by 0037, 0045, 0051, 0073, 0083,
// 0095, 0098 and 0117): plan, pull_request, deployment, acceptance,
// release_notes, grooming_report, acceptance_transcript, upkeep_report,
// comms_report, conflict_resolution_diff.
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
	// KindGroomingReport is E54's backlog-grooming proposal record
	// (E54.3 / #2235, ADR-065 §3): the durable artifact a `plan`-typed
	// PROPOSE stage emits carrying the proposed ordering (each entry citing
	// at least one charter rubric line), duplicate candidates, hygiene
	// defects, suggested depends_on edges, vision-drift flags, and
	// decomposition suggestions, validated against grooming-report-v1.
	// Admitted by migration 0073, which widens artifacts_kind_check; the
	// constant and migration ship together (a Create with this kind fails
	// SQLSTATE 23514 against the un-widened CHECK), exactly as 0037 paired
	// with KindDeployment, 0045 with KindAcceptance, and 0051 with
	// KindReleaseNotes.
	KindGroomingReport Kind = "grooming_report"
	// KindAcceptanceTranscript is E72.5's per-criterion acceptance TRANSCRIPT
	// (#3329): the runner-shipped record of every request sent, response
	// observed, assertion evaluated and its outcome, persisted by POST
	// /v0/runs/{run_id}/acceptance/transcript beside the boolean verdict
	// (KindAcceptance), whose runner-injected `transcript` ref is
	// cross-checked against it. Admitted by migration 0083, which widens
	// artifacts_kind_check; the constant and migration ship together (a
	// Create with this kind fails SQLSTATE 23514 against the un-widened
	// CHECK), exactly as 0073 paired with KindGroomingReport.
	KindAcceptanceTranscript Kind = "acceptance_transcript"
	// KindUpkeepReport is E79's upkeep-scan proposal record (E79.2 / #3726):
	// the durable artifact a `plan`-typed PROPOSE stage emits carrying the
	// recorded-run flake, toolchain-pin drift and deprecation findings to
	// file, validated against upkeep_report_v1 (contract and ingest handler:
	// #3921). Admitted by migration 0095, which widens artifacts_kind_check;
	// the constant and migration ship together (a Create with this kind fails
	// SQLSTATE 23514 against the un-widened CHECK), exactly as 0073 paired
	// with KindGroomingReport and 0083 with KindAcceptanceTranscript.
	KindUpkeepReport Kind = "upkeep_report"
	// KindCommsReport is E81.5's user-report-scan proposal record (#3775):
	// the durable artifact a `plan`-typed PROPOSE stage emits carrying the
	// draft responses and issues proposed from user-authored reports,
	// validated against comms_report_v1 (contract and ingest handler:
	// #4015). Admitted by migration 0098, which widens artifacts_kind_check;
	// the constant and migration ship together (a Create with this kind fails
	// SQLSTATE 23514 against the un-widened CHECK), exactly as 0095 paired
	// with KindUpkeepReport.
	KindCommsReport Kind = "comms_report"
	// KindConflictResolutionDiff is the REDACTED remerge-diff summary of a
	// conflict-resolution pass (#4795): how the conflicted files of a merge
	// commit were resolved, written by the conflict_resolution_pushed report
	// handler on the implement stage the report arrived on, validated as
	// conflict_resolution_diff_v1, and read back ONLY through the server's
	// single framing loader, which renders it as an additional section of the
	// conflict-resolution implement-review round. Admitted by migration 0117,
	// which widens artifacts_kind_check; the constant and migration ship
	// together (a Create with this kind fails SQLSTATE 23514 against the
	// un-widened CHECK), exactly as 0098 paired with KindCommsReport.
	KindConflictResolutionDiff Kind = "conflict_resolution_diff"
)

// ConflictResolutionDiffSchema is the SchemaVersion of a
// KindConflictResolutionDiff artifact (#4795).
const ConflictResolutionDiffSchema = "conflict_resolution_diff_v1"

// ConflictResolutionDiffContent is the JSON content of a
// KindConflictResolutionDiff artifact (#4795). Patch is the REDACTED remerge
// patch; PatchUnavailable is a closed slug naming why Patch is empty, and
// PatchUnavailableDetail a redacted, 256-byte-bounded runner detail that is
// stored for the operator and never rendered into a prompt.
type ConflictResolutionDiffContent struct {
	Schema                 string   `json:"schema"`
	HeadSHA                string   `json:"head_sha"`
	BaseSHA                string   `json:"base_sha"`
	ResolvedPaths          []string `json:"resolved_paths"`
	ResolvedPathsTruncated bool     `json:"resolved_paths_truncated,omitempty"`
	Patch                  string   `json:"patch,omitempty"`
	PatchTruncated         bool     `json:"patch_truncated,omitempty"`
	PatchUnavailable       string   `json:"patch_unavailable,omitempty"`
	PatchUnavailableDetail string   `json:"patch_unavailable_detail,omitempty"`
}

// Conflict-resolution-diff cross-check causes (#4795), returned by
// CheckConflictResolutionDiff. They are internal log causes, never rendered.
const (
	ConflictResolutionDiffCauseArtifactMissing = "artifact_missing"
	ConflictResolutionDiffCauseKindMismatch    = "kind_mismatch"
	ConflictResolutionDiffCauseStageMismatch   = "stage_mismatch"
	ConflictResolutionDiffCauseHashMismatch    = "hash_mismatch"
	ConflictResolutionDiffCauseHeadMismatch    = "head_mismatch"
	ConflictResolutionDiffCauseUndecodable     = "undecodable"
)

// CheckConflictResolutionDiff cross-checks a read-back artifact against the
// conflict_resolution_pushed row that names it and decodes its content. It
// returns the content and "" on success, else nil and the cause.
//
// The hash is compared COLUMN TO ROW: a's stored ContentHash against the row's
// resolution_diff_sha256. It is never recomputed over a.Content, because the
// artifacts table stores content as JSONB, which normalises whitespace and key
// order, so a recomputed hash would differ from the one computed at insert.
func CheckConflictResolutionDiff(a *Artifact, stageID uuid.UUID, rowSHA256, headSHA string) (*ConflictResolutionDiffContent, string) {
	switch {
	case a == nil:
		return nil, ConflictResolutionDiffCauseArtifactMissing
	case a.Kind != KindConflictResolutionDiff:
		return nil, ConflictResolutionDiffCauseKindMismatch
	case a.StageID != stageID:
		return nil, ConflictResolutionDiffCauseStageMismatch
	case rowSHA256 == "" || a.ContentHash != rowSHA256:
		return nil, ConflictResolutionDiffCauseHashMismatch
	}
	var c ConflictResolutionDiffContent
	if err := json.Unmarshal(a.Content, &c); err != nil || c.Schema != ConflictResolutionDiffSchema {
		return nil, ConflictResolutionDiffCauseUndecodable
	}
	if c.HeadSHA != headSHA {
		return nil, ConflictResolutionDiffCauseHeadMismatch
	}
	return &c, ""
}
