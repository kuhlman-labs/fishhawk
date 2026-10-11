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
//
// One entry is REWRITTEN rather than filtered: GOFLAGS gets -trimpath merged
// in (#4180). Without it the go command hashes the build directory into every
// workspace-package compile action ID, so each per-run lineage worktree path
// recompiles — and re-caches — packages another worktree at the same base
// already built (the #3901 host build-cache growth). Every other surviving
// entry stays byte-identical.
package agentenv

import (
	"errors"
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
// GOFLAGS is the one entry Env REWRITES: -trimpath is merged into its value as
// the final step (#4180; withTrimpath), so the agent's own go build / go test
// in a lineage worktree stops hashing the worktree path into compile action
// IDs and N worktrees at one base share Go build-cache entries. The merge never
// clobbers: an inherited value (the runner's own GOFLAGS, or a
// FISHHAWK_AGENT_ENV_GOFLAGS passthrough) is kept verbatim and the flag is
// appended only when no field already names it, so an explicit
// -trimpath=false is the operator's opt-out. A value the go command could not
// parse (an unterminated quote) is left untouched. Residual: an env-var
// GOFLAGS shadows a `go env -w GOFLAGS=` file value entirely, so a runner env
// with no GOFLAGS hides the file's; set GOFLAGS in the runner env or through
// the passthrough instead.
//
// The returned slice is always non-nil, including when every entry is dropped:
// os/exec treats a nil Cmd.Env as inherit-parent-env, the opposite of
// default-deny. The GOFLAGS overlay always yields at least one entry, so the
// guarantee holds trivially.
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
	return withTrimpath(out), refused
}

// trimpathFlag is the go build flag merged into the agent's GOFLAGS.
const trimpathFlag = "-trimpath"

// withTrimpath merges -trimpath into every GOFLAGS entry of env, in place and
// keeping each entry's position, and appends GOFLAGS=-trimpath when env has
// none. Every entry is merged, not just the first: os/exec uses the LAST value
// of a duplicated key, and an ambient GOFLAGS and a FISHHAWK_AGENT_ENV_GOFLAGS
// passthrough can both survive composition, so the effective (last) one must
// carry the flag too.
func withTrimpath(env []string) []string {
	const key = "GOFLAGS="
	found := false
	for i, kv := range env {
		if val, ok := strings.CutPrefix(kv, key); ok {
			env[i] = key + mergeTrimpath(val)
			found = true
		}
	}
	if !found {
		env = append(env, key+trimpathFlag)
	}
	return env
}

// mergeTrimpath returns val with -trimpath merged in. A val the go command
// cannot split (an unterminated quote) comes back unchanged, so the go command
// reports the operator's own parse error rather than one this package
// introduced. A val where any field already names the flag comes back
// unchanged: that covers a repeat (no duplicate) and -trimpath=false (the
// operator's opt-out is honored). A blank val becomes exactly -trimpath;
// otherwise the inherited bytes are kept as a verbatim prefix.
func mergeTrimpath(val string) string {
	fields, err := splitGOFLAGS(val)
	if err != nil {
		return val
	}
	for _, f := range fields {
		if namesTrimpath(f) {
			return val
		}
	}
	if len(fields) == 0 {
		return trimpathFlag
	}
	return val + " " + trimpathFlag
}

// splitGOFLAGS splits s into fields exactly as the go command does for GOFLAGS
// (base.InitGOFLAGS calls quoted.Split): a byte-for-byte mirror of go1.25.6
// src/cmd/internal/quoted/quoted.go Split. Fields are separated by space, tab,
// newline or carriage return; a field that STARTS with ' or " runs to the
// matching quote with no unescaping; an unterminated quote is an error. A
// strings.Fields split would misread a quoted field holding spaces, which is
// the difference between "-trimpath" the flag and the text of a -ldflags value.
func splitGOFLAGS(s string) ([]string, error) {
	// Split fields allowing '' or "" around elements.
	// Quotes further inside the string do not count.
	var f []string
	for len(s) > 0 {
		for len(s) > 0 && isSpaceByte(s[0]) {
			s = s[1:]
		}
		if len(s) == 0 {
			break
		}
		// Accepted quoted string. No unescaping inside.
		if s[0] == '"' || s[0] == '\'' {
			quote := s[0]
			s = s[1:]
			i := 0
			for i < len(s) && s[i] != quote {
				i++
			}
			if i >= len(s) {
				return nil, errUnterminatedQuote
			}
			f = append(f, s[:i])
			s = s[i+1:]
			continue
		}
		// Else accept a space-separated field.
		i := 0
		for i < len(s) && !isSpaceByte(s[i]) {
			i++
		}
		f = append(f, s[:i])
		s = s[i:]
	}
	return f, nil
}

var errUnterminatedQuote = errors.New("unterminated quoted string in GOFLAGS")

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// namesTrimpath reports whether a split GOFLAGS field is the -trimpath flag in
// any spelling the go command accepts: -trimpath, --trimpath, or either with
// an =value (so -trimpath=false is recognized as the operator's opt-out).
func namesTrimpath(field string) bool {
	name := strings.TrimPrefix(strings.TrimPrefix(field, "-"), "-")
	return name == "trimpath" || strings.HasPrefix(name, "trimpath=")
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
