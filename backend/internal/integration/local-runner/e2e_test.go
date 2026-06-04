// Package localrunnere2e_test is the cross-component integration test
// for the local-runner plan-stage loop: real backend HTTP server →
// real Postgres → real fishhawk-runner binary → fake claude shim →
// assertions on stage state and plan artifact. Catches the two
// regression classes from #419 (missing --plan-out and pending→
// dispatched state-machine gap) that passed every prior unit-test gate.
package localrunnere2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
)

// localRunnerFixture wires every component the local-runner loop
// needs. Built once per test via newLocalRunnerFixture; teardown is
// handled via t.Cleanup.
type localRunnerFixture struct {
	url          string // httptest server URL
	pool         *pgxpool.Pool
	runRepo      runpkg.Repository
	artifactRepo artifact.Repository
	runnerBinary string // path to built fishhawk-runner
	fakeAgentDir string // dir containing the fake claude shim
}

// cannedPlanJSON returns a minimal schema-valid standard_v1 plan
// fixture marshalled to JSON. Pre-written to disk before the runner
// spawns; the runner reads and validates it after the fake agent exits.
func cannedPlanJSON(t *testing.T) []byte {
	t.Helper()
	m := map[string]any{
		"plan_version": "standard_v1",
		"ticket_reference": map[string]any{
			"type": "github_issue",
			"url":  "https://github.com/x/y/issues/1",
			"id":   "x/y#1",
		},
		"generated_by": map[string]any{
			"agent":     "claude-code",
			"model":     "claude-opus-4-7",
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		},
		"summary": "E2E integration test plan.",
		"scope": map[string]any{
			"files": []any{
				map[string]any{"path": "main.go", "operation": "create"},
			},
		},
		"approach": []any{
			map[string]any{"step": 1, "description": "Write the code."},
		},
		"verification": map[string]any{
			"test_strategy": "Run go test.",
			"rollback_plan": "Revert the commit.",
		},
		"predicted_runtime_minutes":    5,
		"predicted_runtime_confidence": "high",
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal canned plan: %v", err)
	}
	return b
}

func newLocalRunnerFixture(t *testing.T) *localRunnerFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// 1. Spin up Postgres in a throwaway container.
	c, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("fishhawk"),
		tcpostgres.WithUsername("fishhawk"),
		tcpostgres.WithPassword("fishhawk"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		if isDockerUnavailable(err) {
			t.Skipf("Docker not available; skipping local-runner E2E: %v", err)
		}
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutCancel()
		_ = c.Terminate(shutCtx)
	})
	pgURL, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres connection string: %v", err)
	}

	// 2. Apply migrations + open pool.
	if err := postgres.MigrateUp(pgURL); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	pool, err := pgxpool.New(ctx, pgURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// 3. Wire repos + backend server with an in-memory trace store.
	// No Orchestrator, no notifier, no GitHub integration — all nil-
	// guarded by the server. OIDCVerifier is nil so the signing-key
	// endpoint is open (v0 self-execution posture per server.go doc).
	runRepo := runpkg.NewPostgresRepository(pool)
	signingRepo := signing.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	artifactRepo := artifact.NewPostgresRepository(pool)
	srv := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      runRepo,
		SigningRepo:  signingRepo,
		AuditRepo:    auditRepo,
		ArtifactRepo: artifactRepo,
		TraceStore:   tracestore.NewMem(),
		// A no-op client (nil TokenProvider) satisfies the prompt
		// handler's non-nil GitHub guard. GetIssue is never reached for
		// CLI-triggered runs with no issue ref, so the nil tokens never
		// touch the wire.
		GitHub: githubclient.New(nil),
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	// 4. Build the real fishhawk-runner binary. A real binary rather
	// than `go run` keeps the runner's stdout clean for the trace
	// stream. Cold build is a few seconds; subsequent builds in the
	// same test process hit the build cache.
	binary := filepath.Join(t.TempDir(), runnerBinaryName())
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer buildCancel()
	build := exec.CommandContext(buildCtx, "go", "build",
		"-o", binary,
		"github.com/kuhlman-labs/fishhawk/runner/cmd/fishhawk-runner",
	)
	var buildStderr bytes.Buffer
	build.Stderr = &buildStderr
	if err := build.Run(); err != nil {
		t.Fatalf("build fishhawk-runner: %v\nstderr: %s", err, buildStderr.String())
	}

	// 5. Write the fake claude shim. The script prints one JSON line
	// and exits 0 — it does NOT write the plan file. The test pre-
	// writes the canned plan before spawning the runner so the runner
	// reads the pre-existing file after the fake agent exits.
	fakeDir := t.TempDir()
	fakeScript := filepath.Join(fakeDir, "claude")
	if runtime.GOOS == "windows" {
		fakeScript = filepath.Join(fakeDir, "claude.bat")
	}
	scriptBody := "#!/bin/sh\nprintf '{\"type\":\"result\"}\\n'\n"
	if err := os.WriteFile(fakeScript, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}

	return &localRunnerFixture{
		url:          httpSrv.URL,
		pool:         pool,
		runRepo:      runRepo,
		artifactRepo: artifactRepo,
		runnerBinary: binary,
		fakeAgentDir: fakeDir,
	}
}

func runnerBinaryName() string {
	if runtime.GOOS == "windows" {
		return "fishhawk-runner.exe"
	}
	return "fishhawk-runner"
}

// TestE2E_LocalRunner_PlanStage_HappyPath drives the full local-runner
// plan-stage loop end-to-end and asserts:
//
//   - The runner exits 0 (agent + upload chain succeeded).
//   - The stage transitions to awaiting_approval (pending→dispatched→
//     running→awaiting_approval via advanceStageAfterTrace, which is the
//     #419 state-machine fix under test).
//   - A plan artifact exists for the stage (the --plan-out upload path
//     fired, which was the second regression class from #419).
func TestE2E_LocalRunner_PlanStage_HappyPath(t *testing.T) {
	fx := newLocalRunnerFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Seed run with RunnerKind=local so advanceStageAfterTrace handles
	// the missing workflow_dispatch step (pending→dispatched).
	r, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef-e2e",
		TriggerSource: runpkg.TriggerCLI,
		RunnerKind:    runpkg.RunnerKindLocal,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Seed plan stage with RequiresApproval=true so the trace upload
	// drives the stage to awaiting_approval rather than succeeded.
	stage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            r.ID,
		Sequence:         1,
		Type:             runpkg.StageTypePlan,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "claude-code",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage: %v", err)
	}

	// Pre-write the canned plan. The runner reads this file after the
	// fake agent exits; the fake agent script does NOT write it.
	planPath := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(planPath, cannedPlanJSON(t), 0o600); err != nil {
		t.Fatalf("write canned plan: %v", err)
	}

	// Write a dummy prompt file.
	promptPath := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(promptPath, []byte("analyse and plan the work"), 0o600); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	// Spawn the runner. PATH is prepended with fakeAgentDir so the
	// runner's claudecode adapter resolves 'claude' to our shim.
	cmd := exec.CommandContext(ctx, fx.runnerBinary,
		"--run-id", r.ID.String(),
		"--backend-url", fx.url,
		"--workflow", "feature_change",
		"--stage", "plan",
		"--stage-id", stage.ID.String(),
		"--prompt-file", promptPath,
		"--plan-out", planPath,
		"--upload-trace",
	)
	// Replace PATH in the inherited environment so 'claude' resolves
	// to the shim; other vars (HOME, TMPDIR, etc.) are inherited as-is.
	runnerEnv := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "PATH=") {
			e = "PATH=" + fx.fakeAgentDir + ":" + strings.TrimPrefix(e, "PATH=")
		}
		runnerEnv = append(runnerEnv, e)
	}
	cmd.Env = runnerEnv

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		t.Fatalf("runner exited non-zero: %v\noutput:\n%s", err, out.String())
	}

	// Assert stage reached awaiting_approval. This is the load-bearing
	// assertion: it exercises the pending→dispatched gap fix from #419
	// (advanceStageAfterTrace lines 311-321) that the unit-test gate
	// could not catch.
	got, err := fx.runRepo.GetStage(ctx, stage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != runpkg.StageStateAwaitingApproval {
		t.Errorf("stage.State = %q, want %q\nrunner output:\n%s",
			got.State, runpkg.StageStateAwaitingApproval, out.String())
	}

	// Assert a plan artifact was uploaded. This exercises the
	// --plan-out upload path (the second regression class from #419).
	artifacts, err := fx.artifactRepo.ListForStage(ctx, stage.ID)
	if err != nil {
		t.Fatalf("ListForStage: %v", err)
	}
	if len(artifacts) == 0 {
		t.Errorf("no artifacts for stage %s; expected a plan artifact\nrunner output:\n%s",
			stage.ID, out.String())
	}
}

// implementTimeoutSpecYAML is a feature_change workflow with a 30m policy
// budget and no per-stage executor timeouts, so both stages resolve to the
// 30m policy max_stage_runtime (1800s).
const implementTimeoutSpecYAML = `version: "0.3"
workflows:
  feature_change:
    policy:
      max_stage_runtime: "30m"
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

// TestE2E_LocalRunner_ImplementTimeout_WidenedByPlan is the cross-layer
// guard for #523. It crosses the full seam that per-layer unit tests miss
// (cf. #618): a standard_v1 plan persisted through real Postgres → the
// server's dynamic implement-timeout computation → the prompt-fetch
// response payload that the runner consumes. The plan predicts 22 minutes,
// so the implement-stage agent_timeout_seconds must be widened to 22×2=44
// minutes (2640s) — above the 30m (1800s) spec budget — and the prompt-text
// "spec-resolved stage budget" hint must carry the SAME enlarged value, so
// the hint and the actual kill cap can't silently diverge.
func TestE2E_LocalRunner_ImplementTimeout_WidenedByPlan(t *testing.T) {
	fx := newLocalRunnerFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const predictedMinutes = 22
	const specBudgetSeconds = 1800
	wantTimeoutSeconds := predictedMinutes * 2 * 60 // 2640

	r, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef-impl-timeout",
		TriggerSource: runpkg.TriggerCLI,
		RunnerKind:    runpkg.RunnerKindLocal,
		WorkflowSpec:  []byte(implementTimeoutSpecYAML),
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	planStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:        r.ID,
		Sequence:     1,
		Type:         runpkg.StageTypePlan,
		ExecutorKind: runpkg.ExecutorAgent,
		ExecutorRef:  "claude-code",
	})
	if err != nil {
		t.Fatalf("CreateStage plan: %v", err)
	}

	// Persist the approved plan artifact under the plan stage. predicted
	// lands in the 20-25m calibration-tail band the issue (run 891ef85d)
	// motivated.
	planJSON := implementTimeoutPlanJSON(t, predictedMinutes)
	schema := "standard_v1"
	if _, err := fx.artifactRepo.Create(ctx, artifact.CreateParams{
		StageID:       planStage.ID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &schema,
		Content:       planJSON,
		ContentHash:   "impl-timeout-e2e",
	}); err != nil {
		t.Fatalf("Create plan artifact: %v", err)
	}

	implStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:        r.ID,
		Sequence:     2,
		Type:         runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent,
		ExecutorRef:  "claude-code",
	})
	if err != nil {
		t.Fatalf("CreateStage implement: %v", err)
	}

	// Fetch the implement-stage prompt over the real HTTP handler (the
	// SPA-readable render path needs no signature).
	url := fmt.Sprintf("%s/v0/stages/%s/prompt-render", fx.url, implStage.ID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("prompt-render request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("prompt-render status = %d:\n%s", resp.StatusCode, body)
	}

	var pr struct {
		Prompt              string `json:"prompt"`
		AgentTimeoutSeconds int    `json:"agent_timeout_seconds"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		t.Fatalf("decode prompt response: %v\n%s", err, body)
	}

	// Wire value: the kill cap reaching the runner is the widened value,
	// not the spec budget.
	if pr.AgentTimeoutSeconds != wantTimeoutSeconds {
		t.Errorf("agent_timeout_seconds = %d, want %d (predicted %dm × 2; spec budget was %ds)",
			pr.AgentTimeoutSeconds, wantTimeoutSeconds, predictedMinutes, specBudgetSeconds)
	}
	// Prompt-text hint: the "spec-resolved stage budget" the agent is told
	// must equal the wire kill cap (agent_timeout_seconds / 60), so the two
	// can't diverge.
	wantBudgetText := fmt.Sprintf("The spec-resolved stage budget is **%d minutes**.", pr.AgentTimeoutSeconds/60)
	if !strings.Contains(pr.Prompt, wantBudgetText) {
		t.Errorf("prompt missing budget hint %q (must match wire timeout %ds)\n---\n%s",
			wantBudgetText, pr.AgentTimeoutSeconds, pr.Prompt)
	}
}

// TestE2E_LocalRunner_AppBotIdentity_ResolvedAndCarriedOnPrompt is the
// backend half of the #722 cross-boundary seam: a stubbed GitHub App slug
// (GET /app) + bot user-id (GET /users/<slug>[bot]) resolves on the backend
// through the REAL githubclient (App-JWT auth against a fake GitHub), is
// composed into the bot commit identity, and is carried on the implement
// prompt-fetch response. The runner-consumer→git-author half of the seam is
// covered in the runner module (gitops.TestCommitAndPush_AppBotAuthorIdentity
// + the runner's CommitAndPush threading test) — the backend module must not
// depend on the runner.
//
// The email format assertion pins the exact GitHub convention
// `<bot-user-id>+<slug>[bot]@users.noreply.github.com`, the contract that
// makes GitHub attribute the commit to the App's bot account.
func TestE2E_LocalRunner_AppBotIdentity_ResolvedAndCarriedOnPrompt(t *testing.T) {
	fx := newLocalRunnerFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const (
		appSlug   = "fishhawk"
		botUserID = 41898282
		wantName  = "fishhawk[bot]"
		wantEmail = "41898282+fishhawk[bot]@users.noreply.github.com"
	)

	// Fake GitHub: serves the two App-level endpoints resolveAppBotIdentity
	// reads, asserting both carry the App JWT as Bearer auth.
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-app-jwt" {
			t.Errorf("Authorization = %q, want App JWT bearer", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app":
			_, _ = io.WriteString(w, `{"slug":"`+appSlug+`"}`)
		case "/users/" + appSlug + "[bot]":
			_, _ = fmt.Fprintf(w, `{"id":%d,"login":"%s[bot]"}`, botUserID, appSlug)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(gh.Close)

	// Backend server wired with a real githubclient pointed at the fake
	// GitHub, with AppJWT supplied so the App-level endpoints authenticate.
	ghClient := githubclient.New(nil)
	ghClient.BaseURL = gh.URL
	ghClient.AppJWT = func() (string, error) { return "test-app-jwt", nil }
	srv := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      fx.runRepo,
		ArtifactRepo: fx.artifactRepo,
		GitHub:       ghClient,
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	// CLI-triggered run (no issue ref → GetIssue never reached) with a plan
	// stage + approved plan + implement stage.
	r, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef-appbot",
		TriggerSource: runpkg.TriggerCLI,
		RunnerKind:    runpkg.RunnerKindLocal,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	planStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: r.ID, Sequence: 1, Type: runpkg.StageTypePlan,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("CreateStage plan: %v", err)
	}
	schema := "standard_v1"
	if _, err := fx.artifactRepo.Create(ctx, artifact.CreateParams{
		StageID:       planStage.ID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &schema,
		Content:       cannedPlanJSON(t),
		ContentHash:   "appbot-e2e",
	}); err != nil {
		t.Fatalf("Create plan artifact: %v", err)
	}
	implStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: r.ID, Sequence: 2, Type: runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("CreateStage implement: %v", err)
	}

	// Fetch the implement-stage prompt over the real HTTP handler.
	url := fmt.Sprintf("%s/v0/stages/%s/prompt-render", httpSrv.URL, implStage.ID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("prompt-render request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("prompt-render status = %d:\n%s", resp.StatusCode, body)
	}

	var pr struct {
		CommitAuthorName  string `json:"commit_author_name"`
		CommitAuthorEmail string `json:"commit_author_email"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		t.Fatalf("decode prompt response: %v\n%s", err, body)
	}
	if pr.CommitAuthorName != wantName {
		t.Errorf("commit_author_name = %q, want %q", pr.CommitAuthorName, wantName)
	}
	if pr.CommitAuthorEmail != wantEmail {
		t.Errorf("commit_author_email = %q, want %q (exact GitHub bot email convention)",
			pr.CommitAuthorEmail, wantEmail)
	}
}

// implementTimeoutPlanJSON returns a schema-valid standard_v1 plan with the
// given predicted_runtime_minutes, marshalled to JSON for artifact storage.
func implementTimeoutPlanJSON(t *testing.T, predictedMinutes int) []byte {
	t.Helper()
	m := map[string]any{
		"plan_version": "standard_v1",
		"ticket_reference": map[string]any{
			"type": "github_issue",
			"url":  "https://github.com/x/y/issues/1",
			"id":   "x/y#1",
		},
		"generated_by": map[string]any{
			"agent":     "claude-code",
			"model":     "claude-opus-4-7",
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		},
		"summary": "Implement-timeout E2E plan.",
		"scope": map[string]any{
			"files": []any{
				map[string]any{"path": "main.go", "operation": "create"},
			},
		},
		"approach": []any{
			map[string]any{"step": 1, "description": "Write the code."},
		},
		"verification": map[string]any{
			"test_strategy": "Run go test.",
			"rollback_plan": "Revert the commit.",
		},
		"predicted_runtime_minutes":    predictedMinutes,
		"predicted_runtime_confidence": "medium",
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	return b
}

// isDockerUnavailable matches the guard pattern from
// backend/internal/integration/mcp/e2e_test.go. FISHHAWK_SKIP_INTEGRATION
// provides an explicit escape hatch for CI environments without Docker.
func isDockerUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if os.Getenv("FISHHAWK_SKIP_INTEGRATION") != "" {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"cannot connect to the docker daemon",
		"docker: not found",
		"executable file not found",
		"dial unix /var/run/docker.sock",
	} {
		if strings.Contains(msg, strings.ToLower(marker)) {
			return true
		}
	}
	return errors.Is(err, exec.ErrNotFound)
}
