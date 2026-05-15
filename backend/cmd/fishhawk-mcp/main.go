// Command fishhawk-mcp exposes Fishhawk run + plan + audit state to
// Claude Code (and any other MCP client) over the Model Context
// Protocol per ADR-021 / #322.
//
// Two audiences share one surface:
//
//   - The in-runner Claude Code agent reads its own run's state mid-
//     execution: what's the active plan, what audit entries have
//     fired for the current retry chain, what constraints apply.
//     Closes the agent-is-blind-to-Fishhawk-state gap that
//     motivated ADR-019.
//   - The interactive Claude Code session — an engineer asking
//     "what's the status of my current run" — gets the answer
//     through natural language without a CLI alt-tab.
//
// All v0 tools are read-only. Action verbs (approve, retry, cancel)
// stay in the CLI / SPA / GitHub-side approval surfaces; the agent
// can articulate proposed actions, humans take them. A future v0.x
// or v1 may add write-side tools — out of scope here.
//
// Auth shape: FISHHAWK_API_TOKEN + FISHHAWK_BACKEND_URL via env, same
// shape as the CLI. For runner-side use the runner will provision a
// scoped, short-lived token at stage-start (E19.8 / future work);
// for interactive use the operator generates a token via the
// existing API-token surface.
//
// Distribution: built as a separate Go binary so operators install
// it via `claude mcp add fishhawk --binary <path>` without pulling
// the rest of the backend's dependency tree. CI builds the binary
// alongside fishhawkd; the release pipeline publishes it (E19.7).
//
// E19.2 / #342 ships handshake-only — empty tool registry. The
// individual tools land in E19.3 (get_active_run), E19.4 (get_plan),
// E19.5 (get_run_status), and E19.6 (list_audit).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	exitOK      = 0
	exitFailure = 1
)

// serverName and serverVersion identify this binary on the MCP
// handshake. Bumped manually as the tool surface evolves; tied to
// the Fishhawk release line rather than the protocol spec version.
const (
	serverName    = "fishhawk-mcp"
	serverVersion = "v0.1.0"
)

func main() {
	os.Exit(run(context.Background(), os.Stderr))
}

// run is the testable entry point. Validates env, builds the MCP
// server, and blocks on the stdio transport until the client
// disconnects. Errors during the loop terminate the process with
// exitFailure — MCP clients restart their server processes, so a
// graceful exit on transport failure is correct.
func run(ctx context.Context, stderr io.Writer) int {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk-mcp: %v\n", err)
		return exitFailure
	}
	srv := buildServer(cfg)
	registerTools(srv, &runResolver{
		api:    newAPIClient(cfg),
		getenv: os.Getenv,
	})
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk-mcp: transport error: %v\n", err)
		return exitFailure
	}
	return exitOK
}

// config captures the validated startup environment. Kept tiny on
// purpose — tools that need additional state read from the same env
// at registration time rather than threading a giant struct.
type config struct {
	backendURL string
	apiToken   string
}

// loadConfig validates the env. Mirrors the CLI's defaults so an
// operator who already has FISHHAWK_BACKEND_URL exported can flip
// between cli and mcp without re-configuring.
//
//   - FISHHAWK_BACKEND_URL: defaults to http://localhost:8080 (same
//     fallback as the CLI). Trailing slashes are stripped to keep
//     URL concatenation safe at request time.
//   - FISHHAWK_API_TOKEN: required, no default. The MCP server has
//     no notion of an anonymous backend (unlike the CLI's dev mode)
//     because every tool round-trips the API; running without auth
//     would be a silent permission bug, not a developer convenience.
func loadConfig(getenv func(string) string) (config, error) {
	c := config{
		backendURL: strings.TrimRight(getenv("FISHHAWK_BACKEND_URL"), "/"),
		apiToken:   getenv("FISHHAWK_API_TOKEN"),
	}
	if c.backendURL == "" {
		c.backendURL = "http://localhost:8080"
	}
	if c.apiToken == "" {
		return config{}, errors.New("FISHHAWK_API_TOKEN is required (generate via the backend's API-token surface)")
	}
	return c, nil
}

// buildServer constructs the MCP server shell without any tools.
// Splitting the constructor out of run + registerTools keeps the
// test surface small — buildServer is the empty server every test
// can start from, registerTools is the part each tool's test
// exercises.
func buildServer(_ config) *mcp.Server {
	return mcp.NewServer(&mcp.Implementation{
		Name:    serverName,
		Version: serverVersion,
	}, nil)
}
