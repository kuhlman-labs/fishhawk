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
	Stage         string `json:"stage" jsonschema:"stage type: plan | implement | review | acceptance. dispatch is the DEFAULT verb for a local acceptance stage (E31.9) — it validates against a running preview/target instance and runs long, so non-blocking dispatch keeps the session free; no new argv (composeRunnerArgv passes --stage through, and acceptance takes neither --plan-out nor --check-base-ref)"`
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

	// NeedsTarget is the pre-spawn acceptance refusal (E48.6 / #1953): set only
	// when the acceptance-admission endpoint reported the plan needs live
	// validation against a declared target and the verb-side probe found that
	// target unreachable or stale. No spawn evidence was recorded and no runner
	// was spawned — the stage stays awaiting_host_dispatch for a clean
	// re-dispatch once the operator brings up the target at the named head SHA.
	NeedsTarget *AcceptanceNeedsTarget `json:"needs_target,omitempty" jsonschema:"present when the acceptance target is unreachable or stale, so no runner was dispatched. Names the target_host and expected_head_sha to provision (remediation), then re-dispatch. The stage remains awaiting_host_dispatch"`
}

// dispatchStageSourceTag is the run_auto_driven source that distinguishes a
// manual fishhawk_dispatch_stage spawn-evidence row from the driver's own
// (driveSourceTag). Caller identity lives ONLY in source — the action value is
// the shared canonical autoDriveDispatchActionName for both callers (#1905).
const dispatchStageSourceTag = "fishhawk_dispatch_stage"

// autoDriveRecordableStage reports whether a manual-dispatch stage type is in
// the /auto-drive/acts endpoint's closed dispatch-stage set (plan|implement|
// acceptance). A review dispatch is NOT recordable — the endpoint would 400 an
// unknown stage, and the driver never host-dispatches review stages anyway.
func autoDriveRecordableStage(stage string) bool {
	switch stage {
	case "plan", "implement", "acceptance":
		return true
	default:
		return false
	}
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
(#1232), the durable answer to the #1189 amendment-timeout that superseded
the interim "fishhawk run auto-decide" second channel (#1233/#1234), since
removed (#1554).

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

This is the DEFAULT verb for a local IMPLEMENT stage (#1247): the implement
stage is the one stage type that can file a mid-stage scope amendment, and a
blocking fishhawk_run_stage holds the session so the amendment cannot be
decided in-band. When you drive an implement stage, reach for dispatch first.
fishhawk_run_stage is the synchronous-with-progress opt-in — the right verb for
plan/review stages, or the compact one-shot for an implement stage when a
mid-stage amendment is impossible — and it blocks to terminal returning the
full events list, diff_summary, and next_actions in one call.

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

	// (1a) Pre-dispatch runner_kind mismatch guardrail (#1355). A host
	// dispatch always spawns a LOCAL runner, so reject one against a run
	// already LOCKED to runner_kind=github_actions BEFORE the runner spawns.
	// Engages only on the locked state (un-resolved runs auto-resolve to
	// local on first dispatch); fails OPEN on a GetRun error.
	guardWarnings, guardErr := r.guardHostDispatch(ctx, runUUID)
	if guardErr != nil {
		return nil, DispatchStageOutput{}, guardErr
	}

	// (2) Resolve the stage id from (run_id, stage type) — the same
	// belongs-to-run-validating resolver fishhawk_run_stage uses.
	resolvedStageID, err := r.resolveStageID(ctx, runUUID, in.Stage, in.StageID)
	if err != nil {
		return nil, DispatchStageOutput{}, err
	}

	// (2a) Sibling-in-flight admission guard (#1872). Refuse a host dispatch
	// while another stage of the run is dispatched/running (or the target
	// itself is running), so a second local runner cannot rotate the signing
	// key out from under an in-flight sibling. Allows the target's own
	// 'dispatched' park state (retry/fixup re-dispatch); fails OPEN on a
	// stage-list read error. Any fail-open warning merges into warnings below.
	siblingWarnings, siblingErr := r.guardSiblingStageInFlight(ctx, runUUID, resolvedStageID)
	if siblingErr != nil {
		return nil, DispatchStageOutput{}, siblingErr
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
	// Seeded with any guard fail-open warning from step (1a) and (2a).
	warnings := append(guardWarnings, siblingWarnings...)
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

	// (6) Spawn DETACHED — start and return; the runner outlives this call. The
	// reporter closure binds the backend client + durable handle so the detached
	// reaper can report a spawn-phase non-zero exit (a runner that died before
	// registering a terminal stage state) to the backend, transitioning the stage
	// to failed/category-C instead of leaving it stuck 'dispatched' (#1747).
	stageUUID, err := uuid.Parse(resolvedStageID)
	if err != nil {
		return nil, DispatchStageOutput{}, fmt.Errorf("resolved stage_id %q is not a valid UUID: %w", resolvedStageID, err)
	}

	// (5b) Acceptance-dispatch admission (#1928). For an acceptance stage, ask
	// the backend to evaluate the approved plan's short-circuit predicates
	// BEFORE recording spawn evidence or spawning a runner: an
	// all-skip-with-basis / empty-criteria / out-of-scope plan settles the stage
	// server-side to a passed verdict with no preview, so spawning a runner would
	// only fail category-C acceptance_target_unreachable — the #1928 parity gap.
	// Fail OPEN on a TRANSPORT error (network / 5xx): append a warning and proceed
	// to record+spawn as today; a short_circuited:false result is the normal no-op
	// and adds NO warning (the reconciliation binding condition). A 4xx admission
	// REJECTION (401/403 cross_run_admission / 404 / 422) is NOT a fail-open
	// condition — it halts with a tool error so a runner never spawns after the
	// backend rejected the request on authorization grounds (#1928).
	if in.Stage == "acceptance" {
		admission, warn, admitErr := r.maybeShortCircuitAcceptance(ctx, runUUID, stageUUID)
		if admitErr != nil {
			return nil, DispatchStageOutput{}, admitErr
		}
		if warn != "" {
			warnings = append(warnings, warn)
		}
		if admission != nil && admission.ShortCircuited {
			// The stage settled server-side; record NO spawn evidence and spawn
			// NO runner. Compose the output from the freshly-settled (terminal)
			// stage — a fetch failure omits the wait status with a warning.
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
					fmt.Sprintf("post-short-circuit stage fetch failed (stage_wait_status omitted): %v", fetchErr))
			}
			warnings = append(warnings, fmt.Sprintf(
				"acceptance stage short-circuited to a passed verdict server-side (%s) with no runner spawn and no preview; no spawn evidence recorded.",
				shortCircuitLabel(admission)))
			return nil, DispatchStageOutput{
				RunID:           runUUID.String(),
				StageID:         resolvedStageID,
				StageWaitStatus: stageWaitStatus,
				RunURL:          r.api.baseURL + "/runs/" + runUUID.String(),
				Warnings:        warnings,
			}, nil
		}

		// (5b') Acceptance target-identity gate (#1953): a needs_target admission
		// means the runner would validate against a live target — probe it FROM
		// THIS HOST BEFORE recording any spawn evidence. Unreachable/stale refuses
		// here, ahead of the (5a) record-act AND the (5c) host-dispatch marker, so
		// nothing is recorded and the stage stays awaiting_host_dispatch/pending
		// for a clean re-dispatch. Every proceed outcome (verified / unverifiable /
		// no hosts / preview-cmd-set / empty-SHA) returns nil and falls through.
		if refusal, gwarn := r.checkAcceptanceTarget(ctx, admission); refusal != nil {
			var stageWaitStatus *StageWaitStatus
			if fetchErr := func() error {
				fetchCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				stages, ferr := r.api.ListRunStages(fetchCtx, runUUID)
				if ferr != nil {
					return ferr
				}
				stageWaitStatus = stageWaitStatusFor(stages, in.Stage, "")
				return nil
			}(); fetchErr != nil {
				warnings = append(warnings,
					fmt.Sprintf("post-needs-target stage fetch failed (stage_wait_status omitted): %v", fetchErr))
			}
			warnings = append(warnings, fmt.Sprintf(
				"acceptance target %q not ready for the merge candidate (%s); NOT recording spawn evidence or spawning a runner that would fail category-C acceptance_target_unreachable. %s",
				refusal.TargetHost, refusal.Detail, refusal.Remediation))
			return nil, DispatchStageOutput{
				RunID:           runUUID.String(),
				StageID:         resolvedStageID,
				StageWaitStatus: stageWaitStatus,
				RunURL:          r.api.baseURL + "/runs/" + runUUID.String(),
				Warnings:        warnings,
				NeedsTarget:     refusal,
			}, nil
		} else if gwarn != "" {
			warnings = append(warnings, gwarn)
		}
	}

	report := func(ctx context.Context, category, reason, detail string, exitCode int) error {
		_, rerr := r.api.ReportStageFailure(ctx, runUUID, stageUUID, category, reason, detail, exitCode)
		return rerr
	}

	// (5a) Record this manual host-spawn as run_auto_driven ATTRIBUTION under the
	// SAME canonical action value the driver uses (autoDriveDispatchActionName,
	// 'dispatch_stage'), distinguished only by Source (#1905). Post-#1912 this row
	// is ATTRIBUTION ONLY — no longer the staleness evidence: the host-dispatch
	// marker below stamps the 'dispatched' spawn signal (with a fresh updated_at),
	// which is what a later fishhawk_drive_run resume anchors staleness on.
	// BEST-EFFORT: on error (including insufficient_scope on a token lacking
	// write:approvals) append a warning naming the degraded attribution and
	// proceed — the record is provenance, not an authorization gate, so making it
	// mandatory would regress the core manual recovery verb. Only stage types the
	// endpoint's closed set accepts (plan|implement|acceptance) are recorded; a
	// review dispatch records nothing (the endpoint would 400 an unknown stage,
	// and the driver never host-dispatches review stages anyway).
	if autoDriveRecordableStage(in.Stage) {
		if _, rerr := r.api.RecordAutoDriveAct(ctx, runUUID, RecordAutoDriveAct{
			Action: autoDriveDispatchActionName,
			Stage:  in.Stage,
			Source: dispatchStageSourceTag,
			Note:   "manual host dispatch",
		}); rerr != nil {
			warnings = append(warnings, fmt.Sprintf(
				"could not record manual-dispatch attribution (%v); the run_auto_driven provenance row is missing. Proceeding — this row is attribution, not the staleness evidence (the host-dispatch marker stamps that) and not an authorization gate.",
				rerr))
		}
	}

	// (5c) Mark the host spawn BEFORE spawning (#1912): the endpoint CAS-flips
	// {pending, awaiting_host_dispatch} → dispatched so post-#1912 'dispatched'
	// unambiguously means a spawn attempt exists. FAIL CLOSED — a transport error
	// or 4xx means NO spawn (an unmarked spawn would recreate the ambiguity #1912
	// removes). transitioned:false (already 'dispatched') proceeds: the manual
	// dead-runner re-dispatch.
	if _, hderr := r.api.HostDispatchStage(ctx, runUUID, stageUUID); hderr != nil {
		return nil, DispatchStageOutput{}, fmt.Errorf(
			"host-dispatch marker for stage %s failed; NOT spawning (fail-closed): %w", resolvedStageID, hderr)
	}

	logPath, err := spawnRunnerStageDetached(binary, argv, env, runUUID.String(), resolvedStageID, report)
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
