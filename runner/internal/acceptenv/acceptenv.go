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
//   - The model API keys (ANTHROPIC_API_KEY / OPENAI_API_KEY) are the ONE
//     secret class re-admitted — the agent cannot run without its model.
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
package acceptenv

import (
	"sort"
	"strings"
)

// PassthroughPrefix is the operator's explicit channel for target-instance
// credentials: FISHHAWK_ACCEPTANCE_ENV_FOO=bar on the runner env becomes
// FOO=bar on the acceptance invocation env.
const PassthroughPrefix = "FISHHAWK_ACCEPTANCE_ENV_"

// allowExact is the system-essential allow-list (PATH to find the agent
// binary and standard tools, HOME for its config, plus locale/terminal/
// temp essentials). Deliberately NARROWER than gateenv's: the acceptance
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
// decision #2: the model API key is unavoidable).
var modelKeys = map[string]struct{}{
	"ANTHROPIC_API_KEY": {},
	"OPENAI_API_KEY":    {},
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

// Env composes the acceptance invocation environment from the runner's
// base env (os.Environ()) and the running egress proxy's URL. The second
// return value lists passthrough names that were REFUSED because their
// stripped name collides with the deny set — the caller logs them so a
// misconfigured (or hostile) passthrough is loud, never silent.
func Env(base []string, proxyURL string) (env []string, refused []string) {
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
			if isProxyVar(name) {
				// The proxy vars are the containment; a passthrough must not
				// re-point them.
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

	sort.Strings(refused)
	return out, refused
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

// isProxyVar reports whether name is one of the proxy-routing variables
// this package owns.
func isProxyVar(name string) bool {
	switch strings.ToUpper(name) {
	case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
		return true
	}
	return false
}
