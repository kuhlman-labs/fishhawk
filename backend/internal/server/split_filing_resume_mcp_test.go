package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/mcpserver"
)

// TestResumeSplitFiling_MCPToolDecodesRealHandler202 (V8, #3925) is the
// hermetic MCP → REST test: the REAL fishhawk_resume_split_filing tool calls
// the REAL route and handler over httptest, backed by the in-memory split
// fakes, with no database. It lives in package server because those fakes are
// _test.go files mcpserver cannot import. Every asserted field is decoded from
// the handler's ACTUAL body through client.go's ResumeSplitFilingResult, so a
// json-tag drift in that hand-maintained mirror zeroes a field here. The
// pgtest-backed twin is mcpserver TestResumeSplitFiling_RealServerDecodes202.
func TestResumeSplitFiling_MCPToolDecodesRealHandler202(t *testing.T) {
	ctx := context.Background()
	h := newResumeHarness(t, nil)
	h.seedPhaseMarker(0, 4001)
	// The harness builds its Server without a token repo, and New captures
	// the handler chain, so rebuild the chain (the same Server, registry and
	// fakes) once the write:approvals bearer is wired.
	tok := stubToken("write:approvals")
	h.s.cfg.APITokenRepo = tok

	ts := httptest.NewServer(h.s.buildHandler())
	defer ts.Close()

	mcpSrv := mcpserver.NewServer(mcpserver.Config{BackendURL: ts.URL, APIToken: tok.tok.PlainText})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ss, err := mcpSrv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("mcp server connect: %v", err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "resume-split-e2e", Version: "0"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("mcp client connect: %v", err)
	}
	defer cs.Close()

	result, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_resume_split_filing",
		Arguments: map[string]any{"run_id": h.runID.String()},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_resume_split_filing: %v", err)
	}
	if result.IsError {
		var msg string
		for _, c := range result.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				msg += tc.Text
			}
		}
		t.Fatalf("fishhawk_resume_split_filing returned a tool error: %s", msg)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out mcpserver.ResumeSplitFilingOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode tool output: %v\nraw: %s", err, raw)
	}

	if out.State != splitFilingResumeStateResuming {
		t.Errorf("state = %q, want %q\nraw: %s", out.State, splitFilingResumeStateResuming, raw)
	}
	if out.PhaseCount != 3 {
		t.Errorf("phase_count = %d, want 3\nraw: %s", out.PhaseCount, raw)
	}
	if out.AlreadyFiled != 1 {
		t.Errorf("already_filed = %d, want 1 (the seeded phase-0 marker)\nraw: %s", out.AlreadyFiled, raw)
	}
	if want := int(splitFilingBudgetFor(3) / time.Second); out.BudgetSeconds != want {
		t.Errorf("budget_seconds = %d, want %d\nraw: %s", out.BudgetSeconds, want, raw)
	}
	if out.StageID != h.planStage.ID.String() || out.RunID != h.runID.String() {
		t.Errorf("ids = %s/%s, want %s/%s", out.RunID, out.StageID, h.runID, h.planStage.ID)
	}

	h.s.waitSplitFiling()
	if got := h.provider.requestCount(); got != 2 {
		t.Errorf("File requests = %d, want 2 (phases 1 and 2 only)", got)
	}
	for _, title := range []string{"migrate consumers to NewFoo", "contract: delete the transitional Foo"} {
		if n := h.provider.countRequestsFor(title); n != 1 {
			t.Errorf("File requests for %q = %d, want 1", title, n)
		}
	}
	if n := h.provider.countRequestsFor("expand: add NewFoo alongside Foo"); n != 0 {
		t.Errorf("phase 0 (recorded) was filed again %d times", n)
	}
}
