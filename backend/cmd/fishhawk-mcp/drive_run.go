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
	// "paged:<event>" and "decision_required:<state>" are composed inline.
)

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
	StoppedReason string       `json:"stopped_reason" jsonschema:"why the drive stopped: merged | paged:<event> | decision_required:<state> | timeout | stalled | stage_failed | unrecorded_act | run_failed | cancelled | gate_error"`
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

It is a bounded, resumable loop: for any local stage in a dispatchable state it
FIRST records the dispatch (POST /v0/runs/{id}/auto-drive/acts) and only on a
successful record host-spawns the runner (record-before-dispatch makes an
unaudited mechanical act impossible by construction); it polls stages/reviews
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

Requires the fishhawk-runner binary to resolve on the MCP server's host, like
fishhawk_run_stage / fishhawk_dispatch_stage (local-only by design, ADR-024).
Reach for fishhawk_dispatch_stage instead to drive a single stage by hand.
`),
	}, resolver.driveRun)
}

// driveRun is the tool handler: the bounded, resumable drive loop.
func (r *runResolver) driveRun(ctx context.Context, _ *mcp.CallToolRequest, in DriveRunInput) (*mcp.CallToolResult, DriveRunOutput, error) {
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
	deadline := time.Now().Add(wall)

	pollInterval := r.drivePollInterval
	if pollInterval <= 0 {
		pollInterval = defaultDrivePollInterval
	}

	out := DriveRunOutput{RunID: runUUID.String(), Warnings: warnings}

	spawned := map[string]bool{}        // stage IDs spawned this invocation (idempotency guard)
	dispatchedCount := map[string]int{} // stage ID -> dispatch count (fixup_redispatch discriminator)
	stall := 0
	var lastSig string

	for {
		if time.Now().After(deadline) {
			out.StoppedReason = stoppedTimeout
			return nil, out, nil
		}
		if ctx.Err() != nil {
			out.StoppedReason = stoppedTimeout
			out.Warnings = append(out.Warnings, "context cancelled: "+ctx.Err().Error())
			return nil, out, nil
		}

		runRow, gerr := r.api.GetRun(ctx, runUUID)
		if gerr != nil {
			return nil, out, fmt.Errorf("drive: get run: %w", gerr)
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

		stages, serr := r.api.ListRunStages(ctx, runUUID)
		if serr != nil {
			return nil, out, fmt.Errorf("drive: list stages: %w", serr)
		}

		// (b) A pending scope amendment is ALWAYS a decision — no delegation
		// knob covers amendments, and its window times out in minutes.
		if pending, aerr := r.driveScopeAmendmentPending(ctx, runUUID); aerr != nil {
			out.Warnings = append(out.Warnings, "scope-amendment poll failed: "+aerr.Error())
		} else if pending {
			out.StoppedReason = "decision_required:scope_amendment_requested"
			out.NextActions = driveDecisionActions("scope_amendment_requested", runUUID, in.WorkingDir)
			return nil, out, nil
		}

		// (c) Dispatch the first host-dispatchable stage — record BEFORE spawn.
		if disp := driveDispatchableStage(stages, spawned); disp != nil {
			stall = 0
			recordName := driveDispatchName(*disp, dispatchedCount)
			note := ""
			if had, herr := r.driveHasPriorDispatchRow(ctx, runUUID, recordName); herr == nil && had {
				// A run_auto_driven dispatch row already exists for this stage
				// name and the stage is still dispatchable — a crash-resume or a
				// re-open. Re-record honestly with a retry note.
				note = "retry"
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

// driveDispatchableStage returns the lowest-sequence host-dispatchable stage
// (a plan/implement/acceptance stage in pending or dispatched, not already
// spawned this invocation), or nil. Review stages are server-driven, never
// host-spawned, so they are never dispatchable (driveAnyInFlight polls them).
func driveDispatchableStage(stages []Stage, spawned map[string]bool) *Stage {
	var best *Stage
	for i := range stages {
		st := &stages[i]
		if spawned[st.ID] {
			continue
		}
		switch st.Type {
		case "plan", "implement", "acceptance":
		default:
			continue
		}
		if st.State != "pending" && st.State != "dispatched" {
			continue
		}
		if best == nil || st.Sequence < best.Sequence {
			best = st
		}
	}
	return best
}

// driveAnyInFlight reports whether any stage is still executing: a running
// stage, a server-driven review stage that is pending/dispatched/running, or
// a stage this invocation already spawned that has not yet advanced.
func driveAnyInFlight(stages []Stage, spawned map[string]bool) bool {
	for i := range stages {
		st := &stages[i]
		if st.State == "running" {
			return true
		}
		if st.Type == "review" && (st.State == "pending" || st.State == "dispatched") {
			return true
		}
		if spawned[st.ID] && (st.State == "pending" || st.State == "dispatched") {
			return true
		}
	}
	return false
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

// driveHasPriorDispatchRow reports whether a run_auto_driven audit row for the
// given dispatch stage name already exists — a crash-resume or re-open signal
// so the re-record carries an honest retry note.
func (r *runResolver) driveHasPriorDispatchRow(ctx context.Context, runID uuid.UUID, stageName string) (bool, error) {
	rows, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{Category: CategoryRunAutoDriven, Limit: 500})
	if err != nil {
		return false, err
	}
	for _, e := range rows {
		fields, ok := e.Payload.(map[string]any)
		if !ok {
			continue
		}
		if fields["act"] == "dispatch" && fields["stage"] == stageName {
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
