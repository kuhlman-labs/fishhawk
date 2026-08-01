// Package mcpserver is the importable MCP tool library extracted from the
// fishhawk-mcp binary (E66.7 / #2408). It builds a fully-registered MCP
// server — the same tool registry, onboarding instructions, and runbook
// resource the stdio binary serves — behind one exported constructor,
// NewServer, so the fishhawkd /mcp route (#2390) can serve the identical
// registry instead of forking the tool list.
//
// The exported surface is deliberately narrow: Config (BackendURL,
// APIToken), NewServer, and Instructions. Everything else — the tool
// handlers, the API client, runResolver, the registration helpers — stays
// unexported. An export-surface AST test (export_surface_test.go) pins
// that boundary, so an accidental export from a bulk package-clause
// rewrite fails loudly.
//
// All v0 tools are read-only reads plus the operator action verbs; the CLI
// concerns (flag parsing, the credstore token ladder) stay in the binary's
// main package. NewServer takes the resolved Config; loadConfig lives in
// the cmd binary.
package mcpserver

import (
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/version"
)

// serverName and serverVersion identify this server on the MCP handshake.
// Bumped manually as the tool surface evolves; tied to the Fishhawk
// release line rather than the protocol spec version.
const (
	serverName    = "fishhawk-mcp"
	serverVersion = "v0.1.0"
)

// Config captures the validated startup environment NewServer needs.
// Kept tiny on purpose — tools that need additional state read from the
// same env at registration time rather than threading a giant struct.
// The CLI binary resolves it (env + credstore ladder) and hands it in.
type Config struct {
	BackendURL string
	APIToken   string
}

// NewServer builds a fully-registered MCP server: the empty server shell
// (buildServer) plus the full tool registry (registerTools) and the
// onboarding runbook resource (registerOnboardingResources). The stdio
// transport uses one instance; the streamable-HTTP transport calls it per
// session. Tool registration is identical across both transports, and —
// after #2390 — across fishhawkd's /mcp route.
func NewServer(cfg Config) *mcp.Server {
	srv := buildServer(cfg)
	registerTools(srv, &runResolver{
		api:    newAPIClient(cfg),
		getenv: os.Getenv,
	})
	registerOnboardingResources(srv)
	return srv
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
// test surface small — buildServer is the empty server every test can
// start from, registerTools is the part each tool's test exercises.
// It stays unexported: NewServer is the only exported constructor.
func buildServer(_ Config) *mcp.Server {
	return mcp.NewServer(&mcp.Implementation{
		Name:    serverName,
		Version: handshakeVersion(version.GitSHA),
	}, &mcp.ServerOptions{Instructions: Instructions})
}
