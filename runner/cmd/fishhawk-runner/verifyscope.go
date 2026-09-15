package main

import (
	"context"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
)

// Scoped-verify wire contract (#3315).
//
// `scripts/test verify` gained a SCOPED form: `--packages <repo/rel/pkg,...>`
// restricts the per-module `go test -race` loop to the listed packages (every
// other module is skipped) while lint, the doc-line budget, schema-sync, the
// gate harnesses and both site gates run unchanged. It also takes a
// per-repository verify lock in the shared git common dir, whose contention
// semantics depend on WHO is asking: a plain agent-shell invocation meeting a
// live holder refuses immediately, while a RUNNER-marked one waits (bounded)
// and only then displaces a shell holder.
//
// The runner drives both through the gate subprocess's environment:
//
//	FISHHAWK_VERIFY_PACKAGES=<comma-joined repo-relative package dirs>
//	FISHHAWK_VERIFY_LOCK_OWNER=runner
//	FISHHAWK_VERIFY_LOCK_PATH=<primary git-common-dir>/fishhawk-verify.lock
//
// The third (ADR-063 / #2134) exists because the runner's throwaway verify
// checkout is a `git clone --no-hardlinks` with a common dir of its OWN, not a
// linked worktree sharing the primary's: left to `scripts/test`'s own
// derivation the runner's lock would be keyed to a directory no shell verify
// ever looks at, silently reopening the #2645 contention. The runner resolves
// the PRIMARY repository's lock path and injects it after sanitization.
//
// Those three names and the comma joiner are a BARE STRING CONTRACT duplicated
// across a bash script and a Go module that cannot import it — the shape
// backend/internal/wirecontract exists for. They are declared here as package
// consts so the Go side has ONE source of truth, and they are pinned from both
// directions: TestVerifyPackagesEnvNameMatchesScriptsTest greps the repo's real
// `scripts/test` for these exact names (the cheap NAME pin), and
// TestRunnerEnvDrivesRealScriptsTestVerify runs the REAL `scripts/test verify`
// with an environment built by CALLING the builders below (the executable pin
// that binds the VALUE ENCODING — the half a name pin structurally cannot
// cover). That executable pin carries a MULTI-PACKAGE arm on purpose: a
// one-element join emits the same bytes under any separator, so only a two-
// package set can bind verifyPackagesSeparator at all.
const (
	// verifyPackagesEnvVar is the scoped-verify package-set variable
	// `scripts/test` reads as its `--packages` fallback.
	verifyPackagesEnvVar = "FISHHAWK_VERIFY_PACKAGES"

	// verifyLockOwnerEnvVar is the verify-lock owner-kind variable. Only the
	// exact value verifyLockOwnerRunner selects the runner's wait-then-maybe-
	// displace semantics; anything else (including absence) is a shell caller.
	verifyLockOwnerEnvVar = "FISHHAWK_VERIFY_LOCK_OWNER"

	// verifyLockOwnerRunner is the one value that marks an invocation as the
	// runner's own authoritative gate.
	verifyLockOwnerRunner = "runner"

	// verifyLockPathEnvVar is the verify-lock LOCATION override `scripts/test`
	// honours FIRST, before its own common-dir derivation (ADR-063 / #2134).
	// The runner sets it to the primary repository's lock path so a verify run
	// from the throwaway CLONE still contends with a shell verify in the
	// primary's worktree family.
	verifyLockPathEnvVar = "FISHHAWK_VERIFY_LOCK_PATH"

	// verifyLockFileName is the lock's basename under the git common dir —
	// the same literal `scripts/test`'s `_verify_lock_path` appends.
	verifyLockFileName = "fishhawk-verify.lock"

	// verifyLockPathResolveTimeout bounds the single `git rev-parse` the
	// resolver runs. Resolution is best-effort: a slow or wedged git degrades
	// to "unchanged env" (no injection), never to a stalled gate.
	verifyLockPathResolveTimeout = 10 * time.Second

	// verifyPackagesSeparator joins the package set into the env value. It is
	// the reason a package path containing a comma makes verifyScopePackages
	// return the EMPTY set: the encoding could not present such a path without
	// splitting it into two, and the shell layer would then test a package the
	// caller never named.
	verifyPackagesSeparator = ","
)

// verifyScopePackages maps a stage's scope file paths to the repo-relative
// package DIRECTORIES the scoped verify form should test.
//
//   - Only `.go` files contribute; a docs/YAML/shell scope file names no Go
//     package and is skipped.
//   - A root-level Go file maps to "." (the module-root package).
//   - The result is deduplicated and sorted, so the env value is deterministic
//     and a trace comparison is stable.
//
// It returns the EMPTY set — meaning "no scoping", which the caller widens to
// the FULL verify form — whenever the scope cannot be presented faithfully:
//
//   - ANY scope path (Go or not) carrying a comma, tab or newline. The comma is
//     the env encoding's own separator and a tab/newline is the shell layer's;
//     either would emit a truncated or split package list, and the shell would
//     then run a DIFFERENT set than the one derived here. Non-Go paths are
//     checked too, deliberately: widening on a path we would have skipped costs
//     one full verify, whereas a narrower check risks scoping off a mangled set.
//   - Any absolute path, or one that escapes the repository root after
//     cleaning. Such a path is not a repo-relative package dir at all.
//
// Every one of those branches widens rather than narrows. That is the whole
// fail-safe direction of this change: a scoped run must never silently test
// LESS than the caller asked for, and the shell layer independently widens on
// a package it cannot bucket (`--packages entry '…' is not inside any go.work
// module — running the full test loop`).
func verifyScopePackages(scopeFiles []string) []string {
	seen := make(map[string]struct{}, len(scopeFiles))
	pkgs := make([]string, 0, len(scopeFiles))
	for _, raw := range scopeFiles {
		if raw == "" {
			continue
		}
		// Undecodable-path check runs over EVERY entry, before the .go filter.
		if strings.ContainsAny(raw, ",\t\n") {
			return nil
		}
		cleaned := path.Clean(raw)
		if strings.HasPrefix(cleaned, "/") || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			return nil
		}
		if !strings.HasSuffix(cleaned, ".go") {
			continue
		}
		dir := path.Dir(cleaned)
		if dir == "" {
			dir = "."
		}
		if _, ok := seen[dir]; ok {
			continue
		}
		seen[dir] = struct{}{}
		pkgs = append(pkgs, dir)
	}
	if len(pkgs) == 0 {
		return nil
	}
	sort.Strings(pkgs)
	return pkgs
}

// verifyScopeEnv returns env with FISHHAWK_VERIFY_PACKAGES set to the
// comma-joined package set, dropping any inherited entry of that name FIRST.
//
// The drop-then-append shape mirrors withIsolatedLintCache and exists for the
// same reason: os/exec does not document duplicate-key resolution
// (https://pkg.go.dev/os/exec#Cmd — Env is passed as-is with no dedup
// guarantee), so appending without dropping could leave the platform free to
// resolve a duplicate to the ambient value. Combined with sanitizedGateEnv's
// default-deny allow-list — which already strips this name, since it is on
// neither gateEnvAllowExact, gateEnvAllowGo nor gateEnvAllowPrefix (CGO_/LC_
// only) — the value the gate subprocess sees can ONLY be one the runner
// explicitly injected here, after sanitization.
//
// An EMPTY package set returns env UNCHANGED, with no variable appended. That
// is what makes "no derivable scope" and "the final authoritative invocation"
// produce byte-identically unscoped environments: absence of the variable is
// the full form, and there is no in-band empty value to misread.
func verifyScopeEnv(env []string, pkgs []string) []string {
	if len(pkgs) == 0 {
		return env
	}
	out := dropEnvKey(env, verifyPackagesEnvVar)
	return append(out, verifyPackagesEnvVar+"="+strings.Join(pkgs, verifyPackagesSeparator))
}

// verifyLockOwnerEnv returns env with FISHHAWK_VERIFY_LOCK_OWNER=runner set,
// dropping any inherited entry first (same rationale as verifyScopeEnv).
//
// It is appended to EVERY runner-issued verify invocation — scoped and full
// alike — because the lock semantics are about WHO is running the gate, not
// about how narrow it is. A verify the runner owns waits for a live shell
// holder instead of racing it, and is itself never displaced by a shell caller.
func verifyLockOwnerEnv(env []string) []string {
	out := dropEnvKey(env, verifyLockOwnerEnvVar)
	return append(out, verifyLockOwnerEnvVar+"="+verifyLockOwnerRunner)
}

// verifyLockPathEnv returns env with FISHHAWK_VERIFY_LOCK_PATH set to the
// PRIMARY repository's `<git-common-dir>/fishhawk-verify.lock`, dropping any
// inherited entry first (same rationale as verifyScopeEnv). repoDir is the
// primary checkout (or any linked worktree of it — `--git-common-dir` resolves
// the SHARED common dir either way), NOT the throwaway clone the gate runs in:
// the clone's common dir is its own, and a lock keyed there contends with
// nothing.
//
// When the common dir cannot be resolved — repoDir is not a git work tree, git
// is absent, or the command fails/times out — env is returned UNCHANGED with no
// variable appended. That is deliberate: `scripts/test` then falls back to its
// own derivation, which in the clone degrades to the clone's own common dir
// (still a lock, just an uncontended one) — the pre-#2134 behaviour, never a
// gate failure. The injection is a contention guard, not a correctness gate.
func verifyLockPathEnv(env []string, repoDir string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), verifyLockPathResolveTimeout)
	defer cancel()
	common, err := gitOut(ctx, repoDir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || common == "" {
		return env
	}
	out := dropEnvKey(env, verifyLockPathEnvVar)
	return append(out, verifyLockPathEnvVar+"="+strings.TrimRight(common, "/")+"/"+verifyLockFileName)
}

// dropEnvKey returns a copy of env with every "key=…" entry removed. It always
// copies rather than filtering in place, so a caller's slice is never aliased
// or mutated across two successive builder calls.
func dropEnvKey(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// The two verify FORM names (#3315). Every verify log line the fix loop emits
// names the form that produced it, so an agent reading a green-scoped /
// red-full pair in one iteration can tell which run said what.
const (
	verifyFormScoped = "scoped"
	verifyFormFull   = "full"
)

// verifyFormName reports which form a package set selects: "scoped" for a
// non-empty set, "full" for an empty one. Empty means no narrowing, which IS
// the full form — the fail-safe direction.
func verifyFormName(pkgs []string) string {
	if len(pkgs) == 0 {
		return verifyFormFull
	}
	return verifyFormScoped
}

// logVerifyFormOutcome records WHICH verify form produced an outcome (#3315).
// With the scoped pre-pass in force ONE iteration can emit two verify_run
// events — a green scoped run followed by a red full one — and without this
// line the pair reads as a contradiction. It is a log line only: the verdict
// still travels on the verify_run / verify_summary events.
func logVerifyFormOutcome(logSink io.Writer, cfg config, iteration int, form, outcome string) {
	_, _ = fmt.Fprintf(logSink,
		`{"event":"verify_form_outcome","run_id":%q,"stage_id":%q,"iteration":%d,"form":%q,"outcome":%q}`+"\n",
		cfg.runID, cfg.stageID, iteration, form, outcome)
}
