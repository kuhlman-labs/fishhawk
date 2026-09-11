package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryAcceptanceStageOmitted is the audit category of the durable marker
// the plan gate writes when an approved plan declares
// verification.acceptance_surface: none (E72.1 / #3325): the run's pending
// acceptance stage is OMITTED — physically deleted — rather than minted only
// to short-circuit to not_validated. The marker is scoped to the PLAN stage
// (never the acceptance stage it names: audit_entries.stage_id is ON DELETE
// RESTRICT and the row must outlive the stage) and is the merge gate's fifth
// merge-eligible disposition when no acceptance stage row exists (acceptance.go
// acceptanceGateOmitted). Registered in audit.KnownCategories.
const CategoryAcceptanceStageOmitted = "acceptance_stage_omitted"

// acceptanceStageOmissionBasis is the fixed basis string every
// acceptance_stage_omitted payload carries. There is exactly one trigger today;
// the field exists so a future trigger is distinguishable without a payload
// shape change.
const acceptanceStageOmissionBasis = "acceptance_surface_none"

// acceptanceStageOmitter is the OPTIONAL run-repository capability the omission
// hook asserts. It is deliberately NOT part of run.Repository: the many test
// fakes that never omit a stage need no stub, and a RunRepo lacking it makes
// the hook fail OPEN (stage retained, WARN logged) rather than fail the
// approval. The postgres repository implements it
// (run.(*postgresRepo).DeletePendingAcceptanceStage).
type acceptanceStageOmitter interface {
	DeletePendingAcceptanceStage(ctx context.Context, id uuid.UUID) (bool, error)
}

// acceptanceStageOmittedPayload is the acceptance_stage_omitted audit payload.
// Ids, a sequence, counts and a fixed basis constant — nothing derived from
// plan prose, so no redaction path is involved (#3338 hold).
type acceptanceStageOmittedPayload struct {
	OmittedStageID  uuid.UUID `json:"omitted_stage_id"`
	OmittedSequence int       `json:"omitted_sequence"`
	Basis           string    `json:"basis"`
	CriteriaTotal   int       `json:"criteria_total"`
	OutOfScopeCount int       `json:"out_of_scope_count"`
}

// omitAcceptanceStageForSurfaceNone drops a run's pending acceptance stage at
// plan approval when the approved plan declares
// verification.acceptance_surface: none (E72.1 / #3325). Called from
// finishApprovalAdvance on an approve of a plan stage, BEFORE
// Orchestrator.Advance, so the orchestrator never observes (and never
// dispatches or short-circuits) a stage the plan has declared moot.
//
// Best-effort like the sibling approval hooks: it never unwinds the approval
// and every failure WARN-logs and returns with the run in a SAFE state.
//
// WRITE ORDER IS DURABLE-RECORD-FIRST, and that ordering is the control:
//
//  1. append the acceptance_stage_omitted marker (scoped to the PLAN stage);
//  2. only then delete the pending acceptance stage row.
//
// The two reachable partial states are therefore both recoverable — (i) no
// marker + stage present (the append failed, or the process died before it):
// the run simply keeps its acceptance stage, which short-circuits as before;
// (ii) marker + stage present (the delete failed, or the process died between
// the writes): the next approval finds the marker by omitted_stage_id, skips
// the append and retries the idempotent delete. The stage-less-and-marker-less
// state — a run the merge gate would wedge at acceptance_pending forever — is
// unreachable BY CONSTRUCTION, not merely unlikely. Swapping the order
// (delete-then-append) is exactly what
// TestApprovePlan_AcceptanceSurfaceNone_AppendFails_StageRetainedNoMarker
// reddens on.
//
// Idempotency is anchored on the MARKER, never on the stage's absence: the
// marker set is read once per call and matched on omitted_stage_id, so a
// re-approval after a fully successful pass finds no pending acceptance stage
// and writes nothing, and a re-approval in state (ii) completes the delete
// without a second append. No lock wraps the list-then-append: the caller runs
// behind the approve-advance CAS, which admits exactly one approval into
// finishApprovalAdvance per gate (the loser is refused at gateActionAdvance
// with run.StageStateChangedError), so the marker append inherits that
// exclusion — a second lock here would only invite the next reader to conclude
// the CAS is insufficient.
//
// Ordering of the checks is load-bearing for the existing approval fixtures:
// the hook returns with ZERO side effects and ZERO audit reads unless the
// approved plan declares none, so a fixture whose audit fake errors on
// ListForRunByCategory is never touched by it.
func (s *Server) omitAcceptanceStageForSurfaceNone(ctx context.Context, planStage *run.Stage) {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return
	}
	runID := planStage.RunID

	approvedPlan, err := s.loadApprovedPlanForRun(ctx, runID)
	if err != nil {
		s.logAcceptanceOmissionWarn(ctx, runID, "load approved plan failed", err.Error())
		return
	}
	if approvedPlan == nil || !plan.DeclaresNoAcceptanceSurface(approvedPlan.Verification) {
		return // not a none plan → no reads, no writes
	}

	omitter, ok := s.cfg.RunRepo.(acceptanceStageOmitter)
	if !ok {
		s.logAcceptanceOmissionWarn(ctx, runID, "repo lacks DeletePendingAcceptanceStage; stage retained", "")
		return
	}

	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		s.logAcceptanceOmissionWarn(ctx, runID, "list stages failed", err.Error())
		return
	}
	var pending []*run.Stage
	for _, st := range stages {
		if st.Type == run.StageTypeAcceptance && st.State == run.StageStatePending {
			pending = append(pending, st)
		}
	}
	if len(pending) == 0 {
		return // already omitted, already dispatched, or never declared
	}

	// Marker set: the idempotency anchor. A read error fails OPEN — nothing is
	// written, the stage stays, and the next approval retries.
	marked, err := s.acceptanceOmissionMarkers(ctx, runID)
	if err != nil {
		s.logAcceptanceOmissionWarn(ctx, runID, "list omission markers failed", err.Error())
		return
	}

	for _, acc := range pending {
		if _, already := marked[acc.ID]; !already {
			// (1) Durable record FIRST. An append failure leaves state (i):
			// stage intact, no marker — safe, and retried by the next approval.
			if !s.appendAcceptanceOmissionMarker(ctx, planStage, acc, approvedPlan.Verification) {
				continue
			}
		}
		// (2) Only after a durable marker exists for THIS stage. A delete error
		// leaves state (ii): marker present, stage present — repaired by the
		// next approval, which finds the marker and retries this delete. false
		// (row already gone, or raced away) is silent.
		if _, derr := omitter.DeletePendingAcceptanceStage(ctx, acc.ID); derr != nil {
			s.logAcceptanceOmissionWarn(ctx, runID, "delete pending acceptance stage failed; marker present, stage retained", derr.Error())
		}
	}
}

// acceptanceOmissionMarkers decodes the run's acceptance_stage_omitted entries
// into the set of stage ids already marked. An undecodable payload is skipped
// (it cannot name a stage, so it cannot anchor idempotency for one).
func (s *Server) acceptanceOmissionMarkers(ctx context.Context, runID uuid.UUID) (map[uuid.UUID]struct{}, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryAcceptanceStageOmitted)
	if err != nil {
		return nil, err
	}
	marked := make(map[uuid.UUID]struct{}, len(entries))
	for _, e := range entries {
		var p acceptanceStageOmittedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil || p.OmittedStageID == uuid.Nil {
			continue
		}
		marked[p.OmittedStageID] = struct{}{}
	}
	return marked, nil
}

// appendAcceptanceOmissionMarker writes the acceptance_stage_omitted row scoped
// to the PLAN stage and reports whether it landed. The plan-stage scope is
// deliberate: the marker must outlive the acceptance stage it names, and
// audit_entries.stage_id is ON DELETE RESTRICT.
func (s *Server) appendAcceptanceOmissionMarker(ctx context.Context, planStage, acc *run.Stage, v plan.Verification) bool {
	payload, _ := json.Marshal(acceptanceStageOmittedPayload{
		OmittedStageID:  acc.ID,
		OmittedSequence: acc.Sequence,
		Basis:           acceptanceStageOmissionBasis,
		CriteriaTotal:   len(v.AcceptanceCriteria),
		OutOfScopeCount: len(v.OutOfScope),
	})
	planStageID := planStage.ID
	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     planStage.RunID,
		StageID:   &planStageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryAcceptanceStageOmitted,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.logAcceptanceOmissionWarn(ctx, planStage.RunID, "append acceptance_stage_omitted failed; stage retained", err.Error())
		return false
	}
	return true
}

func (s *Server) logAcceptanceOmissionWarn(ctx context.Context, runID uuid.UUID, msg, detail string) {
	attrs := []slog.Attr{slog.String("run_id", runID.String())}
	if detail != "" {
		attrs = append(attrs, slog.String("error", detail))
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "acceptance omission: "+msg, attrs...)
}
