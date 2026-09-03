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

// TestAwaitStage_PollTransportErrorSurfaces covers the mid-poll transport-error
// branch: GetRunStageWait failing INSIDE the ticker loop while the poll deadline
// is still live must surface as a tool error ("poll stage wait"), NOT a
// fabricated status and NOT the fast-path "read stage wait" error. The fast-path
// read (read 1) succeeds unsettled; the flip arms a 500 on read 2 (the first
// poll read) so the failure lands mid-loop with pollCtx.Err()==nil (a large
// TimeoutSeconds keeps the deadline live), exercising the branch that
// TestAwaitStage_TransportErrorSurfaces (fast path) and
// TestAwaitStage_TimeoutIsResumable (deadline-hit disambiguation) do not.
func TestAwaitStage_PollTransportErrorSurfaces(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "running", false)
	// Arm the 500 on the SECOND read (the first poll read), not the first: the
	// flip runs under fb.mu before the handler reads the status, so setting the
	// override here makes read 2 fail while read 1 (fast path) already succeeded.
	fb.stageWaitFlip = func(sid uuid.UUID, reads int) {
		if sid == stageID && reads == 2 {
			fb.stageWaitStatusByStageID[sid] = 500
		}
	}
	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	// Large timeout so the deadline stays live when the mid-poll read fails —
	// this routes through the transport-error return, not the pollCtx.Err()
	// resumable-timeout branch.
	_, _, err := r.awaitStage(context.Background(), nil, AwaitStageInput{
		RunID:          runID.String(),
		Stage:          "implement",
		TimeoutSeconds: 600,
	})
	if err == nil {
		t.Fatal("expected a mid-poll transport error to surface")
	}
	if !strings.Contains(err.Error(), "poll stage wait") {
		t.Errorf("error should name the failed poll read (poll stage wait), not a fabricated status; got %q", err.Error())
	}
	if strings.Contains(err.Error(), "read stage wait") {
		t.Errorf("error should be the mid-poll error, not the fast-path read error; got %q", err.Error())
	}
}

// TestAwaitStage_PerCallWaitStaysUnderClientTimeout asserts the ?wait value
// observed ON THE WIRE never exceeds the INDEPENDENT LITERAL bound 15 — NOT
// awaitStagePerCallWaitSeconds, which would move the bound WITH the constant and
// make the comparison unfalsifiable (raise the constant to 30 and `w > constant`
// can never fire). The literal 15 is strictly under the apiClient short-client's
// 30s timeout, so a held long-poll can never race that deadline; raising the
// constant to 30 — the exact regression this guards — drives the wire value to
// 30 and fails `w > 15`. The test also asserts at least one poll read carried
// wait>0 (the long-poll path was taken), and separately pins the constant itself
// at 15. (The integration test at backend/internal/integration/mcp is the
// cross-package control that CANNOT reference the constant; this unit test now
// stands on the same independent literal so it holds on a host without Docker.)
func TestAwaitStage_PerCallWaitStaysUnderClientTimeout(t *testing.T) {
	// Separately pin the constant so a change to it is loud here too — this is a
	// named pin, distinct from the wire-value bound below which does NOT depend
	// on it.
	if awaitStagePerCallWaitSeconds != 15 {
		t.Errorf("awaitStagePerCallWaitSeconds = %d, want 15 (must stay strictly under the apiClient's 30s client timeout)", awaitStagePerCallWaitSeconds)
	}

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
		// Bound against the LITERAL 15, not awaitStagePerCallWaitSeconds: an
		// independent literal is the only form that goes red when the constant is
		// raised to 30 (which would let a held long-poll race the apiClient's 30s
		// client timeout and surface as a transport error).
		if w > 15 {
			t.Errorf("read[%d] ?wait = %d on the wire, want <= 15 (strictly under the apiClient's 30s client timeout)", i, w)
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

// --- second release condition: a pending mid-stage scope amendment (#2588) ---

// seedAmendment appends one scope-amendment row for (runID, stageID) with the
// given status to the fake's list fixture and returns its id.
func seedAmendment(fb *fakeBackend, runID, stageID uuid.UUID, status string, paths ...string) uuid.UUID {
	amendmentID := uuid.New()
	p := make([]ScopeAmendmentPath, 0, len(paths))
	for _, path := range paths {
		p = append(p, ScopeAmendmentPath{Path: path, Operation: "modify"})
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.amendmentsByRun[runID] = append(fb.amendmentsByRun[runID], ScopeAmendmentItem{
		ID:      amendmentID.String(),
		RunID:   runID.String(),
		StageID: stageID.String(),
		Paths:   p,
		Reason:  "the coupled registration table must change too",
		Status:  status,
	})
	return amendmentID
}

func amendmentReads(fb *fakeBackend, runID uuid.UUID) int {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.amendmentsReadsByRun[runID]
}

// TestAwaitStage_ReleasesOnPendingAmendmentMidPoll is the #2588 done-means: an
// implement stage that stays `running` (filing an amendment does NOT park it)
// files a mid-stage amendment, and the armed wait releases with
// amendment_pending — carrying the row and a pre-filled decide next_step — well
// inside the timeout, instead of holding until the agent's ~15-minute window has
// elapsed and the request has expired undecided. This is the assertion the
// pre-#2588 code fails.
func TestAwaitStage_ReleasesOnPendingAmendmentMidPoll(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "running", false)
	// The stage NEVER settles. The amendment appears at the second list read
	// (read 1 is the fast-path probe, read 2 the first poll tick), so the
	// release can only come from the poll-loop probe.
	var amendmentID uuid.UUID
	fb.amendmentsFlip = func(rid uuid.UUID, reads int) {
		if rid == runID && reads == 2 {
			amendmentID = uuid.New()
			fb.amendmentsByRun[rid] = append(fb.amendmentsByRun[rid], ScopeAmendmentItem{
				ID:      amendmentID.String(),
				RunID:   rid.String(),
				StageID: stageID.String(),
				Paths: []ScopeAmendmentPath{
					{Path: "backend/internal/mcpserver/tools_test.go", Operation: "modify"},
					{Path: "docs/new.md", Operation: "create"},
				},
				Reason: "the coupled registration table must change too",
				Status: "pending",
			})
		}
	}
	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	// A large timeout: if the amendment did not release the wait this would run
	// to the deadline, so a prompt return IS the proof.
	started := time.Now()
	_, out, err := r.awaitStage(context.Background(), nil, AwaitStageInput{
		RunID:          runID.String(),
		Stage:          "implement",
		TimeoutSeconds: 600,
	})
	if err != nil {
		t.Fatalf("awaitStage: %v", err)
	}
	if out.Status != "amendment_pending" {
		t.Fatalf("Status = %q, want amendment_pending", out.Status)
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Errorf("wait took %s; it must release well before the agent's ~15-minute amendment window", elapsed)
	}
	if out.Terminal {
		t.Error("Terminal = true, want false — the stage has NOT settled, only the wait released")
	}
	if out.State != "running" {
		t.Errorf("State = %q, want the raw running state", out.State)
	}
	if out.PendingAmendment == nil {
		t.Fatal("PendingAmendment = nil; the amendment row must ride out so no second lookup is needed")
	}
	if out.PendingAmendment.ID != amendmentID.String() {
		t.Errorf("PendingAmendment.ID = %q, want %s", out.PendingAmendment.ID, amendmentID)
	}
	if len(out.PendingAmendment.Paths) != 2 || out.PendingAmendment.Paths[1].Path != "docs/new.md" {
		t.Errorf("PendingAmendment.Paths = %+v, want the two requested paths", out.PendingAmendment.Paths)
	}
	if out.PendingAmendment.Reason != "the coupled registration table must change too" {
		t.Errorf("PendingAmendment.Reason = %q, want the agent's reason echoed", out.PendingAmendment.Reason)
	}
	if out.NextStep == nil {
		t.Fatal("NextStep = nil; the decide call must be pre-filled so the operator acts in one hop")
	}
	if out.NextStep.Action != "fishhawk_decide_scope_amendment" {
		t.Errorf("NextStep.Action = %q, want fishhawk_decide_scope_amendment", out.NextStep.Action)
	}
	if got := out.NextStep.Params["run_id"]; got != runID.String() {
		t.Errorf("NextStep.Params[run_id] = %q, want %s", got, runID)
	}
	if got := out.NextStep.Params["amendment_id"]; got != amendmentID.String() {
		t.Errorf("NextStep.Params[amendment_id] = %q, want %s", got, amendmentID)
	}
	// Post-#2601 contract: the message frames a lapsed window as an EXPIRY, not a
	// denial. Require the amendment id AND an expiry marker, and assert the
	// retired "as if denied" wording is ABSENT.
	if !strings.Contains(strings.ToLower(out.Message), "expire") || !strings.Contains(out.Message, amendmentID.String()) {
		t.Errorf("message should name the amendment id and the expiry framing; got %q", out.Message)
	}
	if strings.Contains(strings.ToLower(out.Message), "as if denied") {
		t.Errorf("message must not carry the retired proceed-as-denied wording; got %q", out.Message)
	}
}

// TestAwaitStage_ReleasesOnAmendmentAlreadyPendingAtArmTime covers the FAST
// PATH probe: an operator whose wait timed out re-arms it against an amendment
// that is ALREADY pending, and it must release immediately rather than after a
// poll interval. Proven by the ?wait values on the wire: every stage read
// carries wait=0 (the arm read + the settledness re-check), so no poll tick
// (which issues ?wait=15) was ever taken.
func TestAwaitStage_ReleasesOnAmendmentAlreadyPendingAtArmTime(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "running", false)
	amendmentID := seedAmendment(fb, runID, stageID, "pending", "backend/internal/mcpserver/onboarding.go")
	r := newResolver(srv, nil)
	// A poll interval long enough that a tick would never fire inside the test:
	// releasing at all is proof the FAST-PATH probe fired.
	r.reviewPollInterval = 30 * time.Second

	_, out, err := r.awaitStage(context.Background(), nil, AwaitStageInput{
		RunID:          runID.String(),
		Stage:          "implement",
		TimeoutSeconds: 600,
	})
	if err != nil {
		t.Fatalf("awaitStage: %v", err)
	}
	if out.Status != "amendment_pending" {
		t.Fatalf("Status = %q, want amendment_pending on the fast path", out.Status)
	}
	if out.PendingAmendment == nil || out.PendingAmendment.ID != amendmentID.String() {
		t.Fatalf("PendingAmendment = %+v, want the seeded amendment %s", out.PendingAmendment, amendmentID)
	}
	fb.mu.Lock()
	waits := append([]int(nil), fb.stageWaitWaitsByStageID[stageID]...)
	fb.mu.Unlock()
	// Two stage reads: the arm read, then the settledness re-check the
	// amendment release performs before resolving. Neither is a poll tick.
	if len(waits) != 2 {
		t.Errorf("stage reads = %d (%v), want 2 (arm read + settledness re-check)", len(waits), waits)
	}
	for i, w := range waits {
		if w > 0 {
			t.Errorf("read[%d] ?wait = %d; a fast-path release must take no poll tick (which issues ?wait=15)", i, w)
		}
	}
	if got := amendmentReads(fb, runID); got != 1 {
		t.Errorf("amendment list reads = %d, want 1 (one probe, not a loop)", got)
	}
}

// TestAwaitStage_SettledStageWinsOverPendingAmendment pins the ORDERING: the
// settled check runs before the amendment probe, so a stage that has settled
// with an amendment still pending resolves `settled`, never amendment_pending.
//
// The status assertion alone does NOT discriminate the ordering — the release's
// own settledness re-check (see the interleaving test below) would recover
// `settled` even with the probe moved first. The ordering is pinned on the
// observable it uniquely controls: a settled stage costs ZERO amendment-list
// reads, because the wait short-circuits before the probe. Moving the probe
// above the sw.Terminal check drives that count to 1.
func TestAwaitStage_SettledStageWinsOverPendingAmendment(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "succeeded", true)
	seedAmendment(fb, runID, stageID, "pending", "docs/whatever.md")
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
		t.Fatalf("Status = %q, want settled — settledness wins over a pending amendment", out.Status)
	}
	if out.PendingAmendment != nil {
		t.Errorf("PendingAmendment = %+v, want nil on the settled resolve", out.PendingAmendment)
	}
	if got := amendmentReads(fb, runID); got != 0 {
		t.Errorf("amendment list reads = %d, want 0 — a settled stage must short-circuit BEFORE the probe", got)
	}
}

// TestAwaitStage_SettledDuringAmendmentProbeWinsTheRace drives the
// INTERLEAVING the pre-seeded settled-wins test above cannot discriminate: the
// stage reads NON-TERMINAL when the wait checks it, and SETTLES while the
// amendment-list read is in flight. Ordering alone does not deliver
// "settledness always wins" here — the release must re-check settledness after
// the probe finds a pending amendment and prefer settled. Deleting that
// re-check makes this return amendment_pending for a stage that has in fact
// settled.
func TestAwaitStage_SettledDuringAmendmentProbeWinsTheRace(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "running", false)
	seedAmendment(fb, runID, stageID, "pending", "docs/whatever.md")
	// The stage settles DURING the first amendment-list read: the flip runs
	// under fb.mu inside that handler, after the wait's own stage read already
	// reported running. The list still returns the pending amendment.
	fb.amendmentsFlip = func(rid uuid.UUID, reads int) {
		if rid == runID && reads == 1 {
			env := fb.stageWaitByStageID[stageID]
			env.State = "succeeded"
			env.Terminal = true
			fb.stageWaitByStageID[stageID] = env
		}
	}
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
		t.Fatalf("Status = %q, want settled — a stage that settled while the amendment list was in flight must not surface as amendment_pending", out.Status)
	}
	if out.State != "succeeded" || !out.Terminal {
		t.Errorf("State/Terminal = %q/%v, want succeeded/true from the re-check read", out.State, out.Terminal)
	}
	if out.PendingAmendment != nil {
		t.Errorf("PendingAmendment = %+v, want nil on the settled resolve", out.PendingAmendment)
	}
}

// TestAwaitStage_AmendmentReleaseSurvivesReCheckReadFailure covers the
// settledness re-check's own failure branch: when the re-read fails, the
// amendment release still stands (a pending amendment is KNOWN to exist;
// settledness merely could not be confirmed) rather than failing the wait or
// dereferencing a nil envelope.
func TestAwaitStage_AmendmentReleaseSurvivesReCheckReadFailure(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "running", false)
	amendmentID := seedAmendment(fb, runID, stageID, "pending", "docs/whatever.md")
	// Arm a 500 on the settledness RE-CHECK read (stage read 2): the arm read
	// (read 1) already succeeded, so the failure lands only on the re-check.
	fb.stageWaitFlip = func(sid uuid.UUID, reads int) {
		if sid == stageID && reads == 2 {
			fb.stageWaitStatusByStageID[sid] = 500
		}
	}
	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	_, out, err := r.awaitStage(context.Background(), nil, AwaitStageInput{
		RunID:          runID.String(),
		Stage:          "implement",
		TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("awaitStage returned an error despite a best-effort re-check failure: %v", err)
	}
	if out.Status != "amendment_pending" {
		t.Fatalf("Status = %q, want amendment_pending — a failed re-check leaves the amendment release in charge", out.Status)
	}
	if out.PendingAmendment == nil || out.PendingAmendment.ID != amendmentID.String() {
		t.Fatalf("PendingAmendment = %+v, want the seeded amendment %s", out.PendingAmendment, amendmentID)
	}
}

// TestAwaitStage_AmendmentMessageWithNoPaths covers the message's own degrade
// branch: a row carrying no paths must render "(no paths listed)" rather than a
// dangling "for ." — the message is what the operator reads to decide.
func TestAwaitStage_AmendmentMessageWithNoPaths(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	// Seeded with NO paths by construction (the variadic is empty), so the red
	// lands on the message assertion, not on fixture setup.
	stageID := seedStageWait(fb, runID, "implement", "running", false)
	seedAmendment(fb, runID, stageID, "pending")
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
	if out.Status != "amendment_pending" {
		t.Fatalf("Status = %q, want amendment_pending", out.Status)
	}
	if !strings.Contains(out.Message, "(no paths listed)") {
		t.Errorf("message should degrade to the no-paths placeholder; got %q", out.Message)
	}
}

// TestAwaitStage_DecidedAmendmentDoesNotRelease pins the strict status
// predicate: already-decided (approved / denied) amendments are not actionable
// and must not release a re-armed wait. Deleting the Status=="pending" filter
// makes this release instead of timing out.
func TestAwaitStage_DecidedAmendmentDoesNotRelease(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "running", false)
	seedAmendment(fb, runID, stageID, "approved", "docs/a.md")
	seedAmendment(fb, runID, stageID, "denied", "docs/b.md")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fb.stageWaitFlip = func(sid uuid.UUID, reads int) {
		if sid == stageID && reads == 3 {
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
		t.Fatalf("Status = %q, want timeout — a DECIDED amendment must not release the wait", out.Status)
	}
}

// TestAwaitStage_AmendmentForAnotherStageDoesNotRelease pins the strict
// stage_id predicate: a sibling stage's pending amendment on the SAME run must
// not release this stage's wait. Deleting the StageID equality filter makes
// this release instead of timing out.
func TestAwaitStage_AmendmentForAnotherStageDoesNotRelease(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "running", false)
	siblingID := seedStageWait(fb, runID, "acceptance", "running", false)
	seedAmendment(fb, runID, siblingID, "pending", "docs/sibling.md")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fb.stageWaitFlip = func(sid uuid.UUID, reads int) {
		if sid == stageID && reads == 3 {
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
		t.Fatalf("Status = %q, want timeout — a SIBLING stage's amendment must not release this wait", out.Status)
	}
}

// TestAwaitStage_AmendmentListErrorDoesNotFailTheWait proves the probe is
// best-effort: a 500-ing scope-amendments endpoint must neither fail nor abort
// the wait — the stage still settles mid-poll. Replacing the nil-on-error
// return with a returned error fails this.
func TestAwaitStage_AmendmentListErrorDoesNotFailTheWait(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "running", false)
	fb.amendmentsStatus = 500
	fb.stageWaitFlip = settleStageWaitAt(fb, stageID, 3, "succeeded")
	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	_, out, err := r.awaitStage(context.Background(), nil, AwaitStageInput{
		RunID:          runID.String(),
		Stage:          "implement",
		TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("awaitStage returned an error despite a best-effort amendment probe: %v", err)
	}
	if out.Status != "settled" {
		t.Fatalf("Status = %q, want settled (the poll path stayed in charge through a 500-ing probe)", out.Status)
	}
	if got := amendmentReads(fb, runID); got < 2 {
		t.Errorf("amendment list reads = %d, want >= 2 (the failing probe kept being attempted, not disabled)", got)
	}
}

// TestAwaitStage_RunTerminalStillWinsWhenNoAmendment is the regression pin that
// the ADR-036 backstop is unchanged when the probe finds nothing: the amendment
// probe is inserted BEFORE the backstop, so a probe that swallowed the
// no-amendment case incorrectly (or resolved on it) would shadow run_terminal.
func TestAwaitStage_RunTerminalStillWinsWhenNoAmendment(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedStageWait(fb, runID, "implement", "running", false)
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "failed"}
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
	if out.Status != "run_terminal" {
		t.Fatalf("Status = %q, want run_terminal (the backstop must still fire when no amendment is pending)", out.Status)
	}
}

// TestAwaitStageToolDescription_NamesAmendmentRelease pins the tool text: the
// verb must advertise its SECOND release condition, and must not imply the
// settled-state set is the only one.
func TestAwaitStageToolDescription_NamesAmendmentRelease(t *testing.T) {
	ctx := context.Background()
	_, srv := newFakeBackend(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0"}, nil)
	registerAwaitStage(server, newResolver(srv, nil))

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

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var desc string
	for _, tool := range res.Tools {
		if tool.Name == "fishhawk_await_stage" {
			desc = tool.Description
			break
		}
	}
	if desc == "" {
		t.Fatal("fishhawk_await_stage not registered")
	}
	lower := strings.ToLower(desc)
	for _, want := range []string{
		"two release conditions",
		"amendment_pending",
		"expires",
		"does not park the stage",
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("await_stage description must carry %q; got:\n%s", want, desc)
		}
	}
	// The retired proceed-as-denied wording must be gone from the operator-facing
	// tool description (#2601 / #2617).
	if strings.Contains(lower, "as if denied") {
		t.Errorf("await_stage description must not carry the retired 'as if denied' wording; got:\n%s", desc)
	}
}

// TestAwaitStageProgressMessage pins the pure heartbeat-message helper.
func TestAwaitStageProgressMessage(t *testing.T) {
	got := awaitStageProgressMessage("implement", 12*time.Second)
	if got != `await_stage: waiting for stage "implement" to settle; elapsed 12s` {
		t.Errorf("awaitStageProgressMessage = %q", got)
	}
}

// --- fix-up recovery marker on the settled response (E68.31 / #3081) ---

// TestAwaitStage_SettledCarriesFixupRecoveryMarker is the DONE-MEANS assertion
// on the shipped await_stage response: an implement stage whose latest fix-up
// pass FAILED and was recovered settles as `succeeded` — true of the stage,
// misleading about the fix-up — so the response must carry the marker on
// stage_wait_status AND repeat its advisory at the top level.
func TestAwaitStage_SettledCarriesFixupRecoveryMarker(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "succeeded", true)
	seedFixupTriggeredAudit(fb, runID, stageID)
	seedFixupRecoveredPayload(fb, runID, stageID, "A", "the fix-up agent exited non-zero")
	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	_, out, err := r.awaitStage(context.Background(), nil, AwaitStageInput{
		RunID: runID.String(),
		Stage: "implement",
	})
	if err != nil {
		t.Fatalf("awaitStage: %v", err)
	}
	if out.Status != "settled" || out.State != "succeeded" {
		t.Fatalf("Status/State = %q/%q, want settled/succeeded", out.Status, out.State)
	}
	if out.StageWaitStatus == nil || out.StageWaitStatus.FixupRecovered == nil {
		t.Fatalf("stage_wait_status.fixup_recovered is absent; want the #3081 marker (status=%+v)", out.StageWaitStatus)
	}
	rec := out.StageWaitStatus.FixupRecovered
	if rec.SourceFailureCategory != "A" {
		t.Errorf("source_failure_category = %q, want A", rec.SourceFailureCategory)
	}
	if rec.SourceFailureReason != "the fix-up agent exited non-zero" {
		t.Errorf("source_failure_reason = %q", rec.SourceFailureReason)
	}
	if !rec.DetailsAvailable {
		t.Error("details_available = false, want true")
	}
	if out.Message == "" {
		t.Error("top-level message is empty; a recovered fix-up must not settle silently")
	}
	if out.Message != rec.Message {
		t.Errorf("top-level message differs from the marker's:\n top: %s\n rec: %s", out.Message, rec.Message)
	}
}

// TestAwaitStage_SucceededFixupCarriesNoMarker is the CONTROL: a fix-up that
// genuinely LANDED writes no stage_fixup_recovered entry, so the settled
// response must be byte-identical to today — no marker, no message.
func TestAwaitStage_SucceededFixupCarriesNoMarker(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "succeeded", true)
	seedFixupTriggeredAudit(fb, runID, stageID)
	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	_, out, err := r.awaitStage(context.Background(), nil, AwaitStageInput{
		RunID: runID.String(),
		Stage: "implement",
	})
	if err != nil {
		t.Fatalf("awaitStage: %v", err)
	}
	if out.StageWaitStatus == nil {
		t.Fatal("stage_wait_status is nil")
	}
	if out.StageWaitStatus.FixupRecovered != nil {
		t.Errorf("fixup_recovered = %+v, want absent for a fix-up that landed", out.StageWaitStatus.FixupRecovered)
	}
	if out.Message != "" {
		t.Errorf("message = %q, want empty on an ordinary settled response", out.Message)
	}
}

// TestAwaitStage_NonImplementStageNeverProbes pins the cost gate: only an
// implement stage can carry a fix-up, so a plan wait must issue NO audit read
// at all. Asserted on the fake's per-category read counters, not on the output
// alone — an output-only assertion would pass even if the probe ran and found
// nothing.
func TestAwaitStage_NonImplementStageNeverProbes(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedStageWait(fb, runID, "plan", "succeeded", true)
	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	_, out, err := r.awaitStage(context.Background(), nil, AwaitStageInput{
		RunID: runID.String(),
		Stage: "plan",
	})
	if err != nil {
		t.Fatalf("awaitStage: %v", err)
	}
	if out.StageWaitStatus != nil && out.StageWaitStatus.FixupRecovered != nil {
		t.Errorf("fixup_recovered = %+v, want absent on a plan stage", out.StageWaitStatus.FixupRecovered)
	}
	fb.mu.Lock()
	triggerReads := fb.perRunAuditCategoryReads[categoryStageFixupTriggered]
	recoveredReads := fb.perRunAuditCategoryReads[categoryStageFixupRecovered]
	fb.mu.Unlock()
	if triggerReads != 0 || recoveredReads != 0 {
		t.Errorf("audit reads on a plan wait = %d trigger / %d recovered, want 0/0 (the probe is implement-only)", triggerReads, recoveredReads)
	}
}

// TestAwaitStage_AuditErrorStillReturnsTheSettledResponse is the operator's
// BINDING CONDITION 3 case, at the boundary where the wait actually happens:
// the audit read fails while the stage settles normally. The whole reason the
// probe is best-effort is that a multi-hour wait must never fail on an
// advisory, so the settled response must come back INTACT with the marker
// simply absent — not an error, not a degraded status.
func TestAwaitStage_AuditErrorStillReturnsTheSettledResponse(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "succeeded", true)
	seedFixupTriggeredAudit(fb, runID, stageID)
	seedFixupRecoveredPayload(fb, runID, stageID, "C", "commit/push onto PR branch failed")
	// The audit endpoint now 500s: the probe cannot read the recovery it would
	// otherwise report.
	fb.mu.Lock()
	fb.perRunAuditStatus = 500
	fb.mu.Unlock()
	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	_, out, err := r.awaitStage(context.Background(), nil, AwaitStageInput{
		RunID: runID.String(),
		Stage: "implement",
	})
	if err != nil {
		t.Fatalf("awaitStage returned an error on a failing audit read: %v (the probe must be best-effort)", err)
	}
	if out.Status != "settled" || out.State != "succeeded" || !out.Terminal {
		t.Fatalf("settled response degraded: Status/State/Terminal = %q/%q/%v, want settled/succeeded/true", out.Status, out.State, out.Terminal)
	}
	if out.StageID != stageID.String() {
		t.Errorf("StageID = %q, want %s (the settled response must be intact)", out.StageID, stageID)
	}
	if out.StageWaitStatus == nil {
		t.Fatal("stage_wait_status is nil; the settled response must be intact")
	}
	if out.StageWaitStatus.FixupRecovered != nil {
		t.Errorf("fixup_recovered = %+v, want absent when the audit read failed", out.StageWaitStatus.FixupRecovered)
	}
	if out.Message != "" {
		t.Errorf("message = %q, want empty when the marker could not be resolved", out.Message)
	}
}

// TestAwaitStage_MarkerSurvivesTheMidPollSettlePath proves the decoration is
// wired on the POLL path too, not just the fast path: the three settled call
// sites all route through awaitStageSettled, so a stage that settles mid-poll
// carries the marker as well.
func TestAwaitStage_MarkerSurvivesTheMidPollSettlePath(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "running", false)
	seedFixupTriggeredAudit(fb, runID, stageID)
	seedFixupRecoveredPayload(fb, runID, stageID, "B", "scope drift refused")
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
	if out.StageWaitStatus == nil || out.StageWaitStatus.FixupRecovered == nil {
		t.Fatal("fixup_recovered is absent on the mid-poll settle path")
	}
	if out.StageWaitStatus.FixupRecovered.SourceFailureCategory != "B" {
		t.Errorf("source_failure_category = %q, want B", out.StageWaitStatus.FixupRecovered.SourceFailureCategory)
	}
}

// TestAwaitStage_MarkerSurvivesTheBackstopFinalReadPath is the third of the
// four settled call sites: a stage that settles on the run-terminal backstop's
// FINAL read resolves through awaitStageSettled too, so it must carry the
// marker. Without a behavioural case here, a refactor of the backstop back to
// the pure awaitStageSettledOutput would drop the marker silently while patch
// coverage stayed green.
func TestAwaitStage_MarkerSurvivesTheBackstopFinalReadPath(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "running", false)
	seedFixupTriggeredAudit(fb, runID, stageID)
	seedFixupRecoveredPayload(fb, runID, stageID, "C", "commit/push onto PR branch failed")
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "succeeded"}
	// Read 1 is the fast path (unsettled); read 2 is the backstop's final read,
	// where the stage settles — so the backstop resolves settled, not
	// run_terminal.
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
	if out.StageWaitStatus == nil || out.StageWaitStatus.FixupRecovered == nil {
		t.Fatal("fixup_recovered is absent on the run-terminal backstop settle path")
	}
	if got := out.StageWaitStatus.FixupRecovered.SourceFailureCategory; got != "C" {
		t.Errorf("source_failure_category = %q, want C", got)
	}
	if out.Message != out.StageWaitStatus.FixupRecovered.Message {
		t.Errorf("top-level message differs from the marker's:\n top: %s\n rec: %s", out.Message, out.StageWaitStatus.FixupRecovered.Message)
	}
}

// TestAwaitStage_MarkerSurvivesTheAmendmentReleaseSettlePath is the fourth
// settled call site: the amendment release's settledness re-check wins the race
// and resolves through awaitStageSettled, so the marker must ride out on that
// path too.
func TestAwaitStage_MarkerSurvivesTheAmendmentReleaseSettlePath(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := seedStageWait(fb, runID, "implement", "running", false)
	seedFixupTriggeredAudit(fb, runID, stageID)
	seedFixupRecoveredPayload(fb, runID, stageID, "A", "the fix-up agent exited non-zero")
	seedAmendment(fb, runID, stageID, "pending", "docs/whatever.md")
	// The stage settles while the amendment list is in flight, so the release's
	// re-check read finds it terminal and resolves settled.
	fb.amendmentsFlip = func(rid uuid.UUID, reads int) {
		if rid == runID && reads == 1 {
			env := fb.stageWaitByStageID[stageID]
			env.State = "succeeded"
			env.Terminal = true
			fb.stageWaitByStageID[stageID] = env
		}
	}
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
	if out.StageWaitStatus == nil || out.StageWaitStatus.FixupRecovered == nil {
		t.Fatal("fixup_recovered is absent on the amendment-release settle path")
	}
	if got := out.StageWaitStatus.FixupRecovered.SourceFailureCategory; got != "A" {
		t.Errorf("source_failure_category = %q, want A", got)
	}
}

// TestDecorateSettledWithFixupRecovery_PairsBlockAndMessage pins the pairing
// rule the four settled call sites share: the structured block and the
// top-level prose are ONE advisory, attached together or not at all. The
// nil-block arm is unreachable through awaitStageSettled today (its builder
// always populates StageWaitStatus), which is exactly why the rule is a pure
// function — a test can hand it the shape the builder cannot produce, so a
// future refactor that makes the field optional is caught rather than silently
// emitting a warning with no block to act on.
func TestDecorateSettledWithFixupRecovery_PairsBlockAndMessage(t *testing.T) {
	rec := &FixupRecovery{Message: "the fix-up pass FAILED", DetailsAvailable: true}

	// No block to attach to: neither half is written.
	bare := decorateSettledWithFixupRecovery(AwaitStageOutput{Status: "settled"}, rec)
	if bare.Message != "" {
		t.Errorf("Message = %q, want empty — the prose must not ship without the block it describes", bare.Message)
	}
	if bare.StageWaitStatus != nil {
		t.Errorf("StageWaitStatus = %+v, want nil (unchanged)", bare.StageWaitStatus)
	}

	// No marker: the settled response is returned untouched.
	none := decorateSettledWithFixupRecovery(AwaitStageOutput{Status: "settled", StageWaitStatus: &StageWaitStatus{}}, nil)
	if none.Message != "" || none.StageWaitStatus.FixupRecovered != nil {
		t.Errorf("a nil marker decorated the response: %+v", none)
	}

	// Both present: both halves are written, and they agree.
	full := decorateSettledWithFixupRecovery(AwaitStageOutput{Status: "settled", StageWaitStatus: &StageWaitStatus{}}, rec)
	if full.StageWaitStatus.FixupRecovered != rec {
		t.Errorf("fixup_recovered = %+v, want the marker", full.StageWaitStatus.FixupRecovered)
	}
	if full.Message != rec.Message {
		t.Errorf("Message = %q, want %q", full.Message, rec.Message)
	}
}
