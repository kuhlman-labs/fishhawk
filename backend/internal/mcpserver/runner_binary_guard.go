package mcpserver

import "errors"

// runnerBinaryNotAllowedCode is the stable, named error token every
// runner_binary refusal over the HTTP MCP transport carries (E66.64 / #4476).
// Callers (and tests) match on this rather than on the surrounding prose.
const runnerBinaryNotAllowedCode = "runner_binary_not_allowed_over_http"

// errRunnerBinaryNotAllowedOverHTTP is the ONE refusal for a caller-supplied
// runner_binary over the HTTP MCP transport. It names the code and the
// daemon-side remedy, and nothing about the supplied value.
var errRunnerBinaryNotAllowedOverHTTP = errors.New(runnerBinaryNotAllowedCode + ": runner_binary is refused over the HTTP MCP transport; the serving daemon resolves fishhawk-runner only from its own configuration (FISHHAWK_RUNNER_BIN, then a fishhawk-runner beside the serving binary, then PATH). Omit runner_binary, and have the operator set FISHHAWK_RUNNER_BIN in the daemon's environment")

// refuseHTTPRunnerBinary is the ONE shared control closing the HTTP /mcp
// remote-code-execution path (E66.64 / #4476, under ADR-076 / E66): over the
// HTTP transport a write:stages token could otherwise name any host executable
// as runner_binary and have fishhawkd (or `fishhawk-mcp --transport http`)
// spawn it with the daemon's environment. Every runner-spawning tool calls it
// twice — as an early guard before any backend read, marker write or spawn,
// and again inside resolveRunnerBinary, the chokepoint a future spawning tool
// cannot bypass.
//
// The refusal is decided on NON-EMPTINESS ALONE (a whitespace-only value counts
// as supplied). It never stats, LookPaths or echoes the value, so it is not a
// filesystem-existence oracle — the same posture as #3589's
// outsideAllowedRootsRefusal.
//
// STDIO is deliberately unaffected: the stdio process is spawned BY the caller
// and runs as them, so a caller-chosen runner_binary there grants nothing the
// caller does not already hold.
func (r *runResolver) refuseHTTPRunnerBinary(in string) error {
	if !r.httpTransport || in == "" {
		return nil
	}
	return errRunnerBinaryNotAllowedOverHTTP
}
