package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// envFunc returns a func(string) string backed by a literal map. Relocated
// into the package with the moved tests (E66.7 / #2408): it previously lived
// in the command's main_test.go, which stayed with the binary, but
// onboarding_test.go (moved here) still constructs a runResolver with it. We
// don't use os.Setenv in tests because env state would leak across parallel
// runs; the tool constructors take the getter as a parameter for exactly this
// reason.
func envFunc(env map[string]string) func(string) string {
	return func(k string) string {
		return env[k]
	}
}

func TestBuildServer_HandshakeReady(t *testing.T) {
	// The server should construct cleanly with a fully-registered
	// registry. The protocol-level handshake itself is tested by the
	// SDK; this test just locks in that buildServer doesn't panic and
	// returns a non-nil server we can hand to a transport later. Relocated
	// from the command's main_test.go with buildServer (E66.7 / #2408).
	srv := buildServer(config{
		backendURL: "http://localhost:8080",
		apiToken:   "tok",
	})
	if srv == nil {
		t.Fatal("buildServer returned nil")
	}
}

func TestHandshakeVersion(t *testing.T) {
	// Unstamped builds (GitSHA "unknown") advertise the bare base so the
	// handshake string is unchanged from pre-stamping behavior; stamped
	// builds append "+<sha>" including any -dirty suffix. Relocated from
	// the command's main_test.go with handshakeVersion (E66.7 / #2408).
	for _, tc := range []struct {
		sha  string
		want string
	}{
		{"unknown", serverVersion},
		{"", serverVersion},
		{"abc1234", serverVersion + "+abc1234"},
		{"abc1234-dirty", serverVersion + "+abc1234-dirty"},
	} {
		if got := handshakeVersion(tc.sha); got != tc.want {
			t.Errorf("handshakeVersion(%q) = %q, want %q", tc.sha, got, tc.want)
		}
	}
}

// TestNewServer_ReturnsRegisteredServer asserts the exported entry point
// constructs a non-nil server — the coverage gap left when the command's
// newServer closure collapsed into a call to mcpserver.NewServer.
func TestNewServer_ReturnsRegisteredServer(t *testing.T) {
	srv := NewServer(Config{BackendURL: "http://localhost:8080", APIToken: "tok"})
	if srv == nil {
		t.Fatal("NewServer returned nil")
	}
}

// TestConfigInternal_PlumbsTokenToAPIClient pins the Config -> config ->
// apiClient bearer path: the exported Config's APIToken must reach the
// apiClient's token unchanged through the internal() adapter. This is the
// assertion relocated out of the command's main_test.go (which can no
// longer see newAPIClient after the extraction) and is what keeps the
// two-similarly-named-types trade (unexported config alongside exported
// Config) honest.
func TestConfigInternal_PlumbsTokenToAPIClient(t *testing.T) {
	cfg := Config{BackendURL: "http://localhost:8080", APIToken: "fhk_bearer"}
	client := newAPIClient(cfg.internal())
	if client.token != "fhk_bearer" {
		t.Errorf("apiClient bearer = %q, want the Config token fhk_bearer", client.token)
	}
	if client.baseURL != "http://localhost:8080" {
		t.Errorf("apiClient baseURL = %q, want http://localhost:8080", client.baseURL)
	}
}

// callDispatchViaNewServer builds a REAL server via NewServer(cfg), connects an
// in-memory MCP client session, and calls fishhawk_dispatch_stage with the given
// arguments. It crosses Config -> config -> runResolver -> handler -> tool result
// — the seam a per-layer unit test on resolveWorkingDir cannot cover, because a
// unit test stays green even if NewServer forgets to thread HTTPTransport onto
// the resolver (#2479).
func callDispatchViaNewServer(t *testing.T, cfg Config, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	ctx := context.Background()
	server := NewServer(cfg)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { serverSession.Close() })
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { clientSession.Close() })
	res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_dispatch_stage",
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	return res
}

// TestNewServer_HTTPTransportRefusesOmittedWorkingDir is the cross-boundary
// proof: a REAL NewServer(Config{HTTPTransport:true}) refuses an omitted
// working_dir at the tool-result layer, and the fake backend saw NO dispatch
// (no host-dispatch marker). A unit test on resolveWorkingDir alone would pass
// even if NewServer failed to thread the flag; this one would not (#2479).
func TestNewServer_HTTPTransportRefusesOmittedWorkingDir(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "awaiting_host_dispatch")

	res := callDispatchViaNewServer(t, Config{BackendURL: srv.URL, APIToken: "tok", HTTPTransport: true},
		map[string]any{
			"run_id":           runID.String(),
			"workflow":         "feature_change",
			"stage":            "implement",
			"github_repo":      "x/y",
			"push_and_open_pr": false,
			// working_dir intentionally omitted.
		})
	if !res.IsError {
		t.Fatalf("expected the tool call to error over HTTP with working_dir omitted; content: %+v", res.Content)
	}
	blob, _ := json.Marshal(res.Content)
	if !strings.Contains(string(blob), "working_dir") {
		t.Errorf("error content should name working_dir; got %s", blob)
	}
	// The fake backend saw no dispatch (refusal committed no state).
	if n := fb.hostDispatchCalledByID[stageID]; n != 0 {
		t.Errorf("host-dispatch marker called %d times, want 0 (refused before any state)", n)
	}
}

// TestNewServer_HTTPTransportEchoesResolvedWorkingDir satisfies binding
// condition 2: an actual MCP tool-call result over the HTTP-transport server
// carries resolved_working_dir, PRESENT and correct, for an explicit absolute
// working_dir. Handler-level echo assertions alone don't prove the field
// survives serialization into a real tool result — a reflection/tag typo could
// silently drop it (#2479).
func TestNewServer_HTTPTransportEchoesResolvedWorkingDir(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withFakeRunner(t, "exit 0")
	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "pending")

	dir := t.TempDir() // absolute
	res := callDispatchViaNewServer(t, Config{BackendURL: srv.URL, APIToken: "tok", HTTPTransport: true},
		map[string]any{
			"run_id":           runID.String(),
			"workflow":         "feature_change",
			"stage":            "implement",
			"working_dir":      dir,
			"github_repo":      "x/y",
			"push_and_open_pr": false,
			"runner_binary":    "/fake/fishhawk-runner",
		})
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}
	if res.StructuredContent == nil {
		t.Fatal("StructuredContent is nil; the typed output did not serialize")
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(raw), "resolved_working_dir") {
		t.Fatalf("tool result missing the resolved_working_dir field on the wire: %s", raw)
	}
	var out DispatchStageOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode DispatchStageOutput: %v", err)
	}
	if out.ResolvedWorkingDir != dir {
		t.Errorf("resolved_working_dir = %q, want %q", out.ResolvedWorkingDir, dir)
	}
}

// TestNewServer_StdioTransportResolvesOmittedWorkingDir is the mirror: a REAL
// NewServer(Config{HTTPTransport:false}) does NOT refuse an omitted working_dir
// — it resolves it to the absolute process cwd and dispatches — proving the
// divergence is keyed on the threaded transport flag (#2479).
func TestNewServer_StdioTransportResolvesOmittedWorkingDir(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withFakeRunner(t, "exit 0")
	runID := uuid.New()
	stageID := uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "pending")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	res := callDispatchViaNewServer(t, Config{BackendURL: srv.URL, APIToken: "tok", HTTPTransport: false},
		map[string]any{
			"run_id":           runID.String(),
			"workflow":         "feature_change",
			"stage":            "implement",
			"github_repo":      "x/y",
			"push_and_open_pr": false,
			"runner_binary":    "/fake/fishhawk-runner",
			// working_dir intentionally omitted.
		})
	if res.IsError {
		t.Fatalf("stdio transport must NOT refuse an omitted working_dir; content: %+v", res.Content)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out DispatchStageOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode DispatchStageOutput: %v", err)
	}
	if out.ResolvedWorkingDir != cwd {
		t.Errorf("resolved_working_dir = %q, want the absolute cwd %q", out.ResolvedWorkingDir, cwd)
	}
}
