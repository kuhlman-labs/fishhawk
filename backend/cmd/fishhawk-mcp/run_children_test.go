package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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
