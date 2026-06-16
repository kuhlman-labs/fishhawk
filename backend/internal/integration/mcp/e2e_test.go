// Package mcpe2e_test is the cross-component integration test for
// the MCP loop: real backend HTTP server → real Postgres → real
// fishhawk-mcp binary spawned as a subprocess → MCP tool call over
// stdio → assertion on the response. Completes the acceptance gap
// E19.8 / #348 left open (#371): "agent invocation receives the
// env vars; MCP server can authenticate using them; revocation
// works."
//
// Lives under backend/internal/integration/mcp/ because it imports
// every backend repo + spawns the backend's MCP binary. The
// runner's FetchMCPToken HTTP shape is reproduced inline rather
// than imported (backend → runner would invert the module
// dependency direction).
package mcpe2e_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mcptoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// e2eFixture wires every piece the cross-component loop needs.
// Built once per test via newFixture; teardown closes the pool +
// shuts down the httptest server + terminates the container.
type e2eFixture struct {
	url          string // backend httptest URL
	pool         *pgxpool.Pool
	runID        uuid.UUID
	signingPriv  ed25519.PrivateKey
	mcpTokenRepo mcptoken.Repository
	runRepo      runpkg.Repository
	apitokenRepo apitoken.Repository
	operatorTok  string // fhk_* apitoken with the full operator write scopes
	mcpBinary    string // path to the built fishhawk-mcp binary
}

func newFixture(t *testing.T) *e2eFixture {
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
			t.Skipf("Docker not available; skipping MCP E2E: %v", err)
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

	// 2. Apply migrations + open the pool.
	if err := postgres.MigrateUp(pgURL); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	pool, err := pgxpool.New(ctx, pgURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// 3. Build the real repos + the backend server. SigningRepo,
	// RunRepo, MCPTokenRepo, AuditRepo are the four the
	// mcp-token + bearerAuth + tool paths consult. APITokenRepo
	// is wired so the operator-side write tools (E22 / #389) have
	// a real authenticator to resolve against.
	runRepo := runpkg.NewPostgresRepository(pool)
	signingRepo := signing.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	mcpRepo := mcptoken.NewPostgresRepository(pool)
	apiRepo := apitoken.NewPostgresRepository(pool)
	s := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      runRepo,
		SigningRepo:  signingRepo,
		AuditRepo:    auditRepo,
		MCPTokenRepo: mcpRepo,
		APITokenRepo: apiRepo,
	})
	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)

	// 4. Seed a run + issue a signing key directly via the repos.
	// We bypass the HTTP signing-key endpoint (which would need
	// OIDC) — the test already has the pool and the runner-side
	// flow at production runtime is the same: issue, then sign.
	r, err := runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: runpkg.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	issued, err := signingRepo.Issue(ctx, r.ID, time.Hour)
	if err != nil {
		t.Fatalf("Issue signing key: %v", err)
	}

	// Issue an operator-side apitoken with the full write scopes
	// E22's Phase A tools require. Same shape `fishhawkd token
	// issue` produces in dev.
	operatorTok, err := apiRepo.Issue(ctx, "brett@e2e-test", []string{
		"read:runs", "read:audit",
		"write:runs", "write:approvals", "write:stages",
	})
	if err != nil {
		t.Fatalf("Issue operator apitoken: %v", err)
	}

	// 5. Build the fishhawk-mcp binary into a temp dir. We use a
	// real binary rather than `go run` so the MCP server's
	// JSON-RPC stdout isn't contaminated by Go's build-progress
	// chatter. Cold build is a few seconds; subsequent builds in
	// the same test process hit the build cache.
	binary := filepath.Join(t.TempDir(), mcpBinaryName())
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer buildCancel()
	build := exec.CommandContext(buildCtx, "go", "build",
		"-o", binary,
		"github.com/kuhlman-labs/fishhawk/backend/cmd/fishhawk-mcp",
	)
	var buildErr bytes.Buffer
	build.Stderr = &buildErr
	if err := build.Run(); err != nil {
		t.Fatalf("build fishhawk-mcp: %v\nstderr: %s", err, buildErr.String())
	}

	return &e2eFixture{
		url:          httpSrv.URL,
		pool:         pool,
		runID:        r.ID,
		signingPriv:  issued.PrivateKey,
		mcpTokenRepo: mcpRepo,
		runRepo:      runRepo,
		apitokenRepo: apiRepo,
		operatorTok:  operatorTok.PlainText,
		mcpBinary:    binary,
	}
}

func mcpBinaryName() string {
	if runtime.GOOS == "windows" {
		return "fishhawk-mcp.exe"
	}
	return "fishhawk-mcp"
}

// fetchMCPToken reproduces runner/internal/upload.FetchMCPToken's
// HTTP shape inline. Backend → runner imports would invert the
// module dependency, so we keep this ~20-line duplicate over a
// cross-module dependency. If a third caller emerges, hoist into
// a shared package.
func fetchMCPToken(t *testing.T, ctx context.Context, baseURL string, runID uuid.UUID, priv ed25519.PrivateKey) string {
	t.Helper()
	body := []byte{}
	digest := sha256.Sum256(body)
	signature := ed25519.Sign(priv, digest[:])
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		baseURL+"/v0/runs/"+runID.String()+"/mcp-token",
		bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Fishhawk-Signature", hex.EncodeToString(signature))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fetch mcp token: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("mcp-token endpoint status %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Token   string    `json:"token"`
		TokenID uuid.UUID `json:"token_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode mcp-token response: %v", err)
	}
	if !strings.HasPrefix(out.Token, mcptoken.TokenPrefix) {
		t.Fatalf("token missing %q prefix: %q", mcptoken.TokenPrefix, out.Token)
	}
	return out.Token
}

// connectMCPClient spawns the fishhawk-mcp binary with the supplied
// env and returns a connected MCP ClientSession. The cmd's stderr
// is wired through for diagnostics — JSON-RPC stays on stdout.
func connectMCPClient(t *testing.T, ctx context.Context, binary, token, backendURL string) *mcp.ClientSession {
	t.Helper()
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(),
		"FISHHAWK_API_TOKEN="+token,
		"FISHHAWK_BACKEND_URL="+backendURL,
	)
	// Capture stderr separately. The MCP SDK pipes stdin/stdout
	// itself via CommandTransport; if the server logs something on
	// stderr, the test sees it on failure rather than losing it.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "fishhawk-e2e-test",
		Version: "v0.0.1",
	}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect MCP client: %v\nstderr: %s", err, stderr.String())
	}
	t.Cleanup(func() {
		_ = session.Close()
		// Surface server stderr if the test failed — the SDK
		// closes the subprocess on session.Close() so any panic
		// or error message is in this buffer.
		if t.Failed() && stderr.Len() > 0 {
			t.Logf("fishhawk-mcp stderr:\n%s", stderr.String())
		}
	})
	return session
}

// TestE2E_MCPLoop_HappyPath_IssuedTokenAuthenticates covers acceptance
// criteria (a) and (b): the runner-side fetch returns a real
// `fhm_`-prefixed bearer; spawning the MCP binary with that bearer
// + the backend URL produces a working tool surface; calling
// fishhawk_get_run_status returns the seeded run's data.
func TestE2E_MCPLoop_HappyPath_IssuedTokenAuthenticates(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	token := fetchMCPToken(t, ctx, fx.url, fx.runID, fx.signingPriv)
	session := connectMCPClient(t, ctx, fx.mcpBinary, token, fx.url)

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_get_run_status",
		Arguments: map[string]any{
			"run_id": fx.runID.String(),
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %+v", result.Content)
	}
	body := toolContentString(t, result)
	// The response is the serialized GetRunStatusOutput — assert
	// the seeded run id is present (the tool returns the full
	// Run row, ordered stages, and recent audit).
	if !strings.Contains(body, fx.runID.String()) {
		t.Errorf("tool response missing seeded run id %s\nresponse: %s", fx.runID, body)
	}
	if !strings.Contains(body, "feature_change") {
		t.Errorf("tool response missing workflow id; response: %s", body)
	}
}

// TestE2E_MCPLoop_RevokedToken_AuthLayerRejects covers acceptance
// criterion (c): revocation propagates to the bearer-auth layer.
//
// Why not assert on the tool call? In v0 the /v0/runs/{id} reads
// don't enforce authentication — the bearer middleware resolves
// what it can and falls through to the anonymous identity on
// failure (`backend/internal/server/middleware.go::bearerAuth`).
// Handlers serve anonymous reads. So a revoked token's tool call
// still succeeds at the data layer — what changes is the identity
// the middleware *would* propagate downstream once enforcement
// lands. The cross-component property we can demonstrate today:
// the HTTP-issued token authenticates through the mcptoken repo
// before revocation and fails ErrNotFound after.
func TestE2E_MCPLoop_RevokedToken_AuthLayerRejects(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	token := fetchMCPToken(t, ctx, fx.url, fx.runID, fx.signingPriv)

	// Pre-revocation: the same token the HTTP endpoint minted
	// resolves through the repo to the seeded run.
	rec, err := fx.mcpTokenRepo.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("pre-revocation Authenticate: %v", err)
	}
	if rec.RunID != fx.runID {
		t.Errorf("Authenticate run id = %s, want %s", rec.RunID, fx.runID)
	}

	// Revoke directly via the repo. RevokeForRun is what the
	// production cancel-run path calls.
	n, err := fx.mcpTokenRepo.RevokeForRun(ctx, fx.runID)
	if err != nil {
		t.Fatalf("RevokeForRun: %v", err)
	}
	if n != 1 {
		t.Errorf("RevokeForRun count = %d, want 1", n)
	}

	// Post-revocation: the same token now fails authentication.
	// This is what the bearer middleware sees on the next request;
	// once handlers enforce, it surfaces as 401 at the wire.
	if _, err := fx.mcpTokenRepo.Authenticate(ctx, token); !errors.Is(err, mcptoken.ErrNotFound) {
		t.Errorf("post-revocation Authenticate err = %v, want ErrNotFound", err)
	}
}

// TestE2E_MCPLoop_MalformedToken_AuthRejects is a defensive guard:
// a token with a valid prefix but never issued must fail the repo's
// Authenticate check. Same v0 caveat as the revocation test — the
// observable side of the auth layer is at the repo today, not the
// handler. The HappyPath test already proves real tokens flow
// through the tool surface end-to-end.
func TestE2E_MCPLoop_MalformedToken_AuthRejects(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	bogus := mcptoken.TokenPrefix + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, err := fx.mcpTokenRepo.Authenticate(ctx, bogus); !errors.Is(err, mcptoken.ErrNotFound) {
		t.Errorf("Authenticate(bogus) err = %v, want ErrNotFound", err)
	}
}

// toolContentString collapses a tool result's Content slice into
// one string so substring assertions can run against the full
// payload. The SDK returns Content as []Content (heterogeneous);
// for our tools the payload is a single TextContent block
// carrying the serialized output struct.
func toolContentString(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	if r == nil {
		t.Fatal("nil CallToolResult")
	}
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	// The tool's typed Output is also surfaced via the structured
	// content channel on the SDK side. Walk it too for fidelity.
	if r.StructuredContent != nil {
		raw, _ := json.Marshal(r.StructuredContent)
		b.Write(raw)
	}
	return b.String()
}

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

// TestE2E_MCPLoop_OperatorWritePath_StartRun covers E22.6's "exercise
// one write tool end-to-end" acceptance criterion. The flow:
//
//  1. Operator-side fhk_* apitoken (with write:runs scope) registers
//     with the spawned fishhawk-mcp binary via FISHHAWK_API_TOKEN.
//  2. Calls fishhawk_start_run with valid params.
//  3. Asserts the backend response carries a fresh run id.
//  4. Verifies the run row exists in Postgres directly — proves the
//     MCP → backend → DB write path is intact end-to-end.
//
// Distinct from the existing read-only tests (#371) because this is
// the first time we exercise a write tool through the real MCP
// binary + real backend + real DB stack.
func TestE2E_MCPLoop_OperatorWritePath_StartRun(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, fx.url)

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_start_run",
		Arguments: map[string]any{
			"repo":           "kuhlman-labs/fishhawk",
			"workflow_id":    "feature_change",
			"workflow_sha":   "deadbeef-mcp-e2e",
			"trigger_source": "cli",
		},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_start_run: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %+v", result.Content)
	}

	// Extract the created run id from the structured output. The
	// tool surfaces {run: {id, ...}, idempotent} per StartRunOutput.
	if result.StructuredContent == nil {
		t.Fatal("tool returned no StructuredContent")
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out struct {
		Run struct {
			ID    string `json:"id"`
			Repo  string `json:"repo"`
			State string `json:"state"`
		} `json:"run"`
		Idempotent bool `json:"idempotent"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode structured output: %v", err)
	}
	if out.Run.ID == "" {
		t.Fatalf("created run has no id; structured output: %s", raw)
	}
	if out.Idempotent {
		t.Errorf("fresh start_run returned Idempotent=true; expected false")
	}
	if out.Run.Repo != "kuhlman-labs/fishhawk" {
		t.Errorf("run.Repo = %q, want kuhlman-labs/fishhawk", out.Run.Repo)
	}
	if out.Run.State != "pending" {
		t.Errorf("run.State = %q, want pending", out.Run.State)
	}

	// Verify the run actually landed in the DB. Closes the "MCP →
	// backend → DB" loop end-to-end.
	createdID, err := uuid.Parse(out.Run.ID)
	if err != nil {
		t.Fatalf("created run id %q is not a UUID: %v", out.Run.ID, err)
	}
	got, err := fx.runRepo.GetRun(ctx, createdID)
	if err != nil {
		t.Fatalf("GetRun for created id: %v", err)
	}
	if got.WorkflowID != "feature_change" {
		t.Errorf("DB row WorkflowID = %q, want feature_change", got.WorkflowID)
	}
	if got.WorkflowSHA != "deadbeef-mcp-e2e" {
		t.Errorf("DB row WorkflowSHA = %q, want deadbeef-mcp-e2e", got.WorkflowSHA)
	}
}

// TestE2E_BindingAssertions_PersistedAndEchoedOnPrompt drives the #1171
// declaration end-to-end (real MCP binary → real backend HTTP → real
// Postgres): fishhawk_approve_plan carrying binding_assertions must (a) persist
// them on the approval_submitted audit payload AND (b) surface them on the
// subsequent implement prompt-response. This writer→audit→read-back→response
// seam is the one per-side unit tests cannot cover (#618).
func TestE2E_BindingAssertions_PersistedAndEchoedOnPrompt(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Second backend over the SAME pool with ArtifactRepo + GitHub wired so
	// the implement prompt can load the approved plan (the echo is guarded on
	// approvedPlan != nil). The fixture's own server has neither.
	auditRepo := audit.NewPostgresRepository(fx.pool)
	signingRepo := signing.NewPostgresRepository(fx.pool)
	artifactRepo := artifact.NewPostgresRepository(fx.pool)
	srv := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      fx.runRepo,
		AuditRepo:    auditRepo,
		SigningRepo:  signingRepo,
		ArtifactRepo: artifactRepo,
		ApprovalRepo: approval.NewPostgresRepository(fx.pool),
		APITokenRepo: fx.apitokenRepo,
		GitHub:       githubclient.New(nil),
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	// 1. Plan stage parked at the approval gate carrying an approved
	// standard_v1 plan (the echo needs a loadable approved plan).
	planStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            fx.runID,
		Sequence:         1,
		Type:             runpkg.StageTypePlan,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage plan: %v", err)
	}
	planContent, err := json.Marshal(map[string]any{
		"plan_version": "standard_v1",
		"summary":      "scoped plan",
		"verification": map[string]any{"test_strategy": "ts", "rollback_plan": "rb"},
		"scope": map[string]any{
			"files": []map[string]any{
				{"path": "backend/internal/server/prompt.go", "operation": "modify"},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	sum := sha256.Sum256(planContent)
	if _, err := artifactRepo.Create(ctx, artifact.CreateParams{
		StageID:       planStage.ID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planContent,
		ContentHash:   hex.EncodeToString(sum[:]),
	}); err != nil {
		t.Fatalf("Create plan artifact: %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, planStage.ID)

	// 2. Implement stage left pending (a runnable state for prompt-render).
	implStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:        fx.runID,
		Sequence:     2,
		Type:         runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent,
		ExecutorRef:  "fishhawk/runner@v1",
	})
	if err != nil {
		t.Fatalf("CreateStage implement: %v", err)
	}

	// 3. Approve the plan through the real MCP binary, declaring one
	// file_contains and one test_asserts assertion.
	wantAssertions := []struct{ Type, Path, Literal string }{
		{"file_contains", "backend/internal/server/prompt.go", "BindingAssertions"},
		{"test_asserts", "backend/internal/server/prompt_test.go", "TestResolveApprovalBindingAssertions"},
	}
	args := make([]map[string]any, 0, len(wantAssertions))
	for _, a := range wantAssertions {
		args = append(args, map[string]any{"type": a.Type, "path": a.Path, "literal": a.Literal})
	}
	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_approve_plan",
		Arguments: map[string]any{
			"run_id":             fx.runID.String(),
			"reason":             "enforce the binding-assertion invariants",
			"binding_assertions": args,
		},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_approve_plan: %v", err)
	}
	if result.IsError {
		t.Fatalf("approve tool returned error: %s", toolContentString(t, result))
	}

	// 4. Persistence half: the approval_submitted payload carries the exact
	// declared assertions.
	entries, err := auditRepo.ListForRunByCategory(ctx, fx.runID, "approval_submitted")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	var persisted []struct {
		Type    string `json:"type"`
		Path    string `json:"path"`
		Literal string `json:"literal"`
	}
	found := false
	for _, e := range entries {
		var payload struct {
			Decision          string `json:"decision"`
			BindingAssertions []struct {
				Type    string `json:"type"`
				Path    string `json:"path"`
				Literal string `json:"literal"`
			} `json:"binding_assertions"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			continue
		}
		if payload.Decision == "approve" && len(payload.BindingAssertions) > 0 {
			persisted = payload.BindingAssertions
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no approval_submitted entry carried binding_assertions; entries=%d", len(entries))
	}
	if len(persisted) != len(wantAssertions) {
		t.Fatalf("persisted binding_assertions len = %d, want %d: %+v", len(persisted), len(wantAssertions), persisted)
	}
	for i, w := range wantAssertions {
		if persisted[i].Type != w.Type || persisted[i].Path != w.Path || persisted[i].Literal != w.Literal {
			t.Errorf("persisted[%d] = %+v, want {%s %s %s}", i, persisted[i], w.Type, w.Path, w.Literal)
		}
	}

	// 5. Read-back half: the implement prompt-response echoes them.
	echoed := getPromptRenderBindingAssertions(t, ctx, httpSrv.URL, implStage.ID)
	if len(echoed) != len(wantAssertions) {
		t.Fatalf("echoed binding_assertions len = %d, want %d: %+v", len(echoed), len(wantAssertions), echoed)
	}
	for i, w := range wantAssertions {
		if echoed[i].Type != w.Type || echoed[i].Path != w.Path || echoed[i].Literal != w.Literal {
			t.Errorf("echoed[%d] = %+v, want {%s %s %s}", i, echoed[i], w.Type, w.Path, w.Literal)
		}
	}
}

// bindingAssertionView mirrors the prompt-response binding_assertions wire
// shape so the integration test can decode it off the prompt-render endpoint.
type bindingAssertionView struct {
	Type    string `json:"type"`
	Path    string `json:"path"`
	Literal string `json:"literal"`
}

// getPromptRenderBindingAssertions fetches GET /v0/stages/{id}/prompt-render
// and returns the echoed binding_assertions (#1171), in order.
func getPromptRenderBindingAssertions(t *testing.T, ctx context.Context, baseURL string, stageID uuid.UUID) []bindingAssertionView {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		baseURL+"/v0/stages/"+stageID.String()+"/prompt-render", nil)
	if err != nil {
		t.Fatalf("build prompt-render request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("prompt-render request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("prompt-render status %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		BindingAssertions []bindingAssertionView `json:"binding_assertions"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode prompt-render response: %v", err)
	}
	return out.BindingAssertions
}

// nextActionsView mirrors the MCP server's NextActions output shape so
// the integration tests can decode it off the get_run_status response.
type nextActionsView struct {
	State   string `json:"state"`
	Actions []struct {
		Action       string            `json:"action"`
		Params       map[string]string `json:"params"`
		Precondition string            `json:"precondition"`
		Consumes     string            `json:"consumes"`
		Reason       string            `json:"reason"`
	} `json:"actions"`
}

// getNextActions calls fishhawk_get_run_status and returns the decoded
// next_actions block (nil when absent).
func getNextActions(t *testing.T, ctx context.Context, session *mcp.ClientSession, runID uuid.UUID) *nextActionsView {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_get_run_status",
		Arguments: map[string]any{"run_id": runID.String()},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_get_run_status: %v", err)
	}
	if result.IsError {
		t.Fatalf("get_run_status tool returned error: %s", toolContentString(t, result))
	}
	var out struct {
		NextActions *nextActionsView `json:"next_actions"`
	}
	decodeStructured(t, result, &out)
	return out.NextActions
}

// TestE2E_NextActions_PlanGateParkedAndMergeRitual asserts the #1024
// next_actions block end-to-end (real MCP binary → real backend HTTP →
// real Postgres) at two parked points of the driven lifecycle:
//
//   - plan stage parked at its approval gate → the block classifies
//     plan_gate_parked and offers fishhawk_approve_plan (consuming an
//     approval slot);
//   - run succeeded with its PR open → the block classifies
//     succeeded_pr_open and leads with the ordered merge ritual
//     (approve_pr → merge_pr → post_merge).
//
// Per-layer units cover the classifier table; this drives the audit/API
// → classifier seam against the real backend reads (#618 rule).
func TestE2E_NextActions_PlanGateParkedAndMergeRitual(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 1. Plan stage parked at the approval gate (no review configured, so
	// the review status reads none — the approve/reject arm, not the
	// await-verdicts arm).
	planStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            fx.runID,
		Sequence:         1,
		Type:             runpkg.StageTypePlan,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage(plan): %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, planStage.ID)

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, fx.url)

	na := getNextActions(t, ctx, session, fx.runID)
	if na == nil {
		t.Fatal("next_actions absent at the parked plan gate; want plan_gate_parked")
	}
	if na.State != "plan_gate_parked" {
		t.Errorf("next_actions.state = %q, want plan_gate_parked", na.State)
	}
	if len(na.Actions) == 0 {
		t.Fatal("next_actions.actions empty at a non-terminal state — the structural invariant is broken")
	}
	approve := na.Actions[0]
	if approve.Action != "fishhawk_approve_plan" {
		t.Errorf("actions[0] = %q, want fishhawk_approve_plan", approve.Action)
	}
	if approve.Consumes != "approval_slot" {
		t.Errorf("approve consumes = %q, want approval_slot", approve.Consumes)
	}
	if approve.Params["run_id"] != fx.runID.String() {
		t.Errorf("approve params.run_id = %q, want %s", approve.Params["run_id"], fx.runID)
	}

	// 2. Drive the run to succeeded with its PR stamped — the merge-ritual
	// parked point.
	if _, err := fx.runRepo.SetRunPullRequestURL(ctx, fx.runID, "https://github.com/kuhlman-labs/fishhawk/pull/4242"); err != nil {
		t.Fatalf("SetRunPullRequestURL: %v", err)
	}
	for _, to := range []runpkg.State{runpkg.StateRunning, runpkg.StateSucceeded} {
		if _, err := fx.runRepo.TransitionRun(ctx, fx.runID, to); err != nil {
			t.Fatalf("TransitionRun → %s: %v", to, err)
		}
	}

	na = getNextActions(t, ctx, session, fx.runID)
	if na == nil {
		t.Fatal("next_actions absent on the succeeded+PR-open run; want the merge ritual")
	}
	if na.State != "succeeded_pr_open" {
		t.Errorf("next_actions.state = %q, want succeeded_pr_open", na.State)
	}
	wantRitual := []string{"approve_pr", "merge_pr", "post_merge"}
	if len(na.Actions) != len(wantRitual) {
		t.Fatalf("merge ritual actions = %+v, want %v in order", na.Actions, wantRitual)
	}
	for i, want := range wantRitual {
		if na.Actions[i].Action != want {
			t.Errorf("actions[%d] = %q, want %q (the ritual is ordered)", i, na.Actions[i].Action, want)
		}
	}
	if na.Actions[0].Params["pr_url"] != "https://github.com/kuhlman-labs/fishhawk/pull/4242" {
		t.Errorf("approve_pr params.pr_url = %q, want the stamped PR", na.Actions[0].Params["pr_url"])
	}
}
