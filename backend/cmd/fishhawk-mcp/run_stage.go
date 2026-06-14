package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// runStageCommand is the subprocess fishhawk_run_stage spawns.
// Exposed as a var so tests can substitute a recording fake without
// actually running the runner binary. Production wires exec.Command.
var runStageCommand = exec.Command

// runStageLookPath looks up the fishhawk-runner binary on PATH.
// Test seam mirroring the CLI's runnerBinaryLookPath.
var runStageLookPath = exec.LookPath

// runStageExecutable returns the path to the running binary (os.Executable).
// Test seam: allows tests to inject a fake executable path for the
// sibling-binary resolution rung without needing a real binary on disk.
var runStageExecutable = os.Executable

// runStageGitRemoteOriginURL returns `origin`'s URL for the working
// dir. Mirrors the CLI's gitRemoteOriginURL — test seam.
var runStageGitRemoteOriginURL = func(dir string) (string, error) {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// runStageGracePeriod is the time SIGTERM is given to land before
// the handler escalates to SIGKILL. Exposed as a var so tests can
// shorten it.
var runStageGracePeriod = 30 * time.Second

// runStageGitNumstat runs `git show --numstat HEAD` in dir and returns
// the raw output. Exposed as a var so tests can inject fake output
// without needing a real git repo.
var runStageGitNumstat = func(dir string) (string, error) {
	cmd := exec.Command("git", "show", "--numstat", "HEAD")
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// DiffSummary reports the diff stats for the commit the runner made.
// Present only when the runner emitted a git_diff event and
// git show --numstat HEAD succeeded; nil otherwise (e.g. plan stages,
// or runners that exited without committing).
type DiffSummary struct {
	FilesChanged int `json:"files_changed"`
	Insertions   int `json:"insertions"`
	Deletions    int `json:"deletions"`
}

// AuditPointer identifies the most-recent audit entry for the run.
// Populated after the runner exits via GET /v0/audit?run_id=<id>&limit=1;
// nil on any fetch failure.
type AuditPointer struct {
	LatestSequence int64  `json:"latest_sequence"`
	EntryHash      string `json:"entry_hash"`
	URL            string `json:"url"`
}

// RunStageInput is the fishhawk_run_stage tool's input schema
// (ADR-024 / #433, impl #434). Mirrors `fishhawk runner start`'s
// flags so an agent driving the local-runner loop end-to-end via
// MCP composes the same arguments the CLI would.
type RunStageInput struct {
	RunID         string `json:"run_id" jsonschema:"Fishhawk run UUID minted by fishhawk_start_run"`
	StageID       string `json:"stage_id,omitempty" jsonschema:"stage UUID inside the run; optional — when omitted it is auto-resolved from (run_id, stage type). Pass explicitly only to disambiguate, or for back-compat; when supplied it must match the resolved stage of the requested type"`
	Workflow      string `json:"workflow" jsonschema:"workflow ID matching the run's workflow"`
	Stage         string `json:"stage" jsonschema:"stage type: plan | implement | review"`
	WorkingDir    string `json:"working_dir,omitempty" jsonschema:"checkout the agent runs in; defaults to the MCP server's cwd"`
	GitHubRepo    string `json:"github_repo,omitempty" jsonschema:"GitHub repo as owner/name; auto-detected from working_dir's origin remote when empty"`
	BaseBranch    string `json:"base_branch,omitempty" jsonschema:"base branch for the implement-stage PR (no effect when push_and_open_pr is false); defaults to main"`
	PushAndOpenPR *bool  `json:"push_and_open_pr,omitempty" jsonschema:"when true, the implement stage pushes and opens a PR. Defaults to TRUE for the MCP-driven local loop (ADR-031 Phase 1) so every run carries a pull_request_url for the review gate + merge reconciler. Pass false explicitly to keep the commit-yourself flow (the operator commits + pushes). A bare omitted value resolves to true."`
	RunnerBinary  string `json:"runner_binary,omitempty" jsonschema:"path to fishhawk-runner; resolved in order: FISHHAWK_RUNNER_BIN env, then fishhawk-runner sibling to this binary (os.Executable dir), then PATH"`
	Verbose       bool   `json:"verbose,omitempty" jsonschema:"when true, return the full runner event list including every stage_progress heartbeat; default false returns a compact result that omits routine heartbeats"`
}

// RunStageOutput is the structured result of one stage run. The
// runner's JSONL events accumulate on Events; the final stage state
// (fetched after the runner exits) lives on StageState; ExitCode is
// the runner's process exit code so callers can branch on failure
// categories (the runner uses distinct codes per category).
//
// Warnings collects best-effort surfaces: non-JSON runner stderr
// lines, failed post-run stage fetch, missing github_repo, etc.
// None of these fail the tool itself — the runner's exit is the
// canonical outcome.
//
// DiffSummary, AuditPointer, and RunURL are best-effort enrichment
// fields; they are omitted (nil / empty) when unavailable and never
// fail the tool.
//
// Outcome, Turns, TokensUsed, ElapsedSeconds, and LastEventKind are
// the compact-summary scalars distilled from the runner's event
// stream (#647): Outcome and TokensUsed come from the terminal
// `runner_completed` event, the remaining counters from the last
// stage_progress heartbeat. They let the driving agent read the
// stage's headline result without re-deriving it from Events. All are
// omitempty so they reflect as plain JSON scalars (avoiding the
// array-reflection trap, #371). By default the routine stage_progress
// heartbeats are dropped from Events (their signal survives in these
// scalars); pass verbose:true on the input to restore the full list.
type RunStageOutput struct {
	ExitCode     int           `json:"exit_code"`
	StageState   string        `json:"stage_state,omitempty" jsonschema:"final stage state fetched after the runner exits; empty when the fetch failed"`
	Events       []RunnerEvent `json:"events" jsonschema:"runner-emitted JSONL events in arrival order; compact by default (routine stage_progress heartbeats omitted), full list when verbose:true was set on the input"`
	Warnings     []string      `json:"warnings,omitempty"`
	DiffSummary  *DiffSummary  `json:"diff_summary,omitempty" jsonschema:"present when the runner emitted a git_diff event and git show --numstat HEAD succeeded; nil for plan stages"`
	AuditPointer *AuditPointer `json:"audit_pointer,omitempty" jsonschema:"most-recent audit entry for this run with its entry hash; nil on any fetch failure"`
	RunURL       string        `json:"run_url,omitempty" jsonschema:"direct link to the run-detail view"`

	// StageWaitStatus records the stage-execution wait contract on the durable
	// (run_id, stage_id) handle (#879/#880, ADR-037), derived from the same
	// post-run stage fetch that populates StageState — no extra round-trip. On
	// a normal synchronous return the stage is already terminal, so the field
	// records the terminal outcome (succeeded/failed/cancelled) and omits
	// poll_interval_seconds; the non-terminal + poll-interval path is meaningful
	// via fishhawk_get_run_status (and the future native-async mode). Omitted
	// when the post-run stage fetch failed.
	StageWaitStatus *StageWaitStatus `json:"stage_wait_status,omitempty" jsonschema:"stage-execution wait status on the durable (run_id, stage_id) handle: status is one of pending, running, succeeded, failed, cancelled. On a synchronous return the stage is normally already terminal (interval omitted); poll fishhawk_get_run_status on the advertised poll_interval_seconds to await a non-terminal stage. Omitted when the post-run stage fetch failed"`

	Outcome        string `json:"outcome,omitempty" jsonschema:"terminal runner outcome (ok | failed) from the runner_completed event; empty when the runner never reported one"`
	Turns          int    `json:"turns,omitempty" jsonschema:"agent turn count from the last stage_progress heartbeat"`
	TokensUsed     int    `json:"tokens_used,omitempty" jsonschema:"tokens consumed; from runner_completed when present, else the last heartbeat's running total"`
	ElapsedSeconds int    `json:"elapsed_seconds,omitempty" jsonschema:"wall-clock seconds from the last stage_progress heartbeat"`
	LastEventKind  string `json:"last_event_kind,omitempty" jsonschema:"the agent's last event kind from the last stage_progress heartbeat"`

	FixupNoChanges bool `json:"fixup_no_changes,omitempty" jsonschema:"true when this fix-up pass produced NO commit (the runner reported implement_fixup_no_changes): the PR branch tip is unchanged and the stage returned to its review gate. The pass is refunded against the normal fix-up budget (the absolute 3-pass ceiling still counts it), so a corrected fixup can be re-triggered without force_additional_pass"`

	// Budget is the workflow's current periodic-budget status (#693 /
	// ADR-030), fetched best-effort after the stage runs. Omitted when
	// the workflow declares no budget or the fetch failed (a fetch error
	// appends to Warnings) — DISPLAY-ONLY, never gates the stage.
	Budget *BudgetStatus `json:"budget,omitempty" jsonschema:"workflow periodic-budget status for the current calendar period (spend vs limit, tier ok|warn|over); omitted when no budget is configured. Display-only — never blocks the stage"`

	// ReviewActionHint is a display-only next-action pointer (#777),
	// populated only after an IMPLEMENT stage when its review landed with
	// unresolved approve_with_concerns concerns and the bounded fix-up
	// budget is not yet spent. It points at fishhawk_fixup_stage (route the
	// concerns back to the agent) vs approving to merge. Omitted on plan and
	// review stages (no implement review exists there) and when there is
	// nothing to act on — never gates the stage. Plan stages and start_run
	// are intentionally excluded.
	ReviewActionHint *ReviewActionHint `json:"review_action_hint,omitempty" jsonschema:"display-only next-action pointer after an implement stage whose review returned unresolved approve_with_concerns concerns and a non-spent fix-up budget; points at fishhawk_fixup_stage vs approving to merge. Omitted for non-implement stages and when there is nothing to act on. Never gates the stage"`

	// NextActions is the server-suggested next-action block (#1024),
	// computed after the post-stage fetches (run row + drive view, stage
	// list, review statuses, hint) so a terminal run_stage call hands the
	// operator the legal next move directly. Same classifier as
	// fishhawk_get_run_status. Best-effort: omitted when the post-run run
	// fetch failed. Display-only, never gates the stage.
	NextActions *NextActions `json:"next_actions,omitempty" jsonschema:"server-suggested next actions (#1024): the classified run lifecycle state plus the legal next moves after this stage ran — each entry names the tool to call (with key params), its precondition, what it consumes (none, fixup_budget, retry_budget, approval_slot, new_run), and a one-line reason. Omitted when the post-run fetches failed. Display-only — never gates the stage"`
}

// RunnerEvent wraps an unstructured runner event. Each event is the
// parsed JSON object the runner emitted on one stdout line. Typed
// as `any` (not json.RawMessage) for the same reason Artifact.Content
// and AuditEntry.Payload are: the SDK's schema reflection treats
// json.RawMessage as `type: array`, which rejects the runner's
// object-shaped events at wire time.
type RunnerEvent struct {
	Payload any `json:"payload"`
}

// registerRunStage wires the fishhawk_run_stage tool (ADR-024 /
// #433, impl #434).
//
// Spawns the operator's fishhawk-runner binary as a child process,
// composes argv from the input (mirroring `fishhawk runner start`),
// parses each JSONL line on stdout as one runner event, and either
// (a) emits a `notifications/progress` update per event when the
// client provided a progress token, or (b) accumulates events in
// memory for the final tool result. The audit log carries the
// durable record either way.
//
// Cancellation: on tool-call context cancellation, the handler
// sends SIGTERM to the runner subprocess, waits up to
// runStageGracePeriod, then escalates to SIGKILL. The runner
// having a SIGTERM handler is a prerequisite for graceful cleanup
// (#435); without it, cancellation is kill-only with the SLA
// ticker reaping the stuck stage.
//
// Resolution failure modes:
//   - fishhawk-runner not on PATH (and no override): tool error with
//     a clean remediation.
//   - GitHub repo auto-detect failed without push_and_open_pr: tool
//     error.
//   - Subprocess spawn failure (rare; permission, missing binary
//     after lookup): tool error.
//
// Auth: write tool. The MCP server's FISHHAWK_API_TOKEN is forwarded
// to the runner subprocess via env (same shape as the CLI).
func registerRunStage(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_run_stage",
		Description: strings.TrimSpace(`
Drive one stage of a run to completion. Use this after fishhawk_start_run
to execute each stage in turn — it spawns the fishhawk-runner binary on
the operator's host.

Wait contract (ADR-037 #879/#880): (run_id, stage_id) is the DURABLE handle
for a stage's execution. The blessed, AUTHORITATIVE way to await stage
completion is to poll fishhawk_get_run_status and re-call it on the
poll_interval_seconds (30s) it advertises on
plan_stage_wait_status / implement_stage_wait_status while the status is
non-terminal. This synchronous-with-progress call is the NEGOTIATED FALLBACK
for clients that prefer to block (or for short stages): it runs the stage to
completion and returns the terminal outcome, also surfacing stage_wait_status
on the handle. A future native MCP Tasks (invocationMode:async) mode — gated
on ADR-033 transport + MCP Tasks leaving experimental — will let this call
return a handle immediately and poll to terminal; that mode is NOT available
today.

Mirrors the CLI's "fishhawk runner start" verb. Pair with
fishhawk_start_run + fishhawk_approve_plan for the full
agent-driven local-runner loop:

  1. fishhawk_start_run --working-dir ... --issue ... --runner-kind local
  2. fishhawk_run_stage --stage plan ...
  3. fishhawk_approve_plan ...
  4. fishhawk_run_stage --stage implement ...

Runner output streams as MCP progress notifications ONLY when the
client supplies a progressToken on the call (opt-in per the MCP
spec).

The final tool result is COMPACT by default: the routine
stage_progress heartbeats are dropped from the events list (their
signal is preserved in the scalar summary fields below), while
stage_state, the outcome/timing/token summary, the terminal
runner_completed event, git_diff, runner_cancelled, and every other
non-heartbeat event are retained in arrival order. Set verbose:true
to restore the full event list including every heartbeat. This
roughly halves the driver's per-stage context cost without losing
any durable signal — the audit log and signed trace bundle are
unchanged.

During execution the runner emits periodic stage_progress
heartbeats (~every 15s) carrying the turn count, elapsed time,
tokens-so-far, and last event kind, so the driver can distinguish a
progressing stage from a stalled one (the counters keep ticking on
elapsed even when turns/tokens stall). With a progressToken these
arrive live as progress notifications for the operator/client
watching the run. This is NOT a live mid-call early-cancel signal
for the synchronously-blocked driving agent — the agent sees the
heartbeats only after the call returns (and as groundwork for a
future async run_stage).

Summary fields (scalars distilled from the event stream, omitted
when zero/empty):

  - outcome         — terminal runner outcome (ok | failed) from the
                      runner_completed event.
  - turns           — agent turn count from the last heartbeat.
  - tokens_used     — tokens consumed; from runner_completed when
                      present, else the last heartbeat's running total.
  - elapsed_seconds — wall-clock seconds from the last heartbeat.
  - last_event_kind — the agent's last event kind from the last
                      heartbeat.

Output fields (three additional best-effort fields, omitted when
unavailable):

  - diff_summary    — present when the runner emitted a git_diff
                      event and 'git show --numstat HEAD' succeeded;
                      reports files_changed, insertions, deletions.
                      nil for plan stages and failed implement stages
                      that did not commit.
  - audit_pointer   — the most-recent audit entry for this run:
                      latest_sequence, entry_hash, and a URL to the
                      per-run audit API endpoint. nil when the
                      best-effort fetch fails.
  - run_url         — direct link to the run-detail view in the SPA.
  - stage_wait_status — the stage-execution wait status on the durable
                      (run_id, stage_id) handle: status is one of
                      pending/running/succeeded/failed/cancelled. On a
                      synchronous return the stage is normally already
                      terminal (poll_interval_seconds omitted); poll
                      fishhawk_get_run_status to await a non-terminal stage.
  - next_actions    — server-suggested next actions (#1024): the
                      classified run lifecycle state plus the legal next
                      moves after this stage ran, each naming the tool to
                      call, its precondition, what it consumes (none /
                      fixup_budget / retry_budget / approval_slot /
                      new_run), and a one-line reason. Same classifier as
                      fishhawk_get_run_status; display-only, never gates.

Cancellation: cancelling the tool call sends SIGTERM (then SIGKILL
after a 30s grace) to the runner. Graceful cleanup requires the
runner-side SIGTERM handler (#435) — until it lands, cancellation
is kill-only and the stage relies on the SLA ticker to reap.

Requires the fishhawk-runner binary to resolve on the MCP server's
host; this tool is local-only by design (ADR-024 Q5).
`),
	}, resolver.runStage)
}

// runStage is the tool handler.
//
// Composition order:
//  1. validate the obvious inputs (run_id, stage_id, workflow, stage).
//  2. resolve the runner binary (input > FISHHAWK_RUNNER_BIN env > os.Executable sibling dir > PATH > error).
//  3. resolve GitHub repo (input > working-dir origin remote;
//     soft-failure when push_and_open_pr is false).
//  4. compose argv mirroring `fishhawk runner start`.
//  5. spawn the runner; pipe stdout through a JSONL parser that
//     forwards each event as a progress notification (when a
//     progress token was given) and accumulates them all.
//  6. on context cancellation, SIGTERM + grace + SIGKILL the child.
//  7. wait for exit; fetch the post-run stage state for the result.
func (r *runResolver) runStage(ctx context.Context, req *mcp.CallToolRequest, in RunStageInput) (*mcp.CallToolResult, RunStageOutput, error) {
	// (1) Input validation. stage_id is optional: when omitted it is
	// resolved tool-side from (run_id, stage type) below.
	if in.RunID == "" || in.Workflow == "" || in.Stage == "" {
		return nil, RunStageOutput{}, errors.New("run_id, workflow, and stage are all required")
	}
	runUUID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, RunStageOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}

	// Resolve the push_and_open_pr default. A bare bool can't tell an
	// omitted JSON key from an explicit false, so the field is *bool:
	// nil (omitted) -> true for the MCP-driven local loop (ADR-031
	// Phase 1) so every run carries a pull_request_url; an explicit
	// false is honored (the commit-yourself flow).
	pushAndOpenPR := in.PushAndOpenPR == nil || *in.PushAndOpenPR
	// Only parse stage_id when explicitly supplied — preserve the
	// "not a valid UUID" error for a non-empty bad value.
	if in.StageID != "" {
		if _, perr := uuid.Parse(in.StageID); perr != nil {
			return nil, RunStageOutput{}, fmt.Errorf("stage_id %q is not a valid UUID: %w", in.StageID, perr)
		}
	}

	// Resolve the stage id from (run_id, stage type), honouring an
	// explicit stage_id when supplied (back-compat) and erroring when
	// it disagrees. This replaces the hand-copied-UUID error class
	// (#602) and folds in #583's belongs-to-run validation.
	resolvedStageID, err := r.resolveStageID(ctx, runUUID, in.Stage, in.StageID)
	if err != nil {
		return nil, RunStageOutput{}, err
	}
	stageUUID, err := uuid.Parse(resolvedStageID)
	if err != nil {
		return nil, RunStageOutput{}, fmt.Errorf("resolved stage_id %q is not a valid UUID: %w", resolvedStageID, err)
	}

	// (2) Resolve the runner binary.
	// Resolution order: input > FISHHAWK_RUNNER_BIN env > os.Executable sibling dir > PATH > error.
	binary := in.RunnerBinary
	if binary == "" {
		if env := r.getenv("FISHHAWK_RUNNER_BIN"); env != "" {
			binary = env
		}
	}
	if binary == "" {
		if exe, exeErr := runStageExecutable(); exeErr == nil {
			sibling := filepath.Join(filepath.Dir(exe), "fishhawk-runner")
			if _, statErr := os.Stat(sibling); statErr == nil {
				binary = sibling
			}
		}
	}
	if binary == "" {
		resolved, lerr := runStageLookPath("fishhawk-runner")
		if lerr != nil {
			return nil, RunStageOutput{}, errors.New(
				"fishhawk-runner not on PATH; this tool requires local MCP execution — " +
					"pass runner_binary, set FISHHAWK_RUNNER_BIN, or co-locate fishhawk-runner with fishhawk-mcp")
		}
		binary = resolved
	}

	workingDir := in.WorkingDir
	if workingDir == "" {
		workingDir = "."
	}

	// (3) Resolve the GitHub repo. push_and_open_pr=false means the
	// runner won't push, so a missing repo is acceptable — collect
	// as a warning rather than failing.
	var warnings []string
	repo := in.GitHubRepo
	if repo == "" {
		detected, derr := runStageDetectGitHubRepo(workingDir)
		switch {
		case derr == nil:
			repo = detected
		case pushAndOpenPR:
			return nil, RunStageOutput{}, fmt.Errorf(
				"github_repo not set and could not detect from origin (push_and_open_pr requires a repo): %w", derr)
		default:
			warnings = append(warnings,
				fmt.Sprintf("github_repo not set and origin auto-detect failed (%v); proceeding without push.", derr))
		}
	}

	// (4) Build the runner argv. Mirrors
	// cli/cmd/fishhawk/runner.go::runRunnerStart — keep in sync.
	argv := []string{
		"--run-id", in.RunID,
		"--backend-url", r.api.baseURL,
		"--workflow", in.Workflow,
		"--stage", in.Stage,
		"--stage-id", resolvedStageID,
		"--working-dir", workingDir,
		"--fetch-prompt",
		"--upload-trace",
	}
	if in.Stage == "plan" {
		argv = append(argv, "--plan-out", "/tmp/fishhawk-plan.json")
	}
	if repo != "" {
		argv = append(argv, "--github-repo", repo)
	}
	baseBranch := in.BaseBranch
	if baseBranch == "" {
		baseBranch = "main"
	}
	argv = append(argv, "--base-branch", baseBranch)
	// Only implement stages produce a diff to enforce. Passing
	// --check-base-ref makes the runner run computeAndEmitDiff, which
	// emits the git_diff event the backend needs to re-evaluate policy
	// (policy_evaluated) and run implement-review (#561/#585). Plan and
	// review stages legitimately produce no diff, so they omit it.
	// Mirrors the runner's own gate (computeAndEmitDiff runs iff
	// cfg.checkBaseRef != "").
	if in.Stage == "implement" {
		argv = append(argv, "--check-base-ref", baseBranch)
	}
	if !pushAndOpenPR {
		argv = append(argv, "--no-pr")
	}

	cmd := runStageCommand(binary, argv...)
	cmd.Env = append(os.Environ(), "FISHHAWK_API_TOKEN="+r.api.token)

	// Run the subprocess in its own process group so signals reach
	// the whole tree, not just the direct child. The runner spawns
	// further descendants (the agent it invokes, that agent's tool
	// processes); without a group, SIGTERM/SIGKILL on the runner
	// leaves orphans alive that inherit our stdout fd — which keeps
	// the pipe open and prevents the bufio scanner from ever
	// reaching EOF. Signalling -pgid hits every descendant in
	// lockstep. The #446 plan's risk section claimed "SIGKILL
	// handles orphans" — that's incorrect for Unix fd inheritance,
	// surfaced by TestRunStage_ContextCancelSendsSIGTERM after the
	// pipe-drain reorder.
	runStageSetProcessGroup(cmd)

	// (5) Wire stderr to a JSONL parser via TeeReader so events reach
	// the accumulator while the raw stream is also forwarded to the
	// operator's terminal. Stdout carries no structured events when
	// --upload-trace is set (the runner writes all JSONL to stderr /
	// logSink), so forward it directly to the terminal.
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, RunStageOutput{}, fmt.Errorf("attach stderr: %w", err)
	}
	cmd.Stdout = os.Stdout

	if err := cmd.Start(); err != nil {
		return nil, RunStageOutput{}, fmt.Errorf("spawn fishhawk-runner: %w", err)
	}

	// Concurrently: parse stdout into events; watch ctx for
	// cancellation and signal the child. parseDone closes when the
	// parser goroutine drains stdout to EOF — guaranteeing cmd.Wait
	// is called after the pipe is fully read (#446).
	var (
		events    []RunnerEvent
		eventsMu  sync.Mutex
		parseDone = make(chan struct{})
		progToken any
	)
	if req != nil && req.Params != nil {
		progToken = req.Params.GetProgressToken()
	}

	go func() {
		defer close(parseDone)
		runStageParseEvents(ctx, io.TeeReader(stderrPipe, os.Stderr), &events, &eventsMu, &warnings, req, progToken)
	}()

	// (6) Cancellation watcher: on ctx.Done(), signal the whole
	// process group (not just the direct child) so descendants that
	// inherited stdout die too — only then does the pipe close and
	// the scanner reach EOF. Escalates to SIGKILL (group-wide)
	// after the grace period.
	//
	// Snapshot the grace period at entry so a test's t.Cleanup
	// restoring runStageGracePeriod doesn't race the goroutine.
	grace := runStageGracePeriod
	go func() {
		select {
		case <-parseDone:
			// Normal exit — subprocess closed stdout, scanner drained.
		case <-ctx.Done():
			runStageSignalGroup(cmd, syscall.SIGTERM)
			select {
			case <-parseDone:
				// Subprocess exited within grace; scanner done.
			case <-time.After(grace):
				runStageSignalGroup(cmd, syscall.SIGKILL)
				<-parseDone
			}
		}
	}()

	// (7) Block until the parser goroutine has fully drained stdout,
	// then call cmd.Wait(). The pipe is guaranteed drained at this
	// point — no scanner-vs-Wait race.
	<-parseDone
	waitErr := cmd.Wait()

	exitCode := 0
	switch {
	case waitErr == nil:
		exitCode = 0
	case errors.As(waitErr, new(*exec.ExitError)):
		var exitErr *exec.ExitError
		_ = errors.As(waitErr, &exitErr)
		exitCode = exitErr.ExitCode()
	default:
		return nil, RunStageOutput{}, fmt.Errorf("runner subprocess: %w", waitErr)
	}

	// Fetch the post-run stage state. Best-effort: a backend hiccup
	// here doesn't fail the tool — the audit log has the canonical
	// transition record.
	stageState := ""
	var postStages []Stage
	if fetchErr := func() error {
		fetchCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stages, ferr := r.api.ListRunStages(fetchCtx, runUUID)
		if ferr != nil {
			return ferr
		}
		postStages = stages
		for _, s := range stages {
			if s.ID == stageUUID.String() {
				stageState = s.State
				return nil
			}
		}
		return fmt.Errorf("stage %s not found in run %s stage list", stageUUID, runUUID)
	}(); fetchErr != nil {
		warnings = append(warnings,
			fmt.Sprintf("post-run stage fetch failed: %v", fetchErr))
	}

	// Stage-execution wait status (#879/#880, ADR-037) on the durable
	// (run_id, stage_id) handle, derived from the post-run stage fetch above —
	// no extra round-trip. On a normal synchronous return the stage is already
	// terminal, so the interval is omitted and the field records the terminal
	// outcome; nil when the fetch failed (stageState empty). The run state is
	// not fetched here (the synchronous return implies a settled stage), so the
	// ADR-036 backstop is a no-op — pass "" for runState.
	var stageWaitStatus *StageWaitStatus
	if stageState != "" {
		stageWaitStatus = classifyStageWaitStatus(in.Stage, stageState, "")
	}

	// Populate DiffSummary: gate on the git_diff runner event being
	// present so plan stages (which never emit git_diff) always yield
	// nil, and failed implement stages that didn't commit also yield nil.
	var diffSummary *DiffSummary
	if gitDiffPayload := findGitDiffPayload(events); gitDiffPayload != nil {
		filesChanged := 0
		if cf, ok := gitDiffPayload["changed_files"].([]any); ok {
			filesChanged = len(cf)
		}
		if numstatOut, numstatErr := runStageGitNumstat(workingDir); numstatErr == nil {
			ins, dels := parseNumstat(numstatOut)
			diffSummary = &DiffSummary{
				FilesChanged: filesChanged,
				Insertions:   ins,
				Deletions:    dels,
			}
		}
	}

	// Populate AuditPointer: best-effort, 5-second timeout. Mirrors
	// the stageState fetch pattern above. Nil on any failure — no
	// warning per the acceptance criteria.
	var auditPointer *AuditPointer
	func() {
		fetchCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		entries, aerr := r.api.ListRecentRunAudit(fetchCtx, runUUID, 1)
		if aerr != nil || len(entries) == 0 {
			return
		}
		auditPointer = &AuditPointer{
			LatestSequence: entries[0].Sequence,
			EntryHash:      entries[0].EntryHash,
			URL:            r.api.baseURL + "/v0/runs/" + runUUID.String() + "/audit?limit=1",
		}
	}()

	// RunURL is the SPA run-detail link — pure string concatenation,
	// no failure mode.
	runURL := r.api.baseURL + "/runs/" + runUUID.String()

	// Distill the scalar summary and the compact (heartbeat-free) event
	// list. Default to compact; verbose:true restores the full list.
	summary, filtered := summarizeRunStageEvents(events)
	resultEvents := events
	if !in.Verbose {
		resultEvents = filtered
	}

	// Best-effort periodic-budget status (#693), fetched after the stage
	// runs so it reflects this stage's spend. A fetch error appends a
	// warning and leaves the field nil — never fails the stage.
	budgetStatus, budgetWarn := r.fetchBudgetStatus(ctx, runUUID)
	if budgetWarn != "" {
		warnings = append(warnings, budgetWarn)
	}

	// Post-run run row + drive view (#1023/#1024), fetched once
	// best-effort: it feeds the hint's terminal suppression (#968), the
	// drive fold, and the next_actions classifier. Same GET
	// /v0/runs/{run_id} the old per-hint GetRun hit. Nil on a fetch
	// error — never fails the stage.
	var runView *runDriveView
	if v, verr := r.fetchRunDriveView(ctx, runUUID); verr == nil {
		runView = v
	} else {
		warnings = append(warnings, fmt.Sprintf("post-run run fetch failed (next_actions omitted): %v", verr))
	}

	// Review statuses (best-effort): the implement status feeds both the
	// #777 hint and the next_actions classifier; the plan status feeds the
	// classifier's plan-gate arms. A fetch error appends a warning and
	// leaves the field nil — never fails the stage.
	implementReviewStatus, irsErr := r.reviewStatusFor(ctx, runUUID, "implement")
	if irsErr != nil {
		implementReviewStatus = nil
		warnings = append(warnings, fmt.Sprintf("implement review status unavailable: %v", irsErr))
	}
	planReviewStatus, prsErr := r.reviewStatusFor(ctx, runUUID, "plan")
	if prsErr != nil {
		planReviewStatus = nil
		warnings = append(warnings, fmt.Sprintf("plan review status unavailable: %v", prsErr))
	}

	// Best-effort review-action hint (#777), only for implement stages —
	// plan and review stages have no implement review to act on.
	// start_run is excluded by construction.
	var reviewActionHint *ReviewActionHint
	if in.Stage == "implement" && irsErr == nil {
		runState := ""
		if runView != nil {
			runState = runView.State
		}
		var hintErr error
		reviewActionHint, hintErr = r.reviewActionHintFor(ctx, runUUID, stageUUID, runState, implementReviewStatus)
		if hintErr != nil {
			warnings = append(warnings, fmt.Sprintf("review-action hint unavailable: %v", hintErr))
		}
	}

	// Server-suggested next actions (#1024): the same classifier
	// fishhawk_get_run_status uses, computed from the post-stage fetches
	// above so a terminal run_stage call hands the operator the legal
	// next move directly. Omitted when the run fetch failed.
	var nextActions *NextActions
	if runView != nil {
		nextActions = nextActionsFor(&runView.Run, postStages, planReviewStatus, implementReviewStatus, reviewActionHint, runView.driveStatus())
	}

	out := RunStageOutput{
		ExitCode:         exitCode,
		StageState:       stageState,
		Events:           resultEvents,
		Warnings:         warnings,
		DiffSummary:      diffSummary,
		AuditPointer:     auditPointer,
		RunURL:           runURL,
		StageWaitStatus:  stageWaitStatus,
		Outcome:          summary.Outcome,
		Turns:            summary.Turns,
		TokensUsed:       summary.TokensUsed,
		ElapsedSeconds:   summary.ElapsedSeconds,
		LastEventKind:    summary.LastEventKind,
		FixupNoChanges:   summary.FixupNoChanges,
		Budget:           budgetStatus,
		ReviewActionHint: reviewActionHint,
		NextActions:      nextActions,
	}

	// Return-cancellation signal: if the parent ctx was the reason
	// for exit, surface a clear tool error rather than a 0 / silent
	// success.
	if ctx.Err() != nil {
		return nil, out, fmt.Errorf("fishhawk_run_stage cancelled: %w", ctx.Err())
	}

	return nil, out, nil
}

// resolveStageID resolves the stage UUID to spawn the runner against
// from (run_id, stage type). It lists the run's stages (ListRunStages)
// and matches on Stage.Type:
//
//   - explicitStageID != "": back-compat path. The explicit id must be
//     one of the run's stages of stageType; otherwise it errors. This
//     also enforces #583's belongs-to-run invariant on the explicit
//     path — a fat-fingered or wrong-run UUID no longer spawns.
//   - explicitStageID == "": auto-resolve. Requires exactly one stage
//     of stageType. Zero matches errors naming the available stage
//     types; more than one errors (ambiguous) naming the duplicate ids
//     rather than silently picking one.
//
// Uses a bounded context mirroring the post-run stage fetch.
func (r *runResolver) resolveStageID(ctx context.Context, runUUID uuid.UUID, stageType, explicitStageID string) (string, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stages, err := r.api.ListRunStages(fetchCtx, runUUID)
	if err != nil {
		return "", fmt.Errorf("resolve stage_id: list stages for run %s: %w", runUUID, err)
	}

	var matches []string
	available := make([]string, 0, len(stages))
	for _, s := range stages {
		available = append(available, s.Type)
		if s.Type == stageType {
			matches = append(matches, s.ID)
		}
	}

	if explicitStageID != "" {
		for _, id := range matches {
			if id == explicitStageID {
				return explicitStageID, nil
			}
		}
		return "", fmt.Errorf(
			"stage_id %s does not match a %q stage in run %s; available stage types: %v",
			explicitStageID, stageType, runUUID, available)
	}

	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf(
			"stage type %q not found in run %s; available: %v",
			stageType, runUUID, available)
	default:
		return "", fmt.Errorf(
			"stage type %q is ambiguous in run %s (matched %d stages: %s); pass stage_id explicitly",
			stageType, runUUID, len(matches), strings.Join(matches, ", "))
	}
}

// runStageParseEvents reads JSONL lines from r and appends each
// parsed event to events. When progToken is non-nil and req has a
// session, each event also emits as a progress notification (best-
// effort — a failed notify only adds a warning).
//
// Lines that don't parse as JSON land in warnings rather than
// failing the read; the runner is the authority on its output
// format and a CLI-style "Running plan stage..." print shouldn't
// blow up the tool.
func runStageParseEvents(
	ctx context.Context,
	r io.Reader,
	events *[]RunnerEvent,
	mu *sync.Mutex,
	warnings *[]string,
	req *mcp.CallToolRequest,
	progToken any,
) {
	scanner := bufio.NewScanner(r)
	// JSONL lines from the runner can be large (trace events with
	// embedded payloads). Bump from the default 64KiB to 1MiB.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var progress float64
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r\n")
		if line == "" {
			continue
		}
		var payload any
		if perr := json.Unmarshal([]byte(line), &payload); perr != nil {
			mu.Lock()
			*warnings = append(*warnings, fmt.Sprintf("non-JSON runner stderr: %q", line))
			mu.Unlock()
			continue
		}
		mu.Lock()
		*events = append(*events, RunnerEvent{Payload: payload})
		mu.Unlock()
		if progToken != nil && req != nil && req.Session != nil {
			progress++
			_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
				ProgressToken: progToken,
				Progress:      progress,
				Message:       runStageEventMessage(payload),
			})
		}
	}
	if serr := scanner.Err(); serr != nil && !errors.Is(serr, io.EOF) {
		mu.Lock()
		*warnings = append(*warnings, fmt.Sprintf("scan runner stderr: %v", serr))
		mu.Unlock()
	}
}

// runStageEventMessage extracts a short message from a runner event
// for the progress notification's human-readable summary. Looks for
// a top-level `kind` or `type` field (the runner's event shape uses
// `kind`); falls back to the JSON-encoded payload truncated.
//
// A stage_progress heartbeat (#580) is special-cased: its Message
// carries the coarse counters (turns / tokens-so-far / elapsed /
// last event kind) so the driver can tell a progressing stage from a
// stalled one. This Message is the only per-event field the relay
// populates, so it is where the liveness signal must land.
func runStageEventMessage(payload any) string {
	if m, ok := payload.(map[string]any); ok {
		if ev, _ := m["event"].(string); ev == "stage_progress" {
			// JSON numbers decode as float64; format as integers.
			num := func(k string) float64 {
				f, _ := m[k].(float64)
				return f
			}
			last, _ := m["last_event_kind"].(string)
			return fmt.Sprintf("stage_progress turns=%.0f tokens=%.0f elapsed=%.0fs last=%s",
				num("turns"), num("tokens_so_far"), num("elapsed_seconds"), last)
		}
		// A scope_amendment_pending event (#1035) is the in-band signal that
		// the agent filed a mid-stage scope amendment and is now blocking on
		// its ?wait long-poll for a decision. The generic fallback below
		// would relay only the bare event name, losing the actionable
		// amendment_id + paths, so format them explicitly: the operator,
		// watching this blocked fishhawk_run_stage call's progress, can then
		// decide the request from a second session via
		// fishhawk_decide_scope_amendment while the agent waits. The field
		// names {event, amendment_id, paths} are the literal-JSONL seam this
		// relay shares with the runner emitter (cf. #618).
		if ev, _ := m["event"].(string); ev == "scope_amendment_pending" {
			id, _ := m["amendment_id"].(string)
			var paths []string
			if raw, ok := m["paths"].([]any); ok {
				for _, p := range raw {
					pm, ok := p.(map[string]any)
					if !ok {
						continue
					}
					path, _ := pm["path"].(string)
					if path == "" {
						continue
					}
					op, _ := pm["operation"].(string)
					if op != "" {
						paths = append(paths, path+" ("+op+")")
					} else {
						paths = append(paths, path)
					}
				}
			}
			return fmt.Sprintf("scope_amendment_pending id=%s paths=%s",
				id, strings.Join(paths, ", "))
		}
		for _, key := range []string{"kind", "type", "event"} {
			if v, ok := m[key].(string); ok && v != "" {
				return v
			}
		}
	}
	enc, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	const maxLen = 120
	if len(enc) > maxLen {
		return string(enc[:maxLen]) + "..."
	}
	return string(enc)
}

// runStageDetectGitHubRepo mirrors the CLI's helper. Returns
// (owner/name, nil) when the working-dir origin resolves to a
// github.com URL; otherwise a descriptive error.
func runStageDetectGitHubRepo(workingDir string) (string, error) {
	raw, err := runStageGitRemoteOriginURL(workingDir)
	if err != nil {
		return "", fmt.Errorf("`git remote get-url origin`: %w", err)
	}
	owner, name, err := runStageParseGitHubRemote(raw)
	if err != nil {
		return "", err
	}
	return owner + "/" + name, nil
}

// runStageSummary holds the scalar progress/outcome fields distilled
// from the runner's event stream so the compact tool result carries
// the durable signal without the per-heartbeat noise (#647).
type runStageSummary struct {
	Outcome        string
	Turns          int
	TokensUsed     int
	ElapsedSeconds int
	LastEventKind  string
	// FixupNoChanges is set when the relayed stream carried an
	// implement_fixup_no_changes event (#967): the fix-up pass produced no
	// commit, which summary.Outcome alone ("ok" from runner_completed)
	// would mask as a plain success.
	FixupNoChanges bool
}

// summarizeRunStageEvents walks the runner's events once and returns
// (a) a scalar summary distilled from the stage_progress heartbeats
// and the terminal runner_completed event, and (b) a filtered event
// slice with the routine stage_progress heartbeats removed. Every
// other event — including runner_completed, git_diff, and
// runner_cancelled — is retained in arrival order.
//
// Heartbeats are discriminated by a top-level `event=="stage_progress"`
// field; the last heartbeat's counters win. The terminal
// `event=="runner_completed"` event carries the authoritative
// outcome/tokens_used and overrides the heartbeat-derived TokensUsed
// when present. Reading outcome/tokens from runner_completed — not the
// bundle-only `kind=="invocation_end"` event — is load-bearing: only
// runner_completed is relayed on the JSONL stderr stream this tool
// reads (invocation_end is appended to the signed trace bundle only),
// so keying on invocation_end would leave Outcome permanently empty in
// production.
func summarizeRunStageEvents(events []RunnerEvent) (runStageSummary, []RunnerEvent) {
	var summary runStageSummary
	filtered := make([]RunnerEvent, 0, len(events))
	numInt := func(m map[string]any, k string) int {
		f, _ := m[k].(float64)
		return int(f)
	}
	for _, ev := range events {
		m, ok := ev.Payload.(map[string]any)
		if !ok {
			filtered = append(filtered, ev)
			continue
		}
		evType, _ := m["event"].(string)
		switch evType {
		case "stage_progress":
			// Routine heartbeat: capture counters (last wins) and drop
			// it from the filtered slice.
			summary.Turns = numInt(m, "turns")
			summary.TokensUsed = numInt(m, "tokens_so_far")
			summary.ElapsedSeconds = numInt(m, "elapsed_seconds")
			if last, _ := m["last_event_kind"].(string); last != "" {
				summary.LastEventKind = last
			}
			continue
		case "runner_completed":
			// Terminal runner-level event: authoritative outcome and
			// token total. Overrides the heartbeat-derived TokensUsed.
			if oc, _ := m["outcome"].(string); oc != "" {
				summary.Outcome = oc
			}
			summary.TokensUsed = numInt(m, "tokens_used")
		case "implement_fixup_no_changes":
			// No-change fix-up pass (#967): surfaced as a dedicated flag so
			// the operator sees the no-op in the tool result instead of a
			// plain runner_completed "ok". The event stays in the filtered
			// slice (it is not a routine heartbeat).
			summary.FixupNoChanges = true
		}
		filtered = append(filtered, ev)
	}
	return summary, filtered
}

// findGitDiffPayload scans events for a runner event with
// kind=="git_diff" and returns its payload map, or nil when absent.
func findGitDiffPayload(events []RunnerEvent) map[string]any {
	for _, ev := range events {
		m, ok := ev.Payload.(map[string]any)
		if !ok {
			continue
		}
		if m["kind"] == "git_diff" {
			return m
		}
	}
	return nil
}

// parseNumstat parses `git show --numstat HEAD` output and sums
// insertions and deletions across all file rows, skipping binary-file
// rows where either column is '-'.
func parseNumstat(output string) (insertions, deletions int) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 2 {
			continue
		}
		if parts[0] == "-" || parts[1] == "-" {
			continue // binary file — columns are not numeric
		}
		ins, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		del, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		insertions += ins
		deletions += del
	}
	return
}

// runStageParseGitHubRemote turns a remote URL into (owner, name).
// Mirrors cli/cmd/fishhawk/runner.go::parseGitHubRemote.
func runStageParseGitHubRemote(raw string) (owner, name string, err error) {
	s := strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(s, "https://github.com/"):
		s = strings.TrimPrefix(s, "https://github.com/")
	case strings.HasPrefix(s, "git@github.com:"):
		s = strings.TrimPrefix(s, "git@github.com:")
	case strings.HasPrefix(s, "ssh://git@github.com/"):
		s = strings.TrimPrefix(s, "ssh://git@github.com/")
	default:
		return "", "", fmt.Errorf("remote %q is not a github.com URL", raw)
	}
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("remote %q is not owner/name", raw)
	}
	return parts[0], parts[1], nil
}
