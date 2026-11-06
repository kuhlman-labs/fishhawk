package mcpe2e_test

// The cross-boundary regression for #5095: a stage runs under the provider its
// cached workflow declares. TestE2E_ExecutorAgentRouting crosses the cached
// workflow, stage row, stage API, MCP verb, runner argv, provider adapter, signed
// trace and audit row with a real backend, Postgres, fishhawk-mcp and
// fishhawk-runner. Only the provider CLIs are fake: `codex` and `claude` shims
// on PATH record their argv, so every assertion reads what was actually spawned.
//
// TestE2E_TraceUploaded_AgentClosedSet drives real trace ingest with hand-built
// signed bundles, for the manifest values a real runner never produces.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mcptoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
)

const (
	routingRepo     = "kuhlman-labs/fishhawk"
	routingWorkflow = "feature_change"
	routingModel    = "declared-model"
	routingEffort   = "high"
	codexShimName   = "codex"
	claudeShimName  = "claude"
)

// routingBackend is a real backend over a per-test database, wired so a real
// runner can fetch a prompt, upload a plan and ship a signed trace.
type routingBackend struct {
	url       string
	operator  string
	runRepo   runpkg.Repository
	auditRepo audit.Repository
	signing   signing.Repository
}

func newRoutingBackend(t *testing.T) *routingBackend {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timescale.D(90*time.Second))
	defer cancel()

	pool, err := postgres.Connect(ctx, pgtest.NewURL(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	runRepo := runpkg.NewPostgresRepository(pool)
	signingRepo := signing.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	apiRepo := apitoken.NewPostgresRepository(pool)
	srv := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      runRepo,
		SigningRepo:  signingRepo,
		AuditRepo:    auditRepo,
		ArtifactRepo: artifact.NewPostgresRepository(pool),
		ApprovalRepo: approval.NewPostgresRepository(pool),
		MCPTokenRepo: mcptoken.NewPostgresRepository(pool),
		APITokenRepo: apiRepo,
		TraceStore:   tracestore.NewMem(),
		GitHub:       githubclient.New(nil),
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	operator, err := apiRepo.Issue(ctx, "operator@executor-agent-routing", []string{
		"read:runs", "read:audit", "write:runs", "write:approvals", "write:stages",
	})
	if err != nil {
		t.Fatalf("issue operator token: %v", err)
	}
	return &routingBackend{
		url:       httpSrv.URL,
		operator:  operator.PlainText,
		runRepo:   runRepo,
		auditRepo: auditRepo,
		signing:   signingRepo,
	}
}

// traceUploadedPayloads returns the decoded payload of every trace_uploaded
// entry on the run.
func (b *routingBackend) traceUploadedPayloads(t *testing.T, ctx context.Context, runID uuid.UUID) []map[string]any {
	t.Helper()
	entries, err := b.auditRepo.ListForRunByCategory(ctx, runID, "trace_uploaded")
	if err != nil {
		t.Fatalf("list trace_uploaded: %v", err)
	}
	out := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		var payload map[string]any
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			t.Fatalf("decode trace_uploaded payload: %v", err)
		}
		out = append(out, payload)
	}
	return out
}

// buildRoutingRunner builds the real fishhawk-runner into a test-owned dir. It is
// a real binary for the reason local-runner/e2e_test.go gives: the MCP server
// parses its stdout as JSONL, so `go run` chatter would corrupt the stream.
func buildRoutingRunner(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "fishhawk-runner")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), timescale.D(3*time.Minute))
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", binary,
		"github.com/kuhlman-labs/fishhawk/runner/cmd/fishhawk-runner")
	var stderr bytes.Buffer
	build.Stderr = &stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build fishhawk-runner: %v\nstderr: %s", err, stderr.String())
	}
	return binary
}

// routingProbe holds one subtest's shims, recorded argv and checkout. Every
// provider and runner invocation writes one NUL-separated argv file, so "no
// record" means the binary was never started, not even for --version.
type routingProbe struct {
	recordDir  string
	binDir     string
	workingDir string
	runnerPath string
}

func newRoutingProbe(t *testing.T, realRunner string) *routingProbe {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the provider shims are POSIX shell scripts")
	}
	p := &routingProbe{recordDir: t.TempDir(), binDir: t.TempDir(), workingDir: t.TempDir()}

	// FISHHAWK_RUNNER_BIN points at this wrapper, so the runner's argv from the
	// MCP spawn is observable at the process boundary.
	p.runnerPath = filepath.Join(p.binDir, "fishhawk-runner-recording")
	writeRoutingScript(t, p.runnerPath, fmt.Sprintf("#!/bin/sh\nprintf '%%s\\0' \"$@\" > %q/runner.$$.argv\nexec %q \"$@\"\n",
		p.recordDir, realRunner))

	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "e2e@example.invalid"},
		{"config", "user.name", "e2e"},
		{"commit", "-q", "--allow-empty", "-m", "base"},
	} {
		cmd := exec.Command("git", append([]string{"-C", p.workingDir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return p
}

func writeRoutingScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// installShims writes both provider shims for one run and stage. A shim records
// its argv, answers --version, writes the keyed plan artifact a real agent would
// write, and prints its adapter's minimal stream.
func (p *routingProbe) installShims(t *testing.T, runID, stageID string) {
	t.Helper()
	planDst := fmt.Sprintf("/tmp/fishhawk-plan-%s-%s.json", runID, stageID)
	t.Cleanup(func() { _ = os.Remove(planDst) })
	planSrc := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(planSrc, routingPlanJSON(t), 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	const codexStream = `{"type":"thread.started","thread_id":"t-1"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"planned"}}
{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1,"reasoning_output_tokens":0}}`
	for name, stream := range map[string]string{
		codexShimName:  codexStream,
		claudeShimName: `{"type":"result"}`,
	} {
		body := fmt.Sprintf(`#!/bin/sh
printf '%%s\0' "$@" > %q/%s.$$.argv
if [ "$1" = "--version" ]; then
  echo "%s-shim 9.9.9"
  exit 0
fi
cp %q %q
cat <<'STREAM'
%s
STREAM
`, p.recordDir, name, name, planSrc, planDst, stream)
		writeRoutingScript(t, filepath.Join(p.binDir, name), body)
	}
}

// records returns the recorded argv of every invocation of name.
func (p *routingProbe) records(t *testing.T, name string) [][]string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(p.recordDir, name+".*.argv"))
	if err != nil {
		t.Fatalf("glob records: %v", err)
	}
	out := make([][]string, 0, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read record: %v", err)
		}
		out = append(out, strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00"))
	}
	return out
}

// agentInvocations returns the provider's non-probe invocations.
func (p *routingProbe) agentInvocations(t *testing.T, name string) [][]string {
	t.Helper()
	var out [][]string
	for _, argv := range p.records(t, name) {
		if !slices.Equal(argv, []string{"--version"}) {
			out = append(out, argv)
		}
	}
	return out
}

// mcpEnv is the fishhawk-mcp environment, which the runner and shims inherit. It
// scrubs every FISHHAWK_* value, including the FISHHAWK_AGENT_BIN and
// FISHHAWK_CODEX_BIN pins, so a pin on the host cannot shadow a shim, and the CI
// markers, so the runner self-reports the local channel on every host.
func (p *routingProbe) mcpEnv(t *testing.T, backend *routingBackend) []string {
	t.Helper()
	env := make([]string, 0, len(os.Environ())+4)
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		switch {
		case strings.HasPrefix(name, "FISHHAWK_"):
			continue
		case slices.Contains([]string{"GITHUB_ACTIONS", "GITHUB_RUN_ID", "GITLAB_CI", "CI_PIPELINE_ID", "GH_TOKEN", "GITHUB_TOKEN"}, name):
			continue
		case name == "PATH":
			entry = "PATH=" + p.binDir + string(os.PathListSeparator) + value
		}
		env = append(env, entry)
	}
	return append(env,
		"FISHHAWK_API_TOKEN="+backend.operator,
		"FISHHAWK_BACKEND_URL="+backend.url,
		"FISHHAWK_RUNNER_BIN="+p.runnerPath,
	)
}

// connect starts the real fishhawk-mcp with the probe's environment. The stock
// connectMCPClient passes os.Environ through, which is what this test must scrub.
func (p *routingProbe) connect(t *testing.T, ctx context.Context, backend *routingBackend) *mcp.ClientSession {
	t.Helper()
	cmd := exec.Command(buildMCPBinary(t))
	cmd.Env = p.mcpEnv(t, backend)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "fishhawk-e2e-agent-routing", Version: "v0.0.1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect MCP client: %v\nstderr: %s", err, stderr.String())
	}
	t.Cleanup(func() {
		_ = session.Close()
		if t.Failed() && stderr.Len() > 0 {
			t.Logf("fishhawk-mcp stderr:\n%s", stderr.String())
		}
	})
	return session
}

// routingPlanJSON is a minimal schema-valid standard_v1 plan.
func routingPlanJSON(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"plan_version": "standard_v1",
		"ticket_reference": map[string]any{
			"type": "github_issue",
			"url":  "https://github.com/" + routingRepo + "/issues/5095",
			"id":   routingRepo + "#5095",
		},
		"generated_by": map[string]any{
			"agent":     "shim",
			"model":     routingModel,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		},
		"summary":                      "Executor agent routing fixture.",
		"scope":                        map[string]any{"files": []any{map[string]any{"path": "main.go", "operation": "create"}}},
		"approach":                     []any{map[string]any{"step": 1, "description": "Write the code."}},
		"verification":                 map[string]any{"test_strategy": "Run go test.", "rollback_plan": "Revert the commit."},
		"predicted_runtime_minutes":    5,
		"predicted_runtime_confidence": "high",
	})
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	return raw
}

// declaredOnStageSpec declares the provider, model and effort on the plan stage.
func declaredOnStageSpec(agent string) string {
	return fmt.Sprintf(`version: "2"
workflows:
  %s:
    stages:
      - id: plan
        type: plan
        executor:
          agent: %s
          model: %s
          reasoning_effort: %s
        produces:
          - artifact: plan
            schema: standard_v1
`, routingWorkflow, agent, routingModel, routingEffort)
}

// inheritedSpec declares them only in file-level defaults, the shape a raw YAML
// reader of the stage reads as empty.
func inheritedSpec(agent string) string {
	return fmt.Sprintf(`version: "2"
defaults:
  executor:
    agent: %s
    model: %s
    reasoning_effort: %s
workflows:
  %s:
    stages:
      - id: plan
        type: plan
        produces:
          - artifact: plan
            schema: standard_v1
`, agent, routingModel, routingEffort, routingWorkflow)
}

// startRunWithSpec admits a run through the real fishhawk_start_run and returns
// the run and the plan stage row that admission snapshotted from the spec.
func startRunWithSpec(t *testing.T, ctx context.Context, session *mcp.ClientSession, backend *routingBackend, workingDir, workflowSpec string) (uuid.UUID, *runpkg.Stage) {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_start_run",
		Arguments: map[string]any{
			"repo":           routingRepo,
			"workflow_id":    routingWorkflow,
			"workflow_spec":  workflowSpec,
			"trigger_source": "cli",
			"runner_kind":    "local",
			"working_dir":    workingDir,
		},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_start_run: %v", err)
	}
	if result.IsError {
		t.Fatalf("fishhawk_start_run error: %s", toolContentString(t, result))
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal start_run output: %v", err)
	}
	var out struct {
		Run struct {
			ID uuid.UUID `json:"id"`
		} `json:"run"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode start_run output: %v", err)
	}
	return out.Run.ID, planStageOf(t, ctx, backend, out.Run.ID)
}

func planStageOf(t *testing.T, ctx context.Context, backend *routingBackend, runID uuid.UUID) *runpkg.Stage {
	t.Helper()
	stages, err := backend.runRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		t.Fatalf("ListStages: %v", err)
	}
	for _, stage := range stages {
		if stage.Type == runpkg.StageTypePlan {
			return stage
		}
	}
	t.Fatalf("run %s has no plan stage", runID)
	return nil
}

// stageExecutorFromAPI reads the executor through the stage API, the surface the
// MCP verbs and `fishhawk runner start` read.
func stageExecutorFromAPI(t *testing.T, ctx context.Context, backend *routingBackend, runID, stageID uuid.UUID) (kind, ref string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, backend.url+"/v0/runs/"+runID.String()+"/stages", nil)
	if err != nil {
		t.Fatalf("build stages request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+backend.operator)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET stages: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET stages status %d: %s", resp.StatusCode, raw)
	}
	var list struct {
		Items []struct {
			ID       uuid.UUID `json:"id"`
			Executor struct {
				Kind string `json:"kind"`
				Ref  string `json:"ref"`
			} `json:"executor"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode stages: %v", err)
	}
	for _, item := range list.Items {
		if item.ID == stageID {
			return item.Executor.Kind, item.Executor.Ref
		}
	}
	t.Fatalf("stage %s missing from the stage API", stageID)
	return "", ""
}

// routingRunResult is the fishhawk_run_stage output this test reads.
type routingRunResult struct {
	ExitCode   int    `json:"exit_code"`
	StageState string `json:"stage_state"`
	Events     []struct {
		Payload map[string]any `json:"payload"`
	} `json:"events"`
}

func callRunStage(ctx context.Context, session *mcp.ClientSession, runID uuid.UUID, workingDir string) (*mcp.CallToolResult, error) {
	return session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_run_stage",
		Arguments: map[string]any{
			"run_id":           runID.String(),
			"workflow":         routingWorkflow,
			"stage":            "plan",
			"working_dir":      workingDir,
			"github_repo":      routingRepo,
			"base_branch":      "main",
			"push_and_open_pr": false,
		},
	})
}

// runPlanStage drives the plan stage to a settled result and requires a clean
// runner exit.
func runPlanStage(t *testing.T, ctx context.Context, session *mcp.ClientSession, runID uuid.UUID, workingDir string) routingRunResult {
	t.Helper()
	result, err := callRunStage(ctx, session, runID, workingDir)
	if err != nil {
		t.Fatalf("CallTool fishhawk_run_stage: %v", err)
	}
	if result.IsError {
		t.Fatalf("fishhawk_run_stage error: %s", toolContentString(t, result))
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal run_stage output: %v", err)
	}
	var out routingRunResult
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode run_stage output: %v", err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("runner exit_code = %d, want 0: %s", out.ExitCode, raw)
	}
	return out
}

// assertArgvPair asserts flag is immediately followed by value.
func assertArgvPair(t *testing.T, argv []string, flag, value string) {
	t.Helper()
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == value {
			return
		}
	}
	t.Errorf("argv missing %s %s: %q", flag, value, argv)
}

// assertRan asserts the whole provider chain for a run that executed under
// provider through shim: the runner's own argv, the single agent invocation, the
// other shim's silence, the runner_started provenance and the audited agent.
func assertRan(t *testing.T, ctx context.Context, backend *routingBackend, probe *routingProbe, out routingRunResult, runID uuid.UUID, provider, shim, otherShim string, wantRunnerFlag bool) []string {
	t.Helper()
	if out.StageState != string(runpkg.StageStateSucceeded) {
		t.Errorf("stage_state = %q, want %q", out.StageState, runpkg.StageStateSucceeded)
	}
	runners := probe.records(t, "runner")
	if len(runners) != 1 {
		t.Fatalf("runner invocations = %d, want 1: %q", len(runners), runners)
	}
	if wantRunnerFlag {
		assertArgvPair(t, runners[0], "--agent", provider)
	} else if slices.Contains(runners[0], "--agent") {
		t.Errorf("runner argv must omit --agent: %q", runners[0])
	}

	invocations := probe.agentInvocations(t, shim)
	if len(invocations) != 1 {
		t.Fatalf("%s shim agent invocations = %d, want 1: %q", shim, len(invocations), invocations)
	}
	if other := probe.records(t, otherShim); len(other) != 0 {
		t.Errorf("%s shim must never start, not even for --version: %q", otherShim, other)
	}

	started := false
	for _, event := range out.Events {
		if event.Payload["event"] != "runner_started" {
			continue
		}
		started = true
		if got := event.Payload["agent_kind"]; got != provider {
			t.Errorf("runner_started agent_kind = %v, want %s", got, provider)
		}
		if got := event.Payload["agent_binary"]; got != shim {
			t.Errorf("runner_started agent_binary = %v, want %s", got, shim)
		}
		if got, want := event.Payload["agent_version"], shim+"-shim 9.9.9"; got != want {
			t.Errorf("runner_started agent_version = %v, want %s", got, want)
		}
	}
	if !started {
		t.Errorf("tool result carries no runner_started event: %+v", out.Events)
	}

	payloads := backend.traceUploadedPayloads(t, ctx, runID)
	if len(payloads) == 0 {
		t.Fatal("no trace_uploaded audit entry was committed")
	}
	for _, payload := range payloads {
		if got := payload["agent"]; got != provider {
			t.Errorf("trace_uploaded agent = %v, want %s (variant %v)", got, provider, payload["variant"])
		}
	}
	return invocations[0]
}

func TestE2E_ExecutorAgentRouting(t *testing.T) {
	t.Parallel()
	backend := newRoutingBackend(t)
	realRunner := buildRoutingRunner(t)

	// runSpec admits a run for workflowSpec, asserts the row the stage API serves,
	// and drives the plan stage through the real verb.
	runSpec := func(t *testing.T, ctx context.Context, workflowSpec, wantRef string) (*routingProbe, routingRunResult, uuid.UUID) {
		t.Helper()
		probe := newRoutingProbe(t, realRunner)
		session := probe.connect(t, ctx, backend)
		runID, stage := startRunWithSpec(t, ctx, session, backend, probe.workingDir, workflowSpec)
		if kind, ref := stageExecutorFromAPI(t, ctx, backend, runID, stage.ID); kind != "agent" || ref != wantRef {
			t.Fatalf("stage API executor = %s/%q, want agent/%q", kind, ref, wantRef)
		}
		probe.installShims(t, runID.String(), stage.ID.String())
		return probe, runPlanStage(t, ctx, session, runID, probe.workingDir), runID
	}

	assertCodexDeclaredControls := func(t *testing.T, argv []string) {
		t.Helper()
		if len(argv) < 2 || argv[0] != "exec" || argv[1] != "--json" {
			t.Errorf("codex shim argv does not start with exec --json: %q", argv)
		}
		assertArgvPair(t, argv, "--model", routingModel)
		assertArgvPair(t, argv, "-c", "model_reasoning_effort="+routingEffort)
	}

	t.Run("codex_per_stage", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), timescale.D(2*time.Minute))
		defer cancel()
		probe, out, runID := runSpec(t, ctx, declaredOnStageSpec("codex"), "codex")
		argv := assertRan(t, ctx, backend, probe, out, runID, "codex", codexShimName, claudeShimName, true)
		assertCodexDeclaredControls(t, argv)
	})

	t.Run("codex_inherited_defaults", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), timescale.D(2*time.Minute))
		defer cancel()
		probe, out, runID := runSpec(t, ctx, inheritedSpec("codex"), "codex")
		argv := assertRan(t, ctx, backend, probe, out, runID, "codex", codexShimName, claudeShimName, true)
		assertCodexDeclaredControls(t, argv)
	})

	t.Run("claude_control", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), timescale.D(2*time.Minute))
		defer cancel()
		probe, out, runID := runSpec(t, ctx, declaredOnStageSpec("claude-code"), "claude-code")
		argv := assertRan(t, ctx, backend, probe, out, runID, "claude-code", claudeShimName, codexShimName, true)
		assertArgvPair(t, argv, "--model", routingModel)
		assertArgvPair(t, argv, "--effort", routingEffort)
	})

	t.Run("unset_default_control", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), timescale.D(2*time.Minute))
		defer cancel()
		probe := newRoutingProbe(t, realRunner)
		session := probe.connect(t, ctx, backend)
		// A spec cannot declare an empty agent, so this legacy row is seeded.
		requiresCharter := false
		seeded, err := backend.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
			Repo:            routingRepo,
			WorkflowID:      routingWorkflow,
			WorkflowSHA:     "deadbeef-empty-ref",
			TriggerSource:   runpkg.TriggerCLI,
			RunnerKind:      runpkg.RunnerKindLocal,
			RequiresCharter: &requiresCharter,
		})
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		stage, err := backend.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
			RunID:        seeded.ID,
			Sequence:     1,
			Type:         runpkg.StageTypePlan,
			ExecutorKind: runpkg.ExecutorAgent,
			ExecutorRef:  "",
		})
		if err != nil {
			t.Fatalf("CreateStage: %v", err)
		}
		if kind, ref := stageExecutorFromAPI(t, ctx, backend, seeded.ID, stage.ID); kind != "agent" || ref != "" {
			t.Fatalf("stage API executor = %s/%q, want agent with an empty ref", kind, ref)
		}
		probe.installShims(t, seeded.ID.String(), stage.ID.String())
		out := runPlanStage(t, ctx, session, seeded.ID, probe.workingDir)
		assertRan(t, ctx, backend, probe, out, seeded.ID, "claude-code", claudeShimName, codexShimName, false)
	})

	t.Run("unsupported_refused", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), timescale.D(2*time.Minute))
		defer cancel()
		const declared = "not-a-provider"
		probe := newRoutingProbe(t, realRunner)
		session := probe.connect(t, ctx, backend)
		runID, stage := startRunWithSpec(t, ctx, session, backend, probe.workingDir, declaredOnStageSpec(declared))
		probe.installShims(t, runID.String(), stage.ID.String())

		result, err := callRunStage(ctx, session, runID, probe.workingDir)
		if err != nil {
			t.Fatalf("CallTool fishhawk_run_stage: %v", err)
		}
		if !result.IsError {
			t.Fatalf("fishhawk_run_stage should refuse %q: %s", declared, toolContentString(t, result))
		}
		text := toolContentString(t, result)
		for _, want := range []string{"unsupported_executor_agent", declared, runID.String(), stage.ID.String(), "claude-code", "codex"} {
			if !strings.Contains(text, want) {
				t.Errorf("refusal should name %q: %s", want, text)
			}
		}

		reread, err := backend.runRepo.GetStage(ctx, stage.ID)
		if err != nil {
			t.Fatalf("GetStage: %v", err)
		}
		if reread.State != stage.State {
			t.Errorf("stage state moved from %q to %q on a refusal", stage.State, reread.State)
		}
		for _, name := range []string{"runner", codexShimName, claudeShimName} {
			if records := probe.records(t, name); len(records) != 0 {
				t.Errorf("%s must never start on a refusal: %q", name, records)
			}
		}
		if payloads := backend.traceUploadedPayloads(t, ctx, runID); len(payloads) != 0 {
			t.Errorf("a refusal must upload no trace: %+v", payloads)
		}
	})
}

// routingManifestBundle gzips one first-line record of the given kind and data.
func routingManifestBundle(t *testing.T, kind string, data any) []byte {
	t.Helper()
	line, err := json.Marshal(map[string]any{
		"seq":  0,
		"ts":   time.Now().UTC().Format(time.RFC3339),
		"kind": kind,
		"data": data,
	})
	if err != nil {
		t.Fatalf("marshal bundle line: %v", err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(append(line, '\n')); err != nil {
		t.Fatalf("gzip bundle: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// TestE2E_TraceUploaded_AgentClosedSet pins the audit field through real ingest:
// a signed upload is accepted identically for every manifest, and the additive
// agent key appears only for an exact known provider identifier.
func TestE2E_TraceUploaded_AgentClosedSet(t *testing.T) {
	t.Parallel()
	backend := newRoutingBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), timescale.D(2*time.Minute))
	defer cancel()

	manifest := func(fields map[string]any) func(*testing.T) []byte {
		return func(t *testing.T) []byte {
			data := map[string]any{"bundle_schema": "v1", "generated_at": time.Now().UTC().Format(time.RFC3339)}
			for key, value := range fields {
				data[key] = value
			}
			return routingManifestBundle(t, "manifest", data)
		}
	}

	tests := []struct {
		name      string
		bundle    func(*testing.T) []byte
		wantAgent string
	}{
		{name: "present_codex", bundle: manifest(map[string]any{"agent": "codex"}), wantAgent: "codex"},
		{name: "present_claude_code", bundle: manifest(map[string]any{"agent": "claude-code"}), wantAgent: "claude-code"},
		{name: "absent", bundle: manifest(nil)},
		{name: "empty", bundle: manifest(map[string]any{"agent": ""})},
		{name: "invalid", bundle: manifest(map[string]any{"agent": "not-a-provider"})},
		{name: "alias_is_not_an_identifier", bundle: manifest(map[string]any{"agent": "claudecode"})},
		{name: "case_variant", bundle: manifest(map[string]any{"agent": "Codex"})},
		{name: "surrounding_whitespace", bundle: manifest(map[string]any{"agent": " codex "})},
		{name: "oversized", bundle: manifest(map[string]any{"agent": strings.Repeat("codex", 20000)})},
		{name: "oversized_past_the_line_cap", bundle: manifest(map[string]any{"agent": strings.Repeat("codex", 1<<20)})},
		{name: "unreadable_wrong_type", bundle: manifest(map[string]any{"agent": 42})},
		{name: "unreadable_not_gzip", bundle: func(*testing.T) []byte { return []byte("not a gzip frame") }},
		{name: "unreadable_no_manifest_line", bundle: func(t *testing.T) []byte {
			return routingManifestBundle(t, "agent_event", map[string]any{"agent": "codex"})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requiresCharter := false
			seeded, err := backend.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
				Repo:            routingRepo,
				WorkflowID:      routingWorkflow,
				WorkflowSHA:     "deadbeef-trace-agent",
				TriggerSource:   runpkg.TriggerCLI,
				RunnerKind:      runpkg.RunnerKindLocal,
				RequiresCharter: &requiresCharter,
			})
			if err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			stage, err := backend.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
				RunID:        seeded.ID,
				Sequence:     1,
				Type:         runpkg.StageTypePlan,
				ExecutorKind: runpkg.ExecutorAgent,
				ExecutorRef:  "claude-code",
			})
			if err != nil {
				t.Fatalf("CreateStage: %v", err)
			}
			issued, err := backend.signing.Issue(ctx, seeded.ID, time.Hour)
			if err != nil {
				t.Fatalf("Issue signing key: %v", err)
			}

			body := tt.bundle(t)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost,
				backend.url+"/v0/runs/"+seeded.ID.String()+"/trace?variant=raw&stage_id="+stage.ID.String(),
				bytes.NewReader(body))
			if err != nil {
				t.Fatalf("build trace request: %v", err)
			}
			req.Header.Set("Authorization", "Bearer "+backend.operator)
			req.Header.Set("X-Fishhawk-Signature",
				hex.EncodeToString(ed25519.Sign(issued.PrivateKey, signing.ComputeMessage(body))))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("POST trace: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusAccepted {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("POST trace status %d, want 202 for every manifest: %s", resp.StatusCode, raw)
			}

			payloads := backend.traceUploadedPayloads(t, ctx, seeded.ID)
			if len(payloads) != 1 {
				t.Fatalf("trace_uploaded entries = %d, want 1", len(payloads))
			}
			got, present := payloads[0]["agent"]
			if tt.wantAgent == "" {
				if present {
					t.Errorf("trace_uploaded must omit agent, got %.80v", got)
				}
				return
			}
			if got != tt.wantAgent {
				t.Errorf("trace_uploaded agent = %v (present=%v), want %s", got, present, tt.wantAgent)
			}
		})
	}
}
