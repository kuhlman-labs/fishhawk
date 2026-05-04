package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// CategoryPolicyEvaluated is the audit-log category for the chained
// entry produced by EmitEvaluation. Static so log scrapers and the
// compliance export can index on it.
const CategoryPolicyEvaluated = "policy_evaluated"

// EvaluationPayload is the JSON shape of the audit entry's payload
// for a policy evaluation. The full constraint config, the
// resulting violation set, and a derived `passed` boolean are
// included so the audit entry is self-contained — a reader doesn't
// need to cross-reference the workflow spec to interpret it.
type EvaluationPayload struct {
	StageType  string      `json:"stage_type,omitempty"`
	Diff       []DiffEntry `json:"diff"`
	Applied    Constraints `json:"applied_constraints"`
	Violations []Violation `json:"violations"`
	Passed     bool        `json:"passed"`
}

// DiffEntry is the per-file shape that lands in the payload. Mirrors
// ChangedFile but with json tags pinned for the audit on-wire format.
type DiffEntry struct {
	Path   string `json:"path"`
	Status Status `json:"status"`
}

// EmitEvaluation runs Evaluate against the diff + constraints and
// writes a single chained audit entry recording the full result.
// Returns the violations so the caller can decide whether to
// transition the stage to failed (one or more violations → category
// B per MVP_SPEC §6).
//
// The audit category is constant ("policy_evaluated"); the per-file
// and per-violation detail lives in the payload. Writing one entry
// per evaluation rather than one per violation keeps the chain
// compact and lets a reader see "what was checked" alongside "what
// failed."
//
// actorSubject is optional and identifies the agent or user whose
// stage output was evaluated; nil leaves it unset.
func EmitEvaluation(
	ctx context.Context,
	repo audit.Repository,
	runID uuid.UUID,
	stageID uuid.UUID,
	stageType string,
	diff Diff,
	constraints Constraints,
	actorSubject *string,
) ([]Violation, error) {
	violations := Evaluate(diff, constraints)

	entries := make([]DiffEntry, 0, len(diff.ChangedFiles))
	for _, f := range diff.ChangedFiles {
		entries = append(entries, DiffEntry(f))
	}

	payload, err := json.Marshal(EvaluationPayload{
		StageType:  stageType,
		Diff:       entries,
		Applied:    constraints,
		Violations: append([]Violation(nil), violations...),
		Passed:     len(violations) == 0,
	})
	if err != nil {
		return violations, fmt.Errorf("policy: marshal audit payload: %w", err)
	}

	systemKind := audit.ActorSystem
	if _, err := repo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &stageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryPolicyEvaluated,
		ActorKind:    &systemKind,
		ActorSubject: actorSubject,
		Payload:      payload,
	}); err != nil {
		return violations, fmt.Errorf("policy: append audit entry: %w", err)
	}

	return violations, nil
}
