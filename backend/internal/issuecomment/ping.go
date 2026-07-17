package issuecomment

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
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
				who := model
				if who == "" {
					who = "A reviewer"
				}
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

// pageEventResolved reports whether a reviewer-reject page event has
// already been resolved by a LATER audit entry, in which case firePings
// records the dedup row but SKIPS the actual comment: the page would arrive
// stale (the operator has already acted on the concern). This matters for
// the new immediate-dispatch path (#1786) — under the old batched path the
// ping rode the NEXT transition, which could be the very arbitration that
// resolved it, whereas an immediate ping could still fire moments before
// the resolving entry lands on a later re-render.
//
// Only reviewer-reject kinds are resolvable; every other page class (a
// must_page_human park, a CI failure, an acceptance triage page) always
// pages and returns false.
//
//   - plan_review_rejected — resolved by a later approval_submitted entry
//     (the operator arbitrated the plan gate).
//   - implement_review_rejected — resolved by a later stage_fixup_triggered
//     (the operator routed the concern back to the agent) OR a later
//     approval_submitted entry.
func pageEventResolved(ev pageEvent, entries []*audit.Entry) bool {
	switch ev.kind {
	case "plan_review_rejected":
		return hasLaterCategory(entries, ev.sequence, "approval_submitted")
	case "implement_review_rejected":
		return hasLaterCategory(entries, ev.sequence, "stage_fixup_triggered") ||
			hasLaterCategory(entries, ev.sequence, "approval_submitted")
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
	default:
		return "a gate decision"
	}
}

// acceptanceTriageNeedsHuman reads {class, disposition} from an
// acceptance_triage_decided payload (E31.8 / #1536) and reports whether the
// disposition is one that needs a human — the paged variants (paged,
// rerun_budget_exhausted, the *_paged routing-refusal fallbacks, and the
// class-5 externally_unvalidatable_paged terminal page, #1671). The
// auto-routed fixup_dispatched / retry_dispatched dispositions return ok=false
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
		"externally_unvalidatable_paged":
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
func (n *Notifier) firePings(ctx context.Context, ctxv commentContext, entries []*audit.Entry, stages []*run.Stage, runURL string) error {
	events := pageClassEvents(entries, stages)
	if len(events) == 0 {
		return nil
	}
	pinged, err := n.pingedSequences(ctx, ctxv.run.ID)
	if err != nil {
		return fmt.Errorf("issuecomment: load pings: %w", err)
	}
	for _, ev := range events {
		if _, done := pinged[ev.sequence]; done {
			continue
		}
		// A reviewer-reject page the operator has already arbitrated is
		// stale: record the dedup row so it never fires later, but skip the
		// comment (#1786). The source-Sequence dedup gate above is unchanged,
		// so a ping already recorded under the old batched path stays deduped.
		if pageEventResolved(ev, entries) {
			if err := n.appendPingAudit(ctx, ctxv.run.ID, ev); err != nil {
				return err
			}
			continue
		}
		body := pingCommentBody(ev.message, runURL)
		if _, err := n.github.CreateIssueCommentScoped(ctx, forge.FromGitHubInstallationID(*ctxv.run.InstallationID), ctxv.repo, ctxv.issueNumber, body); err != nil {
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
