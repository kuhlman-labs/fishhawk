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

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
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
	// mcp-token + bearerAuth + tool paths consult.
	runRepo := runpkg.NewPostgresRepository(pool)
	signingRepo := signing.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	mcpRepo := mcptoken.NewPostgresRepository(pool)
	s := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      runRepo,
		SigningRepo:  signingRepo,
		AuditRepo:    auditRepo,
		MCPTokenRepo: mcpRepo,
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
