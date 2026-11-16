package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"
)

// Unknown backend metadata is data, not permission, and must remain bounded
// with an honest computed elision if it cannot fit the response skeleton.
func TestReviewFirst_MetadataBudgetAndElision(t *testing.T) {
	view := reviewFirstWire(t)["waiting"]
	view.Stages.Items[0].ReviewGateAdmission = strings.Repeat("unknown-routing-", 10000)
	in := GetRunStatusOutput{Run: *view.Run, Stages: view.Stages.Items, NextActions: reviewFirstActions(view)}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= 4096 {
		t.Fatal("fixture is not over budget")
	}
	skeleton, _, err := skeletonRunStatus(in, view.Run.ID, fixedBudget(1<<30))
	if err != nil {
		t.Fatal(err)
	}
	skeletonBytes, err := json.Marshal(skeleton)
	if err != nil {
		t.Fatal(err)
	}
	budget := len(skeletonBytes)
	if budget < minimalRunStatusMaxBytes {
		budget = minimalRunStatusMaxBytes
	}
	out, err := boundRunStatusOutput(in, view.Run.ID, fixedBudget(budget))
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > budget {
		t.Fatalf("response=%d bytes, budget%d", len(raw), budget)
	}
	found := false
	if out.Elisions != nil {
		for _, f := range out.Elisions.Fields {
			if f.Field == "stages[].review_gate_admission" {
				found = true
				if f.Class != "computed" {
					t.Fatalf("routing omission class=%s, want computed", f.Class)
				}
			}
		}
	}
	if !found {
		t.Fatalf("missing computed routing omission: %+v", out.Elisions)
	}
	if out.NextActions != nil {
		for _, a := range out.NextActions.Actions {
			if a.Action == "approve_review_gate" {
				t.Fatal("unknown metadata gained approval during bounding")
			}
		}
	}
}
