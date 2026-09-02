package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// fixupRecoveryReasonCap bounds the source_failure_reason carried onto the
// wire. A runner error can be arbitrarily long, and this marker rides on
// fishhawk_get_run_status — whose response is tiered against a byte budget
// (bound.go) — so an unbounded reason could push the response down the elision
// ladder and, at the bottom, into the diagnosis skeleton's floor assertions.
// 512 bytes is enough to name what failed and small enough to be invisible to
// the budget.
const fixupRecoveryReasonCap = 512

// fixupRecoverySignal is one decoded stage_fixup_recovered audit entry: its
// audit Sequence plus the payload fields the backend's writeFixupRecoveredAudit
// records (backend/internal/server/fixup.go). Parsed is FALSE when the entry
// exists but its payload could not be decoded — the signal still counts (see
// latestFixupRecovery), it merely carries no details.
type fixupRecoverySignal struct {
	Sequence              int64
	RestoredState         string
	RestoredReviewStageID string
	SourceFailureReason   string
	SourceFailureCategory string
	Parsed                bool
}

// latestFixupRecovery is the PURE firing rule for the #3081 marker, stated once
// here: given the ascending stage_fixup_triggered sequences for a stage and the
// stage's decoded stage_fixup_recovered signals, the marker fires IFF at least
// one recovery is sequenced STRICTLY AFTER the most-recent trigger, and the
// NEWEST such recovery wins.
//
// It returns nil — no marker — in three named cases, and the last two are the
// controls that keep the marker honest:
//
//   - triggerSeqs is empty. No fix-up round exists to scope a recovery against,
//     so there is nothing to report.
//   - there are no recovery signals at all. This is the PRIMARY control: a
//     fix-up that genuinely LANDED writes no stage_fixup_recovered entry, so its
//     wait status stays byte-identical to today.
//   - every recovery signal is sequenced at or before the latest trigger. A
//     LATER fix-up pass superseded the recovery, so the current round's outcome
//     is not a recovery. This is what makes the marker SELF-CLEARING: an
//     operator who retries after a recovered failure and succeeds sees the
//     marker disappear rather than a stale warning about a round that is over.
//
// An UNDECODABLE payload (Parsed false) still FIRES the marker, with
// DetailsAvailable false and empty detail fields. The recovery demonstrably
// happened — the entry exists — so suppressing on a bad payload would restore
// exactly the silence #3081 exists to end.
func latestFixupRecovery(triggerSeqs []int64, recoveries []fixupRecoverySignal) *FixupRecovery {
	if len(triggerSeqs) == 0 {
		return nil
	}
	var latestTrigger int64
	for _, s := range triggerSeqs {
		if s > latestTrigger {
			latestTrigger = s
		}
	}

	var winner *fixupRecoverySignal
	for i := range recoveries {
		if recoveries[i].Sequence <= latestTrigger {
			continue
		}
		if winner == nil || recoveries[i].Sequence > winner.Sequence {
			winner = &recoveries[i]
		}
	}
	if winner == nil {
		return nil
	}

	rec := &FixupRecovery{DetailsAvailable: winner.Parsed}
	if winner.Parsed {
		rec.SourceFailureReason = capJSONString(winner.SourceFailureReason, fixupRecoveryReasonCap)
		rec.SourceFailureCategory = winner.SourceFailureCategory
		rec.RestoredState = winner.RestoredState
		rec.RestoredReviewStageID = winner.RestoredReviewStageID
	}
	rec.Message = fixupRecoveryMessage(rec)
	return rec
}

// fixupRecoveryMessage builds the one-line advisory carried on the marker (and,
// on fishhawk_await_stage, promoted to the response's top-level message so the
// recovery is impossible to miss). Pure, so a table test pins the wording.
//
// It states four things an operator acting on a bare `succeeded` would get
// wrong: the fix-up pass FAILED and pushed no commit; the stage was RESTORED to
// its prior state, which is why the status reads succeeded; the PR head still
// carries the pre-fix-up commit and the routed concerns were NOT addressed; and
// the fix-up BUDGET rule as it stands today — a category-A or category-B
// failure CONSUMES a pass, only a category-C delivered-nothing failure is
// refunded (#1957). The budget sentence is a statement of the CURRENT rule; it
// is not a claim that the rule is wrong or is changing.
func fixupRecoveryMessage(rec *FixupRecovery) string {
	if rec == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("the fix-up pass FAILED and pushed no commit; the stage was restored to its prior state (which is why status reads 'succeeded') — the PR head still carries the pre-fix-up commit and your routed concerns were NOT addressed.")
	switch {
	case !rec.DetailsAvailable:
		b.WriteString(" The recovery audit entry could not be decoded, so no source failure detail is available (details_available=false); read the stage_fixup_recovered entry with fishhawk_list_audit.")
	default:
		if rec.SourceFailureCategory != "" {
			fmt.Fprintf(&b, " Source failure category: %s.", rec.SourceFailureCategory)
		}
		if rec.SourceFailureReason != "" {
			fmt.Fprintf(&b, " Source failure reason: %s.", rec.SourceFailureReason)
		}
	}
	b.WriteString(" Confirm with `git log` on the PR head: the fix-up commit is absent.")
	b.WriteString(" Fix-up budget, as it stands today: a category-A (agent) or category-B (policy) failure CONSUMES a fix-up pass; only a category-C failure that delivered nothing to the PR branch is refunded (#1957).")
	return b.String()
}

// fixupRecoveryFor is the best-effort audit probe behind the marker: it reads
// the stage's stage_fixup_triggered and stage_fixup_recovered entries and hands
// both to the pure latestFixupRecovery rule above.
//
// It reuses the EXISTING api-client audit read, the EXISTING
// categoryStageFixupTriggered / categoryStageFixupRecovered constants and the
// EXISTING fixupPassesAndLatestSeq helper (review_action_hint.go) — no new REST
// surface, no new audit category, and the trigger read is the same one the
// review-action hint already performs.
//
// BEST-EFFORT BY CONSTRUCTION, mirroring awaitStagePendingAmendment: ANY list
// error returns nil. This marker is an ADVISORY; a transient audit failure must
// lose it rather than fail an hours-long fishhawk_await_stage or a
// fishhawk_get_run_status snapshot.
//
// The per-entry StageID double-check mirrors every sibling fix-up helper, so a
// recovery recorded against a DIFFERENT stage is never attributed to this one
// even if an audit backend does not filter by stage_id.
func (r *runResolver) fixupRecoveryFor(ctx context.Context, runID, stageID uuid.UUID) *FixupRecovery {
	_, _, triggerSeqs, err := r.fixupPassesAndLatestSeq(ctx, runID, stageID)
	if err != nil {
		return nil
	}
	if len(triggerSeqs) == 0 {
		return nil
	}

	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: categoryStageFixupRecovered,
		StageID:  stageID.String(),
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return nil
	}

	want := stageID.String()
	signals := make([]fixupRecoverySignal, 0, len(entries))
	for _, e := range entries {
		if e.StageID == nil || *e.StageID != want {
			continue
		}
		sig := fixupRecoverySignal{Sequence: e.Sequence}
		// Marshal-then-unmarshal, matching fixupInfraRefunds' idiom: the client
		// decodes Payload as a generic any, so this is the shortest path to a
		// typed read. A failure on either leg leaves Parsed false, which still
		// FIRES the marker (details_available=false) rather than dropping it.
		if raw, merr := json.Marshal(e.Payload); merr == nil {
			var p struct {
				RestoredState         string `json:"restored_state"`
				RestoredReviewStageID string `json:"restored_review_stage_id"`
				SourceFailureReason   string `json:"source_failure_reason"`
				SourceFailureCategory string `json:"source_failure_category"`
			}
			if json.Unmarshal(raw, &p) == nil {
				sig.RestoredState = p.RestoredState
				sig.RestoredReviewStageID = p.RestoredReviewStageID
				sig.SourceFailureReason = p.SourceFailureReason
				sig.SourceFailureCategory = p.SourceFailureCategory
				sig.Parsed = true
			}
		}
		signals = append(signals, sig)
	}
	return latestFixupRecovery(triggerSeqs, signals)
}
