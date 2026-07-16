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

// --- fishhawk_revive_run (#1915) ---

// reviveFakeBackend is a self-contained backend stub for the revive-run tool:
// it serves only POST /v0/runs/{run_id}/revive. reviveResp is the per-run
// success body; reviveStatus drives the HTTP status (default 200); reviveErrBody,
// when set, is written verbatim for the error-path tests. reviveCalledByID
// counts calls per run id so tests assert the tool did (or did not) reach the
// backend.
type reviveFakeBackend struct {
	mu               sync.Mutex
	reviveResp       map[uuid.UUID]ReviveRunResult
	reviveStatus     int
	reviveErrBody    string
	reviveCalledByID map[uuid.UUID]int
}

func newReviveFakeBackend(t *testing.T) (*reviveFakeBackend, *httptest.Server) {
	fb := &reviveFakeBackend{
		reviveResp:       map[uuid.UUID]ReviveRunResult{},
		reviveStatus:     http.StatusOK,
		reviveCalledByID: map[uuid.UUID]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/revive", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("run_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.reviveCalledByID[id]++
		status := fb.reviveStatus
		errBody := fb.reviveErrBody
		resp, ok := fb.reviveResp[id]
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if !ok {
			resp = ReviveRunResult{Run: Run{ID: id.String(), State: "running"}}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fb, srv
}

// TestReviveRun_HappyPath_SurfacesRunStagesAndHint asserts the tool -> client
// -> HTTP boundary: a successful revive returns the re-opened run (running),
// the per-stage re-park summary, and the constant no-dispatch next_step hint.
func TestReviveRun_HappyPath_SurfacesRunStagesAndHint(t *testing.T) {
	fb, srv := newReviveFakeBackend(t)
	runID := uuid.New()
	stageID := uuid.New()
	fb.reviveResp[runID] = ReviveRunResult{
		Run: Run{ID: runID.String(), State: "running"},
		RestoredStages: []ReviveRestoredStage{{
			StageID:       stageID.String(),
			Type:          "implement",
			PriorCategory: "A",
			PriorReason:   "agent failure",
			RestoredState: "pending",
		}},
	}
	r := newResolver(srv, nil)

	_, out, err := r.reviveRun(context.Background(), nil, ReviveRunInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("reviveRun: %v", err)
	}
	if out.Run.State != "running" {
		t.Errorf("Run.State = %q, want running", out.Run.State)
	}
	if len(out.RestoredStages) != 1 || out.RestoredStages[0].StageID != stageID.String() {
		t.Errorf("RestoredStages = %+v, want one restore for %s", out.RestoredStages, stageID)
	}
	if out.RestoredStages[0].RestoredState != "pending" {
		t.Errorf("restored_state = %q, want pending", out.RestoredStages[0].RestoredState)
	}
	if out.NextStep != reviveNextStepHint {
		t.Errorf("NextStep = %q, want the constant no-dispatch hint", out.NextStep)
	}
	// The hint must convey the load-bearing no-dispatch semantics.
	if !strings.Contains(strings.ToLower(out.NextStep), "without dispatching") {
		t.Errorf("NextStep does not convey the no-dispatch semantics: %q", out.NextStep)
	}
	if fb.reviveCalledByID[runID] != 1 {
		t.Errorf("revive called %d times, want 1", fb.reviveCalledByID[runID])
	}
}

// TestReviveRun_InvalidUUID_FailsLocally asserts the run_id UUID guard fails
// before any HTTP hop.
func TestReviveRun_InvalidUUID_FailsLocally(t *testing.T) {
	fb, srv := newReviveFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.reviveRun(context.Background(), nil, ReviveRunInput{RunID: "not-a-uuid"})
	if err == nil || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Fatalf("err = %v, want UUID parse error", err)
	}
	if len(fb.reviveCalledByID) != 0 {
		t.Errorf("backend revive called %d times, want 0", len(fb.reviveCalledByID))
	}
}

// TestReviveRun_NotApplicable_PropagatesAs422 asserts the backend's
// revive_not_applicable 422 surfaces as a tool error (the non-failed /
// non-retryable-stage refusal path).
func TestReviveRun_NotApplicable_PropagatesAs422(t *testing.T) {
	fb, srv := newReviveFakeBackend(t)
	fb.reviveStatus = http.StatusUnprocessableEntity
	fb.reviveErrBody = `{"error":{"code":"revive_not_applicable","message":"run is in state \"succeeded\" (only failed runs can be revived)"}}`
	r := newResolver(srv, nil)

	_, _, err := r.reviveRun(context.Background(), nil, ReviveRunInput{RunID: uuid.NewString()})
	if err == nil || !strings.Contains(err.Error(), "revive_not_applicable") {
		t.Fatalf("err = %v, want revive_not_applicable", err)
	}
}

// TestReviveRun_AgentTokenForbidden_PropagatesAs403 asserts the backend's
// operator-only guard (an agent/mcp token) surfaces as a tool error.
func TestReviveRun_AgentTokenForbidden_PropagatesAs403(t *testing.T) {
	fb, srv := newReviveFakeBackend(t)
	fb.reviveStatus = http.StatusForbidden
	fb.reviveErrBody = `{"error":{"code":"agent_token_forbidden","message":"revive is an operator action; agent (mcp) tokens may not revive any run"}}`
	r := newResolver(srv, nil)

	_, _, err := r.reviveRun(context.Background(), nil, ReviveRunInput{RunID: uuid.NewString()})
	if err == nil || !strings.Contains(err.Error(), "agent_token_forbidden") {
		t.Fatalf("err = %v, want agent_token_forbidden", err)
	}
}

// TestReviveRun_AuditWarning_ThreadedToOutput asserts the tool handler threads
// the backend's audit_warning into ReviveRunOutput (#1943): a 200 body carrying
// audit_warning surfaces the warning on the output, and a warning-free body
// yields the empty (omitted) field. This is the two-hop seam view; the
// end-to-end CallTool proof is TestReviveRun_AuditWarning_EndToEndCallTool.
func TestReviveRun_AuditWarning_ThreadedToOutput(t *testing.T) {
	fb, srv := newReviveFakeBackend(t)
	r := newResolver(srv, nil)

	t.Run("warning present threads through", func(t *testing.T) {
		runID := uuid.New()
		fb.reviveResp[runID] = ReviveRunResult{
			Run:          Run{ID: runID.String(), State: "running"},
			AuditWarning: "run_revived audit append failed: audit store down — the revive is committed but no chained provenance record was written; see server logs",
		}

		_, out, err := r.reviveRun(context.Background(), nil, ReviveRunInput{RunID: runID.String()})
		if err != nil {
			t.Fatalf("reviveRun: %v", err)
		}
		if !strings.Contains(out.AuditWarning, "run_revived") {
			t.Errorf("AuditWarning = %q, want it to name the run_revived append failure", out.AuditWarning)
		}
	})

	t.Run("no warning yields empty field", func(t *testing.T) {
		runID := uuid.New()
		fb.reviveResp[runID] = ReviveRunResult{Run: Run{ID: runID.String(), State: "running"}}

		_, out, err := r.reviveRun(context.Background(), nil, ReviveRunInput{RunID: runID.String()})
		if err != nil {
			t.Fatalf("reviveRun: %v", err)
		}
		if out.AuditWarning != "" {
			t.Errorf("AuditWarning = %q, want empty on a clean revive", out.AuditWarning)
		}
	})
}

// TestReviveRun_AuditWarning_EndToEndCallTool is the binding-condition test
// (terra's coverage concern, #1943): it drives the REAL fishhawk_revive_run tool
// handler over a real MCP CallTool (in-memory transport) against an httptest fake
// backend whose 200 body carries audit_warning, and asserts the warning arrives
// in the decoded tool output — the full server-body → client → tool-output path in
// ONE continuous test, not two composed hops (the gate_view_test.go pattern from
// #1960).
func TestReviveRun_AuditWarning_EndToEndCallTool(t *testing.T) {
	ctx := context.Background()
	runID := uuid.New()
	const warning = "run_revived audit append failed: audit store down — the revive is committed but no chained provenance record was written; see server logs"

	// A backend that returns a committed revive (200, run running) with the
	// audit_warning field set — the "revive succeeded but the provenance append
	// failed" shape.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/revive", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ReviveRunResult{
			Run:          Run{ID: r.PathValue("run_id"), State: "running"},
			AuditWarning: warning,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resolver := newResolver(srv, nil)
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0"}, nil)
	registerReviveRun(server, resolver)

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
		Name:      "fishhawk_revive_run",
		Arguments: map[string]any{"run_id": runID.String()},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
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
	// The warning survives the seam verbatim in the wire output.
	if !strings.Contains(string(raw), warning) {
		t.Fatalf("audit_warning did not survive the server-body → client → tool-output seam:\n%s", string(raw))
	}
	var out ReviveRunOutput
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("decode ReviveRunOutput from wire: %v", uerr)
	}
	if out.Run.State != "running" {
		t.Errorf("run state = %q, want running (revive committed)", out.Run.State)
	}
	if !strings.Contains(out.AuditWarning, "run_revived") {
		t.Errorf("decoded AuditWarning = %q, want it to name the run_revived append failure", out.AuditWarning)
	}
}
