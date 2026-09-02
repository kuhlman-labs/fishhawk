package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// CategoryCampaignGatePaged is the audit-log category the campaign
// auto-driver writes when it REFUSES a gate because a must_page_human
// condition is active (E25.6 / ADR-047). The actor takes NO action and
// emits this run-chained hand-off entry so the E25.7 pause/page consumer
// can route the gate to a human. It is a documented issue-comment /
// hand-off surface (docs/issue-comment-surfaces.md) — the actor's only
// output on a must_page_human gate.
const CategoryCampaignGatePaged = "campaign_gate_paged"

// requirementConcernCategory is the open-concern category the auto-driver
// reads as a requirement_arbitration must_page_human signal: a
// reviewer-flagged requirement-level disagreement is a human judgment,
// not an agent-routable fix-up, so the actor refuses to auto-route it and
// pages. v0 grounding for the requirement_arbitration page event, whose
// richer detection lands with E25.7's pause/page state machine.
const requirementConcernCategory = "requirement"

// GitHubMerger is the merge seam the campaign auto-driver dispatches a
// delegated may_merge gate through (E25.6 / ADR-047). The concrete — a
// GitHub App-installation merge client — is constructed and bound in
// serve.go (E25.6 slice 3); the actor receives it as a parameter so a
// nil merger fails the merge gate CLOSED to observe-only rather than
// requiring the merger on the Server struct. MergePullRequest merges the
// run's pull request on GitHub; the existing webhook /
// resolveReviewStageOnMerge path then settles the review stage.
type GitHubMerger interface {
	MergePullRequest(ctx context.Context, runRow *run.Run) error
}

// AutoDriveOutcome reports what AutoDriveRunGate did at a run gate so the
// campaign driver (E25.6 slice 3) can record the campaign-level marker.
// A non-observe-only outcome is exactly one of three kinds: Acted (a
// delegated gate action was dispatched), Paged (a must_page_human
// condition was refused), or DecisionRequired (a well-defined but
// non-auto-routable gate state the driver must STOP on and hand to the
// operator). Observe-only is all four booleans false.
type AutoDriveOutcome struct {
	// Acted is true when the actor dispatched a delegated gate action;
	// Action then names the delegation verb taken (delegation.Action*).
	Acted  bool
	Action string
	// Paged is true when the actor refused a must_page_human condition;
	// PageEvent names the page event and a campaign_gate_paged entry was
	// emitted. No gate action was taken.
	Paged     bool
	PageEvent string
	// DecisionRequired is true when the actor could not auto-act on a
	// well-defined, EXPECTED gate state and must hand the gate to the
	// operator — distinct from observe-only (which means keep polling): the
	// driver must STOP, not spin. DecisionState names the state (e.g.
	// fixup_budget_exhausted / fixup_ceiling_reached; the #2381 delegated-approval
	// states operatorrole.DecisionStateHumanQuorumRequired and
	// DecisionStateDelegatedApprovalNoProgress) and Action names the
	// delegated verb whose arm surfaced it. Set with Acted=false and
	// Paged=false. Introduced by the E48.41 / #2091 fix so an exhausted
	// fix-up budget on the delegated route_fixup arm parks the operator with
	// real next moves instead of bubbling to an HTTP 500; extended by #2381 so the
	// may_approve arm parks the operator when a delegated approve could never
	// advance a human-quorum gate (human_quorum_required) or made no progress as a
	// duplicate (delegated_approval_no_progress) instead of wedging the run.
	DecisionRequired bool
	DecisionState    string
	// Reported is true when a workflow-v2 `mode: report` class surfaced a
	// PROPOSAL at a live gate (ADR-066 / #2222): a run_auto_driven act:report
	// attribution row was appended, Action names the delegation verb the
	// class governs, and NOTHING else happened — no gate action was
	// dispatched, no campaign_gate_paged was emitted, and the run did not
	// park. Acted / Paged / DecisionRequired all stay false precisely so the
	// driver KEEPS POLLING: a report is something an operator may act on, not
	// a hand-off that stops the loop.
	Reported bool
	// Note is a short human-readable summary for the driver log /
	// observe-only audit. Always set.
	Note string
}

func observeOnly(note string) AutoDriveOutcome { return AutoDriveOutcome{Note: note} }

// AutoDriveRunGate re-evaluates the run's operator_agent delegation
// in-process (read-only, the same delegation.Evaluator path
// buildDelegationPayload uses) and, for a delegated knob whose condition
// is met AND whose real gate state independently confirms it
// (double-gating), dispatches the gate action under id via the extracted
// slice-1 service method — moving ADR-040's autonomous operator-agent
// cadence into the product (E25.6 / ADR-047).
//
// Fail-CLOSED to observe-only on EVERY uncertainty (binding approval
// condition 3): missing repositories, an unparseable / blockless spec, an
// evaluation error, a knob whose condition is unmet, a knob whose real
// gate state does not match, an unrecognised / unmapped condition, or an
// unconfigured merge seam. In each case the actor returns an observe-only
// outcome (no error) and changes nothing.
//
// A must_page_human condition (reviewer_reject, requirement_arbitration)
// is REFUSED before any knob is considered: the actor emits the
// campaign_gate_paged hand-off and takes no action. A genuine dispatch
// failure (the action was attempted and the service method errored) is
// returned as a non-nil error alongside the no-action outcome so the
// driver can log it; it is NOT swallowed as observe-only.
//
// campaignOverride carries the campaign-level operator_agent block bytes
// (E25.12 / #1451) the campaign driver threads from the campaign row.
// When non-empty it resolves as the effective delegation contract
// WHOLESALE (campaign > gate > workflow); empty/nil leaves each run on
// its own workflow contract, byte-identical to today. Malformed override
// bytes fail CLOSED to observe-only (the override cannot be trusted, so
// the actor takes no action) — handled in evaluateRunDelegation.
func (s *Server) AutoDriveRunGate(ctx context.Context, runRow *run.Run, id Identity, merger GitHubMerger, campaignOverride []byte) (AutoDriveOutcome, error) {
	res, wf, ok := s.evaluateRunDelegation(ctx, runRow, campaignOverride)
	if !ok || res == nil {
		return observeOnly("delegation not evaluable (fail-closed); observe-only"), nil
	}

	// Independently re-derive the gate state the double-gating checks
	// against (the actor never trusts the evaluator's read alone). A read
	// failure fails closed: we cannot confirm the gate, so observe-only.
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runRow.ID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "auto-drive: list stages failed; observe-only",
			slog.String("run_id", runRow.ID.String()), slog.String("error", err.Error()))
		return observeOnly("list stages failed (fail-closed); observe-only"), nil
	}
	open, err := s.cfg.ConcernRepo.ListOpenByRun(ctx, runRow.ID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "auto-drive: list open concerns failed; observe-only",
			slog.String("run_id", runRow.ID.String()), slog.String("error", err.Error()))
		return observeOnly("list open concerns failed (fail-closed); observe-only"), nil
	}

	// must_page_human REFUSAL wins over every delegated knob (the events
	// always page regardless of the may_* knobs). Checked first so a
	// gating reviewer reject / requirement arbitration never slips into an
	// auto-action.
	if pe := s.activePageEvent(ctx, runRow, &wf, res, open); pe != "" {
		s.emitCampaignGatePaged(ctx, runRow, id, pe)
		return AutoDriveOutcome{Paged: true, PageEvent: pe, Note: "must_page_human: " + pe}, nil
	}

	// `mode: report` SURFACES a proposal without acting (ADR-066 / #2222).
	// Checked after the page refusal (a paging condition still wins — an
	// event that must page the human is not a proposal) and before the
	// delegated dispatch, so the operator sees the proposal on the same pass
	// the gate became reachable. A report never suppresses an auto action for
	// longer than one poll cycle: the row is emitted at most once per gate
	// occurrence, so the next pass falls through to the dispatch below.
	if out, reported := s.reportMatrixProposals(ctx, runRow, id, res, stages, open); reported {
		return out, nil
	}

	// Dispatch the first met + state-matched delegated knob. The knobs are
	// mutually exclusive by condition at a single gate (clean approve vs
	// open-concern fix-up vs failed-stage retry vs post-gate merge), so the
	// fixed order only decides ties that cannot co-occur; the first
	// dispatch returns.

	// may_approve(clean_dual_approval) -> approve the gated review stage.
	if d, found := res.Decision(delegation.ActionApprove); found && d.Met {
		if gated := gatedReviewStage(stages); gated != nil {
			// FIX 1 (#2381): pre-check whether a delegated approve could EVER
			// advance THIS gate before submitting one. A human-quorum gate never
			// counts a delegated vote (#1709), and a firing / unevaluable escalation
			// or an unreadable approvals block all fail closed — decline and hand the
			// gate to the operator rather than recording a delegated vote that can
			// only wedge it (occupying the sole eligible approver's slot so the
			// operator's later real approve collides as a #986 duplicate). No approval
			// is submitted, so no approval row, no approval_submitted audit, and no
			// run_auto_driven row for the declined pass.
			if advance, reason := s.delegatedApproveWouldAdvance(ctx, gated); !advance {
				return AutoDriveOutcome{
					DecisionRequired: true,
					DecisionState:    operatorrole.DecisionStateHumanQuorumRequired,
					Action:           delegation.ActionApprove,
					Note:             reason,
				}, nil
			}
			result, aerr := s.approveStageAs(ctx, id, approveActionParams{
				Stage:         gated,
				Decision:      approval.DecisionApprove,
				DelegatedRule: string(d.Condition),
			})
			if aerr != nil {
				return AutoDriveOutcome{Action: delegation.ActionApprove, Note: "approve dispatch failed"}, aerr
			}
			// FIX 2 server leg (#2381): a delegated approve that came back a
			// DUPLICATE (Submit did not insert) made no progress — report
			// decision_required rather than a no-op acted:true, so handleAutoDrive
			// appends no attribution row for the no-op and the driver parks the
			// operator instead of looping the gate.
			if result.Duplicate != nil {
				return AutoDriveOutcome{
					DecisionRequired: true,
					DecisionState:    operatorrole.DecisionStateDelegatedApprovalNoProgress,
					Action:           delegation.ActionApprove,
					Note:             "a delegated approval was already recorded for this gate; no progress",
				}, nil
			}
			return AutoDriveOutcome{Acted: true, Action: delegation.ActionApprove, Note: "auto-approved " + string(gated.Type) + " gate"}, nil
		}
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "auto-drive: may_approve met but no review stage awaiting approval; observe-only",
			slog.String("run_id", runRow.ID.String()))
	}

	// may_route_fixup(convergent_concerns) -> route open concerns back.
	if d, found := res.Decision(delegation.ActionRouteFixup); found && d.Met {
		dispatched, ferr := s.autoFixup(ctx, id, runRow, stages, open, string(d.Condition))
		if ferr != nil {
			// An exhausted fix-up budget / hard ceiling is a well-defined,
			// EXPECTED gate state, not a dispatch failure (#2091): the direct
			// fishwawk_fixup_stage path reports it cleanly as a 422, so the
			// delegated arm must NOT bubble the sentinel to the 500 mapper.
			// Return a decision_required outcome (nil error) so the driver parks
			// the operator with real next moves (force an extra pass, waive /
			// defer the concerns, or cancel). A NON-sentinel ferr is a genuine
			// dispatch failure and still propagates as the raw error (a 500).
			if errors.Is(ferr, run.ErrFixupBudgetExhausted) {
				return AutoDriveOutcome{
					DecisionRequired: true,
					DecisionState:    "fixup_budget_exhausted",
					Action:           delegation.ActionRouteFixup,
					Note:             ferr.Error(),
				}, nil
			}
			if errors.Is(ferr, run.ErrFixupCeilingReached) {
				return AutoDriveOutcome{
					DecisionRequired: true,
					DecisionState:    "fixup_ceiling_reached",
					Action:           delegation.ActionRouteFixup,
					Note:             ferr.Error(),
				}, nil
			}
			return AutoDriveOutcome{Action: delegation.ActionRouteFixup, Note: "fixup dispatch failed"}, ferr
		}
		if dispatched {
			return AutoDriveOutcome{Acted: true, Action: delegation.ActionRouteFixup, Note: "auto-routed implement fix-up"}, nil
		}
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "auto-drive: may_route_fixup met but no fixup-eligible implement state; observe-only",
			slog.String("run_id", runRow.ID.String()))
	}

	// may_retry(infra_flake) -> retry the failed stage.
	if d, found := res.Decision(delegation.ActionRetry); found && d.Met {
		if failed := retryableFailedStage(stages); failed != nil {
			if _, rerr := s.retryStageAs(ctx, id, retryActionParams{
				StageID:       failed.ID,
				DelegatedRule: string(d.Condition),
			}); rerr != nil {
				return AutoDriveOutcome{Action: delegation.ActionRetry, Note: "retry dispatch failed"}, rerr
			}
			return AutoDriveOutcome{Acted: true, Action: delegation.ActionRetry, Note: "auto-retried failed stage"}, nil
		}
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "auto-drive: may_retry met but no retryable failed stage; observe-only",
			slog.String("run_id", runRow.ID.String()))
	}

	// may_merge(gates_resolved_ci_green) -> merge the PR via the seam.
	if d, found := res.Decision(delegation.ActionMerge); found && d.Met {
		// mergeGateReady's structural double-gate (PR open + no stage still
		// parked at an approval gate) is evaluator-independent and stays in the
		// delegated arm — it is the check the operator merge endpoint does NOT
		// share (a human merge deliberately does not block on a review stage
		// still awaiting approval; resolveReviewStageOnMerge settles it ON the
		// merge). The acceptance-gate + nil-merger + dispatch tail is the shared
		// helper both surfaces converge on (E48.7 / #1954).
		if !mergeGateReady(runRow, stages) {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "auto-drive: may_merge met but real merge state not ready; observe-only",
				slog.String("run_id", runRow.ID.String()))
		} else {
			outcome, gateState, derr := s.dispatchAcceptanceGatedMerge(ctx, runRow, stages, merger)
			switch outcome {
			case mergeDispatchAcceptanceNotReady:
				s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "auto-drive: may_merge met but acceptance gate not passed; observe-only",
					slog.String("run_id", runRow.ID.String()),
					slog.String("acceptance_gate_state", gateState),
					slog.Bool("acceptance_read_error", derr != nil))
				return observeOnly("acceptance gate not passed (fail-closed); observe-only"), nil
			case mergeDispatchNoMerger:
				s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "auto-drive: may_merge met but no merge client configured; observe-only",
					slog.String("run_id", runRow.ID.String()))
				return observeOnly("merge seam unconfigured (fail-closed); observe-only"), nil
			case mergeDispatchMerged:
				if derr != nil {
					return AutoDriveOutcome{Action: delegation.ActionMerge, Note: "merge dispatch failed"}, derr
				}
				// may_merge ENABLES/queues GitHub auto-merge; it does NOT prove
				// the PR has landed. The settle (pr_merged + run completion) is
				// deliberately left to the existing pull_request-closed webhook /
				// resolveReviewStageOnMerge path that fires when GitHub actually
				// merges — the actor must not record a merge GitHub may still
				// block (failing checks) or defer (queued). Recording pr_merged
				// here would complete the run before the merge is real.
				return AutoDriveOutcome{Acted: true, Action: delegation.ActionMerge, Note: "enabled auto-merge; webhook settles on merge"}, nil
			}
		}
	}

	// may_waive(solo_low) and any other delegated-but-unmapped knob are a
	// conservative no-op: auto-waive is out of this child's done-means
	// (follow-up tracks it), so the actor never auto-resolves a concern.
	if d, found := res.Decision(delegation.ActionWaive); found && d.Met {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "auto-drive: may_waive met but auto-waive is not implemented; observe-only (follow-up)",
			slog.String("run_id", runRow.ID.String()))
		return observeOnly("may_waive met but unmapped (auto-waive deferred); observe-only"), nil
	}

	return observeOnly("no delegated knob met and state-matched; observe-only"), nil
}

// mergeDispatchOutcome classifies what dispatchAcceptanceGatedMerge decided at
// the shared merge-dispatch tail (E48.7 / #1954) so its two callers — the
// delegated may_merge arm of AutoDriveRunGate and the operator merge endpoint
// (handleMergeRun) — translate it to their own surfaces (an observe-only note
// vs an HTTP status).
type mergeDispatchOutcome int

const (
	// mergeDispatchAcceptanceNotReady: the acceptance gate is pending / failed
	// (triage) / settled-outcome-unknown, or an acceptance-audit read error —
	// all fail-closed, do NOT merge (ADR-049 decision #6).
	mergeDispatchAcceptanceNotReady mergeDispatchOutcome = iota
	// mergeDispatchNoMerger: the merge seam is unconfigured — fail-closed.
	mergeDispatchNoMerger
	// mergeDispatchMerged: the merge was dispatched through the seam. The
	// returned error, when non-nil, is the seam's own dispatch failure.
	mergeDispatchMerged
)

// dispatchAcceptanceGatedMerge is the shared merge-dispatch tail extracted from
// AutoDriveRunGate's may_merge arm (E48.7 / #1954): it confirms the acceptance
// gate admits a merge (ADR-049 decision #6 — passed / not-declared /
// skipped-out-of-scope / not-validated / arbitrated / undecidable proceed;
// pending / un-arbitrated failed / outcome-unknown and a read
// error are all fail-closed), fails closed on a nil merger BEFORE any dispatch,
// and otherwise dispatches the merge through the seam.
//
// Returns the classified outcome, the acceptance gate state (for logging /
// error detail), and — only on mergeDispatchMerged — the seam's dispatch error.
// On mergeDispatchAcceptanceNotReady the returned error is the acceptance-audit
// read error (nil when the gate is simply not-passed rather than unreadable),
// so the caller can distinguish an evidence hole from a read failure.
//
// It is behavior-preserving for the delegated arm and is the SAME path the
// operator merge endpoint converges on by construction.
func (s *Server) dispatchAcceptanceGatedMerge(ctx context.Context, runRow *run.Run, stages []*run.Stage, merger GitHubMerger) (mergeDispatchOutcome, string, error) {
	gateState, gerr := s.acceptanceGateState(ctx, runRow, stages)
	// The admitted set is the SHARED acceptanceGateAdmitsMerge predicate (E66.37
	// / #2474), so the delegated merge and the operator merge endpoint cannot
	// drift. #1877: an out-of-scope skip is a legitimate terminal disposition
	// equivalent to a recorded outcome. #2347: so is a not_validated verdict —
	// the short-circuit settled the stage having verified zero criteria, which is
	// honest-but-mergeable, not a block. #2474: so is an ARBITRATED failed
	// verdict — this widens what a DELEGATED may_merge can land, deliberately:
	// the arbitration itself is operator-only and run-bound-token-forbidden, so
	// the operator decision remains the gating act. #2512: so is an UNDECIDABLE
	// verdict — the stage ran and reported at least one row it could not decide
	// with nothing failing, which is honest-but-mergeable exactly like #2347's
	// not_validated and, unlike an arbitrated failure, needs no operator act to
	// clear. A read error (gerr != nil) admits nothing — fail-closed.
	acceptanceMergeOK := gerr == nil && acceptanceGateAdmitsMerge(gateState)
	if !acceptanceMergeOK {
		return mergeDispatchAcceptanceNotReady, gateState, gerr
	}
	if merger == nil {
		return mergeDispatchNoMerger, gateState, nil
	}
	return mergeDispatchMerged, gateState, merger.MergePullRequest(ctx, runRow)
}

// evaluateRunDelegation resolves the run's effective delegation Result
// in-process via the same Evaluator path buildDelegationPayload uses
// (read-only). Returns ok=false on ANY failure — missing repositories,
// no cached spec, a parse failure, the workflow absent from the spec, a
// malformed campaign override, or an evaluation error — so the caller
// fails closed. A successful call with no effective operator_agent block
// returns (nil, wf, true): the caller treats a nil Result as
// nothing-delegated / observe-only.
//
// campaignOverride is the campaign-level operator_agent block bytes
// (E25.12). Empty/nil falls through to the workflow contract; well-formed
// bytes resolve as the effective block WHOLESALE; malformed bytes fail
// CLOSED (ok=false) so the actor never auto-acts through an unparseable
// override.
func (s *Server) evaluateRunDelegation(ctx context.Context, runRow *run.Run, campaignOverride []byte) (*delegation.Result, spec.Workflow, bool) {
	if s.cfg.RunRepo == nil || s.cfg.ConcernRepo == nil || s.cfg.AuditRepo == nil {
		return nil, spec.Workflow{}, false
	}
	if len(runRow.WorkflowSpec) == 0 {
		return nil, spec.Workflow{}, false
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "auto-drive: parse workflow spec failed; observe-only",
			slog.String("run_id", runRow.ID.String()), slog.String("error", err.Error()))
		return nil, spec.Workflow{}, false
	}
	wf, found := parsed.Workflows[runRow.WorkflowID]
	if !found {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "auto-drive: workflow not in cached spec; observe-only",
			slog.String("run_id", runRow.ID.String()), slog.String("workflow_id", runRow.WorkflowID))
		return nil, spec.Workflow{}, false
	}
	override, ok := parseCampaignOverride(campaignOverride)
	if !ok {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "auto-drive: parse campaign operator_agent override failed; observe-only",
			slog.String("run_id", runRow.ID.String()))
		return nil, wf, false
	}
	// The escalation resolver is REQUIRED at construction (E53.4 / #2227).
	// THIS is the site that would otherwise evaluate UNCLAMPED: the auto-drive
	// gate acts without an operator in the loop, so an unclamped matrix here
	// would auto-approve or auto-merge a change a fired `max_autonomy: low`
	// escalation had restricted — the silent control failure the
	// required-at-construction resolver exists to close. A constructor error
	// reuses this function's existing observe-only degradation.
	ev, err := delegation.NewEvaluator(s.cfg.RunRepo, s.cfg.ConcernRepo, s.cfg.AuditRepo, s.escalationResolver())
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "auto-drive: build delegation evaluator failed; observe-only",
			slog.String("run_id", runRow.ID.String()), slog.String("error", err.Error()))
		return nil, wf, false
	}
	res, err := ev.Evaluate(ctx, runRow, &wf, override)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "auto-drive: delegation evaluate failed; observe-only",
			slog.String("run_id", runRow.ID.String()), slog.String("error", err.Error()))
		return nil, wf, false
	}
	return res, wf, true
}

// parseCampaignOverride decodes the campaign-level operator_agent override
// bytes the campaign driver threads from the campaign row (E25.12 / #1451).
// Empty/nil bytes mean NO override — ok=true with a nil block, so the run
// falls through to its workflow contract (the unchanged-behavior path).
// Well-formed JSON yields the parsed block. Malformed bytes (or an unknown
// field — defensive belt-and-suspenders over the create-time
// DisallowUnknownFields validation) return ok=false so the caller fails
// closed to observe-only rather than auto-acting through an unparseable
// contract.
func parseCampaignOverride(raw []byte) (*spec.OperatorAgent, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var oa spec.OperatorAgent
	if err := dec.Decode(&oa); err != nil {
		return nil, false
	}
	return &oa, true
}

// activePageEvent returns the configured must_page_human event that is
// CURRENTLY active at the run's gate (one of res.MustPageHuman), or "".
// Only events with an observable v0 detector are recognised; an event in
// MustPageHuman with no active state returns "". Detection is
// fail-toward-paging: when in doubt the actor pages (refuses to act)
// rather than auto-acting through a must_page condition.
func (s *Server) activePageEvent(ctx context.Context, runRow *run.Run, wf *spec.Workflow, res *delegation.Result, open []*concern.Concern) string {
	for _, pe := range res.MustPageHuman {
		switch pe {
		case spec.PageEventReviewerReject, spec.PageEventGatingReviewerReject:
			if s.gatingImplementRejectPresent(ctx, runRow, wf) {
				return pe
			}
		case spec.PageEventRequirementArbitration:
			if requirementArbitrationOpen(open) {
				return pe
			}
		}
	}
	return ""
}

// gatingImplementRejectPresent reports whether the latest implement
// review round carries a reject verdict UNDER gating authority (agent-only
// review). A reject under advisory authority is arbitrable and does not
// page; a gateless stage has no agent-reviewer authority. Reads the audit
// surface directly — the same implement_review_started / implement_reviewed
// rounds the evaluator's reviewRound reads — so the page decision mirrors
// the evaluator's reviewer_reject classification.
func (s *Server) gatingImplementRejectPresent(ctx context.Context, runRow *run.Run, wf *spec.Workflow) bool {
	st := implementSpecStage(wf)
	if st == nil || st.Reviewers == nil {
		return false
	}
	if planreview.ResolveAuthority(*st.Reviewers) != planreview.AuthorityGating {
		return false
	}
	started, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runRow.ID, "implement_review_started")
	if err != nil {
		// Fail TOWARD paging: we have a gating reviewer and cannot confirm the
		// review round is clean, so the actor pages rather than risk
		// auto-acting through an unread reject (matches activePageEvent's
		// "when in doubt the actor pages" contract).
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "auto-drive: list implement_review_started failed; fail-toward-paging",
			slog.String("run_id", runRow.ID.String()), slog.String("error", err.Error()))
		return true
	}
	var latestStarted int64 = -1
	for _, e := range started {
		if e.Sequence > latestStarted {
			latestStarted = e.Sequence
		}
	}
	reviewed, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runRow.ID, "implement_reviewed")
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "auto-drive: list implement_reviewed failed; fail-toward-paging",
			slog.String("run_id", runRow.ID.String()), slog.String("error", err.Error()))
		return true
	}
	for _, e := range reviewed {
		if e.Sequence <= latestStarted {
			continue
		}
		var p planreview.ImplementReviewedPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		if p.Verdict == planreview.VerdictReject {
			return true
		}
	}
	return false
}

// requirementArbitrationOpen reports whether any open concern is a
// requirement-level disagreement (category "requirement") — a human
// judgment the actor refuses to auto-route, pages on instead.
func requirementArbitrationOpen(open []*concern.Concern) bool {
	for _, c := range open {
		if strings.EqualFold(c.Category, requirementConcernCategory) {
			return true
		}
	}
	return false
}

// emitCampaignGatePaged writes the run-chained campaign_gate_paged
// hand-off entry under id (ActorAgent for the operator-agent subject).
// Best-effort: a failed append logs and never blocks the driver — the
// actor still took no gate action, which is the safe outcome.
func (s *Server) emitCampaignGatePaged(ctx context.Context, runRow *run.Run, id Identity, pageEvent string) {
	if s.cfg.AuditRepo == nil {
		return
	}
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	kind := actorKindForSubject(subject)
	payload, _ := json.Marshal(map[string]any{
		"page_event": pageEvent,
		"run_id":     runRow.ID.String(),
		"reason":     "campaign auto-driver refused a must_page_human condition; handing the gate off to a human (E25.7)",
	})
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        runRow.ID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryCampaignGatePaged,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError, "auto-drive: emit campaign_gate_paged failed",
			slog.String("run_id", runRow.ID.String()),
			slog.String("page_event", pageEvent),
			slog.String("error", err.Error()))
	}
}

// autoFixup routes every open implement-stage concern back to the agent
// under id via fixupStageAs, defaulting the bounded-pass budget exactly
// as the HTTP handler does. Returns dispatched=false (no error) when the
// double-gate fails — no implement stage, an ineligible state, or no open
// concern to route — so the caller falls to observe-only.
func (s *Server) autoFixup(ctx context.Context, id Identity, runRow *run.Run, stages []*run.Stage, open []*concern.Concern, rule string) (bool, error) {
	impl := implementStage(stages)
	if impl == nil || !fixupEligibleState(impl, stages) {
		return false, nil
	}
	var selected []planreview.Concern
	var ids []uuid.UUID
	for _, c := range open {
		if c.StageKind != concern.StageKindImplement || c.StageID != impl.ID {
			continue
		}
		selected = append(selected, planreview.Concern{
			Severity:       planreview.ConcernSeverity(c.Severity),
			Category:       c.Category,
			Note:           c.Note,
			SuggestedPatch: c.SuggestedPatch,
		})
		ids = append(ids, c.ID)
	}
	if len(selected) == 0 {
		return false, nil
	}
	priorPasses, err := s.countFixupPasses(ctx, runRow.ID, impl.ID)
	if err != nil {
		return false, err
	}
	// Routed through the SHARED chokepoint (#3085) rather than the old
	// no-change-only counter, which counted ONLY the #967 signal and so already
	// diverged from the HTTP handler by missing the #1957 category-C refund
	// entirely. The crash count is discarded — the auto-drive path has no 422
	// body to carry it. Pinned by TestAutoFixup_InfraRefundAdmitsPass (category
	// C) and TestAutoFixup_CrashRefundAdmitsPass (category A).
	refunded, _, err := s.fixupRefundedPasses(ctx, runRow.ID, impl.ID)
	if err != nil {
		return false, err
	}
	if refunded > priorPasses {
		refunded = priorPasses
	}
	// The PR-body-unsatisfiable set (#2782) is unused on the auto-drive path —
	// there is no operator tool result to warn on — so it is discarded; the
	// advisory audit entry is still written inside fixupStageAs.
	if _, _, err := s.fixupStageAs(ctx, id, fixupActionParams{
		StageID: impl.ID,
		Options: run.FixupOptions{
			PriorPassCount: priorPasses,
			MaxPasses:      defaultMaxFixupPasses + refunded,
			HardCeiling:    defaultFixupCeiling,
		},
		Selected:       selected,
		ConcernIDs:     ids,
		PriorPasses:    priorPasses,
		RefundedPasses: refunded,
		DelegatedRule:  rule,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// --- mode: report (ADR-066 / #2222) -----------------------------------------

// reportMatrixProposals walks the resolved action matrix for classes at
// `mode: report` and surfaces the FIRST one whose gate is live as a
// run_auto_driven act:"report" attribution row, acting on nothing.
//
// FIRING RULE (the operator's binding ruling on #2222): a report entry fires
// when the gate is LIVE, and additionally requires its condition to be met
// only WHEN A `when` IS DECLARED. A bare `report` entry — which is legal,
// since validation requires `when` for `mode: auto` only — therefore
// surfaces whenever the gate is reachable. That is what report is for:
// naming a proposal the operator may act on. Gating a bare entry behind a
// condition that need not exist would make the mode inert.
//
// IDEMPOTENCY (binding condition 2): the driver keeps polling rather than
// parking, so an unguarded arm would append a row on EVERY poll for as long
// as the gate stayed live — flooding the evidentiary surface operators and
// the verifier read. Emission is therefore keyed on (run, gate occurrence,
// class) and happens AT MOST ONCE per gate occurrence; a subsequent pass
// falls through to the delegated dispatch below.
//
// Every uncertainty skips the class rather than emitting: an extension class
// (no backend-observable gate), a gate that is not live, a declared
// condition that is unmet or not evaluable, an unreadable audit chain (which
// would make the dedupe unanswerable), and an append failure — the last
// logged, never reported as if it had landed.
func (s *Server) reportMatrixProposals(ctx context.Context, runRow *run.Run, id Identity, res *delegation.Result, stages []*run.Stage, open []*concern.Concern) (AutoDriveOutcome, bool) {
	if s.cfg.AuditRepo == nil {
		return AutoDriveOutcome{}, false
	}
	for _, entry := range res.Matrix {
		if entry.Mode != spec.ModeReport {
			continue
		}
		// An EXTENSION class maps to no delegation verb, so no gate state
		// identifies it — it can never be live and never reports. Same
		// fail-closed-by-construction posture the grammar takes at parse time,
		// where `mode: auto` on an extension class is rejected outright.
		action, known := delegation.ActionForClass(entry.Action)
		if !known {
			continue
		}
		occurrence, anchor, live := reportGateOccurrence(action, runRow, stages, open)
		if !live {
			continue
		}
		// Fold a re-occurrence discriminator into the occurrence key so a gate
		// that CLOSED and RE-OPENED (a fresh review round for approve/route_fixup/
		// waive, a subsequent distinct FAILURE of the same stage for retry, a
		// re-becoming-ready merge gate for merge) is a DISTINCT occurrence and
		// re-surfaces the proposal, while a gate that stays continuously live
		// keeps ONE key across polls (at most once per genuine occurrence). This
		// is what keeps a required proposal from being suppressed on a later,
		// materially-changed occurrence of the same gate. The discriminator is
		// dispatched on the ACTION CLASS (reportOccurrenceKey), not on the
		// anchor's type.
		occurrence = s.reportOccurrenceKey(ctx, runRow, action, anchor, occurrence)
		// A declared `when` must be MET. A report entry whose `when` names
		// another class's condition produces no Reports decision at all, so the
		// not-found branch is the same fail-closed skip.
		if entry.Condition != "" {
			d, found := res.Report(action)
			if !found || !d.Met {
				continue
			}
		}
		note := "mode: report — " + action + " proposed at a live gate (no action taken)"
		// Serialize the dedupe-check -> append critical section per run so two
		// concurrent AutoDriveRunGate callers in THIS process (the POST
		// /auto-drive endpoint and the campaign driver) cannot both observe "no
		// row yet" for the same (run, occurrence, class) and both append,
		// duplicating the act:report row (binding condition 2 under concurrency).
		// A cross-PROCESS guarantee needs a DB uniqueness constraint on the row;
		// this closes the realistic single-process race.
		appended, readErr, appendErr := func() (bool, error, error) {
			lock := reportEmitLockFor(runRow.ID)
			lock.Lock()
			defer lock.Unlock()
			emitted, err := s.reportAlreadyEmitted(ctx, runRow, action, occurrence)
			if err != nil {
				return false, err, nil
			}
			if emitted {
				return false, nil, nil
			}
			if err := s.appendRunAutoDrivenReport(ctx, runRow, id, action, entry, occurrence, note); err != nil {
				return false, nil, err
			}
			return true, nil, nil
		}()
		if readErr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "auto-drive: read run_auto_driven for report dedupe failed; skipping report",
				slog.String("run_id", runRow.ID.String()), slog.String("action", action), slog.String("error", readErr.Error()))
			continue
		}
		if appendErr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelError, "auto-drive: append run_auto_driven report row failed",
				slog.String("run_id", runRow.ID.String()), slog.String("action", action), slog.String("error", appendErr.Error()))
			continue
		}
		if !appended {
			// Already reported this occurrence — fall through to the next class.
			continue
		}
		return AutoDriveOutcome{Reported: true, Action: action, Note: note}, true
	}
	return AutoDriveOutcome{}, false
}

// reportGateOccurrence answers "is this action's gate live right now, WHICH
// occurrence of it is this, and which STAGE anchors it" — the base identity
// the report row is deduped on. The base occurrence is the gate-bearing stage
// plus its state (or the run, for the run-level merge gate), so one live gate
// reports once while a later gate of the same class reports again. The
// returned anchor stage (nil for the run-level merge gate) lets the caller
// fold a re-open discriminator into the key (reportOccurrenceKey).
//
// Liveness reuses the same evaluator-independent double-gate derivations the
// delegated arms use, so a proposal is only ever surfaced where the
// corresponding action could actually be taken. waive is anchored to the
// stage parked at the approval gate where a waive decision is taken, which
// is also why a run parked at awaiting_input surfaces nothing.
func reportGateOccurrence(action string, runRow *run.Run, stages []*run.Stage, open []*concern.Concern) (string, *run.Stage, bool) {
	switch action {
	case delegation.ActionApprove:
		if gated := gatedReviewStage(stages); gated != nil {
			return stageOccurrence(gated), gated, true
		}
	case delegation.ActionRouteFixup:
		if impl := implementStage(stages); impl != nil && fixupEligibleState(impl, stages) && len(open) > 0 {
			return stageOccurrence(impl), impl, true
		}
	case delegation.ActionWaive:
		if gated := currentApprovalGateStage(stages); gated != nil && len(open) > 0 {
			return stageOccurrence(gated), gated, true
		}
	case delegation.ActionRetry:
		if failed := retryableFailedStage(stages); failed != nil {
			return stageOccurrence(failed), failed, true
		}
	case delegation.ActionMerge:
		if mergeGateReady(runRow, stages) {
			return "run:" + runRow.ID.String() + ":merge_ready", nil, true
		}
	}
	return "", nil, false
}

// stageOccurrence names the BASE identity of a stage-anchored gate: the stage
// plus the state it is parked in. On its own this key treats a gate that
// closes and re-opens into the same state on the same stage (a fix-up round
// trip, or a stage that fails, is retried and fails AGAIN) as the SAME
// occurrence — which would suppress a fresh proposal even after the gate's
// evidence materially changed. That is why reportOccurrenceKey folds a
// per-class re-occurrence discriminator on top of this base: the base holds
// duplicate poll cycles to one row; the discriminator makes a genuine
// re-opening a distinct occurrence.
func stageOccurrence(st *run.Stage) string {
	return "stage:" + st.ID.String() + ":" + string(st.State)
}

// reportOccurrenceKey folds a per-CLASS re-occurrence discriminator into a
// gate's base occurrence key, so every gate class re-surfaces its proposal on
// a genuine re-occurrence while a gate that stays continuously live keeps ONE
// key across polls (a report at most once per genuine occurrence). The
// discriminator is dispatched on the ACTION, not the anchor's type:
//
//   - retry folds the count of the anchored stage's retries (stage_retried +
//     stage_override_retried), the signal that distinguishes one FAILURE of a
//     stage from the next. A failed plan/implement stage previously picked up
//     the review-round fold incidentally, because dispatch keyed on the
//     anchor's TYPE; the retry count is the discriminator that actually tracks
//     a re-occurrence of the failure.
//   - merge folds a ready-transition proxy (the run's approval_submitted
//     count), which advances exactly when a re-closed gate lets the merge gate
//     become ready again (see mergeReadyRoundCount for why this is sound).
//   - every other class (approve / route_fixup / waive) folds the review-round
//     count unchanged.
//
// Every discriminator degrades to the BASE key (via the ok=false arms of its
// counter) on an audit read failure or a zero count, so an unreadable
// discriminator at worst UNDER-emits a re-surfaced proposal — the safe
// direction (silence, never a flood).
func (s *Server) reportOccurrenceKey(ctx context.Context, runRow *run.Run, action string, anchor *run.Stage, base string) string {
	switch action {
	case delegation.ActionRetry:
		if n, ok := s.stageRetryCount(ctx, runRow.ID, anchor); ok {
			return base + ":retry:" + strconv.Itoa(n)
		}
		return base
	case delegation.ActionMerge:
		if n, ok := s.mergeReadyRoundCount(ctx, runRow.ID); ok {
			return base + ":approvals:" + strconv.Itoa(n)
		}
		return base
	default:
		if round, ok := s.reviewRoundCount(ctx, runRow.ID, anchor); ok {
			return base + ":round:" + strconv.Itoa(round)
		}
		return base
	}
}

// stageRetryCount returns how many times the anchored stage has been retried
// (its stage_retried + stage_override_retried audit entries, keyed to the
// stage), and ok=false when the anchor is nil, EITHER read errors, or the
// total is zero. ListForRunByCategory is RUN-scoped, so the count is filtered
// on Entry.StageID — the same stage-keyed filter countFixupPasses uses — so a
// SIBLING stage's retry cannot advance THIS stage's key and emit a second row
// inside one occurrence (the flooding direction). The override-retry category
// counts too: an operator override re-dispatch re-opens the same stage, and
// its next failure is a genuinely-distinct occurrence.
//
// ORDERING (retry.go:492): a stage_retried row is written only AFTER
// run.RetryStage has flipped the stage out of `failed`, so a poll that reads
// count=N also sees a stage that has left `failed` for the Nth time — the
// count and the failed state are coupled by that ordering, and the count
// advances across distinct FAILURES rather than within one continuously-failed
// span. A read failure returning ok=false keeps the BASE key (under-emission).
func (s *Server) stageRetryCount(ctx context.Context, runID uuid.UUID, anchor *run.Stage) (int, bool) {
	if anchor == nil {
		return 0, false
	}
	retried, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryStageRetried)
	if err != nil {
		return 0, false
	}
	overridden, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryStageOverrideRetried)
	if err != nil {
		return 0, false
	}
	n := 0
	for _, e := range retried {
		if e.StageID != nil && *e.StageID == anchor.ID {
			n++
		}
	}
	for _, e := range overridden {
		if e.StageID != nil && *e.StageID == anchor.ID {
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return n, true
}

// mergeReadyRoundCount returns the run's approval_submitted count as a proxy
// for how many times the merge gate has become ready, and ok=false on a read
// error or a zero count. This is a SOUND ready-transition proxy: mergeGateReady
// requires that NO stage is parked in awaiting_approval, and an
// approval_submitted row can only be appended against a stage that IS parked
// there (both append sites — approvals.go and issue_approval.go via
// findAwaitingApprovalStage — resolve a stage at the approval gate first). So
// the count cannot advance during a continuously-merge-ready window (the
// flooding guarantee holds) and DOES advance across the un-ready/re-ready cycle
// a re-opened gate produces.
//
// RESIDUAL (known limitation): a merge gate that re-becomes ready WITHOUT an
// intervening approval — a gate re-closed by a retry or a fix-up re-dispatch —
// keeps the same key and UNDER-emits. That is the same safe direction the
// review-round fold takes; closing it would need a new audit category recording
// a stage entering/leaving awaiting_approval, and summing today's signals would
// break the flooding guarantee (mergeGateReady ignores failed stages, so a
// stage_retried row can land while the gate is continuously ready). A read
// failure returning ok=false keeps the BASE key (under-emission).
func (s *Server) mergeReadyRoundCount(ctx context.Context, runID uuid.UUID) (int, bool) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "approval_submitted")
	if err != nil || len(entries) == 0 {
		return 0, false
	}
	return len(entries), true
}

// reviewRoundCount returns the number of review rounds started for the anchor
// stage (the count of its *_review_started audit entries), and ok=false when
// the anchor is nil, its stage type carries no review surface (only plan and
// implement stages do), no round has started, or the audit read fails. A read
// failure returning ok=false keeps the BASE key, which at worst under-emits a
// re-surfaced proposal — the safe direction (silence, never a flood).
func (s *Server) reviewRoundCount(ctx context.Context, runID uuid.UUID, anchor *run.Stage) (int, bool) {
	if anchor == nil {
		return 0, false
	}
	var startedCat string
	switch anchor.Type {
	case run.StageTypePlan:
		startedCat = "plan_review_started"
	case run.StageTypeImplement:
		startedCat = "implement_review_started"
	default:
		return 0, false
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, startedCat)
	if err != nil || len(entries) == 0 {
		return 0, false
	}
	return len(entries), true
}

// reportEmitStripes bounds the report-emission lock set. A fixed stripe count
// (rather than a per-run map that grows without bound) keeps the lock memory
// constant regardless of how many runs the process ever drives; two unrelated
// runs colliding on a stripe only briefly serialize their (rare) report
// emission, which is harmless.
const reportEmitStripes = 64

var reportEmitMu [reportEmitStripes]sync.Mutex

// reportEmitLockFor returns the stripe mutex guarding a run's report
// dedupe-check -> append critical section. See reportMatrixProposals for why
// the section is serialized.
func reportEmitLockFor(runID uuid.UUID) *sync.Mutex {
	var h uint32
	for _, b := range runID {
		h = h*31 + uint32(b)
	}
	return &reportEmitMu[h%reportEmitStripes]
}

// currentApprovalGateStage returns the lowest-sequence stage parked at an
// approval gate, of any type — the gate at which a waive decision is taken.
func currentApprovalGateStage(stages []*run.Stage) *run.Stage {
	var gated *run.Stage
	for _, st := range stages {
		if st.State != run.StageStateAwaitingApproval {
			continue
		}
		if gated == nil || st.Sequence < gated.Sequence {
			gated = st
		}
	}
	return gated
}

// --- double-gate state derivations (independent of the evaluator) -----------

// gatedReviewStage returns the lowest-sequence stage parked in
// awaiting_approval whose type carries an agent-reviewer surface (plan or
// implement). nil means no auto-approvable gate is open — the approve
// double-gate fails closed.
func gatedReviewStage(stages []*run.Stage) *run.Stage {
	var gated *run.Stage
	for _, st := range stages {
		if st.State != run.StageStateAwaitingApproval {
			continue
		}
		if st.Type != run.StageTypePlan && st.Type != run.StageTypeImplement {
			continue
		}
		if gated == nil || st.Sequence < gated.Sequence {
			gated = st
		}
	}
	return gated
}

// implementStage returns the run's implement stage, or nil.
func implementStage(stages []*run.Stage) *run.Stage {
	for _, st := range stages {
		if st.Type == run.StageTypeImplement {
			return st
		}
	}
	return nil
}

// fixupEligibleState reports whether the implement stage is in a state a
// fix-up can re-open from: awaiting_approval (commit-yourself flow) or
// succeeded while a review stage is still open (push_and_open_pr flow).
// Mirrors run.FixupStage's applicability switch so the actor's double-gate
// matches the domain's contract.
func fixupEligibleState(impl *run.Stage, stages []*run.Stage) bool {
	switch impl.State {
	case run.StageStateAwaitingApproval:
		return true
	case run.StageStateSucceeded:
		for _, st := range stages {
			if st.Type == run.StageTypeReview &&
				(st.State == run.StageStateAwaitingApproval || st.State == run.StageStatePending) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// retryableFailedStage returns the highest-sequence failed stage whose
// recorded failure is retryable (the A/C re-dispatch path the evaluator's
// infra_flake condition is a subset of). nil means the retry double-gate
// fails closed.
func retryableFailedStage(stages []*run.Stage) *run.Stage {
	var failed *run.Stage
	for _, st := range stages {
		if st.State != run.StageStateFailed || st.FailureCategory == nil {
			continue
		}
		reason := ""
		if st.FailureReason != nil {
			reason = *st.FailureReason
		}
		if !run.RetryableFailure(*st.FailureCategory, reason) {
			continue
		}
		if failed == nil || st.Sequence > failed.Sequence {
			failed = st
		}
	}
	return failed
}

// mergeGateReady is the merge double-gate: the run carries an open PR and
// no stage is still parked at an approval gate. Independent of the
// evaluator's gates_resolved_ci_green read so a stale Decision cannot
// drive a merge of a run still awaiting approval.
func mergeGateReady(runRow *run.Run, stages []*run.Stage) bool {
	if runRow.PullRequestURL == nil || *runRow.PullRequestURL == "" {
		return false
	}
	for _, st := range stages {
		if st.State == run.StageStateAwaitingApproval {
			return false
		}
	}
	return true
}

// implementSpecStage returns the workflow's implement stage definition
// (by type), or nil.
func implementSpecStage(wf *spec.Workflow) *spec.Stage {
	for i := range wf.Stages {
		if wf.Stages[i].Type == spec.StageTypeImplement {
			return &wf.Stages[i]
		}
	}
	return nil
}
