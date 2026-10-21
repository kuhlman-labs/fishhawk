package main

import (
	"context"
	"sync"
	"time"
)

// The #4023 two-budget stage model. An implement stage alternates between an
// AGENT phase (an agent invocation is running) and a GATE phase (the agent has
// exited and the verify gate owns the stage). Each phase has its own budget:
//
//   - the agent phase is bounded per invocation by executor.timeout
//     (agent.Budget.Timeout = cfg.timeout, handed afresh to every invoke). The
//     clock only OBSERVES it, for the phase reports.
//   - the gate phase is bounded by gateBudget = gateBudgetVerifyForms x
//     executor.verify.timeout of VERIFY-EXECUTION time. Only the wall time of
//     a verify command (startVerify .. endVerify) is charged; staging, commits,
//     diff coverage, counterfactuals and push work are not, so slow plumbing
//     can never starve a later strict re-verify. Each verify's own timeout is
//     clipped to what remains (gateVerifyTimeout), and an exhausted budget ends
//     the stage category C under the existing verifyGateTimedOutLead.
//
// Agent time is never charged to the gate budget: beginGate zeroes the charge,
// so an agent that spent 59 of its 60 minutes leaves the gate its full budget.
//
// The clock rides the stage context (withStagePhases / stagePhasesFrom, the
// withDecidingGate idiom), so no gate-function signature changes. Every method
// is NIL-RECEIVER SAFE: a context without a clock leaves every timeout
// unclipped, never exhausts and reports nothing — byte-identical to the
// pre-#4023 runner.

// gateBudgetVerifyForms is the maximum number of verify EXECUTIONS one gate
// phase can run without verify-lock contention, so the gate budget
// (gateBudgetVerifyForms x executor.verify.timeout) can never kill a tree the
// pre-#4023 runner would have judged. A gate phase starts when an agent
// invocation exits (beginGate) and ends at the next agent invocation, so its
// worst case is the LAST verify-fix iteration's in-place repeats followed by
// the #960 strict re-verify before push. Walked from main.go, counting every
// form charged to the phase (deliberately over-counted: an undercount newly
// fails a passing tree):
//
// runVerifyFixLoop, one iteration PASS = the (c) scoped verify (main.go
// `runVerifyCommittedTree(..., scopePkgs)`) + the (c2) full re-verify
// (`runVerifyCommittedTreeEnv(..., nil, nonceEnv...)`): 2 executions. The
// passes one gate phase can run without a fix re-invoke (`iter--; continue`):
//
//  1. the iteration itself                                    2 (scoped + full)
//  2. the #3983 skipped-gate re-run (skipRerunSpent, once)    2 (scoped + full)
//  3. the #4022 temp-fail re-runs (tempFailState.admit,
//     verifyTempFailMaxReruns = 2)                             4 (2 x scoped + full)
//  4. the #972 infra-flake absorb (flakeRetried, once)        2 (scoped + full)
//  5. the #3316 auto-format absorb (autoformatted, once)      2 (scoped + full)
//  6. the #4229 outside-only flake absorb at exhaustion
//     (absorbPending, `runVerifyCommittedTree(..., absorbing.dirs)`,
//     scoped only, no c2)                                      1
//
// = 13 in the fix loop. Then the #960 strict re-verify (the pre-push hook,
// `runVerifyCommittedTree(..., reverifyTimeout, nil)`), walked separately:
//
//  7. the strict re-verify itself                              1
//  8. its #4022 temp-fail re-runs (verifyTempFailMaxReruns)    2
//  9. its #2645 infra / #3948 skipped absorb re-run (once)     1
//
// = 4, for a fix-loop gate phase of 17.
//
// runVerifyGateCommitted (the single-shot gate, also the deterministic fix-up
// apply's fixupVerifyGate), walked separately: the initial full verify (1),
// its temp-fail re-runs (2), its #972 infra absorb re-run (1) and its #3983
// skipped re-run (1) = 5, plus the strict re-verify's 4 = 9 <= 17. The
// working-tree runVerifyGate runs exactly one verify.
//
// EXCLUDED: verify-lock contention re-runs (#3948). Their admission reads the
// gate budget (verifyLockRerunFits), so a contended re-run the budget cannot
// cover is refused category C verify_lock_contended instead of being started
// and killed mid-wait.
//
// Pinned by TestStagePhaseClock_GateBudgetVerifyFormsEnumeration, which
// re-derives the sum from gateBudgetVerifyFormTable and verifyTempFailMaxReruns:
// dropping a form, or raising a re-run bound, turns it RED.
const gateBudgetVerifyForms = 17

// gateBudgetVerifyForm is one row of the enumeration above: a verify form one
// gate phase can execute and how many executions it contributes.
type gateBudgetVerifyForm struct {
	site       string
	executions int
}

// gateBudgetVerifyFormTable is the machine-readable twin of the
// gateBudgetVerifyForms doc comment, summed by its pinning test.
var gateBudgetVerifyFormTable = []gateBudgetVerifyForm{
	{"runVerifyFixLoop: (c) scoped + (c2) full verify of the iteration", 2},
	{"runVerifyFixLoop: #3983 skipped-gate re-run of the iteration (scoped + full)", 2},
	{"runVerifyFixLoop: #4022 temp-fail re-runs of the iteration (scoped + full each)", 2 * verifyTempFailMaxReruns},
	{"runVerifyFixLoop: #972 infra-flake absorb re-run (scoped + full)", 2},
	{"runVerifyFixLoop: #3316 auto-format absorb re-run (scoped + full)", 2},
	{"runVerifyFixLoop: #4229 outside-only flake absorb re-run (scoped)", 1},
	{"#960 strict re-verify", 1},
	{"#960 strict re-verify: #4022 temp-fail re-runs", verifyTempFailMaxReruns},
	{"#960 strict re-verify: #2645 infra / #3948 skipped absorb re-run", 1},
}

// defaultGateVerifyTimeout is the per-verify timeout every gate site applies
// when executor.verify.timeout is unset (runVerifyGate, runVerifyFixLoop,
// runVerifyGateCommitted and the strict re-verify all default to 10m).
const defaultGateVerifyTimeout = 10 * time.Minute

// Stage phase names, carried verbatim on the progress wire (#4023).
const (
	stagePhaseAgent = "agent"
	stagePhaseGate  = "gate"
)

// stagePhaseSnapshot is the clock's state at one instant, the source of the
// five #4023 progress fields. Phase is "" before the first transition.
// GateBudget / GateRemaining are set only in the gate phase; GateRemaining is
// extrapolated while a verify is in flight and may be negative (callers clamp).
type stagePhaseSnapshot struct {
	Phase         string
	PhaseElapsed  time.Duration
	GateBudget    time.Duration
	GateRemaining time.Duration
	Verifying     bool
}

// stagePhaseClock is the per-stage two-budget clock. now is time.Now in
// production (monotonic, host-clock-skew free) and injectable in tests. report,
// when set, is called OUTSIDE the lock after every transition with the new
// snapshot — wired to progressTee.reportPhase, which may block up to
// progressReportTimeout.
type stagePhaseClock struct {
	mu          sync.Mutex
	now         func() time.Time
	agentBudget time.Duration
	gateBudget  time.Duration
	phase       string
	agentStart  time.Time
	gateStart   time.Time
	verifyStart time.Time // zero when no verify is in flight
	gateUsed    time.Duration
	report      func(stagePhaseSnapshot)
}

// newStagePhaseClock builds the clock for one stage: agentBudget is
// executor.timeout (observed only); the gate budget is the EFFECTIVE
// executor.verify.timeout (defaultGateVerifyTimeout when unset) times
// gateBudgetVerifyForms.
func newStagePhaseClock(agentBudget, verifyTimeout time.Duration) *stagePhaseClock {
	eff := verifyTimeout
	if eff <= 0 {
		eff = defaultGateVerifyTimeout
	}
	return &stagePhaseClock{
		now:         time.Now,
		agentBudget: agentBudget,
		gateBudget:  eff * gateBudgetVerifyForms,
	}
}

type stagePhasesKey struct{}

// withStagePhases carries c on ctx so every gate site downstream reads the
// same clock.
func withStagePhases(ctx context.Context, c *stagePhaseClock) context.Context {
	return context.WithValue(ctx, stagePhasesKey{}, c)
}

// stagePhasesFrom returns the clock withStagePhases carried, or nil — and a
// nil clock is a no-op on every method.
func stagePhasesFrom(ctx context.Context) *stagePhaseClock {
	c, _ := ctx.Value(stagePhasesKey{}).(*stagePhaseClock)
	return c
}

// transition applies mutate under the lock, then reports the resulting
// snapshot outside it.
func (c *stagePhaseClock) transition(mutate func(now time.Time)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	now := c.now()
	mutate(now)
	snap := c.snapshotLocked(now)
	report := c.report
	c.mu.Unlock()
	if report != nil {
		report(snap)
	}
}

// beginAgent enters the agent phase. Called immediately before every agent
// invocation.
func (c *stagePhaseClock) beginAgent() {
	c.transition(func(now time.Time) {
		c.phase = stagePhaseAgent
		c.agentStart = now
	})
}

// beginGate enters a gate phase with its FULL budget: the charge and any
// in-flight verify mark are zeroed, so agent time never reaches the gate.
func (c *stagePhaseClock) beginGate() {
	c.transition(func(now time.Time) {
		c.phase = stagePhaseGate
		c.gateStart = now
		c.gateUsed = 0
		c.verifyStart = time.Time{}
	})
}

// startVerify marks one verify execution as in flight.
func (c *stagePhaseClock) startVerify() {
	c.transition(func(now time.Time) {
		c.verifyStart = now
	})
}

// endVerify charges the in-flight verify's wall time to the gate budget. A
// call with no verify in flight charges nothing.
func (c *stagePhaseClock) endVerify() {
	c.transition(func(now time.Time) {
		if !c.verifyStart.IsZero() {
			c.gateUsed += now.Sub(c.verifyStart)
			c.verifyStart = time.Time{}
		}
	})
}

// gateRemaining is the gate budget not yet charged; 0 on a nil clock (callers
// that need to tell "no clock" apart check for nil first).
func (c *stagePhaseClock) gateRemaining() time.Duration {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gateRemainingLocked()
}

// gateRemainingLocked is the single definition of the gate budget's remainder
// (c.mu held): the gate budget minus the charged verify time, and nothing else.
func (c *stagePhaseClock) gateRemainingLocked() time.Duration {
	return c.gateBudget - c.gateUsed
}

// liveGateRemainingLocked additionally subtracts an in-flight verify's
// elapsed time (c.mu held).
func (c *stagePhaseClock) liveGateRemainingLocked(now time.Time) time.Duration {
	rem := c.gateRemainingLocked()
	if !c.verifyStart.IsZero() {
		rem -= now.Sub(c.verifyStart)
	}
	return rem
}

// gateBudgetTotal is the gate phase's total verify budget; 0 on a nil clock.
func (c *stagePhaseClock) gateBudgetTotal() time.Duration {
	if c == nil {
		return 0
	}
	return c.gateBudget
}

// gateVerifyTimeout clips one verify's timeout vt to the gate budget left:
// it returns min(vt, max(remaining, 0)) and whether that clipped vt. A nil
// clock returns vt unclipped. An effective timeout <= 0 means the budget is
// spent and the caller must not spawn the verify.
func (c *stagePhaseClock) gateVerifyTimeout(vt time.Duration) (time.Duration, bool) {
	if c == nil {
		return vt, false
	}
	c.mu.Lock()
	rem := c.liveGateRemainingLocked(c.now())
	c.mu.Unlock()
	if rem < 0 {
		rem = 0
	}
	if vt > rem {
		return rem, true
	}
	return vt, false
}

// gateExhausted reports whether the gate budget is spent; always false on a
// nil clock.
func (c *stagePhaseClock) gateExhausted() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.liveGateRemainingLocked(c.now()) <= 0
}

// snapshot returns the clock's state now; the zero snapshot on a nil clock.
func (c *stagePhaseClock) snapshot() stagePhaseSnapshot {
	if c == nil {
		return stagePhaseSnapshot{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotLocked(c.now())
}

func (c *stagePhaseClock) snapshotLocked(now time.Time) stagePhaseSnapshot {
	switch c.phase {
	case stagePhaseAgent:
		return stagePhaseSnapshot{Phase: stagePhaseAgent, PhaseElapsed: now.Sub(c.agentStart)}
	case stagePhaseGate:
		return stagePhaseSnapshot{
			Phase:         stagePhaseGate,
			PhaseElapsed:  now.Sub(c.gateStart),
			GateBudget:    c.gateBudget,
			GateRemaining: c.liveGateRemainingLocked(now),
			Verifying:     !c.verifyStart.IsZero(),
		}
	}
	return stagePhaseSnapshot{}
}
