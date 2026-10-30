package server

// merge_queue_checkrun_trigger.go is the check_run webhook's yield re-probe
// trigger (docs/adr/112 A3.8, #4780). A completed check run at a yielded,
// intent-bearing queue entry's anchored head re-probes that entry at once
// instead of waiting for the yield sweep's backoff schedule, so a repaired
// or re-run required check clears a merge-gate yield (merge_checks_red,
// merge_checks_pending) as soon as CI reports. The payload is a trigger
// only: the re-probe (ReprobeMergeQueueYield, never forced) re-reads the
// forge and applies the A3.4 clear predicate, so a forged or stale delivery
// can at most cost one bounded probe.
//
// Amplification is bounded by a process-local per-run coalescer
// (checkRunYieldTrigger):
//
//   - single flight: at most one probe per run runs at a time;
//   - dedupe: a check-run id already seen for the run never re-triggers
//     (a bounded per-run set, checkRunTriggerSeenCap);
//   - the approval webhook's per-run minimum interval
//     (MergeCandidateQueuePumpInterval) between probe starts;
//   - coalescing that never drops: a trigger arriving while a probe is in
//     flight, or inside the interval, records exactly ONE follow-up PER
//     PROCESS, run when the probe finishes or the interval elapses.
//
// The state is per process, like mergeQueueYieldApprovalTriggers: each
// replica coalesces its own deliveries, and duplicates across replicas are
// harmless because every marker write compare-and-swaps on
// (anchored_head_sha, yield_revision) (A3.6). The yield sweep remains the
// shared fallback for a delivery no replica acted on.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// checkRunTriggerSeenCap bounds each run's set of seen check-run ids; the
// oldest id is evicted first. A redelivery of an evicted id costs at most
// one more (coalesced, rate-limited) probe.
const checkRunTriggerSeenCap = 64

// checkRunTriggerIdleRetention is how long an idle run's coalescer state
// (its seen ids and last probe start) is kept after its last activity; the
// state is pruned on the next trigger past it, so the map stays bounded by
// the runs triggered within one retention window.
const checkRunTriggerIdleRetention = time.Hour

// mergeQueueYieldCheckRunTriggers is the check_run webhook's per-run
// coalescer (A3.8). It is process-local: each replica coalesces its own
// triggers, and the yield sweep's schedule is the shared fallback.
var mergeQueueYieldCheckRunTriggers = newCheckRunYieldTrigger(MergeCandidateQueuePumpInterval)

// checkRunYieldTrigger coalesces check_run re-probe triggers per run: single
// flight, dedupe by check-run id, a minimum interval between probe starts,
// and exactly one recorded follow-up (per process) for every trigger that
// arrives while a probe is in flight or inside the interval. interval, now
// and after are fields so tests drive it on a fake clock.
type checkRunYieldTrigger struct {
	interval time.Duration
	now      func() time.Time
	// after runs f once d has elapsed (time.AfterFunc in production). The
	// wait holds no WaitGroup slot, so a pending follow-up never delays
	// Server.Shutdown; only the probe itself is tracked (spawn).
	after func(d time.Duration, f func())

	mu   sync.Mutex
	runs map[uuid.UUID]*checkRunTriggerRun
}

// checkRunTriggerRun is one run's coalescer state.
type checkRunTriggerRun struct {
	inFlight bool // a probe is running
	waiting  bool // a follow-up is scheduled for the end of the interval
	followUp bool // a trigger arrived during the in-flight probe
	last     time.Time
	touched  time.Time
	seen     []int64 // check-run ids, oldest first, at most checkRunTriggerSeenCap
	// probe and spawn are the latest trigger's: a follow-up runs the probe
	// the most recent trigger supplied.
	probe func()
	spawn func(func())
}

// checkRunTriggerResult is fire's verdict, logged by the webhook consumer.
type checkRunTriggerResult string

const (
	// checkRunTriggerProbed started a probe now.
	checkRunTriggerProbed checkRunTriggerResult = "probed"
	// checkRunTriggerCoalesced recorded (or joined) the run's one follow-up.
	checkRunTriggerCoalesced checkRunTriggerResult = "coalesced"
	// checkRunTriggerDuplicate saw a check-run id already delivered.
	checkRunTriggerDuplicate checkRunTriggerResult = "duplicate"
)

// newCheckRunYieldTrigger returns a coalescer on the wall clock.
func newCheckRunYieldTrigger(interval time.Duration) *checkRunYieldTrigger {
	return &checkRunYieldTrigger{
		interval: interval,
		now:      time.Now,
		after:    func(d time.Duration, f func()) { time.AfterFunc(d, f) },
		runs:     map[uuid.UUID]*checkRunTriggerRun{},
	}
}

// fire records a trigger for runID from the check run checkRunID. probe is
// the re-probe; spawn runs a function on a tracked goroutine and MUST NOT
// run it synchronously (the probe's completion re-enters the coalescer).
func (t *checkRunYieldTrigger) fire(runID uuid.UUID, checkRunID int64, probe func(), spawn func(func())) checkRunTriggerResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.pruneLocked(now)
	r := t.runs[runID]
	if r == nil {
		r = &checkRunTriggerRun{}
		t.runs[runID] = r
	}
	r.touched = now
	if slices.Contains(r.seen, checkRunID) {
		return checkRunTriggerDuplicate
	}
	r.seen = append(r.seen, checkRunID)
	if len(r.seen) > checkRunTriggerSeenCap {
		r.seen = slices.Delete(r.seen, 0, len(r.seen)-checkRunTriggerSeenCap)
	}
	r.probe, r.spawn = probe, spawn
	switch {
	case r.inFlight:
		r.followUp = true
		return checkRunTriggerCoalesced
	case r.waiting:
		// The scheduled follow-up starts after this trigger and reads
		// fresh forge state, so it covers this trigger too.
		return checkRunTriggerCoalesced
	}
	if t.startLocked(runID, r, now) {
		return checkRunTriggerProbed
	}
	return checkRunTriggerCoalesced
}

// startLocked starts r's probe now, or, inside the minimum interval since the
// last probe start, schedules exactly one follow-up for the interval's end.
// It reports whether the probe started now. t.mu is held.
func (t *checkRunYieldTrigger) startLocked(runID uuid.UUID, r *checkRunTriggerRun, now time.Time) bool {
	if !r.last.IsZero() {
		if wait := r.last.Add(t.interval).Sub(now); wait > 0 {
			r.waiting = true
			t.after(wait, func() { t.wake(runID) })
			return false
		}
	}
	r.inFlight, r.last = true, now
	probe := r.probe
	r.spawn(func() {
		defer t.finish(runID)
		probe()
	})
	return true
}

// wake runs the follow-up scheduled at the end of the interval.
func (t *checkRunYieldTrigger) wake(runID uuid.UUID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.runs[runID]
	if r == nil || !r.waiting {
		return
	}
	r.waiting = false
	t.startLocked(runID, r, t.now())
}

// finish ends runID's in-flight probe and starts (or schedules) the one
// follow-up recorded while it ran.
func (t *checkRunYieldTrigger) finish(runID uuid.UUID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.runs[runID]
	if r == nil {
		return
	}
	now := t.now()
	r.inFlight, r.touched = false, now
	if r.followUp {
		r.followUp = false
		t.startLocked(runID, r, now)
	}
}

// pruneLocked drops every idle run state (no probe in flight, none scheduled
// or recorded) untouched for the retention window (at least the interval, so
// the minimum interval is never forgotten early). t.mu is held.
func (t *checkRunYieldTrigger) pruneLocked(now time.Time) {
	keep := max(t.interval, checkRunTriggerIdleRetention)
	for id, r := range t.runs {
		if !r.inFlight && !r.waiting && !r.followUp && now.Sub(r.touched) >= keep {
			delete(t.runs, id)
		}
	}
}

// triggerMergeQueueYieldOnCheckRun is the check_run webhook consumer of
// A3.8: it re-probes, through the process-local coalescer, the yielded queue
// entry of every run the completed check run names by pull request.
// Best-effort: every skip and read failure does nothing (the yield sweep
// re-probes on its schedule), never a 5xx.
func (s *Server) triggerMergeQueueYieldOnCheckRun(ctx context.Context, raw []byte) {
	s.fireCheckRunYieldTrigger(ctx, raw, mergeQueueYieldCheckRunTriggers,
		func(pctx context.Context, runID uuid.UUID) { s.ReprobeMergeQueueYield(pctx, runID, false) })
}

// fireCheckRunYieldTrigger parses a check_run delivery and fires t for each
// run whose entry the delivery applies to (checkRunYieldTriggerApplies),
// returning how many runs it fired. probe is the re-probe (the A3.4
// predicate, never forced); each runs on its own admission-bounded context
// on a goroutine tracked by bgMergeQueuePump. Only action completed with a
// check-run id, name, head_sha and repository is considered.
func (s *Server) fireCheckRunYieldTrigger(ctx context.Context, raw []byte, t *checkRunYieldTrigger, probe func(context.Context, uuid.UUID)) int {
	if s.cfg.MergeQueue == nil || s.cfg.RunRepo == nil {
		return 0
	}
	var p checkRunPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return 0 // ingestCheckRun logs the parse failure
	}
	if p.Action != "completed" || p.CheckRun.ID == 0 || p.CheckRun.Name == "" ||
		p.CheckRun.HeadSHA == "" || p.Repository.FullName == "" {
		return 0
	}
	base := context.WithoutCancel(ctx)
	fired := 0
	done := map[uuid.UUID]bool{}
	for _, pr := range p.CheckRun.PullRequests {
		prURL := fmt.Sprintf("https://github.com/%s/pull/%d", p.Repository.FullName, pr.Number)
		target := s.findRunByPullRequestURL(ctx, prURL, "check_run")
		if target == nil || done[target.ID] {
			continue
		}
		done[target.ID] = true
		st, found, err := s.cfg.MergeQueue.ActiveForRun(ctx, target.ID)
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"check_run: merge queue read failed; the yield sweep re-probes on its schedule",
				slog.String("run_id", target.ID.String()), slog.String("error", err.Error()))
			continue
		}
		if !found || !checkRunYieldTriggerApplies(target, st.Entry, p.CheckRun.Name, p.CheckRun.HeadSHA) {
			continue
		}
		runID := target.ID
		res := t.fire(runID, p.CheckRun.ID, func() {
			pctx, cancel := mergeCandidateAdmissionContext(base)
			defer cancel()
			probe(pctx, runID)
		}, s.spawnMergeQueueYieldProbe)
		fired++
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"check_run: merge queue yield re-probe triggered",
			slog.String("run_id", runID.String()),
			slog.String("check_name", p.CheckRun.Name),
			slog.Int64("check_run_id", p.CheckRun.ID),
			slog.String("result", string(res)))
	}
	return fired
}

// checkRunYieldTriggerApplies reports whether a completed check run named
// name at head re-probes runRow's queue entry e: e must be a held, yielded,
// intent-bearing entry anchored at head, and name must be one of the run's
// required-check contexts, or, when the snapshot is nil or empty, any
// observed name. This is the trigger filter only; it never changes how A3.1
// classifies the checks (the re-probe re-reads them).
func checkRunYieldTriggerApplies(runRow *run.Run, e mergequeue.Entry, name, head string) bool {
	if e.State != mergequeue.StateHeld || !e.Yielded() || e.MergeIntentAt == nil || e.AnchoredHeadSHA != head {
		return false
	}
	snap := runRow.RequiredChecksSnapshot
	if snap == nil || len(snap.Contexts) == 0 {
		return true
	}
	return slices.Contains(snap.Contexts, name)
}

// spawnMergeQueueYieldProbe runs fn on a goroutine tracked by
// bgMergeQueuePump, so Server.Shutdown drains an in-flight probe.
func (s *Server) spawnMergeQueueYieldProbe(fn func()) {
	s.bgMergeQueuePump.Add(1)
	go func() {
		defer s.bgMergeQueuePump.Done()
		fn()
	}()
}
