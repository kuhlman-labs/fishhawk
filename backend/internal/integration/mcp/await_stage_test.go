package mcpe2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// stageReadProbe is a reverse-proxy RoundTripper that counts the GET reads of
// ONE stage's /v0/runs/{run}/stages/{stage} endpoint, records every ?wait value
// observed ON THE WIRE, and signals firstDone the moment the FIRST such read's
// response is in hand — i.e. after the backend's fast-path DB read provably
// completed. That server-side signal is what makes the cross-boundary test
// DETERMINISTIC (binding condition 1): the flip is gated on firstDone, so the
// fast path can never observe a settled stage and skip the poll path.
type stageReadProbe struct {
	base      http.RoundTripper
	stagePath string
	mu        sync.Mutex
	reads     int
	waits     []int
	firstDone chan struct{}
	fireOnce  sync.Once
}

func (p *stageReadProbe) RoundTrip(req *http.Request) (*http.Response, error) {
	stageRead := req.Method == http.MethodGet && req.URL.Path == p.stagePath
	if !stageRead {
		return p.base.RoundTrip(req)
	}
	p.mu.Lock()
	p.reads++
	firstRead := p.reads == 1
	waitVal := 0
	if raw := req.URL.Query().Get("wait"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			waitVal = n
		}
	}
	p.waits = append(p.waits, waitVal)
	p.mu.Unlock()

	resp, err := p.base.RoundTrip(req)
	if firstRead {
		// Fire only AFTER the fast-path read's response is in hand: the backend
		// has already read the (still-unsettled) stage and written the response,
		// so a subsequent flip cannot race the fast-path read.
		p.fireOnce.Do(func() { close(p.firstDone) })
	}
	return resp, err
}

func (p *stageReadProbe) snapshot() (reads int, waits []int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reads, append([]int(nil), p.waits...)
}

// newStageReadProxy stands up a reverse proxy in front of the fixture backend
// that instruments reads of the given stage endpoint. Requests flow MCP binary
// -> proxy -> fixture backend, transparently, so the run-bound token still
// authenticates.
func newStageReadProxy(t *testing.T, backendURL, stagePath string) (*httptest.Server, *stageReadProbe) {
	t.Helper()
	target, err := url.Parse(backendURL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	probe := &stageReadProbe{
		base:      http.DefaultTransport,
		stagePath: stagePath,
		firstDone: make(chan struct{}),
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = probe
	srv := httptest.NewServer(proxy)
	t.Cleanup(srv.Close)
	return srv, probe
}

// TestE2E_AwaitStage_ResolvesOnSettle drives the real chain the
// fishhawk_await_stage unit tests fake: tool handler -> apiClient.
// GetRunStageWait -> GET /v0/runs/{run_id}/stages/{stage_id}?wait served by
// handleGetRunStage -> Postgres run repo. The stage is flipped to succeeded
// only AFTER the fast-path read has completed (gated on the proxy's firstDone
// signal), so the POLL path — the ?wait query, parseRunStageWaitSeconds, and
// the top-level state/terminal envelope decode — is the seam that observes the
// settle. The unit layers on both sides pass with a ?wait encode/parse mismatch
// or a state/terminal envelope shadow-collision; only this test catches one
// (the #618 / #962 seam class).
func TestE2E_AwaitStage_ResolvesOnSettle(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// A stage left non-settled (running) so the fast path observes it unsettled.
	stage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:        fx.runID,
		Sequence:     1,
		Type:         runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent,
		ExecutorRef:  "fishhawk/runner@v1",
	})
	if err != nil {
		t.Fatalf("CreateStage: %v", err)
	}
	for _, to := range []runpkg.StageState{runpkg.StageStateDispatched, runpkg.StageStateRunning} {
		if _, err := fx.runRepo.TransitionStage(ctx, stage.ID, to, nil); err != nil {
			t.Fatalf("TransitionStage → %s: %v", to, err)
		}
	}

	stagePath := "/v0/runs/" + fx.runID.String() + "/stages/" + stage.ID.String()
	proxy, probe := newStageReadProxy(t, fx.url, stagePath)

	// The token is minted against the real backend; the binary talks to the
	// proxy (which forwards transparently), so the run-bound token still auths.
	token := fetchMCPToken(t, ctx, fx.url, fx.runID, fx.signingPriv)
	session := connectMCPClient(t, ctx, fx.mcpBinary, token, proxy.URL)

	// Flip the stage to succeeded only AFTER the fast-path read has landed.
	go func() {
		select {
		case <-probe.firstDone:
		case <-ctx.Done():
			return
		}
		if _, ferr := fx.runRepo.TransitionStage(context.Background(), stage.ID, runpkg.StageStateSucceeded, nil); ferr != nil {
			t.Errorf("flip stage → succeeded: %v", ferr)
		}
	}()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_await_stage",
		Arguments: map[string]any{
			"run_id":          fx.runID.String(),
			"stage":           "implement",
			"timeout_seconds": 60,
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %+v", result.Content)
	}

	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out struct {
		Status   string `json:"status"`
		State    string `json:"state"`
		Terminal bool   `json:"terminal"`
		StageID  string `json:"stage_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode await_stage output: %v\nraw: %s", err, raw)
	}
	if out.Status != "settled" {
		t.Fatalf("status = %q, want settled\nraw: %s", out.Status, raw)
	}
	if out.State != "succeeded" || !out.Terminal {
		t.Errorf("state/terminal = %q/%v, want succeeded/true\nraw: %s", out.State, out.Terminal, raw)
	}
	if out.StageID != stage.ID.String() {
		t.Errorf("stage_id = %q, want %s", out.StageID, stage.ID)
	}

	// Binding condition 1(b): the POLL path was actually taken — more than one
	// stage read reached the backend AND a request carrying wait>0 was observed.
	// Without this a future regression that dropped the long-poll would still
	// pass on the fast path alone.
	reads, waits := probe.snapshot()
	if reads < 2 {
		t.Errorf("stage reads reaching the backend = %d, want > 1 (the poll path, not just the fast path)", reads)
	}
	sawWait := false
	for _, w := range waits {
		if w > 0 {
			sawWait = true
		}
		if w > 15 {
			t.Errorf("a stage read carried ?wait=%d on the wire, want <= 15 (strictly under the 30s client timeout)", w)
		}
	}
	if !sawWait {
		t.Errorf("no stage read carried ?wait>0 — the long-poll path was never exercised; waits=%v", waits)
	}
}

// TestE2E_AwaitStage_ParkedStageIsSettled drives a PARKED (awaiting_approval)
// stage through the REAL handler and asserts it resolves as settled with its
// RAW state carried out — the parked half of run.StageState.IsSettled that a
// terminal-only client-side check would silently get wrong. The fast-path read
// resolves it, so no flip is needed.
func TestE2E_AwaitStage_ParkedStageIsSettled(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            fx.runID,
		Sequence:         1,
		Type:             runpkg.StageTypePlan,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage: %v", err)
	}
	// dispatched → running → awaiting_approval (a parked-but-settled state).
	parkAtGate(t, ctx, fx.runRepo, stage.ID)

	token := fetchMCPToken(t, ctx, fx.url, fx.runID, fx.signingPriv)
	session := connectMCPClient(t, ctx, fx.mcpBinary, token, fx.url)

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_await_stage",
		Arguments: map[string]any{
			"run_id":          fx.runID.String(),
			"stage":           "plan",
			"timeout_seconds": 30,
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %+v", result.Content)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out struct {
		Status   string `json:"status"`
		State    string `json:"state"`
		Terminal bool   `json:"terminal"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode await_stage output: %v\nraw: %s", err, raw)
	}
	if out.Status != "settled" {
		t.Fatalf("status = %q, want settled (a parked awaiting_approval IS settled)\nraw: %s", out.Status, raw)
	}
	if out.State != "awaiting_approval" {
		t.Errorf("state = %q, want awaiting_approval — the raw parked state must not be coerced\nraw: %s", out.State, raw)
	}
	if !out.Terminal {
		t.Errorf("terminal = false, want true (IsSettled covers the parked state)\nraw: %s", raw)
	}
}
