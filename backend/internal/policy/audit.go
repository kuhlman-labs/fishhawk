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
//
// SkipReason + SkipDetail are populated only when EmitEvaluationSkipped
// fires — e.g., when the run has no cached spec to evaluate against,
// when the bundle's diff event is malformed, or when the stage type
// isn't in the workflow spec. In those cases the auditable signal is
// "we tried and here's why we couldn't evaluate" rather than nothing
// at all (#283).
type EvaluationPayload struct {
	StageType  string      `json:"stage_type,omitempty"`
	Diff       []DiffEntry `json:"diff"`
	Applied    Constraints `json:"applied_constraints"`
	Violations []Violation `json:"violations"`
	Passed     bool        `json:"passed"`
	SkipReason SkipReason  `json:"skip_reason,omitempty"`
	SkipDetail string      `json:"skip_detail,omitempty"`
	// DeferredOutcomes names required_outcomes that evaluation
	// declined to assert on because no signal was available at
	// evaluation time (#297). At trace-upload time the only entry
	// here is `ci_green`: CI hasn't started against the just-opened
	// PR, so branch protection (#251 / ADR-017) is the actual gate
	// at merge time. The SPA renders this as an info note next to
	// the pass state. Omitted when nothing was deferred.
	DeferredOutcomes []string `json:"deferred_outcomes,omitempty"`
}

// SkipReason names why a policy evaluation couldn't be carried out.
// Closed set so the SPA can branch on shape (#283).
type SkipReason string

// SkipReason values.
const (
	// SkipSpecUnavailable: the run row has no cached workflow spec
	// (legacy row pre-#283 migration). Treat as pass; the auditable
	// signal is "evaluation skipped, spec not available."
	SkipSpecUnavailable SkipReason = "spec_unavailable"
	// SkipSpecUnparseable: the cached spec failed to parse. Indicates
	// data corruption or a parser regression — surface in the audit
	// log so an operator can investigate without grepping logs.
	SkipSpecUnparseable SkipReason = "spec_unparseable"
	// SkipWorkflowNotInSpec: the run's workflow_id isn't a key in
	// the cached spec's workflows map. Spec was edited between
	// run-create and re-eval, OR the cache was populated against
	// the wrong spec.
	SkipWorkflowNotInSpec SkipReason = "workflow_not_in_spec"
	// SkipStageNotInSpec: the run's stage type isn't represented in
	// the matched workflow definition. Same as above but at the
	// stage level.
	SkipStageNotInSpec SkipReason = "stage_not_in_spec"
	// SkipNoDiffInBundle: the trace bundle didn't carry a parseable
	// git_diff event (runner didn't pass --check-base-ref, or the
	// event was malformed). We can't evaluate path-based constraints
	// against an empty diff meaningfully; emit skipped so the SPA
	// renders the reason rather than "policy passed · no
	// constraints" which is misleading.
	SkipNoDiffInBundle SkipReason = "no_diff_in_bundle"
)

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
		StageType:        stageType,
		Diff:             entries,
		Applied:          constraints,
		Violations:       append([]Violation(nil), violations...),
		Passed:           len(violations) == 0,
		DeferredOutcomes: DeferredRequiredOutcomes(constraints),
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

// EmitEvaluationSkipped records an audit entry signaling that a
// policy evaluation was attempted but couldn't be carried out — the
// spec was missing, the diff was malformed, etc. The audit category
// stays `policy_evaluated` (so consumers index on one name); the
// payload's `skip_reason` + `skip_detail` carry the structured cause
// for the SPA to render and for compliance review.
//
// Returns nil on success. Passed=true on the payload so downstream
// gates don't treat the skip as a violation.
func EmitEvaluationSkipped(
	ctx context.Context,
	repo audit.Repository,
	runID uuid.UUID,
	stageID uuid.UUID,
	stageType string,
	reason SkipReason,
	detail string,
) error {
	payload, err := json.Marshal(EvaluationPayload{
		StageType:  stageType,
		Diff:       []DiffEntry{},
		Applied:    Constraints{},
		Violations: []Violation{},
		Passed:     true,
		SkipReason: reason,
		SkipDetail: detail,
	})
	if err != nil {
		return fmt.Errorf("policy: marshal skipped audit payload: %w", err)
	}

	systemKind := audit.ActorSystem
	if _, err := repo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryPolicyEvaluated,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		return fmt.Errorf("policy: append skipped audit entry: %w", err)
	}
	return nil
}
