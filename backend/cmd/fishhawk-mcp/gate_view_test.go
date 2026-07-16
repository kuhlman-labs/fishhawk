package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// gateViewNote96Plus is deliberately longer than the compaction levers'
// 96-byte auditPayloadStringCap so a byte-identical round-trip through the
// client -> tool -> output seam proves none of compact.go's levers apply to
// this surface.
const gateViewNote96Plus = "The reviewer's full concern prose is intentionally longer than ninety-six bytes so any truncation or elision on the new gate-view surface would visibly alter the round-tripped note here."

// gateViewProbe records what the fake backend saw so a test can assert the
// stage_kind query forwarded correctly.
type gateViewProbe struct {
	mu            sync.Mutex
	calls         int
	lastRunID     string
	lastStageKind string
}

// newGateViewBackend serves GET /v0/runs/{run_id}/gate-view with the given
// status + payload, recording the request into the returned probe.
func newGateViewBackend(t *testing.T, status int, payload any) (*httptest.Server, *gateViewProbe) {
	t.Helper()
	probe := &gateViewProbe{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/runs/{run_id}/gate-view", func(w http.ResponseWriter, r *http.Request) {
		probe.mu.Lock()
		probe.calls++
		probe.lastRunID = r.PathValue("run_id")
		probe.lastStageKind = r.URL.Query().Get("stage_kind")
		probe.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, probe
}

// callGateView drives fishhawk_get_gate_view end-to-end through a real MCP
// CallTool over an in-memory transport against srv.
func callGateView(t *testing.T, srv *httptest.Server, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	ctx := context.Background()
	resolver := newResolver(srv, nil)

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0"}, nil)
	registerGetGateView(server, resolver)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSession.Close()

	res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_get_gate_view",
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	return res
}

// TestGetGateView_FullNoteByteIdentical (binding condition 2) drives the tool
// end-to-end and asserts a >96-byte concern note arrives byte-identical through
// the client -> tool -> output seam, proving stripReviewProse and the 96-byte
// auditPayloadStringCap do NOT apply to this surface.
func TestGetGateView_FullNoteByteIdentical(t *testing.T) {
	runID := uuid.New()
	payload := GateView{
		RunID: runID.String(),
		Open: []GateViewConcern{{
			ID:                   uuid.New().String(),
			StageKind:            "implement",
			Round:                2,
			OriginReviewSequence: 30,
			ReviewerModel:        "claude-opus-4-8",
			Severity:             "high",
			Category:             "correctness",
			State:                "reopened",
			Note:                 gateViewNote96Plus,
			Fixups: []GateViewFixup{{
				Sequence: 20, Reason: "route it back", Outcome: "pushed", ApplyPath: "applied", HeadSHA: "abc123",
			}},
		}},
		Settled:                 []GateViewSettledConcern{},
		SuppressedRelitigations: []GateViewSuppressedRelitig{},
	}
	srv, _ := newGateViewBackend(t, http.StatusOK, payload)

	res := callGateView(t, srv, map[string]any{"run_id": runID.String()})
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}
	if res.StructuredContent == nil {
		t.Fatal("StructuredContent is nil; the typed output did not serialize")
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal StructuredContent: %v", err)
	}
	// Byte-identical proof at the wire: the full note is present verbatim.
	if !strings.Contains(string(raw), gateViewNote96Plus) {
		t.Fatalf("full note did not survive the seam byte-identical:\n%s", string(raw))
	}
	var out GetGateViewOutput
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("decode GetGateViewOutput from wire: %v", uerr)
	}
	if out.GateView == nil || len(out.GateView.Open) != 1 {
		t.Fatalf("gate_view.open did not round-trip: %+v", out.GateView)
	}
	if got := out.GateView.Open[0].Note; got != gateViewNote96Plus {
		t.Errorf("note = %q, want the full %d-byte prose byte-identical", got, len(gateViewNote96Plus))
	}
	// The fix-up join fields survive too (full decision context, not just the note).
	fx := out.GateView.Open[0].Fixups
	if len(fx) != 1 || fx[0].Outcome != "pushed" || fx[0].ApplyPath != "applied" || fx[0].HeadSHA != "abc123" {
		t.Errorf("fixup join did not round-trip: %+v", fx)
	}
}

// TestGetGateView_StageKindForwarding asserts the optional stage_kind reaches
// the backend as a query parameter.
func TestGetGateView_StageKindForwarding(t *testing.T) {
	runID := uuid.New()
	srv, probe := newGateViewBackend(t, http.StatusOK, GateView{
		RunID:                   runID.String(),
		Open:                    []GateViewConcern{},
		Settled:                 []GateViewSettledConcern{},
		SuppressedRelitigations: []GateViewSuppressedRelitig{},
	})

	res := callGateView(t, srv, map[string]any{"run_id": runID.String(), "stage_kind": "implement"})
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if probe.lastStageKind != "implement" {
		t.Errorf("forwarded stage_kind = %q, want implement", probe.lastStageKind)
	}
	if probe.lastRunID != runID.String() {
		t.Errorf("forwarded run_id = %q, want %q", probe.lastRunID, runID.String())
	}
}

// TestGetGateView_ErrorMapping asserts a backend 404 / 503 surfaces as a tool
// error rather than a bogus empty success.
func TestGetGateView_ErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   string
	}{
		{"not found", http.StatusNotFound, "run_not_found"},
		{"unconfigured", http.StatusServiceUnavailable, "gate_view_unconfigured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID := uuid.New()
			envelope := map[string]any{"error": map[string]any{"code": tc.code, "message": tc.code}}
			srv, _ := newGateViewBackend(t, tc.status, envelope)

			res := callGateView(t, srv, map[string]any{"run_id": runID.String()})
			if !res.IsError {
				t.Fatalf("CallTool should surface a tool error for HTTP %d; got success: %+v", tc.status, res.StructuredContent)
			}
			var sb strings.Builder
			for _, c := range res.Content {
				if tc, ok := c.(*mcp.TextContent); ok {
					sb.WriteString(tc.Text)
				}
			}
			if !strings.Contains(sb.String(), tc.code) {
				t.Errorf("tool error should mention %q; got %q", tc.code, sb.String())
			}
		})
	}
}

// TestGetGateView_InvalidStageKind rejects a bad stage_kind locally before any
// backend round-trip.
func TestGetGateView_InvalidStageKind(t *testing.T) {
	runID := uuid.New()
	srv, probe := newGateViewBackend(t, http.StatusOK, GateView{})

	res := callGateView(t, srv, map[string]any{"run_id": runID.String(), "stage_kind": "deploy"})
	if !res.IsError {
		t.Fatalf("CallTool should reject an invalid stage_kind locally; got success")
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if probe.calls != 0 {
		t.Errorf("backend was called %d times; a bad stage_kind must fail before the round-trip", probe.calls)
	}
}
