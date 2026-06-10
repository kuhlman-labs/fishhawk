// Package scopeamendment persists the mid-stage operator-gated scope
// amendment requests an implement agent files while its stage is
// running (E22.X / #961). The agent POSTs the paths it needs folded
// into the effective scope.files plus a reason; the operator approves
// or denies in-loop; both the request and the decision land as audit
// entries (scope_amendment_requested / scope_amendment_decided) on the
// owning stage. Approved rows are folded into the prompt-fetch scope
// AND the runner's pre-commit scope refresh, so the #960 verified-tree
// gates verify the same folded tree that is pushed.
//
// Mirrors mcptoken's layout: domain types + Repository here,
// queries.sql + sqlc-generated ./db, postgres.go implementing the
// Repository against pgx.
package scopeamendment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Status is the lifecycle of one amendment request. The agent's poll
// loop terminates when status leaves pending.
type Status string

// Statuses. Pending is the creation state; Decide moves a row to
// approved or denied exactly once.
const (
	StatusPending  Status = "pending"
	StatusApproved Status = "approved"
	StatusDenied   Status = "denied"
)

// Operation is the per-path operation an amendment declares. Mirrors
// the standard_v1 scope.files operation vocabulary, restricted to the
// two an amendment can widen: modify (an existing file the plan didn't
// declare) and create (a net-new file the #818/#825 gates would
// otherwise fail loud on).
const (
	OperationModify = "modify"
	OperationCreate = "create"
)

// Errors callers switch on. Mirror mcptoken's shape so handlers map
// them to HTTP statuses uniformly.
var (
	// ErrNotFound means no amendment row matches the lookup.
	ErrNotFound = errors.New("scopeamendment: not found")

	// ErrAlreadyDecided means the amendment has already been
	// approved or denied; the decision endpoint maps it to 409.
	ErrAlreadyDecided = errors.New("scopeamendment: already decided")
)

// PathEntry is one requested path + its operation.
type PathEntry struct {
	Path      string `json:"path"`
	Operation string `json:"operation"`
}

// Amendment is the public-facing record of one request.
type Amendment struct {
	ID             uuid.UUID
	RunID          uuid.UUID
	StageID        uuid.UUID
	Paths          []PathEntry
	Reason         string
	Status         Status
	DecisionReason *string
	DecidedBy      *string
	RequestedAt    time.Time
	DecidedAt      *time.Time
}

// CreateParams bundles the inputs to Create.
type CreateParams struct {
	RunID   uuid.UUID
	StageID uuid.UUID
	Paths   []PathEntry
	Reason  string
}

// DecideParams bundles the inputs to Decide.
type DecideParams struct {
	ID        uuid.UUID
	Status    Status // StatusApproved or StatusDenied
	Reason    string
	DecidedBy string
}

// Repository persists amendments.
type Repository interface {
	// Create persists a fresh pending amendment.
	Create(ctx context.Context, p CreateParams) (*Amendment, error)

	// GetByID returns the amendment regardless of status.
	GetByID(ctx context.Context, id uuid.UUID) (*Amendment, error)

	// ListByRun returns every amendment for the run, oldest first.
	ListByRun(ctx context.Context, runID uuid.UUID) ([]*Amendment, error)

	// CountByStage returns the number of amendment rows for the
	// stage, regardless of status — a denied request still consumes
	// per-stage budget (the cap bounds operator interruptions, not
	// approvals).
	CountByStage(ctx context.Context, stageID uuid.UUID) (int, error)

	// Decide transitions a pending amendment to approved/denied.
	// ErrAlreadyDecided when the row has already left pending;
	// ErrNotFound when no row matches.
	Decide(ctx context.Context, p DecideParams) (*Amendment, error)
}

// ValidatePaths normalizes and validates requested path entries. Same
// containment contract as the fix-up allow_create validation
// (server/fixup.go validateAllowCreate, #823): each path is trimmed;
// empty/whitespace-only, absolute, and ".."-containing entries are
// rejected so the amendment stays repo-relative and cannot widen scope
// outside the tree. Operation must be modify or create. Returns the
// normalized entries, or an error describing the first bad entry.
func ValidatePaths(paths []PathEntry) ([]PathEntry, error) {
	if len(paths) == 0 {
		return nil, errors.New("paths must name at least one file")
	}
	out := make([]PathEntry, 0, len(paths))
	for _, e := range paths {
		trimmed := strings.TrimSpace(e.Path)
		if trimmed == "" {
			return nil, errors.New("paths entries must be non-empty repo-relative paths")
		}
		if strings.HasPrefix(trimmed, "/") {
			return nil, fmt.Errorf("path %q must be repo-relative, not absolute", trimmed)
		}
		if strings.Contains(trimmed, "..") {
			return nil, fmt.Errorf("path %q must not contain '..'", trimmed)
		}
		if e.Operation != OperationModify && e.Operation != OperationCreate {
			return nil, fmt.Errorf("path %q operation must be %q or %q, got %q",
				trimmed, OperationModify, OperationCreate, e.Operation)
		}
		out = append(out, PathEntry{Path: trimmed, Operation: e.Operation})
	}
	return out, nil
}
