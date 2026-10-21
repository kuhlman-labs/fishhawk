package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// manualClock is an injectable now() for stagePhaseClock: time moves only when
// the test advances it, so every budget assertion is exact.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func newManualClock() *manualClock {
	return &manualClock{t: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
}

func (m *manualClock) now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.t
}

func (m *manualClock) advance(d time.Duration) {
	m.mu.Lock()
	m.t = m.t.Add(d)
	m.mu.Unlock()
}

// newTestPhaseClock builds a stage clock on a manual time source.
func newTestPhaseClock(agentBudget, verifyTimeout time.Duration) (*stagePhaseClock, *manualClock) {
	mc := newManualClock()
	c := newStagePhaseClock(agentBudget, verifyTimeout)
	c.now = mc.now
	return c, mc
}

// TestStagePhaseClock_GateBudgetIsVerifyFormsTimesTimeout pins the exact gate
// budget (CF-C: any change to gateBudgetVerifyForms turns it RED — the
// expectations are literals, not re-derived from the constant) and the 10m
// default for an unset executor.verify.timeout.
func TestStagePhaseClock_GateBudgetIsVerifyFormsTimesTimeout(t *testing.T) {
	if got, want := newStagePhaseClock(time.Hour, 7*time.Minute).gateBudget, 119*time.Minute; got != want {
		t.Errorf("gate budget for verify.timeout=7m = %s, want %s (17 x 7m)", got, want)
	}
	if got, want := newStagePhaseClock(time.Hour, 0).gateBudget, 170*time.Minute; got != want {
		t.Errorf("gate budget for an unset verify.timeout = %s, want %s (17 x the 10m default)", got, want)
	}
	if got := newStagePhaseClock(time.Hour, time.Minute).agentBudget; got != time.Hour {
		t.Errorf("agent budget = %s, want 1h (observed, never charged to the gate)", got)
	}
}

// TestStagePhaseClock_GateBudgetVerifyFormsEnumeration pins the constant to
// its walked enumeration (approval condition C2): the table's executions must
// sum to gateBudgetVerifyForms, so dropping a form or raising a re-run bound
// (verifyTempFailMaxReruns feeds the table) turns it RED until the count is
// re-walked. C2 also floors the count at 9.
func TestStagePhaseClock_GateBudgetVerifyFormsEnumeration(t *testing.T) {
	sum := 0
	for _, f := range gateBudgetVerifyFormTable {
		if f.executions <= 0 || strings.TrimSpace(f.site) == "" {
			t.Errorf("form %+v: every enumerated form names its site and contributes >= 1 execution", f)
		}
		sum += f.executions
	}
	if sum != gateBudgetVerifyForms {
		t.Errorf("enumerated verify executions sum to %d, gateBudgetVerifyForms = %d: re-walk the gate sites and update both", sum, gateBudgetVerifyForms)
	}
	if len(gateBudgetVerifyFormTable) != 9 {
		t.Errorf("enumeration has %d forms, want 9 (one per walked site; see the gateBudgetVerifyForms doc comment)", len(gateBudgetVerifyFormTable))
	}
	if gateBudgetVerifyForms < 9 {
		t.Errorf("gateBudgetVerifyForms = %d, approval condition C2 requires >= 9", gateBudgetVerifyForms)
	}
}

// TestStagePhaseClock_GateRemainingIsIndependentOfAgentClock is the clock-level
// done-means: an agent phase that spends 59 of its 60 minutes leaves the gate
// its FULL budget (CF-A: a gateRemaining derived from the agent clock leaves
// ~1m and goes RED here).
func TestStagePhaseClock_GateRemainingIsIndependentOfAgentClock(t *testing.T) {
	c, mc := newTestPhaseClock(time.Hour, 10*time.Minute)
	c.beginAgent()
	mc.advance(59 * time.Minute)
	c.beginGate()
	if got := c.gateRemaining(); got != c.gateBudget {
		t.Errorf("gateRemaining after a 59m agent phase = %s, want the full gate budget %s", got, c.gateBudget)
	}
	if eff, clipped := c.gateVerifyTimeout(10 * time.Minute); eff != 10*time.Minute || clipped {
		t.Errorf("gateVerifyTimeout(10m) = (%s, %t), want (10m, false): agent time must not clip the verify", eff, clipped)
	}
	if snap := c.snapshot(); snap.GateRemaining != c.gateBudget || snap.PhaseElapsed != 0 {
		t.Errorf("snapshot = %+v, want the full gate budget remaining and a fresh phase clock", snap)
	}
}

// TestStagePhaseClock_VerifyChargesGateBudget: only startVerify..endVerify is
// charged; time between verifies (staging, commits, push work) is not.
func TestStagePhaseClock_VerifyChargesGateBudget(t *testing.T) {
	c, mc := newTestPhaseClock(time.Hour, 10*time.Minute)
	c.beginGate()
	mc.advance(20 * time.Minute) // uncharged plumbing
	c.startVerify()
	mc.advance(3 * time.Minute)
	c.endVerify()
	mc.advance(30 * time.Minute) // uncharged plumbing
	c.startVerify()
	mc.advance(4 * time.Minute)
	c.endVerify()
	if got, want := c.gateRemaining(), c.gateBudget-7*time.Minute; got != want {
		t.Errorf("gateRemaining = %s, want %s (budget minus the two verifies' 7m only)", got, want)
	}
	c.endVerify() // no verify in flight: charges nothing
	if got, want := c.gateRemaining(), c.gateBudget-7*time.Minute; got != want {
		t.Errorf("an endVerify with no verify in flight charged the budget: %s, want %s", got, want)
	}
}

// TestStagePhaseClock_GateVerifyTimeoutClips: a verify timeout above the
// remaining budget is clipped to it; one below is returned unclipped.
func TestStagePhaseClock_GateVerifyTimeoutClips(t *testing.T) {
	c, mc := newTestPhaseClock(time.Hour, time.Minute) // budget 17m
	c.beginGate()
	c.startVerify()
	mc.advance(16*time.Minute + 30*time.Second)
	c.endVerify()
	if eff, clipped := c.gateVerifyTimeout(time.Minute); eff != 30*time.Second || !clipped {
		t.Errorf("gateVerifyTimeout(1m) with 30s left = (%s, %t), want (30s, true)", eff, clipped)
	}
	if eff, clipped := c.gateVerifyTimeout(10 * time.Second); eff != 10*time.Second || clipped {
		t.Errorf("gateVerifyTimeout(10s) with 30s left = (%s, %t), want (10s, false)", eff, clipped)
	}
	if c.gateExhausted() {
		t.Error("gateExhausted with 30s left = true, want false")
	}
	// An in-flight verify is extrapolated: 20s into it only 10s remain.
	c.startVerify()
	mc.advance(20 * time.Second)
	if eff, clipped := c.gateVerifyTimeout(time.Minute); eff != 10*time.Second || !clipped {
		t.Errorf("gateVerifyTimeout(1m) 20s into a verify = (%s, %t), want (10s, true)", eff, clipped)
	}
}

// TestStagePhaseClock_ExhaustedClipsToZero: an overdrawn budget clips every
// verify to 0 (the caller must not spawn) and reports exhausted.
func TestStagePhaseClock_ExhaustedClipsToZero(t *testing.T) {
	c, mc := newTestPhaseClock(time.Hour, time.Minute)
	c.beginGate()
	c.startVerify()
	mc.advance(c.gateBudget + time.Minute) // a clipped kill lands slightly late
	c.endVerify()
	if eff, clipped := c.gateVerifyTimeout(time.Minute); eff != 0 || !clipped {
		t.Errorf("gateVerifyTimeout on an overdrawn budget = (%s, %t), want (0, true)", eff, clipped)
	}
	if !c.gateExhausted() {
		t.Error("gateExhausted on an overdrawn budget = false, want true")
	}
	// Exactly at zero is exhausted too.
	c2, mc2 := newTestPhaseClock(time.Hour, time.Minute)
	c2.beginGate()
	c2.startVerify()
	mc2.advance(c2.gateBudget)
	c2.endVerify()
	if !c2.gateExhausted() {
		t.Error("gateExhausted with exactly 0 left = false, want true")
	}
}

// TestStagePhaseClock_BeginGateResetsCharge: each gate phase starts with the
// full budget and no in-flight verify, even after an exhausted one.
func TestStagePhaseClock_BeginGateResetsCharge(t *testing.T) {
	c, mc := newTestPhaseClock(time.Hour, time.Minute)
	c.beginGate()
	c.startVerify()
	mc.advance(c.gateBudget * 2)
	c.endVerify()
	c.beginAgent()
	mc.advance(time.Minute)
	c.startVerify() // a stray in-flight mark must not survive beginGate
	c.beginGate()
	mc.advance(5 * time.Minute)
	if got := c.gateRemaining(); got != c.gateBudget {
		t.Errorf("gateRemaining after beginGate = %s, want the full budget %s", got, c.gateBudget)
	}
	if snap := c.snapshot(); snap.Verifying || snap.GateRemaining != c.gateBudget {
		t.Errorf("snapshot after beginGate = %+v, want not verifying and the full budget", snap)
	}
	if c.gateExhausted() {
		t.Error("gateExhausted after beginGate = true, want false")
	}
}

// TestStagePhaseClock_NilClockIsNoOp: a nil clock (no clock on ctx) leaves
// every timeout unclipped, never exhausts and snapshots to the zero value.
func TestStagePhaseClock_NilClockIsNoOp(t *testing.T) {
	var c *stagePhaseClock
	c.beginAgent()
	c.beginGate()
	c.startVerify()
	c.endVerify()
	if eff, clipped := c.gateVerifyTimeout(5 * time.Minute); eff != 5*time.Minute || clipped {
		t.Errorf("nil gateVerifyTimeout(5m) = (%s, %t), want (5m, false)", eff, clipped)
	}
	if c.gateExhausted() {
		t.Error("nil gateExhausted = true, want false")
	}
	if c.gateRemaining() != 0 || c.gateBudgetTotal() != 0 {
		t.Error("nil gateRemaining / gateBudgetTotal must be 0")
	}
	if snap := c.snapshot(); snap != (stagePhaseSnapshot{}) {
		t.Errorf("nil snapshot = %+v, want the zero snapshot", snap)
	}
	if got := stagePhasesFrom(context.Background()); got != nil {
		t.Errorf("stagePhasesFrom(no clock) = %v, want nil", got)
	}
	clk := newStagePhaseClock(time.Hour, time.Minute)
	if got := stagePhasesFrom(withStagePhases(context.Background(), clk)); got != clk {
		t.Error("stagePhasesFrom did not return the clock withStagePhases carried")
	}
}

// TestStagePhaseClock_ReportsEachTransition: the report hook sees every
// transition, in order, with its snapshot.
func TestStagePhaseClock_ReportsEachTransition(t *testing.T) {
	c, mc := newTestPhaseClock(time.Hour, time.Minute)
	var got []stagePhaseSnapshot
	c.report = func(s stagePhaseSnapshot) { got = append(got, s) }
	c.beginAgent()
	mc.advance(2 * time.Minute)
	c.beginGate()
	c.startVerify()
	mc.advance(30 * time.Second)
	c.endVerify()

	want := []struct {
		phase     string
		verifying bool
	}{{stagePhaseAgent, false}, {stagePhaseGate, false}, {stagePhaseGate, true}, {stagePhaseGate, false}}
	if len(got) != len(want) {
		t.Fatalf("reported %d transitions, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Phase != w.phase || got[i].Verifying != w.verifying {
			t.Errorf("report %d = %+v, want phase=%s verifying=%t", i, got[i], w.phase, w.verifying)
		}
	}
	if got[0].GateBudget != 0 || got[0].GateRemaining != 0 {
		t.Errorf("agent-phase report carries gate fields: %+v", got[0])
	}
	if got[3].GateRemaining != c.gateBudget-30*time.Second || got[3].PhaseElapsed != 30*time.Second {
		t.Errorf("post-verify report = %+v, want remaining budget-30s and gate elapsed 30s", got[3])
	}
}

// TestStagePhaseClock_SnapshotPhaseElapsedAndExtrapolation: the agent phase
// reports time since beginAgent; the gate phase reports time since beginGate
// and, while verifying, a remainder extrapolated by the in-flight verify.
func TestStagePhaseClock_SnapshotPhaseElapsedAndExtrapolation(t *testing.T) {
	c, mc := newTestPhaseClock(time.Hour, time.Minute)
	if snap := c.snapshot(); snap != (stagePhaseSnapshot{}) {
		t.Errorf("snapshot before any transition = %+v, want zero", snap)
	}
	c.beginAgent()
	mc.advance(90 * time.Second)
	if snap := c.snapshot(); snap.Phase != stagePhaseAgent || snap.PhaseElapsed != 90*time.Second {
		t.Errorf("agent snapshot = %+v, want agent / 90s", snap)
	}
	c.beginGate()
	mc.advance(time.Minute)
	c.startVerify()
	mc.advance(2 * time.Minute)
	snap := c.snapshot()
	if snap.Phase != stagePhaseGate || snap.PhaseElapsed != 3*time.Minute || !snap.Verifying ||
		snap.GateBudget != c.gateBudget || snap.GateRemaining != c.gateBudget-2*time.Minute {
		t.Errorf("gate snapshot = %+v, want gate / 3m elapsed / verifying / remaining budget-2m", snap)
	}
}

// TestGateTimedOutReason_NamesGateBudgetWhenExhausted: an exhausted gate
// budget renders the gate-budget reason, still LEADING with
// verifyGateTimedOutLead (the failuresig anchor); a live or absent clock keeps
// verifyTimedOutReason byte-for-byte.
func TestGateTimedOutReason_NamesGateBudgetWhenExhausted(t *testing.T) {
	c, mc := newTestPhaseClock(time.Hour, time.Minute)
	c.beginGate()
	live := withStagePhases(context.Background(), c)
	legacy := verifyTimedOutReason("scripts/test verify", verifyFormFull, time.Minute, 2)
	if got := gateTimedOutReason(live, "scripts/test verify", verifyFormFull, time.Minute, 2); got != legacy {
		t.Errorf("live budget reason = %q, want verifyTimedOutReason %q", got, legacy)
	}
	if got := gateTimedOutReason(context.Background(), "scripts/test verify", verifyFormFull, time.Minute, 2); got != legacy {
		t.Errorf("no-clock reason = %q, want verifyTimedOutReason %q", got, legacy)
	}

	c.startVerify()
	mc.advance(c.gateBudget)
	c.endVerify()
	got := gateTimedOutReason(live, "scripts/test verify", verifyFormFull, time.Minute, 2)
	if !strings.HasPrefix(got, verifyGateTimedOutLead+":") {
		t.Errorf("exhausted reason = %q, must begin with %q (failuresig.AnchorVerifyGateTimedOut)", got, verifyGateTimedOutLead)
	}
	for _, want := range []string{"gate budget", "17m0s", "17 x executor.verify.timeout", "attempt 2", "raise executor.verify.timeout"} {
		if !strings.Contains(got, want) {
			t.Errorf("exhausted reason = %q, want it to name %q", got, want)
		}
	}
	if got == legacy {
		t.Error("exhausted reason is the agent-free verifyTimedOutReason; it must name the gate budget")
	}
}

// TestGateBudgetTimeoutTrailer: the clipped-verify trailer keeps the
// no-verdict marker and lead, names the gate budget and the clipped timeout,
// and quotes the last captured line.
func TestGateBudgetTimeoutTrailer(t *testing.T) {
	out := "=== RUN TestX\n>> > ./runner\n\n"
	got := gateBudgetTimeoutTrailer("scripts/test verify", verifyFormScoped, 17*time.Minute, 30*time.Second, 31*time.Second, out)
	for _, want := range []string{
		"--- fishhawk-runner: verify TERMINATED, no verdict ---",
		verifyGateTimedOutLead + ":",
		"gate budget (17m0s = 17 x executor.verify.timeout",
		"had only 30s left",
		"after 31s",
		`last captured line: ">> > ./runner"`,
		"the ABSENCE of FAIL lines proves nothing",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("trailer missing %q:\n%s", want, got)
		}
	}
}

// TestVerifyRunEventGateBudgetExhausted: the record keeps every timed-out key
// and adds gate_budget_seconds + gate_budget_exhausted:true.
func TestVerifyRunEventGateBudgetExhausted(t *testing.T) {
	ev := verifyRunEventGateBudgetExhausted("scripts/test verify", "head", "tree", "out", 30*time.Second, 31*time.Second, 17*time.Minute)
	if ev.Kind != "verify_run" {
		t.Fatalf("kind = %q, want verify_run", ev.Kind)
	}
	var p map[string]any
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	want := map[string]any{
		"command": "scripts/test verify", "head_sha": "head", "tree_sha": "tree", "exit_code": float64(-1),
		"output": "out", "outcome": "failed", "timed_out": true, "timeout_seconds": float64(30),
		"elapsed_seconds": float64(31), "gate_budget_seconds": float64(1020), "gate_budget_exhausted": true,
	}
	for k, v := range want {
		if p[k] != v {
			t.Errorf("payload[%q] = %v, want %v", k, p[k], v)
		}
	}
}
