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
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/mcpserver"
	"github.com/kuhlman-labs/fishhawk/credstore"
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
	cfg, err := loadConfig(os.Getenv, credstore.Load)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk-mcp: %v\n", err)
		return exitFailure
	}

	// newServer builds a fully-registered server through the extracted
	// mcpserver package (E66.7 / #2408); the stdio path uses one instance,
	// the http path calls it per session. Tool registration is identical
	// across both transports and identical to fishhawkd's /mcp route (#2390),
	// which constructs the same server from the same entry point.
	//
	// The Config is TRANSPORT-aware, not binary-aware (#2479): the `--transport
	// http` branch puts THIS binary in the same refusing posture as fishhawkd's
	// route — its process cwd is a long-lived daemon's directory, as wrong for a
	// caller as fishhawkd's — so mcpServerConfig sets HTTPTransport:true there and
	// false on the stdio default. Keying the divergence on the transport (not the
	// binary) is what makes `fishhawk-mcp --transport http` refuse an omitted
	// working_dir too.
	httpTransport := tf.transport == transportHTTP
	newServer := func() *mcp.Server {
		return mcpserver.NewServer(mcpServerConfig(cfg, httpTransport))
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

// mcpServerConfig builds the mcpserver.Config for this binary's transport
// (#2479). HTTPTransport is keyed on the TRANSPORT, not the binary: true for
// `--transport http` (where this process's cwd is a long-lived daemon's
// directory, as wrong for a caller as fishhawkd's /mcp route), false for the
// stdio default (the client-spawned process, whose cwd IS the caller's project).
// Keying on transport is what makes `fishhawk-mcp --transport http` refuse an
// omitted or relative working_dir on the runner-spawning verbs, matching the
// route, while stdio keeps the resolve-to-cwd default.
func mcpServerConfig(cfg config, httpTransport bool) mcpserver.Config {
	return mcpserver.Config{
		BackendURL:    cfg.backendURL,
		APIToken:      cfg.apiToken,
		HTTPTransport: httpTransport,
	}
}

// config captures the validated startup environment. Kept tiny on
// purpose — tools that need additional state read from the same env
// at registration time rather than threading a giant struct.
type config struct {
	backendURL string
	apiToken   string
}

// loadConfig validates the env and resolves the bearer token through
// a three-rung ladder, so an operator can register fishhawk-mcp with
// no secret in any client config. Mirrors the CLI's defaults so an
// operator who already has FISHHAWK_BACKEND_URL exported can flip
// between cli and mcp without re-configuring.
//
//   - FISHHAWK_BACKEND_URL: defaults to http://localhost:8080 (same
//     fallback as the CLI). Trailing slashes are stripped to keep
//     URL concatenation safe at request time AND to match the key
//     the credential store is keyed under (credstore.normalizeURL
//     trims the same way, so the two normalizations agree).
//
// Token resolution ladder:
//
//  1. FISHHAWK_API_TOKEN non-empty → use it and do NOT touch the
//     store at all. An explicit env token always wins, so a stored
//     credential can never shadow it. This is the rung the in-runner
//     agent always hits (the runner stamps the per-run token onto
//     the agent env), so the credstore rung never fires there.
//  2. Env empty → load the credential keyed by the resolved backend
//     URL via loadCred (credstore.Load in production; injected in
//     tests to keep the table hermetic and parallel-safe).
//  3. Nothing usable → return an error, NEVER an empty token. Every
//     not-usable case (no credential stored, a stored-but-empty
//     token, an expired credential, a corrupt/unreadable store) is a
//     distinct, precisely-worded STARTUP failure naming both the
//     backend URL that was looked up and `fishhawk token login`, so
//     a mis-registration fails loudly at startup instead of
//     degrading to an empty bearer that becomes a mid-session 401
//     storm. A NIL ExpiresAt means non-expiring and IS accepted —
//     v0 tokens do not expire (cli/README.md, E39.3 / #1708), so
//     reading nil as expired would refuse every field credential.
func loadConfig(getenv func(string) string, loadCred func(string) (credstore.Credential, error)) (config, error) {
	c := config{
		backendURL: strings.TrimRight(getenv("FISHHAWK_BACKEND_URL"), "/"),
		apiToken:   getenv("FISHHAWK_API_TOKEN"),
	}
	if c.backendURL == "" {
		c.backendURL = "http://localhost:8080"
	}
	// Rung 1: an explicit env token wins outright; the store is never
	// consulted.
	if c.apiToken != "" {
		return c, nil
	}

	// Rung 2: fall back to the credential stored for this backend URL.
	// The URL default + trailing-slash trim above are applied BEFORE
	// the lookup so the key matches what `fishhawk token login
	// --backend-url <url>` stored.
	cred, err := loadCred(c.backendURL)
	if err != nil {
		if errors.Is(err, credstore.ErrNotFound) {
			return config{}, fmt.Errorf("no Fishhawk credential stored for %s: run `fishhawk token login --backend-url %s`, or set FISHHAWK_API_TOKEN", c.backendURL, c.backendURL)
		}
		// Rung 3d: a corrupt or unreadable store (credstore.List wraps
		// the read/parse error with the file path). Surface it wrapped,
		// worded distinctly from not-found, so a corrupt store is loud
		// rather than silently re-read as "no credential".
		return config{}, fmt.Errorf("cannot read the Fishhawk credential store for %s: %w; fix or remove the store, then run `fishhawk token login --backend-url %s`, or set FISHHAWK_API_TOKEN", c.backendURL, err, c.backendURL)
	}
	// Rung 3b: a credential exists but carries no token (a truncated
	// store) — never let that become a silent empty bearer.
	if cred.Token == "" {
		return config{}, fmt.Errorf("the stored Fishhawk credential for %s has an empty token: re-run `fishhawk token login --backend-url %s`, or set FISHHAWK_API_TOKEN", c.backendURL, c.backendURL)
	}
	// Rung 3c: an expired credential. A nil ExpiresAt means non-
	// expiring (v0 tokens do not expire) and is accepted.
	if cred.ExpiresAt != nil && cred.ExpiresAt.Before(time.Now()) {
		return config{}, fmt.Errorf("the stored Fishhawk credential for %s expired at %s: re-run `fishhawk token login --backend-url %s`, or set FISHHAWK_API_TOKEN", c.backendURL, cred.ExpiresAt.Format(time.RFC3339), c.backendURL)
	}
	c.apiToken = cred.Token
	return c, nil
}
