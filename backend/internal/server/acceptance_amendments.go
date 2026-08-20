package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Acceptance-criteria amendment actions (#2581). retire moves a criterion out
// of the live set (it is no longer validated, and a failure reported against it
// can be neutralized at ingest under the four downgrade preconditions); restate
// replaces a live criterion's statement and leaves it LIVE — restatement is
// deliberately NOT a silencing channel, so a restated criterion still fails
// acceptance if it genuinely fails.
const (
	acceptanceAmendActionRetire  = "retire"
	acceptanceAmendActionRestate = "restate"
)

// acceptanceRetirementSourceOperator tags a retirement recorded through the
// operator's explicit approve-time channel. There is deliberately exactly ONE
// retirement source: automatic DERIVED retirement (retiring a criterion whose
// subject is a file dropped by remove_scope_files) was considered and REJECTED
// (#2581) because whether a repo path is a criterion's sole subject is a
// natural-language judgement no token rule decides, and a rule that silently
// retires a live criterion converts a loud acceptance failure into silence. The
// source tag exists so provenance is explicit on the chain, not because a second
// source is planned.
const acceptanceRetirementSourceOperator = "operator_approval"

// maxAcceptanceAmendmentTextBytes caps each amendment's reason/statement so an
// oversized field cannot bloat the approval_submitted audit payload. Sized off
// the same family as the operator's approve-with-conditions text; oversized
// input is TRUNCATED (prompt.CapText), never refused — the operator's decision
// still lands, it just cannot carry an unbounded blob onto the hash chain.
const maxAcceptanceAmendmentTextBytes = prompt.MaxApprovalConditionBytes

// acceptanceCriteriaAmendment is the wire + audit shape of one operator
// amendment to the approved plan's acceptance criteria, supplied on a PLAN-stage
// approve (#2581). It is recorded on the SAME approval_submitted row as the
// reason / add_scope_files / remove_scope_files that motivated it, so the
// retirement and its justification are reconstructable from the chain alone.
type acceptanceCriteriaAmendment struct {
	// ID is the plan criterion this amendment targets (the same slug join key
	// the acceptance verdict reports against).
	ID string `json:"id"`
	// Action is retire | restate.
	Action string `json:"action"`
	// Reason is REQUIRED per criterion: it is the reconstructable why.
	Reason string `json:"reason"`
	// Statement is REQUIRED for restate and ignored for retire.
	Statement string `json:"statement,omitempty"`
}

// retiredCriterion is one criterion the operator retired, with the provenance
// needed to reconstruct the decision from the audit chain alone.
type retiredCriterion struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
	Source string `json:"source"`
	// ApprovalSequence is the audit Sequence of the approval_submitted entry
	// that recorded the retirement; 0 for an in-flight (not yet recorded)
	// amendment supplied as the resolver's `pending` argument.
	ApprovalSequence int64 `json:"approval_sequence"`
}

// effectiveAcceptanceCriteria is the COMPLETE effective criteria set for a run:
// the live criteria (with restatements applied, in plan order), the retired set
// with provenance (also in plan order), the ids whose statement a restatement
// replaced (plan order), and AllIDs — ALWAYS the full plan id list, never
// narrowed, because the acceptance_criteria_ids served to the runner must stay a
// SUPERSET so a verdict reporting a retired id can never fail the stage closed on
// join-key validation.
//
// Restated exists because Live alone cannot tell a consumer whether an amendment
// applied: a restate-only history leaves Live the same LENGTH as the plan set
// with only a statement differing, so a consumer keying "was anything amended?"
// off len(Retired) silently drops the restatement (the round-four defect — the
// acceptance prompt fell back to the plan's original statements).
type effectiveAcceptanceCriteria struct {
	Live     []plan.AcceptanceCriterion
	Retired  []retiredCriterion
	Restated []string
	AllIDs   []string
}

// amended reports whether any recorded or pending amendment actually changed the
// plan's criteria set — a retirement or a restatement. It is the single predicate
// consumers use to decide between the effective set and the plan set verbatim.
func (e effectiveAcceptanceCriteria) amended() bool {
	return len(e.Retired) > 0 || len(e.Restated) > 0
}

// retiredIDSet returns the retired criterion ids as a set for membership tests.
func (e effectiveAcceptanceCriteria) retiredIDSet() map[string]struct{} {
	out := make(map[string]struct{}, len(e.Retired))
	for _, r := range e.Retired {
		out[r.ID] = struct{}{}
	}
	return out
}

// resolveEffectiveAcceptanceCriteria is the SINGLE seam that computes a run's
// effective acceptance-criteria set (#2581). No caller may union, filter, or
// recompute any part of the result: if a consumer needs a different projection,
// the signature changes HERE. It consumes the approved plan, every prior
// source-tagged retirement recorded on the run's approval chain, and the
// caller's in-flight `pending` amendments (nil for every consumer but the
// approve gate), and returns the complete set.
//
// Call sites — the full, actual list (operator binding condition 2):
//  1. checkAmendAcceptanceCriteria (the approve gate) — VALIDATION-ONLY. It
//     resolves the PRIOR effective set to evaluate the anti-silencing refusals
//     (unknown id, already-retired id, the all-retired union) and records
//     nothing derived from the result.
//  2. handleGetPrompt's acceptance branch (the dispatch prompt path).
//  3. handleRenderPrompt's acceptance branch (the render/preview prompt path).
//  4. handleShipAcceptance (acceptance-verdict ingest), which uses the recorded
//     retired-id set as the strict key for the downgrade preconditions.
//
// Ordering: prior recorded amendments are applied in ASCENDING audit Sequence,
// then the caller's pending amendments. Retire is ABSORBING (a repeat retire of
// an already-retired id no-ops here; it is REFUSED at the gate). Restate
// replaces only the Statement of a LIVE criterion and never moves it out of
// Live.
//
// An audit read failure returns a typed error and NO partial set, so each caller
// chooses its own fail direction explicitly (the gate fails closed; the prompt
// builder renders the FULL plan criteria set; ingest performs no downgrade —
// every one of those directions is toward MORE validation, never toward
// silence).
func (s *Server) resolveEffectiveAcceptanceCriteria(ctx context.Context, runID uuid.UUID, p *plan.Plan, pending []acceptanceCriteriaAmendment) (effectiveAcceptanceCriteria, error) {
	var eff effectiveAcceptanceCriteria
	if p == nil || len(p.Verification.AcceptanceCriteria) == 0 {
		return eff, nil
	}
	// Seed the live set from the plan in plan order; AllIDs carries every plan
	// criterion id and is never narrowed by an amendment.
	eff.Live = append(eff.Live, p.Verification.AcceptanceCriteria...)
	planOrder := make(map[string]int, len(eff.Live))
	for i, c := range eff.Live {
		eff.AllIDs = append(eff.AllIDs, c.ID)
		planOrder[c.ID] = i
	}

	recorded, err := s.recordedAcceptanceAmendments(ctx, runID)
	if err != nil {
		return effectiveAcceptanceCriteria{}, err
	}

	retired := map[string]retiredCriterion{}
	restated := map[string]struct{}{}
	apply := func(a acceptanceCriteriaAmendment, seq int64) {
		if _, already := retired[a.ID]; already {
			// Retire is absorbing and a restate of a retired criterion is a
			// no-op here; both are refused at the gate.
			return
		}
		idx := -1
		for i, c := range eff.Live {
			if c.ID == a.ID {
				idx = i
				break
			}
		}
		if idx < 0 {
			return
		}
		switch a.Action {
		case acceptanceAmendActionRetire:
			retired[a.ID] = retiredCriterion{
				ID:               a.ID,
				Reason:           a.Reason,
				Source:           acceptanceRetirementSourceOperator,
				ApprovalSequence: seq,
			}
			eff.Live = append(eff.Live[:idx], eff.Live[idx+1:]...)
		case acceptanceAmendActionRestate:
			eff.Live[idx].Statement = a.Statement
			restated[a.ID] = struct{}{}
		}
	}
	for _, rec := range recorded {
		for _, a := range rec.amendments {
			apply(a, rec.sequence)
		}
	}
	for _, a := range pending {
		apply(a, 0)
	}

	for _, r := range retired {
		eff.Retired = append(eff.Retired, r)
	}
	sort.Slice(eff.Retired, func(i, j int) bool {
		return planOrder[eff.Retired[i].ID] < planOrder[eff.Retired[j].ID]
	})
	for id := range restated {
		eff.Restated = append(eff.Restated, id)
	}
	sort.Slice(eff.Restated, func(i, j int) bool {
		return planOrder[eff.Restated[i]] < planOrder[eff.Restated[j]]
	})
	return eff, nil
}

// recordedAmendmentEntry is one approval_submitted row's recorded amendments,
// carried with the row's audit Sequence so a retirement's provenance names the
// approval that recorded it.
type recordedAmendmentEntry struct {
	sequence   int64
	amendments []acceptanceCriteriaAmendment
}

// recordedAcceptanceAmendments reads the run's approval_submitted entries and
// returns every approve-decision, PLAN-stage row's amend_acceptance_criteria in
// ASCENDING audit Sequence. A reject-decision row and a row recorded off the
// plan stage are ignored (the same filter loadApprovalAddScopeFiles applies via
// approvalEntryStageIsPlan, #2598). An undecodable payload is skipped; the audit
// read error is PROPAGATED so the seam returns no partial set.
func (s *Server) recordedAcceptanceAmendments(ctx context.Context, runID uuid.UUID) ([]recordedAmendmentEntry, error) {
	if s.cfg.AuditRepo == nil {
		return nil, nil
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "approval_submitted")
	if err != nil {
		return nil, fmt.Errorf("list approval_submitted for acceptance amendments: %w", err)
	}
	sorted := make([]*audit.Entry, 0, len(entries))
	sorted = append(sorted, entries...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Sequence < sorted[j].Sequence })

	var out []recordedAmendmentEntry
	for _, e := range sorted {
		var payload struct {
			Decision   string                        `json:"decision"`
			Amendments []acceptanceCriteriaAmendment `json:"amend_acceptance_criteria"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			continue
		}
		if payload.Decision != "approve" || len(payload.Amendments) == 0 {
			continue
		}
		if !s.approvalEntryStageIsPlan(ctx, e) {
			continue
		}
		out = append(out, recordedAmendmentEntry{sequence: e.Sequence, amendments: payload.Amendments})
	}
	return out, nil
}

// checkAmendAcceptanceCriteria validates a plan-stage approve's
// amend_acceptance_criteria channel PRE-Submit — before any approval row is
// inserted — mirroring the #983/#2415 gate placement so a refusal leaves the
// operator's submission slot free for a corrected retry. It returns the
// canonical (trimmed, text-capped) amendment slice writeApprovalAudit records.
//
// It is a no-op returning (nil, true) when the caller supplied no amendments, so
// an approve that does not use the channel is untouched and records a
// byte-identical payload to today.
//
// Nine named refusals, each carrying details.rule so it is separately
// assertable:
//
//	R1 unknown_criterion_id            400 — id absent from the approved plan
//	R2 reason_required                 400 — blank/whitespace reason
//	R3 statement_required              400 — restate with no statement
//	R4 duplicate_id                    400 — the same id twice in one request
//	R5 amendment_not_approve_plan_stage 400 — reject, or a non-plan stage
//	R6 (all-retired, single call)      422 acceptance_criteria_all_retired
//	R7 (all-retired, cumulative)       422 acceptance_criteria_all_retired
//	R8 already_retired                 400 — id retired by a PRIOR approval
//	R9 (plan unavailable / no criteria) 422 acceptance_criteria_unavailable
//
// R6 and R7 are ONE control evaluated on the deduplicated union of prior and
// in-flight retirements — the anti-silencing gate: the channel cannot be used to
// empty a plan's acceptance contract, whether in one call or cumulatively across
// approvals. An unknown action value is refused under unknown_action (400).
func (s *Server) checkAmendAcceptanceCriteria(w http.ResponseWriter, r *http.Request, stage *run.Stage, decision approval.Decision, amendments []acceptanceCriteriaAmendment) ([]acceptanceCriteriaAmendment, bool) {
	if len(amendments) == 0 {
		return nil, true
	}
	refuse := func(rule, msg string, extra map[string]any) {
		details := map[string]any{"field": "amend_acceptance_criteria", "rule": rule}
		for k, v := range extra {
			details[k] = v
		}
		s.writeError(w, r, http.StatusBadRequest, "validation_failed", msg, details)
	}

	// R5: an amendment supplied with a non-approve decision or on a non-plan
	// stage is REFUSED, never silently dropped — a dropped amendment is a silent
	// divergence between what the operator believes they retired and what the
	// chain records.
	if decision != approval.DecisionApprove || stage.Type != run.StageTypePlan {
		refuse("amendment_not_approve_plan_stage",
			"amend_acceptance_criteria is accepted only on an approve decision for a plan stage",
			map[string]any{"decision": string(decision), "stage_type": string(stage.Type)})
		return nil, false
	}

	// R9: fail closed on a plan that cannot be loaded or carries no acceptance
	// criteria — never record an amendment that anchors to nothing.
	approvedPlan, err := s.loadApprovedPlanForRun(r.Context(), stage.RunID)
	if err != nil || approvedPlan == nil || len(approvedPlan.Verification.AcceptanceCriteria) == 0 {
		details := map[string]any{"stage_id": stage.ID.String()}
		if err != nil {
			details["error"] = err.Error()
		}
		s.writeError(w, r, http.StatusUnprocessableEntity, "acceptance_criteria_unavailable",
			"the approved plan carries no acceptance_criteria to amend; re-plan so the criteria exist, or approve without amend_acceptance_criteria",
			details)
		return nil, false
	}

	// The PRIOR effective set: what the chain already records. Fail CLOSED on an
	// unreadable chain — the anti-silencing refusals cannot be evaluated without
	// it, and recording an unverified retirement is the failure this gate exists
	// to prevent.
	prior, err := s.resolveEffectiveAcceptanceCriteria(r.Context(), stage.RunID, approvedPlan, nil)
	if err != nil {
		s.writeError(w, r, http.StatusUnprocessableEntity, "acceptance_criteria_unavailable",
			"the run's prior acceptance-criteria amendments could not be read, so the amendment cannot be validated",
			map[string]any{"stage_id": stage.ID.String(), "error": err.Error()})
		return nil, false
	}
	priorRetired := prior.retiredIDSet()

	known := make(map[string]struct{}, len(approvedPlan.Verification.AcceptanceCriteria))
	for _, c := range approvedPlan.Verification.AcceptanceCriteria {
		known[c.ID] = struct{}{}
	}

	seen := make(map[string]struct{}, len(amendments))
	// union seeds with every prior retirement so R7 (the cumulative all-retired
	// refusal) reads the same union R6 does.
	union := make(map[string]struct{}, len(priorRetired)+len(amendments))
	for id := range priorRetired {
		union[id] = struct{}{}
	}
	out := make([]acceptanceCriteriaAmendment, 0, len(amendments))
	for _, a := range amendments {
		id := strings.TrimSpace(a.ID)
		action := strings.TrimSpace(a.Action)
		reason := strings.TrimSpace(a.Reason)
		statement := strings.TrimSpace(a.Statement)

		if _, ok := known[id]; !ok {
			refuse("unknown_criterion_id",
				"amend_acceptance_criteria names a criterion id that is not in the approved plan's acceptance_criteria",
				map[string]any{"id": id})
			return nil, false
		}
		switch action {
		case acceptanceAmendActionRetire, acceptanceAmendActionRestate:
		default:
			refuse("unknown_action",
				"amend_acceptance_criteria action must be retire or restate",
				map[string]any{"id": id, "action": action})
			return nil, false
		}
		if reason == "" {
			refuse("reason_required",
				"each amend_acceptance_criteria entry requires a reason — it is the reconstructable why",
				map[string]any{"id": id})
			return nil, false
		}
		if action == acceptanceAmendActionRestate && statement == "" {
			refuse("statement_required",
				"an amend_acceptance_criteria restate requires the replacement statement",
				map[string]any{"id": id})
			return nil, false
		}
		if _, dup := seen[id]; dup {
			refuse("duplicate_id",
				"amend_acceptance_criteria names the same criterion id twice in one request",
				map[string]any{"id": id})
			return nil, false
		}
		if _, already := priorRetired[id]; already {
			refuse("already_retired",
				"amend_acceptance_criteria names a criterion a prior approval already retired; a retirement cannot be re-reasoned or undone",
				map[string]any{"id": id})
			return nil, false
		}
		seen[id] = struct{}{}
		if action == acceptanceAmendActionRetire {
			union[id] = struct{}{}
		}
		reason, _ = prompt.CapText(reason, maxAcceptanceAmendmentTextBytes)
		statement, _ = prompt.CapText(statement, maxAcceptanceAmendmentTextBytes)
		canonical := acceptanceCriteriaAmendment{ID: id, Action: action, Reason: reason}
		if action == acceptanceAmendActionRestate {
			canonical.Statement = statement
		}
		out = append(out, canonical)
	}

	// R6/R7: the anti-silencing gate. Evaluated on the deduplicated union of the
	// prior recorded retirements and this request's, so the channel cannot empty
	// a plan's acceptance contract in one call OR cumulatively.
	if len(union) >= len(known) {
		s.writeError(w, r, http.StatusUnprocessableEntity, "acceptance_criteria_all_retired",
			"this amendment would retire every acceptance criterion in the approved plan, leaving nothing to validate; re-plan instead of emptying the contract",
			map[string]any{
				"stage_id":       stage.ID.String(),
				"retired_count":  len(union),
				"criteria_count": len(known),
			})
		return nil, false
	}
	return out, true
}

// acceptanceDowngradeBasisRetiredOnly is the only downgrade basis recorded
// today: every failed criterion result named a criterion the operator retired at
// the approval gate.
const acceptanceDowngradeBasisRetiredOnly = "retired_criteria_only"

// acceptanceDowngrade decides whether a FAILED acceptance verdict is neutralized
// to passed because every failure it reports names a criterion the operator
// retired at the approval gate (#2581). It is pure — the caller supplies the
// recorded effective set — and CONJUNCTIVE: all four preconditions must hold.
//
//	D1 verdict==failed AND failure_mode==assertion_fail. An `error` failure mode
//	   (a crash / 500 / exception) is NEVER downgraded: a crashing target is not
//	   a superseded expectation.
//	D2 EVERY failed criterion result's id is a member of the RECORDED retired-id
//	   set. Strict keying on the set the chain records — never a prompt-text or
//	   heuristic match.
//	D3 the surviving partition is non-empty: at least one reported criterion
//	   result whose id is NOT retired. A verdict that reports only retired
//	   criteria evidences nothing about the live contract.
//	D4 no surviving blocking UN-EVALUATED criterion: no non-retired criterion
//	   REPORTED `skipped` OR `undecidable` whose plan `blocking` value (nil
//	   defaults to true) is true. A blocking criterion the validator reported as
//	   un-exercised must not ride out on a downgrade. #2512 adds `undecidable`
//	   with IDENTICAL semantics: without it, a retirement plus a surviving
//	   blocking criterion the validator could not decide would silently produce
//	   `passed` — a green light over unevaluated evidence, the exact hazard #2512
//	   exists to remove. D4 keys on what the verdict REPORTS: a blocking live
//	   criterion the verdict OMITS entirely does not block the downgrade — the
//	   same trust model that already lets a validator omit criteria from a passed
//	   verdict (pinned by TestDowngrade_OmittedBlockingLiveCriterion_StillDowngrades).
//
// Returns the retired ids that were reported failed (plan order, via the
// effective set's ordering) and the downgrade basis.
func acceptanceDowngrade(acc acceptanceBody, eff effectiveAcceptanceCriteria) (bool, []string, string) {
	// D1.
	if acc.Verdict != acceptanceVerdictFailed || acc.FailureMode != acceptanceFailureAssertionFail {
		return false, nil, ""
	}
	retired := eff.retiredIDSet()
	if len(retired) == 0 {
		return false, nil, ""
	}
	blocking := make(map[string]bool, len(eff.Live))
	for _, c := range eff.Live {
		blocking[c.ID] = c.Blocking == nil || *c.Blocking
	}

	failedRetired := map[string]struct{}{}
	surviving := 0
	for _, res := range acc.normalizedCriteria {
		_, isRetired := retired[res.ID]
		if !isRetired {
			surviving++
		}
		switch res.Result {
		case acceptanceResultFailed:
			// D2: a failure against a criterion the operator did NOT retire
			// still fails acceptance.
			if !isRetired {
				return false, nil, ""
			}
			failedRetired[res.ID] = struct{}{}
		case acceptanceResultSkipped, acceptanceResultUndecidable:
			// D4: a surviving BLOCKING criterion the validator did not evaluate —
			// reported `skipped` or (#2512) `undecidable` — blocks the downgrade.
			// An id absent from the live set is treated as blocking (fail closed).
			if isRetired {
				continue
			}
			if b, ok := blocking[res.ID]; !ok || b {
				return false, nil, ""
			}
		}
	}
	// D3.
	if surviving == 0 {
		return false, nil, ""
	}
	if len(failedRetired) == 0 {
		return false, nil, ""
	}
	ids := make([]string, 0, len(failedRetired))
	for _, r := range eff.Retired {
		if _, ok := failedRetired[r.ID]; ok {
			ids = append(ids, r.ID)
		}
	}
	return true, ids, acceptanceDowngradeBasisRetiredOnly
}

// recordedAcceptanceEffectiveVerdict reads the acceptance_outcome_recorded entry
// paired with artifactID and reports the EFFECTIVE verdict the chain already
// records for it: the recorded `verdict` when that entry carries a downgrade
// (`verdict_reported` present), and "" when it does not. The second return
// distinguishes "read a recorded entry" from "no readable entry", so the caller
// can fall back to its freshly computed value rather than silently reporting an
// un-downgraded verdict.
//
// It exists for the idempotent replay branch of handleShipAcceptance only: the
// downgrade decision is recomputed from the CURRENT approval chain, so a
// retirement recorded after the original ship would otherwise make a replay
// answer differently from the governance entry the merge gate reads.
func (s *Server) recordedAcceptanceEffectiveVerdict(ctx context.Context, runID uuid.UUID, artifactID string) (string, bool) {
	if s.cfg.AuditRepo == nil {
		return "", false
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryAcceptanceOutcomeRecorded)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		var p struct {
			ArtifactID      string `json:"artifact_id"`
			Verdict         string `json:"verdict"`
			VerdictReported string `json:"verdict_reported"`
		}
		if json.Unmarshal(e.Payload, &p) != nil || p.ArtifactID != artifactID {
			continue
		}
		if p.VerdictReported == "" {
			// The recorded entry carries no downgrade.
			return "", true
		}
		return p.Verdict, true
	}
	return "", false
}

// resolveAcceptancePromptCriteria resolves the effective criteria set for an
// acceptance-stage prompt build, converting the retired set into the prompt
// package's render shape. It FAILS OPEN: on any resolve error it WARN-logs and
// returns (nil, nil), which makes buildAcceptance render the FULL plan criteria
// set — a degraded read can never silence a criterion.
//
// The live set is returned whenever ANY amendment applied — a retirement or a
// RESTATEMENT. Keying this off the retired set alone dropped a restate-only
// history on the floor: the prompt fell back to the plan's original statements
// and the validator judged the change against the very text the operator
// replaced at the gate. Both prompt paths call this one function, so they cannot
// diverge on it.
func (s *Server) resolveAcceptancePromptCriteria(ctx context.Context, runID uuid.UUID, p *plan.Plan) ([]plan.AcceptanceCriterion, []prompt.RetiredAcceptanceCriterion) {
	if p == nil {
		return nil, nil
	}
	eff, err := s.resolveEffectiveAcceptanceCriteria(ctx, runID, p, nil)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"prompt: acceptance-criteria amendment resolution failed; rendering the full plan criteria set",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil, nil
	}
	if !eff.amended() {
		// Nothing was amended: leave both trigger fields nil so the prompt is
		// byte-identical to today.
		return nil, nil
	}
	if len(eff.Retired) == 0 {
		// Restate-only: the live set carries the replacement statements, and the
		// retired block must not render (there is nothing retired to name).
		return eff.Live, nil
	}
	retired := make([]prompt.RetiredAcceptanceCriterion, 0, len(eff.Retired))
	for _, r := range eff.Retired {
		retired = append(retired, prompt.RetiredAcceptanceCriterion{ID: r.ID, Reason: r.Reason})
	}
	return eff.Live, retired
}
