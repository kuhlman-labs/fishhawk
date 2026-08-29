// Package agentenv composes the environment for the NON-acceptance agent
// invocation — the plan, implement and review spawns (#2894).
//
// It is the third and last member of a family. gateenv.go (ADR-029 / #650
// item 4) composes the env for gate subprocesses; runner/internal/acceptenv
// (ADR-050 / #1535) composes it for the acceptance agent. Until this package
// existed the implement agent was the one subprocess class with NO allow-list:
// its adapter left Invocation.BaseEnv nil, which os/exec defines as
// inherit-parent-env, so the agent saw the runner's whole os.Environ() —
// including the ambient operator bearer FISHHAWK_API_TOKEN, the GitHub App
// installation token, and every other secret the runner happened to carry.
// This package closes that asymmetry with the same posture the other two use:
//
//   - DEFAULT-DENY allow-list. An entry survives only if its key is
//     explicitly recognized. A secret env var added to the runner LATER is
//     dropped automatically, by omission rather than by remembering to
//     denylist it.
//   - An explicit known-secret DENYLIST layered on top, applied BEFORE the
//     allow-list. Belt-and-suspenders against a future widened allow-rule,
//     and — load-bearing, not merely defensive — the layer that makes a
//     passthrough name REFUSABLE.
//   - An operator passthrough channel, FISHHAWK_AGENT_ENV_<NAME>: the
//     operator re-admits exactly one variable by declaring it deliberately.
//     The prefix is stripped, so the agent sees <NAME>. A stripped name that
//     collides with the denylist is REFUSED and reported, never honored.
//
// The agent still receives the two credentials it legitimately needs, and
// both arrive as OVERLAYS applied on top of this base by the adapter, not
// from the ambient environment: the run-bound MCP token (main.go sets
// Invocation.Env["FISHHAWK_API_TOKEN"] to the freshly minted fhm_ bearer,
// plus FISHHAWK_BACKEND_URL) and the model API key (the adapter appends
// Invocation/Invoker APIKey via agent.AppendEnvOverride). Both routes use
// AppendEnvOverride, which strips any same-named entry before appending, so
// the overlay deterministically wins over anything the base might carry.
package agentenv

import (
	"sort"
	"strings"
)

// PassthroughPrefix is the operator's explicit re-admission channel:
// FISHHAWK_AGENT_ENV_FOO=bar on the runner env becomes FOO=bar on the agent
// invocation env. It is checked BEFORE the FISHHAWK_ deny prefix, so the
// channel keeps working while every other FISHHAWK_* var stays out.
const PassthroughPrefix = "FISHHAWK_AGENT_ENV_"

// allowExact is the system-essential + tool-client allow-list, keyed to what
// the implement agent actually drives: the repo's Go toolchain, git, a
// Node-based agent CLI, and Docker-backed testcontainers.
//
// The Docker client vars are admitted by EXACT name rather than a DOCKER_
// prefix on purpose — a prefix would also admit DOCKER_AUTH_CONFIG (a
// registry credential blob) and DOCKER_PASSWORD. CI is admitted because
// scripts/test's timescale auto-5x keys off it (backend/internal/timescale).
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
	"CC":      {},
	"CXX":     {},
	"CI":      {},

	"HTTP_PROXY":  {},
	"HTTPS_PROXY": {},
	"NO_PROXY":    {},
	"ALL_PROXY":   {},
	"http_proxy":  {},
	"https_proxy": {},
	"no_proxy":    {},
	"all_proxy":   {},

	"DOCKER_HOST":        {},
	"DOCKER_CONTEXT":     {},
	"DOCKER_CONFIG":      {},
	"DOCKER_CERT_PATH":   {},
	"DOCKER_TLS_VERIFY":  {},
	"DOCKER_API_VERSION": {},
}

// allowGo is the explicit Go toolchain/runtime name set. It is DUPLICATED
// from gateEnvAllowGo in runner/cmd/fishhawk-runner/gateenv.go rather than
// shared: this package cannot import package main, and hoisting the set out
// of gateenv.go would break the source-reading TestGateEnvListsMatchCLICopy
// lockstep with cli/cmd/fishhawk/doctor_verify.go's copy. The duplication is
// pinned by TestAgentEnvNotNarrowerThanGateEnv in the runner cmd package,
// which asserts Allowed() for every gateEnvAllowExact/gateEnvAllowGo key — so
// drift fails a test instead of silently narrowing the agent's toolchain env.
//
// It is deliberately an explicit NAME set and NOT a bare "GO" prefix: that
// prefix also admitted GOOGLE_API_KEY / GOOGLE_APPLICATION_CREDENTIALS and
// every other GOOGLE_* value (#2504).
var allowGo = map[string]struct{}{
	"GO111MODULE":         {},
	"GO386":               {},
	"GOAMD64":             {},
	"GOARCH":              {},
	"GOARM":               {},
	"GOARM64":             {},
	"GOAUTH":              {},
	"GOBIN":               {},
	"GOCACHE":             {},
	"GOCACHEPROG":         {},
	"GOCOVERDIR":          {},
	"GODEBUG":             {},
	"GOENV":               {},
	"GOEXE":               {},
	"GOEXPERIMENT":        {},
	"GOFIPS140":           {},
	"GOFLAGS":             {},
	"GOGC":                {},
	"GOGCCFLAGS":          {},
	"GOHOSTARCH":          {},
	"GOHOSTOS":            {},
	"GOINSECURE":          {},
	"GOLANGCI_LINT_CACHE": {},
	"GOMAXPROCS":          {},
	"GOMEMLIMIT":          {},
	"GOMIPS":              {},
	"GOMIPS64":            {},
	"GOMOD":               {},
	"GOMODCACHE":          {},
	"GONOPROXY":           {},
	"GONOSUMCHECK":        {},
	"GONOSUMDB":           {},
	"GOOS":                {},
	"GOPATH":              {},
	"GOPPC64":             {},
	"GOPRIVATE":           {},
	"GOPROXY":             {},
	"GORISCV64":           {},
	"GOROOT":              {},
	"GOSUMDB":             {},
	"GOTELEMETRY":         {},
	"GOTELEMETRYDIR":      {},
	"GOTMPDIR":            {},
	"GOTOOLCHAIN":         {},
	"GOTOOLDIR":           {},
	"GOTRACEBACK":         {},
	"GOVCS":               {},
	"GOVERSION":           {},
	"GOWASM":              {},
	"GOWORK":              {},
	"GO_EXTLINK_ENABLED":  {},
}

// allowPrefix lists key prefixes admitted wholesale: locale (LC_), cgo
// (CGO_), the freedesktop base directories the agent CLI and its cache use
// (XDG_), the Node/npm runtime the agent CLI runs on (NODE_, NPM_CONFIG_,
// npm_config_), the testcontainers client knobs the repo's own test loop sets
// (TESTCONTAINERS_), the OpenSSL/Go CA-bundle overrides (SSL_CERT_), and the
// model/agent-CLI configuration family (ANTHROPIC_, OPENAI_, CLAUDE_ —
// gateway base URLs, alternate auth tokens, CLI knobs).
//
// That last family is the ONE class deliberately re-admitted, exactly as
// acceptenv re-admits the model keys: the agent cannot reach its model
// without it. The RAW keys ANTHROPIC_API_KEY / OPENAI_API_KEY are still
// stripped from the base by denyExact below — the adapter re-injects the key
// as an overlay, so the adapter is the single injection point.
var allowPrefix = []string{
	"LC_",
	"CGO_",
	"XDG_",
	"NODE_",
	"NPM_CONFIG_",
	"npm_config_",
	"TESTCONTAINERS_",
	"SSL_CERT_",
	"ANTHROPIC_",
	"OPENAI_",
	"CLAUDE_",
}

// denyExact is the explicit known-secret denylist, applied BEFORE the
// allow-list and consulted by the passthrough branch.
//
// ANTHROPIC_API_KEY / OPENAI_API_KEY are denied from the BASE, which is NOT
// withholding the model key from the agent. ORDERING, stated explicitly
// because the deny depends on it: the runner reads the model key from its OWN
// ambient process environment via apiKeyForAgent (os.Getenv) at
// invoker-selection time; this package filters a SNAPSHOT (os.Environ()) and
// never mutates the runner's environment, so apiKeyForAgent still resolves the
// ambient key regardless of when it runs relative to this filter. The adapter
// then re-appends it onto the composed child env through
// agent.AppendEnvOverride, making the adapter the SINGLE injection point for
// the model credential instead of ambient inheritance.
//
// SSH_AUTH_SOCK is an authority handle to the operator's SSH agent; the
// runner, not the agent, performs the run-branch push, so the agent has no
// need for it.
var denyExact = map[string]struct{}{
	"FISHHAWK_API_TOKEN":    {},
	"FISHHAWK_GITHUB_TOKEN": {},
	"FISHHAWK_GITLAB_TOKEN": {},
	"GITHUB_TOKEN":          {},
	"GH_TOKEN":              {},
	"ANTHROPIC_API_KEY":     {},
	"OPENAI_API_KEY":        {},
	"NPM_TOKEN":             {},
	"DOCKER_AUTH_CONFIG":    {},
	"SSH_AUTH_SOCK":         {},
}

// denyPrefix lists key prefixes dropped unconditionally.
//
// FISHHAWK_ is checked AFTER the passthrough branch, so FISHHAWK_AGENT_ENV_*
// still works. It also keeps the FISHHAWK_TEST_* / FISHHAWK_SKIP_PATCH_COVERAGE
// dev knobs out of the agent's reach, matching the runner's existing gate-env
// posture in which no FISHHAWK_* var reaches the in-loop coverage gate
// precisely so an agent cannot disable it.
//
// GOOGLE_ mirrors gateEnvDenyPrefix (#2504); AWS_ and AZURE_ are the sibling
// cloud-credential families. A deployment routing the agent through Bedrock or
// Vertex CANNOT recover its cloud credential through the passthrough: Env
// applies Denied to the STRIPPED passthrough name, which lands right back on
// these deny rules, so FISHHAWK_AGENT_ENV_AWS_BEARER_TOKEN_BEDROCK is refused
// and logged rather than honored (TestEnv_PassthroughDeniedCollisionRefused).
// Bedrock/Vertex routing is therefore unsupported under this policy until these
// deny rules are narrowed here in code.
var denyPrefix = []string{"FISHHAWK_", "GOOGLE_", "AWS_", "AZURE_"}

// Env composes the agent invocation environment from base (the runner's
// os.Environ(), a slice of "KEY=value" entries). The second return value lists
// passthrough names REFUSED because their stripped name collides with the
// denylist — the caller logs them so a misconfigured (or hostile) passthrough
// is loud, never silent. refused is sorted for determinism.
//
// The returned slice is always non-nil, including when every entry is dropped:
// os/exec treats a nil Cmd.Env as inherit-parent-env, the opposite of
// default-deny.
func Env(base []string) (env []string, refused []string) {
	out := make([]string, 0, len(base))
	for _, kv := range base {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			// No '=' (malformed) or an empty key — not a usable
			// assignment; drop.
			continue
		}
		key, val := kv[:eq], kv[eq+1:]

		if name, ok := strings.CutPrefix(key, PassthroughPrefix); ok {
			if name == "" {
				// Bare FISHHAWK_AGENT_ENV_= — nothing to re-admit.
				continue
			}
			if Denied(name) {
				refused = append(refused, name)
				continue
			}
			out = append(out, name+"="+val)
			continue
		}

		if Denied(key) {
			continue
		}
		if Allowed(key) {
			// Kept byte-identical: the value is never rewritten, so a
			// value containing '=' survives verbatim.
			out = append(out, kv)
		}
	}

	sort.Strings(refused)
	return out, refused
}

// Allowed reports whether key survives the default-deny allow-list. It does
// NOT consult the denylist — that is a separate layer Env applies first,
// mirroring gateEnvAllowed — so a test that restores a broadening allow-rule
// turns red on this predicate alone rather than being masked by the deny.
func Allowed(key string) bool {
	if _, ok := allowExact[key]; ok {
		return true
	}
	if _, ok := allowGo[key]; ok {
		return true
	}
	for _, p := range allowPrefix {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// Denied reports whether key is on the known-secret denylist: an exact match
// (denyExact) or a denied prefix (denyPrefix). Env applies it BEFORE the
// allow-list, and the passthrough branch applies it to the STRIPPED name, so
// a denied key is dropped regardless of any allow-rule and cannot be smuggled
// back in through the operator channel.
func Denied(key string) bool {
	if _, ok := denyExact[key]; ok {
		return true
	}
	for _, p := range denyPrefix {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}
