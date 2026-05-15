// Package approval owns the gate-decision side of the workflow
// run lifecycle (E3.5 / #45). Approvals are first-class records:
// who said yes/no, when, from what surface, with what comment.
//
// State-machine integration: the HTTP handler holds a SELECT FOR
// UPDATE on the stage row inside the same transaction as the
// approval INSERT, so concurrent approvers can't fork the state
// machine. Approve transitions stage → succeeded; reject
// transitions stage → failed with category D (per OpenAPI). SLA
// timeout handling is its own follow-up — the same category-D
// transition fires when a background job observes the elapsed
// gate.
package approval

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Decision is the closed set of values a submission can carry.
// Mirrors docs/api/v0.openapi.yaml's enum.
type Decision string

// Decision values; the schema's CHECK constraint mirrors this set.
const (
	DecisionApprove Decision = "approve"
	DecisionReject  Decision = "reject"
)

// Valid reports whether d is one of the closed-set values.
func (d Decision) Valid() bool {
	return d == DecisionApprove || d == DecisionReject
}

// Surface identifies where the approval came from. Captured for
// the audit log so a post-hoc reviewer can see whether the gate
// was passed in the UI, via the CLI, or in a GitHub comment.
type Surface string

// Surface values; the schema's CHECK constraint mirrors this set.
const (
	SurfaceAPI           Surface = "api"
	SurfaceUI            Surface = "ui"
	SurfaceCLI           Surface = "cli"
	SurfaceGitHubComment Surface = "github_comment"
	// SurfaceGitHubReplyComment tags approvals submitted via a
	// typed reply pattern (+1 / lgtm / 👍) on the originating
	// issue thread (E17.4 / #339). Distinct from
	// SurfaceGitHubComment (the explicit slash command) so the
	// audit log can attribute the decision to the right UX
	// affordance — slash commands carry a typed rationale and
	// produce explicit replies on misuse; reply-pattern approvals
	// have no rationale and silently skip on misuse.
	SurfaceGitHubReplyComment Surface = "github_reply_comment"
)

// Valid reports whether s is one of the closed-set values.
func (s Surface) Valid() bool {
	switch s {
	case SurfaceAPI, SurfaceUI, SurfaceCLI, SurfaceGitHubComment, SurfaceGitHubReplyComment:
		return true
	}
	return false
}

// Approval is the persisted record. Append-only (the Postgres
// schema enforces this with triggers).
type Approval struct {
	ID              uuid.UUID
	StageID         uuid.UUID
	ApproverSubject string
	Decision        Decision
	Comment         *string
	Surface         Surface
	SubmittedAt     time.Time
}

// SubmitParams are the fields required to record a new approval.
// Idempotency is by (StageID, ApproverSubject): a second Submit
// from the same approver returns the existing row.
type SubmitParams struct {
	StageID         uuid.UUID
	ApproverSubject string
	Decision        Decision
	Comment         *string
	Surface         Surface
}

// SubmitResult is what Submit returns: the canonical record plus a
// flag telling the caller whether this Submit produced a new row
// (Inserted=true) or returned a previously-recorded one.
type SubmitResult struct {
	Approval *Approval
	Inserted bool
}

// Errors callers may want to switch on.
var (
	// ErrInvalidDecision is returned for an out-of-set decision
	// value at the application layer (the schema CHECK is the
	// belt-and-suspenders backstop).
	ErrInvalidDecision = errors.New("approval: invalid decision")

	// ErrInvalidSurface is returned for an out-of-set surface
	// value.
	ErrInvalidSurface = errors.New("approval: invalid surface")

	// ErrEmptyApprover is returned when the approver subject is
	// empty — defense in depth before hitting the DB's NOT NULL.
	ErrEmptyApprover = errors.New("approval: approver subject required")
)

// Repository persists approvals. UPDATE/DELETE is intentionally
// absent; the schema enforces append-only at the DB layer.
type Repository interface {
	// Submit upserts an approval. Idempotent on (StageID,
	// ApproverSubject) — calling twice with the same approver
	// returns the existing row's contents and Inserted=false.
	Submit(ctx context.Context, p SubmitParams) (*SubmitResult, error)

	// ListForStage returns every approval recorded against a
	// stage, ordered by submitted_at ascending.
	ListForStage(ctx context.Context, stageID uuid.UUID) ([]*Approval, error)
}
