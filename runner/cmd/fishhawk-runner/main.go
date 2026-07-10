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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/acceptenv"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agent/claudecode"
	"github.com/kuhlman-labs/fishhawk/runner/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/runner/internal/constraint"
	"github.com/kuhlman-labs/fishhawk/runner/internal/egressproxy"
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
// non-test code; tests reassign and restore it via t.Cleanup. The
// binary arg is the operator FISHHAWK_AGENT_BIN override; empty leaves
// the adapter .Binary empty so it resolves claudecode.DefaultBinary
// (#1741).
var newInvoker = func(apiKey, binary string) agent.Invoker {
	inv := claudecode.New(apiKey)
	inv.Binary = binary
	return inv
}

// deriveStructuredOutputSchema is the seam that produces the plan-stage
// structured-output schema (#1325). Production wiring derives it at runtime
// from the embedded canonical standard_v1 schema; tests reassign it to force
// the derivation-error path and assert the runner degrades gracefully to an
// UNCONSTRAINED invocation rather than failing the plan stage.
var deriveStructuredOutputSchema = plan.StructuredOutputSchema

// uploadClient is the test seam for the backend HTTP client.
// Tests substitute a fake to drive ShipTrace / IssueKey / FetchPrompt
// without standing up an httptest.Server.
type uploadClient interface {
	IssueKey(ctx context.Context, runID string, ttl time.Duration) (*upload.IssuedKey, error)
	ShipTrace(ctx context.Context, args upload.ShipArgs) (*upload.ShipResult, error)
	ShipPlan(ctx context.Context, args upload.ShipPlanArgs) (*upload.ShipPlanResult, error)
	ShipAcceptance(ctx context.Context, args upload.ShipAcceptanceArgs) (*upload.ShipAcceptanceResult, error)
	ShipPullRequest(ctx context.Context, args upload.ShipPullRequestArgs) (*upload.ShipPullRequestResult, error)
	FetchPrompt(ctx context.Context, args upload.FetchPromptArgs) (*upload.FetchedPrompt, error)
	FetchInstallationToken(ctx context.Context, args upload.FetchInstallationTokenArgs) (*upload.FetchInstallationTokenResult, error)
	FetchMCPToken(ctx context.Context, args upload.FetchMCPTokenArgs) (*upload.FetchMCPTokenResult, error)
	FetchScopeAmendments(ctx context.Context, args upload.FetchScopeAmendmentsArgs) ([]upload.ScopeAmendment, error)
	RetryStage(ctx context.Context, args upload.RetryStageArgs) error
	RunLineageComplete(ctx context.Context, runID string) (bool, error)
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

// agentVersionTokenRe extracts the first 1-to-3-part dotted numeric token
// from a free-form agent CLI version string. "2.1.5 (Claude Code)" -> "2.1.5";
// "codex 0.30" -> "0.30"; "unknown" -> "" (no token, uncomparable).
var agentVersionTokenRe = regexp.MustCompile(`[0-9]+(?:\.[0-9]+){0,2}`)

// agentVersionComparator is one parsed term of an agent_version range: an
// operator and its 3-slot version.
type agentVersionComparator struct {
	op  string
	ver [3]int
}

// satisfiedBy reports whether ver satisfies this comparator.
func (c agentVersionComparator) satisfiedBy(ver [3]int) bool {
	cmp := compareVersionTriples(ver, c.ver)
	switch c.op {
	case ">":
		return cmp > 0
	case ">=":
		return cmp >= 0
	case "<":
		return cmp < 0
	case "<=":
		return cmp <= 0
	case "=", "==":
		return cmp == 0
	default:
		return false
	}
}

// agentVersionOps is the closed comparator-operator set, longest-first so the
// two-character operators match before their single-character prefixes.
var agentVersionOps = []string{">=", "<=", "==", ">", "<", "="}

// parseAgentVersionComparators parses an agent_version range (a space-separated
// AND list of comparators, e.g. ">=2.1 <2.2") into its comparator list,
// returning ok=false on an empty or malformed range so the caller degrades to
// a warn-and-proceed rather than blocking on a backend authoring bug.
func parseAgentVersionComparators(rng string) ([]agentVersionComparator, bool) {
	fields := strings.Fields(rng)
	if len(fields) == 0 {
		return nil, false
	}
	comps := make([]agentVersionComparator, 0, len(fields))
	for _, tok := range fields {
		op := ""
		for _, cand := range agentVersionOps {
			if strings.HasPrefix(tok, cand) {
				op = cand
				break
			}
		}
		if op == "" {
			return nil, false
		}
		ver, ok := parseVersionTriple(strings.TrimPrefix(tok, op))
		if !ok {
			return nil, false
		}
		comps = append(comps, agentVersionComparator{op: op, ver: ver})
	}
	return comps, true
}

// parseVersionTriple parses a 1-to-3-part dotted numeric version into a 3-slot
// array, zero-padding missing components ("2.1" -> {2,1,0}). Returns ok=false
// on an empty string, more than three parts, or a non-numeric part.
func parseVersionTriple(v string) ([3]int, bool) {
	var out [3]int
	if v == "" {
		return out, false
	}
	parts := strings.Split(v, ".")
	if len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// compareVersionTriples returns -1, 0, or 1 as a is less than, equal to, or
// greater than b, comparing the three components in order.
func compareVersionTriples(a, b [3]int) int {
	for i := 0; i < 3; i++ {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}

// matchAgentVersionRange evaluates a probed agent CLI version against the
// spec-declared agent_version compatibility range (#1743). It returns
// (matched, comparable):
//
//   - comparable is false when no semver token can be extracted from probed
//     (the #1769 "unknown" sentinel, or any version with no dotted-number
//     token) OR the range itself is malformed — the caller degrades to a
//     warn-and-proceed, mirroring semverLT's "dev" degrade.
//   - matched is true when the extracted probed version satisfies EVERY
//     comparator in the range (only meaningful when comparable is true).
//
// This duplicates spec.MatchAgentVersionRange: the runner is a separate Go
// module and cannot import backend/internal/spec, exactly as it carries its
// own semverLT/parseSemver rather than the backend's version package.
func matchAgentVersionRange(rng, probed string) (matched, comparable bool) {
	comps, ok := parseAgentVersionComparators(rng)
	if !ok {
		return false, false
	}
	ver, ok := parseVersionTriple(agentVersionTokenRe.FindString(probed))
	if !ok {
		return false, false
	}
	for _, c := range comps {
		if !c.satisfiedBy(ver) {
			return false, true
		}
	}
	return true, true
}

// runSliceIndex holds the decomposed child's 0-based sub_plan position
// (E24.1 / #1141 / ADR-041), set at runtime from the fetched prompt's
// slice_index. It names the per-child sole-writer slice branch
// fishhawk/run-<parent>/slice-<runSliceIndex>. The runner handles one run
// per process and sets this once during run() startup before any reader
// executes, so package-level storage carries it to the helper functions
// (resolvePolicyBaseRef, resolveImplementBranchRouting) that only receive
// config by value. Defaults to 0, the correct value for slice 0. Only read
// when decomposedFromRunID is non-empty.
var runSliceIndex int

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
			"git_sha":          runnerGitSHA(),
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

	// Capture the operator-provided dispatch working_dir BEFORE the
	// E22.X/#1137 lineage-worktree relocation below overwrites
	// cfg.workingDir with the worktree path. The acceptance preview hook
	// must resolve a RELATIVE FISHHAWK_ACCEPTANCE_PREVIEW_CMD against the
	// operator's dispatch checkout (which carries the untracked .env), not
	// the lineage worktree (also missing .env) nor the runner-inherited
	// fishhawk-mcp cwd (#1746).
	dispatchWorkingDir := cfg.workingDir

	ctx, stop := newRunnerContext()
	defer stop()

	// Resolve the agent CLI binary (operator override or adapter default)
	// and probe its version BEFORE the startup line, so runner_started
	// records the exact binary + version that will be invoked (#1741).
	cfg.agentBinary = effectiveAgentBinary(cfg.agent, os.Getenv)
	cfg.agentVersion = probeAgentVersion(ctx, cfg.agentBinary)

	logStartup(logSink, cfg)
	_, _ = fmt.Fprintf(logSink, `{"event":"coercion_registry","summary":%q}`+"\n", plan.CoercionRegistrySummary())

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

	// fixupExpectedHeadSHA is the fetched prompt's fixup_expected_head_sha
	// (#967): the run's recorded head per the backend's ADR-035 lineage
	// ledger. The pre-invoke fix-up base establishment below fetches +
	// checks out cfg.fixupBranch and fails fast when the fetched tip
	// differs from this value. Empty (older backend / backend-side
	// resolution failure, or no --fetch-prompt) skips the comparison.
	// Function-scoped rather than a cfg field: it is produced and consumed
	// entirely within run().
	var fixupExpectedHeadSHA string

	// implement-model routing (#1013): the backend resolves the implement model
	// and carries it on the prompt response under `implement_model`, the
	// prompt-response decoder threads it onto FetchedPrompt.ImplementModel, and
	// the runner pins it onto agent.Invocation.Model below (the claudecode
	// adapter then appends `--model <m>`). Captured here from the fetch and held
	// for the inv construction. EMPTY (no rung of the ladder supplied a model —
	// the common case) leaves inv.Model unset, so the spawn is byte-identical to
	// today. Function-scoped like fixupExpectedHeadSHA: produced and consumed
	// within run().
	var implementModel string

	// plan-model routing (#1416): the backend resolves the plan model and carries
	// it on the plan-stage prompt response under `plan_model`, the prompt-response
	// decoder threads it onto FetchedPrompt.PlanModel, and the runner pins it onto
	// agent.Invocation.Model below for a plan-type stage (parallel to
	// implementModel). EMPTY (no rung of the ladder supplied a model — the common
	// case) leaves inv.Model unset, so the plan spawn is byte-identical to today.
	// Function-scoped like implementModel: produced and consumed within run().
	var planModel string

	// exemptOpenPR / exemptHeldSHA / exemptHeldBranch carry the operator EXEMPT
	// resolution of a scope-completeness park (#1231) from the fetched prompt to
	// the early zero-re-run branch below. When exemptOpenPR is true the stage's
	// gate-verified commit already sits on exemptHeldBranch at exemptHeldSHA (the
	// park pushed it), so the runner skips the agent, the gates, and CommitAndPush
	// and opens the PR from that exact held commit. Function-scoped like
	// fixupExpectedHeadSHA: produced and consumed entirely within run().
	var (
		exemptOpenPR     bool
		exemptHeldSHA    string
		exemptHeldBranch string
	)

	// bindingAssertions is the fetched prompt's binding_assertions (#1171):
	// the operator-declared deterministic substring checks the runner
	// evaluates against the committed scope-only tree. It is surfaced in the
	// pre-push gate evidence and enforced in openPRAndShipArtifact's
	// verifyCommit closure (category-B before the push on any unsatisfied
	// assertion). Empty (no declaration, older backend, or no --fetch-prompt)
	// makes both a no-op — byte-identical to behavior before #1171.
	// Function-scoped like fixupExpectedHeadSHA: produced and consumed within
	// run().
	var bindingAssertions []upload.BindingAssertion

	// fixupApplyPatches is the fetched prompt's fixup_apply_patches (#1165):
	// the routed concerns' reviewer-emitted unified diffs, non-empty ONLY when
	// every routed concern carries a suggested_patch. The pre-invoke
	// deterministic-apply block below attempts `git apply --3way` of each patch
	// and, on a clean apply that passes the committed-tree verify gate, skips
	// the agent spawn entirely. Empty (non-eligible fix-up, older backend, or
	// no --fetch-prompt) leaves the unchanged agent fix-up path in force.
	// Function-scoped like fixupExpectedHeadSHA: produced and consumed in run().
	var fixupApplyPatches []upload.FixupApplyPatch

	// operatorExemptions is the operator-declared exempt_scope_files set (#1229)
	// delivered via the prompt-response scope_exemptions field. Declared up here
	// (alongside bindingAssertions) because it is set from the prompt at fetch
	// time below, BEFORE the agent loop's var block. It is set ONCE and is NEVER
	// cleared by loadScopeExemptions — unlike validatedExemptions (the agent's
	// consumable #1153 sidecar self-exemptions), so an operator exemption
	// survives the base-rebase re-invoke reset. Merged with validatedExemptions
	// (mergeExemptions) at each openPRAndShipArtifact gate call so the #1151
	// shortfall subtracts the agent∪operator union. Empty on every non-recovery
	// run, keeping the strict gate byte-identical.
	var operatorExemptions []scopeExemption

	// egressTargetHosts / acceptanceCriteriaIDs are the acceptance-stage
	// prompt-response fields (E31.7 / #1535): the spec-declared egress
	// target hosts feed the ADR-050 proxy allow-list; the approved plan's
	// criterion ids are the join-key set the shipped verdict's
	// criteria[].id entries are validated against. Both empty on every
	// non-acceptance stage (the backend serves them only there) and on
	// local replay without --fetch-prompt. Function-scoped like
	// fixupExpectedHeadSHA: produced and consumed within run().
	var (
		egressTargetHosts     []string
		acceptanceCriteriaIDs []string
	)

	// acceptanceExpectedHeadSHA is the run's merge-candidate identity
	// (E31.18 / #1569): the newest reported head SHA the backend resolved
	// from its lineage ledger, served only on acceptance-stage prompt
	// responses. The pre-spawn target-identity gate below compares the
	// declared target's /healthz git_sha against it so acceptance validates
	// the merge candidate, not whatever build answers at the declared host.
	// Empty (older backend, ledger gap, or no --fetch-prompt) degrades the
	// gate to unverifiable-warn. Function-scoped like fixupExpectedHeadSHA:
	// produced and consumed within run().
	var acceptanceExpectedHeadSHA string

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
		path, sType, agentTimeoutSecs, specVerifyCmd, specVerifyTimeoutSecs, specVerifyMaxIterations, decomposedFromRunID, minRunnerVersion, agentVersionRange, agentSelfRetry, maxRetriesSnapshot, retryAttempt, scopeFiles, commitAuthorName, commitAuthorEmail, fixup, fixupBranch, expectedHeadSHA, promptBindingAssertions, applyPatches, sliceIndex, promptScopeExemptions, openPRFromHeldCommit, heldCommitSHA, heldCommitBranch, promptImplementModel, promptPlanModel, promptEgressTargetHosts, promptAcceptanceCriteriaIDs, promptAcceptanceExpectedHeadSHA, fetchErr := fetchPromptToFile(ctx, client, cfg, key, logSink)
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
		// Executor agent-version compatibility check (E32.13 / #1743): when the
		// stage's executor declares an agent_version range and the resolved
		// coding-agent CLI version (the #1769-probed cfg.agentVersion) falls
		// OUTSIDE it, fail the stage LOUDLY pre-spawn (category C) — turning an
		// opaque mid-run CLI-drift break (the 2026-07-08 claude CLI auto-update,
		// #1741) into a one-line diagnosis. An unprobeable/unknown version (no
		// extractable semver token) or a malformed range degrades to a warn and
		// proceeds, mirroring semverLT's "dev" degrade. Absent range = no check.
		if agentVersionRange != "" {
			matched, comparable := matchAgentVersionRange(agentVersionRange, cfg.agentVersion)
			switch {
			case !comparable:
				_, _ = fmt.Fprintf(logSink,
					`{"event":"agent_version_uncomparable","range":%q,"resolved":%q}`+"\n",
					agentVersionRange, cfg.agentVersion)
			case !matched:
				_, _ = fmt.Fprintf(logSink,
					`{"event":"agent_version_mismatch","category":"C","range":%q,"resolved":%q}`+"\n",
					agentVersionRange, cfg.agentVersion)
				return exitFailure
			}
		}
		cfg.promptFile = path
		cfg.decomposedFromRunID = decomposedFromRunID
		runSliceIndex = sliceIndex
		cfg.scopeFiles = scopeFiles
		cfg.commitAuthorName = commitAuthorName
		cfg.commitAuthorEmail = commitAuthorEmail
		cfg.fixup = fixup
		cfg.fixupBranch = fixupBranch
		exemptOpenPR = openPRFromHeldCommit
		exemptHeldSHA = heldCommitSHA
		exemptHeldBranch = heldCommitBranch
		fixupExpectedHeadSHA = expectedHeadSHA
		fixupApplyPatches = applyPatches
		bindingAssertions = promptBindingAssertions
		// Operator scope exemptions (#1229) arrive in the prompt-response, NOT
		// the consumable #1153 sidecar, so they are captured ONCE here and held
		// in operatorExemptions for the whole stage. They MUST survive the
		// base-rebase re-invoke reset (run() ~line 1522) that reloads the
		// agent's self-exemptions from the swept sidecar — hence a separate var
		// re-merged at each openPRAndShipArtifact gate call, never cleared by
		// loadScopeExemptions.
		operatorExemptions = toScopeExemptions(promptScopeExemptions)
		// Backend-resolved implement model (#1013): held for the inv
		// construction below, where it is pinned onto agent.Invocation.Model.
		// Empty (no ladder rung supplied a model) leaves the spawn unchanged.
		implementModel = promptImplementModel
		// Backend-resolved plan model (#1416): held for the inv construction
		// below, where it is pinned onto agent.Invocation.Model for a plan-type
		// stage. Empty (no ladder rung supplied a model) leaves the spawn
		// unchanged.
		planModel = promptPlanModel
		// Acceptance-stage inputs (E31.7 / #1535): held for the acceptance
		// containment branch (proxy allow-list) and the post-agent verdict
		// validation (criteria join-key membership) below. Both empty on
		// every non-acceptance stage.
		egressTargetHosts = promptEgressTargetHosts
		acceptanceCriteriaIDs = promptAcceptanceCriteriaIDs
		acceptanceExpectedHeadSHA = promptAcceptanceExpectedHeadSHA
		stageType = sType
		// Hand the resolved scope.files to the out-of-process CLI
		// auto-PR path (#581) the same way the PR description is
		// handed off via the run/stage-keyed sidecar. Only implement stages
		// carry a scope; a write failure is non-fatal — the CLI falls
		// back to `git add -A` when the file is missing.
		if sType == "implement" && len(scopeFiles) > 0 {
			writeScopeHandoff(cfg, scopeFiles, logSink)
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
		if cfg.verifyMaxIterations == 0 && specVerifyMaxIterations > 0 {
			cfg.verifyMaxIterations = specVerifyMaxIterations
		}
		// ADR-023 self-retry fields — set from prompt response, not CLI flags.
		cfg.agentSelfRetry = agentSelfRetry
		cfg.maxRetriesSnapshot = maxRetriesSnapshot
		cfg.retryAttempt = retryAttempt

		// Per-run working-tree isolation (E22.X / #1137). This is the first
		// point the runner knows decomposedFromRunID, so it is where the
		// lineage worktree is provisioned. Compute the lineage root (parent
		// id for a decomposed child so siblings share a tree, else this run's
		// id), provision/reuse the worktree against the ORIGINAL operator
		// checkout, take the same-lineage lock, and relocate cfg.workingDir
		// into the worktree — every downstream `repoDir := cfg.workingDir`
		// git op then runs in isolation with no further change. The lock is
		// released at stage end via defer.
		root := lineageRoot(cfg.runID, cfg.decomposedFromRunID, cfg.parallelIsolate)
		baseRepoDir := cfg.workingDir
		if baseRepoDir == "" {
			baseRepoDir = "."
		}
		// Cross-lineage worktree-admin safety (#1181, issue option (b)): a
		// sibling run of a DIFFERENT lineage must not interleave its
		// `git worktree add`/`list` with this run's sweep `git worktree
		// remove --force` on the shared gitdir — git gives no
		// cross-invocation mutual-exclusion guarantee, so we serialize the
		// fast sweep+provision critical section ourselves. The lock BLOCKS
		// (cross-lineage concurrency is the feature's expected steady state,
		// not a bug) and is held ONLY here — released the moment provision
		// returns, before the long stage.
		adminRelease, adminErr := acquireWorktreeAdminLock(ctx, baseRepoDir, logSink)
		if adminErr != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"runner_failed","reason":"worktree_admin_lock","detail":%q}`+"\n", adminErr.Error())
			return exitFailure
		}
		// Reclaim worktrees of terminal lineages before provisioning a new
		// one (#1137). Best-effort: it never fails the stage and never
		// removes a worktree whose lineage the backend doesn't report
		// complete.
		sweepTerminalWorktrees(ctx, baseRepoDir, client, logSink)
		wt, provErr := provisionLineageWorktree(ctx, baseRepoDir, root, logSink)
		if provErr != nil {
			adminRelease()
			_, _ = fmt.Fprintf(logSink,
				`{"event":"runner_failed","reason":"worktree_provision","detail":%q}`+"\n", provErr.Error())
			return exitFailure
		}
		// Critical section done — release the cross-lineage admin lock before
		// the long stage; the per-lineage lock below independently guards
		// same-lineage serialization.
		adminRelease()
		// Record the lineage root's FULL run id beside the worktree so a
		// later sweep can resolve the short `run-<root>` dir name back to a
		// run id for the lineage_complete read (best-effort).
		writeLineageRunID(ctx, baseRepoDir, root,
			lineageRootFull(cfg.runID, cfg.decomposedFromRunID, cfg.parallelIsolate), logSink)
		release, lockErr := acquireLineageLock(ctx, baseRepoDir, root, cfg.runID, logSink)
		if lockErr != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"runner_failed","reason":"lineage_lock","detail":%q}`+"\n", lockErr.Error())
			return exitFailure
		}
		defer release()
		cfg.workingDir = wt
	}

	// Operator EXEMPT resolution of a scope-completeness park (#1231): the
	// stage's gate-verified commit already sits on its run branch (the park
	// pushed it; ADR-035 sole-writer), so open the PR from that exact held
	// commit with ZERO agent re-invocation. Handled HERE — before the prompt
	// file is read and the agent.Invocation is wired below — so the agent
	// invoker is provably never spawned on the exempt path (the e2e asserts
	// invoker-called-exactly-once across the park+exempt sequence). No
	// CommitAndPush, no gates, no trace bundle: the commit and its verified
	// tree are unchanged from the park.
	if exemptOpenPR {
		return openHeldCommitPR(ctx, cfg, exemptHeldSHA, exemptHeldBranch, logSink, client, issuedKey)
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

	// From here on the runner's structured stderr stream (logSink) has
	// concurrent producers during the agent invocation: the agent
	// heartbeat goroutine (wired below via inv.ProgressSink) and the
	// mid-stage scope-amendment watcher (#1035). io.Writer carries no
	// concurrency guarantee, so serialize every write behind a mutex —
	// a torn pair of single-line fmt.Fprintf writes would produce a line
	// the fishhawk-mcp relay's JSON scanner rejects. Reassigning the local
	// keeps every existing logSink call site (and ProgressSink) on the
	// guarded writer with no further changes.
	logSink = newSyncWriter(logSink)

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

	// Pin the backend-resolved implement model onto the spawn (#1013). The
	// claudecode adapter appends `--model <inv.Model>` when this is non-empty;
	// empty (no ladder rung supplied a model — the common case) appends no
	// --model flag, byte-identical to today. This single assignment covers
	// every spawn path — primary implement, fix-up dispatch (a fresh run()
	// invocation), the verify-fix re-invoke, and the base-rebase re-invoke —
	// because each derives its agent.Invocation from this inv by value
	// (fixInv := baseInv, reinvokeInv := baseInv), inheriting Model.
	inv.Model = implementModel

	// Pin the backend-resolved plan model onto a plan-stage spawn (#1416). Plan
	// and implement stages are both spawned by the runner from a fetched prompt,
	// so the plan model reuses the same FetchedPrompt -> inv.Model seam. Gated on
	// the plan stage type so the implement path's inv.Model above is untouched;
	// planModel is empty on every non-plan stage and whenever no rung supplied a
	// model, so an empty assignment here is byte-identical to today's spawn.
	if stageType == "plan" {
		inv.Model = planModel
	}

	// Constrain the plan-stage agent's standard_v1 artifact to the canonical
	// schema's SHAPE via the claude CLI --json-schema structured-output flag
	// (#1325), so a schema-invalid plan shape becomes (near-)impossible rather
	// than the auto-retried norm. Populated ONLY for the plan stage (the same
	// gate the post-invoke plan handling uses: a plan-out path on a non-
	// implement/non-review stage); empty stageType (local replay without
	// --fetch-prompt) preserves the historical --plan-out-driven behavior.
	//
	// NOTE: inv.JSONSchema (and the captured Result.StructuredOutput it
	// produces) are honored ONLY by the claudecode backend; on any other
	// backend they are a documented graceful no-op — the field is ignored and
	// StructuredOutput stays nil, so the run falls through to TryCoerce+validate.
	//
	// Graceful degradation (#1325): if the derivation errors (a malformed
	// embedded schema, an unresolvable $ref, or a future unsupported keyword),
	// log and leave inv.JSONSchema empty — proceed UNCONSTRAINED rather than
	// hard-fail the stage. The TryCoerce+validate path remains the fallback.
	if cfg.planOut != "" && stageType != "implement" && stageType != "review" {
		if schema, derr := deriveStructuredOutputSchema(); derr != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"plan_schema_derive_failed","run_id":%q,"detail":%q}`+"\n",
				cfg.runID, derr.Error())
		} else {
			inv.JSONSchema = string(schema)
		}
	}

	// E19.8 / #348: mint a short-lived MCP token for the agent and
	// layer it onto the invocation env. Best-effort — if the token
	// fetch fails we log and continue. The agent loses Fishhawk
	// MCP awareness but the run still produces a valid trace /
	// plan / PR per the rest of the stage flow.
	//
	// mcpBearerToken retains the run-bound fhm_ bearer for the
	// pre-commit scope-amendment refresh (E22.X / #961) — the runner
	// reuses the SAME token the agent's poll loop holds, keeping one
	// agent-side auth path on the amendment surface. Empty when the
	// fetch failed or never ran; the refresh then skips and the
	// original scope stays authoritative.
	mcpBearerToken := ""
	switch {
	case stageType == "acceptance":
		// ADR-050 decision #2: the acceptance agent gets NO Fishhawk MCP
		// token — its verdict ships via the signature-authed evidence
		// upload after the invocation, and withholding FISHHAWK_API_TOKEN
		// removes the credential leg an injected agent could turn against
		// the run's own control plane. The explicit event makes the
		// omission deliberate-and-visible in the stage log.
		_, _ = fmt.Fprintf(logSink,
			`{"event":"acceptance_no_mcp_token","run_id":%q,"stage_id":%q}`+"\n",
			cfg.runID, cfg.stageID)
	case issuedKey != nil:
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
			mcpBearerToken = mcpTok.Token
			_, _ = fmt.Fprintf(logSink,
				`{"event":"mcp_token_issued","run_id":%q,"token_id":%q,"expires_at":%q}`+"\n",
				cfg.runID, mcpTok.TokenID, mcpTok.ExpiresAt.Format(time.RFC3339))
		}
	}

	// Acceptance-agent containment (E31.7 / #1535, ADR-050 / ADR-049 #4).
	// The acceptance agent validates the RUNNING INSTANCE from intent +
	// criteria, never the diff, and is treated as potentially
	// prompt-injected — so before it spawns:
	//
	//   1. WorkingDir moves to a fresh empty temp dir. This is
	//      diff-withholding (ADR-049 #4: the tree must not leak into the
	//      independent validation) plus accidental-write hygiene ONLY —
	//      it does NOT remove repo write access or authority, since a
	//      child process can still write by absolute path. The real
	//      authority boundary is that the invocation env carries no
	//      repo-write credential of any kind (acceptenv denies
	//      GITHUB_TOKEN / GH_TOKEN / FISHHAWK_GITHUB_TOKEN and the
	//      MCP-token block above never runs), the acceptance stage
	//      performs no git add/commit/push and never opens a PR (every
	//      commit/PR path below is stage-gated to implement), and
	//      hostile filesystem writes remain the documented OS-sandbox
	//      residual (ADR-050; runner README residual note; #611-class).
	//   2. The ADR-050 egress proxy starts with the composed allow-list
	//      (spec-declared target hosts + model APIs + backend). A start
	//      error fails the stage category-C BEFORE any agent spawn — the
	//      acceptance agent never runs uncontained.
	//   3. The invocation env is REPLACED with the acceptenv minimized
	//      set (BaseEnv): default-deny essentials + model key +
	//      operator-declared FISHHAWK_ACCEPTANCE_ENV_* passthrough, with
	//      HTTP(S)_PROXY pointed at the proxy. Refused passthrough names
	//      are logged, never honored.
	//   4. The verdict schema constrains claudecode structured output;
	//      other backends fall back to the /tmp/fishhawk-acceptance.json
	//      file transport named in the prompt's output contract.
	if stageType == "acceptance" {
		tmpDir, err := os.MkdirTemp("", "fishhawk-acceptance-*")
		if err != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"runner_failed","reason":"acceptance_workdir","category":"C","detail":%q}`+"\n",
				err.Error())
			return exitFailure
		}
		inv.WorkingDir = tmpDir

		proxy, err := egressproxy.Start(egressproxy.Config{
			AllowHosts: egressproxy.BuildAllowlist(egressTargetHosts, cfg.backendURL),
			Logf: func(format string, args ...any) {
				_, _ = fmt.Fprintf(logSink,
					`{"event":"acceptance_egress","run_id":%q,"detail":%q}`+"\n",
					cfg.runID, fmt.Sprintf(format, args...))
			},
		})
		if err != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"runner_failed","reason":"acceptance_egress_proxy","category":"C","detail":%q}`+"\n",
				err.Error())
			return exitFailure
		}
		defer func() { _ = proxy.Close() }()

		// Pre-spawn target-identity gate + preview provisioning hook
		// (E31.18 / #1569): provision (FISHHAWK_ACCEPTANCE_PREVIEW_CMD) →
		// readiness poll → /healthz git_sha identity check against the
		// backend-resolved merge-candidate head. Stale, unreachable, or a
		// provision failure fails the stage category-C BEFORE any spawn —
		// acceptance must never validate the wrong build. Unverifiable
		// (no build identifier, or no expectation from an older backend)
		// warns and proceeds; no declared hosts skips the gate. The
		// teardown hook is deferred IMMEDIATELY so it runs on every
		// post-provision return — the failure paths right below AND the
		// happy path after the verdict ships (binding approval condition).
		// The probe dials direct from the runner process, not through the
		// egress proxy — the proxy contains the agent, not the runner.
		// A relative FISHHAWK_ACCEPTANCE_PREVIEW_CMD must resolve against
		// the operator's dispatch checkout (which carries .env), not the
		// runner-inherited fishhawk-mcp cwd nor the lineage worktree
		// (#1746). hookDir rides inside the gate config; empty falls back
		// to the runner cwd (os/exec semantics), preserving prior behavior.
		gcfg := previewGateConfigFromEnv()
		gcfg.hookDir = dispatchWorkingDir
		gateTeardown, gateFailReason, gateFailDetail := acceptanceTargetGate(
			ctx, gcfg, egressTargetHosts, acceptanceExpectedHeadSHA, cfg.runID, logSink)
		if gateTeardown != nil {
			defer gateTeardown()
		}
		if gateFailReason != "" {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"runner_failed","reason":%q,"category":"C","detail":%q}`+"\n",
				gateFailReason, gateFailDetail)
			return exitFailure
		}

		baseEnv, refused := acceptenv.Env(os.Environ(), proxy.URL())
		if len(refused) > 0 {
			refusedJSON, _ := json.Marshal(refused)
			_, _ = fmt.Fprintf(logSink,
				`{"event":"acceptance_env_refused","run_id":%q,"names":%s}`+"\n",
				cfg.runID, refusedJSON)
		}
		inv.BaseEnv = baseEnv
		inv.JSONSchema = acceptanceVerdictJSONSchema

		// Clear any stale fallback verdict from a PRIOR run BEFORE the
		// acceptance agent runs. captureAcceptanceVerdict reads the run/stage-
		// keyed path first and falls back to the legacy fixed path
		// (/tmp/fishhawk-acceptance.json) the prompt still names, so the
		// pre-spawn clear MUST cover BOTH (#1777, binding condition 2): without
		// it, an agent that produces neither structured output nor a fresh file
		// would let captureAcceptanceVerdict read and ship a previous run's stale
		// verdict instead of failing acceptance_verdict_missing (#1535 fix-up).
		// Clearing the legacy path can delete a concurrent OLD-PROMPT run's
		// verdict during the deprecation window; that failure is loud (the other
		// run reports verdict-missing) and acceptable — the alternative (shipping
		// a foreign stale verdict) is the bug this defends against. A remove error
		// other than not-exist means we cannot guarantee a clean transport — fail
		// category-C before any agent spawn rather than risk shipping stale
		// evidence.
		for _, path := range []string{
			acceptanceVerdictPath(cfg.runID, cfg.stageID),
			legacyAcceptanceVerdictPath,
		} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				_, _ = fmt.Fprintf(logSink,
					`{"event":"runner_failed","reason":"acceptance_stale_verdict_clear","category":"C","detail":%q}`+"\n",
					err.Error())
				return exitFailure
			}
		}
	}

	invoker, selErr := selectInvoker(cfg.agent, apiKeyForAgent(cfg.agent), agentBinaryOverride(cfg.agent, os.Getenv))
	if selErr != nil {
		// Category-A runner/agent failure: the requested provider maps to
		// no known invoker. Fail fast BEFORE any agent is invoked.
		_, _ = fmt.Fprintf(logSink,
			`{"event":"runner_failed","reason":"agent_select","detail":%q}`+"\n", selErr.Error())
		return exitFailure
	}

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
		// appliedFixup records that the near-deterministic fix-up apply path
		// (#1165) resolved this dispatch: every routed concern's suggested_patch
		// applied cleanly and the committed-tree verify gate passed, so the agent
		// invocation and the downstream verify gates are SKIPPED and the applied
		// working tree flows straight into openPRAndShipArtifact's fix-up push.
		appliedFixup bool
		// verifiedTreeSHA is the tree object hash the committed-tree gates passed
		// against (#960), threaded into openPRAndShipArtifact's pre-push
		// invariant. Hoisted here (was a gate-region local) so the #1165 apply
		// block, which runs its verify gate before the agent loop, can set it.
		verifiedTreeSHA string
		// applyPath is the near-deterministic fix-up apply provenance (#1165):
		// "agent" (default — no apply-list served / agent re-derived), "applied"
		// (deterministic git-apply, no agent), "apply_failed_fellback" (apply-list
		// served, apply/gate failed, reset cleanly, agent ran), or
		// "apply_failed_reset_failed". Hoisted here (was a fixup-dispatch local)
		// so openPRAndShipArtifact's fixup_pushed report can carry it onto the
		// audit entry. Default "agent" for non-fix-up paths, which never reach the
		// fixup_pushed report, so the value is unused there (omitempty drops it).
		applyPath = "agent"
		// fixupApplyEvents carries the #1165 deterministic-apply trace (apply +
		// verify-gate events, plus the scope-only git_diff on a clean apply).
		// Collected before the agent loop but appended to res.Events AFTER it,
		// because the agent loop reassigns res wholesale — on the fallback path
		// that would otherwise discard the apply-attempt trace.
		fixupApplyEvents []agent.Event
		// validatedExemptions is the agent's validated scope self-exemptions
		// (#1153): declared scope.files paths deliberately left unchanged and
		// justified in-band. Read + validated after the agent settles (run()
		// line ~1162), surfaced once to gate_evidence, and threaded into
		// openPRAndShipArtifact so the pre-push scope-completeness gate subtracts
		// them from its missing set. Empty on every non-standalone-implement path
		// (the gate is inert there), keeping the strict gate byte-identical.
		validatedExemptions []scopeExemption
		// sealedExemptions is the agent∪operator exemption set folded into the
		// trace bundle's scope_files_exempted gate_evidence event (#1153/#1218),
		// captured at the single emit site below so it survives the base-rebase
		// re-invoke reset of validatedExemptions. On a re-invoke the reloaded set
		// minus this sealed set is the supplemental delta the post-seal audit row
		// re-surfaces (the bundle already shipped, #742 forward gating).
		sealedExemptions []scopeExemption
	)

	// Capture the operator's HEAD ref BEFORE the agent is invoked (#941).
	// The implement agent shares the operator's checkout; an agent that runs
	// `git checkout -b` mid-stage would move HEAD, and the old capture-at-
	// upload ordering then re-read that agent-moved ref as the restore target,
	// stranding the operator on the agent's branch. Capturing here pins the
	// restore target to where the operator actually started. Gated on the
	// implement stage to mirror the prior capture's effective scope (it lived
	// on the implement push path inside openPRAndShipArtifact). A capture
	// failure must never break the run: emit the diagnostic and skip restore.
	var (
		preAgentRef      string
		preAgentDetached bool
		preAgentCaptured bool
		// preAgentDirty is the pre-agent dirty-paths snapshot (#943),
		// captured alongside the HEAD ref so the post-push drift cleanup can
		// partition CommitAndPush's ScopeDrift into agent-introduced drift
		// (reverted) and operator pre-existing edits (preserved across the
		// HEAD restore). A capture failure disables cleanup for the stage —
		// never revert blind: with no trustworthy pre-agent baseline, every
		// drift path must be treated as potentially operator-owned.
		preAgentDirty         []string
		preAgentDirtyCaptured bool
	)
	if stageType == "implement" {
		repoDir := cfg.workingDir
		if repoDir == "" {
			repoDir = "."
		}
		if origRef, detached, capErr := captureHead(ctx, repoDir); capErr != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"working_tree_capture_failed","run_id":%q,"stage_id":%q,"detail":%q}`+"\n",
				cfg.runID, cfg.stageID, capErr.Error())
		} else {
			preAgentRef = origRef
			preAgentDetached = detached
			preAgentCaptured = true
		}
		if dirty, derr := dirtyPaths(ctx, repoDir); derr != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"working_tree_dirty_capture_failed","run_id":%q,"stage_id":%q,"detail":%q}`+"\n",
				cfg.runID, cfg.stageID, derr.Error())
		} else {
			preAgentDirty = dirty
			preAgentDirtyCaptured = true
		}
		// Pre-invoke scope-justification sweep (#1153): delete any leftover
		// sidecar at THIS run/stage's keyed path before the agent runs — the
		// load-bearing freshness defense that stops a same-keyed leftover from a
		// prior retry of this run/stage bleeding a stale exemption into a fresh
		// attempt. (The keyed path + embedded-id validation are the other two
		// independent defenses.)
		sweepStaleScopeJustification(cfg, logSink)
		// Pre-invoke fix-up self-report sweep (#1210): delete any leftover
		// self-report sidecar at THIS run/stage's keyed path before the agent
		// runs, so a same-keyed leftover from a prior retry of this run/stage
		// cannot bleed a stale claim into a fresh attempt. No-ops on every
		// non-fix-up implement stage (the sidecar is fix-up-only).
		sweepStaleFixupSelfReport(cfg, logSink)
		// Pre-invoke fix-up commit-message sweep (#1572): delete any leftover
		// commit-message sidecar at THIS run/stage's keyed path before the agent
		// runs, so a pass whose agent never re-writes the file falls back to the
		// conventional-shaped fallback rather than reusing the PRIOR pass's
		// message. No-ops on every non-fix-up implement stage (the sidecar is
		// fix-up-only).
		sweepStaleFixupCommitMessage(cfg, logSink)
		// Pre-invoke initial-implement commit-message sweep (#1686): delete any
		// leftover commit-message sidecar at THIS run/stage's keyed path before
		// the agent runs, so a retry whose agent never re-writes the file falls
		// back to today's title + body rather than reusing a PRIOR attempt's
		// message. Fires on both the GHA and local paths (the runner subprocess
		// always invokes the agent, even under --no-pr).
		sweepStaleImplementCommitMessage(cfg, logSink)
		// Pre-invoke PR-description sweep (#1777): delete any leftover PR
		// description at THIS run/stage's keyed path AND at the legacy fixed path
		// before the agent runs, so a stale foreign handoff (a prior retry, or a
		// concurrent run that raced on the shared legacy path) can never be
		// silently parsed into this run's PR title/body — the exact clobber this
		// issue fixes. Covers keyed AND legacy to match loadAgentAuthoredPR's
		// keyed-first-legacy-fallback read (binding condition 2). A remove error
		// other than not-exist means a stale readable file would survive to be
		// parsed downstream — fail category-C here rather than risk the silent
		// clobber, mirroring the acceptance stale-verdict clear.
		if !sweepStalePullRequestDescription(cfg, logSink) {
			return exitFailure
		}
	}

	// Run()-level restore net (#953, the #941 residual): the restore defers
	// inside openPRAndShipArtifact and the fixup block below only cover their
	// own paths, so an implement stage whose agent moved HEAD (e.g. `git
	// checkout -b`) and then FAILED before reaching them stranded the operator
	// on the agent's branch. Installed here — before the fixup defer and the
	// agent invoke — so LIFO ordering makes it fire LAST at run() exit, on
	// every path: success, failure, or panic. Double-fire safe: when an inner
	// defer already restored (or HEAD never moved — the common failure case
	// where the agent only edited files), the re-read below sees HEAD on the
	// captured ref and the defer no-ops. That guard is load-bearing:
	// restoreHead is `git checkout --force`, which discards staged+unstaged
	// tracked modifications, and an unconditional checkout here would destroy
	// the dirty tree the operator inspects after a failure. --no-pr keeps its
	// deliberate leave-the-tree-as-is semantics (the dirty tree IS the
	// deliverable), so the defer is skipped entirely. Note it fires AFTER
	// logCompletion, so the working_tree_restored line lands after the
	// completion log line on failure paths — stderr-only; the trace bundle was
	// already uploaded earlier either way, matching the success-path behavior.
	if stageType == "implement" && preAgentCaptured && !cfg.noPR {
		repoDir := cfg.workingDir
		if repoDir == "" {
			repoDir = "."
		}
		defer func() {
			// A deadline/cancellation-failed stage is exactly a failure path
			// this net targets, and `git checkout` under the already-dead ctx
			// would silently fail — detach from the parent's cancellation and
			// bound the restore with a fresh timeout so a hung git cannot
			// stall runner exit indefinitely.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			curRef, curDetached, capErr := captureHead(rctx, repoDir)
			if capErr != nil {
				// Never run `git checkout --force` blind: destroying
				// uncommitted work is worse than leaving the operator
				// stranded (stranded is recoverable by hand).
				_, _ = fmt.Fprintf(logSink,
					`{"event":"working_tree_restore_failed","run_id":%q,"stage_id":%q,"original_ref":%q,"detached":%t,"detail":%q}`+"\n",
					cfg.runID, cfg.stageID, preAgentRef, preAgentDetached,
					"restore skipped: current HEAD could not be determined: "+capErr.Error())
				return
			}
			if curRef == preAgentRef && curDetached == preAgentDetached {
				// HEAD never moved, or an inner defer already restored it.
				return
			}
			if rerr := restoreHead(rctx, repoDir, preAgentRef); rerr != nil {
				_, _ = fmt.Fprintf(logSink,
					`{"event":"working_tree_restore_failed","run_id":%q,"stage_id":%q,"original_ref":%q,"detached":%t,"detail":%q}`+"\n",
					cfg.runID, cfg.stageID, preAgentRef, preAgentDetached, rerr.Error())
				return
			}
			_, _ = fmt.Fprintf(logSink,
				`{"event":"working_tree_restored","run_id":%q,"stage_id":%q,"original_ref":%q,"detached":%t}`+"\n",
				cfg.runID, cfg.stageID, preAgentRef, preAgentDetached)
		}()
	}

	// Fix-up base establishment (#967): a fix-up pass must run against the
	// run's PR branch, not the operator's incidental checkout — an operator
	// tree left on main makes the agent's edits a silent no-op that burns a
	// fix-up pass. Before invoking the agent, fetch + checkout the PR branch
	// and verify the fetched tip against the backend-advertised recorded
	// head (ADR-035 lineage): a mismatch means the branch tip is not the
	// stage's recorded head, so FAIL FAST before any agent invocation. An
	// empty expected SHA (older backend, or backend-side resolution failure)
	// proceeds with checkout only. The restore defer below puts the operator
	// back on their original ref even on agent-failure paths that never reach
	// openPRAndShipArtifact's restore defer (#911 family); on the success
	// path both defers fire — the second is a harmless same-ref checkout.
	// Either way this defer restores BEFORE the #953 stage-wide net installed
	// above it (LIFO), whose moved-HEAD guard then sees HEAD already back on
	// the captured ref and no-ops — double-fire safe by construction.
	if stageType == "implement" && cfg.fixup {
		repoDir := cfg.workingDir
		if repoDir == "" {
			repoDir = "."
		}
		fixupCheckoutMoved := false
		if preAgentCaptured {
			defer func() {
				if !fixupCheckoutMoved {
					return
				}
				if rerr := restoreHead(ctx, repoDir, preAgentRef); rerr != nil {
					_, _ = fmt.Fprintf(logSink,
						`{"event":"working_tree_restore_failed","run_id":%q,"stage_id":%q,"original_ref":%q,"detached":%t,"detail":%q}`+"\n",
						cfg.runID, cfg.stageID, preAgentRef, preAgentDetached, rerr.Error())
					return
				}
				_, _ = fmt.Fprintf(logSink,
					`{"event":"working_tree_restored","run_id":%q,"stage_id":%q,"original_ref":%q,"detached":%t}`+"\n",
					cfg.runID, cfg.stageID, preAgentRef, preAgentDetached)
			}()
		}
		tipSHA, coErr := checkoutFixupBase(ctx, repoDir, gitops.DefaultRemote, cfg.fixupBranch)
		if coErr != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"runner_failed","reason":%q,"detail":%q}`+"\n", fixupCheckoutFailReason(coErr), coErr.Error())
			return exitFailure
		}
		fixupCheckoutMoved = true
		if fixupExpectedHeadSHA != "" && tipSHA != fixupExpectedHeadSHA {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"fixup_base_mismatch","run_id":%q,"stage_id":%q,"branch":%q,"fetched_tip_sha":%q,"expected_head_sha":%q}`+"\n",
				cfg.runID, cfg.stageID, cfg.fixupBranch, tipSHA, fixupExpectedHeadSHA)
			_, _ = fmt.Fprintf(logSink,
				`{"event":"runner_failed","reason":"fixup_base_mismatch","detail":%q}`+"\n",
				fmt.Sprintf("run branch %s tip %s does not match the stage's recorded head %s (ADR-035): the branch carries commits the run did not report, or the recorded head was never pushed",
					cfg.fixupBranch, tipSHA, fixupExpectedHeadSHA))
			return exitFailure
		}
		if fixupExpectedHeadSHA == "" {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"fixup_expected_head_missing","run_id":%q,"stage_id":%q,"branch":%q,"detail":"backend advertised no fixup_expected_head_sha; proceeding with checkout only (no lineage comparison)"}`+"\n",
				cfg.runID, cfg.stageID, cfg.fixupBranch)
		}
		_, _ = fmt.Fprintf(logSink,
			`{"event":"fixup_base_established","run_id":%q,"stage_id":%q,"branch":%q,"head_sha":%q,"original_ref":%q}`+"\n",
			cfg.runID, cfg.stageID, cfg.fixupBranch, tipSHA, preAgentRef)

		// Near-deterministic fix-up apply (#1165): when the backend served an
		// apply-list (every routed concern carried a suggested_patch) AND a
		// committed-tree verify gate is configured, try git-applying the patches
		// onto the freshly-checked-out PR branch instead of spawning the agent.
		// On a clean apply that passes the gate, the agent invocation and the
		// downstream verify gates are skipped and the applied tree flows into the
		// existing fix-up commit/push path. On ANY apply/verify failure the
		// worktree is reset to the branch tip (tipSHA) and the unchanged agent
		// path runs. The default "agent" stands when no apply-list was served (a
		// non-mechanical / mixed fix-up) or no verify gate is configured (we
		// cannot confirm the apply, so we conservatively re-derive with the
		// agent). Provenance is recorded in the trace bundle below and (since
		// #1213) on the fixup_pushed audit entry via the hoisted applyPath.
		if len(fixupApplyPatches) > 0 && cfg.verifyCmd != "" && !cfg.noPR {
			ok, tree, evs, ap := attemptDeterministicFixup(ctx, cfg, fixupApplyPatches, tipSHA, logSink)
			fixupApplyEvents = evs
			applyPath = ap
			if ap == "apply_failed_reset_failed" {
				// The post-failure worktree reset failed, so the tree may be
				// half-applied. Falling through to the agent here would commit and
				// push a corrupted tree — fail the stage loud instead (the binding
				// condition: never ship a half-applied tree). The fixup-checkout
				// restore defer above still puts the operator back on their original
				// ref via a forced checkout on the way out. The specific reset error
				// is already on the trace as fixup_apply_reset_failed.
				_, _ = fmt.Fprintf(logSink,
					`{"event":"runner_failed","reason":"fixup_apply_reset_failed","detail":%q}`+"\n",
					"worktree reset after a failed deterministic apply did not succeed; refusing to run the agent on a possibly half-applied tree")
				return exitFailure
			}
			if ok {
				appliedFixup = true
				verifiedTreeSHA = tree
				res.OK = true
				// Emit the reconciled scope-only diff so the implement re-review
				// and policy re-eval see the applied change — the agent path emits
				// this from inside the invoke loop, which the apply path skips.
				// Gated on checkBaseRef like the agent path's git_diff emission.
				if cfg.checkBaseRef != "" {
					fixupApplyEvents = append(fixupApplyEvents, reemitScopedGitDiff(cfg, logSink)...)
				}
				_, _ = fmt.Fprintf(logSink,
					`{"event":"fixup_applied_deterministically","run_id":%q,"stage_id":%q,"patch_count":%d,"verified_tree_sha":%q}`+"\n",
					cfg.runID, cfg.stageID, len(fixupApplyPatches), tree)
			}
		}
		// Record the fix-up apply provenance (#1165) in the trace bundle for every
		// fix-up dispatch — "applied" (deterministic git-apply, no agent), "agent"
		// (no apply-list served, the agent re-derived), or "apply_failed_fellback"
		// (an apply-list was served but reset + the agent ran). Since #1213 the same
		// runtime value is ALSO threaded onto the fixup_pushed /pull-request report
		// (openPRAndShipArtifact's ShipPullRequestArgs.ApplyPath) and persisted onto
		// the fixup_pushed AUDIT entry by succeedFixupPushStage — the backend's
		// pullRequestBody now declares apply_path as an omitempty field, so the
		// DisallowUnknownFields decoder accepts it. The trace bundle event remains
		// the in-trace provenance for the apply-attempt timeline.
		fixupApplyEvents = append(fixupApplyEvents, agent.Event{
			Kind:    "fixup_apply_path",
			Payload: agent.MakePayload(map[string]any{"apply_path": applyPath}),
		})
	}

	// Child wave-base establishment (#1302, supersedes the dormant #1036 /
	// ADR-041 slice-branch keying): a dependent (wave-N) decomposition slice
	// must run against its predecessors' INTEGRATED tree, not the operator's
	// ambient HEAD. The wave loop (fishhawk_run_children) merges each settled
	// wave onto the consolidated branch and re-dispatches the next wave with
	// --base-branch / --check-base-ref=<consolidated>; for wave 0 (and every
	// independent fan-out) that resolved base is main. So fetch + check out the
	// resolved WAVE base ref (cfg.checkBaseRef) into the worktree BEFORE the
	// agent is invoked — then a slice referencing a predecessor's new symbols
	// sees them and can compile, instead of correctly writing nothing and
	// failing child_no_changes (#1302). The prior block keyed this checkout on
	// the child's OWN sole-writer slice branch (childSliceBranch), which is
	// minted once and so NEVER pre-exists on the remote: remoteBranchExists was
	// always false and the block silently no-oped in production — the exact
	// #1302 defect. The remoteBranchExists guard is retained so an absent base
	// (a never-pushed main, or an empty consolidated when GitHub is not wired)
	// gracefully skips and falls through to today's ambient-HEAD behavior. This
	// base now AGREES with the commit-time branch cut (resolveImplementBranch-
	// Routing's freshFetchBase = baseRef) and the policy diff (decomposedPolicy-
	// Base → cfg.checkBaseRef), so the agent's view, the policy-diff base, and
	// the push base are one wave base. The restore defer fires BEFORE the #953
	// stage-wide net (LIFO), whose moved-HEAD guard then sees HEAD already
	// restored and no-ops — the same double-fire-safe construction as the fixup
	// block above.
	//
	// The guard is the REMOTE-AUTHORITATIVE remoteHasBranch (git ls-remote),
	// NOT the local-tracking remoteBranchExists (git show-ref) the #1302 block
	// originally used (#1363). The consolidated wave base is created on GitHub
	// via the API during integrate-wave and is NEVER fetched into the local
	// runner's tracking refs, so show-ref ALWAYS returned false for it: the
	// whole block silently no-oped and a dependent slice ran against ambient
	// HEAD (plain main) — exactly the #1363 symptom. ls-remote queries the
	// remote directly, so the just-created base is detected immediately.
	// remoteHasBranch returns (exists, error): a genuine branch-ABSENCE
	// (ls-remote succeeded, empty output) gracefully skips and falls through to
	// today's ambient-HEAD behavior (a never-pushed main, or an empty
	// consolidated when GitHub is not wired — the #1302 degrade contract).
	// A remote-query FAILURE (ls-remote errored) is then classified by
	// remoteConfigured: against a CONFIGURED remote it is a transient failure
	// (network/auth/transient) on a child that HAS an expected base, which must
	// NOT silently degrade to skip-and-run-against-ambient-HEAD — that
	// reintroduces the #1363 symptom — so it fails loud at child_base_checkout;
	// against an UNCONFIGURED remote (a bare local-runner checkout with no
	// origin) it IS the GitHub-not-wired degrade state and skips like an absence.
	if stageType == "implement" && cfg.decomposedFromRunID != "" && !cfg.fixup {
		repoDir := cfg.workingDir
		if repoDir == "" {
			repoDir = "."
		}
		baseRef := cfg.checkBaseRef
		if baseRef == "" {
			baseRef = resolveImplementBaseRef(cfg)
		}
		baseExists, rhErr := remoteHasBranch(ctx, repoDir, gitops.DefaultRemote, baseRef)
		if rhErr != nil {
			// A remote-query FAILURE splits two ways. Against a CONFIGURED remote
			// it is a genuine transient failure (network/auth/SSH-agent drop) on a
			// child with an expected wave base — fail loud rather than silently
			// degrade to ambient HEAD (#1363). But against a remote that is NOT
			// configured at all — a bare local-runner checkout with no origin —
			// the failure is the "GitHub not wired" degrade state (#1302), which
			// must gracefully skip to ambient HEAD exactly like a genuine
			// branch-absence (false, nil). remoteConfigured is the discriminator.
			if remoteConfigured(ctx, repoDir, gitops.DefaultRemote) {
				_, _ = fmt.Fprintf(logSink,
					`{"event":"runner_failed","reason":"child_base_checkout","detail":%q}`+"\n", rhErr.Error())
				return exitFailure
			}
			baseExists = false
		}
		if baseExists {
			childCheckoutMoved := false
			if preAgentCaptured {
				defer func() {
					if !childCheckoutMoved {
						return
					}
					if rerr := restoreHead(ctx, repoDir, preAgentRef); rerr != nil {
						_, _ = fmt.Fprintf(logSink,
							`{"event":"working_tree_restore_failed","run_id":%q,"stage_id":%q,"original_ref":%q,"detached":%t,"detail":%q}`+"\n",
							cfg.runID, cfg.stageID, preAgentRef, preAgentDetached, rerr.Error())
						return
					}
					_, _ = fmt.Fprintf(logSink,
						`{"event":"working_tree_restored","run_id":%q,"stage_id":%q,"original_ref":%q,"detached":%t}`+"\n",
						cfg.runID, cfg.stageID, preAgentRef, preAgentDetached)
				}()
			}
			tipSHA, coErr := checkoutChildBase(ctx, repoDir, gitops.DefaultRemote, baseRef)
			if coErr != nil {
				_, _ = fmt.Fprintf(logSink,
					`{"event":"runner_failed","reason":"child_base_checkout","detail":%q}`+"\n", coErr.Error())
				return exitFailure
			}
			childCheckoutMoved = true
			_, _ = fmt.Fprintf(logSink,
				`{"event":"child_base_established","run_id":%q,"stage_id":%q,"branch":%q,"head_sha":%q,"original_ref":%q}`+"\n",
				cfg.runID, cfg.stageID, baseRef, tipSHA, preAgentRef)
		}
	}

	// Mid-stage scope-amendment watcher (#1035): for implement stages,
	// emit an in-band scope_amendment_pending event the moment the agent
	// files a request, so the fishhawk_run_stage relay surfaces the
	// actionable amendment id + paths to an operator who can decide it from
	// a second session via fishhawk_decide_scope_amendment while the agent
	// blocks on its own ?wait long-poll. Best-effort and implement-only —
	// the guard inside watchScopeAmendments no-ops on a nil client, an empty
	// mcpBearerToken, or a non-implement stage. Stopped right after the
	// invoke loop (ctx cancel + WaitGroup join) so it never races the
	// post-invoke logSink writes.
	watchCtx, stopWatch := context.WithCancel(ctx)
	var watchWG sync.WaitGroup
	watchWG.Add(1)
	go func() {
		defer watchWG.Done()
		watchScopeAmendments(watchCtx, client, cfg, mcpBearerToken, stageType, logSink)
	}()

	invokeStart := time.Now()
	// When appliedFixup is set, the near-deterministic fix-up apply (#1165)
	// already produced the change and passed the committed-tree verify gate
	// (res.OK was set true above), so the agent invocation and its self-retry
	// loop are skipped entirely — the applied working tree flows straight into
	// the fix-up push. appliedFixup never changes inside the loop, so guarding
	// the loop condition is equivalent to an early break on the first iteration.
	for !appliedFixup {
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
			// Adoption precedence (#1325): clarification(file) > structured_output
			// > agent-written file, then the existing TryCoerce+validate gate.
			if isClarif, ev := detectClarificationRequest(cfg.planOut); isClarif {
				// (1) The planner parked: it emitted a clarification_request sibling
				// (#1057) instead of a plan because the issue is not yet
				// plannable. Clarification ALWAYS wins — ignore any
				// structured_output the (schema-constrained) invocation may also
				// have produced. It is NOT a plan, so skip plan-schema validation
				// (which would wrongly demote it to category-B) and leave res.OK
				// true. uploadPlan ships the bytes as-is; the backend ingests the
				// sibling, persists it, and parks the stage at awaiting_input.
				res.Events = append(res.Events, ev)
			} else {
				// (2) structured_output present: overwrite the plan-out file with
				// the schema-guaranteed bytes so uploadPlan ships them and the
				// validate gate runs against them (now rarely tripped). Best-effort
				// — a write failure logs via the returned event and leaves the
				// agent-written file in place, degrading to branch (3).
				if len(res.StructuredOutput) > 0 {
					res.Events = append(res.Events, adoptStructuredOutput(cfg.planOut, res.StructuredOutput))
				}
				// (3) Run the existing TryCoerce+validate path against whatever now
				// sits at cfg.planOut (the adopted structured_output, or the
				// agent-written file when structured_output was absent).
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
		//
		// Single-shot WORKING-TREE gate. It runs ONLY on the paths that have
		// no committed scope-only tree to gate: plan/non-implement stages and
		// --no-pr implement runs (which keep the dirty working tree). Every
		// implement push (stageType=="implement" && !cfg.noPR) is now owned by
		// a committed-tree gate below instead — the verify-fix loop (#651) when
		// verifyMaxIterations>0, or the single-shot committed gate (#802) when
		// verifyMaxIterations==0. Firing here too on those paths would double-run
		// the command and demote inside the ADR-023 self-retry loop (the very
		// compounding c2 forbids).
		if res.OK && cfg.verifyCmd != "" &&
			(stageType != "implement" || cfg.noPR) {
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

	// Stop the mid-stage scope-amendment watcher before the post-invoke
	// phase resumes writing to logSink on the main goroutine, so the two
	// never write concurrently (#1035).
	stopWatch()
	watchWG.Wait()

	// Fold the #1165 deterministic-apply trace into the bundle. Appended here
	// (not before the loop) because the agent loop reassigns res wholesale, so
	// on the fallback path an earlier append would be discarded; on the applied
	// path the loop broke without touching res, so these are the only stage
	// events. No-op (nil) on every non-apply dispatch.
	if len(fixupApplyEvents) > 0 {
		res.Events = append(res.Events, fixupApplyEvents...)
	}

	// Acceptance verdict capture + validation (E31.7 / #1535). On a
	// successful acceptance invocation, capture the structured verdict
	// (StructuredOutput preferred, /tmp/fishhawk-acceptance.json file
	// fallback), validate it against the backend-mirrored rules + the
	// served criteria-id set, and redact it BEFORE it is embedded in the
	// trace bundle (acceptance_evidence event) or shipped. A missing or
	// invalid verdict demotes to category-B: the agent ran but did not
	// produce the contracted artifact. A VALID verdict of "failed" is NOT
	// a runner failure — res.OK stays true; the validation completed and
	// routing the failure is E31.8's scope. acceptanceVerdictRedacted
	// carries the redacted bytes to the post-trace ShipAcceptance below.
	var acceptanceVerdictRedacted []byte
	if stageType == "acceptance" && res.OK {
		// Shared runner-log seam for the capture (legacy-path deprecation) and
		// validate (coercion) events.
		acceptanceWarn := func(event, detail string) {
			_, _ = fmt.Fprintf(logSink,
				`{"event":%q,"run_id":%q,"stage_id":%q,"detail":%q}`+"\n",
				event, cfg.runID, cfg.stageID, detail)
		}
		rawVerdict, capErr := captureAcceptanceVerdict(res,
			acceptanceVerdictPath(cfg.runID, cfg.stageID), legacyAcceptanceVerdictPath, acceptanceWarn)
		if capErr != nil {
			res.OK = false
			res.FailureCategory = "B"
			res.FailureReason = "acceptance_verdict_missing: " + capErr.Error()
			invokeErr = capErr
			_, _ = fmt.Fprintf(logSink,
				`{"event":"acceptance_verdict_missing","run_id":%q,"stage_id":%q,"detail":%q}`+"\n",
				cfg.runID, cfg.stageID, capErr.Error())
		} else if coercedVerdict, valErr := validateAcceptanceVerdict(rawVerdict, acceptanceCriteriaIDs,
			acceptanceWarn); valErr != nil {
			res.OK = false
			res.FailureCategory = "B"
			res.FailureReason = "acceptance_verdict_invalid: " + valErr.Error()
			invokeErr = valErr
			_, _ = fmt.Fprintf(logSink,
				`{"event":"acceptance_verdict_invalid","run_id":%q,"stage_id":%q,"detail":%q}`+"\n",
				cfg.runID, cfg.stageID, valErr.Error())
		} else {
			// Use the normalized (possibly coerced) verdict bytes for redaction
			// and ship so downstream carries the canonical shape.
			rawVerdict = coercedVerdict
			redacted, hits := redactAcceptanceVerdict(rawVerdict)
			acceptanceVerdictRedacted = redacted
			if len(hits) > 0 {
				hitsJSON, _ := json.Marshal(hits)
				_, _ = fmt.Fprintf(logSink,
					`{"event":"acceptance_verdict_redacted","run_id":%q,"stage_id":%q,"hits":%s}`+"\n",
					cfg.runID, cfg.stageID, hitsJSON)
			}
			// Appended before EITHER PackBytes below so both bundle
			// variants carry the evidence event; pre-redacted because
			// consumers dispatch on the raw variant too (the same posture
			// as composeGateEvidence, #963/#793).
			res.Events = append(res.Events, composeAcceptanceEvidence(redacted))
		}
	}

	// Mid-stage scope-amendment refresh (E22.X / #961). Immediately before
	// the commit phase — and crucially BEFORE the committed-tree verify
	// gates and every StageScoped call below — fetch the run's scope
	// amendments with the retained fhm_ bearer and fold APPROVED paths
	// into cfg.scopeFiles. Every downstream consumer (runVerifyFixLoop,
	// runVerifyGateCommitted, gateScopeFiles, CommitAndPush.ScopeFiles)
	// derives from cfg.scopeFiles, so the #960 invariant holds: the gates
	// verify the same folded tree that is pushed, and the #818/#825
	// created-out-of-scope gate honors approved creates while staying
	// fail-loud for anything NOT requested. Best-effort (ADR-021): a
	// refresh failure logs and proceeds with the unamended scope.
	//
	// Capture whether the fold added any new scope path (length-delta):
	// refreshScopeAmendments only GROWS cfg.scopeFiles (appends deduped
	// approved paths, returns early with no mutation when nothing is added),
	// so `len(cfg.scopeFiles) > before` is a sound fold signal. amendmentsFolded
	// broadens the git_diff re-emit gate below (#1660) so a folded-and-committed
	// path is not left out of the last git_diff when the verify-fix loop does
	// NOT reinvoke the agent.
	amendmentsFolded := false
	if res.OK && stageType == "implement" && !cfg.noPR {
		before := len(cfg.scopeFiles)
		res.Events = append(res.Events, refreshScopeAmendments(ctx, client, &cfg, mcpBearerToken, logSink)...)
		amendmentsFolded = len(cfg.scopeFiles) > before
	}

	// Committed-tree verify-fix loop (#651). On the implement push path,
	// when executor.verify.max_iterations > 0, run the verify command
	// against the isolated committed SCOPE-ONLY tree (the same drift-
	// excluded HEAD the #728/#800 compile+test gate checks) and, on
	// failure with budget remaining, feed the captured output back into a
	// fresh agent invocation and re-verify — a bounded evaluator-optimizer
	// loop that converges to ONE commit before the PR opens.
	//
	// This lives OUTSIDE and AFTER the ADR-023 self-retry for{} loop by
	// construction: a verify-fix exhaustion is TERMINAL (it can never reach
	// the `continue` that calls client.RetryStage), so the total verify-fix
	// agent invocations are capped at max_iterations with no multiplicative
	// compounding (DECISION c2). It is placed BEFORE EmitStage so the span's
	// token/model counts include the fix-loop invocations' cost (ADR-030).
	reinvoked := false
	// verifiedTreeSHA is declared in the function-scope var block above (hoisted
	// for the #1165 apply block). The #960 contract is unchanged: empty when no
	// gate ran (plan stage, --no-pr, verifyCmd unset, skip). The deterministic
	// apply path (appliedFixup) already ran its gate and set it, so the two
	// committed-tree gates below are skipped for that path.
	if res.OK && !appliedFixup && stageType == "implement" && !cfg.noPR &&
		cfg.verifyCmd != "" && cfg.verifyMaxIterations > 0 {
		// A POST-commit reset failure (#816) is fatal: the throwaway commit is
		// still on HEAD, so the stage must NOT proceed to the real push. Demote
		// to category-B (park for re-scope/re-plan; no self-retry), mirroring
		// runVerifyGateCommitted's treatment of its post-commit reset failure.
		var ferr error
		reinvoked, verifiedTreeSHA, ferr = runVerifyFixLoop(ctx, cfg, invoker, inv, &res, logSink)
		if ferr != nil {
			res.OK = false
			res.FailureCategory = "B"
			res.FailureReason = ferr.Error()
			invokeErr = ferr
		}
	}

	// Re-emit a fresh scope-only git_diff event when the last emitted git_diff is
	// stale relative to the tree CommitAndPush will commit. computeAndEmitDiff
	// emitted the FIRST git_diff before both the amendment fold and the verify-fix
	// loop, from the pre-reconcile tree, so it goes stale on two triggers:
	//   1. a verify-fix reinvoke (#870) rewrites in-scope files; and
	//   2. an amendment fold (#1660) — refreshScopeAmendments folds an
	//      operator-approved mid-stage scope path into cfg.scopeFiles AFTER
	//      computeAndEmitDiff ran, and CommitAndPush stages that folded scope, so
	//      the folded-and-committed path is absent from the first git_diff when
	//      verify passes first-iteration (no reinvoke).
	// Re-emitting the reconciled scope-only diff here — and ExtractDiff's
	// last-write-wins on the backend — keeps the implement review, the policy
	// re-eval, and the deterministic operator_scope_path_undelivered check bound
	// to the committed scope-only tree the PR ships. Gated on (reinvoked ||
	// amendmentsFolded), so the no-fold no-reinvoke path and the maxIterations==0
	// single-shot gate (#802) still emit exactly one git_diff. checkBaseRef must be
	// set too — it is the same condition that gated computeAndEmitDiff's original
	// git_diff above, so without it there is no first event to supersede.
	if res.OK && stageType == "implement" && !cfg.noPR && (reinvoked || amendmentsFolded) && cfg.checkBaseRef != "" {
		res.Events = append(res.Events, reemitScopedGitDiff(cfg, logSink)...)
	}

	// Committed-tree single-shot verify gate (#802). On the implement push
	// path with executor.verify.max_iterations == 0, run the configured
	// verify command ONCE against the isolated committed SCOPE-ONLY tree —
	// the language-agnostic single-shot twin of the #728/#800 Go gate. This
	// catches a drift-excluded test failure (#780/#776) for ANY language
	// without the fix-loop cost; the older max_iterations==0 path ran against
	// the dirty working tree and false-greened that class. A failure blocks
	// as category B (artifact broken → park for re-scope/re-plan; category B
	// does not self-retry, consistent with the operator opting OUT of the fix
	// loop at max_iterations==0). Placed here — a sibling of runVerifyFixLoop,
	// OUTSIDE the ADR-023 self-retry for{} loop and BEFORE EmitStage so the
	// throwaway-commit work is reflected and the demotion happens before
	// openPRAndShipArtifact. The three verify guards now partition the
	// cfg.verifyCmd!="" space with no overlap: working-tree in-loop gate =
	// (plan || --no-pr); this committed single-shot gate = implement &&
	// !noPR && maxIter==0; fix loop = implement && !noPR && maxIter>0.
	if res.OK && !appliedFixup && stageType == "implement" && !cfg.noPR &&
		cfg.verifyCmd != "" && cfg.verifyMaxIterations == 0 {
		evs, tree, demote := runVerifyGateCommitted(ctx, cfg, logSink)
		res.Events = append(res.Events, evs...)
		verifiedTreeSHA = tree
		if demote != nil {
			res.OK = false
			res.FailureCategory = "B"
			res.FailureReason = demote.Error()
			invokeErr = demote
		}
	}

	// Binding-assertion gate evidence (#1171). The authoritative gate runs in
	// openPRAndShipArtifact's verifyCommit closure (category-B before the
	// push), but that closure executes AFTER the trace bundle is shipped, so a
	// gate_evidence event must be emitted HERE, before composeGateEvidence
	// packs the bundle, for the implement review to see which binding
	// assertions were declared and whether the committed tree satisfied them.
	// We evaluate against verifiedTreeSHA — the same tree object the
	// committed-tree gate verified and the #960 invariant proves equal to the
	// pushed commit's tree (`git show <tree>:<path>` accepts a tree-ish), so
	// this evidence reflects exactly what the push will carry. Guarded to the
	// standalone open-PR implement path (not fix-ups or decomposed children,
	// matching the enforcement gate) and skipped when no tree was verified
	// (verifyCmd unset/skip) — evidence is best-effort, the verifyCommit gate
	// is the enforcement of record. EVIDENCE ONLY: it never demotes res.OK, so
	// it cannot bypass the explicit verifyCommit→reportPullRequestFailure
	// category-B report path.
	if res.OK && stageType == "implement" && !cfg.noPR && cfg.decomposedFromRunID == "" && !cfg.fixup &&
		verifiedTreeSHA != "" && len(bindingAssertions) > 0 {
		repoDir := cfg.workingDir
		if repoDir == "" {
			repoDir = "."
		}
		if results, berr := gitops.EvaluateBindingAssertions(ctx, repoDir, verifiedTreeSHA, toGitopsBindingAssertions(bindingAssertions)); berr == nil {
			res.Events = append(res.Events, bindingAssertionEvidenceEvent(results))
		}
	}

	// Scope self-exempt read + validate (#1153). On the standalone open-PR
	// implement path — the only path the pre-push scope-completeness gate
	// (#1151/#1154) runs on — read the agent's run/stage-keyed justification
	// sidecar, validate freshness fail-closed, and surface the validated
	// exemptions BOTH to the audit/review (a single scope_files_exempted event
	// appended to res.Events here, BEFORE composeGateEvidence folds it into
	// gate_evidence) AND to the gate (threaded into openPRAndShipArtifact as the
	// last param). Emitted EXACTLY ONCE here so the gate_evidence fold counts it
	// once regardless of whether the gate later passes (all-exempted) or fails
	// (partial); the gate site never re-emits. Same standalone guard as the
	// binding-assertion evidence block above (gate excludes fix-ups + decomposed
	// children). loadScopeExemptions consumes (deletes) the sidecar.
	if res.OK && stageType == "implement" && !cfg.noPR && cfg.decomposedFromRunID == "" && !cfg.fixup {
		validatedExemptions = loadScopeExemptions(cfg, scopePaths(cfg.scopeFiles), logSink)
		// Surface BOTH the agent's self-exemptions (#1153) and the operator's
		// exempt_scope_files exemptions (#1229) through the single
		// scope_files_exempted event so gate_evidence/the review see the full
		// agent∪operator set the gate will subtract. operatorExemptions came
		// from the prompt at fetch time. Emitted EXACTLY ONCE here (binding
		// condition 1) — the gate site never re-emits, and the base-rebase
		// re-invoke reuses the already-shipped event.
		emitExemptions := mergeExemptions(validatedExemptions, operatorExemptions)
		// Capture the bundle-sealed set so the base-rebase re-invoke (#1218) can
		// compute the delta the sealed scope_files_exempted event did NOT carry.
		// Set unconditionally (even when empty) so a re-invoke that newly justifies
		// a path against a zero-exemption first attempt surfaces the full delta.
		sealedExemptions = emitExemptions
		if len(emitExemptions) > 0 {
			res.Events = append(res.Events, scopeFilesExemptedEvent(cfg, emitExemptions))
		}
	}

	// Fix-up self-report divergence (#1210). On a fix-up pass — the complement
	// of the scope self-exempt block's `!cfg.fixup` guard — read the agent's
	// structured self-report sidecar (loadFixupSelfReport always consumes/sweeps
	// it, even when no comparison is possible), compute the committed-tree verify
	// outcome the runner ALREADY computed for this stage (the same value
	// composeGateEvidence digests), and on a determinate disagreement append an
	// ADVISORY fixup_selfreport_divergence event for the implement review to
	// arbitrate. Placed AFTER the verify gates (runVerifyFixLoop /
	// runVerifyGateCommitted) leave their events on res.Events and BEFORE
	// composeGateEvidence folds it into gate_evidence. EVIDENCE ONLY: this block
	// NEVER touches res.OK / res.FailureCategory / budget (that would reintroduce
	// the #1150 budget-wedge family).
	if stageType == "implement" && cfg.fixup {
		claimed := loadFixupSelfReport(cfg, logSink)
		actual := terminalVerifyOutcome(res.Events)
		if fixupSelfReportDiverges(claimed, actual) {
			res.Events = append(res.Events, fixupSelfReportDivergenceEvent(cfg, claimed, actual))
		}
	}

	// Emit the GenAI observability span for this stage as soon as the
	// agent invocation (and any self-retries) settled — before bundle
	// packing / upload, so a downstream upload failure doesn't lose
	// the model-call span. Token counts + model come from the
	// aggregated Result; the span carries an estimated cost via
	// pricing.Cost. No-op when OTel emission is gated off. The committed-
	// tree verify-fix loop above (#651) runs before this point so its
	// fix-iteration token usage is folded into res and captured here.
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
	//
	// A fix-up pass (#762) commits onto the EXISTING PR branch and updates
	// the open PR rather than opening a new one, so it is NOT a
	// push_and_open_pr stage: willOpenPR stays false. The fix-up path is now
	// forward-gated on its own flag (willPushFixup / push_fixup, #794) and
	// ships a {outcome:"fixup_pushed"} /pull-request report after the push.
	willOpenPR := stageType == "implement" && !cfg.noPR && cfg.decomposedFromRunID == "" && !cfg.fixup

	// willPushChild is the decomposed-child complement of willOpenPR: an
	// implement stage that will commit + push onto the shared parent branch
	// after the trace upload but never open a PR (the parent run opens one
	// consolidated PR after all children settle, per ADR-032). It stamps
	// push_to_shared_branch in the bundle manifest so the backend
	// forward-gates the child stage's terminal transition onto the
	// /pull-request upload (#771) — the decomposition-child analogue of the
	// #742 forward-gate — and it gates the failure-report POST below so a
	// child commit/push failure lands the stage `failed` instead of leaving
	// the trace-time succeeded zombie. willOpenPR and willPushChild are
	// mutually exclusive: a decomposed child has decomposedFromRunID != "" so
	// willOpenPR is false; a fix-up pass sets neither and transitions on the
	// trace upload as today.
	willPushChild := stageType == "implement" && cfg.decomposedFromRunID != "" && !cfg.fixup

	// willPushFixup is the fix-up-pass complement of willOpenPR/willPushChild:
	// a fix-up re-dispatch implement stage (#762) that commits onto the EXISTING
	// PR branch after the trace upload (updating the open PR, not opening a new
	// one). It stamps push_fixup in the bundle manifest so the backend
	// forward-gates the fix-up stage's terminal transition onto the
	// /pull-request upload (#794) — the fix-up analogue of the #742/#771
	// forward-gate — and it gates the failure-report POST below so a fix-up
	// commit/push/compile-gate failure lands the stage `failed` (firing #788
	// recovery) instead of leaving the trace-time succeeded zombie whose
	// implement re-review approves an unlanded diff. willOpenPR, willPushChild,
	// and willPushFixup are mutually exclusive: willOpenPR and willPushChild are
	// both false for a fix-up (each requires !cfg.fixup), and a fix-up is never
	// a decomposed child (decomposedFromRunID == "").
	willPushFixup := stageType == "implement" && cfg.fixup

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
			Agent:   cfg.agent,
			// Carry the resolved model id + token split to the manifest
			// so the backend prices the run from this signed record
			// (authoritative cost), not from a runner-emitted span.
			Model:        res.Model,
			InputTokens:  res.InputTokens,
			OutputTokens: res.OutputTokens,
			// Prompt-cache split (ADR-044 / #1349): InputTokens is the fresh
			// (cache-exclusive) input; these carry the cache-served read and
			// cache-creation write portions so the backend prices cache reads
			// at the discount and writes at the premium. manifestRedacted
			// copies them verbatim below (token counts are not secrets).
			CacheReadInputTokens:  res.CacheReadInputTokens,
			CacheWriteInputTokens: res.CacheWriteInputTokens,
			AgentFailed:           agentFailed,
			AgentFailureReason:    agentFailureReason,
			// Self-observed execution channel (#1346 / ADR-045). Set on the
			// raw manifest; manifestRedacted copies it verbatim below (it is
			// a provenance tag, not a secret). The backend reconciles it
			// against the run's creation-time hint and LOCKS runner_kind to
			// it, closing the #1344 local-loop wedge.
			RunnerKind:         detectRunnerKind(os.Getenv),
			PushAndOpenPR:      willOpenPR,
			PushToSharedBranch: willPushChild,
			PushFixup:          willPushFixup,
		}
		// Gate evidence (#963): fold the stage's verify / scope /
		// constraint gate results into one bounded, pre-redacted
		// gate_evidence event so review prompts see machine-verified
		// truth. Appended before EITHER PackBytes so both variants
		// carry it; pre-redacted because the implement review
		// dispatches on the raw variant (#793). Nil when no gate ran.
		if ev := composeGateEvidence(res.Events, len(scopePaths(cfg.scopeFiles))); ev != nil {
			res.Events = append(res.Events, *ev)
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
		} else {
			// The fetch-prompt path already minted a key at stage start.
			// A long stage (agent runtime + scope-amendment blocks +
			// verify reinvokes) can outlive that key's TTL before this
			// terminal upload, which would fail category-C and discard
			// all completed work (#1182). Re-issue a fresh key here so
			// the TTL clock restarts at egress time. Because trace,
			// plan (uploadPlan), openPRAndShipArtifact (FetchInstallationToken
			// + ShipPullRequest), and reportPullRequestFailure all read
			// this same issuedKey, this single refresh at the top of the
			// block covers every terminal signed-egress path. Best-effort:
			// on a pre-0012 / transient failure the helper returns the
			// existing start-of-stage key unchanged — never worse than before.
			issuedKey = reissueSigningKeyForTerminalUpload(ctx, client, cfg, logSink, issuedKey)
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

		// Acceptance-stage post-processing (E31.7 / #1535): ship the
		// redacted, validated verdict to POST /v0/runs/{run_id}/acceptance
		// with the re-issued signing key. Mirrors the uploadPlan slot's
		// classification contract: ErrAcceptanceInvalid (the backend
		// rejected the verdict shape) is category-B — the agent's output
		// is bad; everything else (network, 5xx-exhausted) is category-C
		// infra. Gated on res.OK, so a missing/invalid verdict (already
		// demoted to B above) never ships, and only fires on the
		// acceptance stage type.
		if res.OK && stageType == "acceptance" {
			shipRes, err := client.ShipAcceptance(ctx, upload.ShipAcceptanceArgs{
				RunID:      cfg.runID,
				StageID:    cfg.stageID,
				Body:       acceptanceVerdictRedacted,
				PrivateKey: issuedKey.PrivateKey,
			})
			if err != nil {
				res.OK = false
				if errors.Is(err, upload.ErrAcceptanceInvalid) {
					res.FailureCategory = "B"
				} else {
					res.FailureCategory = "C"
				}
				res.FailureReason = err.Error()
				invokeErr = err
				_, _ = fmt.Fprintf(logSink,
					`{"event":"runner_failed","reason":"acceptance_upload","detail":%q}`+"\n", err.Error())
				logCompletion(logSink, res, invokeErr)
				return exitFailure
			}
			_, _ = fmt.Fprintf(logSink,
				`{"event":"acceptance_shipped","run_id":%q,"stage_id":%q,"artifact_id":%q,"verdict":%q,"content_hash":%q,"idempotent":%t}`+"\n",
				cfg.runID, cfg.stageID, shipRes.ID, shipRes.Verdict, shipRes.ContentHash, shipRes.Idempotent)
		}

		// Implement-stage post-processing: commit + push + open PR
		// + ship the pull_request artifact. (E5.X / #195.) Mirrors
		// the plan-stage upload chain: same signing key, same
		// failure-classification rules. ErrPullRequestInvalid is
		// category-B (we shipped the wrong shape); everything else
		// is category-C (network, git, GitHub API).
		if res.OK && stageType == "implement" {
			// First (pre-re-invoke) ship: nil supplemental — these exemptions are
			// already in the sealed bundle's scope_files_exempted gate_evidence event.
			prErr := openPRAndShipArtifact(ctx, cfg, logSink, client, issuedKey, preAgentRef, preAgentDetached, preAgentCaptured, preAgentDirty, preAgentDirtyCaptured, verifiedTreeSHA, applyPath, bindingAssertions, mergeExemptions(validatedExemptions, operatorExemptions), nil)
			// Bounded base-rebase-conflict re-invoke (#989): a stash-reapply
			// conflict means the base moved under the agent (a sibling's
			// shared-branch commit, or an advanced origin/<base>) — often a
			// trivially-resolvable overlap that previously killed the stage
			// (and a decomposition's whole fan-out) category-B at commit time.
			// Re-invoke the agent ONCE on the fresh base with the captured
			// conflict context, then retry the commit+push chain. Structurally
			// bounded: this block is linear, so a second conflict on the retry
			// falls through to the unchanged category-B path below. Gated on
			// the open-PR and decomposed-child push paths only — a fix-up pass
			// keeps its existing #788 recovery semantics. Gate re-coverage for
			// the re-landed tree needs no special handling: it differs from
			// the gate-verified tree, so the #960 verified_tree_mismatch path
			// runs its single strict re-verify and only an explicit pass
			// reaches origin (#969 stamps the re-verified tree).
			if prErr != nil && errors.Is(prErr, gitops.ErrBaseRebaseConflict) &&
				(willOpenPR || willPushChild) {
				// Fail-closed exemption freshness across the re-invoke (#1153). The
				// base-rebase re-invoke runs a SECOND agent within this stage, so
				// attempt 1's validatedExemptions (loaded + consumed at run() line
				// ~1172) must NOT shape the post-reinvoke scope-completeness gate.
				// Sweep any leftover sidecar before the re-invoke and re-derive the
				// exemptions from whatever the re-invoked agent writes AFTER it — nil
				// when it justifies nothing, which restores the strict gate. Gated to
				// the open-PR path: only it runs the gate with exemptions (decomposed
				// children are isDecomposed-excluded and carry an empty set). The trace
				// bundle — and its gate_evidence scope_files_exempted fold — already
				// shipped above (#742 forward gating), so this re-emits NO event
				// (binding condition 1's single-emission holds); the reload feeds the
				// enforcement gate only.
				if willOpenPR {
					sweepStaleScopeJustification(cfg, logSink)
				}
				if rerr := reinvokeOnBaseRebaseConflict(ctx, cfg, invoker, inv, &res, prErr, logSink); rerr == nil {
					if willOpenPR {
						validatedExemptions = loadScopeExemptions(cfg, scopePaths(cfg.scopeFiles), logSink)
					}
					// Re-merge operatorExemptions (#1229): the line above reset
					// validatedExemptions to whatever the re-invoked agent wrote to
					// the swept sidecar (nil if it justified nothing), but the
					// operator's exempt_scope_files exemptions came from the prompt
					// and MUST persist across the re-invoke. mergeExemptions unions
					// them so the second gate call still subtracts the operator set.
					reinvokeExemptions := mergeExemptions(validatedExemptions, operatorExemptions)
					// Supplemental re-invoke delta (#1218): the trace bundle (and its
					// scope_files_exempted gate_evidence fold) already shipped above
					// under #742 forward gating, BEFORE this re-invoke reloaded the
					// agent's freshly-validated exemptions. So any exemption the final
					// gate now honors that the sealed event did NOT carry is invisible
					// to the audit/review. Pass that delta into the post-re-invoke ship
					// so the backend re-emits it as a supplemental scope_files_exempted
					// audit row — the visibility surface for the re-invoke branch. The
					// review's gate_evidence can NOT be re-fed: it is dispatched at
					// trace-upload time, strictly before this re-invoke (#1153).
					supplemental := diffExemptions(reinvokeExemptions, sealedExemptions)
					prErr = openPRAndShipArtifact(ctx, cfg, logSink, client, issuedKey, preAgentRef, preAgentDetached, preAgentCaptured, preAgentDirty, preAgentDirtyCaptured, verifiedTreeSHA, applyPath, bindingAssertions, reinvokeExemptions, supplemental)
				} else {
					// Re-invoke infra exhaustion (or checkout failure): log and
					// fall through to the unchanged category-B failure path
					// with the ORIGINAL conflict error.
					_, _ = fmt.Fprintf(logSink,
						`{"event":"base_rebase_reinvoke_aborted","run_id":%q,"stage_id":%q,"detail":%q}`+"\n",
						cfg.runID, cfg.stageID, rerr.Error())
				}
			}
			if err := prErr; err != nil {
				res.OK = false
				// Wrong-shaped output → category-B (parks the run for
				// re-scope/re-plan). ErrPullRequestInvalid is the backend
				// rejecting the artifact shape; ErrCommitWouldNotCompile is
				// the pre-push compile gate (#728) catching a scope-only
				// commit that dropped build-required drift; ErrCommittedTestsFailed
				// is the test-phase extension (#800) catching a scope-only commit
				// whose touched-package tests fail because a drift-excluded test
				// fake was dropped. ErrCreatedOutOfScope is the created-out-of-scope
				// gate (#818, generalized to the open-PR push by #825) catching a
				// stage that created net-new out-of-scope files (silently stripped →
				// misleadingly-green partial); it matches both the open-PR and the
				// fix-up (ErrFixupCreatedOutOfScope) wrapped errors.
				// ErrBaseRebaseConflict is the fresh-fetch base path (#866,
				// ADR-035) failing because the run branched from a base that
				// diverged from the agent's edits, so reapplying them onto the
				// clean fetched base conflicts (re-base/re-plan).
				// ErrPushedTreeNotVerified is the verified-SHA invariant (#960):
				// the staged commit's tree is not the tree the committed-tree
				// gates verified and the single strict re-verify did not pass —
				// pushing it would vouch for a tree no gate ever saw.
				// ErrCommitOutOfScope is the post-commit scope assertion (#980):
				// the staged commit contains a path outside the declared
				// scope.files, so the drift report disagrees with the commit's
				// actual content. ErrScopeFilesMissing is the pre-push
				// scope-completeness (shortfall) gate (#1151), the inverse of
				// the above: the commit did not touch every declared concrete
				// scope.files path, so a declared edit was dropped (the #1148
				// subset PR). ErrBindingAssertionUnsatisfied is the binding-
				// assertion gate (#1171): a deterministic operator-declared
				// substring check the committed tree did not satisfy — the
				// artifact does not meet a declared binding condition, so park
				// for re-scope/re-plan. Everything else (network, git, GitHub
				// API) is category-C infra.
				if errors.Is(err, upload.ErrPullRequestInvalid) ||
					errors.Is(err, gitops.ErrCommitWouldNotCompile) ||
					errors.Is(err, gitops.ErrCommittedTestsFailed) ||
					errors.Is(err, gitops.ErrCreatedOutOfScope) ||
					errors.Is(err, gitops.ErrBaseRebaseConflict) ||
					errors.Is(err, gitops.ErrCommitOutOfScope) ||
					errors.Is(err, gitops.ErrScopeFilesMissing) ||
					errors.Is(err, gitops.ErrBindingAssertionUnsatisfied) ||
					errors.Is(err, gitops.ErrPushedTreeNotVerified) {
					res.FailureCategory = "B"
				} else {
					res.FailureCategory = "C"
				}
				res.FailureReason = err.Error()
				invokeErr = err
				_, _ = fmt.Fprintf(logSink,
					`{"event":"runner_failed","reason":"pull_request_upload","detail":%q}`+"\n", err.Error())
				// Report the failure to /pull-request so the backend fails the
				// implement stage that its trace gate left in `running` (#742,
				// #771, #794). Without this the gated stage would hang until the
				// SLA watchdog reaps it. Gated on willOpenPR || willPushChild ||
				// willPushFixup: a push_and_open_pr stage (#742), a decomposed
				// child (push_to_shared_branch, #771), AND a fix-up re-dispatch
				// (push_fixup, #794) are all left in running by the trace gate, so
				// each must report a commit/push/compile-gate failure; a --no-pr
				// failure was never gated, so reporting one would wrongly fail an
				// already-advanced stage. For a fix-up, the backend's
				// failPullRequestStage → maybeRecoverFixupFailure (#788) then
				// restores the run to its pre-fix-up review gate. Best-effort —
				// the failure exit stands regardless of whether the report lands.
				if willOpenPR || willPushChild || willPushFixup {
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

// reissueSigningKeyForTerminalUpload mints a FRESH signing key immediately
// before the terminal signed-egress sequence (trace/plan/PR-artifact/
// installation-token), so a stage whose wall-clock (agent runtime +
// scope-amendment blocking waits + verify reinvokes + the upload itself)
// outlives the start-of-stage key's TTL still signs its terminal uploads
// with an unexpired key (#1182). The refreshed key's TTL clock starts at
// issuance time, so its expiry comfortably exceeds the few seconds the
// terminal uploads take.
//
// Multi-call safety: the backend's signing Issue is multi-call against
// migration 0012+ (0012_signing_keys_allow_rotation appends a new row per
// invocation) and Verify always uses the latest unexpired key, so a second
// issuance before terminal egress is safe and authoritative — no 409 on a
// 0012+ backend.
//
// Best-effort by design: on upload.ErrAlreadyIssued (a pre-0012 backend that
// cannot multi-issue) or any other issuance error, it logs a
// signing_key_refresh_degraded warning and returns the existing
// start-of-stage key unchanged so the caller is never worse than the
// pre-#1182 behavior. The caller assigns the return value to its shared
// issued key unconditionally — a refresh failure is a no-op, not fatal.
func reissueSigningKeyForTerminalUpload(ctx context.Context, client uploadClient, cfg config, logSink io.Writer, existing *upload.IssuedKey) *upload.IssuedKey {
	issued, err := client.IssueKey(ctx, cfg.runID, 0)
	if err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"signing_key_refresh_degraded","run_id":%q,"detail":%q}`+"\n",
			cfg.runID, err.Error(),
		)
		return existing
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"signing_key_refreshed","run_id":%q,"expires_at":%q}`+"\n",
		issued.RunID, issued.ExpiresAt.Format(time.RFC3339),
	)
	return issued
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
// 15-minute fallback. verifyCmd, verifyTimeoutSecs, and
// verifyMaxIterations are the spec-resolved verify gate settings;
// zero/empty when the spec declares none. decomposedFromRunID is non-empty when this run is a
// decomposed child. minRunnerVersion is non-empty when the backend
// requires a minimum runner version; the caller checks it against
// runnerVersion() before proceeding. agentSelfRetry, maxRetriesSnapshot,
// and retryAttempt drive the ADR-023 self-retry loop. commitAuthorName and
// commitAuthorEmail are the backend-resolved App bot commit identity (#722);
// both empty when the backend couldn't resolve it and the caller keeps the
// gitops default bot identity. fixup and fixupBranch (#762) mark an
// operator-triggered implement-review fix-up pass: when fixup is true the
// implement stage commits onto the existing PR branch fixupBranch and updates
// the open PR rather than opening a new one. fixupExpectedHeadSHA (#967) is
// the run's recorded head the pre-invoke fix-up base establishment verifies
// the fetched branch tip against; empty skips the comparison.
// acceptanceExpectedHeadSHA (E31.18 / #1569) is the merge-candidate head the
// acceptance pre-spawn target-identity gate compares the declared target's
// /healthz git_sha against; empty degrades the gate to unverifiable-warn.
// The temp file is 0o600 — bundle-style defense in depth, since prompts
// may include issue bodies that the customer would prefer not to leave on
// the runner's filesystem world-readable.
func fetchPromptToFile(ctx context.Context, client uploadClient, cfg config, key *upload.IssuedKey, logSink io.Writer) (path string, stageType string, agentTimeoutSecs int, verifyCmd string, verifyTimeoutSecs int, verifyMaxIterations int, decomposedFromRunID string, minRunnerVersion string, agentVersionRange string, agentSelfRetry bool, maxRetriesSnapshot int, retryAttempt int, scopeFiles []upload.ScopeFile, commitAuthorName string, commitAuthorEmail string, fixup bool, fixupBranch string, fixupExpectedHeadSHA string, bindingAssertions []upload.BindingAssertion, fixupApplyPatches []upload.FixupApplyPatch, sliceIndex int, scopeExemptions []upload.ScopeExemption, openPRFromHeldCommit bool, heldCommitSHA string, heldCommitBranch string, implementModel string, planModel string, egressTargetHosts []string, acceptanceCriteriaIDs []string, acceptanceExpectedHeadSHA string, err error) {
	got, fetchErr := client.FetchPrompt(ctx, upload.FetchPromptArgs{
		StageID:    cfg.stageID,
		PrivateKey: key.PrivateKey,
	})
	if fetchErr != nil {
		return "", "", 0, "", 0, 0, "", "", "", false, 0, 0, nil, "", "", false, "", "", nil, nil, 0, nil, false, "", "", "", "", nil, nil, "", fetchErr
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"prompt_fetched","stage_id":%q,"stage_type":%q,"prompt_hash":%q,"prompt_bytes":%d}`+"\n",
		got.StageID, got.StageType, got.PromptHash, len(got.Prompt),
	)
	tmp, tmpErr := os.CreateTemp("", "fishhawk-prompt-*.txt")
	if tmpErr != nil {
		return "", "", 0, "", 0, 0, "", "", "", false, 0, 0, nil, "", "", false, "", "", nil, nil, 0, nil, false, "", "", "", "", nil, nil, "", fmt.Errorf("create prompt temp file: %w", tmpErr)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		_ = tmp.Close()
		return "", "", 0, "", 0, 0, "", "", "", false, 0, 0, nil, "", "", false, "", "", nil, nil, 0, nil, false, "", "", "", "", nil, nil, "", fmt.Errorf("chmod prompt temp file: %w", err)
	}
	if _, err := tmp.WriteString(got.Prompt); err != nil {
		_ = tmp.Close()
		return "", "", 0, "", 0, 0, "", "", "", false, 0, 0, nil, "", "", false, "", "", nil, nil, 0, nil, false, "", "", "", "", nil, nil, "", fmt.Errorf("write prompt temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", "", 0, "", 0, 0, "", "", "", false, 0, 0, nil, "", "", false, "", "", nil, nil, 0, nil, false, "", "", "", "", nil, nil, "", fmt.Errorf("close prompt temp file: %w", err)
	}
	return tmp.Name(), got.StageType, got.AgentTimeoutSeconds, got.VerifyCommand, got.VerifyTimeoutSeconds, got.VerifyMaxIterations, got.DecomposedFromRunID, got.MinRunnerVersion, got.AgentVersionRange, got.AgentSelfRetry, got.MaxRetriesSnapshot, got.RetryAttempt, got.ScopeFiles, got.CommitAuthorName, got.CommitAuthorEmail, got.Fixup, got.FixupBranch, got.FixupExpectedHeadSHA, got.BindingAssertions, got.FixupApplyPatches, got.SliceIndex, got.ScopeExemptions, got.OpenPRFromHeldCommit, got.HeldCommitSHA, got.HeldCommitBranch, got.ImplementModel, got.PlanModel, got.EgressTargetHosts, got.AcceptanceCriteriaIDs, got.AcceptanceExpectedHeadSHA, nil
}

func logStartup(w io.Writer, cfg config) {
	// runner_kind is the self-observed execution channel (#1346 / ADR-045),
	// surfaced here for log observability and computed from the SAME
	// detectRunnerKind(os.Getenv) source that stamps the signed manifest, so
	// the log line and the attestable manifest claim never disagree.
	//
	// agent_kind/agent_binary/agent_version record the coding-agent provider
	// (cfg.agent), the resolved CLI binary (operator override or adapter
	// default), and its probed `--version` string (#1741) so an operator can
	// tell exactly which agent build produced a run from the log alone. A
	// binary that has no --version flag records agent_version:"unknown".
	_, _ = fmt.Fprintf(w,
		`{"event":"runner_started","run_id":%q,"workflow":%q,"stage":%q,"backend_url":%q,"version":%q,"git_sha":%q,"prompt_file":%q,"runner_kind":%q,"agent_kind":%q,"agent_binary":%q,"agent_version":%q}`+"\n",
		cfg.runID, cfg.workflow, cfg.stage, cfg.backendURL, runnerVersion(), runnerGitSHA(), cfg.promptFile, detectRunnerKind(os.Getenv), cfg.agent, cfg.agentBinary, cfg.agentVersion,
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
	// Marshal the failure line via a struct so api_error_status is emitted
	// with omitempty — present when a terminal 5xx external-API error lifted
	// a status onto the Result (>0), absent otherwise (==0) — without hand
	// managing comma placement. The field ordering is irrelevant to the
	// substring-based log assertions.
	line, mErr := json.Marshal(struct {
		Event          string `json:"event"`
		Outcome        string `json:"outcome"`
		Category       string `json:"category"`
		Reason         string `json:"reason"`
		TokensUsed     int    `json:"tokens_used"`
		ErrClass       string `json:"err_class"`
		APIErrorStatus int    `json:"api_error_status,omitempty"`
	}{
		Event:          "runner_completed",
		Outcome:        "failed",
		Category:       category,
		Reason:         reason,
		TokensUsed:     res.TokensUsed,
		ErrClass:       classifyErr(err),
		APIErrorStatus: res.APIErrorStatus,
	})
	if mErr != nil {
		// Should never happen (all fields are plain scalars); fall back to
		// the pre-struct format so a failure line is never silently dropped.
		_, _ = fmt.Fprintf(w,
			`{"event":"runner_completed","outcome":"failed","category":%q,"reason":%q,"tokens_used":%d,"err_class":%q}`+"\n",
			category, reason, res.TokensUsed, classifyErr(err))
		return
	}
	_, _ = fmt.Fprintf(w, "%s\n", line)
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
	case errors.Is(err, agent.ErrExternalAPI):
		return "external_api"
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
	// Same slice-branch derivation and predicate the upload-phase routing
	// uses (see the branch-routing block). Under ADR-041 (#1141) each child
	// owns a sole-writer slice branch minted once, so it never pre-exists on
	// the remote: remoteBranchExists is always false and this returns
	// cfg.checkBaseRef — each child's policy diff is bounded against base,
	// the correct behavior for an independent slice cut fresh from base
	// (the pre-ADR-041 origin/<shared-branch> cumulative-diff base belonged
	// to the shared-branch model; fan-in is E24.2).
	sharedBranch := childSliceBranch(cfg.decomposedFromRunID, runSliceIndex)
	repoDir := cfg.workingDir
	if repoDir == "" {
		repoDir = "."
	}
	if !remoteBranchExists(ctx, repoDir, sharedBranch) {
		// Slice branch not on the remote (always, for a sole-writer slice):
		// HEAD == base, so the policy base is cfg.checkBaseRef.
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

	// ADR-043 rev 2 (#1294): convert the staged-index policy diff from a
	// 2-dot comparison (staged index vs. the base-branch TIP) to a 3-dot one
	// (staged index vs. the run's fork point) by diffing against the
	// merge-base of baseRef and HEAD instead of the moving tip. Without this,
	// a file the base branch added orthogonally AFTER the run branched is
	// present in the tip's tree but absent from the run's index, so `git diff
	// --cached <tip>` reports it as a phantom deletion (status D) that
	// inflates the StagedFiles count gateevidence + the #1151
	// scope-completeness gate read — reproducing #1290's 16-staged-vs-7-
	// declared implement-review failure. merge-base is a purely LOCAL git
	// operation (no forge API), so the gate stays provider-agnostic.
	//
	// FAIL-OPEN: if the merge-base cannot be resolved (unrelated histories,
	// shallow clone, or a base ref not yet fetched locally) we log a
	// degradation line and fall back to the original tip baseRef — today's
	// exact behavior — never blocking the diff. baseRef stays the
	// human-meaningful label on the git_diff event; diffBaseRef is the
	// commit-ish the staged index is actually measured against, so the
	// name-status (Run), patch (RunPatch), and numstat are all 3-dot and
	// internally consistent.
	diffBaseRef := baseRef
	if mb, mbErr := (&gitdiff.Runner{}).MergeBase(context.Background(), baseRef, repoDir); mbErr != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"merge_base_unresolved","stage_id":%q,"base_ref":%q,"detail":%q}`+"\n",
			cfg.stageID, baseRef, mbErr.Error())
	} else {
		diffBaseRef = mb
	}

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
			payload := map[string]any{
				"check":      "scope_drift",
				"outcome":    "excluded",
				"undeclared": drift,
			}
			// Per-path A/B categorization (#991) rides alongside the
			// `undeclared` list, which stays byte-for-byte unchanged for
			// its existing consumers. Best-effort: categorization is
			// advisory review evidence, never a gate — on error, log a
			// degradation line and emit the uncategorized payload.
			if categorized, cerr := categorizeDrift(context.Background(), repoDir, drift,
				cfg.decomposedFromRunID != ""); cerr != nil {
				_, _ = fmt.Fprintf(logSink,
					`{"event":"scope_drift_categorize_failed","stage_id":%q,"detail":%q}`+"\n",
					cfg.stageID, cerr.Error())
			} else {
				payload["undeclared_categorized"] = categorized
			}
			events = append(events, agent.Event{
				Kind:    "policy_event",
				Payload: agent.MakePayload(payload),
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
	d, err := runner.Run(context.Background(), diffBaseRef, repoDir)
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
	patch, truncated, perr := runner.RunPatch(context.Background(), diffBaseRef, repoDir)
	if perr != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"git_diff_patch_failed","base_ref":%q,"detail":%q}`+"\n",
			diffBaseRef, perr.Error())
		patch, truncated = "", false
	}
	ins, dels := computeDiffNumstat(context.Background(), diffBaseRef, repoDir, logSink)
	events = append(events, makeGitDiffEventStats(baseRef, d, patch, truncated, ins, dels))
	return d, events, nil
}

// categorizeDrift partitions a scope_drift list into per-path A/B
// categories (#991) using gitops.UntrackedPaths — the same primitive
// the #818/#825 created-out-of-scope gate uses, so the evidence's
// category-B set and the gate's enforcement set agree by construction
// (StageScoped's leading reset leaves created drift untracked at this
// point). Untracked drift is category B (created out of scope); the
// remainder (tracked modify/delete) is category A (edit excluded from
// the commit). Disposition: A is always "excluded_from_commit"; B is
// "would_fail_loud" except for decomposed children, which are exempt
// from the created-out-of-scope gate (a child may legitimately create
// files a later child declares) and so get "excluded_from_commit".
func categorizeDrift(ctx context.Context, repoDir string, drift []string, decomposedChild bool) ([]driftPathEvidence, error) {
	created, err := untrackedPaths(ctx, repoDir, drift)
	if err != nil {
		return nil, err
	}
	createdSet := make(map[string]bool, len(created))
	for _, p := range created {
		createdSet[p] = true
	}
	out := make([]driftPathEvidence, 0, len(drift))
	for _, p := range drift {
		if createdSet[p] {
			disposition := "would_fail_loud"
			if decomposedChild {
				disposition = "excluded_from_commit"
			}
			out = append(out, driftPathEvidence{Path: p, Category: "B", Disposition: disposition})
			continue
		}
		out = append(out, driftPathEvidence{Path: p, Category: "A", Disposition: "excluded_from_commit"})
	}
	return out, nil
}

// reemitScopedGitDiff recomputes the scope-only git_diff from the reconciled
// committed tree, returning a single git_diff event to append AFTER
// computeAndEmitDiff's original. It fires on either of two staleness triggers:
// a verify-fix loop reinvoke that rewrote in-scope files (#870), or an
// operator-approved scope amendment folded into cfg.scopeFiles after the first
// git_diff was emitted (#1660). It mirrors computeAndEmitDiff's diff-producing
// core but deliberately does NOT re-emit the scope_drift / constraint
// policy_events: the pre-fold scope_drift snapshot and the constraint evaluation
// that already ran remain authoritative (the fold's own scope_amendments_folded
// policy_event is emitted by refreshScopeAmendments) — re-emitting only the
// git_diff keeps drift and policy accounting untouched while making the
// reconciled diff last-write-wins.
//
// Re-emit is best-effort: on any infra error (stage / name-status / nothing to
// re-stage) it logs a degradation line and returns nil. The original git_diff
// stays in the bundle as a graceful fallback, and the stage is never failed on a
// re-emit error — the load-bearing gate already passed inside the loop.
func reemitScopedGitDiff(cfg config, logSink io.Writer) []agent.Event {
	repoDir := cfg.workingDir
	if repoDir == "" {
		repoDir = "."
	}
	baseRef := resolvePolicyBaseRef(context.Background(), cfg, logSink)

	// Re-stage the reconciled in-scope tree. The verify-fix loop's final
	// `git reset --soft` preserves the index, but re-staging is deterministic and
	// matches computeAndEmitDiff. Drift is discarded — already reported above.
	if paths := scopePaths(cfg.scopeFiles); len(paths) > 0 {
		if _, err := (&gitops.Pusher{}).StageScoped(context.Background(), repoDir, paths); err != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"git_diff_reemit_skipped","stage_id":%q,"detail":%q}`+"\n",
				cfg.stageID, fmt.Sprintf("stage scoped: %v", err))
			return nil
		}
	} else {
		addCmd := exec.CommandContext(context.Background(), "git", "add", "-A")
		addCmd.Dir = repoDir
		if out, err := addCmd.CombinedOutput(); err != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"git_diff_reemit_skipped","stage_id":%q,"detail":%q}`+"\n",
				cfg.stageID, fmt.Sprintf("git add -A: %v: %s", err, strings.TrimSpace(string(out))))
			return nil
		}
	}

	runner := &gitdiff.Runner{}
	d, err := runner.Run(context.Background(), baseRef, repoDir)
	if err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"git_diff_reemit_skipped","stage_id":%q,"detail":%q}`+"\n",
			cfg.stageID, fmt.Sprintf("name-status: %v", err))
		return nil
	}
	patch, truncated, perr := runner.RunPatch(context.Background(), baseRef, repoDir)
	if perr != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"git_diff_patch_failed","base_ref":%q,"detail":%q}`+"\n",
			baseRef, perr.Error())
		patch, truncated = "", false
	}
	ins, dels := computeDiffNumstat(context.Background(), baseRef, repoDir, logSink)
	return []agent.Event{makeGitDiffEventStats(baseRef, d, patch, truncated, ins, dels)}
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
//
// Insertions/Deletions carry the `git diff --cached --numstat <base>`
// totals (E22.X / #1137). They moved onto the wire because per-run
// worktree isolation relocates the run's commit off the operator
// checkout's HEAD, so the MCP server can no longer recompute the diff
// stats by shelling `git show --numstat HEAD` in working_dir — it reads
// them from this event instead. Additive and `omitempty`: older bundles
// omit them and decode to zero.
type gitDiffPayload struct {
	Kind           string        `json:"kind"`
	BaseRef        string        `json:"base_ref"`
	Files          []gitDiffFile `json:"files"`
	NumFiles       int           `json:"num_files"`
	Patch          string        `json:"patch,omitempty"`
	PatchTruncated bool          `json:"patch_truncated,omitempty"`
	Insertions     int           `json:"insertions,omitempty"`
	Deletions      int           `json:"deletions,omitempty"`
}

// makeGitDiffEvent converts a constraint.Diff into the bundle event
// the backend's policy re-evaluation reads. Kind is "git_diff";
// payload schema is gitDiffPayload (above). patch is the unified-diff
// hunk text (empty when capture failed); truncated marks a capped
// patch.
func makeGitDiffEvent(baseRef string, d constraint.Diff, patch string, truncated bool) agent.Event {
	return makeGitDiffEventStats(baseRef, d, patch, truncated, 0, 0)
}

// makeGitDiffEventStats is makeGitDiffEvent with the staged-diff numstat
// totals (#1137). The diff-producing paths (computeAndEmitDiff,
// reemitScopedGitDiff) carry the real insertion/deletion counts so the
// MCP server reports diff stats from the event rather than re-shelling
// git in the operator checkout — which per-run worktree isolation makes
// stale. The zero-arg makeGitDiffEvent shim is retained for synthetic
// events (gate-evidence tests) that don't compute numstat.
func makeGitDiffEventStats(baseRef string, d constraint.Diff, patch string, truncated bool, insertions, deletions int) agent.Event {
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
			Insertions:     insertions,
			Deletions:      deletions,
		}),
	}
}

// computeDiffNumstat sums the insertion/deletion totals of the staged diff
// against baseRef via `git diff --cached --numstat <base>`, the same staged
// index the name-status and patch captures see. It rides onto the git_diff
// event so the MCP server can report diff stats without re-deriving them
// from the operator checkout's HEAD, which per-run worktree isolation no
// longer carries the run's commit (#1137). Best-effort: a git failure logs
// a degradation line and yields (0, 0) — the diff stats are advisory.
func computeDiffNumstat(ctx context.Context, baseRef, repoDir string, logSink io.Writer) (insertions, deletions int) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "diff", "--cached", "--numstat", baseRef).Output()
	if err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"git_diff_numstat_failed","base_ref":%q,"detail":%q}`+"\n",
			baseRef, gitErr(err).Error())
		return 0, 0
	}
	return parseDiffNumstat(string(out))
}

// parseDiffNumstat parses `git diff --numstat` output and sums insertions
// and deletions across all rows, skipping binary-file rows where either
// column is '-'. Mirrors the backend's parseNumstat shape
// (backend/cmd/fishhawk-mcp/run_stage.go) so the two sides agree on the
// stat semantics.
func parseDiffNumstat(output string) (insertions, deletions int) {
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

// validatePlan reads the plan artifact at path and validates it
// against the standard_v1 schema. The first return is a policy_event
// suitable for the trace bundle: kind=policy_event, payload describes
// the validation outcome. The second return is non-nil ONLY on
// validation failure — it carries the reason for callers wiring up
// category-B failure handling per MVP_SPEC §6.
// detectClarificationRequest peeks the plan-out file's top-level "kind"
// discriminator (#1057). A clarification_request is the additive standard_v1
// sibling the planner emits when an issue is not yet plannable — it is shipped
// as-is (the backend ingests it and parks the stage at awaiting_input) rather
// than validated as a plan, so the runner must NOT demote it to category-B.
// Mirrors backend/internal/plan.DetectArtifactKind: a plan artifact carries no
// "kind" field (it has plan_version), so only an explicit
// kind == "clarification_request" routes here.
//
// Returns (false, zero Event) on any read/parse error so the caller falls
// through to validatePlan, where a genuinely-missing or malformed plan is
// demoted as before. On a hit it returns a policy_event recording the
// detection in the trace bundle.
func detectClarificationRequest(path string) (bool, agent.Event) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, agent.Event{}
	}
	var disc struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &disc); err != nil {
		return false, agent.Event{}
	}
	if disc.Kind != "clarification_request" {
		return false, agent.Event{}
	}
	return true, agent.Event{
		Kind: "policy_event",
		Payload: agent.MakePayload(map[string]string{
			"check":   "plan_validation",
			"outcome": "clarification_request",
			"path":    path,
		}),
	}
}

// adoptStructuredOutput overwrites the plan-out file with the schema-guaranteed
// structured_output bytes captured from the agent's terminal result event
// (#1325), so the subsequent uploadPlan ships them and validatePlan runs against
// them. Returns a policy_event recording the adoption. On a write failure it
// returns an event noting the failure and leaves the agent-written file in
// place — the caller's validate path then runs against whatever the agent wrote
// (today's fallback), so a write fault degrades rather than failing the stage.
func adoptStructuredOutput(path string, out []byte) agent.Event {
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return agent.Event{
			Kind: "policy_event",
			Payload: agent.MakePayload(map[string]string{
				"check": "plan_validation", "outcome": "structured_output_write_failed",
				"path": path, "error": err.Error(),
			}),
		}
	}
	return agent.Event{
		Kind: "policy_event",
		Payload: agent.MakePayload(map[string]string{
			"check": "plan_validation", "outcome": "structured_output_adopted", "path": path,
		}),
	}
}

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
	// Strip runner credentials from the gate subprocess env (ADR-029 #650
	// item 4): the verify command runs agent-authored code, so it must not
	// see the installation token / API keys / MCP token.
	cmd.Env = sanitizedGateEnv()
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

// maxFixInvokeInfraRetries bounds the fix re-invocation against a transient
// agent-API/transport failure (#804). Its value is the MAXIMUM TOTAL number of
// invoker.Invoke attempts for the fix re-invocation WITHIN A SINGLE outer
// verify iteration — i.e. the first attempt plus up to (maxFixInvokeInfraRetries-1)
// in-place retries. The counter is local to one outer iteration and resets
// when the next iteration begins; that reset is intentional and does NOT make
// the total unbounded, because the outer loop is itself hard-bounded by
// verifyMaxIterations. The worst-case total fix-Invokes across an entire
// runVerifyFixLoop call is therefore O(verifyMaxIterations * maxFixInvokeInfraRetries)
// — finite. A transient blip here must not advance the outer iteration counter
// (the verify that consumed this iteration already ran and was counted), so a
// retry does not burn a fix-loop budget unit. Exhausting all attempts routes to
// the existing non-blocking skip path (verify_fix_skipped), never category-A.
const maxFixInvokeInfraRetries = 2

// isTestcontainersStartFlake reports whether a failed verify's output matches
// the testcontainers container-start-timeout signature (#972): under
// full-suite parallel load a developer-Mac Docker daemon intermittently times
// out starting a Postgres container, failing one unlucky package with
// "... wait until ready: mapped port: check target: retries: 9 ... context
// deadline exceeded" against docker.sock — load contention, not a code bug.
// Both committed-tree gates use this to re-run the verify ONCE in place
// (without invoking the fix agent and without consuming a fix iteration)
// before the normal failure path resumes.
//
// The matcher is deliberately conservative: it requires the generic
// "context deadline exceeded" AND at least one container-start marker, so an
// ordinary Go test failure that merely mentions a deadline never matches.
// Matching is on testcontainers-go error text and can drift across library
// upgrades; failure is safe in both directions (a missed match degrades to
// today's fail-the-stage behavior, a false positive costs exactly one extra
// verify run). The table test pins both verbatim #972 outputs.
func isTestcontainersStartFlake(output string) bool {
	if !strings.Contains(output, "context deadline exceeded") {
		return false
	}
	for _, marker := range []string{
		"/var/run/docker.sock",
		"%2Fvar%2Frun%2Fdocker.sock", // URL-escaped socket path inside the client's GET error
		"failed to start container",
		"mapped port",
		"wait until ready",
	} {
		if strings.Contains(output, marker) {
			return true
		}
	}
	return false
}

// runVerifyFixLoop is the committed-tree verify-fix loop (#651). It runs
// ONLY on the implement push path with executor.verify.max_iterations > 0
// (the caller gates that). Per iteration it:
//
//  1. stages the scope-only files (StageScoped, #581) and makes a THROWAWAY
//     local commit to materialize a committed-HEAD SHA;
//  2. runs the verify command against that committed SHA in an isolated git
//     worktree (runVerifyCommittedTree) — NOT the dirty working tree, so a
//     drift-excluded test failure surfaces here exactly as it would in CI;
//  3. on PASS: undoes the throwaway commit (git reset --soft HEAD~1, which
//     leaves the converged working-tree edits + index intact) so the real
//     CommitAndPush in openPRAndShipArtifact makes the single push commit;
//  4. on FAIL with budget remaining: undoes the throwaway commit, then
//     re-invokes the agent in-place with a fix prompt embedding the captured
//     verify output. The next iteration re-commits scope-only onto the SAME
//     base — one converged commit, never a stack of fix commits on a bad base;
//  5. on FAIL with the budget exhausted (iter == max_iterations): undoes the
//     throwaway commit and demotes res to category-A. This is TERMINAL — the
//     loop is outside the ADR-023 self-retry for{} loop, so it can never call
//     RetryStage (DECISION c2, non-compounding).
//
// Failure handling splits on WHERE HEAD is when the failing op runs, symmetric
// with runVerifyGateCommitted (#816):
//
//   - A PRE-commit infra error (StageScoped / commitVerifyWIP / commit produced
//     nothing) left HEAD untouched, so it routes through the NON-BLOCKING skip
//     path (verify_fix_skipped, nil return) — never invent a failure from gate
//     plumbing; the real push runs its own #728/#800 gate.
//   - A POST-commit gitResetSoftHEAD1 failure is FATAL, not a skip. Once the
//     throwaway commit is materialized, a failed undo leaves HEAD on the
//     throwaway commit, so openPRAndShipArtifact's real CommitAndPush would
//     stack on top and push the bot-identity, --no-verify WIP commit into the
//     PR. Return it as a hard error so the stage fails loudly (category-B at the
//     call site) instead of silently shipping a throwaway commit.
//
// Every fix re-invocation's Result.Events are appended to res.Events and its
// token usage folded into res, so a multi-iteration run produces a complete
// audit trace and an honest cost (every other invoker.Invoke site replaces
// res wholesale; here append is the correct handling). A verify_summary event
// is appended once the loop settles, carrying the verdict + iteration count.
// runVerifyFixLoop returns reinvoked=true when it performed at least one agent
// fix re-invocation (#870). The caller uses that signal to re-emit a fresh
// scope-only git_diff event after the loop, so the implement review and policy
// re-eval evaluate the reconciled committed tree rather than the pre-reconcile
// diff computeAndEmitDiff emitted before the loop ran.
//
// On the passing iteration it also returns the verified tree object hash
// (#960), captured before the reset, for the pre-push VerifyCommit invariant.
// A passing verify whose tree cannot be resolved FAILS CLOSED
// (ErrPushedTreeNotVerified, hard error → category-B at the call site) — an
// empty tree would silently disable the invariant. Empty is returned only on
// the no-verdict paths: nothing-staged pass, skip, and exhaustion.
func runVerifyFixLoop(ctx context.Context, cfg config, invoker agent.Invoker, baseInv agent.Invocation, res *agent.Result, logSink io.Writer) (bool, string, error) {
	repoDir := cfg.workingDir
	if repoDir == "" {
		repoDir = "."
	}
	scopeFiles := scopePaths(cfg.scopeFiles)
	timeout := cfg.verifyTimeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}

	var (
		passed          bool
		reinvoked       bool
		attempts        int
		lastOutput      string
		verifiedTreeSHA string // set only on a passing iteration's real verify (#960)
		lastIterErr     error  // non-nil only on a PRE-commit infra failure → non-blocking skip
		fatalErr        error  // non-nil on a POST-commit reset failure (#816) or a passed-but-unresolvable verified tree (#960) → hard abort
		flakeRetried    bool   // once-per-stage testcontainers infra-flake absorb (#972) already spent
	)

	for iter := 0; iter <= cfg.verifyMaxIterations; iter++ {
		// (a) Stage scope-only and make a throwaway commit so the verify
		// command sees the drift-excluded committed tree, not the working tree.
		if _, err := (&gitops.Pusher{}).StageScoped(ctx, repoDir, scopeFiles); err != nil {
			lastIterErr = fmt.Errorf("verify-fix: stage scoped: %w", err)
			break
		}
		committed, err := commitVerifyWIP(ctx, cfg, repoDir)
		if err != nil {
			lastIterErr = err
			break
		}
		if !committed {
			// Nothing staged — no scope-only changes to gate. The real
			// openPRAndShipArtifact will hit its NoChanges short-circuit; treat
			// the loop as satisfied so it doesn't demote a no-op stage.
			passed = true
			break
		}

		// (b) Resolve the throwaway commit's SHA. HEAD has now moved, so any undo
		// failure from here on is FATAL (see (d)).
		headSHA, err := gitRevParseHEAD(ctx, repoDir)
		if err != nil {
			// rev-parse failed but the commit landed — undo it so the working
			// tree is left as the real push expects, then non-blocking skip. A
			// failed undo here is FATAL for the same reason as (d): HEAD is left
			// on the throwaway commit and the real push would stack on top.
			if rerr := gitResetSoftHEAD1(ctx, repoDir); rerr != nil {
				fatalErr = rerr
				break
			}
			lastIterErr = err
			break
		}

		// (c) Verify against the committed tree.
		ev, out, outcome := runVerifyCommittedTree(ctx, cfg.verifyCmd, repoDir, headSHA, timeout)
		res.Events = append(res.Events, ev)
		attempts++
		lastOutput = out

		// Capture the verified tree's object hash BEFORE the reset (#960).
		// Only a real "passed" earns one; the tolerant infra-skip outcome
		// keeps the loop's existing skip-as-pass treatment below but carries
		// no verified tree (no enforcement — reclassification is #959's scope).
		var treeErr error
		if outcome == "passed" {
			verifiedTreeSHA, treeErr = gitRevParseTreeOf(ctx, repoDir, headSHA)
		}

		// (d)/(e)/(f) Always undo the throwaway commit first — git reset --soft
		// moves HEAD without touching the index or working tree, so the agent's
		// edits + staged scope survive for either the real push (pass) or the
		// next iteration's re-commit (fail+retry). A reset failure here is FATAL
		// (#816): HEAD is left on the throwaway commit and the real push would
		// stack on top, pushing the WIP commit into the PR. Do NOT swallow it to
		// the non-blocking skip — abort the stage hard.
		if rerr := gitResetSoftHEAD1(ctx, repoDir); rerr != nil {
			fatalErr = rerr
			break
		}

		// Fail closed on a passed-but-unresolvable verified tree (#960
		// approval condition): an empty tree would silently disable the
		// pre-push invariant for a verify that DID pass.
		if outcome == "passed" && treeErr != nil {
			verifiedTreeSHA = ""
			fatalErr = fmt.Errorf("%w: committed-tree verify passed for %s but its verified tree could not be resolved: %v",
				gitops.ErrPushedTreeNotVerified, headSHA, treeErr)
			break
		}

		if outcome != "failed" {
			passed = true
			break
		}

		// Testcontainers start-timeout infra-flake absorb (#972): a failed
		// verify whose output carries the container-start-timeout signature is
		// re-run ONCE in place — repeat the iteration via the existing loop
		// body (re-stage, re-commit, re-verify) WITHOUT invoking the fix agent
		// and WITHOUT advancing iter, mirroring the maxFixInvokeInfraRetries
		// budget-preserving pattern below. The once-flag bounds the absorb to
		// one extra verify run per stage; a second flake or a real failure
		// proceeds into the fix-loop / exhaustion path unchanged. This sits
		// AFTER the reset --soft above, so the #816 fatal-reset and #960
		// verified-tree invariants are untouched, and BEFORE the exhaustion
		// check so a flake on the last iteration is absorbed rather than
		// demoting the stage category-A. Never silent: each absorb emits a
		// verify_infra_flake_retry log line + trace event.
		if !flakeRetried && isTestcontainersStartFlake(out) {
			flakeRetried = true
			const detail = "testcontainers container-start timeout signature in verify output; re-running verify once without consuming a fix iteration"
			_, _ = fmt.Fprintf(logSink,
				`{"event":"verify_infra_flake_retry","run_id":%q,"stage_id":%q,"iteration":%d,"detail":%q}`+"\n",
				cfg.runID, cfg.stageID, iter+1, detail)
			res.Events = append(res.Events, agent.Event{
				Kind: "verify_infra_flake_retry",
				Payload: agent.MakePayload(map[string]any{
					"iteration": iter + 1,
					"detail":    detail,
				}),
			})
			iter--
			continue
		}

		if iter == cfg.verifyMaxIterations {
			// Budget exhausted — terminal demotion, no re-invoke.
			break
		}

		// Re-invoke the agent in-place with a fix prompt embedding the captured
		// verify output. The agent edits the same repoDir working tree; the
		// next iteration re-commits scope-only onto the same base.
		reinvoked = true
		_, _ = fmt.Fprintf(logSink,
			`{"event":"verify_fix_reinvoke","run_id":%q,"stage_id":%q,"iteration":%d}`+"\n",
			cfg.runID, cfg.stageID, iter+1)
		fixInv := baseInv
		fixInv.Prompt = verifyFixPrompt(cfg.verifyCmd, out)

		// Bounded infra-retry on the fix re-invocation (#804). A transient
		// agent-API/transport failure (invoker.Invoke returns a non-nil error,
		// e.g. a #798 Claude usage-limit blip) is RETRIED IN PLACE — up to
		// maxFixInvokeInfraRetries total attempts within THIS outer iteration —
		// WITHOUT advancing iter, so a transient blip does not burn a fix-loop
		// budget unit by re-verifying an unchanged tree. The error is never
		// silently swallowed: each failed attempt emits a verify_fix_reinvoke_error
		// log line AND an auditable trace event on res.Events. Only a SUCCESSFUL
		// invoke (fixErr == nil) contributes events/tokens/model to res; an errored
		// zero-value fixRes contributes nothing. Exhausting all attempts is an
		// infra failure that aborts the loop into the non-blocking skip path
		// below (verify_fix_skipped), never a category-A code failure.
		var (
			fixRes agent.Result
			fixErr error
		)
		for attempt := 1; attempt <= maxFixInvokeInfraRetries; attempt++ {
			fixRes, fixErr = invoker.Invoke(ctx, fixInv)
			if fixErr == nil {
				break
			}
			_, _ = fmt.Fprintf(logSink,
				`{"event":"verify_fix_reinvoke_error","run_id":%q,"stage_id":%q,"iteration":%d,"attempt":%d,"detail":%q}`+"\n",
				cfg.runID, cfg.stageID, iter+1, attempt, fixErr.Error())
			res.Events = append(res.Events, agent.Event{
				Kind: "verify_fix_reinvoke_error",
				Payload: agent.MakePayload(map[string]any{
					"iteration": iter + 1,
					"attempt":   attempt,
					"detail":    fixErr.Error(),
				}),
			})
		}
		if fixErr != nil {
			// Every fix-Invoke attempt for this iteration failed on infra. Route
			// through the existing non-blocking skip — the verify already ran and
			// was counted; the iteration counter is NOT advanced.
			lastIterErr = fmt.Errorf("verify-fix: fix re-invocation failed after %d attempts: %w",
				maxFixInvokeInfraRetries, fixErr)
			break
		}
		res.Events = append(res.Events, fixRes.Events...)
		res.TokensUsed += fixRes.TokensUsed
		res.InputTokens += fixRes.InputTokens
		res.OutputTokens += fixRes.OutputTokens
		// Aggregate the cache buckets across the verify-fix re-dispatch too
		// (#1349) — a dropped += would silently under-count cache spend across
		// retries.
		res.CacheReadInputTokens += fixRes.CacheReadInputTokens
		res.CacheWriteInputTokens += fixRes.CacheWriteInputTokens
		if fixRes.Model != "" {
			res.Model = fixRes.Model
		}
	}

	// Emit verify_summary EXACTLY ONCE on every exit path (#804 Gap 2, #816). The
	// outcome must reflect the real exit: a POST-commit reset failure aborts the
	// stage hard, so it is "failed" (carrying the abort detail) and returns a
	// hard error; a PRE-commit infra abort short-circuits into the non-blocking
	// skip below, so it is "skipped" (carrying the abort detail), NOT "failed" —
	// the old `if !passed` form mislabelled the errored-exit path as failed.
	summary := map[string]any{
		"iterations":     attempts,
		"max_iterations": cfg.verifyMaxIterations,
	}
	switch {
	case fatalErr != nil:
		summary["outcome"] = "failed"
		summary["detail"] = fatalErr.Error()
	case lastIterErr != nil:
		summary["outcome"] = "skipped"
		summary["detail"] = lastIterErr.Error()
	case !passed:
		summary["outcome"] = "failed"
	default:
		summary["outcome"] = "passed"
	}
	res.Events = append(res.Events, agent.Event{
		Kind:    "verify_summary",
		Payload: agent.MakePayload(summary),
	})

	if fatalErr != nil {
		// A POST-commit reset failure left HEAD on the throwaway commit (#816),
		// or a passing verify's tree could not be resolved (#960 fail-closed).
		// Abort the stage hard — the call site demotes to category-B so the real
		// push never ships an unverified or throwaway-stacked commit.
		return reinvoked, "", fatalErr
	}

	if lastIterErr != nil {
		// A PRE-commit infra failure inside the loop (git/stage error) is a
		// non-blocking skip — never invent a new failure source from gate
		// plumbing. The stage proceeds to the real push, which runs its own
		// #728/#800 gate.
		_, _ = fmt.Fprintf(logSink,
			`{"event":"verify_fix_skipped","run_id":%q,"stage_id":%q,"detail":%q}`+"\n",
			cfg.runID, cfg.stageID, lastIterErr.Error())
		return reinvoked, "", nil
	}

	if !passed {
		res.OK = false
		res.FailureCategory = "A"
		res.FailureReason = fmt.Sprintf("verify command %q still failing after %d iteration(s):\n%s",
			cfg.verifyCmd, attempts, lastOutput)
		return reinvoked, "", nil
	}
	return reinvoked, verifiedTreeSHA, nil
}

// runVerifyGateCommitted is the single-shot committed-tree verify gate (#802):
// the language-agnostic twin of the #728/#800 Go gate. On the implement push
// path with executor.verify.max_iterations == 0, it runs the configured verify
// command ONCE against the isolated committed SCOPE-ONLY tree (not the agent's
// dirty working tree), so a drift-excluded test failure (#780/#776) is caught
// for ANY language without the fix-loop cost. It reuses the #651 scaffolding:
// StageScoped + a throwaway commit + runVerifyCommittedTree + reset --soft.
//
// Failure handling splits on WHERE HEAD is when the failing op runs:
//
//   - A PRE-commit infra error (StageScoped / commitVerifyWIP / rev-parse) left
//     HEAD untouched, so it is a NON-BLOCKING skip: emit a skipped verify_run +
//     nil error — never invent a failure from gate plumbing. The real push runs
//     its own #728/#800 gate.
//   - A non-zero verify exit whose output matches the testcontainers
//     start-timeout signature (#972, isTestcontainersStartFlake) is re-run
//     ONCE against the same throwaway headSHA before the reset; both
//     verify_run events plus a verify_infra_flake_retry event ship in the
//     returned slice. Only if the retry also fails does the gate classify
//     the failure as below.
//   - A non-zero verify exit returns the events plus an error wrapping
//     gitops.ErrCommittedTestsFailed, naming the drift files + captured output
//     (category-B at the call site, symmetric with #800).
//   - A POST-commit gitResetSoftHEAD1 failure is FATAL, not a skip (#802
//     approval condition). After the throwaway commit is materialized, a failed
//     undo leaves HEAD on the throwaway commit, so openPRAndShipArtifact's real
//     commit would stack on top and push the WIP commit into the PR. Propagate
//     it as a hard error so the stage fails loudly instead of silently shipping
//     a throwaway commit.
//
// On a pass it also returns the verified tree object hash (#960) — captured
// via gitRevParseTreeOf(headSHA) before the reset — which the pre-push
// VerifyCommit hook compares against the real commit's tree. A passing gate
// whose tree cannot be resolved is an inconsistent state and FAILS CLOSED
// (ErrPushedTreeNotVerified, category-B): an empty tree would silently
// disable the invariant. The empty-string/no-enforcement return is reserved
// for the paths where no gate verdict exists (skip, nothing staged, failure).
func runVerifyGateCommitted(ctx context.Context, cfg config, logSink io.Writer) ([]agent.Event, string, error) {
	repoDir := cfg.workingDir
	if repoDir == "" {
		repoDir = "."
	}
	scopeFiles := scopePaths(cfg.scopeFiles)
	timeout := cfg.verifyTimeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}

	// (a) Stage scope-only, capturing the drift list for the failure message.
	// Pre-commit infra error — HEAD never moved; non-blocking skip.
	drift, err := (&gitops.Pusher{}).StageScoped(ctx, repoDir, scopeFiles)
	if err != nil {
		return []agent.Event{verifyRunEvent(cfg.verifyCmd, "", "", -1, "stage_scoped: "+err.Error(), "skipped")}, "", nil
	}

	// (b) Throwaway commit to materialize a committed-HEAD SHA. Still pre-commit
	// from HEAD's perspective on error (the commit failed) → non-blocking skip.
	committed, err := commitVerifyWIP(ctx, cfg, repoDir)
	if err != nil {
		return []agent.Event{verifyRunEvent(cfg.verifyCmd, "", "", -1, "commit_wip: "+err.Error(), "skipped")}, "", nil
	}
	if !committed {
		// Nothing staged — no scope-only change to gate. The real push hits its
		// NoChanges short-circuit; treat as a no-op skip (no demotion).
		return []agent.Event{verifyRunEvent(cfg.verifyCmd, "", "", 0, "no scope-only changes to gate", "skipped")}, "", nil
	}

	// (c) Resolve the throwaway commit's SHA. HEAD has now moved, so any undo
	// failure from here on is FATAL (see (e)).
	headSHA, err := gitRevParseHEAD(ctx, repoDir)
	if err != nil {
		// rev-parse failed but the commit landed — undo it so the working tree
		// is left as the real push expects, then non-blocking skip. A failed
		// undo here is FATAL for the same reason as (e).
		if rerr := gitResetSoftHEAD1(ctx, repoDir); rerr != nil {
			return []agent.Event{verifyRunEvent(cfg.verifyCmd, "", "", -1, "reset_after_revparse: "+rerr.Error(), "failed")}, "", rerr
		}
		return []agent.Event{verifyRunEvent(cfg.verifyCmd, "", "", -1, "rev_parse: "+err.Error(), "skipped")}, "", nil
	}

	// (d) Verify against the committed scope-only tree.
	ev, out, outcome := runVerifyCommittedTree(ctx, cfg.verifyCmd, repoDir, headSHA, timeout)
	events := []agent.Event{ev}

	// Testcontainers start-timeout infra-flake absorb (#972): a failed verify
	// whose output carries the container-start-timeout signature is re-run
	// ONCE against the SAME throwaway headSHA — still before the reset --soft
	// below, so the (e)/(f) invariants are untouched. Both verify_run events
	// plus the verify_infra_flake_retry event ship in the trace. Only if the
	// retry also fails does the gate classify ErrCommittedTestsFailed as
	// before; the single-shot gate has no loop, so the retry is inherently
	// once-per-stage.
	if outcome == "failed" && isTestcontainersStartFlake(out) {
		const detail = "testcontainers container-start timeout signature in verify output; re-running verify once"
		_, _ = fmt.Fprintf(logSink,
			`{"event":"verify_infra_flake_retry","run_id":%q,"stage_id":%q,"iteration":%d,"detail":%q}`+"\n",
			cfg.runID, cfg.stageID, 1, detail)
		events = append(events, agent.Event{
			Kind: "verify_infra_flake_retry",
			Payload: agent.MakePayload(map[string]any{
				"iteration": 1,
				"detail":    detail,
			}),
		})
		ev, out, outcome = runVerifyCommittedTree(ctx, cfg.verifyCmd, repoDir, headSHA, timeout)
		events = append(events, ev)
	}

	// Capture the verified tree's object hash BEFORE the reset (#960). Only a
	// real pass earns a verified tree; the tolerant skip paths return empty
	// (no enforcement). The fail-closed decision on a capture error is made
	// AFTER the reset below — the throwaway commit must be undone either way.
	var verifiedTreeSHA string
	var treeErr error
	if outcome == "passed" {
		verifiedTreeSHA, treeErr = gitRevParseTreeOf(ctx, repoDir, headSHA)
	}

	// (e) ALWAYS undo the throwaway commit so the working tree + index survive
	// for openPRAndShipArtifact's real commit. A reset failure here is FATAL:
	// HEAD is left on the throwaway commit and the real push would stack on top,
	// pushing the WIP commit into the PR. Do NOT swallow it to a skip (#802
	// approval condition) — propagate it so the stage fails loudly.
	if rerr := gitResetSoftHEAD1(ctx, repoDir); rerr != nil {
		return events, "", rerr
	}

	// Fail closed on a passed-but-unresolvable verified tree (#960 approval
	// condition): downgrading to empty would silently disable the pre-push
	// invariant for a gate that DID pass — an inconsistent state, never a
	// tolerated one. Wrapped so it classifies category-B like its siblings.
	if outcome == "passed" && treeErr != nil {
		return events, "", fmt.Errorf("%w: committed-tree gate passed for %s but its verified tree could not be resolved: %v",
			gitops.ErrPushedTreeNotVerified, headSHA, treeErr)
	}

	// (f) Non-zero exit blocks as category-B, symmetric with #800. The infra
	// "skipped" outcome stays tolerant here (reclassification is #959's scope).
	if outcome == "failed" {
		return events, "", fmt.Errorf("%w: committed tree verify command %q failed; %d file(s) outside scope are build/test-required: %s\n%s",
			gitops.ErrCommittedTestsFailed, cfg.verifyCmd, len(drift), strings.Join(drift, ", "), out)
	}
	return events, verifiedTreeSHA, nil
}

// verifyFixPrompt builds the fix-iteration prompt fed back to the agent when
// the committed-tree verify command fails. It embeds the failing command and
// its captured output and instructs the agent to make the tests pass without
// relying on files outside the approved scope.
func verifyFixPrompt(verifyCmd, output string) string {
	return fmt.Sprintf(`The verify command failed against the committed scope-only tree.

Command:
%s

Output:
%s

Edit the code so this command passes. The fix must live in the files you are
already allowed to change (the approved scope) — a change that only works
because of an out-of-scope file will be dropped when the commit is scoped and
will fail verification again. Make the smallest change that turns the command
green.`, verifyCmd, output)
}

// baseRebaseConflictPrompt builds the re-invoke prompt fed to the agent when
// its working-tree edits could not be reapplied onto the freshly-fetched base
// (#989): a sibling's shared-branch commit (or an advanced origin/<base>)
// landed lines the agent also touched, so the stash pop conflicted and was
// cleanly aborted. The agent is re-invoked ONCE on the updated base — which
// already contains the newer commits — with the captured conflict context.
// detail may be nil (a plain ErrBaseRebaseConflict with no typed context, or
// a capture-degraded error); the prompt then omits the context sections.
func baseRebaseConflictPrompt(detail *gitops.BaseRebaseConflictError) string {
	var b strings.Builder
	b.WriteString(`Your previous edits did NOT land: the base branch moved while you worked —
sibling or base commits landed after you started, and re-applying your changes
onto the updated base produced a merge conflict. Your working tree is now
checked out on the UPDATED base, which already contains those newer commits.

Re-land your full original slice on top of the current tree:
- Preserve the changes already committed on the current base (a sibling's
  work). For additive conflicts where both sides added at the same place,
  keep BOTH sides.
- Never revert or overwrite committed work.
- Stay within the approved scope.files — edit only the files you were already
  allowed to change.
`)
	if detail == nil {
		return b.String()
	}
	if len(detail.ConflictPaths) > 0 {
		b.WriteString("\nConflicted paths:\n")
		for _, p := range detail.ConflictPaths {
			b.WriteString("- " + p + "\n")
		}
	}
	if detail.ConflictHunks != "" {
		b.WriteString("\nConflicting hunks (git diff of the aborted re-apply; the upstream side is the updated base, the stashed side is your previous edits):\n```\n")
		b.WriteString(detail.ConflictHunks)
		b.WriteString("\n```\n")
	}
	if detail.StashPatch != "" {
		b.WriteString("\nYour previous (un-landed) changes as a patch:\n```\n")
		b.WriteString(detail.StashPatch)
		b.WriteString("\n```\n")
	}
	return b.String()
}

// reinvokeOnBaseRebaseConflict is the bounded single agent re-invoke on a
// stash-reapply base conflict (#989, proposal 1). The first
// openPRAndShipArtifact attempt failed with ErrBaseRebaseConflict: the agent's
// edits were stashed (preserved on the stash stack), the half-applied pop was
// cleanly aborted, and the local run-branch ref points at the freshly-fetched
// base. This handler re-checks-out that branch and re-invokes the agent with
// the captured conflict context so it re-lands its slice on top of the moved
// base; the caller then retries openPRAndShipArtifact exactly once. The
// re-landed tree differs from the gate-verified tree, so the #960
// verified_tree_mismatch path runs its single strict re-verify on the retry —
// only an explicit pass reaches origin (#969 stamps the re-verified tree).
//
// Returns nil when the agent re-invocation succeeded (events/tokens/model are
// accumulated into res). A non-nil error — checkout failure, infra-retry
// exhaustion (the maxFixInvokeInfraRetries pattern, #804), or the agent
// completing with OK=false (it declined or failed semantically; its trace is
// still accumulated) — means the re-invoke did not produce a usable re-land;
// the caller falls through to the unchanged category-B failure path with the
// ORIGINAL conflict error rather than retrying the push with a tree the agent
// itself did not vouch for.
func reinvokeOnBaseRebaseConflict(ctx context.Context, cfg config, invoker agent.Invoker, baseInv agent.Invocation, res *agent.Result, conflictErr error, logSink io.Writer) error {
	repoDir := cfg.workingDir
	if repoDir == "" {
		repoDir = "."
	}
	routing, err := resolveImplementBranchRouting(ctx, cfg, repoDir, resolveImplementBaseRef(cfg))
	if err != nil {
		return err
	}

	// Extract the typed conflict context. Best-effort: a plain
	// ErrBaseRebaseConflict with no typed wrapper still re-invokes, with the
	// prompt's context sections omitted.
	var detail *gitops.BaseRebaseConflictError
	_ = errors.As(conflictErr, &detail)
	var conflictPaths []string
	if detail != nil {
		conflictPaths = detail.ConflictPaths
	}

	const stashNote = "attempt 1's stashed edits remain on the stash stack (git stash list) for forensics; the re-invoke re-lands the slice as fresh working-tree edits"
	pathsJSON, _ := json.Marshal(conflictPaths)
	_, _ = fmt.Fprintf(logSink,
		`{"event":"base_rebase_conflict_reinvoke","run_id":%q,"stage_id":%q,"branch":%q,"conflict_paths":%s,"note":%q}`+"\n",
		cfg.runID, cfg.stageID, routing.branch, pathsJSON, stashNote)
	res.Events = append(res.Events, agent.Event{
		Kind: "base_rebase_conflict_reinvoke",
		Payload: agent.MakePayload(map[string]any{
			"branch":         routing.branch,
			"conflict_paths": conflictPaths,
			"note":           stashNote,
		}),
	})

	// Put the agent on the fresh base: the run-branch ref already points at
	// the fetched tip (see the function comment), so a plain forced checkout
	// suffices — no fetch, no auth.
	if err := checkoutRunBranch(ctx, repoDir, routing.branch); err != nil {
		return fmt.Errorf("base-rebase re-invoke: checkout run branch %s: %w", routing.branch, err)
	}

	reinvokeInv := baseInv
	reinvokeInv.Prompt = baseRebaseConflictPrompt(detail)

	// Bounded infra-retry on the re-invocation, mirroring the verify-fix
	// loop's maxFixInvokeInfraRetries pattern (#804): a transient agent-API/
	// transport failure is retried in place, never silently — each failed
	// attempt emits a base_rebase_reinvoke_error log line AND an auditable
	// trace event. Only a successful invoke contributes events/tokens/model.
	var (
		reRes agent.Result
		reErr error
	)
	for attempt := 1; attempt <= maxFixInvokeInfraRetries; attempt++ {
		reRes, reErr = invoker.Invoke(ctx, reinvokeInv)
		if reErr == nil {
			break
		}
		_, _ = fmt.Fprintf(logSink,
			`{"event":"base_rebase_reinvoke_error","run_id":%q,"stage_id":%q,"attempt":%d,"detail":%q}`+"\n",
			cfg.runID, cfg.stageID, attempt, reErr.Error())
		res.Events = append(res.Events, agent.Event{
			Kind: "base_rebase_reinvoke_error",
			Payload: agent.MakePayload(map[string]any{
				"attempt": attempt,
				"detail":  reErr.Error(),
			}),
		})
	}
	if reErr != nil {
		return fmt.Errorf("base-rebase re-invoke: agent invocation failed after %d attempts: %w",
			maxFixInvokeInfraRetries, reErr)
	}
	res.Events = append(res.Events, reRes.Events...)
	res.TokensUsed += reRes.TokensUsed
	res.InputTokens += reRes.InputTokens
	res.OutputTokens += reRes.OutputTokens
	// Aggregate the cache buckets across the base-rebase re-invoke too
	// (#1349) — a dropped += would silently under-count cache spend across
	// retries.
	res.CacheReadInputTokens += reRes.CacheReadInputTokens
	res.CacheWriteInputTokens += reRes.CacheWriteInputTokens
	if reRes.Model != "" {
		res.Model = reRes.Model
	}
	if !reRes.OK {
		// The invocation completed but the agent reported failure (declined,
		// errored mid-run, produced no usable re-land). Retrying the push on
		// such a tree could ship partial or unchanged work — abort instead.
		return fmt.Errorf("base-rebase re-invoke: agent completed without success (category %s): %s",
			reRes.FailureCategory, reRes.FailureReason)
	}
	return nil
}

// runVerifyCommittedTree runs the verify command against the committed HEAD
// SHA in an isolated git worktree (#651), reusing the #728/#800 worktree
// pattern: `git worktree add --detach` checks out the committed scope-only
// tree without disturbing the runner's working tree, so the verify command
// sees the drift-excluded tree and not the dirty working tree. It emits a
// verify_run event carrying the committed head_sha + tree_sha and returns the
// captured output plus an explicit outcome string: "passed" (command exited
// zero), "failed" (non-zero exit), or "skipped" (gate infra never ran the
// command). The outcome replaces the former lossy ok bool, whose infra paths
// returned true indistinguishably from a real pass — the verified-SHA
// invariant's strict re-verify (#960) needs "passed" to be unambiguous.
//
// Setpgid + cmd.Cancel kill the whole process group on context cancellation
// so the verify command's grandchildren (e.g. `go test` subprocesses) don't
// keep the output pipe open and block CombinedOutput — the same hazard
// runVerifyGate handles. A worktree-tmp/worktree-add failure (infra) returns
// outcome "skipped"; the gate call sites map that to their existing tolerant
// behavior (no failure invented from gate plumbing — reclassification is
// #959's scope), while the #960 re-verify treats it as not-verified.
func runVerifyCommittedTree(ctx context.Context, verifyCmd, repoDir, headSHA string, timeout time.Duration) (agent.Event, string, string) {
	// Best-effort tree identity for the trace event: the enforcement-grade
	// capture is the gates' fail-closed gitRevParseTreeOf; an empty tree_sha
	// here only degrades the audit stamp, never the invariant.
	treeSHA, _ := gitRevParseTreeOf(ctx, repoDir, headSHA)
	parent, err := os.MkdirTemp("", "fishhawk-verify-*")
	if err != nil {
		return verifyRunEvent(verifyCmd, headSHA, treeSHA, -1, "worktree_tmp: "+err.Error(), "skipped"), "", "skipped"
	}
	wt := filepath.Join(parent, "tree")
	defer func() {
		_ = exec.CommandContext(ctx, "git", "-C", repoDir, "worktree", "remove", "--force", wt).Run()
		_ = os.RemoveAll(parent)
	}()
	if out, err := exec.CommandContext(ctx, "git", "-C", repoDir,
		"worktree", "add", "--detach", wt, headSHA).CombinedOutput(); err != nil {
		return verifyRunEvent(verifyCmd, headSHA, treeSHA, -1,
			"worktree_add: "+strings.TrimSpace(string(out)), "skipped"), "", "skipped"
	}

	childCtx, childCancel := context.WithTimeout(ctx, timeout)
	defer childCancel()
	cmd := exec.CommandContext(childCtx, "sh", "-c", verifyCmd)
	cmd.Dir = wt
	// Strip runner credentials from the gate subprocess env (ADR-029 #650
	// item 4): the committed-tree verify command runs agent-authored code.
	cmd.Env = sanitizedGateEnv()
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
	if exitCode != 0 {
		outcome = "failed"
	}
	return verifyRunEvent(verifyCmd, headSHA, treeSHA, exitCode, string(output), outcome), string(output), outcome
}

// verifyRunEvent builds the verify_run trace event for a committed-tree verify
// run, carrying the committed head_sha and its tree_sha (#960 — the tree the
// gate actually ran against; empty on the pre-commit skip paths) alongside the
// command/exit/output the working-tree gate (runVerifyGate) emits.
func verifyRunEvent(command, headSHA, treeSHA string, exitCode int, output, outcome string) agent.Event {
	return agent.Event{
		Kind: "verify_run",
		Payload: agent.MakePayload(map[string]any{
			"command":   command,
			"head_sha":  headSHA,
			"tree_sha":  treeSHA,
			"exit_code": exitCode,
			"output":    output,
			"outcome":   outcome,
		}),
	}
}

// commitVerifyWIP stages nothing new (the caller already ran StageScoped) and
// makes a throwaway local commit to materialize a committed-HEAD SHA for the
// verify-fix loop. It returns committed=false (and no error) when there is
// nothing staged to commit, so a no-op stage doesn't error the loop. The bot
// identity + gpgsign=false + --no-verify keep it hermetic and hook-free.
func commitVerifyWIP(ctx context.Context, cfg config, repoDir string) (committed bool, err error) {
	// Nothing staged → nothing to gate.
	if exec.CommandContext(ctx, "git", "-C", repoDir, "diff", "--cached", "--quiet").Run() == nil {
		return false, nil
	}
	name := cfg.commitAuthorName
	if name == "" {
		name = gitops.DefaultAuthorName
	}
	email := cfg.commitAuthorEmail
	if email == "" {
		email = gitops.DefaultAuthorEmail
	}
	out, cerr := exec.CommandContext(ctx, "git", "-C", repoDir,
		"-c", "user.name="+name,
		"-c", "user.email="+email,
		"-c", "commit.gpgsign=false",
		"commit", "--no-verify", "-m", "fishhawk verify wip").CombinedOutput()
	if cerr != nil {
		return false, fmt.Errorf("verify-fix: throwaway commit: %s", strings.TrimSpace(string(out)))
	}
	return true, nil
}

// gitRevParseHEAD returns the current HEAD SHA of repoDir.
func gitRevParseHEAD(ctx context.Context, repoDir string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("verify-fix: rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitRevParseTreeOf resolves rev to its tree object hash via the
// gitrevisions(7) `<rev>^{tree}` peel syntax. The tree hash is the
// content-addressed identity of the snapshot (content + modes + paths,
// independent of commit metadata), so two commits with equal tree hashes
// are byte-identical trees — the equivalence the verified-SHA invariant
// (#960) compares on.
func gitRevParseTreeOf(ctx context.Context, repoDir, rev string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "rev-parse", rev+"^{tree}").Output()
	if err != nil {
		return "", fmt.Errorf("verify-gate: rev-parse %s^{tree}: %w", rev, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitResetSoftHEAD1 undoes the most recent commit with `git reset --soft
// HEAD~1`, which moves HEAD back one commit WITHOUT touching the index or the
// working tree — so the staged scope and the agent's edits both survive. This
// is what lets each verify-fix iteration re-commit scope-only onto the same
// base rather than stacking fix commits.
func gitResetSoftHEAD1(ctx context.Context, repoDir string) error {
	out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "reset", "--soft", "HEAD~1").CombinedOutput()
	if err != nil {
		return fmt.Errorf("verify-fix: reset --soft HEAD~1: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// gitApply3Way applies a single unified-diff patch to repoDir's working tree
// via `git apply --3way`, reading the patch from stdin (#1165). --3way falls
// back to a 3-way merge using the blob index when straight application fails,
// and exits non-zero — leaving the tree unmodified for a non-applying hunk —
// when a hunk cannot be reconciled, which is the apply-failure fallback signal
// the deterministic fix-up path treats as "re-derive with the agent instead".
func gitApply3Way(ctx context.Context, repoDir, patch string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "apply", "--3way", "-")
	cmd.Stdin = strings.NewReader(patch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git apply --3way: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// gitResetHardClean restores repoDir's working tree to ref with `git reset
// --hard <ref>` followed by `git clean -fd` (#1165), discarding any
// half-applied patch — both tracked edits AND the untracked new files a CREATE
// patch left behind. It is the fail-safe reset the deterministic fix-up path
// runs before falling through to the agent, so the agent never inherits a
// partially-applied tree.
func gitResetHardClean(ctx context.Context, repoDir, ref string) error {
	if out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "reset", "--hard", ref).CombinedOutput(); err != nil {
		return fmt.Errorf("git reset --hard %s: %s", ref, strings.TrimSpace(string(out)))
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "clean", "-fd").CombinedOutput(); err != nil {
		return fmt.Errorf("git clean -fd: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// attemptDeterministicFixup runs the near-deterministic fix-up apply path
// (#1165). For each routed concern's suggested_patch it runs `git apply --3way`
// against the already-checked-out PR branch (working tree at baseTipSHA); on a
// clean apply of EVERY patch it runs the committed-tree verify gate. It returns
// applied=true with the verified tree SHA ONLY when every patch applied AND the
// gate passed — the caller then skips the agent spawn and commits/pushes the
// applied tree via the existing fixup_pushed path. On ANY failure mode (a patch
// did not apply cleanly, the post-apply verify gate failed, or the gate
// produced no verifiable tree) it resets the worktree to baseTipSHA and returns
// applied=false so the caller falls through to the unchanged agent fix-up path
// — never a half-applied tree. The returned events carry the apply/verify
// trace; applyPath is "applied" on success or "apply_failed_fellback" on a
// fallback whose reset succeeded.
//
// applyPath is also the fail-safe escalation channel: if the post-failure
// worktree reset itself fails, the tree may be HALF-APPLIED and the caller must
// NOT continue into the agent path (which would run, commit, and push against a
// corrupted tree). On a reset failure attemptDeterministicFixup returns
// applyPath "apply_failed_reset_failed" (applied=false) and the caller fails the
// stage loud — the only way to honour "never ship a half-applied tree" when the
// reset that would have made the fall-through safe did not happen.
func attemptDeterministicFixup(ctx context.Context, cfg config, patches []upload.FixupApplyPatch, baseTipSHA string, logSink io.Writer) (applied bool, verifiedTreeSHA string, evs []agent.Event, applyPath string) {
	repoDir := cfg.workingDir
	if repoDir == "" {
		repoDir = "."
	}
	fallback := func(reason, detail string) (bool, string, []agent.Event, string) {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"fixup_apply_fallback","run_id":%q,"stage_id":%q,"reason":%q,"detail":%q}`+"\n",
			cfg.runID, cfg.stageID, reason, detail)
		if rerr := resetFixupWorktree(ctx, repoDir, baseTipSHA); rerr != nil {
			// A failed reset may leave a half-applied tree. This is the one edge
			// where falling through to the agent is UNSAFE — the agent would run,
			// commit, and push against a corrupted tree. Signal the caller via the
			// distinct applyPath sentinel so it fails the stage loud rather than
			// continuing.
			_, _ = fmt.Fprintf(logSink,
				`{"event":"fixup_apply_reset_failed","run_id":%q,"stage_id":%q,"detail":%q}`+"\n",
				cfg.runID, cfg.stageID, rerr.Error())
			return false, "", evs, "apply_failed_reset_failed"
		}
		return false, "", evs, "apply_failed_fellback"
	}
	for i, p := range patches {
		if err := applyFixupPatch(ctx, repoDir, p.Patch); err != nil {
			return fallback("patch_did_not_apply", fmt.Sprintf("patch %d/%d: %v", i+1, len(patches), err))
		}
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"fixup_patches_applied","run_id":%q,"stage_id":%q,"patch_count":%d}`+"\n",
		cfg.runID, cfg.stageID, len(patches))
	gateEvs, tree, demote := fixupVerifyGate(ctx, cfg, logSink)
	evs = append(evs, gateEvs...)
	if demote != nil {
		return fallback("verify_gate_failed", demote.Error())
	}
	if tree == "" {
		// The gate skipped (no scope-only change to verify) — the apply produced
		// nothing the committed-tree gate could confirm. Don't ship an unverified
		// apply; reset and let the agent re-derive.
		return fallback("verify_gate_no_tree", "committed-tree verify gate produced no verifiable tree")
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"fixup_apply_verified","run_id":%q,"stage_id":%q,"verified_tree_sha":%q}`+"\n",
		cfg.runID, cfg.stageID, tree)
	return true, tree, evs, "applied"
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

	// untrackedPaths is the gitops primitive categorizeDrift partitions
	// drift with. Test seam: gitops.UntrackedPaths only fails when git
	// ls-files itself fails, which a temp-repo test cannot force while
	// StageScoped succeeds in the same computeAndEmitDiff call — tests
	// swap in a failing function to exercise the categorize-failure
	// degradation branch (log + fall back to the uncategorized payload).
	untrackedPaths = gitops.UntrackedPaths

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

	// remoteHasBranch is the REMOTE-AUTHORITATIVE branch-existence seam used by
	// the wave-N decomposed-child base-establishment block (#1363). Unlike
	// remoteBranchExists (git show-ref against refs/remotes/origin/<branch> —
	// LOCAL tracking refs only), it queries the actual remote via git
	// ls-remote, so the consolidated wave base — created on GitHub via the API
	// during integrate-wave and NEVER fetched into local tracking refs — is
	// seen immediately. Wired DIRECTLY to gitops.RemoteHasBranch (no closure)
	// so the #1363 wiring/identity test can assert it has not silently reverted
	// to the local-tracking show-ref guard (which would reintroduce the
	// wave-N-on-main defect). It returns (exists, error): the wave-base block
	// treats a query ERROR as fail-loud (a transient ls-remote failure must NOT
	// degrade to running a dependent slice against ambient HEAD), distinct from
	// a successful empty result (branch genuinely absent → graceful skip,
	// preserving the #1302 degrade contract).
	remoteHasBranch = gitops.RemoteHasBranch

	// remoteConfigured is the not-wired-vs-transient discriminator the wave-base
	// block consults ONLY when remoteHasBranch errors (#1363). A remoteHasBranch
	// failure against a remote that is NOT configured — a bare local-runner
	// checkout with no origin — is the "GitHub not wired" degrade state (#1302),
	// which must gracefully skip to ambient HEAD, NOT fail loud; a transient
	// failure against a CONFIGURED remote stays fail-loud. A package-level var
	// for the same reason as remoteHasBranch: fake-pusher run() tests default
	// repoDir to "." and must never probe the runner's own source repo.
	remoteConfigured = gitops.RemoteConfigured

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

	// captureHead / restoreHead are the working-tree restoration seam
	// (#911). Production wires them to the real gitops helpers, which run
	// `git symbolic-ref` / `git checkout --force` against the operator's
	// checkout. They are package-level vars (not direct gitops calls) ONLY
	// so the fake-pusher run() tests — which default repoDir to "." (the
	// runner's own source repo) — can swap in safe no-ops via
	// withFakeGitOps; without the seam those tests would force-checkout the
	// real working tree and discard uncommitted work. The real helpers are
	// exercised directly by the gitops unit tests against throwaway repos.
	captureHead = gitops.CaptureHead
	restoreHead = gitops.RestoreHead

	// dirtyPaths / cleanDriftPaths / restoreHeadPreserving are the #943
	// drift-cleanup seam. dirtyPaths snapshots the pre-agent dirty set in
	// run(); after a successful implement push, cleanDriftPaths reverts the
	// agent-introduced subset of the scope drift (pathspec-limited stash
	// push + drop) and restoreHeadPreserving carries the operator's
	// pre-existing edits across the forced HEAD restore that previously
	// discarded them. Package-level vars for the same reason as captureHead
	// / restoreHead: the fake-pusher run() tests default repoDir to "." and
	// must never stash/checkout the runner's own source repo. The real
	// helpers are exercised by the gitops unit tests against throwaway repos.
	dirtyPaths            = gitops.DirtyPaths
	cleanDriftPaths       = gitops.CleanDriftPaths
	restoreHeadPreserving = gitops.RestoreHeadPreserving

	// checkoutFixupBase is the fix-up base-establishment seam (#967).
	// Production wires it to gitops.CheckoutRemoteBranch, which fetches the
	// run's PR branch from the named remote and checks the working tree out
	// onto the fetched tip, returning that tip SHA for the ADR-035 lineage
	// comparison. A package-level var for the same reason as captureHead /
	// restoreHead: the fake-pusher run() tests default repoDir to "." and
	// must never fetch + force-checkout the runner's own source repo.
	checkoutFixupBase = gitops.CheckoutRemoteBranch

	// checkoutChildBase is the subsequent-decomposed-child base-establishment
	// seam (#1036), the decomposition analogue of checkoutFixupBase:
	// production fetches the shared parent branch from the named remote and
	// checks the working tree out onto the fetched tip so the agent runs
	// against its declared policy base (#765), returning that tip SHA for the
	// child_base_established record. A package-level var for the same reason
	// as checkoutFixupBase: the fake-pusher run() tests default repoDir to
	// "." and must never fetch + force-checkout the runner's own source repo.
	//
	// Wired to gitops.CheckoutRemoteBranchDetached (NOT the on-branch
	// CheckoutRemoteBranch used by checkoutFixupBase) so the child base is a
	// DETACHED HEAD at the base tip (#1361). The on-branch `checkout -B main`
	// claims the gitdir-global `main` branch name, which git refuses when the
	// operator's primary checkout already holds `main` in another linked
	// worktree — failing at child_base_checkout for BOTH the run_children
	// --parallel-isolate child (own run-<child> worktree) and the host
	// fishhawk_dispatch_stage child (shared run-<parent> worktree). A detached
	// HEAD claims no branch name, so it never collides; the per-slice
	// sole-writer branch is cut later by CommitAndPush's freshFetchBase routing
	// (ADR-035), which does not require HEAD to be ON the base branch.
	checkoutChildBase = gitops.CheckoutRemoteBranchDetached

	// checkoutRunBranch re-checks-out the run branch for the base-rebase-
	// conflict re-invoke (#989). The local branch ref already points at the
	// freshly-fetched base — CommitAndPush ran `checkout -B <branch>
	// FETCH_HEAD` before the conflicted pop, and the restore defer only moved
	// HEAD off the ref, never deleted it — so production reuses
	// gitops.RestoreHead (`git checkout --force <ref>`); the tree is clean
	// after popStash's reset --hard abort. A package-level var for the same
	// reason as restoreHead: the fake-pusher run() tests default repoDir to
	// "." and must never force-checkout the runner's own source repo.
	checkoutRunBranch = gitops.RestoreHead

	// applyFixupPatch / resetFixupWorktree / fixupVerifyGate are the
	// near-deterministic fix-up apply seam (#1165). Production wires them to the
	// real exec-based git helpers and the committed-tree verify gate;
	// attemptDeterministicFixup calls them through these vars so a unit test can
	// drive each enumerated failure mode (patch-did-not-apply, verify-gate-fail,
	// no-verifiable-tree, happy apply) without a live git repo or verify command.
	// Package-level vars for the same reason as checkoutFixupBase: the
	// fake-pusher run() tests default repoDir to "." and must never apply
	// patches to or reset the runner's own source repo.
	applyFixupPatch    = gitApply3Way
	resetFixupWorktree = gitResetHardClean
	fixupVerifyGate    = runVerifyGateCommitted
)

// fixupCheckoutFailReason classifies a checkoutFixupBase failure into the
// runner_failed reason token. A gitops.ErrBranchCheckedOutElsewhere failure
// (the run branch is checked out in another worktree — typically the
// operator's main tree) maps to the self-diagnosing "fixup_base_worktree_
// conflict" whose wrapped message names the blocking path and recovery (#1549);
// every other checkout failure keeps the generic "fixup_base_checkout".
func fixupCheckoutFailReason(err error) string {
	if errors.Is(err, gitops.ErrBranchCheckedOutElsewhere) {
		return "fixup_base_worktree_conflict"
	}
	return "fixup_base_checkout"
}

// compileDiagnosticMarkers are substrings that positively identify a
// Go compile / typecheck diagnostic in `go vet` output. Since #959 they
// are a BACKSTOP behind goDiagnosticBlocks, not the primary classifier:
// the gate blocks on any clean compiler diagnostic line (the regexp),
// and these markers additionally catch diagnostic shapes the regexp
// could miss. The marker-allowlist-only era chased the open-ended
// typecheck-message tail one production bug at a time — #774 added the
// selector-on-type form 'X undefined (type Y has no field or method
// Z)' ("undefined (" + "has no field or method") after #762 child D,
// and #959's 'unknown field ... in struct literal' slipped past the
// list again, letting an uncompilable head push (run 07bce059).
var compileDiagnosticMarkers = []string{
	"does not implement",
	"cannot use",
	"undefined:",
	"undefined (",
	"has no field or method",
	"missing method",
	"undeclared name",
	"redeclared",
	"not enough arguments",
	"too many arguments",
	"is not a type",
	"cannot convert",
}

// looksLikeCompileError reports whether `go vet` output contains a
// known compile / typecheck diagnostic marker. Since #959 it is the
// backstop half of the gate's block condition — the primary classifier
// is goDiagnosticBlocks; see compileDiagnosticMarkers.
func looksLikeCompileError(output string) bool {
	for _, m := range compileDiagnosticMarkers {
		if strings.Contains(output, m) {
			return true
		}
	}
	return false
}

// goDiagnosticLine matches a Go compiler / typecheck diagnostic line:
// `path/to/file.go:LINE:COL: message`, optionally prefixed with `vet: `
// (go vet prefixes typecheck/load errors that way; analyzer findings
// print the bare form under a `# pkg` header). The COLUMN field is the
// discriminator that keeps `go test` log lines (`file_test.go:42: msg`,
// no column) and testcontainers/infra noise out: only the toolchain
// emits the three-field form. The trailing capture group is the
// diagnostic message, which goDiagnosticBlocks checks against
// depResolutionMarkers.
var goDiagnosticLine = regexp.MustCompile(`(?m)^(?:vet: )?[^\s:]+\.go:\d+:\d+: (.+)$`)

// depResolutionMarkers identify the dependency-resolution failure
// class WITHIN diagnostic-shaped lines. This exclusion is load-bearing:
// a cold module cache / offline run prints dep failures in the same
// `file.go:LINE:COL: message` form as a compile error (e.g.
// `use_test.go:6:2: no required module provides package example.com/x;
// to add it: go get ...` under GOPROXY=off), so the regexp alone would
// false-block on gate infrastructure, violating the #728/#800
// infra-skip contract.
var depResolutionMarkers = []string{
	"no required module provides",
	"missing go.sum entry",
	"cannot find module",
	"cannot find package",
	"cannot query module",
}

// goDiagnosticBlocks reports whether toolchain output carries a clean
// Go compiler / typecheck diagnostic the committed-tree gates must
// BLOCK on (#959): at least one line matches goDiagnosticLine and that
// line's message is not a recognized dependency-resolution form. This
// replaces marker-only classification as the primary block condition —
// a definitive `file.go:LINE:COL:` diagnostic is evidence the tree
// being pushed does not compile, regardless of whether the message text
// is in compileDiagnosticMarkers. A skip now requires evidence the
// toolchain itself failed (no parseable diagnostic, or dep-resolution-
// only diagnostics), not merely a nonzero exit with an unrecognized
// message — the #959 misclassification that let run 07bce059 push an
// uncompilable head.
func goDiagnosticBlocks(output string) bool {
	for _, m := range goDiagnosticLine.FindAllStringSubmatch(output, -1) {
		msg := m[1]
		dep := false
		for _, dm := range depResolutionMarkers {
			if strings.Contains(msg, dm) {
				dep = true
				break
			}
		}
		if !dep {
			return true
		}
	}
	return false
}

// testFailureMarkers are substrings that positively identify a GENUINE
// `go test` failure to block on (#800): `--- FAIL` is a failed
// assertion / sub-test; `panic:` is a test panic. Since #959 these are
// the FIRST of two block conditions in the test phase — a `FAIL <pkg>
// [build failed]` accompanied by a clean compiler diagnostic line now
// also blocks (goDiagnosticBlocks → ErrCommitWouldNotCompile) instead
// of skipping as infra, because a definitive compile failure of the
// tree being pushed is not gate infrastructure. Still-skipping shapes:
// `[setup failed]`, bare `[build failed]` with no diagnostic line,
// dep-resolution errors, `ok\t<pkg>`, `no test files`, exec/OOM noise.
// The skip-on-uncertainty asymmetry of #728/#800 narrows accordingly:
// uncertain means NO parseable diagnostic, not an unrecognized
// diagnostic message — and a block routes to verify_fix_reinvoke (an
// in-place agent fix) rather than killing the stage outright, so the
// false-block cost that originally justified the wide skip no longer
// holds.
var testFailureMarkers = []string{
	"--- FAIL",
	"panic:",
}

// looksLikeTestFailure reports whether `go test` output contains a
// genuine test failure (a failed assertion or a panic), as opposed to a
// test-binary build/setup failure or a dependency-resolution error. Used
// to separate a real test failure (block, category-B) from gate
// infrastructure failures (skip, non-blocking).
func looksLikeTestFailure(output string) bool {
	for _, m := range testFailureMarkers {
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
// on a genuine compile failure: any clean compiler/typecheck diagnostic
// line (goDiagnosticBlocks, #959) or a known diagnostic marker
// (looksLikeCompileError, the backstop). Every infrastructure problem —
// no drift to drop, no go.work (non-Go repo), no Go toolchain, a
// worktree or `go work edit` failure, or a dependency-resolution
// failure — is a NON-blocking skip (logged as compile_gate_skipped,
// returns nil), so the gate never becomes a new failure source in
// non-Go or misconfigured environments. A skip requires the failure to
// look like toolchain/dependency infrastructure (no parseable
// diagnostic, or dep-resolution-only diagnostics) — NOT merely a
// nonzero exit with a message outside the marker list, the #959
// misclassification (#728/#800/#774 history).
//
// After the per-module `go vet` compile check passes, it runs a second
// phase (#800): `go test` on the touched packages (the packages
// containing the DECLARED scopeFiles, NOT the drift dirs — drift is
// excluded from the committed tree) in the SAME isolated committed-HEAD
// worktree. This catches the drift-excluded-test-failure class
// (#780/#776) where a scope-only commit compiles but a necessary test
// fake/helper was dropped as scope drift, so the committed tree's tests
// are red while the agent's full working tree is green. A genuine test
// failure (`--- FAIL`/`panic:`) returns ErrCommittedTestsFailed; a test
// package that fails to BUILD with a clean compiler diagnostic returns
// ErrCommitWouldNotCompile (#959 — `[build failed]` plus a
// file:line:col diagnostic is a definitive compile failure of the tree
// being pushed, not infra); only genuine test-infrastructure failures
// (deps unresolvable, setup failure, exec error, no parseable
// diagnostic) are a non-blocking skip (test_gate_skipped). The test
// phase runs ONLY on the drift-present path — the len(drift)==0 and
// empty-scope fast paths return before it.
func verifyCommittedTreeCompiles(ctx context.Context, repoDir, headSHA string, drift, scopeFiles []string, logSink io.Writer) error {
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
	// Gate subprocess running on the committed agent tree — strip runner
	// credentials from its env (ADR-029 #650 item 4).
	workCmd.Env = sanitizedGateEnv()
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
		vetCmd.Env = sanitizedGateEnv()
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
		// vet ran and exited non-zero. Block on any clean compiler /
		// typecheck diagnostic line (goDiagnosticBlocks, #959) or a known
		// diagnostic marker (looksLikeCompileError, the backstop); a
		// dependency-resolution failure (cold cache / no network) or a
		// nonzero exit with no parseable diagnostic is a non-blocking skip.
		// Note this means a vet ANALYZER finding (plain file:line:col under
		// a `# pkg` header) now blocks too — deliberate (#959): CI's
		// golangci-lint bundles govet so the PR would red-line anyway, and
		// a block routes to verify_fix_reinvoke rather than failing the
		// stage. `continue` (not return) so a skipped failure in ONE module
		// doesn't abandon the gate — a later module may still carry the
		// real build-required-drift compile error.
		vetOut := strings.TrimSpace(string(out))
		if !looksLikeCompileError(vetOut) && !goDiagnosticBlocks(vetOut) {
			skip("vet_nonzero_non_compile", vetOut)
			continue
		}
		return fmt.Errorf("%w: PR would not compile; %d file(s) outside scope are build-required: %s\n%s",
			gitops.ErrCommitWouldNotCompile, len(drift), strings.Join(drift, ", "), vetOut)
	}

	// Test phase (#800): the committed tree compiles, but a dropped
	// scope-drift test fake/helper can still leave a touched package's
	// tests red. Run `go test` on the packages containing the declared
	// scope files (NOT the drift dirs — those are excluded from the tree)
	// in the SAME committed-HEAD worktree. Plain `go test` (no -race): CI
	// runs -race separately; this is the fast touched-package pre-push
	// check. Empty scope (plan_missing `git add -A` fallback) → nothing to
	// derive packages from, skip.
	if len(scopeFiles) == 0 {
		return nil
	}
	skipTest := func(reason, detail string) {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"test_gate_skipped","head_sha":%q,"reason":%q,"detail":%q}`+"\n",
			headSHA, reason, detail)
	}
	for _, m := range workspace.Use {
		pkgArgs := touchedPackageArgs(m.DiskPath, scopeFiles)
		if len(pkgArgs) == 0 {
			continue
		}
		testCmd := exec.CommandContext(ctx, "go", append([]string{"test"}, pkgArgs...)...)
		testCmd.Dir = filepath.Join(wt, m.DiskPath)
		testCmd.Env = sanitizedGateEnv()
		out, terr := testCmd.CombinedOutput()
		if terr == nil {
			continue
		}
		var exitErr *exec.ExitError
		if !errors.As(terr, &exitErr) {
			// go test never ran (go missing / exec start failure) — infra-skip.
			skipTest("test_exec", terr.Error())
			continue
		}
		// go test ran and exited non-zero. A positively identified test
		// failure (`--- FAIL`/`panic:`) blocks first; then a `[build
		// failed]` accompanied by a clean compiler diagnostic line is a
		// definitive compile failure of the tree being pushed, not infra,
		// and blocks as ErrCommitWouldNotCompile (#959). Only outputs with
		// neither — `[setup failed]`, dep resolution, exec/OOM noise —
		// skip as gate infrastructure.
		testOut := strings.TrimSpace(string(out))
		if looksLikeTestFailure(testOut) {
			return fmt.Errorf("%w: committed tree tests fail; %d file(s) outside scope are build/test-required: %s\n%s",
				gitops.ErrCommittedTestsFailed, len(drift), strings.Join(drift, ", "), testOut)
		}
		if goDiagnosticBlocks(testOut) {
			return fmt.Errorf("%w: committed tree's test packages do not compile; %d file(s) outside scope are build-required: %s\n%s",
				gitops.ErrCommitWouldNotCompile, len(drift), strings.Join(drift, ", "), testOut)
		}
		skipTest("test_nonzero_non_failure", testOut)
	}
	return nil
}

// touchedPackageArgs derives the `go test` package arguments for one
// go.work module from the declared scope files (#800). For each scope
// file that falls under the module's DiskPath, it collects the distinct
// module-relative directory as a `./<reldir>/...` pattern (a root-level
// file maps to `.`), bounding the test phase to the touched packages
// rather than the whole module (`./...`). Returns nil when no scope file
// lies under the module, so non-Go scope files in other modules neither
// block nor false-skip.
func touchedPackageArgs(diskPath string, scopeFiles []string) []string {
	modPrefix := filepath.ToSlash(filepath.Clean(diskPath))
	if modPrefix == "." {
		modPrefix = ""
	}
	seen := map[string]bool{}
	var args []string
	for _, f := range scopeFiles {
		f = filepath.ToSlash(filepath.Clean(f))
		var rel string
		switch {
		case modPrefix == "":
			rel = f
		case f == modPrefix:
			rel = "."
		case strings.HasPrefix(f, modPrefix+"/"):
			rel = strings.TrimPrefix(f, modPrefix+"/")
		default:
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		var arg string
		if dir == "." {
			arg = "."
		} else {
			arg = "./" + dir + "/..."
		}
		if !seen[arg] {
			seen[arg] = true
			args = append(args, arg)
		}
	}
	return args
}

// resolveImplementBaseRef resolves the PR base branch for the implement push
// path: --base-branch flag > GITHUB_REF_NAME env > "main", the same
// flag > env > default precedence as the repo lookup. Shared by
// openPRAndShipArtifact and the base-rebase-conflict re-invoke handler (#989).
func resolveImplementBaseRef(cfg config) string {
	baseRef := cfg.baseBranch
	if baseRef == "" {
		baseRef = os.Getenv("GITHUB_REF_NAME")
	}
	if baseRef == "" {
		baseRef = "main"
	}
	return baseRef
}

// implementBranchRouting is the resolved branch routing for an implement
// push: which branch the stage commits to and which of the three mutually
// exclusive paths (fix-up / decomposed child / standalone) it is on.
// Factored out of openPRAndShipArtifact so the base-rebase-conflict
// re-invoke handler (#989) can resolve the same run branch to re-checkout
// without duplicating the routing logic.
type implementBranchRouting struct {
	branch       string
	isDecomposed bool
	isSubsequent bool
	isFixup      bool
	// freshFetchBase, when non-empty, makes CommitAndPush cut the run
	// branch from a freshly-fetched origin/<base> instead of ambient HEAD
	// (ADR-035 prevention, #861). Set in the standalone default case and on
	// the decomposed-first-child case that creates the shared branch (#865).
	freshFetchBase string
}

// The ctx/repoDir params are unused under ADR-041 (#1141): routing no longer
// consults remoteBranchExists for a decomposed child (each child cuts a fresh
// sole-writer slice branch). They are retained — passed by every caller and
// named `_` — so a future fan-in variant (E24.2) that needs the remote read
// can re-enable it without a signature/call-site change.
func resolveImplementBranchRouting(_ context.Context, cfg config, _, baseRef string) (implementBranchRouting, error) {
	var r implementBranchRouting
	switch {
	case cfg.fixup:
		// Fix-up pass (#762): commit onto the EXISTING PR branch and rebase
		// it from the remote first (fetch + checkout + pull --rebase), the
		// same shared-branch path subsequent decomposed children use. The
		// open PR tracks this branch, so the pushed fix-up commit updates it
		// — no OpenPR (a PR already exists for head→base), no fresh artifact.
		r.isFixup = true
		r.branch = cfg.fixupBranch
		r.isSubsequent = true
	case cfg.decomposedFromRunID != "":
		// Per-child sole-writer slice branch (E24.1 / #1141 / ADR-041
		// point 1): each child pushes onto its own
		// fishhawk/run-<parent>/slice-<n>, replacing the pre-ADR-041
		// shared fishhawk/run-<parent> branch every sibling force-pushed.
		// isDecomposed stays true so the scope-gate exemptions and the
		// child_pushed audit/report path (both keyed on it) are preserved;
		// only the push MECHANICS decouple. A sole-writer slice branch is
		// minted once and so never pre-exists on the remote — cut it fresh
		// from the freshly-fetched authoritative base (ADR-035 prevention,
		// #861/#865) and leave isSubsequent false so RebaseFromRemote is
		// false (no prior sibling commit to rebase onto; fan-in is E24.2).
		r.isDecomposed = true
		r.branch = childSliceBranch(cfg.decomposedFromRunID, runSliceIndex)
		r.freshFetchBase = baseRef
	default:
		r.branch = fmt.Sprintf("fishhawk/run-%s/stage-%s", shortID(cfg.runID), shortID(cfg.stageID))
		// Standalone single-writer run: cut the branch from the freshly-
		// fetched authoritative base so a foreign ambient-HEAD commit (#797)
		// can't become the recorded fork point (ADR-035 prevention, #861).
		r.freshFetchBase = baseRef
	}
	if r.isFixup && r.branch == "" {
		return r, errors.New("upload: fix-up pass requires a non-empty existing PR branch (fixup_branch)")
	}
	return r, nil
}

// mintImplementToken resolves the push/PR-create credential for an implement
// stage. It always mints a fresh App installation token (App tokens are
// ~1-hour TTL and a long agent run can outlive the auth pre-step's token);
// when no App installation is attributed to the run (a local / MCP run on a
// repo with no App) it falls back to the operator's local `gh` CLI token
// (#713) — a user OAuth token authenticates both `git push` over HTTPS and the
// REST PR-create call. Shared by the open-PR push path (openPRAndShipArtifact)
// and the zero-re-run exempt resolution (openHeldCommitPR, #1231) so both
// resolve the credential identically.
func mintImplementToken(ctx context.Context, cfg config, client uploadClient, issued *upload.IssuedKey, logSink io.Writer) (string, error) {
	tokenRes, err := client.FetchInstallationToken(ctx, upload.FetchInstallationTokenArgs{
		RunID:      cfg.runID,
		StageID:    cfg.stageID,
		PrivateKey: issued.PrivateKey,
	})
	switch {
	case err == nil:
		_, _ = fmt.Fprintf(logSink,
			`{"event":"installation_token_received","run_id":%q,"stage_id":%q,"source":"backend"}`+"\n",
			cfg.runID, cfg.stageID,
		)
		return tokenRes.Token, nil
	case errors.Is(err, upload.ErrNoInstallation):
		ghTok, ghErr := ghAuthToken(ctx)
		if ghErr != nil {
			repoHint := cfg.githubRepo
			if repoHint == "" {
				repoHint = os.Getenv("GITHUB_REPOSITORY")
			}
			if repoHint == "" {
				repoHint = "the target repo"
			}
			return "", fmt.Errorf("this run has no GitHub App installation and no `gh` CLI token is available for the push + PR fallback; "+
				"either install the Fishhawk GitHub App on %s, or run `gh auth login` so the runner can use your local token: %w",
				repoHint, ghErr)
		}
		_, _ = fmt.Fprintf(logSink,
			`{"event":"installation_token_received","run_id":%q,"stage_id":%q,"source":"gh_cli"}`+"\n",
			cfg.runID, cfg.stageID,
		)
		return ghTok, nil
	default:
		return "", fmt.Errorf("fetch installation token: %w", err)
	}
}

// openHeldCommitPR resolves an operator EXEMPT decision on a scope-completeness
// park (#1231) with ZERO agent re-invocation. The implement stage previously
// parked because the missing-declared-scope-file gate was its sole failure, and
// the runner pushed the gate-verified commit to heldBranch at heldSHA WITHOUT
// opening a PR. The operator decided exempt, so this opens the PR from that
// exact held commit and ships the pull_request artifact — no agent, no gates, no
// CommitAndPush (the commit and its tree are unchanged from the park). ADR-035
// sole-writer holds: the same run that wrote the branch opens its PR, and the
// opened-PR head is byte-identical to the held tree.
//
// Returns a process exit code (exitOK / exitFailure). A push/PR/report failure
// reports a "failed" outcome to /pull-request so the dispatched stage the trace
// gate left in `running` lands `failed` rather than hanging — symmetric with
// openPRAndShipArtifact's failure path, minus the (absent) commit.
func openHeldCommitPR(ctx context.Context, cfg config, heldSHA, heldBranch string, logSink io.Writer, client uploadClient, issued *upload.IssuedKey) int {
	fail := func(category, reason string) int {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"runner_failed","reason":"scope_exempt_open_pr","detail":%q}`+"\n", reason)
		reportPullRequestFailure(ctx, cfg, logSink, client, issued, category, reason)
		return exitFailure
	}
	if cfg.runID == "" || cfg.stageID == "" {
		return fail("C", "exempt open-PR requires --run-id and --stage-id")
	}
	if issued == nil {
		return fail("C", "exempt open-PR: signing key not issued")
	}
	if heldBranch == "" || heldSHA == "" {
		return fail("C", "exempt open-PR: backend did not advertise held_commit_branch/held_commit_sha")
	}
	if client == nil {
		client = newUploadClient(cfg.backendURL)
	}

	repoSlug := cfg.githubRepo
	if repoSlug == "" {
		repoSlug = os.Getenv("GITHUB_REPOSITORY")
	}
	owner, repoName, ok := strings.Cut(repoSlug, "/")
	if !ok || owner == "" || repoName == "" {
		return fail("C", fmt.Sprintf("exempt open-PR: github repo %q is not owner/name", repoSlug))
	}
	baseRef := resolveImplementBaseRef(cfg)
	branch := heldBranch

	token, err := mintImplementToken(ctx, cfg, client, issued, logSink)
	if err != nil {
		return fail("C", err.Error())
	}

	title, body := prTitleAndBody(cfg, branch, logSink)
	prRes, err := newPROpener(token).OpenPR(ctx, gitops.OpenPRArgs{
		Owner: owner,
		Repo:  repoName,
		Head:  branch,
		Base:  baseRef,
		Title: title,
		Body:  body,
	})
	if err != nil {
		return fail("C", fmt.Sprintf("open PR from held commit: %v", err))
	}
	// The opened PR head is the held commit (#1231): the branch tip is heldSHA,
	// unchanged since the park pushed it (ADR-035 sole-writer — no other writer
	// touches the run branch). Stamp it so the audit chain proves
	// opened-PR-head == held-commit without re-resolving the remote tip.
	_, _ = fmt.Fprintf(logSink,
		`{"event":"scope_completeness_pr_opened","run_id":%q,"stage_id":%q,"pr_number":%d,"pr_url":%q,"head_sha":%q,"branch":%q}`+"\n",
		cfg.runID, cfg.stageID, prRes.PRNumber, prRes.PRURL, heldSHA, branch)

	artifactBody, _ := json.Marshal(map[string]any{
		"pr_number": prRes.PRNumber,
		"pr_url":    prRes.PRURL,
		"branch":    branch,
		"head_sha":  heldSHA,
		"title":     title,
		"body":      body,
	})
	shipRes, err := client.ShipPullRequest(ctx, upload.ShipPullRequestArgs{
		RunID:      cfg.runID,
		StageID:    cfg.stageID,
		Body:       artifactBody,
		PrivateKey: issued.PrivateKey,
	})
	if err != nil {
		category := "C"
		if errors.Is(err, upload.ErrPullRequestInvalid) {
			category = "B"
		}
		return fail(category, fmt.Sprintf("ship pull-request from held commit: %v", err))
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"pull_request_uploaded","run_id":%q,"stage_id":%q,"artifact_id":%q,"content_hash":%q,"idempotent":%t}`+"\n",
		cfg.runID, cfg.stageID, shipRes.ID, shipRes.ContentHash, shipRes.Idempotent)
	return exitOK
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
//
// verifiedTreeSHA, when non-empty, is the tree object hash the committed-tree
// verify gates (#651/#802) passed against; the pre-push VerifyCommit hook
// enforces the verified-SHA invariant (#960) against it — see the closure
// below. Empty disables the check (no committed-tree gate ran: plan stage,
// --no-pr, verifyCmd unset, or gate skipped).
// supplementalExemptions, when non-empty, is the base-rebase re-invoke exemption
// delta (#1218): exemptions the final scope-completeness gate honored that the
// already-sealed trace bundle's scope_files_exempted event did not carry (the
// bundle ships before the re-invoke under #742 forward gating). It rides inside
// the success artifact body so the backend re-emits a supplemental
// scope_files_exempted audit row — the visibility surface for the re-invoke
// branch. nil on the first (pre-re-invoke) ship and every non-re-invoke ship, so
// the artifact body stays byte-identical there.
func openPRAndShipArtifact(ctx context.Context, cfg config, logSink io.Writer, client uploadClient, issued *upload.IssuedKey, preAgentRef string, preAgentDetached bool, preAgentCaptured bool, preAgentDirty []string, preAgentDirtyCaptured bool, verifiedTreeSHA string, applyPath string, bindingAssertions []upload.BindingAssertion, scopeExemptions []scopeExemption, supplementalExemptions []scopeExemption) error {
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
	token, err := mintImplementToken(ctx, cfg, client, issued, logSink)
	if err != nil {
		return err
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
	baseRef := resolveImplementBaseRef(cfg)

	repoDir := cfg.workingDir
	if repoDir == "" {
		repoDir = "."
	}

	// Working-tree restoration (#911, #941): CommitAndPush below switches HEAD
	// onto the run branch (checkout -b/-B), and on a CommitAndPush-side failure
	// (e.g. the #800 committed-test verify gate flaking) the tree is also left
	// dirty. Either way the operator's checkout is stranded on the run branch,
	// which silently breaks the next `scripts/dev post-merge` (a dirty tree
	// refuses `git checkout main`; the run-branch HEAD is not an ancestor of
	// the squash-merge commit so `git merge --ff-only` fails). The restore
	// target (preAgentRef) was captured in run() BEFORE the agent was invoked
	// (#941) — not re-read here, where an agent that ran `git checkout -b`
	// mid-stage could have moved HEAD onto its own branch and made that the
	// restore target. This block sits AFTER the cfg.noPR early return above, so
	// --no-pr keeps its deliberate leave-the-tree-dirty-for-the-operator
	// semantics. The defer fires at function return — AFTER the inline gitdiff
	// filesChanged reads and ShipPullRequest reports that need the run-branch
	// tip — so those reads are unaffected. Restore is BEST-EFFORT and LOG-ONLY:
	// it never overrides the function's primary push success/failure outcome.
	// preAgentCaptured is false when the capture in run() failed (or was
	// skipped); restoration is then skipped, never breaking the push path.
	// This defer restores BEFORE run()'s #953 stage-wide net (LIFO: run()'s
	// defer was installed first, so it fires last), whose moved-HEAD guard
	// then sees HEAD already back on the captured ref and no-ops — the two
	// never double-checkout.
	//
	// preservedDrift names the operator's pre-existing edits the post-push
	// drift partition below identifies (#943); the defer closes over it so
	// restoreHeadPreserving carries them across the forced checkout (stash /
	// checkout / pop) instead of discarding them. It stays nil on every
	// failure path and whenever the pre-agent dirty snapshot failed, where
	// restoreHeadPreserving delegates to the plain RestoreHead semantics.
	var preservedDrift []string
	if preAgentCaptured {
		defer func() {
			if rerr := restoreHeadPreserving(ctx, repoDir, preAgentRef, preservedDrift); rerr != nil {
				_, _ = fmt.Fprintf(logSink,
					`{"event":"working_tree_restore_failed","run_id":%q,"stage_id":%q,"original_ref":%q,"detached":%t,"detail":%q}`+"\n",
					cfg.runID, cfg.stageID, preAgentRef, preAgentDetached, rerr.Error())
				return
			}
			_, _ = fmt.Fprintf(logSink,
				`{"event":"working_tree_restored","run_id":%q,"stage_id":%q,"original_ref":%q,"detached":%t}`+"\n",
				cfg.runID, cfg.stageID, preAgentRef, preAgentDetached)
		}()
	}

	// Branch routing: a fix-up pass commits onto the existing PR branch;
	// decomposed children share a single parent branch; standalone runs
	// get a per-stage branch.
	routing, err := resolveImplementBranchRouting(ctx, cfg, repoDir, baseRef)
	if err != nil {
		return err
	}
	branch := routing.branch
	isDecomposed := routing.isDecomposed
	isSubsequent := routing.isSubsequent
	isFixup := routing.isFixup
	freshFetchBase := routing.freshFetchBase

	title, body := prTitleAndBody(cfg, branch, logSink)
	// Initial (non-fix-up) implement commit message (#1686): prefer the agent's
	// clean Conventional-Commits sidecar (consumed + deleted by
	// loadImplementCommitMessage), falling back to today's title + "\n\n" + body
	// when no sidecar is present — so the initial commit no longer stuffs the
	// whole PR review body into its message. The PR title/body still come from
	// the (run/stage-keyed, #1777) PR-description handoff unchanged. Overridden
	// below on the isFixup path.
	commitMessage := implementCommitMessage(cfg, title, body, logSink)
	if isFixup {
		// Per-pass fix-up commit message (#1572): a fix-up gets its own commit
		// subject/body from the run/stage-keyed sidecar the fix-up agent wrote
		// — NOT the PR title/body (the PR already exists and must not be
		// clobbered). Falls back to a conventional-shaped, per-pass-unique
		// subject keyed by the pass's base tip when the agent wrote no sidecar.
		// HEAD here is the fix-up branch tip (checkoutFixupBase moved onto it and
		// the agent commits nothing), so it is the base tip; a rev-parse failure
		// degrades to an empty base component rather than blocking the commit.
		baseTipSHA, rpErr := gitRevParseHEAD(ctx, repoDir)
		if rpErr != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"fixup_base_tip_unresolved","run_id":%q,"stage_id":%q,"detail":%q}`+"\n",
				cfg.runID, cfg.stageID, rpErr.Error())
		}
		commitMessage = fixupCommitMessage(cfg, baseTipSHA, logSink)
	}

	// Compile-gate the scope-only committed tree before push (#728), on every
	// implement push including decomposed children (#766). A scope-bounded
	// child commit is the highest-risk path for a drift-dropped non-compiling
	// HEAD: StageScoped (#581) can strip build-required drift, so an
	// incomplete child commit is a scope-drift defect to surface, not a
	// tolerated intermediate. The gate's isolated worktree checks out the
	// specific headSHA, so it works unchanged for a shared-branch child
	// commit. The hook runs inside CommitAndPush after the commit and before
	// the push, so a failure leaves origin untouched.
	gateScopeFiles := scopePaths(cfg.scopeFiles)
	verifyCommit := func(ctx context.Context, headSHA string, drift []string) error {
		// Scope-completeness park deferral (#1231). The missing-declared-scope-file
		// gate is checked at its usual position below but RECORDS rather than
		// returns, so the remaining gates (binding-assertion, compile/test,
		// verified-tree) all run afterward. The park is signaled — via the typed
		// *gitops.ScopeFilesMissingError that CommitAndPush recognizes — ONLY at a
		// terminal pass point, i.e. when every OTHER gate is green: that proves
		// missing-scope is the SOLE failure (any compound failure returns the other
		// gate's error first, keeping today's category-B). created-out-of-scope
		// runs BEFORE the recording point and returns on failure, so it too is
		// covered by the sole-failure guarantee.
		var parkMissing []string
		var parkMessage string
		scopeParkResult := func() error {
			if len(parkMissing) > 0 {
				return &gitops.ScopeFilesMissingError{Missing: parkMissing, Message: parkMessage}
			}
			return nil
		}
		// Created-out-of-scope gate (#818, generalized to the open-PR path by
		// #825). A net-new (untracked) out-of-scope file is silently stripped
		// from the scope-only commit by StageScoped (#581) while in-scope edits
		// referencing it land — a misleadingly-green partial result. Fail
		// category-B BEFORE the push (origin untouched). Checked before the
		// compile gate as the more specific signal. Modified-but-out-of-scope
		// drift stays flag-only (ADR-027) — only CREATED files trip this, so we
		// filter drift to the untracked subset.
		//
		// The gate runs on the fix-up pass AND the normal open-PR implement push,
		// but NOT on decomposed children: a child may legitimately create files a
		// later child declares, so the shared-branch path tolerates net-new
		// out-of-scope files. (cfg.noPR is already excluded by the early return
		// upstream.) The fix-up and open-PR paths emit distinct events and wrap
		// distinct sentinels with path-specific recovery guidance.
		if cfg.fixup || (!isFixup && !isDecomposed) {
			created, uerr := gitops.UntrackedPaths(ctx, repoDir, drift)
			if uerr != nil {
				return uerr
			}
			if len(created) > 0 {
				createdJSON, _ := json.Marshal(created)
				if cfg.fixup {
					_, _ = fmt.Fprintf(logSink,
						`{"event":"fixup_created_out_of_scope","run_id":%q,"stage_id":%q,"head_sha":%q,"created":%s}`+"\n",
						cfg.runID, cfg.stageID, headSHA, createdJSON)
					return fmt.Errorf("%w: the fix-up created %d file(s) outside the stage's fixed scope.files: %s. "+
						"A fix-up cannot widen scope.files, so these net-new files were rejected rather than silently "+
						"stripped (which would ship a misleadingly-green partial result). The run has been restored to "+
						"its pre-fix-up review gate — recover with fishhawk_resume_run on the parent run (add_scope_files "+
						"naming the created file(s) with operation 'create'), hand-apply the named file(s), or start a "+
						"fresh run with a corrected scope that declares them",
						gitops.ErrFixupCreatedOutOfScope, len(created), strings.Join(created, ", "))
				}
				_, _ = fmt.Fprintf(logSink,
					`{"event":"created_out_of_scope","run_id":%q,"stage_id":%q,"head_sha":%q,"created":%s}`+"\n",
					cfg.runID, cfg.stageID, headSHA, createdJSON)
				return fmt.Errorf("%w: the implement stage created %d file(s) outside the approved scope.files: %s. "+
					"These net-new files were rejected rather than silently stripped (which would ship a "+
					"misleadingly-green partial PR). Recover with fishhawk_resume_run (parent_run_id = this run, "+
					"add_scope_files naming the created file(s) with operation 'create'); at the plan-approval gate "+
					"you can instead name the files in the approval condition; otherwise replan with a corrected "+
					"scope that declares them",
					gitops.ErrCreatedOutOfScope, len(created), strings.Join(created, ", "))
			}
		}
		// Pre-push scope-completeness (shortfall) gate (#1151): the inverse of
		// the created-out-of-scope (#818/#825) and #980 commit-in-scope
		// assertions. Assert the commit TOUCHED every concrete declared
		// scope.files path; a shortfall means the agent dropped a declared edit
		// (the #1148 subset PR — 8 declared, 6 committed). Runs on the
		// standalone open-PR push ONLY: NOT fix-ups (which legitimately touch
		// fewer files than the full scope) and NOT decomposed children (narrowed
		// slice scope on a shared branch). Fail category-B BEFORE the push
		// (origin untouched). The gate honors the agent's validated in-band
		// self-exemptions (#1153): a deliberately-unchanged declared file the
		// agent justified is subtracted from `missing` before the len>0 check, so
		// an all-exempted shortfall passes the gate and a partial one fails
		// listing only the unexempted remainder (noting which were self-exempted).
		// The exemptions were validated fail-closed and recorded once via the
		// scope_files_exempted event in run(); this gate runs after the trace
		// ship, so it re-emits nothing.
		if !isFixup && !isDecomposed && len(gateScopeFiles) > 0 {
			missing, committed, merr := gitops.MissingScopeFiles(ctx, repoDir, headSHA, gateScopeFiles)
			if merr != nil {
				return merr
			}
			exempted, remaining := partitionExemptedMissing(missing, scopeExemptions)
			if len(remaining) > 0 {
				remainingJSON, _ := json.Marshal(remaining)
				declaredJSON, _ := json.Marshal(gateScopeFiles)
				committedJSON, _ := json.Marshal(committed)
				exemptedJSON, _ := json.Marshal(exempted)
				_, _ = fmt.Fprintf(logSink,
					`{"event":"scope_files_missing","run_id":%q,"stage_id":%q,"head_sha":%q,"declared":%s,"committed":%s,"missing":%s,"exempted":%s}`+"\n",
					cfg.runID, cfg.stageID, headSHA, declaredJSON, committedJSON, remainingJSON, exemptedJSON)
				// Record the shortfall but DON'T return (#1231): the remaining gates
				// must run first so the park is signaled only when missing-scope is the
				// SOLE failure. The terminal scopeParkResult() converts this into the
				// typed *gitops.ScopeFilesMissingError. Message preserves the pre-#1231
				// "%w: declared N, committed M" narrative byte-for-byte.
				parkMissing = remaining
				parkMessage = fmt.Sprintf("%s: %s", gitops.ErrScopeFilesMissing.Error(),
					missingScopeFilesMessage(cfg.scopeFiles, remaining, exempted, len(gateScopeFiles), len(committed)))
			}
		}
		// Binding-assertion gate (#1171): the operator-declared deterministic
		// substring checks (fishhawk_approve_plan binding_assertions) are
		// evaluated against the committed scope-only tree at headSHA. Any
		// unsatisfied assertion fails category-B BEFORE the push (origin
		// untouched) — the artifact does not meet a declared binding condition,
		// so park for re-scope/re-plan, the same disposition as the
		// scope-completeness gate above. Guarded `!isFixup && !isDecomposed`
		// exactly like MissingScopeFiles: a fix-up legitimately touches fewer
		// files, and a decomposed child's narrowed slice may target a sibling's
		// file, so neither can false-trip. A no-op when none were declared.
		if !isFixup && !isDecomposed && len(bindingAssertions) > 0 {
			results, berr := gitops.EvaluateBindingAssertions(ctx, repoDir, headSHA, toGitopsBindingAssertions(bindingAssertions))
			if berr != nil {
				return berr
			}
			if unsatisfied := gitops.UnsatisfiedBindingAssertions(results); len(unsatisfied) > 0 {
				unsatisfiedJSON, _ := json.Marshal(unsatisfied)
				_, _ = fmt.Fprintf(logSink,
					`{"event":"binding_assertion_unsatisfied","run_id":%q,"stage_id":%q,"head_sha":%q,"unsatisfied":%s}`+"\n",
					cfg.runID, cfg.stageID, headSHA, unsatisfiedJSON)
				return fmt.Errorf("%w: %d of %d declared binding assertion(s) not satisfied by the committed tree: %s. "+
					"The operator attached these deterministic checks at plan approval; the committed scope-only tree "+
					"did not contain the declared literal(s). Recover by making the implement output satisfy the "+
					"declared condition(s), or replan/re-approve with corrected binding_assertions",
					gitops.ErrBindingAssertionUnsatisfied, len(unsatisfied), len(results), gitops.FormatUnsatisfied(unsatisfied))
			}
		}
		if err := verifyCommittedTreeCompiles(ctx, repoDir, headSHA, drift, gateScopeFiles, logSink); err != nil {
			driftJSON, _ := json.Marshal(drift)
			// The test phase (#800) shares the gate; emit a test_gate_failed
			// event for a committed-test failure and compile_gate_failed for a
			// compile failure so the two phases are distinguishable in the trace.
			event := "compile_gate_failed"
			if errors.Is(err, gitops.ErrCommittedTestsFailed) {
				event = "test_gate_failed"
			}
			_, _ = fmt.Fprintf(logSink,
				`{"event":%q,"run_id":%q,"stage_id":%q,"head_sha":%q,"drift":%s}`+"\n",
				event, cfg.runID, cfg.stageID, headSHA, driftJSON)
			return err
		}
		// Verified-SHA invariant (#960), the cheap backstop run LAST so the
		// more specific, more actionable sentinels above report first. The
		// committed-tree gates verified a throwaway scope-only commit; THIS
		// commit was built independently (re-staged scope, possibly a
		// freshly-fetched moved base via FreshFetchBase), so prove the trees
		// match before anything reaches origin. Equal tree hashes mean a
		// byte-identical snapshot — the gates' verdict transfers for free. A
		// mismatch gets exactly ONE strict re-verify against the real
		// committed HEAD; only an explicit "passed" lets the push proceed
		// (an infra-skip is NOT a pass — #959-complementary), anything else
		// returns ErrPushedTreeNotVerified BEFORE the push (origin untouched,
		// category-B). Empty verifiedTreeSHA = no gate ran = no-op.
		if verifiedTreeSHA == "" {
			return scopeParkResult()
		}
		realTree, terr := gitRevParseTreeOf(ctx, repoDir, headSHA)
		if terr != nil {
			// A gate passed, so an unresolvable pushed tree is fail-closed:
			// never push a tree whose equivalence to the verified one is
			// unprovable (#960 approval condition).
			return fmt.Errorf("%w: verified tree %s, but the staged commit %s tree could not be resolved: %v",
				gitops.ErrPushedTreeNotVerified, verifiedTreeSHA, headSHA, terr)
		}
		if realTree == verifiedTreeSHA {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"verified_tree_match","run_id":%q,"stage_id":%q,"head_sha":%q,"tree_sha":%q}`+"\n",
				cfg.runID, cfg.stageID, headSHA, realTree)
			return scopeParkResult()
		}
		driftJSON, _ := json.Marshal(drift)
		_, _ = fmt.Fprintf(logSink,
			`{"event":"verified_tree_mismatch","run_id":%q,"stage_id":%q,"head_sha":%q,"verified_tree_sha":%q,"pushed_tree_sha":%q,"drift":%s}`+"\n",
			cfg.runID, cfg.stageID, headSHA, verifiedTreeSHA, realTree, driftJSON)
		reverifyTimeout := cfg.verifyTimeout
		if reverifyTimeout == 0 {
			reverifyTimeout = 10 * time.Minute
		}
		ev, out, outcome := runVerifyCommittedTree(ctx, cfg.verifyCmd, repoDir, headSHA, reverifyTimeout)
		// Emit the decisive re-verify's verify_run record unconditionally
		// (pass or fail) before the outcome check (#969). The gate's first
		// verify_run shipped inside the trace bundle, but the bundle is
		// sealed and uploaded before this pre-push hook runs (#742 forward
		// gating), so logSink — the channel carrying the rest of the
		// invariant chain — is the record's home. `output` is omitted: the
		// failure path embeds the full verify output in the returned
		// ErrPushedTreeNotVerified error, and an unbounded blob would bloat
		// a single JSONL line.
		var evp struct {
			Command  string `json:"command"`
			HeadSHA  string `json:"head_sha"`
			TreeSHA  string `json:"tree_sha"`
			ExitCode int    `json:"exit_code"`
			Outcome  string `json:"outcome"`
		}
		_ = json.Unmarshal(ev.Payload, &evp)
		_, _ = fmt.Fprintf(logSink,
			`{"event":"verify_run","run_id":%q,"stage_id":%q,"command":%q,"head_sha":%q,"tree_sha":%q,"exit_code":%d,"outcome":%q}`+"\n",
			cfg.runID, cfg.stageID, evp.Command, evp.HeadSHA, evp.TreeSHA, evp.ExitCode, evp.Outcome)
		if outcome != "passed" {
			return fmt.Errorf("%w: gates verified tree %s but the staged commit %s carries tree %s, and the strict re-verify of %q did not pass (outcome %s); %d file(s) excluded as scope drift: %s\n%s",
				gitops.ErrPushedTreeNotVerified, verifiedTreeSHA, headSHA, realTree,
				cfg.verifyCmd, outcome, len(drift), strings.Join(drift, ", "), out)
		}
		// The pushed SHA is now itself gate-verified. Stamp the forensic
		// record with BOTH trees — verified_tree_sha is the ORIGINAL
		// throwaway gate tree, tree_sha the re-verified pushed tree,
		// mirroring verified_tree_mismatch's field naming — BEFORE the
		// rebind below, so the original gate tree's provenance survives
		// (#969).
		_, _ = fmt.Fprintf(logSink,
			`{"event":"pushed_tree_reverified","run_id":%q,"stage_id":%q,"head_sha":%q,"verified_tree_sha":%q,"tree_sha":%q}`+"\n",
			cfg.runID, cfg.stageID, headSHA, verifiedTreeSHA, realTree)
		// Rebind the effective verified tree to the re-verified pushed tree.
		// The closure shares verifiedTreeSHA with openPRAndShipArtifact, and
		// this hook runs before CommitAndPush returns, so the stamp sites
		// downstream (implement_fixup_pushed, implement_child_pushed,
		// pull_request_opened) record verified_tree_sha == tree_sha
		// unconditionally — the audit trail's equality claim holds on the
		// reverify-pass path too (#969).
		verifiedTreeSHA = realTree
		return scopeParkResult()
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
		PushToken: token,
		// ADR-041 (#1141): a decomposed child now owns a sole-writer slice
		// branch cut fresh from base, so no path force-pushes a shared branch.
		// ForceWithLease is false for every routing case (the fix-up path
		// updates its PR branch via RebaseFromRemote; standalone and slice
		// both cut a fresh sole-writer branch). The shared-branch
		// force-with-lease coupling that #767 needed is dropped.
		ForceWithLease:   false,
		RebaseFromRemote: isSubsequent,
		// Cut a standalone run branch from a freshly-fetched authoritative
		// base (origin/<baseRef>) rather than the ambient local HEAD, so a
		// foreign commit another writer made in the same shared checkout (the
		// #797 shape) cannot become the run branch base (ADR-035 prevention,
		// #861). Set ONLY for the standalone `default:` routing case — the
		// fix-up and decomposed-child paths keep their existing branch
		// machinery (RebaseFromRemote / checkout -b from a controlled base).
		// Empty for those callers keeps the unchanged checkout -b path.
		FreshFetchBase: freshFetchBase,
		// ADR-041 (#1141): a sole-writer slice branch is pushed once by one
		// child and never re-read by a sibling's routing or lease, so there is
		// nothing to keep in sync — the pre-ADR-041 shared-branch tracking-ref
		// materialization (#770/#767) is no longer needed and is dropped for
		// every routing case.
		UpdateTrackingRef: false,
		// Scope-bounded commit (#581): stage exactly the approved
		// plan's declared paths, excluding stray dirty files. Empty
		// (plan_missing_for_implement) falls back to `git add -A`.
		ScopeFiles: scopePaths(cfg.scopeFiles),
		// Scope-completeness park (#1231): on the standalone open-PR push ONLY
		// (the same guard the missing-scope gate runs under), a
		// missing-declared-scope-file-ONLY failure pushes the gate-verified
		// commit and surfaces ScopeShortfall instead of aborting category-B, so
		// the caller can report a park. False for fix-ups and decomposed children
		// keeps their strict category-B behavior byte-identical.
		ParkOnScopeShortfall: !isFixup && !isDecomposed,
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
		// Defensive build-artifact WARN (#980): a drift path that looks like
		// a compiled binary (the agent's accidental `go build` output) gets
		// its own advisory event so the operator can spot it before staging
		// drift by hand. Log-only — the post-commit ErrCommitOutOfScope
		// assertion and the #818 created-out-of-scope gate are the
		// enforcement layers.
		for _, dp := range cap.ScopeDrift {
			if hit, size := isBinaryArtifactDrift(repoDir, dp); hit {
				_, _ = fmt.Fprintf(logSink,
					`{"event":"scope_drift_binary_artifact","run_id":%q,"stage_id":%q,"path":%q,"size_bytes":%d}`+"\n",
					cfg.runID, cfg.stageID, dp, size)
			}
		}
	}
	// Drift cleanup (#943): partition the scope drift against the pre-agent
	// dirty snapshot. Paths NOT dirty before the agent ran are
	// agent-introduced — revert them (tracked and untracked alike) so they
	// don't accumulate across loop runs. Paths dirty pre-agent are
	// operator-owned — preserve them across the restore defer above. Runs on
	// every CommitAndPush success including the NoChanges-with-drift return
	// (which never creates the branch but still reports ScopeDrift). All
	// best-effort and log-only, matching the restore discipline: a cleanup
	// failure never overrides the push's primary outcome. Skipped entirely
	// when the pre-agent snapshot failed (preAgentDirtyCaptured false) —
	// with no trustworthy baseline, never revert blind. A pre-agent-dirty
	// path that also carries agent edits is preserved whole (never destroy
	// operator work) — separating intra-file edits is out of scope.
	if preAgentDirtyCaptured && len(cap.ScopeDrift) > 0 {
		preDirty := make(map[string]bool, len(preAgentDirty))
		for _, dp := range preAgentDirty {
			preDirty[dp] = true
		}
		var agentDrift []string
		for _, dp := range cap.ScopeDrift {
			if preDirty[dp] {
				preservedDrift = append(preservedDrift, dp)
			} else {
				agentDrift = append(agentDrift, dp)
			}
		}
		if len(agentDrift) > 0 {
			agentDriftJSON, _ := json.Marshal(agentDrift)
			if cerr := cleanDriftPaths(ctx, repoDir, agentDrift); cerr != nil {
				_, _ = fmt.Fprintf(logSink,
					`{"event":"drift_clean_failed","run_id":%q,"stage_id":%q,"paths":%s,"detail":%q}`+"\n",
					cfg.runID, cfg.stageID, agentDriftJSON, cerr.Error())
			} else {
				_, _ = fmt.Fprintf(logSink,
					`{"event":"drift_cleaned","run_id":%q,"stage_id":%q,"paths":%s}`+"\n",
					cfg.runID, cfg.stageID, agentDriftJSON)
			}
		}
		if len(preservedDrift) > 0 {
			preservedJSON, _ := json.Marshal(preservedDrift)
			_, _ = fmt.Fprintf(logSink,
				`{"event":"drift_preserved","run_id":%q,"stage_id":%q,"paths":%s}`+"\n",
				cfg.runID, cfg.stageID, preservedJSON)
		}
	}
	if cap.NoChanges {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"implement_no_changes","run_id":%q,"stage_id":%q,"base_sha":%q}`+"\n",
			cfg.runID, cfg.stageID, cap.BaseSHA,
		)
		// Fix-up no-changes (#856): a fix-up re-dispatch (push_fixup gate) that
		// produces no changes must NOT `return nil` — the trace handler left the
		// fix-up stage in `running`, so without an authoritative report the stage
		// hangs in running until the SLA watchdog reaps it, stranding the review
		// stage in pending. Report a dedicated "fixup_no_changes" outcome so the
		// backend drives the fix-up stage terminal and re-parks the review gate,
		// mirroring the fixup_pushed path minus the (absent) new commit. No
		// HeadSHA — no commit landed; branch + base_sha pin the unchanged tip. A
		// report error is category-C (network) and is surfaced so the failure
		// path reports it.
		if isFixup {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"implement_fixup_no_changes","run_id":%q,"stage_id":%q,"branch":%q,"base_sha":%q}`+"\n",
				cfg.runID, cfg.stageID, branch, cap.BaseSHA,
			)
			if _, err := client.ShipPullRequest(ctx, upload.ShipPullRequestArgs{
				RunID:             cfg.runID,
				StageID:           cfg.stageID,
				PrivateKey:        issued.PrivateKey,
				Outcome:           "fixup_no_changes",
				Branch:            branch,
				BaseSHA:           cap.BaseSHA,
				FilesChangedCount: 0,
			}); err != nil {
				return fmt.Errorf("report fix-up no-changes: %w", err)
			}
			_, _ = fmt.Fprintf(logSink,
				`{"event":"implement_fixup_no_changes_reported","run_id":%q,"stage_id":%q,"branch":%q,"base_sha":%q}`+"\n",
				cfg.runID, cfg.stageID, branch, cap.BaseSHA,
			)
			return nil
		}
		// Decomposed-child no-changes (#1036): the push_to_shared_branch trace
		// gate (#771) left this child stage in `running`, deferring its
		// terminal transition to a child-push report a bare return would never
		// send — the stage hangs until the SLA watchdog reaps it. Report the
		// existing `failed` outcome (category C, retryable) so the backend
		// terminalizes the stage, mirroring the standalone no_diff_captured
		// semantics (#691/#692) with no new backend surface. With the
		// pre-invoke shared-branch checkout in place a genuine no-changes
		// child is overwhelmingly a planning/decomposition error; category C
		// keeps it operator-retryable rather than dooming the fan-out parent
		// on an un-redrivable B.
		if isDecomposed {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"implement_child_no_changes","run_id":%q,"stage_id":%q,"shared_branch":%q,"base_sha":%q,"slice_index":%d}`+"\n",
				cfg.runID, cfg.stageID, branch, cap.BaseSHA, runSliceIndex,
			)
			reportPullRequestFailure(ctx, cfg, logSink, client, issued, "C",
				childNoChangesReason(runSliceIndex))
			return nil
		}
		// Non-fix-up no-changes (first implement pass, no forward gate): no PR to
		// open; no artifact to ship. The stage still counts as succeeded — the
		// agent decided no edits were needed. Reviewer will see the empty trace +
		// this log. That path's stage was never left in running by a forward
		// gate, so `return nil` remains correct.
		return nil
	}

	// Fix-up pass (#762): the commit is now on the existing PR branch and
	// pushed, so the open PR's head has advanced. Don't open a new PR (one
	// already exists for this head→base) and don't ship a pull_request
	// artifact. Instead REPORT push success to the backend (#794): the trace
	// handler left this fix-up stage in `running` (push_fixup gate), so THIS
	// report is the authoritative driver of the stage's terminal transition —
	// without it the stage would hang in running until the SLA watchdog reaps
	// it, and a commit/push/compile-gate failure (reported via
	// reportPullRequestFailure above → #788 recovery) could never be
	// distinguished from a still-pending push. The "fixup_pushed" outcome
	// carries only branch/head_sha/base_sha + the diff size; the backend
	// writes a fixup_pushed audit entry (no PR artifact) and drives the
	// running → terminal transition the advisory implement re-review keys off.
	// A report error is category-C (network) and is surfaced so the failure
	// path reports it.
	if isFixup {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"implement_fixup_pushed","run_id":%q,"stage_id":%q,"branch":%q,"head_sha":%q,"verified_tree_sha":%q,"tree_sha":%q}`+"\n",
			cfg.runID, cfg.stageID, branch, cap.HeadSHA, verifiedTreeSHA, cap.TreeSHA,
		)
		filesChanged := 0
		if d, err := (&gitdiff.Runner{}).Run(ctx, baseRef, repoDir); err == nil {
			filesChanged = len(d.ChangedFiles)
		} else {
			// Informational only: files_changed_count is not load-bearing for
			// the stage transition. Log rather than silently discard.
			_, _ = fmt.Fprintf(logSink,
				`{"event":"fixup_push_files_changed_unavailable","run_id":%q,"stage_id":%q,"detail":%q}`+"\n",
				cfg.runID, cfg.stageID, err.Error())
		}
		if _, err := client.ShipPullRequest(ctx, upload.ShipPullRequestArgs{
			RunID:             cfg.runID,
			StageID:           cfg.stageID,
			PrivateKey:        issued.PrivateKey,
			Outcome:           "fixup_pushed",
			Branch:            branch,
			HeadSHA:           cap.HeadSHA,
			BaseSHA:           cap.BaseSHA,
			FilesChangedCount: filesChanged,
			ApplyPath:         applyPath,
		}); err != nil {
			return fmt.Errorf("report fix-up push: %w", err)
		}
		_, _ = fmt.Fprintf(logSink,
			`{"event":"implement_fixup_push_reported","run_id":%q,"stage_id":%q,"branch":%q,"head_sha":%q}`+"\n",
			cfg.runID, cfg.stageID, branch, cap.HeadSHA,
		)
		return nil
	}

	// Decomposed children only push their commit onto their own sole-writer
	// slice branch fishhawk/run-<parent>/slice-<n> (E24.1 / #1141 / ADR-041;
	// one branch per sub-plan); they never open a PR or ship a pull_request
	// artifact. Per ADR-032 (#719) the parent run opens ONE consolidated PR
	// for the whole decomposition after all children settle — so suppress
	// OpenPR + ShipPullRequest for every decomposed child. The
	// implement_child_pushed event's is_subsequent field is now always false
	// for children (each slice is cut fresh from base; fan-in is E24.2), and
	// its shared_branch field carries the per-child slice branch.
	if isDecomposed {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"implement_child_pushed","run_id":%q,"stage_id":%q,"shared_branch":%q,"head_sha":%q,"is_subsequent":%t,"verified_tree_sha":%q,"tree_sha":%q}`+"\n",
			cfg.runID, cfg.stageID, branch, cap.HeadSHA, isSubsequent, verifiedTreeSHA, cap.TreeSHA,
		)

		// Report child-push SUCCESS to the backend (#771). The backend's
		// trace handler left this child stage in `running` (push_to_shared_branch
		// gate), so THIS report is the authoritative driver of the child
		// stage's terminal transition — without it the stage would hang in
		// running until the SLA watchdog reaps it, and a commit/push failure
		// (reported via reportPullRequestFailure above) could never be
		// distinguished from a still-pending push. No PR was opened, so the
		// "pushed" outcome carries only branch/head_sha/base_sha + the diff
		// size; the backend writes a child_pushed audit entry (no PR
		// artifact). A report error is category-C (network) and is surfaced
		// like the success ShipPullRequest error below so the failure path
		// reports it.
		filesChanged := 0
		if d, err := (&gitdiff.Runner{}).Run(ctx, baseRef, repoDir); err == nil {
			filesChanged = len(d.ChangedFiles)
		} else {
			// Informational only: files_changed_count is not load-bearing for
			// the stage transition. Log rather than silently discard so a diff
			// failure here is observable.
			_, _ = fmt.Fprintf(logSink,
				`{"event":"child_push_files_changed_unavailable","run_id":%q,"stage_id":%q,"detail":%q}`+"\n",
				cfg.runID, cfg.stageID, err.Error())
		}
		if _, err := client.ShipPullRequest(ctx, upload.ShipPullRequestArgs{
			RunID:             cfg.runID,
			StageID:           cfg.stageID,
			PrivateKey:        issued.PrivateKey,
			Outcome:           "pushed",
			Branch:            branch,
			HeadSHA:           cap.HeadSHA,
			BaseSHA:           cap.BaseSHA,
			FilesChangedCount: filesChanged,
		}); err != nil {
			return fmt.Errorf("report child push: %w", err)
		}
		_, _ = fmt.Fprintf(logSink,
			`{"event":"implement_child_push_reported","run_id":%q,"stage_id":%q,"shared_branch":%q,"head_sha":%q}`+"\n",
			cfg.runID, cfg.stageID, branch, cap.HeadSHA,
		)
		return nil
	}

	// Scope-completeness park (#1231): the missing-declared-scope-file gate was
	// this standalone implement stage's SOLE committed-tree failure, so
	// CommitAndPush pushed the gate-verified commit to the run branch but
	// surfaced ScopeShortfall instead of failing category-B. Report the park to
	// the backend INSTEAD of opening a PR — the backend records the held-commit
	// payload, transitions the implement stage to awaiting_scope_decision (a
	// parked judgment, not a category-B failure), and surfaces an in-band
	// operator exempt/fail decision. The held commit (cap.HeadSHA on `branch`)
	// survives the runner exit so an exempt resolution opens the PR from this
	// exact head with no agent re-run (ADR-035 sole-writer). A report error is
	// category-C (network) and is surfaced like the OpenPR/ship errors so the
	// failure path reports it.
	if len(cap.ScopeShortfall) > 0 {
		missingJSON, _ := json.Marshal(cap.ScopeShortfall)
		_, _ = fmt.Fprintf(logSink,
			`{"event":"scope_completeness_parked","run_id":%q,"stage_id":%q,"branch":%q,"head_sha":%q,"base_sha":%q,"verified_tree_sha":%q,"tree_sha":%q,"missing_paths":%s}`+"\n",
			cfg.runID, cfg.stageID, branch, cap.HeadSHA, cap.BaseSHA, verifiedTreeSHA, cap.TreeSHA, missingJSON)
		if _, err := client.ShipPullRequest(ctx, upload.ShipPullRequestArgs{
			RunID:        cfg.runID,
			StageID:      cfg.stageID,
			PrivateKey:   issued.PrivateKey,
			Outcome:      "scope_park",
			Branch:       branch,
			HeadSHA:      cap.HeadSHA,
			BaseSHA:      cap.BaseSHA,
			TreeSHA:      verifiedTreeSHA,
			MissingPaths: cap.ScopeShortfall,
		}); err != nil {
			return fmt.Errorf("report scope-completeness park: %w", err)
		}
		_, _ = fmt.Fprintf(logSink,
			`{"event":"scope_completeness_park_reported","run_id":%q,"stage_id":%q,"branch":%q,"head_sha":%q}`+"\n",
			cfg.runID, cfg.stageID, branch, cap.HeadSHA)
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
	// verified_tree_sha + tree_sha close the audit chain (#960): the trail
	// proves verified_tree_sha == tree_sha unconditionally — on the
	// reverify-pass path the pre-push hook rebound verified_tree_sha to the
	// re-verified pushed tree (#969); the original gate tree's provenance
	// lives in verified_tree_mismatch + pushed_tree_reverified above.
	_, _ = fmt.Fprintf(logSink,
		`{"event":"pull_request_opened","run_id":%q,"stage_id":%q,"pr_number":%d,"pr_url":%q,"head_sha":%q,"verified_tree_sha":%q,"tree_sha":%q}`+"\n",
		cfg.runID, cfg.stageID, prRes.PRNumber, prRes.PRURL, cap.HeadSHA, verifiedTreeSHA, cap.TreeSHA,
	)

	// Diff size: count files via gitdiff against the base ref.
	// Best-effort; failure here doesn't block the artifact upload.
	filesChanged := 0
	if d, err := (&gitdiff.Runner{}).Run(ctx, baseRef, repoDir); err == nil {
		filesChanged = len(d.ChangedFiles)
	}

	artifactFields := map[string]any{
		"pr_number":           prRes.PRNumber,
		"pr_url":              prRes.PRURL,
		"branch":              branch,
		"head_sha":            cap.HeadSHA,
		"base_sha":            cap.BaseSHA,
		"title":               title,
		"body":                body,
		"files_changed_count": filesChanged,
	}
	// Base-rebase re-invoke exemption delta (#1218): include the supplemental set
	// ONLY when the re-invoke produced one, so every non-re-invoke ship omits the
	// key entirely and stays byte-identical. The backend decodes it off
	// pullRequestBody.SupplementalScopeExemptions and re-emits a supplemental
	// scope_files_exempted audit row — the trace bundle that would normally carry
	// these already shipped before the re-invoke ran (#742 forward gating).
	// Marshal through scopeExemptionEvidence (NOT the tagless internal
	// scopeExemption) so the wire keys are the lowercase path/reason the backend's
	// pullRequestBody.SupplementalScopeExemptions decoder expects — the
	// cross-boundary seam pinned by the runner+backend tests.
	if len(supplementalExemptions) > 0 {
		wire := make([]scopeExemptionEvidence, 0, len(supplementalExemptions))
		for _, e := range supplementalExemptions {
			wire = append(wire, scopeExemptionEvidence(e))
		}
		artifactFields["supplemental_scope_exemptions"] = wire
	}
	artifactBody, _ := json.Marshal(artifactFields)

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

// childNoChangesReason builds the failure reason for a decomposed-child
// implement stage that produced no changes (#1258 slice C / #1279). The
// diagnostic is position-aware on the child's 0-based sub_plan slice index:
//
//   - slice 0 has no predecessor, so a no-changes child is overwhelmingly a
//     genuine no-op or a planning/decomposition error — advise reviewing the
//     sub-plan scope (preserves the #1036 framing intent).
//   - slice N>0 is a dependent slice: the merged changes of predecessor slices
//     0..N-1 are absent from this slice's isolated base, so code referencing
//     them could not compile and was correctly not written. Name the recovery:
//     consolidate the predecessor slices via fishhawk_consolidate_slices, then
//     re-drive against the integrated base.
//
// BOTH branches retain the literal `child_no_changes` token: it is load-bearing
// for audit/await keying and the #1036 terminalization mirroring.
func childNoChangesReason(sliceIndex int) string {
	if sliceIndex <= 0 {
		return "child_no_changes: decomposition child slice 0 implement stage produced no changes; " +
			"slice 0 has no predecessor slice, so a no-changes child is overwhelmingly a genuine no-op or a " +
			"planning/decomposition error — review the sub-plan scope. Stage terminalized retryable (#1036)."
	}
	return fmt.Sprintf("child_no_changes: dependent child slice %d implement stage produced no changes; "+
		"the merged changes of predecessor slices 0..%d (every slice before this one) are absent from this "+
		"slice's isolated base, so code referencing them could not compile and was correctly not written. "+
		"Recovery: re-drive this child's implement stage with fishhawk_retry_stage (category C is retryable), "+
		"then fishhawk_run_children on the parent — the wave loop integrates the predecessor slices and "+
		"re-bases this slice on the consolidated branch so it can compile. Do NOT call "+
		"fishhawk_consolidate_slices while this child is failed; it returns 409 children_failed by design. "+
		"Stage terminalized retryable (#1036).",
		sliceIndex, sliceIndex-1)
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

// childSliceBranch is the per-child sole-writer branch name a decomposed
// child pushes onto (E24.1 / #1141 / ADR-041 point 1):
// fishhawk/run-<short-parent>/slice-<sliceIndex>. Each slice index is
// minted once by orchestrator fanout, so each child owns a distinct
// branch cut fresh from the declared base — replacing the pre-ADR-041
// shared fishhawk/run-<parent> branch every sibling force-pushed onto.
// Fan-in onto the consolidated branch is ADR-041 / E24.2, out of scope
// here.
func childSliceBranch(decomposedFromRunID string, sliceIndex int) string {
	return fmt.Sprintf("fishhawk/run-%s/slice-%d", shortID(decomposedFromRunID), sliceIndex)
}

// scopeHandoffDir is the directory the run/stage-keyed scope handoff lives in
// (#581, keyed by #1777). var (not const) so tests can redirect it to a
// t.TempDir path and avoid /tmp pollution / parallel-test races.
var scopeHandoffDir = "/tmp"

// scopeHandoffPath mirrors the PR-description handoff: the runner writes the
// implement stage's resolved scope.files here so the out-of-process CLI auto-PR
// path (cli/cmd/fishhawk/autopr.go, a separate Go module) can bound its staging
// to the same declared paths (#581). Keyed by the FULL run id + stage id (#1777)
// so parallel implement runners on one host no longer share the single fixed
// /tmp/fishhawk-scope.json — the last writer could otherwise bound another run's
// commit to the wrong scope. No legacy fixed-path fallback is needed (unlike the
// PR-description handoff): the runner writer and the CLI reader upgrade in
// lockstep within one binary set, so there is no old-writer/new-reader
// deprecation window to bridge. The CLI mirrors this EXACT format string in its
// own scopeFilePath.
func scopeHandoffPath(runID, stageID string) string {
	return filepath.Join(scopeHandoffDir, fmt.Sprintf("fishhawk-scope-%s-%s.json", runID, stageID))
}

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
func writeScopeHandoff(cfg config, files []upload.ScopeFile, logSink io.Writer) {
	path := scopeHandoffPath(cfg.runID, cfg.stageID)
	data, err := json.Marshal(scopeHandoff{Files: files})
	if err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"scope_handoff_failed","reason":"marshal","detail":%q}`+"\n", err.Error())
		return
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"scope_handoff_failed","reason":"write","detail":%q}`+"\n", err.Error())
		return
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"scope_handoff_written","path":%q,"file_count":%d}`+"\n",
		path, len(files))
}

// syncWriter serializes concurrent Write calls to an underlying writer
// behind a mutex. The runner's structured stderr stream (logSink) has
// multiple concurrent producers during the agent invocation — the agent
// heartbeat goroutine (via inv.ProgressSink) and the mid-stage
// scope-amendment watcher (#1035) — and io.Writer carries no concurrency
// guarantee, so an unguarded pair of single-line fmt.Fprintf writes could
// interleave into a line the fishhawk-mcp relay's JSON scanner rejects.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func newSyncWriter(w io.Writer) *syncWriter {
	return &syncWriter{w: w}
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// scopeAmendmentWatchInterval is how often watchScopeAmendments re-lists
// the run's scope amendments looking for a newly-pending request. A var so
// tests can shorten it. The agent's own ?wait long-poll (#1035) is what
// makes a decision reach it promptly; this interval only bounds how fast
// the in-band scope_amendment_pending SIGNAL reaches the operator.
var scopeAmendmentWatchInterval = 20 * time.Second

// watchScopeAmendments polls the run's scope amendments during the agent
// invocation and emits a single-line scope_amendment_pending JSONL event
// the first time it observes each newly-pending amendment (#1035). That
// event is the in-band signal the fishhawk_run_stage relay surfaces so an
// operator driving a second session can decide a mid-stage request via
// fishhawk_decide_scope_amendment while the agent blocks on its own ?wait
// long-poll — guaranteeing an in-window decision is HONORED rather than
// the agent having already proceeded as-denied.
//
// Best-effort and implement-only: a nil client, an empty mcpToken, or a
// non-implement stage disables it (clean no-op). Fetch errors are swallowed
// (the operator still has fishhawk_list_scope_amendments). Each amendment_id
// emits at most once. The caller cancels ctx and joins a WaitGroup to stop
// it before the post-invoke phase resumes writing to the shared sink.
func watchScopeAmendments(ctx context.Context, client uploadClient, cfg config, mcpToken, stageType string, sink io.Writer) {
	if client == nil || mcpToken == "" || stageType != "implement" {
		return
	}
	seen := make(map[string]struct{})
	ticker := time.NewTicker(scopeAmendmentWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			items, err := client.FetchScopeAmendments(ctx, upload.FetchScopeAmendmentsArgs{
				RunID:    cfg.runID,
				MCPToken: mcpToken,
			})
			if err != nil {
				continue
			}
			emitPendingScopeAmendments(seen, items, cfg.runID, cfg.stageID, sink)
		}
	}
}

// emitPendingScopeAmendments writes a scope_amendment_pending JSONL line
// for each pending amendment in items not already in seen, marking it seen
// so a later poll does not re-emit it. Factored out of watchScopeAmendments
// so the literal-JSONL seam contract (#1035, #618 — the field set is
// {event, run_id, stage_id, amendment_id, paths}) and the emit-at-most-once
// behavior are unit-testable without timing. The paths array marshals to
// the same [{path, operation}, ...] shape the fishhawk-mcp relay decodes.
func emitPendingScopeAmendments(seen map[string]struct{}, items []upload.ScopeAmendment, runID, stageID string, sink io.Writer) {
	for _, a := range items {
		if a.Status != "pending" {
			continue
		}
		if _, ok := seen[a.ID]; ok {
			continue
		}
		seen[a.ID] = struct{}{}
		pathsJSON, _ := json.Marshal(a.Paths)
		_, _ = fmt.Fprintf(sink,
			`{"event":"scope_amendment_pending","run_id":%q,"stage_id":%q,"amendment_id":%q,"paths":%s}`+"\n",
			runID, stageID, a.ID, pathsJSON)
	}
}

// refreshScopeAmendments fetches the run's scope amendments with the
// retained run-bound fhm_ bearer (mcpToken, from the FetchMCPToken
// call that fed the agent's FISHHAWK_API_TOKEN env) and folds the
// paths of every APPROVED amendment into cfg.scopeFiles, deduped by
// path (E22.X / #961). Called once, after the agent invocation settles
// and BEFORE any committed-tree gate or StageScoped call reads
// cfg.scopeFiles, so the verify gates and the push see the same folded
// tree (#960 invariant) and the #818/#825 created-out-of-scope gate
// honors approved creates.
//
// No-ops when the token is absent (fetch failed / never ran) or when
// the scope is empty — an empty scope is the `git add -A` fallback,
// which already stages everything; folding would silently NARROW the
// commit to just the amendment paths. Best-effort throughout: a fetch
// failure logs scope_amendment_refresh_failed and the original scope
// stays authoritative.
// The returned []agent.Event carries a single scope_amendments_folded
// policy_event recording EXACTLY the paths this call folded into
// cfg.scopeFiles for this commit (the approved-and-not-already-present
// set) — the authoritative per-commit fold record. The backend reads it
// via bundle.ExtractScopeAmendmentsFolded and subtracts it from the
// review-surface scope_drift, so an approved-amendment path that landed
// in the pushed HEAD is no longer reported to the implement reviewer as
// drift-excluded (#1317). The slice is sourced ONLY from the fold (never
// from amendment intent): an approved-but-already-present or unapproved
// path is never in `added`, so an approved-but-NOT-folded path stays as
// real drift downstream. nil (no event) on the no-op guards and when
// nothing was folded — mirroring the absent scope_drift event.
func refreshScopeAmendments(ctx context.Context, client uploadClient, cfg *config, mcpToken string, logSink io.Writer) []agent.Event {
	if client == nil || mcpToken == "" || len(cfg.scopeFiles) == 0 {
		return nil
	}
	items, err := client.FetchScopeAmendments(ctx, upload.FetchScopeAmendmentsArgs{
		RunID:    cfg.runID,
		MCPToken: mcpToken,
	})
	if err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"scope_amendment_refresh_failed","run_id":%q,"stage_id":%q,"detail":%q}`+"\n",
			cfg.runID, cfg.stageID, err.Error())
		return nil
	}
	existing := make(map[string]struct{}, len(cfg.scopeFiles))
	for _, f := range cfg.scopeFiles {
		existing[f.Path] = struct{}{}
	}
	var added []string
	for _, a := range items {
		if a.Status != "approved" {
			continue
		}
		for _, p := range a.Paths {
			if p.Path == "" {
				continue
			}
			if _, ok := existing[p.Path]; ok {
				continue
			}
			existing[p.Path] = struct{}{}
			cfg.scopeFiles = append(cfg.scopeFiles, upload.ScopeFile(p))
			added = append(added, p.Path)
		}
	}
	if len(added) == 0 {
		return nil
	}
	addedJSON, _ := json.Marshal(added)
	_, _ = fmt.Fprintf(logSink,
		`{"event":"scope_amendments_folded","run_id":%q,"stage_id":%q,"added":%s}`+"\n",
		cfg.runID, cfg.stageID, addedJSON)
	// Mirror the scope_drift policy_event shape (computeAndEmitDiff) so the
	// folded set rides into BOTH bundle variants (PackBytes / redactEvents)
	// alongside the pre-fold scope_drift snapshot.
	return []agent.Event{{
		Kind: "policy_event",
		Payload: agent.MakePayload(map[string]any{
			"check": "scope_amendments_folded",
			"added": added,
		}),
	}}
}

// scopePaths extracts the repo-relative path list from the resolved
// scope.files, dropping entries with an empty path. Used to bound both
// the policy diff staging and the implement commit.
// missingScopeFilesMessage builds the category-B recovery message for the
// pre-push scope-completeness (shortfall) gate (#1151), annotating each missing
// declared path with its declared operation (create/modify/delete) from
// scopeFiles. declaredCount/committedCount render the precise "declared N scope
// file(s), committed M" preamble from real data (the MissingScopeFiles committed
// return), not a recomputation. `missing` is the UNEXEMPTED remainder (the
// validated #1153 self-exemptions already subtracted); `exempted` names the
// declared paths the agent self-exempted, appended as context so the message
// distinguishes a dropped edit from a deliberately-justified no-op. The recovery
// guidance: justify the file in-band via the scope self-exempt sidecar if it
// correctly needs no change, replan to drop it, or fishhawk_resume_run.
func missingScopeFilesMessage(scopeFiles []upload.ScopeFile, missing, exempted []string, declaredCount, committedCount int) string {
	op := make(map[string]string, len(scopeFiles))
	for _, f := range scopeFiles {
		op[f.Path] = f.Operation
	}
	annotated := make([]string, 0, len(missing))
	for _, m := range missing {
		if o := op[m]; o != "" {
			annotated = append(annotated, fmt.Sprintf("%s (%s)", m, o))
		} else {
			annotated = append(annotated, m)
		}
	}
	exemptNote := ""
	if len(exempted) > 0 {
		exemptNote = fmt.Sprintf(" (%d declared path(s) were self-exempted in-band and are not counted here: %s)",
			len(exempted), strings.Join(exempted, ", "))
	}
	return fmt.Sprintf("declared %d scope file(s), committed %d; missing: %s%s. "+
		"The implement commit did not touch every concrete file the approved plan declared in scope.files — "+
		"a declared edit was dropped (the subset-PR class). If a missing file correctly needs no change, justify it "+
		"in-band via the scope self-exempt sidecar (#1153); otherwise replan to drop the intentionally-unchanged "+
		"file, or recover with fishhawk_resume_run.",
		declaredCount, committedCount, strings.Join(annotated, ", "), exemptNote)
}

// scopeJustificationDir is the directory the run/stage-keyed scope self-exempt
// sidecar lives in (#1153). var (not const) so tests can redirect it to a
// t.TempDir, avoiding /tmp pollution / parallel-test races — the same seam
// pattern as pullRequestDescriptionPath.
var scopeJustificationDir = "/tmp"

// scopeJustificationPath mirrors prompt.ScopeJustificationPath in the backend:
// the run/stage-keyed path the implement agent writes its scope self-exempt
// sidecar to and the runner reads it from (#1153). The format string is
// hardcoded in both independent modules by design — the same coordination as
// pullRequestDescriptionPath/PullRequestDescriptionPath. The FULL run + stage
// ids key the path so a leftover sidecar from another run/stage can never
// collide (the first of three freshness defenses; the others are the embedded-
// id validation in loadScopeExemptions and the pre-invoke sweep in run()).
func scopeJustificationPath(runID, stageID string) string {
	return filepath.Join(scopeJustificationDir, fmt.Sprintf("fishhawk-scope-justifications-%s-%s.json", runID, stageID))
}

// scopeExemption is one validated scope self-exemption (#1153): a declared
// scope.files path the agent deliberately left unchanged plus its reason.
type scopeExemption struct {
	Path   string
	Reason string
}

// sweepStaleScopeJustification deletes any leftover scope self-exempt sidecar
// at this run/stage's keyed path before the agent is invoked (#1153) — the
// pre-invoke freshness defense that stops a same-keyed leftover from a prior
// retry of THIS run/stage bleeding a stale exemption into a fresh attempt.
// Best-effort: a not-exist (the common case) is silent; a leftover that was
// actually removed emits scope_justification_swept.
func sweepStaleScopeJustification(cfg config, logSink io.Writer) {
	path := scopeJustificationPath(cfg.runID, cfg.stageID)
	if rerr := os.Remove(path); rerr == nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"scope_justification_swept","run_id":%q,"stage_id":%q,"path":%q}`+"\n",
			cfg.runID, cfg.stageID, path)
	}
}

// loadScopeExemptions reads + validates the agent's scope self-exempt sidecar
// (#1153) fail-closed. An absent (or unreadable) sidecar returns nil — the
// normal strict path, no log. On a present sidecar it deletes the file on EVERY
// return path (consumed/malformed/stale all clean up, so an invalid sidecar is
// never left behind to bleed into a later read — binding condition 4), and:
//   - fails closed (nil) on malformed JSON, logging scope_justification_invalid;
//   - fails closed (nil) when the embedded run_id/stage_id do not match cfg,
//     logging scope_justification_stale (a leftover from another run/stage);
//   - drops any entry whose path is not a CONCRETE declared scope.files path
//     (trailing-slash directory entries and unknown paths excluded) or whose
//     reason is empty/whitespace, logging scope_justification_entry_ignored.
//
// `declared` is the declared scope.files path set (scopePaths(cfg.scopeFiles)).
// Returns the surviving validated entries.
func loadScopeExemptions(cfg config, declared []string, logSink io.Writer) []scopeExemption {
	path := scopeJustificationPath(cfg.runID, cfg.stageID)
	raw, err := os.ReadFile(path)
	if err != nil {
		// Absent sidecar is the common no-op (strict gate, no exemptions); any
		// other read error is fail-closed too. Only an existing file's content
		// can exempt anything, so no log on the not-exist path.
		return nil
	}
	// A present sidecar is consumed regardless of outcome: remove it on every
	// return path so a malformed/stale/parsed sidecar is never left behind.
	defer func() { _ = os.Remove(path) }()

	var doc struct {
		RunID      string `json:"run_id"`
		StageID    string `json:"stage_id"`
		Exemptions []struct {
			Path   string `json:"path"`
			Reason string `json:"reason"`
		} `json:"exemptions"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"scope_justification_invalid","run_id":%q,"stage_id":%q,"path":%q}`+"\n",
			cfg.runID, cfg.stageID, path)
		return nil
	}
	if doc.RunID != cfg.runID || doc.StageID != cfg.stageID {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"scope_justification_stale","run_id":%q,"stage_id":%q,"sidecar_run_id":%q,"sidecar_stage_id":%q}`+"\n",
			cfg.runID, cfg.stageID, doc.RunID, doc.StageID)
		return nil
	}
	concrete := make(map[string]bool, len(declared))
	for _, d := range declared {
		if d == "" || strings.HasSuffix(d, "/") {
			continue // directory entries are not concrete declared paths
		}
		concrete[d] = true
	}
	var out []scopeExemption
	for _, e := range doc.Exemptions {
		if !concrete[e.Path] {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"scope_justification_entry_ignored","run_id":%q,"stage_id":%q,"path":%q,"reason":"not a concrete declared scope.files path"}`+"\n",
				cfg.runID, cfg.stageID, e.Path)
			continue
		}
		if strings.TrimSpace(e.Reason) == "" {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"scope_justification_entry_ignored","run_id":%q,"stage_id":%q,"path":%q,"reason":"empty justification reason"}`+"\n",
				cfg.runID, cfg.stageID, e.Path)
			continue
		}
		out = append(out, scopeExemption{Path: e.Path, Reason: e.Reason})
	}
	return out
}

// partitionExemptedMissing splits the scope-completeness gate's `missing`
// declared-path set into the subset covered by validated self-exemptions
// (#1153) and the unexempted remainder. Only paths that are actually missing
// AND exempted land in `exempted`; the remainder still fails the gate.
// Order-preserving over `missing`. With no exemptions every missing path
// remains, keeping the strict gate byte-identical.
func partitionExemptedMissing(missing []string, exemptions []scopeExemption) (exempted, remaining []string) {
	if len(exemptions) == 0 {
		return nil, missing
	}
	ex := make(map[string]bool, len(exemptions))
	for _, e := range exemptions {
		ex[e.Path] = true
	}
	for _, m := range missing {
		if ex[m] {
			exempted = append(exempted, m)
		} else {
			remaining = append(remaining, m)
		}
	}
	return exempted, remaining
}

// toScopeExemptions maps the prompt-response wire type (upload.ScopeExemption)
// to the runner's internal scopeExemption (#1229). The operator's
// exempt_scope_files exemptions arrive on the implement prompt rather than the
// agent's #1153 sidecar; this is the single conversion at the fetch site. nil
// in (no exemptions delivered) returns nil — the strict gate default.
func toScopeExemptions(in []upload.ScopeExemption) []scopeExemption {
	if len(in) == 0 {
		return nil
	}
	out := make([]scopeExemption, 0, len(in))
	for _, e := range in {
		out = append(out, scopeExemption{Path: e.Path, Reason: e.Reason})
	}
	return out
}

// mergeExemptions unions two scope-exemption sets by Path (#1229),
// deduplicating so the agent's self-exemptions (#1153) and the operator's
// exempt_scope_files exemptions both subtract from the #1151 shortfall without
// double-counting. The first occurrence of a path wins (`a` before `b`), which
// only affects the surfaced reason — partitionExemptedMissing keys on Path
// alone. Order-preserving: all of `a`, then `b`'s new paths. Returns nil when
// both are empty so the strict gate stays byte-identical.
func mergeExemptions(a, b []scopeExemption) []scopeExemption {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]scopeExemption, 0, len(a)+len(b))
	for _, e := range a {
		if seen[e.Path] {
			continue
		}
		seen[e.Path] = true
		out = append(out, e)
	}
	for _, e := range b {
		if seen[e.Path] {
			continue
		}
		seen[e.Path] = true
		out = append(out, e)
	}
	return out
}

// diffExemptions returns the entries in `honored` (keyed by Path) absent from
// `alreadySealed` — the base-rebase re-invoke exemption delta (#1218): what the
// final scope-completeness gate honored that the trace bundle's already-sealed
// scope_files_exempted event did NOT carry. The bundle ships at push time under
// #742 forward gating, BEFORE the re-invoke reloads the re-invoked agent's fresh
// sidecar, so this delta is the set the audit/review never saw. Order-preserving
// over `honored`; returns nil when `honored` is a subset of `alreadySealed` (no
// new exemption — the common base-rebase case), keeping the non-re-invoke ship
// byte-identical.
func diffExemptions(honored, alreadySealed []scopeExemption) []scopeExemption {
	if len(honored) == 0 {
		return nil
	}
	sealed := make(map[string]bool, len(alreadySealed))
	for _, e := range alreadySealed {
		sealed[e.Path] = true
	}
	var out []scopeExemption
	for _, e := range honored {
		if sealed[e.Path] {
			continue
		}
		out = append(out, e)
	}
	return out
}

// scopeFilesExemptedEvent builds the scope_files_exempted trace event (#1153)
// composeGateEvidence folds into gate_evidence and the audit surfaces. Emitted
// EXACTLY ONCE in run() before composeGateEvidence; the gate site never
// re-emits (binding condition 1).
func scopeFilesExemptedEvent(cfg config, exemptions []scopeExemption) agent.Event {
	// Reuse the gate_evidence wire type so the composeGateEvidence fold
	// (Exemptions []scopeExemptionEvidence) decodes the event byte-for-byte.
	out := make([]scopeExemptionEvidence, 0, len(exemptions))
	for _, e := range exemptions {
		out = append(out, scopeExemptionEvidence(e))
	}
	return agent.Event{
		Kind: "scope_files_exempted",
		Payload: agent.MakePayload(map[string]any{
			"run_id":     cfg.runID,
			"stage_id":   cfg.stageID,
			"exemptions": out,
		}),
	}
}

// fixupSelfReportDir is the directory the run/stage-keyed fix-up self-report
// sidecar lives in (#1210). var (not const) so tests can redirect it to a
// t.TempDir, avoiding /tmp pollution / parallel-test races — the same seam
// pattern as scopeJustificationDir.
var fixupSelfReportDir = "/tmp"

// fixupSelfReportPath mirrors prompt.FixupSelfReportPath in the backend: the
// run/stage-keyed path the fix-up agent writes its claimed verify outcome to
// and the runner reads it from (#1210). The format string is hardcoded in both
// independent modules by design — the same coordination as
// scopeJustificationPath / ScopeJustificationPath. The FULL run + stage ids key
// the path so a leftover sidecar from another run/stage can never collide (the
// first of three freshness defenses; the others are the embedded-id validation
// in loadFixupSelfReport and the pre-invoke sweep in run()).
func fixupSelfReportPath(runID, stageID string) string {
	return filepath.Join(fixupSelfReportDir, fmt.Sprintf("fishhawk-fixup-selfreport-%s-%s.json", runID, stageID))
}

// fixupSelfReport is the agent's structured self-report of its claimed verify
// outcome on a fix-up pass (#1210). The json tags match the sidecar shape the
// backend's writeFixupSelfReport instructs the agent to write: RunID/StageID
// are the embedded freshness ids validated against cfg, VerifyStatus is the
// claimed committed-tree verify verdict ("passed" | "failed").
type fixupSelfReport struct {
	RunID        string `json:"run_id"`
	StageID      string `json:"stage_id"`
	VerifyStatus string `json:"verify_status"`
}

// sweepStaleFixupSelfReport deletes any leftover fix-up self-report sidecar at
// this run/stage's keyed path before the agent is invoked (#1210) — the
// pre-invoke freshness defense mirroring sweepStaleScopeJustification. Best-
// effort: a not-exist (the common case, including every non-fix-up implement
// stage) is silent; a leftover actually removed emits fixup_selfreport_swept.
func sweepStaleFixupSelfReport(cfg config, logSink io.Writer) {
	path := fixupSelfReportPath(cfg.runID, cfg.stageID)
	if rerr := os.Remove(path); rerr == nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"fixup_selfreport_swept","run_id":%q,"stage_id":%q,"path":%q}`+"\n",
			cfg.runID, cfg.stageID, path)
	}
}

// loadFixupSelfReport reads + validates the agent's fix-up self-report sidecar
// (#1210) fail-closed, mirroring loadScopeExemptions byte-for-byte in shape. An
// absent (or unreadable) sidecar returns "" — the common no-op, no log. On a
// present sidecar it deletes the file on EVERY return path (consumed/malformed/
// stale all clean up, so an invalid sidecar is never left behind to bleed into a
// later read), and:
//   - fails closed ("") on malformed JSON, logging fixup_selfreport_invalid;
//   - fails closed ("") when the embedded run_id/stage_id do not match cfg,
//     logging fixup_selfreport_stale (a leftover from another run/stage);
//   - fails closed ("") when verify_status is not in {"passed","failed"}
//     (including absent/empty), logging fixup_selfreport_status_ignored.
//
// Returns the validated claimed verify status ("passed" | "failed") or "".
func loadFixupSelfReport(cfg config, logSink io.Writer) string {
	path := fixupSelfReportPath(cfg.runID, cfg.stageID)
	raw, err := os.ReadFile(path)
	if err != nil {
		// Absent sidecar is the common no-op (the agent reported nothing); any
		// other read error is fail-closed too. Only an existing file's content
		// can claim anything, so no log on the not-exist path.
		return ""
	}
	// A present sidecar is consumed regardless of outcome: remove it on every
	// return path so a malformed/stale/parsed sidecar is never left behind.
	defer func() { _ = os.Remove(path) }()

	var doc fixupSelfReport
	if json.Unmarshal(raw, &doc) != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"fixup_selfreport_invalid","run_id":%q,"stage_id":%q,"path":%q}`+"\n",
			cfg.runID, cfg.stageID, path)
		return ""
	}
	if doc.RunID != cfg.runID || doc.StageID != cfg.stageID {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"fixup_selfreport_stale","run_id":%q,"stage_id":%q,"sidecar_run_id":%q,"sidecar_stage_id":%q}`+"\n",
			cfg.runID, cfg.stageID, doc.RunID, doc.StageID)
		return ""
	}
	if doc.VerifyStatus != "passed" && doc.VerifyStatus != "failed" {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"fixup_selfreport_status_ignored","run_id":%q,"stage_id":%q,"verify_status":%q}`+"\n",
			cfg.runID, cfg.stageID, doc.VerifyStatus)
		return ""
	}
	return doc.VerifyStatus
}

// fixupCommitMessageDir is the directory the run/stage-keyed fix-up commit-
// message sidecar lives in (#1572). var (not const) so tests can redirect it to
// a t.TempDir, avoiding /tmp pollution / parallel-test races — the same seam
// pattern as fixupSelfReportDir.
var fixupCommitMessageDir = "/tmp"

// fixupCommitMessagePath mirrors prompt.FixupCommitMessagePath in the backend:
// the run/stage-keyed path a fix-up agent writes its per-pass Conventional-
// Commits commit message to and the runner reads it from (#1572). The format
// string is hardcoded in both independent modules by design — the same
// coordination as fixupSelfReportPath / FixupSelfReportPath.
func fixupCommitMessagePath(runID, stageID string) string {
	return filepath.Join(fixupCommitMessageDir, fmt.Sprintf("fishhawk-fixup-commitmsg-%s-%s.txt", runID, stageID))
}

// sweepStaleFixupCommitMessage deletes any leftover fix-up commit-message
// sidecar at this run/stage's keyed path before the agent is invoked (#1572) —
// the pre-invoke freshness defense mirroring sweepStaleFixupSelfReport. Without
// it, a pass whose agent never re-writes the file would silently reuse the
// PRIOR pass's message. Best-effort: a not-exist (the common case, including
// every non-fix-up implement stage) is silent; a leftover actually removed
// emits fixup_commitmsg_swept.
func sweepStaleFixupCommitMessage(cfg config, logSink io.Writer) {
	path := fixupCommitMessagePath(cfg.runID, cfg.stageID)
	if rerr := os.Remove(path); rerr == nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"fixup_commitmsg_swept","run_id":%q,"stage_id":%q,"path":%q}`+"\n",
			cfg.runID, cfg.stageID, path)
	}
}

// loadFixupCommitMessage reads the agent's fix-up commit-message sidecar (#1572)
// and splits it into (subject, body). It deletes the file on EVERY return path
// (delete-after-read) so a stale sidecar can never bleed into a later pass.
// Returns ok=false when the sidecar is absent, unreadable, or empty/whitespace-
// only — the fallback cases the caller resolves to a conventional-shaped
// synthetic subject. On success the first line is the subject (the Conventional-
// Commits header) and the remainder after it is the body.
func loadFixupCommitMessage(cfg config, logSink io.Writer) (subject, body string, ok bool) {
	path := fixupCommitMessagePath(cfg.runID, cfg.stageID)
	raw, err := os.ReadFile(path)
	if err != nil {
		// Absent sidecar is the common no-op (the agent wrote nothing); any
		// other read error is fail-closed too. Only an existing file's content
		// can supply a message, so no log on the not-exist path.
		return "", "", false
	}
	// A present sidecar is consumed regardless of outcome: remove it on every
	// return path so a stale sidecar is never reused by a later pass.
	defer func() { _ = os.Remove(path) }()

	// TrimSpace strips leading/trailing whitespace (incl. leading blank lines),
	// so an empty/whitespace-only sidecar is treated as missing (fallback wins).
	text := strings.TrimSpace(strings.ReplaceAll(string(raw), "\r\n", "\n"))
	if text == "" {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"fixup_commitmsg_empty","run_id":%q,"stage_id":%q,"path":%q}`+"\n",
			cfg.runID, cfg.stageID, path)
		return "", "", false
	}
	// First line is the subject; the remainder after the first newline (leading
	// blank lines trimmed) is the body. text is already TrimSpace'd, so the
	// first line is non-empty.
	lines := strings.SplitN(text, "\n", 2)
	subject = strings.TrimSpace(lines[0])
	if len(lines) == 2 {
		body = strings.TrimSpace(lines[1])
	}
	return subject, body, true
}

// fixupCommitMessage resolves the commit message for a fix-up pass (#1572): the
// agent-authored per-pass message from the run/stage-keyed sidecar (consumed +
// deleted by loadFixupCommitMessage), falling back to a conventional-shaped,
// per-pass-unique `chore: fishhawk fixup stage <id> (base <sha>)` when the
// sidecar is absent/empty. baseTipSHA is the pass's base tip (HEAD before the
// fix-up commit) — its short form makes the fallback unique per pass, since each
// fix-up pass starts from a different base tip, so two passes never carry an
// identical fallback subject.
func fixupCommitMessage(cfg config, baseTipSHA string, logSink io.Writer) string {
	if subject, body, ok := loadFixupCommitMessage(cfg, logSink); ok {
		if body == "" {
			return subject
		}
		return subject + "\n\n" + body
	}
	return fmt.Sprintf("chore: fishhawk fixup stage %s (base %s)",
		shortID(cfg.stageID), shortID(baseTipSHA))
}

// implementCommitMessageDir is the directory the run/stage-keyed INITIAL-implement
// commit-message sidecar lives in (#1686). var (not const) so tests can redirect
// it to a t.TempDir, avoiding /tmp pollution / parallel-test races — the same
// seam pattern as fixupCommitMessageDir.
var implementCommitMessageDir = "/tmp"

// implementCommitMessagePath mirrors prompt.ImplementCommitMessagePath in the
// backend: the run/stage-keyed path the initial implement agent writes its clean
// Conventional-Commits commit message to and the runner reads it from (#1686).
// The format string is hardcoded in all three independent modules (backend
// prompt, runner, CLI) by design — the same coordination as fixupCommitMessagePath
// / FixupCommitMessagePath.
func implementCommitMessagePath(runID, stageID string) string {
	return filepath.Join(implementCommitMessageDir, fmt.Sprintf("fishhawk-implement-commitmsg-%s-%s.txt", runID, stageID))
}

// sweepStaleImplementCommitMessage deletes any leftover initial-implement commit-
// message sidecar at this run/stage's keyed path before the agent is invoked
// (#1686) — the pre-invoke freshness defense mirroring sweepStaleFixupCommitMessage.
// Without it, a retry whose agent never re-writes the file would silently reuse a
// PRIOR attempt's message. Best-effort: a not-exist (the common case) is silent;
// a leftover actually removed emits implement_commitmsg_swept.
func sweepStaleImplementCommitMessage(cfg config, logSink io.Writer) {
	path := implementCommitMessagePath(cfg.runID, cfg.stageID)
	if rerr := os.Remove(path); rerr == nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"implement_commitmsg_swept","run_id":%q,"stage_id":%q,"path":%q}`+"\n",
			cfg.runID, cfg.stageID, path)
	}
}

// sweepStalePullRequestDescription deletes any leftover PR-description handoff at
// THIS run/stage's keyed path AND at the legacy fixed path before the agent is
// invoked (#1777) — the pre-invoke freshness defense mirroring
// sweepStaleImplementCommitMessage. A stale foreign PR file must never be
// silently parsed (the exact clobber this issue fixes), so the sweep covers
// BOTH the path this run's agent may write (keyed) and the path an older
// prompt/agent may write (legacy), matching loadAgentAuthoredPR's keyed-first-
// legacy-fallback read (binding condition 2). Best-effort: a not-exist (the
// common case) is silent; a leftover actually removed emits pr_description_swept.
//
// Clearing the legacy path can delete a concurrent OLD-PROMPT run's handoff
// during the deprecation window; that failure is loud (the other run falls back
// to the generic Fishhawk template) and acceptable — the alternative (silently
// parsing a foreign run's PR text) is the bug this issue closes.
//
// Returns false when a path could not be removed for a reason OTHER than not-
// exist: loadAgentAuthoredPR reads these SAME paths after the agent runs, so a
// stale-but-unremovable readable file (e.g. a foreign legacy file in /tmp the
// runner cannot unlink) would otherwise be silently parsed into this run's PR
// title/body — the exact clobber this issue closes. The caller fails the stage
// category-C on false rather than risk that silent parse, mirroring the
// acceptance stale-verdict clear (loud-over-silent, binding condition 2). A
// not-exist (the common case) is silent and never fails.
func sweepStalePullRequestDescription(cfg config, logSink io.Writer) bool {
	ok := true
	for _, path := range []string{
		pullRequestDescriptionPath(cfg.runID, cfg.stageID),
		legacyPullRequestDescriptionPath,
	} {
		rerr := os.Remove(path)
		switch {
		case rerr == nil:
			_, _ = fmt.Fprintf(logSink,
				`{"event":"pr_description_swept","run_id":%q,"stage_id":%q,"path":%q}`+"\n",
				cfg.runID, cfg.stageID, path)
		case !os.IsNotExist(rerr):
			// A stale readable file we cannot unlink survives to be parsed by
			// loadAgentAuthoredPR — fail closed before the agent spawns rather
			// than clobber this run's PR text with a foreign handoff.
			_, _ = fmt.Fprintf(logSink,
				`{"event":"runner_failed","reason":"pr_description_stale_clear","category":"C","path":%q,"detail":%q}`+"\n",
				path, rerr.Error())
			ok = false
		}
	}
	return ok
}

// loadImplementCommitMessage reads the agent's initial-implement commit-message
// sidecar (#1686) and splits it into (subject, body). It deletes the file on
// EVERY return path (delete-after-read) so a stale sidecar can never bleed into a
// later run/stage. Returns ok=false when the sidecar is absent, unreadable, or
// empty/whitespace-only — the fallback cases the caller resolves to today's
// title + "\n\n" + body. On success the first line is the subject (the
// Conventional-Commits header) and the remainder after it is the body.
func loadImplementCommitMessage(cfg config, logSink io.Writer) (subject, body string, ok bool) {
	path := implementCommitMessagePath(cfg.runID, cfg.stageID)
	raw, err := os.ReadFile(path)
	if err != nil {
		// Absent sidecar is the common no-op (an older agent wrote nothing); any
		// other read error is fail-closed too. Only an existing file's content
		// can supply a message, so no log on the not-exist path.
		return "", "", false
	}
	// A present sidecar is consumed regardless of outcome: remove it on every
	// return path so a stale sidecar is never reused by a later run/stage.
	defer func() { _ = os.Remove(path) }()

	// TrimSpace strips leading/trailing whitespace (incl. leading blank lines),
	// so an empty/whitespace-only sidecar is treated as missing (fallback wins).
	text := strings.TrimSpace(strings.ReplaceAll(string(raw), "\r\n", "\n"))
	if text == "" {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"implement_commitmsg_empty","run_id":%q,"stage_id":%q,"path":%q}`+"\n",
			cfg.runID, cfg.stageID, path)
		return "", "", false
	}
	// First line is the subject; the remainder after the first newline (leading
	// blank lines trimmed) is the body. text is already TrimSpace'd, so the
	// first line is non-empty.
	lines := strings.SplitN(text, "\n", 2)
	subject = strings.TrimSpace(lines[0])
	if len(lines) == 2 {
		body = strings.TrimSpace(lines[1])
	}
	return subject, body, true
}

// implementCommitMessage resolves the commit message for the INITIAL (non-fix-up)
// implement commit (#1686): the agent-authored clean Conventional-Commits message
// from the run/stage-keyed sidecar (consumed + deleted by loadImplementCommitMessage),
// falling back to EXACTLY today's title + "\n\n" + body when the sidecar is
// absent/empty — no synthetic subject, so an older agent that writes no sidecar
// sees no behavior change. title/body are the PR title/body sourced unchanged
// from the run/stage-keyed PR-description handoff (#1777).
func implementCommitMessage(cfg config, title, body string, logSink io.Writer) string {
	if subject, sidecarBody, ok := loadImplementCommitMessage(cfg, logSink); ok {
		// Warn-only conventional-commit header check on the sidecar subject,
		// matching the PR-title warn — advisory, never a rewrite or hard failure.
		if !conventionalCommitHeaderRe.MatchString(subject) {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"implement_commitmsg_warning","run_id":%q,"stage_id":%q,"reason":%q}`+"\n",
				cfg.runID, cfg.stageID, "sidecar subject is not a conventional-commit header")
		}
		if sidecarBody == "" {
			return subject
		}
		return subject + "\n\n" + sidecarBody
	}
	return title + "\n\n" + body
}

// terminalVerifyOutcome returns the committed-tree verify verdict already on the
// bundle for this stage (#1210): the verify_summary.Outcome when a verify_summary
// event is present (the fix-loop's authoritative terminal outcome), else the LAST
// verify_run.Outcome (which agrees with verify_summary and is the non-superseded
// committed-tree result per the #1205 comment), else "" when no verify gate ran.
// The vocabulary is the production verify literals "passed" | "failed" | "skipped"
// emitted by runVerifyFixLoop's verify_summary and verifyRunEvent; "" and
// "skipped" are indeterminate to the divergence detector below.
func terminalVerifyOutcome(events []agent.Event) string {
	lastRun := ""
	for _, e := range events {
		switch e.Kind {
		case "verify_summary":
			var w struct {
				Outcome string `json:"outcome"`
			}
			if json.Unmarshal(e.Payload, &w) == nil && w.Outcome != "" {
				return w.Outcome
			}
		case "verify_run":
			var w struct {
				Outcome string `json:"outcome"`
			}
			if json.Unmarshal(e.Payload, &w) == nil {
				lastRun = w.Outcome
			}
		}
	}
	return lastRun
}

// fixupSelfReportDiverges is the pure advisory divergence decision (#1210):
// true iff BOTH the claimed and actual verify statuses are determinate
// ("passed" | "failed") AND they disagree. Any indeterminate side (actual ""/
// "skipped", an absent/invalid claim "") yields false — conservative, near-zero
// false positives (the indeterminate sides never fire a spurious honesty flag).
func fixupSelfReportDiverges(claimed, actual string) bool {
	determinate := func(s string) bool { return s == "passed" || s == "failed" }
	return determinate(claimed) && determinate(actual) && claimed != actual
}

// fixupSelfReportDivergenceEvent builds the advisory fixup_selfreport_divergence
// trace event (#1210) composeGateEvidence folds into gate_evidence. The payload
// carries the agent's CLAIMED verify status and the runner's ACTUAL committed-
// tree verify outcome so the implement review can arbitrate the honesty flag.
// ADVISORY ONLY: it never demotes res.OK / res.FailureCategory and never affects
// budget.
func fixupSelfReportDivergenceEvent(cfg config, claimed, actual string) agent.Event {
	return agent.Event{
		Kind: "fixup_selfreport_divergence",
		Payload: agent.MakePayload(map[string]any{
			"run_id":                cfg.runID,
			"stage_id":              cfg.stageID,
			"claimed_verify_status": claimed,
			"actual_verify_status":  actual,
		}),
	}
}

// toGitopsBindingAssertions converts the decoded prompt-response
// binding-assertion list into the gitops gate's input shape, keeping the
// gitops package free of the upload import (#1171).
func toGitopsBindingAssertions(assertions []upload.BindingAssertion) []gitops.BindingAssertion {
	if len(assertions) == 0 {
		return nil
	}
	out := make([]gitops.BindingAssertion, 0, len(assertions))
	for _, a := range assertions {
		out = append(out, gitops.BindingAssertion{Type: a.Type, Path: a.Path, Literal: a.Literal})
	}
	return out
}

// bindingAssertionEvidenceEvent builds the binding_assertion event
// composeGateEvidence folds into gate_evidence (#1171). The payload carries
// the count checked plus each assertion's type/path/literal and whether the
// committed tree satisfied it, so the implement review sees which operator
// binding conditions were machine-verified.
func bindingAssertionEvidenceEvent(results []gitops.BindingAssertionResult) agent.Event {
	type assertion struct {
		Type      string `json:"type"`
		Path      string `json:"path"`
		Literal   string `json:"literal"`
		Satisfied bool   `json:"satisfied"`
	}
	assertions := make([]assertion, 0, len(results))
	for _, r := range results {
		assertions = append(assertions, assertion{Type: r.Type, Path: r.Path, Literal: r.Literal, Satisfied: r.Satisfied})
	}
	return agent.Event{
		Kind: "binding_assertion",
		Payload: agent.MakePayload(map[string]any{
			"checked":    len(results),
			"assertions": assertions,
		}),
	}
}

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

// binaryArtifactSizeThreshold is the on-disk size above which an
// executable scope-drift file is flagged as a likely compiled build
// artifact (#980 advisory WARN). 5 MiB clears every plausible source
// file while catching Go binaries (the incident's fishhawk-runner
// binary was 21MB).
const binaryArtifactSizeThreshold = 5 << 20

// isBinaryArtifactDrift classifies a scope-drift path as a likely compiled
// build artifact the agent accidentally produced in the working tree (#980:
// a `go build`-dropped 21MB fishhawk-runner binary rode toward a run's
// commit). Two heuristics, either sufficient: (a) the file on disk is
// executable (any 0111 bit) and larger than binaryArtifactSizeThreshold;
// (b) the path has the Go module-binary shape cmd/<name>/<name> — basename
// equal to its parent directory, itself directly under a cmd/ segment,
// exactly where `go build ./...` from inside a module drops the binary.
// Advisory only: false negatives are acceptable because the post-commit
// ErrCommitOutOfScope assertion and the #818 created-out-of-scope gate are
// the enforcement layers. Returns the stat'd size when available (0 when
// the path is gone or a shape-only hit on an unstattable file).
func isBinaryArtifactDrift(repoDir, path string) (bool, int64) {
	var size int64
	executableOversized := false
	if fi, err := os.Stat(filepath.Join(repoDir, path)); err == nil && fi.Mode().IsRegular() {
		size = fi.Size()
		executableOversized = fi.Mode().Perm()&0o111 != 0 && size > binaryArtifactSizeThreshold
	}
	parts := strings.Split(path, "/")
	n := len(parts)
	cmdBinaryShape := n >= 3 && parts[n-1] == parts[n-2] && parts[n-3] == "cmd"
	return executableOversized || cmdBinaryShape, size
}

// pullRequestDescriptionDir is the directory the run/stage-keyed PR-description
// handoff lives in (#1777). var (not const) so tests can redirect it to a
// t.TempDir, avoiding /tmp pollution / parallel-test races — the same seam
// pattern as implementCommitMessageDir.
var pullRequestDescriptionDir = "/tmp"

// pullRequestDescriptionPath mirrors prompt.PullRequestDescriptionPath in the
// backend: the run/stage-keyed path the implement agent writes its PR
// description to and the runner reads it from (#1777). The format string is
// hardcoded in all three independent modules (backend prompt, runner, CLI) by
// design — the same coordination as implementCommitMessagePath. Keying isolates
// each parallel runner's handoff so the last writer can no longer win and open
// a PR with another run's title/body (the #1775/#1776 incident).
func pullRequestDescriptionPath(runID, stageID string) string {
	return filepath.Join(pullRequestDescriptionDir, fmt.Sprintf("fishhawk-pr-%s-%s.md", runID, stageID))
}

// legacyPullRequestDescriptionPath is the fixed shared path the PR description
// used to be written to before #1777 keyed it. Retained ONLY as the
// deprecation-window fallback: loadAgentAuthoredPR reads the keyed path first
// and falls back to this legacy path (emitting pr_description_legacy_path) so an
// older prompt/agent that still writes the fixed path is not silently lost. var
// (not const) so tests can redirect it to a t.TempDir path.
var legacyPullRequestDescriptionPath = "/tmp/fishhawk-pr.md"

// conventionalCommitHeaderRe matches a Conventional Commits v1.0.0 header
// (#1572): a lowercase type from the allowed set, an optional lowercase scope in
// parens, an optional breaking-change `!`, then `: ` and a non-empty
// description. Applied WARN-ONLY to the agent-authored PR title — a non-match
// emits pr_template_warning and the title is used verbatim; it never rewrites
// the title or fails the stage.
//
// MIRRORED in backend/internal/orchestrator/orchestrator.go's
// conventionalCommitHeaderRe (#1774), which uses the byte-identical pattern to
// decide the consolidated PR title's chore-prefix. The backend is a separate Go
// module, so it cannot import this — keep the two patterns in sync.
var conventionalCommitHeaderRe = regexp.MustCompile(`^(feat|fix|docs|refactor|test|chore|perf|build)(\([a-z0-9/._-]+\))?!?: .+$`)

// prTitleAndBody assembles the PR title and body for the implement
// stage. Tries the agent-authored file first; falls back to the
// generic Fishhawk template when missing or malformed.
//
// Either way the body gets a "Fishhawk attribution" footer appended
// — run id, audit-log URL, branch, and the PR's own branch — so
// auditable provenance is preserved without requiring the agent to
// remember to include it in every PR.
func prTitleAndBody(cfg config, branch string, logSink io.Writer) (title, body string) {
	agentTitle, agentBody, kind := loadAgentAuthoredPR(cfg, logSink)

	switch kind {
	case prSourceAgent:
		title = agentTitle
		body = agentBody
	case prSourceFallback:
		title = fmt.Sprintf("chore: fishhawk implement stage %s", shortID(cfg.stageID))
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
	//
	// MIRRORED in backend/internal/orchestrator/orchestrator.go's
	// consolidatedPRFooter (#1774), which appends the byte-identical literal to
	// the decomposed-parent consolidated PR body. The backend is a separate Go
	// module, so it cannot import this — keep the two literals in sync.
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
// Path resolution (#1777): reads the run/stage-KEYED path first, then falls
// back to the LEGACY fixed path — mirroring the acceptance keyed-first-legacy
// design — so an older prompt/agent that still writes the fixed path is not
// silently lost during the deprecation window (a legacy read emits a
// pr_description_legacy_path deprecation event). The consumed file is deleted on
// EVERY read path (delete-after-read) so a leftover cannot bleed into a later
// run/stage.
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
func loadAgentAuthoredPR(cfg config, logSink io.Writer) (title, body string, kind prSource) {
	keyed := pullRequestDescriptionPath(cfg.runID, cfg.stageID)
	raw, err := os.ReadFile(keyed)
	path := keyed
	if err != nil {
		if !os.IsNotExist(err) {
			// Unreadable keyed file (permissions, etc.): fall back to the
			// generic template, same as the pre-#1777 absent-file no-op.
			return "", "", prSourceFallback
		}
		// Keyed path absent: fall back to the legacy fixed path so a fixed-path
		// prompt render (an older agent/prompt) still lands its PR text.
		legacyRaw, legacyErr := os.ReadFile(legacyPullRequestDescriptionPath)
		if legacyErr != nil {
			// Neither path present is the common no-op (agent didn't follow the
			// instruction, or a stage type that produces no PR). Don't log.
			return "", "", prSourceFallback
		}
		raw = legacyRaw
		path = legacyPullRequestDescriptionPath
		_, _ = fmt.Fprintf(logSink,
			`{"event":"pr_description_legacy_path","run_id":%q,"stage_id":%q,"path":%q}`+"\n",
			cfg.runID, cfg.stageID, path)
	}
	// A present file is consumed regardless of outcome: remove whichever path we
	// read so a stale handoff is never reused by a later run/stage.
	defer func() { _ = os.Remove(path) }()

	text := strings.TrimRight(string(raw), "\n")
	if text == "" {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"pr_template_invalid","reason":%q,"path":%q}`+"\n",
			"empty file", path)
		return "", "", prSourceFallback
	}

	// Split into title (first line) + body (rest after blank line).
	// We tolerate either CRLF or LF; normalize to LF for the body.
	lines := strings.SplitN(text, "\n", 2)
	title = strings.TrimSpace(lines[0])
	if title == "" {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"pr_template_invalid","reason":%q,"path":%q}`+"\n",
			"empty title line", path)
		return "", "", prSourceFallback
	}

	// Warn-only conventional-commit header check (#1572): the title doubles as
	// the commit subject, so we nudge agents toward the Conventional Commits
	// v1.0.0 shape. A non-match is advisory — emit pr_template_warning and use
	// the title VERBATIM. Never a hard failure, never a rewrite. Runs before
	// both prSourceAgent return paths (title-only and title+body).
	if !conventionalCommitHeaderRe.MatchString(title) {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"pr_template_warning","reason":%q,"path":%q}`+"\n",
			"title is not a conventional-commit header", path)
	}

	if len(lines) < 2 {
		// Title-only file is treated as a body-less PR — that's
		// allowed; GitHub accepts an empty PR body. Still log so
		// the operator can spot agents that aren't writing bodies.
		_, _ = fmt.Fprintf(logSink,
			`{"event":"pr_template_warning","reason":%q,"path":%q}`+"\n",
			"title-only (no body)", path)
		return title, "", prSourceAgent
	}

	rest := strings.TrimLeft(lines[1], "\n")
	// The format calls for a blank line between title and body, but
	// agents are sloppy. Trim leading newlines and let it through;
	// only flag truly malformed cases (no separator at all).
	if !strings.HasPrefix(lines[1], "\n") {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"pr_template_warning","reason":%q,"path":%q}`+"\n",
			"no blank line between title and body", path)
	}

	return title, rest, prSourceAgent
}
