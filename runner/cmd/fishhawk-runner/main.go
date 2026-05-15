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
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agent/claudecode"
	"github.com/kuhlman-labs/fishhawk/runner/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/runner/internal/constraint"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitdiff"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/plan"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

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
}

// newUploadClient returns the production uploadClient for the
// given backend URL. Overridable by tests.
var newUploadClient = func(baseURL string) uploadClient {
	return upload.New(baseURL)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run is split out so tests can drive it without exiting the test
// process. Returns the intended process exit code.
//
// logSink receives the structured startup log line and any failure
// notes; trace events (when the harness runs) go to os.Stdout so
// they can be piped or captured separately. This split lets a
// caller redirect stderr for diagnostics while keeping the trace
// stream clean.
func run(args []string, logSink io.Writer) int {
	cfg, err := parseFlags(args, logSink)
	if err != nil {
		// parseFlags already wrote a usage / error message.
		return exitUsage
	}

	logStartup(logSink, cfg)

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
		key, fetchErr := issueSigningKey(context.Background(), client, cfg, logSink)
		if fetchErr != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"runner_failed","reason":"issue_key","detail":%q}`+"\n", fetchErr.Error())
			return exitFailure
		}
		issuedKey = key
		path, sType, fetchErr := fetchPromptToFile(context.Background(), client, cfg, key, logSink)
		if fetchErr != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"runner_failed","reason":"fetch_prompt","detail":%q}`+"\n", fetchErr.Error())
			return exitFailure
		}
		cfg.promptFile = path
		stageType = sType
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
	}

	// E19.8 / #348: mint a short-lived MCP token for the agent and
	// layer it onto the invocation env. Best-effort — if the token
	// fetch fails we log and continue. The agent loses Fishhawk
	// MCP awareness but the run still produces a valid trace /
	// plan / PR per the rest of the stage flow.
	if issuedKey != nil {
		mcpTok, err := client.FetchMCPToken(context.Background(), upload.FetchMCPTokenArgs{
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
	res, invokeErr := invoker.Invoke(context.Background(), inv)

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
	var diff *constraint.Diff
	var diffErr error
	if cfg.checkBaseRef != "" {
		d, ev, err := computeAndEmitDiff(cfg)
		res.Events = append(res.Events, ev)
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
	var rawBundle, redactedBundle []byte
	if cfg.bundleOut != "" || cfg.uploadTrace {
		agentFailed := res.FailureCategory == "A"
		agentFailureReason := ""
		if agentFailed {
			agentFailureReason = res.FailureReason
		}

		manifestRaw := bundle.PackInputs{
			RunID:              cfg.runID,
			StageID:            bundleStageID(cfg),
			Agent:              "claude-code",
			AgentFailed:        agentFailed,
			AgentFailureReason: agentFailureReason,
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
			key, err := issueSigningKey(context.Background(), client, cfg, logSink)
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
			if err := uploadTrace(cfg, v.name, v.bytes, logSink, client, issuedKey); err != nil {
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
		if res.OK && cfg.planOut != "" && stageType != "implement" && stageType != "review" {
			if err := uploadPlan(cfg, logSink, client, issuedKey); err != nil {
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
			if err := openPRAndShipArtifact(cfg, logSink, client, issuedKey); err != nil {
				res.OK = false
				if errors.Is(err, upload.ErrPullRequestInvalid) {
					res.FailureCategory = "B"
				} else {
					res.FailureCategory = "C"
				}
				res.FailureReason = err.Error()
				invokeErr = err
				_, _ = fmt.Fprintf(logSink,
					`{"event":"runner_failed","reason":"pull_request_upload","detail":%q}`+"\n", err.Error())
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

func uploadTrace(cfg config, variant string, bundleBytes []byte, logSink io.Writer, client uploadClient, issued *upload.IssuedKey) error {
	if cfg.stageID == "" {
		return errors.New("upload: --stage-id required with --upload-trace")
	}
	if client == nil {
		client = newUploadClient(cfg.backendURL)
	}
	ctx := context.Background()

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
func uploadPlan(cfg config, logSink io.Writer, client uploadClient, issued *upload.IssuedKey) error {
	if cfg.stageID == "" {
		return errors.New("upload: --stage-id required with --plan-out + --upload-trace")
	}
	if client == nil {
		client = newUploadClient(cfg.backendURL)
	}
	ctx := context.Background()

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
// writes it to a temp file, and returns the path plus the stage
// type from the response. stageType drives per-stage post-processing
// (plan validation + upload for plan stages, commit+push+PR upload
// for implement stages). The temp file is 0o600 — bundle-style
// defense in depth, since prompts may include issue bodies that the
// customer would prefer not to leave on the runner's filesystem
// world-readable.
func fetchPromptToFile(ctx context.Context, client uploadClient, cfg config, key *upload.IssuedKey, logSink io.Writer) (string, string, error) {
	got, err := client.FetchPrompt(ctx, upload.FetchPromptArgs{
		StageID:    cfg.stageID,
		PrivateKey: key.PrivateKey,
	})
	if err != nil {
		return "", "", err
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"prompt_fetched","stage_id":%q,"stage_type":%q,"prompt_hash":%q,"prompt_bytes":%d}`+"\n",
		got.StageID, got.StageType, got.PromptHash, len(got.Prompt),
	)
	tmp, err := os.CreateTemp("", "fishhawk-prompt-*.txt")
	if err != nil {
		return "", "", fmt.Errorf("create prompt temp file: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		_ = tmp.Close()
		return "", "", fmt.Errorf("chmod prompt temp file: %w", err)
	}
	if _, err := tmp.WriteString(got.Prompt); err != nil {
		_ = tmp.Close()
		return "", "", fmt.Errorf("write prompt temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", "", fmt.Errorf("close prompt temp file: %w", err)
	}
	return tmp.Name(), got.StageType, nil
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
// Stages everything with `git add -A` before running the diff so
// fresh files the agent created (test fixtures, new packages) show
// up. The runner's later CommitAndPush calls `git add -A` again,
// which is a no-op on an already-staged index. Pre-#296 the diff
// ran against `<base>...HEAD` and saw nothing because the agent's
// edits hadn't been committed yet; every PR silently failed
// `tests_added_or_updated` and friends at the backend's policy
// re-evaluation step.
//
// Returns the parsed Diff (consumed by enforceConstraints when
// constraints-file is also set), a bundle event (always — either
// the git_diff payload on success, or a policy_event marking the
// failure on error), and the underlying error for the caller's
// log line. The error is intentionally NOT load-bearing on the
// run's res.OK; the in-band constraint enforcer below is the one
// that demotes to category-B.
func computeAndEmitDiff(cfg config) (constraint.Diff, agent.Event, error) {
	repoDir := cfg.workingDir
	if repoDir == "" {
		repoDir = "."
	}
	// Stage everything the agent touched so `git diff --cached` can
	// see it. add -A respects .gitignore, idempotent on a clean
	// repo, and doesn't fail when there's nothing to stage. A
	// failure here means we can't reliably compute the diff — surface
	// as a diff_failed policy_event same as a git diff failure.
	addCmd := exec.CommandContext(context.Background(), "git", "add", "-A")
	addCmd.Dir = repoDir
	if out, err := addCmd.CombinedOutput(); err != nil {
		return constraint.Diff{}, agent.Event{
			Kind: "policy_event",
			Payload: agent.MakePayload(map[string]string{
				"check": "diff", "outcome": "stage_failed",
				"error": fmt.Sprintf("git add -A: %v: %s", err, strings.TrimSpace(string(out))),
			}),
		}, fmt.Errorf("computeAndEmitDiff: stage: %w", err)
	}
	d, err := (&gitdiff.Runner{}).Run(context.Background(), cfg.checkBaseRef, repoDir)
	if err != nil {
		return constraint.Diff{}, agent.Event{
			Kind:    "policy_event",
			Payload: agent.MakePayload(map[string]string{"check": "diff", "outcome": "diff_failed", "error": err.Error()}),
		}, fmt.Errorf("computeAndEmitDiff: %w", err)
	}
	return d, makeGitDiffEvent(cfg.checkBaseRef, d), nil
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
type gitDiffPayload struct {
	Kind     string        `json:"kind"`
	BaseRef  string        `json:"base_ref"`
	Files    []gitDiffFile `json:"files"`
	NumFiles int           `json:"num_files"`
}

// makeGitDiffEvent converts a constraint.Diff into the bundle event
// the backend's policy re-evaluation reads. Kind is "git_diff";
// payload schema is gitDiffPayload (above).
func makeGitDiffEvent(baseRef string, d constraint.Diff) agent.Event {
	files := make([]gitDiffFile, 0, len(d.ChangedFiles))
	for _, f := range d.ChangedFiles {
		files = append(files, gitDiffFile{Path: f.Path, Status: string(f.Status)})
	}
	return agent.Event{
		Kind: "git_diff",
		Payload: agent.MakePayload(gitDiffPayload{
			Kind:     "name_status",
			BaseRef:  baseRef,
			Files:    files,
			NumFiles: len(files),
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
	if vErr := plan.Validate(data); vErr != nil {
		return agent.Event{
			Kind:    "policy_event",
			Payload: agent.MakePayload(map[string]string{"check": "plan_validation", "outcome": "invalid", "path": path, "error": vErr.Error()}),
		}, vErr
	}
	return agent.Event{
		Kind:    "policy_event",
		Payload: agent.MakePayload(map[string]string{"check": "plan_validation", "outcome": "valid", "path": path}),
	}, nil
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
)

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
func openPRAndShipArtifact(cfg config, logSink io.Writer, client uploadClient, issued *upload.IssuedKey) error {
	if cfg.runID == "" || cfg.stageID == "" {
		return errors.New("upload: --run-id and --stage-id required for implement stage")
	}
	if issued == nil {
		return errors.New("upload: signing key not issued (caller must hoist IssueKey before openPRAndShipArtifact)")
	}

	ctx := context.Background()

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
	tokenRes, err := client.FetchInstallationToken(ctx, upload.FetchInstallationTokenArgs{
		RunID:      cfg.runID,
		StageID:    cfg.stageID,
		PrivateKey: issued.PrivateKey,
	})
	if err != nil {
		return fmt.Errorf("fetch installation token: %w", err)
	}
	token := tokenRes.Token
	_, _ = fmt.Fprintf(logSink,
		`{"event":"installation_token_received","run_id":%q,"stage_id":%q,"source":"backend"}`+"\n",
		cfg.runID, cfg.stageID,
	)

	repoSlug := os.Getenv("GITHUB_REPOSITORY") // "owner/name"
	if repoSlug == "" {
		return errors.New("upload: GITHUB_REPOSITORY env var is required for implement-stage push + PR")
	}
	owner, repoName, ok := strings.Cut(repoSlug, "/")
	if !ok || owner == "" || repoName == "" {
		return fmt.Errorf("upload: GITHUB_REPOSITORY %q is not owner/name", repoSlug)
	}
	baseRef := os.Getenv("GITHUB_REF_NAME")
	if baseRef == "" {
		baseRef = "main"
	}

	repoDir := cfg.workingDir
	if repoDir == "" {
		repoDir = "."
	}
	branch := fmt.Sprintf("fishhawk/run-%s/stage-%s", shortID(cfg.runID), shortID(cfg.stageID))
	title, body := prTitleAndBody(cfg, branch, logSink)
	commitMessage := title + "\n\n" + body

	cap, err := newPusher().CommitAndPush(ctx, gitops.CommitAndPushArgs{
		RepoDir:       repoDir,
		Branch:        branch,
		CommitMessage: commitMessage,
		RemoteURL:     fmt.Sprintf("https://github.com/%s/%s", owner, repoName),
		// Refresh the local extraheader with the freshly-minted
		// token before push. Handles the long-running-stage case
		// where the auth pre-step's token (set by actions/checkout)
		// has expired by the time the agent finishes. See the
		// FetchInstallationToken call above.
		PushToken: token,
	})
	if err != nil {
		return fmt.Errorf("commit+push: %w", err)
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

// shortID returns the first 8 characters of a UUID-shaped string,
// for use in branch names and titles. Non-UUID strings round-trip
// up to 8 chars.
func shortID(id string) string {
	if len(id) < 8 {
		return id
	}
	return id[:8]
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
