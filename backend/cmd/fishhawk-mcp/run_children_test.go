package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	runmodel "github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// --- clamp helper (approval-condition verification mode (e)) ---

func TestClampMaxParallel(t *testing.T) {
	cases := []struct {
		effective, override, want int
	}{
		{0, 2, 2},  // unlimited effective, override tightens to 2
		{2, 5, 2},  // override looser than effective → clamp DOWN to effective
		{2, 0, 2},  // no override → effective unchanged
		{0, 0, 0},  // unlimited, no override → still unlimited
		{5, 3, 3},  // override strictly tighter → override wins
		{3, 3, 3},  // override equal to effective → effective
		{2, -1, 2}, // negative override treated as no override
	}
	for _, c := range cases {
		if got := clampMaxParallel(c.effective, c.override); got != c.want {
			t.Errorf("clampMaxParallel(%d, %d) = %d, want %d", c.effective, c.override, got, c.want)
		}
	}
}

// --- spawn seam helpers ---

// withFakeSpawn swaps spawnRunnerStageFn for the duration of a test.
func withFakeSpawn(t *testing.T, fn func(ctx context.Context, binary string, argv, env []string, req *mcp.CallToolRequest, progToken any) ([]RunnerEvent, []string, int, error)) {
	t.Helper()
	orig := spawnRunnerStageFn
	spawnRunnerStageFn = fn
	t.Cleanup(func() { spawnRunnerStageFn = orig })
}

// completedEvents returns a one-event slice carrying a terminal
// runner_completed event with the given outcome, so summarizeRunStageEvents
// distills ChildResult.Outcome.
func completedEvents(outcome string) []RunnerEvent {
	return []RunnerEvent{{Payload: map[string]any{"event": "runner_completed", "outcome": outcome}}}
}

// seedPlanDecomposed appends a plan_decomposed audit entry to the parent run so
// LatestPlanDecomposed discovers the children + effective cap. No waves field is
// seeded, so the run_children loop collapses to a single all-indices wave
// (back-compat).
func seedPlanDecomposed(fb *fakeBackend, parent uuid.UUID, childIDs []string, effectiveMax int) {
	seedPlanDecomposedWaves(fb, parent, childIDs, effectiveMax, nil)
}

// seedPlanDecomposedWaves is seedPlanDecomposed with an explicit waves field —
// the topological dispatch order of slice indices into childIDs (#1278 slice B).
// A nil waves omits the field (back-compat single-wave).
func seedPlanDecomposedWaves(fb *fakeBackend, parent uuid.UUID, childIDs []string, effectiveMax int, waves [][]int) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	seq := int64(len(fb.perRunAuditByRun[parent]) + 1)
	payload := map[string]any{
		"child_run_ids":          childIDs,
		"effective_max_parallel": effectiveMax,
	}
	if waves != nil {
		payload["waves"] = waves
	}
	fb.perRunAuditByRun[parent] = append(fb.perRunAuditByRun[parent], AuditEntry{
		ID:       uuid.NewString(),
		Sequence: seq,
		RunID:    parent.String(),
		Category: "plan_decomposed",
		Payload:  payload,
	})
}

// seedChildRun seeds a child run row at the given state plus its implement
// stage at the SAME state, so the run-level and stage-level states agree —
// the shape the original partition tests rely on.
func seedChildRun(fb *fakeBackend, childID uuid.UUID, state string) {
	seedChildRunStage(fb, childID, state, state)
}

// seedChildRunStage seeds a child run row at runState plus its implement stage
// at a DISTINCT stageState. It reproduces a local decomposed child parked by
// RuleChildrenDispatch (#1143): the RUN is advanced to 'running' while the
// implement STAGE stays at pending/dispatched awaiting a host spawn (#1237).
func seedChildRunStage(fb *fakeBackend, childID uuid.UUID, runState, stageState string) {
	stageID := uuid.New()
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.getRunByID[childID] = Run{ID: childID.String(), State: runState, Repo: "x/y"}
	fb.stagesByRun[childID] = []Stage{{ID: stageID.String(), RunID: childID.String(), Type: "implement", State: stageState}}
}

// --- concurrency: peak in-flight never exceeds the cap ---

func TestRunChildren_ConcurrencyRespectsCap(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	const n = 5
	childIDs := make([]string, n)
	for i := range childIDs {
		c := uuid.New()
		childIDs[i] = c.String()
		seedChildRun(fb, c, "pending")
	}
	seedPlanDecomposed(fb, parent, childIDs, 2) // effective cap 2

	var (
		inFlight int32
		peak     int32
		argvSeen []string
		argvMu   sync.Mutex
	)
	withFakeSpawn(t, func(_ context.Context, _ string, argv, _ []string, _ *mcp.CallToolRequest, _ any) ([]RunnerEvent, []string, int, error) {
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if cur <= p || atomic.CompareAndSwapInt32(&peak, p, cur) {
				break
			}
		}
		time.Sleep(25 * time.Millisecond) // hold the slot so concurrency can build
		atomic.AddInt32(&inFlight, -1)
		argvMu.Lock()
		argvSeen = append(argvSeen, strings.Join(argv, " "))
		argvMu.Unlock()
		return completedEvents("ok"), nil, 0, nil
	})

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID:        parent.String(),
		Workflow:     "wf",
		GitHubRepo:   "x/y",
		RunnerBinary: "/fake/fishhawk-runner",
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != n {
		t.Errorf("dispatched_count = %d, want %d", out.DispatchedCount, n)
	}
	if out.EffectiveCap != 2 {
		t.Errorf("effective_cap = %d, want 2", out.EffectiveCap)
	}
	if got := atomic.LoadInt32(&peak); got > 2 {
		t.Errorf("peak concurrency = %d, want <= cap 2", got)
	}
	// Every dispatched child carried the --parallel-isolate flag (the
	// load-bearing MCP→runner contract).
	argvMu.Lock()
	defer argvMu.Unlock()
	if len(argvSeen) != n {
		t.Fatalf("recorded %d spawns, want %d", len(argvSeen), n)
	}
	for _, a := range argvSeen {
		if !strings.Contains(a, "--parallel-isolate") {
			t.Errorf("child argv missing --parallel-isolate: %q", a)
		}
	}
}

// --- unlimited cap: SetLimit is skipped, all children dispatch ---

func TestRunChildren_UnlimitedCapDispatchesAll(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	const n = 4
	childIDs := make([]string, n)
	for i := range childIDs {
		c := uuid.New()
		childIDs[i] = c.String()
		seedChildRun(fb, c, "pending")
	}
	seedPlanDecomposed(fb, parent, childIDs, 0) // 0 == unlimited

	withFakeSpawn(t, func(_ context.Context, _ string, _, _ []string, _ *mcp.CallToolRequest, _ any) ([]RunnerEvent, []string, int, error) {
		return completedEvents("ok"), nil, 0, nil
	})

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.EffectiveCap != 0 {
		t.Errorf("effective_cap = %d, want 0 (unlimited)", out.EffectiveCap)
	}
	if out.DispatchedCount != n {
		t.Errorf("dispatched_count = %d, want %d", out.DispatchedCount, n)
	}
}

// --- await-all, no sibling-cancel: a child failure is data, not a tool error ---

func TestRunChildren_AwaitsAllNoSiblingCancel(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	failID := uuid.New()
	okID1 := uuid.New()
	okID2 := uuid.New()
	for _, c := range []uuid.UUID{failID, okID1, okID2} {
		seedChildRun(fb, c, "pending")
	}
	seedPlanDecomposed(fb, parent, []string{failID.String(), okID1.String(), okID2.String()}, 0)

	var completed int32
	withFakeSpawn(t, func(_ context.Context, _ string, argv, _ []string, _ *mcp.CallToolRequest, _ any) ([]RunnerEvent, []string, int, error) {
		atomic.AddInt32(&completed, 1)
		// The failing child returns a non-zero exit + failed outcome; the
		// others succeed. None of this should cancel the siblings.
		if strings.Contains(strings.Join(argv, " "), failID.String()) {
			return completedEvents("failed"), nil, 7, nil
		}
		return completedEvents("ok"), nil, 0, nil
	})

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	// A child failure must NOT surface as a Go tool error.
	if err != nil {
		t.Fatalf("runChildren returned an error for a child failure: %v", err)
	}
	// ALL three children were awaited (the fake ran for each).
	if got := atomic.LoadInt32(&completed); got != 3 {
		t.Errorf("spawned %d children, want all 3 awaited", got)
	}
	if len(out.Children) != 3 {
		t.Fatalf("children = %d, want 3", len(out.Children))
	}
	// The failure appears as DATA in the consolidated result.
	var fail *ChildResult
	for i := range out.Children {
		if out.Children[i].RunID == failID.String() {
			fail = &out.Children[i]
		}
	}
	if fail == nil {
		t.Fatal("failing child missing from children[]")
	}
	if fail.ExitCode != 7 {
		t.Errorf("failing child exit_code = %d, want 7", fail.ExitCode)
	}
	if fail.Outcome != "failed" {
		t.Errorf("failing child outcome = %q, want failed", fail.Outcome)
	}
}

// --- spawnErr branch: a failed-to-start spawn is a per-child warning, not a
// tool error, and does NOT cancel siblings ---

// TestRunChildren_SpawnErrIsDataNoSiblingCancel exercises the spawnErr branch of
// the dispatch loop (run_children.go: spawnRunnerStageFn returning a non-nil
// err — spawnRunnerStage failing to START, or returning a non-ExitError wait
// failure). That path is distinct from a non-zero runner exit (which carries a
// nil err): it must convert the failure into a per-child "spawn failed" warning,
// leave Outcome empty, and still return no tool error / no sibling-cancel.
func TestRunChildren_SpawnErrIsDataNoSiblingCancel(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	spawnErrID := uuid.New()
	okID1 := uuid.New()
	okID2 := uuid.New()
	for _, c := range []uuid.UUID{spawnErrID, okID1, okID2} {
		seedChildRun(fb, c, "pending")
	}
	seedPlanDecomposed(fb, parent, []string{spawnErrID.String(), okID1.String(), okID2.String()}, 0)

	var completed int32
	withFakeSpawn(t, func(_ context.Context, _ string, argv, _ []string, _ *mcp.CallToolRequest, _ any) ([]RunnerEvent, []string, int, error) {
		atomic.AddInt32(&completed, 1)
		// One child's spawn fails to start (non-nil err, no events, no exit
		// code) — the spawnErr branch. The others spawn cleanly.
		if strings.Contains(strings.Join(argv, " "), spawnErrID.String()) {
			return nil, nil, 0, errors.New("fork/exec: no such file or directory")
		}
		return completedEvents("ok"), nil, 0, nil
	})

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	// A spawn failure must NOT surface as a Go tool error.
	if err != nil {
		t.Fatalf("runChildren returned an error for a spawn failure: %v", err)
	}
	// ALL three children were awaited (the spawnErr did not cancel siblings).
	if got := atomic.LoadInt32(&completed); got != 3 {
		t.Errorf("spawned %d children, want all 3 awaited despite the spawnErr", got)
	}
	if len(out.Children) != 3 {
		t.Fatalf("children = %d, want 3", len(out.Children))
	}
	byID := map[string]ChildResult{}
	for _, c := range out.Children {
		byID[c.RunID] = c
	}
	// The spawn-failed child is reported as DATA: dispatched, no terminal
	// outcome, and a "spawn failed" warning.
	failed := byID[spawnErrID.String()]
	if !failed.Dispatched {
		t.Errorf("spawn-failed child not marked dispatched: %+v", failed)
	}
	if failed.Outcome != "" {
		t.Errorf("spawn-failed child outcome = %q, want empty (no terminal runner_completed)", failed.Outcome)
	}
	var sawSpawnWarning bool
	for _, w := range failed.Warnings {
		if strings.Contains(w, "spawn failed") {
			sawSpawnWarning = true
		}
	}
	if !sawSpawnWarning {
		t.Errorf("spawn-failed child warnings = %v, want one containing 'spawn failed'", failed.Warnings)
	}
	// The siblings still distilled a clean outcome — no sibling-cancel.
	for _, ok := range []uuid.UUID{okID1, okID2} {
		if c := byID[ok.String()]; c.Outcome != "ok" {
			t.Errorf("sibling %s outcome = %q, want ok (spawnErr must not cancel it)", ok, c.Outcome)
		}
	}
}

// --- concurrent invocations: the MCP-layer partition seam under a race ---

// TestRunChildren_ConcurrentInvocationsNoToolError exercises the seam the
// low/correctness coverage note flagged: two overlapping run_children calls on
// the same parent. The single-call partition (pending vs in-flight vs terminal)
// is covered by TestRunChildren_PartitionsPendingOnly; here we prove that the
// MCP layer ITSELF tolerates concurrent invocations — neither call returns a
// tool error and the pending child is dispatched (spawned) exactly as many
// times as callers observed it pending.
//
// Post-#1912-fixup the per-child host-dispatch marker (fired at spawn time,
// CAS-flipping the child's implement stage pending → dispatched) ELIMINATES the
// old one-slot overshoot: run_children keys the spawn on the marker's
// transitioned signal, so exactly ONE caller wins the CAS (transitioned:true →
// spawns) and the other observes the idempotent no-op (transitioned:false → does
// NOT spawn). Whether the loser reads the child AFTER the winner's marker (its
// partition sees 'dispatched' and skips) or BEFORE (both partitions read
// 'pending' but the loser's marker returns transitioned:false), combined
// dispatch is exactly 1 and actual spawns equal it — no double-spawn.
func TestRunChildren_ConcurrentInvocationsNoToolError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	childID := uuid.New()
	seedChildRun(fb, childID, "pending")
	seedPlanDecomposed(fb, parent, []string{childID.String()}, 0)

	var spawns int32
	withFakeSpawn(t, func(_ context.Context, _ string, _, _ []string, _ *mcp.CallToolRequest, _ any) ([]RunnerEvent, []string, int, error) {
		atomic.AddInt32(&spawns, 1)
		time.Sleep(10 * time.Millisecond) // widen the race window
		return completedEvents("ok"), nil, 0, nil
	})

	const callers = 2
	var wg sync.WaitGroup
	errs := make([]error, callers)
	dispatched := make([]int, callers)
	for i := 0; i < callers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
				RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
			})
			errs[i] = err
			dispatched[i] = out.DispatchedCount
		}()
	}
	wg.Wait()

	// Neither concurrent invocation returns a tool error.
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent runChildren[%d] returned an error: %v", i, err)
		}
	}
	// Each call dispatched 0 or 1 (a caller either won the host-dispatch marker
	// CAS and spawned, or observed the idempotent no-op and skipped), and the
	// combined total is EXACTLY 1 — the marker's transitioned signal makes the
	// double-spawn impossible (#1912 fix-up). At least one caller must have
	// dispatched it: the MCP layer never silently drops the dispatch.
	combined := 0
	for i, d := range dispatched {
		if d < 0 || d > 1 {
			t.Errorf("concurrent runChildren[%d] dispatched_count = %d, want 0 or 1", i, d)
		}
		combined += d
	}
	if combined != 1 {
		t.Errorf("combined dispatched_count = %d, want exactly 1 (the marker's transitioned signal eliminates the double-spawn)", combined)
	}
	// Actual spawns equal the combined dispatched_count: the winning caller
	// spawned exactly once, and the caller that observed the marker no-op spawned
	// nothing — no double-spawn of a runner against the same child.
	if got := int(atomic.LoadInt32(&spawns)); got != combined {
		t.Errorf("total spawns = %d, want %d (== combined dispatched_count)", got, combined)
	}
}

// --- fail-closed host-dispatch marker (#1912 fix-up; plan test c) ---

// TestRunChildren_HostDispatchMarkerFails_NoSpawnStopsWave pins run_children's
// fail-closed posture for the host-dispatch marker — the branch the low/
// test-coverage concern flagged as untested here, analogous to the
// run_stage/dispatch_stage/drive_run marker-4xx tests (fb.hostDispatchStatus).
// When a child's marker 4xxes, that child spawns NO runner, is reported as NOT
// dispatched with the fail-closed warning, and — because its empty Outcome trips
// the partial-wave guard — the dependent next wave is never dispatched and
// integrate-wave is never called.
func TestRunChildren_HostDispatchMarkerFails_NoSpawnStopsWave(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	child0, child1 := uuid.New(), uuid.New()
	seedChildRun(fb, child0, "pending")
	seedChildRun(fb, child1, "pending")
	seedPlanDecomposedWaves(fb, parent, []string{child0.String(), child1.String()}, 0, [][]int{{0}, {1}})

	fb.mu.Lock()
	fb.hostDispatchStatus = http.StatusConflict // the marker 4xx -> fail closed
	fb.mu.Unlock()

	var spawns int32
	withFakeSpawn(t, func(_ context.Context, _ string, _, _ []string, _ *mcp.CallToolRequest, _ any) ([]RunnerEvent, []string, int, error) {
		atomic.AddInt32(&spawns, 1)
		return completedEvents("ok"), nil, 0, nil
	})

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	// A marker failure is DATA, never a tool error.
	if err != nil {
		t.Fatalf("runChildren returned an error for a marker 4xx (must be data): %v", err)
	}
	// Fail closed: NO runner spawned despite the child being partitioned pending.
	if got := atomic.LoadInt32(&spawns); got != 0 {
		t.Fatalf("spawned %d runners despite a failed host-dispatch marker; must fail closed", got)
	}
	// dispatched_count reflects ACTUAL spawns (zero), not the attempt.
	if out.DispatchedCount != 0 {
		t.Errorf("dispatched_count = %d, want 0 (a marker fail-closed spawns nothing)", out.DispatchedCount)
	}
	byID := map[string]ChildResult{}
	for _, c := range out.Children {
		byID[c.RunID] = c
	}
	// The wave-0 child is reported NOT dispatched, carrying the fail-closed warning.
	c0 := byID[child0.String()]
	if c0.Dispatched {
		t.Errorf("wave-0 child marked dispatched despite the marker fail-closed: %+v", c0)
	}
	var sawWarn bool
	for _, w := range c0.Warnings {
		if strings.Contains(w, "host-dispatch marker failed") && strings.Contains(w, "NOT spawning") {
			sawWarn = true
		}
	}
	if !sawWarn {
		t.Errorf("wave-0 child warnings = %v, want one carrying the fail-closed marker message", c0.Warnings)
	}
	// The partial-wave guard tripped: wave 1 never dispatched, no integrate-wave.
	if c1 := byID[child1.String()]; c1.Dispatched {
		t.Errorf("wave-1 child dispatched despite the wave-0 marker failure; the partial-wave guard did not stop the loop")
	}
	fb.mu.Lock()
	calls := fb.integrateWaveCalledBy[parent]
	fb.mu.Unlock()
	if calls != 0 {
		t.Errorf("integrate-wave calls = %d, want 0 (no integration after a fail-closed wave)", calls)
	}
	if !containsWarning(out.Warnings, "did not succeed") {
		t.Errorf("warnings = %v, want one mentioning the failed wave", out.Warnings)
	}
}

// --- uuid-parse fail-closed guard (in-closure half; #1945) ---

// TestRunChildren_StageIDUnparseable_FailsClosedStopsWave pins the STAGE-ID
// half of the in-closure uuid.Parse fail-closed guard at run_children.go:338-348.
// resolveStage (run_stage.go:1233) returns the backend-provided stage ID string
// verbatim without itself validating it as a UUID, so a seeded non-UUID
// implement-stage id reaches the g.Go closure's uuid.Parse(d.stageID) check
// before any host-dispatch marker call or spawn.
//
// The RUN-ID half of the same guard is deliberately NOT exercised here: the
// partition loop at run_children.go:224-230 already uuid.Parses every
// child_run_id and marks an unparseable one non-pending ("not a valid UUID;
// skipped") before dispatch ever runs — covered by
// TestLatestPlanDecomposed_CorruptPayloadErrors and the partition tests above —
// so the closure's cerr branch is defensive-only and unreachable in practice.
// That is the recorded rationale for deliberately closing only the stage-ID
// half (approved plan risk note).
func TestRunChildren_StageIDUnparseable_FailsClosedStopsWave(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	child0, child1 := uuid.New(), uuid.New()
	seedChildRun(fb, child0, "pending")
	seedChildRun(fb, child1, "pending")
	seedPlanDecomposedWaves(fb, parent, []string{child0.String(), child1.String()}, 0, [][]int{{0}, {1}})

	// Overwrite the wave-0 child's implement stage with a non-UUID stage id.
	// resolveStage matches by Type "implement" and passes the ID through
	// verbatim, so the partition still marks it pending (State "pending"), but
	// the g.Go closure's uuid.Parse(d.stageID) guard fails closed.
	fb.mu.Lock()
	fb.stagesByRun[child0] = []Stage{{ID: "not-a-uuid", RunID: child0.String(), Type: "implement", State: "pending"}}
	fb.mu.Unlock()

	var spawns int32
	withFakeSpawn(t, func(_ context.Context, _ string, _, _ []string, _ *mcp.CallToolRequest, _ any) ([]RunnerEvent, []string, int, error) {
		atomic.AddInt32(&spawns, 1)
		return completedEvents("ok"), nil, 0, nil
	})

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	// An unparseable stage id is DATA, never a tool error.
	if err != nil {
		t.Fatalf("runChildren returned an error for an unparseable stage id (must be data): %v", err)
	}
	// Fail closed: NO runner spawned despite the child being partitioned pending.
	if got := atomic.LoadInt32(&spawns); got != 0 {
		t.Fatalf("spawned %d runners despite an unparseable stage id; must fail closed", got)
	}
	if out.DispatchedCount != 0 {
		t.Errorf("dispatched_count = %d, want 0 (a uuid-parse fail-closed spawns nothing)", out.DispatchedCount)
	}
	byID := map[string]ChildResult{}
	for _, c := range out.Children {
		byID[c.RunID] = c
	}
	c0 := byID[child0.String()]
	if c0.Dispatched {
		t.Errorf("wave-0 child marked dispatched despite an unparseable stage id: %+v", c0)
	}
	var sawWarn bool
	for _, w := range c0.Warnings {
		if strings.Contains(w, "could not parse child run/stage id") && strings.Contains(w, "NOT spawning") {
			sawWarn = true
		}
	}
	if !sawWarn {
		t.Errorf("wave-0 child warnings = %v, want one carrying the uuid-parse fail-closed message", c0.Warnings)
	}
	// The partial-wave guard tripped: wave 1 never dispatched, no integrate-wave.
	if c1 := byID[child1.String()]; c1.Dispatched {
		t.Errorf("wave-1 child dispatched despite the wave-0 uuid-parse failure; the partial-wave guard did not stop the loop")
	}
	fb.mu.Lock()
	calls := fb.integrateWaveCalledBy[parent]
	fb.mu.Unlock()
	if calls != 0 {
		t.Errorf("integrate-wave calls = %d, want 0 (no integration after a fail-closed wave)", calls)
	}
}

// --- concurrent no-op skip (deterministic; #1945) ---

// TestRunChildren_MarkerNoop_SkipsSpawnDeterministic pins the
// transitioned:false skip branch (run_children.go:359-372) deterministically,
// forcing the host-dispatch marker to answer the concurrent-invocation no-op
// via the fakeBackend's hostDispatchForceNoop knob — rather than relying on the
// lucky goroutine interleaving TestRunChildren_ConcurrentInvocationsNoToolError
// depends on to occasionally hit this branch.
func TestRunChildren_MarkerNoop_SkipsSpawnDeterministic(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	childID := uuid.New()
	seedChildRun(fb, childID, "pending")
	seedPlanDecomposed(fb, parent, []string{childID.String()}, 0)

	fb.mu.Lock()
	fb.hostDispatchForceNoop = true
	fb.mu.Unlock()

	var spawns int32
	withFakeSpawn(t, func(_ context.Context, _ string, _, _ []string, _ *mcp.CallToolRequest, _ any) ([]RunnerEvent, []string, int, error) {
		atomic.AddInt32(&spawns, 1)
		return completedEvents("ok"), nil, 0, nil
	})

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	// A marker no-op is DATA, never a tool error.
	if err != nil {
		t.Fatalf("runChildren returned an error for a marker no-op (must be data): %v", err)
	}
	// The no-op must NEVER spawn a second runner.
	if got := atomic.LoadInt32(&spawns); got != 0 {
		t.Fatalf("spawned %d runners despite a marker no-op; must skip to avoid a double-spawn", got)
	}
	if out.DispatchedCount != 0 {
		t.Errorf("dispatched_count = %d, want 0 (a marker no-op spawns nothing)", out.DispatchedCount)
	}
	if len(out.Children) != 1 {
		t.Fatalf("children = %d, want 1", len(out.Children))
	}
	c := out.Children[0]
	if c.Dispatched {
		t.Errorf("child marked dispatched despite the marker no-op: %+v", c)
	}
	if c.StageState != "dispatched" {
		t.Errorf("child stage_state = %q, want %q (the marker's echoed state)", c.StageState, "dispatched")
	}
	var sawWarn bool
	for _, w := range c.Warnings {
		if strings.Contains(w, "no-op") && strings.Contains(w, "NOT spawning to avoid a double-spawn") {
			sawWarn = true
		}
	}
	if !sawWarn {
		t.Errorf("child warnings = %v, want one carrying the no-op double-spawn-avoidance message", c.Warnings)
	}
	// The marker fired exactly once for the child's stage.
	fb.mu.Lock()
	stageID := fb.stagesByRun[childID][0].ID
	calls := fb.hostDispatchCalledByID[uuid.MustParse(stageID)]
	fb.mu.Unlock()
	if calls != 1 {
		t.Errorf("host-dispatch marker calls for the child's stage = %d, want exactly 1", calls)
	}
}

// --- discovery: no plan_decomposed entry → clean tool error ---

func TestRunChildren_NoDecompositionErrors(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent := uuid.New()
	_, _, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	if err == nil {
		t.Fatal("expected error for a run with no plan_decomposed entry")
	}
	if !strings.Contains(err.Error(), "not a decomposed parent") {
		t.Errorf("error = %v, want a 'not a decomposed parent' message", err)
	}
}

// --- discovery: only pending children dispatch; re-call is idempotent ---

func TestRunChildren_PartitionsPendingOnly(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	pendingID := uuid.New()
	runningID := uuid.New()
	doneID := uuid.New()
	seedChildRun(fb, pendingID, "pending")
	seedChildRun(fb, runningID, "running") // in-flight
	seedChildRun(fb, doneID, "succeeded")  // terminal
	seedPlanDecomposed(fb, parent, []string{pendingID.String(), runningID.String(), doneID.String()}, 0)

	var dispatchedIDs []string
	var mu sync.Mutex
	withFakeSpawn(t, func(_ context.Context, _ string, argv, _ []string, _ *mcp.CallToolRequest, _ any) ([]RunnerEvent, []string, int, error) {
		mu.Lock()
		dispatchedIDs = append(dispatchedIDs, strings.Join(argv, " "))
		mu.Unlock()
		return completedEvents("ok"), nil, 0, nil
	})

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != 1 {
		t.Errorf("dispatched_count = %d, want 1 (only the pending child)", out.DispatchedCount)
	}
	mu.Lock()
	if len(dispatchedIDs) != 1 || !strings.Contains(dispatchedIDs[0], pendingID.String()) {
		t.Errorf("dispatched the wrong children: %v (want only %s)", dispatchedIDs, pendingID)
	}
	mu.Unlock()
	// The in-flight and terminal children are reported as-is, not re-spawned.
	if len(out.Children) != 3 {
		t.Fatalf("children = %d, want 3", len(out.Children))
	}
	byID := map[string]ChildResult{}
	for _, c := range out.Children {
		byID[c.RunID] = c
	}
	if c := byID[runningID.String()]; c.Dispatched {
		t.Errorf("in-flight child marked dispatched: %+v", c)
	}
	if c := byID[doneID.String()]; c.Dispatched || c.StageState != "succeeded" {
		t.Errorf("terminal child = %+v, want not dispatched + state succeeded", c)
	}
	if c := byID[pendingID.String()]; !c.Dispatched {
		t.Errorf("pending child not dispatched: %+v", c)
	}
}

// --- predicate: dispatchable keys on the implement STAGE state (#1237) ---

func TestImplementStageDispatchable(t *testing.T) {
	cases := []struct {
		state string
		want  bool
	}{
		{"pending", true},
		{"awaiting_host_dispatch", true}, // #1912: the new host-spawnable park
		{"dispatched", false},            // #1912: a spawn attempt exists — in-flight, not re-dispatchable
		{"running", false},
		{"awaiting_approval", false},
		{"succeeded", false},
		{"failed", false},
		{"cancelled", false},
		{"", false},
	}
	for _, c := range cases {
		if got := implementStageDispatchable(c.state); got != c.want {
			t.Errorf("implementStageDispatchable(%q) = %v, want %v", c.state, got, c.want)
		}
	}
}

// TestRunChildren_LocalParkedChildrenDispatch is the behavioral done-means
// test for #1237: a decomposed parent whose children are at RUN state 'running'
// but implement STAGE state pending/awaiting_host_dispatch (the
// RuleChildrenDispatch-parked shape) must dispatch BOTH. Under the old run-state
// predicate this dispatched ZERO (run=='running' classified as in-flight); it
// passes only with the stage-state fix. Post-#1912 a 'dispatched' child (a spawn
// attempt EXISTS — a runner is in flight) is now SKIPPED as in-flight, and a
// GENUINELY executing (implement stage 'running') child is likewise skipped.
func TestRunChildren_LocalParkedChildrenDispatch(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	awaitingID := uuid.New()   // run='running', stage='awaiting_host_dispatch' => dispatch
	pendingID := uuid.New()    // run='running', stage='pending'                 => dispatch
	dispatchedID := uuid.New() // run='running', stage='dispatched'              => skip (in-flight, #1912)
	executingID := uuid.New()  // run='running', stage='running'                 => skip
	seedChildRunStage(fb, awaitingID, "running", "awaiting_host_dispatch")
	seedChildRunStage(fb, pendingID, "running", "pending")
	seedChildRunStage(fb, dispatchedID, "running", "dispatched")
	seedChildRunStage(fb, executingID, "running", "running")
	seedPlanDecomposed(fb, parent, []string{awaitingID.String(), pendingID.String(), dispatchedID.String(), executingID.String()}, 0)

	var argvSeen []string
	var mu sync.Mutex
	withFakeSpawn(t, func(_ context.Context, _ string, argv, _ []string, _ *mcp.CallToolRequest, _ any) ([]RunnerEvent, []string, int, error) {
		mu.Lock()
		argvSeen = append(argvSeen, strings.Join(argv, " "))
		mu.Unlock()
		return completedEvents("ok"), nil, 0, nil
	})

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != 2 {
		t.Fatalf("dispatched_count = %d, want 2 (the run='running' + stage=pending/awaiting_host_dispatch children)", out.DispatchedCount)
	}
	mu.Lock()
	if len(argvSeen) != 2 {
		t.Errorf("spawned %d children, want 2: %v", len(argvSeen), argvSeen)
	}
	for _, argv := range argvSeen {
		if !strings.Contains(argv, "--parallel-isolate") {
			t.Errorf("dispatch argv missing --parallel-isolate: %s", argv)
		}
	}
	mu.Unlock()

	byID := map[string]ChildResult{}
	for _, c := range out.Children {
		byID[c.RunID] = c
	}
	if c := byID[awaitingID.String()]; !c.Dispatched {
		t.Errorf("stage='awaiting_host_dispatch' child not dispatched: %+v", c)
	}
	if c := byID[pendingID.String()]; !c.Dispatched {
		t.Errorf("stage='pending' child not dispatched: %+v", c)
	}
	// The 'dispatched' child (a spawn attempt exists, #1912) is in-flight: not
	// re-spawned, and its stage_state reported as the stage state.
	if c := byID[dispatchedID.String()]; c.Dispatched || c.StageState != "dispatched" {
		t.Errorf("dispatched child = %+v, want not dispatched + stage_state dispatched", c)
	}
	// The genuinely-executing child (implement stage 'running') is in-flight:
	// not re-spawned, and its stage_state reported as the stage state.
	if c := byID[executingID.String()]; c.Dispatched || c.StageState != "running" {
		t.Errorf("executing child = %+v, want not dispatched + stage_state running", c)
	}
}

// --- input validation ---

func TestRunChildren_RequiresRunAndWorkflow(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	if _, _, err := r.runChildren(context.Background(), nil, RunChildrenInput{Workflow: "wf"}); err == nil {
		t.Error("expected error when run_id is empty")
	}
	if _, _, err := r.runChildren(context.Background(), nil, RunChildrenInput{RunID: uuid.NewString()}); err == nil {
		t.Error("expected error when workflow is empty")
	}
	if _, _, err := r.runChildren(context.Background(), nil, RunChildrenInput{RunID: "not-a-uuid", Workflow: "wf"}); err == nil {
		t.Error("expected error for a non-UUID run_id")
	}
}

// --- client decode: a corrupt plan_decomposed payload fails loud ---

func TestLatestPlanDecomposed_CorruptPayloadErrors(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent := uuid.New()
	fb.mu.Lock()
	fb.perRunAuditByRun[parent] = []AuditEntry{{
		ID: uuid.NewString(), Sequence: 1, RunID: parent.String(), Category: "plan_decomposed",
		// child_run_ids must be a []string; a string here forces a decode error.
		Payload: map[string]any{"child_run_ids": "not-a-list"},
	}}
	fb.mu.Unlock()
	if _, err := r.api.LatestPlanDecomposed(context.Background(), parent); err == nil {
		t.Fatal("expected a decode error for a corrupt plan_decomposed payload")
	}
}

// --- topological-wave dispatch (#1278 slice B) ---

// argvFlag returns the value following flag in argv, or "" if absent.
func argvFlag(argv []string, flag string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag {
			return argv[i+1]
		}
	}
	return ""
}

func intPtr(i int) *int { return &i }

// captureBaseSpawn returns a fake spawn that records each child's --base-branch
// keyed by --run-id, plus the recording map and its mutex. outcomeFor lets a
// test fail a specific child (return "" for the default "ok"/exit 0).
func captureBaseSpawn(outcomeFor func(runID string) (string, int)) (
	func(context.Context, string, []string, []string, *mcp.CallToolRequest, any) ([]RunnerEvent, []string, int, error),
	map[string]string, *sync.Mutex,
) {
	var mu sync.Mutex
	baseByID := map[string]string{}
	fn := func(_ context.Context, _ string, argv, _ []string, _ *mcp.CallToolRequest, _ any) ([]RunnerEvent, []string, int, error) {
		runID := argvFlag(argv, "--run-id")
		mu.Lock()
		baseByID[runID] = argvFlag(argv, "--base-branch")
		mu.Unlock()
		outcome, exit := "ok", 0
		if outcomeFor != nil {
			if o, e := outcomeFor(runID); o != "" {
				outcome, exit = o, e
			}
		}
		return completedEvents(outcome), nil, exit, nil
	}
	return fn, baseByID, &mu
}

// TestRunChildren_TwoWaveDispatch is the binding 2-wave test (verification mode
// 8): wave 0 dispatches against main, integrate-wave is called ONCE, and wave 1
// dispatches against the consolidated branch integrate-wave returned — NOT main.
func TestRunChildren_TwoWaveDispatch(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	child0, child1 := uuid.New(), uuid.New()
	seedChildRun(fb, child0, "pending")
	seedChildRun(fb, child1, "pending")
	seedPlanDecomposedWaves(fb, parent, []string{child0.String(), child1.String()}, 0, [][]int{{0}, {1}})

	spawn, baseByID, mu := captureBaseSpawn(nil)
	withFakeSpawn(t, spawn)

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != 2 {
		t.Errorf("dispatched_count = %d, want 2 (one per wave)", out.DispatchedCount)
	}

	wantConsolidated := "fishhawk/run-" + parent.String()[:8] + "-consolidated"
	mu.Lock()
	defer mu.Unlock()
	if got := baseByID[child0.String()]; got != "main" {
		t.Errorf("wave-0 child --base-branch = %q, want main", got)
	}
	if got := baseByID[child1.String()]; got != wantConsolidated {
		t.Errorf("wave-1 child --base-branch = %q, want the consolidated branch %q", got, wantConsolidated)
	}
	fb.mu.Lock()
	calls := fb.integrateWaveCalledBy[parent]
	fb.mu.Unlock()
	if calls != 1 {
		t.Errorf("integrate-wave calls = %d, want exactly 1 (between the two waves)", calls)
	}
}

// TestRunChildren_NoDependsOn_SingleWaveNeverIntegrates is the binding back-
// compat test (condition #3 / verification mode 9): a no-depends_on
// decomposition (no waves field) collapses to ONE concurrent wave dispatched
// with --base-branch main, and integrate-wave is NEVER called (counter == 0).
func TestRunChildren_NoDependsOn_SingleWaveNeverIntegrates(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	const n = 3
	childIDs := make([]string, n)
	for i := range childIDs {
		c := uuid.New()
		childIDs[i] = c.String()
		seedChildRun(fb, c, "pending")
	}
	seedPlanDecomposed(fb, parent, childIDs, 0) // NO waves field — back-compat

	spawn, baseByID, mu := captureBaseSpawn(nil)
	withFakeSpawn(t, spawn)

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != n {
		t.Errorf("dispatched_count = %d, want %d (one concurrent wave)", out.DispatchedCount, n)
	}
	mu.Lock()
	for _, id := range childIDs {
		if got := baseByID[id]; got != "main" {
			t.Errorf("child %s --base-branch = %q, want main", id, got)
		}
	}
	mu.Unlock()
	fb.mu.Lock()
	calls := fb.integrateWaveCalledBy[parent]
	fb.mu.Unlock()
	if calls != 0 {
		t.Errorf("integrate-wave calls = %d, want 0 (a single wave is never integrate-waved)", calls)
	}
}

// TestRunChildren_WaveFailureStopsBeforeNextWave is the partial-wave guard
// (verification mode 10): a wave-0 child that did not succeed STOPS the loop —
// integrate-wave is not called and wave 1's child is never dispatched.
func TestRunChildren_WaveFailureStopsBeforeNextWave(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	child0, child1 := uuid.New(), uuid.New()
	seedChildRun(fb, child0, "pending")
	seedChildRun(fb, child1, "pending")
	seedPlanDecomposedWaves(fb, parent, []string{child0.String(), child1.String()}, 0, [][]int{{0}, {1}})

	spawn, baseByID, mu := captureBaseSpawn(func(runID string) (string, int) {
		if runID == child0.String() {
			return "failed", 7 // wave-0 child fails
		}
		return "ok", 0
	})
	withFakeSpawn(t, spawn)

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	// Only wave 0's child was dispatched; wave 1 never ran.
	if out.DispatchedCount != 1 {
		t.Errorf("dispatched_count = %d, want 1 (wave 1 never dispatched)", out.DispatchedCount)
	}
	mu.Lock()
	_, child1Ran := baseByID[child1.String()]
	mu.Unlock()
	if child1Ran {
		t.Error("wave-1 child was dispatched despite a wave-0 failure; the partial-wave guard did not stop the loop")
	}
	fb.mu.Lock()
	calls := fb.integrateWaveCalledBy[parent]
	fb.mu.Unlock()
	if calls != 0 {
		t.Errorf("integrate-wave calls = %d, want 0 (no integration of a partial wave)", calls)
	}
	if !containsWarning(out.Warnings, "did not succeed") {
		t.Errorf("warnings = %v, want one mentioning the failed wave", out.Warnings)
	}
}

// TestRunChildren_SliceConflictStopsLoop (verification mode 11): a slice_conflict
// returned by integrate-wave stops the loop — wave 1 is not dispatched — and the
// conflict is surfaced as a warning.
func TestRunChildren_SliceConflictStopsLoop(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	child0, child1 := uuid.New(), uuid.New()
	seedChildRun(fb, child0, "pending")
	seedChildRun(fb, child1, "pending")
	seedPlanDecomposedWaves(fb, parent, []string{child0.String(), child1.String()}, 0, [][]int{{0}, {1}})

	fb.mu.Lock()
	fb.integrateWaveResp[parent] = IntegrateWaveResult{
		RunID:                 parent.String(),
		Outcome:               "slice_conflict",
		ConflictingSliceIndex: intPtr(0),
		ConflictingChildRunID: child0.String(),
		Detail:                "slice integration conflict: slice 0",
	}
	fb.mu.Unlock()

	spawn, baseByID, mu := captureBaseSpawn(nil)
	withFakeSpawn(t, spawn)

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != 1 {
		t.Errorf("dispatched_count = %d, want 1 (wave 1 not dispatched after a conflict)", out.DispatchedCount)
	}
	mu.Lock()
	_, child1Ran := baseByID[child1.String()]
	mu.Unlock()
	if child1Ran {
		t.Error("wave-1 child dispatched despite a slice_conflict; the loop did not stop")
	}
	if !containsWarning(out.Warnings, "slice conflict") {
		t.Errorf("warnings = %v, want one mentioning the slice conflict", out.Warnings)
	}
}

// TestRunChildren_IntegrateWaveTransportErrorStopsLoop (verification mode 11b):
// a transport error from integrate-wave (the apiClient.IntegrateWave ierr != nil
// branch, distinct from the slice_conflict decoded-outcome branch) stops the loop
// — wave 1 is not dispatched — and the transport error is surfaced as a warning
// rather than hard-failing runChildren. Setting the fakeBackend's integrateWaveStatus
// to 502 routes through the production client.go do/doWithStatus *apiError path, so
// the real IntegrateWave error surface is exercised rather than a mocked error.
func TestRunChildren_IntegrateWaveTransportErrorStopsLoop(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	child0, child1 := uuid.New(), uuid.New()
	seedChildRun(fb, child0, "pending")
	seedChildRun(fb, child1, "pending")
	seedPlanDecomposedWaves(fb, parent, []string{child0.String(), child1.String()}, 0, [][]int{{0}, {1}})

	// Wrap the fake with an interceptor that answers integrate-wave with a 502
	// carrying a proper error envelope (details.error = the real cause) so
	// apiClient.IntegrateWave returns an *apiError whose Details map is
	// populated (ierr != nil). Every other route reverse-proxies to the fake
	// unchanged. This exercises the #1548 apiError.Error() Details rendering:
	// the between-wave warning must surface the cause, not just an opaque
	// HTTP-status stop.
	const cause = "consolidated branch fetch failed: connection reset"
	fakeURL, perr := url.Parse(srv.URL)
	if perr != nil {
		t.Fatalf("parse fake url: %v", perr)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/integrate-wave", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    "slice_integration_error",
				"message": "integrate-wave failed",
				"details": map[string]any{"error": cause},
			},
		})
	})
	mux.Handle("/", httputil.NewSingleHostReverseProxy(fakeURL))
	wrapped := httptest.NewServer(mux)
	defer wrapped.Close()
	// Point the resolver's client at the wrapper; seeds still land on the fake
	// via the proxy.
	r.api = newAPIClient(config{backendURL: wrapped.URL, apiToken: "tok-test"})

	spawn, baseByID, mu := captureBaseSpawn(nil)
	withFakeSpawn(t, spawn)

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	if err != nil {
		t.Fatalf("runChildren: %v (transport error must surface as a warning, not a hard error)", err)
	}
	if out.DispatchedCount != 1 {
		t.Errorf("dispatched_count = %d, want 1 (wave 1 not dispatched after a transport error)", out.DispatchedCount)
	}
	mu.Lock()
	_, child1Ran := baseByID[child1.String()]
	mu.Unlock()
	if child1Ran {
		t.Error("wave-1 child dispatched despite an integrate-wave transport error; the loop did not stop")
	}
	if !containsWarning(out.Warnings, "integrate-wave after wave") {
		t.Errorf("warnings = %v, want one mentioning the failed integrate-wave", out.Warnings)
	}
	if !containsWarning(out.Warnings, "stopping before the next wave") {
		t.Errorf("warnings = %v, want the transport-error branch wording (distinct from slice conflict)", out.Warnings)
	}
	// #1548: the warning now surfaces the real cause from the error envelope's
	// details map (rendered by apiError.Error()), not an opaque HTTP-status stop.
	if !containsWarning(out.Warnings, cause) {
		t.Errorf("warnings = %v, want the details.error cause %q surfaced via apiError.Error()", out.Warnings, cause)
	}
}

// TestRunChildren_EmptyConsolidatedBranchKeepsBase (verification mode 12): an
// empty consolidated_branch from integrate-wave (the GitHub-not-wired graceful
// skip) leaves the next wave's --base-branch unchanged (main) and warns rather
// than dispatching against an empty ref.
func TestRunChildren_EmptyConsolidatedBranchKeepsBase(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	child0, child1 := uuid.New(), uuid.New()
	seedChildRun(fb, child0, "pending")
	seedChildRun(fb, child1, "pending")
	seedPlanDecomposedWaves(fb, parent, []string{child0.String(), child1.String()}, 0, [][]int{{0}, {1}})

	fb.mu.Lock()
	fb.integrateWaveResp[parent] = IntegrateWaveResult{
		RunID:              parent.String(),
		Outcome:            "integrated",
		ConsolidatedBranch: "", // graceful-skip: GitHub not wired
	}
	fb.mu.Unlock()

	spawn, baseByID, mu := captureBaseSpawn(nil)
	withFakeSpawn(t, spawn)

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != 2 {
		t.Errorf("dispatched_count = %d, want 2 (both waves still dispatch)", out.DispatchedCount)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := baseByID[child1.String()]; got != "main" {
		t.Errorf("wave-1 child --base-branch = %q, want main unchanged (empty consolidated_branch must not blank the base)", got)
	}
	if !containsWarning(out.Warnings, "empty consolidated_branch") {
		t.Errorf("warnings = %v, want one mentioning the empty consolidated_branch", out.Warnings)
	}
}

// TestRunChildren_LegacyDispatchedZeroDispatchWarns is the #1980 named test: a
// post-#1912 decomposed parent whose children sit in legacy 'dispatched' with NO
// runner ever spawned yields dispatched_count=0, ZERO spawn-seam invocations, and
// a LOUD top-level warning naming sequential per-child fishhawk_dispatch_stage —
// never a SILENT zero-dispatch success.
func TestRunChildren_LegacyDispatchedZeroDispatchWarns(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	child0, child1 := uuid.New(), uuid.New()
	// The legacy pre-#1980 park signature: run advanced to 'running', implement
	// stage flipped to 'dispatched' with no spawn attempt behind it.
	seedChildRunStage(fb, child0, "running", "dispatched")
	seedChildRunStage(fb, child1, "running", "dispatched")
	seedPlanDecomposed(fb, parent, []string{child0.String(), child1.String()}, 0)

	var spawns int32
	withFakeSpawn(t, func(_ context.Context, _ string, _, _ []string, _ *mcp.CallToolRequest, _ any) ([]RunnerEvent, []string, int, error) {
		atomic.AddInt32(&spawns, 1)
		return completedEvents("ok"), nil, 0, nil
	})

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != 0 {
		t.Errorf("dispatched_count = %d, want 0 (all children in-flight/legacy-parked)", out.DispatchedCount)
	}
	if got := atomic.LoadInt32(&spawns); got != 0 {
		t.Errorf("spawn seam invoked %d times, want 0", got)
	}
	if !containsWarning(out.Warnings, "fishhawk_dispatch_stage") {
		t.Errorf("warnings = %v, want one naming fishhawk_dispatch_stage recovery", out.Warnings)
	}
	if !containsWarning(out.Warnings, "SEQUENTIALLY") {
		t.Errorf("warnings = %v, want one naming SEQUENTIAL recovery", out.Warnings)
	}
	// Both stuck children must be named so the operator knows what to recover.
	if !containsWarning(out.Warnings, child0.String()) || !containsWarning(out.Warnings, child1.String()) {
		t.Errorf("warnings = %v, want both stuck child ids named", out.Warnings)
	}
}

// TestRunChildren_WaveIntegrityStuckWaveStopsBeforeIntegrate is the #1980
// wave-integrity guard: a two-wave decomposition whose wave-0 child sits in
// legacy 'dispatched' (non-attempted, not 'succeeded') STOPS before
// integrate-wave — IntegrateWave is NEVER called, wave 1 is not dispatched, no
// bogus empty-consolidated-branch warning is emitted, and the stop warning names
// the stuck child.
func TestRunChildren_WaveIntegrityStuckWaveStopsBeforeIntegrate(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	child0, child1 := uuid.New(), uuid.New()
	seedChildRunStage(fb, child0, "running", "dispatched") // wave-0 stuck legacy park
	seedChildRun(fb, child1, "pending")                    // wave-1 pending
	seedPlanDecomposedWaves(fb, parent, []string{child0.String(), child1.String()}, 0, [][]int{{0}, {1}})

	spawn, baseByID, mu := captureBaseSpawn(nil)
	withFakeSpawn(t, spawn)

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != 0 {
		t.Errorf("dispatched_count = %d, want 0 (wave 0 stuck, wave 1 never reached)", out.DispatchedCount)
	}
	mu.Lock()
	_, child1Ran := baseByID[child1.String()]
	mu.Unlock()
	if child1Ran {
		t.Error("wave-1 child dispatched despite a stuck wave 0; the wave-integrity guard did not stop the loop")
	}
	fb.mu.Lock()
	calls := fb.integrateWaveCalledBy[parent]
	fb.mu.Unlock()
	if calls != 0 {
		t.Errorf("integrate-wave calls = %d, want 0 (no integration of a stuck wave)", calls)
	}
	if containsWarning(out.Warnings, "empty consolidated_branch") {
		t.Errorf("emitted the bogus empty-consolidated-branch warning: %v", out.Warnings)
	}
	if !containsWarning(out.Warnings, child0.String()) {
		t.Errorf("warnings = %v, want the stuck wave-0 child named", out.Warnings)
	}
}

// TestRunChildren_WaveIntegrityUnknownStateBlocks pins the #1980 concern
// (9dfc3bea from #1980's review): a non-attempted wave-0 child whose implement
// stage could not even be DISCOVERED (resolveStage fails — the run_children.go
// discovery-failure path, stateKnown=false) must be treated as the same
// fail-safe as a known-non-succeeded state: the wave-integrity guard blocks
// before integrating, reporting the child's stage_state as "unknown" (never a
// bare empty string, and never silently passed as if it were terminal-ok).
func TestRunChildren_WaveIntegrityUnknownStateBlocks(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	child0, child1 := uuid.New(), uuid.New()
	// child0 is deliberately NOT seeded at all: ListRunStages returns an empty
	// list for it, so resolveStage fails to find an "implement" stage and the
	// partition loop leaves it with stateKnown=false (discovery failure, not a
	// known non-succeeded state).
	seedChildRun(fb, child1, "pending") // wave-1 pending
	seedPlanDecomposedWaves(fb, parent, []string{child0.String(), child1.String()}, 0, [][]int{{0}, {1}})

	spawn, baseByID, mu := captureBaseSpawn(nil)
	withFakeSpawn(t, spawn)

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != 0 {
		t.Errorf("dispatched_count = %d, want 0 (wave 0's discovery-failed child blocks, wave 1 never reached)", out.DispatchedCount)
	}
	mu.Lock()
	_, child1Ran := baseByID[child1.String()]
	mu.Unlock()
	if child1Ran {
		t.Error("wave-1 child dispatched despite an unknown-state wave-0 child; the wave-integrity guard did not stop the loop")
	}
	fb.mu.Lock()
	calls := fb.integrateWaveCalledBy[parent]
	fb.mu.Unlock()
	if calls != 0 {
		t.Errorf("integrate-wave calls = %d, want 0 (never integrate over an unknown predecessor state)", calls)
	}
	if !containsWarning(out.Warnings, child0.String()) {
		t.Errorf("warnings = %v, want the discovery-failed wave-0 child named", out.Warnings)
	}
	if !containsWarning(out.Warnings, `stage_state "unknown"`) {
		t.Errorf(`warnings = %v, want stage_state "unknown" reported for the discovery-failed child`, out.Warnings)
	}
}

// TestRunChildren_WaveIntegrityIdempotentReinvocation pins that the wave-integrity
// guard does NOT break legitimate idempotent re-invocation: a wave-0 child already
// terminal-'succeeded' (non-attempted this call) passes the guard, so the wave
// integrates and wave 1's pending child dispatches on the consolidated branch.
func TestRunChildren_WaveIntegrityIdempotentReinvocation(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	child0, child1 := uuid.New(), uuid.New()
	seedChildRun(fb, child0, "succeeded") // wave-0 already terminal-ok, not attempted
	seedChildRun(fb, child1, "pending")   // wave-1 pending
	seedPlanDecomposedWaves(fb, parent, []string{child0.String(), child1.String()}, 0, [][]int{{0}, {1}})

	spawn, baseByID, mu := captureBaseSpawn(nil)
	withFakeSpawn(t, spawn)

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != 1 {
		t.Errorf("dispatched_count = %d, want 1 (only the wave-1 pending child)", out.DispatchedCount)
	}
	fb.mu.Lock()
	calls := fb.integrateWaveCalledBy[parent]
	fb.mu.Unlock()
	if calls != 1 {
		t.Errorf("integrate-wave calls = %d, want 1 (wave 0 succeeded → integrate → wave 1)", calls)
	}
	wantConsolidated := "fishhawk/run-" + parent.String()[:8] + "-consolidated"
	mu.Lock()
	got := baseByID[child1.String()]
	mu.Unlock()
	if got != wantConsolidated {
		t.Errorf("wave-1 child --base-branch = %q, want the consolidated branch %q", got, wantConsolidated)
	}
}

// TestRunChildren_WaveIndexOutOfRange asserts a waves index that does not address
// a child is a loud tool error, never a silent skip.
func TestRunChildren_WaveIndexOutOfRange(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	child0 := uuid.New()
	seedChildRun(fb, child0, "pending")
	// waves references index 1 but there is only one child (index 0).
	seedPlanDecomposedWaves(fb, parent, []string{child0.String()}, 0, [][]int{{0}, {1}})

	withFakeSpawn(t, func(_ context.Context, _ string, _, _ []string, _ *mcp.CallToolRequest, _ any) ([]RunnerEvent, []string, int, error) {
		return completedEvents("ok"), nil, 0, nil
	})

	_, _, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	if err == nil {
		t.Fatal("expected a tool error for an out-of-range waves index")
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Errorf("error = %v, want one mentioning an out-of-range waves index", err)
	}
}

// containsWarning reports whether any warning contains substr.
func containsWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// --- cross-boundary integration: real fishhawk-runner subprocesses ---

// gitRunT runs a git command in dir, failing the test on error.
func gitRunT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// TestRunChildren_CrossBoundary_SpawnsRealRunners is the binding cross-boundary
// proof (verification mode k): it drives the REAL fishhawk_run_children tool so
// it spawns ACTUAL fishhawk-runner subprocesses (the real spawnRunnerStage seam,
// not the in-package fake) for a decomposed parent's two pending children, and
// asserts each child provisioned a DISTINCT per-child worktree directory under
// the worktrees root — proving --parallel-isolate genuinely flows MCP → runner
// → git-worktree. A shared-tree (parallel-isolate NOT flowing) would yield ONE
// run-<parent> worktree instead of two run-<child> worktrees. The operator's
// tracked tree stays untouched throughout.
//
// The children's agent invocation is forced to fail fast (a fake `claude` that
// exits non-zero, prepended to PATH) — but the worktree is provisioned BEFORE
// the agent runs, so the per-child worktree assertion holds regardless, and
// run_children records each child's non-zero exit as data (await-all).
func TestRunChildren_CrossBoundary_SpawnsRealRunners(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the fishhawk-runner binary and spawns real subprocesses")
	}
	for _, tool := range []string{"go", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}

	// (1) Build the real fishhawk-runner from the runner module.
	_, thisFile, _, _ := runtime.Caller(0)
	runnerDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "runner", "cmd", "fishhawk-runner")
	runnerBin := filepath.Join(t.TempDir(), "fishhawk-runner")
	build := exec.Command("go", "build", "-o", runnerBin, ".")
	build.Dir = runnerDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fishhawk-runner: %v\n%s", err, out)
	}

	// (2) A fake `claude` that exits non-zero gives a deterministic, fast agent
	// failure AFTER the runner provisions its worktree — independent of whether
	// a real claude is installed on the host.
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "claude"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	// (3) Operator git repo with a seed commit (worktree add --detach HEAD
	// needs a commit).
	repo := t.TempDir()
	gitRunT(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunT(t, repo, "add", "-A")
	gitRunT(t, repo, "commit", "-q", "-m", "seed")

	// (4) Backend that serves BOTH the MCP-side discovery calls and the
	// runner-subprocess calls (signing-key, prompt, trace).
	parent := uuid.New()
	child1, child2 := uuid.New(), uuid.New()
	stage1, stage2 := uuid.New(), uuid.New()
	stageByChild := map[string]string{
		child1.String(): stage1.String(),
		child2.String(): stage2.String(),
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}

	mux := http.NewServeMux()
	// Runner: per-run signing key (multi-call; one keypair reused).
	mux.HandleFunc("POST /v0/runs/{run_id}/signing-key", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{
			"run_id":      r.PathValue("run_id"),
			"public_key":  base64.StdEncoding.EncodeToString(pub),
			"private_key": base64.StdEncoding.EncodeToString(priv),
			"issued_at":   time.Now().UTC(),
			"expires_at":  time.Now().Add(time.Hour).UTC(),
		})
	})
	// Runner: prompt fetch. decomposed_from_run_id=parent makes the worktree
	// key on the CHILD id under --parallel-isolate.
	mux.HandleFunc("GET /v0/stages/{stage_id}/prompt", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"stage_type":             "implement",
			"prompt":                 "do the slice work",
			"prompt_hash":            "sha256:test",
			"decomposed_from_run_id": parent.String(),
		})
	})
	// Runner: trace upload (terminal) — accept so the runner doesn't retry.
	mux.HandleFunc("POST /v0/runs/{run_id}/trace", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"run_id": r.PathValue("run_id"), "stage_id": "", "variant": "redacted", "content_hash": "x",
		})
	})
	// MCP discovery + runner lineage read: GET run (no lineage_complete → the
	// sibling's sweep never reclaims a live child worktree).
	mux.HandleFunc("GET /v0/runs/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, Run{ID: r.PathValue("run_id"), State: "pending", Repo: "x/y"})
	})
	// MCP discovery: child stages (resolveStageID matches the implement stage).
	mux.HandleFunc("GET /v0/runs/{run_id}/stages", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("run_id")
		sid, ok := stageByChild[id]
		if !ok {
			writeJSON(w, http.StatusOK, listStagesResult{})
			return
		}
		writeJSON(w, http.StatusOK, listStagesResult{Items: []Stage{
			{ID: sid, RunID: id, Type: "implement", State: "pending"},
		}})
	})
	// MCP discovery: parent's plan_decomposed audit entry.
	mux.HandleFunc("GET /v0/runs/{run_id}/audit", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("category") != "plan_decomposed" {
			writeJSON(w, http.StatusOK, listAuditResult{})
			return
		}
		writeJSON(w, http.StatusOK, listAuditResult{Items: []AuditEntry{{
			ID: uuid.NewString(), Sequence: 1, RunID: r.PathValue("run_id"), Category: "plan_decomposed",
			Payload: map[string]any{
				"child_run_ids":          []string{child1.String(), child2.String()},
				"effective_max_parallel": 2,
			},
		}}})
	})
	// MCP host-dispatch marker (#1912): the real run_children marks each child's
	// host spawn and spawns ONLY on transitioned:true, so the marker must report
	// it won the CAS or the real runner is (correctly) never spawned.
	mux.HandleFunc("POST /v0/runs/{run_id}/stages/{stage_id}/host-dispatch", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, HostDispatchResult{Transitioned: true, StageState: "dispatched"})
	})
	// Permissive fallback for any other runner call (best-effort surfaces).
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// (5) Drive the REAL tool. RunnerBinary points at the freshly built binary;
	// the spawn seam is left at its default (real spawnRunnerStage).
	r := &runResolver{
		api:    newAPIClient(config{backendURL: srv.URL, apiToken: "tok"}),
		getenv: func(string) string { return "" },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, out, err := r.runChildren(ctx, nil, RunChildrenInput{
		RunID:        parent.String(),
		Workflow:     "wf",
		WorkingDir:   repo,
		GitHubRepo:   "x/y",
		RunnerBinary: runnerBin,
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != 2 {
		t.Fatalf("dispatched_count = %d, want 2", out.DispatchedCount)
	}

	// (6) Each child provisioned a DISTINCT per-child worktree (run-<child[:8]>),
	// NOT a shared run-<parent> tree.
	wtRoot := filepath.Join(repo, ".git", "fishhawk-worktrees")
	entries, err := os.ReadDir(wtRoot)
	if err != nil {
		t.Fatalf("read worktrees root %s: %v", wtRoot, err)
	}
	worktrees := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "run-") {
			worktrees[e.Name()] = true
		}
	}
	want1 := "run-" + child1.String()[:8]
	want2 := "run-" + child2.String()[:8]
	if !worktrees[want1] || !worktrees[want2] {
		t.Errorf("per-child worktrees missing: have %v, want %s and %s", worktrees, want1, want2)
	}
	if want1 == want2 {
		t.Fatal("children share a short id; pick distinct UUIDs")
	}
	if len(worktrees) != 2 {
		t.Errorf("worktree count = %d (%v), want exactly 2 distinct per-child dirs", len(worktrees), worktrees)
	}

	// The operator's tracked tree stayed clean — worktrees live under .git.
	statusOut, err := exec.Command("git", "-C", repo, "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if strings.TrimSpace(string(statusOut)) != "" {
		t.Errorf("operator tree not clean after parallel children:\n%s", statusOut)
	}
}

// TestRunChildren_CrossBoundary_TwoWaveStopsOnRealChildFailure is the 2-wave
// dependency-ordered cross-boundary proof through REAL fishhawk-runner
// subprocesses (#1278 slice B). It seeds a depends_on decomposition whose
// plan_decomposed waves are [[0],[1]] — slice 1 depends on slice 0 — and drives
// the real run_children wave loop. Wave 0's child spawns an actual runner
// subprocess (its per-child worktree IS provisioned, proving the wave loop flows
// MCP → runner → git-worktree) but its agent fails (the fake `claude` exits
// non-zero), so the PARTIAL-WAVE GUARD stops the loop BEFORE integrate-wave is
// called and BEFORE wave 1 dispatches — wave 1's child gets no worktree.
//
// NOTE ON SCOPE: the literal "wave 1 sees wave 0's MERGED symbol → non-empty
// compiling commit" half of the cross-boundary requirement is INFEASIBLE in a
// hermetic test. The runner hard-codes the GitHub HTTPS remote for both the
// child slice-branch push and the wave-N base fetch (main.go ~4673), and a
// decomposed child that produces no changes reports `failed` category C (main.go
// ~4846) — so a decomposed child can NEVER reach an `ok` outcome locally without
// a real GitHub, which means wave 0 can never succeed and wave 1's merged-base
// dispatch is unobservable through real subprocesses. The merged-base
// visibility only exists in the dogfood/prod posture where GitHub is wired. The
// base-PASSING contract (wave 1 dispatched with --base-branch == the
// consolidated branch) is fully and deterministically proven by the unit wave
// tests (TestRunChildren_TwoWaveDispatch et al.); this test proves the
// wave-ordering + real-runner spawn + partial-wave guard end-to-end.
func TestRunChildren_CrossBoundary_TwoWaveStopsOnRealChildFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the fishhawk-runner binary and spawns real subprocesses")
	}
	for _, tool := range []string{"go", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}

	// (1) Build the real fishhawk-runner.
	_, thisFile, _, _ := runtime.Caller(0)
	runnerDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "runner", "cmd", "fishhawk-runner")
	runnerBin := filepath.Join(t.TempDir(), "fishhawk-runner")
	build := exec.Command("go", "build", "-o", runnerBin, ".")
	build.Dir = runnerDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fishhawk-runner: %v\n%s", err, out)
	}

	// (2) A fake `claude` that exits non-zero: a deterministic agent failure
	// AFTER the runner provisions wave 0's worktree.
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "claude"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	// (3) Operator git repo with a seed commit.
	repo := t.TempDir()
	gitRunT(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunT(t, repo, "add", "-A")
	gitRunT(t, repo, "commit", "-q", "-m", "seed")

	parent := uuid.New()
	child0, child1 := uuid.New(), uuid.New()
	stage0, stage1 := uuid.New(), uuid.New()
	stageByChild := map[string]string{
		child0.String(): stage0.String(),
		child1.String(): stage1.String(),
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}

	var integrateWaveCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/signing-key", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{
			"run_id":      r.PathValue("run_id"),
			"public_key":  base64.StdEncoding.EncodeToString(pub),
			"private_key": base64.StdEncoding.EncodeToString(priv),
			"issued_at":   time.Now().UTC(),
			"expires_at":  time.Now().Add(time.Hour).UTC(),
		})
	})
	mux.HandleFunc("GET /v0/stages/{stage_id}/prompt", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"stage_type":             "implement",
			"prompt":                 "do the slice work",
			"prompt_hash":            "sha256:test",
			"decomposed_from_run_id": parent.String(),
		})
	})
	mux.HandleFunc("POST /v0/runs/{run_id}/trace", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"run_id": r.PathValue("run_id"), "stage_id": "", "variant": "redacted", "content_hash": "x",
		})
	})
	mux.HandleFunc("GET /v0/runs/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, Run{ID: r.PathValue("run_id"), State: "pending", Repo: "x/y"})
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/stages", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("run_id")
		sid, ok := stageByChild[id]
		if !ok {
			writeJSON(w, http.StatusOK, listStagesResult{})
			return
		}
		writeJSON(w, http.StatusOK, listStagesResult{Items: []Stage{
			{ID: sid, RunID: id, Type: "implement", State: "pending"},
		}})
	})
	// plan_decomposed carries the topological waves [[0],[1]] — slice 1 depends
	// on slice 0.
	mux.HandleFunc("GET /v0/runs/{run_id}/audit", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("category") != "plan_decomposed" {
			writeJSON(w, http.StatusOK, listAuditResult{})
			return
		}
		writeJSON(w, http.StatusOK, listAuditResult{Items: []AuditEntry{{
			ID: uuid.NewString(), Sequence: 1, RunID: r.PathValue("run_id"), Category: "plan_decomposed",
			Payload: map[string]any{
				"child_run_ids":          []string{child0.String(), child1.String()},
				"effective_max_parallel": 2,
				"waves":                  [][]int{{0}, {1}},
			},
		}}})
	})
	// integrate-wave: count calls. The partial-wave guard must ensure this is
	// NEVER reached (wave 0's child failed).
	mux.HandleFunc("POST /v0/runs/{run_id}/integrate-wave", func(w http.ResponseWriter, r *http.Request) {
		integrateWaveCalls.Add(1)
		writeJSON(w, http.StatusOK, IntegrateWaveResult{RunID: r.PathValue("run_id"), Outcome: "integrated"})
	})
	// MCP host-dispatch marker (#1912): the real run_children marks each child's
	// host spawn and spawns ONLY on transitioned:true, so the marker must report
	// it won the CAS or the real runner is (correctly) never spawned.
	mux.HandleFunc("POST /v0/runs/{run_id}/stages/{stage_id}/host-dispatch", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, HostDispatchResult{Transitioned: true, StageState: "dispatched"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := &runResolver{
		api:    newAPIClient(config{backendURL: srv.URL, apiToken: "tok"}),
		getenv: func(string) string { return "" },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, out, err := r.runChildren(ctx, nil, RunChildrenInput{
		RunID:        parent.String(),
		Workflow:     "wf",
		WorkingDir:   repo,
		GitHubRepo:   "x/y",
		RunnerBinary: runnerBin,
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}

	// Only wave 0's child was dispatched; the partial-wave guard stopped before
	// wave 1.
	if out.DispatchedCount != 1 {
		t.Errorf("dispatched_count = %d, want 1 (wave 1 never dispatched after wave 0's child failed)", out.DispatchedCount)
	}
	if got := integrateWaveCalls.Load(); got != 0 {
		t.Errorf("integrate-wave calls = %d, want 0 (no integration of a failed wave)", got)
	}

	// Wave 0's child spawned a REAL runner (its per-child worktree exists);
	// wave 1's child did NOT (the guard stopped the loop).
	wtRoot := filepath.Join(repo, ".git", "fishhawk-worktrees")
	worktrees := map[string]bool{}
	if entries, derr := os.ReadDir(wtRoot); derr == nil {
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "run-") {
				worktrees[e.Name()] = true
			}
		}
	}
	wave0WT := "run-" + child0.String()[:8]
	wave1WT := "run-" + child1.String()[:8]
	if !worktrees[wave0WT] {
		t.Errorf("wave-0 child worktree %s missing (real runner did not provision it): have %v", wave0WT, worktrees)
	}
	if worktrees[wave1WT] {
		t.Errorf("wave-1 child worktree %s present, but the guard should have stopped before dispatching wave 1: have %v", wave1WT, worktrees)
	}
}

// --- #1980 continuous cross-boundary: real orchestrator fanout → HTTP → run_children ---

// xbRepo is a minimal in-memory runmodel.Repository (embeds runmodel.BaseFake, overriding
// only the methods the orchestrator fanout + child-dispatch path exercises) for
// the #1980 continuous cross-boundary test. It is the PRODUCER-side store: a real
// orchestrator.Orchestrator mints and parks children into it, and the HTTP
// handlers below serve its state to run_children. State transitions are validated
// by runmodel.ValidStageTransition / runmodel.ValidRunTransition so the park is a genuine
// state-machine walk, not a hand-set field.
type xbRepo struct {
	runmodel.BaseFake
	mu     sync.Mutex
	runs   map[uuid.UUID]*runmodel.Run
	stages map[uuid.UUID][]*runmodel.Stage
}

func newXBRepo() *xbRepo {
	return &xbRepo{runs: map[uuid.UUID]*runmodel.Run{}, stages: map[uuid.UUID][]*runmodel.Stage{}}
}

func (r *xbRepo) CreateRun(_ context.Context, p runmodel.CreateRunParams) (*runmodel.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rr := &runmodel.Run{
		ID: uuid.New(), Repo: p.Repo, WorkflowID: p.WorkflowID, WorkflowSHA: p.WorkflowSHA,
		TriggerSource: p.TriggerSource, InstallationID: p.InstallationID,
		ParentRunID: p.ParentRunID, DecomposedFrom: p.DecomposedFrom, SliceIndex: p.SliceIndex,
		// The #1980 bug's root: CreateRunParams carries no RunnerKindResolved, so
		// every child row is created UNRESOLVED with RunnerKind copied from parent.
		RunnerKind: p.RunnerKind, WorkflowSpec: p.WorkflowSpec,
		State: runmodel.StatePending, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	r.runs[rr.ID] = rr
	return rr, nil
}

func (r *xbRepo) CreateStage(_ context.Context, p runmodel.CreateStageParams) (*runmodel.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := &runmodel.Stage{
		ID: uuid.New(), RunID: p.RunID, Sequence: p.Sequence, Type: p.Type,
		ExecutorKind: p.ExecutorKind, ExecutorRef: p.ExecutorRef, State: runmodel.StageStatePending,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	r.stages[p.RunID] = append(r.stages[p.RunID], st)
	return st, nil
}

func (r *xbRepo) GetRun(_ context.Context, id uuid.UUID) (*runmodel.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rr, ok := r.runs[id]; ok {
		return rr, nil
	}
	return nil, runmodel.ErrNotFound
}

func (r *xbRepo) ListStagesForRun(_ context.Context, id uuid.UUID) ([]*runmodel.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stages[id], nil
}

func (r *xbRepo) ListRuns(_ context.Context, f runmodel.ListRunsFilter) ([]*runmodel.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f.DecomposedFrom == nil {
		return nil, nil
	}
	var out []*runmodel.Run
	for _, rr := range r.runs {
		if rr.DecomposedFrom != nil && *rr.DecomposedFrom == *f.DecomposedFrom {
			out = append(out, rr)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := out[i].SliceIndex, out[j].SliceIndex
		switch {
		case si != nil && sj != nil && *si != *sj:
			return *si < *sj
		default:
			return out[i].ID.String() < out[j].ID.String()
		}
	})
	if f.Offset > 0 {
		if f.Offset >= len(out) {
			return nil, nil
		}
		out = out[f.Offset:]
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (r *xbRepo) TransitionRun(_ context.Context, id uuid.UUID, to runmodel.State) (*runmodel.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rr := r.runs[id]
	if rr == nil {
		return nil, runmodel.ErrNotFound
	}
	if !runmodel.ValidRunTransition(rr.State, to) {
		return nil, runmodel.InvalidTransitionError{Kind: "run", From: string(rr.State), To: string(to)}
	}
	rr.State = to
	return rr, nil
}

func (r *xbRepo) TransitionStage(_ context.Context, id uuid.UUID, to runmodel.StageState, _ *runmodel.StageCompletion) (*runmodel.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, list := range r.stages {
		for _, st := range list {
			if st.ID != id {
				continue
			}
			if !runmodel.ValidStageTransition(st.State, to) {
				return nil, runmodel.InvalidTransitionError{Kind: "stage", From: string(st.State), To: string(to)}
			}
			st.State = to
			return st, nil
		}
	}
	return nil, runmodel.ErrNotFound
}

// xbArtifacts serves ONE plan artifact for the parent's plan stage so the
// orchestrator's loadApprovedPlan resolves the decomposition.
type xbArtifacts struct {
	planStageID uuid.UUID
	plan        *artifact.Artifact
}

func (a *xbArtifacts) ListForStage(_ context.Context, stageID uuid.UUID) ([]*artifact.Artifact, error) {
	if stageID == a.planStageID {
		return []*artifact.Artifact{a.plan}, nil
	}
	return nil, nil
}
func (a *xbArtifacts) Create(context.Context, artifact.CreateParams) (*artifact.Artifact, error) {
	return nil, artifact.ErrNotFound
}
func (a *xbArtifacts) Get(context.Context, uuid.UUID) (*artifact.Artifact, error) {
	return nil, artifact.ErrNotFound
}
func (a *xbArtifacts) GetByHash(context.Context, uuid.UUID, string) (*artifact.Artifact, error) {
	return nil, artifact.ErrNotFound
}

// xbAudit captures the orchestrator's plan_decomposed AppendChained so the HTTP
// audit handler can serve it to run_children's discovery — the discovery entry
// is itself orchestrator-produced, not hand-built.
type xbAudit struct {
	audit.BaseFake
	mu       sync.Mutex
	appended []audit.ChainAppendParams
}

func (a *xbAudit) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.appended = append(a.appended, p)
	return &audit.Entry{ID: uuid.New()}, nil
}

func (a *xbAudit) planDecomposed(runID uuid.UUID) (json.RawMessage, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range a.appended {
		if p.RunID == runID && p.Category == "plan_decomposed" {
			return p.Payload, true
		}
	}
	return nil, false
}

// xbDecomposedPlanBytes builds a minimal standard_v1 plan carrying a
// no-depends_on decomposition — same shape the orchestrator fanout consumes.
func xbDecomposedPlanBytes(t *testing.T, titles []string) []byte {
	t.Helper()
	subs := make([]map[string]any, 0, len(titles))
	for _, title := range titles {
		subs = append(subs, map[string]any{
			"title":                        title,
			"scope_hint":                   "scope hint for " + title,
			"predicted_runtime_minutes":    10,
			"predicted_runtime_confidence": "high",
		})
	}
	body := map[string]any{
		"plan_version": "standard_v1",
		"ticket_reference": map[string]any{
			"type": "github_issue", "url": "https://github.com/example/repo/issues/1", "id": "example/repo#1",
		},
		"generated_by":                 map[string]any{"agent": "claude-code", "model": "claude-opus-4-7", "timestamp": time.Now().UTC().Format(time.RFC3339)},
		"summary":                      "xb plan with decomposition",
		"scope":                        map[string]any{"files": []map[string]any{{"path": "x.go", "operation": "create"}}},
		"approach":                     []map[string]any{{"step": 1, "description": "do it"}},
		"verification":                 map[string]any{"test_strategy": "run tests", "rollback_plan": "revert"},
		"predicted_runtime_minutes":    100,
		"predicted_runtime_confidence": "medium",
		"decomposition":                map[string]any{"rationale": "xb rationale", "sub_plans": subs},
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	return b
}

// TestRunChildren_CrossBoundary_OrchestratorFanoutToRunChildren is the binding
// approval-condition test (#1980): ONE continuous path in which the children are
// PRODUCED by the REAL orchestrator fanout — a decomposed, runner_kind-locked-
// local parent driven through Advance/fanoutIfDecomposed, NOT hand-seeded stage
// rows — and then CONSUMED by run_children across the REAL HTTP serialization
// boundary. It asserts the children arrive at awaiting_host_dispatch ON THE WIRE
// and that run_children dispatches them (nonzero dispatched_count, spawn seam
// invoked). This FAILS on pre-#1980 code at the exact seam the bug lived in: the
// fanout would park the children at legacy 'dispatched', the wire assertion would
// read 'dispatched', and run_children's dispatchable predicate would treat them
// as in-flight (dispatched_count=0).
func TestRunChildren_CrossBoundary_OrchestratorFanoutToRunChildren(t *testing.T) {
	repo := newXBRepo()

	// (1) Seed a decomposed parent LOCKED to runner_kind=local (the dogfood
	// shape): plan succeeded, implement pending, resolved-local.
	parent := &runmodel.Run{
		ID: uuid.New(), Repo: "example/repo", WorkflowID: "feature_change", WorkflowSHA: "sha",
		TriggerSource: runmodel.TriggerGitHubIssue, State: runmodel.StateRunning,
		RunnerKind: runmodel.RunnerKindLocal, RunnerKindResolved: true,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	planStage := &runmodel.Stage{
		ID: uuid.New(), RunID: parent.ID, Sequence: 0, Type: runmodel.StageTypePlan,
		ExecutorKind: runmodel.ExecutorAgent, ExecutorRef: "claude-code", State: runmodel.StageStateSucceeded,
	}
	implStage := &runmodel.Stage{
		ID: uuid.New(), RunID: parent.ID, Sequence: 1, Type: runmodel.StageTypeImplement,
		ExecutorKind: runmodel.ExecutorAgent, ExecutorRef: "claude-code", State: runmodel.StageStatePending,
	}
	repo.mu.Lock()
	repo.runs[parent.ID] = parent
	repo.stages[parent.ID] = []*runmodel.Stage{planStage, implStage}
	repo.mu.Unlock()

	schemaV := "standard_v1"
	arts := &xbArtifacts{
		planStageID: planStage.ID,
		plan: &artifact.Artifact{
			ID: uuid.New(), StageID: planStage.ID, Kind: artifact.KindPlan,
			SchemaVersion: &schemaV, Content: xbDecomposedPlanBytes(t, []string{"Part A", "Part B"}),
			CreatedAt: time.Now().UTC(),
		},
	}
	au := &xbAudit{}

	// (2) Run the REAL orchestrator fanout. This mints the children and, with the
	// #1980 fix, parks each child's implement stage at awaiting_host_dispatch.
	o := &orchestrator.Orchestrator{
		Runs: repo, Logger: slog.Default(), Artifacts: arts, Audit: au,
		DefaultRef: "main", MaxParallelChildren: 0,
	}
	out, err := o.Advance(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("orchestrator Advance (fanout): %v", err)
	}
	if out != orchestrator.OutcomeDecomposed {
		t.Fatalf("fanout outcome = %q, want decomposed", out)
	}
	pdPayload, ok := au.planDecomposed(parent.ID)
	if !ok {
		t.Fatal("orchestrator emitted no plan_decomposed entry")
	}

	// (3) HTTP boundary: serve the orchestrator-produced repo + audit state.
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/runs/{run_id}/audit", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("category") != "plan_decomposed" {
			writeJSON(w, http.StatusOK, listAuditResult{})
			return
		}
		id, _ := uuid.Parse(req.PathValue("run_id"))
		payload, found := au.planDecomposed(id)
		if !found {
			writeJSON(w, http.StatusOK, listAuditResult{})
			return
		}
		writeJSON(w, http.StatusOK, listAuditResult{Items: []AuditEntry{{
			ID: uuid.NewString(), Sequence: 1, RunID: id.String(), Category: "plan_decomposed",
			Payload: json.RawMessage(payload),
		}}})
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/stages", func(w http.ResponseWriter, req *http.Request) {
		id, _ := uuid.Parse(req.PathValue("run_id"))
		repo.mu.Lock()
		src := repo.stages[id]
		items := make([]Stage, 0, len(src))
		for _, s := range src {
			items = append(items, Stage{ID: s.ID.String(), RunID: s.RunID.String(), Sequence: s.Sequence, Type: string(s.Type), State: string(s.State)})
		}
		repo.mu.Unlock()
		writeJSON(w, http.StatusOK, listStagesResult{Items: items})
	})
	mux.HandleFunc("GET /v0/runs/{run_id}", func(w http.ResponseWriter, req *http.Request) {
		id, _ := uuid.Parse(req.PathValue("run_id"))
		if rr, gerr := repo.GetRun(req.Context(), id); gerr == nil {
			writeJSON(w, http.StatusOK, Run{ID: rr.ID.String(), State: string(rr.State), Repo: rr.Repo})
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]any{})
	})
	// Real host-dispatch CAS backed by the repo's state machine: flip a
	// pending|awaiting_host_dispatch stage to dispatched (transitioned:true).
	mux.HandleFunc("POST /v0/runs/{run_id}/stages/{stage_id}/host-dispatch", func(w http.ResponseWriter, req *http.Request) {
		sid, perr := uuid.Parse(req.PathValue("stage_id"))
		if perr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{})
			return
		}
		st, terr := repo.TransitionStage(req.Context(), sid, runmodel.StageStateDispatched, nil)
		if terr != nil {
			// A non-admissible state (already dispatched/terminal) is the
			// idempotent no-op; echo the current state.
			repo.mu.Lock()
			cur := ""
			for _, list := range repo.stages {
				for _, s := range list {
					if s.ID == sid {
						cur = string(s.State)
					}
				}
			}
			repo.mu.Unlock()
			writeJSON(w, http.StatusOK, HostDispatchResult{Transitioned: false, StageState: cur})
			return
		}
		writeJSON(w, http.StatusOK, HostDispatchResult{Transitioned: true, StageState: string(st.State)})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, map[string]any{}) })

	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := &runResolver{api: newAPIClient(config{backendURL: srv.URL, apiToken: "tok"}), getenv: func(string) string { return "" }}

	// (4) Decode the orchestrator-produced child ids and assert each child's
	// implement stage arrives at awaiting_host_dispatch ON THE WIRE — the
	// producer-side pin that fails on pre-#1980 code.
	var pd struct {
		ChildRunIDs []string `json:"child_run_ids"`
	}
	if err := json.Unmarshal(pdPayload, &pd); err != nil {
		t.Fatalf("decode plan_decomposed: %v", err)
	}
	if len(pd.ChildRunIDs) != 2 {
		t.Fatalf("child_run_ids = %d, want 2", len(pd.ChildRunIDs))
	}
	for _, cid := range pd.ChildRunIDs {
		cu := uuid.MustParse(cid)
		stages, lerr := r.api.ListRunStages(context.Background(), cu)
		if lerr != nil {
			t.Fatalf("ListRunStages(%s): %v", cid, lerr)
		}
		var implState string
		for _, s := range stages {
			if s.Type == "implement" {
				implState = s.State
			}
		}
		if implState != "awaiting_host_dispatch" {
			t.Fatalf("child %s implement stage on the wire = %q, want awaiting_host_dispatch (the #1980 producer-side pin)", cid, implState)
		}
	}

	// (5) CONSUME across the boundary: run_children must dispatch both parked
	// children. Fake spawn seam records invocations.
	var spawns int32
	withFakeSpawn(t, func(_ context.Context, _ string, _, _ []string, _ *mcp.CallToolRequest, _ any) ([]RunnerEvent, []string, int, error) {
		atomic.AddInt32(&spawns, 1)
		return completedEvents("ok"), nil, 0, nil
	})
	_, rcOut, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.ID.String(), Workflow: "feature_change", GitHubRepo: "example/repo", RunnerBinary: "/fake/fishhawk-runner",
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if rcOut.DispatchedCount != 2 {
		t.Fatalf("dispatched_count = %d, want 2 (both parked children dispatched across the wire)", rcOut.DispatchedCount)
	}
	if got := atomic.LoadInt32(&spawns); got != 2 {
		t.Errorf("spawn seam invoked %d times, want 2", got)
	}
}

// --- pending scope amendment surfacing (#2095 gap #1) ---

// amendmentThenCompletedEvents returns an event stream where the child first
// emits a scope_amendment_pending event (the mid-stage amendment that timed out
// undecided during the blocking fan-out) and then terminates with a
// runner_completed of the given outcome.
func amendmentThenCompletedEvents(amendmentID string, paths []string, outcome string) []RunnerEvent {
	pathObjs := make([]any, 0, len(paths))
	for _, p := range paths {
		pathObjs = append(pathObjs, map[string]any{"path": p, "operation": "modify"})
	}
	return []RunnerEvent{
		{Payload: map[string]any{"event": "scope_amendment_pending", "amendment_id": amendmentID, "paths": pathObjs}},
		{Payload: map[string]any{"event": "runner_completed", "outcome": outcome}},
	}
}

// warningContaining returns the first warning mentioning needle, or "".
func warningContaining(warnings []string, needle string) string {
	for _, w := range warnings {
		if strings.Contains(w, needle) {
			return w
		}
	}
	return ""
}

// TestRunChildren_PendingAmendment_FailedChildGuidesRetry is binding-condition-2
// branch (a): a child files a mid-stage amendment that times out UNDECIDED and
// then FAILS (the #2095 primary incident). The amendment (id + paths) is recorded
// on the child and in pending_amendment_children, and the recovery warning is
// TERMINAL-STATE-ACCURATE — it names fishhawk_decide_scope_amendment THEN
// fishhawk_retry_stage (a failed child is not re-run by a bare re-invoke), and
// does NOT route to fixup (that is the succeeded-child move).
func TestRunChildren_PendingAmendment_FailedChildGuidesRetry(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	child := uuid.New()
	seedChildRun(fb, child, "pending")
	seedPlanDecomposed(fb, parent, []string{child.String()}, 0)

	const amendID = "8f14e45f-ceea-467d-9a0c-1a2b3c4d5e6f"
	const amendPath = "backend/internal/foo/foo_test.go"
	withFakeSpawn(t, func(_ context.Context, _ string, _, _ []string, _ *mcp.CallToolRequest, _ any) ([]RunnerEvent, []string, int, error) {
		return amendmentThenCompletedEvents(amendID, []string{amendPath}, "failed"), nil, 1, nil
	})

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if len(out.Children) != 1 {
		t.Fatalf("want 1 child, got %d", len(out.Children))
	}
	c := out.Children[0]
	if len(c.PendingAmendments) != 1 || c.PendingAmendments[0].AmendmentID != amendID {
		t.Fatalf("child must record the pending amendment %q; got %+v", amendID, c.PendingAmendments)
	}
	if len(c.PendingAmendments[0].Paths) != 1 || c.PendingAmendments[0].Paths[0] != amendPath {
		t.Errorf("amendment paths = %v, want [%q]", c.PendingAmendments[0].Paths, amendPath)
	}
	if len(out.PendingAmendmentChildren) != 1 || out.PendingAmendmentChildren[0] != child.String() {
		t.Errorf("pending_amendment_children = %v, want [%s]", out.PendingAmendmentChildren, child)
	}
	msg := warningContaining(out.Warnings, child.String())
	if msg == "" {
		t.Fatalf("no recovery warning naming child %s in %v", child, out.Warnings)
	}
	for _, want := range []string{"fishhawk_decide_scope_amendment", "fishhawk_retry_stage"} {
		if !strings.Contains(msg, want) {
			t.Errorf("failed-child guidance must name %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "fishhawk_fixup_stage") {
		t.Errorf("failed-child guidance must NOT route to fixup (that is the succeeded-child move): %s", msg)
	}
}

// TestRunChildren_PendingAmendment_SucceededChildGuidesFixup is binding-condition-2
// branch (b): a child surfaces a mid-stage amendment but ultimately SUCCEEDS — it
// shipped WITHOUT the amendment (an inferior fallback). The amendment is still
// recorded (not silently swallowed), and the recovery warning states a re-run will
// NOT reopen it: it names fishhawk_decide_scope_amendment + fishhawk_fixup_stage and
// does NOT name fishhawk_retry_stage (which is the failed-child move).
func TestRunChildren_PendingAmendment_SucceededChildGuidesFixup(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	child := uuid.New()
	seedChildRun(fb, child, "pending")
	seedPlanDecomposed(fb, parent, []string{child.String()}, 0)

	const amendID = "1b4e28ba-2fa1-11d2-883f-0016d3cca427"
	const amendPath = "docs/api/v0.md"
	withFakeSpawn(t, func(_ context.Context, _ string, _, _ []string, _ *mcp.CallToolRequest, _ any) ([]RunnerEvent, []string, int, error) {
		return amendmentThenCompletedEvents(amendID, []string{amendPath}, "ok"), nil, 0, nil
	})

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if len(out.Children) != 1 {
		t.Fatalf("want 1 child, got %d", len(out.Children))
	}
	c := out.Children[0]
	if len(c.PendingAmendments) != 1 || c.PendingAmendments[0].AmendmentID != amendID {
		t.Fatalf("succeeded child must STILL record the pending amendment %q (not swallowed); got %+v", amendID, c.PendingAmendments)
	}
	if len(out.PendingAmendmentChildren) != 1 || out.PendingAmendmentChildren[0] != child.String() {
		t.Errorf("pending_amendment_children = %v, want [%s]", out.PendingAmendmentChildren, child)
	}
	msg := warningContaining(out.Warnings, child.String())
	if msg == "" {
		t.Fatalf("no recovery warning naming child %s in %v", child, out.Warnings)
	}
	for _, want := range []string{"fishhawk_decide_scope_amendment", "fishhawk_fixup_stage"} {
		if !strings.Contains(msg, want) {
			t.Errorf("succeeded-child guidance must name %q: %s", want, msg)
		}
	}
	if !strings.Contains(msg, "will NOT reopen") {
		t.Errorf("succeeded-child guidance must state a re-run will NOT reopen it: %s", msg)
	}
	if strings.Contains(msg, "fishhawk_retry_stage") {
		t.Errorf("succeeded-child guidance must NOT route to retry_stage (that is the failed-child move): %s", msg)
	}
}

// TestRunChildren_NoAmendment_NoSurfacing is binding-condition-2 branch (c): a
// child with NO scope_amendment_pending event produces output identical to today
// — no pending_amendments, no pending_amendment_children, and no amendment
// recovery warning (no false surfacing).
func TestRunChildren_NoAmendment_NoSurfacing(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	child := uuid.New()
	seedChildRun(fb, child, "pending")
	seedPlanDecomposed(fb, parent, []string{child.String()}, 0)

	withFakeSpawn(t, func(_ context.Context, _ string, _, _ []string, _ *mcp.CallToolRequest, _ any) ([]RunnerEvent, []string, int, error) {
		return completedEvents("ok"), nil, 0, nil
	})

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if len(out.Children) != 1 || len(out.Children[0].PendingAmendments) != 0 {
		t.Errorf("no amendment event must leave pending_amendments empty; got %+v", out.Children)
	}
	if out.PendingAmendmentChildren != nil {
		t.Errorf("pending_amendment_children must be nil with no amendment; got %v", out.PendingAmendmentChildren)
	}
	if msg := warningContaining(out.Warnings, "scope amendment"); msg != "" {
		t.Errorf("no amendment must emit no amendment recovery warning; got %q", msg)
	}
}

// TestRunChildren_PendingAmendment_IndeterminateChildHedges pins the
// high/untested_edge fix (#2095): an amendment-emitting child whose runner
// stream ends with a spawn error and NO runner_completed event has an
// INDETERMINATE terminal state — it must NOT be misclassified as FAILED and
// handed a possibly-invalid fishhawk_retry_stage instruction. The guidance must
// HEDGE: say the outcome could not be determined, name BOTH the failed-path
// (retry_stage) and succeeded-path (fixup_stage) recovery, and tell the operator
// to inspect the child's stage_state rather than assume failure.
func TestRunChildren_PendingAmendment_IndeterminateChildHedges(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	child := uuid.New()
	seedChildRun(fb, child, "pending")
	seedPlanDecomposed(fb, parent, []string{child.String()}, 0)

	const amendID = "3d813cbb-47fb-32ba-91df-831e1593ac29"
	const amendPath = "backend/internal/bar/bar.go"
	withFakeSpawn(t, func(_ context.Context, _ string, _, _ []string, _ *mcp.CallToolRequest, _ any) ([]RunnerEvent, []string, int, error) {
		// The child emits the scope_amendment_pending event, then its spawn
		// errors out with NO runner_completed — an indeterminate terminal state
		// (empty Outcome).
		pathObjs := []any{map[string]any{"path": amendPath, "operation": "modify"}}
		events := []RunnerEvent{
			{Payload: map[string]any{"event": "scope_amendment_pending", "amendment_id": amendID, "paths": pathObjs}},
		}
		return events, nil, 0, errors.New("fork/exec: process killed before runner_completed")
	})

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if len(out.Children) != 1 {
		t.Fatalf("want 1 child, got %d", len(out.Children))
	}
	c := out.Children[0]
	// The amendment is still surfaced despite the indeterminate outcome.
	if len(c.PendingAmendments) != 1 || c.PendingAmendments[0].AmendmentID != amendID {
		t.Fatalf("indeterminate child must record the pending amendment %q; got %+v", amendID, c.PendingAmendments)
	}
	// The child has an empty terminal Outcome (no runner_completed) — the seam the
	// classifier keys on for indeterminacy.
	if c.Outcome != "" {
		t.Fatalf("indeterminate child must have an empty Outcome; got %q", c.Outcome)
	}
	if len(out.PendingAmendmentChildren) != 1 || out.PendingAmendmentChildren[0] != child.String() {
		t.Errorf("pending_amendment_children = %v, want [%s]", out.PendingAmendmentChildren, child)
	}
	msg := warningContaining(out.Warnings, child.String())
	if msg == "" {
		t.Fatalf("no recovery warning naming child %s in %v", child, out.Warnings)
	}
	// The guidance must always name the decide verb.
	if !strings.Contains(msg, "fishhawk_decide_scope_amendment") {
		t.Errorf("indeterminate guidance must name fishhawk_decide_scope_amendment: %s", msg)
	}
	// It must HEDGE — say the outcome could not be determined and not assume failure.
	if !strings.Contains(msg, "could NOT be determined") {
		t.Errorf("indeterminate guidance must state the terminal outcome could not be determined (no FAILED assertion): %s", msg)
	}
	if !strings.Contains(msg, "Do NOT assume it failed") {
		t.Errorf("indeterminate guidance must tell the operator not to assume failure: %s", msg)
	}
	// It must name BOTH recovery verbs (failed-path retry, succeeded-path fixup),
	// unlike the determinate failed/succeeded branches which name exactly one.
	for _, want := range []string{"fishhawk_retry_stage", "fishhawk_fixup_stage"} {
		if !strings.Contains(msg, want) {
			t.Errorf("indeterminate guidance must name both recovery verbs (missing %q): %s", want, msg)
		}
	}
}

// TestScanPendingScopeAmendments_MalformedEventsNotSwallowed pins the
// low/untested_path fix (#2095): scanPendingScopeAmendments's doc contract that a
// malformed/absent field degrades to an empty value rather than DROPPING the
// event, so a pending amendment is never silently swallowed. Exercises a
// missing amendment_id, a non-string amendment_id, a non-[]any paths value, and
// path entries that are not maps — none of which existing tests (which feed only
// well-formed events via amendmentThenCompletedEvents) cover.
func TestScanPendingScopeAmendments_MalformedEventsNotSwallowed(t *testing.T) {
	events := []RunnerEvent{
		// (0) missing amendment_id → empty id, well-formed paths preserved.
		{Payload: map[string]any{"event": "scope_amendment_pending", "paths": []any{map[string]any{"path": "a.go"}}}},
		// (1) non-string amendment_id → degrades to empty id, event kept.
		{Payload: map[string]any{"event": "scope_amendment_pending", "amendment_id": 12345, "paths": []any{map[string]any{"path": "b.go"}}}},
		// (2) non-[]any paths → nil paths, event kept.
		{Payload: map[string]any{"event": "scope_amendment_pending", "amendment_id": "id-3", "paths": "not-a-list"}},
		// (3) path entries that are not maps → skipped, event kept.
		{Payload: map[string]any{"event": "scope_amendment_pending", "amendment_id": "id-4", "paths": []any{"not-a-map", 42}}},
		// A non-amendment event is correctly ignored (not surfaced).
		{Payload: map[string]any{"event": "runner_completed", "outcome": "ok"}},
	}
	got := scanPendingScopeAmendments(events)
	// The never-swallowed contract: all four malformed pending events surface.
	if len(got) != 4 {
		t.Fatalf("all 4 malformed scope_amendment_pending events must be surfaced (never swallowed); got %d: %+v", len(got), got)
	}
	if got[0].AmendmentID != "" || len(got[0].Paths) != 1 || got[0].Paths[0] != "a.go" {
		t.Errorf("missing amendment_id must degrade to empty id with paths preserved; got %+v", got[0])
	}
	if got[1].AmendmentID != "" || len(got[1].Paths) != 1 || got[1].Paths[0] != "b.go" {
		t.Errorf("non-string amendment_id must degrade to empty id with paths preserved; got %+v", got[1])
	}
	if got[2].AmendmentID != "id-3" || len(got[2].Paths) != 0 {
		t.Errorf("non-[]any paths must degrade to nil paths with the event kept; got %+v", got[2])
	}
	if got[3].AmendmentID != "id-4" || len(got[3].Paths) != 0 {
		t.Errorf("non-map path entries must be skipped with the event kept; got %+v", got[3])
	}
}
