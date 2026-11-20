package mcpserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
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

// --- detached spawn seam helpers (#2363) ---

// detachedSpawnCall records one spawnRunnerStageDetachedFn invocation: the argv
// the handler composed plus the (run, stage) pair and the report/probe closures
// it threaded.
type detachedSpawnCall struct {
	binary  string
	argv    []string
	env     []string
	runID   string
	stageID string
	report  detachedFailureReporter
	probe   detachedStageStateProbe
}

// fakeDetachedSpawn is a recording stand-in for spawnRunnerStageDetachedFn. It
// returns IMMEDIATELY, exactly as the real detached spawn does, records every
// call, and lets a test fail the spawn for a specific child run id.
type fakeDetachedSpawn struct {
	mu      sync.Mutex
	calls   []detachedSpawnCall
	failFor map[string]error
	// onCall, when non-nil, runs after the call is recorded and before the
	// return value is computed — the hook the modelled-runner tests use to
	// start a goroutine standing in for a live child.
	onCall func(c detachedSpawnCall)
}

func (f *fakeDetachedSpawn) fn(binary string, argv, env []string, runID, stageID string, report detachedFailureReporter, probe detachedStageStateProbe) (string, error) {
	c := detachedSpawnCall{binary: binary, argv: append([]string(nil), argv...), env: append([]string(nil), env...), runID: runID, stageID: stageID, report: report, probe: probe}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	err := f.failFor[runID]
	hook := f.onCall
	f.mu.Unlock()
	if hook != nil {
		hook(c)
	}
	if err != nil {
		return "", err
	}
	return "/tmp/fishhawk-runner-" + runID + ".log", nil
}

func (f *fakeDetachedSpawn) snapshot() []detachedSpawnCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]detachedSpawnCall(nil), f.calls...)
}

func (f *fakeDetachedSpawn) runIDs() []string {
	out := []string{}
	for _, c := range f.snapshot() {
		out = append(out, c.runID)
	}
	return out
}

// withFakeDetachedSpawn swaps spawnRunnerStageDetachedFn for the duration of a
// test and returns the recorder.
func withFakeDetachedSpawn(t *testing.T) *fakeDetachedSpawn {
	t.Helper()
	f := &fakeDetachedSpawn{failFor: map[string]error{}}
	orig := spawnRunnerStageDetachedFn
	spawnRunnerStageDetachedFn = f.fn
	t.Cleanup(func() { spawnRunnerStageDetachedFn = orig })
	return f
}

// childByID indexes a RunChildrenOutput's children by run id.
func childByID(out RunChildrenOutput) map[string]ChildResult {
	byID := map[string]ChildResult{}
	for _, c := range out.Children {
		byID[c.RunID] = c
	}
	return byID
}

// withFakeSpawn is a COMPATIBILITY ADAPTER over the detached seam. The child
// spawn is detached post-#2363, but drive_run_test.go's
// TestProbePatternMatchesChildSpawnArgv binds the child argv SHAPE through this
// helper and is not in this change's scope, so the helper survives as an
// adapter rather than being deleted: it installs a detached-seam fake that
// forwards the composed argv to the caller's blocking-shaped callback, ignores
// the event / exit-code returns a detached spawn cannot have, and maps a
// non-nil callback error to a spawn error.
func withFakeSpawn(t *testing.T, fn func(ctx context.Context, binary string, argv, env []string, req *mcp.CallToolRequest, progToken any) ([]RunnerEvent, []string, int, error)) {
	t.Helper()
	orig := spawnRunnerStageDetachedFn
	spawnRunnerStageDetachedFn = func(binary string, argv, env []string, runID, stageID string, _ detachedFailureReporter, _ detachedStageStateProbe) (string, error) {
		if _, _, _, err := fn(context.Background(), binary, argv, env, nil, nil); err != nil {
			return "", err
		}
		return "/tmp/fishhawk-runner-" + runID + ".log", nil
	}
	t.Cleanup(func() { spawnRunnerStageDetachedFn = orig })
}

// completedEvents returns a one-event slice carrying a terminal
// runner_completed event. Retained only for the withFakeSpawn adapter's
// callers; a detached spawn returns no event stream to run_children.
func completedEvents(outcome string) []RunnerEvent {
	return []RunnerEvent{{Payload: map[string]any{"event": "runner_completed", "outcome": outcome}}}
}

// gitRunT runs a git command in dir, failing the test on error.
func gitRunT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// --- fixture seeding ---

// seedPlanDecomposed appends a plan_decomposed audit entry to the parent run so
// LatestPlanDecomposed discovers the children + effective cap.

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

	spawn := withFakeDetachedSpawn(t)

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != 1 {
		t.Errorf("dispatched_count = %d, want 1 (only the pending child)", out.DispatchedCount)
	}
	if got := spawn.runIDs(); len(got) != 1 || got[0] != pendingID.String() {
		t.Errorf("dispatched the wrong children: %v (want only %s)", got, pendingID)
	}
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

	spawn := withFakeDetachedSpawn(t)

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != 2 {
		t.Fatalf("dispatched_count = %d, want 2 (the run='running' + stage=pending/awaiting_host_dispatch children)", out.DispatchedCount)
	}
	calls := spawn.snapshot()
	if len(calls) != 2 {
		t.Errorf("spawned %d children, want 2: %v", len(calls), spawn.runIDs())
	}
	for _, c := range calls {
		if !strings.Contains(strings.Join(c.argv, " "), "--parallel-isolate") {
			t.Errorf("dispatch argv missing --parallel-isolate: %v", c.argv)
		}
	}

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

	spawn := withFakeDetachedSpawn(t)

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != 0 {
		t.Errorf("dispatched_count = %d, want 0 (all children in-flight/legacy-parked)", out.DispatchedCount)
	}
	if got := len(spawn.snapshot()); got != 0 {
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

func containsWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// --- cross-boundary integration: real fishhawk-runner subprocesses ---

func warningContaining(warnings []string, needle string) string {
	for _, w := range warnings {
		if strings.Contains(w, needle) {
			return w
		}
	}
	return ""
}

func TestRunChildren_HTTPTransportRefusesOmittedWorkingDir(t *testing.T) {
	fb, srv := newFakeBackend(t)
	// The binary comes from the daemon's FISHHAWK_RUNNER_BIN rung: runner_binary
	// is refused over HTTP (E66.64 / #4476), and a resolvable binary keeps a
	// deleted working_dir control reaching a spawn.
	r := newResolver(srv, map[string]string{"FISHHAWK_RUNNER_BIN": "/fake/fishhawk-runner"})
	r.httpTransport = true

	parent := uuid.New()
	child0 := uuid.New()
	seedChildRun(fb, child0, "pending")
	seedPlanDecomposed(fb, parent, []string{child0.String()}, 0)

	spawn := withFakeDetachedSpawn(t)

	_, _, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y",
		// working_dir intentionally omitted.
	})
	if err == nil {
		t.Fatal("expected a refusal error when working_dir is omitted over HTTP")
	}
	if !strings.Contains(err.Error(), "working_dir") {
		t.Errorf("error should name working_dir; got %v", err)
	}
	// No per-child worktree provisioned: no runner spawned (the runner is what
	// provisions the worktree via --parallel-isolate).
	if got := len(spawn.snapshot()); got != 0 {
		t.Errorf("spawned %d runners (and provisioned worktrees) despite the refusal; want 0", got)
	}
	// No host-dispatch marker CAS-flipped either.
	fb.mu.Lock()
	markers := 0
	for _, n := range fb.hostDispatchCalledByID {
		markers += n
	}
	fb.mu.Unlock()
	if markers != 0 {
		t.Errorf("host-dispatch markers called %d times, want 0 (refusal must commit no state)", markers)
	}
}

// TestRunChildren_HTTPTransportRefusesOmittedWorkingDir_NoWorktree is approval
// condition 3's load-bearing counterfactual for run_children: it drives the REAL
// spawnRunnerStage seam (a real fishhawk-runner provisions a per-child worktree
// via --parallel-isolate) and asserts, ON THE FILESYSTEM, that the
// omitted-working_dir HTTP refusal provisions NONE. The state assertion lands on
// the worktrees root directly, not on a spawn-count proxy — so a regression that
// provisioned a worktree BEFORE refusing working_dir (the fired-then-refused
// case the fast variant's spawn-count check cannot catch) leaves a run-<child>
// dir here and turns this red.
//
// getwd is injected to the real repo so the counterfactual is attainable:
// DELETING the http+empty refusal in resolveWorkingDir makes an omitted
// working_dir fall THROUGH to getwd (the repo), the real runner spawns, and a
// real worktree appears under repo/.git/fishhawk-worktrees — the observed RED.
// With the control present, resolveWorkingDir refuses before getwd is consulted,
// so no runner spawns and the root stays empty/absent (#2479, fix-up).
func TestRunChildren_HTTPTransportRefusesOmittedWorkingDir_NoWorktree(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the fishhawk-runner binary and spawns real subprocesses")
	}
	for _, tool := range []string{"go", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}

	// (1) Build the real fishhawk-runner from the runner module.
	runnerDir := filepath.Join(pkgSrcDir, "..", "..", "..", "runner", "cmd", "fishhawk-runner")
	runnerBin := filepath.Join(t.TempDir(), "fishhawk-runner")
	build := exec.Command("go", "build", "-o", runnerBin, ".")
	build.Dir = runnerDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fishhawk-runner: %v\n%s", err, out)
	}

	// (2) A fake `claude` that exits non-zero — a spawned runner would fail fast
	// but only AFTER it provisions its worktree, so a worktree would persist.
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "claude"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	// (3) Real operator git repo with a seed commit (worktree add needs one).
	repo := t.TempDir()
	gitRunT(t, repo, "init", "-q")
	// Seed an identity explicitly, the convention the rest of the suite follows.
	// A fresh temp repo inherits none, and git only falls back to an OS-derived
	// ident when that ident is usable: the CI runner's is EMPTY, so the commit
	// below died `exit status 128 / fatal: empty ident name`. It passed locally
	// only because a developer machine derives a populated name — an
	// environment dependency, not a property of the test.
	gitRunT(t, repo, "config", "user.email", "seed@example.com")
	gitRunT(t, repo, "config", "user.name", "seed")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunT(t, repo, "add", "-A")
	gitRunT(t, repo, "commit", "-q", "-m", "seed")

	// (4) Backend serving BOTH the MCP-side discovery calls (reached before the
	// refusal) and the runner-subprocess calls (only reached in the deleted-control
	// counterfactual, when the real runner actually spawns).
	parent := uuid.New()
	child := uuid.New()
	stage := uuid.New()
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
		if r.PathValue("run_id") != child.String() {
			writeJSON(w, http.StatusOK, listStagesResult{})
			return
		}
		writeJSON(w, http.StatusOK, listStagesResult{Items: []Stage{
			{ID: stage.String(), RunID: child.String(), Type: "implement", State: "pending"},
		}})
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/audit", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("category") != "plan_decomposed" {
			writeJSON(w, http.StatusOK, listAuditResult{})
			return
		}
		writeJSON(w, http.StatusOK, listAuditResult{Items: []AuditEntry{{
			ID: uuid.NewString(), Sequence: 1, RunID: r.PathValue("run_id"), Category: "plan_decomposed",
			Payload: map[string]any{
				"child_run_ids":          []string{child.String()},
				"effective_max_parallel": 1,
			},
		}}})
	})
	mux.HandleFunc("POST /v0/runs/{run_id}/stages/{stage_id}/host-dispatch", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, HostDispatchResult{Transitioned: true, StageState: "dispatched"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// (5) HTTP transport; the spawn seam is left at its default (real
	// spawnRunnerStage). getwd is injected to the real repo so a DELETED
	// http+empty refusal would fall through to it and spawn a real runner — the
	// counterfactual RED. With the control present getwd is never consulted.
	// The real runner resolves through the daemon's FISHHAWK_RUNNER_BIN rung,
	// not runner_binary, which is refused over HTTP (E66.64 / #4476) — so the
	// deleted-control counterfactual still reaches a real spawn.
	r := &runResolver{
		api:           newAPIClient(config{backendURL: srv.URL, apiToken: "tok"}),
		getenv:        envFuncFromMap(map[string]string{"FISHHAWK_RUNNER_BIN": runnerBin}),
		httpTransport: true,
		getwd:         func() (string, error) { return repo, nil },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, _, err = r.runChildren(ctx, nil, RunChildrenInput{
		RunID:      parent.String(),
		Workflow:   "wf",
		GitHubRepo: "x/y",
		// working_dir intentionally omitted → refused over HTTP before any spawn.
	})
	// Non-fatal on the error precheck so the worktree-state assertion below ALWAYS
	// runs: under the deleted-control counterfactual the refusal error disappears
	// AND a real per-child worktree is provisioned, and the load-bearing RED must
	// land on that filesystem state, not on the error precheck.
	if err == nil {
		t.Error("expected a refusal error when working_dir is omitted over HTTP")
	} else if !strings.Contains(err.Error(), "working_dir") {
		t.Errorf("error should name working_dir; got %v", err)
	}

	// (6) The load-bearing state assertion (condition 3): NO per-child worktree
	// was provisioned. Assert directly on the worktrees root rather than inferring
	// it from a spawn count — an absent root means zero worktrees; a present root
	// must hold no run-<child> dir.
	wtRoot := filepath.Join(repo, ".git", "fishhawk-worktrees")
	entries, rderr := os.ReadDir(wtRoot)
	if rderr == nil {
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "run-") {
				t.Errorf("per-child worktree %q provisioned despite the refusal; want none", e.Name())
			}
		}
	} else if !os.IsNotExist(rderr) {
		t.Fatalf("read worktrees root %s: %v", wtRoot, rderr)
	}
}

// TestRunChildren_StdioTransportOmittedResolvesToAbsoluteCwd asserts the stdio
// default: an omitted working_dir resolves to the absolute process cwd and each
// child's argv carries `--working-dir <cwd>`, never "." (#2479).
func TestRunChildren_StdioTransportOmittedResolvesToAbsoluteCwd(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil) // stdio

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	parent := uuid.New()
	child0 := uuid.New()
	seedChildRun(fb, child0, "pending")
	seedPlanDecomposed(fb, parent, []string{child0.String()}, 0)

	spawn := withFakeDetachedSpawn(t)

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	calls := spawn.snapshot()
	if len(calls) != 1 {
		t.Fatalf("spawn calls = %d, want 1", len(calls))
	}
	joined := strings.Join(calls[0].argv, " ")
	if !strings.Contains(joined, "--working-dir "+cwd) {
		t.Errorf("child argv missing --working-dir %q: %v", cwd, joined)
	}
	if strings.Contains(joined, "--working-dir .") {
		t.Errorf("child argv carries the literal \".\": %v", joined)
	}
	if out.ResolvedWorkingDir != cwd {
		t.Errorf("resolved_working_dir = %q, want %q", out.ResolvedWorkingDir, cwd)
	}
}

// TestRunChildren_ExplicitWorkingDirEchoedAndUnchanged passes an explicit
// absolute working_dir and asserts resolved_working_dir echoes it and the child
// argv still carries `--working-dir <that path>` (#2479).
func TestRunChildren_ExplicitWorkingDirEchoedAndUnchanged(t *testing.T) {
	fb, srv := newFakeBackend(t)
	// Daemon-side binary rung: runner_binary is refused over HTTP (E66.64 / #4476).
	r := newResolver(srv, map[string]string{"FISHHAWK_RUNNER_BIN": "/fake/fishhawk-runner"})
	r.httpTransport = true

	dir := t.TempDir()
	// E66.63 / #3589: the fixture's own temp root is the allowed checkout root,
	// so this ACCEPT case keeps pinning what it was written to pin instead of
	// tripping on the confinement fail-closed default.
	r.allowedRoots = []string{dir}
	parent := uuid.New()
	child0 := uuid.New()
	seedChildRun(fb, child0, "pending")
	seedPlanDecomposed(fb, parent, []string{child0.String()}, 0)

	spawn := withFakeDetachedSpawn(t)

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y",
		WorkingDir: dir,
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.ResolvedWorkingDir != dir {
		t.Errorf("resolved_working_dir = %q, want %q", out.ResolvedWorkingDir, dir)
	}
	calls := spawn.snapshot()
	if len(calls) != 1 {
		t.Fatalf("spawn calls = %d, want 1", len(calls))
	}
	joined := strings.Join(calls[0].argv, " ")
	if !strings.Contains(joined, "--working-dir "+dir) {
		t.Errorf("child argv missing --working-dir %q: %v", dir, joined)
	}
}

// TestRunChildren_InheritsBoundWorkingDir (E66.42 / #2482): run_children called
// over HTTP with working_dir OMITTED inherits the PARENT run's start_run binding
// (children provision their per-child worktrees under that checkout) — the child
// argv carries --working-dir <bound> and resolved_working_dir echoes it.
func TestRunChildren_InheritsBoundWorkingDir(t *testing.T) {
	fb, srv := newFakeBackend(t)
	// Daemon-side binary rung: runner_binary is refused over HTTP (E66.64 / #4476).
	r := newResolver(srv, map[string]string{"FISHHAWK_RUNNER_BIN": "/fake/fishhawk-runner"})
	r.httpTransport = true

	bound := t.TempDir()
	// E66.63 / #3589: the fixture's own temp root is the allowed checkout root,
	// so this ACCEPT case keeps pinning what it was written to pin instead of
	// tripping on the confinement fail-closed default.
	r.allowedRoots = []string{bound}
	parent := uuid.New()
	child0 := uuid.New()
	seedRunWorkingDir(fb, parent, bound)
	seedChildRun(fb, child0, "pending")
	seedPlanDecomposed(fb, parent, []string{child0.String()}, 0)

	spawn := withFakeDetachedSpawn(t)

	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y",
		// working_dir OMITTED — inherits the parent's binding.
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.ResolvedWorkingDir != bound {
		t.Errorf("resolved_working_dir = %q, want inherited %q", out.ResolvedWorkingDir, bound)
	}
	calls := spawn.snapshot()
	if len(calls) != 1 {
		t.Fatalf("spawn calls = %d, want 1", len(calls))
	}
	joined := strings.Join(calls[0].argv, " ")
	if !strings.Contains(joined, "--working-dir "+bound) {
		t.Errorf("child argv missing inherited --working-dir %q: %v", bound, joined)
	}
}

// --- E50.13 / #2363: non-blocking detached dispatch ---

// stageStateOf reads a seeded stage's state back out of the fake — the
// COMMITTED-STATE read the spawn-error counterfactuals turn on. The tool's
// returned value is byte-identical with and without the compensation, so
// asserting on the error would leave the control undetectable.
func stageStateOf(fb *fakeBackend, childID uuid.UUID) string {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	for _, s := range fb.stagesByRun[childID] {
		if s.Type == "implement" {
			return s.State
		}
	}
	return ""
}

func stageIDOf(fb *fakeBackend, childID uuid.UUID) string {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	for _, s := range fb.stagesByRun[childID] {
		if s.Type == "implement" {
			return s.ID
		}
	}
	return ""
}

func runChildrenIn(parent uuid.UUID) RunChildrenInput {
	return RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", GitHubRepo: "x/y", RunnerBinary: "/fake/fishhawk-runner",
	}
}

// TestRunChildren_ReturnsWithoutAwaitingChildren is the STRUCTURAL pin for the
// whole point of #2363: the handler returns while the spawned children are
// still notionally live. The seam records the call and returns immediately (as
// the real detached spawn does), and the response carries Dispatched:true plus a
// non-empty log_path — the durable handle — with no terminal outcome field to
// carry, because the type no longer has one.
func TestRunChildren_ReturnsWithoutAwaitingChildren(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent, child := uuid.New(), uuid.New()
	seedChildRun(fb, child, "pending")
	seedPlanDecomposed(fb, parent, []string{child.String()}, 0)
	spawn := withFakeDetachedSpawn(t)

	_, out, err := r.runChildren(context.Background(), nil, runChildrenIn(parent))
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != 1 {
		t.Fatalf("dispatched_count = %d, want 1", out.DispatchedCount)
	}
	c := childByID(out)[child.String()]
	if !c.Dispatched || c.LogPath == "" {
		t.Errorf("child = %+v, want dispatched with a non-empty log_path", c)
	}
	if len(spawn.snapshot()) != 1 {
		t.Errorf("spawn calls = %d, want 1", len(spawn.snapshot()))
	}
	if out.NextStep == nil || out.NextStep.Action != "fishhawk_await_children" {
		t.Errorf("next_step = %+v, want fishhawk_await_children", out.NextStep)
	}
	if out.NextStep.Params["run_id"] != parent.String() {
		t.Errorf("next_step run_id = %q, want %q", out.NextStep.Params["run_id"], parent)
	}
}

// TestRunChildren_SourceHasNoConcurrencyShape is the deterministic drift guard
// (the scripts/test-dev body-grep precedent) for the ownership shape the five
// rejected option-1 cycles kept reintroducing: a handler returning while its
// goroutines write into the value it returned. The rewrite deleted the
// goroutines rather than relocating them, and this pins the deletion.
func TestRunChildren_SourceHasNoConcurrencyShape(t *testing.T) {
	src, err := os.ReadFile("run_children.go")
	if err != nil {
		t.Fatalf("read run_children.go: %v", err)
	}
	body := string(src)
	// Strip line comments: the doc comments deliberately NAME the deleted shape.
	var code strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code.WriteString(line)
		code.WriteString("\n")
	}
	for _, needle := range []string{"errgroup", "g.Wait", "sync.Mutex", "go func("} {
		if strings.Contains(code.String(), needle) {
			t.Errorf("run_children.go code contains %q — the concurrency shape #2363 deleted must not return", needle)
		}
	}
}

// TestRunChildren_MarkerTransportErrorNoSpawn pins the #1912 fail-closed marker
// arm: a marker error means NO spawn.
func TestRunChildren_MarkerTransportErrorNoSpawn(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent, child := uuid.New(), uuid.New()
	seedChildRun(fb, child, "pending")
	seedPlanDecomposed(fb, parent, []string{child.String()}, 0)
	fb.mu.Lock()
	fb.hostDispatchStatus = http.StatusInternalServerError
	fb.mu.Unlock()
	spawn := withFakeDetachedSpawn(t)

	_, out, err := r.runChildren(context.Background(), nil, runChildrenIn(parent))
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != 0 || len(spawn.snapshot()) != 0 {
		t.Fatalf("dispatched=%d spawns=%d, want 0/0 (fail closed)", out.DispatchedCount, len(spawn.snapshot()))
	}
	if w := warningContaining(childByID(out)[child.String()].Warnings, "fail-closed"); w == "" {
		t.Errorf("child warnings = %v, want a fail-closed marker warning", childByID(out)[child.String()].Warnings)
	}
}

// TestRunChildren_MarkerNoopNoSpawn pins the #1912 double-spawn guard: a
// transitioned:false marker (a concurrent invocation already won the CAS) never
// spawns a second runner.
func TestRunChildren_MarkerNoopNoSpawn(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent, child := uuid.New(), uuid.New()
	seedChildRun(fb, child, "pending")
	seedPlanDecomposed(fb, parent, []string{child.String()}, 0)
	fb.mu.Lock()
	fb.hostDispatchForceNoop = true
	fb.mu.Unlock()
	spawn := withFakeDetachedSpawn(t)

	_, out, err := r.runChildren(context.Background(), nil, runChildrenIn(parent))
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != 0 || len(spawn.snapshot()) != 0 {
		t.Fatalf("dispatched=%d spawns=%d, want 0/0 (double-spawn guard)", out.DispatchedCount, len(spawn.snapshot()))
	}
	c := childByID(out)[child.String()]
	if c.StageState != "dispatched" {
		t.Errorf("stage_state = %q, want the marker's echoed 'dispatched'", c.StageState)
	}
	if w := warningContaining(c.Warnings, "double-spawn"); w == "" {
		t.Errorf("warnings = %v, want the no-op/double-spawn warning", c.Warnings)
	}
}

// TestRunChildren_WaveNotIntegratedRefusalNoSpawn pins the NEW marker arm
// (#2363): a 409 wave_not_integrated is not an error to fail closed on
// silently — it is "wait, the server is integrating", and the warning must say
// so and point at fishhawk_await_children.
func TestRunChildren_WaveNotIntegratedRefusalNoSpawn(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent, child := uuid.New(), uuid.New()
	seedChildRun(fb, child, "pending")
	seedPlanDecomposed(fb, parent, []string{child.String()}, 0)
	fb.mu.Lock()
	fb.hostDispatchStatus = http.StatusConflict
	fb.hostDispatchErrBody = `{"error":{"code":"wave_not_integrated","message":"predecessors not merged"}}`
	fb.mu.Unlock()
	spawn := withFakeDetachedSpawn(t)

	_, out, err := r.runChildren(context.Background(), nil, runChildrenIn(parent))
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if len(spawn.snapshot()) != 0 {
		t.Fatalf("spawns = %d, want 0", len(spawn.snapshot()))
	}
	c := childByID(out)[child.String()]
	if c.Dispatched {
		t.Error("child marked dispatched despite the wave_not_integrated refusal")
	}
	// Assert on text ONLY THIS ARM produces. The client already annotates the
	// 409 with both "wave_not_integrated" and "fishhawk_await_children", so
	// matching either of those would be satisfied by the generic fail-closed arm
	// re-printing the client's error — a green counterfactual (observed).
	w := warningContaining(c.Warnings, "The server integrates between waves")
	if w == "" {
		t.Fatalf("warnings = %v, want the distinct wave_not_integrated arm", c.Warnings)
	}
	if strings.Contains(w, "fail-closed") {
		t.Errorf("the refusal took the generic fail-closed arm rather than the wave arm: %s", w)
	}
	if !strings.Contains(w, "fishhawk_await_children") {
		t.Errorf("warning does not point at fishhawk_await_children: %s", w)
	}
}

// TestRunChildren_UnparseableChildSkipsAndContinues pins that a malformed child
// id is recorded and the loop CONTINUES to the next child.
func TestRunChildren_UnparseableChildSkipsAndContinues(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent, good := uuid.New(), uuid.New()
	seedChildRun(fb, good, "pending")
	seedPlanDecomposed(fb, parent, []string{"not-a-uuid", good.String()}, 0)
	spawn := withFakeDetachedSpawn(t)

	_, out, err := r.runChildren(context.Background(), nil, runChildrenIn(parent))
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != 1 {
		t.Fatalf("dispatched_count = %d, want 1 (the loop must continue past the bad id)", out.DispatchedCount)
	}
	if got := spawn.runIDs(); len(got) != 1 || got[0] != good.String() {
		t.Errorf("spawned %v, want only %s", got, good)
	}
	if !containsWarning(out.Warnings, "not a valid UUID") {
		t.Errorf("warnings = %v, want the unparseable-child warning", out.Warnings)
	}
}

// --- C5: THE SPAWN-ERROR COMPENSATION ---

// TestRunChildren_SpawnErrorDoesNotStrandStage is counterfactual C5. The
// host-dispatch marker has ALREADY CAS-flipped the child's stage to
// 'dispatched' when the spawn fails, and the tool's returned value is
// byte-identical with or without the compensation — so the assertion is a
// COMMITTED-STATE READ plus the report seam's recorded category. Bad state is
// seeded BY CONSTRUCTION: the spawn seam is configured to fail for this child,
// so nothing in the setup calls the control.
func TestRunChildren_SpawnErrorDoesNotStrandStage(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent, child, other := uuid.New(), uuid.New(), uuid.New()
	seedChildRun(fb, child, "pending")
	seedChildRun(fb, other, "pending")
	seedPlanDecomposed(fb, parent, []string{child.String(), other.String()}, 0)
	stageID := uuid.MustParse(stageIDOf(fb, child))

	spawn := withFakeDetachedSpawn(t)
	spawn.failFor[child.String()] = errors.New("fork/exec /fake/fishhawk-runner: no such file")

	_, out, err := r.runChildren(context.Background(), nil, runChildrenIn(parent))
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}

	// (1) COMMITTED STATE: the stage must NOT be left 'dispatched'.
	if got := stageStateOf(fb, child); got != "failed" {
		t.Errorf("child implement stage = %q, want \"failed\" — an uncompensated spawn error strands it in 'dispatched'", got)
	}
	// (2) The report seam fired with the retryable infrastructure category.
	fb.mu.Lock()
	reports := fb.reapFailureByStage[stageID]
	fb.mu.Unlock()
	if len(reports) != 1 {
		t.Fatalf("reap-failure reports = %d, want 1", len(reports))
	}
	if reports[0].Category != "C" {
		t.Errorf("report category = %q, want \"C\"", reports[0].Category)
	}
	if reports[0].Reason != "runner_spawn_failed" {
		t.Errorf("report reason = %q, want runner_spawn_failed", reports[0].Reason)
	}
	// (3) The child is data, not a tool error, and the warning names the
	// recovery that genuinely works from failed/category-C.
	c := childByID(out)[child.String()]
	if c.Dispatched {
		t.Error("child marked dispatched despite the spawn error")
	}
	if w := warningContaining(c.Warnings, "fishhawk_retry_stage"); w == "" {
		t.Errorf("warnings = %v, want a retry_stage recovery pointer", c.Warnings)
	}
	// (3b) #2695 item 4: on a SUCCEEDING report, exactly ONE warning is appended
	// and it carries the success claim (report-before-claim ordering).
	if len(c.Warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one (the single success-claim warning)", c.Warnings)
	}
	if !strings.Contains(c.Warnings[0], "Reported it as a category-C stage failure") {
		t.Errorf("success warning missing the report claim: %v", c.Warnings)
	}
	// (4) The loop CONTINUED to the next child.
	if !childByID(out)[other.String()].Dispatched {
		t.Error("the loop did not continue to the next child after a spawn error")
	}
}

// TestRunChildren_SpawnErrorThenRetryRedispatches drives the rejection's
// "a subsequent run_children can retry that child" claim END TO END rather than
// reasoning it: after the fake applies the standard local retry park
// (failed -> awaiting_host_dispatch, backend/internal/server/retry.go), a SECOND
// real runChildren invocation re-partitions the child as dispatchable and spawns
// it again.
func TestRunChildren_SpawnErrorThenRetryRedispatches(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent, child := uuid.New(), uuid.New()
	seedChildRun(fb, child, "pending")
	seedPlanDecomposed(fb, parent, []string{child.String()}, 0)

	spawn := withFakeDetachedSpawn(t)
	spawn.failFor[child.String()] = errors.New("boom")
	if _, _, err := r.runChildren(context.Background(), nil, runChildrenIn(parent)); err != nil {
		t.Fatalf("first runChildren: %v", err)
	}
	if got := stageStateOf(fb, child); got != "failed" {
		t.Fatalf("stage after failed spawn = %q, want failed", got)
	}

	// The retry park a runner_kind local run gets: failed -> awaiting_host_dispatch.
	fb.mu.Lock()
	for i := range fb.stagesByRun[child] {
		fb.stagesByRun[child][i].State = "awaiting_host_dispatch"
	}
	fb.mu.Unlock()
	delete(spawn.failFor, child.String())

	_, out, err := r.runChildren(context.Background(), nil, runChildrenIn(parent))
	if err != nil {
		t.Fatalf("second runChildren: %v", err)
	}
	if out.DispatchedCount != 1 {
		t.Fatalf("dispatched_count on the retry = %d, want 1", out.DispatchedCount)
	}
	if n := len(spawn.snapshot()); n != 2 {
		t.Errorf("spawn calls = %d, want 2 (the failed attempt + the retry)", n)
	}
}

// TestRunChildren_CancelRefusedSpawnIsNotCompensated pins BRANCH 1: a spawn
// refused because a cancel already landed must NOT be reported as a stage
// failure — the cancel path owns the run's terminal state, and relabelling a
// cancelled run as failed is the defect.
func TestRunChildren_CancelRefusedSpawnIsNotCompensated(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent, child := uuid.New(), uuid.New()
	seedChildRun(fb, child, "pending")
	seedPlanDecomposed(fb, parent, []string{child.String()}, 0)
	stageID := uuid.MustParse(stageIDOf(fb, child))

	spawn := withFakeDetachedSpawn(t)
	spawn.failFor[child.String()] = fmt.Errorf("wrapped: %w", errRunCancelledBeforeSpawn)

	_, out, err := r.runChildren(context.Background(), nil, runChildrenIn(parent))
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	fb.mu.Lock()
	reports := fb.reapFailureByStage[stageID]
	fb.mu.Unlock()
	if len(reports) != 0 {
		t.Fatalf("reap-failure reports = %d, want 0 (the cancel owns the terminal state)", len(reports))
	}
	c := childByID(out)[child.String()]
	if c.Dispatched {
		t.Error("child marked dispatched despite the refused spawn")
	}
	if w := warningContaining(c.Warnings, "cancel"); w == "" {
		t.Errorf("warnings = %v, want a cancel-refusal warning", c.Warnings)
	}
}

// TestRunChildren_FailedCompensationDisclosesStrand pins the HONEST residual
// (binding condition 2, option (b)): when the report ITSELF fails the stage is
// genuinely stranded in 'dispatched' with no runner, and the second warning must
// say so AND name the verb that actually clears it. fishhawk_retry_stage does
// NOT: run.RetryStage admits only a stage already in state 'failed'. The
// clearing verb is the reap-failure REST endpoint.
func TestRunChildren_FailedCompensationDisclosesStrand(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent, child := uuid.New(), uuid.New()
	seedChildRun(fb, child, "pending")
	seedPlanDecomposed(fb, parent, []string{child.String()}, 0)

	spawn := withFakeDetachedSpawn(t)
	spawn.failFor[child.String()] = errors.New("boom")
	fb.mu.Lock()
	fb.reapFailureStatus = http.StatusInternalServerError
	fb.mu.Unlock()

	_, out, err := r.runChildren(context.Background(), nil, runChildrenIn(parent))
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	// The honest committed state: still 'dispatched'. The test asserts the
	// STRAND, not a recovery, because no recovery happened.
	if got := stageStateOf(fb, child); got != "dispatched" {
		t.Errorf("stage = %q, want the disclosed 'dispatched' strand", got)
	}
	c := childByID(out)[child.String()]
	// #2695 item 4: on a FAILED report, exactly ONE warning is appended — the
	// self-contained strand disclosure — with NO preceding success claim to
	// contradict.
	if len(c.Warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one (the single strand disclosure)", c.Warnings)
	}
	strand := warningContaining(c.Warnings, "STRANDED")
	if strand == "" {
		t.Fatalf("warnings = %v, want an explicit strand disclosure", c.Warnings)
	}
	// IDENTITY assertion (not just a count): the strand must NOT claim a
	// successful report. A reworded duplicate that still asserted the report
	// succeeded would pass a count-only check but fails here.
	if strings.Contains(strand, "Reported it as a category-C stage failure") {
		t.Errorf("strand disclosure falsely claims the report succeeded: %s", strand)
	}
	if !strings.Contains(strand, "reap-failure") {
		t.Errorf("strand warning does not name the reap-failure endpoint (the fallback for a client without the verb): %s", strand)
	}
	if !strings.Contains(strand, "will NOT clear it") {
		t.Errorf("strand warning does not retract the retry_stage claim: %s", strand)
	}
	// #2689: the warning must now prescribe a recovery that ACTUALLY WORKS as an
	// MCP verb, not only a hand-rolled POST. This is the behavioral done-means
	// test for the prose change — a comment-only touch of run_children.go fails
	// here.
	for _, want := range []string{"fishhawk_reap_stage", "fishhawk_retry_stage"} {
		if !strings.Contains(strand, want) {
			t.Errorf("strand warning does not name %s as the recovery: %s", want, strand)
		}
	}
	// The prescribed call must be actionable: it carries THIS stage's ids.
	if !strings.Contains(strand, stageIDOf(fb, child)) {
		t.Errorf("strand warning does not name the stranded stage id: %s", strand)
	}
}

// TestRunChildren_ChildExitsNonZeroWithoutSettling_ReapStageRecovers is the
// issue's done-means test (#2689) with BOTH halves composed in ONE test:
// strand PRODUCTION and strand RECOVERY.
//
// PRODUCTION. A REAL child is spawned detached through the REAL
// spawnRunnerStageDetached (no seam fake) with a stub runner that emits a
// runner_failed line and exits NON-ZERO without settling its stage. The reaper
// goroutine fires exactly as in production — and its reap-failure POST is
// answered 500 for the whole of its bounded-backoff retry loop, which is the
// SECOND residual strand path this change serves (reportDetachedFailureWithRetry
// exhausting to a stderr diagnostic). The test waits for that loop to exhaust
// (an exhaustion the accepted-bodies map cannot express, hence the fake's
// attempt counter) and then asserts the honest committed state: the stage is
// STILL 'running', with no runner and no reaper left.
//
// ORDERING (#2689 review). The stub runner cannot itself perform the runner's
// signed prompt fetch — the one transition that flips a stage dispatched ->
// running — so the test applies that flip on its behalf. For the observed strand
// to be PRODUCED by the child's exit rather than MANUFACTURED after it, the flip
// must provably precede the exit, so the stub and the test handshake through two
// files: the stub touches `ready` as its FIRST act and then BLOCKS until the test
// creates `gate`, and only then emits its runner_failed line and exits 7. The
// test waits for `ready`, applies the flip, asserts the fake has recorded ZERO
// reap-failure attempts at that instant (a positive check that the child had not
// yet exited and no reaper had yet run), and only then opens the gate. The
// 'running' state the reaper leaves behind is therefore the state the child's
// non-zero exit found and failed to settle.
//
// RECOVERY. With the backend healthy again the operator invokes
// fishhawk_reap_stage against that SAME fake backend, and the test asserts
// transitioned:true / stage_state failed / a fishhawk_retry_stage next_step, and
// that the fake's COMMITTED stage state is now 'failed'. The accepted body's
// reason is asserted to be the OPERATOR's default, not the runner's parsed
// reason, so a late reaper attempt could not be mistaken for the recovery.
func TestRunChildren_ChildExitsNonZeroWithoutSettling_ReapStageRecovers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub runner uses /bin/sh")
	}
	// Shrink the reaper's retry backoff so the exhaustion path is fast. Every
	// wall-clock bound below derives from timescale.D so one factor scales them
	// together on a loaded runner (AGENTS.md / #1984).
	origBackoff := reapReportBackoff
	reapReportBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { reapReportBackoff = origBackoff })

	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent, child := uuid.New(), uuid.New()
	seedChildRun(fb, child, "pending")
	seedPlanDecomposed(fb, parent, []string{child.String()}, 0)

	// The reaper's OWN report fails throughout: this is what leaves the residual
	// strand instead of the reaper clearing it itself.
	fb.mu.Lock()
	fb.reapFailureStatus = http.StatusInternalServerError
	fb.mu.Unlock()

	// The stub's handshake files. `ready` proves the child is alive and has NOT
	// yet emitted its failure line; `gate` releases it to exit.
	handshakeDir := t.TempDir()
	readyPath := filepath.Join(handshakeDir, "ready")
	gatePath := filepath.Join(handshakeDir, "gate")

	// The stub's own wedge is bounded so a test that fails before opening the
	// gate leaks no lingering process; the bound derives from timescale.D like
	// every other duration here (AGENTS.md / #1984), polled at an unscaled 50ms.
	stubWedgeTicks := int(timescale.D(30*time.Second) / (50 * time.Millisecond))
	stub := filepath.Join(t.TempDir(), "stub-runner")
	script := "#!/bin/sh\n" +
		": > '" + readyPath + "'\n" +
		"i=0\n" +
		"while [ ! -f '" + gatePath + "' ]; do\n" +
		"  i=$((i+1))\n" +
		"  [ \"$i\" -gt " + fmt.Sprintf("%d", stubWedgeTicks) + " ] && break\n" +
		"  sleep 0.05 2>/dev/null || sleep 1\n" +
		"done\n" +
		`echo '{"event":"runner_failed","reason":"agent_invocation_failed","detail":"boom"}'` + "\n" +
		"exit 7\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	in := runChildrenIn(parent)
	in.RunnerBinary = stub
	_, out, err := r.runChildren(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != 1 {
		t.Fatalf("dispatched_count = %d, want 1", out.DispatchedCount)
	}
	t.Cleanup(func() { _, _ = detachedRunners.terminateRunners(child.String(), nil) })

	stageID := stageIDOf(fb, child)

	// ORDERING STEP 1: wait for the child to announce itself. Past this point the
	// child is alive and wedged BEFORE its runner_failed line, so it has not
	// exited and its reaper has not run.
	readyDeadline := time.Now().Add(timescale.D(10 * time.Second))
	for {
		if _, statErr := os.Stat(readyPath); statErr == nil {
			break
		}
		if time.Now().After(readyDeadline) {
			t.Fatalf("stub runner never touched %s — the child was not spawned", readyPath)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// ORDERING STEP 2: model the runner's signed prompt fetch, the only
	// transition that flips a stage dispatched -> running. The child never
	// settles it further.
	fb.mu.Lock()
	for i := range fb.stagesByRun[child] {
		fb.stagesByRun[child][i].State = "running"
	}
	attemptsAtFlip := fb.reapFailureAttempts
	fb.mu.Unlock()

	// ORDERING STEP 3: the positive proof that the flip PRECEDED the exit. A
	// reaper attempt can only exist once the child has exited, so zero attempts
	// at the instant of the flip means the 'running' state was in place before
	// the child could produce the strand — it is not manufactured afterwards.
	if attemptsAtFlip != 0 {
		t.Fatalf("reap-failure attempts at the dispatched->running flip = %d, want 0 — the child exited before the transition, so the strand below would be manufactured, not produced", attemptsAtFlip)
	}

	// ORDERING STEP 4: release the child to emit runner_failed and exit 7.
	if err := os.WriteFile(gatePath, nil, 0o600); err != nil {
		t.Fatalf("open the stub runner's gate: %v", err)
	}

	// Wait for the reaper's retry loop to EXHAUST: one initial attempt plus one
	// per backoff step. Only then is the strand residual rather than in-flight.
	wantAttempts := 1 + len(reapReportBackoff)
	deadline := time.Now().Add(timescale.D(10 * time.Second))
	for {
		fb.mu.Lock()
		attempts := fb.reapFailureAttempts
		fb.mu.Unlock()
		if attempts >= wantAttempts {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the reaper made %d reap-failure attempts, want %d — its retry loop never exhausted", attempts, wantAttempts)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// THE STRAND, produced by a real non-zero child exit: no runner, no reaper,
	// and every other recovery verb refuses a stage in this state.
	if got := stageStateOf(fb, child); got != "running" {
		t.Fatalf("stage = %q, want the produced 'running' strand", got)
	}

	// RECOVERY: the backend is healthy again and the operator reaps.
	fb.mu.Lock()
	fb.reapFailureStatus = http.StatusOK
	fb.mu.Unlock()

	_, reaped, err := r.reapStage(context.Background(), nil, ReapStageInput{
		RunID: child.String(), StageID: stageID,
	})
	if err != nil {
		t.Fatalf("fishhawk_reap_stage on the stranded stage: %v", err)
	}
	if !reaped.Transitioned {
		t.Error("transitioned = false, want true — the verb did not clear the strand")
	}
	if reaped.StageState != "failed" {
		t.Errorf("stage_state = %q, want failed", reaped.StageState)
	}
	if !strings.Contains(reaped.NextStep, "fishhawk_retry_stage") {
		t.Errorf("next_step must name fishhawk_retry_stage: %q", reaped.NextStep)
	}
	if got := stageStateOf(fb, child); got != "failed" {
		t.Errorf("committed stage state = %q, want failed (the state fishhawk_retry_stage admits)", got)
	}
	// Identity: the ACCEPTED body is the operator's, not a late reaper attempt's.
	stageUUID, perr := uuid.Parse(stageID)
	if perr != nil {
		t.Fatalf("stage id %q: %v", stageID, perr)
	}
	fb.mu.Lock()
	accepted := append([]reapFailureRequest(nil), fb.reapFailureByStage[stageUUID]...)
	fb.mu.Unlock()
	if len(accepted) != 1 {
		t.Fatalf("accepted reap-failure bodies = %d, want exactly 1 (the operator's)", len(accepted))
	}
	if accepted[0].Reason != reapStageDefaultReason {
		t.Errorf("accepted reason = %q, want the operator default %q — the recovery was not this verb's",
			accepted[0].Reason, reapStageDefaultReason)
	}
}

// --- the per-invocation dispatch budget ---

func TestRunChildren_BudgetPartialSpawnsExactlyOne(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent := uuid.New()
	a, b, live := uuid.New(), uuid.New(), uuid.New()
	seedChildRun(fb, a, "pending")
	seedChildRun(fb, b, "pending")
	seedChildRunStage(fb, live, "running", "running") // one slot already spent
	seedPlanDecomposed(fb, parent, []string{a.String(), b.String(), live.String()}, 2)
	spawn := withFakeDetachedSpawn(t)

	_, out, err := r.runChildren(context.Background(), nil, runChildrenIn(parent))
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != 1 {
		t.Fatalf("dispatched_count = %d, want 1 (cap 2 minus 1 in flight)", out.DispatchedCount)
	}
	if got := spawn.runIDs(); len(got) != 1 || got[0] != a.String() {
		t.Errorf("spawned %v, want only the first dispatchable child %s", got, a)
	}
	if !containsWarning(out.Warnings, b.String()) {
		t.Errorf("warnings = %v, want the deferred child named", out.Warnings)
	}
	if !containsWarning(out.Warnings, "PER-INVOCATION DISPATCH BUDGET") {
		t.Errorf("warnings = %v, want the honest cap disclosure", out.Warnings)
	}
}

func TestRunChildren_BudgetExhaustedSpawnsNothing(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent, pend, live := uuid.New(), uuid.New(), uuid.New()
	seedChildRun(fb, pend, "pending")
	seedChildRunStage(fb, live, "running", "dispatched")
	seedPlanDecomposed(fb, parent, []string{pend.String(), live.String()}, 1)
	spawn := withFakeDetachedSpawn(t)

	_, out, err := r.runChildren(context.Background(), nil, runChildrenIn(parent))
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != 0 || len(spawn.snapshot()) != 0 {
		t.Fatalf("dispatched=%d spawns=%d, want 0/0", out.DispatchedCount, len(spawn.snapshot()))
	}
	if !containsWarning(out.Warnings, pend.String()) {
		t.Errorf("warnings = %v, want the undispatched child named", out.Warnings)
	}
}

func TestRunChildren_UnlimitedCapSpawnsEveryDispatchableChild(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent := uuid.New()
	ids := []string{}
	for i := 0; i < 4; i++ {
		c := uuid.New()
		seedChildRun(fb, c, "pending")
		ids = append(ids, c.String())
	}
	seedPlanDecomposed(fb, parent, ids, 0) // 0 == unlimited
	spawn := withFakeDetachedSpawn(t)

	_, out, err := r.runChildren(context.Background(), nil, runChildrenIn(parent))
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != 4 || len(spawn.snapshot()) != 4 {
		t.Fatalf("dispatched=%d spawns=%d, want 4/4", out.DispatchedCount, len(spawn.snapshot()))
	}
	if out.EffectiveCap != 0 {
		t.Errorf("effective_cap = %d, want 0 (unlimited)", out.EffectiveCap)
	}
}

// --- THE REAL DECODE BOUNDARY (#2660 precedent) ---

// TestRunChildren_DecodesServerBaseBranchIntoArgv stands up an httptest handler
// that writes THE RAW BYTES of the fixture the SERVER test proves is its own
// wire form (backend/internal/server/testdata/host_dispatch_dependent_child.json
// — the file, not a re-marshal), points a REAL apiClient at it, drives the real
// runChildren, and asserts the composed argv carries the FIXTURE's base_branch
// on both --base-branch and --check-base-ref, and NOT the input base_branch.
//
// This is the assertion the package's own fakeBackend cannot make: it MARSHALS
// the client's own HostDispatchResult, so a wrong or missing json tag
// round-trips symmetrically and every mode test stays green.
func TestRunChildren_DecodesServerBaseBranchIntoArgv(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "server", "testdata", "host_dispatch_dependent_child.json"))
	if err != nil {
		t.Fatalf("read wire fixture: %v", err)
	}
	var want struct {
		BaseBranch string `json:"base_branch"`
	}
	if err := json.Unmarshal(fixture, &want); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if want.BaseBranch == "" {
		t.Fatal("fixture carries no base_branch — the boundary would be untested")
	}

	parent, child := uuid.New(), uuid.New()
	stageID := uuid.New()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/runs/{run_id}/audit", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.PathValue("run_id") != parent.String() {
			_, _ = w.Write([]byte(`{"items":[]}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{
			"id": uuid.NewString(), "sequence": 1, "run_id": parent.String(),
			"category": "plan_decomposed",
			"payload":  map[string]any{"child_run_ids": []string{child.String()}, "effective_max_parallel": 0},
		}}})
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/stages", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{
			"id": stageID.String(), "run_id": child.String(), "type": "implement", "state": "pending",
		}}})
	})
	// The parent's single-run read: resolveRunForgeTarget (E45.46 / #3463)
	// REFUSES on an unreadable parent, so this fixture-driven mux serves a plain
	// github row for it (forge omitted → github; no forge flags on the argv).
	mux.HandleFunc("GET /v0/runs/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Run{ID: r.PathValue("run_id"), Repo: "x/y", State: "running", RunnerKind: "local"})
	})
	// THE BOUNDARY: the raw fixture bytes, served verbatim.
	mux.HandleFunc("POST /v0/runs/{run_id}/stages/{stage_id}/host-dispatch", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	r := newResolver(srv, nil)
	spawn := withFakeDetachedSpawn(t)
	in := runChildrenIn(parent)
	in.BaseBranch = "main-from-the-input"
	if _, _, err := r.runChildren(context.Background(), nil, in); err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	calls := spawn.snapshot()
	if len(calls) != 1 {
		t.Fatalf("spawn calls = %d, want 1", len(calls))
	}
	for _, flag := range []string{"--base-branch", "--check-base-ref"} {
		if got := argvFlag(calls[0].argv, flag); got != want.BaseBranch {
			t.Errorf("%s = %q, want the SERVER's %q (not the input's)", flag, got, want.BaseBranch)
		}
	}
}

// TestRunChildren_FallsBackToInputBaseWhenServerReturnsNone pins the other half:
// an empty base_branch means "keep the base you already had".
func TestRunChildren_FallsBackToInputBaseWhenServerReturnsNone(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent, child := uuid.New(), uuid.New()
	seedChildRun(fb, child, "pending")
	seedPlanDecomposed(fb, parent, []string{child.String()}, 0)
	spawn := withFakeDetachedSpawn(t)

	in := runChildrenIn(parent)
	in.BaseBranch = "release/x"
	if _, _, err := r.runChildren(context.Background(), nil, in); err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	calls := spawn.snapshot()
	if len(calls) != 1 {
		t.Fatalf("spawn calls = %d, want 1", len(calls))
	}
	if got := argvFlag(calls[0].argv, "--base-branch"); got != "release/x" {
		t.Errorf("--base-branch = %q, want the input fallback release/x", got)
	}
}

// TestRunChildren_RegistersChildrenForCancelReap makes the #2679 subsumption
// TESTABLE rather than asserted. It deliberately uses the REAL
// spawnRunnerStageDetached (a faked seam registers nothing and the assertion
// would be vacuous) against a sleeping /bin/sh stub, then asserts
// detachedRunners.terminateRunners reports the child terminated — which IS the
// capability #2679 says run_children lacks today.
func TestRunChildren_RegistersChildrenForCancelReap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub runner uses /bin/sh")
	}
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent, child := uuid.New(), uuid.New()
	seedChildRun(fb, child, "pending")
	seedPlanDecomposed(fb, parent, []string{child.String()}, 0)

	stub := filepath.Join(t.TempDir(), "stub-runner")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	in := runChildrenIn(parent)
	in.RunnerBinary = stub
	_, out, err := r.runChildren(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != 1 {
		t.Fatalf("dispatched_count = %d, want 1", out.DispatchedCount)
	}
	terminated, _ := detachedRunners.terminateRunners(child.String(), nil)
	t.Cleanup(func() { _, _ = detachedRunners.terminateRunners(child.String(), nil) })
	if terminated == 0 {
		t.Fatal("terminateRunners reaped 0 runners for the child — run_children's spawns are not registered (the #2679 gap)")
	}
}

// --- THE BINDING MID-FAN-OUT TEST (issue done-means) ---

// modelledRunner stands in for a live detached child. It files a scope
// amendment, then POLLS its own amendment status back THROUGH the backend and
// proceeds ONLY after it observes a DECIDED status. Every status it reads is
// recorded, so the test can assert the child actually observed the transition
// rather than being flipped by hand.
type modelledRunner struct {
	mu           sync.Mutex
	observations []string
	filed        chan struct{}
	proceeded    chan struct{}
}

const modelledRunnerTimeoutSentinel = "TIMED_OUT_STILL_PENDING"

func (m *modelledRunner) record(s string) {
	m.mu.Lock()
	m.observations = append(m.observations, s)
	m.mu.Unlock()
}

func (m *modelledRunner) seen() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.observations...)
}

// TestFanOut_ChildAmendmentParkedAndDecidedMidFanOut is the issue's binding
// done-means test, and it proves CAUSALITY rather than asserting it.
//
// FIXTURE / INTEGRATION STEP (binding condition 1). Child A is slice 0; child B
// is slice 1 with depends_on [0]. await_children releases children_dispatchable
// for B only once the newest slices_integrated entry COVERS A — and nothing in
// an MCP-package test runs the sweeper or orchestrator.IntegrateCompletedWave.
// So the fixture SEEDS THE slices_integrated ENTRY covering A explicitly, at the
// point in the sequence where the server-side between-wave integration would
// have landed it. That is the choice made here (the alternative — driving
// IntegrateCompletedWave — would require the orchestrator's repository stack in
// a tool-level test). The seeding goes through slicesIntegratedPayloadMap, whose
// key names are reflected from the decoder and separately proven to match the
// real emitter, so the seeded shape is not hand-written.
func TestFanOut_ChildAmendmentParkedAndDecidedMidFanOut(t *testing.T) {
	run := func(t *testing.T, decide bool) {
		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		r.reviewPollInterval = time.Millisecond

		parent, childA, childB := uuid.New(), uuid.New(), uuid.New()
		stageA := seedChildWithSlice(fb, childA, "pending", "awaiting_host_dispatch", 0, nil)
		stageB := seedChildWithSlice(fb, childB, "pending", "awaiting_host_dispatch", 1, []int{0})
		seedPlanDecomposed(fb, parent, []string{childA.String(), childB.String()}, 0)
		fb.mu.Lock()
		fb.decideFlipsListStatus = true
		// childB depends on slice 0, which has not integrated at fan-out time, so
		// the SERVER refuses its host dispatch (wave_not_integrated) and childB's
		// implement stage stays awaiting_host_dispatch — the state its later
		// dispatchable release is keyed on (#1237). Without this the fake would
		// over-transition childB to 'dispatched' at the initial fan-out and it
		// could never become dispatchable again.
		fb.hostDispatchWaveNotIntegrated[stageB.String()] = true
		fb.mu.Unlock()

		m := &modelledRunner{filed: make(chan struct{}), proceeded: make(chan struct{})}
		amendmentID := uuid.New()
		spawn := withFakeDetachedSpawn(t)
		spawn.onCall = func(c detachedSpawnCall) {
			if c.runID != childA.String() {
				return
			}
			go func() {
				// (1) File the amendment through the fake backend, then signal so
				// the test never races it.
				fb.mu.Lock()
				fb.amendmentsByRun[childA] = []ScopeAmendmentItem{{
					ID: amendmentID.String(), RunID: childA.String(), StageID: stageA.String(),
					Status: "pending", Reason: "a coupled test file",
					Paths: []ScopeAmendmentPath{{Path: "x/y_test.go", Operation: "create"}},
				}}
				fb.mu.Unlock()
				close(m.filed)

				// (2) POLL the decision back THROUGH the backend, recording every
				// observed status. (3) Proceed only on a non-pending status.
				deadline := time.Now().Add(timescale.D(5 * time.Second))
				for time.Now().Before(deadline) {
					items, err := r.api.ListScopeAmendments(context.Background(), childA)
					if err == nil {
						for _, it := range items {
							if it.ID != amendmentID.String() {
								continue
							}
							m.record(it.Status)
							if it.Status != "pending" {
								// The ONLY writer of A's terminal stage state on
								// this path. The test never writes it.
								fb.mu.Lock()
								for i := range fb.stagesByRun[childA] {
									fb.stagesByRun[childA][i].State = "succeeded"
								}
								row := fb.getRunByID[childA]
								row.State = "succeeded"
								fb.getRunByID[childA] = row
								fb.mu.Unlock()
								close(m.proceeded)
								return
							}
						}
					}
					time.Sleep(2 * time.Millisecond)
				}
				// (4) Deadline elapsed with the status still pending: record the
				// sentinel and release the test WITHOUT flipping the stage, so the
				// assertions fail RED instead of the test hanging.
				m.record(modelledRunnerTimeoutSentinel)
				close(m.proceeded)
			}()
		}

		// THE PROPERTY THIS WHOLE CHANGE EXISTS TO CREATE: run_children RETURNS
		// while the fan-out is live and the session is free.
		_, out, err := r.runChildren(context.Background(), nil, runChildrenIn(parent))
		if err != nil {
			t.Fatalf("runChildren: %v", err)
		}
		ca := childByID(out)[childA.String()]
		if !ca.Dispatched || ca.LogPath == "" {
			t.Fatalf("child A = %+v, want dispatched with a log_path", ca)
		}
		if st := stageStateOf(fb, childA); st == "succeeded" || st == "failed" {
			t.Fatalf("child A's stage is already terminal (%q) when runChildren returned — the call awaited it", st)
		}

		<-m.filed

		_, rel, err := r.awaitChildren(context.Background(), nil, awaitIn(parent))
		if err != nil {
			t.Fatalf("awaitChildren: %v", err)
		}
		if rel.Status != "amendment_pending" || rel.PendingAmendment == nil ||
			rel.PendingAmendment.ID != amendmentID.String() {
			t.Fatalf("await release = %q / %+v, want amendment_pending carrying A's row", rel.Status, rel.PendingAmendment)
		}
		if rel.NextStep == nil || rel.NextStep.Action != "fishhawk_decide_scope_amendment" {
			t.Fatalf("next_step = %+v, want a pre-filled decide call", rel.NextStep)
		}

		if decide {
			// Drive the REAL decide handler with the next_step's OWN arguments,
			// unmarshalled from the returned SuggestedAction and never hand-built —
			// this is what proves the handoff works.
			if _, _, derr := r.decideScopeAmendment(context.Background(), nil, DecideScopeAmendmentInput{
				RunID:       rel.NextStep.Params["run_id"],
				AmendmentID: rel.NextStep.Params["amendment_id"],
				Decision:    "approve",
			}); derr != nil {
				t.Fatalf("decide: %v", derr)
			}
		}

		<-m.proceeded
		seen := m.seen()

		if !decide {
			// THE PAIRED CAUSAL COUNTERFACTUAL: no decision, so the child must NOT
			// proceed.
			if len(seen) == 0 || seen[len(seen)-1] != modelledRunnerTimeoutSentinel {
				t.Fatalf("observations = %v, want the timeout sentinel last (the child must not proceed undecided)", seen)
			}
			for _, s := range seen[:len(seen)-1] {
				if s != "pending" {
					t.Errorf("observed %q with no decision made; every reading must be pending: %v", s, seen)
				}
			}
			if st := stageStateOf(fb, childA); st == "succeeded" {
				t.Errorf("child A's stage is succeeded with no decision — something other than the modelled runner wrote it")
			}
			return
		}

		// (a) The modelled runner read a DECIDED status back through the backend.
		last := seen[len(seen)-1]
		if last == "pending" || last == modelledRunnerTimeoutSentinel {
			t.Fatalf("final observation = %q, want a decided status — the child never saw the decision", last)
		}
		// (b) It observed a PENDING reading first, so the decided read is a
		// transition it actually observed, not a first-read artifact.
		sawPending := false
		for _, s := range seen[:len(seen)-1] {
			if s == "pending" {
				sawPending = true
			}
		}
		if !sawPending {
			t.Errorf("observations = %v, want at least one 'pending' reading BEFORE the decided one", seen)
		}
		// (c) A's stage is succeeded, and the TEST NEVER WROTE THAT STATE: the
		// only writer is the modelled runner's step (3), unreachable without (a).
		if st := stageStateOf(fb, childA); st != "succeeded" {
			t.Errorf("child A stage = %q, want succeeded (written only by the modelled runner)", st)
		}

		// (d) THE INTEGRATION STEP, seeded explicitly: the between-wave
		// integration that would otherwise be the sweeper's job now covers A.
		seedSlicesIntegrated(t, fb, parent, "fishhawk/run-"+parent.String()+"-consolidated", []string{childA.String()})

		// (e) The wave advanced: a subsequent await no longer releases on A's
		// amendment and instead releases children_dispatchable for B.
		_, rel2, err := r.awaitChildren(context.Background(), nil, awaitIn(parent))
		if err != nil {
			t.Fatalf("second awaitChildren: %v", err)
		}
		if rel2.Status != "children_dispatchable" {
			t.Fatalf("second release = %q, want children_dispatchable for B", rel2.Status)
		}
		if len(rel2.DispatchableChildRunIDs) != 1 || rel2.DispatchableChildRunIDs[0] != childB.String() {
			t.Errorf("dispatchable = %v, want [%s]", rel2.DispatchableChildRunIDs, childB)
		}
	}

	t.Run("decision_reaches_child_and_it_proceeds", func(t *testing.T) { run(t, true) })
	t.Run("no_decision_child_does_not_proceed", func(t *testing.T) { run(t, false) })
}

// --- forge target (E45.46 / #3463) -------------------------------------------

// seedGitLabDecomposition seeds a gitlab PARENT (served on the single-run route
// with the given base URL) and two parked local children that inherit the
// parent's forge server-side (run.ChildParamsFrom copies installation_ref), so
// each child row reads forge gitlab too. Children carry NO forge_base_url —
// the parent's single-run read is the authority for every child argv.
func seedGitLabDecomposition(fb *fakeBackend, parent uuid.UUID, baseURL string) []uuid.UUID {
	seedGitLabRun(fb, parent, "acme/platform/widgets", baseURL)
	children := []uuid.UUID{uuid.New(), uuid.New()}
	for _, c := range children {
		seedChildRunStage(fb, c, "running", "awaiting_host_dispatch")
		fb.mu.Lock()
		row := fb.getRunByID[c]
		row.Repo = "acme/platform/widgets"
		row.Forge = "gitlab"
		fb.getRunByID[c] = row
		fb.mu.Unlock()
	}
	ids := make([]string, 0, len(children))
	for _, c := range children {
		ids = append(ids, c.String())
	}
	seedPlanDecomposed(fb, parent, ids, 0)
	return children
}

// TestRunChildren_GitLabParent_ChildArgvCarriesForgeFlags is the argv half of
// approval-plan constraint 4: every inline child argv of a gitlab parent
// carries `--forge gitlab --gitlab-base-url <url>` right after `--github-repo
// <parent path_with_namespace>` (github_repo omitted → the parent row's repo;
// the github.com-only origin detector is never consulted).
func TestRunChildren_GitLabParent_ChildArgvCarriesForgeFlags(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeGitRemote(t, "git@gitlab.example.com:acme/platform/widgets.git", nil)
	parent := uuid.New()
	children := seedGitLabDecomposition(fb, parent, "https://gitlab.example.com")
	spawn := withFakeDetachedSpawn(t)

	// A gitlab parent reports forge_unsupported, so base_branch is passed (#4758).
	_, out, err := r.runChildren(context.Background(), nil, RunChildrenInput{
		RunID: parent.String(), Workflow: "wf", RunnerBinary: "/fake/fishhawk-runner", BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if out.DispatchedCount != len(children) {
		t.Fatalf("dispatched_count = %d, want %d; children=%+v", out.DispatchedCount, len(children), out.Children)
	}
	calls := spawn.snapshot()
	if len(calls) != len(children) {
		t.Fatalf("spawned %d children, want %d", len(calls), len(children))
	}
	for _, c := range calls {
		if !argvHasPair(c.argv, "--github-repo", "acme/platform/widgets") {
			t.Errorf("child %s argv should default --github-repo from the parent row: %v", c.runID, c.argv)
		}
		if !argvHasPair(c.argv, "--forge", "gitlab") || !argvHasPair(c.argv, "--gitlab-base-url", "https://gitlab.example.com") {
			t.Errorf("child %s argv should carry the gitlab forge flags: %v", c.runID, c.argv)
		}
		// Position parity with composeRunnerArgv: the forge flags follow
		// --github-repo immediately and precede --base-branch.
		joined := strings.Join(c.argv, " ")
		if !strings.Contains(joined, "--github-repo acme/platform/widgets --forge gitlab --gitlab-base-url https://gitlab.example.com --base-branch") {
			t.Errorf("child %s forge flags are not positioned right after --github-repo: %v", c.runID, c.argv)
		}
	}
}

// TestRunChildren_ForgeTarget_ReadViaSingleRunGet: the child spawns carried the
// base URL the fake serves ONLY on the parent's single-run route (the child
// rows and the list route omit it), and the list route was never read.
// Counterfactual: derive the target from a list read (or from the child rows)
// and this goes RED on the base-URL refusal.
func TestRunChildren_ForgeTarget_ReadViaSingleRunGet(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent := uuid.New()
	seedGitLabDecomposition(fb, parent, "https://gitlab.example.com")
	fb.mu.Lock()
	fb.listResp = listRunsResult{Items: []Run{fb.getRunByID[parent]}}
	fb.mu.Unlock()
	spawn := withFakeDetachedSpawn(t)

	// A gitlab parent reports forge_unsupported, so base_branch is passed (#4758).
	in := runChildrenIn(parent)
	in.BaseBranch = "main"
	if _, _, err := r.runChildren(context.Background(), nil, in); err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	calls := spawn.snapshot()
	if len(calls) == 0 {
		t.Fatal("no child spawned")
	}
	for _, c := range calls {
		if !argvHasPair(c.argv, "--gitlab-base-url", "https://gitlab.example.com") {
			t.Fatalf("child %s spawn did not carry the parent's single-run-route base URL: %v", c.runID, c.argv)
		}
	}
	fb.mu.Lock()
	gets, lists := fb.getRunCalledByID[parent], fb.listRunCalls
	fb.mu.Unlock()
	if gets < 1 || lists != 0 {
		t.Fatalf("parent single-run GETs = %d, list GETs = %d; the forge target must come from the parent's single-run route only", gets, lists)
	}
}

// TestRunChildren_GitLabParent_NoBaseURL_RefusesBeforeAnyMarker: a gitlab
// parent with no forge_base_url refuses the whole fan-out — no marker POST on
// any child and no spawn. Counterfactual: delete the empty-ForgeBaseURL
// refusal in resolveRunForgeTarget and this goes RED.
func TestRunChildren_GitLabParent_NoBaseURL_RefusesBeforeAnyMarker(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent := uuid.New()
	seedGitLabDecomposition(fb, parent, "")
	spawn := withFakeDetachedSpawn(t)

	_, _, err := r.runChildren(context.Background(), nil, runChildrenIn(parent))
	if err == nil || !strings.Contains(err.Error(), "forge_base_url") {
		t.Fatalf("expected the empty-base-URL refusal; got %v", err)
	}
	if calls := spawn.snapshot(); len(calls) != 0 {
		t.Fatalf("spawned %d children despite the refusal", len(calls))
	}
	fb.mu.Lock()
	markers := 0
	for _, n := range fb.hostDispatchCalledByID {
		markers += n
	}
	fb.mu.Unlock()
	if markers != 0 {
		t.Fatalf("host-dispatch marker POSTed %d times before the refusal", markers)
	}
}

// TestRunChildren_GitHubParent_ArgvUnchanged: a github parent (no forge key —
// the older-backend shape) composes the pre-#3463 child argv with no forge flag.
func TestRunChildren_GitHubParent_ArgvUnchanged(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent := uuid.New()
	child := uuid.New()
	seedChildRunStage(fb, child, "running", "awaiting_host_dispatch")
	seedPlanDecomposed(fb, parent, []string{child.String()}, 0)
	spawn := withFakeDetachedSpawn(t)

	if _, _, err := r.runChildren(context.Background(), nil, runChildrenIn(parent)); err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	calls := spawn.snapshot()
	if len(calls) != 1 {
		t.Fatalf("spawned %d children, want 1", len(calls))
	}
	for _, tok := range calls[0].argv {
		if tok == "--forge" || tok == "--gitlab-base-url" {
			t.Fatalf("github child argv must carry no forge flag: %v", calls[0].argv)
		}
	}
	if !strings.Contains(strings.Join(calls[0].argv, " "), "--github-repo x/y --base-branch main") {
		t.Fatalf("github child argv shape changed: %v", calls[0].argv)
	}
}

// --- base branch (#4758) -----------------------------------------------------

// seedParentDefaultBranch seeds the PARENT's single-run read with the #4660
// base-branch fields.
func seedParentDefaultBranch(fb *fakeBackend, parent uuid.UUID, branch, unresolved string) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.getRunByID[parent] = Run{
		ID: parent.String(), Repo: "x/y", State: "running", RunnerKind: "local",
		DefaultBranch: branch, DefaultBranchUnresolvedReason: unresolved,
	}
}

// TestRunChildren_OmittedBaseBranch_FallsBackToRunDefaultBranch: base_branch
// omitted and the parent reports default_branch "trunk" → a wave-0 child whose
// marker returns NO base gets --base-branch trunk, while a child whose marker
// returns a consolidated base keeps THAT base (server authority, #2363).
func TestRunChildren_OmittedBaseBranch_FallsBackToRunDefaultBranch(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent, plain, dependent := uuid.New(), uuid.New(), uuid.New()
	seedParentDefaultBranch(fb, parent, "trunk", "")
	seedChildRun(fb, plain, "pending")
	seedChildRun(fb, dependent, "pending")
	seedPlanDecomposed(fb, parent, []string{plain.String(), dependent.String()}, 0)
	fb.mu.Lock()
	fb.hostDispatchBaseBranchByStage = map[string]string{fb.stagesByRun[dependent][0].ID: "fishhawk/consolidated-x"}
	fb.mu.Unlock()
	spawn := withFakeDetachedSpawn(t)

	in := runChildrenIn(parent) // no base_branch
	if _, _, err := r.runChildren(context.Background(), nil, in); err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	byRun := map[string][]string{}
	for _, c := range spawn.snapshot() {
		byRun[c.runID] = c.argv
	}
	for _, tc := range []struct {
		child uuid.UUID
		want  string
	}{
		{plain, "trunk"},
		{dependent, "fishhawk/consolidated-x"},
	} {
		argv, ok := byRun[tc.child.String()]
		if !ok {
			t.Fatalf("child %s was not spawned; spawns=%v", tc.child, byRun)
		}
		for _, flag := range []string{"--base-branch", "--check-base-ref"} {
			if got := argvFlag(argv, flag); got != tc.want {
				t.Errorf("child %s %s = %q, want %q", tc.child, flag, got, tc.want)
			}
		}
	}
}

// TestRunChildren_UnresolvedDefaultBranch_RefusesBeforeAnyMarker: base_branch
// omitted and the parent's default branch is unresolved → a refusal naming the
// reason, with a dispatchable wave-0 child left unmarked and unspawned.
func TestRunChildren_UnresolvedDefaultBranch_RefusesBeforeAnyMarker(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent, child := uuid.New(), uuid.New()
	seedParentDefaultBranch(fb, parent, "", "repository_not_visible")
	seedChildRun(fb, child, "pending") // dispatchable — a deleted control would mark and spawn it
	seedPlanDecomposed(fb, parent, []string{child.String()}, 0)
	spawn := withFakeDetachedSpawn(t)

	_, _, err := r.runChildren(context.Background(), nil, runChildrenIn(parent))
	if err == nil {
		t.Fatal("expected a base_branch_unresolved refusal")
	}
	for _, want := range []string{"base_branch_unresolved", "repository_not_visible", "Pass base_branch explicitly"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should name %q: %v", want, err)
		}
	}
	fb.mu.Lock()
	markers := 0
	for _, n := range fb.hostDispatchCalledByID {
		markers += n
	}
	fb.mu.Unlock()
	if markers != 0 {
		t.Fatalf("host-dispatch markers = %d; the refusal must land before any marker", markers)
	}
	if calls := spawn.snapshot(); len(calls) != 0 {
		t.Fatalf("spawned %d children despite the refusal", len(calls))
	}
}

// --- queued children (#3969 / ADR-094 D1) -----------------------------------

// queuedRunChildrenSeams installs the recording detached-spawn seam and
// shrinks the slot waiter's poll interval and cap BEFORE the queued call,
// because the call reads them synchronously (approval condition Q6). Its last
// cleanup, which runs first, waits for every waiter to exit before the seams
// are restored.
func queuedRunChildrenSeams(t *testing.T) *fakeDetachedSpawn {
	t.Helper()
	spawn := withFakeDetachedSpawn(t)
	savedPoll, savedCap := concurrencySlotPollInterval, concurrencyWaiterCap
	concurrencySlotPollInterval = 5 * time.Millisecond
	concurrencyWaiterCap = timescale.D(20 * time.Second)
	t.Cleanup(func() { concurrencySlotPollInterval, concurrencyWaiterCap = savedPoll, savedCap })
	t.Cleanup(func() { waitSlotWaitersDrained(t) })
	return spawn
}

// captureSlotWaiterStarts stubs startSlotWaiter so no waiter goroutine polls
// the fake; it records every spec a queued arm hands it.
func captureSlotWaiterStarts(t *testing.T) *[]slotWaiterSpec {
	t.Helper()
	var mu sync.Mutex
	captured := &[]slotWaiterSpec{}
	saved := startSlotWaiter
	startSlotWaiter = func(spec slotWaiterSpec) (string, <-chan struct{}) {
		mu.Lock()
		defer mu.Unlock()
		*captured = append(*captured, spec)
		return slotWaiterStarted, nil
	}
	t.Cleanup(func() { startSlotWaiter = saved })
	return captured
}

// TestRunChildren_QueuedChildMarkedQueued (#3969, approval condition Q2): a
// child whose host-dispatch marker answers 409 concurrency_slot_queued is NOT
// a failure. It is reported queued with its queue block and a started waiter,
// NO runner is spawned at return, and the child spends one unit of the
// per-invocation budget exactly like a spawned child. With effective_cap 1 and
// two dispatchable children that would both queue, only ONE waiter starts and
// the other child is named in the budget warning.
func TestRunChildren_QueuedChildMarkedQueued(t *testing.T) {
	fb, srv := newFakeBackend(t)
	sc, r := newQueuedMarkerResolver(t, srv)
	spawn := queuedRunChildrenSeams(t)
	starts := captureSlotWaiterStarts(t)
	parent, a, b := uuid.New(), uuid.New(), uuid.New()
	seedChildRun(fb, a, "awaiting_host_dispatch")
	seedChildRun(fb, b, "awaiting_host_dispatch")
	seedPlanDecomposed(fb, parent, []string{a.String(), b.String()}, 1)

	_, out, err := r.runChildren(context.Background(), nil, runChildrenIn(parent))
	if err != nil {
		t.Fatalf("runChildren on a queued marker: %v (a queued child is data, not a tool error)", err)
	}
	if len(*starts) != 1 {
		t.Fatalf("slot waiters started = %d, want exactly 1 (effective_cap 1: a queued child spends the budget)", len(*starts))
	}
	spec := (*starts)[0]
	if spec.verb != "fishhawk_run_children" || spec.runID != a || spec.stageID.String() != stageIDOf(fb, a) ||
		spec.poll != 5*time.Millisecond || spec.spawn == nil || spec.report == nil || spec.probe == nil || spec.prepare == nil {
		t.Fatalf("waiter spec = %+v, want child %s's stage, the shrunk poll and the detached spawn/report/probe/prepare", spec, a)
	}
	if out.QueuedCount != 1 || out.DispatchedCount != 0 {
		t.Fatalf("queued_count = %d, dispatched_count = %d; want 1 and 0", out.QueuedCount, out.DispatchedCount)
	}
	ca := childByID(out)[a.String()]
	if !ca.Queued || ca.Dispatched || ca.LogPath != "" || ca.StageState != "awaiting_host_dispatch" {
		t.Fatalf("child a = %+v, want queued, not dispatched, no log path, stage awaiting_host_dispatch", ca)
	}
	slot := ca.ConcurrencySlot
	if slot == nil || slot.Status != concurrencyStatusAwaitingSlot || slot.Group != "local-implement:h1" ||
		slot.Position != 2 || slot.Waiter != slotWaiterStarted {
		t.Fatalf("child a concurrency_slot = %+v, want the queued block with a started waiter", slot)
	}
	if w := warningContaining(ca.Warnings, "NO runner was spawned"); w == "" || !strings.Contains(w, "re-invoke fishhawk_run_children") {
		t.Errorf("child a warnings = %v, want the queued warning naming fishhawk_run_children as the re-queue", ca.Warnings)
	}
	cb := childByID(out)[b.String()]
	if cb.Queued || cb.Dispatched {
		t.Errorf("child b = %+v, want neither queued nor dispatched (the budget was spent)", cb)
	}
	budget := warningContaining(out.Warnings, "per-invocation dispatch budget")
	if !strings.Contains(budget, b.String()) || strings.Contains(budget, a.String()) {
		t.Errorf("budget warning = %q, want child b named and child a not", budget)
	}
	if n := len(spawn.snapshot()); n != 0 {
		t.Fatalf("detached spawns at return = %d, want 0 while queued", n)
	}
	if sc.queuedAnswers() != 1 {
		t.Errorf("queued marker answers = %d, want 1 (child b was never marked)", sc.queuedAnswers())
	}
	if out.NextStep == nil || out.NextStep.Action != "fishhawk_await_children" || out.NextStep.Params["run_id"] != parent.String() {
		t.Errorf("next_step = %+v, want fishhawk_await_children on the parent", out.NextStep)
	}
	if got := stageStateOf(fb, a); got != "awaiting_host_dispatch" {
		t.Errorf("child a stage = %q, want awaiting_host_dispatch while queued", got)
	}

	desc := strings.Join(strings.Fields(listToolDescriptions(t)["fishhawk_run_children"]), " ")
	for _, want := range []string{
		"reported queued:true with its concurrency_slot block",
		"spawns the child DETACHED exactly once on admission, against the ADMITTING response's base_branch",
		"a child this call queues spends one unit exactly like a spawned one",
		"does not announce a queued child with a live waiter as dispatchable",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("fishhawk_run_children description missing %q", want)
		}
	}
	if !strings.Contains(runChildrenCapDisclosure, "counts as in flight") || !strings.Contains(runChildrenCapDisclosure, "spends one unit") {
		t.Errorf("runChildrenCapDisclosure = %q, want the queued-child budget rule", runChildrenCapDisclosure)
	}
}

// TestRunChildren_LiveWaiterChildCountsInFlight (approval condition Q2, the
// re-invocation half): a child whose implement stage discovery reads queued
// for a concurrency slot with a LIVE waiter is in flight. It is not marked
// again, it is reported queued, and it debits the budget, so with
// effective_cap 1 the other dispatchable child is deferred. A child whose
// waiter is stale (waiter_live false) is dispatchable and is marked and
// spawned: the in-flight count keys on liveness, not on queued status.
func TestRunChildren_LiveWaiterChildCountsInFlight(t *testing.T) {
	for _, tc := range []struct {
		name string
		live bool
	}{{"live waiter", true}, {"stale waiter", false}} {
		t.Run(tc.name, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			r := newResolver(srv, nil)
			spawn := withFakeDetachedSpawn(t)
			parent, q, b := uuid.New(), uuid.New(), uuid.New()
			seedChildRun(fb, q, "awaiting_host_dispatch")
			seedChildRun(fb, b, "pending")
			live := tc.live
			fb.mu.Lock()
			fb.stagesByRun[q][0].Concurrency = &StageConcurrency{
				Status: concurrencyStatusAwaitingSlot, Group: "local-implement:h1", Limit: 1, Position: 1, WaiterLive: &live,
			}
			fb.mu.Unlock()
			seedPlanDecomposed(fb, parent, []string{q.String(), b.String()}, 1)
			qStage, bStage := uuid.MustParse(stageIDOf(fb, q)), uuid.MustParse(stageIDOf(fb, b))

			_, out, err := r.runChildren(context.Background(), nil, runChildrenIn(parent))
			if err != nil {
				t.Fatalf("runChildren: %v", err)
			}
			fb.mu.Lock()
			qMarkers, bMarkers := fb.hostDispatchCalledByID[qStage], fb.hostDispatchCalledByID[bStage]
			fb.mu.Unlock()
			cq := childByID(out)[q.String()]
			if tc.live {
				if qMarkers != 0 || bMarkers != 0 || len(spawn.snapshot()) != 0 {
					t.Fatalf("markers q=%d b=%d, spawns %d; want none (the live-waiter child fills effective_cap 1)", qMarkers, bMarkers, len(spawn.snapshot()))
				}
				if !cq.Queued || cq.Dispatched || cq.ConcurrencySlot == nil || !cq.ConcurrencySlot.waiterLive() || out.QueuedCount != 1 {
					t.Fatalf("child q = %+v, queued_count %d; want reported queued with its live-waiter block", cq, out.QueuedCount)
				}
				if w := warningContaining(out.Warnings, "per-invocation dispatch budget"); !strings.Contains(w, b.String()) || !strings.Contains(w, "minus 1 already in flight") {
					t.Errorf("budget warning = %q, want child b deferred against 1 in flight", w)
				}
				return
			}
			if qMarkers != 1 || cq.Queued || !cq.Dispatched {
				t.Fatalf("stale-waiter child = %+v after %d markers; want marked and spawned like any dispatchable child", cq, qMarkers)
			}
			if got := spawn.runIDs(); len(got) != 1 || got[0] != q.String() {
				t.Fatalf("spawned %v, want only the stale-waiter child (b deferred by the budget it spent)", got)
			}
		})
	}
}

// TestRunChildren_QueuedChildSpawnsOnAdmittingBaseBranch (#3969): the queued
// child's waiter spawns it exactly ONCE on admission, against the base_branch
// the ADMITTING marker response carries. The queue-time 409 carries no base
// and the fallback is main, so a waiter that rebuilt the argv from anything
// but the admitting response spawns against main. A second invocation while
// the child is still queued finds the live waiter (already_waiting) and
// spawns nothing more.
func TestRunChildren_QueuedChildSpawnsOnAdmittingBaseBranch(t *testing.T) {
	fb, srv := newFakeBackend(t)
	sc, r := newQueuedMarkerResolver(t, srv)
	spawn := queuedRunChildrenSeams(t)
	parent, child := uuid.New(), uuid.New()
	seedChildRun(fb, child, "awaiting_host_dispatch")
	seedPlanDecomposed(fb, parent, []string{child.String()}, 0)
	stageID := stageIDOf(fb, child)
	fb.mu.Lock()
	fb.hostDispatchBaseBranchByStage = map[string]string{stageID: "fishhawk/consolidated-x"}
	fb.mu.Unlock()
	in := runChildrenIn(parent)
	in.BaseBranch = "main"

	_, out, err := r.runChildren(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	c := childByID(out)[child.String()]
	if !c.Queued || c.Dispatched || c.ConcurrencySlot == nil || c.ConcurrencySlot.Waiter != slotWaiterStarted {
		t.Fatalf("child = %+v, want queued with a started waiter", c)
	}
	if n := len(spawn.snapshot()); n != 0 {
		t.Fatalf("detached spawns at return = %d, want 0", n)
	}

	_, out2, err := r.runChildren(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("second runChildren: %v", err)
	}
	if c2 := childByID(out2)[child.String()]; !c2.Queued || c2.ConcurrencySlot == nil || c2.ConcurrencySlot.Waiter != slotWaiterAlreadyWaiting {
		t.Fatalf("second call child = %+v, want queued with waiter already_waiting", c2)
	}

	sc.admit()
	slotWaitFor(t, "the waiter's detached spawn after admission", func() bool { return len(spawn.snapshot()) > 0 })
	waitSlotWaitersDrained(t)
	calls := spawn.snapshot()
	if len(calls) != 1 || calls[0].runID != child.String() || calls[0].stageID != stageID {
		t.Fatalf("detached spawns = %d, want exactly one for the queued child", len(calls))
	}
	for _, flag := range []string{"--base-branch", "--check-base-ref"} {
		if got := argvFlag(calls[0].argv, flag); got != "fishhawk/consolidated-x" {
			t.Errorf("%s = %q, want the ADMITTING response's fishhawk/consolidated-x (not the fallback main)", flag, got)
		}
	}
	if !argvHasPair(calls[0].argv, "--run-id", child.String()) || !argvContains(calls[0].argv, "--parallel-isolate") {
		t.Errorf("spawn argv = %v, want the child's runner argv", calls[0].argv)
	}
	if got := stageStateOf(fb, child); got != "dispatched" {
		t.Errorf("child stage = %q, want dispatched after the waiter's admission", got)
	}
}

// argvContains reports whether argv carries the bare flag.
func argvContains(argv []string, flag string) bool {
	for _, a := range argv {
		if a == flag {
			return true
		}
	}
	return false
}

// TestRunChildren_QueuedPrepareUsesAdmissionTimeInputs (approval condition
// Q3, ADR-087): a queued child's prepare re-resolves the runner binary and
// rebuilds the env with the token CURRENT at admission. Both change after the
// queued call returns and before the waiter is admitted; the detached spawn
// carries the new values, not the queue-time ones.
func TestRunChildren_QueuedPrepareUsesAdmissionTimeInputs(t *testing.T) {
	fb, srv := newFakeBackend(t)
	sc, r := newQueuedMarkerResolver(t, srv)
	spawn := queuedRunChildrenSeams(t)
	starts := captureSlotWaiterStarts(t)
	runnerBin := "/old/fishhawk-runner"
	r.getenv = func(k string) string {
		if k == "FISHHAWK_RUNNER_BIN" {
			return runnerBin
		}
		return ""
	}
	parent, child := uuid.New(), uuid.New()
	seedChildRun(fb, child, "awaiting_host_dispatch")
	seedPlanDecomposed(fb, parent, []string{child.String()}, 0)
	in := runChildrenIn(parent)
	in.RunnerBinary = "" // resolved from FISHHAWK_RUNNER_BIN, which changes below

	if _, out, err := r.runChildren(context.Background(), nil, in); err != nil || out.QueuedCount != 1 || len(*starts) != 1 {
		t.Fatalf("runChildren = queued %d, %v, waiter starts %d; want one queued waiter", out.QueuedCount, err, len(*starts))
	}
	spec := (*starts)[0]

	// Between queue and admission: a new runner binary and a rotated token.
	runnerBin = "/new/fishhawk-runner"
	r.api.token = "tok-rotated"
	sc.admit()
	_, done := slotWaiters.start(spec) // the real waiter, started after the change
	waitDone(t, done)
	calls := spawn.snapshot()
	if len(calls) != 1 {
		t.Fatalf("detached spawns = %d, want exactly one on admission", len(calls))
	}
	if calls[0].binary != "/new/fishhawk-runner" {
		t.Errorf("spawned binary = %q, want the admission-time /new/fishhawk-runner", calls[0].binary)
	}
	if !argvHasPair(calls[0].argv, "--run-id", child.String()) {
		t.Errorf("spawn argv = %v, want the child's argv", calls[0].argv)
	}
	// Never print the env: the process environment may carry a real token.
	hasRotated, hasQueueTime := envHas(calls[0].env, "FISHHAWK_API_TOKEN=tok-rotated"), envHas(calls[0].env, "FISHHAWK_API_TOKEN=tok-test")
	if !hasRotated || hasQueueTime {
		t.Errorf("spawn env: admission-time token present = %v, queue-time token present = %v; want only FISHHAWK_API_TOKEN=tok-rotated", hasRotated, hasQueueTime)
	}
}

// TestRunChildren_AdmissionResolveFailureCompensates: on the immediate path
// the child re-resolves its runner binary after the marker flipped the stage
// to 'dispatched' (childSpawnInputs). A binary that no longer resolves is a
// spawn error: nothing is forked, and the stage is reported category C so it
// is retryable rather than stranded.
func TestRunChildren_AdmissionResolveFailureCompensates(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	spawn := withFakeDetachedSpawn(t)
	withFakeRunnerMissing(t)
	withFakeExecutable(t, t.TempDir(), false)
	var reads int
	r.getenv = func(k string) string {
		if k != "FISHHAWK_RUNNER_BIN" {
			return ""
		}
		reads++
		if reads == 1 { // the up-front resolution only
			return "/fake/fishhawk-runner"
		}
		return ""
	}
	parent, child := uuid.New(), uuid.New()
	seedChildRun(fb, child, "pending")
	seedPlanDecomposed(fb, parent, []string{child.String()}, 0)
	stageID := uuid.MustParse(stageIDOf(fb, child))
	in := runChildrenIn(parent)
	in.RunnerBinary = ""

	_, out, err := r.runChildren(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("runChildren: %v", err)
	}
	if n := len(spawn.snapshot()); n != 0 {
		t.Fatalf("detached spawns = %d, want 0 (the runner binary did not resolve at admission)", n)
	}
	c := childByID(out)[child.String()]
	if c.Dispatched || warningContaining(c.Warnings, "Reported it as a category-C stage failure") == "" {
		t.Fatalf("child = %+v, want not dispatched with the category-C compensation warning", c)
	}
	fb.mu.Lock()
	reports := fb.reapFailureByStage[stageID]
	fb.mu.Unlock()
	if len(reports) != 1 || reports[0].Category != "C" || reports[0].Reason != "runner_spawn_failed" {
		t.Fatalf("reap-failure reports = %+v, want one category-C runner_spawn_failed", reports)
	}
	if got := stageStateOf(fb, child); got != "failed" {
		t.Errorf("child stage = %q, want failed (compensated, retryable)", got)
	}
}
