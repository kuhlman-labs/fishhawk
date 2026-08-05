// Package mcpserver is the Fishhawk MCP tool library, extracted verbatim
// from the fishhawk-mcp command (E66.7 / #2408, ADR-076) so fishhawkd can
// serve the identical registry over its own /mcp route (#2390) without
// re-implementing every tool. The command backend/cmd/fishhawk-mcp keeps
// its import path and build target; it now imports this package and calls
// NewServer to construct the same fully-registered server it built inline
// before.
//
// This is a behaviour-preserving relocation: no route, no new dependency,
// and the handshake identity (serverName/serverVersion/GitSHA), tool
// surface, onboarding instructions, and embedded runbook resource are all
// byte-identical before and after the move.
//
// The package's exported surface is intentionally large — 232 identifiers.
// Only Config, NewServer, and Instructions are the intended entry points
// (#2390 needs only Config and NewServer). The rest are the tool I/O
// structs: in `package main` their exportedness was cosmetic, but the MCP
// SDK's jsonschema reflection requires the request/response fields to be
// EXPORTED to build each tool's input/output schema, so unexporting them
// would break tool registration. They are grandfathered by
// export_surface_test.go's baseline rather than unexported. See the package
// README for the long-form contract.
package mcpserver

import (
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/version"
)

// serverName and serverVersion identify this binary on the MCP
// handshake. Bumped manually as the tool surface evolves; tied to
// the Fishhawk release line rather than the protocol spec version.
const (
	serverName    = "fishhawk-mcp"
	serverVersion = "v0.1.0"
)

// Instructions is the server `instructions` field advertised on the MCP
// initialize handshake — the intended public alias of the package-private
// onboardingInstructions const declared in onboarding.go. Exposed so
// fishhawkd's /mcp route (#2390) can advertise the identical, byte-for-byte
// instructions string this command does.
const Instructions = onboardingInstructions

// config captures the validated startup environment. Kept tiny on
// purpose — tools that need additional state read from the same env
// at registration time rather than threading a giant struct. Retained
// as the package-private type the moved tool files and their test
// literals already reference; Config is the exported bridge into it.
type config struct {
	backendURL string
	apiToken   string

	// httpTransport marks the tool registry as served over an HTTP
	// transport (fishhawkd's /mcp route, or `fishhawk-mcp --transport
	// http`). Package-private mirror of Config.HTTPTransport; threaded onto
	// the runResolver so the runner-spawning verbs refuse an omitted or
	// relative working_dir in that posture (#2479).
	httpTransport bool
}

// Config is the exported construction input for NewServer. Its two fields
// are the entire startup contract fishhawkd needs to build the tool
// registry: the backend base URL every tool calls, and the bearer token
// the apiClient authenticates with. Kept separate from the unexported
// config rather than renaming it, so the fourteen moved test files that
// build config{...} literals with the unexported field names stay
// byte-identical apart from their package clause.
type Config struct {
	BackendURL string
	APIToken   string

	// HTTPTransport is the TRANSPORT signal (not a binary signal): true
	// whenever this tool registry is served over an HTTP transport —
	// fishhawkd's /mcp route (ADR-076 / #2390) or `fishhawk-mcp --transport
	// http`. In that posture the serving process's cwd is a long-lived
	// daemon's checkout that can NEVER be a caller's intent, so the
	// runner-spawning verbs (dispatch_stage, run_stage, run_children,
	// drive_run) refuse an omitted or relative working_dir and require an
	// absolute path instead (#2479). On the stdio transport (the client-
	// spawned process) it stays false: the process cwd is the caller's own
	// project directory, so an omitted working_dir resolves to it.
	HTTPTransport bool
}

// internal bridges the exported Config into the package-private config the
// tool constructors consume, without touching newAPIClient's signature.
func (c Config) internal() config {
	return config{backendURL: c.BackendURL, apiToken: c.APIToken, httpTransport: c.HTTPTransport}
}

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

// buildServer constructs the MCP server shell without any tools.
// Splitting the constructor out of NewServer + registerTools keeps the
// test surface small — buildServer is the empty server every test
// can start from, registerTools is the part each tool's test
// exercises.
func buildServer(_ config) *mcp.Server {
	return mcp.NewServer(&mcp.Implementation{
		Name:    serverName,
		Version: handshakeVersion(version.GitSHA),
	}, &mcp.ServerOptions{Instructions: onboardingInstructions})
}

// NewServer builds a fully-registered Fishhawk MCP server: the empty
// server shell, every fishhawk_* tool, and the onboarding runbook
// resource. It is the single cross-package entry point the command (and,
// in #2390, fishhawkd's /mcp route) uses; the body is exactly the inline
// newServer closure the command carried before the extraction.
func NewServer(cfg Config) *mcp.Server {
	c := cfg.internal()
	srv := buildServer(c)
	registerTools(srv, &runResolver{
		api:           newAPIClient(c),
		getenv:        os.Getenv,
		httpTransport: c.httpTransport,
	})
	registerOnboardingResources(srv)
	return srv
}
