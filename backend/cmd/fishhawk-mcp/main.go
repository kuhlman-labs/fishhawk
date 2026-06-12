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
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/version"
)

const (
	exitOK      = 0
	exitFailure = 1
)

// Transport selection. stdio is the default and keeps every existing
// per-client subprocess consumer (Claude Code, Codex) unchanged; http
// is the opt-in loopback-only streamable-HTTP transport (#927).
const (
	transportStdio = "stdio"
	transportHTTP  = "http"

	// defaultHTTPAddr is loopback by default so a bare --transport http
	// never exposes the endpoint off-host. Overridable via --addr; a
	// non-loopback value is rejected before binding (validateLoopbackAddr).
	defaultHTTPAddr = "127.0.0.1:8765"
)

// serverName and serverVersion identify this binary on the MCP
// handshake. Bumped manually as the tool surface evolves; tied to
// the Fishhawk release line rather than the protocol spec version.
const (
	serverName    = "fishhawk-mcp"
	serverVersion = "v0.1.0"
)

// handshakeVersion returns the version string advertised on the MCP
// handshake: the manually-bumped serverVersion base, suffixed with the
// build's git SHA when one was stamped (e.g. "v0.1.0+abc1234-dirty") so
// an operator can tell which commit the connected server was built from.
// serverInfo.version is informational in the MCP handshake — clients do
// not parse or gate on it.
func handshakeVersion(sha string) string {
	if sha == "unknown" || sha == "" {
		return serverVersion
	}
	return serverVersion + "+" + sha
}

func main() {
	os.Exit(run(context.Background(), os.Args, os.Stderr))
}

// transportFlags captures the parsed CLI flags governing the transport.
// Flags govern transport selection; env (loadConfig) still governs the
// backend URL + token.
type transportFlags struct {
	transport string
	addr      string
}

// parseFlags parses the transport-selection flags from args[1:] (args
// is os.Args, so args[0] is the program name). An unknown --transport
// value is rejected with a precise error.
func parseFlags(args []string, stderr io.Writer) (transportFlags, error) {
	fs := flag.NewFlagSet("fishhawk-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var tf transportFlags
	fs.StringVar(&tf.transport, "transport", transportStdio, "transport to serve on: stdio|http (http is loopback-only, opt-in — #927)")
	fs.StringVar(&tf.addr, "addr", defaultHTTPAddr, "host:port for --transport http; loopback-only, ignored for stdio")
	if err := fs.Parse(args[1:]); err != nil {
		return transportFlags{}, err
	}
	switch tf.transport {
	case transportStdio, transportHTTP:
	default:
		return transportFlags{}, fmt.Errorf("unknown --transport %q: want stdio or http", tf.transport)
	}
	return tf, nil
}

// run is the testable entry point. Parses flags, validates env, builds
// the MCP server, and blocks on the selected transport until the client
// disconnects (stdio) or ctx is cancelled (http). Errors terminate the
// process with exitFailure — MCP clients restart their server
// processes, so a graceful exit on transport failure is correct.
func run(ctx context.Context, args []string, stderr io.Writer) int {
	tf, err := parseFlags(args, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk-mcp: %v\n", err)
		return exitFailure
	}
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk-mcp: %v\n", err)
		return exitFailure
	}

	// newServer builds a fully-registered server; the stdio path uses one
	// instance, the http path calls it per session. Tool registration is
	// identical across both transports.
	newServer := func() *mcp.Server {
		srv := buildServer(cfg)
		registerTools(srv, &runResolver{
			api:    newAPIClient(cfg),
			getenv: os.Getenv,
		})
		return srv
	}

	switch tf.transport {
	case transportHTTP:
		if err := serveHTTP(ctx, tf.addr, cfg.apiToken, newServer); err != nil {
			_, _ = fmt.Fprintf(stderr, "fishhawk-mcp: transport error: %v\n", err)
			return exitFailure
		}
	default:
		if err := newServer().Run(ctx, &mcp.StdioTransport{}); err != nil {
			_, _ = fmt.Fprintf(stderr, "fishhawk-mcp: transport error: %v\n", err)
			return exitFailure
		}
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
		Version: handshakeVersion(version.GitSHA),
	}, nil)
}
