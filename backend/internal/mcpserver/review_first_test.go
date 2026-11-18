package mcpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type reviewFirstWireView struct {
	Run    *Run `json:"run"`
	Stages struct {
		Items []Stage `json:"items"`
	} `json:"stages"`
}

func reviewFirstWire(t *testing.T) map[string]reviewFirstWireView {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "server", "testdata", "human_review_first_wire.json"))
	if err != nil {
		t.Fatal(err)
	}
	var views map[string]reviewFirstWireView
	if err := json.Unmarshal(raw, &views); err != nil {
		t.Fatal(err)
	}
	if views["waiting"].Run == nil || views["reviewing"].Run == nil {
		t.Fatal("missing server wire views")
	}
	return views
}
func reviewFirstActions(v reviewFirstWireView) *NextActions {
	return nextActionsFor(v.Run, v.Stages.Items, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
}
func TestNextActions_ReviewFirst_ServerWire(t *testing.T) {
	views := reviewFirstWire(t)
	for _, tc := range []struct{ name, state, action string }{{"waiting", "human_review_awaiting_pull_request", "attach_pull_request"}, {"reviewing", "human_review_pull_request", "review_pull_request"}} {
		t.Run(tc.name, func(t *testing.T) {
			na := reviewFirstActions(views[tc.name])
			if na.State != tc.state || len(na.Actions) == 0 || na.Actions[0].Action != tc.action {
				t.Fatalf("actions=%+v, want %s/%s", na, tc.state, tc.action)
			}
			for _, a := range na.Actions {
				if a.Action == "approve_review_gate" || a.Consumes == consumesApprovalSlot {
					t.Fatalf("PR-managed gate offers direct approval: %+v", a)
				}
			}
		})
	}
}
func TestNextActions_ReviewFirst_UnknownWireMetadataCannotApprove(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "server", "testdata", "human_review_first_wire.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"absent", "null", "empty", "unknown", "workflow_spec_unparseable"} {
		t.Run(value, func(t *testing.T) {
			var views map[string]json.RawMessage
			if err := json.Unmarshal(raw, &views); err != nil {
				t.Fatal(err)
			}
			var view map[string]any
			if err := json.Unmarshal(views["waiting"], &view); err != nil {
				t.Fatal(err)
			}
			stage := view["stages"].(map[string]any)["items"].([]any)[0].(map[string]any)
			switch value {
			case "absent":
				delete(stage, "review_gate_admission")
			case "null":
				stage["review_gate_admission"] = nil
			case "empty":
				stage["review_gate_admission"] = ""
			default:
				stage["review_gate_admission"] = value
			}
			encoded, err := json.Marshal(view)
			if err != nil {
				t.Fatal(err)
			}
			var decoded reviewFirstWireView
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			na := reviewFirstActions(decoded)
			if na.State != "human_review_gate_unresolved" {
				t.Fatalf("%s metadata produced %+v", value, na)
			}
			for _, a := range na.Actions {
				if a.Consumes == consumesApprovalSlot || a.Action == "approve_review_gate" {
					t.Fatalf("unresolved gate offers approval: %+v", a)
				}
			}
		})
	}
}
func TestNextActions_ReviewFirst_InterruptedInitializationNeedsAttachmentReplay(t *testing.T) {
	for _, state := range []string{"pending", "dispatched", "running"} {
		t.Run(state, func(t *testing.T) {
			view := reviewFirstWire(t)["reviewing"]
			view.Stages.Items[0].State = state
			na := reviewFirstActions(view)
			if na.State != "human_review_awaiting_pull_request" || na.Actions[0].Action != "attach_pull_request" {
				t.Fatalf("interrupted stage: %+v", na)
			}
		})
	}
}

func TestNextActions_ReviewFirst_DeclarationOwnsOperatorGateWithLinkedPR(t *testing.T) {
	run, stages := humanReviewGateWire(t)
	pr := "https://github.com/other/widgets/pull/42"
	run.PullRequestURL = &pr
	na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
	if na.State != "human_review_gate_parked" || na.Actions[0].Action != "approve_review_gate" {
		t.Fatalf("linked PR replaced declared operator confirmation: %+v", na)
	}
}
