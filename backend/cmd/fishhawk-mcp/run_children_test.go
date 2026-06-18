package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
// LatestPlanDecomposed discovers the children + effective cap.
func seedPlanDecomposed(fb *fakeBackend, parent uuid.UUID, childIDs []string, effectiveMax int) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	seq := int64(len(fb.perRunAuditByRun[parent]) + 1)
	fb.perRunAuditByRun[parent] = append(fb.perRunAuditByRun[parent], AuditEntry{
		ID:       uuid.NewString(),
		Sequence: seq,
		RunID:    parent.String(),
		Category: "plan_decomposed",
		Payload: map[string]any{
			"child_run_ids":          childIDs,
			"effective_max_parallel": effectiveMax,
		},
	})
}

// seedChildRun seeds a child run row at the given state plus its implement
// stage so GetRun + resolveStageID succeed during discovery.
func seedChildRun(fb *fakeBackend, childID uuid.UUID, state string) {
	stageID := uuid.New()
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.getRunByID[childID] = Run{ID: childID.String(), State: state, Repo: "x/y"}
	fb.stagesByRun[childID] = []Stage{{ID: stageID.String(), RunID: childID.String(), Type: "implement", State: state}}
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
