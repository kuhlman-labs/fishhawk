package opsmonitor

// Scope-amendment probes (E85.2 / #4146). Both read ONE scan of the pending
// scope_amendments rows joined to their stage state
// (scopeamendment.PendingLister) and partition it with one shared
// classifier: a row on a LIVE stage is the window probe's, a row on a
// TERMINAL stage is the terminal probe's, never both.
//
// Relation to #4127 (undecided amendments lapse): the scan reads
// status='pending' rows only and classifyAmendments drops any non-pending
// row, so once the lapse step marks a row lapsed the terminal probe stops
// reporting it, and the two never double-report. The lapse step leaves a
// FAILED stage's requests pending on purpose (fishhawk_retry_stage reopens a
// failed run), so the terminal probe reports those, plus any row the lapse
// step missed. Contract: README.md § "Scope amendments".

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// AmendmentReader is the narrow pending-amendment scan the amendment probes
// need; scopeamendment.PendingLister satisfies it.
type AmendmentReader interface {
	ListPendingWithStage(ctx context.Context, lookback time.Duration, limit int) ([]scopeamendment.PendingWithStage, error)
}

// Amendment probe defaults. DefaultAmendmentWindow mirrors the implement
// agent's decision wait-poll (server.AmendmentPollWindowSeconds, pinned by a
// fishhawkd test). DefaultAmendmentWarnLead and DefaultAmendmentLookback are
// recommended defaults, not issue-stated numbers: the lookback bounds the
// scan so the pre-#4127 backlog of stale pending rows cannot burst.
const (
	DefaultAmendmentWindow   = 15 * time.Minute
	DefaultAmendmentWarnLead = 10 * time.Minute
	DefaultAmendmentLookback = 7 * 24 * time.Hour
	// amendmentScanLimit bounds one scan; a scan returning this many rows
	// is truncated (rows past it are unobserved).
	amendmentScanLimit = 200
)

// windowDue reports whether a pending amendment of this age is inside the
// warning lead of its decision window (strictly exceeds window-warnLead).
func windowDue(age, window, warnLead time.Duration) bool {
	return age > window-warnLead
}

// amendmentClasses is classifyAmendments' partition of one scan.
type amendmentClasses struct {
	live, terminal     []scopeamendment.PendingWithStage
	hidden, notPending int
}

// classifyAmendments partitions rows: a non-pending row is dropped (the
// second guard behind the query's pending-only filter), a row whose stage is
// not visible (row-level security) is hidden, and the rest split on whether
// the stage is terminal.
func classifyAmendments(rows []scopeamendment.PendingWithStage) amendmentClasses {
	var c amendmentClasses
	for _, row := range rows {
		if row.Status != scopeamendment.StatusPending {
			c.notPending++
			continue
		}
		if row.StageState == nil {
			c.hidden++
			continue
		}
		if run.StageState(*row.StageState).IsTerminal() {
			c.terminal = append(c.terminal, row)
			continue
		}
		c.live = append(c.live, row)
	}
	return c
}

// scanAmendments runs the shared scan. A nil reader or a scan error returns
// the probe's unknown result; truncated reports a full page.
func scanAmendments(ctx context.Context, probe string, reader AmendmentReader, lookback time.Duration) (rows []scopeamendment.PendingWithStage, truncated bool, failed *Result) {
	if reader == nil {
		r := unknown(probe, "no database configured")
		return nil, false, &r
	}
	rows, err := reader.ListPendingWithStage(ctx, lookback, amendmentScanLimit)
	if err != nil {
		r := unknown(probe, "scope-amendment scan failed: "+err.Error())
		return nil, false, &r
	}
	return rows, len(rows) >= amendmentScanLimit, nil
}

// amendmentOutcome assembles a probe's results. Failing results win over a
// hidden count, but a truncated scan ALWAYS adds its unknown result, alongside
// any failing ones, so the unobserved tail is never silent. With nothing
// failing, a hidden row or a truncation reads unknown, never ok.
func amendmentOutcome(probe string, failing []Result, c amendmentClasses, truncated bool, ok Result) []Result {
	var reasons []string
	if c.hidden > 0 {
		reasons = append(reasons, fmt.Sprintf("%d pending amendment(s) whose stage is not visible to this session (row-level security)", c.hidden))
	}
	if truncated {
		reasons = append(reasons, fmt.Sprintf("scan truncated at %d rows: pending amendments older than the newest %d are unobserved until newer ones settle", amendmentScanLimit, amendmentScanLimit))
	}
	if len(failing) > 0 {
		if truncated {
			return append(failing, unknown(probe, strings.Join(reasons, "; ")))
		}
		return failing
	}
	if len(reasons) > 0 {
		return []Result{unknown(probe, strings.Join(reasons, "; "))}
	}
	return []Result{ok}
}

func amendmentCounts(scanned int, c amendmentClasses) map[string]any {
	return map[string]any{
		"scanned":     scanned,
		"live":        len(c.live),
		"terminal":    len(c.terminal),
		"hidden":      c.hidden,
		"not_pending": c.notPending,
	}
}

func seconds(d time.Duration) int64 { return int64(d / time.Second) }

// --- amendment_window --------------------------------------------------------

// AmendmentWindowProbe reports a pending scope amendment on a NON-terminal
// stage whose age (the database's now() minus requested_at, one clock domain)
// exceeds Window minus WarnLead: the implement agent stops waiting for a
// decision once Window elapses. A non-positive Window or Lookback selects its
// default. WarnLead is used VERBATIM: a zero-value WarnLead means no lead, and
// the probe fires only once the age exceeds the window itself.
type AmendmentWindowProbe struct {
	// Amendments reads the scan; nil reads unknown (no database configured).
	Amendments AmendmentReader
	Window     time.Duration
	WarnLead   time.Duration
	Lookback   time.Duration
}

// Name implements Probe.
func (AmendmentWindowProbe) Name() string { return "amendment_window" }

// HostLocal implements Probe: the probe reads the tracker, not the host.
func (AmendmentWindowProbe) HostLocal() bool { return false }

// Check implements Probe.
func (p AmendmentWindowProbe) Check(ctx context.Context) []Result {
	window, warnLead, lookback := p.Window, p.WarnLead, p.Lookback
	if window <= 0 {
		window = DefaultAmendmentWindow
	}
	if lookback <= 0 {
		lookback = DefaultAmendmentLookback
	}
	rows, truncated, failed := scanAmendments(ctx, p.Name(), p.Amendments, lookback)
	if failed != nil {
		return []Result{*failed}
	}
	c := classifyAmendments(rows)
	threshold := fmt.Sprintf("decided within %s of request (%s window, %s warning lead)", window-warnLead, window, warnLead)
	var failing []Result
	for _, row := range c.live {
		age := row.ObservedAt.Sub(row.RequestedAt)
		if !windowDue(age, window, warnLead) {
			continue
		}
		remaining := window - age
		state := run.StageState(*row.StageState)
		tail := fmt.Sprintf("decision window closes in %s", remaining.Truncate(time.Second))
		if remaining <= 0 {
			tail = fmt.Sprintf("decision window elapsed %s ago", (-remaining).Truncate(time.Second))
		}
		failing = append(failing, Result{
			Probe:     p.Name(),
			Condition: ConditionAmendmentWindowClosing,
			Subject:   row.ID.String(),
			Status:    StatusFailing,
			Observed: fmt.Sprintf("scope amendment %s on run %s stage %s (%s) pending for %s; %s",
				row.ID, row.RunID, row.StageID, state, age.Truncate(time.Second), tail),
			Threshold: threshold,
			Fields: map[string]any{
				"amendment_id":           row.ID.String(),
				"run_id":                 row.RunID.String(),
				"stage_id":               row.StageID.String(),
				"stage_state":            string(state),
				"requested_at":           row.RequestedAt.UTC().Format(time.RFC3339),
				"age_seconds":            seconds(age),
				"window_seconds":         seconds(window),
				"warn_lead_seconds":      seconds(warnLead),
				"time_remaining_seconds": seconds(max(0, remaining)),
				"window_elapsed":         remaining <= 0,
			},
		})
	}
	return amendmentOutcome(p.Name(), failing, c, truncated, Result{
		Probe:     p.Name(),
		Status:    StatusOK,
		Observed:  fmt.Sprintf("%d pending amendment(s) scanned within %s, none due", len(rows), lookback),
		Threshold: threshold,
		Detail:    amendmentCounts(len(rows), c),
	})
}

// --- amendment_terminal ------------------------------------------------------

// AmendmentTerminalProbe reports a scope amendment still pending although its
// stage is terminal (run.StageState.IsTerminal): no agent will read a
// decision. A non-positive Lookback selects DefaultAmendmentLookback.
type AmendmentTerminalProbe struct {
	// Amendments reads the scan; nil reads unknown (no database configured).
	Amendments AmendmentReader
	Lookback   time.Duration
}

// Name implements Probe.
func (AmendmentTerminalProbe) Name() string { return "amendment_terminal" }

// HostLocal implements Probe: the probe reads the tracker, not the host.
func (AmendmentTerminalProbe) HostLocal() bool { return false }

const amendmentTerminalThreshold = "no scope amendment stays pending after its stage is terminal"

// Check implements Probe.
func (p AmendmentTerminalProbe) Check(ctx context.Context) []Result {
	lookback := p.Lookback
	if lookback <= 0 {
		lookback = DefaultAmendmentLookback
	}
	rows, truncated, failed := scanAmendments(ctx, p.Name(), p.Amendments, lookback)
	if failed != nil {
		return []Result{*failed}
	}
	c := classifyAmendments(rows)
	var failing []Result
	for _, row := range c.terminal {
		state := run.StageState(*row.StageState)
		failing = append(failing, Result{
			Probe:     p.Name(),
			Condition: ConditionAmendmentPendingOnTerminalStage,
			Subject:   row.ID.String(),
			Status:    StatusFailing,
			Observed: fmt.Sprintf("scope amendment %s on run %s stage %s still pending; the stage is %s",
				row.ID, row.RunID, row.StageID, state),
			Threshold: amendmentTerminalThreshold,
			Fields: map[string]any{
				"amendment_id": row.ID.String(),
				"run_id":       row.RunID.String(),
				"stage_id":     row.StageID.String(),
				"stage_state":  string(state),
				"requested_at": row.RequestedAt.UTC().Format(time.RFC3339),
				"age_seconds":  seconds(row.ObservedAt.Sub(row.RequestedAt)),
			},
		})
	}
	return amendmentOutcome(p.Name(), failing, c, truncated, Result{
		Probe:     p.Name(),
		Status:    StatusOK,
		Observed:  fmt.Sprintf("%d pending amendment(s) scanned within %s, none due", len(rows), lookback),
		Threshold: amendmentTerminalThreshold,
		Detail:    amendmentCounts(len(rows), c),
	})
}
