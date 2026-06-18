package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sync/errgroup"
)

// spawnRunnerStageFn is the spawn seam fishhawk_run_children dispatches each
// child through. Production points it at the real spawnRunnerStage (the same
// process-group SIGKILL core fishhawk_run_stage uses); the concurrency / cap /
// await-all unit tests swap in an in-flight-observing fake. The cross-boundary
// integration test leaves it at the default so it spawns ACTUAL fishhawk-runner
// subprocesses, proving --parallel-isolate flows MCP → runner → git-worktree.
var spawnRunnerStageFn = spawnRunnerStage

// clampMaxParallel resolves the effective concurrency cap for a decomposed
// parent's children: the orchestrator-resolved effective cap, optionally
// clamped DOWN by an operator max_parallel override. It NEVER raises above the
// effective cap (the orchestrator's resolution is authoritative) — it only
// lowers an unlimited or looser cap.
//
// Contract (approval-condition verification mode (e), clamp-DOWN-only):
//   - effective == 0 means UNLIMITED.
//   - override <= 0 means NO override → return effective unchanged.
//   - override > 0 → return override when effective is unlimited (0) OR the
//     override is strictly tighter than effective; otherwise return effective.
//
// So: (0,2) => 2 ; (2,5) => 2 ; (2,0) => 2 ; (0,0) => 0 ; (5,3) => 3 ; (3,3) => 3.
func clampMaxParallel(effective, override int) int {
	if override > 0 {
		if effective == 0 || override < effective {
			return override
		}
		return effective
	}
	return effective
}

// RunChildrenInput is the fishhawk_run_children tool's input schema (E24.4 /
// #1144). run_id is the DECOMPOSED PARENT whose pending children are dispatched.
type RunChildrenInput struct {
	RunID        string `json:"run_id" jsonschema:"the DECOMPOSED PARENT run UUID; the tool discovers its children from the parent's plan_decomposed audit entry"`
	Workflow     string `json:"workflow" jsonschema:"workflow ID matching the run's workflow (passed through to each child's runner)"`
	WorkingDir   string `json:"working_dir,omitempty" jsonschema:"checkout the children run in; defaults to the MCP server's cwd. Each child provisions its OWN per-child worktree under this checkout's shared gitdir (--parallel-isolate), so the operator's tracked tree is untouched"`
	GitHubRepo   string `json:"github_repo,omitempty" jsonschema:"GitHub repo as owner/name; auto-detected from working_dir's origin remote when empty"`
	BaseBranch   string `json:"base_branch,omitempty" jsonschema:"base branch for each child's implement stage; defaults to main"`
	MaxParallel  int    `json:"max_parallel,omitempty" jsonschema:"optional operator concurrency override; clamp-DOWN-only against the orchestrator-resolved effective cap (it can lower an unlimited/looser cap, never raise it). Omit (0) to use the effective cap as-is"`
	RunnerBinary string `json:"runner_binary,omitempty" jsonschema:"path to fishhawk-runner; resolved in order: this input, FISHHAWK_RUNNER_BIN env, fishhawk-runner sibling to this binary, then PATH"`
	Verbose      bool   `json:"verbose,omitempty" jsonschema:"when true, each child result carries the full runner event list including routine heartbeats; default false omits them"`
}

// ChildResult is one decomposed child's consolidated dispatch outcome.
// Dispatched is false for a child that was NOT spawned this call (it was
// already in-flight or terminal when discovered — re-invocation is idempotent),
// in which case ExitCode/Outcome are zero and StageState reflects the state read
// at discovery.
type ChildResult struct {
	RunID      string   `json:"run_id"`
	StageID    string   `json:"stage_id,omitempty"`
	Dispatched bool     `json:"dispatched" jsonschema:"true when this call spawned the child's implement stage; false when it was already in-flight or terminal at discovery and left untouched"`
	ExitCode   int      `json:"exit_code,omitempty" jsonschema:"the child runner's process exit code; meaningful only when dispatched"`
	Outcome    string   `json:"outcome,omitempty" jsonschema:"terminal runner outcome (ok | failed) from the child's runner_completed event"`
	StageState string   `json:"stage_state,omitempty" jsonschema:"the child implement stage's state, fetched best-effort after the runner exits (or read at discovery for a non-dispatched child)"`
	Warnings   []string `json:"warnings,omitempty"`
}

// RunChildrenOutput is the consolidated per-child result. A child failure is
// DATA, never a tool error: every child is awaited and surfaces in Children
// with its exit code / outcome regardless of success.
type RunChildrenOutput struct {
	Children        []ChildResult `json:"children" jsonschema:"one entry per discovered child, in plan_decomposed order"`
	DispatchedCount int           `json:"dispatched_count" jsonschema:"how many children this call spawned (pending at discovery)"`
	EffectiveCap    int           `json:"effective_cap" jsonschema:"the resolved concurrency cap the dispatch ran under (0 == unlimited)"`
	Warnings        []string      `json:"warnings,omitempty"`
}

// registerRunChildren wires the fishhawk_run_children tool (E24.4 / #1144).
func registerRunChildren(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_run_children",
		Description: strings.TrimSpace(`
Use this after a decomposed plan is approved to drive ALL of a parent run's
pending decomposed children concurrently — the fan-out sibling of
fishhawk_run_stage (which drives ONE stage of ONE run). Pass the DECOMPOSED
PARENT's run_id; the tool discovers the children from the parent's
plan_decomposed audit entry, spawns each pending child's implement stage as a
fishhawk-runner subprocess under an errgroup bounded by the orchestrator-resolved
effective concurrency cap (clamp-down-only against an optional max_parallel
override), and AWAITS ALL of them before returning.

Each child provisions its OWN isolated per-child git worktree (--parallel-isolate)
keyed on the child run id, so concurrent siblings — which already own distinct
per-slice sole-writer branches — never race a shared checkout and the operator's
tracked tree stays untouched.

A child failure is DATA, not a tool error: there is NO sibling-cancel — every
child is awaited and surfaces in children[] with its exit_code, outcome, and
stage_state regardless of success. Re-invocation is idempotent: only children
that are still pending at discovery are dispatched; in-flight and terminal
children are reported as-is and left untouched.

Returns children[] (one entry per discovered child), dispatched_count (how many
were pending and spawned), and effective_cap (the concurrency cap used; 0 means
unlimited). Requires the fishhawk-runner binary to resolve on the MCP server's
host, exactly like fishhawk_run_stage.
`),
	}, resolver.runChildren)
}

// childDispatch is one discovered child's pre-dispatch handle: its run id, the
// resolved implement stage id, and whether it was pending at discovery.
type childDispatch struct {
	runID      string
	stageID    string
	state      string
	pending    bool
	stateKnown bool
}

// runChildren is the tool handler.
func (r *runResolver) runChildren(ctx context.Context, req *mcp.CallToolRequest, in RunChildrenInput) (*mcp.CallToolResult, RunChildrenOutput, error) {
	if in.RunID == "" || in.Workflow == "" {
		return nil, RunChildrenOutput{}, errors.New("run_id and workflow are both required")
	}
	parentUUID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, RunChildrenOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}

	// (a) Discover the children + the effective cap from the parent's
	// plan_decomposed audit entry. Absence means the run is not decomposed —
	// a clean tool error, not a silent empty dispatch.
	pd, err := r.api.LatestPlanDecomposed(ctx, parentUUID)
	if err != nil {
		return nil, RunChildrenOutput{}, fmt.Errorf("discover decomposed children: %w", err)
	}
	if pd == nil {
		return nil, RunChildrenOutput{}, fmt.Errorf(
			"run %s has no plan_decomposed audit entry; it is not a decomposed parent (nothing to fan out)", in.RunID)
	}
	if len(pd.ChildRunIDs) == 0 {
		return nil, RunChildrenOutput{}, fmt.Errorf(
			"run %s plan_decomposed entry names no child_run_ids", in.RunID)
	}

	// Resolve the runner binary once (shared by every child spawn).
	binary, err := resolveRunnerBinary(in.RunnerBinary, r.getenv)
	if err != nil {
		return nil, RunChildrenOutput{}, err
	}

	workingDir := in.WorkingDir
	if workingDir == "" {
		workingDir = "."
	}
	baseBranch := in.BaseBranch
	if baseBranch == "" {
		baseBranch = "main"
	}

	var warnings []string

	// Children push to per-slice branches for the fan-in, so a repo is
	// required — auto-detect from working_dir's origin when not supplied.
	repo := in.GitHubRepo
	if repo == "" {
		detected, derr := runStageDetectGitHubRepo(workingDir)
		if derr != nil {
			return nil, RunChildrenOutput{}, fmt.Errorf(
				"github_repo not set and could not detect from origin: %w", derr)
		}
		repo = detected
	}

	// (b) Partition children by their freshly-read state: dispatch only
	// State==pending; report in-flight and terminal children as-is. Reading
	// state fresh per call (not from the audit snapshot) is what makes
	// re-invocation idempotent.
	dispatches := make([]childDispatch, 0, len(pd.ChildRunIDs))
	for _, childID := range pd.ChildRunIDs {
		d := childDispatch{runID: childID}
		childUUID, perr := uuid.Parse(childID)
		if perr != nil {
			d.stateKnown = false
			warnings = append(warnings, fmt.Sprintf("child %q is not a valid UUID; skipped", childID))
			dispatches = append(dispatches, d)
			continue
		}
		runRow, gerr := r.api.GetRun(ctx, childUUID)
		if gerr != nil {
			warnings = append(warnings, fmt.Sprintf("child %s: get run failed (%v); skipped", childID, gerr))
			dispatches = append(dispatches, d)
			continue
		}
		stageID, serr := r.resolveStageID(ctx, childUUID, "implement", "")
		if serr != nil {
			warnings = append(warnings, fmt.Sprintf("child %s: resolve implement stage failed (%v); skipped", childID, serr))
			dispatches = append(dispatches, d)
			continue
		}
		d.stageID = stageID
		d.state = runRow.State
		d.stateKnown = true
		d.pending = runRow.State == "pending"
		dispatches = append(dispatches, d)
	}

	// (c) Resolve the effective concurrency cap.
	concurrencyCap := clampMaxParallel(pd.EffectiveMaxParallel, in.MaxParallel)

	env := append(os.Environ(), "FISHHAWK_API_TOKEN="+r.api.token)
	var progToken any
	if req != nil && req.Params != nil {
		progToken = req.Params.GetProgressToken()
	}

	// (d) Concurrent dispatch under an errgroup bounded by the cap. SetLimit
	// is skipped for cap<=0 (unlimited) — SetLimit(n<=0) is rejected by
	// errgroup. Each child ALWAYS returns nil so a failure never cancels
	// siblings (await-all, no-sibling-cancel); the failure is recorded as data.
	g, gctx := errgroup.WithContext(ctx)
	if concurrencyCap > 0 {
		g.SetLimit(concurrencyCap)
	}
	var (
		mu         sync.Mutex
		dispatched = map[string]*ChildResult{}
	)
	dispatchedCount := 0
	for _, d := range dispatches {
		if !d.pending {
			continue
		}
		d := d
		dispatchedCount++
		g.Go(func() error {
			argv := []string{
				"--run-id", d.runID,
				"--backend-url", r.api.baseURL,
				"--workflow", in.Workflow,
				"--stage", "implement",
				"--stage-id", d.stageID,
				"--working-dir", workingDir,
				"--fetch-prompt",
				"--upload-trace",
				"--github-repo", repo,
				"--base-branch", baseBranch,
				"--check-base-ref", baseBranch,
				// The load-bearing flag: each concurrent child keys its worktree
				// on its OWN run id (run-<child>) instead of the shared parent
				// root, so siblings get isolated checkouts (E24.4 / #1144).
				"--parallel-isolate",
			}
			events, spawnWarnings, exitCode, spawnErr := spawnRunnerStageFn(gctx, binary, argv, env, req, progToken)
			res := &ChildResult{
				RunID:      d.runID,
				StageID:    d.stageID,
				Dispatched: true,
				ExitCode:   exitCode,
				Warnings:   spawnWarnings,
			}
			if spawnErr != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("spawn failed: %v", spawnErr))
			} else {
				summary, _ := summarizeRunStageEvents(events)
				res.Outcome = summary.Outcome
			}
			mu.Lock()
			dispatched[d.runID] = res
			mu.Unlock()
			return nil
		})
	}
	// Await ALL — g.Wait never returns a sibling-cancel error because every
	// g.Go returned nil.
	_ = g.Wait()

	// (e) Best-effort post-run stage_state for each dispatched child.
	for id, res := range dispatched {
		if childUUID, perr := uuid.Parse(id); perr == nil {
			if stages, lerr := r.api.ListRunStages(ctx, childUUID); lerr == nil {
				for _, s := range stages {
					if s.ID == res.StageID {
						res.StageState = s.State
						break
					}
				}
			}
		}
	}

	// Assemble the consolidated output in plan_decomposed order so the result
	// is stable across calls.
	out := RunChildrenOutput{
		DispatchedCount: dispatchedCount,
		EffectiveCap:    concurrencyCap,
		Warnings:        warnings,
	}
	for _, d := range dispatches {
		if res, ok := dispatched[d.runID]; ok {
			out.Children = append(out.Children, *res)
			continue
		}
		// Not dispatched this call: report its discovery state as-is.
		cr := ChildResult{RunID: d.runID, StageID: d.stageID, Dispatched: false}
		if d.stateKnown {
			cr.StageState = d.state
		}
		out.Children = append(out.Children, cr)
	}

	return nil, out, nil
}
