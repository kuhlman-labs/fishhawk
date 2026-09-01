package main

import (
	"net/url"
	"os"
	"strings"
)

// ADR-029 (#650) item 4 — gate-subprocess credential stripping.
//
// The compile/test/verify gates execute committed AGENT-AUTHORED code
// (`go vet`, `go test`, and the spec-supplied `sh -c <verifyCmd>`). If those
// children inherited the runner's process environment (os.Environ()), agent
// code would see the runner's secrets: the GitHub App installation token
// (FISHHAWK_GITHUB_TOKEN / GITHUB_TOKEN / GH_TOKEN), the agent API keys
// (ANTHROPIC_API_KEY / OPENAI_API_KEY), and the MCP backend token
// (FISHHAWK_API_TOKEN). That is the lethal-trifecta shape: agent code + network
// + runner credentials. sanitizedGateEnv builds a minimal environment with the
// secrets stripped so no runner credential is visible to gate code.
//
// The policy is DEFAULT-DENY (allow-list), not denylist-only: an entry survives
// only if its key is explicitly recognized as a system essential or a Go
// toolchain variable. A secret env var added to the runner LATER is dropped
// automatically because it will not be on the allow-list — a denylist would
// silently leak any newly-introduced secret until someone remembered to add it.
// The explicit denylist below is belt-and-suspenders: it guarantees the known
// secret keys are dropped even if some future allow-rule would otherwise admit
// one of them.

// gateEnvAllowExact is the set of system-essential variable names the Go
// build/test toolchain (and the spec verify command) needs to run: PATH to
// find `go`/`git`/the C compiler, HOME for the default GOPATH/GOCACHE when
// those are unset, plus the usual locale/terminal/temp essentials.
var gateEnvAllowExact = map[string]struct{}{
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
}

// gateEnvAllowGo is the explicit set of Go toolchain / runtime variable names
// admitted by EXACT match. It replaces an earlier bare "GO" allow-prefix, which
// was broader than the Go namespace it was written for: GOOGLE_API_KEY,
// GOOGLE_APPLICATION_CREDENTIALS, and every other GOOGLE_* value matched it and
// passed through verbatim (the redactGoEnvUserinfo transform only rewrites
// URL-shaped values, so a bare API key was forwarded unchanged) — #2504. The set
// is the union of `go env` output and the GO-prefixed names in
// `go help environment` on the Go toolchain this repo pins, plus the runtime
// knobs the bare prefix used to admit (GOMAXPROCS/GOGC/GOTRACEBACK/GOMEMLIMIT).
// GOLANGCI_LINT_CACHE is admitted too so an operator's inherited linter-cache
// path survives; on the runner withIsolatedLintCache strips any inherited value
// and appends its own AFTER sanitization (#1796), so admitting it here is
// behaviour-neutral. Kept sorted, one per line, and pinned IDENTICAL to
// cli/cmd/fishhawk/doctor_verify.go's verifyEnvAllowGo by
// TestGateEnvListsMatchCLICopy — adding a Go variable means adding it to BOTH.
var gateEnvAllowGo = map[string]struct{}{
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

// gateEnvAllowPrefix lists key prefixes admitted wholesale: every CGO_* var and
// every LC_* locale var. The Go toolchain namespace is NO LONGER a prefix rule
// (see gateEnvAllowGo) — a bare "GO" prefix also admitted GOOGLE_* credentials.
//
// An allowed GO* value can be URL-valued (notably GOPROXY/GOSUMDB), and an
// operator may embed userinfo credentials in that URL (scheme://user:pass@host).
// Those credentials would otherwise be visible to the agent-authored code the
// gates execute, re-opening the #940 gap that #931's allow-list preserved.
// Before a GO* value reaches gate code, sanitizeEnv runs it through
// redactGoEnvUserinfo, which strips embedded userinfo from each credentialed URL
// entry while keeping the proxy host/path so dependency resolution still works.
// Non-URL GO* forms (off, direct, bare host, GOFLAGS, GOPATH, GO111MODULE, …)
// pass through byte-identical.
var gateEnvAllowPrefix = []string{"CGO_", "LC_"}

// gateEnvDeny is the explicit known-secret denylist (belt-and-suspenders on top
// of the default-deny allow-list). These keys are dropped unconditionally.
var gateEnvDeny = map[string]struct{}{
	"FISHHAWK_GITHUB_TOKEN": {},
	"FISHHAWK_GITLAB_TOKEN": {},
	"GITHUB_TOKEN":          {},
	"GH_TOKEN":              {},
	"ANTHROPIC_API_KEY":     {},
	"OPENAI_API_KEY":        {},
	"FISHHAWK_API_TOKEN":    {},
}

// gateEnvDenyPrefix lists key prefixes dropped unconditionally — the
// belt-and-suspenders layer on top of the default-deny allow-list. There is no
// Go toolchain variable named GOOGLE_*, so this entry is unambiguous: it keeps
// GOOGLE_API_KEY / GOOGLE_APPLICATION_CREDENTIALS (and any future GOOGLE_*
// credential) out of gate children even if a later allow-rule re-widens (#2504).
var gateEnvDenyPrefix = []string{"GOOGLE_"}

// gateEnvGitConfigPins neutralizes global and system git config for every gate
// subprocess by pinning both git config-file environment variables at /dev/null
// (git reads an empty configuration from an empty file rather than erroring;
// git-config(1) ENVIRONMENT). It exists because #912 / #3102: a gate child's
// temp-repo `git commit` must not inherit the OPERATOR's global
// commit.gpgsign + gpg.ssh.program, whose signing agent may be unavailable —
// when it is, every such commit fails and the stage is misclassified category B
// (#3102, run 34252f17 / #2929).
//
// Default-deny alone does NOT achieve this: the allow-list loop drops any
// inherited GIT_CONFIG_GLOBAL / GIT_CONFIG_SYSTEM (neither key is on any
// allow-list), but HOME IS allow-listed, so with GIT_CONFIG_GLOBAL unset git
// falls back to $HOME/.gitconfig and picks the operator's settings back up. The
// pin closes that fallback. It is a no-op on CI, where neither config file
// exists, and matches the mitigation scripts/test — the very command the
// runner's verify gate runs — has exported since #912.
//
// It deliberately does NOT affect the runner's OWN git plumbing
// (worktree/rev-parse/reset/commit/push), which keeps the inherited env so push
// credentials still work and never assigns this sanitized slice.
//
// Kept IDENTICAL to cli/cmd/fishhawk/doctor_verify.go's verifyEnvGitConfigPins
// by TestGateEnvListsMatchCLICopy — editing one copy fails the runner suite.
var gateEnvGitConfigPins = []string{
	"GIT_CONFIG_GLOBAL=/dev/null",
	"GIT_CONFIG_SYSTEM=/dev/null",
}

// sanitizedGateEnv returns the allow-listed environment to assign to a gate
// subprocess's cmd.Env. Assigning a non-nil cmd.Env replaces the child's
// environment wholesale (os/exec.Cmd.Env: "If Env is nil, the new process uses
// the current process's environment."), so the child sees only these entries.
func sanitizedGateEnv() []string {
	return sanitizeEnv(os.Environ())
}

// sanitizeEnv applies the default-deny allow-list to base (a slice of "KEY=VALUE"
// entries). It is the testable inner core of sanitizedGateEnv.
func sanitizeEnv(base []string) []string {
	out := make([]string, 0, len(base)+len(gateEnvGitConfigPins))
	for _, kv := range base {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			// No '=' (malformed) or empty key — not a usable assignment; drop.
			continue
		}
		key := kv[:eq]
		if gateEnvDenied(key) {
			continue
		}
		if gateEnvAllowed(key) {
			if strings.HasPrefix(key, "GO") {
				// Redact embedded URL userinfo (e.g. credentials in a GOPROXY
				// URL) before it reaches gate code; non-URL values are
				// unchanged, so kv is reconstructed byte-identical in that case.
				out = append(out, key+"="+redactGoEnvUserinfo(kv[eq+1:]))
				continue
			}
			out = append(out, kv)
		}
	}
	// Pin git config-file env to /dev/null AFTER the allow-list loop. GIT_CONFIG_*
	// is on no allow-list, so the loop already dropped any inherited value — the
	// append therefore REPLACES rather than duplicates it, and cannot produce a
	// second entry the platform's undocumented Cmd.Env dedup could resolve to the
	// ambient value. Do NOT add these keys to gateEnvAllowExact: admitting the
	// inherited value would defeat the determinism the pin exists for.
	return append(out, gateEnvGitConfigPins...)
}

// redactGoEnvUserinfo strips embedded URL userinfo from a GO* env value. The
// value may be a list of proxy entries separated by ',' (fall through on
// 404/410) or '|' (fall through on any error) per the GOPROXY protocol
// (https://go.dev/ref/mod#goproxy-protocol); both separators and the entry
// order are preserved exactly. Each entry is redacted independently and the
// transform is pure/deterministic so the gate env stays replay-stable.
func redactGoEnvUserinfo(value string) string {
	var b strings.Builder
	start := 0
	for i := 0; i < len(value); i++ {
		if c := value[i]; c == ',' || c == '|' {
			b.WriteString(redactURLEntryUserinfo(value[start:i]))
			b.WriteByte(c)
			start = i + 1
		}
	}
	b.WriteString(redactURLEntryUserinfo(value[start:]))
	return b.String()
}

// redactURLEntryUserinfo returns entry with any embedded URL userinfo removed.
// It rewrites only entries that parse as a credentialed URL — one with both a
// scheme AND non-nil userinfo. net/url.Parse populates URL.User only when the
// entry has a scheme, so non-URL forms (off, direct, a bare 'user:pass@host'
// with no scheme, or a parse error) have User==nil and pass through verbatim.
// Setting URL.User=nil drops the userinfo and its '@', yielding
// 'scheme://host/path' (https://pkg.go.dev/net/url#URL.String).
func redactURLEntryUserinfo(entry string) string {
	u, err := url.Parse(entry)
	if err != nil || u.Scheme == "" || u.User == nil {
		return entry
	}
	u.User = nil
	return u.String()
}

// withIsolatedLintCache returns a copy of env with GOLANGCI_LINT_CACHE forced to
// cacheDir, dropping any inherited GOLANGCI_LINT_CACHE= entry first so the
// override cannot be undercut by an ambient value regardless of the platform's
// os/exec.Cmd duplicate-key env semantics (which are not documented to be
// "last wins"; see https://pkg.go.dev/os/exec#Cmd — Env is passed as-is with no
// dedup guarantee).
//
// golangci-lint keys its analysis cache by ABSOLUTE file path and shares one
// cache dir across processes when GOLANGCI_LINT_CACHE is unset (falling back to
// os.UserCacheDir()/golangci-lint). Under the shared local gitdir, two
// concurrent runs' lineage worktrees carry distinct absolute paths, so run A's
// cached lint results could surface in run B's strict re-verify — a spurious
// category-B (#1796). Isolating the linter cache per verify-gate invocation
// makes that leak impossible. GOCACHE is deliberately left SHARED: the Go build
// cache is content-addressed (not path-keyed) and safe across worktrees, so
// sharing it keeps compilation warm; only the linter analysis re-runs cold.
func withIsolatedLintCache(env []string, cacheDir string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "GOLANGCI_LINT_CACHE=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GOLANGCI_LINT_CACHE="+cacheDir)
}

// gateEnvAllowed reports whether key is on the allow-list: a system essential
// (gateEnvAllowExact), a Go toolchain/runtime name (gateEnvAllowGo), or an
// allowed prefix (gateEnvAllowPrefix). It does NOT consult the denylist — that
// is a separate layer applied first in sanitizeEnv — so a test that restores a
// broadening allow-rule turns red on this predicate alone.
func gateEnvAllowed(key string) bool {
	if _, ok := gateEnvAllowExact[key]; ok {
		return true
	}
	if _, ok := gateEnvAllowGo[key]; ok {
		return true
	}
	for _, p := range gateEnvAllowPrefix {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// gateEnvDenied reports whether key is on the known-secret denylist: an exact
// match (gateEnvDeny) or a denied prefix (gateEnvDenyPrefix). It is the
// belt-and-suspenders layer sanitizeEnv applies BEFORE the allow-list, so a
// denied key is dropped regardless of any allow-rule.
func gateEnvDenied(key string) bool {
	if _, ok := gateEnvDeny[key]; ok {
		return true
	}
	for _, p := range gateEnvDenyPrefix {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}
