package mcpserver

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
)

// acceptance_verdict_wait.go closes the settle-before-verdict window on the MCP
// surfaces (E72.56 / #4072). The runner settles an acceptance stage
// `succeeded` with its trace upload and ships the acceptance verdict AFTER
// that (transcript, then POST /acceptance), so for a few seconds a settled
// acceptance stage carries no acceptance_outcome_recorded entry. Read in that
// window, the stage looks exactly like the #1574 / #3447 settled-outcome-unknown
// hole, whose recovery is fishhawk_retry_stage. This file holds the shared
// logic that tells the two apart:
//
//   - fishhawk_await_stage(acceptance) HOLDS a settled-succeeded release until
//     the verdict for the stage's LATEST attempt lands, bounded by the caller's
//     deadline and the in-flight window below (awaitAcceptanceVerdict).
//   - fishhawk_get_run_status / fishhawk_run_stage next_actions classify an
//     acceptance_verdict_pending state inside the window instead of the
//     retry-offering outcome-unknown arm (acceptanceVerdictSignal).
//
// The two surfaces read DIFFERENT audit windows and do not share one. The
// await_stage probe reads auditLimitMax (50) entries; get_run_status reads the
// CALLER's clamped audit_limit (default auditLimitDefault, 5), and run_stage
// reads runStageAuditWindow. So the probe can see a verdict the status
// snapshot's window has already aged out, and the reverse never matters: the
// get_run_status-derived sentinel fires only when NO verdict is visible in that
// snapshot's window and the stage settled inside the in-flight window, so a
// too-narrow window fails toward verdict_pending (wait/read), never toward
// retry or merge.

// acceptanceVerdictInFlightWindow is how long after an acceptance stage's
// ended_at a missing verdict is still read as IN FLIGHT rather than as the
// settled-outcome-unknown hole. The runner ships the verdict after the trace
// upload succeeds, and that ship opens a FRESH terminal-egress phase bounded by
// runner/internal/upload/upload.go DefaultTerminalEgressBudget (90s), so a
// verdict can legitimately land up to ~90s after the stage settles. 120s adds
// margin over that budget. The observed lag in #4072 was ~5s.
const acceptanceVerdictInFlightWindow = 120 * time.Second

// acceptanceAttemptDispositionSkipped names a latest attempt that the
// orchestrator auto-terminated as out of scope (E38.3): no verdict is recorded
// BY DESIGN, so the hold must release at once. The unshipped disposition reuses
// the acceptanceVerdictUnshipped sentinel.
const acceptanceAttemptDispositionSkipped = "skipped_out_of_scope"

// acceptanceAttemptVerdict is what one probe of the audit chain established
// about the LATEST attempt of one acceptance stage.
//
//   - Determined false: the slice was exhausted before any stage-scoped
//     acceptance signal was found (the entries aged out of the window, or the
//     read failed) — the probe cannot tell an in-flight verdict from a missing
//     one.
//   - Determined true with Verdict set: the attempt's verdict is recorded.
//   - Determined true with Disposition set: a named non-verdict outcome — the
//     verdict ship FAILED (acceptanceVerdictUnshipped) or the stage was
//     auto-terminated out of scope (acceptanceAttemptDispositionSkipped).
//   - Determined true with neither: the newest signal is the attempt's own
//     dispatch / reopen ANCHOR, so the latest attempt has not shipped a verdict.
type acceptanceAttemptVerdict struct {
	Determined  bool
	Verdict     string
	Disposition string
}

// acceptanceAttemptVerdictIn walks the audit entries for stageID newest-first
// and returns the first acceptance signal it meets. It copies and sorts by
// Sequence DESCENDING, so it does not depend on the endpoint's ordering, and it
// ignores every entry not scoped to stageID (each writer — the dispatch anchors,
// acceptance_reopened, handleShipAcceptance, the reap-failure marker and the
// orchestrator's skip marker — stage-scopes its entry). An anchor found ABOVE
// any outcome means the latest attempt has not shipped: an older outcome below
// it is a PRIOR attempt's verdict and is deliberately not returned.
func acceptanceAttemptVerdictIn(entries []AuditEntry, stageID string) acceptanceAttemptVerdict {
	sorted := append([]AuditEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Sequence > sorted[j].Sequence })
	for _, e := range sorted {
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		switch e.Category {
		case auditCategoryAcceptanceOutcomeRecorded:
			return acceptanceAttemptVerdict{Determined: true, Verdict: acceptancePayloadString(e.Payload, "verdict")}
		case auditCategoryAcceptanceVerdictUnshipped:
			return acceptanceAttemptVerdict{Determined: true, Disposition: acceptanceVerdictUnshipped}
		case auditCategoryAcceptanceSkippedOutOfScope:
			return acceptanceAttemptVerdict{Determined: true, Disposition: acceptanceAttemptDispositionSkipped}
		case auditCategoryAcceptanceDispatched, categoryAcceptanceReopened:
			return acceptanceAttemptVerdict{Determined: true}
		}
	}
	return acceptanceAttemptVerdict{}
}

// acceptanceVerdictHoldUntil is the instant the in-flight window closes: the
// stage's backend-stamped ended_at plus window, CLAMPED to [now, now+window].
// ended_at is stamped by the backend's clock and now is this process's, so the
// clamp is what bounds cross-clock skew in BOTH directions (the AGENTS.md #3048
// class): a backend clock ahead of this host can never make the hold longer
// than one window, and one behind can never make it negative. A nil ended_at
// (an older backend, or a stage that never stamped one) holds a full window
// from now.
func acceptanceVerdictHoldUntil(endedAt *time.Time, now time.Time, window time.Duration) time.Time {
	ceiling := now.Add(window)
	if endedAt == nil {
		return ceiling
	}
	until := endedAt.Add(window)
	if until.Before(now) {
		return now
	}
	if until.After(ceiling) {
		return ceiling
	}
	return until
}

// acceptanceVerdictSignal is the verdict string BOTH nextActionsFor call sites
// (getRunStatus, runStage) hand the classifier. A visible verdict (or the #3447
// unshipped sentinel) passes through unchanged. Otherwise it returns the
// acceptanceVerdictPending sentinel iff the acceptance stage is `succeeded`,
// carries an ended_at, and settled inside the in-flight window — and "" in
// every other case, including:
//
//   - an acceptance_skipped_out_of_scope marker in the window (binding
//     condition C1 on #4072): an out-of-scope skip writes NO outcome row by
//     design, and the classifier's acceptance_skipped_out_of_scope arms key on
//     an EMPTY verdict, so a sentinel here would shadow that merge-eligible
//     disposition with a wait;
//   - a nil ended_at: without the stamp the window cannot be anchored, so the
//     surface keeps its pre-#4072 outcome-unknown reading.
func acceptanceVerdictSignal(recent []AuditEntry, stages []Stage, now time.Time) string {
	if v := latestAcceptanceVerdict(recent); v != "" {
		return v
	}
	if acceptanceSkippedOutOfScopeIn(recent) {
		return ""
	}
	acc := stageByType(stages, "acceptance")
	if acc == nil || acc.State != "succeeded" || acc.EndedAt == nil {
		return ""
	}
	if now.Before(acceptanceVerdictHoldUntil(acc.EndedAt, now, acceptanceVerdictInFlightWindow)) {
		return acceptanceVerdictPending
	}
	return ""
}

// acceptanceAttemptVerdictFor is the BEST-EFFORT probe: ONE recent-audit read
// at auditLimitMax, classified by acceptanceAttemptVerdictIn. A read error
// returns Determined=false and never fails the wait — losing the verdict
// confirmation is strictly cheaper than failing a wait that may have been held
// for hours (the awaitStageRunTerminalBackstop / fixupRecoveryFor discipline).
func (r *runResolver) acceptanceAttemptVerdictFor(ctx context.Context, runID, stageID uuid.UUID) acceptanceAttemptVerdict {
	entries, err := r.api.ListRecentRunAudit(ctx, runID, auditLimitMax)
	if err != nil {
		return acceptanceAttemptVerdict{}
	}
	return acceptanceAttemptVerdictIn(entries, stageID.String())
}

// acceptanceVerdictHold is the outcome of awaitAcceptanceVerdict. Verdict or
// Disposition is set when a probe found one; otherwise Pending reports whether
// the release is still UNCONFIRMED (true) or a confirmed absence past the
// in-flight window (false).
type acceptanceVerdictHold struct {
	Verdict     string
	Disposition string
	Pending     bool
}

// awaitAcceptanceVerdict holds a settled-succeeded acceptance release until the
// latest attempt's verdict lands. It probes immediately and returns on any
// result carrying a verdict or a disposition; otherwise it re-probes on
// r.reviewPollInterval until min(deadline, holdUntil) or ctx is done, with one
// final probe at the release instant.
//
// deadline is the CALLER's: start + the clamped timeout from
// clampAwaitTimeoutHeartbeat. On the await_stage poll path ctx is the pollCtx
// that the same deadline cancels, so a cancelled probe there is undetermined
// and releases as Pending=true — never an error.
//
// At release, Pending is true when the last probe was undetermined OR the
// release came before holdUntil (the verdict may still land), and false only
// when a probe confirmed no verdict for the attempt AND the window elapsed.
func (r *runResolver) awaitAcceptanceVerdict(ctx context.Context, runID, stageID uuid.UUID, endedAt *time.Time, deadline time.Time) acceptanceVerdictHold {
	window := r.acceptanceVerdictWindow
	if window <= 0 {
		window = acceptanceVerdictInFlightWindow
	}
	interval := r.reviewPollInterval
	if interval <= 0 {
		interval = defaultReviewPollInterval
	}
	holdUntil := acceptanceVerdictHoldUntil(endedAt, time.Now(), window)
	release := holdUntil
	if deadline.Before(release) {
		release = deadline
	}

	var last acceptanceAttemptVerdict
	for {
		last = r.acceptanceAttemptVerdictFor(ctx, runID, stageID)
		if last.Verdict != "" || last.Disposition != "" {
			return acceptanceVerdictHold{Verdict: last.Verdict, Disposition: last.Disposition}
		}
		remaining := time.Until(release)
		if remaining <= 0 || ctx.Err() != nil {
			break
		}
		if remaining > interval {
			remaining = interval
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	return acceptanceVerdictHold{Pending: !last.Determined || time.Now().Before(holdUntil)}
}
