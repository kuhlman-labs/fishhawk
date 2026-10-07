package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// upkeep_report stage binding (#3921). An upkeep_report is bound to the
// shipping stage's DECLARATION — `produces: upkeep_report` on that stage in the
// run's cached workflow spec — never to the uploaded body's `kind` alone. These
// helpers resolve that declaration and make the two pure refusal decisions the
// ingest handler and the plan-path guard act on, plus the cited-run tenancy
// check. Contract: docs/spec/upkeep-report-v1.md § "Ingest" and § "Plan-path
// guard"; residuals: backend/internal/upkeep/README.md.

// Undecidable reasons carried by upkeepStageBinding: the shared seam's
// generic reasons (report_seam.go). Internal: the ingest collapses both into
// the caller-visible stage_binding_undecidable.
const (
	upkeepBindingWorkflowUnresolved = stageBindingWorkflowUnresolved
	upkeepBindingStageUnmappable    = stageBindingStageUnmappable
)

// Ingest refusal reasons — the details.reason values of a 400
// upkeep_report_stage_invalid.
const (
	upkeepRefusalStageBindingUndecidable = "stage_binding_undecidable"
	upkeepRefusalStageDoesNotDeclare     = "stage_does_not_declare_upkeep_report"
)

// upkeepStageBinding is what the run's cached workflow spec says about one
// stage and the upkeep_report artifact.
//
//   - Resolved: the run row, its cached spec and the run's workflow were all
//     found and parsed. False means there is no declaration to read
//     (Undecidable is then workflow_unresolved).
//   - WorkflowDeclaresUpkeep: some stage of the run's workflow declares
//     `produces: upkeep_report`.
//   - StageDeclaresUpkeep: THIS stage does.
//   - Undecidable: non-empty when the declaration could not be resolved for
//     this stage (workflow_unresolved, stage_unmappable).
type upkeepStageBinding struct {
	Resolved               bool
	WorkflowDeclaresUpkeep bool
	StageDeclaresUpkeep    bool
	Undecidable            string
}

// resolveUpkeepStageBinding reads the stage's upkeep_report declaration from
// the run's cached spec: resolveStageArtifactBinding (report_seam.go) with the
// kind fixed to upkeep_report, converted field-for-field.
//
// The returned error is reserved for a store that did not ANSWER (a GetRun or
// ListStagesForRun transport failure); the caller maps it to 500 and leaves
// the stage untouched. Every "no declaration exists" leg (nil RunRepo, no run
// row, no cached spec, unparseable spec, workflow absent) is Resolved=false
// with Undecidable=workflow_unresolved, and a stage the spec cannot be mapped
// onto is Undecidable=stage_unmappable — each caller decides its own posture
// from that (upkeepIngestRefusal fails closed, upkeepStageRefusesOtherProposal
// fails open).
//
// ListStagesForRun is called ONLY when the workflow declares upkeep_report on
// some stage, so ordinary workflows pay one GetRun and one spec parse and
// never list stages.
func (s *Server) resolveUpkeepStageBinding(ctx context.Context, runID uuid.UUID, stage *run.Stage) (upkeepStageBinding, error) {
	b, err := s.resolveStageArtifactBinding(ctx, runID, stage, spec.ArtifactUpkeepReport)
	if err != nil {
		return upkeepStageBinding{}, err
	}
	return upkeepStageBinding{
		Resolved:               b.Resolved,
		WorkflowDeclaresUpkeep: b.WorkflowDeclares,
		StageDeclaresUpkeep:    b.StageDeclares,
		Undecidable:            b.Undecidable,
	}, nil
}

// upkeepIngestRefusal is the upkeep_report ingest's binding decision. It
// returns the details.reason of a 400 upkeep_report_stage_invalid, or "" when
// the stage may ship an upkeep_report. It fails CLOSED: an undecidable binding
// refuses, and so does any binding (including the zero value) that does not
// positively record the stage's declaration.
func upkeepIngestRefusal(b upkeepStageBinding) string {
	if b.Undecidable != "" {
		return upkeepRefusalStageBindingUndecidable
	}
	if !b.StageDeclaresUpkeep {
		return upkeepRefusalStageDoesNotDeclare
	}
	return ""
}

// upkeepStageRefusesOtherProposal is the plan-path guard's decision: true when
// the stage must NOT have any artifact outside the upkeep allowlist
// (upkeep_report, and clarification_request, which parks and writes nothing
// approvable) ingested. It is kind-agnostic on purpose: the caller applies it
// to EVERY kind outside that allowlist, so a kind added later is refused by
// default.
//
// Fails OPEN when nothing is resolvable (Resolved=false: nil RunRepo, no run
// row, no cached spec), so deployments and runs that cannot resolve a workflow
// keep today's path; fails CLOSED inside a workflow that declares
// upkeep_report, where a stage that declares it, or one that cannot be mapped,
// refuses.
func upkeepStageRefusesOtherProposal(b upkeepStageBinding) bool {
	return stageRefusesOtherProposal(stageArtifactBinding{
		Resolved:         b.Resolved,
		WorkflowDeclares: b.WorkflowDeclaresUpkeep,
		StageDeclares:    b.StageDeclaresUpkeep,
		Undecidable:      b.Undecidable,
	})
}

// checkUpkeepRunRefs verifies every cited run id names a run in the reporting
// run's repository (case-insensitive) and, when BOTH rows carry an account id,
// in the same account; otherwise the check is repository-only. A ref with an
// empty repository never matches.
//
// It returns ok=true when every id is accepted, and the FIRST refused id with
// ok=false otherwise. Unknown and foreign refs are deliberately
// indistinguishable to the caller — the stage failure reason and audit rows
// are readable by the run's tenant, so which of the two it was goes ONLY to the
// WARN log. Any other GetRun error is returned (a 500 at the caller), as is a
// missing run repository or reporting run, since tenancy cannot be checked
// without them.
func (s *Server) checkUpkeepRunRefs(ctx context.Context, runRow *run.Run, ids []uuid.UUID) (uuid.UUID, bool, error) {
	if s.cfg.RunRepo == nil || runRow == nil {
		return uuid.Nil, false, errors.New("upkeep run-ref check: run repository or reporting run unavailable")
	}
	for _, id := range ids {
		ref, err := s.cfg.RunRepo.GetRun(ctx, id)
		reason := ""
		switch {
		case errors.Is(err, run.ErrNotFound):
			reason = "unknown_run"
		case err != nil:
			return uuid.Nil, false, fmt.Errorf("upkeep run-ref check: read run %s: %w", id, err)
		default:
			reason = upkeepRunOwnershipRefusal(ref, runRow)
		}
		if reason != "" {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "upkeep report: cited run refused",
				slog.String("run_id", runRow.ID.String()),
				slog.String("cited_run_id", id.String()),
				slog.String("reason", reason))
			return id, false, nil
		}
	}
	return uuid.Nil, true, nil
}

// Ownership refusal reasons returned by upkeepRunOwnershipRefusal.
const (
	upkeepOwnershipForeignRepo    = "foreign_repo"
	upkeepOwnershipForeignAccount = "foreign_account"
)

// upkeepRunOwnershipRefusal is the upkeep_report ownership predicate (#3921,
// extracted for #3922): "" when ref belongs to the reporting run's tenancy,
// otherwise the refusal reason. ref's repository must equal the reporting
// run's case-insensitively, and an empty repository never matches; the
// account is compared only when BOTH rows carry one, so an account-less row
// on either side is checked by repository alone.
//
// It has two callers that must agree: checkUpkeepRunRefs refuses a report
// citing a run it rejects, and the upkeep scan's flake gather
// (upkeep_evidence.go) drops a run it rejects BEFORE reading any trace, so the
// agent is only ever shown runs the ingest would accept.
func upkeepRunOwnershipRefusal(ref, reporting *run.Run) string {
	switch {
	case ref.Repo == "" || !strings.EqualFold(ref.Repo, reporting.Repo):
		return upkeepOwnershipForeignRepo
	case ref.AccountID != "" && reporting.AccountID != "" && ref.AccountID != reporting.AccountID:
		return upkeepOwnershipForeignAccount
	}
	return ""
}
