package main

import (
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
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
