// Package sla parses workflow-spec gate SLA strings into
// wall-clock durations and runs the background ticker that
// transitions awaiting_approval stages to failed-D when their SLA
// elapses.
//
// v0 punts business-hours math: "4_business_hours" is treated as
// 4 wall-clock hours. The string is preserved verbatim on the
// stage row so a v0.x parser can swap in real business-hours
// math without a schema change.
//
// Supported unit suffixes: hours, business_hours (= hours in v0),
// minutes, days. Days are 24 wall-clock hours each.
package sla

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryApprovalSLAElapsed is the audit-log category for the
// chained entry the ticker writes when a stage times out. Static
// so log scrapers and the compliance export can index on it.
const CategoryApprovalSLAElapsed = "approval_sla_elapsed"

// ErrUnknownUnit is returned by Parse when the SLA string carries
// an unrecognized unit suffix. Callers (the ticker, the dispatcher)
// log the malformed value and skip the SLA rather than failing the
// whole stage — a typo in the workflow spec shouldn't black-hole
// a run.
var ErrUnknownUnit = errors.New("sla: unknown unit")

// Parse converts a workflow-spec SLA string into a wall-clock
// duration. Empty input returns (0, nil) — callers treat zero as
// "no SLA configured."
//
// Format: "<number>_<unit>" where unit is one of
//
//	hours | business_hours | minutes | days
//
// "business_hours" is currently aliased to "hours" — see package
// doc.
func Parse(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	parts := strings.SplitN(s, "_", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("sla: %q is not <number>_<unit>", s)
	}
	n, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, fmt.Errorf("sla: %q: parse number: %w", s, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("sla: %q: number must be > 0", s)
	}
	switch parts[1] {
	case "hours", "hour", "business_hours", "business_hour":
		return time.Duration(n) * time.Hour, nil
	case "minutes", "minute":
		return time.Duration(n) * time.Minute, nil
	case "days", "day":
		return time.Duration(n) * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrUnknownUnit, parts[1])
	}
}

// Ticker scans for awaiting_approval stages whose SLA has elapsed
// and transitions them to failed with category D. Run() blocks
// until ctx is done; for production wiring start it on its own
// goroutine via the server config.
type Ticker struct {
	// Repo persists stages and applies the failed-D transition.
	// Required.
	Repo run.Repository

	// Audit appends the approval_sla_elapsed entry per timeout.
	// Required.
	Audit audit.Repository

	// Logger receives structured warnings about parse failures and
	// transient transition errors. nil → slog.Default().
	Logger *slog.Logger

	// Interval is the tick period. Defaults to 60s when zero. The
	// ticker fires immediately on Run() start; the interval gates
	// subsequent ticks.
	Interval time.Duration

	// Now is the clock used for timeout computation. Tests inject
	// a fake clock; production leaves it nil for time.Now.
	Now func() time.Time
}

// Run drives the ticker until ctx is cancelled. Each tick lists
// awaiting_approval stages, computes their per-row deadline, and
// transitions any that have elapsed. Errors at the per-stage level
// are logged but don't abort the loop — a stuck stage shouldn't
// block timeouts on others.
func (t *Ticker) Run(ctx context.Context) error {
	if t.Repo == nil {
		return errors.New("sla: ticker requires Repo")
	}
	if t.Audit == nil {
		return errors.New("sla: ticker requires Audit")
	}
	logger := t.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := t.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	interval := t.Interval
	if interval <= 0 {
		interval = 60 * time.Second
	}

	timer := time.NewTicker(interval)
	defer timer.Stop()

	// Fire once immediately so a startup-race deployment doesn't
	// wait the full interval before catching its first elapsed
	// stage. Subsequent ticks are gated by interval.
	t.tick(ctx, logger, now)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			t.tick(ctx, logger, now)
		}
	}
}

// Tick performs a single scan + transition pass. Exposed for tests
// (so they can drive ticks deterministically without spinning up
// the goroutine) and for one-shot CLI use.
func (t *Ticker) Tick(ctx context.Context) {
	logger := t.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := t.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	t.tick(ctx, logger, now)
}

func (t *Ticker) tick(ctx context.Context, logger *slog.Logger, now func() time.Time) {
	stages, err := t.Repo.ListStagesAwaitingApproval(ctx)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "sla: list awaiting_approval failed",
			slog.String("error", err.Error()),
		)
		return
	}
	current := now()
	for _, s := range stages {
		t.handleStage(ctx, logger, current, s)
	}
}

// handleStage parses the stage's SLA, computes the deadline, and
// fails the stage if the deadline has passed. Per-stage errors are
// logged but don't propagate — one bad row shouldn't black-hole
// the rest of the scan.
func (t *Ticker) handleStage(ctx context.Context, logger *slog.Logger, now time.Time, s *run.Stage) {
	if s.GateSLA == nil || *s.GateSLA == "" {
		return
	}
	d, err := Parse(*s.GateSLA)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "sla: parse failed",
			slog.String("stage_id", s.ID.String()),
			slog.String("sla", *s.GateSLA),
			slog.String("error", err.Error()),
		)
		return
	}
	if d == 0 {
		return
	}
	// SLA clock starts when the stage entered awaiting_approval,
	// approximated by UpdatedAt. Trace handler walks dispatched →
	// running → awaiting_approval atomically; subsequent updates
	// only happen on this transition we're about to write.
	deadline := s.UpdatedAt.Add(d)
	if now.Before(deadline) {
		return
	}
	elapsed := now.Sub(s.UpdatedAt)
	reason := fmt.Sprintf("sla_timeout: %s elapsed (deadline %s)", elapsed.Round(time.Second), d)
	if _, err := run.FailStage(ctx, t.Repo, s.ID, run.FailureD, reason); err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "sla: transition failed",
			slog.String("stage_id", s.ID.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	payload, _ := json.Marshal(map[string]any{
		"stage_id":         s.ID.String(),
		"sla":              *s.GateSLA,
		"sla_seconds":      int64(d.Seconds()),
		"elapsed_seconds":  int64(elapsed.Seconds()),
		"updated_at":       s.UpdatedAt.UTC().Format(time.RFC3339Nano),
		"transitioned_at":  now.UTC().Format(time.RFC3339Nano),
		"failure_category": string(run.FailureD),
	})
	stageID := s.ID
	systemKind := audit.ActorSystem
	if _, err := t.Audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     s.RunID,
		StageID:   &stageID,
		Timestamp: now.UTC(),
		Category:  CategoryApprovalSLAElapsed,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		// State is already failed; surface the audit gap loudly
		// but don't unwind — a missing audit row is a regression
		// signal, not a reason to keep the stage hanging.
		logger.LogAttrs(ctx, slog.LevelError, "sla: append audit failed",
			slog.String("stage_id", s.ID.String()),
			slog.String("error", err.Error()),
		)
		return
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "sla: stage timed out",
		slog.String("stage_id", s.ID.String()),
		slog.String("sla", *s.GateSLA),
		slog.Duration("elapsed", elapsed),
	)
}
