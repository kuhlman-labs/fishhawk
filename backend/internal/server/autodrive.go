package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
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
// Exactly one of Acted / Paged is true on a non-observe-only outcome.
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

	// Dispatch the first met + state-matched delegated knob. The knobs are
	// mutually exclusive by condition at a single gate (clean approve vs
	// open-concern fix-up vs failed-stage retry vs post-gate merge), so the
	// fixed order only decides ties that cannot co-occur; the first
	// dispatch returns.

	// may_approve(clean_dual_approval) -> approve the gated review stage.
	if d, found := res.Decision(delegation.ActionApprove); found && d.Met {
		if gated := gatedReviewStage(stages); gated != nil {
			if _, aerr := s.approveStageAs(ctx, id, approveActionParams{
				Stage:         gated,
				Decision:      approval.DecisionApprove,
				DelegatedRule: string(d.Condition),
			}); aerr != nil {
				return AutoDriveOutcome{Action: delegation.ActionApprove, Note: "approve dispatch failed"}, aerr
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
		// Acceptance-gate AND (E31.17 / #1568): on a run whose workflow declares
		// an acceptance stage the merge is gated on the acceptance_passed
		// evidence condition (ADR-049 decision #6). mergeGateReady's structural
		// checks are kept; the acceptance gate is added as an independent AND at
		// this call site so its unit tests stay valid. FAIL-CLOSED: a read error
		// or any non-passed/non-declared state (pending / settled-outcome-unknown
		// / failed) is observe-only — the actor never merges on unknown or unmet
		// acceptance evidence.
		gateState, gerr := s.acceptanceGateState(ctx, runRow, stages)
		acceptanceMergeOK := gerr == nil && (gateState == acceptanceGateNotDeclared || gateState == acceptanceGatePassed)
		switch {
		case !mergeGateReady(runRow, stages):
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "auto-drive: may_merge met but real merge state not ready; observe-only",
				slog.String("run_id", runRow.ID.String()))
		case !acceptanceMergeOK:
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "auto-drive: may_merge met but acceptance gate not passed; observe-only",
				slog.String("run_id", runRow.ID.String()),
				slog.String("acceptance_gate_state", gateState),
				slog.Bool("acceptance_read_error", gerr != nil))
			return observeOnly("acceptance gate not passed (fail-closed); observe-only"), nil
		case merger == nil:
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "auto-drive: may_merge met but no merge client configured; observe-only",
				slog.String("run_id", runRow.ID.String()))
			return observeOnly("merge seam unconfigured (fail-closed); observe-only"), nil
		default:
			if merr := merger.MergePullRequest(ctx, runRow); merr != nil {
				return AutoDriveOutcome{Action: delegation.ActionMerge, Note: "merge dispatch failed"}, merr
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
	ev := &delegation.Evaluator{Stages: s.cfg.RunRepo, Concerns: s.cfg.ConcernRepo, Audit: s.cfg.AuditRepo}
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
	refunded, err := s.countFixupNoChangeRefunds(ctx, runRow.ID, impl.ID)
	if err != nil {
		return false, err
	}
	if refunded > priorPasses {
		refunded = priorPasses
	}
	if _, err := s.fixupStageAs(ctx, id, fixupActionParams{
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
