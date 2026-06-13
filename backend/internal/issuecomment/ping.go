package issuecomment

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

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
			if verdictOf(e.Payload) == "reject" {
				stage := strings.TrimSuffix(e.Category, "_reviewed")
				out = append(out, pageEvent{
					sequence: e.Sequence,
					kind:     stage + "_review_rejected",
					message:  fmt.Sprintf("🚫 A reviewer rejected the %s.", stage),
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
		}
	}
	// Deterministic, oldest-first ordering regardless of the order the
	// plan-awaiting event was prepended in.
	sort.SliceStable(out, func(i, j int) bool { return out[i].sequence < out[j].sequence })
	return out
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
		body := fmt.Sprintf("%s [View the run →](%s)", ev.message, runURL)
		if _, err := n.github.CreateIssueComment(ctx, *ctxv.run.InstallationID, ctxv.repo, ctxv.issueNumber, body); err != nil {
			return fmt.Errorf("issuecomment: create ping comment: %w", err)
		}
		if err := n.appendPingAudit(ctx, ctxv.run.ID, ev); err != nil {
			return err
		}
	}
	return nil
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
