package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Stall watch (#5497). A page-only server-side scan that pages the operator
// when a failed stage, or a dispatchable decomposition child, has seen no
// operator or driver act for a threshold. It writes one chained, stage-scoped
// system audit row per condition EPISODE; issuecomment.pageClassEvents projects
// the row to a page kind and notifyPageClass fires it at once.
//
// The watch only pages. It holds no token identity, calls no retry, revive,
// resume or dispatch path, writes no stage_retried or run_auto_driven row and
// evaluates no delegation or campaign override. The delegated auto-retry of
// #5497 is deliberately out of scope pending an operator decision.

// Stall-watch audit categories (#5497). Both are open-set strings (no
// migration, no CHECK) registered in audit.KnownCategories, and both are
// pings-only page-class markers: no activityCategories entry and no Notifier
// method. issuecomment duplicates the literals because it cannot import this
// package.
const (
	// CategoryStageFailedAwaitingOperator is written on the failed stage's own
	// run chain when it has seen no operator or driver act past the threshold.
	CategoryStageFailedAwaitingOperator = "stage_failed_awaiting_operator"
	// CategoryChildDispatchAwaitingOperator is written on a decomposition
	// child's chain when its implement stage is dispatchable but undispatched
	// past the threshold (server/stall_watch_children.go).
	CategoryChildDispatchAwaitingOperator = "child_dispatch_awaiting_operator"
)

// The closed next-action set a stall row may name. Each is the one legal
// operator remedy for the paged condition; the watch never takes it itself.
const (
	StallNextActionRetryStage  = "fishhawk_retry_stage"
	StallNextActionReviveRun   = "fishhawk_revive_run"
	StallNextActionResumeRun   = "fishhawk_resume_run"
	StallNextActionRunChildren = "fishhawk_run_children"
)

// StallWatchActorSubject is the actor subject on every stall row (ActorKind
// system).
const StallWatchActorSubject = "system:stall-watch"

// stallScanPageSize is the ListRuns page size for the candidate scans.
const stallScanPageSize = 100

// errStallWatchUnwired is returned when the run or audit repository is nil.
var errStallWatchUnwired = errors.New("stall watch: run and audit repositories are required")

// StallScanParams are one scan's inputs. Threshold is the quiet period a
// condition must reach before it pages. Lookback bounds the failed-run scan:
// a terminal-failed run is a candidate only when it was created, and its
// stage failed, within Lookback of Now. Now is the scan's clock.
type StallScanParams struct {
	Threshold time.Duration
	Lookback  time.Duration
	Now       time.Time
}

// validate refuses a degenerate scan: a non-positive threshold (which would
// page every failure at once), a lookback shorter than the threshold (which
// could never page a failed run) or a zero clock.
func (p StallScanParams) validate() error {
	switch {
	case p.Threshold <= 0:
		return fmt.Errorf("stall watch: threshold must be positive, got %s", p.Threshold)
	case p.Lookback < p.Threshold:
		return fmt.Errorf("stall watch: lookback %s must be at least the threshold %s", p.Lookback, p.Threshold)
	case p.Now.IsZero():
		return errors.New("stall watch: scan clock (Now) is required")
	}
	return nil
}

// StallScanReport counts one scan: Scanned candidates examined, Paged rows
// appended and Skipped candidates whose reads failed.
type StallScanReport struct {
	Scanned int
	Paged   int
	Skipped int
}

// stallQuietPastThreshold is the ONE threshold predicate both scanners share.
func stallQuietPastThreshold(quietSince, now time.Time, threshold time.Duration) bool {
	return now.Sub(quietSince) >= threshold
}

// quietSince is the operator/driver-act clock: the later of base and the
// timestamp of the newest user- or agent-kind entry across chains. The two
// stall categories are skipped (they are system rows, so this is belt and
// braces). Any user- or agent-kind row restarts the clock, whatever it is.
func quietSince(base time.Time, chains ...[]*audit.Entry) time.Time {
	q := base
	for _, entries := range chains {
		for _, e := range entries {
			if e == nil || e.ActorKind == nil {
				continue
			}
			if *e.ActorKind != audit.ActorUser && *e.ActorKind != audit.ActorAgent {
				continue
			}
			if e.Category == CategoryStageFailedAwaitingOperator || e.Category == CategoryChildDispatchAwaitingOperator {
				continue
			}
			if e.Timestamp.After(q) {
				q = e.Timestamp
			}
		}
	}
	return q
}

// appendStallPage appends one stall row for (stageID, episodeKey) and, only
// when a row landed, fires the pings-only notifyPageClass. The dedup key is
// the episode: a later tick in the same episode writes nothing.
//
// When the audit repository implements audit.DedupedChainAppender (the
// Postgres repo) the scan runs under the run-row lock and a
// *DedupedDuplicateError is the benign already-paged branch. Otherwise it
// falls back to a non-atomic list-then-append over ListForRunByCategory; a
// list error there fails toward NO append, since an unguarded append would
// re-page the same episode on every tick. That fallback is single-instance
// only.
func (s *Server) appendStallPage(ctx context.Context, runID, stageID uuid.UUID, category string, payload []byte, episodeKey string) (bool, error) {
	kind := audit.ActorSystem
	subject := StallWatchActorSubject
	p := audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &stageID,
		Timestamp:    time.Now().UTC(),
		Category:     category,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	}
	spec := audit.DedupeSpec{StageID: &stageID, PayloadKey: "episode_key", PayloadValue: episodeKey}
	if d, ok := s.cfg.AuditRepo.(audit.DedupedChainAppender); ok {
		_, err := d.AppendChainedDeduped(ctx, p, spec)
		var dup *audit.DedupedDuplicateError
		if errors.As(err, &dup) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	} else {
		existing, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, category)
		if err != nil {
			return false, fmt.Errorf("stall watch: list %s rows: %w", category, err)
		}
		var dup *audit.DedupedDuplicateError
		if err := audit.CheckDedupeSpec(existing, &p, spec); errors.As(err, &dup) {
			return false, nil
		}
		if _, err := s.cfg.AuditRepo.AppendChained(ctx, p); err != nil {
			return false, err
		}
	}
	s.notifyPageClass(ctx, runID, "stall_watch")
	return true, nil
}

// ScanFailedStageStalls pages each failed stage that has seen no operator or
// driver act past p.Threshold (#5497).
//
// Candidates are every non-terminal run, plus terminal-failed runs in
// ListRuns' created_at DESC order, stopping at the first one created before
// Now-Lookback. ListRuns has no updated_at order or filter, so a failed run
// created before the window whose stage failed inside it (a revived week-old
// run) is never paged: a documented residual. A failed candidate is then
// qualified on its FAILURE time, the failed stage's EndedAt. A non-terminal
// run is not bounded by Lookback: an open run is exactly what the watch is
// for, and the non-terminal set is bounded on its own.
//
// Per run it classifies the HIGHEST-sequence failed stage carrying a
// FailureCategory and maps it to the one legal remedy:
//
//   - non-terminal run, retryable failure (run.RetryableFailure) →
//     fishhawk_retry_stage on the stage. A category-B or non-timeout D failure
//     gets no page: retry_stage would refuse it.
//   - failed run, every failed stage retryable (ReviveRun's own pre-validate)
//     → fishhawk_revive_run.
//   - failed decomposition child under a non-terminal parent, every failed
//     stage run.RecoverableInDecomposition (the category-B case) →
//     fishhawk_resume_run.
//   - anything else (a failed top-level B or D run) → no page.
//
// Exclusions: a decomposed parent's IMPLEMENT failure (its children's pages
// cover it; mirrors run.ClassifyInfraFailurePark's implement-only guard, so a
// parent's acceptance failure still pages); a spent infra-park budget; and a
// failed run superseded by a newer non-slice run on the same trigger_ref or by
// a recovery descendant.
//
// A read error on one run WARN-logs and skips only that run. A ListRuns error
// fails the scan.
func (s *Server) ScanFailedStageStalls(ctx context.Context, p StallScanParams) (StallScanReport, error) {
	var rep StallScanReport
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return rep, errStallWatchUnwired
	}
	if err := p.validate(); err != nil {
		return rep, err
	}
	candidates, err := s.failedStageStallCandidates(ctx, p)
	if err != nil {
		return rep, err
	}
	for _, r := range candidates {
		rep.Scanned++
		paged, err := s.scanFailedStageStall(ctx, r, p)
		if err != nil {
			rep.Skipped++
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"stall watch: failed-stage scan of one run failed; skipping it",
				slog.String("run_id", r.ID.String()), slog.String("error", err.Error()))
			continue
		}
		if paged {
			rep.Paged++
		}
	}
	return rep, nil
}

// failedStageStallCandidates lists the non-terminal runs and the failed runs
// created within Lookback of Now.
func (s *Server) failedStageStallCandidates(ctx context.Context, p StallScanParams) ([]*run.Run, error) {
	var out []*run.Run
	for off := 0; ; off += stallScanPageSize {
		page, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{NonTerminal: true, Limit: stallScanPageSize, Offset: off})
		if err != nil {
			return nil, fmt.Errorf("stall watch: list non-terminal runs: %w", err)
		}
		out = append(out, page...)
		if len(page) < stallScanPageSize {
			break
		}
	}
	cutoff := p.Now.Add(-p.Lookback)
	for off := 0; ; off += stallScanPageSize {
		page, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{State: string(run.StateFailed), Limit: stallScanPageSize, Offset: off})
		if err != nil {
			return nil, fmt.Errorf("stall watch: list failed runs: %w", err)
		}
		for _, r := range page {
			if r.CreatedAt.Before(cutoff) {
				return out, nil
			}
			out = append(out, r)
		}
		if len(page) < stallScanPageSize {
			break
		}
	}
	return out, nil
}

// newestFailedStage returns the highest-sequence failed stage carrying a
// FailureCategory, or nil.
func newestFailedStage(stages []*run.Stage) *run.Stage {
	var out *run.Stage
	for _, st := range stages {
		if st == nil || st.State != run.StageStateFailed || st.FailureCategory == nil {
			continue
		}
		if out == nil || st.Sequence > out.Sequence {
			out = st
		}
	}
	return out
}

// allFailedStagesMatch reports whether every failed stage carries a
// FailureCategory satisfying pred, and at least one failed stage exists. A
// failed stage with no category fails the check, as in ReviveRun.
func allFailedStagesMatch(stages []*run.Stage, pred func(run.FailureCategory, string) bool) bool {
	n := 0
	for _, st := range stages {
		if st == nil || st.State != run.StageStateFailed {
			continue
		}
		n++
		if st.FailureCategory == nil || !pred(*st.FailureCategory, failureReasonOf(st)) {
			return false
		}
	}
	return n > 0
}

func failureReasonOf(st *run.Stage) string {
	if st == nil || st.FailureReason == nil {
		return ""
	}
	return *st.FailureReason
}

// scanFailedStageStall evaluates one candidate run and appends its page when
// every condition holds. It returns paged=true only when a row landed.
func (s *Server) scanFailedStageStall(ctx context.Context, r *run.Run, p StallScanParams) (bool, error) {
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, r.ID)
	if err != nil {
		return false, fmt.Errorf("list stages: %w", err)
	}
	failed := newestFailedStage(stages)
	if failed == nil {
		return false, nil
	}
	episode, ok := issuecomment.FailedStageStallEpisode(failed)
	if !ok {
		// No EndedAt: no failure time to clock or key the episode on.
		return false, nil
	}
	failedAt := *failed.EndedAt
	if r.State == run.StateFailed && p.Now.Sub(failedAt) > p.Lookback {
		return false, nil
	}
	entries, err := s.cfg.AuditRepo.ListForRun(ctx, r.ID)
	if err != nil {
		return false, fmt.Errorf("list audit: %w", err)
	}
	h := run.ReadInfraParkHistory(entries, failed)
	if failed.Type == run.StageTypeImplement && h.DecomposedParent {
		return false, nil
	}
	if run.ClassifyInfraFailurePark(stages, failed, h) == run.InfraParkBudgetExhausted {
		return false, nil
	}
	action, target, ok, err := s.failedStageStallNextAction(ctx, r, stages, failed)
	if err != nil || !ok {
		return false, err
	}
	qs := quietSince(failedAt, entries)
	if !stallQuietPastThreshold(qs, p.Now, p.Threshold) {
		return false, nil
	}
	if r.State == run.StateFailed {
		superseded, err := s.failedRunSuperseded(ctx, r)
		if err != nil || superseded {
			return false, err
		}
	}
	payload, _ := json.Marshal(map[string]any{
		"stage_id":           failed.ID.String(),
		"stage_type":         string(failed.Type),
		"failure_category":   string(*failed.FailureCategory),
		"run_state":          string(r.State),
		"next_action":        action,
		"next_action_target": target.String(),
		"quiet_since":        qs.UTC().Format(time.RFC3339Nano),
		"quiet_seconds":      int64(p.Now.Sub(qs) / time.Second),
		"threshold_seconds":  int64(p.Threshold / time.Second),
		"episode_key":        episode,
		"reason":             stallFailedStageReason(action),
	})
	return s.appendStallPage(ctx, r.ID, failed.ID, CategoryStageFailedAwaitingOperator, payload, episode)
}

// failedStageStallNextAction maps a failed stage to its one legal remedy and
// target (the stage id for retry_stage, the run id otherwise). ok=false means
// no remedy the watch may name, so no page.
func (s *Server) failedStageStallNextAction(ctx context.Context, r *run.Run, stages []*run.Stage, failed *run.Stage) (string, uuid.UUID, bool, error) {
	if !r.State.IsTerminal() {
		if !run.RetryableFailure(*failed.FailureCategory, failureReasonOf(failed)) {
			return "", uuid.Nil, false, nil
		}
		return StallNextActionRetryStage, failed.ID, true, nil
	}
	if r.State != run.StateFailed {
		// Cancelled and succeeded runs are never candidates; no remedy.
		return "", uuid.Nil, false, nil
	}
	if allFailedStagesMatch(stages, run.RetryableFailure) {
		return StallNextActionReviveRun, r.ID, true, nil
	}
	if r.DecomposedFrom != nil && allFailedStagesMatch(stages, run.RecoverableInDecomposition) {
		parent, err := s.cfg.RunRepo.GetRun(ctx, *r.DecomposedFrom)
		if err != nil {
			return "", uuid.Nil, false, fmt.Errorf("get decomposition parent: %w", err)
		}
		if parent.State.IsTerminal() {
			return "", uuid.Nil, false, nil
		}
		return StallNextActionResumeRun, r.ID, true, nil
	}
	return "", uuid.Nil, false, nil
}

// failedRunSuperseded reports whether a failed run has been superseded: a
// recovery descendant exists (a run with parent_run_id = this run and no
// decomposed_from, #2549), or a newer run on the same repo and trigger_ref
// that is not a decomposition slice exists. Slices inherit their parent's
// trigger_ref, so they are never read as superseding a run.
func (s *Server) failedRunSuperseded(ctx context.Context, r *run.Run) (bool, error) {
	parentID := r.ID
	for off := 0; ; off += stallScanPageSize {
		kids, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{ParentRunID: &parentID, Limit: stallScanPageSize, Offset: off})
		if err != nil {
			return false, fmt.Errorf("list recovery descendants: %w", err)
		}
		for _, k := range kids {
			if k.ID != r.ID && k.DecomposedFrom == nil {
				return true, nil
			}
		}
		if len(kids) < stallScanPageSize {
			break
		}
	}
	if r.TriggerRef == nil || *r.TriggerRef == "" {
		return false, nil
	}
	for off := 0; ; off += stallScanPageSize {
		page, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{Repo: r.Repo, TriggerRef: r.TriggerRef, Limit: stallScanPageSize, Offset: off})
		if err != nil {
			return false, fmt.Errorf("list runs on trigger ref: %w", err)
		}
		for _, o := range page {
			if o.ID == r.ID || o.DecomposedFrom != nil {
				continue
			}
			if !o.CreatedAt.After(r.CreatedAt) {
				// created_at DESC: nothing later in the listing is newer.
				return false, nil
			}
			return true, nil
		}
		if len(page) < stallScanPageSize {
			break
		}
	}
	return false, nil
}

// stallFailedStageReason is the server-written reason for a failed-stage page.
func stallFailedStageReason(action string) string {
	switch action {
	case StallNextActionRetryStage:
		return "the failed stage has seen no operator or driver act past the stall threshold; fishhawk_retry_stage re-opens it"
	case StallNextActionReviveRun:
		return "the failed run has seen no operator or driver act past the stall threshold; every failed stage is retryable, so fishhawk_revive_run re-parks them"
	default:
		return "the failed decomposition child has seen no operator or driver act past the stall threshold; fishhawk_resume_run re-drives it in place under its live parent"
	}
}
