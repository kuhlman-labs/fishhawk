package issuecomment

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// KindPagePing tags a one-line page-class ping comment (#1054). GitHub
// does not notify subscribers when an existing comment is *edited*, only
// when a new one is created — so the living anchor comment, which is
// edited in place on every transition, is silent to watchers. The four
// page-class moments (a gate parking for human approval, a reviewer
// reject landing, an ADR-040 must_page_human event, and a CI-failure
// auto-retry) each post a NEW one-line comment that links back to the
// anchor, so the operator's notification stream still surfaces the
// moments that actually need them.
//
// Dedup is keyed on (stage id, event) carried in the payload — a webhook
// redelivery or a projection retry for the same moment finds the existing
// row and skips, so a moment pages at most once. Lives on
// CategoryIssueCommented alongside the other one-shot notifications.
const KindPagePing Kind = "page_ping"

// Page-event identifiers. These are the per-moment keys the page_ping
// dedup scopes on (together with the stage id). Stable strings: changing
// one re-pages every in-flight run for that moment, so treat them as
// part of the audit contract.
const (
	// PageEventGateAwaitingApproval fires when a stage parks at
	// awaiting_approval behind a human approval gate.
	PageEventGateAwaitingApproval = "gate_awaiting_approval"
	// PageEventReviewerReject fires when a reviewer returns a reject
	// verdict on a plan or implement review.
	PageEventReviewerReject = "reviewer_reject"
	// PageEventMustPageHuman fires when the run's effective ADR-040
	// delegation block lists the current event in must_page_human, so a
	// human must be paged even though an operator agent could otherwise
	// act. Where this names the same moment as a reviewer reject the
	// single dedup row absorbs both.
	PageEventMustPageHuman = "must_page_human"
	// PageEventCIFailure fires when the dispatcher auto-retries a
	// CI-failed run. Reframes the legacy ci_retry comment with an anchor
	// link; the dispatcher keeps its own per-attempt dedup, so this event
	// is excluded from NotifyPagePing's stage+event dedup path.
	PageEventCIFailure = "ci_failure"
)

// PagePing is the input to NotifyPagePing. Summary is the one-line,
// human-facing description of the moment ("Plan awaiting your approval",
// "Reviewer rejected the implementation"); Event is one of the
// PageEvent* identifiers and, with StageID, forms the dedup key. A nil
// StageID scopes the dedup at the run level (events with no stage).
type PagePing struct {
	Event   string
	StageID *uuid.UUID
	Summary string
}

// NotifyPagePing posts the one-line page-class ping comment linking back
// to the run's anchor comment (#1054), then records a page_ping audit row
// so the (stage id, event) moment pages at most once.
//
// Best-effort throughout, mirroring the other notifier surfaces:
//   - Nil receiver / empty event or summary / non-issue-trigger / missing
//     run coordinates return nil; the caller doesn't branch.
//   - A page_ping row already recording this (stage id, event) suppresses
//     the post (redelivery / retry idempotency).
//   - GitHub create failures return a wrapped error the caller logs; they
//     do not unwind the underlying transition.
//   - An audit-append failure after a successful post returns the wrapped
//     error; a re-fire would re-dedup off the (now-absent) row and could
//     double-post, accepted as strictly better than silently dropping the
//     receipt.
func (n *Notifier) NotifyPagePing(ctx context.Context, runID uuid.UUID, p PagePing) error {
	if n == nil {
		return nil
	}
	if p.Event == "" || strings.TrimSpace(p.Summary) == "" {
		return nil
	}
	ctxv, ok, err := n.contextForStatus(ctx, runID)
	if err != nil || !ok {
		return err
	}
	already, err := n.alreadyPagedPing(ctx, runID, p)
	if err != nil {
		return fmt.Errorf("issuecomment: page-ping dedup check: %w", err)
	}
	if already {
		return nil
	}
	anchorID, err := n.findStatusCommentID(ctx, runID)
	if err != nil {
		return fmt.Errorf("issuecomment: lookup anchor comment: %w", err)
	}
	body := renderPagePingBody(p.Summary, ctxv, anchorID)
	if _, err := n.github.CreateIssueComment(ctx, *ctxv.run.InstallationID,
		ctxv.repo, ctxv.issueNumber, body); err != nil {
		return fmt.Errorf("issuecomment: create page-ping: %w", err)
	}
	return n.appendPagePingAudit(ctx, ctxv, p)
}

// alreadyPagedPing returns true when a page_ping audit row on this run
// already records the same (stage id, event). Different from alreadyPosted
// (kind alone) because a run legitimately pages multiple distinct moments;
// only a repeat of the SAME moment is suppressed.
func (n *Notifier) alreadyPagedPing(ctx context.Context, runID uuid.UUID, p PagePing) (bool, error) {
	entries, err := n.audit.ListForRunByCategory(ctx, runID, CategoryIssueCommented)
	if err != nil {
		return false, err
	}
	want := pingDedupStage(p.StageID)
	for _, e := range entries {
		if extractKind(e.Payload) != KindPagePing {
			continue
		}
		ev, st := extractPingEventStage(e.Payload)
		if ev == p.Event && st == want {
			return true, nil
		}
	}
	return false, nil
}

// appendPagePingAudit records that the run paged for this (stage id,
// event). Stamps the dedup key into the payload so alreadyPagedPing can
// scope per-moment.
func (n *Notifier) appendPagePingAudit(ctx context.Context, ctxv commentContext, p PagePing) error {
	systemKind := audit.ActorSystem
	payload, _ := json.Marshal(map[string]any{
		"kind":         string(KindPagePing),
		"issue_number": ctxv.issueNumber,
		"repo":         ctxv.repo.String(),
		"event":        p.Event,
		"stage_id":     pingDedupStage(p.StageID),
	})
	var stageID *uuid.UUID
	if p.StageID != nil {
		stageID = p.StageID
	}
	if _, err := n.audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     ctxv.run.ID,
		StageID:   stageID,
		Timestamp: n.now().UTC(),
		Category:  CategoryIssueCommented,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		return fmt.Errorf("issuecomment: page-ping audit append: %w", err)
	}
	return nil
}

// pingDedupStage renders the stage half of the dedup key — the stage UUID
// string, or the "run" sentinel for run-level events. A stable, non-UUID
// sentinel keeps run-level moments from ever colliding with a real stage.
func pingDedupStage(stageID *uuid.UUID) string {
	if stageID == nil {
		return "run"
	}
	return stageID.String()
}

// extractPingEventStage reads the (event, stage_id) dedup key out of a
// page_ping payload. Returns empty strings on any decode failure or
// absent field; the caller treats those as a non-match.
func extractPingEventStage(payload []byte) (event, stage string) {
	if len(payload) == 0 {
		return "", ""
	}
	var p struct {
		Event   string `json:"event"`
		StageID string `json:"stage_id"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", ""
	}
	return p.Event, p.StageID
}

// renderPagePingBody renders the one-line ping. The link points at the
// run's anchor comment when its id is known — built as the GitHub
// issue-comment permalink from the repo + issue number — falling back to
// the Fishhawk run page when no anchor exists yet (a page event that
// races ahead of the first status projection). Kept to a single line so
// the notification preview reads cleanly.
func renderPagePingBody(summary string, c commentContext, anchorCommentID int64) string {
	link := anchorCommentURL(c, anchorCommentID)
	if link == "" {
		link = c.runURL
	}
	return fmt.Sprintf("👋 %s — see the [run status](%s).\n", strings.TrimSpace(summary), link)
}

// anchorCommentURL builds the GitHub permalink to the anchor issue
// comment, or "" when the comment id is unknown. GitHub renders the
// anchor on the same issue thread the ping posts to, so the permalink
// scrolls the reader straight to it. Form:
// https://github.com/<owner>/<name>/issues/<n>#issuecomment-<id>.
func anchorCommentURL(c commentContext, anchorCommentID int64) string {
	if anchorCommentID <= 0 {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/issues/%d#issuecomment-%d",
		c.repo.String(), c.issueNumber, anchorCommentID)
}
