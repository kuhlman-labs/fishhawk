package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
)

// The local concurrency-slot waiter (#3964 / ADR-087), the MCP half of the
// host-dispatch queue. When the host-dispatch marker answers 409
// concurrency_slot_queued, fishhawk_dispatch_stage spawns nothing and hands
// the dispatch to ONE in-process waiter per stage. The waiter re-POSTs the
// marker every concurrencySlotPollInterval ±20% (each POST refreshes the queue
// row's last_seen_at, which is what keeps the stage's place in the FIFO),
// retries once at once after a contended answer, and spawns the runner only
// when the marker admits it. Contract and residuals: README.md § "Local
// concurrency slot waiter".

// concurrencyWaiterCap bounds one waiter's lifetime: three full 60-minute
// implement budgets queued ahead. Package var so tests shrink it.
var concurrencyWaiterCap = 3 * time.Hour

// concurrencySlotPollInterval is the marker re-POST cadence before jitter; it
// mirrors the server's concurrency.WaiterPollInterval (the 409's
// poll_interval_seconds and Retry-After). Package var so tests shrink it.
var concurrencySlotPollInterval = 5 * time.Second

// slotPollJitter is the ± fraction applied to every poll wait, so two waiters
// whose polls collide on the group lock do not stay phase-aligned.
const slotPollJitter = 0.2

// jitteredPoll scales base by a factor in [1-slotPollJitter, 1+slotPollJitter)
// picked by r in [0, 1). Pure so a test pins the bounds.
func jitteredPoll(base time.Duration, r float64) time.Duration {
	return time.Duration(float64(base) * (1 - slotPollJitter + 2*slotPollJitter*r))
}

// randomJitteredPoll is the production slotWaiterSpec.jitter.
func randomJitteredPoll(base time.Duration) time.Duration {
	return jitteredPoll(base, rand.Float64())
}

// newAdmissionNonce mints a slot waiter's admission nonce: random, one per
// waiter, sent on every marker POST it makes.
func newAdmissionNonce() string { return uuid.NewString() }

// The StageConcurrency.Waiter values a queued dispatch reports.
const (
	slotWaiterStarted        = "started"
	slotWaiterAlreadyWaiting = "already_waiting"
)

// slotMarker is the narrow backend surface the waiter uses. *apiClient
// satisfies it; the unit tests use an in-file sequenced fake.
type slotMarker interface {
	HostDispatchStageWithNonce(ctx context.Context, runID, stageID uuid.UUID, nonce string) (*HostDispatchResult, error)
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
	// nonce is this waiter's admission nonce (newAdmissionNonce), sent on
	// every marker POST and recorded by the server on the slot it admits. A
	// waiter claims a lost admission only when the holding row carries it;
	// an empty nonce never claims one.
	nonce string
	// contended reports the dispatch call's own queued answer was contended,
	// so the waiter's first poll is the one immediate retry.
	contended bool
	marker    slotMarker
	// prepare rebuilds the spawn inputs AT ADMISSION (never at queue time),
	// so a runner binary or token change while queued is picked up.
	prepare func() (slotSpawnInputs, error)
	spawn   detachedSpawnFunc
	report  detachedFailureReporter
	probe   detachedStageStateProbe
	cap     time.Duration
	poll    time.Duration
	// jitter maps poll onto one wait (randomJitteredPoll when nil).
	jitter func(time.Duration) time.Duration
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
//
// Each wait is spec.poll ±20% (spec.jitter). A CONTENDED queued answer — the
// group lock was held by another admission, so this poll queued without
// deciding — is retried once at once, unless it answered that immediate
// retry itself: the colliding winner's transaction has normally committed by
// then, so a waiter at the head of the queue is admitted instead of losing
// the round (concurrency README § "Admit, step by step", step 7).
func runSlotWaiter(ctx context.Context, spec slotWaiterSpec) {
	reqCtx := context.WithoutCancel(ctx)
	jitter := spec.jitter
	if jitter == nil {
		jitter = randomJitteredPoll
	}
	wait := jitter(spec.poll)
	// retried reports the next (or last) poll is the one immediate retry.
	retried := spec.contended
	if retried {
		wait = 0
	}
	lost := false
	for {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			spec.logf("fishhawk: concurrency slot waiter for run %s stage %s stopped after its %s cap without admission; the stage stays awaiting_host_dispatch — re-dispatch it with fishhawk_dispatch_stage\n",
				spec.runID, spec.stageID, spec.cap)
			return
		case <-timer.C:
		}
		res, err := spec.marker.HostDispatchStageWithNonce(reqCtx, spec.runID, spec.stageID, spec.nonce)
		out, wasLost, queued := classifySlotPoll(res, err)
		wait = jitter(spec.poll)
		immediate := queued != nil && queued.Contended && !retried
		retried = immediate
		if immediate {
			wait = 0
		}
		switch out {
		case slotKeepWaiting:
			if queued != nil {
				// A definitive queued answer: the stage was not admitted, so
				// any earlier lost request did not admit it either.
				lost = false
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
//   - a holding concurrency block whose admission_nonce is this waiter's
//     nonce. The server records the admitting request's nonce on the held
//     row, so another session's admission (on this host or any other) and a
//     direct fishhawk_dispatch_stage admission (which sends no nonce) never
//     match. The host label is NOT used: two sessions on one host share it.
//
// Any read error, missing field or empty nonce answers false: no spawn. That
// leaves a stage this waiter admitted dispatched with no runner, which a
// plain re-dispatch recovers (the marker's idempotent arm proceeds to spawn)
// and the server's 15-minute dispatched backstop releases.
func ownLostAdmission(ctx context.Context, spec slotWaiterSpec) bool {
	if spec.nonce == "" {
		return false
	}
	sw, err := spec.marker.GetRunStageWait(ctx, spec.runID, spec.stageID, 0)
	if err != nil || sw == nil {
		return false
	}
	c := sw.Concurrency
	return sw.State == "dispatched" &&
		c != nil &&
		c.Status == concurrencyStatusHolding &&
		c.AdmissionNonce == spec.nonce
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
