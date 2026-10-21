package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// TestRun_ReportsAgentThenGatePhase drives run() end to end (#4023): the fake
// backend mints the MCP token and records every stage-progress report, the
// fake agent exits, and the spec-sourced verify command runs on the
// working-tree gate. The recorded reports must announce the agent phase
// before any gate report, a gate report before the first verifying one, and a
// not-verifying gate report after it. Counterfactual CF-E: deleting run()'s
// beginGate after the primary invocation leaves the verify reported in the
// agent phase, so no gate report is ever recorded.
func TestRun_ReportsAgentThenGatePhase(t *testing.T) {
	verifyTimeout := scaledD(10 * time.Second)
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:              "22222222-3333-4444-5555-666666666666",
		StageType:            "plan",
		Prompt:               "Hello agent.",
		PromptHash:           "deadbeef",
		VerifyCommand:        "true",
		VerifyTimeoutSeconds: int(verifyTimeout / time.Second),
	}
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "plan",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}

	reports := fu.progressArgs()
	firstAgent, firstGate, firstVerifying, gateAfterVerify := -1, -1, -1, -1
	for i, r := range reports {
		if r.MCPToken == "" {
			t.Errorf("report %d carried no bearer token", i)
		}
		switch {
		case r.Phase == stagePhaseAgent && firstAgent == -1:
			firstAgent = i
		case r.Phase == stagePhaseGate && r.GateVerifying && firstVerifying == -1:
			firstVerifying = i
		case r.Phase == stagePhaseGate && !r.GateVerifying && firstVerifying == -1 && firstGate == -1:
			firstGate = i
		case r.Phase == stagePhaseGate && !r.GateVerifying && firstVerifying != -1 && gateAfterVerify == -1:
			gateAfterVerify = i
		}
	}
	if firstAgent == -1 || firstGate == -1 || firstVerifying == -1 || gateAfterVerify == -1 {
		t.Fatalf("reports missing a transition (agent=%d gate=%d verifying=%d gate-after=%d): %+v",
			firstAgent, firstGate, firstVerifying, gateAfterVerify, reports)
	}
	if firstAgent >= firstGate || firstGate >= firstVerifying || firstVerifying >= gateAfterVerify {
		t.Errorf("phase order agent=%d < gate=%d < verifying=%d < gate-after=%d violated: %+v",
			firstAgent, firstGate, firstVerifying, gateAfterVerify, reports)
	}
	if r := reports[firstAgent]; r.LastEvent != phaseEventAgentStart {
		t.Errorf("agent report last_event = %q, want %q", r.LastEvent, phaseEventAgentStart)
	}
	if r := reports[firstVerifying]; r.LastEvent != phaseEventGateVerify {
		t.Errorf("verifying report last_event = %q, want %q", r.LastEvent, phaseEventGateVerify)
	}
	// Every gate report names the budget the stage clock derived from the
	// spec's executor.verify.timeout.
	wantBudget := int(verifyTimeout * gateBudgetVerifyForms / time.Second)
	for _, i := range []int{firstGate, firstVerifying, gateAfterVerify} {
		if r := reports[i]; r.GateBudgetSeconds == nil || *r.GateBudgetSeconds != wantBudget {
			t.Errorf("report %d gate_budget_seconds = %v, want %d", i, r.GateBudgetSeconds, wantBudget)
		}
	}
}

// recordPhases wires a recording report hook onto a manual-time stage clock
// and returns the ctx carrying it plus the recorded phase sequence.
func recordPhases(t *testing.T) (context.Context, *stagePhaseClock, *manualClock, *[]string) {
	t.Helper()
	clock, mc := newTestPhaseClock(time.Hour, time.Minute)
	var phases []string
	clock.report = func(s stagePhaseSnapshot) { phases = append(phases, s.Phase) }
	return withStagePhases(context.Background(), clock), clock, mc, &phases
}

// TestReinvokeOnBaseRebaseConflict_BracketsAgentThenGatePhase (#4023): the
// base-rebase re-land is its own agent phase, and the retried push's strict
// re-verify runs in a FRESH gate phase with its full budget — on success, and
// after every infra-retry attempt failed. The prior gate phase's charge must
// not survive the re-land.
func TestReinvokeOnBaseRebaseConflict_BracketsAgentThenGatePhase(t *testing.T) {
	orig := checkoutRunBranch
	checkoutRunBranch = func(_ context.Context, _, _ string) error { return nil }
	t.Cleanup(func() { checkoutRunBranch = orig })
	cfg := config{
		runID:      "11111111-2222-3333-4444-555555555555",
		stageID:    "22222222-3333-4444-5555-666666666666",
		workingDir: t.TempDir(),
	}

	for _, tc := range []struct {
		name       string
		invokeErr  error
		wantPhases []string
	}{
		{"success", nil, []string{stagePhaseAgent, stagePhaseGate}},
		{"every attempt failed", errors.New("agent api 529"),
			append(slices.Repeat([]string{stagePhaseAgent}, maxFixInvokeInfraRetries), stagePhaseGate)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, clock, mc, phases := recordPhases(t)
			clock.beginGate()
			clock.startVerify()
			mc.advance(5 * time.Minute)
			clock.endVerify()
			*phases = nil

			var atInvoke []string
			fi := &fakeInvoker{
				canned:    agent.Result{OK: true},
				returnErr: tc.invokeErr,
				onInvoke: func(int, agent.Invocation) {
					atInvoke = append(atInvoke, clock.snapshot().Phase)
				},
			}
			res := agent.Result{}
			err := reinvokeOnBaseRebaseConflict(ctx, cfg, fi, agent.Invocation{}, &res, gitops.ErrBaseRebaseConflict, io.Discard)
			if (err != nil) != (tc.invokeErr != nil) {
				t.Fatalf("reinvokeOnBaseRebaseConflict err = %v, want error=%t", err, tc.invokeErr != nil)
			}
			if strings.Join(*phases, ",") != strings.Join(tc.wantPhases, ",") {
				t.Errorf("reported phases = %v, want %v", *phases, tc.wantPhases)
			}
			for i, p := range atInvoke {
				if p != stagePhaseAgent {
					t.Errorf("invoke %d ran in phase %q, want agent", i, p)
				}
			}
			if snap := clock.snapshot(); snap.Phase != stagePhaseGate || snap.GateRemaining != clock.gateBudget {
				t.Errorf("after the re-land snapshot = %+v, want a fresh gate phase with the full budget %s", snap, clock.gateBudget)
			}
		})
	}
}

// TestAttemptDeterministicFixup_BeginsGatePhase (#4023): no agent runs on the
// deterministic apply path, so its gate starts a fresh gate phase — the apply
// gate sees the gate phase with its FULL budget, not the agent phase (or a
// prior gate phase's charge) the clock was left in.
func TestAttemptDeterministicFixup_BeginsGatePhase(t *testing.T) {
	swapFixupApplySeams(t, nil, "tree-abc", nil, nil)
	ctx, clock, mc, phases := recordPhases(t)
	clock.beginGate()
	clock.startVerify()
	mc.advance(5 * time.Minute)
	clock.endVerify()
	clock.beginAgent()
	mc.advance(time.Minute)
	*phases = nil

	var atGate stagePhaseSnapshot
	fixupVerifyGate = func(gctx context.Context, _ config, _ io.Writer) ([]agent.Event, string, error) {
		atGate = stagePhasesFrom(gctx).snapshot()
		return []agent.Event{{Kind: "verify_run"}}, "tree-abc", nil
	}
	var log bytes.Buffer
	applied, _, _, ap := attemptDeterministicFixup(ctx, config{runID: "r", stageID: "s", workingDir: t.TempDir()},
		[]upload.FixupApplyPatch{{Patch: "diff1"}}, "tip-sha", &log)
	if !applied || ap != "applied" {
		t.Fatalf("applied/applyPath = %t/%q, want true/applied", applied, ap)
	}
	if atGate.Phase != stagePhaseGate || atGate.GateRemaining != clock.gateBudget || atGate.PhaseElapsed != 0 {
		t.Errorf("apply gate ran in %+v, want a fresh gate phase with the full budget %s", atGate, clock.gateBudget)
	}
	if strings.Join(*phases, ",") != stagePhaseGate {
		t.Errorf("reported phases = %v, want [gate]", *phases)
	}
}
