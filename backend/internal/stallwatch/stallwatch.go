// Package stallwatch runs the background ticker that pages the operator when a
// failed stage, or a dispatchable decomposition child, has seen no operator or
// driver act past a threshold (#5497).
//
// The ticker only schedules. Both scanners live in the server package
// (Server.ScanFailedStageStalls and Server.ScanUndispatchedChildStalls), which
// write the stall audit rows and fire the page; this package does not import
// server, and fishhawkd adapts the two methods to the Scanner signature. The
// watch never retries, revives, resumes or dispatches anything.
//
// Mirrors the dispatchwatchdog.Ticker pattern. See README.md for both
// conditions, every exclusion and the residuals.
package stallwatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Defaults fishhawkd's --stall-watch-* flags use.
const (
	DefaultThreshold = 30 * time.Minute
	DefaultInterval  = 5 * time.Minute
	DefaultLookback  = 168 * time.Hour
)

// Params are one scan's inputs, passed to both scanners.
type Params struct {
	Threshold time.Duration
	Lookback  time.Duration
	Now       time.Time
}

// Report counts one scan: candidates examined, rows appended and candidates
// skipped on a read error.
type Report struct {
	Scanned int
	Paged   int
	Skipped int
}

// Scanner is one stall scan. An error fails that scan only.
type Scanner func(ctx context.Context, p Params) (Report, error)

// Ticker runs both scanners immediately and then every Interval.
type Ticker struct {
	// FailedStages pages failed stages. Required.
	FailedStages Scanner
	// Children pages undispatched decomposition children. Required.
	Children Scanner
	// Threshold is the quiet period a condition must reach. Required, > 0.
	Threshold time.Duration
	// Lookback bounds the failed-run scan. Must be >= Threshold.
	Lookback time.Duration
	// Interval is the tick period. Required, > 0.
	Interval time.Duration
	// Now is the scan clock; nil means time.Now.
	Now func() time.Time
	// Logger receives scan errors and panics; nil means slog.Default().
	Logger *slog.Logger
}

// Validate refuses a degenerate configuration: a nil scanner, a non-positive
// threshold or interval, or a lookback shorter than the threshold (which
// could never page a failed run). fishhawkd calls it at boot so a bad flag
// refuses to start rather than run a never- or always-firing watch.
func (t *Ticker) Validate() error {
	switch {
	case t.FailedStages == nil || t.Children == nil:
		return errors.New("stallwatch: both the failed-stage and the child scanner are required")
	case t.Threshold <= 0:
		return fmt.Errorf("stallwatch: threshold must be positive, got %s", t.Threshold)
	case t.Interval <= 0:
		return fmt.Errorf("stallwatch: interval must be positive, got %s", t.Interval)
	case t.Lookback < t.Threshold:
		return fmt.Errorf("stallwatch: lookback %s must be at least the threshold %s", t.Lookback, t.Threshold)
	}
	return nil
}

// Run validates, ticks once immediately, then every Interval until ctx is
// done. It returns the Validate error without ticking.
func (t *Ticker) Run(ctx context.Context) error {
	if err := t.Validate(); err != nil {
		return err
	}
	t.Tick(ctx)
	tick := time.NewTicker(t.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			t.Tick(ctx)
		}
	}
}

// Tick runs both scanners once with the same Params. Each runs under its own
// recover, and an error or panic in one WARN-logs without stopping the other.
func (t *Ticker) Tick(ctx context.Context) {
	now := time.Now().UTC()
	if t.Now != nil {
		now = t.Now().UTC()
	}
	p := Params{Threshold: t.Threshold, Lookback: t.Lookback, Now: now}
	t.runOne(ctx, "failed_stage", t.FailedStages, p)
	t.runOne(ctx, "child_dispatch", t.Children, p)
}

func (t *Ticker) runOne(ctx context.Context, name string, scan Scanner, p Params) {
	logger := t.Logger
	if logger == nil {
		logger = slog.Default()
	}
	defer func() {
		if r := recover(); r != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "stallwatch: scanner panicked; the other scanner still runs",
				slog.String("scanner", name), slog.String("panic", fmt.Sprint(r)))
		}
	}()
	if scan == nil {
		return
	}
	rep, err := scan(ctx, p)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "stallwatch: scan failed",
			slog.String("scanner", name), slog.String("error", err.Error()))
		return
	}
	if rep.Paged > 0 || rep.Skipped > 0 {
		logger.LogAttrs(ctx, slog.LevelInfo, "stallwatch: scan paged",
			slog.String("scanner", name), slog.Int("scanned", rep.Scanned),
			slog.Int("paged", rep.Paged), slog.Int("skipped", rep.Skipped))
	}
}
