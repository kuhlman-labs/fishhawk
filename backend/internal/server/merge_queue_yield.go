package server

// ADR-112 R3 (#4632, slice 2): the merge queue's YIELD. A non-approved
// forge-approval fold at the head of the line no longer drops or ejects an
// entry carrying the operator's merge intent: it WAITS at the head for a
// bounded window (Config.MergeQueueApprovalWait), then yields; vetoed and
// provider_unsupported yield immediately ("On window expiry (including for
// REVIEW_REQUIRED, which follows row 3: wait, then yield), or immediately on
// vetoed or provider_unsupported, the entry YIELDS."). A yielded entry is held
// with a marker AdmitNext skips; clearing the marker is the only way back in.
//
// This file is the yield's closed vocabulary (yield_reason and its TOTAL
// next_step mapping), the fold → yield mapping, the sanitised yield detail and
// the merge_queue_yielded writer. The pump applies a wait or a yield outside
// the merge-intent lock (applyMergeOnPassOutcome); the re-probe that clears a
// marker is merge_queue_yield_reprobe.go. Contract:
// backend/internal/server/README.md and backend/internal/mergequeue/README.md
// § "Yield".

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
)

// CategoryMergeQueueYielded records an entry yielding its place (and a
// re-probe's reason transition). INTERNAL: not an issue-comment surface.
// CategoryMergeQueueYieldPosted (operator-visible, once per episode) and
// CategoryMergeQueueYieldCleared (INTERNAL, the resolution) are written by the
// re-probe (merge_queue_yield_reprobe.go). All three are registered in
// audit/categories.go.
const (
	CategoryMergeQueueYielded      = "merge_queue_yielded"
	CategoryMergeQueueYieldPosted  = "merge_queue_yield_posted"
	CategoryMergeQueueYieldCleared = "merge_queue_yield_cleared"
)

// DefaultMergeQueueApprovalWait is the head-of-line approval wait window when
// Config.MergeQueueApprovalWait is zero (ADR-112 R3: default 30 minutes).
const DefaultMergeQueueApprovalWait = 30 * time.Minute

// mergeQueueApprovalWait is the configured approval-wait window: the zero
// value is the default; fishhawkd refuses a non-positive flag at startup.
func (s *Server) mergeQueueApprovalWait() time.Duration {
	if s.cfg.MergeQueueApprovalWait > 0 {
		return s.cfg.MergeQueueApprovalWait
	}
	return DefaultMergeQueueApprovalWait
}

// The yield_reason vocabulary (closed). The forge_approval_* reasons were
// #4619's interim drop/eject reasons; they now name a yield.
// mergeCandidateQueueReasonForgeApprovalVetoed is also the intent-bearing
// admission's R2 veto reason code (merge_queue_eligibility.go).
const (
	mergeCandidateQueueReasonForgeApprovalVetoed              = "forge_approval_vetoed"
	mergeCandidateQueueReasonForgeApprovalReviewRequired      = "forge_approval_review_required"
	mergeCandidateQueueReasonForgeApprovalDismissed           = "forge_approval_dismissed"
	mergeCandidateQueueReasonForgeApprovalAbsent              = "forge_approval_absent"
	mergeCandidateQueueReasonForgeApprovalUndetermined        = "forge_approval_undetermined"
	mergeCandidateQueueReasonForgeApprovalReviewsTruncated    = "forge_approval_reviews_truncated"
	mergeCandidateQueueReasonForgeApprovalProviderUnsupported = "forge_approval_provider_unsupported"
	// A member_of gate with no github identity provider (keyed on the fold's
	// REASON, not its provider_unsupported result) so its next step,
	// configure_identity_provider, is derivable from the reason alone.
	mergeCandidateQueueReasonForgeApprovalIdentityProviderUnconfigured = "forge_approval_identity_provider_unconfigured"
	// mergeQueueYieldForgeReviewRequired is R3 precedence row 4: the forge
	// refused the pinned merge (a typed RELEASE outcome) and a fresh snapshot
	// reads REVIEW_REQUIRED, CHANGES_REQUESTED or mergeStateStatus BLOCKED.
	mergeQueueYieldForgeReviewRequired = "forge_review_required"
)

// mergeQueueYieldNextNone is the next_step of a yield nothing the operator
// does at the forge clears (provider_unsupported, or an unknown reason).
const mergeQueueYieldNextNone = "none"

// mergeQueueYieldNextStep is the TOTAL mapping yield_reason → next_step
// (approve_pr | dismiss_review | retry_after | configure_identity_provider |
// none) and the ONLY source of a yield's next_step: the merge_queue_yielded
// row records it (newMergeQueueYield), and GET merge-queue and the run payload
// derive it from the stored reason, so the recorded and derived values always
// agree. It agrees with the fold's own NextStep for every fold result EXCEPT
// undetermined/veto_undetermined, whose fold names dismiss_review (the inline
// 409) while its queue yield, forge_approval_undetermined, maps to retry_after
// (TestMergeQueueYieldNextStep_AgreesWithFold).
func mergeQueueYieldNextStep(reason string) string {
	switch reason {
	case mergeCandidateQueueReasonForgeApprovalVetoed:
		return forgeApprovalNextDismissReview
	case mergeCandidateQueueReasonForgeApprovalReviewRequired, mergeCandidateQueueReasonForgeApprovalDismissed,
		mergeCandidateQueueReasonForgeApprovalAbsent, mergeQueueYieldForgeReviewRequired:
		return forgeApprovalNextApprovePR
	case mergeCandidateQueueReasonForgeApprovalUndetermined, mergeCandidateQueueReasonForgeApprovalReviewsTruncated:
		return forgeApprovalNextRetryAfter
	case mergeCandidateQueueReasonForgeApprovalIdentityProviderUnconfigured:
		return forgeApprovalNextConfigureIdentityProvider
	}
	return mergeQueueYieldNextNone
}

// The sites and ADR-112 R3 precedence rows a merge_queue_yielded row names.
const (
	mergeQueueYieldSiteMergeOnPass = "merge_on_pass"
	mergeQueueYieldSiteAdmission   = "admission"
	mergeQueueYieldSiteMergeRun    = "merge_run"

	mergeQueueYieldRowVeto         = 2
	mergeQueueYieldRowApprovalFold = 3
	mergeQueueYieldRowForgeRefusal = 4
)

// mergeQueueYieldForgeRefusalDetail is the row-4 yield's detail format: the
// fresh snapshot's reviewDecision and mergeStateStatus.
const mergeQueueYieldForgeRefusalDetail = "the forge refused the pinned merge and reports the pull request still needs review or is blocked (reviewDecision %s, mergeStateStatus %s)"

// mergeQueueYield is one yield decision: the marker's reason and detail, its
// next step and precedence row.
type mergeQueueYield struct {
	Reason   string
	Detail   string
	NextStep string
	Row      int
}

// marker is the store's yield marker for y.
func (y mergeQueueYield) marker() mergequeue.YieldMarker {
	return mergequeue.YieldMarker{Reason: y.Reason, Detail: y.Detail}
}

// newMergeQueueYield builds a yield for reason: the detail sanitised and
// bounded (mergeQueueYieldDetail) and next_step derived through the total
// mapping.
func newMergeQueueYield(reason, detail string, row int) mergeQueueYield {
	return mergeQueueYield{Reason: reason, Detail: mergeQueueYieldDetail(detail), NextStep: mergeQueueYieldNextStep(reason), Row: row}
}

// mergeQueueYieldDetail is THE sanitiser for yield_detail, which carries
// forge-derived (untrusted) text: tokens redacted, cut at the first line
// break, control characters dropped and bounded to
// forgeApprovalDetailMaxBytes. Every sink (the queue write, the queue read,
// head_of_line, the run payload and the thread line) passes it.
func mergeQueueYieldDetail(detail string) string {
	return sanitizeForgeReadDetail("", detail)
}

// mergeOnPassFoldYield maps a NON-approved fold to its yield and whether it
// is IMMEDIATE (ADR-112 R3): vetoed and provider_unsupported (including a
// member_of gate with no identity provider, keyed on fold.Reason) yield at
// once; review_required, dismissed, absent, reviews_truncated and
// undetermined wait at the head first. next_step is always the mapping's,
// never the fold's, so the recorded row and every read surface agree.
func mergeOnPassFoldYield(fold forgeApprovalDecision) (y mergeQueueYield, immediate bool) {
	reason, row := mergeCandidateQueueReasonForgeApprovalUndetermined, mergeQueueYieldRowApprovalFold
	switch {
	case fold.Result == forgeApprovalVetoed:
		reason, row, immediate = mergeCandidateQueueReasonForgeApprovalVetoed, mergeQueueYieldRowVeto, true
	case fold.Reason == forgeApprovalReasonNoIdentityProvider:
		reason, immediate = mergeCandidateQueueReasonForgeApprovalIdentityProviderUnconfigured, true
	case fold.Result == forgeApprovalProviderUnsupported:
		reason, immediate = mergeCandidateQueueReasonForgeApprovalProviderUnsupported, true
	case fold.Result == forgeApprovalReviewRequired:
		reason = mergeCandidateQueueReasonForgeApprovalReviewRequired
	case fold.Result == forgeApprovalDismissed:
		reason = mergeCandidateQueueReasonForgeApprovalDismissed
	case fold.Result == forgeApprovalAbsent:
		reason = mergeCandidateQueueReasonForgeApprovalAbsent
	case fold.Result == forgeApprovalReviewsTruncated:
		reason = mergeCandidateQueueReasonForgeApprovalReviewsTruncated
	}
	return newMergeQueueYield(reason, fold.Detail, row), immediate
}

// yieldMergeQueueEntry yields the LIVE entry e at its admission epoch (one
// store transaction: held, marker stamped, enqueued_at and the wait anchor
// kept), then appends merge_queue_yielded. A same-reason re-yield continues
// the entry's last cleared episode (mergeQueueYieldContinuation). It reports
// whether the entry yielded: a CAS conflict or a failed write leaves it
// untouched and false. It is called OUTSIDE the merge-intent lock (ADR-112
// R3: never inside it).
func (s *Server) yieldMergeQueueEntry(ctx context.Context, e mergequeue.Entry, y mergeQueueYield, site string) bool {
	m := y.marker()
	m.Continue = s.mergeQueueYieldContinuation(ctx, e, y.Reason)
	yielded, err := s.cfg.MergeQueue.Yield(ctx, e.ID, e.AdmissionEpoch, m)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge queue: the yield did not land; another admission owns the entry or the write failed",
			slog.String("run_id", e.RunID.String()), slog.String("entry_id", e.ID.String()),
			slog.String("yield_reason", y.Reason), slog.Int64("admission_epoch", e.AdmissionEpoch),
			slog.String("error", err.Error()))
		return false
	}
	_ = s.writeMergeQueueYielded(ctx, yielded, y, site, "")
	return true
}

// mergeQueueYieldContinuation is the episode a re-yield of e for reason
// continues (ADR-112 R3, #4632): the entry's LAST merge_queue_yield_cleared
// row, when it names e and the same reason. The store keeps that row's
// yielded_at only when e's anchor (after the write) is still the head and
// base it was cleared at (mergequeue.YieldMarker.Continue), so a re-yield for
// the same reason on the same head and base stays in one episode: the
// re-probe backoff keeps growing and the episode is posted once, while a moved
// head or base is a new episode. No such row (none cleared, an unreadable
// audit chain, a failed cleared append) is nil: a fresh episode. Residual: a
// continued episode reuses (anchored head, yielded_at), the marker's CAS key,
// so a re-probe that read the PRIOR marker and races the re-yield may still
// clear it; that costs one re-attempt, which re-yields in the same episode.
func (s *Server) mergeQueueYieldContinuation(ctx context.Context, e mergequeue.Entry, reason string) *mergequeue.YieldEpisode {
	if s.cfg.AuditRepo == nil {
		return nil
	}
	rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, e.RunID, CategoryMergeQueueYieldCleared)
	if err != nil {
		return nil
	}
	var last *mergequeue.YieldEpisode
	var lastReason string
	for _, r := range rows {
		var p struct {
			EntryID     string `json:"entry_id"`
			HeadSHA     string `json:"head_sha"`
			BaseSHA     string `json:"base_sha"`
			YieldReason string `json:"yield_reason"`
			YieldedAt   string `json:"yielded_at"`
		}
		if json.Unmarshal(r.Payload, &p) != nil || p.EntryID != e.ID.String() {
			continue
		}
		at, perr := time.Parse(time.RFC3339Nano, p.YieldedAt)
		if perr != nil {
			last, lastReason = nil, ""
			continue
		}
		last, lastReason = &mergequeue.YieldEpisode{YieldedAt: at, HeadSHA: p.HeadSHA, BaseSHA: p.BaseSHA}, p.YieldReason
	}
	if last == nil || lastReason != reason {
		return nil
	}
	return last
}

// writeMergeQueueYielded appends the chained merge_queue_yielded row for the
// yielded entry e (e.YieldedAt is the episode). transitionFrom is the prior
// reason on a re-probe transition ("" for a fresh yield). The Yield / ClearYield
// CAS has already committed, so a failed append is logged at ERROR naming
// entry_id and yielded_at and the row is never rolled back; the re-probe
// sweep re-appends a missing row for a still-marked episode (approval
// condition C7). The error is returned for that sweep.
func (s *Server) writeMergeQueueYielded(ctx context.Context, e mergequeue.Entry, y mergeQueueYield, site, transitionFrom string) error {
	if s.cfg.AuditRepo == nil {
		return nil
	}
	yieldedAt := ""
	if e.YieldedAt != nil {
		yieldedAt = e.YieldedAt.UTC().Format(time.RFC3339Nano)
	}
	fields := map[string]any{
		"entry_id":       e.ID.String(),
		"base_ref":       e.BaseRef,
		"head_sha":       e.AnchoredHeadSHA,
		"yield_reason":   y.Reason,
		"yield_detail":   y.Detail,
		"next_step":      y.NextStep,
		"site":           site,
		"precedence_row": y.Row,
		"yielded_at":     yieldedAt,
	}
	if transitionFrom != "" {
		fields["transition_from"] = transitionFrom
	}
	payload, _ := json.Marshal(fields)
	kind, subject := audit.ActorSystem, mergeCandidateSystemSubject
	_, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        e.RunID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryMergeQueueYielded,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
			"merge queue: the entry yielded but merge_queue_yielded could not be recorded; the marker stands and the re-probe sweep re-appends it",
			slog.String("run_id", e.RunID.String()), slog.String("entry_id", e.ID.String()),
			slog.String("yielded_at", yieldedAt), slog.String("yield_reason", y.Reason),
			slog.String("error", err.Error()))
	}
	return err
}
