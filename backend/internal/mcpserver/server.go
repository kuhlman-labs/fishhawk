// Package mcpserver is the Fishhawk MCP tool library, extracted verbatim
// from the fishhawk-mcp command (E66.7 / #2408, ADR-076) so fishhawkd can
// serve the identical registry over its own /mcp route (#2390) without
// re-implementing every tool. The command backend/cmd/fishhawk-mcp keeps
// its import path and build target; it now imports this package and calls
// NewServer to construct the same fully-registered server it built inline
// before.
//
// This is a behaviour-preserving relocation: no route, no new dependency,
// and the tool surface, onboarding instructions, and embedded runbook
// resource are all byte-identical before and after the move. The handshake
// identity (serverName + handshakeVersion) has since moved off a frozen
// manual base onto the stamped build version (#4117).
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
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/mcptoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/version"
)

// serverName identifies this server on the MCP handshake. The version half
// is the stamped build version (handshakeVersion), not a manual constant.
const serverName = "fishhawk-mcp"

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

	// allowedRoots is the package-private mirror of Config.AllowedRoots: the
	// checkout roots every path-taking input must resolve inside over the HTTP
	// transport (E66.63 / #3589). Empty over HTTP = fail closed.
	allowedRoots []string

	// hostLabel resolves this process's host-dispatch host label (#4212).
	// Config.internal wires the lazy, once-per-process processHostLabel; a
	// test-built config{} literal leaves it nil, which is no label (the
	// server's `unknown` host).
	hostLabel func() hostLabelResolution

	// hostCapacity resolves this process's FISHHAWK_HOST_CAPACITY (#4241).
	// Config.internal wires the lazy, once-per-process processHostCapacity, so
	// fishhawkd's /mcp route reads it at the first marker or doctor call and an
	// invalid value fails that marker closed; a test-built config{} literal
	// leaves it nil, which sends no capacity (the server default, 3).
	hostCapacity func() (hostCapacityResolution, error)

	// credentialSource, storedLoginPresent and localHostRungs are the
	// package-private mirrors of the Config fields of the same names (#5227).
	credentialSource   string
	storedLoginPresent func() (bool, error)
	localHostRungs     bool
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

	// AllowedRoots is the operator-configured allow-list of absolute checkout
	// roots every path-taking MCP input (working_dir, spec_file) must resolve
	// inside when this registry is served over an HTTP transport (E66.63 /
	// #3589). Containment compares cleaned, symlink-resolved paths byte-wise
	// with a separator anchor, so `/roots/repo-evil` is not inside
	// `/roots/repo`.
	//
	// It is INERT on the stdio transport: a client-spawned stdio process runs
	// as the caller in the caller's own checkout, so the caller already owns
	// every path it can reach (the operator's option (c)).
	//
	// EMPTY over HTTP is the FAIL-CLOSED posture, not "unrestricted": every
	// path-taking verb is refused path_outside_allowed_roots until at least one
	// root is configured. Operators set it via fishhawkd's
	// --mcp-allowed-roots / FISHHAWKD_MCP_ALLOWED_ROOTS or fishhawk-mcp's
	// --allowed-roots / FISHHAWK_MCP_ALLOWED_ROOTS.
	AllowedRoots []string

	// CredentialSource names WHERE the bearer this registry authenticates with
	// came from (#5227), never the bearer itself. fishhawk-mcp sets
	// env:FISHHAWK_API_TOKEN, stored_login or stored_login_refreshed from its
	// loadConfig ladder; fishhawkd's /mcp route sets mcp_route_request_bearer.
	// fishhawk_doctor's credential rung and fishhawk_approve_plan's
	// credential_source echo it. Empty means the embedder did not say.
	CredentialSource string

	// StoredLoginPresent is the optional PRESENCE-only seam the credential rung
	// uses to tell whether an env token is shadowing a stored login (#5227). It
	// answers whether a usable stored credential exists for this backend URL and
	// must never return or retain the credential itself. It is called lazily,
	// at doctor time, and only when LocalHostRungs is true and the bearer is not
	// run-bound. Nil means presence is not checked.
	StoredLoginPresent func() (bool, error)

	// LocalHostRungs enables fishhawk_doctor's two host-local rungs, credential
	// and model_credential (#5227), and the host-dispatch status overlay on
	// fishhawk_get_campaign_status and fishhawk_get_run_status (#5402), which
	// reads the host dispatcher's status.json. Only the operator's own stdio
	// fishhawk-mcp process sets it (backend/cmd/fishhawk-mcp/main.go). It is
	// false by default and false on fishhawkd's /mcp route, where this
	// process's host, files and credential store are the daemon's, not the
	// caller's. When false, both rungs report not_applicable, the overlay is
	// off, and nothing is exec'd or read. A run-bound bearer (an fhm_ in-runner
	// agent token) also reports not_applicable and gets no overlay, so an agent
	// never learns whether an operator login exists or reads the host's files.
	LocalHostRungs bool
}

// internal bridges the exported Config into the package-private config the
// tool constructors consume, without touching newAPIClient's signature.
func (c Config) internal() config {
	return config{
		backendURL:    c.BackendURL,
		apiToken:      c.APIToken,
		httpTransport: c.HTTPTransport,
		allowedRoots:  c.AllowedRoots,
		hostLabel:     processHostLabel,
		hostCapacity:  processHostCapacity,

		credentialSource:   c.CredentialSource,
		storedLoginPresent: c.StoredLoginPresent,
		localHostRungs:     c.LocalHostRungs,
	}
}

// isRunBoundToken reports whether bearer is a run-bound in-runner agent token
// (the fhm_ prefix mcptoken mints per stage). Only the prefix is read; the
// value is never retained.
func isRunBoundToken(bearer string) bool {
	return strings.HasPrefix(bearer, mcptoken.TokenPrefix)
}

// handshakeVersion returns the version string advertised on the MCP
// handshake: the stamped build version (version.Version — the release
// version for releases, "dev" for scripts/dev builds), suffixed with the
// build's git SHA when one was stamped (e.g. "dev+abc1234-dirty") so an
// operator can tell which commit the connected server was built from. Both
// halves come from scripts/release-ldflags (#4117). serverInfo.version is
// informational in the MCP handshake — clients do not parse or gate on it.
func handshakeVersion(ver, sha string) string {
	if sha == "unknown" || sha == "" {
		return ver
	}
	return ver + "+" + sha
}

// buildServer constructs the MCP server shell without any tools.
// Splitting the constructor out of NewServer + registerTools keeps the
// test surface small — buildServer is the empty server every test
// can start from, registerTools is the part each tool's test
// exercises.
func buildServer(_ config) *mcp.Server {
	return mcp.NewServer(&mcp.Implementation{
		Name:    serverName,
		Version: handshakeVersion(version.Version, version.GitSHA),
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
		allowedRoots:  c.allowedRoots,

		credentialSource:   c.credentialSource,
		storedLoginPresent: c.storedLoginPresent,
		localHostRungs:     c.localHostRungs,
		runBoundToken:      isRunBoundToken(c.apiToken),
	})
	registerOnboardingResources(srv)
	return srv
}
