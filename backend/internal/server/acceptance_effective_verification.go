package server

import (
	"context"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// Compile-time proof that *Server satisfies the orchestrator's back-reference
// hook, so a signature drift is a build error rather than a silently unwired
// field.
var _ orchestrator.EffectiveAcceptanceResolver = (*Server)(nil)

// EffectiveAcceptanceVerification implements
// orchestrator.EffectiveAcceptanceResolver (#3181): it returns a COPY of the
// approved plan's verification whose acceptance_criteria are replaced by the
// run's effective live set — operator retirements removed, restatements
// applied, operator-added criteria appended after every plan-origin one. Every
// other verification field is the plan's own.
//
// It is a thin adapter over resolveEffectiveAcceptanceCriteria, the single seam
// that computes the effective set; nothing here recomputes any part of it. An
// un-amended run returns the plan's verification verbatim, so the
// orchestrator's short-circuit predicates answer exactly as before #3181. A
// resolve error is returned to the caller, which falls back to the plan
// verbatim (WARN-logged) — the same fail-open direction the prompt builder
// takes.
func (s *Server) EffectiveAcceptanceVerification(ctx context.Context, runID uuid.UUID, p *plan.Plan) (plan.Verification, error) {
	if p == nil {
		return plan.Verification{}, nil
	}
	eff, err := s.resolveEffectiveAcceptanceCriteria(ctx, runID, p, nil)
	if err != nil {
		return plan.Verification{}, err
	}
	v := p.Verification
	if !eff.amended() {
		return v, nil
	}
	v.AcceptanceCriteria = append([]plan.AcceptanceCriterion(nil), eff.Live...)
	return v, nil
}
