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
//     ONE row per base, the oldest due one, is RE-PROBED on a stateless
//     exponential schedule over its yielded_at age (yieldReprobeDue).
//   - ReprobeMergeQueueYield is the targeted, no-backoff re-probe by run id
//     (the merge_run re-invoke and the approval webhook, slice 4).
//
// A re-probe runs the ONE forge-approval fold at the row's anchored head with
// no store transaction open: approved → ClearYield (CAS on anchored head and
// yielded_at) → merge_queue_yield_cleared → a detached pump of the base (the
// row rejoins FIFO at its enqueued_at); the same non-approved reason → nothing;
// a changed reason → UpdateYield (same CAS) and a merge_queue_yielded
// transition row; the PR head moved → ReanchorYielded to the current head,
// KEEPING the marker and the episode (approval condition C2: a head move never
// clears a marker), then the new head is judged on the same probe; merged, a
// transport error or a CAS conflict → nothing. Contract:
// backend/internal/server/README.md and backend/internal/mergequeue/README.md
// § "Yield".

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
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

// yieldReprobeDue reports whether a yielded row of age is due a scheduled
// re-probe on the tick that observes it: the schedule points are interval·2^k
// (k = 0..log2 of the cap) and then every cap·interval, and a row is due when
// a point fell inside the last tick, (age − interval, age]. It is pure (no
// state: the schedule is the row's own database-clock age), so replicas and
// restarts need no bookkeeping. A row younger than one tick is never due (the
// first point is one interval): the decision that yielded it just ran.
func yieldReprobeDue(age, interval time.Duration) bool {
	if interval <= 0 {
		return false
	}
	capSpan := interval * mergeQueueYieldReprobeMaxBackoffTicks
	if age <= capSpan {
		for p := interval; p <= capSpan; p *= 2 {
			if p > age-interval && p <= age {
				return true
			}
		}
		return false
	}
	return (age-capSpan)%capSpan < interval
}

// RunMergeQueueYieldReprobe sweeps every active base on every interval tick
// (sweepMergeQueueYields), returning when ctx is cancelled. It joins
// bgMergeQueuePump, so Server.Shutdown drains it; the boot-window residual is
// RunMergeCandidateQueuePump's.
func (s *Server) RunMergeQueueYieldReprobe(ctx context.Context, interval time.Duration) {
	s.bgMergeQueuePump.Add(1)
	defer s.bgMergeQueuePump.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepMergeQueueYields(ctx, interval)
		}
	}
}

// sweepMergeQueueYields surfaces every yielded row older than one tick and
// re-probes at most one due row per base, the oldest by (yielded_at, id). It
// returns how many rows it re-probed. A list error skips the base (or the
// sweep); the next tick retries.
func (s *Server) sweepMergeQueueYields(ctx context.Context, interval time.Duration) int {
	if s.cfg.MergeQueue == nil {
		return 0
	}
	bases, err := s.cfg.MergeQueue.ActiveBases(ctx)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge queue: listing active bases for the yield re-probe failed; the next tick retries",
			slog.String("error", err.Error()))
		return 0
	}
	probed := 0
	for _, b := range bases {
		entries, err := s.cfg.MergeQueue.ListForBase(ctx, b)
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"merge queue: listing a base for the yield re-probe failed; the next tick retries",
				slog.String("repo", b.Repo), slog.String("base_ref", b.BaseRef), slog.String("error", err.Error()))
			continue
		}
		var due *mergequeue.Entry
		for i := range entries {
			e := entries[i]
			if e.State != mergequeue.StateHeld || !e.Yielded() {
				continue
			}
			if e.YieldAge >= interval { // posted-once age check: a fresh yield is not surfaced yet
				s.surfaceMergeQueueYield(ctx, e)
			}
			if yieldReprobeDue(e.YieldAge, interval) && (due == nil || yieldedBefore(e, *due)) {
				due = &e
			}
		}
		if due != nil {
			s.reprobeMergeQueueYield(ctx, *due)
			probed++
		}
	}
	return probed
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

// mergeQueueYieldRow is the ADR-112 R3 precedence row a stored reason names.
func mergeQueueYieldRow(reason string) int {
	switch reason {
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
// backoff: the merge_run re-invoke and the approval webhook's trigger (the
// fold re-reads the forge and is the sole decision). It reports whether the
// marker was cleared (the base is then pumped, detached). A run with no
// active entry, or one not yielded, does nothing.
func (s *Server) ReprobeMergeQueueYield(ctx context.Context, runID uuid.UUID) bool {
	if s.cfg.MergeQueue == nil {
		return false
	}
	st, found, err := s.cfg.MergeQueue.ActiveForRun(ctx, runID)
	if err != nil || !found {
		return false
	}
	return s.reprobeMergeQueueYield(ctx, st.Entry) == reprobeCleared
}

// reprobeMergeQueueYield re-judges the yielded entry e at its anchored head
// (see the file comment). No store transaction is open across the fold.
func (s *Server) reprobeMergeQueueYield(ctx context.Context, e mergequeue.Entry) mergeQueueYieldReprobeOutcome {
	if e.State != mergequeue.StateHeld || !e.Yielded() || s.cfg.RunRepo == nil {
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
	fold, err := s.forgeApprovalFold(ctx, runRow, e.AnchoredHeadSHA)
	if err != nil {
		log("merge queue: the yield re-probe's review snapshot failed; the next tick retries", err)
		return reprobeNothing
	}
	if fold.Merged {
		return reprobeNothing
	}
	if fold.Reason == forgeApprovalReasonHeadMoved { // C2(a): re-anchor the yielded row, keep the marker
		if fold.ObservedHead == "" {
			return reprobeNothing
		}
		moved, err := s.cfg.MergeQueue.ReanchorYielded(ctx, e.ID, e.AnchoredHeadSHA, *e.YieldedAt, fold.ObservedHead, "")
		if err != nil {
			log("merge queue: the yield re-probe could not re-anchor the moved head; another write owns the entry", err)
			return reprobeNothing
		}
		e = moved
		fold, err = s.forgeApprovalFold(ctx, runRow, e.AnchoredHeadSHA)
		if err != nil || fold.Merged || fold.Reason == forgeApprovalReasonHeadMoved {
			return reprobeReanchored
		}
	}
	if fold.approved() {
		cleared, err := s.cfg.MergeQueue.ClearYield(ctx, e.ID, e.AnchoredHeadSHA, *e.YieldedAt)
		if err != nil {
			log("merge queue: the yield re-probe found a qualifying approval but the clear did not land", err)
			return reprobeNothing
		}
		s.writeMergeQueueYieldCleared(ctx, e, cleared, fold)
		s.pumpMergeCandidateQueueDetached(ctx, e.Base())
		return reprobeCleared
	}
	y, _ := mergeOnPassFoldYield(fold)
	if y.Reason == e.YieldReason { // same reason: dedup, nothing written
		return reprobeUnchanged
	}
	updated, err := s.cfg.MergeQueue.UpdateYield(ctx, e.ID, e.AnchoredHeadSHA, *e.YieldedAt, y.marker())
	if err != nil {
		log("merge queue: the yield re-probe's reason transition did not land", err)
		return reprobeNothing
	}
	_ = s.writeMergeQueueYielded(ctx, updated, y, mergeQueueYieldSiteReprobe, e.YieldReason)
	return reprobeTransitioned
}

// writeMergeQueueYieldCleared appends the INTERNAL merge_queue_yield_cleared
// row (the episode's resolution) for prior, the entry as it was yielded, now
// cleared. The ClearYield CAS has committed, so a failed append is logged at
// ERROR naming entry_id and yielded_at and the row is never rolled back
// (approval condition C7).
func (s *Server) writeMergeQueueYieldCleared(ctx context.Context, prior, cleared mergequeue.Entry, fold forgeApprovalDecision) {
	if s.cfg.AuditRepo == nil {
		return
	}
	entryID, yieldedAt := mergeQueueYieldEpisode(prior)
	payload, _ := json.Marshal(map[string]any{
		"entry_id":     entryID,
		"base_ref":     cleared.BaseRef,
		"head_sha":     cleared.AnchoredHeadSHA,
		"yield_reason": prior.YieldReason,
		"yielded_at":   yieldedAt,
		"basis":        fold.Basis,
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
