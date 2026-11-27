package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// Fix-up `reason` is strictly audit-only (#5561). This test crosses the REAL
// fix-up handler, the persisted stage_fixup_triggered payload, the prompt-serve
// resolvers and the renderer, and pins the channel split on the served signed
// prompt: operator_concern reaches the agent, `reason` does not.

// TestFixupPrompt_ReasonNeverReachesServedPrompt routes a reason that is
// deliberately OBLIGATION-SHAPED (recording verb "report", report surface "run
// log"), because that is the shape that used to be echoed in the "Reporting
// obligations" block.
//
// Counterfactual: re-add reason as a fixupobligation source on the prompt-serve
// path and the REASON-SENTINEL absence assertion goes RED.
func TestFixupPrompt_ReasonNeverReachesServedPrompt(t *testing.T) {
	const (
		operatorSentinel = "OPERATOR-SENTINEL-5561"
		reasonSentinel   = "REASON-SENTINEL-5561"
		operatorConcern  = operatorSentinel + ": guard the nil pool before the retry"
		reason           = "Report the " + reasonSentinel + " output in the run log."
	)
	trigger := routeFixupWireTriggerWith(t, func(ids []string) fixupRequest {
		return fixupRequest{ConcernIDs: ids, OperatorConcern: operatorConcern, Reason: reason}
	})

	resp := serveFixupWirePrompt(t, trigger)

	section := fixupConcernsSection(resp.Prompt)
	if !strings.Contains(section, operatorSentinel) {
		t.Errorf("served Fix-up concerns section lacks the operator_concern %q:\n%s", operatorSentinel, resp.Prompt)
	}
	if strings.Contains(resp.Prompt, reasonSentinel) {
		t.Errorf("the audit-only reason %q reached the served fix-up prompt:\n%s", reasonSentinel, resp.Prompt)
	}
	var payload struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(trigger.Payload, &payload); err != nil {
		t.Fatalf("decode trigger payload: %v", err)
	}
	if payload.Reason != reason {
		t.Errorf("persisted reason = %q, want it still recorded for audit as %q", payload.Reason, reason)
	}
}
