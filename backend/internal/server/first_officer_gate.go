package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/crewmessage"
	"github.com/kuhlman-labs/fishhawk/backend/internal/firstofficer"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/specialistjob"
)

// First-officer findings and the gate hold (ADR-089 D4, E85.6 / #4148).
//
// A seat's crew finding is ADVISORY like every other crew finding, with ONE
// exception: a finding whose evidence cites a runner-attested FAILING
// specialist-job completion on its anchor run is BLOCKING. The send handler
// resolves that evidence BEFORE the mailbox append (a read error refuses the
// send, so it is never recorded as advisory), and after the append converts
// the finding into a concern with provenance first_officer. While such a
// concern is open, plan approval, review approval and merge refuse 409
// first_officer_finding_open; the captain clears it through the existing
// accept / waive-with-reason / defer surface. Long-form contract:
// backend/internal/crewmessage/README.md ("First-officer findings").

// codeFirstOfficerFindingOpen is the 409 hold code on approve and merge.
const codeFirstOfficerFindingOpen = "first_officer_finding_open"

// firstOfficerHoldNextStep names how the captain clears the hold.
const firstOfficerHoldNextStep = "waive the concern with a reason (POST /v0/concerns/{concern_id}/waive), defer it (POST /v0/concerns/{concern_id}/defer), or route a fix-up naming it"

// firstOfficerBlockingBasis is the runner-attested failing completion a
// blocking seat finding cites.
type firstOfficerBlockingBasis struct {
	JobID             uuid.UUID `json:"specialist_job_id"`
	CompletedSequence int64     `json:"completed_sequence"`
}

// firstOfficerCheckKey is the converted concern's de-duplication key.
func firstOfficerCheckKey(jobID uuid.UUID) string {
	return "first_officer:job:" + jobID.String()
}

// resolveFirstOfficerBlockingEvidence decides whether a seat finding anchored
// on anchorRun is BLOCKING. It is blocking iff one of its audit_entry evidence
// refs is the decimal sequence of a specialist_job_completed entry on the
// anchor run's OWN chain whose job — re-read through the store under the
// run's account — is completed, verdict fail, runner_attested
// (specialistjob.BlockingEligible, #4363) and about the anchor run. Every
// other ref (bearer provenance, a pass/inconclusive verdict, another run's
// sequence, a non-numeric ref, a job the store does not hold) leaves the
// finding advisory: (nil, nil).
//
// FAIL-CLOSED: a chain or store read error is returned, and the caller
// refuses the send, so an outage never records a blocking finding as
// advisory. An unwired specialist-job store answers (nil, nil): no
// runner-attested completion can exist without it.
func (s *Server) resolveFirstOfficerBlockingEvidence(ctx context.Context, anchorRun *run.Run, evidence []crewmessage.EvidenceReference) (*firstOfficerBlockingBasis, error) {
	cited := map[int64]bool{}
	for _, ev := range evidence {
		if ev.Kind != crewmessage.EvidenceAuditEntry {
			continue
		}
		seq, err := strconv.ParseInt(ev.Ref, 10, 64)
		if err != nil || seq <= 0 {
			continue
		}
		cited[seq] = true
	}
	if len(cited) == 0 || s.cfg.SpecialistJobs == nil {
		return nil, nil
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, anchorRun.ID, specialistjob.CategoryCompleted)
	if err != nil {
		return nil, fmt.Errorf("list specialist_job_completed entries: %w", err)
	}
	acct := parseAccountUUID(anchorRun.AccountID)
	if anchorRun.AccountID != "" && acct == nil {
		return nil, fmt.Errorf("run %s account id %q is not a UUID", anchorRun.ID, anchorRun.AccountID)
	}
	for _, e := range entries {
		if e == nil || !cited[e.Sequence] {
			continue
		}
		var payload struct {
			JobID string `json:"job_id"`
		}
		if json.Unmarshal(e.Payload, &payload) != nil {
			continue
		}
		jobID, perr := uuid.Parse(payload.JobID)
		if perr != nil {
			continue
		}
		job, gerr := s.cfg.SpecialistJobs.Get(ctx, acct, jobID)
		if errors.Is(gerr, specialistjob.ErrNotFound) {
			continue
		}
		if gerr != nil {
			return nil, fmt.Errorf("get specialist job %s: %w", jobID, gerr)
		}
		if job.State == specialistjob.StateCompleted &&
			job.Verdict == specialistjob.VerdictFail &&
			specialistjob.BlockingEligible(job.Provenance) &&
			job.Subject.RunID == anchorRun.ID.String() {
			return &firstOfficerBlockingBasis{JobID: job.ID, CompletedSequence: e.Sequence}, nil
		}
	}
	return nil, nil
}

// crewConcernMint is the conversion core shared by the captain's
// convert-to-concern route and the automatic first-officer conversion: one
// concern for one finding, attached to the run's newest plan or implement
// stage.
type crewConcernMint struct {
	RunID      uuid.UUID
	Stage      *run.Stage
	StageKind  string
	Sequence   int64
	Msg        *crewmessage.Message
	Severity   string
	Provenance string
	CheckKey   string
}

// mintCrewFindingConcern inserts the concern and returns it. An empty insert
// result is an error.
func (s *Server) mintCrewFindingConcern(ctx context.Context, p crewConcernMint) (*concern.Concern, error) {
	minted, err := s.cfg.ConcernRepo.InsertRaised(ctx, concern.InsertRaisedParams{
		RunID:                p.RunID,
		StageID:              p.Stage.ID,
		StageKind:            p.StageKind,
		Provenance:           p.Provenance,
		OriginReviewSequence: p.Sequence,
		Concerns: []concern.RaisedConcern{{
			Severity: p.Severity,
			Category: crewConvertedConcernCategory,
			Note:     crewConvertedNote(p.Msg, p.Sequence),
			CheckKey: p.CheckKey,
		}},
	})
	if err == nil && len(minted) != 1 {
		err = errors.New("concern insert returned no row")
	}
	if err != nil {
		return nil, err
	}
	return minted[0], nil
}

// convertBlockingFirstOfficerFinding converts a just-sent blocking seat
// finding. Order is INSERT-THEN-DISPOSE (the reverse of the captain's route):
// the concern is inserted first, so a failure can never leave a disposed
// blocking finding with no concern. A failed insert leaves the finding OPEN
// (and advisory) and returns the error text for the 201 response; a failed
// dispose after a successful insert leaves the concern (the gate is held) plus
// a still-open finding. Both directions fail toward visibility.
func (s *Server) convertBlockingFirstOfficerFinding(ctx context.Context, runID uuid.UUID, msg *crewmessage.Message, row *crewmessage.Row, basis *firstOfficerBlockingBasis) (*uuid.UUID, string) {
	if s.cfg.ConcernRepo == nil {
		return nil, "the concern repository is not configured; the finding stays open and advisory"
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		return nil, "could not read the anchor run's stages: " + err.Error()
	}
	stage, stageKind, ok := crewConcernStage(stages)
	if !ok {
		return nil, "the anchor run has no plan or implement stage for a concern to attach to"
	}
	severity := msg.Payload.Severity
	if severity == "" {
		severity = crewConvertDefaultSeverity
	}
	c, err := s.mintCrewFindingConcern(ctx, crewConcernMint{
		RunID: runID, Stage: stage, StageKind: stageKind, Sequence: row.SentSequence, Msg: msg,
		Severity: severity, Provenance: concern.ProvenanceFirstOfficer, CheckKey: firstOfficerCheckKey(basis.JobID),
	})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError, "first-officer finding: concern insert failed; finding stays open",
			slog.String("run_id", runID.String()), slog.Int64("sent_sequence", row.SentSequence),
			slog.String("error", err.Error()))
		return nil, "the blocking concern could not be recorded: " + err.Error()
	}
	disposed, derr := s.cfg.CrewMailbox.Dispose(ctx, crewmessage.DisposeParams{
		SentSequence: row.SentSequence,
		Disposition:  crewmessage.DispositionAccepted,
		Reason: "converted to first_officer concern " + c.ID.String() +
			": runner-attested failing specialist job " + basis.JobID.String(),
		Actor: crewmessage.Actor{Kind: audit.ActorSystem},
	})
	var projErr *crewmessage.ProjectionError
	var dispSeq *int64
	projectionDegraded := errors.As(derr, &projErr) && disposed != nil
	if derr != nil && !projectionDegraded {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "first-officer finding: dispose after conversion failed; concern stands, finding stays open",
			slog.String("run_id", runID.String()), slog.Int64("sent_sequence", row.SentSequence),
			slog.String("concern_id", c.ID.String()), slog.String("error", derr.Error()))
	} else if disposed != nil {
		dispSeq = disposed.DispositionSequence
	}
	s.appendCrewConvertAudit(ctx, runID, stage.ID, CategoryCrewFindingConverted, crewFindingConvertedPayload{
		SentSequence:        row.SentSequence,
		DispositionSequence: dispSeq,
		ConcernID:           c.ID.String(),
		StageKind:           stageKind,
		Severity:            severity,
		Provenance:          concern.ProvenanceFirstOfficer,
		BlockingBasis:       basis,
	})
	id := c.ID
	return &id, ""
}

// firstOfficerHoldRefusal is the ONE predicate deciding the first-officer
// hold: the run's OPEN concerns with provenance first_officer, filtered to
// stageKind when non-empty (plan approve -> plan, review approve -> implement,
// merge -> ""). nil when none. A read error is returned (callers fail closed,
// 500, nothing recorded).
//
// An UNCONFIGURED concern repository answers (nil, nil) — no hold. That is a
// deliberate fail-open for the unwired case ONLY (approval condition 2): with
// no concern store no first_officer concern can exist, because the conversion
// itself needs that store. A read ERROR on a wired store stays fail-closed.
func (s *Server) firstOfficerHoldRefusal(ctx context.Context, runID uuid.UUID, stageKind string) (*mergeReadinessRefusal, error) {
	if s.cfg.ConcernRepo == nil {
		return nil, nil
	}
	open, err := s.cfg.ConcernRepo.ListOpenByRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list open concerns: %w", err)
	}
	var ids []string
	for _, c := range open {
		if c == nil || c.Provenance != concern.ProvenanceFirstOfficer {
			continue
		}
		if stageKind != "" && c.StageKind != stageKind {
			continue
		}
		ids = append(ids, c.ID.String())
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return &mergeReadinessRefusal{
		Code: codeFirstOfficerFindingOpen,
		Message: "a first-officer finding backed by a runner-attested failing specialist job is open on this run; " +
			"disposition it before approving or merging",
		Details: map[string]any{"concern_ids": ids, "next_step": firstOfficerHoldNextStep},
	}, nil
}

// refuseFirstOfficerApprovalHold applies the hold to an approval submission.
// Only an APPROVE is held — a reject is never held. A plan stage checks
// plan-kind concerns and a review stage implement-kind ones; every other
// stage type is not held. It writes the refusal and returns false (409 on a
// hold, 500 on a read error), and runs before ApprovalRepo.Submit, so a
// refused approve inserts no row.
func (s *Server) refuseFirstOfficerApprovalHold(w http.ResponseWriter, r *http.Request, stage *run.Stage, decision approval.Decision) bool {
	if decision != approval.DecisionApprove {
		return true
	}
	var kind string
	switch stage.Type {
	case run.StageTypePlan:
		kind = concern.StageKindPlan
	case run.StageTypeReview:
		kind = concern.StageKindImplement
	default:
		return true
	}
	hold, err := s.firstOfficerHoldRefusal(r.Context(), stage.RunID, kind)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"read open first-officer concerns failed", map[string]any{"error": err.Error()})
		return false
	}
	if hold != nil {
		details := map[string]any{"run_id": stage.RunID.String(), "stage_id": stage.ID.String()}
		for k, v := range hold.Details {
			details[k] = v
		}
		s.writeError(w, r, http.StatusConflict, hold.Code, hold.Message, details)
		return false
	}
	return true
}

// authorizeFirstOfficerSend is the seat branch's post-parse gate on a crew
// send (ADR-089 D4): a seat sends only a finding or a notice (never a
// work_request — that would be a tracker write — a consult or an
// escalation), only to the captain, and only anchored on a run inside its
// seat. Every refusal fires BEFORE any mailbox append. It returns the anchor
// run on success.
func (s *Server) authorizeFirstOfficerSend(w http.ResponseWriter, r *http.Request, seat firstofficer.Anchor, msg *crewmessage.Message) (*run.Run, bool) {
	if msg.Type != crewmessage.TypeFinding && msg.Type != crewmessage.TypeNotice {
		s.writeError(w, r, http.StatusForbidden, "first_officer_type_not_permitted",
			"a first-officer seat sends only finding and notice messages", map[string]any{"type": string(msg.Type)})
		return nil, false
	}
	if msg.RecipientRole != crewmessage.RoleCaptain {
		s.writeError(w, r, http.StatusForbidden, "first_officer_recipient_not_permitted",
			"a first-officer seat addresses its findings and notices to the captain only",
			map[string]any{"recipient_role": string(msg.RecipientRole)})
		return nil, false
	}
	anchorRun, perr := uuid.Parse(msg.Anchor.RunID)
	if msg.Anchor.RunID == "" || perr != nil {
		s.writeError(w, r, http.StatusForbidden, "first_officer_anchor_out_of_seat",
			"a first-officer seat's message must be anchored on a run inside its seat",
			map[string]any{"anchor": "run_less"})
		return nil, false
	}
	ctx := r.Context()
	inSeat, err := s.firstOfficerRunInSeat(ctx, seat, anchorRun)
	if err != nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "first_officer_seat_unverifiable",
			"could not verify that the anchor run lies inside the seat; retry shortly",
			map[string]any{"reason": "anchor_unreadable", internalCauseKey: err.Error()})
		return nil, false
	}
	if !inSeat {
		s.writeError(w, r, http.StatusForbidden, "first_officer_anchor_out_of_seat",
			"a first-officer seat's message must be anchored on a run inside its seat",
			map[string]any{"anchor_run_id": anchorRun.String()})
		return nil, false
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, anchorRun)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found", "no run with that id",
				map[string]any{"field": "anchor.run_id"})
			return nil, false
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run failed", map[string]any{"error": err.Error()})
		return nil, false
	}
	if !s.enforceAccount(w, r, memberWrite, runRow) {
		return nil, false
	}
	return runRow, true
}
