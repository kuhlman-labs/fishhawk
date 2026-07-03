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

// TestFiledWorkItem_DecodesLabelCompleteness pins the MCP-side wire contract
// for the #1616 LOUD label-completeness report: a work-items response carrying
// defaulted_labels + missing_label_namespaces decodes into FiledWorkItem so the
// tool result surfaces them verbatim to the operator.
func TestFiledWorkItem_DecodesLabelCompleteness(t *testing.T) {
	const body = `{"type":"feature","title":"[E22.1] x","number":7,` +
		`"url":"https://example/7","provider":"github_projects",` +
		`"applied_labels":["type:feature","autonomy:medium"],` +
		`"defaulted_labels":["autonomy:medium"],` +
		`"missing_label_namespaces":["area"],"boarded":true,"epic_linked":true,"audited":false}`
	var got FiledWorkItem
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode FiledWorkItem: %v", err)
	}
	if len(got.DefaultedLabels) != 1 || got.DefaultedLabels[0] != "autonomy:medium" {
		t.Errorf("DefaultedLabels = %v, want [autonomy:medium]", got.DefaultedLabels)
	}
	if len(got.MissingLabelNamespaces) != 1 || got.MissingLabelNamespaces[0] != "area" {
		t.Errorf("MissingLabelNamespaces = %v, want [area]", got.MissingLabelNamespaces)
	}
}

// TestDeferFiledIssue_DecodesLabelCompleteness pins the same wire contract on
// the defer path's filed-issue block (#1616).
func TestDeferFiledIssue_DecodesLabelCompleteness(t *testing.T) {
	const body = `{"type":"chore","title":"[E22.4] x","number":9,` +
		`"url":"https://example/9","provider":"github_projects",` +
		`"applied_labels":["type:chore","autonomy:medium"],` +
		`"defaulted_labels":["autonomy:medium"],"missing_label_namespaces":["area"]}`
	var got DeferFiledIssue
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode DeferFiledIssue: %v", err)
	}
	if len(got.DefaultedLabels) != 1 || got.DefaultedLabels[0] != "autonomy:medium" {
		t.Errorf("DefaultedLabels = %v, want [autonomy:medium]", got.DefaultedLabels)
	}
	if len(got.MissingLabelNamespaces) != 1 || got.MissingLabelNamespaces[0] != "area" {
		t.Errorf("MissingLabelNamespaces = %v, want [area]", got.MissingLabelNamespaces)
	}
}
