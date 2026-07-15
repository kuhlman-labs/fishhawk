package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// driveSpawnFunc is the injectable spawn seam for fishhawk_drive_run. It
// matches spawnRunnerStageDetached exactly; production uses that function,
// tests inject a recording spawner.
type driveSpawnFunc func(binary string, argv, env []string, runID, stageID string, report detachedFailureReporter) (string, error)

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
	// spawn has sat in 'dispatched' past the runner-liveness threshold with no
	// runner observed: a genuinely runner-less stage (the cross-invocation
	// in-flight guard would otherwise poll every resume to timeout). The driver
	// still NEVER auto-spawns it — double-spawn stays impossible by construction;
	// it hands the manual re-dispatch to the operator via next_actions.
	stoppedDispatchedStale = "dispatched_stale"
	// stoppedContextCancelled is the stop when the drive context is cancelled
	// (distinct from the run-state-derived 'cancelled', which reports a run the
	// backend moved to the cancelled terminal state).
	stoppedContextCancelled = "context_cancelled"
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
	StoppedReason string       `json:"stopped_reason" jsonschema:"why the drive stopped: merged | paged:<event> | decision_required:<state> | timeout | stalled | stage_failed | unrecorded_act | run_failed | cancelled | gate_error | amendment_check_failed | dispatch_check_failed | dispatched_stale | context_cancelled"`
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
approve fires only on settled reviews. A stage left 'dispatched' with no
runner ever observed is handed back for a manual re-dispatch
(dispatched_stale); a fresh manual fishhawk_dispatch_stage records spawn
evidence so a re-invoked drive reads it as live and polls to convergence
rather than re-reporting stale.

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
			priorRow, newestDispatchSeq, newestDispatchTs, herr := r.driveHasPriorDispatchRow(ctx, runUUID, recordName)
			if herr != nil {
				out.StoppedReason = stoppedDispatchCheckFailed
				out.Warnings = append(out.Warnings,
					fmt.Sprintf("prior-dispatch-row poll for %s failed; stopping fail-closed: %v", recordName, herr))
				out.NextActions = driveDecisionActions("stalled", runUUID, in.WorkingDir)
				return nil, out, nil
			}
			// Concurrency / cross-invocation resume guard: a stage already in the
			// 'dispatched' window that THIS invocation did not spawn
			// (dispatchedCount==0) has a runner in flight — from a PRIOR driver
			// invocation OR a manual fishhawk_dispatch_stage (which lands NO
			// run_auto_driven dispatch row). A fresh invocation's per-run
			// `spawned` map cannot see it, and driveDispatchableStage treats
			// 'dispatched' as dispatchable. Treat it as in-flight and POLL; never
			// host-spawn a SECOND runner for the same stage — regardless of
			// whether a driver dispatch row exists (keying on priorRow would miss
			// the manual-dispatch case and double-spawn). A 'pending' resume
			// (crashed before the stage started) still re-records + re-dispatches
			// with a retry note below; a fixup/retry re-open (dispatchedCount>0)
			// still re-dispatches as fixup_redispatch.
			if disp.State == "dispatched" && dispatchedCount[disp.ID] == 0 {
				staleAfter := r.driveDispatchedStaleAfter
				if staleAfter <= 0 {
					staleAfter = defaultDriveDispatchedStaleAfter
				}
				// Spawn evidence = a prior run_auto_driven dispatch row (driver OR
				// manual fishhawk_dispatch_stage — one canonical vocabulary) OR a
				// non-nil StartedAt. A bare UpdatedAt is NOT evidence: on local, plan
				// approval pre-flips implement to 'dispatched' (with a fresh
				// UpdatedAt) BEFORE any host dispatch, so UpdatedAt alone cannot prove
				// a runner was ever spawned.
				hasEvidence := priorRow || disp.StartedAt != nil
				if !hasEvidence {
					// (a) NO evidence of any spawn attempt: the local parked-for-host-
					// dispatch handoff. Stop IMMEDIATELY rather than polling the full
					// liveness threshold first, and hand the dispatch to the operator.
					// The message names the handoff and instructs FIRST confirming no
					// runner is live — a fishhawk_run_stage / fishhawk_dispatch_stage
					// invoked moments ago may not have registered yet, so that transient
					// must not induce a double dispatch. The driver still NEVER
					// auto-spawns, so double-spawn stays impossible by construction.
					out.StoppedReason = stoppedDispatchedStale
					out.Warnings = append(out.Warnings, fmt.Sprintf(
						"stage %s (%s) is 'dispatched' with no spawn evidence (no dispatch row, no started_at). On local, plan approval parks the implement stage in 'dispatched' awaiting a host dispatch, so no runner has been spawned yet. FIRST confirm no runner process is live: a fishhawk_run_stage or fishhawk_dispatch_stage invoked moments ago may not have registered yet — wait for it to register (dispatched->running) rather than hand-dispatching during that transient. Only after confirming no live runner, re-dispatch by hand with fishhawk_dispatch_stage, then re-invoke fishhawk_drive_run.",
						disp.Type, disp.ID))
					out.NextActions = driveDecisionActions("dispatched_stale", runUUID, in.WorkingDir)
					return nil, out, nil
				}
				// Evidence exists: anchor staleness on the NEWEST spawn evidence —
				// max{UpdatedAt, StartedAt, newest dispatch-row Timestamp}. A fresh
				// manual re-dispatch records an act row with a server-set wall-clock
				// timestamp, so a just-recovered stage reads as fresh and the recovery
				// loop converges instead of insta-tripping stale on a stale UpdatedAt.
				anchor := disp.UpdatedAt
				if disp.StartedAt != nil && disp.StartedAt.After(anchor) {
					anchor = *disp.StartedAt
				}
				if priorRow && newestDispatchTs.After(anchor) {
					anchor = newestDispatchTs
				}
				// (b) Genuinely runner-less: the newest spawn evidence is older than
				// the liveness threshold and no signed prompt fetch flipped the stage
				// to running. Stop distinct and hand the manual re-dispatch to the
				// operator rather than polling every resume to timeout. The backend
				// flips dispatched->running on the runner's prompt fetch within
				// seconds of spawn (#1924), so a stage still 'dispatched' past the
				// threshold is a runner that never reached that fetch — not a
				// misclassified live one. A zero-value anchor (no timestamped
				// evidence at all) degrades to polling — fail toward polling, never
				// toward a stale stop or a spawn.
				if !anchor.IsZero() && time.Since(anchor) > staleAfter {
					age := time.Since(anchor).Round(time.Second)
					out.StoppedReason = stoppedDispatchedStale
					out.Warnings = append(out.Warnings, fmt.Sprintf(
						"stage %s (%s) has sat in 'dispatched' for %s (newest spawn evidence), past the %s runner-liveness threshold, and no prompt fetch flipped it to 'running'. Before re-dispatching, FIRST verify no runner process is live on this host — check `pgrep -f fishhawk-runner` and the dispatch's log_path — because a second runner into the same lineage lock is the failure this driver exists to prevent. Only if none is live, re-dispatch by hand with fishhawk_dispatch_stage, then re-invoke fishhawk_drive_run.",
						disp.Type, disp.ID, age, staleAfter))
					out.NextActions = driveDecisionActions("dispatched_stale", runUUID, in.WorkingDir)
					return nil, out, nil
				}
				// (c) Anchor fresh (or the zero-value degrade): poll and re-evaluate
				// the threshold next iteration. Deliberately NOT a one-shot spawned[]
				// mark — leaving the stage dispatchable lets this guard re-trip once
				// the threshold passes mid-invocation.
				driveSleep(ctx, pollInterval)
				continue
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
			// Acceptance-dispatch admission (#1928): before recording + spawning
			// an acceptance dispatch, ask the backend to short-circuit an
			// all-skip-with-basis / empty-criteria / out-of-scope plan. On a hit
			// the stage settles server-side (a passed verdict, no preview) — record
			// NO act, spawn nothing, and let the next poll observe the terminal
			// stage. Fail OPEN: an admission-call error appends a warning and falls
			// through to today's record+spawn path (which opens no NEW code-execution
			// surface); a short_circuited:false result is the normal no-op with no
			// warning.
			if disp.Type == "acceptance" {
				accStageUUID, perr := uuid.Parse(disp.ID)
				if perr != nil {
					return nil, out, fmt.Errorf("drive: resolved stage_id %q not a UUID: %w", disp.ID, perr)
				}
				admission, warn := r.maybeShortCircuitAcceptance(ctx, accStageUUID)
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
			out.StepsTaken = append(out.StepsTaken, DriveStep{
				Kind: "dispatch", Stage: recordName, Delegated: false,
				Note: "mechanical stage dispatch",
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

// driveSleep waits for d or until ctx is cancelled.
func driveSleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// driveDispatchableStage returns the EARLIEST non-terminal stage when — and
// only when — it is itself host-dispatchable: a plan/implement/acceptance stage
// in pending or dispatched that this invocation has not already spawned, with
// its gate preconditions satisfied. It returns nil when nothing is host-
// dispatchable right now: the earliest non-terminal stage is a review stage
// (server-driven, polled by driveAnyInFlight) or a plan/implement/acceptance
// stage parked in awaiting_approval/running/blocked (the gate branch / poll own
// those).
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
	if earliest.State != "pending" && earliest.State != "dispatched" {
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
// signal so the re-record carries an honest retry note — and returns the
// highest audit sequence among the matching rows PLUS that newest row's
// server-set Timestamp (the staleness anchor's spawn-evidence input, #1905).
// For an implement stage the match set includes BOTH 'implement' and
// 'fixup_redispatch' dispatch rows (a fix-up re-dispatch is still an implement
// dispatch), so the returned sequence is the newest implement-family dispatch —
// the caller compares it against the newest stage_fixup_triggered row to
// attribute a cross-invocation fix-up re-open.
//
// The match predicate is deliberately act=='dispatch' + stage family, SOURCE-
// and action-value-agnostic: the endpoint rejects any action other than
// autoDriveDispatchActionName ('dispatch_stage') with a 400 that appends
// nothing (autodrive_http.go), so driver rows (source fishhawk_drive_run) and
// manual fishhawk_dispatch_stage rows are ONE dispatch-evidence vocabulary by
// server-side construction — matching on act+stage is provably complete over
// it.
func (r *runResolver) driveHasPriorDispatchRow(ctx context.Context, runID uuid.UUID, stageName string) (bool, int64, time.Time, error) {
	rows, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{Category: CategoryRunAutoDriven, Limit: 500})
	if err != nil {
		return false, 0, time.Time{}, err
	}
	found := false
	var newestSeq int64
	var newestTs time.Time
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
				newestTs = e.Timestamp
			}
		}
	}
	return found, newestSeq, newestTs, nil
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
	case state == "dispatched_stale":
		return &NextActions{State: state, Actions: []SuggestedAction{{
			Action:       "fishhawk_dispatch_stage",
			Params:       params,
			Precondition: "the stage has sat in 'dispatched' beyond the runner-liveness threshold with no runner observed",
			Consumes:     consumesNone,
			Reason:       "confirm no runner process is live, re-dispatch the stage by hand, then re-invoke fishhawk_drive_run",
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
