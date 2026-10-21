package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// progressReportTimeout bounds every heartbeat POST. It is chosen against the
// runner's ~15s heartbeat interval (defaultHeartbeatInterval in the codex /
// claudecode adapters) so a slow report cannot still be running when the next
// tick fires — a 3x margin. If the heartbeat interval is ever shortened below
// ~10s this constant must shrink with it.
const progressReportTimeout = 5 * time.Second

// stageProgressReporter is the narrow capability progressTee needs from the
// upload client — just the single-attempt heartbeat POST. upload.Client (and
// the uploadClient test seam) satisfy it.
type stageProgressReporter interface {
	ReportStageProgress(ctx context.Context, args upload.ReportStageProgressArgs) error
}

// progressTee wraps the agent adapter's ProgressSink so each stage_progress
// heartbeat line stays byte-identical on stderr AND is best-effort POSTed to the
// backend, projecting it onto the stage row so an operator poll returns real
// activity instead of a single 'running' bit (#2541).
//
// FAIL-OPEN BY CONSTRUCTION — the reporter can never fail or slow a stage. Four
// independent guards, each verified by a test:
//
//   - A nil client or an empty bearer token makes the tee a pure pass-through
//     (the acceptance stage's zero-credential posture, ADR-050 decision #2).
//   - A line that is not a stage_progress JSON object is forwarded and ignored.
//   - The POST runs asynchronously under a bounded context (progressReportTimeout,
//     ~1/3 of the heartbeat interval), so a slow backend never stalls the agent's
//     heartbeat goroutine, and in-flight reports are capped at ONE — a tick issued
//     while a report is outstanding is SKIPPED, not queued, so a wedged backend
//     cannot leak goroutines across a 60-minute stage.
//   - Any POST error is written as a single stage_progress_report_failed line and
//     dropped, never returned.
//
// WHOLE-LINE ASSUMPTION (#2541 approval condition 5). The agent adapters' heartbeat
// goroutine is the SOLE writer to ProgressSink during an invocation and emits ONE
// complete stage_progress line per Write call (a single trailing-newline Fprintf).
// A split or batched write would fail the JSON parse and simply yield no POST —
// superseded ~15s later by the next tick — and can NEVER corrupt stderr, because
// Write returns the wrapped sink's (n, err) UNCHANGED regardless of what the
// reporter does.
//
// CONCURRENCY (#2541 approval condition 1). The async report goroutine's
// diagnostic line is written through the SAME diag writer the caller passes —
// wired in main.go to the mutex-guarded syncWriter that also carries the
// heartbeat forwarding — so a report's error log and a later heartbeat write
// share one synchronization boundary and cannot tear a line.
type progressTee struct {
	sink    io.Writer // the wrapped ProgressSink; forwarded to first, byte-identical
	diag    io.Writer // where a report failure line is written (same syncWriter as sink)
	client  stageProgressReporter
	runID   string
	stageID string
	token   string
	// tokens, when set, supplies a refreshed bearer per report (#3255); nil
	// keeps the constructor token byte-for-byte. Assigned after construction.
	tokens *mcpTokenSource
	// phases, when set, is the stage's two-budget clock (#4023): every
	// heartbeat POST carries its snapshot, read at Write time so a late
	// heartbeat still names the CURRENT phase. nil keeps every POST
	// byte-identical to the pre-#4023 body. Assigned after construction.
	phases *stagePhaseClock

	slot chan struct{}  // capacity 1: the single-in-flight cap, shared with reportPhase
	wg   sync.WaitGroup // lets tests await in-flight reports deterministically

	mu         sync.Mutex // guards lastTurns / lastTokens
	lastTurns  int        // the last heartbeat's counters, carried by reportPhase
	lastTokens int
}

// newProgressTee wraps sink. diag is where report-failure diagnostics go — the
// caller passes the SAME mutex-guarded writer that carries heartbeat forwarding
// so the two share one synchronization boundary (approval condition 1).
func newProgressTee(sink io.Writer, client stageProgressReporter, runID, stageID, token string, diag io.Writer) *progressTee {
	return &progressTee{
		sink:    sink,
		diag:    diag,
		client:  client,
		runID:   runID,
		stageID: stageID,
		token:   token,
		slot:    make(chan struct{}, 1),
	}
}

// Write forwards p to the wrapped sink FIRST and returns that write's (n, err)
// UNCHANGED, then best-effort parses and POSTs the heartbeat. The pass-through
// return is what guarantees the tee can never shorten or fail a stderr write.
func (t *progressTee) Write(p []byte) (int, error) {
	n, err := t.sink.Write(p)

	// Guard 1: nil client / empty token → pure pass-through.
	if t.client == nil || t.token == "" {
		return n, err
	}
	// Guard 2: not a stage_progress heartbeat → forwarded, not posted.
	hb, ok := parseStageProgressLine(p)
	if !ok {
		return n, err
	}
	t.mu.Lock()
	t.lastTurns, t.lastTokens = hb.Turns, hb.Tokens
	t.mu.Unlock()
	// Read the phase snapshot NOW, at Write time, not in the goroutine.
	snap := t.phases.snapshot()
	// Guard 3: single-in-flight cap. A tick issued while a report is
	// outstanding is SKIPPED, not queued.
	select {
	case t.slot <- struct{}{}:
	default:
		return n, err
	}
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer func() { <-t.slot }()
		// Resolve the bearer BEFORE the report's own bounded context starts, so
		// a refresh (bounded separately by mcpTokenRefreshTimeout) never eats
		// into the POST's progressReportTimeout.
		token := t.token
		if t.tokens != nil {
			token = t.tokens.bearer(context.Background())
		}
		ctx, cancel := context.WithTimeout(context.Background(), progressReportTimeout)
		defer cancel()
		args := upload.ReportStageProgressArgs{
			RunID:             t.runID,
			StageID:           t.stageID,
			MCPToken:          token,
			LastEvent:         hb.LastEvent,
			TurnsThisAttempt:  hb.Turns,
			TokensThisAttempt: hb.Tokens,
		}
		applyPhaseSnapshot(&args, snap)
		// Guard 4: swallow any error as a single diagnostic line, never return it.
		t.logReportFailure(t.client.ReportStageProgress(ctx, args))
	}()
	return n, err
}

// reportPhase POSTs one runner-authored phase report (#4023) — the stage
// clock's report hook, called after every transition (agent start, gate
// start, verify start/end). It is a no-op on a nil tee, a nil client or an
// empty token (guard 1). Otherwise it is SYNCHRONOUS and ORDERED: it first
// takes the single-in-flight slot, which WAITS for an in-flight heartbeat to
// finish, so a heartbeat posted before the transition cannot land after it and
// flip the stored phase back (last-writer-wins); and a heartbeat tick arriving
// while it holds the slot is skipped, exactly like a tick behind another
// heartbeat. The POST runs under progressReportTimeout and carries the last
// heartbeat's turns/tokens plus a runner-authored last_event: agent_start,
// gate, or gate:verify while a verify is in flight. A failure is one
// stage_progress_report_failed line and is dropped (guard 4): it never fails
// the stage. The bound per call is NOT progressReportTimeout alone: the slot
// wait can cover an in-flight heartbeat's bearer refresh
// (mcpTokenRefreshTimeout, 15s) and POST (5s), and this call then pays its
// own refresh and POST, so ~40s against a wedged backend. The stage clock
// never charges that latency to the gate budget (startVerify re-stamps).
func (t *progressTee) reportPhase(snap stagePhaseSnapshot) {
	if t == nil || t.client == nil || t.token == "" {
		return
	}
	t.slot <- struct{}{}
	defer func() { <-t.slot }()

	token := t.token
	if t.tokens != nil {
		token = t.tokens.bearer(context.Background())
	}
	t.mu.Lock()
	turns, tokens := t.lastTurns, t.lastTokens
	t.mu.Unlock()
	args := upload.ReportStageProgressArgs{
		RunID:             t.runID,
		StageID:           t.stageID,
		MCPToken:          token,
		LastEvent:         phaseReportEvent(snap),
		TurnsThisAttempt:  turns,
		TokensThisAttempt: tokens,
	}
	applyPhaseSnapshot(&args, snap)
	ctx, cancel := context.WithTimeout(context.Background(), progressReportTimeout)
	defer cancel()
	t.logReportFailure(t.client.ReportStageProgress(ctx, args))
}

// logReportFailure writes the single stage_progress_report_failed diag line
// for a non-nil report error and drops it.
func (t *progressTee) logReportFailure(rerr error) {
	if rerr == nil {
		return
	}
	_, _ = fmt.Fprintf(t.diag,
		`{"event":"stage_progress_report_failed","run_id":%q,"stage_id":%q,"detail":%q}`+"\n",
		t.runID, t.stageID, rerr.Error())
}

// Runner-authored last_event values of a phase report (#4023).
const (
	phaseEventAgentStart = "agent_start"
	phaseEventGate       = "gate"
	phaseEventGateVerify = "gate:verify"
)

// phaseReportEvent names the transition a phase report announces.
func phaseReportEvent(snap stagePhaseSnapshot) string {
	switch {
	case snap.Phase == stagePhaseAgent:
		return phaseEventAgentStart
	case snap.Phase == stagePhaseGate && snap.Verifying:
		return phaseEventGateVerify
	case snap.Phase == stagePhaseGate:
		return phaseEventGate
	}
	return ""
}

// applyPhaseSnapshot copies a stage clock snapshot onto the five optional
// #4023 progress fields. An empty Phase (no clock, or no transition yet) sets
// none of them, so the body stays byte-identical to the pre-#4023 one; the
// gate fields are set only in the gate phase. Every duration is reported in
// whole seconds and clamped at 0 — the backend ingest rejects a negative
// *_seconds value, and an extrapolated in-flight remainder can dip below 0.
func applyPhaseSnapshot(args *upload.ReportStageProgressArgs, snap stagePhaseSnapshot) {
	if snap.Phase == "" {
		return
	}
	args.Phase = snap.Phase
	args.PhaseElapsedSeconds = nonNegativeSeconds(snap.PhaseElapsed)
	if snap.Phase != stagePhaseGate {
		return
	}
	args.GateBudgetSeconds = nonNegativeSeconds(snap.GateBudget)
	args.GateRemainingSeconds = nonNegativeSeconds(snap.GateRemaining)
	args.GateVerifying = snap.Verifying
}

// nonNegativeSeconds renders d as whole seconds, clamped at 0.
func nonNegativeSeconds(d time.Duration) *int {
	s := int(d / time.Second)
	if s < 0 {
		s = 0
	}
	return &s
}

// waitForReports blocks until every in-flight report goroutine has returned.
// Test-only: production never needs it (the reports are fire-and-forget).
func (t *progressTee) waitForReports() { t.wg.Wait() }

// stageProgressLine is the parsed shape of one stage_progress heartbeat line.
// The runner emits turns / tokens_so_far / last_event_kind; elapsed_seconds is
// deliberately ignored here — the backend derives the operator-facing elapsed
// from the stage's started_at.
type stageProgressLine struct {
	Event     string `json:"event"`
	Turns     int    `json:"turns"`
	Tokens    int    `json:"tokens_so_far"`
	LastEvent string `json:"last_event_kind"`
}

// parseStageProgressLine decodes p as a stage_progress heartbeat. ok=false for
// any line that is not a single JSON object with event=="stage_progress" — a
// split/batched write, a non-heartbeat line, or malformed JSON — so a
// non-heartbeat write is forwarded and silently unposted.
func parseStageProgressLine(p []byte) (stageProgressLine, bool) {
	var hb stageProgressLine
	if err := json.Unmarshal(bytes.TrimSpace(p), &hb); err != nil {
		return stageProgressLine{}, false
	}
	if hb.Event != "stage_progress" {
		return stageProgressLine{}, false
	}
	return hb, true
}
