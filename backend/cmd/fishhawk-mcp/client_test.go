package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// TestCreateCampaign_OperatorAgentBytes_OmittedWhenNil pins the apiClient wire
// contract for the OPTIONAL campaign-level operator_agent override (E25.12 /
// #1451) at the HTTP-body layer, below the tool handler: a nil/empty
// operatorAgent argument omits the field entirely (json.RawMessage + omitempty),
// so a campaign without an override sends NO operator_agent key — the
// byte-identical default where each issue-run inherits its workflow contract.
func TestCreateCampaign_OperatorAgentBytes_OmittedWhenNil(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, err := r.api.CreateCampaign(context.Background(), "x/y", "#1", "", nil)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if len(fb.createCampaignBody.OperatorAgent) != 0 {
		t.Errorf("operator_agent present on a nil override: %s", fb.createCampaignBody.OperatorAgent)
	}
}

// TestCreateCampaign_OperatorAgentBytes_CarriedVerbatim proves a non-nil
// operatorAgent argument travels in the POST body verbatim as opaque JSON (the
// client does not parse or validate it — the backend is the validation
// authority) and the created Campaign decodes back.
func TestCreateCampaign_OperatorAgentBytes_CarriedVerbatim(t *testing.T) {
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	override := map[string]any{"may_waive": "solo_low"}
	fb.createCampaignResp = Campaign{
		ID: id.String(), Repo: "x/y", EpicRef: "#25", State: "pending", PausePolicy: "pause_campaign",
		OperatorAgent: override,
	}
	r := newResolver(srv, nil)

	got, err := r.api.CreateCampaign(context.Background(), "x/y", "#25", "",
		json.RawMessage(`{"may_waive":"solo_low"}`))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	var sent map[string]any
	if err := json.Unmarshal(fb.createCampaignBody.OperatorAgent, &sent); err != nil {
		t.Fatalf("operator_agent body not valid JSON: %v", err)
	}
	if sent["may_waive"] != "solo_low" {
		t.Errorf("sent operator_agent.may_waive = %v, want solo_low", sent["may_waive"])
	}
	if got.OperatorAgent["may_waive"] != "solo_low" {
		t.Errorf("decoded Campaign.OperatorAgent = %+v", got.OperatorAgent)
	}
}
