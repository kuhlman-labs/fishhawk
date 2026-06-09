package main

import (
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

// gateEnvAllowPrefix lists key prefixes admitted wholesale: every GO* var
// (GOPATH/GOCACHE/GOMODCACHE/GOPROXY/GOFLAGS/GOTOOLCHAIN/…), every CGO_* var,
// and every LC_* locale var. Preserving the whole GO* prefix avoids turning a
// real compile/test failure into an infra-skip from a missing toolchain var.
var gateEnvAllowPrefix = []string{"GO", "CGO_", "LC_"}

// gateEnvDeny is the explicit known-secret denylist (belt-and-suspenders on top
// of the default-deny allow-list). These keys are dropped unconditionally.
var gateEnvDeny = map[string]struct{}{
	"FISHHAWK_GITHUB_TOKEN": {},
	"GITHUB_TOKEN":          {},
	"GH_TOKEN":              {},
	"ANTHROPIC_API_KEY":     {},
	"OPENAI_API_KEY":        {},
	"FISHHAWK_API_TOKEN":    {},
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
	out := make([]string, 0, len(base))
	for _, kv := range base {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			// No '=' (malformed) or empty key — not a usable assignment; drop.
			continue
		}
		key := kv[:eq]
		if _, denied := gateEnvDeny[key]; denied {
			continue
		}
		if gateEnvAllowed(key) {
			out = append(out, kv)
		}
	}
	return out
}

// gateEnvAllowed reports whether key is on the allow-list (exact match or an
// allowed prefix).
func gateEnvAllowed(key string) bool {
	if _, ok := gateEnvAllowExact[key]; ok {
		return true
	}
	for _, p := range gateEnvAllowPrefix {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}
