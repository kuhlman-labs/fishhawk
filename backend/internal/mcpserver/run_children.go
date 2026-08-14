package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// spawnRunnerStageDetachedFn is the DETACHED spawn seam fishhawk_run_children
// dispatches each child through (E50.13 / #2363). Production points it at the
// real spawnRunnerStageDetached — the SAME machinery fishhawk_dispatch_stage
// uses (ADR-037 / #1232), which forks through detachedRunners.startAndRegister
// so fishhawk_cancel_run's terminateRunners can reap the children this verb
// spawns (the capability #2679 says is missing). Tests swap in a fake to
// capture the composed argv and to model a runner without launching one; the
// registry-membership test deliberately leaves the seam at its default, because
// a faked seam registers nothing and the assertion would be vacuous.
//
// It mirrors run_children.go's pre-#2363 `var spawnRunnerStageFn` precedent —
// only the underlying spawn changed from blocking-with-events to detached.
var spawnRunnerStageDetachedFn = spawnRunnerStageDetached

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
// #1144). run_id is the DECOMPOSED PARENT whose dispatchable children are
// spawned.
type RunChildrenInput struct {
	RunID        string `json:"run_id" jsonschema:"the DECOMPOSED PARENT run UUID; the tool discovers its children from the parent's plan_decomposed audit entry"`
	Workflow     string `json:"workflow" jsonschema:"workflow ID matching the run's workflow (passed through to each child's runner)"`
	WorkingDir   string `json:"working_dir,omitempty" jsonschema:"checkout the children run in. OPTIONAL when the parent run carries a start_run binding (E66.42 / #2482): omit it to INHERIT the bound checkout. An explicit value is an override and must match the binding after path cleaning — a conflicting value is refused. Over the HTTP MCP transport (fishhawkd's /mcp route, or fishhawk-mcp --transport http) an omitted-and-unbound or relative value is refused — the server's cwd is the daemon's own checkout. On the stdio transport an omitted-and-unbound value defaults to the client-spawned process's own directory (resolved to an absolute path). Each child provisions its OWN per-child worktree under this checkout's shared gitdir (--parallel-isolate), so the operator's tracked tree is untouched"`
	GitHubRepo   string `json:"github_repo,omitempty" jsonschema:"GitHub repo as owner/name; auto-detected from working_dir's origin remote when empty"`
	BaseBranch   string `json:"base_branch,omitempty" jsonschema:"fallback base branch for a child's implement stage; defaults to main. The SERVER is the authority for a dependent fan-out child — the host-dispatch marker returns the consolidated base_branch and this value is used only when it returns none"`
	MaxParallel  int    `json:"max_parallel,omitempty" jsonschema:"optional operator concurrency override; clamp-DOWN-only against the orchestrator-resolved effective cap (it can lower an unlimited/looser cap, never raise it). Omit (0) to use the effective cap as-is"`
	RunnerBinary string `json:"runner_binary,omitempty" jsonschema:"path to fishhawk-runner; resolved in order: this input, FISHHAWK_RUNNER_BIN env, fishhawk-runner sibling to this binary, then PATH"`
	Verbose      bool   `json:"verbose,omitempty" jsonschema:"accepted for back-compat and ignored: a detached child returns no event stream to this call, so there is nothing to make verbose"`
}

// ChildResult is one decomposed child's dispatch outcome. Dispatched is true
// only when THIS call actually forked a detached runner for the child.
//
// Post-#2363 there is NO exit code and NO terminal outcome here: the spawn is
// DETACHED and this call returns in milliseconds, long before any child
// finishes. The handle a caller keeps is (RunID, StageID, LogPath); the wait is
// fishhawk_await_children.
type ChildResult struct {
	RunID      string   `json:"run_id"`
	StageID    string   `json:"stage_id,omitempty"`
	Dispatched bool     `json:"dispatched" jsonschema:"true when this call forked a detached runner for the child's implement stage; false when no runner was spawned this call — it was already in-flight/terminal at discovery, the host-dispatch marker failed closed or refused, the per-invocation dispatch budget was exhausted, or the spawn itself failed"`
	StageState string   `json:"stage_state,omitempty" jsonschema:"the child implement stage's state as read at discovery (or echoed by the host-dispatch marker); NOT a post-run state — the runner is still running when this call returns"`
	LogPath    string   `json:"log_path,omitempty" jsonschema:"the detached runner's log file on the MCP server's host; present only for a child this call spawned"`
	Warnings   []string `json:"warnings,omitempty"`
}

// RunChildrenOutput is the consolidated per-child dispatch result. A per-child
// dispatch problem is DATA, never a tool error: every discovered child surfaces
// in Children with its handle and any warning.
type RunChildrenOutput struct {
	Children        []ChildResult `json:"children" jsonschema:"one entry per discovered child, in plan_decomposed order"`
	DispatchedCount int           `json:"dispatched_count" jsonschema:"how many children this call actually spawned"`
	// EffectiveCap is a PER-INVOCATION DISPATCH BUDGET, not a live-runner
	// bound. See runChildrenCapDisclosure.
	EffectiveCap int `json:"effective_cap" jsonschema:"the PER-INVOCATION dispatch budget this call ran under (0 == unlimited). It bounds how many children THIS call spawned against a point-in-time in-flight snapshot; it does NOT bound the number of live detached runners for the parent — two concurrent invocations can each spend a full budget against the same snapshot"`
	// ResolvedWorkingDir echoes the absolute checkout each child's per-child
	// worktree was provisioned under, after transport-conditional working_dir
	// resolution (#2479).
	ResolvedWorkingDir string `json:"resolved_working_dir,omitempty" jsonschema:"the absolute checkout directory the children were spawned against, after transport-conditional working_dir resolution (#2479)"`
	// NextStep points at fishhawk_await_children with run_id pre-filled — the
	// in-band wait that replaced this verb's old blocking await-all.
	NextStep *SuggestedAction `json:"next_step,omitempty" jsonschema:"the single call to make after a dispatch: fishhawk_await_children with run_id pre-filled"`
	Warnings []string         `json:"warnings,omitempty"`
}

// runChildrenCapDisclosure is the honest statement of what effective_cap is and
// is NOT, rendered into both the tool description and the code path that spends
// it. The mechanism is a per-invocation budget computed over a point-in-time
// snapshot: count the discovered children whose implement stage reads
// dispatched|running during THIS invocation's partition, and spawn at most
// cap-inFlight children. It does NOT bound the number of live detached runners
// for the parent — two concurrent invocations can each snapshot the same
// inFlight and each spend a full budget against it. The per-stage host-dispatch
// CAS stops them double-spawning the SAME child, but the loser simply proceeds
// to a different one. Bounding live runners would need a server-side
// reservation the host-dispatch marker does not offer.
const runChildrenCapDisclosure = "effective_cap is a PER-INVOCATION DISPATCH BUDGET over a point-in-time snapshot, " +
	"not a bound on the number of live detached runners: two concurrent invocations can each spend a full budget " +
	"against the same in-flight snapshot (the per-stage host-dispatch CAS prevents double-spawning the SAME child, " +
	"but the loser proceeds to a different one)"

// registerRunChildren wires the fishhawk_run_children tool (E24.4 / #1144,
// rewritten as a non-blocking detached dispatcher by E50.13 / #2363).
func registerRunChildren(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_run_children",
		Description: strings.TrimSpace(`
Use this after a decomposed plan is approved to DISPATCH a parent run's
currently-dispatchable decomposed children. Pass the DECOMPOSED PARENT's
run_id; the tool discovers the children from the parent's plan_decomposed audit
entry, marks each dispatchable child's host spawn, forks a DETACHED
fishhawk-runner for it, and RETURNS IMMEDIATELY with the handles. It does NOT
await any child.

This is the whole point (#2363): the session is free the moment the call
returns, so a child that files a mid-stage scope amendment can have it decided
IN BAND — by construction, not by mitigation. Wait with fishhawk_await_children,
which releases on a child's pending amendment, on a child becoming
dispatchable, or on all children settling.

Wave ordering and between-wave integration are enforced SERVER-SIDE. The
host-dispatch marker refuses a child whose dependency slices have not succeeded
(409 dependency_not_satisfied) and one whose succeeded predecessors are not yet
merged (409 wave_not_integrated); the server integrates between waves on its own.
So a dependent wave's children simply become dispatchable on a LATER invocation
— re-invoke when fishhawk_await_children releases with children_dispatchable.
The marker is also the authority on the per-wave re-base: the base_branch it
returns is what the child spawns against, and base_branch input is only the
fallback when it returns none.

Each child provisions its OWN isolated per-child git worktree
(--parallel-isolate) keyed on the child run id, so concurrent siblings — which
already own distinct per-slice sole-writer branches — never race a shared
checkout and the operator's tracked tree stays untouched. Children spawned here
are registered in the detached-runner registry, so fishhawk_cancel_run reaps
them.

A per-child dispatch problem is DATA, not a tool error: every discovered child
surfaces in children[] with dispatched, stage_state, log_path and any warning.
Re-invocation is idempotent — only children whose implement stage is
pending|awaiting_host_dispatch are dispatched; in-flight and terminal children
are reported as-is and left untouched.

effective_cap is a PER-INVOCATION DISPATCH BUDGET over a point-in-time snapshot
of in-flight children (0 means unlimited). It does NOT bound the number of live
detached runners for this parent: two concurrent invocations can each spend a
full budget against the same snapshot. The per-stage host-dispatch CAS prevents
them double-spawning the SAME child, but the loser proceeds to a different one.
Children left undispatched by the budget are named in a warning; re-invoke once
slots free.

Requires the fishhawk-runner binary to resolve on the MCP server's host,
exactly like fishhawk_run_stage.
`),
	}, resolver.runChildren)
}

// childDispatch is one discovered child's pre-dispatch handle: its run id, the
// resolved implement stage id, that stage's state, and whether the stage was
// dispatchable (awaiting a host-side spawn) at discovery. The partition keys on
// the IMPLEMENT STAGE state, not the run-level state (#1237): a local
// decomposed child parked by RuleChildrenDispatch has run state 'running' while
// its implement stage sits at pending/awaiting_host_dispatch awaiting this
// fan-out.
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

// implementStageInFlight reports whether a discovered child's implement stage
// already has a runner in flight — the population the per-invocation dispatch
// budget debits against. Deliberately the exact complement of the dispatchable
// and terminal sets on the states a fan-out child can be read in: a terminal
// child consumes no budget because no runner is live for it.
func implementStageInFlight(state string) bool {
	return state == "dispatched" || state == "running"
}

// runChildren is the tool handler: a PLAIN SEQUENTIAL LOOP. There are no
// goroutines, no errgroup, no mutex and no shared mutable result map here, and
// that absence is the load-bearing property (#2363). The five rejected
// option-1 cycles all failed on one shape — a handler returning while its
// goroutines kept writing into the value it returned, leaving per-child results
// with no clean owner. Detaching the spawn removes that shape rather than
// relocating it: each child is marked, forked and recorded inline, and the call
// returns.
func (r *runResolver) runChildren(ctx context.Context, _ *mcp.CallToolRequest, in RunChildrenInput) (*mcp.CallToolResult, RunChildrenOutput, error) {
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

	// Resolve working_dir transport-conditionally BEFORE the runner-binary
	// resolution, the origin auto-detect, and — crucially — before any per-child
	// worktree is provisioned under the checkout's shared gitdir (#2479). An
	// omitted/relative working_dir over HTTP fails here with a clean error and
	// provisions no worktree. Run-aware against the PARENT run (E66.42 / #2482):
	// an omitted parameter inherits the parent's start_run binding (children
	// provision their per-child worktrees under that checkout); a conflicting
	// explicit value is refused; a run-read failure fails closed over HTTP.
	workingDir, err := r.resolveWorkingDirForRun(ctx, parentUUID, in.WorkingDir)
	if err != nil {
		return nil, RunChildrenOutput{}, err
	}

	// Resolve the runner binary once (shared by every child spawn).
	binary, err := resolveRunnerBinary(in.RunnerBinary, r.getenv)
	if err != nil {
		return nil, RunChildrenOutput{}, err
	}

	fallbackBase := in.BaseBranch
	if fallbackBase == "" {
		fallbackBase = "main"
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
	inFlight := 0
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
		if implementStageInFlight(stage.State) {
			inFlight++
		}
		dispatches = append(dispatches, d)
	}

	// (c) Resolve the per-invocation dispatch budget. See
	// runChildrenCapDisclosure for what this is NOT.
	concurrencyCap := clampMaxParallel(pd.EffectiveMaxParallel, in.MaxParallel)
	budget := -1 // -1 == unlimited
	if concurrencyCap > 0 {
		budget = concurrencyCap - inFlight
		if budget < 0 {
			budget = 0
		}
	}

	env := append(os.Environ(), "FISHHAWK_API_TOKEN="+r.api.token)

	// (d) The sequential dispatch loop, in plan_decomposed order.
	results := make(map[string]*ChildResult, len(dispatches))
	var deferred []string
	for _, d := range dispatches {
		if !d.pending {
			continue
		}
		if budget == 0 {
			deferred = append(deferred, d.runID)
			continue
		}
		res := r.dispatchOneChild(ctx, dispatchChildParams{
			d:            d,
			in:           in,
			binary:       binary,
			env:          env,
			workingDir:   workingDir,
			repo:         repo,
			fallbackBase: fallbackBase,
		})
		results[d.runID] = res
		if res.Dispatched && budget > 0 {
			budget--
		}
	}
	if len(deferred) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"per-invocation dispatch budget (effective_cap %d minus %d already in flight) exhausted; %d dispatchable child(ren) NOT spawned this call: %s. Re-invoke fishhawk_run_children once slots free (fishhawk_await_children releases with children_dispatchable). Note: %s.",
			concurrencyCap, inFlight, len(deferred), strings.Join(deferred, ", "), runChildrenCapDisclosure))
	}

	// (e) Assemble the consolidated output in plan_decomposed order so the
	// result is stable across calls. dispatched_count is the number of children
	// this call ACTUALLY spawned — a marker fail-closed, a wave refusal, a
	// budget deferral or a spawn error records Dispatched:false and so does not
	// inflate the count. There is no post-run stage_state refresh: the runners
	// are still running.
	out := RunChildrenOutput{
		EffectiveCap:       concurrencyCap,
		ResolvedWorkingDir: workingDir,
		Warnings:           warnings,
		NextStep:           awaitChildrenNextStep(parentUUID.String()),
	}
	for _, d := range dispatches {
		if res, ok := results[d.runID]; ok {
			if res.Dispatched {
				out.DispatchedCount++
			}
			if res.StageState == "" && d.stateKnown {
				res.StageState = d.state
			}
			out.Children = append(out.Children, *res)
			continue
		}
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

// dispatchChildParams carries the per-child inputs of one sequential dispatch
// step. It exists only to keep dispatchOneChild's signature readable; every
// field is immutable for the whole loop except d.
type dispatchChildParams struct {
	d            childDispatch
	in           RunChildrenInput
	binary       string
	env          []string
	workingDir   string
	repo         string
	fallbackBase string
}

// dispatchOneChild marks the host spawn for one dispatchable child and forks a
// DETACHED runner for it. It never returns an error: every no-spawn path is
// recorded as Dispatched:false with an explanatory warning, so the loop
// continues to the next child and dispatched_count counts ACTUAL spawns.
func (r *runResolver) dispatchOneChild(ctx context.Context, p dispatchChildParams) *ChildResult {
	d := p.d
	res := &ChildResult{RunID: d.runID, StageID: d.stageID}

	childUUID, cerr := uuid.Parse(d.runID)
	stageUUID, serr := uuid.Parse(d.stageID)
	if cerr != nil || serr != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"could not parse child run/stage id for host-dispatch marker (run=%q stage=%q); NOT spawning", d.runID, d.stageID))
		return res
	}

	// (#1912) Mark the host spawn BEFORE spawning, exactly as
	// fishhawk_run_stage / fishhawk_dispatch_stage do: the endpoint CAS-flips
	// {pending, awaiting_host_dispatch} → dispatched so 'dispatched'
	// unambiguously means a spawn attempt exists at spawn time. UNLIKE the
	// single-stage verbs, run_children keys the spawn on the marker's
	// transitioned signal: only transitioned:true (THIS call won the CAS)
	// proceeds. FAIL CLOSED on any marker error.
	hdr, hderr := r.api.HostDispatchStage(ctx, childUUID, stageUUID)
	if hderr != nil {
		// The per-wave integration refusal (E50.13 / #2363) is the one marker
		// error that means "nothing is wrong, wait": this child's predecessors
		// have succeeded but the server has not merged them onto the
		// consolidated branch yet. The client annotates it once
		// (client.go HostDispatchStage), so match on that annotation's code via
		// the preserved *apiError rather than on message text.
		if isWaveNotIntegrated(hderr) {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"host-dispatch refused 409 wave_not_integrated; NOT spawning: this child's predecessors have succeeded but are not yet merged onto the parent's consolidated branch. The server integrates between waves — wait with fishhawk_await_children (it releases children_dispatchable once the integration lands) and re-invoke fishhawk_run_children. Detail: %v", hderr))
			return res
		}
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"host-dispatch marker failed; NOT spawning (fail-closed): %v", hderr))
		return res
	}
	if !hdr.Transitioned {
		// Idempotent already-'dispatched' no-op: a concurrent invocation won the
		// CAS and a runner is already in flight. Do NOT spawn a second one (the
		// #1912 double-spawn).
		res.StageState = hdr.StageState
		res.Warnings = append(res.Warnings, "host-dispatch marker was a no-op (already 'dispatched' by a concurrent run_children/drive_run invocation; a runner is already in flight); NOT spawning to avoid a double-spawn")
		return res
	}
	if hdr.StageState != "" {
		res.StageState = hdr.StageState
	}

	// The SERVER is the authority on the per-wave re-base (#2363): the marker
	// returns the consolidated base_branch once every dependency slice this
	// child declares is provably merged onto it, and empty for every other
	// caller. The client derives nothing.
	base := p.fallbackBase
	if hdr.BaseBranch != "" {
		base = hdr.BaseBranch
	}

	argv := []string{
		"--run-id", d.runID,
		"--backend-url", r.api.baseURL,
		"--workflow", p.in.Workflow,
		"--stage", "implement",
		stageIDArgFlag, d.stageID,
		"--working-dir", p.workingDir,
		"--fetch-prompt",
		"--upload-trace",
		"--github-repo", p.repo,
		// --base-branch / --check-base-ref drives BOTH halves of a dependent
		// wave's basing on its predecessors' merged tree: (a) the runner's
		// PRE-INVOKE working-tree checkout of this base into the child worktree,
		// so the agent SEES its predecessors' integrated symbols and can compile
		// (#1302); and (b) the commit-time branch cut from origin/<base> (the
		// runner's freshFetchBase routing). freshFetchBase alone governs only
		// (b) — the agent's working tree is established by (a).
		"--base-branch", base,
		"--check-base-ref", base,
		// The load-bearing flag: each concurrent child keys its worktree on its
		// OWN run id (run-<child>) instead of the shared parent root, so siblings
		// get isolated checkouts (E24.4 / #1144).
		"--parallel-isolate",
	}

	// The report + probe closures are built exactly as dispatch_stage.go builds
	// them, bound to THIS child's (run, stage). report is BOTH the detached
	// reaper's failure channel AND — see below — the spawn-error compensation.
	report := func(ctx context.Context, category, reason, detail string, exitCode int) error {
		_, rerr := r.api.ReportStageFailure(ctx, childUUID, stageUUID, category, reason, detail, exitCode)
		return rerr
	}
	probe := r.stageStateProbe(childUUID, d.stageID)

	logPath, spawnErr := spawnRunnerStageDetachedFn(p.binary, argv, p.env, d.runID, d.stageID, report, probe)
	if spawnErr == nil {
		res.Dispatched = true
		res.LogPath = logPath
		return res
	}

	// THE SPAWN-ERROR COMPENSATION (#2363). The host-dispatch CAS committed
	// BEFORE the spawn (the #1912 fail-closed ordering, deliberately unchanged),
	// so the stage is 'dispatched' right now. Nothing recovers it on its own:
	// the reaper goroutine that handles a non-zero exit (#1747) and a zero-exit
	// strand (#2630) starts only AFTER detachedRunners.startAndRegister
	// succeeds, so a FAILED spawn has no reaper at all. Left uncompensated the
	// stage sits 'dispatched' forever, every later invocation reads
	// Transitioned:false and treats it as already-dispatched, and the child can
	// never be retried — the permanent-strand class #2630 exists to remove.
	//
	// BRANCH 1 — the cancel-refused spawn. errRunCancelledBeforeSpawn means a
	// cancel for this run already landed and the registry refused the fork.
	// Failing the stage would relabel a cancelled run as failed, and the cancel
	// path owns the run's terminal state. Do NOT compensate.
	if errors.Is(spawnErr, errRunCancelledBeforeSpawn) {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"spawn refused because a cancel for this run already landed; NOT reporting a stage failure (the cancel owns the run's terminal state): %v", spawnErr))
		return res
	}

	// BRANCH 2 — any other spawn error (missing/unexecutable runner binary, a
	// fork failure, a log-file open failure). Invoke the SAME report closure the
	// reaper would have used. This is authorized by construction and needs no
	// new endpoint: reapProtectedParkStates (backend/internal/server/reap_failure.go)
	// is a CLOSED allow-list of five PARK states the reap handler must never
	// collapse, and 'dispatched' is deliberately NOT among them — the reaper's
	// authority is exactly {dispatched, running} — and the stage is 'dispatched'
	// at exactly this point. The stage lands failed/category-C, the retryable
	// infrastructure class, which fishhawk_retry_stage recovers: for a
	// runner_kind local run it parks the stage back at awaiting_host_dispatch,
	// a state implementStageDispatchable admits, so a SUBSEQUENT
	// fishhawk_run_children re-spawns the child.
	res.Warnings = append(res.Warnings, fmt.Sprintf(
		"spawn failed after the host-dispatch marker had already flipped the stage to 'dispatched': %v. Reported it as a category-C stage failure so the stage is retryable; recover with fishhawk_retry_stage (which parks a local child back at awaiting_host_dispatch) then re-invoke fishhawk_run_children.", spawnErr))
	if rerr := report(ctx, "C", "runner_spawn_failed", spawnErr.Error(), 0); rerr != nil {
		// A FAILED COMPENSATION IS A DISCLOSED RESIDUAL STRAND, not a claimed
		// recovery. The stage stays 'dispatched' with no runner and no reaper.
		// fishhawk_retry_stage does NOT clear it: run.RetryStage admits ONLY a
		// stage in state 'failed' (backend/internal/run — a non-failed stage is
		// refused ErrRetryNotApplicable), so a 'dispatched' stage is out of its
		// reach. The verb that actually clears it is the reap-failure REST
		// endpoint this compensation just failed to reach — POST
		// /v0/runs/{run_id}/stages/{stage_id}/reap-failure with category C —
		// whose authority IS {dispatched, running}; once that lands the stage is
		// 'failed' and fishhawk_retry_stage becomes applicable. Since #2689 that
		// endpoint HAS an MCP verb — fishhawk_reap_stage — so the warning
		// prescribes the verb and keeps the literal endpoint only as the fallback
		// for a client that has not picked it up yet.
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"the category-C failure report ALSO failed (%v), so this child's implement stage is STRANDED in 'dispatched' with no runner and no reaper. fishhawk_retry_stage will NOT clear it — it admits only a 'failed' stage. Recover with fishhawk_reap_stage(run_id:%q, stage_id:%q, category:\"C\"), then fishhawk_retry_stage, then re-invoke fishhawk_run_children. (Fallback for a client without that verb: POST %s/v0/runs/%s/stages/%s/reap-failure with {\"category\":\"C\",\"reason\":\"runner_spawn_failed\"}.)",
			rerr, d.runID, d.stageID, r.api.baseURL, d.runID, d.stageID))
	}
	return res
}

// isWaveNotIntegrated reports whether a host-dispatch marker error is the
// per-wave integration refusal (E50.13 / #2363). The client wraps that 409 with
// an actionable annotation but preserves the *apiError in the chain (%w), so the
// match is on the structured CODE, never on message text.
func isWaveNotIntegrated(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Code == "wave_not_integrated"
}
