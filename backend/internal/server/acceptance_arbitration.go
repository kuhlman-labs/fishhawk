package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryAcceptanceTriageArbitrated is the audit-log category for the chained
// entry handleAcceptanceArbitration writes when an operator discharges a PAGED
// acceptance triage (E66.37 / #2474). It is a durable, operator-authored
// declaration modeled on merge_verdict_recorded / operator_commit_vouched: the
// payload names the run, the acceptance stage, the operator's reason, and — load
// bearing — the outcome_sequence of the acceptance_outcome_recorded entry it
// discharges.
//
// The sequence binding IS the invalidation mechanism: acceptanceGateState only
// honours an arbitration whose outcome_sequence EQUALS the newest recorded
// outcome, so a later acceptance re-run (a higher-sequence outcome no prior
// arbitration names) re-wedges the gate by construction rather than by a
// separate expiry rule. Open-set string — audit_entries.category has no CHECK,
// so this needs no migration; it IS registered in audit.KnownCategories so an
// operator can arm fishhawk_await_audit on their own discharge.
//
// Internal audit kind projected through the living-anchor timeline — NOT a new
// issue-comment surface, so docs/issue-comment-surfaces.md is untouched.
const CategoryAcceptanceTriageArbitrated = "acceptance_triage_arbitrated"

// acceptanceArbitrationRequest is the JSON body of
// POST /v0/runs/{run_id}/acceptance-arbitration.
//
// Reason is required non-empty: the arbitration overrides a FAILED acceptance
// verdict, so it must carry the operator's rationale. AcknowledgeFailedCriteria
// is the issue's "deliberate, separately-stated decision" — required only when
// the discharged outcome carries genuinely FAILED criteria (as opposed to a
// class-5 all-skip verdict, where nothing failed and the reason alone suffices).
type acceptanceArbitrationRequest struct {
	Reason                    string `json:"reason"`
	AcknowledgeFailedCriteria bool   `json:"acknowledge_failed_criteria"`
}

// acceptanceArbitrationResponse reports the recorded discharge. OutcomeSequence
// is the acceptance_outcome_recorded sequence the arbitration is BOUND to (an
// operator can correlate it against the audit chain); ArbitrationSequence is the
// appended entry's own sequence; AlreadyRecorded is true when a prior POST
// already discharged this same outcome, so no duplicate row was appended.
type acceptanceArbitrationResponse struct {
	RunID               string `json:"run_id"`
	AcceptanceGateState string `json:"acceptance_gate_state"`
	OutcomeSequence     int64  `json:"outcome_sequence"`
	ArbitrationSequence int64  `json:"arbitration_sequence"`
	AlreadyRecorded     bool   `json:"already_recorded"`
}

// handleAcceptanceArbitration implements
// POST /v0/runs/{run_id}/acceptance-arbitration (E66.37 / #2474).
//
// It is the missing discharge for a PAGED acceptance triage. Before this verb a
// paged run had no blessed path: acceptanceGateState mapped ANY recorded failed
// verdict to acceptance_triage, fishhawk_merge_run 409'd on it, and
// fishhawk_retry_stage's acceptance-reopen arm requires NO recorded verdict — so
// the operator the product explicitly told to "arbitrate before merging" could
// only leave the blessed path and hand-merge, losing the merge_verdict_recorded
// audit entry. This endpoint records the arbitration as a chained
// acceptance_triage_arbitrated entry, after which the gate reads
// acceptance_arbitrated and the ordinary merge verb works.
//
// Auth ladder — mirrors handleMergeRun exactly, because it admits the SAME
// merge:
//   - anonymous → 401 authentication_required;
//   - a run-bound MCP token ("mcp:run:<uuid>") → 403 run_token_forbidden, even
//     for its own run: an agent discharging the acceptance failure on its own
//     change would bypass the operator gate entirely;
//   - any identity missing write:approvals → 403 insufficient_scope, enforced
//     UNCONDITIONALLY (no cookie-session bypass).
//
// Guards, ALL fail-closed and evaluated BEFORE any write, so a refused
// arbitration leaves ZERO acceptance_triage_arbitrated rows:
//  1. 400 validation_failed on a non-UUID run_id or an empty/whitespace reason;
//  2. 503 acceptance_arbitration_unconfigured when the run/audit repositories
//     are not wired;
//  3. 404 run_not_found;
//  4. 409 acceptance_arbitration_not_applicable unless the gate currently reads
//     acceptance_triage (a read error is a 500, never a write). A passed /
//     pending / not-declared / already-arbitrated gate has nothing to discharge;
//  5. 409 acceptance_arbitration_not_applicable unless the triage disposition
//     CORRELATED with that outcome is one acceptanceDispositionPages classifies
//     as PAGED. A class-1/2 verdict that auto-routed to fixup_dispatched /
//     retry_dispatched keeps its automatic route — arbitration is not a way to
//     skip a fix-up the loop already dispatched. An ABSENT or undecodable
//     disposition is refused too (fail-closed on unknown evidence);
//  6. 409 acceptance_arbitration_requires_acknowledgement when the outcome
//     carries criteria_failed > 0 and acknowledge_failed_criteria is not true;
//  7. 409 acceptance_outcome_superseded when a concurrent acceptance re-run
//     landed a NEWER outcome between the guards and the append (binding
//     approval condition 1 — WRITE-SIDE REVALIDATION). Without it the endpoint
//     could persist an arbitration naming an outcome already superseded.
//
// Idempotence lives on the ENDPOINT and is evaluated BETWEEN guard 3 and guard
// 4: a repeated POST that finds an arbitration already bound to the newest
// outcome returns 200 already_recorded:true with no second row. It must precede
// the gate-state guard, because after the first POST the gate reads
// acceptance_arbitrated and a later-placed check could never match.
//
// Admission is keyed on whether the disposition PAGED, not on the triage class
// number (binding approval condition 6): a class-1 that auto-routed is refused,
// while a class-1 fixup_unavailable_paged — the fix-up ceiling spent, the human
// paged — is arbitrable with the explicit acknowledgement.
func (s *Server) handleAcceptanceArbitration(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	// Operator-token-only: a run-bound agent token may NEVER arbitrate its own
	// acceptance failure — the arbitration admits a merge, so an agent reaching
	// it would bypass the operator gate outright. Mirrors handleMergeRun /
	// handleVouchCommit.
	if _, runBound := runBoundTokenRunID(id); runBound {
		s.writeError(w, r, http.StatusForbidden, "run_token_forbidden",
			"a run-bound agent token may not arbitrate an acceptance triage; discharging a failed acceptance verdict is an operator action",
			nil)
		return
	}
	// write:approvals enforced UNCONDITIONALLY (no cookie-session bypass): the
	// arbitration is an approval-class override of a failed verdict that makes
	// the run merge-eligible.
	if !hasScope(id, "write:approvals") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:approvals",
			map[string]any{"required_scope": "write:approvals"})
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	var reqBody acceptanceArbitrationRequest
	if r.Body != nil {
		if decErr := json.NewDecoder(r.Body).Decode(&reqBody); decErr != nil && !errors.Is(decErr, io.EOF) {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"request body must be valid JSON {reason, acknowledge_failed_criteria}",
				map[string]any{"error": decErr.Error()})
			return
		}
	}
	reason := strings.TrimSpace(reqBody.Reason)
	if reason == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"reason is required: the arbitration is an audited operator declaration overriding a failed acceptance verdict; state why the change is acceptable",
			map[string]any{"field": "reason"})
		return
	}

	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "acceptance_arbitration_unconfigured",
			"acceptance arbitration requires run + audit repositories", nil)
		return
	}

	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run failed", map[string]any{"error": err.Error()})
		return
	}

	stages, err := s.cfg.RunRepo.ListStagesForRun(r.Context(), runID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list stages failed", map[string]any{"error": err.Error()})
		return
	}

	// The outcome the guards evaluate and the arbitration will BIND to.
	outcome, err := s.latestAcceptanceOutcome(r.Context(), runID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"read acceptance outcome failed", map[string]any{"error": err.Error()})
		return
	}

	// Idempotence on the ENDPOINT: a repeated POST that finds an arbitration
	// already bound to THIS outcome appends no duplicate row and reports the
	// existing sequence. It runs BEFORE the gate-state guard on purpose — once
	// the first POST lands, the gate reads acceptance_arbitrated, so a check
	// placed after that guard could never match and a timed-out-then-retried
	// operator call would get a confusing 409 instead of the idempotent 200 the
	// merge verb's contract already taught them to expect. Skipped entirely when
	// no outcome is recorded, so a zero-valued sequence can never alias a real
	// arbitration's binding.
	if outcome.Recorded {
		existing, lerr := s.cfg.AuditRepo.ListForRunByCategory(r.Context(), runID, CategoryAcceptanceTriageArbitrated)
		if lerr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"read prior acceptance arbitration failed", map[string]any{"error": lerr.Error()})
			return
		}
		for _, e := range existing {
			if seq, ok := arbitrationOutcomeSequence(e.Payload); ok && seq == outcome.Sequence {
				s.writeJSON(w, r, http.StatusOK, acceptanceArbitrationResponse{
					RunID:               runID.String(),
					AcceptanceGateState: acceptanceGateArbitrated,
					OutcomeSequence:     outcome.Sequence,
					ArbitrationSequence: e.Sequence,
					AlreadyRecorded:     true,
				})
				return
			}
		}
	}

	// Guard 4: only a run PARKED at acceptance_triage has something to discharge.
	// A read error is a 500 and never a write — fail closed on unknown evidence.
	gateState, gerr := s.acceptanceGateState(r.Context(), runRow, stages)
	if gerr != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"read acceptance gate state failed", map[string]any{"error": gerr.Error()})
		return
	}
	if gateState != acceptanceGateTriage {
		s.writeError(w, r, http.StatusConflict, "acceptance_arbitration_not_applicable",
			"the acceptance gate is not parked at acceptance_triage; there is no paged triage to arbitrate",
			map[string]any{"run_id": runID.String(), "acceptance_gate_state": gateState})
		return
	}

	// Guard 5: the triage disposition CORRELATED with this outcome must be a
	// paged one. Correlation is by audit sequence — the backend writes
	// acceptance_triage_decided AFTER the acceptance_outcome_recorded entry it
	// triages (handleShipAcceptance: the outcome append precedes
	// triageAcceptanceFailure), the same relation the MCP surface's
	// latestAcceptanceTriageDisposition relies on.
	class, disposition, err := s.acceptanceTriageForOutcome(r.Context(), runID, outcome.Sequence)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"read acceptance triage disposition failed", map[string]any{"error": err.Error()})
		return
	}
	if !acceptanceDispositionPages(disposition) {
		s.writeError(w, r, http.StatusConflict, "acceptance_arbitration_not_applicable",
			"the acceptance triage disposition correlated with this verdict did not page a human; an auto-routed disposition keeps its automatic route and an absent/unreadable one is refused",
			map[string]any{
				"run_id":             runID.String(),
				"triage_disposition": disposition,
				"outcome_sequence":   outcome.Sequence,
			})
		return
	}

	// Guard 6: a verdict carrying genuinely FAILED criteria needs the operator's
	// deliberate, separately-stated acknowledgement — the reason alone is not it.
	// A class-5 all-skip verdict (criteria_failed == 0) is arbitrable on the
	// reason alone: nothing failed, the criteria were unvalidatable.
	if outcome.CriteriaFailed > 0 && !reqBody.AcknowledgeFailedCriteria {
		s.writeError(w, r, http.StatusConflict, "acceptance_arbitration_requires_acknowledgement",
			"this acceptance verdict carries failed criteria; set acknowledge_failed_criteria:true to state deliberately that you are merging despite them",
			map[string]any{
				"run_id":           runID.String(),
				"criteria_failed":  outcome.CriteriaFailed,
				"outcome_sequence": outcome.Sequence,
			})
		return
	}

	// Guard 7 — WRITE-SIDE REVALIDATION (binding approval condition 1). Every
	// guard above read the chain at some earlier instant; a concurrent acceptance
	// re-run (or the delegated auto-driver's own path) can land a NEWER
	// acceptance_outcome_recorded entry in between. Persisting an arbitration
	// that names a superseded outcome would be a durable fail-OPEN artifact: it
	// records an operator discharge of a verdict nobody evaluated. Re-read the
	// latest outcome IMMEDIATELY before appending and refuse 409 naming both
	// sequences if it moved.
	current, err := s.latestAcceptanceOutcome(r.Context(), runID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"re-read acceptance outcome before recording failed", map[string]any{"error": err.Error()})
		return
	}
	if !current.Recorded || current.Sequence != outcome.Sequence {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo,
			"acceptance arbitration: outcome superseded between guard evaluation and append; refusing",
			slog.String("run_id", runID.String()),
			slog.Int64("evaluated_outcome_sequence", outcome.Sequence),
			slog.Int64("current_outcome_sequence", current.Sequence))
		s.writeError(w, r, http.StatusConflict, "acceptance_outcome_superseded",
			"a newer acceptance outcome was recorded while this arbitration was being evaluated; re-read the acceptance verdict and arbitrate the current one",
			map[string]any{
				"run_id":                     runID.String(),
				"evaluated_outcome_sequence": outcome.Sequence,
				"current_outcome_sequence":   current.Sequence,
			})
		return
	}

	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := audit.ActorUser
	payloadMap := map[string]any{
		"run_id":                       runID.String(),
		"reason":                       reason,
		"outcome_sequence":             outcome.Sequence,
		"verdict":                      outcome.Verdict,
		"criteria_failed":              outcome.CriteriaFailed,
		"criteria_skipped":             outcome.CriteriaSkipped,
		"acknowledged_failed_criteria": reqBody.AcknowledgeFailedCriteria,
		"triage_class":                 class,
		"triage_disposition":           disposition,
		"delegated":                    false,
	}
	if outcome.StageID != nil {
		payloadMap["stage_id"] = outcome.StageID.String()
	}
	payload, _ := json.Marshal(payloadMap)
	entry, aerr := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        runID,
		StageID:      outcome.StageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryAcceptanceTriageArbitrated,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	})
	if aerr != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"acceptance arbitration: append acceptance_triage_arbitrated audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("error", aerr.Error()))
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"record acceptance arbitration failed", map[string]any{"error": aerr.Error()})
		return
	}

	// Best-effort tail, never unwinding the recorded arbitration. Both only
	// WARN-log: the durable declaration is already on the chain, and merge_run
	// reads acceptanceGateState directly rather than derived_status, so a failure
	// here costs at most a stale presentation until the next reconciler tick.
	s.notifyStatusUpdate(r.Context(), runID, "acceptance_arbitrated")
	prURL := ""
	if runRow.PullRequestURL != nil {
		prURL = *runRow.PullRequestURL
	}
	s.refreshDriveAfterArbitration(r.Context(), runID, stages, prURL)

	s.writeJSON(w, r, http.StatusOK, acceptanceArbitrationResponse{
		RunID:               runID.String(),
		AcceptanceGateState: acceptanceGateArbitrated,
		OutcomeSequence:     outcome.Sequence,
		ArbitrationSequence: entry.Sequence,
		AlreadyRecorded:     false,
	})
}

// acceptanceTriageForOutcome returns the class + disposition of the
// acceptance_triage_decided entry CORRELATED with the acceptance outcome at
// outcomeSequence — the newest such entry whose Sequence is strictly GREATER
// than the outcome's, because the backend writes the triage decision AFTER the
// outcome it triages.
//
// Returns ("", "", nil) when no correlated entry exists or its payload cannot be
// decoded: the caller refuses on a non-paging disposition, so an absent or
// unreadable triage decision fails CLOSED (an auto-routed class-1/2 verdict
// stays un-arbitrable, and so does a verdict whose triage has not landed yet).
// The read error is propagated.
func (s *Server) acceptanceTriageForOutcome(ctx context.Context, runID uuid.UUID, outcomeSequence int64) (class, disposition string, err error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryAcceptanceTriageDecided)
	if err != nil {
		return "", "", err
	}
	var latest *audit.Entry
	for _, e := range entries {
		if e.Sequence <= outcomeSequence {
			continue
		}
		if latest == nil || e.Sequence > latest.Sequence {
			latest = e
		}
	}
	if latest == nil {
		return "", "", nil
	}
	var p struct {
		Class       string `json:"class"`
		Disposition string `json:"disposition"`
	}
	if uerr := json.Unmarshal(latest.Payload, &p); uerr != nil {
		return "", "", nil
	}
	return p.Class, p.Disposition, nil
}

// refreshDriveAfterArbitration re-runs the drive observer over the run's review
// stage so derived_status flips from acceptance_triage to awaiting_merge
// immediately rather than on the next merge-reconciler tick. Purely a
// convenience: it never unwinds the recorded arbitration, and merge_run is
// unaffected because it reads acceptanceGateState directly.
func (s *Server) refreshDriveAfterArbitration(ctx context.Context, runID uuid.UUID, stages []*run.Stage, prURL string) {
	if s.drive == nil {
		return
	}
	for _, st := range stages {
		if st.Type != run.StageTypeReview {
			continue
		}
		s.ObserveParkedReviewForDrive(ctx, st, prURL)
		return
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelDebug,
		"acceptance arbitration: no review stage to refresh for drive presentation",
		slog.String("run_id", runID.String()))
}
