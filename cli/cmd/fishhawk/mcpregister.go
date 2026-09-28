package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// mcpHTTPRegistrationName is the name `fishhawk init` registers the
// backend's HTTP MCP surface under — the same `fishhawk-http` name the
// doctor `MCP registered` rung and docs/mcp/install.md use.
const mcpHTTPRegistrationName = "fishhawk-http"

// mcpGetwd is the os.Getwd seam for registerMCP's cwd check.
var mcpGetwd = os.Getwd

// claudeMCPAddArgs is the Claude Code HTTP-transport registration argv.
func claudeMCPAddArgs(backendURL string) []string {
	return []string{"mcp", "add", "--transport", "http", mcpHTTPRegistrationName, mcpEndpoint(backendURL)}
}

// codexMCPAddArgs is the Codex CLI streamable-HTTP registration argv
// (`codex mcp add <name> --url <url>`).
func codexMCPAddArgs(backendURL string) []string {
	return []string{"mcp", "add", mcpHTTPRegistrationName, "--url", mcpEndpoint(backendURL)}
}

func mcpEndpoint(backendURL string) string {
	return strings.TrimRight(backendURL, "/") + "/mcp"
}

// shellSafeWord is the character set a word carries through a shell with no
// quoting at all. Everything else — a space, a quote, and every
// metacharacter — must be quoted.
var shellSafeWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellQuote renders s as exactly ONE shell word. The commands registerMCP
// prints are meant to be COPIED INTO A SHELL, so a value it interpolates —
// the repository root from --working-dir, the backend URL, the registration
// name — must survive that paste as the literal value. An ordinary directory
// name holding a space (`/tmp/my repo`) would otherwise be split into two
// arguments, and one holding a metacharacter (`$(...)`, `;`, a backtick)
// would turn the printed line into unintended execution. POSIX single quotes
// suppress every expansion; an embedded `'` is closed, escaped and reopened.
func shellQuote(s string) string {
	if s != "" && shellSafeWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellCommand renders name plus args as a copy-pasteable command line,
// quoting every word so argument boundaries survive the paste.
func shellCommand(name string, args []string) string {
	out := make([]string, 0, len(args)+1)
	out = append(out, shellQuote(name))
	for _, a := range args {
		out = append(out, shellQuote(a))
	}
	return strings.Join(out, " ")
}

// registerMCP registers the Fishhawk MCP server with a detected agent CLI
// over the HTTP transport (#2390: HTTP first — it needs no extra binary; the
// stdio shim stays the documented fallback in docs/mcp/install.md). Every
// subprocess goes through the doctorRunOutput seam. It NEVER fails init: in
// every branch it prints the exact registration command, and a failed add is
// a warning. The ladder:
//
//  1. --skip-mcp-register — print the command, run nothing.
//  2. `claude` present, cwd is not the scaffolded repo root — print the
//     command to run FROM the root, run nothing: Claude Code's default
//     `local` scope is keyed to the directory the add runs in, so an add
//     from elsewhere would register the wrong project. This check comes
//     BEFORE the already-registered probes on purpose: `claude mcp list`
//     answers for the CURRENT directory's scope, so a match seen from
//     somewhere else would report the TARGET repository as registered when
//     nothing about the target was inspected.
//  3. `claude` present, fishhawk already registered for THIS directory
//     (`claude mcp list`, then the `claude mcp get` fallback) — report it,
//     run no add.
//  4. `claude` present — run `claude mcp add --transport http ...`; a
//     non-zero exit is a warning naming the exit and the command (the flag
//     shape is Claude-Code-version dependent).
//  5. only `codex` present — print the Codex command, run nothing.
//  6. neither — print the Claude Code command, run nothing.
func registerMCP(w io.Writer, root, backendURL string, skip bool) {
	claudeCmd := shellCommand("claude", claudeMCPAddArgs(backendURL))
	_, _ = fmt.Fprintln(w, "")
	_, _ = fmt.Fprintln(w, "MCP registration:")
	if skip {
		_, _ = fmt.Fprintf(w, "  skipped (--skip-mcp-register); to register by hand run:\n    %s\n", claudeCmd)
		return
	}

	_, claudeErr := doctorRunOutput("claude", "--version")
	if claudeErr != nil {
		if _, codexErr := doctorRunOutput("codex", "--version"); codexErr == nil {
			_, _ = fmt.Fprintf(w, "  Codex CLI detected (Claude Code not found); register by running:\n    %s\n",
				shellCommand("codex", codexMCPAddArgs(backendURL)))
			return
		}
		_, _ = fmt.Fprintf(w, "  no supported agent CLI found on PATH (claude, codex); register by running:\n    %s\n", claudeCmd)
		return
	}

	if !sameDir(mcpGetwd, root) {
		_, _ = fmt.Fprintf(w, "  not registered here: Claude Code's default scope is keyed to the current directory, "+
			"which is not %s (so a registration listed here says nothing about that repository); register from there by running:\n    cd %s && %s\n",
			root, shellQuote(root), claudeCmd)
		return
	}

	if out, listErr := doctorRunOutput("claude", "mcp", "list"); listErr == nil {
		if detail, ok := matchMCPRegistration(out, backendURL); ok {
			_, _ = fmt.Fprintf(w, "  already registered: %s (no change); the registration command is:\n    %s\n", detail, claudeCmd)
			return
		}
	}
	if detail, ok := mcpGetFallback(); ok {
		_, _ = fmt.Fprintf(w, "  already %s (no change); the registration command is:\n    %s\n", detail, claudeCmd)
		return
	}

	if _, err := doctorRunOutput("claude", claudeMCPAddArgs(backendURL)...); err != nil {
		_, _ = fmt.Fprintf(w, "  warn: `%s` failed (%v); run it by hand "+
			"(`claude mcp add --help` is authoritative for your Claude Code version; see docs/mcp/install.md):\n    %s\n",
			claudeCmd, err, claudeCmd)
		return
	}
	_, _ = fmt.Fprintf(w, "  registered %s with Claude Code:\n    %s\n", mcpHTTPRegistrationName, claudeCmd)
}

// sameDir reports whether the process cwd resolves to dir. Any resolution
// error answers false, which routes registerMCP to print-only.
func sameDir(getwd func() (string, error), dir string) bool {
	cwd, err := getwd()
	if err != nil {
		return false
	}
	a, errA := filepath.EvalSymlinks(cwd)
	b, errB := filepath.EvalSymlinks(dir)
	return errA == nil && errB == nil && a == b
}
