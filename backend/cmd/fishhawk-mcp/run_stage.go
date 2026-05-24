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
	StageID       string `json:"stage_id" jsonschema:"stage UUID inside the run"`
	Workflow      string `json:"workflow" jsonschema:"workflow ID matching the run's workflow"`
	Stage         string `json:"stage" jsonschema:"stage type: plan | implement | review"`
	WorkingDir    string `json:"working_dir,omitempty" jsonschema:"checkout the agent runs in; defaults to the MCP server's cwd"`
	GitHubRepo    string `json:"github_repo,omitempty" jsonschema:"GitHub repo as owner/name; auto-detected from working_dir's origin remote when empty"`
	BaseBranch    string `json:"base_branch,omitempty" jsonschema:"base branch for the implement-stage PR (no effect when push_and_open_pr is false); defaults to main"`
	PushAndOpenPR bool   `json:"push_and_open_pr,omitempty" jsonschema:"when true, the implement stage pushes and opens a PR; default false (the operator commits the changes themselves)"`
	RunnerBinary  string `json:"runner_binary,omitempty" jsonschema:"path to fishhawk-runner; resolved in order: FISHHAWK_RUNNER_BIN env, then fishhawk-runner sibling to this binary (os.Executable dir), then PATH"`
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
type RunStageOutput struct {
	ExitCode     int           `json:"exit_code"`
	StageState   string        `json:"stage_state,omitempty" jsonschema:"final stage state fetched after the runner exits; empty when the fetch failed"`
	Events       []RunnerEvent `json:"events" jsonschema:"runner-emitted JSONL events in arrival order"`
	Warnings     []string      `json:"warnings,omitempty"`
	DiffSummary  *DiffSummary  `json:"diff_summary,omitempty" jsonschema:"present when the runner emitted a git_diff event and git show --numstat HEAD succeeded; nil for plan stages"`
	AuditPointer *AuditPointer `json:"audit_pointer,omitempty" jsonschema:"most-recent audit entry for this run with its entry hash; nil on any fetch failure"`
	RunURL       string        `json:"run_url,omitempty" jsonschema:"direct link to the run-detail view"`
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
Drive one stage of a Fishhawk run to completion by spawning the
fishhawk-runner binary on the operator's host.

Mirrors the CLI's "fishhawk runner start" verb. Pair with
fishhawk_start_run + fishhawk_approve_plan for the full
agent-driven local-runner loop:

  1. fishhawk_start_run --working-dir ... --issue ... --runner-kind local
  2. fishhawk_run_stage --stage plan ...
  3. fishhawk_approve_plan ...
  4. fishhawk_run_stage --stage implement ...

Runner output streams as MCP progress notifications when the
client provides a progress token; the final tool result carries
the full event list plus the post-run stage state.

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
	// (1) Input validation.
	if in.RunID == "" || in.StageID == "" || in.Workflow == "" || in.Stage == "" {
		return nil, RunStageOutput{}, errors.New("run_id, stage_id, workflow, and stage are all required")
	}
	runUUID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, RunStageOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	stageUUID, err := uuid.Parse(in.StageID)
	if err != nil {
		return nil, RunStageOutput{}, fmt.Errorf("stage_id %q is not a valid UUID: %w", in.StageID, err)
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
		case in.PushAndOpenPR:
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
		"--stage-id", in.StageID,
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
	if !in.PushAndOpenPR {
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
	if fetchErr := func() error {
		fetchCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stages, ferr := r.api.ListRunStages(fetchCtx, runUUID)
		if ferr != nil {
			return ferr
		}
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

	// Return-cancellation signal: if the parent ctx was the reason
	// for exit, surface a clear tool error rather than a 0 / silent
	// success.
	if ctx.Err() != nil {
		return nil, RunStageOutput{
				ExitCode:     exitCode,
				StageState:   stageState,
				Events:       events,
				Warnings:     warnings,
				DiffSummary:  diffSummary,
				AuditPointer: auditPointer,
				RunURL:       runURL,
			},
			fmt.Errorf("fishhawk_run_stage cancelled: %w", ctx.Err())
	}

	return nil, RunStageOutput{
		ExitCode:     exitCode,
		StageState:   stageState,
		Events:       events,
		Warnings:     warnings,
		DiffSummary:  diffSummary,
		AuditPointer: auditPointer,
		RunURL:       runURL,
	}, nil
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
func runStageEventMessage(payload any) string {
	if m, ok := payload.(map[string]any); ok {
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
