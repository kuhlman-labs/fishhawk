package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// seedStageWait seeds a stage of stageType for runID in the fake (so
// resolveStage finds it) plus its GET /v0/runs/{run}/stages/{stage} wait
// envelope with the given state + settledness flag. Returns the stage id the
// tests key their flip callbacks and read-count assertions on.
func seedStageWait(fb *fakeBackend, runID uuid.UUID, stageType, state string, terminal bool) uuid.UUID {
	stageID := uuid.New()
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.stagesByRun[runID] = append(fb.stagesByRun[runID], Stage{
		ID:    stageID.String(),
		RunID: runID.String(),
		Type:  stageType,
		State: state,
	})
	fb.stageWaitByStageID[stageID] = RunStageWait{
		ID:       stageID.String(),
		RunID:    runID.String(),
		Type:     stageType,
		State:    state,
		Terminal: terminal,
	}
	return stageID
}

// settleStageWaitAt returns a stageWaitFlip callback that flips the seeded
// envelope to (state, terminal:true) the FIRST time the given stage id is read
// at read count == at. Runs under fb.mu (the handler holds it), so it mutates
// stageWaitByStageID directly.
func settleStageWaitAt(fb *fakeBackend, target uuid.UUID, at int, state string) func(uuid.UUID, int) {
	return func(sid uuid.UUID, reads int) {
		if sid == target && reads == at {
			env := fb.stageWaitByStageID[sid]
			env.State = state
			env.Terminal = true
			fb.stageWaitByStageID[sid] = env
		}
	}
}

func stageReads(fb *fakeBackend, stageID uuid.UUID) int {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.stageWaitReadsByStageID[stageID]
}

// --- happy paths ---

func TestAwaitStage_SettledOnFastPath(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "succeeded", true)
	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	_, out, err := r.awaitStage(context.Background(), nil, AwaitStageInput{
		RunID: runID.String(),
		Stage: "implement",
	})
	if err != nil {
		t.Fatalf("awaitStage: %v", err)
	}
	if out.Status != "settled" {
		t.Fatalf("Status = %q, want settled", out.Status)
	}
	if out.State != "succeeded" || !out.Terminal {
		t.Errorf("State/Terminal = %q/%v, want succeeded/true", out.State, out.Terminal)
	}
	if out.StageID != stageID.String() {
		t.Errorf("StageID = %q, want %s", out.StageID, stageID)
	}
	if got := stageReads(fb, stageID); got != 1 {
		t.Errorf("stage reads = %d, want 1 (fast path only, no poll)", got)
	}
}

func TestAwaitStage_SettlesMidPoll(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "running", false)
	fb.stageWaitFlip = settleStageWaitAt(fb, stageID, 2, "succeeded")
	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	_, out, err := r.awaitStage(context.Background(), nil, AwaitStageInput{
		RunID:          runID.String(),
		Stage:          "implement",
		TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("awaitStage: %v", err)
	}
	if out.Status != "settled" {
		t.Fatalf("Status = %q, want settled after poll-land", out.Status)
	}
	if out.State != "succeeded" {
		t.Errorf("State = %q, want succeeded", out.State)
	}
	if got := stageReads(fb, stageID); got <= 1 {
		t.Errorf("stage reads = %d, want > 1 (the poll path was taken)", got)
	}
}

// TestAwaitStage_ParkedStageIsSettled is the settledness proof (and the
// counterfactual for keying on terminality instead of settledness): a stage
// parked at awaiting_approval — NOT a terminal state — resolves as settled
// because the endpoint's terminal flag is IsSettled, and its RAW state is
// carried out verbatim (never coerced to succeeded). If the verb keyed on
// stageStateIsTerminal(state) instead of the envelope's terminal flag, this
// stage would never resolve on the fast path and the wait would time out.
func TestAwaitStage_ParkedStageIsSettled(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedStageWait(fb, runID, "implement", "awaiting_approval", true)
	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	_, out, err := r.awaitStage(context.Background(), nil, AwaitStageInput{
		RunID: runID.String(),
		Stage: "implement",
	})
	if err != nil {
		t.Fatalf("awaitStage: %v", err)
	}
	if out.Status != "settled" {
		t.Fatalf("Status = %q, want settled (a parked awaiting_approval IS settled)", out.Status)
	}
	if out.State != "awaiting_approval" {
		t.Errorf("State = %q, want awaiting_approval — the raw state must NOT be coerced to succeeded", out.State)
	}
	if !out.Terminal {
		t.Errorf("Terminal = false, want true (IsSettled covers the parked state)")
	}
}

func TestAwaitStage_DefaultsToImplement(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedStageWait(fb, runID, "implement", "succeeded", true)
	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	// No Stage given → defaults to implement and resolves the implement stage.
	_, out, err := r.awaitStage(context.Background(), nil, AwaitStageInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("awaitStage: %v", err)
	}
	if out.Stage != "implement" || out.Status != "settled" {
		t.Errorf("Stage/Status = %q/%q, want implement/settled on the default", out.Stage, out.Status)
	}
}

// --- timeout ---

func TestAwaitStage_TimeoutIsResumable(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "running", false)

	// Nothing ever settles; drive the deadline deterministically by cancelling
	// the parent context from the read hook once the poll loop has begun (fast
	// path = read 1, first tick = read 2).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fb.stageWaitFlip = func(sid uuid.UUID, reads int) {
		if sid == stageID && reads == 2 {
			cancel()
		}
	}
	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	_, out, err := r.awaitStage(ctx, nil, AwaitStageInput{
		RunID:          runID.String(),
		Stage:          "implement",
		TimeoutSeconds: 600,
	})
	if err != nil {
		t.Fatalf("awaitStage: %v", err)
	}
	if out.Status != "timeout" {
		t.Fatalf("Status = %q, want timeout", out.Status)
	}
	if out.PollIntervalSeconds != suggestedStageWaitPollIntervalSeconds {
		t.Errorf("PollIntervalSeconds = %d, want %d", out.PollIntervalSeconds, suggestedStageWaitPollIntervalSeconds)
	}
	if !strings.Contains(out.Message, "re-call fishhawk_await_stage") || !strings.Contains(out.Message, "no-op") {
		t.Errorf("timeout message should frame the resumable no-op re-call: %q", out.Message)
	}
}

// TestAwaitStage_TimeoutClampTable pins the timeout cap AND the effective
// (clamped) timeout the wait uses, across the four #2490 regimes. The clamped
// timeout is observed through the timeout message ("did not settle within
// <clamped>s"), so deleting the clamp (raw passthrough) goes red on the
// over-cap-without-long_wait row (600 -> 99999).
func TestAwaitStage_TimeoutClampTable(t *testing.T) {
	cases := []struct {
		name        string
		timeoutSec  int
		longWait    bool
		token       bool
		wantCap     int
		wantClamped int
	}{
		{"default", 0, false, false, 600, 360},
		{"over-cap no long_wait", 99999, false, false, 600, 600},
		{"long_wait raises cap", 7000, true, false, 7200, 7000},
		{"progressToken raises cap", 99999, false, true, 7200, 7200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			runID := uuid.New()
			stageID := seedStageWait(fb, runID, "implement", "running", false)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fb.stageWaitFlip = func(sid uuid.UUID, reads int) {
				if sid == stageID && reads == 2 {
					cancel()
				}
			}
			r := newResolver(srv, nil)
			r.reviewPollInterval = 100 * time.Microsecond

			var req *mcp.CallToolRequest
			if tc.token {
				req = &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "fishhawk_await_stage"}}
				req.Params.SetProgressToken("clamp-tok")
			}

			_, out, err := r.awaitStage(ctx, req, AwaitStageInput{
				RunID:          runID.String(),
				Stage:          "implement",
				TimeoutSeconds: tc.timeoutSec,
				LongWait:       tc.longWait,
			})
			if err != nil {
				t.Fatalf("awaitStage: %v", err)
			}
			if out.Status != "timeout" {
				t.Fatalf("Status = %q, want timeout", out.Status)
			}
			if out.TimeoutCapSeconds != tc.wantCap {
				t.Errorf("TimeoutCapSeconds = %d, want %d", out.TimeoutCapSeconds, tc.wantCap)
			}
			if want := fmt.Sprintf("within %ds", tc.wantClamped); !strings.Contains(out.Message, want) {
				t.Errorf("timeout message should report the CLAMPED timeout %q; got %q", want, out.Message)
			}
			if out.Heartbeat != tc.token {
				t.Errorf("Heartbeat = %v, want %v (true only when a progressToken was supplied)", out.Heartbeat, tc.token)
			}
		})
	}
}

// --- run-terminal backstop ---

func TestAwaitStage_RunTerminalBackstop(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedStageWait(fb, runID, "implement", "running", false)
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "failed"}
	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	// Large timeout: if the backstop did NOT fire the test would hang on the
	// deadline, so a prompt return is the proof.
	_, out, err := r.awaitStage(context.Background(), nil, AwaitStageInput{
		RunID:          runID.String(),
		Stage:          "implement",
		TimeoutSeconds: 600,
	})
	if err != nil {
		t.Fatalf("awaitStage: %v", err)
	}
	if out.Status != "run_terminal" {
		t.Fatalf("Status = %q, want run_terminal", out.Status)
	}
	if !strings.Contains(out.Message, "Do not re-arm blindly") {
		t.Errorf("run_terminal message should warn against blind re-arm: %q", out.Message)
	}
}

// TestAwaitStage_RunTerminalBackstop_FinalReadWins pins the backstop's
// final-read ordering: a stage that SETTLES on the backstop's final read (at/
// after the run's terminal transition) resolves as settled, not run_terminal.
// The run is terminal from the start; the stage settles on the SECOND stage
// read (fast path = read 1 found it unsettled; the pre-loop backstop's final
// read = read 2).
func TestAwaitStage_RunTerminalBackstop_FinalReadWins(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "running", false)
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "succeeded"}
	fb.stageWaitFlip = settleStageWaitAt(fb, stageID, 2, "succeeded")
	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	_, out, err := r.awaitStage(context.Background(), nil, AwaitStageInput{
		RunID:          runID.String(),
		Stage:          "implement",
		TimeoutSeconds: 600,
	})
	if err != nil {
		t.Fatalf("awaitStage: %v", err)
	}
	if out.Status != "settled" {
		t.Fatalf("Status = %q, want settled (the backstop's final read must win)", out.Status)
	}
	if out.State != "succeeded" {
		t.Errorf("State = %q, want succeeded from the final read", out.State)
	}
}

// TestAwaitStage_BackstopReadFailureKeepsPolling proves the backstop's GetRun
// is best-effort: when GetRun fails (500), the poll/timeout path stays in
// charge and a mid-poll settlement still resolves. Without the best-effort
// swallow a GetRun error would fail the wait.
func TestAwaitStage_BackstopReadFailureKeepsPolling(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "running", false)
	// Every GetRun for this run returns 500, so the backstop can never resolve.
	fb.getStatusByID[runID] = 500
	fb.stageWaitFlip = settleStageWaitAt(fb, stageID, 3, "succeeded")
	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	_, out, err := r.awaitStage(context.Background(), nil, AwaitStageInput{
		RunID:          runID.String(),
		Stage:          "implement",
		TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("awaitStage returned error despite a best-effort backstop read failure: %v", err)
	}
	if out.Status != "settled" {
		t.Fatalf("Status = %q, want settled (the poll path stayed in charge)", out.Status)
	}
}

// --- per-failure-mode / error branches ---

func TestAwaitStage_InvalidRunID(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	// "not-a-uuid" is non-parseable BY CONSTRUCTION, so the red lands on the
	// behavioral assertion, not fixture setup.
	_, _, err := r.awaitStage(context.Background(), nil, AwaitStageInput{RunID: "not-a-uuid", Stage: "implement"})
	if err == nil {
		t.Fatal("expected an error on a non-UUID run_id")
	}
	if !strings.Contains(err.Error(), "run_id") {
		t.Errorf("error should name the run_id field; got %q", err.Error())
	}
}

func TestAwaitStage_UnknownStageType(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	// Only a plan stage exists; awaiting implement finds zero matches.
	seedStageWait(fb, runID, "plan", "succeeded", true)
	r := newResolver(srv, nil)

	_, _, err := r.awaitStage(context.Background(), nil, AwaitStageInput{RunID: runID.String(), Stage: "implement"})
	if err == nil {
		t.Fatal("expected a zero-match resolve error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should be resolveStage's zero-match text; got %q", err.Error())
	}
}

func TestAwaitStage_AmbiguousStage(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedStageWait(fb, runID, "implement", "running", false)
	seedStageWait(fb, runID, "implement", "running", false)
	r := newResolver(srv, nil)

	_, _, err := r.awaitStage(context.Background(), nil, AwaitStageInput{RunID: runID.String(), Stage: "implement"})
	if err == nil {
		t.Fatal("expected an ambiguous-stage resolve error")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error should be resolveStage's ambiguous text; got %q", err.Error())
	}
}

func TestAwaitStage_ExplicitStageIDMismatch(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedStageWait(fb, runID, "implement", "running", false)
	r := newResolver(srv, nil)

	// A freshly generated unrelated UUID — definitionally not one of the run's
	// stages — so the resolveStage explicit-disagrees branch is what fires, not
	// a fixture built by calling the resolver.
	bogus := uuid.New().String()
	_, _, err := r.awaitStage(context.Background(), nil, AwaitStageInput{
		RunID:   runID.String(),
		Stage:   "implement",
		StageID: bogus,
	})
	if err == nil {
		t.Fatal("expected an explicit-stage-id-disagrees resolve error")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Errorf("error should be resolveStage's explicit-disagree text; got %q", err.Error())
	}
}

func TestAwaitStage_TransportErrorSurfaces(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "running", false)
	// The stage-wait read itself fails — a transport error must surface as a
	// tool error, never a fabricated status.
	fb.stageWaitStatusByStageID[stageID] = 500
	r := newResolver(srv, nil)

	_, _, err := r.awaitStage(context.Background(), nil, AwaitStageInput{RunID: runID.String(), Stage: "implement"})
	if err == nil {
		t.Fatal("expected a transport error to surface")
	}
	if !strings.Contains(err.Error(), "read stage wait") {
		t.Errorf("error should name the failed stage-wait read; got %q", err.Error())
	}
}

// TestAwaitStage_PerCallWaitStaysUnderClientTimeout asserts the ?wait value
// observed ON THE WIRE never exceeds awaitStagePerCallWaitSeconds (15) — i.e.
// strictly under the apiClient short-client 30s timeout — AND that at least one
// poll read carried wait>0 (the poll path was taken). Deleting the clamp / the
// 15s constant so the wire value goes to 30 fails the <=15 assertion.
func TestAwaitStage_PerCallWaitStaysUnderClientTimeout(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "running", false)
	fb.stageWaitFlip = settleStageWaitAt(fb, stageID, 2, "succeeded")
	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	_, out, err := r.awaitStage(context.Background(), nil, AwaitStageInput{
		RunID:          runID.String(),
		Stage:          "implement",
		TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("awaitStage: %v", err)
	}
	if out.Status != "settled" {
		t.Fatalf("Status = %q, want settled", out.Status)
	}
	fb.mu.Lock()
	waits := append([]int(nil), fb.stageWaitWaitsByStageID[stageID]...)
	fb.mu.Unlock()
	if len(waits) < 2 {
		t.Fatalf("observed %d stage reads, want >= 2 (fast path + at least one poll)", len(waits))
	}
	sawPoll := false
	for i, w := range waits {
		if w > awaitStagePerCallWaitSeconds {
			t.Errorf("read[%d] ?wait = %d on the wire, want <= %d (strictly under the 30s client timeout)", i, w, awaitStagePerCallWaitSeconds)
		}
		if w > 0 {
			sawPoll = true
		}
	}
	if !sawPoll {
		t.Error("no read carried ?wait>0 — the long-poll path was never exercised")
	}
}

// --- heartbeat (opt-in) ---

// TestAwaitStage_NoHeartbeatWithoutToken is the MCP opt-in proof: a real
// CallTool with NO progressToken receives ZERO notifications and reports
// heartbeat=false, even though the wait polls (the stage settles mid-poll so
// ticks that COULD emit occur). Deleting the progressToken nil-guard so a
// heartbeat is emitted without a token fails the notes==0 assertion.
func TestAwaitStage_NoHeartbeatWithoutToken(t *testing.T) {
	ctx := context.Background()
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "running", false)
	fb.stageWaitFlip = settleStageWaitAt(fb, stageID, 4, "succeeded")
	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0"}, nil)
	registerAwaitStage(server, r)

	var mu sync.Mutex
	var notes int
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, _ *mcp.ProgressNotificationClientRequest) {
			mu.Lock()
			notes++
			mu.Unlock()
		},
	})
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
		Name:      "fishhawk_await_stage",
		Arguments: map[string]any{"run_id": runID.String(), "stage": "implement", "timeout_seconds": 5},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}
	raw, merr := json.Marshal(res.StructuredContent)
	if merr != nil {
		t.Fatalf("marshal StructuredContent: %v", merr)
	}
	if !strings.Contains(string(raw), `"status":"settled"`) {
		t.Errorf("no-token result should still resolve settled; got %s", raw)
	}
	if !strings.Contains(string(raw), `"heartbeat":false`) {
		t.Errorf("no-token result should report heartbeat=false; got %s", raw)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if notes != 0 {
		t.Errorf("received %d progress notifications with no progressToken; want 0 (opt-in)", notes)
	}
}

// TestAwaitStage_HeartbeatFailureDoesNotFailWait proves a heartbeat whose
// NotifyProgress FAILS (a closed session) is swallowed — the wait still
// resolves settled.
func TestAwaitStage_HeartbeatFailureDoesNotFailWait(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "running", false)
	fb.stageWaitFlip = settleStageWaitAt(fb, stageID, 3, "succeeded")
	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	sess := errNotifySession(t)
	req := &mcp.CallToolRequest{
		Session: sess,
		Params:  &mcp.CallToolParamsRaw{Name: "fishhawk_await_stage"},
	}
	req.Params.SetProgressToken("err-tok")

	_, out, err := r.awaitStage(context.Background(), req, AwaitStageInput{
		RunID:          runID.String(),
		Stage:          "implement",
		TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("awaitStage returned error despite a swallowed notify failure: %v", err)
	}
	if out.Status != "settled" {
		t.Fatalf("Status = %q, want settled — the wait must resolve despite notify errors", out.Status)
	}
}

// TestAwaitStage_SuppliedTokenRaisesCapAndEmits is the mode-1 heartbeat proof at
// the real MCP boundary: a call carrying a progressToken reports heartbeat=true
// + the raised 7200 cap and receives at least one keep-alive.
func TestAwaitStage_SuppliedTokenRaisesCapAndEmits(t *testing.T) {
	ctx := context.Background()
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "running", false)
	var reads atomic.Int64
	fb.stageWaitFlip = func(sid uuid.UUID, _ int) {
		// Settle only after a few reads so heartbeats are emitted first.
		if sid == stageID && reads.Add(1) == 4 {
			env := fb.stageWaitByStageID[sid]
			env.State = "succeeded"
			env.Terminal = true
			fb.stageWaitByStageID[sid] = env
		}
	}
	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0"}, nil)
	registerAwaitStage(server, r)

	var mu sync.Mutex
	var notes []*mcp.ProgressNotificationParams
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			mu.Lock()
			notes = append(notes, req.Params)
			mu.Unlock()
		},
	})
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

	params := &mcp.CallToolParams{
		Name:      "fishhawk_await_stage",
		Arguments: map[string]any{"run_id": runID.String(), "stage": "implement", "timeout_seconds": 5},
	}
	params.SetProgressToken("stage-tok-1")
	res, err := clientSession.CallTool(ctx, params)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}
	raw, merr := json.Marshal(res.StructuredContent)
	if merr != nil {
		t.Fatalf("marshal StructuredContent: %v", merr)
	}
	if !strings.Contains(string(raw), `"heartbeat":true`) {
		t.Errorf("supplied-token result should report heartbeat=true; got %s", raw)
	}
	if !strings.Contains(string(raw), `"timeout_cap_seconds":7200`) {
		t.Errorf("supplied-token result should report timeout_cap_seconds=7200; got %s", raw)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(notes) == 0 {
		t.Fatal("expected at least one keep-alive heartbeat with a supplied progressToken")
	}
	for i, n := range notes {
		if n.ProgressToken != "stage-tok-1" {
			t.Errorf("notification[%d] progressToken = %v, want stage-tok-1", i, n.ProgressToken)
		}
		if !strings.HasPrefix(n.Message, `await_stage: waiting for stage "implement"`) {
			t.Errorf("notification[%d] message = %q, want the awaitStageProgressMessage text", i, n.Message)
		}
	}
}

// TestAwaitStageProgressMessage pins the pure heartbeat-message helper.
func TestAwaitStageProgressMessage(t *testing.T) {
	got := awaitStageProgressMessage("implement", 12*time.Second)
	if got != `await_stage: waiting for stage "implement" to settle; elapsed 12s` {
		t.Errorf("awaitStageProgressMessage = %q", got)
	}
}
