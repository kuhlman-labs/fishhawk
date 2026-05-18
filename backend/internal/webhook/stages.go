package webhook

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// StageCreator is the slice of run.Repository CreateStagesFromSpec
// needs. Defining it here keeps the helper free of the full
// repository interface so server-side callers can pass any type
// that knows how to persist a stage (notably run.Repository, which
// satisfies it).
type StageCreator interface {
	CreateStage(ctx context.Context, p run.CreateStageParams) (*run.Stage, error)
}

// CreateStagesFromSpec translates the workflow spec's stage
// definitions into Stage rows (in StagePending) on the named run.
// Returns the created stages in spec order so callers can address
// the first one.
//
// Mapping decisions:
//   - sequence is the position in the spec's stages array (0-based).
//   - executorKind comes from spec.Executor: agent → ExecutorAgent,
//     human → ExecutorHuman.
//   - executorRef is the agent name for agent stages and a
//     conventional "human" string for human stages — the field is
//     non-nullable in the DB schema, and we never read it for human
//     stages.
//
// Shared between the webhook dispatcher and the API runs handler
// (#411): both paths need to translate spec → stage rows from the
// same source of truth so spec-quirks land in one place.
func CreateStagesFromSpec(ctx context.Context, runs StageCreator, runID uuid.UUID, defs []spec.Stage) ([]*run.Stage, error) {
	out := make([]*run.Stage, 0, len(defs))
	for i, def := range defs {
		execKind, execRef := mapExecutor(def)
		params := run.CreateStageParams{
			RunID:        runID,
			Sequence:     i,
			Type:         run.StageType(def.Type),
			ExecutorKind: execKind,
			ExecutorRef:  execRef,
		}
		if sla := firstApprovalSLA(def.Gates); sla != "" {
			params.GateSLA = &sla
		}
		params.RequiresApproval = hasApprovalGate(def.Gates)
		params.Gate = primaryGate(def.Gates)
		stage, err := runs.CreateStage(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("create stage %d (%s): %w", i, def.ID, err)
		}
		out = append(out, stage)
	}
	return out, nil
}

// WorkflowMaxRetries returns the spec's on_ci_failure.max_retries
// value, defaulting to spec.DefaultMaxRetries when the block is
// absent. Used by the run-create path to snapshot the cap on the
// runs row (#280) so the SPA can render "Retry N/M" without
// re-parsing the spec.
func WorkflowMaxRetries(wf spec.Workflow) int {
	if wf.OnCIFailure != nil {
		return wf.OnCIFailure.MaxRetries
	}
	return spec.DefaultMaxRetries
}

// firstApprovalSLA returns the first non-empty SLA from any
// approval gate in the stage's Gates list. Returns "" when no gate
// has an SLA (or no approval gate exists).
func firstApprovalSLA(gates []spec.Gate) string {
	for _, g := range gates {
		if g.Type == spec.GateTypeApproval && g.SLA != "" {
			return g.SLA
		}
	}
	return ""
}

// hasApprovalGate reports whether any of the stage's gates is the
// approval type. The trace upload handler reads this through
// stages.requires_approval to pick the right post-upload state
// (gated → awaiting_approval, gateless → succeeded). #207.
func hasApprovalGate(gates []spec.Gate) bool {
	for _, g := range gates {
		if g.Type == spec.GateTypeApproval {
			return true
		}
	}
	return false
}

// primaryGate picks the gate to persist on the stages row (#213).
// Approval gates win over check gates so the review-stage UI can
// always reach the approvers when they're declared. Returns nil for
// stages with no gate.
func primaryGate(gates []spec.Gate) *run.Gate {
	if len(gates) == 0 {
		return nil
	}
	g := pickPrimaryGate(gates)
	if g == nil {
		return nil
	}
	out := &run.Gate{
		Kind: run.GateKind(g.Type),
	}
	if g.Approvers != nil {
		out.Approvers = &run.GateApprovers{
			AnyOf: g.Approvers.AnyOf,
			AllOf: g.Approvers.AllOf,
		}
	}
	return out
}

// pickPrimaryGate is the inner choice — first approval gate if any,
// else first check gate. Split out from primaryGate so the policy
// is unit-testable without a run.Gate round-trip.
func pickPrimaryGate(gates []spec.Gate) *spec.Gate {
	for i := range gates {
		if gates[i].Type == spec.GateTypeApproval {
			return &gates[i]
		}
	}
	for i := range gates {
		if gates[i].Type == spec.GateTypeCheck {
			return &gates[i]
		}
	}
	return nil
}

// mapExecutor projects a spec.Executor onto the run-package
// executor enum. Per the schema, exactly one of Agent / Human is
// set; we trust that here rather than reasserting it.
func mapExecutor(s spec.Stage) (run.ExecutorKind, string) {
	if s.Executor.Human {
		return run.ExecutorHuman, "human"
	}
	return run.ExecutorAgent, s.Executor.Agent
}
