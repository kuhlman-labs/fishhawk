package issuecomment

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
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
//   - a plan is ready and the gate is awaiting human approval
//   - a reviewer rejected (plan or implement)
//   - CI failed (#1045)
//
// Dedup is per source audit event: each ping records the originating
// entry's audit Sequence on CategoryAnchorPingPosted, so a re-render of
// the anchor (which re-reads the whole chain) never double-pings the same
// event. must_page_human (ADR-040) paging is a request-time delegation
// computation rather than a standalone audit category, so it is not
// derivable from a pure chain projection here; it is tracked as a
// follow-up extension point on pageClassEvents.

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
// chain, oldest-first by sequence. Pure + dedup-friendly: each event is
// keyed by its source Sequence so the caller can skip ones already
// pinged. Categories that are NOT page-class (status edits, plan scope
// prechecks, cost rollups, etc.) produce no event.
func pageClassEvents(entries []*audit.Entry) []pageEvent {
	var out []pageEvent
	for _, e := range entries {
		switch e.Category {
		case "plan_generated":
			out = append(out, pageEvent{
				sequence: e.Sequence,
				kind:     "plan_awaiting_approval",
				message:  "📋 A plan is ready and awaiting your review.",
			})
		case "plan_reviewed", "implement_reviewed":
			if verdictOf(e.Payload) == "reject" {
				stage := strings.TrimSuffix(e.Category, "_reviewed")
				out = append(out, pageEvent{
					sequence: e.Sequence,
					kind:     stage + "_review_rejected",
					message:  fmt.Sprintf("🚫 A reviewer rejected the %s.", stage),
				})
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
		}
	}
	return out
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
func (n *Notifier) firePings(ctx context.Context, ctxv commentContext, entries []*audit.Entry, runURL string) error {
	events := pageClassEvents(entries)
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
