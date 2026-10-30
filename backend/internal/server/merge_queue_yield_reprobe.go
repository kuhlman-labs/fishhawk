package server

// ADR-112 R3/R5 (#4632, slice 3): the yield RE-PROBE and its surfacing. A
// yielded entry is held with a marker AdmitNext skips, and "clearing a marker
// is the only way back in". This file is what clears one:
//
//   - RunMergeQueueYieldReprobe is a sibling ticker of the queue pump
//     (MergeCandidateQueuePumpInterval). Each tick sweeps every active base:
//     every yielded row older than one tick is SURFACED (its missing
//     merge_queue_yielded row re-appended, approval condition C7, and one
//     operator-visible merge_queue_yield_posted row per yield episode, both
//     under the run's merge-intent lock, approval condition C6), and AT MOST
//     ONE due row per base is RE-PROBED on an exponential schedule over its
//     yielded_at age. Due is judged against the row's OWN service mark
//     (ADR-112 Amendment A2.1, yieldReprobeDueFor: the age at which this
//     process last probed it), and the probed row is the due one with the
//     oldest mark, never-serviced first, ties by yielded_at (A2.2,
//     yieldServicedBefore), so N due rows on a base are each probed within N
//     sweeps.
//   - The same tick then runs the ADR-093 D4 automatic re-entry sweep
//     (sweepMergeQueueReentries, merge_queue_reentry.go), which re-enters an
//     ejected red pass at the tail once its delegated fix-up has pushed and
//     the run is eligible again, on the same schedule (yieldReprobeDue) over
//     its own previous-sweep window.
//   - ReprobeMergeQueueYield is the targeted, no-backoff re-probe by run id
//     (the merge_run re-invoke, which FORCES one re-attempt, and the approval
//     webhook, which does not, slice 4).
//
// A re-probe judges the row with no store transaction open, in the order of
// docs/adr/112 Amendment A3.4 (#4780): (1) GATE-INDEPENDENT PR-head
// discovery: the PR is read (merged, closed or a read error → nothing) and a
// moved head re-anchors the row with ReanchorYielded (CAS on head and
// yield_revision), KEEPING the marker, yielded_at, the approval anchor, the
// row-4 flag and the budget (approval condition C2, A3.7: a head move never
// clears a marker), and the probe judges the new head; (2) the forge-approval
// fold at the anchored head (its head_moved branch stays as the race
// fallback); (3) the merge-gate legs (mergeQueueMergeEligibility at the
// anchored head; an undeterminable result → nothing, fail closed); (4) the
// tier (mergeQueueYieldTier, A3.5: veto > merge-gate > row-4 > fold). Not
// blocked → ClearYield (CAS on anchored head and yield_revision, A3.6) →
// merge_queue_yield_cleared → a detached pump of the base (the row rejoins
// FIFO at its enqueued_at); blocked with the stored reason → nothing; blocked
// with another reason → UpdateYield (same CAS) and a merge_queue_yielded
// transition row; a CAS conflict → nothing. force (the operator's merge_run,
// A3.9) skips ONLY the row-4 fresh snapshot. Contract:
// backend/internal/server/README.md and backend/internal/mergequeue/README.md
// § "Yield".

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The re-probe's own merge_queue_yielded sites: a reason transition, and a
// row re-appended for a still-marked episode whose original append failed.
const (
	mergeQueueYieldSiteReprobe = "reprobe"
	mergeQueueYieldSiteHealed  = "reprobe_heal"
)

// mergeQueueYieldReprobeMaxBackoffTicks caps yieldReprobeDue's doubling: past
// it a yielded row is re-probed once every this many ticks.
const mergeQueueYieldReprobeMaxBackoffTicks = 16

// mergeQueueYieldSurfaceTimeout bounds one row's surfacing: the merge-intent
// lock wait plus its audit reads and appends.
const mergeQueueYieldSurfaceTimeout = 30 * time.Second

// mergeQueueYieldReprobeOutcome is what one re-probe did.
type mergeQueueYieldReprobeOutcome string

const (
	reprobeNothing      mergeQueueYieldReprobeOutcome = "nothing"
	reprobeCleared      mergeQueueYieldReprobeOutcome = "cleared"
	reprobeUnchanged    mergeQueueYieldReprobeOutcome = "unchanged"
	reprobeTransitioned mergeQueueYieldReprobeOutcome = "transitioned"
	reprobeReanchored   mergeQueueYieldReprobeOutcome = "reanchored"
)

// yieldReprobeDue reports whether a row of age is due a scheduled probe on
// the sweep that observes it, judged over the time since the caller's
// previous sweep: a schedule point (yieldReprobeLastPoint) fell inside
// (age − since, age]. since <= 0 (no previous sweep: a restart) widens the
// window to (0, age], so every row past its first point is due once. The
// window is the ACTUAL time since the previous sweep, not one interval: a
// sweep that overran its tick (time.Ticker drops ticks while the receiver is
// busy) never skips a point, so no row past the cap starves on a tick-parity
// stride. The schedule is the row's own database-clock age and since is a
// duration on this process's clock (no instants cross the two clocks); only
// since is process state. The ADR-093 D4 re-entry sweep judges its rows with
// it; the yield re-probe judges against per-entry service marks instead
// (yieldReprobeDueAfter, ADR-112 Amendment A2.1).
func yieldReprobeDue(age, since, interval time.Duration) bool {
	if interval <= 0 || age < interval {
		return false
	}
	lo := time.Duration(0)
	if since > 0 && age-since > 0 {
		lo = age - since
	}
	return yieldReprobeLastPoint(age, interval) > lo
}

// yieldReprobeLastPoint is the latest schedule point <= age (age >= interval
// > 0): the points are interval·2^k (k = 0..log2 of the cap) and then every
// cap·interval. A row younger than one interval has no point (the first is
// one interval): the decision that yielded it just ran.
func yieldReprobeLastPoint(age, interval time.Duration) time.Duration {
	capSpan := interval * mergeQueueYieldReprobeMaxBackoffTicks
	last := interval
	if age > capSpan {
		return age / capSpan * capSpan
	}
	for last*2 <= age && last*2 <= capSpan {
		last *= 2
	}
	return last
}

// yieldReprobeDueAfter reports whether a yielded row of age is due a
// scheduled re-probe given the age at which this process last serviced it
// (ADR-112 Amendment A2.1): a schedule point fell inside (servicedAge, age].
// servicedAge 0 (never serviced, or a new episode) judges (0, age], so a row
// past its first point is due once. A row younger than one interval is never
// due.
func yieldReprobeDueAfter(age, servicedAge, interval time.Duration) bool {
	if interval <= 0 || age < interval {
		return false
	}
	return yieldReprobeLastPoint(age, interval) > servicedAge
}

// yieldServicedMark is one entry's ADR-112 Amendment A2.1 service mark: the
// yield episode (yieldedAt) and database-clock age at which this process's
// sweep last re-probed it, and seq, its place in the process-local service
// order (strictly increasing from 1; 0 is never serviced).
type yieldServicedMark struct {
	yieldedAt time.Time
	age       time.Duration
	seq       uint64
}

// yieldReprobeDueFor judges e (yielded) against its own service mark and
// returns whether it is due and its service seq (0: never serviced). The mark
// applies only while e.YieldedAt is NOT after the mark's: a later yielded_at
// is a new episode, judged as never serviced over (0, age].
func (s *Server) yieldReprobeDueFor(e mergequeue.Entry, interval time.Duration) (due bool, seq uint64) {
	var servicedAge time.Duration
	s.yieldSweepMu.Lock()
	if m, ok := s.yieldServiced[e.ID]; ok && e.YieldedAt != nil && !e.YieldedAt.After(m.yieldedAt) {
		servicedAge, seq = m.age, m.seq
	}
	s.yieldSweepMu.Unlock()
	return yieldReprobeDueAfter(e.YieldAge, servicedAge, interval), seq
}

// markYieldReprobeServiced advances e's service mark to its current episode
// and age, at the next service seq. Only the probed row's mark advances, even
// when its probe did nothing (ADR-112 Amendment A2.1).
func (s *Server) markYieldReprobeServiced(e mergequeue.Entry) {
	if e.YieldedAt == nil {
		return
	}
	s.yieldSweepMu.Lock()
	defer s.yieldSweepMu.Unlock()
	if s.yieldServiced == nil {
		s.yieldServiced = make(map[uuid.UUID]yieldServicedMark)
	}
	s.yieldServiceSeq++
	s.yieldServiced[e.ID] = yieldServicedMark{yieldedAt: *e.YieldedAt, age: e.YieldAge, seq: s.yieldServiceSeq}
}

// pruneYieldReprobeServiced drops the mark of every entry not in seen. The
// sweep calls it only after a FULLY listed sweep, so a failed listing never
// drops a live row's mark.
func (s *Server) pruneYieldReprobeServiced(seen map[uuid.UUID]struct{}) {
	s.yieldSweepMu.Lock()
	defer s.yieldSweepMu.Unlock()
	for id := range s.yieldServiced {
		if _, ok := seen[id]; !ok {
			delete(s.yieldServiced, id)
		}
	}
}

// RunMergeQueueYieldReprobe sweeps every active base on every interval tick
// (sweepMergeQueueYields), then runs the ADR-093 D4 automatic re-entry sweep
// (sweepMergeQueueReentries, merge_queue_reentry.go) on its own
// process-local previous-sweep clock and keyset pager, returning when ctx is
// cancelled. It joins bgMergeQueuePump, so Server.Shutdown drains it; the
// boot-window residual is RunMergeCandidateQueuePump's.
func (s *Server) RunMergeQueueYieldReprobe(ctx context.Context, interval time.Duration) {
	s.bgMergeQueuePump.Add(1)
	defer s.bgMergeQueuePump.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var reentry mergeQueueReentryClock
	var reentryPage mergeQueueReentryPager
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepMergeQueueYields(ctx, interval)
			s.sweepMergeQueueReentries(ctx, interval, reentry.since(time.Now()), &reentryPage)
		}
	}
}

// sweepMergeQueueYields surfaces every yielded row older than one tick and
// re-probes at most one due row per base: due against the row's own service
// mark (yieldReprobeDueFor, ADR-112 Amendment A2.1), and among due rows the
// oldest mark first, never-serviced first, ties by (yielded_at, id)
// (yieldServicedBefore, A2.2). Only the probed row's mark advances. Marks of
// entries no longer listed are pruned, but only when ActiveBases and every
// ListForBase succeeded. It returns how many rows it re-probed. A list error
// skips the base (or the sweep); the next tick retries.
func (s *Server) sweepMergeQueueYields(ctx context.Context, interval time.Duration) int {
	if s.cfg.MergeQueue == nil {
		return 0
	}
	bases, err := s.cfg.MergeQueue.ActiveBases(ctx)
	listed := err == nil // every listing succeeded: only then may marks be pruned
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge queue: listing active bases for the yield re-probe failed; the next tick retries",
			slog.String("error", err.Error()))
	}
	seen := make(map[uuid.UUID]struct{})
	probed := 0
	for _, b := range bases {
		entries, err := s.cfg.MergeQueue.ListForBase(ctx, b)
		if err != nil {
			listed = false
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"merge queue: listing a base for the yield re-probe failed; the next tick retries",
				slog.String("repo", b.Repo), slog.String("base_ref", b.BaseRef), slog.String("error", err.Error()))
			continue
		}
		var due *mergequeue.Entry
		var dueSeq uint64
		for i := range entries {
			e := entries[i]
			seen[e.ID] = struct{}{}
			if e.State != mergequeue.StateHeld || !e.Yielded() {
				continue
			}
			if e.YieldAge >= interval { // posted-once age check: a fresh yield is not surfaced yet
				s.surfaceMergeQueueYield(ctx, e)
			}
			if ok, seq := s.yieldReprobeDueFor(e, interval); ok && (due == nil || yieldServicedBefore(seq, e, dueSeq, *due)) {
				due, dueSeq = &e, seq
			}
		}
		if due != nil {
			s.reprobeMergeQueueYield(ctx, *due, false)
			s.markYieldReprobeServiced(*due)
			probed++
		}
	}
	if listed {
		s.pruneYieldReprobeServiced(seen)
	}
	return probed
}

// yieldServicedBefore is ADR-112 Amendment A2.2's re-probe order: a (service
// seq aSeq) before b (bSeq) when its service mark is older, a never-serviced
// row (seq 0) first, and between equal marks by (yielded_at, id).
func yieldServicedBefore(aSeq uint64, a mergequeue.Entry, bSeq uint64, b mergequeue.Entry) bool {
	if aSeq != bSeq { // ADR-112 A2.2: the oldest service mark first; never serviced (0) first
		return aSeq < bSeq
	}
	return yieldedBefore(a, b)
}

// yieldedBefore orders yielded entries by (yielded_at, id).
func yieldedBefore(a, b mergequeue.Entry) bool {
	if !a.YieldedAt.Equal(*b.YieldedAt) {
		return a.YieldedAt.Before(*b.YieldedAt)
	}
	return a.ID.String() < b.ID.String()
}

// mergeQueueYieldEpisode is the (entry_id, yielded_at) key of e's yield
// episode, in the spelling writeMergeQueueYielded stores.
func mergeQueueYieldEpisode(e mergequeue.Entry) (entryID, yieldedAt string) {
	if e.YieldedAt == nil {
		return e.ID.String(), ""
	}
	return e.ID.String(), e.YieldedAt.UTC().Format(time.RFC3339Nano)
}

// mergeQueueYieldRow is the ADR-112 R3 precedence row a stored reason names
// (the three merge-gate reasons are row 1, docs/adr/112 A3.2).
func mergeQueueYieldRow(reason string) int {
	switch reason {
	case mergeQueueYieldMergeChecksRed, mergeQueueYieldMergeChecksPending, mergeQueueYieldMergeReviewPending:
		return mergeQueueYieldRowEligibility
	case mergeCandidateQueueReasonForgeApprovalVetoed:
		return mergeQueueYieldRowVeto
	case mergeQueueYieldForgeReviewRequired:
		return mergeQueueYieldRowForgeRefusal
	}
	return mergeQueueYieldRowApprovalFold
}

// surfaceMergeQueueYield runs, under the run's merge-intent lock so two
// sweeps (two replicas) cannot both write one episode's row (approval
// condition C6): (1) the C7 heal, re-appending merge_queue_yielded when no
// row names this (entry_id, yielded_at); (2) the posted-once surface, one
// merge_queue_yield_posted per episode. A posted row refreshes the issue
// thread (notifyOperatorVisible) AFTER the lock is released. Every failure is
// logged and retried on the next tick.
func (s *Server) surfaceMergeQueueYield(ctx context.Context, e mergequeue.Entry) {
	if s.cfg.AuditRepo == nil {
		return
	}
	lctx, cancel := context.WithTimeout(ctx, mergeQueueYieldSurfaceTimeout)
	defer cancel()
	posted := false
	err := s.cfg.MergeQueue.WithMergeIntentLock(lctx, e.RunID, func(fctx context.Context) error {
		have, err := s.mergeQueueYieldEpisodeRecorded(fctx, e, CategoryMergeQueueYielded)
		if err != nil {
			return err
		}
		if !have { // C7 heal: the original append failed after its CAS committed
			y := newMergeQueueYield(e.YieldReason, e.YieldDetail, mergeQueueYieldRow(e.YieldReason))
			if err := s.writeMergeQueueYielded(fctx, e, y, mergeQueueYieldSiteHealed, ""); err != nil {
				return err
			}
		}
		have, err = s.mergeQueueYieldEpisodeRecorded(fctx, e, CategoryMergeQueueYieldPosted)
		if err != nil || have { // posted-once dedup scan
			return err
		}
		if err := s.writeMergeQueueYieldPosted(fctx, e); err != nil {
			return err
		}
		posted = true
		return nil
	})
	if err != nil {
		entryID, yieldedAt := mergeQueueYieldEpisode(e)
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge queue: surfacing a yielded entry failed; the next re-probe tick retries",
			slog.String("run_id", e.RunID.String()), slog.String("entry_id", entryID),
			slog.String("yielded_at", yieldedAt), slog.String("error", err.Error()))
	}
	if posted {
		s.notifyOperatorVisible(ctx, e.RunID, CategoryMergeQueueYieldPosted)
	}
}

// mergeQueueYieldEpisodeRecorded reports whether the run carries a category
// row naming e's (entry_id, yielded_at).
func (s *Server) mergeQueueYieldEpisodeRecorded(ctx context.Context, e mergequeue.Entry, category string) (bool, error) {
	rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, e.RunID, category)
	if err != nil {
		return false, err
	}
	entryID, yieldedAt := mergeQueueYieldEpisode(e)
	for _, r := range rows {
		var p struct {
			EntryID   string `json:"entry_id"`
			YieldedAt string `json:"yielded_at"`
		}
		if json.Unmarshal(r.Payload, &p) == nil && p.EntryID == entryID && p.YieldedAt == yieldedAt {
			return true, nil
		}
	}
	return false, nil
}

// writeMergeQueueYieldPosted appends the operator-visible
// merge_queue_yield_posted row for e's episode. yield_detail passes the yield
// sanitiser here too (the thread line's sink).
func (s *Server) writeMergeQueueYieldPosted(ctx context.Context, e mergequeue.Entry) error {
	entryID, yieldedAt := mergeQueueYieldEpisode(e)
	payload, _ := json.Marshal(map[string]any{
		"entry_id":     entryID,
		"base_ref":     e.BaseRef,
		"head_sha":     e.AnchoredHeadSHA,
		"yield_reason": e.YieldReason,
		"yield_detail": mergeQueueYieldDetail(e.YieldDetail),
		"next_step":    mergeQueueYieldNextStep(e.YieldReason),
		"yielded_at":   yieldedAt,
	})
	kind, subject := audit.ActorSystem, mergeCandidateSystemSubject
	_, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID: e.RunID, Timestamp: time.Now().UTC(), Category: CategoryMergeQueueYieldPosted,
		ActorKind: &kind, ActorSubject: &subject, Payload: payload,
	})
	return err
}

// ReprobeMergeQueueYield re-probes runID's yielded entry NOW, with no
// backoff: the merge_run re-invoke and the approval and check_run webhook
// triggers (the probe re-reads the forge and is the sole decision). force is
// the operator's explicit merge_run, which skips ONLY the row-4 fresh
// snapshot (mergeQueueYieldTier, docs/adr/112 A3.9); every automatic trigger
// passes false. It reports
// whether the marker was cleared (the base is then pumped, detached). A run
// with no active entry, or one not yielded, does nothing.
func (s *Server) ReprobeMergeQueueYield(ctx context.Context, runID uuid.UUID, force bool) bool {
	if s.cfg.MergeQueue == nil {
		return false
	}
	st, found, err := s.cfg.MergeQueue.ActiveForRun(ctx, runID)
	if err != nil || !found {
		return false
	}
	return s.reprobeMergeQueueYield(ctx, st.Entry, force) == reprobeCleared
}

// reprobeMergeQueueYield re-judges the yielded entry e (see the file
// comment; force as ReprobeMergeQueueYield). No store transaction is open
// across a forge read.
func (s *Server) reprobeMergeQueueYield(ctx context.Context, e mergequeue.Entry, force bool) mergeQueueYieldReprobeOutcome {
	if e.State != mergequeue.StateHeld || !e.Yielded() || s.cfg.RunRepo == nil || s.cfg.GitHub == nil {
		return reprobeNothing
	}
	log := func(msg string, err error) {
		entryID, yieldedAt := mergeQueueYieldEpisode(e)
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, msg,
			slog.String("run_id", e.RunID.String()), slog.String("entry_id", entryID),
			slog.String("yielded_at", yieldedAt), slog.String("error", err.Error()))
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, e.RunID)
	if err != nil {
		log("merge queue: the yield re-probe could not read the run; the next tick retries", err)
		return reprobeNothing
	}
	if runRow.State.IsTerminal() {
		return reprobeNothing
	}
	nothing := reprobeNothing // reprobeReanchored once this probe moved the anchor
	reanchor := func(head string) bool {
		moved, err := s.cfg.MergeQueue.ReanchorYielded(ctx, e.ID, e.AnchoredHeadSHA, e.YieldRevision, head, "")
		if err != nil {
			log("merge queue: the yield re-probe could not re-anchor the moved head; another write owns the entry", err)
			return false
		}
		e, nothing = moved, reprobeReanchored
		return true
	}

	// (1) A3.4: gate-independent PR-head discovery. A gate-less fold is
	// approved without reading the PR, so the head is read here.
	pr, err := s.mergeQueueReentryReadPR(ctx, runRow)
	if err != nil {
		log("merge queue: the yield re-probe could not read the pull request; the next tick retries", err)
		return reprobeNothing
	}
	if pr.Merged || pr.State == "closed" {
		return reprobeNothing
	}
	if pr.HeadSHA != "" && pr.HeadSHA != e.AnchoredHeadSHA && !reanchor(pr.HeadSHA) { // A3.4 head discovery
		return reprobeNothing
	}

	// (2) The forge-approval fold at the anchored head.
	fold, err := s.forgeApprovalFold(ctx, runRow, e.AnchoredHeadSHA)
	if err != nil {
		log("merge queue: the yield re-probe's review snapshot failed; the next tick retries", err)
		return nothing
	}
	if fold.Merged {
		return nothing
	}
	if fold.Reason == forgeApprovalReasonHeadMoved { // the race fallback: re-anchor the yielded row, keep the marker
		if fold.ObservedHead == "" || fold.ObservedHead == e.AnchoredHeadSHA || !reanchor(fold.ObservedHead) {
			return nothing
		}
		fold, err = s.forgeApprovalFold(ctx, runRow, e.AnchoredHeadSHA)
		if err != nil || fold.Merged || fold.Reason == forgeApprovalReasonHeadMoved {
			return reprobeReanchored
		}
	}

	// (3) The merge-gate legs at the anchored head (merge variant).
	elig := s.mergeQueueMergeEligibility(ctx, runRow, e.AnchoredHeadSHA,
		mergeQueueIntentAnchor{Sequence: e.MergeIntentSequence, BaseRef: e.BaseRef})
	if elig.Undeterminable { // fail closed: an unreadable leg never clears
		log("merge queue: the yield re-probe could not determine every merge-gate leg; the entry stays yielded",
			errors.New(mergeQueueReasonsText(elig.Reasons)))
		return nothing
	}

	// (4) The tier.
	y, blocked, row4Gone, ok := s.mergeQueueYieldTier(ctx, runRow, e, fold, elig, force)
	if !ok {
		return nothing
	}
	if !blocked {
		cleared, err := s.cfg.MergeQueue.ClearYield(ctx, e.ID, e.AnchoredHeadSHA, e.YieldRevision, row4Gone)
		if err != nil {
			log("merge queue: the yield re-probe found every leg passing but the clear did not land", err)
			return nothing
		}
		s.writeMergeQueueYieldCleared(ctx, e, cleared, fold)
		s.pumpMergeCandidateQueueDetached(ctx, e.Base())
		return reprobeCleared
	}
	if y.Reason == e.YieldReason { // same reason: dedup, nothing written
		if nothing == reprobeReanchored {
			return reprobeReanchored
		}
		return reprobeUnchanged
	}
	updated, err := s.cfg.MergeQueue.UpdateYield(ctx, e.ID, e.AnchoredHeadSHA, e.YieldRevision, y.marker())
	if err != nil {
		log("merge queue: the yield re-probe's reason transition did not land", err)
		return nothing
	}
	_ = s.writeMergeQueueYielded(ctx, updated, y, mergeQueueYieldSiteReprobe, e.YieldReason)
	return reprobeTransitioned
}

// mergeQueueYieldTier is THE clear predicate and reason tiering for a yielded
// entry e (docs/adr/112 A3.4/A3.5); every clear through the re-probe (the
// sweep, the targeted re-probe, the approval and check_run triggers, the
// merge_run re-invoke) judges it here. fold is the forge-approval fold and
// elig the merge-variant eligibility, both at e's anchored head. The FIRST
// matching tier blocks:
//
//  1. veto: the fold vetoed (forge_approval_vetoed);
//  2. merge-gate: a retryable merge-gate leg (mergeGateYieldFor over
//     mergeGateLegsOf(elig): merge_checks_red > merge_checks_pending >
//     merge_review_pending);
//  3. row-4: e.Row4Flag is set and force is false, and a FRESH review
//     snapshot (forgeReviewRequiredSnapshot, read even on a gate-less run)
//     shows REVIEW_REQUIRED, CHANGES_REQUESTED or BLOCKED
//     (forge_review_required); a snapshot read error is ok=false (the row
//     stays yielded, fail closed);
//  4. fold: any other non-approved fold (mergeOnPassFoldYield).
//
// Otherwise blocked is false: the row clears. row4Gone reports the snapshot
// read on THIS probe showed the row-4 condition gone, the only evidence that
// clears row4_flag (A3.6): a forced clear never reads it, so it keeps the
// flag. force (A3.9, the operator's merge_run) skips ONLY the tier-3
// snapshot: never the fold, the veto, the merge-gate legs or the CAS.
func (s *Server) mergeQueueYieldTier(ctx context.Context, runRow *run.Run, e mergequeue.Entry, fold forgeApprovalDecision, elig mergeQueueEligibilityResult, force bool) (y mergeQueueYield, blocked bool, row4Gone bool, ok bool) {
	if fold.Result == forgeApprovalVetoed { // A3.5 tier 1: the veto
		vy, _ := mergeOnPassFoldYield(fold)
		return vy, true, false, true
	}
	if gy, gated := mergeGateYieldFor(mergeGateLegsOf(elig), elig); gated { // A3.5 tier 2: the merge-gate legs
		return gy, true, false, true
	}
	if e.Row4Flag && !force { // A3.5 tier 3: the row-4 fresh snapshot; force skips only this
		decision, status, present, err := s.forgeReviewRequiredSnapshot(ctx, runRow)
		if err != nil {
			entryID, yieldedAt := mergeQueueYieldEpisode(e)
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
				"merge queue: the forge_review_required re-probe could not read a fresh review snapshot; the entry stays yielded",
				slog.String("run_id", e.RunID.String()), slog.String("entry_id", entryID), slog.String("yielded_at", yieldedAt),
				slog.String("detail", sanitizeForgeReadDetail("review_snapshot", err.Error())))
			return mergeQueueYield{}, false, false, false
		}
		if present {
			entryID, yieldedAt := mergeQueueYieldEpisode(e)
			s.cfg.Logger.LogAttrs(ctx, slog.LevelDebug,
				"merge queue: the forge still reports the pull request needs review or is blocked; the forge_review_required entry stays yielded",
				slog.String("run_id", e.RunID.String()), slog.String("entry_id", entryID), slog.String("yielded_at", yieldedAt),
				slog.String("review_decision", decision), slog.String("merge_state_status", status))
			return newMergeQueueYield(mergeQueueYieldForgeReviewRequired,
				fmt.Sprintf(mergeQueueYieldForgeRefusalDetail, decision, status), mergeQueueYieldRowForgeRefusal), true, false, true
		}
		row4Gone = true
	}
	if !fold.approved() { // A3.5 tier 4: the fold
		fy, _ := mergeOnPassFoldYield(fold)
		return fy, true, row4Gone, true
	}
	return mergeQueueYield{}, false, row4Gone, true
}

// writeMergeQueueYieldCleared appends the INTERNAL merge_queue_yield_cleared
// row (the episode's resolution) for prior, the entry as it was yielded, now
// cleared. Its (entry_id, base_ref, yielded_at) is the key a later re-yield
// continues (mergeQueueYieldContinuation, docs/adr/112 A3.7). The ClearYield CAS has committed, so a failed append is logged at
// ERROR naming entry_id and yielded_at and the row is never rolled back
// (approval condition C7).
func (s *Server) writeMergeQueueYieldCleared(ctx context.Context, prior, cleared mergequeue.Entry, fold forgeApprovalDecision) {
	if s.cfg.AuditRepo == nil {
		return
	}
	entryID, yieldedAt := mergeQueueYieldEpisode(prior)
	payload, _ := json.Marshal(map[string]any{
		"entry_id":       entryID,
		"base_ref":       cleared.BaseRef,
		"head_sha":       cleared.AnchoredHeadSHA,
		"base_sha":       cleared.AnchoredBaseSHA,
		"yield_reason":   prior.YieldReason,
		"yielded_at":     yieldedAt,
		"yield_revision": cleared.YieldRevision,
		"basis":          fold.Basis,
	})
	kind, subject := audit.ActorSystem, mergeCandidateSystemSubject
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID: prior.RunID, Timestamp: time.Now().UTC(), Category: CategoryMergeQueueYieldCleared,
		ActorKind: &kind, ActorSubject: &subject, Payload: payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
			"merge queue: the yield cleared but merge_queue_yield_cleared could not be recorded; the entry rejoins the line regardless",
			slog.String("run_id", prior.RunID.String()), slog.String("entry_id", entryID),
			slog.String("yielded_at", yieldedAt), slog.String("error", err.Error()))
	}
}
