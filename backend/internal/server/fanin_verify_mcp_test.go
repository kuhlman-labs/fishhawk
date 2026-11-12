package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/mcpserver"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// runStatusActions calls the REGISTERED fishhawk_get_run_status tool over the
// SDK's in-memory transport against a real server and returns its next_actions
// action names plus the decoded run block.
func runStatusActions(t *testing.T, s *Server, runID string) (actions []string, runBlock map[string]json.RawMessage) {
	t.Helper()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	ctx := context.Background()
	mcpSrv := mcpserver.NewServer(mcpserver.Config{BackendURL: ts.URL, APIToken: "tok-unused-without-auth-wiring"})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ss, err := mcpSrv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("mcp server connect: %v", err)
	}
	defer func() { _ = ss.Close() }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "fanin-unavailable-e2e", Version: "0"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("mcp client connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_get_run_status",
		Arguments: map[string]any{"run_id": runID},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_get_run_status: %v", err)
	}
	if res.IsError {
		t.Fatalf("fishhawk_get_run_status returned a tool error: %s", toolText(res))
	}
	structured, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out struct {
		Run         map[string]json.RawMessage `json:"run"`
		NextActions *struct {
			Actions []struct {
				Action   string `json:"action"`
				Consumes string `json:"consumes"`
			} `json:"actions"`
		} `json:"next_actions"`
	}
	if err := json.Unmarshal(structured, &out); err != nil {
		t.Fatalf("decode tool output %s: %v", structured, err)
	}
	if out.NextActions == nil {
		t.Fatalf("tool output carries no next_actions: %s", structured)
	}
	for _, a := range out.NextActions.Actions {
		actions = append(actions, a.Action)
	}
	return actions, out.Run
}

// TestFanInVerifyUnavailable_ServerThroughMCP (#4792, approval condition C2)
// drives the REAL producer — the server hook and orchestrator releasing a
// forge-less decomposed parent's acceptance — and reads the result through the
// REGISTERED fishhawk_get_run_status tool pointed at that same server. The
// advisory must reach next_actions while every pre-existing action keeps its
// place and order; nothing here is hand-written server JSON, so a mirror tag
// that does not byte-match, or an advisory that is not wired, goes RED.
// COUNTERFACTUAL: change the mcpserver.Run.FanInVerifyUnavailable json tag, or
// delete the fold's call sites in nextActionsFor → the advisory never reaches
// next_actions, RED.
func TestFanInVerifyUnavailable_ServerThroughMCP(t *testing.T) {
	f, acc, o := failOpenParent(t)
	if _, err := o.Advance(context.Background(), f.parent.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	f.s.waitBackgroundReviews()
	if got := f.state(acc.ID); got != run.StageStateAwaitingHostDispatch {
		t.Fatalf("acceptance state = %q, want awaiting_host_dispatch (released)", got)
	}
	if n := f.count(orchestrator.CategoryFanInVerifyUnavailable); n != 1 {
		t.Fatalf("fan_in_verify_unavailable rows = %d, want 1", n)
	}

	with, runBlock := runStatusActions(t, f.s, f.parent.ID.String())
	raw, ok := runBlock["fan_in_verify_unavailable"]
	if !ok {
		t.Fatalf("tool run block lacks fan_in_verify_unavailable (the mirror did not decode the server's key); run keys decoded: %d", len(runBlock))
	}
	var block struct {
		Reason            string `json:"reason"`
		AcceptanceStageID string `json:"acceptance_stage_id"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		t.Fatalf("decode block %s: %v", raw, err)
	}
	if block.Reason != orchestrator.FanInVerifyUnavailableForgeNotWired || block.AcceptanceStageID != acc.ID.String() {
		t.Errorf("tool block = %+v, want forge_not_wired on acceptance stage %s", block, acc.ID)
	}

	// The SAME server with the signal's row removed is the baseline: the
	// advisory is exactly one appended action and nothing else moves.
	f.au.mu.Lock()
	kept := f.au.appended[:0:0]
	for _, p := range f.au.appended {
		if p.Category != orchestrator.CategoryFanInVerifyUnavailable {
			kept = append(kept, p)
		}
	}
	f.au.appended = kept
	f.au.mu.Unlock()
	without, runBlockWithout := runStatusActions(t, f.s, f.parent.ID.String())
	if _, ok := runBlockWithout["fan_in_verify_unavailable"]; ok {
		t.Fatalf("baseline run block still carries fan_in_verify_unavailable after the row was removed")
	}
	if len(without) == 0 {
		t.Fatal("baseline next_actions is empty; the fixture must carry pre-existing actions")
	}
	want := append(append([]string{}, without...), "fan_in_verify_unavailable")
	if !reflect.DeepEqual(with, want) {
		t.Errorf("next_actions = %v, want the baseline %v with fan_in_verify_unavailable appended", with, without)
	}
}
