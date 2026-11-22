package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ScanUndispatchedChildStalls pages each decomposition child whose implement
// stage is dispatchable but has sat undispatched, with no operator or driver
// act, past p.Threshold (#5497). The row is written on the CHILD's chain and
// names fishhawk_run_children on the parent. The watch never dispatches.
//
// A child qualifies only when ALL of these hold:
//
//   - its run is non-terminal and its implement stage is pending or
//     awaiting_host_dispatch (issuecomment.ChildDispatchStallEpisode ok);
//   - it is not queued for a local concurrency slot with a live waiter (that
//     waiter spawns it on admission);
//   - the host-dispatch marker's wave-order and dependency-coverage guards
//     would admit it: the SAME guardDecompositionWaveOrder and
//     resolveDependentChildBase calls host_dispatch.go makes, so a page never
//     announces a child those two guards refuse. Both are read-only: they list
//     runs, the parent plan and the parent's slices_integrated rows, and write
//     nothing (pinned by TestScanUndispatchedChildStalls_GuardsStayReadOnly);
//   - it is not held behind a live fan-out's concurrency cap: with the
//     parent's newest plan_decomposed effective_max_parallel > 0, the
//     in-flight siblings (see childStallInFlight) must be below the cap.
//
// It mirrors only those marker guards plus the host memory hold, not every
// marker refusal: the campaign dispatch hold, the deploy- and
// failed-stage-ahead holds and the fishhawk_run_children max_parallel
// override are not read, so such a child can still page (documented in
// backend/internal/stallwatch/README.md).
//
// While Config.HostAdmissionHold reports held, the marker refuses every new
// admission, so the scan pages no child at all (M2).
//
// The quiet clock's base is the latest of the stage's UpdatedAt, the child's
// CreatedAt, the parent's newest slices_integrated row and the newest
// UpdatedAt of a sibling run that reached a terminal state (a freed slot).
// quietSince then folds in the newest user- or agent-kind row on BOTH the
// child's and the parent's chains.
//
// A read error on one child WARN-logs and skips only that child. A ListRuns
// error fails the scan.
func (s *Server) ScanUndispatchedChildStalls(ctx context.Context, p StallScanParams) (StallScanReport, error) {
	var rep StallScanReport
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return rep, errStallWatchUnwired
	}
	if err := p.validate(); err != nil {
		return rep, err
	}
	if s.cfg.HostAdmissionHold != nil {
		if held, reason := s.cfg.HostAdmissionHold.Held(p.Now); held {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
				"stall watch: host admission hold is held, so the marker refuses every child; no child page this tick",
				slog.String("reason", reason))
			return rep, nil
		}
	}
	var children []*run.Run
	for off := 0; ; off += stallScanPageSize {
		page, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{NonTerminal: true, Limit: stallScanPageSize, Offset: off})
		if err != nil {
			return rep, fmt.Errorf("stall watch: list non-terminal runs: %w", err)
		}
		for _, r := range page {
			if r.DecomposedFrom != nil {
				children = append(children, r)
			}
		}
		if len(page) < stallScanPageSize {
			break
		}
	}
	for _, c := range children {
		rep.Scanned++
		paged, err := s.scanUndispatchedChildStall(ctx, c, p)
		if err != nil {
			rep.Skipped++
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"stall watch: child-dispatch scan of one child failed; skipping it",
				slog.String("run_id", c.ID.String()), slog.String("error", err.Error()))
			continue
		}
		if paged {
			rep.Paged++
		}
	}
	return rep, nil
}

// childImplementStage returns the highest-sequence implement stage, or nil.
func childImplementStage(stages []*run.Stage) *run.Stage {
	var out *run.Stage
	for _, st := range stages {
		if st == nil || st.Type != run.StageTypeImplement {
			continue
		}
		if out == nil || st.Sequence > out.Sequence {
			out = st
		}
	}
	return out
}

// childStallSibling is one sibling's implement stage as the cap and clock
// rules read it.
type childStallSibling struct {
	run   *run.Run
	stage *run.Stage
}

// childStallInFlight reports whether a NON-TERMINAL sibling occupies a slot
// of the fan-out cap: its implement stage has left pending /
// awaiting_host_dispatch without reaching a terminal state (a dispatch is
// held), or it is still pending / awaiting_host_dispatch but queued for a
// concurrency slot with a live waiter, which fishhawk_run_children counts as
// in flight too (run_children.go, M3). An undispatched sibling without a live
// waiter deliberately does not count, so children that are all stuck still
// page.
func childStallInFlight(st *run.Stage, liveWaiter bool) bool {
	if st == nil || st.State.IsTerminal() {
		return false
	}
	if st.State == run.StageStatePending || st.State == run.StageStateAwaitingHostDispatch {
		return liveWaiter
	}
	return true
}

// stallLiveWaiters reports, per stage id, whether the stage is queued for a
// concurrency slot with a live waiter (childQueuedWithLiveWaiter's predicate
// over the store's own Status). A nil store reports none. A read error is
// returned so the caller skips the child rather than paging one a live waiter
// may be about to spawn.
func (s *Server) stallLiveWaiters(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if s.cfg.Concurrency == nil || len(ids) == 0 {
		return out, nil
	}
	statuses, err := s.cfg.Concurrency.StatusForStages(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("read concurrency status: %w", err)
	}
	for id, st := range statuses {
		out[id] = st.State != concurrency.SlotHeld && st.WaiterLive
	}
	return out, nil
}

// latestEffectiveMaxParallel reads effective_max_parallel from the newest
// plan_decomposed row on the parent's chain. 0 (absent, undecodable or no
// row) is unlimited.
func latestEffectiveMaxParallel(parentEntries []*audit.Entry) int {
	var newest *audit.Entry
	for _, e := range parentEntries {
		if e != nil && e.Category == "plan_decomposed" && (newest == nil || e.Sequence > newest.Sequence) {
			newest = e
		}
	}
	if newest == nil {
		return 0
	}
	var p struct {
		EffectiveMaxParallel int `json:"effective_max_parallel"`
	}
	if json.Unmarshal(newest.Payload, &p) != nil || p.EffectiveMaxParallel < 0 {
		return 0
	}
	return p.EffectiveMaxParallel
}

// latestCategoryTime returns the timestamp of the newest row of category.
func latestCategoryTime(entries []*audit.Entry, category string) time.Time {
	var t time.Time
	for _, e := range entries {
		if e != nil && e.Category == category && e.Timestamp.After(t) {
			t = e.Timestamp
		}
	}
	return t
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// scanUndispatchedChildStall evaluates one child and appends its page when
// every condition holds. It returns paged=true only when a row landed.
func (s *Server) scanUndispatchedChildStall(ctx context.Context, child *run.Run, p StallScanParams) (bool, error) {
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, child.ID)
	if err != nil {
		return false, fmt.Errorf("list stages: %w", err)
	}
	stage := childImplementStage(stages)
	episode, ok := issuecomment.ChildDispatchStallEpisode(stage)
	if !ok {
		return false, nil
	}
	parentID := *child.DecomposedFrom

	// The marker's wave-order and coverage guards (read-only).
	if blocked, err := s.guardDecompositionWaveOrder(ctx, child); err != nil {
		return false, fmt.Errorf("wave-order guard: %w", err)
	} else if blocked != nil {
		return false, nil
	}
	if _, uncovered, err := s.resolveDependentChildBase(ctx, child); err != nil {
		return false, fmt.Errorf("dependency coverage: %w", err)
	} else if uncovered != nil {
		return false, nil
	}

	// Siblings: their implement stages for the cap, their terminal
	// UpdatedAt for the clock.
	runs, err := s.listDecomposedSiblings(ctx, parentID)
	if err != nil {
		return false, fmt.Errorf("list siblings: %w", err)
	}
	var siblings []childStallSibling
	ids := []uuid.UUID{stage.ID}
	var freedAt time.Time
	for _, sib := range runs {
		if sib.ID == child.ID {
			continue
		}
		if sib.State.IsTerminal() {
			freedAt = laterOf(freedAt, sib.UpdatedAt)
			continue
		}
		sts, err := s.cfg.RunRepo.ListStagesForRun(ctx, sib.ID)
		if err != nil {
			return false, fmt.Errorf("list sibling %s stages: %w", sib.ID, err)
		}
		st := childImplementStage(sts)
		if st == nil {
			continue
		}
		siblings = append(siblings, childStallSibling{run: sib, stage: st})
		ids = append(ids, st.ID)
	}
	live, err := s.stallLiveWaiters(ctx, ids)
	if err != nil {
		return false, err
	}
	if live[stage.ID] {
		return false, nil
	}

	parentEntries, err := s.cfg.AuditRepo.ListForRun(ctx, parentID)
	if err != nil {
		return false, fmt.Errorf("list parent audit: %w", err)
	}
	capN := latestEffectiveMaxParallel(parentEntries)
	inFlight := 0
	for _, sib := range siblings {
		if childStallInFlight(sib.stage, live[sib.stage.ID]) {
			inFlight++
		}
	}
	if capN > 0 && inFlight >= capN {
		return false, nil
	}

	childEntries, err := s.cfg.AuditRepo.ListForRun(ctx, child.ID)
	if err != nil {
		return false, fmt.Errorf("list audit: %w", err)
	}
	base := laterOf(stage.UpdatedAt, child.CreatedAt)
	base = laterOf(base, latestCategoryTime(parentEntries, "slices_integrated"))
	base = laterOf(base, freedAt)
	qs := quietSince(base, childEntries, parentEntries)
	if !stallQuietPastThreshold(qs, p.Now, p.Threshold) {
		return false, nil
	}

	var sliceIndex any
	if child.SliceIndex != nil {
		sliceIndex = *child.SliceIndex
	}
	payload, _ := json.Marshal(map[string]any{
		"stage_id":               stage.ID.String(),
		"child_run_id":           child.ID.String(),
		"parent_run_id":          parentID.String(),
		"slice_index":            sliceIndex,
		"stage_state":            string(stage.State),
		"auto_dispatch":          child.AutoDispatch,
		"effective_max_parallel": capN,
		"in_flight_siblings":     inFlight,
		"next_action":            StallNextActionRunChildren,
		"next_action_target":     parentID.String(),
		"quiet_since":            qs.UTC().Format(time.RFC3339Nano),
		"quiet_seconds":          int64(p.Now.Sub(qs) / time.Second),
		"threshold_seconds":      int64(p.Threshold / time.Second),
		"episode_key":            episode,
		"reason":                 "the decomposition child's implement stage is dispatchable but undispatched past the stall threshold with no operator or driver act; fishhawk_run_children on the parent dispatches it",
	})
	return s.appendStallPage(ctx, child.ID, stage.ID, CategoryChildDispatchAwaitingOperator, payload, episode)
}
