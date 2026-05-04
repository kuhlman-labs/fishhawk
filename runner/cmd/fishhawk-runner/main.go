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
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agent/claudecode"
	"github.com/kuhlman-labs/fishhawk/runner/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/runner/internal/constraint"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitdiff"
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
	FetchPrompt(ctx context.Context, args upload.FetchPromptArgs) (*upload.FetchedPrompt, error)
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
		path, fetchErr := fetchPromptToFile(context.Background(), client, cfg, key, logSink)
		if fetchErr != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"runner_failed","reason":"fetch_prompt","detail":%q}`+"\n", fetchErr.Error())
			return exitFailure
		}
		cfg.promptFile = path
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
	}

	invoker := newInvoker(os.Getenv("ANTHROPIC_API_KEY"))
	res, invokeErr := invoker.Invoke(context.Background(), inv)

	// Plan validation runs only if the agent itself succeeded —
	// no point re-stating "your plan is malformed" when the
	// agent already failed. A plan-validation failure overrides
	// res.OK and demotes the run to category-B (constraint /
	// policy violation per MVP_SPEC §6).
	if res.OK && cfg.planOut != "" {
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

	// Constraint evaluation: same demotion rules as plan
	// validation. Only runs if everything before it succeeded so
	// we don't double-stamp a category-A failure as B.
	if res.OK && cfg.constraintsFile != "" && cfg.checkBaseRef != "" {
		if evs, demote := enforceConstraints(cfg); demote != nil {
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
	// We build once into memory, then write to disk and/or upload.
	// When neither is configured, fall back to stdout JSONL so
	// callers exercising --prompt-file alone can still inspect.
	var bundleBytes []byte
	if cfg.bundleOut != "" || cfg.uploadTrace {
		bytesData, _, err := bundle.PackBytes(bundle.PackInputs{
			RunID:   cfg.runID,
			StageID: bundleStageID(cfg),
			Agent:   "claude-code",
		}, res.Events)
		if err != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"runner_failed","reason":"bundle_pack","detail":%q}`+"\n", err.Error())
			logCompletion(logSink, res, invokeErr)
			return exitFailure
		}
		bundleBytes = bytesData
	}

	if cfg.bundleOut != "" {
		// 0o600 — bundle may carry redacted credentials in raw events
		// until E2.4 redaction is wired upstream of the bundler. The
		// runner's filesystem is ephemeral, but defense in depth is
		// cheap.
		if err := os.WriteFile(cfg.bundleOut, bundleBytes, 0o600); err != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"runner_failed","reason":"bundle_write","detail":%q}`+"\n", err.Error())
			logCompletion(logSink, res, invokeErr)
			return exitFailure
		}
	}

	if cfg.uploadTrace {
		if err := uploadTrace(cfg, bundleBytes, logSink, client, issuedKey); err != nil {
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
				`{"event":"runner_failed","reason":"trace_upload","detail":%q}`+"\n", err.Error())
			logCompletion(logSink, res, invokeErr)
			return exitFailure
		}
	}

	if bundleBytes == nil {
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
func uploadTrace(cfg config, bundleBytes []byte, logSink io.Writer, client uploadClient, issued *upload.IssuedKey) error {
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
		Variant:    cfg.variant,
		Bundle:     bundleBytes,
		PrivateKey: issued.PrivateKey,
	})
	if err != nil {
		return fmt.Errorf("ship trace: %w", err)
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"trace_uploaded","run_id":%q,"stage_id":%q,"variant":%q,"content_hash":%q}`+"\n",
		res.RunID, res.StageID, res.Variant, res.ContentHash,
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
// writes it to a temp file, and returns the path. The temp file is
// 0o600 — bundle-style defense in depth, since prompts may include
// issue bodies that the customer would prefer not to leave on the
// runner's filesystem world-readable.
func fetchPromptToFile(ctx context.Context, client uploadClient, cfg config, key *upload.IssuedKey, logSink io.Writer) (string, error) {
	got, err := client.FetchPrompt(ctx, upload.FetchPromptArgs{
		StageID:    cfg.stageID,
		PrivateKey: key.PrivateKey,
	})
	if err != nil {
		return "", err
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"prompt_fetched","stage_id":%q,"stage_type":%q,"prompt_hash":%q,"prompt_bytes":%d}`+"\n",
		got.StageID, got.StageType, got.PromptHash, len(got.Prompt),
	)
	tmp, err := os.CreateTemp("", "fishhawk-prompt-*.txt")
	if err != nil {
		return "", fmt.Errorf("create prompt temp file: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("chmod prompt temp file: %w", err)
	}
	if _, err := tmp.WriteString(got.Prompt); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write prompt temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close prompt temp file: %w", err)
	}
	return tmp.Name(), nil
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

// enforceConstraints runs `git diff --name-status` against the
// configured base ref and evaluates the constraints from the JSON
// file. Returns one or more policy_event events for the bundle
// plus a non-nil error iff any constraint was violated. The
// caller demotes the run to category-B on a non-nil error.
//
// On infra failures (file read, git error) we still demote to
// category-B because from the workflow author's perspective "the
// runner couldn't verify your constraints" is a constraint-stage
// failure, not an agent failure.
func enforceConstraints(cfg config) ([]agent.Event, error) {
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

	repoDir := cfg.workingDir
	if repoDir == "" {
		repoDir = "."
	}
	d, err := (&gitdiff.Runner{}).Run(context.Background(), cfg.checkBaseRef, repoDir)
	if err != nil {
		return []agent.Event{{
			Kind:    "policy_event",
			Payload: agent.MakePayload(map[string]string{"check": "constraints", "outcome": "diff_failed", "error": err.Error()}),
		}}, fmt.Errorf("constraints: %w", err)
	}

	// Emit a git_diff event ahead of policy evaluation so the
	// backend can re-evaluate constraints independently (E3.13).
	// The runner is operating on a customer machine; the backend's
	// re-evaluation is the auditable source of truth for whether a
	// stage's output passes policy.
	diffEvent := makeGitDiffEvent(cfg.checkBaseRef, d)

	violations := constraint.Evaluate(d, c)
	if len(violations) == 0 {
		return []agent.Event{
			diffEvent,
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
	// parsing free-text. The git_diff event leads so a single-pass
	// backend reader can pull the file list before the violations.
	evs := []agent.Event{diffEvent}
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
