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
// Dispatched is false for a child that was NOT spawned this call — it was
// already in-flight or terminal when discovered (re-invocation is idempotent),
// the host-dispatch marker failed closed, or the marker was a
// concurrent-invocation no-op (another caller already marked it) — in which case
// ExitCode/Outcome are zero and StageState reflects the state read at discovery
// (or echoed by the marker). dispatched_count counts only Dispatched:true
// children, so it never overstates the runners this call actually spawned.
type ChildResult struct {
	RunID      string   `json:"run_id"`
	StageID    string   `json:"stage_id,omitempty"`
	Dispatched bool     `json:"dispatched" jsonschema:"true when this call spawned the child's implement stage; false when no runner was spawned this call — it was already in-flight/terminal at discovery, the host-dispatch marker failed closed, or the marker was a concurrent-invocation no-op"`
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
// resolved implement stage id, that stage's state, and whether the stage was
// dispatchable (awaiting a host-side spawn) at discovery. The partition keys on
// the IMPLEMENT STAGE state, not the run-level state (#1237): a local
// decomposed child parked by RuleChildrenDispatch has run state 'running' while
// its implement stage sits at pending/dispatched awaiting this fan-out.
type childDispatch struct {
	runID   string
	stageID string
	// state is the implement STAGE state read at discovery (pending |
	// awaiting_host_dispatch | dispatched | running | terminal), surfaced to the
	// caller as stage_state.
	state string
	// pending is true when the stage was dispatchable (pending|awaiting_host_dispatch)
	// at discovery — i.e. awaiting a host spawn, so this call dispatches it.
	pending    bool
	stateKnown bool
}

// implementStageDispatchable reports whether an implement stage in the given
// state is awaiting a host-side runner spawn and so should be dispatched by
// fishhawk_run_children. Only {pending, awaiting_host_dispatch} qualify (#1912):
// a local decomposed child parked by RuleChildrenDispatch (#1143) has its RUN
// advanced to 'running' but its implement STAGE left at
// pending/awaiting_host_dispatch for a host dispatch, so keying on the run state
// skipped every such child as in-flight (#1237). Post-#1912 'dispatched' means a
// spawn attempt EXISTS — a runner is in flight — so a 'dispatched' child is
// deliberately NOT dispatchable here: re-spawning would double-drive it. running
// / terminal stages are likewise genuinely executing or done and left untouched.
// This mirrors the next_actions implement_pending arm (which routes
// pending|awaiting_host_dispatch to dispatch and bare dispatched to poll); raw
// string literals match that file's convention (the mcp package does not import
// the run.StageState* constants).
func implementStageDispatchable(state string) bool {
	return state == "pending" || state == "awaiting_host_dispatch"
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

	// (b) Partition children by their freshly-read IMPLEMENT STAGE state:
	// dispatch only the stages awaiting a host spawn (pending|awaiting_host_dispatch);
	// report in-flight (dispatched|running) and terminal children as-is. Keying on
	// the stage state — not the run state — is what makes a local decomposed child
	// (run='running', stage=pending/awaiting_host_dispatch after RuleChildrenDispatch)
	// dispatchable here (#1237); a 'dispatched' child already has a runner in flight
	// (#1912). Reading state fresh per call (not from the audit snapshot) is what
	// makes re-invocation idempotent.
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
		stage, serr := r.resolveStage(ctx, childUUID, "implement", "")
		if serr != nil {
			warnings = append(warnings, fmt.Sprintf("child %s: resolve implement stage failed (%v); skipped", childID, serr))
			dispatches = append(dispatches, d)
			continue
		}
		d.stageID = stage.ID
		d.state = stage.State
		d.stateKnown = true
		d.pending = implementStageDispatchable(stage.State)
		dispatches = append(dispatches, d)
	}

	// (c) Resolve the effective concurrency cap.
	concurrencyCap := clampMaxParallel(pd.EffectiveMaxParallel, in.MaxParallel)

	env := append(os.Environ(), "FISHHAWK_API_TOKEN="+r.api.token)
	var progToken any
	if req != nil && req.Params != nil {
		progToken = req.Params.GetProgressToken()
	}

	// (d) Topological-wave dispatch (#1258 slice B). The plan_decomposed payload
	// carries waves of SLICE INDICES into ChildRunIDs (ChildRunIDs[i] is the
	// child minted for slice i, and dispatches was built in that same order, so
	// wave index idx maps to dispatches[idx]). Each wave's pending children
	// dispatch concurrently under the cap against currentBase; between waves the
	// loop integrates the succeeded slices so far and re-bases the next wave's
	// children on the consolidated branch so dependent slices see predecessors'
	// merged symbols.
	//
	// Back-compat is load-bearing: a nil/empty waves (an old plan_decomposed
	// entry, or a no-depends_on decomposition) collapses to a single all-indices
	// wave — one concurrent wave dispatched with --base-branch main, and
	// integrate-wave is NEVER called (a single wave is also the last wave). This
	// reduces to the pre-#1278 single-errgroup behavior byte-for-byte.
	waves := pd.Waves
	if len(waves) == 0 {
		all := make([]int, len(dispatches))
		for i := range all {
			all[i] = i
		}
		waves = [][]int{all}
	}

	var (
		mu         sync.Mutex
		dispatched = map[string]*ChildResult{}
	)
	currentBase := baseBranch

	for wi, wave := range waves {
		// Map this wave's slice indices to the partitioned child dispatches. An
		// index out of range is a loud tool error, never a silent skip — it
		// means the waves payload and child_run_ids disagree.
		waveDispatches := make([]childDispatch, 0, len(wave))
		for _, idx := range wave {
			if idx < 0 || idx >= len(dispatches) {
				return nil, RunChildrenOutput{}, fmt.Errorf(
					"plan_decomposed waves index %d out of range [0,%d) for run %s", idx, len(dispatches), in.RunID)
			}
			waveDispatches = append(waveDispatches, dispatches[idx])
		}

		// Concurrent dispatch of THIS wave's pending children under an errgroup
		// bounded by the cap. SetLimit is skipped for cap<=0 (unlimited) —
		// SetLimit(n<=0) is rejected by errgroup. Each child ALWAYS returns nil
		// so a failure never cancels siblings (await-all, no-sibling-cancel);
		// the failure is recorded as data. Children already in-flight/terminal
		// at discovery stay partitioned out (idempotent re-invocation).
		g, gctx := errgroup.WithContext(ctx)
		if concurrencyCap > 0 {
			g.SetLimit(concurrencyCap)
		}
		waveDispatchedIDs := make([]string, 0, len(waveDispatches))
		for _, d := range waveDispatches {
			if !d.pending {
				continue
			}
			d := d
			// waveDispatchedIDs records every child this wave ATTEMPTS to spawn
			// (the partial-wave guard keys on these); dispatched_count is derived
			// from the ACTUAL Dispatched:true results at assembly time, so a
			// marker fail-closed / concurrent no-op that spawns nothing is counted
			// as an attempt for the guard but NOT as a spawn in dispatched_count.
			waveDispatchedIDs = append(waveDispatchedIDs, d.runID)
			waveBase := currentBase
			g.Go(func() error {
				// (#1912 fix-up) Mark the host spawn BEFORE spawning, exactly as
				// fishhawk_run_stage / fishhawk_dispatch_stage do: the endpoint
				// CAS-flips {pending, awaiting_host_dispatch} → dispatched so
				// 'dispatched' unambiguously means a spawn attempt exists at spawn
				// time. UNLIKE the single-stage verbs, run_children keys the spawn
				// on the marker's transitioned signal: only transitioned:true (THIS
				// call won the CAS) proceeds to spawn. transitioned:false is the
				// endpoint's idempotent already-'dispatched' no-op — a concurrent
				// run_children / drive_run invocation already marked this child, so a
				// runner is ALREADY in flight and spawning here would be the #1912
				// double-spawn; skip it. FAIL CLOSED on any marker error: a transport
				// error or 4xx means NO spawn (an unmarked spawn would recreate the
				// ambiguity #1912 removes). Every no-spawn path records the child as
				// Dispatched:false with an explanatory warning — NEVER Dispatched:true
				// — so dispatched_count and the child's dispatched flag count ACTUAL
				// spawns, not attempts. The empty Outcome still trips the partial-wave
				// guard below (which stops before integrating or dispatching a
				// dependent wave); this is never a sibling-cancel (this g.Go still
				// returns nil).
				childUUID, cerr := uuid.Parse(d.runID)
				stageUUID, serr := uuid.Parse(d.stageID)
				if cerr != nil || serr != nil {
					mu.Lock()
					dispatched[d.runID] = &ChildResult{
						RunID: d.runID, StageID: d.stageID, Dispatched: false,
						Warnings: []string{fmt.Sprintf("could not parse child run/stage id for host-dispatch marker (run=%q stage=%q); NOT spawning", d.runID, d.stageID)},
					}
					mu.Unlock()
					return nil
				}
				hdr, hderr := r.api.HostDispatchStage(gctx, childUUID, stageUUID)
				if hderr != nil {
					mu.Lock()
					dispatched[d.runID] = &ChildResult{
						RunID: d.runID, StageID: d.stageID, Dispatched: false,
						Warnings: []string{fmt.Sprintf("host-dispatch marker failed; NOT spawning (fail-closed): %v", hderr)},
					}
					mu.Unlock()
					return nil
				}
				if !hdr.Transitioned {
					// Idempotent already-'dispatched' no-op: a concurrent invocation
					// won the CAS and a runner is already in flight. Do NOT spawn a
					// second one (the #1912 double-spawn). Report it as not-dispatched
					// this call with the marker's echoed state.
					mu.Lock()
					dispatched[d.runID] = &ChildResult{
						RunID: d.runID, StageID: d.stageID, Dispatched: false,
						StageState: hdr.StageState,
						Warnings:   []string{"host-dispatch marker was a no-op (already 'dispatched' by a concurrent run_children/drive_run invocation; a runner is already in flight); NOT spawning to avoid a double-spawn"},
					}
					mu.Unlock()
					return nil
				}
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
					// --base-branch / --check-base-ref drives BOTH halves of wave
					// N's basing on the prior wave's merged tree: (a) the runner's
					// PRE-INVOKE working-tree checkout of this base into the child
					// worktree, so the agent SEES its predecessors' integrated
					// symbols and can compile (#1302); and (b) the commit-time
					// branch cut from origin/<base> (the runner's freshFetchBase
					// routing). freshFetchBase alone governs only (b) — the
					// agent's working tree is established by (a).
					"--base-branch", waveBase,
					"--check-base-ref", waveBase,
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
		// Await ALL of this wave — g.Wait never returns a sibling-cancel error
		// because every g.Go returned nil.
		_ = g.Wait()

		// Partial-wave guard: if any DISPATCHED child in this wave did not
		// succeed (non-ok outcome or non-zero exit), STOP — do NOT integrate a
		// partial wave and do NOT dispatch a dependent wave against an
		// incomplete base. The failure is already recorded as data in children[].
		waveFailed := false
		mu.Lock()
		for _, cid := range waveDispatchedIDs {
			res := dispatched[cid]
			if res == nil || res.Outcome != "ok" || res.ExitCode != 0 {
				waveFailed = true
				break
			}
		}
		mu.Unlock()
		if waveFailed {
			warnings = append(warnings, fmt.Sprintf(
				"wave %d had a child that did not succeed; stopping before integrating or dispatching further waves", wi))
			break
		}

		// The last wave needs no integrate-wave: the final terminal fan-in
		// (stage resolution + consolidated PR) stays driven by /consolidate
		// after every child settles. A single (back-compat) wave is also the
		// last wave, so integrate-wave is NEVER called for a no-depends_on
		// decomposition.
		if wi == len(waves)-1 {
			break
		}

		// Wave-integrity guard (#1980): before integrating this wave and
		// dispatching a DEPENDENT wave against its merged tree, every
		// NON-attempted child of THIS wave must be terminal-'succeeded'. The
		// waveFailed guard above only inspects children ATTEMPTED this call, so a
		// wave whose children were ALL partitioned out as in-flight (e.g. legacy
		// 'dispatched' park children, #1980) passed it vacuously — then
		// IntegrateWave ran against nothing (the bogus empty-consolidated-branch
		// warning) and the next wave dispatched against a base missing its
		// predecessors. A non-attempted child that is not 'succeeded' means its
		// slice is NOT in the base yet, so STOP loudly here rather than corrupt
		// the dependent wave's base.
		var waveBlockers []string
		for _, d := range waveDispatches {
			if d.pending {
				continue // attempted this call; covered by the waveFailed guard
			}
			if !d.stateKnown || d.state != "succeeded" {
				state := d.state
				if !d.stateKnown {
					state = "unknown"
				}
				blocker := fmt.Sprintf("child %s (stage_state %q)", d.runID, state)
				if d.state == "dispatched" {
					blocker += " — if no runner is live, recover with fishhawk_dispatch_stage run SEQUENTIALLY per child (concurrent manual dispatches race the shared parent lineage worktree)"
				}
				waveBlockers = append(waveBlockers, blocker)
			}
		}
		if len(waveBlockers) > 0 {
			warnings = append(warnings, fmt.Sprintf(
				"wave %d has non-attempted child(ren) not yet 'succeeded'; stopping before integrating or dispatching the next wave against a base missing predecessors: %s",
				wi, strings.Join(waveBlockers, "; ")))
			break
		}

		// Between waves: the NON-settling per-wave fan-in merges the slices
		// succeeded so far onto the consolidated branch. On a transport error or
		// a slice conflict, STOP and surface it rather than dispatching the next
		// wave against a stale base.
		iw, ierr := r.api.IntegrateWave(ctx, parentUUID)
		if ierr != nil {
			warnings = append(warnings, fmt.Sprintf("integrate-wave after wave %d failed: %v; stopping before the next wave", wi, ierr))
			break
		}
		if iw.Outcome == "slice_conflict" {
			conflictSlice := -1
			if iw.ConflictingSliceIndex != nil {
				conflictSlice = *iw.ConflictingSliceIndex
			}
			warnings = append(warnings, fmt.Sprintf(
				"integrate-wave after wave %d hit a slice conflict (slice %d, child %s): %s; stopping before the next wave",
				wi, conflictSlice, iw.ConflictingChildRunID, iw.Detail))
			break
		}
		// Integrated. Re-base the next wave's children on the consolidated
		// branch. DEFENSIVELY: an empty consolidated_branch (the GitHub-not-
		// wired graceful-skip) keeps currentBase unchanged + warns rather than
		// dispatching the next wave against an empty ref.
		if iw.ConsolidatedBranch != "" {
			currentBase = iw.ConsolidatedBranch
		} else {
			warnings = append(warnings, fmt.Sprintf(
				"integrate-wave after wave %d returned an empty consolidated_branch; keeping base %q for the next wave", wi, currentBase))
		}
	}

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
	// is stable across calls. dispatched_count is the number of children this
	// call ACTUALLY spawned (Dispatched:true) — a marker fail-closed or a
	// concurrent-invocation no-op recorded Dispatched:false and so does not
	// inflate the count.
	out := RunChildrenOutput{
		EffectiveCap: concurrencyCap,
		Warnings:     warnings,
	}
	for _, d := range dispatches {
		if res, ok := dispatched[d.runID]; ok {
			if res.Dispatched {
				out.DispatchedCount++
			}
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

	// Loud zero-dispatch guard (#1980): dispatched_count==0 with one or more
	// children whose implement stage reads 'dispatched' is the legacy park
	// signature — pre-#1980, a decomposed child of a locked-local parent flipped
	// to 'dispatched' with NO runner ever spawned, and run_children's dispatchable
	// predicate (correctly, post-#1912) treats 'dispatched' as in-flight, so it
	// dispatches nothing and would otherwise return a SILENT zero-dispatch
	// success. Surface it. This is a WARNING, not a tool error: a concurrent
	// run_children/drive_run invocation can legitimately own the in-flight
	// children, and run_children has no host-process view for children it did not
	// spawn — so the guidance is conditional on no live runner.
	if out.DispatchedCount == 0 {
		var stuck []string
		for _, c := range out.Children {
			if c.StageState == "dispatched" {
				stuck = append(stuck, c.RunID)
			}
		}
		if len(stuck) > 0 {
			out.Warnings = append(out.Warnings, fmt.Sprintf(
				"dispatched_count=0 but %d child implement stage(s) read 'dispatched' (%s) and were treated as in-flight; if NO runner process is actually live for them (a legacy pre-#1980 park, not a concurrent run_children/drive_run invocation), recover by running fishhawk_dispatch_stage per child SEQUENTIALLY — concurrent manual dispatches share the parent lineage worktree and race the lineage lock",
				len(stuck), strings.Join(stuck, ", ")))
		}
	}

	return nil, out, nil
}
