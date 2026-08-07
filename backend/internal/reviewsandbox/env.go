// Package reviewsandbox builds the constrained execution environment a local
// review adapter (backend/internal/codex, backend/internal/claudecode) hands to
// its reviewer subprocess: a scrubbed child environment (Env) and an exported,
// read-only source tree (ExportTree).
//
// Both halves exist to bound what a tool-enabled reviewer — which processes
// untrusted diff and issue text — can reach. Env replaces the wholesale
// os.Environ() the adapters used to seed the child (which carried
// FISHHAWKD_DATABASE_URL, GITHUB_TOKEN, and every API key) with an explicitly
// enumerated per-adapter allow-list plus an operator-configured passthrough
// list. ExportTree replaces "point the reviewer at the live checkout" with
// "hand the reviewer a throwaway copy of the tracked files at one commit" — more
// review context (the whole tree, not just the diff) and less exposure (no .git,
// no untracked files, no other branches, no operator-personal config).
//
// The long-form contract — allow-list rationale, the export-not-mount decision,
// every named extractor guard and degrade, and the accepted residual risks —
// lives in this package's README.md.
package reviewsandbox

import "strings"

// BaseAllow is the environment-variable name allow-list every review adapter
// shares. Each entry is present for a specific, documented reason:
//
//   - PATH, HOME, USER, LOGNAME, SHELL — a subprocess needs PATH to resolve its
//     own helpers; HOME/USER/LOGNAME/SHELL are read by CLIs to locate host
//     config (e.g. ~/.codex, ~/.claude) and identify the invoking user.
//   - TMPDIR — the CLI writes scratch state; without it a child may write to a
//     surprising default.
//   - TERM, LANG, LC_ALL, TZ — locale/terminal shaping; absent these a CLI can
//     mis-decode output or emit control sequences.
//   - HTTP_PROXY/HTTPS_PROXY/NO_PROXY (and the lowercase forms) — corporate
//     egress: a deployment behind a forward proxy cannot reach the model
//     endpoint without them.
//
// Anything a specific deployment additionally needs — Bedrock/Vertex routing
// variables, an unusual CA-bundle variable — is named EXPLICITLY via the
// passthrough knob (FISHHAWKD_REVIEWER_ENV_PASSTHROUGH), never admitted by
// widening this list to a prefix match. Env does exact-name matching only, so a
// secret whose name merely shares a prefix with an allowed name is never leaked.
var BaseAllow = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR",
	"TERM", "LANG", "LC_ALL", "TZ",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
}

// ClaudeAllow is the claudecode adapter's allow-list: BaseAllow plus the
// variables the `claude` CLI reads to reach the model and discover host auth
// config. ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN carry the credential;
// ANTHROPIC_BASE_URL redirects the endpoint (Bedrock/Vertex/proxy deployments);
// CLAUDE_CONFIG_DIR points the CLI at its config/subscription-auth directory.
var ClaudeAllow = extend(BaseAllow,
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CONFIG_DIR",
)

// CodexAllow is the codex adapter's allow-list: BaseAllow plus the variables the
// `codex` CLI reads. OPENAI_API_KEY carries the credential; OPENAI_BASE_URL
// redirects the endpoint; CODEX_HOME points the CLI at its config/ChatGPT-login
// directory.
var CodexAllow = extend(BaseAllow,
	"OPENAI_API_KEY", "OPENAI_BASE_URL", "CODEX_HOME",
)

// extend returns a fresh slice of base followed by extra, so the exported
// allow-list vars never alias BaseAllow's backing array (an append that grew
// BaseAllow in place would corrupt every other list).
func extend(base []string, extra ...string) []string {
	out := make([]string, 0, len(base)+len(extra))
	out = append(out, base...)
	out = append(out, extra...)
	return out
}

// Env filters parent (an os.Environ()-shaped "NAME=value" slice, passed in
// rather than read so the function is testable without mutating process env)
// down to entries whose NAME appears in allow ∪ passthrough. Matching is by
// EXACT variable name — no prefix matching, no wildcards — so a secret named
// e.g. PATH_EXTRA is not admitted by an allowed PATH. An entry with no '='
// (a malformed environ line) is dropped. Input order and last-wins duplicate
// semantics are preserved: the result lists surviving entries in their original
// order, so a later "NAME=v2" still shadows an earlier "NAME=v1" for the child's
// os.Getenv, exactly as it would in the unfiltered parent.
func Env(parent, allow, passthrough []string) []string {
	admitted := make(map[string]struct{}, len(allow)+len(passthrough))
	for _, name := range allow {
		admitted[name] = struct{}{}
	}
	for _, name := range passthrough {
		if name == "" {
			continue
		}
		admitted[name] = struct{}{}
	}

	out := make([]string, 0, len(parent))
	for _, kv := range parent {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		name := kv[:eq]
		if _, ok := admitted[name]; ok {
			out = append(out, kv)
		}
	}
	return out
}
