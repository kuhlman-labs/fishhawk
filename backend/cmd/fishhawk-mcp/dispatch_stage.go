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

// DispatchStageInput is the fishhawk_dispatch_stage tool's input schema
// (#1232). It is the run_stage-shaped subset needed to SPAWN a stage: it
// carries EVERY argv-affecting field fishhawk_run_stage composes into the
// runner argv — run_id, stage_id, workflow, stage, working_dir, github_repo,
// base_branch, push_and_open_pr, runner_binary — so composeRunnerArgv produces
// byte-identical argv for both verbs (including the plan-only --plan-out and
// the implement-only --check-base-ref, which are derived from `stage` +
// `base_branch`). Only `verbose` is omitted: it shapes the post-run event list
// a detached call never returns.
type DispatchStageInput struct {
	RunID         string `json:"run_id" jsonschema:"Fishhawk run UUID minted by fishhawk_start_run"`
	StageID       string `json:"stage_id,omitempty" jsonschema:"stage UUID inside the run; optional — when omitted it is auto-resolved from (run_id, stage type), same as fishhawk_run_stage"`
	Workflow      string `json:"workflow" jsonschema:"workflow ID matching the run's workflow"`
	Stage         string `json:"stage" jsonschema:"stage type: plan | implement | review"`
	WorkingDir    string `json:"working_dir,omitempty" jsonschema:"checkout the agent runs in; defaults to the MCP server's cwd"`
	GitHubRepo    string `json:"github_repo,omitempty" jsonschema:"GitHub repo as owner/name; auto-detected from working_dir's origin remote when empty"`
	BaseBranch    string `json:"base_branch,omitempty" jsonschema:"base branch for the implement-stage PR (no effect when push_and_open_pr is false); defaults to main"`
	PushAndOpenPR *bool  `json:"push_and_open_pr,omitempty" jsonschema:"when true, the implement stage pushes and opens a PR. Defaults to TRUE for the MCP-driven local loop (ADR-031 Phase 1), same as fishhawk_run_stage. A bare omitted value resolves to true"`
	RunnerBinary  string `json:"runner_binary,omitempty" jsonschema:"path to fishhawk-runner; resolved in order: input, FISHHAWK_RUNNER_BIN env, fishhawk-runner sibling to this binary, then PATH"`
}

// DispatchStageOutput is the non-blocking dispatch handle (#1232). Unlike
// RunStageOutput it carries NO events/diff/next_actions — the call returns
// before the stage runs, so the operator polls fishhawk_get_run_status on the
// (run_id, stage_id) handle to terminal. StageWaitStatus is the freshly
// classified (expected non-terminal) wait status with poll_interval_seconds;
// LogPath points at the detached runner's redirected stdout/stderr (a local
// diagnostic only — the durable record is the backend + signed trace).
type DispatchStageOutput struct {
	RunID           string           `json:"run_id" jsonschema:"the run UUID the stage was dispatched on (the durable ADR-037 handle, with stage_id)"`
	StageID         string           `json:"stage_id" jsonschema:"the resolved stage UUID the runner was spawned against (the durable ADR-037 handle, with run_id)"`
	StageWaitStatus *StageWaitStatus `json:"stage_wait_status,omitempty" jsonschema:"the freshly-dispatched stage's execution wait status (normally pending/running with poll_interval_seconds=30); poll fishhawk_get_run_status on that cadence to terminal. Omitted (with a warning) when the post-dispatch stage fetch failed"`
	RunURL          string           `json:"run_url,omitempty" jsonschema:"direct link to the run-detail view"`
	LogPath         string           `json:"log_path,omitempty" jsonschema:"path to the detached runner's redirected stdout/stderr log on the MCP host (a diagnostic only; the durable record is the backend state + the signed trace bundle)"`
	Warnings        []string         `json:"warnings,omitempty"`
}

// registerDispatchStage wires the fishhawk_dispatch_stage tool (#1232).
func registerDispatchStage(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_dispatch_stage",
		Description: strings.TrimSpace(`
Dispatch one stage of a run NON-BLOCKING. Use this instead of
fishhawk_run_stage when you need a SINGLE MCP session to both await a stage
and decide a mid-stage scope amendment in-band — it is the SDK-independent
non-blocking half of the ADR-037 (#879/#880) poll-to-terminal contract
(#1232), the durable answer to the #1189 amendment-timeout that supersedes
the interim "fishhawk run auto-decide" second channel (#1233/#1234).

It spawns the fishhawk-runner binary on the operator's host DETACHED (its own
process group; output redirected to a per-run log file; a reaper goroutine
collects the exit) and returns the durable (run_id, stage_id) handle plus a
non-terminal stage_wait_status IMMEDIATELY instead of blocking to terminal.

Workflow after dispatch:

  1. fishhawk_dispatch_stage --stage implement ...   (returns the handle now)
  2. poll fishhawk_get_run_status on the advertised poll_interval_seconds (30s)
     until the stage's implement_stage_wait_status goes terminal.
  3. between polls, when a scope_amendment_pending surfaces, call
     fishhawk_decide_scope_amendment — so the runner's amendment poll resolves
     before its window elapses, with no failed-stage retry.

Contrast with fishhawk_run_stage, the synchronous-with-progress DEFAULT/FALLBACK
that blocks to terminal and returns the full events list, diff_summary, and
next_actions. Reach for dispatch only when in-band amendment decisions matter;
otherwise prefer run_stage.

Requires the fishhawk-runner binary to resolve on the MCP server's host,
exactly like fishhawk_run_stage (this tool is local-only by design, ADR-024 Q5).
A future native MCP Tasks (invocationMode:async) mode is a later transport
refinement that would layer onto this same handle.
`),
	}, resolver.dispatchStage)
}

// dispatchStage is the tool handler. It reuses fishhawk_run_stage's input
// validation, stage-id resolution, runner-binary resolution, repo detection,
// and argv composition, but spawns the runner DETACHED and returns the durable
// handle immediately (#1232).
func (r *runResolver) dispatchStage(ctx context.Context, _ *mcp.CallToolRequest, in DispatchStageInput) (*mcp.CallToolResult, DispatchStageOutput, error) {
	// (1) Input validation, mirroring runStage.
	if in.RunID == "" || in.Workflow == "" || in.Stage == "" {
		return nil, DispatchStageOutput{}, errors.New("run_id, workflow, and stage are all required")
	}
	runUUID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, DispatchStageOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	pushAndOpenPR := in.PushAndOpenPR == nil || *in.PushAndOpenPR
	if in.StageID != "" {
		if _, perr := uuid.Parse(in.StageID); perr != nil {
			return nil, DispatchStageOutput{}, fmt.Errorf("stage_id %q is not a valid UUID: %w", in.StageID, perr)
		}
	}

	// (2) Resolve the stage id from (run_id, stage type) — the same
	// belongs-to-run-validating resolver fishhawk_run_stage uses.
	resolvedStageID, err := r.resolveStageID(ctx, runUUID, in.Stage, in.StageID)
	if err != nil {
		return nil, DispatchStageOutput{}, err
	}

	// (3) Resolve the runner binary (input > env > sibling > PATH > error).
	binary, err := resolveRunnerBinary(in.RunnerBinary, r.getenv)
	if err != nil {
		return nil, DispatchStageOutput{}, err
	}

	workingDir := in.WorkingDir
	if workingDir == "" {
		workingDir = "."
	}

	// (4) Resolve the GitHub repo with the same soft-fail rule run_stage uses:
	// push_and_open_pr=false makes a missing repo a warning, not an error.
	var warnings []string
	repo := in.GitHubRepo
	if repo == "" {
		detected, derr := runStageDetectGitHubRepo(workingDir)
		switch {
		case derr == nil:
			repo = detected
		case pushAndOpenPR:
			return nil, DispatchStageOutput{}, fmt.Errorf(
				"github_repo not set and could not detect from origin (push_and_open_pr requires a repo): %w", derr)
		default:
			warnings = append(warnings,
				fmt.Sprintf("github_repo not set and origin auto-detect failed (%v); proceeding without push.", derr))
		}
	}

	// (5) Compose the runner argv via the shared composer so the dispatched
	// argv is byte-identical to fishhawk_run_stage's for the same input.
	baseBranch := in.BaseBranch
	if baseBranch == "" {
		baseBranch = "main"
	}
	runStageIn := RunStageInput{
		RunID:         in.RunID,
		StageID:       in.StageID,
		Workflow:      in.Workflow,
		Stage:         in.Stage,
		WorkingDir:    in.WorkingDir,
		GitHubRepo:    in.GitHubRepo,
		BaseBranch:    in.BaseBranch,
		PushAndOpenPR: in.PushAndOpenPR,
		RunnerBinary:  in.RunnerBinary,
	}
	argv := r.composeRunnerArgv(runStageIn, resolvedStageID, repo, baseBranch, pushAndOpenPR)
	env := append(os.Environ(), "FISHHAWK_API_TOKEN="+r.api.token)

	// (6) Spawn DETACHED — start and return; the runner outlives this call.
	logPath, err := spawnRunnerStageDetached(binary, argv, env, runUUID.String(), resolvedStageID)
	if err != nil {
		return nil, DispatchStageOutput{}, err
	}

	// (7) One best-effort post-dispatch stage fetch to classify the
	// freshly-dispatched stage into a (normally non-terminal) StageWaitStatus.
	// A fetch failure is a warning, never a tool error — the handle is already
	// durable and pollable.
	var stageWaitStatus *StageWaitStatus
	if fetchErr := func() error {
		fetchCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stages, ferr := r.api.ListRunStages(fetchCtx, runUUID)
		if ferr != nil {
			return ferr
		}
		stageWaitStatus = stageWaitStatusFor(stages, in.Stage, "")
		if stageWaitStatus == nil {
			return fmt.Errorf("stage %s not found in run %s stage list", resolvedStageID, runUUID)
		}
		return nil
	}(); fetchErr != nil {
		warnings = append(warnings,
			fmt.Sprintf("post-dispatch stage fetch failed (stage_wait_status omitted): %v", fetchErr))
	}

	return nil, DispatchStageOutput{
		RunID:           runUUID.String(),
		StageID:         resolvedStageID,
		StageWaitStatus: stageWaitStatus,
		RunURL:          r.api.baseURL + "/runs/" + runUUID.String(),
		LogPath:         logPath,
		Warnings:        warnings,
	}, nil
}
