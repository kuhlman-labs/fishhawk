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
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
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
	auditRepo    audit.Repository
	traceStore   *tracestore.MemStorage
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
	return newLocalRunnerFixtureWithTokenProvider(t, false)
}

// newLocalRunnerFixtureWithTokenProvider is newLocalRunnerFixture with the
// installation-token endpoint's GitHubTokens provider optionally wired.
//
// Default (false): the endpoint 503s installation_token_unconfigured, which
// the runner surfaces as a generic token-fetch failure WITHOUT consulting the
// gh-CLI fallback (#713). The push-failure tests depend on this: it is what
// keeps them hermetic on a developer machine, where `gh auth token` can
// resolve real keychain credentials even with GH_CONFIG_DIR pointed at an
// empty dir — wiring the provider fixture-wide once routed those tests
// through the fallback and pushed real branches to the real repo.
//
// withTokenProvider=true: runs without an InstallationID get the documented
// 400 no_installation_for_run (→ the runner's ErrNoInstallation → gh-CLI
// fallback). Only safe for tests that ALSO shim `gh` on PATH so the fallback
// can never reach real credentials.
func newLocalRunnerFixtureWithTokenProvider(t *testing.T, withTokenProvider bool) *localRunnerFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// 1. Get a freshly-migrated per-test database on the single
	// shared Postgres container (pgtest starts one container for the
	// whole run and clones a per-test DB from the migrated template;
	// #1174). Skips when Docker is unavailable.
	pgURL := pgtest.NewURL(t)

	// 2. Open the pool — pgtest already migrated the per-test DB.
	pool, err := postgres.Connect(ctx, pgURL)
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
	traceStore := tracestore.NewMem()
	srvCfg := server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      runRepo,
		SigningRepo:  signingRepo,
		AuditRepo:    auditRepo,
		ArtifactRepo: artifactRepo,
		TraceStore:   traceStore,
		// A no-op client (nil TokenProvider) satisfies the prompt
		// handler's non-nil GitHub guard. GetIssue is never reached for
		// CLI-triggered runs with no issue ref, so the nil tokens never
		// touch the wire.
		GitHub: githubclient.New(nil),
	}
	if withTokenProvider {
		// A never-called token provider satisfies the installation-token
		// endpoint's configured guard so runs WITHOUT an installation get
		// the documented 400 no_installation_for_run. No fixture run
		// carries an InstallationID, so the no-installation check always
		// fires before Token could be reached. See the function doc for
		// why this is opt-in.
		srvCfg.GitHubTokens = unreachableTokenProvider{}
	}
	srv := server.New(srvCfg)
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
		auditRepo:    auditRepo,
		traceStore:   traceStore,
		runnerBinary: binary,
		fakeAgentDir: fakeDir,
	}
}

// unreachableTokenProvider satisfies githubapp.TokenProvider for the
// installation-token endpoint's configured guard. The fixture never stamps
// an InstallationID on a run, so the handler's no_installation_for_run check
// rejects every request before Token is reached.
type unreachableTokenProvider struct{}

func (unreachableTokenProvider) Token(context.Context, int64) (string, error) {
	return "", errors.New("unreachable: no fixture run carries an installation_id")
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

	// Fake GitHub: serves the two endpoints resolveAppBotIdentity reads,
	// asserting auth PER PATH — the App-level /app endpoint must carry the
	// App JWT, while the public /users/{login} lookup must carry NO auth
	// header (the App JWT is only valid for /app* endpoints; routing it to
	// /users 401s — that was the #750 bug this test now guards against).
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app":
			if got := r.Header.Get("Authorization"); got != "Bearer test-app-jwt" {
				t.Errorf("/app Authorization = %q, want App JWT bearer", got)
			}
			_, _ = io.WriteString(w, `{"slug":"`+appSlug+`"}`)
		case "/users/" + appSlug + "[bot]":
			if got := r.Header.Get("Authorization"); got != "" {
				t.Errorf("/users Authorization = %q, want NO auth header (public lookup, #750)", got)
			}
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

// TestE2E_LocalRunner_ImplementPRFailure_NoZombie reproduces the exact
// b6811dc9 zombie shape (#742) end-to-end through the real runner against
// the real backend: an implement stage whose commit/push/PR-open step
// FAILS after policy passes must land the stage `failed` (category C,
// retryable) — it must NEVER be observed at review:awaiting_approval with
// pull_request_url=null.
//
// The forced failure is hermetic: the run has no GitHub App installation
// and the gh-CLI push fallback (#713) is denied an auth token (empty
// GH_CONFIG_DIR + empty token env), so the PR-open step cannot
// authenticate. Because the trace bundle carries a non-empty scope-bounded
// diff, the push-and-open-pr gate leaves the implement stage in `running`
// until the runner reports the failure to /pull-request — without the #742
// fix the trace upload would have advanced the stage to awaiting_approval
// before the PR-open step ran, stranding the run with a null PR.
func TestE2E_LocalRunner_ImplementPRFailure_NoZombie(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available; skipping PR-failure E2E")
	}
	fx := newLocalRunnerFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	r, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef-prfail",
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
		Content:       scopedPlanJSON(t, "added.txt"),
		ContentHash:   "prfail-e2e",
	}); err != nil {
		t.Fatalf("Create plan artifact: %v", err)
	}

	implStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: r.ID, Sequence: 2, Type: runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "claude-code", RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage implement: %v", err)
	}

	// Working tree: a git repo on `main` with one base commit, so the
	// implement stage's scope-bounded diff (added.txt) is non-empty — the
	// signal that fires the push-and-open-pr gate.
	workDir := t.TempDir()
	gitRepoInit(t, workDir)

	// Fake agent: writes the scoped file (added.txt) into the working tree.
	fakeDir := t.TempDir()
	fakeScript := filepath.Join(fakeDir, "claude")
	if runtime.GOOS == "windows" {
		fakeScript = filepath.Join(fakeDir, "claude.bat")
	}
	scriptBody := "#!/bin/sh\nprintf '{\"type\":\"result\"}\\n'\necho 'package main' > added.txt\n"
	if err := os.WriteFile(fakeScript, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}

	// Empty gh config dir + empty token env so the gh-CLI push fallback
	// (#713) deterministically fails — the PR-open step cannot authenticate.
	ghConfigDir := t.TempDir()

	cmd := exec.CommandContext(ctx, fx.runnerBinary,
		"--run-id", r.ID.String(),
		"--backend-url", fx.url,
		"--workflow", "feature_change",
		"--stage", "implement",
		"--stage-id", implStage.ID.String(),
		"--fetch-prompt",
		"--upload-trace",
		"--working-dir", workDir,
		"--check-base-ref", "main",
		"--github-repo", "kuhlman-labs/fishhawk",
		"--base-branch", "main",
	)
	runnerEnv := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		switch {
		case strings.HasPrefix(e, "PATH="):
			e = "PATH=" + fakeDir + ":" + strings.TrimPrefix(e, "PATH=")
		case strings.HasPrefix(e, "GH_TOKEN="), strings.HasPrefix(e, "GITHUB_TOKEN="), strings.HasPrefix(e, "GH_CONFIG_DIR="):
			continue
		}
		runnerEnv = append(runnerEnv, e)
	}
	runnerEnv = append(runnerEnv, "GH_TOKEN=", "GITHUB_TOKEN=", "GH_CONFIG_DIR="+ghConfigDir)
	cmd.Env = runnerEnv

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	// The runner is EXPECTED to exit non-zero: the PR-open step fails.
	if err := cmd.Run(); err == nil {
		t.Fatalf("runner exited 0; expected failure on the PR-open step\noutput:\n%s", out.String())
	}

	// The implement stage must land FAILED with category C — never
	// awaiting_approval (the zombie shape from run b6811dc9).
	got, err := fx.runRepo.GetStage(ctx, implStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != runpkg.StageStateFailed {
		t.Fatalf("implement stage State = %q, want failed\nrunner output:\n%s", got.State, out.String())
	}
	if got.FailureCategory == nil || *got.FailureCategory != runpkg.FailureC {
		t.Errorf("FailureCategory = %v, want C (retryable)\nrunner output:\n%s", got.FailureCategory, out.String())
	}
	// Category C is retryable: the failed → pending redrive must be allowed.
	if !runpkg.ValidStageRetryTransition(got.State, runpkg.StageStatePending) {
		t.Errorf("category-C failed stage should permit retry (failed → pending)")
	}
	// The zombie shape is impossible: no PR was opened, so the run carries
	// no pull_request_url.
	gotRun, err := fx.runRepo.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if gotRun.PullRequestURL != nil {
		t.Errorf("run.PullRequestURL = %q, want nil after a PR-open failure", *gotRun.PullRequestURL)
	}
}

// TestE2E_LocalRunner_DecomposedChildPushFailure_NoZombie is the
// decomposition-child analogue of TestE2E_LocalRunner_ImplementPRFailure_NoZombie
// (#771). A decomposed child opens no PR — it commits + pushes onto the shared
// parent branch — but the same zombie hole applied: before #771 the child's
// implement stage advanced to its terminal state at trace-upload time, BEFORE
// the push, so a push failure left the child run terminal-succeeded with no
// code on the shared branch and the childcompletion sweeper would consolidate
// the parent into a PR silently missing that child's work.
//
// This drives a real decomposed-child implement stage (run.DecomposedFrom set,
// so the prompt stamps decomposed_from_run_id and the runner stamps
// push_to_shared_branch) whose push step fails hermetically (no App
// installation + denied gh-CLI token, exactly as the #742 e2e). It asserts:
//
//	(a) the child implement stage ends `failed` (category C, retryable) — NOT
//	    succeeded, and the failed → pending retry transition is permitted;
//	(b) the child run is not succeeded — no code-bearing succeeded child is
//	    visible to the parent / the childcompletion sweeper.
//
// (a) and (b) are the load-bearing assertions: they catch the zombie at its
// source (the child stage/run never reaches a terminal succeeded state on a
// push failure). The downstream "parent does not consolidate a PR missing the
// child" property follows transitively — the childcompletion sweeper only
// consolidates a parent once every child RUN is terminal AND succeeded
// (sweeper.go resolveParent), which a non-succeeded child (b) precludes. That
// sweeper is not wired into this fixture, so this test does not re-assert the
// parent state (an earlier (c) assertion on parent.PullRequestURL/State was
// vacuous here and was removed).
func TestE2E_LocalRunner_DecomposedChildPushFailure_NoZombie(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available; skipping decomposed-child push-failure E2E")
	}
	fx := newLocalRunnerFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Parent run: the decomposition root the child is minted from.
	parent, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef-childpush-parent",
		TriggerSource: runpkg.TriggerCLI,
		RunnerKind:    runpkg.RunnerKindLocal,
	})
	if err != nil {
		t.Fatalf("CreateRun parent: %v", err)
	}

	// Child run: DecomposedFrom = parent.ID, so the prompt response stamps
	// decomposed_from_run_id and the runner takes the push-to-shared-branch
	// path (push_to_shared_branch manifest flag, no PR open).
	child, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:           "kuhlman-labs/fishhawk",
		WorkflowID:     "feature_change",
		WorkflowSHA:    "deadbeef-childpush-child",
		TriggerSource:  runpkg.TriggerCLI,
		RunnerKind:     runpkg.RunnerKindLocal,
		DecomposedFrom: &parent.ID,
	})
	if err != nil {
		t.Fatalf("CreateRun child: %v", err)
	}

	planStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: child.ID, Sequence: 1, Type: runpkg.StageTypePlan,
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
		Content:       scopedPlanJSON(t, "added.txt"),
		ContentHash:   "childpush-e2e",
	}); err != nil {
		t.Fatalf("Create plan artifact: %v", err)
	}

	implStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: child.ID, Sequence: 2, Type: runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "claude-code", RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage implement: %v", err)
	}

	// Working tree: a git repo on `main` with one base commit, so the child's
	// scope-bounded diff (added.txt) is non-empty and there is work to push.
	// (The push_to_shared_branch gate itself fires on the manifest flag alone
	// since #4079; the absent-diff variant is the test below.)
	workDir := t.TempDir()
	gitRepoInit(t, workDir)

	fakeDir := t.TempDir()
	fakeScript := filepath.Join(fakeDir, "claude")
	if runtime.GOOS == "windows" {
		fakeScript = filepath.Join(fakeDir, "claude.bat")
	}
	scriptBody := "#!/bin/sh\nprintf '{\"type\":\"result\"}\\n'\necho 'package main' > added.txt\n"
	if err := os.WriteFile(fakeScript, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}

	// Deny the gh-CLI push fallback (#713) so the push step cannot
	// authenticate — the hermetic forced failure.
	ghConfigDir := t.TempDir()

	cmd := exec.CommandContext(ctx, fx.runnerBinary,
		"--run-id", child.ID.String(),
		"--backend-url", fx.url,
		"--workflow", "feature_change",
		"--stage", "implement",
		"--stage-id", implStage.ID.String(),
		"--fetch-prompt",
		"--upload-trace",
		"--working-dir", workDir,
		"--check-base-ref", "main",
		"--github-repo", "kuhlman-labs/fishhawk",
		"--base-branch", "main",
	)
	runnerEnv := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		switch {
		case strings.HasPrefix(e, "PATH="):
			e = "PATH=" + fakeDir + ":" + strings.TrimPrefix(e, "PATH=")
		case strings.HasPrefix(e, "GH_TOKEN="), strings.HasPrefix(e, "GITHUB_TOKEN="), strings.HasPrefix(e, "GH_CONFIG_DIR="):
			continue
		}
		runnerEnv = append(runnerEnv, e)
	}
	runnerEnv = append(runnerEnv, "GH_TOKEN=", "GITHUB_TOKEN=", "GH_CONFIG_DIR="+ghConfigDir)
	cmd.Env = runnerEnv

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	// The runner is EXPECTED to exit non-zero: the child push step fails.
	if err := cmd.Run(); err == nil {
		t.Fatalf("runner exited 0; expected failure on the child push step\noutput:\n%s", out.String())
	}

	// (a) The child implement stage must land FAILED with category C — never
	// the terminal succeeded zombie.
	got, err := fx.runRepo.GetStage(ctx, implStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != runpkg.StageStateFailed {
		t.Fatalf("child implement stage State = %q, want failed\nrunner output:\n%s", got.State, out.String())
	}
	if got.State == runpkg.StageStateSucceeded {
		t.Error("child implement stage reached succeeded on a push failure (the zombie shape)")
	}
	if got.FailureCategory == nil || *got.FailureCategory != runpkg.FailureC {
		t.Errorf("FailureCategory = %v, want C (retryable)\nrunner output:\n%s", got.FailureCategory, out.String())
	}
	if !runpkg.ValidStageRetryTransition(got.State, runpkg.StageStatePending) {
		t.Errorf("category-C failed stage should permit retry (failed → pending)")
	}

	// (b) The child run is not succeeded — no code-bearing succeeded child is
	// visible to the parent / the childcompletion sweeper.
	gotChild, err := fx.runRepo.GetRun(ctx, child.ID)
	if err != nil {
		t.Fatalf("GetRun child: %v", err)
	}
	if gotChild.State == runpkg.StateSucceeded {
		t.Errorf("child run State = succeeded, want non-succeeded after a push failure")
	}
	// The parent-does-not-consolidate-a-missing-child property follows from (b):
	// see the function doc — the sweeper only consolidates terminal-succeeded
	// children and is not wired into this fixture, so asserting parent state
	// here would be vacuous.
}

// TestE2E_LocalRunner_DecomposedChildPushFailure_AbsentDiff_FailsC reproduces
// the #4079 incident end to end on the real runner binary and the real trace +
// /pull-request handlers. It is the NoZombie test above with the three changes
// that make the incident's shape:
//
//   - the child implement stage is GATELESS (RequiresApproval=false). This is
//     load-bearing: an awaiting_approval terminal would MASK the bug, because
//     awaiting_approval → failed is a legal edge, whereas succeeded is terminal
//     and the later failed-C report's FailStage is refused;
//   - --check-base-ref names a ref that never exists, so the runner's diff
//     capture fails (diff_failed policy_event) and the trace bundle carries NO
//     git_diff event — the stage 0afdfa73 bundle;
//   - the fake agent still writes added.txt, so there IS work to push.
//
// The push fails hermetically (the gh-CLI token fallback is denied, as in
// NoZombie). Before #4079 the trace handler's child-push gate required a
// non-empty diff, so the absent-diff trace moved the stage straight to
// succeeded at trace time and the runner's pull_request_failed report was
// refused; the child read succeeded while its branch was never pushed.
func TestE2E_LocalRunner_DecomposedChildPushFailure_AbsentDiff_FailsC(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available; skipping decomposed-child absent-diff push-failure E2E")
	}
	fx := newLocalRunnerFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	parent, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef-childabsentdiff-parent",
		TriggerSource: runpkg.TriggerCLI,
		RunnerKind:    runpkg.RunnerKindLocal,
	})
	if err != nil {
		t.Fatalf("CreateRun parent: %v", err)
	}
	child, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:           "kuhlman-labs/fishhawk",
		WorkflowID:     "feature_change",
		WorkflowSHA:    "deadbeef-childabsentdiff-child",
		TriggerSource:  runpkg.TriggerCLI,
		RunnerKind:     runpkg.RunnerKindLocal,
		DecomposedFrom: &parent.ID,
	})
	if err != nil {
		t.Fatalf("CreateRun child: %v", err)
	}

	planStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: child.ID, Sequence: 1, Type: runpkg.StageTypePlan,
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
		Content:       scopedPlanJSON(t, "added.txt"),
		ContentHash:   "childabsentdiff-e2e",
	}); err != nil {
		t.Fatalf("Create plan artifact: %v", err)
	}

	// GATELESS — the incident's shape (see the function doc).
	implStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: child.ID, Sequence: 2, Type: runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "claude-code", RequiresApproval: false,
	})
	if err != nil {
		t.Fatalf("CreateStage implement: %v", err)
	}

	workDir := t.TempDir()
	gitRepoInit(t, workDir)

	fakeDir := t.TempDir()
	fakeScript := filepath.Join(fakeDir, "claude")
	if runtime.GOOS == "windows" {
		fakeScript = filepath.Join(fakeDir, "claude.bat")
	}
	scriptBody := "#!/bin/sh\nprintf '{\"type\":\"result\"}\\n'\necho 'package main' > added.txt\n"
	if err := os.WriteFile(fakeScript, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}

	// Deny the gh-CLI push fallback (#713): the hermetic forced push failure.
	ghConfigDir := t.TempDir()

	cmd := exec.CommandContext(ctx, fx.runnerBinary,
		"--run-id", child.ID.String(),
		"--backend-url", fx.url,
		"--workflow", "feature_change",
		"--stage", "implement",
		"--stage-id", implStage.ID.String(),
		"--fetch-prompt",
		"--upload-trace",
		"--working-dir", workDir,
		// A base ref that never resolves: the incident's
		// fishhawk/run-<parent>-consolidated was absent locally.
		"--check-base-ref", "fishhawk/run-deadbeef-consolidated",
		"--github-repo", "kuhlman-labs/fishhawk",
		"--base-branch", "main",
	)
	runnerEnv := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		switch {
		case strings.HasPrefix(e, "PATH="):
			e = "PATH=" + fakeDir + ":" + strings.TrimPrefix(e, "PATH=")
		case strings.HasPrefix(e, "GH_TOKEN="), strings.HasPrefix(e, "GITHUB_TOKEN="), strings.HasPrefix(e, "GH_CONFIG_DIR="):
			continue
		}
		runnerEnv = append(runnerEnv, e)
	}
	runnerEnv = append(runnerEnv, "GH_TOKEN=", "GITHUB_TOKEN=", "GH_CONFIG_DIR="+ghConfigDir)
	cmd.Env = runnerEnv

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err == nil {
		t.Fatalf("runner exited 0; expected failure on the child push step\noutput:\n%s", out.String())
	}

	// The incident precondition actually held: the diff capture failed, so
	// the bundle the trace handler received carries a diff_failed
	// policy_event and NO git_diff event. Read back through the real stage
	// trace endpoint; without this the test would re-cover the non-empty-diff
	// case NoZombie already pins.
	traceResp, err := http.Get(fx.url + "/v0/stages/" + implStage.ID.String() + "/trace")
	if err != nil {
		t.Fatalf("GET stage trace: %v", err)
	}
	traceBody, err := io.ReadAll(traceResp.Body)
	_ = traceResp.Body.Close()
	if err != nil {
		t.Fatalf("read stage trace: %v", err)
	}
	if traceResp.StatusCode != http.StatusOK {
		t.Fatalf("GET stage trace status = %d, want 200:\n%s", traceResp.StatusCode, traceBody)
	}
	if !bytes.Contains(traceBody, []byte(`diff_failed`)) {
		t.Fatalf("shipped bundle carries no diff_failed policy event; the fixture did not reproduce the absent-diff shape\nbundle:\n%s\nrunner output:\n%s", traceBody, out.String())
	}
	if bytes.Contains(traceBody, []byte(`"kind":"git_diff"`)) {
		t.Fatalf("shipped bundle carries a git_diff event; want none (the #4079 absent-diff shape)\nbundle:\n%s", traceBody)
	}

	got, err := fx.runRepo.GetStage(ctx, implStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State == runpkg.StageStateSucceeded {
		t.Fatalf("child implement stage reached succeeded with its push failed (the #4079 shape)\nrunner output:\n%s", out.String())
	}
	if got.State != runpkg.StageStateFailed {
		t.Fatalf("child implement stage State = %q, want failed\nrunner output:\n%s", got.State, out.String())
	}
	if got.FailureCategory == nil || *got.FailureCategory != runpkg.FailureC {
		t.Errorf("FailureCategory = %v, want C (retryable)\nrunner output:\n%s", got.FailureCategory, out.String())
	}

	gotChild, err := fx.runRepo.GetRun(ctx, child.ID)
	if err != nil {
		t.Fatalf("GetRun child: %v", err)
	}
	if gotChild.State == runpkg.StateSucceeded {
		t.Errorf("child run State = succeeded, want non-succeeded after a push failure")
	}
}

// TestE2E_LocalRunner_DecomposedChildNoChanges_TerminalizesFailedC is the
// #1036 no-changes cross-boundary test: a decomposed child whose agent
// produces ZERO edits used to bare-return from the runner's NoChanges
// short-circuit, leaving the implement stage wedged in `running` — the
// push_to_shared_branch trace gate (#771) defers the terminal transition to
// a child-push report that never came. The runner now reports
// {outcome:"failed", category:"C"} and the backend's existing failed-outcome
// handler drives the stage terminal. This drives a real decomposed-child
// implement stage whose fake agent writes nothing and asserts the stage
// lands `failed` category C (retryable) end to end — runner report wire →
// backend handler → stage state machine — rather than wedged in `running`.
//
// Unlike the push-failure test above, the gh-CLI token fallback (#713) is
// SATISFIED via a fake `gh` shim: the runner must get past the token step to
// reach CommitAndPush's NoChanges short-circuit, which probes the working
// tree (`status --porcelain`) before any remote access, so the remote-less
// temp repo never sees a network call.
func TestE2E_LocalRunner_DecomposedChildNoChanges_TerminalizesFailedC(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available; skipping decomposed-child no-changes E2E")
	}
	// Token provider wired so the run gets 400 no_installation_for_run and
	// the runner takes the gh-CLI fallback — satisfied below by a fake `gh`
	// shim, never the operator's real credentials.
	fx := newLocalRunnerFixtureWithTokenProvider(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	parent, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef-childnochanges-parent",
		TriggerSource: runpkg.TriggerCLI,
		RunnerKind:    runpkg.RunnerKindLocal,
	})
	if err != nil {
		t.Fatalf("CreateRun parent: %v", err)
	}

	child, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:           "kuhlman-labs/fishhawk",
		WorkflowID:     "feature_change",
		WorkflowSHA:    "deadbeef-childnochanges-child",
		TriggerSource:  runpkg.TriggerCLI,
		RunnerKind:     runpkg.RunnerKindLocal,
		DecomposedFrom: &parent.ID,
	})
	if err != nil {
		t.Fatalf("CreateRun child: %v", err)
	}

	planStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: child.ID, Sequence: 1, Type: runpkg.StageTypePlan,
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
		Content:       scopedPlanJSON(t, "added.txt"),
		ContentHash:   "childnochanges-e2e",
	}); err != nil {
		t.Fatalf("Create plan artifact: %v", err)
	}

	implStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: child.ID, Sequence: 2, Type: runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "claude-code", RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage implement: %v", err)
	}

	// Working tree: a git repo on `main` with one base commit. The fake agent
	// writes NOTHING, so the tree stays clean — the no-changes shape.
	workDir := t.TempDir()
	gitRepoInit(t, workDir)

	fakeDir := t.TempDir()
	fakeScript := filepath.Join(fakeDir, "claude")
	if runtime.GOOS == "windows" {
		fakeScript = filepath.Join(fakeDir, "claude.bat")
	}
	scriptBody := "#!/bin/sh\nprintf '{\"type\":\"result\"}\\n'\n"
	if err := os.WriteFile(fakeScript, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	// Fake gh shim: the gh-CLI token fallback must SUCCEED so the runner
	// reaches CommitAndPush (which short-circuits NoChanges before any
	// remote access — the canned token never touches the wire).
	fakeGh := filepath.Join(fakeDir, "gh")
	if runtime.GOOS == "windows" {
		fakeGh = filepath.Join(fakeDir, "gh.bat")
	}
	if err := os.WriteFile(fakeGh, []byte("#!/bin/sh\necho fake-token\n"), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}

	ghConfigDir := t.TempDir()

	cmd := exec.CommandContext(ctx, fx.runnerBinary,
		"--run-id", child.ID.String(),
		"--backend-url", fx.url,
		"--workflow", "feature_change",
		"--stage", "implement",
		"--stage-id", implStage.ID.String(),
		"--fetch-prompt",
		"--upload-trace",
		"--working-dir", workDir,
		"--check-base-ref", "main",
		"--github-repo", "kuhlman-labs/fishhawk",
		"--base-branch", "main",
	)
	runnerEnv := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		switch {
		case strings.HasPrefix(e, "PATH="):
			e = "PATH=" + fakeDir + ":" + strings.TrimPrefix(e, "PATH=")
		case strings.HasPrefix(e, "GH_TOKEN="), strings.HasPrefix(e, "GITHUB_TOKEN="), strings.HasPrefix(e, "GH_CONFIG_DIR="):
			continue
		}
		runnerEnv = append(runnerEnv, e)
	}
	runnerEnv = append(runnerEnv, "GH_TOKEN=", "GITHUB_TOKEN=", "GH_CONFIG_DIR="+ghConfigDir)
	cmd.Env = runnerEnv

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	// The runner exits 0: the no-changes report is the deliberate outcome of
	// a successful runner pass, not a runner-side failure.
	if err := cmd.Run(); err != nil {
		t.Fatalf("runner exited non-zero: %v\noutput:\n%s", err, out.String())
	}

	// The child implement stage must land FAILED with category C — never
	// wedged in `running` (the pre-#1036 shape: the push_to_shared_branch
	// gate deferred the terminal transition to a report that never came).
	got, err := fx.runRepo.GetStage(ctx, implStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State == runpkg.StageStateRunning {
		t.Fatalf("child implement stage wedged in running (the #1036 shape)\nrunner output:\n%s", out.String())
	}
	if got.State != runpkg.StageStateFailed {
		t.Fatalf("child implement stage State = %q, want failed\nrunner output:\n%s", got.State, out.String())
	}
	if got.FailureCategory == nil || *got.FailureCategory != runpkg.FailureC {
		t.Errorf("FailureCategory = %v, want C (retryable)\nrunner output:\n%s", got.FailureCategory, out.String())
	}
	if !runpkg.ValidStageRetryTransition(got.State, runpkg.StageStatePending) {
		t.Errorf("category-C failed stage should permit retry (failed → pending)")
	}
	if !strings.Contains(out.String(), `"event":"implement_child_no_changes"`) {
		t.Errorf("missing implement_child_no_changes log line:\n%s", out.String())
	}
}

// oversizedLineSpecYAML is a feature_change workflow with no constraints, so
// the trace handler's backend re-evaluation runs (a cached spec is present)
// and records policy_evaluated passed:true over the diff, instead of
// policy_skipped spec_unavailable.
const oversizedLineSpecYAML = `version: "0.3"
workflows:
  feature_change:
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

// traceTimeForbiddenSpecYAML declares an implement-stage forbidden_paths glob
// matching added.txt. It lives ONLY on the run row's cached spec: the runner
// enforces constraints only from --constraints-file, which the test does not
// pass, so only the backend's trace-time re-evaluation sees it (#5535).
const traceTimeForbiddenSpecYAML = `version: "0.3"
workflows:
  feature_change:
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
        constraints:
          - forbidden_paths:
              - "added.txt"
        produces:
          - artifact: pull_request
`

// oversizedTextBytes is the size of the text block the oversized-line shim
// writes: over the pre-#5535 4 MiB trace-line cap and under the 16 MiB one.
const oversizedTextBytes = 5 * 1024 * 1024

// seedScopedImplementRun creates a CLI-triggered local run carrying spec as
// its cached WorkflowSpec, a plan stage with an approved plan scoped to
// added.txt, and a gated (RequiresApproval) implement stage. It returns the
// run and the implement stage.
func seedScopedImplementRun(ctx context.Context, t *testing.T, fx *localRunnerFixture, workflowSHA, spec string) (*runpkg.Run, *runpkg.Stage) {
	t.Helper()
	r, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   workflowSHA,
		TriggerSource: runpkg.TriggerCLI,
		RunnerKind:    runpkg.RunnerKindLocal,
		WorkflowSpec:  []byte(spec),
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
		Content:       scopedPlanJSON(t, "added.txt"),
		ContentHash:   workflowSHA,
	}); err != nil {
		t.Fatalf("Create plan artifact: %v", err)
	}
	implStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: r.ID, Sequence: 2, Type: runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "claude-code", RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage implement: %v", err)
	}
	return r, implStage
}

// runImplementRunner runs the real runner binary for an implement stage with
// the claude shim in fakeDir first on PATH and the gh-CLI push fallback (#713)
// denied (empty GH_CONFIG_DIR and token env), so no push can authenticate.
// extraArgs are appended to the common flags. It returns the combined output
// and the exit error.
func runImplementRunner(ctx context.Context, t *testing.T, fx *localRunnerFixture, r *runpkg.Run, stage *runpkg.Stage, workDir, fakeDir string, extraArgs ...string) (string, error) {
	t.Helper()
	args := []string{
		"--run-id", r.ID.String(),
		"--backend-url", fx.url,
		"--workflow", "feature_change",
		"--stage", "implement",
		"--stage-id", stage.ID.String(),
		"--fetch-prompt",
		"--upload-trace",
		"--working-dir", workDir,
		"--check-base-ref", "main",
		"--github-repo", "kuhlman-labs/fishhawk",
		"--base-branch", "main",
	}
	cmd := exec.CommandContext(ctx, fx.runnerBinary, append(args, extraArgs...)...)
	ghConfigDir := t.TempDir()
	runnerEnv := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		switch {
		case strings.HasPrefix(e, "PATH="):
			e = "PATH=" + fakeDir + ":" + strings.TrimPrefix(e, "PATH=")
		case strings.HasPrefix(e, "GH_TOKEN="), strings.HasPrefix(e, "GITHUB_TOKEN="), strings.HasPrefix(e, "GH_CONFIG_DIR="):
			continue
		}
		runnerEnv = append(runnerEnv, e)
	}
	runnerEnv = append(runnerEnv, "GH_TOKEN=", "GITHUB_TOKEN=", "GH_CONFIG_DIR="+ghConfigDir)
	cmd.Env = runnerEnv
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// writeClaudeShim writes an executable `claude` shim with the given sh body
// into a fresh directory and returns that directory.
func writeClaudeShim(t *testing.T, body string) string {
	t.Helper()
	fakeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeDir, "claude"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return fakeDir
}

// runnerLogLines returns the runner's JSONL log lines, each cut to 512 bytes
// so a failure message never dumps a multi-MiB trace line.
func runnerLogLines(out string) string {
	var b strings.Builder
	for _, line := range strings.Split(out, "\n") {
		if len(line) > 512 {
			line = line[:512] + fmt.Sprintf("…(%d bytes)", len(line))
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// stageAudit returns the run's audit entries of category for stageID.
func stageAudit(ctx context.Context, t *testing.T, fx *localRunnerFixture, runID, stageID uuid.UUID, category string) []*audit.Entry {
	t.Helper()
	entries, err := fx.auditRepo.ListForRunByCategory(ctx, runID, category)
	if err != nil {
		t.Fatalf("ListForRunByCategory(%s): %v", category, err)
	}
	var out []*audit.Entry
	for _, e := range entries {
		if e.StageID != nil && *e.StageID == stageID {
			out = append(out, e)
		}
	}
	return out
}

// TestE2E_LocalRunner_ImplementOversizedEventLine_ReachesPushGate is the
// real-runner cross-boundary test for #5535's line bound. The fake claude
// shim prints one ~5 MiB stream-json assistant line, writes the scoped file
// added.txt and prints a result line. Before #5535 the runner's 4 MiB trace
// line cap and the backend's 4 MiB bundle reader each dropped or refused such
// a line. It asserts, across the real runner line reader and Pack, the real
// trace handler's bounded reader and policy arm, and real Postgres:
//
//   - the stored RAW bundle decodes with the backend's bounded
//     bundle.ReadEvents and still carries a line over 4 MiB;
//   - policy_evaluated records passed:true over the one-file diff;
//   - the trace 202 held the stage `running` for its push report (it was not
//     advanced as a no-changes pass);
//   - the hermetic PR-open failure (no App installation, denied gh fallback)
//     then fails the stage category C through the runner's /pull-request
//     report.
func TestE2E_LocalRunner_ImplementOversizedEventLine_ReachesPushGate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available; skipping oversized-line E2E")
	}
	if runtime.GOOS == "windows" {
		t.Skip("sh claude shim; skipping on windows")
	}
	fx := newLocalRunnerFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	r, implStage := seedScopedImplementRun(ctx, t, fx, "deadbeef-oversized", oversizedLineSpecYAML)
	workDir := t.TempDir()
	gitRepoInit(t, workDir)

	fakeDir := writeClaudeShim(t, fmt.Sprintf(
		"printf '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"'\n"+
			"head -c %d /dev/zero | tr '\\000' 'a'\n"+
			"printf '\"}]}}\\n'\n"+
			"echo 'package main' > added.txt\n"+
			"printf '{\"type\":\"result\"}\\n'\n", oversizedTextBytes))

	out, runErr := runImplementRunner(ctx, t, fx, r, implStage, workDir, fakeDir)
	logs := runnerLogLines(out)
	if runErr == nil {
		t.Fatalf("runner exited 0; expected failure on the hermetic PR-open step\noutput:\n%s", logs)
	}

	// The stored raw bundle: located by its trace_uploaded audit row, read
	// from the trace store, and decoded with the backend's bounded reader.
	var rawHash string
	for _, e := range stageAudit(ctx, t, fx, r.ID, implStage.ID, "trace_uploaded") {
		var p struct {
			Variant     string `json:"variant"`
			ContentHash string `json:"content_hash"`
		}
		if err := json.Unmarshal(e.Payload, &p); err == nil && p.Variant == string(tracestore.VariantRaw) {
			rawHash = p.ContentHash
		}
	}
	if rawHash == "" {
		t.Fatalf("no raw trace_uploaded audit row for the stage\noutput:\n%s", logs)
	}
	rc, err := fx.traceStore.Get(ctx, tracestore.BundleRef{RunID: r.ID, Variant: tracestore.VariantRaw, ContentHash: rawHash})
	if err != nil {
		t.Fatalf("get raw bundle: %v", err)
	}
	rawBundle, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read raw bundle: %v", err)
	}
	lines, err := bundle.ReadEvents(rawBundle)
	if err != nil {
		t.Fatalf("backend bundle.ReadEvents refused the stored raw bundle: %v", err)
	}
	largest := 0
	for _, l := range lines {
		if len(l.Data) > largest {
			largest = len(l.Data)
		}
	}
	if largest <= 4*1024*1024 {
		t.Fatalf("largest stored raw bundle line data = %d bytes, want > 4 MiB (the shim's %d-byte text was dropped or elided)\noutput:\n%s",
			largest, oversizedTextBytes, logs)
	}

	// Policy evaluated the diff (not skipped, not undeterminable) and passed.
	evals := stageAudit(ctx, t, fx, r.ID, implStage.ID, policy.CategoryPolicyEvaluated)
	if len(evals) == 0 {
		t.Fatalf("no policy_evaluated audit row for the stage\noutput:\n%s", logs)
	}
	var eval policy.EvaluationPayload
	if err := json.Unmarshal(evals[0].Payload, &eval); err != nil {
		t.Fatalf("decode policy_evaluated: %v", err)
	}
	if !eval.Passed || eval.SkipReason != "" {
		t.Errorf("policy_evaluated passed=%t skip_reason=%q, want passed with no skip\npayload: %s", eval.Passed, eval.SkipReason, evals[0].Payload)
	}
	if len(eval.Diff) != 1 || eval.Diff[0].Path != "added.txt" {
		t.Errorf("policy_evaluated diff = %+v, want exactly added.txt", eval.Diff)
	}

	// Held at trace time: the raw upload's 202 answered stage_state running.
	if !regexpTraceUploadedRunning.MatchString(out) {
		t.Errorf("no trace_uploaded line with stage_state running; the stage was not held for its push report\noutput:\n%s", logs)
	}

	got, err := fx.runRepo.GetStage(ctx, implStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != runpkg.StageStateFailed {
		t.Fatalf("implement stage State = %q, want failed\noutput:\n%s", got.State, logs)
	}
	if got.FailureCategory == nil || *got.FailureCategory != runpkg.FailureC {
		t.Errorf("FailureCategory = %v, want C\noutput:\n%s", got.FailureCategory, logs)
	}
	if got.FailureReason != nil && strings.HasPrefix(*got.FailureReason, "bundle_unreadable:") {
		t.Errorf("FailureReason = %q, want the runner's PR-open failure, not a trace-time bundle_unreadable", *got.FailureReason)
	}
	// The failure came from the runner's /pull-request report.
	if prFails := stageAudit(ctx, t, fx, r.ID, implStage.ID, "pull_request_failed"); len(prFails) == 0 {
		t.Errorf("no pull_request_failed audit row; the stage was not failed by the runner's PR-open report\noutput:\n%s", logs)
	}
	gotRun, err := fx.runRepo.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if gotRun.PullRequestURL != nil {
		t.Errorf("run.PullRequestURL = %q, want nil after a PR-open failure", *gotRun.PullRequestURL)
	}
}

// regexpTraceUploadedRunning matches the runner's raw trace_uploaded log line
// when the 202 reported the stage held `running`.
var regexpTraceUploadedRunning = regexp.MustCompile(`"event":"trace_uploaded"[^\n]*"variant":"raw"[^\n]*"stage_state":"running"`)

// TestE2E_LocalRunner_ImplementTraceTimeFailure_KeepsVerifyPin is the
// real-runner cross-boundary test for #5535's trace-time failure path (W10).
// The run row's cached spec forbids added.txt on the implement stage, and the
// runner gets no --constraints-file, so the runner's own pass and its
// committed-tree verify (--verify-cmd true) succeed while the backend's
// trace-time re-evaluation fails the stage category B. It asserts that:
//
//   - the runner pinned the verified head BEFORE the trace upload, and stopped
//     on the trace_rejected 202 (trace_time_stage_failed) without pushing;
//   - refs/fishhawk/checkpoints/<run>/<stage>-verify still exists in the
//     working checkout;
//   - the stage failure_reason is the policy reason ending with the verify-pin
//     clause, whose SHA equals that ref;
//   - no /pull-request report was sent and no PR URL was recorded.
//
// If the seeded constraint reached the runner, its own policy check would fail
// the pass before the pin and the short-circuit, so the verify_head_pinned and
// trace_time_stage_failed assertions fail loudly rather than passing vacuously.
func TestE2E_LocalRunner_ImplementTraceTimeFailure_KeepsVerifyPin(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available; skipping trace-time-failure E2E")
	}
	if runtime.GOOS == "windows" {
		t.Skip("sh claude shim; skipping on windows")
	}
	fx := newLocalRunnerFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	r, implStage := seedScopedImplementRun(ctx, t, fx, "deadbeef-tracetimefail", traceTimeForbiddenSpecYAML)
	workDir := t.TempDir()
	gitRepoInit(t, workDir)
	fakeDir := writeClaudeShim(t, "printf '{\"type\":\"result\"}\\n'\necho 'package main' > added.txt\n")

	out, runErr := runImplementRunner(ctx, t, fx, r, implStage, workDir, fakeDir, "--verify-cmd", "true")
	logs := runnerLogLines(out)
	if runErr == nil {
		t.Fatalf("runner exited 0; expected exitFailure on the trace-time failure\noutput:\n%s", logs)
	}

	// Ordering: pin before the trace upload, then the short-circuit.
	pinAt := strings.Index(out, `"event":"verify_head_pinned"`)
	traceAt := strings.Index(out, `"event":"trace_uploaded"`)
	stopAt := strings.Index(out, `"event":"trace_time_stage_failed"`)
	// Non-fatal: the state assertions below must also observe a missing pin or
	// a missing short-circuit, not only these log lines.
	if pinAt < 0 || traceAt < 0 || stopAt < 0 {
		t.Errorf("runner log missing verify_head_pinned (%d), trace_uploaded (%d) or trace_time_stage_failed (%d); "+
			"if the constraint reached the runner this test no longer isolates the backend-only check\noutput:\n%s",
			pinAt, traceAt, stopAt, logs)
	} else if pinAt >= traceAt || traceAt >= stopAt {
		t.Errorf("runner log order verify_head_pinned=%d trace_uploaded=%d trace_time_stage_failed=%d, want pin < trace < stop\noutput:\n%s",
			pinAt, traceAt, stopAt, logs)
	}
	if !strings.Contains(out, `"stage_failure_kind":"trace_rejected"`) {
		t.Errorf("runner log carries no stage_failure_kind trace_rejected\noutput:\n%s", logs)
	}
	for _, pushSign := range []string{`"reason":"pull_request_upload"`, `"event":"pull_request_uploaded"`, `"event":"push_token_refreshed"`, `"event":"push_token_refresh_failed"`} {
		if strings.Contains(out, pushSign) {
			t.Errorf("runner log carries %s; the runner reached the push path after a trace-time failure\noutput:\n%s", pushSign, logs)
		}
	}

	// The pin survives in the working checkout.
	ref := "refs/fishhawk/checkpoints/" + r.ID.String() + "/" + implStage.ID.String() + "-verify"
	revParse := exec.Command("git", "-C", workDir, "rev-parse", "--verify", ref)
	shaOut, err := revParse.Output()
	if err != nil {
		t.Fatalf("git rev-parse %s: %v (the verify pin was released or never taken)\noutput:\n%s", ref, err, logs)
	}
	pinnedSHA := strings.TrimSpace(string(shaOut))

	got, err := fx.runRepo.GetStage(ctx, implStage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != runpkg.StageStateFailed {
		t.Fatalf("implement stage State = %q, want failed\noutput:\n%s", got.State, logs)
	}
	if got.FailureCategory == nil || *got.FailureCategory != runpkg.FailureB {
		t.Errorf("FailureCategory = %v, want B (policy violations on backend re-evaluation)", got.FailureCategory)
	}
	if got.FailureReason == nil {
		t.Fatalf("FailureReason is nil, want the policy reason with the verify-pin clause")
	}
	reason := *got.FailureReason
	if !strings.HasPrefix(reason, "policy violations on backend re-evaluation") {
		t.Errorf("FailureReason = %q, want prefix %q", reason, "policy violations on backend re-evaluation")
	}
	wantClause := "; verified head " + pinnedSHA + " pinned at " + ref
	if !strings.HasSuffix(reason, wantClause) {
		t.Errorf("FailureReason = %q, want suffix %q (the clause naming the kept pin)", reason, wantClause)
	}

	if prFails := stageAudit(ctx, t, fx, r.ID, implStage.ID, "pull_request_failed"); len(prFails) != 0 {
		t.Errorf("pull_request_failed audit rows = %d, want 0 (the runner must not push or report after trace_rejected)", len(prFails))
	}
	gotRun, err := fx.runRepo.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if gotRun.PullRequestURL != nil {
		t.Errorf("run.PullRequestURL = %q, want nil", *gotRun.PullRequestURL)
	}
}

// gitRepoInit makes dir a git repo on branch `main` with a single base
// commit, so an implement stage run against it produces a non-empty diff.
func gitRepoInit(t *testing.T, dir string) {
	t.Helper()
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	runGit := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit("add", "README.md")
	runGit("commit", "-m", "base")
	// Rename whatever the default initial branch is to `main` so
	// --base-branch main resolves regardless of the host git's init.defaultBranch.
	runGit("branch", "-M", "main")
}

// scopedPlanJSON returns a schema-valid standard_v1 plan whose scope.files
// declares exactly scopePath, so the implement stage's scope-bounded diff
// stages the file the fake agent writes.
func scopedPlanJSON(t *testing.T, scopePath string) []byte {
	t.Helper()
	m := map[string]any{
		"plan_version": "standard_v1",
		"ticket_reference": map[string]any{
			"type": "github_issue", "url": "https://github.com/x/y/issues/1", "id": "x/y#1",
		},
		"generated_by": map[string]any{
			"agent": "claude-code", "model": "claude-opus-4-7",
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		},
		"summary": "PR-open failure E2E plan.",
		"scope": map[string]any{
			"files": []any{map[string]any{"path": scopePath, "operation": "create"}},
		},
		"approach": []any{map[string]any{"step": 1, "description": "Write the file."}},
		"verification": map[string]any{
			"test_strategy": "Run go test.", "rollback_plan": "Revert the commit.",
		},
		"predicted_runtime_minutes":    5,
		"predicted_runtime_confidence": "high",
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal scoped plan: %v", err)
	}
	return b
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
