// Package acceptenv composes the acceptance-agent invocation environment
// (ADR-050 / #1532, decision #2 of the acceptance security posture): the
// minimized credential set that keeps the third lethal-trifecta leg as
// small as the stage can function with.
//
// The posture, mirroring gateenv.go's default-deny discipline (ADR-029
// item 4) but for a DIFFERENT consumer — the acceptance agent process, not
// a gate subprocess:
//
//   - Default-deny allow-list of system essentials. Everything else in the
//     runner's env is dropped, so a secret added to the runner later never
//     leaks into the invocation by omission.
//   - The model credentials (ANTHROPIC_API_KEY / OPENAI_API_KEY, and
//     CLAUDE_CODE_OAUTH_TOKEN — the inference-only token `claude
//     setup-token` mints, E72.40 / #3792) are the ONE secret class
//     re-admitted — the agent cannot run without its model. Admission is
//     independent of credential isolation: it is a model-credential choice,
//     inert when unset.
//   - Customer-supplied target-instance credentials pass through the
//     explicit FISHHAWK_ACCEPTANCE_ENV_<NAME> prefix channel: the operator
//     declares each one deliberately, and the prefix is stripped so the
//     agent sees <NAME>. A passthrough whose stripped name collides with a
//     denied key (e.g. an attempt to smuggle GITHUB_TOKEN back in) is
//     REFUSED, not honored — the deny set outranks the passthrough.
//   - Explicitly denied, never present: FISHHAWK_API_TOKEN (the acceptance
//     agent gets NO MCP/Fishhawk token — its verdict ships via the
//     signature-authed evidence upload), FISHHAWK_GITHUB_TOKEN /
//     FISHHAWK_GITLAB_TOKEN / GITHUB_TOKEN / GH_TOKEN (repo write), and
//     anything deploy-shaped.
//   - HTTP_PROXY / HTTPS_PROXY / ALL_PROXY (upper and lower case) are set
//     to the egress proxy and NO_PROXY is cleared, so every cooperating
//     HTTP client in the invocation routes through the ADR-050 proxy.
//   - FISHHAWK_FORGE_WRITES=deny is injected as a FIXED value (E72.13 /
//     #3500), never copied from the base env: every descendant
//     fishhawk-runner the acceptance agent could spawn inherits the deny
//     and refuses pre-spawn (runner_failed forge_writes_denied, category
//     C) before it can push a branch or open a pull request on the real
//     forge. A passthrough named FISHHAWK_FORGE_WRITES is REFUSED exactly
//     like a proxy-var passthrough — the containment is not re-pointable.
//   - The run-agent marker FISHHAWK_RUN_AGENT (agent.RunAgentEnvVar, #3945)
//     is RUNNER-STAMPED, never operator input: the agent adapter applies it
//     last on every spawn, a base-env value is dropped by the allow-list,
//     and a passthrough named FISHHAWK_RUN_AGENT (any case) is REFUSED like
//     FISHHAWK_FORGE_WRITES.
//   - Credential isolation (E72.40 / #3792) is OPT-IN per call:
//     WithCredentialIsolation makes Env also REFUSE passthroughs that could
//     re-point the agent at the runner host's credential stores — the home
//     family (HOME, XDG_{CONFIG,CACHE,DATA,STATE}_HOME, XDG_RUNTIME_DIR),
//     GH_CONFIG_DIR, CLAUDE_CONFIG_DIR, CODEX_HOME, SSH_AUTH_SOCK and any
//     GIT_CONFIG* name, all in any letter case — and IsolateCredentials then
//     points HOME/XDG/GH_CONFIG_DIR at the runner-created synthetic home and
//     pins GIT_CONFIG_GLOBAL=/dev/null + GIT_CONFIG_NOSYSTEM=1.
//
// Kill-switch contract: the runner passes WithCredentialIsolation and calls
// IsolateCredentials ONLY while FISHHAWK_ACCEPTANCE_CREDENTIAL_ISOLATION is
// not `off`. Without the option those names pass through exactly as they did
// before #3792, so `off` restores the earlier posture; the one stated
// difference is that CLAUDE_CODE_OAUTH_TOKEN stays admitted.
package acceptenv

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// PassthroughPrefix is the operator's explicit channel for target-instance
// credentials: FISHHAWK_ACCEPTANCE_ENV_FOO=bar on the runner env becomes
// FOO=bar on the acceptance invocation env.
const PassthroughPrefix = "FISHHAWK_ACCEPTANCE_ENV_"

// ForgeWritesVar is the env var the runner's pre-spawn forge-writes gate
// reads (E72.13 / #3500); ForgeWritesDeny is the only value it recognizes.
// Env injects ForgeWritesVar=ForgeWritesDeny unconditionally so a runner
// spawned from inside the acceptance sandbox refuses before any forge write.
const (
	ForgeWritesVar  = "FISHHAWK_FORGE_WRITES"
	ForgeWritesDeny = "deny"
)

// allowExact is the system-essential allow-list (PATH to find the agent
// binary and standard tools, HOME for its config — re-pointed at the
// synthetic home by IsolateCredentials when isolation is active — plus
// locale/terminal/temp essentials). Deliberately NARROWER than gateenv's: the acceptance
// agent is not a Go build and gets no toolchain vars.
var allowExact = map[string]struct{}{
	"PATH":    {},
	"HOME":    {},
	"USER":    {},
	"LOGNAME": {},
	"SHELL":   {},
	"TMPDIR":  {},
	"TMP":     {},
	"TEMP":    {},
	"TERM":    {},
	"TZ":      {},
	"LANG":    {},
}

// allowPrefix admits the locale family wholesale.
var allowPrefix = []string{"LC_"}

// modelKeys are the one secret class the invocation keeps (ADR-050
// decision #2: the model API key is unavoidable). CLAUDE_CODE_OAUTH_TOKEN
// is the env-carried Claude Code subscription credential (#3792): with it,
// the agent authenticates without the runner host's keychain.
var modelKeys = map[string]struct{}{
	"ANTHROPIC_API_KEY":       {},
	"CLAUDE_CODE_OAUTH_TOKEN": {},
	"OPENAI_API_KEY":          {},
}

// deny is the belt-and-suspenders explicit denylist. These keys never
// appear on the invocation env — not from the base env (the allow-list
// already drops them) and not via the passthrough channel (refused by
// name).
var deny = map[string]struct{}{
	"FISHHAWK_API_TOKEN":    {},
	"FISHHAWK_GITHUB_TOKEN": {},
	"FISHHAWK_GITLAB_TOKEN": {},
	"GITHUB_TOKEN":          {},
	"GH_TOKEN":              {},
}

// EnvOption adjusts Env's refusal set.
type EnvOption func(*envOptions)

type envOptions struct {
	credentialIsolation bool
}

// WithCredentialIsolation makes Env refuse the passthrough names that could
// re-point the agent at the runner host's credential stores
// (isCredentialLocatorVar). The runner passes it only while credential
// isolation is active; without it those names pass through as before.
func WithCredentialIsolation() EnvOption {
	return func(o *envOptions) { o.credentialIsolation = true }
}

// Env composes the acceptance invocation environment from the runner's
// base env (os.Environ()) and the running egress proxy's URL. The second
// return value lists passthrough names that were REFUSED because their
// stripped name collides with the deny set — the caller logs them so a
// misconfigured (or hostile) passthrough is loud, never silent.
func Env(base []string, proxyURL string, opts ...EnvOption) (env []string, refused []string) {
	var o envOptions
	for _, opt := range opts {
		opt(&o)
	}
	out := make([]string, 0, len(base)+8)
	for _, kv := range base {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		key, val := kv[:eq], kv[eq+1:]

		if name, ok := strings.CutPrefix(key, PassthroughPrefix); ok {
			if name == "" {
				continue
			}
			if _, denied := deny[name]; denied {
				refused = append(refused, name)
				continue
			}
			if isProxyVar(name) || isForgeWritesVar(name) {
				// The proxy vars and the forge-writes deny are the
				// containment; a passthrough must not re-point them.
				refused = append(refused, name)
				continue
			}
			if isRunAgentVar(name) {
				// The run-agent marker is runner-stamped (#3945); an
				// operator passthrough must not supply or re-point it.
				refused = append(refused, name)
				continue
			}
			if o.credentialIsolation && isCredentialLocatorVar(name) {
				// Under isolation the synthetic home and the git pins are
				// the containment; a passthrough must not re-point them
				// at the host's credential stores (#3792).
				refused = append(refused, name)
				continue
			}
			out = append(out, name+"="+val)
			continue
		}

		if _, denied := deny[key]; denied {
			continue
		}
		if _, isModel := modelKeys[key]; isModel {
			out = append(out, kv)
			continue
		}
		if allowed(key) {
			out = append(out, kv)
		}
	}

	// Route every cooperating client through the egress proxy; clear
	// NO_PROXY so nothing opts out.
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		out = append(out, k+"="+proxyURL)
	}
	out = append(out, "NO_PROXY=", "no_proxy=")
	// Deny forge writes to every descendant runner (E72.13 / #3500). Fixed
	// value: a base-env FISHHAWK_FORGE_WRITES is dropped by the allow-list
	// above, so this is the ONLY entry under that name.
	out = append(out, ForgeWritesVar+"="+ForgeWritesDeny)

	sort.Strings(refused)
	return out, refused
}

// HasModelCredential reports whether base carries a non-empty env model
// credential for agentID: OPENAI_API_KEY for codex; ANTHROPIC_API_KEY or
// CLAUDE_CODE_OAUTH_TOKEN for any other agent. A synthetic home hides the
// keychain/file subscription login, so the runner isolates the home only
// when this is true. The last entry for a key wins, as in os/exec.
func HasModelCredential(base []string, agentID string) bool {
	vals := map[string]string{}
	for _, kv := range base {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vals[k] = v
		}
	}
	if agentID == "codex" {
		return vals["OPENAI_API_KEY"] != ""
	}
	return vals["ANTHROPIC_API_KEY"] != "" || vals["CLAUDE_CODE_OAUTH_TOKEN"] != ""
}

// Git config pins IsolateCredentials always applies: no global
// (~/.gitconfig, $XDG_CONFIG_HOME/git/config) and no system gitconfig — the
// latter carries `credential.helper=osxkeychain` on macOS Command Line Tools.
const (
	GitConfigGlobalPin   = "GIT_CONFIG_GLOBAL=/dev/null"
	GitConfigNoSystemPin = "GIT_CONFIG_NOSYSTEM=1"
)

// homeFamily are the variables IsolateCredentials re-points under the
// synthetic home, with the path each gets relative to it.
var homeFamily = []struct{ name, rel string }{
	{"HOME", ""},
	{"XDG_CONFIG_HOME", ".config"},
	{"XDG_CACHE_HOME", ".cache"},
	{"XDG_DATA_HOME", ".local/share"},
	{"XDG_STATE_HOME", ".local/state"},
	{"GH_CONFIG_DIR", ".config/gh"},
}

// IsolateCredentials rewrites an Env-composed env so the agent's config
// lookups miss the runner host's credential stores (E72.40 / #3792). It
// ALWAYS strips every GIT_CONFIG* entry and appends GitConfigGlobalPin and
// GitConfigNoSystemPin. When home != "" it also strips HOME, the
// XDG_{CONFIG,CACHE,DATA,STATE}_HOME dirs and GH_CONFIG_DIR and re-sets
// them under home. It creates nothing: the caller owns home's lifecycle.
func IsolateCredentials(env []string, home string) []string {
	out := make([]string, 0, len(env)+len(homeFamily)+2)
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if isGitConfigVar(key) {
			continue
		}
		if home != "" && inHomeFamily(key) {
			continue
		}
		out = append(out, kv)
	}
	if home != "" {
		for _, v := range homeFamily {
			out = append(out, v.name+"="+filepath.Join(home, v.rel))
		}
	}
	return append(out, GitConfigGlobalPin, GitConfigNoSystemPin)
}

// inHomeFamily reports whether key (any case) is one IsolateCredentials
// re-points under the synthetic home.
func inHomeFamily(key string) bool {
	for _, v := range homeFamily {
		if strings.EqualFold(key, v.name) {
			return true
		}
	}
	return false
}

// credentialLocatorVars are the names (upper case) that locate a credential
// store or agent; refused as passthroughs under WithCredentialIsolation.
var credentialLocatorVars = map[string]struct{}{
	"HOME":              {},
	"XDG_CONFIG_HOME":   {},
	"XDG_CACHE_HOME":    {},
	"XDG_DATA_HOME":     {},
	"XDG_STATE_HOME":    {},
	"XDG_RUNTIME_DIR":   {},
	"GH_CONFIG_DIR":     {},
	"CLAUDE_CONFIG_DIR": {},
	"CODEX_HOME":        {},
	"SSH_AUTH_SOCK":     {},
}

// isCredentialLocatorVar reports whether name (any case) locates a host
// credential store: a credentialLocatorVars entry or any GIT_CONFIG* name.
func isCredentialLocatorVar(name string) bool {
	if _, ok := credentialLocatorVars[strings.ToUpper(name)]; ok {
		return true
	}
	return isGitConfigVar(name)
}

// isGitConfigVar reports whether name (any case) is a GIT_CONFIG* variable
// (GIT_CONFIG_GLOBAL, _SYSTEM, _NOSYSTEM, _COUNT, _KEY_n, _VALUE_n,
// _PARAMETERS, GIT_CONFIG).
func isGitConfigVar(name string) bool {
	return strings.HasPrefix(strings.ToUpper(name), "GIT_CONFIG")
}

// allowed reports whether key survives the default-deny allow-list.
func allowed(key string) bool {
	if _, ok := allowExact[key]; ok {
		return true
	}
	for _, p := range allowPrefix {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// isForgeWritesVar reports whether name is the forge-writes gate variable
// (any case), which a passthrough must not re-point.
func isForgeWritesVar(name string) bool {
	return strings.ToUpper(name) == ForgeWritesVar
}

// isRunAgentVar reports whether name is the runner-stamped run-agent marker
// (any case), which a passthrough must not supply (#3945).
func isRunAgentVar(name string) bool {
	return strings.ToUpper(name) == agent.RunAgentEnvVar
}

// isProxyVar reports whether name is one of the proxy-routing variables
// this package owns.
func isProxyVar(name string) bool {
	switch strings.ToUpper(name) {
	case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
		return true
	}
	return false
}
