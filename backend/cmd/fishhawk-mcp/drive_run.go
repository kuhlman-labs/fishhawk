package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// driveSpawnFunc is the injectable spawn seam for fishhawk_drive_run. It
// matches spawnRunnerStageDetached exactly; production uses that function,
// tests inject a recording spawner.
type driveSpawnFunc func(binary string, argv, env []string, runID, stageID string, report detachedFailureReporter) (string, error)

// runnerLivenessVerdict is the three-valued result of the host runner-liveness
// probe fishhawk_drive_run runs on a stale 'dispatched' stage (#1955). The MCP
// server runs on the same host that spawned every local runner (ADR-024), so
// the driver can decide whether the runner is genuinely dead before choosing
// between an auto-recovery re-dispatch and a manual hand-back.
type runnerLivenessVerdict int

const (
	// runnerUnknown is the fail-SAFE default: pgrep is absent from PATH, exited
	// with a syntax/fatal code, timed out, or failed to exec — the driver cannot
	// confirm the runner dead, so it degrades to today's manual verify-first
	// dispatched_stale stop. It is the zero value so any un-probed path is safe.
	runnerUnknown runnerLivenessVerdict = iota
	// runnerDead: pgrep exited 1 (no process carries the stale stage's id), so the
	// spawned runner genuinely died — the driver auto-recovers by re-dispatching.
	runnerDead
	// runnerLive: pgrep exited 0 (a process carrying the stage id exists) yet the
	// stage never flipped 'running' — anomalous; the driver stops dispatched_stale
	// and NEVER spawns a second runner into the same lineage lock.
	runnerLive
)

// driveLivenessProbeTimeout bounds the pgrep exec so a wedged process table
// read can never hang the drive loop.
const driveLivenessProbeTimeout = 5 * time.Second

// probeRunnerLiveness execs `pgrep -f` scoped to the stale stage's id to decide
// whether a runner process for it is still alive on this host (#1955). The
// pattern is "stage-id <uuid>" — no leading '-', so BSD/procps flag parsing
// never eats it as an option — which matches the runner argv's `--stage-id
// <uuid>` token pair (pgrep -f matches against the full argument list) regardless
// of the runner binary's path or name. Stage ids are UUIDs (hex + dashes), so the
// pattern is ERE-safe, and pgrep never reports itself as a match. Classification
// is delegated to the pure classifyPgrepResult so the exit-code contract is
// unit-testable without a live process.
func probeRunnerLiveness(ctx context.Context, stageID string) runnerLivenessVerdict {
	pctx, cancel := context.WithTimeout(ctx, driveLivenessProbeTimeout)
	defer cancel()
	err := exec.CommandContext(pctx, "pgrep", "-f", "stage-id "+stageID).Run()
	return classifyPgrepResult(err)
}

// classifyPgrepResult maps a pgrep exec error to a liveness verdict. It is the
// pure unit-test seam for the pgrep exit-code contract (procps-ng / BSD pgrep(1)
// EXIT STATUS: 0 = one or more matched, 1 = none matched, 2 = syntax error,
// 3 = fatal error):
//   - nil                       -> runnerLive   (exit 0: a process carries the id)
//   - *exec.ExitError, code 1   -> runnerDead   (no process matched)
//   - any other exit code (2/3/…), pgrep-not-on-PATH (exec.ErrNotFound), a
//     context timeout, or any other error -> runnerUnknown (degrade to the
//     manual verify-first stop)
func classifyPgrepResult(err error) runnerLivenessVerdict {
	if err == nil {
		return runnerLive
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return runnerDead
	}
	return runnerUnknown
}

const (
	// defaultDrivePollInterval is the in-flight poll cadence.
	defaultDrivePollInterval = 30 * time.Second
	// defaultDriveMaxMinutes / maxDriveMaxMinutes bound the wall-clock budget.
	defaultDriveMaxMinutes = 60
	maxDriveMaxMinutes     = 240
	// driveSourceTag tags every record-act the verb performs.
	driveSourceTag = "fishhawk_drive_run"
	// driveStallThreshold is the number of consecutive observe-only,
	// no-state-change, no-parked-gate iterations after which the verb returns
	// stalled rather than spinning.
	driveStallThreshold = 3
	// defaultDriveDispatchedStaleAfter is the runner-liveness threshold: a stage
	// this invocation did not spawn that has sat in 'dispatched' longer than this
	// (with no runner observed) is treated as genuinely runner-less rather than
	// in-flight. The backend flips dispatched->running on the runner's signed
	// prompt fetch (#1924), which lands within seconds of spawn for BOTH runner
	// kinds — so a stage still reading 'dispatched' past this threshold means the
	// runner never reached its prompt fetch (it died at or just after spawn) or a
	// mixed-version backend without the flip is in play; 10 minutes of headroom
	// cannot misclassify a live runner that has fetched its prompt. Overridable
	// via the runResolver.driveDispatchedStaleAfter seam (tests inject a tiny
	// value).
	defaultDriveDispatchedStaleAfter = 10 * time.Minute
)

// stopped_reason values (some are composed with a suffix at return).
const (
	stoppedMerged        = "merged"
	stoppedTimeout       = "timeout"
	stoppedStalled       = "stalled"
	stoppedStageFailed   = "stage_failed"
	stoppedUnrecordedAct = "unrecorded_act"
	stoppedCancelled     = "cancelled"
	stoppedRunFailed     = "run_failed"
	stoppedGateError     = "gate_error"
	// stoppedAmendmentCheckFailed is the fail-CLOSED stop when the scope-
	// amendment audit read errors: a pending amendment is always a human
	// decision, so an unreadable amendment state must halt the driver rather
	// than fall through to a stage dispatch.
	stoppedAmendmentCheckFailed = "amendment_check_failed"
	// stoppedDispatchCheckFailed is the fail-CLOSED stop when the prior-
	// dispatch-row audit read errors: an unreadable run_auto_driven state can
	// never be silently downgraded to "no prior dispatch", which on a resume of
	// a still-'dispatched' stage would let the loop record + host-spawn a SECOND
	// runner while the first is in flight. Halt rather than risk concurrent
	// repository command execution.
	stoppedDispatchCheckFailed = "dispatch_check_failed"
	// stoppedDispatchedStale is the stop when a stage this invocation did not
	// spawn has sat in 'dispatched' past the runner-liveness threshold AND the
	// driver's own host liveness probe (pgrep -f on the stage id, #1955) could
	// NOT confirm the runner dead — either a process matching the stage id is
	// LIVE yet never flipped 'running' (anomalous) or the probe was UNPROBEABLE
	// (pgrep absent / errored / timed out). A DEAD probe does NOT stop here — the
	// driver auto-recovers by re-dispatching. Post-#1912 'dispatched' means a
	// spawn attempt EXISTS, so this is never a parked-for-host-dispatch handoff
	// (that is 'awaiting_host_dispatch', which the loop auto-dispatches). The
	// driver hands the manual verify-first re-dispatch to the operator via
	// next_actions only on the ambiguous probe results.
	stoppedDispatchedStale = "dispatched_stale"
	// stoppedHostDispatchFailed is the fail-CLOSED stop when the host-dispatch
	// spawn-marker call (POST .../host-dispatch) errors between record-act and
	// spawn (#1912): the marker is the core 'dispatched' signal on the
	// code-execution path, so an unmarked spawn would recreate the ambiguity #1912
	// removes. NO runner is spawned — the operator re-invokes once the transient
	// clears.
	stoppedHostDispatchFailed = "host_dispatch_failed"
	// stoppedContextCancelled is the stop when the drive context is cancelled
	// (distinct from the run-state-derived 'cancelled', which reports a run the
	// backend moved to the cancelled terminal state).
	stoppedContextCancelled = "context_cancelled"
	// stoppedAcceptanceNeedsTarget is the resumable stop when the acceptance
	// target-identity gate (#1953) refuses to spawn: the admission endpoint
	// reported the plan needs live validation against a declared target and the
	// verb-side probe found that target unreachable or stale. No record-act and no
	// spawn — the stage stays awaiting_host_dispatch, so the drive is resumable
	// with the same run_id once the operator provisions the target at the named
	// head SHA.
	stoppedAcceptanceNeedsTarget = "acceptance_needs_target"
	// "paged:<event>" and "decision_required:<state>" are composed inline.
)

// driveActionMerge is the delegation verb the gate endpoint returns for a
// queued merge; mirrors backend delegation.ActionMerge (not imported per the
// thin local-copy rule).
const driveActionMerge = "merge"

// driveRunnerKindLocal is the only runner_kind fishhawk_drive_run will act on.
// The verb records + host-spawns a LOCAL runner for every dispatchable stage
// (local-only by design, ADR-024), so a non-local run must be rejected BEFORE
// any record/spawn reaches composeRunnerArgv — the run's persisted runner_kind
// is materialized non-empty by the backend (empty → github_actions), so a
// strict positive "== local" check is fail-closed against every other value.
const driveRunnerKindLocal = "local"

// DriveRunInput is the fishhawk_drive_run tool's input (#1700).
type DriveRunInput struct {
	RunID        string `json:"run_id" jsonschema:"Fishhawk run UUID; the local runner_kind:local run to drive between human gates"`
	WorkingDir   string `json:"working_dir,omitempty" jsonschema:"checkout the runner runs in; defaults to the MCP server's cwd"`
	GitHubRepo   string `json:"github_repo,omitempty" jsonschema:"GitHub repo as owner/name; auto-detected from working_dir's origin remote when empty"`
	BaseBranch   string `json:"base_branch,omitempty" jsonschema:"base branch for the implement-stage PR; defaults to main"`
	RunnerBinary string `json:"runner_binary,omitempty" jsonschema:"path to fishhawk-runner; resolved in order: input, FISHHAWK_RUNNER_BIN env, sibling to this binary, then PATH"`
	MaxMinutes   int    `json:"max_minutes,omitempty" jsonschema:"wall-clock budget in minutes for this drive; clamped to [1,240], default 60. Every return is resumable by re-invoking with the same run_id"`
}

// DriveStep is one act the driver took, labeled mechanical (a stage
// dispatch) vs delegated (a gate action taken under an ADR-040 rule).
type DriveStep struct {
	Kind      string `json:"kind" jsonschema:"dispatch (a mechanical stage spawn) or gate (a delegated gate action)"`
	Stage     string `json:"stage,omitempty" jsonschema:"the dispatched stage name (plan|implement|acceptance|fixup_redispatch) for a dispatch step"`
	Action    string `json:"action,omitempty" jsonschema:"the delegation verb for a gate step (approve|route_fixup|retry|merge)"`
	Delegated bool   `json:"delegated" jsonschema:"true for a delegated gate action, false for a mechanical dispatch"`
	Note      string `json:"note,omitempty"`
}

// DriveRunOutput is the drive verb's terminal report. Every outcome is
// resumable by re-invoking with the same run_id.
type DriveRunOutput struct {
	RunID         string       `json:"run_id"`
	StoppedReason string       `json:"stopped_reason" jsonschema:"why the drive stopped: merged | paged:<event> | decision_required:<state> | timeout | stalled | stage_failed | unrecorded_act | host_dispatch_failed | run_failed | cancelled | gate_error | amendment_check_failed | dispatch_check_failed | dispatched_stale | acceptance_needs_target | context_cancelled"`
	RunState      string       `json:"run_state"`
	StepsTaken    []DriveStep  `json:"steps_taken,omitempty" jsonschema:"the ordered acts the driver performed; each dispatch and gate act also landed a run_auto_driven audit row"`
	PageEvent     string       `json:"page_event,omitempty" jsonschema:"the must_page_human event, set only on a paged stop"`
	NextActions   *NextActions `json:"next_actions,omitempty" jsonschema:"the legal next operator moves, set on a decision_required / paged / stalled stop. fishhawk_get_run_status carries the full lifecycle block"`
	Warnings      []string     `json:"warnings,omitempty"`
}

// registerDriveRun wires the fishhawk_drive_run tool (#1700).
func registerDriveRun(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_drive_run",
		Description: strings.TrimSpace(`
Use this when you want the operator host to execute EVERY mechanical step
between human gates on a runner_kind:local run under ADR-040 delegation, and
stop at the first genuine decision — the local sibling of the GHA campaign
auto-driver (#1700).

It is a bounded, resumable loop: it dispatches only the EARLIEST non-terminal
stage once its gate preconditions hold (plan always, implement after plan
succeeds, acceptance after implement succeeds and every review settles), so a
fresh run whose stages are all created pending never dispatches implement or
acceptance while the plan runner still holds the lineage lock. For that
dispatchable stage it FIRST records the dispatch (POST
/v0/runs/{id}/auto-drive/acts) and only on a successful record host-spawns the
runner (record-before-dispatch makes an unaudited mechanical act impossible by
construction); it polls stages/reviews
to settle; and at every gate it calls POST /v0/runs/{id}/auto-drive, continuing
on a delegated act (approve/route_fixup/retry/merge) and returning immediately
on a page, on an observe-only outcome at a decision state (a plan gate without
may_approve, a split verdict), or on a pending scope amendment (no delegation
knob covers amendments, so every one is a decision).

A clean run under fully delegated knobs goes start_run -> merged with no
operator tool calls in between, and its audit trail carries a delegated-context
run_auto_driven row for EVERY driver dispatch and gate act. Every return is
resumable — re-invoke with the same run_id. stopped_reason names why it
stopped; next_actions names the legal moves at a decision. merge is
queued-not-landed, so merged is reported only after the webhook-settled
terminal run state.

Supply a progressToken on the call to receive a keep-alive heartbeat: the
driver emits an MCP notifications/progress update once per poll iteration
(run state + earliest non-terminal stage + steps taken + elapsed seconds), so
a >30-min drive is not aborted by the client's idle timeout. Progress is
opt-in per the MCP spec — no token means no heartbeat (the return is still
resumable). At a parked approval gate the driver waits for the stage's
advisory agent reviews to settle before calling the gate, so a delegated
approve fires only on settled reviews. A parked implement stage
(awaiting_host_dispatch) after a delegated plan approval is AUTO-DISPATCHED by
the loop — record-act, host-dispatch marker, then one spawn — with no manual
handoff (#1912). A stage left 'dispatched' (a spawn attempt exists) with no
runner ever observed past the liveness threshold triggers the driver's OWN host
liveness probe (pgrep -f scoped to the stage's --stage-id argv, #1955): a
NEGATIVE probe (no runner process) is auto-recovered in place — the driver
re-dispatches through its record + host-dispatch marker + spawn path with no
operator action. A LIVE-but-unregistered process stops dispatched_stale for
INSPECTION with NO re-dispatch (a second runner into the same lineage lock is
the failure this prevents); only an UNPROBEABLE result (pgrep absent / errored /
timed out) hands the manual verify-first re-dispatch to the operator.
A fresh manual fishhawk_dispatch_stage marks a new spawn so a re-invoked drive
reads it as live and polls to convergence rather than re-reporting stale.

Requires the fishhawk-runner binary to resolve on the MCP server's host, like
fishhawk_run_stage / fishhawk_dispatch_stage (local-only by design, ADR-024).
Reach for fishhawk_dispatch_stage instead to drive a single stage by hand.
`),
	}, resolver.driveRun)
}

// driveRun is the tool handler: the bounded, resumable drive loop.
func (r *runResolver) driveRun(ctx context.Context, req *mcp.CallToolRequest, in DriveRunInput) (*mcp.CallToolResult, DriveRunOutput, error) {
	if in.RunID == "" {
		return nil, DriveRunOutput{}, errors.New("run_id is required")
	}
	runUUID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, DriveRunOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}

	baseBranch := in.BaseBranch
	if baseBranch == "" {
		baseBranch = "main"
	}
	workingDir := in.WorkingDir
	if workingDir == "" {
		workingDir = "."
	}

	// Resolve the spawn seam + binary once. When a spawn seam is injected
	// (tests) the real binary resolution is skipped so the loop runs without a
	// runner on PATH; the production nil-seam path resolves the binary exactly
	// as fishhawk_dispatch_stage does.
	spawn := r.driveSpawn
	binary := in.RunnerBinary
	if spawn == nil {
		spawn = spawnRunnerStageDetached
		binary, err = resolveRunnerBinary(in.RunnerBinary, r.getenv)
		if err != nil {
			return nil, DriveRunOutput{}, err
		}
	}

	// Resolve the repo once (best-effort): implement dispatch needs it, but a
	// missing repo is a warning here — the per-stage spawn surfaces a real
	// failure rather than blocking the whole drive.
	var warnings []string
	repo := in.GitHubRepo
	if repo == "" {
		if detected, derr := runStageDetectGitHubRepo(workingDir); derr == nil {
			repo = detected
		} else {
			warnings = append(warnings,
				fmt.Sprintf("github_repo not set and origin auto-detect failed (%v); implement dispatch may fail to open a PR.", derr))
		}
	}

	maxMinutes := in.MaxMinutes
	if maxMinutes <= 0 {
		maxMinutes = defaultDriveMaxMinutes
	}
	if maxMinutes > maxDriveMaxMinutes {
		maxMinutes = maxDriveMaxMinutes
	}
	wall := time.Duration(maxMinutes) * time.Minute
	if r.driveMaxWallclock > 0 {
		wall = r.driveMaxWallclock
	}
	start := time.Now()
	deadline := start.Add(wall)

	pollInterval := r.drivePollInterval
	if pollInterval <= 0 {
		pollInterval = defaultDrivePollInterval
	}

	// Progress heartbeat (defect 2, #1905): a >30-min drive would otherwise be
	// aborted by the client's idle timeout. Capture the client-supplied
	// progressToken exactly as run_stage.go:443-447 / run_children.go:240-242
	// do, and emit a best-effort NotifyProgress once per poll iteration. MCP
	// progress is opt-in per spec: no token (or no session) -> no emission.
	var progToken any
	if req != nil && req.Params != nil {
		progToken = req.Params.GetProgressToken()
	}
	var progress float64

	out := DriveRunOutput{RunID: runUUID.String(), Warnings: warnings}

	spawned := map[string]bool{}        // stage IDs spawned this invocation (idempotency guard)
	dispatchedCount := map[string]int{} // stage ID -> dispatch count (fixup_redispatch discriminator)
	stall := 0
	var lastSig string
	mergeQueued := false // a delegated merge was queued; poll for the webhook-settle, don't re-act

	// QUEUED-MERGE MEMORY across invocations: a prior run_auto_driven act:gate
	// merge row means a merge was already queued on an earlier invocation, so a
	// resume during merge latency must poll for the webhook-settle instead of
	// re-calling the gate (no duplicate gate:merge act, no auto-merge re-enable).
	// FAIL-OPEN on a read error, unlike the dispatch-path fail-closed: this check
	// opens NO code-execution surface — the worst case of a false negative is the
	// pre-existing duplicate gate:merge attribution row, so fail-closed halting
	// would trade a benign duplicate for a wedge.
	if queued, merr := r.driveHasPriorGateMergeRow(ctx, runUUID); merr != nil {
		out.Warnings = append(out.Warnings,
			"prior gate:merge poll failed; continuing (fail-open, merge memory not seeded): "+merr.Error())
	} else if queued {
		mergeQueued = true
	}

	for {
		if time.Now().After(deadline) {
			out.StoppedReason = stoppedTimeout
			return nil, out, nil
		}
		if ctx.Err() != nil {
			out.StoppedReason = stoppedContextCancelled
			out.Warnings = append(out.Warnings, "context cancelled: "+ctx.Err().Error())
			return nil, out, nil
		}

		runRow, gerr := r.api.GetRun(ctx, runUUID)
		if gerr != nil {
			return nil, out, fmt.Errorf("drive: get run: %w", gerr)
		}
		// Local-only guard (authz): the drive loop records + host-spawns a LOCAL
		// runner for every dispatchable stage, so a run whose runner_kind is not
		// 'local' must be rejected BEFORE anything reaches the record-act /
		// composeRunnerArgv / spawn seam — no non-local run may expand the host
		// code-execution surface. This is a permanent misuse (a github_actions run
		// never becomes local), so it fails loud as an error rather than a
		// resumable stopped_reason. Checked every iteration but returns on the
		// first, so a runner_kind that somehow flips mid-drive is also caught.
		if runRow.RunnerKind != driveRunnerKindLocal {
			kind := runRow.RunnerKind
			if kind == "" {
				kind = "github_actions" // backend default when unset
			}
			return nil, DriveRunOutput{}, fmt.Errorf(
				"run %s is runner_kind=%s, but fishhawk_drive_run is local-only (ADR-024): it records and host-spawns a LOCAL runner for every stage. To drive a run from the operator host, start a NEW run with runner_kind=local; a github_actions run executes through the Actions workflow channel",
				runUUID, kind)
		}
		out.RunState = runRow.State
		if runStateIsTerminal(runRow.State) {
			switch runRow.State {
			case "succeeded":
				out.StoppedReason = stoppedMerged
			case "failed":
				out.StoppedReason = stoppedRunFailed
			case "cancelled":
				out.StoppedReason = stoppedCancelled
			}
			return nil, out, nil
		}

		// A delegated merge was queued on a prior iteration: GitHub auto-merge
		// settles it via the pull_request webhook (merge is queued-not-landed).
		// Poll the run state ONLY — never re-call the gate, which would
		// re-enable auto-merge and append a duplicate run_auto_driven act:gate
		// merge row every interval (and, if the seam rejects re-enabling on an
		// already-enabled PR, exit gate_error) — until the run settles terminal
		// (handled at the top of the loop) or the deadline elapses.
		if mergeQueued {
			driveSleep(ctx, pollInterval)
			continue
		}

		stages, serr := r.api.ListRunStages(ctx, runUUID)
		if serr != nil {
			return nil, out, fmt.Errorf("drive: list stages: %w", serr)
		}

		// Progress heartbeat once per loop iteration (best-effort, opt-in): a
		// healthy long drive keeps the client's idle timeout from aborting it.
		// Emitted only when the caller supplied a progressToken AND the request
		// carries a live session (the go-sdk emission seam, mirroring
		// run_stage.go:1117). A failed notify is swallowed — the drive is
		// authoritative, the heartbeat advisory.
		if progToken != nil && req != nil && req.Session != nil {
			progress++
			_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
				ProgressToken: progToken,
				Progress:      progress,
				Message:       driveProgressMessage(runRow, stages, len(out.StepsTaken), time.Since(start)),
			})
		}

		// (b) A pending scope amendment is ALWAYS a decision — no delegation
		// knob covers amendments, and its window times out in minutes.
		if pending, aerr := r.driveScopeAmendmentPending(ctx, runUUID); aerr != nil {
			// FAIL-CLOSED: we cannot confirm no amendment is parked, and a
			// pending amendment is a human decision. STOP rather than fall
			// through to (c) dispatch — an unreadable amendment state must never
			// open the code-execution path (concern: audit-read failure was
			// downgraded to a warning and the loop continued into record+spawn).
			out.StoppedReason = stoppedAmendmentCheckFailed
			out.Warnings = append(out.Warnings, "scope-amendment poll failed; stopping fail-closed: "+aerr.Error())
			out.NextActions = driveDecisionActions("scope_amendment_requested", runUUID, in.WorkingDir)
			return nil, out, nil
		} else if pending {
			out.StoppedReason = "decision_required:scope_amendment_requested"
			out.NextActions = driveDecisionActions("scope_amendment_requested", runUUID, in.WorkingDir)
			return nil, out, nil
		}

		// (c) Dispatch the first host-dispatchable stage — record BEFORE spawn.
		if disp := driveDispatchableStage(stages, spawned); disp != nil {
			stall = 0
			recordName := driveDispatchName(*disp, dispatchedCount)
			// A prior run_auto_driven dispatch row for this stage name is a
			// crash-resume / re-open signal (drives the retry note below).
			// FAIL-CLOSED on a read error: an unreadable audit state must NEVER be
			// silently downgraded to "no prior row". On a resume that observes a
			// still-'dispatched' stage, downgrading would let the loop record +
			// host-spawn a SECOND runner while the first is in flight (concurrent
			// repository command execution). Halt instead.
			priorRow, newestDispatchSeq, herr := r.driveHasPriorDispatchRow(ctx, runUUID, recordName)
			if herr != nil {
				out.StoppedReason = stoppedDispatchCheckFailed
				out.Warnings = append(out.Warnings,
					fmt.Sprintf("prior-dispatch-row poll for %s failed; stopping fail-closed: %v", recordName, herr))
				out.NextActions = driveDecisionActions("stalled", runUUID, in.WorkingDir)
				return nil, out, nil
			}
			// Cross-invocation in-flight guard (#1912): post-#1912 a 'dispatched'
			// stage THIS invocation did not spawn (dispatchedCount==0) has a runner
			// in flight — the host-dispatch spawn marker CAS-flipped it 'dispatched'
			// from a PRIOR driver invocation OR a manual fishhawk_dispatch_stage. A
			// fresh invocation's per-run `spawned` map cannot see it, and
			// driveDispatchableStage returns it. Treat it as in-flight and POLL;
			// never host-spawn a SECOND runner for the same stage. Only past the
			// liveness threshold with no running flip do we probe host liveness
			// (#1955): a DEAD probe auto-recovers by falling through to the
			// record+spawn path below; a LIVE or UNPROBEABLE result stops
			// dispatched_stale and NEVER spawns. A 'pending'/'awaiting_host_dispatch'
			// stage never reaches here (its state is not 'dispatched'); it is recorded
			// + host-spawned below, which is the auto-dispatch of a parked implement
			// (#1912).
			staleRedispatch := false
			if disp.State == "dispatched" && dispatchedCount[disp.ID] == 0 {
				staleAfter := r.driveDispatchedStaleAfter
				if staleAfter <= 0 {
					staleAfter = defaultDriveDispatchedStaleAfter
				}
				// Anchor staleness on the newest spawn timestamp: max(UpdatedAt,
				// StartedAt). Post-#1912 the awaiting_host_dispatch → dispatched spawn
				// flip stamps UpdatedAt, so UpdatedAt IS the spawn signal — the #1905
				// dispatch-row timestamp max-in is removed (the marker, not a separate
				// audit row, is now the timestamp of record). StartedAt (set on the
				// running flip) can only be newer. A zero-value anchor (no timestamped
				// evidence) degrades to polling — fail toward polling, never toward a
				// stale stop or a spawn.
				anchor := disp.UpdatedAt
				if disp.StartedAt != nil && disp.StartedAt.After(anchor) {
					anchor = *disp.StartedAt
				}
				if !anchor.IsZero() && time.Since(anchor) > staleAfter {
					age := time.Since(anchor).Round(time.Second)
					// Past the threshold: probe host runner liveness ourselves rather
					// than unconditionally handing the pgrep to the operator (#1955). The
					// MCP server runs on the host that spawned the runner (ADR-024) and the
					// runner argv carries `--stage-id <uuid>`, so the probe is precise.
					probe := r.driveProbeRunnerLiveness
					if probe == nil {
						probe = probeRunnerLiveness
					}
					switch probe(ctx, disp.ID) {
					case runnerDead:
						// Negative probe: no process carries this stage id, so the spawned
						// runner genuinely died. AUTO-RECOVER — do NOT stop; warn and fall
						// through to the record-act → host-dispatch marker → spawn sequence
						// below, carrying a stale-re-dispatch note. dispatchedCount stays 0
						// here so the fixup-attribution computation below still runs; the
						// spawn then marks spawned[disp.ID]/dispatchedCount so this guard
						// cannot re-trip this invocation and driveAnyInFlight polls the fresh
						// runner.
						out.Warnings = append(out.Warnings, fmt.Sprintf(
							"stage %s (%s) sat in 'dispatched' for %s, past the %s runner-liveness threshold; a host liveness probe (pgrep -f on the stage id) found NO matching runner process, so the spawn died at or just after launch — auto-re-dispatching (stale re-dispatch: liveness probe found no runner process).",
							disp.Type, disp.ID, age, staleAfter))
						staleRedispatch = true
						// fall through (NOT continue/return) to record + spawn below.
					case runnerLive:
						// A process carrying this stage id IS alive yet never flipped
						// 'running' — anomalous (a live process stuck past the prompt-fetch
						// window). STOP and NEVER spawn: a second runner into the same lineage
						// lock stays impossible by construction.
						out.StoppedReason = stoppedDispatchedStale
						out.Warnings = append(out.Warnings, fmt.Sprintf(
							"stage %s (%s) has sat in 'dispatched' for %s, past the %s runner-liveness threshold, yet a host process matching this stage id IS live (pgrep -f on the stage id) and never fetched its prompt to flip it to 'running' — anomalous. Inspect the dispatch's log_path; do NOT re-dispatch while that process lives, because a second runner into the same lineage lock is the failure this driver exists to prevent.",
							disp.Type, disp.ID, age, staleAfter))
						// A LIVE process is confirmed present, so the manual verify-and-
						// re-dispatch instruction MUST NOT survive here (it belongs only to
						// the UNKNOWN/unprobeable branch): re-dispatching would spawn the very
						// second runner this driver exists to prevent. Hand back an
						// inspect-only next action instead.
						out.NextActions = driveDecisionActions("dispatched_stale_live", runUUID, in.WorkingDir)
						return nil, out, nil
					default: // runnerUnknown
						// pgrep absent / exit ≥2 / timeout / other exec error: we cannot
						// confirm the runner dead, so DEGRADE to today's manual verify-first
						// stop verbatim — never auto-spawn on an ambiguous probe.
						out.StoppedReason = stoppedDispatchedStale
						out.Warnings = append(out.Warnings, fmt.Sprintf(
							"stage %s (%s) has sat in 'dispatched' for %s (newest spawn timestamp), past the %s runner-liveness threshold, and no prompt fetch flipped it to 'running' — the spawned runner never reached its prompt fetch (it died at or just after spawn). Before re-dispatching, FIRST verify no runner process is live on this host — check `pgrep -f fishhawk-runner` and the dispatch's log_path — because a second runner into the same lineage lock is the failure this driver exists to prevent. Only if none is live, re-dispatch by hand with fishhawk_dispatch_stage, then re-invoke fishhawk_drive_run.",
							disp.Type, disp.ID, age, staleAfter))
						out.NextActions = driveDecisionActions("dispatched_stale", runUUID, in.WorkingDir)
						return nil, out, nil
					}
				} else {
					// Anchor fresh (or the zero-value degrade): poll and re-evaluate the
					// threshold next iteration. Deliberately NOT a one-shot spawned[] mark —
					// leaving the stage dispatchable lets this guard re-trip once the
					// threshold passes mid-invocation.
					driveSleep(ctx, pollInterval)
					continue
				}
			}
			note := ""
			if priorRow {
				// A run_auto_driven dispatch row already exists for this stage
				// name and the stage is still dispatchable (pending) — a
				// crash-resume. Re-record honestly with a retry note.
				note = "retry"
			}
			// Cross-invocation fixup_redispatch attribution: a fresh invocation
			// (dispatchedCount==0) re-dispatching a still-'pending' implement stage
			// that a prior invocation already dispatched, where a
			// stage_fixup_triggered row is NEWER than that newest implement dispatch
			// row, is a fix-up re-open from a prior invocation. Attribute the record
			// as fixup_redispatch rather than a generic implement retry.
			// recordName=="implement" excludes the intra-invocation path
			// (dispatchedCount>0), which driveDispatchName already names
			// fixup_redispatch.
			if disp.Type == "implement" && recordName == "implement" && dispatchedCount[disp.ID] == 0 && priorRow {
				fixupSeq, ferr := r.driveNewestFixupTriggeredSeq(ctx, runUUID)
				if ferr != nil {
					// FAIL-CLOSED on the code-execution path, exactly as the prior-
					// dispatch-row read: never downgrade an unreadable audit state,
					// which precedes a record + host-spawn.
					out.StoppedReason = stoppedDispatchCheckFailed
					out.Warnings = append(out.Warnings,
						fmt.Sprintf("fixup-attribution poll failed; stopping fail-closed: %v", ferr))
					out.NextActions = driveDecisionActions("stalled", runUUID, in.WorkingDir)
					return nil, out, nil
				}
				if fixupSeq > newestDispatchSeq {
					recordName = "fixup_redispatch"
					note = ""
				}
			}
			if staleRedispatch {
				// A dead-runner auto-recovery (#1955): mark the record-act note so the
				// audit trail distinguishes this stale re-dispatch (after a negative
				// liveness probe) from a plain dispatch or a crash-resume retry. Applied
				// after the fixup-attribution block so recordName's fixup_redispatch vs
				// implement classification is preserved while the note names the recovery.
				note = staleRedispatchNote
			}
			// Acceptance-dispatch admission (#1928): before recording + spawning
			// an acceptance dispatch, ask the backend to short-circuit an
			// all-skip-with-basis / empty-criteria / out-of-scope plan. On a hit
			// the stage settles server-side (a passed verdict, no preview) — record
			// NO act, spawn nothing, and let the next poll observe the terminal
			// stage. Fail OPEN on a TRANSPORT error (network / 5xx): append a warning
			// and fall through to today's record+spawn path (which opens no NEW
			// code-execution surface); a short_circuited:false result is the normal
			// no-op with no warning. A 4xx admission REJECTION (403
			// cross_run_admission etc.) is NOT fail-open — it halts the drive so a
			// runner never spawns after the backend rejected the request on
			// authorization grounds.
			if disp.Type == "acceptance" {
				accStageUUID, perr := uuid.Parse(disp.ID)
				if perr != nil {
					return nil, out, fmt.Errorf("drive: resolved stage_id %q not a UUID: %w", disp.ID, perr)
				}
				admission, warn, admitErr := r.maybeShortCircuitAcceptance(ctx, runUUID, accStageUUID)
				if admitErr != nil {
					return nil, out, admitErr
				}
				if warn != "" {
					out.Warnings = append(out.Warnings, warn)
				}
				if admission != nil && admission.ShortCircuited {
					out.StepsTaken = append(out.StepsTaken, DriveStep{
						Kind: "dispatch", Stage: "acceptance", Delegated: false,
						Note: fmt.Sprintf("acceptance short-circuited server-side (%s); no runner spawned, no act recorded", shortCircuitLabel(admission)),
					})
					spawned[disp.ID] = true
					driveSleep(ctx, pollInterval)
					continue
				}
				// Acceptance target-identity gate (#1953): a needs_target admission
				// means the runner would validate against a live target — probe it
				// FROM THIS HOST BEFORE recording an act or spawning. Unreachable/
				// stale STOPS the drive resumably (acceptance_needs_target) without
				// record-act or spawn, so the stage stays awaiting_host_dispatch and a
				// re-invocation with the same run_id dispatches cleanly once the
				// operator provisions the target. Verified/unverifiable/no-hosts/
				// preview-cmd-set/empty-SHA all proceed (nil refusal).
				if refusal, gwarn := r.checkAcceptanceTarget(ctx, admission); refusal != nil {
					out.StepsTaken = append(out.StepsTaken, DriveStep{
						Kind: "dispatch", Stage: "acceptance", Delegated: false,
						Note: fmt.Sprintf(
							"acceptance target %q not ready for head %s (%s); NOT spawning — %s",
							refusal.TargetHost, refusal.ExpectedHeadSHA, refusal.Detail, refusal.Remediation),
					})
					out.StoppedReason = stoppedAcceptanceNeedsTarget
					out.NextActions = driveDecisionActions(stoppedAcceptanceNeedsTarget, runUUID, in.WorkingDir)
					return nil, out, nil
				} else if gwarn != "" {
					out.Warnings = append(out.Warnings, gwarn)
				}
			}

			if _, rerr := r.api.RecordAutoDriveAct(ctx, runUUID, RecordAutoDriveAct{
				Action: autoDriveDispatchActionName,
				Stage:  recordName,
				Source: driveSourceTag,
				Note:   note,
			}); rerr != nil {
				// FAIL by construction: the record failed, so we do NOT spawn.
				out.StoppedReason = stoppedUnrecordedAct
				out.Warnings = append(out.Warnings,
					fmt.Sprintf("record-act for %s failed; NOT dispatching: %v", recordName, rerr))
				return nil, out, nil
			}

			stageUUID, perr := uuid.Parse(disp.ID)
			if perr != nil {
				return nil, out, fmt.Errorf("drive: resolved stage_id %q not a UUID: %w", disp.ID, perr)
			}
			// Mark the host spawn BEFORE spawning (#1912), between record-act and
			// spawn: the endpoint CAS-flips {pending, awaiting_host_dispatch} →
			// dispatched so post-#1912 'dispatched' unambiguously means a spawn
			// attempt exists — the signal the in-flight/stale guard above anchors on.
			// FAIL CLOSED — a transport error or 4xx means NO spawn (an unmarked
			// spawn would recreate the ambiguity #1912 removes). transitioned:false
			// (already 'dispatched') proceeds; the guard above already handled the
			// unspawned-this-invocation dispatched case, so reaching here means a
			// pending/awaiting_host_dispatch stage the marker flips forward.
			if _, hderr := r.api.HostDispatchStage(ctx, runUUID, stageUUID); hderr != nil {
				out.StoppedReason = stoppedHostDispatchFailed
				out.Warnings = append(out.Warnings, fmt.Sprintf(
					"host-dispatch marker for %s failed; NOT spawning (fail-closed): %v", recordName, hderr))
				return nil, out, nil
			}
			argv := r.composeRunnerArgv(RunStageInput{
				RunID:      in.RunID,
				Workflow:   runRow.WorkflowID,
				Stage:      disp.Type,
				WorkingDir: in.WorkingDir,
				GitHubRepo: repo,
				BaseBranch: baseBranch,
			}, disp.ID, repo, baseBranch, true)
			env := append(os.Environ(), "FISHHAWK_API_TOKEN="+r.api.token)
			report := func(ctx context.Context, category, reason, detail string, exitCode int) error {
				_, e := r.api.ReportStageFailure(ctx, runUUID, stageUUID, category, reason, detail, exitCode)
				return e
			}
			if _, spwErr := spawn(binary, argv, env, runUUID.String(), disp.ID, report); spwErr != nil {
				out.StoppedReason = stoppedStageFailed
				out.Warnings = append(out.Warnings,
					fmt.Sprintf("spawn of %s stage failed: %v", disp.Type, spwErr))
				return nil, out, nil
			}
			spawned[disp.ID] = true
			dispatchedCount[disp.ID]++
			stepNote := "mechanical stage dispatch"
			if staleRedispatch {
				stepNote = "stale re-dispatch after a negative liveness probe (auto-recovery)"
			}
			out.StepsTaken = append(out.StepsTaken, DriveStep{
				Kind: "dispatch", Stage: recordName, Delegated: false,
				Note: stepNote,
			})
			driveSleep(ctx, pollInterval)
			continue
		}

		// (d) A stage or review is still in flight — poll.
		if driveAnyInFlight(stages, spawned) {
			stall = 0
			driveSleep(ctx, pollInterval)
			continue
		}

		// (e-pre) Review-settlement wait at a parked approval gate (defect 1,
		// #1905). A feature_change run creates its human 'review' stage row
		// 'pending' at run creation, so once the plan stage parks
		// awaiting_approval the loop must not spin gate-calls while the advisory
		// agent reviews are still landing: the delegated may_approve can only fire
		// on settled reviews. When a parked approval gate's advisory round is still
		// 'pending' (the #1127 count-based primitive), poll instead of gate-calling.
		if parkedType := driveParkedApprovalStageType(stages); parkedType != "" {
			if reviewStage := driveReviewStageForParkedType(parkedType); reviewStage != "" {
				st, rerr := r.reviewStatusFor(ctx, runUUID, reviewStage)
				switch {
				case rerr != nil:
					// FAIL TOWARD THE OPERATOR: an unreadable review state at a
					// parked gate appends a warning and falls through to the
					// gate/decision return (which hands the operator a decision).
					// Unlike the dispatch-path fail-closed, this path opens no
					// code-execution surface, so failing toward a decision — not a
					// wedge — is correct.
					out.Warnings = append(out.Warnings, fmt.Sprintf(
						"review-status poll for the parked %s gate failed; falling through to the gate decision: %v",
						parkedType, rerr))
				case st != nil && st.Status == "pending":
					// Reviews still landing: wait for settlement rather than a noisy
					// observe-only gate call every interval.
					stall = 0
					driveSleep(ctx, pollInterval)
					continue
				}
			}
		}

		// (e) Parked at a gate — call the auto-drive endpoint.
		gate, err := r.api.AutoDriveRunGate(ctx, runUUID)
		if err != nil {
			// FAIL-LOUD: the endpoint surfaces a supplementary-append or
			// dispatch failure as an error; stop acting and surface it.
			out.StoppedReason = stoppedGateError
			out.Warnings = append(out.Warnings, "auto-drive gate error: "+err.Error())
			return nil, out, nil
		}
		switch {
		case gate.Paged:
			out.StepsTaken = append(out.StepsTaken, DriveStep{
				Kind: "gate", Action: "page", Note: gate.PageEvent,
			})
			out.StoppedReason = "paged:" + gate.PageEvent
			out.PageEvent = gate.PageEvent
			out.NextActions = driveDecisionActions("paged", runUUID, in.WorkingDir)
			return nil, out, nil
		case gate.Acted:
			stall = 0
			// A gate action re-shapes the stage topology (a re-opened stage on
			// retry/route_fixup); clear the per-invocation spawn guard so a
			// re-opened stage is re-dispatched rather than treated as in-flight.
			spawned = map[string]bool{}
			out.StepsTaken = append(out.StepsTaken, DriveStep{
				Kind: "gate", Action: gate.Action, Delegated: true, Note: gate.Note,
			})
			// A merge is queued-not-landed: remember it so subsequent iterations
			// poll for the webhook-settle instead of re-calling the gate (which
			// would duplicate the gate:merge attribution row and re-enable
			// auto-merge every interval).
			if gate.Action == driveActionMerge {
				mergeQueued = true
			}
			driveSleep(ctx, pollInterval)
			continue
		default:
			// Observe-only. If a real gate is parked (a stage awaiting
			// approval the driver could not auto-act), it is a decision.
			if state := driveParkedGateState(stages); state != "" {
				out.StoppedReason = "decision_required:" + state
				out.NextActions = driveDecisionActions(state, runUUID, in.WorkingDir)
				return nil, out, nil
			}
			// No gate, nothing dispatchable/in-flight, run not terminal — guard
			// against spinning.
			sig := driveSignature(runRow, stages)
			if sig == lastSig {
				stall++
			} else {
				stall = 0
				lastSig = sig
			}
			if stall >= driveStallThreshold {
				out.StoppedReason = stoppedStalled
				out.NextActions = driveDecisionActions("stalled", runUUID, in.WorkingDir)
				return nil, out, nil
			}
			driveSleep(ctx, pollInterval)
		}
	}
}

// autoDriveDispatchActionName is the record-act action the drive verb sends;
// it mirrors the backend's autoDriveDispatchAction ("dispatch_stage").
const autoDriveDispatchActionName = "dispatch_stage"

// staleRedispatchNote is the RecordAutoDriveAct note the driver stamps on a
// dead-runner auto-recovery (#1955): a 'dispatched' stage the driver did not
// spawn, past the liveness threshold, whose host liveness probe found no runner
// process, re-dispatched by falling through to the record+spawn path.
const staleRedispatchNote = "stale re-dispatch: liveness probe found no runner process"

// driveSleep waits for d or until ctx is cancelled.
func driveSleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// driveDispatchableStage returns the EARLIEST non-terminal stage when — and
// only when — it is a plan/implement/acceptance stage this invocation has not
// already spawned, with its gate preconditions satisfied, whose state is one the
// caller acts on: {pending, awaiting_host_dispatch} (host-SPAWNABLE — the loop
// records + host-spawns it, #1912) OR 'dispatched' (in-flight — the caller's
// dispatched-guard polls it or, past the liveness threshold, hands back a manual
// re-dispatch; it is NEVER spawned). It returns nil when nothing here is
// actionable: the earliest non-terminal stage is a review stage (server-driven,
// polled by driveAnyInFlight) or a plan/implement/acceptance stage parked in
// awaiting_approval/running/blocked (the gate branch / poll own those).
//
// awaiting_host_dispatch (#1912) is the NEW host-spawnable park: a runner_kind-
// locked-local agent stage the backend parked for a host spawn (e.g. a parked
// implement after a delegated plan approval). Returning it here is what lets the
// loop AUTO-DISPATCH it with no manual handoff — the issue's primary done-means.
// 'dispatched' is returned only so the caller's in-flight/stale guard can route
// it; it is never carried through to a spawn.
//
// Gating on the EARLIEST non-terminal stage is the load-bearing fix (run
// fdcc17cd): a fresh run creates every stage 'pending', and the prior lowest-
// sequence-dispatchable rule dispatched implement and acceptance the moment plan
// was spawned — both then died category-C on the lineage lock the plan runner
// held. Only the earliest non-terminal stage can be host-dispatched, so plan
// runs alone until it settles. The precondition check
// (driveGatePreconditionsMet) is a belt-and-suspenders mirror of the backend
// drive rules for any unexpected sequence layout: implement requires a preceding
// plan stage to have succeeded; acceptance requires the implement stage to have
// succeeded and every review stage to be terminal.
func driveDispatchableStage(stages []Stage, spawned map[string]bool) *Stage {
	var earliest *Stage
	for i := range stages {
		st := &stages[i]
		if stageStateIsTerminal(st.State) {
			continue
		}
		if earliest == nil || st.Sequence < earliest.Sequence {
			earliest = st
		}
	}
	if earliest == nil {
		return nil
	}
	switch earliest.Type {
	case "plan", "implement", "acceptance":
	default:
		return nil // earliest non-terminal is a review stage → nothing host-dispatchable
	}
	switch earliest.State {
	case "pending", "awaiting_host_dispatch", "dispatched":
		// spawnable ({pending, awaiting_host_dispatch}) or in-flight ('dispatched',
		// routed to the caller's dispatched-guard) — actionable here.
	default:
		return nil // parked at a gate / running / blocked → not host-dispatchable
	}
	if spawned[earliest.ID] {
		return nil // already spawned this invocation → driveAnyInFlight polls it
	}
	if !driveGatePreconditionsMet(*earliest, stages) {
		return nil
	}
	return earliest
}

// driveGatePreconditionsMet reports whether the gate preconditions for host-
// dispatching st hold, mirroring the backend drive rules: plan is always
// dispatchable; implement requires any existing plan stage to have succeeded;
// acceptance requires the implement stage to have succeeded AND every review
// stage to be terminal. A defensive backstop for an unexpected sequence layout —
// the earliest-non-terminal ordering in driveDispatchableStage already prevents
// the common premature-dispatch case.
func driveGatePreconditionsMet(st Stage, stages []Stage) bool {
	switch st.Type {
	case "implement":
		for i := range stages {
			if stages[i].Type == "plan" && stages[i].State != "succeeded" {
				return false
			}
		}
		return true
	case "acceptance":
		implementSucceeded := false
		for i := range stages {
			s := &stages[i]
			if s.Type == "implement" && s.State == "succeeded" {
				implementSucceeded = true
			}
			if s.Type == "review" && !stageStateIsTerminal(s.State) {
				return false
			}
		}
		return implementSucceeded
	default: // plan
		return true
	}
}

// driveAnyInFlight reports whether any stage is still executing: a running
// stage, a server-driven review stage that is actually triggered, or a stage
// this invocation already spawned that has not yet advanced.
//
// Reachability (defect 1, #1905): a review-type stage in 'pending' counts as
// in-flight ONLY when it is REACHABLE — every lower-sequence stage terminal. A
// feature_change run creates its human 'review' stage row 'pending' at run
// creation (CreateStagesFromSpec), so an unconditional pending-review-is-in-
// flight rule polls branch (d) forever once the plan gate parks awaiting_
// approval, and the gate/decision branch (e) is never reached — the #1905
// silent hang. A 'dispatched' review stage is always in-flight (the server has
// triggered it), regardless of predecessor state.
func driveAnyInFlight(stages []Stage, spawned map[string]bool) bool {
	for i := range stages {
		st := &stages[i]
		if st.State == "running" {
			return true
		}
		if st.Type == "review" && st.State == "dispatched" {
			return true
		}
		if st.Type == "review" && st.State == "pending" && driveLowerStagesTerminal(stages, st.Sequence) {
			return true
		}
		if spawned[st.ID] && (st.State == "pending" || st.State == "dispatched") {
			return true
		}
	}
	return false
}

// driveLowerStagesTerminal reports whether every stage with a sequence lower
// than seq is terminal — i.e. the stage at seq is reachable (un-triggered
// predecessors would mean it has not started). Used to distinguish a pending
// review stage that is genuinely in flight from one that is merely a row
// created pending at run creation whose predecessor has not yet succeeded.
func driveLowerStagesTerminal(stages []Stage, seq int) bool {
	for i := range stages {
		if stages[i].Sequence < seq && !stageStateIsTerminal(stages[i].State) {
			return false
		}
	}
	return true
}

// driveParkedApprovalStageType returns the type of a stage parked at an
// approval gate (awaiting_approval), else "". The review-settlement wait maps
// it to the advisory review round to poll before calling the gate.
func driveParkedApprovalStageType(stages []Stage) string {
	for i := range stages {
		if stages[i].State == "awaiting_approval" {
			return stages[i].Type
		}
	}
	return ""
}

// driveReviewStageForParkedType maps a parked approval-gate stage type to the
// reviewStatusFor stage whose advisory round gates it: a parked plan stage
// waits on the 'plan' review, a parked review-type stage on the 'implement'
// review. Every other parked type ("" return) skips the review-settlement
// wait and falls straight through to the gate call.
func driveReviewStageForParkedType(parkedType string) string {
	switch parkedType {
	case "plan":
		return "plan"
	case "review":
		return "implement"
	default:
		return ""
	}
}

// driveParkedGateState returns a short state label when a stage is parked at
// an approval gate (awaiting_approval), else "".
func driveParkedGateState(stages []Stage) string {
	for i := range stages {
		if stages[i].State == "awaiting_approval" {
			return stages[i].Type + "_gate_parked"
		}
	}
	return ""
}

// driveDispatchName maps a dispatchable stage to its record-act stage name.
// A re-dispatch of the same implement stage (a delegated route_fixup/retry
// re-open) is recorded as fixup_redispatch.
func driveDispatchName(st Stage, dispatched map[string]int) string {
	if st.Type == "implement" && dispatched[st.ID] > 0 {
		return "fixup_redispatch"
	}
	return st.Type
}

// driveSignature is a cheap change-detector for the stall guard: the run
// state plus each stage's id:state.
func driveSignature(run *Run, stages []Stage) string {
	var b strings.Builder
	b.WriteString(run.State)
	for i := range stages {
		b.WriteByte('|')
		b.WriteString(stages[i].ID)
		b.WriteByte(':')
		b.WriteString(stages[i].State)
	}
	return b.String()
}

// driveScopeAmendmentPending reports whether the run has an undecided scope
// amendment: more scope_amendment_requested entries than scope_amendment_decided.
func (r *runResolver) driveScopeAmendmentPending(ctx context.Context, runID uuid.UUID) (bool, error) {
	req, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{Category: "scope_amendment_requested", Limit: 200})
	if err != nil {
		return false, err
	}
	if len(req) == 0 {
		return false, nil
	}
	dec, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{Category: "scope_amendment_decided", Limit: 200})
	if err != nil {
		return false, err
	}
	return len(req) > len(dec), nil
}

// driveHasPriorDispatchRow reports whether a run_auto_driven dispatch row for
// the given dispatch stage name already exists — a crash-resume or re-open
// signal so the re-record carries an honest retry note — and returns the highest
// audit sequence among the matching rows (the cross-invocation fixup-attribution
// input). Post-#1912 it is used for ATTRIBUTION ONLY (the retry note + the
// fixup_redispatch discriminator); the staleness anchor no longer reads a
// dispatch-row timestamp — the host-dispatch spawn marker stamps the
// 'dispatched' updated_at that is now the spawn signal. For an implement stage
// the match set includes BOTH 'implement' and 'fixup_redispatch' dispatch rows
// (a fix-up re-dispatch is still an implement dispatch), so the returned sequence
// is the newest implement-family dispatch — the caller compares it against the
// newest stage_fixup_triggered row to attribute a cross-invocation fix-up
// re-open.
//
// The match predicate is deliberately act=='dispatch' + stage family, SOURCE-
// and action-value-agnostic: the endpoint rejects any action other than
// autoDriveDispatchActionName ('dispatch_stage') with a 400 that appends
// nothing (autodrive_http.go), so driver rows (source fishhawk_drive_run) and
// manual fishhawk_dispatch_stage rows are ONE dispatch-evidence vocabulary by
// server-side construction — matching on act+stage is provably complete over
// it.
func (r *runResolver) driveHasPriorDispatchRow(ctx context.Context, runID uuid.UUID, stageName string) (bool, int64, error) {
	rows, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{Category: CategoryRunAutoDriven, Limit: 500})
	if err != nil {
		return false, 0, err
	}
	found := false
	var newestSeq int64
	for _, e := range rows {
		fields, ok := e.Payload.(map[string]any)
		if !ok {
			continue
		}
		if fields["act"] != "dispatch" {
			continue
		}
		stage, _ := fields["stage"].(string)
		match := stage == stageName
		if stageName == "implement" && stage == "fixup_redispatch" {
			match = true
		}
		if match {
			found = true
			if e.Sequence > newestSeq {
				newestSeq = e.Sequence
			}
		}
	}
	return found, newestSeq, nil
}

// driveProgressMessage builds the per-iteration heartbeat message: the run
// state, the earliest non-terminal stage type:state, the number of driver
// steps taken so far, and the elapsed wall-clock seconds. Pure (no I/O) so a
// table test pins it (#1905).
func driveProgressMessage(run *Run, stages []Stage, steps int, elapsed time.Duration) string {
	stagePart := "no non-terminal stage"
	var earliest *Stage
	for i := range stages {
		st := &stages[i]
		if stageStateIsTerminal(st.State) {
			continue
		}
		if earliest == nil || st.Sequence < earliest.Sequence {
			earliest = st
		}
	}
	if earliest != nil {
		stagePart = earliest.Type + ":" + earliest.State
	}
	return fmt.Sprintf("drive: run %s; next %s; steps %d; elapsed %ds",
		run.State, stagePart, steps, int(elapsed.Seconds()))
}

// driveNewestFixupTriggeredSeq returns the highest audit sequence among the
// run's stage_fixup_triggered rows (0 when none). A trigger newer than the
// newest implement dispatch row means a pending implement stage a prior
// invocation already dispatched is a fix-up re-open, attributed
// fixup_redispatch on the re-record.
func (r *runResolver) driveNewestFixupTriggeredSeq(ctx context.Context, runID uuid.UUID) (int64, error) {
	rows, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{Category: categoryStageFixupTriggered, Limit: 500})
	if err != nil {
		return 0, err
	}
	var newest int64
	for _, e := range rows {
		if e.Sequence > newest {
			newest = e.Sequence
		}
	}
	return newest, nil
}

// driveHasPriorGateMergeRow reports whether a run_auto_driven act:gate
// action:merge row already exists — the cross-invocation signal that a merge was
// queued on an earlier invocation, so a resume seeds mergeQueued and polls for
// the webhook-settle instead of re-calling the gate. The payload shape mirrors
// appendRunAutoDrivenGate (backend/internal/server/autodrive_http.go).
func (r *runResolver) driveHasPriorGateMergeRow(ctx context.Context, runID uuid.UUID) (bool, error) {
	rows, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{Category: CategoryRunAutoDriven, Limit: 500})
	if err != nil {
		return false, err
	}
	for _, e := range rows {
		fields, ok := e.Payload.(map[string]any)
		if !ok {
			continue
		}
		if fields["act"] == "gate" && fields["action"] == driveActionMerge {
			return true, nil
		}
	}
	return false, nil
}

// CategoryRunAutoDriven mirrors the backend audit category (#1700). Repeated
// (not imported) per the thin local-copy rule — import direction is cli →
// backend, not the reverse.
const CategoryRunAutoDriven = "run_auto_driven"

// driveDecisionActions builds the drive verb's own next-actions pointer for a
// decision / paged / stalled stop. It is a focused pointer at the decision
// tool; fishhawk_get_run_status carries the full lifecycle next_actions block.
func driveDecisionActions(state string, runID uuid.UUID, workingDir string) *NextActions {
	params := map[string]string{"run_id": runID.String()}
	if workingDir != "" {
		params["working_dir"] = workingDir
	}
	switch {
	case state == "scope_amendment_requested":
		return &NextActions{State: state, Actions: []SuggestedAction{{
			Action:       "fishhawk_decide_scope_amendment",
			Params:       params,
			Precondition: "a scope amendment is pending; no delegation knob covers amendments",
			Consumes:     consumesNone,
			Reason:       "decide the amendment before its window elapses, then re-invoke fishhawk_drive_run",
		}}}
	case state == "paged":
		return &NextActions{State: state, Actions: []SuggestedAction{{
			Action:       "fishhawk_get_run_status",
			Params:       params,
			Precondition: "a must_page_human condition halted the driver",
			Consumes:     consumesNone,
			Reason:       "the gate was handed to you; arbitrate, then re-invoke fishhawk_drive_run",
		}}}
	case state == "dispatched_stale_live":
		// The driver's own probe found a LIVE process carrying the stage id, so
		// the manual verify-and-re-dispatch instruction is deliberately WITHHELD
		// here (it survives only on the UNKNOWN/unprobeable branch below): a
		// re-dispatch while that process lives is the second-runner failure this
		// driver exists to prevent. Point the operator at inspection only.
		return &NextActions{State: state, Actions: []SuggestedAction{{
			Action:       "fishhawk_get_run_status",
			Params:       params,
			Precondition: "the stage sat in 'dispatched' beyond the runner-liveness threshold and the driver's own liveness probe found a LIVE host process matching the stage id that never flipped 'running' — anomalous",
			Consumes:     consumesNone,
			Reason:       "inspect the dispatch's log_path; do NOT re-dispatch while that process lives, because a second runner into the same lineage lock is the failure this driver exists to prevent — once the process exits or you terminate it, re-invoke fishhawk_drive_run",
		}}}
	case state == "dispatched_stale":
		return &NextActions{State: state, Actions: []SuggestedAction{{
			Action:       "fishhawk_dispatch_stage",
			Params:       params,
			Precondition: "the stage sat in 'dispatched' beyond the runner-liveness threshold and the driver's own liveness probe could NOT confirm the runner dead (pgrep unavailable / errored / timed out)",
			Consumes:     consumesNone,
			Reason:       "verify by hand that no runner process is live (pgrep -f fishhawk-runner + the dispatch log_path); only if none is, re-dispatch the stage and re-invoke fishhawk_drive_run",
		}}}
	case state == stoppedAcceptanceNeedsTarget:
		return &NextActions{State: state, Actions: []SuggestedAction{{
			Action:       "fishhawk_dispatch_stage",
			Params:       params,
			Precondition: "the acceptance target is unreachable or stale for the merge candidate; no runner was spawned and the stage stays awaiting_host_dispatch",
			Consumes:     consumesNone,
			Reason:       "bring up the acceptance target at the expected head SHA (e.g. scripts/dev preview), then re-dispatch the acceptance stage and re-invoke fishhawk_drive_run",
		}}}
	case strings.HasPrefix(state, "plan_"):
		return &NextActions{State: state, Actions: []SuggestedAction{{
			Action:       "fishhawk_approve_plan",
			Params:       params,
			Precondition: "the plan gate is parked and no delegated may_approve applied",
			Consumes:     consumesApprovalSlot,
			Reason:       "read the plan + reviews and approve/revise/reject, then re-invoke fishhawk_drive_run",
		}}}
	default:
		return &NextActions{State: state, Actions: []SuggestedAction{{
			Action:       "fishhawk_get_run_status",
			Params:       params,
			Precondition: "the driver could not auto-act at this state",
			Consumes:     consumesNone,
			Reason:       "inspect the run's full next_actions block and decide",
		}}}
	}
}
