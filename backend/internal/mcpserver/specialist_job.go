package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Specialist jobs (ADR-097 D2/D6, E85.9 / #4300): the two MCP tools over the
// /v0/specialist-jobs routes. fishhawk_request_specialist_job creates a job
// and, for every runner-executed kind, spawns `fishhawk-runner
// specialist-job` DETACHED on this host; fishhawk_get_specialist_job reads it,
// optionally waiting for completion. Long-form contract:
// backend/internal/specialistjob/README.md; tool notes:
// backend/internal/mcpserver/README.md § "Specialist jobs".

const (
	// specialistJobKindDraft is the one kind rendered in-process by the
	// server at request time: it answers already completed and never spawns.
	specialistJobKindDraft = "draft"
	// specialistJobKindPlanCrosscheck and specialistJobKindInventory are the
	// two other kinds next_actions suggests (ADR-097 D1, #4301): a tree-fact
	// check goes to a plan_crosscheck job, a bounded git listing to an
	// inventory job. TestSpecialistJobSuggestionKindsAreKnown pins every
	// suggested kind to specialistjob.AllKinds.
	specialistJobKindPlanCrosscheck = "plan_crosscheck"
	specialistJobKindInventory      = "inventory"
	// specialistJobStateCompleted is the job's terminal state.
	specialistJobStateCompleted = "completed"
	// specialistJobMaxWaitSeconds bounds fishhawk_get_specialist_job's wait.
	specialistJobMaxWaitSeconds = 300
	// defaultSpecialistJobPollInterval is the get tool's poll cadence.
	defaultSpecialistJobPollInterval = 2 * time.Second
	// specialistJobUnknownHost mirrors concurrency.UnknownHost: the label the
	// server substitutes for an empty host, so a host-less MCP process puts
	// its job in the SAME local-verify:unknown group its stages land in.
	// (concurrency is not imported: it would pull pgx into fishhawk-mcp.)
	specialistJobUnknownHost = "unknown"
	// specialistJobAttestTokenEnv names the runner credential holding
	// attest:specialist-jobs (E85.15 / #4363). The spawned runner inherits it
	// through os.Environ() and prefers it over FISHHAWK_API_TOKEN; unset, the
	// result records provenance bearer.
	specialistJobAttestTokenEnv = "FISHHAWK_RUNNER_ATTEST_TOKEN"
)

// specialistJobBearerProvenanceWarning is the warning a spawn without
// FISHHAWK_RUNNER_ATTEST_TOKEN carries: the result will be advisory.
const specialistJobBearerProvenanceWarning = specialistJobAttestTokenEnv + " is unset: job %s's result will record provenance bearer " +
	"(advisory, not ADR-089 D4 blocking-grade evidence). To make results runner_attested, issue a runner token with " +
	"`fishhawkd token issue --subject specialist-runner/<host> --scopes attest:specialist-jobs` and export it as " +
	specialistJobAttestTokenEnv + " in this process's environment"

// RequestSpecialistJobInput is fishhawk_request_specialist_job's input.
type RequestSpecialistJobInput struct {
	Kind         string               `json:"kind" jsonschema:"the job kind: counterfactual_battery, fanin_build, overlap_scan, plan_crosscheck, inventory or draft"`
	Subject      SpecialistJobSubject `json:"subject" jsonschema:"what the job is about (run_id, campaign_id, head_sha, repo); at least one member, and head_sha is required for every kind except draft"`
	Inputs       map[string]any       `json:"inputs" jsonschema:"the per-kind inputs object, exactly as docs/spec/specialist-job-v1.md defines it (closed members, bounded lists; no free-form shell)"`
	WorkingDir   string               `json:"working_dir,omitempty" jsonschema:"ABSOLUTE path of the checkout the runner clones from (the clone SOURCE; the job never executes in it). Required over the HTTP MCP transport; ignored for draft"`
	RunnerBinary string               `json:"runner_binary,omitempty" jsonschema:"path to fishhawk-runner; defaults to FISHHAWK_RUNNER_BIN, the fishhawk-mcp sibling, then PATH"`
	Spawn        *bool                `json:"spawn,omitempty" jsonschema:"spawn the runner for a runner-executed kind (default true). false creates the job queued and spawns nothing — you run fishhawk-runner specialist-job yourself"`
}

// RequestSpecialistJobOutput is fishhawk_request_specialist_job's result.
type RequestSpecialistJobOutput struct {
	Job      SpecialistJob    `json:"job" jsonschema:"the created job: completed for draft (result.evidence.text carries the rendering), queued otherwise"`
	Spawned  bool             `json:"spawned" jsonschema:"true when a detached fishhawk-runner specialist-job process was started for this job"`
	LogPath  string           `json:"log_path,omitempty" jsonschema:"the detached runner's local stdout/stderr log (owner-only); present when spawned"`
	Warnings []string         `json:"warnings,omitempty" jsonschema:"non-fatal notes, including a spawn failure AFTER the job was created (the job then stays queued; run the named command yourself)"`
	NextStep *SuggestedAction `json:"next_step,omitempty" jsonschema:"the single wait to call: fishhawk_get_specialist_job with job_id and wait_seconds pre-filled. Absent for draft (already completed)"`
}

// GetSpecialistJobInput is fishhawk_get_specialist_job's input.
type GetSpecialistJobInput struct {
	JobID       string `json:"job_id" jsonschema:"the specialist job UUID"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"0..300: hold until the job completes or this many seconds pass (0 reads once)"`
}

// GetSpecialistJobOutput is fishhawk_get_specialist_job's result.
type GetSpecialistJobOutput struct {
	Job       SpecialistJob    `json:"job"`
	Completed bool             `json:"completed" jsonschema:"true once the job is completed and job.result carries the typed verdict and evidence"`
	TimedOut  bool             `json:"timed_out,omitempty" jsonschema:"true when wait_seconds elapsed before the job completed"`
	Stale     bool             `json:"stale" jsonschema:"the server's stale flag: the job's runner stopped polling or heartbeating and is likely gone; re-spawn it or abandon the job"`
	NextStep  *SuggestedAction `json:"next_step,omitempty" jsonschema:"present while the job is not completed and not stale: wait again with fishhawk_get_specialist_job"`
}

// specialistJobSpawnFunc starts a detached runner; it returns the log path.
type specialistJobSpawnFunc func(binary string, argv, env []string, jobID string) (string, error)

func registerRequestSpecialistJob(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_request_specialist_job",
		Description: strings.TrimSpace(`
WHEN: you want typed, runner-observed evidence about a run, campaign or
commit without opening a run — a counterfactual battery, a fan-in build of
slice heads, a file-overlap scan across runs, a plan cross-check, a bounded
repository inventory — or a deterministic ADR/issue draft skeleton (ADR-097
D2). ELIGIBILITY: an operator token carrying write:stages; a run-bound agent
token is refused specialist_job_requester_not_a_seat. The requester is
derived from your token, never from the input.

What it does: validates working_dir (path confinement) and resolves the
runner binary FIRST, so a refusal creates nothing; then POSTs
/v0/specialist-jobs (one specialist_job_requested audit row). A draft is
rendered server-side as text and returns already completed — it never
spawns and writes nothing beyond its two audit rows. Every other kind is
created queued and, unless spawn=false, a fishhawk-runner specialist-job
process is spawned DETACHED on this host: it clones working_dir into a
scratch directory (never the lineage worktree), waits for a slot in the
local-verify:<host> concurrency group (it yields to verify stages), runs
the kind and ships the typed result. It cannot push, and cannot call any
run or stage route.

Inputs:
  - kind, subject, inputs : the specialist-job-v1 request (see
    docs/spec/specialist-job-v1.md for each kind's inputs).
  - working_dir : ABSOLUTE checkout path (required over HTTP; not for draft).
  - runner_binary, spawn : optional.

Returns the job, spawned, log_path and a next_step naming
fishhawk_get_specialist_job. Results are evidence, not a gate verdict: a
result records provenance runner_attested (blocking_eligible true) only when
the spawned runner holds FISHHAWK_RUNNER_ATTEST_TOKEN (a token carrying
attest:specialist-jobs) and signs it; otherwise it records bearer, and a
spawn without that variable carries a warning saying so.

Tool errors: path_outside_allowed_roots / working_dir refusals and an
unresolvable runner binary (before any POST); validation_failed (400),
insufficient_scope / specialist_job_requester_not_a_seat (403),
run_not_found / campaign_not_found (404), request_too_large (413),
specialist_jobs_unconfigured (503).
`),
	}, resolver.requestSpecialistJob)
}

func registerGetSpecialistJob(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_get_specialist_job",
		Description: strings.TrimSpace(`
WHEN: you requested a specialist job and want its state or typed result.
ELIGIBILITY: a token carrying read:runs; a job in another account is
specialist_job_not_found.

What it does: GET /v0/specialist-jobs/{job_id}. With wait_seconds (0..300)
it re-reads every 2s until the job is completed or the wait elapses. It
reports the server's stale flag: a queued job whose runner stopped polling
admission, or a running job whose runner stopped heartbeating (a crashed
runner stops holding its local-verify slot within 2 minutes). Writes
nothing.

Returns the job (result carries verdict pass | fail | inconclusive plus the
kind's evidence once completed, with provenance runner_attested | bearer and
blocking_eligible, true only for runner_attested), completed, timed_out,
stale and, while the job is still live, a next_step to wait again.
`),
	}, resolver.getSpecialistJob)
}

// requestSpecialistJob is fishhawk_request_specialist_job's handler.
//
// ORDERING (approval condition C6): every pre-spawn refusal — working_dir
// confinement, working_dir resolution, runner-binary resolution — runs BEFORE
// the POST, so a refusal leaves no orphan queued job row behind it.
func (r *runResolver) requestSpecialistJob(ctx context.Context, _ *mcp.CallToolRequest, in RequestSpecialistJobInput) (*mcp.CallToolResult, RequestSpecialistJobOutput, error) {
	kind := strings.TrimSpace(in.Kind)
	if kind == "" {
		return nil, RequestSpecialistJobOutput{}, errors.New("kind is required")
	}
	if cerr := r.confinePath("working_dir", in.WorkingDir); cerr != nil {
		return nil, RequestSpecialistJobOutput{}, cerr
	}
	spawn := kind != specialistJobKindDraft && (in.Spawn == nil || *in.Spawn)
	var workingDir, binary string
	if spawn {
		var err error
		if workingDir, err = r.resolveWorkingDir(in.WorkingDir); err != nil {
			return nil, RequestSpecialistJobOutput{}, err
		}
		if binary, err = resolveRunnerBinary(in.RunnerBinary, r.getenv); err != nil {
			return nil, RequestSpecialistJobOutput{}, err
		}
	}

	job, err := r.api.RequestSpecialistJob(ctx, kind, in.Subject, in.Inputs)
	if err != nil {
		return nil, RequestSpecialistJobOutput{}, fmt.Errorf("request specialist job: %w", err)
	}
	out := RequestSpecialistJobOutput{Job: *job}
	if job.State == specialistJobStateCompleted {
		return nil, out, nil
	}
	out.NextStep = specialistJobWaitStep(job.JobID)
	if !spawn {
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"spawn=false: job %s is queued with no runner; start one with `fishhawk-runner specialist-job --job-id %s --backend-url %s --working-dir <abs checkout> --host <label>` (FISHHAWK_API_TOKEN in env)",
			job.JobID, job.JobID, r.api.baseURL))
		return nil, out, nil
	}

	host := r.api.hostLabelResolution().Label
	if host == "" {
		host = specialistJobUnknownHost
	}
	argv := []string{
		"specialist-job",
		"--job-id", job.JobID,
		"--backend-url", r.api.baseURL,
		"--working-dir", workingDir,
		"--host", host,
	}
	env := append(os.Environ(), "FISHHAWK_API_TOKEN="+r.api.token)
	spawner := r.specialistJobSpawn
	if spawner == nil {
		spawner = spawnSpecialistJobDetached
	}
	logPath, serr := spawner(binary, argv, env, job.JobID)
	if serr != nil {
		// The job row already exists (the POST succeeded), so a spawn failure
		// is reported, not returned: the caller keeps the handle and the
		// command that would run it. The row goes stale after QueueTTL.
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"job %s was created but the runner did not spawn (%v); it stays queued until a runner is started: %s %s",
			job.JobID, serr, binary, strings.Join(argv, " ")))
		return nil, out, nil
	}
	out.Spawned, out.LogPath = true, logPath
	if r.getenv(specialistJobAttestTokenEnv) == "" {
		out.Warnings = append(out.Warnings, fmt.Sprintf(specialistJobBearerProvenanceWarning, job.JobID))
	}
	return nil, out, nil
}

// specialistJobWaitStep is the pre-filled wait on a live job.
func specialistJobWaitStep(jobID string) *SuggestedAction {
	return &SuggestedAction{
		Action:       "fishhawk_get_specialist_job",
		Params:       map[string]string{"job_id": jobID, "wait_seconds": strconv.Itoa(specialistJobMaxWaitSeconds)},
		Precondition: "the job is queued or running",
		Consumes:     "none",
		Reason:       "the runner executes the job asynchronously; wait for its typed result",
	}
}

// spawnSpecialistJobDetached starts `fishhawk-runner specialist-job` detached:
// its own process group (a signal to this MCP process's group is not
// forwarded), output to an owner-only log file under os.TempDir() (never an
// undrained pipe), and a reaper goroutine collecting its exit so it never
// zombies. The same three properties as spawnRunnerStageDetached, without the
// run/stage registry and failure reporting: a job has no stage to fail, and a
// runner that dies leaves the job stale on the read surface.
func spawnSpecialistJobDetached(binary string, argv, env []string, jobID string) (string, error) {
	cmd := runStageCommand(binary, argv...)
	cmd.Env = env
	runStageSetProcessGroup(cmd)
	logPath := filepath.Join(os.TempDir(),
		fmt.Sprintf("fishhawk-specialist-job-%s-%d.log", jobID, time.Now().UnixNano()))
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", fmt.Errorf("open detached specialist-job log %s: %w", logPath, err)
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		_ = os.Remove(logPath)
		return "", fmt.Errorf("start fishhawk-runner specialist-job: %w", err)
	}
	_ = logFile.Close()
	go func() { _ = cmd.Wait() }()
	return logPath, nil
}

// getSpecialistJob is fishhawk_get_specialist_job's handler.
func (r *runResolver) getSpecialistJob(ctx context.Context, _ *mcp.CallToolRequest, in GetSpecialistJobInput) (*mcp.CallToolResult, GetSpecialistJobOutput, error) {
	jobID, err := uuid.Parse(strings.TrimSpace(in.JobID))
	if err != nil {
		return nil, GetSpecialistJobOutput{}, fmt.Errorf("job_id %q is not a valid UUID: %w", in.JobID, err)
	}
	if in.WaitSeconds < 0 || in.WaitSeconds > specialistJobMaxWaitSeconds {
		return nil, GetSpecialistJobOutput{}, fmt.Errorf("wait_seconds must be 0..%d, got %d", specialistJobMaxWaitSeconds, in.WaitSeconds)
	}
	interval := r.specialistJobPollInterval
	if interval <= 0 {
		interval = defaultSpecialistJobPollInterval
	}
	deadline := time.Now().Add(time.Duration(in.WaitSeconds) * time.Second)
	for {
		job, err := r.api.GetSpecialistJob(ctx, jobID)
		if err != nil {
			return nil, GetSpecialistJobOutput{}, fmt.Errorf("get specialist job: %w", err)
		}
		out := GetSpecialistJobOutput{Job: *job, Stale: job.Stale, Completed: job.State == specialistJobStateCompleted}
		if out.Completed || job.Stale {
			return nil, out, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			out.TimedOut = in.WaitSeconds > 0
			out.NextStep = specialistJobWaitStep(job.JobID)
			return nil, out, nil
		}
		select {
		case <-ctx.Done():
			return nil, GetSpecialistJobOutput{}, ctx.Err()
		case <-time.After(min(interval, remaining)):
		}
	}
}
