// Command fishhawk-runner runs an agent under a Fishhawk workflow
// stage and ships the trace.
//
// E5.1 (#52) shipped the scaffold. E5.2 (#29) wired the Claude Code
// invocation harness. E5.3 (#30) added trace bundling: when
// --prompt-file and --bundle-out are supplied together, the runner
// invokes the agent, packs the captured events into the *.jsonl.gz
// wire format from ADR-007, and writes the bundle to disk. Signed
// upload to the backend lands in E5.6 (#32).
//
// E3.12 (#128) wired prompt construction: with --fetch-prompt and
// --stage-id, the runner pulls the constructed prompt from the
// backend before invoking the agent (writing it to a temp file
// the existing --prompt-file plumbing reads). --prompt-file still
// wins as a local override for replay / debug.
//
// With neither --prompt-file nor --fetch-prompt, the binary parses
// its inputs, prints a single startup log line, and exits 0 —
// preserving the dispatch-path probe used by early demo runs.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agent/claudecode"
	"github.com/kuhlman-labs/fishhawk/runner/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/runner/internal/constraint"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitdiff"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/otelemit"
	"github.com/kuhlman-labs/fishhawk/runner/internal/plan"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

const (
	exitOK          = 0
	exitFailure     = 1
	exitUsage       = 2
	exitVersionSkew = 3   // runner version is older than backend's min_runner_version
	exitCancelled   = 130 // 128 + SIGINT; matches the convention for terminate-by-signal exit codes.
)

// newRunnerContext returns the top-level context for the runner's
// long-running calls. Production wires signal.NotifyContext for
// SIGINT + SIGTERM so the MCP fishhawk_run_stage tool's
// cancellation chain (ADR-024 / #433) translates cleanly into a
// graceful runner exit (#435).
//
// Exposed as a var so tests can swap in a context they can cancel
// programmatically without raising signals at the test process.
var newRunnerContext = func() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// newInvoker is the seam tests use to swap the real Claude Code
// adapter for a fake. Production wiring is the only assignment in
// non-test code; tests reassign and restore it via t.Cleanup.
var newInvoker = func(apiKey string) agent.Invoker {
	return claudecode.New(apiKey)
}

// uploadClient is the test seam for the backend HTTP client.
// Tests substitute a fake to drive ShipTrace / IssueKey / FetchPrompt
// without standing up an httptest.Server.
type uploadClient interface {
	IssueKey(ctx context.Context, runID string, ttl time.Duration) (*upload.IssuedKey, error)
	ShipTrace(ctx context.Context, args upload.ShipArgs) (*upload.ShipResult, error)
	ShipPlan(ctx context.Context, args upload.ShipPlanArgs) (*upload.ShipPlanResult, error)
	ShipPullRequest(ctx context.Context, args upload.ShipPullRequestArgs) (*upload.ShipPullRequestResult, error)
	FetchPrompt(ctx context.Context, args upload.FetchPromptArgs) (*upload.FetchedPrompt, error)
	FetchInstallationToken(ctx context.Context, args upload.FetchInstallationTokenArgs) (*upload.FetchInstallationTokenResult, error)
	FetchMCPToken(ctx context.Context, args upload.FetchMCPTokenArgs) (*upload.FetchMCPTokenResult, error)
	RetryStage(ctx context.Context, args upload.RetryStageArgs) error
}

// newUploadClient returns the production uploadClient for the
// given backend URL. Overridable by tests.
var newUploadClient = func(baseURL string) uploadClient {
	return upload.New(baseURL)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// semverLT reports whether semver string a is strictly less than b.
// Both strings may have an optional "v" prefix. Returns false whenever
// either value is "dev" or cannot be parsed — degrades gracefully so
// local dev builds (both sides "dev") never trip the version-skew check.
func semverLT(a, b string) bool {
	ap := parseSemver(a)
	bp := parseSemver(b)
	if ap == nil || bp == nil {
		return false
	}
	for i := 0; i < 3; i++ {
		if ap[i] < bp[i] {
			return true
		}
		if ap[i] > bp[i] {
			return false
		}
	}
	return false
}

// parseSemver parses a semver string (with optional "v" prefix) into a
// [major, minor, patch] int slice. Returns nil when the string is "dev"
// or cannot be parsed (pre-release suffixes like "-alpha.1" are stripped).
func parseSemver(v string) []int {
	v = strings.TrimPrefix(v, "v")
	if v == "dev" || v == "" {
		return nil
	}
	parts := strings.SplitN(v, ".", 3)
	if len(parts) != 3 {
		return nil
	}
	result := make([]int, 3)
	for i, p := range parts {
		if idx := strings.IndexByte(p, '-'); idx >= 0 {
			p = p[:idx]
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		result[i] = n
	}
	return result
}

// run is split out so tests can drive it without exiting the test
// process. Returns the intended process exit code.
//
// logSink receives the structured startup log line and any failure
// notes; trace events (when the harness runs) go to os.Stdout so
// they can be piped or captured separately. This split lets a
// caller redirect stderr for diagnostics while keeping the trace
// stream clean.
//
// Signal handling (#435): newRunnerContext registers SIGINT +
// SIGTERM. When either fires before the body returns, the deferred
// cleanup emits a `runner_cancelled` log line and overrides the
// exit code to exitCancelled (130). The body itself receives ctx
// — long-running calls (Invoke, ShipTrace, FetchPrompt, etc.)
// terminate early via ctx.Done(), which is the cooperative half of
// the cleanup. The remaining trace bundle (whatever events were
// captured up to the cancellation point) still packs + ships best-
// effort if the body gets that far.
func run(args []string, logSink io.Writer) (exitCode int) {
	// version subcommand: emit JSON {version, plan_schema_hash} to stdout
	// and exit. Stdout (not logSink) because this is the command's primary
	// data output, not a log line — `fishhawk doctor` and operators read it
	// via `exec.Command().Output()` which captures stdout only.
	// Must be checked before parseFlags, which requires run-id / backend-url.
	if len(args) > 0 && args[0] == "version" {
		out, _ := json.Marshal(map[string]string{
			"version":          runnerVersion(),
			"plan_schema_hash": plan.EmbeddedSchemaHash(),
		})
		_, _ = fmt.Fprintf(os.Stdout, "%s\n", out)
		return exitOK
	}

	cfg, err := parseFlags(args, logSink)
	if err != nil {
		// parseFlags already wrote a usage / error message.
		return exitUsage
	}

	logStartup(logSink, cfg)
	_, _ = fmt.Fprintf(logSink, `{"event":"coercion_registry","summary":%q}`+"\n", plan.CoercionRegistrySummary())

	ctx, stop := newRunnerContext()
	defer stop()

	// GenAI observability (#649). Bootstrap is gated by
	// OTEL_EXPORTER_OTLP_ENDPOINT: a disabled (no-op) Emitter when
	// unset, so the local loop is unaffected. The deferred Shutdown
	// force-flushes buffered spans before this short-lived process
	// exits — without it the batch processor drops them. Shutdown runs
	// on a fresh background context because ctx may already be
	// cancelled (the SIGTERM path) by the time we exit.
	emitter, oerr := otelemit.Bootstrap(ctx)
	if oerr != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"otel_bootstrap_failed","detail":%q}`+"\n", oerr.Error())
		emitter = &otelemit.Emitter{}
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = emitter.Shutdown(flushCtx)
	}()

	defer func() {
		// Override whatever the body returned when ctx was the
		// cause of exit. Emit a structured terminator so the MCP
		// run_stage tool (and any other consumer parsing the
		// runner's stdout stream) sees a clean cancellation marker
		// regardless of where in the body the cancel landed.
		if ctx.Err() != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"runner_cancelled","run_id":%q,"stage_id":%q,"reason":%q}`+"\n",
				cfg.runID, cfg.stageID, ctx.Err().Error())
			exitCode = exitCancelled
		}
	}()

	// One uploadClient instance shared by fetch-prompt + upload-trace
	// paths. Constructed lazily so the scaffold-mode short-circuit
	// below doesn't pay for a client it never uses.
	var client uploadClient
	// One IssuedKey reused across fetch-prompt and upload-trace. The
	// signing-key endpoint is one-shot (409 on second call), so we
	// must issue exactly once per runner invocation.
	var issuedKey *upload.IssuedKey

	// stageType comes from the fetch-prompt response. Drives
	// per-stage post-processing: plan validation + upload for
	// "plan", commit/push/PR for "implement". Empty when --fetch-
	// prompt is unset (local replay) — both branches simply skip,
	// preserving the existing local-replay behavior.
	var stageType string

	// If --fetch-prompt is set and no --prompt-file was supplied,
	// pull the constructed prompt from the backend and write it to
	// a temp file. Sets cfg.promptFile so the rest of the path is
	// unchanged. --prompt-file always wins (local override for
	// replay / debug). If only --fetch-prompt is set with no
	// --stage-id, that's a config error.
	if cfg.fetchPrompt && cfg.promptFile == "" {
		if cfg.stageID == "" {
			_, _ = fmt.Fprintln(logSink,
				`{"event":"runner_failed","reason":"config","detail":"--fetch-prompt requires --stage-id"}`)
			return exitUsage
		}
		client = newUploadClient(cfg.backendURL)
		key, fetchErr := issueSigningKey(ctx, client, cfg, logSink)
		if fetchErr != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"runner_failed","reason":"issue_key","detail":%q}`+"\n", fetchErr.Error())
			return exitFailure
		}
		issuedKey = key
		path, sType, agentTimeoutSecs, specVerifyCmd, specVerifyTimeoutSecs, decomposedFromRunID, minRunnerVersion, agentSelfRetry, maxRetriesSnapshot, retryAttempt, scopeFiles, commitAuthorName, commitAuthorEmail, fetchErr := fetchPromptToFile(ctx, client, cfg, key, logSink)
		if fetchErr != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"runner_failed","reason":"fetch_prompt","detail":%q}`+"\n", fetchErr.Error())
			return exitFailure
		}
		// Version-skew check: if the backend requires a newer runner, exit
		// immediately rather than invoking the agent with potentially
		// incompatible protocol assumptions.
		if minRunnerVersion != "" && semverLT(runnerVersion(), minRunnerVersion) {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"version_skew_detected","runner_version":%q,"min_required":%q}`+"\n",
				runnerVersion(), minRunnerVersion)
			return exitVersionSkew
		}
		cfg.promptFile = path
		cfg.decomposedFromRunID = decomposedFromRunID
		cfg.scopeFiles = scopeFiles
		cfg.commitAuthorName = commitAuthorName
		cfg.commitAuthorEmail = commitAuthorEmail
		stageType = sType
		// Hand the resolved scope.files to the out-of-process CLI
		// auto-PR path (#581) the same way the PR description is
		// handed off via /tmp/fishhawk-pr.md. Only implement stages
		// carry a scope; a write failure is non-fatal — the CLI falls
		// back to `git add -A` when the file is missing.
		if sType == "implement" && len(scopeFiles) > 0 {
			writeScopeHandoff(scopeFiles, logSink)
		}
		// Server-resolved timeout wins when operator didn't pass --timeout explicitly.
		if cfg.timeout == 0 && agentTimeoutSecs > 0 {
			cfg.timeout = time.Duration(agentTimeoutSecs) * time.Second
		}
		// Operator flag wins; fall back to spec-resolved verify settings.
		if cfg.verifyCmd == "" && specVerifyCmd != "" {
			cfg.verifyCmd = specVerifyCmd
		}
		if cfg.verifyTimeout == 0 && specVerifyTimeoutSecs > 0 {
			cfg.verifyTimeout = time.Duration(specVerifyTimeoutSecs) * time.Second
		}
		// ADR-023 self-retry fields — set from prompt response, not CLI flags.
		cfg.agentSelfRetry = agentSelfRetry
		cfg.maxRetriesSnapshot = maxRetriesSnapshot
		cfg.retryAttempt = retryAttempt
	}

	if cfg.promptFile == "" {
		// Scaffold mode preserved: --prompt-file unset (and not
		// fetched) means "exercise the dispatch path; do not invoke
		// the agent."
		return exitOK
	}

	prompt, err := os.ReadFile(cfg.promptFile)
	if err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"runner_failed","reason":"read_prompt","detail":%q}`+"\n", err.Error())
		return exitUsage
	}

	// Apply the 15-minute fallback when no timeout was resolved by the
	// operator flag or the server-fetch path above. Covers the case where
	// --fetch-prompt was not used, or the server returned 0.
	if cfg.timeout == 0 {
		cfg.timeout = 15 * time.Minute
	}

	inv := agent.Invocation{
		RunID:      cfg.runID,
		Stage:      cfg.stage,
		Prompt:     string(prompt),
		WorkingDir: cfg.workingDir,
		Budget: agent.Budget{
			MaxTokens: cfg.maxTokens,
			Timeout:   cfg.timeout,
		},
		Env: map[string]string{},
		// Wire mid-stage progress heartbeats to logSink (the runner's
		// structured stderr stream, which the fishhawk-mcp run_stage
		// relay forwards as progress notifications). The agent adapter
		// emits a single-line stage_progress JSON heartbeat here every
		// ~15s during the agent invocation so a long stage is visibly
		// progressing rather than silent (#580). These never enter the
		// signed trace bundle. Reverting this one line fully disables
		// emission.
		ProgressSink: logSink,
	}

	// E19.8 / #348: mint a short-lived MCP token for the agent and
	// layer it onto the invocation env. Best-effort — if the token
	// fetch fails we log and continue. The agent loses Fishhawk
	// MCP awareness but the run still produces a valid trace /
	// plan / PR per the rest of the stage flow.
	if issuedKey != nil {
		mcpTok, err := client.FetchMCPToken(ctx, upload.FetchMCPTokenArgs{
			RunID:      cfg.runID,
			PrivateKey: issuedKey.PrivateKey,
		})
		if err != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"mcp_token_fetch_failed","run_id":%q,"detail":%q}`+"\n",
				cfg.runID, err.Error())
		} else {
			inv.Env["FISHHAWK_API_TOKEN"] = mcpTok.Token
			inv.Env["FISHHAWK_BACKEND_URL"] = cfg.backendURL
			_, _ = fmt.Fprintf(logSink,
				`{"event":"mcp_token_issued","run_id":%q,"token_id":%q,"expires_at":%q}`+"\n",
				cfg.runID, mcpTok.TokenID, mcpTok.ExpiresAt.Format(time.RFC3339))
		}
	}

	invoker := newInvoker(os.Getenv("ANTHROPIC_API_KEY"))

	// selfRetryBudget is the number of additional in-process retries the
	// runner may attempt before giving up. Clamped to 0 when negative
	// (retryAttempt >= maxRetriesSnapshot, e.g. because a prior GHA runner
	// already exhausted the budget at the orchestration layer).
	selfRetryBudget := cfg.maxRetriesSnapshot - cfg.retryAttempt
	if selfRetryBudget < 0 {
		selfRetryBudget = 0
	}
	selfRetryCount := 0

	var (
		res       agent.Result
		invokeErr error
		diff      *constraint.Diff
		diffErr   error
		// planValidationFailed records that res.OK was demoted
		// specifically by a LOCAL plan-validation failure — not an agent
		// category-A failure, a constraint violation, or a verify-gate
		// failure. It widens the plan-upload gate below so the
		// known-invalid plan is still POSTed, letting the backend's
		// handleShipPlan accept-and-reject path own the running->failed(B)
		// transition (#613).
		planValidationFailed bool
	)

	invokeStart := time.Now()
	for {
		res, invokeErr = invoker.Invoke(ctx, inv)

		// Plan validation runs only if the agent itself succeeded —
		// no point re-stating "your plan is malformed" when the
		// agent already failed. A plan-validation failure overrides
		// res.OK and demotes the run to category-B (constraint /
		// policy violation per MVP_SPEC §6).
		//
		// Gated on stageType to skip implement / review stages where
		// the agent legitimately produces no plan file. Empty
		// stageType (local replay without --fetch-prompt) preserves
		// the historical behavior: operator's --plan-out flag drives
		// validation directly.
		if res.OK && cfg.planOut != "" && stageType != "implement" && stageType != "review" {
			if ev, demote := validatePlan(cfg.planOut); demote != nil {
				res.Events = append(res.Events, ev)
				res.OK = false
				res.FailureCategory = "B"
				res.FailureReason = demote.Error()
				invokeErr = demote
				planValidationFailed = true
			} else {
				res.Events = append(res.Events, ev)
			}
		}

		// Diff emission: when --check-base-ref is set, compute the
		// stage's git diff and emit a git_diff event into the bundle.
		// The backend's policy re-evaluation (E3.13) reads this event
		// regardless of whether the runner does its own in-band
		// constraint check; decoupling emission from enforcement (#247)
		// means the SPA's policy section works even for customers who
		// don't pass --constraints-file.
		//
		// A diff failure on its own doesn't demote res.OK — when the
		// customer didn't pass constraints-file we have nothing to
		// enforce. The in-band constraint check below treats a failed
		// diff as fatal IF constraints-file is set (preserves the
		// pre-#247 "couldn't enforce constraints → category-B" semantic).
		diff = nil
		diffErr = nil
		if cfg.checkBaseRef != "" {
			d, evs, err := computeAndEmitDiff(cfg, logSink)
			res.Events = append(res.Events, evs...)
			if err == nil {
				diff = &d
			} else {
				diffErr = err
			}
		}

		// Constraint evaluation: same demotion rules as plan
		// validation. Only runs if everything before it succeeded so
		// we don't double-stamp a category-A failure as B.
		//
		// Requires both flags. --constraints-file alone is a silent
		// skip (legitimate for stages that don't produce diffs); the
		// customer can pass it as a default in their action and only
		// add --check-base-ref to stages that emit a diff. A diff
		// failure when both flags are set is fatal — preserves the
		// pre-#247 "couldn't enforce constraints → category-B"
		// semantic.
		if res.OK && cfg.constraintsFile != "" && cfg.checkBaseRef != "" {
			if diff == nil {
				res.OK = false
				res.FailureCategory = "B"
				res.FailureReason = diffErr.Error()
				invokeErr = diffErr
			} else if evs, demote := enforceConstraints(cfg, *diff); demote != nil {
				res.Events = append(res.Events, evs...)
				res.OK = false
				res.FailureCategory = "B"
				res.FailureReason = demote.Error()
				invokeErr = demote
			} else {
				res.Events = append(res.Events, evs...)
			}
		}

		// Verify gate: optional in-band test gate that fires after constraint
		// evaluation and before bundle building. Non-zero exit from the
		// verify command demotes to category-A (#441).
		if res.OK && cfg.verifyCmd != "" {
			ev, demote := runVerifyGate(ctx, cfg, logSink)
			res.Events = append(res.Events, ev)
			if demote != nil {
				res.OK = false
				res.FailureCategory = "A"
				res.FailureReason = demote.Error()
				invokeErr = demote
			}
		}

		// ADR-023 self-retry: when the stage fails with category A or C,
		// the spec opts in with agent_self_retry:true, and the budget is
		// not exhausted, call POST /v0/stages/{id}/retry and re-invoke.
		// issuedKey != nil guarantees client is non-nil (key comes from
		// the fetch-prompt path which always constructs the client first).
		if !res.OK &&
			(res.FailureCategory == "A" || res.FailureCategory == "C") &&
			cfg.agentSelfRetry && issuedKey != nil && selfRetryCount < selfRetryBudget {
			retryErr := client.RetryStage(ctx, upload.RetryStageArgs{
				StageID:    cfg.stageID,
				PrivateKey: issuedKey.PrivateKey,
			})
			if retryErr == nil {
				_, _ = fmt.Fprintf(logSink,
					`{"event":"stage_self_retry","run_id":%q,"stage_id":%q,"attempt":%d}`+"\n",
					cfg.runID, cfg.stageID, selfRetryCount+1)
				selfRetryCount++
				// Reset per-attempt state; the next iteration's agent
				// invocation re-populates res and invokeErr. diff and
				// diffErr are recomputed inside the loop body too, so
				// no explicit reset is needed.
				res = agent.Result{}
				invokeErr = nil
				continue
			}
			_, _ = fmt.Fprintf(logSink,
				`{"event":"retry_stage_failed","run_id":%q,"stage_id":%q,"detail":%q}`+"\n",
				cfg.runID, cfg.stageID, retryErr.Error())
		}
		break
	}

	// Emit the GenAI observability span for this stage as soon as the
	// agent invocation (and any self-retries) settled — before bundle
	// packing / upload, so a downstream upload failure doesn't lose
	// the model-call span. Token counts + model come from the
	// aggregated Result; the span carries an estimated cost via
	// pricing.Cost. No-op when OTel emission is gated off.
	emitter.EmitStage(ctx, otelemit.StageSpan{
		RunID:        cfg.runID,
		Stage:        cfg.stage,
		Model:        res.Model,
		InputTokens:  res.InputTokens,
		OutputTokens: res.OutputTokens,
		Latency:      time.Since(invokeStart),
		OK:           res.OK,
	})

	// Bundle building is shared by --bundle-out and --upload-trace.
	// We build raw + redacted variants once into memory, then write
	// to disk and/or upload. When neither is configured, fall back
	// to stdout JSONL so callers exercising --prompt-file alone can
	// still inspect.
	//
	// Both variants ship per stage (E2.4): the raw bundle stays
	// gated by S3 Object Lock for compliance; the redacted bundle
	// is what default-readable surfaces (the SPA transcript view
	// from #218) read. Identical event order, identical manifest
	// timestamps — the only difference is `redaction.RedactDefault`
	// applied to each event's payload + the manifest's
	// agent_failure_reason.
	//
	// Category-A is the only failure class the runner stamps in the
	// bundle manifest (E8.5): agent process failure. B is decided by
	// the backend's authoritative re-evaluation; C originates inside
	// the upload itself (no bundle to stamp); D never reaches the
	// runner.
	// willOpenPR is true when this stage will commit + push + open a PR
	// after the trace upload — a standalone implement stage that isn't a
	// --no-pr local run or a decomposed child. It stamps push_and_open_pr
	// in the bundle manifest so the backend forward-gates the implement
	// stage's terminal transition onto the /pull-request upload (#742), and
	// it gates the failure-report POST below so only a gated stage reports
	// a commit/push/PR-open failure back to the backend.
	willOpenPR := stageType == "implement" && !cfg.noPR && cfg.decomposedFromRunID == ""

	var rawBundle, redactedBundle []byte
	if cfg.bundleOut != "" || cfg.uploadTrace {
		agentFailed := res.FailureCategory == "A"
		agentFailureReason := ""
		if agentFailed {
			agentFailureReason = res.FailureReason
		}

		manifestRaw := bundle.PackInputs{
			RunID:   cfg.runID,
			StageID: bundleStageID(cfg),
			Agent:   "claude-code",
			// Carry the resolved model id + token split to the manifest
			// so the backend prices the run from this signed record
			// (authoritative cost), not from a runner-emitted span.
			Model:              res.Model,
			InputTokens:        res.InputTokens,
			OutputTokens:       res.OutputTokens,
			AgentFailed:        agentFailed,
			AgentFailureReason: agentFailureReason,
			PushAndOpenPR:      willOpenPR,
		}
		bytesData, _, err := bundle.PackBytes(manifestRaw, res.Events)
		if err != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"runner_failed","reason":"bundle_pack","detail":%q}`+"\n", err.Error())
			logCompletion(logSink, res, invokeErr)
			return exitFailure
		}
		rawBundle = bytesData

		// Redact at the source-event level so the redacted bundle's
		// trailer hash stays consistent with its events — pack
		// produces the trailer from whatever bytes it sees, so any
		// post-pack rewrite would invalidate it.
		redactedEvents, eventHits := redactEvents(res.Events)
		redactedReason, reasonHits := redactString(agentFailureReason)
		hits := mergeHits(eventHits, reasonHits)
		if len(hits) > 0 {
			hitsJSON, _ := json.Marshal(hits)
			_, _ = fmt.Fprintf(logSink,
				`{"event":"trace_redacted","run_id":%q,"stage_id":%q,"hits":%s}`+"\n",
				cfg.runID, cfg.stageID, hitsJSON)
		}
		manifestRedacted := manifestRaw
		manifestRedacted.AgentFailureReason = redactedReason
		redBytes, _, err := bundle.PackBytes(manifestRedacted, redactedEvents)
		if err != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"runner_failed","reason":"bundle_pack_redacted","detail":%q}`+"\n", err.Error())
			logCompletion(logSink, res, invokeErr)
			return exitFailure
		}
		redactedBundle = redBytes
	}

	if cfg.bundleOut != "" {
		// Write the redacted variant to --bundle-out: that's what
		// the dev/inspection flow wants by default. 0o600 because
		// even the redacted bundle can leak the *shape* of secrets
		// (event timing, lengths) and the runner's filesystem is
		// ephemeral but defense in depth is cheap.
		if err := os.WriteFile(cfg.bundleOut, redactedBundle, 0o600); err != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"runner_failed","reason":"bundle_write","detail":%q}`+"\n", err.Error())
			logCompletion(logSink, res, invokeErr)
			return exitFailure
		}
	}

	if cfg.uploadTrace {
		// Hoist signing-key issuance so trace + plan share the same
		// key. The /v0/runs/{id}/signing-key endpoint is one-shot per
		// run; without this, plan upload issues a second key and the
		// backend's idempotent-issuance check responds 409.
		if issuedKey == nil {
			if client == nil {
				client = newUploadClient(cfg.backendURL)
			}
			key, err := issueSigningKey(ctx, client, cfg, logSink)
			if err != nil {
				_, _ = fmt.Fprintf(logSink,
					`{"event":"runner_failed","reason":"issue_key","detail":%q}`+"\n", err.Error())
				if res.OK {
					res.OK = false
					res.FailureCategory = "C"
					res.FailureReason = err.Error()
					invokeErr = err
				}
				logCompletion(logSink, res, invokeErr)
				return exitFailure
			}
			issuedKey = key
		}

		// Ship raw first so the audit log records the unredacted
		// bundle's content_hash before the redacted one — the
		// raw row is the source of truth for compliance, and
		// downstream consumers that want the redacted variant can
		// follow the second audit entry.
		for _, v := range []traceVariant{
			{name: "raw", bytes: rawBundle},
			{name: "redacted", bytes: redactedBundle},
		} {
			if err := uploadTrace(ctx, cfg, v.name, v.bytes, logSink, client, issuedKey); err != nil {
				// Upload failures are MVP_SPEC §6 category-C (infra).
				// We DON'T overwrite an earlier A/B failure category;
				// only stamp C when the agent itself succeeded.
				if res.OK {
					res.OK = false
					res.FailureCategory = "C"
					res.FailureReason = err.Error()
					invokeErr = err
				}
				_, _ = fmt.Fprintf(logSink,
					`{"event":"runner_failed","reason":"trace_upload","variant":%q,"detail":%q}`+"\n",
					v.name, err.Error())
				logCompletion(logSink, res, invokeErr)
				return exitFailure
			}
		}

		// Plan upload follows trace upload so they share the signing
		// key and the audit log carries the trace event before the
		// plan_generated entry. Only fires when the agent succeeded
		// AND --plan-out was set (E5.X / #191) AND the stage actually
		// produced a plan (i.e. not implement / review). A failed
		// plan upload is category-C unless the backend rejected the
		// body as schema-invalid (ErrPlanInvalid) — that's category-B
		// since it's the agent's output that's bad, not the network.
		// planValidationFailed widens the gate so a locally-invalid plan
		// is still shipped (#613): the backend's handleShipPlan FailStage-B
		// path (#603) then transitions the stage to failed promptly with
		// the validation error in the audit trail, instead of leaving it
		// in `running` until the SLA watchdog reaps it. uploadPlan returns
		// ErrPlanInvalid in this case, so the success path is never reached
		// and the existing error handler maps it to category B below.
		if (res.OK || planValidationFailed) && cfg.planOut != "" && stageType != "implement" && stageType != "review" {
			if planValidationFailed {
				_, _ = fmt.Fprintf(logSink,
					`{"event":"plan_invalid_shipped","run_id":%q,"stage_id":%q}`+"\n",
					cfg.runID, cfg.stageID)
			}
			if err := uploadPlan(ctx, cfg, logSink, client, issuedKey); err != nil {
				res.OK = false
				if errors.Is(err, upload.ErrPlanInvalid) {
					res.FailureCategory = "B"
				} else {
					res.FailureCategory = "C"
				}
				res.FailureReason = err.Error()
				invokeErr = err
				_, _ = fmt.Fprintf(logSink,
					`{"event":"runner_failed","reason":"plan_upload","detail":%q}`+"\n", err.Error())
				logCompletion(logSink, res, invokeErr)
				return exitFailure
			}
		}

		// Implement-stage post-processing: commit + push + open PR
		// + ship the pull_request artifact. (E5.X / #195.) Mirrors
		// the plan-stage upload chain: same signing key, same
		// failure-classification rules. ErrPullRequestInvalid is
		// category-B (we shipped the wrong shape); everything else
		// is category-C (network, git, GitHub API).
		if res.OK && stageType == "implement" {
			if err := openPRAndShipArtifact(ctx, cfg, logSink, client, issuedKey); err != nil {
				res.OK = false
				// Wrong-shaped output → category-B (parks the run for
				// re-scope/re-plan). ErrPullRequestInvalid is the backend
				// rejecting the artifact shape; ErrCommitWouldNotCompile is
				// the pre-push compile gate (#728) catching a scope-only
				// commit that dropped build-required drift. Everything else
				// (network, git, GitHub API) is category-C infra.
				if errors.Is(err, upload.ErrPullRequestInvalid) || errors.Is(err, gitops.ErrCommitWouldNotCompile) {
					res.FailureCategory = "B"
				} else {
					res.FailureCategory = "C"
				}
				res.FailureReason = err.Error()
				invokeErr = err
				_, _ = fmt.Fprintf(logSink,
					`{"event":"runner_failed","reason":"pull_request_upload","detail":%q}`+"\n", err.Error())
				// Report the failure to /pull-request so the backend fails the
				// implement stage that its trace gate left in `running` (#742).
				// Without this the gated stage would hang until the SLA watchdog
				// reaps it. Gated on willOpenPR: only a push_and_open_pr stage
				// was left in running; a decomposed-child / --no-pr failure was
				// never gated, so reporting one would wrongly fail an
				// already-advanced stage. Best-effort — the failure exit stands
				// regardless of whether the report lands.
				if willOpenPR {
					reportPullRequestFailure(ctx, cfg, logSink, client, issuedKey, res.FailureCategory, err.Error())
				}
				logCompletion(logSink, res, invokeErr)
				return exitFailure
			}
		}
	}

	if rawBundle == nil {
		emitEvents(os.Stdout, res.Events)
	}

	logCompletion(logSink, res, invokeErr)

	if !res.OK {
		return exitFailure
	}
	return exitOK
}

// bundleStageID returns the value passed to bundle.PackBytes for
// the stage identifier. We prefer the UUID form (--stage-id) when
// supplied, otherwise fall back to the workflow-spec stage name
// (--stage). Bundling tolerates either; the upload path needs the
// UUID.
func bundleStageID(cfg config) string {
	if cfg.stageID != "" {
		return cfg.stageID
	}
	return cfg.stage
}

// uploadTrace ships the bundle bytes to the backend's trace
// endpoint. If `client` is nil, a fresh client is constructed; if
// `issued` is nil, a signing key is issued. Both are reused from
// the prompt-fetch path when --fetch-prompt is set, since the
// signing-key endpoint is one-shot per run.
//
// Returns nil on accepted, non-nil on any failure (key issuance,
// signing, network, signature rejection). The caller translates
// the error into the right category-C audit narrative.
// traceVariant pairs a variant name with the bundle bytes produced
// for that variant. Used to drive the per-variant ShipTrace loop.
type traceVariant struct {
	name  string
	bytes []byte
}

func uploadTrace(ctx context.Context, cfg config, variant string, bundleBytes []byte, logSink io.Writer, client uploadClient, issued *upload.IssuedKey) error {
	if cfg.stageID == "" {
		return errors.New("upload: --stage-id required with --upload-trace")
	}
	if client == nil {
		client = newUploadClient(cfg.backendURL)
	}

	if issued == nil {
		key, err := issueSigningKey(ctx, client, cfg, logSink)
		if err != nil {
			return fmt.Errorf("issue key: %w", err)
		}
		issued = key
	}

	res, err := client.ShipTrace(ctx, upload.ShipArgs{
		RunID:      cfg.runID,
		StageID:    cfg.stageID,
		Variant:    variant,
		Bundle:     bundleBytes,
		PrivateKey: issued.PrivateKey,
	})
	if err != nil {
		return fmt.Errorf("ship trace (%s): %w", variant, err)
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"trace_uploaded","run_id":%q,"stage_id":%q,"variant":%q,"content_hash":%q}`+"\n",
		res.RunID, res.StageID, res.Variant, res.ContentHash,
	)
	return nil
}

// uploadPlan ships the validated plan-out file to /v0/runs/{run_id}/plan.
// Reuses the signing key issued earlier in the run (during prompt
// fetch or by uploadTrace). Returns nil on 201/200.
//
// The plan is read fresh from disk rather than handed in; the
// upstream call site already validated the file and we want one
// canonical source of bytes for the signature.
func uploadPlan(ctx context.Context, cfg config, logSink io.Writer, client uploadClient, issued *upload.IssuedKey) error {
	if cfg.stageID == "" {
		return errors.New("upload: --stage-id required with --plan-out + --upload-trace")
	}
	if client == nil {
		client = newUploadClient(cfg.backendURL)
	}

	if issued == nil {
		// One-shot per run; if neither --fetch-prompt nor uploadTrace
		// have run, this branch is the issuer.
		key, err := issueSigningKey(ctx, client, cfg, logSink)
		if err != nil {
			return fmt.Errorf("issue key: %w", err)
		}
		issued = key
	}

	planBytes, err := os.ReadFile(cfg.planOut)
	if err != nil {
		return fmt.Errorf("read plan: %w", err)
	}

	res, err := client.ShipPlan(ctx, upload.ShipPlanArgs{
		RunID:      cfg.runID,
		StageID:    cfg.stageID,
		Plan:       planBytes,
		PrivateKey: issued.PrivateKey,
	})
	if err != nil {
		return fmt.Errorf("ship plan: %w", err)
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"plan_uploaded","run_id":%q,"stage_id":%q,"artifact_id":%q,"content_hash":%q,"idempotent":%t}`+"\n",
		cfg.runID, res.StageID, res.ID, res.ContentHash, res.Idempotent,
	)
	return nil
}

// issueSigningKey issues a per-run Ed25519 keypair via the backend.
// Logs the issuance to logSink so the trace timeline shows when the
// key was minted.
func issueSigningKey(ctx context.Context, client uploadClient, cfg config, logSink io.Writer) (*upload.IssuedKey, error) {
	issued, err := client.IssueKey(ctx, cfg.runID, 0)
	if err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"signing_key_issued","run_id":%q,"expires_at":%q}`+"\n",
		issued.RunID, issued.ExpiresAt.Format(time.RFC3339),
	)
	return issued, nil
}

// fetchPromptToFile pulls the constructed prompt from the backend,
// writes it to a temp file, and returns the path, stage type,
// agent_timeout_seconds, verify_command, verify_timeout_seconds,
// decomposed_from_run_id, min_runner_version, agent_self_retry,
// max_retries_snapshot, and retry_attempt from the response.
// stageType drives per-stage post-processing (plan validation + upload
// for plan stages, commit+push+PR upload for implement stages).
// agentTimeoutSecs is the spec-resolved wall-clock cap; 0 means the
// server didn't resolve one and the caller should apply the local
// 15-minute fallback. verifyCmd and verifyTimeoutSecs are the
// spec-resolved verify gate settings; both zero/empty when the spec
// declares none. decomposedFromRunID is non-empty when this run is a
// decomposed child. minRunnerVersion is non-empty when the backend
// requires a minimum runner version; the caller checks it against
// runnerVersion() before proceeding. agentSelfRetry, maxRetriesSnapshot,
// and retryAttempt drive the ADR-023 self-retry loop. commitAuthorName and
// commitAuthorEmail are the backend-resolved App bot commit identity (#722);
// both empty when the backend couldn't resolve it and the caller keeps the
// gitops default bot identity.
// The temp file is 0o600 — bundle-style defense in depth, since prompts
// may include issue bodies that the customer would prefer not to leave on
// the runner's filesystem world-readable.
func fetchPromptToFile(ctx context.Context, client uploadClient, cfg config, key *upload.IssuedKey, logSink io.Writer) (path string, stageType string, agentTimeoutSecs int, verifyCmd string, verifyTimeoutSecs int, decomposedFromRunID string, minRunnerVersion string, agentSelfRetry bool, maxRetriesSnapshot int, retryAttempt int, scopeFiles []upload.ScopeFile, commitAuthorName string, commitAuthorEmail string, err error) {
	got, fetchErr := client.FetchPrompt(ctx, upload.FetchPromptArgs{
		StageID:    cfg.stageID,
		PrivateKey: key.PrivateKey,
	})
	if fetchErr != nil {
		return "", "", 0, "", 0, "", "", false, 0, 0, nil, "", "", fetchErr
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"prompt_fetched","stage_id":%q,"stage_type":%q,"prompt_hash":%q,"prompt_bytes":%d}`+"\n",
		got.StageID, got.StageType, got.PromptHash, len(got.Prompt),
	)
	tmp, tmpErr := os.CreateTemp("", "fishhawk-prompt-*.txt")
	if tmpErr != nil {
		return "", "", 0, "", 0, "", "", false, 0, 0, nil, "", "", fmt.Errorf("create prompt temp file: %w", tmpErr)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		_ = tmp.Close()
		return "", "", 0, "", 0, "", "", false, 0, 0, nil, "", "", fmt.Errorf("chmod prompt temp file: %w", err)
	}
	if _, err := tmp.WriteString(got.Prompt); err != nil {
		_ = tmp.Close()
		return "", "", 0, "", 0, "", "", false, 0, 0, nil, "", "", fmt.Errorf("write prompt temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", "", 0, "", 0, "", "", false, 0, 0, nil, "", "", fmt.Errorf("close prompt temp file: %w", err)
	}
	return tmp.Name(), got.StageType, got.AgentTimeoutSeconds, got.VerifyCommand, got.VerifyTimeoutSeconds, got.DecomposedFromRunID, got.MinRunnerVersion, got.AgentSelfRetry, got.MaxRetriesSnapshot, got.RetryAttempt, got.ScopeFiles, got.CommitAuthorName, got.CommitAuthorEmail, nil
}

func logStartup(w io.Writer, cfg config) {
	_, _ = fmt.Fprintf(w,
		`{"event":"runner_started","run_id":%q,"workflow":%q,"stage":%q,"backend_url":%q,"version":%q,"prompt_file":%q}`+"\n",
		cfg.runID, cfg.workflow, cfg.stage, cfg.backendURL, runnerVersion(), cfg.promptFile,
	)
}

// logCompletion writes a single structured line summarizing the
// invocation outcome. Mirrors the failure categories from
// MVP_SPEC §6 so log scrapers can switch on `category`.
func logCompletion(w io.Writer, res agent.Result, err error) {
	if res.OK {
		_, _ = fmt.Fprintf(w,
			`{"event":"runner_completed","outcome":"ok","tokens_used":%d}`+"\n",
			res.TokensUsed)
		return
	}
	reason := res.FailureReason
	if reason == "" && err != nil {
		reason = err.Error()
	}
	category := res.FailureCategory
	if category == "" {
		category = "A"
	}
	_, _ = fmt.Fprintf(w,
		`{"event":"runner_completed","outcome":"failed","category":%q,"reason":%q,"tokens_used":%d,"err_class":%q}`+"\n",
		category, reason, res.TokensUsed, classifyErr(err))
}

// classifyErr returns a stable short label for the wrapped agent
// error so log consumers don't have to substring-match prose.
func classifyErr(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, agent.ErrTimeout):
		return "timeout"
	case errors.Is(err, agent.ErrBudgetExceeded):
		return "budget_exceeded"
	case errors.Is(err, agent.ErrBinaryNotFound):
		return "binary_not_found"
	case errors.Is(err, agent.ErrAgentThinkingBlock):
		return "agent_api_thinking_block"
	case errors.Is(err, agent.ErrLoopDetected):
		return "loop_detected"
	case errors.Is(err, agent.ErrAgentFailed):
		return "agent_failed"
	default:
		return "other"
	}
}

// computeAndEmitDiff runs `git diff --cached --name-status` against
// the configured base ref and returns a `git_diff` bundle event the
// backend's policy re-evaluation reads (E3.13). Decoupled from
// constraint enforcement (#247) so the diff lands in the bundle
// even when the customer doesn't pass --constraints-file — the
// SPA's policy section needs the diff to render anything other
// than "pending."
//
// Staging before the diff makes fresh files the agent created (test
// fixtures, new packages) show up under `git diff --cached`. Pre-#296
// the diff ran against `<base>...HEAD` and saw nothing because the
// agent's edits hadn't been committed yet; every PR silently failed
// `tests_added_or_updated` and friends at the backend's policy
// re-evaluation step.
//
// Staging is scope-bounded (#581) when the approved plan declared a
// scope.files set: exactly those paths are staged and any
// dirty-but-undeclared files are excluded and flagged as drift, so the
// policy_evaluated diff sees the identical scoped index the eventual
// commit will. When no scope is available (plan_missing_for_implement)
// it falls back to `git add -A`.
//
// Returns the parsed Diff (consumed by enforceConstraints when
// constraints-file is also set), one or more bundle events (the
// git_diff payload on success plus an optional scope_drift
// policy_event, or a policy_event marking the failure on error), and
// the underlying error for the caller's log line. The error is
// intentionally NOT load-bearing on the run's res.OK; the in-band
// constraint enforcer below is the one that demotes to category-B.
// resolvePolicyBaseRef returns the git ref the implement-stage policy
// diff (max_files_changed + forbidden/allowed file-list constraints)
// should be measured against.
//
// For standalone runs and the FIRST child of a decomposition the answer
// is cfg.checkBaseRef (main): a standalone run's increment IS the diff,
// and the first child's HEAD == main so its increment equals the
// cumulative diff too. For a SUBSEQUENT decomposition child the working
// tree already sits on the shared-branch tip carrying every prior
// child's commit, so diffing against main sums the whole fan-out and
// wedges a legitimately-decomposed feature against its own per-child cap
// (#765). In that case we measure against origin/<shared-branch> so each
// child's constraints bound only its own increment.
//
// Fallback-safe by construction: anything that isn't a subsequent
// decomposition child — including a subsequent child whose
// remote-tracking ref is unexpectedly absent — returns cfg.checkBaseRef
// unchanged, preserving the prior behavior exactly.
func resolvePolicyBaseRef(ctx context.Context, cfg config, logSink io.Writer) string {
	if cfg.decomposedFromRunID == "" {
		return cfg.checkBaseRef
	}
	// Same shared-branch derivation and predicate the upload-phase
	// isSubsequent routing uses (see the branch-routing block).
	sharedBranch := "fishhawk/run-" + shortID(cfg.decomposedFromRunID)
	repoDir := cfg.workingDir
	if repoDir == "" {
		repoDir = "."
	}
	if !remoteBranchExists(ctx, repoDir, sharedBranch) {
		// First child: shared branch not yet on the remote, HEAD == main.
		return cfg.checkBaseRef
	}
	baseRef := "origin/" + sharedBranch
	_, _ = fmt.Fprintf(logSink,
		`{"event":"policy_base_decomposition_child","stage_id":%q,"shared_branch":%q,"base_ref":%q}`+"\n",
		cfg.stageID, sharedBranch, baseRef)
	return baseRef
}

func computeAndEmitDiff(cfg config, logSink io.Writer) (constraint.Diff, []agent.Event, error) {
	repoDir := cfg.workingDir
	if repoDir == "" {
		repoDir = "."
	}

	// Resolve the effective policy-diff base ref once. For a subsequent
	// decomposition child this is origin/<shared-branch> so each child's
	// constraints bound its own increment, not the cumulative fan-out
	// (#765); for everything else it is cfg.checkBaseRef unchanged.
	baseRef := resolvePolicyBaseRef(context.Background(), cfg, logSink)

	var events []agent.Event

	if paths := scopePaths(cfg.scopeFiles); len(paths) > 0 {
		// Scope-bounded staging: same primitive CommitAndPush uses, so
		// the diff and the commit agree on which paths are in scope.
		drift, err := (&gitops.Pusher{}).StageScoped(context.Background(), repoDir, paths)
		if err != nil {
			return constraint.Diff{}, []agent.Event{{
				Kind: "policy_event",
				Payload: agent.MakePayload(map[string]string{
					"check": "diff", "outcome": "stage_failed",
					"error": err.Error(),
				}),
			}}, fmt.Errorf("computeAndEmitDiff: stage scoped: %w", err)
		}
		if len(drift) > 0 {
			driftJSON, _ := json.Marshal(drift)
			_, _ = fmt.Fprintf(logSink,
				`{"event":"scope_drift","stage_id":%q,"undeclared":%s}`+"\n",
				cfg.stageID, driftJSON)
			events = append(events, agent.Event{
				Kind: "policy_event",
				Payload: agent.MakePayload(map[string]any{
					"check":      "scope_drift",
					"outcome":    "excluded",
					"undeclared": drift,
				}),
			})
		}
	} else {
		// Fallback: stage everything the agent touched. add -A respects
		// .gitignore, idempotent on a clean repo, and doesn't fail when
		// there's nothing to stage. A failure here means we can't
		// reliably compute the diff — surface as a stage_failed
		// policy_event same as a git diff failure.
		addCmd := exec.CommandContext(context.Background(), "git", "add", "-A")
		addCmd.Dir = repoDir
		if out, err := addCmd.CombinedOutput(); err != nil {
			return constraint.Diff{}, []agent.Event{{
				Kind: "policy_event",
				Payload: agent.MakePayload(map[string]string{
					"check": "diff", "outcome": "stage_failed",
					"error": fmt.Sprintf("git add -A: %v: %s", err, strings.TrimSpace(string(out))),
				}),
			}}, fmt.Errorf("computeAndEmitDiff: stage: %w", err)
		}
	}

	runner := &gitdiff.Runner{}
	d, err := runner.Run(context.Background(), baseRef, repoDir)
	if err != nil {
		events = append(events, agent.Event{
			Kind:    "policy_event",
			Payload: agent.MakePayload(map[string]string{"check": "diff", "outcome": "diff_failed", "error": err.Error()}),
		})
		return constraint.Diff{}, events, fmt.Errorf("computeAndEmitDiff: %w", err)
	}

	// Capture the full unified-diff hunk text for content-level
	// implement-review (#585). This is additive trace payload; a
	// failure here degrades gracefully — we emit the git_diff event
	// WITHOUT a patch rather than failing the whole diff, because the
	// name-status list (d, above) is the load-bearing policy input and
	// is already in hand. The reviewer prompt falls back to its
	// file-list rendering when the patch is absent.
	patch, truncated, perr := runner.RunPatch(context.Background(), baseRef, repoDir)
	if perr != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"git_diff_patch_failed","base_ref":%q,"detail":%q}`+"\n",
			baseRef, perr.Error())
		patch, truncated = "", false
	}
	events = append(events, makeGitDiffEvent(baseRef, d, patch, truncated))
	return d, events, nil
}

// enforceConstraints reads the constraints config and evaluates
// it against the pre-computed diff. Returns one or more policy_event
// events for the bundle plus a non-nil error iff any constraint
// was violated. The caller demotes the run to category-B on a
// non-nil error.
//
// On infra failures (config file read / parse error) we still
// demote to category-B because from the workflow author's
// perspective "the runner couldn't verify your constraints" is a
// constraint-stage failure, not an agent failure.
//
// The git_diff event is emitted by computeAndEmitDiff before this
// runs (see #247); this function returns only policy_event entries.
func enforceConstraints(cfg config, d constraint.Diff) ([]agent.Event, error) {
	raw, err := os.ReadFile(cfg.constraintsFile)
	if err != nil {
		return []agent.Event{{
			Kind:    "policy_event",
			Payload: agent.MakePayload(map[string]string{"check": "constraints", "outcome": "config_unreadable", "error": err.Error()}),
		}}, fmt.Errorf("constraints: read %s: %w", cfg.constraintsFile, err)
	}
	var c constraint.Constraints
	if err := json.Unmarshal(raw, &c); err != nil {
		return []agent.Event{{
			Kind:    "policy_event",
			Payload: agent.MakePayload(map[string]string{"check": "constraints", "outcome": "config_invalid", "error": err.Error()}),
		}}, fmt.Errorf("constraints: parse %s: %w", cfg.constraintsFile, err)
	}

	violations := constraint.Evaluate(d, c)
	if len(violations) == 0 {
		return []agent.Event{
			{
				Kind: "policy_event",
				Payload: agent.MakePayload(map[string]any{
					"check":         "constraints",
					"outcome":       "valid",
					"files_checked": len(d.ChangedFiles),
				}),
			},
		}, nil
	}

	// One policy_event per violation keeps the bundle structured —
	// audit consumers can group / filter by Constraint without
	// parsing free-text.
	var evs []agent.Event
	var summary []string
	for _, v := range violations {
		evs = append(evs, agent.Event{
			Kind: "policy_event",
			Payload: agent.MakePayload(map[string]any{
				"check":      "constraints",
				"outcome":    "violation",
				"constraint": v.Constraint,
				"detail":     v.Detail,
				"files":      v.Files,
			}),
		})
		summary = append(summary, v.String())
	}
	return evs, fmt.Errorf("constraint violations: %s", strings.Join(summary, "; "))
}

// gitDiffFile mirrors constraint.ChangedFile but with json tags
// pinned for the bundle's wire format. The backend's reader
// decodes into the same shape.
type gitDiffFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// gitDiffPayload is the body of a git_diff event in the bundle.
// `kind` lets a single bundle support multiple diff variants in
// the future (e.g. base-vs-head, stage-vs-stage); for v0 it's
// always "name_status" — the parsed `git diff --name-status`
// output.
//
// Patch carries the full unified-diff hunk text for content-level
// implement-review (#585). It is additive and `omitempty`: older
// bundles (and patch-compute failures) omit it, and the backend
// reader decodes the absence to an empty Patch. PatchTruncated marks
// a patch that hit the maxPatchBytes cap. The `patch` / `patch_truncated`
// json tags MUST stay identical to backend/internal/bundle/bundle.go's
// gitDiffPayload mirror — this is the runner↔backend wire contract,
// not a JSON Schema, so the two sides agree field-by-field in lockstep.
type gitDiffPayload struct {
	Kind           string        `json:"kind"`
	BaseRef        string        `json:"base_ref"`
	Files          []gitDiffFile `json:"files"`
	NumFiles       int           `json:"num_files"`
	Patch          string        `json:"patch,omitempty"`
	PatchTruncated bool          `json:"patch_truncated,omitempty"`
}

// makeGitDiffEvent converts a constraint.Diff into the bundle event
// the backend's policy re-evaluation reads. Kind is "git_diff";
// payload schema is gitDiffPayload (above). patch is the unified-diff
// hunk text (empty when capture failed); truncated marks a capped
// patch.
func makeGitDiffEvent(baseRef string, d constraint.Diff, patch string, truncated bool) agent.Event {
	files := make([]gitDiffFile, 0, len(d.ChangedFiles))
	for _, f := range d.ChangedFiles {
		files = append(files, gitDiffFile{Path: f.Path, Status: string(f.Status)})
	}
	return agent.Event{
		Kind: "git_diff",
		Payload: agent.MakePayload(gitDiffPayload{
			Kind:           "name_status",
			BaseRef:        baseRef,
			Files:          files,
			NumFiles:       len(files),
			Patch:          patch,
			PatchTruncated: truncated,
		}),
	}
}

// validatePlan reads the plan artifact at path and validates it
// against the standard_v1 schema. The first return is a policy_event
// suitable for the trace bundle: kind=policy_event, payload describes
// the validation outcome. The second return is non-nil ONLY on
// validation failure — it carries the reason for callers wiring up
// category-B failure handling per MVP_SPEC §6.
func validatePlan(path string) (agent.Event, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return agent.Event{
			Kind:    "policy_event",
			Payload: agent.MakePayload(map[string]string{"check": "plan_validation", "outcome": "missing", "path": path, "error": err.Error()}),
		}, fmt.Errorf("plan: read %s: %w", path, err)
	}

	// Attempt structural coercion (#537) for the known string-elision
	// failure class — when the agent emits a bare string where the schema
	// requires an object at /generated_by, /scope/files[], or
	// /decomposition/sub_plans[]. TryCoerce returns the coerced bytes when
	// it could fix the plan; the file is rewritten so the subsequent
	// uploadPlan reads the corrected payload and the backend sees a clean
	// (already-validated) artifact. The backend's mirror coercion is a
	// belt-and-suspenders fallback for cases where this runner skips coercion
	// (older binary, future code path).
	coercedBytes, coercions, coerceErr := plan.TryCoerce(data, time.Now().UTC())
	if coerceErr == nil && len(coercions) > 0 {
		if err := os.WriteFile(path, coercedBytes, 0o600); err != nil {
			return agent.Event{
				Kind:    "policy_event",
				Payload: agent.MakePayload(map[string]string{"check": "plan_validation", "outcome": "coerce_write_failed", "path": path, "error": err.Error()}),
			}, fmt.Errorf("plan: write coerced %s: %w", path, err)
		}
		data = coercedBytes
	} else if coerceErr != nil && len(coercions) > 0 {
		// Partial coercion: some fields were fixed but the plan is still
		// invalid. Rewrite the file with the partially-fixed bytes so the
		// subsequent Validate reports the remaining violation rather than
		// the original error (which may name a field already coerced).
		if err := os.WriteFile(path, coercedBytes, 0o600); err != nil {
			return agent.Event{
				Kind:    "policy_event",
				Payload: agent.MakePayload(map[string]string{"check": "plan_validation", "outcome": "coerce_write_failed", "path": path, "error": err.Error()}),
			}, fmt.Errorf("plan: write coerced %s: %w", path, err)
		}
		data = coercedBytes
	}

	if vErr := plan.Validate(data); vErr != nil {
		invalidPayload := map[string]string{"check": "plan_validation", "outcome": "invalid", "path": path, "error": vErr.Error()}
		if len(coercions) > 0 {
			invalidPayload["coerced"] = fmt.Sprintf("%d field(s)", len(coercions))
		}
		return agent.Event{
			Kind:    "policy_event",
			Payload: agent.MakePayload(invalidPayload),
		}, vErr
	}

	outcomePayload := map[string]string{"check": "plan_validation", "outcome": "valid", "path": path}
	if len(coercions) > 0 {
		outcomePayload["coerced"] = fmt.Sprintf("%d field(s)", len(coercions))
	}
	return agent.Event{
		Kind:    "policy_event",
		Payload: agent.MakePayload(outcomePayload),
	}, nil
}

// runVerifyGate runs the --verify-cmd shell command as an in-band test
// gate after the agent exits cleanly. It captures combined output,
// emits a verify_run event, and returns a non-nil error (which the
// caller uses to demote the run to category-A) when the command exits
// non-zero.
//
// The command runs via "sh -c <verifyCmd>" so the flag accepts any
// shell expression. Setpgid=true places the child in a new process
// group; cmd.Cancel kills the whole group on context cancellation so
// grandchildren (e.g. go test subprocesses) don't keep the output
// pipe open and block CombinedOutput.
func runVerifyGate(ctx context.Context, cfg config, _ io.Writer) (agent.Event, error) {
	timeout := cfg.verifyTimeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	childCtx, childCancel := context.WithTimeout(ctx, timeout)
	defer childCancel()

	cmd := exec.CommandContext(childCtx, "sh", "-c", cfg.verifyCmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}

	output, cmdErr := cmd.CombinedOutput()

	exitCode := 0
	if cmdErr != nil {
		var exitErr *exec.ExitError
		if errors.As(cmdErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	outcome := "passed"
	var gateErr error
	if exitCode != 0 {
		outcome = "failed"
		gateErr = fmt.Errorf("verify gate failed: %s exited %d", cfg.verifyCmd, exitCode)
	}

	ev := agent.Event{
		Kind: "verify_run",
		Payload: agent.MakePayload(map[string]any{
			"command":   cfg.verifyCmd,
			"exit_code": exitCode,
			"output":    string(output),
			"outcome":   outcome,
		}),
	}
	return ev, gateErr
}

// emitEvents writes one JSON object per line. This is the
// placeholder transport — E5.3 / #30 replaced it with the
// JSONL.gz bundle format when --bundle-out is set; E5.6 / #32
// adds the signed upload.
func emitEvents(w io.Writer, events []agent.Event) {
	enc := json.NewEncoder(w)
	for _, ev := range events {
		_ = enc.Encode(ev)
	}
}

// pusher abstracts gitops.Pusher for tests.
type pusher interface {
	CommitAndPush(ctx context.Context, args gitops.CommitAndPushArgs) (*gitops.CommitAndPushResult, error)
}

// prOpener abstracts gitops.OpenPRClient for tests. Both seams
// are package-level vars so tests can swap the production
// implementations without changing every call site.
type prOpener interface {
	OpenPR(ctx context.Context, args gitops.OpenPRArgs) (*gitops.OpenPRResult, error)
}

// newPusher / newPROpener are test seams. Production code
// returns the real gitops types; tests substitute fakes via
// withFakeGitOps().
var (
	newPusher = func() pusher { return &gitops.Pusher{} }

	newPROpener = func(token string) prOpener {
		return &gitops.OpenPRClient{Token: token}
	}

	// remoteBranchExists reports whether the named branch exists on the
	// remote. Used to distinguish first vs. subsequent decomposed-child
	// runs for shared-branch routing. Test seam: production runs
	// git show-ref; tests swap in a function that reads from a map.
	remoteBranchExists = func(ctx context.Context, repoDir, branch string) bool {
		// show-ref exits 0 when the ref exists, non-zero otherwise.
		cmd := exec.CommandContext(ctx, "git", "show-ref", "--verify", "refs/remotes/origin/"+branch)
		cmd.Dir = repoDir
		return cmd.Run() == nil
	}

	// ghAuthToken sources a GitHub token from the operator's local `gh`
	// CLI, used as the push + PR fallback when the run has no attributed
	// App installation (#713). `gh auth token` prints the authenticated
	// user's token to stdout and exits 0 when logged in, non-zero when
	// gh is absent or not authenticated. Test seam: production shells
	// out; tests swap in a function that returns a canned token or error.
	ghAuthToken = func(ctx context.Context) (string, error) {
		out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(out)), nil
	}
)

// compileDiagnosticMarkers are substrings that positively identify a
// Go compile / typecheck diagnostic in `go vet` output, as opposed to a
// dependency-resolution failure (cold cache / no network) or a pure vet
// analyzer finding. The gate blocks ONLY when one of these appears — a
// positive allowlist, not a blanket "vet exited non-zero → block" — so a
// dep-fetch failure or an unrelated vet warning degrades to a
// non-blocking skip rather than killing every implement stage (#728,
// reviewer concern 2). The first three are the markers the reviewer
// named explicitly; the rest cover the same typecheck-error class.
var compileDiagnosticMarkers = []string{
	"does not implement",
	"cannot use",
	"undefined:",
	"missing method",
	"undeclared name",
	"redeclared",
	"not enough arguments",
	"too many arguments",
}

// looksLikeCompileError reports whether `go vet` output contains a
// genuine compile / typecheck diagnostic. Used to separate a real
// build failure (block, category-B) from a dependency-resolution
// failure or vet-analyzer finding (skip, non-blocking).
func looksLikeCompileError(output string) bool {
	for _, m := range compileDiagnosticMarkers {
		if strings.Contains(output, m) {
			return true
		}
	}
	return false
}

// verifyCommittedTreeCompiles compile-gates the scope-only committed
// tree before the runner pushes it (#728). When a scope-bounded commit
// drops build-required drift (e.g. an interface-conformance stub left in
// a test file the plan didn't declare), the committed tree fails to
// build even though each per-layer change looked complete — and the
// runner would otherwise open a non-compiling PR. This checks out the
// committed HEAD SHA in an isolated git worktree (NOT the operator's
// working tree, which still carries the drift on disk) and runs
// `go vet ./...` per go.work module. `go vet` typechecks test files,
// which `go build` skips, so it catches `does not implement <iface>
// (missing method ...)` that `go build ./...` would miss.
//
// It returns ErrCommitWouldNotCompile (wrapped, naming the drift files)
// ONLY on a genuine compile failure. Every infrastructure problem — no
// drift to drop, no go.work (non-Go repo), no Go toolchain, a worktree
// or `go work edit` failure, or a dependency-resolution failure — is a
// NON-blocking skip (logged as compile_gate_skipped, returns nil), so
// the gate never becomes a new failure source in non-Go or misconfigured
// environments. It degrades to today's "open the PR" behavior instead.
func verifyCommittedTreeCompiles(ctx context.Context, repoDir, headSHA string, drift []string, logSink io.Writer) error {
	// Fast paths (zero runtime on the common case): no excluded files
	// means nothing build-required could have been dropped; no go.work at
	// the repo root means this isn't a Go workspace to vet.
	if len(drift) == 0 {
		return nil
	}
	if _, err := os.Stat(filepath.Join(repoDir, "go.work")); err != nil {
		return nil
	}

	skip := func(reason, detail string) {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"compile_gate_skipped","head_sha":%q,"reason":%q,"detail":%q}`+"\n",
			headSHA, reason, detail)
	}

	// Isolated worktree at the committed SHA. os.MkdirTemp makes the
	// parent; the worktree target is a not-yet-existing child because
	// `git worktree add` refuses a populated path. Both are torn down on
	// return (worktree remove unregisters it from the source repo; the
	// RemoveAll is belt-and-suspenders for the parent).
	parent, err := os.MkdirTemp("", "fishhawk-compilegate-*")
	if err != nil {
		skip("worktree_tmp", err.Error())
		return nil
	}
	wt := filepath.Join(parent, "tree")
	defer func() {
		_ = exec.CommandContext(ctx, "git", "-C", repoDir, "worktree", "remove", "--force", wt).Run()
		_ = os.RemoveAll(parent)
	}()

	if out, err := exec.CommandContext(ctx, "git", "-C", repoDir,
		"worktree", "add", "--detach", wt, headSHA).CombinedOutput(); err != nil {
		// A worktree-add failure is itself an *exec.ExitError but is NOT a
		// compile failure (reviewer concern 1) — treat as infra-skip.
		skip("worktree_add", strings.TrimSpace(string(out)))
		return nil
	}

	// Enumerate the workspace modules from the committed go.work.
	workCmd := exec.CommandContext(ctx, "go", "work", "edit", "-json")
	workCmd.Dir = wt
	workOut, err := workCmd.Output()
	if err != nil {
		// `go` missing (exec start error) or `go work edit` failure — infra,
		// not a compile failure (reviewer concern 1). Skip.
		skip("go_work_edit", err.Error())
		return nil
	}
	var workspace struct {
		Use []struct {
			DiskPath string `json:"DiskPath"`
		} `json:"Use"`
	}
	if err := json.Unmarshal(workOut, &workspace); err != nil {
		skip("go_work_parse", err.Error())
		return nil
	}

	// Per-module `go vet ./...`. Running from the repo root fails (no root
	// go.mod in this multi-module workspace), so iterate the modules the
	// same way the CLAUDE.md coverage loop does.
	for _, m := range workspace.Use {
		vetCmd := exec.CommandContext(ctx, "go", "vet", "./...")
		vetCmd.Dir = filepath.Join(wt, m.DiskPath)
		out, verr := vetCmd.CombinedOutput()
		if verr == nil {
			continue
		}
		var exitErr *exec.ExitError
		if !errors.As(verr, &exitErr) {
			// vet never ran (go missing / exec start failure) — infra-skip.
			skip("vet_exec", verr.Error())
			return nil
		}
		// vet ran and exited non-zero. Block ONLY on a genuine compile /
		// typecheck diagnostic; a dependency-resolution failure (cold cache
		// / no network) or vet-analyzer finding is a non-blocking skip
		// (reviewer concern 2). `continue` (not return) so a non-compile
		// failure in ONE module doesn't abandon the gate — a later module
		// may still carry the real build-required-drift compile error.
		vetOut := strings.TrimSpace(string(out))
		if !looksLikeCompileError(vetOut) {
			skip("vet_nonzero_non_compile", vetOut)
			continue
		}
		return fmt.Errorf("%w: PR would not compile; %d file(s) outside scope are build-required: %s\n%s",
			gitops.ErrCommitWouldNotCompile, len(drift), strings.Join(drift, ", "), vetOut)
	}
	return nil
}

// openPRAndShipArtifact is the implement-stage post-processing
// chain. It commits the agent's edits, pushes a fresh branch via
// HTTPS, opens a PR via the GitHub REST API, and ships a
// pull_request artifact to the backend.
//
// Skips with an early return when the working tree is clean (no
// commit → no PR → no artifact). The agent producing zero edits
// is a real workflow signal but not necessarily a failure; we emit
// a policy_event into the trace via a follow-up bundle so the
// approver sees "agent decided no changes were needed."
func openPRAndShipArtifact(ctx context.Context, cfg config, logSink io.Writer, client uploadClient, issued *upload.IssuedKey) error {
	if cfg.runID == "" || cfg.stageID == "" {
		return errors.New("upload: --run-id and --stage-id required for implement stage")
	}
	if issued == nil {
		return errors.New("upload: signing key not issued (caller must hoist IssueKey before openPRAndShipArtifact)")
	}

	// Local-runner mode (E22.8 / #406): --no-pr skips the entire
	// push + PR-open + artifact-ship chain. The trace has already
	// uploaded above; the working tree stays dirty for the operator
	// to commit themselves. Emitting a structured log line so the
	// audit story is "we deliberately skipped" rather than "we lost
	// the PR step somewhere."
	if cfg.noPR {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"implement_pr_skipped","run_id":%q,"stage_id":%q,"reason":"no_pr_flag"}`+"\n",
			cfg.runID, cfg.stageID,
		)
		return nil
	}

	// Always mint a fresh App installation token at this point in
	// the stage, even if the auth pre-step's OIDC-minted token is
	// available via FISHHAWK_GITHUB_TOKEN. App tokens have a ~1-hour
	// TTL and a long agent run can outlive the original (the
	// pre-step minted at T+0s, the agent might finish at T+50min).
	// Backend's githubapp.CachedProvider returns the cached token
	// when it's still valid (with refresh-lead headroom) and mints
	// a fresh one otherwise — either way the runner gets a token
	// with maximum remaining life right when it needs to push.
	//
	// Audit gets two `installation_token_issued` events per
	// implement stage: the OIDC one at workflow start (used by
	// actions/checkout) and the Ed25519 one here (used by push +
	// PR). Both attribute to the App; auth_method on each entry
	// identifies which path served. (#201.)
	var token string
	tokenRes, err := client.FetchInstallationToken(ctx, upload.FetchInstallationTokenArgs{
		RunID:      cfg.runID,
		StageID:    cfg.stageID,
		PrivateKey: issued.PrivateKey,
	})
	switch {
	case err == nil:
		token = tokenRes.Token
		_, _ = fmt.Fprintf(logSink,
			`{"event":"installation_token_received","run_id":%q,"stage_id":%q,"source":"backend"}`+"\n",
			cfg.runID, cfg.stageID,
		)
	case errors.Is(err, upload.ErrNoInstallation):
		// No App installation attributed to this run (a local / MCP run
		// on a repo with no App). Fall back to the operator's local `gh`
		// CLI token so the push + PR still work without an operator hand-
		// push (#713). A user OAuth token authenticates both `git push`
		// over HTTPS and the REST PR-create call.
		ghTok, ghErr := ghAuthToken(ctx)
		if ghErr != nil {
			repoHint := cfg.githubRepo
			if repoHint == "" {
				repoHint = os.Getenv("GITHUB_REPOSITORY")
			}
			if repoHint == "" {
				repoHint = "the target repo"
			}
			return fmt.Errorf("this run has no GitHub App installation and no `gh` CLI token is available for the push + PR fallback; "+
				"either install the Fishhawk GitHub App on %s, or run `gh auth login` so the runner can use your local token: %w",
				repoHint, ghErr)
		}
		token = ghTok
		_, _ = fmt.Fprintf(logSink,
			`{"event":"installation_token_received","run_id":%q,"stage_id":%q,"source":"gh_cli"}`+"\n",
			cfg.runID, cfg.stageID,
		)
	default:
		return fmt.Errorf("fetch installation token: %w", err)
	}

	// Repo: --github-repo flag > GITHUB_REPOSITORY env. The flag
	// path is used by local-runner (Phase C of E22 / #389); the env
	// var stays the GHA-native source. Either way the value is the
	// canonical "owner/name" form.
	repoSlug := cfg.githubRepo
	if repoSlug == "" {
		repoSlug = os.Getenv("GITHUB_REPOSITORY")
	}
	if repoSlug == "" {
		return errors.New("upload: --github-repo flag or GITHUB_REPOSITORY env var is required for implement-stage push + PR")
	}
	owner, repoName, ok := strings.Cut(repoSlug, "/")
	if !ok || owner == "" || repoName == "" {
		return fmt.Errorf("upload: github repo %q is not owner/name", repoSlug)
	}
	// Base branch: --base-branch flag > GITHUB_REF_NAME env > "main"
	// default. Same precedence pattern as the repo lookup above.
	baseRef := cfg.baseBranch
	if baseRef == "" {
		baseRef = os.Getenv("GITHUB_REF_NAME")
	}
	if baseRef == "" {
		baseRef = "main"
	}

	repoDir := cfg.workingDir
	if repoDir == "" {
		repoDir = "."
	}

	// Branch routing: decomposed children share a single parent branch;
	// standalone runs get a per-stage branch.
	var (
		branch       string
		isDecomposed bool
		isSubsequent bool
	)
	if cfg.decomposedFromRunID != "" {
		isDecomposed = true
		branch = "fishhawk/run-" + shortID(cfg.decomposedFromRunID)
		isSubsequent = remoteBranchExists(ctx, repoDir, branch)
	} else {
		branch = fmt.Sprintf("fishhawk/run-%s/stage-%s", shortID(cfg.runID), shortID(cfg.stageID))
	}

	title, body := prTitleAndBody(cfg, branch, logSink)
	commitMessage := title + "\n\n" + body

	// Compile-gate the scope-only committed tree before push (#728), on every
	// implement push including decomposed children (#766). A scope-bounded
	// child commit is the highest-risk path for a drift-dropped non-compiling
	// HEAD: StageScoped (#581) can strip build-required drift, so an
	// incomplete child commit is a scope-drift defect to surface, not a
	// tolerated intermediate. The gate's isolated worktree checks out the
	// specific headSHA, so it works unchanged for a shared-branch child
	// commit. The hook runs inside CommitAndPush after the commit and before
	// the push, so a failure leaves origin untouched.
	verifyCommit := func(ctx context.Context, headSHA string, drift []string) error {
		if err := verifyCommittedTreeCompiles(ctx, repoDir, headSHA, drift, logSink); err != nil {
			driftJSON, _ := json.Marshal(drift)
			_, _ = fmt.Fprintf(logSink,
				`{"event":"compile_gate_failed","run_id":%q,"stage_id":%q,"head_sha":%q,"drift":%s}`+"\n",
				cfg.runID, cfg.stageID, headSHA, driftJSON)
			return err
		}
		return nil
	}

	cap, err := newPusher().CommitAndPush(ctx, gitops.CommitAndPushArgs{
		RepoDir:       repoDir,
		Branch:        branch,
		CommitMessage: commitMessage,
		RemoteURL:     fmt.Sprintf("https://github.com/%s/%s", owner, repoName),
		// App bot commit identity (#722): the backend resolves the App's
		// `<slug>[bot]` name + `<id>+<slug>[bot]@users.noreply.github.com`
		// email and echoes them on the prompt response. Empty values flow
		// through to the gitops orDefault fallback (DefaultAuthorName/Email)
		// unchanged — e.g. local/dev runs with no resolvable App.
		AuthorName:  cfg.commitAuthorName,
		AuthorEmail: cfg.commitAuthorEmail,
		// Refresh the local extraheader with the freshly-minted
		// token before push. Handles the long-running-stage case
		// where the auth pre-step's token (set by actions/checkout)
		// has expired by the time the agent finishes. See the
		// FetchInstallationToken call above.
		PushToken:        token,
		ForceWithLease:   isDecomposed,
		RebaseFromRemote: isSubsequent,
		// Scope-bounded commit (#581): stage exactly the approved
		// plan's declared paths, excluding stray dirty files. Empty
		// (plan_missing_for_implement) falls back to `git add -A`.
		ScopeFiles: scopePaths(cfg.scopeFiles),
		// Compile-gate the committed tree before push (#728). Always wired,
		// including the decomposed-child path (#766, see above) — the gate
		// runs on every implement push.
		VerifyCommit: verifyCommit,
	})
	if err != nil {
		return fmt.Errorf("commit+push: %w", err)
	}
	if len(cap.ScopeDrift) > 0 {
		driftJSON, _ := json.Marshal(cap.ScopeDrift)
		_, _ = fmt.Fprintf(logSink,
			`{"event":"scope_drift","run_id":%q,"stage_id":%q,"undeclared":%s}`+"\n",
			cfg.runID, cfg.stageID, driftJSON)
	}
	if cap.NoChanges {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"implement_no_changes","run_id":%q,"stage_id":%q,"base_sha":%q}`+"\n",
			cfg.runID, cfg.stageID, cap.BaseSHA,
		)
		// No PR to open; no artifact to ship. The stage still
		// counts as succeeded — the agent decided no edits were
		// needed. Reviewer will see the empty trace + this log.
		return nil
	}

	// Decomposed children only push their commit onto the shared parent
	// branch (one commit per sub-plan, in dependency order); they never
	// open a PR or ship a pull_request artifact. Per ADR-032 (#719) the
	// parent run opens ONE consolidated PR for the whole decomposition
	// after all children settle — so suppress OpenPR + ShipPullRequest
	// for every decomposed child, first and subsequent alike. (Before
	// #714 only the subsequent children skipped, and the first child
	// opened a child-owned PR the parent never tracked.)
	if isDecomposed {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"implement_child_pushed","run_id":%q,"stage_id":%q,"shared_branch":%q,"head_sha":%q,"is_subsequent":%t}`+"\n",
			cfg.runID, cfg.stageID, branch, cap.HeadSHA, isSubsequent,
		)
		return nil
	}

	prRes, err := newPROpener(token).OpenPR(ctx, gitops.OpenPRArgs{
		Owner: owner,
		Repo:  repoName,
		Head:  branch,
		Base:  baseRef,
		Title: title,
		Body:  body,
	})
	if err != nil {
		return fmt.Errorf("open PR: %w", err)
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"pull_request_opened","run_id":%q,"stage_id":%q,"pr_number":%d,"pr_url":%q,"head_sha":%q}`+"\n",
		cfg.runID, cfg.stageID, prRes.PRNumber, prRes.PRURL, cap.HeadSHA,
	)

	// Diff size: count files via gitdiff against the base ref.
	// Best-effort; failure here doesn't block the artifact upload.
	filesChanged := 0
	if d, err := (&gitdiff.Runner{}).Run(ctx, baseRef, repoDir); err == nil {
		filesChanged = len(d.ChangedFiles)
	}

	artifactBody, _ := json.Marshal(map[string]any{
		"pr_number":           prRes.PRNumber,
		"pr_url":              prRes.PRURL,
		"branch":              branch,
		"head_sha":            cap.HeadSHA,
		"base_sha":            cap.BaseSHA,
		"title":               title,
		"body":                body,
		"files_changed_count": filesChanged,
	})

	shipRes, err := client.ShipPullRequest(ctx, upload.ShipPullRequestArgs{
		RunID:      cfg.runID,
		StageID:    cfg.stageID,
		Body:       artifactBody,
		PrivateKey: issued.PrivateKey,
	})
	if err != nil {
		return fmt.Errorf("ship pull-request: %w", err)
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"pull_request_uploaded","run_id":%q,"stage_id":%q,"artifact_id":%q,"content_hash":%q,"idempotent":%t}`+"\n",
		cfg.runID, cfg.stageID, shipRes.ID, shipRes.ContentHash, shipRes.Idempotent,
	)
	return nil
}

// reportPullRequestFailure POSTs a failure-outcome body to
// /v0/runs/{run_id}/pull-request so the backend fails the implement stage
// its trace gate left in `running` (#742). In push_and_open_pr mode the
// /pull-request upload is the authoritative driver of the implement
// stage's terminal transition, so a commit/push/PR-open failure must be
// reported here — otherwise the gated stage hangs in `running` until the
// SLA watchdog reaps it, instead of landing `failed` (retryable for C).
//
// category is "B" (ErrPullRequestInvalid / ErrCommitWouldNotCompile) or
// "C" (network, git, GitHub API). Best-effort: a report failure is logged
// but never changes the runner's exit — the failed exit already stands.
func reportPullRequestFailure(ctx context.Context, cfg config, logSink io.Writer, client uploadClient, issued *upload.IssuedKey, category, reason string) {
	if issued == nil || client == nil {
		return
	}
	if _, err := client.ShipPullRequest(ctx, upload.ShipPullRequestArgs{
		RunID:      cfg.runID,
		StageID:    cfg.stageID,
		PrivateKey: issued.PrivateKey,
		Outcome:    "failed",
		Category:   category,
		Reason:     reason,
	}); err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"pull_request_failure_report_failed","run_id":%q,"stage_id":%q,"detail":%q}`+"\n",
			cfg.runID, cfg.stageID, err.Error())
		return
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"pull_request_failure_reported","run_id":%q,"stage_id":%q,"category":%q}`+"\n",
		cfg.runID, cfg.stageID, category)
}

// shortID returns the first 8 characters of a UUID-shaped string,
// for use in branch names and titles. Non-UUID strings round-trip
// up to 8 chars.
func shortID(id string) string {
	if len(id) < 8 {
		return id
	}
	return id[:8]
}

// scopeHandoffPath mirrors the /tmp/fishhawk-pr.md handoff: the runner
// writes the implement stage's resolved scope.files here so the
// out-of-process CLI auto-PR path (cli/cmd/fishhawk/autopr.go, a
// separate Go module) can bound its staging to the same declared paths
// (#581). var (not const) so tests can redirect it to a t.TempDir path
// and avoid /tmp pollution / parallel-test races.
var scopeHandoffPath = "/tmp/fishhawk-scope.json"

// scopeHandoff is the JSON written to scopeHandoffPath. `files` mirrors
// the standard_v1 plan scope.files shape (path + operation) so the CLI
// and runner agree field-for-field — this is the runner↔CLI wire
// contract for #581, not a JSON Schema.
type scopeHandoff struct {
	Files []upload.ScopeFile `json:"files"`
}

// writeScopeHandoff writes the resolved scope.files to scopeHandoffPath
// for the CLI auto-PR path. Best-effort: a marshal or write failure is
// logged but never fails the stage — the CLI falls back to `git add -A`
// when the file is absent or empty.
func writeScopeHandoff(files []upload.ScopeFile, logSink io.Writer) {
	data, err := json.Marshal(scopeHandoff{Files: files})
	if err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"scope_handoff_failed","reason":"marshal","detail":%q}`+"\n", err.Error())
		return
	}
	if err := os.WriteFile(scopeHandoffPath, data, 0o600); err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"scope_handoff_failed","reason":"write","detail":%q}`+"\n", err.Error())
		return
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"scope_handoff_written","path":%q,"file_count":%d}`+"\n",
		scopeHandoffPath, len(files))
}

// scopePaths extracts the repo-relative path list from the resolved
// scope.files, dropping entries with an empty path. Used to bound both
// the policy diff staging and the implement commit.
func scopePaths(files []upload.ScopeFile) []string {
	if len(files) == 0 {
		return nil
	}
	paths := make([]string, 0, len(files))
	for _, f := range files {
		if f.Path == "" {
			continue
		}
		paths = append(paths, f.Path)
	}
	return paths
}

// pullRequestDescriptionPath mirrors prompt.PullRequestDescriptionPath
// in the backend. Hardcoded in both places by design — the runner
// and the backend are independent Go modules; using the shared
// string here is the cheapest coordination. v0.x can move to a
// per-stage env var if multi-tenancy demands isolation. (#206.)
//
// var (not const) so tests can swap it for a t.TempDir-scoped path
// and avoid /tmp pollution / parallel-test races.
var pullRequestDescriptionPath = "/tmp/fishhawk-pr.md"

// prTitleAndBody assembles the PR title and body for the implement
// stage. Tries the agent-authored file first; falls back to the
// generic Fishhawk template when missing or malformed.
//
// Either way the body gets a "Fishhawk attribution" footer appended
// — run id, audit-log URL, branch, and the PR's own branch — so
// auditable provenance is preserved without requiring the agent to
// remember to include it in every PR.
func prTitleAndBody(cfg config, branch string, logSink io.Writer) (title, body string) {
	agentTitle, agentBody, kind := loadAgentAuthoredPR(logSink)

	switch kind {
	case prSourceAgent:
		title = agentTitle
		body = agentBody
	case prSourceFallback:
		title = fmt.Sprintf("Fishhawk: implement stage %s", shortID(cfg.stageID))
		body = fmt.Sprintf(
			"Opened by Fishhawk for run `%s`, stage `%s`.\n\nBranch: `%s`\nAudit log: see `%s/v0/runs/%s/audit`.\n",
			cfg.runID, cfg.stageID, branch,
			strings.TrimRight(cfg.backendURL, "/"), cfg.runID,
		)
		// Footer for fallback is rolled into the body itself, so
		// don't double up below.
		return title, body
	}

	// Agent-authored path: append the attribution footer so
	// reviewers can find the run + audit log even when the agent's
	// body doesn't mention them.
	footer := fmt.Sprintf(
		"\n\n---\n_Opened by [Fishhawk](https://github.com/kuhlman-labs/fishhawk) for run `%s`, stage `%s`._\n_Branch: `%s` · Audit log: `%s/v0/runs/%s/audit`._\n",
		cfg.runID, cfg.stageID, branch,
		strings.TrimRight(cfg.backendURL, "/"), cfg.runID,
	)
	return title, body + footer
}

// prSource categorizes where the PR title + body came from. Lets
// callers branch on "agent wrote it" vs "we synthesized a fallback"
// without inspecting the strings.
type prSource int

const (
	prSourceFallback prSource = iota
	prSourceAgent
)

// loadAgentAuthoredPR tries to read the agent-authored PR file and
// parse it into a (title, body) pair. Returns prSourceFallback when
// the file is absent or malformed; prSourceAgent on success.
//
// Format (#206):
//   - First line is the title (≤72 chars; we don't enforce, GitHub
//     handles overflow gracefully).
//   - Blank line.
//   - Remaining lines are the body (markdown).
//
// Malformed cases (logged as a `pr_template_invalid` policy event
// but non-fatal):
//   - Empty file.
//   - First line empty.
//   - No blank line separating title from body.
func loadAgentAuthoredPR(logSink io.Writer) (title, body string, kind prSource) {
	raw, err := os.ReadFile(pullRequestDescriptionPath)
	if err != nil {
		// File absent is the common no-op path (agent didn't follow
		// the instruction, or we're in a stage type that doesn't
		// produce a PR). Don't log; just fall back.
		return "", "", prSourceFallback
	}

	text := strings.TrimRight(string(raw), "\n")
	if text == "" {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"pr_template_invalid","reason":%q,"path":%q}`+"\n",
			"empty file", pullRequestDescriptionPath)
		return "", "", prSourceFallback
	}

	// Split into title (first line) + body (rest after blank line).
	// We tolerate either CRLF or LF; normalize to LF for the body.
	lines := strings.SplitN(text, "\n", 2)
	title = strings.TrimSpace(lines[0])
	if title == "" {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"pr_template_invalid","reason":%q,"path":%q}`+"\n",
			"empty title line", pullRequestDescriptionPath)
		return "", "", prSourceFallback
	}

	if len(lines) < 2 {
		// Title-only file is treated as a body-less PR — that's
		// allowed; GitHub accepts an empty PR body. Still log so
		// the operator can spot agents that aren't writing bodies.
		_, _ = fmt.Fprintf(logSink,
			`{"event":"pr_template_warning","reason":%q,"path":%q}`+"\n",
			"title-only (no body)", pullRequestDescriptionPath)
		return title, "", prSourceAgent
	}

	rest := strings.TrimLeft(lines[1], "\n")
	// The format calls for a blank line between title and body, but
	// agents are sloppy. Trim leading newlines and let it through;
	// only flag truly malformed cases (no separator at all).
	if !strings.HasPrefix(lines[1], "\n") {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"pr_template_warning","reason":%q,"path":%q}`+"\n",
			"no blank line between title and body", pullRequestDescriptionPath)
	}

	return title, rest, prSourceAgent
}
