package issuecomment

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The living anchor (#1054) is edited in place on every transition, but
// GitHub does NOT deliver a webhook or notification on a comment EDIT —
// only on a new comment. So a watcher who muted the issue (or just isn't
// looking) would miss a state change that genuinely needs their
// attention. ping.go closes that gap: when a page-class event newly
// appears in the audit chain, the notifier posts a ONE-LINE new comment
// that pings subscribers and links back to the anchor. Everything else
// stays edit-only.
//
// Page-class events (the ones worth interrupting a human for):
//   - a plan gate is actually awaiting human approval (NOT every
//     plan_generated — a gateless/routine plan stage that never parks
//     awaiting_approval produces no ping; #1054 review)
//   - a reviewer rejected (plan or implement)
//   - a must_page_human (ADR-040) event always parks for a human and has
//     no other issue-comment surface — today the scope-amendment request
//     (scope_amendment_requested). The request-time may_* delegation knobs
//     are NOT chain-derivable, but the concrete must_page_human EVENTS in
//     the closed v0 set (spec.PageEvent*) are audit categories, and this
//     surfaces the one that is otherwise silent on edits.
//   - the planner parked the plan stage at awaiting_input with a
//     clarification_request (#1057) — a must_page_human event
//     (spec.PageEventClarificationRequest) that waits on the operator's
//     answers before planning can resume.
//   - CI failed (#1045)
//   - a crew disagreement was escalated to the captain — either a SENT root
//     `escalation` or a thread whose reject-and-reply round bound is exhausted
//     (E77.6 / #3740). Both page at EVERY autonomy tier, `low` included,
//     because the refusal is keyed to server.PageEventCrewEscalation rather
//     than to a `page_human_on` list.
//   - a fix-up push re-opened the settled acceptance stage and no dispatcher
//     will run it (#5308): the server wrote acceptance_reopen_awaiting_operator
//     after the orchestrator advance left the stage parked on a run that is
//     not host-dispatched. Resolved by a later acceptance_dispatched or
//     acceptance_outcome_recorded (pageEventResolved).
//   - a campaign-driver act left a stage parked at awaiting_host_dispatch on a
//     run no dispatcher will spawn (#5562): the server wrote
//     campaign_driver_act_awaiting_operator for the stage the act itself parked.
//     Resolved once that stage leaves the park episode the row names
//     (pageEventResolved).
//
// Dedup is per source audit event: each ping records the originating
// entry's audit Sequence on CategoryAnchorPingPosted, so a re-render of
// the anchor (which re-reads the whole chain) never double-pings the same
// event.

// CategoryAnchorPingPosted records that the notifier posted a page-class
// ping comment for a specific source audit event. The payload carries
// `source_sequence` (the dedup key) and `event` (a short label).
const CategoryAnchorPingPosted = "anchor_ping_posted"

// pageEvent is one page-class event derived from the audit chain. The
// sequence is the originating entry's audit Sequence — the dedup key.
type pageEvent struct {
	sequence int64
	// kind is a short stable token stored in the ping audit payload.
	kind string
	// message is the one-line comment body (sans the anchor link, which
	// firePings appends).
	message string
	// stageID and episodeKey are set only for the two stall-watch kinds
	// (#5497) and the #5562 driver-act page: the stage the row names and the
	// condition episode it was written for. pageEventResolved compares
	// episodeKey with the named stage's CURRENT episode, so a row from an
	// ended episode never posts.
	stageID    uuid.UUID
	episodeKey string
	// nextAction is the stall row's next_action when it is in the closed
	// stallNextActions set, else "". The parent arm of stallPageResolved
	// reads it (a fishhawk_resume_run page needs a live parent).
	nextAction string
}

// Stall-watch page kinds (#5497). The kind tokens differ from the server's
// audit categories (stage_failed_awaiting_operator,
// child_dispatch_awaiting_operator) so each case label in this file stays
// unique; the category literals are duplicated below because this package
// cannot import server.
const (
	pageKindFailedStageStalled   = "failed_stage_stalled"
	pageKindChildDispatchStalled = "child_dispatch_stalled"
)

// pageKindDriverActAwaitingOperator is the #5562 page kind projected from
// server.CategoryCampaignDriverActAwaitingOperator. The token differs from the
// category so each case label in this file stays unique.
const pageKindDriverActAwaitingOperator = "driver_act_awaiting_operator_dispatch"

// stallNextActions is the closed next-action set a stall row may name. A
// payload next_action outside it renders the generic remedy line instead, so
// no payload string reaches the comment body.
var stallNextActions = map[string]struct{}{
	"fishhawk_retry_stage":  {},
	"fishhawk_revive_run":   {},
	"fishhawk_resume_run":   {},
	"fishhawk_run_children": {},
}

// FailedStageStallEpisode derives the condition-episode key of a failed stage
// (#5497). ok is true only when the stage is failed and carries EndedAt. The
// key is "<stage_id>/<ended_at RFC3339Nano UTC>/<self_retry_count>": ended_at
// is stamped by the failed transition, and RetryStageState clears it and bumps
// self_retry_count on every re-open (retry_stage, revive_run and resume_run's
// in-place reset), so a retried-and-refailed stage is a NEW episode. It is the
// ONE derivation the server's stall-watch writer and this package's
// resolution share. No stage-transition audit row exists to key on, so the
// stage row's own per-transition stamps identify the episode.
func FailedStageStallEpisode(st *run.Stage) (string, bool) {
	if st == nil || st.State != run.StageStateFailed || st.EndedAt == nil {
		return "", false
	}
	return fmt.Sprintf("%s/%s/%d", st.ID, st.EndedAt.UTC().Format(time.RFC3339Nano), st.SelfRetryCount), true
}

// ChildDispatchStallEpisode derives the condition-episode key of an
// undispatched decomposition-child implement stage (#5497). ok is true only
// when the stage is pending or awaiting_host_dispatch. The key is
// "<stage_id>/<self_retry_count>/<dispatched_at RFC3339Nano UTC, or never>":
// dispatched_at is the per-attempt clock migration 0072's trigger re-stamps on
// every entry into dispatched, so a re-dispatch that returns to pending
// without a retry bump is still a new episode.
func ChildDispatchStallEpisode(st *run.Stage) (string, bool) {
	if st == nil || (st.State != run.StageStatePending && st.State != run.StageStateAwaitingHostDispatch) {
		return "", false
	}
	dispatched := "never"
	if st.DispatchedAt != nil {
		dispatched = st.DispatchedAt.UTC().Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("%s/%d/%s", st.ID, st.SelfRetryCount, dispatched), true
}

// HostDispatchParkEpisode derives the park-episode key of a stage parked at
// awaiting_host_dispatch (#5562). ok is true only in that state. The key is
// "<stage_id>/<self_retry_count>/<dispatched_at RFC3339Nano UTC, or never>":
// the next park of the same stage follows either a dispatch (migration 0072's
// trigger re-stamps dispatched_at on every entry into dispatched) or a retry
// re-open (which bumps self_retry_count), so each park episode has its own key.
// It is the ONE derivation the server's campaign-driver page writer (dedup)
// and this package's resolution share. No stage-transition audit row exists to
// key on, so the stage row's own per-transition stamps identify the episode.
func HostDispatchParkEpisode(st *run.Stage) (string, bool) {
	if st == nil || st.State != run.StageStateAwaitingHostDispatch {
		return "", false
	}
	dispatched := "never"
	if st.DispatchedAt != nil {
		dispatched = st.DispatchedAt.UTC().Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("%s/%d/%s", st.ID, st.SelfRetryCount, dispatched), true
}

// pageClassEvents projects the page-class events out of the run's audit
// chain, ascending by sequence. Pure + dedup-friendly: each event is
// keyed by its source Sequence so the caller can skip ones already
// pinged. Categories that are NOT page-class (status edits, plan scope
// prechecks, cost rollups, etc.) produce no event. `stages` gates the
// plan-awaiting-approval event on an actual human-approval wait.
func pageClassEvents(entries []*audit.Entry, stages []*run.Stage) []pageEvent {
	var out []pageEvent

	// "Gate awaiting human approval" fires ONLY when a plan stage is
	// actually parked at an approval gate right now. A gateless/routine
	// plan stage proceeds straight to its next stage and never enters
	// awaiting_approval, so plan_generated alone is not sufficient — we'd
	// otherwise ping with misleading "awaiting your review" text on every
	// ungated plan (#1054 review). Keyed to the LATEST plan_generated
	// sequence so a replan round pings once per round (dedup absorbs
	// re-renders).
	if planStageAwaitingApproval(stages) {
		if seq := latestSequenceForCategory(entries, "plan_generated"); seq > 0 {
			out = append(out, pageEvent{
				sequence: seq,
				kind:     "plan_awaiting_approval",
				message:  "📋 A plan is ready and awaiting your review.",
			})
		}
	}

	for _, e := range entries {
		switch e.Category {
		case "plan_reviewed", "implement_reviewed":
			verdict, model := decodeReviewerVerdict(e.Payload)
			if verdict == "reject" {
				stage := strings.TrimSuffix(e.Category, "_reviewed")
				// Name the reviewer's role ahead of its model (E89.33 / #4669):
				// "Security Reviewer (<model>)", or the stage's standard role
				// ("Plan Reviewer" / "PR Reviewer") alone when no model is known.
				who := roleWithModel(reviewerRole(stage, reviewerPersonaOf(e.Payload)), model)
				// A reviewer reject is ADVISORY — the operator arbitrates the
				// gate. Word it so it cannot read as a GATE rejection (a stale
				// "🚫 rejected the plan" as the thread's last word when the
				// operator in fact approved over it). Once the operator
				// arbitrates (an approval_submitted, or a fixup dispatch for an
				// implement reject), firePings treats the not-yet-posted page as
				// already-resolved via pageEventResolved and records the dedup
				// row WITHOUT posting — the arbitration stays on the anchor
				// timeline and no longer pages the approver about their own
				// action (#1786). Advisory-reject arbitration is deliberately
				// NOT its own page-class event.
				out = append(out, pageEvent{
					sequence: e.Sequence,
					kind:     stage + "_review_rejected",
					message:  fmt.Sprintf("🚫 %s flagged a blocking concern on the %s (advisory reject) — awaiting operator arbitration.", who, stage),
				})
			}
		case "scope_amendment_requested":
			// must_page_human (ADR-040, spec.PageEventScopeAmendment): a
			// scope-amendment request always parks for an operator decision
			// and is an internal audit kind with no other issue-comment
			// surface, so it is silent on anchor edits without a ping.
			out = append(out, pageEvent{
				sequence: e.Sequence,
				kind:     "scope_amendment",
				message:  "🔔 An agent requested a scope amendment — your decision is needed.",
			})
		case "clarification_requested":
			// must_page_human (ADR-040, spec.PageEventClarificationRequest):
			// the planner parked the plan stage at awaiting_input with a
			// clarification_request because the issue was not yet plannable
			// (#1057). The park always waits on an operator decision and has no
			// other issue-comment surface — the anchor edit alone is silent —
			// so it gets a ping. The question count comes from the parked
			// document, which rides in this entry's payload.
			out = append(out, pageEvent{
				sequence: e.Sequence,
				kind:     "clarification_request",
				message: fmt.Sprintf("❓ The planner parked this issue for direction — %s your answer before planning resumes.",
					clarificationQuestionPhrase(clarificationQuestionCount(e.Payload))),
			})
		case "campaign_gate_paged":
			// must_page_human hand-off (E25.7, server.CategoryCampaignGatePaged):
			// the campaign auto-driver REFUSED a gate a human must own
			// (reviewer_reject / requirement_arbitration) and paused the item —
			// see backend/internal/campaigndriver. The run-chained entry the
			// auto-driver wrote is otherwise silent on anchor edits, so it gets a
			// page-class ping naming the gate/decision the human must act on.
			out = append(out, pageEvent{
				sequence: e.Sequence,
				kind:     "campaign_gate_paged",
				message: fmt.Sprintf("🛑 The campaign auto-driver paused this issue and needs you: %s.",
					campaignGatePagedPhrase(pagePageEvent(e.Payload))),
			})
		case "crew_message_escalated":
			// E77.6 (#3740, crewmessage.CategoryEscalated): a crew-to-crew
			// disagreement exhausted its reject-and-reply round bound, so the
			// captain must rule before anything the thread argued can bind. This
			// page is TIER-INDEPENDENT: it is derived from the chain here and
			// refused in server.activePageEvent under the server-package
			// PageEventCrewEscalation token, NOT through a `page_human_on` list —
			// so `autonomy: low`, whose expanded page list is EMPTY, still pages.
			// Deliberately NOT routed through campaignGatePagedPhrase: that
			// wording is the campaign auto-driver's hand-off, and this event
			// pages on ANY run, campaign or not.
			out = append(out, pageEvent{
				sequence: e.Sequence,
				kind:     "crew_escalation_exhausted",
				message:  crewEscalationExhaustedMessage(e.Payload),
			})
		case "crew_message_sent":
			// E77.6 (#3740, crewmessage.CategorySent): a crew role SENT an
			// escalation. Only a thread ROOT pages — a threaded reply inside an
			// escalation thread is part of an exchange the captain is already
			// paged about — and only the `escalation` type, so the other four
			// crew-message types (consult, finding, work_request, notice) produce
			// NO event and the one-page-per-crew-message noise class cannot
			// appear. Unlike the exhaustion above this reaches the operator
			// through the pings-only notifyPageClass path ONLY: registering
			// crew_message_sent in activityCategories would put EVERY crew
			// message on the anchor timeline (docs/issue-comment-surfaces.md).
			if crewSentEscalation(e.Payload) {
				out = append(out, pageEvent{
					sequence: e.Sequence,
					kind:     "crew_escalation_sent",
					message:  "🛑 A crew role escalated a disagreement to the captain — your ruling is needed before it binds anything.",
				})
			}
		case "acceptance_reopen_awaiting_operator":
			// #5308 (server.CategoryAcceptanceReopenAwaitingOperator): a fix-up
			// push re-opened a settled acceptance stage and, after the
			// orchestrator advance, no dispatcher will run it, so it waits on an
			// operator dispatch. The server writes the row only on that case
			// (never for an auto_dispatch local run, a dispatched stage or a
			// short-circuit), so the projection needs no further gate. Pings-only:
			// like crew_message_sent above it has no activityCategories entry, so
			// the anchor timeline stays quiet. The kind token differs from the
			// category so each case label in this file is unique; the literal is
			// duplicated here because this package cannot import server.
			out = append(out, pageEvent{
				sequence: e.Sequence,
				kind:     "acceptance_awaiting_operator_dispatch",
				message:  "🔁 A fix-up push re-opened the acceptance stage and no dispatcher will run it — dispatch acceptance against the new head.",
			})
		case "campaign_driver_act_awaiting_operator":
			// #5562 (server.CategoryCampaignDriverActAwaitingOperator): a
			// campaign-driver act (route_fixup, approve or retry) parked a stage
			// at awaiting_host_dispatch on a run no dispatcher will spawn. The
			// server writes the row only for the stage the act itself parked, once
			// per park episode. Pings-only, like acceptance_reopen_awaiting_operator.
			if ev, ok := driverActPageEvent(e, stages); ok {
				out = append(out, ev)
			}
		case "stage_failed_awaiting_operator":
			// #5497 (server.CategoryStageFailedAwaitingOperator): the stall
			// watch found a failed stage with no operator or driver act past its
			// threshold. Pings-only, like acceptance_reopen_awaiting_operator.
			if ev, ok := stallPageEvent(e, pageKindFailedStageStalled, stages); ok {
				out = append(out, ev)
			}
		case "child_dispatch_awaiting_operator":
			// #5497 (server.CategoryChildDispatchAwaitingOperator): the stall
			// watch found a dispatchable decomposition child nobody dispatched.
			if ev, ok := stallPageEvent(e, pageKindChildDispatchStalled, stages); ok {
				out = append(out, ev)
			}
		case "ci_failure_retry_dispatched":
			out = append(out, pageEvent{
				sequence: e.Sequence,
				kind:     "ci_failure",
				message:  "❌ CI failed; Fishhawk dispatched an auto-retry.",
			})
		case "ci_retry_exhausted":
			out = append(out, pageEvent{
				sequence: e.Sequence,
				kind:     "ci_retry_exhausted",
				message:  "❌ CI failed and the auto-retry budget is exhausted — needs a human.",
			})
		case "acceptance_triage_decided":
			// E31.8 (#1536): a failed acceptance verdict was triaged. Ping ONLY
			// for the human-needed dispositions (a paged variant) — the
			// auto-routed fixup_dispatched / retry_dispatched dispositions stay
			// edit-only, since the fixup/retry surfaces already render. Otherwise
			// silent on anchor edits, so a paged disposition gets a ping naming
			// the class + disposition the human must act on.
			if class, disposition, ok := acceptanceTriageNeedsHuman(e.Payload); ok {
				out = append(out, pageEvent{
					sequence: e.Sequence,
					kind:     "acceptance_triage",
					message: fmt.Sprintf("🔎 Acceptance triage — class-%s: %s — your decision is needed.",
						class, disposition),
				})
			}
		}
	}
	// Deterministic, oldest-first ordering regardless of the order the
	// plan-awaiting event was prepended in.
	sort.SliceStable(out, func(i, j int) bool { return out[i].sequence < out[j].sequence })
	return out
}

// PageClassEventKinds returns the KIND tokens pageClassEvents projects out of a
// run's audit chain, ascending by source sequence. It is the read accessor a
// consumer in another package uses to assert that a chain it produced actually
// yields the page it intends — the same role RendersActivity plays for the
// activity registry. It exposes only the stable kind tokens, never the comment
// bodies, so a caller cannot come to depend on the wording.
//
// Added for E77.6 (#3740): the crew-escalation page is written in
// backend/internal/server (the notify sites and PageEventCrewEscalation) and
// projected here, so its end-to-end test needs to cross that package boundary
// without reimplementing the projection.
func PageClassEventKinds(entries []*audit.Entry, stages []*run.Stage) []string {
	events := pageClassEvents(entries, stages)
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.kind)
	}
	return out
}

// UnresolvedPageClassEventKinds returns the KIND tokens firePings would post
// before its per-sequence dedup: the projected events pageEventResolved does
// not suppress. Like PageClassEventKinds it exposes only the kind tokens. The
// run state is not consulted (see UnresolvedPageClassEventKindsForRun).
//
// Added for #5497: the stall-watch writer lives in backend/internal/server, so
// its re-entry tests feed the PERSISTED chain and the CURRENT stages here to
// prove an ended episode's row would not post.
func UnresolvedPageClassEventKinds(entries []*audit.Entry, stages []*run.Stage) []string {
	return UnresolvedPageClassEventKindsForRun(nil, entries, stages)
}

// UnresolvedPageClassEventKindsForRun is UnresolvedPageClassEventKinds with the
// run row, so the run-state arm of pageEventResolved (a cancelled run
// resolves a stall page) applies too. A nil run skips that arm. The parent
// arm is not applied (no parent row is passed).
func UnresolvedPageClassEventKindsForRun(r *run.Run, entries []*audit.Entry, stages []*run.Stage) []string {
	events := pageClassEvents(entries, stages)
	out := make([]string, 0, len(events))
	for _, ev := range events {
		if pageEventResolved(ev, entries, stages, r, nil) {
			continue
		}
		out = append(out, ev.kind)
	}
	return out
}

// stallPageEvent builds the page for a stall-watch row (#5497). It emits an
// event only when the payload decodes to a stage_id that parses as a UUID and
// a non-empty episode_key. The message is built ONLY from server-owned text: a
// fixed lead per kind, the stage TYPE read from the matching live stage (never
// from the payload), the next action only when it is in the closed
// stallNextActions set, and for the child kind the parent run id only when
// next_action_target parses as a UUID (rendered in canonical form). The
// payload's reason, quiet_since and every other string never reach the body.
func stallPageEvent(e *audit.Entry, kind string, stages []*run.Stage) (pageEvent, bool) {
	if len(e.Payload) == 0 {
		return pageEvent{}, false
	}
	var p struct {
		StageID          string `json:"stage_id"`
		EpisodeKey       string `json:"episode_key"`
		NextAction       string `json:"next_action"`
		NextActionTarget string `json:"next_action_target"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return pageEvent{}, false
	}
	stageID, err := uuid.Parse(p.StageID)
	if err != nil || p.EpisodeKey == "" {
		return pageEvent{}, false
	}
	action := ""
	if _, ok := stallNextActions[p.NextAction]; ok {
		action = p.NextAction
	}
	var msg string
	switch kind {
	case pageKindFailedStageStalled:
		lead := "⏳ A failed stage"
		if t := stallStageTypeLabel(stages, stageID); t != "" {
			lead = "⏳ A failed " + t + " stage"
		}
		msg = lead + " has had no operator or driver action past the stall threshold"
		if action != "" {
			msg += " — run `" + action + "` to recover it."
		} else {
			msg += " — open the run for the remedy."
		}
	default:
		msg = "⏳ A decomposition child's implement stage is dispatchable and has not been dispatched past the stall threshold"
		parent, perr := uuid.Parse(p.NextActionTarget)
		switch {
		case action != "" && perr == nil:
			msg += " — run `" + action + "` on parent run " + parent.String() + "."
		case action != "":
			msg += " — run `" + action + "` on its parent run."
		default:
			msg += " — open the run for the remedy."
		}
	}
	return pageEvent{
		sequence:   e.Sequence,
		kind:       kind,
		message:    msg,
		stageID:    stageID,
		episodeKey: p.EpisodeKey,
		nextAction: action,
	}, true
}

// driverActPageEvent builds the #5562 driver-act page. It emits an event only
// when the payload decodes to a stage_id that parses as a UUID and a non-empty
// episode_key, the stallPageEvent posture. The message is built ONLY from
// server-owned text: a fixed lead, the stage TYPE read from the matching live
// stage (never from the payload) and the fixed fishhawk_dispatch_stage remedy.
// The payload's reason, action and every other string never reach the body.
func driverActPageEvent(e *audit.Entry, stages []*run.Stage) (pageEvent, bool) {
	if len(e.Payload) == 0 {
		return pageEvent{}, false
	}
	var p struct {
		StageID    string `json:"stage_id"`
		EpisodeKey string `json:"episode_key"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return pageEvent{}, false
	}
	stageID, err := uuid.Parse(p.StageID)
	if err != nil || p.EpisodeKey == "" {
		return pageEvent{}, false
	}
	label := stallStageTypeLabel(stages, stageID)
	if label == "" {
		label = "next"
	}
	return pageEvent{
		sequence:   e.Sequence,
		kind:       pageKindDriverActAwaitingOperator,
		message:    "🔁 The campaign driver advanced this run, and no dispatcher will run its " + label + " stage — dispatch it with `fishhawk_dispatch_stage`.",
		stageID:    stageID,
		episodeKey: p.EpisodeKey,
	}, true
}

// driverActPageResolved reports whether a #5562 driver-act page is resolved:
// the named stage is absent, has left awaiting_host_dispatch, or is parked in
// a LATER episode than the one the row names, or the run is terminal.
func driverActPageResolved(ev pageEvent, stages []*run.Stage, r *run.Run) bool {
	if r != nil && r.State.IsTerminal() {
		return true
	}
	key, ok := HostDispatchParkEpisode(stageByID(stages, ev.stageID))
	return !ok || key != ev.episodeKey
}

// stallStageTypeLabel returns the closed-set type of the live stage with id,
// or "" when it is absent or not a known type.
func stallStageTypeLabel(stages []*run.Stage, id uuid.UUID) string {
	st := stageByID(stages, id)
	if st == nil {
		return ""
	}
	switch st.Type {
	case run.StageTypePlan, run.StageTypeImplement, run.StageTypeReview,
		run.StageTypeDeploy, run.StageTypeAcceptance:
		return string(st.Type)
	default:
		return ""
	}
}

// stageByID returns the stage with id, or nil.
func stageByID(stages []*run.Stage, id uuid.UUID) *run.Stage {
	for _, st := range stages {
		if st != nil && st.ID == id {
			return st
		}
	}
	return nil
}

// stallPageResolved is the resolution arm for both stall kinds (#5497). The
// page is resolved when the named stage is absent, when its condition has
// ended (the episode helper returns !ok), when the stage's CURRENT episode
// differs from the row's (a later episode, which the stall watch pages afresh
// only once it is itself past the threshold), or when the run state puts the
// remedy out of reach. Stage stamps are unchanged by TransitionRun, so the run
// state is read separately: a failed-stage page is resolved on a cancelled or
// succeeded run (neither can be retried or revived), and a child-dispatch
// page on any terminal run (the child can no longer be dispatched). A nil run
// skips the run-state arm.
//
// The parent arm reads what the writer reads: the server withholds a
// fishhawk_resume_run page, and every child-dispatch page, under a terminal
// decomposition parent, so a row written while the parent was live resolves
// once the parent is terminal (the child's own stamps and state unchanged). A
// nil parent skips that arm; stallParentMemo loads it only when this arm
// needs it. Supersession is not re-read here (stallwatch README § Residuals).
// Episode identity only moves forward and a terminal parent stays terminal,
// so a resolved row stays resolved.
func stallPageResolved(ev pageEvent, stages []*run.Stage, r, parent *run.Run) bool {
	st := stageByID(stages, ev.stageID)
	if st == nil {
		return true
	}
	if stallResolutionReadsParent(ev) && parent != nil && parent.State.IsTerminal() {
		return true
	}
	var key string
	var ok bool
	switch ev.kind {
	case pageKindFailedStageStalled:
		key, ok = FailedStageStallEpisode(st)
		if r != nil && (r.State == run.StateCancelled || r.State == run.StateSucceeded) {
			return true
		}
	default:
		key, ok = ChildDispatchStallEpisode(st)
		if r != nil && r.State.IsTerminal() {
			return true
		}
	}
	return !ok || key != ev.episodeKey
}

// stallResolutionReadsParent reports whether a stall page's resolution reads
// the decomposition parent's state: a child-dispatch page (its remedy runs on
// the parent) and a fishhawk_resume_run page (resume re-drives the child
// under its live parent). It mirrors the server writer's parent checks.
func stallResolutionReadsParent(ev pageEvent) bool {
	return ev.kind == pageKindChildDispatchStalled ||
		(ev.kind == pageKindFailedStageStalled && ev.nextAction == "fishhawk_resume_run")
}

// stallParentMemo loads a decomposition child's parent row at most once per
// fire, and only when an unresolved stall page's resolution reads it
// (stallResolutionReadsParent). A non-child run, or an event that does not
// read the parent, returns nil with no read.
type stallParentMemo struct {
	runs   run.Repository
	r      *run.Run
	loaded bool
	parent *run.Run
}

func (m *stallParentMemo) get(ctx context.Context, ev pageEvent) (*run.Run, error) {
	if m.loaded || m.runs == nil || m.r == nil || m.r.DecomposedFrom == nil || !stallResolutionReadsParent(ev) {
		return m.parent, nil
	}
	p, err := m.runs.GetRun(ctx, *m.r.DecomposedFrom)
	if err != nil {
		return nil, err
	}
	m.parent, m.loaded = p, true
	return p, nil
}

// pageEventResolved reports whether a resolvable page event (a reviewer
// reject, the #5308 acceptance dispatch page, a #5497 stall page or the #5562
// driver-act page) has
// already been resolved by a LATER audit entry or, for a stall page, by the
// end of its episode, in which case firePings
// records the dedup row but SKIPS the actual comment: the page would arrive
// stale (the operator has already acted on the concern). This matters for
// the new immediate-dispatch path (#1786) — under the old batched path the
// ping rode the NEXT transition, which could be the very arbitration that
// resolved it, whereas an immediate ping could still fire moments before
// the resolving entry lands on a later re-render.
//
// Only the reviewer-reject kinds, the #5308 acceptance dispatch page, the
// two #5497 stall kinds and the #5562 driver-act page are resolvable; every other page class (a must_page_human park, a CI failure, an
// acceptance triage page, either crew escalation kind) always pages and returns
// false.
//
//   - plan_review_rejected — resolved by a later approval_submitted entry
//     (the operator arbitrated the plan gate).
//   - implement_review_rejected — resolved by a later stage_fixup_triggered
//     (the operator routed the concern back to the agent) OR a later
//     approval_submitted entry.
//   - acceptance_awaiting_operator_dispatch (#5308) — resolved by a later
//     acceptance_dispatched (the operator dispatched acceptance) OR a later
//     acceptance_outcome_recorded (a verdict or short-circuit landed), so a
//     late ping cannot announce a wait that has already ended.
//   - failed_stage_stalled and child_dispatch_stalled (#5497) — resolved by
//     stallPageResolved over the CURRENT stages, run row and parent row: the
//     episode the row names has ended, or the run can no longer take the
//     named remedy.
//   - driver_act_awaiting_operator_dispatch (#5562) — resolved by
//     driverActPageResolved over the CURRENT stages and run row: the named
//     stage left awaiting_host_dispatch (it was dispatched or is absent), is
//     parked in a later episode, or the run is terminal.
//
// stages and r are the caller's already-loaded stage list and run row (both
// firePings and PushChannel.firePushes have them); parent is the
// decomposition parent from stallParentMemo, nil when not read. Only the
// stall and driver-act arms read them; a nil r skips their run-state checks
// and a nil parent the stall parent check.
func pageEventResolved(ev pageEvent, entries []*audit.Entry, stages []*run.Stage, r, parent *run.Run) bool {
	switch ev.kind {
	case pageKindFailedStageStalled, pageKindChildDispatchStalled:
		return stallPageResolved(ev, stages, r, parent)
	case "plan_review_rejected":
		return hasLaterCategory(entries, ev.sequence, "approval_submitted")
	case "implement_review_rejected":
		return hasLaterCategory(entries, ev.sequence, "stage_fixup_triggered") ||
			hasLaterCategory(entries, ev.sequence, "approval_submitted")
	case "acceptance_awaiting_operator_dispatch":
		return hasLaterCategory(entries, ev.sequence, "acceptance_dispatched") ||
			hasLaterCategory(entries, ev.sequence, "acceptance_outcome_recorded")
	case pageKindDriverActAwaitingOperator:
		return driverActPageResolved(ev, stages, r)
	default:
		return false
	}
}

// hasLaterCategory reports whether any entry in the named category has an
// audit Sequence strictly greater than afterSeq.
func hasLaterCategory(entries []*audit.Entry, afterSeq int64, category string) bool {
	for _, e := range entries {
		if e.Sequence > afterSeq && e.Category == category {
			return true
		}
	}
	return false
}

// planStageAwaitingApproval reports whether a plan stage is currently
// parked at an approval gate — the signal that a human approval is
// genuinely pending (a gateless plan stage never enters this state).
func planStageAwaitingApproval(stages []*run.Stage) bool {
	for _, s := range stages {
		if s.Type == run.StageTypePlan && s.State == run.StageStateAwaitingApproval {
			return true
		}
	}
	return false
}

// latestSequenceForCategory returns the highest audit Sequence among
// entries in the named category, or 0 when none exist.
func latestSequenceForCategory(entries []*audit.Entry, category string) int64 {
	var seq int64
	for _, e := range entries {
		if e.Category == category && e.Sequence > seq {
			seq = e.Sequence
		}
	}
	return seq
}

// verdictOf reads the `verdict` field from a *_reviewed audit payload.
func verdictOf(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var p struct {
		Verdict string `json:"verdict"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return p.Verdict
}

// decodeReviewerVerdict reads the `verdict` and `reviewer_model` fields
// from a *_reviewed audit payload — the same shape decodeAnchorVerdict
// reads — so the reviewer-reject ping can name the model that flagged the
// concern. Empty strings when absent or unparseable.
func decodeReviewerVerdict(payload []byte) (verdict, model string) {
	if len(payload) == 0 {
		return "", ""
	}
	var p struct {
		Verdict       string `json:"verdict"`
		ReviewerModel string `json:"reviewer_model"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", ""
	}
	return p.Verdict, p.ReviewerModel
}

// clarificationQuestionCount reads how many questions the planner parked
// from a clarification_requested audit payload — the full
// clarification_request document rides under the `clarification_request`
// key (server/plan.go). Returns 0 when absent or unparseable; the phrase
// helper renders that as a non-numeric fallback so a malformed payload
// never produces "0 questions".
func clarificationQuestionCount(payload []byte) int {
	if len(payload) == 0 {
		return 0
	}
	var p struct {
		ClarificationRequest struct {
			Questions []json.RawMessage `json:"questions"`
		} `json:"clarification_request"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return 0
	}
	return len(p.ClarificationRequest.Questions)
}

// clarificationQuestionPhrase renders the count-aware noun+verb fragment
// for the clarification ping ("1 question needs", "3 questions need").
// A zero/unknown count degrades to a count-free phrase rather than a
// misleading "0 questions".
func clarificationQuestionPhrase(n int) string {
	switch {
	case n == 1:
		return "1 question needs"
	case n > 1:
		return fmt.Sprintf("%d questions need", n)
	default:
		return "your parked questions need"
	}
}

// pagePageEvent reads the `page_event` field from a campaign_gate_paged audit
// payload (server.emitCampaignGatePaged writes it) — the must_page_human event
// the auto-driver refused (e.g. "reviewer_reject"). Empty when absent or
// unparseable; campaignGatePagedPhrase degrades to a generic phrase.
func pagePageEvent(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var p struct {
		PageEvent string `json:"page_event"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return p.PageEvent
}

// campaignGatePagedPhrase renders the human-readable gate/decision fragment for
// the campaign-paused ping from the raw page_event token. A known token gets a
// specific phrase; anything else (including an empty/unparseable payload)
// degrades to a generic "a gate decision" rather than a bare token or "”".
func campaignGatePagedPhrase(pageEvent string) string {
	switch pageEvent {
	case "reviewer_reject", "gating_reviewer_reject":
		return "a reviewer flagged a blocking concern that needs your decision"
	case "requirement_arbitration":
		return "a requirement needs your arbitration"
	case "crew_escalation":
		// server.PageEventCrewEscalation. The token is a SERVER-package constant
		// deliberately outside spec.PageEvent* (E77.6 / #3740), so it can never
		// appear in a page_human_on list; this arm exists so a campaign hand-off
		// on it names the escalation instead of "a gate decision".
		return "a crew disagreement was escalated to the captain and needs your ruling"
	default:
		return "a gate decision"
	}
}

// crewEscalatedCounts reads the thread root, the round bound and the rejection
// count from a crew_message_escalated payload (crewmessage's escalatedPayload).
// ok=false for an absent, unparseable, or non-positive-valued payload so a
// malformed entry degrades to a count-free phrase rather than rendering "0"
// (the clarificationQuestionCount posture).
func crewEscalatedCounts(payload []byte) (root int64, bound, rejections int, ok bool) {
	if len(payload) == 0 {
		return 0, 0, 0, false
	}
	var p struct {
		ThreadRootSequence int64 `json:"thread_root_sequence"`
		RoundBound         int   `json:"round_bound"`
		Rejections         int   `json:"rejections"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return 0, 0, 0, false
	}
	if p.ThreadRootSequence <= 0 || p.RoundBound <= 0 {
		return 0, 0, 0, false
	}
	return p.ThreadRootSequence, p.RoundBound, p.Rejections, true
}

// crewEscalationExhaustedMessage renders the round-bound-exhaustion page body.
// A payload crewEscalatedCounts cannot read degrades to a count-free phrase:
// the disagreement is real whether or not its metadata decodes, so the page
// still fires — it just never names a fabricated "0".
func crewEscalationExhaustedMessage(payload []byte) string {
	root, bound, _, ok := crewEscalatedCounts(payload)
	if !ok {
		return "🛑 A crew disagreement exhausted its reject-and-reply rounds and needs your ruling."
	}
	return fmt.Sprintf("🛑 A crew disagreement on thread %d exhausted its %d reject-and-reply rounds and needs your ruling.", root, bound)
}

// crewSentEscalation reports whether a crew_message_sent payload
// (crewmessage's sentPayload) is an escalation THREAD ROOT: type `escalation`
// AND no thread_root_sequence. A reply inside an escalation thread carries the
// root and returns false, so a reject-and-reply exchange pages once (at its
// root) plus once more on exhaustion, never once per reply. Any other type, and
// any absent/unparseable payload, returns false.
func crewSentEscalation(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	var p struct {
		Message struct {
			Type string `json:"type"`
		} `json:"message"`
		ThreadRootSequence *int64 `json:"thread_root_sequence"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return false
	}
	return p.Message.Type == "escalation" && p.ThreadRootSequence == nil
}

// acceptanceTriageNeedsHuman reads {class, disposition} from an
// acceptance_triage_decided payload (E31.8 / #1536) and reports whether the
// disposition is one that needs a human — the paged variants (paged,
// rerun_budget_exhausted, the *_paged routing-refusal fallbacks, and the
// class-5 externally_unvalidatable_paged terminal page, #1671, and the
// post-deploy rollback_offered disposition, E35.3 / #1600, which pages a human
// to decide on an operator-gated rollback the backend never auto-fires). The
// rollback_offer payload object (its pipeline-supplied rollback_handle
// especially) is deliberately NOT read or rendered here: the ping names only
// the class and disposition, so no pipeline free text reaches the forge thread.
// The auto-routed fixup_dispatched / retry_dispatched dispositions return ok=false
// (they stay edit-only). ok=false on any decode failure or an unrecognized
// disposition, so a malformed payload never fires a page-class ping. This
// package uses string literals, not the server consts — the value is pinned
// byte-for-byte by the ping_test assertion.
func acceptanceTriageNeedsHuman(payload []byte) (class, disposition string, ok bool) {
	if len(payload) == 0 {
		return "", "", false
	}
	var p struct {
		Class       string `json:"class"`
		Disposition string `json:"disposition"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", "", false
	}
	switch p.Disposition {
	case "paged", "rerun_budget_exhausted",
		"fixup_unavailable_paged", "retry_unavailable_paged", "unsettled_paged",
		"externally_unvalidatable_paged", "rollback_offered":
		return p.Class, p.Disposition, true
	default:
		return "", "", false
	}
}

// firePings posts a one-line ping comment for each page-class event in
// the chain that has not already been pinged, and records the ping on
// CategoryAnchorPingPosted. Best-effort: a post failure for one event
// returns a wrapped error but the dedup row for any earlier successful
// ping is already written, so a retry only re-attempts the unpinged tail.
//
// Captain addressing (E76.3 / #3766, ADR-083 rule 6): the captain is resolved
// through captainRes at most ONCE per invocation, lazily at the first comment
// actually posted, and composed at body-assembly time (addressPing) — so the
// dedup key, the source-Sequence gate, the pageEventResolved skip and the
// CategoryAnchorPingPosted payload are all unchanged. A nil captainRes (no
// resolver wired) posts exactly today's body.
func (n *Notifier) firePings(ctx context.Context, ctxv commentContext, entries []*audit.Entry, stages []*run.Stage, runURL string, captainRes *captainMemo) error {
	events := pageClassEvents(entries, stages)
	if len(events) == 0 {
		return nil
	}
	pinged, err := n.pingedSequences(ctx, ctxv.run.ID)
	if err != nil {
		return fmt.Errorf("issuecomment: load pings: %w", err)
	}
	parents := &stallParentMemo{runs: n.runs, r: ctxv.run}
	for _, ev := range events {
		if _, done := pinged[ev.sequence]; done {
			continue
		}
		// A stall page's parent arm (#5497) needs the parent row; a read
		// error posts nothing and records nothing, so the next fire retries.
		parent, err := parents.get(ctx, ev)
		if err != nil {
			return fmt.Errorf("issuecomment: load decomposition parent: %w", err)
		}
		// A reviewer-reject page the operator has already arbitrated is
		// stale: record the dedup row so it never fires later, but skip the
		// comment (#1786). The source-Sequence dedup gate above is unchanged,
		// so a ping already recorded under the old batched path stays deduped.
		if pageEventResolved(ev, entries, stages, ctxv.run, parent) {
			if err := n.appendPingAudit(ctx, ctxv.run.ID, ev); err != nil {
				return err
			}
			continue
		}
		body := addressPing(pingCommentBody(ev.message, runURL), captainRes.get(ctx), ctxv.family)
		// createComment routes by the context's family (E45.52 / #3481): the
		// github family keeps the GitHub client call; a non-GitHub family posts
		// through forge.IssueOperations. Pings are one-shot, so no id is kept.
		if _, err := n.createComment(ctx, ctxv, body); err != nil {
			return fmt.Errorf("issuecomment: create ping comment: %w", err)
		}
		if err := n.appendPingAudit(ctx, ctxv.run.ID, ev); err != nil {
			return err
		}
	}
	return nil
}

// pingCommentBody assembles a page-class ping body. It appends the anchor link
// only when the base URL is configured (runURL non-empty); an unset base URL
// (runURL == "") degrades the ping to the bare message rather than a dead
// operator-host-local link (#1787).
func pingCommentBody(message, runURL string) string {
	if runURL == "" {
		return message
	}
	return fmt.Sprintf("%s [View the run →](%s)", message, runURL)
}

// captainVacantSentence is appended to a page when the captain record read
// fine and the seat is vacant (ADR-083 rule 5): the vacancy is STATED, never
// implied by the absence of an address.
const captainVacantSentence = "_This repository has no captain, so this page is unaddressed._"

// addressPing composes the captain address onto an assembled ping body:
//
//   - nil resolution (no resolver wired) or CaptainBasisUnavailable → body
//     unchanged. An unreadable record must not be asserted as a vacancy.
//   - CaptainBasisVacant → body + " " + captainVacantSentence.
//   - CaptainBasisCaptain → "Captain <address>: " + body, where the address
//     is renderCaptainAddress's mention or code span. A subject that
//     sanitizes to nothing leaves the body unaddressed.
func addressPing(body string, res *CaptainResolution, family string) string {
	if res == nil {
		return body
	}
	switch res.Basis {
	case CaptainBasisVacant:
		return body + " " + captainVacantSentence
	case CaptainBasisCaptain:
		if addr := renderCaptainAddress(res.Subject, res.IdentityVerified, family); addr != "" {
			return "Captain " + addr + ": " + body
		}
	}
	return body
}

// renderCaptainAddress renders a seated captain's subject for a page on a
// run of the given comment family. It emits an ACTIVE forge mention
// ("@<login>") ONLY when all three hold:
//
//  1. identityVerified — the subject is provider-qualified (captain.
//     IdentityVerified); a static token subject such as brett@local-mcp is
//     never mentioned;
//  2. the subject's provider prefix IS the run's comment family ("github:"
//     on the github family, "gitlab:" on the gitlab family) — a gitlab
//     identity is never mentioned on a GitHub thread, where the same login
//     may belong to someone else;
//  3. the remainder passes validApproverLogin (the #751 stop-the-ping
//     filter: no '@', no '.', at most 39 chars).
//
// Every other subject renders inside a backtick code span via
// sanitizeSubjectForCodeSpan, so it can never close the span and never
// becomes a mention. Returns "" for a subject that sanitizes to nothing.
func renderCaptainAddress(subject string, identityVerified bool, family string) string {
	if identityVerified && family != "" {
		if login, ok := strings.CutPrefix(subject, family+":"); ok && validApproverLogin(login) {
			return "@" + login
		}
	}
	if s := sanitizeSubjectForCodeSpan(subject); s != "" {
		return "`" + s + "`"
	}
	return ""
}

// pingedSequences returns the set of source audit sequences already
// pinged for the run, the per-event dedup gate.
func (n *Notifier) pingedSequences(ctx context.Context, runID uuid.UUID) (map[int64]struct{}, error) {
	entries, err := n.audit.ListForRunByCategory(ctx, runID, CategoryAnchorPingPosted)
	if err != nil {
		return nil, err
	}
	seen := make(map[int64]struct{}, len(entries))
	for _, e := range entries {
		if seq := pingSourceSequence(e.Payload); seq > 0 {
			seen[seq] = struct{}{}
		}
	}
	return seen, nil
}

func (n *Notifier) appendPingAudit(ctx context.Context, runID uuid.UUID, ev pageEvent) error {
	systemKind := audit.ActorSystem
	payload, _ := json.Marshal(map[string]any{
		"source_sequence": ev.sequence,
		"event":           ev.kind,
	})
	if _, err := n.audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		Timestamp: n.now().UTC(),
		Category:  CategoryAnchorPingPosted,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		return fmt.Errorf("issuecomment: ping audit append: %w", err)
	}
	return nil
}

func pingSourceSequence(payload []byte) int64 {
	if len(payload) == 0 {
		return 0
	}
	var p struct {
		SourceSequence int64 `json:"source_sequence"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return 0
	}
	return p.SourceSequence
}
