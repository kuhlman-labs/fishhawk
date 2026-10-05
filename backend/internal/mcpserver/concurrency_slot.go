package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
)

// The local concurrency-slot waiter (#3964 / ADR-087), the MCP half of the
// host-dispatch queue. When the host-dispatch marker answers 409
// concurrency_slot_queued, fishhawk_dispatch_stage spawns nothing and hands
// the dispatch to ONE in-process waiter per stage. The waiter re-POSTs the
// marker every concurrencySlotPollInterval (each POST refreshes the queue
// row's last_seen_at, which is what keeps the stage's place in the FIFO) and
// spawns the runner only when the marker admits it. Contract and residuals:
// README.md § "Local concurrency slot waiter".

// concurrencyWaiterCap bounds one waiter's lifetime: three full 60-minute
// implement budgets queued ahead. Package var so tests shrink it.
var concurrencyWaiterCap = 3 * time.Hour

// concurrencySlotPollInterval is the marker re-POST cadence; it mirrors the
// server's concurrency.WaiterPollInterval (the 409's poll_interval_seconds and
// Retry-After). Package var so tests shrink it.
var concurrencySlotPollInterval = 5 * time.Second

// The StageConcurrency.Waiter values a queued dispatch reports.
const (
	slotWaiterStarted        = "started"
	slotWaiterAlreadyWaiting = "already_waiting"
)

// slotHostUnknown is the host the server files a body-less marker under; it
// mirrors backend/internal/concurrency UnknownHost (pinned by
// TestSlotWaiter_UnknownHostMirrorsServer).
const slotHostUnknown = "unknown"

// slotMarker is the narrow backend surface the waiter uses. *apiClient
// satisfies it; the unit tests use an in-file sequenced fake.
type slotMarker interface {
	HostDispatchStage(ctx context.Context, runID, stageID uuid.UUID) (*HostDispatchResult, error)
	ReportStageFailureFrom(ctx context.Context, runID, stageID uuid.UUID, expectedState, category, reason, detail string, exitCode int) (*ReapFailureResult, error)
	GetRunStageWait(ctx context.Context, runID, stageID uuid.UUID, waitSeconds int) (*RunStageWait, error)
}

// slotSpawnInputs is what a prepare closure rebuilds at admission: the runner
// binary, argv and env (with the current token).
type slotSpawnInputs struct {
	binary string
	argv   []string
	env    []string
}

// detachedSpawnFunc is the dispatchSpawnDetached signature.
type detachedSpawnFunc func(binary string, argv, env []string, runID, stageID string, report detachedFailureReporter, probe detachedStageStateProbe) (string, error)

// slotWaiterSpec is everything one waiter needs, captured SYNCHRONOUSLY by the
// dispatch call so the goroutine never reads a package seam a test may swap.
type slotWaiterSpec struct {
	runID, stageID uuid.UUID
	// host is the label this process's marker sends ("" sends no body, which
	// the server files under slotHostUnknown).
	host string
	// episodeStart is the queued answer's enqueued_at (DB clock): the start
	// of this stage's queue episode. A holding row admitted for this waiter
	// has held_dispatched_at at or after it.
	episodeStart *time.Time
	marker       slotMarker
	// prepare rebuilds the spawn inputs AT ADMISSION (never at queue time),
	// so a runner binary or token change while queued is picked up.
	prepare func() (slotSpawnInputs, error)
	spawn   detachedSpawnFunc
	report  detachedFailureReporter
	probe   detachedStageStateProbe
	cap     time.Duration
	poll    time.Duration
	// logf writes the waiter's one-line diagnostics (stderr in production).
	logf func(format string, args ...any)
}

// slotWaiterRegistry dedupes one waiter per stage per process.
type slotWaiterRegistry struct {
	mu     sync.Mutex
	active map[uuid.UUID]chan struct{}
}

// slotWaiters is the process-wide registry.
var slotWaiters = &slotWaiterRegistry{active: map[uuid.UUID]chan struct{}{}}

// start launches a waiter for spec.stageID, or reports already_waiting when
// one is live in this process. done closes when the (new or existing) waiter
// exits. Every exit clears the registry entry, so a later start succeeds.
func (reg *slotWaiterRegistry) start(spec slotWaiterSpec) (status string, done <-chan struct{}) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if ch, ok := reg.active[spec.stageID]; ok {
		return slotWaiterAlreadyWaiting, ch
	}
	ch := make(chan struct{})
	reg.active[spec.stageID] = ch
	// DETACHED from the tool call's context: the call returns at once and the
	// waiter outlives it, bounded only by its own cap.
	ctx, cancel := context.WithTimeout(context.Background(), spec.cap)
	go func() {
		defer func() {
			cancel()
			reg.mu.Lock()
			delete(reg.active, spec.stageID)
			reg.mu.Unlock()
			close(ch)
		}()
		runSlotWaiter(ctx, spec)
	}()
	return slotWaiterStarted, ch
}

// slotPollOutcome classifies one marker answer.
type slotPollOutcome int

const (
	slotKeepWaiting    slotPollOutcome = iota // queued, or a transient failure
	slotAdmitted                              // 200 transitioned:true — spawn
	slotOtherAdmission                        // 200 transitioned:false — another session won
	slotTerminal                              // any other 4xx — stop, no spawn
)

// classifySlotPoll maps a marker answer onto an outcome. lost reports a
// TRANSPORT error (no HTTP answer at all): the request may have been
// processed and its response lost, so a following transitioned:false must be
// checked against this waiter's own admission. A 5xx is transient but not
// lost: the server answered, and an admission that errors rolls back.
func classifySlotPoll(res *HostDispatchResult, err error) (out slotPollOutcome, lost bool, queued *concurrencySlotQueuedError) {
	if err == nil {
		if res != nil && res.Transitioned {
			return slotAdmitted, false, nil
		}
		return slotOtherAdmission, false, nil
	}
	if q, ok := asConcurrencySlotQueued(err); ok {
		return slotKeepWaiting, false, q
	}
	var ae *apiError
	if !errors.As(err, &ae) {
		return slotKeepWaiting, true, nil
	}
	if ae.StatusCode >= 500 {
		return slotKeepWaiting, false, nil
	}
	return slotTerminal, false, nil
}

// runSlotWaiter is the poll loop. ctx carries the cap; each request runs on
// context.WithoutCancel(ctx), so the cap stops POLLING but never cancels an
// in-flight marker POST (whose admission would otherwise be lost with no
// spawn). The apiClient's own timeout bounds each request.
func runSlotWaiter(ctx context.Context, spec slotWaiterSpec) {
	reqCtx := context.WithoutCancel(ctx)
	ticker := time.NewTicker(spec.poll)
	defer ticker.Stop()
	lost := false
	for {
		select {
		case <-ctx.Done():
			spec.logf("fishhawk: concurrency slot waiter for run %s stage %s stopped after its %s cap without admission; the stage stays awaiting_host_dispatch — re-dispatch it with fishhawk_dispatch_stage\n",
				spec.runID, spec.stageID, spec.cap)
			return
		case <-ticker.C:
		}
		res, err := spec.marker.HostDispatchStage(reqCtx, spec.runID, spec.stageID)
		out, wasLost, queued := classifySlotPoll(res, err)
		switch out {
		case slotKeepWaiting:
			if queued != nil {
				// A definitive queued answer: the stage was not admitted, so
				// any earlier lost request did not admit it either.
				lost = false
				if queued.EnqueuedAt != nil {
					spec.episodeStart = queued.EnqueuedAt
				}
			}
			if wasLost {
				lost = true
			}
			continue
		case slotAdmitted:
			spawnOrRelease(reqCtx, spec)
			return
		case slotOtherAdmission:
			if lost && ownLostAdmission(reqCtx, spec) {
				spawnOrRelease(reqCtx, spec)
				return
			}
			spec.logf("fishhawk: concurrency slot waiter for run %s stage %s: the stage was already dispatched by another session; not spawning\n",
				spec.runID, spec.stageID)
			return
		default:
			spec.logf("fishhawk: concurrency slot waiter for run %s stage %s stopped without spawning: %v\n",
				spec.runID, spec.stageID, err)
			return
		}
	}
}

// ownLostAdmission decides, after a lost response followed by 200
// transitioned:false, whether the stage's admission was THIS waiter's (#3964
// approval condition 3). It reads the stage once and requires all of:
//
//   - state dispatched — a running stage already has a runner, so spawning
//     would double-spawn;
//   - a holding concurrency block whose host is this process's host label;
//   - held_dispatched_at at or after this waiter's queue-episode start (the
//     queued answer's enqueued_at; both are DB-clock stamps).
//
// Any read error or missing field answers false: no spawn. That leaves a
// stage this waiter admitted dispatched with no runner, which a plain
// re-dispatch recovers (the marker's idempotent arm proceeds to spawn) and the
// server's 15-minute dispatched backstop releases. RESIDUAL: another session
// on the SAME host admitting the stage inside the same episode carries the
// same host label and is indistinguishable without an admission token.
func ownLostAdmission(ctx context.Context, spec slotWaiterSpec) bool {
	sw, err := spec.marker.GetRunStageWait(ctx, spec.runID, spec.stageID, 0)
	if err != nil || sw == nil {
		return false
	}
	c := sw.Concurrency
	host := spec.host
	if host == "" {
		host = slotHostUnknown
	}
	return sw.State == "dispatched" &&
		c != nil &&
		c.Status == concurrencyStatusHolding &&
		c.Host == host &&
		c.HeldDispatchedAt != nil &&
		spec.episodeStart != nil &&
		!c.HeldDispatchedAt.Before(*spec.episodeStart)
}

// spawnOrRelease rebuilds the spawn inputs and spawns the runner for an
// admitted stage. On a prepare or spawn failure it releases the slot through
// the existing reap-failure path (expected_state dispatched, category C), so
// the stage leaves dispatched and the next queued stage is admitted at once.
func spawnOrRelease(ctx context.Context, spec slotWaiterSpec) {
	in, err := spec.prepare()
	if err == nil {
		_, err = spec.spawn(in.binary, in.argv, in.env, spec.runID.String(), spec.stageID.String(), spec.report, spec.probe)
	}
	if err == nil {
		return
	}
	if rerr := releaseSlotAfterSpawnFailure(ctx, spec.marker, spec.runID, spec.stageID, "concurrency slot waiter", err); rerr != nil {
		spec.logf("fishhawk: concurrency slot waiter for run %s stage %s: spawn failed after admission (%v) and the slot release failed: %v\n",
			spec.runID, spec.stageID, err, rerr)
		return
	}
	spec.logf("fishhawk: concurrency slot waiter for run %s stage %s: spawn failed after admission (%v); the stage was failed category C and its slot released\n",
		spec.runID, spec.stageID, err)
}

// releaseSlotAfterSpawnFailure fails an ADMITTED stage whose runner never
// spawned, through the reap-failure path pinned to dispatched: the stage
// leaves dispatched (so it stops counting as a holder) and is retryable as
// category C. Shared by the waiter and dispatch_stage's immediate admission.
// source names the spawner in the failure reason.
func releaseSlotAfterSpawnFailure(ctx context.Context, marker slotMarker, runID, stageID uuid.UUID, source string, spawnErr error) error {
	_, err := marker.ReportStageFailureFrom(ctx, runID, stageID, "dispatched", "C",
		source+": spawn failed after admission: "+spawnErr.Error(), "", 0)
	return err
}

// stderrLogf is the production slotWaiterSpec.logf.
func stderrLogf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format, args...)
}
